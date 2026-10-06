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
	// authority caches user delegator authority keyed by delegator,
	// resource, action, permission and edge scope.
	authority map[string]delegatorAuthorityResult
}

// delegatorAuthorityResult is one cached resolveUserDelegatorAuthority result.
type delegatorAuthorityResult struct {
	allowed bool
	reason  string
	err     error
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
		edges:     make(map[string][]*store.DelegationEdge),
		perms:     make(map[string][]string),
		authority: make(map[string]delegatorAuthorityResult),
	})
}

// isReadOnlyOperation reports whether action is a read-class action (read,
// list, verify). Any other action, including actions added later, is not
// read-class. It is one input to ceilingReadAllowance and never allows on
// its own.
func isReadOnlyOperation(action Action) bool {
	switch action {
	case ActionRead, ActionList, ActionVerify:
		return true
	default:
		return false
	}
}

// migrationDelegatorID is the typed user delegator recorded by the
// delegation-edge backfill for agents that predate delegation edges. It is
// the only delegator that is not a real principal; it is recognised by exact
// type and ID and never by a failed lookup.
const migrationDelegatorID = "system/migration"

// isMigrationSentinel reports whether edge was recorded by the delegation-edge
// backfill (exact user:system/migration delegator).
func isMigrationSentinel(edge *store.DelegationEdge) bool {
	return edge != nil && edge.DelegatorType == store.DelegationPrincipalUser && edge.DelegatorID == migrationDelegatorID
}

// sensitiveReadResourceTypes are resource types whose reads expose material
// (secret values, environment, injected material). Reads of these types never
// qualify for a read allowance in the delegation ceiling.
var sensitiveReadResourceTypes = map[string]bool{
	"secret":   true,
	"env":      true,
	"env_var":  true,
	"material": true,
}

// ceilingReadAllowance reports whether a request is a registered,
// non-sensitive read. Only such requests may run where the delegation ceiling
// grants a bounded read allowance (a missing edge after the backfill, or the
// migration sentinel at its frozen edge role). The action must be read, list
// or verify, the exact permission must be registered with a read-class
// action, and the resource type must not carry material.
func ceilingReadAllowance(resource Resource, action Action, permissionID string) bool {
	if !isReadOnlyOperation(action) {
		return false
	}
	if sensitiveReadResourceTypes[resource.Type] {
		return false
	}
	for _, perm := range permissions.Registry {
		if perm.ID == permissionID {
			switch perm.Action {
			case "read", "list", "verify":
				return !sensitiveReadResourceTypes[perm.Resource]
			default:
				return false
			}
		}
	}
	return false
}

// checkDelegationCeiling verifies that every live ancestor in the agent's
// delegation chain holds permissionID for the request's resource. Returns
// (allowed, reason, error). A non-nil error means an authorization lookup
// failed; the caller denies.
//
// Scope derivation: the ceiling scope is the PRINCIPAL's own project
// (AgentIdentity.ProjectID()). Delegation edges are created with the agent's
// project ID. A resource in a different project maps to that project's scope,
// where no edge of the agent matches, so the request is denied.
//
// The chain rules:
//   - Every link must name a live delegator. A user delegator must exist and be
//     active; an agent delegator must exist and not be deleted (a stopped agent
//     is live). A non-live delegator supplies no authority for any permission.
//   - The exact migration sentinel (user:system/migration) supplies a frozen
//     ceiling at the edge role for registered non-sensitive reads only.
//   - A user delegator holds the permission through super-admin, a role grant
//     in the edge scope or system scope, or a named relationship to this
//     resource evaluated through the common relationship stages.
//   - An agent delegator holds the permission through its stored role scopes,
//     and the walk continues to its own delegator.
//   - The walk is bounded by maxDelegationDepth and a repeated delegate denies.
//   - The Grandfathered flag is provenance metadata only.
//
// cause, when non-nil, receives a structural classification of the deny
// (see DenyCause) for the specific sub-cases callers need to distinguish.
// It is left at its zero value ("") for every other outcome, including
// allows and denials with no dedicated classification.
func (a *AuthzService) checkDelegationCeiling(
	ctx context.Context,
	req AuthzRequest,
	permissionID string,
	agentID string,
	explain *[]DecisionStep,
	cause *DenyCause,
) (bool, string, error) {
	return a.checkDelegationCeilingWithNote(ctx, req, permissionID, agentID, explain, cause, nil)
}

