package discord

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------------------------------------------------------------------------
// activityColor
// ---------------------------------------------------------------------------

func TestActivityColor_KnownStatuses(t *testing.T) {
	tests := []struct {
		activity string
		want     int
	}{
		{"COMPLETED", colorCompleted},
		{"WAITING_FOR_INPUT", colorInputWait},
		{"ERROR", colorError},
		{"STALLED", colorStalled},
		{"LIMITS_EXCEEDED", colorStalled},
		{"DELETED", colorDeleted},
		{"RUNNING", colorRunning},
	}
	for _, tt := range tests {
		t.Run(tt.activity, func(t *testing.T) {
			assert.Equal(t, tt.want, activityColor(tt.activity))
		})
	}
}

func TestActivityColor_UnknownFallsToDefault(t *testing.T) {
	assert.Equal(t, colorDefault, activityColor("UNKNOWN"))
	assert.Equal(t, colorDefault, activityColor(""))
}

// ---------------------------------------------------------------------------
// RenderStateChangeEmbed
// ---------------------------------------------------------------------------

func TestRenderStateChangeEmbed_NilMessage(t *testing.T) {
	assert.Nil(t, RenderStateChangeEmbed(nil, "coder"))
}

func TestRenderStateChangeEmbed_Basic(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg:       "Deployment finished",
		Timestamp: "2026-06-03T10:00:00Z",
		Metadata: map[string]string{
			"activity":   "COMPLETED",
			"project_id": "my-project",
		},
	}

	embed := RenderStateChangeEmbed(msg, "deploy-agent")
	require.NotNil(t, embed)
	assert.Equal(t, "deploy-agent — COMPLETED", embed.Title)
	assert.Equal(t, "Deployment finished", embed.Description)
	assert.Equal(t, colorCompleted, embed.Color)
	assert.Equal(t, "2026-06-03T10:00:00Z", embed.Timestamp)
	require.NotNil(t, embed.Footer)
	assert.Equal(t, "Project: my-project", embed.Footer.Text)
	assert.Empty(t, embed.Fields)
}

func TestRenderStateChangeEmbed_WithSummary(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "Agent is running",
		Metadata: map[string]string{
			"activity": "RUNNING",
			"summary":  "Processing 42 files",
		},
	}

	embed := RenderStateChangeEmbed(msg, "coder")
	require.NotNil(t, embed)
	assert.Equal(t, colorRunning, embed.Color)
	require.Len(t, embed.Fields, 1)
	assert.Equal(t, "Summary", embed.Fields[0].Name)
	assert.Equal(t, "Processing 42 files", embed.Fields[0].Value)
}

func TestRenderStateChangeEmbed_NoActivity(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "Something happened",
	}

	embed := RenderStateChangeEmbed(msg, "agent")
	require.NotNil(t, embed)
	assert.Equal(t, "agent", embed.Title)
	assert.Equal(t, colorDefault, embed.Color)
}

func TestRenderStateChangeEmbed_NoFooterWithoutProjectID(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg:      "Something happened",
		Metadata: map[string]string{"activity": "ERROR"},
	}

	embed := RenderStateChangeEmbed(msg, "agent")
	require.NotNil(t, embed)
	assert.Nil(t, embed.Footer)
}

func TestRenderStateChangeEmbed_TruncatesLongDescription(t *testing.T) {
	longMsg := strings.Repeat("a", 5000)
	msg := &messages.StructuredMessage{
		Msg:      longMsg,
		Metadata: map[string]string{"activity": "RUNNING"},
	}

	embed := RenderStateChangeEmbed(msg, "coder")
	require.NotNil(t, embed)
	assert.LessOrEqual(t, len(embed.Description), maxEmbedDescriptionLength)
	assert.True(t, strings.HasSuffix(embed.Description, truncationSuffix))
}

func TestRenderStateChangeEmbed_TruncatesLongSummary(t *testing.T) {
	longSummary := strings.Repeat("x", 2000)
	msg := &messages.StructuredMessage{
		Msg: "short",
		Metadata: map[string]string{
			"activity": "COMPLETED",
			"summary":  longSummary,
		},
	}

	embed := RenderStateChangeEmbed(msg, "agent")
	require.NotNil(t, embed)
	require.Len(t, embed.Fields, 1)
	assert.LessOrEqual(t, len(embed.Fields[0].Value), maxEmbedFieldValueLength)
}

// ---------------------------------------------------------------------------
// RenderInputNeeded
// ---------------------------------------------------------------------------

func TestRenderInputNeeded_NilMessage(t *testing.T) {
	embed, components := RenderInputNeeded(nil, "coder", "req-1")
	assert.Nil(t, embed)
	assert.Nil(t, components)
}

