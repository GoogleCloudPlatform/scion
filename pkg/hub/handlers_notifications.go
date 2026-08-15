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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// notificationSubscriber identifies the caller of the notification endpoints.
//
// Users are global: their user ID is unique hub-wide. Agents are not — an agent
// subscribes under its slug (matching the subscriber ID the dispatcher writes
// for agent subscriptions, see notifications.go), and slugs are unique only
// within a project (pkg/ent/schema/agent.go). The subscriber identity of an
// agent is therefore the pair (ProjectID, ID), and every read and ownership
// check for an agent caller must qualify by project — the store keys
// notifications and subscriptions on (subscriberType, subscriberID) alone.
type notificationSubscriber struct {
	Type      string // store.SubscriberTypeUser or store.SubscriberTypeAgent
	ID        string // user ID, or agent slug
	ProjectID string // agent callers only; empty for users
}

// isAgent reports whether the caller is an agent.
func (n *notificationSubscriber) isAgent() bool {
	return n.Type == store.SubscriberTypeAgent
}

// ownsSubscription reports whether sub belongs to this caller.
func (n *notificationSubscriber) ownsSubscription(sub *store.NotificationSubscription) bool {
	if sub.SubscriberType != n.Type || sub.SubscriberID != n.ID {
		return false
	}
	return !n.isAgent() || sub.ProjectID == n.ProjectID
}

// ownsNotification reports whether notif was addressed to this caller.
func (n *notificationSubscriber) ownsNotification(notif *store.Notification) bool {
	if notif.SubscriberType != n.Type || notif.SubscriberID != n.ID {
		return false
	}
	return !n.isAgent() || notif.ProjectID == n.ProjectID
}

// scopeNotifications drops rows outside the caller's project. Agent slugs are
// reused across projects, so a store lookup by subscriber ID alone can return
// another project's rows.
func (n *notificationSubscriber) scopeNotifications(notifs []store.Notification) []store.Notification {
	if !n.isAgent() {
		return notifs
	}
	scoped := make([]store.Notification, 0, len(notifs))
	for _, notif := range notifs {
		if notif.ProjectID == n.ProjectID {
			scoped = append(scoped, notif)
		}
	}
	return scoped
}

// scopeSubscriptions drops rows outside the caller's project, for the same
// reason as scopeNotifications.
func (n *notificationSubscriber) scopeSubscriptions(subs []store.NotificationSubscription) []store.NotificationSubscription {
	if !n.isAgent() {
		return subs
	}
	scoped := make([]store.NotificationSubscription, 0, len(subs))
	for _, sub := range subs {
		if sub.ProjectID == n.ProjectID {
			scoped = append(scoped, sub)
		}
	}
	return scoped
}

// notificationCaller resolves the notification subscriber identity of the
// caller. Returns nil when the request carries no usable identity; returns an
// error only when the identity could not be resolved because of a store
// failure.
func (s *Server) notificationCaller(ctx context.Context) (*notificationSubscriber, error) {
	if user := GetUserIdentityFromContext(ctx); user != nil {
		return &notificationSubscriber{Type: store.SubscriberTypeUser, ID: user.ID()}, nil
	}

	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		return nil, nil
	}

	// The slug is not on the token, so the agent record is needed to build the
	// subscriber identity.
	agent, err := s.store.GetAgent(ctx, agentIdent.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A valid token for a deleted agent, or one issued for a record
			// that never existed. Worth a line: it should not happen.
			slog.Warn("Notification caller token names an unknown agent",
				"agentID", agentIdent.ID(), "projectID", agentIdent.ProjectID())
			return nil, nil
		}
		return nil, err
	}
	// The token's project is what authorization is granted against; a record
	// that disagrees with it must not be usable.
	if agent.ProjectID != agentIdent.ProjectID() {
		slog.Warn("Notification caller token project disagrees with the agent record",
			"agentID", agentIdent.ID(), "tokenProjectID", agentIdent.ProjectID(), "agentProjectID", agent.ProjectID)
		return nil, nil
	}

	return &notificationSubscriber{
		Type:      store.SubscriberTypeAgent,
		ID:        agent.Slug,
		ProjectID: agent.ProjectID,
	}, nil
}

