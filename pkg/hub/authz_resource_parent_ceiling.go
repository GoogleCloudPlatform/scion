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
// actAs, write anything, or emit a decision audit record; the caller
// composes it with the actAs check and records the decision with permission
// gcp_service_account.use and Proof.Permission as the parent proof.

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

// ParentProofKind names what a ParentProof proves.
type ParentProofKind string

const (
	// ParentProofKindParentPermission: the recorded source live-holds a
	// registered parent permission on the resource (Permission).
	ParentProofKindParentPermission ParentProofKind = "parent_permission"
	// ParentProofKindOwnershipFact: no registered permission expresses the
	// parent authority, so the proof is the live ownership fact of the
	// resource (ResourceID) by the recorded source; Permission is the
	// decision permission.
	ParentProofKindOwnershipFact ParentProofKind = "ownership_fact"
)

// ParentProof describes the parent authority a parent-ceiling decision
// checked: the permission, the recorded source and the resource.
//   - Kind parent_permission (the service-account ceiling): Permission is
//     the parent permission the source holds, AssignmentID and
//     ServiceAccountID name the assignment and the account.
//   - Kind ownership_fact (the user-scope secret ceiling): Permission is the
//     decision permission (secret.use), and ResourceID is the secret whose
//     ownership by the source was proved.
type ParentProof struct {
	Kind                 ParentProofKind
	Permission           string
	SourcePrincipal      ProvenancePrincipal
	SourceCredentialKind store.SourceCredentialKind
	AssignmentID         string
	ServiceAccountID     string
	ResourceID           string
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
	proof := ParentProof{Kind: ParentProofKindParentPermission, Permission: gcpServiceAccountAssignPermission, ServiceAccountID: serviceAccountID}
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

// ---- user-scope secret parent ceiling ---------------------------------------
//
// An agent's use of a user-scope secret (secret.use with ActionUse on a
// secret whose parent is a user) has no registered parent permission: no
// role or relationship grants a user secret.use on their own secret. The
// parent proof is instead the live ownership fact: the user at the end of the
// agent's recorded delegation chain is active, admitted to the agent's
// project, not restricted by an access constraint or the test-admin policy,
// and is the owner and author of the exact secret, which they have shared
// with their progeny. Every hop of the chain carries recorded provenance
// and an effect ceiling that allows secret.use.
//
// The evaluation has two steps, so a caller that evaluates many secrets for
// one agent can run the first once:
//   - the chain verdict (userSecretChainVerdict): the principal, the stored
//     agent, every hop, and the terminal user;
//   - the source check (userSecretSourceCheck): the exact secret, against
//     inputs loaded once per request (loadUserSecretSourceInputs).
//
// It proves neither the requesting agent's credential scope nor the
// requesting agent's own access constraints: Decide applies those (steps 7b
// and 7c), and a caller outside Decide applies them itself.

const secretUsePermission = "secret.use"

// errUserSecretParentCeiling is returned by the step-10 arm when the
// evaluation could not be completed; Decide denies with ceiling_error.
var errUserSecretParentCeiling = errors.New("user secret parent ceiling could not be evaluated")

// userSecretParentProofApplies reports whether step 10 evaluates the
// user-scope secret parent ceiling instead of the generic chain walk: the
// permission is secret.use, the action is use, the resource is a secret
// whose parent is a user, and the principal is not the hub delivery
// credential. It is the only definition of that test. An empty resource ID
// or parent ID is not part of it: such a request takes the parent ceiling
// and denies there, never the generic walk.
func userSecretParentProofApplies(req AuthzRequest, permissionID string) bool {
	if permissionID != secretUsePermission {
		return false
	}
	if req.Action != ActionUse {
		return false
	}
	if req.Resource.Type != "secret" {
		return false
	}
	if req.Resource.ParentType != "user" {
		return false
	}
	if _, ok := req.Principal.Identity.(*hubDeliveryIdentity); ok {
		return false
	}
	return true
}

// userSecretResource is the resource for an agent's use of the user-scope
// secret meta. Check 8 and the agent secret list both build it here, so the
// read and the list evaluate the same resource.
func userSecretResource(id, scopeID string) Resource {
	return Resource{Type: "secret", ID: id, OwnerID: scopeID, ParentType: "user", ParentID: scopeID}
}

// userSecretChain is the result of the chain verdict. When decision is
// allowed, agent is the stored agent row, user the terminal user and
// sourceCredential the credential kind recorded on the terminal hop.
type userSecretChain struct {
	decision         ParentCeilingDecision
	agent            *store.Agent
	user             *store.User
	sourceCredential store.SourceCredentialKind
	// note is filled from a ceiling_unrecorded deny, as the generic walk
	// fills it (logUnrecordedHop).
	note unrecordedHopNote
	// unrecorded is set when the deny came from a hop that carries no
	// recorded provenance: no active edge, more than one, the migration
	// sentinel, an unrecorded ceiling or unknown provenance version, or an
	// edge whose recorded principal does not type-check.
	//
	// The walk stops at the first failing clause, so in a chain that mixes
	// recorded and unrecorded hops every recorded hop below the first
	// unrecorded one is still enforced: S3b, S3c and S3d run on it before
	// the walk reaches the unrecorded hop, and a deny there leaves
	// unrecorded false. Only a hop without recorded provenance sets it, and
	// the callers (check 8 and the agent secret list) never deny on such a
	// hop alone: they apply the progeny rule instead.
	unrecorded bool
}

func (c userSecretChain) deny(cause DenyCause, reason string) userSecretChain {
	c.decision = parentCeilingDeny(cause, reason, c.decision.Proof)
	return c
}

// userSecretSourceInputs are the inputs of the source check, loaded once per
// request: the access-constraint table, the terminal user's principal
// closure and the progeny secrets of the stored agent's ancestry.
type userSecretSourceInputs struct {
	constraints []*store.AccessConstraint
	userClosure map[string]struct{}
	progeny     []store.Secret
}

// secretParentStep appends a secret_parent_ceiling_* step when explain is
// non-nil.
func secretParentStep(explain *[]DecisionStep, step, detail string) {
	if explain != nil {
		*explain = append(*explain, DecisionStep{Step: "secret_parent_ceiling_" + step, Detail: detail})
	}
}

// EvaluateUserSecretParentCeiling proves that principal, a local agent, may
// use the user-scope secret resource under its recorded delegation chain.
// Clauses, in order, each denying with the given cause:
//   - the request names a principal (ceiling_error) and a secret and its
//     parent user (ceiling_resource_missing);
//   - S1 the principal's ancestry is hub-attested (ceiling_source_not_allowed);
//   - S2 the stored agent exists, is not deleted or held, and is in the
//     principal's project (ceiling_orphaned);
//   - per hop, walking up in the agent's project: S3 exactly one active edge
//     (ceiling_provenance_missing, ceiling_provenance_ambiguous); S3s not the
//     migration sentinel (ceiling_orphaned); S3a a known provenance version
//     and a principal or bounded ceiling (ceiling_unrecorded); S3f the
//     recorded principal type-checks (ceiling_orphaned); S3b a local
//     development hop needs dev auth and the local development user
//     (ceiling_source_not_allowed); S3c a bounded ceiling allows secret.use,
//     with no self-operation exception (ceiling_effect_exceeded); S3d an
//     agent delegator is live and holds secret.use through its role
//     (ceiling_orphaned, ceiling_delegator_lacks_permission); S3e depth and
//     cycles (ceiling_orphaned);
//   - S4 the terminal user exists and is active (ceiling_orphaned); S4a it
//     is admitted to the agent's project for secret.use; S4c it is not a
//     caged test admin (both ceiling_delegator_lacks_permission);
//   - S5 exactly one progeny secret of the stored ancestry has the resource
//     ID (ceiling_resource_missing, ceiling_provenance_ambiguous); S6 it is
//     user-scoped to the resource's parent, shared with progeny and not
//     internal (ceiling_provenance_stale for another parent, otherwise
//     ceiling_resource_missing); S4b the terminal user's access constraints
//     allow secret.use on it (ceiling_delegator_lacks_permission); S7 its
//     recorded creator (CreatedBy) is its owner; S8 the terminal user is
//     its recorded creator and the head of the stored ancestry (both
//     ceiling_source_not_allowed). CreatedBy is attribution, not proof of
//     authorship: the agent write path records the scope owner as the
//     creator of a user-scope secret an agent writes.
//
// Any lookup error denies with ceiling_error. It never returns Allowed on an
// error, writes nothing and emits no audit record. The stored agent row is
// the only source of the agent's project and ancestry.
func (a *AuthzService) EvaluateUserSecretParentCeiling(ctx context.Context, principal AgentIdentity, resource Resource) ParentCeilingDecision {
	d, _ := a.evaluateUserSecretParentCeiling(ctx, principal, resource, nil)
	return d
}

// evaluateUserSecretParentCeiling is EvaluateUserSecretParentCeiling with
// explain steps and the unrecorded-hop note.
func (a *AuthzService) evaluateUserSecretParentCeiling(ctx context.Context, principal AgentIdentity, resource Resource, explain *[]DecisionStep) (ParentCeilingDecision, unrecordedHopNote) {
	proof := ParentProof{Kind: ParentProofKindOwnershipFact, Permission: secretUsePermission, ResourceID: resource.ID}
	if a == nil || a.store == nil || isNilIdentity(principal) || principal.ID() == "" {
		secretParentStep(explain, "error", "invalid parent ceiling request")
		return parentCeilingDeny(DenyCauseCeilingError, "invalid parent ceiling request", proof), unrecordedHopNote{}
	}
	if resource.ID == "" || resource.ParentID == "" {
		secretParentStep(explain, "invalid_resource", "the request names no secret or no parent user")
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "invalid resource: no secret or parent user", proof), unrecordedHopNote{}
	}
	ctx = maskAuthzInputs(ctx)
	chain := a.userSecretChainVerdict(ctx, principal, explain)
	if !chain.decision.Allowed {
		d := chain.decision
		d.Proof.ResourceID = resource.ID
		return d, chain.note
	}
	in, err := a.loadUserSecretSourceInputs(ctx, chain)
	if err != nil {
		secretParentStep(explain, "error", "source input lookup failed")
		d := parentCeilingDeny(DenyCauseCeilingError, "source input lookup failed", chain.decision.Proof)
		d.Proof.ResourceID = resource.ID
		return d, unrecordedHopNote{}
	}
	return userSecretSourceCheck(chain, in, resource, explain), unrecordedHopNote{}
}

