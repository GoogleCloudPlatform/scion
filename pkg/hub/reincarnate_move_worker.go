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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reincarnationMove describes a cross-broker move carried out by the
// reincarnate worker (`scion reincarnate --broker`, design ptone/scion#2727
// §3.4). A nil *reincarnationMove is a plain, same-broker reincarnation.
type reincarnationMove struct {
	SourceBrokerID string
	TargetBrokerID string
	ProjectID      string
	// SelfMove is true when the agent asked to move itself: the target
	// already serves the project and no provider link is ever created.
	SelfMove bool
	// AgentDirWorkspace is true when the workspace is the agent's own
	// directory on the export (clone-per-agent, empty-per-agent); false for
	// the project's shared checkout (shared-plain, hub-managed).
	AgentDirWorkspace bool
	// Profile is the profile the move was checked against (explicit, else
	// the project's active profile; "" = the target's default).
	Profile string
	// Force is the request's forceMove: the eligibility re-check runs with
	// it, as the request's check did.
	Force bool
	// TransferMode is the verdict's transfer mode when the move was
	// accepted (MoveTransferModeExport or MoveTransferModeRecreate; "" is
	// read as export, the only mode before forced moves). The re-check
	// refuses a move whose mode has changed since, and a recreate move
	// provisions a fresh workspace and rolls back with a files delete.
	TransferMode string
}

// transferMode is the move's transfer mode, reading "" as export.
func (m *reincarnationMove) transferMode() string {
	if m.TransferMode == "" {
		return MoveTransferModeExport
	}
	return m.TransferMode
}

// recreates reports whether the move provisions the workspace fresh on the
// target (a forced move).
func (m *reincarnationMove) recreates() bool {
	return m.transferMode() == MoveTransferModeRecreate
}

// expectedNFSWorkspace is the moved-agent workspace expectation the target
// broker checks before provisioning: for an export move, the workspace it
// must find on its mount of the export; for a recreate move, that it has
// nothing for the agent yet.
func (m *reincarnationMove) expectedNFSWorkspace() string {
	if m.recreates() {
		return agent.MoveWorkspaceFreshAgentDir
	}
	if m.AgentDirWorkspace {
		return agent.MoveWorkspaceAgentDir
	}
	return agent.MoveWorkspaceProject
}

// recheckMoveEligibility re-runs the move eligibility checks against the
// current agent and broker records, just before the first side effect: the
// facts may have changed since the request was accepted. Caller access was
// decided at request time (the worker has no request identity), so the
// access check re-confirms only that a self-move's target still serves the
// project; the capacity limit is enforced when the slot is reserved. The
// project's workspace facts are read again, as the request read them, and
// the check runs with the request's force flag. A move whose transfer mode
// differs from the one it was accepted with is refused: a move never
// switches between carrying and recreating the workspace. It returns the
// agent it validated, as it is on the source broker.
func (s *Server) recheckMoveEligibility(ctx context.Context, agentID string, mv *reincarnationMove) (*store.Agent, error) {
	a, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}
	if a.RuntimeBrokerID != mv.SourceBrokerID {
		return nil, fmt.Errorf("agent is no longer on broker %s", mv.SourceBrokerID)
	}
	src, err := s.store.GetRuntimeBroker(ctx, mv.SourceBrokerID)
	if err != nil {
		return nil, fmt.Errorf("load source broker: %w", err)
	}
	dst, err := s.store.GetRuntimeBroker(ctx, mv.TargetBrokerID)
	if err != nil {
		return nil, fmt.Errorf("load target broker: %w", err)
	}
	project, err := s.store.GetProject(ctx, mv.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load project: %w", err)
	}
	// The workspace gate is judged again, exactly as the request judged it.
	gate := s.reincarnateWorkspaceGate(ctx, a, project, dst, mv.Force)
	if gate.workspaceModeErr == "" && gate.cloneMode != mv.AgentDirWorkspace {
		return nil, fmt.Errorf("move no longer eligible: the agent's workspace mode changed since the move was accepted")
	}
	allowed := func(*store.RuntimeBroker) bool { return true }
	v, ref := evaluateMoveEligibility(moveEligibilityInput{
		Agent:              a,
		Src:                src,
		Dst:                dst,
		WorkspaceModeError: gate.workspaceModeErr,
		CloneMode:          gate.cloneMode,
		Profile:            mv.Profile,
		SelfMove:           mv.SelfMove,
		Force:              mv.Force,
		EmptyPerAgent:      project.IsEmptyPerAgent(),
		WorkspaceMode:      string(project.SharingMode()),
		GitRemote:          project.GitRemote,
		Probes: moveProbes{
			Reachable:        func(b *store.RuntimeBroker) bool { return s.brokerRecordReachable(ctx, b) },
			CanDispatch:      allowed,
			CanUseAsProvider: allowed,
			ServesProject: func(b *store.RuntimeBroker) bool {
				return s.brokerServesProject(ctx, b.ID, mv.ProjectID)
			},
			Capacity:     func(*store.RuntimeBroker) string { return "" },
			HostSARecord: s.hostSARecordMoveProbe(ctx, a.AppliedConfig),
		},
	})
	if ref != nil {
		return nil, fmt.Errorf("move no longer eligible: %s", ref.Message)
	}
	if v.TransferMode != mv.transferMode() {
		return nil, fmt.Errorf("move no longer eligible: the workspace transfer mode changed from %s to %s since the move was accepted",
			mv.transferMode(), valueOrUnknown(v.TransferMode))
	}
	return a, nil
}

