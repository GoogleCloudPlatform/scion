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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the broker's async start of an existing agent (design wave 3
// §2 D-f, D-T3; ptone/scion#4669).

const asyncStartHubAgentID = "hub-agent-1"

// asyncStartBody is an eligible async start request body.
func asyncStartBody(launchID string) map[string]any {
	return map[string]any{
		"asyncLaunch": true, "launchId": launchID, "launchTimeoutSeconds": 300,
		"runId": "run-1",
	}
}

// postStart posts a start of slug with the given query (without "?") and
// body, through the server's handler.
func postAsyncStart(t *testing.T, srv *Server, slug, query string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	target := "/api/v1/agents/" + slug + "/start"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func asyncStartQuery() string {
	return api.AgentIDQueryParam + "=" + asyncStartHubAgentID + "&runId=run-1"
}

// setHook installs a package test hook for the test's duration.
func setHook[T any](t *testing.T, hook *T, fn T) {
	t.Helper()
	var zero T
	*hook = fn
	t.Cleanup(func() { *hook = zero })
}

func decodeStartResponse(t *testing.T, w *httptest.ResponseRecorder) CreateAgentResponse {
	t.Helper()
	var resp CreateAgentResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

func hasReport(rtb *mockRuntimeBrokerService, state string) bool {
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == state {
			return true
		}
	}
	return false
}

func registryEmptyFor(srv *Server, key launchKey) bool {
	srv.launchRegistry.mu.Lock()
	defer srv.launchRegistry.mu.Unlock()
	return srv.launchRegistry.records[key] == nil
}

// TestAsyncStart_Accepted202BeforeStart: an eligible async start answers 202
// with the launch echo while Manager.Start is still blocked, reports to the
// hub under the hub's agent ID, journals the launch as kind start, and
// sends succeeded once Start returns.
func TestAsyncStart_Accepted202BeforeStart(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	srv, rtb := newAsyncTestServer(t, mgr)

	w := postAsyncStart(t, srv, "agent-s1", asyncStartQuery(), asyncStartBody("L-s1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeStartResponse(t, w)
	if !resp.LaunchPending || resp.LaunchID != "L-s1" || resp.LaunchInstanceID == "" {
		t.Fatalf("unexpected accepted response: %+v", resp)
	}
	if resp.Agent != nil || resp.Created {
		t.Fatalf("an accepted async start carries no agent and Created=false: %+v", resp)
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() == 1 }) {
		t.Fatal("the launch did not reach Manager.Start")
	}
	if hasReport(rtb, hubclient.AgentLaunchReportStateSucceeded) {
		t.Fatal("succeeded was reported while Start was still blocked")
	}
	for _, r := range rtb.getLaunchReports() {
		if r.AgentID != asyncStartHubAgentID {
			t.Fatalf("launch report sent for agent %q, want the hub agent ID %q", r.AgentID, asyncStartHubAgentID)
		}
	}

	path := filepath.Join(srv.stateDir, journalDirName, srv.launchInstanceID, "L-s1.json")
	jr := readJournalFileT(t, path)
	if jr.Kind != store.LaunchKindStart || jr.AgentID != asyncStartHubAgentID || jr.RunID != "run-1" || jr.Slug != "agent-s1" {
		t.Fatalf("journal record = kind %q agent %q run %q slug %q, want start/%s/run-1/agent-s1",
			jr.Kind, jr.AgentID, jr.RunID, jr.Slug, asyncStartHubAgentID)
	}

	close(mgr.startBlock)
	if !waitUntil(t, 2*time.Second, func() bool { return hasReport(rtb, hubclient.AgentLaunchReportStateSucceeded) }) {
		t.Fatal("expected a succeeded report once Start returned")
	}
	if !waitUntil(t, 2*time.Second, func() bool { return registryEmptyFor(srv, launchKey{Slug: "agent-s1"}) }) {
		t.Fatal("the launch did not leave the registry when it ended")
	}
}

// TestAsyncStart_GateFallsBackSync: a start without every async input is
// synchronous exactly as before and never registers under the name.
func TestAsyncStart_GateFallsBackSync(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		mutate func(map[string]any)
	}{
		{"no agentId", "runId=run-1", func(map[string]any) {}},
		{"no launchId", asyncStartQuery(), func(b map[string]any) { delete(b, "launchId") }},
		{"timeout at the margin", asyncStartQuery(), func(b map[string]any) { b["launchTimeoutSeconds"] = minAsyncLaunchTimeoutSeconds }},
		{"asyncLaunch absent", asyncStartQuery(), func(b map[string]any) { delete(b, "asyncLaunch") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newAsyncManager()
			srv, rtb := newAsyncTestServer(t, mgr)
			var registrations atomic.Int32
			setHook(t, &testHookNameRegistrationBeforeBegin, func(launchKey) { registrations.Add(1) })

			body := asyncStartBody("L-g")
			tc.mutate(body)
			w := postAsyncStart(t, srv, "agent-g", tc.query, body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			resp := decodeStartResponse(t, w)
			if resp.LaunchPending || resp.LaunchID != "" || resp.Agent == nil {
				t.Fatalf("expected the synchronous start answer, got %+v", resp)
			}
			if n := registrations.Load(); n != 0 {
				t.Fatalf("a synchronous start registered under the name %d time(s)", n)
			}
			if mgr.StartCallCount() != 1 || len(rtb.getLaunchReports()) != 0 {
				t.Fatalf("expected one synchronous Start and no launch reports, got %d starts, %d reports",
					mgr.StartCallCount(), len(rtb.getLaunchReports()))
			}
		})
	}
}

// TestAsyncStart_UnsupportedRuntimeSyncUnderRegistration: an eligible
// request whose runtime cannot serve an async launch starts synchronously,
// under the registration, which it releases when it ends.
func TestAsyncStart_UnsupportedRuntimeSyncUnderRegistration(t *testing.T) {
	mgr := &runtimeAsyncManager{asyncManager: newAsyncManager(), rt: newTestSubstrateRuntime()}
	srv, rtb := newAsyncTestServer(t, mgr)
	var registrations atomic.Int32
	setHook(t, &testHookNameRegistrationBeforeBegin, func(launchKey) { registrations.Add(1) })

	w := postAsyncStart(t, srv, "agent-u", asyncStartQuery(), asyncStartBody("L-u"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeStartResponse(t, w)
	if resp.LaunchPending || resp.Agent == nil {
		t.Fatalf("expected the synchronous start answer for a runtime without async support, got %+v", resp)
	}
	if registrations.Load() != 1 {
		t.Fatalf("expected the eligible request to register once, got %d", registrations.Load())
	}
	if mgr.StartCallCount() != 1 || len(rtb.getLaunchReports()) != 0 {
		t.Fatalf("expected one synchronous Start and no launch reports, got %d starts, %d reports",
			mgr.StartCallCount(), len(rtb.getLaunchReports()))
	}
	if !registryEmptyFor(srv, launchKey{Slug: "agent-u"}) {
		t.Fatal("the synchronous fallback left its registration behind")
	}
}

// TestAsyncStart_StopBeforeTransfer is D-T3 (i): a stop's registry cancel
// that reaches the registration before the transfer refuses the start with
// 409 start_cancelled, and Manager.Start is never called.
func TestAsyncStart_StopBeforeTransfer(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-i"}
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey, _ *launchRecord, _ launchCtx) {
		if k == key {
			srv.cancelLocalLaunch(key, asyncStartHubAgentID)
		}
	})

	w := postAsyncStart(t, srv, "agent-i", asyncStartQuery(), asyncStartBody("L-i"))
	// The refusal is written before any launch exists, so nothing can call
	// Start after it: no wait is needed for the negative checks below.
	assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Manager.Start called %d time(s) for a cancelled start", n)
	}
	if len(rtb.getLaunchReports()) != 0 {
		t.Fatal("a cancelled start sent launch reports")
	}
	if !registryEmptyFor(srv, key) {
		t.Fatal("a cancelled start left a registry record")
	}
}

// TestAsyncStart_StopBetweenCancels is D-T3 (ii): a stop parked between its
// registry cancel and its tracker cancel, with the start released to the
// transfer, never leaves a container running.
func TestAsyncStart_StopBetweenCancels(t *testing.T) {
	mgr := newAsyncManager() // Start returns at once: a launch that got through would run to succeeded
	srv, rtb := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-ii"}

	atHandOff := make(chan struct{})
	releaseStart := make(chan struct{})
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey, _ *launchRecord, _ launchCtx) {
		if k == key {
			close(atHandOff)
			<-releaseStart
		}
	})
	parked := make(chan struct{})
	releaseStop := make(chan struct{})
	setHook(t, &testHookStopBetweenCancels, func(k launchKey) {
		if k == key {
			close(parked)
			<-releaseStop
		}
	})

	startDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { startDone <- postAsyncStart(t, srv, "agent-ii", asyncStartQuery(), asyncStartBody("L-ii")) }()
	<-atHandOff
	stopDone := make(chan struct{})
	go func() {
		postStop(t, srv, "agent-ii", api.AgentIDQueryParam+"="+asyncStartHubAgentID)
		close(stopDone)
	}()
	<-parked
	close(releaseStart)
	w := <-startDone
	close(releaseStop)
	<-stopDone

	// The stop's registry cancel ran while the start waited at the
	// transfer, so here the transfer must refuse: design (ii) answers as
	// (i). A 202 is a launch the stop's cancel missed.
	assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	if mgr.StartCallCount() != 0 || len(rtb.getLaunchReports()) != 0 || !registryEmptyFor(srv, key) {
		t.Fatalf("a start the stop reached before its transfer still launched (%d Start calls, %d reports)",
			mgr.StartCallCount(), len(rtb.getLaunchReports()))
	}
}

