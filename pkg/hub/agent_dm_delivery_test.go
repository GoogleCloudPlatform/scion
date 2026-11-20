//go:build !no_sqlite

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

package hub

// ---------------------------------------------------------------------------
// Agent DM Delivery Lifecycle Tests (#1689)
//
// These tests verify truthful broker/managed-runtime delivery outcomes
// through the ExecuteAgentDM operation. Each test corresponds to one or
// more acceptance criteria from issue #1689.
//
// AC-1: Report dispatched only after broker/managed-runtime acceptance.
// AC-2: Definite failures return non-2xx and persist failed state.
// AC-3: CreateMessage failure prevents dispatch; final-state storage
//        failure is reported as ambiguous with the stable message ID.
// AC-4: Process interruption leaves inspectable pending rows; no blind
//        retry guidance for ambiguous completion.
// AC-5: Existing DEF-171 test replaced; no duplicate dispatch.
// ---------------------------------------------------------------------------

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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spyStateDispatcher is a test dispatcher that invokes a callback before
// returning, allowing tests to inspect mid-flight state or simulate custom
// dispatch outcomes.
type spyStateDispatcher struct {
	store      store.Store
	onDispatch func(msgText string)
	dispatchFn func(ctx context.Context, agent *store.Agent, msgText string, sm *messages.StructuredMessage) error
}

func (d *spyStateDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, _ bool, sm *messages.StructuredMessage) error {
	if d.onDispatch != nil {
		d.onDispatch(message)
	}
	if d.dispatchFn != nil {
		return d.dispatchFn(ctx, agent, message, sm)
	}
	return nil
}

func (d *spyStateDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *spyStateDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *spyStateDispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent, _ ReprovisionOptions) error {
	return nil
}
func (d *spyStateDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	return nil
}
func (d *spyStateDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }
func (d *spyStateDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *spyStateDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *spyStateDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *spyStateDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *spyStateDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *spyStateDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *spyStateDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *spyStateDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// AC-1: Report dispatched only after broker acceptance
// ---------------------------------------------------------------------------

func TestDelivery_DispatchSuccess_MarkedDispatched(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "dispatch-success"))
	require.Nil(t, dmErr, "dispatch success must not return an error")
	assert.Equal(t, AgentDMAccepted, result.Outcome,
		"successful dispatch must have accepted outcome")
	assert.NotEmpty(t, result.MessageID, "result must have a message ID")
	assert.Nil(t, result.DispatchErr, "DispatchErr must be nil on success")

	// Verify the persisted message has dispatch_state="dispatched".
	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState,
		"message dispatch_state must be 'dispatched' after successful dispatch")
	assert.NotNil(t, msg.DispatchedAt,
		"DispatchedAt must be set after successful dispatch")
}

func TestDelivery_DispatchedStructuredMessage_HasVersionAndTimestamp(t *testing.T) {
	// The StructuredMessage built inside ExecuteAgentDM (independent of any
	// client-supplied structured_message) must carry the current wire
	// Version and a non-empty, parseable RFC3339 Timestamp — matching the
	// web-chat, broker-inbound routing and messages.NewMention/NewSystemMessage
	// construction sites.
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	before := time.Now().Truncate(time.Second)
	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "version-timestamp-check"))
	require.Nil(t, dmErr, "dispatch must succeed")
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "expected exactly one dispatch call")
	structuredMsg := calls[0].StructuredMessage
	require.NotNil(t, structuredMsg, "dispatch call must carry a structured message")

	assert.Equal(t, messages.Version, structuredMsg.Version,
		"dispatched StructuredMessage must carry the current wire Version")

	require.NotEmpty(t, structuredMsg.Timestamp,
		"dispatched StructuredMessage must have a non-empty Timestamp")
	parsed, err := time.Parse(time.RFC3339, structuredMsg.Timestamp)
	require.NoError(t, err, "Timestamp must be RFC3339-parseable")
	now := time.Now()
	assert.False(t, parsed.Before(before), "Timestamp must not predate the dispatch call: got %s, before %s", parsed, before)
	assert.False(t, parsed.After(now), "Timestamp must not be in the future: got %s, now %s", parsed, now)

	persisted, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err, "persisted message must be retrievable")
	assert.True(t, parsed.UTC().Truncate(time.Second).Equal(persisted.CreatedAt.UTC().Truncate(time.Second)),
		"dispatched Timestamp must equal the persisted row's CreatedAt at second granularity: got %s, want %s",
		parsed, persisted.CreatedAt)
}

