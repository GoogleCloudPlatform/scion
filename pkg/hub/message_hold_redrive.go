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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Fixed failure reasons for held messages (ptone/scion#7881, A2-S8, OPT-5).
// Never store or echo broker-supplied text or failure codes on held-row
// expiry, re-validation, or re-drive failures.
const (
	holdFailReasonSessionTimeout       = "agent session did not connect"
	holdFailReasonAgentStopped         = "agent stopped"
	holdFailReasonCannotReceiveConduit = "agent cannot receive messages over conduit"
	holdFailReasonAgentRestarted       = "agent restarted before delivery"
	holdFailReasonSenderUnauthorized   = "sender is no longer authorized"
	holdFailReasonMessageUnrecoverable = "message cannot be re-delivered"
	holdFailReasonBrokerDeliveryFailed = "broker delivery failed"
)

const (
	// messageHoldClaimBatchSize matches the Conduit deliver-message route's
	// 64-message-ID batch bound (design.md Phase A2 §3, AC A2-3b, AC A2-6).
	messageHoldClaimBatchSize = agentcontrol.MaxDeliverMessageIDs

	// messageHoldRedriveTimeout bounds a single detached per-agent re-drive pass.
	messageHoldRedriveTimeout = 90 * time.Second

	// messageHoldRedriveLeaseTTL bounds the DB-backed cross-replica per-agent
	// re-drive lease (REQ-2, O-1). Exceeds messageHoldRedriveTimeout (90s) and is
	// also renewed mid-batch and before/after each inter-batch settle wait so a
	// slow pass never loses its lease mid-drain, while still expiring on crash.
	messageHoldRedriveLeaseTTL = 2 * time.Minute

	// messageHoldSweepTimeout bounds one 30s backstop sweep pass.
	messageHoldSweepTimeout = 20 * time.Second

	// messageHoldBatchPollInterval is the initial inter-batch settling poll
	// interval, backing off exponentially up to messageHoldBatchPollMaxInterval (OPT-3).
	messageHoldBatchPollInterval    = 25 * time.Millisecond
	messageHoldBatchPollMaxInterval = 250 * time.Millisecond

	// messageHoldStopGrace bounds how long CleanupBackgroundResources waits
	// for in-flight re-drive passes before cancelling abortCtx.
	messageHoldStopGrace = 10 * time.Second
)

var (
	// messageHoldSweepTick is the 30s backstop sweep interval (design.md §3/§4).
	messageHoldSweepTick = 30 * time.Second

	// messageHoldBatchSettleCap bounds inter-batch waiting when draining >64
	// held rows so batches arrive in order without hanging indefinitely.
	messageHoldBatchSettleCap = 30 * time.Second
)

// messageHoldRuntime coordinates coalesced per-agent re-drive workers and the
// 30s backstop sweep on a Server replica.
type messageHoldRuntime struct {
	mu               sync.Mutex
	stopped          bool
	stopLoop         context.CancelFunc
	abortCtx         context.Context
	abort            context.CancelFunc
	inFlight         map[string]bool
	pending          map[string]bool
	running          sync.WaitGroup
	maxHeldOverride  int           // >0 overrides maxHeldMessagesPerAgent in tests
	leaseTTLOverride time.Duration // >0 overrides messageHoldRedriveLeaseTTL in tests
}

func (s *Server) holdRuntime() *messageHoldRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messageHold == nil {
		abortCtx, abort := context.WithCancel(context.Background())
		s.messageHold = &messageHoldRuntime{
			abortCtx: abortCtx,
			abort:    abort,
			inFlight: make(map[string]bool),
			pending:  make(map[string]bool),
		}
	}
	return s.messageHold
}

// heldMessagesCap returns the per-agent held-message cap (default 100).
func (s *Server) heldMessagesCap() int {
	if s == nil {
		return maxHeldMessagesPerAgent
	}
	rt := s.holdRuntime()
	rt.mu.Lock()
	override := rt.maxHeldOverride
	rt.mu.Unlock()
	if override > 0 {
		return override
	}
	return maxHeldMessagesPerAgent
}

func (s *Server) redriveLeaseTTL() time.Duration {
	if s == nil {
		return messageHoldRedriveLeaseTTL
	}
	rt := s.holdRuntime()
	rt.mu.Lock()
	override := rt.leaseTTLOverride
	rt.mu.Unlock()
	if override > 0 {
		return override
	}
	return messageHoldRedriveLeaseTTL
}

// heldMessageTargetPhaseEligible reports whether an agent in phase may keep
// holding messages awaiting a Conduit session: provisioning, starting, or running.
func heldMessageTargetPhaseEligible(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseProvisioning, state.PhaseStarting, state.PhaseRunning:
		return true
	default:
		return false
	}
}

type agentDeliverSessionState int

const (
	agentDeliverSessionNone agentDeliverSessionState = iota
	agentDeliverSessionReady
	agentDeliverSessionUnsupported
)

// agentConduitDeliverSessionState evaluates whether agent currently has an
// eligible Conduit session across the cluster-wide session registry, and
// whether that session advertises the deliver-message control route (A2-S7).
func (s *Server) agentConduitDeliverSessionState(ctx context.Context, agent *store.Agent) (agentDeliverSessionState, error) {
	if s == nil || agent == nil || !s.conduitRoutable() {
		return agentDeliverSessionNone, nil
	}
	rtr := s.conduitRouter()
	if rtr == nil {
		return agentDeliverSessionNone, nil
	}
	res, err := rtr.Resolve(ctx, router.Request{
		Op:    router.OpStatefulRPC,
		Kind:  registry.PrincipalAgent,
		ID:    agent.ID,
		Want:  registry.Want{ProjectID: agent.ProjectID},
		Agent: agentIncarnationFacts(agent),
	}, nil)
	if err != nil {
		if errors.Is(err, router.ErrNoSession) {
			return agentDeliverSessionNone, nil
		}
		return agentDeliverSessionNone, err
	}
	if slices.Contains(res.Record.Capabilities.RPC, agentcontrol.RouteDeliverMessage) {
		return agentDeliverSessionReady, nil
	}
	return agentDeliverSessionUnsupported, nil
}

// onAgentConduitSessionAdmitted is the in-process Conduit relay callback invoked
// once an agent session is registered and passes revalidation.
func (s *Server) onAgentConduitSessionAdmitted(agentID string, rpcRoutes []string) {
	if s == nil || s.store == nil || agentID == "" || !s.conduitDeliverEnabled() {
		return
	}
	if slices.Contains(rpcRoutes, agentcontrol.RouteDeliverMessage) {
		s.scheduleHeldMessageRedrive(agentID)
		return
	}
	// Session admitted without deliver-message advertised: fail held rows if
	// no other eligible session for this agent advertises deliver-message.
	rt := s.holdRuntime()
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.running.Add(1)
	abortCtx := rt.abortCtx
	rt.mu.Unlock()
	go func() {
		defer rt.running.Done()
		ctx, cancel := context.WithTimeout(abortCtx, 10*time.Second)
		defer cancel()
		hasHold, err := s.store.HasAgentDeliveryHold(ctx, agentID)
		if err != nil || !hasHold {
			return
		}
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || agent == nil {
			return
		}
		st, err := s.agentConduitDeliverSessionState(ctx, agent)
		if err == nil && st == agentDeliverSessionUnsupported {
			s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonCannotReceiveConduit)
		}
	}()
}

