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

package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestDiscordBroker_Publish_ConversationRoutingAndFallback(t *testing.T) {
	f := newDMRouteFixture(t)
	topic := "scion.project.p1.user.u1.messages"

	// 1. Conversation field (Phase 11) with empty legacy Channel/ThreadID routes to Discord.
	err := f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:" + dmTestEmail,
		Msg:            "phase 11 message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "discord",
			ExternalRef: "555555555555555555",
			ParentRef:   "200000000000000001",
			DisplayName: "#eng-thread",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"555555555555555555"}, sentChannelIDs(f.transport))

	// 2. Conversation targeting another surface ("slack") is ignored.
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:" + dmTestEmail,
		Msg:       "slack message",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
		},
	})
	require.NoError(t, err)
	assert.Len(t, sentChannelIDs(f.transport), 1)

	// 3. Defeat scenario (a): reply affinity — Channel:"discord" + native DM
	// conversation + Recipient:user must deliver to the user's Discord channel
	// (not be dropped by Surface:"native" and not try to post to "dm:...").
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:" + dmTestEmail,
		Msg:            "reply affinity DM",
		Type:           messages.TypeInstruction,
		Channel:        "discord",
		ConversationID: "conv-dm-affinity",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:user:u1",
		},
	})
	require.NoError(t, err)
	assert.Len(t, sentChannelIDs(f.transport), 2)

	// 3b. Foreign non-empty Channel ("web") is skipped by Discord.
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:" + dmTestEmail,
		Msg:       "webchat message",
		Type:      messages.TypeInstruction,
		Channel:   "web",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "thread:p1:topic-1",
		},
	})
	require.NoError(t, err)
	assert.Len(t, sentChannelIDs(f.transport), 2)

	// 4. Legacy fallback (older hub sends Channel/ThreadID without Conversation).
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:" + dmTestEmail,
		Msg:       "legacy hub message",
		Type:      messages.TypeInstruction,
		Channel:   "discord",
		ThreadID:  "666666666666666666",
	})
	require.NoError(t, err)
	got := sentChannelIDs(f.transport)
	require.Len(t, got, 3)
	assert.Equal(t, "555555555555555555", got[0])
	assert.Equal(t, "666666666666666666", got[2])
}

