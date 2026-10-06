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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the effect-ceiling rules for delegation edges: the ceiling
// and provenance frozen when an agent is created, and the way the frozen
// ceilings of an agent's chain bound the scopes minted into its token and the
// permissions the step-10 walk allows.
//
// A ceiling has three kinds (store.EffectCeilingKind):
//   - bounded: PermissionIDs is the complete allow-list, read under Version;
//   - principal: no credential caveat; the live principal is the only bound;
//   - unrecorded: provenance was not recorded (the zero value). It never
//     carries IDs and is never read as principal.

// Structural provenance outcomes. Each one denies; none is a lookup fault.
var (
	// ErrProvenanceMissing: the agent has no active project-scoped edge and
	// is not exempt under the pre-backfill rule.
	ErrProvenanceMissing = errors.New("provenance: no active project-scoped edge")
	// ErrProvenanceAmbiguous: the agent has more than one active
	// project-scoped edge.
	ErrProvenanceAmbiguous = errors.New("provenance: more than one active project-scoped edge")
	// ErrProvenanceChain: the chain is invalid (depth, cycle, an unsupported
	// delegator type or ceiling kind, or a deleted source agent).
	ErrProvenanceChain = errors.New("provenance: chain invalid")
)

var (
	// errSourceNotAllowed: the source credential is not accepted as an
	// authority source on this server (an identity kind that cannot create
	// authority, or a dev_local hop that fails a local-development check).
	errSourceNotAllowed = errors.New("provenance: source credential not accepted on this server")
	// errSourceCeilingUnrecorded: the source credential's ceiling version is
	// not one FrozenPermissionCeiling.Allows interprets.
	errSourceCeilingUnrecorded = errors.New("provenance: source ceiling version not interpreted")
)

// Neutral reasons returned with ceiling denials.
const (
	reasonDevLocalDisabled  = "local development authority is not enabled on this server"
	reasonPrincipalInactive = "principal inactive"
	reasonNoUsableRole      = "the token's scopes fit no usable agent role; add the selectors for readonly, request role=none explicitly, or create from a session"
)

// selfOperationPermissionIDs is the reviewed list of agent permissions that
// apply only to the bearer itself. Under a bounded ceiling these are allowed
// when the target is the acting agent.
var selfOperationPermissionIDs = []string{
	"agent.status_update",
	"agent.log_append",
	"agent.notify",
	"agent.token_refresh",
	"agent.port_forward",
}

// nonSelfOperationPermissionIDs lists agent-scoped permissions without a UAT
// selector that are deliberately not self operations. A bounded ceiling
// allows them only by listing them.
var nonSelfOperationPermissionIDs = []string{
	"project.secret_read",
	"secret.use",
	"agent.identity_token",
	"agent.set_message_mode",
}

var selfOperationSet = toPermissionSet(selfOperationPermissionIDs)

// recordedProvenanceRequiredIDs are the permissions whose effect is issuing,
// using or delivering sensitive material. An unrecorded hop never allows
// them.
var recordedProvenanceRequiredIDs = []string{
	"gcp_service_account.use",
	"gcp_service_account.assign",
	"project.secret_read",
	"secret.use",
	"secret.deliver",
	"env_var.deliver",
	"skill_injection.deliver",
	"agent.identity_token",
}

var recordedProvenanceRequired = toPermissionSet(recordedProvenanceRequiredIDs)

// legacyChainExcludedPermissions are the permissions covered by the
// ceilingOptionalRoleScopes (the artifact permissions). A chain with no
// recorded bound (an unrecorded ceiling or a migration-sentinel edge) never
// held them, so it is not issued those scopes and is denied these
// permissions at use. Principal chains are unaffected: their authority is
// the live user.
var legacyChainExcludedPermissions = toPermissionSet(agentScopeCoverage(sortedOptionalRoleScopes()))

