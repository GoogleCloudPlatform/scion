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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CommandRunLauncher hands an approved command run to the executor
// asynchronously after the approval transaction commits.
type CommandRunLauncher interface {
	EnqueueCommandRun(req *store.CommandRequest, run *store.CommandRun)
}

// Command-run executor and sweeper bounds. Variables only as test seams: tests
// that change them must not use t.Parallel and must restore them via t.Cleanup.
var (
	commandRunMaxTracked       = 256
	commandRunPollInitial      = 1 * time.Second
	commandRunPollMax          = 10 * time.Second
	commandRunStopGrace        = 2 * time.Second
	commandRunNotAfter         = agentcontrol.ExecNotAfter
	commandRunLeaseGrace       = 120 * time.Second
	commandRunSweepBatch       = 100
	commandRunSweepMaxPage     = 10
	commandRunSweepInFlight    = 8
	commandRunPollClaimTTL     = 10 * time.Second
	commandRunWakeTimeout      = commandRunWakeBudget
	commandRunReadyPollInitial = 1 * time.Second
	commandRunReadyPollMax     = 5 * time.Second
	commandRunOutputRetention  = 7 * 24 * time.Hour
)

// Stages of a command run at which commandRunExecHook runs (test seam only).
const (
	commandRunStageResolved       = "resolved"
	commandRunStageWaking         = "waking"
	commandRunStageReady          = "ready"
	commandRunStageBeforeDispatch = "before_dispatch"
	commandRunStageDispatched     = "dispatched"
	commandRunStageStarted        = "started"
)

// commandRunExecHook, when set in tests, runs at each executor stage to test
// deterministic interleavings and hub-restart recovery (AC-S4). Production
// never sets it.
var commandRunExecHook func(stage, runID string)

func commandRunExecStage(stage, runID string) {
	if hook := commandRunExecHook; hook != nil {
		hook(stage, runID)
	}
}

// commandRunTracker is the replica's in-memory tracker for approved command
// runs (bounded slots + shutdown gate, mirroring scheduledExecRunTracker).
type commandRunTracker struct {
	work brokerWorkTracker

	mu    sync.Mutex
	owned map[string]struct{}
	used  int
}

// acquire reserves a slot for runID. It returns (acquired, full).
func (t *commandRunTracker) acquire(runID string) (acquired bool, full bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.owned[runID]; ok {
		return false, false
	}
	if t.used >= execSeamInt(&commandRunMaxTracked) {
		return false, true
	}
	if t.owned == nil {
		t.owned = make(map[string]struct{})
	}
	t.owned[runID] = struct{}{}
	t.used++
	return true, false
}

func (t *commandRunTracker) release(runID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.owned[runID]; !ok {
		return
	}
	delete(t.owned, runID)
	t.used--
}

func (t *commandRunTracker) ownedIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.owned) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.owned))
	for id := range t.owned {
		out = append(out, id)
	}
	return out
}

// stopCommandRuns cancels in-flight command-run trackers during hub shutdown
// without recording outcomes; the sweeper resolves unfinished runs after restart.
func (s *Server) stopCommandRuns(ctx context.Context) {
	if n := s.commandRuns.work.stop(ctx, execSeamDuration(&commandRunStopGrace)); n > 0 {
		slog.Info("Command runs still tracked at shutdown; the sweeper resolves them after restart",
			"running", n)
	}
}

// EnqueueCommandRun implements CommandRunLauncher. It schedules the run on this
// replica's bounded commandRunTracker. If all slots are full, the run is
// finished as not_started/hub_busy; it is never left queued for another replica
// (design §4.8). If hub shutdown has already closed admission, the run is left
// unfinished for the sweeper to resolve after restart.
func (s *Server) EnqueueCommandRun(req *store.CommandRequest, run *store.CommandRun) {
	if s == nil || run == nil {
		return
	}
	runCopy := *run
	var reqCopy *store.CommandRequest
	if req != nil {
		c := *req
		reqCopy = &c
	}

	acquired, full := s.commandRuns.acquire(runCopy.ID)
	if !acquired {
		if full {
			_ = s.commandRuns.work.goTracked(func(ctx context.Context) {
				recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
				defer cancel()
				_, _ = s.finishCommandRun(recordCtx, reqCopy, &runCopy, []string{store.CommandRunStatusQueued}, store.CommandRunStatusNotStarted, "hub_busy", nil, "")
			})
		}
		return
	}

	if !s.commandRuns.work.goTracked(func(ctx context.Context) {
		defer s.commandRuns.release(runCopy.ID)
		s.executeCommandRun(ctx, reqCopy, &runCopy)
	}) {
		// Hub shutdown closed admission: release slot and leave unfinished for
		// the sweeper after restart (design §4.12).
		s.commandRuns.release(runCopy.ID)
	}
}

// commandRunArgv builds the fixed Conduit exec argv from the stored request
// script (design §4.6): no implicit set -e / pipefail; what the card showed is
// what runs.
func commandRunArgv(script string) []string {
	return []string{"/bin/bash", "--login", "-c", script, "scion-command"}
}

