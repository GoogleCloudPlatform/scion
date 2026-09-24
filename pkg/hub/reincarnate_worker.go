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
	// now, when non-zero, pins ReincarnationUpdatedAt to this exact instant
	// instead of a freshly computed time.Now(). tryAdvanceReincarnation's
	// callers pass the same instant they just stamped on the corresponding
	// record-side write, so the two clocks the replica-safe sweep reads
	// (design §3.4 Amendment A6.6) are never observably out of order for the
	// same step, regardless of which of the two writes physically commits
	// first. Zero means "compute internally" (used by writes with no
	// paired record-side CAS, e.g. the agent-state backstop's reset).
	now time.Time
}

// updateReincarnationStep re-reads the agent and writes back reincarnation-
// owned fields only, retrying on a version conflict up to maxAttempts times.
// It returns the agent row as written, for the caller to pass to the next
// dispatcher call. Every call bumps ReincarnationUpdatedAt (design §3.4
// Amendment A6.6): this is the only place a worker step or a
// failure/completion write touches the agent row, so it is exactly the set
// of writes that clock is meant to track — unlike Updated, which broker
// heartbeats bump too, and would otherwise hide a genuinely stuck agent from
// the replica-safe sweep's backstop.
func (s *Server) updateReincarnationStep(ctx context.Context, agentID string, upd reincarnationStepUpdate, maxAttempts int) (*store.Agent, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil {
			return nil, err
		}
		agent.ReincarnationState = upd.reincarnationState
		stepNow := upd.now
		if stepNow.IsZero() {
			stepNow = time.Now()
		}
		agent.ReincarnationUpdatedAt = &stepNow
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

// tryAdvanceReincarnation is the record-side half of every step and terminal
// transition the WORKER makes (as opposed to the sweep, which uses
// advanceListedRecord below). The caller supplies fromState EXPLICITLY — the
// exact step the worker KNOWS it is leaving (pending→stopping expects
// "pending", …, starting→completed expects "starting"; a failure expects
// whatever step it is failing out of) — rather than a value read fresh off
// the record (design §3.4 Amendment A8.1). A fresh read cannot be trusted as
// expectState: it can return an already-terminal value (the replica-safe
// sweep, or a racing completion, resolved the record while this call was
// stalled), and a CAS "WHERE state = 'failed'" against an already-'failed'
// row would trivially match itself, letting a stale caller "win" a race it
// actually lost. Knowing fromState up front also means there is no read to
// race against the sweep in the first place: the check and the write are
// the same conditional UPDATE.
//
// It sets the record's State to newState plus whatever mutate applies
// (Error, CompletedAt, NewAppliedConfig), and CASes the write through
// store.TryAdvanceAgentReincarnation. It returns the instant it stamped on
// the record (zero if it never got that far) alongside the CAS result, so
// the caller can pin the SAME instant onto the paired agent-row write
// (reincarnationStepUpdate.now) — the two writes are ordered (this one
// always happens first), but the sweep's two staleness clocks should never
// look out of order relative to each other for the same step just because
// of which write physically landed first.
//
// maxAttempts retries a CAS ERROR — a transient DB failure — up to
// maxAttempts times (design §3.4 Amendment A8.2): reincarnationStepMaxAttempts
// for a step transition, reincarnationStepMaxAttempts+3 for completion or
// failure, matching updateReincarnationStep's convention. A CLEAN loss of
// the race (ok=false, err=nil) is never retried — retrying it would just
// observe the same lost race again — but after a retry that ITSELF returns
// (false, nil), the record is re-read once to disambiguate two situations
// that look identical from the CAS return value alone: "someone else owns
// it now" vs. "an earlier attempt's write actually landed, and only the
// error reporting that back was lost" (a network timeout after commit, for
// example). If the record's current State already equals newState, this
// call is the only writer that could have put it there — a reincarnation ID
// has exactly one live worker, and the sweep never sets a forward-step
// state — so it is treated as a win rather than a loss.
//
// The three-way result distinguishes two very different outcomes a caller
// must not confuse (design §3.4 Amendment A7):
//   - (ok=false, err=nil): a clean loss of the CAS race — this worker no
//     longer owns the record (something else, most likely the replica-safe
//     sweep, already moved it). The caller MUST return immediately without
//     writing the agent row, and must NOT treat this as a failure to report.
//   - (ok=false, err!=nil): a persistent CAS error, after retrying.
//     Silently swallowing this (as an earlier round did, by treating every
//     error the same as losing the race) can leave the agent stuck
//     mid-transition with no worker left to retry it and nothing for the
//     replica-safe sweep to see as non-terminal-but-stale on the RECORD side
//     (it still has a stale agent-state backstop, but that takes the full
//     30-minute bound). A step-transition caller must run the normal
//     failure path instead of returning silently; a completion-transition
//     caller must not, since the agent is already running gen N+1 and
//     failing would restore the wrong config onto it (see
//     runReincarnationWorker's completion step).
func (s *Server) tryAdvanceReincarnation(ctx context.Context, reincarnationID, fromState, newState string, maxAttempts int, mutate func(rec *store.AgentReincarnation, now time.Time)) (time.Time, bool, error) {
	var lastErr error
	sawError := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		now := time.Now()
		rec := &store.AgentReincarnation{ID: reincarnationID, State: newState, UpdatedAt: now}
		if mutate != nil {
			mutate(rec, now)
		}
		ok, err := s.store.TryAdvanceAgentReincarnation(ctx, rec, fromState)
		if err != nil {
			lastErr = err
			sawError = true
			s.agentLifecycleLog.Warn("reincarnation worker: failed to advance record, retrying",
				"reincarnation_id", reincarnationID, "from_state", fromState, "target_state", newState, "attempt", attempt, "error", err)
			continue
		}
		if !ok && sawError {
			// This attempt's CAS cleanly found 0 rows, but an EARLIER attempt
			// on this same call errored — that earlier write might have
			// actually landed, with only the error report lost afterward.
			// Re-read to tell the two cases apart before concluding this
			// worker lost the record to someone else.
			if cur, rerr := s.store.GetAgentReincarnation(ctx, reincarnationID); rerr == nil && cur.State == newState {
				s.agentLifecycleLog.Info("reincarnation worker: an earlier errored attempt's write had actually landed, treating it as owned",
					"reincarnation_id", reincarnationID, "target_state", newState)
				return cur.UpdatedAt, true, nil
			}
		}
		if !ok {
			s.agentLifecycleLog.Warn("reincarnation worker: record no longer in the expected state, aborting without writing the agent row",
				"reincarnation_id", reincarnationID, "from_state", fromState, "target_state", newState)
		}
		return now, ok, nil
	}
	s.agentLifecycleLog.Error("reincarnation worker: failed to advance record after retries",
		"reincarnation_id", reincarnationID, "from_state", fromState, "target_state", newState, "attempts", maxAttempts, "error", lastErr)
	return time.Time{}, false, lastErr
}

