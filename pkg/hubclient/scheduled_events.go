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
	"fmt"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ScheduledEventService handles scheduled event operations scoped to a grove.
type ScheduledEventService interface {
	// Create creates a new scheduled event.
	Create(ctx context.Context, req *CreateScheduledEventRequest) (*ScheduledEvent, error)

	// Get retrieves a scheduled event by ID.
	Get(ctx context.Context, id string) (*ScheduledEvent, error)

	// List returns scheduled events matching the filter criteria.
	List(ctx context.Context, opts *ListScheduledEventsOptions) (*ListScheduledEventsResponse, error)

	// Cancel cancels a pending scheduled event.
	Cancel(ctx context.Context, id string) error
}

// scheduledEventService is the implementation of ScheduledEventService.
type scheduledEventService struct {
	c       *client
	groveID string
}

func (s *scheduledEventService) basePath() string {
	return fmt.Sprintf("/api/v1/groves/%s/scheduled-events", url.PathEscape(s.groveID))
}

// CreateScheduledEventRequest is the client-side request for creating a scheduled event.
type CreateScheduledEventRequest struct {
	EventType string `json:"eventType"`
	FireAt    string `json:"fireAt,omitempty"`
	FireIn    string `json:"fireIn,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Plain     bool   `json:"plain,omitempty"`
}

// ScheduledEvent represents a scheduled event returned by the Hub API.
type ScheduledEvent struct {
	ID         string     `json:"id"`
	GroveID    string     `json:"groveId"`
	EventType  string     `json:"eventType"`
	FireAt     time.Time  `json:"fireAt"`
	Payload    string     `json:"payload"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	CreatedBy  string     `json:"createdBy"`
	FiredAt    *time.Time `json:"firedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	ScheduleID string     `json:"scheduleId,omitempty"`
}

// ListScheduledEventsOptions configures scheduled event listing.
type ListScheduledEventsOptions struct {
	Status    string
	EventType string
	Page      apiclient.PageOptions
}

// ListScheduledEventsResponse is the response from listing scheduled events.
type ListScheduledEventsResponse struct {
	Events     []ScheduledEvent `json:"events"`
	NextCursor string           `json:"nextCursor,omitempty"`
	TotalCount int              `json:"totalCount,omitempty"`
	ServerTime time.Time        `json:"serverTime"`
}

// Create creates a new scheduled event.
func (s *scheduledEventService) Create(ctx context.Context, req *CreateScheduledEventRequest) (*ScheduledEvent, error) {
	resp, err := s.c.transport.Post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ScheduledEvent](resp)
}

// Get retrieves a scheduled event by ID.
func (s *scheduledEventService) Get(ctx context.Context, id string) (*ScheduledEvent, error) {
	resp, err := s.c.transport.Get(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ScheduledEvent](resp)
}

// List returns scheduled events matching the filter criteria.
func (s *scheduledEventService) List(ctx context.Context, opts *ListScheduledEventsOptions) (*ListScheduledEventsResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.EventType != "" {
			query.Set("eventType", opts.EventType)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.transport.GetWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListScheduledEventsResponse](resp)
}

// Cancel cancels a pending scheduled event.
func (s *scheduledEventService) Cancel(ctx context.Context, id string) error {
	resp, err := s.c.transport.Delete(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