// userSecretChainVerdict runs clauses S1-S4c for principal (see
// EvaluateUserSecretParentCeiling). It masks the request's authorization
// inputs itself, so it can be called on its own.
func (a *AuthzService) userSecretChainVerdict(ctx context.Context, principal AgentIdentity, explain *[]DecisionStep) userSecretChain {
	chain := userSecretChain{decision: ParentCeilingDecision{Proof: ParentProof{Kind: ParentProofKindOwnershipFact, Permission: secretUsePermission}}}
	if a == nil || a.store == nil || isNilIdentity(principal) || principal.ID() == "" {
		secretParentStep(explain, "error", "invalid parent ceiling request")
		return chain.deny(DenyCauseCeilingError, "invalid parent ceiling request")
	}
	ctx = maskAuthzInputs(ctx)

	// S1
	if !AncestryIsHubAttested(principal) {
		secretParentStep(explain, "not_attested", "principal ancestry is not hub-attested")
		return chain.deny(DenyCauseCeilingSourceNotAllowed, "principal ancestry is not hub-attested")
	}

	// S2
	agent, err := a.store.GetAgent(ctx, principal.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			secretParentStep(explain, "agent_not_live", "agent does not exist")
			return chain.deny(DenyCauseCeilingOrphaned, "agent does not exist")
		}
		secretParentStep(explain, "error", "agent lookup failed")
		return chain.deny(DenyCauseCeilingError, "agent lookup failed")
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		secretParentStep(explain, "agent_not_live", "agent does not exist")
		return chain.deny(DenyCauseCeilingOrphaned, "agent does not exist")
	}
	held, err := a.store.HasActiveAgentHold(ctx, agent.ID)
	if err != nil {
		secretParentStep(explain, "error", "agent hold lookup failed")
		return chain.deny(DenyCauseCeilingError, "agent hold lookup failed")
	}
	if held {
		secretParentStep(explain, "agent_not_live", "agent is held")
		return chain.deny(DenyCauseCeilingOrphaned, "agent is not live")
	}
	if agent.ProjectID == "" || agent.ProjectID != principal.ProjectID() {
		secretParentStep(explain, "agent_project", "agent project does not match the principal's project")
		return chain.deny(DenyCauseCeilingOrphaned, "agent project does not match the principal's project")
	}

	// S3-S3e
	visited := make(map[string]bool, maxDelegationDepth+1)
	delegateID := agent.ID
	var terminal *store.DelegationEdge
	for depth := 0; terminal == nil; depth++ {
		if depth > maxDelegationDepth {
			secretParentStep(explain, "depth", "delegation chain exceeded maximum depth")
			return chain.deny(DenyCauseCeilingOrphaned, "delegation chain exceeded maximum depth")
		}
		if visited[delegateID] {
			secretParentStep(explain, "cycle", fmt.Sprintf("delegate %s repeats in the chain", delegateID))
			return chain.deny(DenyCauseCeilingOrphaned, "delegation chain contains a cycle")
		}
		visited[delegateID] = true

		active, err := a.activeProjectEdges(ctx, delegateID, agent.ProjectID)
		if err != nil {
			secretParentStep(explain, "error", "delegation edge lookup failed")
			return chain.deny(DenyCauseCeilingError, "delegation edge lookup failed")
		}
		switch len(active) {
		case 0:
			secretParentStep(explain, "no_edge", fmt.Sprintf("no active delegation edge for agent:%s", delegateID))
			chain.unrecorded = true
			return chain.deny(DenyCauseCeilingProvenanceMissing, "no recorded delegation for agent "+delegateID)
		case 1:
		default:
			secretParentStep(explain, "duplicate_edges", fmt.Sprintf("%d active edges for agent:%s", len(active), delegateID))
			chain.unrecorded = true
			return chain.deny(DenyCauseCeilingProvenanceAmbiguous, "more than one active delegation for agent "+delegateID)
		}
		edge := active[0]

		// S3s
		if isMigrationSentinel(edge) {
			secretParentStep(explain, "migration_edge", fmt.Sprintf("migration-provenance edge for agent:%s", delegateID))
			chain.unrecorded = true
			return chain.deny(DenyCauseCeilingOrphaned, "migration-provenance delegation supplies no source")
		}
		// S3a
		if !knownProvenanceVersion(edge.ProvenanceVersion) ||
			(edge.Kind != store.EffectCeilingPrincipal && edge.Kind != store.EffectCeilingBounded) {
			secretParentStep(explain, "unrecorded", fmt.Sprintf("edge %s has no recorded provenance", edge.ID))
			a.logUnrecordedHop(&chain.note, DenyCauseCeilingUnrecorded, edge, secretUsePermission)
			chain.unrecorded = true
			return chain.deny(DenyCauseCeilingUnrecorded, "secret.use requires recorded provenance")
		}
		// S3f
		if err := checkEdgePrincipal(edge); err != nil {
			secretParentStep(explain, "principal_mismatch", fmt.Sprintf("edge %s: recorded principal is inconsistent", edge.ID))
			chain.unrecorded = true
			return chain.deny(DenyCauseCeilingOrphaned, "recorded delegation principal is inconsistent")
		}
		// S3b
		if hasDevLocalProvenance(edge) {
			if !a.devLocalAuthorityEnabled() {
				secretParentStep(explain, "dev_local", fmt.Sprintf("edge %s: %s", edge.ID, reasonDevLocalDisabled))
				return chain.deny(DenyCauseCeilingSourceNotAllowed, reasonDevLocalDisabled)
			}
			if edge.DelegatorType != store.DelegationPrincipalUser || edge.DelegatorID != DevUserID {
				secretParentStep(explain, "dev_local", fmt.Sprintf("edge %s: local development provenance with another delegator", edge.ID))
				return chain.deny(DenyCauseCeilingSourceNotAllowed, "local development provenance with another delegator")
			}
		}
		// S3c
		if edge.Kind == store.EffectCeilingBounded && !EffectCeilingAllows(edge.EffectCeiling, secretUsePermission, false) {
			secretParentStep(explain, "effect_exceeded", fmt.Sprintf("edge %s: secret.use is outside the recorded effect ceiling", edge.ID))
			return chain.deny(DenyCauseCeilingEffectExceeded, "secret.use is outside the recorded effect ceiling")
		}

		switch edge.DelegatorType {
		case store.DelegationPrincipalUser:
			terminal = edge
		case store.DelegationPrincipalAgent:
			// S3d, before the next hop's edges are loaded.
			ok, _, err := a.checkAgentHoldsPermission(ctx, edge.DelegatorID, secretUsePermission, edge.ScopeType, edge.ScopeID)
			switch {
			case err != nil && errors.Is(err, errCeilingHoldLookup):
				secretParentStep(explain, "error", "delegator agent hold lookup failed")
				return chain.deny(DenyCauseCeilingError, "delegator agent lookup failed")
			case err != nil && errors.Is(err, store.ErrNotFound):
				secretParentStep(explain, "delegator_not_live", fmt.Sprintf("delegator agent %s is not live", edge.DelegatorID))
				return chain.deny(DenyCauseCeilingOrphaned, "delegator agent "+edge.DelegatorID+" is not live")
			case err != nil:
				secretParentStep(explain, "error", "delegator agent lookup failed")
				return chain.deny(DenyCauseCeilingError, "delegator agent lookup failed")
			case !ok:
				secretParentStep(explain, "delegator_lacks_permission", fmt.Sprintf("delegator agent %s does not hold secret.use", edge.DelegatorID))
				return chain.deny(DenyCauseCeilingDelegatorLacksPermission, "delegator agent "+edge.DelegatorID+" does not hold secret.use")
			}
			secretParentStep(explain, "link_allowed", fmt.Sprintf("delegator agent %s holds secret.use; continuing", edge.DelegatorID))
			delegateID = edge.DelegatorID
		default:
			// checkEdgePrincipal admits only user and agent delegators.
			secretParentStep(explain, "principal_mismatch", fmt.Sprintf("edge %s has delegator type %q", edge.ID, edge.DelegatorType))
			chain.unrecorded = true
			return chain.deny(DenyCauseCeilingOrphaned, "recorded delegation principal is inconsistent")
		}
	}
	chain.decision.Proof.SourcePrincipal = ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: terminal.DelegatorID}
	chain.decision.Proof.SourceCredentialKind = terminal.SourceCredentialKind

	// S4
	user, err := a.store.GetUser(ctx, terminal.DelegatorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			secretParentStep(explain, "source_not_live", "terminal user does not exist")
			return chain.deny(DenyCauseCeilingOrphaned, "recorded source does not exist")
		}
		secretParentStep(explain, "error", "terminal user lookup failed")
		return chain.deny(DenyCauseCeilingError, "source lookup failed")
	}
	if user == nil {
		secretParentStep(explain, "source_not_live", "terminal user does not exist")
		return chain.deny(DenyCauseCeilingOrphaned, "recorded source does not exist")
	}
	if user.Status != store.UserStatusActive {
		secretParentStep(explain, "source_not_live", "terminal user is not active")
		return chain.deny(DenyCauseCeilingOrphaned, reasonPrincipalInactive)
	}

	// S4a
	admitted, _, err := a.sourceUserExecutionAdmission(ctx, user, agent.ProjectID, secretUsePermission)
	if err != nil {
		secretParentStep(explain, "error", "terminal user project admission check failed")
		return chain.deny(DenyCauseCeilingError, "source project admission check failed")
	}
	if !admitted {
		secretParentStep(explain, "source_not_admitted", "terminal user lacks admission to the agent's project")
		return chain.deny(DenyCauseCeilingDelegatorLacksPermission, "recorded source lacks admission to the agent's project")
	}

	// S4c: the error first, since a kind-read error also reports caged.
	if testAdminPermissionDenied(secretUsePermission) {
		caged, err := a.principalIsCagedTestAdmin(ctx, user.ID)
		if err != nil {
			secretParentStep(explain, "error", "terminal user kind lookup failed")
			return chain.deny(DenyCauseCeilingError, "source kind lookup failed")
		}
		if caged {
			secretParentStep(explain, "source_caged", "terminal user is subject to the test-admin policy")
			return chain.deny(DenyCauseCeilingDelegatorLacksPermission, "recorded source is subject to the test-admin policy")
		}
	}

	secretParentStep(explain, "chain_allowed", fmt.Sprintf("recorded chain ends at active user %s", user.ID))
	chain.agent = agent
	chain.user = user
	chain.sourceCredential = terminal.SourceCredentialKind
	chain.decision.Allowed = true
	return chain
}

