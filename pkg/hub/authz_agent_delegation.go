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
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent delegation codes (.design/agent-delegation.md §8.4, §11.2). The
// decision procedure records the precise code in the decision's
// AgentDelegation block and Reason; external responses collapse to the
// admitted handler's own 403 or 404.
const (
	agentDelegationCodeCredentialNotAdmitted     = "credential_not_admitted"
	agentDelegationCodeExperimentDisabled        = "experiment_disabled"
	agentDelegationCodeStateMissing              = "delegated_state_missing"
	agentDelegationCodeCredentialInactive        = "credential_inactive"
	agentDelegationCodeInvalidAudience           = "invalid_audience"
	agentDelegationCodeGrantInactive             = "grant_inactive"
	agentDelegationCodeSubdelegation             = "subdelegation_not_supported"
	agentDelegationCodeIssuerInvalid             = "issuer_invalid"
	agentDelegationCodeIssuerSuspended           = "issuer_suspended"
	agentDelegationCodeReservedIdentity          = "reserved_identity"
	agentDelegationCodeGrantAgentChanged         = "grant_agent_changed"
	agentDelegationCodeIssuerNotController       = "issuer_not_controller"
	agentDelegationCodeIssuerProjectAccess       = "issuer_project_access"
	agentDelegationCodeAgentCredentialInvalid    = "agent_credential_invalid"
	agentDelegationCodePermissionUnresolved      = "permission_unresolved"
	agentDelegationCodeOutsideCeiling            = "outside_ceiling"
	agentDelegationCodePermissionNotDelegable    = "permission_not_delegable"
	agentDelegationCodeGrantBoundaryInvalid      = "grant_boundary_invalid"
	agentDelegationCodeTargetUnknown             = "target_unknown"
	agentDelegationCodeOutsideBoundary           = "outside_boundary"
	agentDelegationCodeOutsideBoundaryEligibility = "outside_boundary_eligibility"
	agentDelegationCodeIssuerAuthority           = "issuer_authority"
	agentDelegationCodeEvaluationError           = "evaluation_error"
	agentDelegationCodeLookupError               = "lookup_error"
)

// actorKindAgentDelegated is the actor kind recorded for an agent
// delegated actor (mutation audit actor_kind, Decision.AgentDelegation).
const actorKindAgentDelegated = string(PrincipalKindAgentDelegated)

// AgentDelegationAttribution is the attribution block of a decision for an
// agent delegated credential (.design/agent-delegation.md §14.1). Field
// names match the reserved G names (e2a_no_g_column_test.go). It is
// in-memory only: no decision-audit sink records it today.
type AgentDelegationAttribution struct {
	ActorKind                 string
	ActorAgentID              string
	AuthorizingUserID         string
	SourceGrantID             string
	ParentGrantID             string
	DelegationEdgeID          string
	ExchangeAgentCredentialID string
	// AgentDelegationCode is the agent delegation code of a deny; empty on
	// allow.
	AgentDelegationCode string
}

// agentDelegationHooks are the server facts decideAgentDelegation needs
// that AuthzService does not own. Server.wireAgentDelegation sets them; a
// missing hook denies.
type agentDelegationHooks struct {
	// enabled reports whether hub.agent_delegation is on right now.
	enabled func() bool
	// audience returns this hub's delegated-credential audience.
	audience func() string
	// reservedIdentity reports whether email is a reserved platform
	// identity (isReservedPlatformIdentity).
	reservedIdentity func(email string) bool
	// standing returns nil only when the agent is in good standing
	// (Server.agentStanding).
	standing func(ctx context.Context, agentID string) error
	// now returns the current time.
	now func() time.Time
}

func (h agentDelegationHooks) complete() bool {
	return h.enabled != nil && h.audience != nil && h.reservedIdentity != nil && h.standing != nil && h.now != nil
}

// catalogOperationByID returns the catalog operation with id.
func catalogOperationByID(id authzop.OperationID) (authzop.OperationSpec, bool) {
	for _, spec := range authzop.Catalog {
		if spec.ID == id {
			return spec, true
		}
	}
	return authzop.OperationSpec{}, false
}

// catalogAdmitsDelegated reports whether spec admits the agent delegated
// principal with the agent delegated credential.
func catalogAdmitsDelegated(spec authzop.OperationSpec) bool {
	return slices.Contains(spec.Principals, authzop.PrincipalAgentDelegated) &&
		slices.Contains(spec.Credentials, authzop.CredentialDelegatedAgent)
}

// isAgentPhaseSuspended reports whether the agent's lifecycle phase is
// suspended.
func isAgentPhaseSuspended(agent *store.Agent) bool {
	return agent.Phase == string(state.PhaseSuspended)
}

// issuerControlsAgent reports whether userID is the agent's current owner
// or a user in its recorded ancestry, read from the stored agent row.
func issuerControlsAgent(agent *store.Agent, userID string) bool {
	if agent == nil || userID == "" {
		return false
	}
	return agent.OwnerID == userID || slices.Contains(agent.Ancestry, userID)
}

