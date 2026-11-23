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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Security inventory for trigger-routed exec (Stage 2 P8, ptone/scion#8241):
//
// A. Every way event data or rule config can influence what runs:
//  1. argv (command): literal []string fixed at authoring (1..64 elements, <=4 KiB
//     each, <=16 KiB total, no NUL). Event data NEVER templates into argv;
//     trigger authoring rejects any "${" in command (400 templating_not_allowed_in_argv),
//     and Fire sends p.Command verbatim, bound by ExecArgvDigest(p.Command) in
//     scheduled_exec_runs.command_sha256 and sciontool's execBinding.
//  2. Environment (env): inherited from the target agent's harness tmux session
//     plus PWD; for trigger-routed runs ONLY the single fixed-name variable
//     SCION_TRIGGER_CONTEXT is added. No arbitrary env var names from rule config
//     or event data.
//  3. Context variable (SCION_TRIGGER_CONTEXT): hub-built JSON object
//     {event_id, trigger_id, fire_id, subject?, attributes?} sent via optional
//     ExecStartRequest.TriggerContext (v=1) ONLY when the agent's Conduit Hello
//     capabilities.features advertises exec_context.v1. If a fire-owned run has
//     trigger context and the session lacks exec_context.v1, the hub refuses the
//     run before sending exec-start (fire failed, "sciontool lacks exec_context.v1")
//     and never starts without context or drops the field. Only keys in the rule's
//     validated contextAttributes allow-list are copied (each value <=4 KiB, valid
//     UTF-8, no NUL; total JSON <=16 KiB; violations refuse the fire without
//     truncation). Bound into sciontool's execBinding sha256 digest and never logged.
//  4. Working directory (workdir): fixed by sciontool (/workspace -> HOME -> /);
//     neither rule config nor event data can influence it.
//  5. Attach target (agentId / launch_id): resolved to authoritative UUID agentId
//     at authoring (never falls back to agentName at fire) and pinned to
//     target.RunID via callAgentControlPinned.
//  6. Overlap & subject claim: overlap_key="trigger:<triggerID>" (never
//     schedule_id) in DB + ExecStartRequest.ScheduleID=triggerID in sciontool;
//     subject mode "reuse" refused at authoring; "skip" acquires a
//     trigger_subject_claims row bound to target.ID at admission and held until
//     FinishScheduledExecRun finishes the run in the same transaction.
//
// B. Every authority check:
//  1. Authoring (POST/PATCH /triggers, POST /triggers/{id}/resume):
//     hub.triggers + hub.exec_schedules + hub.conduit; authorizeDeferredAuthoringCredential;
//     trigger.create/update; trigger.SourceReplayProtected (400 source_not_replay_protected);
//     authorizeAgentTargetAction(ActionAttach) (no existence oracle 404; 422 on
//     managed runtime); PATCH authorizeStoredExecTargetForUpdate; EffectCeilingAllows
//     agent.attach; per-principal rate limit and per-trigger-exec authoring caps
//     under LockTriggerQuota.
//  2. Fire-time re-check (Preauthorize, resolveRecordedAuthority, Authorize):
//     strict payload decode; hub.exec_schedules + hub.conduit; reload endpoint
//     and re-check trigger.SourceReplayProtected; resolveRecordedAuthority;
//     frozen ceiling EffectCeilingAllows(agent.attach); live authorizeAgentTargetAction
//     (ActionAttach); fireStanding; target preflight; per-trigger exec fire rate cap.
//  3. Read-time argv redaction (trigger_response.go):
//     command revealed only when the caller holds live agent.attach on the stored
//     target via execRevealer; otherwise omitted with commandRedacted=true, argc,
//     commandSha256 (fail closed on malformed payload).

// execAction implements FireAction, FirePayloadValidator, FirePreauthorizer,
// and FireAuthorityRefusalHandler for deferred "exec" fires executed in a
// running target agent's container over its Conduit session.
type execAction struct {
	srv *Server
}

type execAuthorized struct {
	payload        *scheduledExecPayload
	target         *store.Agent
	auth           FireAuthority
	triggerContext string
}

func (a *execAction) Type() string {
	return scheduledEventTypeExec
}

func (a *execAction) Permission() string {
	return scheduledExecPermission
}

func (a *execAction) Detaches(req hubfire.FireRequest) bool {
	return req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule
}

// Validate refuses in Stage 1: schedule authoring handlers continue to run
// authorizeScheduledExecAuthoringOpts (rate limiting, HTTP 404/403/422/429
// parity, and per-project locked quota checks), while trigger authoring refuses
// exec actions until Stage 2 addendum P8 / Stage 3 wire trigger exec quotas and
// replay-protected source rules.
func (a *execAction) Validate(_ context.Context, _ Identity, _ string, _ string) (string, error) {
	return "", errors.New("exec action authoring validation is not wired in Stage 1")
}

func unmarshalExecFirePayload(raw string) (*scheduledExecPayload, error) {
	p, err := decodeScheduledExecPayload(raw)
	if err == nil && p.AgentID == "" {
		err = &scheduledExecPayloadError{field: "agentId"}
	}
	return p, err
}

func (a *execAction) ValidatePayload(req hubfire.FireRequest) error {
	if _, err := unmarshalExecFirePayload(req.Payload); err != nil {
		// Fail closed on a stored row that no longer validates.
		slog.Warn("Scheduler: exec event payload invalid",
			"subsystem", "scheduler",
			"eventID", req.Authority.FireID,
			"projectID", req.ProjectID,
			"error", err)
		return errScheduledExecPayloadInvalid
	}
	return nil
}

func (a *execAction) skipExecFire(ctx context.Context, req hubfire.FireRequest, p *scheduledExecPayload, reason, detail string) error {
	if a != nil && a.srv != nil {
		auditRec := newExecFireAudit(ctx, ScheduledExecAuditSkipped, req, p)
		auditRec.Reason = detail
		a.srv.auditScheduledExec(ctx, auditRec)
	}
	var agentID string
	if p != nil {
		agentID = p.AgentID
	}
	slog.Info("Scheduler: exec event skipped",
		"subsystem", "scheduler",
		"eventID", req.Authority.FireID,
		"projectID", req.ProjectID,
		"agent_id", agentID,
		"reason", detail)
	return skipScheduledFire(reason)
}

func (a *execAction) refuseExecFire(ctx context.Context, req hubfire.FireRequest, auth FireAuthority, p *scheduledExecPayload, err error) {
	if a != nil && a.srv != nil {
		auditRec := newExecFireAudit(ctx, ScheduledExecAuditRefused, req, p)
		auditRec.Reason = err.Error()
		a.srv.auditScheduledExec(ctx, auditRec)
	}
	var agentID string
	if p != nil {
		agentID = p.AgentID
	}
	slog.Warn("Scheduler: exec event refused at fire time",
		"subsystem", "scheduler",
		"eventID", req.Authority.FireID,
		"projectID", req.ProjectID,
		"agent_id", agentID,
		"principal_kind", auth.PrincipalKind,
		"principal_id", auth.PrincipalID,
		"authorization_revision", req.Authority.Attribution.AuthorizationRevision,
		"error", err)
}

func (a *execAction) Preauthorize(ctx context.Context, req hubfire.FireRequest) (*FireRefusal, error) {
	if a == nil || a.srv == nil {
		return nil, errors.New("exec action: server not configured")
	}
	p, err := unmarshalExecFirePayload(req.Payload)
	if err != nil {
		return nil, errScheduledExecPayloadInvalid
	}
	if !a.srv.execSchedulesEnabled() {
		return nil, a.skipExecFire(ctx, req, p, execReasonDisabled, "experiment_off")
	}
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		if req.Event == nil {
			a.refuseExecFire(ctx, req, FireAuthority{}, p, errors.New("missing trigger event"))
			return &FireRefusal{Reason: execReasonRefused}, nil
		}
		var ep *store.WebhookEndpoint
		if req.Event.SourceKind == store.TriggerSourceKindWebhook {
			loaded, epErr := a.srv.store.GetWebhookEndpoint(ctx, req.Event.SourceID)
			if epErr != nil {
				if errors.Is(epErr, store.ErrNotFound) || errors.Is(epErr, store.ErrInvalidInput) {
					a.refuseExecFire(ctx, req, FireAuthority{}, p, errors.New(execReasonSourceNotReplayProtected))
					return &FireRefusal{Reason: execReasonSourceNotReplayProtected}, nil
				}
				return nil, fmt.Errorf("exec trigger fire: load webhook endpoint: %w", epErr)
			}
			ep = loaded
		}
		if !trigger.SourceReplayProtected(req.Event.SourceKind, ep) {
			a.refuseExecFire(ctx, req, FireAuthority{}, p, errors.New(execReasonSourceNotReplayProtected))
			return &FireRefusal{Reason: execReasonSourceNotReplayProtected}, nil
		}
		return nil, nil
	}
	if !req.FireAt.IsZero() && time.Since(req.FireAt) > scheduledExecMaxWindow {
		return nil, a.skipExecFire(ctx, req, p, execReasonMissedWindow, "missed_window")
	}
	return nil, nil
}

