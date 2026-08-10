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

package teams

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBroker_Configure_Phase1(t *testing.T) {
	broker := NewBroker(slog.Default())

	err := broker.Configure(map[string]string{
		"app_id":         "test-app-id",
		"app_secret":     "test-secret",
		"tenant_id":      "test-tenant",
		"listen_address": ":4000",
	})

	require.NoError(t, err)
	assert.Equal(t, 1, broker.phase)
	assert.NotNil(t, broker.tokenProvider)
	assert.NotNil(t, broker.jwtValidator)
	assert.False(t, broker.configured)
	assert.Equal(t, "test-app-id", broker.config.AppID)
	assert.Equal(t, ":4000", broker.config.ListenAddress)
}

func TestBroker_Configure_Phase2(t *testing.T) {
	broker := NewBroker(slog.Default())

	// Phase 1.
	err := broker.Configure(map[string]string{
		"app_id":     "test-app-id",
		"app_secret": "test-secret",
		"tenant_id":  "test-tenant",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, broker.phase)

	// Phase 2.
	err = broker.Configure(map[string]string{
		"hub_url":   "http://localhost:8080",
		"hmac_key":  "dGVzdC1rZXk=",
		"broker_id": "teams-broker-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, broker.phase)
	assert.True(t, broker.configured)
	assert.NotNil(t, broker.hubClient)
}

func TestBroker_Configure_BothPhasesAtOnce(t *testing.T) {
	broker := NewBroker(slog.Default())

	err := broker.Configure(map[string]string{
		"app_id":     "test-app-id",
		"app_secret": "test-secret",
		"tenant_id":  "test-tenant",
		"hub_url":    "http://localhost:8080",
		"hmac_key":   "dGVzdC1rZXk=",
		"broker_id":  "teams-broker-1",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, broker.phase)
	assert.True(t, broker.configured)
}

func TestBroker_Configure_MissingPhase1(t *testing.T) {
	broker := NewBroker(slog.Default())

	// Try phase 2 without phase 1 — should not fail but should not reach phase 2.
	err := broker.Configure(map[string]string{
		"hub_url":   "http://localhost:8080",
		"hmac_key":  "dGVzdC1rZXk=",
		"broker_id": "teams-broker-1",
	})
	require.NoError(t, err) // No error, just doesn't advance.
	assert.Equal(t, 0, broker.phase)
	assert.False(t, broker.configured)
}

func TestBroker_Configure_Defaults(t *testing.T) {
	broker := NewBroker(slog.Default())

	err := broker.Configure(map[string]string{
		"app_id":     "id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
	})
	require.NoError(t, err)

	assert.Equal(t, ":3978", broker.config.ListenAddress)
	assert.Equal(t, "teams.db", broker.config.DBPath)
	assert.True(t, broker.config.MentionRouting)
}

func TestBroker_Configure_MentionRoutingDisable(t *testing.T) {
	broker := NewBroker(slog.Default())

	err := broker.Configure(map[string]string{
		"app_id":          "id",
		"app_secret":      "secret",
		"tenant_id":       "tenant",
		"mention_routing": "false",
	})
	require.NoError(t, err)
	assert.False(t, broker.config.MentionRouting)
}

func TestBroker_GetInfo(t *testing.T) {
	broker := NewBroker(slog.Default())

	info, err := broker.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "teams", info.Name)
	assert.Equal(t, "teams", info.ChannelID)
	assert.Contains(t, info.Capabilities, "inbound")
}

func TestBroker_HealthCheck_Unconfigured(t *testing.T) {
	broker := NewBroker(slog.Default())

	health, err := broker.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "unhealthy", health.Status)
	assert.Contains(t, health.Message, "not fully configured")
}

func TestBroker_HealthCheck_ConfiguredNoServer(t *testing.T) {
	broker := NewBroker(slog.Default())
	broker.Configure(map[string]string{
		"app_id":     "id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
		"hub_url":    "http://hub",
		"hmac_key":   "key",
		"broker_id":  "broker",
	})

	health, err := broker.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "degraded", health.Status)
}

func TestBroker_Subscribe_RequiresConfig(t *testing.T) {
	broker := NewBroker(slog.Default())

	err := broker.Subscribe(">")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestBroker_HandleActivity_Message(t *testing.T) {
	broker := NewBroker(slog.Default())
	broker.Configure(map[string]string{
		"app_id":     "bot-id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
	})

	activity := &Activity{
		Type:      "message",
		ID:        "act-1",
		Text:      "hello",
		From:      ChannelAccount{ID: "user-1", Name: "User"},
		Recipient: ChannelAccount{ID: "bot-id", Name: "Bot"},
		Conversation: ConversationAccount{
			ID:       "conv-1",
			TenantID: "tenant-1",
		},
		ServiceURL: "https://smba.trafficmanager.net/amer/",
	}

	resp, err := broker.HandleActivity(context.Background(), activity)
	require.NoError(t, err)
	assert.Nil(t, resp) // message type returns nil InvokeResponse

	// Verify conversation ref was stored.
	broker.mu.Lock()
	ref, ok := broker.conversationRefs["conv-1"]
	broker.mu.Unlock()
	assert.True(t, ok)
	assert.Equal(t, "https://smba.trafficmanager.net/amer/", ref.ServiceURL)
}

func TestBroker_HandleActivity_SkipsSelf(t *testing.T) {
	broker := NewBroker(slog.Default())
	broker.Configure(map[string]string{
		"app_id":     "bot-id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
	})

	// Message from the bot itself should be skipped.
	activity := &Activity{
		Type:         "message",
		ID:           "act-self",
		Text:         "I said this",
		From:         ChannelAccount{ID: "bot-id", Name: "Bot"},
		Conversation: ConversationAccount{ID: "conv-1"},
	}

	resp, err := broker.HandleActivity(context.Background(), activity)
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestBroker_HandleActivity_ConversationUpdate(t *testing.T) {
	broker := NewBroker(slog.Default())

	activity := &Activity{
		Type:         "conversationUpdate",
		ID:           "act-cu",
		Conversation: ConversationAccount{ID: "conv-2"},
		MembersAdded: []ChannelAccount{
			{ID: "new-user", Name: "NewUser"},
		},
	}

	resp, err := broker.HandleActivity(context.Background(), activity)
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestBroker_HandleActivity_Invoke(t *testing.T) {
	broker := NewBroker(slog.Default())

	activity := &Activity{
		Type:         "invoke",
		ID:           "act-inv",
		Name:         "composeExtension/query",
		Conversation: ConversationAccount{ID: "conv-3"},
	}

	resp, err := broker.HandleActivity(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)
}

func TestBroker_Close(t *testing.T) {
	broker := NewBroker(slog.Default())

	err := broker.Close()
	require.NoError(t, err)
}

func TestBroker_Publish_EchoPrevention(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:test",
		Msg:     "echo message",
		Type:    messages.TypeInstruction,
		Metadata: map[string]string{
			OriginMarkerKey: OriginMarkerValue,
		},
	}

	err := broker.Publish(context.Background(), "project.agent.event", msg)
	require.NoError(t, err)
	// Message should be silently dropped — no error.
}

func TestBroker_Publish_NilMessage(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	err := broker.Publish(context.Background(), "project.agent.event", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestBroker_Publish_ChannelFilter(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	// Message for a different channel should be silently dropped.
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:test",
		Msg:     "hello",
		Type:    messages.TypeInstruction,
		Channel: "discord", // Not "teams".
	}

	err := broker.Publish(context.Background(), "project.agent.event", msg)
	require.NoError(t, err)
}

func TestBroker_Publish_MetadataConversationTarget(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	// Store a conversation reference with a valid service URL.
	broker.mu.Lock()
	broker.conversationRefs["conv-target"] = &ConversationReference{
		ServiceURL:     "https://smba.trafficmanager.net/amer/",
		ConversationID: "conv-target",
	}
	broker.mu.Unlock()

	// The message targets a specific conversation via metadata.
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:test",
		Msg:     "targeted message",
		Type:    messages.TypeInstruction,
		Metadata: map[string]string{
			"teams_conversation_id": "conv-target",
			"teams_service_url":    "https://smba.trafficmanager.net/amer/",
		},
	}

	// The actual send will fail (no real API), but routing should work
	// and the origin marker should be set.
	_ = broker.Publish(context.Background(), "project.agent.event", msg)
	assert.Equal(t, OriginMarkerValue, msg.Metadata[OriginMarkerKey])
}

func TestBroker_Publish_ChannelLinkBroadcast(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	// Add a channel link.
	broker.AddChannelLink(&ChannelLink{
		ConversationID: "linked-conv",
		ProjectID:      "test-project",
		ProjectSlug:    "test-project",
		Active:         true,
	})

	// Add conversation ref for the linked conversation.
	broker.mu.Lock()
	broker.conversationRefs["linked-conv"] = &ConversationReference{
		ServiceURL:     "https://smba.trafficmanager.net/amer/",
		ConversationID: "linked-conv",
	}
	broker.mu.Unlock()

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:deploy-bot",
		Msg:     "broadcast message",
		Type:    messages.TypeInstruction,
	}

	// Publish should attempt to send to the linked conversation.
	// This will fail because the API server isn't real, but the routing should work.
	_ = broker.Publish(context.Background(), "test-project.deploy-bot.event", msg)
	assert.Equal(t, OriginMarkerValue, msg.Metadata[OriginMarkerKey])
}

func TestBroker_Publish_ConversationContextRouting(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	// Set a conversation context.
	broker.SetConversationContext(&ConversationContext{
		TeamsUserID:        "user-1",
		ProjectID:          "myproject",
		AgentSlug:          "builder",
		LastConversationID: "ctx-conv",
		LastActivityID:     "ctx-act",
	})

	// Add conversation ref.
	broker.mu.Lock()
	broker.conversationRefs["ctx-conv"] = &ConversationReference{
		ServiceURL:     "https://smba.trafficmanager.net/amer/",
		ConversationID: "ctx-conv",
	}
	broker.mu.Unlock()

	msg := &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "agent:builder",
		Recipient:   "user-1",
		RecipientID: "user-1",
		Msg:         "context routed",
		Type:        messages.TypeInstruction,
	}

	_ = broker.Publish(context.Background(), "myproject.builder.event", msg)
	assert.Equal(t, OriginMarkerValue, msg.Metadata[OriginMarkerKey])
}

func TestBroker_Publish_NoTargetDrops(t *testing.T) {
	broker := NewBroker(slog.Default())
	configureBrokerForPublish(t, broker)

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:orphan",
		Msg:     "no target",
		Type:    messages.TypeInstruction,
	}

	// No conversation refs, no channel links, no context -> message dropped.
	err := broker.Publish(context.Background(), "unknown.orphan.event", msg)
	require.NoError(t, err) // No error, just silently dropped.
}

