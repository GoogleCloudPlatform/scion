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
// Phase 0.2 (ptone/scion#2192): reject unsafe raw messaging forms before
// side effects.
//
// These tests cover the containment guard added in raw_guard.go and wired
// into handleAgentMessage, handleProjectBroadcast, agent_dm_operation.go and
// createScheduledEvent. Raw remains a temporary guard-only compatibility
// path (ptone/scion#2184): it still supports exactly one shape — an
// unadorned direct message to a single, non-managed, same-project agent —
// and every other combination described in "Raw compatibility and removal"
// is rejected here, before conversation resolution, mention work,
// attachment ingestion, wake/lifecycle calls, persistence or dispatch.
//
// Broker/plugin ingress coverage (both inbound routes) lives in
// handlers_broker_inbound_raw_test.go.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendAgentDMWithMsg posts a StructuredMessage as an agent sender to the
// project-scoped handleAgentMessage route and returns the recorder.
func sendAgentDMWithMsg(t *testing.T, srv *Server, sender, target *store.Agent, sm *messages.StructuredMessage, req MessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	req.StructuredMessage = sm

	reqBody, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message",
		bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, httpReq, target.ID)
	return rr
}

func baseRawStructuredMessage(sender, target *store.Agent, text string) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + sender.Slug,
		SenderID:    sender.ID,
		Recipient:   "agent:" + target.Slug,
		RecipientID: target.ID,
		Msg:         text,
		Raw:         true,
	}
}

// countStoreConversations returns the total number of non-deleted conversations
// in the store (no filter — this matches every conversation, including
// direct DM conversations whose ProjectID is nil).
func countStoreConversations(t *testing.T, s store.Store, ctx context.Context) int {
	t.Helper()
	convs, err := s.ListConversations(ctx, store.ConversationFilter{}, store.ListOptions{Limit: 1000, SkipTotalCount: true})
	require.NoError(t, err)
	return len(convs.Items)
}

// assertZeroMessagingSideEffects asserts, for a rejected raw request: zero
// dispatch calls (including mention fan-out), zero persisted messages of
// any kind (which also transitively proves zero attachment ingestion, since
// ingestion in this codebase is keyed off an already-persisted message
// row), zero persisted mention rows, no conversation created (count
// unchanged from convCountBefore), no notification subscription created for
// target, and zero `PublishUserMessage` calls on the wired spy.
//
// This does NOT observe `MessageBrokerProxy.PublishMessage` (the
// agent-to-agent observer copy, or broker broadcast/group fan-out) or
// `PublishAgentStatus` (wake) — spyEventPublisher only overrides
// PublishUserMessage. Callers that need to prove zero *observer-bus*
// publishes specifically (as opposed to zero SSE `PublishUserMessage`
// calls) must wire a MessageBrokerProxy over a spy bus themselves — see
// raw_guard_observer_test.go for that pattern. The name and assertion
// message below are scoped to what this helper actually checks; do not
// read "SSE" as "every publish path".
func assertZeroMessagingSideEffects(t *testing.T, s store.Store, ctx context.Context, dispatcher *recordingDispatcher, spy *spyEventPublisher, target *store.Agent, convCountBefore, notifyCountBefore int) {
	t.Helper()

	assert.Empty(t, dispatcher.getCalls(), "rejected raw request must produce zero dispatch calls (incl. mention fan-out)")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected raw request must produce zero persisted message rows of any kind")

	mentionMsgs, err := s.ListMessages(ctx, store.MessageFilter{Type: messages.TypeMention}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, mentionMsgs.Items, "rejected raw request must produce zero mention deliveries")

	assert.Equal(t, convCountBefore, countStoreConversations(t, s, ctx), "rejected raw request must not create or modify a conversation")

	subs, err := s.GetNotificationSubscriptions(ctx, target.ID)
	require.NoError(t, err)
	assert.Len(t, subs, notifyCountBefore, "rejected raw request must not create a notification subscription")

	assert.Empty(t, spy.getUserMessages(), "rejected raw request must publish zero SSE PublishUserMessage events")
}

