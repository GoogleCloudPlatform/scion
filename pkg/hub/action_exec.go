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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// execAction implements FireAction, FirePayloadValidator, FirePreauthorizer,
// and FireAuthorityRefusalHandler for deferred "exec" fires executed in a
// running target agent's container over its Conduit session.
//
// Fire sequence through fireAction:
//  1. ValidatePayload decodes the stored payload strictly and requires an
//     authoritative agentId, failing closed before authority resolution.
//  2. Preauthorize skips when hub.exec_schedules / hub.conduit is disabled or
//     when req.FireAt is older than scheduledExecMaxWindow.
//  3. resolveRecordedAuthority (run by fireAction) resolves the recorded
//     authorization revision; OnAuthorityRefused emits schedule.exec.refused
//     and returns execReasonRefused on denial.
//  4. Authorize runs authorizeExecFire (target lookup by agentId, frozen
//     ceiling, live agent.attach via authorizeAgentTargetAction, and
//     fireStanding) followed by target preflight (not managed, running phase,
//     not reincarnating). The Authorize/Fire seam sits immediately after
//     preflight (Stage 1 design §5.2, §17).
//  5. Fire owns admission, scheduled_exec_runs row insertion (keyed on
//     event_id), the dispatch guard, the single exec-start call, atomic run +
//     owner compare-and-set completion, and tracker handoff via
//     FireOutcome.AfterRecord.
type execAction struct {
	srv *Server
}

type execAuthorized struct {
	payload *scheduledExecPayload
	target  *store.Agent
	auth    FireAuthority
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
	// Stage 1 keeps event-owned exec runs (UNIQUE scheduled_exec_runs.event_id,
	// owner+run CAS, and sweeper/unadmitted recovery via event_id); trigger
	// runs require the Stage 2 addendum P8 fire_id owner extension and a
	// dedicated overlap_key, never schedule_id (§15 PR 2, rev 8.23). Fail closed
	// before any schedule.exec.* skip or refusal audit is emitted.
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		slog.Warn("exec fire refused: trigger-owned exec runs are not wired in Stage 1",
			"definitionID", req.Authority.DefinitionID,
			"fireID", req.Authority.FireID,
			"projectID", req.ProjectID)
		return &FireRefusal{Reason: execReasonRefused}, nil
	}
	p, err := unmarshalExecFirePayload(req.Payload)
	if err != nil {
		return nil, errScheduledExecPayloadInvalid
	}
	if !a.srv.execSchedulesEnabled() {
		return nil, a.skipExecFire(ctx, req, p, execReasonDisabled, "experiment_off")
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
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		slog.Warn("exec fire refused: trigger-owned exec runs are not wired in Stage 1",
			"definitionID", req.Authority.DefinitionID,
			"fireID", req.Authority.FireID,
			"projectID", req.ProjectID)
		return nil, &FireRefusal{Reason: execReasonRefused}, nil
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
	if phaseErr := a.srv.validateAgentDeliverable(target); phaseErr != nil {
		return nil, nil, a.skipExecFire(ctx, req, p, execReasonTargetNotRunning, "phase_"+store.EffectiveRestingPhase(target))
	}
	if reincarnationInFlight(target) {
		return nil, nil, a.skipExecFire(ctx, req, p, execReasonTargetNotRunning, "reincarnating")
	}

	return &execAuthorized{
		payload: p,
		target:  target,
		auth:    auth,
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
	// Trigger-owned runs are refused in Preauthorize / Authorize; guard direct
	// Fire calls with the same generic refusal text.
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger {
		return hubfire.FireOutcome{}, errors.New(execReasonRefused)
	}
	var scheduleID string
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = req.Authority.DefinitionID
	}

	log := slog.With("subsystem", "scheduler", "eventID", fireID, "projectID", req.ProjectID)

	// Admission: the replica's tracked-run bound (never waits) and the
	// shutdown gate. The slot and ownership pass to the tracker via
	// FireOutcome.AfterRecord, or are released when no tracker starts.
	if !s.execRuns.acquire(fireID) {
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
		log.Warn("Scheduler: exec event not started: the hub is shutting down")
		return hubfire.FireOutcome{}, errors.New(execReasonShuttingDown)
	}
	defer endWork()

	executor, _ := ExecutorContextFromContext(ctx)
	attr := req.Authority.Attribution
	now := time.Now()
	timeout := p.timeout()
	run := store.ScheduledExecRun{
		EventID:                 fireID,
		ProjectID:               req.ProjectID,
		ScheduleID:              scheduleID,
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
		Runtime:                 target.Runtime,
		LaunchID:                target.RunID,
	}
	if err := s.store.CreateScheduledExecRun(ctx, &run); err != nil {
		if errors.Is(err, store.ErrScheduledExecOverlap) {
			return hubfire.FireOutcome{}, a.skipExecFire(ctx, req, p, execReasonOverlap, "db_overlap_lease")
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
	// here through the run's compare-and-set, with the owning event in the same
	// transaction (and the event only while it is still fired or expired),
	// and Detached: true tells the caller to write nothing after Fire so a
	// recorded outcome is never overwritten (R1, ptone/scion#7883).
	terminal := func(outcome string, res *agentcontrol.ExecResult, eventStatus, reason string, fireStatus hubfire.FireStatus) (hubfire.FireOutcome, error) {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		if _, err := s.finishScheduledExecRun(recordCtx, run, outcome, res, eventStatus, reason); err != nil {
			// The run stays unfinished; the sweeper resolves it.
			log.Warn("Scheduler: exec run outcome not recorded at fire; left to the sweeper", "error", err)
		}
		return hubfire.FireOutcome{
			Status:    fireStatus,
			Error:     reason,
			ResultRef: fireID,
			Detached:  true,
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
			Status:      hubfire.FireStatusFired,
			ResultRef:   fireID,
			AfterRecord: func(context.Context) { s.trackScheduledExecRun(run) },
			Detached:    true,
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
			Status:    hubfire.FireStatusFired,
			ResultRef: fireID,
			Detached:  true,
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
		ScheduleID:     scheduleID,
		Argv:           p.Command,
		TimeoutS:       timeout,
		NotAfterUnixMS: run.NotAfter.UnixMilli(),
	})
	stopStart()
	cancelStart()
	scheduledExecFireStage(scheduledExecStageStarted, fireID)

	if startErr != nil {
		switch {
		case errors.Is(startErr, ErrExecSchedulesDisabled):
			return skipRun(execReasonDisabled, "experiment_off_at_start")
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
			// start_failed at 202, or a replay of a finished run.
			outcome, status, reason := scheduledExecFinalOutcome(res, timeout)
			fireStatus := hubfire.FireStatusFired
			if status == store.ScheduledEventFailed {
				fireStatus = hubfire.FireStatusFailed
			}
			return terminal(outcome, &res, status, reason, fireStatus)
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		if changed, err := s.store.MarkScheduledExecRunRunning(recordCtx, fireID); err == nil && changed {
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
