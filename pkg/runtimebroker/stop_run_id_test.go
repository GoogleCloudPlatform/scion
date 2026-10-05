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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// Tests for the runId stop filter (ptone/scion#2550 P3): a stop that names a
// run acts only on that run's runtime entry. A stop for a run that no longer
// holds the name answers 404 and has no side effects at all: no launch
// cancel, no runtime stop, no forced heartbeat.

// stopRunFixture is a broker holding run "run-new" of agent "dev" in project
// B, with an in-flight launch record and a heartbeat service attached so
// every side effect of a stop is observable.
type stopRunFixture struct {
	srv       *Server
	mgr       *filteringMockManager
	hubSvc    *mockRuntimeBrokerService
	cancels   atomic.Int32
	launchKey launchKey
}

func newStopRunFixture(t *testing.T, launchRunID string) *stopRunFixture {
	t.Helper()
	f := &stopRunFixture{mgr: &filteringMockManager{}}
	srv, home := newScopeTestServer(t, f.mgr)
	f.srv = srv
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	f.mgr.agents = []api.AgentInfo{withRun(labelled("dev", "cid-new", scopeProjB, scionB), "run-new")}

	f.hubSvc = &mockRuntimeBrokerService{}
	hb := NewHeartbeatService(f.hubSvc, "test-host", time.Hour, f.mgr, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.hubMu.Lock()
	srv.hubConnections["local"] = &HubConnection{Name: "local", Heartbeat: hb}
	srv.hubMu.Unlock()

	if launchRunID != "" {
		f.launchKey = launchKey{ProjectID: scopeProjB, Slug: "dev"}
		rec := newLaunchRecord("sync-1", "dev", "create", "", time.Time{}, func() { f.cancels.Add(1) })
		rec.RunID = launchRunID
		srv.launchRegistry.Begin(f.launchKey, rec)
	}
	return f
}

func (f *stopRunFixture) stop(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/stop?"+query, nil)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *stopRunFixture) stopCalls() int {
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	return f.mgr.stopCalls
}

// waitHeartbeats waits up to d for at least want forced heartbeats and
// returns the count seen. forceHeartbeatAll sends them asynchronously.
func (f *stopRunFixture) waitHeartbeats(want int, d time.Duration) int {
	deadline := time.Now().Add(d)
	for {
		n := len(f.hubSvc.getHeartbeatCalls())
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNoStopSideEffects checks that a refused stop touched nothing.
func (f *stopRunFixture) assertNoStopSideEffects(t *testing.T) {
	t.Helper()
	if n := f.stopCalls(); n != 0 {
		t.Errorf("stale run stop reached the runtime (%d stop calls, last %q)", n, f.mgr.lastStopAgentID)
	}
	if n := f.cancels.Load(); n != 0 {
		t.Errorf("stale run stop cancelled the newer run's launch (%d cancels)", n)
	}
	// Give an (incorrect) asynchronous forced heartbeat time to land.
	if n := f.waitHeartbeats(1, 200*time.Millisecond); n != 0 {
		t.Errorf("stale run stop forced %d heartbeat(s)", n)
	}
}

// Acceptance (a) for stop: a stale runId gets 404, and the newer run's
// container keeps running, with zero side effects.
func TestStopAgent_StaleRunID_404NoSideEffects(t *testing.T) {
	f := newStopRunFixture(t, "run-new")
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-old")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	f.assertNoStopSideEffects(t)
}

// The current runId stops that run's entry, passing its run to the
// runtime, and has the usual side effects.
func TestStopAgent_CurrentRunID_Stops(t *testing.T) {
	f := newStopRunFixture(t, "run-new")
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-new")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" || f.mgr.lastStopRunID != "run-new" {
		t.Errorf("stop calls = %d, last = %q run %q; want one stop of cid-new run-new",
			f.stopCalls(), f.mgr.lastStopAgentID, f.mgr.lastStopRunID)
	}
	if f.cancels.Load() == 0 {
		t.Error("the stop for the launch's own run did not cancel it")
	}
	if n := f.waitHeartbeats(1, 2*time.Second); n == 0 {
		t.Error("a successful stop did not force a heartbeat")
	}
}

// Without a runId the stop resolves by name, as before: it stops whatever
// run holds the name and cancels any launch.
func TestStopAgent_NoRunID_StopsByNameAsBefore(t *testing.T) {
	f := newStopRunFixture(t, "run-new")
	rec := f.stop(t, "projectId="+scopeProjB)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" {
		t.Errorf("stop calls = %d, last = %q; want one stop of cid-new", f.stopCalls(), f.mgr.lastStopAgentID)
	}
	if f.cancels.Load() == 0 {
		t.Error("a stop without runId did not cancel the in-flight launch")
	}
}

// A legacy entry with no run label still matches a run-scoped stop by
// name, as a run-scoped delete does.
func TestStopAgent_RunIDWithLegacyUnlabelledContainer_Stops(t *testing.T) {
	f := newStopRunFixture(t, "")
	f.mgr.agents[0].RunID = ""
	delete(f.mgr.agents[0].Labels, api.LabelRunID)
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-old")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" {
		t.Errorf("stop calls = %d, last = %q; want one stop of cid-new", f.stopCalls(), f.mgr.lastStopAgentID)
	}
}

// No container holds the name yet, but a launch of a different run is in
// flight: the name is that run's, so the stale stop gets 404, not the
// "not found in project" 202, and the launch is not cancelled.
func TestStopAgent_StaleRunIDAgainstInFlightLaunch_404(t *testing.T) {
	f := newStopRunFixture(t, "run-new")
	f.mgr.agents = nil
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-old")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	f.assertNoStopSideEffects(t)

	// The stop for the launching run itself cancels it and is accepted.
	rec = f.stop(t, "projectId="+scopeProjB+"&runId=run-new")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("own-run stop: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.cancels.Load() == 0 {
		t.Error("the stop for the launch's own run did not cancel it")
	}
}

// Nothing holds the name and no launch is in flight: the run-scoped stop
// is the idempotent "not found in project" 202, as before.
func TestStopAgent_RunIDNothingThere_IdempotentAccepted(t *testing.T) {
	f := newStopRunFixture(t, "")
	f.mgr.agents = nil
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-old")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.stopCalls() != 0 {
		t.Errorf("stop calls = %d, want 0", f.stopCalls())
	}
}

// The same refusal over the control channel: the tunnelled request reaches
// the same handler, and the hub sees a 404 with nothing stopped.
func TestStopAgent_StaleRunID_ControlChannel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runID     string
		wantCode  int
		wantStops int
	}{
		{"stale", "run-old", http.StatusNotFound, 0},
		{"current", "run-new", http.StatusAccepted, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopRunFixture(t, "run-new")
			brokerConn, hubConn, cleanup := newWSPair(t)
			t.Cleanup(cleanup)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			client := &ControlChannelClient{
				config:      ControlChannelConfig{},
				conn:        brokerConn,
				handlers:    f.srv.Handler(),
				log:         slog.Default(),
				streams:     make(map[string]*StreamHandler),
				dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
				cancels:     make(map[string]context.CancelFunc),
				ctx:         ctx,
				cancel:      cancel,
			}
			client.wg.Add(1)
			go client.dispatchRequest(brokerConn, wsprotocol.RequestEnvelope{
				Type: "request", RequestID: "stop-" + tc.name, Method: http.MethodPost,
				Path:  "/api/v1/agents/dev/stop",
				Query: "projectId=" + scopeProjB + "&runId=" + tc.runID,
			})
			client.wg.Wait()
			var resp wsprotocol.ResponseEnvelope
			if err := hubConn.ReadJSON(&resp); err != nil {
				t.Fatalf("reading response envelope: %v", err)
			}
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantCode, resp.Body)
			}
			if f.stopCalls() != tc.wantStops {
				t.Errorf("stop calls = %d, want %d", f.stopCalls(), tc.wantStops)
			}
			if tc.wantStops == 0 {
				f.assertNoStopSideEffects(t)
			}
		})
	}
}

