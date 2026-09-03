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
// Narrowly scoped admin endpoint returning a principal's effective-access
// composition: potential permissions, applicable access constraints
// (boundaries), intrinsic restrictions, and final effective permission count.
//
// Authorization: requires hub.audit.read (super-admin). Cross-principal
// access is explicitly gated; no confidential IDs or policy internals are
// exposed beyond what is needed for the admin composition view.
// ---------------------------------------------------------------------------

// adminEffectiveAccessResponse is the response for GET /api/v1/admin/effective-access.
type adminEffectiveAccessResponse struct {
	PrincipalType string `json:"principalType,omitempty"`
	PrincipalID   string `json:"principalId,omitempty"`

	PotentialPermissionCount int `json:"potentialPermissionCount"`
	EffectivePermissionCount int `json:"effectivePermissionCount"`

	Boundaries   []adminEffectiveAccessBoundary    `json:"boundaries"`
	Restrictions []adminEffectiveAccessRestriction  `json:"restrictions"`

	// Redacted is present when the viewer does not have full audit access.
	Redacted *adminRedactionNotice `json:"redacted,omitempty"`
}

type adminEffectiveAccessBoundary struct {
	ID                string `json:"id"`
	Name              string `json:"name,omitempty"`
	Status            string `json:"status"`
	RemovedCount      int    `json:"removedCount"`
	OverlapCount      int    `json:"overlapCount"`
	MembershipSummary string `json:"membershipSummary,omitempty"`
}

type adminEffectiveAccessRestriction struct {
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	RemovedCount int    `json:"removedCount"`
	Detail       string `json:"detail,omitempty"`
}

type adminRedactionNotice struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// handleAdminEffectiveAccess serves GET /api/v1/admin/effective-access.
func (s *Server) handleAdminEffectiveAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Authorization: require hub.audit.read.
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}
	if s.authzService != nil {
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

	// Cross-principal privacy: non-self requests require hub.audit.read
	// (already checked above). Self-requests are allowed for self-diagnostics.
	// No additional check needed here since hub.audit.read is already enforced.

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

	// Compute potential permissions (from all active role bindings, before
	// access constraint intersection).
	normalizedType := NormalizePrincipalType(principalType)
	principals := []store.PrincipalRef{{Type: normalizedType, ID: principalID}}

	// Expand group memberships.
	switch normalizedType {
	case store.RoleBindingPrincipalUser:
		if gids, err := s.store.GetEffectiveGroups(ctx, principalID); err == nil {
			for _, gid := range gids {
				principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
			}
		}
	case store.RoleBindingPrincipalAgent:
		if gids, err := s.store.GetEffectiveGroupsForAgent(ctx, principalID); err == nil {
			for _, gid := range gids {
				principals = append(principals, store.PrincipalRef{Type: "group", ID: gid})
			}
		}
	}

	bindings, err := s.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Compute potential permissions from active bindings.
	now := time.Now()
	potentialSet := make(map[string]bool)
	for _, b := range bindings {
		// Activation check.
		if b.NotBefore != nil && now.Before(*b.NotBefore) {
			continue
		}
		if b.ExpiresAt != nil && now.After(*b.ExpiresAt) {
			continue
		}
		rd, err := s.store.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		for _, p := range rd.Permissions {
			potentialSet[p] = true
		}
	}
	potentialCount := len(potentialSet)

	// Compute effective permissions (with access constraint intersection).
	effectivePerms, err := s.authzService.getEffectivePermissions(
		ctx, normalizedType, principalID,
		ScopeTypeSystem, "",
	)
	if err != nil {
		effectivePerms = nil
	}
	effectiveSet := make(map[string]bool, len(effectivePerms))
	for _, p := range effectivePerms {
		effectiveSet[p] = true
	}
	effectiveCount := len(effectiveSet)

	// Compute boundary information from access constraints.
	boundaries := make([]adminEffectiveAccessBoundary, 0)
	constraints, err := s.store.ListAccessConstraints(ctx, 0, 0)
	if err == nil {
		// Build principal ID lookup for matching.
		principalIDSet := make(map[string]bool, len(principals))
		for _, p := range principals {
			principalIDSet[p.ID] = true
		}

		for _, c := range constraints {
			if c.Disabled {
				continue
			}

			// Check if this constraint applies to this principal.
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

			status := "active"
			if c.NotBefore != nil && now.Before(*c.NotBefore) {
				status = "scheduled"
			}
			if c.ExpiresAt != nil && now.After(*c.ExpiresAt) {
				status = "expired"
			}

			// Count how many potential permissions are removed by this boundary.
			maxPermsSet := make(map[string]bool, len(c.MaximumPermissions))
			for _, p := range c.MaximumPermissions {
				maxPermsSet[p] = true
			}
			removedCount := 0
			for p := range potentialSet {
				if !maxPermsSet[p] {
					removedCount++
				}
			}

			boundaries = append(boundaries, adminEffectiveAccessBoundary{
				ID:           c.ID,
				Name:         c.Name,
				Status:       status,
				RemovedCount: removedCount,
			})
		}
	}

	// Intrinsic restrictions.
	restrictions := make([]adminEffectiveAccessRestriction, 0)
	if principalType == "agent" {
		restrictions = append(restrictions, adminEffectiveAccessRestriction{
			Kind:         "credential_scope",
			Label:        "Agent credential scope",
			RemovedCount: 0,
			Detail:       "Agent credentials are scoped to their project",
		})
	}

	// Sort boundaries by name for stable output.
	sort.Slice(boundaries, func(i, j int) bool {
		return boundaries[i].Name < boundaries[j].Name
	})

	resp := adminEffectiveAccessResponse{
		PrincipalType:            principalType,
		PrincipalID:              principalID,
		PotentialPermissionCount: potentialCount,
		EffectivePermissionCount: effectiveCount,
		Boundaries:               boundaries,
		Restrictions:             restrictions,
	}

	writeJSON(w, http.StatusOK, resp)
}
