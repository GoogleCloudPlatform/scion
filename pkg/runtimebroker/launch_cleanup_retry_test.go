// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// cleanupCanary is text a failing clean-up's error carries; it stands for a
// path or credential that must never reach the give-up log.
const cleanupCanary = "/var/secret/cleanup-canary-path"

// failingCleanupManager is a recFakeManager whose CleanupLaunch and
// DeleteTarget can fail.
type failingCleanupManager struct {
	*recFakeManager
	cleanupErr error
	deleteErr  error

	mu           sync.Mutex
	cleanupCalls int
}

func (m *failingCleanupManager) CleanupLaunch(ctx context.Context, handles []agent.ResourceHandle) error {
	m.mu.Lock()
	m.cleanupCalls++
	m.mu.Unlock()
	return m.cleanupErr
}

func (m *failingCleanupManager) CleanupCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupCalls
}

func (m *failingCleanupManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	if _, err := m.recFakeManager.DeleteTarget(ctx, agentName, ref, deleteFiles, projectPath, removeBranch); err != nil {
		return false, err
	}
	if m.deleteErr != nil {
		return false, m.deleteErr
	}
	return true, nil
}

// TestRunLaunch_FailedCleanupKeepsJournalRecord covers ptone/scion#5177 on
// a running broker: a launch whose clean-up fails keeps its journal record
// as cleanup_pending with one attempt (no terminal), and a launch whose
// clean-up succeeds is removed.
func TestRunLaunch_FailedCleanupKeepsJournalRecord(t *testing.T) {
	for _, c := range []struct {
		name       string
		cleanupErr error
	}{
		{"clean-up fails", errors.New("delete pod: " + cleanupCanary)},
		{"clean-up succeeds", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			mgr := &failingCleanupManager{recFakeManager: newRecFakeManager(), cleanupErr: c.cleanupErr}
			mgr.setStartErr(errors.New("runtime refused"))
			srv, rtb := newAsyncTestServer(t, mgr)
			rtb.launchReportFunc = appliedReports
			w := postCreate(t, srv, map[string]any{
				"name": "agent-cr", "asyncLaunch": true, "launchId": "L-cr",
				"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
			})
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if !waitUntil(t, 5*time.Second, func() bool { return mgr.CleanupCalls() == 1 && registryEmpty(srv) }) {
				t.Fatalf("the launch did not fail and clean up (cleanup calls %d)", mgr.CleanupCalls())
			}
			path := filepath.Join(srv.launchJournal.dir(), "L-cr.json")
			if c.cleanupErr == nil {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("a cleaned-up launch's record was not removed: %v", err)
				}
				return
			}
			jr := readJournalFileT(t, path)
			if jr.State != "cleanup_pending" || jr.CleanupAttempts != 1 || jr.CleanupFirstFailedAt.IsZero() || jr.Terminal != nil {
				t.Fatalf("a failed clean-up did not keep a cleanup_pending record with 1 attempt: %+v", jr)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), cleanupCanary) {
				t.Fatalf("the journal record carries the clean-up error text: %s", raw)
			}
			assertJournalKeysAllowed(t, raw)
		})
	}
}

// TestRecoverLaunches_FailedCleanupKeptForRetry covers recovery of a
// launching record whose clean-up fails after the hub's confirming answer:
// the record is rewritten as cleanup_pending with one attempt instead of
// being removed.
func TestRecoverLaunches_FailedCleanupKeptForRetry(t *testing.T) {
	mgr := &failingCleanupManager{recFakeManager: newRecFakeManager(), cleanupErr: errors.New("delete pod: " + cleanupCanary)}
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = appliedReports
	path := writeDeadRecord(t, srv, launchingRecord("LR1"))

	runRecovery(t, srv)

	if n := len(terminalReports(rtb)); n != 1 {
		t.Fatalf("expected the launch reported once, got %d", n)
	}
	jr := readJournalFileT(t, path)
	if jr.State != "cleanup_pending" || jr.CleanupAttempts != 1 || jr.CleanupFirstFailedAt.IsZero() || jr.Terminal != nil {
		t.Fatalf("a failed recovery clean-up did not keep a cleanup_pending record with 1 attempt: %+v", jr)
	}
	if len(jr.Handles) != 1 || jr.Handles[0].UID != "uid-LR1" {
		t.Fatalf("the kept record lost its handles: %+v", jr.Handles)
	}
}

