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

// Tests for the p2a-u2 review round-3 fix (design agent-reincarnate §3.7,
// Amendment A25.8, R1): the agent outbound endpoint
// (POST /agents/{id}/outbound-message) never resolved a caller-supplied
// recipient_id through GetUser before using it as the "user" side of a
// derived dm: key (resolveOutboundRouting's Rules 2/3 branch). An agent
// sender could supply another agent's UUID (or any nonexistent UUID) as
// recipient_id and the hub would derive dm:agent:<A>:user:<whatever> and
// register a phantom "user:<whatever>" participant row — the same class of
// defect A25.7 R2 closed for caller-supplied ThreadIDs. Folds in the
// reviewer's TestRev2aU2_Outbound_RecipientIDAgentUUID_WithRecipientString
// repro (p2a-u2.md R1).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outboundSendA258 posts to the agent outbound-message endpoint as the given
// sender, with an optional caller-supplied ThreadID. recipientStr is the
// human-facing "recipient" field the reviewer's repro also sets (non-empty,
// to prove validation applies regardless of what that display string says).
func outboundSendA258(t *testing.T, srv *Server, sender *store.Agent, recipientID, recipientStr, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"recipient_id": recipientID,
		"recipient":    recipientStr,
		"msg":          "hi",
		"type":         "input-needed",
	}
	if threadID != "" {
		body["thread_id"] = threadID
		body["channel"] = "web" // ValidateLegacyMessage requires a channel when thread_id is set.
	}
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(sender.ID, sender.ProjectID)))
	rec := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rec, req, sender.ID)
	return rec
}

// outboundSendA259ConvRef posts to the agent outbound-message endpoint with a
// conversation_ref (DEF-138 Rule 1) plus a recipient_id, as the given sender.
func outboundSendA259ConvRef(t *testing.T, srv *Server, sender *store.Agent, conversationRef, recipientID, recipientStr string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]interface{}{
		"conversation_ref": conversationRef,
		"recipient_id":     recipientID,
		"recipient":        recipientStr,
		"msg":              "hi peer",
		"type":             "instruction",
	}
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(sender.ID, sender.ProjectID)))
	rec := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rec, req, sender.ID)
	return rec
}

// setupA258Broker wires a minimal MessageBrokerProxy with "web" registered as
// a valid channel, mirroring TestDEF158_AC7_OriginalValidation_StillRejects.
// ValidateLegacyMessage requires a non-empty Channel whenever ThreadID is
// set, and validateChannelRegistered requires a real broker for any
// non-empty channel — deliverySetup's bare fixture has neither, so any
// ThreadID-carrying request needs this to reach the recipient_id validation
// (and beyond) instead of dying early with an unrelated 503.
func setupA258Broker(t *testing.T, srv *Server, s store.Store, projectID string) {
	t.Helper()
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events, func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(projectID)
}

// assertAddrUnknown decodes rec's body as an ErrorResponse and asserts the
// error code is exactly "addr_unknown" (A25.9 O1) — not merely "some 4xx",
// which an unrelated earlier 400 (e.g. the DM ownership check) would also
// satisfy.
func assertAddrUnknown(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.True(t, rec.Code >= 400 && rec.Code < 500, "expected 4xx, got %d: %s", rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp), "body: %s", rec.Body.String())
	assert.Equal(t, ErrCodeAddrUnknown, errResp.Error.Code, "body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// A25.8 R1: recipient_id alone (no caller ThreadID)
// ---------------------------------------------------------------------------

// TestHandleAgentOutboundMessage_A258_R1_RecipientIDAgentUUID_Rejected is the
// reviewer's TestRev2aU2_Outbound_RecipientIDAgentUUID_WithRecipientString
// repro: agent A posts recipient_id=<agent Z's UUID> with an unrelated
// "recipient" display string. Must be rejected before any conversation or
// participant write — not the pre-fix "200, then a phantom row".
func TestHandleAgentOutboundMessage_A258_R1_RecipientIDAgentUUID_Rejected(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	rec := outboundSendA258(t, srv, sender, target.ID, "user:anything", "")

	assertAddrUnknown(t, rec)
	assert.Empty(t, dispatcher.calls, "a rejected recipient_id must not dispatch")

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "user", target.ID)
	require.NoError(t, err)
	conv, convErr := s.GetConversationByExternalRef(ctx, "native", dmKey)
	assert.True(t, convErr != nil || conv == nil,
		"no conversation may be created for an unresolved recipient_id naming an agent")
}

