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

// ScheduleService handles recurring schedule operations scoped to a grove.
type ScheduleService interface {
	// Create creates a new recurring schedule.
	Create(ctx context.Context, req *CreateScheduleRequest) (*Schedule, error)

	// Get retrieves a schedule by ID.
	Get(ctx context.Context, id string) (*Schedule, error)

	// List returns schedules matching the filter criteria.
	List(ctx context.Context, opts *ListSchedulesOptions) (*ListSchedulesResponse, error)

	// Update updates a schedule.
	Update(ctx context.Context, id string, req *UpdateScheduleRequest) (*Schedule, error)

	// Delete deletes a schedule.
	Delete(ctx context.Context, id string) error

	// Pause pauses an active schedule.
	Pause(ctx context.Context, id string) (*Schedule, error)

	// Resume resumes a paused schedule.
	Resume(ctx context.Context, id string) (*Schedule, error)

	// History returns execution history for a schedule.
	History(ctx context.Context, id string, opts *ListScheduledEventsOptions) (*ListScheduledEventsResponse, error)
}

// scheduleService is the implementation of ScheduleService.
type scheduleService struct {
	c       *client
	groveID string
}

func (s *scheduleService) basePath() string {
	return fmt.Sprintf("/api/v1/groves/%s/schedules", url.PathEscape(s.groveID))
}

// CreateScheduleRequest is the client-side request for creating a recurring schedule.
type CreateScheduleRequest struct {
	Name      string `json:"name"`
	CronExpr  string `json:"cronExpr"`
	EventType string `json:"eventType"`
	Payload   string `json:"payload,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Template  string `json:"template,omitempty"`
	Task      string `json:"task,omitempty"`
	Branch    string `json:"branch,omitempty"`

	// Fields for workflow_run schedules.
	WorkflowSource string `json:"workflowSource,omitempty"`
	WorkflowInputs string `json:"workflowInputs,omitempty"`
}

// UpdateScheduleRequest is the client-side request for updating a schedule.
type UpdateScheduleRequest struct {
	Name      string `json:"name,omitempty"`
	CronExpr  string `json:"cronExpr,omitempty"`
	EventType string `json:"eventType,omitempty"`
	Payload   string `json:"payload,omitempty"`
	Status    string `json:"status,omitempty"`
}

// Schedule represents a recurring schedule returned by the Hub API.
type Schedule struct {
	ID            string     `json:"id"`
	GroveID       string     `json:"groveId"`
	Name          string     `json:"name"`
	CronExpr      string     `json:"cronExpr"`
	EventType     string     `json:"eventType"`
	Payload       string     `json:"payload"`
	Status        string     `json:"status"`
	NextRunAt     *time.Time `json:"nextRunAt,omitempty"`
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"`
	LastRunStatus string     `json:"lastRunStatus,omitempty"`
	LastRunError  string     `json:"lastRunError,omitempty"`
	RunCount      int        `json:"runCount"`
	ErrorCount    int        `json:"errorCount"`
	CreatedAt     time.Time  `json:"createdAt"`
	CreatedBy     string     `json:"createdBy,omitempty"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// ListSchedulesOptions configures schedule listing.
type ListSchedulesOptions struct {
	Status string
	Name   string
	Page   apiclient.PageOptions
}

// ListSchedulesResponse is the response from listing schedules.
type ListSchedulesResponse struct {
	Schedules  []Schedule `json:"schedules"`
	NextCursor string     `json:"nextCursor,omitempty"`
	TotalCount int        `json:"totalCount,omitempty"`
	ServerTime time.Time  `json:"serverTime"`
}

// Create creates a new recurring schedule.
func (s *scheduleService) Create(ctx context.Context, req *CreateScheduleRequest) (*Schedule, error) {
	resp, err := s.c.transport.Post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// Get retrieves a schedule by ID.
func (s *scheduleService) Get(ctx context.Context, id string) (*Schedule, error) {
	resp, err := s.c.transport.Get(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// List returns schedules matching the filter criteria.
func (s *scheduleService) List(ctx context.Context, opts *ListSchedulesOptions) (*ListSchedulesResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.Name != "" {
			query.Set("name", opts.Name)
		}
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.transport.GetWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListSchedulesResponse](resp)
}

// Update updates a schedule.
func (s *scheduleService) Update(ctx context.Context, id string, req *UpdateScheduleRequest) (*Schedule, error) {
	resp, err := s.c.transport.Patch(ctx, s.basePath()+"/"+url.PathEscape(id), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// Delete deletes a schedule.
func (s *scheduleService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.transport.Delete(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Pause pauses an active schedule.
func (s *scheduleService) Pause(ctx context.Context, id string) (*Schedule, error) {
	resp, err := s.c.transport.Post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/pause", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// Resume resumes a paused schedule.
func (s *scheduleService) Resume(ctx context.Context, id string) (*Schedule, error) {
	resp, err := s.c.transport.Post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/resume", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// History returns execution history for a schedule.
func (s *scheduleService) History(ctx context.Context, id string, opts *ListScheduledEventsOptions) (*ListScheduledEventsResponse, error) {
	query := url.Values{}
	if opts != nil {
		opts.Page.ToQuery(query)
	}

	resp, err := s.c.transport.GetWithQuery(ctx, s.basePath()+"/"+url.PathEscape(id)+"/history", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListScheduledEventsResponse](resp)
}
