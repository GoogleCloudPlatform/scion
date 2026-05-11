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
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MessageService provides operations on the user's message inbox.
type MessageService interface {
	// List returns messages for the authenticated user.
	List(ctx context.Context, opts *ListMessagesOptions) (*store.ListResult[store.Message], error)

	// Get returns a single message by ID.
	Get(ctx context.Context, id string) (*store.Message, error)

	// MarkRead marks a message as read.
	MarkRead(ctx context.Context, id string) error

	// MarkAllRead marks all messages as read.
	MarkAllRead(ctx context.Context) error
}

// messageService is the implementation of MessageService.
type messageService struct {
	c *client
}

// ListMessagesOptions configures message listing.
type ListMessagesOptions struct {
	OnlyUnread bool
	AgentID    string
	ProjectID  string
	Type       string
	Limit      int
	Cursor     string
}

// Message is a local alias for the store message type used in CLI responses.
type Message = store.Message

// MessageListResult is a local alias for list results.
type MessageListResult = store.ListResult[store.Message]

// AgentMessage is a lightweight view of a message used in agent-scoped listings.
type AgentMessage struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	Sender      string    `json:"sender"`
	SenderID    string    `json:"senderId"`
	Recipient   string    `json:"recipient"`
	RecipientID string    `json:"recipientId"`
	Msg         string    `json:"msg"`
	Type        string    `json:"type"`
	Urgent      bool      `json:"urgent,omitempty"`
	Broadcasted bool      `json:"broadcasted,omitempty"`
	Read        bool      `json:"read"`
	AgentID     string    `json:"agentId"`
	CreatedAt   time.Time `json:"createdAt"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (m *AgentMessage) UnmarshalJSON(data []byte) error {
	type Alias AgentMessage
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if m.ProjectID == "" && aux.GroveID != "" {
		m.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (m AgentMessage) MarshalJSON() ([]byte, error) {
	type Alias AgentMessage
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(m),
		GroveID: m.ProjectID,
	})
}

// List returns messages for the authenticated user.
func (s *messageService) List(ctx context.Context, opts *ListMessagesOptions) (*store.ListResult[store.Message], error) {
	query := url.Values{}
	if opts != nil {
		if opts.OnlyUnread {
			query.Set("unread", "true")
		}
		if opts.AgentID != "" {
			query.Set("agent", opts.AgentID)
		}
		if opts.ProjectID != "" {
			query.Set("grove", opts.ProjectID)
		}
		if opts.Type != "" {
			query.Set("type", opts.Type)
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/messages", query, nil)
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

// Get returns a single message by ID.
func (s *messageService) Get(ctx context.Context, id string) (*store.Message, error) {
	resp, err := s.c.get(ctx, "/api/v1/messages/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[store.Message](resp)
}

// MarkRead marks a message as read.
func (s *messageService) MarkRead(ctx context.Context, id string) error {
	resp, err := s.c.post(ctx, "/api/v1/messages/"+url.PathEscape(id)+"/read", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// MarkAllRead marks all messages as read.
func (s *messageService) MarkAllRead(ctx context.Context) error {
	resp, err := s.c.post(ctx, "/api/v1/messages/read-all", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
