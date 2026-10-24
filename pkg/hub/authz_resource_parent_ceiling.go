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

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the resource-aware parent ceiling for agent use of a GCP
// service account. The decision permission for minting a GCP token through
// an assignment is gcp_service_account.use. Its parent proof is that the
// principal recorded as the assignment's source currently holds
// gcp_service_account.assign on that exact, freshly loaded service account,
// within the effect ceiling recorded with the assignment. A past successful
// assignment is not enough, and a configured default grants nothing.
//
// The evaluator here is an authority proof only. It does not check GCP IAM
// actAs, write anything, or emit a decision audit record. Decide runs it as
// step 10 for gcp_service_account.use (applyResourceParentCeiling), so the
// one audited decision has permission gcp_service_account.use and names
// Proof.Permission as the parent proof. The GCP token mint handlers compose
// that decision with the mode-aware actAs check on the recorded source
// (authorizeAgentGCPMint, handlers_gcp_identity_mint_authority.go). The GCS
// link proxy, which uses the sending agent's account without the agent's
// credential, runs the same step 10 through
// decideStoredAgentServiceAccountUse and composes it with the same actAs
// check.

const (
	gcpServiceAccountUsePermission    = "gcp_service_account.use"
	gcpServiceAccountAssignPermission = "gcp_service_account.assign"
)

// ResourceParentCeiling maps a decision permission on a resource to the
// parent permission the recorded source must live-hold on the same resource.
type ResourceParentCeiling struct {
	DecisionPermission string // the decision and audit permission
	ParentPermission   string // the parent proof
	ResourceType       string // the resource both permissions apply to
}

// resourceParentCeilings is the reviewed mapping table.
var resourceParentCeilings = []ResourceParentCeiling{
	{
		DecisionPermission: gcpServiceAccountUsePermission,
		ParentPermission:   gcpServiceAccountAssignPermission,
		ResourceType:       "gcp_service_account",
	},
}

// ResourceParentCeilingFor returns the reviewed parent-ceiling mapping for
// permissionID. false means no parent ceiling is defined for it; a caller
// must never read false as an allow.
func ResourceParentCeilingFor(permissionID string) (ResourceParentCeiling, bool) {
	for _, m := range resourceParentCeilings {
		if m.DecisionPermission == permissionID {
			return m, true
		}
	}
	return ResourceParentCeiling{}, false
}

// ParentProof describes the parent authority a parent-ceiling decision
// checked: the permission, the recorded source and the assignment.
type ParentProof struct {
	Permission           string
	SourcePrincipal      ProvenancePrincipal
	SourceCredentialKind store.SourceCredentialKind
	AssignmentID         string
	ServiceAccountID     string
}

// ParentCeilingDecision is the result of a parent-ceiling evaluation.
// DeniedBy is DeniedByDelegationCeiling on every deny. Reason is neutral
// and safe to log. Proof is populated on allow, and on deny as far as known.
type ParentCeilingDecision struct {
	Allowed   bool
	DeniedBy  DeniedBy
	DenyCause DenyCause
	Reason    string
	Proof     ParentProof
}

func parentCeilingDeny(cause DenyCause, reason string, proof ParentProof) ParentCeilingDecision {
	return ParentCeilingDecision{
		DeniedBy:  DeniedByDelegationCeiling,
		DenyCause: cause,
		Reason:    reason,
		Proof:     proof,
	}
}

// assignmentProvenanceRecorded reports whether an assignment carries
// provenance this binary can read: a known provenance version and a bounded
// or principal ceiling.
func assignmentProvenanceRecorded(row store.AgentServiceAccountAssignment) bool {
	if !knownProvenanceVersion(row.ProvenanceVersion) {
		return false
	}
	switch row.Kind {
	case store.EffectCeilingBounded, store.EffectCeilingPrincipal:
		return true
	default:
		return false
	}
}

// errAssignmentSourceMismatch marks an assignment whose recorded source is
// not internally consistent.
var errAssignmentSourceMismatch = errors.New("service-account assignment: recorded source is inconsistent")

