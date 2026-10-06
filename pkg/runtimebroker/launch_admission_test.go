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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// asyncManager is a concurrency-safe agent.Manager fake for the async-create
// tests (B-1..B-6): runLaunch calls it from its own goroutine, concurrently
// with the test's assertions, so (unlike mockManager, built for the
// single-goroutine synchronous path) every field here is mutex-guarded.
type asyncManager struct {
	*mockManager // Provision/Reprovision/Stop/Delete/DeleteTarget/List/Message/Watch/Close: unused by these tests

	mu             sync.Mutex
	preflightErr   error
	preflightCalls int
	startErr       error
	startCalls     int
	startBlock     chan struct{} // if non-nil, Start waits on it (or ctx) before returning
	startCancelErr error         // if non-nil, returned instead of ctx.Err() when ctx ends a blocked Start
	cleanupCalls   int
	cleanupLast    []agent.ResourceHandle
	cleanupCtxErr  error         // ctx.Err() at the last CleanupLaunch call
	cleanupBlock   chan struct{} // if non-nil, CleanupLaunch waits on it (or ctx) before returning
	lastStartCtx   context.Context
}

func (m *asyncManager) LastStartCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStartCtx
}

func newAsyncManager() *asyncManager {
	return &asyncManager{mockManager: &mockManager{}}
}

func (m *asyncManager) Preflight(ctx context.Context, opts api.StartOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.preflightCalls++
	return m.preflightErr
}

func (m *asyncManager) PreflightCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.preflightCalls
}

func (m *asyncManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.startCalls++
	m.lastStartCtx = ctx
	block := m.startBlock
	startErr := m.startErr
	cancelErr := m.startCancelErr
	m.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			if cancelErr != nil {
				return nil, cancelErr
			}
			return nil, ctx.Err()
		}
	}
	if startErr != nil {
		return nil, startErr
	}
	return &api.AgentInfo{ID: "container-1", Name: opts.Name, Phase: "running", Runtime: "mock"}, nil
}

func (m *asyncManager) CleanupLaunch(ctx context.Context, handles []agent.ResourceHandle) error {
	m.mu.Lock()
	m.cleanupCalls++
	m.cleanupLast = handles
	m.cleanupCtxErr = ctx.Err()
	block := m.cleanupBlock
	m.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *asyncManager) setCleanupBlock(ch chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupBlock = ch
}

func (m *asyncManager) StartCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCalls
}

func (m *asyncManager) CleanupCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupCalls
}

func (m *asyncManager) setPreflightErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.preflightErr = err
}

func (m *asyncManager) setStartErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startErr = err
}

// waitUntil polls cond every 5ms up to timeout. Used instead of a fixed sleep
// so the tests are fast on a quiet machine and tolerant of a loaded one.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func newAsyncTestServer(t *testing.T, mgr agent.Manager) (*Server, *mockRuntimeBrokerService) {
	t.Helper()
	srv := newTestServerWithManager(t, mgr)
	rtb := &mockRuntimeBrokerService{}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-on-a", HubClient: &stubBrokerHubClient{brokers: rtb}}
	srv.hubMu.Unlock()
	return srv, rtb
}

func postCreate(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func decodeCreateResponse(t *testing.T, w *httptest.ResponseRecorder) CreateAgentResponse {
	t.Helper()
	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

func claimState(req *hubclient.AgentLaunchReport) bool {
	return req.State == hubclient.AgentLaunchReportStateClaim
}

// --- B-1: 201 with the echo before Run completes; a replayed RequestID does
// not launch twice; Preflight's template 404 is synchronous, with no
// goroutine. ---

func TestAsyncCreate_AcceptsBeforeStartReturns(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{}) // Start never returns during this test
	srv, rtb := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-1", "asyncLaunch": true, "launchId": "L-1",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeCreateResponse(t, w)
	if !resp.LaunchPending || resp.LaunchID != "L-1" || resp.LaunchInstanceID == "" {
		t.Fatalf("unexpected accepted response: %+v", resp)
	}
	if resp.Agent != nil {
		t.Fatalf("expected no agent in the accepted response, got %+v", resp.Agent)
	}
	if resp.Created {
		t.Fatalf("expected Created to be false on the accepted response")
	}

	// By construction (writeJSON happens before "go runLaunch" in
	// beginAsyncLaunch) the 201 above was always written before Start could
	// possibly return. Confirm no terminal report has gone out either.
	if !waitUntil(t, time.Second, func() bool {
		for _, r := range rtb.getLaunchReports() {
			if claimState(r.Report) {
				return true
			}
		}
		return false
	}) {
		t.Fatal("expected a claim report eventually")
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("terminal report sent while Start was still blocked: %+v", r.Report)
		}
	}
}

func TestAsyncCreate_RequestIDReplayDoesNotLaunchTwice(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)

	body := map[string]any{
		"name": "agent-2", "requestId": "req-1", "asyncLaunch": true, "launchId": "L-2",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	}
	w1 := postCreate(t, srv, body)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first request: status = %d, body = %s", w1.Code, w1.Body.String())
	}
	resp1 := decodeCreateResponse(t, w1)

	w2 := postCreate(t, srv, body)
	if w2.Code != http.StatusCreated {
		t.Fatalf("replayed request: status = %d, body = %s", w2.Code, w2.Body.String())
	}
	resp2 := decodeCreateResponse(t, w2)

	if resp1.LaunchID != resp2.LaunchID || resp1.LaunchInstanceID != resp2.LaunchInstanceID || !resp2.LaunchPending {
		t.Fatalf("replay returned a different accepted response: %+v vs %+v", resp1, resp2)
	}

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("Start was never called")
	}
	time.Sleep(50 * time.Millisecond) // give a wrongly-duplicated launch a chance to call Start again
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("expected exactly 1 Start call across both requests, got %d", n)
	}
}

func TestAsyncCreate_PreflightTemplateNotFound_SynchronousNoGoroutine(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setPreflightErr(config.ErrTemplateNotFound)
	srv, rtb := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-3", "asyncLaunch": true, "launchId": "L-3",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "missing"},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Deterministic, not timing-dependent: beginAsyncLaunch returns on the
	// Preflight error before registry.Begin or "go runLaunch" ever run.
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must not be called when Preflight fails, got %d calls", n)
	}
	if len(rtb.getLaunchReports()) != 0 {
		t.Fatalf("no launch report should be sent when admission itself refused the request")
	}
}