func TestDelivery_DispatchSuccess_PendingBeforeDispatch(t *testing.T) {
	// Verify that the message is in "pending" state when the dispatcher is
	// called, and transitions to "dispatched" after dispatch completes.
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	// Use a spy dispatcher that checks the message state mid-flight.
	var midFlightState string
	spy := &spyStateDispatcher{
		store: s,
		onDispatch: func(msgText string) {
			// Look up the message by content to find its dispatch state
			// at the moment the dispatcher is invoked.
			msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 50})
			if err != nil {
				return
			}
			for _, m := range msgs.Items {
				if m.Msg == msgText {
					midFlightState = m.DispatchState
					break
				}
			}
		},
	}
	srv.SetDispatcher(spy)

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "pending-check"))
	require.Nil(t, dmErr)

	// Mid-flight state must have been "pending".
	assert.Equal(t, store.MessageDispatchPending, midFlightState,
		"message must be in 'pending' state when dispatcher is called")

	// Final state must be "dispatched".
	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState,
		"final state must be 'dispatched'")
}

// ---------------------------------------------------------------------------
// AC-1: HTTP response says "dispatched" not "sent"
// ---------------------------------------------------------------------------

func TestDelivery_WriteAgentDMResult_Dispatched(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAgentDMResult(w, &AgentDMResult{
		Outcome:     AgentDMAccepted,
		MessageID:   "test-msg-id",
		Recipient:   "agent:test-agent",
		RecipientID: "test-agent-id",
	}, nil)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "dispatched", resp["status"],
		"status must be 'dispatched' not 'sent' (#1689)")
	assert.Equal(t, "test-msg-id", resp["message_id"])
}

// ---------------------------------------------------------------------------
// AC-2: Definite dispatch failure returns non-2xx and persists failed state
// ---------------------------------------------------------------------------

func TestDelivery_BrokerDispatchFailure_Returns502AndPersistsFailed(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	// Simulate definite dispatch failure (not context cancellation).
	dispatcher.returnErr = errors.New("broker rejected message")

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "dispatch-failure"))
	require.NotNil(t, dmErr, "definite dispatch failure must return an error")
	assert.Equal(t, ErrCodeDeliveryFailed, dmErr.Code)
	assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus,
		"definite dispatch failure must return 502")

	// Error must include message ID in details.
	require.NotNil(t, dmErr.Details)
	msgID, ok := dmErr.Details["message_id"].(string)
	require.True(t, ok, "details must contain message_id")
	assert.NotEmpty(t, msgID)

	// Verify the persisted message has dispatch_state="failed".
	msg, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState,
		"message dispatch_state must be 'failed' after definite dispatch failure")
	require.NotNil(t, msg.DispatchFailureReason)
	assert.Contains(t, *msg.DispatchFailureReason, "broker rejected",
		"failure reason must capture the dispatch error")
}

func TestDelivery_BrokerTimeout_DefiniteFailure(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	// Create a dispatcher that returns ErrBrokerTimeout (definite failure:
	// the broker was never reached within the deadline).
	errDispatcher := &recordingDispatcher{returnErr: ErrBrokerTimeout}
	srv.SetDispatcher(errDispatcher)

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "broker-timeout"))
	require.NotNil(t, dmErr, "ErrBrokerTimeout must return an error")
	assert.Equal(t, ErrCodeDeliveryFailed, dmErr.Code)
	assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus)

	// Message must be persisted as failed.
	msgID := dmErr.Details["message_id"].(string)
	msg, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState)
}

// ---------------------------------------------------------------------------
// AC-2: Missing dispatcher/broker returns explicit availability error
// ---------------------------------------------------------------------------

func TestDelivery_MissingDispatcher_ReturnsUnavailable(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	// Remove the dispatcher.
	srv.SetDispatcher(nil)

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "no-dispatcher"))
	require.NotNil(t, dmErr, "missing dispatcher must return an error")
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
}

func TestDelivery_MissingBrokerID_ReturnsError(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	// Target agent has no runtime broker ID.
	noBrokerTarget := *target
	noBrokerTarget.RuntimeBrokerID = ""

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, &noBrokerTarget, "no-broker"))
	require.NotNil(t, dmErr, "missing broker ID must return an error")
	assert.Equal(t, ErrCodeDeliveryFailed, dmErr.Code)
	assert.Equal(t, http.StatusUnprocessableEntity, dmErr.HTTPStatus)
}

func TestDelivery_MissingDispatcher_NoMessagePersisted(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	// Remove the dispatcher — pre-flight check should fail before persistence.
	srv.SetDispatcher(nil)

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "no-persist-check"))
	require.NotNil(t, dmErr)

	// Verify no message was persisted.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	for _, m := range msgs.Items {
		assert.NotEqual(t, "no-persist-check", m.Msg,
			"availability error must prevent persistence")
	}
}

// ---------------------------------------------------------------------------
// AC-3: CAS/store failure after dispatch returns ambiguous with message ID
// ---------------------------------------------------------------------------