// checkAssignmentSourcePrincipal applies the principal-typing rule to an
// assignment's recorded source, as checkEdgePrincipal does for an edge. The
// principal comes from SourcePrincipalKind, never from probing an ID:
//   - session and uat sources are users, agent sources are agents;
//   - a dev_local source is user DevUserID;
//   - a scheduler source carries its revision reference, and its recorded
//     initiator is the source principal (a dev_local initiator: the local
//     development user);
//   - any other credential kind is not accepted (errSourceNotAllowed).
func checkAssignmentSourcePrincipal(row store.AgentServiceAccountAssignment) error {
	if row.SourcePrincipalID == "" {
		return fmt.Errorf("%w: no source principal", errAssignmentSourceMismatch)
	}
	kind := row.SourcePrincipalKind
	switch row.SourceCredentialKind {
	case store.SourceCredentialSession, store.SourceCredentialUAT:
		if kind != store.DelegationPrincipalUser {
			return fmt.Errorf("%w: %s source of kind %q", errAssignmentSourceMismatch, row.SourceCredentialKind, kind)
		}
	case store.SourceCredentialAgent:
		if kind != store.DelegationPrincipalAgent {
			return fmt.Errorf("%w: agent source of kind %q", errAssignmentSourceMismatch, kind)
		}
	case store.SourceCredentialDevLocal:
		if kind != store.DelegationPrincipalUser || row.SourcePrincipalID != DevUserID {
			return fmt.Errorf("%w: local development source with another principal", errAssignmentSourceMismatch)
		}
	case store.SourceCredentialScheduler:
		if row.SourceEventID == "" || row.SourceAuthorizationRevision < 1 {
			return fmt.Errorf("%w: scheduler source without a revision reference", errAssignmentSourceMismatch)
		}
		switch row.InitiatorCredentialKind {
		case store.InitiatorCredentialKindSession, store.InitiatorCredentialKindUAT, store.InitiatorCredentialKindAgent:
			if kind != store.DelegationPrincipalUser && kind != store.DelegationPrincipalAgent {
				return fmt.Errorf("%w: scheduler source of kind %q", errAssignmentSourceMismatch, kind)
			}
			if row.InitiatorPrincipalKind != kind || row.InitiatorPrincipalID != row.SourcePrincipalID {
				return fmt.Errorf("%w: scheduler initiator is not the source", errAssignmentSourceMismatch)
			}
		case store.InitiatorCredentialKindDevLocal:
			if row.InitiatorPrincipalKind != string(PrincipalKindDev) || row.InitiatorPrincipalID != DevUserID ||
				kind != store.DelegationPrincipalUser || row.SourcePrincipalID != DevUserID {
				return fmt.Errorf("%w: scheduler local development source with another principal", errAssignmentSourceMismatch)
			}
		default:
			return fmt.Errorf("%w: scheduler initiator credential %q", errAssignmentSourceMismatch, row.InitiatorCredentialKind)
		}
	default:
		return fmt.Errorf("%w: source credential %q", errSourceNotAllowed, row.SourceCredentialKind)
	}
	return nil
}