// TestAsyncCreate_RequestNotTouchedAfterResponse is B-6's -race evidence
// (private-scope passage): once the 201 is written, the test goroutine
// mutates the *http.Request concurrently with runLaunch proceeding in the
// background. Run with -race, this fails if the launch goroutine (or
// anything it calls) ever reads the request.
func TestAsyncCreate_RequestNotTouchedAfterResponse(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	defer close(mgr.startBlock)
	srv, _ := newAsyncTestServer(t, mgr)

	body, err := json.Marshal(map[string]any{
		"name": "agent-13", "asyncLaunch": true, "launchId": "L-13",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// The launch goroutine is running concurrently now (Start is blocked).
	// Mutate the request freely; -race must find nothing.
	req.Header.Set("X-Test-Touch-After-Response", "1")
	_ = req.Context()

	if !waitUntil(t, time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called")
	}
}

// --- B-6: with asyncLaunch absent, behavior is byte-identical; ProvisionOnly
// plus async stays synchronous. ---

func TestAsyncCreate_FlagAbsent_StaysSynchronous(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	// launchId and launchTimeoutSeconds are both present and otherwise
	// eligible, so only the absent asyncLaunch field itself can be keeping
	// this synchronous.
	w := postCreate(t, srv, map[string]any{
		"name": "agent-4", "launchId": "L-4", "launchTimeoutSeconds": 300,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeCreateResponse(t, w)
	if resp.LaunchPending {
		t.Fatalf("LaunchPending must be false with asyncLaunch absent: %+v", resp)
	}
	if resp.Agent == nil || !resp.Created {
		t.Fatalf("expected a synchronous created agent: %+v", resp)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("expected exactly 1 synchronous Start call, got %d", n)
	}
	if n := mgr.PreflightCallCount(); n != 0 {
		t.Fatalf("Preflight must not be called on the synchronous path, got %d calls", n)
	}
	if len(rtb.getLaunchReports()) != 0 {
		t.Fatalf("the synchronous path must send no launch reports")
	}
}

// TestAsyncCreate_ReprovisionStaysSynchronous covers Reprovision taking the
// same precedence over AsyncLaunch that ProvisionOnly does (design §3.2:
// "ProvisionOnly and Reprovision ignore AsyncLaunch"), with ProvisionOnly
// left false so this exercises the gate's own Reprovision check rather than
// the already-covered ProvisionOnly one.
func TestAsyncCreate_ReprovisionStaysSynchronous(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-14", "reprovision": true,
		"asyncLaunch": true, "launchId": "L-14", "launchTimeoutSeconds": 300,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeCreateResponse(t, w)
	if resp.LaunchPending {
		t.Fatalf("Reprovision must ignore AsyncLaunch and stay synchronous: %+v", resp)
	}
	if n := mgr.PreflightCallCount(); n != 0 {
		t.Fatalf("Preflight must not be called on the Reprovision path, got %d calls", n)
	}
	if n := mgr.StartCallCount(); n != 1 {
		t.Fatalf("expected exactly 1 synchronous Start call, got %d", n)
	}
}

// TestAsyncCreate_LaunchTimeoutTooSmallStaysSynchronous covers the
// LaunchTimeoutSeconds sanity check in createAgent's gate (handlers.go): a
// value at or below the broker's 20s abort margin, or the field absent
// entirely, falls back to the synchronous path rather than accepting a
// launch that cannot succeed.
func TestAsyncCreate_LaunchTimeoutTooSmallStaysSynchronous(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"at the margin (20)": {
			"name": "agent-15", "asyncLaunch": true, "launchId": "L-15", "launchTimeoutSeconds": 20,
			"config": map[string]any{"template": "claude"},
		},
		"field absent": {
			"name": "agent-16", "asyncLaunch": true, "launchId": "L-16",
			"config": map[string]any{"template": "claude"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			mgr := newAsyncManager()
			srv, _ := newAsyncTestServer(t, mgr)
			w := postCreate(t, srv, body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			resp := decodeCreateResponse(t, w)
			if resp.LaunchPending {
				t.Fatalf("expected a synchronous response, got %+v", resp)
			}
		})
	}
}

func TestAsyncCreate_ProvisionOnlyStaysSynchronous(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-5", "provisionOnly": true,
		"asyncLaunch": true, "launchId": "L-5", "launchTimeoutSeconds": 300,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeCreateResponse(t, w)
	if resp.LaunchPending {
		t.Fatalf("ProvisionOnly must ignore AsyncLaunch and stay synchronous: %+v", resp)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("ProvisionOnly must not call Start, got %d calls", n)
	}
	if len(rtb.getLaunchReports()) != 0 {
		t.Fatalf("ProvisionOnly must send no launch reports")
	}
}

// --- B-2: answer handling per the §3.8.2 table. ---

func TestAsyncCreate_FailureAfterCompletedClaim_NoCleanupNoExtraReport(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(errors.New("boom"))
	srv, rtb := newAsyncTestServer(t, mgr)

	var sawExtra bool
	var mu sync.Mutex
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil
		}
		mu.Lock()
		sawExtra = true
		mu.Unlock()
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-6", "asyncLaunch": true, "launchId": "L-6",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("Start was never called")
	}
	time.Sleep(100 * time.Millisecond) // let the (no-op) failure branch finish
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("expected no cleanup after an earlier completed answer, got %d calls", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if sawExtra {
		t.Fatal("expected no report after a completed claim and a subsequent failure (send nothing)")
	}
}

func TestAsyncCreate_FailureAnsweredApplied_CleansUpResourcesAndFiles(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(errors.New("boom"))
	srv, rtb := newAsyncTestServer(t, mgr)
	_ = rtb // default launchReportFunc: always "applied"

	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, ".scion", "agents", "agent-7")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte("task"), 0644); err != nil {
		t.Fatalf("write prompt.md: %v", err)
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-7", "asyncLaunch": true, "launchId": "L-7",
		"launchTimeoutSeconds": 300, "projectPath": projectDir,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected CleanupLaunch to be called after a failed report answered applied")
	}
	if !waitUntil(t, 2*time.Second, func() bool {
		_, err := os.Stat(agentDir)
		return os.IsNotExist(err)
	}) {
		t.Fatalf("expected the agent directory %s to be removed after cleanup", agentDir)
	}
}

func TestAsyncCreate_SucceededAfterCompletedClaim_StillSendsSucceeded(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	var terminalSeen int
	var mu sync.Mutex
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, nil
		}
		if req.State == hubclient.AgentLaunchReportStateSucceeded {
			mu.Lock()
			terminalSeen++
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-7b", "asyncLaunch": true, "launchId": "L-7b",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// design §3.8.2 step 5.6, F3: success sends `succeeded` even after an
	// earlier `completed` answer.
	if !waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return terminalSeen >= 1
	}) {
		t.Fatal("expected a succeeded report even after an earlier completed answer")
	}
}