func TestRenderInputNeeded_WithChoices(t *testing.T) {
	choices := []string{"Yes", "No", "Maybe"}
	choicesJSON, _ := json.Marshal(choices)

	msg := &messages.StructuredMessage{
		Msg: "Do you approve?",
		Metadata: map[string]string{
			"choices": string(choicesJSON),
		},
	}

	embed, components := RenderInputNeeded(msg, "reviewer", "req-abc")
	require.NotNil(t, embed)
	assert.Equal(t, "Input Needed — reviewer", embed.Title)
	assert.Equal(t, "Do you approve?", embed.Description)
	assert.Equal(t, colorInputWait, embed.Color)

	// 3 choices should fit in 1 action row.
	require.Len(t, components, 1)
	row, ok := components[0].(discordgo.ActionsRow)
	require.True(t, ok)
	assert.Len(t, row.Components, 3)

	// Verify button custom IDs.
	for idx, comp := range row.Components {
		btn, ok := comp.(discordgo.Button)
		require.True(t, ok)
		assert.Equal(t, choices[idx], btn.Label)
		assert.Equal(t, discordgo.PrimaryButton, btn.Style)
		assert.Contains(t, btn.CustomID, "ask:opt:req-abc:")
	}
}

func TestRenderInputNeeded_WithChoicesMultipleRows(t *testing.T) {
	// 7 choices should produce 2 action rows (5 + 2).
	choices := []string{"A", "B", "C", "D", "E", "F", "G"}
	choicesJSON, _ := json.Marshal(choices)

	msg := &messages.StructuredMessage{
		Msg: "Pick one",
		Metadata: map[string]string{
			"choices": string(choicesJSON),
		},
	}

	_, components := RenderInputNeeded(msg, "agent", "req-2")
	require.Len(t, components, 2)

	row1, ok := components[0].(discordgo.ActionsRow)
	require.True(t, ok)
	assert.Len(t, row1.Components, 5)

	row2, ok := components[1].(discordgo.ActionsRow)
	require.True(t, ok)
	assert.Len(t, row2.Components, 2)
}

func TestRenderInputNeeded_WithoutChoices(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "What should I do next?",
	}

	embed, components := RenderInputNeeded(msg, "coder", "req-xyz")
	require.NotNil(t, embed)
	assert.Equal(t, "Input Needed — coder", embed.Title)
	assert.Equal(t, colorInputWait, embed.Color)

	// Default: 1 action row with Reply + Dismiss.
	require.Len(t, components, 1)
	row, ok := components[0].(discordgo.ActionsRow)
	require.True(t, ok)
	require.Len(t, row.Components, 2)

	replyBtn, ok := row.Components[0].(discordgo.Button)
	require.True(t, ok)
	assert.Equal(t, "Reply", replyBtn.Label)
	assert.Equal(t, discordgo.PrimaryButton, replyBtn.Style)
	assert.Equal(t, "ask:reply:req-xyz", replyBtn.CustomID)

	dismissBtn, ok := row.Components[1].(discordgo.Button)
	require.True(t, ok)
	assert.Equal(t, "Dismiss", dismissBtn.Label)
	assert.Equal(t, discordgo.SecondaryButton, dismissBtn.Style)
	assert.Equal(t, "ask:dismiss:req-xyz", dismissBtn.CustomID)
}

func TestRenderInputNeeded_InvalidChoicesJSON(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "Choose something",
		Metadata: map[string]string{
			"choices": "not-valid-json",
		},
	}

	embed, components := RenderInputNeeded(msg, "agent", "req-bad")
	require.NotNil(t, embed)

	// Falls back to Reply + Dismiss.
	require.Len(t, components, 1)
	row, ok := components[0].(discordgo.ActionsRow)
	require.True(t, ok)
	assert.Len(t, row.Components, 2)
}

func TestRenderInputNeeded_EmptyChoicesArray(t *testing.T) {
	choicesJSON, _ := json.Marshal([]string{})
	msg := &messages.StructuredMessage{
		Msg: "Choose",
		Metadata: map[string]string{
			"choices": string(choicesJSON),
		},
	}

	_, components := RenderInputNeeded(msg, "agent", "req-empty")

	// Falls back to Reply + Dismiss.
	require.Len(t, components, 1)
	row, ok := components[0].(discordgo.ActionsRow)
	require.True(t, ok)
	assert.Len(t, row.Components, 2)
}

// ---------------------------------------------------------------------------
// FormatWithEmbed
// ---------------------------------------------------------------------------

func TestFormatWithEmbed_NilMessage(t *testing.T) {
	content, embeds := FormatWithEmbed(nil, "agent")
	assert.Equal(t, "", content)
	assert.Nil(t, embeds)
}

func TestFormatWithEmbed_ShortMessage(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "Hello world",
	}

	content, embeds := FormatWithEmbed(msg, "agent")
	assert.Equal(t, "Hello world", content)
	assert.Nil(t, embeds)
}

func TestFormatWithEmbed_ExactlyAtMessageLimit(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: strings.Repeat("a", maxDiscordMessageLength),
	}

	content, embeds := FormatWithEmbed(msg, "agent")
	assert.Equal(t, msg.Msg, content)
	assert.Nil(t, embeds)
}