// delegatedChainCode checks the delegated request's revocation chain and
// actor state (.design/agent-delegation.md §11.2 steps 1-5c) against the
// rows the middleware loaded. It returns "" when every check passes and the
// agent delegation code of the first failure otherwise.
func (a *AuthzService) delegatedChainCode(ctx context.Context, st *delegatedRequestState) string {
	h := a.agentDelegation
	now := h.now()
	c, g := st.credential, st.grant
	if c == nil || g == nil {
		return agentDelegationCodeStateMissing
	}
	id := st.identity
	// 1. The credential is active, unexpired and for this hub.
	if c.RevokedAt != nil || !now.Before(c.ExpiresAt) {
		return agentDelegationCodeCredentialInactive
	}
	if aud := h.audience(); aud == "" || c.Audience != aud {
		return agentDelegationCodeInvalidAudience
	}
	// 2. The grant is the credential's, bound to the same agent, active,
	// unexpired and at ceiling version 1.
	if g.ID != c.GrantID || g.AgentID != c.AgentID || g.ID != id.grantID || g.AgentID != id.agentID {
		return agentDelegationCodeGrantInactive
	}
	if g.RevokedAt != nil || !now.Before(g.ExpiresAt) || g.CeilingVersion != int(permissions.CeilingVersionV1) {
		return agentDelegationCodeGrantInactive
	}
	// 3. No parent chain in v1.
	if g.ParentGrantID != "" || g.AllowSubdelegation || g.Depth != 0 {
		return agentDelegationCodeSubdelegation
	}
	// 4. The issuer is an existing, active, local user and not a reserved
	// platform identity. Grants are issued only by a local session user
	// with a store row, so a stored issuer row is a local user.
	if st.issuer == nil || st.issuer.ID != g.IssuerUserID {
		return agentDelegationCodeIssuerInvalid
	}
	if st.issuer.Status != store.UserStatusActive {
		return agentDelegationCodeIssuerSuspended
	}
	if h.reservedIdentity(st.issuer.Email) {
		return agentDelegationCodeReservedIdentity
	}
	// 5. The actor agent: standing, not suspended, same project and
	// generation as at issuance, no reincarnation in flight.
	agent := st.agent
	if agent == nil || agent.ID != g.AgentID || !agent.DeletedAt.IsZero() {
		return agentDelegationCodeGrantAgentChanged
	}
	if err := h.standing(ctx, agent.ID); err != nil {
		if errors.Is(err, errAgentNotInStanding) {
			return agentDelegationCodeGrantAgentChanged
		}
		return agentDelegationCodeLookupError
	}
	if isAgentPhaseSuspended(agent) || agent.ProjectID != g.AgentProjectID ||
		agent.Generation != g.AgentGeneration || reincarnationInFlight(agent) {
		return agentDelegationCodeGrantAgentChanged
	}
	// 5a. The issuer still controls the agent (stored row only).
	if !issuerControlsAgent(agent, g.IssuerUserID) {
		return agentDelegationCodeIssuerNotController
	}
	// 5b. The issuer is still admitted to the agent's project.
	if isNilIdentity(st.issuerPC.Identity) {
		return agentDelegationCodeIssuerInvalid
	}
	adm, err := a.ProjectTargetAdmission(ctx, st.issuerPC, agent.ProjectID, agentDelegationCreatePermission, agentResource(agent), st.memo)
	if err != nil || !adm.Admitted {
		return agentDelegationCodeIssuerProjectAccess
	}
	// 5c. The exchange agent credential is the one recorded, unrevoked for
	// any reason, and unexpired.
	ac := st.exchangeCred
	if ac == nil || ac.ID != c.ExchangeAgentCredentialID || ac.AgentID != agent.ID ||
		ac.RevokedAt != nil || !now.Before(ac.ExpiresAt) {
		return agentDelegationCodeAgentCredentialInvalid
	}
	return ""
}

// bearerStageAgentDelegationCode maps an EvaluateBearerCeiling deny stage to
// its agent delegation code (.design/agent-delegation.md §11.2).
func bearerStageAgentDelegationCode(stage string) string {
	switch stage {
	case BearerStageBoundaryInvalid:
		return agentDelegationCodeGrantBoundaryInvalid
	case BearerStageTargetUnknown:
		return agentDelegationCodeTargetUnknown
	case BearerStageOutsideBoundary:
		return agentDelegationCodeOutsideBoundary
	case BearerStageCeiling:
		return agentDelegationCodeOutsideCeiling
	case BearerStageBoundaryEligibility:
		return agentDelegationCodeOutsideBoundaryEligibility
	case BearerStageProjectAccess:
		return agentDelegationCodeIssuerProjectAccess
	case BearerStageAuthority:
		return agentDelegationCodeIssuerAuthority
	default:
		return agentDelegationCodeEvaluationError
	}
}