// --- B-4: a second replica's claim gets other_owner; Manager.Start is never
// called; no cleanup. ---

func TestAsyncCreate_ClaimOtherOwner_AbortsNoStartNoCleanup(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-8", "asyncLaunch": true, "launchId": "L-8",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, time.Second, func() bool { return len(rtb.getLaunchReports()) >= 1 }) {
		t.Fatal("expected a claim report")
	}
	time.Sleep(100 * time.Millisecond)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called after a 409 other_owner claim, got %d calls", n)
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("other_owner must not clean up (the owning launch holds the names), got %d calls", n)
	}
	if got := len(rtb.getLaunchReports()); got != 1 {
		t.Fatalf("expected no terminal report after other_owner, got %d reports", got)
	}
}

// TestAsyncCreate_AbortRecordedBeforeClaimSucceeds_NoStart covers the
// IsAborted check right after the claim succeeds. The claim is deliberately
// held blocked until a keepalive abort has already been recorded (and given
// time to finish being recorded), so the claim's own gate sees applied/
// gateContinue and the only thing that can stop the launch from here is this
// specific check.
//
// To make this check's removal observable on its own (not just masked by
// the identical IsAborted checks later in runLaunch), a predecessor record
// is pre-registered for the same key and deliberately never released: if
// this check does its job, the launch returns before ever reaching
// WaitSuperseded, so the stuck predecessor is irrelevant. If this check is
// missing, the launch falls through into WaitSuperseded and blocks forever,
// so cleanup never happens within the wait below -- a distinct,
// unmistakable failure rather than one a later check could incidentally
// paper over.
func TestAsyncCreate_AbortRecordedBeforeClaimSucceeds_NoStart(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	key := launchKey{Slug: "agent-guard-b"}
	stuckRec := newLaunchRecord("L-stuck-for-guard-b", "agent-stuck-for-guard-b", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	srv.launchRegistry.Begin(key, stuckRec) // never released by this test

	releaseClaim := make(chan struct{})
	abortAnswered := make(chan struct{})
	var abortAnsweredOnce sync.Once
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			<-releaseClaim
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		abortAnsweredOnce.Do(func() { close(abortAnswered) })
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-guard-b", "asyncLaunch": true,
		"launchId":             "L-guard-b",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case <-abortAnswered:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the keepalive's abort-triggering answer")
	}
	// Let recordKeepaliveAbort finish (it runs just after the call above
	// returns, in the keepalive's own goroutine) well before the claim is
	// allowed to proceed.
	time.Sleep(100 * time.Millisecond)
	close(releaseClaim)

	if !waitUntil(t, 3*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected the already-recorded abort to trigger cleanup right after the claim succeeds, without ever blocking on WaitSuperseded (the predecessor here is never released)")
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the abort was already recorded before the claim succeeded, got %d calls", n)
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("an abort recorded before the claim succeeds must send no terminal, got %+v", r.Report)
		}
	}
}

// TestAsyncCreate_AbortRecordedDuringWaitSuperseded_NoMarkerNoStart covers
// the IsAborted check right after WaitSuperseded/the marker write: a
// pre-registered record for the same key makes WaitSuperseded actually
// block, during which a keepalive abort is recorded (and given time to
// finish); the claim has already succeeded by then (the earlier IsAborted
// check passed), so only this specific check can be stopping the launch.
//
// workspaceStoragePath is set (with a fake, failing download) so that if
// this check is missing, the launch falls through into the GCS download
// step, which records the call and fails fast -- observable independently of the IsAborted check after the
// download (that one would never even be reached, since a download error
// fails the launch directly), so this test cannot be satisfied by that
// later check doing the work instead.
func TestAsyncCreate_AbortRecordedDuringWaitSuperseded_NoMarkerNoStart(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	// A real WorktreeBase, so the workspace directory passes validation
	// (at admission and in the download) and a launch that wrongly reaches
	// the download calls the fake this test looks at.
	srv.config.WorktreeBase = t.TempDir()
	fake := installFakeWorkspaceSync(t, errors.New("fake sync failure"))

	key := launchKey{Slug: "agent-guard-c"}
	oldRec := newLaunchRecord("L-old-for-guard-c", "agent-old-for-guard-c", store.LaunchKindCreate, "", time.Now().Add(time.Hour), func() {})
	srv.launchRegistry.Begin(key, oldRec)

	abortAnswered := make(chan struct{})
	var abortAnsweredOnce sync.Once
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		abortAnsweredOnce.Do(func() { close(abortAnswered) })
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-guard-c", "asyncLaunch": true,
		"launchId":             "L-guard-c",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"workspaceStoragePath":   "some/path",
		"workspaceStorageBucket": "hub-bucket",
		"config":                 map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case <-abortAnswered:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the keepalive's abort-triggering answer")
	}
	// The claim already succeeded well before this (it is answered
	// immediately, with no blocking); the new launch is now parked in
	// WaitSuperseded, since oldRec is still registered. Let
	// recordKeepaliveAbort finish, then release the predecessor.
	time.Sleep(100 * time.Millisecond)
	srv.launchRegistry.Finish(key, oldRec)

	if !waitUntil(t, 3*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected the already-recorded abort to trigger cleanup once WaitSuperseded returns")
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the abort was already recorded during WaitSuperseded, got %d calls", n)
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded {
			t.Fatalf("an abort recorded during WaitSuperseded must send no terminal, got %+v", r.Report)
		}
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("the launch reached the GCS download step despite the abort already being recorded during WaitSuperseded, got %+v", calls)
	}
}

// TestAsyncCreate_ClaimStopNoCleanup_401 covers a 401 claim answer
// classifying to gateStopNoCleanup (design §3.8.2's table has no reason for
// 400/401): Start must never be called, nothing is cleaned up, and no
// terminal is sent, the same as the gateAbortNoCleanup cases above.
func TestAsyncCreate_ClaimStopNoCleanup_401(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-claim-401", "asyncLaunch": true, "launchId": "L-claim-401",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, time.Second, func() bool { return len(rtb.getLaunchReports()) >= 1 }) {
		t.Fatal("expected a claim report")
	}
	time.Sleep(100 * time.Millisecond)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called after a 401 claim, got %d calls", n)
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("a 401 claim must not clean up, got %d calls", n)
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("a 401 claim must send no terminal, got %+v", r.Report)
		}
	}
}