// withBroker returns a copy of a with its runtime broker replaced, for
// dispatching to or accounting against a specific broker.
func withBroker(a *store.Agent, brokerID string) *store.Agent {
	c := *a
	c.RuntimeBrokerID = brokerID
	return &c
}

// moveBrokerQuota moves the agent's broker reservation from the source to
// the target. Release comes first: Reserve refuses a request while the agent
// holds an active reservation on another broker (ReservationScopeConflictError),
// so reserving first would always fail. If the source release itself fails,
// the target reserve returns that conflict and the move fails with the slot
// still on the source. When the target has no room, the source reservation
// is re-asserted and the error returned.
func (s *Server) moveBrokerQuota(ctx context.Context, a *store.Agent, mv *reincarnationMove) error {
	src := withBroker(a, mv.SourceBrokerID)
	s.releaseBrokerQuota(ctx, src)
	if _, err := s.checkAndReserveBrokerQuota(ctx, withBroker(a, mv.TargetBrokerID)); err != nil {
		s.restoreBrokerReservation(ctx, src)
		return err
	}
	return nil
}

// linkMoveTargetProvider links the target broker to the agent's project if
// it does not serve it yet. The caller's right to link it was checked when
// the move was accepted; a self-move's target already serves the project,
// so a self-move never links.
//
// A move onto the co-located broker also records, at once, the references
// it adds there (the project's activeProfile and the moved agent's own
// profile), whether or not it links, so the built-ins they name are
// retained before the move provisions (ptone/scion#5113).
func (s *Server) linkMoveTargetProvider(ctx context.Context, mv *reincarnationMove) error {
	if mv.SelfMove {
		return nil
	}
	if s.brokerServesProject(ctx, mv.TargetBrokerID, mv.ProjectID) {
		s.onColocatedPlacement(ctx, mv.ProjectID, mv.TargetBrokerID, mv.Profile)
		return nil
	}
	dst, err := s.store.GetRuntimeBroker(ctx, mv.TargetBrokerID)
	if err != nil {
		return err
	}
	if err := s.linkProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  mv.ProjectID,
		BrokerID:   dst.ID,
		BrokerName: dst.Name,
		Status:     dst.Status,
		LinkedBy:   "agent-move",
	}); err != nil {
		return err
	}
	// linkProjectProvider recorded the project's activeProfile; add the
	// moved agent's own profile.
	s.addColocatedAgentReference(dst.ID, mv.Profile)
	return nil
}