// EvaluateServiceAccountParentCeiling proves that the recorded source of the
// agent's active service-account assignment currently holds
// gcp_service_account.assign on exactly serviceAccountID, freshly loaded,
// within the effect ceiling recorded with the assignment. Steps, each of
// which denies with the given cause:
//  1. the agent loads and is not deleted (ceiling_orphaned);
//  2. it has exactly one active assignment (none: ceiling_provenance_missing;
//     more: ceiling_provenance_ambiguous);
//  3. the assignment, the agent's applied assign-mode identity and
//     serviceAccountID name the same account (ceiling_provenance_stale), and
//     the assignment's provenance is recorded and well-formed
//     (ceiling_provenance_missing, ceiling_orphaned,
//     ceiling_source_not_allowed);
//  4. the service account loads and passes saAssignPolicyPreconditions for
//     the agent's project (ceiling_resource_missing);
//  5. the recorded ceiling allows gcp_service_account.assign
//     (ceiling_effect_exceeded);
//  6. the source is live: an active user (local development sources also
//     need dev authority enabled, ceiling_source_not_allowed), or an agent
//     that is not deleted and whose own chain resolves (ceiling_orphaned);
//  7. a kernel decision for the source on the account with ActionAssign
//     allows (ceiling_delegator_lacks_permission). For a source recorded
//     from an access token the user's live authority is evaluated, bounded
//     by the recorded ceiling of step 5; the token's later revocation or
//     expiry does not reopen the assignment.
//
// Any lookup error denies with ceiling_error. It never returns Allowed on an
// error, does not read the assignment's Origin, does not check actAs, writes
// nothing and emits no audit record.
func (a *AuthzService) EvaluateServiceAccountParentCeiling(ctx context.Context, agentID, serviceAccountID string) ParentCeilingDecision {
	proof := ParentProof{Permission: gcpServiceAccountAssignPermission, ServiceAccountID: serviceAccountID}
	if a == nil || a.store == nil || agentID == "" || serviceAccountID == "" {
		return parentCeilingDeny(DenyCauseCeilingError, "invalid parent ceiling request", proof)
	}

	// 1. The agent, loaded fresh.
	agent, err := a.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return parentCeilingDeny(DenyCauseCeilingOrphaned, "agent does not exist", proof)
		}
		return parentCeilingDeny(DenyCauseCeilingError, "agent lookup failed", proof)
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return parentCeilingDeny(DenyCauseCeilingOrphaned, "agent does not exist", proof)
	}

	// 2. The active assignment.
	rows, err := a.store.GetActiveAgentServiceAccountAssignments(ctx, agent.ID)
	if err != nil {
		return parentCeilingDeny(DenyCauseCeilingError, "service-account assignment lookup failed", proof)
	}
	switch len(rows) {
	case 0:
		return parentCeilingDeny(DenyCauseCeilingProvenanceMissing, "no recorded service-account assignment", proof)
	case 1:
	default:
		return parentCeilingDeny(DenyCauseCeilingProvenanceAmbiguous, "more than one active service-account assignment", proof)
	}
	row := rows[0]
	proof.AssignmentID = row.ID
	proof.SourcePrincipal = ProvenancePrincipal{Kind: row.SourcePrincipalKind, ID: row.SourcePrincipalID}
	proof.SourceCredentialKind = row.SourceCredentialKind

	// 3. Freshness and recorded provenance. Rows come from the active-only
	// lookup, so !row.Active is defence in depth.
	if !row.Active || row.ServiceAccountID != serviceAccountID ||
		agentAssignedServiceAccountID(agent) != serviceAccountID {
		return parentCeilingDeny(DenyCauseCeilingProvenanceStale, "recorded service account does not match the account in use", proof)
	}
	if !assignmentProvenanceRecorded(row) {
		return parentCeilingDeny(DenyCauseCeilingProvenanceMissing, "service-account assignment provenance is not recorded", proof)
	}
	if err := checkAssignmentSourcePrincipal(row); err != nil {
		if errors.Is(err, errSourceNotAllowed) {
			return parentCeilingDeny(DenyCauseCeilingSourceNotAllowed, "recorded source credential is not accepted", proof)
		}
		return parentCeilingDeny(DenyCauseCeilingOrphaned, "recorded source is inconsistent", proof)
	}

	// 4. The service account, loaded fresh, under the shared rules.
	sa, err := a.store.GetGCPServiceAccount(ctx, serviceAccountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return parentCeilingDeny(DenyCauseCeilingResourceMissing, "service account does not exist", proof)
		}
		return parentCeilingDeny(DenyCauseCeilingError, "service account lookup failed", proof)
	}
	if sa == nil {
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "service account does not exist", proof)
	}
	mode := ""
	if a.saIAMCheckMode != nil {
		mode = a.saIAMCheckMode()
	}
	if pre := saAssignPolicyPreconditions(sa, agent.ProjectID, mode); pre != nil {
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "service account is not usable: "+string(pre.Kind), proof)
	}

	// 5. The recorded effect ceiling.
	if !EffectCeilingAllows(row.EffectCeiling, gcpServiceAccountAssignPermission, false) {
		return parentCeilingDeny(DenyCauseCeilingEffectExceeded, "recorded ceiling does not allow assigning the service account", proof)
	}

	// 6. The source, live.
	source, cause, reason := a.parentCeilingSourceIdentity(ctx, row)
	if cause != "" {
		return parentCeilingDeny(cause, reason, proof)
	}

	// 7. Live assign authority on this exact account. The kernel runs in
	// full; only the decision audit wrapper is skipped, since the caller
	// records the one decision for gcp_service_account.use.
	evalCtx := maskAuthzInputs(ctx)
	decision := a.decide(evalCtx, AuthzRequest{
		Principal:  principalContextForIdentity(source),
		Credential: credentialContextForIdentity(source),
		Resource:   gcpServiceAccountResource(sa),
		Action:     ActionAssign,
	})
	if !decision.Allowed {
		if decision.DenyCause == DenyCauseCeilingError || decision.IsIndeterminate() {
			return parentCeilingDeny(DenyCauseCeilingError, "source authority could not be evaluated", proof)
		}
		return parentCeilingDeny(DenyCauseCeilingDelegatorLacksPermission, "recorded source does not hold assign on the service account", proof)
	}
	return ParentCeilingDecision{Allowed: true, Proof: proof}
}