// loadUserSecretSourceInputs loads the source check's inputs for an allowed
// chain, once per request. The constraint table is loaded uncached, so no
// mint-eligibility cache or request memo is read, and the terminal user's
// closure is built on a masked ctx.
func (a *AuthzService) loadUserSecretSourceInputs(ctx context.Context, chain userSecretChain) (*userSecretSourceInputs, error) {
	if a == nil || a.store == nil || !chain.decision.Allowed || chain.agent == nil || chain.user == nil {
		return nil, errUserSecretParentCeiling
	}
	ctx = maskAuthzInputs(ctx)
	constraints, err := a.loadAllAccessConstraintsUncached(ctx)
	if err != nil {
		return nil, fmt.Errorf("access constraint lookup: %w", err)
	}
	identity := NewAuthenticatedUser(chain.user.ID, chain.user.Email, chain.user.DisplayName, chain.user.Role, "")
	principals, err := a.authorizationPrincipals(ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("source principal lookup: %w", err)
	}
	closure := make(map[string]struct{}, len(principals))
	for _, p := range principals {
		closure[p.Type+":"+p.ID] = struct{}{}
	}
	var progeny []store.Secret
	if len(chain.agent.Ancestry) > 0 {
		progeny, err = a.store.ListProgenySecrets(ctx, chain.agent.Ancestry)
		if err != nil {
			return nil, fmt.Errorf("progeny secret lookup: %w", err)
		}
	}
	return &userSecretSourceInputs{constraints: constraints, userClosure: closure, progeny: progeny}, nil
}

