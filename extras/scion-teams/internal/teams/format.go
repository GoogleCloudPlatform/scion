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
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	// plainTextThreshold is the character length below which messages
	// without special formatting are sent as plain text instead of cards.
	plainTextThreshold = 100

	// maxCardTextLength is the maximum character length for message bodies
	// included in Adaptive Card templates. Longer bodies are truncated
	// with a "... [truncated]" suffix.
	maxCardTextLength = 4096

	// truncationSuffix is appended to truncated message bodies.
	truncationSuffix = "... [truncated]"

	// adaptiveCardContentType is the MIME type for Adaptive Card attachments.
	adaptiveCardContentType = "application/vnd.microsoft.card.adaptive"
)

// formatStructuredMessage converts a StructuredMessage into a Teams Activity
// ready to send via the Bot Connector REST API. It returns an Activity with
// either an Adaptive Card attachment or plain text, depending on the message.
func formatStructuredMessage(msg *messages.StructuredMessage) (*Activity, error) {
	if msg == nil {
		return nil, fmt.Errorf("message is nil")
	}

	activity := &Activity{
		Type: "message",
	}

	// Determine agent slug for display.
	agentSlug := deriveAgentSlug(msg.Sender)
	projectSlug := ""
	if msg.Metadata != nil {
		if ps, ok := msg.Metadata["project_id"]; ok {
			projectSlug = ps
		}
	}

	// Plain text fallback: short messages or msg.Plain = true.
	if isPlainTextMessage(msg) {
		text := msg.Msg
		if agentSlug != "" && text != "" {
			text = fmt.Sprintf("[%s] %s", agentSlug, text)
		} else if agentSlug != "" {
			text = fmt.Sprintf("[%s]", agentSlug)
		}
		activity.Text = text
		return activity, nil
	}

	// Truncate long message bodies before card rendering.
	msg.Msg = truncateText(msg.Msg, maxCardTextLength)

	// Build the appropriate Adaptive Card.
	var card *AdaptiveCard
	switch msg.Type {
	case messages.TypeInputNeeded:
		card = buildAskUserCard(msg, agentSlug)
	case messages.TypeStateChange:
		card = buildStatusCard(msg, agentSlug)
	default:
		card = buildAgentResponseCard(msg, agentSlug, projectSlug)
	}

	// Marshal the card to JSON for the attachment.
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("marshal adaptive card: %w", err)
	}

	activity.Attachments = []Attachment{
		{
			ContentType: adaptiveCardContentType,
			Content:     json.RawMessage(cardJSON),
		},
	}

	return activity, nil
}

// buildAgentResponseCard creates an Adaptive Card for agent response messages
// (state-change, assistant-reply, instruction, etc.). The card includes a
// ColumnSet header with the agent name (bold, accent) and project slug (subtle).
func buildAgentResponseCard(msg *messages.StructuredMessage, agentSlug, projectSlug string) *AdaptiveCard {
	card := NewAdaptiveCard()

	// Header: agent name + project slug.
	header := ColumnSet{
		Type: "ColumnSet",
		Columns: []Column{
			{
				Type:  "Column",
				Width: "auto",
				Items: []CardElement{
					TextBlock{
						Type:   "TextBlock",
						Text:   agentSlug,
						Weight: "Bolder",
						Color:  "Accent",
					},
				},
			},
		},
	}

	if projectSlug != "" {
		header.Columns = append(header.Columns, Column{
			Type:  "Column",
			Width: "stretch",
			Items: []CardElement{
				TextBlock{
					Type:     "TextBlock",
					Text:     projectSlug,
					IsSubtle: true,
					Size:     "Small",
				},
			},
		})
	}

	card.Body = append(card.Body, header)

	// Body: message text.
	if msg.Msg != "" {
		card.Body = append(card.Body, TextBlock{
			Type: "TextBlock",
			Text: msg.Msg,
			Wrap: true,
		})
	}

	return card
}