// parentCeilingSourceIdentity resolves the recorded source of row to a live
// identity for the step-7 decision. A non-empty cause denies.
func (a *AuthzService) parentCeilingSourceIdentity(ctx context.Context, row store.AgentServiceAccountAssignment) (Identity, DenyCause, string) {
	if assignmentHasDevLocalProvenance(row) {
		if !a.devLocalAuthorityEnabled() {
			return nil, DenyCauseCeilingSourceNotAllowed, reasonDevLocalDisabled
		}
		if row.SourcePrincipalKind != store.DelegationPrincipalUser || row.SourcePrincipalID != DevUserID {
			return nil, DenyCauseCeilingSourceNotAllowed, "recorded local development source is not the local development user"
		}
	}

	switch row.SourcePrincipalKind {
	case store.DelegationPrincipalUser:
		user, err := a.store.GetUser(ctx, row.SourcePrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, DenyCauseCeilingOrphaned, "recorded source does not exist"
			}
			return nil, DenyCauseCeilingError, "source lookup failed"
		}
		if user == nil {
			return nil, DenyCauseCeilingOrphaned, "recorded source does not exist"
		}
		if user.Status != store.UserStatusActive {
			return nil, DenyCauseCeilingOrphaned, reasonPrincipalInactive
		}
		return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "sa_parent_ceiling"), "", ""

	case store.DelegationPrincipalAgent:
		src, err := a.store.GetAgent(ctx, row.SourcePrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, DenyCauseCeilingOrphaned, "recorded source does not exist"
			}
			return nil, DenyCauseCeilingError, "source lookup failed"
		}
		if src == nil || !src.DeletedAt.IsZero() {
			return nil, DenyCauseCeilingOrphaned, "recorded source does not exist"
		}
		scopes, err := a.ceilingFilteredAgentScopes(ctx, src, a.mintCandidateScopes(src))
		if err != nil {
			switch {
			case errors.Is(err, errSourceNotAllowed):
				return nil, DenyCauseCeilingSourceNotAllowed, "recorded source's chain is not accepted"
			case errors.Is(err, ErrProvenanceMissing), errors.Is(err, ErrProvenanceAmbiguous), errors.Is(err, ErrProvenanceChain):
				return nil, DenyCauseCeilingOrphaned, "recorded source's chain does not resolve"
			default:
				return nil, DenyCauseCeilingError, "source chain lookup failed"
			}
		}
		ancestry := make([]string, len(src.Ancestry))
		copy(ancestry, src.Ancestry)
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: src.ID},
			ProjectID: src.ProjectID,
			Scopes:    scopes,
			Ancestry:  ancestry,
		}}, "", ""
	}
	return nil, DenyCauseCeilingOrphaned, "recorded source is inconsistent"
}