// TestAsyncStart_StopAfter202 is D-T3 (iii): a stop after the 202 wakes the
// launch, which removes what it created and sends no terminal.
func TestAsyncStart_StopAfter202(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	t.Cleanup(func() { close(mgr.startBlock) })
	srv, rtb := newAsyncTestServer(t, mgr)

	w := postAsyncStart(t, srv, "agent-iii", asyncStartQuery(), asyncStartBody("L-iii"))
	if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
		t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() == 1 }) {
		t.Fatal("the launch did not reach Manager.Start")
	}
	postStop(t, srv, "agent-iii", api.AgentIDQueryParam+"="+asyncStartHubAgentID)
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("the stop did not end the launch with a resource cleanup")
	}
	if hasReport(rtb, hubclient.AgentLaunchReportStateSucceeded) || hasReport(rtb, hubclient.AgentLaunchReportStateFailed) {
		t.Fatal("a locally stopped launch sent a terminal report")
	}
}

// startIVFixture drives D-T3 (iv): a stop whose registry cancel runs before
// the start registers (hook before Begin), with its tracker cancel released
// either before the transfer or after the 202.
type startIVFixture struct {
	srv     *Server
	mgr     *asyncManager
	rtb     *mockRuntimeBrokerService
	key     launchKey
	parked  chan struct{}
	release chan struct{}
	stopped chan struct{}
	// stopResp is the stop's answer, set before stopped is closed.
	stopResp *httptest.ResponseRecorder
}

