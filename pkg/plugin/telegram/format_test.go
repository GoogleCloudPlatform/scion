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

package telegram

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
)

func TestFormatMessage_Instruction(t *testing.T) {
	msg := messages.NewInstruction("user:alice", "agent:coder", "please review the code")
	text := FormatMessage(msg)
	assert.Contains(t, text, "user:alice")
	assert.Contains(t, text, "please review the code")
	assert.NotContains(t, text, "Please reply")
}

func TestFormatMessage_InputNeeded(t *testing.T) {
	msg := messages.NewNotification("agent:coder", "user:alice", "need approval", messages.TypeInputNeeded)
	text := FormatMessage(msg)
	assert.Contains(t, text, "🤖 coder")
	assert.Contains(t, text, "need approval")
	assert.Contains(t, text, "Please reply in this chat to respond.")
}

func TestFormatMessage_StateChange(t *testing.T) {
	msg := messages.NewNotification("agent:coder", "user:alice", "task completed", messages.TypeStateChange)
	text := FormatMessage(msg)
	assert.Contains(t, text, "🤖 coder")
	assert.Contains(t, text, "task completed")
}

func TestFormatMessage_AssistantReply(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "here is the solution",
		Type:      messages.TypeAssistantReply,
	}
	text := FormatMessage(msg)
	assert.Contains(t, text, "🤖 coder")
	assert.Contains(t, text, "here is the solution")
}

func TestFormatMessage_UnknownType(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "system",
		Recipient: "user:alice",
		Msg:       "unknown type message",
		Type:      "unknown",
	}
	text := FormatMessage(msg)
	assert.Contains(t, text, "system")
	assert.Contains(t, text, "unknown type message")
}

func TestFormatMessage_Urgent(t *testing.T) {
	msg := messages.NewInstruction("user:alice", "agent:coder", "fix this now")
	msg.Urgent = true
	text := FormatMessage(msg)
	assert.True(t, strings.HasPrefix(text, "[URGENT] "))
	assert.Contains(t, text, "user:alice")
}

func TestFormatMessage_Broadcasted(t *testing.T) {
	msg := messages.NewInstruction("user:alice", "project:all", "attention everyone")
	msg.Broadcasted = true
	text := FormatMessage(msg)
	assert.Contains(t, text, "[Broadcast] ")
	assert.Contains(t, text, "user:alice")
}

func TestFormatMessage_UrgentAndBroadcasted(t *testing.T) {
	msg := messages.NewInstruction("user:alice", "project:all", "critical alert")
	msg.Urgent = true
	msg.Broadcasted = true
	text := FormatMessage(msg)
	assert.True(t, strings.HasPrefix(text, "[URGENT] [Broadcast] "))
}

func TestFormatMessage_WithStatus(t *testing.T) {
	msg := messages.NewNotification("agent:coder", "user:alice", "working", messages.TypeStateChange)
	msg.Status = "THINKING"
	text := FormatMessage(msg)
	assert.Contains(t, text, "[THINKING]")
}

func TestFormatMessage_Truncation(t *testing.T) {
	longBody := strings.Repeat("x", maxTelegramMessageLength+100)
	msg := messages.NewInstruction("user:alice", "agent:coder", longBody)
	text := FormatMessage(msg)
	assert.LessOrEqual(t, len(text), maxTelegramMessageLength)
	assert.True(t, strings.HasSuffix(text, truncationSuffix))
}

func TestFormatMessage_ExactLimit(t *testing.T) {
	// Create a message that's exactly at the limit — should NOT be truncated
	msg := messages.NewInstruction("a", "b", "c")
	text := FormatMessage(msg)
	// This should be well under the limit and not truncated
	assert.NotContains(t, text, truncationSuffix)
}

func TestFormatMessage_Nil(t *testing.T) {
	text := FormatMessage(nil)
	assert.Equal(t, "", text)
}

// --- FormatStateChangeCard tests ---

func TestFormatStateChangeCard_Running(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T14:30:00Z",
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "Reviewing PR #42",
		Type:      messages.TypeStateChange,
		Status:    "running",
		Metadata: map[string]string{
			"project_id": "alpha",
		},
	}
	text := FormatStateChangeCard(msg, "coder")
	assert.Contains(t, text, "<b>🟢 coder — Running</b>")
	assert.Contains(t, text, "📋 Project: alpha")
	assert.Contains(t, text, "🕐 May 13, 2:30 PM UTC")
	assert.Contains(t, text, "Reviewing PR #42")
	assert.NotContains(t, text, "⚠️")
}