// evaluateResourceParentCeiling runs the evaluator for mapping m. Only the
// reviewed mappings have an evaluator; any other mapping denies with
// ceiling_error, so adding a table row without an evaluator cannot allow.
func (a *AuthzService) evaluateResourceParentCeiling(ctx context.Context, m ResourceParentCeiling, agentID string, resource Resource) ParentCeilingDecision {
	proof := ParentProof{Permission: m.ParentPermission}
	if resource.Type != m.ResourceType {
		return parentCeilingDeny(DenyCauseCeilingError, "resource does not match the parent ceiling", proof)
	}
	switch m.DecisionPermission {
	case gcpServiceAccountUsePermission:
		return a.EvaluateServiceAccountParentCeiling(ctx, agentID, resource.ID)
	default:
		return parentCeilingDeny(DenyCauseCeilingError, "no evaluator for the parent ceiling", proof)
	}
}

// applyResourceParentCeiling is Decide's step 10 for a permission with a
// resource parent ceiling. decision must be allowed on entry. In order:
//   - a nil agent (an agent-kind principal whose identity is not an
//     AgentIdentity) denies with ceiling_error;
//   - parentCeilingChainDeny: an unrecorded hop on the agent's own chain
//     denies, with the cause the chain check gives it;
//   - the parent ceiling evaluator for mapping m.
//
// On allow the reason names the parent proof and the decision is always
// audited, so the record of the use decision carries the parent permission
// and the recorded source; on deny the cause and neutral reason replace the
// decision's. The proof is attached either way (Decision.parentProof).
func (a *AuthzService) applyResourceParentCeiling(ctx context.Context, m ResourceParentCeiling, agent AgentIdentity, resource Resource, decision Decision) Decision {
	if agent == nil || isNilIdentity(agent) || agent.ID() == "" {
		return parentCeilingDenyDecision(decision, DenyCauseCeilingError,
			"parent ceiling: the principal is not an agent identity", ParentProof{Permission: m.ParentPermission})
	}
	if deny, cause, reason, adoptable := a.parentCeilingChainDeny(ctx, agent, m.DecisionPermission); deny {
		decision = parentCeilingDenyDecision(decision, cause, reason, ParentProof{Permission: m.ParentPermission})
		decision.adoptionRemediable = cause == DenyCauseCeilingUnrecorded && adoptable
		return decision
	}
	pc := a.evaluateResourceParentCeiling(ctx, m, agent.ID(), resource)
	if !pc.Allowed {
		return parentCeilingDenyDecision(decision, pc.DenyCause, "parent ceiling: "+pc.Reason, pc.Proof)
	}
	proof := pc.Proof
	decision.parentProof = &proof
	decision.AlwaysAudit = true
	decision.Reason = parentProofReason(decision.Reason, proof)
	return decision
}

// parentCeilingDenyDecision turns decision into a step-10 deny with cause
// and reason, and attaches proof.
func parentCeilingDenyDecision(decision Decision, cause DenyCause, reason string, proof ParentProof) Decision {
	decision.Allowed = false
	decision.Reason = reason
	decision.DeniedBy = DeniedByDelegationCeiling
	decision.DenyCause = cause
	decision.parentProof = &proof
	return decision
}

