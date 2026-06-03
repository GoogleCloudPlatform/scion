package discord

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	// maxDiscordMessageLength is the maximum character length for a Discord message.
	maxDiscordMessageLength = 2000

	// truncationSuffix is appended when a message exceeds the Discord limit.
	truncationSuffix = "\n*[truncated]*"

	// headerBudget is a generous estimate of the byte overhead from header
	// text (agent name, mentions, prefix tags). The body is truncated to
	// leave room for the header so the total stays under the limit.
	headerBudget = 100
)

// FormatMessage converts a StructuredMessage to Discord-compatible text.
// For Phase 1, this is plain text formatting (embeds come in Phase 2).
func FormatMessage(msg *messages.StructuredMessage, agentSlug string, recipientMention string) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	// Determine sender slug for display.
	slug := agentSlug
	if slug == "" {
		if strings.HasPrefix(msg.Sender, "agent:") {
			slug = strings.TrimPrefix(msg.Sender, "agent:")
		} else {
			slug = msg.Sender
		}
	}

	// Header: agent identity and optional recipient.
	isAgentToAgent := strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:")
	if isAgentToAgent {
		recipientSlug := strings.TrimPrefix(msg.Recipient, "agent:")
		fmt.Fprintf(&b, "[agent:%s -> agent:%s]\n", slug, recipientSlug)
	} else if recipientMention != "" {
		fmt.Fprintf(&b, "**%s** -> %s\n", slug, recipientMention)
	} else {
		fmt.Fprintf(&b, "**%s**\n", slug)
	}

	// Prefix tags.
	if msg.Urgent {
		b.WriteString("**[URGENT]** ")
	}
	if msg.Broadcasted {
		b.WriteString("**[Broadcast]** ")
	}

	// Body text, truncated to fit within the Discord limit.
	body := msg.Msg
	maxBody := maxDiscordMessageLength - b.Len() - len(truncationSuffix)
	if maxBody < 0 {
		maxBody = 0
	}
	if len(body) > maxBody {
		body = truncateAtRuneBoundary(body, maxBody)
		body += truncationSuffix
	}
	b.WriteString(body)

	// Call-to-action for input-needed.
	if msg.Type == messages.TypeInputNeeded {
		b.WriteString("\n\nPlease reply to respond.")
	}

	return truncateForDiscord(b.String(), maxDiscordMessageLength)
}

// FormatStateChangeText formats a TypeStateChange as plain text (Phase 1).
// Phase 2 will use embeds with colored sidebars.
func FormatStateChangeText(msg *messages.StructuredMessage, agentSlug string) string {
	if msg == nil {
		return ""
	}

	slug := agentSlug
	if slug == "" {
		if strings.HasPrefix(msg.Sender, "agent:") {
			slug = strings.TrimPrefix(msg.Sender, "agent:")
		} else {
			slug = msg.Sender
		}
	}

	status := msg.Status
	if status == "" {
		status = "unknown"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s] **%s**", strings.ToUpper(status), slug)

	// Add activity from metadata if available.
	if msg.Metadata != nil {
		if activity, ok := msg.Metadata["activity"]; ok && activity != "" {
			fmt.Fprintf(&b, " -- %s", activity)
		}
	}

	if msg.Msg != "" {
		b.WriteString("\n")
		b.WriteString(msg.Msg)
	}

	return truncateForDiscord(b.String(), maxDiscordMessageLength)
}

// truncateForDiscord ensures text fits within the specified character limit.
// If truncation is needed, it walks backward to a valid rune boundary and
// appends a truncation indicator.
func truncateForDiscord(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	cutoff := maxLen - len(truncationSuffix)
	if cutoff < 0 {
		cutoff = 0
	}
	cutoff = truncateAtRuneBoundaryLen(text, cutoff)
	return text[:cutoff] + truncationSuffix
}

// truncateAtRuneBoundary truncates text to at most maxLen bytes, backing
// up to a valid UTF-8 rune boundary.
func truncateAtRuneBoundary(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	cutoff := maxLen
	for cutoff > 0 && !utf8.RuneStart(text[cutoff]) {
		cutoff--
	}
	return text[:cutoff]
}

// truncateAtRuneBoundaryLen returns a byte offset <= maxLen that sits on
// a valid UTF-8 rune boundary.
func truncateAtRuneBoundaryLen(text string, maxLen int) int {
	if maxLen >= len(text) {
		return len(text)
	}
	cutoff := maxLen
	for cutoff > 0 && !utf8.RuneStart(text[cutoff]) {
		cutoff--
	}
	return cutoff
}

// FormatDiscordMention formats a Discord user mention from a user ID.
func FormatDiscordMention(discordUserID string) string {
	return fmt.Sprintf("<@%s>", discordUserID)
}