// TestAsyncCreate_ClaimSuperseded_AbortsNoStartNoCleanup covers B-2/B-4's
// superseded case specifically (distinct from other_owner, though both
// classify to gateAbortNoCleanup).
func TestAsyncCreate_ClaimSuperseded_AbortsNoStartNoCleanup(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonSuperseded}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-10", "asyncLaunch": true, "launchId": "L-10",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, time.Second, func() bool { return len(rtb.getLaunchReports()) >= 1 }) {
		t.Fatal("expected a claim report")
	}
	time.Sleep(100 * time.Millisecond)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called after a 409 superseded claim, got %d calls", n)
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("superseded must not clean up, got %d calls", n)
	}
}

// TestAsyncCreate_ClaimDeletedAbortsWithCleanup covers B-2's abort-cleanup
// branch at the claim: a 409 the Hub uses for "the agent never ran and this
// launch is over" (deleted here; timed_out/lost/stopped/failed/not_launched,
// 403 and 404 agent_launch_unknown all classify the same way).
func TestAsyncCreate_ClaimDeletedAbortsWithCleanup(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-11", "asyncLaunch": true, "launchId": "L-11",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected CleanupLaunch to be called after a 409 deleted claim")
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called after an abort-cleanup claim answer, got %d calls", n)
	}
}

// TestAsyncCreate_KeepaliveAbortDuringStart_CancelsAndCleansUp covers design
// §3.8.2's gate-answer table applying to keepalives too, end to end: a
// stale 409 landing on a keepalive while Manager.Start is still blocked must
// cancel ctx' and clean up immediately, without waiting for Start to return
// on its own and without sending a failed/succeeded terminal (the Hub
// already said this launch is over).
func TestAsyncCreate_KeepaliveAbortDuringStart_CancelsAndCleansUp(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	defer close(mgr.startBlock)
	srv, rtb := newAsyncTestServer(t, mgr)

	var mu sync.Mutex
	keepalives := 0
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		mu.Lock()
		keepalives++
		n := keepalives
		mu.Unlock()
		if n == 1 {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonLost}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-12", "asyncLaunch": true, "launchId": "L-12",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called (and then blocked)")
	}
	if !waitUntil(t, 5*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected a keepalive abort to trigger CleanupLaunch while Start was still blocked")
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("a keepalive-triggered abort must send no terminal, got %+v", r.Report)
		}
	}
}

// TestAsyncCreate_Keepalive401DoesNotAbortDuringStart covers gateStopNoCleanup
// specifically on a keepalive (design §3.8.2's table has no entry for
// 400/401, so unlike every other non-continue answer it must not abort):
// a 401 landing on a keepalive while Manager.Start is still blocked must
// leave the launch running, not cancel ctx' or record an abort. Start is
// then allowed to complete, and the succeeded terminal must still be sent.
func TestAsyncCreate_Keepalive401DoesNotAbortDuringStart(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	srv, rtb := newAsyncTestServer(t, mgr)

	var mu sync.Mutex
	keepalives := 0
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		if req.State == hubclient.AgentLaunchReportStateProgress {
			mu.Lock()
			keepalives++
			mu.Unlock()
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, nil
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-keepalive-401", "asyncLaunch": true, "launchId": "L-keepalive-401",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return keepalives >= 1
	}) {
		t.Fatal("expected at least one keepalive attempt answered 401")
	}
	// Give the sender a moment to (incorrectly, if the guard were missing)
	// record an abort before Start is released.
	time.Sleep(100 * time.Millisecond)
	close(mgr.startBlock)

	if !waitUntil(t, 3*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called and allowed to complete")
	}
	if !waitUntil(t, 3*time.Second, func() bool {
		for _, r := range rtb.getLaunchReports() {
			if r.Report.State == hubclient.AgentLaunchReportStateSucceeded {
				return true
			}
		}
		return false
	}) {
		t.Fatal("expected a succeeded terminal to be sent despite the 401 keepalive answers")
	}
	if n := mgr.CleanupCallCount(); n != 0 {
		t.Fatalf("a 401 keepalive answer must never trigger cleanup, got %d calls", n)
	}
}

// TestAsyncCreate_KeepaliveAnswerAfterTerminalStarts_DoesNotCutOffRetries
// covers design §3.8.5's "the keepalive stops when the terminal's first
// attempt starts; from then on only the terminal's answer decides cleanup":
// an in-flight keepalive attempt that was already dispatched before the
// terminal started must not be allowed to end the terminal's own retries (or
// record an outcome nothing then acts on) just because its answer lands
// after markTerminalStarted. The succeeded terminal's own first attempt is
// unreachable; its second attempt is what actually decides cleanup.
func TestAsyncCreate_KeepaliveAnswerAfterTerminalStarts_DoesNotCutOffRetries(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{}) // held open until the keepalive is confirmed in flight
	srv, rtb := newAsyncTestServer(t, mgr)

	keepaliveInFlight := make(chan struct{})
	releaseKeepalive := make(chan struct{})
	var keepaliveInFlightOnce sync.Once
	var mu sync.Mutex
	var terminalAttempts int

	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		switch req.State {
		case hubclient.AgentLaunchReportStateClaim:
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		case hubclient.AgentLaunchReportStateProgress:
			// Signal that this keepalive attempt is now in flight, then
			// wait until the test says the terminal has started before
			// answering -- an answer that classifies to an abort action
			// (409 timed_out is one of the design's listed cleanup
			// reasons), arriving only once the terminal is already under
			// way. keepaliveInFlightOnce guards against the mock's own
			// attempt timeout elapsing before releaseKeepalive closes and
			// retrying this call a second time.
			keepaliveInFlightOnce.Do(func() { close(keepaliveInFlight) })
			<-releaseKeepalive
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonTimedOut}, nil
		case hubclient.AgentLaunchReportStateSucceeded:
			mu.Lock()
			terminalAttempts++
			n := terminalAttempts
			mu.Unlock()
			if n == 1 {
				// The terminal has now started (markTerminalStarted runs
				// before this call). Let the in-flight keepalive answer
				// land while the terminal's own retries are still ahead
				// of it.
				close(releaseKeepalive)
				return nil, errors.New("simulated unreachable")
			}
			// The terminal's own second attempt is what decides cleanup,
			// matching the design's late-success rule (§3.10).
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonTimedOut}, nil
		default:
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-keepalive-after-terminal", "asyncLaunch": true,
		"launchId":             "L-keepalive-after-terminal",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case <-keepaliveInFlight:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the keepalive's first attempt to be dispatched")
	}
	// Now let Start return: the terminal starts only after this, so the
	// keepalive attempt above is reliably in flight (and will stay blocked
	// on releaseKeepalive) before markTerminalStarted runs.
	close(mgr.startBlock)

	if !waitUntil(t, 15*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		mu.Lock()
		n := terminalAttempts
		mu.Unlock()
		t.Fatalf("expected the terminal's own retries to eventually trigger cleanup, got %d terminal attempts and %d cleanup calls", n, mgr.CleanupCallCount())
	}
	mu.Lock()
	n := terminalAttempts
	mu.Unlock()
	if n < 2 {
		t.Fatalf("expected at least 2 terminal attempts (the stale keepalive answer must not cut the terminal's retries short), got %d", n)
	}
	if n := mgr.CleanupCallCount(); n != 1 {
		t.Fatalf("expected exactly 1 cleanup call, got %d", n)
	}
}