// parentCeilingChainDeny keeps, under a resource parent ceiling, the chain
// states the delegation chain check (walkDelegationChainHops) denies from
// what the chain recorded, so that the parent ceiling never admits a chain
// the chain check refuses for its recorded state. In particular an
// unrecorded hop never allows a permission in recordedProvenanceRequired.
// The parent ceiling reads the agent's recorded assignment, not its
// delegation chain, so without this check a legacy agent whose own edge was
// never recorded would be admitted once its assignment was re-recorded by a
// live assigner.
//
// It walks the agent's active delegation edges in its own project, from the
// agent up to the first user delegator, and reads only what each hop
// recorded. It never asks whether a delegator holds the permission: the
// parent ceiling replaces that question. It returns deny with a cause and a
// reason, and adoptable for a ceiling_unrecorded deny that delegation
// adoption can address (see unrecordedHopNote). In order, per hop:
//   - an edge lookup error, a cycle, a chain deeper than
//     maxDelegationDepth, an agent with no project, a ceiling kind or
//     delegator type this binary does not know: ceiling_error;
//   - no active edge: the chain check's no-edge rule, exactly. A
//     permission in legacyChainExcludedPermissions denies with
//     ceiling_unrecorded. Otherwise the missing edge is admitted only for
//     the agent's own edge (depth 0), only for a hub-attested identity, and
//     only while the delegation edge backfill has not completed; the
//     recorded assignment is then the proof. After the backfill, at
//     depth > 0 (an agent delegator with no active edge, for example a
//     soft-deleted parent), or for an identity that is not hub-attested, it
//     denies with the chain check's reason and its cause, which is empty
//     (no dedicated classification). The chain check's post-backfill
//     allowance for registered non-sensitive reads does not apply: no
//     permission with a parent ceiling is a read (ResourceParentCeilingFor);
//   - more than one active edge: ceiling_provenance_ambiguous;
//   - a migration-sentinel edge: ceiling_orphaned, the cause the chain check
//     gives it for a permission that is not a registered read;
//   - an edge with local development provenance, when dev authority is not
//     enabled on this server or the delegator is not user:DevUserID:
//     ceiling_source_not_allowed, as hopEffectCeilingDenyFor gives it;
//   - an unrecorded edge (unrecorded kind, or a provenance version this
//     binary does not understand): ceiling_unrecorded, with the chain
//     check's reason.
func (a *AuthzService) parentCeilingChainDeny(ctx context.Context, agent AgentIdentity, permissionID string) (deny bool, cause DenyCause, reason string, adoptable bool) {
	scopeID := agent.ProjectID()
	if scopeID == "" {
		return true, DenyCauseCeilingError, "delegation ceiling: the agent has no project scope", false
	}
	visited := make(map[string]bool, maxDelegationDepth+1)
	delegateID := agent.ID()
	for depth := 0; ; depth++ {
		if depth > maxDelegationDepth {
			return true, DenyCauseCeilingError, "delegation chain exceeded maximum depth", false
		}
		if visited[delegateID] {
			return true, DenyCauseCeilingError, "delegation chain contains a cycle", false
		}
		visited[delegateID] = true

		edges, err := a.getCachedDelegationEdges(ctx, store.DelegationPrincipalAgent, delegateID)
		if err != nil {
			return true, DenyCauseCeilingError, "delegation ceiling check failed (fail-closed): delegation edge lookup failed", false
		}
		var active []*store.DelegationEdge
		for _, e := range filterEdgesByScope(edges, store.RoleScopeProject, scopeID) {
			if e.Active {
				active = append(active, e)
			}
		}
		switch len(active) {
		case 0:
			return a.parentCeilingMissingEdge(ctx, agent, permissionID, delegateID, depth)
		case 1:
		default:
			return true, DenyCauseCeilingProvenanceAmbiguous,
				fmt.Sprintf("multiple active delegation edges for agent:%s (fail-closed on invariant violation)", delegateID), false
		}
		edge := active[0]
		if isMigrationSentinel(edge) {
			return true, DenyCauseCeilingOrphaned,
				fmt.Sprintf("migration-provenance delegation permits registered non-sensitive reads only; %s denied", permissionID), false
		}
		if hasDevLocalProvenance(edge) {
			if !a.devLocalAuthorityEnabled() {
				return true, DenyCauseCeilingSourceNotAllowed, "delegation ceiling: " + reasonDevLocalDisabled, false
			}
			if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID {
				return true, DenyCauseCeilingSourceNotAllowed, "delegation ceiling: local development provenance with another delegator", false
			}
		}
		if hopUnrecorded(edge) {
			var note unrecordedHopNote
			a.logUnrecordedHop(&note, DenyCauseCeilingUnrecorded, edge, permissionID)
			return true, DenyCauseCeilingUnrecorded, fmt.Sprintf("delegation ceiling: %s requires recorded provenance", permissionID), note.adoptable
		}
		switch edge.Kind {
		case store.EffectCeilingPrincipal, store.EffectCeilingBounded:
		default:
			return true, DenyCauseCeilingError, fmt.Sprintf("delegation edge %s has an unsupported ceiling kind", edge.ID), false
		}
		switch edge.DelegatorType {
		case store.DelegationPrincipalUser:
			return false, "", "", false
		case store.DelegationPrincipalAgent:
			delegateID = edge.DelegatorID
		default:
			return true, DenyCauseCeilingError, fmt.Sprintf("delegation edge %s has an unsupported delegator type", edge.ID), false
		}
	}
}

