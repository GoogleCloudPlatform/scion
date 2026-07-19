package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
)

// newMockMessage creates a MessageCreate with the given content and user mentions.
func newMockMessage(content string, mentions []*discordgo.User) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{
		Message: &discordgo.Message{
			Content:  content,
			Mentions: mentions,
		},
	}
}

// --- resolveTargetAgents tests ---

func TestResolveTargetAgents_BotMentionOnly(t *testing.T) {
	msg := newMockMessage("<@BOT123> please help", []*discordgo.User{{ID: "BOT123"}})
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_SingleAgentMention(t *testing.T) {
	msg := newMockMessage("@reviewer check this PR", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MultipleAgentMentions(t *testing.T) {
	msg := newMockMessage("@coder @reviewer both of you look at this", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer", "tester"})
	assert.Equal(t, []string{"coder", "reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_All(t *testing.T) {
	known := []string{"coder", "reviewer", "tester"}
	msg := newMockMessage("@all deploy update", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", known)
	assert.Equal(t, known, result)
	assert.True(t, isAll)
}

func TestResolveTargetAgents_NoMentions(t *testing.T) {
	msg := newMockMessage("just a regular message", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_NilMessage(t *testing.T) {
	result, isAll := resolveTargetAgents(nil, "BOT123", "coder", []string{"coder"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_NilInnerMessage(t *testing.T) {
	msg := &discordgo.MessageCreate{Message: nil}
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_BotPlusAgentMention(t *testing.T) {
	msg := newMockMessage("<@BOT123> @reviewer check this", []*discordgo.User{{ID: "BOT123"}})
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder", "reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_BotPlusExplicitDefault(t *testing.T) {
	// When bot is mentioned and the user also explicitly mentions the default agent,
	// the default agent should appear only once.
	msg := newMockMessage("<@BOT123> @coder hello", []*discordgo.User{{ID: "BOT123"}})
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_DuplicateMentions(t *testing.T) {
	msg := newMockMessage("@coder @coder help me", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_UnknownMention(t *testing.T) {
	msg := newMockMessage("@stranger hello", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_BotMentionEmptyDefault(t *testing.T) {
	msg := newMockMessage("<@BOT123> hello", []*discordgo.User{{ID: "BOT123"}})
	result, isAll := resolveTargetAgents(msg, "BOT123", "", []string{"coder"})
	assert.Nil(t, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MentionWithTrailingPunctuation(t *testing.T) {
	msg := newMockMessage("@coder, can you help?", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MentionWithPeriod(t *testing.T) {
	msg := newMockMessage("Hey @reviewer.", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"reviewer"}, result)
	assert.False(t, isAll)
}

func TestResolveTargetAgents_MentionWithExclamation(t *testing.T) {
	msg := newMockMessage("@coder!", nil)
	result, isAll := resolveTargetAgents(msg, "BOT123", "coder", []string{"coder"})
	assert.Equal(t, []string{"coder"}, result)
	assert.False(t, isAll)
}

// --- isBotMentioned tests ---

func TestIsBotMentioned_Present(t *testing.T) {
	msg := newMockMessage("hello <@BOT123>", []*discordgo.User{{ID: "BOT123"}})
	assert.True(t, isBotMentioned(msg, "BOT123"))
}

func TestIsBotMentioned_NotPresent(t *testing.T) {
	msg := newMockMessage("hello", nil)
	assert.False(t, isBotMentioned(msg, "BOT123"))
}

func TestIsBotMentioned_OtherUser(t *testing.T) {
	msg := newMockMessage("hello <@USER456>", []*discordgo.User{{ID: "USER456"}})
	assert.False(t, isBotMentioned(msg, "BOT123"))
}

func TestIsBotMentioned_MultipleMentions(t *testing.T) {
	msg := newMockMessage("hello <@USER456> <@BOT123>", []*discordgo.User{
		{ID: "USER456"},
		{ID: "BOT123"},
	})
	assert.True(t, isBotMentioned(msg, "BOT123"))
}

func TestIsBotMentioned_NilMessage(t *testing.T) {
	assert.False(t, isBotMentioned(nil, "BOT123"))
}

func TestIsBotMentioned_NilInnerMessage(t *testing.T) {
	msg := &discordgo.MessageCreate{Message: nil}
	assert.False(t, isBotMentioned(msg, "BOT123"))
}

func TestIsBotMentioned_EmptyBotUserID(t *testing.T) {
	msg := newMockMessage("hello", nil)
	assert.False(t, isBotMentioned(msg, ""))
}

// --- extractAgentMentions tests ---

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

func TestExtractAgentMentions_CaseInsensitive(t *testing.T) {
	agents, hasAll := extractAgentMentions("@Coder help", []string{"coder", "reviewer"})
	assert.False(t, hasAll)
	assert.Equal(t, []string{"coder"}, agents)
}

// --- stripMentions tests ---

func TestStripMentions_BotAndAgent(t *testing.T) {
	result := stripMentions("<@BOT123> @coder please review this", "BOT123", []string{"coder"})
	assert.Equal(t, "please review this", result)
}

func TestStripMentions_BotNicknameFormat(t *testing.T) {
	result := stripMentions("<@!BOT123> hello world", "BOT123", nil)
	assert.Equal(t, "hello world", result)
}

func TestStripMentions_OnlyBot(t *testing.T) {
	result := stripMentions("<@BOT123> hello world", "BOT123", nil)
	assert.Equal(t, "hello world", result)
}

func TestStripMentions_PreservesUnknownMentions(t *testing.T) {
	result := stripMentions("<@BOT123> @stranger hello", "BOT123", []string{"coder"})
	assert.Equal(t, "@stranger hello", result)
}

func TestStripMentions_WithTrailingPunctuation(t *testing.T) {
	result := stripMentions("@coder, please help", "BOT123", []string{"coder"})
	assert.Equal(t, ", please help", result)
}

func TestStripMentions_AllMention(t *testing.T) {
	result := stripMentions("@all attention please", "BOT123", []string{"coder"})
	assert.Equal(t, "attention please", result)
}

func TestStripMentions_EmptyAfterStrip(t *testing.T) {
	result := stripMentions("@coder", "BOT123", []string{"coder"})
	assert.Equal(t, "", result)
}

func TestStripMentions_NoMentions(t *testing.T) {
	result := stripMentions("just regular text", "BOT123", []string{"coder"})
	assert.Equal(t, "just regular text", result)
}

func TestStripMentions_BotAndMultipleAgents(t *testing.T) {
	result := stripMentions("<@BOT123> @coder @reviewer do the thing", "BOT123", []string{"coder", "reviewer"})
	assert.Equal(t, "do the thing", result)
}

// --- extractUnresolvedMentions tests ---

func TestExtractUnresolvedMentions_TypoAgent(t *testing.T) {
	result := extractUnresolvedMentions("@agent-typo hello", "BOT123", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"agent-typo"}, result)
}

func TestExtractUnresolvedMentions_AllKnown(t *testing.T) {
	result := extractUnresolvedMentions("@coder @reviewer hello", "BOT123", []string{"coder", "reviewer"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_SkipsBotMentionFormat(t *testing.T) {
	result := extractUnresolvedMentions("<@BOT123> hello", "BOT123", []string{"coder"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_SkipsBotNicknameFormat(t *testing.T) {
	result := extractUnresolvedMentions("<@!BOT123> hello", "BOT123", []string{"coder"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_MixedKnownAndUnknown(t *testing.T) {
	result := extractUnresolvedMentions("@coder @agent-typo hello", "BOT123", []string{"coder", "reviewer"})
	assert.Equal(t, []string{"agent-typo"}, result)
}

func TestExtractUnresolvedMentions_MultipleUnknown(t *testing.T) {
	result := extractUnresolvedMentions("@typo1 @typo2 hello", "BOT123", []string{"coder"})
	assert.Equal(t, []string{"typo1", "typo2"}, result)
}

func TestExtractUnresolvedMentions_NoMentions(t *testing.T) {
	result := extractUnresolvedMentions("just regular text", "BOT123", []string{"coder"})
	assert.Nil(t, result)
}

func TestExtractUnresolvedMentions_AllIsKnown(t *testing.T) {
	result := extractUnresolvedMentions("@all hello", "BOT123", []string{"coder"})
	assert.Nil(t, result)
}

// --- agentFromReply tests ---

func TestAgentFromReply_WebhookMessage(t *testing.T) {
	ref := &discordgo.Message{
		WebhookID: "wh-123",
		Author:    &discordgo.User{ID: "wh-123", Username: "coder"},
	}
	assert.Equal(t, "coder", agentFromReply(ref, "BOT123"))
}

func TestAgentFromReply_BotMessage(t *testing.T) {
	ref := &discordgo.Message{
		Author: &discordgo.User{ID: "BOT123", Username: "ScionBot"},
	}
	assert.Equal(t, "", agentFromReply(ref, "BOT123"))
}

func TestAgentFromReply_NilRef(t *testing.T) {
	assert.Equal(t, "", agentFromReply(nil, "BOT123"))
}

func TestAgentFromReply_NilAuthor(t *testing.T) {
	ref := &discordgo.Message{
		WebhookID: "wh-123",
	}
	assert.Equal(t, "", agentFromReply(ref, "BOT123"))
}

func TestAgentFromReply_RegularUserMessage(t *testing.T) {
	ref := &discordgo.Message{
		Author: &discordgo.User{ID: "USER999", Username: "someone"},
	}
	assert.Equal(t, "", agentFromReply(ref, "BOT123"))
}

func TestAgentFromReply_WebhookWithHyphenatedSlug(t *testing.T) {
	ref := &discordgo.Message{
		WebhookID: "wh-456",
		Author:    &discordgo.User{ID: "wh-456", Username: "my-agent"},
	}
	assert.Equal(t, "my-agent", agentFromReply(ref, "BOT123"))
}

// --- classifyMentions tests ---

// noopResolver is a userResolver that never finds a match.
func noopResolver(_ string) (string, bool) { return "", false }

func TestClassifyMentions_StartMentionsOnly(t *testing.T) {
	result := classifyMentions("@agent-a @agent-b do this", "BOT123",
		[]string{"agent-a", "agent-b"}, noopResolver)

	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
		{Name: "agent-b", Kind: "agent", Identity: "agent:agent-b"},
	}, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
	assert.Equal(t, "do this", result.StrippedBody)
}

func TestClassifyMentions_StartAndBodyMentions(t *testing.T) {
	result := classifyMentions("@agent-a do this cc @agent-b", "BOT123",
		[]string{"agent-a", "agent-b"}, noopResolver)

	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
	}, result.StartMentions)
	assert.Equal(t, []Mention{
		{Name: "agent-b", Kind: "agent", Identity: "agent:agent-b"},
	}, result.BodyMentions)
	assert.Equal(t, "do this cc @agent-b", result.StrippedBody)
}

func TestClassifyMentions_BodyMentionsOnly(t *testing.T) {
	result := classifyMentions("do this @agent-a and @agent-b", "BOT123",
		[]string{"agent-a", "agent-b"}, noopResolver)

	assert.Empty(t, result.StartMentions)
	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
		{Name: "agent-b", Kind: "agent", Identity: "agent:agent-b"},
	}, result.BodyMentions)
	assert.Equal(t, "do this @agent-a and @agent-b", result.StrippedBody)
}

func TestClassifyMentions_SelfDedup(t *testing.T) {
	// @agent-a at start, then @agent-a in body → body should be empty (deduped).
	result := classifyMentions("@agent-a do this and ask @agent-a again", "BOT123",
		[]string{"agent-a"}, noopResolver)

	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
	}, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
}

func TestClassifyMentions_NoMentions(t *testing.T) {
	result := classifyMentions("fix the tests", "BOT123",
		[]string{"agent-a", "agent-b"}, noopResolver)

	assert.Empty(t, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
	assert.Equal(t, "fix the tests", result.StrippedBody)
}

func TestClassifyMentions_AllAtStart(t *testing.T) {
	// @all → return empty ClassifiedMentions (broadcast).
	result := classifyMentions("@all deploy", "BOT123",
		[]string{"agent-a", "agent-b"}, noopResolver)

	assert.Empty(t, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
	assert.Equal(t, "", result.StrippedBody)
}

func TestClassifyMentions_BodyMentionCap(t *testing.T) {
	// 7 body mentions → only first 5 should be captured.
	text := "do this @a1 @a2 @a3 @a4 @a5 @a6 @a7"
	agents := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7"}
	result := classifyMentions(text, "BOT123", agents, noopResolver)

	assert.Empty(t, result.StartMentions)
	assert.Len(t, result.BodyMentions, 5)
	assert.Equal(t, "a1", result.BodyMentions[0].Name)
	assert.Equal(t, "a5", result.BodyMentions[4].Name)
}

func TestClassifyMentions_DiscordBotMentionSkipped(t *testing.T) {
	// <@BOT_ID> should be skipped, @agent-a should be a start mention.
	result := classifyMentions("<@BOT123> @agent-a do this", "BOT123",
		[]string{"agent-a"}, noopResolver)

	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
	}, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
	assert.Equal(t, "do this", result.StrippedBody)
}

func TestClassifyMentions_EmptyText(t *testing.T) {
	result := classifyMentions("", "BOT123", []string{"agent-a"}, noopResolver)
	assert.Empty(t, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
	assert.Equal(t, "", result.StrippedBody)
}

func TestClassifyMentions_UserResolver(t *testing.T) {
	resolver := func(username string) (string, bool) {
		if username == "alice" {
			return "alice@example.com", true
		}
		return "", false
	}
	result := classifyMentions("@agent-a do this cc @alice", "BOT123",
		[]string{"agent-a"}, resolver)

	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
	}, result.StartMentions)
	assert.Equal(t, []Mention{
		{Name: "alice", Kind: "user", Identity: "user:alice@example.com"},
	}, result.BodyMentions)
}

func TestClassifyMentions_UnknownBodyMentionsSkipped(t *testing.T) {
	// Unknown mentions in the body should be skipped.
	result := classifyMentions("do this @stranger", "BOT123",
		[]string{"agent-a"}, noopResolver)

	assert.Empty(t, result.StartMentions)
	assert.Empty(t, result.BodyMentions)
}

func TestClassifyMentions_BodyMentionDedup(t *testing.T) {
	// Duplicate @agent-b in body should be deduplicated.
	result := classifyMentions("@agent-a hey @agent-b check @agent-b", "BOT123",
		[]string{"agent-a", "agent-b"}, noopResolver)

	assert.Equal(t, []Mention{
		{Name: "agent-a", Kind: "agent", Identity: "agent:agent-a"},
	}, result.StartMentions)
	assert.Equal(t, []Mention{
		{Name: "agent-b", Kind: "agent", Identity: "agent:agent-b"},
	}, result.BodyMentions)
}
