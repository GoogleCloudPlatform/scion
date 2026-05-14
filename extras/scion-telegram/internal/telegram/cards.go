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

import "fmt"

// ProjectOption represents a project choice for keyboard selection.
type ProjectOption struct {
	ID   string
	Name string
	Slug string
}

// DisplayName returns a human-readable label for the project.
func (p ProjectOption) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	if p.Slug != "" {
		return p.Slug
	}
	return p.ID
}

// maxCallbackData is the Telegram limit for callback_data (64 bytes).
const maxCallbackData = 64

// buildProjectSelectionKeyboard creates an inline keyboard for /setup project selection.
// Callback data format: setup:proj:<projectID>
func buildProjectSelectionKeyboard(projects []ProjectOption) *InlineKeyboardMarkup {
	var rows [][]InlineKeyboardButton
	var row []InlineKeyboardButton

	for _, p := range projects {
		btn := InlineKeyboardButton{
			Text:         p.DisplayName(),
			CallbackData: truncateCallback(fmt.Sprintf("setup:proj:%s", p.ID)),
		}
		row = append(row, btn)
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}

	rows = append(rows, []InlineKeyboardButton{
		{Text: "Cancel", CallbackData: "setup:cancel"},
	})

	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// buildAgentSelectionKeyboard creates an inline keyboard for default agent selection during /setup.
// Callback data format: setup:dflt:<agentSlug>
func buildAgentSelectionKeyboard(agents []string, currentDefault string) *InlineKeyboardMarkup {
	kb := buildAgentKeyboard(agents, currentDefault, "setup:dflt")
	kb.InlineKeyboard = append(kb.InlineKeyboard, []InlineKeyboardButton{
		{Text: "No default agent", CallbackData: "setup:dflt:"},
	})
	return kb
}

// buildDefaultAgentKeyboard creates an inline keyboard for /default command.
// Callback data format: dflt:<agentSlug>
func buildDefaultAgentKeyboard(agents []string, currentDefault string) *InlineKeyboardMarkup {
	kb := buildAgentKeyboard(agents, currentDefault, "dflt")
	noneLabel := "No default agent"
	if currentDefault == "" {
		noneLabel = "✓ No default agent (current)"
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, []InlineKeyboardButton{
		{Text: noneLabel, CallbackData: "dflt:__none__"},
	})
	return kb
}

func buildAgentKeyboard(agents []string, currentDefault string, prefix string) *InlineKeyboardMarkup {
	var rows [][]InlineKeyboardButton
	var row []InlineKeyboardButton

	for _, agent := range agents {
		label := agent
		if agent == currentDefault {
			label = "✓ " + agent + " (current)"
		}
		btn := InlineKeyboardButton{
			Text:         label,
			CallbackData: truncateCallback(fmt.Sprintf("%s:%s", prefix, agent)),
		}
		row = append(row, btn)
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}

	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// buildAskUserKeyboard creates an inline keyboard for InputNeeded messages.
// If choices are provided, each gets a button: ask:opt:<requestID>:<index>
// If no choices, returns nil so the user can type a free-form reply.
func buildAskUserKeyboard(requestID string, choices []string) *InlineKeyboardMarkup {
	if len(choices) == 0 {
		return nil
	}

	var rows [][]InlineKeyboardButton
	var row []InlineKeyboardButton
	for i, choice := range choices {
		btn := InlineKeyboardButton{
			Text:         choice,
			CallbackData: truncateCallback(fmt.Sprintf("ask:opt:%s:%d", requestID, i)),
		}
		row = append(row, btn)
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}

	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// buildSetupConfirmKeyboard creates a keyboard showing current project link with change/keep/unlink options.
// Callback data: setup:change / setup:keep / setup:unlink
func buildSetupConfirmKeyboard(currentProject string) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: fmt.Sprintf("Keep (%s)", currentProject), CallbackData: "setup:keep"},
				{Text: "Change project", CallbackData: "setup:change"},
			},
			{
				{Text: "Unlink this group", CallbackData: "setup:unlink"},
			},
		},
	}
}

// buildSettingsKeyboard creates keyboard for /settings command.
// Includes observer mode toggle and group notifications toggle.
func buildSettingsKeyboard(showAgentToAgent, notifyInGroup, showAssistantReply bool) *InlineKeyboardMarkup {
	a2aOnLabel := "Observer: On"
	a2aOffLabel := "Observer: Off"
	if showAgentToAgent {
		a2aOnLabel = "✓ Observer: On"
	} else {
		a2aOffLabel = "✓ Observer: Off"
	}

	grpOnLabel := "Group Notifications: On"
	grpOffLabel := "Group Notifications: Off"
	if notifyInGroup {
		grpOnLabel = "✓ Group Notifications: On"
	} else {
		grpOffLabel = "✓ Group Notifications: Off"
	}

	comOnLabel := "Commentary: On"
	comOffLabel := "Commentary: Off"
	if showAssistantReply {
		comOnLabel = "✓ Commentary: On"
	} else {
		comOffLabel = "✓ Commentary: Off"
	}

	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: a2aOnLabel, CallbackData: "settings:a2a:on"},
				{Text: a2aOffLabel, CallbackData: "settings:a2a:off"},
			},
			{
				{Text: comOnLabel, CallbackData: "settings:commentary:on"},
				{Text: comOffLabel, CallbackData: "settings:commentary:off"},
			},
			{
				{Text: grpOnLabel, CallbackData: "settings:grp:on"},
				{Text: grpOffLabel, CallbackData: "settings:grp:off"},
			},
		},
	}
}

// notificationAgentEntry pairs an agent with its notification-enabled state for keyboard building.
type notificationAgentEntry struct {
	ProjectSlug string
	ProjectID   string
	AgentSlug   string
	Enabled     bool
}

// buildNotificationsKeyboard creates an inline keyboard for per-agent notification toggles.
// Callback data: notify:<projectID>:<agentSlug>
func buildNotificationsKeyboard(agents []notificationAgentEntry) *InlineKeyboardMarkup {
	var rows [][]InlineKeyboardButton
	for _, a := range agents {
		label := a.AgentSlug
		if a.ProjectSlug != "" {
			label = a.ProjectSlug + "/" + a.AgentSlug
		}
		if a.Enabled {
			label = "🔔 " + label
		} else {
			label = "🔕 " + label
		}
		btn := InlineKeyboardButton{
			Text:         label,
			CallbackData: truncateCallback(fmt.Sprintf("notify:%s:%s", a.ProjectID, a.AgentSlug)),
		}
		rows = append(rows, []InlineKeyboardButton{btn})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// truncateCallback ensures callback data stays within Telegram's 64-byte limit.
func truncateCallback(data string) string {
	if len(data) <= maxCallbackData {
		return data
	}
	return data[:maxCallbackData]
}