// TestCrossProjectRawUnsupported is a direct unit test of the shared
// cross-project raw predicate (used by both the HTTP-layer check in
// handlers_agent_messaging.go and ExecuteAgentDM step 4b in
// agent_dm_operation.go), proving it has no carve-out for an empty project
// ID on either side. The store layer rejects persisting an Agent with an
// empty ProjectID (it is parsed as a UUID), so this behavior can only be
// exercised as a pure unit test, not through the full HTTP handler with a
// persisted fixture.
func TestCrossProjectRawUnsupported(t *testing.T) {
	tests := []struct {
		name          string
		senderProject string
		targetProject string
		want          bool
	}{
		{"same non-empty project", "proj-a", "proj-a", false},
		{"different non-empty projects", "proj-a", "proj-b", true},
		{"empty sender project, non-empty target", "", "proj-b", true},
		{"non-empty sender project, empty target", "proj-a", "", true},
		{"both empty (not reachable via a real agent record)", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, crossProjectRawUnsupported(tt.senderProject, tt.targetProject))
		})
	}
}

// TestHandleAgentMessage_RawGuard_RejectsUnsupportedCombinations proves that
// every unsupported raw combination listed in ptone/scion#2184 ("Raw
// compatibility and removal") is rejected before any side effect: zero
// dispatch calls, zero persisted messages/mentions, zero conversation
// writes, zero notification subscriptions and zero `PublishUserMessage`
// events, for every case.
func TestHandleAgentMessage_RawGuard_RejectsUnsupportedCombinations(t *testing.T) {
	tests := []struct {
		name       string
		mutateMsg  func(sm *messages.StructuredMessage)
		mutateReq  func(req *MessageRequest)
		wantStatus int
		wantReason MessageDenialCode
	}{
		{
			name:       "raw+plain conflict",
			mutateMsg:  func(sm *messages.StructuredMessage) { sm.Plain = true },
			wantStatus: http.StatusBadRequest,
			wantReason: MessageDenialRawPlainConflict,
		},
		{
			name: "raw+group recipient",
			mutateMsg: func(sm *messages.StructuredMessage) {
				sm.Recipient = "group[agent:a,agent:b]"
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawGroupUnsupported,
		},
		{
			name:       "raw+explicit mentions",
			mutateReq:  func(req *MessageRequest) { req.Mentions = []string{"some-agent"} },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawMentionsUnsupported,
		},
		{
			name:       "raw+attachments",
			mutateMsg:  func(sm *messages.StructuredMessage) { sm.Attachments = []string{"att-1"} },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawAttachUnsupported,
		},
		{
			name:       "raw+wake",
			mutateReq:  func(req *MessageRequest) { req.Wake = true },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawWakeUnsupported,
		},
		{
			name:       "raw+interrupt",
			mutateReq:  func(req *MessageRequest) { req.Interrupt = true },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawInterruptUnsupported,
		},
		{
			// StructuredMessage.Urgent is a second spelling of interrupt
			// (`scion message --raw --interrupt` sets it, and ExecuteAgentDM
			// dispatches with Urgent||Interrupt) and must be rejected the
			// same way req.Interrupt is.
			name:       "raw+urgent (interrupt spelling)",
			mutateMsg:  func(sm *messages.StructuredMessage) { sm.Urgent = true },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawInterruptUnsupported,
		},
		{
			// metadata["group_id"] is a side channel for simulating group
			// membership without the group[] recipient syntax
			// IsGroupRecipient checks; reject it the same way.
			name: "raw+metadata.group_id",
			mutateMsg: func(sm *messages.StructuredMessage) {
				sm.Metadata = map[string]string{"group_id": "some-group-id"}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawGroupUnsupported,
		},
		{
			name:       "raw+observer-only",
			mutateMsg:  func(sm *messages.StructuredMessage) { sm.ObserverOnly = true },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawObserverUnsupported,
		},
		{
			name:       "raw+conversation_id",
			mutateMsg:  func(sm *messages.StructuredMessage) { sm.ConversationID = "some-conversation-id" },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
		{
			name:       "raw+channel (conversation-addressed)",
			mutateMsg:  func(sm *messages.StructuredMessage) { sm.Channel = "web" },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
		{
			// isRawConversationAddressed has no dm:-prefix carve-out, so
			// every thread_id is rejected, not just non-"dm:" ones (see the
			// explicit dm:-prefixed case below).
			name: "raw+thread_id (conversation-addressed)",
			mutateMsg: func(sm *messages.StructuredMessage) {
				sm.Channel = "web"
				sm.ThreadID = "thread:abc"
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
		{
			// A "dm:"-prefixed thread_id is rejected too — there is no
			// carve-out for it.
			name: "raw+dm-prefixed thread_id (conversation-addressed)",
			mutateMsg: func(sm *messages.StructuredMessage) {
				sm.Channel = "web"
				sm.ThreadID = "dm:agent:a:agent:b"
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
		{
			// Each of surface/external_ref/parent_ref is tested alone
			// (rather than only together) so that dropping any single
			// disjunct from raw_guard.go's isRawConversationAddressed check
			// would fail a test.
			name:       "raw+surface (conversation-addressed)",
			mutateReq:  func(req *MessageRequest) { req.Surface = "discord" },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
		{
			// The guard runs before the "external_ref requires surface"
			// validation, so external_ref alone must also return the
			// guard's 422 here, not that validation's 400.
			name:       "raw+external_ref (conversation-addressed)",
			mutateReq:  func(req *MessageRequest) { req.ExternalRef = "123" },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
		{
			name:       "raw+parent_ref (conversation-addressed)",
			mutateReq:  func(req *MessageRequest) { req.ParentRef = "456" },
			wantStatus: http.StatusUnprocessableEntity,
			wantReason: MessageDenialRawConversationUnsupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
			spy := &spyEventPublisher{}
			srv.SetEventPublisher(spy)
			ctx := context.Background()

			convCountBefore := countStoreConversations(t, s, ctx)
			subsBefore, err := s.GetNotificationSubscriptions(ctx, target.ID)
			require.NoError(t, err)

			sm := baseRawStructuredMessage(sender, target, "RAWGUARD-"+tt.name)
			if tt.mutateMsg != nil {
				tt.mutateMsg(sm)
			}
			req := MessageRequest{}
			if tt.mutateReq != nil {
				tt.mutateReq(&req)
			}

			rr := sendAgentDMWithMsg(t, srv, sender, target, sm, req)
			require.Equal(t, tt.wantStatus, rr.Code, "body: %s", rr.Body.String())

			var errResp ErrorResponse
			require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
			require.NotNil(t, errResp.Error.Details)
			assert.Equal(t, string(tt.wantReason), errResp.Error.Details["reason"])

			assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, target, convCountBefore, len(subsBefore))
		})
	}
}

// TestHandleAgentMessage_RawGuard_ManagedBackendRejected proves raw to a
// managed-runtime target is rejected and never reaches CreateInteraction:
// managedAgentMessage only accepts a plain-text body.
func TestHandleAgentMessage_RawGuard_ManagedBackendRejected(t *testing.T) {
	srv, s, project, sender, _, _, dispatcher := deliverySetup(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)
	ctx := context.Background()

	managedTarget := &store.Agent{
		ID:              tid("delivery-managed-target"),
		Name:            "delivery-managed-target",
		Slug:            "delivery-managed-target",
		ProjectID:       project.ID,
		Phase:           "running",
		Runtime:         ManagedRuntimePrefix + "google-adk",
		MessageMode:     store.MessageModeProject,
		RuntimeBrokerID: sender.RuntimeBrokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, managedTarget))

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, managedTarget.ID)
	require.NoError(t, err)

	sm := baseRawStructuredMessage(sender, managedTarget, "RAWGUARD-managed")
	rr := sendAgentDMWithMsg(t, srv, sender, managedTarget, sm, MessageRequest{})

	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawManagedUnsupported), errResp.Error.Details["reason"])

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, managedTarget, convCountBefore, len(subsBefore))
}

// TestHandleAgentMessage_RawGuard_UserSenderManagedBackendRejected covers
// the user caller kind: the agent-sender case above forks into
// ExecuteAgentDM, but a USER sender's managed-backend path calls
// managedAgentMessage directly
// (handlers_agent_messaging.go, the non-agent-DM-fork branch) — the most
// direct route to CreateInteraction, and the guard there
// (isManagedAgentRuntime check, sender-agnostic) had no test for this caller
// kind.
func TestHandleAgentMessage_RawGuard_UserSenderManagedBackendRejected(t *testing.T) {
	srv, s, _, _, target, _, dispatcher := deliverySetup(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)
	ctx := context.Background()

	managedTarget := *target
	managedTarget.Runtime = ManagedRuntimePrefix + "google-adk"
	require.NoError(t, s.UpdateAgent(ctx, &managedTarget))

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, managedTarget.ID)
	require.NoError(t, err)

	rr := sendUserMessage(t, srv, &managedTarget, map[string]any{
		"message": "RAWGUARD-user-managed",
		"raw":     true,
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawManagedUnsupported), errResp.Error.Details["reason"])

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, &managedTarget, convCountBefore, len(subsBefore))
}

// TestExecuteAgentDM_ManagedBackendRaw_Denied exercises the same guard at
// the ExecuteAgentDM choke point directly (agent_dm_operation.go step 4c),
// independent of the HTTP handler's earlier guard.
func TestExecuteAgentDM_ManagedBackendRaw_Denied(t *testing.T) {
	srv, s, project, sender, _, _, dispatcher := deliverySetup(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)
	ctx := context.Background()

	managedTarget := &store.Agent{
		ID:              tid("dm-managed-target"),
		Name:            "dm-managed-target",
		Slug:            "dm-managed-target",
		ProjectID:       project.ID,
		Phase:           "running",
		Runtime:         ManagedRuntimePrefix + "google-adk",
		MessageMode:     store.MessageModeProject,
		RuntimeBrokerID: sender.RuntimeBrokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, managedTarget))

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, managedTarget.ID)
	require.NoError(t, err)

	input := deliveryDMInput(sender, managedTarget, "RAWPROBE-MANAGED")
	input.Raw = true

	_, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.NotNil(t, dmErr, "raw DM to a managed-runtime target must be rejected")
	assert.Equal(t, ErrCodeUnsupportedCapability, dmErr.Code)
	assert.Equal(t, http.StatusUnprocessableEntity, dmErr.HTTPStatus)
	assert.Equal(t, string(MessageDenialRawManagedUnsupported), dmErr.Details["reason"])

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, managedTarget, convCountBefore, len(subsBefore))
}

