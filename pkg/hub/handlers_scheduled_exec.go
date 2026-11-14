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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// scheduledEventTypeSupported reports whether one-shot scheduled events of
// eventType may be created. exec is supported only while exec schedules are
// enabled (hub.exec_schedules and hub.conduit, D8); otherwise it is
// answered exactly like any unknown type.
func (s *Server) scheduledEventTypeSupported(eventType string) bool {
	switch eventType {
	case "message", "dispatch_agent":
		return true
	case scheduledEventTypeExec:
		return s.execSchedulesEnabled()
	}
	return false
}

// supportedScheduledEventTypes names the supported one-shot event types for
// the unsupported-type message.
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
	if err := s.scheduler.ScheduleEvent(r.Context(), evt); err != nil {
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