// tryAdvanceReincarnationUnknownState is tryAdvanceReincarnation's fallback
// for the one caller that cannot know fromState: panic recovery, which has
// no reliable way to tell which step the worker was on when it panicked. It
// re-reads the record, and proceeds only if the freshly-read State is
// non-terminal (store.IsAgentReincarnationStateNonTerminal) — for the exact
// reason tryAdvanceReincarnation itself no longer trusts a fresh read as
// expectState (design §3.4 Amendment A8.1): passing an already-terminal
// value through would let this call trivially "win" a race it actually
// lost. Once confirmed non-terminal, it delegates to tryAdvanceReincarnation
// with that value as fromState.
func (s *Server) tryAdvanceReincarnationUnknownState(ctx context.Context, reincarnationID, newState string, maxAttempts int, mutate func(rec *store.AgentReincarnation, now time.Time)) (time.Time, bool, error) {
	rec, err := s.store.GetAgentReincarnation(ctx, reincarnationID)
	if err != nil {
		s.agentLifecycleLog.Error("reincarnation worker: failed to read record before advancing it from an unknown state",
			"reincarnation_id", reincarnationID, "target_state", newState, "error", err)
		return time.Time{}, false, err
	}
	if !store.IsAgentReincarnationStateNonTerminal(rec.State) {
		s.agentLifecycleLog.Warn("reincarnation worker: record already terminal, aborting without writing the agent row",
			"reincarnation_id", reincarnationID, "current_state", rec.State, "target_state", newState)
		return time.Time{}, false, nil
	}
	return s.tryAdvanceReincarnation(ctx, reincarnationID, rec.State, newState, maxAttempts, mutate)
}