// TestHandleAgentOutboundMessage_A258_R1_RecipientIDNonexistentUUID_Rejected
// covers the same defect for a recipient_id that doesn't resolve to any
// principal at all.
func TestHandleAgentOutboundMessage_A258_R1_RecipientIDNonexistentUUID_Rejected(t *testing.T) {
	srv, s, _, sender, _, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	ghostID := tid("a258-ghost")
	rec := outboundSendA258(t, srv, sender, ghostID, "user:ghost", "")

	assertAddrUnknown(t, rec)
	assert.Empty(t, dispatcher.calls)

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "user", ghostID)
	require.NoError(t, err)
	conv, convErr := s.GetConversationByExternalRef(ctx, "native", dmKey)
	assert.True(t, convErr != nil || conv == nil,
		"no conversation may be created for a nonexistent recipient_id")
}

// TestHandleAgentOutboundMessage_A258_R1_RecipientIDRealUser_Allowed is the
// positive control: a genuine, store-resolved user recipient_id must still
// succeed, with both participant rows registered (A25.6 F1/F3 unaffected).
func TestHandleAgentOutboundMessage_A258_R1_RecipientIDRealUser_Allowed(t *testing.T) {
	srv, s, _, sender, _, _, _ := deliverySetup(t)
	ctx := context.Background()

	user := &store.User{ID: tid("a258-real-user"), Email: "a258-real-user@test.com", DisplayName: "A258 User"}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := outboundSendA258(t, srv, sender, user.ID, "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.MessageID)

	msg, err := s.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	require.NotEmpty(t, msg.ConversationID)
	assertBothParticipants(t, s, msg.ConversationID, "agent", sender.ID, "user", user.ID)
}

// ---------------------------------------------------------------------------
// A25.8 R1: the same three, with a caller-supplied "dm:" ThreadID that is
// self-consistent with recipient_id (so the A25.7 ownership check alone
// would pass — the fix must close this via the earlier recipient_id
// validation, independent of the ownership check).
// ---------------------------------------------------------------------------

// TestHandleAgentOutboundMessage_A258_R1_ThreadIDVariant_AgentUUID_Rejected
// wires a real broker proxy (A25.9 O1) so the request reaches the actual
// recipient_id validation instead of dying early at validateChannelRegistered
// (503, deliverySetup has no broker) — the p2a-u3 review's mR1_errnil
// (dropping the `return` after the 400) is only observable through the real
// path: without the broker, execution never gets far enough for a missing
// `return` to matter.
func TestHandleAgentOutboundMessage_A258_R1_ThreadIDVariant_AgentUUID_Rejected(t *testing.T) {
	srv, s, project, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()
	setupA258Broker(t, srv, s, project.ID)

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "user", target.ID)
	require.NoError(t, err)

	rec := outboundSendA258(t, srv, sender, target.ID, "", dmKey)

	assertAddrUnknown(t, rec)
	assert.Empty(t, dispatcher.calls)

	conv, convErr := s.GetConversationByExternalRef(ctx, "native", dmKey)
	assert.True(t, convErr != nil || conv == nil,
		"no conversation may be created even when the ThreadID is self-consistent with the phantom recipient_id")
}

func TestHandleAgentOutboundMessage_A258_R1_ThreadIDVariant_NonexistentUUID_Rejected(t *testing.T) {
	srv, s, project, sender, _, _, dispatcher := deliverySetup(t)
	ctx := context.Background()
	setupA258Broker(t, srv, s, project.ID)

	ghostID := tid("a258-thread-ghost")
	dmKey, err := messages.DMConversationKey("agent", sender.ID, "user", ghostID)
	require.NoError(t, err)

	rec := outboundSendA258(t, srv, sender, ghostID, "", dmKey)

	assertAddrUnknown(t, rec)
	assert.Empty(t, dispatcher.calls)

	conv, convErr := s.GetConversationByExternalRef(ctx, "native", dmKey)
	assert.True(t, convErr != nil || conv == nil, "no conversation may be created")
}

func TestHandleAgentOutboundMessage_A258_R1_ThreadIDVariant_RealUser_Allowed(t *testing.T) {
	srv, s, project, sender, _, _, _ := deliverySetup(t)
	ctx := context.Background()
	setupA258Broker(t, srv, s, project.ID)

	user := &store.User{ID: tid("a258-thread-real-user"), Email: "a258-thread-real-user@test.com", DisplayName: "A258 Thread User"}
	require.NoError(t, s.CreateUser(ctx, user))

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "user", user.ID)
	require.NoError(t, err)

	rec := outboundSendA258(t, srv, sender, user.ID, "", dmKey)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The conversation is resolved (and participants registered) synchronously
	// inside resolveOutboundRouting, before the broker publish — so this is
	// safe to assert immediately without waiting on the async delivery path.
	conv, convErr := s.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, convErr)
	require.NotNil(t, conv)
	assertBothParticipants(t, s, conv.ID, "agent", sender.ID, "user", user.ID)
}

