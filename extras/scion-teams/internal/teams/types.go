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
	"time"
)

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

// ChannelLink maps a Teams conversation to a Scion project.
type ChannelLink struct {
	ConversationID string
	TeamID         string
	TeamName       string
	ChannelName    string
	ProjectID      string
	ProjectSlug    string
	DefaultAgent   string
	Active         bool
}

// ConversationContext tracks the last conversation context for a
// user+project+agent tuple, used for outbound message routing.
type ConversationContext struct {
	TeamsUserID        string
	ProjectID          string
	AgentSlug          string
	LastConversationID string
	LastActivityID     string
	LastMessageAt      time.Time
}

// -------------------------------------------------------------------
// Adaptive Card types — minimal builder types that marshal to the
// Adaptive Card JSON schema (version 1.5).
// -------------------------------------------------------------------

// AdaptiveCard is the top-level Adaptive Card structure.
type AdaptiveCard struct {
	Type    string        `json:"type"`              // always "AdaptiveCard"
	Schema  string        `json:"$schema,omitempty"` // "http://adaptivecards.io/schemas/adaptive-card.json"
	Version string        `json:"version"`           // "1.5"
	Body    []CardElement `json:"body,omitempty"`
	Actions []CardAction  `json:"actions,omitempty"`
}

// NewAdaptiveCard creates a new AdaptiveCard with default type/version.
func NewAdaptiveCard() *AdaptiveCard {
	return &AdaptiveCard{
		Type:    "AdaptiveCard",
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Version: "1.5",
	}
}

// CardElement is implemented by all Adaptive Card body elements.
type CardElement interface {
	cardElement()
}

// CardAction is implemented by all Adaptive Card action types.
type CardAction interface {
	cardAction()
}

// TextBlock displays text in an Adaptive Card.
type TextBlock struct {
	Type     string `json:"type"`                // "TextBlock"
	Text     string `json:"text"`
	Weight   string `json:"weight,omitempty"`    // "Bolder"
	Size     string `json:"size,omitempty"`      // "Small", "Medium", "Large"
	Color    string `json:"color,omitempty"`     // "Accent", "Good", "Warning", "Attention"
	Wrap     bool   `json:"wrap,omitempty"`
	IsSubtle bool   `json:"isSubtle,omitempty"`
}

func (TextBlock) cardElement() {}

// ColumnSet is a container with horizontal columns.
type ColumnSet struct {
	Type    string   `json:"type"` // "ColumnSet"
	Columns []Column `json:"columns"`
}

func (ColumnSet) cardElement() {}

// Column is a vertical container within a ColumnSet.
type Column struct {
	Type  string        `json:"type"`            // "Column"
	Width string        `json:"width,omitempty"` // "auto", "stretch", or a number
	Items []CardElement `json:"items,omitempty"`
}

// Image displays an image in an Adaptive Card.
type Image struct {
	Type string `json:"type"`            // "Image"
	URL  string `json:"url"`
	Size string `json:"size,omitempty"`  // "Small", "Medium", "Large"
	Alt  string `json:"altText,omitempty"`
}

func (Image) cardElement() {}

// Container groups elements together.
type Container struct {
	Type  string        `json:"type"` // "Container"
	Items []CardElement `json:"items,omitempty"`
}

func (Container) cardElement() {}

// ActionSubmit is a button that submits data back to the bot.
type ActionSubmit struct {
	Type  string      `json:"type"`            // "Action.Submit"
	Title string      `json:"title"`
	Style string      `json:"style,omitempty"` // "positive", "destructive"
	Data  interface{} `json:"data"`
}

func (ActionSubmit) cardAction() {}

// ActionOpenURL opens a URL when clicked.
type ActionOpenURL struct {
	Type  string `json:"type"`  // "Action.OpenUrl"
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (ActionOpenURL) cardAction() {}

// ActivityResponse is the JSON response from the Bot Connector REST API
// when creating or updating an activity.
type ActivityResponse struct {
	ID string `json:"id"`
}
