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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const deadInstance = "inst-dead"

func appliedReports(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
	return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
}

// recFakeManager is an asyncManager whose List serves agents and whose
// DeleteTarget records every call and can block.
type recFakeManager struct {
	*asyncManager

	fmu           sync.Mutex
	agents        []api.AgentInfo
	deleted       []runtime.RunRef
	deleteBlock   chan struct{}
	deleteStarted chan struct{}
}

func newRecFakeManager(agents ...api.AgentInfo) *recFakeManager {
	return &recFakeManager{asyncManager: newAsyncManager(), agents: agents}
}

func (m *recFakeManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.fmu.Lock()
	defer m.fmu.Unlock()
	return append([]api.AgentInfo(nil), m.agents...), nil
}

func (m *recFakeManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.fmu.Lock()
	m.deleted = append(m.deleted, ref)
	block, started := m.deleteBlock, m.deleteStarted
	m.fmu.Unlock()
	if started != nil {
		close(started)
	}
	if block != nil {
		<-block
	}
	return true, nil
}

func (m *recFakeManager) Deleted() []runtime.RunRef {
	m.fmu.Lock()
	defer m.fmu.Unlock()
	return append([]runtime.RunRef(nil), m.deleted...)
}

// writeDeadRecord writes a journal record for a dead instance under srv's
// state dir, as an earlier process would have left it, and returns its path.
func writeDeadRecord(t *testing.T, srv *Server, jr journalRecord) string {
	t.Helper()
	dir := filepath.Join(srv.stateDir, journalDirName, deadInstance)
	if err := mkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	jr.Version = journalVersion
	jr.InstanceID = deadInstance
	path := filepath.Join(dir, jr.LaunchID+".json")
	data, err := json.Marshal(jr)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJournalFile(path, data); err != nil {
		t.Fatal(err)
	}
	return path
}

func runRecovery(t *testing.T, srv *Server) {
	t.Helper()
	srv.recoverLaunches()
	done := make(chan struct{})
	go func() { srv.launchDrain.recoveryWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery did not finish")
	}
}

func launchingRecord(id string) journalRecord {
	return journalRecord{
		LaunchID: id, AgentID: "agent-id-" + id, Kind: store.LaunchKindCreate, RunID: "run-" + id,
		Deadline: time.Now().Add(5 * time.Minute), Seq: 3, ProjectID: "proj", Slug: "agent-" + id,
		AgentName: "agent-" + id, RuntimeName: "mock", RuntimeIdentity: "mock", LastStep: api.LaunchStepContainerCreate, State: journalLaunching.String(),
		Handles: []api.ResourceHandle{{Kind: "Pod", Namespace: "ns", Name: "agent-" + id, UID: "uid-" + id}},
	}
}

