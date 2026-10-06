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
	"github.com/google/uuid"
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
	// The guards run whenever the update touches a field they own —
	// Phase/Activity/Message/ExitCode/ExitReason — so a message-only or
	// exit-only POST is subject to the same reincarnation-sticky Guard 0b as
	// a phase report (ptone/scion#2267). Fields the guards never touch
	// (heartbeat, tool name, task summary, limits, ...) skip the extra read.
	if statusUpdateTouchesGuardedFields(status) {
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
		// Guarded no-op: when a guard that owns the agent's status (Guard 0c,
		// a delete in progress or a soft-deleted row; Guard 0b, an in-flight
		// `scion reincarnate` migration) has blanked the report and nothing
		// is left to persist, skip the store write and tell the caller the
		// report was not applied, so a dropped report is distinguishable from
		// an accepted one. 200 (not an error) keeps `sciontool status`
		// succeeding; it ignores the body today.
		//
		// Partial apply: if the report also carries fields those guards do
		// not own (heartbeat, toolName, taskSummary, limits, metadata, ...),
		// those are still written and the response is {"applied":true} even
		// though the guarded fields were dropped.
		//
		// Best effort: {"applied":true} means "not guarded when the handler
		// read the agent". A delete or reincarnation claimed between that
		// read and the write can make it inaccurate. The persisted state is
		// still correct for a delete (the store repeats Guard 0c inside the
		// UpdateAgentStatus transaction); Guard 0b has no store-side twin.
		if reason := statusGuardNoopReason(agent); reason != "" && statusUpdateIsEmpty(status) {
			writeJSON(w, http.StatusOK, statusUpdateResult{Applied: false, Reason: reason})
			return
		}
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

	writeJSON(w, http.StatusOK, statusUpdateResult{Applied: true})
}

// statusUpdateResult.Reason values for a status report dropped because a
// guard owns the agent's status.
const (
	// statusUpdateReasonDeleteInProgress: a delete holds the agent, or the
	// row is soft-deleted (Guard 0c).
	statusUpdateReasonDeleteInProgress = "delete_in_progress"
	// statusUpdateReasonReincarnationInFlight: a reincarnation owns the
	// agent (Guard 0b).
	statusUpdateReasonReincarnationInFlight = "reincarnation_in_flight"
)

// statusGuardNoopReason returns the statusUpdateResult.Reason for the guard
// that blanks every guarded field (Phase/Activity/Message/ExitCode/
// ExitReason) of agent's status reports, or "" when none does. It mirrors
// guardAgentPhaseTransition's order: Guard 0c (delete) takes precedence
// over Guard 0b (reincarnation), and both run before Guard 0 (suspended).
// Guard 0 blanks only Phase/Activity, so it never yields a no-op on its
// own and has no reason here.
func statusGuardNoopReason(agent *store.Agent) string {
	switch {
	case deletionActive(agent) || !agent.DeletedAt.IsZero():
		return statusUpdateReasonDeleteInProgress
	case reincarnationInFlight(agent):
		return statusUpdateReasonReincarnationInFlight
	}
	return ""
}

// statusUpdateResult is the response body of POST /agents/{id}/status.
// Applied is false when the hub accepted the request but persisted nothing
// (see Reason).
type statusUpdateResult struct {
	Applied bool   `json:"applied"`
	Reason  string `json:"reason,omitempty"`
}

// statusUpdateTouchesGuardedFields reports whether the update sets any field
// guardAgentPhaseTransition may blank or rewrite. The internal ClearExit
// and ClearMessageIf (json:"-") are never set on a decoded status POST, so
// they do not trigger the guards here; statusUpdateIsEmpty still counts
// them.
func statusUpdateTouchesGuardedFields(su store.AgentStatusUpdate) bool {
	return su.Phase != "" || su.Activity != "" || su.Message != "" ||
		su.ExitCode != nil || su.ExitReason != ""
}