func TestDelivery_WriteAgentDMResult_Ambiguous(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAgentDMResult(w, &AgentDMResult{
		Outcome:     AgentDMAmbiguous,
		MessageID:   "ambig-msg-id",
		Recipient:   "agent:test-agent",
		RecipientID: "test-agent-id",
		DispatchErr: errors.New("CAS failed"),
	}, nil)

	assert.Equal(t, http.StatusAccepted, w.Code,
		"ambiguous outcome must return 202 Accepted")
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "ambiguous", resp["status"],
		"ambiguous outcome must report status 'ambiguous'")
	assert.Equal(t, "ambig-msg-id", resp["message_id"],
		"ambiguous response must include the stable message ID")
}

// ---------------------------------------------------------------------------
// AC-4 / ptone/scion#5111: Unknown dispatch outcome → finalized failed,
// wire outcome stays ambiguous
// ---------------------------------------------------------------------------

func TestDelivery_ContextCancellation_AmbiguousOutcome(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)

	// A dispatcher that itself reports context.Canceled. The caller's
	// cancellation no longer reaches the dispatch (dispatchContext,
	// ptone/scion#4827; see TestAgentDM_CallerCancelMidDispatch_RowNotLeftPending),
	// and an unknown dispatch outcome is finalized as failed right away rather
	// than left pending for the 24h TTL (ptone/scion#5111).
	cancelDispatcher := &recordingDispatcher{returnErr: context.Canceled}
	srv.SetDispatcher(cancelDispatcher)

	ctx := context.Background()
	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "ctx-cancel"))
	require.Nil(t, dmErr, "context cancellation must not return an error (ambiguous)")
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAmbiguous, result.Outcome,
		"context cancellation must produce ambiguous outcome")
	assert.NotEmpty(t, result.MessageID,
		"ambiguous result must include message ID")

	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState,
		"unknown dispatch outcome must finalize row as failed (ptone/scion#5111)")
	require.NotNil(t, msg.DispatchFailureReason)
	assert.Equal(t, "dispatch outcome unknown", *msg.DispatchFailureReason)
}

func TestDelivery_DeadlineExceeded_AmbiguousOutcome(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)

	// Context deadline exceeded during dispatch has an unknown delivery
	// outcome: wire result stays ambiguous while the stored row is finalized
	// as failed straight away (ptone/scion#5111).
	deadlineDispatcher := &recordingDispatcher{returnErr: context.DeadlineExceeded}
	srv.SetDispatcher(deadlineDispatcher)

	ctx := context.Background()
	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "deadline"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAmbiguous, result.Outcome)

	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState,
		"deadline-ambiguous row must be finalized as failed (ptone/scion#5111)")
	require.NotNil(t, msg.DispatchFailureReason)
	assert.Equal(t, "dispatch outcome unknown", *msg.DispatchFailureReason)
}

func TestDelivery_DispatchTimeoutAfterStore_FinalizedFailedNotStuck(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)

	var midFlightState string
	srv.SetDispatcher(&spyStateDispatcher{
		store: s,
		dispatchFn: func(ctx context.Context, _ *store.Agent, msgText string, _ *messages.StructuredMessage) error {
			msgs, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
			if err == nil {
				for _, m := range msgs.Items {
					if m.Msg == msgText {
						midFlightState = m.DispatchState
						break
					}
				}
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})

	callCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, dmErr := srv.ExecuteAgentDM(callCtx, deliveryDMInput(sender, target, "timeout-after-store-5111"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAmbiguous, result.Outcome)
	assert.Equal(t, store.MessageDispatchPending, midFlightState,
		"precondition: message must be stored as pending while dispatch is in flight")

	ctx := context.Background()
	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState,
		"timed-out dispatch after persist must finalize row as failed")
	require.NotNil(t, msg.DispatchFailureReason)
	assert.Equal(t, "dispatch outcome unknown", *msg.DispatchFailureReason)

	// The pending-message check must no longer flag this message.
	stuckCount, err := s.CountStuckPendingMessages(ctx, time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, stuckCount, "pending-message check must not flag finalized unknown-outcome DM")
	stuckIDs, err := s.ListStuckPendingMessageIDs(ctx, time.Now().UTC().Add(time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, stuckIDs)
}

func TestDelivery_DispatchTimeoutAfterConduitDelivered_LeavesDeliveredUnaffected(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	// Case 1: conduit delivery finishes and marks the stored row "delivered"
	// just before the dispatch call itself times out. Finalization must leave
	// the delivered message unaffected.
	srv.SetDispatcher(&spyStateDispatcher{
		store: s,
		dispatchFn: func(dctx context.Context, _ *store.Agent, msgText string, _ *messages.StructuredMessage) error {
			msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			var msgID string
			for _, m := range msgs.Items {
				if m.Msg == msgText {
					msgID = m.ID
					break
				}
			}
			require.NotEmpty(t, msgID)
			_, err = s.BindMessageDelivery(ctx, target.ID, "d-5111", []string{msgID}, time.Now())
			require.NoError(t, err)
			n, err := s.FinishMessageDelivery(ctx, "d-5111", []string{msgID}, store.MessageDispatchDelivered, `{"outcome":"delivered"}`)
			require.NoError(t, err)
			require.Equal(t, 1, n)

			<-dctx.Done()
			return dctx.Err()
		},
	})

	callCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	deliveredRes, dmErr := srv.ExecuteAgentDM(callCtx, deliveryDMInput(sender, target, "delivered-before-timeout-5111"))
	require.Nil(t, dmErr)
	require.NotNil(t, deliveredRes)
	assert.Equal(t, AgentDMAmbiguous, deliveredRes.Outcome)

	deliveredMsg, err := s.GetMessage(ctx, deliveredRes.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDelivered, deliveredMsg.DispatchState,
		"a message already marked delivered must be unaffected by timeout finalization")
	assert.Nil(t, deliveredMsg.DispatchFailureReason)

	// Case 2: an ordinary dispatch that completes within the deadline is
	// unaffected as well.
	srv.SetDispatcher(&recordingDispatcher{})
	okRes, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "normal-ok-5111"))
	require.Nil(t, dmErr)
	require.NotNil(t, okRes)
	assert.Equal(t, AgentDMAccepted, okRes.Outcome)

	okMsg, err := s.GetMessage(ctx, okRes.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, okMsg.DispatchState)
	assert.Nil(t, okMsg.DispatchFailureReason)
}

