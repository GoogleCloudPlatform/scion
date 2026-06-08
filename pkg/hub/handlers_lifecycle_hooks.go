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
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecyclehooks"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// createLifecycleHookRequest is the payload for POST /api/v1/admin/lifecycle-hooks.
type createLifecycleHookRequest struct {
	Name              string                       `json:"name"`
	ScopeType         string                       `json:"scopeType"`
	ScopeID           string                       `json:"scopeId,omitempty"`
	Selector          *store.LifecycleHookSelector `json:"selector,omitempty"`
	Trigger           string                       `json:"trigger"`
	Action            *store.LifecycleHookAction   `json:"action,omitempty"`
	ExecutionIdentity string                       `json:"executionIdentity,omitempty"`
	Enabled           bool                         `json:"enabled"`
}

// updateLifecycleHookRequest is the payload for PUT /api/v1/admin/lifecycle-hooks/{id}.
type updateLifecycleHookRequest struct {
	Name              string                       `json:"name"`
	Selector          *store.LifecycleHookSelector `json:"selector,omitempty"`
	Trigger           string                       `json:"trigger"`
	Action            *store.LifecycleHookAction   `json:"action,omitempty"`
	ExecutionIdentity string                       `json:"executionIdentity,omitempty"`
	Enabled           bool                         `json:"enabled"`
	StateVersion      int64                        `json:"stateVersion"`
}

// listLifecycleHooksResponse wraps the list result for the API.
type listLifecycleHooksResponse struct {
	Items      []store.LifecycleHook `json:"items"`
	TotalCount int                   `json:"totalCount"`
}

// ---------------------------------------------------------------------------
// GCPServiceAccountResolver adapter
// ---------------------------------------------------------------------------

// storeGCPServiceAccountResolver adapts the store's GetGCPServiceAccount to the
// lifecyclehooks.GCPServiceAccountResolver interface.
type storeGCPServiceAccountResolver struct {
	store store.Store
}

func (r *storeGCPServiceAccountResolver) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	return r.store.GetGCPServiceAccount(ctx, id)
}

// ---------------------------------------------------------------------------
// Route handler: collection
// ---------------------------------------------------------------------------

