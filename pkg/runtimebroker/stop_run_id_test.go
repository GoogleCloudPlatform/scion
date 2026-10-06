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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
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
	// The requested run goes to the runtime (as deleteRunRef does), so a
	// run-checking runtime still refuses a pod another run recreated.
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" || f.mgr.lastStopRunID != "run-old" {
		t.Errorf("stop calls = %d, last = %q run %q; want one stop of cid-new with run-old",
			f.stopCalls(), f.mgr.lastStopAgentID, f.mgr.lastStopRunID)
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
				cancels:     make(map[string]*requestCancel),
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
	if _, ok := r.otherRunInFlightID(key, "run-a"); ok {
		t.Fatal("empty registry reported a launch")
	}
	rec := newLaunchRecord("1", "dev", "create", "", time.Time{}, func() {})
	rec.RunID = "run-b"
	r.Begin(key, rec)
	if current, ok := r.otherRunInFlightID(key, "run-a"); !ok || current != "run-b" {
		t.Errorf("launch of run-b reported as (%q, %v) for run-a, want (run-b, true)", current, ok)
	}
	if _, ok := r.otherRunInFlightID(key, "run-b"); ok {
		t.Error("launch of the requested run reported as another run")
	}
	if _, ok := r.otherRunInFlightID(key, ""); ok {
		t.Error("an empty run ID must never count")
	}
	rec.RunID = ""
	if _, ok := r.otherRunInFlightID(key, "run-a"); ok {
		t.Error("a launch without a run ID must never count")
	}
	var nilReg *launchRegistry
	if _, ok := nilReg.otherRunInFlightID(key, "run-a"); ok {
		t.Error("nil registry reported a launch")
	}
}

// The mismatch 404 carries api.BrokerErrorCodeRunMismatch and names both
// the requested run and the run holding the name, whether that is a runtime
// entry or an in-flight launch.
func TestStopAgent_RunMismatch404Details(t *testing.T) {
	check := func(t *testing.T, rec *httptest.ResponseRecorder, wantCurrent string) {
		t.Helper()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		var body ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Error.Code != api.BrokerErrorCodeRunMismatch {
			t.Errorf("code = %q, want %q", body.Error.Code, api.BrokerErrorCodeRunMismatch)
		}
		if got := body.Error.Details[api.BrokerErrorDetailRunID]; got != "run-old" {
			t.Errorf("details runId = %v, want run-old", got)
		}
		if got := body.Error.Details[api.BrokerErrorDetailCurrentRunID]; got != wantCurrent {
			t.Errorf("details currentRunId = %v, want %s", got, wantCurrent)
		}
	}
	t.Run("runtime entry", func(t *testing.T) {
		f := newStopRunFixture(t, "")
		check(t, f.stop(t, "projectId="+scopeProjB+"&runId=run-old"), "run-new")
	})
	t.Run("in-flight launch", func(t *testing.T) {
		f := newStopRunFixture(t, "run-launching")
		f.mgr.agents = nil
		check(t, f.stop(t, "projectId="+scopeProjB+"&runId=run-old"), "run-launching")
	})
}