// legacyStopQuery is a stop without a run ID; runScopedStopQuery is the
// run-scoped stop the hub sends when it knows the run.
var (
	legacyStopQuery    = api.AgentIDQueryParam + "=" + asyncStartHubAgentID
	runScopedStopQuery = "runId=run-1&" + api.AgentIDQueryParam + "=" + asyncStartHubAgentID
)

func newStartIVFixture(t *testing.T, slug, stopQuery string) *startIVFixture {
	f := &startIVFixture{
		mgr:     newAsyncManager(),
		key:     launchKey{Slug: slug},
		parked:  make(chan struct{}),
		release: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	f.mgr.startBlock = make(chan struct{})
	t.Cleanup(func() { close(f.mgr.startBlock) })
	f.srv, f.rtb = newAsyncTestServer(t, f.mgr)
	var stopOnce sync.Once
	setHook(t, &testHookNameRegistrationBeforeBegin, func(k launchKey) {
		if k != f.key {
			return
		}
		stopOnce.Do(func() {
			go func() {
				f.stopResp = postStop(t, f.srv, slug, stopQuery)
				close(f.stopped)
			}()
			<-f.parked // the stop's registry cancel has run: nothing was registered yet
		})
	})
	setHook(t, &testHookStopBetweenCancels, func(k launchKey) {
		if k == f.key {
			close(f.parked)
			<-f.release
		}
	})
	return f
}

// TestAsyncStart_RegistryCancelBeforeBegin_TrackerBeforeHandoff is D-T3
// (iv), tracker cancel before the transfer: the registration follows the
// tracker's context, so the start is refused and never starts. Run for a
// legacy stop and for the run-scoped stop the hub sends.
func TestAsyncStart_RegistryCancelBeforeBegin_TrackerBeforeHandoff(t *testing.T) {
	for name, stopQuery := range map[string]string{"legacy": legacyStopQuery, "run-scoped": runScopedStopQuery} {
		t.Run(name, func(t *testing.T) {
			f := newStartIVFixture(t, "agent-iva", stopQuery)
			setHook(t, &testHookAsyncLaunchBeforeHandOff, func(ctx context.Context, k launchKey, _ *launchRecord, _ launchCtx) {
				if k != f.key {
					return
				}
				close(f.release) // the stop goes on to cancel the tracked start
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			})

			w := postAsyncStart(t, f.srv, "agent-iva", asyncStartQuery(), asyncStartBody("L-iva"))
			<-f.stopped
			// Refused before any launch exists: nothing can call Start
			// after this answer.
			assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
			if n := f.mgr.StartCallCount(); n != 0 {
				t.Fatalf("Manager.Start called %d time(s) after the stop's tracker cancel", n)
			}
			if !registryEmptyFor(f.srv, f.key) {
				t.Fatal("a refused start left a registry record")
			}
			if f.stopResp.Code != http.StatusAccepted {
				t.Fatalf("stop answered %d, want 202 (it cancelled the start): %s", f.stopResp.Code, f.stopResp.Body.String())
			}
		})
	}
}

// TestAsyncStart_RegistryCancelBeforeBegin_TrackerAfterHandoff is D-T3
// (iv), tracker cancel after the 202: the stop's second registry cancel,
// after its tracker wait, ends the launch. Run for a legacy stop and for
// the run-scoped stop the hub sends; the run-scoped stop counts the launch
// it woke as its own, so it answers 202 rather than 404.
func TestAsyncStart_RegistryCancelBeforeBegin_TrackerAfterHandoff(t *testing.T) {
	for name, stopQuery := range map[string]string{"legacy": legacyStopQuery, "run-scoped": runScopedStopQuery} {
		t.Run(name, func(t *testing.T) {
			f := newStartIVFixture(t, "agent-ivb", stopQuery)

			w := postAsyncStart(t, f.srv, "agent-ivb", asyncStartQuery(), asyncStartBody("L-ivb"))
			if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
				t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
			}
			if !waitUntil(t, 2*time.Second, func() bool { return f.mgr.StartCallCount() == 1 }) {
				t.Fatal("the launch did not reach Manager.Start")
			}
			close(f.release)
			<-f.stopped
			if !waitUntil(t, 2*time.Second, func() bool { return f.mgr.CleanupCallCount() >= 1 }) {
				t.Fatal("the stop left the launch running: no resource cleanup")
			}
			if hasReport(f.rtb, hubclient.AgentLaunchReportStateSucceeded) {
				t.Fatal("the stopped launch reported succeeded")
			}
			if f.stopResp.Code != http.StatusAccepted {
				t.Fatalf("stop answered %d, want 202 (it ended the launch): %s", f.stopResp.Code, f.stopResp.Body.String())
			}
		})
	}
}

