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
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// keysRequestEnvelope builds a wsprotocol.RequestEnvelope that tunnels a
// POST /api/v1/agents/{slug}/keys call, the same shape the Hub's control
// channel adapter sends (pkg/hub/controlchannel_client.go).
func keysRequestEnvelope(t *testing.T, requestID, slug, projectID string, body agentkeys.BrokerRequest) wsprotocol.RequestEnvelope {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal keys request body: %v", err)
	}
	return wsprotocol.RequestEnvelope{
		Type:      "request",
		RequestID: requestID,
		Method:    "POST",
		Path:      "/api/v1/agents/" + slug + "/keys",
		Query:     "projectId=" + projectID,
		Body:      b,
	}
}

// newSaturatedKeysClient builds a ControlChannelClient wired to srv, with
// every dispatch slot pre-filled (saturated), for the queued-keys
// tests below. Filling the semaphore directly (rather than occupying it with
// real blocking handlers) isolates "queued behind a full semaphore" from the
// mechanics of any particular occupant, matching
// TestDispatchRequest_ContextCancelledBeforeSemaphore's pattern.
func newSaturatedKeysClient(t *testing.T, srv *Server) (client *ControlChannelClient, hubConn *wsprotocol.Connection) {
	t.Helper()
	brokerConn, hubConn, cleanup := newWSPair(t)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client = &ControlChannelClient{
		config:      ControlChannelConfig{},
		conn:        brokerConn,
		handlers:    srv.Handler(),
		log:         slog.Default(),
		streams:     make(map[string]*StreamHandler),
		dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
		cancels:     make(map[string]*requestCancel),
		ctx:         ctx,
		cancel:      cancel,
	}
	for i := 0; i < defaultMaxConcurrentDispatches; i++ {
		client.dispatchSem <- struct{}{}
	}
	return client, hubConn
}

// TestControlChannel_Keys_SaturatedSemaphore_ExpiresBeforeAdmission covers:
// a keys request tunneled while the dispatch semaphore is
// saturated queues behind it (dispatchRequest's semaphore wait selects only
// on ctx.Done(), not on the request's own execute_before deadline). By the
// time a slot frees and the handler actually runs, admittedAt (computed at
// handler entry — handlers.go sendKeys) is already past execute_before, so
// the request must be rejected at admission with 503 keys_unavailable and
// Manager.SendKeys must never be called.
func TestControlChannel_Keys_SaturatedSemaphore_ExpiresBeforeAdmission(t *testing.T) {
	var sendKeysCalls atomic.Int32
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			sendKeysCalls.Add(1)
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)
	client, hubConn := newSaturatedKeysClient(t, srv)
	brokerConn := client.conn

	start := time.Now().UTC()
	req := keysRequestEnvelope(t, "keys-req-1", "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: start.Add(200 * time.Millisecond),
		Keys:          "C-c",
	})

	client.wg.Add(1)
	go client.dispatchRequest(brokerConn, req)

	// Give the goroutine time to reach (and block on) the semaphore.
	time.Sleep(100 * time.Millisecond)

	// Free one slot only after execute_before has already passed, so the
	// handler's own admittedAt (computed when it finally runs) is past the
	// deadline.
	time.Sleep(250 * time.Millisecond)
	<-client.dispatchSem

	client.wg.Wait()

	var resp wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("reading response envelope: %v", err)
	}

	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503; body = %s", resp.StatusCode, resp.Body)
	}
	var result agentkeys.BrokerResult
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		t.Fatalf("decoding BrokerResult: %v; body = %s", err, resp.Body)
	}
	if result.Outcome != agentkeys.OutcomeKeysUnavailable {
		t.Errorf("Outcome = %q, want %q", result.Outcome, agentkeys.OutcomeKeysUnavailable)
	}
	if got := sendKeysCalls.Load(); got != 0 {
		t.Errorf("Manager.SendKeys called %d times, want 0 (admission must reject before dispatch once the deadline has passed while queued)", got)
	}
}

// handleRaw feeds one raw frame through the client's real message handler,
// the same entry point its read loop uses.
func handleRaw(t *testing.T, client *ControlChannelClient, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := client.handleMessage(data); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
}

// trackedCancels returns how many requests currently have a registered
// cancel.
func trackedCancels(client *ControlChannelClient) int {
	client.cancelMu.Lock()
	defer client.cancelMu.Unlock()
	return len(client.cancels)
}