// sortedOptionalRoleScopes returns the keys of ceilingOptionalRoleScopes in
// sorted order.
func sortedOptionalRoleScopes() []AgentTokenScope {
	out := make([]AgentTokenScope, 0, len(ceilingOptionalRoleScopes))
	for s := range ceilingOptionalRoleScopes {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// hubDeliveryPermissionList is the fixed set of delivery permissions an
// agent-created edge may carry through parentDeliverEligibility, in sorted
// order. It is derived from hubDeliveryPermissionIDs
// (authz_delivery_credential.go) so the two cannot drift.
var hubDeliveryPermissionList = sortedPermissionKeys(hubDeliveryPermissionIDs)

var hubDeliveryPermissionSet = toPermissionSet(hubDeliveryPermissionList)

// sortedPermissionKeys returns the keys of set in sorted order.
func sortedPermissionKeys(set map[string]struct{}) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return sortedUniqueIDs(ids)
}

// zeroCoverageScopeMapping is the reviewed table for agent scopes that cover
// no registry permission. Under a bounded ceiling such a scope is issued only
// through an entry here, and only when the ceiling allows the mapped
// permission. The GCP token scope maps to gcp_service_account.assign, the
// parent permission of gcp_service_account.use.
var zeroCoverageScopeMapping = map[string]string{ScopeGCPTokenPrefix: "gcp_service_account.assign"}

func toPermissionSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// sortedUniqueIDs returns ids sorted and de-duplicated, never nil.
func sortedUniqueIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// knownCeilingVersion reports whether FrozenPermissionCeiling.Allows
// interprets v.
func knownCeilingVersion(v permissions.CeilingVersion) bool {
	switch v {
	case permissions.CeilingVersionUnspecified, permissions.CeilingVersionV1:
		return true
	default:
		return false
	}
}

// knownProvenanceVersion reports whether this binary understands provenance
// version v. Exactly version 1 is understood.
func knownProvenanceVersion(v int) bool {
	return v == store.ProvenanceVersionV1
}

// hasDevLocalProvenance reports whether edge was recorded from the
// recognized local development user.
func hasDevLocalProvenance(edge *store.DelegationEdge) bool {
	return edge != nil && edge.SourceCredentialKind == store.SourceCredentialDevLocal
}

// EffectCeilingAllows is the one predicate for "does this frozen ceiling
// allow this permission". selfTarget is true when the resource is the bearer
// agent itself.
//   - principal: allows every permission;
//   - unrecorded: denies every permission in recordedProvenanceRequired or
//     legacyChainExcludedPermissions and applies the frozen legacy
//     characterization to the rest;
//   - bounded: FrozenPermissionCeiling.Allows under the recorded Version, or
//     a self operation on the bearer itself;
//   - any other kind: denies.
func EffectCeilingAllows(c store.EffectCeiling, permissionID string, selfTarget bool) bool {
	if permissionID == "" {
		return false
	}
	switch c.Kind {
	case store.EffectCeilingPrincipal:
		return true
	case store.EffectCeilingUnrecorded:
		return !recordedProvenanceRequired[permissionID] && !legacyChainExcludedPermissions[permissionID]
	case store.EffectCeilingBounded:
		frozen, ok := c.Frozen()
		if !ok {
			return false
		}
		if frozen.Allows(permissionID) {
			return true
		}
		return selfTarget && selfOperationSet[permissionID]
	default:
		return false
	}
}

// agentScopeCoverage returns the registry permission IDs whose AgentScopes
// are covered by scopes: a permission is included when any of its
// AgentScopes is in scopes. It reads permissions.Registry, never edits it,
// and never adds permissions from zeroCoverageScopeMapping.
func agentScopeCoverage(scopes []AgentTokenScope) []string {
	want := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		want[string(s)] = true
	}
	var ids []string
	for _, perm := range permissions.Registry {
		for _, as := range perm.AgentScopes {
			if want[as] {
				ids = append(ids, perm.ID)
				break
			}
		}
	}
	return sortedUniqueIDs(ids)
}

