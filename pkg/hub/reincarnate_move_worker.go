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
}

// expectedNFSWorkspace is the workspace the target broker must find on its
// mount of the export before provisioning the moved agent.
func (m *reincarnationMove) expectedNFSWorkspace() string {
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
// project; the capacity limit is enforced when the slot is reserved. It
// returns the agent it validated, as it is on the source broker.
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
	allowed := func(*store.RuntimeBroker) bool { return true }
	_, ref := evaluateMoveEligibility(moveEligibilityInput{
		Agent:     a,
		Src:       src,
		Dst:       dst,
		CloneMode: mv.AgentDirWorkspace,
		Profile:   mv.Profile,
		SelfMove:  mv.SelfMove,
		Probes: moveProbes{
			Reachable:        s.brokerRecordReachable,
			CanDispatch:      allowed,
			CanUseAsProvider: allowed,
			ServesProject: func(b *store.RuntimeBroker) bool {
				return s.brokerServesProject(ctx, b.ID, mv.ProjectID)
			},
			Capacity: func(*store.RuntimeBroker) string { return "" },
		},
	})
	if ref != nil {
		return nil, fmt.Errorf("move no longer eligible: %s", ref.Message)
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
// the target. Release comes first: Reserve short-circuits on any active
// reservation for the agent, whatever its broker, so reserving first would
// leave the slot counted against the source. When the target has no room,
// the source reservation is re-asserted and the error returned.
func (s *Server) moveBrokerQuota(ctx context.Context, a *store.Agent, mv *reincarnationMove) error {
	src := withBroker(a, mv.SourceBrokerID)
	s.releaseBrokerQuota(ctx, src)
	if _, err := s.checkAndReserveBrokerQuota(ctx, withBroker(a, mv.TargetBrokerID)); err != nil {
		s.reassertBrokerReservation(ctx, src)
		return err
	}
	return nil
}

// linkMoveTargetProvider links the target broker to the agent's project if
// it does not serve it yet. The caller's right to link it was checked when
// the move was accepted; a self-move's target already serves the project,
// so a self-move never links.
func (s *Server) linkMoveTargetProvider(ctx context.Context, mv *reincarnationMove) error {
	if mv.SelfMove || s.brokerServesProject(ctx, mv.TargetBrokerID, mv.ProjectID) {
		return nil
	}
	dst, err := s.store.GetRuntimeBroker(ctx, mv.TargetBrokerID)
	if err != nil {
		return err
	}
	return s.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  mv.ProjectID,
		BrokerID:   dst.ID,
		BrokerName: dst.Name,
		Status:     dst.Status,
		LinkedBy:   "agent-move",
	})
}

// rollbackMove undoes a move that failed after the agent was assigned to
// the target broker and before it definitely started there, leaving the
// agent on the source as it was: a best-effort localOnly delete of whatever
// the target created; the agent's broker, runtime, run and workspace
// placement restored from src (the agent as it was on the source); and,
// only once that assignment is restored, the broker reservation moved back.
// The caller runs it only after winning the record's CAS to failed, then
// writes the failed agent with the previous applied config.
func (s *Server) rollbackMove(ctx context.Context, md agentMoveDispatcher, agentID string, mv *reincarnationMove, src *store.Agent) {
	cur, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		s.agentLifecycleLog.Error("move rollback: failed to load agent; it may be left assigned to the target broker",
			"agent_id", agentID, "target_broker_id", mv.TargetBrokerID, "error", err)
		return
	}
	if err := md.DispatchAgentDeleteLocalOnly(ctx, withBroker(cur, mv.TargetBrokerID)); err != nil {
		s.agentLifecycleLog.Warn("move rollback: could not remove the agent's state on the target broker; left in place",
			"agent_id", agentID, "target_broker_id", mv.TargetBrokerID, "error", err)
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
	// The target's start may have recorded its own run; put the source's
	// back (unless a newer run was recorded since), so later deletes and
	// launch reports address the run that exists on the source. Only once
	// the agent is assigned to the source again, so the row never pairs the
	// target with the source's run.
	if cur.RunID != src.RunID {
		if _, err := s.store.CompareAndSwapAgentRunID(ctx, agentID, cur.RunID, src.RunID); err != nil {
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
	s.reassertBrokerReservation(ctx, withBroker(cur, mv.SourceBrokerID))
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
		s.reassertBrokerReservation(ctx, a)
	}
	if rec.SourceCleanup == "" {
		if err := s.store.SetAgentReincarnationSourceCleanup(ctx, rec.ID, sourceCleanupSkippedInterrupted); err != nil {
			s.agentLifecycleLog.Warn("failed move: could not record the skipped source cleanup",
				"agent_id", rec.AgentID, "reincarnation_id", rec.ID, "error", err)
		}
	}
}
