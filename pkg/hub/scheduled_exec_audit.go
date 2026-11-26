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
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Audit records of exec schedules (ptone/scion#7883, design "Audit"; Q3:
// the AuditLogger now, an auditevent family later as F4/F9). Emission is
// best effort; the durable record of a run is its scheduled_exec_runs row.
// A record is content-free: it never carries argv beyond the argv0
// basename (sanitized, scheduledExecArgv0) and never command output.

// Actions of an exec schedule audit record (audit_action
// "schedule.exec.<action>").
const (
	ScheduledExecAuditCreate  = "create"
	ScheduledExecAuditUpdate  = "update"
	ScheduledExecAuditResume  = "resume"
	ScheduledExecAuditPause   = "pause"
	ScheduledExecAuditDelete  = "delete"
	ScheduledExecAuditRefused = "refused"
	ScheduledExecAuditSkipped = "skipped"
	ScheduledExecAuditAdmit   = "admit"
	ScheduledExecAuditOutcome = "outcome"
)

// ScheduledExecEvent is one exec schedule audit record.
type ScheduledExecEvent struct {
	// Action is one of the ScheduledExecAudit* constants.
	Action string
	// Actor of an authoring action (create, delete): the request's
	// principal and credential.
	ActorKind           string
	ActorID             string
	ActorCredentialKind string
	ActorCredentialID   string
	// Initiator of a fire action (refused, skipped, admit, outcome): the
	// event's authorization revision.
	InitiatorPrincipalKind  string
	InitiatorPrincipalID    string
	InitiatorCredentialKind string
	InitiatorCredentialID   string
	AuthorizationRevision   int
	// Executor of a fire action (scheduler, scheduled_event:<id>).
	ExecutorKind string
	ExecutorID   string

	ProjectID     string
	ScheduleID    string
	EventID       string
	TargetAgentID string

	// Trigger-routed exec audit fields (Stage 2 P8, ptone/scion#8241).
	// When IsTrigger is true, audit_action is "trigger.fire.exec.<action>" and
	// event_type is "trigger_fire_exec". ContextAttributeKeys records allow-listed
	// keys only, never values.
	IsTrigger            bool
	TriggerID            string
	FireID               string
	TriggerEventID       string
	ContextAttributeKeys []string

	CommandSHA256  string
	Argc           int
	Argv0          string
	ShellForm      bool
	TimeoutSeconds int
	CaptureMode    string

	// Reason is the detailed reason code of a refusal or skip.
	Reason string
	// OperationID correlates the run across hub and sciontool logs (the
	// event id).
	OperationID string
	// Outcome group (outcome action).
	Outcome      string
	ExitCode     *int
	DurationMS   *int64
	OutputBytes  *int64
	OutputStatus string

	RequestID string
	Timestamp time.Time
}

// scheduledExecAuditor is implemented by audit loggers that record exec
// schedule events (LogAuditLogger does). It is separate from AuditLogger
// so other implementations need no change.
type scheduledExecAuditor interface {
	LogScheduledExecEvent(ctx context.Context, event *ScheduledExecEvent) error
}

func scheduledExecAuditAttrs(e *ScheduledExecEvent) []slog.Attr {
	eventType := "schedule_exec"
	auditAction := "schedule.exec." + e.Action
	if e.IsTrigger {
		eventType = "trigger_fire_exec"
		auditAction = "trigger.fire.exec." + e.Action
	}
	attrs := []slog.Attr{
		slog.String("event_type", eventType),
		slog.String("audit_action", auditAction),
		slog.String("project_id", e.ProjectID),
	}
	if !e.IsTrigger || e.EventID != "" {
		attrs = append(attrs, slog.String("scheduled_event_id", e.EventID))
	}
	attrs = append(attrs, slog.String("target_agent_id", e.TargetAgentID))
	add := func(key, val string) {
		if val != "" {
			attrs = append(attrs, slog.String(key, val))
		}
	}
	add("schedule_id", e.ScheduleID)
	add("trigger_id", e.TriggerID)
	add("fire_id", e.FireID)
	add("trigger_event_id", e.TriggerEventID)
	if len(e.ContextAttributeKeys) > 0 {
		attrs = append(attrs, slog.Any("context_attribute_keys", e.ContextAttributeKeys))
	}
	add("actor_kind", e.ActorKind)
	add("actor_id", e.ActorID)
	add("actor_credential_kind", e.ActorCredentialKind)
	add("actor_credential_id", e.ActorCredentialID)
	add("initiator_principal_kind", e.InitiatorPrincipalKind)
	add("initiator_principal_id", e.InitiatorPrincipalID)
	add("initiator_credential_kind", e.InitiatorCredentialKind)
	add("initiator_credential_id", e.InitiatorCredentialID)
	if e.AuthorizationRevision != 0 {
		attrs = append(attrs, slog.Int("authorization_revision", e.AuthorizationRevision))
	}
	add("executor_kind", e.ExecutorKind)
	add("executor_id", e.ExecutorID)
	add("command_sha256", e.CommandSHA256)
	if e.Argc > 0 {
		attrs = append(attrs, slog.Int("argc", e.Argc), slog.String("argv0", e.Argv0), slog.Bool("shell_form", e.ShellForm))
	}
	if e.TimeoutSeconds > 0 {
		attrs = append(attrs, slog.Int("timeout_seconds", e.TimeoutSeconds))
	}
	add("capture_mode", e.CaptureMode)
	add("reason", e.Reason)
	add("operation_id", e.OperationID)
	add("outcome", e.Outcome)
	if e.ExitCode != nil {
		attrs = append(attrs, slog.Int("exit_code", *e.ExitCode))
	}
	if e.DurationMS != nil {
		attrs = append(attrs, slog.Int64("duration_ms", *e.DurationMS))
	}
	if e.OutputBytes != nil {
		attrs = append(attrs, slog.Int64("output_bytes", *e.OutputBytes))
	}
	add("output_status", e.OutputStatus)
	add("request_id", e.RequestID)
	if !e.Timestamp.IsZero() {
		attrs = append(attrs, slog.Time("event_time", e.Timestamp.UTC()))
	}
	return attrs
}

// LogScheduledExecEvent logs an exec schedule audit event. A nil receiver is
// safe.
func (l *LogAuditLogger) LogScheduledExecEvent(ctx context.Context, e *ScheduledExecEvent) error {
	if l == nil || e == nil {
		return nil
	}
	l.logger().LogAttrs(ctx, slog.LevelInfo, "schedule exec event", scheduledExecAuditAttrs(e)...)
	return nil
}

// withCommand fills the content-free command fields from argv and the
// timeout.
func (e *ScheduledExecEvent) withCommand(argv []string, timeoutSeconds int) *ScheduledExecEvent {
	e.CommandSHA256 = agentcontrol.ExecArgvDigest(argv)
	e.Argc = len(argv)
	e.Argv0 = scheduledExecArgv0(argv)
	e.ShellForm = scheduledExecShellForm(argv)
	e.TimeoutSeconds = timeoutSeconds
	return e
}

// newScheduledExecAuthoringAudit returns the record of a one-shot event
// authoring action by the request in ctx.
func newScheduledExecAuthoringAudit(ctx context.Context, action, projectID, eventID string, p *scheduledExecPayload) *ScheduledExecEvent {
	e := &ScheduledExecEvent{
		Action:        action,
		ProjectID:     projectID,
		EventID:       eventID,
		TargetAgentID: p.AgentID,
		RequestID:     logging.RequestIDFromContext(ctx),
		Timestamp:     time.Now().UTC(),
	}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		e.ActorKind, e.ActorID = identity.Type(), identity.ID()
	}
	cred := GetCredentialContextFromContext(ctx)
	e.ActorCredentialKind, e.ActorCredentialID = string(cred.Kind), cred.ID
	e.CaptureMode = p.CaptureOutput
	return e.withCommand(p.Command, p.timeout())
}

