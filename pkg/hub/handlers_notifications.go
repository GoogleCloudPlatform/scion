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
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleNotifications handles GET /api/v1/notifications.
// Lists notifications for the authenticated user.
//
// Without agentId: returns flat []Notification array (existing tray behavior).
// With ?agentId=X: returns { userNotifications: [...], agentNotifications: [...] }.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	acknowledged := r.URL.Query().Get("acknowledged")
	onlyUnacknowledged := acknowledged != "true"

	agentID := r.URL.Query().Get("agentId")

	if agentID == "" {
		// Existing behaviour: flat array of user notifications
		notifs, err := s.store.GetNotifications(r.Context(), "user", user.ID(), onlyUnacknowledged)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, notifs)
		return
	}

	// Agent-scoped: return combined response
	userNotifs, err := s.store.GetNotificationsByAgent(r.Context(), agentID, "user", user.ID(), onlyUnacknowledged)
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
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	id, action := extractAction(r, "/api/v1/notifications")

	// POST /api/v1/notifications/ack-all
	if id == "ack-all" && r.Method == http.MethodPost {
		if err := s.store.AcknowledgeAllNotifications(r.Context(), "user", user.ID()); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		slog.Info("All notifications acknowledged", "userID", user.ID())
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Subscription routes: /api/v1/notifications/subscriptions[/...]
	if id == "subscriptions" {
		s.handleSubscriptionRoutes(w, r, user, action)
		return
	}

	// Subscription template routes: /api/v1/notifications/templates[/...]
	if id == "templates" {
		s.handleSubscriptionTemplateRoutes(w, r, user, action)
		return
	}

	// POST /api/v1/notifications/{id}/ack
	if id != "" && action == "ack" && r.Method == http.MethodPost {
		if err := s.store.AcknowledgeNotification(r.Context(), id); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		slog.Info("Notification acknowledged", "notificationID", id, "userID", user.ID())
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
	ProjectID           string   `json:"projectId"`
	TriggerActivities []string `json:"triggerActivities"`
}

// updateSubscriptionRequest is the request body for PATCH /api/v1/notifications/subscriptions/{id}.
type updateSubscriptionRequest struct {
	TriggerActivities []string `json:"triggerActivities"`
}

// handleSubscriptionRoutes handles CRUD for notification subscriptions.
func (s *Server) handleSubscriptionRoutes(w http.ResponseWriter, r *http.Request, user UserIdentity, subID string) {
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

		// Enforce subscription limit if configured
		if s.config.MaxSubscriptionsPerUser > 0 {
			existing, err := s.store.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, user.ID())
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if len(existing) >= s.config.MaxSubscriptionsPerUser {
				writeError(w, http.StatusConflict, "limit_exceeded",
					fmt.Sprintf("Maximum subscription limit reached (%d)", s.config.MaxSubscriptionsPerUser), nil)
				return
			}
		}

		sub := &store.NotificationSubscription{
			ID:                api.NewUUID(),
			Scope:             req.Scope,
			AgentID:           req.AgentID,
			SubscriberType:    store.SubscriberTypeUser,
			SubscriberID:      user.ID(),
			ProjectID:           req.ProjectID,
			TriggerActivities: req.TriggerActivities,
			CreatedBy:         user.ID(),
		}

		if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
			if err == store.ErrAlreadyExists {
				// Idempotent: return existing subscription
				existing, listErr := s.store.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, user.ID())
				if listErr == nil {
					for _, e := range existing {
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
			"subscriptionID", sub.ID, "scope", sub.Scope, "userID", user.ID())
		writeJSON(w, http.StatusCreated, sub)

	// GET /api/v1/notifications/subscriptions — List
	case subID == "" && r.Method == http.MethodGet:
		subs, err := s.store.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, user.ID())
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		// Apply optional filters
		projectID := r.URL.Query().Get("projectId")
		if projectID == "" {
			projectID = r.URL.Query().Get("groveId")
		}
		agentID := r.URL.Query().Get("agentId")
		scope := r.URL.Query().Get("scope")
		if scope == "project" {
			scope = "project"
		}

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
		if sub.SubscriberType != store.SubscriberTypeUser || sub.SubscriberID != user.ID() {
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
			"subscriptionID", subID, "userID", user.ID())
		writeJSON(w, http.StatusOK, sub)

	// DELETE /api/v1/notifications/subscriptions/{id} — Delete
	case subID != "" && r.Method == http.MethodDelete:
		// Verify ownership before deleting
		sub, err := s.store.GetNotificationSubscription(ctx, subID)
		if err != nil {
			writeErrorFromErr(w, err, "Subscription")
			return
		}
		if sub.SubscriberType != store.SubscriberTypeUser || sub.SubscriberID != user.ID() {
			Forbidden(w)
			return
		}

		if err := s.store.DeleteNotificationSubscription(ctx, subID); err != nil {
			writeErrorFromErr(w, err, "Subscription")
			return
		}

		slog.Info("Subscription deleted",
			"subscriptionID", subID, "userID", user.ID())
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

		// Enforce subscription limit if configured
		if s.config.MaxSubscriptionsPerUser > 0 {
			existing, err := s.store.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, user.ID())
			if err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			if len(existing)+len(reqs) > s.config.MaxSubscriptionsPerUser {
				writeError(w, http.StatusConflict, "limit_exceeded",
					fmt.Sprintf("Bulk create would exceed subscription limit (%d)", s.config.MaxSubscriptionsPerUser), nil)
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

			sub := &store.NotificationSubscription{
				ID:                api.NewUUID(),
				Scope:             req.Scope,
				AgentID:           req.AgentID,
				SubscriberType:    store.SubscriberTypeUser,
				SubscriberID:      user.ID(),
				ProjectID:           req.ProjectID,
				TriggerActivities: req.TriggerActivities,
				CreatedBy:         user.ID(),
			}

			if err := s.store.CreateNotificationSubscription(ctx, sub); err != nil {
				if err == store.ErrAlreadyExists {
					// Idempotent: find and return existing
					existing, listErr := s.store.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, user.ID())
					if listErr == nil {
						for _, e := range existing {
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
			"count", len(results), "userID", user.ID())
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
			if sub.SubscriberType != store.SubscriberTypeUser || sub.SubscriberID != user.ID() {
				continue
			}
			if err := s.store.DeleteNotificationSubscription(ctx, id); err != nil {
				continue
			}
			deleted++
		}

		slog.Info("Bulk subscriptions deleted",
			"deleted", deleted, "requested", len(req.IDs), "userID", user.ID())
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
	ProjectID           string   `json:"projectId"`
}

// handleSubscriptionTemplateRoutes handles CRUD for subscription templates.
func (s *Server) handleSubscriptionTemplateRoutes(w http.ResponseWriter, r *http.Request, user UserIdentity, templateID string) {
	ctx := r.Context()

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
			ProjectID:           req.ProjectID,
			CreatedBy:         user.ID(),
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
			"templateID", tmpl.ID, "name", tmpl.Name, "userID", user.ID())
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
		if tmpl.CreatedBy != user.ID() {
			Forbidden(w)
			return
		}
		if err := s.store.DeleteSubscriptionTemplate(ctx, templateID); err != nil {
			writeErrorFromErr(w, err, "Template")
			return
		}
		slog.Info("Subscription template deleted",
			"templateID", templateID, "userID", user.ID())
		w.WriteHeader(http.StatusNoContent)

	default:
		MethodNotAllowed(w)
	}
}