func (a *execAction) OnAuthorityRefused(ctx context.Context, req hubfire.FireRequest, err error) string {
	p, _ := unmarshalExecFirePayload(req.Payload)
	a.refuseExecFire(ctx, req, FireAuthority{}, p, err)
	return execReasonRefused
}

// authorizeExecFire authorizes a deferred exec fire immediately before
// preflight and dispatch under auth and identity: the authority and identity
// resolveRecordedAuthority returned for the fire.
//
//  1. Resolves the target strictly by the stored agentId (never agentName): it
//     must exist, not be deleted, and belong to projectID;
//  2. Requires the revision's frozen ceiling to allow agent.attach (for user
//     and agent principals alike: an agent identity is rebuilt from its current
//     role, so only the frozen ceiling keeps a revision authored without attach
//     authority from running after that role widens);
//  3. Enforces the live rule authorizeAgentTargetAction(identity, target,
//     ActionAttach);
//  4. Enforces standing: target not held and authoring agent(s) in good
//     standing (fireStanding).
//
// Any denial returns an error naming the detailed cause for audit and slog;
// the fire record persists only the generic execReasonRefused.
func (s *Server) authorizeExecFire(
	ctx context.Context,
	projectID string,
	auth FireAuthority,
	identity Identity,
	p *scheduledExecPayload,
) (*store.Agent, error) {
	if p == nil || p.AgentID == "" {
		return nil, &scheduledExecPayloadError{field: "agentId"}
	}
	target, err := s.store.GetAgent(ctx, p.AgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errScheduledExecTargetGone
		}
		return nil, fmt.Errorf("exec target lookup: %w", err)
	}
	if target == nil || !target.DeletedAt.IsZero() || target.ProjectID != projectID {
		return nil, errScheduledExecTargetGone
	}
	if identity == nil {
		return target, fmt.Errorf("%w: no resolved identity", errScheduledAuthorityDenied)
	}
	if !EffectCeilingAllows(auth.Ceiling, scheduledExecPermission, false) {
		return target, fmt.Errorf("%w: revision ceiling does not allow %s", errScheduledAuthorityDenied, scheduledExecPermission)
	}
	if denial := s.authorizeAgentTargetAction(ctx, identity, target, ActionAttach); denial != nil {
		return target, fmt.Errorf("%w: %s", errScheduledAuthorityDenied, denial.reason)
	}
	if err := s.fireStanding(ctx, auth, target); err != nil {
		return target, fmt.Errorf("%w: standing: %v", errScheduledAuthorityDenied, err)
	}
	return target, nil
}