// zeroCoverageMappedPermission returns the reviewed permission for a scope
// with zero registry coverage, matched by prefix.
func zeroCoverageMappedPermission(scope AgentTokenScope) (string, bool) {
	s := string(scope)
	for prefix, perm := range zeroCoverageScopeMapping {
		if strings.HasPrefix(s, prefix) && len(s) > len(prefix) {
			return perm, true
		}
	}
	return "", false
}

// ceilingAllowsScope reports whether scope may be issued under c:
//   - principal → true;
//   - unrecorded → true except for the ceilingOptionalRoleScopes (frozen
//     legacy characterization; see legacyChainExcludedPermissions);
//   - bounded → with zero registry coverage, the scope needs a
//     zeroCoverageScopeMapping entry whose permission c allows; otherwise
//     every covered permission is allowed by c or is a self operation;
//   - any other kind → false.
func ceilingAllowsScope(c store.EffectCeiling, scope AgentTokenScope) bool {
	switch c.Kind {
	case store.EffectCeilingPrincipal:
		return true
	case store.EffectCeilingUnrecorded:
		return !ceilingOptionalRoleScopes[scope]
	case store.EffectCeilingBounded:
	default:
		return false
	}
	frozen, ok := c.Frozen()
	if !ok {
		return false
	}
	coverage := agentScopeCoverage([]AgentTokenScope{scope})
	if len(coverage) == 0 {
		perm, mapped := zeroCoverageMappedPermission(scope)
		return mapped && frozen.Allows(perm)
	}
	for _, perm := range coverage {
		if !frozen.Allows(perm) && !selfOperationSet[perm] {
			return false
		}
	}
	return true
}

// ceilingOptionalRoleScopes are role scopes that do not decide whether a
// role fits a ceiling. A child whose ceiling does not allow one still gets
// the role; the mint filter (ceilingAllowsScope at issue time) leaves the
// scope out of its tokens. The artifact scopes joined the agent roles after
// UAT selector sets were in use, so making them optional keeps every token
// that fit a role before still fitting it, while a child only uses the
// artifact service when its source could. project:artifact:write is listed
// although no role carries it yet, so adding it to a role later needs no
// change here.
var ceilingOptionalRoleScopes = map[AgentTokenScope]bool{
	ScopeProjectArtifactRead:  true,
	ScopeProjectArtifactWrite: true,
}

// roleFitsCeiling reports whether every scope of role, other than the
// ceilingOptionalRoleScopes, passes ceilingAllowsScope under c.
func roleFitsCeiling(c store.EffectCeiling, role AgentRole) bool {
	for _, scope := range ScopesForRole(role) {
		if ceilingOptionalRoleScopes[scope] {
			continue
		}
		if !ceilingAllowsScope(c, scope) {
			return false
		}
	}
	return true
}

// childRoleWithinCeiling decides the child's stored role under the source's
// frozen ceiling. roleExplicit is true when the request named a role.
//   - an explicit role=none is always allowed;
//   - an explicit role that does not fit denies with ceiling_effect_exceeded;
//   - a defaulted role becomes the lower of role (the pre-cap role) and the
//     highest of full > baseline > readonly that fits. A defaulted pre-cap
//     role of none is kept as is (no cap applies to it). When no role above
//     none fits, it denies with ceiling_effect_exceeded; a defaulted role is
//     never capped to none.
func childRoleWithinCeiling(c store.EffectCeiling, role AgentRole, roleExplicit bool) (AgentRole, DenyCause, bool) {
	if roleExplicit {
		if role == AgentRoleNone || roleFitsCeiling(c, role) {
			return role, "", true
		}
		return "", DenyCauseCeilingEffectExceeded, false
	}
	if role == AgentRoleNone {
		return role, "", true
	}
	for _, candidate := range []AgentRole{AgentRoleFull, AgentRoleBaseline, AgentRoleReadOnly} {
		if roleFitsCeiling(c, candidate) {
			return minRole(role, candidate), "", true
		}
	}
	return "", DenyCauseCeilingEffectExceeded, false
}

