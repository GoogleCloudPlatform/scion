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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// delegationCeilingCacheKey is the context key for request-scoped caching of
// delegation edge lookups.
type delegationCeilingCacheKey struct{}

// delegationCeilingCache stores delegation edge lookups within a single request
// to avoid redundant store queries. It is NOT safe across requests.
type delegationCeilingCache struct {
	edges map[string][]*store.DelegationEdge // key: "delegateType:delegateID"
	perms map[string][]string                // key: "principalType:principalID:scopeType:scopeID"
}

// getDelegationCeilingCache retrieves or creates the request-scoped cache from context.
func getDelegationCeilingCache(ctx context.Context) *delegationCeilingCache {
	if cache, ok := ctx.Value(delegationCeilingCacheKey{}).(*delegationCeilingCache); ok {
		return cache
	}
	return nil
}

// contextWithDelegationCeilingCache attaches a delegation ceiling cache to the context.
func contextWithDelegationCeilingCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, delegationCeilingCacheKey{}, &delegationCeilingCache{
		edges: make(map[string][]*store.DelegationEdge),
		perms: make(map[string][]string),
	})
}

// isReadOnlyOperation returns true ONLY for actions that are genuinely safe
// to fail-open on store errors during delegation ceiling checks. The default
// is false (fail-closed) so that any future action added to the system is
// automatically fail-closed until explicitly classified as safe.
//
// This is an allowlist: read and list are safe; everything else (including
// delete, update, stop, attach, dispatch, etc.) is NOT safe and must fail
// closed on errors.
func isReadOnlyOperation(action Action) bool {
	switch action {
	case ActionRead, ActionList, ActionVerify:
		return true
	default:
		return false
	}
}

// isMintingOperation returns true for actions that create or extend durable
// authority. Kept as a predicate for callers that need to distinguish
// minting from other mutation types (e.g. orphaned delegation).
func isMintingOperation(action Action) bool {
	switch action {
	case ActionCreate, ActionManage, ActionRegister, ActionAddMember, ActionMint, ActionAssign:
		return true
	default:
		return false
	}
}

// checkDelegationCeiling walks the delegation edge chain for an agent and
// verifies that every ancestor still holds the permission being exercised.
// Returns (allowed, reason, error).
//
// On store errors for minting/extension operations, returns (false, "fail-closed", err).
// Uses request-scoped caching to avoid repeated lookups within the same request.
//
// Scope derivation (R3-1): the ceiling scope is derived from the PRINCIPAL's
// own project (AgentIdentity.ProjectID()), not from the Resource. This is
// correct because delegation edges are always created with the agent's project
// ID, and most Resource literals in handlers do not set ParentType, which
// would silently map to system scope and deny.
//
// If the resource belongs to a DIFFERENT project than the agent's own, the
// request is treated as cross-project and denied (no edge will match).
//
// The algorithm:
//  1. Look up the active delegation edge(s) where delegate = this agent.
//  2. If no edge found:
//     - If the backfill has not yet run (no marker): allow pre-migration
//     agents temporarily. Once the backfill completes, this guard expires.
//     - For federated agents: deny (absent edge = no authority, not unlimited).
//     - Post-backfill with no edge: deny (no delegated authority).
//  3. Walk up the chain: for each delegator, verify they still hold the
//     permission. Branch on delegator resolvability:
//     - Real delegator: live ceiling check always.
//     - Synthetic/missing delegator (ErrNotFound): freeze at agent's own role.
//     - Genuine store fault: fail closed for minting, open for reads.
//  4. The Grandfathered flag is provenance metadata only — never used in
//     allow/deny decisions.
func (a *AuthzService) checkDelegationCeiling(
	ctx context.Context,
	req AuthzRequest,
	agentID string,
	explain *[]DecisionStep,
) (bool, string, error) {
	// Derive scope from the principal's own project, not from the resource.
	// The agent's project ID is always available from the identity and matches
	// the scope under which delegation edges were created.
	//
	// scopeType is deliberately hard-coded to RoleScopeProject. System-scoped
	// edges could never match, and no system-scoped edges exist. The effect
	// is protective — it restricts ceiling matching to project-scoped edges only.
	scopeType := store.RoleScopeProject
	scopeID := ""
	if agent, ok := req.Principal.Identity.(AgentIdentity); ok {
		scopeID = agent.ProjectID()
	}
	if scopeID == "" {
		a.logger.Warn("delegation ceiling: identity has no project scope, denying",
			"principal_type", store.DelegationPrincipalAgent,
			"principal_id", agentID,
			"identity_type", fmt.Sprintf("%T", req.Principal.Identity),
		)
	}

	// If the resource resolves to a different project, treat it as
	// cross-project: use the resource's project as scope so that no edge
	// matches (the agent's edges are scoped to the agent's own project).
	resourceProjectID := resourceProjectScope(req.Resource)
	if resourceProjectID != "" && resourceProjectID != scopeID {
		scopeID = resourceProjectID
	}

	return a.walkDelegationChain(ctx, req, store.DelegationPrincipalAgent, agentID, req.Principal.Identity, scopeType, scopeID, explain, 0)
}