func TestDiscordBroker_Inbound_SendsSurfaceExternalRefParentRef(t *testing.T) {
	var mu sync.Mutex
	var gotInbound []map[string]any
	var gotRouted []map[string]any

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		switch r.URL.Path {
		case "/api/v1/broker/inbound":
			gotInbound = append(gotInbound, body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"delivered":true}`))
		case "/api/v1/broker/inbound/routed":
			gotRouted = append(gotRouted, body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"delivered":true,"primary_agent":"coder"}`))
		default:
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hubSrv.Close()

	b := NewBroker(discardLogger())
	b.hubURL = hubSrv.URL
	b.threadParents = map[string]string{
		"777777777777777777": "200000000000000001",
	}

	// Threaded channel: without hub_inbound_conversation flag (older hub),
	// inbound payloads must omit surface/external_ref/parent_ref/display_name/legacy_ref.
	threadMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello from thread",
		Type:      messages.TypeInstruction,
		Channel:   "discord",
		ThreadID:  "777777777777777777",
		Metadata: map[string]string{
			"discord_channel_id": "777777777777777777",
		},
	}

	require.Nil(t, b.deliverInbound("scion.project.p1.agent.coder.messages", threadMsg))
	resNoFlag, heNoFlag := b.deliverRoutedInbound("p1", "coder", threadMsg)
	require.Nil(t, heNoFlag)
	require.NotNil(t, resNoFlag)
	assert.True(t, resNoFlag.Delivered)

	mu.Lock()
	require.Len(t, gotInbound, 1)
	require.Len(t, gotRouted, 1)
	for _, field := range []string{"surface", "external_ref", "parent_ref", "display_name", "legacy_ref"} {
		_, hasIn := gotInbound[0][field]
		assert.False(t, hasIn, "older hub (no flag) must omit %q on /broker/inbound", field)
		_, hasRt := gotRouted[0][field]
		assert.False(t, hasRt, "older hub (no flag) must omit %q on /broker/inbound/routed", field)
	}
	gotInbound = nil
	gotRouted = nil
	mu.Unlock()

	// Enable hub_inbound_conversation="1" (P4+/P5a hub).
	require.NoError(t, b.Configure(map[string]string{
		"hub_inbound_conversation": "1",
	}))

	// Threaded channel: includes surface, external_ref, parent_ref, and legacy_ref.
	require.Nil(t, b.deliverInbound("scion.project.p1.agent.coder.messages", threadMsg))
	res, he := b.deliverRoutedInbound("p1", "coder", threadMsg)
	require.Nil(t, he)
	require.NotNil(t, res)
	assert.True(t, res.Delivered)

	// Top-level channel (non-threaded): includes surface, external_ref, and legacy_ref, omits parent_ref.
	channelMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello from channel",
		Type:      messages.TypeInstruction,
		Channel:   "discord",
		ThreadID:  "200000000000000001",
		Metadata: map[string]string{
			"discord_channel_id": "200000000000000001",
		},
	}
	require.Nil(t, b.deliverInbound("scion.project.p1.agent.coder.messages", channelMsg))

	// Bare message (no metadata/ThreadID): still sends surface:"discord", omits external_ref/parent_ref/legacy_ref.
	bareMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "bare dm",
		Type:      messages.TypeInstruction,
		Channel:   "discord",
	}
	require.Nil(t, b.deliverInbound("scion.project.p1.agent.coder.messages", bareMsg))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotInbound, 3)
	assert.Equal(t, "discord", gotInbound[0]["surface"])
	assert.Equal(t, "777777777777777777", gotInbound[0]["external_ref"])
	assert.Equal(t, "200000000000000001", gotInbound[0]["parent_ref"])
	assert.Equal(t, "777777777777777777", gotInbound[0]["legacy_ref"])

	assert.Equal(t, "discord", gotInbound[1]["surface"])
	assert.Equal(t, "200000000000000001", gotInbound[1]["external_ref"])
	assert.Equal(t, "200000000000000001", gotInbound[1]["legacy_ref"])
	_, hasParent := gotInbound[1]["parent_ref"]
	assert.False(t, hasParent, "non-threaded channel should omit parent_ref")

	assert.Equal(t, "discord", gotInbound[2]["surface"])
	_, hasBareExt := gotInbound[2]["external_ref"]
	assert.False(t, hasBareExt, "bare message should omit external_ref")
	_, hasBareParent := gotInbound[2]["parent_ref"]
	assert.False(t, hasBareParent, "bare message should omit parent_ref")
	_, hasBareLegacy := gotInbound[2]["legacy_ref"]
	assert.False(t, hasBareLegacy, "bare message should omit legacy_ref")

	require.Len(t, gotRouted, 1)
	assert.Equal(t, "discord", gotRouted[0]["surface"])
	assert.Equal(t, "777777777777777777", gotRouted[0]["external_ref"])
	assert.Equal(t, "200000000000000001", gotRouted[0]["parent_ref"])
	assert.Equal(t, "777777777777777777", gotRouted[0]["legacy_ref"])
}

// TestDiscordBroker_Publish_HubEnrichedOptionAShapes drives the Discord broker
// with the exact outbound shapes emitted by MessageBrokerProxy.enrichConversation
// under Option A: (1) an external thread reply with translated snowflake
// ExternalRef, and (2) a DM reply with Conversation omitted (nil) and
// reply-affinity Channel:"discord".
func TestDiscordBroker_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	f := newDMRouteFixture(t)
	topic := "scion.project.p1.user.u1.messages"

	// 1. Outbound `--channel discord --thread-id 777777777777777777` after
	// Option A translation (thread:<proj>: stripped).
	err := f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:" + dmTestEmail,
		Msg:            "reply on discord thread",
		Type:           messages.TypeInstruction,
		Channel:        "discord",
		ThreadID:       "777777777777777777",
		ConversationID: "conv-discord-thread",
		Conversation: &messages.Conversation{
			Surface:     "discord",
			ExternalRef: "777777777777777777",
			ParentRef:   "200000000000000001",
		},
	})
	require.NoError(t, err)

	// 2. Outbound DM reply with affinity Channel:"discord" and Conversation:nil.
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:" + dmTestEmail,
		Msg:            "dm reply via discord affinity",
		Type:           messages.TypeInstruction,
		Channel:        "discord",
		ConversationID: "conv-dm-1",
		Conversation:   nil,
	})
	require.NoError(t, err)

	got := sentChannelIDs(f.transport)
	require.Len(t, got, 2)
	assert.Equal(t, "777777777777777777", got[0])
}