// The runtime enforces the stop ref's run (Kubernetes Stop is a run-checked
// Delete, GoogleCloudPlatform/scion#2515), so the entry the broker resolved
// can turn out to be replaced by another run when the runtime acts, and
// StopTarget returns a wrapped ErrRunMismatch, leaving the entry that now
// holds the name running. A run-scoped stop that cancelled nothing of its
// own answers the run-mismatch 404 with no side effects: another run's
// launch is not cancelled, and no heartbeat or launch report is sent. It
// is never a 500. (The same with another run's tracked start in flight:
// TestStopAgent_RuntimeRunMismatch_OtherRunsStartAndLaunchUntouched.)
func TestStopAgent_RuntimeRunMismatch_RunScoped404(t *testing.T) {
	f := newStopRunFixture(t, "run-other")
	f.mgr.stopErr = fmt.Errorf("pod ns/dev belongs to run %q, not %q: %w", "run-newer", "run-new", scionrt.ErrRunMismatch)
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-new")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != api.BrokerErrorCodeRunMismatch {
		t.Errorf("code = %q, want %q", body.Error.Code, api.BrokerErrorCodeRunMismatch)
	}
	if got := body.Error.Details[api.BrokerErrorDetailRunID]; got != "run-new" {
		t.Errorf("details runId = %v, want run-new", got)
	}
	if got, ok := body.Error.Details[api.BrokerErrorDetailCurrentRunID]; ok {
		t.Errorf("details currentRunId = %v, want it omitted (unknown)", got)
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" || f.mgr.lastStopRunID != "run-new" {
		t.Errorf("stop calls = %d, last = %q run %q; want one stop of cid-new run-new",
			f.stopCalls(), f.mgr.lastStopAgentID, f.mgr.lastStopRunID)
	}
	if n := f.cancels.Load(); n != 0 {
		t.Errorf("run-mismatched stop cancelled another run's launch (%d cancels)", n)
	}
	if !f.srv.launchRegistry.runInFlight(f.launchKey, "run-other") {
		t.Error("another run's launch is no longer registered")
	}
	if n := f.waitHeartbeats(1, 200*time.Millisecond); n != 0 {
		t.Errorf("run-mismatched stop forced %d heartbeat(s)", n)
	}
	if n := len(f.hubSvc.getLaunchReports()); n != 0 {
		t.Errorf("run-mismatched stop sent %d launch report(s)", n)
	}
}

// The same runtime run mismatch on a legacy stop (no runId; the resolved
// entry's run is still on the ref): the entry it resolved is gone, so the
// stop is the "not found" 202, never a 500.
func TestStopAgent_RuntimeRunMismatch_Legacy202(t *testing.T) {
	f := newStopRunFixture(t, "")
	f.mgr.stopErr = fmt.Errorf("pod ns/dev was replaced before it could be deleted: %w", scionrt.ErrRunMismatch)
	rec := f.stop(t, "projectId="+scopeProjB)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "accepted" {
		t.Errorf("body = %v, want status accepted", body)
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" || f.mgr.lastStopRunID != "run-new" {
		t.Errorf("stop calls = %d, last = %q run %q; want one stop of cid-new run-new",
			f.stopCalls(), f.mgr.lastStopAgentID, f.mgr.lastStopRunID)
	}
	if n := f.waitHeartbeats(1, time.Second); n == 0 {
		t.Error("the legacy 202 did not force a heartbeat")
	}
}

// The runtime run mismatch after a run-scoped stop already cancelled its
// own run's launch: the stop did act, so it is
// accepted (202, with the forced heartbeat), not the zero-side-effect 404,
// as in the restart overlap.
func TestStopAgent_RuntimeRunMismatch_AfterOwnCancel202(t *testing.T) {
	f := newStopRunFixture(t, "run-new")
	f.mgr.stopErr = fmt.Errorf("pod ns/dev was replaced before it could be deleted: %w", scionrt.ErrRunMismatch)
	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-new")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := f.cancels.Load(); n != 1 {
		t.Errorf("cancels = %d, want the own run's launch cancelled once", n)
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "cid-new" || f.mgr.lastStopRunID != "run-new" {
		t.Errorf("stop calls = %d, last = %q run %q; want one stop of cid-new run-new",
			f.stopCalls(), f.mgr.lastStopAgentID, f.mgr.lastStopRunID)
	}
	if n := f.waitHeartbeats(1, time.Second); n == 0 {
		t.Error("the accepted stop did not force a heartbeat")
	}
}

