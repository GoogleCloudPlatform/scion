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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Scope re-issue (ptone/scion#3652): re-issue an agent's role scopes from its
// delegator's current authority. Operator-initiated, super-admin only,
// audited.
//
// The agent's token scopes are bounded by the frozen ceiling of its own
// delegation edge, and every mint and refresh reads that edge. Re-minting
// alone therefore cannot change the scope set; the re-issue re-records the
// agent's edge (E -> E') from the delegator's live authority, through the
// same gate agent creation uses:
//
//	role    = minRole(stored role, delegator's stored role, project max)
//	CanDelegate(delegator's live grant, role)
//	ceiling = agentRowEffectCeiling(delegator) less every permission the
//	          delegator's live chain no longer supports
//	role    = childRoleWithinCeiling(ceiling, role)   (only ever lowers)
//	scopes  = role scopes + config scopes, filtered by the fold of the new
//	          ceiling and the delegator's chain
//
// The operator's own authority authorizes the operation only; it is never
// an input to the computation. Any hop that cannot be evaluated refuses the
// whole operation; a per-permission lookup fault withholds that scope. The
// result replaces the previous set, so scopes the chain no longer supports
// are removed. Descendants are not changed: each needs its own re-issue.
//
// The re-issue never runs from refresh, start, restart, startup, the
// scheduler or reconcile.
//
// Phase 1 covers agents whose edge names an agent delegator. A user
// delegator is refused with errReissueUnsupportedDelegator.

// mintSiteReissue is the mint site of a scope re-issue.
const mintSiteReissue mintSite = "reissue"

// Audit mutation types written by a scope re-issue.
const (
	// mutationTypeAgentScopesReissued is the one record that names scopes:
	// the diff is the point of the operation. It is written in the same
	// transaction as the edge re-record (and alone for a dry run).
	mutationTypeAgentScopesReissued = "agent_scopes_reissued"
	// mutationTypeAgentScopesReissueDispatch records the post-commit token
	// dispatch: whether the new token reached the agent. It names no
	// scopes.
	mutationTypeAgentScopesReissueDispatch = "agent_scopes_reissue_dispatch"
)

// agentCredentialRevokeReasonScopesReissued is the revoke reason recorded on
// the credentials a committed re-issue revokes.
const agentCredentialRevokeReasonScopesReissued = "scopes_reissued"

// Withhold causes recorded per scope.
const (
	reissueWithheldCeiling     = "ceiling"
	reissueWithheldDelegator   = string(DenyCauseCeilingDelegatorLacksPermission)
	reissueWithheldLookup      = "lookup"
	reissueWithheldUnevaluated = "unevaluated"
)

var (
	// errReissueUnsupportedDelegator: the agent's edge names a delegator
	// kind the re-issue does not handle yet.
	errReissueUnsupportedDelegator = errors.New("scope re-issue: delegator kind not supported")
	// errReissueConflict: the agent or its edge changed while the re-issue
	// ran.
	errReissueConflict = errors.New("scope re-issue: agent or delegation edge changed concurrently")
)

// reissueWithheldScope is one candidate scope left out of the re-issued set.
type reissueWithheldScope struct {
	Scope string `json:"scope"`
	Cause string `json:"cause"`
}

// reissueCeilingSource names where the re-issued ceiling came from.
type reissueCeilingSource struct {
	DelegatorKind        string `json:"delegator_kind"`
	DelegatorID          string `json:"delegator_id"`
	SourceCredentialKind string `json:"source_credential_kind"`
	SourceCredentialID   string `json:"source_credential_id,omitempty"`
	CeilingKind          string `json:"ceiling_kind"`
}

// scopeReissuePlan is the outcome of the re-issue computation. A dry run
// and a real run compute it the same way (computeScopeReissue).
type scopeReissuePlan struct {
	agent      *store.Agent
	edge       *store.DelegationEdge // E, the edge being replaced
	roleBefore AgentRole
	roleAfter  AgentRole
	ceiling    store.EffectCeiling // the ceiling E' carries
	before     []AgentTokenScope   // what a refresh issues under E
	after      []AgentTokenScope   // what a mint issues under E'
	added      []AgentTokenScope
	removed    []AgentTokenScope
	kept       []AgentTokenScope
	withheld   []reissueWithheldScope
	source     reissueCeilingSource
	noop       bool
}

// ScopeReissueRequest is the reset-auth request body.
type ScopeReissueRequest struct {
	ReissueScopes bool `json:"reissue_scopes"`
	DryRun        bool `json:"dry_run"`
}

// ScopeReissueResponse is the response to a scope re-issue.
type ScopeReissueResponse struct {
	OpID               string                 `json:"op_id"`
	AgentID            string                 `json:"agent_id"`
	DryRun             bool                   `json:"dry_run"`
	Noop               bool                   `json:"noop"`
	Added              []string               `json:"added"`
	Removed            []string               `json:"removed"`
	Kept               []string               `json:"kept"`
	Withheld           []reissueWithheldScope `json:"withheld"`
	RoleBefore         string                 `json:"role_before"`
	RoleAfter          string                 `json:"role_after"`
	CeilingSource      reissueCeilingSource   `json:"ceiling_source"`
	EdgeReplaced       string                 `json:"edge_replaced,omitempty"`
	EdgeNew            string                 `json:"edge_new,omitempty"`
	CredentialsRevoked int                    `json:"credentials_revoked"`
	Dispatched         bool                   `json:"dispatched"`
	DispatchError      string                 `json:"dispatch_error,omitempty"`
	Message            string                 `json:"message"`
}

// reissueAuditSummary is the AfterSummary of agent_scopes_reissued.
type reissueAuditSummary struct {
	OpID               string                 `json:"op_id"`
	DryRun             bool                   `json:"dry_run"`
	ScopesAdded        []string               `json:"scopes_added"`
	ScopesRemoved      []string               `json:"scopes_removed"`
	ScopesWithheld     []reissueWithheldScope `json:"scopes_withheld"`
	RoleBefore         string                 `json:"role_before"`
	RoleAfter          string                 `json:"role_after"`
	EdgeReplaced       string                 `json:"edge_replaced"`
	EdgeNew            string                 `json:"edge_new"`
	CredentialsRevoked int                    `json:"credentials_revoked"`
	CeilingSource      reissueCeilingSource   `json:"ceiling_source"`
}

// reissueDispatchSummary is the AfterSummary of
// agent_scopes_reissue_dispatch. It names no scopes.
type reissueDispatchSummary struct {
	OpID               string `json:"op_id"`
	CredentialsRevoked int    `json:"credentials_revoked"`
	Dispatched         bool   `json:"dispatched"`
	Skipped            string `json:"skipped,omitempty"`
	ErrorClass         string `json:"error_class,omitempty"`
}

// reissueOperator is the authorized caller of a re-issue.
type reissueOperator struct {
	UserID         string
	CredentialKind string // store.InitiatorCredentialKindSession | ...DevLocal
}

// authorizeScopeReissue admits a hub super-admin on an interactive (or
// enabled local development) credential, and writes 401/403 otherwise. An
// agent token, a user access token, a federated identity and a hub admin
// without super-admin are all refused.
func (s *Server) authorizeScopeReissue(w http.ResponseWriter, r *http.Request, agentID string) (reissueOperator, bool) {
	ctx := r.Context()
	resource := Resource{Type: "agent", ID: agentID}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return reissueOperator{}, false
	}
	deny := func(reason string) (reissueOperator, bool) {
		logAuthzDenial(r, identity, resource, Action("reissue_scopes"), reason)
		writeForbidden(w, "Re-issuing an agent's scopes requires a hub super-admin session")
		return reissueOperator{}, false
	}
	credential := GetCredentialContextFromContext(ctx)
	switch credential.Kind {
	case CredentialKindInteractive, CredentialKindDev:
	default:
		return deny("credential kind may not re-issue agent scopes")
	}
	user, ok := identity.(UserIdentity)
	if !ok || isNilIdentity(user) || user.ID() == "" {
		return deny("non-user identity")
	}
	if IsScopedUserIdentity(user) {
		return deny("scoped user access token")
	}
	if _, federated := user.(FederatedIdentity); federated {
		return deny("federated identity")
	}
	kind := initiatorCredentialKindFor(identity, credential.Kind)
	switch kind {
	case store.InitiatorCredentialKindSession:
	case store.InitiatorCredentialKindDevLocal:
		if s.authzService == nil || !s.authzService.devLocalAuthorityEnabled() {
			return deny("local development authority not enabled")
		}
	default:
		return deny("credential kind may not re-issue agent scopes")
	}
	if s.authzService == nil {
		return deny("authorization service not initialized")
	}
	if !IsUnscopedLocalPlatformAdmin(user) && !s.authzService.IsSystemAdmin(ctx, user.ID()) {
		return deny("not a hub super-admin")
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("execute"),
		Permission: "hub.auth_reset.execute",
	})
	if !decision.Allowed {
		return deny("hub.auth_reset.execute denied: " + decision.Reason)
	}
	return reissueOperator{UserID: user.ID(), CredentialKind: kind}, true
}