// scheduleExternalRelayAgentAdmissionNudge nudges held-message re-drive shortly
// after an external relay admits an agent handshake (allowing the relay a brief
// moment to insert the PrincipalSession row into the shared registry).
func (s *Server) scheduleExternalRelayAgentAdmissionNudge(agentID string) {
	if s == nil || s.store == nil || agentID == "" || !s.conduitDeliverEnabled() {
		return
	}
	rt := s.holdRuntime()
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.running.Add(1)
	abortCtx := rt.abortCtx
	rt.mu.Unlock()
	go func() {
		defer rt.running.Done()
		for _, delay := range []time.Duration{50 * time.Millisecond, 250 * time.Millisecond} {
			timer := time.NewTimer(delay)
			select {
			case <-abortCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			ctx, cancel := context.WithTimeout(abortCtx, 5*time.Second)
			hasHold, err := s.store.HasAgentDeliveryHold(ctx, agentID)
			cancel()
			if err != nil || !hasHold {
				return
			}
			s.maybeScheduleHeldMessageRedriveIfConnected(agentID)
		}
	}()
}

// maybeScheduleHeldMessageRedriveIfConnected triggers re-drive when an active
// conduit session advertising deliver-message is already connected for agentID
// (closing M-09 when a session connects while the hold CAS is in flight).
func (s *Server) maybeScheduleHeldMessageRedriveIfConnected(agentID string) {
	if s == nil || s.store == nil || agentID == "" || !s.conduitDeliverEnabled() || !s.conduitRoutable() {
		return
	}
	rt := s.holdRuntime()
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	rt.running.Add(1)
	abortCtx := rt.abortCtx
	rt.mu.Unlock()
	go func() {
		defer rt.running.Done()
		ctx, cancel := context.WithTimeout(abortCtx, 10*time.Second)
		defer cancel()
		hasHold, err := s.store.HasAgentDeliveryHold(ctx, agentID)
		if err != nil || !hasHold {
			return
		}
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || agent == nil {
			return
		}
		st, err := s.agentConduitDeliverSessionState(ctx, agent)
		if err != nil {
			return
		}
		switch st {
		case agentDeliverSessionReady:
			if agent.IdleSuspendClaimActive {
				return
			}
			s.scheduleHeldMessageRedrive(agentID)
		case agentDeliverSessionUnsupported:
			s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonCannotReceiveConduit)
		}
	}()
}

// scheduleHeldMessageRedrive coalesces re-drive triggers per agent (singleflight
// with one queued follow-up pass when nudged mid-run; AC A2-6, A2-S5) and runs
// off the request path on a detached, bounded context.
func (s *Server) scheduleHeldMessageRedrive(agentID string) {
	if s == nil || s.store == nil || agentID == "" {
		return
	}
	rt := s.holdRuntime()
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		return
	}
	if rt.inFlight[agentID] {
		rt.pending[agentID] = true
		rt.mu.Unlock()
		return
	}
	rt.inFlight[agentID] = true
	rt.running.Add(1)
	abortCtx := rt.abortCtx
	rt.mu.Unlock()

	go func() {
		defer rt.running.Done()
		for {
			s.redriveAgentHeldMessagesOnce(abortCtx, agentID)

			rt.mu.Lock()
			if rt.stopped || !rt.pending[agentID] {
				delete(rt.inFlight, agentID)
				delete(rt.pending, agentID)
				rt.mu.Unlock()
				return
			}
			delete(rt.pending, agentID)
			rt.mu.Unlock()
		}
	}()
}

// startMessageHoldSweeper starts the 30s backstop sweep for held messages.
func (s *Server) startMessageHoldSweeper(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	rt := s.holdRuntime()
	loopCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if rt.stopped {
		rt.mu.Unlock()
		cancel()
		return
	}
	if rt.stopLoop != nil {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.stopLoop = cancel
	rt.running.Add(1)
	rt.mu.Unlock()

	go func() {
		defer rt.running.Done()
		ticker := time.NewTicker(messageHoldSweepTick)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				rt.mu.Lock()
				if rt.stopped {
					rt.mu.Unlock()
					return
				}
				rt.mu.Unlock()
				s.sweepHeldMessages(loopCtx)
			}
		}
	}()
}

// stopMessageHoldSweeper stops the 30s sweep and waits (up to messageHoldStopGrace
// or ctx cancellation) for in-flight re-drive passes to finish before aborting them.
func (s *Server) stopMessageHoldSweeper(ctx context.Context) {
	if s == nil {
		return
	}
	rt := s.holdRuntime()
	rt.mu.Lock()
	rt.stopped = true
	stopLoop := rt.stopLoop
	abort := rt.abort
	rt.mu.Unlock()
	if stopLoop != nil {
		stopLoop()
	}

	done := make(chan struct{})
	go func() {
		rt.running.Wait()
		close(done)
	}()

	graceTimer := time.NewTimer(messageHoldStopGrace)
	defer graceTimer.Stop()
	select {
	case <-done:
		if abort != nil {
			abort()
		}
		return
	case <-ctx.Done():
	case <-graceTimer.C:
	}
	if abort != nil {
		abort()
	}
	<-done
}

// sweepHeldMessages runs one backstop sweep pass:
//  1. Expires rows past hold_until (DB time, conditional on awaiting_session)
//     with fixed reason "agent session did not connect" and notifies senders.
//     Runs regardless of hub.conduit_deliver so turning the flag off never
//     strands previously held rows (REQ-5).
//  2. Inspects remaining agent_delivery_holds markers: fails stopped/deleted/held
//     targets ("agent stopped") regardless of flag, and when hub.conduit_deliver
//     is enabled fails connected sessions without deliver-message ("agent cannot
//     receive messages over conduit") or schedules re-drive for agents with an
//     eligible cluster-wide deliver-message session (A2-S7).
func (s *Server) sweepHeldMessages(parentCtx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parentCtx, messageHoldSweepTimeout)
	defer cancel()

	expired, err := s.store.ExpireHeldMessagesPastDeadline(ctx, holdFailReasonSessionTimeout, 500)
	if err != nil {
		slog.Error("message-hold: expire due held messages failed", "error", err)
	} else if len(expired) > 0 {
		if s.messageLog != nil {
			s.messageLog.Warn("expired held messages past hold_until",
				"expired_count", len(expired),
				"reason", holdFailReasonSessionTimeout,
			)
		}
		s.notifyExpiredHeldMessages(ctx, expired, holdFailReasonSessionTimeout)
	}

	holdAgentIDs, err := s.store.ListAgentDeliveryHoldAgentIDs(ctx)
	if err != nil {
		slog.Error("message-hold: list agent delivery holds failed", "error", err)
		return
	}
	conduitDeliverOn := s.conduitDeliverEnabled()
	for _, agentID := range holdAgentIDs {
		if ctx.Err() != nil {
			return
		}
		agent, gerr := s.store.GetAgent(ctx, agentID)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonAgentStopped)
			}
			continue
		}
		eligible, terr := s.heldTargetEligible(ctx, agent)
		if terr != nil {
			continue
		}
		if !eligible {
			s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonAgentStopped)
			continue
		}
		if agent.IdleSuspendClaimActive || !heldMessageTargetPhaseEligible(agent.Phase) {
			continue
		}
		if !conduitDeliverOn {
			continue
		}
		sessState, serr := s.agentConduitDeliverSessionState(ctx, agent)
		if serr != nil {
			continue
		}
		switch sessState {
		case agentDeliverSessionReady:
			s.scheduleHeldMessageRedrive(agentID)
		case agentDeliverSessionUnsupported:
			s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonCannotReceiveConduit)
		}
	}
}