func TestDelivery_AutoWakeMentionDispatchTimeout_FinalizedFailed(t *testing.T) {
	srv, s, project, sender, target, _, _ := deliverySetup(t)
	ctx := context.Background()

	srv.SetDispatcher(&recordingDispatcher{returnErr: context.DeadlineExceeded})

	storeMsg := &store.Message{
		ID:            tid("autowake-timeout-5111"),
		ProjectID:     project.ID,
		Sender:        "agent:" + sender.Slug,
		SenderID:      sender.ID,
		Recipient:     "agent:" + target.Slug,
		RecipientID:   target.ID,
		Msg:           "@delivery-target ping",
		Type:          messages.TypeMention,
		AgentID:       sender.ID,
		CreatedAt:     time.Now(),
		DispatchState: store.MessageDispatchPending,
	}
	require.NoError(t, s.CreateMessage(ctx, storeMsg))

	in := deliveryDMInput(sender, target, storeMsg.Msg)
	in.Type = messages.TypeMention
	in.SkipPhaseGate = true
	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      storeMsg.Sender,
		SenderID:    storeMsg.SenderID,
		Recipient:   storeMsg.Recipient,
		RecipientID: storeMsg.RecipientID,
		Msg:         storeMsg.Msg,
		Type:        storeMsg.Type,
	}

	item := srv.agentDMAutoWakeItem(in, storeMsg, sm, sm)
	item.deliver(ctx, target)

	got, err := s.GetMessage(ctx, storeMsg.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState,
		"post-auto-wake unknown dispatch outcome must finalize row as failed")
	require.NotNil(t, got.DispatchFailureReason)
	assert.Equal(t, "dispatch outcome unknown", *got.DispatchFailureReason)

	stuckCount, err := s.CountStuckPendingMessages(ctx, time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, stuckCount)
}

// ---------------------------------------------------------------------------
// AC-5: No duplicate dispatch — CAS prevents double dispatch
// ---------------------------------------------------------------------------

func TestDelivery_NoDuplicateDispatch_CASGuard(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	// First send — succeeds normally.
	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "no-dup-1"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, result.Outcome)

	// Verify exactly one dispatch call.
	assert.Len(t, dispatcher.getCalls(), 1, "first send must dispatch exactly once")

	// Verify the message is dispatched.
	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)

	// The CAS on MarkMessageDispatched would prevent a second transition
	// for the same message ID. Verify: calling MarkMessageDispatched again
	// returns dispatched=false (CAS miss).
	dispatched, casErr := s.MarkMessageDispatched(ctx, result.MessageID)
	require.NoError(t, casErr)
	assert.False(t, dispatched,
		"second MarkMessageDispatched must return false (CAS miss)")
}

// ---------------------------------------------------------------------------
// AC-3: CreateMessage failure prevents dispatch
// ---------------------------------------------------------------------------
//
// Coverage note: CreateMessage failure → zero dispatch calls is a structural
// invariant (dispatch is only reachable after CreateMessage returns nil).
// Verifying this with an injectable store mock requires a test-only Store
// wrapper, which is beyond the scope of this change. The code path is
// verified by inspection: the early return on CreateMessage error at step 8
// in ExecuteAgentDM prevents execution from reaching step 11 (dispatch).