func TestFormatStateChangeCard_Error(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T14:30:00Z",
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "Task failed: migration script returned exit code 1",
		Type:      messages.TypeStateChange,
		Status:    "error",
		Metadata: map[string]string{
			"project_id": "alpha",
		},
	}
	text := FormatStateChangeCard(msg, "coder")
	assert.Contains(t, text, "<b>🔴 coder — Error</b>")
	assert.Contains(t, text, "📋 Project: alpha")
	assert.Contains(t, text, "⚠️")
	assert.Contains(t, text, "Task failed: migration script returned exit code 1")
}

func TestFormatStateChangeCard_Idle(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T10:00:00Z",
		Sender:    "agent:worker",
		Recipient: "user:bob",
		Msg:       "Waiting for input",
		Type:      messages.TypeStateChange,
		Status:    "idle",
	}
	text := FormatStateChangeCard(msg, "worker")
	assert.Contains(t, text, "<b>🟡 worker — Idle</b>")
	assert.Contains(t, text, "Waiting for input")
	assert.NotContains(t, text, "📋 Project:")
}

func TestFormatStateChangeCard_Stopped(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T10:00:00Z",
		Sender:    "agent:worker",
		Recipient: "user:bob",
		Msg:       "Agent stopped",
		Type:      messages.TypeStateChange,
		Status:    "stopped",
	}
	text := FormatStateChangeCard(msg, "worker")
	assert.Contains(t, text, "<b>🟡 worker — Stopped</b>")
}

func TestFormatStateChangeCard_Starting(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T09:00:00Z",
		Sender:    "agent:deployer",
		Recipient: "user:alice",
		Msg:       "Initializing workspace",
		Type:      messages.TypeStateChange,
		Status:    "starting",
		Metadata: map[string]string{
			"project_id": "beta",
		},
	}
	text := FormatStateChangeCard(msg, "deployer")
	assert.Contains(t, text, "<b>⏳ deployer — Starting</b>")
	assert.Contains(t, text, "📋 Project: beta")
	assert.Contains(t, text, "Initializing workspace")
}

func TestFormatStateChangeCard_UnknownStatus(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T09:00:00Z",
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "Doing something",
		Type:      messages.TypeStateChange,
		Status:    "THINKING",
	}
	text := FormatStateChangeCard(msg, "coder")
	// Unknown status gets ⚪ emoji and capitalised label.
	assert.Contains(t, text, "<b>⚪ coder — THINKING</b>")
}

func TestFormatStateChangeCard_HTMLEscape(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T09:00:00Z",
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "Check <script>alert('xss')</script> file",
		Type:      messages.TypeStateChange,
		Status:    "running",
		Metadata: map[string]string{
			"project_id": "proj<evil>",
		},
	}
	text := FormatStateChangeCard(msg, "co<der")
	// Agent slug, project, and message body should be escaped.
	assert.Contains(t, text, "co&lt;der")
	assert.Contains(t, text, "proj&lt;evil&gt;")
	assert.Contains(t, text, "&lt;script&gt;alert")
	assert.NotContains(t, text, "<script>")
}

func TestFormatStateChangeCard_Nil(t *testing.T) {
	text := FormatStateChangeCard(nil, "coder")
	assert.Equal(t, "", text)
}

func TestFormatStateChangeCard_EmptySlugFallsBackToSender(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T09:00:00Z",
		Sender:    "agent:fallback-agent",
		Recipient: "user:alice",
		Msg:       "Hello",
		Type:      messages.TypeStateChange,
		Status:    "running",
	}
	text := FormatStateChangeCard(msg, "")
	assert.Contains(t, text, "fallback-agent")
}

func TestFormatStateChangeCard_LongSummaryTruncated(t *testing.T) {
	longMsg := strings.Repeat("a", 300)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T09:00:00Z",
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       longMsg,
		Type:      messages.TypeStateChange,
		Status:    "running",
	}
	text := FormatStateChangeCard(msg, "coder")
	// The task summary should be truncated with "…".
	assert.Contains(t, text, "…")
	assert.LessOrEqual(t, len(text), maxTelegramMessageLength)
}

func TestFormatStateChangeCard_EmptyStatus(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-05-13T09:00:00Z",
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "Some update",
		Type:      messages.TypeStateChange,
		Status:    "",
	}
	text := FormatStateChangeCard(msg, "coder")
	assert.Contains(t, text, "<b>⚪ coder — Unknown</b>")
}

func TestFormatStateChangeCard_NoTimestamp(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "task done",
		Type:      messages.TypeStateChange,
		Status:    "idle",
	}
	text := FormatStateChangeCard(msg, "coder")
	assert.NotContains(t, text, "🕐")
}

func TestFormatTimestamp(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"2026-05-13T14:30:00Z", "May 13, 2:30 PM UTC"},
		{"2026-01-01T00:00:00Z", "Jan 1, 12:00 AM UTC"},
		{"", ""},
		{"not-a-date", "not-a-date"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := formatTimestamp(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}