// reissueRefusal builds the error for a whole-operation refusal.
func reissueRefusal(cause DenyCause, err error) error {
	return &agentTokenIssueError{Site: mintSiteReissue, Cause: cause, Err: err}
}

// reissueLookupFault builds the error for a lookup fault (503).
func reissueLookupFault(err error) error {
	return &agentTokenIssueError{Site: mintSiteReissue, Lookup: true, Err: fmt.Errorf("%w: %w", errMintLookup, err)}
}

// reissueErrorFromChain classifies a ceiling or provenance error from an
// existing helper into a refusal or a lookup fault. A held agent keeps its
// standing classification.
func reissueErrorFromChain(err error) error {
	var issueErr *agentTokenIssueError
	if errors.As(err, &issueErr) {
		out := *issueErr
		out.Site = mintSiteReissue
		return &out
	}
	if errors.Is(err, errAgentNotInStanding) {
		return &agentTokenIssueError{Site: mintSiteReissue, Standing: true, Err: err}
	}
	if cause, structural := ceilingDenyCauseForError(err); structural {
		return reissueRefusal(cause, err)
	}
	return reissueLookupFault(err)
}

// computeScopeReissue runs the re-issue computation for agent and returns
// the plan. It writes nothing. A dry run and a real run both call it, so
// the dry-run diff is the real one.
//
// Errors are *agentTokenIssueError (Cause for a refusal, Lookup for a
// fault, Standing for a held agent), or errReissueUnsupportedDelegator.
func (s *Server) computeScopeReissue(ctx context.Context, agent *store.Agent) (*scopeReissuePlan, error) {
	a := s.authzService
	if a == nil {
		return nil, reissueLookupFault(errors.New("authorization service not initialized"))
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return nil, reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: agent deleted", ErrProvenanceChain))
	}
	ctx = contextWithDelegationCeilingCache(ctx)

	// The single active project edge E.
	active, err := a.activeProjectEdges(ctx, agent.ID, agent.ProjectID)
	if err != nil {
		return nil, reissueLookupFault(err)
	}
	switch {
	case len(active) == 0:
		return nil, reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: agent %s", ErrProvenanceMissing, agent.ID))
	case len(active) > 1:
		return nil, reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: agent %s", ErrProvenanceAmbiguous, agent.ID))
	}
	edge := active[0]
	// An edge with no recorded provenance (the backfill sentinel, an
	// unrecorded ceiling, or a provenance version this binary does not
	// understand) is refused: there is no verified source to re-record
	// from. The agent must be recreated.
	if isMigrationSentinel(edge) || edge.Kind == store.EffectCeilingUnrecorded || !knownProvenanceVersion(edge.ProvenanceVersion) {
		return nil, reissueRefusal(DenyCauseCeilingUnrecorded, fmt.Errorf("%w: edge %s has no recorded provenance", errSourceCeilingUnrecorded, edge.ID))
	}

	storedRole, _ := agentRoleAndScopes(agent)
	if !ValidAgentRole(storedRole) {
		return nil, reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: agent %s has invalid stored role %q", ErrProvenanceChain, agent.ID, storedRole))
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: project %s not found", ErrProvenanceChain, agent.ProjectID))
		}
		return nil, reissueLookupFault(fmt.Errorf("project lookup: %w", err))
	}
	projectMax := projectMaxAgentRole(project)

	// The refresh-equivalent "before": what a mint issues under E now.
	// Credentials do not store their scopes, so this is the honest
	// baseline. It also refuses a held agent.
	beforeGrant, err := s.AuthorizeAgentToken(ctx, agent)
	if err != nil {
		return nil, reissueErrorFromChain(err)
	}

	plan := &scopeReissuePlan{
		agent:      agent,
		edge:       edge,
		roleBefore: storedRole,
		before:     beforeGrant.Scopes,
	}

	switch edge.DelegatorType {
	case store.DelegationPrincipalAgent:
		if err := s.planAgentDelegatorReissue(ctx, plan, storedRole, projectMax); err != nil {
			return nil, err
		}
	case store.DelegationPrincipalUser:
		return nil, errReissueUnsupportedDelegator
	default:
		return nil, reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: edge %s has delegator type %q", ErrProvenanceChain, edge.ID, edge.DelegatorType))
	}

	plan.added, plan.removed, plan.kept = diffScopes(plan.before, plan.after)
	plan.noop = len(plan.added) == 0 && len(plan.removed) == 0 &&
		plan.roleAfter == plan.roleBefore &&
		effectCeilingsEqual(plan.ceiling, edge.EffectCeiling)
	return plan, nil
}

