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
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// DecisionService provides agent self-service operations on choice decisions.
type DecisionService interface {
	// List returns one page of decisions belonging to the calling agent.
	List(ctx context.Context, opts *ListAgentDecisionsOptions) (*AgentDecisionListResponse, error)

	// Show returns all versions and answer rows of a decision key belonging to
	// the calling agent.
	Show(ctx context.Context, key string) (*AgentDecisionShowResponse, error)

	// RecordInterpreted atomically records 1..10 interpreted answers from a user
	// free-text reply message.
	RecordInterpreted(ctx context.Context, req *RecordInterpretedRequest) (*RecordInterpretedResponse, error)
}

type decisionService struct {
	c *client
}

// ListAgentDecisionsOptions configures GET /api/v1/agent/decisions.
type ListAgentDecisionsOptions struct {
	State  string // pending (default), decided, or all
	Cursor string
	Limit  int
}

// DecisionAnswerView is one recorded answer on a decision version.
type DecisionAnswerView struct {
	ChoiceID             string    `json:"choiceId"`
	Label                string    `json:"label"`
	AnsweredByUserID     string    `json:"answeredByUserId,omitempty"`
	AnsweredByName       string    `json:"answeredByName,omitempty"`
	AnsweredByEmail      string    `json:"answeredByEmail,omitempty"`
	Provenance           string    `json:"provenance"`
	Outcome              string    `json:"outcome,omitempty"`
	InterpretedByAgentID string    `json:"interpretedByAgentId,omitempty"`
	SourceMessageID      string    `json:"sourceMessageId,omitempty"`
	Surface              string    `json:"surface"`
	ResponseMessageID    string    `json:"responseMessageId"`
	BatchID              string    `json:"batchId,omitempty"`
	MatchedDefault       bool      `json:"matchedDefault,omitempty"`
	At                   time.Time `json:"at"`
}

// DecisionReplacedByView names the version that replaced a decision.
type DecisionReplacedByView struct {
	RowID     string    `json:"rowId"`
	MessageID string    `json:"messageId"`
	Version   int       `json:"version"`
	At        time.Time `json:"at"`
}

// DecisionView is the full view of one decision version returned by record/answer endpoints.
type DecisionView struct {
	RowID           string                  `json:"rowId"`
	MessageID       string                  `json:"messageId"`
	Key             string                  `json:"key"`
	Ref             string                  `json:"ref"`
	Version         int                     `json:"version"`
	Status          string                  `json:"status"`
	AskerAgentID    string                  `json:"askerAgentId"`
	AskerAgentName  string                  `json:"askerAgentName"`
	Question        string                  `json:"question"`
	DefaultChoiceID string                  `json:"defaultChoiceId,omitempty"`
	Choices         []messages.Choice       `json:"choices"`
	ReplacedBy      *DecisionReplacedByView `json:"replacedBy,omitempty"`
	Decision        *DecisionAnswerView     `json:"decision,omitempty"`
}

// AgentDecisionListItem is one decision version in AgentDecisionListResponse.
type AgentDecisionListItem struct {
	RowID           string              `json:"rowId"`
	Key             string              `json:"key"`
	Ref             string              `json:"ref"`
	Version         int                 `json:"version"`
	Status          string              `json:"status"`
	Question        string              `json:"question"`
	ConversationID  string              `json:"conversationId"`
	PromptMessageID string              `json:"promptMessageId"`
	CreatedAt       time.Time           `json:"createdAt"`
	DecidedAt       *time.Time          `json:"decidedAt,omitempty"`
	Answer          *DecisionAnswerView `json:"answer,omitempty"`
}

// AgentDecisionListResponse is the response from GET /api/v1/agent/decisions.
type AgentDecisionListResponse struct {
	Items      []AgentDecisionListItem `json:"items"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

// AgentDecisionVersionView is one version in AgentDecisionShowResponse.
type AgentDecisionVersionView struct {
	RowID           string                  `json:"rowId"`
	Key             string                  `json:"key"`
	Ref             string                  `json:"ref"`
	Version         int                     `json:"version"`
	Status          string                  `json:"status"`
	Question        string                  `json:"question"`
	ConversationID  string                  `json:"conversationId"`
	PromptMessageID string                  `json:"promptMessageId"`
	Choices         []messages.Choice       `json:"choices"`
	DefaultChoiceID string                  `json:"defaultChoiceId,omitempty"`
	ReplacedBy      *DecisionReplacedByView `json:"replacedBy,omitempty"`
	CreatedAt       time.Time               `json:"createdAt"`
	DecidedAt       *time.Time              `json:"decidedAt,omitempty"`
	Answers         []DecisionAnswerView    `json:"answers"`
}

// AgentDecisionShowResponse is the response from GET /api/v1/agent/decisions/{key}.
type AgentDecisionShowResponse struct {
	Key      string                     `json:"key"`
	Ref      string                     `json:"ref"`
	Versions []AgentDecisionVersionView `json:"versions"`
}

// InterpretedAnswerInput is one key=choice_id pair in RecordInterpretedRequest.
type InterpretedAnswerInput struct {
	Key      string `json:"key"`
	ChoiceID string `json:"choice_id"`
}

// RecordInterpretedRequest is the body of POST /api/v1/agent/decisions/interpreted.
type RecordInterpretedRequest struct {
	SourceMessageID string                   `json:"source_message_id"`
	IdempotencyKey  string                   `json:"idempotency_key,omitempty"`
	Answers         []InterpretedAnswerInput `json:"answers"`
}

// RecordInterpretedResponse is the response from POST /api/v1/agent/decisions/interpreted.
type RecordInterpretedResponse struct {
	ResponseMessageID string         `json:"response_message_id"`
	BatchID           string         `json:"batch_id,omitempty"`
	Decisions         []DecisionView `json:"decisions"`
	Replayed          bool           `json:"replayed,omitempty"`
}

func (s *decisionService) List(ctx context.Context, opts *ListAgentDecisionsOptions) (*AgentDecisionListResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.State != "" {
			query.Set("state", opts.State)
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/agent/decisions", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AgentDecisionListResponse](resp)
}

func (s *decisionService) Show(ctx context.Context, key string) (*AgentDecisionShowResponse, error) {
	resp, err := s.c.get(ctx, "/api/v1/agent/decisions/"+url.PathEscape(key), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[AgentDecisionShowResponse](resp)
}

func (s *decisionService) RecordInterpreted(ctx context.Context, req *RecordInterpretedRequest) (*RecordInterpretedResponse, error) {
	resp, err := s.c.post(ctx, "/api/v1/agent/decisions/interpreted", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[RecordInterpretedResponse](resp)
}
