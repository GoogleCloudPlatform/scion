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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

// createLimitDefinitionRequest is the payload for POST /api/v1/admin/limits.
type createLimitDefinitionRequest struct {
	Name         string `json:"name"`
	ResourceType string `json:"resourceType"`
	Unit         string `json:"unit"`
	Description  string `json:"description"`
	DefaultValue int64  `json:"defaultValue"`
}

// updateLimitDefinitionRequest is the payload for PUT /api/v1/admin/limits/:id.
type updateLimitDefinitionRequest struct {
	Name         string `json:"name"`
	ResourceType string `json:"resourceType"`
	Unit         string `json:"unit"`
	Description  string `json:"description"`
	DefaultValue int64  `json:"defaultValue"`
}

// createEntitlementBindingRequest is the payload for POST /api/v1/admin/limits/:id/entitlements.
type createEntitlementBindingRequest struct {
	SubjectType string `json:"subjectType"`
	SubjectID   string `json:"subjectId"`
	ScopeType   string `json:"scopeType"`
	ScopeID     string `json:"scopeId"`
	Value       int64  `json:"value"`
}

// updateEntitlementBindingRequest is the payload for PUT /api/v1/admin/entitlements/:id.
type updateEntitlementBindingRequest struct {
	SubjectType string `json:"subjectType"`
	SubjectID   string `json:"subjectId"`
	ScopeType   string `json:"scopeType"`
	ScopeID     string `json:"scopeId"`
	Value       int64  `json:"value"`
}

// listLimitDefinitionsResponse wraps the list result for the API.
type listLimitDefinitionsResponse struct {
	Items      []*store.LimitDefinition `json:"items"`
	TotalCount int                      `json:"totalCount"`
}

// listEntitlementBindingsResponse wraps the list result for the API.
type listEntitlementBindingsResponse struct {
	Items      []*store.EntitlementBinding `json:"items"`
	TotalCount int                         `json:"totalCount"`
}

// usageSummaryEntry represents a single limit's usage summary.
type usageSummaryEntry struct {
	LimitDefinition *store.LimitDefinition `json:"limitDefinition"`
	ActiveCount     int                    `json:"activeCount"`
}

// usageSummaryResponse wraps the admin usage summary.
type usageSummaryResponse struct {
	Items []usageSummaryEntry `json:"items"`
}

// usageByLimitResponse wraps the admin usage-by-limit result.
type usageByLimitResponse struct {
	LimitDefinition *store.LimitDefinition    `json:"limitDefinition"`
	Reservations    []*store.UsageReservation `json:"reservations"`
	TotalActive     int                       `json:"totalActive"`
}

// myUsageEntry represents one limit's current/max for the current user.
type myUsageEntry struct {
	LimitDefinition *store.LimitDefinition `json:"limitDefinition"`
	Current         int64                  `json:"current"`
	Max             int64                  `json:"max"` // 0 = unlimited
}

// myUsageResponse wraps the /usage/me result.
type myUsageResponse struct {
	Items []myUsageEntry `json:"items"`
}

// ---------------------------------------------------------------------------
// Route handlers: Limit Definitions
// ---------------------------------------------------------------------------