// TestAsyncStart_ScopedCancelsLeaveRegistration: a stop or delete naming
// another run, or another agent of the same name, leaves the start's
// registration alone (#2632, #2550).
func TestAsyncStart_ScopedCancelsLeaveRegistration(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-sc"}
	var otherRun, otherAgent bool
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey, _ *launchRecord, _ launchCtx) {
		if k == key {
			otherRun = srv.cancelLocalLaunchForRun(key, asyncStartHubAgentID, "run-other")
			otherAgent = srv.cancelLocalLaunchForRun(key, "hub-agent-other", "")
		}
	})

	w := postAsyncStart(t, srv, "agent-sc", asyncStartQuery(), asyncStartBody("L-sc"))
	if otherRun {
		t.Error("a cancel naming another run woke the start's registration")
	}
	if otherAgent {
		t.Error("a cancel naming another agent ID woke the start's registration")
	}
	if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
		t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAsyncStart_SupersedesOldRecord: a registration finding an earlier
// record under the name cancels it and waits for it; past the wait it
// answers a retryable 503 prior_start_in_progress.
func TestAsyncStart_SupersedesOldRecord(t *testing.T) {
	key := launchKey{Slug: "agent-sup"}
	t.Run("old record finishes", func(t *testing.T) {
		mgr := newAsyncManager()
		srv, _ := newAsyncTestServer(t, mgr)
		var old *launchRecord
		old = newLaunchRecord("L-old", asyncStartHubAgentID, store.LaunchKindStart, "", time.Now().Add(time.Minute), func() {
			go srv.launchRegistry.Finish(key, old)
		})
		srv.launchRegistry.Begin(key, old)

		w := postAsyncStart(t, srv, "agent-sup", asyncStartQuery(), asyncStartBody("L-new"))
		if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
			t.Fatalf("expected an accepted async start after the old record ended, got %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("old record outlasts the wait", func(t *testing.T) {
		mgr := newAsyncManager()
		srv, _ := newAsyncTestServer(t, mgr)
		srv.createSupersedeWait = 100 * time.Millisecond
		var cancelled atomic.Bool
		old := newLaunchRecord("L-old", asyncStartHubAgentID, store.LaunchKindStart, "", time.Now().Add(time.Minute), func() { cancelled.Store(true) })
		srv.launchRegistry.Begin(key, old)
		t.Cleanup(func() { close(old.done) })

		w := postAsyncStart(t, srv, "agent-sup", asyncStartQuery(), asyncStartBody("L-new"))
		assertErrorCode(t, w, http.StatusServiceUnavailable, api.BrokerErrCodePriorStartInProgress)
		if w.Header().Get("Retry-After") == "" {
			t.Error("prior_start_in_progress carries no Retry-After")
		}
		if !cancelled.Load() {
			t.Error("the old record was not cancelled")
		}
		if mgr.StartCallCount() != 0 {
			t.Error("a refused start called Manager.Start")
		}
	})
}

// asyncStartProject makes a project with an existing agent directory for
// slug and returns the project dir and the agent dir.
func asyncStartProject(t *testing.T, slug string) (string, string) {
	t.Helper()
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, ".scion", "agents", slug)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte("task"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir, agentDir
}

// TestAsyncStart_FailureKeepsFiles: a failed async start reports failed and
// removes the resources it created, but never the agent's files.
func TestAsyncStart_FailureKeepsFiles(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(errors.New("boom"))
	srv, rtb := newAsyncTestServer(t, mgr)
	projectDir, agentDir := asyncStartProject(t, "agent-f")

	body := asyncStartBody("L-f")
	body["projectPath"] = projectDir
	w := postAsyncStart(t, srv, "agent-f", asyncStartQuery(), body)
	if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
		t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool { return hasReport(rtb, hubclient.AgentLaunchReportStateFailed) }) {
		t.Fatal("expected a failed report")
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected the launch's resources to be cleaned up")
	}
	if !waitUntil(t, 2*time.Second, func() bool { return registryEmptyFor(srv, launchKey{Slug: "agent-f"}) }) {
		t.Fatal("the launch did not end")
	}
	if _, err := os.Stat(filepath.Join(agentDir, "prompt.md")); err != nil {
		t.Fatalf("a failed start launch removed the agent's files: %v", err)
	}
}

