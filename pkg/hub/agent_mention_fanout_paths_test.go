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
// Agent-authored @mention fan-out, covering all four agent send paths: an
// agent's structured message to another agent, an outbound agent-to-agent
// DM, an outbound post into a group conversation, and an outbound message to
// a human user. Human native-chat sends (handlers_chat_v2.go
// sendAgentRouted) already fan out via resolveRoutingAgents; these tests
// cover the agent-authored side. Each TestMentionFanout_*_BodyMentionFannedOut
// test below sends an agent-authored message whose body contains a bystander
// mention and asserts the bystander agent receives exactly one TypeMention
// dispatch via fanOutAgentMentions.
//
// TestProcessMentions_ExplicitMentionsField_FansOut is a control: it shows
// processMentions (the executor for human/broker senders) working when a
// caller supplies MessageRequest.Mentions explicitly.
//
// Run: go test ./pkg/hub/ -run 'TestMentionFanout|TestProcessMentions' -count=1
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

const mentionBystanderSlug = "amf-bystander"

// mentionFanoutPollTimeout bounds waitForNonMentionMessages, matching the headroom
// handlers_outbound_def162_test.go uses for the same broker-async-persist
// class of race.
const mentionFanoutPollTimeout = 5 * time.Second

// waitForNonMentionMessages polls ListMessages(ConversationID, ExcludeType:
// mention) until it has at least want rows or the timeout elapses. The
// primary post into a group/user conversation via the broker path persists
// in the eventbus subscriber's goroutine, asynchronously relative to the
// HTTP response, so a bare single ListMessages call right after the request
// returns can race it.
func waitForNonMentionMessages(t *testing.T, s store.Store, conversationID string, want int) []store.Message {
	t.Helper()
	deadline := time.Now().Add(mentionFanoutPollTimeout)
	for {
		rows, err := s.ListMessages(context.Background(), store.MessageFilter{
			ConversationID: conversationID, ExcludeType: messages.TypeMention,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		if len(rows.Items) >= want || time.Now().After(deadline) {
			return rows.Items
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// enableConversationEnvelopeForMentionTests turns on the consolidated conversation
// envelope switch (writeDenyEnabled) so DeliveryText gets rendered — needed
// to assert on the rendered envelope's content. An empty fake settings store
// has no "messaging" section, which ConversationEnvelopeSwitch documents as
// defaulting to ON, but this makes the ON state explicit and independent of
// that default.
func enableConversationEnvelopeForMentionTests(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("messaging", json.RawMessage(`{}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("failed to refresh operational settings: %v", err)
	}
	srv.SetOperationalSettings(ops)
}

// mentionFanoutSetup reuses paritySetup (two same-project agents + DM conversation,
// recording dispatcher, broker proxy) and adds a third same-project agent,
// the bystander, which is only ever @mentioned in message bodies.
func mentionFanoutSetup(t *testing.T) (srv *Server, s store.Store, project *store.Project,
	sender, target, bystander *store.Agent, dmConvID string, dispatcher *recordingDispatcher) {
	t.Helper()
	srv, s, project, sender, target, dmConvID, dispatcher, _ = paritySetup(t)
	bystander = &store.Agent{
		ID:              tid("amf-bystander"),
		Name:            mentionBystanderSlug,
		Slug:            mentionBystanderSlug,
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        sender.Ancestry,
	}
	require.NoError(t, s.CreateAgent(context.Background(), bystander))
	return
}

func dispatchesTo(d *recordingDispatcher, agentID string) []dispatchCall {
	var out []dispatchCall
	for _, c := range d.getCalls() {
		if c.Agent != nil && c.Agent.ID == agentID {
			out = append(out, c)
		}
	}
	return out
}

func agentCtx(ctx context.Context, a *store.Agent) context.Context {
	return contextWithIdentity(ctx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: a.ID},
		ProjectID: a.ProjectID,
		Ancestry:  a.Ancestry,
	}})
}

// wireWebBrokerForMentionTests swaps in a production-shaped broker proxy with a "web"
// spoke and the project's user-message subscription, so agent→user and
// agent→group-conversation sends take the deliveryUserBroker path end to end
// (handler → PublishUserMessage → MessageBrokerProxy.deliverToUser). The
// dispatcher is the same recordingDispatcher, so any mention fan-out would
// be observed.
func wireWebBrokerForMentionTests(t *testing.T, srv *Server, s store.Store, projectID string, dispatcher *recordingDispatcher) {
	t.Helper()
	if srv.webChatStore == nil {
		dbProvider, ok := s.(interface{ DB() *sql.DB })
		require.True(t, ok)
		wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
		require.NoError(t, wcs.Init())
		srv.SetWebChatStore(wcs)
	}
	if old := srv.GetMessageBrokerProxy(); old != nil {
		old.Stop()
	}
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()
	proxy.subscribeProjectUserMessages(projectID)
}

// Path 1: agent → agent via POST /agents/{target}/message (handleAgentMessage
// → ExecuteAgentDM). This is what agent-mode `scion message @target "..."`
// uses (cmd/message.go sendMessageViaConversation, RefAgent branch — which,
// unlike the bare-slug sendMessageViaHub path, does no client-side
// sendMentionMessages either).
//
// fanOutAgentMentions detects the body mention and delivers exactly one
// TypeMention to the bystander, sent as the original agent sender.
func TestMentionFanout_AgentToAgentStructured_BodyMentionFannedOut(t *testing.T) {
	srv, s, _, sender, target, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)

	rr := sendViaStructured(t, srv, sender, target, "hey @"+mentionBystanderSlug+" can you take a look?")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	require.Len(t, dispatchesTo(dispatcher, target.ID), 1, "primary recipient must be dispatched")
	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "@%s in an agent-authored message body must be fanned out exactly once", mentionBystanderSlug)
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
	require.Equal(t, "agent:"+sender.Slug, got[0].StructuredMessage.Sender)

	// The mention row must never carry the sender<->target parent DM's key
	// or conversation id — the bystander is not a party to that DM. It gets
	// its own, fresh sender<->bystander conversation instead.
	senderTargetDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	require.NotEqual(t, senderTargetDMKey, got[0].StructuredMessage.ThreadID,
		"the mention row must not carry the sender<->target parent DM key")
	require.NotEqual(t, dmConvID, got[0].StructuredMessage.ConversationID,
		"the mention row must not carry the sender<->target parent DM's conversation id")

	senderBystanderDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
	require.NoError(t, err)
	bystanderConv, err := s.GetConversationByExternalRef(context.Background(), "native", senderBystanderDMKey)
	require.NoError(t, err, "the sender<->bystander DM conversation must have been created")
	require.Equal(t, bystanderConv.ID, got[0].StructuredMessage.ConversationID,
		"the mention row must carry its OWN sender<->bystander DM conversation id")

	// Not landing in a verified group: mention_source names the sender, not
	// the DM's own primary recipient.
	require.Equal(t, "agent:"+sender.Slug, got[0].StructuredMessage.Metadata["mention_source"])

	// mention_of names the primary message the mention was extracted from.
	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.MessageID, "sanity: the primary send must report its own message id")
	require.Equal(t, resp.MessageID, got[0].StructuredMessage.Metadata["mention_of"],
		"the mention's metadata must name the primary message it was extracted from")
}

// An agent sender's direct structured message can also carry a free-text
// thread_id (not a caller-referenced existing conversation_id), which the
// shared conversation-resolution block mints into a project-scoped group on
// the spot (DEF-138 Rules 2/3 applied to /message). A mention fanned out
// from a message like that must not carry that thread_id, or any key
// derived from it — fan-out falls back to its own fresh
// sender<->mentioned-agent DM instead, exactly as it does when there is no
// group context at all.
func TestMentionFanout_StructuredMessageFreeTextThreadIDNeverCopiedOntoMentionRow(t *testing.T) {
	// A "dm:agent:<target>:user:<sender>" case is deliberately absent here:
	// the /message ownership check now requires the authenticated principal
	// to actually be a user, not merely have a UUID that matches the "user"
	// slot (design agent-reincarnate §3.7 Amendment A25.7 R2, closing the
	// phantom "user:<agent-uuid>" participant row an agent sender could
	// otherwise write). An agent sender supplying any "dm:"-prefixed
	// thread_id is now denied (400) before fan-out ever runs, so that shape
	// is no longer a reachable input to this invariant — it is covered
	// instead by TestHandleAgentMessage_A257_R2_AgentSenderPhantomUserSlotDenied
	// (reincarnation_gate_r7_test.go), which asserts the denial itself.
	cases := []struct {
		name     string
		threadID func(sender, target *store.Agent) string
	}{
		{"an agent-prefixed identifier", func(sender, target *store.Agent) string {
			return "agent:" + target.ID
		}},
		{"a user-prefixed identifier", func(sender, target *store.Agent) string {
			return "user:someone@example.com"
		}},
		{"text equal to another project's real group conversation key", func(sender, target *store.Agent) string {
			return "thread:" + api.NewUUID() + ":some-topic"
		}},
		{"arbitrary caller-chosen text", func(sender, target *store.Agent) string {
			return "whatever-i-feel-like-typing"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

			threadID := tc.threadID(sender, target)
			sm := &messages.StructuredMessage{
				Version:     messages.Version,
				Timestamp:   time.Now().UTC().Format(time.RFC3339),
				Type:        messages.TypeInstruction,
				Sender:      "agent:" + sender.Slug,
				SenderID:    sender.ID,
				Recipient:   "agent:" + target.Slug,
				RecipientID: target.ID,
				Msg:         "hey @" + mentionBystanderSlug + " can you take a look?",
				ThreadID:    threadID,
				Channel:     "web",
			}
			reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(agentCtx(req.Context(), sender))
			rr := httptest.NewRecorder()
			srv.handleAgentMessage(rr, req, target.ID)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

			got := dispatchesTo(dispatcher, bystander.ID)
			require.Len(t, got, 1)
			require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)

			agentDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
			require.NoError(t, err)
			require.Equal(t, agentDMKey, got[0].StructuredMessage.ThreadID,
				"the mention must fall back to its own fresh sender<->mentioned-agent DM, not anything derived from the primary's thread_id")

			bystanderConv, err := s.GetConversationByExternalRef(context.Background(), "native", agentDMKey)
			require.NoError(t, err)
			require.Equal(t, bystanderConv.ID, got[0].StructuredMessage.ConversationID)
		})
	}
}

// Path 2: agent → agent via POST /agents/{sender}/outbound-message with
// conversation_ref=conv:<agent-agent DM> (handleAgentOutboundMessage →
// ExecuteAgentDM). The outbound DM branch calls fanOutAgentMentions after
// ExecuteAgentDM succeeds.
func TestMentionFanout_AgentToAgentOutboundConvRef_BodyMentionFannedOut(t *testing.T) {
	srv, s, _, sender, target, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)

	rr := sendViaOutbound(t, srv, sender, dmConvID, "looping in @"+mentionBystanderSlug+" here")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	require.Len(t, dispatchesTo(dispatcher, target.ID), 1, "primary recipient must be dispatched")
	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "@%s in an agent outbound DM body must be fanned out exactly once", mentionBystanderSlug)
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp["mention_results"], "outbound DM response must include mention_results")

	// Same non-reuse guarantee as the structured-message path above,
	// exercised through the outbound DM adapter this time.
	senderTargetDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)
	require.NotEqual(t, senderTargetDMKey, got[0].StructuredMessage.ThreadID,
		"the mention row must not carry the sender<->target parent DM key")
	require.NotEqual(t, dmConvID, got[0].StructuredMessage.ConversationID,
		"the mention row must not carry the sender<->target parent DM's conversation id")

	senderBystanderDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
	require.NoError(t, err)
	bystanderConv, err := s.GetConversationByExternalRef(context.Background(), "native", senderBystanderDMKey)
	require.NoError(t, err, "the sender<->bystander DM conversation must have been created")
	require.Equal(t, bystanderConv.ID, got[0].StructuredMessage.ConversationID,
		"the mention row must carry its OWN sender<->bystander DM conversation id")
}

// An outbound agent-to-agent DM whose body mentions the DM's own primary
// recipient must dispatch to that recipient exactly once (the primary
// delivery), not twice (primary plus a redundant mention) — this pins the
// wiring that passes the outbound DM branch's actual primary into fan-out's
// exclusion, not just that fanOutAgentMentions itself honours a Primary
// value when called directly.
func TestMentionFanout_OutboundDM_BodyMentioningThePrimaryDispatchesOnlyOnce(t *testing.T) {
	srv, _, _, sender, target, _, dmConvID, dispatcher := mentionFanoutSetup(t)

	rr := sendViaOutbound(t, srv, sender, dmConvID, "thanks @"+target.Slug+" appreciate it")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, target.ID)
	require.Len(t, got, 1, "the DM's own primary recipient must be dispatched exactly once, not once as primary and again as a mention")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	mentionResults, _ := resp["mention_results"].([]interface{})
	for _, r := range mentionResults {
		entry, _ := r.(map[string]interface{})
		require.NotEqual(t, target.Slug, entry["slug"], "the primary recipient must not also appear as a mention result")
	}
}

// Path 3: agent → native-chat group conversation via outbound-message with
// conversation_ref=conv:<group>. This is the `scion message conv:<id>` /
// `#thread` path and the one most visible in web chat: a human sees the
// agent's "@other-agent ..." post in the conversation, and other-agent is
// paged too.
//
// The user/group-conversation tail calls fanOutAgentMentions after the
// primary post succeeds; the mention lands in the SAME group conversation
// and the bystander is registered as a participant.
func TestMentionFanout_AgentToGroupConversationOutboundConvRef_BodyMentionFannedOut(t *testing.T) {
	srv, s, project, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)

	enableConversationEnvelopeForMentionTests(t, srv) // needed to assert on DeliveryText below.
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	grantAgentProjectAccess(t, s, sender.ID, project.ID)

	createBytes, _ := json.Marshal(createConversationRequest{
		DisplayName: "amf-group-conversation",
		ProjectID:   project.ID,
		Kind:        "group",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(sender.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())
	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))

	sendRR := postConvRefNoRecipient(t, srv, project.ID, sender.ID,
		"@"+mentionBystanderSlug+" please pick up the review", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, sendRR.Code, "body: %s", sendRR.Body.String())

	// Sanity: the message itself was persisted into the group conversation.
	var sendResp map[string]interface{}
	require.NoError(t, json.Unmarshal(sendRR.Body.Bytes(), &sendResp))
	msgID, _ := sendResp["message_id"].(string)
	require.NotEmpty(t, msgID)
	require.NotEmpty(t, sendResp["mention_results"], "group-conversation post response must include mention_results")

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "agent post into group conversation %s with @%s must be fanned out exactly once", created.ID, mentionBystanderSlug)
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
	require.Equal(t, created.ID, got[0].StructuredMessage.ConversationID,
		"the mention row must carry the group conversation's own conversation id")
	// ThreadID must be the group conversation's own external ref too, not
	// left empty or inherited from anywhere else.
	require.Equal(t, created.ExternalRef, got[0].StructuredMessage.ThreadID)
	// Landing in a verified group: mention_source names that group's own
	// reference too, not the sender and not anything caller-supplied.
	require.Equal(t, created.ExternalRef, got[0].StructuredMessage.Metadata["mention_source"])
	// The envelope (DeliveryText) must actually render as a mention naming
	// this conversation — ConversationID alone is not "the envelope".
	// RenderDeliveryText renders a MarshalIndent'd JSON envelope (indented
	// output puts a space after each colon), embedded in the dispatched
	// text.
	require.NotEmpty(t, got[0].StructuredMessage.DeliveryText, "write-deny is ON in this test; DeliveryText must render")
	require.Contains(t, got[0].StructuredMessage.DeliveryText, `"type": "mention"`,
		"the envelope must be rendered with IsMention=true, not as a plain message")
	require.Contains(t, got[0].StructuredMessage.DeliveryText, created.ID,
		"the envelope's conversation id must be the group conversation's")

	// The bystander is registered as a participant of the group conversation
	// after successful dispatch.
	participants, err := s.ListParticipants(context.Background(), created.ID)
	require.NoError(t, err)
	var foundBystander bool
	for _, p := range participants {
		if p.PrincipalKind == "agent" && p.PrincipalID == bystander.ID {
			foundBystander = true
		}
	}
	require.True(t, foundBystander, "the mentioned agent must be registered as a group conversation participant")

	// Exactly one non-mention row is visible in the group conversation's
	// history — the original post. The fan-out's own TypeMention row is
	// excluded from that view, so web chat shows one row per agent post.
	// The primary post's persistence happens in the broker's eventbus
	// subscriber callback, asynchronously relative to the HTTP response
	// (see handlers_outbound_def162_test.go's waitForBrokerMessage), so this
	// polls rather than asserting immediately.
	visible := waitForNonMentionMessages(t, s, created.ID, 1)
	require.Len(t, visible, 1, "exactly one non-mention row must be visible in the group conversation")
}

// Path 4: agent → human user via outbound-message with recipient=user:<email>
// (`scion message user:<email> "..."`). The reply lands in the human's DM;
// an @agent in it is forwarded via its own sender<->mentioned-agent DM.
//
// The mention row's conversation is the sender<->bystander agent DM, NOT the
// parent agent->user DM; a mention row never carries the parent's DM key.
func TestMentionFanout_AgentToUserOutbound_BodyMentionFannedOut(t *testing.T) {
	srv, s, project, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)

	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + owner.Email,
		Msg:       "done — @" + mentionBystanderSlug + " please deploy",
		Type:      "instruction",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "@%s in an agent->user message body must be fanned out exactly once", mentionBystanderSlug)
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)

	// The mention row's thread/conversation belongs to the sender<->bystander
	// agent DM, not the agent->user parent DM.
	agentDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
	require.NoError(t, err)
	require.Equal(t, agentDMKey, got[0].StructuredMessage.ThreadID)

	userDMKey, err := messages.DMConversationKey("agent", sender.ID, "user", owner.ID)
	require.NoError(t, err)
	require.NotEqual(t, userDMKey, got[0].StructuredMessage.ThreadID, "the mention row must never carry the parent agent->user DM key")

	// Not landing in a verified group: mention_source names the sender, not
	// the parent send's own recipient.
	require.Equal(t, "agent:"+sender.Slug, got[0].StructuredMessage.Metadata["mention_source"])
	require.NotEqual(t, "user:"+owner.Email, got[0].StructuredMessage.Metadata["mention_source"])

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp["mention_results"], "outbound user-recipient response must include mention_results")

	// Verify ConversationID too, and on the actual PERSISTED row, not just
	// the dispatch payload.
	bystanderConv, err := s.GetConversationByExternalRef(context.Background(), "native", agentDMKey)
	require.NoError(t, err, "the sender<->bystander DM conversation must have been created")
	userConv, err := s.GetConversationByExternalRef(context.Background(), "native", userDMKey)
	require.NoError(t, err, "the agent->user parent DM conversation must have been created")
	require.NotEqual(t, userConv.ID, bystanderConv.ID, "sanity: the two conversations must actually differ")

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{
		SenderID: sender.ID, RecipientID: bystander.ID, Type: messages.TypeMention,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	require.Equal(t, bystanderConv.ID, rows.Items[0].ConversationID,
		"the persisted mention row must carry the sender<->bystander DM's own conversation id")
	require.NotEqual(t, userConv.ID, rows.Items[0].ConversationID,
		"the persisted mention row must never carry the parent agent->user DM's conversation id")
}

// Mention extraction for fan-out must use the message body as the agent
// actually wrote it, before translateMentionsInbound rewrites a human's raw
// email into their display-name slug for human-facing display. Here that
// display-name slug happens to collide with a real agent's slug: extracting
// from the translated body would wrongly page the agent, but the human's
// raw email in the original body resolves to no agent at all.
func TestMentionFanout_UsesPreTranslationBodyForMentionExtraction(t *testing.T) {
	srv, s, project, sender, _, _, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	builderAgent := &store.Agent{
		ID: tid("mention-fanout-pretranslate-agent"), Name: "builder", Slug: "builder",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: sender.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, builderAgent))

	builderHuman := &store.User{
		ID: tid("mention-fanout-pretranslate-human"), Email: "builder-human@test.example", DisplayName: "Builder",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, builderHuman))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      builderHuman.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	// The body mentions the human by raw email. translateMentionsInbound
	// would rewrite "@builder-human@test.example" to "@builder" (the
	// human's display-name slug) for the persisted/displayed message — which
	// is exactly the "builder" agent's own slug.
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + owner.Email,
		Msg:       "cc @" + builderHuman.Email + " thanks",
		Type:      "instruction",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentCtx(req.Context(), sender))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	require.Empty(t, dispatchesTo(dispatcher, builderAgent.ID),
		"a human's raw-email mention must not fan out to an agent whose slug matches that human's translated display-name slug")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Empty(t, resp["mention_results"], "the raw-email human mention must produce no mention result at all")
}