// maxDelegationDepth limits the delegation chain walk to prevent infinite loops.
const maxDelegationDepth = 10

// walkDelegationChain recursively walks the delegation chain to verify
// that every ancestor still holds the requested permission.
// The identity parameter is used at depth 0 to determine whether
// ancestry is hub-attested (local) or federated.
// scopeType/scopeID define the delegation scope (derived from the principal's
// project at depth 0, propagated unchanged at deeper levels).
func (a *AuthzService) walkDelegationChain(
	ctx context.Context,
	req AuthzRequest,
	principalType, principalID string,
	identity Identity,
	scopeType, scopeID string,
	explain *[]DecisionStep,
	depth int,
) (bool, string, error) {
	if depth > maxDelegationDepth {
		return false, "delegation chain exceeded maximum depth", nil
	}

	// Look up active delegation edges for this principal.
	allEdges, err := a.getCachedDelegationEdges(ctx, principalType, principalID)
	if err != nil {
		reason := fmt.Sprintf("delegation ceiling check failed (store error): %v", err)
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_error",
				Detail: reason,
			})
		}
		// Fail closed unless the action is explicitly read-only.
		if !isReadOnlyOperation(req.Action) {
			return false, "delegation ceiling check failed (fail-closed): " + err.Error(), err
		}
		return true, "delegation ceiling check skipped (store error, read-only)", nil
	}

	// Filter edges by the principal-derived scope. An agent with authority
	// in project P1 must not satisfy the ceiling for a request in project P2.
	edges := filterEdgesByScope(allEdges, scopeType, scopeID)

	// No edge found (for this scope): check whether the backfill migration
	// has completed.
	//
	// Before the backfill runs, hub-attested agents may not have edges yet.
	// Gate the temporary allow on the ABSENCE of the backfill completion
	// marker — once the marker exists, all agents must have edges.
	//
	// For federated agents: no edge means no authority was delegated by this
	// hub. Absent edge = floor (no permissions), NEVER unlimited authority.
	if len(edges) == 0 {
		if depth == 0 && identity != nil && AncestryIsHubAttested(identity) {
			// Check if the backfill has already run. If the marker exists,
			// every agent should have an edge — deny if missing.
			if !a.backfillCompleted(ctx) {
				if explain != nil {
					*explain = append(*explain, DecisionStep{
						Step:   "delegation_ceiling_pre_backfill",
						Detail: fmt.Sprintf("no delegation edge for local %s:%s; backfill not yet complete — allowing temporarily", principalType, principalID),
					})
				}
				a.logger.Debug("No delegation edge found for local agent, backfill not yet complete",
					"principal_type", principalType,
					"principal_id", principalID)
				return true, "no delegation edge (pre-backfill)", nil
			}
		}
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_no_edge",
				Detail: fmt.Sprintf("no delegation edge for %s:%s; no delegated authority", principalType, principalID),
			})
		}
		return false, fmt.Sprintf("no delegation edge for %s:%s (no delegated authority)", principalType, principalID), nil
	}

	// The single active edge for the scope decides. The partial unique index
	// on (delegate_type, delegate_id, scope_type, scope_id) WHERE active=true
	// enforces at most one active edge per (delegate, scope). Because we
	// already filtered by request scope, activeCount here matches the
	// constraint tuple exactly. If multiple active edges are found
	// (invariant violation), fail closed unless read-only.
	//
	// The Grandfathered flag is recorded for provenance/audit only.
	// It NEVER causes a bypass — all edges get the live ceiling check.
	activeCount := 0
	for _, e := range edges {
		if e.Active {
			activeCount++
		}
	}
	if activeCount > 1 {
		a.logger.Error("Multiple active delegation edges found (invariant violation)",
			"principal_type", principalType,
			"principal_id", principalID,
			"active_count", activeCount)
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_duplicate_edges",
				Detail: fmt.Sprintf("INVARIANT VIOLATION: %d active edges for %s:%s", activeCount, principalType, principalID),
			})
		}
		if !isReadOnlyOperation(req.Action) {
			return false, fmt.Sprintf("multiple active delegation edges for %s:%s (fail-closed on invariant violation)", principalType, principalID), nil
		}
		// For read-only, continue with the first edge but log the error.
	}

	for _, edge := range edges {
		if !edge.Active {
			continue
		}

		if edge.Grandfathered && explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_grandfathered_edge_provenance",
				Detail: fmt.Sprintf("edge %s has grandfathered provenance (audit only, no bypass)", edge.ID),
			})
		}

		// Resolve the permission ID from the request action and resource.
		permissionID := resolvePermissionID(req.Resource, req.Action)

		if edge.DelegatorType == store.DelegationPrincipalUser {
			allowed, reason, err := a.checkUserHoldsPermission(ctx, edge.DelegatorID, permissionID, edge.ScopeType, edge.ScopeID, explain)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					// Delegator definitively does not exist (e.g. synthetic
					// "system/migration" principal). Freeze ceiling at the
					// agent's own recorded role — allow reads at current
					// level, deny minting to prevent escalation.
					return a.handleOrphanedDelegation(ctx, req, principalID, edge, permissionID, explain)
				}
				// Genuine store fault — fail closed unless read-only.
				if !isReadOnlyOperation(req.Action) {
					return false, "delegation ceiling check failed (fail-closed): " + err.Error(), err
				}
				return true, "delegation ceiling check skipped (store error, read-only)", nil
			}
			if !allowed {
				if explain != nil {
					*explain = append(*explain, DecisionStep{
						Step:   "delegation_ceiling_denied",
						Detail: fmt.Sprintf("delegator user %s no longer holds permission %s: %s", edge.DelegatorID, permissionID, reason),
					})
				}
				return false, fmt.Sprintf("delegator %s no longer holds %s", edge.DelegatorID, permissionID), nil
			}
			if explain != nil {
				*explain = append(*explain, DecisionStep{
					Step:   "delegation_ceiling_allowed",
					Detail: fmt.Sprintf("delegator user %s holds permission %s", edge.DelegatorID, permissionID),
				})
			}
			return true, "delegation ceiling passed", nil
		}

		if edge.DelegatorType == store.DelegationPrincipalAgent {
			allowed, reason, err := a.checkAgentHoldsPermission(ctx, edge.DelegatorID, permissionID, edge.ScopeType, edge.ScopeID, explain)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					// Delegator agent definitively does not exist.
					return a.handleOrphanedDelegation(ctx, req, principalID, edge, permissionID, explain)
				}
				// Genuine store fault — fail closed unless read-only.
				if !isReadOnlyOperation(req.Action) {
					return false, "delegation ceiling check failed (fail-closed): " + err.Error(), err
				}
				return true, "delegation ceiling check skipped (store error, read-only)", nil
			}
			if !allowed {
				if explain != nil {
					*explain = append(*explain, DecisionStep{
						Step:   "delegation_ceiling_denied",
						Detail: fmt.Sprintf("delegator agent %s no longer holds permission %s: %s", edge.DelegatorID, permissionID, reason),
					})
				}
				return false, fmt.Sprintf("delegator agent %s no longer holds %s: %s", edge.DelegatorID, permissionID, reason), nil
			}

			// The parent agent holds the permission, but we need to walk further up.
			chainAllowed, chainReason, chainErr := a.walkDelegationChain(ctx, req, store.DelegationPrincipalAgent, edge.DelegatorID, nil, scopeType, scopeID, explain, depth+1)
			if chainErr != nil {
				if !isReadOnlyOperation(req.Action) {
					return false, "delegation ceiling check failed (fail-closed): " + chainErr.Error(), chainErr
				}
				return true, "delegation ceiling check skipped (store error, read-only)", nil
			}
			if !chainAllowed {
				return false, chainReason, nil
			}
			if explain != nil {
				*explain = append(*explain, DecisionStep{
					Step:   "delegation_ceiling_allowed",
					Detail: fmt.Sprintf("delegation chain through agent %s verified", edge.DelegatorID),
				})
			}
			return true, "delegation ceiling passed", nil
		}
	}

	// All edges checked and none resolved to an allowed state
	return false, "no active delegation edge resolved to an allowed state", nil
}