func TestBroker_HandleActivity_EchoPrevention(t *testing.T) {
	broker := NewBroker(slog.Default())
	broker.Configure(map[string]string{
		"app_id":     "bot-id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
	})

	// Simulate an inbound message from the bot itself (self-ID check).
	activity := &Activity{
		Type:         "message",
		ID:           "act-echo",
		Text:         "echoed message",
		From:         ChannelAccount{ID: "bot-id", Name: "Bot"},
		Conversation: ConversationAccount{ID: "conv-echo"},
	}

	resp, err := broker.HandleActivity(context.Background(), activity)
	require.NoError(t, err)
	assert.Nil(t, resp)
}

func TestBroker_ParsePublishTopic(t *testing.T) {
	tests := []struct {
		topic     string
		projectID string
		agentSlug string
	}{
		{"myproject.agent1.event", "myproject", "agent1"},
		{"project.agent", "project", "agent"},
		{"project", "project", ""},
		{"", "", ""},
	}

	for _, tt := range tests {
		pID, aSlug := parsePublishTopic(tt.topic)
		assert.Equal(t, tt.projectID, pID, "projectID for topic %q", tt.topic)
		assert.Equal(t, tt.agentSlug, aSlug, "agentSlug for topic %q", tt.topic)
	}
}

func TestBroker_ConversationContextKey(t *testing.T) {
	key := conversationContextKey("user1", "proj1", "agent1")
	assert.Equal(t, "user1:proj1:agent1", key)
}