// planAgentDelegatorReissue fills plan for an edge whose delegator is the
// live parent agent P.
func (s *Server) planAgentDelegatorReissue(ctx context.Context, plan *scopeReissuePlan, storedRole, projectMax AgentRole) error {
	a := s.authzService
	agent, edge := plan.agent, plan.edge

	// P must exist and not be deleted. A lookup error refuses the
	// operation; it never falls back to a default role.
	parent, err := s.store.GetAgent(ctx, edge.DelegatorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: delegator agent %s not found", ErrProvenanceChain, edge.DelegatorID))
		}
		return reissueLookupFault(fmt.Errorf("delegator agent lookup: %w", err))
	}
	if parent == nil || !parent.DeletedAt.IsZero() {
		return reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: delegator agent %s deleted", ErrProvenanceChain, edge.DelegatorID))
	}
	if parent.ProjectID != agent.ProjectID {
		return reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: delegator agent %s is in another project", ErrProvenanceChain, parent.ID))
	}
	parentRole, _ := agentRoleAndScopes(parent)
	if !ValidAgentRole(parentRole) {
		return reissueRefusal(DenyCauseCeilingOrphaned, fmt.Errorf("%w: delegator agent %s has invalid stored role %q", ErrProvenanceChain, parent.ID, parentRole))
	}
	role := minRole(storedRole, parentRole, projectMax)

	// P's live grant: what a mint for P issues now. It also refuses when
	// P is held or P's own chain is invalid.
	parentGrant, err := s.AuthorizeAgentToken(ctx, parent)
	if err != nil {
		return reissueErrorFromChain(err)
	}
	actor := &agentIdentityWrapper{AgentTokenClaims: &AgentTokenClaims{
		ProjectID: parent.ProjectID,
		Scopes:    parentGrant.Scopes,
		Ancestry:  parent.Ancestry,
	}}
	actor.Subject = parent.ID

	// The ceiling P's authority-producing writes carry now: P's current
	// coverage, bounded by P's own edge, plus delivery eligibility.
	ceiling, err := a.agentRowEffectCeiling(ctx, parent)
	if err != nil {
		return reissueErrorFromChain(err)
	}
	parentChain, err := a.chainEffectCeiling(ctx, parent)
	if err != nil {
		return reissueErrorFromChain(err)
	}

	// The live check of P's chain, per permission, narrows the ceiling:
	// a permission the chain no longer supports is removed from E' so
	// refresh after the re-issue issues exactly the re-issued set.
	liveDenied := map[string]string{}
	liveCheck := func(perm string) string {
		if cause, seen := liveDenied[perm]; seen {
			return cause
		}
		cause := s.reissueAgentDelegatorLive(ctx, agent, parent, perm)
		liveDenied[perm] = cause
		return cause
	}

	// Cap the role and narrow the ceiling until both are stable. The role
	// only goes down, so this ends after at most one pass per role level.
	var candidates []AgentTokenScope
	for iter := 0; ; iter++ {
		if iter > 4 {
			return reissueLookupFault(errors.New("scope re-issue: role did not settle"))
		}
		decision := a.CanDelegate(ctx, actor, GrantDescriptor{
			Type:      GrantTypeAgentDelegation,
			AgentRole: string(role),
			ProjectID: agent.ProjectID,
			ScopeType: store.RoleScopeProject,
			ScopeID:   agent.ProjectID,
		})
		if !decision.Allowed {
			return reissueRefusal(DenyCauseCeilingDelegatorLacksPermission, fmt.Errorf("delegator agent %s cannot delegate role %q: %s", parent.ID, role, decision.Reason))
		}
		capped, cause, ok := childRoleWithinCeiling(ceiling, role, false)
		if !ok {
			return reissueRefusal(cause, fmt.Errorf("no agent role fits the delegator's current ceiling"))
		}
		role = capped
		candidates = reissueCandidateScopes(a, agent, role)

		narrowed := false
		for _, scope := range candidates {
			for _, perm := range scopeCeilingPermissions(scope) {
				if selfOperationSet[perm] || !containsString(ceiling.PermissionIDs, perm) {
					continue
				}
				if liveCheck(perm) != "" {
					ceiling.PermissionIDs = removeString(ceiling.PermissionIDs, perm)
					narrowed = true
				}
			}
		}
		if !narrowed {
			break
		}
	}

	// The projected chain ceiling after E' commits: E' folded with P's
	// chain. It is what chainEffectCeiling(agent) returns then.
	projected := foldWithChain(ceiling, parentChain)
	after := filterScopes(candidates, projected, ScopeCeilings{})
	afterSet := scopeSet(after)
	var withheld []reissueWithheldScope
	for _, scope := range candidates {
		if afterSet[scope] {
			continue
		}
		withheld = append(withheld, reissueWithheldScope{Scope: string(scope), Cause: reissueWithholdCause(scope, liveDenied)})
	}

	plan.roleAfter = role
	plan.ceiling = ceiling
	plan.after = after
	plan.withheld = withheld
	plan.source = reissueCeilingSource{
		DelegatorKind:        store.DelegationPrincipalAgent,
		DelegatorID:          parent.ID,
		SourceCredentialKind: string(edge.SourceCredentialKind),
		SourceCredentialID:   edge.SourceCredentialID,
		CeilingKind:          string(ceiling.Kind),
	}
	return nil
}