// TestHandleAgentMessage_RawGuard_StillSupportsUnadornedDirectMessage is a
// negative control: the guard must not reject the one shape raw still
// supports until Phase 2.3 replaces it with /keys.
//
// It also wires spyEventPublisher and asserts exactly one PublishUserMessage
// call — the positive control proving the spy used throughout this file
// actually fires when a message is accepted and persisted, so a mis-wired
// spy in the rejection tests is caught rather than passing undetected.
func TestHandleAgentMessage_RawGuard_StillSupportsUnadornedDirectMessage(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)

	sm := baseRawStructuredMessage(sender, target, "RAWGUARD-allowed")
	rr := sendAgentDMWithMsg(t, srv, sender, target, sm, MessageRequest{})

	require.Equal(t, http.StatusOK, rr.Code, "unadorned direct raw message must still be accepted; body: %s", rr.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.True(t, calls[0].StructuredMessage.Raw)

	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, msgs.Items, 1)

	assert.Len(t, spy.getUserMessages(), 1,
		"positive control: an accepted raw DM must publish exactly one SSE PublishUserMessage event")
}

// TestHandleAgentMessage_RawGuard_LiteralAtTextNotParsedAsMention proves a
// literal "@other-agent" inside a raw message body is never parsed for
// mentions (ptone/scion#2192): exactly one dispatch to the addressed
// target, and zero mention deliveries/rows. Explicit mentions (req.Mentions)
// are already rejected above when combined with raw; this covers the
// separate body-text auto-extraction path (fanOutAgentMentions,
// GoogleCloudPlatform/scion#2083, which calls
// messages.ExtractProseMentions(msg) independently of req.Mentions) in
// handleAgentMessage's agent-DM-fork branch, which is guarded on
// !structuredMsg.Raw so a regression here is caught by this test.
//
// "@other-agent" alone would not prove the guard does anything: the fan-out
// call resolves mention names against real project agents and returns
// immediately for anything that doesn't resolve, so an unresolvable name
// looks identical (one dispatch, zero mention rows) whether or not the
// call happened at all. This test instead mentions a real, resolvable third
// agent in the same project, so removing the guard would make the fan-out
// actually resolve and dispatch to it.
func TestHandleAgentMessage_RawGuard_LiteralAtTextNotParsedAsMention(t *testing.T) {
	srv, s, project, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	mentioned := &store.Agent{
		ID:              tid("delivery-mentioned"),
		Name:            "delivery-mentioned",
		Slug:            "delivery-mentioned",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: sender.RuntimeBrokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, mentioned))

	sm := baseRawStructuredMessage(sender, target, "hey @delivery-mentioned can you look at this")
	rr := sendAgentDMWithMsg(t, srv, sender, target, sm, MessageRequest{})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1, "exactly one dispatch — no mention fan-out from literal @text, even to a real, resolvable agent")
	assert.Equal(t, target.ID, calls[0].Agent.ID, "the single dispatch must go to the addressed target, not the mentioned agent")
	assert.Equal(t, "hey @delivery-mentioned can you look at this", calls[0].Message,
		"the literal @text must reach the target unchanged")

	mentionMsgs, err := s.ListMessages(ctx, store.MessageFilter{Type: messages.TypeMention}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, mentionMsgs.Items, "literal @text must not create mention rows")

	mentionedMsgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: mentioned.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, mentionedMsgs.Items, "the mentioned agent must receive nothing")
}

