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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Run-scoped stop against starts in flight on the start tracker
// (ptone/scion#2550 P3 merged with GoogleCloudPlatform/scion#2482): a stop
// naming a run cancels and waits only for starts of that run (or with no
// run recorded), never for a start of another run.

func actionWithRun(srv *Server, action, query, body string) *httptest.ResponseRecorder {
	path := "/api/v1/agents/same-name/" + action
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func trackedRunEntry(id, runID string) api.AgentInfo {
	return api.AgentInfo{ID: id, Name: "same-name", Phase: "running", RunID: runID,
		Labels: map[string]string{api.LabelRunID: runID}}
}

func setAgents(mgr *startFuncManager, agents ...api.AgentInfo) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.agents = agents
}

// startInFlight runs a start of runID (empty: no runId in the body) that
// blocks until cancelled or released, and waits until it is running.
func startInFlight(t *testing.T, srv *Server, mgr *startFuncManager, runID string, cleanup func()) (release chan struct{}, done chan *httptest.ResponseRecorder) {
	t.Helper()
	started := make(chan struct{})
	release = make(chan struct{})
	blockedStart(mgr, started, release, cleanup)
	body := `{}`
	if runID != "" {
		body = `{"runId":"` + runID + `"}`
	}
	done = make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- actionWithRun(srv, "start", "", body) }()
	waitSignal(t, started, "the start")
	return release, done
}

