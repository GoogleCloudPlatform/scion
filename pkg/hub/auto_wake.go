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

// shouldAutoWake reports whether new work for agent resumes it without an
// explicit wake: the lifecycle.idle_suspend experiment is on, the agent is
// suspended, and the hub suspended it (state.SuspendReason.AutoWakes). A
// row with no suspend reason (suspended by a user, by a hold, or before
// reasons were recorded) never auto-wakes.
func (s *Server) shouldAutoWake(agent *store.Agent) bool {
	if agent == nil || state.Phase(agent.Phase) != state.PhaseSuspended {
		return false
	}
	if !state.SuspendReason(agent.SuspendReason).AutoWakes() {
		return false
	}
	return s.experimentEnabled(experiments.IdleSuspend)
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
// running needs nothing. A start already in progress (WakeDeferred) is
// waited for, as a scheduled wake does.
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
	if state.Phase(current.Phase) == state.PhaseSuspended && !s.shouldAutoWake(current) {
		return nil, validateAgentDeliverable(current)
	}

	result, dmErr := s.wakeAgentForDM(ctx, current)
	if dmErr != nil {
		return nil, dmErr
	}
	if result != nil && result.Outcome == WakeDeferred {
		err := s.waitForStartToBegin(ctx, current.ID, wakeReadyTimeout)
		if err == nil {
			_, err = s.waitForAgentReady(ctx, current.ID, wakeReadyTimeout)
		}
		if err != nil {
			return nil, &AgentDMError{
				Code:       ErrCodeUnavailable,
				Message:    "agent was already starting and did not become ready",
				HTTPStatus: http.StatusServiceUnavailable,
			}
		}
	}
	fresh, err := s.store.GetAgent(ctx, current.ID)
	if err != nil || fresh == nil {
		// The wake succeeded; deliver on the row the wake updated.
		return current, nil
	}
	return fresh, nil
}

// waitAutoWakeIdle waits until every background auto-wake has ended.
// Tests use it; shutdown uses stopAutoWakes.
func (s *Server) waitAutoWakeIdle() {
	s.autoWakeWork.wg.Wait()
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
			dispatcher := s.GetDispatcher()
			if dispatcher == nil {
				s.finishAttachmentAddressee(ctx, addressee, errors.New("dispatch not available"))
				_ = s.markFailed(ctx, messageID, "dispatch not available")
				return
			}
			dctx, cancel := context.WithTimeout(withDispatchMessageID(ctx, messageID), chatWakeDeliveryBudget)
			defer cancel()
			if err := dispatchWithBrokerRetry(dctx, nil, dispatcher, woken, content, false, msg); err != nil {
				s.messageLog.Error("Failed to dispatch mention after auto-wake", "slug", woken.Slug, "error", err)
				s.finishAttachmentAddressee(ctx, addressee, err)
				_ = s.markFailed(ctx, messageID, err.Error())
				return
			}
			s.finishAttachmentAddressee(ctx, addressee, nil)
		},
		fail: func(ctx context.Context, dmErr *AgentDMError) {
			err := fmt.Errorf("auto-wake failed: %s", dmErr.Message)
			s.finishAttachmentAddressee(ctx, addressee, err)
			_ = s.markFailed(ctx, messageID, err.Error())
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
	if !s.shouldAutoWake(subscriber) {
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
	ids, err := s.store.ListParkedNotificationSubscribers(ctx, autoWakeSuspendReasons)
	if err != nil {
		s.agentLifecycleLog.Error("notification-dispatch-sweep: parked subscriber query failed", "error", err)
		return
	}
	for _, id := range ids {
		subscriber, err := s.store.GetAgent(ctx, id)
		if err != nil || subscriber == nil {
			continue
		}
		s.autoWakeNotificationSubscriber(subscriber)
	}
}

// autoWakeSuspendReasons are the suspend reasons that auto-wake
// (state.SuspendReason.AutoWakes), for store queries.
var autoWakeSuspendReasons = []string{string(state.SuspendReasonIdle), string(state.SuspendReasonStalled)}
