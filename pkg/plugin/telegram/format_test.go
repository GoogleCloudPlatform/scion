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
	assert.Contains(t, text, "Instruction from user:alice")
	assert.Contains(t, text, "please review the code")
	assert.NotContains(t, text, "Please reply")
}

func TestFormatMessage_InputNeeded(t *testing.T) {
	msg := messages.NewNotification("agent:coder", "user:alice", "need approval", messages.TypeInputNeeded)
	text := FormatMessage(msg)
	assert.Contains(t, text, "Input Needed from agent:coder")
	assert.Contains(t, text, "need approval")
	assert.Contains(t, text, "Please reply in this chat to respond.")
}

func TestFormatMessage_StateChange(t *testing.T) {
	msg := messages.NewNotification("agent:coder", "user:alice", "task completed", messages.TypeStateChange)
	text := FormatMessage(msg)
	assert.Contains(t, text, "State Change from agent:coder")
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
	assert.Contains(t, text, "Reply from agent:coder")
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
	assert.Contains(t, text, "Message from system")
	assert.Contains(t, text, "unknown type message")
}

func TestFormatMessage_Urgent(t *testing.T) {
	msg := messages.NewInstruction("user:alice", "agent:coder", "fix this now")
	msg.Urgent = true
	text := FormatMessage(msg)
	assert.True(t, strings.HasPrefix(text, "[URGENT] "))
	assert.Contains(t, text, "Instruction from user:alice")
}

func TestFormatMessage_Broadcasted(t *testing.T) {
	msg := messages.NewInstruction("user:alice", "project:all", "attention everyone")
	msg.Broadcasted = true
	text := FormatMessage(msg)
	assert.Contains(t, text, "[Broadcast] ")
	assert.Contains(t, text, "Instruction from user:alice")
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
