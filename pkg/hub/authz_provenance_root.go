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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file resolves the recorded provenance root of an agent: the typed
// principal that delegated to it, read from its single active project-scoped
// delegation edge, and the terminal user reached by walking those edges
// upward. It also exposes the chain effect ceiling and the scopes an agent
// may hold, computed exactly as token mint computes them.
//
// The result is authority evidence only. It is not attested ancestry and
// grants no user-material rights.

// ProvenancePrincipal is a typed principal recorded on a delegation edge.
type ProvenancePrincipal struct {
	Kind string // store.DelegationPrincipalUser or store.DelegationPrincipalAgent
	ID   string
}

// ProvenanceRevision identifies the schedule revision under which the
// scheduler wrote an edge. It is zero unless the edge was written at fire
// time.
type ProvenanceRevision struct {
	ScheduleID            string
	EventID               string
	AuthorizationRevision int
}

// RecordedProvenanceRoot is the resolved provenance of an agent.
type RecordedProvenanceRoot struct {
	Principal  ProvenancePrincipal       // immediate typed delegator of the agent
	Edge       store.DelegationEdge      // the single active project-scoped edge
	Revision   ProvenanceRevision        // set for an edge written by the scheduler
	Ceiling    store.EffectCeiling       // Edge.EffectCeiling
	Provenance store.AuthorityProvenance // Edge.AuthorityProvenance
	RootUser   *store.User               // terminal user after the structural chain walk

	// rootHopDevLocal is true when the hop that reaches RootUser carries
	// local-development provenance.
	rootHopDevLocal bool
}

// ResolveProvenanceOptions configures ResolveProvenanceRoot. The zero value
// denies unrecorded provenance.
type ResolveProvenanceOptions struct {
	// PermissionID is required: the exact canonical permission the caller
	// is about to authorize. The root user is admitted to the agent's
	// project for this permission through ProjectAdmissionForClass with
	// executionProjectClass(PermissionID). An empty or unregistered value
	// returns ErrProvenanceRequest.
	PermissionID string
	// AllowUnrecordedLegacy, when true, accepts unrecorded hops under the
	// frozen legacy characterization. Production callers are pinned by a
	// test; none sets it.
	AllowUnrecordedLegacy bool
}

// AgentAuthorityOptions configures AgentEffectCeiling and
// EffectiveAgentAuthority. The zero value denies unrecorded hops.
type AgentAuthorityOptions struct {
	// AllowUnrecordedLegacy accepts unrecorded hops; the result then drops
	// every permission an unrecorded hop never allows. Production callers
	// are pinned by a test; none sets it.
	AllowUnrecordedLegacy bool
}

// Provenance resolution outcomes. Every one denies. ErrProvenanceMissing,
// ErrProvenanceAmbiguous and ErrProvenanceChain are declared with the chain
// fold in authz_effect_ceiling.go.
var (
	ErrProvenanceRequest     = errors.New("provenance: invalid request")
	ErrProvenanceMismatch    = errors.New("provenance: recorded type or revision does not match")
	ErrProvenanceUnrecorded  = errors.New("provenance: not recorded")
	ErrProvenanceInactive    = errors.New("provenance: principal inactive or deleted")
	ErrProvenanceNotAdmitted = errors.New("provenance: principal lacks project admission")
	// ErrProvenanceSourceNotAllowed: a hop carries local-development
	// provenance and fails a local-development check (dev auth is not
	// enabled on this server; or, from AgentEffectCeiling and
	// EffectiveAgentAuthority, the delegator is not the local development
	// user or that user is missing or not active).
	ErrProvenanceSourceNotAllowed = errors.New("provenance: source credential not accepted on this server")
)

// edgeHasDevLocalProvenance reports whether edge was recorded from the
// recognized local development user, directly or by the scheduler under a
// revision that user authorized.
func edgeHasDevLocalProvenance(edge *store.DelegationEdge) bool {
	if edge == nil {
		return false
	}
	if edge.SourceCredentialKind == store.SourceCredentialDevLocal {
		return true
	}
	return edge.SourceCredentialKind == store.SourceCredentialScheduler &&
		edge.InitiatorCredentialKind == store.InitiatorCredentialKindDevLocal
}