// expireHeldMessagesForAgent fails all awaiting_session rows for agentID with
// fixedReason, deletes its agent_delivery_holds marker, and sends sender
// failure notices. Runs a cheap HasAgentDeliveryHold PK check first so normal
// lifecycle transitions on agents without held messages incur negligible work.
func (s *Server) expireHeldMessagesForAgent(ctx context.Context, agentID, fixedReason string) {
	if s == nil || s.store == nil || agentID == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Recovered panic expiring held messages for agent",
				"agent_id", agentID, "reason", fixedReason, "panic", r)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()

	hasHold, err := s.store.HasAgentDeliveryHold(finCtx, agentID)
	if err != nil {
		slog.Error("message-hold: check delivery hold failed", "agent_id", agentID, "error", err)
		return
	}
	if !hasHold {
		return
	}
	expired, err := s.store.ExpireHeldMessagesForAgent(finCtx, agentID, fixedReason)
	if err != nil {
		slog.Error("message-hold: expire held messages for agent failed",
			"agent_id", agentID, "reason", fixedReason, "error", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	if s.messageLog != nil {
		s.messageLog.Info("expired held messages for agent",
			"agent_id", agentID,
			"expired_count", len(expired),
			"reason", fixedReason,
		)
	}
	s.notifyExpiredHeldMessages(finCtx, expired, fixedReason)
}

// expirePastDeadlineHeldMessagesForAgent expires past-deadline awaiting_session
// rows for agentID inline when a re-drive pass claims nothing (RV3-R1), sending
// sender failure notices with the fixed session-timeout reason instead of
// leaving stale markers until the 30s backstop sweep.
func (s *Server) expirePastDeadlineHeldMessagesForAgent(ctx context.Context, agentID string) {
	if s == nil || s.store == nil || agentID == "" {
		return
	}
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()

	expired, err := s.store.ExpireHeldMessagesPastDeadlineForAgent(finCtx, agentID, holdFailReasonSessionTimeout)
	if err != nil {
		slog.Error("message-hold: expire due held messages for agent failed",
			"agent_id", agentID, "error", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	if s.messageLog != nil {
		s.messageLog.Warn("expired held messages past hold_until",
			"agent_id", agentID,
			"expired_count", len(expired),
			"reason", holdFailReasonSessionTimeout,
		)
	}
	s.notifyExpiredHeldMessages(finCtx, expired, holdFailReasonSessionTimeout)
}

func (s *Server) notifyExpiredHeldMessages(ctx context.Context, expired []store.Message, reason string) {
	agentCache := make(map[string]*store.Agent)
	for i := range expired {
		msg := &expired[i]
		targetID := msg.AgentID
		if targetID == "" {
			targetID = msg.RecipientID
		}
		agent, seen := agentCache[targetID]
		if !seen && targetID != "" {
			a, err := s.store.GetAgent(ctx, targetID)
			if err == nil {
				agent = a
			}
			agentCache[targetID] = agent
		}
		s.notifyStoredMessageSenderFailed(ctx, agent, msg, reason)
	}
}

func heldTargetIdlePreserved(target *store.Agent) bool {
	if target == nil {
		return false
	}
	if target.IdleSuspendClaimActive {
		return true
	}
	return target.Phase == string(state.PhaseSuspended) &&
		target.SuspendReason == string(state.SuspendReasonIdle)
}

func (s *Server) heldTargetEligible(ctx context.Context, target *store.Agent) (bool, error) {
	if target == nil || !target.DeletedAt.IsZero() ||
		target.DeletionHoldsRow(time.Now()) ||
		reincarnationInFlight(target) ||
		(!heldMessageTargetPhaseEligible(target.Phase) && !heldTargetIdlePreserved(target)) {
		return false, nil
	}
	held, err := s.agentHeld(ctx, target.ID)
	if err != nil {
		return false, err
	}
	if held {
		return false, nil
	}
	return true, nil
}

type claimedRowOutcome int

const (
	claimedRowDispatched claimedRowOutcome = iota
	claimedRowFailed
	claimedRowReheldTransient
)

// redriveAgentHeldMessagesOnce executes a bounded, ordered re-drive pass for
// agentID: cheap marker precheck, session eligibility check, 64-row atomic
// ClaimHeldMessages batches, per-row re-validation (A2-S4, R2, R3), envelope
// reconstruction via storedMessageDispatch (A2-S1), back-to-back dispatch, and
// inter-batch settling (AC A2-3b).
func (s *Server) redriveAgentHeldMessagesOnce(parentCtx context.Context, agentID string) {
	if s == nil || s.store == nil || agentID == "" || !s.conduitDeliverEnabled() {
		return
	}
	ctx, cancel := context.WithTimeout(parentCtx, messageHoldRedriveTimeout)
	defer cancel()

	// Cheap PK check first so flapping sessions without held rows cost one read (A2-S5).
	hasHold, err := s.store.HasAgentDeliveryHold(ctx, agentID)
	if err != nil || !hasHold {
		return
	}

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonAgentStopped)
		}
		return
	}
	eligible, err := s.heldTargetEligible(ctx, agent)
	if err != nil {
		return
	}
	if !eligible {
		s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonAgentStopped)
		return
	}
	if agent.IdleSuspendClaimActive || !heldMessageTargetPhaseEligible(agent.Phase) {
		return
	}

	sessState, err := s.agentConduitDeliverSessionState(ctx, agent)
	if err != nil {
		return
	}
	switch sessState {
	case agentDeliverSessionUnsupported:
		s.expireHeldMessagesForAgent(ctx, agentID, holdFailReasonCannotReceiveConduit)
		return
	case agentDeliverSessionNone:
		return
	case agentDeliverSessionReady:
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}

	// Acquire cross-replica per-agent DB lease so only one Hub replica claims
	// and drains batches for agentID at a time (REQ-2). While leased, the
	// agent_delivery_holds marker stays present (even when held_count == 0) so
	// concurrent live sends queue behind the in-progress drain instead of
	// overtaking earlier batches.
	leaseOwner := api.NewUUID()
	leaseTTL := s.redriveLeaseTTL()
	acquired, err := s.store.AcquireAgentRedriveLease(ctx, agentID, leaseOwner, leaseTTL)
	if err != nil {
		slog.Error("message-hold: acquire redrive lease failed", "agent_id", agentID, "error", err)
		return
	}
	if !acquired {
		return
	}
	lastLeaseRenew := time.Now()
	renewLease := func() bool {
		renewed, rerr := s.store.RenewAgentRedriveLease(ctx, agentID, leaseOwner, leaseTTL)
		if rerr != nil {
			slog.Error("message-hold: renew redrive lease failed", "agent_id", agentID, "error", rerr)
			return false
		}
		if !renewed {
			return false
		}
		lastLeaseRenew = time.Now()
		return true
	}
	claimedAny := false
	reheldOnTransient := false
	defer func() {
		relCtx, relCancel := finalizationContext(ctx)
		rerr := s.store.ReleaseAgentRedriveLease(relCtx, agentID, leaseOwner)
		relCancel()
		if rerr != nil {
			slog.Error("message-hold: release redrive lease failed", "agent_id", agentID, "error", rerr)
			return
		}
		// Reschedule only when this pass actually claimed progress (RV3-R1), so an
		// unswept past-deadline marker can never self-schedule into a tight loop.
		if claimedAny && !reheldOnTransient && ctx.Err() == nil && s.conduitDeliverEnabled() {
			chkCtx, chkCancel := finalizationContext(ctx)
			stillHeld, herr := s.store.HasAgentDeliveryHold(chkCtx, agentID)
			chkCancel()
			if herr == nil && stillHeld {
				s.scheduleHeldMessageRedrive(agentID)
			}
		}
	}()

	claimCtx := store.WithRedriveLeaseOwner(ctx, leaseOwner)
	var pendingSettleIDs []string
	webMentionCache := newHeldWebMentionCache()

	for {
		if ctx.Err() != nil || !s.conduitDeliverEnabled() {
			return
		}
		claimLimit := messageHoldClaimBatchSize - len(pendingSettleIDs)
		if claimLimit <= 0 {
			claimLimit = messageHoldClaimBatchSize
		}
		claimed, err := s.store.ClaimHeldMessages(claimCtx, agentID, claimLimit)
		if err != nil {
			slog.Error("message-hold: claim held messages failed", "agent_id", agentID, "error", err)
			return
		}
		if len(claimed) == 0 {
			s.expirePastDeadlineHeldMessagesForAgent(ctx, agentID)
			return
		}
		claimedAny = true
		if s.messageLog != nil {
			s.messageLog.Info("claimed held messages for conduit re-drive",
				"agent_id", agentID,
				"project_id", agent.ProjectID,
				"claimed_count", len(claimed),
			)
		}

		for i := range claimed {
			if time.Since(lastLeaseRenew) >= leaseTTL/2 {
				if !renewLease() {
					for j := i; j < len(claimed); j++ {
						s.reholdOrFailClaimedRow(ctx, agentID, &claimed[j])
					}
					reheldOnTransient = true
					break
				}
			}
			row := &claimed[i]
			outcome := s.processClaimedHeldRow(ctx, dispatcher, agentID, row, webMentionCache)
			switch outcome {
			case claimedRowDispatched:
				pendingSettleIDs = append(pendingSettleIDs, row.ID)
			case claimedRowFailed:
			case claimedRowReheldTransient:
				// R3 + REQ-3 + OPT-6 + Ordering: row i was re-held (or failed if
				// re-hold was refused) due to a transient lookup fault or pass
				// timeout. Re-hold (or deterministically fail) all remaining
				// rows in this claimed batch so later messages never overtake
				// row i and no row remains stranded in 'dispatched'.
				for j := i + 1; j < len(claimed); j++ {
					s.reholdOrFailClaimedRow(ctx, agentID, &claimed[j])
				}
				reheldOnTransient = true
			}
			if reheldOnTransient {
				break
			}
		}
		if reheldOnTransient {
			return
		}

		// Whenever 64 rows have been dispatched in the current batch window,
		// renew the cross-replica lease, wait until all of them have left
		// 'dispatched' (bound to 'delivering', settled, or re-held) or the 30s
		// cap passes (continuing to renew the lease while waiting), and renew
		// again before claiming the next batch so batches arrive strictly in
		// order without lease lapse (AC A2-3b, REQ-2, O-1).
		if len(pendingSettleIDs) >= messageHoldClaimBatchSize || (len(pendingSettleIDs) > 0 && len(claimed) == claimLimit) {
			if !renewLease() {
				return
			}
			renewIfDue := func() bool {
				if time.Since(lastLeaseRenew) < leaseTTL/2 {
					return true
				}
				return renewLease()
			}
			reheldDuringFlush, leaseLost := s.waitForBatchBoundOrSettled(ctx, pendingSettleIDs, leaseTTL, renewIfDue)
			if leaseLost {
				return
			}
			if reheldDuringFlush {
				reheldOnTransient = true
				return
			}
			pendingSettleIDs = pendingSettleIDs[:0]
			if !renewLease() {
				return
			}
		}
	}
}

