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

// Package delegationadoption plans and applies the adoption of delegation
// edges that were written before authority provenance was recorded.
//
// Such an edge reads back with provenance version 0 and an unrecorded effect
// ceiling, and the delegation walk denies every permission that requires
// recorded provenance on it. Adoption replaces a validated edge with a
// recorded one: the same typed delegator, delegate, scope and role, source
// credential kind system_migration, and a bounded ceiling taken from a
// frozen compatibility policy (permissions.CompatibilityCeiling). Nothing
// about the historic credential is invented.
//
// The planner is pure over a Reader. The boot migration (pkg/store/entadapter)
// and the admin recovery API (pkg/hub) both use it, so the two cannot drift.
package delegationadoption

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// PolicyVersion is the compatibility policy this package applies.
const PolicyVersion = permissions.CompatibilityPolicyV1

// MaxDelegationDepth mirrors the delegation walk's depth limit: the walk
// examines at most MaxDelegationDepth+1 hops.
const MaxDelegationDepth = 10

// migrationDelegatorID is the delegator recorded by the edge backfill for
// agents with no usable provenance. It is not a principal.
const migrationDelegatorID = "system/migration"

// Reader is the read surface the planner needs. store.Store satisfies it,
// including a transactional store.
type Reader interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
	ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error)
	GetUser(ctx context.Context, id string) (*store.User, error)
	GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error)
	ListAllDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error)
}

// Reason is a stable code for why a hop is not adopted.
type Reason string

// Exclusion and skip reasons.
const (
	ReasonMissingEdge               Reason = "missing_edge"
	ReasonDuplicateActiveEdges      Reason = "duplicate_active_edges"
	ReasonScopeMismatch             Reason = "scope_mismatch"
	ReasonUnsupportedDelegatorType  Reason = "unsupported_delegator_type"
	ReasonNoPrincipalRoot           Reason = "no_principal_root"
	ReasonRootMissing               Reason = "root_missing"
	ReasonRootInactive              Reason = "root_inactive"
	ReasonParentMissing             Reason = "parent_missing"
	ReasonParentDeleted             Reason = "parent_deleted"
	ReasonAncestorExcluded          Reason = "ancestor_excluded"
	ReasonDelegatorAncestryMismatch Reason = "delegator_ancestry_mismatch"
	ReasonTooDeep                   Reason = "too_deep"
	ReasonCycle                     Reason = "cycle"
	ReasonUnknownProvenanceVersion  Reason = "unknown_provenance_version"
	ReasonMalformedProvenance       Reason = "malformed_provenance"
	ReasonMalformedCeiling          Reason = "malformed_ceiling"
	ReasonRoleNone                  Reason = "role_none"
	ReasonUnknownRole               Reason = "unknown_role"
	ReasonUnreadableAgentConfig     Reason = "unreadable_agent_config"

	// Skip reasons, recorded when state changed after planning.
	ReasonDelegateNotLive    Reason = "delegate_not_live"
	ReasonEdgeChanged        Reason = "edge_changed"
	ReasonFingerprintChanged Reason = "fingerprint_changed"
	ReasonAncestorNotAdopted Reason = "ancestor_not_adopted"
	ReasonConcurrentAdoption Reason = "concurrent_adoption"
	ReasonNotAdoptable       Reason = "not_adoptable"
	ReasonNotRevertible      Reason = "not_revertible"
	ReasonAmbiguousOriginal  Reason = "ambiguous_original"
	ReasonOriginalChanged    Reason = "original_changed"
	ReasonAdoptedEdgeChanged Reason = "adopted_edge_changed"
	ReasonRecordMissing      Reason = "record_missing"
	// ReasonCoveredOriginalDiffers: a record covered by a revert hop names
	// a different original edge than the hop it is reverted with.
	ReasonCoveredOriginalDiffers Reason = "covered_original_differs"
)

// Outcome is the planned treatment of one hop.
type Outcome string

const (
	// OutcomeAdopt: an unrecorded hop on a valid path; it gets a recorded
	// edge with CeilingIDs.
	OutcomeAdopt Outcome = "adopt"
	// OutcomeRecognized: an already-adopted hop (system_migration, bounded
	// V1, project boundary on its own scope), left as is.
	OutcomeRecognized Outcome = "recognized"
	// OutcomeRecognizedAbovePolicy: recognized, and its IDs are not a
	// subset of the policy for its role. Reported only, never narrowed.
	OutcomeRecognizedAbovePolicy Outcome = "recognized_above_policy"
	// OutcomeRecorded: a recorded hop on a valid path. Never rewritten.
	OutcomeRecorded Outcome = "recorded"
	// OutcomeExcluded: never adopted (Reason).
	OutcomeExcluded Outcome = "excluded"
)

