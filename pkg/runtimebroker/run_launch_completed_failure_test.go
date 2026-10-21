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

// This file covers ptone/scion#5368 on a running broker: a launch whose
// Start fails is reported and cleaned up even when the hub had already
// answered completed. The journal's terminal flags and the report's runId
// are read as JSON, so the file builds without the fields it checks.
package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleRecordingManager is a failingCleanupManager that records the
// handles of every CleanupLaunch call.
type handleRecordingManager struct {
	*failingCleanupManager

	hmu     sync.Mutex
	cleaned [][]agent.ResourceHandle
}

func (m *handleRecordingManager) CleanupLaunch(ctx context.Context, handles []agent.ResourceHandle) error {
	m.hmu.Lock()
	m.cleaned = append(m.cleaned, append([]agent.ResourceHandle(nil), handles...))
	m.hmu.Unlock()
	return m.failingCleanupManager.CleanupLaunch(ctx, handles)
}

func (m *handleRecordingManager) Cleaned() [][]agent.ResourceHandle {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	return append([][]agent.ResourceHandle(nil), m.cleaned...)
}

// sfOptions shapes runStartFailedLaunch's launch.
type sfOptions struct {
	// claimCompleted answers the claim completed (else applied).
	claimCompleted bool
	// keepaliveCompleted answers keepalives completed and holds Start
	// until a completed keepalive answer has been recorded.
	keepaliveCompleted bool
	// terminal answers the n-th failed report (n from 1).
	terminal func(n int) (*hubclient.AgentLaunchReportResult, error)
	// cleanupErr makes CleanupLaunch fail.
	cleanupErr error
	// deadlinePassed puts the launch deadline 15 minutes in the past, so
	// the terminal has no TTL left and an unanswered send gives up at once.
	deadlinePassed bool
	// beforeRun runs on the server just before runLaunch.
	beforeRun func(srv *Server)
	// download, when set, replaces the workspace download step.
	download func(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (api.StartOptions, string, string, error)
	// localCancel holds Start and cancels the launch locally once Start
	// is reached.
	localCancel bool
	// shutdownCancel holds Start and, once Start is reached, cancels the
	// launch's ctx' with errBrokerShutdown as its cause, as the shutdown
	// drain does (the drain's wait has not ended).
	shutdownCancel bool
}

// startFailedLaunch is the outcome of runStartFailedLaunch.
type startFailedLaunch struct {
	srv        *Server
	rtb        *mockRuntimeBrokerService
	mgr        *handleRecordingManager
	rec        *launchRecord
	projectDir string
	agentDir   string
	slug       string

	mu sync.Mutex
	// journalAtTerminal is the raw journal record when the first failed
	// report reached the hub (the terminal is journalled before it is
	// sent).
	journalAtTerminal []byte
}

func (l *startFailedLaunch) JournalAtTerminal() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.journalAtTerminal
}

var completedAnswer = &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}