// reissueAgentDelegatorLive checks permission perm against the live chain
// above agent: the parent P holds perm through its stored role scopes, and
// the walk from P (every ancestor live and holding perm, every hop's frozen
// ceiling) allows it. It returns "" when allowed, otherwise the withhold
// cause. A lookup fault is never read as allowed.
func (s *Server) reissueAgentDelegatorLive(ctx context.Context, agent, parent *store.Agent, perm string) string {
	a := s.authzService
	if reissueLiveCheckFault != nil {
		if err := reissueLiveCheckFault(perm); err != nil {
			return reissueWithheldLookup
		}
	}
	held, _, err := a.checkAgentHoldsPermission(ctx, parent.ID, perm, store.RoleScopeProject, agent.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return reissueWithheldDelegator
		}
		return reissueWithheldLookup
	}
	if !held {
		return reissueWithheldDelegator
	}
	resource, action, ok := reissuePermissionTarget(agent, perm)
	if !ok {
		return reissueWithheldUnevaluated
	}
	var cause DenyCause
	allowed, _, err := a.walkDelegationChainWithCause(ctx, resource, action, perm, parent.ID, true,
		store.RoleScopeProject, agent.ProjectID, nil, &cause)
	if err != nil {
		return reissueWithheldLookup
	}
	if !allowed {
		switch cause {
		case DenyCauseResolutionError, DenyCauseCeilingError:
			return reissueWithheldLookup
		case "":
			return reissueWithheldDelegator
		default:
			return string(cause)
		}
	}
	return ""
}