// newScheduleExecAuthoringAudit returns the record of a recurring schedule
// authoring action (create, update, resume, pause, delete) by the request in
// ctx (ptone/scion#7884). p may be nil when auditing a stored row whose payload
// no longer decodes.
func newScheduleExecAuthoringAudit(ctx context.Context, action string, sc store.Schedule, p *scheduledExecPayload) *ScheduledExecEvent {
	e := &ScheduledExecEvent{
		Action:                action,
		ProjectID:             sc.ProjectID,
		ScheduleID:            sc.ID,
		AuthorizationRevision: sc.AuthorizationRevision,
		RequestID:             logging.RequestIDFromContext(ctx),
		Timestamp:             time.Now().UTC(),
	}
	if identity := GetIdentityFromContext(ctx); identity != nil {
		e.ActorKind, e.ActorID = identity.Type(), identity.ID()
	}
	cred := GetCredentialContextFromContext(ctx)
	e.ActorCredentialKind, e.ActorCredentialID = string(cred.Kind), cred.ID
	if p != nil {
		e.TargetAgentID = p.AgentID
		e.CaptureMode = p.CaptureOutput
		e.withCommand(p.Command, p.timeout())
	} else {
		e.TargetAgentID = execPayloadTarget(sc.Payload)
	}
	return e
}

// auditScheduleExecMutation records a mutation (update, pause, delete) of a
// recurring exec schedule. A stored payload that no longer decodes is audited
// without its command fields.
func (s *Server) auditScheduleExecMutation(ctx context.Context, action string, sc store.Schedule) {
	p, err := decodeScheduledExecPayload(sc.Payload)
	if err != nil {
		p = nil
	}
	s.auditScheduledExec(ctx, newScheduleExecAuthoringAudit(ctx, action, sc, p))
}

