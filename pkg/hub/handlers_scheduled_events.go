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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CreateScheduledEventRequest is the API request for creating a scheduled event.
type CreateScheduledEventRequest struct {
	EventType string `json:"eventType"`         // Required: "message" or "dispatch_agent"
	FireAt    string `json:"fireAt,omitempty"`  // ISO 8601 absolute time
	FireIn    string `json:"fireIn,omitempty"`  // Duration string (e.g. "30m")
	Payload   string `json:"payload,omitempty"` // Raw JSON payload (advanced)

	// Convenience fields for "message" events — used to auto-construct Payload
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Plain     bool   `json:"plain,omitempty"`

	// There is no top-level raw convenience field. This request surface
	// never accepted one: a "raw" key here is an unknown field that JSON
	// decoding ignores, and the "message" auto-construct path below reads
	// only AgentID/AgentName/Message/Interrupt/Plain. The caller-supplied
	// Payload is different: callers write that JSON directly, so a "raw"
	// key inside it is rejected with raw_input_removed
	// (validateAndRejectScheduledPayload).

	// Convenience fields for "dispatch_agent" events — used to auto-construct Payload
	Template string `json:"template,omitempty"`
	Task     string `json:"task,omitempty"`
	Branch   string `json:"branch,omitempty"`
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

// authorizeScheduledEventAccess is the project-level gate the scheduled-event
// and schedule routers run before any row lookup. It fails closed for every
// caller kind. See scheduledEventAccessDecision for the rule.
func (s *Server) authorizeScheduledEventAccess(w http.ResponseWriter, r *http.Request, projectID string, action Action) bool {
	return s.authorizeScheduledEventRow(w, r, projectID, action, nil)
}

// authorizeScheduledEventRow decides action on one scheduled row (nil for
// the project-level gate) and writes the 403 for a denial. Per-ID handlers
// call it with the row they loaded, after their project check and before
// any other work.
func (s *Server) authorizeScheduledEventRow(w http.ResponseWriter, r *http.Request, projectID string, action Action, row *scheduledEventRow) bool {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return false
	}
	if row != nil && identity.Type() != "agent" {
		// Only an agent caller's decision depends on the row. For every
		// other caller the router gate already decided this action for
		// this request, so it is not decided (or audited) a second time.
		return true
	}
	d := s.scheduledEventAccessDecision(r.Context(), projectID, action, row)
	if d.allowed {
		return true
	}
	logAuthzDenial(r, identity, scheduledEventResource(projectID), action, d.reason)
	if d.byPolicy {
		writeForbidden(w, "You don't have permission to access scheduled events in this project")
	} else {
		writeForbidden(w, "")
	}
	return false
}

// scheduledEventAccess is the outcome of scheduledEventAccessDecision.
type scheduledEventAccess struct {
	allowed bool
	// reason is logged for a denial.
	reason string
	// byPolicy is true when the authorization service denied the request,
	// the one denial authorizeScheduledEventRow explains to the caller.
	byPolicy bool
}

// scheduledEventRow is the row a scheduled-event decision is about: a
// schedule or a scheduled event, by its current initiator attribution.
type scheduledEventRow struct {
	attribution store.InitiatorAttribution
	// edit marks a PATCH of a schedule definition. Pause and resume share
	// ActionUpdate with it, so agentScheduleEditAllowed keys on this flag.
	edit bool
}

func scheduleRow(sc *store.Schedule) *scheduledEventRow {
	return &scheduledEventRow{attribution: sc.InitiatorAttribution}
}

func scheduleEditRow(sc *store.Schedule) *scheduledEventRow {
	return &scheduledEventRow{attribution: sc.InitiatorAttribution, edit: true}
}

func scheduledEventRowOf(evt *store.ScheduledEvent) *scheduledEventRow {
	return &scheduledEventRow{attribution: evt.InitiatorAttribution}
}

// agentScheduleEditAllowed is the single switch for whether an agent caller
// may PATCH a schedule definition it holds. Agent callers do not edit
// schedule definitions; a user edits them.
const agentScheduleEditAllowed = false

// scheduledEventResource is the resource a scheduled event decision is
// about: scheduled events under projectID.
func scheduledEventResource(projectID string) Resource {
	return Resource{
		Type:       "scheduled_event",
		ParentType: "project",
		ParentID:   projectID,
	}
}

