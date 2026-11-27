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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
		Metadata: map[string]string{
			"slack_channel_id": "C0123ABC",
			"slack_thread_ts":  "1700000000.000100",
		},
	}

	// 1. Without hub_inbound_conversation capability flag (older hub):
	// inbound payloads must omit surface, external_ref, parent_ref, display_name.
	he := b.deliverInbound("scion.project.p1.agent.coder.messages", msg)
	require.Nil(t, he)
	res, he := b.deliverRoutedInbound("p1", "coder", msg)
	require.Nil(t, he)
	require.NotNil(t, res)
	assert.True(t, res.Delivered)

	mu.Lock()
	for _, field := range []string{"surface", "external_ref", "parent_ref", "display_name"} {
		_, hasIn := gotInbound[field]
		assert.False(t, hasIn, "older hub (no flag) must omit %q on /broker/inbound", field)
		_, hasRt := gotRouted[field]
		assert.False(t, hasRt, "older hub (no flag) must omit %q on /broker/inbound/routed", field)
	}
	gotInbound = nil
	gotRouted = nil
	mu.Unlock()

	// 2. With hub_inbound_conversation="1" (P4+ hub):
	// inbound payloads include surface, external_ref, and parent_ref.
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
	defer mu.Unlock()
	require.NotNil(t, gotInbound)
	assert.Equal(t, "slack", gotInbound["surface"])
	assert.Equal(t, "C0123ABC:1700000000.000100", gotInbound["external_ref"])
	assert.Equal(t, "C0123ABC", gotInbound["parent_ref"])

	require.NotNil(t, gotRouted)
	assert.Equal(t, "slack", gotRouted["surface"])
	assert.Equal(t, "C0123ABC:1700000000.000100", gotRouted["external_ref"])
	assert.Equal(t, "C0123ABC", gotRouted["parent_ref"])
}

// TestSlackBroker_Publish_HubEnrichedOptionAShapes drives the Slack broker
// with the exact outbound shapes emitted by MessageBrokerProxy.enrichConversation
// under Option A: (1) an external thread reply carrying translated surface-native
// Conversation.ExternalRef alongside legacy Channel/ThreadID, and (2) a DM reply
// with Conversation omitted (nil) and reply-affinity Channel:"slack".
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

	topic := "scion.project.p1.user.u1.messages"

	// 1. Outbound `--channel slack --thread-id C0123ABC:1700000000.000100`
	// after Option A hub translation (thread:<proj>: stripped).
	err := b.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "reply on slack thread",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ThreadID:       "C0123ABC:1700000000.000100",
		ConversationID: "conv-slack-thread",
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
			ParentRef:   "C0123ABC",
		},
	})
	require.NoError(t, err)

	// 2. Outbound DM reply with affinity Channel:"slack": hub omits
	// Conversation (nil) while preserving ConversationID and legacy fields.
	err = b.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "dm reply via slack affinity",
		Type:           messages.TypeInstruction,
		Channel:        "slack",
		ConversationID: "conv-dm-1",
		Conversation:   nil,
	})
	require.NoError(t, err)

	posted := getPosted()
	require.Len(t, posted, 2)
	assert.Equal(t, "C0123ABC", posted[0].Channel)
	assert.Equal(t, "1700000000.000100", posted[0].ThreadTS)
	assert.Equal(t, "D_ALICE_DM", posted[1].Channel)
	assert.Equal(t, "", posted[1].ThreadTS)
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
