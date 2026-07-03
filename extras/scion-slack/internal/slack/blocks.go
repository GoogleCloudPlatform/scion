package slack

import (
	"fmt"
	"strings"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	maxSlackMessageLength = 4000
	truncationSuffix      = "\n_[truncated]_"
)

// FormatMessage converts a StructuredMessage to Slack-compatible mrkdwn text.
func FormatMessage(msg *messages.StructuredMessage, agentSlug string) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	slug := agentSlug
	if slug == "" {
		if strings.HasPrefix(msg.Sender, "agent:") {
			slug = strings.TrimPrefix(msg.Sender, "agent:")
		} else {
			slug = msg.Sender
		}
	}

	isAgentToAgent := strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:")
	if isAgentToAgent {
		recipientSlug := strings.TrimPrefix(msg.Recipient, "agent:")
		fmt.Fprintf(&b, "*%s* → *%s*\n", slug, recipientSlug)
	} else if slug != "" {
		fmt.Fprintf(&b, "*%s*", slug)
		if msg.Status != "" {
			fmt.Fprintf(&b, " [%s]", msg.Status)
		}
		b.WriteString("\n")
	}

	if msg.Urgent {
		b.WriteString("*[URGENT]* ")
	}
	if msg.Broadcasted {
		b.WriteString("*[Broadcast]* ")
	}

	b.WriteString(msg.Msg)

	if msg.Type == messages.TypeInputNeeded {
		b.WriteString("\n\nPlease reply to respond.")
	}

	return truncateMessage(b.String())
}

// FormatWebhookMessage formats a StructuredMessage for sending with per-agent
// identity (username override). The agent name is already shown as the sender,
// so the body omits the agent header.
func FormatWebhookMessage(msg *messages.StructuredMessage) string {
	if msg == nil {
		return ""
	}

	var b strings.Builder

	if msg.Urgent {
		b.WriteString("*[URGENT]* ")
	}
	if msg.Broadcasted {
		b.WriteString("*[Broadcast]* ")
	}

	if strings.HasPrefix(msg.Sender, "agent:") && strings.HasPrefix(msg.Recipient, "agent:") {
		recipientSlug := strings.TrimPrefix(msg.Recipient, "agent:")
		fmt.Fprintf(&b, "→ *%s*\n", recipientSlug)
	}

	if msg.Status != "" {
		fmt.Fprintf(&b, "[%s] ", msg.Status)
	}

	b.WriteString(msg.Msg)

	return truncateMessage(b.String())
}

// FormatStateChange formats a TypeStateChange message as mrkdwn text.
func FormatStateChange(msg *messages.StructuredMessage, agentSlug string) string {
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
	fmt.Fprintf(&b, "[%s] *%s*", strings.ToUpper(status), slug)

	if msg.Metadata != nil {
		if activity, ok := msg.Metadata["activity"]; ok && activity != "" {
			fmt.Fprintf(&b, " — %s", activity)
		}
	}

	if msg.Msg != "" {
		b.WriteString("\n")
		b.WriteString(msg.Msg)
	}

	return truncateMessage(b.String())
}

// RenderInputNeededBlocks builds Block Kit blocks with action buttons for
// a TypeInputNeeded message.
func RenderInputNeededBlocks(msg *messages.StructuredMessage, agentSlug, requestID string) []slackapi.Block {
	if msg == nil {
		return nil
	}

	var blocks []slackapi.Block

	title := fmt.Sprintf("Input Needed — %s", agentSlug)
	blocks = append(blocks, slackapi.NewHeaderBlock(
		slackapi.NewTextBlockObject("plain_text", title, false, false),
	))

	if msg.Msg != "" {
		body := msg.Msg
		if len(body) > 3000 {
			body = truncateAtRuneBoundary(body, 2990) + truncationSuffix
		}
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", body, false, false),
			nil, nil,
		))
	}

	blocks = append(blocks, slackapi.NewDividerBlock())

	var buttons []slackapi.BlockElement
	buttons = append(buttons,
		slackapi.NewButtonBlockElement(
			fmt.Sprintf("ask:reply:%s", requestID),
			"reply",
			slackapi.NewTextBlockObject("plain_text", "Reply", false, false),
		),
		slackapi.NewButtonBlockElement(
			fmt.Sprintf("ask:dismiss:%s", requestID),
			"dismiss",
			slackapi.NewTextBlockObject("plain_text", "Dismiss", false, false),
		),
	)
	blocks = append(blocks, slackapi.NewActionBlock("", buttons...))

	return blocks
}

// truncateMessage ensures the message fits within Slack's message length limit.
func truncateMessage(text string) string {
	if len(text) <= maxSlackMessageLength {
		return text
	}
	cutoff := maxSlackMessageLength - len(truncationSuffix)
	if cutoff < 0 {
		cutoff = 0
	}
	cutoff = truncateAtRuneBoundaryLen(text, cutoff)
	return text[:cutoff] + truncationSuffix
}

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