// handleOrphanedDelegation handles the case where a delegator cannot be
// resolved (ErrNotFound). This covers synthetic principals like
// "system/migration" and deleted delegators. The agent keeps its current
// permissions (reads work at its recorded role) but cannot mint or escalate.
func (a *AuthzService) handleOrphanedDelegation(
	ctx context.Context,
	req AuthzRequest,
	agentID string,
	edge *store.DelegationEdge,
	permissionID string,
	explain *[]DecisionStep,
) (bool, string, error) {
	a.logger.Info("Orphaned delegation: delegator not found, ceiling frozen at agent's own role",
		"agent_id", agentID,
		"delegator_type", edge.DelegatorType,
		"delegator_id", edge.DelegatorID,
		"edge_role", edge.Role)

	// Minting operations are always denied for orphaned delegations —
	// the agent cannot escalate without a live delegator.
	if isMintingOperation(req.Action) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_orphaned_deny_mint",
				Detail: fmt.Sprintf("delegator %s:%s not found; minting denied (ceiling frozen)", edge.DelegatorType, edge.DelegatorID),
			})
		}
		return false, fmt.Sprintf("delegator %s not found; minting denied (orphaned delegation)", edge.DelegatorID), nil
	}

	// Non-minting operations that are NOT read-only must also be denied
	// for orphaned delegations — the agent cannot mutate without a live
	// delegator.
	if !isReadOnlyOperation(req.Action) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_orphaned_deny_mutation",
				Detail: fmt.Sprintf("delegator %s:%s not found; non-read-only action %s denied (ceiling frozen)", edge.DelegatorType, edge.DelegatorID, req.Action),
			})
		}
		return false, fmt.Sprintf("delegator %s not found; non-read-only action %s denied (orphaned delegation)", edge.DelegatorID, req.Action), nil
	}

	// For read-only operations: allow if the agent's recorded role on the
	// edge covers the requested permission. The ceiling is frozen at the
	// agent's edge role — it can exercise existing permissions but not
	// expand them.
	edgeRole := AgentRole(edge.Role)
	edgeScopes := ScopesForRole(edgeRole)
	requiredScope := permissionToAgentScope(permissionID)

	if requiredScope == "" {
		// No agent scope maps to this permission. Check whether it is a
		// known read/list permission in the registry (agents have implicit
		// baseline read access) or a genuinely unmapped permission.
		//
		// A genuinely unmapped permission must DENY — "absence is not
		// permission" (R2-3). Only known read/list permissions are allowed
		// at the frozen ceiling level.
		isKnownRead := false
		for _, perm := range permissions.Registry {
			if perm.ID == permissionID {
				if perm.Action == "read" || perm.Action == "list" || perm.Action == "verify" {
					isKnownRead = true
				}
				break
			}
		}
		if !isKnownRead {
			a.logger.Warn("Unmapped permission in orphaned delegation — denying",
				"agent_id", agentID,
				"permission_id", permissionID,
				"edge_role", edge.Role)
			if explain != nil {
				*explain = append(*explain, DecisionStep{
					Step:   "delegation_ceiling_orphaned_deny_unmapped",
					Detail: fmt.Sprintf("delegator %s:%s not found; permission %s has no agent scope mapping — denied", edge.DelegatorType, edge.DelegatorID, permissionID),
				})
			}
			return false, fmt.Sprintf("orphaned delegation: unmapped permission %s denied", permissionID), nil
		}
		// Known read/list permission — allow at the agent's frozen
		// ceiling level.
		if explain != nil {
			*explain = append(*explain, DecisionStep{
				Step:   "delegation_ceiling_orphaned_allow_read",
				Detail: fmt.Sprintf("delegator %s:%s not found; read allowed at frozen ceiling (role=%s)", edge.DelegatorType, edge.DelegatorID, edge.Role),
			})
		}
		return true, "orphaned delegation: read allowed at frozen ceiling", nil
	}

	for _, scope := range edgeScopes {
		if scope == requiredScope {
			if explain != nil {
				*explain = append(*explain, DecisionStep{
					Step:   "delegation_ceiling_orphaned_allow_scope",
					Detail: fmt.Sprintf("delegator %s:%s not found; scope %s covered by frozen ceiling (role=%s)", edge.DelegatorType, edge.DelegatorID, requiredScope, edge.Role),
				})
			}
			return true, "orphaned delegation: scope covered at frozen ceiling", nil
		}
	}

	if explain != nil {
		*explain = append(*explain, DecisionStep{
			Step:   "delegation_ceiling_orphaned_deny_scope",
			Detail: fmt.Sprintf("delegator %s:%s not found; scope %s not in frozen ceiling (role=%s)", edge.DelegatorType, edge.DelegatorID, requiredScope, edge.Role),
		})
	}
	return false, fmt.Sprintf("orphaned delegation: scope %s exceeds frozen ceiling (role=%s)", requiredScope, edge.Role), nil
}

