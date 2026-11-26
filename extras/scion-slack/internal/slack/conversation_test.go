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

	// 3. Native conversation surface is ignored by external Slack plugin.
	err = b.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "native webchat message",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "thread:p1:topic-1",
		},
	})
	require.NoError(t, err)
	assert.Len(t, getPosted(), 1)

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
	require.Len(t, posted, 2)
	assert.Equal(t, "C9999XYZ", posted[1].Channel)
	assert.Equal(t, "1700000002.000200", posted[1].ThreadTS)
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

	he := b.deliverInbound("scion.project.p1.agent.coder.messages", msg)
	require.Nil(t, he)

	res, he := b.deliverRoutedInbound("p1", "coder", msg)
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
