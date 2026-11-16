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
	"errors"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// scheduledEventTypeSupported reports whether one-shot scheduled events and
// recurring schedules of eventType may be authored. exec is supported only
// while exec schedules are enabled (hub.exec_schedules and hub.conduit, D8);
// otherwise it is answered exactly like any unknown type.
func (s *Server) scheduledEventTypeSupported(eventType string) bool {
	switch eventType {
	case "message", "dispatch_agent":
		return true
	case scheduledEventTypeExec:
		return s.execSchedulesEnabled()
	}
	return false
}

// supportedScheduledEventTypes names the supported event types for the
// unsupported-type message.
func (s *Server) supportedScheduledEventTypes() string {
	if s.execSchedulesEnabled() {
		return "message, dispatch_agent, exec"
	}
	return "message, dispatch_agent"
}

// createScheduledExecEvent creates a one-shot exec event (ptone/scion#7883).
// The router has already run the authoring credential gate and the
// scheduled_event.create check; this adds the exec payload validation and
// the exec authoring decision (authorizeScheduledExecAuthoring), records the
// revision's ceiling, and audits the create. Nothing is stored on a refusal.
func (s *Server) createScheduledExecEvent(w http.ResponseWriter, r *http.Request, projectID string, req *CreateScheduledEventRequest) {
	if req.Message != "" || req.Interrupt || req.Plain || req.Wake || req.Template != "" || req.Task != "" || req.Branch != "" {
		ValidationError(w, "exec events take agentId or agentName, command and timeoutSeconds only", nil)
		return
	}

	var p *scheduledExecPayload
	if req.Payload != "" {
		if len(req.Command) > 0 || req.TimeoutSeconds != 0 || req.AgentID != "" || req.AgentName != "" {
			ValidationError(w, "give either payload or the exec convenience fields, not both", nil)
			return
		}
		if !s.validateAndRejectScheduledPayload(w, r, req.EventType, req.Payload) {
			return
		}
		var err error
		if p, err = decodeScheduledExecPayload(req.Payload); err != nil {
			BadRequest(w, err.Error())
			return
		}
	} else {
		p = &scheduledExecPayload{
			AgentID:        req.AgentID,
			AgentName:      req.AgentName,
			Command:        req.Command,
			TimeoutSeconds: req.TimeoutSeconds,
		}
		if err := p.validate(); err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
	}

	fireAt, ok := parseScheduledFireTime(w, req)
	if !ok {
		return
	}

	if _, ok := s.authorizeScheduledExecAuthoring(w, r, projectID, p); !ok {
		return
	}

	payload, err := p.encode()
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	ceiling, ok := s.revisionAuthorityCeiling(w, r, projectID, ActionCreate)
	if !ok {
		return
	}

	createdBy := ""
	if identity := GetIdentityFromContext(r.Context()); identity != nil {
		createdBy = identity.ID()
	}
	evt := store.ScheduledEvent{
		ID:                   api.NewUUID(),
		ProjectID:            projectID,
		EventType:            scheduledEventTypeExec,
		FireAt:               fireAt,
		Payload:              payload,
		Status:               store.ScheduledEventPending,
		CreatedBy:            createdBy,
		InitiatorAttribution: newInitiatorAttribution(r.Context()),
		AuthorityCeiling:     ceiling,
	}
	// The quota is checked again atomically with the insert, under the
	// per-project lock (R4): concurrent authors on any replica cannot
	// exceed it.
	targetID := p.AgentID
	if err := s.scheduler.ScheduleEventVia(r.Context(), evt, func(ctx context.Context, e *store.ScheduledEvent) error {
		return s.store.CreateScheduledEventChecked(ctx, e, scheduledExecMaxPerProject+1, func(snap store.ScheduledExecQuotaSnapshot) error {
			return scheduledExecQuotaCheck(snap, targetID, "")
		})
	}); err != nil {
		if errors.Is(err, errScheduledExecQuota) {
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(), nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	s.auditScheduledExec(r.Context(), newScheduledExecAuthoringAudit(r.Context(), ScheduledExecAuditCreate, projectID, evt.ID, p))
	s.writeCreatedScheduledEvent(w, r, evt)
}

// auditScheduledExecCancel records the cancel of a one-shot exec event by
// the request in ctx. A payload that no longer decodes is audited without
// its command fields.
func (s *Server) auditScheduledExecCancel(ctx context.Context, evt store.ScheduledEvent) {
	p, err := decodeScheduledExecPayload(evt.Payload)
	if err != nil {
		p = &scheduledExecPayload{AgentID: execPayloadTarget(evt.Payload)}
	}
	a := newScheduledExecAuthoringAudit(ctx, ScheduledExecAuditDelete, evt.ProjectID, evt.ID, p)
	if err != nil {
		a.CommandSHA256, a.Argc, a.Argv0, a.ShellForm, a.TimeoutSeconds = "", 0, "", false, 0
	}
	s.auditScheduledExec(ctx, a)
}

// createExecSchedule creates a recurring exec schedule (ptone/scion#7884).
// The router has already enforced the authoring credential gate and
// scheduled_event.create; this validates the exec payload and cron timing,
// authorizes live agent.attach on the target, checks active exec quotas
// atomically under the per-project quota lock, and emits schedule.exec.create.
func (s *Server) createExecSchedule(w http.ResponseWriter, r *http.Request, projectID string, req *CreateScheduleRequest) {
	if req.Message != "" || req.Interrupt || req.Wake || req.Template != "" || req.Task != "" || req.Branch != "" {
		ValidationError(w, "exec schedules take agentId or agentName, command and timeoutSeconds only", nil)
		return
	}

	var p *scheduledExecPayload
	if req.Payload != "" {
		if req.Command != nil || req.TimeoutSeconds != 0 || req.AgentID != "" || req.AgentName != "" {
			ValidationError(w, "give either payload or the exec convenience fields, not both", nil)
			return
		}
		if !s.validateAndRejectScheduledPayload(w, r, req.EventType, req.Payload) {
			return
		}
		var err error
		if p, err = decodeScheduledExecPayload(req.Payload); err != nil {
			BadRequest(w, err.Error())
			return
		}
	} else {
		p = &scheduledExecPayload{
			AgentID:        req.AgentID,
			AgentName:      req.AgentName,
			Command:        req.Command,
			TimeoutSeconds: req.TimeoutSeconds,
		}
		if err := p.validate(); err != nil {
			ValidationError(w, err.Error(), nil)
			return
		}
	}

	if _, err := parseScheduleCron(req.CronExpr); err != nil {
		writeCronParseError(w, err)
		return
	}
	timeZone, err := normalizeScheduleTimeZone(req.TimeZone)
	if err != nil {
		writeScheduleTimingError(w, err)
		return
	}
	nextRunAt, err := scheduleNextRun(req.CronExpr, timeZone, time.Now().UTC())
	if err != nil {
		writeScheduleTimingError(w, err)
		return
	}

	if _, ok := s.authorizeScheduledExecAuthoringOpts(w, r, projectID, p, "", true); !ok {
		return
	}
	payload, err := p.encode()
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	ceiling, ok := s.revisionAuthorityCeiling(w, r, projectID, ActionCreate)
	if !ok {
		return
	}

	createdBy := ""
	if identity := GetIdentityFromContext(r.Context()); identity != nil {
		createdBy = identity.ID()
	}
	schedule := store.Schedule{
		ID:                   api.NewUUID(),
		ProjectID:            projectID,
		Name:                 req.Name,
		CronExpr:             req.CronExpr,
		TimeZone:             timeZone,
		EventType:            scheduledEventTypeExec,
		Payload:              payload,
		Status:               store.ScheduleStatusActive,
		NextRunAt:            &nextRunAt,
		CreatedBy:            createdBy,
		InitiatorAttribution: newInitiatorAttribution(r.Context()),
		AuthorityCeiling:     ceiling,
	}
	if err := stampSchedule(r.Context(), s.store, &schedule, fixturePrincipalFromContext(r.Context())); err != nil {
		if !writeFixtureRootError(w, err) {
			writeErrorFromErr(w, err, "")
		}
		return
	}

	targetID := p.AgentID
	if err := s.store.CreateScheduleChecked(r.Context(), &schedule, scheduledExecMaxPerProject+1, func(snap store.ScheduledExecQuotaSnapshot) error {
		return scheduledExecQuotaCheck(snap, targetID, "")
	}); err != nil {
		if errors.Is(err, errScheduledExecQuota) {
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(), nil)
			return
		}
		if errors.Is(err, store.ErrAlreadyExists) {
			writeScheduleNameConflict(w, r, req.Name)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	out := schedule
	if created, err := s.store.GetSchedule(r.Context(), schedule.ID); err == nil {
		out = *created
	}
	s.auditScheduledExec(r.Context(), newScheduleExecAuthoringAudit(r.Context(), ScheduledExecAuditCreate, out, p))
	writeJSON(w, http.StatusCreated, scheduleResponse(s.newExecRevealer(r.Context()), out))
}

// prepareExecScheduleUpdate validates and authorizes an exec schedule PATCH
// that supplies exec fields/payload or changes future dispatch, returning the
// resolved payload and its canonical JSON encoding.
func (s *Server) prepareExecScheduleUpdate(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	schedule *store.Schedule,
	originalEventType, originalPayload string,
	req *UpdateScheduleRequest,
	hasExecFields bool,
) (*scheduledExecPayload, string, bool) {
	prevLive, ok := s.authorizeStoredExecTargetForUpdate(w, r, projectID, execPayloadTarget(originalPayload))
	if !ok {
		return nil, "", false
	}
	if !prevLive && req.Payload == "" && req.Command == nil && (req.AgentID != "" || req.AgentName != "") {
		ValidationError(w, "retargeting an exec schedule whose previous target was deleted requires command or payload", nil)
		return nil, "", false
	}

	var p *scheduledExecPayload
	switch {
	case req.Payload != "":
		if hasExecFields {
			ValidationError(w, "give either payload or the exec convenience fields, not both", nil)
			return nil, "", false
		}
		decoded, err := decodeScheduledExecPayload(req.Payload)
		if err != nil {
			BadRequest(w, err.Error())
			return nil, "", false
		}
		p = decoded
	case hasExecFields:
		var base scheduledExecPayload
		if prevLive && originalEventType == scheduledEventTypeExec && originalPayload != "" {
			if prev, err := decodeScheduledExecPayload(originalPayload); err == nil {
				base = *prev
				if base.AgentID != "" {
					base.AgentName = ""
				}
			}
		}
		if req.AgentID != "" || req.AgentName != "" {
			base.AgentID = req.AgentID
			base.AgentName = req.AgentName
		}
		if req.Command != nil {
			base.Command = req.Command
		}
		if req.TimeoutSeconds != nil {
			base.TimeoutSeconds = *req.TimeoutSeconds
		}
		if err := base.validate(); err != nil {
			ValidationError(w, err.Error(), nil)
			return nil, "", false
		}
		p = &base
	default:
		decoded, err := decodeScheduledExecPayload(originalPayload)
		if err != nil {
			BadRequest(w, err.Error())
			return nil, "", false
		}
		if decoded.AgentID != "" {
			decoded.AgentName = ""
		}
		p = decoded
	}

	checkQuota := schedule.Status == store.ScheduleStatusActive
	if _, ok := s.authorizeScheduledExecAuthoringOpts(w, r, projectID, p, schedule.ID, checkQuota); !ok {
		return nil, "", false
	}
	encoded, err := p.encode()
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return nil, "", false
	}
	return p, encoded, true
}
