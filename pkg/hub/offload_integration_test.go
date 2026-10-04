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

//go:build !no_sqlite

package hub

// ---------------------------------------------------------------------------
// P1 integration tests (ptone/scion#2257, design auto-offload-large-dm §12):
// I1 (agent DM through /message and /outbound-message), I2 (envelope switch
// OFF), I7 (user-token sender + mention invariant), I8 (dead-stub guard), I9
// (#2083 agent mentions), I10 (sender deleted).
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enableOffload sets messaging.offload_threshold_runes (and, when
// envelopeOn, conversation_envelope_switch) via OperationalSettings, mirroring
// enableCPM's pattern (cross_project_messaging_test.go).
func enableOffload(t *testing.T, srv *Server, threshold int, envelopeOn bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := koanf.New(".")
	envK := koanf.New(".")
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	doc := []byte(`{"conversation_envelope_switch":` + boolStr(envelopeOn) +
		`,"offload_threshold_runes":` + strconv.Itoa(threshold) + `}`)
	rev, err := ops.Update(context.Background(), "messaging", doc, "test", 0, "managed")
	require.NoError(t, err, "failed to seed messaging opsettings")
	require.Greater(t, rev, int64(0))
	require.Equal(t, threshold, ops.OffloadThresholdRunes())
	srv.SetOperationalSettings(ops)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func offloadSHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// I1: agent-token DM through /message and /outbound-message.
// ---------------------------------------------------------------------------

// assertStubDeliveryText asserts that dt is a rendered stub envelope: non-empty,
// bounded (design §5 AC1: msg field <= 1200 bytes escaped, but the whole
// envelope is checked here against the more generous 2048-byte AC1 bound),
// names the fetch command, and — the actual regression finding r1 #1 guards
// against — does NOT contain the full body verbatim. If the second render
// (agent_dm_operation.go / handlers_agent_messaging.go) were dropped or
// reordered, DeliveryText would silently still contain the full paste while
// every other assertion in these tests kept passing.
func assertStubDeliveryText(t *testing.T, dt, body, msgID string) {
	t.Helper()
	require.NotEmpty(t, dt, "dispatched DeliveryText must be rendered when the envelope switch is on")
	assert.LessOrEqual(t, len(dt), 2048, "AC1: the rendered envelope must be <= 2048 bytes")
	assert.Contains(t, dt, "scion conversation get-message", "the stub's fetch command must be in the rendered envelope")
	assert.Contains(t, dt, msgID, "the rendered envelope must name the persisted message ID")
	assert.NotContains(t, dt, body, "the rendered envelope must NOT contain the full body — this is what a dropped/reordered second render would silently leak")
}

func TestOffload_I1_StructuredEndpoint_StubDispatchedFullBodyPersisted(t *testing.T) {
	srv, s, _, sender, target, dmConvID, dispatcher, spyBus := paritySetup(t)
	enableOffload(t, srv, 4000, true)
	if bp := srv.GetMessageBrokerProxy(); bp != nil {
		bp.subscribeAgent(target.ProjectID, target.Slug)
	}

	body := strings.Repeat("x", 12000)
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	call := calls[0]

	// The dispatcher receives the stub both in DeliveryText/StructuredMessage
	// and as the plain "message" argument.
	require.NotNil(t, call.StructuredMessage)
	assert.NotEqual(t, body, call.StructuredMessage.Msg, "dispatched Msg must be the stub, not the full body")
	assert.NotEqual(t, body, call.Message, "the plain dispatch argument must also be the stub")
	assert.Contains(t, call.StructuredMessage.Msg, "scion conversation get-message")
	assert.Contains(t, call.StructuredMessage.Msg, "--body")
	assert.Equal(t, "true", call.StructuredMessage.Metadata[messaging.MetaBodyOffloaded])
	assert.Equal(t, offloadSHA256Hex(body), call.StructuredMessage.Metadata[messaging.MetaBodySHA256])

	// The row has the full body.
	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{ConversationID: dmConvID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	var msgID string
	for _, m := range msgs.Items {
		if m.Msg == body {
			msgID = m.ID
		}
	}
	require.NotEmpty(t, msgID, "the persisted row must have the full body")

	// r1 #1: the dispatched envelope (DeliveryText) must be the stub, not the
	// full-body paste, and the SSE-published row (checked above) and the
	// observer copy must keep the full body AND the full-body envelope.
	assertStubDeliveryText(t, call.StructuredMessage.DeliveryText, body, msgID)

	var sawObserver bool
	for _, evt := range spyBus.getEvents() {
		if evt.msg != nil && evt.msg.ObserverOnly {
			sawObserver = true
			assert.Equal(t, body, evt.msg.Msg, "observer must keep the full body")
			assert.Contains(t, evt.msg.DeliveryText, body, "observer's rendered envelope must contain the full body")
		}
	}
	assert.True(t, sawObserver, "expected at least one observer-only event")
}

func TestOffload_I1_OutboundEndpoint_StubDispatched(t *testing.T) {
	srv, s, _, sender, target, dmConvID, dispatcher, _ := paritySetup(t)
	enableOffload(t, srv, 4000, true)

	body := strings.Repeat("y", 12000)
	rr := sendViaOutbound(t, srv, sender, dmConvID, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assert.NotEqual(t, body, calls[0].StructuredMessage.Msg)
	assert.NotEqual(t, body, calls[0].Message)

	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{ConversationID: dmConvID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	var msgID string
	for _, m := range msgs.Items {
		if m.Msg == body {
			msgID = m.ID
		}
	}
	require.NotEmpty(t, msgID)
	assertStubDeliveryText(t, calls[0].StructuredMessage.DeliveryText, body, msgID)
}

func TestOffload_I1_ObserverAndSSEKeepFullBody(t *testing.T) {
	srv, s, _, sender, target, dmConvID, _, spyBus := paritySetup(t)
	enableOffload(t, srv, 4000, true)
	if bp := srv.GetMessageBrokerProxy(); bp != nil {
		bp.subscribeAgent(target.ProjectID, target.Slug)
	}

	body := strings.Repeat("z", 12000)
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// SSE: the persisted row (what PublishUserMessage publishes from) has the
	// full body.
	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{ConversationID: dmConvID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	var sawFullRow bool
	for _, m := range msgs.Items {
		if m.Msg == body {
			sawFullRow = true
		}
	}
	assert.True(t, sawFullRow, "SSE publishes from the persisted row, which must have the full body")

	var sawObserver bool
	for _, evt := range spyBus.getEvents() {
		if evt.msg != nil && evt.msg.ObserverOnly {
			sawObserver = true
			assert.Equal(t, body, evt.msg.Msg, "observer must keep the full body")
			assert.Contains(t, evt.msg.DeliveryText, body, "observer's rendered envelope must contain the full body")
		}
	}
	assert.True(t, sawObserver, "expected at least one observer-only event")
}

// r4 #3: at threshold 0 (compiled default), the dispatched and observer
// messages are exactly today's — same Msg, same DeliveryText — and carry no
// client metadata. Golden check (r1 #1): the dispatched and observer
// DeliveryText are byte-identical, since both render from the same
// unmodified structuredMsg when nothing qualifies for offload.
func TestOffload_Threshold0_LeavesDispatchUnaffected(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher, spyBus := paritySetup(t)
	// Threshold explicitly 0 (matches the compiled default), envelope switch
	// ON so DeliveryText actually renders (writeDenyEnabled() is false when
	// OperationalSettings itself is nil, which it is by default in
	// testServer — without this call there would be nothing to compare).
	enableOffload(t, srv, 0, true)
	if bp := srv.GetMessageBrokerProxy(); bp != nil {
		bp.subscribeAgent(target.ProjectID, target.Slug)
	}

	body := strings.Repeat("w", 12000)
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assert.Equal(t, body, calls[0].StructuredMessage.Msg, "threshold 0 must leave the dispatched body unchanged")
	assert.Equal(t, body, calls[0].Message)
	_, hasOffloaded := calls[0].StructuredMessage.Metadata[messaging.MetaBodyOffloaded]
	assert.False(t, hasOffloaded)
	require.NotEmpty(t, calls[0].StructuredMessage.DeliveryText)
	assert.Contains(t, calls[0].StructuredMessage.DeliveryText, body, "threshold 0 must render the full body into the envelope")

	var sawObserver bool
	for _, evt := range spyBus.getEvents() {
		if evt.msg != nil && evt.msg.ObserverOnly {
			sawObserver = true
			assert.Equal(t, calls[0].StructuredMessage.Msg, evt.msg.Msg,
				"golden: at threshold 0, dispatched and observer Msg must be identical")
			assert.Equal(t, calls[0].StructuredMessage.DeliveryText, evt.msg.DeliveryText,
				"golden: at threshold 0, dispatched and observer DeliveryText must be identical")
		}
	}
	assert.True(t, sawObserver, "expected at least one observer-only event")
}

// AC3/AC6: client-supplied body_* metadata is dropped even when the message
// does not qualify for offload (threshold 0 or below-threshold body).
func TestOffload_ClientSuppliedReservedMetadataAlwaysStripped(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher, _ := paritySetup(t)

	sm := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + sender.Slug,
		SenderID:    sender.ID,
		Recipient:   "agent:" + target.Slug,
		RecipientID: target.ID,
		Msg:         "short body",
		Metadata: map[string]string{
			messaging.MetaBodyOffloaded: "true",
			messaging.MetaBodyChars:     "999999",
			messaging.MetaBodySHA256:    "deadbeef",
			"harmless":                  "kept",
		},
	}
	reqBody, err := json.Marshal(MessageRequest{StructuredMessage: sm})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+target.ProjectID+"/agents/"+target.ID+"/message",
		strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(sender.ID, sender.ProjectID))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	md := calls[0].StructuredMessage.Metadata
	_, hasOffloaded := md[messaging.MetaBodyOffloaded]
	_, hasChars := md[messaging.MetaBodyChars]
	_, hasSHA := md[messaging.MetaBodySHA256]
	assert.False(t, hasOffloaded, "spoofed body_offloaded must be stripped")
	assert.False(t, hasChars, "spoofed body_chars must be stripped")
	assert.False(t, hasSHA, "spoofed body_sha256 must be stripped")
	assert.Equal(t, "kept", md["harmless"], "other client metadata is unaffected")
}

// ---------------------------------------------------------------------------
// I2: envelope switch OFF.
// ---------------------------------------------------------------------------

func TestOffload_I2_EnvelopeSwitchOff_MsgStillReplaced(t *testing.T) {
	srv, _, _, sender, target, _, dispatcher, _ := paritySetup(t)
	enableOffload(t, srv, 4000, false) // envelope switch OFF

	body := strings.Repeat("q", 12000)
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assert.NotEqual(t, body, calls[0].StructuredMessage.Msg, "Msg must be the stub regardless of the envelope switch")
	assert.Equal(t, "true", calls[0].StructuredMessage.Metadata[messaging.MetaBodyOffloaded])
	// With the switch OFF, the hub never renders DeliveryText — the broker's
	// legacy FormatForDelivery renders from Msg/Metadata at dispatch time
	// instead. Since P2 added the 3 keys to deliveryMetadataAllowlist
	// (pkg/messages/format.go), that legacy render must show them (nit 8/I2).
	require.Empty(t, calls[0].StructuredMessage.DeliveryText, "the hub does not render when the envelope switch is off")
	legacyText := messages.FormatForDelivery(calls[0].StructuredMessage)
	assert.Contains(t, legacyText, messaging.MetaBodyOffloaded)
	assert.Contains(t, legacyText, messaging.MetaBodyChars)
	assert.Contains(t, legacyText, messaging.MetaBodySHA256)
	assert.NotContains(t, legacyText, body, "the legacy envelope must also be the stub, not the full body")
}

// ---------------------------------------------------------------------------
// I7: user-token sender, and the mention invariant.
// ---------------------------------------------------------------------------

func TestOffload_I7_UserTokenSender_Offloaded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("i7-project"), Name: "i7-project", Slug: "i7-project", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("i7-agent"), Name: "i7-agent", Slug: "i7-agent",
		ProjectID: project.ID, Phase: "running", MessageMode: store.MessageModeProject, RuntimeBrokerID: "broker",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableOffload(t, srv, 4000, true)

	body := strings.Repeat("u", 12000)
	rr := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:dev",
				Recipient: "agent:" + agent.Slug,
				Msg:       body,
				Type:      messages.TypeInstruction,
			},
		})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, agent.ID)
	require.Len(t, calls, 1)
	assert.NotEqual(t, body, calls[0].StructuredMessage.Msg, "user-token sender's over-threshold body must also be offloaded")
	assert.Equal(t, offloadSHA256Hex(body), calls[0].StructuredMessage.Metadata[messaging.MetaBodySHA256])

	msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: agent.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	var msgID string
	for _, m := range msgs.Items {
		if m.Msg == body {
			msgID = m.ID
		}
	}
	require.NotEmpty(t, msgID)
	assertStubDeliveryText(t, calls[0].StructuredMessage.DeliveryText, body, msgID)
}