// executeCommandRun runs design §4.8 (steps 1–7): resolve container, wake
// suspended containers (never auto-starting stopped containers, D5), wait for
// Conduit exec readiness, re-resolve & pin, CAS to dispatching before send,
// send exec-start once, and track to completion.
func (s *Server) executeCommandRun(ctx context.Context, req *store.CommandRequest, run *store.CommandRun) {
	if ctx.Err() != nil {
		return
	}

	// Always load the stored request row so argv is built from the immutable
	// persisted script, never from client input (§4.3, AC-S3).
	if req == nil || req.Script == "" {
		loaded, err := s.store.GetCommandRequest(ctx, run.RequestID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("Command run: failed to load request row", "run_id", run.ID, "request_id", run.RequestID, "error", err)
			_, _ = s.finishCommandRun(ctx, req, run, []string{store.CommandRunStatusQueued, store.CommandRunStatusWaking}, store.CommandRunStatusNotStarted, "request_missing", nil, "")
			return
		}
		req = loaded
	}

	finishBeforeDispatch := func(reason string) {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		_, _ = s.finishCommandRun(recordCtx, req, run, []string{store.CommandRunStatusQueued, store.CommandRunStatusWaking}, store.CommandRunStatusNotStarted, reason, nil, "")
	}
	finishAfterDispatch := func(status, reason string, res *agentcontrol.ExecResult, fallbackOutputStatus string) {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		_, _ = s.finishCommandRun(recordCtx, req, run, []string{store.CommandRunStatusDispatching}, status, reason, res, fallbackOutputStatus)
	}

	// Step 1: resolve the approver's execution container.
	r1, err := s.ResolveExecContainer(ctx, run.ApproverUserID)
	if ctx.Err() != nil {
		return
	}
	if err != nil || r1.Agent == nil || r1.Agent.ID != run.ExecAgentID {
		finishBeforeDispatch("container_changed")
		return
	}

	wakeBudget := execSeamDuration(&commandRunWakeTimeout)
	wakeBase := run.CreatedAt
	if wakeBase.IsZero() {
		wakeBase = time.Now().UTC()
	}
	wakeDeadline := wakeBase.Add(wakeBudget)

	// Step 2: never auto-start stopped containers (D5); resume suspended
	// containers as the approver after re-checking agent-lifecycle permission.
	switch r1.State {
	case ExecContainerStateStopped:
		finishBeforeDispatch("container_stopped")
		return
	case ExecContainerStateSuspended:
		approver, uerr := s.store.GetUser(ctx, run.ApproverUserID)
		if ctx.Err() != nil {
			return
		}
		if uerr != nil || approver == nil {
			finishBeforeDispatch("container_changed")
			return
		}
		approverIdent := NewAuthenticatedUser(approver.ID, approver.Email, approver.DisplayName, approver.Role, "internal")
		if !s.agentLifecycleAllowed(ctx, approverIdent, r1.Agent) {
			finishBeforeDispatch("wake_not_permitted")
			return
		}
		wctx, cancelWakeCAS := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		wonWake, werr := s.store.TransitionCommandRunToWaking(wctx, run.ID, store.CommandRunWakeResume)
		cancelWakeCAS()
		if werr != nil || !wonWake {
			// Sweeper or another writer already finished or moved the row.
			return
		}
		run.Status = store.CommandRunStatusWaking
		run.Wake = store.CommandRunWakeResume
		s.emitCommandRunAudit(context.WithoutCancel(ctx), req, run, msgaction.EventCmdRunWaking, "", nil)
		if req != nil {
			s.publishActionUpdated(msgaction.KindCommandRequest, req.ProjectID, req.MessageID, req.ChatKey, commandRequestSSEView(req, nil, run))
		}
		commandRunExecStage(commandRunStageWaking, run.ID)
		if ctx.Err() != nil {
			return
		}

		agent := r1.Agent
		if agent.IdleSuspendClaimActive || agent.IdleSuspendClaimUntil != nil {
			settleCtx, cancelSettle := context.WithDeadline(ctx, wakeDeadline)
			settleErr := s.waitForIdleSuspendToSettle(settleCtx, agent)
			cancelSettle()
			if ctx.Err() != nil {
				return
			}
			if settleErr != nil {
				finishBeforeDispatch("wake_failed")
				return
			}
		}

		switch mapExecContainerState(agent) {
		case ExecContainerStateStopped:
			finishBeforeDispatch("container_stopped")
			return
		case ExecContainerStateSuspended:
			if isManagedAgentRuntime(agent.Runtime) {
				finishBeforeDispatch("wake_failed")
				return
			}
			// Re-check lifecycle permission against the refreshed row after
			// any idle-suspend settle wait so a revocation during the wait
			// still refuses the wake before startAgentCore.
			if !s.agentLifecycleAllowed(ctx, approverIdent, agent) {
				finishBeforeDispatch("wake_not_permitted")
				return
			}
			startCtx, cancelStart := context.WithDeadline(ctx, wakeDeadline)
			startErr := s.startAgentCore(startCtx, agent, StartOpts{
				Kind:               store.StartClaimWake,
				Resume:             true,
				KeepCallerDeadline: true,
				SyncDispatchBound:  true,
			})
			cancelStart()
			if ctx.Err() != nil {
				return
			}
			if startErr != nil && !s.isCommandRunStartInProgress(ctx, agent.ID, startErr) {
				if errors.Is(startErr, store.ErrNotFound) || errors.Is(startErr, store.ErrDeleteInProgress) {
					finishBeforeDispatch("container_changed")
					return
				}
				if cur, gerr := s.store.GetAgent(ctx, agent.ID); errors.Is(gerr, store.ErrNotFound) || (gerr == nil && (cur == nil || !cur.DeletedAt.IsZero())) {
					finishBeforeDispatch("container_changed")
					return
				} else if gerr == nil && mapExecContainerState(cur) == ExecContainerStateStopped {
					finishBeforeDispatch("container_stopped")
					return
				}
				slog.Warn("Command run: wake resume failed",
					"run_id", run.ID, "exec_agent_id", run.ExecAgentID, "error", startErr)
				finishBeforeDispatch("wake_failed")
				return
			}
		case ExecContainerStateReady, ExecContainerStateStarting:
			// Already starting or running after idle-suspend settled; just wait.
		default:
			finishBeforeDispatch("wake_failed")
			return
		}
	case ExecContainerStateStarting, ExecContainerStateReady:
		// Already starting or ready; wait for Conduit exec readiness in step 3.
	default:
		finishBeforeDispatch("container_not_running")
		return
	}
	commandRunExecStage(commandRunStageResolved, run.ID)
	if ctx.Err() != nil {
		return
	}

	// Step 3: wait for a Conduit session of (agent, RunID) advertising the exec
	// routes and required features (exec_capture.v1, exec_workdir.v1).
	sessionLaunchID, readyFailReason := s.waitForConduitExecReady(ctx, run.ExecAgentID, wakeDeadline)
	if ctx.Err() != nil {
		return
	}
	if readyFailReason != "" {
		finishBeforeDispatch(readyFailReason)
		return
	}
	commandRunExecStage(commandRunStageReady, run.ID)
	if ctx.Err() != nil {
		return
	}

	// Step 4: re-resolve and pin the live launch RunID (must match session incarnation).
	r2, err := s.ResolveExecContainer(ctx, run.ApproverUserID)
	if ctx.Err() != nil {
		return
	}
	if err != nil || r2.State != ExecContainerStateReady || r2.Agent == nil || r2.Agent.ID != run.ExecAgentID || r2.RunID == "" || r2.RunID != sessionLaunchID {
		finishBeforeDispatch("container_changed")
		return
	}

	commandRunExecStage(commandRunStageBeforeDispatch, run.ID)
	if ctx.Err() != nil {
		return
	}

	// Step 5: CAS queued|waking -> dispatching BEFORE sending exec-start.
	now := time.Now().UTC()
	timeoutS := run.TimeoutS
	if timeoutS <= 0 {
		timeoutS = req.TimeoutS
	}
	if timeoutS <= 0 {
		timeoutS = messages.DefaultCommandRequestTimeoutS
	}
	notAfter := now.Add(execSeamDuration(&commandRunNotAfter))
	leaseUntil := now.Add(time.Duration(timeoutS)*time.Second + execSeamDuration(&commandRunLeaseGrace))

	dispatchCtx, cancelDispatch := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
	won, err := s.store.TransitionCommandRunToDispatching(dispatchCtx, &store.CommandRunDispatchInput{
		RunID:        run.ID,
		LaunchID:     r2.RunID,
		NotAfter:     notAfter,
		LeaseUntil:   leaseUntil,
		DispatchedAt: now,
	})
	cancelDispatch()
	if err != nil {
		slog.Warn("Command run: failed to commit dispatching state; nothing sent", "run_id", run.ID, "error", err)
		return
	}
	if !won {
		// Sweeper or another writer already finished or moved the row: send nothing.
		return
	}

	run.Status = store.CommandRunStatusDispatching
	run.LaunchID = r2.RunID
	run.NotAfter = &notAfter
	run.LeaseUntil = leaseUntil
	run.DispatchedAt = &now

	argv := commandRunArgv(req.Script)
	workdir := req.Workdir
	if workdir == "" {
		workdir = "~"
	}
	captureMode := run.CaptureMode
	if captureMode == "" {
		captureMode = agentcontrol.ExecCaptureTail
	}
	run.CaptureMode = captureMode

	emitDispatched := func() {
		s.emitCommandRunAudit(context.WithoutCancel(ctx), req, run, msgaction.EventCmdRunDispatched, "", map[string]string{
			"exec_argv_sha256": agentcontrol.ExecArgvDigest(argv),
			"workdir_kind":     messages.CommandWorkdirKind(workdir),
		})
	}

	commandRunExecStage(commandRunStageDispatched, run.ID)
	if ctx.Err() != nil {
		// Shutdown after committing dispatching, before send: sweeper resolves.
		return
	}

	// Step 6: one exec-start attempt ever. Ambiguous errors after send never
	// resend; they fall through to polling (Step 7).
	res, startErr := s.StartAgentExecRun(ctx, execOwnerCommandRun, run.ExecAgentID, run.LaunchID, agentcontrol.ExecStartRequest{
		RunID:          run.ID,
		Argv:           argv,
		TimeoutS:       timeoutS,
		NotAfterUnixMS: notAfter.UnixMilli(),
		Capture:        captureMode,
		Workdir:        workdir,
	})
	commandRunExecStage(commandRunStageStarted, run.ID)

	if startErr != nil {
		if ctx.Err() != nil {
			// Shutdown cancelled the wait: leave unfinished for the sweeper.
			return
		}
		switch {
		case errors.Is(startErr, ErrCommandRequestsDisabled):
			finishAfterDispatch(store.CommandRunStatusNotStarted, "disabled", nil, "")
			return
		case errors.Is(startErr, ErrCommandRunTargetNotShell),
			errors.Is(startErr, ErrAgentExecLaunchChanged),
			errors.Is(startErr, store.ErrNotFound):
			finishAfterDispatch(store.CommandRunStatusNotStarted, "container_changed", nil, "")
			return
		case errors.Is(startErr, ErrAgentExecWorkdirUnsupported),
			errors.Is(startErr, errExecCaptureUnsupported),
			errors.Is(startErr, ErrAgentControlNotAdvertised):
			finishAfterDispatch(store.CommandRunStatusNotStarted, "exec_unsupported", nil, "")
			return
		case errors.Is(startErr, router.ErrNoSession):
			finishAfterDispatch(store.CommandRunStatusNotStarted, "container_not_running", nil, "")
			return
		case isExecFieldError(startErr):
			finishAfterDispatch(store.CommandRunStatusNotStarted, "invalid_request", nil, "")
			return
		default:
			// Timeout, transport drop, or schema error after send: ambiguous.
			// Never resend; track via exec-status only.
			emitDispatched()
			slog.Warn("Command run: exec-start result ambiguous; tracking via exec-status",
				"run_id", run.ID, "exec_agent_id", run.ExecAgentID, "error", startErr)
			s.pollCommandRun(ctx, req, run)
			return
		}
	}

	// Non-pre-send answer from the agent: audit run_dispatched now (O2).
	emitDispatched()

	// Documented exec-start answer.
	switch res.Status {
	case http.StatusConflict:
		switch res.Outcome {
		case agentcontrol.ExecOutcomeBusy, agentcontrol.ExecOutcomeOverlap:
			finishAfterDispatch(store.CommandRunStatusNotStarted, res.Outcome, nil, "")
		default:
			// Conservative mapping (O4): any 409 other than busy/overlap (e.g.
			// mismatch) means a run with this ID may already exist on the agent.
			reason := res.Outcome
			if reason == "" {
				reason = "conflict"
			}
			finishAfterDispatch(store.CommandRunStatusUnknown, reason, nil, agentcontrol.ExecOutputLost)
		}
		return
	case http.StatusRequestTimeout:
		finishAfterDispatch(store.CommandRunStatusNotStarted, agentcontrol.ExecOutcomeExpiredBeforeStart, nil, "")
		return
	case http.StatusBadRequest:
		finishAfterDispatch(store.CommandRunStatusNotStarted, "invalid_request", nil, "")
		return
	case http.StatusAccepted, http.StatusOK:
		if res.Finished() && res.Outcome == agentcontrol.ExecOutcomeStartFailed {
			finishAfterDispatch(store.CommandRunStatusNotStarted, agentcontrol.ExecOutcomeStartFailed, &res, "")
			return
		}
		if res.State == agentcontrol.ExecStateRunning {
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
			wonRun, terr := s.store.TransitionCommandRunToRunning(tctx, run.ID)
			cancel()
			// Monotonic transition: sciontool confirmed the run is running, so
			// advance the local snapshot even if another replica's sweeper won
			// the DB CAS first (R1).
			run.Status = store.CommandRunStatusRunning
			if terr == nil && wonRun && req != nil {
				s.publishActionUpdated(msgaction.KindCommandRequest, req.ProjectID, req.MessageID, req.ChatKey, commandRequestSSEView(req, nil, run))
			}
		}
		// Track via exec-status (even on a 200 finished replay, because
		// exec-start never includes captured output).
		s.pollCommandRun(ctx, req, run)
	default:
		// Unexpected status: treat as ambiguous and poll.
		s.pollCommandRun(ctx, req, run)
	}
}

