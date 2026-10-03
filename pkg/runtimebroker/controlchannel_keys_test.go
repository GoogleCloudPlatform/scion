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
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

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
		cancels:     make(map[string]context.CancelFunc),
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

// TestControlChannel_Keys_CancelWhileQueued covers the ruling that
// a Hub cancel sent while a keys request is still waiting on
// the saturated dispatch semaphore is lost, because dispatchRequest only
// calls registerCancel *after* it acquires the semaphore
// (controlchannel.go dispatchRequest). Both halves of the ruling are pinned
// as subtests:
//
//   - ReleasedBeforeExpiry_MayExecuteOnce: the slot frees while execute_before
//     is still open. The earlier (lost) cancel has no effect; the broker
//     executes the request exactly once. No resend/replay occurs.
//   - ReleasedAfterExpiry_MustNotInject: the slot frees after execute_before
//     has passed. Expiry is enforced at admission (handler entry) regardless
//     of the cancel, so the request is rejected with 503 keys_unavailable and
//     Manager.SendKeys is never called.
//
// Per the ruling, this does not include an intentionally failing
// future-behaviour test for earlier cancel registration — that is tracked as
// a separate follow-up, out of scope here.
func TestControlChannel_Keys_CancelWhileQueued(t *testing.T) {
	cases := []struct {
		name            string
		executeBefore   func(start time.Time) time.Time
		releaseDelay    time.Duration
		wantSendKeys    int32
		wantStatus      int
		wantOutcome     agentkeys.Outcome
		checkOutcomeDoc bool
	}{
		{
			// The ruling says the
			// request "may execute once" here -- it does not require
			// exactly one. wantSendKeys: 1 pins more than that: today's
			// implementation is single-attempt with no retry, so under
			// this case's deterministic timing (one slot, one queued
			// request, release before expiry) it always executes exactly
			// once, never zero. If a future early-cancel improvement
			// (tracked separately, see the ruling) changes queued-cancel
			// handling, this exact count may need to become "at most 1"
			// instead -- it is not a frozen contract guarantee the way the
			// after-expiry case's "must not inject" is.
			name:          "ReleasedBeforeExpiry_MayExecuteOnce",
			executeBefore: func(start time.Time) time.Time { return start.Add(10 * time.Second) },
			releaseDelay:  150 * time.Millisecond,
			wantSendKeys:  1,
		},
		{
			name:            "ReleasedAfterExpiry_MustNotInject",
			executeBefore:   func(start time.Time) time.Time { return start.Add(200 * time.Millisecond) },
			releaseDelay:    400 * time.Millisecond,
			wantSendKeys:    0,
			wantStatus:      503,
			wantOutcome:     agentkeys.OutcomeKeysUnavailable,
			checkOutcomeDoc: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			req := keysRequestEnvelope(t, "keys-req-cancel-"+tc.name, "test-agent", "proj-1", agentkeys.BrokerRequest{
				ProjectID:     "proj-1",
				AgentID:       "agent-abc",
				OperationID:   "op-1",
				ExecuteBefore: tc.executeBefore(start),
				Keys:          "C-c",
			})

			client.wg.Add(1)
			go client.dispatchRequest(brokerConn, req)

			// Give the goroutine time to reach (and block on) the semaphore
			// before the cancel arrives — registerCancel has not run yet, so
			// the Hub's cancel message finds nothing in client.cancels for
			// this RequestID, and the cancel is silently lost.
			time.Sleep(50 * time.Millisecond)

			cancelMsg, err := json.Marshal(wsprotocol.NewCancelMessage(req.RequestID))
			if err != nil {
				t.Fatalf("marshal cancel message: %v", err)
			}
			if err := client.handleMessage(cancelMsg); err != nil {
				t.Fatalf("handleMessage(cancel) returned error: %v", err)
			}

			time.Sleep(tc.releaseDelay)
			<-client.dispatchSem

			client.wg.Wait()

			var resp wsprotocol.ResponseEnvelope
			if err := hubConn.ReadJSON(&resp); err != nil {
				t.Fatalf("reading response envelope: %v", err)
			}

			if got := sendKeysCalls.Load(); got > 1 {
				t.Fatalf("Manager.SendKeys called %d times, want at most 1 (no replay)", got)
			}
			if got := sendKeysCalls.Load(); got != tc.wantSendKeys {
				t.Errorf("Manager.SendKeys called %d times, want %d", got, tc.wantSendKeys)
			}

			if tc.checkOutcomeDoc {
				if resp.StatusCode != tc.wantStatus {
					t.Errorf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantStatus, resp.Body)
				}
				var result agentkeys.BrokerResult
				if err := json.Unmarshal(resp.Body, &result); err != nil {
					t.Fatalf("decoding BrokerResult: %v; body = %s", err, resp.Body)
				}
				if result.Outcome != tc.wantOutcome {
					t.Errorf("Outcome = %q, want %q", result.Outcome, tc.wantOutcome)
				}
			}
		})
	}
}