func (s *Server) processClaimedHeldRow(
	ctx context.Context,
	dispatcher AgentDispatcher,
	agentID string,
	row *store.Message,
	webMentionCache *heldWebMentionCache,
) claimedRowOutcome {
	if ctx.Err() != nil {
		s.reholdOrFailClaimedRow(ctx, agentID, row)
		return claimedRowReheldTransient
	}

	target, fixedReason, transientErr := s.revalidateClaimedHeldMessage(ctx, agentID, row)
	if transientErr != nil || ctx.Err() != nil {
		errToLog := transientErr
		if errToLog == nil {
			errToLog = ctx.Err()
		}
		slog.Warn("message-hold: transient error or pass timeout during re-validation; re-holding row",
			"agent_id", agentID, "message_id", row.ID, "error", errToLog)
		s.reholdOrFailClaimedRow(ctx, agentID, row)
		return claimedRowReheldTransient
	}
	if fixedReason != "" {
		s.failClaimedHeldMessage(ctx, target, row, fixedReason)
		return claimedRowFailed
	}

	dctx, sm, err := storedMessageDispatch(ctx, row)
	if err != nil {
		s.failClaimedHeldMessage(ctx, target, row, holdFailReasonMessageUnrecoverable)
		return claimedRowFailed
	}
	// Interrupts held while awaiting session are re-delivered as ordinary
	// buffered messages (H3, design.md §2) so they do not cut into processing
	// of earlier held rows.
	sm.Urgent = false

	// Decision responses store a chat summary in row.Msg while live delivery
	// sends the hub-formatted [decision-response] line and hub-set decision
	// metadata from decision_answers / decisions (OPT-1). Reconstruct both
	// before RenderDeliveryText so legacy and envelope consumers match live send.
	if row.Type == messages.TypeDecisionResponse {
		decisionFailReason, decisionTransientErr := s.enrichHeldDecisionResponse(ctx, row, sm)
		if decisionTransientErr != nil || ctx.Err() != nil {
			errToLog := decisionTransientErr
			if errToLog == nil {
				errToLog = ctx.Err()
			}
			slog.Warn("message-hold: transient error enriching decision-response; re-holding row",
				"agent_id", agentID, "message_id", row.ID, "error", errToLog)
			s.reholdOrFailClaimedRow(ctx, agentID, row)
			return claimedRowReheldTransient
		}
		if decisionFailReason != "" {
			s.failClaimedHeldMessage(ctx, target, row, decisionFailReason)
			return claimedRowFailed
		}
	} else if shouldTranslateHeldWebMentions(row) {
		if mentionTransientErr := s.enrichHeldWebMentions(ctx, row, sm, webMentionCache); mentionTransientErr != nil || ctx.Err() != nil {
			errToLog := mentionTransientErr
			if errToLog == nil {
				errToLog = ctx.Err()
			}
			slog.Warn("message-hold: transient error resolving project human mentions; re-holding row",
				"agent_id", agentID, "message_id", row.ID, "error", errToLog)
			s.reholdOrFailClaimedRow(ctx, agentID, row)
			return claimedRowReheldTransient
		}
	}

	if s.writeDenyEnabled() {
		var convRes *messaging.ConversationResult
		if row.ConversationID != "" {
			conv, cerr := s.store.GetConversation(ctx, row.ConversationID)
			if cerr != nil && !errors.Is(cerr, store.ErrNotFound) {
				slog.Warn("message-hold: transient error loading conversation; re-holding row",
					"agent_id", agentID, "message_id", row.ID, "error", cerr)
				s.reholdOrFailClaimedRow(ctx, agentID, row)
				return claimedRowReheldTransient
			}
			if conv != nil {
				convRes = &messaging.ConversationResult{
					ConversationID: conv.ID,
					ExternalRef:    conv.ExternalRef,
					Kind:           conv.Kind,
					Surface:        conv.Surface,
					DisplayName:    conv.DisplayName,
				}
			}
		}
		sm.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
			MessageID:  row.ID,
			ConvResult: convRes,
			Msg:        sm,
			CreatedAt:  row.CreatedAt,
			IsMention:  row.Type == messages.TypeMention,
		})
	}

	if ctx.Err() != nil {
		s.reholdOrFailClaimedRow(ctx, agentID, row)
		return claimedRowReheldTransient
	}

	if err := s.dispatchClaimedHeldMessage(dctx, dispatcher, target, sm); err != nil {
		var (
			idleErr *errAgentIdleSuspending
			markErr *errDeliveryMarkFailed
		)
		if errors.As(err, &idleErr) || errors.As(err, &markErr) {
			slog.Warn("message-hold: delivery gate deferred claimed row before broker dispatch; re-holding row",
				"agent_id", agentID, "message_id", row.ID, "error", err)
			s.reholdOrFailClaimedRow(ctx, agentID, row)
			return claimedRowReheldTransient
		}
		s.failClaimedHeldMessage(ctx, target, row, holdFailReasonBrokerDeliveryFailed)
		return claimedRowFailed
	}
	return claimedRowDispatched
}