// childEffectCeiling returns the ceiling for a new agent created under an
// agent parent whose own edge ceiling is source: bounded, Version V1, with
// IDs = (source bounded: the coverage IDs source allows; otherwise the
// coverage alone) ∪ parentDeliver. parentDeliver is restricted to
// hubDeliveryPermissionIDs.
func childEffectCeiling(source store.EffectCeiling, parentCoverage []string, parentDeliver []string) store.EffectCeiling {
	var ids []string
	if source.Kind == store.EffectCeilingBounded {
		frozen, ok := source.Frozen()
		for _, p := range parentCoverage {
			if ok && frozen.Allows(p) {
				ids = append(ids, p)
			}
		}
	} else {
		ids = append(ids, parentCoverage...)
	}
	for _, d := range parentDeliver {
		if hubDeliveryPermissionSet[d] {
			ids = append(ids, d)
		}
	}
	return store.EffectCeiling{
		Kind:          store.EffectCeilingBounded,
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: sortedUniqueIDs(ids),
	}
}

// ChainCeiling is the fold of every hop's frozen ceiling on an agent's chain.
type ChainCeiling struct {
	Ceiling        store.EffectCeiling // the fold
	UnrecordedHops int                 // number of unrecorded hops on the chain
}

// activeProjectEdges returns the active edges of agentID in the project scope
// projectID, read from the store.
func (a *AuthzService) activeProjectEdges(ctx context.Context, agentID, projectID string) ([]*store.DelegationEdge, error) {
	all, err := a.store.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID)
	if err != nil {
		return nil, fmt.Errorf("delegation edge lookup for agent %s: %w", agentID, err)
	}
	var active []*store.DelegationEdge
	for _, e := range filterEdgesByScope(all, store.RoleScopeProject, projectID) {
		if e.Active {
			active = append(active, e)
		}
	}
	return active, nil
}

// devLocalHopUsable applies both local-development checks to a hop with
// dev_local provenance: dev-auth is enabled on this server, and the hop's
// delegator is user:DevUserID and that user is active. devUserActive caches
// the GetUser result across hops of one fold. A failed check returns
// errSourceNotAllowed; a GetUser fault returns a wrapped lookup error.
func (a *AuthzService) devLocalHopUsable(ctx context.Context, edge *store.DelegationEdge, devUserActive *bool) error {
	if !a.devLocalAuthorityEnabled() {
		return fmt.Errorf("%w: %s", errSourceNotAllowed, reasonDevLocalDisabled)
	}
	if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID {
		return fmt.Errorf("%w: %s", errSourceNotAllowed, reasonPrincipalInactive)
	}
	if devUserActive != nil && *devUserActive {
		return nil
	}
	user, err := a.store.GetUser(ctx, DevUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", errSourceNotAllowed, reasonPrincipalInactive)
		}
		return fmt.Errorf("local development user lookup: %w", err)
	}
	if user == nil || user.Status != store.UserStatusActive {
		return fmt.Errorf("%w: %s", errSourceNotAllowed, reasonPrincipalInactive)
	}
	if devUserActive != nil {
		*devUserActive = true
	}
	return nil
}

