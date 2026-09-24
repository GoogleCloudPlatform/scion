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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reincarnationStepMaxAttempts bounds the merge-and-retry loop in
// updateReincarnationStep: one retry recovers the common case (a single
// concurrent status report bumped state_version while the worker was
// mid-dispatch), matching updateAgentAfterDispatch's convention elsewhere in
// the package. The completion write gets a couple of extra attempts (passed
// explicitly) since losing it is worse: the agent is already running on the
// new generation, and generation/reincarnation_state end up wrong on the
// stored row (AC-1).
const reincarnationStepMaxAttempts = 2

// reincarnationStepUpdate lists the only Agent fields the reincarnation
// worker may write. Every call to updateReincarnationStep re-reads the
// CURRENT row and applies just these fields on top of it (p1b-r1 R2): a
// concurrent status report goes through UpdateAgentStatus, which does not
// bump state_version, so a stale full-row UpdateAgent from this worker could
// otherwise silently overwrite Phase/Activity/ContainerStatus with
// pre-dispatch values with no conflict ever being detected.
type reincarnationStepUpdate struct {
	reincarnationState string
	phase              string                    // "" = leave Phase untouched
	appliedConfig      *store.AgentAppliedConfig // nil = leave untouched
	generation         *int                      // nil = leave untouched
	message            *string                   // nil = leave untouched
}

// updateReincarnationStep re-reads the agent and writes back reincarnation-
// owned fields only, retrying on a version conflict up to maxAttempts times.
// It returns the agent row as written, for the caller to pass to the next
// dispatcher call.
func (s *Server) updateReincarnationStep(ctx context.Context, agentID string, upd reincarnationStepUpdate, maxAttempts int) (*store.Agent, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil {
			return nil, err
		}
		agent.ReincarnationState = upd.reincarnationState
		if upd.phase != "" {
			agent.Phase = upd.phase
		}
		if upd.appliedConfig != nil {
			agent.AppliedConfig = upd.appliedConfig
		}
		if upd.generation != nil {
			agent.Generation = *upd.generation
		}
		if upd.message != nil {
			agent.Message = *upd.message
		}
		if err := s.store.UpdateAgent(ctx, agent); err != nil {
			if errors.Is(err, store.ErrVersionConflict) {
				lastErr = err
				continue
			}
			return nil, err
		}
		return agent, nil
	}
	return nil, lastErr
}

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
//
// Each step re-reads the agent and writes back only the fields this worker
// owns (see updateReincarnationStep) and moves Phase through stopping,
// provisioning and starting as it goes (p1b-r1 R2) — mirroring what the
// synchronous stop/start handlers do, so a message sender or a status reader
// mid-migration sees an accurate phase instead of a stale "running".
func (s *Server) runReincarnationWorker(ctx context.Context, agentID, reincarnationID string, fresh *store.AgentAppliedConfig, handoff string) {
	defer func() {
		if p := recover(); p != nil {
			s.agentLifecycleLog.Error("reincarnation worker panicked",
				"agent_id", agentID, "reincarnation_id", reincarnationID, "panic", p)
			s.failReincarnation(ctx, agentID, reincarnationID, fmt.Sprintf("worker panic: %v", p))
		}
	}()

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "no dispatcher available")
		return
	}

	// Step: stop. updateReincarnationStep's own GetAgent is this worker's
	// first read of the agent — no separate upfront fetch is needed, since
	// nothing before this point reads the agent either.
	agent, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStopping,
		phase:              string(state.PhaseStopping),
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "failed to record stopping state: "+err.Error())
		return
	}

	// p1b-r1 R3: a stop failure is fatal, checked BEFORE any config write.
	// The broker itself already treats "already stopped" and "not found" as
	// success (runtimebroker handlers.go stopAgent), so any error returned
	// here is a genuine failure — broker unreachable, a real runtime error,
	// or a stop that timed out — and continuing past it would re-render
	// config and dispatch a fresh session under a container that is still
	// running the old generation.
	if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "stop failed: "+err.Error())
		return
	}

	// Step: write the new AppliedConfig. The Task is replaced by the hub-built
	// preamble plus handoff — this is the new generation's first harness
	// input (AC-3), delivered as the task argument to DispatchAgentStart below
	// and also persisted onto AppliedConfig.Task for restart consistency.
	toGeneration := agent.Generation + 1
	preamble := s.buildReincarnationPreamble(agent, toGeneration, handoff)
	fresh.Task = preamble
	agent, err = s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateProvisioning,
		phase:              string(state.PhaseProvisioning),
		appliedConfig:      fresh,
	}, reincarnationStepMaxAttempts)
	if err != nil {
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
	agent, err = s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStarting,
		phase:              string(state.PhaseStarting),
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "failed to record starting state: "+err.Error())
		return
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, preamble, false); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "start failed: "+err.Error())
		return
	}

	// Step: complete. generation++ and reincarnation_state clears (AC-1).
	// Phase is deliberately left at "starting" here — exactly like a normal
	// start dispatch (see wake_dm.go), the container's own status report
	// moves it to "running"; the worker forcing that value would be a lie if
	// the container is still booting when this write lands. A few extra
	// retry attempts: losing this write leaves the row saying "starting"
	// forever even though the new generation is live, and generation stuck
	// at N even though gen N+1 is what is actually running.
	if _, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateNone,
		generation:         &toGeneration,
	}, reincarnationStepMaxAttempts+3); err != nil {
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
// AgentReincarnation record is marked failed with the error, and the agent is
// left with reincarnation_state=failed and phase=error. The previous config
// snapshot and the handoff remain retrievable on the AgentReincarnation
// record for a subsequent --rollback (Phase 3).
//
// Notification (design §3.4 Amendment A3.8, p1b-r1 N1): both the requester
// and the creator must learn of a failure, and both do, through the existing
// PublishAgentStatus subscription path — the same mechanism any other
// phase=error transition uses. The creator is already subscribed from
// create. handleReincarnateAgent's ensureReincarnateRequesterSubscribed
// subscribes the requester too, at request time, if they are not the agent
// itself and not already subscribed — the same createNotifySubscription
// mechanism create's --notify flag uses, so no new delivery path is needed.
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

// sweepStaleReincarnations marks every non-terminal reincarnation record —
// and its agent's reincarnation_state — failed, at hub startup (design §3.7
// F4, p1b-r1). A non-terminal record can only survive to the next boot if
// the hub restarted mid-flight: runReincarnationWorker's own detached
// goroutine died with the process, and R1's claim-then-create order means a
// merely-failed request never leaves an orphan behind. Without this sweep
// such an agent is stuck behind AC-8's 409 forever, with no API to clear it.
//
// This is a stop-gap, not resume: Phase 3 owns actually completing an
// interrupted reincarnation from where it left off. Marking it failed here
// is honest about what happened (the hub does not know how far the worker
// got) and unblocks the agent for a fresh `scion reincarnate` retry.
func (s *Server) sweepStaleReincarnations(ctx context.Context) (int, error) {
	stale, err := s.store.ListNonTerminalAgentReincarnations(ctx)
	if err != nil {
		return 0, fmt.Errorf("list non-terminal reincarnations: %w", err)
	}

	const reason = "hub restarted during reincarnation"
	for _, rec := range stale {
		// Reuse failReincarnation: it already does exactly this pair of
		// writes (mark the record failed, mark the agent's
		// reincarnation_state failed/phase=error) for the worker's own
		// failure path, best-effort logging its own errors rather than
		// aborting the sweep over one bad row.
		s.failReincarnation(ctx, rec.AgentID, rec.ID, reason)
	}
	return len(stale), nil
}