// checkDelegationCeilingWithNote is checkDelegationCeiling that also fills
// note, when non-nil, from an unrecorded-hop denial (see
// unrecordedHopNote). The note is an explicit out-parameter, so nothing is
// stored in the request context.
func (a *AuthzService) checkDelegationCeilingWithNote(
	ctx context.Context,
	req AuthzRequest,
	permissionID string,
	agentID string,
	explain *[]DecisionStep,
	cause *DenyCause,
	note *unrecordedHopNote,
) (bool, string, error) {
	// A hubDeliveryIdentity principal (ptone/scion#2228 part 2) never takes
	// the ordinary delegator-permission proof below: it is routed to its own
	// arm before the AgentIdentity assertion this function would otherwise
	// reach. A typed nil also matches this type assertion, which is exactly
	// why checkHubDeliveryCeiling's first check handles h == nil itself
	// rather than relying on a panic-free path through the rest of this
	// function.
	if h, ok := req.Principal.Identity.(*hubDeliveryIdentity); ok {
		return a.checkHubDeliveryCeiling(ctx, req, h, permissionID, agentID, explain, cause)
	}

	// scopeType is fixed to RoleScopeProject: delegation edges are
	// project-scoped.
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

	// A resource in another project is evaluated in that project's scope,
	// where the agent has no edge.
	resourceProjectID := resourceProjectScope(req.Resource)
	if resourceProjectID != "" && resourceProjectID != scopeID {
		scopeID = resourceProjectID
	}

	attested := req.Principal.Identity != nil && AncestryIsHubAttested(req.Principal.Identity)
	return a.walkDelegationChainWithNote(ctx, req.Resource, req.Action, permissionID, agentID, attested, scopeType, scopeID, explain, cause, note)
}

// maxDelegationDepth limits the delegation chain walk.
const maxDelegationDepth = 10

// walkDelegationChain walks the delegation chain upward from agentID and
// verifies that every delegator is live and holds permissionID for resource.
// attested states whether the starting agent's ancestry is hub-attested; it
// governs the pre-backfill allowance for a missing first edge. scopeType and
// scopeID define the delegation scope for every link.
func (a *AuthzService) walkDelegationChain(
	ctx context.Context,
	resource Resource,
	action Action,
	permissionID string,
	agentID string,
	attested bool,
	scopeType, scopeID string,
	explain *[]DecisionStep,
) (bool, string, error) {
	return a.walkDelegationChainWithCause(ctx, resource, action, permissionID, agentID, attested, scopeType, scopeID, explain, nil)
}

// walkDelegationChainWithCause is walkDelegationChain that also records a
// DenyCause when cause is non-nil: DenyCauseCeilingOrphaned for a delegator
// that does not resolve or is deleted and for a migration-provenance deny,
// and DenyCauseCeilingDelegatorLacksPermission for a delegator that resolves
// (including a user that is not active, for example suspended or invited)
// but does not hold the permission. After a hop's delegator-authority check
// passes, hopEffectCeilingDeny applies the hop's frozen provenance and
// effect ceiling and records ceiling_source_not_allowed, ceiling_unrecorded
// or ceiling_effect_exceeded. A lookup error is returned as an error and
// classified by the caller. Every other deny leaves cause unchanged.
func (a *AuthzService) walkDelegationChainWithCause(
	ctx context.Context,
	resource Resource,
	action Action,
	permissionID string,
	agentID string,
	attested bool,
	scopeType, scopeID string,
	explain *[]DecisionStep,
	cause *DenyCause,
) (bool, string, error) {
	return a.walkDelegationChainWithNote(ctx, resource, action, permissionID, agentID, attested, scopeType, scopeID, explain, cause, nil)
}