// nit 8 (impl review r1): a CreateMessage failure means MessageID == "", so
// OffloadForDelivery's own guard never offloads — the body must be
// delivered inline, in full, with no stub, regardless of threshold.
func TestOffload_I7_CreateMessageFails_DeliversInlineNoStub(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("i7f-project"), Name: "i7f-project", Slug: "i7f-project", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("i7f-agent"), Name: "i7f-agent", Slug: "i7f-agent",
		ProjectID: project.ID, Phase: "running", MessageMode: store.MessageModeProject, RuntimeBrokerID: "broker",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableOffload(t, srv, 4000, true)
	srv.store = &createMessageFailStore{Store: s}

	body := strings.Repeat("f", 12000)
	rr := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:dev",
				Recipient: "agent:" + agent.Slug,
				Msg:       body,
				Type:      messages.TypeInstruction,
			},
		})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, agent.ID)
	require.Len(t, calls, 1)
	assert.Equal(t, body, calls[0].StructuredMessage.Msg, "an unpersisted message (CreateMessage failed) must deliver inline, never a stub")
	_, hasOffloaded := calls[0].StructuredMessage.Metadata[messaging.MetaBodyOffloaded]
	assert.False(t, hasOffloaded)
}

func TestOffload_I7_MentionRowKeepsFullBody(t *testing.T) {
	// Human-sender mentions (processMentions) are not offloaded until Phase 4
	// (design §10 P4), and the mention row must always persist the full body
	// regardless — the primary's stub must never be copied into it.
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("i7m-project"), Name: "i7m-project", Slug: "i7m-project", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("i7m-primary"), Name: "i7m-primary", Slug: "i7m-primary", ProjectID: project.ID, Phase: "running", MessageMode: store.MessageModeProject, RuntimeBrokerID: "broker"}
	require.NoError(t, s.CreateAgent(ctx, primary))
	mentioned := &store.Agent{ID: tid("i7m-mentioned"), Name: "i7m-mentioned", Slug: "i7m-mentioned", ProjectID: project.ID, Phase: "running", MessageMode: store.MessageModeProject, RuntimeBrokerID: "broker"}
	require.NoError(t, s.CreateAgent(ctx, mentioned))
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableOffload(t, srv, 4000, true)

	body := strings.Repeat("m", 12000) + " @" + mentioned.Slug
	rr := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+primary.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:dev",
				Recipient: "agent:" + primary.Slug,
				Msg:       body,
				Type:      messages.TypeInstruction,
			},
			Mentions: []string{mentioned.Slug},
		})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Primary is offloaded.
	primaryCalls := dispatchesTo(dispatcher, primary.ID)
	require.Len(t, primaryCalls, 1)
	assert.NotEqual(t, body, primaryCalls[0].StructuredMessage.Msg)

	// Before Phase 4, the mentioned agent's full body is delivered inline —
	// never the primary's stub.
	mentionCalls := dispatchesTo(dispatcher, mentioned.ID)
	require.Len(t, mentionCalls, 1)
	assert.Equal(t, body, mentionCalls[0].StructuredMessage.Msg, "human-sender mention delivery is unoffloaded pre-Phase-4")

	// The mention row itself always has the full body (sha matches).
	msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: mentioned.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, body, msgs.Items[0].Msg)
}