// buildAskUserCard creates an Adaptive Card for input-needed messages.
// It includes a bold header ("agent-name needs your input"), a question body,
// and action buttons for each choice.
func buildAskUserCard(msg *messages.StructuredMessage, agentSlug string) *AdaptiveCard {
	card := NewAdaptiveCard()

	// Header.
	card.Body = append(card.Body, TextBlock{
		Type:   "TextBlock",
		Text:   fmt.Sprintf("%s needs your input", agentSlug),
		Weight: "Bolder",
	})

	// Question body.
	if msg.Msg != "" {
		card.Body = append(card.Body, TextBlock{
			Type: "TextBlock",
			Text: msg.Msg,
			Wrap: true,
		})
	}

	// Extract request_id from metadata.
	requestID := ""
	if msg.Metadata != nil {
		requestID = msg.Metadata["request_id"]
	}

	// Parse choices from metadata.
	var choices []string
	if msg.Metadata != nil {
		if choicesJSON, ok := msg.Metadata["choices"]; ok && choicesJSON != "" {
			_ = json.Unmarshal([]byte(choicesJSON), &choices)
		}
	}

	if len(choices) > 0 {
		for _, choice := range choices {
			style := ""
			lc := strings.ToLower(choice)
			if lc == "approve" || lc == "yes" || lc == "accept" {
				style = "positive"
			} else if lc == "reject" || lc == "no" || lc == "deny" {
				style = "destructive"
			}

			card.Actions = append(card.Actions, ActionSubmit{
				Type:  "Action.Submit",
				Title: choice,
				Style: style,
				Data: map[string]string{
					"action":     "ask_response",
					"request_id": requestID,
					"choice":     choice,
				},
			})
		}
	} else {
		// Default: Approve + Reject buttons.
		card.Actions = append(card.Actions,
			ActionSubmit{
				Type:  "Action.Submit",
				Title: "Approve",
				Style: "positive",
				Data: map[string]string{
					"action":     "ask_response",
					"request_id": requestID,
					"choice":     "approve",
				},
			},
			ActionSubmit{
				Type:  "Action.Submit",
				Title: "Reject",
				Style: "destructive",
				Data: map[string]string{
					"action":     "ask_response",
					"request_id": requestID,
					"choice":     "reject",
				},
			},
		)
	}

	// Custom Reply button.
	card.Actions = append(card.Actions, ActionSubmit{
		Type:  "Action.Submit",
		Title: "Custom Reply...",
		Data: map[string]string{
			"action":     "ask_input",
			"request_id": requestID,
		},
	})

	return card
}

// buildStatusCard creates an Adaptive Card for system/status update messages.
// Uses a ColumnSet with a status icon placeholder and subtle message text.
//
// NOTE: The design doc specifies an Image element for the status icon, but
// we intentionally use an emoji text character ("ℹ️") instead. This avoids
// a dependency on external image hosting for the icon URL while keeping
// the visual intent of the status indicator.
func buildStatusCard(msg *messages.StructuredMessage, agentSlug string) *AdaptiveCard {
	card := NewAdaptiveCard()

	statusText := msg.Msg
	if agentSlug != "" {
		statusText = fmt.Sprintf("%s: %s", agentSlug, msg.Msg)
	}

	header := ColumnSet{
		Type: "ColumnSet",
		Columns: []Column{
			{
				Type:  "Column",
				Width: "auto",
				Items: []CardElement{
					TextBlock{
						Type: "TextBlock",
						Text: "ℹ️",
						Size: "Medium",
					},
				},
			},
			{
				Type:  "Column",
				Width: "stretch",
				Items: []CardElement{
					TextBlock{
						Type:     "TextBlock",
						Text:     statusText,
						Wrap:     true,
						IsSubtle: true,
					},
				},
			},
		},
	}

	card.Body = append(card.Body, header)

	return card
}

// isPlainTextMessage determines whether a message should be sent as plain
// text (no card). Returns true when msg.Plain is set or the body is very
// short with no special formatting.
func isPlainTextMessage(msg *messages.StructuredMessage) bool {
	if msg.Plain {
		return true
	}

	body := msg.Msg
	if len(body) >= plainTextThreshold {
		return false
	}

	// Short messages without special formatting go as plain text.
	// Input-needed (needs buttons) and state-change (needs status card)
	// always use cards regardless of length.
	if msg.Type == messages.TypeInputNeeded || msg.Type == messages.TypeStateChange {
		return false
	}

	return true
}

// truncateText truncates s to maxLen characters. If truncated, it appends
// the truncation suffix so the user knows the message was cut.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-len(truncationSuffix)] + truncationSuffix
}

// deriveAgentSlug extracts the agent display name from the sender field.
func deriveAgentSlug(sender string) string {
	if strings.HasPrefix(sender, "agent:") {
		return strings.TrimPrefix(sender, "agent:")
	}
	return sender
}
