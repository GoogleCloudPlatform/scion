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

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DEF-158: conv:<uuid> with no explicit recipient — Channel and ThreadID
// must be set so the message is visible on the read path and the recipient
// gets a signal (notification + watermark).
// ---------------------------------------------------------------------------

// def158BrokerSetup extends def138Setup with a broker, WebChatStore,
// read-switch, and reply-affinity seeding. Returns the DM conversation
// and key for a direct conversation between the agent and user.
func def158BrokerSetup(t *testing.T) (
	srv *Server,
	s store.Store,
	wcs WebChatStore,
	project *store.Project,
	agent *store.Agent,
	user *store.User,
	dmConv *store.Conversation,
	dmKey string,
) {
	t.Helper()
	srv, s, project, agent, user = def138Setup(t)
	ctx := context.Background()

	// WebChatStore — also sets up ChatNotifier.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	wcs = NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Broker with "web" spoke — required for channel validation and
	// affinity to run (the guard at :238 checks GetMessageBrokerProxy != nil).
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	// Wire the WebChatStore and ChatNotifier into the broker.
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	proxy.chatNotifier = srv.chatNotifier
	srv.mu.RUnlock()

	// Subscribe the project so deliverToUser actually fires.
	proxy.subscribeProjectUserMessages(project.ID)

	// Enable the read-switch so handleConversationHistory uses
	// ConversationID+Channel:"web" (the filter the defect targets).
	enableReadSwitch(t, srv)

	// Create the DM conversation.
	dmKey, err = messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	dmConv, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Seed reply-affinity: the user's last channel for this agent is "web".
	require.NoError(t, wcs.RecordChannel(ctx, user.ID, project.ID, agent.ID, "web", time.Now()))

	return srv, s, wcs, project, agent, user, dmConv, dmKey
}

// postConvRefNoRecipient sends an outbound message with conversation_ref
// but NO explicit recipient — the exact shape the CLI produces.
func postConvRefNoRecipient(t *testing.T, srv *Server, projectID, agentID, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/outbound-message",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// readConversationHistoryAsUser calls GET /api/v1/chat/conversations/{key}/messages
// authenticated as the given user, returning the decoded response.
func readConversationHistoryAsUser(t *testing.T, srv *Server, user *store.User, key string) (int, chatHistoryResponse) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/chat/conversations/"+key+"/messages", nil)
	var resp chatHistoryResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, resp
}

// ---------------------------------------------------------------------------
// AC-1: conv:<uuid> direct, no recipient → non-empty Channel persisted.
// AC-2: same message returned by handleConversationHistory.
// ---------------------------------------------------------------------------

func TestDEF158_AC1_AC2_DirectConv_NoRecipient_ChannelSetAndVisible(t *testing.T) {
	srv, s, _, project, agent, user, dmConv, dmKey := def158BrokerSetup(t)
	ctx := context.Background()

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"hello from conv-ref", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"conv-ref direct with no recipient must succeed; body: %s", rr.Body.String())

	// Decode the response to get the message ID.
	var resp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.MessageID)

	// Give the broker goroutine time to persist.
	var stored *store.Message
	require.Eventually(t, func() bool {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			ConversationID: dmConv.ID,
		}, store.ListOptions{Limit: 10})
		if err != nil || len(msgs.Items) == 0 {
			return false
		}
		for i := range msgs.Items {
			if msgs.Items[i].Msg == "hello from conv-ref" {
				stored = &msgs.Items[i]
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "message not persisted within timeout")

	// AC-1: Channel must be non-empty.
	assert.NotEmpty(t, stored.Channel, "AC-1: Channel must be set on conv-ref path")
	assert.Equal(t, "web", stored.Channel, "AC-1: Channel should be 'web' from reply affinity")

	// AC-1 supplementary: ThreadID must be the DM key.
	assert.Equal(t, dmKey, stored.ThreadID, "ThreadID must be the DM key")

	// AC-2: the message must be returned by handleConversationHistory.
	// The read-switch is ON, so the filter is Channel:"web" + ConversationID.
	// This is the assertion the brief requires — not a store query.
	// Authenticate as the recipient (user), who is a participant in the DM.
	code, histResp := readConversationHistoryAsUser(t, srv, user, dmKey)
	require.Equal(t, http.StatusOK, code,
		"AC-2: conversation history read must succeed for DM participant")

	found := false
	for _, m := range histResp.Messages {
		if m.Msg == "hello from conv-ref" {
			found = true
			assert.Equal(t, "web", m.Channel, "AC-2: message in history must have Channel='web'")
			assert.Equal(t, user.ID, m.RecipientID, "AC-2: recipientID must be derived from DM key")
			break
		}
	}
	require.True(t, found, "AC-2: message must be visible in handleConversationHistory")
}

// ---------------------------------------------------------------------------
// Surface fallback: conv:<uuid> direct, no recipient, NO affinity row.
// The channel must still be derived from the conversation surface ("native"
// → "web" via SurfaceToChannel), not left empty (the pre-fix defect).
// This is the primary fix target — the affinity-hit case is a bonus.
// ---------------------------------------------------------------------------

