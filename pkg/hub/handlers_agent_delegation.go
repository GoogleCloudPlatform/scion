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
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Agent delegation error codes (.design/agent-delegation.md §8.4).
const (
	errCodeCredentialNotAdmitted     = agentDelegationCodeCredentialNotAdmitted
	errCodeAgentCredentialInvalid    = agentDelegationCodeAgentCredentialInvalid
	errCodeReservedIdentity          = agentDelegationCodeReservedIdentity
	errCodeIssuerNotController       = agentDelegationCodeIssuerNotController
	errCodeIssuerProjectAccess       = agentDelegationCodeIssuerProjectAccess
	errCodeIssuerInvalid             = agentDelegationCodeIssuerInvalid
	errCodeScopeViolation            = "scope_violation"
	errCodePermissionNotDelegable    = agentDelegationCodePermissionNotDelegable
	errCodeOutsideCeiling            = agentDelegationCodeOutsideCeiling
	errCodeGrantInactive             = agentDelegationCodeGrantInactive
	errCodeGrantAgentChanged         = agentDelegationCodeGrantAgentChanged
	errCodeAgentNotFound             = "agent_not_found"
	errCodeGrantNotFound             = "grant_not_found"
	errCodeAgentNotEligible          = "agent_not_eligible"
	errCodeAgentReincarnating        = "agent_reincarnating"
	errCodeSubdelegationNotSupported = agentDelegationCodeSubdelegation
	errCodeInvalidAudience           = agentDelegationCodeInvalidAudience
	errCodeAuditFailed               = "audit_failed"
)

// agentDelegationBoundaryRequest is the boundary of a grant request.
type agentDelegationBoundaryRequest struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

// CreateAgentDelegationRequest is the body of
// POST /api/v1/agents/{agentId}/delegations.
type CreateAgentDelegationRequest struct {
	Boundary                *agentDelegationBoundaryRequest `json:"boundary"`
	Permissions             []string                        `json:"permissions"`
	ExpiresAt               *time.Time                      `json:"expiresAt,omitempty"`
	Name                    string                          `json:"name"`
	Purpose                 string                          `json:"purpose,omitempty"`
	Labels                  map[string]string               `json:"labels,omitempty"`
	MaxCredentialTTLSeconds *int                            `json:"maxCredentialTtlSeconds,omitempty"`
	// AllowSubdelegation and ParentGrantID are refused when set; v1 has no
	// subdelegation.
	AllowSubdelegation *bool   `json:"allowSubdelegation,omitempty"`
	ParentGrantID      *string `json:"parentGrantId,omitempty"`
}

