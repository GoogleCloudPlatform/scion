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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// NotificationStatusHalted is the notification trigger and status of an
// agent halted by the platform (ptone/scion#4853).
const NotificationStatusHalted = "HALTED"

// reportOfOtherRun reports whether a status report from run reportRunID is
// about a run other than the agent's current run rowRunID. The store then
// applies nothing of it but the last-seen time (AgentStatusUpdate.ReportRunID),
// so the handlers skip their own phase-driven side effects for it too. A
// report without a run (an older broker or token) is not one.
func reportOfOtherRun(reportRunID, rowRunID string) bool {
	return reportRunID != "" && reportRunID != rowRunID
}

// haltedDispatchTimeout bounds one halt's notifications and hooks: well
// above a subscriber agent's 30s delivery retry and the hooks' own
// timeouts, so only a stuck store or delivery hits it.
const haltedDispatchTimeout = 5 * time.Minute

// onAgentHalted runs once for each halt, on the hub instance whose write
// moved the agent into phase halted (the store reports it through
// AgentStatusResult, or the container-missing CAS returns a halted row).
// haltedFrom is the phase the write would otherwise have stored, when it
// differs from the agent's previous phase, or "".
//
// The halt write is the claim: it commits once per run, so notifying and
// firing hooks here, in-process, does so once. The bus consumers (the
// notification dispatcher's and the hook evaluator's handleEvent) skip
// halted events, which every instance receives.
//
// The notifications and hooks run in the background, on a context detached
// from the caller's and bounded by haltedDispatchTimeout: a subscriber
// agent's delivery retries for up to 30s, which must not hold up the
// heartbeat or status report that halted the agent. Delivery is
// at-most-once, as for the bus. The returned channel is closed when they
// are done (at once when there is nothing to run). s.haltedDispatches
// counts them as a test handle only: shutdown does not wait for it, and
// each dispatch is bounded by haltedDispatchTimeout.
func (s *Server) onAgentHalted(ctx context.Context, agent *store.Agent, haltedFrom string) <-chan struct{} {
	done := make(chan struct{})
	if agent == nil {
		close(done)
		return done
	}
	s.agentLifecycleLog.Warn("agent halted by the platform",
		"agent_id", agent.ID, "agent", agent.Slug, "project_id", agent.ProjectID,
		"exit_reason", agent.ExitReason, "halted_from", haltedFrom)
	s.mu.RLock()
	nd := s.notificationDispatcher
	ev := s.lifecycleHookEvaluator
	s.mu.RUnlock()
	if nd == nil && ev == nil {
		close(done)
		return done
	}
	snapshot := *agent
	s.haltedDispatches.Add(1)
	go func() {
		defer s.haltedDispatches.Done()
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				s.agentLifecycleLog.Error("halted agent notification panicked",
					"agent_id", snapshot.ID, "panic", fmt.Sprint(rec))
			}
		}()
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), haltedDispatchTimeout)
		defer cancel()
		if nd != nil {
			nd.dispatchHalted(dctx, &snapshot, haltedFrom)
		}
		if ev != nil {
			ev.fireHalted(dctx, &snapshot, haltedFrom)
		}
	}()
	return done
}

// haltedNotificationMessage is the text of a HALTED notification:
// what happened, what happens to the workspace on the next start, and how
// to resume.
//
// The reason is its fixed display label (state.ExitReason.Label), and the
// agent's free-text message is not included: the sentence is attributed to
// the platform, so it holds only values the hub controls.
func haltedNotificationMessage(agent *store.Agent) string {
	return fmt.Sprintf("%s was halted by the platform (%s). %s. Resume: scion start %s.",
		agent.Slug, state.ExitReason(agent.ExitReason).Label(), haltedWorkspaceWording(agent), agent.Slug)
}

// haltedWorkspaceWording says what a start does to a halted agent's
// workspace: a Kubernetes EmptyDir workspace is re-cloned; a workspace
// whose placement is unknown (no runtime recorded, or a Kubernetes agent
// with no reported placement) may be; any other is kept.
func haltedWorkspaceWording(agent *store.Agent) string {
	switch {
	case hasEphemeralWorkspace(agent):
		return "Workspace re-cloned"
	case agent.Runtime == "" || (isKubernetesRuntimeType(agent.Runtime) && agent.WorkspacePlacement == ""):
		return "Workspace may be re-cloned"
	default:
		return "Workspace kept"
	}
}