// TestRecoverLaunches_CleanupPendingRetried covers a cleanup_pending record:
// recovery sends no report (the hub already answered), retries the
// clean-up, and removes the record once it succeeds.
func TestRecoverLaunches_CleanupPendingRetried(t *testing.T) {
	mgr := &failingCleanupManager{recFakeManager: newRecFakeManager(api.AgentInfo{Name: "agent-LR2", ContainerID: "c-run", RunID: "run-LR2"})}
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = appliedReports
	jr := launchingRecord("LR2")
	jr.State = journalCleanupPending.String()
	jr.CleanupAttempts = 2
	jr.CleanupFirstFailedAt = time.Now().Add(-time.Hour)
	path := writeDeadRecord(t, srv, jr)

	runRecovery(t, srv)

	if n := len(rtb.getLaunchReports()); n != 0 {
		t.Fatalf("a cleanup_pending record sent %d report(s); the hub already answered", n)
	}
	if mgr.CleanupCalls() != 1 || len(mgr.Deleted()) != 1 {
		t.Fatalf("the clean-up was not retried (cleanup %d, deleted %+v)", mgr.CleanupCalls(), mgr.Deleted())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a successfully retried record was not removed: %v", err)
	}
}

// TestRecoverLaunches_CleanupRetryBound covers the retry bound: a failing
// retry below the bound is kept with one more attempt; at the attempt bound
// or past the age bound, recovery gives up, logs an Error carrying IDs and
// the failure class but never the error's text, and removes the record.
func TestRecoverLaunches_CleanupRetryBound(t *testing.T) {
	cases := []struct {
		name         string
		attempts     int
		firstAgo     time.Duration
		wantGiveUp   bool
		wantAttempts int
	}{
		{"below the bound", 2, time.Hour, false, 3},
		{"attempt bound", launchCleanupMaxAttempts - 1, time.Hour, true, 0},
		{"age bound", 1, launchCleanupMaxAge + time.Hour, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := &failingCleanupManager{recFakeManager: newRecFakeManager(), cleanupErr: errors.New("delete pod: " + cleanupCanary)}
			srv, _ := newAsyncTestServer(t, mgr)
			logs := captureLifecycleLog(srv)
			jr := launchingRecord("LR3")
			jr.State = journalCleanupPending.String()
			jr.CleanupAttempts = c.attempts
			jr.CleanupFirstFailedAt = time.Now().Add(-c.firstAgo)
			path := writeDeadRecord(t, srv, jr)

			runRecovery(t, srv)

			out := logs.String()
			if !c.wantGiveUp {
				got := readJournalFileT(t, path)
				if got.State != "cleanup_pending" || got.CleanupAttempts != c.wantAttempts {
					t.Fatalf("a failing retry below the bound was not kept with %d attempts: %+v", c.wantAttempts, got)
				}
				if strings.Contains(out, "given up") {
					t.Fatalf("recovery gave up below the bound:\n%s", out)
				}
				return
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("recovery did not give up at the bound: the record is still there (%v)", err)
			}
			var giveUp string
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "given up") {
					giveUp = line
				}
			}
			if giveUp == "" || !strings.Contains(giveUp, "level=ERROR") || !strings.Contains(giveUp, "launch_id=LR3") ||
				!strings.Contains(giveUp, "error_class="+string(cleanupFailRuntimeResources)) {
				t.Fatalf("no Error give-up log with the launch ID and failure class:\n%s", out)
			}
			if strings.Contains(giveUp, cleanupCanary) {
				t.Fatalf("the give-up log carries the clean-up error text: %s", giveUp)
			}
		})
	}
}