// rollbackMove undoes a move that failed after the agent was assigned to
// the target broker and before it definitely started there, leaving the
// agent on the source as it was: a best-effort delete of whatever the
// target created (localOnly for an export move, whose workspace on the
// export is the agent's real one; a files delete for a recreate move, whose
// target workspace this move created, see rollbackMoveTarget; nothing when
// targetUntouched, for a forced move the target refused before creating
// anything); the agent's broker, runtime, run and workspace placement
// restored from src (the agent as it was on the source); and,
// only once that assignment is restored, the broker reservation moved back.
// The caller runs it only after winning the record's CAS to failed, then
// writes the failed agent with the previous applied config.
func (s *Server) rollbackMove(ctx context.Context, md agentMoveDispatcher, agentID, reincarnationID string, mv *reincarnationMove, src *store.Agent, targetUntouched bool) {
	cur, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		s.agentLifecycleLog.Error("move rollback: failed to load agent; it may be left assigned to the target broker",
			"agent_id", agentID, "target_broker_id", mv.TargetBrokerID, "error", err)
		return
	}
	if targetUntouched {
		s.agentLifecycleLog.Info("move rollback: the target broker created nothing for the agent; nothing to remove there",
			"agent_id", agentID, "target_broker_id", mv.TargetBrokerID)
	} else if err := rollbackMoveTarget(ctx, md, withBroker(cur, mv.TargetBrokerID), mv, src.RunID, reincarnationID); isMoveMarkerMismatch(err) {
		// The target found files for the agent that this move did not
		// create, and removed nothing: not a rollback failure.
		s.agentLifecycleLog.Info("move rollback: target cleanup skipped: target files not created by this move",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "target_broker_id", mv.TargetBrokerID)
	} else if err != nil {
		s.agentLifecycleLog.Warn("move rollback: could not remove the agent's state on the target broker; left in place",
			"agent_id", agentID, "target_broker_id", mv.TargetBrokerID, "transfer_mode", mv.TransferMode, "error", err)
	}
	srcID, srcRuntime := mv.SourceBrokerID, src.Runtime
	if _, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: cur.ReincarnationState,
		runtimeBrokerID:    &srcID,
		runtime:            &srcRuntime,
	}, reincarnationStepMaxAttempts+3); err != nil {
		s.agentLifecycleLog.Error("move rollback: failed to restore the agent to the source broker; the reservation stays on the target",
			"agent_id", agentID, "source_broker_id", mv.SourceBrokerID, "error", err)
		return
	}
	// The target's start may have recorded its own run; revert to the
	// source's (unless a newer run was recorded since), so later deletes and
	// launch reports address the run that exists on the source. A revert,
	// not a settle: the runs listed before the move may still have entries.
	// Only once the agent is assigned to the source again, so the row never
	// pairs the target with the source's run.
	if cur.RunID != src.RunID {
		if _, err := s.store.RevertAgentRunID(ctx, agentID, cur.RunID, src.RunID); err != nil {
			s.agentLifecycleLog.Warn("move rollback: failed to restore the agent's source run ID",
				"agent_id", agentID, "error", err)
		}
	}
	if src.WorkspacePlacement != "" {
		if err := s.store.SetAgentWorkspacePlacement(ctx, agentID, src.WorkspacePlacement); err != nil {
			s.agentLifecycleLog.Warn("move rollback: failed to restore the agent's workspace placement",
				"agent_id", agentID, "error", err)
		}
	}
	s.releaseBrokerQuota(ctx, cur)
	s.restoreBrokerReservation(ctx, withBroker(cur, mv.SourceBrokerID))
}