// scheduledEventAccessDecision decides whether the identity in ctx may
// perform action on the scheduled events and schedules of projectID,
// without writing a response. It fails closed. Every router, list filter
// and the health summary decide through it.
//
// A user caller is decided by the authorization service on
// scheduled_event.<action>; row is not consulted.
//
// An agent caller manages only the rows it holds authority for: the row's
// current initiator principal (scheduledInitiator, never CreatedBy) must
// be that agent. A LegacyUnknown row is held by no agent. With row nil
// (the project-level gate the routers run before a lookup) an agent is let
// through: create keeps its own checks, and every other agent request is
// decided again with its row before a row is returned or changed. Agent
// project isolation and read scope are enforced on the request path (see
// handleSchedules and healthSummaryCanReadSchedules).
func (s *Server) scheduledEventAccessDecision(ctx context.Context, projectID string, action Action, row *scheduledEventRow) scheduledEventAccess {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return scheduledEventAccess{reason: "no identity"}
	}
	switch identity.Type() {
	case "agent":
		if row == nil || action == ActionCreate {
			return scheduledEventAccess{allowed: true}
		}
		if row.edit && !agentScheduleEditAllowed {
			return scheduledEventAccess{reason: "agent callers do not edit schedule definitions"}
		}
		if !s.agentHoldsScheduledRow(identity, row.attribution) {
			return scheduledEventAccess{reason: "agent does not hold authority for this scheduled row"}
		}
		return scheduledEventAccess{allowed: true}

	case "user", "dev", "federated_user":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return scheduledEventAccess{reason: "invalid user identity"}
		}
		if s.authzService == nil {
			return scheduledEventAccess{reason: "authorization service is uninitialized"}
		}
		// Pass the canonical permission ID so the kernel evaluates
		// scheduled_event permissions through system-scoped role bindings.
		permissionID := "scheduled_event." + string(action)
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   scheduledEventResource(projectID),
			Action:     action,
			Permission: permissionID,
		})
		if !decision.Allowed {
			return scheduledEventAccess{reason: decision.Reason, byPolicy: true}
		}
		return scheduledEventAccess{allowed: true}

	default:
		return scheduledEventAccess{reason: "identity type may not access scheduled events"}
	}
}

// agentHoldsScheduledRow reports whether agent is the current initiator
// principal of a scheduled row. A LegacyUnknown row is held by no agent.
func (s *Server) agentHoldsScheduledRow(agent Identity, attr store.InitiatorAttribution) bool {
	initiator := s.scheduledInitiator(attr)
	return !initiator.LegacyUnknown &&
		initiator.PrincipalKind == "agent" &&
		initiator.PrincipalID != "" &&
		initiator.PrincipalID == agent.ID()
}

// isAgentCaller reports whether the identity in ctx is an agent.
func isAgentCaller(ctx context.Context) bool {
	id := GetIdentityFromContext(ctx)
	return id != nil && id.Type() == "agent"
}

// handleScheduledEvents routes requests under /api/v1/projects/{projectId}/scheduled-events[/{id}]
func (s *Server) handleScheduledEvents(w http.ResponseWriter, r *http.Request, projectID, eventPath string) {
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

	// Determine the action for authorization based on method and path.
	var action Action
	if eventPath == "" {
		switch r.Method {
		case http.MethodGet:
			action = ActionList
		case http.MethodPost:
			action = ActionCreate
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
			return
		}
	} else {
		switch r.Method {
		case http.MethodGet:
			action = ActionRead
		case http.MethodDelete:
			action = ActionDelete
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
			return
		}
	}

	// Creating a scheduled event authors future work: the credential gate
	// runs before the permission check and before the body is read.
	if action == ActionCreate && !authorizeScheduleAuthoringCredential(w, r) {
		return
	}

	// Authorize access — fail closed for all identity types.
	if !s.authorizeScheduledEventAccess(w, r, projectID, action) {
		return
	}

	// Dispatch to handler — method filtering is done in the authorization
	// block above; only valid methods reach this point.
	if eventPath == "" {
		switch r.Method {
		case http.MethodGet:
			s.listScheduledEvents(w, r, projectID)
		case http.MethodPost:
			s.createScheduledEvent(w, r, projectID)
		}
		return
	}

	// Individual event endpoint
	eventID := eventPath
	switch r.Method {
	case http.MethodGet:
		s.getScheduledEvent(w, r, projectID, eventID)
	case http.MethodDelete:
		s.cancelScheduledEvent(w, r, projectID, eventID)
	}
}