// TestRecoverLaunches_RunDeleteFailureKeptForRetry covers the run-scoped
// delete counting as a clean-up step: a DeleteTarget error keeps the record
// for a retry, with failure class run_delete.
func TestRecoverLaunches_RunDeleteFailureKeptForRetry(t *testing.T) {
	mgr := &failingCleanupManager{
		recFakeManager: newRecFakeManager(api.AgentInfo{Name: "agent-LR4", ContainerID: "c-run", RunID: "run-LR4"}),
		deleteErr:      errors.New("container rm: " + cleanupCanary),
	}
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = appliedReports
	logs := captureLifecycleLog(srv)
	path := writeDeadRecord(t, srv, launchingRecord("LR4"))

	runRecovery(t, srv)

	if len(mgr.Deleted()) != 1 {
		t.Fatalf("the run-scoped delete was not attempted: %+v", mgr.Deleted())
	}
	jr := readJournalFileT(t, path)
	if jr.State != "cleanup_pending" || jr.CleanupAttempts != 1 {
		t.Fatalf("a failed run-scoped delete did not keep the record for a retry: %+v", jr)
	}
	if !strings.Contains(logs.String(), "error_class="+string(cleanupFailRunDelete)) {
		t.Fatalf("the retry log lacks the run_delete class:\n%s", logs.String())
	}
}

// TestLaunchJournal_CleanupPendingOrdering covers the writer: cleanup_pending
// follows a terminal, only removal follows cleanup_pending, and nothing is
// written after removal.
func TestLaunchJournal_CleanupPendingOrdering(t *testing.T) {
	j, _ := newTestJournal(t, "inst-cp")
	rec := newLaunchRecord("Lcp", "agent-cp", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	e := j.admit(rec, journalRecord{Slug: "s-cp", AgentName: "s-cp"})
	if e == nil {
		t.Fatal("admit returned nil")
	}
	e.terminal(journalTerminal{ErrorCode: "runtime_error"})
	e.cleanupPending(time.Now(), "")
	jr := readJournalFileT(t, e.path)
	if jr.State != "cleanup_pending" || jr.CleanupAttempts != 1 || jr.Terminal != nil {
		t.Fatalf("cleanup_pending did not follow the terminal: %+v", jr)
	}
	e.update()
	e.terminal(journalTerminal{Succeeded: true})
	if got := readJournalFileT(t, e.path); got.State != "cleanup_pending" {
		t.Fatalf("a write after cleanup_pending replaced it: %+v", got)
	}
	e.remove()
	e.cleanupPending(time.Now(), "")
	if _, err := os.Stat(e.path); !os.IsNotExist(err) {
		t.Fatalf("a cleanup_pending write after removal recreated the record: %v", err)
	}
}

// TestRecoveryClassifiesCleanupFailures pins the class helpers.
func TestRecoveryClassifiesCleanupFailures(t *testing.T) {
	rec := newLaunchRecord("Lc", "a", store.LaunchKindCreate, "", time.Now(), func() {})
	err := errors.Join(nil, failCleanup(rec, cleanupFailFilesLock), failCleanup(rec, cleanupFailRunDelete))
	if got := cleanupFailureClassOf(err); got != cleanupFailFilesLock {
		t.Fatalf("class = %q, want the first failure %q", got, cleanupFailFilesLock)
	}
	if p := rec.cleanupFailure.Load(); p == nil || *p != cleanupFailFilesLock {
		t.Fatalf("the record does not keep its first failure class: %v", p)
	}
	if cleanupFailureClassOf(nil) != "" {
		t.Fatal("nil must have no class")
	}
}

// TestLaunchCleanup_LiveFilesFailureRetriedAtRecovery is R1 of the #5177
// review end to end: a live create launch whose agent-file clean-up fails
// (its workspace lock is held) keeps both its journal record and its launch
// marker; after a broker restart, recovery retries the file deletion, which
// now succeeds, then removes the marker and the record.
func TestLaunchCleanup_LiveFilesFailureRetriedAtRecovery(t *testing.T) {
	mgr := &failingCleanupManager{recFakeManager: newRecFakeManager()}
	mgr.setStartErr(errors.New("runtime refused"))
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = appliedReports
	srv.agentFilesLockWait = 50 * time.Millisecond

	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", "agent-lf")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLaunchMarker(projectDir, false, "agent-lf", "L-lf"); err != nil {
		t.Fatal(err)
	}
	// Hold the agent's workspace lock so the live file clean-up fails.
	unlock, err := srv.locks().Lock(context.Background(), agentFileLockPaths(projectDir, "agent-lf")...)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newLaunchRecord("L-lf", "agent-id-lf", store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	rec.journal = srv.launchJournal.admit(rec, journalRecord{Slug: "agent-lf", AgentName: "agent-lf",
		ProjectPath: projectDir, RuntimeName: "mock", RuntimeIdentity: "mock"})
	if rec.journal == nil {
		t.Fatal("launch not journalled")
	}
	srv.trackAsyncLaunch(rec)
	srv.runLaunch(ctx, rec, launchCtx{
		opts: api.StartOptions{Name: "agent-lf", ProjectPath: projectDir},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-lf"},
	})
	unlock()

	path := rec.journal.path
	if jr := readJournalFileT(t, path); jr.State != "cleanup_pending" {
		t.Fatalf("a live file clean-up failure did not keep the record: %+v", jr)
	}
	if !launchMarkerMatches(projectDir, false, "agent-lf", "L-lf") {
		t.Fatal("the launch marker was removed although the file clean-up failed; the retry cannot delete the files")
	}
	if _, err := os.Stat(agentDir); err != nil {
		t.Fatalf("test setup: the agent files should still be there: %v", err)
	}

	// The broker restarts with the same state dir.
	srv.launchJournal.Close()
	cfg := srv.config
	cfg.StateDir = srv.stateDir
	srv2 := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "mock" }}, config.RuntimeBuildRecord{})
	closeJournalAtCleanup(t, srv2)
	runRecovery(t, srv2)

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("recovery did not retry the file deletion: %v", err)
	}
	if launchMarkerMatches(projectDir, false, "agent-lf", "L-lf") {
		t.Fatal("recovery left the launch marker after the retry succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("recovery did not remove the record after the retry succeeded: %v", err)
	}
}