// userSecretSourceCheck runs clauses S5, S6, S4b, S7 and S8 (see
// EvaluateUserSecretParentCeiling) for resource, against an allowed chain
// and its request-scoped inputs. It does no I/O.
func userSecretSourceCheck(chain userSecretChain, in *userSecretSourceInputs, resource Resource, explain *[]DecisionStep) ParentCeilingDecision {
	proof := chain.decision.Proof
	proof.ResourceID = resource.ID
	if !chain.decision.Allowed || chain.agent == nil || chain.user == nil || in == nil {
		secretParentStep(explain, "error", "source check without an allowed chain")
		return parentCeilingDeny(DenyCauseCeilingError, "source check without an allowed chain", proof)
	}
	if resource.ID == "" || resource.ParentID == "" {
		secretParentStep(explain, "invalid_resource", "the request names no secret or no parent user")
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "invalid resource: no secret or parent user", proof)
	}

	// S5
	var matches []store.Secret
	for _, sec := range in.progeny {
		if sec.ID == resource.ID {
			matches = append(matches, sec)
		}
	}
	switch len(matches) {
	case 0:
		secretParentStep(explain, "resource_missing", "the secret is not a progeny secret of the agent's ancestry")
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "secret is not shared with this agent's lineage", proof)
	case 1:
	default:
		secretParentStep(explain, "resource_ambiguous", "more than one progeny secret has the ID")
		return parentCeilingDeny(DenyCauseCeilingProvenanceAmbiguous, "secret is ambiguous", proof)
	}
	r := matches[0]

	// S6
	if r.Scope != store.ScopeUser {
		secretParentStep(explain, "resource_missing", "the secret is not user-scoped")
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "secret is not user-scoped", proof)
	}
	if r.ScopeID != resource.ParentID {
		secretParentStep(explain, "resource_stale", "the secret's owner is not the requested parent")
		return parentCeilingDeny(DenyCauseCeilingProvenanceStale, "secret owner does not match the requested parent", proof)
	}
	if !r.AllowProgeny || r.SecretType == store.SecretTypeInternal {
		secretParentStep(explain, "resource_missing", "the secret is not shared with progeny")
		return parentCeilingDeny(DenyCauseCeilingResourceMissing, "secret is not shared with progeny", proof)
	}

	// S4b
	for _, rs := range restrictionsFromConstraints(in.constraints, in.userClosure, ResourceContext{
		ResourceType: "secret",
		ResourceID:   r.ID,
		OwnerID:      r.ScopeID,
	}) {
		if rs.Check == nil || !rs.Check(secretUsePermission) {
			secretParentStep(explain, "source_restricted", "an access constraint on the terminal user restricts secret.use")
			return parentCeilingDeny(DenyCauseCeilingDelegatorLacksPermission, "recorded source is restricted by an access constraint", proof)
		}
	}

	// S7
	if r.CreatedBy != r.ScopeID {
		secretParentStep(explain, "source_not_allowed", "the secret's author is not its owner")
		return parentCeilingDeny(DenyCauseCeilingSourceNotAllowed, "secret author is not its owner", proof)
	}
	// S8
	ancestry := chain.agent.Ancestry
	inAncestry := false
	for _, id := range ancestry {
		if id == r.CreatedBy {
			inAncestry = true
			break
		}
	}
	if chain.user.ID != r.CreatedBy || !inAncestry || len(ancestry) == 0 || ancestry[0] != chain.user.ID {
		secretParentStep(explain, "source_not_allowed", "the recorded source is not the secret's author at the head of the ancestry")
		return parentCeilingDeny(DenyCauseCeilingSourceNotAllowed, "recorded source is not the secret's author", proof)
	}

	// S9
	secretParentStep(explain, "allowed", fmt.Sprintf("user %s owns and shares secret %s", chain.user.ID, r.ID))
	return ParentCeilingDecision{Allowed: true, Proof: proof}
}