// edgeProvenanceRecorded reports whether edge carries provenance this binary
// can read: a recorded ceiling kind and a known provenance version.
func edgeProvenanceRecorded(edge *store.DelegationEdge) bool {
	return edge.Kind != store.EffectCeilingUnrecorded && knownProvenanceVersion(edge.ProvenanceVersion)
}

// checkEdgePrincipal applies the principal-typing rule to one hop. The
// principal comes from the edge's DelegatorType, never from probing an ID.
//   - DelegatorType must be user or agent;
//   - with readable provenance, SourcePrincipalKind must equal the
//     delegator type;
//   - a dev_local edge must have delegator user:DevUserID and
//     SourcePrincipalID DevUserID;
//   - a scheduler edge must be internally consistent (checkSchedulerEdge).
func checkEdgePrincipal(edge *store.DelegationEdge) error {
	switch edge.DelegatorType {
	case store.DelegationPrincipalUser, store.DelegationPrincipalAgent:
	default:
		return fmt.Errorf("%w: edge %s has delegator type %q", ErrProvenanceMismatch, edge.ID, edge.DelegatorType)
	}
	if !knownProvenanceVersion(edge.ProvenanceVersion) {
		return nil
	}
	if edge.SourcePrincipalKind != edge.DelegatorType {
		return fmt.Errorf("%w: edge %s records source kind %q for a %s delegator", ErrProvenanceMismatch, edge.ID, edge.SourcePrincipalKind, edge.DelegatorType)
	}
	switch edge.SourceCredentialKind {
	case store.SourceCredentialDevLocal:
		if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID || edge.SourcePrincipalID != DevUserID {
			return fmt.Errorf("%w: edge %s has local development provenance with another principal", ErrProvenanceMismatch, edge.ID)
		}
	case store.SourceCredentialScheduler:
		return checkSchedulerEdge(edge)
	}
	return nil
}

// checkSchedulerEdge checks a scheduler edge against its own frozen copy of
// the schedule revision. The scheduled event row is never read, so purging
// events cannot change the result. The source access token's current state
// is not checked: a created child is a completed action.
func checkSchedulerEdge(edge *store.DelegationEdge) error {
	if edge.SourceEventID == "" || edge.SourceAuthorizationRevision < 1 {
		return fmt.Errorf("%w: scheduler edge %s has no revision reference", ErrProvenanceMismatch, edge.ID)
	}
	if edge.Kind == store.EffectCeilingUnrecorded {
		return fmt.Errorf("%w: scheduler edge %s has no recorded ceiling", ErrProvenanceMismatch, edge.ID)
	}
	switch edge.InitiatorCredentialKind {
	case store.InitiatorCredentialKindSession, store.InitiatorCredentialKindUAT, store.InitiatorCredentialKindAgent:
		if edge.InitiatorPrincipalKind != edge.DelegatorType || edge.InitiatorPrincipalID != edge.DelegatorID {
			return fmt.Errorf("%w: scheduler edge %s initiator is not its delegator", ErrProvenanceMismatch, edge.ID)
		}
	case store.InitiatorCredentialKindDevLocal:
		// A dev_local revision records the local development identity kind.
		if edge.InitiatorPrincipalKind != string(PrincipalKindDev) || edge.InitiatorPrincipalID != DevUserID ||
			edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID {
			return fmt.Errorf("%w: scheduler edge %s has local development provenance with another principal", ErrProvenanceMismatch, edge.ID)
		}
	default:
		return fmt.Errorf("%w: scheduler edge %s has initiator credential %q", ErrProvenanceMismatch, edge.ID, edge.InitiatorCredentialKind)
	}
	return nil
}