// statusUpdateIsEmpty reports whether the update carries nothing for the
// store to persist (beyond the Updated/LastSeen bump every write does).
// Every field counts, including the internal json:"-" ones a decoded status
// POST never sets (ClearExit, ClearMessageIf, ClearTerminalRemnants,
// IfPhase): erring towards "not empty" only means the store write runs.
// TestStatusUpdateIsEmpty_EveryFieldCounts catches a field missing here.
func statusUpdateIsEmpty(su store.AgentStatusUpdate) bool {
	return !statusUpdateTouchesGuardedFields(su) &&
		su.ToolName == "" && su.ConnectionState == "" && su.ContainerStatus == "" &&
		su.RuntimeState == "" && su.TaskSummary == "" && !su.Heartbeat &&
		len(su.Metadata) == 0 && su.CurrentTurns == nil && su.CurrentModelCalls == nil &&
		su.StartedAt == "" && !su.ClearExit && su.ClearMessageIf == "" &&
		!su.ClearTerminalRemnants && su.IfPhase == ""
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

	// Guard 0b: a `scion reincarnate` migration in flight is sticky like a
	// delete — the reincarnation worker owns Phase/Activity/ExitCode/ExitReason/
	// Message for the agent until it completes or fails, so an async
	// sciontool /status POST from the OLD container racing the migration
	// (e.g. a crash report from the generation the worker is in the middle of
	// tearing down and replacing) must not surface as the agent's live status.
	// ContainerStatus, heartbeat and the other non-status fields are not
	// owned by the worker (reincarnationStepUpdate never writes them), so
	// they are left as reported (partial apply; see updateAgentStatus). It
	// runs before Guard 0 (suspended) because it blanks a
	// superset of Guard 0's fields: a suspended agent with a reincarnation
	// pending (the worker has not yet written its first step) must have a
	// message- or exit-only report dropped too (ptone/scion#2267).
	if reincarnationInFlight(agent) {
		status.Phase = ""
		status.Activity = ""
		status.ExitCode = nil
		status.ExitReason = ""
		status.Message = ""
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
// The run intent is set to stopped before the dispatch.
func (s *Server) suspendAgent(ctx context.Context, agent *store.Agent) error {
	if ok, reason := s.harnessSupportsResume(agent); !ok {
		return &errHarnessNoResume{reason: reason}
	}

	// The container is stopped before phase=suspended is written; see
	// beginLifecycleOp.
	defer s.beginLifecycleOp(agent.ID)()

	supersedes := agent.StartClaimID
	intentAt, err := s.recordRunIntent(ctx, agent, store.RunIntentStopped)
	if err != nil {
		return err
	}

	dispatcher := s.GetDispatcher()
	stopRunID := agent.RunID
	if dispatcher != nil && agent.RuntimeBrokerID != "" {
		s.syncWorkspaceOnStop(ctx, agent)
		if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
			s.logStopRunMismatch(agent, "suspend", err)
			return err
		}
		// The superseded start claim is released last, after the
		// suspension's status write and quota release: released earlier, a
		// new start could take the claim and then lose its status and
		// reservation to them.
		defer s.releaseSupersededClaim(ctx, agent.ID, supersedes, intentAt)
	}

	// Record the suspension only while the row still holds the run the stop
	// was dispatched for; a newer run keeps its state, credentials and
	// reservation (ptone/scion#2550).
	newPhase := string(state.PhaseSuspended)
	recorded, err := s.recordStopStatus(ctx, agent.ID, stopRunID, "suspend", store.AgentStatusUpdate{
		Phase:           newPhase,
		ContainerStatus: "stopped",
		Activity:        "",
	})
	if err != nil {
		// The container is stopped but the write failed. Revoke the
		// credentials anyway while the row still holds the stopped run (a
		// check, not a lock), so a stopped container's credentials do not
		// outlive it; a newer run's are left alone.
		if s.stopRunStillCurrent(ctx, agent.ID, stopRunID, "suspend (credential revoke)") {
			s.revokeSuspendedCredentials(ctx, agent.ID)
		}
		return err
	}
	if !recorded {
		return nil
	}

	s.revokeSuspendedCredentials(ctx, agent.ID)

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

// revokeSuspendedCredentials revokes every credential of a suspended agent
// and audits it (best-effort, Phase 1H).
func (s *Server) revokeSuspendedCredentials(ctx context.Context, agentID string) {
	if _, err := s.store.RevokeAgentCredentialsByAgent(ctx, agentID, "system", "agent_suspended"); err != nil {
		slog.Warn("Failed to revoke agent credentials on suspend", "agent_id", agentID, "error", err)
	}
	s.emitMutationAudit(ctx, &store.MutationAuditRecord{
		MutationType: "agent_credential_revoke",
		TargetType:   "agent_credential",
		TargetID:     agentID,
	})
}

// recordRunIntent sets the agent's run intent in the store and mirrors the
// stored value on agent. Every lifecycle path that dispatches a start, stop,
// restart, create or delete records the intent first (see
// TestLifecycleDispatchCallsRecordRunIntent).
func (s *Server) recordRunIntent(ctx context.Context, agent *store.Agent, intent store.RunIntent) (time.Time, error) {
	_, at, err := s.swapRunIntent(ctx, agent, intent)
	return at, err
}

// swapRunIntent is recordRunIntent that also returns the intent the agent
// held before the write ("" for none). A system-initiated stop uses it to
// put back only an intent it actually replaced.
func (s *Server) swapRunIntent(ctx context.Context, agent *store.Agent, intent store.RunIntent) (store.RunIntent, time.Time, error) {
	prior, at, err := s.store.SwapRunIntent(ctx, agent.ID, intent)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("record run intent %s: %w", intent, err)
	}
	agent.RunIntent = intent
	stored := at
	agent.RunIntentAt = &stored
	return prior, at, nil
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
		// Fail fast on a GCP identity the token-mint gate would refuse.
		// Checked before restart's stop leg, so a refused restart leaves
		// the running agent alone. Stop and suspend are never gated.
		if s.gcpIdentityStartRefusal(ctx, w, agent, action) {
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

	// The start claim a stop supersedes: the one held when the stop is
	// recorded. It is released once the stop's dispatch succeeds.
	stopSupersedes := agent.StartClaimID
	var stopIntentAt time.Time
	if action == api.AgentActionStop {
		// A stop is recorded before the broker check, so it holds even when
		// the broker is offline: the stop is then queued for the broker's
		// reconnect.
		intentAt, err := s.recordRunIntent(ctx, agent, store.RunIntentStopped)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		stopIntentAt = intentAt
		if !s.brokerReachable(ctx, agent) {
			s.queueOfflineStop(w, r, agent, intentAt)
			return
		}
	} else if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// While this lifecycle action runs, the agent's container may be
	// legitimately absent with the row still in phase running (for example
	// between the stop and the start of a restart); keep the heartbeat
	// missing-container reconcile away from it until the final status write.
	defer s.beginLifecycleOp(agent.ID)()

	var newPhase string
	var dispatchErr error
	// startStatusWritten: startAgentCore wrote the started status.
	var startStatusWritten bool
	// startFromRest: a start of an agent in an uncounted phase, which
	// startAgentCore marked starting (a new generation).
	var startFromRest bool
	// startDispatched: a start's dispatch through startAgentCore reached the
	// broker successfully (its status write may have failed).
	var startDispatched bool
	// stopRunID is the run a stop is dispatched for; the stopped status is
	// recorded only while the row still holds it (ptone/scion#2550).
	var stopRunID string

	// Collect warnings the start leg raises (hub-side TZ drops and the
	// broker's hub-only env warnings) so the start and restart responses
	// carry them.
	ctx, dispatchWarns := withDispatchWarnings(ctx)

	// If a dispatcher is available, dispatch the operation to the runtime broker
	dispatcher := s.GetDispatcher()

	// sd holds the broker slot across a start or restart's start leg
	// (beginStartDispatch); the final status write below settles it.
	var sd *startDispatch

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
			// Flat placement pre-check: a stale pin is refused before the
			// reservation, the run intent and any credential or run-ID
			// write.
			if err := s.checkPinnedPlacement(agent); err != nil {
				if !writeRuntimeTargetRefusal(w, err) {
					writeErrorFromErr(w, err, "")
				}
				return
			}
			// From here the start no longer follows the client
			// (ptone/scion#1961): a client that gives up must not cancel the
			// broker launch, the rollback or the final status write. The
			// dispatch is bounded by syncDispatch (SyncDispatchBound).
			ctx = detachLaunchFromClient(ctx)
			// The start runs under a start claim, which records run intent
			// running (it stays running if the dispatch fails: a failed
			// start is still a start the user asked for). startAgentCore
			// re-reserves the per-broker ceiling before dispatch, exactly
			// as create does (ptone/scion#1963), marks an uncounted agent
			// starting so the quota reconcile keeps the slot
			// (ptone/scion#2014), rolls back the phase and a reservation it
			// made when the start fails (ptone/scion#1978), and writes the
			// started status while the claim is held.
			startFromRest = !isBrokerQuotaCountedPhase(agent.Phase)
			dispatchErr = s.startAgentCore(ctx, agent, StartOpts{Kind: store.StartClaimUser, Resume: resume, SyncDispatchBound: true})
			startDispatched = dispatchErr == nil || errors.Is(dispatchErr, errStartedStatusWrite)
			if dispatchErr == nil {
				startStatusWritten = true
				newPhase = agent.Phase
			} else if errors.Is(dispatchErr, errStartedStatusWrite) {
				// The start succeeded; only its status write failed. The
				// handler's own status write below runs and answers as
				// before if it fails too.
				dispatchErr = nil
			}
		} else if _, err := s.recordRunIntent(ctx, agent, store.RunIntentRunning); err != nil {
			writeRunIntentError(w, err, agent.ID)
			return
		}
	case api.AgentActionStop:
		newPhase = string(state.PhaseStopped)
		// Clear exposed ports — the agent is stopping so its ports are unreachable.
		s.clearExposedPortsForAgent(ctx, agent.ID)
		stopRunID = agent.RunID
		if dispatcher != nil && agent.RuntimeBrokerID != "" {
			// Before stopping, sync workspace back for hub-managed projects on remote brokers.
			// This is best-effort: failures are logged but don't block the stop.
			s.syncWorkspaceOnStop(ctx, agent)
			dispatchErr = dispatcher.DispatchAgentStop(ctx, agent)
			s.logStopRunMismatch(agent, "stop", dispatchErr)
		}
		// The max_agents_per_broker reservation is released once the
		// stopped status is recorded below, for the run that was stopped.
		if dispatchErr == nil {
			// Released last, after the stopped status write and the quota
			// release below (see suspendAgent).
			defer s.releaseSupersededClaim(ctx, agent.ID, stopSupersedes, stopIntentAt)
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
			if writeBrokerRuntimeUnavailable(w, err, agent.Runtime) {
				return
			}
			RuntimeError(w, "Failed to dispatch to runtime broker: "+err.Error())
			return
		}
		if agent.Phase != string(state.PhaseSuspended) {
			// The row moved to a newer run while the stop was in flight,
			// so nothing was recorded: answer with the current row, as a
			// stop does.
			if current, gerr := s.store.GetAgent(ctx, agent.ID); gerr == nil {
				agent = current
			}
		}
		respAgent := *agent
		respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
		writeJSON(w, http.StatusOK, respAgent)
		return
	case api.AgentActionRestart:
		newPhase = string(state.PhaseRunning)
		hasBroker := dispatcher != nil && agent.RuntimeBrokerID != ""
		var restartClaim *startClaimRun
		if hasBroker {
			// As for start: from here the restart no longer follows the
			// client (ptone/scion#1961). Each leg is bounded by
			// syncDispatch.
			ctx = detachLaunchFromClient(ctx)
			// Refuse before the stop leg: otherwise a broker without
			// the empty-per-agent capability would have the agent
			// stopped and then the start refused (design #2703 D3).
			if !s.requireEmptyPerAgentBrokerCapabilityForAgent(ctx, w, agent) {
				return
			}
			// Flat placement pre-check, before the broker cap check, the
			// start claim, the run intent and the stop leg: a refused
			// restart never stops the agent.
			if err := s.checkPinnedPlacement(agent); err != nil {
				if !writeRuntimeTargetRefusal(w, err) {
					writeErrorFromErr(w, err, "")
				}
				return
			}
			// Check the broker cap before the start claim, the run intent
			// write and the stop leg (ptone/scion#2010): a restart refused
			// at the cap must leave the agent as it was, with its container
			// up and its run intent untouched, not stopped and then
			// answered with 429. The reservation is held across both legs
			// (ptone/scion#1978): releasing it after the stop leg and
			// re-reserving before the start leg would let another start
			// take the slot in between. This reserve is a no-op for an
			// agent that already holds one, and applies the cap to an
			// agent that does not (for example, a stopped agent), which
			// is also marked starting until the start leg settles, so the
			// quota reconcile keeps the slot (ptone/scion#2014).
			var err error
			if sd, err = s.reserveStartCapacity(ctx, agent); err != nil {
				if !s.writeStartClaimError(w, err, agent.ID) && !writeStartQuotaError(w, err) {
					writeErrorFromErr(w, err, "")
				}
				return
			}
			// A restart leaves the agent running. Its start claim (which
			// records that intent) is taken before the stop leg, so no
			// other start runs between the stop and the start. A refused
			// claim undoes the capacity hold.
			if s.startClaimsEnabled() {
				run, err := s.acquireStartClaim(ctx, agent, store.StartClaimRestart)
				if err != nil {
					sd.rollback(ctx)
					if !s.writeStartClaimError(w, err, agent.ID) {
						writeErrorFromErr(w, err, "")
					}
					return
				}
				restartClaim = run
				// Released on every return that does not reach the start leg.
				defer func() {
					if restartClaim != nil {
						restartClaim.finish(startReleased)
					}
				}()
			}
		}
		// A restart leaves the agent running: record that before the stop
		// leg (with start claims, the restart's claim recorded it).
		if restartClaim == nil {
			if _, err := s.recordRunIntent(ctx, agent, store.RunIntentRunning); err != nil {
				sd.rollback(ctx)
				writeRunIntentError(w, err, agent.ID)
				return
			}
		}
		if hasBroker {
			// Restart is implemented as stop + start so that env vars
			// (API keys, secrets) are re-resolved from Hub storage.
			// Stop errors are tolerated: the container may already be
			// exited and some runtimes (podman) return non-standard
			// errors for stopping non-running containers. The subsequent
			// Start will handle cleanup of the exited container.
			stopErr := syncDispatch(ctx, func(dctx context.Context) error {
				return dispatcher.DispatchAgentStop(dctx, agent)
			})
			// The broker has no runtime of the agent's recorded type
			// registered (ptone/scion#2748): the agent may still be
			// running there, so do not start it anywhere else.
			if writeBrokerRuntimeUnavailable(w, stopErr, agent.Runtime) {
				slog.Warn("Restart: agent's runtime not available on broker, not starting",
					"agent_id", id, "runtime", agent.Runtime)
				sd.rollback(ctx)
				return
			}
			if stopErr != nil {
				slog.Warn("Restart: stop dispatch failed, proceeding with start",
					"agent_id", id, "error", stopErr)
			}
			// The dying container's own status report (phase stopped)
			// may have released the slot during the stop leg; the restart
			// held it throughout, so put it back without the cap check.
			sd.reassertReservation(ctx)
			// Restart is stop + start: a fresh harness session, not a resume.
			// The start leg runs under the claim taken before the stop leg,
			// with the capacity hold taken before it; its dispatch is bounded
			// by syncDispatch (SyncDispatchBound).
			existing := restartClaim
			restartClaim = nil
			dispatchErr = s.startAgentCore(ctx, agent, StartOpts{Kind: store.StartClaimRestart, Existing: existing, Dispatch: sd, NewGeneration: true, SyncDispatchBound: true})
			if dispatchErr == nil {
				startStatusWritten = true
				newPhase = agent.Phase
			} else if errors.Is(dispatchErr, errStartedStatusWrite) {
				// The start leg succeeded; only its status write failed:
				// keep the reservation and let the write below run.
				dispatchErr = nil
			}
			if dispatchErr != nil {
				if errors.Is(dispatchErr, errStartClaimLost) {
					// The claim was lost while the start leg ran (a stop
					// superseded it): the container may be up, so no
					// stopped state is recorded and the slot is not
					// released; startAgentCore already undid only what its
					// hold created.
				} else if errors.Is(dispatchErr, store.ErrDeleteInProgress) {
					// A delete claimed the row between the legs
					// (ptone/scion#2550, round 5 N3). The delete engine
					// owns the row now: leave its phase and the
					// reservation it held to the engine, and undo only a
					// reservation this call created. The phase restore
					// is conditional on the row still reading starting,
					// so it leaves the engine's phase alone.
					sd.rollback(ctx)
				} else if stopErr == nil {
					// The stop leg succeeded, so the container is down:
					// release the slot and record the stopped state as an
					// explicit stop would, so the agent does not keep
					// showing its pre-restart phase until the next
					// heartbeat.
					sd.settle()
					// Guard on the run this restart left on the row: the
					// failed start leg may keep the run it minted (or the
					// broker's), and the dispatcher keeps agent.RunID in
					// step with it. A run from another caller still misses.
					if s.recordRestartStopped(ctx, agent.ID, agent.RunID) {
						s.releaseBrokerQuota(ctx, agent)
					}
				} else {
					// The container may still be running: keep a
					// reservation this call did not create, and
					// restore the phase.
					sd.rollback(ctx)
				}
			}
		}
	}

	// If dispatch failed, return error. A required-skill resolution failure
	// keeps the broker's status and code; anything else is a 502.
	if dispatchErr != nil {
		if s.writeStartClaimError(w, dispatchErr, agent.ID) || writeStartQuotaError(w, dispatchErr) {
			return
		}
		if ref := deleteClaimedDuringDispatch(dispatchErr, agent.ID); ref != nil {
			ref.write(w)
			return
		}
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
		if writeBrokerRuntimeUnavailable(w, dispatchErr, agent.Runtime) {
			return
		}
		if writeAgentTokenIssueError(w, dispatchErr) {
			return
		}
		if writeEmptyPerAgentCapabilityError(w, dispatchErr) {
			return
		}
		// A flat Runtime Broker refusal is a definite start failure that no
		// retry can fix: settle it by recording the refusal message on the
		// agent, then relay the refusal.
		s.settleRuntimeTargetRefusal(ctx, agent, dispatchErr)
		if relayDispatchRefusal(w, dispatchErr) {
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
		// A new generation: clear the prior one's message and stalled
		// marker too, whatever the row reads now (beginStartDispatch wrote
		// starting, or a restart's stop leg ran, and a heartbeat guarded
		// mid-dispatch may have stored the old container's exit message).
		// A start on an agent that was already running is not a new
		// generation and keeps its live status.
		statusUpdate.ClearTerminalRemnants = action == api.AgentActionRestart || sd.wroteStarting() || startFromRest
	}
	// A start or restart whose broker start landed after a delete won
	// (ptone/scion#3255): the dispatch has already deleted the landed run
	// again (compensateLandedRun), so answer 409 delete_in_progress as the
	// mid-dispatch case does, rather than writing the status and answering
	// 200 (delete-claimed or soft-deleted row) or 404 (row gone). The
	// delete engine owns the row and its reservation; nothing is written or
	// published here. Reached only after a successful dispatch
	// (dispatchErr == nil above). A restart's start leg holds sd; a start
	// dispatched through startAgentCore sets startDispatched.
	landed := (sd != nil || startDispatched) && (action == api.AgentActionStart || action == api.AgentActionRestart)
	if landed {
		if s.deleteWonAfterLanding(ctx, id) {
			writeDeleteWon(w, id, deletedWhileStartingMessage, dispatchWarns.Warnings())
			return
		}
	}
	if action == api.AgentActionStop {
		// Only while the row still holds the stopped run: a newer run keeps
		// its state and reservation (ptone/scion#2550).
		recorded, err := s.recordStopStatus(ctx, id, stopRunID, "stop", statusUpdate)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if !recorded {
			current, gerr := s.store.GetAgent(ctx, id)
			if gerr != nil {
				writeErrorFromErr(w, gerr, "")
				return
			}
			respAgent := *current
			respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(current.AppliedConfig, canViewAgentEnv(ctx, s, current))
			respAgent.Deletion = store.ComputeAgentDeletion(current, time.Now())
			writeJSON(w, http.StatusOK, agentLifecycleResponse{Agent: &respAgent, Warnings: dispatchWarns.Warnings()})
			return
		}
		// A stopped agent has no running container: release its
		// max_agents_per_broker reservation (ptone/scion#1963).
		s.releaseBrokerQuota(ctx, agent)
	} else if !startStatusWritten {
		// A start or restart run by startAgentCore wrote its status while
		// its claim was held.
		if err := s.store.UpdateAgentStatus(ctx, id, statusUpdate); err != nil {
			// The row was hard-deleted between the re-read above and this
			// write: the same delete_in_progress answer.
			if landed && errors.Is(err, store.ErrNotFound) {
				writeDeleteWon(w, id, deletedWhileStartingMessage, dispatchWarns.Warnings())
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}
	}

	// A successful start/stop/restart clears a failed delete marker
	// (design ptone/scion#2483 §2.1); publish and respond from the stored
	// row, which a racing delete claim may have kept off newPhase.
	reloaded := s.settleLifecycleWrite(ctx, agent, newPhase)
	// A stopped report about the old container (its own status POST still
	// in flight, or a heartbeat handled by another replica) can land after
	// the restart's post-stop re-assert and release the slot during the
	// start leg. Re-assert once more now that the final write landed, if the
	// reloaded row is counted and no delete holds it (skipped when the
	// reload failed: the gate would decide on stale columns). A report after
	// this point heals itself: the next running heartbeat re-reserves on
	// stopped -> running (best-effort, with the cap check; at the cap the
	// hourly backfill records it).
	if action == api.AgentActionRestart && sd != nil && reloaded &&
		isBrokerQuotaCountedPhase(agent.Phase) && agent.DeletedAt.IsZero() && !deletionActive(agent) {
		s.reassertBrokerReservation(ctx, agent)
	}
	s.events.PublishAgentStatus(ctx, agent)

	respAgent := *agent
	respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
	respAgent.Deletion = store.ComputeAgentDeletion(agent, time.Now())
	writeJSON(w, http.StatusOK, agentLifecycleResponse{Agent: &respAgent, Warnings: dispatchWarns.Warnings()})
}

// Offline stop: the container status and message an agent gets when its
// stop is queued for an offline broker.
const (
	containerStatusStopQueued = "stop_queued"
	offlineStopMessage        = "Stop queued: broker offline; it will be applied when the broker reconnects."
)

// queueOfflineStop handles a stop for an agent whose broker is offline. The
// stop intent is already recorded at intentAt. It queues a durable stop
// dispatch carrying intentAt, which the broker's reconnect drain applies only
// if no newer start or stop has been recorded since (see execDispatchStop),
// marks the agent stopped with container status stop_queued, and responds
// 202 Accepted with a warning.
func (s *Server) queueOfflineStop(w http.ResponseWriter, r *http.Request, agent *store.Agent, intentAt time.Time) {
	ctx := r.Context()
	at := intentAt
	argsJSON, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &at, SupersedesClaim: agent.StartClaimID, RunID: agent.RunID})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	dispatch := &store.BrokerDispatch{
		ID:        uuid.NewString(),
		BrokerID:  agent.RuntimeBrokerID,
		AgentID:   agent.ID,
		AgentSlug: agent.Slug,
		ProjectID: agent.ProjectID,
		Op:        "stop",
		Args:      argsJSON,
	}
	setBrokerDispatchInitiator(ctx, dispatch)
	if err := s.store.InsertBrokerDispatch(ctx, dispatch); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.clearExposedPortsForAgent(ctx, agent.ID)
	newPhase := string(state.PhaseStopped)
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:           newPhase,
		ContainerStatus: containerStatusStopQueued,
		Activity:        "",
		Message:         offlineStopMessage,
	}); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.agentLifecycleLog.Info("Stop queued for offline broker",
		"agent_id", agent.ID, "broker_id", agent.RuntimeBrokerID, "dispatch_id", dispatch.ID)

	agent.Phase = newPhase
	agent.ContainerStatus = containerStatusStopQueued
	agent.Activity = ""
	agent.Message = offlineStopMessage
	s.events.PublishAgentStatus(ctx, agent)

	// The broker may have reconnected after the reachability check, in which
	// case its reconnect drain already ran and missed this row.
	s.wakeBrokerDrain(ctx, agent.RuntimeBrokerID)

	respAgent := *agent
	respAgent.AppliedConfig = redactAppliedConfigEnvForResponse(agent.AppliedConfig, canViewAgentEnv(ctx, s, agent))
	writeJSON(w, http.StatusAccepted, agentLifecycleResponse{Agent: &respAgent, Warnings: []string{offlineStopMessage}})
}