// Hop is the planner's result for one live agent's hop in its project.
type Hop struct {
	DelegateID string `json:"delegateId"`
	ProjectID  string `json:"projectId"`
	// Edge is the single active project edge; nil when missing or
	// duplicated.
	Edge *store.DelegationEdge `json:"-"`
	// Depth is the number of hops from the root principal (1 = delegated
	// by a user). 0 when the path could not be resolved.
	Depth   int     `json:"depth"`
	Outcome Outcome `json:"outcome"`
	Reason  Reason  `json:"reason,omitempty"`
	// Role is the role the adopted ceiling is computed for: the lower of
	// the edge role and the agent's applied role.
	Role          string `json:"role,omitempty"`
	HasAssignedSA bool   `json:"hasAssignedServiceAccount,omitempty"`
	// CeilingIDs: adopt → the ceiling to record; recognized → the recorded
	// IDs.
	CeilingIDs []string `json:"ceilingPermissionIds,omitempty"`
	// OriginalEdgeID: adopt → Edge.ID; recognized → the inactive
	// unrecorded row it replaced, when exactly one matches.
	OriginalEdgeID string `json:"originalEdgeId,omitempty"`
	Fingerprint    string `json:"fingerprint"`

	unrecorded     bool
	delegatorState string
}

// Unrecorded reports whether the hop's edge is an unrecorded candidate row.
func (h *Hop) Unrecorded() bool { return h != nil && h.unrecorded }

// adoptedCeiling is the hop's ceiling once the plan is applied.
func (h *Hop) adoptedCeiling() store.EffectCeiling {
	if h.Outcome == OutcomeAdopt {
		return store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: h.CeilingIDs}
	}
	if h.Edge == nil {
		return store.EffectCeiling{}
	}
	return h.Edge.EffectCeiling
}

// Plan is the planner's result over the live-agent ancestor closure of a
// scope.
type Plan struct {
	PolicyVersion permissions.CompatibilityPolicyVersion `json:"policyVersion"`
	// Hops, ordered by depth then delegate ID, so applying them in order is
	// top-down.
	Hops []*Hop `json:"hops"`
}

// Hop returns the hop of delegateID, or nil.
func (p *Plan) Hop(delegateID string) *Hop {
	for _, h := range p.Hops {
		if h.DelegateID == delegateID {
			return h
		}
	}
	return nil
}

// Count returns the number of hops with outcome o.
func (p *Plan) Count(o Outcome) int {
	n := 0
	for _, h := range p.Hops {
		if h.Outcome == o {
			n++
		}
	}
	return n
}

// Scope selects the live agents whose ancestor closure is planned. The zero
// value selects every live agent.
type Scope struct {
	ProjectID string   `json:"projectId,omitempty"`
	AgentIDs  []string `json:"agentIds,omitempty"`
}