// agentMaySubscribe reports whether an agent caller is allowed to create the
// requested subscription. Both the project the subscription is filed under and
// the agent it watches must belong to the caller's project — otherwise an agent
// could file a subscription in its own project that watches a foreign agent and
// receive that agent's activity transitions. User callers are unrestricted.
//
// cleared memoizes agent IDs already found to be in the caller's project, so a
// bulk request costs one lookup per distinct watched agent rather than one per
// entry. Pass nil when there is nothing to share.
//
// Returns false when a response has already been written.
func (s *Server) agentMaySubscribe(w http.ResponseWriter, r *http.Request, caller *notificationSubscriber, req *createSubscriptionRequest, cleared map[string]bool) bool {
	if !caller.isAgent() {
		return true
	}
	if req.ProjectID != caller.ProjectID {
		Forbidden(w)
		return false
	}
	if req.Scope != store.SubscriptionScopeAgent || req.AgentID == "" {
		return true
	}
	if cleared[req.AgentID] {
		return true
	}
	if !s.agentTargetInProject(w, r, caller, req.AgentID) {
		return false
	}
	if cleared != nil {
		cleared[req.AgentID] = true
	}
	return true
}

// agentTargetInProject reports whether agentID names an agent in the caller's
// project. An agent that does not exist draws the same 403 as one in another
// project, so the answer never confirms or denies the existence of an agent the
// caller has no business naming.
//
// Returns false when a response has already been written.
func (s *Server) agentTargetInProject(w http.ResponseWriter, r *http.Request, caller *notificationSubscriber, agentID string) bool {
	target, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			Forbidden(w)
			return false
		}
		writeErrorFromErr(w, err, "")
		return false
	}
	if target.ProjectID != caller.ProjectID {
		Forbidden(w)
		return false
	}
	return true
}

// resolveNotificationCaller resolves the caller and writes the failure response
// itself. Returns nil when the request should not proceed.
func (s *Server) resolveNotificationCaller(w http.ResponseWriter, r *http.Request) *notificationSubscriber {
	caller, err := s.notificationCaller(r.Context())
	if err != nil {
		slog.Error("Failed to resolve notification caller identity", "error", err)
		writeErrorFromErr(w, err, "")
		return nil
	}
	if caller == nil {
		Forbidden(w)
		return nil
	}
	return caller
}

// checkAgentNotifyScope verifies that agent callers may modify notification
// state. Returns true if the request should proceed, false if a 403 was
// written. User callers are not affected.
func checkAgentNotifyScope(w http.ResponseWriter, r *http.Request) bool {
	if agentIdent := GetAgentIdentityFromContext(r.Context()); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentNotify) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Missing required scope: project:agent:notify", nil)
			return false
		}
	}
	return true
}

// handleNotifications handles GET /api/v1/notifications.
// Lists notifications for the authenticated caller (user or agent).
//
// Response shape depends on the caller and the agentId filter:
//   - user, no agentId: flat []Notification array (tray behavior).
//   - user with ?agentId=X: { userNotifications: [...], agentNotifications: [...] }.
//   - agent (with or without ?agentId=X): flat []Notification array of the
//     notifications addressed to the calling agent, optionally narrowed to
//     those about agent X. An agent never sees another subscriber's rows, so
//     the combined shape has nothing to carry.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	caller := s.resolveNotificationCaller(w, r)
	if caller == nil {
		return
	}
	if !checkAgentReadScope(w, r) {
		return
	}

	acknowledged := r.URL.Query().Get("acknowledged")
	onlyUnacknowledged := acknowledged != "true"

	agentID := r.URL.Query().Get("agentId")

	if caller.isAgent() || agentID == "" {
		var notifs []store.Notification
		var err error
		if agentID == "" {
			notifs, err = s.store.GetNotifications(r.Context(), caller.Type, caller.ID, onlyUnacknowledged)
		} else {
			// An agent may only name an agent in its own project. The rows
			// returned are scoped either way, but the query itself should not
			// reach across the project boundary.
			if caller.isAgent() && !s.agentTargetInProject(w, r, caller, agentID) {
				return
			}
			notifs, err = s.store.GetNotificationsByAgent(r.Context(), agentID, caller.Type, caller.ID, onlyUnacknowledged)
		}
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, caller.scopeNotifications(notifs))
		return
	}

	// User caller, agent-scoped: return combined response
	userNotifs, err := s.store.GetNotificationsByAgent(r.Context(), agentID, caller.Type, caller.ID, onlyUnacknowledged)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	agentNotifs, err := s.store.GetNotifications(r.Context(), "agent", agentID, false)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, agentNotificationsResponse{
		UserNotifications:  userNotifs,
		AgentNotifications: agentNotifs,
	})
}

