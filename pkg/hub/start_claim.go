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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file is the hub side of the start claim: every agent start takes an
// owned, leased claim in the store before it is dispatched, renews it while
// the start runs, and settles it by outcome. A start whose outcome is
// unknown keeps the claim, unconfirmed, until the start-claim reaper sees
// what the runtime did, so no second start can remove a container the
// first may have created.

// errStartClaimLost is returned when the claim was lost while the start ran:
// a stop superseded it, the reaper demoted it, or its lease could not be
// renewed in time. The start's context is cancelled and nothing is written.
var errStartClaimLost = errors.New("start abandoned: the start claim was lost")

// stopClaimWait bounds how long a start waits for a queued stop's short
// stop-kind claim to be released before giving up.
const stopClaimWait = 15 * time.Second

// startClaimRetryInterval is the retry interval of a renew whose outcome was
// an error (unknown, not lost), until the fence deadline.
const startClaimRetryInterval = 5 * time.Second

// startOutcome is how a start ended, for the claim.
type startOutcome int

const (
	// startReleased: the start succeeded, or definitely did not act
	// (nothing reached the broker, or the broker refused it). The claim is
	// released.
	startReleased startOutcome = iota
	// startUnconfirmed: the start may have reached the runtime (a timeout,
	// a dropped connection, a response the hub could not read, an expired
	// cross-node wait, or the start deadline). The claim is kept,
	// unconfirmed.
	startUnconfirmed
	// startHandedOff: an async launch accepted the start; the launch's end
	// settles the claim.
	startHandedOff
)

// startOutcomeOf classifies a dispatch error.
func startOutcomeOf(err error) startOutcome {
	if err == nil || isConfirmedStartNotActedOnError(err) {
		return startReleased
	}
	var incomplete *AgentCreateIncompleteError
	var tokenErr *agentTokenIssueError
	// errStartClaimLost is listed as released, but a lost claim is never
	// settled by its holder (finish writes nothing once lost), so the
	// classification only reaches callers, which treat the start as possibly
	// running.
	var stillMissing *ErrEnvStillMissing
	var quotaErr *startQuotaError
	if errors.Is(err, errStartedStatusWrite) {
		return startReleased // the start succeeded
	}
	switch {
	case errors.As(err, &quotaErr):
		return startReleased
	case errors.Is(err, ErrLaunchInFlight), errors.As(err, &incomplete), errors.As(err, &stillMissing),
		errors.Is(err, ErrLaunchInvalidPhase),
		errors.As(err, &tokenErr), errors.Is(err, errBrokerLacksEmptyPerAgent),
		isBrokerRuntimeUnavailable(err), errors.Is(err, errStartClaimLost), errors.Is(err, store.ErrDeleteInProgress):
		return startReleased
	}
	return startUnconfirmed
}

// startClaimsEnabled reports whether starts take start claims: always, for a
// server built by New. A Server constructed directly (tests) has them off.
func (s *Server) startClaimsEnabled() bool { return s.startClaimsOn }

// expectedClaimTarget is the runtime target a start of a is expected to
// use: its recorded target, or, for a runtime whose target is its name
// (every runtime but Kubernetes, whose target also names the cluster
// context and namespace), the runtime name. "" when unknown; the
// observations then fall back to the broker's only complete target.
func expectedClaimTarget(a *store.Agent) string {
	if t := agentRuntimeTarget(a); t != "" {
		return t
	}
	if a.Runtime != "" && !strings.HasPrefix(a.Runtime, "kubernetes") {
		return a.Runtime
	}
	return ""
}

// startClaimRun is one held claim: its lease renewal, self-fence and start
// deadline.
type startClaimRun struct {
	s     *Server
	agent string
	owner string
	claim store.StartClaim
	cfg   StartClaimSettings

	ctx    context.Context // the start runs under it: deadline, cancelled when lost
	cancel context.CancelFunc

	mu      sync.Mutex
	fenceAt time.Time // monotonic; past it the claim is treated as lost
	lost    bool

	stop chan struct{}
	done chan struct{}

	// test hooks
	renewEvery time.Duration
	retryEvery time.Duration
}