// walkDelegationChainWithNote is walkDelegationChainWithCause that also
// fills note, when non-nil, from a hop's ceiling_unrecorded denial (see
// logUnrecordedHop).
func (a *AuthzService) walkDelegationChainWithNote(
	ctx context.Context,
	resource Resource,
	action Action,
	permissionID string,
	agentID string,
	attested bool,
	scopeType, scopeID string,
	explain *[]DecisionStep,
	cause *DenyCause,
	note *unrecordedHopNote,
) (bool, string, error) {
	// The delegator side is evaluated for principals other than the
	// requester, so it must never read the requester's memoized principals
	// or access constraints. Masking here covers every caller; only
	// delegation edges, keyed by delegate, stay shared.
	ctx = maskAuthzInputs(ctx)
	addStep := func(step, detail string) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{Step: step, Detail: detail})
		}
	}
	setCause := func(c DenyCause) {
		if cause != nil {
			*cause = c
		}
	}

	if permissionID == "" {
		a.logger.Error("delegation ceiling evaluated without a permission; denying",
			"principal_id", agentID, "resource_type", resource.Type, "action", string(action))
		addStep("delegation_ceiling_no_permission", "no permission to evaluate")
		return false, "delegation ceiling: no permission to evaluate", nil
	}

	visited := make(map[string]bool, maxDelegationDepth+1)
	delegateID := agentID
	for depth := 0; ; depth++ {
		if depth > maxDelegationDepth {
			addStep("delegation_ceiling_depth", "delegation chain exceeded maximum depth")
			return false, "delegation chain exceeded maximum depth", nil
		}
		if visited[delegateID] {
			addStep("delegation_ceiling_cycle", fmt.Sprintf("delegate %s repeats in the chain", delegateID))
			return false, "delegation chain contains a cycle", nil
		}
		visited[delegateID] = true

		allEdges, err := a.getCachedDelegationEdges(ctx, store.DelegationPrincipalAgent, delegateID)
		if err != nil {
			addStep("delegation_ceiling_error", fmt.Sprintf("delegation edge lookup failed: %v", err))
			return false, "delegation ceiling check failed (fail-closed): " + err.Error(), err
		}

		// Only edges in the request scope count: an agent with authority in
		// project P1 does not satisfy the ceiling for a request in P2.
		edges := filterEdgesByScope(allEdges, scopeType, scopeID)
		var active []*store.DelegationEdge
		for _, e := range edges {
			if e.Active {
				active = append(active, e)
			}
		}

		if len(active) == 0 {
			// A chain with no edge has no recorded bound, so it is denied the
			// permissions such chains never held (legacyChainExcludedPermissions)
			// before either no-edge allowance below.
			if legacyChainExcludedPermissions[permissionID] {
				addStep("delegation_ceiling_no_edge_excluded",
					fmt.Sprintf("no delegation edge for agent:%s; %s requires a recorded delegation", delegateID, permissionID))
				setCause(DenyCauseCeilingUnrecorded)
				return false, fmt.Sprintf("no delegation edge for agent:%s: %s requires a recorded delegation", delegateID, permissionID), nil
			}
			if depth == 0 && attested {
				// Before the edge backfill runs, hub-attested agents may
				// have no edge yet. Once the backfill marker exists every
				// agent has an edge; a missing edge then permits only
				// registered non-sensitive reads (the project read
				// baseline) and denies everything else.
				if !a.backfillCompleted(ctx) {
					addStep("delegation_ceiling_pre_backfill",
						fmt.Sprintf("no delegation edge for local agent:%s; backfill not complete", delegateID))
					return true, "no delegation edge (pre-backfill)", nil
				}
				if ceilingReadAllowance(resource, action, permissionID) {
					addStep("delegation_ceiling_no_edge_read_allowed",
						fmt.Sprintf("no delegation edge for local agent:%s; registered non-sensitive read allowed", delegateID))
					return true, "no delegation edge (post-backfill, read-only allowed)", nil
				}
			}
			addStep("delegation_ceiling_no_edge",
				fmt.Sprintf("no delegation edge for agent:%s; no delegated authority", delegateID))
			return false, fmt.Sprintf("no delegation edge for agent:%s (no delegated authority)", delegateID), nil
		}

		// A partial unique index allows at most one active edge per
		// (delegate, scope). More than one is an invariant violation and
		// denies.
		if len(active) > 1 {
			a.logger.Error("Multiple active delegation edges found (invariant violation)",
				"principal_type", store.DelegationPrincipalAgent,
				"principal_id", delegateID,
				"active_count", len(active))
			addStep("delegation_ceiling_duplicate_edges",
				fmt.Sprintf("%d active edges for agent:%s", len(active), delegateID))
			return false, fmt.Sprintf("multiple active delegation edges for agent:%s (fail-closed on invariant violation)", delegateID), nil
		}

		edge := active[0]
		if edge.Grandfathered {
			addStep("delegation_ceiling_grandfathered_edge_provenance",
				fmt.Sprintf("edge %s has grandfathered provenance (audit only)", edge.ID))
		}

		if isMigrationSentinel(edge) {
			allowed, reason, err := a.migrationSentinelCeiling(resource, action, delegateID, edge, permissionID, explain)
			if !allowed && err == nil {
				setCause(DenyCauseCeilingOrphaned)
			}
			return allowed, reason, err
		}

		switch edge.DelegatorType {
		case store.DelegationPrincipalUser:
			allowed, reason, err := a.resolveUserDelegatorAuthority(ctx, edge.DelegatorID, resource, action, permissionID, edge.ScopeType, edge.ScopeID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					addStep("delegation_ceiling_delegator_not_live",
						fmt.Sprintf("delegator user %s does not exist", edge.DelegatorID))
					setCause(DenyCauseCeilingOrphaned)
					return false, fmt.Sprintf("delegator %s is not live", edge.DelegatorID), nil
				}
				addStep("delegation_ceiling_error", fmt.Sprintf("delegator user lookup failed: %v", err))
				return false, "delegation ceiling check failed (fail-closed): " + err.Error(), err
			}
			if !allowed {
				addStep("delegation_ceiling_denied",
					fmt.Sprintf("delegator user %s does not hold %s: %s", edge.DelegatorID, permissionID, reason))
				setCause(DenyCauseCeilingDelegatorLacksPermission)
				return false, fmt.Sprintf("delegator %s does not hold %s", edge.DelegatorID, permissionID), nil
			}
			if c, why := hopEffectCeilingDeny(edge, permissionID, resource, agentID, a.devLocalAuthorityEnabled()); c != "" {
				addStep("delegation_ceiling_effect_denied", fmt.Sprintf("edge %s: %s", edge.ID, why))
				a.logUnrecordedHop(note, c, edge, permissionID)
				setCause(c)
				return false, why, nil
			}
			addStep("delegation_ceiling_allowed",
				fmt.Sprintf("delegator user %s holds %s (%s)", edge.DelegatorID, permissionID, reason))
			return true, "delegation ceiling passed", nil

		case store.DelegationPrincipalAgent:
			allowed, reason, err := a.checkAgentHoldsPermission(ctx, edge.DelegatorID, permissionID, edge.ScopeType, edge.ScopeID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					addStep("delegation_ceiling_delegator_not_live",
						fmt.Sprintf("delegator agent %s does not exist", edge.DelegatorID))
					setCause(DenyCauseCeilingOrphaned)
					return false, fmt.Sprintf("delegator agent %s is not live", edge.DelegatorID), nil
				}
				addStep("delegation_ceiling_error", fmt.Sprintf("delegator agent lookup failed: %v", err))
				return false, "delegation ceiling check failed (fail-closed): " + err.Error(), err
			}
			if !allowed {
				addStep("delegation_ceiling_denied",
					fmt.Sprintf("delegator agent %s does not hold %s: %s", edge.DelegatorID, permissionID, reason))
				setCause(DenyCauseCeilingDelegatorLacksPermission)
				return false, fmt.Sprintf("delegator agent %s does not hold %s: %s", edge.DelegatorID, permissionID, reason), nil
			}
			if c, why := hopEffectCeilingDeny(edge, permissionID, resource, agentID, a.devLocalAuthorityEnabled()); c != "" {
				addStep("delegation_ceiling_effect_denied", fmt.Sprintf("edge %s: %s", edge.ID, why))
				a.logUnrecordedHop(note, c, edge, permissionID)
				setCause(c)
				return false, why, nil
			}
			addStep("delegation_ceiling_link_allowed",
				fmt.Sprintf("delegator agent %s holds %s; continuing", edge.DelegatorID, permissionID))
			delegateID = edge.DelegatorID

		default:
			addStep("delegation_ceiling_unknown_delegator",
				fmt.Sprintf("edge %s has delegator type %q", edge.ID, edge.DelegatorType))
			return false, fmt.Sprintf("delegation edge %s has an unsupported delegator type", edge.ID), nil
		}
	}
}

