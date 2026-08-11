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
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatStructuredMessage_Nil(t *testing.T) {
	_, err := formatStructuredMessage(nil)
	assert.Error(t, err)
}

func TestFormatStructuredMessage_PlainTextShort(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:deploy-bot",
		Msg:     "Done.",
		Type:    messages.TypeInstruction,
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	assert.Equal(t, "message", activity.Type)
	assert.Equal(t, "[deploy-bot] Done.", activity.Text)
	assert.Empty(t, activity.Attachments)
}

func TestFormatStructuredMessage_PlainFlag(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:code-reviewer",
		Msg:     strings.Repeat("x", 200), // Long, but Plain=true.
		Type:    messages.TypeInstruction,
		Plain:   true,
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	assert.Contains(t, activity.Text, "[code-reviewer]")
	assert.Empty(t, activity.Attachments)
}

func TestFormatStructuredMessage_AgentResponseCard(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:deploy-bot",
		Msg:     strings.Repeat("This is a detailed response. ", 10),
		Type:    messages.TypeInstruction,
		Metadata: map[string]string{
			"project_id": "my-project",
		},
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	assert.Equal(t, "message", activity.Type)
	assert.Empty(t, activity.Text)
	require.Len(t, activity.Attachments, 1)
	assert.Equal(t, adaptiveCardContentType, activity.Attachments[0].ContentType)

	// Parse and verify card structure.
	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	assert.Equal(t, "AdaptiveCard", card["type"])
	assert.Equal(t, "1.5", card["version"])

	body, ok := card["body"].([]interface{})
	require.True(t, ok)
	require.GreaterOrEqual(t, len(body), 2) // ColumnSet header + TextBlock body

	// Verify header ColumnSet.
	headerRaw := body[0].(map[string]interface{})
	assert.Equal(t, "ColumnSet", headerRaw["type"])
	columns := headerRaw["columns"].([]interface{})
	require.GreaterOrEqual(t, len(columns), 1)

	// First column: agent name.
	col0 := columns[0].(map[string]interface{})
	col0Items := col0["items"].([]interface{})
	require.Len(t, col0Items, 1)
	nameBlock := col0Items[0].(map[string]interface{})
	assert.Equal(t, "deploy-bot", nameBlock["text"])
	assert.Equal(t, "Bolder", nameBlock["weight"])
	assert.Equal(t, "Accent", nameBlock["color"])

	// Second column: project slug.
	if len(columns) > 1 {
		col1 := columns[1].(map[string]interface{})
		col1Items := col1["items"].([]interface{})
		require.Len(t, col1Items, 1)
		projBlock := col1Items[0].(map[string]interface{})
		assert.Equal(t, "my-project", projBlock["text"])
		assert.Equal(t, true, projBlock["isSubtle"])
	}

	// Verify body TextBlock.
	bodyBlock := body[1].(map[string]interface{})
	assert.Equal(t, "TextBlock", bodyBlock["type"])
	assert.Contains(t, bodyBlock["text"], "detailed response")
	assert.Equal(t, true, bodyBlock["wrap"])
}

func TestFormatStructuredMessage_AskUserCard(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:deploy-bot",
		Msg:     "Should I proceed with the deployment?",
		Type:    messages.TypeInputNeeded,
		Metadata: map[string]string{
			"request_id": "req-123",
			"choices":    `["Approve","Reject"]`,
		},
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	require.Len(t, activity.Attachments, 1)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	// Verify header text.
	body := card["body"].([]interface{})
	require.GreaterOrEqual(t, len(body), 2)
	headerBlock := body[0].(map[string]interface{})
	assert.Contains(t, headerBlock["text"], "deploy-bot needs your input")
	assert.Equal(t, "Bolder", headerBlock["weight"])

	// Verify question text.
	questionBlock := body[1].(map[string]interface{})
	assert.Contains(t, questionBlock["text"], "deployment")

	// Verify actions: 2 choices + 1 custom reply = 3 actions.
	actions := card["actions"].([]interface{})
	require.Len(t, actions, 3)

	// Approve button.
	approveAction := actions[0].(map[string]interface{})
	assert.Equal(t, "Action.Submit", approveAction["type"])
	assert.Equal(t, "Approve", approveAction["title"])
	assert.Equal(t, "positive", approveAction["style"])
	approveData := approveAction["data"].(map[string]interface{})
	assert.Equal(t, "ask_response", approveData["action"])
	assert.Equal(t, "req-123", approveData["request_id"])
	assert.Equal(t, "Approve", approveData["choice"])

	// Reject button.
	rejectAction := actions[1].(map[string]interface{})
	assert.Equal(t, "Reject", rejectAction["title"])
	assert.Equal(t, "destructive", rejectAction["style"])

	// Custom Reply button.
	customAction := actions[2].(map[string]interface{})
	assert.Equal(t, "Custom Reply...", customAction["title"])
	customData := customAction["data"].(map[string]interface{})
	assert.Equal(t, "ask_input", customData["action"])
	assert.Equal(t, "req-123", customData["request_id"])
}

