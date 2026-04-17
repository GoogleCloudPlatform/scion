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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CreateScheduledEventRequest is the API request for creating a scheduled event.
type CreateScheduledEventRequest struct {
	EventType string `json:"eventType"`         // Required: "message", "dispatch_agent", or "workflow_run"
	FireAt    string `json:"fireAt,omitempty"`  // ISO 8601 absolute time
	FireIn    string `json:"fireIn,omitempty"`  // Duration string (e.g. "30m")
	Payload   string `json:"payload,omitempty"` // Raw JSON payload (advanced)

	// Convenience fields for "message" events — used to auto-construct Payload
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Plain     bool   `json:"plain,omitempty"`

	// Convenience fields for "dispatch_agent" events — used to auto-construct Payload
	Template string `json:"template,omitempty"`
	Task     string `json:"task,omitempty"`
	Branch   string `json:"branch,omitempty"`

	// Fields for "workflow_run" events — mutually exclusive with agent-message fields.
	WorkflowSource string `json:"workflowSource,omitempty"` // Raw YAML of the duckflux workflow (required for workflow_run)
	WorkflowInputs string `json:"workflowInputs,omitempty"` // JSON inputs for the workflow run (optional)
}

// ScheduledEventResponse is the API response for a single scheduled event.
type ScheduledEventResponse struct {
	store.ScheduledEvent
}

// ListScheduledEventsResponse is the API response for listing scheduled events.
type ListScheduledEventsResponse struct {
	Events     []store.ScheduledEvent `json:"events"`
	NextCursor string                 `json:"nextCursor,omitempty"`
	TotalCount int                    `json:"totalCount,omitempty"`
	ServerTime time.Time              `json:"serverTime"`
}