// wakeBrokerDrain asks the node that holds brokerID's control channel to
// drain its pending broker_dispatch rows. It sends a best-effort command-bus
// signal (other nodes) and, when this node holds the channel, starts a local
// drain. Both are wakeups only: the row is durable, and a reconnect drains
// it as well. The drain is CAS-gated, so a duplicate wakeup is harmless.
func (s *Server) wakeBrokerDrain(ctx context.Context, brokerID string) {
	if brokerID == "" {
		return
	}
	if s.commandBus != nil {
		if err := s.commandBus.SignalBrokerCmd(ctx, brokerID); err != nil {
			s.agentLifecycleLog.Warn("Broker drain signal failed; the row drains on reconnect",
				"broker_id", brokerID, "error", err)
		}
	}
	if s.controlChannel != nil && s.controlChannel.IsConnected(brokerID) {
		go func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), brokerDrainWakeTimeout)
			defer cancel()
			s.reconcileBroker(dctx, brokerID)
		}()
	}
}

// brokerDrainWakeTimeout bounds the local drain wakeBrokerDrain starts. A
// drain cut short leaves its unclaimed rows queued for the next drain; the
// row in flight at the deadline stays in progress until ReapStuckDispatch
// requeues it.
const brokerDrainWakeTimeout = 5 * time.Minute

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

