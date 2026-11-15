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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Fixed failure reasons for held messages (ptone/scion#7881, A2-S8).
// Never store or echo broker-supplied text or failure codes on held-row
// expiry or re-validation failures.
const (
	holdFailReasonSessionTimeout       = "agent session did not connect"
	holdFailReasonAgentStopped         = "agent stopped"
	holdFailReasonCannotReceiveConduit = "agent cannot receive messages over conduit"
	holdFailReasonAgentRestarted       = "agent restarted before delivery"
	holdFailReasonSenderUnauthorized   = "sender is no longer authorized"
	holdFailReasonMessageUnrecoverable = "message cannot be re-delivered"
)

const (
	// messageHoldClaimBatchSize matches the Conduit deliver-message route's
	// 64-message-ID batch bound (design.md Phase A2 §3, AC A2-3b, AC A2-6).
	messageHoldClaimBatchSize = agentcontrol.MaxDeliverMessageIDs

	// messageHoldRedriveTimeout bounds a single detached per-agent re-drive pass.
	messageHoldRedriveTimeout = 90 * time.Second

	// messageHoldSweepTimeout bounds one 30s backstop sweep pass.
	messageHoldSweepTimeout = 20 * time.Second

	// messageHoldBatchPollInterval is how often inter-batch settling checks
	// whether the previous batch's rows have bound or left 'dispatched'.
	messageHoldBatchPollInterval = 25 * time.Millisecond

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
	mu              sync.Mutex
	stopped         bool
	stopLoop        context.CancelFunc
	abortCtx        context.Context
	abort           context.CancelFunc
	inFlight        map[string]bool
	pending         map[string]bool
	running         sync.WaitGroup
	maxHeldOverride int // >0 overrides maxHeldMessagesPerAgent in tests
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
//  2. Inspects remaining agent_delivery_holds markers: fails stopped/deleted/held
//     targets ("agent stopped"), fails connected sessions without deliver-message
//     ("agent cannot receive messages over conduit"), and schedules re-drive for
//     agents with an eligible cluster-wide deliver-message session (A2-S7).
func (s *Server) sweepHeldMessages(parentCtx context.Context) {
	if s == nil || s.store == nil || !s.conduitDeliverEnabled() {
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

func (s *Server) heldTargetEligible(ctx context.Context, target *store.Agent) (bool, error) {
	if target == nil || !target.DeletedAt.IsZero() ||
		target.DeletionHoldsRow(time.Now()) ||
		reincarnationInFlight(target) ||
		!heldMessageTargetPhaseEligible(target.Phase) {
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

	for {
		if ctx.Err() != nil || !s.conduitDeliverEnabled() {
			return
		}
		claimed, err := s.store.ClaimHeldMessages(ctx, agentID, messageHoldClaimBatchSize)
		if err != nil {
			slog.Error("message-hold: claim held messages failed", "agent_id", agentID, "error", err)
			return
		}
		if len(claimed) == 0 {
			return
		}
		if s.messageLog != nil {
			s.messageLog.Info("claimed held messages for conduit re-drive",
				"agent_id", agentID,
				"project_id", agent.ProjectID,
				"claimed_count", len(claimed),
			)
		}

		dispatchedIDs := make([]string, 0, len(claimed))
		reheldOnTransient := false
		maxHeld := s.heldMessagesCap()
		for i := range claimed {
			row := &claimed[i]
			outcome := s.processClaimedHeldRow(ctx, dispatcher, agentID, row)
			switch outcome {
			case claimedRowDispatched:
				dispatchedIDs = append(dispatchedIDs, row.ID)
			case claimedRowFailed:
			case claimedRowReheldTransient:
				// R3 + Ordering: row i returned to awaiting_session due to a
				// transient lookup error. Re-hold all remaining rows in this
				// claimed batch so later messages never overtake row i.
				for j := i + 1; j < len(claimed); j++ {
					rest := &claimed[j]
					rctx, rcancel := finalizationContext(ctx)
					if _, rerr := s.store.ReholdClaimedMessage(rctx, agentID, rest.ID, maxHeld); rerr != nil {
						slog.Error("message-hold: re-hold remaining batch row failed",
							"agent_id", agentID, "message_id", rest.ID, "error", rerr)
					}
					rcancel()
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

		// When more held rows may follow (full batch of 64 or hold marker still
		// present), wait until the dispatched rows in this batch have left
		// 'dispatched' (bound to 'delivering' or settled) or the 30s cap passes,
		// so batches arrive in order (AC A2-3b).
		if len(dispatchedIDs) > 0 {
			hasMore, _ := s.store.HasAgentDeliveryHold(ctx, agentID)
			if hasMore || len(claimed) == messageHoldClaimBatchSize {
				if reheldDuringFlush := s.waitForBatchBoundOrSettled(ctx, dispatchedIDs); reheldDuringFlush {
					// Session dropped during flush and rows returned to
					// awaiting_session; wait for next session admission or sweep.
					return
				}
			}
		}
	}
}

func (s *Server) processClaimedHeldRow(
	ctx context.Context,
	dispatcher AgentDispatcher,
	agentID string,
	row *store.Message,
) claimedRowOutcome {
	target, fixedReason, transientErr := s.revalidateClaimedHeldMessage(ctx, agentID, row)
	if transientErr != nil {
		slog.Warn("message-hold: transient error during re-validation; re-holding row",
			"agent_id", agentID, "message_id", row.ID, "error", transientErr)
		rctx, rcancel := finalizationContext(ctx)
		if _, rerr := s.store.ReholdClaimedMessage(rctx, agentID, row.ID, s.heldMessagesCap()); rerr != nil {
			slog.Error("message-hold: re-hold claimed message after transient error failed",
				"agent_id", agentID, "message_id", row.ID, "error", rerr)
		}
		rcancel()
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
	if s.writeDenyEnabled() {
		var convRes *messaging.ConversationResult
		if row.ConversationID != "" {
			if conv, cerr := s.store.GetConversation(ctx, row.ConversationID); cerr == nil && conv != nil {
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

	if err := s.dispatchClaimedHeldMessage(dctx, dispatcher, target, sm); err != nil {
		reason := sanitizeFailureReason(err.Error())
		if reason == "" {
			reason = "broker delivery failed on re-drive"
		}
		s.failClaimedHeldMessage(ctx, target, row, reason)
		return claimedRowFailed
	}
	return claimedRowDispatched
}

func (s *Server) failClaimedHeldMessage(ctx context.Context, target *store.Agent, row *store.Message, reason string) {
	finCtx, finCancel := finalizationContext(ctx)
	won, err := s.store.FailUndeliveredMessage(finCtx, row.ID, reason)
	finCancel()
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
	s.notifyStoredMessageSenderFailed(ctx, target, row, reason)
}

// dispatchClaimedHeldMessage hands a re-validated held row to the runtime broker
// through the shared dispatchWithBrokerRetry helper with interrupt=false.
// Callers must run revalidateClaimedHeldMessage immediately before calling this.
func (s *Server) dispatchClaimedHeldMessage(
	dctx context.Context,
	dispatcher AgentDispatcher,
	target *store.Agent,
	sm *messages.StructuredMessage,
) error {
	return dispatchWithBrokerRetry(dctx, s.recordLog(), dispatcher, target, sm.Msg, false, sm)
}

// waitForBatchBoundOrSettled waits until every row in messageIDs has left
// dispatch_state='dispatched' (i.e. bound to 'delivering', finished 'delivered',
// failed, or re-held to 'awaiting_session') or messageHoldBatchSettleCap passes.
// Returns reheld=true if any row settled back into 'awaiting_session'.
func (s *Server) waitForBatchBoundOrSettled(ctx context.Context, messageIDs []string) (reheld bool) {
	if len(messageIDs) == 0 {
		return false
	}
	waitCtx, cancel := context.WithTimeout(ctx, messageHoldBatchSettleCap)
	defer cancel()

	pending := slices.Clone(messageIDs)
	ticker := time.NewTicker(messageHoldBatchPollInterval)
	defer ticker.Stop()

	check := func() (done bool) {
		stillDispatched := pending[:0]
		for _, id := range pending {
			m, err := s.store.GetMessage(waitCtx, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				stillDispatched = append(stillDispatched, id)
				continue
			}
			switch m.DispatchState {
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
		return reheld
	}
	for {
		select {
		case <-waitCtx.Done():
			return reheld
		case <-ticker.C:
			if check() {
				return reheld
			}
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
	// 1. Scheduled message event fire (persistScheduledMessageEvent sets
	// Channel="scheduler", ThreadID=evt.ID).
	if row.Channel == "scheduler" && row.ThreadID != "" {
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
	evt, err := s.store.GetScheduledEvent(ctx, row.ThreadID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("scheduled event lookup: %w", err)
	}
	if evt == nil {
		return false, nil
	}
	auth, identity, err := s.resolveScheduledAuthority(ctx, *evt)
	if err != nil {
		if errors.Is(err, errScheduledAuthorityDenied) || errors.Is(err, errScheduledAuthorityUnrecorded) {
			return false, nil
		}
		return false, fmt.Errorf("scheduled authority lookup: %w", err)
	}
	// Pre-check transient lookup faults on identity -> target before
	// authorizeScheduledMessageFire evaluates policy.
	allowed, terr := s.revalidateIdentityToAgent(ctx, identity, target)
	if terr != nil {
		return false, terr
	}
	if !allowed {
		return false, nil
	}
	if err := s.authorizeScheduledMessageFire(ctx, *evt, auth, identity, target); err != nil {
		return false, nil
	}
	return true, nil
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
