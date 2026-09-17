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

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ConversationService provides operations on conversations.
type ConversationService interface {
	// List returns conversations the caller participates in.
	List(ctx context.Context, opts *ListConversationsOptions) (*ConversationListResult, error)

	// Get returns a single conversation with participants.
	Get(ctx context.Context, id string) (*ConversationDetail, error)

	// ListMessages returns messages in a conversation.
	ListMessages(ctx context.Context, conversationID string, opts *ConversationMessagesOptions) (*store.ListResult[store.Message], error)

	// Create creates a new conversation.
	Create(ctx context.Context, req *CreateConversationRequest) (*ConversationDetail, error)

	// SetDefaultAgent sets the default agent for a conversation.
	SetDefaultAgent(ctx context.Context, conversationID, agentID string) error

	// AddParticipant adds a participant to a conversation.
	AddParticipant(ctx context.Context, conversationID string, req *AddParticipantRequest) (*store.ConversationParticipant, error)

	// Leave removes the caller from a conversation.
	Leave(ctx context.Context, conversationID string) error
}

// conversationService is the implementation of ConversationService.
type conversationService struct {
	c *client
}

// ListConversationsOptions configures conversation listing.
type ListConversationsOptions struct {
	Kind      string
	Surface   string
	ProjectID string
	Limit     int
}

// ConversationMessagesOptions configures message listing within a conversation.
type ConversationMessagesOptions struct {
	Limit  int
	Cursor string
	Before string // RFC3339 time filter
	After  string // RFC3339 time filter
}

// CreateConversationRequest is the request to create a new conversation.
type CreateConversationRequest struct {
	DisplayName string `json:"displayName"`
	ProjectID   string `json:"projectId,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

// ConversationDetail wraps a conversation with its participants.
type ConversationDetail struct {
	store.Conversation
	Participants []store.ConversationParticipant `json:"participants,omitempty"`
}

// AddParticipantRequest is the request to add a participant to a conversation.
type AddParticipantRequest struct {
	PrincipalKind string `json:"principalKind"`
	PrincipalID   string `json:"principalId"`
}

// ConversationListResult is the response from listing conversations.
type ConversationListResult struct {
	Conversations []ConversationDetail `json:"conversations"`
	Cursor        string               `json:"cursor,omitempty"`
}

// List returns conversations the caller participates in.
func (s *conversationService) List(ctx context.Context, opts *ListConversationsOptions) (*ConversationListResult, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Kind != "" {
			query.Set("kind", opts.Kind)
		}
		if opts.Surface != "" {
			query.Set("surface", opts.Surface)
		}
		if opts.ProjectID != "" {
			query.Set("project_id", opts.ProjectID)
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/conversations", query, nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[ConversationListResult](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &ConversationListResult{Conversations: []ConversationDetail{}}, nil
	}
	if result.Conversations == nil {
		result.Conversations = []ConversationDetail{}
	}
	return result, nil
}

// Get returns a single conversation with participants.
func (s *conversationService) Get(ctx context.Context, id string) (*ConversationDetail, error) {
	resp, err := s.c.get(ctx, "/api/v1/conversations/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ConversationDetail](resp)
}

// ListMessages returns messages in a conversation.
func (s *conversationService) ListMessages(ctx context.Context, conversationID string, opts *ConversationMessagesOptions) (*store.ListResult[store.Message], error) {
	query := url.Values{}
	if opts != nil {
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
		if opts.Before != "" {
			query.Set("before", opts.Before)
		}
		if opts.After != "" {
			query.Set("after", opts.After)
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/conversations/"+url.PathEscape(conversationID)+"/messages", query, nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[store.ListResult[store.Message]](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &store.ListResult[store.Message]{Items: []store.Message{}}, nil
	}
	if result.Items == nil {
		result.Items = []store.Message{}
	}
	return result, nil
}

// Create creates a new conversation.
func (s *conversationService) Create(ctx context.Context, req *CreateConversationRequest) (*ConversationDetail, error) {
	resp, err := s.c.post(ctx, "/api/v1/conversations", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ConversationDetail](resp)
}

// SetDefaultAgent sets the default agent for a conversation.
func (s *conversationService) SetDefaultAgent(ctx context.Context, conversationID, agentID string) error {
	body := map[string]string{"agentId": agentID}
	resp, err := s.c.put(ctx, "/api/v1/conversations/"+url.PathEscape(conversationID)+"/default-agent", body, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// AddParticipant adds a participant to a conversation.
func (s *conversationService) AddParticipant(ctx context.Context, conversationID string, req *AddParticipantRequest) (*store.ConversationParticipant, error) {
	resp, err := s.c.post(ctx, "/api/v1/conversations/"+url.PathEscape(conversationID)+"/participants", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.ConversationParticipant](resp)
}

// Leave removes the caller from a conversation.
func (s *conversationService) Leave(ctx context.Context, conversationID string) error {
	resp, err := s.c.post(ctx, "/api/v1/conversations/"+url.PathEscape(conversationID)+"/leave", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