// acquireStartClaim takes a claim of kind for agent and starts its lease
// renewal. The returned run's Context carries the start deadline and is
// cancelled when the claim is lost. A held stop-kind claim (a queued stop
// being applied) is waited for, up to stopClaimWait. Errors: a
// *store.ClaimHeldError, store.ErrClaimPredicate (the agent is deleted or
// mid-reincarnation), store.ErrDeleteInProgress, or a store error.
func (s *Server) acquireStartClaim(ctx context.Context, agent *store.Agent, kind store.StartClaimKind) (*startClaimRun, error) {
	cfg := s.startClaimSettings()
	target := expectedClaimTarget(agent)
	sent := time.Now()
	claim, err := s.store.ClaimAgentStart(ctx, agent.ID, s.instanceID, kind, target, cfg.LeaseTTL)
	var held *store.ClaimHeldError
	if errors.As(err, &held) && held.Kind == store.StartClaimStop {
		deadline := time.Now().Add(stopClaimWait)
		for errors.As(err, &held) && held.Kind == store.StartClaimStop && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
			sent = time.Now()
			claim, err = s.store.ClaimAgentStart(ctx, agent.ID, s.instanceID, kind, target, cfg.LeaseTTL)
		}
	}
	if err != nil {
		var held *store.ClaimHeldError
		if errors.As(err, &held) || errors.Is(err, store.ErrClaimPredicate) || ctx.Err() != nil {
			return nil, err
		}
		// The claim (the start's run-intent write) failed, or was refused
		// because a delete holds the row, as the running-intent write is.
		return nil, fmt.Errorf("%w: %w", errStartClaimWrite, err)
	}
	agent.RunIntent = store.RunIntentRunning
	at := claim.RunIntentAt
	agent.RunIntentAt = &at
	return s.runStartClaim(ctx, agent.ID, claim, cfg, sent), nil
}

// runStartClaim starts the lease renewal of an acquired claim. sent is the
// time the claim request was sent: the first fence deadline is measured
// from it.
func (s *Server) runStartClaim(parent context.Context, agentID string, claim store.StartClaim, cfg StartClaimSettings, sent time.Time) *startClaimRun {
	r := &startClaimRun{
		s: s, agent: agentID, owner: claim.Owner, claim: claim, cfg: cfg,
		fenceAt:    sent.Add(cfg.LeaseTTL - cfg.LeaseTTL/3),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		renewEvery: cfg.LeaseTTL / 3,
		retryEvery: startClaimRetryInterval,
	}
	if s.startClaimTestHook != nil {
		s.startClaimTestHook(r)
	}
	// A start runs to its outcome even when the request that triggered it
	// ends (a client disconnect would otherwise cancel a dispatch that may
	// already have reached the broker, and leave an unconfirmed claim);
	// start_max_duration bounds it.
	r.ctx, r.cancel = context.WithTimeout(context.WithoutCancel(parent), r.cfg.MaxDuration)
	go r.renewLoop()
	return r
}

// Context is the context the start runs under.
func (r *startClaimRun) Context() context.Context { return r.ctx }

// check reports errStartClaimLost when the claim was lost or its fence
// deadline has passed. A start calls it immediately before dispatching.
func (r *startClaimRun) check() error {
	r.mu.Lock()
	lost, fenced := r.lost, !time.Now().Before(r.fenceAt)
	r.mu.Unlock()
	if fenced && !lost {
		r.markLost("fence deadline passed before dispatch")
	}
	if lost || fenced {
		return errStartClaimLost
	}
	return nil
}

func (r *startClaimRun) markLost(reason string) {
	r.mu.Lock()
	already := r.lost
	r.lost = true
	r.mu.Unlock()
	if !already {
		slog.Warn("Start claim lost; abandoning the start", "agent_id", r.agent, "claim_id", r.claim.ID, "reason", reason)
	}
	r.cancel()
}

func (r *startClaimRun) isLost() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lost
}