// TestAsyncStart_FailedLaunchReportsStartText pins the launch kind's
// wiring through the real call path (runLaunch -> classifyStartError,
// wave 3 E-AC6): a failed asynchronous start reports runtime_error with
// "Failed to start agent", the synchronous start's text, never the
// create text, and never the runtime's own error text. The create side of
// the same path is pinned by TestStartErrorText_* (asyncCreateFailure).
func TestAsyncStart_FailedLaunchReportsStartText(t *testing.T) {
	const detail = "container scion-ctr-7f3a exited in namespace secret-ns"
	mgr := newAsyncManager()
	mgr.setStartErr(errors.New(detail))
	srv, rtb := newAsyncTestServer(t, mgr)
	projectDir, _ := asyncStartProject(t, "agent-st")

	body := asyncStartBody("L-st")
	body["projectPath"] = projectDir
	w := postAsyncStart(t, srv, "agent-st", asyncStartQuery(), body)
	if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
		t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
	}
	var failed *hubclient.AgentLaunchReport
	if !waitUntil(t, 2*time.Second, func() bool {
		for _, r := range rtb.getLaunchReports() {
			if r.Report.State == hubclient.AgentLaunchReportStateFailed {
				failed = r.Report
				return true
			}
		}
		return false
	}) {
		t.Fatalf("no failed terminal report; reports: %+v", rtb.getLaunchReports())
	}
	if failed.ErrorCode != api.LaunchCodeRuntimeError {
		t.Errorf("error code = %q, want %q", failed.ErrorCode, api.LaunchCodeRuntimeError)
	}
	if failed.Message != "Failed to start agent" {
		t.Errorf("a failed start launch reported %q, want %q", failed.Message, "Failed to start agent")
	}
	if strings.Contains(failed.Message, "secret-ns") {
		t.Errorf("the report leaked the runtime error: %q", failed.Message)
	}
}