// resolveProvenanceChain walks agent's active project-scoped edges upward
// and returns the immediate hop and the terminal user. Per hop: exactly one
// active edge in the agent's project, the principal-typing rule
// (checkEdgePrincipal), live agent delegators, depth ≤ maxDelegationDepth
// and no cycle. The migration sentinel returns ErrProvenanceChain. Unless
// allowUnrecordedLegacy, a hop whose ceiling is unrecorded or whose
// provenance version is not known returns ErrProvenanceUnrecorded.
//
// It loads RootUser but does not check its status or admission, and it does
// not apply the local-development enablement check. Lookup errors are
// returned wrapped.
func (a *AuthzService) resolveProvenanceChain(ctx context.Context, agent *store.Agent, allowUnrecordedLegacy bool) (RecordedProvenanceRoot, error) {
	if agent == nil || agent.ID == "" {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: no agent", ErrProvenanceRequest)
	}
	if !agent.DeletedAt.IsZero() {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s is deleted", ErrProvenanceInactive, agent.ID)
	}
	if agent.ProjectID == "" {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s has no project", ErrProvenanceMissing, agent.ID)
	}

	var root RecordedProvenanceRoot
	visited := make(map[string]bool, maxDelegationDepth+1)
	delegateID := agent.ID
	for depth := 0; depth <= maxDelegationDepth; depth++ {
		if visited[delegateID] {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s repeats in the chain", ErrProvenanceChain, delegateID)
		}
		visited[delegateID] = true

		active, err := a.activeProjectEdges(ctx, delegateID, agent.ProjectID)
		if err != nil {
			return RecordedProvenanceRoot{}, err
		}
		if len(active) == 0 {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s", ErrProvenanceMissing, delegateID)
		}
		if len(active) > 1 {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s", ErrProvenanceAmbiguous, delegateID)
		}
		edge := active[0]
		if isMigrationSentinel(edge) {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: migration-provenance edge for agent %s", ErrProvenanceChain, delegateID)
		}
		if !allowUnrecordedLegacy && !edgeProvenanceRecorded(edge) {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: edge %s", ErrProvenanceUnrecorded, edge.ID)
		}
		switch edge.Kind {
		case store.EffectCeilingUnrecorded, store.EffectCeilingPrincipal, store.EffectCeilingBounded:
		default:
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: edge %s has ceiling kind %q", ErrProvenanceChain, edge.ID, edge.Kind)
		}
		if err := checkEdgePrincipal(edge); err != nil {
			return RecordedProvenanceRoot{}, err
		}

		if depth == 0 {
			root.Principal = ProvenancePrincipal{Kind: edge.DelegatorType, ID: edge.DelegatorID}
			root.Edge = *edge
			root.Ceiling = edge.EffectCeiling
			root.Provenance = edge.AuthorityProvenance
			if edge.SourceCredentialKind == store.SourceCredentialScheduler {
				root.Revision = ProvenanceRevision{
					ScheduleID:            edge.SourceScheduleID,
					EventID:               edge.SourceEventID,
					AuthorizationRevision: edge.SourceAuthorizationRevision,
				}
			}
		}

		if edge.DelegatorType == store.DelegationPrincipalUser {
			user, err := a.store.GetUser(ctx, edge.DelegatorID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return RecordedProvenanceRoot{}, fmt.Errorf("%w: user %s does not exist", ErrProvenanceInactive, edge.DelegatorID)
				}
				return RecordedProvenanceRoot{}, fmt.Errorf("provenance user lookup %s: %w", edge.DelegatorID, err)
			}
			if user == nil {
				return RecordedProvenanceRoot{}, fmt.Errorf("%w: user %s does not exist", ErrProvenanceInactive, edge.DelegatorID)
			}
			root.RootUser = user
			root.rootHopDevLocal = edgeHasDevLocalProvenance(edge)
			return root, nil
		}

		parent, err := a.store.GetAgent(ctx, edge.DelegatorID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s does not exist", ErrProvenanceInactive, edge.DelegatorID)
			}
			return RecordedProvenanceRoot{}, fmt.Errorf("provenance agent lookup %s: %w", edge.DelegatorID, err)
		}
		if parent == nil || !parent.DeletedAt.IsZero() {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s is deleted", ErrProvenanceInactive, edge.DelegatorID)
		}
		delegateID = parent.ID
	}
	return RecordedProvenanceRoot{}, fmt.Errorf("%w: maximum depth exceeded", ErrProvenanceChain)
}