// backfillCompleted checks whether the delegation edge backfill migration
// has run by looking for the hub-settings marker.
//
// Caching is monotonic (R3-2): once latched to true, the value is permanent
// and the store is never re-queried. A false result (marker not yet present)
// is NOT cached — the store is re-queried on the next call so that the latch
// catches up as soon as the backfill completes. This prevents a race where
// the first call lands before the marker exists and permanently caches false,
// allowing edge-less agents for the entire process lifetime.
//
// Error handling:
//   - ErrNotFound → backfill has not run yet, return false (allow pre-backfill agents)
//   - nil (marker found) → latch true, return true (require edges, deny on absence)
//   - Any other error → return true (unknown state, fail closed — require edges)
func (a *AuthzService) backfillCompleted(ctx context.Context) bool {
	// Fast path: once latched true, never re-query.
	if a.backfillDone.Load() {
		return true
	}
	// Not yet latched — query the store.
	_, err := a.store.GetHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	if err == nil {
		a.backfillDone.Store(true)
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		// Genuinely pre-backfill. Do NOT cache — re-query next time.
		return false
	}
	// Store fault — fail closed (assume completed, require edges).
	a.logger.Error("backfillCompleted: store error, assuming completed (fail closed)", "error", err)
	return true
}

// filterEdgesByScope returns only the edges whose scope matches the given
// scope type and ID. This prevents a cross-scope authority leak where an
// edge in project P1 could satisfy a ceiling check for a request in P2.
func filterEdgesByScope(edges []*store.DelegationEdge, scopeType, scopeID string) []*store.DelegationEdge {
	var filtered []*store.DelegationEdge
	for _, e := range edges {
		if e.ScopeType == scopeType && e.ScopeID == scopeID {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// resourceProjectScope returns the project ID that a resource belongs to.
// For resources of type "project", the resource itself IS the project.
// For resources with ParentType="project", the parent is the project.
// Returns "" if no project scope can be determined.
func resourceProjectScope(r Resource) string {
	if r.Type == "project" && r.ID != "" {
		return r.ID
	}
	if r.ParentType == "project" && r.ParentID != "" {
		return r.ParentID
	}
	return ""
}

// getCachedDelegationEdges retrieves delegation edges with request-scoped caching.
func (a *AuthzService) getCachedDelegationEdges(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	cache := getDelegationCeilingCache(ctx)
	key := delegateType + ":" + delegateID

	if cache != nil {
		if edges, ok := cache.edges[key]; ok {
			return edges, nil
		}
	}

	edges, err := a.store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil {
		return nil, err
	}

	if cache != nil {
		cache.edges[key] = edges
	}
	return edges, nil
}

// getCachedEffectivePermissions retrieves effective permissions with request-scoped caching.
func (a *AuthzService) getCachedEffectivePermissions(ctx context.Context, principalType, principalID, scopeType, scopeID string) ([]string, error) {
	cache := getDelegationCeilingCache(ctx)
	key := principalType + ":" + principalID + ":" + scopeType + ":" + scopeID

	if cache != nil {
		if perms, ok := cache.perms[key]; ok {
			return perms, nil
		}
	}

	perms, err := a.getEffectivePermissions(ctx, principalType, principalID, scopeType, scopeID)
	if err != nil {
		return nil, err
	}

	if cache != nil {
		cache.perms[key] = perms
	}
	return perms, nil
}

// checkUserHoldsPermission checks if a user still holds a specific permission
// via their role bindings and policy grants.
func (a *AuthzService) checkUserHoldsPermission(
	ctx context.Context,
	userID, permissionID, scopeType, scopeID string,
	explain *[]DecisionStep,
) (bool, string, error) {
	// First check if the user is a super-admin (they hold all permissions).
	if a.IsSystemAdmin(ctx, userID) {
		return true, "super-admin", nil
	}

	// Check role bindings for the permission.
	perms, err := a.getCachedEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, scopeType, scopeID)
	if err != nil {
		return false, "", err
	}

	// Also include system-scoped permissions (they apply everywhere).
	if scopeType == store.RoleScopeProject {
		systemPerms, err := a.getCachedEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, store.RoleScopeSystem, "")
		if err == nil {
			perms = append(perms, systemPerms...)
		}
	}

	// Also check policy-granted permissions.
	user, err := a.store.GetUser(ctx, userID)
	if err != nil {
		// Propagate the exact error (including ErrNotFound) so the caller
		// can distinguish "definitely not found" from "store fault".
		return false, fmt.Sprintf("user %s lookup failed: %v", userID, err), err
	}
	// Check if user is still active.
	if user.Status != store.UserStatusActive {
		return false, fmt.Sprintf("user %s is %s", userID, user.Status), nil
	}

	// CO1: Policy-granted permissions removed. All authority now flows
	// through RoleBindings resolved above.

	for _, p := range perms {
		if p == permissionID {
			return true, "holds permission", nil
		}
	}

	return false, fmt.Sprintf("user lacks permission %s", permissionID), nil
}

// checkAgentHoldsPermission checks if an agent still holds a specific permission
// via its role and scope configuration.
func (a *AuthzService) checkAgentHoldsPermission(
	ctx context.Context,
	agentID, permissionID, scopeType, scopeID string,
	explain *[]DecisionStep,
) (bool, string, error) {
	agent, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		// Propagate the exact error (including ErrNotFound) so the caller
		// can distinguish "definitely not found" from "store fault".
		return false, fmt.Sprintf("agent %s lookup failed: %v", agentID, err), err
	}

	// Check agent's effective role scopes include the required permission.
	role, additionalScopes := agentRoleAndScopes(agent)
	scopes := append(ScopesForRole(role), additionalScopes...)

	// Map the permission ID to the agent token scope that would grant it.
	requiredScope := permissionToAgentScope(permissionID)
	if requiredScope == "" {
		// If no specific scope maps to this permission, check if the agent
		// has read-level access for read operations.
		for _, perm := range permissions.Registry {
			if perm.ID == permissionID {
				if perm.Action == "read" || perm.Action == "list" {
					// Agents have implicit read access for their project.
					if scopeType == store.RoleScopeProject && agent.ProjectID == scopeID {
						return true, "agent project read baseline", nil
					}
				}
				break
			}
		}
		return false, fmt.Sprintf("no agent scope maps to permission %s", permissionID), nil
	}

	for _, scope := range scopes {
		if scope == requiredScope {
			return true, "holds scope", nil
		}
	}

	return false, fmt.Sprintf("agent lacks scope %s for permission %s", requiredScope, permissionID), nil
}

// resolvePermissionID maps a resource and action to a canonical permission ID.
func resolvePermissionID(resource Resource, action Action) string {
	for _, perm := range permissions.Registry {
		if perm.Resource == resource.Type && perm.Action == string(action) {
			return perm.ID
		}
	}
	// Fallback: construct a permission ID from resource type and action.
	return resource.Type + "." + string(action)
}

// permissionToAgentScope maps a permission ID to the agent token scope
// that would grant it. Returns "" if no mapping exists.
func permissionToAgentScope(permissionID string) AgentTokenScope {
	for _, perm := range permissions.Registry {
		if perm.ID == permissionID && len(perm.AgentScopes) > 0 {
			// Return the first agent scope (they map to token scopes).
			return AgentTokenScope(perm.AgentScopes[0])
		}
	}
	return ""
}