// An agent sender's outbound message can carry a free-text thread_id that
// resolves to an existing project-scoped group conversation via DEF-138
// Rules 2/3, without the caller asserting it.
// A mention fanned out from a message like that must not carry that
// thread_id, or any key derived from it, onto the mentioned agent's row:
// fan-out falls back to its own fresh sender<->mentioned-agent DM, exactly
// as it does for a non-group parent, rather than trusting a group context it
// wasn't independently given a verified reference to.
func TestMentionFanout_FreeTextThreadIDNeverCopiedOntoMentionRow(t *testing.T) {
	cases := []struct {
		name     string
		threadID func(sender *store.Agent, ownerID string) string
	}{
		{"a DM key naming the sender and the primary recipient", func(sender *store.Agent, ownerID string) string {
			// The outbound ownership check (parseDMKeyIDs) only recognizes
			// "dm:agent:<id>:user:<id>" and requires the agent side to be
			// the sender and the user side to be the resolved recipient —
			// this is the only dm:-shaped value this endpoint accepts here;
			// anything else is rejected (400) before fan-out ever runs.
			return "dm:agent:" + sender.ID + ":user:" + ownerID
		}},
		{"an agent-prefixed identifier", func(sender *store.Agent, ownerID string) string {
			return "agent:" + sender.ID
		}},
		{"a user-prefixed identifier", func(sender *store.Agent, ownerID string) string {
			return "user:someone@example.com"
		}},
		{"text equal to another project's real group conversation key", func(sender *store.Agent, ownerID string) string {
			return "thread:" + api.NewUUID() + ":some-topic"
		}},
		{"arbitrary caller-chosen text", func(sender *store.Agent, ownerID string) string {
			return "whatever-i-feel-like-typing"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, sender, _, bystander, _, dispatcher := mentionFanoutSetup(t)

			wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
			owner, err := s.GetUser(context.Background(), project.OwnerID)
			require.NoError(t, err)

			threadID := tc.threadID(sender, owner.ID)
			if !strings.HasPrefix(threadID, "dm:") {
				// ptone/scion#2026: a free-text thread_id must name an
				// existing thread conversation, or the hub rejects the send
				// before fan-out. Seed it so the send reaches fan-out; the
				// assertion below is about the mention row, not the thread.
				seedThreadConversation(t, s, project.ID, threadID)
			}
			body, _ := json.Marshal(OutboundMessageRequest{
				Recipient: "user:" + owner.Email,
				Msg:       "done — @" + mentionBystanderSlug + " please deploy",
				Type:      "instruction",
				ThreadID:  threadID,
				Channel:   "web",
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(agentCtx(req.Context(), sender))
			rr := httptest.NewRecorder()
			srv.handleAgentOutboundMessage(rr, req, sender.ID)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

			got := dispatchesTo(dispatcher, bystander.ID)
			require.Len(t, got, 1)
			require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)

			agentDMKey, err := messages.DMConversationKey("agent", sender.ID, "agent", bystander.ID)
			require.NoError(t, err)
			require.Equal(t, agentDMKey, got[0].StructuredMessage.ThreadID,
				"the mention must fall back to its own fresh sender<->mentioned-agent DM, same as any other non-verified-group parent")

			bystanderConv, err := s.GetConversationByExternalRef(context.Background(), "native", agentDMKey)
			require.NoError(t, err)

			rows, err := s.ListMessages(context.Background(), store.MessageFilter{
				SenderID: sender.ID, RecipientID: bystander.ID, Type: messages.TypeMention,
			}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, rows.Items, 1)
			require.Equal(t, agentDMKey, rows.Items[0].ThreadID,
				"the persisted mention row must carry only the fresh sender<->mentioned-agent DM key")
			require.Equal(t, bystanderConv.ID, rows.Items[0].ConversationID)
		})
	}
}

// Control: a HUMAN sender (not an agent) posts to the target agent with the
// explicit MessageRequest.Mentions field populated. Human senders take a
// different branch of handleAgentMessage than agents do, one that still
// calls processMentions rather than fanOutAgentMentions, so this pins that
// pre-existing explicit-mentions machinery independently of everything else
// in this file, which is agent-sender-only.
func TestProcessMentions_ExplicitMentionsField_FansOut(t *testing.T) {
	srv, s, project, _, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	// The project owner is a human sender (not an agent) that is always
	// authorized to message any agent in their own project.
	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	sm := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Recipient: "agent:" + target.Slug,
		Msg:       "hey @" + mentionBystanderSlug + " can you take a look?",
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{mentionBystanderSlug}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1, "explicit Mentions field must fan out to the bystander")
	require.NotNil(t, got[0].StructuredMessage)
	require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
	require.Equal(t, "user:"+owner.Email, got[0].StructuredMessage.Sender)
}