// sendUserMessage posts a plain-JSON message body as a genuine (non-agent)
// user identity to the top-level handleAgentMessage route.
func sendUserMessage(t *testing.T, srv *Server, target *store.Agent, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	reqBody, err := json.Marshal(body)
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+target.ID+"/message",
		bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(),
		NewAuthenticatedUser(tid("rawguard-user"), "rawguard-user@example.com", "RawGuard User", "admin", "cli")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, httpReq, target.ID)
	return rr
}

// TestHandleAgentMessage_RawGuard_UserSenderStillSupportsDirectMessage is
// the user-caller-kind counterpart of
// TestHandleAgentMessage_RawGuard_StillSupportsUnadornedDirectMessage: both
// caller kinds must be covered, not just agent senders.
func TestHandleAgentMessage_RawGuard_UserSenderStillSupportsDirectMessage(t *testing.T) {
	srv, s, _, _, target, _, dispatcher := deliverySetup(t)

	rr := sendUserMessage(t, srv, target, map[string]any{
		"message": "RAWGUARD-user-allowed",
		"raw":     true,
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	require.NotNil(t, calls[0].StructuredMessage)
	assert.True(t, calls[0].StructuredMessage.Raw)

	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, msgs.Items, 1)
}

// TestHandleAgentMessage_RawGuard_UserSenderRejectsUnsupportedCombinations
// covers the user caller kind for a representative subset of the rejection
// table, plus a wake/lifecycle case: a suspended target with wake=true from
// a USER sender exercises the inline wakeAgentForDM call
// (handleAgentMessage's `req.Wake && !senderIsAgent` branch) that the agent
// caller kind never reaches (agent senders wake inside ExecuteAgentDM,
// post-admission). Asserting the target agent's phase is unchanged proves
// the guard runs before that inline wake, not just before ExecuteAgentDM.
func TestHandleAgentMessage_RawGuard_UserSenderRejectsUnsupportedCombinations(t *testing.T) {
	t.Run("raw+plain conflict", func(t *testing.T) {
		srv, _, _, _, target, _, dispatcher := deliverySetup(t)
		rr := sendUserMessage(t, srv, target, map[string]any{
			"message": "RAWGUARD-user-rawplain",
			"raw":     true,
			"plain":   true,
		})
		require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
		assert.Equal(t, string(MessageDenialRawPlainConflict), errResp.Error.Details["reason"])
		assert.Empty(t, dispatcher.getCalls())
	})

	t.Run("raw+group recipient", func(t *testing.T) {
		srv, s, _, _, target, _, dispatcher := deliverySetup(t)
		ctx := context.Background()
		reqBody, err := json.Marshal(map[string]any{
			"structured_message": map[string]any{
				"msg":       "RAWGUARD-user-group",
				"recipient": "group[agent:a,agent:b]",
				"raw":       true,
			},
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(),
			NewAuthenticatedUser(tid("rawguard-user-2"), "rawguard-user-2@example.com", "RawGuard User 2", "admin", "cli")))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, httpReq, target.ID)
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
		assert.Equal(t, string(MessageDenialRawGroupUnsupported), errResp.Error.Details["reason"])
		assert.Empty(t, dispatcher.getCalls())
		msgs, err := s.ListMessages(ctx, store.MessageFilter{}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items)
	})

	t.Run("raw+wake: zero lifecycle effect on a suspended target", func(t *testing.T) {
		srv, s, _, _, target, _, dispatcher := deliverySetup(t)
		ctx := context.Background()

		suspended := *target
		suspended.Phase = string(state.PhaseSuspended)
		require.NoError(t, s.UpdateAgentStatus(ctx, suspended.ID, store.AgentStatusUpdate{Phase: suspended.Phase}))

		rr := sendUserMessage(t, srv, &suspended, map[string]any{
			"message": "RAWGUARD-user-wake",
			"raw":     true,
			"wake":    true,
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
		assert.Equal(t, string(MessageDenialRawWakeUnsupported), errResp.Error.Details["reason"])
		assert.Empty(t, dispatcher.getCalls())

		reloaded, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), reloaded.Phase,
			"rejected raw+wake must not resume the suspended agent (zero lifecycle effect)")
	})
}