// TestAsyncStart_RecoveryNeverDeletesFiles: boot recovery of a start
// launch cleans up by handles but never deletes the agent's files, even
// with a marker naming the launch.
func TestAsyncStart_RecoveryNeverDeletesFiles(t *testing.T) {
	mgr := newRecFakeManager()
	srv, rtb := newAsyncTestServer(t, newAsyncManager())
	installRecoveryManager(srv, "mock", mgr)
	rtb.launchReportFunc = appliedReports
	projectDir, agentDir := asyncStartProject(t, "agent-LR")

	jr := launchingRecord("LR")
	jr.Kind = store.LaunchKindStart
	jr.ProjectPath = projectDir
	if err := writeLaunchMarker(projectDir, false, jr.Slug, jr.LaunchID); err != nil {
		t.Fatal(err)
	}
	writeDeadRecord(t, srv, jr)
	runRecovery(t, srv)

	if len(terminalReports(rtb)) != 1 {
		t.Fatal("the recovered start launch was not reported")
	}
	if mgr.CleanupCallCount() == 0 {
		t.Fatal("the recovered start launch's resources were not cleaned up")
	}
	if _, err := os.Stat(filepath.Join(agentDir, "prompt.md")); err != nil {
		t.Fatalf("recovery of a start launch removed the agent's files: %v", err)
	}
}

// panickingStartManager is an async-capable manager whose Start panics.
type panickingStartManager struct {
	*asyncManager
}

func (m *panickingStartManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.startCalls++
	m.mu.Unlock()
	panic("runtime exploded")
}