func TestDEF158_SurfaceFallback_NoAffinity_ChannelDerivedFromSurface(t *testing.T) {
	// Same as def158BrokerSetup but WITHOUT seeding reply-affinity.
	// The surface fallback must be the sole source of Channel.
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	proxy.chatNotifier = srv.chatNotifier
	srv.mu.RUnlock()
	proxy.subscribeProjectUserMessages(project.ID)

	enableReadSwitch(t, srv)

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// NO RecordChannel call — affinity is empty.

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"hello no affinity", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"surface fallback must succeed; body: %s", rr.Body.String())

	// Wait for broker to persist.
	var stored *store.Message
	require.Eventually(t, func() bool {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			ConversationID: dmConv.ID,
		}, store.ListOptions{Limit: 10})
		if err != nil || len(msgs.Items) == 0 {
			return false
		}
		for i := range msgs.Items {
			if msgs.Items[i].Msg == "hello no affinity" {
				stored = &msgs.Items[i]
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "message not persisted within timeout")

	// Channel must be "web" — derived from Surface:"native" via SurfaceToChannel.
	// Precedent: sendAgentRouted (handlers_chat_v2.go:1059) writes Channel:"web"
	// for native-surface conversations.
	assert.Equal(t, "web", stored.Channel,
		"surface fallback: Channel must be 'web' for native-surface conversation")
	assert.Equal(t, dmKey, stored.ThreadID,
		"surface fallback: ThreadID must still be the DM key")

	// Visible on read path (Channel:"web" matches the filter).
	code, histResp := readConversationHistoryAsUser(t, srv, user, dmKey)
	require.Equal(t, http.StatusOK, code)
	found := false
	for _, m := range histResp.Messages {
		if m.Msg == "hello no affinity" {
			found = true
			assert.Equal(t, "web", m.Channel)
			break
		}
	}
	require.True(t, found,
		"surface fallback: message must be visible in history via Channel:'web' filter")
}

// ---------------------------------------------------------------------------
// AC-3: conv:<uuid> group, no recipient → 400 unchanged.
// ---------------------------------------------------------------------------

func TestDEF158_AC3_GroupConv_NoRecipient_StillRejected(t *testing.T) {
	srv, s, _, project, agent, _, _, _ := def158BrokerSetup(t)
	ctx := context.Background()

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d158-group-ac3",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d158-group-ac3",
	})
	require.NoError(t, err)

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"should fail", "conv:"+conv.ID)
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"AC-3: group conv with no explicit recipient must be rejected")
	assert.Contains(t, rr.Body.String(), "group conversations require an explicit recipient")
}

// ---------------------------------------------------------------------------
// AC-4: invalid channel rejected on the conv-ref path (R2).
// ---------------------------------------------------------------------------

func TestDEF158_AC4_InvalidChannel_Rejected_ConvRefPath(t *testing.T) {
	srv, s, wcs, project, agent, user, dmConv, _ := def158BrokerSetup(t)
	ctx := context.Background()

	// Seed affinity with a channel that is NOT registered with the broker.
	require.NoError(t, wcs.RecordChannel(ctx, user.ID, project.ID, agent.ID, "nonexistent-spoke", time.Now()))

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"should fail validation", "conv:"+dmConv.ID)

	// The channel affinity re-run should pick up "nonexistent-spoke",
	// and the validation re-run should reject it.
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"AC-4: unregistered channel from affinity must be rejected; body: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "nonexistent-spoke")

	// Verify no message was persisted.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{
		ConversationID: dmConv.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	for _, m := range msgs.Items {
		if m.Msg == "should fail validation" {
			t.Fatal("AC-4: message must NOT be persisted when channel validation fails")
		}
	}
}

// ---------------------------------------------------------------------------
// AC-6: TouchDMActivity and NotifyDMReceived fire for the conv-ref direct
// case. Behavioural assertion, not field inspection.
// ---------------------------------------------------------------------------