func (a *execAction) Authorize(
	ctx context.Context,
	req hubfire.FireRequest,
	auth FireAuthority,
	identity Identity,
) (FireAuthorized, *FireRefusal, error) {
	if a == nil || a.srv == nil {
		return nil, nil, errors.New("exec action: server not configured")
	}
	p, err := unmarshalExecFirePayload(req.Payload)
	if err != nil {
		return nil, nil, errScheduledExecPayloadInvalid
	}
	target, err := a.srv.authorizeExecFire(ctx, req.ProjectID, auth, identity, p)
	if err != nil {
		a.refuseExecFire(ctx, req, auth, p, err)
		return nil, &FireRefusal{Reason: execReasonRefused}, nil
	}

	// Preflight (D4): running targets only. The Authorize/Fire seam sits right
	// after preflight (Stage 1 design §5.2, §17): Authorize returns the
	// resolved deliverable target, while Fire owns admission, run-row creation,
	// exec-start, and tracker handoff.
	if isManagedAgentRuntime(target.Runtime) {
		return nil, nil, a.skipExecFire(ctx, req, p, execReasonTargetNotRunning, "managed_runtime")
	}
	if phaseErr := validateAgentRunning(target); phaseErr != nil {
		return nil, nil, a.skipExecFire(ctx, req, p, execReasonTargetNotRunning, "phase_"+store.EffectiveRestingPhase(target))
	}
	if reincarnationInFlight(target) {
		return nil, nil, a.skipExecFire(ctx, req, p, execReasonTargetNotRunning, "reincarnating")
	}

	var triggerCtx string
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		built, ctxErr := buildTriggerExecContext(req, p.ContextAttributes)
		if ctxErr != nil {
			a.refuseExecFire(ctx, req, auth, p, ctxErr)
			return nil, &FireRefusal{Reason: execReasonContextInvalid}, nil
		}
		triggerCtx = built

		// Per-trigger exec fire rate cap (own cap, separate from schedule quotas).
		if req.Authority.DefinitionID != "" {
			recentCount, countErr := a.srv.store.CountTriggerFiresSince(
				ctx,
				req.Authority.DefinitionID,
				time.Now().Add(-time.Minute),
				store.TriggerFireStatusSkipped,
			)
			if countErr != nil {
				return nil, nil, fmt.Errorf("exec trigger fire: count recent fires: %w", countErr)
			}
			// The current firing row is already persisted in trigger_fires.
			if recentCount > triggerExecMaxFiresPerMinute {
				return nil, nil, a.skipExecFire(ctx, req, p, execReasonRateLimit, "rate_limit")
			}
		}
	}

	return &execAuthorized{
		payload:        p,
		target:         target,
		auth:           auth,
		triggerContext: triggerCtx,
	}, nil, nil
}