// TestRecoverLaunches_LaunchingReportsBrokerRestarted is C-T2: a launching
// record is reported failed{broker_restarted} under its own (dead) instance
// ID, and cleaned up only on a confirming answer.
func TestRecoverLaunches_LaunchingReportsBrokerRestarted(t *testing.T) {
	cases := []struct {
		name        string
		answer      func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error)
		wantCleanup bool
	}{
		{"applied", appliedReports, true},
		{"superseded", func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, nil
		}, false},
		{"other_owner", func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, nil
		}, false},
		{"no answer", func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return nil, errors.New("simulated unreachable")
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := newRecFakeManager(api.AgentInfo{Name: "agent-L2", ContainerID: "c-run", RunID: "run-L2"})
			srv, rtb := newAsyncTestServer(t, mgr)
			srv.launchTimingOverride = fastLaunchTimings(fastTestKeepaliveInterval)
			srv.launchRecoveryTTL = 300 * time.Millisecond
			rtb.launchReportFunc = c.answer
			path := writeDeadRecord(t, srv, launchingRecord("L2"))

			runRecovery(t, srv)

			reports := terminalReports(rtb)
			if len(reports) == 0 {
				t.Fatal("no terminal report sent")
			}
			r := reports[0]
			if r.State != hubclient.AgentLaunchReportStateFailed || r.ErrorCode != api.LaunchCodeBrokerRestarted ||
				r.Step != api.LaunchStepContainerCreate || r.LaunchID != "L2" {
				t.Fatalf("unexpected recovery report: %+v", r)
			}
			if r.InstanceID != deadInstance {
				t.Fatalf("recovery report InstanceID = %q, want the journalled %q (not this process's %q)", r.InstanceID, deadInstance, srv.launchInstanceID)
			}
			gotCleanup := mgr.CleanupCallCount() > 0
			if gotCleanup != c.wantCleanup {
				t.Fatalf("handle cleanup = %v, want %v", gotCleanup, c.wantCleanup)
			}
			if c.wantCleanup {
				if mgr.cleanupLast == nil || mgr.cleanupLast[0].UID != "uid-L2" {
					t.Fatalf("cleanup did not get the journalled handles: %+v", mgr.cleanupLast)
				}
				if d := mgr.Deleted(); len(d) != 1 || d[0].RunID != "run-L2" || d[0].ID != "c-run" {
					t.Fatalf("expected the exact-run entry deleted, got %+v", d)
				}
			} else if d := mgr.Deleted(); len(d) != 0 {
				t.Fatalf("expected no delete, got %+v", d)
			}
			if c.name == "no answer" {
				// Kept for the next boot to resend (ptone/scion#5186).
				if jr := readJournalFileT(t, path); jr.State != "terminal_pending" || jr.ReportAttempts != 1 {
					t.Fatalf("an unanswered report did not keep the record for a retry: %+v", jr)
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("recovered record not removed: %v", err)
			}
			if srv.launchRegistry.anyInFlight(launchKey{ProjectID: "proj", Slug: "agent-L2"}) {
				t.Fatal("recovery left a registry entry")
			}
		})
	}
}

// TestRecoverLaunches_TerminalPendingSucceededResent is C-T3: a journalled
// succeeded is resent unchanged and nothing is deleted.
func TestRecoverLaunches_TerminalPendingSucceededResent(t *testing.T) {
	mgr := newRecFakeManager(api.AgentInfo{Name: "agent-L3", ContainerID: "c-run", RunID: "run-L3"})
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = appliedReports
	jr := launchingRecord("L3")
	jr.State = journalTerminalPending.String()
	jr.Terminal = &journalTerminal{Succeeded: true, AgentInfo: &hubclient.AgentLaunchReportInfo{ID: "c-run", Name: "agent-L3", RunID: "run-L3"}}
	path := writeDeadRecord(t, srv, jr)

	runRecovery(t, srv)

	reports := terminalReports(rtb)
	if len(reports) != 1 {
		t.Fatalf("expected one terminal report, got %d", len(reports))
	}
	r := reports[0]
	if r.State != hubclient.AgentLaunchReportStateSucceeded || r.ErrorCode != "" || r.Agent == nil || r.Agent.RunID != "run-L3" || r.InstanceID != deadInstance {
		t.Fatalf("succeeded was not resent unchanged: %+v", r)
	}
	if mgr.CleanupCallCount() != 0 || len(mgr.Deleted()) != 0 {
		t.Fatalf("a resent succeeded must delete nothing (cleanup %d, deleted %+v)", mgr.CleanupCallCount(), mgr.Deleted())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("recovered record not removed: %v", err)
	}
}

