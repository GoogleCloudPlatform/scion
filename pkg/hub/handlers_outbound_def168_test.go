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
// DEF-168: explicit conversation reference must not be overridden by
// channel affinity.
//
// Live bug: an agent replied to conv:<uuid> (kind=direct, surface=native),
// but delivery was routed to Discord because GetLastChannel returned
// "discord" for the (agent, project, user) triple. Discord rejected the
// native dm: key as a channel_id (not a snowflake) — HTTP 502 in production.
//
// Fix: skip channel-affinity lookup when req.ConversationRef != "" (Site 1),
// and remove the redundant affinity re-run inside the direct conv-ref
// branch (Site 2) so SurfaceToChannel runs unconditionally.
// ---------------------------------------------------------------------------

// def168Setup creates a server with:
//   - a direct/native DM conversation between agent and user
//   - affinity record pointing to "discord" (NOT the conversation's actual surface)
//   - broker with both "web" and "discord" spokes registered
//
// This reproduces the exact live scenario from DEF-168.
func def168Setup(t *testing.T) (
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

	// Broker with "web" AND "discord" spokes — both must be registered for
	// channel validation to pass. The live bug required "discord" to be a
	// valid channel for the misroute to succeed.
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
		{Name: "discord", Bus: nullSpokeEventBus{}},
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

	// Create the DM conversation with surface="native".
	dmKey, err = messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	dmConv, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Seed reply-affinity: user's last channel for this agent is "discord" —
	// THIS IS THE BUG TRIGGER. The conversation is native, but affinity says
	// discord, exactly as observed in production.
	require.NoError(t, wcs.RecordChannel(ctx, user.ID, project.ID, agent.ID, "discord", time.Now()))

	return srv, s, wcs, project, agent, user, dmConv, dmKey
}

// ---------------------------------------------------------------------------
// TestDEF168_ConvRefNotOverriddenByAffinity: the core regression test.
//
// A direct/native conversation exists. Affinity says "discord" for this
// agent+user pair. An agent sends via conv:<uuid> with no explicit channel.
// The message MUST be routed to "web" (native's channel), NOT "discord".
// ---------------------------------------------------------------------------

func TestDEF168_ConvRefNotOverriddenByAffinity(t *testing.T) {
	srv, s, _, project, agent, _, dmConv, _ := def168Setup(t)
	ctx := context.Background()

	// Send via conv-ref, no explicit channel — this is the live scenario.
	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"def168 conv-ref message", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"conv-ref direct must succeed; body: %s", rr.Body.String())

	// Wait for the message to be persisted by the broker goroutine.
	var stored *store.Message
	require.Eventually(t, func() bool {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			ConversationID: dmConv.ID,
		}, store.ListOptions{Limit: 10})
		if err != nil || len(msgs.Items) == 0 {
			return false
		}
		for i := range msgs.Items {
			if msgs.Items[i].Msg == "def168 conv-ref message" {
				stored = &msgs.Items[i]
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "message not persisted within timeout")

	// CRITICAL ASSERTION: the message must be routed to "web" (the native
	// surface's channel), NOT "discord" (the affinity guess).
	assert.Equal(t, "web", stored.Channel,
		"DEF-168: conv-ref message must use conversation's surface channel ('web'), not affinity ('discord')")
	assert.NotEqual(t, "discord", stored.Channel,
		"DEF-168: affinity must not override explicit conv-ref routing")
}

// ---------------------------------------------------------------------------
// TestDEF168_ExplicitChannelStillWinsOverSurface: when the caller passes
// both a conv-ref AND an explicit --channel, the explicit channel must win
// over SurfaceToChannel. This confirms the fix doesn't regress the explicit
// channel path (the existing `if req.Channel == ""` guard preserves it).
// ---------------------------------------------------------------------------

func TestDEF168_ExplicitChannelStillWinsOverSurface(t *testing.T) {
	srv, s, _, project, agent, _, dmConv, _ := def168Setup(t)
	ctx := context.Background()

	// Send with an explicit channel alongside the conv-ref.
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             "def168 explicit channel",
		ConversationRef: "conv:" + dmConv.ID,
		Channel:         "discord", // explicit channel should win
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
		"explicit channel with conv-ref must succeed; body: %s", rr.Body.String())

	var stored *store.Message
	require.Eventually(t, func() bool {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			ConversationID: dmConv.ID,
		}, store.ListOptions{Limit: 10})
		if err != nil || len(msgs.Items) == 0 {
			return false
		}
		for i := range msgs.Items {
			if msgs.Items[i].Msg == "def168 explicit channel" {
				stored = &msgs.Items[i]
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "message not persisted within timeout")

	// Explicit channel must be preserved — it should NOT be overwritten by
	// SurfaceToChannel.
	assert.Equal(t, "discord", stored.Channel,
		"DEF-168: explicit --channel flag must win over SurfaceToChannel")
}

// ---------------------------------------------------------------------------
// TestDEF168_AffinityStillAppliesWhenNoConvRef: the ambiguous case.
//
// When no conv-ref is set and no explicit channel is provided, affinity
// should still determine the channel. This confirms the fix is scoped to
// "skip affinity when a stronger signal exists" — not "affinity is gone."
// ---------------------------------------------------------------------------

func TestDEF168_AffinityStillAppliesWhenNoConvRef(t *testing.T) {
	srv, s, _, project, agent, user, dmConv, _ := def168Setup(t)
	ctx := context.Background()

	// Send WITHOUT a conv-ref, with an explicit recipient — affinity should
	// kick in and route to "discord" (the seeded affinity value).
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "def168 ambiguous message",
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
		"ambiguous send must succeed; body: %s", rr.Body.String())

	var stored *store.Message
	require.Eventually(t, func() bool {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{
			ConversationID: dmConv.ID,
		}, store.ListOptions{Limit: 10})
		if err != nil || len(msgs.Items) == 0 {
			return false
		}
		for i := range msgs.Items {
			if msgs.Items[i].Msg == "def168 ambiguous message" {
				stored = &msgs.Items[i]
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "message not persisted within timeout")

	// Affinity should have fired: channel should be "discord" (the seeded value).
	assert.Equal(t, "discord", stored.Channel,
		"DEF-168: affinity must still apply when no conv-ref is present")
}