// ResolveProvenanceRoot resolves agentID's recorded provenance root and
// checks the root user for opts.PermissionID:
//  1. the agent exists and is not deleted;
//  2. resolveProvenanceChain succeeds (zero options deny unrecorded hops);
//  3. when the hop reaching the root user carries local-development
//     provenance, dev auth is enabled on this server;
//  4. the root user is active and holds ProjectAdmissionForClass to the
//     agent's project for opts.PermissionID.
//
// Permission-specific authority beyond project admission is the caller's to
// evaluate. A nil error always comes with a non-zero Principal and a
// non-nil RootUser. Every error denies; lookup errors are returned wrapped.
func (a *AuthzService) ResolveProvenanceRoot(ctx context.Context, agentID string, opts ResolveProvenanceOptions) (RecordedProvenanceRoot, error) {
	if agentID == "" || opts.PermissionID == "" || registryResourceType(opts.PermissionID) == "" {
		return RecordedProvenanceRoot{}, ErrProvenanceRequest
	}
	if a == nil || a.store == nil {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: store not available", ErrProvenanceRequest)
	}
	agent, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s does not exist", ErrProvenanceInactive, agentID)
		}
		return RecordedProvenanceRoot{}, fmt.Errorf("provenance agent lookup %s: %w", agentID, err)
	}
	if agent == nil {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: agent %s does not exist", ErrProvenanceInactive, agentID)
	}

	root, err := a.resolveProvenanceChain(ctx, agent, opts.AllowUnrecordedLegacy)
	if err != nil {
		return RecordedProvenanceRoot{}, err
	}
	if root.rootHopDevLocal && !a.devLocalAuthorityEnabled() {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: %s", ErrProvenanceSourceNotAllowed, reasonDevLocalDisabled)
	}
	user := root.RootUser
	if user.Status != store.UserStatusActive {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: user %s is %s", ErrProvenanceInactive, user.ID, user.Status)
	}

	// Admission is evaluated for the root user, never the requester.
	admitCtx := maskAuthzInputs(ctx)
	userPC := PrincipalContext{
		Kind:     PrincipalKindUser,
		ID:       user.ID,
		Identity: NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, ""),
	}
	res, err := a.ProjectAdmissionForClass(admitCtx, userPC, agent.ProjectID, opts.PermissionID, executionProjectClass(opts.PermissionID), nil)
	if err != nil {
		return RecordedProvenanceRoot{}, fmt.Errorf("provenance project admission: %w", err)
	}
	if !res.Admitted {
		return RecordedProvenanceRoot{}, fmt.Errorf("%w: user %s, permission %s", ErrProvenanceNotAdmitted, user.ID, opts.PermissionID)
	}
	return root, nil
}

// DenyCauseForProvenanceError maps an error from ResolveProvenanceRoot,
// AgentEffectCeiling or EffectiveAgentAuthority to a DenyCause:
//   - source not accepted → ceiling_source_not_allowed;
//   - unrecorded → ceiling_unrecorded;
//   - missing, ambiguous, invalid chain, mismatch, inactive → ceiling_orphaned;
//   - root user not admitted → ceiling_delegator_lacks_permission;
//   - nil → "";
//   - anything else (invalid request, lookup fault) → ceiling_error.
func DenyCauseForProvenanceError(err error) DenyCause {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrProvenanceSourceNotAllowed), errors.Is(err, errSourceNotAllowed):
		return DenyCauseCeilingSourceNotAllowed
	case errors.Is(err, ErrProvenanceUnrecorded), errors.Is(err, errSourceCeilingUnrecorded):
		return DenyCauseCeilingUnrecorded
	case errors.Is(err, ErrProvenanceMissing),
		errors.Is(err, ErrProvenanceAmbiguous),
		errors.Is(err, ErrProvenanceChain),
		errors.Is(err, ErrProvenanceMismatch),
		errors.Is(err, ErrProvenanceInactive):
		return DenyCauseCeilingOrphaned
	case errors.Is(err, ErrProvenanceNotAdmitted):
		return DenyCauseCeilingDelegatorLacksPermission
	default:
		return DenyCauseCeilingError
	}
}

// exportProvenanceError maps the chain fold's internal source error to the
// exported ErrProvenanceSourceNotAllowed, keeping the detail. Other errors
// pass through.
func exportProvenanceError(err error) error {
	if errors.Is(err, errSourceNotAllowed) && !errors.Is(err, ErrProvenanceSourceNotAllowed) {
		return fmt.Errorf("%w: %v", ErrProvenanceSourceNotAllowed, err)
	}
	return err
}