// isCommandRunStartInProgress reports whether a startAgentCore error indicates
// another start of agentID is already in progress elsewhere (design §4.8 step 2:
// do not start again; just wait in waitForConduitExecReady).
func (s *Server) isCommandRunStartInProgress(ctx context.Context, agentID string, err error) bool {
	if err == nil {
		return false
	}
	var held *store.ClaimHeldError
	if errors.As(err, &held) && held.Kind != store.StartClaimStop {
		return true
	}
	if errors.Is(err, ErrLaunchInFlight) {
		return true
	}
	if errors.Is(err, errStartingWrite) && errors.Is(err, store.ErrPhaseMismatch) {
		if fresh, gerr := s.store.GetAgent(ctx, agentID); gerr == nil && fresh != nil {
			st := mapExecContainerState(fresh)
			return st == ExecContainerStateStarting || st == ExecContainerStateReady
		}
	}
	return false
}

// waitForConduitExecReady polls conduitRouter() with 1s->5s exponential backoff
// until deadline for a Conduit session of (agentID, agent.RunID) advertising
// RouteExecStart, RouteExecStatus, FeatureExecCapture, and FeatureExecWorkdir
// (design §4.8 step 3).
//
// Returns (sessionLaunchID, "") when ready, ("", "") on ctx cancellation
// (shutdown), or ("", failReason) where failReason is one of "wake_timeout",
// "exec_unsupported", "container_stopped", or "container_changed".
func (s *Server) waitForConduitExecReady(ctx context.Context, agentID string, deadline time.Time) (string, string) {
	interval := execSeamDuration(&commandRunReadyPollInitial)
	longest := execSeamDuration(&commandRunReadyPollMax)
	for {
		if ctx.Err() != nil {
			return "", ""
		}
		agent, err := s.store.GetAgent(ctx, agentID)
		if ctx.Err() != nil {
			return "", ""
		}
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", "container_changed"
			}
		} else {
			if agent == nil || !agent.DeletedAt.IsZero() {
				return "", "container_changed"
			}
			st := mapExecContainerState(agent)
			if st == ExecContainerStateStopped {
				return "", "container_stopped"
			}
			if st == ExecContainerStateReady && agent.RunID != "" {
				if rtr := s.conduitRouter(); rtr != nil {
					res, rerr := rtr.Resolve(ctx, router.Request{
						Op:    router.OpStatefulRPC,
						Kind:  registry.PrincipalAgent,
						ID:    agent.ID,
						Want:  registry.Want{ProjectID: agent.ProjectID},
						Agent: agentIncarnationFacts(agent),
					}, nil)
					if ctx.Err() != nil {
						return "", ""
					}
					if rerr == nil && res.Session != nil &&
						res.Record.EndpointIncarnation == agent.RunID &&
						res.Want.Incarnation == agent.RunID {
						caps := res.Session.Info().Capabilities
						rpc := caps.GetRpc()
						features := caps.GetFeatures()
						if slices.Contains(rpc, agentcontrol.RouteExecStart) &&
							slices.Contains(rpc, agentcontrol.RouteExecStatus) &&
							slices.Contains(features, agentcontrol.FeatureExecCapture) &&
							slices.Contains(features, agentcontrol.FeatureExecWorkdir) {
							return agent.RunID, ""
						}
						return "", "exec_unsupported"
					}
				}
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", "wake_timeout"
		}
		wait := min(interval, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ""
		case <-timer.C:
		}
		interval = min(interval*2, longest)
	}
}

