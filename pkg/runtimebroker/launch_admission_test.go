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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// asyncManager is a concurrency-safe agent.Manager fake for the async-create
// tests (B-1..B-6): runLaunch calls it from its own goroutine, concurrently
// with the test's assertions, so (unlike mockManager, built for the
// single-goroutine synchronous path) every field here is mutex-guarded.
type asyncManager struct {
	*mockManager // Provision/Reprovision/Stop/Delete/DeleteTarget/List/Message/MessageRaw/Watch/Close: unused by these tests

	mu           sync.Mutex
	preflightErr error
	startErr     error
	startCalls   int
	startBlock   chan struct{} // if non-nil, Start waits on it (or ctx) before returning
	cleanupCalls int
	cleanupLast  []agent.ResourceHandle
}

func newAsyncManager() *asyncManager {
	return &asyncManager{mockManager: &mockManager{}}
}

func (m *asyncManager) Preflight(ctx context.Context, opts api.StartOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.preflightErr
}

func (m *asyncManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.startCalls++
	block := m.startBlock
	startErr := m.startErr
	m.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
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
	defer m.mu.Unlock()
	m.cleanupCalls++
	m.cleanupLast = handles
	return nil
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
	// Deterministic, not a race: beginAsyncLaunch returns on the Preflight
	// error before registry.Begin or "go runLaunch" ever run.
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must not be called when Preflight fails, got %d calls", n)
	}
	if len(rtb.getLaunchReports()) != 0 {
		t.Fatalf("no launch report should be sent when admission itself refused the request")
	}
}

// --- B-6: with asyncLaunch absent, behavior is byte-identical; ProvisionOnly
// plus async stays synchronous. ---

func TestAsyncCreate_FlagAbsent_StaysSynchronous(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)

	w := postCreate(t, srv, map[string]any{
		"name": "agent-4", "config": map[string]any{"template": "claude"},
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

	w := postCreate(t, srv, map[string]any{
		"name": "agent-7", "asyncLaunch": true, "launchId": "L-7",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !waitUntil(t, 2*time.Second, func() bool { return mgr.CleanupCallCount() >= 1 }) {
		t.Fatal("expected CleanupLaunch to be called after a failed report answered applied")
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

// --- B-3: a claim blocked by an unreachable Hub retries until ctx' expires,
// then sends failed{hub_unreachable}. ---

func TestAsyncCreate_ClaimUnreachableUntilDeadline_SendsHubUnreachable(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)

	var mu sync.Mutex
	var terminalCode string
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if claimState(req) {
			return nil, errors.New("simulated unreachable")
		}
		mu.Lock()
		terminalCode = req.ErrorCode
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
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called when the claim never got through, got %d calls", n)
	}
}