// handleScheduledEvents routes requests under /api/v1/groves/{groveId}/scheduled-events[/{id}]
func (s *Server) handleScheduledEvents(w http.ResponseWriter, r *http.Request, groveID, eventPath string) {
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

	if eventPath == "" {
		// Collection endpoint
		switch r.Method {
		case http.MethodGet:
			s.listScheduledEvents(w, r, groveID)
		case http.MethodPost:
			s.createScheduledEvent(w, r, groveID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// Individual event endpoint
	eventID := eventPath
	switch r.Method {
	case http.MethodGet:
		s.getScheduledEvent(w, r, groveID, eventID)
	case http.MethodDelete:
		s.cancelScheduledEvent(w, r, groveID, eventID)
	default:
		MethodNotAllowed(w)
	}
}

// createScheduledEvent handles POST /api/v1/groves/{groveId}/scheduled-events
func (s *Server) createScheduledEvent(w http.ResponseWriter, r *http.Request, groveID string) {
	var req CreateScheduledEventRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate event type
	if req.EventType == "" {
		ValidationError(w, "eventType is required", nil)
		return
	}
	if req.EventType != "message" && req.EventType != "dispatch_agent" && req.EventType != "workflow_run" {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: message, dispatch_agent, workflow_run)", req.EventType), nil)
		return
	}

	// Validate fire time: exactly one of FireAt or FireIn must be provided
	if req.FireAt == "" && req.FireIn == "" {
		ValidationError(w, "either fireAt or fireIn is required", nil)
		return
	}
	if req.FireAt != "" && req.FireIn != "" {
		ValidationError(w, "fireAt and fireIn are mutually exclusive", nil)
		return
	}

	var fireAt time.Time
	if req.FireAt != "" {
		var err error
		fireAt, err = time.Parse(time.RFC3339, req.FireAt)
		if err != nil {
			ValidationError(w, "fireAt must be a valid ISO 8601 / RFC 3339 timestamp", nil)
			return
		}
		if fireAt.Before(time.Now()) {
			ValidationError(w, "fireAt must be in the future", nil)
			return
		}
	} else {
		duration, err := time.ParseDuration(req.FireIn)
		if err != nil {
			ValidationError(w, "fireIn must be a valid Go duration string (e.g. 30m, 1h)", nil)
			return
		}
		if duration <= 0 {
			ValidationError(w, "fireIn must be a positive duration", nil)
			return
		}
		fireAt = time.Now().Add(duration)
	}

	// Build payload
	payload := req.Payload
	if payload == "" && req.EventType == "dispatch_agent" {
		if req.AgentName == "" {
			ValidationError(w, "agentName is required for dispatch_agent events", nil)
			return
		}
		p := DispatchAgentEventPayload{
			AgentName: req.AgentName,
			Template:  req.Template,
			Task:      req.Task,
			Branch:    req.Branch,
		}
		payloadBytes, err := json.Marshal(p)
		if err != nil {
			InternalError(w)
			return
		}
		payload = string(payloadBytes)
	}
	if payload == "" && req.EventType == "message" {
		// Auto-construct payload from convenience fields
		if req.Message == "" {
			ValidationError(w, "message is required for message events", nil)
			return
		}
		if req.AgentID == "" && req.AgentName == "" {
			ValidationError(w, "agentId or agentName is required for message events", nil)
			return
		}
		p := MessageEventPayload{
			AgentID:   req.AgentID,
			AgentName: req.AgentName,
			Message:   req.Message,
			Interrupt: req.Interrupt,
			Plain:     req.Plain,
		}
		payloadBytes, err := json.Marshal(p)
		if err != nil {
			InternalError(w)
			return
		}
		payload = string(payloadBytes)
	}

	// Validate and extract workflow_run fields.
	workflowSource := ""
	workflowInputs := ""
	if req.EventType == "workflow_run" {
		if req.WorkflowSource == "" {
			ValidationError(w, "workflowSource is required for workflow_run events", nil)
			return
		}
		workflowSource = req.WorkflowSource
		workflowInputs = req.WorkflowInputs
		if workflowInputs == "" {
			workflowInputs = "{}"
		} else if !json.Valid([]byte(workflowInputs)) {
			ValidationError(w, "workflowInputs must be valid JSON", nil)
			return
		}
		if payload == "" {
			payload = "{}" // placeholder to satisfy NOT NULL constraint
		}
	}

	// Determine creator identity
	createdBy := ""
	if identity := GetIdentityFromContext(r.Context()); identity != nil {
		createdBy = identity.ID()
	}

	evt := store.ScheduledEvent{
		ID:             api.NewUUID(),
		GroveID:        groveID,
		EventType:      req.EventType,
		FireAt:         fireAt,
		Payload:        payload,
		Status:         store.ScheduledEventPending,
		CreatedBy:      createdBy,
		WorkflowSource: workflowSource,
		WorkflowInputs: workflowInputs,
	}

	if err := s.scheduler.ScheduleEvent(r.Context(), evt); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Fetch the created event to get the full record (with CreatedAt etc.)
	created, err := s.store.GetScheduledEvent(r.Context(), evt.ID)
	if err != nil {
		// Event was created successfully, but we can't fetch it back — return what we have
		writeJSON(w, http.StatusCreated, evt)
		return
	}

	writeJSON(w, http.StatusCreated, created)
}

// listScheduledEvents handles GET /api/v1/groves/{groveId}/scheduled-events
func (s *Server) listScheduledEvents(w http.ResponseWriter, r *http.Request, groveID string) {
	query := r.URL.Query()

	filter := store.ScheduledEventFilter{
		GroveID:   groveID,
		EventType: query.Get("eventType"),
		Status:    query.Get("status"),
	}

	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	result, err := s.store.ListScheduledEvents(r.Context(), filter, store.ListOptions{
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

// getScheduledEvent handles GET /api/v1/groves/{groveId}/scheduled-events/{id}
func (s *Server) getScheduledEvent(w http.ResponseWriter, r *http.Request, groveID, eventID string) {
	evt, err := s.store.GetScheduledEvent(r.Context(), eventID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the event belongs to the requested grove
	if evt.GroveID != groveID {
		NotFound(w, "Scheduled event")
		return
	}

	writeJSON(w, http.StatusOK, evt)
}

// cancelScheduledEvent handles DELETE /api/v1/groves/{groveId}/scheduled-events/{id}
func (s *Server) cancelScheduledEvent(w http.ResponseWriter, r *http.Request, groveID, eventID string) {
	// Verify event exists and belongs to the grove
	evt, err := s.store.GetScheduledEvent(r.Context(), eventID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if evt.GroveID != groveID {
		NotFound(w, "Scheduled event")
		return
	}

	if err := s.scheduler.CancelEvent(r.Context(), eventID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