// waitGroupDone waits for client.wg with a timeout, so a regression that
// leaves a dispatch goroutine blocked fails the test instead of hanging it.
func waitGroupDone(t *testing.T, client *ControlChannelClient, timeout time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		client.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// expectNoFrame asserts nothing arrives on hubConn within window.
func expectNoFrame(t *testing.T, hubConn *wsprotocol.Connection, window time.Duration) {
	t.Helper()
	_ = hubConn.SetReadDeadline(time.Now().Add(window))
	var extra wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&extra); err == nil {
		t.Fatalf("unexpected frame sent to the Hub: %+v (body %s)", extra, extra.Body)
	}
}

// TestControlChannel_Keys_CancelWhileQueued_LeavesQueueWithoutInjecting
// covers ptone/scion#2877's cancel-before-dequeue case: a keys request
// tunneled while every dispatch slot is busy, then cancelled by the Hub,
// must leave the queue at once — without waiting for a slot and without
// ever reaching Manager.SendKeys — and must not send a response (the Hub
// has already given up on this RequestID and decides its own outcome). The
// request and the cancel go through handleMessage, in order, exactly as the
// read loop delivers them, and the cancel arrives well before
// execute_before, so only the cancel can stop the request.
func TestControlChannel_Keys_CancelWhileQueued_LeavesQueueWithoutInjecting(t *testing.T) {
	var sendKeysCalls atomic.Int32
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			sendKeysCalls.Add(1)
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)
	client, hubConn := newSaturatedKeysClient(t, srv)

	req := keysRequestEnvelope(t, "keys-req-queued-cancel", "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "C-c",
	})

	handleRaw(t, client, req)
	// Registration happens synchronously on the read loop, before the
	// dispatch goroutine is even scheduled.
	if got := trackedCancels(client); got != 1 {
		t.Fatalf("tracked cancels after request = %d, want 1 (cancel must be registered before queueing)", got)
	}
	// Let the dispatch goroutine reach the saturated semaphore.
	time.Sleep(50 * time.Millisecond)

	handleRaw(t, client, wsprotocol.NewCancelMessage(req.RequestID))

	// No slot is ever freed: the request must leave the queue on the cancel
	// alone.
	if !waitGroupDone(t, client, 2*time.Second) {
		t.Fatal("queued request did not leave the dispatch queue after the Hub's cancel")
	}

	if got := sendKeysCalls.Load(); got != 0 {
		t.Errorf("Manager.SendKeys called %d times, want 0 (cancelled before dequeue)", got)
	}
	if got := len(client.dispatchSem); got != defaultMaxConcurrentDispatches {
		t.Errorf("dispatch slots in use = %d, want %d (a cancelled queued request must not take or free a slot)", got, defaultMaxConcurrentDispatches)
	}
	if got := trackedCancels(client); got != 0 {
		t.Errorf("tracked cancels after queued cancel = %d, want 0", got)
	}

	// Freeing slots afterwards must not resurrect the request.
	for i := 0; i < defaultMaxConcurrentDispatches; i++ {
		<-client.dispatchSem
	}
	expectNoFrame(t, hubConn, 200*time.Millisecond)
	if got := sendKeysCalls.Load(); got != 0 {
		t.Errorf("Manager.SendKeys called %d times after slots freed, want 0", got)
	}
}

// TestControlChannel_Keys_QueuedWithoutCancel_ExecutesOnce pins the other
// side of the queue: a queued request the Hub does not cancel still runs
// exactly once when a slot frees before execute_before (single attempt, no
// replay), and its cancel registration is removed afterwards.
func TestControlChannel_Keys_QueuedWithoutCancel_ExecutesOnce(t *testing.T) {
	var sendKeysCalls atomic.Int32
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			sendKeysCalls.Add(1)
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)
	client, hubConn := newSaturatedKeysClient(t, srv)

	req := keysRequestEnvelope(t, "keys-req-queued-run", "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "C-c",
	})
	handleRaw(t, client, req)
	time.Sleep(50 * time.Millisecond)
	<-client.dispatchSem

	if !waitGroupDone(t, client, 2*time.Second) {
		t.Fatal("queued request did not complete after a slot was freed")
	}

	var resp wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("reading response envelope: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, resp.Body)
	}
	if got := sendKeysCalls.Load(); got != 1 {
		t.Errorf("Manager.SendKeys called %d times, want exactly 1", got)
	}
	if got := trackedCancels(client); got != 0 {
		t.Errorf("tracked cancels after completion = %d, want 0", got)
	}
}