// reissueLiveCheckFault is a test seam: when set, a non-nil error for perm
// is treated as a lookup fault of the live check. Nil in production.
var reissueLiveCheckFault func(perm string) error

// reissuePermissionTarget returns the resource and action the live check
// evaluates perm on: the agent itself for an agent permission, otherwise a
// resource of the permission's type in the agent's project. ok is false for
// an unregistered permission.
func reissuePermissionTarget(agent *store.Agent, perm string) (Resource, Action, bool) {
	for _, p := range permissions.Registry {
		if p.ID != perm {
			continue
		}
		if p.Resource == "agent" {
			return Resource{
				Type: "agent", ID: agent.ID, OwnerID: agent.OwnerID,
				ParentType: "project", ParentID: agent.ProjectID,
				Ancestry: agent.Ancestry,
			}, Action(p.Action), true
		}
		return Resource{Type: p.Resource, ParentType: "project", ParentID: agent.ProjectID}, Action(p.Action), true
	}
	return Resource{}, "", false
}

// reissueCandidateScopes returns the mint candidates for agent at role: the
// role scopes plus the config-derived scopes (mintCandidateScopes on a copy
// of the row carrying role).
func reissueCandidateScopes(a *AuthzService, agent *store.Agent, role AgentRole) []AgentTokenScope {
	row := *agent
	cfg := store.AgentAppliedConfig{}
	if agent.AppliedConfig != nil {
		cfg = *agent.AppliedConfig
	}
	cfg.AgentRole = string(role)
	row.AppliedConfig = &cfg
	return a.mintCandidateScopes(&row)
}

// scopeCeilingPermissions returns the registry permissions ceilingAllowsScope
// consults for scope: its coverage, or the reviewed mapping for a scope with
// no coverage.
func scopeCeilingPermissions(scope AgentTokenScope) []string {
	coverage := agentScopeCoverage([]AgentTokenScope{scope})
	if len(coverage) > 0 {
		return coverage
	}
	if perm, ok := zeroCoverageMappedPermission(scope); ok {
		return []string{perm}
	}
	return nil
}

// reissueWithholdCause names why scope is not in the re-issued set: the
// first live-check cause among its permissions, else the ceiling.
func reissueWithholdCause(scope AgentTokenScope, liveDenied map[string]string) string {
	for _, perm := range scopeCeilingPermissions(scope) {
		if cause := liveDenied[perm]; cause != "" {
			return cause
		}
	}
	return reissueWithheldCeiling
}

