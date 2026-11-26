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

package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

const (
	inboxSummaryPath = "/api/v1/users/me/inbox/summary"

	// InboxSummaryDefaultPreviews and InboxSummaryMaxPreviews mirror the hub's
	// bounds for the ?previews= query parameter.
	InboxSummaryDefaultPreviews = 3
	InboxSummaryMaxPreviews     = 10

	// InboxCategory* are the stable keys returned in InboxSummaryResponse.Categories.
	InboxCategoryDecisions      = "decisions"
	InboxCategoryAgentSecrets   = "agent_secrets"
	InboxCategoryWaiting        = "waiting"
	InboxCategoryReviews        = "reviews"
	InboxCategoryPendingSecrets = "pending_secrets"
	InboxCategoryChat           = "chat"

	// InboxBreakdown* are the documented stable keys for InboxCategory.Breakdown.
	InboxBreakdownAgents              = "agents"
	InboxBreakdownProjects            = "projects"
	InboxBreakdownSecretsProjects     = "secretsProjects"
	InboxBreakdownToAddress           = "toAddress"
	InboxBreakdownRequested           = "requested"
	InboxBreakdownUnreadConversations = "unreadConversations"
	InboxBreakdownDMs                 = "dms"
	InboxBreakdownMentions            = "mentions"

	// InboxState* are the valid values for InboxCategory.State.
	InboxStateOK          = "ok"
	InboxStateDisabled    = "disabled"
	InboxStateUnavailable = "unavailable"
	InboxStateError       = "error"

	// InboxError* are the valid values for InboxCategory.ErrorCode when State == "error".
	InboxErrorTimeout  = "timeout"
	InboxErrorInternal = "internal"

	// InboxChatFilter* are the valid values for the ?chat= query parameter.
	InboxChatFilterAll      = "all"
	InboxChatFilterDMs      = "dms"
	InboxChatFilterMentions = "mentions"

	// InboxPendingSecretKind is the kind value for pending user-scope secrets.
	InboxPendingSecretKind = "pending_secret"
)

// InboxSummaryOptions configures GET /api/v1/users/me/inbox/summary.
type InboxSummaryOptions struct {
	// Previews caps the number of previews returned per category (0..10).
	// When nil, the hub default (3) applies; a pointer to 0 requests counts only.
	Previews *int

	// Chat selects which unread conversations populate chat previews:
	// "all" (default), "dms", or "mentions".
	Chat string
}

// InboxSummaryResponse is the response of GET /api/v1/users/me/inbox/summary.
// It mirrors hub.InboxSummaryResponse.
type InboxSummaryResponse struct {
	GeneratedAt string                   `json:"generatedAt"`
	Total       int                      `json:"total"`
	TotalCapped bool                     `json:"totalCapped"`
	Categories  map[string]InboxCategory `json:"categories"`

	// Raw is the response body exactly as the hub sent it, so callers such as
	// `scion inbox --format json` can pass the response through verbatim
	// without dropping fields from newer hub versions.
	Raw json.RawMessage `json:"-"`
}

// InboxCategory is the common summary envelope for one inbox category.
// It mirrors hub.InboxCategory.
type InboxCategory struct {
	State             string            `json:"state"`
	Count             int               `json:"count"`
	Capped            bool              `json:"capped"`
	CountsTowardTotal bool              `json:"countsTowardTotal"`
	Href              string            `json:"href"`
	ErrorCode         string            `json:"errorCode,omitempty"`
	Previews          []json.RawMessage `json:"previews"`
	AgentIDs          []string          `json:"agentIds,omitempty"`
	Breakdown         map[string]int    `json:"breakdown,omitempty"`
	NextExpiresAt     *time.Time        `json:"nextExpiresAt,omitempty"`
}

// InboxDecisionInlineChoice is one selectable option for an inline decision preview.
type InboxDecisionInlineChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// InboxDecisionInline carries inline-triage metadata for a single-decision preview.
type InboxDecisionInline struct {
	DecisionRowID string                      `json:"decisionRowId"`
	Choices       []InboxDecisionInlineChoice `json:"choices"`
	AllowsOther   bool                        `json:"allowsOther"`
}