// chainEffectCeiling walks the agent's active project-scoped edges upward
// (depth ≤ maxDelegationDepth) and folds every hop's frozen ceiling:
//   - bounded hops intersect over the registry: the result holds each
//     registry permission that every bounded hop's own Allows admits;
//   - principal is the identity element;
//   - an unrecorded hop adds no narrowing and is counted in UnrecordedHops;
//   - a hop whose ProvenanceVersion fails knownProvenanceVersion is counted
//     as unrecorded too, and a bounded ceiling on it intersects as usual.
//
// Result kind: bounded if any hop is bounded; otherwise unrecorded if any hop
// is unrecorded; otherwise principal.
//
// Each hop with dev_local provenance gets both local-development checks.
// The agent's own missing edge (depth 0) while the edge backfill is not
// complete is exempt and folds as one unrecorded hop; the migration sentinel
// edge folds as an unrecorded terminal hop.
//
// Errors: ErrProvenanceMissing, ErrProvenanceAmbiguous, ErrProvenanceChain,
// errSourceNotAllowed, and wrapped lookup errors.
func (a *AuthzService) chainEffectCeiling(ctx context.Context, agent *store.Agent) (ChainCeiling, error) {
	if agent == nil {
		return ChainCeiling{}, fmt.Errorf("%w: no agent", ErrProvenanceChain)
	}
	var (
		bounded       []permissions.FrozenPermissionCeiling
		unrecorded    int
		devUserActive bool
		visited       = make(map[string]bool, maxDelegationDepth+1)
		delegateID    = agent.ID
	)
walk:
	for depth := 0; ; depth++ {
		if depth > maxDelegationDepth {
			return ChainCeiling{}, fmt.Errorf("%w: maximum depth exceeded", ErrProvenanceChain)
		}
		if visited[delegateID] {
			return ChainCeiling{}, fmt.Errorf("%w: agent %s repeats in the chain", ErrProvenanceChain, delegateID)
		}
		visited[delegateID] = true

		active, err := a.activeProjectEdges(ctx, delegateID, agent.ProjectID)
		if err != nil {
			return ChainCeiling{}, err
		}
		if len(active) == 0 {
			if depth == 0 && !a.backfillCompleted(ctx) {
				unrecorded++
				break walk
			}
			return ChainCeiling{}, fmt.Errorf("%w: agent %s", ErrProvenanceMissing, delegateID)
		}
		if len(active) > 1 {
			return ChainCeiling{}, fmt.Errorf("%w: agent %s", ErrProvenanceAmbiguous, delegateID)
		}
		edge := active[0]
		if isMigrationSentinel(edge) {
			unrecorded++
			break walk
		}

		versionKnown := knownProvenanceVersion(edge.ProvenanceVersion)
		switch edge.Kind {
		case store.EffectCeilingBounded:
			frozen, _ := edge.Frozen()
			bounded = append(bounded, frozen)
			if !versionKnown {
				unrecorded++
			}
		case store.EffectCeilingPrincipal:
			if !versionKnown {
				unrecorded++
			}
		case store.EffectCeilingUnrecorded:
			unrecorded++
		default:
			return ChainCeiling{}, fmt.Errorf("%w: edge %s has ceiling kind %q", ErrProvenanceChain, edge.ID, edge.Kind)
		}

		if hasDevLocalProvenance(edge) {
			if err := a.devLocalHopUsable(ctx, edge, &devUserActive); err != nil {
				return ChainCeiling{}, err
			}
		}

		switch edge.DelegatorType {
		case store.DelegationPrincipalUser:
			break walk
		case store.DelegationPrincipalAgent:
			delegateID = edge.DelegatorID
		default:
			return ChainCeiling{}, fmt.Errorf("%w: edge %s has delegator type %q", ErrProvenanceChain, edge.ID, edge.DelegatorType)
		}
	}

	return ChainCeiling{Ceiling: foldCeilings(bounded, unrecorded), UnrecordedHops: unrecorded}, nil
}

// foldCeilings returns the fold of the bounded hops' ceilings: bounded V1
// over the registry permissions every bounded hop allows when there is any
// bounded hop; otherwise unrecorded when unrecordedHops > 0; otherwise
// principal.
func foldCeilings(bounded []permissions.FrozenPermissionCeiling, unrecordedHops int) store.EffectCeiling {
	if len(bounded) == 0 {
		if unrecordedHops > 0 {
			return store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
		}
		return store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	}
	ids := []string{}
	for _, perm := range permissions.Registry {
		allowed := true
		for _, f := range bounded {
			if !f.Allows(perm.ID) {
				allowed = false
				break
			}
		}
		if allowed {
			ids = append(ids, perm.ID)
		}
	}
	return store.EffectCeiling{
		Kind:          store.EffectCeilingBounded,
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: sortedUniqueIDs(ids),
	}
}