// TestAsyncCreate_AbortRecordedAsStartReturns_CleansUpConsistently covers the
// select in runLaunch that chooses between Manager.Start's completion and
// KeepaliveAborted: when a keepalive answer is fully processed (and so
// recorded as an abort) well before Start returns, the eventual outcome must
// still be correct -- cleanup runs exactly once and no terminal is sent --
// regardless of which of the select's two cases actually wakes the
// goroutine. (Go's select only picks randomly between cases that are
// *already* ready at the moment it is evaluated; once the keepalive's
// answer has fully landed, a parked select wakes on KeepaliveAborted as
// soon as it closes, well before Start's own completion -- so this test
// reliably exercises that path deterministically, without depending on
// which of the two happens to complete first at a microsecond scale.)
func TestAsyncCreate_AbortRecordedAsStartReturns_CleansUpConsistently(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{})
	srv, rtb := newAsyncTestServer(t, mgr)

	abortAnswered := make(chan struct{})
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		defer close(abortAnswered)
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-abort-as-start-returns", "asyncLaunch": true,
		"launchId":             "L-abort-as-start-returns",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	select {
	case <-abortAnswered:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the keepalive's abort-triggering answer")
	}
	// Give the sender's own goroutine a moment to finish classifying the
	// answer and recording the abort (recordKeepaliveAbort runs just after
	// the call above returns, in the same goroutine) -- well before letting
	// Start return, so the abort is unambiguously recorded first.
	time.Sleep(100 * time.Millisecond)
	close(mgr.startBlock)

	if !waitUntil(t, 3*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected the recorded abort to trigger cleanup")
	}
	if !waitUntil(t, time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to have been called")
	}
	if n := mgr.CleanupCallCount(); n != 1 {
		t.Fatalf("expected exactly 1 cleanup call, got %d", n)
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("an abort recorded before Start returns must send no terminal, got %+v", r.Report)
		}
	}
}

// TestAsyncCreate_KeepaliveAbortWhileClaimUnreachable_CleansUpWithoutStart
// covers a keepalive answer ending the launch while the claim itself is
// still blocked retrying against an unreachable Hub (the keepalive starts
// before the claim, design §3.8.5, so its answers can end the launch before
// the claim ever gets through): Manager.Start must never run, and the
// claim's own retry loop must not keep it blocked until ctx' expires and
// reports a separate failed{hub_unreachable}.
func TestAsyncCreate_KeepaliveAbortWhileClaimUnreachable_CleansUpWithoutStart(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	var mu sync.Mutex
	keepalives := 0
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return nil, errors.New("simulated unreachable")
		}
		mu.Lock()
		keepalives++
		n := keepalives
		mu.Unlock()
		if n == 1 {
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonDeleted}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-keepalive-abort-unreachable-claim", "asyncLaunch": true,
		"launchId":             "L-keepalive-abort-unreachable-claim",
		"launchTimeoutSeconds": 300, "launchKeepaliveSeconds": 1,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 5*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected a keepalive abort to trigger CleanupLaunch while the claim was still unreachable")
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the keepalive ends the launch before the claim gets through, got %d calls", n)
	}
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("a keepalive-triggered abort must send no terminal, got %+v", r.Report)
		}
	}
}

// --- B-3: a claim blocked by an unreachable Hub retries until ctx' expires,
// then sends failed{hub_unreachable}. ---

func TestAsyncCreate_ClaimUnreachableUntilDeadline_SendsHubUnreachable(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	var mu sync.Mutex
	var terminalCode, terminalStep string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return nil, errors.New("simulated unreachable")
		}
		mu.Lock()
		terminalCode = req.ErrorCode
		terminalStep = req.Step
		mu.Unlock()
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	// LaunchTimeoutSeconds=23 gives ctx' a ~3s budget (23 - the 20s abort
	// margin), enough to be robust against admission overhead without
	// making the test slow.
	w := postCreate(t, srv, map[string]any{
		"name": "agent-9", "asyncLaunch": true, "launchId": "L-9",
		"launchTimeoutSeconds": 23, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 8*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return terminalCode == "hub_unreachable"
	}) {
		t.Fatal("expected a failed{hub_unreachable} terminal once ctx' expired")
	}
	mu.Lock()
	if terminalStep != "claim" {
		t.Errorf("Step = %q, want claim", terminalStep)
	}
	mu.Unlock()
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the claim never got through, got %d calls", n)
	}
}

// TestAsyncCreate_ClaimLocallyCancelled_SendsNoTerminal covers a local
// stop/delete (launchRegistry.CancelLocal) waking a claim that is still
// blocked on an unreachable Hub: the Hub already knows, or will
// independently learn, that this launch is over, so this must not be
// reported as failed{hub_unreachable} the way a real ctx' deadline is.
func TestAsyncCreate_ClaimLocallyCancelled_SendsNoTerminal(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return nil, errors.New("simulated unreachable")
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-cancel-local", "asyncLaunch": true, "launchId": "L-cancel-local",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, time.Second, func() bool { return len(rtb.getLaunchReports()) >= 1 }) {
		t.Fatal("expected at least one claim attempt")
	}
	srv.launchRegistry.CancelLocal(launchKey{Slug: "agent-cancel-local"})

	time.Sleep(200 * time.Millisecond) // let runLaunch observe the cancellation
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateFailed {
			t.Fatalf("a locally cancelled claim must send no terminal, got %+v", r.Report)
		}
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called after a locally cancelled claim, got %d calls", n)
	}
}