// ---------------------------------------------------------------------------
// I8: dead-stub guard. A sender can stamp a conversation the recipient
// cannot read; in P1 (no fetch-by-ID) that body must go inline, never as a
// stub naming an unreadable conversation.
// ---------------------------------------------------------------------------

func TestOffload_I8_DeadStubGuard_UnreadableConversationDeliversInline(t *testing.T) {
	// Design example (§4.3): agent A has a DM with user U and sends a large
	// body to agent B naming conversation_id=<A<->U>. B is not a party to
	// that key, so RecipientCanReadConv is false and the body must go
	// inline — never a stub naming a conversation B can't fetch from.
	srv, s, _, sender, target, _, dispatcher, _ := paritySetup(t)
	ctx := context.Background()
	enableOffload(t, srv, 4000, true)

	otherUserID := tid("i8-other-user")
	key, err := messages.DMConversationKey("agent", sender.ID, "user", otherUserID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: key, DriftState: "active",
	})
	require.NoError(t, err)

	body := strings.Repeat("d", 12000)
	senderIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}
	result, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    target,
		Msg:            body,
		Type:           messages.TypeInstruction,
		ConversationID: conv.ID,
		ProjectID:      sender.ProjectID,
	})
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assert.Equal(t, body, calls[0].StructuredMessage.Msg,
		"the target cannot read the sender's stamped A<->U conversation, and there is no fetch-by-ID in P1, so the body must go inline")
}