// agentNotificationsResponse is returned when ?agentId= is provided.
type agentNotificationsResponse struct {
	UserNotifications  []store.Notification `json:"userNotifications"`
	AgentNotifications []store.Notification `json:"agentNotifications"`
}

// handleNotificationRoutes handles requests under /api/v1/notifications/.
// Routes:
//   - POST /api/v1/notifications/ack-all: Acknowledge all notifications
//   - POST /api/v1/notifications/{id}/ack: Acknowledge a single notification
//   - POST /api/v1/notifications/subscriptions: Create a subscription
//   - GET  /api/v1/notifications/subscriptions: List subscriptions for caller
//   - PATCH /api/v1/notifications/subscriptions/{id}: Update trigger activities
//   - DELETE /api/v1/notifications/subscriptions/{id}: Delete a subscription
//   - POST /api/v1/notifications/subscriptions/bulk: Bulk create subscriptions
//   - POST /api/v1/notifications/subscriptions/bulk-delete: Bulk delete subscriptions
func (s *Server) handleNotificationRoutes(w http.ResponseWriter, r *http.Request) {
	caller := s.resolveNotificationCaller(w, r)
	if caller == nil {
		return
	}
	// Reads need the standard project read scope; everything else here mutates
	// notification state.
	if r.Method == http.MethodGet {
		if !checkAgentReadScope(w, r) {
			return
		}
	} else if !checkAgentNotifyScope(w, r) {
		return
	}

	id, action := extractAction(r, "/api/v1/notifications")

	// POST /api/v1/notifications/ack-all
	if id == "ack-all" && r.Method == http.MethodPost {
		// AcknowledgeAllNotifications keys on the subscriber ID alone, which
		// for an agent would span every project that reuses its slug. There is
		// no project-scoped variant, so agents ack individually instead.
		if caller.isAgent() {
			writeError(w, http.StatusNotImplemented, "not_implemented",
				"agent tokens must acknowledge notifications individually", nil)
			return
		}
		if err := s.store.AcknowledgeAllNotifications(r.Context(), caller.Type, caller.ID); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		slog.Info("All notifications acknowledged", "subscriberType", caller.Type, "subscriberID", caller.ID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Subscription routes: /api/v1/notifications/subscriptions[/...]
	if id == "subscriptions" {
		s.handleSubscriptionRoutes(w, r, caller, action)
		return
	}

	// Subscription template routes: /api/v1/notifications/templates[/...]
	if id == "templates" {
		s.handleSubscriptionTemplateRoutes(w, r, caller, action)
		return
	}

	// POST /api/v1/notifications/{id}/ack
	if id != "" && action == "ack" && r.Method == http.MethodPost {
		notif, err := s.store.GetNotification(r.Context(), id)
		if err != nil {
			writeErrorFromErr(w, err, "Notification")
			return
		}
		if !caller.ownsNotification(notif) {
			Forbidden(w)
			return
		}
		if err := s.store.AcknowledgeNotification(r.Context(), id); err != nil {
			writeErrorFromErr(w, err, "Notification")
			return
		}
		slog.Info("Notification acknowledged", "notificationID", id, "subscriberType", caller.Type, "subscriberID", caller.ID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	if id == "" {
		NotFound(w, "Notification")
		return
	}

	MethodNotAllowed(w)
}

// createSubscriptionRequest is the request body for POST /api/v1/notifications/subscriptions.
type createSubscriptionRequest struct {
	Scope             string   `json:"scope"`
	AgentID           string   `json:"agentId,omitempty"`
	ProjectID         string   `json:"projectId"`
	TriggerActivities []string `json:"triggerActivities"`
}

// updateSubscriptionRequest is the request body for PATCH /api/v1/notifications/subscriptions/{id}.
type updateSubscriptionRequest struct {
	TriggerActivities []string `json:"triggerActivities"`
}

// handleSubscriptionRoutes handles CRUD for notification subscriptions.
func (s *Server) handleSubscriptionRoutes(w http.ResponseWriter, r *http.Request, caller *notificationSubscriber, subID string) {
	ctx := r.Context()

	switch {
	// POST /api/v1/notifications/subscriptions — Create
	case subID == "" && r.Method == http.MethodPost:
		var req createSubscriptionRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body", nil)
			return
		}

		// Validate scope
		if req.Scope != store.SubscriptionScopeAgent && req.Scope != store.SubscriptionScopeProject {
			writeError(w, http.StatusBadRequest, "bad_request", "scope must be 'agent' or 'project'", nil)
			return
		}
		if req.Scope == store.SubscriptionScopeAgent && req.AgentID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "agentId is required when scope is 'agent'", nil)
			return
		}
		if req.Scope == store.SubscriptionScopeProject && req.AgentID != "" {
			writeError(w, http.StatusBadRequest, "bad_request", "agentId must be empty when scope is 'project'", nil)
			return
		}
		if req.ProjectID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "projectId is required", nil)
			return
		}
		if len(req.TriggerActivities) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "triggerActivities must be non-empty", nil)
			return
		}
		// Agent callers may only subscribe within their own project, and may
		// only watch agents in it.
		if !s.agentMaySubscribe(w, r, caller, &req, nil) {
			return
		}

		// Enforce subscription limit if configured
		if s.config.MaxSubscriptionsPerUser > 0 {
			existing, err := s.store.GetSubscriptionsForSubscriber(ctx, caller.Type, caller.ID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if len(caller.scopeSubscriptions(existing)) >= s.config.MaxSubscriptionsPerUser {
				writeError(w, http.StatusConflict, "limit_exceeded",
					fmt.Sprintf("Maximum subscription limit reached (%d)", s.config.MaxSubscriptionsPerUser), nil)
				return
			}
		}

		sub := &store.NotificationSubscription{
			ID:                api.NewUUID(),
			Scope:             req.Scope,
			AgentID:           req.AgentID,
			SubscriberType:    caller.Type,
			SubscriberID:      caller.ID,
			ProjectID:         req.ProjectID,
			TriggerActivities: req.TriggerActivities,
			CreatedBy:         caller.ID,
		}

		if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
			if err == store.ErrAlreadyExists {
				// Idempotent: return existing subscription
				existing, listErr := s.store.GetSubscriptionsForSubscriber(ctx, caller.Type, caller.ID)
				if listErr == nil {
					for _, e := range caller.scopeSubscriptions(existing) {
						if e.Scope == req.Scope && e.AgentID == req.AgentID && e.ProjectID == req.ProjectID {
							writeJSON(w, http.StatusOK, e)
							return
						}
					}
				}
				writeJSON(w, http.StatusOK, sub)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		slog.Info("Subscription created",
			"subscriptionID", sub.ID, "scope", sub.Scope, "subscriberType", caller.Type, "subscriberID", caller.ID)
		writeJSON(w, http.StatusCreated, sub)

	// GET /api/v1/notifications/subscriptions — List
	case subID == "" && r.Method == http.MethodGet:
		subs, err := s.store.GetSubscriptionsForSubscriber(ctx, caller.Type, caller.ID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		subs = caller.scopeSubscriptions(subs)

		// Apply optional filters
		projectID := r.URL.Query().Get("projectId")
		if projectID == "" {
			projectID = r.URL.Query().Get("groveId")
		}
		agentID := r.URL.Query().Get("agentId")
		scope := r.URL.Query().Get("scope")

		filtered := make([]store.NotificationSubscription, 0)
		for _, sub := range subs {
			if projectID != "" && sub.ProjectID != projectID {
				continue
			}
			if agentID != "" && sub.AgentID != agentID {
				continue
			}
			if scope != "" && sub.Scope != scope {
				continue
			}
			filtered = append(filtered, sub)
		}

		// Enrich agent-scoped subscriptions with agent slug for display
		for i := range filtered {
			if filtered[i].Scope == store.SubscriptionScopeAgent && filtered[i].AgentID != "" {
				agent, err := s.store.GetAgent(ctx, filtered[i].AgentID)
				if err == nil {
					filtered[i].AgentSlug = agent.Slug
				}
			}
		}

		writeJSON(w, http.StatusOK, filtered)

	// PATCH /api/v1/notifications/subscriptions/{id} — Update trigger activities
	case subID != "" && r.Method == http.MethodPatch:
		// Verify ownership
		sub, err := s.store.GetNotificationSubscription(ctx, subID)
		if err != nil {
			writeErrorFromErr(w, err, "Subscription")
			return
		}
		if !caller.ownsSubscription(sub) {
			Forbidden(w)
			return
		}

		var req updateSubscriptionRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body", nil)
			return
		}
		if len(req.TriggerActivities) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "triggerActivities must be non-empty", nil)
			return
		}

		if err := s.store.UpdateNotificationSubscriptionTriggers(ctx, subID, req.TriggerActivities); err != nil {
			writeErrorFromErr(w, err, "Subscription")
			return
		}

		// Return the updated subscription
		sub.TriggerActivities = req.TriggerActivities
		slog.Info("Subscription updated",
			"subscriptionID", subID, "subscriberType", caller.Type, "subscriberID", caller.ID)
		writeJSON(w, http.StatusOK, sub)

	// DELETE /api/v1/notifications/subscriptions/{id} — Delete
	case subID != "" && r.Method == http.MethodDelete:
		// Verify ownership before deleting
		sub, err := s.store.GetNotificationSubscription(ctx, subID)
		if err != nil {
			writeErrorFromErr(w, err, "Subscription")
			return
		}
		if !caller.ownsSubscription(sub) {
			Forbidden(w)
			return
		}

		if err := s.store.DeleteNotificationSubscription(ctx, subID); err != nil {
			writeErrorFromErr(w, err, "Subscription")
			return
		}

		slog.Info("Subscription deleted",
			"subscriptionID", subID, "subscriberType", caller.Type, "subscriberID", caller.ID)
		w.WriteHeader(http.StatusNoContent)

	// POST /api/v1/notifications/subscriptions/bulk — Bulk create
	case subID == "bulk" && r.Method == http.MethodPost:
		var reqs []createSubscriptionRequest
		if err := readJSON(r, &reqs); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Expected JSON array of subscription requests", nil)
			return
		}
		if len(reqs) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "Empty request array", nil)
			return
		}
		// Enforce subscription limit if configured. This runs before the
		// authorization pass below so that an oversized batch is rejected on
		// the count alone, rather than after one agent lookup per entry.
		if s.config.MaxSubscriptionsPerUser > 0 {
			existing, err := s.store.GetSubscriptionsForSubscriber(ctx, caller.Type, caller.ID)
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if len(caller.scopeSubscriptions(existing))+len(reqs) > s.config.MaxSubscriptionsPerUser {
				writeError(w, http.StatusConflict, "limit_exceeded",
					fmt.Sprintf("Bulk create would exceed subscription limit (%d)", s.config.MaxSubscriptionsPerUser), nil)
				return
			}
		}

		// Agent callers may only subscribe within their own project, and may
		// only watch agents in it. An authorization denial fails the whole
		// request rather than silently dropping entries the way malformed
		// items are dropped below.
		cleared := make(map[string]bool)
		for i := range reqs {
			if !s.agentMaySubscribe(w, r, caller, &reqs[i], cleared) {
				return
			}
		}

		var results []store.NotificationSubscription
		for _, req := range reqs {
			if req.Scope != store.SubscriptionScopeAgent && req.Scope != store.SubscriptionScopeProject {
				continue
			}
			if req.ProjectID == "" || len(req.TriggerActivities) == 0 {
				continue
			}
			if req.Scope == store.SubscriptionScopeAgent && req.AgentID == "" {
				continue
			}
			// A project-scoped entry carrying an agentId is malformed the same
			// way single create treats it; skipping keeps the stored row
			// unambiguous rather than filing a project subscription that also
			// names an agent.
			if req.Scope == store.SubscriptionScopeProject && req.AgentID != "" {
				continue
			}
			sub := &store.NotificationSubscription{
				ID:                api.NewUUID(),
				Scope:             req.Scope,
				AgentID:           req.AgentID,
				SubscriberType:    caller.Type,
				SubscriberID:      caller.ID,
				ProjectID:         req.ProjectID,
				TriggerActivities: req.TriggerActivities,
				CreatedBy:         caller.ID,
			}

			if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
				if err == store.ErrAlreadyExists {
					// Idempotent: find and return existing
					existing, listErr := s.store.GetSubscriptionsForSubscriber(ctx, caller.Type, caller.ID)
					if listErr == nil {
						for _, e := range caller.scopeSubscriptions(existing) {
							if e.Scope == req.Scope && e.AgentID == req.AgentID && e.ProjectID == req.ProjectID {
								results = append(results, e)
								break
							}
						}
					}
					continue
				}
				// Skip failed items, continue with the rest
				slog.Warn("Bulk subscription creation failed for item", "error", err)
				continue
			}
			results = append(results, *sub)
		}

		slog.Info("Bulk subscriptions created",
			"count", len(results), "subscriberType", caller.Type, "subscriberID", caller.ID)
		writeJSON(w, http.StatusCreated, results)

	// POST /api/v1/notifications/subscriptions/bulk-delete — Bulk delete
	case subID == "bulk-delete" && r.Method == http.MethodPost:
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Expected JSON with 'ids' array", nil)
			return
		}
		if len(req.IDs) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "ids must be non-empty", nil)
			return
		}

		deleted := 0
		for _, id := range req.IDs {
			sub, err := s.store.GetNotificationSubscription(ctx, id)
			if err != nil {
				continue
			}
			if !caller.ownsSubscription(sub) {
				continue
			}
			if err := s.store.DeleteNotificationSubscription(ctx, id); err != nil {
				continue
			}
			deleted++
		}

		slog.Info("Bulk subscriptions deleted",
			"deleted", deleted, "requested", len(req.IDs), "subscriberType", caller.Type, "subscriberID", caller.ID)
		writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})

	default:
		MethodNotAllowed(w)
	}
}