// reholdOrFailClaimedRow returns a never-dispatched claimed row to
// awaiting_session (with maxHeld=0 so returning an already-admitted row never
// trips the per-agent cap). When ReholdClaimedMessage refuses (reheld=false,
// e.g. the target stopped, was deleted, or the row passed hold_until) or fails,
// it deterministically transitions the row to failed with a fixed reason and
// notifies the sender (REQ-3, OPT-6: a claimed row never remains stranded in
// dispatch_state='dispatched').
func (s *Server) reholdOrFailClaimedRow(ctx context.Context, agentID string, row *store.Message) {
	if s == nil || s.store == nil || row == nil {
		return
	}
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()

	reheld, rerr := s.store.ReholdClaimedMessage(finCtx, agentID, row.ID, 0)
	if rerr == nil && reheld {
		return
	}
	if rerr != nil {
		slog.Error("message-hold: re-hold claimed message failed; failing row",
			"agent_id", agentID, "message_id", row.ID, "error", rerr)
	}

	var target *store.Agent
	reason := holdFailReasonSessionTimeout
	agent, gerr := s.store.GetAgent(finCtx, agentID)
	switch {
	case errors.Is(gerr, store.ErrNotFound):
		reason = holdFailReasonAgentStopped
	case gerr == nil && agent != nil:
		target = agent
		if eligible, eerr := s.heldTargetEligible(finCtx, agent); eerr == nil && !eligible {
			reason = holdFailReasonAgentStopped
		} else if row.HoldLaunchID == "" || agent.RunID == "" || agent.RunID != row.HoldLaunchID {
			reason = holdFailReasonAgentRestarted
		}
	}
	s.failClaimedHeldMessage(finCtx, target, row, reason)
}

type heldWebMentionCache struct {
	membersByProject map[string][]chatMemberEntry
	topicsByProjKey  map[string]bool
}

func newHeldWebMentionCache() *heldWebMentionCache {
	return &heldWebMentionCache{
		membersByProject: make(map[string][]chatMemberEntry),
		topicsByProjKey:  make(map[string]bool),
	}
}

// shouldTranslateHeldWebMentions reports whether row is a candidate native
// chat-v2 human message whose live dispatch in sendAgentRouted runs
// translateMentionsOutbound (ptone/scion#8212).
//
// In live delivery, translateMentionsOutbound runs only on native chat-v2
// conversation sends (sendChatMessage -> authorizeChatSend / chatKeyReadAudience
// -> sendAgentRouted), which always carry Channel="web" and a chat-v2
// conversation key in ThreadID: either a canonical agent DM key
// (dm:agent:<agentUUID>:user:<senderUUID>) or a project webchat_topic ID
// (confirmed against WebChatStore in enrichHeldWebMentions). Direct
// POST /api/v1/agents/{id}/message sends from browser clients
// (handleAgentMessage + processMentions, e.g. quick-message-dialog and
// agent-message-viewer) also stamp Channel="web" when ClientType()=="web",
// but carry no chat-v2 conversation key (ThreadID is "", wave-1 "agent:<id>",
// or non-topic external refs) and deliver verbatim on live send, so re-drive
// must leave them verbatim too.
func shouldTranslateHeldWebMentions(row *store.Message) bool {
	if row == nil || row.Type == messages.TypeDecisionResponse {
		return false
	}
	if row.Channel != "web" || !strings.HasPrefix(row.Sender, "user:") || row.SenderID == "" {
		return false
	}
	if row.Broadcasted || row.GroupID != "" ||
		row.ScheduledEventID != "" || row.DelegationGrantID != "" || row.AuthorizingUserID != "" {
		return false
	}
	if row.ProjectID == "" || row.ProjectID == uuid.Nil.String() {
		return false
	}
	if !hasCandidateChatV2Key(row.ThreadID, row.SenderID) {
		return false
	}
	return strings.Contains(row.Msg, "@")
}

// hasCandidateChatV2Key mirrors chatKeyReadAudience + authorizeDMKeyParticipant
// + authorizeDMPeer (handlers_chat_send.go) and isWebThreadMessage
// (chat_member_fanout.go / events.go) on pure row fields: either a canonical
// chat-v2 agent DM key naming senderID, or a valid non-"agent:" candidate
// topic key to be verified against WebChatStore.
func hasCandidateChatV2Key(key, senderID string) bool {
	if !validChatTopicKey(key) {
		return false
	}
	if strings.HasPrefix(key, "dm:") {
		return isCanonicalChatAgentDMKey(key, senderID)
	}
	return !strings.HasPrefix(key, "agent:")
}

func isCanonicalChatAgentDMKey(key, senderID string) bool {
	if senderID == "" || !validDMKey(key) {
		return false
	}
	agentID, userID := parseDMKeyIDs(key)
	if agentID == "" || userID != senderID {
		return false
	}
	canonical, err := messages.DMConversationKey("agent", agentID, "user", userID)
	return err == nil && canonical == key
}

func (s *Server) enrichHeldWebMentions(
	ctx context.Context,
	row *store.Message,
	sm *messages.StructuredMessage,
	cache *heldWebMentionCache,
) error {
	if !strings.HasPrefix(row.ThreadID, "dm:") {
		isTopic, err := s.isProjectChatV2Topic(ctx, row.ProjectID, row.ThreadID, cache)
		if err != nil {
			return err
		}
		if !isTopic {
			return nil
		}
	}

	var (
		members []chatMemberEntry
		ok      bool
	)
	if cache != nil && cache.membersByProject != nil {
		members, ok = cache.membersByProject[row.ProjectID]
	}
	if !ok {
		var err error
		members, err = s.projectHumanMembersStrict(ctx, row.ProjectID)
		if err != nil {
			return fmt.Errorf("project human members lookup: %w", err)
		}
		if cache != nil && cache.membersByProject != nil {
			cache.membersByProject[row.ProjectID] = members
		}
	}
	if len(members) > 0 {
		sm.Msg = translateMentionsOutbound(sm.Msg, members)
	}
	return nil
}

func (s *Server) isProjectChatV2Topic(
	ctx context.Context,
	projectID, topicID string,
	cache *heldWebMentionCache,
) (bool, error) {
	s.mu.RLock()
	wcs := s.webChatStore
	s.mu.RUnlock()
	if wcs == nil {
		return false, nil
	}
	cacheKey := projectID + "\x00" + topicID
	if cache != nil && cache.topicsByProjKey != nil {
		if isTopic, ok := cache.topicsByProjKey[cacheKey]; ok {
			return isTopic, nil
		}
	}
	_, err := wcs.GetTopicConversationIDIncludingDeletedInProject(ctx, projectID, topicID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if cache != nil && cache.topicsByProjKey != nil {
				cache.topicsByProjKey[cacheKey] = false
			}
			return false, nil
		}
		return false, fmt.Errorf("webchat topic lookup: %w", err)
	}
	if cache != nil && cache.topicsByProjKey != nil {
		cache.topicsByProjKey[cacheKey] = true
	}
	return true, nil
}