// handleAdminLifecycleHooks handles GET (list) and POST (create) on
// /api/v1/admin/lifecycle-hooks.
func (s *Server) handleAdminLifecycleHooks(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.listLifecycleHooks(w, r)
	case http.MethodPost:
		s.createLifecycleHook(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminLifecycleHookByID handles GET / PUT / DELETE on
// /api/v1/admin/lifecycle-hooks/{id}.
func (s *Server) handleAdminLifecycleHookByID(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	id := extractID(r, "/api/v1/admin/lifecycle-hooks")
	if id == "" {
		BadRequest(w, "lifecycle hook ID is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getLifecycleHook(w, r, id)
	case http.MethodPut:
		s.updateLifecycleHook(w, r, id, user)
	case http.MethodDelete:
		s.deleteLifecycleHook(w, r, id, user)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// CRUD operations
// ---------------------------------------------------------------------------

func (s *Server) createLifecycleHook(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req createLifecycleHookRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	hook := &store.LifecycleHook{
		ID:                uuid.New().String(),
		Name:              req.Name,
		ScopeType:         req.ScopeType,
		ScopeID:           req.ScopeID,
		Selector:          req.Selector,
		Trigger:           req.Trigger,
		Action:            req.Action,
		ExecutionIdentity: req.ExecutionIdentity,
		Enabled:           req.Enabled,
		CreatedBy:         user.Email(),
	}

	// Default scope to hub for v1.
	if hook.ScopeType == "" {
		hook.ScopeType = store.LifecycleHookScopeHub
	}

	// Validate using the M2 validation library.
	resolver := &storeGCPServiceAccountResolver{store: s.store}
	if err := lifecyclehooks.ValidateHook(r.Context(), hook, resolver); err != nil {
		if ve, ok := err.(*lifecyclehooks.ValidationError); ok {
			writeLifecycleHookValidationError(w, ve)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	now := time.Now()
	hook.Created = now
	hook.Updated = now

	if err := s.store.CreateLifecycleHook(r.Context(), hook); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, ErrCodeConflict,
				"a lifecycle hook with this ID already exists", nil)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Audit: record creation.
	LogLifecycleHookEvent(r.Context(), s.auditLogger, LifecycleHookEventCreate,
		hook.ID, hook.Name, user.Email(), true, "")

	slog.Info("lifecycle hook created",
		"hook_id", hook.ID, "name", hook.Name,
		"trigger", hook.Trigger, "actor", user.Email())

	writeJSON(w, http.StatusCreated, hook)
}

func (s *Server) getLifecycleHook(w http.ResponseWriter, r *http.Request, id string) {
	hook, err := s.store.GetLifecycleHook(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Lifecycle Hook")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, hook)
}

func (s *Server) listLifecycleHooks(w http.ResponseWriter, r *http.Request) {
	filter := store.LifecycleHookFilter{
		ScopeType: r.URL.Query().Get("scopeType"),
		Trigger:   r.URL.Query().Get("trigger"),
	}

	enabledParam := r.URL.Query().Get("enabled")
	if enabledParam != "" {
		enabled := enabledParam == "true"
		filter.Enabled = &enabled
	}

	result, err := s.store.ListLifecycleHooks(r.Context(), filter, store.ListOptions{})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	items := result.Items
	if items == nil {
		items = []store.LifecycleHook{}
	}

	writeJSON(w, http.StatusOK, listLifecycleHooksResponse{
		Items:      items,
		TotalCount: result.TotalCount,
	})
}

func (s *Server) updateLifecycleHook(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	var req updateLifecycleHookRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Fetch existing hook.
	existing, err := s.store.GetLifecycleHook(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Lifecycle Hook")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Optimistic lock check: client must send the current state version.
	if req.StateVersion != existing.StateVersion {
		writeError(w, http.StatusConflict, ErrCodeVersionConflict,
			"version conflict — the hook was modified since you last read it", map[string]interface{}{
				"expected": req.StateVersion,
				"actual":   existing.StateVersion,
			})
		return
	}

	// Detect enable/disable change for auditing.
	enableChanged := existing.Enabled != req.Enabled

	// Apply mutable fields. Scope type/ID are immutable after creation.
	existing.Name = req.Name
	existing.Selector = req.Selector
	existing.Trigger = req.Trigger
	existing.Action = req.Action
	existing.ExecutionIdentity = req.ExecutionIdentity
	existing.Enabled = req.Enabled

	// Validate the updated hook.
	resolver := &storeGCPServiceAccountResolver{store: s.store}
	if err := lifecyclehooks.ValidateHook(r.Context(), existing, resolver); err != nil {
		if ve, ok := err.(*lifecyclehooks.ValidationError); ok {
			writeLifecycleHookValidationError(w, ve)
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	existing.Updated = time.Now()

	if err := s.store.UpdateLifecycleHook(r.Context(), existing); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			writeError(w, http.StatusConflict, ErrCodeVersionConflict,
				"version conflict — the hook was modified concurrently", nil)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Lifecycle Hook")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Audit: record update.
	LogLifecycleHookEvent(r.Context(), s.auditLogger, LifecycleHookEventUpdate,
		existing.ID, existing.Name, user.Email(), true, "")

	// Audit: record enable/disable if it changed.
	if enableChanged {
		eventType := LifecycleHookEventEnable
		if !existing.Enabled {
			eventType = LifecycleHookEventDisable
		}
		LogLifecycleHookEvent(r.Context(), s.auditLogger, eventType,
			existing.ID, existing.Name, user.Email(), true, "")
	}

	slog.Info("lifecycle hook updated",
		"hook_id", existing.ID, "name", existing.Name,
		"trigger", existing.Trigger, "actor", user.Email())

	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteLifecycleHook(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	// Fetch first so we can include the name in audit.
	hook, err := s.store.GetLifecycleHook(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Lifecycle Hook")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	if err := s.store.DeleteLifecycleHook(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Lifecycle Hook")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Audit: record deletion.
	LogLifecycleHookEvent(r.Context(), s.auditLogger, LifecycleHookEventDelete,
		hook.ID, hook.Name, user.Email(), true, "")

	slog.Info("lifecycle hook deleted",
		"hook_id", hook.ID, "name", hook.Name, "actor", user.Email())

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Validation error formatting
// ---------------------------------------------------------------------------

// writeLifecycleHookValidationError writes a 400 response with structured
// field-level validation details, matching the convention in errors.go.
func writeLifecycleHookValidationError(w http.ResponseWriter, ve *lifecyclehooks.ValidationError) {
	fieldErrors := make([]map[string]string, len(ve.Errors))
	for i, fe := range ve.Errors {
		fieldErrors[i] = map[string]string{
			"field":   fe.Field,
			"message": fe.Message,
		}
	}
	writeError(w, http.StatusBadRequest, ErrCodeValidationError,
		ve.Error(), map[string]interface{}{
			"fields": fieldErrors,
		})
}
