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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Agent delegation grant management (.design/agent-delegation.md §7): grant
// revoke, delegated credential revoke, list and read. These routes are not
// behind hub.agent_delegation, so an operator can review and end grants
// while the experiment is off (§10).

const (
	// The permissions of the management operations.
	agentDelegationRevokePermission           = "agent.delegation.revoke"
	agentDelegationCredentialRevokePermission = "agent.delegation.credential.revoke"
	agentDelegationListPermission             = "agent.delegation.list"
	agentDelegationReadPermission             = "agent.delegation.read"

	// mutationAgentDelegationCredentialRevoke is the mutation audit type of
	// a delegated credential revocation, by a caller or by supersede.
	mutationAgentDelegationCredentialRevoke = "agent_delegation_credential_revoke"

	// Revoke reasons written by the management routes and by exchange.
	// revokeReasonRevoked: a caller with control authority revoked it;
	// revokeReasonRelinquished: the bound agent gave up its own grant;
	// revokeReasonSuperseded: a newer exchange pushed the credential past
	// the per-grant cap.
	revokeReasonRevoked      = "revoked"
	revokeReasonRelinquished = "relinquished"
	revokeReasonSuperseded   = "superseded"

	// Caps (decided by ptone, 2026-09-30; §6 rule 8, §8.2 step 13).
	agentDelegationMaxActiveGrantsPerAgent      = 10
	agentDelegationMaxActiveGrantsPerIssuer     = 50
	agentDelegationMaxActiveCredentialsPerGrant = 2

	// agentDelegatedCredentialRetention is how long a delegated credential
	// row is kept after its expiry before the purge job deletes it. A
	// revoked credential also carries an expiry at most 60 minutes after
	// issue, so it is purged on the same rule. Grants are never purged.
	agentDelegatedCredentialRetention = 24 * time.Hour

	errCodeGrantLimitReached = "grant_limit_reached"
)

// agentDelegationUserSuppliedFields names the response fields that carry
// issuer-supplied text. They are descriptive only and never an identity.
var agentDelegationUserSuppliedFields = []string{"name", "purpose", "labels"}

