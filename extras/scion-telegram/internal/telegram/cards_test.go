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
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProjectSelectionKeyboard_SingleProject(t *testing.T) {
	kb := buildProjectSelectionKeyboard([]ProjectOption{
		{ID: "proj1", Slug: "my-project"},
	})
	require.Len(t, kb.InlineKeyboard, 2)
	assert.Len(t, kb.InlineKeyboard[0], 1)
	assert.Equal(t, "my-project", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "setup:proj:proj1", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "Cancel", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "setup:cancel", kb.InlineKeyboard[1][0].CallbackData)
}

func TestBuildProjectSelectionKeyboard_ThreeProjects(t *testing.T) {
	kb := buildProjectSelectionKeyboard([]ProjectOption{
		{ID: "p1", Slug: "alpha"},
		{ID: "p2", Slug: "beta"},
		{ID: "p3", Slug: "gamma"},
	})
	require.Len(t, kb.InlineKeyboard, 3)
	assert.Len(t, kb.InlineKeyboard[0], 2)
	assert.Len(t, kb.InlineKeyboard[1], 1)
	assert.Equal(t, "alpha", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "beta", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "gamma", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "Cancel", kb.InlineKeyboard[2][0].Text)
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
	require.Len(t, kb.InlineKeyboard, 6)
	for _, row := range kb.InlineKeyboard[:5] {
		assert.LessOrEqual(t, len(row), 2)
	}
	assert.Equal(t, "Cancel", kb.InlineKeyboard[5][0].Text)
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
	store := newTestStore(t)
	kb := buildDefaultAgentKeyboard(context.Background(), store, []string{"coder", "reviewer"}, "coder", 0)
	assert.Equal(t, "dflt:coder", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "✓ coder (current)", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "dflt:reviewer", kb.InlineKeyboard[0][1].CallbackData)
	assert.Equal(t, "reviewer", kb.InlineKeyboard[0][1].Text)
}

func TestBuildDefaultAgentKeyboard_TopicScoped(t *testing.T) {
	store := newTestStore(t)
	kb := buildDefaultAgentKeyboard(context.Background(), store, []string{"coder", "reviewer"}, "coder", 42)
	assert.Equal(t, "dflt:coder:42", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "✓ coder (current)", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "dflt:reviewer:42", kb.InlineKeyboard[0][1].CallbackData)

	lastRow := kb.InlineKeyboard[len(kb.InlineKeyboard)-1]
	assert.Equal(t, "dflt:__none__:42", lastRow[0].CallbackData)
	assert.Contains(t, lastRow[0].Text, "use chat default")
}

func TestBuildDefaultAgentKeyboard_LongSlugUsesLookup(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	longSlug := "this-is-a-very-long-agent-slug-that-will-exceed-sixty-four-bytes"
	kb := buildDefaultAgentKeyboard(ctx, store, []string{longSlug}, "", 99999999)

	cbData := kb.InlineKeyboard[0][0].CallbackData
	assert.True(t, strings.HasPrefix(cbData, callbackLookupPrefix),
		"expected callback lookup prefix, got %q", cbData)
	assert.LessOrEqual(t, len(cbData), maxCallbackData)

	shortID := strings.TrimPrefix(cbData, callbackLookupPrefix)
	lookup, err := store.GetCallbackLookup(ctx, shortID)
	require.NoError(t, err)
	require.NotNil(t, lookup)
	assert.Equal(t, fmt.Sprintf("dflt:%s:99999999", longSlug), lookup.FullData)
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
	assert.Nil(t, kb)
}

func TestBuildAskUserKeyboard_EmptyChoices(t *testing.T) {
	kb := buildAskUserKeyboard("req-1", []string{})
	assert.Nil(t, kb)
}

func TestBuildSetupConfirmKeyboard(t *testing.T) {
	kb := buildSetupConfirmKeyboard("my-project")
	require.Len(t, kb.InlineKeyboard, 2)
	assert.Len(t, kb.InlineKeyboard[0], 2)
	assert.Equal(t, "Keep (my-project)", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "setup:keep", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "Change project", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "setup:change", kb.InlineKeyboard[0][1].CallbackData)
	assert.Len(t, kb.InlineKeyboard[1], 1)
	assert.Equal(t, "Unlink this group", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "setup:unlink", kb.InlineKeyboard[1][0].CallbackData)
}

