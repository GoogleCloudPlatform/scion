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
// principal recorded as the assignment's source still holds
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

	// 3. Freshness and recorded provenance.
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
