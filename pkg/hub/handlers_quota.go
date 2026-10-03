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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	LimitDefinition *store.LimitDefinition `json:"limitDefinition"`
	Reservations    []usageReservationView `json:"reservations"`
	TotalActive     int                    `json:"totalActive"`
}

// usageReservationView extends store.UsageReservation with the effective
// per-broker agent limit and its precedence source (ptone/scion#2061 P2.2,
// design.md §5.9, AC-P2-10). Both are populated only for
// max_agents_per_broker reservations at broker scope — every other
// reservation (a different limit, or a system/project-scoped one) leaves
// them unset. They come from the shared brokerCapacity read model, the same
// one Reserve enforces and the providers listing / broker settings GET
// report, so the usage page never shows the raw entitlement-binding value
// or the hub-wide default in place of what create actually enforces.
type usageReservationView struct {
	*store.UsageReservation
	// BrokerAgentLimit is nil when the broker is unlimited, when limit
	// resolution failed, or when this row isn't a max_agents_per_broker
	// broker-scoped reservation.
	BrokerAgentLimit *int64 `json:"brokerAgentLimit,omitempty"`
	// BrokerAgentLimitSource is one of the BrokerLimitSource* constants
	// (broker_capacity.go): "broker" | "entitlement" | "hub_default" |
	// "unlimited" | "not_enforced". Empty only when this row isn't a
	// max_agents_per_broker broker-scoped reservation, or brokerCapacity's
	// own resolution failed outright (BrokerCapacity{}, both fields zero).
	// It is the precedence step that produced the result, not a statement
	// about whether the result is a cap: when the effective limit is <= 0
	// (unlimited), BrokerAgentLimit is absent but BrokerAgentLimitSource is
	// still whichever step produced it ("broker" for a settings.maxAgents=0
	// override, "entitlement"/"hub_default" for a 0 binding or default).
	// "unlimited" itself needs a nil limitDef or a nil quotaService in
	// effectiveBrokerLimit; neither can occur here: def is this request's
	// limit definition (never nil — see getUsageByLimit, the only builder of
	// usageReservationView), and s.quotaService is always constructed in
	// NewServer (server.go).
	//
	// "not_enforced" (design.md Amendment A1, ptone/scion#2270/P1b, not yet
	// wired as of this field) means the P1b enforcement switch is off.
	// BrokerAgentLimit keeps whatever the precedence steps resolved — a cap,
	// or absent when that resolves to unlimited, exactly as in every other
	// source above — but the value is informational only and is not
	// currently enforced by Reserve. The usage page must render this
	// visibly (not tooltip-only) next to the cap, since a limit shown
	// without that context would look enforced when it is not.
	BrokerAgentLimitSource string `json:"brokerAgentLimitSource,omitempty"`

	// EntryAgentLimit, EntryAgentLimitKey and EntryAgentLimitEnforced are
	// set only on max_agents_per_broker reservations held against a
	// profile or runtime entry's own max_agents (scope type
	// broker_profile, ptone/scion#2728). EntryAgentLimitKey names the
	// settings key, e.g. "profiles.gke.max_agents". EntryAgentLimit is the
	// current value, nil when that key is no longer set (the reconcile
	// pass then moves the reservation to the broker-wide total).
	// EntryAgentLimitEnforced is false when broker quota enforcement is
	// switched off; the limit is then informational only.
	EntryAgentLimit         *int64 `json:"entryAgentLimit,omitempty"`
	EntryAgentLimitKey      string `json:"entryAgentLimitKey,omitempty"`
	EntryAgentLimitEnforced *bool  `json:"entryAgentLimitEnforced,omitempty"`
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// ---------------------------------------------------------------------------
// Route handlers: Usage Queries
// ---------------------------------------------------------------------------

// handleAdminUsage handles GET on /api/v1/admin/usage.
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.getUsageSummary(w, r)
}