func TestFormatWithEmbed_JustOverMessageLimit(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: strings.Repeat("a", maxDiscordMessageLength+1),
	}

	content, embeds := FormatWithEmbed(msg, "agent")
	assert.Equal(t, "", content)
	require.Len(t, embeds, 1)
	assert.Equal(t, msg.Msg, embeds[0].Description)
}

func TestFormatWithEmbed_AtEmbedLimit(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: strings.Repeat("b", maxEmbedDescriptionLength),
	}

	content, embeds := FormatWithEmbed(msg, "agent")
	assert.Equal(t, "", content)
	require.Len(t, embeds, 1)
	assert.Equal(t, msg.Msg, embeds[0].Description)
}

func TestFormatWithEmbed_OverEmbedLimit(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: strings.Repeat("c", maxEmbedDescriptionLength+500),
	}

	content, embeds := FormatWithEmbed(msg, "agent")
	// Content should be the remainder beyond the embed.
	assert.NotEmpty(t, content)
	require.Len(t, embeds, 1)
	// Embed description should be truncated with suffix.
	assert.LessOrEqual(t, len(embeds[0].Description), maxEmbedDescriptionLength)
	assert.True(t, strings.HasSuffix(embeds[0].Description, truncationSuffix))
	// Content + embed description (minus suffix) should cover the full message.
	assert.Greater(t, len(content)+len(embeds[0].Description), maxEmbedDescriptionLength)
}

// ---------------------------------------------------------------------------
// SplitLongMessage
// ---------------------------------------------------------------------------

func TestSplitLongMessage_ShortText(t *testing.T) {
	chunks := SplitLongMessage("hello", 100)
	assert.Equal(t, []string{"hello"}, chunks)
}

func TestSplitLongMessage_ExactFit(t *testing.T) {
	text := strings.Repeat("a", 10)
	chunks := SplitLongMessage(text, 10)
	assert.Equal(t, []string{text}, chunks)
}

func TestSplitLongMessage_SplitAtNewline(t *testing.T) {
	text := "line1\nline2\nline3\nline4"
	chunks := SplitLongMessage(text, 12)
	// "line1\nline2\n" is 12 chars, exactly at the limit.
	require.Len(t, chunks, 2)
	assert.Equal(t, "line1\nline2\n", chunks[0])
	assert.Equal(t, "line3\nline4", chunks[1])
}

func TestSplitLongMessage_NoNewline(t *testing.T) {
	text := strings.Repeat("x", 30)
	chunks := SplitLongMessage(text, 10)
	require.Len(t, chunks, 3)
	for _, chunk := range chunks {
		assert.LessOrEqual(t, len(chunk), 10)
	}
	assert.Equal(t, text, strings.Join(chunks, ""))
}

func TestSplitLongMessage_EmptyText(t *testing.T) {
	chunks := SplitLongMessage("", 100)
	assert.Nil(t, chunks)
}

func TestSplitLongMessage_ZeroMaxLen(t *testing.T) {
	// maxLen <= 0 should default to maxDiscordMessageLength.
	text := strings.Repeat("a", maxDiscordMessageLength+10)
	chunks := SplitLongMessage(text, 0)
	require.Len(t, chunks, 2)
	assert.Equal(t, maxDiscordMessageLength, len(chunks[0]))
}

func TestSplitLongMessage_PreservesContent(t *testing.T) {
	text := "aaaa\nbbbb\ncccc\ndddd\neeee"
	chunks := SplitLongMessage(text, 10)
	reconstructed := strings.Join(chunks, "")
	assert.Equal(t, text, reconstructed)
}

// ---------------------------------------------------------------------------
// Existing format tests
// ---------------------------------------------------------------------------

func TestFormatMessage_NilMessage(t *testing.T) {
	assert.Equal(t, "", FormatMessage(nil, "agent", ""))
}

func TestFormatMessage_BasicMessage(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
		Msg:    "Hello world",
	}
	result := FormatMessage(msg, "coder", "")
	assert.Contains(t, result, "**coder**")
	assert.Contains(t, result, "Hello world")
}

func TestFormatStateChangeText_NilMessage(t *testing.T) {
	assert.Equal(t, "", FormatStateChangeText(nil, "agent"))
}

func TestFormatStateChangeText_WithActivity(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:deploy",
		Status: "running",
		Msg:    "Deploying to staging",
		Metadata: map[string]string{
			"activity": "deploying",
		},
	}
	result := FormatStateChangeText(msg, "deploy")
	assert.Contains(t, result, "[RUNNING]")
	assert.Contains(t, result, "**deploy**")
	assert.Contains(t, result, "deploying")
	assert.Contains(t, result, "Deploying to staging")
}

func TestTruncateForDiscord_NoTruncation(t *testing.T) {
	text := "short text"
	assert.Equal(t, text, truncateForDiscord(text, 100))
}

func TestTruncateForDiscord_Truncates(t *testing.T) {
	text := strings.Repeat("a", 2100)
	result := truncateForDiscord(text, 2000)
	assert.LessOrEqual(t, len(result), 2000)
	assert.True(t, strings.HasSuffix(result, truncationSuffix))
}