// ---------------------------------------------------------------------------
// A25.9 R1: pin the ConversationRef half of the scoping clause. The
// ConversationID half is already pinned by
// TestOutboundDMAuthz_ConversationID_Allowed_Persisted /
// _TargetModeNone_Denied (handlers_outbound_authz_test.go); the
// ConversationRef half had no test, and dropping
// "&& req.ConversationRef == """ (mR1_refscope) survived every existing
// test. Folds in the reviewer's TestRevU3_ConvRef_AgentConv_PeerRecipient.
// ---------------------------------------------------------------------------

// TestHandleAgentOutboundMessage_A259_R1_ConversationRef_AgentPeer_Allowed is
// the reviewer's repro: conversation_ref names an existing agent<->agent DM,
// recipient_id names the peer agent. On this asserted path recipient_id is
// only compared against the conversation's own already-authorized identity
// (Case (b) in handleAgentOutboundMessage) — it must never be required to
// resolve as a user, and the send must reach ExecuteAgentDM and dispatch.
func TestHandleAgentOutboundMessage_A259_R1_ConversationRef_AgentPeer_Allowed(t *testing.T) {
	srv, _, _, sender, target, convID, dispatcher := deliverySetup(t)

	rec := outboundSendA259ConvRef(t, srv, sender, "conv:"+convID, target.ID, "agent:"+target.Slug)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	found := false
	for _, d := range dispatcher.calls {
		if d.Agent != nil && d.Agent.ID == target.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a dispatch to the peer agent %s, got calls: %+v", target.ID, dispatcher.calls)
}

// TestHandleAgentOutboundMessage_A259_R1_ConversationRef_UserPeer_Allowed is
// the agent<->user variant of the same scoping clause: conversation_ref
// names an existing agent<->user DM, recipient_id names that user. This
// exercises the "user delivery" half of DEF-138 Rule 1 (not Case (b)'s
// agent-DM detection), confirming the ConversationRef scope-out isn't
// accidentally agent-DM-specific.
func TestHandleAgentOutboundMessage_A259_R1_ConversationRef_UserPeer_Allowed(t *testing.T) {
	srv, s, _, sender, _, _, _ := deliverySetup(t)
	ctx := context.Background()

	user := &store.User{ID: tid("a259-convref-user"), Email: "a259-convref-user@test.com", DisplayName: "A259 ConvRef User"}
	require.NoError(t, s.CreateUser(ctx, user))

	dmKey, err := messages.DMConversationKey("agent", sender.ID, "user", user.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: dmKey, DriftState: "active",
	})
	require.NoError(t, err)

	rec := outboundSendA259ConvRef(t, srv, sender, "conv:"+conv.ID, user.ID, "user:"+user.Email)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	found := false
	for _, m := range msgs.Items {
		if m.Msg == "hi peer" {
			found = true
			break
		}
	}
	assert.True(t, found, "the conversation_ref send must be persisted")
}

// ---------------------------------------------------------------------------
// A25.9 O3: recipient_id -> canonical-ID normalization.
// ---------------------------------------------------------------------------

// TestHandleAgentOutboundMessage_A259_O3_RecipientIDNonCanonicalCase_Canonicalized
// pins `recipientID = u.ID` (the assignment right after a successful
// GetUser): a non-canonical-case UUID must resolve to, and be used as, the
// canonical-case ID everywhere downstream — the persisted message and the
// participant row.
func TestHandleAgentOutboundMessage_A259_O3_RecipientIDNonCanonicalCase_Canonicalized(t *testing.T) {
	srv, s, _, sender, _, _, _ := deliverySetup(t)
	ctx := context.Background()

	user := &store.User{ID: tid("a259-o3-user"), Email: "a259-o3-user@test.com", DisplayName: "A259 O3 User"}
	require.NoError(t, s.CreateUser(ctx, user))
	upperID := strings.ToUpper(user.ID)
	require.NotEqual(t, user.ID, upperID, "fixture ID must contain letters for the case flip to matter")

	rec := outboundSendA258(t, srv, sender, upperID, "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		MessageID   string `json:"message_id"`
		RecipientID string `json:"recipient_id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, user.ID, resp.RecipientID, "the response must carry the canonical-case ID, not the caller's casing")

	msg, err := s.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, user.ID, msg.RecipientID, "the persisted row must carry the canonical-case ID, not the caller's casing")

	require.NotEmpty(t, msg.ConversationID)
	parts, err := s.ListParticipants(ctx, msg.ConversationID)
	require.NoError(t, err)
	found := false
	for _, p := range parts {
		if p.PrincipalKind == "user" && p.PrincipalID == user.ID {
			found = true
		}
		assert.NotEqual(t, upperID, p.PrincipalID, "no participant row may carry the non-canonical casing")
	}
	assert.True(t, found, "expected a canonical-case user participant row, got: %+v", parts)
}