// createTemplateRequest is the request body for POST /api/v1/notifications/templates.
type createTemplateRequest struct {
	Name              string   `json:"name"`
	Scope             string   `json:"scope"`
	TriggerActivities []string `json:"triggerActivities"`
	ProjectID         string   `json:"projectId"`
}

// UnmarshalJSON implements backward compatibility for the grove-to-project rename.
func (r *createTemplateRequest) UnmarshalJSON(data []byte) error {
	type Alias createTemplateRequest
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{Alias: (*Alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
}

// handleSubscriptionTemplateRoutes handles CRUD for subscription templates.
//
// Templates are a user-facing convenience with no project containment of their
// own (CreatedBy is a bare ID, and list/delete are not project-scoped), and
// agents have no use for them. Agent callers are refused outright rather than
// admitted to a surface that cannot express their containment rules.
func (s *Server) handleSubscriptionTemplateRoutes(w http.ResponseWriter, r *http.Request, caller *notificationSubscriber, templateID string) {
	ctx := r.Context()

	if caller.isAgent() {
		Forbidden(w)
		return
	}

	switch {
	// POST /api/v1/notifications/templates — Create
	case templateID == "" && r.Method == http.MethodPost:
		var req createTemplateRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body", nil)
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "name is required", nil)
			return
		}
		if len(req.TriggerActivities) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "triggerActivities must be non-empty", nil)
			return
		}
		if req.Scope == "" {
			req.Scope = store.SubscriptionScopeProject
		}

		tmpl := &store.SubscriptionTemplate{
			ID:                api.NewUUID(),
			Name:              req.Name,
			Scope:             req.Scope,
			TriggerActivities: req.TriggerActivities,
			ProjectID:         req.ProjectID,
			CreatedBy:         caller.ID,
		}

		if err := s.store.CreateSubscriptionTemplate(ctx, tmpl); err != nil {
			if err == store.ErrAlreadyExists {
				writeError(w, http.StatusConflict, "already_exists", "A template with that name already exists in this project", nil)
				return
			}
			writeErrorFromErr(w, err, "")
			return
		}

		slog.Info("Subscription template created",
			"templateID", tmpl.ID, "name", tmpl.Name, "createdBy", caller.ID)
		writeJSON(w, http.StatusCreated, tmpl)

	// GET /api/v1/notifications/templates — List
	case templateID == "" && r.Method == http.MethodGet:
		projectID := r.URL.Query().Get("projectId")
		if projectID == "" {
			projectID = r.URL.Query().Get("groveId")
		}
		templates, err := s.store.ListSubscriptionTemplates(ctx, projectID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, templates)

	// DELETE /api/v1/notifications/templates/{id} — Delete
	case templateID != "" && r.Method == http.MethodDelete:
		tmpl, err := s.store.GetSubscriptionTemplate(ctx, templateID)
		if err != nil {
			writeErrorFromErr(w, err, "Template")
			return
		}
		if tmpl.CreatedBy != caller.ID {
			Forbidden(w)
			return
		}
		if err := s.store.DeleteSubscriptionTemplate(ctx, templateID); err != nil {
			writeErrorFromErr(w, err, "Template")
			return
		}
		slog.Info("Subscription template deleted",
			"templateID", templateID, "createdBy", caller.ID)
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}