// fakeWorkspaceSync replaces syncWorkspaceFromGCS for the test, recording
// each call's bucket and storage path and returning err.
type fakeWorkspaceSync struct {
	mu    sync.Mutex
	calls []fakeWorkspaceSyncCall
}

type fakeWorkspaceSyncCall struct{ bucket, prefix, dest string }

func installFakeWorkspaceSync(t *testing.T, err error) *fakeWorkspaceSync {
	t.Helper()
	f := &fakeWorkspaceSync{}
	prev := syncWorkspaceFromGCS
	syncWorkspaceFromGCS = func(_ context.Context, bucket, prefix, dest string) error {
		f.mu.Lock()
		f.calls = append(f.calls, fakeWorkspaceSyncCall{bucket, prefix, dest})
		f.mu.Unlock()
		return err
	}
	t.Cleanup(func() { syncWorkspaceFromGCS = prev })
	return f
}

func (f *fakeWorkspaceSync) Calls() []fakeWorkspaceSyncCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeWorkspaceSyncCall(nil), f.calls...)
}

// assertWorkspaceStorageUnconfigured checks w is the broker's explicit 422
// refusal for a workspace upload it has no bucket for (ptone/scion#3422).
func assertWorkspaceStorageUnconfigured(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s; want 422", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeWorkspaceStorageUnconfigured {
		t.Fatalf("code = %q, want %q", errResp.Error.Code, ErrCodeWorkspaceStorageUnconfigured)
	}
	if errResp.Error.Message != workspaceStorageUnconfiguredMessage {
		t.Fatalf("message = %q, want %q", errResp.Error.Message, workspaceStorageUnconfiguredMessage)
	}
}

// TestCreateAgent_SyncGCSNoBucket_Returns422 covers a synchronous create
// carrying a workspace upload when neither the request nor the broker names
// a bucket (ptone/scion#3422): it is refused with a typed 422 rather than a
// runtime error the hub would turn into a 502, before the workspace
// directory is created, and nothing is downloaded or started.
func TestCreateAgent_SyncGCSNoBucket_Returns422(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	// A real WorktreeBase, so the workspace directory passes validation and
	// the download reaches the bucket check.
	srv.config.WorktreeBase = t.TempDir()
	fake := installFakeWorkspaceSync(t, nil)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-sync-gcs", "workspaceStoragePath": "some/path",
		"config": map[string]any{"template": "claude"},
	})
	assertWorkspaceStorageUnconfigured(t, w)
	if _, err := os.Lstat(filepath.Join(srv.config.WorktreeBase, "agent-sync-gcs")); !os.IsNotExist(err) {
		t.Fatalf("expected no workspace directory for a refused create, Lstat err = %v", err)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("expected no download, got %+v", calls)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called without a workspace bucket, got %d calls", n)
	}
}

// TestCreateAgent_SyncGCSDownload_UsesRequestBucket covers the broker
// downloading the workspace upload from the bucket the hub sent with the
// create, even though the broker has no bucket setting of its own
// (ptone/scion#3422).
func TestCreateAgent_SyncGCSDownload_UsesRequestBucket(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	srv.config.WorktreeBase = t.TempDir()
	fake := installFakeWorkspaceSync(t, nil)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-sync-gcs-bucket", "workspaceStoragePath": "some/path",
		"workspaceStorageBucket": "hub-bucket",
		"config":                 map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected one download, got %+v", calls)
	}
	if calls[0].bucket != "hub-bucket" || calls[0].prefix != "some/path/files" {
		t.Fatalf("download = %+v, want bucket hub-bucket and prefix some/path/files", calls[0])
	}
}

