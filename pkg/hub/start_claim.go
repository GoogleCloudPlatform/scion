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
	"strings"
	"sync"
	"time"

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
	switch {
	case errors.Is(err, ErrLaunchInFlight), errors.As(err, &incomplete),
		errors.As(err, &tokenErr), errors.Is(err, errBrokerLacksEmptyPerAgent),
		isBrokerRuntimeUnavailable(err), errors.Is(err, errStartClaimLost):
		return startReleased
	}
	return startUnconfirmed
}

// startClaimsEnabled reports whether starts take start claims. Off until
// every start trigger runs under one.
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
// cancelled when the claim is lost. A claim predicate mismatch is retried
// once after re-reading the agent; a held stop-kind claim (a queued stop
// being applied) is waited for, up to stopClaimWait. Errors: a
// *store.ClaimHeldError, store.ErrClaimPredicate, or a store error.
func (s *Server) acquireStartClaim(ctx context.Context, agent *store.Agent, kind store.StartClaimKind) (*startClaimRun, error) {
	cfg := s.startClaimSettings()
	target := expectedClaimTarget(agent)
	sent := time.Now()
	claim, err := s.store.ClaimAgentStart(ctx, agent.ID, s.instanceID, kind, target, cfg.LeaseTTL)
	if errors.Is(err, store.ErrClaimPredicate) {
		sent = time.Now()
		claim, err = s.store.ClaimAgentStart(ctx, agent.ID, s.instanceID, kind, target, cfg.LeaseTTL)
	}
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
		return nil, err
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
	r.ctx, r.cancel = context.WithTimeout(parent, r.cfg.MaxDuration)
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

// startDispatch runs a start under its claim's context. handoff reports an
// async launch that accepted the start (its end settles the claim).
type startDispatch func(ctx context.Context) (handoff bool, err error)

// withStartClaim runs dispatch under a start claim of kind for agent: claim
// (or the caller's existing claim), lease renewal, the start deadline, the
// fence check immediately before dispatch, and the outcome. Credential
// handling is the dispatch's own and is unchanged. With start claims off,
// it records run intent running and runs dispatch as before.
func (s *Server) withStartClaim(ctx context.Context, agent *store.Agent, kind store.StartClaimKind, existing *startClaimRun, dispatch startDispatch) error {
	if !s.startClaimsEnabled() {
		if _, err := s.recordRunIntent(ctx, agent, store.RunIntentRunning); err != nil {
			return err
		}
		_, err := dispatch(ctx)
		return err
	}
	run := existing
	if run == nil {
		var err error
		if run, err = s.acquireStartClaim(ctx, agent, kind); err != nil {
			return err
		}
	}
	if err := run.check(); err != nil {
		run.finish(startReleased)
		return err
	}
	handoff, err := dispatch(run.Context())
	outcome := startOutcomeOf(err)
	if handoff && err == nil {
		outcome = startHandedOff
	}
	lost := run.isLost()
	run.finish(outcome)
	if err == nil {
		s.compensatingStop(ctx, agent, run.claim)
	}
	if lost && err != nil {
		return fmt.Errorf("%w: %v", errStartClaimLost, err)
	}
	return err
}

// startAgentCore dispatches a start of agent under a start claim. It is the
// single start path for start triggers; credential handling is
// DispatchAgentStart's own.
func (s *Server) startAgentCore(ctx context.Context, agent *store.Agent, kind store.StartClaimKind, existing *startClaimRun, task string, resume bool) error {
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return errors.New("no dispatcher")
	}
	return s.withStartClaim(ctx, agent, kind, existing, func(ctx context.Context) (bool, error) {
		return false, dispatcher.DispatchAgentStart(ctx, agent, task, resume)
	})
}

// compensatingStop stops an agent whose start succeeded after a stop was
// accepted (run intent stopped, written after the claim was taken): the
// start may have reached the broker after the stop did.
func (s *Server) compensatingStop(ctx context.Context, agent *store.Agent, claim store.StartClaim) {
	cur, err := s.store.GetAgent(context.WithoutCancel(ctx), agent.ID)
	if err != nil || cur.RunIntent != store.RunIntentStopped || cur.RunIntentAt == nil || !cur.RunIntentAt.After(claim.At) {
		return
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	slog.Info("Stopping an agent whose start completed after a stop was accepted", "agent_id", agent.ID)
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := dispatcher.DispatchAgentStop(stopCtx, cur); err != nil {
		slog.Warn("Compensating stop failed", "agent_id", agent.ID, "error", err)
	}
}