func (s *Server) enrichHeldDecisionResponse(
	ctx context.Context,
	row *store.Message,
	sm *messages.StructuredMessage,
) (fixedReason string, transientErr error) {
	byResp, err := s.store.GetDecisionAnswersByResponseMessageIDs(ctx, []string{row.ID})
	if err != nil {
		return "", fmt.Errorf("decision answer lookup: %w", err)
	}
	answers := byResp[row.ID]
	if len(answers) == 0 {
		return holdFailReasonMessageUnrecoverable, nil
	}

	if len(answers) == 1 && answers[0].BatchID == "" {
		a := answers[0]
		var overridden *store.DecisionAnswer
		if a.Outcome == store.DecisionOutcomeOverride && a.DecisionRowID != "" {
			d, derr := s.store.GetDecision(ctx, a.DecisionRowID)
			if derr != nil && !errors.Is(derr, store.ErrNotFound) {
				return "", fmt.Errorf("decision lookup: %w", derr)
			}
			if d != nil {
				for _, cand := range d.Answers {
					if cand.Provenance == store.DecisionProvenanceAgentInterpreted {
						overridden = cand
						break
					}
				}
			}
		}
		sm.Msg = decisionResponseAgentLine(a, overridden)
		sm.Metadata = messaging.StripReservedMetadata(sm.Metadata)
		if sm.Metadata == nil {
			sm.Metadata = make(map[string]string, 4)
		}
		sm.Metadata[messages.MetadataDecisionKey] = a.Key
		sm.Metadata[messages.MetadataDecisionVersion] = strconv.Itoa(a.Version)
		sm.Metadata[messages.MetadataChoiceID] = a.ChoiceID
		sm.Metadata[messages.MetadataDecisionProvenance] = a.Provenance
		if a.ChoiceID == messages.ChoiceIDOther && a.OtherText != "" {
			sm.Metadata[messages.MetadataDecisionOtherText] = a.OtherText
		}
		return "", nil
	}

	batchID := answers[0].BatchID
	if batchID == "" {
		return holdFailReasonMessageUnrecoverable, nil
	}
	rowIDs := make([]string, 0, len(answers))
	needOverridden := false
	for _, a := range answers {
		if a.BatchID != batchID {
			return holdFailReasonMessageUnrecoverable, nil
		}
		if a.DecisionRowID != "" {
			rowIDs = append(rowIDs, a.DecisionRowID)
		}
		if a.Outcome == store.DecisionOutcomeOverride {
			needOverridden = true
		}
	}
	rowsByID, rerr := s.store.GetDecisionRowsByIDs(ctx, rowIDs)
	if rerr != nil {
		return "", fmt.Errorf("decision set rows lookup: %w", rerr)
	}
	promptID := ""
	for _, a := range answers {
		if d := rowsByID[a.DecisionRowID]; d != nil && d.MessageID != "" {
			promptID = d.MessageID
			break
		}
	}
	ordered := slices.Clone(answers)
	slices.SortFunc(ordered, func(a, b *store.DecisionAnswer) int {
		posA, posB := math.MaxInt, math.MaxInt
		if d := rowsByID[a.DecisionRowID]; d != nil {
			posA = d.Position
		}
		if d := rowsByID[b.DecisionRowID]; d != nil {
			posB = d.Position
		}
		if c := cmp.Compare(posA, posB); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	if ordered[0].Outcome == store.DecisionOutcomeDismiss {
		var promptRows []*store.Decision
		if promptID != "" {
			byPrompt, perr := s.store.GetDecisionsByMessageIDs(ctx, []string{promptID})
			if perr != nil {
				return "", fmt.Errorf("decision dismiss prompt decisions lookup: %w", perr)
			}
			promptRows = byPrompt[promptID]
		}
		sm.Msg = decisionDismissAgentLine(promptRows, ordered)
		sm.Metadata = decisionDismissMetadata(sm.Metadata, batchID, ordered)
		return "", nil
	}
	var overridden map[string]*store.DecisionAnswer
	if needOverridden && promptID != "" {
		byPrompt, perr := s.store.GetDecisionsByMessageIDs(ctx, []string{promptID})
		if perr != nil {
			return "", fmt.Errorf("decision set prompt decisions lookup: %w", perr)
		}
		overridden = make(map[string]*store.DecisionAnswer)
		for _, d := range byPrompt[promptID] {
			for _, cand := range d.Answers {
				if cand.Provenance == store.DecisionProvenanceAgentInterpreted {
					overridden[d.ID] = cand
					break
				}
			}
		}
	}
	sm.Msg = decisionSetAgentLines(promptID, ordered, overridden)
	sm.Metadata = messaging.StripReservedMetadata(sm.Metadata)
	if sm.Metadata == nil {
		sm.Metadata = make(map[string]string, 2)
	}
	sm.Metadata[messages.MetadataDecisionBatch] = batchID
	sm.Metadata[messages.MetadataDecisionAnswers] = decisionAnswersMetadata(ordered)
	return "", nil
}

func (s *Server) failClaimedHeldMessage(ctx context.Context, target *store.Agent, row *store.Message, reason string) {
	finCtx, finCancel := finalizationContext(ctx)
	defer finCancel()
	won, err := s.store.FailUndeliveredMessage(finCtx, row.ID, reason)
	if err != nil {
		slog.Error("message-hold: failed to mark claimed row failed",
			"message_id", row.ID, "reason", reason, "error", err)
		return
	}
	if !won {
		return
	}
	if s.messageLog != nil {
		projectID := row.ProjectID
		if target != nil && target.ProjectID != "" {
			projectID = target.ProjectID
		}
		s.messageLog.Warn("held message failed before conduit delivery",
			"message_id", row.ID,
			"agent_id", row.AgentID,
			"project_id", projectID,
			"reason", reason,
		)
	}
	s.notifyStoredMessageSenderFailed(finCtx, target, row, reason)
}

// dispatchClaimedHeldMessage hands a re-validated held row to the runtime broker
// through the shared dispatchWithBrokerRetry helper with s.deliveryMarker() and
// interrupt=false (and no auto-wake hook: re-drive never wakes a stopping or
// suspended target). Callers must run revalidateClaimedHeldMessage immediately
// before calling this.
func (s *Server) dispatchClaimedHeldMessage(
	dctx context.Context,
	dispatcher AgentDispatcher,
	target *store.Agent,
	sm *messages.StructuredMessage,
) error {
	baseCtx := context.WithoutCancel(dctx)
	if sm != nil && sm.StoredMessageID != "" {
		baseCtx = withDispatchMessageID(baseCtx, sm.StoredMessageID)
	}
	callCtx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
	defer cancel()
	return dispatchWithBrokerRetry(callCtx, s.deliveryMarker(), s.recordLog(), dispatcher, target, sm.Msg, false, sm)
}

// waitForBatchBoundOrSettled waits until every row in messageIDs has left
// dispatch_state='dispatched' (i.e. bound to 'delivering', finished 'delivered',
// failed, or re-held to 'awaiting_session') or messageHoldBatchSettleCap passes.
// Uses a single GetMessagesByIDs IN query per poll with exponential backoff
// capped at messageHoldBatchPollMaxInterval (OPT-3) and renews the cross-replica
// lease via renewIfDue while waiting (O-1).
// Returns reheld=true if any row settled back into 'awaiting_session', or
// leaseLost=true if renewIfDue reported lease loss.
func (s *Server) waitForBatchBoundOrSettled(
	ctx context.Context,
	messageIDs []string,
	leaseTTL time.Duration,
	renewIfDue func() bool,
) (reheld, leaseLost bool) {
	if len(messageIDs) == 0 {
		return false, false
	}
	waitCtx, cancel := context.WithTimeout(ctx, messageHoldBatchSettleCap)
	defer cancel()

	pending := slices.Clone(messageIDs)
	check := func() (done bool) {
		msgs, err := s.store.GetMessagesByIDs(waitCtx, pending)
		if err != nil {
			return false
		}
		byID := make(map[string]string, len(msgs))
		for i := range msgs {
			byID[msgs[i].ID] = msgs[i].DispatchState
		}
		stillDispatched := pending[:0]
		for _, id := range pending {
			st, ok := byID[id]
			if !ok {
				continue
			}
			switch st {
			case store.MessageDispatchDispatched:
				stillDispatched = append(stillDispatched, id)
			case store.MessageDispatchAwaitingSession:
				reheld = true
			}
		}
		pending = stillDispatched
		return len(pending) == 0
	}

	if check() {
		return reheld, false
	}
	maxInterval := messageHoldBatchPollMaxInterval
	if leaseTTL > 0 && leaseTTL/3 < maxInterval {
		if half := leaseTTL / 3; half >= messageHoldBatchPollInterval {
			maxInterval = half
		} else {
			maxInterval = messageHoldBatchPollInterval
		}
	}
	interval := messageHoldBatchPollInterval
	if interval > maxInterval {
		interval = maxInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return reheld, false
		case <-timer.C:
			if renewIfDue != nil && !renewIfDue() {
				return reheld, true
			}
			if check() {
				return reheld, false
			}
			interval *= 2
			if interval > maxInterval {
				interval = maxInterval
			}
			timer.Reset(interval)
		}
	}
}

// revalidateClaimedHeldMessage checks target state, launch incarnation (R2),
// and sender/delegation authorization (A2-S4) before dispatching a claimed row.
// Returns:
//   - (target, "", nil) when valid to dispatch;
//   - (target, fixedReason, nil) on a policy/lifecycle denial;
//   - (nil, "", err) on a transient lookup/authz fault (R3: re-hold, never dispatch).
func (s *Server) revalidateClaimedHeldMessage(
	ctx context.Context,
	agentID string,
	row *store.Message,
) (*store.Agent, string, error) {
	if row == nil {
		return nil, holdFailReasonMessageUnrecoverable, nil
	}
	target, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, holdFailReasonAgentStopped, nil
		}
		return nil, "", fmt.Errorf("target agent lookup: %w", err)
	}
	eligible, err := s.heldTargetEligible(ctx, target)
	if err != nil {
		return nil, "", fmt.Errorf("target eligibility lookup: %w", err)
	}
	if !eligible {
		return target, holdFailReasonAgentStopped, nil
	}
	if target.IdleSuspendClaimActive || !heldMessageTargetPhaseEligible(target.Phase) {
		return nil, "", errors.New("target agent has an active idle-suspend claim or is idle-suspended")
	}

	// Launch incarnation match (A2-S4, R2): empty hold_launch_id or empty
	// target.RunID is always a mismatch, never equal by default.
	if row.HoldLaunchID == "" || target.RunID == "" || target.RunID != row.HoldLaunchID {
		return target, holdFailReasonAgentRestarted, nil
	}

	allowed, terr := s.revalidateHeldMessageSender(ctx, row, target)
	if terr != nil {
		return nil, "", terr
	}
	if !allowed {
		return target, holdFailReasonSenderUnauthorized, nil
	}
	return target, "", nil
}

