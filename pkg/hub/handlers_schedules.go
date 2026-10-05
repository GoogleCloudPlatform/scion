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
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
				MethodNotAllowed(w, http.MethodGet, http.MethodPatch, http.MethodDelete)
				return
			}
		case "pause", "resume":
			if r.Method != http.MethodPost {
				MethodNotAllowed(w, http.MethodPost)
				return
			}
			authzAction = ActionUpdate
		case "history":
			if r.Method != http.MethodGet {
				MethodNotAllowed(w, http.MethodGet)
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
	// Recurring schedules accept (ptone/scion#2200)
	// the same advanced Payload JSON as one-shot scheduled events and must
	// be tombstoned the same way (see createScheduledEvent), for both
	// supported event types — not just "message". A malformed or non-object
	// payload is rejected first, with a sanitized 400; see
	// validateAndRejectScheduledPayload for the required order.
	if !s.validateAndRejectScheduledPayload(w, r, req.EventType, req.Payload) {
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

	// Validate cron expression (standard 5-field, UTC-only).
	cronSchedule, err := parseScheduleCron(req.CronExpr)
	if err != nil {
		writeCronParseError(w, err)
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
	originalCronExpr := schedule.CronExpr
	originalEventType := schedule.EventType
	originalPayload := schedule.Payload

	var req UpdateScheduleRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.EventType != "" && req.EventType != "message" && req.EventType != "dispatch_agent" {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: message, dispatch_agent)", req.EventType), nil)
		return
	}
	// effectiveEventType is computed here, ahead of the payload
	// validation block below, so the replacement
	// payload is decoded against whichever event type will actually be
	// stored -- the request's own EventType when it changes it, otherwise
	// the schedule's existing one -- not always re-validated as "message".
	effectiveEventType := schedule.EventType
	if req.EventType != "" {
		effectiveEventType = req.EventType
	}
	// Tombstone a caller-supplied
	// "raw" key in the advanced Payload JSON, for both supported event types
	// — not just "message". Checked whenever the caller supplies a
	// replacement Payload in this request: an update that leaves Payload
	// untouched must not retroactively fail on an existing stored value. A
	// malformed, non-object, or mistyped-for-effectiveEventType replacement
	// payload is rejected first, with a sanitized 400; see
	// validateAndRejectScheduledPayload for the required order.
	if req.Payload != "" {
		if !s.validateAndRejectScheduledPayload(w, r, effectiveEventType, req.Payload) {
			return
		}
	} else if req.EventType != "" && req.EventType != schedule.EventType {
		// The caller is switching EventType
		// without supplying a new Payload, so the existing stored Payload
		// carries forward unchanged but will be reinterpreted as
		// effectiveEventType's shape at fire time. Validate the existing
		// Payload against the new type now, so an incompatible stored
		// payload (e.g. one with a field only valid for the old type) is
		// caught at authoring time instead of failing silently later.
		if !s.validateAndRejectScheduledPayload(w, r, effectiveEventType, schedule.Payload) {
			return
		}
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
	if effectiveEventType == "message" {
		effectivePayload := schedule.Payload
		if req.Payload != "" {
			effectivePayload = req.Payload
		}
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, effectivePayload, "", "") {
			return
		}
	}

	// fields tracks exactly which columns this request changes. UpdateSchedule
	// writes only these — never the rest of the struct — so a column this
	// request doesn't mention can never be reverted to whatever GetSchedule
	// happened to return above, even if that read has since gone stale.
	var fields store.ScheduleFieldMask
	if req.Name != "" {
		schedule.Name = req.Name
		fields.Name = true
	}
	// CronExpr/EventType/Payload are compared against the stored values, not
	// just checked for presence: a request that resends the current value
	// (e.g. a client round-tripping the full resource on every PATCH) is
	// metadata-only, the same as omitting the field. Only an actual change
	// writes the column, which keeps the field mask consistent with
	// changesFutureDispatch below.
	if req.CronExpr != "" && req.CronExpr != originalCronExpr {
		// Validate new cron expression
		cronSchedule, err := parseScheduleCron(req.CronExpr)
		if err != nil {
			writeCronParseError(w, err)
			return
		}
		schedule.CronExpr = req.CronExpr
		nextRunAt := cronSchedule.Next(time.Now().UTC())
		schedule.NextRunAt = &nextRunAt
		fields.CronExpr = true
		fields.NextRunAt = true
	}
	if req.EventType != "" && req.EventType != originalEventType {
		schedule.EventType = req.EventType
		fields.EventType = true
	}
	if req.Payload != "" && req.Payload != originalPayload {
		schedule.Payload = req.Payload
		fields.Payload = true
	}
	if req.Status != "" {
		schedule.Status = req.Status
		fields.Status = true
	}

	// An enable transition (any prior status, e.g. paused, back to active)
	// re-arms future dispatch the same way resumeSchedule does. If this
	// request didn't already recompute NextRunAt via a real CronExpr change,
	// it must be recomputed here from the stored cron: otherwise a schedule
	// that went stale while paused (or was paused with a next_run_at already
	// in the past) would reactivate carrying that stale time, and the
	// scheduler would treat it as immediately due.
	enabling := req.Status == store.ScheduleStatusActive && originalStatus != store.ScheduleStatusActive
	if enabling && !fields.CronExpr {
		// A stored expression that no longer parses (for example one with a
		// zone prefix, which is no longer supported) cannot be enabled; the
		// user must edit it first.
		cronSchedule, err := parseScheduleCron(schedule.CronExpr)
		if err != nil {
			writeCronParseError(w, err)
			return
		}
		nextRunAt := cronSchedule.Next(time.Now().UTC())
		schedule.NextRunAt = &nextRunAt
		fields.NextRunAt = true
	}

	// Ruling Q2: a fully reauthorized mutation that changes future dispatch
	// (payload/target/type/timing, or an enable transition) replaces the
	// attribution and bumps authorization_revision atomically in the same
	// write. A metadata-only edit (name, an unchanged cron/type/payload
	// resent as-is, or a status change other than an enable, e.g. pause)
	// does not re-attribute, and attribution is passed to the store only
	// when it changed. Deriving this from the field mask itself (rather
	// than from req's presence checks) is what keeps the two in sync: a
	// dispatch field only re-attributes when the field mask also writes it.
	changesFutureDispatch := fields.CronExpr || fields.EventType || fields.Payload || enabling

	// The write is conditioned on the revision this handler just read,
	// regardless of whether this particular call replaces attribution: a
	// stale read must never be able to apply any field once a newer,
	// revision-bumping write has landed first. prevRevisionKnown is derived
	// from the same field the store predicates on (AuthorizationRevision),
	// not from AttributionVersion — a schedule can carry a revision while its
	// attribution_version is still NULL (e.g. a historical row), and that
	// combination must remain writable, not permanently conflict.
	prevRevision := schedule.AuthorizationRevision
	prevRevisionKnown := schedule.AuthorizationRevision != 0

	var attribution *store.InitiatorAttribution
	if changesFutureDispatch {
		newAttr := reattributeInitiator(r.Context(), schedule.InitiatorAttribution)
		schedule.InitiatorAttribution = newAttr
		attribution = &newAttr
	}

	if err := s.store.UpdateSchedule(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, attribution); err != nil {
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

	// No future dispatch is created by a delete, so there is no
	// re-attribution — just a record of who deleted it.
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

	// No future dispatch is created by a pause, so there is no
	// re-attribution — just a record of who paused it.
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
	// A stored expression that no longer parses (for example one with a
	// zone prefix, which is no longer supported) cannot be resumed; the user
	// must edit it first.
	cronSchedule, err := parseScheduleCron(schedule.CronExpr)
	if err != nil {
		writeCronParseError(w, err)
		return
	}
	nextRunAt := cronSchedule.Next(time.Now().UTC())

	// Ruling Q2: status, next_run_at and the re-attribution are one write,
	// not two — a failure here must be reported as an error, never as a 200
	// naming an attribution that was never persisted. prevRevisionKnown
	// comes from AuthorizationRevision, the same field the store predicates
	// on, not from AttributionVersion: a schedule can carry a revision while
	// its attribution_version is still NULL, and that combination must
	// remain resumable.
	prevRevision := schedule.AuthorizationRevision
	prevRevisionKnown := schedule.AuthorizationRevision != 0
	schedule.Status = store.ScheduleStatusActive
	schedule.NextRunAt = &nextRunAt
	newAttr := reattributeInitiator(r.Context(), schedule.InitiatorAttribution)
	schedule.InitiatorAttribution = newAttr

	fields := store.ScheduleFieldMask{Status: true, NextRunAt: true}
	if err := s.store.UpdateSchedule(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, &newAttr); err != nil {
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