// A human sender's explicit mention must not carry the primary send's own
// thread_id onto the mentioned agent's row, no matter what shape that value
// takes: a DM key, an identifier that looks like an agent or user address, a
// string that happens to equal another project's real group conversation
// key, or plain arbitrary text. A mention row gets a thread key only from a
// verified group conversation, never from this source.
func TestProcessMentions_ThreadIDNeverCopiedOntoMentionRow(t *testing.T) {
	dmKeyOwnerTarget := func(t *testing.T, s store.Store, project *store.Project, target *store.Agent) string {
		t.Helper()
		owner, err := s.GetUser(context.Background(), project.OwnerID)
		require.NoError(t, err)
		dmKey, err := messages.DMConversationKey("user", owner.ID, "agent", target.ID)
		require.NoError(t, err)
		return dmKey
	}

	cases := []struct {
		name     string
		threadID func(t *testing.T, s store.Store, project *store.Project, target *store.Agent) string
	}{
		{"a DM key naming the sender and the primary recipient", dmKeyOwnerTarget},
		{"an agent-prefixed identifier", func(t *testing.T, s store.Store, project *store.Project, target *store.Agent) string {
			return "agent:" + target.ID
		}},
		{"a user-prefixed identifier", func(t *testing.T, s store.Store, project *store.Project, target *store.Agent) string {
			return "user:someone@example.com"
		}},
		{"text equal to another project's real group conversation key", func(t *testing.T, s store.Store, project *store.Project, target *store.Agent) string {
			foreignProjectID := api.NewUUID()
			foreignProject := &store.Project{ID: foreignProjectID, Name: "foreign", Slug: "foreign-" + foreignProjectID[:8]}
			require.NoError(t, s.CreateProject(context.Background(), foreignProject))
			foreignGroup := &store.Conversation{
				ID: api.NewUUID(), Kind: "group", Surface: "native",
				ExternalRef: "thread:" + foreignProjectID + ":some-topic",
				ProjectID:   &foreignProjectID, DriftState: "active",
			}
			require.NoError(t, s.CreateConversation(context.Background(), foreignGroup))
			return foreignGroup.ExternalRef
		}},
		{"arbitrary caller-chosen text", func(t *testing.T, s store.Store, project *store.Project, target *store.Agent) string {
			return "whatever-i-feel-like-typing"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, _, target, bystander, _, dispatcher := mentionFanoutSetup(t)
			ctx := context.Background()
			owner, err := s.GetUser(ctx, project.OwnerID)
			require.NoError(t, err)

			sm := &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Type:      messages.TypeInstruction,
				Recipient: "agent:" + target.Slug,
				Msg:       "hey @" + mentionBystanderSlug + " can you take a look?",
				ThreadID:  tc.threadID(t, s, project, target),
				Channel:   "web",
			}
			reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{mentionBystanderSlug}})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
			rr := httptest.NewRecorder()
			srv.handleAgentMessage(rr, req, target.ID)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

			got := dispatchesTo(dispatcher, bystander.ID)
			require.Len(t, got, 1)
			require.Equal(t, messages.TypeMention, got[0].StructuredMessage.Type)
			require.Empty(t, got[0].StructuredMessage.ThreadID,
				"the mentioned agent's dispatched row must not carry the primary send's thread_id")

			rows, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: bystander.ID, Type: messages.TypeMention}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			require.Len(t, rows.Items, 1)
			require.Empty(t, rows.Items[0].ThreadID,
				"the persisted mention row must not carry the primary send's thread_id either")
		})
	}
}

