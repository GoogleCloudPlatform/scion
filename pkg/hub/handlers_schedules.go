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

package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/robfig/cron/v3"
)

// CreateScheduleRequest is the API request for creating a recurring schedule.
type CreateScheduleRequest struct {
	Name      string `json:"name"`
	CronExpr  string `json:"cronExpr"`
	EventType string `json:"eventType"`
	Payload   string `json:"payload,omitempty"` // Raw JSON payload (advanced)

	// Convenience fields for "message" events — used to auto-construct Payload
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

// UpdateScheduleRequest is the API request for updating a recurring schedule.
type UpdateScheduleRequest struct {
	Name      string `json:"name,omitempty"`
	CronExpr  string `json:"cronExpr,omitempty"`
	EventType string `json:"eventType,omitempty"`
	Payload   string `json:"payload,omitempty"`
	Status    string `json:"status,omitempty"`
}

// ListSchedulesResponse is the API response for listing schedules.
type ListSchedulesResponse struct {
	Schedules  []store.Schedule `json:"schedules"`
	NextCursor string           `json:"nextCursor,omitempty"`
	TotalCount int              `json:"totalCount,omitempty"`
	ServerTime time.Time        `json:"serverTime"`
}

// handleSchedules routes requests under /api/v1/groves/{groveId}/schedules[/{id}[/{action}]]
func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request, groveID, schedulePath string) {
	// Require authentication
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	// For agent identities, enforce grove isolation
	if agentIdentity := GetAgentIdentityFromContext(r.Context()); agentIdentity != nil {
		if agentIdentity.GroveID() != groveID {
			Forbidden(w)
			return
		}
	}

	if schedulePath == "" {
		// Collection endpoint
		switch r.Method {
		case http.MethodGet:
			s.listSchedules(w, r, groveID)
		case http.MethodPost:
			s.createSchedule(w, r, groveID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// Parse schedule ID and optional action from path
	parts := strings.SplitN(schedulePath, "/", 2)
	scheduleID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		// Individual schedule endpoint
		switch r.Method {
		case http.MethodGet:
			s.getSchedule(w, r, groveID, scheduleID)
		case http.MethodPatch:
			s.updateSchedule(w, r, groveID, scheduleID)
		case http.MethodDelete:
			s.deleteSchedule(w, r, groveID, scheduleID)
		default:
			MethodNotAllowed(w)
		}
	case "pause":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.pauseSchedule(w, r, groveID, scheduleID)
	case "resume":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w)
			return
		}
		s.resumeSchedule(w, r, groveID, scheduleID)
	case "history":
		if r.Method != http.MethodGet {
			MethodNotAllowed(w)
			return
		}
		s.getScheduleHistory(w, r, groveID, scheduleID)
	default:
		NotFound(w, "Schedule action")
	}
}

// createSchedule handles POST /api/v1/groves/{groveId}/schedules
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, groveID string) {
	var req CreateScheduleRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	if req.CronExpr == "" {
		ValidationError(w, "cronExpr is required", nil)
		return
	}
	if req.EventType == "" {
		ValidationError(w, "eventType is required", nil)
		return
	}
	if req.EventType != "message" {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s", req.EventType), nil)
		return
	}

	// Validate cron expression using standard 5-field parser
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	cronSchedule, err := parser.Parse(req.CronExpr)
	if err != nil {
		ValidationError(w, fmt.Sprintf("invalid cron expression: %v", err), nil)
		return
	}

	// Build payload
	payload := req.Payload
	if payload == "" && req.EventType == "message" {
		if req.Message == "" {
			ValidationError(w, "message is required for message schedules (or provide raw payload)", nil)
			return
		}
		if req.AgentName == "" {
			ValidationError(w, "agentName is required for message schedules", nil)
			return
		}
		p := MessageEventPayload{
			AgentName: req.AgentName,
			Message:   req.Message,
			Interrupt: req.Interrupt,
		}
		payloadBytes, marshalErr := json.Marshal(p)
		if marshalErr != nil {
			InternalError(w)
			return
		}
		payload = string(payloadBytes)
	}

	// Compute next run time
	nextRunAt := cronSchedule.Next(time.Now().UTC())

	// Determine creator identity
	createdBy := ""
	if identity := GetIdentityFromContext(r.Context()); identity != nil {
		createdBy = identity.ID()
	}

	schedule := store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      req.Name,
		CronExpr:  req.CronExpr,
		EventType: req.EventType,
		Payload:   payload,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &nextRunAt,
		CreatedBy: createdBy,
	}

	if err := s.store.CreateSchedule(r.Context(), &schedule); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Fetch the created schedule to get the full record
	created, err := s.store.GetSchedule(r.Context(), schedule.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, schedule)
		return
	}

	writeJSON(w, http.StatusCreated, created)
}