// ScopeCeilings is the single input set for every agent-token scope filter.
// Mint and refresh obtain it from loadScopeCeilings and pass it to
// filterScopes; no other code filters an agent's scope list by a ceiling.
type ScopeCeilings struct {
	Chain ChainCeiling // chainEffectCeiling(agent)
}

// loadScopeCeilings loads the scope-filter inputs. It performs no filtering.
// Errors: those of chainEffectCeiling.
func (a *AuthzService) loadScopeCeilings(ctx context.Context, agent *store.Agent) (ScopeCeilings, error) {
	chain, err := a.chainEffectCeiling(ctx, agent)
	if err != nil {
		return ScopeCeilings{}, err
	}
	return ScopeCeilings{Chain: chain}, nil
}

// filterScopes is the one pure scope filter. It returns the candidates, in
// order, less every scope that fails ceilingAllowsScope(chain, scope). chain
// is passed separately from sc so a caller can pass a projected chain
// ceiling; filterScopes never reads sc.Chain.
func filterScopes(candidates []AgentTokenScope, chain store.EffectCeiling, sc ScopeCeilings) []AgentTokenScope {
	_ = sc
	out := make([]AgentTokenScope, 0, len(candidates))
	for _, scope := range candidates {
		if ceilingAllowsScope(chain, scope) {
			out = append(out, scope)
		}
	}
	return out
}

// mintCandidateScopes returns the scopes a mint starts from: the role scopes
// plus the config-derived scopes from agentRoleAndScopes, de-duplicated.
// When mintDevAuthOverride is set, a role below full (including none) is
// raised to full before its scopes are taken.
func (a *AuthzService) mintCandidateScopes(agent *store.Agent) []AgentTokenScope {
	role, additional := agentRoleAndScopes(agent)
	if a != nil && a.mintDevAuthOverride && CompareRoles(role, AgentRoleFull) < 0 {
		role = AgentRoleFull
	}
	scopes := ScopesForRole(role)
	seen := make(map[AgentTokenScope]bool, len(scopes)+len(additional))
	for _, s := range scopes {
		seen[s] = true
	}
	for _, s := range additional {
		if !seen[s] {
			scopes = append(scopes, s)
			seen[s] = true
		}
	}
	return scopes
}

// ceilingFilteredAgentScopes returns the agent-token scopes a mint or refresh
// may issue for agent, given the candidate scopes:
//
//	sc, err := a.loadScopeCeilings(ctx, agent)
//	return filterScopes(candidates, sc.Chain.Ceiling, sc), err
//
// Errors: those of chainEffectCeiling.
func (a *AuthzService) ceilingFilteredAgentScopes(ctx context.Context, agent *store.Agent, candidates []AgentTokenScope) ([]AgentTokenScope, error) {
	sc, err := a.loadScopeCeilings(ctx, agent)
	if err != nil {
		return nil, err
	}
	return filterScopes(candidates, sc.Chain.Ceiling, sc), nil
}

// deliverIDsAllowedByChain is the pure step of parentDeliverEligibility: the
// delivery permissions the chain allows, exactly when the chain has no
// unrecorded hop and its ceiling is not unrecorded. No self-operation
// exception applies.
func deliverIDsAllowedByChain(chain ChainCeiling) []string {
	if chain.UnrecordedHops != 0 || chain.Ceiling.Kind == store.EffectCeilingUnrecorded {
		return nil
	}
	var ids []string
	for _, d := range hubDeliveryPermissionList {
		if EffectCeilingAllows(chain.Ceiling, d, false) {
			ids = append(ids, d)
		}
	}
	return ids
}

// isStructuralProvenanceError reports whether err is a structural chain
// outcome (missing, ambiguous or invalid chain, or a source that is not
// accepted) rather than a lookup fault.
func isStructuralProvenanceError(err error) bool {
	return errors.Is(err, ErrProvenanceMissing) ||
		errors.Is(err, ErrProvenanceAmbiguous) ||
		errors.Is(err, ErrProvenanceChain) ||
		errors.Is(err, errSourceNotAllowed) ||
		errors.Is(err, errSourceCeilingUnrecorded)
}

