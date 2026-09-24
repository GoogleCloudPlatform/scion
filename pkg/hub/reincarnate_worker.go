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
// the package. The completion write and failReincarnation get a couple of
// extra attempts (passed explicitly) since losing either is worse: for
// completion, the agent is already running on the new generation and
// generation/reincarnation_state end up wrong on the stored row (AC-1); for
// failure, a lost write wedges the agent behind a permanent 409 with no
// worker left to retry it.
const reincarnationStepMaxAttempts = 2

// reincarnationStepUpdate lists the only Agent fields the reincarnation
// worker may write. Every call to updateReincarnationStep re-reads the
// CURRENT row and applies just these fields on top of it (design §3.3): a
// concurrent status report goes through UpdateAgentStatus, which does not
// bump state_version, so a stale full-row UpdateAgent from this worker could
// otherwise silently overwrite Phase/Activity/ContainerStatus with
// pre-dispatch values with no conflict ever being detected.
type reincarnationStepUpdate struct {
	reincarnationState string
	phase              string                    // "" = leave Phase untouched
	activity           *string                   // nil = leave untouched, non-nil (incl. "") = set
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
		if upd.activity != nil {
			agent.Activity = *upd.activity
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

// reincarnateStrPtr is a small helper for populating
// reincarnationStepUpdate.activity and .message, both of which distinguish
// "leave untouched" (nil) from "set to this value, including empty string"
// (non-nil).
func reincarnateStrPtr(s string) *string { return &s }

// reincarnationRecordTerminal reports whether the reincarnation record is
// already completed or failed. The worker checks this before every
// state-transition write (design §3.7): the replica-safe sweep can mark a
// record failed out from under a worker that is still genuinely running
// (the sweep only touches records past the staleness bound, but a very slow
// step or a paused process could still race it), and without this check the
// worker would plow ahead and eventually overwrite the sweep's "failed" with
// its own "completed" — the exact audit-trail contradiction that motivated
// this check. A read error is treated as "not terminal" (proceed): getting
// stuck on a transient store error is worse than the rare case this exists
// to prevent.
func (s *Server) reincarnationRecordTerminal(ctx context.Context, reincarnationID string) bool {
	rec, err := s.store.GetAgentReincarnation(ctx, reincarnationID)
	if err != nil {
		return false
	}
	return rec.State == store.AgentReincarnationStateCompleted || rec.State == store.AgentReincarnationStateFailed
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
// "Task | Replaced") and persists it. previous is the outgoing generation's
// AppliedConfig (the record's PreviousAppliedConfig): if the reprovision
// dispatch never succeeds, failReincarnation restores it, so the store does
// not claim gen N+1 while the disk still holds gen N (design §3.7).
//
// Each step re-reads the agent and writes back only the fields this worker
// owns (see updateReincarnationStep) and moves Phase through stopping,
// provisioning and starting as it goes — mirroring what the synchronous
// stop/start handlers do, so a message sender or a status reader
// mid-migration sees an accurate phase instead of a stale "running".
func (s *Server) runReincarnationWorker(ctx context.Context, agentID, reincarnationID string, previous, fresh *store.AgentAppliedConfig, handoff string) {
	defer func() {
		if p := recover(); p != nil {
			s.agentLifecycleLog.Error("reincarnation worker panicked",
				"agent_id", agentID, "reincarnation_id", reincarnationID, "panic", p)
			// Unknown how far the worker got, so it is not safe to guess
			// whether reprovision already succeeded; leave AppliedConfig as
			// it stands rather than risk restoring over a real gen N+1.
			s.failReincarnation(ctx, agentID, reincarnationID, fmt.Sprintf("worker panic: %v", p), nil)
		}
	}()

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "no dispatcher available", previous)
		return
	}

	if s.reincarnationRecordTerminal(ctx, reincarnationID) {
		s.agentLifecycleLog.Warn("reincarnation worker: record already terminal before the stop step, aborting without writing",
			"agent_id", agentID, "reincarnation_id", reincarnationID)
		return
	}
	// Step: stop. updateReincarnationStep's own GetAgent is this worker's
	// first read of the agent — no separate upfront fetch is needed, since
	// nothing before this point reads the agent either. Activity is cleared
	// here, mirroring the synchronous stop handler: a stale "working"/
	// "waiting_for_input" left over from before the migration would
	// otherwise either suppress the ERROR notification on failure (Activity
	// takes precedence over Phase when matching subscriptions) or dispatch a
	// misleading one.
	agent, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStopping,
		phase:              string(state.PhaseStopping),
		activity:           reincarnateStrPtr(""),
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "failed to record stopping state: "+err.Error(), previous)
		return
	}

	// A stop failure is fatal, checked BEFORE any config write. The broker
	// itself already treats "already stopped" and "not found" as success
	// (runtimebroker handlers.go stopAgent), so any error returned here is a
	// genuine failure — broker unreachable, a real runtime error, or a stop
	// that timed out — and continuing past it would re-render config and
	// dispatch a fresh session under a container that is still running the
	// old generation.
	if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "stop failed: "+err.Error(), previous)
		return
	}

	if s.reincarnationRecordTerminal(ctx, reincarnationID) {
		s.agentLifecycleLog.Warn("reincarnation worker: record already terminal before the provisioning step, aborting without writing",
			"agent_id", agentID, "reincarnation_id", reincarnationID)
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
		// The write itself failed, so the store still holds `previous`
		// (this call is a no-op restore, kept for consistency with every
		// other pre-reprovision failure site).
		s.failReincarnation(ctx, agentID, reincarnationID, "failed to persist new applied config: "+err.Error(), previous)
		return
	}

	// Step: reprovision (re-render scion-agent.json/agent-info.json, re-inject
	// skills, preserve home and workspace — design §3.4).
	if err := dispatcher.DispatchAgentReprovision(ctx, agent); err != nil {
		// The store now holds `fresh` (gen N+1) from the write just above,
		// but the disk was never successfully re-rendered — restore
		// `previous` so the store does not claim a generation that was
		// never actually provisioned.
		s.failReincarnation(ctx, agentID, reincarnationID, "reprovision failed: "+err.Error(), previous)
		return
	}

	if s.reincarnationRecordTerminal(ctx, reincarnationID) {
		s.agentLifecycleLog.Warn("reincarnation worker: record already terminal before the starting step, aborting without writing",
			"agent_id", agentID, "reincarnation_id", reincarnationID)
		return
	}
	// Step: start, without the harness resume flag (design §3.6, decision D3
	// — always a fresh session). Reprovision already succeeded, so the disk
	// now holds gen N+1; a failure from here on must NOT restore `previous`,
	// since that would make the store claim gen N while the disk (and any
	// container the start call did manage to create) is gen N+1.
	agent, err = s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStarting,
		phase:              string(state.PhaseStarting),
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "failed to record starting state: "+err.Error(), nil)
		return
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, preamble, false); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, "start failed: "+err.Error(), nil)
		return
	}

	if s.reincarnationRecordTerminal(ctx, reincarnationID) {
		s.agentLifecycleLog.Warn("reincarnation worker: record already terminal before the completion write; leaving it alone",
			"agent_id", agentID, "reincarnation_id", reincarnationID)
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
// left with reincarnation_state=failed, phase=error, and Activity cleared
// (mirroring the synchronous stop handler). The previous config snapshot and
// the handoff remain retrievable on the AgentReincarnation record for a
// subsequent --rollback (Phase 3). The agent write goes through
// updateReincarnationStep, the same owned-field merge-and-retry every other
// worker step uses, so a concurrent status report cannot be clobbered and a
// version conflict does not silently wedge the agent.
//
// Notification (design §3.4 Amendment A3): both the requester and the
// creator must learn of a failure, and both do, through the existing
// PublishAgentStatus subscription path — the same mechanism any other
// phase=error transition uses. The creator is already subscribed from
// create. handleReincarnateAgent's ensureReincarnateRequesterSubscribed
// subscribes the requester too, at request time, if they are not the agent
// itself and not already subscribed — the same createNotifySubscription
// mechanism create's --notify flag uses, so no new delivery path is needed.
// PublishAgentStatus only fires when the agent write actually succeeds —
// publishing a stale in-memory agent after a failed write would report a
// phase the store never actually reached.
//
// restoreConfig, when non-nil, replaces AppliedConfig with it (design §3.7):
// pass the outgoing generation's config when the failure happened before a
// successful reprovision dispatch, so the store does not claim gen N+1 while
// the disk still holds gen N. Pass nil once reprovision has succeeded — at
// that point the disk really does hold gen N+1, and restoring would create
// the opposite mismatch.
func (s *Server) failReincarnation(ctx context.Context, agentID, reincarnationID, errMsg string, restoreConfig *store.AgentAppliedConfig) {
	s.agentLifecycleLog.Error("reincarnation failed",
		"agent_id", agentID, "reincarnation_id", reincarnationID, "error", errMsg)

	upd := reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateFailed,
		phase:              "error",
		activity:           reincarnateStrPtr(""),
		message:            reincarnateStrPtr("reincarnation failed: " + errMsg),
	}
	if restoreConfig != nil {
		upd.appliedConfig = restoreConfig
	}
	// Extra attempts, same reasoning as the completion write: a lost write
	// here wedges the agent behind a permanent 409 with no worker left to
	// retry it, and the boot/periodic sweep only clears non-terminal
	// records/agent-states — an agent stuck mid-transition because this
	// write never landed would not even match that backstop.
	agent, err := s.updateReincarnationStep(ctx, agentID, upd, reincarnationStepMaxAttempts+3)
	if err != nil {
		s.agentLifecycleLog.Error("failReincarnation: failed to update agent after retries",
			"agent_id", agentID, "error", err)
	} else if s.events != nil {
		s.events.PublishAgentStatus(ctx, agent)
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

// reincarnationStaleAfter bounds how long a non-terminal reincarnation
// record or agent reincarnation_state may go without a write before the
// sweep considers it abandoned rather than genuinely in-flight (design
// §3.7). Comfortably above worst-case worker duration (a handful of
// dispatcher round trips), so a healthy migration on one replica is never
// touched by a sweep running concurrently on another.
const reincarnationStaleAfter = 30 * time.Minute

// sweepStaleReincarnations is the replica-safe sweep (design §3.7): it marks
// every non-terminal reincarnation record whose updated_at is older than
// reincarnationStaleAfter as failed, plus — as a backstop — any agent whose
// reincarnation_state is non-terminal and whose own row is equally stale but
// has no matching non-terminal record for the first half to find. It runs at
// hub boot and periodically (server.go registers it with the scheduler as a
// singleton job). The time bound is what makes both call sites safe to run
// on every replica independently, with no distributed lock needed to decide
// which replica "owns" a given migration: a genuinely in-flight worker keeps
// bumping its record's and its agent's updated_at faster than the bound, on
// whichever replica is actually running it, and runReincarnationWorker
// itself re-checks the record before every step so it aborts rather than
// resurrecting one the sweep already failed.
//
// This is a stop-gap, not resume: Phase 3 owns actually completing an
// interrupted reincarnation from where it left off. Marking it failed here
// is honest about what happened (the hub does not know how far the worker
// got) and unblocks the agent for a fresh `scion reincarnate` retry.
func (s *Server) sweepStaleReincarnations(ctx context.Context) (int, error) {
	return s.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(-reincarnationStaleAfter))
}

// sweepStaleReincarnationsOlderThan is sweepStaleReincarnations with the
// cutoff exposed, so tests can exercise the marking logic without an actual
// 30-minute-old row (e.g. a cutoff in the future treats every existing row as
// stale). Production code should call sweepStaleReincarnations.
func (s *Server) sweepStaleReincarnationsOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	stale, err := s.store.ListStaleNonTerminalAgentReincarnations(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("list stale non-terminal reincarnations: %w", err)
	}
	const reason = "hub restarted during reincarnation"
	for _, rec := range stale {
		// Conservative: the sweep cannot know whether reprovision succeeded
		// before whatever replica owned this record went away, so it
		// restores the outgoing generation's config rather than risk
		// leaving the store claiming a gen N+1 that may never have been
		// rendered to disk. A genuinely running gen N+1 self-corrects on its
		// next status report; this is a stop-gap, not a guarantee.
		s.failReincarnation(ctx, rec.AgentID, rec.ID, reason, rec.PreviousAppliedConfig)
	}

	orphans, err := s.store.ListAgentsWithStaleNonTerminalReincarnationState(ctx, cutoff)
	if err != nil {
		return len(stale), fmt.Errorf("list stale non-terminal agent reincarnation states: %w", err)
	}
	swept := len(stale)
	for _, orphan := range orphans {
		if _, err := s.updateReincarnationStep(ctx, orphan.ID, reincarnationStepUpdate{
			reincarnationState: store.ReincarnationStateFailed,
			phase:              "error",
			activity:           reincarnateStrPtr(""),
			message:            reincarnateStrPtr("reincarnation failed: " + reason + " (no matching reincarnation record found)"),
		}, reincarnationStepMaxAttempts); err != nil {
			s.agentLifecycleLog.Warn("boot sweep: failed to reset orphaned agent reincarnation state",
				"agent_id", orphan.ID, "error", err)
			continue
		}
		swept++
	}
	return swept, nil
}

// reincarnationSweepHandler adapts sweepStaleReincarnations to the
// scheduler's recurring-job signature, for the periodic (every 5 minutes)
// half of the replica-safe sweep (design §3.7) — the boot-time call in
// NewServer covers a restart, this covers a worker that died without one
// (e.g. the process was killed rather than gracefully stopped).
func (s *Server) reincarnationSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		if n, err := s.sweepStaleReincarnations(ctx); err != nil {
			s.agentLifecycleLog.Warn("periodic sweep: failed to sweep stale reincarnations", "error", err)
		} else if n > 0 {
			s.agentLifecycleLog.Info("periodic sweep: marked stale reincarnations failed", "count", n)
		}
	}
}