func TestBuildSettingsKeyboard_AgentToAgentOn(t *testing.T) {
	kb := buildSettingsKeyboard(true, false, true)
	require.Len(t, kb.InlineKeyboard, 3)
	assert.Equal(t, "✓ Observer: On", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "settings:a2a:on", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "Observer: Off", kb.InlineKeyboard[0][1].Text)
	assert.Equal(t, "settings:a2a:off", kb.InlineKeyboard[0][1].CallbackData)
}

func TestBuildSettingsKeyboard_AgentToAgentOff(t *testing.T) {
	kb := buildSettingsKeyboard(false, false, true)
	require.Len(t, kb.InlineKeyboard, 3)
	assert.Equal(t, "Observer: On", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "✓ Observer: Off", kb.InlineKeyboard[0][1].Text)
}

func TestBuildSettingsKeyboard_CommentaryOn(t *testing.T) {
	kb := buildSettingsKeyboard(false, false, true)
	require.Len(t, kb.InlineKeyboard, 3)
	assert.Equal(t, "✓ Commentary: On", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "settings:commentary:on", kb.InlineKeyboard[1][0].CallbackData)
	assert.Equal(t, "Commentary: Off", kb.InlineKeyboard[1][1].Text)
	assert.Equal(t, "settings:commentary:off", kb.InlineKeyboard[1][1].CallbackData)
}

func TestBuildSettingsKeyboard_CommentaryOff(t *testing.T) {
	kb := buildSettingsKeyboard(false, false, false)
	require.Len(t, kb.InlineKeyboard, 3)
	assert.Equal(t, "Commentary: On", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "✓ Commentary: Off", kb.InlineKeyboard[1][1].Text)
}

func TestBuildSettingsKeyboard_GroupNotifyOn(t *testing.T) {
	kb := buildSettingsKeyboard(false, true, true)
	require.Len(t, kb.InlineKeyboard, 3)
	assert.Equal(t, "✓ Group Notifications: On", kb.InlineKeyboard[2][0].Text)
	assert.Equal(t, "settings:grp:on", kb.InlineKeyboard[2][0].CallbackData)
	assert.Equal(t, "Group Notifications: Off", kb.InlineKeyboard[2][1].Text)
	assert.Equal(t, "settings:grp:off", kb.InlineKeyboard[2][1].CallbackData)
}

func TestBuildNotificationsKeyboard(t *testing.T) {
	agents := []notificationAgentEntry{
		{ProjectSlug: "proj-a", ProjectID: "id-a", AgentSlug: "coder", Enabled: true},
		{ProjectSlug: "proj-a", ProjectID: "id-a", AgentSlug: "reviewer", Enabled: false},
	}
	kb := buildNotificationsKeyboard(agents)
	require.Len(t, kb.InlineKeyboard, 2)
	assert.Equal(t, "🔔 proj-a/coder", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "notify:id-a:coder", kb.InlineKeyboard[0][0].CallbackData)
	assert.Equal(t, "🔕 proj-a/reviewer", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "notify:id-a:reviewer", kb.InlineKeyboard[1][0].CallbackData)
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
	require.Len(t, kb.InlineKeyboard, 1)
	assert.Equal(t, "Cancel", kb.InlineKeyboard[0][0].Text)
}

func TestBuildAgentSelectionKeyboard_SingleAgent(t *testing.T) {
	kb := buildAgentSelectionKeyboard([]string{"coder"}, "coder")
	require.Len(t, kb.InlineKeyboard, 2)
	assert.Len(t, kb.InlineKeyboard[0], 1)
	assert.Equal(t, "✓ coder (current)", kb.InlineKeyboard[0][0].Text)
	assert.Equal(t, "No default agent", kb.InlineKeyboard[1][0].Text)
	assert.Equal(t, "setup:dflt:", kb.InlineKeyboard[1][0].CallbackData)
}