// parentDeliverEligibility returns the delivery permissions an edge created
// by parent agent P may carry: each of hubDeliveryPermissionList that P's
// whole chain allows (deliverIDsAllowedByChain over chainEffectCeiling(P)).
// A structural chain outcome yields (nil, nil); a lookup fault is returned.
func (a *AuthzService) parentDeliverEligibility(ctx context.Context, parent *store.Agent) ([]string, error) {
	chain, err := a.chainEffectCeiling(ctx, parent)
	if err != nil {
		if isStructuralProvenanceError(err) {
			return nil, nil
		}
		return nil, err
	}
	return deliverIDsAllowedByChain(chain), nil
}

// ceilingDenyCauseForError maps a ceiling or provenance error to its
// DenyCause. ok is false for a lookup fault (the caller maps it to 503).
func ceilingDenyCauseForError(err error) (DenyCause, bool) {
	switch {
	case errors.Is(err, errSourceNotAllowed):
		return DenyCauseCeilingSourceNotAllowed, true
	case errors.Is(err, errSourceCeilingUnrecorded):
		return DenyCauseCeilingUnrecorded, true
	case errors.Is(err, ErrProvenanceMissing),
		errors.Is(err, ErrProvenanceAmbiguous),
		errors.Is(err, ErrProvenanceChain):
		return DenyCauseCeilingOrphaned, true
	default:
		return "", false
	}
}

// sourceEffectCeiling returns the ceiling and provenance to freeze for a
// write authorized by an interactive request identity. It type-switches on
// the concrete identity:
//   - *AuthenticatedUser (session): principal, provenance session;
//   - the recognized local development user (isTrustedLocalDevUser):
//     principal, provenance dev_local;
//   - *ScopedUserIdentity (UAT) whose ceiling version Allows interprets:
//     bounded, with the UAT's Version and IDs copied unchanged; any other
//     version → errSourceCeilingUnrecorded;
//   - *agentIdentityWrapper with verified claims, parent agent P: bounded V1
//     over P's current coverage (bounded further by P's own edge ceiling)
//     plus parentDeliverEligibility(P). When P has no active edge, the
//     coverage-only form applies while the edge backfill is not complete;
//     otherwise ErrProvenanceMissing;
//   - every other identity, including nil and typed-nil pointers →
//     errSourceNotAllowed.
//
// It does not consult the principal's live grants; the walk does that at use
// time. Callers write nothing on any error.
func (a *AuthzService) sourceEffectCeiling(ctx context.Context, identity Identity) (store.EffectCeiling, store.AuthorityProvenance, error) {
	switch id := identity.(type) {
	case *ScopedUserIdentity:
		if id == nil || id.UserIdentity == nil {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, errSourceNotAllowed
		}
		return a.uatSourceEffectCeiling(ctx, id)
	case *DevUser:
		if !isTrustedLocalDevUser(id) {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, errSourceNotAllowed
		}
		return store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    DevUserID,
			SourceCredentialKind: store.SourceCredentialDevLocal,
		}, nil
	case *AuthenticatedUser:
		if id == nil || id.ID() == "" {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, errSourceNotAllowed
		}
		return store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    id.ID(),
			SourceCredentialKind: store.SourceCredentialSession,
		}, nil
	case *agentIdentityWrapper:
		if id == nil || id.AgentTokenClaims == nil || id.ID() == "" {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, errSourceNotAllowed
		}
		return a.agentSourceEffectCeiling(ctx, id)
	default:
		return store.EffectCeiling{}, store.AuthorityProvenance{}, errSourceNotAllowed
	}
}