// TestRecoverLaunches_ExactRunDelete is C-T5 (C-AC5): on a confirming
// answer, the entry labelled with the launch's run is removed through
// DeleteTarget, on the default and on an auxiliary (kubernetes) runtime; an
// entry with another run's label or no run label is never touched.
func TestRecoverLaunches_ExactRunDelete(t *testing.T) {
	entries := func(id string) []api.AgentInfo {
		return []api.AgentInfo{
			{Name: "agent-" + id, ContainerID: "c-legacy"},
			{Name: "agent-" + id, ContainerID: "c-other", RunID: "run-other"},
		}
	}
	for _, rtName := range []string{"mock", "kubernetes"} {
		t.Run(rtName+" only legacy and other-run entries", func(t *testing.T) {
			mgr := newRecFakeManager(entries("L5")...)
			srv, rtb := newAsyncTestServer(t, newAsyncManager())
			installRecoveryManager(srv, rtName, mgr)
			rtb.launchReportFunc = appliedReports
			jr := launchingRecord("L5")
			jr.RuntimeName, jr.RuntimeIdentity = rtName, rtName
			writeDeadRecord(t, srv, jr)
			runRecovery(t, srv)
			if d := mgr.Deleted(); len(d) != 0 {
				t.Fatalf("an unlabelled or other-run entry was deleted: %+v", d)
			}
		})
		t.Run(rtName+" exact run entry", func(t *testing.T) {
			mgr := newRecFakeManager(append(entries("L5"), api.AgentInfo{Name: "agent-L5", ContainerID: "c-run", RunID: "run-L5"})...)
			srv, rtb := newAsyncTestServer(t, newAsyncManager())
			installRecoveryManager(srv, rtName, mgr)
			rtb.launchReportFunc = appliedReports
			jr := launchingRecord("L5")
			jr.RuntimeName, jr.RuntimeIdentity = rtName, rtName
			writeDeadRecord(t, srv, jr)
			runRecovery(t, srv)
			if d := mgr.Deleted(); len(d) != 1 || d[0].ID != "c-run" || d[0].RunID != "run-L5" {
				t.Fatalf("expected only the exact-run entry deleted, got %+v", d)
			}
		})
	}
	t.Run("runtime no longer configured", func(t *testing.T) {
		mgr := newAsyncManager()
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = appliedReports
		jr := launchingRecord("L5x")
		jr.RuntimeName, jr.RuntimeIdentity = "podman", "podman"
		path := writeDeadRecord(t, srv, jr)
		runRecovery(t, srv)
		if len(terminalReports(rtb)) != 1 {
			t.Fatal("the launch was not reported")
		}
		if mgr.CleanupCallCount() != 0 {
			t.Fatal("cleanup ran against a runtime that is not the launch's")
		}
		// The runtime may come back (a later boot's auxiliary discovery),
		// so the clean-up is kept for a bounded retry (ptone/scion#5177).
		if got := readJournalFileT(t, path); got.State != "cleanup_pending" || got.CleanupAttempts != 1 {
			t.Fatalf("a launch whose runtime is unavailable was not kept for a retry: %+v", got)
		}
	})
}

// installRecoveryManager makes mgr the manager of runtime rtName on srv:
// the default manager for srv's default runtime ("mock"), else an
// auxiliary runtime.
func installRecoveryManager(srv *Server, rtName string, mgr agent.Manager) {
	if rtName == "mock" {
		srv.mu.Lock()
		srv.manager = mgr
		srv.mu.Unlock()
		return
	}
	srv.auxiliaryRuntimesMu.Lock()
	rt := &runtime.MockRuntime{NameFunc: func() string { return rtName }}
	srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rt)] = auxiliaryRuntime{Runtime: rt, Manager: mgr}
	srv.auxiliaryRuntimesMu.Unlock()
}