// renewLoop renews the lease every renewEvery. Zero rows means the claim is
// lost, at once. An error is unknown and is retried every retryEvery, but
// only until the fence deadline, which also bounds each renew call: at the
// deadline without a successful renew the claim is treated as lost.
func (r *startClaimRun) renewLoop() {
	defer close(r.done)
	wait := r.renewEvery
	for {
		r.mu.Lock()
		fence := r.fenceAt
		r.mu.Unlock()
		if until := time.Until(fence); until < wait {
			wait = until
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-r.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if !time.Now().Before(fence) {
			r.markLost("lease not renewed before the fence deadline")
			return
		}
		if r.ctx.Err() != nil {
			// The start deadline passed: the run context is detached from
			// the triggering request, so only the deadline, a loss
			// (markLost) or finish cancels it. Stop renewing. A dispatch that ignores its context keeps running,
			// but the lease lapses and the reaper makes the claim
			// unconfirmed.
			r.markLost("start deadline passed")
			return
		}
		sent := time.Now()
		callCtx, cancel := context.WithDeadline(context.WithoutCancel(r.ctx), fence)
		held, err := r.s.store.RenewAgentStart(callCtx, r.agent, r.claim.ID, r.owner, r.cfg.LeaseTTL)
		cancel()
		switch {
		case err == nil && held:
			r.mu.Lock()
			r.fenceAt = sent.Add(r.cfg.LeaseTTL - r.cfg.LeaseTTL/3)
			r.mu.Unlock()
			wait = r.renewEvery
		case err == nil:
			r.markLost("claim no longer held")
			return
		default:
			wait = r.retryEvery
		}
	}
}

// finish stops the renewal and settles the claim by outcome. A lost claim
// writes nothing: whoever took it (a stop or the reaper) owns its next
// state. It reports whether the claim was still held.
func (r *startClaimRun) finish(outcome startOutcome) bool {
	close(r.stop)
	<-r.done
	defer r.cancel()
	if r.isLost() {
		return false
	}
	if outcome == startHandedOff {
		return true
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 10*time.Second)
	defer cancel()
	var held bool
	var err error
	if outcome == startUnconfirmed {
		held, err = r.s.store.MarkStartUnconfirmed(ctx, r.agent, r.claim.ID, r.owner, r.cfg.Holds().For(r.claim.Kind))
	} else {
		held, err = r.s.store.ReleaseAgentStart(ctx, r.agent, r.claim.ID, r.owner)
	}
	if err != nil {
		slog.Warn("Start claim: settling the claim failed; the reaper will settle it", "agent_id", r.agent, "claim_id", r.claim.ID, "error", err)
	}
	return held
}

// claimedDispatch runs a start under its claim's context. fence reports
// errStartClaimLost once the claim is lost or its fence deadline passed; the
// dispatch calls it immediately before dispatching. handoff reports an async
// launch that accepted the start (its end settles the claim).
type claimedDispatch func(ctx context.Context, fence func() error) (handoff bool, err error)

// withStartClaim runs dispatch under a start claim of kind for agent: claim
// (or the caller's existing claim), lease renewal, the start deadline, the
// fence check immediately before dispatch, and the outcome. Credential
// handling is the dispatch's own and is unchanged. With start claims off,
// it records run intent running and runs dispatch as before.
func (s *Server) withStartClaim(ctx context.Context, agent *store.Agent, kind store.StartClaimKind, existing *startClaimRun, dispatch claimedDispatch) error {
	if !s.startClaimsEnabled() {
		if _, err := s.recordRunIntent(ctx, agent, store.RunIntentRunning); err != nil {
			return err
		}
		_, err := dispatch(ctx, func() error { return nil })
		return err
	}
	run := existing
	if run == nil {
		var err error
		if run, err = s.acquireStartClaim(ctx, agent, kind); err != nil {
			return err
		}
	}
	// The dispatch checks the fence immediately before dispatching (after
	// any capacity reservation, which it rolls back if the fence fails).
	handoff, err := dispatch(run.Context(), run.check)
	outcome := startOutcomeOf(err)
	if handoff && err == nil {
		outcome = startHandedOff
	}
	run.finish(outcome)
	if err == nil || errors.Is(err, errStartedStatusWrite) {
		s.compensatingStop(ctx, agent, run.claim)
	}
	if run.isLost() {
		// The claim was lost while the start ran: whatever the dispatch
		// returned, the start was abandoned (and may still be running).
		if err != nil {
			return fmt.Errorf("%w: %v", errStartClaimLost, err)
		}
		return errStartClaimLost
	}
	return err
}

// StartOpts are the inputs of startAgentCore.
type StartOpts struct {
	// Kind is the claim kind the start takes.
	Kind store.StartClaimKind
	// Existing is a claim the caller already holds (a restart's, taken
	// before its stop leg); the start runs under it instead of a new one.
	Existing *startClaimRun
	// Task and Resume are passed to DispatchAgentStart.
	Task   string
	Resume bool
	// Dispatch is a broker-capacity hold the caller already took with
	// reserveStartCapacity (a restart, before its stop leg); the start uses
	// it instead of taking one. startAgentCore settles it on success and
	// rolls it back on any failure.
	Dispatch *startDispatch
	// KeepCallerDeadline bounds the dispatch by the caller's context
	// deadline too, when it has one and it is earlier than the claim run's
	// (a direct-message wake bounds its resume this way). Cancellation of
	// the caller's context still does not reach the start.
	KeepCallerDeadline bool
	// NewGeneration clears the previous run's message, stalled marker and
	// exit fields in the post-start write even when the agent was already
	// in a counted phase (a restart). A start from a resting phase always
	// clears them.
	NewGeneration bool
	// AfterStart, when set, runs after a successful dispatch while the claim
	// is still held, in place of the default status write (phase from the
	// broker response, or running; its container status; exit fields
	// cleared). It writes the caller's post-start state, so a newer start's
	// status is never overwritten by this one's.
	AfterStart func(ctx context.Context, st startedState) error
}

// startedState is what AfterStart gets from startAgentCore.
type startedState struct {
	// ClearRemnants reports whether the post-start write must clear the
	// previous run's message, stalled marker and exit fields
	// (AgentStatusUpdate.ClearTerminalRemnants).
	ClearRemnants bool
	// EndOp ends the lifecycle op early (idempotent), for a caller that
	// waits after its dispatch leg and must see heartbeat-reported exits.
	EndOp func()
}

// startAgentCore dispatches a start of agent under a start claim. It is the
// single start path for start triggers, in this order: claim (or the
// caller's existing claim), lease and self-fence; the lifecycle op that
// keeps the heartbeat's missing-container reconcile and uncounted-phase
// reports away from the agent; broker capacity and the starting phase
// (reserveStartCapacity, or the caller's hold); fence check; then
// DispatchAgentStart, whose credential handling is unchanged; then the
// post-start status write and the outcome, before the claim is released;
// then the compensating stop. When the start fails, the capacity hold is
// rolled back (the starting phase restored, a reservation it created
// released).
func (s *Server) startAgentCore(ctx context.Context, agent *store.Agent, opts StartOpts) error {
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		opts.Dispatch.rollback(ctx)
		return errors.New("no dispatcher")
	}
	provisioned := agent.Phase == string(state.PhaseCreated) || agent.Phase == string(state.PhaseProvisioning)
	supersedesQueuedStop := agent.ContainerStatus == containerStatusStopQueued
	sd := opts.Dispatch
	callerDeadline, hasCallerDeadline := ctx.Deadline()
	err := s.withStartClaim(ctx, agent, opts.Kind, opts.Existing, func(ctx context.Context, fence func() error) (bool, error) {
		endOp := s.beginLifecycleOp(agent.ID)
		defer endOp()
		if sd == nil {
			var err error
			if sd, err = s.reserveStartCapacity(ctx, agent); err != nil {
				return false, err
			}
		}
		if err := fence(); err != nil {
			sd.rollback(ctx)
			return false, err
		}
		dctx := ctx
		if opts.KeepCallerDeadline && hasCallerDeadline {
			var cancel context.CancelFunc
			dctx, cancel = context.WithDeadline(ctx, callerDeadline)
			defer cancel()
		}
		if err := dispatcher.DispatchAgentStart(dctx, agent, opts.Task, opts.Resume); err != nil {
			sd.rollback(ctx)
			return false, err
		}
		// The container is up: the reservation is kept whatever the
		// status write below does.
		sd.settle()
		st := startedState{ClearRemnants: opts.NewGeneration || sd.wroteStarting(), EndOp: endOp}
		if opts.AfterStart != nil {
			return false, opts.AfterStart(ctx, st)
		}
		return false, s.writeStartedStatus(ctx, agent, st.ClearRemnants)
	})
	if err != nil && !errors.Is(err, errStartedStatusWrite) {
		// A start refused before its dispatch ran (a held or lost claim, a
		// refused intent) leaves a caller's hold to undo; a no-op once the
		// hold was settled or rolled back.
		sd.rollback(ctx)
	}
	if supersedesQueuedStop && (err == nil || errors.Is(err, errStartedStatusWrite)) {
		s.clearSupersededQueuedStop(ctx, agent)
	}
	if err != nil && provisioned && startDidNotHappen(err) && agent.RunIntentAt != nil {
		s.settleFailedProvisionedStart(ctx, agent.ID, *agent.RunIntentAt, "Start failed: "+err.Error()+". "+provisionedRestingNote)
	}
	return err
}