// foldWithChain folds a bounded edge ceiling with the chain ceiling above
// it, the way chainEffectCeiling folds the hops.
func foldWithChain(edgeCeiling store.EffectCeiling, chain ChainCeiling) store.EffectCeiling {
	var bounded []permissions.FrozenPermissionCeiling
	if f, ok := edgeCeiling.Frozen(); ok {
		bounded = append(bounded, f)
	}
	if f, ok := chain.Ceiling.Frozen(); ok {
		bounded = append(bounded, f)
	}
	return foldCeilings(bounded, chain.UnrecordedHops)
}

// effectCeilingsEqual compares the authorization-relevant fields of two
// ceilings.
func effectCeilingsEqual(a, b store.EffectCeiling) bool {
	if a.Kind != b.Kind || a.Version != b.Version || a.BoundaryKind != b.BoundaryKind || a.BoundaryProjectID != b.BoundaryProjectID {
		return false
	}
	x, y := sortedUniqueIDs(a.PermissionIDs), sortedUniqueIDs(b.PermissionIDs)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// diffScopes returns the scopes of after not in before (added), of before
// not in after (removed), and in both (kept), each sorted.
func diffScopes(before, after []AgentTokenScope) (added, removed, kept []AgentTokenScope) {
	b, a := scopeSet(before), scopeSet(after)
	for s := range a {
		if b[s] {
			kept = append(kept, s)
		} else {
			added = append(added, s)
		}
	}
	for s := range b {
		if !a[s] {
			removed = append(removed, s)
		}
	}
	sortScopes(added)
	sortScopes(removed)
	sortScopes(kept)
	return added, removed, kept
}

func scopeSet(scopes []AgentTokenScope) map[AgentTokenScope]bool {
	set := make(map[AgentTokenScope]bool, len(scopes))
	for _, s := range scopes {
		set[s] = true
	}
	return set
}

func sortScopes(scopes []AgentTokenScope) {
	sort.Slice(scopes, func(i, j int) bool { return scopes[i] < scopes[j] })
}

func scopeStrings(scopes []AgentTokenScope) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, string(s))
	}
	return out
}

