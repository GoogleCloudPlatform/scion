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

package slack

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type postedSlackMessage struct {
	Channel  string
	ThreadTS string
}

func newTestSlackServer(t *testing.T) (*httptest.Server, func() []postedSlackMessage) {
	t.Helper()
	var mu sync.Mutex
	var posted []postedSlackMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			posted = append(posted, postedSlackMessage{
				Channel:  r.FormValue("channel"),
				ThreadTS: r.FormValue("thread_ts"),
			})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C0123","ts":"1700000001.000100"}`))
	}))
	t.Cleanup(srv.Close)

	return srv, func() []postedSlackMessage {
		mu.Lock()
		defer mu.Unlock()
		out := make([]postedSlackMessage, len(posted))
		copy(out, posted)
		return out
	}
}

func TestSlackBroker_Publish_ConversationRoutingAndFallback(t *testing.T) {
	srv, getPosted := newTestSlackServer(t)

	b := NewBroker(nil)
	b.client = slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	b.store = newTestStore(t)

	// 1. Conversation field (Phase 11) with empty legacy Channel/ThreadID routes to Slack.
	err := b.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "phase 11 message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
			ParentRef:   "C0123ABC",
			DisplayName: "#eng",
		},
	})
	require.NoError(t, err)
	posted := getPosted()
	require.Len(t, posted, 1)
	assert.Equal(t, "C0123ABC", posted[0].Channel)
	assert.Equal(t, "1700000000.000100", posted[0].ThreadTS)

	// 2. Conversation targeting another surface ("discord") is ignored.
	err = b.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "discord message",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "discord",
			ExternalRef: "987654321",
		},
	})
	require.NoError(t, err)
	assert.Len(t, getPosted(), 1)

	// 3. Defeat scenario (b): agent-to-agent observer copy with native DM conversation
	// (Channel == "") must be delivered to ShowAgentToAgent links, not dropped.
	require.NoError(t, b.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:        "C_OBS_LINK",
		ProjectID:        "p1",
		Active:           true,
		ShowAgentToAgent: true,
	}))
	err = b.Publish(context.Background(), "scion.project.p1.agent.reviewer.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "agent-to-agent observer copy",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-a2a",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:agent:a2",
		},
	})
	require.NoError(t, err)
	posted = getPosted()
	require.Len(t, posted, 2)
	assert.Equal(t, "C_OBS_LINK", posted[1].Channel)
	assert.Equal(t, "", posted[1].ThreadTS)

	// 3b. Foreign non-empty Channel ("web") is skipped by Slack.
	err = b.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "webchat message",
		Type:      messages.TypeInstruction,
		Channel:   "web",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "thread:p1:topic-1",
		},
	})
	require.NoError(t, err)
	assert.Len(t, getPosted(), 2)

	// 4. Legacy fallback (older hub sends Channel/ThreadID without Conversation).
	err = b.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "legacy hub message",
		Type:      messages.TypeInstruction,
		Channel:   "slack",
		ThreadID:  "C9999XYZ:1700000002.000200",
	})
	require.NoError(t, err)
	posted = getPosted()
	require.Len(t, posted, 3)
	assert.Equal(t, "C9999XYZ", posted[2].Channel)
	assert.Equal(t, "1700000002.000200", posted[2].ThreadTS)
}

