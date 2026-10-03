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
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func (s *Server) updateAgentStatus(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)

	// Every identity kind is handled explicitly; there is no fall-through.
	// Agents may update only their own status and need the status scope.
	// Any other caller must be authorized to update the agent itself.
	switch ident := identity.(type) {
	case nil:
		Unauthorized(w)
		return
	case AgentIdentity:
		if ident.ID() != id {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only update their own status", nil)
			return
		}
		if !ident.HasScope(ScopeAgentStatusUpdate) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: agent:status:update", nil)
			return
		}
	default:
		agent, err := s.store.GetAgent(ctx, id)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		// SECURITY-GATE: CheckAccess — non-agent callers need update access
		// to this specific agent.
		if !s.authorize(w, r, agentResource(agent), ActionUpdate) {
			return
		}
	}

	var status store.AgentStatusUpdate
	if err := readJSON(r, &status); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Validate ExitReason before passing to the store: only terminal
	// activities are valid exit reasons. Silently drop invalid values
	// rather than rejecting the entire status update.
	if status.ExitReason != "" && !isValidExitReason(status.ExitReason) {
		status.ExitReason = "" // silently drop invalid values
	}

	// Observability only: sciontool init reports elapsed-since-process-start
	// via Metadata["startup_ms"] on its first running status report. Log the
	// parsed value only — never the rest of the Metadata map — so start-time
	// attribution does not depend on persisting a new column.
	if raw, ok := status.Metadata["startup_ms"]; ok {
		if ms, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			s.agentLifecycleLog.Info("agent reported startup timing", "agent_id", id, "startup_ms", ms)
		}
	}

	// Guard against phase regressions and auto-correct phase from activity.
	if status.Phase != "" || status.Activity != "" {
		agent, err := s.store.GetAgent(ctx, id)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Observability only: start-time attribution from agent.Created to the
		// first "running"/"working" status report, logged at whichever of two
		// known sources actually reaches here first. A no-auth/drop-to-shell
		// agent never runs a harness session, so it never emits SessionStart
		// and only ever reaches the first case below (see ptone/scion#2519):
		//
		//  - "Agent started": sciontool init's own report, right after the
		//    supervised child process starts (same request that carries
		//    Metadata["startup_ms"] above). Fires for every agent, including
		//    no-auth/drop-to-shell ones. This is the dispatch-to-init-ready
		//    number.
		//  - "Session started": the harness's SessionStart hook (see
		//    ReportState's one call site for EventSessionStart in
		//    pkg/sciontool/hooks/handlers/hub.go). Only fires once a real
		//    harness session starts, i.e. when credentials are configured.
		//    This is the dispatch-to-harness-ready number.
		//
		// Logging every time a matching report arrives, rather than tracking
		// a persisted "first report" flag — testers take the earliest line
		// per agent and source as the number.
		if status.Phase == string(state.PhaseRunning) && status.Activity == string(state.ActivityWorking) && !agent.Created.IsZero() {
			switch status.Message {
			case "Agent started":
				s.agentLifecycleLog.Info("dispatch ready: Agent started status received",
					"agent_id", id, "since_create_ms", time.Since(agent.Created).Milliseconds())
			case "Session started":
				s.agentLifecycleLog.Info("harness ready: SessionStart status received",
					"agent_id", id, "since_create_ms", time.Since(agent.Created).Milliseconds())
			}
		}

		oldPhase := agent.Phase
		guardAgentPhaseTransition(agent, &status)
		// Reconcile the max_agents_per_broker reservation against the phase
		// this self-reported status update will actually persist (post-guard,
		// since the guard may clear status.Phase on a regression or while
		// suspended) — ptone/scion#1963.
		s.reconcileBrokerQuotaOnPhaseChange(ctx, agent, oldPhase, status.Phase)
	}

	if err := s.store.UpdateAgentStatus(ctx, id, status); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Publish status event (best-effort: fetch agent for ProjectID). A
	// soft-deleted row publishes nothing: its deletion was already announced,
	// and a late report from its container must not resurrect it in clients.
	if agent, err := s.store.GetAgent(ctx, id); err == nil {
		if agent.DeletedAt.IsZero() {
			s.events.PublishAgentStatus(ctx, agent)
		}
	} else {
		s.agentLifecycleLog.Warn("Failed to fetch agent for status event", "agent_id", id, "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

// guardAgentPhaseTransition applies two guards to a status update:
//
//  1. Phase regression guard: rejects transitions that would move an agent
//     backward in its forward-progress lifecycle (e.g. running → starting).
//  2. Activity-driven phase auto-correction: when an activity that implies the
//     agent is running arrives but the phase is pre-running, auto-promotes the
//     phase to running.
//
// It also blocks a status update entirely while a `scion reincarnate`
// migration owns the agent (Guard 0b, design §3.4 Amendment A11 item 2) —
// see that guard's comment.
func guardAgentPhaseTransition(agent *store.Agent, status *store.AgentStatusUpdate) {
	currentPhase := state.Phase(agent.Phase)

	// Guard 0c: a delete in progress (design ptone/scion#2483 §2.1) owns the
	// agent's Phase/Activity/ExitCode/ExitReason/Message, exactly like a
	// reincarnation (Guard 0b): a dying container's report must not revive a
	// row the delete engine is tearing down. A soft-deleted row is done — no
	// report changes it. The store repeats this check inside the
	// UpdateAgentStatus transaction, so a delete claim that lands between
	// this read and that write is still honored.
	if deletionActive(agent) || !agent.DeletedAt.IsZero() {
		status.Phase = ""
		status.Activity = ""
		status.ExitCode = nil
		status.ExitReason = ""
		status.Message = ""
		// The store copy of this guard also drops ClearExit; mirror it so the
		// two stay the same predicate (reports never set it: json:"-").
		status.ClearExit = false
		return
	}

	// Guard 0: suspended is sticky against async status updates. When an agent
	// is suspended, its container is being torn down, and the dying container's
	// async sciontool /status POST (e.g. phase=stopped, activity=crashed) must
	// not clobber the suspended phase — otherwise a subsequent /start would not
	// see suspended and would skip the harness --continue (resume) flag.
	// Only explicit start/stop lifecycle actions may leave the suspended phase,
	// and those write phase directly without going through this guard.
	if currentPhase == state.PhaseSuspended {
		status.Phase = ""
		status.Activity = ""
		return
	}

	// Guard 0b: a `scion reincarnate` migration in flight is sticky the same
	// way — the reincarnation worker owns Phase/Activity/ExitCode/ExitReason/
	// Message for the agent until it completes or fails, so an async
	// sciontool /status POST from the OLD container racing the migration
	// (e.g. a crash report from the generation the worker is in the middle of
	// tearing down and replacing) must not surface as the agent's live status.
	// ContainerStatus and the Heartbeat/LastSeen bump are not status's
	// concern here (this endpoint does not set them), so nothing further
	// needs blanking.
	if reincarnationInFlight(agent) {
		status.Phase = ""
		status.Activity = ""
		status.ExitCode = nil
		status.ExitReason = ""
		status.Message = ""
		return
	}

	// Guard 1: reject phase regressions within the forward-progress lifecycle.
	if status.Phase != "" {
		newPhase := state.Phase(status.Phase)
		if currentPhase.IsActivePhase() && newPhase.IsActivePhase() &&
			newPhase.Ordinal() < currentPhase.Ordinal() {
			status.Phase = ""
		}
	}

	// Guard 2: if an activity that implies the agent is running arrives
	// without an explicit phase, and the current phase is pre-running,
	// auto-correct the phase to running.
	if status.Activity != "" && status.Phase == "" {
		activity := state.Activity(status.Activity)
		if activity.ImpliesRunning() && currentPhase.IsActivePhase() &&
			currentPhase != state.PhaseRunning {
			status.Phase = string(state.PhaseRunning)
		}
	}
}

// errHarnessNoResume is returned by suspendAgent when the agent's harness does
// not support session resume, so suspending would strand it. The wrapped reason
// carries harness-supplied context for the caller's error message.
type errHarnessNoResume struct {
	reason string
}

func (e *errHarnessNoResume) Error() string {
	if e.reason != "" {
		return e.reason
	}
	return "harness does not support session resume"
}

// harnessSupportsResume reports whether the agent's configured harness supports
// resuming a session. An empty harness name (no applied config) is treated as
// supported, matching the HTTP suspend handler's prior behavior of only
// rejecting when a harness was explicitly resolved and declared SupportNo.
func (s *Server) harnessSupportsResume(agent *store.Agent) (bool, string) {
	harnessName := ""
	if agent.AppliedConfig != nil {
		harnessName = agent.AppliedConfig.HarnessConfig
	}
	if harnessName == "" {
		return true, ""
	}
	caps := harness.New(harnessName).AdvancedCapabilities()
	if caps.Resume.Support == api.SupportNo {
		return false, caps.Resume.Reason
	}
	return true, ""
}

// suspendAgent performs the core SUSPEND action shared by the HTTP lifecycle
// handler and the auto-suspend scheduler: it validates harness resume support,
// syncs the workspace on stop, dispatches the container stop to the runtime
// broker, persists phase=suspended (container_status=stopped, activity cleared),
// and publishes the resulting status event. It returns *errHarnessNoResume when
// the harness cannot resume so callers can decline to suspend.
func (s *Server) suspendAgent(ctx context.Context, agent *store.Agent) error {
	if ok, reason := s.harnessSupportsResume(agent); !ok {
		return &errHarnessNoResume{reason: reason}
	}

	// The container is stopped before phase=suspended is written; see
	// beginLifecycleOp.
	defer s.beginLifecycleOp(agent.ID)()

	dispatcher := s.GetDispatcher()
	if dispatcher != nil && agent.RuntimeBrokerID != "" {
		s.syncWorkspaceOnStop(ctx, agent)
		if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
			return err
		}
	}

	// Revoke all credentials for the suspended agent (best-effort, Phase 1H)
	if _, err := s.store.RevokeAgentCredentialsByAgent(ctx, agent.ID, "system", "agent_suspended"); err != nil {
		slog.Warn("Failed to revoke agent credentials on suspend", "agent_id", agent.ID, "error", err)
	}

	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: "agent_credential_revoke",
		TargetType:   "agent_credential",
		TargetID:     agent.ID,
	})

	newPhase := string(state.PhaseSuspended)
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:           newPhase,
		ContainerStatus: "stopped",
		Activity:        "",
	}); err != nil {
		return err
	}

	agent.Phase = newPhase
	agent.ContainerStatus = "stopped"
	agent.Activity = ""
	// A suspended agent has no running container: release its
	// max_agents_per_broker reservation so it stops consuming broker
	// capacity until it is resumed (ptone/scion#1963).
	s.releaseBrokerQuota(ctx, agent)
	s.events.PublishAgentStatus(ctx, agent)
	return nil
}