// runStartFailedLaunch runs one journalled create launch, with a recorded
// pod handle, a matching launch marker and agent files with no recorded
// owner, whose Manager.Start fails, and returns once runLaunch has.
func runStartFailedLaunch(t *testing.T, id string, o sfOptions) *startFailedLaunch {
	t.Helper()
	mgr := &handleRecordingManager{failingCleanupManager: &failingCleanupManager{recFakeManager: newRecFakeManager(), cleanupErr: o.cleanupErr}}
	mgr.setStartErr(errors.New("runtime refused"))
	if o.keepaliveCompleted || o.localCancel || o.shutdownCancel {
		mgr.startBlock = make(chan struct{})
	}
	srv, rtb := newAsyncTestServer(t, mgr)
	srv.launchTimingOverride = fastLaunchTimings(fastTestKeepaliveInterval)

	slug := "agent-" + id
	launchID := "L-" + id
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", slug)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLaunchMarker(projectDir, false, slug, launchID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Hour)
	if o.deadlinePassed {
		deadline = time.Now().Add(-15 * time.Minute)
	}
	ctx, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(nil)
	rec := newLaunchRecord(launchID, "agent-id-"+id, store.LaunchKindCreate, "", deadline, func() { cancelCause(nil) })
	rec.RunID = "run-" + id
	rec.AddHandle(agent.ResourceHandle{Kind: "Pod", Namespace: "ns", Name: slug, UID: "uid-" + id})
	rec.journal = srv.launchJournal.admit(rec, journalRecord{Slug: slug, AgentName: slug, ProjectPath: projectDir,
		RuntimeName: "mock", RuntimeIdentity: "mock"})
	if rec.journal == nil {
		t.Fatal("launch not journalled")
	}
	l := &startFailedLaunch{srv: srv, rtb: rtb, mgr: mgr, rec: rec, projectDir: projectDir, agentDir: agentDir, slug: slug}

	var amu sync.Mutex
	failed, keepalives := 0, 0
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		switch req.State {
		case hubclient.AgentLaunchReportStateClaim:
			if o.claimCompleted {
				return completedAnswer, nil
			}
			return appliedAnswer, nil
		case hubclient.AgentLaunchReportStateFailed:
			amu.Lock()
			failed++
			n := failed
			amu.Unlock()
			if n == 1 {
				raw, _ := os.ReadFile(rec.journal.path)
				l.mu.Lock()
				l.journalAtTerminal = raw
				l.mu.Unlock()
			}
			if o.terminal == nil {
				return appliedAnswer, nil
			}
			return o.terminal(n)
		case hubclient.AgentLaunchReportStateSucceeded:
			return appliedAnswer, nil
		default:
			if o.keepaliveCompleted {
				amu.Lock()
				keepalives++
				amu.Unlock()
				return completedAnswer, nil
			}
			return appliedAnswer, nil
		}
	}
	if o.beforeRun != nil {
		o.beforeRun(srv)
	}
	srv.trackAsyncLaunch(rec)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.runLaunch(ctx, rec, launchCtx{
			opts:                     api.StartOptions{Name: slug, ProjectPath: projectDir},
			mgr:                      mgr,
			key:                      launchKey{Slug: slug},
			downloadWorkspaceFromGCS: o.download,
		})
	}()
	switch {
	case o.keepaliveCompleted:
		// The keepalive loop records an answer before it sends the next
		// keepalive, so a second keepalive means the first completed
		// answer is recorded.
		if !waitUntil(t, 5*time.Second, func() bool {
			amu.Lock()
			defer amu.Unlock()
			return keepalives >= 2
		}) {
			t.Fatal("no completed keepalive answer was recorded")
		}
		close(mgr.startBlock)
	case o.localCancel:
		if !waitUntil(t, 5*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
			t.Fatal("Start was never reached")
		}
		rec.CancelLocal()
	case o.shutdownCancel:
		if !waitUntil(t, 5*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
			t.Fatal("Start was never reached")
		}
		cancelCause(errBrokerShutdown)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLaunch did not return")
	}
	return l
}

// failedReports returns the failed reports the hub received.
func failedReports(rtb *mockRuntimeBrokerService) []*hubclient.AgentLaunchReport {
	var out []*hubclient.AgentLaunchReport
	for _, r := range terminalReports(rtb) {
		if r.State == hubclient.AgentLaunchReportStateFailed {
			out = append(out, r)
		}
	}
	return out
}

// reportJSON is report r as the hub receives it on the wire.
func reportJSON(t *testing.T, r *hubclient.AgentLaunchReport) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// rawRecord decodes a raw journal record; rawTerminal returns its terminal
// object, or nil.
func rawRecord(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("journal record is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

func rawTerminal(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	term, _ := rawRecord(t, raw)["terminal"].(map[string]any)
	return term
}

func rawFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal record: %v", err)
	}
	return raw
}

func filesExist(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
}