// mapCommandRunFinalOutcome maps a finished Conduit ExecResult to the
// command_runs status and reason per design §4.8.
func mapCommandRunFinalOutcome(res agentcontrol.ExecResult) (status, reason string) {
	switch res.Outcome {
	case agentcontrol.ExecOutcomeOK:
		return store.CommandRunStatusSucceeded, ""
	case agentcontrol.ExecOutcomeNonzero:
		return store.CommandRunStatusFailed, fmt.Sprintf("exit status %d", res.ExitCode)
	case agentcontrol.ExecOutcomeTimeout:
		return store.CommandRunStatusUnknown, "timeout"
	case agentcontrol.ExecOutcomeStartFailed:
		return store.CommandRunStatusNotStarted, agentcontrol.ExecOutcomeStartFailed
	default:
		return store.CommandRunStatusUnknown, "outcome_unknown"
	}
}

// pollCommandRun polls exec-status with exponential backoff until the run
// reaches a terminal state or ctx is cancelled by shutdown.
func (s *Server) pollCommandRun(ctx context.Context, req *store.CommandRequest, run *store.CommandRun) {
	interval := execSeamDuration(&commandRunPollInitial)
	longest := execSeamDuration(&commandRunPollMax)
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if s.pollCommandRunOnce(ctx, req, run, false) {
			return
		}
		interval = min(interval*2, longest)
	}
}