// TestRecoverLaunches_NoRegistryEntry is C-T9 (H2, C-AC9).
func TestRecoverLaunches_NoRegistryEntry(t *testing.T) {
	key := launchKey{ProjectID: "proj", Slug: "agent-L9"}

	t.Run("TryBegin refuses a held key and never cancels", func(t *testing.T) {
		r := newLaunchRegistry()
		cancelled := false
		holder := newLaunchRecord("L-h", "a", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() { cancelled = true })
		r.Begin(key, holder)
		if r.TryBegin(key, newLaunchRecord("L-t", "a", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})) {
			t.Fatal("TryBegin took a held key")
		}
		if cancelled || r.records[key] != holder {
			t.Fatal("TryBegin disturbed the holder")
		}
	})

	t.Run("synchronous start of the name in flight", func(t *testing.T) {
		mgr := newRecFakeManager(api.AgentInfo{Name: "agent-L9", ContainerID: "c-run", RunID: "run-L9"})
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = appliedReports
		_, finish := srv.startsInFlight.begin(context.Background(), key)
		defer finish()
		writeDeadRecord(t, srv, launchingRecord("L9"))
		runRecovery(t, srv)
		if d := mgr.Deleted(); len(d) != 0 {
			t.Fatalf("name-addressed delete ran while a start of the name was tracked: %+v", d)
		}
		if !registryEmpty(srv) {
			t.Fatal("recovery left a registry slot")
		}
	})

	t.Run("newer start registered before recovery", func(t *testing.T) {
		projectDir := t.TempDir()
		agentDir := filepath.Join(projectDir, "agents", "agent-L9")
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeLaunchMarker(projectDir, false, "agent-L9", "L9"); err != nil {
			t.Fatal(err)
		}
		mgr := newRecFakeManager(api.AgentInfo{Name: "agent-L9", ContainerID: "c-run", RunID: "run-L9"})
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = appliedReports
		cancelled := make(chan struct{})
		var once sync.Once
		newer := newLaunchRecord("L-new", "agent-id-L9", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() { once.Do(func() { close(cancelled) }) })
		srv.launchRegistry.Begin(key, newer)
		jr := launchingRecord("L9")
		jr.ProjectPath = projectDir
		writeDeadRecord(t, srv, jr)

		runRecovery(t, srv)

		select {
		case <-cancelled:
			t.Fatal("recovery cancelled the newer start")
		default:
		}
		if srv.launchRegistry.records[key] != newer {
			t.Fatal("recovery replaced the newer start's registration")
		}
		if d := mgr.Deleted(); len(d) != 0 {
			t.Fatalf("name-addressed delete ran while a newer start held the name: %+v", d)
		}
		if _, err := os.Stat(agentDir); err != nil {
			t.Fatalf("agent files deleted while a newer start held the name: %v", err)
		}
		if mgr.CleanupCallCount() != 1 {
			t.Fatalf("UID-preconditioned handle cleanup should still run, got %d", mgr.CleanupCallCount())
		}
	})

	t.Run("newer start arrives during the slot", func(t *testing.T) {
		projectDir := t.TempDir()
		agentDir := filepath.Join(projectDir, "agents", "agent-L9")
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeLaunchMarker(projectDir, false, "agent-L9", "L9"); err != nil {
			t.Fatal(err)
		}
		mgr := newRecFakeManager(api.AgentInfo{Name: "agent-L9", ContainerID: "c-run", RunID: "run-L9"})
		mgr.deleteBlock = make(chan struct{})
		mgr.deleteStarted = make(chan struct{})
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = appliedReports
		jr := launchingRecord("L9")
		jr.ProjectPath = projectDir
		writeDeadRecord(t, srv, jr)

		srv.recoverLaunches()
		select {
		case <-mgr.deleteStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("recovery never reached its run-scoped delete")
		}
		newer := newLaunchRecord("L-new", "agent-id-L9", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
		superseded := srv.launchRegistry.Begin(key, newer)
		if superseded == nil {
			t.Fatal("the newer start found no slot to wait for")
		}
		close(mgr.deleteBlock) // the one in-flight name-addressed call returns
		select {
		case <-superseded:
		case <-time.After(5 * time.Second):
			t.Fatal("the slot was not released after its in-flight call")
		}
		srv.launchDrain.recoveryWG.Wait()
		if srv.launchRegistry.records[key] != newer {
			t.Fatal("recovery's Finish dropped the newer start's registration")
		}
		if _, err := os.Stat(agentDir); err != nil {
			t.Fatalf("file deletion ran after a newer start took the name: %v", err)
		}
	})
}

// TestRecoveryTerminalTTL is C-T11 (L5): a stale record still gets a send
// attempt of at least 30s; a fresh one gets its report TTL.
func TestRecoveryTerminalTTL(t *testing.T) {
	now := time.Now()
	if got := recoveryTerminalTTL(now.Add(-time.Hour), now); got != 30*time.Second {
		t.Fatalf("stale record TTL = %v, want 30s", got)
	}
	if got := recoveryTerminalTTL(now.Add(5*time.Minute), now); got != 15*time.Minute {
		t.Fatalf("fresh record TTL = %v, want 15m", got)
	}
}