func TestRunLaunch_StartFailsAfterCompletedKeepalive_ReportsAndCleansUp(t *testing.T) {
	l := runStartFailedLaunch(t, "pcka", sfOptions{keepaliveCompleted: true})

	reports := failedReports(l.rtb)
	if len(reports) != 1 {
		t.Fatalf("expected one failed report after the completed keepalive, got %d", len(reports))
	}
	r := reports[0]
	if r.ErrorCode == "" || r.Step == "" || r.Message == "" {
		t.Fatalf("the failed report lacks its code, step or message: %+v", r)
	}
	if got := reportJSON(t, r)["runId"]; got != "run-pcka" {
		t.Fatalf("failed report runId = %v, want the launch's run run-pcka", got)
	}
	// The keepalive's completed answer makes this the post-completed path.
	if term := rawTerminal(t, l.JournalAtTerminal()); term == nil || term["startFailed"] != true || term["postCompleted"] != true {
		t.Fatalf("the failed terminal was not journalled startFailed and postCompleted: %s", l.JournalAtTerminal())
	}
	cleaned := l.mgr.Cleaned()
	if len(cleaned) != 1 || len(cleaned[0]) != 1 || cleaned[0][0].UID != "uid-pcka" {
		t.Fatalf("the journalled pod handle was not cleaned up: %+v", cleaned)
	}
	if filesExist(l.agentDir) {
		t.Fatal("an applied answer must remove the create's files")
	}
	if _, err := os.Stat(l.rec.journal.path); !os.IsNotExist(err) {
		t.Fatalf("the journal record was not removed: %v", err)
	}
}

func TestRunLaunch_StartFailsAfterCompleted_AnswerTable(t *testing.T) {
	const (
		none = iota
		resources
		full
	)
	answer := func(r *hubclient.AgentLaunchReportResult) func(int) (*hubclient.AgentLaunchReportResult, error) {
		return func(int) (*hubclient.AgentLaunchReportResult, error) { return r, nil }
	}
	cases := []struct {
		name   string
		answer func(int) (*hubclient.AgentLaunchReportResult, error)
		want   int
	}{
		{"applied", answer(appliedAnswer), full},
		{"duplicate", answer(&hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}), full},
		{"409 failed", answer(stale409(hubclient.AgentLaunchReportReasonFailed)), full},
		{"409 superseded", answer(stale409(hubclient.AgentLaunchReportReasonSuperseded)), none},
		{"409 other_owner", answer(stale409(hubclient.AgentLaunchReportReasonOtherOwner)), none},
		{"completed", answer(completedAnswer), resources},
		{"400", answer(&hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}), resources},
		{"401", answer(&hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}), resources},
		{"403", answer(&hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}), resources},
		{"404 agent_launch_unknown", answer(&hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}), resources},
		{"409 deleted", answer(stale409(hubclient.AgentLaunchReportReasonDeleted)), resources},
		{"409 stopped", answer(stale409(hubclient.AgentLaunchReportReasonStopped)), resources},
		{"409 unrecognised reason", answer(stale409("some_future_reason")), resources},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := runStartFailedLaunch(t, "pct"+string(rune('a'+i)), sfOptions{claimCompleted: true, terminal: c.answer})
			if n := len(failedReports(l.rtb)); n != 1 {
				t.Fatalf("expected one failed report after the completed claim, got %d", n)
			}
			cleaned := len(l.mgr.Cleaned())
			switch c.want {
			case none:
				if cleaned != 0 || !filesExist(l.agentDir) {
					t.Fatalf("expected no clean-up (handle clean-ups %d, files kept %v)", cleaned, filesExist(l.agentDir))
				}
			case resources:
				if cleaned != 1 || !filesExist(l.agentDir) {
					t.Fatalf("expected resources-only clean-up (handle clean-ups %d, files kept %v)", cleaned, filesExist(l.agentDir))
				}
			case full:
				if cleaned != 1 || filesExist(l.agentDir) {
					t.Fatalf("expected full clean-up (handle clean-ups %d, files kept %v)", cleaned, filesExist(l.agentDir))
				}
			}
			if _, err := os.Stat(l.rec.journal.path); !os.IsNotExist(err) {
				t.Fatalf("an answered report's journal record was not removed: %v", err)
			}
		})
	}

	t.Run("no answer", func(t *testing.T) {
		l := runStartFailedLaunch(t, "pctnone", sfOptions{
			claimCompleted: true,
			deadlinePassed: true,
			terminal: func(int) (*hubclient.AgentLaunchReportResult, error) {
				return nil, errors.New("simulated unreachable")
			},
		})
		if n := len(l.mgr.Cleaned()); n != 1 {
			t.Fatalf("an unanswered post-completed report must remove the resources now, got %d handle clean-ups", n)
		}
		if !filesExist(l.agentDir) {
			t.Fatal("an unanswered report must keep the files")
		}
		raw := rawFile(t, l.rec.journal.path)
		rec := rawRecord(t, raw)
		if rec["state"] != "terminal_pending" {
			t.Fatalf("the record was not kept as terminal_pending: %s", raw)
		}
		if term := rawTerminal(t, raw); term == nil || term["postCompleted"] != true || term["startFailed"] != true {
			t.Fatalf("the kept terminal is not flagged startFailed and postCompleted: %s", raw)
		}
		if _, ok := rec["cleanupScope"]; ok {
			t.Fatalf("a terminal_pending record must carry no clean-up scope: %s", raw)
		}
		if !launchMarkerMatches(l.projectDir, false, l.slug, l.rec.ID) {
			t.Fatal("the launch marker was removed although the report got no answer")
		}
	})
}