// pollCommandRunOnce polls exec-status once for run and records a terminal
// outcome when known. When pollPastLease is true (used by the sweeper for
// past-lease dispatching/running runs), any non-terminal answer after the poll
// finishes the run as unknown/lease_expired with output_status=lost.
func (s *Server) pollCommandRunOnce(ctx context.Context, req *store.CommandRequest, run *store.CommandRun, pollPastLease bool) bool {
	record := func(status, reason string, res *agentcontrol.ExecResult, fallbackOutputStatus string) bool {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		_, err := s.finishCommandRun(recordCtx, req, run, []string{store.CommandRunStatusDispatching, store.CommandRunStatusRunning}, status, reason, res, fallbackOutputStatus)
		return err == nil
	}

	if !pollPastLease && !run.LeaseUntil.IsZero() && time.Now().After(run.LeaseUntil) {
		return record(store.CommandRunStatusUnknown, "lease_expired", nil, agentcontrol.ExecOutputLost)
	}

	captureMode := run.CaptureMode
	if captureMode == "" {
		captureMode = agentcontrol.ExecCaptureTail
	}

	res, err := s.AgentExecRunStatus(ctx, run.ExecAgentID, run.LaunchID, run.ID, captureMode)
	if ctx.Err() != nil {
		return true // shutdown: leave unfinished for the sweeper
	}

	pollCtx, cancelPoll := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
	_ = s.store.UpdateCommandRunLastPolled(pollCtx, run.ID, time.Now().UTC())
	cancelPoll()

	if err != nil {
		switch {
		case errors.Is(err, ErrAgentExecLaunchChanged):
			return record(store.CommandRunStatusUnknown, "agent_restarted", nil, agentcontrol.ExecOutputLost)
		case errors.Is(err, store.ErrNotFound):
			return record(store.CommandRunStatusUnknown, "container_deleted", nil, agentcontrol.ExecOutputLost)
		case errors.Is(err, agentcontrol.ErrExecSchema):
			slog.Warn("Command run: exec-status answer outside schema", "run_id", run.ID, "error", err)
			return record(store.CommandRunStatusUnknown, "outcome_unknown", nil, agentcontrol.ExecOutputLost)
		case errors.Is(err, router.ErrNoSession), errors.Is(err, ErrAgentControlNotAdvertised):
			if pollPastLease {
				return record(store.CommandRunStatusUnknown, "lease_expired", nil, agentcontrol.ExecOutputLost)
			}
			return false
		default:
			if pollPastLease {
				return record(store.CommandRunStatusUnknown, "lease_expired", nil, agentcontrol.ExecOutputLost)
			}
			return false
		}
	}

	switch res.State {
	case agentcontrol.ExecStateFinished:
		status, reason := mapCommandRunFinalOutcome(res)
		return record(status, reason, &res, "")
	case agentcontrol.ExecStateRunning:
		if run.Status == store.CommandRunStatusDispatching {
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
			_, _ = s.store.TransitionCommandRunToRunning(tctx, run.ID)
			cancel()
			run.Status = store.CommandRunStatusRunning
		}
		if pollPastLease {
			return record(store.CommandRunStatusUnknown, "lease_expired", nil, agentcontrol.ExecOutputLost)
		}
		return false
	default: // ExecStateUnknown (404)
		// Re-read authoritative DB status before choosing between unknown and
		// not_started: another replica's sweeper may have moved the row to
		// running while our in-memory snapshot still reads dispatching (R1).
		if fresh, gerr := s.store.GetCommandRun(ctx, run.ID); gerr == nil && fresh != nil {
			if fresh.FinishedAt != nil {
				return true
			}
			run.Status = fresh.Status
		}
		if run.Status == store.CommandRunStatusRunning {
			// sciontool knew the run and has forgotten it: container restarted.
			return record(store.CommandRunStatusUnknown, "agent_restarted", nil, agentcontrol.ExecOutputLost)
		}
		if run.NotAfter != nil && time.Now().After(run.NotAfter.Add(execSeamDuration(&scheduledExecStartAbsentGrace))) {
			// Frozen start deadline + clock tolerance + grace has passed while
			// still dispatching: sciontool can never admit the start. Guard the
			// not_started finish to ExpectedStatuses={dispatching} only so a
			// concurrent dispatching->running transition can never be recorded
			// as not_started (R1).
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
			won, ferr := s.finishCommandRun(recordCtx, req, run, []string{store.CommandRunStatusDispatching}, store.CommandRunStatusNotStarted, "dispatch_failed", nil, "")
			cancel()
			if ferr != nil {
				return false
			}
			if won {
				return true
			}
			if fresh, gerr := s.store.GetCommandRun(ctx, run.ID); gerr == nil && fresh != nil {
				if fresh.FinishedAt != nil {
					return true
				}
				if fresh.Status == store.CommandRunStatusRunning {
					run.Status = store.CommandRunStatusRunning
					recordCtx2, cancel2 := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
					_, ferr2 := s.finishCommandRun(recordCtx2, req, run, []string{store.CommandRunStatusRunning}, store.CommandRunStatusUnknown, "agent_restarted", nil, agentcontrol.ExecOutputLost)
					cancel2()
					return ferr2 == nil
				}
			}
			return false
		}
		if pollPastLease {
			return record(store.CommandRunStatusUnknown, "lease_expired", nil, agentcontrol.ExecOutputLost)
		}
		return false
	}
}