// unrecordedHopNote receives, from a chain walk, whether a
// ceiling_unrecorded denial came from a hop that delegation-provenance
// adoption can address: an unrecorded row (provenance version 0), denied a
// permission an adopted ceiling carries. A hop denied only because its
// provenance version is not understood is not adoptable. The note is
// descriptive: it selects response details and is never read by an
// authorization decision. It is passed as an explicit out-parameter and is
// never stored in a request context.
type unrecordedHopNote struct {
	adoptable bool
}

// logUnrecordedHop logs at Debug, server-side only, the delegate whose hop
// denied with ceiling_unrecorded, so an admin can correlate the denial with
// the delegation-adoption status view, and fills note when it is non-nil.
// Nothing here reaches the caller.
func (a *AuthzService) logUnrecordedHop(note *unrecordedHopNote, cause DenyCause, edge *store.DelegationEdge, permissionID string) {
	if cause != DenyCauseCeilingUnrecorded {
		return
	}
	unrecordedRow := edge.ProvenanceVersion == 0 && edge.Kind == store.EffectCeilingUnrecorded
	adoptable := unrecordedRow && adoptionCeilingCovers(permissionID)
	if note != nil {
		note.adoptable = adoptable
	}
	if a.logger == nil {
		return
	}
	if !unrecordedRow {
		a.logger.Debug("delegation ceiling: hop with an unsupported provenance version denied a permission that requires recorded provenance",
			"delegate_id", edge.DelegateID, "scope_id", edge.ScopeID, "permission", permissionID,
			"provenance_version", edge.ProvenanceVersion)
		return
	}
	if !adoptable {
		a.logger.Debug("delegation ceiling: unrecorded hop denied a permission no adopted ceiling carries",
			"delegate_id", edge.DelegateID, "scope_id", edge.ScopeID, "permission", permissionID)
		return
	}
	a.logger.Debug("delegation ceiling: unrecorded hop denied a permission that requires recorded provenance",
		"delegate_id", edge.DelegateID, "scope_id", edge.ScopeID, "permission", permissionID,
		"remediation_path", delegationAdoptionPath)
}

