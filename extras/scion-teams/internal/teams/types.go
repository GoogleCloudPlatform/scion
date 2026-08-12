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
	ConversationID   string
	ServiceURL       string
	BotID            string
	BotName          string
	TenantID         string
	ConversationType string // "personal", "groupChat", or "channel"
	TeamID           string
	ChannelID        string
	UpdatedAt        time.Time
}

// ChannelLink maps a Teams conversation to a Scion project.
type ChannelLink struct {
	ConversationID     string
	TeamID             string
	TeamName           string
	ChannelName        string
	ProjectID          string
	ProjectSlug        string
	DefaultAgent       string
	LinkedBy           string // Azure AD object ID of user who ran setup
	LinkedAt           time.Time
	Active             bool
	ShowAgentToAgent   bool
	ShowAssistantReply bool
	ShowStateChanges   bool
	ChatOnly           bool
}

// TeamsUserMapping links a Teams user to a Scion user identity.
type TeamsUserMapping struct {
	TeamsUserID      string
	TeamsDisplayName string
	ScionUserID      string
	ScionEmail       string
	LinkedAt         time.Time
	AutoLinked       bool
}

// ProjectAgents caches the list of agents for a project.
type ProjectAgents struct {
	ProjectID   string
	AgentSlugs  []string
	RefreshedAt time.Time
}

// PendingAskUser represents an ask-user callback awaiting a Teams user response.
type PendingAskUser struct {
	RequestID      string
	ActivityID     string // Teams activity ID of the prompt message
	ConversationID string
	AgentSlug      string
	ProjectID      string
	Choices        []string
	ExpiresAt      time.Time
	Responded      bool
}

// CallbackLookup maps a short callback ID to its full data payload.
type CallbackLookup struct {
	ShortID   string
	FullData  string
	ExpiresAt time.Time
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
	Type                string `json:"type"` // "TextBlock"
	Text                string `json:"text"`
	Weight              string `json:"weight,omitempty"` // "Bolder"
	Size                string `json:"size,omitempty"`   // "Small", "Medium", "Large", "ExtraLarge"
	Color               string `json:"color,omitempty"`  // "Accent", "Good", "Warning", "Attention"
	Wrap                bool   `json:"wrap,omitempty"`
	IsSubtle            bool   `json:"isSubtle,omitempty"`
	HorizontalAlignment string `json:"horizontalAlignment,omitempty"` // "Left", "Center", "Right"
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
	Type string `json:"type"` // "Image"
	URL  string `json:"url"`
	Size string `json:"size,omitempty"` // "Small", "Medium", "Large"
	Alt  string `json:"altText,omitempty"`
}

func (Image) cardElement() {}

// Container groups elements together.
type Container struct {
	Type  string        `json:"type"` // "Container"
	Items []CardElement `json:"items,omitempty"`
}

func (Container) cardElement() {}

// InputText is an Adaptive Card text input field.
type InputText struct {
	Type        string `json:"type"` // "Input.Text"
	ID          string `json:"id"`
	IsMultiline bool   `json:"isMultiline,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

func (InputText) cardElement() {}

// ActionSubmit is a button that submits data back to the bot.
type ActionSubmit struct {
	Type  string      `json:"type"` // "Action.Submit"
	Title string      `json:"title"`
	Style string      `json:"style,omitempty"` // "positive", "destructive"
	Data  interface{} `json:"data"`
}

func (ActionSubmit) cardAction() {}

// ActionExecute is a button that sends an invoke activity to the bot.
// Unlike Action.Submit (which sends a message activity), Action.Execute
// sends an invoke with name "adaptiveCard/action", allowing the bot to
// return an updated card.
type ActionExecute struct {
	Type  string      `json:"type"` // "Action.Execute"
	Title string      `json:"title,omitempty"`
	Verb  string      `json:"verb,omitempty"`
	Data  interface{} `json:"data,omitempty"`
	Style string      `json:"style,omitempty"`
}

func (ActionExecute) cardAction() {}

// ActionOpenURL opens a URL when clicked.
type ActionOpenURL struct {
	Type  string `json:"type"` // "Action.OpenUrl"
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (ActionOpenURL) cardAction() {}

// ActivityResponse is the JSON response from the Bot Connector REST API
// when creating or updating an activity.
type ActivityResponse struct {
	ID string `json:"id"`
}