// TestHandleAgentMessage_RawGuard_BothRawSpellings covers the raw spelling
// matrix: top-level `raw` alone, top-level `raw` merged with a
// nested structured_message that has no raw of its own, the legacy `message`
// field, OR semantics between the two spellings, and raw+plain conflicts
// expressed across either spelling. handlers_agent_messaging.go already
// implements this merge (GoogleCloudPlatform/scion#2053); these are
// regression tests for it, not new guard behavior.
func TestHandleAgentMessage_RawGuard_BothRawSpellings(t *testing.T) {
	t.Run("top-level raw with structured_message that has no nested raw", func(t *testing.T) {
		srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
		reqBody, err := json.Marshal(map[string]any{
			"structured_message": map[string]any{
				"msg": "RAWGUARD-toplevel-nested-no-raw",
			},
			"raw": true,
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, httpReq, target.ID)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		calls := dispatcher.getCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].StructuredMessage)
		assert.True(t, calls[0].StructuredMessage.Raw, "top-level raw must merge onto the nested StructuredMessage")
	})

	t.Run("top-level raw with legacy message field", func(t *testing.T) {
		srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
		reqBody, err := json.Marshal(map[string]any{
			"message": "RAWGUARD-toplevel-legacy-message",
			"raw":     true,
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, httpReq, target.ID)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		calls := dispatcher.getCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].StructuredMessage)
		assert.True(t, calls[0].StructuredMessage.Raw, "top-level raw must apply to the legacy message-field path too")
	})

	t.Run("OR semantics: nested true + top-level false is still raw", func(t *testing.T) {
		srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
		reqBody, err := json.Marshal(map[string]any{
			"structured_message": map[string]any{
				"msg": "RAWGUARD-or-semantics",
				"raw": true,
			},
			"raw": false,
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, httpReq, target.ID)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		calls := dispatcher.getCalls()
		require.Len(t, calls, 1)
		require.NotNil(t, calls[0].StructuredMessage)
		assert.True(t, calls[0].StructuredMessage.Raw,
			"nested raw=true must survive a top-level raw=false (OR semantics, either true means raw)")
	})

	t.Run("raw+plain conflict: top-level raw, nested plain", func(t *testing.T) {
		srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
		reqBody, err := json.Marshal(map[string]any{
			"structured_message": map[string]any{
				"msg":   "RAWGUARD-rawplain-a",
				"plain": true,
			},
			"raw": true,
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, httpReq, target.ID)
		require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
		assert.Equal(t, string(MessageDenialRawPlainConflict), errResp.Error.Details["reason"])
		assert.Empty(t, dispatcher.getCalls())
	})

	t.Run("raw+plain conflict: nested raw, top-level plain", func(t *testing.T) {
		srv, _, _, sender, target, _, dispatcher := deliverySetup(t)
		reqBody, err := json.Marshal(map[string]any{
			"structured_message": map[string]any{
				"msg": "RAWGUARD-rawplain-b",
				"raw": true,
			},
			"plain": true,
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost,
			"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: sender.ProjectID,
			Ancestry:  sender.Ancestry,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentMessage(rr, httpReq, target.ID)
		require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
		assert.Equal(t, string(MessageDenialRawPlainConflict), errResp.Error.Details["reason"])
		assert.Empty(t, dispatcher.getCalls())
	})
}

// TestHandleAgentMessage_LogCapture_RawContentRedacted proves that the
// dedicated "message dispatched" log line does not carry raw keystroke
// content for the still-supported raw path, while confirming (via a non-raw
// control in the same test) that the harness would have caught the secret
// had redaction not been applied.
func TestHandleAgentMessage_LogCapture_RawContentRedacted(t *testing.T) {
	const secret = "HANDLE-AGENT-MESSAGE-RAW-SECRET-P8V2X"

	t.Run("raw message content is redacted", func(t *testing.T) {
		// captureSlog must run before deliverySetup: deliverySetup's
		// testServer call binds the server's subsystem message logger
		// (logging.Subsystem, pkg/util/logging) to whatever slog.Default()
		// is at that moment. That binding does not follow a later
		// slog.SetDefault swap, so calling captureSlog afterward would leave
		// the "message received for delivery" log line — the one carrying
		// message_content — writing to the pre-swap logger instead of buf.
		buf := captureSlog(t)
		srv, _, _, sender, target, _, _ := deliverySetup(t)

		sm := baseRawStructuredMessage(sender, target, secret)
		rr := sendAgentDMWithMsg(t, srv, sender, target, sm, MessageRequest{})
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		assert.NotContains(t, buf.String(), secret, "raw message content must not appear in captured logs")
	})

	t.Run("non-raw message content is not redacted (control)", func(t *testing.T) {
		// See the ordering note in the sibling subtest above.
		buf := captureSlog(t)
		srv, _, _, sender, target, _, _ := deliverySetup(t)

		sm := baseRawStructuredMessage(sender, target, secret)
		sm.Raw = false
		rr := sendAgentDMWithMsg(t, srv, sender, target, sm, MessageRequest{})
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		assert.Contains(t, buf.String(), secret,
			"control: non-raw message content should be logged, proving the harness captures message_content when present")
	})
}

// TestHandleAgentMessage_LogCapture_RejectedRawSecretNotExposed proves a
// rejected (not just accepted) raw request on handleAgentMessage does not
// expose its body in captured logs or the error response.
func TestHandleAgentMessage_LogCapture_RejectedRawSecretNotExposed(t *testing.T) {
	const secret = "HANDLE-AGENT-MESSAGE-REJECTED-RAW-SECRET-M3Q7"
	srv, _, _, sender, target, _, _ := deliverySetup(t)
	buf := captureSlog(t)

	sm := baseRawStructuredMessage(sender, target, secret)
	sm.Attachments = []string{"att-1"} // any unsupported combination rejects before dispatch/log

	rr := sendAgentDMWithMsg(t, srv, sender, target, sm, MessageRequest{})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

	assert.NotContains(t, rr.Body.String(), secret, "rejected raw content must not appear in the error response")
	assert.NotContains(t, buf.String(), secret, "rejected raw content must not appear in captured logs")
}

// TestHandleProjectBroadcast_RawRejected proves broadcast (structured_message.raw)
// is rejected before sender identity is stamped, targets are computed, or
// anything is published — a broadcast is never the "single direct message"
// shape raw still supports.
func TestHandleProjectBroadcast_RawRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("broadcast-raw-owner"),
		Email:   "broadcast-raw-owner@example.com",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("broadcast-raw-project"),
		Slug:      "broadcast-raw-project",
		Name:      "Broadcast Raw Project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	target := &store.Agent{
		ID:           tid("broadcast-raw-target"),
		Slug:         "broadcast-raw-target",
		Name:         "Broadcast Raw Target",
		ProjectID:    project.ID,
		Phase:        "running",
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	spy := &spyEventPublisher{}
	srv.SetEventPublisher(spy)

	const secret = "BROADCAST-RAW-SECRET-9KDX2"
	buf := captureSlog(t)

	convCountBefore := countStoreConversations(t, s, ctx)
	subsBefore, err := s.GetNotificationSubscriptions(ctx, target.ID)
	require.NoError(t, err)

	reqBody, err := json.Marshal(BroadcastMessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version,
			Type:    messages.TypeInstruction,
			Msg:     secret,
			Raw:     true,
		},
	})
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "admin", "cli")))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, httpReq, project.ID)

	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawBroadcastUnsupported), errResp.Error.Details["reason"])

	assertZeroMessagingSideEffects(t, s, ctx, dispatcher, spy, target, convCountBefore, len(subsBefore))
	assert.NotContains(t, rr.Body.String(), secret, "raw broadcast content must not appear in the error response")
	assert.NotContains(t, buf.String(), secret, "raw broadcast content must not appear in captured logs")
}