// createScheduledEvent handles POST /api/v1/projects/{projectId}/scheduled-events
func (s *Server) createScheduledEvent(w http.ResponseWriter, r *http.Request, projectID string) {
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
	if req.EventType != "message" && req.EventType != "dispatch_agent" {
		ValidationError(w, fmt.Sprintf("unsupported event type: %s (supported: message, dispatch_agent)", req.EventType), nil)
		return
	}
	// The payload "raw" tombstone applies to both supported event types:
	// the advanced Payload field is accepted verbatim for "dispatch_agent"
	// too, and neither payload type has a raw field. A "raw" key (any case,
	// any value including false/null) is rejected with 422
	// raw_input_removed before storage and before either event type's own
	// authorization runs. A malformed or non-object payload is rejected
	// first, with a sanitized 400; see validateAndRejectScheduledPayload.
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
	// C1 containment: for message events, validate the target agent belongs to
	// this project and the caller is authorized to message it. Scheduled messages
	// are request-derived (not system-plane) and must pass the same authorization
	// as direct sends — both at authoring and again at fire time.
	if req.EventType == "message" {
		if !s.authorizeScheduledMessageAuthoring(w, r, projectID, req.Payload, req.AgentID, req.AgentName) {
			return
		}
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

	// The revision's frozen ceiling is computed before any write.
	ceiling, ok := s.revisionAuthorityCeiling(w, r, projectID, ActionCreate)
	if !ok {
		return
	}

	// Determine creator identity
	createdBy := ""
	if identity := GetIdentityFromContext(r.Context()); identity != nil {
		createdBy = identity.ID()
	}

	evt := store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: req.EventType,
		FireAt:    fireAt,
		Payload:   payload,
		Status:    store.ScheduledEventPending,
		CreatedBy: createdBy,
		// Record the authoring request's initiator attribution in the same
		// write as the event row (atomic by construction, since
		// ScheduleEvent below issues a single insert), together with the
		// credential's frozen ceiling.
		InitiatorAttribution: newInitiatorAttribution(r.Context()),
		AuthorityCeiling:     ceiling,
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

// listScheduledEvents handles GET /api/v1/projects/{projectId}/scheduled-events
func (s *Server) listScheduledEvents(w http.ResponseWriter, r *http.Request, projectID string) {
	query := r.URL.Query()

	filter := store.ScheduledEventFilter{
		ProjectID: projectID,
		EventType: query.Get("eventType"),
		Status:    query.Get("status"),
		// An agent caller lists only the events it holds.
		InitiatorAgentID: agentListFilterID(r.Context()),
	}

	result, err := s.store.ListScheduledEvents(r.Context(), filter, listOptionsFromQuery(query))
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	events, totalCount := visibleScheduledRows(r.Context(), s, projectID, result.Items, result.TotalCount, scheduledEventRowOf)
	writeJSON(w, http.StatusOK, ListScheduledEventsResponse{
		Events:     events,
		NextCursor: result.NextCursor,
		TotalCount: totalCount,
		ServerTime: time.Now().UTC(),
	})
}

// visibleScheduledRows keeps the rows of one list page that the caller may
// see, each decided by scheduledEventAccessDecision for ActionList. A user
// caller's decision does not depend on the row, so its page and count are
// returned as listed. An agent caller's list is already narrowed in the
// store to the rows it holds (agentListFilterID); the decision is still made
// per row, and if it drops any row the store's count is omitted.
func visibleScheduledRows[T any](ctx context.Context, s *Server, projectID string, items []T, totalCount int, rowOf func(*T) *scheduledEventRow) ([]T, int) {
	if !isAgentCaller(ctx) {
		return items, totalCount
	}
	visible := make([]T, 0, len(items))
	for i := range items {
		if s.scheduledEventAccessDecision(ctx, projectID, ActionList, rowOf(&items[i])).allowed {
			visible = append(visible, items[i])
		}
	}
	if len(visible) != len(items) {
		totalCount = 0
	}
	return visible, totalCount
}

// agentListFilterID is the agent ID an agent caller's list is narrowed to in
// the store, so its pages fill with the rows it holds; "" for other callers.
func agentListFilterID(ctx context.Context) string {
	if !isAgentCaller(ctx) {
		return ""
	}
	return GetIdentityFromContext(ctx).ID()
}

// getScheduledEvent handles GET /api/v1/projects/{projectId}/scheduled-events/{id}
func (s *Server) getScheduledEvent(w http.ResponseWriter, r *http.Request, projectID, eventID string) {
	evt, err := s.store.GetScheduledEvent(r.Context(), eventID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the event belongs to the requested project
	if evt.ProjectID != projectID {
		NotFound(w, "Scheduled event")
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionRead, scheduledEventRowOf(evt)) {
		return
	}

	writeJSON(w, http.StatusOK, evt)
}

// cancelScheduledEvent handles DELETE /api/v1/projects/{projectId}/scheduled-events/{id}
func (s *Server) cancelScheduledEvent(w http.ResponseWriter, r *http.Request, projectID, eventID string) {
	// Verify event exists and belongs to the project
	evt, err := s.store.GetScheduledEvent(r.Context(), eventID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if evt.ProjectID != projectID {
		NotFound(w, "Scheduled event")
		return
	}
	if !s.authorizeScheduledEventRow(w, r, projectID, ActionDelete, scheduledEventRowOf(evt)) {
		return
	}

	// No future dispatch remains after a cancel, so there is no
	// re-attribution — just a record of who cancelled it, written in the
	// same transaction as the cancel. The in-memory timer is stopped only
	// once the cancel commits, so a failed cancel leaves the event pending
	// and armed.
	audit := newScheduledEventAudit(r.Context(), mutationTypeScheduledEventCancel, eventID)
	if err := s.store.WithTx(r.Context(), func(tx store.Store) error {
		if err := tx.CancelScheduledEvent(r.Context(), eventID); err != nil {
			return err
		}
		return tx.CreateMutationAudit(r.Context(), audit)
	}); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if s.scheduler != nil {
		s.scheduler.StopEventTimer(eventID)
	}

	w.WriteHeader(http.StatusNoContent)
}