// provisionedRestingNote ends the message a provisioned agent gets when its
// start did not happen: it is back at rest and can be started again.
const provisionedRestingNote = "The agent is still provisioned and can be started again."

// clearSupersededQueuedStop clears the queued-stop status and notice of an
// agent whose start succeeded after a stop was queued for its offline
// broker: the start superseded the stop. It runs after the start's claim is
// released, so it re-reads the row and clears only while run intent is still
// running and the queued status or notice is still there: a stop recorded
// since keeps its own state. The container status becomes running unless
// the broker reported one.
func (s *Server) clearSupersededQueuedStop(ctx context.Context, agent *store.Agent) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cur, err := s.store.GetAgent(cctx, agent.ID)
	if err != nil || cur.RunIntent != store.RunIntentRunning ||
		(cur.ContainerStatus != containerStatusStopQueued && cur.Message != offlineStopMessage) {
		return
	}
	upd := store.AgentStatusUpdate{ClearMessageIf: offlineStopMessage}
	if cur.ContainerStatus == containerStatusStopQueued {
		upd.ContainerStatus = "running"
	}
	if err := s.store.UpdateAgentStatus(cctx, agent.ID, upd); err != nil {
		slog.Warn("Start: clearing the superseded queued stop failed", "agent_id", agent.ID, "error", err)
		return
	}
	if upd.ContainerStatus != "" {
		agent.ContainerStatus = upd.ContainerStatus
	}
}

