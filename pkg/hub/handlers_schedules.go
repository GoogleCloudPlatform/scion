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
// NOTE (O-R2-3): Message targets are specified via AgentName (convenience) or
// raw Payload (advanced). AgentID, Command, and TimeoutSeconds are convenience
// fields for "exec" schedules only (ptone/scion#7884) and are rejected on
// non-exec schedules.
type CreateScheduleRequest struct {
	Name      string `json:"name"`
	CronExpr  string `json:"cronExpr"`
	TimeZone  string `json:"timeZone,omitempty"` // IANA zone for CronExpr; empty or "UTC" means UTC
	EventType string `json:"eventType"`
	Payload   string `json:"payload,omitempty"` // Raw JSON payload (advanced)

	// Convenience fields for "message" events — used to auto-construct Payload
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	// Wake resumes a suspended target before delivery (ptone/scion#7734).
	Wake bool `json:"wake,omitempty"`

	// Convenience fields for "dispatch_agent" events — used to auto-construct Payload
	Template string `json:"template,omitempty"`
	Task     string `json:"task,omitempty"`
	Branch   string `json:"branch,omitempty"`
	NoAuth   bool   `json:"noAuth,omitempty"`

	// Convenience fields for "exec" events (ptone/scion#7884)
	AgentID        string   `json:"agentId,omitempty"`
	Command        []string `json:"command,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
}

// UpdateScheduleRequest is the API request for updating a recurring schedule.
type UpdateScheduleRequest struct {
	Name     string `json:"name,omitempty"`
	CronExpr string `json:"cronExpr,omitempty"`
	// TimeZone is the IANA zone for the cron expression. Absent leaves the
	// stored zone unchanged; "" (or "UTC") sets it back to UTC.
	TimeZone  *string `json:"timeZone,omitempty"`
	EventType string  `json:"eventType,omitempty"`
	Payload   string  `json:"payload,omitempty"`
	Status    string  `json:"status,omitempty"`

	// Convenience fields for "exec" schedules (ptone/scion#7884).
	AgentID        string   `json:"agentId,omitempty"`
	AgentName      string   `json:"agentName,omitempty"`
	Command        []string `json:"command,omitempty"`
	TimeoutSeconds *int     `json:"timeoutSeconds,omitempty"`
}

// ScheduleListScopeCaller indicates that a list response is filtered to rows
// held by the calling principal (an agent caller).
const ScheduleListScopeCaller = "caller"

// ListSchedulesResponse is the API response for listing schedules.
type ListSchedulesResponse struct {
	Scope      string             `json:"scope,omitempty"`
	Schedules  []ScheduleResponse `json:"schedules"`
	NextCursor string             `json:"nextCursor,omitempty"`
	TotalCount int                `json:"totalCount,omitempty"`
	ServerTime time.Time          `json:"serverTime"`
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

	// Determine the authorization action from method and path. authoring
	// is set for the routes that author future work (create, update and
	// resume); pause and delete only stop future runs.
	var authzAction Action
	authoring := false
	if schedulePath == "" {
		switch r.Method {
		case http.MethodGet:
			authzAction = ActionList
		case http.MethodPost:
			authzAction = ActionCreate
			authoring = true
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
				authoring = true
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
			authoring = subAction == "resume"
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

	// The authoring credential gate runs before the permission check and
	// before any schedule lookup or body read.
	if authoring && !authorizeScheduleAuthoringCredential(w, r) {
		return
	}

	// Pause and delete only stop future runs: when the project-level gate
	// refuses them, the user who created the schedule may still do them
	// (scheduleCreatorMayStop). The gate is decided once either way;
	// everything else, and every other caller, gets the gate alone.
	stopOnly := schedulePath != "" &&
		(((len(pathParts) == 1 || pathParts[1] == "") && r.Method == http.MethodDelete) ||
			(len(pathParts) > 1 && pathParts[1] == "pause"))
	var creatorStop func() bool
	// creatorRevision is set when the creator path allowed the request: the
	// authorization revision it checked, which the handler compares with the
	// row it reloads.
	var creatorRevision *int
	if stopOnly {
		creatorStop = func() bool {
			rev, ok := s.scheduleCreatorMayStop(r, projectID, pathParts[0])
			if ok {
				creatorRevision = &rev
			}
			return ok
		}
	}

	// Authorize access — fail closed for all identity types.
	if !s.authorizeScheduledEventRowOr(w, r, projectID, authzAction, nil, creatorStop) {
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
			s.deleteSchedule(w, r, projectID, scheduleID, creatorRevision)
		}
	case "pause":
		s.pauseSchedule(w, r, projectID, scheduleID, creatorRevision)
	case "resume":
		s.resumeSchedule(w, r, projectID, scheduleID)
	case "history":
		s.getScheduleHistory(w, r, projectID, scheduleID)
	}
}

// createSchedule handles POST /api/v1/projects/{projectId}/schedules
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, projectID string) {
	var req CreateScheduleRequest
	r.Body = http.MaxBytesReader(w, r.Body, scheduleRequestBodyLimit)
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
	if !s.scheduledEventTypeSupported(req.EventType) {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: %s)", req.EventType, s.supportedScheduledEventTypes()), nil)
		return
	}
	if req.NoAuth && req.EventType != "dispatch_agent" {
		ValidationError(w, "noAuth applies to dispatch_agent events only", nil)
		return
	}
	if req.EventType == scheduledEventTypeExec {
		s.createExecSchedule(w, r, projectID, &req)
		return
	}
	if req.AgentID != "" || req.Command != nil || req.TimeoutSeconds != 0 {
		ValidationError(w, "agentId, command and timeoutSeconds apply to exec schedules only", nil)
		return
	}
	if req.Wake && req.EventType != "message" {
		ValidationError(w, "wake applies to message events only", nil)
		return
	}
	if req.NoAuth && req.Payload != "" {
		ValidationError(w, "give either payload or noAuth, not both", nil)
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
	// Validate the cron expression (standard 5-field, no zone prefix) and
	// the time zone it is evaluated in (empty means UTC).
	if _, err := parseScheduleCron(req.CronExpr); err != nil {
		writeCronParseError(w, err)
		return
	}
	if !s.checkScheduleFrequency(w, req.CronExpr) {
		return
	}
	timeZone, err := normalizeScheduleTimeZone(req.TimeZone)
	if err != nil {
		writeScheduleTimingError(w, err)
		return
	}

	// Build payload
	payload := req.Payload
	if payload == "" && req.EventType == "dispatch_agent" {
		if req.AgentName == "" {
			ValidationError(w, "agentName is required for dispatch_agent schedules", nil)
			return
		}
		if !templateRefAllowed(req.Template) {
			ValidationError(w, templateRefRefusal, nil)
			return
		}
		p := DispatchAgentEventPayload{
			AgentName: req.AgentName,
			Template:  req.Template,
			Task:      req.Task,
			Branch:    req.Branch,
			NoAuth:    req.NoAuth,
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
			Wake:      req.Wake,
		}
		payloadBytes, marshalErr := json.Marshal(p)
		if marshalErr != nil {
			InternalError(w)
			return
		}
		payload = string(payloadBytes)
	}
	// C1 containment: validate target agent resolution and authorization for
	// message schedules.
	if req.EventType == "message" {
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, req.Payload, "", req.AgentName) {
			return
		}
	}

	// The revision's frozen ceiling is computed before any write.
	ceiling, ok := s.revisionAuthorityCeiling(w, r, projectID, ActionCreate)
	if !ok {
		return
	}

	// Compute next run time
	nextRunAt, err := scheduleNextRun(req.CronExpr, timeZone, time.Now().UTC())
	if err != nil {
		writeScheduleTimingError(w, err)
		return
	}

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
		TimeZone:  timeZone,
		EventType: req.EventType,
		Payload:   payload,
		Status:    store.ScheduleStatusActive,
		NextRunAt: &nextRunAt,
		CreatedBy: createdBy,
		// Record the authoring request's initiator attribution in the same
		// write as the schedule row, together with the credential's frozen
		// ceiling.
		InitiatorAttribution: newInitiatorAttribution(r.Context()),
		AuthorityCeiling:     ceiling,
	}

	// The creation-time provenance stamp comes from the request's
	// principal, never from the request body.
	if err := stampSchedule(r.Context(), s.store, &schedule, fixturePrincipalFromContext(r.Context())); err != nil {
		if !writeFixtureRootError(w, err) {
			writeErrorFromErr(w, err, "")
		}
		return
	}

	// The per-project recurring schedule cap is checked in the same
	// transaction as the insert, under the per-project quota lock.
	maxActive := s.config.ScheduleMaxActivePerProject
	if err := s.store.CreateScheduleChecked(r.Context(), &schedule, 1, func(snap store.ScheduledExecQuotaSnapshot) error {
		return scheduleQuotaCheck(snap, maxActive)
	}); err != nil {
		if writeScheduleQuotaError(w, err) {
			return
		}
		if errors.Is(err, store.ErrAlreadyExists) {
			writeScheduleNameConflict(w, r, req.Name)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Fetch the created schedule to get the full record
	created, err := s.store.GetSchedule(r.Context(), schedule.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, scheduleResponse(s.newExecRevealer(r.Context()), schedule))
		return
	}

	writeJSON(w, http.StatusCreated, scheduleResponse(s.newExecRevealer(r.Context()), *created))
}

// writeScheduleNameConflict writes the 409 Conflict response when creating or
// renaming a recurring schedule collides with an existing (project_id, name).
// The message interpolates only the caller's requested name and never loads or
// discloses any field of the conflicting row (ID, holder, cron, status, or
// payload).
func writeScheduleNameConflict(w http.ResponseWriter, r *http.Request, name string) {
	if isAgentCaller(r.Context()) {
		Conflict(w, fmt.Sprintf(
			"Schedule %q already exists in this project; if it does not appear in 'scion schedule list', it is held by another principal and not visible to this agent",
			name))
		return
	}
	Conflict(w, fmt.Sprintf("Schedule %q already exists in this project", name))
}

// writeScheduleNotFound writes the per-ID 404 response for schedules. Both
// missing IDs (including non-UUID names) and cross-project IDs go through this
// helper so the two cases return the exact same body.
func writeScheduleNotFound(w http.ResponseWriter, r *http.Request) {
	if isAgentCaller(r.Context()) {
		writeError(w, http.StatusNotFound, ErrCodeNotFound,
			"Schedule not found (per-ID schedule endpoints take a schedule ID, not a name; schedules held by another agent or user are hidden from agent lists and return 403 by ID)", nil)
		return
	}
	NotFound(w, "Schedule")
}

func (s *Server) loadScheduleForProject(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) (*store.Schedule, bool) {
	schedule, err := s.store.GetSchedule(r.Context(), scheduleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeScheduleNotFound(w, r)
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if schedule.ProjectID != projectID {
		writeScheduleNotFound(w, r)
		return nil, false
	}
	return schedule, true
}

// listSchedules handles GET /api/v1/projects/{projectId}/schedules
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, projectID string) {
	query := r.URL.Query()
	scope := scheduleListScope(r.Context())

	// An agent caller lists only the schedules it holds.
	agentID, ok := agentListFilter(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, ListSchedulesResponse{
			Scope:      scope,
			Schedules:  []ScheduleResponse{},
			ServerTime: time.Now().UTC(),
		})
		return
	}
	filter := store.ScheduleFilter{
		ProjectID:        projectID,
		Status:           query.Get("status"),
		Name:             query.Get("name"),
		InitiatorAgentID: agentID,
	}

	result, err := s.store.ListSchedules(r.Context(), filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	schedules, totalCount := visibleScheduledRows(r.Context(), s, projectID, result.Items, result.TotalCount, scheduleRow)
	writeJSON(w, http.StatusOK, ListSchedulesResponse{
		Scope:      scope,
		Schedules:  scheduleResponses(s.newExecRevealer(r.Context()), schedules),
		NextCursor: result.NextCursor,
		TotalCount: totalCount,
		ServerTime: time.Now().UTC(),
	})
}

// getSchedule handles GET /api/v1/projects/{projectId}/schedules/{id}
func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, ok := s.loadScheduleForProject(w, r, projectID, scheduleID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionRead, scheduleRow(schedule)) {
		return
	}

	writeJSON(w, http.StatusOK, scheduleResponse(s.newExecRevealer(r.Context()), *schedule))
}

// updateSchedule handles PATCH /api/v1/projects/{projectId}/schedules/{id}
func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, ok := s.loadScheduleForProject(w, r, projectID, scheduleID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionUpdate, scheduleEditRow(schedule)) {
		return
	}
	originalStatus := schedule.Status
	originalCronExpr := schedule.CronExpr
	originalTimeZone := schedule.TimeZone
	originalEventType := schedule.EventType
	originalPayload := schedule.Payload

	var req UpdateScheduleRequest
	r.Body = http.MaxBytesReader(w, r.Body, scheduleRequestBodyLimit)
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.EventType != "" && !s.scheduledEventTypeSupported(req.EventType) {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: %s)", req.EventType, s.supportedScheduledEventTypes()), nil)
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
	hasExecFields := req.AgentID != "" || req.AgentName != "" || req.Command != nil || req.TimeoutSeconds != nil
	isExecRow := originalEventType == scheduledEventTypeExec || effectiveEventType == scheduledEventTypeExec
	if isExecRow && !s.execSchedulesEnabled() {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: %s)", scheduledEventTypeExec, s.supportedScheduledEventTypes()), nil)
		return
	}
	if req.EventType != "" && req.EventType != originalEventType && isExecRow {
		ValidationError(w, "eventType cannot be changed to or from exec", nil)
		return
	}
	if effectiveEventType != scheduledEventTypeExec && hasExecFields {
		ValidationError(w, "agentId, agentName, command and timeoutSeconds apply to exec schedules only", nil)
		return
	}
	// Tombstone a caller-supplied
	// "raw" key in the advanced Payload JSON, for all supported event types
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
	} else if req.EventType != "" && req.EventType != schedule.EventType && !hasExecFields {
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
	// The time zone works the same way: absent or equal to the stored
	// value (after normalizing "UTC" to "") is no change. A change to
	// either the expression or the zone recomputes NextRunAt from both.
	cronChanged := req.CronExpr != "" && req.CronExpr != originalCronExpr
	zoneChanged := false
	newTimeZone := originalTimeZone
	if req.TimeZone != nil {
		tz, err := normalizeScheduleTimeZone(*req.TimeZone)
		if err != nil {
			writeScheduleTimingError(w, err)
			return
		}
		newTimeZone = tz
		zoneChanged = tz != originalTimeZone
	}
	if cronChanged || zoneChanged {
		newCronExpr := originalCronExpr
		if cronChanged {
			newCronExpr = req.CronExpr
		}
		nextRunAt, err := scheduleNextRun(newCronExpr, newTimeZone, time.Now().UTC())
		if err != nil {
			writeScheduleTimingError(w, err)
			return
		}
		if cronChanged {
			schedule.CronExpr = newCronExpr
			fields.CronExpr = true
		}
		if zoneChanged {
			schedule.TimeZone = newTimeZone
			fields.TimeZone = true
		}
		schedule.NextRunAt = &nextRunAt
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
		// A status written by a user carries no system reason: the write
		// clears any stored status_reason.
		schedule.Status = req.Status
		schedule.StatusReason = store.ScheduleStatusReasonNone
		fields.Status = true
	}

	// An enable transition (any prior status, e.g. paused, back to active)
	// re-arms future dispatch the same way resumeSchedule does. If this
	// request didn't already recompute NextRunAt via a real CronExpr or
	// TimeZone change, it must be recomputed here from the stored cron
	// expression and time zone: otherwise a schedule that went stale while
	// paused (or was paused with a next_run_at already in the past) would
	// reactivate carrying that stale time, and the scheduler would treat it
	// as immediately due.
	enabling := req.Status == store.ScheduleStatusActive && originalStatus != store.ScheduleStatusActive
	if enabling && !fields.NextRunAt {
		// A stored expression that no longer parses (for example one with a
		// zone prefix, which is no longer supported) cannot be enabled; the
		// user must edit it first.
		nextRunAt, err := scheduleNextRun(schedule.CronExpr, schedule.TimeZone, time.Now().UTC())
		if err != nil {
			writeScheduleTimingError(w, err)
			return
		}
		schedule.NextRunAt = &nextRunAt
		fields.NextRunAt = true
	}

	// Ruling Q2: a fully reauthorized mutation that changes future dispatch
	// (payload/target/type/timing including the time zone, or an enable
	// transition) replaces the attribution and bumps authorization_revision
	// atomically in the same write. A metadata-only edit (name, an
	// unchanged cron/type/payload resent as-is, or a status change other
	// than an enable, e.g. pause) does not re-attribute, and attribution is
	// passed to the store only when it changed. Deriving this from the field
	// mask itself (rather than from req's presence checks) is what keeps the
	// two in sync: a dispatch field only re-attributes when the field mask
	// also writes it.
	changesFutureDispatch := fields.CronExpr || fields.TimeZone || fields.EventType || fields.Payload || enabling

	// C1 containment / ptone/scion#8588: validate message target resolution and
	// authorization only for dispatch-changing updates (the revision-bump list:
	// cron, time zone, event type, payload, or re-enabling). Metadata-only
	// edits (such as name-only PATCH or pausing) stay accepted regardless of
	// target state.
	if effectiveEventType == "message" && changesFutureDispatch {
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, schedule.Payload, "", "") {
			return
		}
	}

	// Any exec PATCH that supplies payload/exec fields or changes future
	// dispatch runs full exec authoring (live agent.attach, managed runtime,
	// rate limit, and active quota) before comparing encoded payload to
	// originalPayload, so callers without attach get no argv-guessing oracle.
	var execPayload *scheduledExecPayload
	if isExecRow && (req.Payload != "" || hasExecFields || changesFutureDispatch) {
		var encoded string
		var ok bool
		execPayload, encoded, ok = s.prepareExecScheduleUpdate(w, r, projectID, schedule, originalEventType, originalPayload, &req, hasExecFields)
		if !ok {
			return
		}
		if encoded != originalPayload {
			schedule.Payload = encoded
			fields.Payload = true
		} else {
			schedule.Payload = originalPayload
			fields.Payload = false
		}
		changesFutureDispatch = fields.CronExpr || fields.TimeZone || fields.EventType || fields.Payload || enabling
	}

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
		// The store writes this ceiling in the same conditional update as
		// the attribution and the revision bump.
		ceiling, ok := s.revisionAuthorityCeiling(w, r, projectID, ActionUpdate)
		if !ok {
			return
		}
		schedule.AuthorityCeiling = ceiling
		newAttr := reattributeInitiator(r.Context(), schedule.InitiatorAttribution)
		schedule.InitiatorAttribution = newAttr
		attribution = &newAttr
	}

	// A schedule that is (re)armed with a new or re-enabled cron expression
	// must respect the hub's minimum interval.
	if (fields.CronExpr || enabling) && schedule.Status == store.ScheduleStatusActive &&
		!s.checkScheduleFrequency(w, schedule.CronExpr) {
		return
	}

	var updateErr error
	maxActive := s.config.ScheduleMaxActivePerProject
	if effectiveEventType == scheduledEventTypeExec && schedule.Status == store.ScheduleStatusActive && execPayload != nil {
		targetID := execPayload.AgentID
		scheduleID := schedule.ID
		updateErr = s.store.UpdateScheduleChecked(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, attribution,
			scheduledExecMaxPerProject+1, func(snap store.ScheduledExecQuotaSnapshot) error {
				if err := scheduledExecQuotaCheck(snap, targetID, scheduleID); err != nil {
					return err
				}
				if !enabling {
					return nil
				}
				return scheduleQuotaCheck(snap, maxActive)
			})
	} else if enabling {
		// Re-enabling counts against the per-project recurring cap.
		updateErr = s.store.UpdateScheduleChecked(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, attribution,
			1, func(snap store.ScheduledExecQuotaSnapshot) error {
				return scheduleQuotaCheck(snap, maxActive)
			})
	} else {
		updateErr = s.store.UpdateSchedule(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, attribution)
	}
	if updateErr != nil {
		if writeScheduleQuotaError(w, updateErr) {
			return
		}
		if errors.Is(updateErr, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"schedule was concurrently modified; refresh and retry", nil)
			return
		}
		if errors.Is(updateErr, store.ErrAlreadyExists) {
			writeScheduleNameConflict(w, r, schedule.Name)
			return
		}
		writeErrorFromErr(w, updateErr, "")
		return
	}

	if schedule.EventType == scheduledEventTypeExec {
		s.auditScheduleExecMutation(r.Context(), ScheduledExecAuditUpdate, *schedule)
	} else if originalEventType == scheduledEventTypeExec {
		prev := *schedule
		prev.Payload = originalPayload
		s.auditScheduleExecMutation(r.Context(), ScheduledExecAuditUpdate, prev)
	}

	writeJSON(w, http.StatusOK, scheduleResponse(s.newExecRevealer(r.Context()), *schedule))
}

// deleteSchedule handles DELETE /api/v1/projects/{projectId}/schedules/{id}.
// creatorRevision is non-nil when the creator path allowed it (see
// creatorStopCurrent).
func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string, creatorRevision *int) {
	schedule, ok := s.loadScheduleForProject(w, r, projectID, scheduleID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionDelete, scheduleRow(schedule)) {
		return
	}
	if !creatorStopCurrent(w, schedule, creatorRevision) {
		return
	}

	// No future dispatch is created by a delete, so there is no
	// re-attribution — just a record of who deleted it, written in the same
	// transaction as the delete.
	audit := newScheduleAudit(r.Context(), mutationTypeScheduleDelete, scheduleID)
	if err := s.store.WithTx(r.Context(), func(tx store.Store) error {
		if creatorRevision != nil {
			if err := creatorStopGuard(r.Context(), tx, schedule, *creatorRevision); err != nil {
				return err
			}
		}
		if err := tx.DeleteSchedule(r.Context(), scheduleID); err != nil {
			return err
		}
		return tx.CreateMutationAudit(r.Context(), audit)
	}); err != nil {
		if creatorRevision != nil && errors.Is(err, store.ErrRevisionConflict) {
			writeCreatorStopConflict(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if schedule.EventType == scheduledEventTypeExec {
		s.auditScheduleExecMutation(r.Context(), ScheduledExecAuditDelete, *schedule)
	}
	w.WriteHeader(http.StatusNoContent)
}

// pauseSchedule handles POST /api/v1/projects/{projectId}/schedules/{id}/pause.
// creatorRevision is non-nil when the creator path allowed it (see
// creatorStopCurrent).
func (s *Server) pauseSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string, creatorRevision *int) {
	schedule, ok := s.loadScheduleForProject(w, r, projectID, scheduleID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionUpdate, scheduleRow(schedule)) {
		return
	}
	if !creatorStopCurrent(w, schedule, creatorRevision) {
		return
	}
	if schedule.Status != store.ScheduleStatusActive {
		ValidationError(w, "only active schedules can be paused", nil)
		return
	}

	// No future dispatch is created by a pause, so there is no
	// re-attribution — just a record of who paused it, written in the same
	// transaction as the status change.
	audit := newScheduleAudit(r.Context(), mutationTypeSchedulePause, scheduleID)
	if err := s.store.WithTx(r.Context(), func(tx store.Store) error {
		if creatorRevision != nil {
			// The creator path checked this revision: pause only while it
			// is still current, so a re-authorization since then wins.
			paused := *schedule
			paused.Status, paused.StatusReason = store.ScheduleStatusPaused, store.ScheduleStatusReasonNone
			if err := tx.UpdateSchedule(r.Context(), &paused, store.ScheduleFieldMask{Status: true}, *creatorRevision, true, nil); err != nil {
				return err
			}
		} else if err := tx.UpdateScheduleStatus(r.Context(), scheduleID, store.ScheduleStatusPaused, store.ScheduleStatusReasonNone); err != nil {
			return err
		}
		return tx.CreateMutationAudit(r.Context(), audit)
	}); err != nil {
		if creatorRevision != nil && errors.Is(err, store.ErrRevisionConflict) {
			writeCreatorStopConflict(w)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	schedule.Status, schedule.StatusReason = store.ScheduleStatusPaused, store.ScheduleStatusReasonNone
	if schedule.EventType == scheduledEventTypeExec {
		s.auditScheduleExecMutation(r.Context(), ScheduledExecAuditPause, *schedule)
	}
	writeJSON(w, http.StatusOK, scheduleResponse(s.newExecRevealer(r.Context()), *schedule))
}

// resumeSchedule handles POST /api/v1/projects/{projectId}/schedules/{id}/resume
func (s *Server) resumeSchedule(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, ok := s.loadScheduleForProject(w, r, projectID, scheduleID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionUpdate, scheduleRow(schedule)) {
		return
	}
	// Resuming re-arms future dispatch and records the resumer as the
	// revision's authority, so the resumer is re-authorized as the author
	// of the stored schedule, for every event type, before any write. On a
	// denial the attribution, ceiling and revision are left unchanged.
	var execPayload *scheduledExecPayload
	switch schedule.EventType {
	case "dispatch_agent":
		if !s.authorizeScheduledDispatchAgentAuthoring(w, r) {
			return
		}
		if !s.authorizeAgentCreate(w, r, projectID) {
			return
		}
	case "message":
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, schedule.Payload, "", "") {
			return
		}
	case scheduledEventTypeExec:
		if !s.execSchedulesEnabled() {
			ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: %s)", schedule.EventType, s.supportedScheduledEventTypes()), nil)
			return
		}
		var err error
		execPayload, err = decodeScheduledExecPayload(schedule.Payload)
		if err != nil {
			BadRequest(w, err.Error())
			return
		}
		if execPayload.AgentID != "" {
			execPayload.AgentName = ""
		}
		if _, ok := s.authorizeScheduledExecAuthoringOpts(w, r, projectID, execPayload, schedule.ID, true); !ok {
			return
		}
	default:
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: %s)", schedule.EventType, s.supportedScheduledEventTypes()), nil)
		return
	}
	if schedule.Status != store.ScheduleStatusPaused {
		ValidationError(w, "only paused schedules can be resumed", nil)
		return
	}

	// Recompute next run time
	// A stored expression that no longer parses (for example one with a
	// zone prefix, which is no longer supported) cannot be resumed; the user
	// must edit it first.
	nextRunAt, err := scheduleNextRun(schedule.CronExpr, schedule.TimeZone, time.Now().UTC())
	if err != nil {
		writeScheduleTimingError(w, err)
		return
	}
	// A schedule created before the hub's minimum interval (or under a
	// lower one) must be edited before it can be resumed.
	if !s.checkScheduleFrequency(w, schedule.CronExpr) {
		return
	}

	ceiling, ok := s.revisionAuthorityCeiling(w, r, projectID, ActionUpdate)
	if !ok {
		return
	}

	// Status, next_run_at, the re-attribution and its ceiling are one write,
	// not two — a failure here must be reported as an error, never as a 200
	// naming an attribution that was never persisted. prevRevisionKnown
	// comes from AuthorizationRevision, the same field the store predicates
	// on, not from AttributionVersion: a schedule can carry a revision while
	// its attribution_version is still NULL, and that combination must
	// remain resumable.
	prevRevision := schedule.AuthorizationRevision
	prevRevisionKnown := schedule.AuthorizationRevision != 0
	schedule.Status = store.ScheduleStatusActive
	// Resuming clears any system pause reason (for example
	// creator_deleted) in the same write that re-records the authority.
	schedule.StatusReason = store.ScheduleStatusReasonNone
	schedule.NextRunAt = &nextRunAt
	newAttr := reattributeInitiator(r.Context(), schedule.InitiatorAttribution)
	schedule.InitiatorAttribution = newAttr
	schedule.AuthorityCeiling = ceiling

	fields := store.ScheduleFieldMask{Status: true, NextRunAt: true}
	maxActive := s.config.ScheduleMaxActivePerProject
	var resumeErr error
	if execPayload != nil {
		if encoded, encErr := execPayload.encode(); encErr == nil && encoded != schedule.Payload {
			schedule.Payload = encoded
			fields.Payload = true
		}
		targetID := execPayload.AgentID
		resumeErr = s.store.UpdateScheduleChecked(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, &newAttr,
			scheduledExecMaxPerProject+1, func(snap store.ScheduledExecQuotaSnapshot) error {
				if err := scheduledExecQuotaCheck(snap, targetID, scheduleID); err != nil {
					return err
				}
				return scheduleQuotaCheck(snap, maxActive)
			})
	} else {
		resumeErr = s.store.UpdateScheduleChecked(r.Context(), schedule, fields, prevRevision, prevRevisionKnown, &newAttr,
			1, func(snap store.ScheduledExecQuotaSnapshot) error {
				return scheduleQuotaCheck(snap, maxActive)
			})
	}
	if resumeErr != nil {
		if writeScheduleQuotaError(w, resumeErr) {
			return
		}
		if errors.Is(resumeErr, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
				"schedule was concurrently modified; refresh and retry", nil)
			return
		}
		writeErrorFromErr(w, resumeErr, "")
		return
	}

	if execPayload != nil {
		s.auditScheduledExec(r.Context(), newScheduleExecAuthoringAudit(r.Context(), ScheduledExecAuditResume, *schedule, execPayload))
	}
	writeJSON(w, http.StatusOK, scheduleResponse(s.newExecRevealer(r.Context()), *schedule))
}

// getScheduleHistory handles GET /api/v1/projects/{projectId}/schedules/{id}/history
func (s *Server) getScheduleHistory(w http.ResponseWriter, r *http.Request, projectID, scheduleID string) {
	schedule, ok := s.loadScheduleForProject(w, r, projectID, scheduleID)
	if !ok {
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionRead, scheduleRow(schedule)) {
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
		Events:     s.scheduledEventResponses(r.Context(), s.newExecRevealer(r.Context()), result.Items),
		NextCursor: result.NextCursor,
		TotalCount: result.TotalCount,
		ServerTime: time.Now().UTC(),
	})
}