// listSchedules handles GET /api/v1/groves/{groveId}/schedules
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, groveID string) {
	query := r.URL.Query()

	filter := store.ScheduleFilter{
		GroveID: groveID,
		Status:  query.Get("status"),
		Name:    query.Get("name"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListSchedules(r.Context(), filter, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListSchedulesResponse{
		Schedules:  result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}

// getSchedule handles GET /api/v1/groves/{groveId}/schedules/{id}
func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, groveID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if schedule.GroveID != groveID {
		NotFound(w, "Schedule")
		return
	}

	writeJSON(w, http.StatusOK, schedule)
}

// updateSchedule handles PATCH /api/v1/groves/{groveId}/schedules/{id}
func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, groveID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.GroveID != groveID {
		NotFound(w, "Schedule")
		return
	}

	var req UpdateScheduleRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Name != "" {
		schedule.Name = req.Name
	}
	if req.CronExpr != "" {
		// Validate new cron expression
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		cronSchedule, err := parser.Parse(req.CronExpr)
		if err != nil {
			ValidationError(w, fmt.Sprintf("invalid cron expression: %v", err), nil)
			return
		}
		schedule.CronExpr = req.CronExpr
		nextRunAt := cronSchedule.Next(time.Now().UTC())
		schedule.NextRunAt = &nextRunAt
	}
	if req.EventType != "" {
		schedule.EventType = req.EventType
	}
	if req.Payload != "" {
		schedule.Payload = req.Payload
	}
	if req.Status != "" {
		schedule.Status = req.Status
	}

	if err := s.store.UpdateSchedule(r.Context(), schedule); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, schedule)
}

// deleteSchedule handles DELETE /api/v1/groves/{groveId}/schedules/{id}
func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, groveID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.GroveID != groveID {
		NotFound(w, "Schedule")
		return
	}

	if err := s.store.DeleteSchedule(r.Context(), scheduleID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// pauseSchedule handles POST /api/v1/groves/{groveId}/schedules/{id}/pause
func (s *Server) pauseSchedule(w http.ResponseWriter, r *http.Request, groveID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.GroveID != groveID {
		NotFound(w, "Schedule")
		return
	}
	if schedule.Status != store.ScheduleStatusActive {
		ValidationError(w, "only active schedules can be paused", nil)
		return
	}

	if err := s.store.UpdateScheduleStatus(r.Context(), scheduleID, store.ScheduleStatusPaused); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	schedule.Status = store.ScheduleStatusPaused
	writeJSON(w, http.StatusOK, schedule)
}

// resumeSchedule handles POST /api/v1/groves/{groveId}/schedules/{id}/resume
func (s *Server) resumeSchedule(w http.ResponseWriter, r *http.Request, groveID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.GroveID != groveID {
		NotFound(w, "Schedule")
		return
	}
	if schedule.Status != store.ScheduleStatusPaused {
		ValidationError(w, "only paused schedules can be resumed", nil)
		return
	}

	// Recompute next run time
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	cronSchedule, err := parser.Parse(schedule.CronExpr)
	if err != nil {
		InternalError(w)
		return
	}
	nextRunAt := cronSchedule.Next(time.Now().UTC())

	if err := s.store.UpdateScheduleStatus(r.Context(), scheduleID, store.ScheduleStatusActive); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Update next_run_at
	schedule.Status = store.ScheduleStatusActive
	schedule.NextRunAt = &nextRunAt
	if err := s.store.UpdateSchedule(r.Context(), schedule); err != nil {
		// Status was updated, but next_run_at wasn't — still return success
		writeJSON(w, http.StatusOK, schedule)
		return
	}

	writeJSON(w, http.StatusOK, schedule)
}

// getScheduleHistory handles GET /api/v1/groves/{groveId}/schedules/{id}/history
func (s *Server) getScheduleHistory(w http.ResponseWriter, r *http.Request, groveID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.GroveID != groveID {
		NotFound(w, "Schedule")
		return
	}

	query := r.URL.Query()
	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	// List events generated by this schedule
	result, err := s.store.ListScheduledEvents(r.Context(), store.ScheduledEventFilter{
		GroveID:    groveID,
		ScheduleID: scheduleID,
	}, store.ListOptions{
		Limit:  limit,
		Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, ListScheduledEventsResponse{
		Events:     result.Items,
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}