// startDidNotHappen reports whether err is a start that definitely never
// reached a runtime: refused before dispatch for capacity, missing
// environment, a credential that could not be issued, a broker without the
// required capability, a create left incomplete, a phase that no longer
// allows a launch, or a broker that confirmed it did not act. Anything else,
// including a start that succeeded but whose status write failed, an
// ambiguous dispatch error, a claim refusal (the earlier claimant owns the
// intent) or a launch in flight (which may still start a container), is not.
func startDidNotHappen(err error) bool {
	if err == nil || errors.Is(err, errStartedStatusWrite) || errors.Is(err, errStartClaimLost) {
		return false
	}
	var quotaErr *startQuotaError
	var stillMissing *ErrEnvStillMissing
	var tokenErr *agentTokenIssueError
	var incomplete *AgentCreateIncompleteError
	return errors.As(err, &quotaErr) || errors.As(err, &stillMissing) || errors.As(err, &tokenErr) ||
		errors.Is(err, errBrokerLacksEmptyPerAgent) || errors.As(err, &incomplete) ||
		errors.Is(err, ErrLaunchInvalidPhase) || isConfirmedStartNotActedOnError(err)
}

// settleFailedProvisionedStart returns a provisioned agent (phase created)
// whose start definitely did not happen to its resting state: run intent
// goes back to stopped, compare-and-set on the intent the start recorded at
// intentAt, and only when that wins the message says why. A newer start or
// stop since keeps its intent and gets no message.
func (s *Server) settleFailedProvisionedStart(ctx context.Context, agentID string, intentAt time.Time, message string) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	reverted, err := s.store.RevertRunIntent(sctx, agentID, store.RunIntentRunning, intentAt, store.RunIntentStopped)
	if err != nil {
		slog.Warn("Failed provisioned start: intent revert failed", "agent_id", agentID, "error", err)
		return
	}
	if !reverted {
		return
	}
	if err := s.store.UpdateAgentStatus(sctx, agentID, store.AgentStatusUpdate{Message: message}); err != nil {
		slog.Warn("Failed provisioned start: message write failed", "agent_id", agentID, "error", err)
	}
}