func (s *Server) revalidateHeldMessageSender(ctx context.Context, row *store.Message, target *store.Agent) (bool, error) {
	// 1. Scheduled message event fire: selected ONLY from hub-recorded immutable
	// provenance (row.ScheduledEventID plus the scheduler system sender tuple),
	// never from client-controllable Channel or ThreadID (REQ-1).
	if row.ScheduledEventID != "" {
		if row.Sender != "scheduler" || row.SenderID != "SCHEDULER" ||
			row.Type != messages.TypeSystem || row.DelegationGrantID != "" || row.AuthorizingUserID != "" {
			return false, nil
		}
		return s.revalidateScheduledHeldMessage(ctx, row, target)
	}

	// 2. Delegated agent sender (AuthorizingUserID / DelegationGrantID set).
	if row.DelegationGrantID != "" || row.AuthorizingUserID != "" {
		return s.revalidateDelegatedHeldMessage(ctx, row, target)
	}

	// 3. Ordinary agent sender.
	if strings.HasPrefix(row.Sender, "agent:") {
		if row.SenderID == "" {
			return false, nil
		}
		senderAgent, err := s.store.GetAgent(ctx, row.SenderID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("sender agent lookup: %w", err)
		}
		if senderAgent == nil || !senderAgent.DeletedAt.IsZero() {
			return false, nil
		}
		return s.revalidateIdentityToAgent(ctx, &storedAgentIdentity{agent: senderAgent}, target)
	}

	// 4. User or federated visitor sender.
	if strings.HasPrefix(row.Sender, "user:") && row.SenderID != "" {
		return s.revalidateUserHeldMessage(ctx, row, target)
	}

	return false, nil
}

func (s *Server) revalidateScheduledHeldMessage(ctx context.Context, row *store.Message, target *store.Agent) (bool, error) {
	if row.ScheduledEventID == "" || target == nil {
		return false, nil
	}
	evt, err := s.store.GetScheduledEvent(ctx, row.ScheduledEventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("scheduled event lookup: %w", err)
	}
	if evt == nil || evt.EventType != "message" || evt.ProjectID == "" {
		return false, nil
	}
	if row.SenderProjectID == nil || *row.SenderProjectID != evt.ProjectID || evt.ProjectID != target.ProjectID {
		return false, nil
	}
	var payload MessageEventPayload
	if err := json.Unmarshal([]byte(evt.Payload), &payload); err != nil {
		return false, nil
	}
	targetMatched := false
	switch {
	case payload.AgentID != "":
		targetMatched = payload.AgentID == target.ID
	case payload.AgentName != "":
		targetMatched = strings.EqualFold(payload.AgentName, target.Slug) ||
			strings.EqualFold(payload.AgentName, target.Name) ||
			strings.EqualFold(api.Slugify(payload.AgentName), target.Slug)
	}
	if !targetMatched {
		return false, nil
	}

	return s.revalidateHeldScheduledMessageAuthority(ctx, evt.ProjectID, scheduledRecordedAuthority(*evt), evt.CreatedBy, target)
}

func (s *Server) revalidateUserHeldMessage(ctx context.Context, row *store.Message, target *store.Agent) (bool, error) {
	devUser := row.SenderID == DevUserID && s.authConfig.DevAuthToken != ""
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: row.SenderID}
	if devUser {
		principal = devUserPrincipal()
	}
	u, err := checkUserAccount(ctx, s.store, principal)
	switch classifyUserAccount(u, err) {
	case accountAllowed:
	case accountLookupFault:
		return false, fmt.Errorf("sender user lookup: %w", err)
	default:
		return false, nil
	}

	var identity Identity
	switch {
	case devUser:
		identity = s.authConfig.currentDevUser()
	case u != nil && u.IsFederated():
		if !s.experimentEnabled(experiments.FederationProjectVisitors) {
			return false, nil
		}
		email := u.Email
		if u.FederatedEmail != nil && *u.FederatedEmail != "" {
			email = *u.FederatedEmail
		}
		issuer := ""
		if u.HomeIssuer != nil {
			issuer = *u.HomeIssuer
		}
		identity = NewVisitorIdentity(u.ID, issuer, u.ID, email, u.DisplayName)
	case u != nil:
		identity = NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "redrive")
	default:
		return false, nil
	}
	return s.revalidateIdentityToAgent(ctx, identity, target)
}

