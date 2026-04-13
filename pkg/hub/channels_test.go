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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingChannel is a mock NotificationChannel that records deliveries.
type recordingChannel struct {
	mu         sync.Mutex
	name       string
	deliveries []*messages.StructuredMessage
	returnErr  error
	validErr   error
}

func (r *recordingChannel) Name() string    { return r.name }
func (r *recordingChannel) Validate() error { return r.validErr }
func (r *recordingChannel) Deliver(_ context.Context, msg *messages.StructuredMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliveries = append(r.deliveries, msg)
	return r.returnErr
}

func (r *recordingChannel) getDeliveries() []*messages.StructuredMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*messages.StructuredMessage, len(r.deliveries))
	copy(result, r.deliveries)
	return result
}

func TestChannelRegistry_Dispatch(t *testing.T) {
	ch := &recordingChannel{name: "test"}
	registry := &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs:  []ChannelConfig{{Type: "test"}},
		log:      slog.Default(),
	}

	msg := messages.NewNotification("agent:worker", "user:alice", "task completed", messages.TypeStateChange)
	registry.Dispatch(context.Background(), msg)

	deliveries := ch.getDeliveries()
	require.Len(t, deliveries, 1)
	assert.Equal(t, "agent:worker", deliveries[0].Sender)
	assert.Equal(t, "task completed", deliveries[0].Msg)
}

func TestChannelRegistry_FilterTypes(t *testing.T) {
	ch := &recordingChannel{name: "test"}
	registry := &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs: []ChannelConfig{{
			Type:        "test",
			FilterTypes: []string{"input-needed"},
		}},
		log: slog.Default(),
	}

	// state-change should be filtered out
	msg1 := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	registry.Dispatch(context.Background(), msg1)
	assert.Empty(t, ch.getDeliveries())

	// input-needed should pass
	msg2 := messages.NewNotification("agent:worker", "user:alice", "need help", messages.TypeInputNeeded)
	registry.Dispatch(context.Background(), msg2)
	assert.Len(t, ch.getDeliveries(), 1)
}

func TestChannelRegistry_FilterUrgentOnly(t *testing.T) {
	ch := &recordingChannel{name: "test"}
	registry := &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs: []ChannelConfig{{
			Type:             "test",
			FilterUrgentOnly: true,
		}},
		log: slog.Default(),
	}

	// Non-urgent should be filtered
	msg1 := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	registry.Dispatch(context.Background(), msg1)
	assert.Empty(t, ch.getDeliveries())

	// Urgent should pass
	msg2 := messages.NewNotification("agent:worker", "user:alice", "urgent!", messages.TypeStateChange)
	msg2.Urgent = true
	registry.Dispatch(context.Background(), msg2)
	assert.Len(t, ch.getDeliveries(), 1)
}

func TestChannelRegistry_DeliveryError(t *testing.T) {
	ch := &recordingChannel{name: "failing", returnErr: fmt.Errorf("connection refused")}
	registry := &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs:  []ChannelConfig{{Type: "failing"}},
		log:      slog.Default(),
	}

	// Should not panic — errors are logged, not propagated
	msg := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	registry.Dispatch(context.Background(), msg)

	// Delivery was still attempted
	assert.Len(t, ch.getDeliveries(), 1)
}

func TestChannelRegistry_MultipleChannels(t *testing.T) {
	ch1 := &recordingChannel{name: "ch1"}
	ch2 := &recordingChannel{name: "ch2"}
	registry := &ChannelRegistry{
		channels: []NotificationChannel{ch1, ch2},
		configs:  []ChannelConfig{{Type: "ch1"}, {Type: "ch2"}},
		log:      slog.Default(),
	}

	msg := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	registry.Dispatch(context.Background(), msg)

	assert.Len(t, ch1.getDeliveries(), 1)
	assert.Len(t, ch2.getDeliveries(), 1)
}