// finishCommandRun validates captured output before the tx (C-a2), commits the
// finish CAS (finished_at IS NULL) + exec_run_outputs(owner_kind='action_run')
// in one transaction, and on a win emits audit events, SSE update, and the
// outcome-only command-response message to the asker.
func (s *Server) finishCommandRun(
	ctx context.Context,
	req *store.CommandRequest,
	run *store.CommandRun,
	expectedStatuses []string,
	status, reason string,
	res *agentcontrol.ExecResult,
	fallbackOutputStatus string,
) (bool, error) {
	now := time.Now().UTC()
	captureMode := run.CaptureMode
	if captureMode == "" {
		captureMode = agentcontrol.ExecCaptureTail
	}

	fin := store.FinishCommandRunInput{
		RunID:            run.ID,
		Status:           status,
		ExpectedStatuses: expectedStatuses,
		Reason:           reason,
		FinishedAt:       now,
	}
	if res != nil && res.Finished() && res.Outcome != agentcontrol.ExecOutcomeStartFailed {
		code, dur, outBytes := res.ExitCode, res.DurationMS, res.OutputBytes
		fin.ExitCode = &code
		fin.DurationMS = &dur
		fin.OutputBytes = &outBytes
		if err := agentcontrol.ValidateCapturedOutput(captureMode, res.OutputStatus, res.Output); err != nil {
			slog.Warn("Command run: captured output failed validation; recording lost",
				"run_id", run.ID, "output_status", res.OutputStatus, "error", err)
			fin.OutputStatus = agentcontrol.ExecOutputLost
		} else {
			fin.OutputStatus = res.OutputStatus
			fin.Output = res.Output
		}
	} else if fallbackOutputStatus != "" {
		fin.OutputStatus = fallbackOutputStatus
	}

	won, err := s.store.FinishCommandRun(ctx, &fin)
	if err != nil {
		slog.Warn("Command run: finish write failed", "run_id", run.ID, "status", status, "error", err)
		return false, err
	}
	if !won {
		return false, nil
	}

	run.Status = status
	run.Reason = reason
	run.ExitCode = fin.ExitCode
	run.DurationMS = fin.DurationMS
	run.OutputBytes = fin.OutputBytes
	run.OutputStatus = fin.OutputStatus
	run.FinishedAt = &now

	postCtx, cancelPost := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
	defer cancelPost()

	if req == nil && run.RequestID != "" {
		if loaded, lerr := s.store.GetCommandRequest(postCtx, run.RequestID); lerr == nil {
			req = loaded
		}
	}

	// 1. Audit event with allow-listed details only (never script text or output).
	auditDetails := map[string]string{
		"outcome":      status,
		"workdir_kind": messages.CommandWorkdirKind(run.Workdir),
	}
	if fin.ExitCode != nil {
		auditDetails["exit_code"] = strconv.Itoa(*fin.ExitCode)
	}
	if fin.DurationMS != nil {
		auditDetails["duration_ms"] = strconv.FormatInt(*fin.DurationMS, 10)
	}
	if fin.OutputBytes != nil {
		auditDetails["output_bytes"] = strconv.FormatInt(*fin.OutputBytes, 10)
	}
	if fin.OutputStatus != "" {
		auditDetails["output_status"] = fin.OutputStatus
	}
	if status == store.CommandRunStatusNotStarted && reason != "" {
		auditDetails["not_started_reason"] = reason
	}
	s.emitCommandRunAudit(postCtx, req, run, commandRunAuditEventType(status), reason, auditDetails)

	slog.Info("Command run finished",
		"run_id", run.ID,
		"exec_agent_id", run.ExecAgentID,
		"status", status,
		"reason", reason,
		"output_status", fin.OutputStatus)

	// 2. Publish live card update via SSE (uniform CommandRequestView shape, no output bytes or exec container IDs).
	if req != nil {
		s.publishActionUpdated(msgaction.KindCommandRequest, req.ProjectID, req.MessageID, req.ChatKey, commandRequestSSEView(req, nil, run))
	}

	// 3. Deliver command-response message to the asker agent (§4.9).
	// Output tail is included only when run.ShareOutput is true, after
	// best-effort redaction and 4 KiB UTF-8 bounding.
	if req != nil {
		s.deliverCommandRunOutcome(postCtx, req, run, fin.Output)
	}

	return true, nil
}

func commandRunAuditEventType(status string) string {
	switch status {
	case store.CommandRunStatusSucceeded:
		return msgaction.EventCmdRunSucceeded
	case store.CommandRunStatusFailed:
		return msgaction.EventCmdRunFailed
	case store.CommandRunStatusNotStarted:
		return msgaction.EventCmdRunNotStarted
	default:
		return msgaction.EventCmdRunUnknown
	}
}

func (s *Server) emitCommandRunAudit(ctx context.Context, req *store.CommandRequest, run *store.CommandRun, eventType, reason string, extra map[string]string) {
	details := map[string]string{
		"command_sha256":   run.CommandSHA256,
		"exec_agent_id":    run.ExecAgentID,
		"exec_project_id":  run.ExecProjectID,
		"run_id":           run.ID,
		"share_output":     strconv.FormatBool(run.ShareOutput),
		"approver_user_id": run.ApproverUserID,
	}
	if run.LaunchID != "" {
		details["exec_launch_id"] = run.LaunchID
	}
	if run.Wake != "" {
		details["wake"] = run.Wake
	}
	for k, v := range extra {
		if v != "" {
			details[k] = v
		}
	}
	ev := &MessageActionEvent{
		Kind:      msgaction.KindCommandRequest,
		EventType: eventType,
		ActorKind: MessageActionActorHub,
		Reason:    reason,
		Details:   details,
		Timestamp: time.Now().UTC(),
	}
	if req != nil {
		ev.RowID = req.ID
		ev.MessageID = req.MessageID
		ev.ProjectID = req.ProjectID
		ev.AgentID = req.AgentID
		ev.AgentName = req.AgentName
		ev.Key = req.Key
		ev.Version = req.Version
	}
	s.emitMessageActionEvent(ctx, ev)
}