// When the primary message's conversation is a group conversation the caller
// referenced by its existing, already-authorized ID (not one derived on the
// spot from free-text thread_id), a mention into it is a genuine invitation
// into a real shared space — this is the case processMentions still carries
// a thread key for, using the verified conversation's own key rather than
// anything the caller put in thread_id.
func TestProcessMentions_VerifiedGroupConversationThreadKeyIsCarried(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	grantAgentProjectAccess(t, s, sender.ID, project.ID)
	createBytes, _ := json.Marshal(createConversationRequest{
		DisplayName: "verified-group",
		ProjectID:   project.ID,
		Kind:        "group",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(createBytes))
	createReq.Header.Set("Content-Type", "application/json")
	createReq = createReq.WithContext(agentContextWithScopes(sender.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	createRR := httptest.NewRecorder()
	srv.handleCreateConversation(createRR, createReq)
	require.Equal(t, http.StatusCreated, createRR.Code, "body: %s", createRR.Body.String())
	var created conversationResponse
	require.NoError(t, json.Unmarshal(createRR.Body.Bytes(), &created))
	require.NotEmpty(t, created.ExternalRef, "sanity: a native group conversation must mint a real external_ref")

	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Type:           messages.TypeInstruction,
		Recipient:      "agent:" + target.Slug,
		Msg:            "hey @" + mentionBystanderSlug + " can you take a look?",
		ConversationID: created.ID,
		Channel:        "web",
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{mentionBystanderSlug}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1)
	require.Equal(t, created.ExternalRef, got[0].StructuredMessage.ThreadID,
		"a mention into a verified, caller-referenced group conversation must carry that group's own thread key")

	participants, err := s.ListParticipants(ctx, created.ID)
	require.NoError(t, err)
	var foundBystander bool
	for _, p := range participants {
		if p.PrincipalKind == "agent" && p.PrincipalID == bystander.ID {
			foundBystander = true
		}
	}
	require.True(t, foundBystander, "a mention into a verified group conversation must register the mentioned agent as a participant")
}

// A mention into a group conversation that was minted on the spot from
// free-text thread_id (not one the caller referenced by an existing ID) gets
// no thread key (see TestProcessMentions_ThreadIDNeverCopiedOntoMentionRow)
// and, consistently, is not registered as a participant of that group
// either — registration and thread key follow the same verified-or-not rule.
func TestProcessMentions_UnverifiedGroupConversationDoesNotRegisterParticipant(t *testing.T) {
	srv, s, project, _, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	sm := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Recipient: "agent:" + target.Slug,
		Msg:       "hey @" + mentionBystanderSlug + " can you take a look?",
		ThreadID:  "some-ad-hoc-topic",
		Channel:   "web",
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{mentionBystanderSlug}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1)
	require.Empty(t, got[0].StructuredMessage.ThreadID)

	// Find the ad-hoc group conversation the primary landed in, and confirm
	// the mentioned agent was NOT added as one of its participants.
	adHocExtRef, err := messaging.ThreadConversationExternalRef(target.ProjectID, "some-ad-hoc-topic")
	require.NoError(t, err)
	adHocConv, err := s.GetConversationByExternalRef(ctx, "native", adHocExtRef)
	require.NoError(t, err, "sanity: the ad-hoc group conversation must have been minted")

	participants, err := s.ListParticipants(ctx, adHocConv.ID)
	require.NoError(t, err)
	for _, p := range participants {
		require.False(t, p.PrincipalKind == "agent" && p.PrincipalID == bystander.ID,
			"the mentioned agent must not be registered as a participant of an unverified, ad-hoc group conversation")
	}
}

// Same as TestProcessMentions_UnverifiedGroupConversationDoesNotRegisterParticipant,
// but through the managed-runtime primary's own processMentions call site —
// a separate call site with its own gating argument.
func TestProcessMentions_ManagedRuntime_UnverifiedGroupConversationDoesNotRegisterParticipant(t *testing.T) {
	srv, s, project, _, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	managedBackendMu.Lock()
	prevBackend := managedBackendInst
	managedBackendInst = stubManagedAgentBackend{}
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prevBackend
		managedBackendMu.Unlock()
	})
	target.Runtime = ManagedRuntimePrefix + "stub"
	require.NoError(t, s.UpdateAgent(ctx, target))

	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	sm := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Recipient: "agent:" + target.Slug,
		Msg:       "hey @" + mentionBystanderSlug + " can you take a look?",
		ThreadID:  "some-ad-hoc-topic-managed",
		Channel:   "web",
	}
	reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm, Mentions: []string{mentionBystanderSlug}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(owner.ID, owner.Email, "Owner", "member", "cli")))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, target.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := dispatchesTo(dispatcher, bystander.ID)
	require.Len(t, got, 1)
	require.Empty(t, got[0].StructuredMessage.ThreadID)

	adHocExtRef, err := messaging.ThreadConversationExternalRef(target.ProjectID, "some-ad-hoc-topic-managed")
	require.NoError(t, err)
	adHocConv, err := s.GetConversationByExternalRef(ctx, "native", adHocExtRef)
	require.NoError(t, err, "sanity: the ad-hoc group conversation must have been minted")

	participants, err := s.ListParticipants(ctx, adHocConv.ID)
	require.NoError(t, err)
	for _, p := range participants {
		require.False(t, p.PrincipalKind == "agent" && p.PrincipalID == bystander.ID,
			"the mentioned agent must not be registered as a participant of an unverified, ad-hoc group conversation, via the managed-runtime primary's own processMentions call site")
	}
}