// TestLaunchCleanup_ResourcesOnlyScope is O2 of the #5177 review: a launch
// whose clean-up was only its recorded resources (a local stop or delete
// owns the files) keeps scope "resources", and recovery retries only the
// resources, not the run-scoped delete or the files.
func TestLaunchCleanup_ResourcesOnlyScope(t *testing.T) {
	t.Run("endLaunchJournal records the scope", func(t *testing.T) {
		srv, _ := newAsyncTestServer(t, newAsyncManager())
		rec := newLaunchRecord("L-sc", "agent-id-sc", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
		rec.journal = srv.launchJournal.admit(rec, journalRecord{Slug: "agent-sc", AgentName: "agent-sc"})
		rec.resourcesOnlyCleanup.Store(true)
		_ = failCleanup(rec, cleanupFailRuntimeResources)
		srv.endLaunchJournal(rec)
		jr := readJournalFileT(t, rec.journal.path)
		if jr.State != "cleanup_pending" || jr.CleanupScope != cleanupScopeResources {
			t.Fatalf("the resources-only scope was not recorded: %+v", jr)
		}
	})
	t.Run("recovery retries only the resources", func(t *testing.T) {
		mgr := &failingCleanupManager{recFakeManager: newRecFakeManager(api.AgentInfo{Name: "agent-LR5", ContainerID: "c-run", RunID: "run-LR5"})}
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = appliedReports
		jr := launchingRecord("LR5")
		jr.State = journalCleanupPending.String()
		jr.CleanupAttempts = 1
		jr.CleanupFirstFailedAt = time.Now()
		jr.CleanupScope = cleanupScopeResources
		path := writeDeadRecord(t, srv, jr)

		runRecovery(t, srv)

		if mgr.CleanupCalls() != 1 {
			t.Fatalf("the resources were not retried (cleanup %d)", mgr.CleanupCalls())
		}
		if d := mgr.Deleted(); len(d) != 0 {
			t.Fatalf("a resources-only retry ran the run-scoped delete: %+v", d)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the record was not removed after the retry: %v", err)
		}
	})
}

// closeJournalAtCleanup closes srv's journal at the end of the
// test (a server built directly with New, not through the test helpers).
func closeJournalAtCleanup(t *testing.T, srv *Server) {
	t.Helper()
	t.Cleanup(srv.launchJournal.Close)
}
