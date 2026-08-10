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
	"log/slog"
	"testing"

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