// handleAdminUsageByLimit handles GET on /api/v1/admin/usage/:limitID.
func (s *Server) handleAdminUsageByLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
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
		MethodNotAllowed(w, http.MethodGet)
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

	req.ResourceType = strings.TrimSpace(req.ResourceType)
	if req.ResourceType == "" {
		BadRequest(w, "resource type is required")
		return
	}

	// unit is trimmed but not required: it is optional (the admin UI treats
	// it as optional, rendering "—"), and unit is not read by quota
	// resolution. Matches the update path's trimming (updateLimitDefinition).
	req.Unit = strings.TrimSpace(req.Unit)

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

	// Trim before comparing/validating so a PUT that merely pads a field
	// with whitespace is normalised rather than treated as a change.
	req.ResourceType = strings.TrimSpace(req.ResourceType)
	// unit is trimmed but not required: createLimitDefinition accepts an
	// empty unit (the admin UI treats it as optional, rendering "—"),
	// and unit is not read by quota resolution, so requiring it here
	// would make existing empty-unit rows permanently uneditable.
	req.Unit = strings.TrimSpace(req.Unit)

	// System-seeded limit definitions: only default_value and description
	// may be changed (ptone/scion#2061 P1a, ptone/scion#2063). This is the
	// supported admin path for the hub-wide max_agents_per_broker value
	// (and any other system limit) — see pkg/hub/seed.go.
	if existing.System {
		if req.Name != existing.Name || req.ResourceType != existing.ResourceType || req.Unit != existing.Unit {
			writeForbidden(w, "system limit definitions: only default_value and description can be changed")
			return
		}
	} else {
		if req.ResourceType == "" {
			BadRequest(w, "resource type is required")
			return
		}
		existing.Name = req.Name
		existing.ResourceType = req.ResourceType
		existing.Unit = req.Unit
	}
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
	limitDef, err := s.store.GetLimitDefinition(r.Context(), limitID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// ptone/scion#2061 P2-D4 / ptone/scion#2063 item 2: per-broker
	// max_agents_per_broker caps are set via the broker settings API, not
	// via a broker-scoped entitlement binding. System-scoped bindings for
	// this limit are unaffected — they remain the hub-wide override.
	if limitDef.Name == store.LimitMaxAgentsPerBroker && req.ScopeType == store.QuotaScopeBroker {
		BadRequest(w, "per-broker agent caps are set via PUT /api/v1/runtime-brokers/{id}/settings")
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

	// ptone/scion#2061 P2-D4 / ptone/scion#2063 item 2: an update can
	// reshape an existing binding into the same broker-scoped
	// max_agents_per_broker shape that createEntitlement rejects. Block it
	// here too, so PUT cannot be used to bypass the POST check.
	limitDef, err := s.store.GetLimitDefinition(r.Context(), existing.LimitDefinitionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}
	if limitDef.Name == store.LimitMaxAgentsPerBroker && req.ScopeType == store.QuotaScopeBroker {
		BadRequest(w, "per-broker agent caps are set via PUT /api/v1/runtime-brokers/{id}/settings")
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
	ctx := r.Context()

	defs, err := s.store.ListLimitDefinitions(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	entries := make([]usageSummaryEntry, 0, len(defs))
	for _, def := range defs {
		// max_agents_per_broker reservations are stored at
		// store.QuotaScopeBroker scoped to each individual broker
		// (broker_quota.go), never at store.QuotaScopeSystem, so the
		// system-scope query below always returns none for it — the summary
		// row showed 0 active agents regardless of real usage. Sum across
		// every broker instead, the same way getUsageByLimit and
		// ReconcileStaleBrokerQuotaReservations do (ptone/scion#2061 P2.2).
		var reservations []*store.UsageReservation
		var listErr error
		if def.Name == store.LimitMaxAgentsPerBroker {
			reservations, listErr = s.listBrokerScopedActiveReservations(ctx, def.ID)
		} else {
			reservations, listErr = s.store.ListActiveReservations(ctx, def.ID, store.QuotaScopeSystem, "")
		}
		activeCount := 0
		if listErr != nil {
			slog.Error("failed to list active reservations for usage summary",
				"limit_id", def.ID, "error", listErr)
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
	ctx := r.Context()

	def, err := s.store.GetLimitDefinition(ctx, limitID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Limit Definition")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// max_agents_per_broker reservations are stored at store.QuotaScopeBroker
	// scoped to each individual broker (broker_quota.go), never at
	// store.QuotaScopeSystem — so the generic system-scope query below would
	// always return none for it. List across every broker instead, the same
	// way ReconcileStaleBrokerQuotaReservations does.
	var reservations []*store.UsageReservation
	if def.Name == store.LimitMaxAgentsPerBroker {
		reservations, err = s.listBrokerScopedActiveReservations(ctx, def.ID)
	} else {
		reservations, err = s.store.ListActiveReservations(ctx, limitID, store.QuotaScopeSystem, "")
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// limitDef is looked up once and reused for every broker-scoped
	// reservation below (design.md §5.9, AC-P2-10): def is already that one
	// lookup, so no second query is needed.
	brokerCapacityCache := make(map[string]BrokerCapacity)
	var limitSettings *config.VersionedSettings
	limitSettingsLoaded := false
	views := make([]usageReservationView, len(reservations))
	for i, res := range reservations {
		views[i] = usageReservationView{UsageReservation: res}
		if def.Name == store.LimitMaxAgentsPerBroker && res.ScopeType == store.QuotaScopeBrokerProfile {
			if !limitSettingsLoaded {
				limitSettings, _ = s.agentLimitSettings(ctx)
				limitSettingsLoaded = true
			}
			limit, key := entryAgentLimitForScope(limitSettings, res.SubjectID, res.ScopeID)
			views[i].EntryAgentLimitKey = key
			if limit > 0 {
				views[i].EntryAgentLimit = &limit
			}
			enforced := s.brokerQuotasEnforced()
			views[i].EntryAgentLimitEnforced = &enforced
			continue
		}
		if def.Name != store.LimitMaxAgentsPerBroker || res.ScopeType != store.QuotaScopeBroker {
			continue
		}
		bc, ok := brokerCapacityCache[res.ScopeID]
		if !ok {
			bc = s.brokerCapacity(ctx, res.ScopeID, def)
			brokerCapacityCache[res.ScopeID] = bc
		}
		views[i].BrokerAgentLimit = bc.Limit
		views[i].BrokerAgentLimitSource = bc.Source
	}

	writeJSON(w, http.StatusOK, usageByLimitResponse{
		LimitDefinition: def,
		Reservations:    views,
		TotalActive:     len(reservations),
	})
}

// listBrokerScopedActiveReservations aggregates active reservations for
// limitDefinitionID across every runtime broker (ptone/scion#2061 P2.2).
// max_agents_per_broker reservations are always scoped to a specific broker
// (store.QuotaScopeBroker, scope_id=broker ID), so listing them requires
// enumerating brokers first — store.Store.ListActiveReservations takes one
// exact scope, not a wildcard. This mirrors
// ReconcileStaleBrokerQuotaReservations's own broker loop (broker_quota.go),
// including its bounds: 1+B queries (one ListRuntimeBrokers, one
// ListActiveReservations per broker) and the same 10,000-broker cap, below
// which a hub with more brokers than that would silently undercount here
// exactly as reconcile already does. Reviewed and accepted for this PR
// (round 1, F3); a single-query store method (e.g.
// ListActiveReservationsByScopeType) usable by both call sites is a
// follow-up, not required here.
func (s *Server) listBrokerScopedActiveReservations(ctx context.Context, limitDefinitionID string) ([]*store.UsageReservation, error) {
	brokers, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		return nil, fmt.Errorf("list runtime brokers: %w", err)
	}

	var reservations []*store.UsageReservation
	for _, broker := range brokers.Items {
		brokerReservations, err := s.store.ListActiveReservations(ctx, limitDefinitionID, store.QuotaScopeBroker, broker.ID)
		if err != nil {
			return nil, fmt.Errorf("list active reservations for broker %q: %w", broker.ID, err)
		}
		reservations = append(reservations, brokerReservations...)
	}
	// Reservations held against a profile or runtime entry's own
	// max_agents (ptone/scion#2728).
	profileReservations, err := s.store.ListActiveReservationsByScopeType(ctx, limitDefinitionID, store.QuotaScopeBrokerProfile)
	if err != nil {
		return nil, fmt.Errorf("list per-profile reservations: %w", err)
	}
	reservations = append(reservations, profileReservations...)
	return reservations, nil
}

// entryAgentLimitForScope returns the current max_agents for a
// broker_profile scope ID ("BROKER/profiles/NAME" or
// "BROKER/runtimes/NAME") and the settings key it is read from. limit is
// 0 when the key is no longer set or the scope ID is not recognised.
func entryAgentLimitForScope(vs *config.VersionedSettings, brokerID, scopeID string) (limit int64, key string) {
	rest, ok := strings.CutPrefix(scopeID, brokerID+"/")
	if !ok {
		return 0, ""
	}
	kind, name, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, ""
	}
	key = kind + "." + name + ".max_agents"
	if vs == nil {
		return 0, key
	}
	switch kind {
	case "profiles":
		limit = int64(vs.Profiles[name].MaxAgents)
	case "runtimes":
		limit = int64(vs.Runtimes[name].MaxAgents)
	default:
		return 0, ""
	}
	if limit < 0 {
		limit = 0
	}
	return limit, key
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
		// max_agents_per_broker reservations are held at store.QuotaScopeBroker
		// (subject = broker ID, not the user) rather than store.QuotaScopeSystem
		// (ptone/scion#2061 P2.2, broker_quota.go), so the per-user query below
		// always finds none for it: the row showed "0 used" regardless of real
		// broker usage. It also isn't a per-user quota at all — it's an
		// infrastructure ceiling on a broker — so unlike getUsageSummary and
		// getUsageByLimit (which sum broker reservations for their hub-wide
		// admin view), the correct fix here is to omit the row from a user's
		// own usage entirely rather than reporting a broker-wide count under
		// "my usage". ptone/scion#2313.
		if def.Name == store.LimitMaxAgentsPerBroker {
			continue
		}

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