// rollbackMoveTarget removes what a failed move created for the agent on
// the target broker. An export move sends a localOnly delete: the
// workspace on the shared export is the agent's real one and stays. A
// recreate move sends a files delete (never removing the branch, which
// lives on the remote) carrying the move's reincarnation ID
// (reincarnationID). That is safe because the broker removes only
// directories marked by this move: the provision marks each agent
// directory it creates, right after the absence check passed, and the
// broker refuses (api.BrokerErrCodeMoveMarkerMismatch, removing nothing)
// when a directory that exists is unmarked or marked by another move. The
// files delete names the target's run only when the target recorded one
// (dst's run differs from srcRunID, the source's); otherwise the target
// resolves the agent by name within the project. Nothing is ever sent to
// the source. dst is the agent as assigned to the target.
func rollbackMoveTarget(ctx context.Context, md agentMoveDispatcher, dst *store.Agent, mv *reincarnationMove, srcRunID, reincarnationID string) error {
	if mv.recreates() {
		if dst.RunID == srcRunID {
			c := *dst
			c.RunID = ""
			dst = &c
		}
		return md.DispatchAgentDeleteRecreated(ctx, dst, reincarnationID)
	}
	return md.DispatchAgentDeleteLocalOnly(ctx, dst)
}

// cleanUpMoveSource removes the agent's broker-local state from the source
// broker after a completed move (best effort: a failure is logged and
// recorded on the reincarnation record, and never fails the move). src is
// the agent as it was on the source broker, so the delete targets the
// source's run.
func (s *Server) cleanUpMoveSource(ctx context.Context, md agentMoveDispatcher, reincarnationID string, src *store.Agent) {
	outcome := store.SourceCleanupDone
	if src.RunID == "" {
		// Without a run ID the source broker would resolve the delete by
		// name alone, which on a runtime namespace shared with the target
		// could remove the agent's new entry there. Leave the source alone.
		outcome = sourceCleanupSkippedNoRunID
		s.agentLifecycleLog.Warn("move: the agent has no run ID on the source broker; its local state there is left in place",
			"agent_id", src.ID, "source_broker_id", src.RuntimeBrokerID, "agent", src.Slug)
	} else if err := md.DispatchAgentDeleteLocalOnly(ctx, src); err != nil {
		outcome = "failed:" + err.Error()
		s.agentLifecycleLog.Warn("move: could not remove the agent's state on the source broker; left in place",
			"agent_id", src.ID, "source_broker_id", src.RuntimeBrokerID, "agent", src.Slug, "error", err)
	}
	if err := s.store.SetAgentReincarnationSourceCleanup(ctx, reincarnationID, outcome); err != nil {
		s.agentLifecycleLog.Warn("move: failed to record the source cleanup outcome",
			"agent_id", src.ID, "reincarnation_id", reincarnationID, "outcome", outcome, "error", err)
	}
}

// Skipped source cleanup outcomes recorded on a move's record.
const (
	sourceCleanupSkippedNoRunID        = "skipped:no-run-id"
	sourceCleanupSkippedAmbiguousStart = "skipped:ambiguous-start"
	sourceCleanupSkippedInterrupted    = "skipped:interrupted"
)

// reconcileFailedMove runs whenever a move's record is failed (by the worker
// or the stale sweep). The agent row's runtime broker is the truth: on the
// source, the broker reservation is moved back there (released, then
// re-asserted); on the target, it already follows the agent. A record with
// no source cleanup outcome yet records skipped:interrupted. A plain
// reincarnation's record is left alone.
func (s *Server) reconcileFailedMove(ctx context.Context, reincarnationID string) {
	rec, err := s.store.GetAgentReincarnation(ctx, reincarnationID)
	if err != nil || rec.SourceBrokerID == "" {
		return
	}
	a, err := s.store.GetAgent(ctx, rec.AgentID)
	if err != nil {
		s.agentLifecycleLog.Warn("failed move: could not load the agent to reconcile its reservation",
			"agent_id", rec.AgentID, "reincarnation_id", rec.ID, "error", err)
	} else if a.RuntimeBrokerID == rec.SourceBrokerID {
		s.releaseBrokerQuota(ctx, a)
		s.restoreBrokerReservation(ctx, a)
	}
	if rec.SourceCleanup == "" {
		if err := s.store.SetAgentReincarnationSourceCleanup(ctx, rec.ID, sourceCleanupSkippedInterrupted); err != nil {
			s.agentLifecycleLog.Warn("failed move: could not record the skipped source cleanup",
				"agent_id", rec.AgentID, "reincarnation_id", rec.ID, "error", err)
		}
	}
}