// deliverCommandRunOutcome sends one command-response message to the asking
// agent via dispatchActionResponse (design §4.9). Output tail is attached only
// when run.ShareOutput is true, after best-effort redaction and 4 KiB UTF-8
// bounding. Deleting the asker makes delivery a no-op (C2).
func (s *Server) deliverCommandRunOutcome(ctx context.Context, req *store.CommandRequest, run *store.CommandRun, capturedOutput []byte) {
	if req.AgentID == "" {
		return
	}
	deliveredAt := time.Now().UTC()
	claimed, err := s.store.ClaimCommandRunOutcomeDelivery(ctx, run.ID, deliveredAt)
	if err != nil || !claimed {
		return
	}

	asker, err := s.store.GetAgent(ctx, req.AgentID)
	if err != nil || asker == nil || !asker.DeletedAt.IsZero() {
		return
	}

	var outputTail string
	var redactionCount int
	if run.ShareOutput {
		if len(capturedOutput) > 0 {
			outputTail, redactionCount = redactAndBoundCommandOutput(capturedOutput)
		} else {
			outputTail, redactionCount = s.loadSharedCommandRunOutputTail(ctx, run)
		}
	}

	respText := formatCommandRunOutcomeMessage(req, run, outputTail)
	respMsg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      req.ProjectID,
		ConversationID: req.ConversationID,
		ThreadID:       req.ChatKey,
		Sender:         "system:hub",
		SenderID:       "system:hub",
		Recipient:      "agent:" + asker.Slug,
		RecipientID:    asker.ID,
		Msg:            respText,
		Type:           messages.TypeCommandResponse,
		DispatchState:  store.MessageDispatchDispatched,
	}
	if err := s.store.CreateMessage(ctx, respMsg); err != nil {
		slog.Warn("Command run: failed to persist command-response message",
			"run_id", run.ID, "agent_id", asker.ID, "error", err)
		_, _ = s.store.ClearCommandRunOutcomeDelivery(ctx, run.ID, deliveredAt)
		return
	}
	_ = s.store.RecordCommandRunOutcomeMessage(ctx, run.ID, respMsg.ID)

	sm := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      deliveredAt.Format(time.RFC3339),
		Sender:         respMsg.Sender,
		SenderID:       respMsg.SenderID,
		Recipient:      respMsg.Recipient,
		RecipientID:    respMsg.RecipientID,
		Msg:            respText,
		Type:           messages.TypeCommandResponse,
		ThreadID:       req.ChatKey,
		ConversationID: req.ConversationID,
	}
	_, _ = s.dispatchActionResponse(ctx, actionResponse{
		Asker:      asker,
		Message:    respMsg,
		Text:       respText,
		Structured: sm,
		PopulateMeta: func(meta map[string]string) {
			meta["command_request_id"] = req.ID
			meta["command_request_key"] = req.Key
			meta["command_request_version"] = strconv.Itoa(req.Version)
			meta["command_run_id"] = run.ID
			meta["command_run_status"] = run.Status
			if run.ExitCode != nil {
				meta["command_run_exit_code"] = strconv.Itoa(*run.ExitCode)
			}
			if run.DurationMS != nil {
				meta["command_run_duration_ms"] = strconv.FormatInt(*run.DurationMS, 10)
			}
			if run.Reason != "" {
				meta["command_run_reason"] = run.Reason
			}
			if run.OutputStatus != "" {
				meta["command_run_output_status"] = run.OutputStatus
			}
		},
		LogPrefix: "command_run",
		Label:     "command-response",
	})

	if outputTail != "" {
		s.emitCommandRunAudit(ctx, req, run, msgaction.EventCmdOutputReturned, "", map[string]string{
			"output_bytes":    strconv.Itoa(len(outputTail)),
			"redaction_count": strconv.Itoa(redactionCount),
		})
	}
}