type discordHubConvStore struct {
	store.Store
	convs map[string]*store.Conversation
}

func (s *discordHubConvStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	c, ok := s.convs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

type discordForwardingBus struct {
	broker *DiscordBroker
}

func (f *discordForwardingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	return f.broker.Publish(ctx, topic, wire)
}

func (f *discordForwardingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *discordForwardingBus) Close() error { return nil }

// TestDiscordBroker_Publish_HubEnrichedDirectConversationNeverBroadcasts (C1)
// drives DiscordBroker.Publish from REAL hub.MessageBrokerProxy enrichment
// (with real store.Conversation rows and grpcbroker proto round-trip) to verify
// that native/direct Conversations (empty ExternalRef, Kind:"direct") are
// recipient-routed and never broadcast to project-linked channels.
func TestDiscordBroker_Publish_HubEnrichedDirectConversationNeverBroadcasts(t *testing.T) {
	ctx := context.Background()
	projectID := dmTestProjectID

	dmKey, dmKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      dmTestAgentID,
		RecipientKind: "user",
		RecipientID:   dmTestUserID,
	})
	require.NoError(t, err)
	require.Equal(t, "direct", dmKind)

	a2aKey, a2aKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      dmTestAgentID,
		RecipientKind: "agent",
		RecipientID:   "33333333-3333-4333-8333-333333333333",
	})
	require.NoError(t, err)
	require.Equal(t, "direct", a2aKind)

	threadKey, threadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "777777777777777777",
		ProjectID: projectID,
	})
	require.NoError(t, err)

	convStore := &discordHubConvStore{
		convs: map[string]*store.Conversation{
			"conv-discord-thread": {
				ID:          "conv-discord-thread",
				ProjectID:   &projectID,
				Kind:        threadKind,
				Surface:     "discord",
				ExternalRef: threadKey,
				ParentRef:   "200000000000000001",
				DisplayName: "#eng-thread",
			},
			"conv-dm-user": {
				ID:          "conv-dm-user",
				ProjectID:   &projectID,
				Kind:        dmKind,
				Surface:     "native",
				ExternalRef: dmKey,
				DisplayName: "Direct Message",
			},
			"conv-dm-a2a": {
				ID:          "conv-dm-a2a",
				ProjectID:   &projectID,
				Kind:        a2aKind,
				Surface:     "native",
				ExternalRef: a2aKey,
			},
			"conv-native-group": {
				ID:          "conv-native-group",
				ProjectID:   &projectID,
				Kind:        "group",
				Surface:     "native",
				ExternalRef: "thread:" + projectID + ":native-topic",
				DisplayName: "Native Topic",
			},
		},
	}

	t.Run("external discord thread reply with empty ThreadID routes to thread", func(t *testing.T) {
		f := newDMRouteFixture(t)
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, dmTestUserID, &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:" + dmTestEmail,
			RecipientID:    dmTestUserID,
			Msg:            "reply on discord thread",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ConversationID: "conv-discord-thread",
		})
		require.NoError(t, err)
		assert.Empty(t, f.dmCreateCalls())
		assert.Equal(t, []string{"777777777777777777"}, sentChannelIDs(f.transport))
	})

	t.Run("linked user without stored channel opens DM channel and never broadcasts", func(t *testing.T) {
		f := newDMRouteFixture(t)
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, dmTestUserID, &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:" + dmTestEmail,
			RecipientID:    dmTestUserID,
			Msg:            "private dm without stored channel",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ThreadID:       dmKey,
			ConversationID: "conv-dm-user",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{dmTestDiscordID}, f.dmCreateCalls(), "must open DM channel for linked user")
		assert.Equal(t, []string{f.createID}, sentChannelIDs(f.transport), "must deliver to DM channel, never linked project channel")
	})

	t.Run("linked user with stored channel uses stored channel without opening DM", func(t *testing.T) {
		f := newDMRouteFixture(t)
		const storedCh = "500000000000000001"
		require.NoError(t, f.store.SetConversationContext(ctx, &ConversationContext{
			DiscordUserID: dmTestDiscordID,
			ProjectID:     dmTestProjectID,
			AgentSlug:     dmTestAgentSlug,
			LastChannelID: storedCh,
			LastMessageAt: time.Now(),
		}))
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, dmTestUserID, &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:" + dmTestEmail,
			RecipientID:    dmTestUserID,
			Msg:            "private dm with stored channel",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ThreadID:       dmKey,
			ConversationID: "conv-dm-user",
		})
		require.NoError(t, err)
		assert.Empty(t, f.dmCreateCalls())
		assert.Equal(t, []string{storedCh}, sentChannelIDs(f.transport))
	})

	t.Run("unlinked user returns error and never broadcasts to project channel link", func(t *testing.T) {
		f := newDMRouteFixture(t)
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, "99999999-9999-4999-8999-999999999999", &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:bob@example.com",
			RecipientID:    "99999999-9999-4999-8999-999999999999",
			Msg:            "private dm to unlinked user",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ThreadID:       dmKey,
			ConversationID: "conv-dm-user",
		})
		require.Error(t, err)
		assert.Empty(t, f.dmCreateCalls())
		assert.Empty(t, sentChannelIDs(f.transport), "must never broadcast private DM to project channel links")
	})

	t.Run("agent-to-agent observer DM reaches only ShowAgentToAgent link", func(t *testing.T) {
		f := newDMRouteFixture(t)
		const observeCh = "210000000000000001"
		require.NoError(t, f.store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID:        observeCh,
			GuildID:          testGuildID,
			ProjectID:        dmTestProjectID,
			ProjectSlug:      dmTestProjectID,
			LinkedBy:         "test",
			LinkedAt:         time.Now(),
			Active:           true,
			ShowAgentToAgent: true,
		}))
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishMessage(ctx, projectID, &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "agent:reviewer",
			RecipientID:    "33333333-3333-4333-8333-333333333333",
			Msg:            "agent-to-agent direct observer copy",
			Type:           messages.TypeInstruction,
			ObserverOnly:   true,
			ConversationID: "conv-dm-a2a",
		})
		require.NoError(t, err)
		assert.Empty(t, f.dmCreateCalls())
		assert.Equal(t, []string{observeCh}, sentChannelIDs(f.transport),
			"observer copy must reach ShowAgentToAgent link and not non-observe link")
	})

	t.Run("direct user DM ignores bogus snowflake ThreadID when Conversation is present", func(t *testing.T) {
		f := newDMRouteFixture(t)
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, dmTestUserID, &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:" + dmTestEmail,
			RecipientID:    dmTestUserID,
			Msg:            "dm with stale snowflake thread_id",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ThreadID:       "777777777777777777",
			ConversationID: "conv-dm-user",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{dmTestDiscordID}, f.dmCreateCalls(), "must ignore ThreadID and open DM channel")
		assert.Equal(t, []string{f.createID}, sentChannelIDs(f.transport))
	})

	t.Run("native group addressed to linked user without context opens DM and never broadcasts", func(t *testing.T) {
		f := newDMRouteFixture(t)
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, dmTestUserID, &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:" + dmTestEmail,
			RecipientID:    dmTestUserID,
			Msg:            "native group for linked user",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ConversationID: "conv-native-group",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{dmTestDiscordID}, f.dmCreateCalls(), "native group for linked user must open DM channel")
		assert.Equal(t, []string{f.createID}, sentChannelIDs(f.transport), "must deliver to DM channel, never project links")
	})

	t.Run("native group addressed to unlinked user returns error and never broadcasts", func(t *testing.T) {
		f := newDMRouteFixture(t)
		proxy := hub.NewMessageBrokerProxy(&discordForwardingBus{broker: f.broker}, convStore, nil, nil, slog.Default())

		err := proxy.PublishUserMessage(ctx, projectID, "99999999-9999-4999-8999-999999999999", &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:" + dmTestAgentSlug,
			SenderID:       dmTestAgentID,
			Recipient:      "user:bob@example.com",
			RecipientID:    "99999999-9999-4999-8999-999999999999",
			Msg:            "native group for unlinked user",
			Type:           messages.TypeInstruction,
			Channel:        "discord",
			ConversationID: "conv-native-group",
		})
		require.Error(t, err)
		assert.Empty(t, f.dmCreateCalls())
		assert.Empty(t, sentChannelIDs(f.transport), "native group for unlinked user must never broadcast to project links")
	})
}