// AgentDelegationBoundary is the boundary of a grant in a response.
type AgentDelegationBoundary struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"projectId,omitempty"`
}

// AgentDelegationGrantResponse is grant metadata. It never carries a
// credential. Name, purpose and labels are issuer-supplied descriptive text.
type AgentDelegationGrantResponse struct {
	ID                      string                  `json:"id"`
	AgentID                 string                  `json:"agentId"`
	IssuerUserID            string                  `json:"issuerUserId"`
	Boundary                AgentDelegationBoundary `json:"boundary"`
	Permissions             []string                `json:"permissions"`
	CeilingVersion          int                     `json:"ceilingVersion"`
	Name                    string                  `json:"name"`
	Purpose                 string                  `json:"purpose,omitempty"`
	Labels                  map[string]string       `json:"labels,omitempty"`
	ExpiresAt               time.Time               `json:"expiresAt"`
	MaxCredentialTTLSeconds int                     `json:"maxCredentialTtlSeconds"`
	Created                 time.Time               `json:"created"`
	LastExchangedAt         *time.Time              `json:"lastExchangedAt,omitempty"`
	Status                  string                  `json:"status"`
}

// ExchangeAgentDelegationRequest is the body of
// POST /api/v1/agents/{agentId}/delegations/{grantId}/exchange.
type ExchangeAgentDelegationRequest struct {
	Audience    string   `json:"audience"`
	Permissions []string `json:"permissions,omitempty"`
	TTLSeconds  *int     `json:"ttlSeconds,omitempty"`
}

// ExchangeAgentDelegationResponse is the only response that carries a
// delegated credential. It is sent once, with Cache-Control: no-store.
type ExchangeAgentDelegationResponse struct {
	Token        string    `json:"token"`
	ExpiresAt    time.Time `json:"expiresAt"`
	GrantID      string    `json:"grantId"`
	CredentialID string    `json:"credentialId"`
	Audience     string    `json:"audience"`
	Permissions  []string  `json:"permissions"`
}

func agentDelegationGrantResponse(g *store.AgentDelegationGrant, now time.Time) AgentDelegationGrantResponse {
	status := "active"
	switch {
	case g.RevokedAt != nil:
		status = "revoked"
	case !now.Before(g.ExpiresAt):
		status = "expired"
	}
	return AgentDelegationGrantResponse{
		ID:                      g.ID,
		AgentID:                 g.AgentID,
		IssuerUserID:            g.IssuerUserID,
		Boundary:                AgentDelegationBoundary{Kind: g.BoundaryKind, ProjectID: g.BoundaryProjectID},
		Permissions:             append([]string(nil), g.CeilingPermissionIDs...),
		CeilingVersion:          g.CeilingVersion,
		Name:                    g.Name,
		Purpose:                 g.Purpose,
		Labels:                  g.Labels,
		ExpiresAt:               g.ExpiresAt,
		MaxCredentialTTLSeconds: g.MaxCredentialTTLSeconds,
		Created:                 g.Created,
		LastExchangedAt:         g.LastExchangedAt,
		Status:                  status,
	}
}

// handleAgentDelegations serves /api/v1/agents/{agentId}/delegations.
// Only POST (issuance) exists in this phase. While hub.agent_delegation is
// off the route answers 404 for every method.
func (s *Server) handleAgentDelegations(w http.ResponseWriter, r *http.Request, agentID string) {
	if !s.experimentEnabled(experiments.AgentDelegation) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	s.handleCreateAgentDelegation(w, r, agentID)
}

// handleAgentDelegationExchange serves
// /api/v1/agents/{agentId}/delegations/{grantId}/exchange. While
// hub.agent_delegation is off the route answers 404 for every method.
func (s *Server) handleAgentDelegationExchange(w http.ResponseWriter, r *http.Request, agentID, grantID string) {
	if !s.experimentEnabled(experiments.AgentDelegation) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	s.handleExchangeAgentDelegation(w, r, agentID, grantID)
}

// recordAgentDelegationDeny emits the decision record of a deny from
// issuance's or exchange's own checks (.design/agent-delegation.md §14.2),
// through the shared builder. It changes nothing about the response.
func (s *Server) recordAgentDelegationDeny(ctx context.Context, permissionID string, resource Resource, code string) {
	a := s.authzService
	if a == nil || a.decisionAuditEmitter == nil {
		return
	}
	identity := GetIdentityFromContext(ctx)
	principal := principalContextForIdentity(identity)
	credential := credentialContextForIdentity(identity)
	request := AuthzRequest{Principal: principal, Credential: credential, Resource: resource, Permission: permissionID, AlwaysAudit: true}
	d := decorateDecision(Decision{
		Allowed:         false,
		Reason:          "agent delegation: " + code,
		DeniedBy:        DeniedByAgentDelegation,
		AlwaysAudit:     true,
		AgentDelegation: &AgentDelegationAttribution{AgentDelegationCode: code},
	}, request, principal, credential, auditPermissionID(request))
	a.emitDecisionAudit(ctx, request, d)
}

// handleCreateAgentDelegation issues an agent delegation grant
// (.design/agent-delegation.md §6). Only an interactive session of a local
// user with a store row may issue, for an agent the user controls. Each
// rule fails closed; the order and the error codes follow §6 and §8.4.
func (s *Server) handleCreateAgentDelegation(w http.ResponseWriter, r *http.Request, agentID string) {
	ctx := r.Context()
	agentRes := Resource{Type: "agent", ID: agentID}
	refuse := func(status int, code, msg string, details map[string]interface{}) {
		s.recordAgentDelegationDeny(ctx, agentDelegationCreatePermission, agentRes, code)
		writeError(w, status, code, msg, details)
	}

	// Rule 1: credential admission. A UAT, agent JWT, federated, broker or
	// delegated credential is refused by the session gate; a dev session is
	// refused because only an *AuthenticatedUser with a store row issues.
	user, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonCredentialManagement)
	if !ok {
		return
	}
	issuer, isLocal := user.(*AuthenticatedUser)
	if !isLocal || issuer == nil || GetCredentialContextFromContext(ctx).Kind != CredentialKindInteractive {
		refuse(http.StatusForbidden, errCodeCredentialNotAdmitted, "agent delegation requires an interactive user session", nil)
		return
	}
	if isReservedPlatformIdentity(issuer.Email(), s.platformAuthSA) {
		refuse(http.StatusForbidden, errCodeReservedIdentity, "this identity may not issue agent delegations", nil)
		return
	}
	issuerRow, err := s.store.GetUser(ctx, issuer.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			refuse(http.StatusForbidden, errCodeCredentialNotAdmitted, "agent delegation requires a local user", nil)
			return
		}
		InternalError(w)
		return
	}
	if issuerRow.Status != store.UserStatusActive {
		refuse(http.StatusForbidden, errCodeCredentialNotAdmitted, "agent delegation requires an active user", nil)
		return
	}

	// Request shape, metadata, lifetime and subdelegation (rules 5.3, 6, 7
	// and the E.1 metadata rules): 400s that never echo a value.
	var req CreateAgentDelegationRequest
	if err := readJSONStrict(r, &req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}
	if (req.AllowSubdelegation != nil && *req.AllowSubdelegation) || req.ParentGrantID != nil {
		refuse(http.StatusBadRequest, errCodeSubdelegationNotSupported, "subdelegation is not supported", nil)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		ValidationError(w, "name is required", map[string]interface{}{"field": "name"})
		return
	}
	if err := ValidateCredentialMetadata(req.Name, req.Purpose, req.Labels); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	now := time.Now().UTC()
	expiresAt := now.Add(agentDelegationGrantDefaultLifetime)
	if req.ExpiresAt != nil {
		expiresAt = req.ExpiresAt.UTC()
	}
	if !expiresAt.After(now) || expiresAt.After(now.Add(agentDelegationGrantMaxLifetime)) {
		ValidationError(w, "expiresAt must be in the future and at most 30 days ahead", map[string]interface{}{"field": "expiresAt"})
		return
	}
	maxTTL := agentDelegationCredentialDefaultTTL
	if req.MaxCredentialTTLSeconds != nil {
		maxTTL = *req.MaxCredentialTTLSeconds
	}
	if maxTTL <= 0 || maxTTL > agentDelegationCredentialMaxTTL {
		ValidationError(w, "maxCredentialTtlSeconds must be between 1 and 3600", map[string]interface{}{"field": "maxCredentialTtlSeconds"})
		return
	}

	// Rule 2: agent binding.
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil || agent == nil || !agent.DeletedAt.IsZero() {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			InternalError(w)
			return
		}
		refuse(http.StatusNotFound, errCodeAgentNotFound, "agent not found", nil)
		return
	}
	agentRes = agentResource(agent)
	if err := s.agentStanding(ctx, agent.ID); err != nil {
		if !errors.Is(err, errAgentNotInStanding) {
			InternalError(w)
			return
		}
		refuse(http.StatusConflict, errCodeAgentNotEligible, "agent is not eligible for delegation", nil)
		return
	}
	if isAgentPhaseSuspended(agent) || len(agent.Ancestry) == 0 || agent.ProjectID == "" {
		refuse(http.StatusConflict, errCodeAgentNotEligible, "agent is not eligible for delegation", nil)
		return
	}
	issuerPC := principalContextForIdentity(issuer)
	if !issuerControlsAgent(agent, issuer.ID()) {
		refuse(http.StatusForbidden, errCodeIssuerNotController, "only the agent's owner or ancestor may delegate to it", nil)
		return
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  issuerPC,
		Credential: credentialContextForIdentity(issuer),
		Resource:   agentRes,
		Action:     Action(permissions.ActionDelegationCreate),
		Permission: agentDelegationCreatePermission,
	})
	if !decision.Allowed {
		// Decide already wrote this check's decision record, with the
		// pipeline's own denied_by; no second record.
		writeError(w, http.StatusForbidden, errCodeIssuerNotController, "only the agent's owner or ancestor may delegate to it", nil)
		return
	}

	// Rule 3: the issuer is admitted to the agent's project.
	adm, err := s.authzService.ProjectTargetAdmission(ctx, issuerPC, agent.ProjectID, agentDelegationCreatePermission, agentRes, nil)
	if err != nil || !adm.Admitted {
		refuse(http.StatusForbidden, errCodeIssuerProjectAccess, "the issuer is not admitted to the agent's project", nil)
		return
	}

	// Rule 4: boundary shape (400) and mint eligibility (403 forbidden).
	if req.Boundary == nil {
		ValidationError(w, "boundary is required", map[string]interface{}{"field": "boundary", "reason": "boundary_required"})
		return
	}
	boundary := TokenBoundary{Kind: BoundaryKind(req.Boundary.Kind), ProjectID: req.Boundary.ProjectID}
	if !boundary.Valid() || isBlankPresent(req.Boundary.ProjectID) {
		ValidationError(w, "boundary is invalid", map[string]interface{}{"field": "boundary", "reason": "boundary_invalid"})
		return
	}

	// Rule 5: resolve and freeze the selectors.
	selectors := expandScopes(req.Permissions)
	if len(selectors) == 0 {
		ValidationError(w, "permissions must not be empty", map[string]interface{}{"field": "permissions"})
		return
	}
	eligibility, err := s.authzService.CanMintSelector(ctx, issuerPC, boundary, selectors)
	if err != nil {
		refuse(http.StatusForbidden, ErrCodeForbidden, "forbidden", nil)
		return
	}
	for _, result := range eligibility {
		if result.OK {
			continue
		}
		if result.Reason == MintDenialProjectAccessRequired {
			// The uniform answer UAT mint gives, so the response does not
			// confirm whether the project exists.
			refuse(http.StatusForbidden, ErrCodeForbidden, "forbidden", nil)
			return
		}
		refuse(http.StatusForbidden, errCodeScopeViolation, "a requested permission is not eligible",
			map[string]interface{}{"selector": result.Selector, "reason": string(result.Reason)})
		return
	}
	ceiling, ok := permissions.BuildCeilingFromSelectors(selectors)
	if !ok || len(ceiling.PermissionIDs) == 0 || ceiling.Version != permissions.CeilingVersionV1 {
		refuse(http.StatusForbidden, errCodeScopeViolation, "a requested permission is not eligible", nil)
		return
	}
	for _, id := range ceiling.PermissionIDs {
		if !permissions.AgentDelegable(id, permissions.BoundaryKind(boundary.Kind)) {
			refuse(http.StatusForbidden, errCodePermissionNotDelegable, "a requested permission may not be delegated to an agent", nil)
			return
		}
	}

	// Rules 9 and 10: lock the agent row, re-check it, and write the grant
	// with its mutation audit in one transaction.
	grant := &store.AgentDelegationGrant{
		ID:                      uuid.New().String(),
		AgentID:                 agent.ID,
		AgentProjectID:          agent.ProjectID,
		AgentGeneration:         agent.Generation,
		AgentStateVersion:       agent.StateVersion,
		IssuerUserID:            issuer.ID(),
		BoundaryKind:            string(boundary.Kind),
		BoundaryProjectID:       boundary.ProjectID,
		CeilingVersion:          int(ceiling.Version),
		CeilingPermissionIDs:    ceiling.PermissionIDs,
		Name:                    strings.TrimSpace(req.Name),
		Purpose:                 strings.TrimSpace(req.Purpose),
		Labels:                  req.Labels,
		MaxCredentialTTLSeconds: maxTTL,
		ExpiresAt:               expiresAt,
		Created:                 now,
		IssuanceAuditID:         uuid.New().String(),
	}
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.LockAgentRows(ctx, []string{agent.ID}); err != nil {
			return fmt.Errorf("lock agent: %w", err)
		}
		locked, err := tx.GetAgent(ctx, agent.ID)
		if err != nil {
			return fmt.Errorf("re-read agent: %w", err)
		}
		if reincarnationInFlight(locked) || locked.StateVersion != agent.StateVersion ||
			locked.Generation != agent.Generation || !locked.DeletedAt.IsZero() {
			return errAgentDelegationAgentChanged
		}
		if err := tx.CreateAgentDelegationGrant(ctx, grant); err != nil {
			return fmt.Errorf("create grant: %w", err)
		}
		record := &store.MutationAuditRecord{
			ID:                grant.IssuanceAuditID,
			Timestamp:         now,
			MutationType:      mutationAgentDelegationGrantCreate,
			TargetType:        auditTargetAgentDelegationGrant,
			TargetID:          grant.ID,
			AfterSummary:      agentDelegationGrantSummary(grant),
			ActorAgentID:      grant.AgentID,
			AuthorizingUserID: grant.IssuerUserID,
			SourceGrantID:     grant.ID,
		}
		auditActorFromContext(ctx).ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("%w: %w", errAgentDelegationAuditFailed, err)
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errAgentDelegationAgentChanged):
			refuse(http.StatusConflict, errCodeAgentReincarnating, "the agent changed during issuance; retry", nil)
		case errors.Is(err, errAgentDelegationAuditFailed):
			slog.Error("agent delegation grant not issued: audit write failed", "agent_id", agent.ID, "error", err)
			writeError(w, http.StatusInternalServerError, errCodeAuditFailed, "the grant could not be recorded; nothing was issued", nil)
		default:
			slog.Error("agent delegation grant not issued", "agent_id", agent.ID, "error", err)
			InternalError(w)
		}
		return
	}
	writeJSON(w, http.StatusCreated, agentDelegationGrantResponse(grant, now))
}

var (
	errAgentDelegationAgentChanged = errors.New("agent changed during agent delegation issuance")
	errAgentDelegationAuditFailed  = errors.New("agent delegation audit write failed")
)

// handleExchangeAgentDelegation exchanges a grant for a delegated
// credential (.design/agent-delegation.md §8). Only the bound agent, with
// its own agent JWT whose credential row is live, may exchange. The checks
// run in the order of §8.2 and each fails closed.
func (s *Server) handleExchangeAgentDelegation(w http.ResponseWriter, r *http.Request, pathAgentID, grantID string) {
	ctx := r.Context()
	agentRes := Resource{Type: "agent", ID: pathAgentID}
	// refuseWithReason answers with the external code and records the
	// precise reason in the decision record (§8.2: the record says which
	// check applied where the external code is coarser).
	refuseWithReason := func(status int, code, reason, msg string) {
		s.recordAgentDelegationDeny(ctx, agentDelegationExchangePermission, agentRes, reason)
		writeError(w, status, code, msg, nil)
	}
	refuse := func(status int, code, msg string) { refuseWithReason(status, code, code, msg) }

	// 1. A local agent JWT, nothing else.
	identity := GetIdentityFromContext(ctx)
	agentIdent, isAgentJWT := identity.(*agentIdentityWrapper)
	if !isAgentJWT || agentIdent == nil || agentIdent.AgentTokenClaims == nil ||
		GetCredentialContextFromContext(ctx).Kind != CredentialKindAgentJWT || !AncestryIsHubAttested(identity) {
		refuse(http.StatusForbidden, errCodeCredentialNotAdmitted, "exchange requires the agent's own agent token")
		return
	}
	// 2. Re-load the agent credential row; a token with no row (legacy),
	// a revoked or expired row, or a lookup error cannot exchange.
	now := time.Now().UTC()
	ac, err := s.store.GetAgentCredentialByJTIHash(ctx, hashJTI(agentIdent.Claims.ID))
	if err != nil || ac == nil || ac.RevokedAt != nil || !now.Before(ac.ExpiresAt) || ac.AgentID != agentIdent.ID() {
		refuse(http.StatusUnauthorized, errCodeAgentCredentialInvalid, "the agent credential is not valid for exchange")
		return
	}
	// 3. The path agent is the token's subject.
	if pathAgentID != agentIdent.ID() {
		refuse(http.StatusForbidden, errCodeCredentialNotAdmitted, "exchange requires the agent's own agent token")
		return
	}

	var req ExchangeAgentDelegationRequest
	if err := readJSONStrict(r, &req); err != nil {
		ValidationError(w, "invalid request body", nil)
		return
	}

	// 4. The grant, filtered by the subject: a missing grant and another
	// agent's grant answer the same 404.
	grant, err := s.store.GetAgentDelegationGrant(ctx, grantID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		InternalError(w)
		return
	}
	if grant == nil {
		refuse(http.StatusNotFound, errCodeGrantNotFound, "grant not found")
		return
	}
	if grant.AgentID != agentIdent.ID() {
		refuseWithReason(http.StatusNotFound, errCodeGrantNotFound, agentDelegationReasonGrantOtherAgent, "grant not found")
		return
	}

	// 5. Actor state, from the store row.
	agent, err := optionalRow(s.store.GetAgent(ctx, grant.AgentID))
	if err != nil {
		InternalError(w)
		return
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		refuse(http.StatusForbidden, errCodeGrantAgentChanged, "the agent no longer matches the grant")
		return
	}
	agentRes = agentResource(agent)
	if err := s.agentStanding(ctx, agent.ID); err != nil {
		if !errors.Is(err, errAgentNotInStanding) {
			InternalError(w)
			return
		}
		refuse(http.StatusForbidden, errCodeGrantAgentChanged, "the agent no longer matches the grant")
		return
	}
	if isAgentPhaseSuspended(agent) || agent.ProjectID != grant.AgentProjectID ||
		agent.Generation != grant.AgentGeneration || reincarnationInFlight(agent) {
		refuse(http.StatusForbidden, errCodeGrantAgentChanged, "the agent no longer matches the grant")
		return
	}
	// 6. The issuer still controls the agent (stored row, never JWT claims).
	if !issuerControlsAgent(agent, grant.IssuerUserID) {
		refuse(http.StatusForbidden, errCodeIssuerNotController, "the grant issuer no longer controls the agent")
		return
	}
	// 7. The issuer is still admitted to the agent's project.
	issuerRow, err := optionalRow(s.store.GetUser(ctx, grant.IssuerUserID))
	if err != nil {
		InternalError(w)
		return
	}
	if issuerRow == nil {
		refuseWithReason(http.StatusForbidden, errCodeIssuerInvalid, agentDelegationReasonIssuerMissing, "the grant issuer is not valid")
		return
	}
	issuerPC := issuerPrincipal(issuerRow)
	adm, err := s.authzService.ProjectTargetAdmission(ctx, issuerPC, agent.ProjectID, agentDelegationCreatePermission, agentRes, nil)
	if err != nil || !adm.Admitted {
		refuse(http.StatusForbidden, errCodeIssuerProjectAccess, "the grant issuer is not admitted to the agent's project")
		return
	}
	// 8. Grant state.
	if grant.RevokedAt != nil || !now.Before(grant.ExpiresAt) ||
		grant.CeilingVersion != int(permissions.CeilingVersionV1) || grant.ParentGrantID != "" {
		refuse(http.StatusForbidden, errCodeGrantInactive, "the grant is not active")
		return
	}
	// 9. Issuer state. A reserved platform identity denies and revokes.
	if issuerRow.Status != store.UserStatusActive {
		refuseWithReason(http.StatusForbidden, errCodeIssuerInvalid, agentDelegationCodeIssuerSuspended, "the grant issuer is not valid")
		return
	}
	if isReservedPlatformIdentity(issuerRow.Email, s.platformAuthSA) {
		s.revokeAgentDelegationGrantBySystem(ctx, grant, revokeReasonReservedIdentity)
		refuseWithReason(http.StatusForbidden, errCodeIssuerInvalid, agentDelegationCodeReservedIdentity, "the grant issuer is not valid")
		return
	}
	// 10. The policy, which may have narrowed since issuance.
	boundaryKind := permissions.BoundaryKind(grant.BoundaryKind)
	var effective []string
	for _, id := range grant.CeilingPermissionIDs {
		if permissions.AgentDelegable(id, boundaryKind) {
			effective = append(effective, id)
		}
	}
	if len(effective) == 0 {
		refuse(http.StatusForbidden, errCodePermissionNotDelegable, "no granted permission may be delegated")
		return
	}
	// 11. The audience.
	audience := s.agentDelegationAudience()
	if req.Audience != audience {
		refuse(http.StatusBadRequest, errCodeInvalidAudience, "audience is not this hub's")
		return
	}
	// 12. The requested permissions are a subset of the effective ceiling.
	requested := effective
	if len(req.Permissions) > 0 {
		requested = nil
		for _, selector := range expandScopes(req.Permissions) {
			sel, ok := permissions.ResolveSelector(selector)
			if !ok {
				ValidationError(w, "unknown permission selector", map[string]interface{}{"field": "permissions"})
				return
			}
			for _, id := range sel.PermissionIDs {
				if !slices.Contains(effective, id) {
					refuse(http.StatusForbidden, errCodeOutsideCeiling, "a requested permission is outside the grant")
					return
				}
				if !slices.Contains(requested, id) {
					requested = append(requested, id)
				}
			}
		}
	}
	// 14. TTL: never past the grant, the hub maximum, or the agent
	// credential the exchange was made with.
	ttl := grant.MaxCredentialTTLSeconds
	if req.TTLSeconds != nil {
		if *req.TTLSeconds <= 0 || *req.TTLSeconds > agentDelegationCredentialMaxTTL {
			ValidationError(w, "ttlSeconds must be between 1 and 3600", map[string]interface{}{"field": "ttlSeconds"})
			return
		}
		ttl = min(ttl, *req.TTLSeconds)
	}
	ttl = min(ttl, agentDelegationCredentialMaxTTL)
	expiresAt := now.Add(time.Duration(ttl) * time.Second)
	if grant.ExpiresAt.Before(expiresAt) {
		expiresAt = grant.ExpiresAt
	}
	if ac.ExpiresAt.Before(expiresAt) {
		expiresAt = ac.ExpiresAt
	}
	if !expiresAt.After(now) {
		refuse(http.StatusForbidden, errCodeGrantInactive, "the grant is not active")
		return
	}

	// 15. Write the credential (hash only), last_exchanged_at and the
	// mutation audit in one transaction.
	token, hash, err := newDelegatedCredentialToken()
	if err != nil {
		InternalError(w)
		return
	}
	cred := &store.AgentDelegatedCredential{
		ID:                        uuid.New().String(),
		GrantID:                   grant.ID,
		AgentID:                   agent.ID,
		KeyHash:                   hash,
		Prefix:                    delegatedCredentialPrefix,
		Audience:                  audience,
		CeilingPermissionIDs:      requested,
		ExchangeAgentCredentialID: ac.ID,
		IssuedAt:                  now,
		ExpiresAt:                 expiresAt,
	}
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateAgentDelegatedCredential(ctx, cred); err != nil {
			return fmt.Errorf("create delegated credential: %w", err)
		}
		if err := tx.MarkAgentDelegationGrantExchanged(ctx, grant.ID, now); err != nil {
			return fmt.Errorf("mark grant exchanged: %w", err)
		}
		after, _ := json.Marshal(map[string]interface{}{
			"credential_id": cred.ID,
			"grant_id":      grant.ID,
			"permissions":   cred.CeilingPermissionIDs,
			"expires_at":    expiresAt.Format(time.RFC3339),
		})
		record := &store.MutationAuditRecord{
			Timestamp:                 now,
			MutationType:              mutationAgentDelegationCredentialIssue,
			TargetType:                auditTargetAgentDelegatedCredential,
			TargetID:                  cred.ID,
			AfterSummary:              string(after),
			ActorAgentID:              agent.ID,
			AuthorizingUserID:         grant.IssuerUserID,
			SourceGrantID:             grant.ID,
			ExchangeAgentCredentialID: ac.ID,
			ActorKind:                 actorKindAgentDelegated,
		}
		auditActorFromContext(ctx).ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("%w: %w", errAgentDelegationAuditFailed, err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAgentDelegationAuditFailed) {
			slog.Error("delegated credential not issued: audit write failed", "grant_id", grant.ID, "error", err)
			writeError(w, http.StatusInternalServerError, errCodeAuditFailed, "the credential could not be recorded; nothing was issued", nil)
			return
		}
		slog.Error("delegated credential not issued", "grant_id", grant.ID, "error", err)
		InternalError(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, ExchangeAgentDelegationResponse{
		Token:        token,
		ExpiresAt:    expiresAt,
		GrantID:      grant.ID,
		CredentialID: cred.ID,
		Audience:     audience,
		Permissions:  append([]string(nil), cred.CeilingPermissionIDs...),
	})
}