// ---------------------------------------------------------------------------
// I9: #2083 agent mentions — each mention gets its own row, conversation,
// and (when it qualifies) its own stub.
// ---------------------------------------------------------------------------

func TestOffload_I9_AgentMention_GetsOwnStub(t *testing.T) {
	srv, s, _, sender, target, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)
	enableOffload(t, srv, 4000, true)

	body := strings.Repeat("n", 12000) + " @" + mentionBystanderSlug
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	targetCalls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, targetCalls, 1)
	assert.NotEqual(t, body, targetCalls[0].StructuredMessage.Msg, "primary must be offloaded")

	bystanderCalls := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, bystanderCalls, 1, "@%s must be fanned out exactly once", mentionBystanderSlug)
	assert.NotEqual(t, body, bystanderCalls[0].StructuredMessage.Msg, "the mention recipient must get its own stub, not the full body")
	assert.NotEqual(t, targetCalls[0].StructuredMessage.Msg, bystanderCalls[0].StructuredMessage.Msg,
		"the mention's stub must name its OWN row/conversation, not the primary's")

	// The mention row has the full body.
	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: bystander.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, body, msgs.Items[0].Msg)
	assert.NotEqual(t, dmConvID, msgs.Items[0].ConversationID, "the bystander gets its own sender<->bystander conversation, not the primary's DM")

	// nit 8 (impl review r1): assert the bystander's stub names its OWN
	// message ID explicitly — not just "differs from the primary's stub",
	// which a bug that swapped in some other wrong-but-different ID would
	// also satisfy.
	assert.Contains(t, bystanderCalls[0].StructuredMessage.Msg, msgs.Items[0].ID,
		"the mention's stub must name its own persisted message ID")

	// C can fetch its own stub via its own conversation.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/"+msgs.Items[0].ConversationID+"/messages/"+msgs.Items[0].ID, nil)
	req = req.WithContext(agentContext(bystander.ID, bystander.ProjectID))
	rr2 := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr2, req, msgs.Items[0].ConversationID, msgs.Items[0].ID)
	require.Equal(t, http.StatusOK, rr2.Code, "body: %s", rr2.Body.String())
	var fetched store.Message
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &fetched))
	assert.Equal(t, offloadSHA256Hex(body), offloadSHA256Hex(fetched.Msg))
}