// handleAdminLimits handles GET (list) and POST (create) on
// /api/v1/admin/limits.
// Authorization: route guard checks quota.read for GET.
// POST requires quota.create via inline Decide.
func (s *Server) handleAdminLimits(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listLimitDefinitions(w, r)
	case http.MethodPost:
		user, ok := s.requireWritePermissionForQuota(w, r, "quota.create", "create")
		if !ok {
			return
		}
		s.createLimitDefinition(w, r, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminLimitByID handles GET / PUT / DELETE on
// /api/v1/admin/limits/:id, and delegates to handleLimitEntitlements for
// /api/v1/admin/limits/:id/entitlements.
func (s *Server) handleAdminLimitByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/limits/")
	path = strings.TrimSuffix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	limitID := parts[0]
	if limitID == "" {
		BadRequest(w, "limit definition ID is required")
		return
	}
	if len(parts) > 1 && parts[1] == "entitlements" {
		s.handleLimitEntitlements(w, r, limitID)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getLimitDefinition(w, r, limitID)
	case http.MethodPut:
		user, ok := s.requireWritePermissionForQuota(w, r, "quota.update", "update")
		if !ok {
			return
		}
		s.updateLimitDefinition(w, r, limitID, user)
	case http.MethodDelete:
		user, ok := s.requireWritePermissionForQuota(w, r, "quota.delete", "delete")
		if !ok {
			return
		}
		s.deleteLimitDefinition(w, r, limitID, user)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// Route handlers: Entitlement Bindings
// ---------------------------------------------------------------------------

// handleLimitEntitlements handles GET (list) and POST (create) entitlement
// bindings nested under a limit definition: /api/v1/admin/limits/:id/entitlements.
func (s *Server) handleLimitEntitlements(w http.ResponseWriter, r *http.Request, limitID string) {
	switch r.Method {
	case http.MethodGet:
		s.listEntitlements(w, r, limitID)
	case http.MethodPost:
		user, ok := s.requireWritePermissionForQuota(w, r, "quota.create", "create")
		if !ok {
			return
		}
		s.createEntitlement(w, r, limitID, user)
	default:
		MethodNotAllowed(w)
	}
}

// handleAdminEntitlementByID handles GET / PUT / DELETE on
// /api/v1/admin/entitlements/:id.
func (s *Server) handleAdminEntitlementByID(w http.ResponseWriter, r *http.Request) {
	id := extractID(r, "/api/v1/admin/entitlements")
	if id == "" {
		BadRequest(w, "entitlement binding ID is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getEntitlement(w, r, id)
	case http.MethodPut:
		user, ok := s.requireWritePermissionForQuota(w, r, "quota.update", "update")
		if !ok {
			return
		}
		s.updateEntitlement(w, r, id, user)
	case http.MethodDelete:
		user, ok := s.requireWritePermissionForQuota(w, r, "quota.delete", "delete")
		if !ok {
			return
		}
		s.deleteEntitlement(w, r, id, user)
	default:
		MethodNotAllowed(w)
	}
}

// ---------------------------------------------------------------------------
// Route handlers: Usage Queries
// ---------------------------------------------------------------------------

// handleAdminUsage handles GET on /api/v1/admin/usage.
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	s.getUsageSummary(w, r)
}

// handleAdminUsageByLimit handles GET on /api/v1/admin/usage/:limitID.
func (s *Server) handleAdminUsageByLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	limitID := extractID(r, "/api/v1/admin/usage")
	if limitID == "" {
		BadRequest(w, "limit definition ID is required")
		return
	}
	s.getUsageByLimit(w, r, limitID)
}

// handleUsageMe handles GET on /api/v1/usage/me.
func (s *Server) handleUsageMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}
	s.getMyUsage(w, r)
}

// ---------------------------------------------------------------------------
// CRUD: Limit Definitions
// ---------------------------------------------------------------------------

func (s *Server) createLimitDefinition(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	var req createLimitDefinitionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	if req.DefaultValue < 0 {
		BadRequest(w, "default_value must be non-negative (0 means unlimited)")
		return
	}

	now := time.Now()
	def := &store.LimitDefinition{
		ID:           uuid.New().String(),
		Name:         req.Name,
		ResourceType: req.ResourceType,
		Unit:         req.Unit,
		Description:  req.Description,
		DefaultValue: req.DefaultValue,
		System:       false,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	created, err := s.store.CreateLimitDefinition(r.Context(), def)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, "a limit definition with this name already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("limit definition created",
		"limit_id", created.ID, "name", created.Name, "actor", user.Email())

	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getLimitDefinition(w http.ResponseWriter, r *http.Request, id string) {
	def, err := s.store.GetLimitDefinition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, def)
}

func (s *Server) listLimitDefinitions(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.ListLimitDefinitions(r.Context())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if defs == nil {
		defs = []*store.LimitDefinition{}
	}
	writeJSON(w, http.StatusOK, listLimitDefinitionsResponse{
		Items:      defs,
		TotalCount: len(defs),
	})
}