// A project-blind run-scoped stop with nothing of the requested run never
// passes the bare slug to the runtime: it takes the not-found path. Without
// a runId the legacy bare-slug pass-through is unchanged.
func TestStopAgent_RunIDWithoutProjectNeverStopsBareSlug(t *testing.T) {
	f := newStopRunFixture(t, "")
	f.mgr.agents = nil
	rec := f.stop(t, "runId=run-old")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := f.stopCalls(); n != 0 {
		t.Fatalf("run-scoped stop reached the runtime with %q (%d calls)", f.mgr.lastStopAgentID, n)
	}

	rec = f.stop(t, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("legacy stop: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.stopCalls() != 1 || f.mgr.lastStopAgentID != "dev" {
		t.Errorf("legacy stop: calls = %d, last = %q; want the bare slug passed through as before", f.stopCalls(), f.mgr.lastStopAgentID)
	}
}

// Restart's stop leg stops the entry it resolved, with that entry's run on
// the ref, not the new run the restart starts and not an empty run.
func TestRestartAgent_StopLegCarriesEntryRun(t *testing.T) {
	f := newStopRunFixture(t, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/dev/restart?projectId="+scopeProjB,
		strings.NewReader(`{"runId":"run-next"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("restart: status %d: %s", rec.Code, rec.Body.String())
	}
	if f.stopCalls() != 1 {
		t.Fatalf("stop calls = %d, want 1", f.stopCalls())
	}
	if f.mgr.lastStopAgentID != "cid-new" || f.mgr.lastStopRunID != "run-new" {
		t.Errorf("restart stop ref = {%q, %q}, want {cid-new, run-new}", f.mgr.lastStopAgentID, f.mgr.lastStopRunID)
	}
	if got := f.mgr.LastStartOpts().RunID; got != "run-next" {
		t.Errorf("restart start run = %q, want run-next", got)
	}
}

// resolvedStopRef: the matched entry's container is addressed by its
// operation ID, qualified with a Kubernetes entry's namespace, and carries
// the entry's run, or the requested run for a legacy entry with no run
// label (as deleteRunRef does); any other target is passed bare with no
// run, so another entry's namespace or run is never applied.
func TestResolvedStopRef(t *testing.T) {
	k8sEntry := func(id, ns, run string) api.AgentInfo {
		return api.AgentInfo{ContainerID: id, RunID: run, Runtime: "kubernetes",
			Kubernetes: &api.AgentK8sMetadata{Namespace: ns, PodName: id}}
	}
	cases := []struct {
		name    string
		target  string
		match   agentMatch
		request string
		want    scionrt.RunRef
	}{
		{"kubernetes entry in ns-a is namespace-qualified with its run", "dev",
			agentMatch{containerID: "dev", entry: k8sEntry("dev", "ns-a", "run-1"), matched: true}, "",
			scionrt.RunRef{ID: "ns-a/dev", RunID: "run-1"}},
		{"labelled entry keeps its own run over the requested one", "dev",
			agentMatch{containerID: "dev", entry: k8sEntry("dev", "ns-a", "run-1"), matched: true}, "run-req",
			scionrt.RunRef{ID: "ns-a/dev", RunID: "run-1"}},
		{"legacy entry, legacy stop: qualified, no run", "dev",
			agentMatch{containerID: "dev", entry: k8sEntry("dev", "ns-a", ""), matched: true}, "",
			scionrt.RunRef{ID: "ns-a/dev"}},
		{"legacy entry, run-scoped stop: qualified, requested run", "dev",
			agentMatch{containerID: "dev", entry: k8sEntry("dev", "ns-a", ""), matched: true}, "run-req",
			scionrt.RunRef{ID: "ns-a/dev", RunID: "run-req"}},
		{"kubernetes entry with no namespace stays bare", "dev",
			agentMatch{containerID: "dev", entry: k8sEntry("dev", "", "run-1"), matched: true}, "",
			scionrt.RunRef{ID: "dev", RunID: "run-1"}},
		{"docker entry ID is unchanged, with its run", "3f2a9c1d0b7e",
			agentMatch{containerID: "3f2a9c1d0b7e", entry: api.AgentInfo{ContainerID: "3f2a9c1d0b7e", RunID: "run-1", Runtime: "docker"}, matched: true}, "",
			scionrt.RunRef{ID: "3f2a9c1d0b7e", RunID: "run-1"}},
		{"label-overridden container ID is qualified as the target", "dev",
			agentMatch{containerID: "dev", entry: k8sEntry("pod-xyz", "ns-a", "run-1"), matched: true}, "",
			scionrt.RunRef{ID: "ns-a/dev", RunID: "run-1"}},
		{"target other than the matched container is bare, no run", "dev",
			agentMatch{containerID: "other", entry: k8sEntry("other", "ns-a", "run-1"), matched: true}, "run-req",
			scionrt.RunRef{ID: "dev"}},
		{"no match: bare target (legacy pass-through)", "dev",
			agentMatch{}, "",
			scionrt.RunRef{ID: "dev"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedStopRef(tc.target, tc.match, tc.request); got != tc.want {
				t.Errorf("resolvedStopRef(%q, request %q) = %+v, want %+v", tc.target, tc.request, got, tc.want)
			}
		})
	}
}

// dedupeAgentEntries keys on the operation ID: same-named pods in two
// namespaces stay distinct; the same pod, or the same Docker/Podman/Apple
// container ID (no Kubernetes metadata: keyed on the container ID as
// before), listed twice collapses to one.
func TestDedupeAgentEntries_OperationID(t *testing.T) {
	pod := func(ns string) api.AgentInfo {
		return api.AgentInfo{Name: "dev", ContainerID: "dev", Kubernetes: &api.AgentK8sMetadata{Namespace: ns, PodName: "dev"}}
	}
	docker := func(id string) api.AgentInfo { return api.AgentInfo{Name: "dev", ContainerID: id, Runtime: "docker"} }
	cases := []struct {
		name string
		in   []api.AgentInfo
		want int
	}{
		{"pods in two namespaces stay distinct", []api.AgentInfo{pod("ns-a"), pod("ns-b")}, 2},
		{"the same pod listed twice collapses", []api.AgentInfo{pod("ns-a"), pod("ns-a")}, 1},
		{"the same docker container listed twice collapses", []api.AgentInfo{docker("3f2a"), docker("3f2a")}, 1},
		{"two docker containers stay distinct", []api.AgentInfo{docker("3f2a"), docker("9b1c")}, 2},
		{"entries with only an ID collapse by ID", []api.AgentInfo{{ID: "x"}, {ID: "x"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(dedupeAgentEntries(tc.in)); got != tc.want {
				t.Errorf("len(dedupeAgentEntries) = %d, want %d", got, tc.want)
			}
		})
	}
}

// preferRunEntries: the requested run's entries win, then unlabelled legacy
// entries; with neither, every entry is kept so another run holding the
// name is still seen. No run keeps the list unchanged.
func TestPreferRunEntries(t *testing.T) {
	e := func(id, run string) api.AgentInfo { return api.AgentInfo{ContainerID: id, RunID: run} }
	all := []api.AgentInfo{e("a", "run-1"), e("b", "run-2"), e("c", "")}
	ids := func(in []api.AgentInfo) string {
		var out []string
		for _, a := range in {
			out = append(out, a.ContainerID)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		name string
		in   []api.AgentInfo
		run  string
		want string
	}{
		{"exact run wins", all, "run-2", "b"},
		{"legacy entry when no exact", all, "run-3", "c"},
		{"other runs only: all kept", all[:2], "run-3", "a,b"},
		{"no run: unchanged", all, "", "a,b,c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(preferRunEntries(tc.in, tc.run)); got != tc.want {
				t.Errorf("preferRunEntries(%q) = %s, want %s", tc.run, got, tc.want)
			}
		})
	}
}

// runAgentMatchFrom: with a run, only distinct entries of other runs give
// otherRunsHoldNameError (the run-mismatch answer), naming the run when
// they share one; an own-run or legacy entry is matched; two entries of the
// requested run, or two legacy entries, stay an ambiguity, as does any
// ambiguity without a run.
func TestRunAgentMatchFrom(t *testing.T) {
	pod := func(ns, run string) api.AgentInfo {
		return api.AgentInfo{Name: "dev", ContainerID: "dev", RunID: run,
			Kubernetes: &api.AgentK8sMetadata{Namespace: ns, PodName: "dev"}}
	}
	for _, tc := range []struct {
		name          string
		agents        []api.AgentInfo
		run           string
		wantOther     bool
		wantCurrent   string
		wantMatch     string
		wantAmbiguous bool
	}{
		{"two other runs", []api.AgentInfo{pod("a", "run-1"), pod("b", "run-2")}, "run-3", true, "", "", false},
		{"one other run in two namespaces", []api.AgentInfo{pod("a", "run-1"), pod("b", "run-1")}, "run-3", true, "run-1", "", false},
		{"own run beside another", []api.AgentInfo{pod("a", "run-1"), pod("b", "run-2")}, "run-2", false, "", "run-2", false},
		{"legacy beside another run", []api.AgentInfo{pod("a", "run-1"), pod("b", "")}, "run-3", false, "", "", false},
		{"single other-run entry is a match (entry check refuses it)", []api.AgentInfo{pod("a", "run-1")}, "run-3", false, "", "run-1", false},
		{"two entries of the requested run stay ambiguous", []api.AgentInfo{pod("a", "run-3"), pod("b", "run-3")}, "run-3", false, "", "", true},
		{"two legacy entries stay ambiguous", []api.AgentInfo{pod("a", ""), pod("b", "")}, "run-3", false, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := runAgentMatchFrom("dev", tc.run, tc.agents, nil, nil)
			var other *otherRunsHoldNameError
			if got := errors.As(err, &other); got != tc.wantOther {
				t.Fatalf("otherRunsHoldNameError = %v (err %v), want %v", got, err, tc.wantOther)
			}
			if tc.wantOther {
				if other.currentRunID != tc.wantCurrent {
					t.Errorf("currentRunID = %q, want %q", other.currentRunID, tc.wantCurrent)
				}
				return
			}
			if tc.wantAmbiguous {
				if err == nil || !strings.Contains(err.Error(), "ambiguous") {
					t.Fatalf("err = %v, want the ambiguity error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if m.entry.RunID != tc.wantMatch {
				t.Errorf("matched run = %q, want %q", m.entry.RunID, tc.wantMatch)
			}
		})
	}
	_, err := runAgentMatchFrom("dev", "", []api.AgentInfo{pod("a", "run-1"), pod("b", "run-2")}, nil, nil)
	var other *otherRunsHoldNameError
	if err == nil || errors.As(err, &other) || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("no run: err = %v, want the ambiguity error", err)
	}
}

// A run-scoped stop that cancels its own run's launch and then finds only
// entries of two other runs holding the name (the second, entry-only check
// sees otherRunsHoldNameError): the stop did act, so it is accepted (202),
// as for own-cancel then a single other run's entry; nothing is stopped.
func TestStopAgent_OwnCancelThenOtherRunsOnly202(t *testing.T) {
	f := newStopRunFixture(t, "run-3")
	a := f.mgr.agents[0]
	a.RunID = "run-1"
	a.Labels = map[string]string{}
	for k, v := range f.mgr.agents[0].Labels {
		a.Labels[k] = v
	}
	a.Labels[api.LabelRunID] = "run-1"
	b := a
	b.RunID = "run-2"
	b.ContainerID = "cid-other"
	b.ID = "cid-other"
	b.Labels = map[string]string{}
	for k, v := range a.Labels {
		b.Labels[k] = v
	}
	b.Labels[api.LabelRunID] = "run-2"
	b.Labels["scion.container.id"] = "cid-other"
	f.mgr.agents = []api.AgentInfo{a, b}

	rec := f.stop(t, "projectId="+scopeProjB+"&runId=run-3")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if f.cancels.Load() != 1 || f.stopCalls() != 0 {
		t.Errorf("cancels = %d, stop calls = %d; want 1 and 0", f.cancels.Load(), f.stopCalls())
	}
}