// nit 8: the /outbound-message variant of I9 — the #2083 fan-out reaches
// dispatch through the same ExecuteAgentDM call regardless of which HTTP
// entry point the primary used, so a mention from an outbound send must
// also get its own stub.
func TestOffload_I9_AgentMention_OutboundEndpoint_GetsOwnStub(t *testing.T) {
	srv, s, _, sender, _, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)
	enableOffload(t, srv, 4000, true)

	body := strings.Repeat("o", 12000) + " @" + mentionBystanderSlug
	rr := sendViaOutbound(t, srv, sender, dmConvID, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	bystanderCalls := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, bystanderCalls, 1, "@%s must be fanned out exactly once", mentionBystanderSlug)
	assert.NotEqual(t, body, bystanderCalls[0].StructuredMessage.Msg, "the mention recipient must get its own stub, not the full body")

	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: bystander.ID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, body, msgs.Items[0].Msg)
	assert.Contains(t, bystanderCalls[0].StructuredMessage.Msg, msgs.Items[0].ID)
}

// nit 8: the verified-group-conversation shape — a mention posted into an
// existing, caller-referenced group conversation lands the mention row in
// THAT group conversation (not a fresh sender<->mentioned DM), and the
// mention's stub must therefore name the group conversation's own ID.
func TestOffload_I9_AgentMention_VerifiedGroupConversation_GetsOwnStub(t *testing.T) {
	srv, s, project, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)
	enableOffload(t, srv, 4000, true)
	// Group conversation creation requires a wired webChatStore (503
	// otherwise); this also rewires the message broker proxy onto the same
	// recordingDispatcher, matching TestMentionFanout_AgentToGroupConversationOutboundConvRef_BodyMentionFannedOut's
	// setup for this exact shape.
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	grantAgentProjectAccess(t, s, sender.ID, project.ID)

	createBytes, err := json.Marshal(createConversationRequest{
		DisplayName: "offload-i9-group",
		ProjectID:   project.ID,
		Kind:        "group",
	})
	require.NoError(t, err)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(sender.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())
	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))

	body := strings.Repeat("g", 12000) + " @" + mentionBystanderSlug
	sendRR := postConvRefNoRecipient(t, srv, project.ID, sender.ID, body, "conv:"+created.ID)
	require.Equal(t, http.StatusOK, sendRR.Code, "body: %s", sendRR.Body.String())

	bystanderCalls := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, bystanderCalls, 1, "@%s must be fanned out exactly once", mentionBystanderSlug)
	assert.NotEqual(t, body, bystanderCalls[0].StructuredMessage.Msg, "the mention recipient must get its own stub, not the full body")
	assert.Equal(t, created.ID, bystanderCalls[0].StructuredMessage.ConversationID,
		"the mention row must carry the verified group conversation's own id")
	assert.Contains(t, bystanderCalls[0].StructuredMessage.Msg, "conv:"+created.ID,
		"the mention's stub must name the group conversation, not a fresh DM")
}