// ---------------------------------------------------------------------------
// Dispatch failure via HTTP adapters (integration through the contract)
// ---------------------------------------------------------------------------

func TestDelivery_OutboundAdapter_DispatchFailure_Returns502(t *testing.T) {
	srv, _, project, sender, _, convID, dispatcher := deliverySetup(t)

	// Make the dispatcher return a definite error.
	dispatcher.returnErr = errors.New("broker unavailable")

	rr := sendAgentDM(t, srv, sender, convID, project.ID, "outbound-dispatch-fail")
	assert.Equal(t, http.StatusBadGateway, rr.Code,
		"outbound adapter must return 502 on dispatch failure; body: %s",
		rr.Body.String())

	// Verify the error response contains message_id.
	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	errObj, ok := errResp["error"].(map[string]interface{})
	require.True(t, ok, "response must contain error object")
	details, ok := errObj["details"].(map[string]interface{})
	require.True(t, ok, "error must contain details")
	assert.NotEmpty(t, details["message_id"],
		"error details must include message_id")
}

func TestDelivery_OutboundAdapter_DispatchSuccess_ReturnsDispatched(t *testing.T) {
	srv, _, project, sender, _, convID, _ := deliverySetup(t)

	rr := sendAgentDM(t, srv, sender, convID, project.ID, "outbound-dispatch-ok")
	require.Equal(t, http.StatusOK, rr.Code,
		"outbound adapter must return 200 on dispatch success; body: %s",
		rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "dispatched", resp["status"],
		"status must be 'dispatched' (#1689)")
}

func TestDelivery_StructuredAdapter_DispatchFailure_Returns502(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher := deliverySetup(t)

	dispatcher.returnErr = errors.New("broker unavailable")

	rr := sendViaStructured(t, srv, sender, target, "structured-dispatch-fail")
	assert.Equal(t, http.StatusBadGateway, rr.Code,
		"structured adapter must return 502 on dispatch failure; body: %s",
		rr.Body.String())
}

func TestDelivery_StructuredAdapter_DispatchSuccess_ReturnsDispatched(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)

	rr := sendViaStructured(t, srv, sender, target, "structured-dispatch-ok")
	require.Equal(t, http.StatusOK, rr.Code,
		"structured adapter must return 200 on dispatch success; body: %s",
		rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "dispatched", resp["status"],
		"status must be 'dispatched' (#1689)")
}

// ---------------------------------------------------------------------------
// Bounded finalization context
// ---------------------------------------------------------------------------

func TestDelivery_FinalizationContext_ActiveParent(t *testing.T) {
	ctx := context.Background()
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()

	// Active parent → derived context should not be cancelled.
	assert.NoError(t, finCtx.Err(), "finalization context from active parent must not be cancelled")

	// Should have a deadline.
	deadline, ok := finCtx.Deadline()
	assert.True(t, ok, "finalization context must have a deadline")
	assert.WithinDuration(t, time.Now().Add(finalizationTimeout), deadline, 2*time.Second,
		"deadline must be approximately finalizationTimeout from now")
}

func TestDelivery_FinalizationContext_CancelledParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel the parent.

	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()

	// Cancelled parent → fresh bounded context, must not be cancelled yet.
	assert.NoError(t, finCtx.Err(),
		"finalization context from cancelled parent must not be immediately cancelled")

	deadline, ok := finCtx.Deadline()
	assert.True(t, ok, "finalization context must have a deadline")
	assert.WithinDuration(t, time.Now().Add(finalizationTimeout), deadline, 2*time.Second)
}

// ---------------------------------------------------------------------------
// isAmbiguousDispatchError classification
// ---------------------------------------------------------------------------

func TestDelivery_IsAmbiguousDispatchError(t *testing.T) {
	assert.True(t, isAmbiguousDispatchError(context.Canceled),
		"context.Canceled must be ambiguous")
	assert.True(t, isAmbiguousDispatchError(context.DeadlineExceeded),
		"context.DeadlineExceeded must be ambiguous")
	assert.True(t, isAmbiguousDispatchError(errors.Join(errors.New("wrap"), context.Canceled)),
		"wrapped context.Canceled must be ambiguous")
	assert.False(t, isAmbiguousDispatchError(errors.New("definite failure")),
		"ordinary error must not be ambiguous")
	assert.False(t, isAmbiguousDispatchError(ErrBrokerTimeout),
		"ErrBrokerTimeout must not be ambiguous (broker never reached)")
}