// TestControlChannel_Keys_CancelDuringTargetLockWait covers a Hub cancel
// that arrives after the request got a dispatch slot, while SendKeys is
// still waiting for the target's injection lock: the cancel reaches
// SendKeys's ctx, SendKeys reports ErrKeysNotStarted (as sendKeysCore does
// when lock.Lock(ctx) fails — see pkg/agent's lock-wait tests), and the
// handler answers keys_unavailable with nothing injected.
func TestControlChannel_Keys_CancelDuringTargetLockWait(t *testing.T) {
	waiting := make(chan struct{})
	var injected atomic.Bool
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			close(waiting)
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", agent.ErrKeysNotStarted, ctx.Err())
			case <-time.After(5 * time.Second):
				injected.Store(true)
				return nil
			}
		},
	}
	srv := newTestServerWithManager(t, mgr)
	client, hubConn := newSaturatedKeysClient(t, srv)
	<-client.dispatchSem // one free slot

	req := keysRequestEnvelope(t, "keys-req-lock-cancel", "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "C-c",
	})
	handleRaw(t, client, req)
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("SendKeys was never reached")
	}
	handleRaw(t, client, wsprotocol.NewCancelMessage(req.RequestID))

	if !waitGroupDone(t, client, 2*time.Second) {
		t.Fatal("request did not finish after the Hub's cancel")
	}
	if injected.Load() {
		t.Fatal("keys were injected despite the cancel during the lock wait")
	}

	var resp wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("reading response envelope: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503; body = %s", resp.StatusCode, resp.Body)
	}
	var result agentkeys.BrokerResult
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		t.Fatalf("decoding BrokerResult: %v; body = %s", err, resp.Body)
	}
	if result.Outcome != agentkeys.OutcomeKeysUnavailable {
		t.Errorf("Outcome = %q, want %q", result.Outcome, agentkeys.OutcomeKeysUnavailable)
	}
	if got := trackedCancels(client); got != 0 {
		t.Errorf("tracked cancels after completion = %d, want 0", got)
	}
}

// TestControlChannel_Keys_CancelAfterInjectionStarted_NotProvenNotExecuted
// covers ptone/scion#2877's cancel-after-injection-started case: once the
// delivery call may have begun, a cancel must never be turned into a proven
// non-execution. SendKeys returns a plain error (as sendKeysCore does after
// its delivery call starts), so the broker answers with the ordinary error
// envelope, not a BrokerResult. The Hub's adapter classifies that shape as
// keys_outcome_unknown (pkg/hub decodeBrokerKeysResponse), and the Hub has
// already reported unknown for its own cancel in any case.
func TestControlChannel_Keys_CancelAfterInjectionStarted_NotProvenNotExecuted(t *testing.T) {
	deliveryStarted := make(chan struct{})
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			close(deliveryStarted)
			<-ctx.Done()
			return errors.New("failed to send keys to agent 'test-agent': delivery failed (context_canceled)")
		},
	}
	srv := newTestServerWithManager(t, mgr)
	client, hubConn := newSaturatedKeysClient(t, srv)
	<-client.dispatchSem

	req := keysRequestEnvelope(t, "keys-req-inject-cancel", "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "C-c",
	})
	handleRaw(t, client, req)
	select {
	case <-deliveryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery was never reached")
	}
	handleRaw(t, client, wsprotocol.NewCancelMessage(req.RequestID))

	if !waitGroupDone(t, client, 2*time.Second) {
		t.Fatal("request did not finish after the Hub's cancel")
	}

	var resp wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("reading response envelope: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500 (ordinary error envelope); body = %s", resp.StatusCode, resp.Body)
	}
	var result agentkeys.BrokerResult
	if err := json.Unmarshal(resp.Body, &result); err == nil && agentkeys.ValidBrokerOutcome(result.Outcome) {
		t.Fatalf("broker asserted outcome %q after delivery may have started; must not claim a decided outcome", result.Outcome)
	}
	if got := trackedCancels(client); got != 0 {
		t.Errorf("tracked cancels after completion = %d, want 0", got)
	}
}
