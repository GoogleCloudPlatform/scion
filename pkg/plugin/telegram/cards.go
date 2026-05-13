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
	Slug string
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
			Text:         p.Slug,
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

	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// buildAgentSelectionKeyboard creates an inline keyboard for default agent selection during /setup.
// Callback data format: setup:dflt:<agentSlug>
func buildAgentSelectionKeyboard(agents []string, currentDefault string) *InlineKeyboardMarkup {
	return buildAgentKeyboard(agents, currentDefault, "setup:dflt")
}

// buildDefaultAgentKeyboard creates an inline keyboard for /default command.
// Callback data format: dflt:<agentSlug>
func buildDefaultAgentKeyboard(agents []string, currentDefault string) *InlineKeyboardMarkup {
	return buildAgentKeyboard(agents, currentDefault, "dflt")
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
// If no choices, defaults to [Yes] [No]: ask:yes:<requestID> / ask:no:<requestID>
func buildAskUserKeyboard(requestID string, choices []string) *InlineKeyboardMarkup {
	if len(choices) == 0 {
		return &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{
					{Text: "Yes", CallbackData: truncateCallback(fmt.Sprintf("ask:yes:%s", requestID))},
					{Text: "No", CallbackData: truncateCallback(fmt.Sprintf("ask:no:%s", requestID))},
				},
			},
		}
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

// buildSetupConfirmKeyboard creates a keyboard showing current project link with change/keep options.
// Callback data: setup:change / setup:keep
func buildSetupConfirmKeyboard(currentProject string) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: fmt.Sprintf("Keep (%s)", currentProject), CallbackData: "setup:keep"},
				{Text: "Change project", CallbackData: "setup:change"},
			},
		},
	}
}

// buildSettingsKeyboard creates keyboard for /settings command (agent-to-agent visibility toggle).
// Callback data: settings:a2a:on / settings:a2a:off
func buildSettingsKeyboard(showAgentToAgent bool) *InlineKeyboardMarkup {
	onLabel := "A2A: On"
	offLabel := "A2A: Off"
	if showAgentToAgent {
		onLabel = "✓ A2A: On"
	} else {
		offLabel = "✓ A2A: Off"
	}
	return &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: onLabel, CallbackData: "settings:a2a:on"},
				{Text: offLabel, CallbackData: "settings:a2a:off"},
			},
		},
	}
}

// truncateCallback ensures callback data stays within Telegram's 64-byte limit.
func truncateCallback(data string) string {
	if len(data) <= maxCallbackData {
		return data
	}
	return data[:maxCallbackData]
}