// dispatchHalted sends the notifications for one halt of agent. A
// subscription matches when it has the HALTED trigger, or the ERROR trigger
// and the halt replaced an error write (haltedFrom error), so an ERROR
// subscriber is told as before. One notification per subscriber; the
// last-status dedup is not consulted, because a halt happens once per run.
// The agent's creator is notified even with no matching subscription (see
// notifyHaltedCreator).
func (nd *NotificationDispatcher) dispatchHalted(ctx context.Context, agent *store.Agent, haltedFrom string) {
	if nd == nil || agent == nil || agent.ID == "" {
		return
	}
	agentSubs, err := nd.store.GetNotificationSubscriptions(ctx, agent.ID)
	if err != nil {
		nd.log.Error("Failed to get agent notification subscriptions for halted agent",
			"agent_id", agent.ID, "error", err)
		agentSubs = nil
	}
	projectSubs, err := nd.store.GetNotificationSubscriptionsByProjectScope(ctx, agent.ProjectID)
	if err != nil {
		nd.log.Error("Failed to get project notification subscriptions for halted agent",
			"project_id", agent.ProjectID, "error", err)
		projectSubs = nil
	}

	message := haltedNotificationMessage(agent)
	seen := make(map[string]bool)
	subscribed := make(map[string]bool)
	allSubs := append(agentSubs, projectSubs...)
	for i := range allSubs {
		sub := &allSubs[i]
		key := sub.SubscriberType + ":" + sub.SubscriberID
		subscribed[key] = true
		if seen[key] {
			continue
		}
		status := ""
		switch {
		case sub.MatchesActivity(NotificationStatusHalted):
			status = NotificationStatusHalted
		case haltedFrom == string(state.PhaseError) && sub.MatchesActivity("ERROR"):
			// The write would have been an error: the ERROR subscriber
			// gets its ERROR notification, with the halt's text.
			status = "ERROR"
		default:
			continue
		}
		seen[key] = true
		nd.storeAndDispatchMessage(ctx, sub, haltedEvent(agent, status), agent, message)
	}
	nd.notifyHaltedCreator(ctx, agent, message, subscribed)
}

// haltedEvent is the event a halt's notification is built from.
func haltedEvent(agent *store.Agent, status string) AgentStatusEvent {
	return AgentStatusEvent{
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		Phase:     string(state.PhaseHalted),
		Activity:  status,
	}
}

// notifyHaltedCreator sends one HALTED notification to agent's creator
// when the creator has no subscription to the agent or its project
// (subscribed holds every subscriber of either, keyed by type and ID): a
// creator who subscribed chose their triggers, and gets HALTED only by
// selecting it. A user creator is
// addressed by user ID; an agent creator by its slug, and only while it is
// live in the same project (the agent delivery resolves the subscriber by
// slug in the project). Otherwise nothing is sent. The notification row has
// no subscription (store.CreatorFallbackSubscriptionID).
func (nd *NotificationDispatcher) notifyHaltedCreator(ctx context.Context, agent *store.Agent, message string, subscribed map[string]bool) {
	if agent.CreatedBy == "" {
		return
	}
	sub := store.NotificationSubscription{
		ID:                store.CreatorFallbackSubscriptionID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agent.ID,
		ProjectID:         agent.ProjectID,
		TriggerActivities: []string{NotificationStatusHalted},
	}
	_, err := nd.store.GetUser(ctx, agent.CreatedBy)
	switch {
	case err == nil:
		sub.SubscriberType = store.SubscriberTypeUser
		sub.SubscriberID = agent.CreatedBy
	case !errors.Is(err, store.ErrNotFound):
		nd.log.Warn("Failed to look up a halted agent's creator as a user; creator not notified",
			"agent_id", agent.ID, "created_by", agent.CreatedBy, "error", err)
		return
	default:
		creator, err := nd.store.GetAgent(ctx, agent.CreatedBy)
		if err != nil || !creator.DeletedAt.IsZero() || creator.ProjectID != agent.ProjectID {
			nd.log.Debug("Halted agent's creator is not a live agent of its project; creator not notified",
				"agent_id", agent.ID, "created_by", agent.CreatedBy, "error", err)
			return
		}
		sub.SubscriberType = store.SubscriberTypeAgent
		sub.SubscriberID = creator.Slug
	}
	if subscribed[sub.SubscriberType+":"+sub.SubscriberID] {
		return
	}
	nd.storeAndDispatchMessage(ctx, &sub, haltedEvent(agent, NotificationStatusHalted), agent, message)
}
