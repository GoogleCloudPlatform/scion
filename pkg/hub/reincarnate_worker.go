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

// reincarnationInFlight reports whether agent is in the middle of a `scion
// reincarnate` migration (design §3.4 Amendment A11 item 2): its
// ReincarnationState is neither ReincarnationStateNone (no migration ever
// started, or the previous one finished) nor ReincarnationStateFailed (the
// migration ended and the agent is back to being independently owned by its
// broker-reported status again — a failed migration is not "in flight").
// Callers use this to suppress the agent's own status-reporting paths
// (broker heartbeat, direct status POST) while the reincarnation worker owns
// Phase/Activity/ExitCode/ExitReason/Message for the target agent.
func reincarnationInFlight(agent *store.Agent) bool {
	switch agent.ReincarnationState {
	case store.ReincarnationStateNone, store.ReincarnationStateFailed:
		return false
	default:
		return true
	}
}

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
	message            *string                   // nil = leave untouched, unconditional set (wins over clearMessageIfEquals below)
	// clearMessageIfEquals, when non-empty and message is nil, clears
	// Message to "" but ONLY if the freshly re-read agent.Message still
	// equals this exact value (design Amendment A26.8: the completion write
	// uses this defensively, against any other writer of Message during the
	// in-flight window — for example a message-only status POST — so it
	// never clobbers a value that is no longer the one it set itself).
	clearMessageIfEquals string
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
			// Store a copy, never the caller's pointer (ptone/scion#1907):
			// the returned agent is handed to the dispatcher, whose
			// applyBrokerResponse writes the broker's echo onto
			// agent.AppliedConfig in place. Aliasing the caller's struct
			// would let that echo leak into it silently; the worker instead
			// takes the echo explicitly with copyBrokerEcho. A shallow copy
			// is enough, given what the reprovision and start dispatch paths
			// do to agent.AppliedConfig:
			//   - applyBrokerAgentConfig and forgetRuntimeTarget assign
			//     top-level string fields only (the copy absorbs them; fresh
			//     never carries RuntimeTarget/RuntimeTargetCandidate);
			//   - the resolved-env merge into Env is skipped on reprovision
			//     and is not on the start path;
			//   - adoptLegacyTZ (buildCreateRequest on reprovision,
			//     buildStartEnv on start) deletes TZ from the SHARED Env and
			//     InlineConfig.Env maps and may set ExplicitTimezone* on the
			//     copy only. For fresh this is a no-op: resolveDerivedConfig's
			//     captureCreateTZ and buildFreshAppliedConfig's
			//     stripAgentEnvTZ already removed TZ from both maps, so there
			//     is nothing to delete or adopt.
			// The copy still shares Env, InlineConfig and the other
			// reference fields with the caller, so any new in-place mutation
			// of those on these paths must revisit this.
			cfg := *upd.appliedConfig
			agent.AppliedConfig = &cfg
		}
		if upd.generation != nil {
			agent.Generation = *upd.generation
		}
		if upd.message != nil {
			agent.Message = *upd.message
		} else if upd.clearMessageIfEquals != "" && agent.Message == upd.clearMessageIfEquals {
			agent.Message = ""
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

// copyBrokerEcho copies the fields a broker answer writes onto the
// dispatched agent's AppliedConfig from src to dst: HarnessConfig,
// HarnessAuth, Profile and, when includeImage is set, Image.
// applyBrokerAgentConfig (httpdispatcher.go) is the source of truth for that
// field list; TestCopyBrokerEcho_MirrorsApplyBrokerAgentConfig fails if it
// starts writing an AppliedConfig field this helper does not copy.
// Like applyBrokerAgentConfig, an empty src field leaves dst unchanged. src
// starts as a copy of dst (updateReincarnationStep), so a field the broker
// did not echo still holds dst's own value. A nil src or dst is a no-op.
func copyBrokerEcho(dst, src *store.AgentAppliedConfig, includeImage bool) {
	if dst == nil || src == nil {
		return
	}
	if src.HarnessConfig != "" {
		dst.HarnessConfig = src.HarnessConfig
	}
	if src.HarnessAuth != "" {
		dst.HarnessAuth = src.HarnessAuth
	}
	if src.Profile != "" {
		dst.Profile = src.Profile
	}
	if includeImage && src.Image != "" {
		dst.Image = src.Image
	}
}

// reincarnateStrPtr is a small helper for populating
// reincarnationStepUpdate.activity and .message, both of which distinguish
// "leave untouched" (nil) from "set to this value, including empty string"
// (non-nil).
func reincarnateStrPtr(s string) *string { return &s }

// reincarnationMigratingMessage is the exact text the worker owns for the
// in-flight Message field, for every reincarnation, self or not (design
// Amendment A26.8, option ii): the stopping step sets it, and the
// completion write clears it only if it still holds this exact value —
// both call sites must use this one function so the two can never drift
// apart.
func reincarnationMigratingMessage(toGeneration int) string {
	return fmt.Sprintf("migrating to generation %d", toGeneration)
}

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
// observe the same lost race again. Each retry waits a short backoff first
// (design §3.4 Amendment A9.4, reincarnationRetryBackoff), respecting ctx.
//
// After a retry that ITSELF returns (false, nil), the record is re-read once
// to disambiguate two situations that look identical from the CAS return
// value alone: "someone else owns it now" vs. "an earlier attempt's write
// actually landed, and only the error reporting that back was lost" (a
// network timeout after commit, for example). State==newState alone is NOT
// sufficient proof of the second case (design §3.4 Amendment A9.1): when
// newState is "failed", the replica-safe sweep can ALSO have written
// "failed" to this exact record in between attempts, and State=="failed"
// then holds for both this call's own (never-landed) earlier attempt AND
// the sweep's write — the two are indistinguishable by state alone. Instead,
// every attempt's UpdatedAt is truncated to microsecond precision and
// recorded before it is sent; the re-read is treated as this call's own only
// if State==newState AND UpdatedAt exactly matches one of those recorded
// stamps — a value only this call could have produced, since it is derived
// from this call's own clock reads.
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
	var erroredStamps []time.Time
	for attempt := 0; attempt < maxAttempts; attempt++ {
		now := time.Now().Truncate(time.Microsecond)
		rec := &store.AgentReincarnation{ID: reincarnationID, State: newState, UpdatedAt: now}
		if mutate != nil {
			mutate(rec, now)
		}
		ok, err := s.store.TryAdvanceAgentReincarnation(ctx, rec, fromState, time.Time{})
		if err != nil {
			lastErr = err
			erroredStamps = append(erroredStamps, now)
			s.agentLifecycleLog.Warn("reincarnation worker: failed to advance record, retrying",
				"reincarnation_id", reincarnationID, "from_state", fromState, "target_state", newState, "attempt", attempt, "error", err)
			if attempt+1 < maxAttempts {
				if backoffErr := reincarnationRetryBackoff(ctx, attempt); backoffErr != nil {
					s.agentLifecycleLog.Warn("reincarnation worker: context done while backing off, giving up early",
						"reincarnation_id", reincarnationID, "target_state", newState, "error", backoffErr)
					return time.Time{}, false, lastErr
				}
			}
			continue
		}
		if !ok && len(erroredStamps) > 0 {
			// This attempt's CAS cleanly found 0 rows, but an EARLIER attempt
			// on this same call errored — that earlier write might have
			// actually landed, with only the error report lost afterward.
			// Re-read to tell that apart from "someone else (e.g. the sweep,
			// when newState is 'failed') owns it now" — State alone cannot
			// distinguish the two, so also require UpdatedAt to match a
			// stamp only this call could have produced.
			if cur, rerr := s.store.GetAgentReincarnation(ctx, reincarnationID); rerr == nil && cur.State == newState && stampedByUs(cur.UpdatedAt, erroredStamps) {
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

// stampedByUs reports whether t exactly matches one of stamps — the set of
// UpdatedAt values tryAdvanceReincarnation itself sent on attempts that
// errored. Used to prove authorship of a record's current write when
// State==newState is not enough proof on its own (design §3.4 Amendment
// A9.1): a value in stamps could only have been produced by this call's own
// clock reads, so an exact match rules out the record having been written
// by anything else, including the replica-safe sweep.
func stampedByUs(t time.Time, stamps []time.Time) bool {
	for _, stamp := range stamps {
		if t.Equal(stamp) {
			return true
		}
	}
	return false
}

// reincarnationRetryBackoff waits 50ms * (attempt+1) before the next retry
// (design §3.4 Amendment A9.4), so a transient DB error that lasts longer
// than an instant does not exhaust the whole retry budget within
// microseconds. It respects ctx: if ctx is done first, it returns ctx's
// error instead of waiting out the rest of the backoff.
func reincarnationRetryBackoff(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
// record as stale (design §3.4 Amendment A7) — AND cutoff as the CAS's
// olderThan guard (design §3.4 Amendment A7.1/A9.2): the same staleness
// bound the sweep used to select this record in the first place.
//
// Both guards matter, and for the same reason: a fresh read (or a
// staleness-only check) here would defeat the whole purpose. Pinning
// expectState to the value observed at list time means a live worker that
// advanced the record's STATE since then — including all the way to
// "starting", meaning reprovision already succeeded — makes this call lose
// the race outright. Pinning cutoff as well closes the narrower remaining
// gap: a worker that bumped the record's UpdatedAt WITHOUT (yet) changing
// its State — no production writer does this today, but a future same-state
// writer could — would otherwise still look stale enough to fail. Either
// guard alone is not sufficient on its own for every reachable case; both
// together are. If the record has moved on by either measure, this call
// loses the race and does nothing, leaving the live worker's own
// tryAdvanceReincarnation calls — which race this one fairly, on the same
// expectState semantics — to decide the record's fate instead.
func (s *Server) advanceListedRecord(ctx context.Context, rec *store.AgentReincarnation, newState string, cutoff time.Time, mutate func(r *store.AgentReincarnation, now time.Time)) (time.Time, bool, error) {
	expectState := rec.State
	upd := *rec // shallow copy: never mutate the caller's (possibly reused) rec
	now := time.Now().Truncate(time.Microsecond)
	upd.State = newState
	upd.UpdatedAt = now
	if mutate != nil {
		mutate(&upd, now)
	}
	ok, err := s.store.TryAdvanceAgentReincarnation(ctx, &upd, expectState, cutoff)
	if err != nil {
		s.agentLifecycleLog.Error("sweep: failed to advance a listed record",
			"reincarnation_id", rec.ID, "expect_state", expectState, "target_state", newState, "error", err)
		return now, false, err
	}
	if !ok {
		s.agentLifecycleLog.Info("sweep: record changed (state or staleness) since it was listed, leaving it alone",
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
// self-migration. admittedDeletionClaim is the delete claim the handler's
// startGate admitted; the failed-marker clear just before the completion
// write is pinned to it.
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
//
// migrationStart is the exact instant handleReincarnateAgent claimed the
// agent (before the record even existed), used verbatim as the preamble's
// catch-up window start (design §3.7, Amendment A25 2a.3/R4, Nit p2a-r1
// review) — it predates anything the gate in the three delivery paths could
// have deferred, so it is a safe (if very slightly generous) lower bound.
//
// requestedBy is the AgentReincarnation record's raw RequestedBy principal ID
// (Amendment A26.1), resolved via buildReincarnationRequesterContext (which
// also implements the A26.2 R1 self-request fix) once the preamble is
// actually built (below) — not any earlier, so a slow or failing lookup can
// never delay or abort the stop/reprovision/start steps that matter more.
//
// plan is the request handler's already-computed ReincarnationPlan — the
// exact same value returned in the 202 response (Amendment A26.4 O1: passed
// in rather than recomputed here, so "the preamble shows exactly what the
// requester saw in the 202" is true by construction, not just true today by
// coincidence of shared inputs). nil omits the Changes: line entirely
// (buildReincarnationPreamble/reincarnationChangesLine both handle it) —
// this line must never be able to fail the reincarnation.
//
// toGeneration is the target generation (the handler's targetGeneration,
// also the 202 response's Generation field), passed in rather than derived
// from agent.Generation+1 partway through the steps below — the same
// by-construction reasoning as plan above. It is also the value the worker
// stamps into its own in-flight status message (Amendment A26.8): Guard 0b
// (handlers_agent_lifecycle.go) blanks Message on a status update that
// carries a Phase or Activity while a migration is in flight, so the
// worker is the sole writer of §3.9's "migrating to generation N+1" for
// both self and non-self migrations.
func (s *Server) runReincarnationWorker(ctx context.Context, agentID, reincarnationID string, previous, fresh *store.AgentAppliedConfig, handoff string, migrationStart time.Time, requestedBy string, plan *ReincarnationPlan, toGeneration int, admittedDeletionClaim int64) {
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
	//
	// Message is set here too (design Amendment A26.8): this is the
	// worker's first write, and setting it at the same first step write for
	// every reincarnation (self or not) makes the in-flight status
	// mode-neutral and informative for a coordinator watching a child, not
	// just self-migration.
	agent, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStopping,
		phase:              string(state.PhaseStopping),
		activity:           reincarnateStrPtr(""),
		message:            reincarnateStrPtr(reincarnationMigratingMessage(toGeneration)),
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
	requesterCtx := s.buildReincarnationRequesterContext(ctx, agent, requestedBy)
	preamble := s.buildReincarnationPreamble(agent, toGeneration, handoff, migrationStart, requesterCtx, plan)
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
		// never actually provisioned. The failed render may have left the
		// on-disk config partly gen N+1 (and after a hub-side timeout the
		// broker's render may still be running), so first try a
		// best-effort re-render of `previous` (ptone/scion#1935). The row
		// is restored whatever its outcome, as before. The failed
		// reprovision has already revoked the agent's credentials if it
		// minted one, so a refused re-render changes nothing there.
		errMsg := "reprovision failed: " + err.Error()
		s.rerenderPreviousConfig(ctx, dispatcher, agentID, reincarnationID, store.AgentReincarnationStateProvisioning, errMsg, previous)
		s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateProvisioning, errMsg, previous)
		return
	}

	// Take the reprovision echo (HarnessConfig, HarnessAuth, Profile) into
	// fresh, so the starting step's write below persists it, the same as
	// create. The echoed image is deliberately NOT taken: the broker's
	// provision-only response reports the rendered config's image
	// (runtimebroker handlers.go, agentResp.Image = cfg.Image), before the
	// start-time resolution in pkg/agent/run.go applies the broker
	// profile's image_registry rewrite (or keeps a bare name when a local
	// image exists). fresh.Image keeps buildFreshAppliedConfig's value,
	// already rewritten to the dispatcher's registry, until the start echo
	// below supplies the image the runtime actually resolved. If the start
	// fails and the re-render of `previous` does not succeed,
	// failReincarnation leaves the row as the starting step wrote it, so
	// the row keeps that qualified image and not the unqualified echo. If
	// the start succeeds but its response carries no image (the
	// broker's started-but-not-listed fallback, an empty response body, a
	// deferred start), the qualified image stays too.
	copyBrokerEcho(fresh, agent.AppliedConfig, false)

	startingNow, ok, err := s.tryAdvanceReincarnation(ctx, reincarnationID, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting, reincarnationStepMaxAttempts, nil)
	if err != nil {
		// Reprovision already succeeded (same handling as the other
		// post-reprovision-success failure sites below).
		s.failAfterReprovision(ctx, dispatcher, agentID, reincarnationID, store.AgentReincarnationStateProvisioning, "failed to advance record to starting: "+err.Error(), previous)
		return
	}
	if !ok {
		return
	}
	// Step: start, without the harness resume flag (design §3.6, decision D3
	// — always a fresh session). Reprovision already succeeded, so the disk
	// now holds gen N+1; a failure from here on restores `previous` on the
	// row only if failAfterReprovision's re-render of `previous` succeeds.
	// Otherwise restoring would make the store claim gen N while the disk
	// (and any container the start call did manage to create) is gen N+1.
	// A start failure re-renders only when its outcome is definitive (see
	// the DispatchAgentStart error handling below).
	// appliedConfig: fresh persists the fields taken from the reprovision
	// echo (see above).
	//
	// This write sets phase to "starting" before the DispatchAgentStart call
	// below, so that call's own priorPhase capture sees "starting" rather
	// than "provisioning" — isConfirmedNonRunningPhase excludes "starting",
	// so a start failure on this path does not revoke-by-agent even though
	// the stop step earlier in this same reincarnation already confirmed the
	// prior container is gone. This is an accepted, deliberate gap, not an
	// oversight: it fails toward not revoking, the same direction every
	// other guard in DispatchAgentStart takes, rather than threading this
	// worker's own confirmation through the dispatcher as a special case.
	// The re-render after a start failure keeps that direction: it is a
	// reprovision dispatch, and a reprovision that fails revokes the
	// agent's credentials by agent (dispatchProvision), so it runs only
	// when reincarnationStartLeftNoContainer says no container can be
	// using them.
	agent, err = s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStarting,
		phase:              string(state.PhaseStarting),
		appliedConfig:      fresh,
		now:                startingNow,
	}, reincarnationStepMaxAttempts)
	if err != nil {
		s.failAfterReprovision(ctx, dispatcher, agentID, reincarnationID, store.AgentReincarnationStateStarting, "failed to record starting state: "+err.Error(), previous)
		return
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, preamble, false); err != nil {
		errMsg := "start failed: " + err.Error()
		if !reincarnationStartLeftNoContainer(err) {
			// Ambiguous outcome (a timeout, a transport error, a lost or
			// unreadable response, a proxy error, a deferred start, or a
			// broker failure from inside Manager.Start): a gen N+1
			// container may exist and use the agent's credentials. Keep
			// the pre-ptone/scion#1935 behaviour: no re-render (a refused
			// re-render would revoke those credentials), and the row stays
			// at gen N+1, so row, disk and any container agree.
			s.agentLifecycleLog.Info("reincarnation failed: start outcome ambiguous, a container may exist; not re-rendering the previous config",
				"agent_id", agentID, "reincarnation_id", reincarnationID, "from_state", store.AgentReincarnationStateStarting, "cause", errMsg)
			s.failReincarnation(ctx, agentID, reincarnationID, store.AgentReincarnationStateStarting, errMsg, nil)
			return
		}
		s.failAfterReprovision(ctx, dispatcher, agentID, reincarnationID, store.AgentReincarnationStateStarting, errMsg, previous)
		return
	}
	// Take the start echo, image included: this is the runtime-resolved
	// image. The record's NewAppliedConfig and the completion write below
	// both persist fresh.
	copyBrokerEcho(fresh, agent.AppliedConfig, true)

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

	// A successful reincarnate clears a failed delete marker (design
	// ptone/scion#2483 §2.1). The new generation is live and this worker won
	// the record, so the start succeeded. The clear runs BEFORE the
	// completion write below, so any caller that observes completion
	// (reincarnation_state none) also observes the cleared marker; until
	// then start stays blocked by the in-progress reincarnation. If the
	// completion write then fails, the marker stays cleared: that failure
	// is bookkeeping only. The clear bumps state_version; the completion
	// write's re-read-and-retry absorbs it. Pinned to the claim the
	// handler's gate admitted, so a newer delete keeps its marker. agent is
	// the starting-step row: a marker at the admitted claim cannot appear
	// later, because a new claim always bumps the claim epoch.
	s.clearFailedDeletionAtClaim(ctx, agent, admittedDeletionClaim)

	// generation++ and reincarnation_state clears (AC-1). Phase is
	// deliberately left at "starting" here — exactly like a normal start
	// dispatch (see wake_dm.go), the container's own status report moves it
	// to "running"; the worker forcing that value would be a lie if the
	// container is still booting when this write lands. A few extra retry
	// attempts: losing this write leaves the row saying "starting" forever
	// even though the new generation is live, and generation stuck at N even
	// though gen N+1 is what is actually running.
	//
	// appliedConfig: fresh makes the row end with exactly the config recorded
	// in rec.NewAppliedConfig above, including the start echo copyBrokerEcho
	// took after the starting step's write.
	//
	// clearMessageIfEquals (design Amendment A26.8): clears the in-flight
	// "migrating to generation N" message set at the stopping step, but only
	// if it is still exactly that value — defensively, against any other
	// writer of Message during the window (for example a message-only
	// status POST); this write must never clobber a value that is no
	// longer the one the stopping step set.
	if _, err := s.updateReincarnationStep(ctx, agentID, reincarnationStepUpdate{
		reincarnationState:   store.ReincarnationStateNone,
		generation:           &toGeneration,
		appliedConfig:        fresh,
		clearMessageIfEquals: reincarnationMigratingMessage(toGeneration),
		now:                  completedNow,
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

// reincarnationRerenderTimeout bounds the best-effort re-render of the
// previous config after a failed reincarnation (rerenderPreviousConfig). A
// reprovision dispatch can make two broker round trips (the env-gather first
// pass, then the replay with the gathered env), each bounded by the broker
// transport's own timeout (the control-channel RequestTimeout, 120s by
// default), so this allows both plus margin. It is well inside
// reincarnationStaleAfter, so the sweep cannot resolve the record while the
// re-render is still running.
const reincarnationRerenderTimeout = 5 * time.Minute

// reincarnationStartLeftNoContainer reports whether a DispatchAgentStart
// error is a definitive outcome that leaves no gen N+1 container: the broker
// never acted on the request or explicitly refused it
// (isConfirmedStartNotActedOnError, the classification DispatchAgentStart's
// own start-failure revoke uses), and did not report a failure from inside
// Manager.Start (brokerStartAttempted, by which point a container may have
// been created). This is the pair shouldRevertRun uses. The worker's stop
// step already confirmed the previous container is gone, so no container
// is running for the agent. Every other error is ambiguous.
func reincarnationStartLeftNoContainer(err error) bool {
	return isConfirmedStartNotActedOnError(err) && !brokerStartAttempted(err)
}

// rerenderPreviousConfig makes one best-effort reprovision dispatch with the
// outgoing generation's applied config (ptone/scion#1935, option (c)), to put
// the on-disk agent config back to generation N after a reincarnation that
// failed once a reprovision dispatch had been attempted. It runs on a
// context detached from ctx with its own timeout, never retries, and only
// logs a failure: cause, the caller's original error, is what the
// reincarnation fails with. It reports whether the broker accepted the
// re-render.
//
// The agent row's AppliedConfig is not written here (the dispatch's
// runtime-target clear still writes the row; failReincarnation's retry
// absorbs the version bump). The dispatch uses a re-read of the row with
// AppliedConfig replaced by a shallow copy of previous, so neither the row
// nor previous (the record's PreviousAppliedConfig) picks up the broker's
// echo. The copy shares Env and InlineConfig with previous, and the
// dispatch's adoptLegacyTZ can delete TZ from those maps; that is a no-op,
// because buildFreshAppliedConfig already ran adoptLegacyTZ on previous in
// place.
//
// Credentials: like any reprovision dispatch, a failed re-render revokes
// the agent's credentials by agent (dispatchProvision's create-failed
// revoke). Callers therefore re-render only when no container can be using
// them: after a failed reprovision (which already revoked) or a definitive
// start failure (reincarnationStartLeftNoContainer).
//
// The remaining gap, tracked in ptone/scion#3043 (atomic render in
// Manager.Reprovision is the real fix): the re-render can itself fail, be
// refused or be cut off, leaving the disk partly gen N+1 again, and
// Reprovision overlays rather than replaces, so a file only gen N+1
// rendered stays. After a hub-side timeout the broker's original render
// may still be running when the re-render arrives. Nothing on the broker
// serialises the two, so a successful re-render can still leave mixed
// N/N+1 files.
func (s *Server) rerenderPreviousConfig(ctx context.Context, dispatcher AgentDispatcher, agentID, reincarnationID, fromState, cause string, previous *store.AgentAppliedConfig) bool {
	if dispatcher == nil || previous == nil {
		return false
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reincarnationRerenderTimeout)
	defer cancel()
	agent, err := s.store.GetAgent(rctx, agentID)
	if err != nil {
		s.agentLifecycleLog.Warn("reincarnation failed: could not load the agent to re-render the previous config",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "from_state", fromState, "cause", cause, "error", err)
		return false
	}
	cfg := *previous
	agent.AppliedConfig = &cfg
	if err := dispatcher.DispatchAgentReprovision(rctx, agent); err != nil {
		s.agentLifecycleLog.Warn("reincarnation failed: best-effort re-render of the previous config failed; the on-disk agent config may be partly the new generation",
			"agent_id", agentID, "reincarnation_id", reincarnationID, "from_state", fromState, "cause", cause, "error", err)
		return false
	}
	s.agentLifecycleLog.Info("reincarnation failed: re-rendered the previous config",
		"agent_id", agentID, "reincarnation_id", reincarnationID, "from_state", fromState, "cause", cause)
	return true
}

// failAfterReprovision fails a reincarnation after its reprovision dispatch
// succeeded (so the disk holds gen N+1) and no gen N+1 container can be
// running. It first re-renders previous (rerenderPreviousConfig). Only when
// that succeeds, so the disk is back at gen N, is the row restored to
// previous; otherwise the row keeps the gen N+1 config, matching the disk.
// errMsg is recorded unchanged either way.
func (s *Server) failAfterReprovision(ctx context.Context, dispatcher AgentDispatcher, agentID, reincarnationID, fromState, errMsg string, previous *store.AgentAppliedConfig) {
	var restore *store.AgentAppliedConfig
	if s.rerenderPreviousConfig(ctx, dispatcher, agentID, reincarnationID, fromState, errMsg, previous) {
		restore = previous
	}
	s.failReincarnation(ctx, agentID, reincarnationID, fromState, errMsg, restore)
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
func (s *Server) failListedReincarnationRecordOnly(ctx context.Context, rec *store.AgentReincarnation, errMsg string, cutoff time.Time) bool {
	_, ok, err := s.advanceListedRecord(ctx, rec, store.AgentReincarnationStateFailed, cutoff, func(r *store.AgentReincarnation, _ time.Time) {
		r.Error = errMsg
	})
	return err == nil && ok
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
func (s *Server) failListedReincarnation(ctx context.Context, rec *store.AgentReincarnation, errMsg string, restoreConfig *store.AgentAppliedConfig, cutoff time.Time) bool {
	s.agentLifecycleLog.Error("reincarnation failed (sweep)",
		"agent_id", rec.AgentID, "reincarnation_id", rec.ID, "error", errMsg)

	failNow, ok, err := s.advanceListedRecord(ctx, rec, store.AgentReincarnationStateFailed, cutoff, func(r *store.AgentReincarnation, _ time.Time) {
		r.Error = errMsg
	})
	if err != nil || !ok {
		return false
	}
	s.writeFailedAgent(ctx, rec.AgentID, errMsg, restoreConfig, failNow)
	return true
}

// reincarnationRequesterFallback is design §3.9's "{requester}" text for
// when the requester cannot be resolved to a display name (Amendment A26.1):
// used both when AgentReincarnation.RequestedBy is empty and when
// resolveReincarnationRequesterName's lookups miss or error. Per Amendment
// A26.2 O2, this only ever appears in step 3 now — an unresolved header
// clause is omitted entirely rather than rendering this same text there too
// (see buildReincarnationPreamble).
const reincarnationRequesterFallback = "whoever requested this migration"

// resolveReincarnationRequesterName resolves a bare principal ID (design
// §3.2: a polymorphic ID — a User.ID or an Agent.ID, no FK, "like
// Agent.CreatedBy") to a display string for design §3.9's "{requester}"
// slots — an addressable handle usable with `scion message`, since a generic
// phrase gives the new generation nothing to act on (Amendment A26.1). Used
// for both AgentReincarnation.RequestedBy and, in the self-request case,
// agent.CreatedBy (Amendment A26.2 R1) — the same resolution rules apply to
// either kind of principal ID.
//
// Resolution order: GetUser → "user:<email>"; on a miss (store.ErrNotFound),
// GetAgent → "agent:<slug>". The returned bool is true only on a genuine
// resolution; every other outcome — an empty id, a miss in both lookups, a
// user found with an empty Email (logged with its own distinct WARN rather
// than the generic "could not resolve" message, since the principal itself
// was found), or any *other* store error along
// either lookup — returns (reincarnationRequesterFallback, false) with a
// WARN log identifying the unresolved id. Callers that must never emit a
// specific principal without proof (e.g. the self-request guard in
// buildReincarnationRequesterContext) rely on this: a non-nil error can
// never produce a false positive, only a fallback.
//
// This must never fail the reincarnation: it runs deep inside the
// background worker, called only after stop/reprovision have already
// succeeded (see runReincarnationWorker) — a lookup failure here is a
// cosmetic loss in the preamble's wording, not a reason to abandon a
// migration that has already torn down the old container.
func (s *Server) resolveReincarnationRequesterName(ctx context.Context, id string) (handle string, resolved bool) {
	if id == "" {
		return reincarnationRequesterFallback, false
	}

	if user, err := s.store.GetUser(ctx, id); err == nil {
		if user != nil {
			if user.Email != "" {
				return "user:" + user.Email, true
			}
			s.agentLifecycleLog.Warn("reincarnation preamble: resolved a user with an empty email, falling back",
				"id", id)
			return reincarnationRequesterFallback, false
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.agentLifecycleLog.Warn("reincarnation preamble: GetUser failed resolving a principal, falling back",
			"id", id, "error", err)
	}

	if agent, err := s.store.GetAgent(ctx, id); err == nil {
		if agent != nil && agent.Slug != "" {
			return "agent:" + agent.Slug, true
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.agentLifecycleLog.Warn("reincarnation preamble: GetAgent failed resolving a principal, falling back",
			"id", id, "error", err)
	}

	s.agentLifecycleLog.Warn("reincarnation preamble: could not resolve principal to a user or agent, falling back",
		"id", id)
	return reincarnationRequesterFallback, false
}

// reincarnationRequesterContext carries everything buildReincarnationPreamble
// needs to render the requester-dependent parts of the preamble (Amendments
// A26.1 and A26.2 R1): whether the migration was self-requested, the
// resolved requester handle for a non-self request, and — only for a
// self-request — the resolved creator handle used as a fallback hint.
type reincarnationRequesterContext struct {
	// IsSelf is true when the agent migrated itself (RequestedBy ==
	// agent.ID). Handle/Resolved are meaningless in that case: the preamble
	// must never emit the agent's own handle (A26.2 R1), so
	// buildReincarnationRequesterContext never populates them for a self
	// request.
	IsSelf bool

	// Handle and Resolved describe the non-self requester resolution.
	Handle   string
	Resolved bool

	// CreatorHandle and CreatorResolved describe agent.CreatedBy's
	// resolution, used only for the self-request step 3 hint (A26.2 R1).
	CreatorHandle   string
	CreatorResolved bool
}

// defaultReincarnationRequesterResolveTimeout is the production value for
// Server.requesterResolveTimeout() (design Amendment A26.14/A26.18).
const defaultReincarnationRequesterResolveTimeout = 5 * time.Second

// requesterResolveTimeout returns the timeout bounding
// buildReincarnationRequesterContext's store calls. The zero value of the
// Server field means defaultReincarnationRequesterResolveTimeout; a test
// sets the field on its own Server instance to shorten it, with no shared
// mutable state across tests (Amendment A26.18).
func (s *Server) requesterResolveTimeout() time.Duration {
	if s.reincarnationRequesterResolveTimeout == 0 {
		return defaultReincarnationRequesterResolveTimeout
	}
	return s.reincarnationRequesterResolveTimeout
}

// buildReincarnationRequesterContext resolves the requester and, for a
// self-migration, the creator hint, before the preamble is built (Amendments
// A26.1, A26.2 R1). Self-migration (the AC-10/2c dogfood path) must never
// have resolveReincarnationRequesterName resolve the agent's own ID back to
// its own handle — that would tell the new generation to message itself.
// The fix: detect requestedBy == agent.ID up front and never resolve or
// emit the agent's own handle at all; instead resolve agent.CreatedBy as a
// hint for the handoff-driven "Authority and ownership" instruction (see
// buildReincarnationPreamble). The defensive equality check below (never
// surface CreatorHandle if it equals the agent's own handle) is
// belt-and-suspenders: CreatedBy is not expected to ever equal an agent's
// own ID, but "never emit the agent's own handle" is stated as an absolute
// rule.
func (s *Server) buildReincarnationRequesterContext(ctx context.Context, agent *store.Agent, requestedBy string) reincarnationRequesterContext {
	// Amendment A26.14: resolution is best-effort preamble metadata (A26.1
	// 7c: it must never fail the reincarnation), but it runs on the
	// worker's own ctx after the old container is already stopped, so an
	// unbounded store call here could stall the migration with only the
	// replica-safe sweep's 30-minute backstop to eventually notice. The
	// derived, timeout-bound context is used only for the resolver calls
	// below — it is never returned or threaded into any later worker step.
	// On timeout, resolveReincarnationRequesterName's existing error path
	// (a non-ErrNotFound error) yields the fallback and logs a WARN, the
	// same as any other store error.
	resolveCtx, cancel := context.WithTimeout(ctx, s.requesterResolveTimeout())
	defer cancel()

	if requestedBy != "" && requestedBy == agent.ID {
		ctxOut := reincarnationRequesterContext{IsSelf: true}
		if agent.CreatedBy != "" {
			handle, resolved := s.resolveReincarnationRequesterName(resolveCtx, agent.CreatedBy)
			if resolved && handle != "agent:"+agent.Slug {
				ctxOut.CreatorHandle, ctxOut.CreatorResolved = handle, true
			}
		}
		return ctxOut
	}

	handle, resolved := s.resolveReincarnationRequesterName(resolveCtx, requestedBy)
	return reincarnationRequesterContext{Handle: handle, Resolved: resolved}
}

// buildReincarnationPreamble builds the hub-authored instructions delivered
// as the new generation's first harness input (design §3.9).
//
// Amendment A26 (Phase 2b): steps 1 and 2 keep their 2a wording verbatim (the
// A23 neutral step 1 and the A25.2 catch-up-window step 2); "everything else
// in §3.9" — steps 3-4 and the no-handoff fallback text — comes in from the
// original design here. Step 4 names the handoff's actual section headings
// ("Immediate active work" and "Do not redo") from the --handoff-template
// sections (cmd/reincarnate.go's reincarnateHandoffTemplateText), rather than
// §3.9's literal "Next action"/"Do not redo" labels — "Next action" is a
// sub-field of "Immediate active work (status, next action)" in the
// template's actual section list, not its own heading, so pointing at the
// real heading keeps the instruction literally followable against the
// template the outgoing generation was told to use.
//
// requester is pre-resolved by buildReincarnationRequesterContext (Amendment
// A26.1, A26.2 R1) — this function does no store I/O itself and stays a pure
// string builder. It renders three cases:
//   - Self-request (requester.IsSelf): the header says "reincarnated at its
//     own request" and step 3 points at the handoff's own "Authority and
//     ownership" section rather than naming the agent itself (A26.2 R1: a
//     self-migration must never be told to message its own handle). If
//     agent.CreatedBy resolved to a *different* handle, it's added as its
//     own sentence, a hint for when the handoff names no owner.
//   - Resolved (requester.Resolved): the header names requester.Handle
//     ("reincarnated on request of %s") and step 3 addresses it directly.
//   - Unresolved (neither of the above): the header's "on request of" clause
//     is omitted entirely (Amendment A26.2 O2 — a generic phrase there reads
//     as a tautology), while step 3 keeps reincarnationRequesterFallback,
//     since a next action ("message someone") is still better than none.
//
// plan is the request handler's already-computed ReincarnationPlan (Amendment
// A26.4 O1: passed in by runReincarnationWorker rather than recomputed here,
// so this is exactly what the requester saw in the 202 response, by
// construction). nil omits the line entirely — see reincarnationChangesLine,
// defined below this function. A "Changes: template A->B, image X->Y,
// harness-config P->Q" line is rendered right after the header, naming only
// the fields that actually changed, with hash-valued fields abbreviated to
// `sha256:` plus 12 hex in this preamble line only (Amendment A26.4 O2) —
// the dry-run CLI output is unaffected and still shows the full hash. §3.9
// also has a second header line naming the migration time, which is
// declined here as redundant with step 2's stated window start.
//
// migrationStart is step 2's catch-up window start (design §3.7, Amendment
// A25): only the start is named, not an end — an end timestamp would have to
// be the instant reincarnation_state clears, which isn't known until the
// completion write, long after this preamble is built, so any value written
// here would understate the window. "Since {start}" makes no claim about
// when the window closes. The command given is runnable as written:
// `scion conversation catch-up` takes a `--since <duration>` flag, not
// absolute timestamps, so step 2 asks the agent to choose a duration
// reaching back to the stated instant. Plain `scion conversation list`'s
// output truncates IDs to 12 runes with no `conv:` prefix, which `catch-up`
// cannot accept (it only takes `conv:<full-uuid>`, `@name` or `#name`), so
// step 2 says `list --json` (full untruncated IDs) and spells out the
// `conv:<id>` prefix explicitly.
//
// The redelivery-adjacent wording ("messages ... can be read with
// catch-up") was dropped in Phase 1 (A11.4) because it was false then —
// catch-up did not work inside agent containers and a message sent during
// the gap was rejected or silently dropped, not saved. Phase 2a's F4 fix
// (agent-mode hub-context detection) and migration gate (every hub delivery
// path persists-and-defers instead of rejecting/dropping while
// agents.reincarnation_state is non-terminal) make it true again, so it
// comes back here.
func (s *Server) buildReincarnationPreamble(agent *store.Agent, toGeneration int, handoff string, migrationStart time.Time, requester reincarnationRequesterContext, plan *ReincarnationPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[SCION REINCARNATION] You are generation %d of agent %q (id %s)", toGeneration, agent.Slug, agent.ID)
	switch {
	case requester.IsSelf:
		b.WriteString(", reincarnated at its own request.\n")
	case requester.Resolved:
		fmt.Fprintf(&b, ", reincarnated on request of %s.\n", requester.Handle)
	default:
		// Amendment A26.2 O2: omit the clause entirely rather than render
		// the fallback phrase here too — "reincarnated on request of
		// whoever requested this migration" reads as a tautology.
		b.WriteString(".\n")
	}
	if plan != nil {
		if changes := reincarnationChangesLine(*plan); changes != "" {
			b.WriteString(changes)
		}
	}
	b.WriteString("Before resuming:\n")
	// Design §3.4 Amendment A23: neutral wording, since Phase 1b also serves
	// shared-workspace and hub-managed agents, whose git state is not "your
	// branch" the way a clone-per-agent agent's is.
	b.WriteString(" 1. Verify your environment: `git status` shows the branch and state your handoff describes, and any files your handoff names as canonical are readable.\n")
	fmt.Fprintf(&b, " 2. Run `scion conversation list --json` and, for each conversation, "+
		"`scion conversation catch-up conv:<id> --since <duration reaching back to %s>`. "+
		"Messages sent to you since %s were saved to your conversations, not dropped. "+
		"If that command is unavailable in this environment, rely on the handoff and on incoming messages.\n",
		migrationStart.UTC().Format(time.RFC3339), migrationStart.UTC().Format(time.RFC3339))
	switch {
	case requester.IsSelf:
		// Amendment A26.2 R1: never emit the agent's own handle. The
		// handoff's own "Authority and ownership" section (§3.9's handoff
		// template) is authoritative on who to tell; the resolved creator
		// (if any, and not the agent itself) is only a hint for when the
		// handoff names no one, rendered as its own sentence (A26.3 N2).
		fmt.Fprintf(&b, " 3. Message the owner named in your handoff's \"Authority and ownership\" section that generation %d is up, and state your next action.", toGeneration)
		if requester.CreatorResolved {
			fmt.Fprintf(&b, " If the handoff names none, message %s.", requester.CreatorHandle)
		}
		b.WriteString("\n")
	case requester.Resolved:
		fmt.Fprintf(&b, " 3. Message %s that generation %d is up, and state your next action.\n", requester.Handle, toGeneration)
	default:
		fmt.Fprintf(&b, " 3. Message %s that generation %d is up, and state your next action.\n", reincarnationRequesterFallback, toGeneration)
	}
	b.WriteString(" 4. Continue from the handoff's \"Immediate active work\" section below (its next action). Do not redo anything listed under \"Do not redo\".\n")
	b.WriteString("The handoff from your previous generation follows.\n---\n")
	if handoff != "" {
		b.WriteString(handoff)
	} else {
		b.WriteString("No handoff was provided. Reconstruct context from your branch, your conversations " +
			"(`scion conversation list`), and any project scratchpad before acting.")
	}
	return b.String()
}

// reincarnationChangesLine renders §3.9's "Changes:" preamble line from a
// ReincarnationPlan, naming only the fields that actually changed between
// the outgoing and incoming generation, and returning "" (no line at all)
// when nothing did. Bounded deliberately: only the three fields the plan
// already carries for the dry-run response (Template, Image, HarnessCfg)
// are considered. Template and HarnessCfg are content hashes
// (`sha256:<64 hex>`, pkg/transfer/hash.go); Amendment A26.4 O2 abbreviates
// them to `sha256:` plus 12 hex in this line only, since the full hash tells
// the new generation nothing beyond "it changed" — the dry-run CLI output
// (printReincarnationPlan) is untouched and still shows the full value.
func reincarnationChangesLine(plan ReincarnationPlan) string {
	var parts []string
	if plan.Template.Old != plan.Template.New {
		parts = append(parts, fmt.Sprintf("template %s->%s", reincarnateAbbreviateHash(plan.Template.Old), reincarnateAbbreviateHash(plan.Template.New)))
	}
	if plan.Image.Old != plan.Image.New {
		parts = append(parts, fmt.Sprintf("image %s->%s", reincarnateValueOrNone(plan.Image.Old), reincarnateValueOrNone(plan.Image.New)))
	}
	if plan.HarnessCfg.Old != plan.HarnessCfg.New {
		parts = append(parts, fmt.Sprintf("harness-config %s->%s", reincarnateAbbreviateHash(plan.HarnessCfg.Old), reincarnateAbbreviateHash(plan.HarnessCfg.New)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Changes: " + strings.Join(parts, ", ") + "\n"
}

// reincarnateValueOrNone renders an empty FieldChange side as "(none)" so
// e.g. a legacy agent with no recorded template hash reads clearly rather
// than as a blank.
func reincarnateValueOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// reincarnateHashPrefixLen is how many hex characters of a content hash
// Amendment A26.4 O2 keeps in the Changes: preamble line — enough to be a
// stable, greppable short reference without printing all 64.
const reincarnateHashPrefixLen = 12

// reincarnateAbbreviateHash renders s as "(none)" when empty (via
// reincarnateValueOrNone), or, when it matches the `sha256:<64 hex>` shape
// (pkg/transfer/hash.go), abbreviates it to `sha256:` plus the first
// reincarnateHashPrefixLen hex characters. Any other shape (a legacy value
// that isn't a content hash) is returned unabbreviated, unchanged.
func reincarnateAbbreviateHash(s string) string {
	if s == "" {
		return reincarnateValueOrNone(s)
	}
	const prefix = "sha256:"
	if strings.HasPrefix(s, prefix) && len(s) == len(prefix)+64 {
		return prefix + s[len(prefix):len(prefix)+reincarnateHashPrefixLen]
	}
	return s
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
// one write, since the worker always CASes the record before writing the
// matching agent fields (see tryAdvanceReincarnation's callers), so a hub
// crash in that window left the sweep reading a stale "provisioning" for an
// agent whose record already said the truthful "starting". rec.State — the
// thing A6 made truthful — does not have that lag.
//
// Either way, the actual write goes through failListedReincarnation /
// failListedReincarnationRecordOnly, both of which CAS via
// advanceListedRecord against the state AND the staleness cutoff THIS
// FUNCTION observed at list time, not a fresh read: if a live worker has
// since moved the record on by either measure — including all the way to
// "starting" — that CAS loses and this sweep pass does nothing, instead of
// failing a record — and restoring stale config — out from under a
// migration that has since succeeded.
//
// A failure to even read the agent falls through to the restore-from-
// rec.State path: there is no better signal available.
func (s *Server) sweepFailStaleRecord(ctx context.Context, rec *store.AgentReincarnation, reason string, cutoff time.Time) bool {
	agent, err := s.store.GetAgent(ctx, rec.AgentID)
	if err == nil {
		switch agent.ReincarnationState {
		case store.ReincarnationStateNone, store.ReincarnationStateFailed:
			return s.failListedReincarnationRecordOnly(ctx, rec, reason, cutoff)
		}
	}

	var restoreConfig *store.AgentAppliedConfig
	if rec.State != store.AgentReincarnationStateStarting {
		restoreConfig = rec.PreviousAppliedConfig
	}
	return s.failListedReincarnation(ctx, rec, reason, restoreConfig, cutoff)
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
	swept := 0
	for _, rec := range stale {
		// Design §3.4 Amendment A9 (FYI-4): count only CASes this sweep pass
		// actually won — a record advanceListedRecord found already moved on
		// (state or staleness) was not this pass's doing, and counting it
		// anyway would overstate how much the sweep actually changed.
		if s.sweepFailStaleRecord(ctx, rec, reason, cutoff) {
			swept++
		}
	}

	orphans, err := s.store.ListAgentsWithStaleNonTerminalReincarnationState(ctx, cutoff)
	if err != nil {
		return swept, fmt.Errorf("list stale non-terminal agent reincarnation states: %w", err)
	}
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