// writeStartedStatus is startAgentCore's default post-start write: the
// phase the broker reported (running when it reported none or a resting
// phase), its container status, and the previous run's exit fields cleared
// (including a disruption reason recorded while the old pod still ran).
// clearRemnants also clears the previous run's message and stalled marker,
// which the store's stopped/error to running clear no longer does once the
// row reads starting.
func (s *Server) writeStartedStatus(ctx context.Context, agent *store.Agent, clearRemnants bool) error {
	phase := agent.Phase
	switch state.Phase(phase) {
	case "", state.PhaseCreated, state.PhaseProvisioning, state.PhaseStopped, state.PhaseSuspended, state.PhaseError, state.PhaseStarting:
		phase = string(state.PhaseRunning)
	}
	upd := store.AgentStatusUpdate{Phase: phase, ClearExit: true, ClearTerminalRemnants: clearRemnants}
	if agent.ContainerStatus != "" {
		upd.ContainerStatus = agent.ContainerStatus
	}
	if err := s.store.UpdateAgentStatus(ctx, agent.ID, upd); err != nil {
		return fmt.Errorf("%w: %v", errStartedStatusWrite, err)
	}
	agent.Phase = phase
	return nil
}

// errStartClaimWrite marks a start claim the store did not record: the
// write failed, or a delete holds the row (store.ErrDeleteInProgress stays
// in the chain). For a claimed start, its run-intent write failed or was
// refused, and nothing was dispatched.
var errStartClaimWrite = errors.New("record the start claim")

// errStartedStatusWrite marks a start that succeeded but whose status write
// failed: the agent is starting, so for the claim the start succeeded.
var errStartedStatusWrite = errors.New("agent started but its status could not be recorded")

// startQuotaError is a start refused because the broker is at capacity (or
// the capacity check failed); nothing was dispatched.
type startQuotaError struct{ err error }

func (e *startQuotaError) Error() string { return "start refused by broker capacity: " + e.err.Error() }
func (e *startQuotaError) Unwrap() error { return e.err }

// reserveStartCapacity takes the broker capacity hold a start needs: it
// reserves max_agents_per_broker (idempotent for an agent that already
// holds it) and marks an agent in an uncounted phase starting for the
// dispatch (beginStartDispatch). The caller must hold a lifecycle op. A
// capacity refusal is a startQuotaError; a failed starting write wraps
// errStartingWrite (and store.ErrDeleteInProgress when a delete now holds
// the row). It is the one place the start path takes capacity.
func (s *Server) reserveStartCapacity(ctx context.Context, agent *store.Agent) (*startDispatch, error) {
	sd, err := s.beginStartDispatch(ctx, agent)
	if err != nil {
		if errors.Is(err, errStartingWrite) {
			return nil, err
		}
		return nil, &startQuotaError{err: err}
	}
	return sd, nil
}

// writeStartQuotaError writes the answer for a start refused by broker
// capacity, as the create path's quota check does, and reports whether it
// did.
func writeStartQuotaError(w http.ResponseWriter, err error) bool {
	var qe *startQuotaError
	if !errors.As(err, &qe) {
		return false
	}
	writeQuotaReserveError(w, store.LimitMaxAgentsPerBroker, err)
	return true
}