func TestRunLaunch_PostCompleted_FirstAnswerLost_ResendGets409Failed(t *testing.T) {
	l := runStartFailedLaunch(t, "pclost", sfOptions{
		claimCompleted: true,
		terminal: func(n int) (*hubclient.AgentLaunchReportResult, error) {
			if n == 1 {
				// The hub applied the report, but its answer was lost.
				return nil, errors.New("simulated lost answer")
			}
			return stale409(hubclient.AgentLaunchReportReasonFailed), nil
		},
	})
	if n := len(failedReports(l.rtb)); n < 2 {
		t.Fatalf("expected the failed report to be resent, got %d sends", n)
	}
	if n := len(l.mgr.Cleaned()); n != 1 {
		t.Fatalf("expected one handle clean-up, got %d", n)
	}
	if filesExist(l.agentDir) {
		t.Fatal("409 failed confirms this run's failure: the files must be removed")
	}
}

func TestRunLaunch_StartFailsAfterSelfReportedRunning_CompletedAnswer_ResourcesOnly(t *testing.T) {
	// The hub ended the launch running_observed from the harness's own
	// report, so no completed answer reached the broker before Start
	// failed, and the terminal is answered completed.
	l := runStartFailedLaunch(t, "pcself", sfOptions{
		terminal: func(int) (*hubclient.AgentLaunchReportResult, error) { return completedAnswer, nil },
	})
	term := rawTerminal(t, l.JournalAtTerminal())
	if term == nil || term["startFailed"] != true {
		t.Fatalf("the failed terminal was not journalled startFailed: %s", l.JournalAtTerminal())
	}
	if _, ok := term["postCompleted"]; ok {
		t.Fatalf("a terminal sent before any completed answer must not be postCompleted: %s", l.JournalAtTerminal())
	}
	cleaned := l.mgr.Cleaned()
	if len(cleaned) != 1 || len(cleaned[0]) != 1 || cleaned[0][0].UID != "uid-pcself" {
		t.Fatalf("the pod handle was not cleaned up: %+v", cleaned)
	}
	if !filesExist(l.agentDir) {
		t.Fatal("a completed answer must keep the files")
	}
}

func TestRunLaunch_StartFailedOnly_CompletedAnswer_CleanupFails_ScopeResources(t *testing.T) {
	l := runStartFailedLaunch(t, "pcscope", sfOptions{
		terminal:   func(int) (*hubclient.AgentLaunchReportResult, error) { return completedAnswer, nil },
		cleanupErr: errors.New("delete pod: refused"),
	})
	if n := len(l.mgr.Cleaned()); n != 1 {
		t.Fatalf("expected one resources-only clean-up attempt, got %d", n)
	}
	jr := readJournalFileT(t, l.rec.journal.path)
	if jr.State != "cleanup_pending" || jr.CleanupScope != cleanupScopeResources {
		t.Fatalf("a failed resources-only clean-up must keep a cleanup_pending record scoped to the resources: %+v", jr)
	}
	if !filesExist(l.agentDir) {
		t.Fatal("a completed answer must keep the files")
	}
}

func TestRunLaunch_PostCompleted_NoAnswerDuringDrain_SkipsCleanup(t *testing.T) {
	l := runPostCompletedDrainLaunch(t, "pcdrain")
	if n := len(l.mgr.Cleaned()); n != 0 {
		t.Fatalf("no clean-up may run once the shutdown drain has ended, got %d", n)
	}
	assertDrainKeptRecord(t, l)
}