// projectChainCeiling projects a chain fold for a caller's options. With no
// unrecorded hop it returns chain.Ceiling unchanged. With an unrecorded hop:
//   - zero options → ErrProvenanceUnrecorded;
//   - AllowUnrecordedLegacy and a bounded fold → the fold less every
//     permission an unrecorded hop never allows (recordedProvenanceRequired
//     and legacyChainExcludedPermissions), so the result is never wider than
//     the walk or mint;
//   - AllowUnrecordedLegacy and an unrecorded fold → returned as is
//     (EffectCeilingAllows already denies those permissions on it).
func projectChainCeiling(chain ChainCeiling, opts AgentAuthorityOptions) (store.EffectCeiling, error) {
	if chain.UnrecordedHops == 0 {
		return chain.Ceiling, nil
	}
	if !opts.AllowUnrecordedLegacy {
		return store.EffectCeiling{}, fmt.Errorf("%w: %d unrecorded hops", ErrProvenanceUnrecorded, chain.UnrecordedHops)
	}
	if chain.Ceiling.Kind != store.EffectCeilingBounded {
		return chain.Ceiling, nil
	}
	out := chain.Ceiling
	ids := make([]string, 0, len(chain.Ceiling.PermissionIDs))
	for _, id := range chain.Ceiling.PermissionIDs {
		if recordedProvenanceRequired[id] || legacyChainExcludedPermissions[id] {
			continue
		}
		ids = append(ids, id)
	}
	out.PermissionIDs = ids
	return out, nil
}

// AgentEffectCeiling returns the chain ceiling from agentID to its root: the
// fold chainEffectCeiling computes for mint, projected by
// projectChainCeiling. Bounded hops intersect over the registry, each under
// its own Version, into a bounded V1 result; principal is the identity
// element. A local-development check failure on any hop returns
// ErrProvenanceSourceNotAllowed. Every error denies.
func (a *AuthzService) AgentEffectCeiling(ctx context.Context, agentID string, opts AgentAuthorityOptions) (store.EffectCeiling, error) {
	if agentID == "" {
		return store.EffectCeiling{}, ErrProvenanceRequest
	}
	if a == nil || a.store == nil {
		return store.EffectCeiling{}, fmt.Errorf("%w: store not available", ErrProvenanceRequest)
	}
	agent, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.EffectCeiling{}, fmt.Errorf("%w: agent %s does not exist", ErrProvenanceInactive, agentID)
		}
		return store.EffectCeiling{}, fmt.Errorf("provenance agent lookup %s: %w", agentID, err)
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return store.EffectCeiling{}, fmt.Errorf("%w: agent %s is deleted", ErrProvenanceInactive, agentID)
	}
	chain, err := a.chainEffectCeiling(ctx, agent)
	if err != nil {
		return store.EffectCeiling{}, exportProvenanceError(err)
	}
	return projectChainCeiling(chain, opts)
}

// EffectiveAgentAuthority returns the scopes agent may hold and its chain
// ceiling, computed as mint computes them:
//
//	sc, err := a.loadScopeCeilings(ctx, agent)
//	ceiling, err := projectChainCeiling(sc.Chain, opts)
//	scopes := filterScopes(a.mintCandidateScopes(agent), ceiling, sc)
//
// With zero options and a fully recorded chain the scopes equal the minted
// scopes; with AllowUnrecordedLegacy they are a subset of them. The ceiling
// is the chain ceiling only (what AgentEffectCeiling returns for the same
// options). Every error denies.
func (a *AuthzService) EffectiveAgentAuthority(ctx context.Context, agent *store.Agent, opts AgentAuthorityOptions) ([]AgentTokenScope, store.EffectCeiling, error) {
	if agent == nil || agent.ID == "" {
		return nil, store.EffectCeiling{}, ErrProvenanceRequest
	}
	if !agent.DeletedAt.IsZero() {
		return nil, store.EffectCeiling{}, fmt.Errorf("%w: agent %s is deleted", ErrProvenanceInactive, agent.ID)
	}
	sc, err := a.loadScopeCeilings(ctx, agent)
	if err != nil {
		return nil, store.EffectCeiling{}, exportProvenanceError(err)
	}
	ceiling, err := projectChainCeiling(sc.Chain, opts)
	if err != nil {
		return nil, store.EffectCeiling{}, err
	}
	return filterScopes(a.mintCandidateScopes(agent), ceiling, sc), ceiling, nil
}