// ---------------------------------------------------------------------------
// I10: sender deleted.
//
// pkg/store.Message.SenderProjectID/RecipientProjectID — the row stamp
// peerProjectFromRow depends on — are set by StampProvenance and persisted by
// pkg/store/entadapter (ptone/scion#2282), so a row written after #2282
// carries its peer's project even once the peer agent is hard-deleted, and
// enforceCrossProjectReadGate decides from the stamp with no live GetAgent
// call. Rows written before #2282 have a NULL stamp; for those,
// crossProjectPeerAllowed falls back to the live lookup, and a deleted peer
// is allowed only when crossProjectMessagingEnabled() is on (every possible
// peer project would be allowed anyway) and otherwise denied (403, never
// 500) — see TestOffload_I10_LegacyUnstampedRow_SenderDeleted_FlagOff_403NeverA500.
// ---------------------------------------------------------------------------

func offloadSendAndDeleteSender(t *testing.T, srv *Server, s store.Store, sender, target *store.Agent, dmConvID string, dispatcher *recordingDispatcher, body string) (msgID, wantSHA string) {
	t.Helper()
	rr := sendViaStructured(t, srv, sender, target, body)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	wantSHA = calls[0].StructuredMessage.Metadata[messaging.MetaBodySHA256]
	require.NotEmpty(t, wantSHA)

	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{ConversationID: dmConvID}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	for _, m := range msgs.Items {
		if m.Msg == body {
			msgID = m.ID
		}
	}
	require.NotEmpty(t, msgID)

	require.NoError(t, s.DeleteAgent(context.Background(), sender.ID)) // hard delete
	return msgID, wantSHA
}