// compensatingStoreTimeout bounds each store call of a compensating stop,
// which runs detached from the triggering request.
const compensatingStoreTimeout = 15 * time.Second

// compensatingStop stops an agent whose start succeeded after a stop was
// accepted (run intent stopped, written after the claim was taken): the
// start may have reached the broker after the stop did. It runs after the
// start's claim is settled, under a stop-kind claim pinned to the stop's
// intent time: a start accepted meanwhile wrote a newer intent (or holds a
// claim), so the stop claim is refused and nothing is stopped.
func (s *Server) compensatingStop(ctx context.Context, agent *store.Agent, claim store.StartClaim) {
	bg := context.WithoutCancel(ctx)
	readCtx, cancelRead := context.WithTimeout(bg, compensatingStoreTimeout)
	cur, err := s.store.GetAgent(readCtx, agent.ID)
	cancelRead()
	if err != nil || cur.RunIntent != store.RunIntentStopped || cur.RunIntentAt == nil || !cur.RunIntentAt.After(claim.At) {
		return
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(bg, 2*time.Minute)
	defer cancel()
	stop, err := s.store.ClaimAgentStop(stopCtx, agent.ID, s.instanceID, *cur.RunIntentAt, s.startClaimSettings().LeaseTTL)
	if err != nil {
		var held *store.ClaimHeldError
		if !errors.Is(err, store.ErrClaimPredicate) && !errors.As(err, &held) {
			// The claim could not be read or written. The agent is left to
			// the hub's backstop, which stops an agent the broker reports
			// running with run intent stopped and no claim.
			slog.Warn("Compensating stop skipped: its claim could not be taken; the backstop stop applies", "agent_id", agent.ID, "error", err)
		}
		return // otherwise a newer start or stop since: nothing to compensate
	}
	defer func() {
		releaseCtx, cancelRelease := context.WithTimeout(bg, compensatingStoreTimeout)
		defer cancelRelease()
		if _, err := s.store.ReleaseAgentStart(releaseCtx, agent.ID, stop.ID, s.instanceID); err != nil {
			slog.Warn("Compensating stop: releasing its claim failed; the reaper will settle it", "agent_id", agent.ID, "error", err)
		}
	}()
	slog.Info("Stopping an agent whose start completed after a stop was accepted", "agent_id", agent.ID)
	if err := dispatcher.DispatchAgentStop(stopCtx, cur); err != nil {
		slog.Warn("Compensating stop failed", "agent_id", agent.ID, "error", err)
	}
}

// ErrCodeStartInProgress is the 409 code for a start, restart or resume
// refused because another start of the agent holds its start claim.
const ErrCodeStartInProgress = "start_in_progress"

// startClaimHolderNames are the user-facing names of claim kinds.
var startClaimHolderNames = map[store.StartClaimKind]string{
	store.StartClaimUser:        "a user start",
	store.StartClaimRestart:     "a restart",
	store.StartClaimWake:        "a start for a direct message",
	store.StartClaimCreate:      "agent creation",
	store.StartClaimRecovery:    "automatic recovery",
	store.StartClaimReincarnate: "a reincarnation",
	store.StartClaimStop:        "a stop",
}

// expectedClaimRelease estimates when a held claim ends: the end of the
// hold for an unconfirmed claim, or the start deadline for a live one.
func (s *Server) expectedClaimRelease(held *store.ClaimHeldError) time.Time {
	cfg := s.startClaimSettings()
	a := &store.Agent{
		StartClaimID: held.ClaimID, StartClaimKind: held.Kind, StartClaimState: held.State,
		StartClaimUnconfirmedAt: held.UnconfirmedAt, StartClaimHoldUntil: held.HoldUntil,
	}
	if exp, ok := cfg.Holds().HoldExpiry(a); ok {
		return exp
	}
	return held.Since.Add(cfg.MaxDuration)
}

// writeStartInProgress writes the 409 start_in_progress answer when err is
// a held start claim, and reports whether it did.
func (s *Server) writeStartInProgress(w http.ResponseWriter, err error) bool {
	var held *store.ClaimHeldError
	if !errors.As(err, &held) {
		return false
	}
	release := s.expectedClaimRelease(held)
	holder := startClaimHolderNames[held.Kind]
	if holder == "" {
		holder = string(held.Kind)
	}
	msg := fmt.Sprintf("A start is already in progress for this agent (%s, since %s). It will finish or be released by %s; run scion stop to cancel it.",
		holder, held.Since.UTC().Format("15:04"), release.UTC().Format("15:04"))
	if held.Kind == store.StartClaimStop {
		// A queued stop being applied, or the reaper stopping a container an
		// unconfirmed start left running: released once the broker reports
		// the agent stopped (kept while the broker is offline).
		msg = fmt.Sprintf("A stop is in progress for this agent (since %s). It is released when the broker reports the agent stopped; start it again then.",
			held.Since.UTC().Format("15:04"))
	}
	writeError(w, http.StatusConflict, ErrCodeStartInProgress, msg, map[string]interface{}{
		"holderKind":      string(held.Kind),
		"state":           string(held.State),
		"since":           held.Since.UTC(),
		"expectedRelease": release.UTC(),
	})
	return true
}

// writeStartClaimError writes the answer for a start refused by its claim
// or before its dispatch: delete_in_progress when a delete holds the row (as
// for a refused running intent), the starting write's error (409 when the
// stored phase moved on), 409 start_in_progress for a held claim, 409
// conflict when the agent is not eligible (being deleted, or a reincarnation
// in flight), 409 conflict when the claim was lost to a stop. It reports
// whether it wrote.
func (s *Server) writeStartClaimError(w http.ResponseWriter, err error, agentID string) bool {
	if ref := deleteClaimedDuringDispatch(err, agentID); ref != nil {
		ref.write(w)
		return true
	}
	switch {
	case errors.Is(err, errStartingWrite):
		// The starting write before dispatch failed (409 when the stored
		// phase moved on), as beginStartDispatchHTTP answers.
		writeErrorFromErr(w, err, "")
		return true
	case s.writeStartInProgress(w, err):
		return true
	case errors.Is(err, store.ErrClaimPredicate):
		Conflict(w, "the agent cannot be started now: it is being deleted or reincarnated")
		return true
	case errors.Is(err, errStartClaimLost):
		Conflict(w, "the start was abandoned: a stop or another start superseded it")
		return true
	}
	return false
}

// createUnderClaim runs a create-and-start dispatch under a start claim of
// kind create. An accepted async launch takes the claim over: the launch's
// end settles it.
func (s *Server) createUnderClaim(ctx context.Context, agent *store.Agent, dispatch func(ctx context.Context) (*CreateDispatchResult, error)) (*CreateDispatchResult, error) {
	var created *CreateDispatchResult
	err := s.withStartClaim(ctx, agent, store.StartClaimCreate, nil, func(ctx context.Context, fence func() error) (bool, error) {
		if err := fence(); err != nil {
			return false, err
		}
		var err error
		created, err = dispatch(ctx)
		return err == nil && created.AcceptedLaunch() != nil, err
	})
	return created, err
}

// releaseSupersededClaim releases claimID, the start claim an agent held
// when a stop was recorded at stopIntentAt, once that stop's dispatch
// succeeded. A claim taken after the stop wrote a newer intent and is kept;
// a queued stop's own stop-kind claim is kept. A holder still renewing
// finds the claim gone and abandons its start.
func (s *Server) releaseSupersededClaim(ctx context.Context, agentID, claimID string, stopIntentAt time.Time) {
	if claimID == "" || stopIntentAt.IsZero() {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if released, err := s.store.ReleaseSupersededStart(rctx, agentID, claimID, stopIntentAt); err != nil {
		slog.Warn("Releasing a start claim superseded by a stop failed; the reaper will settle it", "agent_id", agentID, "error", err)
	} else if released {
		slog.Info("Released a start claim superseded by a stop", "agent_id", agentID, "claim_id", claimID)
	}
}