// TestAsyncStart_PanicInStartFailsLaunch: a runtime panic in an async
// start's Manager.Start ends the launch failed (runtime_error) and cleans
// up its resources, instead of ending the broker process, as the
// synchronous start contains it (reportRuntimePanic). Agent files stay.
//
// The scenario runs in a child process of the test binary: a panic that
// escapes the launch goroutine ends the process, so only a parent can
// report it as this test's failure, with the panic as the pinned cause.
func TestAsyncStart_PanicInStartFailsLaunch(t *testing.T) {
	if os.Getenv(asyncStartPanicChildEnv) == "1" {
		asyncStartPanicScenario(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAsyncStart_PanicInStartFailsLaunch$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), asyncStartPanicChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		cause := "unknown"
		if i := bytes.Index(out, []byte("panic: ")); i >= 0 {
			cause = string(bytes.SplitN(out[i:], []byte("\n"), 2)[0])
		}
		t.Fatalf("the async start scenario process failed (%v); cause: %s\n%s", err, cause, tailBytes(out, 4000))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestAsyncStart_PanicInStartFailsLaunch")) {
		t.Fatalf("the async start scenario process did not pass:\n%s", tailBytes(out, 4000))
	}
}

// asyncStartPanicChildEnv marks the child process that runs
// asyncStartPanicScenario.
const asyncStartPanicChildEnv = "SCION_TEST_ASYNC_START_PANIC_CHILD"

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// asyncStartPanicScenario is TestAsyncStart_PanicInStartFailsLaunch's body,
// run in the child process.
func asyncStartPanicScenario(t *testing.T) {
	mgr := &panickingStartManager{asyncManager: newAsyncManager()}
	srv, rtb := newAsyncTestServer(t, mgr)
	projectDir, agentDir := asyncStartProject(t, "agent-p")

	body := asyncStartBody("L-p")
	body["projectPath"] = projectDir
	w := postAsyncStart(t, srv, "agent-p", asyncStartQuery(), body)
	if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
		t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
	}
	var failed *hubclient.AgentLaunchReport
	if !waitUntil(t, 2*time.Second, func() bool {
		for _, r := range rtb.getLaunchReports() {
			if r.Report.State == hubclient.AgentLaunchReportStateFailed {
				failed = r.Report
				return true
			}
		}
		return false
	}) {
		t.Fatal("a panicking Start did not end the launch failed")
	}
	if failed.ErrorCode != api.LaunchCodeRuntimeError {
		t.Errorf("failed report code = %q, want %q", failed.ErrorCode, api.LaunchCodeRuntimeError)
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("the panicked launch's resources were not cleaned up")
	}
	if !waitUntil(t, 2*time.Second, func() bool { return registryEmptyFor(srv, launchKey{Slug: "agent-p"}) }) {
		t.Fatal("the panicked launch did not end")
	}
	if _, err := os.Stat(filepath.Join(agentDir, "prompt.md")); err != nil {
		t.Fatalf("a panicked start launch removed the agent's files: %v", err)
	}
}

// listHookManager is an asyncManager whose List runs onList and then fails,
// so startAgent's project-path recovery fails after the registration.
type listHookManager struct {
	*asyncManager
	onList func()
}

func (m *listHookManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.onList != nil {
		m.onList()
	}
	return nil, errors.New("list unavailable")
}

// TestAsyncStart_CancelDuringAdmissionAnswers409: a stop that reaches the
// registration while admission runs, after which an admission step fails,
// is answered 409 start_cancelled, as at the transfer (design D-AC12 (i)),
// not with the step's own error.
func TestAsyncStart_CancelDuringAdmissionAnswers409(t *testing.T) {
	mgr := &listHookManager{asyncManager: newAsyncManager()}
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-adm"}
	mgr.onList = func() { srv.cancelLocalLaunch(key, asyncStartHubAgentID) }

	w := postAsyncStart(t, srv, "agent-adm", asyncStartQuery(), asyncStartBody("L-adm"))
	assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	if mgr.StartCallCount() != 0 || !registryEmptyFor(srv, key) {
		t.Fatalf("a start cancelled during admission went on (%d Start calls)", mgr.StartCallCount())
	}

	// Control: the same failure with no cancel keeps its own answer.
	mgr.onList = nil
	w = postAsyncStart(t, srv, "agent-adm", asyncStartQuery(), asyncStartBody("L-adm2"))
	if w.Code == http.StatusConflict {
		t.Fatalf("an admission failure without a cancel answered 409: %s", w.Body.String())
	}
}

