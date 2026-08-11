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
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// atMentionRegex matches <at>...</at> mention tags injected by Teams.
// Compiled once at package level to avoid re-compiling on every message.
var atMentionRegex = regexp.MustCompile(`<at>[^<]*</at>\s*`)

// activityToStructuredMessage converts a Teams Activity into a Scion
// StructuredMessage for delivery to the hub.
func activityToStructuredMessage(activity *Activity, botID string) *messages.StructuredMessage {
	text := stripBotMention(activity.Text, botID)

	metadata := map[string]string{
		"teams_conversation_id": activity.Conversation.ID,
		"teams_activity_id":     activity.ID,
	}

	if activity.ServiceURL != "" {
		metadata["teams_service_url"] = activity.ServiceURL
	}
	if activity.ReplyToID != "" {
		metadata["teams_reply_to_id"] = activity.ReplyToID
	}
	if activity.Conversation.TenantID != "" {
		metadata["teams_tenant_id"] = activity.Conversation.TenantID
	}

	// Extract Teams-specific channel/team IDs from channelData.
	if activity.ChannelData != nil {
		if activity.ChannelData.TeamsChannelID != "" {
			metadata["teams_channel_id"] = activity.ChannelData.TeamsChannelID
		} else if activity.ChannelData.Channel != nil && activity.ChannelData.Channel.ID != "" {
			metadata["teams_channel_id"] = activity.ChannelData.Channel.ID
		}
		if activity.ChannelData.TeamsTeamID != "" {
			metadata["teams_team_id"] = activity.ChannelData.TeamsTeamID
		} else if activity.ChannelData.Team != nil && activity.ChannelData.Team.ID != "" {
			metadata["teams_team_id"] = activity.ChannelData.Team.ID
		}
		if activity.ChannelData.Tenant != nil && activity.ChannelData.Tenant.ID != "" {
			metadata["teams_tenant_id"] = activity.ChannelData.Tenant.ID
		}
	}

	timestamp := activity.Timestamp
	if timestamp == "" {
		timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	senderID := activity.From.AadObjectID
	if senderID == "" {
		senderID = activity.From.ID
	}

	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: timestamp,
		Sender:    activity.From.Name,
		SenderID:  senderID,
		Msg:       text,
		Type:      "chat",
		Channel:   "", // Resolved later via channel links (Phase 3).
		ThreadID:  resolveThreadID(activity),
		Metadata:  metadata,
	}
}

// resolveThreadID determines the thread ID for the message.
// In Teams, reply chains use replyToId. If present, use the root
// activity ID as the thread ID.
func resolveThreadID(activity *Activity) string {
	if activity.ReplyToID != "" {
		return activity.ReplyToID
	}
	return ""
}

// stripBotMention removes the bot's @-mention from the message text.
// Teams wraps mentions in <at>...</at> tags.
func stripBotMention(text string, botID string) string {
	if text == "" || botID == "" {
		return text
	}

	// Teams mentions look like: <at>BotName</at>
	// We need to find and remove the mention for our bot.
	// The bot mention entity will have mentioned.id == botID.
	// We strip all <at>...</at> tags that might be the bot mention.
	// A more precise approach uses the Entities field, but for the
	// text-level strip we remove by pattern.
	cleaned := atMentionRegex.ReplaceAllString(text, "")
	return strings.TrimSpace(cleaned)
}

// stripBotMentionByEntity removes only the bot's @-mention from text,
// using the Activity's entities to identify which mention belongs to the bot.
func stripBotMentionByEntity(text string, botID string, entities []Entity) string {
	if text == "" || botID == "" {
		return text
	}

	for _, e := range entities {
		if e.Type == "mention" && e.Mentioned.ID == botID && e.Text != "" {
			text = strings.Replace(text, e.Text, "", 1)
		}
	}

	return strings.TrimSpace(text)
}

// handleConversationUpdate logs member added/removed events.
// In Phase 1, this is informational only.
func handleConversationUpdate(activity *Activity, log *slog.Logger) {
	for _, member := range activity.MembersAdded {
		log.Info("Member added to conversation",
			"member_id", member.ID,
			"member_name", member.Name,
			"conversation_id", activity.Conversation.ID,
		)
	}
	for _, member := range activity.MembersRemoved {
		log.Info("Member removed from conversation",
			"member_id", member.ID,
			"member_name", member.Name,
			"conversation_id", activity.Conversation.ID,
		)
	}
}