func TestFormatStructuredMessage_AskUserDefaultButtons(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:reviewer",
		Msg:     "Do you approve?",
		Type:    messages.TypeInputNeeded,
		Metadata: map[string]string{
			"request_id": "req-456",
		},
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	// Default buttons: Approve, Reject, Custom Reply.
	actions := card["actions"].([]interface{})
	require.Len(t, actions, 3)
	assert.Equal(t, "Approve", actions[0].(map[string]interface{})["title"])
	assert.Equal(t, "Reject", actions[1].(map[string]interface{})["title"])
	assert.Equal(t, "Custom Reply...", actions[2].(map[string]interface{})["title"])
}

func TestFormatStructuredMessage_StatusCard(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:builder",
		Msg:     "Build completed successfully",
		Type:    messages.TypeStateChange,
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	require.Len(t, activity.Attachments, 1)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	body := card["body"].([]interface{})
	require.Len(t, body, 1)

	// Status card has a ColumnSet with icon + text.
	columnSet := body[0].(map[string]interface{})
	assert.Equal(t, "ColumnSet", columnSet["type"])
	columns := columnSet["columns"].([]interface{})
	require.Len(t, columns, 2)

	// Status text column.
	textCol := columns[1].(map[string]interface{})
	textItems := textCol["items"].([]interface{})
	require.Len(t, textItems, 1)
	statusBlock := textItems[0].(map[string]interface{})
	assert.Contains(t, statusBlock["text"], "builder")
	assert.Contains(t, statusBlock["text"], "Build completed")
	assert.Equal(t, true, statusBlock["isSubtle"])
}

func TestIsPlainTextMessage(t *testing.T) {
	tests := []struct {
		name     string
		msg      *messages.StructuredMessage
		expected bool
	}{
		{
			name: "plain flag set",
			msg: &messages.StructuredMessage{
				Msg:   strings.Repeat("x", 200),
				Plain: true,
				Type:  messages.TypeInstruction,
			},
			expected: true,
		},
		{
			name: "short instruction message",
			msg: &messages.StructuredMessage{
				Msg:  "OK, done.",
				Type: messages.TypeInstruction,
			},
			expected: true,
		},
		{
			name: "long instruction message",
			msg: &messages.StructuredMessage{
				Msg:  strings.Repeat("This is a long message. ", 10),
				Type: messages.TypeInstruction,
			},
			expected: false,
		},
		{
			name: "input-needed even if short",
			msg: &messages.StructuredMessage{
				Msg:  "Approve?",
				Type: messages.TypeInputNeeded,
			},
			expected: false,
		},
		{
			name: "empty message",
			msg: &messages.StructuredMessage{
				Msg:  "",
				Type: messages.TypeInstruction,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isPlainTextMessage(tt.msg))
		})
	}
}

func TestDeriveAgentSlug(t *testing.T) {
	assert.Equal(t, "deploy-bot", deriveAgentSlug("agent:deploy-bot"))
	assert.Equal(t, "user-name", deriveAgentSlug("user-name"))
	assert.Equal(t, "", deriveAgentSlug(""))
}

func TestFormatStructuredMessage_EmptyMessage(t *testing.T) {
	// N1: Verify no trailing space when msg.Msg is empty.
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:bot",
		Msg:     "",
		Type:    messages.TypeInstruction,
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	// Empty short message -> plain text without trailing space.
	assert.Equal(t, "[bot]", activity.Text)
}

func TestFormatStructuredMessage_SpecialCharacters(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:test-bot",
		Msg:     `Special chars: <script>alert("xss")</script> & "quotes" < > &amp;`,
		Type:    messages.TypeInstruction,
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	// Short message -> plain text with special chars preserved.
	assert.Contains(t, activity.Text, `<script>`)
}

func TestFormatStructuredMessage_LongMessage(t *testing.T) {
	longBody := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50)
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:verbose-bot",
		Msg:     longBody,
		Type:    messages.TypeInstruction,
		Metadata: map[string]string{
			"project_id": "test-project",
		},
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	require.Len(t, activity.Attachments, 1)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)
	assert.Equal(t, "AdaptiveCard", card["type"])

	// Body should contain the full message text.
	body := card["body"].([]interface{})
	require.GreaterOrEqual(t, len(body), 2)
	textBlock := body[1].(map[string]interface{})
	assert.Equal(t, longBody, textBlock["text"])
}