func (s *Server) updateLimitDefinition(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	var req updateLimitDefinitionRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		BadRequest(w, "name is required")
		return
	}

	if req.DefaultValue < 0 {
		BadRequest(w, "default_value must be non-negative (0 means unlimited)")
		return
	}

	existing, err := s.store.GetLimitDefinition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// System-seeded limit definitions cannot be modified.
	if existing.System {
		writeForbidden(w, "system-seeded limit definitions cannot be modified")
		return
	}

	existing.Name = req.Name
	existing.ResourceType = req.ResourceType
	existing.Unit = req.Unit
	existing.Description = req.Description
	existing.DefaultValue = req.DefaultValue
	existing.UpdatedAt = time.Now()

	updated, err := s.store.UpdateLimitDefinition(r.Context(), existing)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("limit definition updated",
		"limit_id", updated.ID, "name", updated.Name, "actor", user.Email())

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteLimitDefinition(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	// Fetch first to check system flag and include name in logs.
	def, err := s.store.GetLimitDefinition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// System-seeded limit definitions cannot be deleted.
	if def.System {
		writeForbidden(w, "system-seeded limit definitions cannot be deleted")
		return
	}

	if err := s.store.DeleteLimitDefinition(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("limit definition deleted",
		"limit_id", def.ID, "name", def.Name, "actor", user.Email())

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// CRUD: Entitlement Bindings
// ---------------------------------------------------------------------------

func (s *Server) createEntitlement(w http.ResponseWriter, r *http.Request, limitID string, user UserIdentity) {
	var req createEntitlementBindingRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.SubjectType == "" {
		BadRequest(w, "subject_type is required")
		return
	}
	if req.SubjectID == "" {
		BadRequest(w, "subject_id is required")
		return
	}

	if req.Value < 0 {
		BadRequest(w, "value must be non-negative (0 means unlimited)")
		return
	}

	// Validate the referenced limit definition exists.
	if _, err := s.store.GetLimitDefinition(r.Context(), limitID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	now := time.Now()
	binding := &store.EntitlementBinding{
		ID:                uuid.New().String(),
		LimitDefinitionID: limitID,
		SubjectType:       req.SubjectType,
		SubjectID:         req.SubjectID,
		ScopeType:         req.ScopeType,
		ScopeID:           req.ScopeID,
		Value:             req.Value,
		CreatedBy:         user.Email(),
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	created, err := s.store.CreateEntitlementBinding(r.Context(), binding)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, "an entitlement binding with these parameters already exists")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("entitlement binding created",
		"binding_id", created.ID, "limit_id", limitID, "actor", user.Email())

	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getEntitlement(w http.ResponseWriter, r *http.Request, id string) {
	binding, err := s.store.GetEntitlementBinding(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Entitlement Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, binding)
}

func (s *Server) listEntitlements(w http.ResponseWriter, r *http.Request, limitID string) {
	bindings, err := s.store.ListEntitlementBindings(r.Context(), limitID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if bindings == nil {
		bindings = []*store.EntitlementBinding{}
	}
	writeJSON(w, http.StatusOK, listEntitlementBindingsResponse{
		Items:      bindings,
		TotalCount: len(bindings),
	})
}

func (s *Server) updateEntitlement(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	var req updateEntitlementBindingRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.SubjectType == "" {
		BadRequest(w, "subject_type is required")
		return
	}
	if req.SubjectID == "" {
		BadRequest(w, "subject_id is required")
		return
	}

	if req.Value < 0 {
		BadRequest(w, "value must be non-negative (0 means unlimited)")
		return
	}

	existing, err := s.store.GetEntitlementBinding(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Entitlement Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	existing.SubjectType = req.SubjectType
	existing.SubjectID = req.SubjectID
	existing.ScopeType = req.ScopeType
	existing.ScopeID = req.ScopeID
	existing.Value = req.Value
	existing.UpdatedAt = time.Now()

	updated, err := s.store.UpdateEntitlementBinding(r.Context(), existing)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Entitlement Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("entitlement binding updated",
		"binding_id", updated.ID, "actor", user.Email())

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteEntitlement(w http.ResponseWriter, r *http.Request, id string, user UserIdentity) {
	if err := s.store.DeleteEntitlementBinding(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Entitlement Binding")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	slog.Info("entitlement binding deleted",
		"binding_id", id, "actor", user.Email())

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Usage Queries
// ---------------------------------------------------------------------------

func (s *Server) getUsageSummary(w http.ResponseWriter, r *http.Request) {
	defs, err := s.store.ListLimitDefinitions(r.Context())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	entries := make([]usageSummaryEntry, 0, len(defs))
	for _, def := range defs {
		reservations, err := s.store.ListActiveReservations(r.Context(), def.ID, store.QuotaScopeSystem, "")
		activeCount := 0
		if err != nil {
			slog.Error("failed to list active reservations for usage summary",
				"limit_id", def.ID, "error", err)
		} else {
			activeCount = len(reservations)
		}
		entries = append(entries, usageSummaryEntry{
			LimitDefinition: def,
			ActiveCount:     activeCount,
		})
	}

	writeJSON(w, http.StatusOK, usageSummaryResponse{Items: entries})
}

func (s *Server) getUsageByLimit(w http.ResponseWriter, r *http.Request, limitID string) {
	def, err := s.store.GetLimitDefinition(r.Context(), limitID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	reservations, err := s.store.ListActiveReservations(r.Context(), limitID, store.QuotaScopeSystem, "")
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if reservations == nil {
		reservations = []*store.UsageReservation{}
	}

	writeJSON(w, http.StatusOK, usageByLimitResponse{
		LimitDefinition: def,
		Reservations:    reservations,
		TotalActive:     len(reservations),
	})
}

func (s *Server) getMyUsage(w http.ResponseWriter, r *http.Request) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}

	if s.quotaService == nil {
		// When quota service is unavailable, return empty usage.
		writeJSON(w, http.StatusOK, myUsageResponse{Items: []myUsageEntry{}})
		return
	}

	userID := identity.ID()

	defs, err := s.store.ListLimitDefinitions(r.Context())
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	entries := make([]myUsageEntry, 0, len(defs))
	for _, def := range defs {
		// Resolve effective limit for this user at system scope.
		effectiveLimit, err := s.quotaService.ResolveEffectiveLimit(
			r.Context(), def.ID, userID, store.QuotaScopeSystem, "")
		if err != nil {
			slog.Warn("failed to resolve effective limit for user",
				"limit_id", def.ID, "user_id", userID, "error", err)
			effectiveLimit = def.DefaultValue
		}

		// Count active reservations for this user at system scope.
		current, err := s.store.CountActiveReservations(
			r.Context(), def.ID, userID, store.QuotaScopeSystem, "")
		if err != nil {
			slog.Warn("failed to count active reservations for user",
				"limit_id", def.ID, "user_id", userID, "error", err)
			current = 0
		}

		entries = append(entries, myUsageEntry{
			LimitDefinition: def,
			Current:         current,
			Max:             effectiveLimit,
		})
	}

	writeJSON(w, http.StatusOK, myUsageResponse{Items: entries})
}

// ---------------------------------------------------------------------------
// Authorization helper
// ---------------------------------------------------------------------------

// requireWritePermissionForQuota checks that the authenticated user has the
// specified quota permission. It uses the same pattern as requireWritePermission
// but with the "quota" resource type.
func (s *Server) requireWritePermissionForQuota(w http.ResponseWriter, r *http.Request, permission string, action string) (UserIdentity, bool) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}
	if s.authzService == nil {
		Forbidden(w)
		return nil, false
	}
	decision := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "quota", ID: "hub"},
		Action:     Action(action),
		Permission: permission,
	})
	if !decision.Allowed {
		Forbidden(w)
		return nil, false
	}
	return user, true
}