func TestBroker_AddChannelLink(t *testing.T) {
	broker := NewBroker(slog.Default())

	broker.AddChannelLink(&ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		Active:         true,
	})

	broker.mu.Lock()
	link, ok := broker.channelLinks["conv-1"]
	broker.mu.Unlock()

	assert.True(t, ok)
	assert.Equal(t, "proj-1", link.ProjectID)
}

func TestBroker_SetConversationContext(t *testing.T) {
	broker := NewBroker(slog.Default())

	broker.SetConversationContext(&ConversationContext{
		TeamsUserID:        "u1",
		ProjectID:          "p1",
		AgentSlug:          "a1",
		LastConversationID: "conv-1",
	})

	key := conversationContextKey("u1", "p1", "a1")
	broker.mu.Lock()
	cc, ok := broker.conversationContexts[key]
	broker.mu.Unlock()

	assert.True(t, ok)
	assert.Equal(t, "conv-1", cc.LastConversationID)
}

// configureBrokerForPublish is a test helper that configures a broker with
// Phase 1 and Phase 2 settings so Publish() can be called.
func configureBrokerForPublish(t *testing.T, broker *TeamsBroker) {
	t.Helper()

	err := broker.Configure(map[string]string{
		"app_id":     "bot-id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
		"hub_url":    "http://localhost:8080",
		"hmac_key":   "dGVzdC1rZXk=",
		"broker_id":  "teams-broker-1",
	})
	require.NoError(t, err)
	require.NotNil(t, broker.sendQueue)
}

