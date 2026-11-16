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
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Auto-wake on new work (ptone/scion#7826, design §5): an agent the hub
// itself suspended (suspend_reason idle or stalled) is resumed when new
// work arrives for it, with no --wake from the sender. The sender needs
// only the authorization to deliver to the agent: the lifecycle permission
// an explicit wake needs (wakeResumeDenial) is not asked for, because the
// hub took the agent down and bringing it back is the hub's job. A held
// agent is never woken. Gated by the lifecycle.idle_suspend experiment.
//
// Paths whose sender waits for the outcome (a user or agent direct
// message, the web chat primary, a scheduled message fire) wake
// synchronously through wakeAgentForDM, exactly as an explicit wake does.
// Paths with tight budgets or a shared loop (agent mention fan-out, web
// chat secondaries, notification delivery) queue the item here: a tracked
// background goroutine resumes the agent once, through the same
// wakeAgentForDM, and then runs every item queued for it.

// autoWakeMode classifies a target for a sender that delivers right after
// the wake and needs a running agent (the direct-message and scheduled
// paths; autoWakeModeOf).
type autoWakeMode int

const (
	// autoWakeNone: not an auto-wake target; today's behaviour applies.
	autoWakeNone autoWakeMode = iota
	// autoWakeResume: the hub suspended it (shouldAutoWake); resume it,
	// then deliver.
	autoWakeResume
	// autoWakeAwait: a wake of an agent the hub suspended is already in
	// flight (phase starting, reason idle or stalled kept through starting);
	// wait for it, then deliver. Nothing is started, so the sender needs
	// only its delivery authorization.
	autoWakeAwait
)

// autoWakeModeOf classifies agent as read by a synchronous sender (see
// autoWakeMode). It requires the lifecycle.idle_suspend experiment and a
// suspend reason that auto-wakes; holds are checked by the callers. A
// starting agent whose reason is user (a resume of a user-suspended agent)
// or none is autoWakeNone. An error row never carries a reason (any phase
// write other than suspended or starting clears it), so the effective
// resting phase need not be consulted. Both non-none modes go through
// wakeOrAwaitStart, which resumes a suspended target and waits on a
// starting one.
func (s *Server) autoWakeModeOf(agent *store.Agent) autoWakeMode {
	if agent == nil {
		return autoWakeNone
	}
	if agent.HasActiveIdleSuspendClaim(time.Now()) {
		if !s.experimentEnabled(experiments.IdleSuspend) {
			return autoWakeNone
		}
		return autoWakeAwait
	}
	if !state.SuspendReason(agent.SuspendReason).AutoWakes() {
		return autoWakeNone
	}
	var mode autoWakeMode
	switch state.Phase(agent.Phase) {
	case state.PhaseSuspended:
		mode = autoWakeResume
	case state.PhaseStarting:
		mode = autoWakeAwait
	default:
		return autoWakeNone
	}
	if !s.experimentEnabled(experiments.IdleSuspend) {
		return autoWakeNone
	}
	return mode
}

// shouldAutoWake reports whether new work for agent resumes it without an
// explicit wake: the lifecycle.idle_suspend experiment is on, the agent is
// suspended, and the hub suspended it (state.SuspendReason.AutoWakes). A
// row with no suspend reason (suspended by a user, by a hold, or before
// reasons were recorded) never auto-wakes.
func (s *Server) shouldAutoWake(agent *store.Agent) bool {
	return s.autoWakeModeOf(agent) == autoWakeResume
}

// shouldQueueAutoWake reports whether a background-queued delivery path
// (mention fan-out, web chat secondaries, notifications) should queue behind
// runAutoWake: either the agent is already hub-suspended (shouldAutoWake), or
// an idle auto-suspend claim is actively stopping its container. Unlike a
// starting target (whose container is coming up and accepts broker-buffered
// messages), a stopping target must settle and resume before dispatch.
func (s *Server) shouldQueueAutoWake(agent *store.Agent) bool {
	if s.shouldAutoWake(agent) {
		return true
	}
	return agent != nil && agent.HasActiveIdleSuspendClaim(time.Now()) && s.experimentEnabled(experiments.IdleSuspend)
}

// ensureDeliverableBeforeDispatch stamps last_delivery_at under the row lock
// right before dispatching an item to agent. If an idle auto-suspend claimed
// (or completed suspending) the agent since the caller's initial read, it
// awaits settlement and resumes the agent via wakeOrAwaitStart, then marks
// delivery on the woken run.
func (s *Server) ensureDeliverableBeforeDispatch(ctx context.Context, agent *store.Agent) (*AgentDMError, error) {
	if agent == nil || s.store == nil {
		return nil, nil
	}
	fresh, err := s.store.MarkAgentDelivery(ctx, agent.ID)
	if err != nil || fresh == nil {
		return nil, nil
	}
	*agent = *fresh
	if s.autoWakeModeOf(agent) == autoWakeNone {
		return nil, nil
	}
	if dmErr := s.heldTargetRefusal(ctx, agent); dmErr != nil {
		return dmErr, nil
	}
	dmErr, awaitErr := s.wakeOrAwaitStart(ctx, agent)
	if dmErr != nil || awaitErr != nil {
		return dmErr, awaitErr
	}
	if marked, merr := s.store.MarkAgentDelivery(ctx, agent.ID); merr == nil && marked != nil {
		*agent = *marked
	}
	return nil, nil
}

// agentHeldOrUnknown reports whether agentID is held, or its hold could not
// be read; either way it must not be auto-woken.
func (s *Server) agentHeldOrUnknown(ctx context.Context, agentID string) bool {
	held, err := s.agentHeld(ctx, agentID)
	return held || err != nil
}

// autoWakeBudget bounds one background auto-wake: the resume dispatch and
// the readiness wait, or the wait on a start already in progress. It
// matches the web chat wake's resume budget.
//
// It is a variable only as a test seam. Tests that change it must not use
// t.Parallel and must restore the original value (t.Cleanup).
var autoWakeBudget = chatWakeResumeBudget

// autoWakeStopGrace bounds how long hub shutdown waits for background
// auto-wakes in flight before it cancels them.
const autoWakeStopGrace = 5 * time.Second

// autoWakeItem is one piece of work queued behind a background auto-wake.
type autoWakeItem struct {
	// key, when set, coalesces items: an item whose key is already queued
	// for the same wake is dropped (for example one drain of parked
	// notifications covers every notification that asked for it).
	key string
	// deliver runs once the agent is ready, with the agent as re-read
	// after the wake.
	deliver func(ctx context.Context, woken *store.Agent)
	// fail runs when the agent could not be woken; dmErr says why.
	fail func(ctx context.Context, dmErr *AgentDMError)
}

// autoWakeRun is the background wake of one agent and the items waiting
// for it.
type autoWakeRun struct {
	items []autoWakeItem
}

// autoWakeRuns holds the background wakes in flight, one per agent, so
// concurrent items for one agent share a single resume.
type autoWakeRuns struct {
	mu   sync.Mutex
	runs map[string]*autoWakeRun
}

// queueAutoWake queues item behind a background wake of agent, starting
// the wake unless one is already in flight for it. Every queued item runs:
// deliver after the wake succeeds, fail if it does not. It returns false,
// and runs nothing, when the hub is shutting down and the wake cannot
// start; the caller then keeps its item as it would without auto-wake.
func (s *Server) queueAutoWake(agent *store.Agent, item autoWakeItem) bool {
	s.autoWakes.mu.Lock()
	defer s.autoWakes.mu.Unlock()
	if run, ok := s.autoWakes.runs[agent.ID]; ok {
		run.add(item)
		return true
	}
	run := &autoWakeRun{}
	run.add(item)
	target := *agent
	// Started under s.autoWakes.mu, so an item queued meanwhile cannot be
	// added to a run that never starts.
	if !s.autoWakeWork.goTracked(func(ctx context.Context) { s.runAutoWake(ctx, &target, run) }) {
		return false
	}
	if s.autoWakes.runs == nil {
		s.autoWakes.runs = make(map[string]*autoWakeRun)
	}
	s.autoWakes.runs[agent.ID] = run
	return true
}

// add appends item unless an item with the same non-empty key is queued.
// The caller holds autoWakeRuns.mu.
func (r *autoWakeRun) add(item autoWakeItem) {
	if item.key != "" {
		for _, it := range r.items {
			if it.key == item.key {
				return
			}
		}
	}
	r.items = append(r.items, item)
}

// runAutoWake wakes agent once and then runs run's items, including any
// queued while they run, until none is left; the run then ends and a later
// item starts a new one (which finds the agent running).
func (s *Server) runAutoWake(ctx context.Context, agent *store.Agent, run *autoWakeRun) {
	woken, dmErr := s.autoWakeAgent(ctx, agent)
	if dmErr != nil {
		s.messageLog.Warn("auto-wake: agent not woken; queued work is left as it would be without a wake",
			"agent_id", agent.ID, "code", dmErr.Code, "error", dmErr.Message)
	}
	for {
		s.autoWakes.mu.Lock()
		items := run.items
		run.items = nil
		if len(items) == 0 {
			delete(s.autoWakes.runs, agent.ID)
			s.autoWakes.mu.Unlock()
			return
		}
		s.autoWakes.mu.Unlock()
		for _, it := range items {
			if dmErr != nil {
				if it.fail != nil {
					it.fail(ctx, dmErr)
				}
				continue
			}
			if it.deliver != nil {
				it.deliver(ctx, woken)
			}
		}
	}
}

// autoWakeAgent resumes agent for queued work and returns it as re-read
// once ready. It re-reads the agent first: a target held, deleted, or
// suspended by a user since the work was queued is not woken; one already
// running needs nothing. A start already in progress (the row reads
// starting, or WakeDeferred) is waited for, as a scheduled wake does.
func (s *Server) autoWakeAgent(ctx context.Context, agent *store.Agent) (*store.Agent, *AgentDMError) {
	ctx, cancel := context.WithTimeout(ctx, autoWakeBudget)
	defer cancel()

	current, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil || current == nil {
		return nil, &AgentDMError{Code: ErrCodeAgentNotRunning, Message: "agent could not be read for auto-wake", HTTPStatus: http.StatusConflict}
	}
	if dmErr := s.heldTargetRefusal(ctx, current); dmErr != nil {
		return nil, dmErr
	}
	// The same classification as the sync paths decides, so an error row
	// that remembers a suspended resume (wakeAgentForDM acts on the
	// effective resting phase) is never resumed here: a failed resume
	// clears the reason, and that agent is not auto-woken again. A running
	// target needs nothing; a starting one is only waited on.
	switch state.Phase(current.Phase) {
	case state.PhaseRunning, state.PhaseStarting:
	default:
		if s.autoWakeModeOf(current) == autoWakeNone {
			return nil, validateAgentDeliverable(current)
		}
	}

	// A target another start is already bringing up (phase starting, or a
	// held start claim) is waited for, then delivered to (wakeOrAwaitStart).
	dmErr, awaitErr := s.wakeOrAwaitStart(ctx, current)
	if dmErr != nil {
		return nil, dmErr
	}
	if awaitErr != nil {
		return nil, startInProgressDMError(current, awaitErr)
	}
	fresh, err := s.store.GetAgent(ctx, current.ID)
	if err != nil || fresh == nil {
		// The wake succeeded; deliver on the row the wake updated.
		return current, nil
	}
	return fresh, nil
}

// stopAutoWakes refuses new background auto-wakes and waits, up to
// autoWakeStopGrace, for those in flight; any still running is cancelled.
func (s *Server) stopAutoWakes(ctx context.Context) {
	if n := s.autoWakeWork.stop(ctx, autoWakeStopGrace); n > 0 {
		s.agentLifecycleLog.Warn("Auto-wakes still running after the shutdown grace; cancelled them",
			"running", n, "grace", autoWakeStopGrace)
	}
}

// chatSecondaryAutoWakeItem is the work queued behind the background wake of
// a web chat secondary (an @mentioned agent): once it is ready, dispatch its
// stored copy messageID (non-interrupt, as for any secondary) and settle the
// attachment addressee; a failed dispatch or wake marks the copy failed.
func (s *Server) chatSecondaryAutoWakeItem(messageID, addressee, content string, msg *messages.StructuredMessage) autoWakeItem {
	return autoWakeItem{
		deliver: func(ctx context.Context, woken *store.Agent) {
			var err error
			if dispatcher := s.GetDispatcher(); dispatcher == nil {
				err = errors.New("dispatch not available")
			} else {
				dctx, cancel := context.WithTimeout(withDispatchMessageID(ctx, messageID), chatWakeDeliveryBudget)
				if marked, merr := s.store.MarkAgentDelivery(dctx, woken.ID); merr == nil && marked != nil {
					woken = marked
				}
				_, err = s.holdOrDispatch(dctx, woken, messageID, nil, func() error {
					return dispatchWithBrokerRetry(dctx, nil, dispatcher, woken, content, false, msg)
				})
				cancel()
			}
			// The settle writes run on a short detached context, so a wake
			// cut short at shutdown still records its outcome.
			fctx, fcancel := finalizationContext(ctx)
			defer fcancel()
			s.finishAttachmentAddressee(fctx, addressee, err)
			if err != nil {
				s.messageLog.Error("Failed to dispatch mention after auto-wake", "slug", woken.Slug, "error", err)
				_ = s.markFailed(fctx, messageID, err.Error())
			}
		},
		fail: func(ctx context.Context, dmErr *AgentDMError) {
			fctx, fcancel := finalizationContext(ctx)
			defer fcancel()
			err := fmt.Errorf("auto-wake failed: %s", dmErr.Message)
			s.finishAttachmentAddressee(fctx, addressee, err)
			_ = s.markFailed(fctx, messageID, err.Error())
		},
	}
}

// autoWakeNotificationSubscriber queues a background wake of subscriber, a
// suspended agent with a parked notification, when the hub suspended it
// (shouldAutoWake) and it is not held. After the wake the parked
// notifications of agents on its broker are drained
// (drainUndispatchedNotifications), which delivers them now that it is
// running; one drain per wake covers every notification that asked. If the
// wake fails they stay parked, as without auto-wake, and the sweep tries
// again (autoWakeParkedNotificationSubscribers). It reports whether a wake
// was queued.
func (s *Server) autoWakeNotificationSubscriber(subscriber *store.Agent) bool {
	if !s.shouldQueueAutoWake(subscriber) {
		return false
	}
	return s.queueAutoWake(subscriber, autoWakeItem{
		key: "notifications",
		deliver: func(ctx context.Context, woken *store.Agent) {
			s.drainUndispatchedNotifications(ctx, woken.RuntimeBrokerID)
		},
		fail: func(_ context.Context, dmErr *AgentDMError) {
			s.agentLifecycleLog.Warn("auto-wake: notifications stay parked; the subscriber could not be woken",
				"agent_id", subscriber.ID, "code", dmErr.Code, "error", dmErr.Message)
		},
	})
}

// autoWakeParkedNotificationSubscribers is the notification sweep's
// auto-wake step: with lifecycle.idle_suspend on, every unheld agent the hub
// suspended (suspend_reason idle or stalled) that has a parked notification
// gets a background wake (autoWakeNotificationSubscriber).
func (s *Server) autoWakeParkedNotificationSubscribers(ctx context.Context) {
	if !s.experimentEnabled(experiments.IdleSuspend) {
		return
	}
	// One batch read selects the candidates; each queued wake re-reads its
	// agent before acting (autoWakeAgent), so the decision to resume is
	// still made on a fresh row.
	subscribers, err := s.store.ListParkedNotificationSubscribers(ctx, autoWakeSuspendReasons)
	if err != nil {
		s.agentLifecycleLog.Error("notification-dispatch-sweep: parked subscriber query failed", "error", err)
		return
	}
	for i := range subscribers {
		s.autoWakeNotificationSubscriber(&subscribers[i])
	}
}

// autoWakeSuspendReasons are the suspend reasons that auto-wake
// (state.SuspendReason.AutoWakes), for store queries.
var autoWakeSuspendReasons = []string{string(state.SuspendReasonIdle), string(state.SuspendReasonStalled)}

// convertHubSuspendToUser handles a user SUSPEND action on an agent that the
// hub already suspended (suspend_reason idle or stalled): it rewrites
// suspend_reason to user in place, with no runtime broker action, so future
// messages do not auto-wake the agent. It returns true when it handled the
// request (writing the response to w), or false when the caller should proceed
// with the normal running-only suspend path (for example when the agent is
// already user-suspended, not suspended, or a concurrent wake changed its
// phase before the CAS write).
func (s *Server) convertHubSuspendToUser(ctx context.Context, w http.ResponseWriter, agent *store.Agent) bool {
	if !s.experimentEnabled(experiments.IdleSuspend) {
		return false
	}
	if agent == nil || agent.Phase != string(state.PhaseSuspended) ||
		!state.SuspendReason(agent.SuspendReason).AutoWakes() ||
		reincarnationInFlight(agent) {
		return false
	}
	if agent.StartClaimID != "" || agent.HasActiveIdleSuspendClaim(time.Now()) || s.lifecycleOps.active(agent.ID) {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"Agent has a lifecycle operation in progress; retry once it settles.", nil)
		return true
	}
	err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:            string(state.PhaseSuspended),
		SuspendReason:    string(state.SuspendReasonUser),
		IfPhase:          string(state.PhaseSuspended),
		IfNoStartClaim:   true,
		IfAutoWakeReason: true,
	})
	if errors.Is(err, store.ErrClaimPredicate) {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"Agent has a lifecycle operation in progress; retry once it settles.", nil)
		return true
	}
	if errors.Is(err, store.ErrPhaseMismatch) {
		if fresh, gerr := s.store.GetAgent(ctx, agent.ID); gerr == nil && fresh != nil {
			*agent = *fresh
		}
		return false
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return true
	}
	s.syncAgentAfterStopRecord(ctx, agent, string(state.PhaseSuspended), string(state.SuspendReasonUser))
	s.events.PublishAgentStatus(ctx, agent)
	respAgent := *agent
	respAgent.AppliedConfig, respAgent.PendingConfig = s.redactAgentConfigsForResponse(
		agent.AppliedConfig, agent.PendingConfig, canViewAgentEnv(ctx, s, agent))
	writeJSON(w, http.StatusOK, agentLifecycleResponse{Agent: &respAgent})
	return true
}