// AgentLifecycleStartRequest is the optional JSON body for the "start"
// lifecycle action. It is empty for a normal start; ForceResume is only
// meaningful when the agent is in phase=error, where it requests a
// best-effort resume of the interrupted harness session instead of a fresh
// one. See resumeInPlaceDecision for the equivalent semantics on the
// create-agent resume path.
type AgentLifecycleStartRequest struct {
	ForceResume bool `json:"forceResume,omitempty"`
}

func (s *Server) handleAgentLifecycle(w http.ResponseWriter, r *http.Request, id, action string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Start gate (design ptone/scion#2483 §2.1). Authz already ran in the
	// caller. This runs before the managed-runtime branch so managed agents
	// get the same answers. Restart is checked here, before its stop leg,
	// so a launching agent is not stopped; the dispatcher guard is the
	// backstop for the start leg. An in-flight launch is answered with 200
	// and the current agent.
	switch action {
	case api.AgentActionStart, api.AgentActionRestart:
		entry := startEntryStart
		if action == api.AgentActionRestart {
			entry = startEntryRestart
		}
		if ref := s.startGate(ctx, agent, entry); ref.refuses() {
			if !ref.InFlight {
				ref.write(w)
				return
			}
			var warnings []string
			if action == api.AgentActionRestart {
				warnings = []string{launchRestartNotPerformedWarning}
			} else if lifecycleStartHasInputs(r) {
				warnings = []string{launchInFlightInputsWarning}
			}
			s.writeLaunchingAgent(ctx, w, agent, warnings)
			return
		}
	case api.AgentActionStop, api.AgentActionSuspend:
		// The agent is already going down: stop (and suspend, which would
		// otherwise race the delete's teardown on the broker) is a no-op
		// success.
		if deleteStopNoop(agent) {
			respAgent := *agent
			respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
			respAgent.Deletion = store.ComputeAgentDeletion(agent, time.Now())
			writeJSON(w, http.StatusOK, &respAgent)
			return
		}
	}

	// Managed agent lifecycle: handle directly without broker dispatch.
	if isManagedAgentRuntime(agent.Runtime) {
		s.handleManagedAgentLifecycle(w, r, agent, action)
		return
	}

	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// While this lifecycle action runs, the agent's container may be
	// legitimately absent with the row still in phase running (for example
	// between the stop and the start of a restart); keep the heartbeat
	// missing-container reconcile away from it until the final status write.
	defer s.beginLifecycleOp(agent.ID)()

	var newPhase string
	var dispatchErr error

	// Collect warnings the start leg raises (hub-side TZ drops and the
	// broker's hub-only env warnings) so the start and restart responses
	// carry them.
	ctx, dispatchWarns := withDispatchWarnings(ctx)

	// If a dispatcher is available, dispatch the operation to the runtime broker
	dispatcher := s.GetDispatcher()

	switch action {
	case api.AgentActionStart:
		newPhase = string(state.PhaseRunning)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Resume the harness session when the agent was suspended. An
			// error-phase agent can also request a best-effort resume of its
			// interrupted session by sending forceResume in the request body
			// (mirrors resumeInPlaceDecision's forcedRecovery case for the
			// create-agent resume path). This never applies to any other
			// phase: a running agent must not be recreated out from under
			// itself, and a stopped agent restarts fresh even if asked to
			// resume, matching the create-agent path's behavior.
			var startReq AgentLifecycleStartRequest
			if r.ContentLength > 0 {
				if err := readJSON(r, &startReq); err != nil {
					BadRequest(w, "invalid request body: "+err.Error())
					return
				}
			}
			forcedRecovery := agent.Phase == string(state.PhaseError) && startReq.ForceResume
			if forcedRecovery {
				s.agentLifecycleLog.Warn("Force-resuming agent from error phase via lifecycle start",
					"agent_id", agent.ID, "agent", agent.Name, "container_status", agent.ContainerStatus)
			}
			resume := agent.Phase == string(state.PhaseSuspended) || forcedRecovery
			// Re-reserve the per-broker ceiling before dispatch, exactly as
			// create does, so a start that would exceed the cap is rejected
			// up front rather than after the container is already running
			// (ptone/scion#1963). Idempotent: a no-op when the agent already
			// holds an active reservation (e.g. start called again on an
			// already-running agent).
			ok, reserved := s.checkAndReserveBrokerQuotaHTTP(ctx, w, agent)
			if !ok {
				return
			}
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent, "", resume)
			// DispatchAgentStart applies the broker response in-place;
			// use the broker-reported phase if it was set.
			if dispatchErr == nil && agent.Phase != "" {
				newPhase = agent.Phase
			}
			if dispatchErr != nil {
				// Roll back a reservation this call took speculatively, so a
				// failed start doesn't strand one with no container behind
				// it. A reservation that already existed (start on a
				// running agent) is kept: that agent is still counted
				// (ptone/scion#1978).
				s.rollbackBrokerQuota(ctx, agent, reserved)
			}
		}
	case api.AgentActionStop:
		newPhase = string(state.PhaseStopped)
		// Clear exposed ports — the agent is stopping so its ports are unreachable.
		s.clearExposedPortsForAgent(ctx, agent.ID)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Before stopping, sync workspace back for hub-managed projects on remote brokers.
			// This is best-effort: failures are logged but don't block the stop.
			s.syncWorkspaceOnStop(ctx, agent)
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
		}
		if dispatchErr == nil {
			// A stopped agent has no running container: release its
			// max_agents_per_broker reservation (ptone/scion#1963).
			s.releaseBrokerQuota(ctx, agent)
		}
	case api.AgentActionSuspend:
		// Only running agents can be suspended via the HTTP lifecycle handler.
		// (The auto-suspend scheduler calls suspendAgent directly and already
		// restricts itself to running+stalled agents.)
		if agent.Phase != string(state.PhaseRunning) {
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				fmt.Sprintf("Cannot suspend agent in phase %q. Only running agents can be suspended.", agent.Phase), nil)
			return
		}
		// Suspend is fully handled by the shared suspendAgent helper, which
		// validates harness resume support, dispatches the stop, persists
		// phase=suspended, and publishes the status event.
		if err := s.suspendAgent(ctx, agent); err != nil {
			var noResume *errHarnessNoResume
			if errors.As(err, &noResume) {
				writeError(w, http.StatusBadRequest, ErrCodeValidationError,
					fmt.Sprintf("Cannot suspend agent: %s. Use 'stop' instead.", noResume.Error()), nil)
				return
			}
			RuntimeError(w, "Failed to dispatch to runtime broker: "+err.Error())
			return
		}
		respAgent := *agent
		respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
		writeJSON(w, http.StatusOK, respAgent)
		return
	case api.AgentActionRestart:
		newPhase = string(state.PhaseRunning)
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Refuse before the stop leg: otherwise a broker without
			// the empty-per-agent capability would have the agent
			// stopped and then the start refused (design #2703 D3).
			if !s.requireEmptyPerAgentBrokerCapabilityForAgent(ctx, w, agent) {
				return
			}
			// Restart is implemented as stop + start so that env vars
			// (API keys, secrets) are re-resolved from Hub storage.
			// Stop errors are tolerated: the container may already be
			// exited and some runtimes (podman) return non-standard
			// errors for stopping non-running containers. The subsequent
			// Start will handle cleanup of the exited container.
			stopErr := dispatcher.DispatchAgentStop(ctx, agent)
			if stopErr != nil {
				slog.Warn("Restart: stop dispatch failed, proceeding with start",
					"agent_id", id, "error", stopErr)
			}
			// The broker reservation is held across the restart
			// (ptone/scion#1978). Releasing it after the stop leg and
			// re-reserving before the start leg would let another start
			// take the slot in between. This reserve is a no-op for an
			// agent that already holds one, and applies the cap to an
			// agent that does not (for example, a stopped agent).
			ok, reserved := s.checkAndReserveBrokerQuotaHTTP(ctx, w, agent)
			if !ok {
				return
			}
			// Restart is stop + start: a fresh harness session, not a resume.
			dispatchErr = dispatcher.DispatchAgentStart(ctx, agent, "", false)
			// DispatchAgentStart applies the broker response in-place;
			// use the broker-reported phase if it was set.
			if dispatchErr == nil && agent.Phase != "" {
				newPhase = agent.Phase
			}
			if dispatchErr != nil {
				if stopErr == nil {
					// The stop leg succeeded, so the container is down:
					// release the slot as an explicit stop would.
					s.releaseBrokerQuota(ctx, agent)
				} else {
					// The container may still be running: keep a
					// reservation this call did not create.
					s.rollbackBrokerQuota(ctx, agent, reserved)
				}
			}
		}
	}

	// If dispatch failed, return error
	if dispatchErr != nil {
		// A launch that began after the entry check is caught by the
		// dispatcher guard; answer as the entry check does.
		if refusal := s.launchRefusalFromError(ctx, agent.ID, dispatchErr); refusal != nil {
			if !refusal.InFlight {
				refusal.write(w)
				return
			}
			var warnings []string
			if action == api.AgentActionRestart {
				warnings = []string{launchRestartNotPerformedWarning}
			}
			s.writeLaunchingAgent(ctx, w, agent, warnings)
			return
		}
		if writeAgentTokenIssueError(w, dispatchErr) {
			return
		}
		if writeEmptyPerAgentCapabilityError(w, dispatchErr) {
			return
		}
		RuntimeError(w, "Failed to dispatch to runtime broker: "+dispatchErr.Error())
		return
	}

	statusUpdate := store.AgentStatusUpdate{
		Phase: newPhase,
	}
	// When stopping, also update container status so the hub immediately
	// reflects the stopped state without waiting for the next heartbeat.
	// (Suspend is handled earlier via suspendAgent and returns before here.)
	if action == api.AgentActionStop {
		statusUpdate.ContainerStatus = "stopped"
		statusUpdate.Activity = ""
		zero := 0
		statusUpdate.ExitCode = &zero
	}
	// When starting or restarting, propagate container status from broker
	// response, and clear any exit reason/code from the prior generation —
	// including a disruption reason recorded while the agent was still
	// running (state.ExitReasonPreempted/ExitReasonEvicted), which the
	// phase-transition clear in UpdateAgentStatus does not catch when the
	// agent was already running (not stopped/error) at dispatch time.
	if action == api.AgentActionStart || action == api.AgentActionRestart {
		if agent.ContainerStatus != "" {
			statusUpdate.ContainerStatus = agent.ContainerStatus
		}
		statusUpdate.ClearExit = true
	}
	if err := s.store.UpdateAgentStatus(ctx, id, statusUpdate); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// A successful start/stop/restart clears a failed delete marker
	// (design ptone/scion#2483 §2.1); publish and respond from the stored
	// row, which a racing delete claim may have kept off newPhase.
	s.settleLifecycleWrite(ctx, agent, newPhase)
	s.events.PublishAgentStatus(ctx, agent)

	respAgent := *agent
	respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
	respAgent.Deletion = store.ComputeAgentDeletion(agent, time.Now())
	writeJSON(w, http.StatusOK, agentLifecycleResponse{Agent: &respAgent, Warnings: dispatchWarns.Warnings()})
}