// uatSourceEffectCeiling is sourceEffectCeiling's UAT row.
func (a *AuthzService) uatSourceEffectCeiling(ctx context.Context, id *ScopedUserIdentity) (store.EffectCeiling, store.AuthorityProvenance, error) {
	ceiling := id.Ceiling()
	if !knownCeilingVersion(ceiling.Version) {
		return store.EffectCeiling{}, store.AuthorityProvenance{}, errSourceCeilingUnrecorded
	}
	ids := make([]string, len(ceiling.PermissionIDs))
	copy(ids, ceiling.PermissionIDs)
	boundary := id.Boundary()
	ec := store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           ceiling.Version,
		PermissionIDs:     ids,
		BoundaryKind:      string(boundary.Kind),
		BoundaryProjectID: boundary.ProjectID,
	}
	if credID := id.CredentialID(); credID != "" {
		tok, err := a.store.GetUserAccessToken(ctx, credID)
		switch {
		case err == nil && tok != nil && tok.ExpiresAt != nil:
			exp := *tok.ExpiresAt
			ec.SourceExpiresAt = &exp
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("access token lookup: %w", err)
		}
	}
	return ec, store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  store.DelegationPrincipalUser,
		SourcePrincipalID:    id.ID(),
		SourceCredentialKind: store.SourceCredentialUAT,
		SourceCredentialID:   id.CredentialID(),
	}, nil
}

// agentSourceEffectCeiling is sourceEffectCeiling's agent row.
func (a *AuthzService) agentSourceEffectCeiling(ctx context.Context, id *agentIdentityWrapper) (store.EffectCeiling, store.AuthorityProvenance, error) {
	parent, err := a.store.GetAgent(ctx, id.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("%w: source agent %s not found", ErrProvenanceChain, id.ID())
		}
		return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("source agent lookup: %w", err)
	}
	ec, err := a.agentRowEffectCeiling(ctx, parent)
	if err != nil {
		return store.EffectCeiling{}, store.AuthorityProvenance{}, err
	}
	return ec, store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  store.DelegationPrincipalAgent,
		SourcePrincipalID:    parent.ID,
		SourceCredentialKind: store.SourceCredentialAgent,
		SourceCredentialID:   id.TokenID(),
	}, nil
}

// agentRowEffectCeiling returns the ceiling an authority-producing write by
// the stored agent parent carries (the agent row of sourceEffectCeiling):
// bounded V1 over parent's current coverage, bounded further by parent's own
// edge ceiling, plus parentDeliverEligibility(parent). When parent has no
// active edge, the coverage-only form applies while the edge backfill is not
// complete; otherwise ErrProvenanceMissing. A nil or deleted parent is
// ErrProvenanceChain.
func (a *AuthzService) agentRowEffectCeiling(ctx context.Context, parent *store.Agent) (store.EffectCeiling, error) {
	if parent == nil || !parent.DeletedAt.IsZero() {
		return store.EffectCeiling{}, fmt.Errorf("%w: source agent deleted", ErrProvenanceChain)
	}

	active, err := a.activeProjectEdges(ctx, parent.ID, parent.ProjectID)
	if err != nil {
		return store.EffectCeiling{}, err
	}
	if len(active) > 1 {
		return store.EffectCeiling{}, fmt.Errorf("%w: agent %s", ErrProvenanceAmbiguous, parent.ID)
	}
	if len(active) == 0 && a.backfillCompleted(ctx) {
		return store.EffectCeiling{}, fmt.Errorf("%w: agent %s", ErrProvenanceMissing, parent.ID)
	}

	scopes, err := a.ceilingFilteredAgentScopes(ctx, parent, a.mintCandidateScopes(parent))
	if err != nil {
		return store.EffectCeiling{}, err
	}
	coverage := agentScopeCoverage(scopes)

	var (
		parentCeiling store.EffectCeiling
		deliver       []string
	)
	if len(active) == 1 {
		parentCeiling = active[0].EffectCeiling
		deliver, err = a.parentDeliverEligibility(ctx, parent)
		if err != nil {
			return store.EffectCeiling{}, err
		}
	}

	ec := childEffectCeiling(parentCeiling, coverage, deliver)
	ec.BoundaryKind = string(permissions.BoundaryKindProject)
	ec.BoundaryProjectID = parent.ProjectID
	return ec, nil
}
