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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ResolveListScopes resolves the set of projects the caller is authorized to
// see for a given list permission (e.g. "project.list" or "agent.list").
//
// Currently wired into the project and agent list handlers. The same
// hasAdminView pattern also exists in handlers_groups.go, template_handlers.go,
// and harness_config_handlers.go — those should be converted in CO1 or a
// follow-up using this same adapter.
//
// It bridges the store layer and the AK1 authorization kernel:
//  1. Resolves the caller's principal closure (direct principal + effective groups).
//  2. Loads applicable role bindings via ListRoleBindingsForPrincipals.
//  3. Loads role definitions for those bindings.
//  4. Calls ResolveAuthorizedScopes to compute the ScopeSet.
//
// The returned ScopeSet tells the handler how to filter:
//   - ScopeSetAll: proceed with unfiltered query (admin view).
//   - ScopeSetNone: return empty list.
//   - Explicit set: push project IDs into the store query as a WHERE filter.
//
// Errors are fail-closed: any resolution failure returns ScopeSetNone.
func (a *AuthzService) ResolveListScopes(ctx context.Context, identity Identity, permissionID string) ScopeSet {
	if identity == nil {
		return ScopeSetNone()
	}

	// Step 1: Resolve principal closure (direct principal + transitive groups).
	principals, err := a.authorizationPrincipals(ctx, identity)
	if err != nil {
		a.logger.WarnContext(ctx, "ResolveListScopes: failed to resolve principals",
			"error", err, "permission", permissionID)
		return ScopeSetNone()
	}

	if len(principals) == 0 {
		return ScopeSetNone()
	}

	// Build the principal closure map for ResolveAuthorizedScopes.
	// O2: Use typed composite keys (type:id) to prevent collisions.
	principalClosure := make(map[string]struct{}, len(principals))
	for _, p := range principals {
		principalClosure[p.Type+":"+p.ID] = struct{}{}
	}

	// Step 2: Load applicable role bindings for all principals in the closure.
	// We pass nil for scopeTypes and scopeIDs to get all bindings (both system
	// and project scoped) so ResolveAuthorizedScopes can compute the full set.
	bindings, err := a.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
	if err != nil {
		a.logger.WarnContext(ctx, "ResolveListScopes: failed to load role bindings",
			"error", err, "permission", permissionID)
		return ScopeSetNone()
	}

	if len(bindings) == 0 {
		return ScopeSetNone()
	}

	// Step 3: Collect unique role definition IDs and load them.
	roleDefIDs := collectRoleDefinitionIDs(bindings)
	roleDefinitions, err := a.loadRoleDefinitions(ctx, roleDefIDs)
	if err != nil {
		a.logger.WarnContext(ctx, "ResolveListScopes: failed to load role definitions",
			"error", err, "permission", permissionID)
		return ScopeSetNone()
	}

	// Step 4: Convert store bindings to kernel CandidateBindings.
	candidates := toCandidateBindings(bindings)

	// Step 5: Call the pure kernel function.
	scopes := ResolveAuthorizedScopes(principalClosure, permissionID, candidates, roleDefinitions, time.Now())

	// Step 6: Apply credential caveats. UAT-scoped users and project-scoped
	// agents must have their scope set intersected with the credential's
	// allowed project, since credential restrictions can only reduce authority.
	scopes = applyCredentialCaveats(identity, scopes)

	return scopes
}

// applyCredentialCaveats intersects the resolved scope set with any credential-
// level project restrictions. This implements the design invariant that
// "credential scopes, suspension, and delegation ceilings run after the union
// [of grants] and can only reduce it."
func applyCredentialCaveats(identity Identity, scopes ScopeSet) ScopeSet {
	switch id := identity.(type) {
	case *ScopedUserIdentity:
		// UAT-scoped user: restrict to the UAT's project scope.
		if pid := id.ScopedProjectID(); pid != "" {
			return scopes.Intersection(ScopeSetExplicit(pid))
		}
	case AgentIdentity:
		// Agent with a project scope from its token.
		if pid := id.ProjectID(); pid != "" {
			return scopes.Intersection(ScopeSetExplicit(pid))
		}
	}
	return scopes
}

// collectRoleDefinitionIDs extracts unique role definition IDs from bindings.
func collectRoleDefinitionIDs(bindings []*store.RoleBinding) []string {
	seen := make(map[string]struct{}, len(bindings))
	var ids []string
	for _, b := range bindings {
		if _, ok := seen[b.RoleDefinitionID]; !ok {
			seen[b.RoleDefinitionID] = struct{}{}
			ids = append(ids, b.RoleDefinitionID)
		}
	}
	return ids
}

// loadRoleDefinitions fetches role definitions by ID in a single batch query
// and converts them to the kernel's RolePermissions map.
//
// Missing role definitions are silently omitted — the corresponding bindings
// will simply not contribute permissions. Transient store errors (DB timeout,
// connection reset) are propagated to the caller, which fails closed by
// returning ScopeSetNone.
func (a *AuthzService) loadRoleDefinitions(ctx context.Context, roleDefIDs []string) (map[string]*RolePermissions, error) {
	if len(roleDefIDs) == 0 {
		return map[string]*RolePermissions{}, nil
	}

	defs, err := a.store.GetRoleDefinitionsByIDs(ctx, roleDefIDs)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*RolePermissions, len(defs))
	for _, rd := range defs {
		result[rd.ID] = NewRolePermissions(rd.ID, rd.Name, rd.ScopeType, rd.Permissions)
	}
	return result, nil
}

// toCandidateBindings converts store RoleBindings to the kernel's
// CandidateBinding type.
func toCandidateBindings(bindings []*store.RoleBinding) []CandidateBinding {
	candidates := make([]CandidateBinding, len(bindings))
	for i, b := range bindings {
		candidates[i] = CandidateBinding{
			BindingID:        b.ID,
			RoleDefinitionID: b.RoleDefinitionID,
			PrincipalType:    b.PrincipalType,
			PrincipalID:      b.PrincipalID,
			ScopeType:        b.ScopeType,
			ScopeID:          b.ScopeID,
		}
		if b.NotBefore != nil {
			candidates[i].NotBefore = *b.NotBefore
		}
		if b.ExpiresAt != nil {
			candidates[i].ExpiresAt = *b.ExpiresAt
		}
	}
	return candidates
}