func TestSlackBroker_Inbound_SendsSurfaceExternalRefParentRef(t *testing.T) {
	var mu sync.Mutex
	var gotInbound map[string]any
	var gotRouted map[string]any

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		switch r.URL.Path {
		case "/api/v1/broker/inbound":
			gotInbound = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"delivered":true}`))
		case "/api/v1/broker/inbound/routed":
			gotRouted = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"delivered":true,"primary_agent":"coder"}`))
		default:
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hubSrv.Close()

	b := NewBroker(nil)
	b.hubURL = hubSrv.URL

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello agent",
		Type:      messages.TypeInstruction,
		Channel:   "slack",
		ThreadID:  "1700000000.000100",
		Metadata: map[string]string{
			"slack_channel_id": "C0123ABC",
			"slack_thread_ts":  "1700000000.000100",
		},
	}

	// 1. Without hub_inbound_conversation capability flag (older hub):
	// inbound payloads must omit surface, external_ref, parent_ref, display_name, legacy_ref.
	he := b.deliverInbound("scion.project.p1.agent.coder.messages", msg)
	require.Nil(t, he)
	res, he := b.deliverRoutedInbound("p1", "coder", msg)
	require.Nil(t, he)
	require.NotNil(t, res)
	assert.True(t, res.Delivered)

	mu.Lock()
	for _, field := range []string{"surface", "external_ref", "parent_ref", "display_name", "legacy_ref"} {
		_, hasIn := gotInbound[field]
		assert.False(t, hasIn, "older hub (no flag) must omit %q on /broker/inbound", field)
		_, hasRt := gotRouted[field]
		assert.False(t, hasRt, "older hub (no flag) must omit %q on /broker/inbound/routed", field)
	}
	gotInbound = nil
	gotRouted = nil
	mu.Unlock()

	// 2. With hub_inbound_conversation="1" (P4+/P5a hub):
	// inbound payloads include surface, external_ref, parent_ref, and legacy_ref.
	require.NoError(t, b.Configure(map[string]string{
		"hub_inbound_conversation": "1",
	}))

	he = b.deliverInbound("scion.project.p1.agent.coder.messages", msg)
	require.Nil(t, he)

	res, he = b.deliverRoutedInbound("p1", "coder", msg)
	require.Nil(t, he)
	require.NotNil(t, res)
	assert.True(t, res.Delivered)

	mu.Lock()
	require.NotNil(t, gotInbound)
	assert.Equal(t, "slack", gotInbound["surface"])
	assert.Equal(t, "C0123ABC:1700000000.000100", gotInbound["external_ref"])
	assert.Equal(t, "C0123ABC", gotInbound["parent_ref"])
	assert.Equal(t, "1700000000.000100", gotInbound["legacy_ref"])

	require.NotNil(t, gotRouted)
	assert.Equal(t, "slack", gotRouted["surface"])
	assert.Equal(t, "C0123ABC:1700000000.000100", gotRouted["external_ref"])
	assert.Equal(t, "C0123ABC", gotRouted["parent_ref"])
	assert.Equal(t, "1700000000.000100", gotRouted["legacy_ref"])
	gotInbound = nil
	mu.Unlock()

	// 3. Even when metadata/ThreadID are absent, surface:"slack" is always sent
	// when hub_inbound_conversation is enabled.
	bareMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "bare dm",
		Type:      messages.TypeInstruction,
		Channel:   "slack",
	}
	he = b.deliverInbound("scion.project.p1.agent.coder.messages", bareMsg)
	require.Nil(t, he)

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, gotInbound)
	assert.Equal(t, "slack", gotInbound["surface"])
	_, hasExt := gotInbound["external_ref"]
	assert.False(t, hasExt)
	_, hasLegacy := gotInbound["legacy_ref"]
	assert.False(t, hasLegacy)
}

type hubConvStore struct {
	store.Store
	convs map[string]*store.Conversation
}

