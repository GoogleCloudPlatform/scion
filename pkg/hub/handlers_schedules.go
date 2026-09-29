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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/robfig/cron/v3"
)

// CreateScheduleRequest is the API request for creating a recurring schedule.
//
// NOTE (O-R2-3): This struct has no AgentID field — message targets are
// specified via AgentName (convenience) or raw Payload (advanced). The
// authoring-time validation in authorizeScheduledMessageAuthoring resolves
// the target from whichever is present. If an AgentID field is added in the
// future, the authoring call must be updated to forward it.
type CreateScheduleRequest struct {
	Name      string `json:"name"`
	CronExpr  string `json:"cronExpr"`
	EventType string `json:"eventType"`
	Payload   string `json:"payload,omitempty"` // Raw JSON payload (advanced)

	// Convenience fields for "message" events — used to auto-construct Payload
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`

	// Convenience fields for "dispatch_agent" events — used to auto-construct Payload
	Template string `json:"template,omitempty"`
	Task     string `json:"task,omitempty"`
	Branch   string `json:"branch,omitempty"`
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

// handleSchedules routes requests under /api/v1/projects/{projectId}/schedules[/{id}[/{action}]]
func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request, projectID, schedulePath string) {
	// Require authentication
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	if !checkAgentReadScope(w, r) {
		return
	}

	// For agent identities, enforce project isolation
	if agentIdentity := GetAgentIdentityFromContext(r.Context()); agentIdentity != nil {
		if agentIdentity.ProjectID() != projectID {
			Forbidden(w)
			return
		}
	}

	// Parse schedule ID and optional sub-action once, reused for both
	// authorization and dispatch below.
	pathParts := strings.SplitN(schedulePath, "/", 2)

	// Determine the authorization action from method and path.
	var authzAction Action
	if schedulePath == "" {
		switch r.Method {
		case http.MethodGet:
			authzAction = ActionList
		case http.MethodPost:
			authzAction = ActionCreate
		default:
			MethodNotAllowed(w)
			return
		}
	} else {
		subAction := ""
		if len(pathParts) > 1 {
			subAction = pathParts[1]
		}

		switch subAction {
		case "":
			switch r.Method {
			case http.MethodGet:
				authzAction = ActionRead
			case http.MethodPatch:
				authzAction = ActionUpdate
			case http.MethodDelete:
				authzAction = ActionDelete
			default:
				MethodNotAllowed(w)
				return
			}
		case "pause", "resume":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w)
				return
			}
			authzAction = ActionUpdate
		case "history":
			if r.Method != http.MethodGet {
				MethodNotAllowed(w)
				return
			}
			authzAction = ActionRead
		default:
			NotFound(w, "Schedule action")
			return
		}
	}

	// Authorize access — fail closed for all identity types.
	if !s.authorizeScheduledEventAccess(w, r, projectID, authzAction) {
		return
	}

	// Dispatch to handler — method filtering is done in the authorization
	// block above; only valid methods reach this point.
	if schedulePath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listSchedules(w, r, projectID)
		case http.MethodPost:
			s.createSchedule(w, r, projectID)
		}
		return
	}

	scheduleID := pathParts[0]
	routeAction := ""
	if len(pathParts) > 1 {
		routeAction = pathParts[1]
	}

	switch routeAction {
	case "":
		// Individual schedule endpoint
		switch r.Method {
		case http.MethodGet:
			s.getSchedule(w, r, projectID, scheduleID)
		case http.MethodPatch:
			s.updateSchedule(w, r, projectID, scheduleID)
		case http.MethodDelete:
			s.deleteSchedule(w, r, projectID, scheduleID)
		}
	case "pause":
		s.pauseSchedule(w, r, projectID, scheduleID)
	case "resume":
		s.resumeSchedule(w, r, projectID, scheduleID)
	case "history":
		s.getScheduleHistory(w, r, projectID, scheduleID)
	}
}

// createSchedule handles POST /api/v1/projects/{projectId}/schedules
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, projectID string) {
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
	if req.EventType != "message" && req.EventType != "dispatch_agent" {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: message, dispatch_agent)", req.EventType), nil)
		return
	}
	if req.EventType == "dispatch_agent" {
		if !s.authorizeScheduledDispatchAgentAuthoring(w, r) {
			return
		}
		if !s.authorizeAgentCreate(w, r, projectID) {
			return
		}
	}
	// C1 containment: validate target agent project scope for message schedules.
	if req.EventType == "message" {
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, req.Payload, "", req.AgentName) {
			return
		}
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
	if payload == "" && req.EventType == "dispatch_agent" {
		if req.AgentName == "" {
			ValidationError(w, "agentName is required for dispatch_agent schedules", nil)
			return
		}
		p := DispatchAgentEventPayload{
			AgentName: req.AgentName,
			Template:  req.Template,
			Task:      req.Task,
			Branch:    req.Branch,
		}
		payloadBytes, marshalErr := json.Marshal(p)
		if marshalErr != nil {
			InternalError(w)
			return
		}
		payload = string(payloadBytes)
	}
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
		ProjectID: projectID,
		Name:      req.Name,
		CronExpr:  req.CronExpr,
		EventType: req.EventType,
		Payload:   payload,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &nextRunAt,
		CreatedBy: createdBy,
		// E.2b: record the authoring request's initiator attribution in the
		// same write as the schedule row (design check (a)).
		InitiatorAttribution: newInitiatorAttribution(r.Context()),
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

// listSchedules handles GET /api/v1/projects/{projectId}/schedules
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, projectID string) {
	query := r.URL.Query()

	filter := store.ScheduleFilter{
		ProjectID: projectID,
		Status:    query.Get("status"),
		Name:      query.Get("name"),
	}

	result, err := s.store.ListSchedules(r.Context(), filter, listOptionsFromQuery(query))
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

// getSchedule handles GET /api/v1/projects/{projectId}/schedules/{id}
func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if schedule.ProjectID != projectID {
		NotFound(w, "Schedule")
		return
	}

	writeJSON(w, http.StatusOK, schedule)
}

// updateSchedule handles PATCH /api/v1/projects/{projectId}/schedules/{id}
func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.ProjectID != projectID {
		NotFound(w, "Schedule")
		return
	}
	originalStatus := schedule.Status

	var req UpdateScheduleRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.EventType != "" && req.EventType != "message" && req.EventType != "dispatch_agent" {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: message, dispatch_agent)", req.EventType), nil)
		return
	}
	if schedule.EventType == "dispatch_agent" || req.EventType == "dispatch_agent" {
		if !s.authorizeScheduledDispatchAgentAuthoring(w, r) {
			return
		}
		if !s.authorizeAgentCreate(w, r, projectID) {
			return
		}
	}
	// C1 containment: validate target agent project scope when the schedule
	// is or becomes a message schedule. Check both the effective event type
	// and the effective payload after the update is applied.
	effectiveEventType := schedule.EventType
	if req.EventType != "" {
		effectiveEventType = req.EventType
	}
	if effectiveEventType == "message" {
		effectivePayload := schedule.Payload
		if req.Payload != "" {
			effectivePayload = req.Payload
		}
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, effectivePayload, "", "") {
			return
		}
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

	// E.2b / ruling Q2: a fully reauthorized mutation that changes future
	// dispatch (payload/target/type/timing, or an enable transition) replaces
	// the attribution and bumps authorization_revision atomically in the same
	// write. A metadata-only edit (name, or a status change other than an
	// enable, e.g. pause) does not re-attribute — and does not touch the
	// attribution columns at all (review R2): attribution is passed to the
	// store only when it changed.
	changesFutureDispatch := req.CronExpr != "" || req.EventType != "" || req.Payload != "" ||
		(req.Status == store.ScheduleStatusActive && originalStatus != store.ScheduleStatusActive)

	var attribution *store.ScheduleAttributionUpdate
	if changesFutureDispatch {
		prevRevision := schedule.AuthorizationRevision
		prevRevisionKnown := schedule.AttributionVersion != 0
		newAttr := reattributeInitiator(r.Context(), schedule.InitiatorAttribution)
		schedule.InitiatorAttribution = newAttr
		attribution = &store.ScheduleAttributionUpdate{
			Attribution:       newAttr,
			PrevRevision:      prevRevision,
			PrevRevisionKnown: prevRevisionKnown,
		}
	}

	if err := s.store.UpdateSchedule(r.Context(), schedule, attribution); err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"schedule was concurrently modified; refresh and retry", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, schedule)
}

// deleteSchedule handles DELETE /api/v1/projects/{projectId}/schedules/{id}
func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.ProjectID != projectID {
		NotFound(w, "Schedule")
		return
	}

	if err := s.store.DeleteSchedule(r.Context(), scheduleID); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// E.2b (review O1): no future dispatch is created by a delete, so there
	// is no re-attribution — just a record of who deleted it.
	s.emitMutationAudit(r.Context(), &store.MutationAuditRecord{
		MutationType: "schedule_delete",
		TargetType:   "schedule",
		TargetID:     scheduleID,
	})

	w.WriteHeader(http.StatusNoContent)
}

// pauseSchedule handles POST /api/v1/projects/{projectId}/schedules/{id}/pause
func (s *Server) pauseSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.ProjectID != projectID {
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

	// E.2b (review O1): no future dispatch is created by a pause, so there
	// is no re-attribution — just a record of who paused it.
	s.emitMutationAudit(r.Context(), &store.MutationAuditRecord{
		MutationType: "schedule_pause",
		TargetType:   "schedule",
		TargetID:     scheduleID,
	})

	schedule.Status = store.ScheduleStatusPaused
	writeJSON(w, http.StatusOK, schedule)
}

// resumeSchedule handles POST /api/v1/projects/{projectId}/schedules/{id}/resume
func (s *Server) resumeSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.ProjectID != projectID {
		NotFound(w, "Schedule")
		return
	}
	// Resuming re-arms future dispatch authority for a dispatch_agent
	// schedule; gate it the same way authoring is gated.
	if schedule.EventType == "dispatch_agent" {
		if !s.authorizeScheduledDispatchAgentAuthoring(w, r) {
			return
		}
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

	// E.2b / ruling Q2 (review R3): status, next_run_at and the
	// re-attribution are one write, not two — a failure here must be
	// reported as an error, never as a 200 naming an attribution that was
	// never persisted.
	prevRevision := schedule.AuthorizationRevision
	prevRevisionKnown := schedule.AttributionVersion != 0
	schedule.Status = store.ScheduleStatusActive
	schedule.NextRunAt = &nextRunAt
	newAttr := reattributeInitiator(r.Context(), schedule.InitiatorAttribution)
	schedule.InitiatorAttribution = newAttr

	if err := s.store.UpdateSchedule(r.Context(), schedule, &store.ScheduleAttributionUpdate{
		Attribution:       newAttr,
		PrevRevision:      prevRevision,
		PrevRevisionKnown: prevRevisionKnown,
	}); err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"schedule was concurrently modified; refresh and retry", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, schedule)
}

// getScheduleHistory handles GET /api/v1/projects/{projectId}/schedules/{id}/history
func (s *Server) getScheduleHistory(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if schedule.ProjectID != projectID {
		NotFound(w, "Schedule")
		return
	}

	query := r.URL.Query()

	// List events generated by this schedule
	result, err := s.store.ListScheduledEvents(r.Context(), store.ScheduledEventFilter{
		ProjectID:  projectID,
		ScheduleID: scheduleID,
	}, listOptionsFromQuery(query))
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