func TestNewChannelRegistry_InvalidType(t *testing.T) {
	configs := []ChannelConfig{
		{Type: "nonexistent", Params: map[string]string{}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 0, registry.Len())
}

func TestNewChannelRegistry_InvalidConfig(t *testing.T) {
	// Webhook without URL should fail validation
	configs := []ChannelConfig{
		{Type: "webhook", Params: map[string]string{}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 0, registry.Len())
}

func TestNewChannelRegistry_ValidWebhook(t *testing.T) {
	configs := []ChannelConfig{
		{Type: "webhook", Params: map[string]string{"url": "https://example.com/hook"}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 1, registry.Len())
}

func TestNewChannelRegistry_ValidSlack(t *testing.T) {
	configs := []ChannelConfig{
		{Type: "slack", Params: map[string]string{"webhook_url": "https://hooks.slack.com/services/T00/B00/xxx"}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 1, registry.Len())
}

func TestNewChannelRegistry_MixedValid(t *testing.T) {
	configs := []ChannelConfig{
		{Type: "webhook", Params: map[string]string{"url": "https://example.com/hook"}},
		{Type: "nonexistent"}, // invalid - skipped
		{Type: "slack", Params: map[string]string{"webhook_url": "https://hooks.slack.com/services/T00/B00/xxx"}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 2, registry.Len())
}

func TestWebhookChannel_Deliver(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = buf
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ch := NewWebhookChannel(map[string]string{
		"url":     server.URL,
		"headers": "Authorization=Bearer test-token",
	})

	msg := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	// Verify the payload is the full structured message
	var got messages.StructuredMessage
	require.NoError(t, json.Unmarshal(received, &got))
	assert.Equal(t, "agent:worker", got.Sender)
	assert.Equal(t, "completed", got.Msg)
	assert.Equal(t, messages.TypeStateChange, got.Type)
}

func TestWebhookChannel_DeliverFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ch := NewWebhookChannel(map[string]string{"url": server.URL})
	msg := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	err := ch.Deliver(context.Background(), msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestWebhookChannel_Validate(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		wantErr bool
	}{
		{"valid https", map[string]string{"url": "https://example.com/hook"}, false},
		{"valid http", map[string]string{"url": "http://localhost:8080/hook"}, false},
		{"missing url", map[string]string{}, true},
		{"invalid url", map[string]string{"url": "ftp://example.com"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := NewWebhookChannel(tt.params)
			err := ch.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewChannelRegistry_ValidEmail(t *testing.T) {
	configs := []ChannelConfig{
		{Type: "email", Params: map[string]string{
			"host": "smtp.example.com",
			"from": "noreply@example.com",
			"to":   "admin@example.com",
		}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 1, registry.Len())
}

func TestEmailChannel_Validate(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		wantErr bool
	}{
		{
			"valid",
			map[string]string{"host": "smtp.example.com", "from": "a@b.com", "to": "c@d.com"},
			false,
		},
		{"missing host", map[string]string{"from": "a@b.com", "to": "c@d.com"}, true},
		{"missing from", map[string]string{"host": "smtp.example.com", "to": "c@d.com"}, true},
		{"missing to", map[string]string{"host": "smtp.example.com", "from": "a@b.com"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := NewEmailChannel(tt.params)
			err := ch.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestEmailChannel_DefaultPort(t *testing.T) {
	ch := NewEmailChannel(map[string]string{
		"host": "smtp.example.com",
		"from": "a@b.com",
		"to":   "c@d.com",
	})
	assert.Equal(t, "587", ch.port)
}

func TestEmailChannel_CustomPort(t *testing.T) {
	ch := NewEmailChannel(map[string]string{
		"host": "smtp.example.com",
		"port": "465",
		"from": "a@b.com",
		"to":   "c@d.com",
	})
	assert.Equal(t, "465", ch.port)
}

func TestSlackChannel_Validate(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		wantErr bool
	}{
		{"valid", map[string]string{"webhook_url": "https://hooks.slack.com/services/T00/B00/xxx"}, false},
		{"missing url", map[string]string{}, true},
		{"wrong domain", map[string]string{"webhook_url": "https://example.com/hook"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := NewSlackChannel(tt.params)
			err := ch.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSlackChannel_Deliver(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Override validation for test (Slack requires hooks.slack.com domain)
	ch := &SlackChannel{
		webhookURL:      server.URL,
		channel:         "#test",
		mentionOnUrgent: "@here",
		client:          http.DefaultClient,
	}

	msg := messages.NewNotification("agent:worker", "user:alice", "task done", messages.TypeStateChange)
	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	var payload slackPayload
	require.NoError(t, json.Unmarshal(received, &payload))
	assert.Equal(t, "#test", payload.Channel)
	assert.Contains(t, payload.Text, "agent:worker")
	assert.Contains(t, payload.Text, "task done")
	assert.Contains(t, payload.Text, "state-change")
}

func TestSlackChannel_UrgentMention(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ch := &SlackChannel{
		webhookURL:      server.URL,
		mentionOnUrgent: "@here",
		client:          http.DefaultClient,
	}

	msg := messages.NewNotification("agent:worker", "user:alice", "urgent task", messages.TypeInputNeeded)
	msg.Urgent = true
	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	var payload slackPayload
	require.NoError(t, json.Unmarshal(received, &payload))
	assert.Contains(t, payload.Text, "@here")
	assert.Contains(t, payload.Text, ":raising_hand:")
}

func TestFormatSlackMessage(t *testing.T) {
	tests := []struct {
		name            string
		msg             *messages.StructuredMessage
		mentionOnUrgent string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "state change",
			msg:          messages.NewNotification("agent:dev", "user:alice", "completed task", messages.TypeStateChange),
			wantContains: []string{":information_source:", "state-change", "agent:dev", "completed task"},
		},
		{
			name:         "input needed",
			msg:          messages.NewNotification("agent:dev", "user:alice", "need help", messages.TypeInputNeeded),
			wantContains: []string{":raising_hand:", "input-needed", "need help"},
		},
		{
			name: "urgent with mention",
			msg: func() *messages.StructuredMessage {
				m := messages.NewNotification("agent:dev", "user:alice", "fire!", messages.TypeStateChange)
				m.Urgent = true
				return m
			}(),
			mentionOnUrgent: "@channel",
			wantContains:    []string{"@channel", "fire!"},
		},
		{
			name:            "not urgent, no mention",
			msg:             messages.NewNotification("agent:dev", "user:alice", "all good", messages.TypeStateChange),
			mentionOnUrgent: "@here",
			wantNotContains: []string{"@here"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatSlackMessage(tt.msg, tt.mentionOnUrgent)
			for _, s := range tt.wantContains {
				assert.Contains(t, result, s)
			}
			for _, s := range tt.wantNotContains {
				assert.NotContains(t, result, s)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Discord channel tests
// ---------------------------------------------------------------------------

func TestNewChannelRegistry_ValidDiscord(t *testing.T) {
	configs := []ChannelConfig{
		{Type: "discord", Params: map[string]string{"webhook_url": "https://discord.com/api/webhooks/123456789/abcDEF_token"}},
	}
	registry := NewChannelRegistry(configs, slog.Default())
	assert.Equal(t, 1, registry.Len())
}

func TestDiscordChannel_Validate(t *testing.T) {
	tests := []struct {
		name        string
		params      map[string]string
		wantErr     bool
		errContains string
	}{
		{"valid discord.com", map[string]string{"webhook_url": "https://discord.com/api/webhooks/123/abc"}, false, ""},
		{"valid discordapp.com", map[string]string{"webhook_url": "https://discordapp.com/api/webhooks/123/abc"}, false, ""},
		{"valid ptb.discord.com", map[string]string{"webhook_url": "https://ptb.discord.com/api/webhooks/123/abc"}, false, ""},
		{"valid canary.discord.com", map[string]string{"webhook_url": "https://canary.discord.com/api/webhooks/123/abc"}, false, ""},
		{"missing url", map[string]string{}, true, ""},
		{"wrong scheme", map[string]string{"webhook_url": "http://discord.com/api/webhooks/123/abc"}, true, ""},
		{"wrong domain", map[string]string{"webhook_url": "https://example.com/api/webhooks/123/abc"}, true, ""},
		{"slack suffix rejected", map[string]string{"webhook_url": "https://discord.com/api/webhooks/123/abc/slack"}, true, "slack"},
		{"github domain", map[string]string{"webhook_url": "https://hooks.slack.com/services/T00/B00/xxx"}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := NewDiscordChannel(tt.params)
			err := ch.Validate()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDiscordChannel_Deliver(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = buf
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ch := &DiscordChannel{webhookURL: server.URL, client: http.DefaultClient}

	msg := messages.NewNotification("agent:worker", "user:alice", "task done", messages.TypeStateChange)
	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	var payload discordPayload
	require.NoError(t, json.Unmarshal(received, &payload))
	require.Len(t, payload.Embeds, 1)
	assert.Equal(t, discordColorStateChange, payload.Embeds[0].Color)
	assert.Contains(t, payload.Embeds[0].Description, "task done")
	assert.Contains(t, payload.Embeds[0].Title, "agent:worker")
	assert.Equal(t, "", payload.Content)
	// AllowedMentions must be set with parse: [] to prevent @everyone/@here
	require.NotNil(t, payload.AllowedMentions)
	parsedJSON, err := json.Marshal(payload.AllowedMentions)
	require.NoError(t, err)
	assert.Contains(t, string(parsedJSON), `"parse":[]`)
}

func TestDiscordChannel_UrgentMention(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		received = buf
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ch := &DiscordChannel{
		webhookURL:      server.URL,
		mentionOnUrgent: "<@&9876543210>",
		client:          http.DefaultClient,
	}

	msg := messages.NewNotification("agent:worker", "user:alice", "urgent task", messages.TypeInputNeeded)
	msg.Urgent = true
	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	var payload discordPayload
	require.NoError(t, json.Unmarshal(received, &payload))
	assert.Contains(t, payload.Content, "<@&9876543210>")
	// Urgent messages use the dedicated urgent colour
	assert.Equal(t, discordColorUrgent, payload.Embeds[0].Color)
	require.NotNil(t, payload.AllowedMentions)
	assert.Contains(t, payload.AllowedMentions.Roles, "9876543210")
	assert.NotContains(t, payload.AllowedMentions.Parse, "everyone")
	assert.NotContains(t, payload.AllowedMentions.Parse, "users")
}

func TestDiscordChannel_ColorByType(t *testing.T) {
	tests := []struct {
		msgType   string
		wantColor int
	}{
		{messages.TypeStateChange, discordColorStateChange},
		{messages.TypeInputNeeded, discordColorInputNeeded},
		{messages.TypeInstruction, discordColorInstruction},
	}
	for _, tt := range tests {
		t.Run(tt.msgType, func(t *testing.T) {
			msg := messages.NewNotification("agent:worker", "user:alice", "msg", tt.msgType)
			payload := formatDiscordPayload(msg, "")
			require.Len(t, payload.Embeds, 1)
			assert.Equal(t, tt.wantColor, payload.Embeds[0].Color)
		})
	}
}

func TestDiscordChannel_TruncateLongMsg(t *testing.T) {
	longMsg := strings.Repeat("A", 5000)
	msg := messages.NewNotification("agent:worker", "user:alice", longMsg, messages.TypeStateChange)
	payload := formatDiscordPayload(msg, "")
	require.Len(t, payload.Embeds, 1)
	assert.LessOrEqual(t, len([]rune(payload.Embeds[0].Description)), 2048)
	assert.True(t,
		strings.HasSuffix(payload.Embeds[0].Description, "...") ||
			strings.HasSuffix(payload.Embeds[0].Description, "…(truncated)"),
		"description should end with an ellipsis marker")
	// Total embed text should be under 6000 chars
	totalLen := len(payload.Embeds[0].Title) + len(payload.Embeds[0].Description)
	assert.Less(t, totalLen, 6000)
}

func TestDiscordChannel_DeliverFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ch := &DiscordChannel{webhookURL: server.URL, client: http.DefaultClient}
	msg := messages.NewNotification("agent:worker", "user:alice", "completed", messages.TypeStateChange)
	err := ch.Deliver(context.Background(), msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestFormatDiscordPayload_URLRejectsSlackSuffix(t *testing.T) {
	ch := NewDiscordChannel(map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/2/slack"})
	err := ch.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slack")
}
