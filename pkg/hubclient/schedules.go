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
	"fmt"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ScheduleService handles recurring schedule operations scoped to a project.
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
	c         *client
	projectID string
}

func (s *scheduleService) basePath() string {
	return fmt.Sprintf("/api/v1/projects/%s/schedules", url.PathEscape(s.projectID))
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
	ProjectID     string     `json:"projectId"`
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
	CreatedBy     string     `json:"createdBy"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	UpdatedBy     string     `json:"updatedBy,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (s *Schedule) UnmarshalJSON(data []byte) error {
	type Alias Schedule
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.ProjectID == "" && aux.GroveID != "" {
		s.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (s Schedule) MarshalJSON() ([]byte, error) {
	type Alias Schedule
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(s),
		GroveID: s.ProjectID,
	})
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
	resp, err := s.c.post(ctx, s.basePath(), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// Get retrieves a schedule by ID.
func (s *scheduleService) Get(ctx context.Context, id string) (*Schedule, error) {
	resp, err := s.c.get(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
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

	resp, err := s.c.getWithQuery(ctx, s.basePath(), query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListSchedulesResponse](resp)
}

// Update updates a schedule.
func (s *scheduleService) Update(ctx context.Context, id string, req *UpdateScheduleRequest) (*Schedule, error) {
	resp, err := s.c.patch(ctx, s.basePath()+"/"+url.PathEscape(id), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// Delete deletes a schedule.
func (s *scheduleService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// Pause pauses an active schedule.
func (s *scheduleService) Pause(ctx context.Context, id string) (*Schedule, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/pause", nil, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Schedule](resp)
}

// Resume resumes a paused schedule.
func (s *scheduleService) Resume(ctx context.Context, id string) (*Schedule, error) {
	resp, err := s.c.post(ctx, s.basePath()+"/"+url.PathEscape(id)+"/resume", nil, nil)
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

	resp, err := s.c.getWithQuery(ctx, s.basePath()+"/"+url.PathEscape(id)+"/history", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ListScheduledEventsResponse](resp)
}
