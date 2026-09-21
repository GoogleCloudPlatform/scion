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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deliverySetup creates a server, project, two agents with a broker and
// dispatcher, and a DM conversation — common scaffolding for delivery
// lifecycle tests.
func deliverySetup(t *testing.T) (
	srv *Server, s store.Store,
	project *store.Project,
	sender, target *store.Agent,
	convID string,
	dispatcher *recordingDispatcher,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("delivery-owner"),
		Email:   "delivery-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project = &store.Project{
		ID:        tid("delivery-project"),
		Name:      "delivery-project",
		Slug:      "delivery-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("delivery-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "delivery-broker",
		Slug:   "delivery-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender = &store.Agent{
		ID:              tid("delivery-sender"),
		Name:            "delivery-sender",
		Slug:            "delivery-sender",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target = &store.Agent{
		ID:              tid("delivery-target"),
		Name:            "delivery-target",
		Slug:            "delivery-target",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	convID = conv.ID

	dispatcher = &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	return srv, s, project, sender, target, convID, dispatcher
}

// deliveryDMInput creates a standard AgentDMInput for delivery tests.
func deliveryDMInput(sender, target *store.Agent, msg string) *AgentDMInput {
	return &AgentDMInput{
		SenderAgent: sender,
		SenderIdentity: &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}},
		TargetAgent: target,
		Msg:         msg,
		Type:        "instruction",
		ProjectID:   sender.ProjectID,
	}
}

// spyStateDispatcher is a test dispatcher that invokes a callback before
// returning, allowing tests to inspect mid-flight state.
type spyStateDispatcher struct {
	store      store.Store
	onDispatch func(msgText string)
}

func (d *spyStateDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, message string, _ bool, _ *messages.StructuredMessage) error {
	if d.onDispatch != nil {
		d.onDispatch(message)
	}
	return nil
}

func (d *spyStateDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *spyStateDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
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
func (d *spyStateDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *spyStateDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
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
	})

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
	})

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
// AC-4: Context cancellation during dispatch → ambiguous outcome
// ---------------------------------------------------------------------------

func TestDelivery_ContextCancellation_AmbiguousOutcome(t *testing.T) {
	srv, s, _, sender, target, _, _ := deliverySetup(t)

	// Create a dispatcher that returns context.Canceled to simulate
	// the request being cancelled during the dispatch call.
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

	// Message should remain in pending state (not transitioned).
	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, msg.DispatchState,
		"message must remain pending on ambiguous dispatch")
}

func TestDelivery_DeadlineExceeded_AmbiguousOutcome(t *testing.T) {
	srv, _, _, sender, target, _, _ := deliverySetup(t)

	// Context deadline exceeded during dispatch is ambiguous.
	deadlineDispatcher := &recordingDispatcher{returnErr: context.DeadlineExceeded}
	srv.SetDispatcher(deadlineDispatcher)

	ctx := context.Background()
	result, dmErr := srv.ExecuteAgentDM(ctx, deliveryDMInput(sender, target, "deadline"))
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAmbiguous, result.Outcome)
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
