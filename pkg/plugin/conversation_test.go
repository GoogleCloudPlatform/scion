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

package plugin

import (
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveConversationTarget(t *testing.T) {
	t.Run("nil message", func(t *testing.T) {
		assert.Equal(t, ConversationTarget{}, ResolveConversationTarget(nil))
	})

	t.Run("prefers conversation over legacy fields", func(t *testing.T) {
		msg := &messages.StructuredMessage{
			ConversationID: "conv-123",
			Channel:        "legacy-channel",
			ThreadID:       "legacy-thread",
			Conversation: &messages.Conversation{
				Surface:     "slack",
				ExternalRef: "C0123:1700.001",
				ParentRef:   "C0123",
				DisplayName: "#general",
			},
		}
		got := ResolveConversationTarget(msg)
		assert.Equal(t, ConversationTarget{
			ConversationID: "conv-123",
			Surface:        "slack",
			ExternalRef:    "C0123:1700.001",
			ParentRef:      "C0123",
			DisplayName:    "#general",
		}, got)
	})

	t.Run("falls back to legacy channel and thread_id when conversation is nil", func(t *testing.T) {
		msg := &messages.StructuredMessage{
			ConversationID: "conv-456",
			Channel:        "discord",
			ThreadID:       "987654321",
		}
		got := ResolveConversationTarget(msg)
		assert.Equal(t, ConversationTarget{
			ConversationID: "conv-456",
			Surface:        "discord",
			ExternalRef:    "987654321",
		}, got)
	})

	t.Run("falls back field-by-field when conversation is partial", func(t *testing.T) {
		msg := &messages.StructuredMessage{
			Channel:  "telegram",
			ThreadID: "42",
			Conversation: &messages.Conversation{
				Surface:     "telegram",
				DisplayName: "Ops Chat",
			},
		}
		got := ResolveConversationTarget(msg)
		assert.Equal(t, ConversationTarget{
			Surface:     "telegram",
			ExternalRef: "42",
			DisplayName: "Ops Chat",
		}, got)
	})
	t.Run("ignores native conversation so hub keys never become external targets", func(t *testing.T) {
		msg := &messages.StructuredMessage{
			ConversationID: "conv-dm-1",
			Channel:        "discord",
			ThreadID:       "",
			Conversation: &messages.Conversation{
				Surface:     "native",
				ExternalRef: "dm:agent:a1:user:u1",
				DisplayName: "DM",
			},
		}
		got := ResolveConversationTarget(msg)
		assert.Equal(t, ConversationTarget{
			ConversationID: "conv-dm-1",
			Surface:        "discord",
			ExternalRef:    "",
		}, got)
	})
}

func TestMatchesSurface(t *testing.T) {
	tests := []struct {
		name    string
		msg     *messages.StructuredMessage
		surface string
		want    bool
	}{
		{
			name:    "nil message matches",
			msg:     nil,
			surface: "slack",
			want:    true,
		},
		{
			name:    "untargeted message matches",
			msg:     &messages.StructuredMessage{Msg: "broadcast"},
			surface: "slack",
			want:    true,
		},
		{
			name: "matching conversation surface with empty channel",
			msg: &messages.StructuredMessage{
				Conversation: &messages.Conversation{Surface: "slack", ExternalRef: "C1:1.0"},
			},
			surface: "slack",
			want:    true,
		},
		{
			name: "other conversation surface rejected when channel is empty",
			msg: &messages.StructuredMessage{
				Conversation: &messages.Conversation{Surface: "discord", ExternalRef: "123"},
			},
			surface: "slack",
			want:    false,
		},
		{
			name: "native conversation with empty channel matches (observer mode)",
			msg: &messages.StructuredMessage{
				ObserverOnly: true,
				Conversation: &messages.Conversation{Surface: "native", ExternalRef: "dm:agent:a1:agent:a2"},
			},
			surface: "slack",
			want:    true,
		},
		{
			name: "reply affinity: matching channel with native dm conversation matches",
			msg: &messages.StructuredMessage{
				Channel:      "discord",
				Conversation: &messages.Conversation{Surface: "native", ExternalRef: "dm:agent:a1:user:u1"},
			},
			surface: "discord",
			want:    true,
		},
		{
			name: "legacy channel fallback matches",
			msg: &messages.StructuredMessage{
				Channel: "slack",
			},
			surface: "slack",
			want:    true,
		},
		{
			name: "foreign non-empty channel rejected",
			msg: &messages.StructuredMessage{
				Channel: "web",
			},
			surface: "slack",
			want:    false,
		},
		{
			name: "P4: non-empty channel takes precedence over conversation surface",
			msg: &messages.StructuredMessage{
				Channel:      "discord",
				Conversation: &messages.Conversation{Surface: "slack"},
			},
			surface: "discord",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchesSurface(tt.msg, tt.surface))
		})
	}
}

func TestInboundConversationJSONEmbedding(t *testing.T) {
	type payload struct {
		Topic string `json:"topic"`
		InboundConversation
	}
	raw, err := json.Marshal(payload{
		Topic: "scion.project.p1.agent.coder.messages",
		InboundConversation: InboundConversation{
			Surface:     "slack",
			ExternalRef: "C0123:1700.001",
			ParentRef:   "C0123",
			DisplayName: "#eng",
		},
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, "scion.project.p1.agent.coder.messages", decoded["topic"])
	assert.Equal(t, "slack", decoded["surface"])
	assert.Equal(t, "C0123:1700.001", decoded["external_ref"])
	assert.Equal(t, "C0123", decoded["parent_ref"])
	assert.Equal(t, "#eng", decoded["display_name"])
}