// adoptionCeilingIDs is the union of the compatibility policy V1 ceilings
// over every role, with the assigned-service-account additions.
var adoptionCeilingIDs = func() map[string]bool {
	ids := map[string]bool{}
	for _, role := range permissions.CompatibilityRoles() {
		row, _ := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, role, true)
		for _, id := range row {
			ids[id] = true
		}
	}
	return ids
}()

// adoptionCeilingCovers reports whether some compatibility policy V1 ceiling
// carries permissionID. A permission no adopted ceiling carries (for
// example one registered after V1) is not addressed by adoption, so its
// ceiling_unrecorded denial does not name adoption as the remedy.
func adoptionCeilingCovers(permissionID string) bool {
	return adoptionCeilingIDs[permissionID]
}

// hopEffectCeilingDeny applies a hop's frozen provenance and effect ceiling
// to permissionID, after the delegator-authority check has passed for the
// hop. It returns the empty cause when the hop passes. In order:
//   - a dev_local hop requires dev auth on this server and the delegator
//     user:DevUserID (the delegator-authority check has already denied a
//     missing or inactive delegator);
//   - a hop whose provenance version is not understood denies every
//     permission in recordedProvenanceRequired;
//   - a bounded hop denies a permission its ceiling does not allow, with the
//     self-operation exception only when the resource is the acting agent;
//   - an unrecorded hop denies every permission in
//     recordedProvenanceRequired or legacyChainExcludedPermissions and
//     passes the rest;
//   - a principal hop passes.
func hopEffectCeilingDeny(edge *store.DelegationEdge, permissionID string, resource Resource, actingAgentID string, devLocalEnabled bool) (DenyCause, string) {
	if hasDevLocalProvenance(edge) {
		if !devLocalEnabled {
			return DenyCauseCeilingSourceNotAllowed, "delegation ceiling: " + reasonDevLocalDisabled
		}
		if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID {
			return DenyCauseCeilingSourceNotAllowed, "delegation ceiling: local development provenance with another delegator"
		}
	}
	if !knownProvenanceVersion(edge.ProvenanceVersion) && recordedProvenanceRequired[permissionID] {
		return DenyCauseCeilingUnrecorded, fmt.Sprintf("delegation ceiling: %s requires recorded provenance", permissionID)
	}
	switch edge.Kind {
	case store.EffectCeilingPrincipal:
		return "", ""
	case store.EffectCeilingUnrecorded:
		if recordedProvenanceRequired[permissionID] || legacyChainExcludedPermissions[permissionID] {
			return DenyCauseCeilingUnrecorded, fmt.Sprintf("delegation ceiling: %s requires recorded provenance", permissionID)
		}
		return "", ""
	default:
		selfTarget := resource.Type == "agent" && resource.ID != "" && resource.ID == actingAgentID
		if !EffectCeilingAllows(edge.EffectCeiling, permissionID, selfTarget) {
			return DenyCauseCeilingEffectExceeded, fmt.Sprintf("delegation ceiling: %s is outside the frozen effect ceiling", permissionID)
		}
		return "", ""
	}
}

