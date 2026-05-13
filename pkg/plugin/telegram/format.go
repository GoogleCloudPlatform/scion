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
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	// maxTelegramMessageLength is the maximum character length for a Telegram message.
	maxTelegramMessageLength = 4096

	// truncationSuffix is appended when a message exceeds the Telegram limit.
	truncationSuffix = "\n[truncated]"
)

// FormatMessage converts a StructuredMessage into formatted text suitable for
// Telegram. It returns the text content. Plain text is used (no parse_mode)
// for reliability, since message content from agents may contain arbitrary
// characters that would break MarkdownV2 escaping.
func FormatMessage(msg *messages.StructuredMessage) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	// Add urgent/broadcast prefixes
	if msg.Urgent {
		b.WriteString("[URGENT] ")
	}
	if msg.Broadcasted {
		b.WriteString("[Broadcast] ")
	}

	// Build sender label: "🤖 agent-slug" for agents (no @ to avoid Telegram mention detection).
	senderLabel := msg.Sender
	if strings.HasPrefix(msg.Sender, "agent:") {
		slug := strings.TrimPrefix(msg.Sender, "agent:")
		senderLabel = "🤖 " + slug
	}

	b.WriteString(senderLabel)

	// Add status if present
	if msg.Status != "" {
		fmt.Fprintf(&b, " [%s]", msg.Status)
	}

	// Add message body
	b.WriteString("\n\n")
	b.WriteString(msg.Msg)

	// Add call-to-action for input-needed
	if msg.Type == messages.TypeInputNeeded {
		b.WriteString("\n\nPlease reply in this chat to respond.")
	}

	text := b.String()
	return truncateMessage(text)
}

// maxTaskSummaryLength is the maximum length for the task summary line
// in a state-change card before truncation.
const maxTaskSummaryLength = 200

// stateEmoji maps agent states to display emoji matching the scion web UI.
var stateEmoji = map[string]string{
	"created":      "🆕",
	"provisioning": "📦",
	"cloning":      "📥",
	"starting":     "🚀",
	"running":      "▶️",
	"stopping":     "⏹️",
	"stopped":      "⏹️",
	"suspended":    "⏸️",
	"error":        "❌",
	"idle":         "💤",
	"thinking":     "💭",
	"executing":    "⚙️",
	"blocked":      "🚧",
	"completed":    "✅",
	"stalled":      "⏳",
	"offline":      "📡",
}

// stateLabel maps agent states to human-readable labels.
var stateLabel = map[string]string{
	"created":      "Created",
	"provisioning": "Provisioning",
	"cloning":      "Cloning",
	"starting":     "Starting",
	"running":      "Running",
	"stopping":     "Stopping",
	"stopped":      "Stopped",
	"suspended":    "Suspended",
	"error":        "Error",
	"idle":         "Idle",
	"thinking":     "Thinking",
	"executing":    "Executing",
	"blocked":      "Blocked",
	"completed":    "Completed",
	"stalled":      "Stalled",
	"offline":      "Offline",
}