// TestHandleProjectBroadcast_RawPlainConflict proves raw+plain on the
// broadcast route answers 400 invalid_request exactly like every other
// route, rather than the broadcast-specific 422 — Plain must be checked
// first since raw+plain is a request-shape conflict independent of which
// route received it.
func TestHandleProjectBroadcast_RawPlainConflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("broadcast-rawplain-owner"),
		Email:   "broadcast-rawplain-owner@example.com",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("broadcast-rawplain-project"),
		Slug:      "broadcast-rawplain-project",
		Name:      "Broadcast RawPlain Project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	reqBody, err := json.Marshal(BroadcastMessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version,
			Type:    messages.TypeInstruction,
			Msg:     "RAWGUARD-broadcast-rawplain",
			Raw:     true,
			Plain:   true,
		},
	})
	require.NoError(t, err)

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "admin", "cli")))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, httpReq, project.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeInvalidRequest, errResp.Error.Code)
	assert.Equal(t, string(MessageDenialRawPlainConflict), errResp.Error.Details["reason"])
}

// TestCreateScheduledEvent_RawPayloadTombstoned proves the advanced scheduled
// event payload cannot carry a "raw" key through to scheduled dispatch.
// MessageEventPayload has no Raw field, so this key is rejected explicitly
// (422) at decode — this test locks that tombstone in place. Uses the same
// setupScheduledEventTest / doRequest convention as
// handlers_scheduled_events_test.go.
func TestCreateScheduledEvent_RawPayloadTombstoned(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		Payload:   `{"agentName":"test-agent","message":"hello","raw":true}`,
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, string(MessageDenialRawSchedulingUnsupported), errResp.Error.Details["reason"])

	events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, events.Items, "rejected raw scheduled payload must not create a scheduled event")

	// raw:false must be tombstoned too — permissive decoding must not treat
	// an explicit false as "not present, safe to ignore".
	reqFalse := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		Payload:   `{"agentName":"test-agent","message":"hello","raw":false}`,
	}
	recFalse := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", reqFalse)
	require.Equal(t, http.StatusUnprocessableEntity, recFalse.Code, "raw:false must also be tombstoned; body: %s", recFalse.Body.String())
}

// TestCreateScheduledEvent_NonRawPayloadStillWorks is a negative control: a
// normal scheduled-message payload (no "raw" key at all) is unaffected by
// the tombstone.
func TestCreateScheduledEvent_NonRawPayloadStillWorks(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		AgentName: "test-agent",
		Message:   "hello",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
}