// TestAsyncStart_LaunchWiring: the async start's launch takes the request's
// budget from the moment it was received (deadline = receipt +
// launchTimeoutSeconds), the hub's keepalive interval, and the request's
// project ID, as a create's does.
func TestAsyncStart_LaunchWiring(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	t.Cleanup(func() { close(mgr.startBlock) })
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{ProjectID: "proj-w", Slug: "agent-w"}
	var deadline time.Time
	var keepalive time.Duration
	var projectID string
	var seen bool
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey, rec *launchRecord, lc launchCtx) {
		if k == key {
			deadline, keepalive, projectID, seen = rec.Deadline, lc.keepalive, lc.projectID, true
		}
	})

	body := asyncStartBody("L-w")
	body["launchKeepaliveSeconds"] = 7
	before := time.Now()
	w := postAsyncStart(t, srv, "agent-w", "projectId=proj-w&"+asyncStartQuery(), body)
	after := time.Now()
	if w.Code != http.StatusAccepted || !decodeStartResponse(t, w).LaunchPending {
		t.Fatalf("expected an accepted async start, got %d: %s", w.Code, w.Body.String())
	}
	if !seen {
		t.Fatal("the transfer hook did not run")
	}
	if lo, hi := before.Add(300*time.Second), after.Add(300*time.Second); deadline.Before(lo) || deadline.After(hi) {
		t.Errorf("launch deadline = %v, want receipt + 300s, within [%v, %v]", deadline, lo, hi)
	}
	if keepalive != 7*time.Second {
		t.Errorf("launch keepalive = %v, want the request's 7s", keepalive)
	}
	if projectID != "proj-w" {
		t.Errorf("launch project ID = %q, want the request's proj-w", projectID)
	}
}

// listCancelRuntimeManager is a manager on a runtime without async launch
// support whose List runs onList (and then lists nothing), so a test can
// cancel the start's registration during admission without failing it.
type listCancelRuntimeManager struct {
	*runtimeAsyncManager
	onList func()
}

func (m *listCancelRuntimeManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.onList != nil {
		m.onList()
	}
	return nil, nil
}

// TestAsyncStart_SyncFallbackCancelledAnswers409: on a runtime without async
// launch support, an eligible start runs synchronously under its
// registration; a stop that cancels the registration before Start is
// answered 409 start_cancelled, and Manager.Start is never called. Control:
// the same start without the cancel starts synchronously.
func TestAsyncStart_SyncFallbackCancelledAnswers409(t *testing.T) {
	mgr := &listCancelRuntimeManager{runtimeAsyncManager: &runtimeAsyncManager{asyncManager: newAsyncManager(), rt: newTestSubstrateRuntime()}}
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-sf"}
	mgr.onList = func() { srv.cancelLocalLaunch(key, asyncStartHubAgentID) }

	w := postAsyncStart(t, srv, "agent-sf", asyncStartQuery(), asyncStartBody("L-sf"))
	assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("a cancelled synchronous fallback called Manager.Start %d time(s)", n)
	}
	if !registryEmptyFor(srv, key) {
		t.Fatal("the cancelled fallback left its registration behind")
	}

	mgr.onList = nil
	w = postAsyncStart(t, srv, "agent-sf", asyncStartQuery(), asyncStartBody("L-sf2"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("control: status = %d, body = %s", w.Code, w.Body.String())
	}
	if resp := decodeStartResponse(t, w); resp.LaunchPending || resp.Agent == nil {
		t.Fatalf("control: expected the synchronous start answer, got %+v", resp)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("control: expected one synchronous Start, got %d", n)
	}
}