// AgentDelegatedCredentialSummary describes one active delegated credential
// of a grant. It never carries the credential, its hash or a prefix.
type AgentDelegatedCredentialSummary struct {
	ID        string    `json:"id"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ListAgentDelegationsResponse is the body of the grant list routes.
type ListAgentDelegationsResponse struct {
	Delegations []AgentDelegationGrantResponse `json:"delegations"`
}

// agentDelegationCaller is the caller of a management route, admitted by
// admitAgentDelegationCaller. Exactly one of boundAgentID and user is set.
type agentDelegationCaller struct {
	// boundAgentID is the agent acting with its own agent token, its
	// credential row live; it acts only on grants bound to itself.
	boundAgentID string
	// user is the session caller. hubOnly is set for a dev session, which
	// counts only as hub authority: the issuer, owner and project paths do
	// not apply to it.
	user    UserIdentity
	hubOnly bool
	// decisions memoizes this request's Decide results, keyed by
	// permission and target.
	decisions map[string]bool
}

// revokedBy names the caller in a grant's revoked_by column.
func (c *agentDelegationCaller) revokedBy() string {
	if c.boundAgentID != "" {
		return "agent:" + c.boundAgentID
	}
	return c.user.Type() + ":" + c.user.ID()
}

// revokeReason is the revoke reason a caller's revocation records.
func (c *agentDelegationCaller) revokeReason() string {
	if c.boundAgentID != "" {
		return revokeReasonRelinquished
	}
	return revokeReasonRevoked
}

// admitAgentDelegationCaller applies the credential admission of the
// management routes (.design/agent-delegation.md §7). A delegated
// credential is refused. On a route under /api/v1/agents/{agentId} the
// bound agent's own agent token is admitted after exchange steps 1-3 (§8.2):
// a local agent token, its credential row re-loaded by JTI hash and live
// and unexpired, and the path agent its subject. Otherwise only an
// interactive or dev session of a user is admitted; a user access token is
// refused with the session-only reason and any other identity with 403. On
// failure the response is written and ok is false.
func (s *Server) admitAgentDelegationCaller(w http.ResponseWriter, r *http.Request, pathAgentID string, agentRoute bool) (*agentDelegationCaller, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isDelegatedAgentIdentity(identity) {
		writeError(w, http.StatusForbidden, errCodeCredentialNotAdmitted, "grant management does not accept a delegated credential", nil)
		return nil, false
	}
	if agentIdent, isAgent := identity.(*agentIdentityWrapper); isAgent && agentRoute {
		// Step 1: a local agent token.
		if agentIdent == nil || agentIdent.AgentTokenClaims == nil ||
			GetCredentialContextFromContext(ctx).Kind != CredentialKindAgentJWT || !AncestryIsHubAttested(identity) {
			writeError(w, http.StatusForbidden, errCodeCredentialNotAdmitted, "grant management requires the agent's own agent token", nil)
			return nil, false
		}
		// Step 2: the credential row exists, is unrevoked and unexpired.
		// A revoked row was already refused by the agent token middleware.
		ac, err := s.store.GetAgentCredentialByJTIHash(ctx, hashJTI(agentIdent.Claims.ID))
		if err != nil || ac == nil || ac.RevokedAt != nil || !time.Now().Before(ac.ExpiresAt) || ac.AgentID != agentIdent.ID() {
			writeError(w, http.StatusUnauthorized, errCodeAgentCredentialInvalid, "the agent credential is not valid", nil)
			return nil, false
		}
		// Step 3: the path agent is the token's subject.
		if pathAgentID != agentIdent.ID() {
			writeError(w, http.StatusForbidden, errCodeCredentialNotAdmitted, "grant management requires the agent's own agent token", nil)
			return nil, false
		}
		return &agentDelegationCaller{boundAgentID: agentIdent.ID()}, true
	}
	user, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonCredentialManagement)
	if !ok {
		return nil, false
	}
	_, isLocal := user.(*AuthenticatedUser)
	hubOnly := !isLocal || GetCredentialContextFromContext(ctx).Kind != CredentialKindInteractive
	return &agentDelegationCaller{user: user, hubOnly: hubOnly, decisions: map[string]bool{}}, true
}

// decide asks Decide for permission on target once per request.
func (s *Server) agentDelegationDecide(ctx context.Context, c *agentDelegationCaller, permission string, target Resource) bool {
	key := permission + "|" + target.Type + "|" + target.ID
	if allowed, seen := c.decisions[key]; seen {
		return allowed
	}
	allowed := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(c.user),
		Credential: credentialContextForIdentity(c.user),
		Resource:   target,
		Action:     Action(agentDelegationActionFor(permission)),
		Permission: permission,
	}).Allowed
	c.decisions[key] = allowed
	return allowed
}

// agentDelegationActionFor returns the registry action of a management
// permission.
func agentDelegationActionFor(permission string) string {
	switch permission {
	case agentDelegationRevokePermission:
		return permissions.ActionDelegationRevoke
	case agentDelegationCredentialRevokePermission:
		return permissions.ActionDelegationCredentialRevoke
	case agentDelegationListPermission:
		return permissions.ActionDelegationList
	default:
		return permissions.ActionDelegationRead
	}
}

// agentDelegationControlAllowed reports whether the caller holds control
// authority over grant for permission, evaluated against the grant's
// boundary (.design/agent-delegation.md §7):
//   - the bound agent, only through the agent token path, for its own
//     grants;
//   - the issuer, and the agent's current owner (agent is the stored row,
//     nil when it no longer exists), from the stored rows;
//   - the permission on the hub (hub admins), for any grant;
//   - the permission on project P (project owners and admins), only for a
//     grant bounded to project P. A hub-bounded grant is never reached
//     through a project, whichever project the agent lives in.
//
// A dev session counts only as hub authority.
func (s *Server) agentDelegationControlAllowed(ctx context.Context, c *agentDelegationCaller, grant *store.AgentDelegationGrant, agent *store.Agent, permission string) bool {
	if c.boundAgentID != "" {
		return grant.AgentID == c.boundAgentID
	}
	if !c.hubOnly {
		if grant.IssuerUserID == c.user.ID() {
			return true
		}
		if agent != nil && agent.ID == grant.AgentID && agent.OwnerID != "" && agent.OwnerID == c.user.ID() {
			return true
		}
	}
	if s.agentDelegationDecide(ctx, c, permission, Resource{Type: "hub", ID: "hub"}) {
		return true
	}
	if c.hubOnly || grant.BoundaryKind != string(permissions.BoundaryKindProject) || grant.BoundaryProjectID == "" {
		return false
	}
	return s.agentDelegationDecide(ctx, c, permission, Resource{Type: "project", ID: grant.BoundaryProjectID})
}

// handleAgentDelegationItem serves
// /api/v1/agents/{agentId}/delegations/{grantId}: GET reads the grant,
// DELETE revokes it.
func (s *Server) handleAgentDelegationItem(w http.ResponseWriter, r *http.Request, agentID, grantID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleReadAgentDelegation(w, r, agentID, grantID)
	case http.MethodDelete:
		s.handleRevokeAgentDelegation(w, r, agentID, grantID)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

// handleAgentDelegationCredential serves
// /api/v1/agents/{agentId}/delegations/{grantId}/credentials/{credentialId}:
// DELETE revokes one delegated credential.
func (s *Server) handleAgentDelegationCredential(w http.ResponseWriter, r *http.Request, agentID, grantID, credentialID string) {
	if r.Method != http.MethodDelete {
		MethodNotAllowed(w, http.MethodDelete)
		return
	}
	s.handleRevokeAgentDelegatedCredential(w, r, agentID, grantID, credentialID)
}

// handleUserMeDelegations serves GET /api/v1/users/me/delegations: the
// grants the session caller issued. Session only.
func (s *Server) handleUserMeDelegations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	c, ok := s.admitAgentDelegationCaller(w, r, "", false)
	if !ok {
		return
	}
	if c.hubOnly {
		// A dev session issues no grants.
		writeJSON(w, http.StatusOK, ListAgentDelegationsResponse{Delegations: []AgentDelegationGrantResponse{}})
		return
	}
	s.writeAgentDelegationList(w, r, c, store.AgentDelegationGrantFilter{IssuerUserID: c.user.ID()}, nil)
}

// handleListAgentDelegations serves GET /api/v1/agents/{agentId}/delegations:
// the agent's grants that the caller holds control authority over. Grants
// outside it are left out and not counted.
func (s *Server) handleListAgentDelegations(w http.ResponseWriter, r *http.Request, agentID string) {
	c, ok := s.admitAgentDelegationCaller(w, r, agentID, true)
	if !ok {
		return
	}
	agent, err := optionalRow(s.store.GetAgent(r.Context(), agentID))
	if err != nil {
		InternalError(w)
		return
	}
	s.writeAgentDelegationList(w, r, c, store.AgentDelegationGrantFilter{AgentID: agentID}, agent)
}

// writeAgentDelegationList answers a list route with the grants matching
// filter that the caller may list, each with its active credentials.
func (s *Server) writeAgentDelegationList(w http.ResponseWriter, r *http.Request, c *agentDelegationCaller, filter store.AgentDelegationGrantFilter, agent *store.Agent) {
	ctx := r.Context()
	grants, err := s.store.ListAgentDelegationGrants(ctx, filter)
	if err != nil {
		slog.Error("agent delegation grant list failed", "error", err)
		InternalError(w)
		return
	}
	visible := make([]*store.AgentDelegationGrant, 0, len(grants))
	for _, g := range grants {
		if s.agentDelegationControlAllowed(ctx, c, g, agent, agentDelegationListPermission) {
			visible = append(visible, g)
		}
	}
	now := time.Now().UTC()
	byGrant, err := s.activeAgentDelegatedCredentials(ctx, visible, now)
	if err != nil {
		InternalError(w)
		return
	}
	out := ListAgentDelegationsResponse{Delegations: make([]AgentDelegationGrantResponse, 0, len(visible))}
	for _, g := range visible {
		out.Delegations = append(out.Delegations, agentDelegationGrantDetailResponse(g, byGrant[g.ID], now))
	}
	writeJSON(w, http.StatusOK, out)
}

// activeAgentDelegatedCredentials returns the active credentials of grants,
// by grant ID.
func (s *Server) activeAgentDelegatedCredentials(ctx context.Context, grants []*store.AgentDelegationGrant, now time.Time) (map[string][]*store.AgentDelegatedCredential, error) {
	byGrant := map[string][]*store.AgentDelegatedCredential{}
	if len(grants) == 0 {
		return byGrant, nil
	}
	ids := make([]string, 0, len(grants))
	for _, g := range grants {
		ids = append(ids, g.ID)
	}
	creds, err := s.store.ListActiveAgentDelegatedCredentials(ctx, ids, now)
	if err != nil {
		slog.Error("agent delegation credential list failed", "error", err)
		return nil, err
	}
	for _, cred := range creds {
		byGrant[cred.GrantID] = append(byGrant[cred.GrantID], cred)
	}
	return byGrant, nil
}

// agentDelegationGrantDetailResponse is the grant metadata the list and read
// routes return: the issuance fields, the revocation fields and the active
// credentials. It never carries a credential, its hash or a prefix.
func agentDelegationGrantDetailResponse(g *store.AgentDelegationGrant, active []*store.AgentDelegatedCredential, now time.Time) AgentDelegationGrantResponse {
	resp := agentDelegationGrantResponse(g, now)
	resp.RevokedAt = g.RevokedAt
	resp.RevokedBy = g.RevokedBy
	resp.RevokeReason = g.RevokeReason
	resp.ActiveCredentials = make([]AgentDelegatedCredentialSummary, 0, len(active))
	for _, cred := range active {
		resp.ActiveCredentials = append(resp.ActiveCredentials, AgentDelegatedCredentialSummary{
			ID: cred.ID, IssuedAt: cred.IssuedAt, ExpiresAt: cred.ExpiresAt,
		})
	}
	resp.ActiveCredentialCount = len(resp.ActiveCredentials)
	return resp
}

// loadManagedAgentDelegationGrant admits the caller and loads the grant on a
// grant route. A grant that does not exist, is bound to another agent, or
// is outside the caller's authority for permission answers the same 404
// grant_not_found. When the caller lacks permission but may read the grant,
// the answer is 403 forbidden. On failure the response is written and ok
// is false.
func (s *Server) loadManagedAgentDelegationGrant(w http.ResponseWriter, r *http.Request, agentID, grantID, permission string) (*agentDelegationCaller, *store.AgentDelegationGrant, bool) {
	ctx := r.Context()
	c, ok := s.admitAgentDelegationCaller(w, r, agentID, true)
	if !ok {
		return nil, nil, false
	}
	grant, err := optionalRow(s.store.GetAgentDelegationGrant(ctx, grantID))
	if err != nil && !errors.Is(err, store.ErrInvalidInput) {
		InternalError(w)
		return nil, nil, false
	}
	if grant == nil || grant.AgentID != agentID {
		writeError(w, http.StatusNotFound, errCodeGrantNotFound, "grant not found", nil)
		return nil, nil, false
	}
	agent, err := optionalRow(s.store.GetAgent(ctx, grant.AgentID))
	if err != nil {
		InternalError(w)
		return nil, nil, false
	}
	if s.agentDelegationControlAllowed(ctx, c, grant, agent, permission) {
		return c, grant, true
	}
	if permission != agentDelegationReadPermission && s.agentDelegationControlAllowed(ctx, c, grant, agent, agentDelegationReadPermission) {
		Forbidden(w)
		return nil, nil, false
	}
	writeError(w, http.StatusNotFound, errCodeGrantNotFound, "grant not found", nil)
	return nil, nil, false
}

// handleReadAgentDelegation serves GET
// /api/v1/agents/{agentId}/delegations/{grantId}.
func (s *Server) handleReadAgentDelegation(w http.ResponseWriter, r *http.Request, agentID, grantID string) {
	_, grant, ok := s.loadManagedAgentDelegationGrant(w, r, agentID, grantID, agentDelegationReadPermission)
	if !ok {
		return
	}
	now := time.Now().UTC()
	byGrant, err := s.activeAgentDelegatedCredentials(r.Context(), []*store.AgentDelegationGrant{grant}, now)
	if err != nil {
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, agentDelegationGrantDetailResponse(grant, byGrant[grant.ID], now))
}

// agentDelegationAuditBlock fills the agent delegation block of a
// management mutation record: the grant's agent, issuer and the grant
// (.design/agent-delegation.md §14.3). When the bound agent acts with its
// own agent token the record's actor kind says so, as on the exchange
// record. ApplyActor then fills the principal, credential and correlation
// fields from the request, never overwriting these.
func agentDelegationAuditBlock(record *store.MutationAuditRecord, grant *store.AgentDelegationGrant, c *agentDelegationCaller) {
	record.ActorAgentID = grant.AgentID
	record.AuthorizingUserID = grant.IssuerUserID
	record.SourceGrantID = grant.ID
	if c != nil && c.boundAgentID != "" {
		record.ActorKind = actorKindAgentDelegated
	}
}

// handleRevokeAgentDelegation serves DELETE
// /api/v1/agents/{agentId}/delegations/{grantId}. It revokes the grant and
// every unrevoked credential exchanged from it and writes one mutation
// audit, whose ID the grant records as revocation_audit_id, all in one
// transaction. Revoking a grant that is already revoked answers 204 and
// writes nothing.
func (s *Server) handleRevokeAgentDelegation(w http.ResponseWriter, r *http.Request, agentID, grantID string) {
	ctx := r.Context()
	c, grant, ok := s.loadManagedAgentDelegationGrant(w, r, agentID, grantID, agentDelegationRevokePermission)
	if !ok {
		return
	}
	now := time.Now().UTC()
	auditID := uuid.New().String()
	reason := c.revokeReason()
	revoked := false
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		locked, err := tx.LockAgentDelegationGrant(ctx, grant.ID)
		if err != nil {
			return fmt.Errorf("lock grant: %w", err)
		}
		if locked.RevokedAt != nil {
			return nil
		}
		// Every unrevoked credential, expired or not, is revoked with the
		// grant; the audit row names them.
		creds, err := tx.ListActiveAgentDelegatedCredentials(ctx, []string{grant.ID}, time.Time{})
		if err != nil {
			return fmt.Errorf("list credentials: %w", err)
		}
		credIDs := make([]string, 0, len(creds))
		for _, cred := range creds {
			credIDs = append(credIDs, cred.ID)
		}
		if revoked, err = tx.RevokeAgentDelegationGrant(ctx, grant.ID, c.revokedBy(), reason, auditID, now); err != nil {
			return fmt.Errorf("revoke grant: %w", err)
		}
		if !revoked {
			return nil
		}
		before, _ := json.Marshal(map[string]interface{}{
			"grant_id":               grant.ID,
			"revoke_reason":          reason,
			"revoked_credential_ids": credIDs,
		})
		record := &store.MutationAuditRecord{
			ID:            auditID,
			Timestamp:     now,
			MutationType:  mutationAgentDelegationGrantRevoke,
			TargetType:    auditTargetAgentDelegationGrant,
			TargetID:      grant.ID,
			BeforeSummary: string(before),
			AfterSummary:  "revoked",
		}
		agentDelegationAuditBlock(record, grant, c)
		auditActorFromContext(ctx).ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("%w: %w", errAgentDelegationAuditFailed, err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAgentDelegationAuditFailed) {
			slog.Error("agent delegation grant not revoked: audit write failed", "grant_id", grant.ID, "error", err)
			writeError(w, http.StatusInternalServerError, errCodeAuditFailed, "the revocation could not be recorded; nothing was revoked", nil)
			return
		}
		slog.Error("agent delegation grant not revoked", "grant_id", grant.ID, "error", err)
		InternalError(w)
		return
	}
	if revoked {
		s.publishConduitAuthzChanged(conduitAuthzMatch{AgentID: grant.AgentID})
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeAgentDelegatedCredential serves DELETE
// /api/v1/agents/{agentId}/delegations/{grantId}/credentials/{credentialId}.
// It revokes one credential of the grant and writes its mutation audit in
// the same transaction. Revoking a credential that is already revoked
// answers 204 and writes nothing.
func (s *Server) handleRevokeAgentDelegatedCredential(w http.ResponseWriter, r *http.Request, agentID, grantID, credentialID string) {
	ctx := r.Context()
	c, grant, ok := s.loadManagedAgentDelegationGrant(w, r, agentID, grantID, agentDelegationCredentialRevokePermission)
	if !ok {
		return
	}
	cred, err := optionalRow(s.store.GetAgentDelegatedCredential(ctx, credentialID))
	if err != nil && !errors.Is(err, store.ErrInvalidInput) {
		InternalError(w)
		return
	}
	if cred == nil || cred.GrantID != grant.ID {
		NotFound(w, "credential")
		return
	}
	now := time.Now().UTC()
	reason := c.revokeReason()
	revoked := false
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if _, err := tx.LockAgentDelegationGrant(ctx, grant.ID); err != nil {
			return fmt.Errorf("lock grant: %w", err)
		}
		if revoked, err = tx.RevokeAgentDelegatedCredential(ctx, cred.ID, reason, now); err != nil {
			return fmt.Errorf("revoke credential: %w", err)
		}
		if !revoked {
			return nil
		}
		record := agentDelegatedCredentialRevokeRecord(grant, cred, reason, now)
		agentDelegationAuditBlock(record, grant, c)
		auditActorFromContext(ctx).ApplyActor(record)
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("%w: %w", errAgentDelegationAuditFailed, err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errAgentDelegationAuditFailed) {
			slog.Error("delegated credential not revoked: audit write failed", "grant_id", grant.ID, "credential_id", cred.ID, "error", err)
			writeError(w, http.StatusInternalServerError, errCodeAuditFailed, "the revocation could not be recorded; nothing was revoked", nil)
			return
		}
		slog.Error("delegated credential not revoked", "grant_id", grant.ID, "credential_id", cred.ID, "error", err)
		InternalError(w)
		return
	}
	if revoked {
		s.publishConduitAuthzChanged(conduitAuthzMatch{AgentID: grant.AgentID})
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentDelegatedCredentialRevokeRecord is the mutation audit of one
// delegated credential revocation. The caller fills the actor fields.
func agentDelegatedCredentialRevokeRecord(grant *store.AgentDelegationGrant, cred *store.AgentDelegatedCredential, reason string, now time.Time) *store.MutationAuditRecord {
	before, _ := json.Marshal(map[string]string{
		"credential_id": cred.ID,
		"grant_id":      grant.ID,
		"revoke_reason": reason,
	})
	return &store.MutationAuditRecord{
		Timestamp:                 now,
		MutationType:              mutationAgentDelegationCredentialRevoke,
		TargetType:                auditTargetAgentDelegatedCredential,
		TargetID:                  cred.ID,
		BeforeSummary:             string(before),
		AfterSummary:              "revoked",
		ExchangeAgentCredentialID: cred.ExchangeAgentCredentialID,
	}
}

// agentDelegatedCredentialPurgeHandler returns the recurring job that
// deletes delegated credential rows whose expiry is more than
// agentDelegatedCredentialRetention in the past (.design/agent-delegation.md
// §18.1). clock is the job's time source.
func (s *Server) agentDelegatedCredentialPurgeHandler(clock func() time.Time) func(ctx context.Context) {
	return func(ctx context.Context) {
		cutoff := clock().UTC().Add(-agentDelegatedCredentialRetention)
		purged, err := s.store.PurgeAgentDelegatedCredentials(ctx, cutoff)
		if err != nil {
			slog.Error("Scheduler: delegated credential purge failed", "error", err)
			return
		}
		if purged > 0 {
			slog.Info("Scheduler: purged expired delegated credentials", "count", purged, "cutoff", cutoff)
		}
	}
}