// The type gate (mentionFanoutTypes) is unit-tested directly against
// fanOutAgentMentions, but each HTTP call site must actually pass the real
// parent type through rather than defaulting or hard-coding it. This sends
// an excluded parent type through the outbound DM branch and the /message
// agent fork and asserts neither fans out.
func TestMentionFanout_ExcludedTypeAtWiringLevelDoesNotFanOut(t *testing.T) {
	cases := []struct {
		name    string
		msgType string
	}{
		{"assistant_reply", messages.TypeAssistantReply},
		{"mention", messages.TypeMention},
	}

	for _, tc := range cases {
		t.Run("outbound_dm/"+tc.name, func(t *testing.T) {
			srv, _, _, sender, _, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)

			reqBody, _ := json.Marshal(OutboundMessageRequest{
				ConversationRef: "conv:" + dmConvID,
				Msg:             "excluded type outbound test @" + bystander.Slug,
				Type:            tc.msgType,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(agentCtx(req.Context(), sender))
			rr := httptest.NewRecorder()
			srv.handleAgentOutboundMessage(rr, req, sender.ID)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

			require.Empty(t, dispatchesTo(dispatcher, bystander.ID),
				"an excluded parent type must not fan out through the outbound DM branch")
		})

		t.Run("message_fork/"+tc.name, func(t *testing.T) {
			srv, _, _, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)

			sm := &messages.StructuredMessage{
				Version:     messages.Version,
				Timestamp:   time.Now().UTC().Format(time.RFC3339),
				Type:        tc.msgType,
				Sender:      "agent:" + sender.Slug,
				SenderID:    sender.ID,
				Recipient:   "agent:" + target.Slug,
				RecipientID: target.ID,
				Msg:         "excluded type message-fork test @" + bystander.Slug,
			}
			reqBody, _ := json.Marshal(MessageRequest{StructuredMessage: sm})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(agentCtx(req.Context(), sender))
			rr := httptest.NewRecorder()
			srv.handleAgentMessage(rr, req, target.ID)
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

			require.Empty(t, dispatchesTo(dispatcher, bystander.ID),
				"an excluded parent type must not fan out through the /message agent fork")
		})
	}
}

