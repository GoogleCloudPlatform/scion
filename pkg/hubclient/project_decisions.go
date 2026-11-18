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
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Tabs of GET /api/v1/projects/{id}/decisions.
const (
	ProjectDecisionsTabPending = "pending"
	ProjectDecisionsTabMade    = "made"
)

// ProjectDecisionService reads a project's Decisions as a user
// (GET /api/v1/projects/{id}/decisions).
type ProjectDecisionService interface {
	// List returns one page of the Waiting on you (tab=pending) or Made
	// (tab=made) list with its facets and counts.
	List(ctx context.Context, opts *ListProjectDecisionsOptions) (*ProjectDecisionsResponse, error)
}

type projectDecisionService struct {
	c         *client
	projectID string
}

// ListProjectDecisionsOptions are the query parameters of a project
// decisions request. Empty fields are left out; Limit is sent when > 0.
type ListProjectDecisionsOptions struct {
	Tab          string // pending (default) or made
	Cursor       string
	Limit        int
	Query        string // q: substring search, 2 to 256 runes
	Conversation string // a conversation chat key
	AgentID      string // asking agent UUID
	// Made only.
	UserID     string // answered_by user ID
	Provenance string // direct, interpreted, confirmed or overridden
	From       string // YYYY-MM-DD or RFC3339, answer time
	To         string // YYYY-MM-DD or RFC3339, answer time
}

// ProjectDecisionCounts are the live pending counts.
type ProjectDecisionCounts struct {
	WaitingOnYou    int `json:"waitingOnYou"`
	WaitingOnOthers int `json:"waitingOnOthers"`
}

// ProjectDecisionActor is an agents or answerers facet option. Hint tells
// apart agents whose names collide; ProjectID is populated for asking agents.
type ProjectDecisionActor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Hint      string `json:"hint,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
}

// ProjectDecisionConversation is a conversations facet option.
type ProjectDecisionConversation struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	ProjectID string `json:"projectId"`
}

// ProjectDecisionProjectRef names the project of a prompt or entry.
type ProjectDecisionProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// ProjectDecisionPrompt is one prompt card on tab=pending.
type ProjectDecisionPrompt struct {
	Project               ProjectDecisionProjectRef `json:"project"`
	PromptMessageID       string                    `json:"promptMessageId"`
	PromptAvailable       bool                      `json:"promptAvailable"`
	ChatKey               string                    `json:"chatKey"`
	ConversationLabel     string                    `json:"conversationLabel"`
	ConversationAvailable bool                      `json:"conversationAvailable"`
	AskerAgentID          string                    `json:"askerAgentId"`
	AskerAgentName        string                    `json:"askerAgentName"`
	CreatedAt             time.Time                 `json:"createdAt"`
	AnswerMode            string                    `json:"answerMode"`
	WaitingOnYou          bool                      `json:"waitingOnYou"`
	CanAnswer             bool                      `json:"canAnswer"`
	DisabledReason        string                    `json:"disabledReason,omitempty"`
	WaitingReasons        map[string]string         `json:"waitingReasons,omitempty"`
	Decisions             []DecisionView            `json:"decisions"`
}

// ProjectDecisionSummary is the decision of one tab=made log entry.
type ProjectDecisionSummary struct {
	RowID           string `json:"rowId"`
	PromptMessageID string `json:"promptMessageId"`
	PromptAvailable bool   `json:"promptAvailable"`
	Key             string `json:"key"`
	Ref             string `json:"ref"`
	Version         int    `json:"version"`
	Question        string `json:"question"`
	AskerAgentID    string `json:"askerAgentId"`
	AskerAgentName  string `json:"askerAgentName"`
}

// ProjectDecisionLogEntry is one answer on tab=made.
type ProjectDecisionLogEntry struct {
	Project                ProjectDecisionProjectRef `json:"project"`
	AnswerID               string                    `json:"answerId"`
	Decision               ProjectDecisionSummary    `json:"decision"`
	ChoiceID               string                    `json:"choiceId"`
	Label                  string                    `json:"label"`
	MatchedDefault         bool                      `json:"matchedDefault"`
	PreviousChoiceLabel    string                    `json:"previousChoiceLabel,omitempty"`
	AnsweredByUserID       string                    `json:"answeredByUserId,omitempty"`
	AnsweredByName         string                    `json:"answeredByName"`
	Provenance             string                    `json:"provenance"`
	Outcome                string                    `json:"outcome"`
	DismissReason          string                    `json:"dismissReason,omitempty"`
	Surface                string                    `json:"surface"`
	InterpretedByAgentID   string                    `json:"interpretedByAgentId,omitempty"`
	InterpretedByAgentName string                    `json:"interpretedByAgentName,omitempty"`
	SourceMessageID        string                    `json:"sourceMessageId,omitempty"`
	SourceMessageAvailable bool                      `json:"sourceMessageAvailable"`
	ResponseMessageID      string                    `json:"responseMessageId,omitempty"`
	BatchID                string                    `json:"batchId,omitempty"`
	At                     time.Time                 `json:"at"`
	ChatKey                string                    `json:"chatKey"`
	ConversationLabel      string                    `json:"conversationLabel"`
	ConversationAvailable  bool                      `json:"conversationAvailable"`
}

// ProjectDecisionsResponse is either tab's response: Prompts on pending,
// Entries on made. Conversations is nil when the hub predates decision
// search (it sends no conversations facet); FilteredCounts is set on
// pending while a filter applies.
type ProjectDecisionsResponse struct {
	Counts         ProjectDecisionCounts         `json:"counts"`
	FilteredCounts *ProjectDecisionCounts        `json:"filteredCounts,omitempty"`
	Prompts        []ProjectDecisionPrompt       `json:"prompts,omitempty"`
	Entries        []ProjectDecisionLogEntry     `json:"entries,omitempty"`
	Agents         []ProjectDecisionActor        `json:"agents"`
	Answerers      []ProjectDecisionActor        `json:"answerers"`
	Conversations  []ProjectDecisionConversation `json:"conversations"`
	NextCursor     string                        `json:"nextCursor,omitempty"`
}

// SupportsSearch reports whether the hub that answered supports decision
// search and the conversation filter.
func (r *ProjectDecisionsResponse) SupportsSearch() bool {
	return r != nil && r.Conversations != nil
}

// ProjectDecisions returns the Decisions service of projectID.
func (c *client) ProjectDecisions(projectID string) ProjectDecisionService {
	return &projectDecisionService{c: c, projectID: projectID}
}

func (s *projectDecisionService) List(ctx context.Context, opts *ListProjectDecisionsOptions) (*ProjectDecisionsResponse, error) {
	query := url.Values{}
	if opts != nil {
		set := func(k, v string) {
			if v != "" {
				query.Set(k, v)
			}
		}
		set("tab", opts.Tab)
		set("cursor", opts.Cursor)
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		set("q", opts.Query)
		set("conversation", opts.Conversation)
		set("agent", opts.AgentID)
		set("user", opts.UserID)
		set("provenance", opts.Provenance)
		set("from", opts.From)
		set("to", opts.To)
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/projects/"+url.PathEscape(s.projectID)+"/decisions", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ProjectDecisionsResponse](resp)
}