func TestOffload_I10_SenderDeleted_CrossProjectFlagOn_Fetchable(t *testing.T) {
	srv, s, _, sender, target, dmConvID, dispatcher, _ := paritySetup(t)
	enableOffloadAndCPM(t, srv, 4000)

	body := strings.Repeat("s", 12000)
	msgID, wantSHA := offloadSendAndDeleteSender(t, srv, s, sender, target, dmConvID, dispatcher, body)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/"+dmConvID+"/messages/"+msgID, nil)
	req = req.WithContext(agentContext(target.ID, target.ProjectID))
	rr2 := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr2, req, dmConvID, msgID)
	require.Equal(t, http.StatusOK, rr2.Code,
		"with cross-project messaging on, a deleted peer's row must stay fetchable; body: %s", rr2.Body.String())

	var fetched store.Message
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &fetched))
	assert.Equal(t, wantSHA, offloadSHA256Hex(fetched.Msg))
}

// With cross-project messaging off, a same-project deleted sender's row is
// still fetchable: the persisted SenderProjectID stamp (ptone/scion#2282)
// proves the peer was same-project, so no live lookup is needed.
func TestOffload_I10_SenderDeleted_CrossProjectFlagOff_FetchableViaPersistedStamp(t *testing.T) {
	srv, s, _, sender, target, dmConvID, dispatcher, _ := paritySetup(t)
	enableOffload(t, srv, 4000, true) // cross_project_messaging_enabled stays at its compiled default (off)

	body := strings.Repeat("t", 12000)
	msgID, wantSHA := offloadSendAndDeleteSender(t, srv, s, sender, target, dmConvID, dispatcher, body)

	stored, err := s.GetMessage(context.Background(), msgID)
	require.NoError(t, err)
	require.NotNil(t, stored.SenderProjectID, "the sender's project stamp must be persisted (ptone/scion#2282)")
	assert.Equal(t, sender.ProjectID, *stored.SenderProjectID)
	require.NotNil(t, stored.RecipientProjectID)
	assert.Equal(t, target.ProjectID, *stored.RecipientProjectID)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/"+dmConvID+"/messages/"+msgID, nil)
	req = req.WithContext(agentContext(target.ID, target.ProjectID))
	rr2 := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr2, req, dmConvID, msgID)
	require.Equal(t, http.StatusOK, rr2.Code,
		"a same-project deleted peer's stamped row must stay fetchable with the flag off; body: %s", rr2.Body.String())

	var fetched store.Message
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &fetched))
	assert.Equal(t, wantSHA, offloadSHA256Hex(fetched.Msg))
}

