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
	"path/filepath"
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

// postStop posts a stop of slug with the given query (without "?").
func postStop(srv *Server, slug, query string) *httptest.ResponseRecorder {
	target := "/api/v1/agents/" + slug + "/stop"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, nil)
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
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey) {
		if k == key {
			srv.cancelLocalLaunch(key, asyncStartHubAgentID)
		}
	})

	w := postAsyncStart(t, srv, "agent-i", asyncStartQuery(), asyncStartBody("L-i"))
	assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	time.Sleep(50 * time.Millisecond)
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
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey) {
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
		postStop(srv, "agent-ii", api.AgentIDQueryParam+"="+asyncStartHubAgentID)
		close(stopDone)
	}()
	<-parked
	close(releaseStart)
	w := <-startDone
	close(releaseStop)
	<-stopDone

	if w.Code == http.StatusConflict {
		assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	} else if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	time.Sleep(100 * time.Millisecond)
	if hasReport(rtb, hubclient.AgentLaunchReportStateSucceeded) || mgr.StartCallCount() != 0 {
		t.Fatalf("a start the stop reached before its transfer still started (%d Start calls, succeeded reported: %v)",
			mgr.StartCallCount(), hasReport(rtb, hubclient.AgentLaunchReportStateSucceeded))
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
	postStop(srv, "agent-iii", api.AgentIDQueryParam+"="+asyncStartHubAgentID)
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
}

func newStartIVFixture(t *testing.T, slug string) *startIVFixture {
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
				postStop(f.srv, slug, api.AgentIDQueryParam+"="+asyncStartHubAgentID)
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
// tracker's context, so the start is refused and never starts.
func TestAsyncStart_RegistryCancelBeforeBegin_TrackerBeforeHandoff(t *testing.T) {
	f := newStartIVFixture(t, "agent-iva")
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(ctx context.Context, k launchKey) {
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
	assertErrorCode(t, w, http.StatusConflict, api.BrokerErrCodeStartCancelled)
	time.Sleep(50 * time.Millisecond)
	if n := f.mgr.StartCallCount(); n != 0 {
		t.Fatalf("Manager.Start called %d time(s) after the stop's tracker cancel", n)
	}
	if !registryEmptyFor(f.srv, f.key) {
		t.Fatal("a refused start left a registry record")
	}
}

// TestAsyncStart_RegistryCancelBeforeBegin_TrackerAfterHandoff is D-T3
// (iv), tracker cancel after the 202: the stop's second registry cancel,
// after its tracker wait, ends the launch.
func TestAsyncStart_RegistryCancelBeforeBegin_TrackerAfterHandoff(t *testing.T) {
	f := newStartIVFixture(t, "agent-ivb")

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
}

// TestAsyncStart_ScopedCancelsLeaveRegistration: a stop or delete naming
// another run, or another agent of the same name, leaves the start's
// registration alone (#2632, #2550).
func TestAsyncStart_ScopedCancelsLeaveRegistration(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-sc"}
	var otherRun, otherAgent bool
	setHook(t, &testHookAsyncLaunchBeforeHandOff, func(_ context.Context, k launchKey) {
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