func assertStartNotCancelled(t *testing.T, srv *Server, release chan struct{}, done chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case w := <-done:
		t.Fatalf("the other run's start returned before release (cancelled): %d %s", w.Code, w.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	if !snapshotHas(srv, "same-name") {
		t.Fatal("the other run's start is no longer in flight")
	}
	close(release)
	select {
	case w := <-done:
		if w.Code >= 300 {
			t.Fatalf("the other run's start failed after release: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(syncStartTestTimeout):
		t.Fatal("start did not return")
	}
}

func waitStopped(t *testing.T, done chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the stop did not cancel the in-flight start")
	}
}

// A stale run-scoped stop while a newer run's start is in flight (no
// container yet) gets the run-mismatch 404 and cancels nothing: the start
// keeps running and no runtime stop happens.
func TestStopAgent_StaleRunIDLeavesOtherRunsTrackedStart(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	release, done := startInFlight(t, srv, mgr, "run-b", nil)

	sw := actionWithRun(srv, "stop", "runId=run-a", "")
	if sw.Code != http.StatusNotFound {
		t.Fatalf("stale stop: status %d, want 404: %s", sw.Code, sw.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(sw.Body.Bytes(), &body); err != nil || body.Error.Code != api.BrokerErrorCodeRunMismatch ||
		body.Error.Details[api.BrokerErrorDetailCurrentRunID] != "run-b" {
		t.Errorf("stale stop body = %s, want run_mismatch naming run-b", sw.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0", mgr.StopCalls())
	}
	assertStartNotCancelled(t, srv, release, done)
}

// A stale run-scoped stop that matches its own leftover container stops
// only that container, and still leaves a newer run's in-flight start
// alone: the cancel-and-wait is limited to the stop's run.
func TestStopAgent_RunScopedCancelSkipsOtherRunsStart(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	setAgents(mgr, trackedRunEntry("c-old", "run-a"))
	release, done := startInFlight(t, srv, mgr, "run-b", nil)

	sw := actionWithRun(srv, "stop", "runId=run-a", "")
	if sw.Code != http.StatusAccepted {
		t.Fatalf("stop: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-old" {
		t.Errorf("stop calls = %d, last %q; want one stop of c-old", mgr.StopCalls(), mgr.lastStopAgentID)
	}
	assertStartNotCancelled(t, srv, release, done)
}

// A run-scoped stop while a start of the same run is in flight cancels it
// and waits for its cleanup before stopping, then stops the container. A
// start with no run recorded (an older hub's, or one whose handler has not
// read its run yet) is treated as the same run. Never a 404.
func TestStopAgent_RunScopedCancelsAndWaitsForSameRunStart(t *testing.T) {
	for _, tc := range []struct{ name, startRun string }{
		{"same run", "run-b"},
		{"no run recorded", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, _, _ := newSyncStartTestServer(t)
			setAgents(mgr, trackedRunEntry("c-1", "run-b"))
			var stopsDuringCleanup int
			_, done := startInFlight(t, srv, mgr, tc.startRun, func() {
				time.Sleep(30 * time.Millisecond)
				stopsDuringCleanup = mgr.StopCalls()
			})

			stopDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
			waitStopped(t, done)
			sw := <-stopDone
			if sw.Code != http.StatusAccepted {
				t.Fatalf("stop: status %d, want 202: %s", sw.Code, sw.Body.String())
			}
			if stopsDuringCleanup != 0 {
				t.Fatal("the stop ran before the cancelled start finished its cleanup")
			}
			if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-1" || mgr.lastStopRunID != "run-b" {
				t.Errorf("stop calls = %d, last {%q, %q}; want one stop of {c-1, run-b}",
					mgr.StopCalls(), mgr.lastStopAgentID, mgr.lastStopRunID)
			}
		})
	}
}

// A run-scoped stop that cancelled its own run's in-flight start (the
// tracked-start leg of cancelledOwn, not a woken launch), and whose
// StopTarget then gets a runtime run mismatch (for example Kubernetes
// reporting the pod recreated under a new UID), did act: it is accepted
// with 202 and a forced heartbeat, never the zero-side-effect 404.
func TestStopAgent_RuntimeRunMismatchAfterCancelledOwnStart_202(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	hubSvc := attachHeartbeat(srv, mgr)
	setAgents(mgr, trackedRunEntry("c-1", "run-b"))
	mgr.mu.Lock()
	mgr.stopErr = fmt.Errorf("pod ns/same-name was replaced before it could be deleted: %w", runtime.ErrRunMismatch)
	mgr.mu.Unlock()
	var stopsDuringCleanup int
	_, done := startInFlight(t, srv, mgr, "run-b", func() {
		time.Sleep(30 * time.Millisecond)
		stopsDuringCleanup = mgr.StopCalls()
	})

	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
	waitStopped(t, done)
	sw := <-stopDone
	if sw.Code != http.StatusAccepted {
		t.Fatalf("stop: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if stopsDuringCleanup != 0 {
		t.Fatal("the stop ran before the cancelled start finished its cleanup")
	}
	if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-1" || mgr.lastStopRunID != "run-b" {
		t.Errorf("stop calls = %d, last {%q, %q}; want one stop of {c-1, run-b}",
			mgr.StopCalls(), mgr.lastStopAgentID, mgr.lastStopRunID)
	}
	if n := waitHubHeartbeats(hubSvc, 1, time.Second); n == 0 {
		t.Error("the accepted stop did not force a heartbeat")
	}
}

// attachHeartbeat gives srv a hub connection whose heartbeat service reports
// to the returned mock, so forced heartbeats and launch reports are visible.
func attachHeartbeat(srv *Server, mgr *startFuncManager) *mockRuntimeBrokerService {
	hubSvc := &mockRuntimeBrokerService{}
	hb := NewHeartbeatService(hubSvc, "test-host", time.Hour, mgr, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.hubMu.Lock()
	srv.hubConnections["local"] = &HubConnection{Name: "local", Heartbeat: hb}
	srv.hubMu.Unlock()
	return hubSvc
}

// waitHubHeartbeats waits up to d for at least want heartbeats and returns
// the count seen (forced heartbeats are sent asynchronously).
func waitHubHeartbeats(hubSvc *mockRuntimeBrokerService, want int, d time.Duration) int {
	deadline := time.Now().Add(d)
	for {
		n := len(hubSvc.getHeartbeatCalls())
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A run-scoped stop whose StopTarget gets a runtime run mismatch, with
// another run's tracked start and async launch both in flight, cancelled
// nothing of its own: it answers the run-mismatch 404 with no side effects.
// The other run's start keeps running and its launch is not woken, and no
// heartbeat or launch report goes to the hub.
func TestStopAgent_RuntimeRunMismatch_OtherRunsStartAndLaunchUntouched(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	hubSvc := attachHeartbeat(srv, mgr)
	setAgents(mgr, trackedRunEntry("c-b", "run-b"))
	mgr.mu.Lock()
	mgr.stopErr = fmt.Errorf("pod ns/same-name belongs to run %q, not %q: %w", "run-c", "run-b", runtime.ErrRunMismatch)
	mgr.mu.Unlock()
	relA, doneA := startInFlight(t, srv, mgr, "run-a", nil)
	var launchCancels atomic.Int32
	rec := newLaunchRecord("async-a", "same-name", "create", "", time.Time{}, func() { launchCancels.Add(1) })
	rec.RunID = "run-a"
	key := launchKey{Slug: "same-name"}
	srv.launchRegistry.Begin(key, rec)

	sw := actionWithRun(srv, "stop", "runId=run-b", "")
	if sw.Code != http.StatusNotFound {
		t.Fatalf("stop: status %d, want 404: %s", sw.Code, sw.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(sw.Body.Bytes(), &body); err != nil || body.Error.Code != api.BrokerErrorCodeRunMismatch {
		t.Errorf("stop body = %s, want %s", sw.Body.String(), api.BrokerErrorCodeRunMismatch)
	}
	if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-b" || mgr.lastStopRunID != "run-b" {
		t.Errorf("stop calls = %d, last {%q, %q}; want one stop of {c-b, run-b}",
			mgr.StopCalls(), mgr.lastStopAgentID, mgr.lastStopRunID)
	}
	if n := launchCancels.Load(); n != 0 {
		t.Errorf("the other run's launch was woken (%d cancels)", n)
	}
	if !srv.launchRegistry.runInFlight(key, "run-a") {
		t.Error("the other run's launch is no longer registered")
	}
	if n := waitHubHeartbeats(hubSvc, 1, 200*time.Millisecond); n != 0 {
		t.Errorf("run-mismatched stop forced %d heartbeat(s)", n)
	}
	if n := len(hubSvc.getLaunchReports()); n != 0 {
		t.Errorf("run-mismatched stop sent %d launch report(s)", n)
	}
	assertStartNotCancelled(t, srv, relA, doneA)
}

// After cancelling its own run's start, a run-scoped stop resolves again:
// a container the start left behind is stopped, and when the re-lookup
// finds nothing the stop is the idempotent 202, as the legacy stop is.
func TestStopAgent_RunScopedReLookupAfterCancelledStart(t *testing.T) {
	t.Run("container left by the start is stopped", func(t *testing.T) {
		srv, mgr, _, _ := newSyncStartTestServer(t)
		_, done := startInFlight(t, srv, mgr, "run-b", func() {
			setAgents(mgr, trackedRunEntry("c-1", "run-b"))
		})
		stopDone := make(chan *httptest.ResponseRecorder, 1)
		go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
		waitStopped(t, done)
		if sw := <-stopDone; sw.Code != http.StatusAccepted {
			t.Fatalf("stop: status %d, want 202: %s", sw.Code, sw.Body.String())
		}
		if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-1" {
			t.Errorf("stop calls = %d, last %q; want one stop of c-1", mgr.StopCalls(), mgr.lastStopAgentID)
		}
	})
	t.Run("re-lookup finds nothing", func(t *testing.T) {
		srv, mgr, _, _ := newSyncStartTestServer(t)
		_, done := startInFlight(t, srv, mgr, "run-b", nil)
		stopDone := make(chan *httptest.ResponseRecorder, 1)
		go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
		waitStopped(t, done)
		if sw := <-stopDone; sw.Code != http.StatusAccepted {
			t.Fatalf("stop: status %d, want 202: %s", sw.Code, sw.Body.String())
		}
		if mgr.StopCalls() != 0 {
			t.Errorf("stop calls = %d, want 0", mgr.StopCalls())
		}
	})
}

// The tracker records each start's run and selects by it; an empty run
// selects every start, as before.
func TestStartTracker_RunSelection(t *testing.T) {
	tr := newStartTracker()
	k := launchKey{ProjectID: "p", Slug: "dev"}
	ctxB, finishB := tr.begin(context.Background(), k)
	defer finishB()
	tr.setRunID(ctxB, "run-b")
	ctxNone, finishNone := tr.begin(context.Background(), k)
	defer finishNone()

	if current, ok := tr.otherRun(k, "run-a"); !ok || current != "run-b" {
		t.Errorf("otherRun(run-a) = (%q, %v), want (run-b, true)", current, ok)
	}
	if _, ok := tr.otherRun(k, "run-b"); ok {
		t.Error("otherRun(run-b) reported its own run")
	}
	if _, ok := tr.otherRun(k, ""); ok {
		t.Error("otherRun with no run ID must never count")
	}

	// run-a selects only the start with no run recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, n := tr.cancelAndWaitRun(ctx, k, "run-a"); n != 1 {
		t.Errorf("cancelAndWaitRun(run-a) selected %d starts, want 1 (the unlabelled one)", n)
	}
	if ctxB.Err() != nil {
		t.Error("run-a's cancel cancelled run-b's start")
	}
	if ctxNone.Err() == nil {
		t.Error("the start with no run recorded was not cancelled")
	}
	if _, n := tr.cancelAndWaitRun(ctx, k, ""); n != 2 {
		t.Errorf("cancelAndWaitRun(\"\") selected %d starts, want 2", n)
	}

	// setRunID on a context without a tracked start, or with no run, is a
	// no-op.
	tr.setRunID(context.Background(), "run-x")
	tr.setRunID(ctxNone, "")
	var nilTracker *startTracker
	nilTracker.setRunID(ctxB, "run-x")
	if _, ok := nilTracker.otherRun(k, "run-a"); ok {
		t.Error("nil tracker reported a start")
	}
}

// A stale start of run-a (its hub cancel lost)
// and the current run-b start are both tracked. A stop for run-b with no
// container yet cancels run-b's start and is accepted; run-a's start, of
// another run, is left alone. Never a 404 that would lose the stop.
func TestStopAgent_BothRunsTrackedStopCurrentRun(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	relA, doneA := startInFlight(t, srv, mgr, "run-a", nil)
	_, doneB := startInFlight(t, srv, mgr, "run-b", nil)

	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
	waitStopped(t, doneB)
	if sw := <-stopDone; sw.Code != http.StatusAccepted {
		t.Fatalf("stop run-b: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0 (no container)", mgr.StopCalls())
	}
	assertStartNotCancelled(t, srv, relA, doneA)
}

// The re-lookup after the cancel: run-b's cancelled start leaves its
// container, which the stop then stops, still with run-a's start tracked.
func TestStopAgent_BothRunsTrackedReLookupStopsOwnContainer(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	relA, doneA := startInFlight(t, srv, mgr, "run-a", nil)
	_, doneB := startInFlight(t, srv, mgr, "run-b", func() {
		setAgents(mgr, trackedRunEntry("c-b", "run-b"))
	})

	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
	waitStopped(t, doneB)
	if sw := <-stopDone; sw.Code != http.StatusAccepted {
		t.Fatalf("stop run-b: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-b" || mgr.lastStopRunID != "run-b" {
		t.Errorf("stop calls = %d, last {%q, %q}; want one stop of {c-b, run-b}",
			mgr.StopCalls(), mgr.lastStopAgentID, mgr.lastStopRunID)
	}
	assertStartNotCancelled(t, srv, relA, doneA)
}

// A stop for run-b while run-b's start is in flight and run-a's stale
// container still holds the name (for example during a restart): the stop
// still cancels run-b's start, so it is not lost, and never stops run-a's
// container. Run-b is then gone, so the stop is accepted (202), not the
// run-mismatch 404 that would make the hub keep showing run-b as running.
func TestStopAgent_OwnStartTrackedWhileOtherRunHoldsName(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	setAgents(mgr, trackedRunEntry("c-a", "run-a"))
	_, doneB := startInFlight(t, srv, mgr, "run-b", nil)

	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- actionWithRun(srv, "stop", "runId=run-b", "") }()
	waitStopped(t, doneB)
	sw := <-stopDone
	if sw.Code != http.StatusAccepted {
		t.Fatalf("stop run-b: status %d, want 202 (run-b's start was cancelled): %s", sw.Code, sw.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0: run-a's container must not be stopped", mgr.StopCalls())
	}
}

// A stop for run-b whose only in-flight work is its
// registered async launch, with run-a's container holding the name, wakes
// that launch; waking it counts as cancelling its own run, so the stop is
// accepted (202), and run-a's container is not stopped.
func TestStopAgent_OwnLaunchWokenWhileOtherRunHoldsName(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	setAgents(mgr, trackedRunEntry("c-a", "run-a"))
	launchCancels := 0
	rec := newLaunchRecord("async-1", "same-name", "create", "", time.Time{}, func() { launchCancels++ })
	rec.RunID = "run-b"
	srv.launchRegistry.Begin(launchKey{Slug: "same-name"}, rec)

	sw := actionWithRun(srv, "stop", "runId=run-b", "")
	if sw.Code != http.StatusAccepted {
		t.Fatalf("stop run-b: status %d, want 202 (run-b's launch was woken): %s", sw.Code, sw.Body.String())
	}
	if launchCancels == 0 {
		t.Error("the stop did not wake run-b's launch")
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0: run-a's container must not be stopped", mgr.StopCalls())
	}
}

// A launch with no run recorded never suppresses the refusal of a stale
// stop: with run-b's container holding the name, a stop for run-a gets the
// 404 before any cancel, the run-less launch is not woken, and nothing is
// stopped.
func TestStopAgent_RunlessLaunchDoesNotSuppressStaleRefusal(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	setAgents(mgr, trackedRunEntry("c-b", "run-b"))
	launchCancels := 0
	rec := newLaunchRecord("async-1", "same-name", "create", "", time.Time{}, func() { launchCancels++ })
	srv.launchRegistry.Begin(launchKey{Slug: "same-name"}, rec)

	sw := actionWithRun(srv, "stop", "runId=run-a", "")
	if sw.Code != http.StatusNotFound {
		t.Fatalf("stale stop: status %d, want 404: %s", sw.Code, sw.Body.String())
	}
	if launchCancels != 0 {
		t.Errorf("the stale stop woke the run-less launch (%d)", launchCancels)
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0", mgr.StopCalls())
	}
}

// hookListManager runs hook once, after the first List call, to change what
// the runtime lists between a stop's two lookups.
type hookListManager struct {
	*startFuncManager
	once sync.Once
	hook func()
}

func (m *hookListManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	out, err := m.startFuncManager.List(ctx, filter)
	if h := m.hook; h != nil {
		m.once.Do(h)
	}
	return out, err
}

func newHookListServer(t *testing.T) (*Server, *hookListManager) {
	t.Helper()
	inner := &startFuncManager{mockManager: &mockManager{}, starts: make(chan func(context.Context, api.StartOptions) (*api.AgentInfo, error), 4)}
	m := &hookListManager{startFuncManager: inner}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	return New(cfg, m, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}), m
}

// Run-b's start finishes, creating c-b, between the stop's first lookup
// and its cancel. The run-scoped stop always resolves again after the
// cancel step, so it stops c-b instead of answering "not found".
func TestStopAgent_OwnStartFinishesBetweenLookupAndCancel(t *testing.T) {
	srv, m := newHookListServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	blockedStart(m.startFuncManager, started, release, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- actionWithRun(srv, "start", "runId=run-b", `{"runId":"run-b"}`) }()
	waitSignal(t, started, "the start")
	m.hook = func() {
		setAgents(m.startFuncManager, trackedRunEntry("c-b", "run-b"))
		close(release)
		<-done
	}

	sw := actionWithRun(srv, "stop", "runId=run-b", "")
	if sw.Code != http.StatusAccepted {
		t.Fatalf("stop run-b: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if m.StopCalls() != 1 || m.lastStopAgentID != "c-b" {
		t.Errorf("stop calls = %d, last %q; want one stop of c-b", m.StopCalls(), m.lastStopAgentID)
	}
}

// The post-cancel refusal: nothing of run-b is in flight or listed at the
// first lookup, and run-a's container appears before the second. Nothing of
// run-b was cancelled, so this is the run-mismatch 404, and run-a's
// container is not stopped.
func TestStopAgent_OtherRunAppearsAfterFirstLookupWithoutOwnCancel(t *testing.T) {
	srv, m := newHookListServer(t)
	m.hook = func() { setAgents(m.startFuncManager, trackedRunEntry("c-a", "run-a")) }

	sw := actionWithRun(srv, "stop", "runId=run-b", "")
	if sw.Code != http.StatusNotFound {
		t.Fatalf("stop run-b: status %d, want 404: %s", sw.Code, sw.Body.String())
	}
	if m.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0: run-a's container must not be stopped", m.StopCalls())
	}
}

// A stop for run-b whose async launch is registered while a tracked start
// of run-a is also in flight: the stop wakes run-b's launch, leaves run-a's
// start alone, and is accepted.
func TestStopAgent_OwnLaunchRegisteredWithOtherRunsStartTracked(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	relA, doneA := startInFlight(t, srv, mgr, "run-a", nil)
	launchCancels := 0
	rec := newLaunchRecord("async-1", "same-name", "create", "", time.Time{}, func() { launchCancels++ })
	rec.RunID = "run-b"
	srv.launchRegistry.Begin(launchKey{Slug: "same-name"}, rec)

	sw := actionWithRun(srv, "stop", "runId=run-b", "")
	if sw.Code != http.StatusAccepted {
		t.Fatalf("stop run-b: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if launchCancels == 0 {
		t.Error("the stop did not wake run-b's launch")
	}
	assertStartNotCancelled(t, srv, relA, doneA)
}

// A stale run-a stop that matches run-b's container while a start with
// no run recorded is in flight gets the 404 before any cancel: the run-less
// start is not cancelled, and nothing is stopped.
func TestStopAgent_StaleRunIDMismatchBeforeCancellingRunlessStart(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	setAgents(mgr, trackedRunEntry("c-b", "run-b"))
	release, done := startInFlight(t, srv, mgr, "", nil)

	sw := actionWithRun(srv, "stop", "runId=run-a", "")
	if sw.Code != http.StatusNotFound {
		t.Fatalf("stale stop: status %d, want 404: %s", sw.Code, sw.Body.String())
	}
	if mgr.StopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0", mgr.StopCalls())
	}
	assertStartNotCancelled(t, srv, release, done)
}

// A legacy stop (no runId) cancels and waits for the in-flight start
// before it looks the agent up, so it sees, and stops, the container the
// start's cleanup left behind (GoogleCloudPlatform/scion#2482's order).
func TestStopAgent_LegacyLooksUpAfterCancelAndWait(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	_, done := startInFlight(t, srv, mgr, "", func() {
		setAgents(mgr, trackedRunEntry("c-1", ""))
	})
	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- actionWithRun(srv, "stop", "", "") }()
	waitStopped(t, done)
	if sw := <-stopDone; sw.Code != http.StatusAccepted {
		t.Fatalf("legacy stop: status %d, want 202: %s", sw.Code, sw.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.lastStopAgentID != "c-1" {
		t.Errorf("stop calls = %d, last %q; want one stop of c-1", mgr.StopCalls(), mgr.lastStopAgentID)
	}
}

// The runId query parameter is recorded when the start is tracked, so a
// stale stop that arrives while the start is still reading its body gets
// the 404 and cancels nothing.
func TestStopAgent_StaleStopDuringStartBodyRead(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			srv, mgr, _, _ := newSyncStartTestServer(t)
			started, release := make(chan struct{}), make(chan struct{})
			blockedStart(mgr, started, release, nil)

			bodyR, bodyW := io.Pipe()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/same-name/"+action+"?runId=run-b", bodyR)
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				done <- w
			}()
			deadline := time.Now().Add(syncStartTestTimeout)
			for !snapshotHas(srv, "same-name") {
				if time.Now().After(deadline) {
					t.Fatal("the start was never tracked")
				}
				time.Sleep(5 * time.Millisecond)
			}

			// The handler is blocked reading its body.
			sw := actionWithRun(srv, "stop", "runId=run-a", "")
			if sw.Code != http.StatusNotFound {
				t.Fatalf("stale stop during the body read: status %d, want 404: %s", sw.Code, sw.Body.String())
			}

			if _, err := bodyW.Write([]byte(`{"runId":"run-b"}`)); err != nil {
				t.Fatal(err)
			}
			_ = bodyW.Close()
			waitSignal(t, started, "the start")
			assertStartNotCancelled(t, srv, release, done)
		})
	}
}

// When the runId query parameter and the body differ, the body's run is
// the one recorded on the tracked start (it is the run StartOptions carries;
// see TestRunID_ThreadedIntoStartOptions); an empty query parameter behaves
// as an absent one.
func TestStartAgent_RunIDQueryAndBody(t *testing.T) {
	for _, tc := range []struct{ name, query, body, want string }{
		{"differ: body wins", "runId=run-q", `{"runId":"run-b"}`, "run-b"},
		{"query only", "runId=run-q", `{}`, "run-q"},
		{"empty query param", "runId=", `{"runId":"run-b"}`, "run-b"},
		{"absent query param", "", `{"runId":"run-b"}`, "run-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, _, _ := newSyncStartTestServer(t)
			started, release := make(chan struct{}), make(chan struct{})
			blockedStart(mgr, started, release, nil)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- actionWithRun(srv, "start", tc.query, tc.body) }()
			waitSignal(t, started, "the start")

			if current, ok := srv.startsInFlight.otherRun(launchKey{Slug: "same-name"}, "run-x"); !ok || current != tc.want {
				t.Errorf("tracked run = (%q, %v), want %q", current, ok, tc.want)
			}
			close(release)
			<-done
		})
	}
}