// FormatStateChangeCard converts a state-change StructuredMessage into an
// HTML-formatted status card for Telegram. The card shows the agent name,
// state emoji, project name, timestamp, and task summary. All user-supplied
// content is HTML-escaped to prevent injection. The result is guaranteed to
// be under maxTelegramMessageLength.
func FormatStateChangeCard(msg *messages.StructuredMessage, agentSlug string) string {
	if msg == nil {
		return ""
	}

	// Determine the status string (normalise to lowercase for lookup).
	status := strings.ToLower(msg.Status)

	emoji := stateEmoji[status]
	if emoji == "" {
		emoji = "⚪"
	}

	label := stateLabel[status]
	if label == "" {
		// Capitalise the first letter of the raw status as fallback.
		if msg.Status != "" {
			label = strings.ToUpper(msg.Status[:1]) + msg.Status[1:]
		} else {
			label = "Unknown"
		}
	}

	escapedSlug := html.EscapeString(agentSlug)
	if escapedSlug == "" {
		// Fall back to sender slug if agentSlug is empty.
		if strings.HasPrefix(msg.Sender, "agent:") {
			escapedSlug = html.EscapeString(strings.TrimPrefix(msg.Sender, "agent:"))
		} else {
			escapedSlug = html.EscapeString(msg.Sender)
		}
	}

	var b strings.Builder

	// Header: <b>🟢 coder — Running</b>
	fmt.Fprintf(&b, "<b>%s %s — %s</b>\n", emoji, escapedSlug, html.EscapeString(label))

	// Project line.
	project := ""
	if msg.Metadata != nil {
		if pid, ok := msg.Metadata["project_id"]; ok && pid != "" {
			project = pid
		}
	}
	if project != "" {
		fmt.Fprintf(&b, "📋 Project: %s\n", html.EscapeString(project))
	}

	// Timestamp line.
	ts := formatTimestamp(msg.Timestamp)
	if ts != "" {
		fmt.Fprintf(&b, "🕐 %s\n", html.EscapeString(ts))
	}

	// Task summary (the message body).
	summary := strings.TrimSpace(msg.Msg)
	if summary != "" {
		if len(summary) > maxTaskSummaryLength {
			summary = summary[:maxTaskSummaryLength] + "…"
		}
		if status == "error" {
			fmt.Fprintf(&b, "⚠️ %s", html.EscapeString(summary))
		} else {
			b.WriteString(html.EscapeString(summary))
		}
	}

	text := b.String()
	return truncateMessage(text)
}

// FormatInputNeededCard converts an input-needed StructuredMessage into an
// HTML-formatted card for Telegram DMs. The card header shows a robot emoji
// and agent name, followed by the prompt text. All user-supplied content is
// HTML-escaped.
func FormatInputNeededCard(msg *messages.StructuredMessage, agentSlug string) string {
	if msg == nil {
		return ""
	}

	escapedSlug := html.EscapeString(agentSlug)
	if escapedSlug == "" {
		if strings.HasPrefix(msg.Sender, "agent:") {
			escapedSlug = html.EscapeString(strings.TrimPrefix(msg.Sender, "agent:"))
		} else {
			escapedSlug = html.EscapeString(msg.Sender)
		}
	}

	var b strings.Builder

	fmt.Fprintf(&b, "<b>🤖 %s [input needed]</b>\n", escapedSlug)

	project := ""
	if msg.Metadata != nil {
		if pid, ok := msg.Metadata["project_id"]; ok && pid != "" {
			project = pid
		}
	}
	if project != "" {
		fmt.Fprintf(&b, "📋 Project: %s\n", html.EscapeString(project))
	}

	ts := formatTimestamp(msg.Timestamp)
	if ts != "" {
		fmt.Fprintf(&b, "🕐 %s\n", html.EscapeString(ts))
	}

	prompt := strings.TrimSpace(msg.Msg)
	if prompt != "" {
		if len(prompt) > maxTaskSummaryLength {
			prompt = prompt[:maxTaskSummaryLength] + "…"
		}
		fmt.Fprintf(&b, "\n%s", html.EscapeString(prompt))
	}

	return truncateMessage(b.String())
}

// formatTimestamp parses an RFC3339 timestamp and returns a human-friendly
// representation like "May 13, 2:30 PM UTC". Returns the raw string if
// parsing fails.
func formatTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.UTC().Format("Jan 2, 3:04 PM UTC")
}

// truncateMessage ensures the text does not exceed Telegram's message limit.
// It walks backward to a valid UTF-8 rune boundary to avoid splitting
// multi-byte characters (emoji, CJK, accented characters).
func truncateMessage(text string) string {
	if len(text) <= maxTelegramMessageLength {
		return text
	}
	// Leave room for the truncation suffix
	cutoff := maxTelegramMessageLength - len(truncationSuffix)
	if cutoff < 0 {
		cutoff = 0
	}
	// Walk backward to a valid rune boundary
	for cutoff > 0 && !utf8.RuneStart(text[cutoff]) {
		cutoff--
	}
	return text[:cutoff] + truncationSuffix
}