// TestCreateAgent_SyncGCSDownloadFailure_PinsOriginalErrorText covers the
// synchronous path's GCS-download failure body staying byte-identical to
// what it was before downloadWorkspaceFromGCS existed as a separate
// function: capitalized, with no wrapped-error prefix.
func TestCreateAgent_SyncGCSDownloadFailure_PinsOriginalErrorText(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	srv.config.WorktreeBase = t.TempDir()
	srv.config.StorageBucket = "broker-bucket"
	installFakeWorkspaceSync(t, errors.New("fake sync failure"))

	w := postCreate(t, srv, map[string]any{
		"name": "agent-sync-gcs-fail", "workspaceStoragePath": "some/path",
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Message != "Failed to download workspace from GCS: fake sync failure" {
		t.Fatalf("message = %q, want the byte-identical capitalized GCS error text", errResp.Error.Message)
	}
}

// symlinkedWorktreeAgentDir points <WorktreeBase>/<name> at a directory
// outside WorktreeBase, so the GCS bootstrap's workspace directory
// (<WorktreeBase>/<name>/workspace) fails workspace-source validation, and
// returns that outside directory. Creating the workspace directory before
// validating it would follow the symlink and leave <outside>/workspace
// behind, which the callers assert does not happen.
func symlinkedWorktreeAgentDir(t *testing.T, srv *Server, name string) string {
	t.Helper()
	srv.config.WorktreeBase = t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(srv.config.WorktreeBase, name)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	return outside
}

func assertNoOutsideWorkspace(t *testing.T, outside string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(outside, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("expected nothing created through the symlink, got Lstat(%s/workspace) err = %v", outside, err)
	}
}

// TestCreateAgent_SyncGCSDownloadInvalidWorkspaceDir_Returns400 covers the
// synchronous path rejecting a GCS-bootstrap workspace directory that fails
// workspace-source validation as a client error (400), before the directory
// is created: the agent directory under WorktreeBase is a symlink to a
// directory outside it, and nothing may be created through that symlink.
func TestCreateAgent_SyncGCSDownloadInvalidWorkspaceDir_Returns400(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	outside := symlinkedWorktreeAgentDir(t, srv, "agent-sync-gcs-invalid")

	w := postCreate(t, srv, map[string]any{
		"name": "agent-sync-gcs-invalid", "workspaceStoragePath": "some/path",
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.HasPrefix(errResp.Error.Message, "Invalid workspace directory: ") {
		t.Fatalf("message = %q, want the \"Invalid workspace directory: \" text", errResp.Error.Message)
	}
	assertNoOutsideWorkspace(t, outside)
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called for an invalid workspace directory, got %d calls", n)
	}
}

// TestAsyncCreate_InvalidWorkspaceDir_Returns400BeforeAccept covers an async
// create whose GCS-bootstrap workspace directory fails validation: it is
// answered with the same 400 as the synchronous path, before the launch is
// accepted, so no launch is registered, no launch report is sent, Start is
// never called and nothing is created through the symlink.
func TestAsyncCreate_InvalidWorkspaceDir_Returns400BeforeAccept(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	outside := symlinkedWorktreeAgentDir(t, srv, "agent-async-gcs-invalid")

	w := postCreate(t, srv, map[string]any{
		"name": "agent-async-gcs-invalid", "asyncLaunch": true, "launchId": "L-async-gcs-invalid",
		"launchTimeoutSeconds": 300, "workspaceStoragePath": "some/path",
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.HasPrefix(errResp.Error.Message, "Invalid workspace directory: ") {
		t.Fatalf("message = %q, want the \"Invalid workspace directory: \" text", errResp.Error.Message)
	}
	// Registration happens before the 201, so a record for the key would
	// mean the launch had been accepted.
	srv.launchRegistry.mu.Lock()
	rec, registered := srv.launchRegistry.records[launchKey{Slug: "agent-async-gcs-invalid"}]
	srv.launchRegistry.mu.Unlock()
	if registered {
		t.Fatalf("expected no launch registered for a launch rejected before the 201, got launch %q", rec.ID)
	}
	if reports := rtb.getLaunchReports(); len(reports) != 0 {
		t.Fatalf("expected no launch reports for a launch rejected before the 201, got %d", len(reports))
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called for an invalid workspace directory, got %d calls", n)
	}
	assertNoOutsideWorkspace(t, outside)
}

// TestAsyncCreate_GCSDownloadRunsOnlyOnceInRunLaunch covers the GCS download
// running only inside runLaunch for an accepted async create, never
// synchronously during admission: the fake download fails, so a synchronous
// download attempt (the regression this guards) would fail admission itself
// with a 500, never reaching the 201 accept.
func TestAsyncCreate_GCSDownloadRunsOnlyOnceInRunLaunch(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	// A real WorktreeBase, so runLaunch's download passes workspace
	// directory validation and reaches the (fake, failing) download.
	srv.config.WorktreeBase = t.TempDir()
	fake := installFakeWorkspaceSync(t, errors.New("fake sync failure"))

	var mu sync.Mutex
	var failedMessage string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateFailed {
			mu.Lock()
			failedMessage = req.Message
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-17", "asyncLaunch": true, "launchId": "L-17",
		"launchTimeoutSeconds": 300, "workspaceStoragePath": "some/path",
		"workspaceStorageBucket": "hub-bucket",
		"config":                 map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s (the GCS download must not run during admission)", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failedMessage != ""
	}) {
		t.Fatal("expected a failed report once runLaunch's own download attempt failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(failedMessage, "failed to download workspace from GCS") {
		t.Fatalf("failed message = %q, want runLaunch's GCS download failure", failedMessage)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0].bucket != "hub-bucket" {
		t.Fatalf("downloads = %+v, want exactly one, from the request's bucket", calls)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the download fails first, got %d calls", n)
	}
}

// TestAsyncCreate_NoWorkspaceBucket_Returns422BeforeAccept covers an async
// create carrying a workspace upload with no bucket in the request or on
// the broker (ptone/scion#3422): it gets the synchronous path's 422 before
// the launch is accepted, so no launch is registered or reported and
// nothing is downloaded or started.
func TestAsyncCreate_NoWorkspaceBucket_Returns422BeforeAccept(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	srv.config.WorktreeBase = t.TempDir()
	fake := installFakeWorkspaceSync(t, nil)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-async-no-bucket", "asyncLaunch": true, "launchId": "L-async-no-bucket",
		"launchTimeoutSeconds": 300, "workspaceStoragePath": "some/path",
		"config": map[string]any{"template": "claude"},
	})
	assertWorkspaceStorageUnconfigured(t, w)
	srv.launchRegistry.mu.Lock()
	_, registered := srv.launchRegistry.records[launchKey{Slug: "agent-async-no-bucket"}]
	srv.launchRegistry.mu.Unlock()
	if registered {
		t.Fatal("expected no launch registered for a launch rejected before the 201")
	}
	if reports := rtb.getLaunchReports(); len(reports) != 0 {
		t.Fatalf("expected no launch reports, got %d", len(reports))
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("expected no download, got %+v", calls)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called without a workspace bucket, got %d calls", n)
	}
}

// TestAsyncCreate_StartSeesAdmissionContextValues covers ctx' being derived
// from the admission context (which carries values createAgent attaches
// after r.Context() was read), not r.Context() itself: a value the Hub's
// config attached before the async gate must still be visible inside
// Manager.Start.
func TestAsyncCreate_StartSeesAdmissionContextValues(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-18", "asyncLaunch": true, "launchId": "L-18",
		"launchTimeoutSeconds": 300,
		"config": map[string]any{
			"template":         "claude",
			"hubAgentDefaults": map[string]any{"maxTurns": 42},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called")
	}
	startCtx := mgr.LastStartCtx()
	if startCtx == nil {
		t.Fatal("Start was called with a nil ctx")
	}
	defaults := api.HubAgentDefaultsFromContext(startCtx)
	if defaults == nil || defaults.MaxTurns != 42 {
		t.Fatalf("HubAgentDefaultsFromContext(startCtx) = %+v, want MaxTurns=42", defaults)
	}
}

// TestAsyncCreate_StartTimeoutNamesLaunchingStep covers design §3.9's generic
// claim -> launching step sequence: once Manager.Start is reached, a failure
// (here, ctx' expiring while Start is still blocked) is reported with
// Step == "launching" and ErrorCode == "launch_timeout".
func TestAsyncCreate_StartTimeoutNamesLaunchingStep(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{}) // never closed: Start blocks until ctx' expires
	defer close(mgr.startBlock)
	srv, rtb := newAsyncTestServer(t, mgr)

	var mu sync.Mutex
	var step, code string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateFailed {
			mu.Lock()
			step, code = req.Step, req.ErrorCode
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	// LaunchTimeoutSeconds=23 gives ctx' a ~3s budget (23 - the 20s abort margin).
	w := postCreate(t, srv, map[string]any{
		"name": "agent-19", "asyncLaunch": true, "launchId": "L-19",
		"launchTimeoutSeconds": 23, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 8*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return code == "launch_timeout"
	}) {
		t.Fatal("expected a failed{launch_timeout} terminal once ctx' expired while Start was blocked")
	}
	mu.Lock()
	defer mu.Unlock()
	if step != "launching" {
		t.Errorf("Step = %q, want launching", step)
	}
}

// TestAsyncCreate_SucceededAnsweredTimedOut_CleansUp covers design §3.10's
// "late succeeded answered 409 timed_out" case end to end: the Hub has
// already recorded the launch as over by the time the succeeded report
// lands, so the broker must remove what it just started.
func TestAsyncCreate_SucceededAnsweredTimedOut_CleansUp(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateSucceeded {
			return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: hubclient.AgentLaunchReportReasonTimedOut}, nil
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-20", "asyncLaunch": true, "launchId": "L-20",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected a succeeded report answered 409 timed_out to trigger cleanup")
	}
}

// TestAsyncCreate_MarkerRemovedAfterSuccessfulLaunch covers the launch
// marker being removed once the launch ends successfully, not just on abort
// (design §3.8.4: "The launch removes the marker when it ends, if it still
// holds L").
func TestAsyncCreate_MarkerRemovedAfterSuccessfulLaunch(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)

	projectDir := t.TempDir()
	w := postCreate(t, srv, map[string]any{
		"name": "agent-21", "asyncLaunch": true, "launchId": "L-21",
		"launchTimeoutSeconds": 300, "projectPath": projectDir,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() >= 1 }) {
		t.Fatal("expected Start to be called")
	}
	if !waitUntil(t, time.Second, func() bool {
		return !launchMarkerMatches(projectDir, false, "agent-21", "L-21")
	}) {
		t.Fatal("expected the launch marker to be removed once the launch ended successfully")
	}
}