// TestRecoverLaunches_DispatchAttemptReplay is C-T6: after a crash, the
// restarted broker replays the persisted 201 for the dead launch's request
// ID without launching it again, and recovery still reports the launch
// failed{broker_restarted} under the dead instance.
func TestRecoverLaunches_DispatchAttemptReplay(t *testing.T) {
	mgr1 := newAsyncManager()
	mgr1.startBlock = make(chan struct{})
	srv1, _ := newAsyncTestServer(t, mgr1)
	t.Cleanup(func() {
		// Let the first process's launch finish before its temp dirs go.
		close(mgr1.startBlock)
		waitUntil(t, 5*time.Second, func() bool {
			srv1.launchRegistry.mu.Lock()
			defer srv1.launchRegistry.mu.Unlock()
			return len(srv1.launchRegistry.records) == 0
		})
	})
	body := map[string]any{
		"name": "agent-6", "requestId": "req-6", "asyncLaunch": true, "launchId": "L-6",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	}
	w1 := postCreate(t, srv1, body)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w1.Code, w1.Body.String())
	}
	resp1 := decodeCreateResponse(t, w1)
	if !waitUntil(t, 2*time.Second, func() bool { return mgr1.StartCallCount() == 1 }) {
		t.Fatal("first launch never started")
	}
	srv1.launchJournal.Close() // the process dies: its lock is released

	cfg := srv1.config
	cfg.StateDir = srv1.stateDir
	mgr2 := newAsyncManager()
	srv2 := New(cfg, mgr2, &runtime.MockRuntime{NameFunc: func() string { return "mock" }}, config.RuntimeBuildRecord{})
	rtb2 := &mockRuntimeBrokerService{launchReportFunc: appliedReports}
	srv2.hubMu.Lock()
	srv2.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-on-a", HubClient: &stubBrokerHubClient{brokers: rtb2}}
	srv2.hubMu.Unlock()

	w2 := postCreate(t, srv2, body)
	if w2.Code != http.StatusCreated {
		t.Fatalf("replayed create: %d %s", w2.Code, w2.Body.String())
	}
	if resp2 := decodeCreateResponse(t, w2); resp2.LaunchID != resp1.LaunchID || resp2.LaunchInstanceID != resp1.LaunchInstanceID {
		t.Fatalf("replay returned a different accepted response: %+v vs %+v", resp2, resp1)
	}
	if mgr2.StartCallCount() != 0 {
		t.Fatal("the replay launched again")
	}

	runRecovery(t, srv2)
	reports := terminalReports(rtb2)
	if len(reports) != 1 || reports[0].LaunchID != "L-6" || reports[0].ErrorCode != api.LaunchCodeBrokerRestarted || reports[0].InstanceID != srv1.launchInstanceID {
		t.Fatalf("expected failed{broker_restarted} for L-6 under the dead instance, got %+v", reports)
	}
}

