package discord

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	// maxDiscordMessageLength is the maximum character length for a Discord message.
	maxDiscordMessageLength = 2000

	// maxEmbedDescriptionLength is the maximum character length for an embed description.
	maxEmbedDescriptionLength = 4096

	// maxEmbedFieldValueLength is the maximum character length for an embed field value.
	maxEmbedFieldValueLength = 1024

	// maxEmbedTitleLength is the maximum character length for an embed title.
	maxEmbedTitleLength = 256

	// maxButtonsPerRow is the maximum number of buttons allowed in a single Discord action row.
	maxButtonsPerRow = 5

	// truncationSuffix is appended when a message exceeds the Discord limit.
	truncationSuffix = "\n*[truncated]*"

	// headerBudget is a generous estimate of the byte overhead from header
	// text (agent name, mentions, prefix tags). The body is truncated to
	// leave room for the header so the total stays under the limit.
	headerBudget = 100
)

// Embed sidebar colors keyed by activity/status string.
const (
	colorCompleted = 0x2ECC71 // Green
	colorInputWait = 0xF1C40F // Yellow
	colorError     = 0xE74C3C // Red
	colorStalled   = 0xE67E22 // Orange
	colorDeleted   = 0x95A5A6 // Gray
	colorRunning   = 0x3498DB // Blue
	colorDefault   = 0x1A1A2E // Dark
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

// activityColor returns the embed sidebar color for the given activity/status.
func activityColor(activity string) int {
	switch activity {
	case "COMPLETED":
		return colorCompleted
	case "WAITING_FOR_INPUT":
		return colorInputWait
	case "ERROR":
		return colorError
	case "STALLED", "LIMITS_EXCEEDED":
		return colorStalled
	case "DELETED":
		return colorDeleted
	case "RUNNING":
		return colorRunning
	default:
		return colorDefault
	}
}

// RenderStateChangeEmbed builds a colored Discord embed for a TypeStateChange message.
// The sidebar color reflects the agent's current activity/status.
func RenderStateChangeEmbed(msg *messages.StructuredMessage, agentSlug string) *discordgo.MessageEmbed {
	if msg == nil {
		return nil
	}

	activity := ""
	projectID := ""
	summary := ""
	if msg.Metadata != nil {
		activity = msg.Metadata["activity"]
		projectID = msg.Metadata["project_id"]
		summary = msg.Metadata["summary"]
	}

	title := agentSlug
	if activity != "" {
		title = fmt.Sprintf("%s — %s", agentSlug, activity)
	}
	title = truncateForDiscord(title, maxEmbedTitleLength)

	description := msg.Msg
	if len(description) > maxEmbedDescriptionLength {
		description = truncateForDiscord(description, maxEmbedDescriptionLength)
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       activityColor(activity),
		Timestamp:   msg.Timestamp,
	}

	if projectID != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Project: %s", projectID),
		}
	}

	if summary != "" {
		if len(summary) > maxEmbedFieldValueLength {
			summary = truncateForDiscord(summary, maxEmbedFieldValueLength)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  "Summary",
			Value: summary,
		})
	}

	return embed
}

// RenderInputNeeded builds an embed and interactive components for a TypeInputNeeded message.
// If msg.Metadata["choices"] contains a JSON array of strings, each choice is rendered as a
// button. Otherwise, a generic "Reply" and "Dismiss" button pair is returned.
func RenderInputNeeded(msg *messages.StructuredMessage, agentSlug, requestID string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	if msg == nil {
		return nil, nil
	}

	description := msg.Msg
	if len(description) > maxEmbedDescriptionLength {
		description = truncateForDiscord(description, maxEmbedDescriptionLength)
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("Input Needed — %s", agentSlug),
		Description: description,
		Color:       colorInputWait,
	}

	var components []discordgo.MessageComponent

	choicesJSON := ""
	if msg.Metadata != nil {
		choicesJSON = msg.Metadata["choices"]
	}

	if choicesJSON != "" {
		var choices []string
		if err := json.Unmarshal([]byte(choicesJSON), &choices); err == nil && len(choices) > 0 {
			var buttons []discordgo.MessageComponent
			for idx, choice := range choices {
				buttons = append(buttons, discordgo.Button{
					Label:    choice,
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("ask:opt:%s:%d", requestID, idx),
				})
				if len(buttons) == maxButtonsPerRow || idx == len(choices)-1 {
					components = append(components, discordgo.ActionsRow{
						Components: buttons,
					})
					buttons = nil
				}
			}
			return embed, components
		}
	}

	// Default: Reply + Dismiss buttons.
	components = append(components, discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "Reply",
				Style:    discordgo.PrimaryButton,
				CustomID: fmt.Sprintf("ask:reply:%s", requestID),
			},
			discordgo.Button{
				Label:    "Dismiss",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ask:dismiss:%s", requestID),
			},
		},
	})
	return embed, components
}

// FormatWithEmbed decides whether to return plain text, an embed, or both,
// based on the message length.
//
//   - ≤2000 chars: plain text content, no embeds
//   - ≤4096 chars: empty content, single embed with description
//   - >4096 chars: first 4096 in an embed, remainder returned as plain text
//     (caller is responsible for splitting the remainder into ≤2000-char chunks
//     via SplitLongMessage)
func FormatWithEmbed(msg *messages.StructuredMessage, agentSlug string) (string, []*discordgo.MessageEmbed) {
	if msg == nil {
		return "", nil
	}

	body := msg.Msg
	if len(body) <= maxDiscordMessageLength {
		return body, nil
	}

	if len(body) <= maxEmbedDescriptionLength {
		embed := &discordgo.MessageEmbed{
			Description: body,
		}
		return "", []*discordgo.MessageEmbed{embed}
	}

	// Body exceeds embed description limit: put the first 4096 in an embed,
	// return the remainder as content text (possibly requiring further splitting).
	cutoff := maxEmbedDescriptionLength - len(truncationSuffix)
	cutoff = truncateAtRuneBoundaryLen(body, cutoff)
	embedText := body[:cutoff] + truncationSuffix

	remainder := body[cutoff:]

	embed := &discordgo.MessageEmbed{
		Description: embedText,
	}
	return remainder, []*discordgo.MessageEmbed{embed}
}

// SplitLongMessage splits text into chunks of at most maxLen characters.
// It prefers to split at newline boundaries. If no newline is found within
// the window, it falls back to splitting at maxLen on a rune boundary.
func SplitLongMessage(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = maxDiscordMessageLength
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Look for the last newline within the allowed window.
		cutoff := maxLen
		if cutoff > len(text) {
			cutoff = len(text)
		}
		splitAt := strings.LastIndex(text[:cutoff], "\n")
		if splitAt <= 0 {
			// No suitable newline — split at rune boundary.
			splitAt = truncateAtRuneBoundaryLen(text, maxLen)
		} else {
			// Include the newline in the current chunk.
			splitAt++
		}

		chunks = append(chunks, text[:splitAt])
		text = text[splitAt:]
	}
	return chunks
}