// A legacy row written before ptone/scion#2282 has no stamp. With the sender
// hard-deleted and cross-project messaging off, its project can't be told
// apart from a cross-project one, so the read is denied — 403, never a 500.
func TestOffload_I10_LegacyUnstampedRow_SenderDeleted_FlagOff_403NeverA500(t *testing.T) {
	srv, s, _, sender, target, dmConvID, _, _ := paritySetup(t)
	enableOffload(t, srv, 4000, true) // cross_project_messaging_enabled stays at its compiled default (off)
	ctx := context.Background()

	legacy := &store.Message{
		ID: tid("offload-i10-legacy-row"), ProjectID: target.ProjectID,
		Sender: "agent:" + sender.Slug, SenderID: sender.ID, // no SenderProjectID: pre-#2282 row
		Recipient: "agent:" + target.Slug, RecipientID: target.ID,
		Msg: "legacy body", Type: messages.TypeInstruction, ConversationID: dmConvID,
	}
	require.NoError(t, s.CreateMessage(ctx, legacy))
	stored, err := s.GetMessage(ctx, legacy.ID)
	require.NoError(t, err)
	require.Nil(t, stored.SenderProjectID, "precondition: legacy row has no stamp")

	require.NoError(t, s.DeleteAgent(ctx, sender.ID)) // hard delete

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/"+dmConvID+"/messages/"+legacy.ID, nil)
	req = req.WithContext(agentContext(target.ID, target.ProjectID))
	rr := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr, req, dmConvID, legacy.ID)
	require.Equal(t, http.StatusForbidden, rr.Code,
		"deleted peer with no stamp must be 403, never 500, when cross-project messaging is off; body: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "peer agent not found")
}

// enableOffloadAndCPM sets both offload_threshold_runes and
// cross_project_messaging_enabled in one opsettings doc (enableCPM and
// enableOffload each overwrite the whole "messaging" section, so a test
// needing both must set them together).
func enableOffloadAndCPM(t *testing.T, srv *Server, threshold int) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := koanf.New(".")
	envK := koanf.New(".")
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	doc := []byte(`{"conversation_envelope_switch":true,"cross_project_messaging_enabled":true,"offload_threshold_runes":` +
		strconv.Itoa(threshold) + `}`)
	rev, err := ops.Update(context.Background(), "messaging", doc, "test", 0, "managed")
	require.NoError(t, err)
	require.Greater(t, rev, int64(0))
	srv.SetOperationalSettings(ops)
}

// TestOffload_I10_StampSurvivesDeletion_WhenStorePersistsIt isolates
// enforceCrossProjectReadGate (message-aware form)/peerProjectFromRow from
// the store by constructing the *store.Message directly. It proves the row's
// stamp authorizes the recipient even though the sender agent has been
// hard-deleted (design auto-offload-large-dm §4.3, I10). The end-to-end
// variant through the persisted stamp is
// TestOffload_I10_SenderDeleted_CrossProjectFlagOff_FetchableViaPersistedStamp.
func TestOffload_I10_StampSurvivesDeletion_WhenStorePersistsIt(t *testing.T) {
	// Same-project sender and recipient (paritySetup), with cross-project
	// messaging left at its compiled-default OFF — this isolates the
	// stamp-based path itself: it must authorize the same-project recipient
	// via the row's stamp alone, with no live GetAgent call and no need for
	// the cross-project flag.
	srv, s, _, senderAgent, targetAgent, dmConvID, _, _ := paritySetup(t)
	ctx := context.Background()

	conv, err := s.GetConversation(ctx, dmConvID)
	require.NoError(t, err)

	require.NoError(t, s.DeleteAgent(ctx, senderAgent.ID)) // sender hard-deleted

	senderProj := senderAgent.ProjectID
	msg := &store.Message{
		Sender: "agent:" + senderAgent.Slug, SenderID: senderAgent.ID, SenderProjectID: &senderProj,
		Recipient: "agent:" + targetAgent.Slug, RecipientID: targetAgent.ID,
	}

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(agentContext(targetAgent.ID, targetAgent.ProjectID))
	rr := httptest.NewRecorder()
	ok := srv.enforceCrossProjectReadGate(rr, req, conv, msg)
	assert.True(t, ok, "a stamped row must authorize the recipient even though the sender no longer exists")
}
