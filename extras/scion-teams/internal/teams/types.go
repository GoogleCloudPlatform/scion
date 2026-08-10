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

import "encoding/json"

// Activity represents a Bot Framework Activity received from Microsoft Teams.
// See https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-activity
type Activity struct {
	Type           string              `json:"type"`
	ID             string              `json:"id"`
	Timestamp      string              `json:"timestamp,omitempty"`
	LocalTimestamp string              `json:"localTimestamp,omitempty"`
	ServiceURL     string              `json:"serviceUrl,omitempty"`
	ChannelID      string              `json:"channelId,omitempty"`
	From           ChannelAccount      `json:"from"`
	Recipient      ChannelAccount      `json:"recipient"`
	Conversation   ConversationAccount `json:"conversation"`
	Text           string              `json:"text,omitempty"`
	TextFormat     string              `json:"textFormat,omitempty"`
	Locale         string              `json:"locale,omitempty"`
	Entities       []Entity            `json:"entities,omitempty"`
	Value          json.RawMessage     `json:"value,omitempty"`
	ReplyToID      string              `json:"replyToId,omitempty"`
	ChannelData    *ChannelData        `json:"channelData,omitempty"`
	Attachments    []Attachment        `json:"attachments,omitempty"`
	Name           string              `json:"name,omitempty"`
	MembersAdded   []ChannelAccount    `json:"membersAdded,omitempty"`
	MembersRemoved []ChannelAccount    `json:"membersRemoved,omitempty"`
	Action         string              `json:"action,omitempty"`
}

// ChannelAccount identifies a user or bot in a channel.
type ChannelAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AadObjectID string `json:"aadObjectId,omitempty"`
	Role        string `json:"role,omitempty"`
}

// ConversationAccount identifies a conversation.
type ConversationAccount struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	IsGroup          bool   `json:"isGroup,omitempty"`
	ConversationType string `json:"conversationType,omitempty"`
	TenantID         string `json:"tenantId,omitempty"`
}

// Entity represents an entity mentioned in the Activity (e.g., @mentions).
type Entity struct {
	Type      string         `json:"type"`
	Mentioned ChannelAccount `json:"mentioned,omitempty"`
	Text      string         `json:"text,omitempty"`
}

// ChannelData contains Teams-specific channel data.
type ChannelData struct {
	TeamsChannelID string          `json:"teamsChannelId,omitempty"`
	TeamsTeamID    string          `json:"teamsTeamId,omitempty"`
	Channel        *TeamsChannelID `json:"channel,omitempty"`
	Team           *TeamsTeamID    `json:"team,omitempty"`
	Tenant         *TenantInfo     `json:"tenant,omitempty"`
}

// TeamsChannelID holds the Teams channel identifier from channelData.
type TeamsChannelID struct {
	ID string `json:"id,omitempty"`
}

// TeamsTeamID holds the Teams team identifier from channelData.
type TeamsTeamID struct {
	ID string `json:"id,omitempty"`
}

// TenantInfo holds Azure AD tenant information.
type TenantInfo struct {
	ID string `json:"id,omitempty"`
}

// Attachment represents a file or card attachment in an Activity.
type Attachment struct {
	ContentType string          `json:"contentType,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
	ContentURL  string          `json:"contentUrl,omitempty"`
	Name        string          `json:"name,omitempty"`
}

// InvokeResponse is the response body for invoke-type Activities.
type InvokeResponse struct {
	Status int         `json:"status"`
	Body   interface{} `json:"body,omitempty"`
}

// ConversationReference stores the information needed to send a proactive
// message to a conversation. Upserted on every inbound activity.
type ConversationReference struct {
	ServiceURL     string
	ConversationID string
	ChannelID      string
	TenantID       string
	BotID          string
}