// TestRecoverLaunches_ResolvesRuntimeByIdentity covers R1 of the C1 review
// (design wave 3 §1 C-c step 2): with two runtimes of one type (two
// Kubernetes clusters), recovery cleans up on the one the launch ran on,
// matched by its journalled identity, and never on the other; a launch
// whose identity is unknown is cleaned up nowhere.
func TestRecoverLaunches_ResolvesRuntimeByIdentity(t *testing.T) {
	rtA := &runtime.KubernetesRuntime{DefaultNamespace: "ns-a"}
	rtB := &runtime.KubernetesRuntime{DefaultNamespace: "ns-b"}
	if auxiliaryRuntimeIdentity(rtA) == auxiliaryRuntimeIdentity(rtB) {
		t.Fatal("test setup: the two runtimes must differ in identity")
	}
	setup := func(t *testing.T) (*Server, *mockRuntimeBrokerService, *recFakeManager, *recFakeManager) {
		entry := api.AgentInfo{Name: "agent-LK", ContainerID: "c-run", RunID: "run-LK"}
		mgrA, mgrB := newRecFakeManager(entry), newRecFakeManager(entry)
		srv, rtb := newAsyncTestServer(t, newAsyncManager())
		srv.auxiliaryRuntimesMu.Lock()
		srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rtA)] = auxiliaryRuntime{Runtime: rtA, Manager: mgrA}
		srv.auxiliaryRuntimes[auxiliaryRuntimeIdentity(rtB)] = auxiliaryRuntime{Runtime: rtB, Manager: mgrB}
		srv.auxiliaryRuntimesMu.Unlock()
		rtb.launchReportFunc = appliedReports
		return srv, rtb, mgrA, mgrB
	}

	t.Run("launch on the second cluster", func(t *testing.T) {
		srv, _, mgrA, mgrB := setup(t)
		jr := launchingRecord("LK")
		jr.RuntimeName, jr.RuntimeIdentity = "kubernetes", auxiliaryRuntimeIdentity(rtB)
		writeDeadRecord(t, srv, jr)
		runRecovery(t, srv)
		if mgrB.CleanupCallCount() != 1 || len(mgrB.Deleted()) != 1 {
			t.Fatalf("the launch's own cluster was not cleaned up (cleanup %d, deleted %+v)", mgrB.CleanupCallCount(), mgrB.Deleted())
		}
		if mgrA.CleanupCallCount() != 0 || len(mgrA.Deleted()) != 0 {
			t.Fatalf("the other cluster of the same type was cleaned up (cleanup %d, deleted %+v)", mgrA.CleanupCallCount(), mgrA.Deleted())
		}
	})
	// The profile fallback (the identity is in neither the default nor the
	// auxiliary map) keeps the manager its profile resolves to only if that
	// manager's identity is the journalled one: here the profile resolves
	// to the default runtime ("mock"), a different identity, so nothing is
	// cleaned up anywhere.
	t.Run("profile resolves to another runtime", func(t *testing.T) {
		srv, rtb, mgrA, mgrB := setup(t)
		def := newRecFakeManager(api.AgentInfo{Name: "agent-LK", ContainerID: "c-run", RunID: "run-LK"})
		installRecoveryManager(srv, "mock", def)
		rtC := &runtime.KubernetesRuntime{DefaultNamespace: "ns-c"}
		jr := launchingRecord("LK")
		jr.RuntimeName, jr.RuntimeIdentity, jr.Profile = "kubernetes", auxiliaryRuntimeIdentity(rtC), "cluster-c"
		writeDeadRecord(t, srv, jr)
		runRecovery(t, srv)
		if len(terminalReports(rtb)) != 1 {
			t.Fatal("the launch was not reported")
		}
		if n := def.CleanupCallCount() + mgrA.CleanupCallCount() + mgrB.CleanupCallCount(); n != 0 {
			t.Fatalf("recovery cleaned up on a runtime whose identity is not the launch's (%d cleanup calls)", n)
		}
		if d := len(def.Deleted()) + len(mgrA.Deleted()) + len(mgrB.Deleted()); d != 0 {
			t.Fatalf("recovery deleted on a runtime whose identity is not the launch's (%d deletes)", d)
		}
	})
	t.Run("identity unknown", func(t *testing.T) {
		srv, rtb, mgrA, mgrB := setup(t)
		jr := launchingRecord("LK")
		jr.RuntimeName, jr.RuntimeIdentity = "kubernetes", ""
		writeDeadRecord(t, srv, jr)
		runRecovery(t, srv)
		if len(terminalReports(rtb)) != 1 {
			t.Fatal("the launch was not reported")
		}
		if mgrA.CleanupCallCount()+mgrB.CleanupCallCount() != 0 || len(mgrA.Deleted())+len(mgrB.Deleted()) != 0 {
			t.Fatal("recovery guessed a runtime for a launch of unknown identity")
		}
	})
}

// TestLaunchRuntimeIdentity covers the identity admission journals: an
// AgentManager's own runtime, else the default or auxiliary runtime the
// manager belongs to.
func TestLaunchRuntimeIdentity(t *testing.T) {
	srv, _ := newAsyncTestServer(t, newAsyncManager())
	rtB := &runtime.KubernetesRuntime{DefaultNamespace: "ns-b"}
	if got := srv.launchRuntimeIdentity(agent.NewManager(rtB)); got != auxiliaryRuntimeIdentity(rtB) {
		t.Fatalf("AgentManager identity = %q, want %q", got, auxiliaryRuntimeIdentity(rtB))
	}
	if got := srv.launchRuntimeIdentity(srv.currentManager()); got != "mock" {
		t.Fatalf("default manager identity = %q, want mock", got)
	}
	auxMgr := newAsyncManager()
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes|context=c|namespace=x"] = auxiliaryRuntime{Runtime: rtB, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()
	if got := srv.launchRuntimeIdentity(auxMgr); got != "kubernetes|context=c|namespace=x" {
		t.Fatalf("auxiliary manager identity = %q", got)
	}
	if got := srv.launchRuntimeIdentity(newAsyncManager()); got != "" {
		t.Fatalf("unknown manager identity = %q, want empty", got)
	}
}