// InboxDecisionPreview is one preview item in the "decisions" category.
type InboxDecisionPreview struct {
	ID                string               `json:"id"`
	ProjectID         string               `json:"projectId"`
	ProjectName       string               `json:"projectName"`
	CreatedAt         time.Time            `json:"createdAt"`
	Href              string               `json:"href"`
	PromptMessageID   string               `json:"promptMessageId"`
	ConversationLabel string               `json:"conversationLabel"`
	AskerAgentName    string               `json:"askerAgentName"`
	Question          string               `json:"question"`
	DecisionCount     int                  `json:"decisionCount"`
	AnswerMode        string               `json:"answerMode"`
	CanAnswer         bool                 `json:"canAnswer"`
	DisabledReason    string               `json:"disabledReason,omitempty"`
	Inline            *InboxDecisionInline `json:"inline,omitempty"`
}

// InboxWaitingAgentPreview is one preview item in the "waiting" category.
type InboxWaitingAgentPreview struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	CreatedAt   time.Time `json:"createdAt"`
	Href        string    `json:"href"`
	AgentID     string    `json:"agentId"`
	AgentName   string    `json:"agentName"`
	AgentSlug   string    `json:"agentSlug"`
	Question    string    `json:"question"`
	Since       time.Time `json:"since"`
}

// InboxReviewPreview is one preview item in the "reviews" category.
type InboxReviewPreview struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"projectId"`
	ProjectName    string    `json:"projectName"`
	CreatedAt      time.Time `json:"createdAt"`
	Href           string    `json:"href"`
	Kind           string    `json:"kind"`
	ArtifactID     string    `json:"artifactId"`
	Title          string    `json:"title"`
	ScopeRef       string    `json:"scopeRef"`
	RequestedBy    string    `json:"requestedBy,omitempty"`
	NotificationID string    `json:"notificationId,omitempty"`
}

// InboxPendingSecretPreview is one preview item in the "pending_secrets" category.
// Never contains secret values or ValueRef. It mirrors hub.InboxPendingSecretPreview.
type InboxPendingSecretPreview struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	ProjectID        string    `json:"projectId"`
	ProjectName      string    `json:"projectName"`
	CreatedAt        time.Time `json:"createdAt"`
	Href             string    `json:"href"`
	Key              string    `json:"key"`
	AgentSlug        string    `json:"agentSlug"`
	InjectionMode    string    `json:"injectionMode,omitempty"`
	ReplacesExisting bool      `json:"replacesExisting"`
	AllowProgeny     bool      `json:"allowProgeny"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// InboxChatPreview is one preview item in the "chat" category.
// Never contains message bodies.
type InboxChatPreview struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"projectId"`
	ProjectName     string    `json:"projectName"`
	CreatedAt       time.Time `json:"createdAt"`
	Href            string    `json:"href"`
	Kind            string    `json:"kind"`
	ConversationKey string    `json:"conversationKey"`
	Label           string    `json:"label"`
	LastMessageAt   time.Time `json:"lastMessageAt"`
}

// InboxSummary fetches GET /api/v1/users/me/inbox/summary.
func (s *userService) InboxSummary(ctx context.Context, opts *InboxSummaryOptions) (*InboxSummaryResponse, error) {
	var query url.Values
	if opts != nil {
		if opts.Previews != nil || opts.Chat != "" {
			query = url.Values{}
			if opts.Previews != nil {
				query.Set("previews", strconv.Itoa(*opts.Previews))
			}
			if opts.Chat != "" {
				query.Set("chat", opts.Chat)
			}
		}
	}

	resp, err := s.c.getWithQuery(ctx, inboxSummaryPath, query, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeRequired[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	if raw == nil || bytes.Equal(bytes.TrimSpace(*raw), []byte("null")) {
		return nil, errors.New("empty or null inbox summary response")
	}
	var summary InboxSummaryResponse
	if err := json.Unmarshal(*raw, &summary); err != nil {
		return nil, fmt.Errorf("failed to decode inbox summary: %w", err)
	}
	summary.Raw = *raw
	return &summary, nil
}