// recordRestartStopped records a restart whose stop leg succeeded but whose
// start leg failed the way an explicit stop is recorded: phase and container
// status stopped, exposed ports cleared, and a status event published, so
// clients do not keep showing the pre-restart state until the next
// heartbeat. Failures are logged; the caller still reports the start error.
//
// The stopped state is recorded only while the row still holds runID, the
// run this restart left on the row after its failed start leg
// (ptone/scion#2550); it reports
// whether it was recorded, and the caller releases the reservation only
// then.
func (s *Server) recordRestartStopped(ctx context.Context, id, runID string) bool {
	zero := 0
	recorded, err := s.recordStopStatus(ctx, id, runID, "restart", store.AgentStatusUpdate{
		Phase:           string(state.PhaseStopped),
		ContainerStatus: "stopped",
		ExitCode:        &zero,
	})
	if err != nil {
		slog.Warn("Restart: failed to record stopped state after start leg failed",
			"agent_id", id, "error", err)
		return false
	}
	if !recorded {
		return false
	}
	s.clearExposedPortsForAgent(ctx, id)
	stored, err := s.store.GetAgent(ctx, id)
	if err != nil {
		slog.Warn("Restart: failed to fetch agent for status event", "agent_id", id, "error", err)
		return true
	}
	if stored.DeletedAt.IsZero() {
		s.events.PublishAgentStatus(ctx, stored)
	}
	return true
}