// TestRunLaunch_PostCompleted_NoAnswerDuringShutdown_SkipsCleanup covers
// the other half of the drain skip: the shutdown drain cancels the
// launch's ctx' while its wait is still running, and the failed report
// gets no answer within its TTL.
func TestRunLaunch_PostCompleted_NoAnswerDuringShutdown_SkipsCleanup(t *testing.T) {
	l := runStartFailedLaunch(t, "pcshut", sfOptions{
		claimCompleted: true,
		shutdownCancel: true,
		deadlinePassed: true,
		terminal: func(int) (*hubclient.AgentLaunchReportResult, error) {
			return nil, errors.New("simulated unreachable")
		},
	})
	if l.srv.drainExpired() {
		t.Fatal("test setup: the drain must not have expired")
	}
	reports := failedReports(l.rtb)
	if len(reports) == 0 || reports[0].ErrorCode != api.LaunchCodeBrokerRestarted {
		t.Fatalf("expected a broker_restarted failed report, got %+v", reports)
	}
	if n := len(l.mgr.Cleaned()); n != 0 {
		t.Fatalf("no clean-up may run while the broker shuts down, got %d", n)
	}
	assertDrainKeptRecord(t, l)
}

// runPostCompletedDrainLaunch runs a post-completed Start failure whose
// failed report cannot be sent because the shutdown drain's wait has ended.
func runPostCompletedDrainLaunch(t *testing.T, id string) *startFailedLaunch {
	t.Helper()
	return runStartFailedLaunch(t, id, sfOptions{
		claimCompleted: true,
		terminal: func(int) (*hubclient.AgentLaunchReportResult, error) {
			return nil, errors.New("simulated unreachable")
		},
		beforeRun: func(srv *Server) {
			srv.launchDrain.init()
			srv.launchDrain.expired.Store(true)
			srv.launchDrain.sendCancel()
		},
	})
}

// assertDrainKeptRecord checks the record a drain leaves for a
// post-completed Start failure: terminal_pending, flagged, no scope and no
// report attempt counted.
func assertDrainKeptRecord(t *testing.T, l *startFailedLaunch) {
	t.Helper()
	raw := rawFile(t, l.rec.journal.path)
	rec := rawRecord(t, raw)
	if rec["state"] != "terminal_pending" {
		t.Fatalf("the drain did not keep the record as terminal_pending: %s", raw)
	}
	if term := rawTerminal(t, raw); term == nil || term["postCompleted"] != true || term["startFailed"] != true {
		t.Fatalf("the kept terminal is not flagged startFailed and postCompleted: %s", raw)
	}
	if _, ok := rec["cleanupScope"]; ok {
		t.Fatalf("a terminal_pending record must carry no clean-up scope: %s", raw)
	}
	if n, ok := rec["reportAttempts"]; ok && n != float64(0) {
		t.Fatalf("a drain-kept record must count no report attempt: %s", raw)
	}
	if !filesExist(l.agentDir) {
		t.Fatal("the files must be kept")
	}
}

func TestRunLaunch_PreStartFailureAfterCompletedClaim_SendsNothing(t *testing.T) {
	l := runStartFailedLaunch(t, "pcpre", sfOptions{
		claimCompleted: true,
		download: func(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (api.StartOptions, string, string, error) {
			return opts, "", "workspace download failed", errors.New("simulated download failure")
		},
	})
	if n := l.mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must not run after a failed download, got %d calls", n)
	}
	if n := len(failedReports(l.rtb)); n != 0 {
		t.Fatalf("a failure before Start after a completed claim must send nothing, got %d failed reports", n)
	}
	if n := len(l.mgr.Cleaned()); n != 0 || !filesExist(l.agentDir) {
		t.Fatalf("a failure before Start after a completed claim must clean nothing (handle clean-ups %d)", n)
	}
}

func TestRunLaunch_LocalCancelAfterCompleted_Unchanged(t *testing.T) {
	l := runStartFailedLaunch(t, "pccancel", sfOptions{claimCompleted: true, localCancel: true})
	if n := len(failedReports(l.rtb)); n != 0 {
		t.Fatalf("a local cancel must send no terminal, got %d failed reports", n)
	}
	if n := len(l.mgr.Cleaned()); n != 0 || !filesExist(l.agentDir) {
		t.Fatalf("a local cancel after a completed answer must clean nothing (handle clean-ups %d)", n)
	}
}
