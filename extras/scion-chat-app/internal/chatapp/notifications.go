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

package chatapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// outboundEmailRe matches scion user emails in outbound messages, with optional "user:" prefix.
var outboundEmailRe = regexp.MustCompile(`(?:user:)?[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// NotificationRelay routes agent notifications to chat spaces as rich cards.
type NotificationRelay struct {
	store     *state.Store
	messenger Messenger
	sendQueue *SendQueue
	log       *slog.Logger
}

// NewNotificationRelay creates a new notification relay.
func NewNotificationRelay(store *state.Store, messenger Messenger, log *slog.Logger) *NotificationRelay {
	return &NotificationRelay{
		store:     store,
		messenger: messenger,
		log:       log,
	}
}

// SetSendQueue attaches a send queue to route outbound messages through
// per-space workers instead of sending directly via the messenger.
func (n *NotificationRelay) SetSendQueue(sq *SendQueue) {
	n.sendQueue = sq
}

// HandleBrokerMessage processes a message received via the broker plugin's Publish() path.
// User-targeted messages are always relayed. Agent-to-agent messages and state
// changes are relayed only when observe mode / state notifications are enabled
// on the space link.
//
// Expected topics:
//
//	scion.grove.<projectID>.user.<userID>.messages  — user-targeted message
//	scion.grove.<projectID>.agent.<agentID>.messages — agent-targeted message
func (n *NotificationRelay) HandleBrokerMessage(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	// Strip the "scion." prefix used by the broker topic hierarchy.
	normalized := strings.TrimPrefix(topic, "scion.")

	parts := strings.Split(normalized, ".")
	if len(parts) < 2 {
		n.log.Debug("ignoring message with short topic", "topic", topic)
		return nil
	}

	if parts[0] != "grove" {
		n.log.Debug("ignoring non-grove topic", "topic", topic)
		return nil
	}
	projectID := parts[1]

	// Classify the message for filtering.
	isAgentToAgent := msg != nil &&
		strings.HasPrefix(msg.Sender, "agent:") &&
		strings.HasPrefix(msg.Recipient, "agent:")
	isStateChange := msg != nil && msg.Type == messages.TypeStateChange

	// User-targeted messages are always relayed (they were explicitly
	// sent to a specific user and bypass observe/filter settings).
	if len(parts) >= 5 && parts[2] == "user" {
		return n.handleUserMessage(ctx, projectID, msg)
	}

	// Agent-to-agent and state change messages require observe filtering.
	if isAgentToAgent || isStateChange {
		return n.handleFilteredMessage(ctx, projectID, msg, isAgentToAgent, isStateChange)
	}

	n.log.Debug("ignoring non-user-targeted topic", "topic", topic)
	return nil
}

// handleFilteredMessage relays agent-to-agent or state change messages to
// linked spaces, applying per-space observe mode and state change filters.
func (n *NotificationRelay) handleFilteredMessage(ctx context.Context, projectID string, msg *messages.StructuredMessage, isAgentToAgent, isStateChange bool) error {
	links, err := n.store.ListSpaceLinks()
	if err != nil {
		return fmt.Errorf("listing space links: %w", err)
	}

	for _, link := range links {
		if link.ProjectID != projectID {
			continue
		}

		if isAgentToAgent && !link.ShowAgentToAgent {
			n.log.Debug("observe mode disabled, filtering agent-to-agent message",
				"space_id", link.SpaceID)
			continue
		}
		if isStateChange && !link.ShowStateChanges {
			n.log.Debug("state change notifications disabled, filtering",
				"space_id", link.SpaceID)
			continue
		}

		// Route to the appropriate renderer.
		if isAgentToAgent {
			card := n.renderObservedMessage(msg)
			if _, err := n.messenger.SendCard(ctx, link.SpaceID, card); err != nil {
				n.log.Error("failed to send observed message",
					"space_id", link.SpaceID,
					"error", err,
				)
			}
		} else {
			// State change notification — use the existing card renderer.
			card := n.renderNotificationCard(msg)
			mentions := n.getSubscriberMentions(msg, link)
			if mentions != "" {
				card.Sections = append(card.Sections, CardSection{
					Widgets: []Widget{
						{Type: WidgetText, Content: mentions},
					},
				})
			}
			if _, err := n.messenger.SendCard(ctx, link.SpaceID, card); err != nil {
				n.log.Error("failed to send state change notification",
					"space_id", link.SpaceID,
					"error", err,
				)
			}
		}
	}
	return nil
}

// renderObservedMessage creates a card for an observed agent-to-agent message,
// visually distinct from direct user messages.
func (n *NotificationRelay) renderObservedMessage(msg *messages.StructuredMessage) Card {
	senderSlug := msg.Sender
	if idx := strings.Index(senderSlug, ":"); idx >= 0 {
		senderSlug = senderSlug[idx+1:]
	}
	recipientSlug := msg.Recipient
	if idx := strings.Index(recipientSlug, ":"); idx >= 0 {
		recipientSlug = recipientSlug[idx+1:]
	}

	body := n.resolveOutboundMentions(msg.Msg)
	if len(body) > 500 {
		body = body[:500] + fmt.Sprintf("\n[%d chars truncated]", len(body)-500)
	}

	return Card{
		Header: CardHeader{
			Title:    fmt.Sprintf("%s → %s", senderSlug, recipientSlug),
			Subtitle: "Agent-to-agent message (observe mode)",
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: body},
				},
			},
		},
	}
}

// handleAgentNotification renders an agent status notification as a card in linked spaces.
func (n *NotificationRelay) handleAgentNotification(ctx context.Context, projectID string, msg *messages.StructuredMessage) error {
	// Find all spaces linked to this project
	links, err := n.store.ListSpaceLinks()
	if err != nil {
		return fmt.Errorf("listing space links: %w", err)
	}

	for _, link := range links {
		if link.ProjectID != projectID {
			continue
		}

		// Determine notification style from message type and content
		card := n.renderNotificationCard(msg)

		// Find subscribers for @mentions
		mentions := n.getSubscriberMentions(msg, link)

		// Add mentions to the card text if any
		if mentions != "" {
			card.Sections = append(card.Sections, CardSection{
				Widgets: []Widget{
					{Type: WidgetText, Content: mentions},
				},
			})
		}

		var sendErr error
		if n.sendQueue != nil {
			_, sendErr = n.sendQueue.Send(ctx, SendMessageRequest{
				SpaceID: link.SpaceID,
				Card:    &card,
			})
		} else if n.messenger != nil {
			_, sendErr = n.messenger.SendCard(ctx, link.SpaceID, card)
		} else {
			n.log.Warn("no messenger or send queue configured, skipping notification",
				"space_id", link.SpaceID,
				"project_id", projectID,
			)
			continue
		}
		if sendErr != nil {
			n.log.Error("failed to send notification card",
				"space_id", link.SpaceID,
				"project_id", projectID,
				"error", sendErr,
			)
			// Continue to other spaces
		}
	}

	return nil
}

// handleUserMessage relays a user-targeted message to chat.
// It maps the Hub user ID (RecipientID) back to a chat platform user and delivers
// the message to all spaces linked to the project. Direct messages from agents do
// not require the user to have any subscriptions — subscriptions only control
// @mentions in agent notification broadcasts.
func (n *NotificationRelay) handleUserMessage(ctx context.Context, projectID string, msg *messages.StructuredMessage) error {
	if msg.RecipientID == "" {
		n.log.Debug("user message has no recipient ID, skipping relay")
		return nil
	}

	if msg.Type == messages.TypeAssistantReply {
		if len(msg.Msg) > 500 {
			msg.Msg = msg.Msg[:500] + fmt.Sprintf("\n[%d chars truncated]", len(msg.Msg)-500)
		}
	} else if msg.Type != messages.TypeInstruction {
		n.log.Debug("routing non-instruction user message to notification path",
			"type", msg.Type,
			"sender", msg.Sender,
			"recipient_id", msg.RecipientID,
		)
		return n.handleAgentNotification(ctx, projectID, msg)
	}

	// Look up the chat platform user for this Hub user
	mapping, err := n.store.GetUserMappingByHubID(msg.RecipientID)
	if err != nil {
		return fmt.Errorf("looking up user mapping: %w", err)
	}
	if mapping == nil {
		n.log.Debug("no chat platform mapping for hub user, skipping relay",
			"hub_user_id", msg.RecipientID,
		)
		return nil
	}

	// Extract agent identity from sender
	agentSlug := msg.Sender
	if idx := strings.Index(agentSlug, ":"); idx >= 0 {
		agentSlug = agentSlug[idx+1:]
	}

	// Resolve outbound mentions (replace hub emails with @mentions).
	msg.Msg = n.resolveOutboundMentions(msg.Msg)

	// Find spaces linked to the project from the message topic
	links, err := n.store.ListSpaceLinks()
	if err != nil {
		return fmt.Errorf("listing space links: %w", err)
	}

	// Resolve outbound attachments from agent-side paths.
	var attachments []Attachment
	var projectSlug string
	if len(msg.Attachments) > 0 {
		for _, link := range links {
			if link.ProjectID == projectID {
				projectSlug = link.ProjectSlug
				attachments = ResolveOutboundAttachments(n.log, msg.Attachments, link.ProjectSlug, link.ProjectID)
				break
			}
		}

		// Check for oversized files and send error cards.
		if projectSlug != "" {
			n.sendOversizeErrorCards(ctx, msg.Attachments, projectSlug, projectID, mapping.Platform, links)
		}
	}

	for _, link := range links {
		if link.ProjectID != projectID || link.Platform != mapping.Platform {
			continue
		}

		card := Card{
			Header: CardHeader{
				Title: fmt.Sprintf("\U0001F916 %s", agentSlug),
			},
			Sections: []CardSection{
				{
					Widgets: []Widget{
						{Type: WidgetText, Content: msg.Msg},
					},
				},
			},
		}

		// @mentions go in the text body (not inside card widgets) so the
		// Chat API renders them as interactive user pills.
		mentions := n.buildMentions(mapping.PlatformUserID, agentSlug, link)

		sendReq := SendMessageRequest{
			SpaceID:     link.SpaceID,
			ThreadID:    msg.ThreadID,
			Text:        mentions,
			Card:        &card,
			Attachments: attachments,
		}
		var sendErr error
		if n.sendQueue != nil {
			_, sendErr = n.sendQueue.Send(ctx, sendReq)
		} else if n.messenger != nil {
			_, sendErr = n.messenger.SendMessage(ctx, sendReq)
		} else {
			n.log.Warn("no messenger or send queue configured, skipping user message relay",
				"space_id", link.SpaceID,
				"recipient", msg.RecipientID,
			)
			continue
		}
		if sendErr != nil {
			n.log.Error("failed to relay user message",
				"space_id", link.SpaceID,
				"recipient", msg.RecipientID,
				"error", sendErr,
			)
		}
	}

	return nil
}

// renderNotificationCard creates a card for an agent notification.
func (n *NotificationRelay) renderNotificationCard(msg *messages.StructuredMessage) Card {
	// Extract agent slug from sender (e.g., "agent:deploy-agent" -> "deploy-agent")
	agentSlug := msg.Sender
	if idx := strings.Index(agentSlug, ":"); idx >= 0 {
		agentSlug = agentSlug[idx+1:]
	}

	// Determine card style based on message type and content
	activity := extractActivity(msg)
	header, style := notificationStyle(activity)

	card := Card{
		Header: CardHeader{
			Title:    fmt.Sprintf("%s %s", style.icon, agentSlug),
			Subtitle: fmt.Sprintf("%s | %s", activity, header),
		},
		Sections: []CardSection{
			{
				Widgets: []Widget{
					{Type: WidgetText, Content: msg.Msg},
				},
			},
		},
	}

	// Add action buttons based on activity
	switch activity {
	case "COMPLETED":
		card.Actions = []CardAction{
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", agentSlug)},
		}
	case "WAITING_FOR_INPUT":
		card.Sections = append(card.Sections, CardSection{
			Header: "Respond",
			Widgets: []Widget{
				{Type: WidgetInput, Label: "Your response", ActionID: fmt.Sprintf("agent.respond.%s", agentSlug)},
			},
		})
		card.Actions = []CardAction{
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", agentSlug)},
		}
	case "ERROR":
		card.Actions = []CardAction{
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", agentSlug)},
			{Label: "Restart", ActionID: fmt.Sprintf("agent.start.%s", agentSlug), Style: "primary"},
		}
	case "STALLED":
		card.Actions = []CardAction{
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", agentSlug)},
			{Label: "Restart", ActionID: fmt.Sprintf("agent.start.%s", agentSlug), Style: "primary"},
			{Label: "Stop", ActionID: fmt.Sprintf("agent.stop.%s", agentSlug), Style: "danger"},
		}
	case "LIMITS_EXCEEDED":
		card.Actions = []CardAction{
			{Label: "View Logs", ActionID: fmt.Sprintf("agent.logs.%s", agentSlug)},
			{Label: "Stop", ActionID: fmt.Sprintf("agent.stop.%s", agentSlug), Style: "danger"},
		}
	case "DELETED":
		// No actions for deleted agents
	}

	return card
}

// notificationStyleInfo holds visual style for a notification type.
type notificationStyleInfo struct {
	icon string
}

// notificationStyle returns the header text and style for a given activity.
func notificationStyle(activity string) (string, notificationStyleInfo) {
	switch activity {
	case "COMPLETED":
		return "Completed", notificationStyleInfo{icon: "\u2705"}
	case "WAITING_FOR_INPUT":
		return "Needs Input", notificationStyleInfo{icon: "\u231b"}
	case "ERROR":
		return "Error", notificationStyleInfo{icon: "\u274c"}
	case "STALLED":
		return "Stalled", notificationStyleInfo{icon: "\u26a0\ufe0f"}
	case "LIMITS_EXCEEDED":
		return "Limits Exceeded", notificationStyleInfo{icon: "\u26a0\ufe0f"}
	case "DELETED":
		return "Deleted", notificationStyleInfo{icon: "\U0001F5D1\ufe0f"}
	default:
		return activity, notificationStyleInfo{icon: "\u2139\ufe0f"}
	}
}

// extractActivity determines the activity type from a message.
func extractActivity(msg *messages.StructuredMessage) string {
	if msg.Status != "" {
		return strings.ToUpper(msg.Status)
	}

	// Legacy fallback: infer activity from message content for messages
	// that pre-date the structured Status field.
	content := strings.ToUpper(msg.Msg)

	activities := []string{"COMPLETED", "WAITING_FOR_INPUT", "LIMITS_EXCEEDED", "STALLED", "ERROR", "DELETED"}
	for _, a := range activities {
		if strings.Contains(content, a) {
			return a
		}
	}

	switch msg.Type {
	case messages.TypeInputNeeded:
		return "WAITING_FOR_INPUT"
	case messages.TypeStateChange:
		return "STATE_CHANGE"
	case messages.TypeSystem:
		return "SYSTEM"
	default:
		return "INFO"
	}
}

// getSubscriberMentions returns a formatted string of @mentions for users
// subscribed to the given agent's notifications.
func (n *NotificationRelay) getSubscriberMentions(msg *messages.StructuredMessage, link state.SpaceLink) string {
	agentSlug := msg.Sender
	if idx := strings.Index(agentSlug, ":"); idx >= 0 {
		agentSlug = agentSlug[idx+1:]
	}

	subs, err := n.store.ListAgentSubscriptions(agentSlug, link.ProjectID)
	if err != nil {
		n.log.Error("listing agent subscriptions", "error", err)
		return ""
	}

	activity := extractActivity(msg)
	var mentions []string

	for _, sub := range subs {
		if sub.Platform != link.Platform {
			continue
		}

		// Check activity filter
		if sub.Activities != "" {
			allowed := strings.Split(sub.Activities, ",")
			matched := false
			for _, a := range allowed {
				if strings.TrimSpace(strings.ToUpper(a)) == activity {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		// Format platform-specific mention
		mentions = append(mentions, fmt.Sprintf("<users/%s>", sub.PlatformUserID))
	}

	if len(mentions) == 0 {
		return ""
	}
	return "CC: " + strings.Join(mentions, " ")
}

// resolveOutboundMentions scans text for scion user emails (with optional
// "user:" prefix) and replaces them with Google Chat @mentions when the user
// has a mapping in the store. Emails embedded in URL paths (preceded by '/'
// or ':' or followed by '/') are left untouched.
func (n *NotificationRelay) resolveOutboundMentions(text string) string {
	if text == "" {
		return text
	}

	matches := outboundEmailRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var b strings.Builder
	b.Grow(len(text))
	prev := 0

	for _, loc := range matches {
		start, end := loc[0], loc[1]

		// Skip emails embedded in URL paths or mailto links.
		if start > 0 {
			if text[start-1] == '/' {
				continue
			}
			if text[start-1] == ':' {
				preceding := text[:start]
				if strings.HasSuffix(preceding, "mailto:") || strings.HasSuffix(preceding, "http:") || strings.HasSuffix(preceding, "https:") {
					continue
				}
			}
		}
		if end < len(text) && text[end] == '/' {
			continue
		}

		email := text[start:end]
		if strings.HasPrefix(email, "user:") {
			email = strings.TrimPrefix(email, "user:")
		}

		mapping, err := n.store.GetUserMappingByHubEmail(email)
		if err != nil || mapping == nil {
			continue
		}

		// Write everything before this match, then the replacement.
		b.WriteString(text[prev:start])
		fmt.Fprintf(&b, "<users/%s>", mapping.PlatformUserID)
		prev = end
	}

	// If no replacements were made, return original text.
	if prev == 0 {
		return text
	}

	b.WriteString(text[prev:])
	return b.String()
}

// sendOversizeErrorCards checks agent-side attachment paths for files exceeding
// the size limit and sends error cards to the relevant spaces.
func (n *NotificationRelay) sendOversizeErrorCards(ctx context.Context, attachPaths []string, projectSlug, projectID, platform string, links []state.SpaceLink) {
	for _, agentPath := range attachPaths {
		if agentPath == "" {
			continue
		}
		hostPath := resolveAgentPath(agentPath, projectSlug, projectID)
		if hostPath == "" {
			continue
		}
		fi, err := os.Stat(hostPath)
		if err != nil {
			continue
		}
		if fi.Size() > MaxAttachmentSize {
			errCard := SizeLimitErrorCard(fi.Name(), fi.Size())
			for _, link := range links {
				if link.ProjectID != projectID || link.Platform != platform {
					continue
				}
				if _, sendErr := n.messenger.SendCard(ctx, link.SpaceID, errCard); sendErr != nil {
					n.log.Error("failed to send size limit error card",
						"space_id", link.SpaceID, "error", sendErr)
				}
			}
		}
	}
}

// buildMentions returns a formatted @mention string for a user-targeted message.
// It always includes the direct recipient and appends any subscribers to the
// agent in that space, deduplicating against the recipient.
func (n *NotificationRelay) buildMentions(recipientPlatformID, agentSlug string, link state.SpaceLink) string {
	// Start with the direct recipient
	seen := map[string]bool{recipientPlatformID: true}
	mentions := []string{fmt.Sprintf("<%s>", recipientPlatformID)}

	// Add subscribers for this agent/project, skipping the recipient to avoid duplication
	subs, err := n.store.ListAgentSubscriptions(agentSlug, link.ProjectID)
	if err != nil {
		n.log.Error("listing agent subscriptions for mentions", "error", err)
		return strings.Join(mentions, " ")
	}

	for _, sub := range subs {
		if sub.Platform != link.Platform || seen[sub.PlatformUserID] {
			continue
		}
		seen[sub.PlatformUserID] = true
		mentions = append(mentions, fmt.Sprintf("<users/%s>", sub.PlatformUserID))
	}

	return strings.Join(mentions, " ")
}