// parentCeilingMissingEdge is parentCeilingChainDeny's rule for a delegate
// with no active edge at depth. It is the no-edge branch of
// walkDelegationChainHops, with the same causes and reasons, less the
// registered-read allowance (see parentCeilingChainDeny).
func (a *AuthzService) parentCeilingMissingEdge(ctx context.Context, agent AgentIdentity, permissionID, delegateID string, depth int) (bool, DenyCause, string, bool) {
	if legacyChainExcludedPermissions[permissionID] {
		return true, DenyCauseCeilingUnrecorded,
			fmt.Sprintf("no delegation edge for agent:%s: %s requires a recorded delegation", delegateID, permissionID), false
	}
	if depth == 0 && AncestryIsHubAttested(agent) && !a.backfillCompleted(ctx) {
		return false, "", "", false
	}
	return true, "", fmt.Sprintf("no delegation edge for agent:%s (no delegated authority)", delegateID), false
}

// decideStoredAgentServiceAccountUse decides gcp_service_account.use for a
// stored agent on a Hub path that uses the agent's assigned account on the
// agent's behalf without the agent's credential: the GCS link proxy mints a
// read-only token for the sending agent's account to fetch an object the
// agent named in a message. With no agent credential on the request, the
// token-scope and grant steps of Decide do not apply. The caller's record
// checks stand in for them: the agent's applied identity is in assign mode
// for exactly this account, and the account passes the same admissibility
// rule as the token mint (resolveCurrentSA). Step 10 is Decide's own
// (applyResourceParentCeiling): the chain check of recorded state, then
// the account's parent ceiling. The decision is always audited, allow or
// deny, as Decide audits a step-10 parent-ceiling decision.
func (a *AuthzService) decideStoredAgentServiceAccountUse(ctx context.Context, agent *store.Agent, sa *store.GCPServiceAccount) Decision {
	if a == nil || agent == nil || sa == nil {
		return Decision{Allowed: false, Reason: "invalid stored-agent use request", DeniedBy: DeniedByDelegationCeiling, DenyCause: DenyCauseCeilingError}
	}
	ident := &storedAgentIdentity{agent: agent}
	// No agent credential was presented, so the request names none; the
	// audit record shows the agent as principal with no credential.
	request := AuthzRequest{
		Principal:   principalContextForIdentity(ident),
		Resource:    gcpServiceAccountResource(sa),
		Action:      ActionUse,
		Permission:  permissions.PermissionGCPServiceAccountUse,
		AlwaysAudit: true,
	}
	decision := Decision{Allowed: true, Reason: "stored agent record names the account in assign mode"}
	m, ok := ResourceParentCeilingFor(gcpServiceAccountUsePermission)
	if !ok {
		decision = parentCeilingDenyDecision(decision, DenyCauseCeilingError, "no parent ceiling for the permission", ParentProof{})
	} else {
		decision = a.applyResourceParentCeiling(maskAuthzInputs(ctx), m, ident, request.Resource, decision)
	}
	decision = decorateDecision(decision, request, request.Principal, request.Credential, auditPermissionID(request))
	if a.decisionAuditEmitter != nil {
		a.emitDecisionAudit(ctx, request, decision)
	}
	return decision
}

// parentProofReason appends the parent proof to an allow reason.
func parentProofReason(reason string, p ParentProof) string {
	note := fmt.Sprintf("parent proof: %s held by %s:%s (%s), assignment %s",
		p.Permission, p.SourcePrincipal.Kind, p.SourcePrincipal.ID, p.SourceCredentialKind, p.AssignmentID)
	if reason == "" {
		return note
	}
	return reason + " (" + note + ")"
}
