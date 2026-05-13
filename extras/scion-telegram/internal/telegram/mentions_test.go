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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveTargetAgents_BotMentionOnly(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot please help",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
}

func TestResolveTargetAgents_SingleAgentMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@reviewer check this PR",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"reviewer"}, result)
}

func TestResolveTargetAgents_MultipleAgentMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder @reviewer both of you look at this",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer", "tester"})
	assert.Equal(t, []string{"coder", "reviewer"}, result)
}

func TestResolveTargetAgents_DuplicateMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder @coder help me",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
}

func TestResolveTargetAgents_BotPlusExplicitDefault(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot @coder hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
}

func TestResolveTargetAgents_All(t *testing.T) {
	msg := &TGMessage{
		Text: "@all deploy update",
	}
	known := []string{"coder", "reviewer", "tester"}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", known)
	assert.Equal(t, known, result)
}

func TestResolveTargetAgents_NoMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "just a regular message",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Nil(t, result)
}

func TestResolveTargetAgents_UnknownMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@stranger hello",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Nil(t, result)
}

func TestResolveTargetAgents_NilMessage(t *testing.T) {
	result := resolveTargetAgents(nil, "ScionHubBot", "coder", []string{"coder"})
	assert.Nil(t, result)
}

func TestResolveTargetAgents_MentionWithTrailingPunctuation(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder, can you help?",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
}

func TestResolveTargetAgents_MentionWithPeriod(t *testing.T) {
	msg := &TGMessage{
		Text: "Hey @reviewer.",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"reviewer"}, result)
}

func TestResolveTargetAgents_MentionWithExclamation(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder!",
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder"})
	assert.Equal(t, []string{"coder"}, result)
}

func TestIsBotMentioned_CaseInsensitive(t *testing.T) {
	msg := &TGMessage{
		Text: "@scionhubbot hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	assert.True(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestIsBotMentioned_UpperCase(t *testing.T) {
	msg := &TGMessage{
		Text: "@SCIONHUBBOT hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	assert.True(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestIsBotMentioned_NoEntities(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot hello",
	}
	assert.False(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestIsBotMentioned_WrongEntityType(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot hello",
		Entities: []MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 12},
		},
	}
	assert.False(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestIsBotMentioned_NilMessage(t *testing.T) {
	assert.False(t, isBotMentioned(nil, "ScionHubBot"))
}

func TestIsBotMentioned_EmptyBotUsername(t *testing.T) {
	msg := &TGMessage{Text: "hello"}
	assert.False(t, isBotMentioned(msg, ""))
}

func TestIsBotMentioned_MidText(t *testing.T) {
	msg := &TGMessage{
		Text: "hey @ScionHubBot help",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 4, Length: 12},
		},
	}
	assert.True(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestIsBotMentioned_InvalidOffset(t *testing.T) {
	msg := &TGMessage{
		Text: "short",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 100},
		},
	}
	assert.False(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestExtractAgentMentions_Basic(t *testing.T) {
	agents, hasAll := extractAgentMentions("@coder help me", []string{"coder", "reviewer"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"coder"}, agents)
}

func TestExtractAgentMentions_All(t *testing.T) {
	agents, hasAll := extractAgentMentions("@all deploy now", []string{"coder", "reviewer"})
	assert.True(t, hasAll)
	assert.Nil(t, agents)
}

func TestExtractAgentMentions_UnknownAgent(t *testing.T) {
	agents, hasAll := extractAgentMentions("@unknown hello", []string{"coder", "reviewer"})
	assert.False(t, hasAll)
	assert.Nil(t, agents)
}

func TestExtractAgentMentions_WithUnderscore(t *testing.T) {
	agents, hasAll := extractAgentMentions("@code_reviewer check", []string{"code_reviewer", "coder"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"code_reviewer"}, agents)
}

func TestExtractAgentMentions_WithHyphen(t *testing.T) {
	agents, hasAll := extractAgentMentions("@my-agent check", []string{"my-agent", "coder"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"my-agent"}, agents)
}

func TestStripMentions_BotAndAgent(t *testing.T) {
	result := stripMentions("@ScionHubBot @coder please review this", "ScionHubBot", []string{"coder"})
	assert.Equal(t, "please review this", result)
}

func TestStripMentions_OnlyBot(t *testing.T) {
	result := stripMentions("@ScionHubBot hello world", "ScionHubBot", nil)
	assert.Equal(t, "hello world", result)
}

func TestStripMentions_PreservesUnknownMentions(t *testing.T) {
	result := stripMentions("@ScionHubBot @stranger hello", "ScionHubBot", []string{"coder"})
	assert.Equal(t, "@stranger hello", result)
}

func TestStripMentions_WithTrailingPunctuation(t *testing.T) {
	result := stripMentions("@coder, please help", "ScionHubBot", []string{"coder"})
	assert.Equal(t, ", please help", result)
}

func TestStripMentions_AllMention(t *testing.T) {
	result := stripMentions("@all attention please", "ScionHubBot", []string{"coder"})
	assert.Equal(t, "attention please", result)
}

func TestStripMentions_EmptyAfterStrip(t *testing.T) {
	result := stripMentions("@coder", "ScionHubBot", []string{"coder"})
	assert.Equal(t, "", result)
}

func TestStripMentions_NoMentions(t *testing.T) {
	result := stripMentions("just regular text", "ScionHubBot", []string{"coder"})
	assert.Equal(t, "just regular text", result)
}

func TestResolveTargetAgents_BotMentionEmptyDefault(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "", []string{"coder"})
	assert.Nil(t, result)
}

func TestResolveTargetAgents_BotAndOtherAgents(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot @reviewer check this",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder", "reviewer"}, result)
}

// --- extractAgentFromBotMessage tests ---

func TestExtractAgentFromBotMessage_StandardFormat(t *testing.T) {
	text := "🤖 coder\n\nSome message body here"
	assert.Equal(t, "coder", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_WithUrgentPrefix(t *testing.T) {
	text := "[URGENT] 🤖 coder\n\nSomething urgent happened"
	assert.Equal(t, "coder", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_WithBroadcastPrefix(t *testing.T) {
	text := "[Broadcast] 🤖 coder\n\nBroadcast message"
	assert.Equal(t, "coder", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_WithUrgentAndBroadcast(t *testing.T) {
	text := "[URGENT] [Broadcast] 🤖 coder\n\nUrgent broadcast"
	assert.Equal(t, "coder", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_WithStatus(t *testing.T) {
	text := "🤖 coder [running]\n\nDoing some work"
	assert.Equal(t, "coder", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_AgentToAgent(t *testing.T) {
	text := "👀 🤖 sender → 🤖 recipient 👀\n\nSome agent-to-agent message"
	assert.Equal(t, "recipient", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_AgentToAgentUrgent(t *testing.T) {
	text := "[URGENT] 👀 🤖 sender → 🤖 recipient 👀\n\nUrgent inter-agent"
	assert.Equal(t, "recipient", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_NoMatch(t *testing.T) {
	assert.Equal(t, "", extractAgentFromBotMessage("Hello world"))
}

func TestExtractAgentFromBotMessage_Empty(t *testing.T) {
	assert.Equal(t, "", extractAgentFromBotMessage(""))
}

func TestExtractAgentFromBotMessage_HyphenatedSlug(t *testing.T) {
	text := "🤖 my-agent\n\nMessage body"
	assert.Equal(t, "my-agent", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_UnderscoreSlug(t *testing.T) {
	text := "🤖 code_reviewer [idle]\n\nWaiting for tasks"
	assert.Equal(t, "code_reviewer", extractAgentFromBotMessage(text))
}
