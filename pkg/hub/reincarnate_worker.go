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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runReincarnationWorker performs the reincarnation teardown/reprovision/start
// sequence in the background (design §3.1): stop → write new AppliedConfig →
// DispatchAgentReprovision → DispatchAgentStart(task=preamble+handoff,
// resume=false) → complete. ctx is expected to be a detached context
// (context.Background()-derived), independent of the HTTP request that
// triggered this — see handleReincarnateAgent for why that matters for
// self-migration.
//
// fresh is the new generation's AppliedConfig, already fully resolved by
// buildFreshAppliedConfig at request time (before the 202 was returned); this
// function only adds the Task (the preamble + handoff, per design §3.3
// "Task | Replaced") and persists it.
func (s *Server) runReincarnationWorker(ctx context.Context, agentID, reincarnationID string, fresh *store.AgentAppliedConfig, handoff string) {
	defer func() {
		if p := recover(); p != nil {
			s.agentLifecycleLog.Error("reincarnation worker panicked",
				"agent_id", agentID, "reincarnation_id", reincarnationID, "panic", p)
			s.failReincarnation(ctx, agentID, reincarnationID, fmt.Sprintf("worker panic: %v", p))
		}
	}()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		s.agentLifecycleLog.Error("reincarnation worker: failed to load agent",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "error", err)
		return
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "no dispatcher available")
		return
	}

	// Step: stop. Tolerate a stop failure (already-stopped is a common,
	// harmless case) rather than aborting the whole migration over it —
	// reprovision and start below will surface a real problem regardless.
	agent.ReincarnationState = store.ReincarnationStateStopping
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		s.agentLifecycleLog.Warn("reincarnation worker: failed to record stopping state",
			"agent_id", agentID, "error", err)
	}
	if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
		s.agentLifecycleLog.Warn("reincarnation worker: stop failed, continuing",
			"agent_id", agentID, "error", err)
	}

	// Step: write the new AppliedConfig. The Task is replaced by the hub-built
	// preamble plus handoff — this is the new generation's first harness
	// input (AC-3), delivered as the task argument to DispatchAgentStart below
	// and also persisted onto AppliedConfig.Task for restart consistency.
	toGeneration := agent.Generation + 1
	preamble := s.buildReincarnationPreamble(agent, toGeneration, handoff)
	fresh.Task = preamble
	agent.AppliedConfig = fresh
	agent.ReincarnationState = store.ReincarnationStateProvisioning
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "failed to persist new applied config: "+err.Error())
		return
	}

	// Step: reprovision (re-render scion-agent.json/agent-info.json, re-inject
	// skills, preserve home and workspace — design §3.4).
	if err := dispatcher.DispatchAgentReprovision(ctx, agent); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "reprovision failed: "+err.Error())
		return
	}

	// Step: start, without the harness resume flag (design §3.6, decision D3
	// — always a fresh session).
	agent.ReincarnationState = store.ReincarnationStateStarting
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		s.agentLifecycleLog.Warn("reincarnation worker: failed to record starting state",
			"agent_id", agentID, "error", err)
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, preamble, false); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "start failed: "+err.Error())
		return
	}

	// Step: complete. generation++ and reincarnation_state clears (AC-1).
	agent.Generation = toGeneration
	agent.ReincarnationState = store.ReincarnationStateNone
	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		// The new generation is already running at this point — do not mark
		// the reincarnation failed over a bookkeeping write. Log loudly so an
		// operator can reconcile agents.generation/reincarnation_state by
		// hand if this persistently fails to land.
		s.agentLifecycleLog.Error("reincarnation worker: agent started on new generation but failed to persist completion",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "target_generation", toGeneration, "error", err)
	}

	rec, err := s.store.GetAgentReincarnation(ctx, reincarnationID)
	if err != nil {
		s.agentLifecycleLog.Error("reincarnation worker: failed to load record to mark it completed",
			"reincarnation_id", reincarnationID, "error", err)
		return
	}
	now := time.Now()
	rec.State = store.AgentReincarnationStateCompleted
	rec.CompletedAt = &now
	rec.NewAppliedConfig = fresh
	if err := s.store.UpdateAgentReincarnation(ctx, rec); err != nil {
		s.agentLifecycleLog.Error("reincarnation worker: failed to persist completion record",
			"reincarnation_id", reincarnationID, "error", err)
	}

	s.agentLifecycleLog.Info("reincarnation completed",
		"agent_id", agentID, "reincarnation_id", reincarnationID, "generation", toGeneration)
}

// failReincarnation records a reincarnation failure (design §3.7): the
// AgentReincarnation record is marked failed with the error, the agent is
// left with reincarnation_state=failed and phase=error, and the requester and
// the agent's creator are notified over the existing notification path
// (PublishAgentStatus — the same mechanism any other phase=error transition
// uses; reincarnate does not add a bespoke notification channel for this).
// The previous config snapshot and the handoff remain retrievable on the
// AgentReincarnation record for a subsequent --rollback (Phase 3).
func (s *Server) failReincarnation(ctx context.Context, agentID, reincarnationID, errMsg string) {
	s.agentLifecycleLog.Error("reincarnation failed",
		"agent_id", agentID, "reincarnation_id", reincarnationID, "error", errMsg)

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		s.agentLifecycleLog.Error("failReincarnation: failed to load agent",
			"agent_id", agentID, "error", err)
	} else {
		agent.ReincarnationState = store.ReincarnationStateFailed
		agent.Phase = "error"
		agent.Message = "reincarnation failed: " + errMsg
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			s.agentLifecycleLog.Error("failReincarnation: failed to update agent",
				"agent_id", agentID, "error", err)
		} else if s.events != nil {
			s.events.PublishAgentStatus(ctx, agent)
		}
	}

	rec, err := s.store.GetAgentReincarnation(ctx, reincarnationID)
	if err != nil {
		s.agentLifecycleLog.Error("failReincarnation: failed to load record",
			"reincarnation_id", reincarnationID, "error", err)
		return
	}
	rec.State = store.AgentReincarnationStateFailed
	rec.Error = errMsg
	if err := s.store.UpdateAgentReincarnation(ctx, rec); err != nil {
		s.agentLifecycleLog.Error("failReincarnation: failed to update record",
			"reincarnation_id", reincarnationID, "error", err)
	}
}

// buildReincarnationPreamble builds the hub-authored instructions delivered
// as the new generation's first harness input (design §3.9). Phase 1 ships
// the wording the design marks as "fine for P1" — the fuller agent-guidance
// text (help output, --handoff-template, the self-mode blocked status) is
// Phase 2.
func (s *Server) buildReincarnationPreamble(agent *store.Agent, toGeneration int, handoff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[SCION REINCARNATION] You are generation %d of agent %q (id %s).\n",
		toGeneration, agent.Slug, agent.ID)
	b.WriteString("Before resuming:\n")
	b.WriteString(" 1. Verify your environment: `git status` shows your branch up to date with the remote, and any files your handoff names as canonical are readable.\n")
	b.WriteString(" 2. Catch up on your conversations (`scion conversation catch-up`); messages may have arrived while you were being reincarnated.\n")
	b.WriteString(" 3. Message whoever requested this migration that the new generation is up, and state your next action.\n")
	b.WriteString(" 4. Continue from the handoff below. Do not redo anything it says not to.\n")
	b.WriteString("The handoff from your previous generation follows.\n---\n")
	if handoff != "" {
		b.WriteString(handoff)
	} else {
		b.WriteString("No handoff was provided. Reconstruct context from your branch, your conversations " +
			"(`scion conversation list`), and any project scratchpad before acting.")
	}
	return b.String()
}