func (a *execAction) Fire(ctx context.Context, req hubfire.FireRequest, authorized FireAuthorized) (hubfire.FireOutcome, error) {
	if a == nil || a.srv == nil {
		return hubfire.FireOutcome{}, errors.New("exec action: server not configured")
	}
	ea, ok := authorized.(*execAuthorized)
	if !ok || ea == nil || ea.payload == nil || ea.target == nil {
		return hubfire.FireOutcome{}, errors.New("exec action: missing authorized state")
	}
	return a.dispatchExecAction(ctx, req, ea)
}

func (a *execAction) dispatchExecAction(ctx context.Context, req hubfire.FireRequest, ea *execAuthorized) (hubfire.FireOutcome, error) {
	s := a.srv
	p := ea.payload
	target := ea.target
	fireID := req.Authority.FireID
	if fireID == "" {
		return hubfire.FireOutcome{}, errors.New("exec action: missing fire ID")
	}
	isTrigger := req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger
	var scheduleID string
	var overlapKey string
	var sciontoolScopeID string
	if isTrigger {
		if req.Authority.DefinitionID != "" {
			overlapKey = "trigger:" + req.Authority.DefinitionID
			sciontoolScopeID = req.Authority.DefinitionID
		}
	} else if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = req.Authority.DefinitionID
		if scheduleID != "" {
			overlapKey = "schedule:" + scheduleID
			sciontoolScopeID = scheduleID
		}
	}

	log := slog.With("subsystem", "scheduler", "eventID", fireID, "projectID", req.ProjectID)

	// Subject policy "skip" for trigger-routed exec: acquire a persistent
	// trigger_subject_claims row before run admission. CreateScheduledExecRun
	// binds it to target.ID atomically with the run insert so it stays held
	// until FinishScheduledExecRun completes the run (P8).
	var heldClaim *store.TriggerSubjectClaim
	if isTrigger && req.Event != nil && req.Event.Trigger != nil && req.Event.Trigger.SubjectMode == store.TriggerSubjectModeSkip {
		fireSubject := req.Event.Trigger.FireSubject
		if fireSubject == "" {
			fireSubject = req.Event.Subject
		}
		claimNow := time.Now().UTC()
		claimed, existing, claimErr := s.store.ClaimTriggerSubject(
			ctx,
			req.Authority.DefinitionID,
			fireSubject,
			req.Event.ID,
			fireID,
			claimNow,
			claimNow.Add(-store.TriggerSubjectAbandonedClaimTTL),
		)
		if claimErr != nil {
			log.Warn("Scheduler: exec trigger subject claim failed", "error", claimErr)
			return hubfire.FireOutcome{}, errors.New(execReasonDispatchFailed)
		}
		if existing != nil {
			return hubfire.FireOutcome{}, a.skipExecFire(ctx, req, p, execReasonSubjectBusy, "subject_busy")
		}
		heldClaim = claimed
	}
	releaseUnboundClaim := func() {
		if heldClaim == nil {
			return
		}
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		_, _ = s.store.ReleaseTriggerSubjectClaim(relCtx, heldClaim.ID, heldClaim.FireID, time.Now().UTC())
	}

	// Admission: the replica's tracked-run bound (never waits) and the
	// shutdown gate. The slot and ownership pass to the tracker via
	// FireOutcome.AfterRecord, or are released when no tracker starts.
	if !s.execRuns.acquire(fireID) {
		releaseUnboundClaim()
		return hubfire.FireOutcome{}, a.skipExecFire(ctx, req, p, execReasonCapacity, "tracked_run_capacity")
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.execRuns.release(fireID)
		}
	}()
	workCtx, endWork, admitted := s.execRuns.work.begin()
	if !admitted {
		releaseUnboundClaim()
		log.Warn("Scheduler: exec event not started: the hub is shutting down")
		return hubfire.FireOutcome{}, errors.New(execReasonShuttingDown)
	}
	defer endWork()

	executor, _ := ExecutorContextFromContext(ctx)
	attr := req.Authority.Attribution
	now := time.Now()
	timeout := p.timeout()
	run := store.ScheduledExecRun{
		ProjectID:               req.ProjectID,
		ScheduleID:              scheduleID,
		OverlapKey:              overlapKey,
		TargetAgentID:           target.ID,
		InitiatorPrincipalKind:  attr.InitiatorPrincipalKind,
		InitiatorPrincipalID:    attr.InitiatorPrincipalID,
		InitiatorCredentialKind: attr.InitiatorCredentialKind,
		InitiatorCredentialID:   attr.InitiatorCredentialID,
		AuthorizationRevision:   attr.AuthorizationRevision,
		ExecutorKind:            executor.Kind,
		ExecutorID:              executor.ID,
		CommandSHA256:           agentcontrol.ExecArgvDigest(p.Command),
		Argc:                    len(p.Command),
		TimeoutSeconds:          timeout,
		StartedAt:               now,
		NotAfter:                now.Add(execSeamDuration(&scheduledExecNotAfter)),
		LeaseUntil:              now.Add(time.Duration(timeout)*time.Second + execSeamDuration(&scheduledExecLeaseGrace)),
		Outcome:                 store.ScheduledExecOutcomeStarting,
		CaptureMode:             p.CaptureOutput,
		Runtime:                 target.Runtime,
		LaunchID:                target.RunID,
	}
	if isTrigger {
		run.FireID = fireID
	} else {
		run.EventID = fireID
	}
	if err := s.store.CreateScheduledExecRun(ctx, &run); err != nil {
		releaseUnboundClaim()
		if errors.Is(err, store.ErrScheduledExecOverlap) {
			return hubfire.FireOutcome{}, a.skipExecFire(ctx, req, p, execReasonOverlap, "db_overlap_lease")
		}
		if errors.Is(err, store.ErrScheduledExecEventClosed) {
			// Unadmitted-event recovery or shutdown already closed this event
			// (and recorded its outcome on any parent schedule); send nothing
			// and do not overwrite the recorded outcome (ptone/scion#8210).
			log.Warn("Scheduler: exec event already closed before run admission; nothing started")
			return hubfire.FireOutcome{
				Status:        hubfire.FireStatusFired,
				ResultRef:     fireID,
				TargetAgentID: target.ID,
				Detached:      true,
			}, nil
		}
		// Nothing was sent. An existing run means this event was admitted
		// before: it is never started again.
		log.Error("Scheduler: exec run record not written; nothing started", "error", err)
		return hubfire.FireOutcome{}, errors.New(execReasonDispatchFailed)
	}
	s.auditScheduledExec(ctx, newExecFireAudit(ctx, ScheduledExecAuditAdmit, req, p))
	scheduledExecFireStage(scheduledExecStageAdmitted, fireID)

	// From here on the run row exists, so another replica's sweeper may
	// record the outcome at any moment. Every outcome is therefore recorded
	// here through the run's compare-and-set, with the owning event/fire in
	// the same transaction, and Detached: true tells the caller to write
	// nothing after Fire so a recorded outcome is never overwritten
	// (R1, ptone/scion#7883, em-2 invariant (b)).
	terminal := func(outcome string, res *agentcontrol.ExecResult, eventStatus, reason string, fireStatus hubfire.FireStatus) (hubfire.FireOutcome, error) {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		if _, err := s.finishScheduledExecRun(recordCtx, run, outcome, res, eventStatus, reason); err != nil {
			// The run stays unfinished; the sweeper resolves it.
			log.Warn("Scheduler: exec run outcome not recorded at fire; left to the sweeper", "error", err)
		}
		return hubfire.FireOutcome{
			Status:        fireStatus,
			Error:         reason,
			ResultRef:     fireID,
			TargetAgentID: target.ID,
			Detached:      true,
		}, nil
	}
	skipRun := func(reason, detail string) (hubfire.FireOutcome, error) {
		_ = a.skipExecFire(ctx, req, p, reason, detail)
		return terminal(store.ScheduledExecOutcomeSkipped, nil, store.ScheduledEventSkipped, reason, hubfire.FireStatusSkipped)
	}
	failRun := func(reason string) (hubfire.FireOutcome, error) {
		return terminal(store.ScheduledExecOutcomeError, nil, store.ScheduledEventFailed, reason, hubfire.FireStatusFailed)
	}
	track := func() (hubfire.FireOutcome, error) {
		handedOff = true
		return hubfire.FireOutcome{
			Status:        hubfire.FireStatusFired,
			ResultRef:     fireID,
			TargetAgentID: target.ID,
			AfterRecord:   func(context.Context) { s.trackScheduledExecRun(run) },
			Detached:      true,
		}, nil
	}

	// Dispatch guard (P3-R1): a fire delayed after the insert must not
	// start a run that recovery has already finished, or whose frozen
	// start deadline has passed. A pause between this check and the send
	// cannot start it late either: exec-start carries the frozen
	// not_after, and sciontool refuses a start received after not_after +
	// tolerance, which is also when recovery treats it as absent.
	checkCtx, cancelCheck := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
	current, checkErr := s.store.GetScheduledExecRun(checkCtx, fireID)
	cancelCheck()
	switch {
	case checkErr != nil:
		log.Warn("Scheduler: exec run not re-read before dispatch; nothing sent", "error", checkErr)
		return failRun(execReasonDispatchFailed)
	case current.FinishedAt != nil:
		log.Warn("Scheduler: exec run already finished before dispatch; nothing sent", "outcome", current.Outcome)
		return hubfire.FireOutcome{
			Status:        hubfire.FireStatusFired,
			ResultRef:     fireID,
			TargetAgentID: target.ID,
			Detached:      true,
		}, nil
	case time.Now().After(run.NotAfter):
		log.Warn("Scheduler: exec run start deadline passed before dispatch; nothing sent")
		return failRun(execReasonStartExpired)
	}
	scheduledExecFireStage(scheduledExecStageChecked, fireID)

	// One exec-start attempt, with the frozen deadline. It runs past the
	// fire handler's deadline on the call's own timeout, and is cancelled
	// only when the shutdown gives up waiting (workCtx); an ambiguous
	// result is only ever polled.
	startCtx, cancelStart := context.WithCancel(context.WithoutCancel(ctx))
	stopStart := context.AfterFunc(workCtx, cancelStart)
	res, startErr := s.StartAgentExecRun(startCtx, target.ID, target.RunID, agentcontrol.ExecStartRequest{
		RunID:          fireID,
		ScheduleID:     sciontoolScopeID,
		Argv:           p.Command,
		TimeoutS:       timeout,
		NotAfterUnixMS: run.NotAfter.UnixMilli(),
		Capture:        p.CaptureOutput,
		TriggerContext: ea.triggerContext,
	})
	stopStart()
	cancelStart()
	scheduledExecFireStage(scheduledExecStageStarted, fireID)

	if startErr != nil {
		switch {
		case errors.Is(startErr, ErrExecSchedulesDisabled):
			return skipRun(execReasonDisabled, "experiment_off_at_start")
		case errors.Is(startErr, errExecCaptureUnsupported):
			return failRun(execReasonCaptureUnsupported)
		case errors.Is(startErr, ErrAgentExecContextUnsupported):
			return failRun(execReasonContextUnsupported)
		case errors.Is(startErr, router.ErrNoSession), errors.Is(startErr, ErrAgentControlNotAdvertised),
			errors.Is(startErr, ErrAgentExecLaunchChanged), errors.Is(startErr, store.ErrNotFound):
			return skipRun(execReasonUnavailable, "executor_unavailable")
		case isExecFieldError(startErr):
			return failRun(errScheduledExecPayloadInvalid.Error())
		case errors.Is(startErr, agentcontrol.ErrExecSchema):
			log.Warn("Scheduler: exec-start answer outside the schema", "error", startErr)
			return failRun(execReasonDispatchFailed)
		default:
			// Timeout or transport error after the send: the run may have
			// started. Never resend; the tracker resolves it.
			log.Warn("Scheduler: exec-start result unknown; tracking the run", "error", startErr)
			return track()
		}
	}

	switch res.Status {
	case http.StatusAccepted, http.StatusOK:
		if res.Finished() {
			// For a capture run, a finished exec-start answer (200 replay)
			// never carries output; hand off to the tracker so exec-status
			// fetches outcome plus output before the CAS runs. start_failed
			// has no output and still finishes immediately.
			if run.CaptureMode != "" && res.Outcome != agentcontrol.ExecOutcomeStartFailed {
				return track()
			}
			// start_failed at 202, or a replay of a finished non-capture run.
			outcome, status, reason := scheduledExecFinalOutcome(res, timeout)
			fireStatus := hubfire.FireStatusFired
			if status == store.ScheduledEventFailed {
				fireStatus = hubfire.FireStatusFailed
			}
			return terminal(outcome, &res, status, reason, fireStatus)
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		if changed, err := s.store.MarkScheduledExecRunRunning(recordCtx, fireID); err != nil {
			slog.Warn("Scheduler: failed to mark scheduled exec run running",
				"eventID", fireID, "scheduleID", scheduleID, "error", err)
		} else if changed {
			run.Outcome = store.ScheduledExecOutcomeRunning
		}
		cancel()
		return track()
	case http.StatusConflict:
		switch res.Outcome {
		case agentcontrol.ExecOutcomeOverlap:
			return skipRun(execReasonOverlap, "agent_overlap")
		case agentcontrol.ExecOutcomeBusy:
			return skipRun(execReasonCapacity, "agent_busy")
		default: // mismatch
			return failRun(execReasonDispatchFailed)
		}
	case http.StatusRequestTimeout:
		return failRun(execReasonStartExpired)
	default: // 400
		return failRun(errScheduledExecPayloadInvalid.Error())
	}
}