// decideAgentDelegation is the only decision procedure for a request made
// with an agent delegated credential (.design/agent-delegation.md §11.2).
// decide routes every matched delegated principal/credential pair here
// after its classification block; none of the ordinary pipeline's steps
// run for it. Authority comes only from the grant: the request is allowed
// only when the route admits the credential, the permission is the
// operation's own, the credential ceiling and the delegation policy both
// hold it, the whole revocation chain is live, and the issuer's live
// authority on the resolved target allows it (EvaluateBearerCeiling).
// Every decision is marked AlwaysAudit; every deny carries
// DeniedByAgentDelegation. Any lookup error denies.
func (a *AuthzService) decideAgentDelegation(ctx context.Context, request AuthzRequest, _ *ProjectAdmissionCache) Decision {
	identity, _ := request.Principal.Identity.(*DelegatedAgentIdentity)
	principal := principalContextForIdentity(request.Principal.Identity)
	credential := credentialContextForIdentity(request.Principal.Identity)
	attribution := func(code string) *AgentDelegationAttribution {
		at := &AgentDelegationAttribution{ActorKind: actorKindAgentDelegated, AgentDelegationCode: code}
		if identity != nil {
			at.ActorAgentID = identity.agentID
			at.AuthorizingUserID = identity.authorizingUserID
			at.SourceGrantID = identity.grantID
			at.ExchangeAgentCredentialID = identity.exchangeAgentCredID
		}
		return at
	}
	deny := func(code string) Decision {
		d := Decision{
			Allowed:         false,
			Reason:          "agent delegation: " + code,
			DeniedBy:        DeniedByAgentDelegation,
			AlwaysAudit:     true,
			AgentDelegation: attribution(code),
		}
		return decorateDecision(d, request, principal, credential, auditPermissionID(request))
	}

	if identity == nil {
		return deny(agentDelegationCodeCredentialNotAdmitted)
	}
	st := delegatedStateFromContext(ctx)
	if st == nil || st.identity != identity {
		// The identity must be the one the middleware built for this
		// request, with its loaded rows.
		return deny(agentDelegationCodeStateMissing)
	}

	// 0. The operation routeGuard matched in the admission table, and the
	// experiment.
	entry, ok := delegatedAdmissionFromContext(ctx)
	if !ok {
		return deny(agentDelegationCodeCredentialNotAdmitted)
	}
	if _, listed := agentDelegationAdmissionFor(entry.Method, entry.RouteID); !listed {
		return deny(agentDelegationCodeCredentialNotAdmitted)
	}
	if !a.agentDelegation.complete() || !a.agentDelegation.enabled() {
		return deny(agentDelegationCodeExperimentDisabled)
	}

	// 1-5c. The revocation chain and actor state.
	if code := a.delegatedChainCode(ctx, st); code != "" {
		return deny(code)
	}

	// 6. The catalog admits the delegated pair for the operation (defence
	// in depth: the admission table drift test pins the same fact).
	spec, ok := catalogOperationByID(entry.Operation)
	if !ok || !catalogAdmitsDelegated(spec) {
		return deny(agentDelegationCodeCredentialNotAdmitted)
	}

	// 7. Exactly one canonical permission: the explicit Permission, or the
	// table-driven resolver.
	permissionID := request.Permission
	if permissionID == "" {
		resolved, err := resolveResourcePermission(request.Resource.Type, request.Action)
		if err != nil || resolved == "" {
			return deny(agentDelegationCodePermissionUnresolved)
		}
		permissionID = resolved
	}
	// 7a. The operation's primary permission, or a listed secondary check.
	if permissionID != spec.BasePermission && !slices.Contains(entry.Secondary, permissionID) {
		return deny(agentDelegationCodeCredentialNotAdmitted)
	}

	// 8. The credential ceiling holds it, and the credential ceiling is a
	// subset of the grant's.
	ceiling := identity.Ceiling()
	grantCeiling := permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: st.grant.CeilingPermissionIDs}
	if !ceiling.Allows(permissionID) || !grantCeiling.Allows(permissionID) {
		return deny(agentDelegationCodeOutsideCeiling)
	}

	// 9. The hub agent-delegation policy holds it for the grant's boundary
	// kind.
	boundary := identity.Boundary()
	if !permissions.AgentDelegable(permissionID, permissions.BoundaryKind(boundary.Kind)) {
		return deny(agentDelegationCodePermissionNotDelegable)
	}

	// 10-11. The issuer's live authority on the resolved target, evaluated
	// as a bearer of the grant's boundary with the credential's ceiling.
	eval := a.EvaluateBearerCeiling(ctx, st.issuerPC, boundary, ceiling, permissionID, request.Resource,
		BearerOptions{Evidence: request.TargetEvidence, Memo: st.memo})
	if !eval.Decision.Allowed {
		return deny(bearerStageAgentDelegationCode(eval.Stage))
	}

	d := Decision{
		Allowed:         true,
		Reason:          "agent delegation",
		MatchedGrant:    "agent_delegation:" + st.grant.ID,
		AlwaysAudit:     true,
		AgentDelegation: attribution(""),
	}
	return decorateDecision(d, request, principal, credential, auditPermissionID(request))
}