func TestFormatStructuredMessage_AgentAttribution(t *testing.T) {
	// Ensure agent name is always visible in card header.
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:code-reviewer",
		Msg:     strings.Repeat("Some longer message for card rendering. ", 5),
		Type:    messages.TypeInstruction,
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	require.Len(t, activity.Attachments, 1)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	body := card["body"].([]interface{})
	// First element is the ColumnSet header.
	header := body[0].(map[string]interface{})
	columns := header["columns"].([]interface{})
	col0 := columns[0].(map[string]interface{})
	items := col0["items"].([]interface{})
	nameBlock := items[0].(map[string]interface{})
	assert.Equal(t, "code-reviewer", nameBlock["text"])
}

func TestBuildAgentResponseCard_NoProject(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "Hello world",
	}

	card := buildAgentResponseCard(msg, "test-agent", "")
	assert.Equal(t, "AdaptiveCard", card.Type)
	assert.Equal(t, "1.5", card.Version)

	// With no project slug, should only have 1 column.
	require.GreaterOrEqual(t, len(card.Body), 1)
	header, ok := card.Body[0].(ColumnSet)
	require.True(t, ok)
	assert.Len(t, header.Columns, 1)
}

func TestFormatStructuredMessage_Truncation(t *testing.T) {
	// R1: Message bodies exceeding maxCardTextLength are truncated.
	longBody := strings.Repeat("A", 5000)
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:verbose",
		Msg:     longBody,
		Type:    messages.TypeInstruction,
		Metadata: map[string]string{
			"project_id": "test",
		},
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	require.Len(t, activity.Attachments, 1)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	body := card["body"].([]interface{})
	// Body should have at least ColumnSet header + TextBlock.
	require.GreaterOrEqual(t, len(body), 2)

	textBlock := body[1].(map[string]interface{})
	text := textBlock["text"].(string)

	// Verify truncation: must be <= maxCardTextLength and end with suffix.
	assert.LessOrEqual(t, len(text), maxCardTextLength)
	assert.True(t, strings.HasSuffix(text, truncationSuffix),
		"truncated text should end with %q", truncationSuffix)
}

func TestFormatStructuredMessage_NoTruncationUnderLimit(t *testing.T) {
	// Messages under maxCardTextLength should not be truncated.
	// Use 200 chars to exceed plainTextThreshold (100) and render as card.
	cardBody := strings.Repeat("B", 200)
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:concise",
		Msg:     cardBody,
		Type:    messages.TypeInstruction,
		Metadata: map[string]string{
			"project_id": "test",
		},
	}

	activity, err := formatStructuredMessage(msg)
	require.NoError(t, err)
	require.Len(t, activity.Attachments, 1)

	var card map[string]interface{}
	err = json.Unmarshal(activity.Attachments[0].Content, &card)
	require.NoError(t, err)

	body := card["body"].([]interface{})
	require.GreaterOrEqual(t, len(body), 2)
	textBlock := body[1].(map[string]interface{})
	text := textBlock["text"].(string)

	// Body is under maxCardTextLength, so no truncation suffix.
	assert.Equal(t, cardBody, text)
	assert.False(t, strings.HasSuffix(text, truncationSuffix))
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"under limit", "short", 100, "short"},
		{"at limit", strings.Repeat("x", 100), 100, strings.Repeat("x", 100)},
		{"over limit", strings.Repeat("x", 200), 100,
			strings.Repeat("x", 100-len(truncationSuffix)) + truncationSuffix},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateText(tt.input, tt.maxLen)
			assert.Equal(t, tt.want, got)
			assert.LessOrEqual(t, len(got), tt.maxLen)
		})
	}
}

func TestBuildAskUserCard_MultipleChoices(t *testing.T) {
	msg := &messages.StructuredMessage{
		Msg: "Pick one",
		Metadata: map[string]string{
			"request_id": "req-789",
			"choices":    `["Yes","No","Maybe"]`,
		},
	}

	card := buildAskUserCard(msg, "chooser")

	// 3 choices + 1 custom reply = 4 actions.
	require.Len(t, card.Actions, 4)

	yesAction, ok := card.Actions[0].(ActionSubmit)
	require.True(t, ok)
	assert.Equal(t, "Yes", yesAction.Title)
	assert.Equal(t, "positive", yesAction.Style)

	noAction, ok := card.Actions[1].(ActionSubmit)
	require.True(t, ok)
	assert.Equal(t, "No", noAction.Title)
	assert.Equal(t, "destructive", noAction.Style)

	maybeAction, ok := card.Actions[2].(ActionSubmit)
	require.True(t, ok)
	assert.Equal(t, "Maybe", maybeAction.Title)
	assert.Equal(t, "", maybeAction.Style) // No special style.
}