// ---------------------------------------------------------------------------
// WriteAgentDMError includes message_id in details (dispatch failure path)
// ---------------------------------------------------------------------------

func TestDelivery_WriteAgentDMError_DispatchFailedIncludesMessageID(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAgentDMError(w, dispatchFailedError("test-msg-123"))

	assert.Equal(t, http.StatusBadGateway, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, ErrCodeDeliveryFailed, errObj["code"])
	details := errObj["details"].(map[string]interface{})
	assert.Equal(t, "test-msg-123", details["message_id"])
}

// ---------------------------------------------------------------------------
// Shutdown admission pre-check and post-persist race (ptone/scion#7405)
// ---------------------------------------------------------------------------

func TestDelivery_ShuttingDown_RefusesBeforePersistenceWith503RetryAfter(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	srv.BeginShutdown()

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "shutting-down-broker"))
	require.NotNil(t, dmErr, "shutting-down hub must refuse ExecuteAgentDM before persistence")
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Equal(t, time.Duration(shutdownRetryAfterSeconds)*time.Second, dmErr.RetryAfter)

	w := httptest.NewRecorder()
	WriteAgentDMError(w, dmErr)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "10", w.Header().Get("Retry-After"))

	// Managed runtime targets must also be refused before persistence during shutdown.
	managedTarget := *target
	managedTarget.Runtime = "gemini-interactions"
	_, managedErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, &managedTarget, "shutting-down-managed"))
	require.NotNil(t, managedErr)
	assert.Equal(t, http.StatusServiceUnavailable, managedErr.HTTPStatus)

	assert.Empty(t, dispatcher.getCalls(), "must not dispatch during shutdown")
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "no message row may be persisted when hub is already shutting down")
}

// TestDelivery_DelegatedSendShuttingDown_RefusesBeforePersistence covers the
// delegated-send ingress path (handleAgentMessage -> delegatedDeliveryRefusal ->
// checkDispatchAvailability, review O2 / mutant m1), which does not go through
// ExecuteAgentDM's Phase 2 re-check.
func TestDelivery_DelegatedSendShuttingDown_RefusesBeforePersistence(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	srv.BeginShutdown()

	bodyBytes, err := json.Marshal(map[string]any{"message": "delegated-send-during-shutdown"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	delegated := &DelegatedAgentIdentity{
		agentID:           sender.ID,
		agentProjectID:    sender.ProjectID,
		authorizingUserID: target.Ancestry[0],
		grantID:           tid("delegated-shutdown-grant"),
	}
	req = req.WithContext(contextWithIdentity(req.Context(), delegated))
	rec := httptest.NewRecorder()

	srv.handleAgentMessage(rec, req, target.ID)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), ErrCodeUnavailable)
	assert.Empty(t, dispatcher.getCalls(), "delegated send during shutdown must not dispatch")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "delegated send during shutdown must not persist a message row")
}

func TestDelivery_ShuttingDownDuringWake_RefusesBeforePersistence(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	// Grant owner role so sender is permitted to wake target, and suspend target.
	target.Phase = "suspended"
	require.NoError(t, s.UpdateAgent(ctx, target))
	ownerID := target.Ancestry[0]
	userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(ownerID, "delivery-owner@test.example", "Owner", "member", "cli"))
	in := deliveryDMInput(sender, target, "shutting-down-after-wake")
	in.SenderIdentity = GetIdentityFromContext(userCtx)
	in.Wake = true

	// Simulate BeginShutdown firing after checkDispatchAvailability, while
	// Phase 1b wake completes (spy marks target running so wakeAgentForDM returns).
	wakeDispatcher := &spyStateDispatcher{store: s}
	srv.SetDispatcher(wakeDispatcher)
	in.BeforeWake = func() {
		target.Phase = "running"
		target.Activity = "idle"
		_ = s.UpdateAgent(ctx, target)
		srv.BeginShutdown()
	}

	_, dmErr := srv.ExecuteAgentDM(ctx, in)
	require.NotNil(t, dmErr, "must refuse before CreateMessage if shutdown began during Phase 1b")
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Equal(t, time.Duration(shutdownRetryAfterSeconds)*time.Second, dmErr.RetryAfter)

	assert.Empty(t, dispatcher.getCalls())
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "no message row may be persisted if shutdown began before CreateMessage")
}

func TestDelivery_ShuttingDownPostPersistRace_MarksFailedAndReturns503RetryAfter(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	// Simulate BeginShutdown firing in the narrow window after CreateMessage
	// and before the dispatcher's shutdownGate check.
	dispatcher.returnErr = ErrHubShuttingDown

	_, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "shutting-down-race"))
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Equal(t, time.Duration(shutdownRetryAfterSeconds)*time.Second, dmErr.RetryAfter)

	require.NotNil(t, dmErr.Details)
	msgID, ok := dmErr.Details["message_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, msgID)

	w := httptest.NewRecorder()
	WriteAgentDMError(w, dmErr)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "10", w.Header().Get("Retry-After"))

	// The persisted row must be marked failed (not left pending).
	msg, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState,
		"post-persist shutdown race must mark row failed rather than leave it pending")
	require.NotNil(t, msg.DispatchFailureReason)
	assert.Contains(t, *msg.DispatchFailureReason, "shutting down")
}