// StopAllAgentsResponse is the response from the stop-all endpoint.
type StopAllAgentsResponse struct {
	Stopped int `json:"stopped"`
	Failed  int `json:"failed"`
	// StopRecorded counts agents whose start was in flight: their intent
	// is now stopped, but the start was not interrupted.
	StopRecorded int             `json:"stopRecorded,omitempty"`
	Total        int             `json:"total"`
	Scope        string          `json:"scope,omitempty"` // "all" or "own"
	Results      []stopAllResult `json:"results"`
}

// stopAllStatusStopRecorded is the stop-all result status for an agent whose
// start was in flight: the stop intent is recorded and nothing is dispatched.
const stopAllStatusStopRecorded = "stop_recorded"

// stopAllStatusRunChanged is the stop-all result status for an agent whose
// row moved to a newer run while its stop was dispatched: the old run was
// stopped, and the newer run's state is left as it is (ptone/scion#2550).
const stopAllStatusRunChanged = "run_changed"

// startInFlightPhase reports whether phase is one a start passes through
// before the agent is running.
func startInFlightPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting:
		return true
	}
	return false
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
	// Running agents, plus any agent whose recorded intent is still running
	// (for example one in phase error), so stop-all leaves no agent
	// intended to run.
	filter := store.AgentFilter{
		ProjectID:   projectID,
		Phase:       string(state.PhaseRunning),
		OrRunIntent: string(store.RunIntentRunning),
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

			supersedes := agent.StartClaimID
			intentAt, err := s.recordRunIntent(ctx, agent, store.RunIntentStopped)
			if err != nil {
				res.Status = "error"
				res.Error = err.Error()
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}
			// An agent that is not running only had its intent left at
			// running: recording the stop intent is all it needs. For an
			// agent whose start is still in flight, the start is not
			// interrupted: the agent may still come up running, with
			// intent stopped (as after a user stop whose dispatch failed).
			// It is reported as stop_recorded rather than stopped.
			if agent.Phase != string(state.PhaseRunning) {
				res.Status = "stopped"
				if startInFlightPhase(agent.Phase) {
					res.Status = stopAllStatusStopRecorded
				}
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
				return
			}

			// Dispatch stop to broker
			var dispatchErr error
			stopRunID := agent.RunID
			if dispatcher != nil && agent.RuntimeBrokerID != "" {
				opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				s.syncWorkspaceOnStop(opCtx, agent)
				dispatchErr = dispatcher.DispatchAgentStop(opCtx, agent)
				if dispatchErr == nil {
					// Released last, after this agent's stopped status
					// write and quota release (see suspendAgent).
					defer s.releaseSupersededClaim(ctx, agent.ID, supersedes, intentAt)
				}
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
				recorded, updateErr := s.recordStopStatus(ctx, agent.ID, stopRunID, "stop-all", statusUpdate)
				if updateErr != nil {
					res.Status = "error"
					res.Error = updateErr.Error()
				} else if !recorded {
					res.Status = stopAllStatusRunChanged
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
	recorded := 0
	for _, r := range results {
		switch r.Status {
		case "stopped":
			stopped++
		case stopAllStatusStopRecorded:
			recorded++
		default:
			failed++
		}
	}

	writeJSON(w, http.StatusOK, StopAllAgentsResponse{
		Stopped:      stopped,
		Failed:       failed,
		StopRecorded: recorded,
		Total:        len(results),
		Scope:        scope,
		Results:      results,
	})
}