// advanceListedRecord is the sweep's counterpart to tryAdvanceReincarnation:
// it CASes an ALREADY-HELD record (rec, as returned by
// ListStaleNonTerminalAgentReincarnations) rather than re-reading it, using
// expectState = rec.State — the value the sweep observed when it listed the
// record as stale (design §3.4 Amendment A7).
//
// This distinction matters. A fresh read here (mirroring
// tryAdvanceReincarnation) would defeat the whole purpose: it would always
// see "the current state" and the CAS would trivially match it, so a live
// worker that advanced the record after it was listed — including all the
// way to "starting", meaning reprovision already succeeded — would still
// have its record failed and, worse, its gen N+1 config replaced by
// rec.PreviousAppliedConfig from the stale, already-superseded rec. Pinning
// the CAS to the value observed at list time means that if the record has
// moved on at all since then, this call loses the race and does nothing,
// leaving the live worker's own tryAdvanceReincarnation calls — which race
// this one fairly, on the same expectState semantics — to decide the
// record's fate instead.
func (s *Server) advanceListedRecord(ctx context.Context, rec *store.AgentReincarnation, newState string, mutate func(r *store.AgentReincarnation, now time.Time)) (time.Time, bool, error) {
	expectState := rec.State
	upd := *rec // shallow copy: never mutate the caller's (possibly reused) rec
	now := time.Now()
	upd.State = newState
	upd.UpdatedAt = now
	if mutate != nil {
		mutate(&upd, now)
	}
	ok, err := s.store.TryAdvanceAgentReincarnation(ctx, &upd, expectState)
	if err != nil {
		s.agentLifecycleLog.Error("sweep: failed to advance a listed record",
			"reincarnation_id", rec.ID, "expect_state", expectState, "target_state", newState, "error", err)
		return now, false, err
	}
	if !ok {
		s.agentLifecycleLog.Info("sweep: record changed since it was listed as stale, leaving it alone",
			"reincarnation_id", rec.ID, "expect_state", expectState, "target_state", newState)
	}
	return now, ok, nil
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
			// Unknown how far the worker got, so fromState is unknowable too —
			// tryAdvanceReincarnationUnknownState reads the record and only
			// proceeds if it is still non-terminal. Leave AppliedConfig as it
			// stands rather than risk restoring over a real gen N+1.
			s.failReincarnationUnknownState(ctx, agentID, reincarnationID, fmt.Sprintf("worker panic: %v", p), nil)
		}
	}()

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStatePending, "no dispatcher available", previous)
		return
	}

	stoppingNow, ok, err := s.tryAdvanceReincarnation(ctx, reincarnationID, store.AgentReincarnationStatePending, store.AgentReincarnationStateStopping, reincarnationStepMaxAttempts, nil)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStatePending, "failed to advance record to stopping: "+err.Error(), previous)
		return
	}
	if !ok {
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
		now:                stoppingNow,
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateStopping, "failed to record stopping state: "+err.Error(), previous)
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
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateStopping, "stop failed: "+err.Error(), previous)
		return
	}

	provisioningNow, ok, err := s.tryAdvanceReincarnation(ctx, reincarnationID, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, reincarnationStepMaxAttempts, nil)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateStopping, "failed to advance record to provisioning: "+err.Error(), previous)
		return
	}
	if !ok {
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
		now:                provisioningNow,
	}, reincarnationStepMaxAttempts)
	if err != nil {
		// The write itself failed, so the store still holds `previous`
		// (this call is a no-op restore, kept for consistency with every
		// other pre-reprovision failure site).
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateProvisioning, "failed to persist new applied config: "+err.Error(), previous)
		return
	}

	// Step: reprovision (re-render scion-agent.json/agent-info.json, re-inject
	// skills, preserve home and workspace — design §3.4).
	if err := dispatcher.DispatchAgentReprovision(ctx, agent); err != nil {
		// The store now holds `fresh` (gen N+1) from the write just above,
		// but the disk was never successfully re-rendered — restore
		// `previous` so the store does not claim a generation that was
		// never actually provisioned.
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateProvisioning, "reprovision failed: "+err.Error(), previous)
		return
	}

	startingNow, ok, err := s.tryAdvanceReincarnation(ctx, reincarnationID, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting, reincarnationStepMaxAttempts, nil)
	if err != nil {
		// Reprovision already succeeded, so no restore (same reasoning as the
		// other post-reprovision-success failure sites below).
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateProvisioning, "failed to advance record to starting: "+err.Error(), nil)
		return
	}
	if !ok {
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
		now:                startingNow,
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateStarting, "failed to record starting state: "+err.Error(), nil)
		return
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, preamble, false); err != nil {
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateStarting, "start failed: "+err.Error(), nil)
		return
	}

	// Step: complete. Design §3.4 Amendment A6: CAS the record to
	// completed FIRST. Only the winner may write the agent row — otherwise a
	// worker whose record the sweep already resolved out from under it could
	// still bump generation and clear reincarnation_state after the sweep
	// (or a completing rival) already decided this record's — and the
	// agent's — fate.
	completedNow, ok, err := s.tryAdvanceReincarnation(ctx, reincarnationID, store.AgentReincarnationStateStarting, store.AgentReincarnationStateCompleted, reincarnationStepMaxAttempts+3, func(rec *store.AgentReincarnation, now time.Time) {
		rec.CompletedAt = &now
		rec.NewAppliedConfig = fresh
	})
	if err != nil {
		// The agent is genuinely running gen N+1 at this point; a persistent
		// error CASing the record to completed is a bookkeeping failure, not
		// a reason to mark the reincarnation failed (that would be worse: it
		// would restore gen N onto an agent already running gen N+1). Log
		// loudly, same as the generation/reincarnation_state write below.
		s.agentLifecycleLog.Error("reincarnation worker: agent started on new generation but failed to advance the record to completed",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "target_generation", toGeneration, "error", err)
		return
	}
	if !ok {
		// The dispatcher calls above already succeeded — the agent is
		// genuinely running gen N+1 on disk — but this worker lost the race
		// for its own record (design §3.4 Amendment A5.7 accepted stop-gap:
		// a sweep that lands on a worker that then goes on to succeed leaves
		// the store saying failed/gen N while the agent runs gen N+1; Phase
		// 3 resume removes this window). Do not write the agent row: the
		// sweep, or whatever else won, gets to keep its own account of what
		// happened.
		s.agentLifecycleLog.Warn("reincarnation worker: record no longer non-terminal at completion; agent already started on the new generation but bookkeeping is skipped",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "target_generation", toGeneration)
		return
	}

	// generation++ and reincarnation_state clears (AC-1). Phase is
	// deliberately left at "starting" here — exactly like a normal start
	// dispatch (see wake_dm.go), the container's own status report moves it
	// to "running"; the worker forcing that value would be a lie if the
	// container is still booting when this write lands. A few extra retry
	// attempts: losing this write leaves the row saying "starting" forever
	// even though the new generation is live, and generation stuck at N even
	// though gen N+1 is what is actually running.
	if _, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateNone,
		generation:         &toGeneration,
		now:                completedNow,
	}, reincarnationStepMaxAttempts+3); err != nil {
		// The new generation is already running at this point — do not mark
		// the reincarnation failed over a bookkeeping write. Log loudly so an
		// operator can reconcile agents.generation/reincarnation_state by
		// hand if this persistently fails to land.
		s.agentLifecycleLog.Error("reincarnation worker: agent started on new generation but failed to persist completion",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "target_generation", toGeneration, "error", err)
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
//
// Design §3.4 Amendment A6: the record is CASed to failed FIRST,
// via tryAdvanceReincarnation. Only the winner goes on to write the agent
// row (or restore restoreConfig onto it). Without this, a worker whose
// record the replica-safe sweep already failed out from under it — and
// which is therefore not necessarily the only writer left for this agent —
// could still land its own late failure write after a second, freshly
// admitted reincarnation has claimed (or even completed on) the same agent,
// clobbering that newer claim's state, config and record error with its own
// stale account.
//
// fromState is the exact step the caller KNOWS the worker is failing out of
// (design §3.4 Amendment A8.1) — see tryAdvanceReincarnation's doc comment
// for why this must be explicit rather than read fresh off the record.
func (s *Server) failReincarnation(ctx context.Context, agentID, reincarnationID, fromState, errMsg string, restoreConfig *store.AgentAppliedConfig) {
	s.agentLifecycleLog.Error("reincarnation failed",
		"agent_id", agentID, "reincarnation_id", reincarnationID, "error", errMsg)

	failNow, ok, err := s.tryAdvanceReincarnation(ctx, reincarnationID, fromState, store.AgentReincarnationStateFailed, reincarnationStepMaxAttempts+3, func(rec *store.AgentReincarnation, _ time.Time) {
		rec.Error = errMsg
	})
	s.finishFailReincarnation(ctx, agentID, reincarnationID, errMsg, restoreConfig, failNow, ok, err)
}

// failReincarnationUnknownState is failReincarnation's fallback for panic
// recovery, the one caller that cannot know fromState (design §3.4
// Amendment A8.1): it goes through tryAdvanceReincarnationUnknownState
// instead, which rejects an already-terminal record up front rather than
// trust a fresh read as expectState.
func (s *Server) failReincarnationUnknownState(ctx context.Context, agentID, reincarnationID, errMsg string, restoreConfig *store.AgentAppliedConfig) {
	s.agentLifecycleLog.Error("reincarnation failed",
		"agent_id", agentID, "reincarnation_id", reincarnationID, "error", errMsg)

	failNow, ok, err := s.tryAdvanceReincarnationUnknownState(ctx, reincarnationID, store.AgentReincarnationStateFailed, reincarnationStepMaxAttempts+3, func(rec *store.AgentReincarnation, _ time.Time) {
		rec.Error = errMsg
	})
	s.finishFailReincarnation(ctx, agentID, reincarnationID, errMsg, restoreConfig, failNow, ok, err)
}

// finishFailReincarnation is the shared tail of failReincarnation and
// failReincarnationUnknownState, once the record-side CAS attempt (by
// whichever path) has already run.
func (s *Server) finishFailReincarnation(ctx context.Context, agentID, reincarnationID, errMsg string, restoreConfig *store.AgentAppliedConfig, failNow time.Time, ok bool, err error) {
	if err != nil {
		s.agentLifecycleLog.Error("failReincarnation: failed to advance record to failed after retries, skipping the agent write",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "error", err)
		return
	}
	if !ok {
		s.agentLifecycleLog.Warn("failReincarnation: record no longer owned by this worker, skipping the agent write",
			"agent_id", agentID, "reincarnation_id", reincarnationID)
		return
	}
	s.writeFailedAgent(ctx, agentID, errMsg, restoreConfig, failNow)
}

// writeFailedAgent is the agent-row half shared by failReincarnation (the
// worker's own failure paths) and failListedReincarnation (the sweep):
// reincarnation_state=failed, phase=error, Activity cleared, and
// AppliedConfig replaced with restoreConfig when non-nil (design §3.7). Only
// called after the caller's own record-side CAS has already won — see both
// callers' doc comments for why that ordering is required.
func (s *Server) writeFailedAgent(ctx context.Context, agentID, errMsg string, restoreConfig *store.AgentAppliedConfig, now time.Time) {
	upd := reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateFailed,
		phase:              "error",
		activity:           reincarnateStrPtr(""),
		now:                now,
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
}

// failListedReincarnationRecordOnly CASes an already-listed stale record
// straight to failed without touching the agent row at all (design §3.4
// Amendment A6.5), pinned to the exact state the sweep observed it at (see
// advanceListedRecord) rather than a fresh read. It exists for exactly one
// caller, sweepFailStaleRecord: a stale record whose agent's
// reincarnation_state is already "" or "failed" means something else (a
// completion write, an earlier failure or sweep pass) already resolved the
// agent side of this migration. Writing the agent here would either
// resurrect a stale phase/config on an agent that has already moved on, or
// duplicate a failure another writer already recorded — the record itself
// is the only thing left to reconcile.
func (s *Server) failListedReincarnationRecordOnly(ctx context.Context, rec *store.AgentReincarnation, errMsg string) {
	_, _, _ = s.advanceListedRecord(ctx, rec, store.AgentReincarnationStateFailed, func(r *store.AgentReincarnation, _ time.Time) {
		r.Error = errMsg
	})
}

// failListedReincarnation is the sweep's counterpart to failReincarnation: it
// CASes an already-listed record (via advanceListedRecord, pinned to the
// state the sweep observed at list time — NOT a fresh read) to failed, and
// only if that wins does it write the agent row (design §3.4 Amendment
// A6/A7). Using tryAdvanceReincarnation's fresh-read semantics here
// instead would defeat the whole point: it would always match "the current
// state" and so could still fail a record — and restore restoreConfig — out
// from under a worker that has genuinely moved it on since the sweep listed
// it as stale.
func (s *Server) failListedReincarnation(ctx context.Context, rec *store.AgentReincarnation, errMsg string, restoreConfig *store.AgentAppliedConfig) {
	s.agentLifecycleLog.Error("reincarnation failed (sweep)",
		"agent_id", rec.AgentID, "reincarnation_id", rec.ID, "error", errMsg)

	failNow, ok, err := s.advanceListedRecord(ctx, rec, store.AgentReincarnationStateFailed, func(r *store.AgentReincarnation, _ time.Time) {
		r.Error = errMsg
	})
	if err != nil || !ok {
		return
	}
	s.writeFailedAgent(ctx, rec.AgentID, errMsg, restoreConfig, failNow)
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

// sweepFailStaleRecord decides, per stale record, whether and how to fail it
// (design §3.4 Amendment A6/A7). The restore decision comes from
// rec.State — the value this sweep pass observed when it listed the record
// as stale — NOT from the agent row:
//
//   - "starting": the worker only reaches this state after
//     DispatchAgentReprovision has already returned successfully, so the
//     disk holds gen N+1. Restoring rec.PreviousAppliedConfig here would
//     make the store claim gen N while the disk (and possibly a container
//     the start call did manage to launch) is gen N+1 — the exact
//     mismatch A4.3 exists to prevent. Fail with no restore.
//   - "pending", "stopping" or "provisioning": reprovision has not
//     (successfully) run yet, so restoring rec.PreviousAppliedConfig is
//     the safe, conservative choice — a genuinely running gen N+1 cannot
//     exist yet for these states.
//
// The agent row is read for exactly one purpose: detecting that something
// else already resolved the agent side of this migration ("" or "failed" —
// a completion write, an earlier failure, or a previous sweep pass), in
// which case the agent is not this record's to touch any more, and only the
// record is failed (failListedReincarnationRecordOnly). The restore policy
// itself never reads the agent row — that row can lag the record by exactly
// one write, since the
// worker always CASes the record before writing the matching agent fields
// (see tryAdvanceReincarnation's callers), so a hub crash in that window
// left the sweep reading a stale "provisioning" for an agent whose record
// already said the truthful "starting". rec.State — the thing A6 made
// truthful — does not have that lag.
//
// Either way, the actual write goes through failListedReincarnation /
// failListedReincarnationRecordOnly, both of which CAS via
// advanceListedRecord against the state THIS FUNCTION observed at list
// time, not a fresh read: if a live worker has since moved the record on
// (to any state, including "starting"), that CAS loses and this sweep pass
// does nothing, instead of failing a record — and restoring stale config —
// out from under a migration that has since succeeded.
//
// A failure to even read the agent falls through to the restore-from-
// rec.State path: there is no better signal available, and it is no worse
// than the round-3 behavior for that case.
func (s *Server) sweepFailStaleRecord(ctx context.Context, rec *store.AgentReincarnation, reason string) {
	agent, err := s.store.GetAgent(ctx, rec.AgentID)
	if err == nil {
		switch agent.ReincarnationState {
		case store.ReincarnationStateNone, store.ReincarnationStateFailed:
			s.failListedReincarnationRecordOnly(ctx, rec, reason)
			return
		}
	}

	var restoreConfig *store.AgentAppliedConfig
	if rec.State != store.AgentReincarnationStateStarting {
		restoreConfig = rec.PreviousAppliedConfig
	}
	s.failListedReincarnation(ctx, rec, reason, restoreConfig)
}

// sweepStaleReincarnations is the replica-safe sweep (design §3.7): it marks
// every non-terminal reincarnation record whose updated_at is older than
// reincarnationStaleAfter as failed, plus — as a backstop — any agent whose
// reincarnation_state is non-terminal and whose own row is equally stale but
// has no matching non-terminal record for the first half to find. It runs at
// hub boot and periodically (server.go registers it with the scheduler as a
// singleton job). The time bound is what makes both call sites safe to run
// on every replica independently, with no distributed lock needed to decide
// which replica "owns" a given migration: a genuinely in-flight worker keeps
// bumping its record's updated_at faster than the bound (tryAdvanceReincarnation
// runs on every step, not just at completion/failure), on whichever replica
// is actually running it, and every write tryAdvanceReincarnation guards
// aborts rather than resurrecting a record the sweep already failed.
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
		s.sweepFailStaleRecord(ctx, rec, reason)
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
