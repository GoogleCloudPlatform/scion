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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ListScopeResult is the outcome of a single authoritative scope decision
// for a list request. It communicates both the authorized scope set and any
// project-level exclusions imposed by AccessConstraints.
type ListScopeResult struct {
	// Scopes is the authorized project scope set (All, None, or Explicit).
	Scopes ScopeSet

	// ExcludedProjectIDs contains project IDs that are excluded from an
	// All scope by project-scoped AccessConstraints. When Scopes is All
	// and project-scoped constraints block the list permission for specific
	// projects, those project IDs appear here. The handler pushes them into
	// the store filter's ExcludedProjectIDs field.
	//
	// This field is only meaningful when Scopes.IsAll() is true.
	// When Scopes is Explicit or None, exclusions are already reflected
	// in the Scopes set directly.
	ExcludedProjectIDs []string
}

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
// RS2: Returns a typed result plus an error. Resolution dependency failures
// are propagated as errors so the handler can fail closed with an appropriate
// status code. ScopeSetNone with a nil error is a legitimate authorization
// result meaning "no authority".
func (a *AuthzService) ResolveListScopes(ctx context.Context, identity Identity, permissionID string) (ListScopeResult, error) {
	none := ListScopeResult{Scopes: ScopeSetNone()}

	if identity == nil {
		return none, nil
	}

	// Note: Suspension is enforced at the auth middleware layer, which rejects
	// suspended principals before the handler runs. ResolveListScopes does not
	// re-check suspension because no suspended identity reaches this code path.

	// Step 1: Resolve principal closure (direct principal + transitive groups).
	principals, err := a.authorizationPrincipals(ctx, identity)
	if err != nil {
		a.logger.WarnContext(ctx, "ResolveListScopes: failed to resolve principals",
			"error", err, "permission", permissionID)
		return none, fmt.Errorf("ResolveListScopes: principal resolution: %w", err)
	}

	if len(principals) == 0 {
		return none, nil
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
		return none, fmt.Errorf("ResolveListScopes: role binding load: %w", err)
	}

	if len(bindings) == 0 {
		return none, nil
	}

	// Step 3: Collect unique role definition IDs and load them.
	roleDefIDs := collectRoleDefinitionIDs(bindings)
	roleDefinitions, err := a.loadRoleDefinitions(ctx, roleDefIDs)
	if err != nil {
		a.logger.WarnContext(ctx, "ResolveListScopes: failed to load role definitions",
			"error", err, "permission", permissionID)
		return none, fmt.Errorf("ResolveListScopes: role definition load: %w", err)
	}

	// Step 4: Convert store bindings to kernel CandidateBindings.
	candidates := toCandidateBindings(bindings)

	// Step 5: Call the pure kernel function.
	scopes := ResolveAuthorizedScopes(principalClosure, permissionID, candidates, roleDefinitions, time.Now())

	// Step 6: Apply credential caveats. UAT-scoped users and project-scoped
	// agents must have their scope set intersected with the credential's
	// allowed project, since credential restrictions can only reduce authority.
	scopes = applyCredentialCaveats(identity, scopes)

	// Step 7: Apply AccessConstraints (C-2 fix).
	// Load applicable constraints for the principal closure and check whether
	// any active constraint excludes the list permission. Constraints can only
	// reduce the authorized scope set, never expand it.
	result, err := a.applyListScopeConstraints(ctx, principalClosure, identity, permissionID, scopes)
	if err != nil {
		return none, err
	}

	return result, nil
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

// applyListScopeConstraints loads active access constraints and applies them
// to the resolved scope set. If any applicable constraint excludes the list
// permission, the affected scope(s) are removed.
//
// C-2 fix: ResolveListScopes previously never loaded or applied
// AccessConstraints, allowing a principal whose list permission was removed
// by an operator boundary to retain full list visibility.
//
// RS2: Returns an error on constraint loading failure so the caller can
// propagate fail-closed status to the handler.
func (a *AuthzService) applyListScopeConstraints(
	ctx context.Context,
	closure map[string]struct{},
	identity Identity,
	permissionID string,
	scopes ScopeSet,
) (ListScopeResult, error) {
	none := ListScopeResult{Scopes: ScopeSetNone()}

	if scopes.IsNone() {
		return none, nil
	}

	constraints, err := a.loadAllAccessConstraints(ctx)
	if err != nil {
		// C-2 + R-1: fail closed when constraint loading errors.
		a.logger.WarnContext(ctx, "ResolveListScopes: failed to load access constraints (fail-closed)",
			"error", err, "permission", permissionID)
		return none, fmt.Errorf("ResolveListScopes: constraint load: %w", err)
	}
	if len(constraints) == 0 {
		return ListScopeResult{Scopes: scopes}, nil
	}

	// Convert store constraints to hub AccessConstraint.
	var hubConstraints []*AccessConstraint
	for _, sc := range constraints {
		hc := storeToHubAccessConstraint(sc)
		if hc != nil {
			hubConstraints = append(hubConstraints, hc)
		}
	}

	// Normalize all closure keys so that dev/federated variants match
	// the canonical "user"/"agent" types used in constraint subjects.
	normalizedClosure := normalizeClosureTypes(closure)
	now := time.Now()

	// Check system-scoped constraints first: if any applicable system-scoped
	// constraint excludes the list permission, return ScopeSetNone.
	systemApplicable := FilterApplicableConstraints(
		hubConstraints, normalizedClosure,
		ScopeTypeSystem, "",
	)
	systemRestrictions := ConstraintsToRestrictions(systemApplicable, now)
	for _, r := range systemRestrictions {
		if r.Check == nil || !r.Check(permissionID) {
			return none, nil
		}
	}

	// For explicit project sets, check project-scoped constraints and remove
	// projects where a constraint excludes the list permission.
	projectIDs := scopes.ProjectIDs()
	if len(projectIDs) > 0 {
		var retained []string
		for _, pid := range projectIDs {
			projectApplicable := FilterApplicableConstraints(
				hubConstraints, normalizedClosure,
				ScopeTypeProject, pid,
			)
			projectRestrictions := ConstraintsToRestrictions(projectApplicable, now)
			blocked := false
			for _, r := range projectRestrictions {
				if r.Check == nil || !r.Check(permissionID) {
					blocked = true
					break
				}
			}
			if !blocked {
				retained = append(retained, pid)
			}
		}
		if len(retained) == 0 {
			return none, nil
		}
		return ListScopeResult{Scopes: ScopeSetExplicit(retained...)}, nil
	}

	// RS2: For All scope, check project-scoped constraints that apply to
	// the principal closure. Collect project IDs where any constraint blocks
	// the list permission — these become exclusions in the store query.
	// This prevents project-scoped constraints from being silently ignored
	// when the fully reduced scope is All.
	if scopes.IsAll() {
		var excludedProjectIDs []string
		for _, c := range hubConstraints {
			if c == nil || c.Scope.Type != ScopeTypeProject || c.Scope.ID == "" {
				continue
			}
			if !c.Subject.MatchesPrincipalClosure(normalizedClosure) {
				continue
			}
			// Check if this project-scoped constraint blocks the permission.
			cr := ConstraintsToRestrictions([]*AccessConstraint{c}, now)
			for _, r := range cr {
				if r.Check == nil || !r.Check(permissionID) {
					excludedProjectIDs = append(excludedProjectIDs, c.Scope.ID)
					break
				}
			}
		}
		return ListScopeResult{
			Scopes:             scopes,
			ExcludedProjectIDs: excludedProjectIDs,
		}, nil
	}

	return ListScopeResult{Scopes: scopes}, nil
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
