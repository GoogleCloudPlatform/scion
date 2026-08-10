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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityToStructuredMessage_BasicFields(t *testing.T) {
	activity := &Activity{
		Type:      "message",
		ID:        "act-123",
		Timestamp: "2026-01-15T10:30:00Z",
		Text:      "Hello from Teams!",
		From: ChannelAccount{
			ID:          "user-1",
			Name:        "Alice",
			AadObjectID: "aad-obj-123",
		},
		Recipient: ChannelAccount{
			ID:   "bot-1",
			Name: "ScionBot",
		},
		Conversation: ConversationAccount{
			ID:       "conv-abc",
			TenantID: "tenant-xyz",
		},
		ServiceURL: "https://smba.trafficmanager.net/amer/",
	}

	msg := activityToStructuredMessage(activity, "bot-1")

	require.NotNil(t, msg)
	assert.Equal(t, messages.Version, msg.Version)
	assert.Equal(t, "2026-01-15T10:30:00Z", msg.Timestamp)
	assert.Equal(t, "Alice", msg.Sender)
	assert.Equal(t, "aad-obj-123", msg.SenderID)
	assert.Equal(t, "Hello from Teams!", msg.Msg)
	assert.Equal(t, "chat", msg.Type)

	// Metadata checks.
	assert.Equal(t, "conv-abc", msg.Metadata["teams_conversation_id"])
	assert.Equal(t, "act-123", msg.Metadata["teams_activity_id"])
	assert.Equal(t, "https://smba.trafficmanager.net/amer/", msg.Metadata["teams_service_url"])
	assert.Equal(t, "tenant-xyz", msg.Metadata["teams_tenant_id"])
}

func TestActivityToStructuredMessage_ChannelData(t *testing.T) {
	activity := &Activity{
		Type:      "message",
		ID:        "act-456",
		Timestamp: "2026-01-15T11:00:00Z",
		Text:      "Channel message",
		From: ChannelAccount{
			ID:   "user-2",
			Name: "Bob",
		},
		Conversation: ConversationAccount{ID: "conv-def"},
		ChannelData: &ChannelData{
			TeamsChannelID: "channel-001",
			TeamsTeamID:    "team-001",
			Tenant:         &TenantInfo{ID: "tenant-abc"},
		},
	}

	msg := activityToStructuredMessage(activity, "bot-1")

	assert.Equal(t, "channel-001", msg.Metadata["teams_channel_id"])
	assert.Equal(t, "team-001", msg.Metadata["teams_team_id"])
	assert.Equal(t, "tenant-abc", msg.Metadata["teams_tenant_id"])
}

func TestActivityToStructuredMessage_ReplyToID(t *testing.T) {
	activity := &Activity{
		Type:         "message",
		ID:           "act-789",
		Timestamp:    "2026-01-15T12:00:00Z",
		Text:         "Reply message",
		From:         ChannelAccount{ID: "user-3", Name: "Charlie"},
		Conversation: ConversationAccount{ID: "conv-ghi"},
		ReplyToID:    "parent-act-001",
	}

	msg := activityToStructuredMessage(activity, "bot-1")

	assert.Equal(t, "parent-act-001", msg.ThreadID)
	assert.Equal(t, "parent-act-001", msg.Metadata["teams_reply_to_id"])
}

func TestActivityToStructuredMessage_FallbackSenderID(t *testing.T) {
	// When AadObjectID is empty, fall back to From.ID.
	activity := &Activity{
		Type:         "message",
		ID:           "act-fallback",
		Timestamp:    "2026-01-15T13:00:00Z",
		Text:         "Test",
		From:         ChannelAccount{ID: "user-no-aad", Name: "NoAad"},
		Conversation: ConversationAccount{ID: "conv-j"},
	}

	msg := activityToStructuredMessage(activity, "")

	assert.Equal(t, "user-no-aad", msg.SenderID)
}

func TestActivityToStructuredMessage_EmptyTimestamp(t *testing.T) {
	activity := &Activity{
		Type:         "message",
		ID:           "act-notime",
		Text:         "No timestamp",
		From:         ChannelAccount{ID: "u1", Name: "User"},
		Conversation: ConversationAccount{ID: "c1"},
	}

	msg := activityToStructuredMessage(activity, "")

	// Should have a generated timestamp.
	assert.NotEmpty(t, msg.Timestamp)
}

func TestStripBotMention(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		botID    string
		expected string
	}{
		{
			name:     "mention at start",
			text:     "<at>ScionBot</at> hello world",
			botID:    "bot-1",
			expected: "hello world",
		},
		{
			name:     "mention in middle",
			text:     "hey <at>ScionBot</at> do something",
			botID:    "bot-1",
			expected: "hey do something",
		},
		{
			name:     "multiple mentions",
			text:     "<at>ScionBot</at> <at>Other</at> hello",
			botID:    "bot-1",
			expected: "hello",
		},
		{
			name:     "no mention",
			text:     "plain message",
			botID:    "bot-1",
			expected: "plain message",
		},
		{
			name:     "empty text",
			text:     "",
			botID:    "bot-1",
			expected: "",
		},
		{
			name:     "empty bot ID",
			text:     "<at>ScionBot</at> hello",
			botID:    "",
			expected: "<at>ScionBot</at> hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stripBotMention(tt.text, tt.botID)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStripBotMentionByEntity(t *testing.T) {
	entities := []Entity{
		{
			Type:      "mention",
			Mentioned: ChannelAccount{ID: "bot-1", Name: "ScionBot"},
			Text:      "<at>ScionBot</at>",
		},
		{
			Type:      "mention",
			Mentioned: ChannelAccount{ID: "user-2", Name: "Alice"},
			Text:      "<at>Alice</at>",
		},
	}

	// Should only strip the bot mention, not user mentions.
	text := "<at>ScionBot</at> <at>Alice</at> hello"
	result := stripBotMentionByEntity(text, "bot-1", entities)
	assert.Equal(t, "<at>Alice</at> hello", result)
}

func TestExtractMentionTarget(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected string
	}{
		{"agent slug", "my-agent do the thing", "my-agent"},
		{"with @ prefix", "@my-agent do the thing", "my-agent"},
		{"single word", "hello", ""},
		{"empty", "", ""},
		{"uppercase not slug", "Alice do something", ""},
		{"too short", "a something", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractMentionTarget(tt.text)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsValidAgentSlug(t *testing.T) {
	assert.True(t, isValidAgentSlug("my-agent"))
	assert.True(t, isValidAgentSlug("agent_123"))
	assert.True(t, isValidAgentSlug("ab"))
	assert.False(t, isValidAgentSlug("a"))          // too short
	assert.False(t, isValidAgentSlug(""))           // empty
	assert.False(t, isValidAgentSlug("HAS-CAPS"))   // uppercase
	assert.False(t, isValidAgentSlug("has spaces")) // spaces
	assert.False(t, isValidAgentSlug("has.dots"))   // dots
}