// A mention rejected by the sender's own rate budget must not fail the
// primary delivery it rode in on. The primary and every mention share one
// token bucket, so with the bucket sized to fit exactly the primary plus one
// mention, a second mention in the same body is rate-limited — and the
// primary must still be reported as dispatched, with exactly one dispatch to
// it, regardless. This exercises the outbound DM branch end to end (HTTP
// response plus dispatcher), not just fanOutAgentMentions in isolation,
// which never touches the primary at all and so cannot prove this on its
// own.
func TestMentionFanout_OutboundDM_RateLimitedMentionDoesNotFailPrimary(t *testing.T) {
	srv, s, project, sender, target, bystander, dmConvID, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	second := &store.Agent{
		ID: tid("mention-fanout-ratelimit-outbound-second"), Name: "second-mention", Slug: "second-mention",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, second))

	// Exactly 2 agent-class sends per minute: the primary consumes one,
	// leaving exactly one more for the first mention. The second mention
	// finds the bucket empty.
	fakeNow := time.Now()
	srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman:       chatSendHumanRatePerMinute,
		chatSenderAgent:       2,
		chatSenderAgentMirror: chatSendAgentMirrorRatePerMinute,
	}, func() time.Time { return fakeNow })

	rr := sendViaOutbound(t, srv, sender, dmConvID, "thanks @"+bystander.Slug+" and @"+second.Slug)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var resp struct {
		Status         string                   `json:"status"`
		MentionResults []messages.MentionResult `json:"mention_results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "dispatched", resp.Status, "the primary must be reported as dispatched, not failed by a mention's rate limit")
	require.Len(t, resp.MentionResults, 2)
	require.Equal(t, "delivered", resp.MentionResults[0].Status)
	require.Equal(t, "rate_limited", resp.MentionResults[1].Status)

	require.Len(t, dispatchesTo(dispatcher, target.ID), 1, "the primary must be dispatched exactly once")
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)
	require.Empty(t, dispatchesTo(dispatcher, second.ID))
}

// Same as above, through the /message agent fork instead of the outbound DM
// branch: a mention rate-limited by the shared token bucket must not fail
// the primary /message delivery.
func TestMentionFanout_MessageFork_RateLimitedMentionDoesNotFailPrimary(t *testing.T) {
	srv, s, project, sender, target, bystander, _, dispatcher := mentionFanoutSetup(t)
	ctx := context.Background()

	second := &store.Agent{
		ID: tid("mention-fanout-ratelimit-message-second"), Name: "second-mention", Slug: "second-mention",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: target.RuntimeBrokerID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, second))

	fakeNow := time.Now()
	srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman:       chatSendHumanRatePerMinute,
		chatSenderAgent:       2,
		chatSenderAgentMirror: chatSendAgentMirrorRatePerMinute,
	}, func() time.Time { return fakeNow })

	rr := sendViaStructured(t, srv, sender, target, "thanks @"+bystander.Slug+" and @"+second.Slug)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var resp struct {
		Status         string                   `json:"status"`
		MentionResults []messages.MentionResult `json:"mention_results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "dispatched", resp.Status, "the primary must be reported as dispatched, not failed by a mention's rate limit")
	require.Len(t, resp.MentionResults, 2)
	require.Equal(t, "delivered", resp.MentionResults[0].Status)
	require.Equal(t, "rate_limited", resp.MentionResults[1].Status)

	require.Len(t, dispatchesTo(dispatcher, target.ID), 1, "the primary must be dispatched exactly once")
	require.Len(t, dispatchesTo(dispatcher, bystander.ID), 1)
	require.Empty(t, dispatchesTo(dispatcher, second.ID))
}
