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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Scheduled message wake (ptone/scion#7734): a message event whose payload
// sets wake resumes a suspended target before delivery, through the shared
// direct-message wake path (wakeAgentForDM). Chat scheduled sends
// (chat_scheduled_send.go) never wake.

// scheduledWakePermission is the permission a scheduled wake exercises: the
// lifecycle permission a direct-message wake needs (wakeResumeDenial).
const scheduledWakePermission = "agent.lifecycle"

// scheduledWakeBudget bounds a scheduled wake (the resume dispatch and the
// readiness wait, or the wait on a start already in progress). It replaces
// the fire handler's deadline for the wake step, which is too short for a
// resume; the dispatch that follows gets its own retry bound. It matches the
// chat wake's resume budget (chatWakeResumeBudget).
//
// It is a variable only as a test seam. Tests that change it must not use
// t.Parallel and must restore the original value (t.Cleanup).
var scheduledWakeBudget = 90 * time.Second

// errScheduledWakeNotReady is recorded on a wake fire that found another
// start (or a stop) of the target in progress, and the target did not become
// ready within the wake budget. There is no sender to retry, so the fire
// fails rather than reporting a delivery that did not happen.
var errScheduledWakeNotReady = errors.New("target agent was already starting or stopping and did not become ready; message not delivered")

// scheduledPayloadWakes reports whether a stored message payload requests a
// wake. A payload that does not decode does not.
func scheduledPayloadWakes(raw string) bool {
	var p MessageEventPayload
	if raw == "" || json.Unmarshal([]byte(raw), &p) != nil {
		return false
	}
	return p.Wake
}

// scheduledWakeNeedsResume reports whether a wake of agent resumes it, the
// case the lifecycle permission is required for: its effective resting
// phase is suspended.
func scheduledWakeNeedsResume(agent *store.Agent) bool {
	return state.Phase(store.EffectiveRestingPhase(agent)) == state.PhaseSuspended
}

// authorizeScheduledWake decides a scheduled wake under the event's
// authorization revision, after the send itself was authorized
// (authorizeScheduledMessageFire): a target that the wake would resume needs
// the permission a direct-message wake needs (agentLifecycleAllowed) for the
// revision's identity and, for a user principal, the revision's frozen
// ceiling must allow it too. A target the wake would not resume needs
// nothing more. A denial returns an error naming the cause, for the log.
func (s *Server) authorizeScheduledWake(ctx context.Context, auth ScheduledAuthority, identity Identity, agent *store.Agent) error {
	if !scheduledWakeNeedsResume(agent) {
		return nil
	}
	if auth.PrincipalKind == store.DelegationPrincipalUser &&
		!EffectCeilingAllows(auth.Ceiling, scheduledWakePermission, false) {
		return fmt.Errorf("%w: revision ceiling does not allow %s", errScheduledAuthorityDenied, scheduledWakePermission)
	}
	if !s.agentLifecycleAllowed(ctx, identity, agent) {
		return fmt.Errorf("scheduled_message_denied: not permitted to resume agent %s", agent.Slug)
	}
	return nil
}

// wakeScheduledTarget runs the wake of a scheduled message fire whose
// authorization, standing and wake permission have passed. It returns nil
// when the target is ready for delivery (resumed, already running, or
// brought up by a start that was already in progress), or the error the
// event records. On an error nothing is dispatched.
//
// The wake runs on a budget of its own (scheduledWakeBudget), detached from
// the fire handler's deadline but keeping its values.
func (s *Server) wakeScheduledTarget(ctx context.Context, evt store.ScheduledEvent, agent *store.Agent) error {
	wakeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledWakeBudget)
	defer cancel()

	result, dmErr := s.wakeAgentForDM(wakeCtx, agent)
	if dmErr != nil {
		slog.Warn("Scheduler: scheduled message wake failed",
			"eventID", evt.ID, "scheduleID", evt.ScheduleID, "agent_id", agent.ID,
			"code", dmErr.Code, "error", dmErr.Message)
		return fmt.Errorf("failed to wake agent %s: %s: %s", agent.Slug, dmErr.Code, dmErr.Message)
	}
	if result == nil || result.Outcome != WakeDeferred {
		return nil
	}

	// Another start (or a stop) holds the target's start claim. A
	// scheduled message has no stored row to keep and no sender to retry,
	// so wait for that start to bring the target up, then deliver; a target
	// that does not become ready fails the fire.
	slog.Info("Scheduler: scheduled message wake deferred; waiting for the start in progress",
		"eventID", evt.ID, "agent_id", agent.ID, "holder", string(agent.StartClaimKind))
	err := s.waitForStartToBegin(wakeCtx, agent.ID, wakeReadyTimeout)
	var activity state.Activity
	if err == nil {
		activity, err = s.waitForAgentReady(wakeCtx, agent.ID, wakeReadyTimeout)
	}
	if err != nil {
		slog.Warn("Scheduler: scheduled message target did not become ready after a deferred wake",
			"eventID", evt.ID, "agent_id", agent.ID, "error", err)
		return errScheduledWakeNotReady
	}
	// Deliver to the run the other start brought up, not to the row read
	// before it.
	if fresh, gerr := s.store.GetAgent(wakeCtx, agent.ID); gerr == nil && fresh != nil {
		*agent = *fresh
	}
	agent.Activity = string(activity)
	return nil
}

// scheduledWakePollInterval is how often waitForStartToBegin re-reads the
// target.
const scheduledWakePollInterval = 100 * time.Millisecond

// waitForStartToBegin waits, up to timeout, while agentID is still at rest
// in the suspended phase: a start that holds the start claim may not have
// written starting yet, and waitForAgentReady treats a suspended row as a
// failed start.
func (s *Server) waitForStartToBegin(ctx context.Context, agentID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		a, err := s.store.GetAgent(ctx, agentID)
		if err != nil {
			return fmt.Errorf("failed to check agent status: %w", err)
		}
		if !scheduledWakeNeedsResume(a) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the start in progress did not begin")
		case <-time.After(scheduledWakePollInterval):
		}
	}
}