// newExecFireAudit returns the record of a deferred exec fire action on req,
// with the initiator from req.Authority.Attribution and the executor from ctx.
// ScheduleID is set only when DefinitionKind == "schedule"; when
// DefinitionKind == "trigger", IsTrigger/TriggerID/FireID/TriggerEventID are
// populated and audit_action uses "trigger.fire.exec.<action>". p may be nil.
func newExecFireAudit(ctx context.Context, action string, req hubfire.FireRequest, p *scheduledExecPayload) *ScheduledExecEvent {
	executor, _ := ExecutorContextFromContext(ctx)
	isTrigger := req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger
	var scheduleID string
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = req.Authority.DefinitionID
	}
	attr := req.Authority.Attribution
	e := &ScheduledExecEvent{
		Action:                  action,
		InitiatorPrincipalKind:  attr.InitiatorPrincipalKind,
		InitiatorPrincipalID:    attr.InitiatorPrincipalID,
		InitiatorCredentialKind: attr.InitiatorCredentialKind,
		InitiatorCredentialID:   attr.InitiatorCredentialID,
		AuthorizationRevision:   attr.AuthorizationRevision,
		ExecutorKind:            executor.Kind,
		ExecutorID:              executor.ID,
		ProjectID:               req.ProjectID,
		ScheduleID:              scheduleID,
		OperationID:             req.Authority.FireID,
		IsTrigger:               isTrigger,
		Timestamp:               time.Now().UTC(),
	}
	if isTrigger {
		e.TriggerID = req.Authority.DefinitionID
		e.FireID = req.Authority.FireID
		if req.Event != nil {
			e.TriggerEventID = req.Event.ID
		}
	} else {
		e.EventID = req.Authority.FireID
	}
	if p != nil {
		e.TargetAgentID = p.AgentID
		e.CaptureMode = p.CaptureOutput
		e.withCommand(p.Command, p.timeout())
		if isTrigger && len(p.ContextAttributes) > 0 {
			e.ContextAttributeKeys = append([]string(nil), p.ContextAttributes...)
		}
	}
	return e
}

// newScheduledExecFireAudit returns the record of a fire action on evt,
// with the initiator from the event's authorization revision and the
// executor from ctx. p may be nil (a payload that does not decode).
func newScheduledExecFireAudit(ctx context.Context, action string, evt store.ScheduledEvent, p *scheduledExecPayload) *ScheduledExecEvent {
	return newExecFireAudit(ctx, action, hubfire.FireRequest{
		ProjectID: evt.ProjectID,
		Authority: scheduledRecordedAuthority(evt),
	}, p)
}

// newScheduledExecRunAudit returns the record of a run's outcome from its
// durable row (the tracker and the sweeper have no payload).
func newScheduledExecRunAudit(run store.ScheduledExecRun) *ScheduledExecEvent {
	e := &ScheduledExecEvent{
		Action:                  ScheduledExecAuditOutcome,
		InitiatorPrincipalKind:  run.InitiatorPrincipalKind,
		InitiatorPrincipalID:    run.InitiatorPrincipalID,
		InitiatorCredentialKind: run.InitiatorCredentialKind,
		InitiatorCredentialID:   run.InitiatorCredentialID,
		AuthorizationRevision:   run.AuthorizationRevision,
		ExecutorKind:            run.ExecutorKind,
		ExecutorID:              run.ExecutorID,
		ProjectID:               run.ProjectID,
		ScheduleID:              run.ScheduleID,
		EventID:                 run.EventID,
		OperationID:             run.RunID(),
		TargetAgentID:           run.TargetAgentID,
		CommandSHA256:           run.CommandSHA256,
		Argc:                    run.Argc,
		TimeoutSeconds:          run.TimeoutSeconds,
		CaptureMode:             run.CaptureMode,
		IsTrigger:               run.IsTriggerOwned(),
		FireID:                  run.FireID,
		Timestamp:               time.Now().UTC(),
	}
	if run.IsTriggerOwned() && strings.HasPrefix(run.OverlapKey, "trigger:") {
		rest := strings.TrimPrefix(run.OverlapKey, "trigger:")
		if id, _, ok := strings.Cut(rest, ":"); ok {
			e.TriggerID = id
		} else {
			e.TriggerID = rest
		}
	}
	return e
}

// auditScheduledExec records e through the hub audit logger, falling back to
// a structured log record with the same fields.
func (s *Server) auditScheduledExec(ctx context.Context, e *ScheduledExecEvent) {
	if a, ok := s.GetAuditLogger().(scheduledExecAuditor); ok {
		_ = a.LogScheduledExecEvent(ctx, e)
		return
	}
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "schedule exec event", scheduledExecAuditAttrs(e)...)
}
