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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// thirdPartyStopDispatchTimeout bounds the background delivery of one
// third-party stop/suspend creator notification (matching
// haltedDispatchTimeout).
const thirdPartyStopDispatchTimeout = 5 * time.Minute

// thirdPartyReasonMaxRunes caps the optional caller-supplied reason included
// in a third-party stop/suspend notification.
const thirdPartyReasonMaxRunes = 200

// AgentLifecycleStopRequest is the optional JSON body for the "stop",
// "suspend", and "stop-all" lifecycle actions.
type AgentLifecycleStopRequest struct {
	Reason string `json:"reason,omitempty"`
}

type lifecycleStopReasonKey struct{}

func withLifecycleStopReason(ctx context.Context, reason string) context.Context {
	if reason == "" {
		return ctx
	}
	return context.WithValue(ctx, lifecycleStopReasonKey{}, reason)
}

func lifecycleStopReasonFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if reason, ok := ctx.Value(lifecycleStopReasonKey{}).(string); ok {
		return reason
	}
	return ""
}

// parseLifecycleStopReason extracts an optional reason from a stop, suspend,
// or stop-all request body. Missing, empty, or unreadable bodies return "".
func parseLifecycleStopReason(r *http.Request) string {
	if r == nil || r.Body == nil || r.ContentLength == 0 {
		return ""
	}
	var req AgentLifecycleStopRequest
	if err := readJSON(r, &req); err != nil {
		return ""
	}
	return req.Reason
}

// notifyThirdPartyLifecycleAction notifies an agent's creator when a third
// party (an authenticated actor other than the creator and other than the
// agent itself) stops or suspends the agent (ptone/scion#8683).
//
// Auto-suspend, platform halts, and other hub-initiated actions carry no
// request identity on ctx and are skipped automatically. Creator-initiated and
// self-initiated actions are also skipped.
//
// The notification row is persisted synchronously before returning so that the
// subsequent PublishAgentStatus event's NotificationDispatcher.handleEvent
// sees the stored notification on the creator's matching subscription (if any)
// and deduplicates against it instead of emitting a second generic notice.
// Delivery to the creator (broker retry for agent creators, SSE/inbox/channels
// for user creators) runs in a detached background goroutine so a slow or
// unreachable subscriber never holds up the HTTP response.
func (s *Server) notifyThirdPartyLifecycleAction(ctx context.Context, agent *store.Agent, action, reason string) {
	if agent == nil || agent.ID == "" || agent.CreatedBy == "" {
		return
	}
	if action != api.AgentActionStop && action != api.AgentActionSuspend {
		return
	}
	identity := GetIdentityFromContext(ctx)
	if identity == nil || identity.ID() == "" {
		return
	}
	// Skip creator-initiated and self-initiated actions.
	if identity.ID() == agent.CreatedBy || identity.ID() == agent.ID {
		return
	}

	s.mu.RLock()
	nd := s.notificationDispatcher
	s.mu.RUnlock()
	if nd == nil {
		return
	}

	phase := string(state.PhaseStopped)
	status := "STOPPED"
	if action == api.AgentActionSuspend {
		phase = string(state.PhaseSuspended)
		status = "SUSPENDED"
	}

	sub := nd.resolveThirdPartyCreatorSubscription(ctx, agent, status)
	if sub == nil {
		return
	}

	actorLabel := s.thirdPartyActorLabel(ctx, identity)
	now := time.Now().UTC()
	message := thirdPartyLifecycleMessage(agent.Slug, action, actorLabel, now, reason)
	evt := AgentStatusEvent{
		AgentID:   agent.ID,
		ProjectID: agent.ProjectID,
		Phase:     phase,
	}

	notif := nd.storeNotification(ctx, sub, evt, message)
	if notif == nil {
		return
	}

	subCopy := *sub
	agentSnapshot := *agent
	s.thirdPartyStopDispatches.Add(1)
	go func() {
		defer s.thirdPartyStopDispatches.Done()
		defer func() {
			if rec := recover(); rec != nil {
				s.agentLifecycleLog.Error("third-party lifecycle notification panicked",
					"agent_id", agentSnapshot.ID, "action", action, "panic", fmt.Sprint(rec))
			}
		}()
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), thirdPartyStopDispatchTimeout)
		defer cancel()
		nd.dispatchStoredNotification(dctx, &subCopy, notif, &agentSnapshot)
	}()
}

