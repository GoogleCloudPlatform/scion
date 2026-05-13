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
	"strings"
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

	// Build sender label: "🤖 @agent-slug" for agents, sender string for others.
	senderLabel := msg.Sender
	if strings.HasPrefix(msg.Sender, "agent:") {
		slug := strings.TrimPrefix(msg.Sender, "agent:")
		senderLabel = "🤖 @" + slug
	}

	// Header: sender label, with type qualifier for non-reply types.
	switch msg.Type {
	case messages.TypeInstruction:
		fmt.Fprintf(&b, "%s [instruction]", senderLabel)
	case messages.TypeInputNeeded:
		fmt.Fprintf(&b, "%s [input needed]", senderLabel)
	case messages.TypeStateChange:
		fmt.Fprintf(&b, "%s [state change]", senderLabel)
	default:
		b.WriteString(senderLabel)
	}

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