// agentLifecycleResponse is the lifecycle action response: the agent, plus
// any warnings the dispatch raised. Warnings is omitted when empty, so the
// body is unchanged for clients that only read the agent.
type agentLifecycleResponse struct {
	*store.Agent
	Warnings []string `json:"warnings,omitempty"`
}

// stopAllResult represents the outcome of stopping a single agent.
type stopAllResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// StopAllAgentsResponse is the response from the stop-all endpoint.
type StopAllAgentsResponse struct {
	Stopped int             `json:"stopped"`
	Failed  int             `json:"failed"`
	Total   int             `json:"total"`
	Scope   string          `json:"scope,omitempty"` // "all" or "own"
	Results []stopAllResult `json:"results"`
}

// handleStopAllAgents stops all running agents, optionally scoped to a project.
// Global (projectID=="") requires agent.stop_all on the hub. Project-scoped
// allows any project member: holders of agent.stop_all on the project
// (project owners/admins, hub admins) stop all agents; other members, by
// active direct or group-derived role binding, stop only their own.
func (s *Server) handleStopAllAgents(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()

	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"Authentication required", nil)
		return
	}

	// Determine authorization and scope
	scope := "all"
	filter := store.AgentFilter{
		ProjectID: projectID,
		Phase:     string(state.PhaseRunning),
	}

	// agent.stop_all is decided against the hub for global stop-all and
	// against the project-scoped agent collection for a project, so
	// project-owner and project-admin role bindings count. This is the
	// same resource the project's stop_all scope capability is computed on.
	resource := Resource{Type: "agent", ID: "hub"}
	if projectID != "" {
		resource = Resource{Type: "agent", ParentType: "project", ParentID: projectID}
	}
	canStopAll := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(userIdent),
		Credential: credentialContextForIdentity(userIdent),
		Resource:   resource,
		Action:     ActionStopAll,
		Permission: "agent.stop_all",
	}).Allowed
	if !canStopAll {
		if projectID == "" {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Only admins can stop all agents", nil)
			return
		}
		// Other project members stop only their own agents. Membership is the
		// effective project role: direct or group-derived role bindings that
		// are currently active. Only the built-in roles count: a custom
		// project role ranks 0 in higherProjectRole, the same as no role, so
		// a caller holding only a custom role gets 403, not scope "own".
		// A store failure is a 500 rather than a misleading 403. That
		// includes a binding whose role definition is missing: the store
		// refuses to delete a role definition that still has bindings, so
		// that is a data integrity fault, and failing closed is correct.
		// A nil membership service is a wiring fault, also a 500, matching
		// the other membership handlers.
		if s.membershipService == nil {
			s.agentLifecycleLog.Error("stop-all: membership service unavailable",
				"project_id", projectID, "user_id", userIdent.ID())
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"membership service unavailable", nil)
			return
		}
		role, err := s.membershipService.projectEffectiveRoleFromStore(ctx, s.store, userIdent.ID(), projectID)
		if err != nil {
			s.agentLifecycleLog.Error("stop-all: failed to resolve project membership",
				"project_id", projectID, "user_id", userIdent.ID(), "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to resolve project membership", nil)
			return
		}
		if role == "" {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"You are not a member of this project", nil)
			return
		}
		filter.OwnerID = userIdent.ID()
		scope = "own"
	}

	result, err := s.store.ListAgents(ctx, filter, store.ListOptions{
		Limit: 1000, // reasonable upper bound
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agents := result.Items
	if scope == "own" && len(agents) > 0 {
		// Members stop the agents the per-agent lifecycle rule allows.
		identity := GetIdentityFromContext(ctx)
		allowed := make([]store.Agent, 0, len(agents))
		for i := range agents {
			if s.agentLifecycleAllowed(ctx, identity, &agents[i]) {
				allowed = append(allowed, agents[i])
			}
		}
		if len(allowed) == 0 {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Not authorized to stop these agents", nil)
			return
		}
		agents = allowed
	}

	// Skip agents a delete is taking down: their stop would race the
	// delete's teardown on the broker, and a stop is a no-op for them
	// anyway (design ptone/scion#2483 §2.1).
	live := make([]store.Agent, 0, len(agents))
	for i := range agents {
		if !deleteStopNoop(&agents[i]) {
			live = append(live, agents[i])
		}
	}
	agents = live
	if len(agents) == 0 {
		writeJSON(w, http.StatusOK, StopAllAgentsResponse{
			Scope:   scope,
			Results: []stopAllResult{},
		})
		return
	}

	dispatcher := s.GetDispatcher()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make([]stopAllResult, 0, len(agents))
	)

	for i := range agents {
		agent := &agents[i]
		wg.Add(1)
		go func(agent *store.Agent) {
			defer wg.Done()

			// The filter above used the list snapshot; re-read so a delete
			// that claimed the row since is skipped the same way (a stop is
			// a no-op for it). A delete claiming after this read is still
			// harmless: the broker stop is idempotent, and the delete's own
			// teardown follows.
			if fresh, err := s.store.GetAgent(ctx, agent.ID); err == nil {
				if deleteStopNoop(fresh) || !fresh.DeletedAt.IsZero() {
					return
				}
				agent = fresh
			}

			res := stopAllResult{
				ID:   agent.ID,
				Name: agent.Name,
			}

			// Dispatch stop to broker
			var dispatchErr error
			if dispatcher != nil && agent.RuntimeBrokerID != "" {
				opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				s.syncWorkspaceOnStop(opCtx, agent)
				dispatchErr = dispatcher.DispatchAgentStop(opCtx, agent)
			}

			if dispatchErr != nil {
				res.Status = "error"
				res.Error = dispatchErr.Error()
				s.agentLifecycleLog.Warn("stop-all: failed to stop agent",
					"agent_id", agent.ID, "error", dispatchErr)
			} else {
				// Update agent status in store
				zero := 0
				statusUpdate := store.AgentStatusUpdate{
					Phase:           string(state.PhaseStopped),
					ContainerStatus: "stopped",
					Activity:        "",
					ExitCode:        &zero,
				}
				if updateErr := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); updateErr != nil {
					res.Status = "error"
					res.Error = updateErr.Error()
				} else {
					res.Status = "stopped"
					// Clear a failed delete marker, as a single stop does,
					// and publish the row as stored.
					s.settleLifecycleWrite(ctx, agent, string(state.PhaseStopped))
					// Release the per-broker reservation, same as a single
					// explicit stop (ptone/scion#1963).
					s.releaseBrokerQuota(ctx, agent)
					// settleLifecycleWrite reloaded DeletedAt: a delete that
					// soft-finished meanwhile gets no stale stopped status.
					if agent.DeletedAt.IsZero() {
						s.events.PublishAgentStatus(ctx, agent)
					}
				}
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(agent)
	}

	wg.Wait()

	stopped := 0
	failed := 0
	for _, r := range results {
		if r.Status == "stopped" {
			stopped++
		} else {
			failed++
		}
	}

	writeJSON(w, http.StatusOK, StopAllAgentsResponse{
		Stopped: stopped,
		Failed:  failed,
		Total:   len(results),
		Scope:   scope,
		Results: results,
	})
}
