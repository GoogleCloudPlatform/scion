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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProjectSelectionKeyboard_SingleProject(t *testing.T) {
	kb := buildProjectSelectionKeyboard([]ProjectOption{
		{ID: "proj1", Slug: "my-project"},
	})
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Len(t, kb.InlineKeyboard[0], 1)
	assert.Equal(t, "my-project", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "setup:proj:proj1", kb.InlineKeyboard[0][0].CallbackData)
}

func TestBuildProjectSelectionKeyboard_ThreeProjects(t *testing.T) {
	kb := buildProjectSelectionKeyboard([]ProjectOption{
		{ID: "p1", Slug: "alpha"},
		{ID: "p2", Slug: "beta"},
		{ID: "p3", Slug: "gamma"},
	})
	require.Len(t, kb.InlineKeyboard, 2)
	assert.Len(t, kb.InlineKeyboard[0], 2)
	assert.Len(t, kb.InlineKeyboard[1], 1)
	assert.Equal(t, "alpha", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "beta", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "gamma", kb.InlineKeyboard[1][0].Text)
}

func TestBuildProjectSelectionKeyboard_TenProjects(t *testing.T) {
	var projects []ProjectOption
	for i := 0; i < 10; i++ {
		projects = append(projects, ProjectOption{
			ID:   fmt.Sprintf("p%d", i),
			Slug: fmt.Sprintf("project-%d", i),
		})
	}
	kb := buildProjectSelectionKeyboard(projects)
	require.Len(t, kb.InlineKeyboard, 5)
	for _, row := range kb.InlineKeyboard {
		assert.LessOrEqual(t, len(row), 2)
	}
}

func TestBuildProjectSelectionKeyboard_CallbackDataFormat(t *testing.T) {
	kb := buildProjectSelectionKeyboard([]ProjectOption{
		{ID: "abc123", Slug: "test"},
	})
	assert.Equal(t, "setup:proj:abc123", kb.InlineKeyboard[0][0].CallbackData)
}

func TestBuildAgentSelectionKeyboard_MarksDefault(t *testing.T) {
	kb := buildAgentSelectionKeyboard([]string{"coder", "reviewer", "tester"}, "reviewer")
	found := false
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if btn.CallbackData == "setup:dflt:reviewer" {
				assert.Equal(t, "✓ reviewer (current)", btn.Text)
				found = true
			}
		}
	}
	assert.True(t, found, "should mark current default")
}

func TestBuildAgentSelectionKeyboard_NoDefault(t *testing.T) {
	kb := buildAgentSelectionKeyboard([]string{"coder", "reviewer"}, "")
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			assert.NotContains(t, btn.Text, "✓")
		}
	}
}

func TestBuildDefaultAgentKeyboard_CallbackFormat(t *testing.T) {
	kb := buildDefaultAgentKeyboard([]string{"coder", "reviewer"}, "coder")
	assert.Equal(t, "dflt:coder", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "✓ coder (current)", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "dflt:reviewer", kb.InlineKeyboard[0][1].CallbackData)
	assert.Equal(t, "reviewer", kb.InlineKeyboard[0][1].Text)
}

func TestBuildAskUserKeyboard_WithChoices(t *testing.T) {
	kb := buildAskUserKeyboard("req-42", []string{"Option A", "Option B", "Option C"})
	require.Len(t, kb.InlineKeyboard, 2)
	assert.Equal(t, "Option A", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "ask:opt:req-42:0", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "Option B", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "ask:opt:req-42:1", kb.InlineKeyboard[0][1].CallbackData)
	assert.Equal(t, "Option C", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "ask:opt:req-42:2", kb.InlineKeyboard[1][0].CallbackData)
}

func TestBuildAskUserKeyboard_NoChoices(t *testing.T) {
	kb := buildAskUserKeyboard("req-99", nil)
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Len(t, kb.InlineKeyboard[0], 2)
	assert.Equal(t, "Yes", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "ask:yes:req-99", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "No", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "ask:no:req-99", kb.InlineKeyboard[0][1].CallbackData)
}

func TestBuildAskUserKeyboard_EmptyChoices(t *testing.T) {
	kb := buildAskUserKeyboard("req-1", []string{})
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Equal(t, "Yes", kb.InlineKeyboard[0][0].Text)
}

func TestBuildSetupConfirmKeyboard(t *testing.T) {
	kb := buildSetupConfirmKeyboard("my-project")
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Len(t, kb.InlineKeyboard[0], 2)
	assert.Equal(t, "Keep (my-project)", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "setup:keep", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "Change project", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "setup:change", kb.InlineKeyboard[0][1].CallbackData)
}

func TestBuildSettingsKeyboard_AgentToAgentOn(t *testing.T) {
	kb := buildSettingsKeyboard(true)
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Equal(t, "✓ A2A: On", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "settings:a2a:on", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "A2A: Off", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "settings:a2a:off", kb.InlineKeyboard[0][1].CallbackData)
}

func TestBuildSettingsKeyboard_AgentToAgentOff(t *testing.T) {
	kb := buildSettingsKeyboard(false)
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Equal(t, "A2A: On", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "✓ A2A: Off", kb.InlineKeyboard[0][1].Text)
}

func TestCallbackData_Under64Bytes(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"project", fmt.Sprintf("setup:proj:%s", "a-fairly-long-project-id-here")},
		{"agent setup", "setup:dflt:my-long-agent-name"},
		{"agent default", "dflt:my-long-agent-name"},
		{"ask yes", "ask:yes:request-id-12345"},
		{"ask no", "ask:no:request-id-12345"},
		{"ask opt", "ask:opt:request-id-12345:99"},
		{"setup keep", "setup:keep"},
		{"setup change", "setup:change"},
		{"settings on", "settings:a2a:on"},
		{"settings off", "settings:a2a:off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := truncateCallback(tc.data)
			assert.LessOrEqual(t, len(result), maxCallbackData,
				"callback data %q exceeds 64 bytes", result)
		})
	}
}

func TestTruncateCallback_LongData(t *testing.T) {
	long := "setup:proj:" + string(make([]byte, 100))
	result := truncateCallback(long)
	assert.Len(t, result, maxCallbackData)
}

func TestBuildProjectSelectionKeyboard_Empty(t *testing.T) {
	kb := buildProjectSelectionKeyboard(nil)
	assert.Empty(t, kb.InlineKeyboard)
}

func TestBuildAgentSelectionKeyboard_SingleAgent(t *testing.T) {
	kb := buildAgentSelectionKeyboard([]string{"coder"}, "coder")
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Len(t, kb.InlineKeyboard[0], 1)
	assert.Equal(t, "✓ coder (current)", kb.InlineKeyboard[0][0].Text)
}