func TestDEF158_AC6_Notification_And_Watermark_Fire(t *testing.T) {
	srv, s, wcs, project, agent, user, dmConv, dmKey := def158BrokerSetup(t)
	ctx := context.Background()

	// Register DM participants so TouchDMActivity has rows to update.
	registerDMParticipants(ctx, wcs, dmKey)

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"signal test", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"send must succeed; body: %s", rr.Body.String())

	// C2: prove the watermark fires — TouchDMActivity updates
	// webchat_dm.last_message_id. Check behaviourally via ListDMs, not by
	// field inspection of ThreadID.
	require.Eventually(t, func() bool {
		dms, err := wcs.ListDMs(ctx, user.ID)
		if err != nil {
			return false
		}
		for _, dm := range dms {
			if dm.ConversationKey == dmKey && dm.LastMessageID != "" {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond,
		"AC-6: TouchDMActivity did not fire — no DM watermark for user+dmKey")

	// C2: prove NotifyDMReceived fires — it creates a notification in the
	// store. The goroutine needs a moment to complete.
	require.Eventually(t, func() bool {
		notifs, err := s.GetNotifications(ctx, "user", user.ID, false)
		if err != nil {
			return false
		}
		for _, n := range notifs {
			if n.Status == ChatNotificationDMReceived {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond,
		"AC-6: NotifyDMReceived did not fire — no DM notification for recipient")
}

// ---------------------------------------------------------------------------
// AC-7: original channel-validation call site still rejects an invalid
// channel after the helper extraction. Proven by mutation: an explicit
// (non-affinity) invalid channel on the pre-existing path.
//
// The original validation runs BEFORE the broker dispatch, so this test
// uses the broker setup (the validator needs broker.ListChannels to check
// registered channels). The explicit channel "nonexistent-spoke" is NOT in
// the broker's registered list, so the validator rejects it at the original
// call site — before the message ever reaches the broker.
// ---------------------------------------------------------------------------

func TestDEF158_AC7_OriginalValidation_StillRejects(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)

	// Set up a broker so that ListChannels is available for validation.
	// Register "web" as the only valid spoke.
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)

	// Send with an EXPLICIT invalid channel and a ThreadID. The ThreadID
	// makes the conversation kind "thread" (not "direct"), so the DEF-158
	// post-affinity validation block is NOT reached — only the ORIGINAL
	// validation call site at :251 can reject this.
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "explicit bad channel via thread",
		Channel:   "nonexistent-spoke",
		ThreadID:  "topic-" + project.ID,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agent.ID+"/outbound-message",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"AC-7: original validation site must still reject unregistered channels; body: %s",
		rr.Body.String())
	assert.Contains(t, rr.Body.String(), "nonexistent-spoke")
}

// ---------------------------------------------------------------------------
// DEF-159 pinning test: KNOWN DEFECT on the normal path.
//
// This test pins a KNOWN DEFECT, not expected behaviour.
//
// On the normal path (explicit recipient via user:<email>, no conv-ref), when
// no affinity row exists, Channel and ThreadID are both empty. The message is
// persisted successfully (HTTP 200) but is invisible to the user through three
// independent gates:
//   (1) Channel="" — the history endpoint filters Channel:"web", so the
//       message is absent from every read path.
//   (2) ThreadID="" — TouchDMActivity (messagebroker.go:583) is gated on
//       storeMsg.ThreadID != "", so no unread-dot watermark fires.
//   (3) ThreadID="" — NotifyDMReceived (messagebroker.go:606) is gated on
//       storeMsg.ThreadID != "", so no notification fires.
// There is no single suppression point; the lived experience is "invisible."
//
// Correct behaviour: Channel and ThreadID should be populated so the message
// is visible in history, generates a watermark, and produces a notification —
// the same shape sendAgentRouted writes for user→agent DMs
// (handlers_chat_v2.go:1059).
//
// The fix is NOT applied here because of FACT 2 (architect review): the
// comment at :233 claims empty Channel fans out to all spokes, but spoke
// selection lives in the broker plugin, outside this repo, and the claim is
// unverified. Narrowing Channel blind risks removing delivery for a
// multi-spoke user who is reached today. DEF-159 tracks this fix once the
// broker plugin's semantics are confirmed.
//
// When DEF-159 is fixed, this test gets INVERTED (assert non-empty, assert
// visible), not deleted.
// ---------------------------------------------------------------------------

func TestDEF159_KnownDefect_NormalPathLeavesChannelAndThreadIDEmpty(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Broker with "web" spoke — needed for the affinity guard at :238.
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)

	// WebChatStore for affinity lookups — but do NOT seed any affinity.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Enable read-switch.
	enableReadSwitch(t, srv)

	// Normal path: explicit recipient, no conv-ref.
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "def159 known defect probe",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agent.ID+"/outbound-message",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"normal path must succeed; body: %s", rr.Body.String())

	// Wait for broker to persist.
	var stored *store.Message
	require.Eventually(t, func() bool {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID},
			store.ListOptions{Limit: 10})
		if err != nil || len(msgs.Items) == 0 {
			return false
		}
		for i := range msgs.Items {
			if msgs.Items[i].Msg == "def159 known defect probe" {
				stored = &msgs.Items[i]
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "message not persisted")

	// DEF-159 KNOWN DEFECT: Channel and ThreadID are both empty.
	// When DEF-159 is fixed, invert these assertions.
	assert.Empty(t, stored.Channel,
		"DEF-159 known defect: on normal path with no affinity, Channel is empty")
	assert.Empty(t, stored.ThreadID,
		"DEF-159 known defect: on normal path, ThreadID is empty (no backfill outside conv-ref scope)")

	// Read-path visibility: the message is NOT visible in history because
	// the filter is Channel:"web" and the stored Channel is "".
	// When DEF-159 is fixed, invert this assertion (message SHOULD be visible).
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)
	code, histResp := readConversationHistoryAsUser(t, srv, user, dmKey)
	require.Equal(t, http.StatusOK, code,
		"DEF-159: history read must succeed so visibility assertion runs")
	found := false
	for _, m := range histResp.Messages {
		if m.Msg == "def159 known defect probe" {
			found = true
			break
		}
	}
	// When DEF-159 is fixed, invert: assert.True(t, found, ...).
	assert.False(t, found,
		"DEF-159 known defect: message with empty Channel must NOT appear in history (Channel:'web' filter)")
}