// ---------------------------------------------------------------------------
// Bounded finalization retries without duplicate broker dispatch (ptone/scion#7405)
// ---------------------------------------------------------------------------

type markFinalizationFaultStore struct {
	store.Store
	fault *storeFaultSwitch

	mu                  sync.Mutex
	dispatchedErrs      []error
	failedErrs          []error
	forceCASMiss        bool
	dispatchedAttempts  int
	failedAttempts      int
	dispatchedDeadlines []time.Time
	failedDeadlines     []time.Time
	sawExpiredCtx       bool
}

func (w *markFinalizationFaultStore) MarkMessageDispatched(ctx context.Context, id string) (bool, error) {
	if !w.fault.Active() {
		return w.Store.MarkMessageDispatched(ctx, id)
	}
	w.mu.Lock()
	w.dispatchedAttempts++
	if ctx.Err() != nil {
		w.sawExpiredCtx = true
	}
	if dl, ok := ctx.Deadline(); ok {
		w.dispatchedDeadlines = append(w.dispatchedDeadlines, dl)
	}
	if len(w.dispatchedErrs) > 0 {
		err := w.dispatchedErrs[0]
		w.dispatchedErrs = w.dispatchedErrs[1:]
		w.mu.Unlock()
		return false, err
	}
	casMiss := w.forceCASMiss
	w.mu.Unlock()
	if casMiss {
		return false, nil
	}
	return w.Store.MarkMessageDispatched(ctx, id)
}

func (w *markFinalizationFaultStore) MarkMessageFailed(ctx context.Context, id string, reason string) error {
	if !w.fault.Active() {
		return w.Store.MarkMessageFailed(ctx, id, reason)
	}
	w.mu.Lock()
	w.failedAttempts++
	if ctx.Err() != nil {
		w.sawExpiredCtx = true
	}
	if dl, ok := ctx.Deadline(); ok {
		w.failedDeadlines = append(w.failedDeadlines, dl)
	}
	if len(w.failedErrs) > 0 {
		err := w.failedErrs[0]
		w.failedErrs = w.failedErrs[1:]
		w.mu.Unlock()
		return err
	}
	w.mu.Unlock()
	return w.Store.MarkMessageFailed(ctx, id, reason)
}