// TestAsyncCreate_ForcesHeartbeatAfterSuccess covers a successful launch
// forcing an immediate heartbeat (rather than waiting for the next regular
// tick), matching the synchronous create path's existing behavior.
func TestAsyncCreate_ForcesHeartbeatAfterSuccess(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	hb := NewHeartbeatService(rtb, "broker-on-a", MinHeartbeatInterval, nil, nil, slog.Default())
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"].Heartbeat = hb
	srv.hubMu.Unlock()

	w := postCreate(t, srv, map[string]any{
		"name": "agent-22", "asyncLaunch": true, "launchId": "L-22",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool { return len(rtb.getHeartbeatCalls()) >= 1 }) {
		t.Fatal("expected a forced heartbeat after the succeeded terminal was answered")
	}
}

// TestAsyncCreate_MarkerWriteFailureFailsLaunch covers a launch marker write
// failure failing the launch outright (a failed report, Start never called)
// rather than only logging a warning and continuing with resource cleanup
// silently disabled for the rest of the launch.
func TestAsyncCreate_MarkerWriteFailureFailsLaunch(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	projectDir := t.TempDir()
	dir, err := launchMarkersDir(projectDir, false)
	if err != nil {
		t.Fatalf("launchMarkersDir: %v", err)
	}
	// Put a regular file where the marker directory needs to be, so
	// writeLaunchMarker's os.MkdirAll fails.
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("write file at marker directory path: %v", err)
	}

	var mu sync.Mutex
	var failedCode string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateFailed {
			mu.Lock()
			failedCode = req.ErrorCode
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"name": "agent-marker-fail", "asyncLaunch": true, "launchId": "L-marker-fail",
		"launchTimeoutSeconds": 300, "projectPath": projectDir,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	if !waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failedCode != ""
	}) {
		t.Fatal("expected a failed report once the marker write failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if failedCode != "runtime_error" {
		t.Fatalf("ErrorCode = %q, want runtime_error", failedCode)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the marker write fails, got %d calls", n)
	}
}

// TestCreateAgent_HubManagedGCSBootstrap_NotAmbiguous is the #2760 r1 B1
// regression: for a shared non-git hub-managed project dispatched to a
// remote broker with hub storage, the hub clears the workspace and sends
// only projectSlug + workspaceStoragePath. That upload is the explicit
// workspace source, so the create must reach the GCS download (here: the
// missing storage bucket's 422) rather than be refused as ambiguous.
func TestCreateAgent_HubManagedGCSBootstrap_NotAmbiguous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-hub-gcs", "id": "agent-hub-gcs-id", "projectId": "proj-1",
		"projectSlug": "notes", "workspaceStoragePath": "workspaces/proj-1/agent-hub-gcs-id",
		"config": map[string]any{"template": "claude", "task": "go"},
	})
	if strings.Contains(w.Body.String(), "ambiguous workspace") {
		t.Fatalf("GCS-bootstrap create refused as ambiguous: %d %s", w.Code, w.Body.String())
	}
	assertWorkspaceStorageUnconfigured(t, w)
}

// TestAsyncCreate_HubManagedGCSBootstrap_NotAmbiguous is B1's async
// counterpart: admission accepts (201) and runLaunch's own download is what
// fails, not buildStartContext's workspace-source check.
func TestAsyncCreate_HubManagedGCSBootstrap_NotAmbiguous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	installFakeWorkspaceSync(t, errors.New("fake sync failure"))

	var mu sync.Mutex
	var failedMessage string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateFailed {
			mu.Lock()
			failedMessage = req.Message
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"workspaceStorageBucket": "hub-bucket",
		"name":                   "agent-hub-gcs-async", "id": "agent-hub-gcs-async-id", "projectId": "proj-1",
		"projectSlug": "notes", "workspaceStoragePath": "workspaces/proj-1/agent-hub-gcs-async-id",
		"asyncLaunch": true, "launchId": "L-hub-gcs", "launchTimeoutSeconds": 300,
		"config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s; want the launch accepted", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return failedMessage != ""
	}) {
		t.Fatal("expected a failed report once runLaunch's download attempt failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(failedMessage, "failed to download workspace from GCS") {
		t.Fatalf("failed message = %q, want runLaunch's GCS download failure", failedMessage)
	}
}

// TestWorkspaceStorageBucket_Precedence pins the bucket a workspace upload
// is downloaded from: the hub's bucket on the request, else the broker's
// own setting, else none (ptone/scion#3422).
func TestWorkspaceStorageBucket_Precedence(t *testing.T) {
	cases := []struct {
		name, reqBucket, cfgBucket, want string
	}{
		{"request only", "hub-bucket", "", "hub-bucket"},
		{"request wins over broker setting", "hub-bucket", "broker-bucket", "hub-bucket"},
		{"broker setting fallback", "", "broker-bucket", "broker-bucket"},
		{"neither", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{config: ServerConfig{StorageBucket: tc.cfgBucket}}
			got := srv.workspaceStorageBucket(CreateAgentRequest{WorkspaceStorageBucket: tc.reqBucket})
			if got != tc.want {
				t.Fatalf("workspaceStorageBucket = %q, want %q", got, tc.want)
			}
		})
	}
}
