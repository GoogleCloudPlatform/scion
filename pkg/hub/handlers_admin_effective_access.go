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
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// GET /api/v1/admin/effective-access
//
// Admin endpoint returning a principal's effective-access composition for the
// system scope: active role bindings, applicable access constraints
// (boundaries), and intrinsic restrictions.
//
// Authorization: requires hub.audit.read (super-admin). Cross-principal
// access is explicitly gated. No confidential principal IDs are exposed
// in the response — the caller already knows the target principal.
//
// Scope: all computations are scoped to the system scope. This endpoint
// does NOT aggregate across project scopes. Per-project effective access
// must be queried with a project-scoped endpoint (not implemented here).
// ---------------------------------------------------------------------------

// adminEffectiveAccessResponse is the response for GET /api/v1/admin/effective-access.
type adminEffectiveAccessResponse struct {
	// ScopeType documents the scope of this computation. Always "system"
	// for this endpoint.
	ScopeType string `json:"scopeType"`

	// ActiveBindingCount is the number of currently active role bindings
	// (within their activation window) at system scope.
	ActiveBindingCount int `json:"activeBindingCount"`

	// Boundaries lists access constraints that apply to this principal
	// at system scope and are currently active.
	Boundaries []adminEffectiveAccessBoundary `json:"boundaries"`

	// Restrictions lists intrinsic restrictions that reduce effective access
	// (e.g., agent credential scope).
	Restrictions []adminEffectiveAccessRestriction `json:"restrictions"`
}

type adminEffectiveAccessBoundary struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
}

type adminEffectiveAccessRestriction struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// handleAdminEffectiveAccess serves GET /api/v1/admin/effective-access.
func (s *Server) handleAdminEffectiveAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()

	// Fail closed: authorization service must be available.
	if s.authzService == nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"authorization service unavailable", nil)
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Authorization: require hub.audit.read (super-admin).
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("manage"),
		Permission: "hub.audit.read",
	})
	if !decision.Allowed {
		writeForbidden(w, "requires hub.audit.read permission")
		return
	}

	// Parse query parameters.
	principalType := r.URL.Query().Get("principalType")
	principalID := r.URL.Query().Get("principalId")
	if principalType == "" || principalID == "" {
		BadRequest(w, "principalType and principalId are required")
		return
	}
	if principalType != "user" && principalType != "agent" {
		BadRequest(w, "principalType must be \"user\" or \"agent\"")
		return
	}

	// Verify the target principal exists.
	switch principalType {
	case "user":
		if _, err := s.store.GetUser(ctx, principalID); err != nil {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "principal not found", nil)
			return
		}
	case "agent":
		if _, err := s.store.GetAgent(ctx, principalID); err != nil {
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "principal not found", nil)
			return
		}
	}

	// Build principal closure (direct + group memberships) for system scope.
	normalizedType := NormalizePrincipalType(principalType)
	principals := []store.PrincipalRef{{Type: normalizedType, ID: principalID}}

	switch normalizedType {
	case store.RoleBindingPrincipalUser:
		gids, err := s.store.GetEffectiveGroups(ctx, principalID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to resolve group memberships", nil)
			return
		}
		for _, gid := range gids {
			principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
		}
	case store.RoleBindingPrincipalAgent:
		gids, err := s.store.GetEffectiveGroupsForAgent(ctx, principalID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to resolve agent group memberships", nil)
			return
		}
		for _, gid := range gids {
			principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
		}
	}

	// List system-scoped bindings for the principal closure.
	scopeTypes := []string{store.RoleScopeSystem}
	bindings, err := s.store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to list role bindings", nil)
		return
	}

	// Count active bindings (within their activation window).
	now := time.Now()
	activeCount := 0
	for _, b := range bindings {
		if b.NotBefore != nil && now.Before(*b.NotBefore) {
			continue
		}
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			continue
		}
		activeCount++
	}

	// Compute boundary information from access constraints.
	// Only include constraints that apply to this principal and are currently
	// in their active window.
	boundaries := make([]adminEffectiveAccessBoundary, 0)
	constraints, err := s.store.ListAccessConstraints(ctx, 0, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to list access constraints", nil)
		return
	}

	// Build principal ID lookup for matching.
	principalIDSet := make(map[string]bool, len(principals))
	for _, p := range principals {
		principalIDSet[p.ID] = true
	}

	for _, c := range constraints {
		if c.Disabled {
			continue
		}

		// Active-window check: skip constraints that are not yet active or expired.
		if c.NotBefore != nil && now.Before(*c.NotBefore) {
			continue
		}
		if c.ExpiresAt != nil && now.After(*c.ExpiresAt) {
			continue
		}

		// Scope applicability: only include constraints whose scope
		// intersects system scope. Constraints scoped to a specific project
		// do not affect system-scope effective access.
		if c.ScopeType == "project" && c.ScopeID != "" {
			continue
		}

		// Subject matching: does this constraint apply to this principal?
		applies := false
		switch c.SubjectKind {
		case "all_principals":
			applies = true
		case "principal":
			if c.SubjectPrincipalID != nil && *c.SubjectPrincipalID == principalID {
				applies = true
			}
		case "group_closure":
			if c.SubjectGroupID != nil && principalIDSet[*c.SubjectGroupID] {
				applies = true
			}
		}
		if !applies {
			continue
		}

		boundaries = append(boundaries, adminEffectiveAccessBoundary{
			ID:     c.ID,
			Name:   c.Name,
			Status: "active",
		})
	}

	// Intrinsic restrictions.
	restrictions := make([]adminEffectiveAccessRestriction, 0)
	if principalType == "agent" {
		restrictions = append(restrictions, adminEffectiveAccessRestriction{
			Kind:   "credential_scope",
			Label:  "Agent credential scope",
			Detail: "Agent credentials are scoped to their project",
		})
	}

	// Sort boundaries by name for stable output.
	sort.Slice(boundaries, func(i, j int) bool {
		return boundaries[i].Name < boundaries[j].Name
	})

	// Response does not echo back principalType/principalId — the caller
	// already knows these values. Avoids unnecessary principal ID exposure.
	resp := adminEffectiveAccessResponse{
		ScopeType:          store.RoleScopeSystem,
		ActiveBindingCount: activeCount,
		Boundaries:         boundaries,
		Restrictions:       restrictions,
	}

	writeJSON(w, http.StatusOK, resp)
}