func deliverySetupWithFinalizationFault(t *testing.T) (
	*Server, store.Store, *markFinalizationFaultStore, *storeFaultSwitch,
	*store.Agent, *store.Agent, *recordingDispatcher,
) {
	t.Helper()
	srv, s, faultStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *markFinalizationFaultStore {
		return &markFinalizationFaultStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("fin-retry-owner"),
		Email:   "fin-retry-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("fin-retry-project"),
		Name:      "fin-retry-project",
		Slug:      "fin-retry-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	ensureStandingRoot(t, s, project.ID, owner.ID)

	brokerID := tid("fin-retry-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "fin-retry-broker",
		Slug:   "fin-retry-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender := &store.Agent{
		ID:              tid("fin-retry-sender"),
		Name:            "fin-retry-sender",
		Slug:            "fin-retry-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target := &store.Agent{
		ID:              tid("fin-retry-target"),
		Name:            "fin-retry-target",
		Slug:            "fin-retry-target",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	return srv, s, faultStore, fault, sender, target, dispatcher
}

func TestDelivery_MarkDispatchedTransientErrors_RetriesWithoutDuplicateBrokerDispatch(t *testing.T) {
	srv, s, faultStore, fault, sender, target, _ := deliverySetupWithFinalizationFault(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel caller ctx mid-dispatch and inject two transient deadline errors on
	// MarkMessageDispatched before allowing the third attempt to succeed.
	d := &cancelCallerDispatcher{recordingDispatcher: &recordingDispatcher{}, cancel: cancel}
	srv.SetDispatcher(d)

	faultStore.dispatchedErrs = []error{context.DeadlineExceeded, context.DeadlineExceeded}
	fault.Arm()

	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "retry-dispatched-mark"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAccepted, result.Outcome,
		"transient MarkMessageDispatched errors must be recovered by bounded retry")

	// Broker dispatch MUST have run once — never retried when only the DB mark failed.
	assert.Len(t, d.getCalls(), 1, "broker dispatch must never be retried when MarkMessageDispatched retries")

	faultStore.mu.Lock()
	attempts := faultStore.dispatchedAttempts
	deadlines := append([]time.Time(nil), faultStore.dispatchedDeadlines...)
	sawExpired := faultStore.sawExpiredCtx
	faultStore.mu.Unlock()

	assert.Equal(t, 3, attempts, "MarkMessageDispatched must retry twice and succeed on attempt 3")
	assert.False(t, sawExpired, "every MarkMessageDispatched attempt must get a fresh live detached context")
	require.Len(t, deadlines, 3, "each attempt must carry its own finalization deadline")
	assert.True(t, deadlines[2].After(deadlines[0]), "later attempts must get freshly minted deadlines")

	msg, err := s.GetMessage(context.Background(), result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
	assert.NotNil(t, msg.DispatchedAt)
}

func TestDelivery_MarkDispatchedExhaustsRetries_ReturnsAmbiguousSingleBrokerDispatch(t *testing.T) {
	srv, s, faultStore, fault, sender, target, dispatcher := deliverySetupWithFinalizationFault(t)

	faultStore.dispatchedErrs = []error{
		context.DeadlineExceeded,
		context.DeadlineExceeded,
		context.DeadlineExceeded,
		context.DeadlineExceeded,
		context.DeadlineExceeded,
	}
	fault.Arm()

	result, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, "exhaust-dispatched-mark"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAmbiguous, result.Outcome,
		"exhausted MarkMessageDispatched retries must fall back to ambiguous")
	assert.NotEmpty(t, result.MessageID)

	assert.Len(t, dispatcher.getCalls(), 1, "broker dispatch must still run only once")

	faultStore.mu.Lock()
	attempts := faultStore.dispatchedAttempts
	faultStore.mu.Unlock()
	assert.Greater(t, attempts, 1, "must retry more than once")
	assert.LessOrEqual(t, attempts, 4, "retries must be bounded")

	msg, err := s.GetMessage(context.Background(), result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, msg.DispatchState)
}

func TestDelivery_MarkDispatchedCASMiss_DoesNotRetry(t *testing.T) {
	srv, _, faultStore, fault, sender, target, dispatcher := deliverySetupWithFinalizationFault(t)

	faultStore.forceCASMiss = true
	fault.Arm()

	result, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, "cas-miss-no-retry"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAccepted, result.Outcome)
	assert.Len(t, dispatcher.getCalls(), 1)

	faultStore.mu.Lock()
	attempts := faultStore.dispatchedAttempts
	faultStore.mu.Unlock()
	assert.Equal(t, 1, attempts, "CAS miss (dispatched=false, err=nil) must not retry")
}

func TestDelivery_MarkFailedTransientErrors_RetriesWithoutDuplicateBrokerDispatch(t *testing.T) {
	srv, s, faultStore, fault, sender, target, dispatcher := deliverySetupWithFinalizationFault(t)
	dispatcher.returnErr = errors.New("definite broker failure")

	faultStore.failedErrs = []error{context.DeadlineExceeded, context.DeadlineExceeded}
	fault.Arm()

	_, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, "retry-failed-mark"))
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeDeliveryFailed, dmErr.Code)
	assert.Len(t, dispatcher.getCalls(), 1, "broker dispatch must never be retried when MarkMessageFailed retries")

	faultStore.mu.Lock()
	attempts := faultStore.failedAttempts
	deadlines := append([]time.Time(nil), faultStore.failedDeadlines...)
	sawExpired := faultStore.sawExpiredCtx
	faultStore.mu.Unlock()

	assert.Equal(t, 3, attempts, "MarkMessageFailed must retry twice and succeed on attempt 3")
	assert.False(t, sawExpired)
	require.Len(t, deadlines, 3)

	msgID := dmErr.Details["message_id"].(string)
	msg, err := s.GetMessage(context.Background(), msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, msg.DispatchState,
		"row must be marked failed after MarkMessageFailed retry succeeds")
}

func TestDelivery_MarkDispatchedNonRetryableError_StopsImmediately(t *testing.T) {
	srv, _, faultStore, fault, sender, target, dispatcher := deliverySetupWithFinalizationFault(t)

	faultStore.dispatchedErrs = []error{store.ErrNotFound, context.DeadlineExceeded}
	fault.Arm()

	result, dmErr := srv.ExecuteAgentDM(context.Background(), deliveryDMInput(sender, target, "non-retryable-mark"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAmbiguous, result.Outcome)
	assert.Len(t, dispatcher.getCalls(), 1, "broker dispatch must run only once")

	faultStore.mu.Lock()
	attempts := faultStore.dispatchedAttempts
	faultStore.mu.Unlock()
	assert.Equal(t, 1, attempts, "non-retryable store error must stop immediately without retry")
}
