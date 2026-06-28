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

	"github.com/stretchr/testify/assert"
)

func TestResolveTargetAgents_BotMentionOnly(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot please help",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_SingleAgentMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@reviewer check this PR",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MultipleAgentMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder @reviewer both of you look at this",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer", "tester"})
	assert.Equal(t, []string{"coder", "reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_DuplicateMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder @coder help me",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_BotPlusExplicitDefault(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot @coder hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_All(t *testing.T) {
	msg := &TGMessage{
		Text: "@all deploy update",
	}
	known := []string{"coder", "reviewer", "tester"}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", known)
	assert.Equal(t, known, result)
	assert.True(t, isAll)
}

func TestResolveTargetAgents_NoMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "just a regular message",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_UnknownMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@stranger hello",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_NilMessage(t *testing.T) {
	result, isAll := resolveTargetAgents(nil, "ScionHubBot", "coder", []string{"coder"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MentionWithTrailingPunctuation(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder, can you help?",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MentionWithPeriod(t *testing.T) {
	msg := &TGMessage{
		Text: "Hey @reviewer.",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MentionWithExclamation(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder!",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
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
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "", []string{"coder"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_BotAndOtherAgents(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot @reviewer check this",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder", "reviewer"}, result)
	assert.False(t, isAll)
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

func TestExtractAgentFromBotMessage_StateChangeCompleted(t *testing.T) {
	text := "✅ coordinator — Completed\n\nTask finished successfully"
	assert.Equal(t, "coordinator", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_StateChangeRunning(t *testing.T) {
	text := "▶️ coder — Running\n\nWorking on task"
	assert.Equal(t, "coder", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_StateChangeHyphenatedSlug(t *testing.T) {
	text := "⏸️ my-agent — Paused\n\nWaiting for input"
	assert.Equal(t, "my-agent", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_StateChangeUrgent(t *testing.T) {
	text := "[URGENT] ✅ coordinator — Completed\n\nDone"
	assert.Equal(t, "coordinator", extractAgentFromBotMessage(text))
}

func TestExtractAgentFromBotMessage_StateChangeBroadcast(t *testing.T) {
	text := "[Broadcast] ✅ coordinator — Completed\n\nDone"
	assert.Equal(t, "coordinator", extractAgentFromBotMessage(text))
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

// --- utf16Extract tests ---

func TestUtf16Extract_ASCIIOnly(t *testing.T) {
	s := "@ScionHubBot hello"
	got, ok := utf16Extract(s, 0, 12)
	assert.True(t, ok)
	assert.Equal(t, "@ScionHubBot", got)
}

func TestUtf16Extract_ASCIIMidString(t *testing.T) {
	s := "hey @ScionHubBot help"
	got, ok := utf16Extract(s, 4, 12)
	assert.True(t, ok)
	assert.Equal(t, "@ScionHubBot", got)
}

func TestUtf16Extract_EmojiBeforeMention(t *testing.T) {
	// 🎉 is U+1F389 — a supplementary-plane character: 4 bytes in UTF-8, 2 UTF-16 code units.
	// So "🎉 " is 2 + 1 = 3 UTF-16 code units, but 4 + 1 = 5 bytes.
	// Telegram would report offset=3, length=12 for "@ScionHubBot".
	s := "🎉 @ScionHubBot hello"
	got, ok := utf16Extract(s, 3, 12)
	assert.True(t, ok)
	assert.Equal(t, "@ScionHubBot", got)
}

func TestUtf16Extract_MultipleEmojisBeforeMention(t *testing.T) {
	// Two supplementary emoji: "🎉🚀 " = 2+2+1 = 5 UTF-16 code units.
	s := "🎉🚀 @ScionHubBot"
	got, ok := utf16Extract(s, 5, 12)
	assert.True(t, ok)
	assert.Equal(t, "@ScionHubBot", got)
}

func TestUtf16Extract_CJKBeforeMention(t *testing.T) {
	// CJK characters are BMP (1 UTF-16 code unit each, but 3 bytes in UTF-8).
	// "你好 " = 3 UTF-16 code units.
	s := "你好 @ScionHubBot"
	got, ok := utf16Extract(s, 3, 12)
	assert.True(t, ok)
	assert.Equal(t, "@ScionHubBot", got)
}

func TestUtf16Extract_MixedEmojiAndCJK(t *testing.T) {
	// "🎉你好 " = 2 + 1 + 1 + 1 = 5 UTF-16 code units.
	s := "🎉你好 @bot"
	got, ok := utf16Extract(s, 5, 4)
	assert.True(t, ok)
	assert.Equal(t, "@bot", got)
}

func TestUtf16Extract_MentionAtEnd(t *testing.T) {
	s := "hello @bot"
	got, ok := utf16Extract(s, 6, 4)
	assert.True(t, ok)
	assert.Equal(t, "@bot", got)
}

func TestUtf16Extract_OutOfBounds(t *testing.T) {
	s := "short"
	_, ok := utf16Extract(s, 0, 100)
	assert.False(t, ok)
}

func TestUtf16Extract_NegativeOffset(t *testing.T) {
	_, ok := utf16Extract("hello", -1, 3)
	assert.False(t, ok)
}

func TestUtf16Extract_NegativeLength(t *testing.T) {
	_, ok := utf16Extract("hello", 0, -1)
	assert.False(t, ok)
}

func TestUtf16Extract_ZeroLength(t *testing.T) {
	got, ok := utf16Extract("hello", 2, 0)
	assert.True(t, ok)
	assert.Equal(t, "", got)
}

func TestUtf16Extract_EmptyString(t *testing.T) {
	got, ok := utf16Extract("", 0, 0)
	assert.True(t, ok)
	assert.Equal(t, "", got)
}

func TestUtf16Extract_EntireString(t *testing.T) {
	s := "@bot"
	got, ok := utf16Extract(s, 0, 4)
	assert.True(t, ok)
	assert.Equal(t, "@bot", got)
}

// --- Integration: isBotMentioned with emoji ---

func TestIsBotMentioned_EmojiBeforeMention(t *testing.T) {
	// "🎉 @ScionHubBot hello" — emoji is 2 UTF-16 code units, space is 1.
	// Telegram reports mention entity at offset=3, length=12.
	msg := &TGMessage{
		Text: "🎉 @ScionHubBot hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 3, Length: 12},
		},
	}
	assert.True(t, isBotMentioned(msg, "ScionHubBot"))
}

func TestIsBotMentioned_MultipleEmojisBeforeMention(t *testing.T) {
	// "🎉🚀 @ScionHubBot" — offset=5 (2+2+1), length=12.
	msg := &TGMessage{
		Text: "🎉🚀 @ScionHubBot",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 5, Length: 12},
		},
	}
	assert.True(t, isBotMentioned(msg, "ScionHubBot"))
}

// --- hasNonBotUserMention tests ---

func TestHasNonBotUserMention_UserMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@bob what is new",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 4},
		},
	}
	assert.True(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder", "reviewer"}))
}

func TestHasNonBotUserMention_BotMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_AgentMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@coder help me",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 6},
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder", "reviewer"}))
}

func TestHasNonBotUserMention_TextMentionNonBot(t *testing.T) {
	msg := &TGMessage{
		Text: "Bob Smith what is new",
		Entities: []MessageEntity{
			{Type: "text_mention", Offset: 0, Length: 9, User: &TGUser{ID: 12345, FirstName: "Bob", LastName: "Smith"}},
		},
	}
	assert.True(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_TextMentionBot(t *testing.T) {
	msg := &TGMessage{
		Text: "ScionHubBot do something",
		Entities: []MessageEntity{
			{Type: "text_mention", Offset: 0, Length: 11, User: &TGUser{ID: 999, Username: "ScionHubBot", IsBot: true}},
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_NoEntities(t *testing.T) {
	msg := &TGMessage{
		Text: "just a regular message",
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_NilMessage(t *testing.T) {
	assert.False(t, hasNonBotUserMention(nil, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_MixedBotAndUserMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@ScionHubBot @alice hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
			{Type: "mention", Offset: 13, Length: 6},
		},
	}
	// @alice is at offset>0 so it does not block default routing.
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_MentionAtOffsetGtZero(t *testing.T) {
	msg := &TGMessage{
		Text: "hey @bob what do you think",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 4, Length: 4},
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_TextMentionAtOffsetGtZero(t *testing.T) {
	msg := &TGMessage{
		Text: "hey Bob Smith what do you think",
		Entities: []MessageEntity{
			{Type: "text_mention", Offset: 4, Length: 9, User: &TGUser{ID: 12345, FirstName: "Bob", LastName: "Smith"}},
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_CaseInsensitive(t *testing.T) {
	msg := &TGMessage{
		Text: "@scionhubbot hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 12},
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

// --- isPartialMentionEntity tests ---

func TestIsPartialMentionEntity_HyphenAfterEntity(t *testing.T) {
	// "@agent-dev" — entity covers "@agent" (offset 0, length 6), next char is '-'
	assert.True(t, isPartialMentionEntity("@agent-dev hello", 0, 6))
}

func TestIsPartialMentionEntity_UnderscoreAfterEntity(t *testing.T) {
	// "@agent_dev" — entity covers "@agent" (offset 0, length 6), next char is '_'
	assert.True(t, isPartialMentionEntity("@agent_dev hello", 0, 6))
}

func TestIsPartialMentionEntity_LetterAfterEntity(t *testing.T) {
	// In theory, if Telegram truncated mid-word.
	assert.True(t, isPartialMentionEntity("@agentdev hello", 0, 4))
}

func TestIsPartialMentionEntity_SpaceAfterEntity(t *testing.T) {
	// "@agent hello" — entity covers "@agent", next char is space. Not partial.
	assert.False(t, isPartialMentionEntity("@agent hello", 0, 6))
}

func TestIsPartialMentionEntity_EndOfString(t *testing.T) {
	// "@agent" — entity covers entire string. Not partial.
	assert.False(t, isPartialMentionEntity("@agent", 0, 6))
}

func TestIsPartialMentionEntity_CommaAfterEntity(t *testing.T) {
	// "@agent, hello" — entity covers "@agent", next char is comma. Not partial.
	assert.False(t, isPartialMentionEntity("@agent, hello", 0, 6))
}

func TestIsPartialMentionEntity_MidTextHyphen(t *testing.T) {
	// "hey @agent-dev" — entity at offset 4 covers "@agent", next char is '-'
	assert.True(t, isPartialMentionEntity("hey @agent-dev", 4, 6))
}

func TestIsPartialMentionEntity_EmojiBeforeHyphenatedMention(t *testing.T) {
	// "🎉 @agent-dev" — emoji is 2 UTF-16 code units, space is 1. Entity at offset 3.
	assert.True(t, isPartialMentionEntity("🎉 @agent-dev", 3, 6))
}

func TestIsPartialMentionEntity_DigitAfterEntity(t *testing.T) {
	// "@agent2dev" — entity covers "@agent", next char is '2' (digit).
	assert.True(t, isPartialMentionEntity("@agent2dev hello", 0, 6))
}

func TestIsPartialMentionEntity_MultipleHyphens(t *testing.T) {
	// "@my-cool-agent" — entity covers "@my" (offset 0, length 3), next char is '-'
	assert.True(t, isPartialMentionEntity("@my-cool-agent check", 0, 3))
}

func TestIsPartialMentionEntity_DotAfterEntity(t *testing.T) {
	// "@agent.dev" — entity covers "@agent" (offset 0, length 6), next char is '.' followed by 'd'
	assert.True(t, isPartialMentionEntity("@agent.dev hello", 0, 6))
}

func TestIsPartialMentionEntity_TrailingDot(t *testing.T) {
	// "@agent." — entity covers "@agent", next char is '.' but nothing after it. Not partial.
	assert.False(t, isPartialMentionEntity("@agent.", 0, 6))
}

func TestIsPartialMentionEntity_DotThenSpace(t *testing.T) {
	// "@agent. hello" — entity covers "@agent", next char is '.' followed by space. Not partial.
	assert.False(t, isPartialMentionEntity("@agent. hello", 0, 6))
}

// --- hasNonBotUserMention with hyphenated agent names ---

func TestHasNonBotUserMention_HyphenatedAgentEntityAtStart(t *testing.T) {
	// Telegram creates entity for "@agent" but the user typed "@agent-dev".
	// Since the entity is partial (next char is '-'), it should NOT be treated
	// as a non-bot user mention.
	msg := &TGMessage{
		Text: "@agent-dev hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 6}, // covers "@agent"
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"agent-dev"}))
}

func TestHasNonBotUserMention_HyphenatedNonAgentEntityAtStart(t *testing.T) {
	// Same partial entity, but agent-dev is not known. Still should not be
	// classified as a user mention since the entity is partial.
	msg := &TGMessage{
		Text: "@agent-dev hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 6}, // covers "@agent"
		},
	}
	assert.False(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

func TestHasNonBotUserMention_NonPartialUserMention(t *testing.T) {
	// "@alice hello" — entity covers "@alice" and next char is space.
	// This is a genuine user mention, not partial.
	msg := &TGMessage{
		Text: "@alice hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 6}, // covers "@alice"
		},
	}
	assert.True(t, hasNonBotUserMention(msg, "ScionHubBot", []string{"coder"}))
}

// --- entityMentionSet tests ---

func TestEntityMentionSet_RealUserMention(t *testing.T) {
	msg := &TGMessage{
		Text: "@john hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 5},
		},
	}
	set := entityMentionSet(msg)
	assert.True(t, set["john"])
	assert.Equal(t, 1, len(set))
}

func TestEntityMentionSet_MultipleMentions(t *testing.T) {
	msg := &TGMessage{
		Text: "@alice and @bob hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 6},
			{Type: "mention", Offset: 11, Length: 4},
		},
	}
	set := entityMentionSet(msg)
	assert.True(t, set["alice"])
	assert.True(t, set["bob"])
	assert.Equal(t, 2, len(set))
}

func TestEntityMentionSet_NoEntities(t *testing.T) {
	msg := &TGMessage{
		Text: "@typo hello",
	}
	set := entityMentionSet(msg)
	assert.Equal(t, 0, len(set))
}

func TestEntityMentionSet_IgnoresNonMentionEntities(t *testing.T) {
	msg := &TGMessage{
		Text: "/start @john",
		Entities: []MessageEntity{
			{Type: "bot_command", Offset: 0, Length: 6},
			{Type: "mention", Offset: 7, Length: 5},
		},
	}
	set := entityMentionSet(msg)
	assert.True(t, set["john"])
	assert.Equal(t, 1, len(set))
}

func TestEntityMentionSet_NilMessage(t *testing.T) {
	set := entityMentionSet(nil)
	assert.Equal(t, 0, len(set))
}

func TestEntityMentionSet_CaseInsensitive(t *testing.T) {
	msg := &TGMessage{
		Text: "@John hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 5},
		},
	}
	set := entityMentionSet(msg)
	assert.True(t, set["john"])
}

func TestEntityMentionSet_IgnoresTextMention(t *testing.T) {
	msg := &TGMessage{
		Text: "Bob Smith hello",
		Entities: []MessageEntity{
			{Type: "text_mention", Offset: 0, Length: 9, User: &TGUser{ID: 123, FirstName: "Bob"}},
		},
	}
	set := entityMentionSet(msg)
	assert.Equal(t, 0, len(set))
}

// --- extractUnresolvedMentions tests ---

func TestExtractUnresolvedMentions_TypoAgent(t *testing.T) {
	result := extractUnresolvedMentions("@agent-typo hello", "ScionHubBot", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"agent-typo"}, result)
}

func TestExtractUnresolvedMentions_AllKnown(t *testing.T) {
	result := extractUnresolvedMentions("@coder @reviewer hello", "ScionHubBot", []string{"coder", "reviewer"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_BotFiltered(t *testing.T) {
	result := extractUnresolvedMentions("@ScionHubBot hello", "ScionHubBot", []string{"coder"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_MixedKnownAndUnknown(t *testing.T) {
	result := extractUnresolvedMentions("@coder @agent-typo hello", "ScionHubBot", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"agent-typo"}, result)
}

func TestExtractUnresolvedMentions_MultipleUnknown(t *testing.T) {
	result := extractUnresolvedMentions("@typo1 @typo2 hello", "ScionHubBot", []string{"coder"})
	assert.Equal(t, []string{"typo1", "typo2"}, result)
}

func TestExtractUnresolvedMentions_NoMentions(t *testing.T) {
	result := extractUnresolvedMentions("just regular text", "ScionHubBot", []string{"coder"})
	assert.Nil(t, result)
}

// --- Integration: typo detection vs real user mention ---

func TestTypoDetection_TypoAgentNoEntity(t *testing.T) {
	// @agent-typo with no entity → not a real Telegram user → should be flagged.
	msg := &TGMessage{
		Text: "@agent-typo hello",
	}
	unresolved := extractUnresolvedMentions(msg.Text, "ScionHubBot", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"agent-typo"}, unresolved)

	entityMentions := entityMentionSet(msg)
	var typos []string
	for _, name := range unresolved {
		lower := name
		if len(lower) > 0 {
			lower = strings.ToLower(lower)
		}
		if !entityMentions[lower] {
			typos = append(typos, "@"+name)
		}
	}
	assert.Equal(t, []string{"@agent-typo"}, typos)
}

func TestTypoDetection_RealUserWithEntity(t *testing.T) {
	// @john with a mention entity → real Telegram user → should NOT be flagged.
	msg := &TGMessage{
		Text: "@john hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 5},
		},
	}
	unresolved := extractUnresolvedMentions(msg.Text, "ScionHubBot", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"john"}, unresolved)

	entityMentions := entityMentionSet(msg)
	var typos []string
	for _, name := range unresolved {
		if !entityMentions[strings.ToLower(name)] {
			typos = append(typos, "@"+name)
		}
	}
	assert.Empty(t, typos)
}

func TestTypoDetection_MixedTypoAndRealUser(t *testing.T) {
	// @john (real user with entity) + @agent-typo (no entity) → only agent-typo flagged.
	msg := &TGMessage{
		Text: "@john @agent-typo hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 5},
		},
	}
	unresolved := extractUnresolvedMentions(msg.Text, "ScionHubBot", []string{"coder"})
	assert.Equal(t, []string{"john", "agent-typo"}, unresolved)

	entityMentions := entityMentionSet(msg)
	var typos []string
	for _, name := range unresolved {
		if !entityMentions[strings.ToLower(name)] {
			typos = append(typos, "@"+name)
		}
	}
	assert.Equal(t, []string{"@agent-typo"}, typos)
}

// --- Comprehensive hyphenated agent name tests ---

func TestResolveTargetAgents_HyphenatedAgent(t *testing.T) {
	msg := &TGMessage{
		Text: "@deploy-agent run the deploy",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "deploy-agent"})
	assert.Equal(t, []string{"deploy-agent"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_HyphenatedAgentWithEntity(t *testing.T) {
	// Telegram creates a partial entity for "@deploy" but the user typed "@deploy-agent".
	msg := &TGMessage{
		Text: "@deploy-agent run the deploy",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 7}, // covers "@deploy"
		},
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "deploy-agent"})
	assert.Equal(t, []string{"deploy-agent"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MultipleHyphens(t *testing.T) {
	msg := &TGMessage{
		Text: "@my-cool-agent help me",
	}
	result, isAll := resolveTargetAgents(msg, "ScionHubBot", "coder", []string{"coder", "my-cool-agent"})
	assert.Equal(t, []string{"my-cool-agent"}, result)
	assert.False(t, isAll)
}

func TestExtractAgentMentions_MultipleHyphens(t *testing.T) {
	agents, hasAll := extractAgentMentions("@my-cool-agent check", []string{"my-cool-agent", "coder"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"my-cool-agent"}, agents)
}

func TestExtractAgentMentions_HyphenatedWithTrailingPunctuation(t *testing.T) {
	agents, hasAll := extractAgentMentions("@deploy-agent, please check", []string{"deploy-agent", "coder"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"deploy-agent"}, agents)
}

func TestExtractAgentMentions_MultipleHyphenatedAgents(t *testing.T) {
	agents, hasAll := extractAgentMentions("@deploy-agent @code-reviewer check", []string{"deploy-agent", "code-reviewer", "coder"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"deploy-agent", "code-reviewer"}, agents)
}

func TestExtractAgentMentions_WithDot(t *testing.T) {
	agents, hasAll := extractAgentMentions("@agent.dev check", []string{"agent.dev", "coder"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"agent.dev"}, agents)
}

func TestStripMentions_PreservesCodeBlockIndentation(t *testing.T) {
	result := stripMentions("@bot ```go\n    fmt.Println(\"hello\")\n```", "bot", nil)
	assert.Equal(t, "```go\n    fmt.Println(\"hello\")\n```", result)
}

func TestStripMentions_PreservesTabIndentation(t *testing.T) {
	result := stripMentions("@bot check this:\n\tline1\n\t\tline2", "bot", nil)
	assert.Equal(t, "check this:\n\tline1\n\t\tline2", result)
}

func TestStripMentions_PreservesMultipleSpaces(t *testing.T) {
	result := stripMentions("@bot ```\n  a  =  1\n```", "bot", nil)
	assert.Equal(t, "```\n  a  =  1\n```", result)
}

func TestStripMentions_MentionBeforeMultilineCode(t *testing.T) {
	input := "@ScionHubBot @coder ```\n    if x > 0 {\n        return x\n    }\n```"
	expected := "```\n    if x > 0 {\n        return x\n    }\n```"
	result := stripMentions(input, "ScionHubBot", []string{"coder"})
	assert.Equal(t, expected, result)
}

func TestStripMentions_MidTextMentionPreservesNewlines(t *testing.T) {
	result := stripMentions("line one\n@bot line two\nline three", "bot", nil)
	assert.Equal(t, "line one\nline two\nline three", result)
}

func TestStripMentions_HyphenatedAgent(t *testing.T) {
	result := stripMentions("@ScionHubBot @deploy-agent please review this", "ScionHubBot", []string{"deploy-agent"})
	assert.Equal(t, "please review this", result)
}

func TestStripMentions_HyphenatedAgentWithTrailingPunctuation(t *testing.T) {
	result := stripMentions("@deploy-agent, please help", "ScionHubBot", []string{"deploy-agent"})
	assert.Equal(t, ", please help", result)
}

func TestStripMentions_MultipleHyphenatedAgents(t *testing.T) {
	result := stripMentions("@deploy-agent @code-reviewer check this", "ScionHubBot", []string{"deploy-agent", "code-reviewer"})
	assert.Equal(t, "check this", result)
}

func TestExtractUnresolvedMentions_HyphenatedKnownAgent(t *testing.T) {
	// Known hyphenated agent should NOT appear in unresolved.
	result := extractUnresolvedMentions("@deploy-agent hello", "ScionHubBot", []string{"deploy-agent"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_HyphenatedUnknownAgent(t *testing.T) {
	// Unknown hyphenated agent should appear in unresolved with full name.
	result := extractUnresolvedMentions("@deploy-agent hello", "ScionHubBot", []string{"coder"})
	assert.Equal(t, []string{"deploy-agent"}, result)
}