func removeString(list []string, v string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

// reissueAuditRecord builds the agent_scopes_reissued record for plan.
func reissueAuditRecord(plan *scopeReissuePlan, opID string, dryRun bool, edgeNew string, revoked int, actor AuditActor, now time.Time) (*store.MutationAuditRecord, error) {
	withheld := plan.withheld
	if withheld == nil {
		withheld = []reissueWithheldScope{}
	}
	return lifecycleAudit(mutationTypeAgentScopesReissued, plan.agent.ID, actor, now, reissueAuditSummary{
		OpID:               opID,
		DryRun:             dryRun,
		ScopesAdded:        scopeStrings(plan.added),
		ScopesRemoved:      scopeStrings(plan.removed),
		ScopesWithheld:     withheld,
		RoleBefore:         string(plan.roleBefore),
		RoleAfter:          string(plan.roleAfter),
		EdgeReplaced:       plan.edge.ID,
		EdgeNew:            edgeNew,
		CredentialsRevoked: revoked,
		CeilingSource:      plan.source,
	})
}

// commitScopeReissue writes the re-issue in one transaction: deactivate E
// (guarded on its updated time, so a concurrent change refuses), create E',
// lower the stored role when it went down, revoke every active credential
// of the agent, and write the agent_scopes_reissued record. Nothing is
// written when any step fails. It returns E' and the revoked count.
func (s *Server) commitScopeReissue(ctx context.Context, plan *scopeReissuePlan, operator reissueOperator, opID string) (*store.DelegationEdge, int, error) {
	actor := auditActorFromContext(ctx)
	now := time.Now()
	old := plan.edge
	newEdge := &store.DelegationEdge{
		DelegatorType: old.DelegatorType,
		DelegatorID:   old.DelegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    plan.agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       plan.agent.ProjectID,
		Role:          string(plan.roleAfter),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:           store.ProvenanceVersionV1,
			SourcePrincipalKind:         old.SourcePrincipalKind,
			SourcePrincipalID:           old.SourcePrincipalID,
			SourceCredentialKind:        old.SourceCredentialKind,
			SourceCredentialID:          old.SourceCredentialID,
			SourceEventID:               old.SourceEventID,
			SourceScheduleID:            old.SourceScheduleID,
			SourceAuthorizationRevision: old.SourceAuthorizationRevision,
			InitiatorPrincipalKind:      store.DelegationPrincipalUser,
			InitiatorPrincipalID:        operator.UserID,
			InitiatorCredentialKind:     operator.CredentialKind,
		},
		EffectCeiling: plan.ceiling,
	}
	revoked := 0
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		updatedAt := old.UpdatedAt
		ok, err := tx.DeactivateDelegationEdgeGuarded(ctx, old.ID, store.DelegationEdgeDeactivateGuard{
			Recorded:  true,
			UpdatedAt: &updatedAt,
		}, store.EdgeDeactivationScopeReissueReplaced, opID)
		if err != nil {
			return fmt.Errorf("scope re-issue: deactivate edge: %w", err)
		}
		if !ok {
			return errReissueConflict
		}
		if err := tx.CreateDelegationEdge(ctx, newEdge); err != nil {
			return fmt.Errorf("scope re-issue: record edge: %w", err)
		}
		cur, err := tx.GetAgent(ctx, plan.agent.ID)
		if err != nil {
			return fmt.Errorf("scope re-issue: re-read agent: %w", err)
		}
		curRole, _ := agentRoleAndScopes(cur)
		if !cur.DeletedAt.IsZero() || curRole != plan.roleBefore {
			return errReissueConflict
		}
		if plan.roleAfter != plan.roleBefore {
			row := *cur
			cfg := store.AgentAppliedConfig{}
			if cur.AppliedConfig != nil {
				cfg = *cur.AppliedConfig
			}
			cfg.AgentRole = string(plan.roleAfter)
			row.AppliedConfig = &cfg
			if err := tx.UpdateAgent(ctx, &row); err != nil {
				if errors.Is(err, store.ErrVersionConflict) {
					return errReissueConflict
				}
				return fmt.Errorf("scope re-issue: lower stored role: %w", err)
			}
		}
		n, err := tx.RevokeAgentCredentialsByAgent(ctx, plan.agent.ID, "system", agentCredentialRevokeReasonScopesReissued)
		if err != nil {
			return fmt.Errorf("scope re-issue: revoke credentials: %w", err)
		}
		revoked = n
		record, err := reissueAuditRecord(plan, opID, false, newEdge.ID, revoked, actor, now)
		if err != nil {
			return err
		}
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("scope re-issue audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return newEdge, revoked, nil
}

// recordReissueDispatch writes the agent_scopes_reissue_dispatch record. A
// write failure is logged; the commit has already happened.
func recordReissueDispatch(ctx context.Context, st store.Store, agentID string, summary reissueDispatchSummary) {
	record, err := lifecycleAudit(mutationTypeAgentScopesReissueDispatch, agentID, auditActorFromContext(ctx), time.Now(), summary)
	if err == nil {
		err = st.CreateMutationAudit(ctx, record)
	}
	if err != nil {
		slog.ErrorContext(ctx, "scope re-issue: dispatch audit write failed", "agent_id", agentID, "op_id", summary.OpID, "error", err)
	}
}

// reissueDispatchErrorClass is the error class recorded for a failed
// post-commit dispatch.
func reissueDispatchErrorClass(err error) string {
	var issueErr *agentTokenIssueError
	switch {
	case errors.As(err, &issueErr):
		return issueErr.errorClass()
	case errors.Is(err, errAgentTokenRecord):
		return "credential_not_recorded"
	case isBrokerRuntimeUnavailable(err):
		return brokerCodeRuntimeUnavailable
	default:
		return "dispatch_failed"
	}
}

// runScopeReissue runs a re-issue for agent: compute, then for a dry run
// write the dry-run record; for a no-op write nothing; otherwise commit and
// dispatch the new token to a running agent.
func (s *Server) runScopeReissue(ctx context.Context, agent *store.Agent, operator reissueOperator, dryRun bool) (*ScopeReissueResponse, error) {
	opID := api.NewUUID()
	plan, err := s.computeScopeReissue(ctx, agent)
	if err != nil {
		var issueErr *agentTokenIssueError
		if errors.As(err, &issueErr) {
			// Scope-free denial record: {site, deny_cause} only.
			recordAgentTokenIssueDenied(ctx, s.store, agent, issueErr)
		}
		return nil, err
	}
	resp := &ScopeReissueResponse{
		OpID:          opID,
		AgentID:       agent.ID,
		DryRun:        dryRun,
		Noop:          plan.noop,
		Added:         scopeStrings(plan.added),
		Removed:       scopeStrings(plan.removed),
		Kept:          scopeStrings(plan.kept),
		Withheld:      plan.withheld,
		RoleBefore:    string(plan.roleBefore),
		RoleAfter:     string(plan.roleAfter),
		CeilingSource: plan.source,
		EdgeReplaced:  plan.edge.ID,
	}
	if resp.Withheld == nil {
		resp.Withheld = []reissueWithheldScope{}
	}

	if dryRun {
		record, err := reissueAuditRecord(plan, opID, true, "", 0, auditActorFromContext(ctx), time.Now())
		if err == nil {
			err = s.store.CreateMutationAudit(ctx, record)
		}
		if err != nil {
			return nil, fmt.Errorf("scope re-issue dry-run audit: %w", err)
		}
		resp.Message = "Dry run: nothing was changed"
		return resp, nil
	}
	if plan.noop {
		resp.Message = "No change: the agent's scopes already match its delegator's current authority"
		return resp, nil
	}

	newEdge, revoked, err := s.commitScopeReissue(ctx, plan, operator, opID)
	if err != nil {
		return nil, err
	}
	resp.EdgeNew = newEdge.ID
	resp.CredentialsRevoked = revoked

	summary := reissueDispatchSummary{OpID: opID, CredentialsRevoked: revoked}
	fresh, err := s.store.GetAgent(ctx, agent.ID)
	if err != nil {
		summary.ErrorClass = mintErrorClassLookup
		recordReissueDispatch(ctx, s.store, agent.ID, summary)
		resp.DispatchError = "the agent could not be re-read after the re-issue; run reset-auth to deliver a new token"
		resp.Message = "Scopes re-issued; the new token was not delivered"
		return resp, nil
	}
	if fresh.Phase != string(state.PhaseRunning) || fresh.RuntimeBrokerID == "" {
		summary.Skipped = "agent_not_running"
		recordReissueDispatch(ctx, s.store, agent.ID, summary)
		resp.Message = "Scopes re-issued; the agent receives them at its next start"
		return resp, nil
	}
	disp := s.GetDispatcher()
	if disp == nil {
		summary.ErrorClass = "dispatcher_not_configured"
		recordReissueDispatch(ctx, s.store, agent.ID, summary)
		resp.DispatchError = "agent dispatcher not configured; run reset-auth to deliver a new token"
		resp.Message = "Scopes re-issued; the new token was not delivered"
		return resp, nil
	}
	// The dispatch mints from E' (it reads the committed edge) and pushes.
	if err := disp.DispatchAgentResetAuth(withMintSite(ctx, mintSiteReissue), fresh); err != nil {
		slog.ErrorContext(ctx, "scope re-issue: token dispatch failed", "agent_id", agent.ID, "op_id", opID, "error", err)
		summary.ErrorClass = reissueDispatchErrorClass(err)
		recordReissueDispatch(ctx, s.store, agent.ID, summary)
		resp.DispatchError = "the new token could not be delivered (" + summary.ErrorClass + "); the agent has no valid token until reset-auth succeeds"
		resp.Message = "Scopes re-issued; the new token was not delivered"
		return resp, nil
	}
	summary.Dispatched = true
	recordReissueDispatch(ctx, s.store, agent.ID, summary)
	resp.Dispatched = true
	resp.Message = "Scopes re-issued and a new token dispatched"
	return resp, nil
}

// handleAgentScopeReissue handles POST .../reset-auth with
// {"reissue_scopes": true}.
func (s *Server) handleAgentScopeReissue(w http.ResponseWriter, r *http.Request, id string, req ScopeReissueRequest) {
	ctx := r.Context()
	operator, ok := s.authorizeScopeReissue(w, r, id)
	if !ok {
		return
	}
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	resp, err := s.runScopeReissue(ctx, agent, operator, req.DryRun)
	if err != nil {
		writeScopeReissueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeScopeReissueError answers a failed re-issue.
func writeScopeReissueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errReissueUnsupportedDelegator):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidRequest,
			"Scope re-issue currently supports only agents created by another agent", nil)
	case errors.Is(err, errReissueConflict), errors.Is(err, store.ErrVersionConflict), errors.Is(err, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"The agent or its delegation record changed during the re-issue; retry", nil)
	case writeAgentTokenIssueError(w, err):
	default:
		slog.Error("scope re-issue failed", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "scope re-issue failed", nil)
	}
}

// mintSiteContextKey carries the mint site a dispatch records on a denial.
type mintSiteContextKey struct{}

// withMintSite returns ctx carrying site for DispatchAgentResetAuth.
func withMintSite(ctx context.Context, site mintSite) context.Context {
	return context.WithValue(ctx, mintSiteContextKey{}, site)
}

// mintSiteFromContext returns the site carried by ctx, or def.
func mintSiteFromContext(ctx context.Context, def mintSite) mintSite {
	if site, ok := ctx.Value(mintSiteContextKey{}).(mintSite); ok && site != "" {
		return site
	}
	return def
}

// decodeScopeReissueRequest reads the optional reset-auth body. An empty
// body is a plain reset-auth.
func decodeScopeReissueRequest(r *http.Request) (ScopeReissueRequest, error) {
	var req ScopeReissueRequest
	if r.Body == nil || r.ContentLength == 0 {
		return req, nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return ScopeReissueRequest{}, nil
		}
		return req, err
	}
	return req, nil
}