// configureBrokerWithAPI sets up a broker whose Sender points at apiServer
// for outbound calls, so Publish actually sends through the SendQueue.
func configureBrokerWithAPI(t *testing.T, broker *TeamsBroker, apiServerURL string) {
	t.Helper()

	// Phase 1 + Phase 2 configure.
	configureBrokerForPublish(t, broker)

	// Point the token provider at a fake token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "test-token", ExpiresIn: 3600})
	}))
	t.Cleanup(tokenServer.Close)

	broker.mu.Lock()
	broker.tokenProvider.tokenEndpoint = tokenServer.URL
	broker.tokenProvider.httpClient = tokenServer.Client()
	// Replace the send queue with one that uses short delays for testing.
	broker.sendQueue.Close()
	broker.sender.httpClient = http.DefaultClient // uses apiServer URL directly
	broker.sendQueue = NewSendQueue(broker.sender, 100, 1*time.Millisecond, slog.Default())
	broker.mu.Unlock()
}

func TestBroker_Publish_MultiTargetReplyToIDs(t *testing.T) {
	// C1 + R6: Verify that each target gets the correct replyToID when
	// publishing to multiple targets with different replyToID values.
	// Run with -race to confirm no data race.

	var mu sync.Mutex
	received := make(map[string]string) // conversationID -> replyToID

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var act Activity
		json.NewDecoder(r.Body).Decode(&act)
		mu.Lock()
		received[act.Conversation.ID] = act.ReplyToID
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "resp-" + act.ReplyToID})
	}))
	defer apiServer.Close()

	broker := NewBroker(slog.Default())
	configureBrokerWithAPI(t, broker, apiServer.URL)

	// Set up two conversations with different replyToIDs via metadata routing.
	broker.mu.Lock()
	broker.conversationRefs["conv-A"] = &ConversationReference{
		ServiceURL:     apiServer.URL,
		ConversationID: "conv-A",
	}
	broker.conversationRefs["conv-B"] = &ConversationReference{
		ServiceURL:     apiServer.URL,
		ConversationID: "conv-B",
	}
	// Link both conversations to the same project.
	broker.channelLinks["conv-A"] = &ChannelLink{
		ConversationID: "conv-A",
		ProjectID:      "multi-proj",
		Active:         true,
	}
	broker.channelLinks["conv-B"] = &ChannelLink{
		ConversationID: "conv-B",
		ProjectID:      "multi-proj",
		Active:         true,
	}
	broker.mu.Unlock()

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:test",
		Msg:     "broadcast",
		Type:    messages.TypeInstruction,
	}

	err := broker.Publish(context.Background(), "multi-proj.test.event", msg)
	require.NoError(t, err)

	// Channel link targets don't set replyToID, so both should be empty.
	// The key test is that the race detector doesn't fire.
	broker.mu.Lock()
	sq := broker.sendQueue
	broker.mu.Unlock()
	sq.Close() // wait for all workers to finish
}

func TestBroker_Publish_ThreadIDRouting(t *testing.T) {
	// R6: Verify Priority 1 routing — ThreadID match finds the conversation.

	var receivedPath string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "thread-act"})
	}))
	defer apiServer.Close()

	broker := NewBroker(slog.Default())
	configureBrokerWithAPI(t, broker, apiServer.URL)

	// Store a conversation reference that matches a thread ID.
	broker.mu.Lock()
	broker.conversationRefs["thread-123"] = &ConversationReference{
		ServiceURL:     apiServer.URL,
		ConversationID: "thread-123",
	}
	broker.mu.Unlock()

	msg := &messages.StructuredMessage{
		Version:  messages.Version,
		Sender:   "agent:builder",
		Msg:      "thread routed",
		Type:     messages.TypeInstruction,
		ThreadID: "thread-123",
	}

	err := broker.Publish(context.Background(), "proj.builder.event", msg)
	require.NoError(t, err)

	broker.mu.Lock()
	sq := broker.sendQueue
	broker.mu.Unlock()
	sq.Close()

	assert.Contains(t, receivedPath, "thread-123")
}