func (s *hubConvStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	c, ok := s.convs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

type slackForwardingBus struct {
	broker *SlackBroker
}

func (f *slackForwardingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	// Round-trip through the real gRPC proto conversion so the Slack plugin
	// receives the exact wire shape emitted by a real hub.
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	return f.broker.Publish(ctx, topic, wire)
}

func (f *slackForwardingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *slackForwardingBus) Close() error { return nil }

// TestSlackBroker_Publish_HubEnrichedOptionAShapes drives the Slack broker
// from REAL hub-enriched shapes produced by hub.MessageBrokerProxy (backed by
// real store.Conversation rows and round-tripped through grpcbroker proto
// conversion):
//  1. External Slack thread reply (Kind:"group", Surface:"slack", translated ExternalRef)
//  2. User DM reply (Kind:"direct", Surface:"native", empty ExternalRef) routed by Recipient,
//     ignoring any legacy ThreadID when Conversation is present
//  3. User DM reply for an unmapped user does not broadcast to public project channel links
//  4. Agent-to-agent observer DM (Kind:"direct", Surface:"native") still delivers to
//     ShowAgentToAgent channel links
func TestSlackBroker_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	srv, getPosted := newTestSlackServer(t)

	b := NewBroker(nil)
	b.client = slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	b.store = newTestStore(t)
	ctx := context.Background()

	require.NoError(t, b.store.CreateUserMapping(ctx, &SlackUserMapping{
		SlackUserID: "U_ALICE",
		ScionEmail:  "alice@example.com",
	}))
	require.NoError(t, b.store.SetConversationContext(ctx, &ConversationContext{
		SlackUserID:   "U_ALICE",
		ProjectID:     "p1",
		AgentSlug:     "coder",
		LastChannelID: "D_ALICE_DM",
	}))
	require.NoError(t, b.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID:        "C_PUBLIC_OBS",
		ProjectID:        "p1",
		Active:           true,
		ShowAgentToAgent: true,
	}))

	projectID := "p1"
	threadKey, threadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "C0123ABC:1700000000.000100",
		ProjectID: projectID,
	})
	require.NoError(t, err)
	dmKey, dmKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      "11111111-1111-1111-1111-111111111111",
		RecipientKind: "user",
		RecipientID:   "22222222-2222-2222-2222-222222222222",
	})
	require.NoError(t, err)
	a2aKey, a2aKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      "11111111-1111-1111-1111-111111111111",
		RecipientKind: "agent",
		RecipientID:   "33333333-3333-3333-3333-333333333333",
	})
	require.NoError(t, err)

	convStore := &hubConvStore{
		convs: map[string]*store.Conversation{
			"conv-slack-thread": {
				ID:          "conv-slack-thread",
				ProjectID:   &projectID,
				Kind:        threadKind,
				Surface:     "slack",
				ExternalRef: threadKey,
				ParentRef:   "C0123ABC",
				DisplayName: "#eng",
			},
			"conv-dm-alice": {
				ID:          "conv-dm-alice",
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
		},
	}

	proxy := hub.NewMessageBrokerProxy(&slackForwardingBus{broker: b}, convStore, nil, nil, slog.Default())

	// 1. Outbound Slack thread reply enriched by MessageBrokerProxy (no legacy ThreadID needed).
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-alice", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "reply on slack thread",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-slack-thread",
	}))

	// 2. Outbound DM reply enriched by MessageBrokerProxy: Conversation is
	// {Surface:"native", Kind:"direct", DisplayName:"Direct Message"} with empty
	// ExternalRef. Even with a legacy ThreadID present, Slack must NOT read
	// ThreadID and must route to Alice's DM via Recipient.
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-alice", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "dm reply via slack affinity",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ThreadID:       "C_WRONG_CHAN:1700000099.000999",
		ConversationID: "conv-dm-alice",
	}))

	// 3. Outbound DM reply to an unmapped user must not broadcast to public project links.
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-bob", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:bob@example.com",
		Msg:            "private dm for unmapped user",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-dm-alice",
	}))

	// 4. Outbound agent-to-agent observer DM still delivers to ShowAgentToAgent link.
	require.NoError(t, proxy.PublishMessage(ctx, projectID, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "agent-to-agent observer dm",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-dm-a2a",
	}))

	posted := getPosted()
	require.Len(t, posted, 3)
	assert.Equal(t, "C0123ABC", posted[0].Channel)
	assert.Equal(t, "1700000000.000100", posted[0].ThreadTS)
	assert.Equal(t, "D_ALICE_DM", posted[1].Channel)
	assert.Equal(t, "", posted[1].ThreadTS)
	assert.Equal(t, "C_PUBLIC_OBS", posted[2].Channel)
	assert.Equal(t, "", posted[2].ThreadTS)

	// 5. Hand-built defense-in-depth test pinning the Kind != "direct" guard:
	// even if a Conversation carries Kind:"direct" alongside a non-empty Slack
	// ExternalRef, Slack routes the DM via Recipient and ignores ExternalRef.
	require.NoError(t, b.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "direct with unexpected external_ref",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-direct-guard",
		Conversation: &messages.Conversation{
			Surface:     "slack",
			Kind:        "direct",
			ExternalRef: "C0123ABC:1700000000.000100",
			ParentRef:   "C0123ABC",
		},
	}))
	posted = getPosted()
	require.Len(t, posted, 4)
	assert.Equal(t, "D_ALICE_DM", posted[3].Channel)
	assert.Equal(t, "", posted[3].ThreadTS)

	// 6. Version-skew guard (pre-P5a hub with Conversation == nil and dm: ThreadID):
	// an unmapped user DM must not fall through to project-linked channel broadcast.
	require.NoError(t, b.Publish(ctx, "scion.project.p1.user.u2.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:bob@example.com",
		Msg:       "pre-p5a dm for unmapped user",
		Type:      messages.TypeInstruction,
		Channel:   "slack",
		ThreadID:  dmKey,
	}))
	assert.Len(t, getPosted(), 4, "pre-P5a dm: ThreadID for unmapped user must not broadcast")
}

// TestSlackBroker_Publish_PreUpgradeBareTimestampNeverPostsToTS (R-FV1)
// verifies that a conv:<id> reply into a pre-upgrade Slack row (thread:p:<ts>,
// translated to ExternalRef="<ts>" with empty ThreadID) never posts to "<ts>"
// as a channel ID: when ParentRef is empty it falls through to the linked
// channel with thread_ts=<ts>, and when ParentRef is set it composes
// (ParentRef, ExternalRef).
func TestSlackBroker_Publish_PreUpgradeBareTimestampNeverPostsToTS(t *testing.T) {
	srv, getPosted := newTestSlackServer(t)

	b := NewBroker(nil)
	b.client = slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	b.store = newTestStore(t)
	ctx := context.Background()

	require.NoError(t, b.store.CreateChannelLink(ctx, &ChannelLink{
		ChannelID: "CLINK",
		ProjectID: "p1",
		Active:    true,
	}))

	topic := "scion.project.p1.user.u1.messages"

	// 1. Pre-upgrade row with no ParentRef: bare ts must fall through to CLINK, never post to channel "1700000000.000100".
	err := b.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "conv:<id> reply into pre-upgrade slack thread",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-legacy-slack",
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "1700000000.000100",
		},
	})
	require.NoError(t, err)

	// 2. Same row once ParentRef is populated: composes (ParentRef, ExternalRef).
	err = b.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "conv:<id> reply with parent_ref",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-legacy-slack",
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "1700000000.000100",
			ParentRef:   "C0123ABC",
		},
	})
	require.NoError(t, err)

	posted := getPosted()
	require.Len(t, posted, 2)
	assert.Equal(t, "CLINK", posted[0].Channel)
	assert.Equal(t, "1700000000.000100", posted[0].ThreadTS)
	assert.Equal(t, "C0123ABC", posted[1].Channel)
	assert.Equal(t, "1700000000.000100", posted[1].ThreadTS)
}