// resolveThirdPartyCreatorSubscription resolves the notification subscription
// to use when notifying agent's creator of a third-party stop/suspend.
//
// If the creator already has an agent-scoped or project-scoped subscription
// matching status, that subscription is returned (checked in the same
// agent-before-project order as handleEvent so handleEvent deduplicates
// against the stored row). Otherwise a synthetic creator-fallback subscription
// (store.CreatorFallbackSubscriptionID) is returned. Returns nil if the
// creator cannot be resolved to a user or a live agent in the same project.
func (nd *NotificationDispatcher) resolveThirdPartyCreatorSubscription(ctx context.Context, agent *store.Agent, status string) *store.NotificationSubscription {
	var subscriberType, subscriberID string
	_, err := nd.store.GetUser(ctx, agent.CreatedBy)
	switch {
	case err == nil:
		subscriberType = store.SubscriberTypeUser
		subscriberID = agent.CreatedBy
	case !errors.Is(err, store.ErrNotFound):
		nd.log.Warn("Failed to look up agent creator as user for third-party lifecycle notice",
			"agent_id", agent.ID, "created_by", agent.CreatedBy, "error", err)
		return nil
	default:
		creator, gerr := nd.store.GetAgent(ctx, agent.CreatedBy)
		if gerr != nil || !creator.DeletedAt.IsZero() || creator.ProjectID != agent.ProjectID {
			nd.log.Debug("Agent creator is not a live agent of its project; third-party lifecycle notice skipped",
				"agent_id", agent.ID, "created_by", agent.CreatedBy, "error", gerr)
			return nil
		}
		subscriberType = store.SubscriberTypeAgent
		subscriberID = creator.Slug
	}

	agentSubs, err := nd.store.GetNotificationSubscriptions(ctx, agent.ID)
	if err != nil {
		nd.log.Warn("Failed to get agent notification subscriptions for third-party lifecycle notice",
			"agent_id", agent.ID, "error", err)
		agentSubs = nil
	}
	projectSubs, err := nd.store.GetNotificationSubscriptionsByProjectScope(ctx, agent.ProjectID)
	if err != nil {
		nd.log.Warn("Failed to get project notification subscriptions for third-party lifecycle notice",
			"project_id", agent.ProjectID, "error", err)
		projectSubs = nil
	}

	allSubs := append(agentSubs, projectSubs...)
	for i := range allSubs {
		sub := &allSubs[i]
		if sub.SubscriberType == subscriberType && sub.SubscriberID == subscriberID && sub.MatchesActivity(status) {
			matched := *sub
			return &matched
		}
	}

	return &store.NotificationSubscription{
		ID:                store.CreatorFallbackSubscriptionID,
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agent.ID,
		ProjectID:         agent.ProjectID,
		SubscriberType:    subscriberType,
		SubscriberID:      subscriberID,
		TriggerActivities: []string{strings.ToUpper(status)},
	}
}

// thirdPartyActorLabel formats a readable, single-line label for the actor who
// stopped or suspended an agent.
func (s *Server) thirdPartyActorLabel(ctx context.Context, identity Identity) string {
	switch ident := identity.(type) {
	case UserIdentity:
		name := sanitizeNoticeLabel(ident.DisplayName())
		email := sanitizeNoticeLabel(ident.Email())
		if name == "" && email == "" && s.store != nil {
			if u, err := s.store.GetUser(ctx, ident.ID()); err == nil && u != nil {
				name = sanitizeNoticeLabel(u.DisplayName)
				email = sanitizeNoticeLabel(u.Email)
			}
		}
		switch {
		case name != "" && email != "" && !strings.EqualFold(name, email):
			return fmt.Sprintf("%s (%s)", name, email)
		case name != "":
			return name
		case email != "":
			return email
		default:
			return "user:" + ident.ID()
		}
	case AgentIdentity:
		if s.store != nil {
			if actorAgent, err := s.store.GetAgent(ctx, ident.ID()); err == nil && actorAgent != nil && actorAgent.Slug != "" {
				return "agent:" + sanitizeNoticeLabel(actorAgent.Slug)
			}
		}
		return "agent:" + ident.ID()
	default:
		kind := sanitizeNoticeLabel(identity.Type())
		if kind != "" {
			return kind + ":" + identity.ID()
		}
		return identity.ID()
	}
}

// thirdPartyLifecycleMessage formats the creator notification for a
// third-party stop or suspend, naming the action, actor, UTC time, and any
// reason given.
func thirdPartyLifecycleMessage(agentSlug, action, actorLabel string, at time.Time, reason string) string {
	actionWord := "stopped"
	if action == api.AgentActionSuspend {
		actionWord = "suspended"
	}
	msg := fmt.Sprintf("%s was %s by %s at %s", agentSlug, actionWord, actorLabel, at.UTC().Format(time.RFC3339))
	if cleanReason := cleanNoticeText(reason, thirdPartyReasonMaxRunes); cleanReason != "" {
		msg += ": " + cleanReason
	}
	return msg
}