// Ordering against P1's recorded-runtime gate: when no runtime lists the
// agent and the recorded runtime type is not registered, the 503
// runtime_unavailable answer comes first, whatever the runId, and nothing
// acts on the agent. When the recorded type does hold another run's entry,
// the run filter answers 404 there, again acting on nothing.
func TestStopAgent_RunIDOrderingAgainstRuntimeUnavailable(t *testing.T) {
	t.Run("unregistered recorded type is 503 before the run check", func(t *testing.T) {
		srv, defaultMgr, _ := newRecordedRuntimeServer(t, false)
		defaultMgr.agents = nil
		w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("kubernetes")+"&runId=run-old", "")
		assertErrorCode(t, w, http.StatusServiceUnavailable, ErrCodeRuntimeUnavailable)
		if a := defaultMgr.acted(); a != 0 {
			t.Errorf("default runtime acted on the agent (%d)", a)
		}
	})
	t.Run("recorded type holding another run is 404", func(t *testing.T) {
		srv, dockerMgr, k8sMgr := newRecordedRuntimeServer(t, true)
		dockerMgr.agents = []api.AgentInfo{withRun(rrAgentInfo("docker-container"), "run-a")}
		k8sMgr.agents = []api.AgentInfo{withRun(rrAgentInfo("k8s-pod"), "run-b")}
		w := serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("kubernetes")+"&runId=run-a", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
		}
		if dockerMgr.acted() != 0 || k8sMgr.acted() != 0 {
			t.Errorf("acted: docker=%d kubernetes=%d, want none", dockerMgr.acted(), k8sMgr.acted())
		}
		// The recorded type's own run is stopped there only.
		w = serveRR(srv, http.MethodPost, "/api/v1/agents/"+rrAgent+"/stop"+rrQuery("kubernetes")+"&runId=run-b", "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
		}
		if k8sMgr.acted() != 1 || dockerMgr.acted() != 0 {
			t.Errorf("acted: docker=%d kubernetes=%d, want kubernetes only", dockerMgr.acted(), k8sMgr.acted())
		}
	})
}

func TestLaunchRegistry_InFlightOtherRun(t *testing.T) {
	r := newLaunchRegistry()
	key := launchKey{ProjectID: "p", Slug: "dev"}
	if r.inFlightOtherRun(key, "run-a") {
		t.Fatal("empty registry reported a launch")
	}
	rec := newLaunchRecord("1", "dev", "create", "", time.Time{}, func() {})
	rec.RunID = "run-b"
	r.Begin(key, rec)
	if !r.inFlightOtherRun(key, "run-a") {
		t.Error("launch of run-b not reported for run-a")
	}
	if r.inFlightOtherRun(key, "run-b") {
		t.Error("launch of the requested run reported as another run")
	}
	if r.inFlightOtherRun(key, "") {
		t.Error("an empty run ID must never count")
	}
	rec.RunID = ""
	if r.inFlightOtherRun(key, "run-a") {
		t.Error("a launch without a run ID must never count")
	}
	var nilReg *launchRegistry
	if nilReg.inFlightOtherRun(key, "run-a") {
		t.Error("nil registry reported a launch")
	}
}