func formatCommandRunOutcomeMessage(req *store.CommandRequest, run *store.CommandRun, outputTail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Command request `%s` (v%d) finished with status **%s**", req.Key, req.Version, run.Status)
	if run.ExitCode != nil {
		fmt.Fprintf(&b, " (exit code %d", *run.ExitCode)
		if run.DurationMS != nil {
			fmt.Fprintf(&b, ", %dms", *run.DurationMS)
		}
		b.WriteByte(')')
	} else if run.DurationMS != nil {
		fmt.Fprintf(&b, " (%dms)", *run.DurationMS)
	}
	if run.Reason != "" {
		fmt.Fprintf(&b, ": %s", run.Reason)
	}
	b.WriteByte('.')
	if run.OutputStatus != "" {
		fmt.Fprintf(&b, " Output status: `%s`.", run.OutputStatus)
	}
	if outputTail != "" {
		fence := "```"
		for strings.Contains(outputTail, fence) {
			fence += "`"
		}
		b.WriteString("\n\nShared output tail:\n")
		b.WriteString(fence)
		b.WriteByte('\n')
		b.WriteString(outputTail)
		if !strings.HasSuffix(outputTail, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(fence)
	}
	return b.String()
}

// commandRunSweepHandler returns the periodic command-run lease-expiry sweeper
// handler (P1 subset of design §4.12: rows (a) and (b) only).
func (s *Server) commandRunSweepHandler() func(ctx context.Context) {
	return s.sweepCommandRuns
}

// sweepCommandRuns resolves expired pending command requests (O2, §4.12),
// expired unfinished command runs (design §4.12 rows (a) and (b)), and live
// unowned dispatching/running command runs (design §4.12 row (c)):
//   - pending command_requests past expires_at -> expired + audit event;
//   - (a) queued/waking past lease -> not_started/lease_expired (nothing sent,
//     guarded by ExpectedStatuses {queued, waking});
//   - (b) dispatching/running past lease -> poll exec-status once, then
//     unknown/lease_expired if still unresolved;
//   - (c) live unowned dispatching/running -> claimed via poll_claimed_at
//     (ClaimCommandRunsForPoll, excluding this replica's owned IDs) and polled
//     once.
//
// The sweeper never sends exec-start and never wakes a container.
func (s *Server) sweepCommandRuns(ctx context.Context) {
	batch := execSeamInt(&commandRunSweepBatch)
	budget := batch * execSeamInt(&commandRunSweepMaxPage)

	// Expire pending command_requests past their 24h TTL (O2, design §4.12).
	for expiredReqs := 0; expiredReqs < budget && ctx.Err() == nil; {
		rows, err := s.store.ExpireCommandRequests(ctx, time.Now().UTC(), batch)
		if err != nil {
			slog.Warn("Command run sweep: expire command requests failed", "error", err)
			break
		}
		if len(rows) == 0 {
			break
		}
		now := time.Now().UTC()
		for _, row := range rows {
			s.emitMessageActionEvent(ctx, &MessageActionEvent{
				Kind:      msgaction.KindCommandRequest,
				EventType: MessageActionEventExpired,
				RowID:     row.ID,
				MessageID: row.MessageID,
				ProjectID: row.ProjectID,
				AgentID:   row.AgentID,
				AgentName: row.AgentName,
				Key:       row.Key,
				Version:   row.Version,
				ActorKind: MessageActionActorHub,
				Details: map[string]string{
					"command_sha256":   row.CommandSHA256,
					"approver_user_id": row.ApproverUserID,
				},
				Timestamp: now,
			})
			s.publishActionUpdated(msgaction.KindCommandRequest, row.ProjectID, row.MessageID, row.ChatKey, commandRequestView(row, nil, nil))
		}
		expiredReqs += len(rows)
		if len(rows) < batch {
			break
		}
	}

	// Purge action_run exec_run_outputs older than 7 days (design §4.9, carry-forward I1).
	if ctx.Err() == nil {
		cutoff := time.Now().UTC().Add(-execSeamDuration(&commandRunOutputRetention))
		if purged, err := s.store.PurgeOldActionRunOutputs(ctx, cutoff); err != nil {
			slog.Warn("Command run sweep: purge old action_run outputs failed", "error", err)
		} else if purged > 0 {
			slog.Info("Command run sweep: purged old action_run outputs", "purged", purged)
		}
	}

	// (a) & (b): expired unfinished runs past their lease.
	for processed := 0; processed < budget && ctx.Err() == nil; {
		runs, err := s.store.ListExpiredCommandRuns(ctx, time.Now().UTC(), batch)
		if err != nil {
			slog.Warn("Command run sweep: list expired failed", "error", err)
			break
		}
		if len(runs) == 0 {
			break
		}
		progressed := false
		for _, run := range runs {
			if ctx.Err() != nil {
				return
			}
			var req *store.CommandRequest
			if run.RequestID != "" {
				if loaded, lerr := s.store.GetCommandRequest(ctx, run.RequestID); lerr == nil {
					req = loaded
				}
			}
			switch run.Status {
			case store.CommandRunStatusQueued, store.CommandRunStatusWaking:
				recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
				won, ferr := s.finishCommandRun(recordCtx, req, run, []string{store.CommandRunStatusQueued, store.CommandRunStatusWaking}, store.CommandRunStatusNotStarted, "lease_expired", nil, "")
				cancel()
				if ferr == nil && won {
					progressed = true
				}
			case store.CommandRunStatusDispatching, store.CommandRunStatusRunning:
				if s.pollCommandRunOnce(ctx, req, run, true) {
					progressed = true
				}
			}
		}
		processed += len(runs)
		if len(runs) < batch || !progressed {
			break
		}
	}

	// (c): live unowned dispatching/running runs claimed via poll_claimed_at.
	inFlight := execSeamInt(&commandRunSweepInFlight)
	if inFlight <= 0 || budget <= 0 || ctx.Err() != nil {
		return
	}
	sem := make(chan struct{}, inFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	var claimedThisSweep []string
	claimTTL := execSeamDuration(&commandRunPollClaimTTL)
	for polled := 0; polled < budget && ctx.Err() == nil; {
		reserved := reserveSweepSlots(ctx, sem, min(inFlight, budget-polled))
		if reserved == 0 {
			return
		}
		owned := s.commandRuns.ownedIDs()
		exclude := make([]string, 0, len(owned)+len(claimedThisSweep))
		exclude = append(exclude, owned...)
		exclude = append(exclude, claimedThisSweep...)

		claimed, err := s.store.ClaimCommandRunsForPoll(ctx, time.Now().UTC(), claimTTL, reserved, exclude)
		if err != nil {
			for i := 0; i < reserved; i++ {
				<-sem
			}
			slog.Warn("Command run sweep: claim live runs failed", "error", err)
			return
		}
		for i := len(claimed); i < reserved; i++ {
			<-sem
		}
		if len(claimed) == 0 {
			return
		}
		for _, run := range claimed {
			claimedThisSweep = append(claimedThisSweep, run.ID)
			wg.Add(1)
			go func(r *store.CommandRun) {
				defer wg.Done()
				defer func() { <-sem }()
				var req *store.CommandRequest
				if r.RequestID != "" {
					if loaded, lerr := s.store.GetCommandRequest(ctx, r.RequestID); lerr == nil {
						req = loaded
					}
				}
				s.pollCommandRunOnce(ctx, req, r, false)
			}(run)
		}
		polled += len(claimed)
		if len(claimed) < reserved {
			return
		}
	}
}