// Build plans the live-agent ancestor closure of scope. Every hop gets
// exactly one outcome. A lookup fault returns an error; a structural defect
// is an exclusion reason. No timestamp selects or excludes a hop.
func Build(ctx context.Context, r Reader, scope Scope) (*Plan, error) {
	roots, err := liveAgents(ctx, r, scope)
	if err != nil {
		return nil, err
	}
	p := &planner{ctx: ctx, r: r, hops: map[string]*Hop{}, inProgress: map[string]bool{}}
	for _, a := range roots {
		if _, err := p.eval(a); err != nil {
			return nil, err
		}
	}
	plan := &Plan{PolicyVersion: PolicyVersion}
	for _, h := range p.hops {
		plan.Hops = append(plan.Hops, h)
	}
	sort.Slice(plan.Hops, func(i, j int) bool {
		a, b := plan.Hops[i], plan.Hops[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		return a.DelegateID < b.DelegateID
	})
	return plan, nil
}

// liveAgents returns the agents selected by scope that are live (not
// deleted). A stopped agent is live.
func liveAgents(ctx context.Context, r Reader, scope Scope) ([]*store.Agent, error) {
	if len(scope.AgentIDs) > 0 {
		var out []*store.Agent
		seen := map[string]bool{}
		for _, id := range scope.AgentIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			a, err := r.GetAgent(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("agent %s lookup: %w", id, err)
			}
			if a == nil || !a.DeletedAt.IsZero() {
				continue
			}
			if scope.ProjectID != "" && a.ProjectID != scope.ProjectID {
				continue
			}
			out = append(out, a)
		}
		return out, nil
	}
	var out []*store.Agent
	cursor := ""
	for {
		res, err := r.ListAgents(ctx, store.AgentFilter{ProjectID: scope.ProjectID}, store.ListOptions{Limit: 500, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		for i := range res.Items {
			a := res.Items[i]
			if a.DeletedAt.IsZero() {
				out = append(out, &a)
			}
		}
		if res.NextCursor == "" || len(res.Items) == 0 {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

type planner struct {
	ctx        context.Context
	r          Reader
	hops       map[string]*Hop
	inProgress map[string]bool
}

// eval returns the hop of live agent a, evaluating its ancestors first.
func (p *planner) eval(a *store.Agent) (*Hop, error) {
	if h, ok := p.hops[a.ID]; ok {
		return h, nil
	}
	h := &Hop{DelegateID: a.ID, ProjectID: a.ProjectID}
	p.inProgress[a.ID] = true
	err := p.evalInto(a, h)
	delete(p.inProgress, a.ID)
	if err != nil {
		return nil, err
	}
	h.Fingerprint = p.fingerprint(a, h)
	p.hops[a.ID] = h
	return h, nil
}

func exclude(h *Hop, reason Reason) error {
	h.Outcome = OutcomeExcluded
	h.Reason = reason
	h.CeilingIDs = nil
	return nil
}

func (p *planner) evalInto(a *store.Agent, h *Hop) error {
	edges, err := p.r.GetDelegationEdgesForDelegate(p.ctx, store.DelegationPrincipalAgent, a.ID)
	if err != nil {
		return fmt.Errorf("delegation edges of agent %s: %w", a.ID, err)
	}
	var inScope []*store.DelegationEdge
	otherScope := 0
	for _, e := range edges {
		if !e.Active {
			continue
		}
		if e.ScopeType == store.RoleScopeProject && e.ScopeID == a.ProjectID {
			inScope = append(inScope, e)
		} else {
			otherScope++
		}
	}
	switch {
	case len(inScope) == 0 && otherScope > 0:
		return exclude(h, ReasonScopeMismatch)
	case len(inScope) == 0:
		return exclude(h, ReasonMissingEdge)
	case len(inScope) > 1:
		return exclude(h, ReasonDuplicateActiveEdges)
	}
	edge := inScope[0]
	h.Edge = edge

	// Row shape.
	switch edge.ProvenanceVersion {
	case 0, store.ProvenanceVersionV1:
	default:
		return exclude(h, ReasonUnknownProvenanceVersion)
	}
	switch edge.Kind {
	case store.EffectCeilingUnrecorded, store.EffectCeilingBounded, store.EffectCeilingPrincipal:
	default:
		return exclude(h, ReasonMalformedCeiling)
	}
	if edge.ProvenanceVersion == 0 {
		if !cleanUnrecorded(edge) {
			return exclude(h, ReasonMalformedProvenance)
		}
		h.unrecorded = true
	} else if edge.Kind == store.EffectCeilingUnrecorded {
		return exclude(h, ReasonMalformedCeiling)
	}

	// Delegator.
	var parent *Hop
	switch edge.DelegatorType {
	case store.DelegationPrincipalUser:
		if edge.DelegatorID == migrationDelegatorID {
			return exclude(h, ReasonNoPrincipalRoot)
		}
		u, err := p.r.GetUser(p.ctx, edge.DelegatorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && u == nil) {
			h.delegatorState = "missing"
			return exclude(h, ReasonRootMissing)
		}
		if err != nil {
			return fmt.Errorf("user %s lookup: %w", edge.DelegatorID, err)
		}
		h.delegatorState = "user:" + u.Status
		if u.Status != store.UserStatusActive {
			return exclude(h, ReasonRootInactive)
		}
		h.Depth = 1
	case store.DelegationPrincipalAgent:
		if p.inProgress[edge.DelegatorID] || edge.DelegatorID == a.ID {
			return exclude(h, ReasonCycle)
		}
		pa, err := p.r.GetAgent(p.ctx, edge.DelegatorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && pa == nil) {
			h.delegatorState = "missing"
			return exclude(h, ReasonParentMissing)
		}
		if err != nil {
			return fmt.Errorf("agent %s lookup: %w", edge.DelegatorID, err)
		}
		h.delegatorState = "agent:live"
		if !pa.DeletedAt.IsZero() {
			h.delegatorState = "agent:deleted"
			return exclude(h, ReasonParentDeleted)
		}
		if pa.ProjectID != edge.ScopeID {
			return exclude(h, ReasonScopeMismatch)
		}
		parent, err = p.eval(pa)
		if err != nil {
			return err
		}
		if parent.Outcome == OutcomeExcluded {
			if parent.Reason == ReasonCycle {
				return exclude(h, ReasonCycle)
			}
			return exclude(h, ReasonAncestorExcluded)
		}
		h.Depth = parent.Depth + 1
	default:
		return exclude(h, ReasonUnsupportedDelegatorType)
	}
	if h.Depth > MaxDelegationDepth+1 {
		return exclude(h, ReasonTooDeep)
	}
	if !delegatorMatchesAncestry(a, edge) {
		return exclude(h, ReasonDelegatorAncestryMismatch)
	}
	h.HasAssignedSA = hasAssignedSA(a)

	if !h.unrecorded {
		if alreadyAdopted(edge) {
			h.Outcome = OutcomeRecognized
			h.CeilingIDs = append([]string(nil), edge.PermissionIDs...)
			policy, ok := permissions.CompatibilityCeiling(PolicyVersion, edge.Role, h.HasAssignedSA)
			if !ok || !subset(edge.PermissionIDs, policy) {
				h.Outcome = OutcomeRecognizedAbovePolicy
			}
			orig, err := p.resolveOriginal(edge)
			if err != nil {
				return err
			}
			h.OriginalEdgeID = orig
			return nil
		}
		h.Outcome = OutcomeRecorded
		return nil
	}

	// Unrecorded candidate on a valid path: role and configuration.
	edgeRole, reason := policyRole(edge.Role)
	if reason != "" {
		return exclude(h, reason)
	}
	if a.AppliedConfig == nil || a.AppliedConfig.AgentRole == "" {
		return exclude(h, ReasonUnreadableAgentConfig)
	}
	appliedRole, reason := policyRole(a.AppliedConfig.AgentRole)
	if reason != "" {
		return exclude(h, reason)
	}
	role := minRole(edgeRole, appliedRole)
	ids, ok := permissions.CompatibilityCeiling(PolicyVersion, role, h.HasAssignedSA)
	if !ok {
		return exclude(h, ReasonUnknownRole)
	}
	if parent != nil {
		ids = intersect(ids, parent.adoptedCeiling())
	}
	h.Outcome = OutcomeAdopt
	h.Role = role
	h.CeilingIDs = ids
	h.OriginalEdgeID = edge.ID
	return nil
}

// resolveOriginal returns the single inactive unrecorded row an
// already-adopted edge replaced (same delegator, delegate, scope and role),
// or "" when none or more than one matches.
func (p *planner) resolveOriginal(adopted *store.DelegationEdge) (string, error) {
	all, err := p.r.ListAllDelegationEdgesForDelegate(p.ctx, store.DelegationPrincipalAgent, adopted.DelegateID)
	if err != nil {
		return "", fmt.Errorf("delegation edges of agent %s: %w", adopted.DelegateID, err)
	}
	found := ""
	for _, e := range all {
		if MatchesOriginal(adopted, e) {
			if found != "" {
				return "", nil
			}
			found = e.ID
		}
	}
	return found, nil
}

// MatchesOriginal reports whether candidate can be the unrecorded row that
// adopted replaced: inactive, a clean unrecorded row, and the same
// delegator, delegate, scope and role.
func MatchesOriginal(adopted, candidate *store.DelegationEdge) bool {
	if candidate == nil || adopted == nil || candidate.Active || candidate.ID == adopted.ID {
		return false
	}
	if candidate.ProvenanceVersion != 0 || candidate.Kind != store.EffectCeilingUnrecorded ||
		candidate.AuthorityProvenance != (store.AuthorityProvenance{}) {
		return false
	}
	switch candidate.Cause {
	case "", store.EdgeDeactivationProvenanceAdopted:
	default:
		return false
	}
	return candidate.DelegatorType == adopted.DelegatorType &&
		candidate.DelegatorID == adopted.DelegatorID &&
		candidate.DelegateType == adopted.DelegateType &&
		candidate.DelegateID == adopted.DelegateID &&
		candidate.ScopeType == adopted.ScopeType &&
		candidate.ScopeID == adopted.ScopeID &&
		candidate.Role == adopted.Role
}

// cleanUnrecorded reports whether edge is an unrecorded candidate row: no
// provenance field set, an unrecorded ceiling with no IDs and no boundary,
// and no deactivation record.
func cleanUnrecorded(edge *store.DelegationEdge) bool {
	if edge.AuthorityProvenance != (store.AuthorityProvenance{}) {
		return false
	}
	c := edge.EffectCeiling
	if c.Kind != store.EffectCeilingUnrecorded || c.Version != 0 || c.PermissionIDs != nil ||
		c.BoundaryKind != "" || c.BoundaryProjectID != "" || c.SourceExpiresAt != nil {
		return false
	}
	return edge.Cause == "" && edge.At == nil && edge.OpID == ""
}

// alreadyAdopted reports whether a recorded edge carries a migration-recorded
// bounded V1 ceiling bound to its own project: a row from a prior adoption
// run or from an operational repair of the same shape.
func alreadyAdopted(edge *store.DelegationEdge) bool {
	return edge.ProvenanceVersion == store.ProvenanceVersionV1 &&
		edge.SourceCredentialKind == store.SourceCredentialSystemMigration &&
		edge.Kind == store.EffectCeilingBounded &&
		edge.Version == permissions.CeilingVersionV1 &&
		edge.BoundaryKind == string(permissions.BoundaryKindProject) &&
		edge.ScopeType == store.RoleScopeProject &&
		edge.BoundaryProjectID == edge.ScopeID
}

// delegatorMatchesAncestry reports whether the edge's delegator agrees with
// the agent's recorded creator: agent:Ancestry[last] when the ancestry has
// two or more entries, user:Ancestry[0] when it has one, and user:CreatedBy
// when it is empty.
func delegatorMatchesAncestry(a *store.Agent, edge *store.DelegationEdge) bool {
	switch n := len(a.Ancestry); {
	case n >= 2:
		return edge.DelegatorType == store.DelegationPrincipalAgent && edge.DelegatorID == a.Ancestry[n-1]
	case n == 1:
		return edge.DelegatorType == store.DelegationPrincipalUser && edge.DelegatorID == a.Ancestry[0]
	default:
		return a.CreatedBy != "" && edge.DelegatorType == store.DelegationPrincipalUser && edge.DelegatorID == a.CreatedBy
	}
}

func hasAssignedSA(a *store.Agent) bool {
	return a.AppliedConfig != nil && a.AppliedConfig.GCPIdentity != nil &&
		a.AppliedConfig.GCPIdentity.MetadataMode == store.GCPMetadataModeAssign &&
		a.AppliedConfig.GCPIdentity.ServiceAccountID != ""
}

var roleRank = map[string]int{"readonly": 1, "baseline": 2, "full": 3}

// policyRole validates role for the compatibility policy.
func policyRole(role string) (string, Reason) {
	if role == "none" {
		return "", ReasonRoleNone
	}
	if _, ok := roleRank[role]; !ok {
		return "", ReasonUnknownRole
	}
	return role, ""
}

func minRole(a, b string) string {
	if roleRank[a] <= roleRank[b] {
		return a
	}
	return b
}

// intersect keeps the IDs of ids that parent allows. A principal parent
// does not narrow; any other non-bounded parent allows nothing.
func intersect(ids []string, parent store.EffectCeiling) []string {
	switch parent.Kind {
	case store.EffectCeilingPrincipal:
		return ids
	case store.EffectCeilingBounded:
	default:
		// Defensive and unreachable: the planner excludes a hop whose
		// parent ceiling is neither principal nor bounded before it
		// intersects. Allowing nothing keeps the result fail-closed.
		return []string{}
	}
	frozen, ok := parent.Frozen()
	out := []string{}
	for _, id := range ids {
		if ok && frozen.Allows(id) {
			out = append(out, id)
		}
	}
	return out
}

func subset(ids, of []string) bool {
	set := make(map[string]bool, len(of))
	for _, id := range of {
		set[id] = true
	}
	for _, id := range ids {
		if !set[id] {
			return false
		}
	}
	return true
}