// migrationSentinelCeiling evaluates an edge recorded by the delegation-edge
// backfill. The sentinel is not a principal and supplies no live authority:
// only registered non-sensitive reads run, bounded by the edge role frozen at
// backfill time. Every other permission (sensitive reads, attach, ports,
// messages, create, lifecycle, writes, unmapped permissions) denies.
func (a *AuthzService) migrationSentinelCeiling(
	resource Resource,
	action Action,
	agentID string,
	edge *store.DelegationEdge,
	permissionID string,
	explain *[]DecisionStep,
) (bool, string, error) {
	addStep := func(step, detail string) {
		if explain != nil {
			*explain = append(*explain, DecisionStep{Step: step, Detail: detail})
		}
	}

	if !ceilingReadAllowance(resource, action, permissionID) {
		addStep("delegation_ceiling_migration_deny",
			fmt.Sprintf("migration-provenance edge for agent %s; %s is not a registered non-sensitive read", agentID, permissionID))
		return false, fmt.Sprintf("migration-provenance delegation permits registered non-sensitive reads only; %s denied", permissionID), nil
	}

	requiredScope := permissionToAgentScope(permissionID)
	if requiredScope == "" {
		addStep("delegation_ceiling_migration_allow_read",
			fmt.Sprintf("migration-provenance edge for agent %s; read allowed at frozen ceiling (role=%s)", agentID, edge.Role))
		return true, "migration-provenance delegation: read allowed at frozen ceiling", nil
	}
	for _, scope := range ScopesForRole(AgentRole(edge.Role)) {
		// The backfilled role predates the ceiling-optional role scopes, so
		// they are never covered here (legacyChainExcludedPermissions).
		if ceilingOptionalRoleScopes[scope] {
			continue
		}
		if scope == requiredScope {
			addStep("delegation_ceiling_migration_allow_scope",
				fmt.Sprintf("migration-provenance edge for agent %s; scope %s covered by frozen ceiling (role=%s)", agentID, requiredScope, edge.Role))
			return true, "migration-provenance delegation: scope covered at frozen ceiling", nil
		}
	}
	addStep("delegation_ceiling_migration_deny_scope",
		fmt.Sprintf("migration-provenance edge for agent %s; scope %s not in frozen ceiling (role=%s)", agentID, requiredScope, edge.Role))
	return false, fmt.Sprintf("migration-provenance delegation: scope %s exceeds frozen ceiling (role=%s)", requiredScope, edge.Role), nil
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

// getCachedDelegationEdges retrieves delegation edges with request-scoped
// caching. It consults, in order: the existing per-decision
// delegationCeilingCache (unchanged); then the phase-wide edges memo (see
// authz_request_inputs.go), whose key is untouched by maskAuthzInputs, so
// this is the one input the delegation ceiling shares across decisions in a
// phase; then the store. A store error is returned directly and is never
// stored in either cache.
func (a *AuthzService) getCachedDelegationEdges(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	cache := getDelegationCeilingCache(ctx)
	key := delegateType + ":" + delegateID

	if cache != nil {
		if edges, ok := cache.edges[key]; ok {
			return edges, nil
		}
	}

	// Phase-wide edges memo: bypassed entirely on a done ctx, so a
	// cancelled request falls straight through to the store call below
	// exactly as it does with no memo installed. Success only — an
	// edge-load error is returned directly and never stored, so it can
	// never manufacture an error the store did not itself produce.
	if memo := delegationEdgesMemoFromContext(ctx); memo != nil && ctx.Err() == nil {
		memo.mu.Lock()
		if edges, ok := memo.edges[key]; ok {
			memo.mu.Unlock()
			if cache != nil {
				cache.edges[key] = edges
			}
			return edges, nil
		}
		memo.mu.Unlock()

		edges, err := a.store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
		if err != nil {
			return nil, err
		}

		memo.mu.Lock()
		if existing, ok := memo.edges[key]; ok {
			edges = existing
		} else {
			memo.edges[key] = edges
		}
		memo.mu.Unlock()

		if cache != nil {
			cache.edges[key] = edges
		}
		return edges, nil
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

// resolveUserDelegatorAuthority reports whether a user delegator is live and
// holds permissionID for resource. It never evaluates a delegation ceiling.
//
// Order: the user must exist (ErrNotFound is returned so the caller records a
// non-live link) and be active before any grant is considered, so a deleted or
// suspended user supplies no authority, including through a super-admin
// binding that outlives the account. A live user then holds the permission
// through super-admin, a role grant in the edge scope or system scope, or a
// named relationship to this resource.
//
// The relationship path applies the delegator's live access constraints and
// the common relationship stages. The credential the delegator used when the
// edge was created is not recorded on the edge; its absence is not read as
// unrestricted authority, and every restriction that can be evaluated live is
// applied.
func (a *AuthzService) resolveUserDelegatorAuthority(
	ctx context.Context,
	userID string,
	resource Resource,
	action Action,
	permissionID, scopeType, scopeID string,
) (bool, string, error) {
	cache := getDelegationCeilingCache(ctx)
	key := userID + "|" + resource.Type + "|" + resource.ID + "|" + resource.ParentType + "|" + resource.ParentID + "|" + string(action) + "|" + permissionID + "|" + scopeType + "|" + scopeID
	if cache != nil {
		if r, ok := cache.authority[key]; ok {
			return r.allowed, r.reason, r.err
		}
	}
	allowed, reason, err := a.evaluateUserDelegatorAuthority(ctx, userID, resource, action, permissionID, scopeType, scopeID)
	if cache != nil {
		cache.authority[key] = delegatorAuthorityResult{allowed: allowed, reason: reason, err: err}
	}
	return allowed, reason, err
}

func (a *AuthzService) evaluateUserDelegatorAuthority(
	ctx context.Context,
	userID string,
	resource Resource,
	action Action,
	permissionID, scopeType, scopeID string,
) (bool, string, error) {
	user, err := a.store.GetUser(ctx, userID)
	if err != nil {
		return false, fmt.Sprintf("user %s lookup failed: %v", userID, err), err
	}
	if user == nil {
		return false, fmt.Sprintf("user %s not found", userID), store.ErrNotFound
	}
	if user.Status != store.UserStatusActive {
		return false, fmt.Sprintf("user %s is %s", userID, user.Status), nil
	}

	if a.IsSystemAdmin(ctx, userID) {
		return true, "super-admin", nil
	}

	perms, err := a.getCachedEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, scopeType, scopeID)
	if err != nil {
		return false, "", err
	}
	if scopeType == store.RoleScopeProject {
		systemPerms, err := a.getCachedEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, store.RoleScopeSystem, "")
		if err != nil {
			return false, "", err
		}
		// perms is returned directly from the request-scoped cache
		// (getCachedEffectivePermissions), so it must not be appended to
		// in place: doing so can write into the cached slice's backing
		// array and corrupt the cache entry for later lookups. Build a
		// new slice instead.
		combined := make([]string, 0, len(perms)+len(systemPerms))
		combined = append(combined, perms...)
		combined = append(combined, systemPerms...)
		perms = combined
	}
	for _, p := range perms {
		if p == permissionID {
			return true, "role grant", nil
		}
	}

	return a.userRelationshipAuthority(ctx, user, resource, action, permissionID)
}

// userRelationshipAuthority evaluates the named relationship grants of a live
// user delegator on resource through the common relationship stages, with the
// user's access constraints as the restriction set.
func (a *AuthzService) userRelationshipAuthority(
	ctx context.Context,
	user *store.User,
	resource Resource,
	action Action,
	permissionID string,
) (bool, string, error) {
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "")
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: user.ID, Identity: identity}

	principals, err := a.authorizationPrincipals(ctx, identity)
	if err != nil {
		return false, "", err
	}
	closure := make(map[string]struct{}, len(principals))
	for _, p := range principals {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	restrictions := a.loadAccessConstraintRestrictions(ctx, closure, ResourceContext{
		ResourceType: resource.Type,
		ResourceID:   resource.ID,
		OwnerID:      resource.OwnerID,
		ProjectID:    projectIDForResource(resource),
		Ancestry:     resource.Ancestry,
	})

	out := a.evaluateRelationshipCandidates(ctx, principal, resource, action, permissionID, restrictions, true, nil)
	if out.accepted != nil {
		return true, "relationship grant: " + out.accepted.MatchedGrant, nil
	}
	if out.restrictedBy != "" {
		return false, "relationship grant restricted by " + out.restrictedBy, nil
	}
	return false, fmt.Sprintf("user lacks permission %s", permissionID), nil
}

// checkAgentHoldsPermission reports whether an agent delegator is live and
// holds permissionID through its stored role scopes. A missing or deleted
// agent returns store.ErrNotFound so the caller records a non-live link. A
// stopped agent is live.
func (a *AuthzService) checkAgentHoldsPermission(
	ctx context.Context,
	agentID, permissionID, scopeType, scopeID string,
) (bool, string, error) {
	agent, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		return false, fmt.Sprintf("agent %s lookup failed: %v", agentID, err), err
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return false, fmt.Sprintf("agent %s is deleted", agentID), store.ErrNotFound
	}

	role, additionalScopes := agentRoleAndScopes(agent)
	scopes := append(ScopesForRole(role), additionalScopes...)

	requiredScope := permissionToAgentScope(permissionID)
	if requiredScope == "" {
		// No agent scope maps to this permission: only the project read
		// baseline applies, for registered read/list permissions in the
		// agent's own project.
		for _, perm := range permissions.Registry {
			if perm.ID == permissionID {
				if perm.Action == "read" || perm.Action == "list" {
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