func (s *Server) revalidateDelegatedHeldMessage(ctx context.Context, row *store.Message, target *store.Agent) (bool, error) {
	if row.DelegationGrantID == "" || row.AuthorizingUserID == "" || row.SenderID == "" ||
		!strings.HasPrefix(row.Sender, "agent:") || row.Broadcasted {
		return false, nil
	}
	if s.authzService == nil || !s.experimentEnabled(experiments.AgentDelegation) {
		return false, nil
	}
	now := time.Now()

	grant, err := s.store.GetAgentDelegationGrant(ctx, row.DelegationGrantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("delegation grant lookup: %w", err)
	}
	if grant == nil || grant.AgentID != row.SenderID || grant.IssuerUserID != row.AuthorizingUserID {
		return false, nil
	}
	if grant.RevokedAt != nil || !now.Before(grant.ExpiresAt) ||
		grant.CeilingVersion != int(permissions.CeilingVersionV1) ||
		grant.ParentGrantID != "" || grant.AllowSubdelegation || grant.Depth != 0 {
		return false, nil
	}

	grantCeiling := permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: grant.CeilingPermissionIDs,
	}
	if !grantCeiling.Allows("agent.message") {
		return false, nil
	}

	narrowing, err := s.agentDelegationPolicyNarrowing()
	if err != nil {
		return false, fmt.Errorf("delegation policy read: %w", err)
	}
	boundary := TokenBoundary{Kind: BoundaryKind(grant.BoundaryKind), ProjectID: grant.BoundaryProjectID}
	if !agentDelegableUnder(narrowing, "agent.message", permissions.BoundaryKind(boundary.Kind)) {
		return false, nil
	}

	issuer, err := s.store.GetUser(ctx, grant.IssuerUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("delegation issuer lookup: %w", err)
	}
	if issuer == nil || issuer.Status != store.UserStatusActive ||
		(issuer.ExpiresAt != nil && !now.Before(*issuer.ExpiresAt)) ||
		s.isTransportIdentity(issuer.Email) || !issuer.IsHumanKind() || isReservedTestIdentityEmail(issuer.Email) {
		return false, nil
	}

	actor, err := s.store.GetAgent(ctx, row.SenderID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("delegation actor lookup: %w", err)
	}
	if actor == nil || !actor.DeletedAt.IsZero() || isAgentPhaseSuspended(actor) ||
		actor.ProjectID != grant.AgentProjectID || actor.Generation != grant.AgentGeneration ||
		reincarnationInFlight(actor) || !issuerControlsAgent(actor, grant.IssuerUserID) {
		return false, nil
	}
	if err := s.agentStanding(ctx, actor.ID); err != nil {
		if errors.Is(err, errAgentNotInStanding) {
			return false, nil
		}
		return false, fmt.Errorf("delegation actor standing: %w", err)
	}

	issuerIdent := NewAuthenticatedUser(issuer.ID, issuer.Email, issuer.DisplayName, issuer.Role, "redrive")
	issuerPC := principalContextForIdentity(issuerIdent)
	adm, err := s.authzService.ProjectTargetAdmission(ctx, issuerPC, actor.ProjectID, agentDelegationCreatePermission, agentResource(actor), nil)
	if err != nil {
		if errors.Is(err, ErrProjectAccessDenied) && !isProjectAccessLookupFault(err) {
			return false, nil
		}
		return false, fmt.Errorf("delegation issuer project admission: %w", err)
	}
	if !adm.Admitted {
		return false, nil
	}

	eval := s.authzService.EvaluateBearerCeiling(ctx, issuerPC, boundary, grantCeiling, "agent.message", agentResource(target), BearerOptions{})
	if !eval.Decision.Allowed {
		if eval.Decision.IsIndeterminate() {
			return false, fmt.Errorf("delegation bearer evaluation indeterminate: %s", eval.Decision.Reason)
		}
		return false, nil
	}

	// Actor agent's own messaging mode / cross-project gates must also allow.
	return s.revalidateIdentityToAgent(ctx, &storedAgentIdentity{agent: actor}, target)
}

// revalidateIdentityToAgent evaluates authorizeAgentMessage(ctx, identity, target, false)
// as the sole allow authority, and when denied probes whether a transient
// store/resolution fault caused the denial (R3 -> non-nil error) vs a definite
// policy denial ((false, nil)).
func (s *Server) revalidateIdentityToAgent(ctx context.Context, identity Identity, target *store.Agent) (bool, error) {
	if identity == nil || target == nil {
		return false, nil
	}
	allowed, _, _ := s.authorizeAgentMessage(ctx, identity, target, false)
	if allowed {
		return true, nil
	}
	if s.authzService == nil {
		return false, nil
	}

	if agentIdent, ok := identity.(AgentIdentity); ok {
		if agentIdent.ID() == target.ID {
			if err := s.agentStanding(ctx, agentIdent.ID()); err != nil && !errors.Is(err, errAgentNotInStanding) {
				return false, fmt.Errorf("self-message standing lookup: %w", err)
			}
			return false, nil
		}
		senderAgent, err := s.store.GetAgent(ctx, agentIdent.ID())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("sender agent lookup: %w", err)
		}
		if senderAgent == nil || !senderAgent.DeletedAt.IsZero() {
			return false, nil
		}
		if senderAgent.ProjectID != target.ProjectID {
			if ops := s.GetOperationalSettings(); ops != nil {
				cpm := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
				if cpm.Err != nil {
					return false, fmt.Errorf("cross-project setting lookup: %w", cpm.Err)
				}
			}
			destProj, perr := s.store.GetProject(ctx, target.ProjectID)
			if perr != nil && !errors.Is(perr, store.ErrNotFound) {
				return false, fmt.Errorf("destination project lookup: %w", perr)
			}
			if originID := agentIdent.OriginUserID(); originID != "" {
				if _, uerr := s.store.GetUser(ctx, originID); uerr != nil && !errors.Is(uerr, store.ErrNotFound) {
					return false, fmt.Errorf("origin user lookup: %w", uerr)
				}
				if destProj != nil && destProj.CrossProjectInbound == store.CrossProjectInboundMembers {
					mem := s.CheckEffectiveMembership(ctx, originID, target.ProjectID)
					if mem.Err != nil {
						return false, fmt.Errorf("cross-project membership lookup: %w", mem.Err)
					}
				}
			}
		}
		if err := s.agentStanding(ctx, senderAgent.ID); err != nil && !errors.Is(err, errAgentNotInStanding) {
			return false, fmt.Errorf("sender agent standing lookup: %w", err)
		}
		return false, nil
	}

	if userIdent, ok := identity.(UserIdentity); ok {
		if target.MessageMode == store.MessageModeNone {
			return false, nil
		}
		targetRes := agentResource(target)
		if scoped, ok := userIdent.(*ScopedUserIdentity); ok {
			eval := s.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(scoped), scoped.Boundary(), scoped.Ceiling(), "agent.message", targetRes, BearerOptions{})
			if !eval.Decision.Allowed && eval.Decision.IsIndeterminate() {
				return false, fmt.Errorf("uat message gate indeterminate: %s", eval.Decision.Reason)
			}
		}
		uatDeniesMessage := false
		if scoped, ok := userIdent.(*ScopedUserIdentity); ok && !scoped.HasScope("agent:message") {
			uatDeniesMessage = true
		}
		if !uatDeniesMessage && AncestryIsHubAttested(identity) && canAccessAsAncestor(userIdent.ID(), targetRes) {
			if _, fault := s.authzService.messageAncestorProjectAccess(ctx, identity, target.ProjectID, targetRes); fault {
				return false, errors.New("ancestor project access lookup failed")
			}
		}
		if !uatDeniesMessage && userIdent.ID() != "" && target.ProjectID != "" {
			if _, err := activeDirectProjectRoles(ctx, s.store, s.authzService, true, userIdent.ID(), target.ProjectID); err != nil {
				return false, fmt.Errorf("project owner role lookup: %w", err)
			}
		}
		if target.MessageMode == store.MessageModeProject || target.MessageMode == store.MessageModeHub {
			decision := s.authzService.CheckAccess(ctx, userIdent, targetRes, ActionMessage)
			if decision.IsIndeterminate() {
				return false, fmt.Errorf("message access check indeterminate: %s", decision.Reason)
			}
		}
		return false, nil
	}

	return false, nil
}
