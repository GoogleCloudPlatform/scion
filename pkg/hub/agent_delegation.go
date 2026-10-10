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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// Agent delegation (.design/agent-delegation.md): an interactive user issues
// a grant that binds one of their agents to a boundary, an exact frozen
// permission ceiling and an expiry; the bound agent exchanges the grant,
// with its own agent credential, for a short-lived opaque delegated
// credential. Everything here is behind the hub.agent_delegation
// experiment.

const (
	// delegatedCredentialPrefix marks an agent delegated credential.
	delegatedCredentialPrefix = "scion_adt_"
	// delegatedCredentialRandomBytes is the size of the random body: 256
	// bits from crypto/rand.
	delegatedCredentialRandomBytes = 32

	// agentDelegationCreatePermission and agentDelegationExchangePermission
	// are the permissions of the issuance and exchange operations.
	agentDelegationCreatePermission   = "agent.delegation.create"
	agentDelegationExchangePermission = "agent.delegation.exchange"

	// Grant and credential lifetimes (decided by ptone, 2026-09-30).
	agentDelegationGrantDefaultLifetime = 7 * 24 * time.Hour
	agentDelegationGrantMaxLifetime     = 30 * 24 * time.Hour
	agentDelegationCredentialDefaultTTL = 900
	agentDelegationCredentialMaxTTL     = 3600

	// Mutation audit types and target types. They never collide with the
	// existing agent_delegation agent-create audit type.
	mutationAgentDelegationGrantCreate     = "agent_delegation_grant_create"
	mutationAgentDelegationGrantRevoke     = "agent_delegation_grant_revoke"
	mutationAgentDelegationCredentialIssue = "agent_delegation_credential_issue"
	auditTargetAgentDelegationGrant        = "agent_delegation_grant"
	auditTargetAgentDelegatedCredential    = "agent_delegated_credential"

	// delegatedLastSeenInterval coalesces delegated credential last_seen_at
	// writes: a credential seen less than this long ago is not written
	// again.
	delegatedLastSeenInterval = time.Minute

	// revokeReasonReservedIdentity is the revoke reason when a grant's
	// issuer turns out to be a reserved platform identity.
	revokeReasonReservedIdentity = "reserved_identity"

	// agentDelegationSystemActor names the hub itself as the actor of a
	// revocation the hub performs on its own.
	agentDelegationSystemActor = "agent_delegation"
)

// errDelegatedCredentialUnavailable marks a delegated credential whose
// status could not be determined (a store error). The middleware answers
// 503, never authenticates.
var errDelegatedCredentialUnavailable = errors.New("delegated credential status unavailable")

// errDelegatedCredentialRefused marks a delegated credential that is not
// usable: unknown, revoked, expired, for another hub, or presented while the
// experiment is off. The middleware answers 401.
var errDelegatedCredentialRefused = errors.New("delegated credential refused")

// wireAgentDelegation installs the server facts decideAgentDelegation reads.
func (s *Server) wireAgentDelegation() {
	if s.authzService == nil {
		return
	}
	s.authzService.agentDelegation = agentDelegationHooks{
		enabled:  func() bool { return s.experimentEnabled(experiments.AgentDelegation) },
		audience: s.agentDelegationAudience,
		reservedIdentity: func(email string) bool {
			return isReservedPlatformIdentity(email, s.platformAuthSA)
		},
		standing: s.agentStanding,
		now:      time.Now,
	}
}

// agentDelegationAudience is this hub's delegated-credential audience:
// "scion-hub:<hub ID>", or "scion-hub" on a hub without an ID. It is stored
// on every delegated credential and compared on every use, so a credential
// row copied to a hub instance with another ID is refused.
func (s *Server) agentDelegationAudience() string {
	if id := strings.TrimSpace(s.HubID()); id != "" {
		return "scion-hub:" + id
	}
	return "scion-hub"
}

// hashDelegatedCredential returns the stored hash of a delegated credential.
func hashDelegatedCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newDelegatedCredentialToken returns a new delegated credential and its
// hash.
func newDelegatedCredentialToken() (token, hash string, err error) {
	b := make([]byte, delegatedCredentialRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate delegated credential: %w", err)
	}
	token = delegatedCredentialPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashDelegatedCredential(token), nil
}

// wellFormedDelegatedCredential reports whether token has the delegated
// credential shape: the prefix and a base64url body of the expected length.
func wellFormedDelegatedCredential(token string) bool {
	body, ok := strings.CutPrefix(token, delegatedCredentialPrefix)
	if !ok || len(body) != base64.RawURLEncoding.EncodedLen(delegatedCredentialRandomBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil
}

// authenticateDelegatedAgentCredential is UnifiedAuthMiddleware's delegated
// credential arm (.design/agent-delegation.md §11.1). It refuses every
// delegated credential while hub.agent_delegation is off, and any
// credential that is unknown, revoked, expired or for another hub. For a
// usable credential it loads the grant, issuer, agent and exchange agent
// credential rows once, builds the delegated identity and the issuer
// principal, and returns ctx carrying the identity (set before the
// credential context, with the same pointer), the credential context and
// the request state. It never decides authorization: decideAgentDelegation
// does, on every Decide.
func (s *Server) authenticateDelegatedAgentCredential(ctx context.Context, token string) (context.Context, error) {
	if !s.experimentEnabled(experiments.AgentDelegation) {
		return nil, errDelegatedCredentialRefused
	}
	if !wellFormedDelegatedCredential(token) {
		return nil, errDelegatedCredentialRefused
	}
	st := s.store
	if st == nil {
		return nil, errDelegatedCredentialUnavailable
	}
	cred, err := st.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(token))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, errDelegatedCredentialRefused
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errDelegatedCredentialUnavailable, err)
	}
	now := time.Now()
	if cred.RevokedAt != nil || !now.Before(cred.ExpiresAt) || cred.Audience != s.agentDelegationAudience() {
		return nil, errDelegatedCredentialRefused
	}

	grant, err := st.GetAgentDelegationGrant(ctx, cred.GrantID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, errDelegatedCredentialRefused
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errDelegatedCredentialUnavailable, err)
	}
	state := &delegatedRequestState{
		identity:   newDelegatedAgentIdentity(cred, grant),
		credential: cred,
		grant:      grant,
		memo:       &ProjectAdmissionCache{},
	}
	if state.issuer, err = optionalRow(st.GetUser(ctx, grant.IssuerUserID)); err != nil {
		return nil, fmt.Errorf("%w: %w", errDelegatedCredentialUnavailable, err)
	}
	if state.agent, err = optionalRow(st.GetAgent(ctx, grant.AgentID)); err != nil {
		return nil, fmt.Errorf("%w: %w", errDelegatedCredentialUnavailable, err)
	}
	if state.exchangeCred, err = optionalRow(st.GetAgentCredentialByID(ctx, cred.ExchangeAgentCredentialID)); err != nil {
		return nil, fmt.Errorf("%w: %w", errDelegatedCredentialUnavailable, err)
	}
	if state.issuer != nil {
		// A grant whose issuer is a reserved platform identity is refused
		// and revoked; a failed revocation still refuses.
		if isReservedPlatformIdentity(state.issuer.Email, s.platformAuthSA) {
			s.revokeAgentDelegationGrantBySystem(ctx, grant, revokeReasonReservedIdentity)
			return nil, errDelegatedCredentialRefused
		}
		state.issuerPC = issuerPrincipal(state.issuer)
	}

	identity := state.identity
	ctx = contextWithIdentity(ctx, identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	ctx = contextWithAuthType(ctx, AuthTypeAgentDelegation)
	ctx = withStandingMemo(ctx)
	ctx = contextWithDelegatedState(ctx, state)

	// last_seen_at is best effort, outside the request, coalesced to one
	// write per delegatedLastSeenInterval, and never affects the decision.
	if cred.LastSeenAt == nil || now.Sub(*cred.LastSeenAt) >= delegatedLastSeenInterval {
		credID := cred.ID
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = st.UpdateAgentDelegatedCredentialLastSeen(bg, credID, time.Now())
		}()
	}
	return ctx, nil
}

// optionalRow returns row, a nil row for store.ErrNotFound, or the error.
func optionalRow[T any](row *T, err error) (*T, error) {
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// issuerPrincipal builds the issuer principal from the stored issuer row
// (.design/agent-delegation.md §11.5). It is only a function argument for
// ProjectTargetAdmission and EvaluateBearerCeiling; it is never placed in
// the request's identity slot.
func issuerPrincipal(issuer *store.User) PrincipalContext {
	user := NewAuthenticatedUser(issuer.ID, issuer.Email, issuer.DisplayName, issuer.Role, "api")
	return principalContextForIdentity(user)
}

// agentDelegationGrantSummary is the audit summary of a grant. It names the
// frozen ceiling, so the durable trail always records the delegated
// permissions. Name, purpose and label values are issuer-supplied and are
// not recorded; only whether a purpose was set and which label keys were
// used.
func agentDelegationGrantSummary(g *store.AgentDelegationGrant) string {
	fields := map[string]interface{}{
		"grant_id":                   g.ID,
		"agent_id":                   g.AgentID,
		"agent_project_id":           g.AgentProjectID,
		"issuer_user_id":             g.IssuerUserID,
		"boundary_kind":              g.BoundaryKind,
		"ceiling_version":            g.CeilingVersion,
		"permissions":                g.CeilingPermissionIDs,
		"expires_at":                 g.ExpiresAt.UTC().Format(time.RFC3339),
		"max_credential_ttl_seconds": g.MaxCredentialTTLSeconds,
	}
	if g.BoundaryProjectID != "" {
		fields["boundary_project_id"] = g.BoundaryProjectID
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	summary := string(b)
	if g.Purpose != "" || len(g.Labels) > 0 {
		summary = appendCredentialMetadataAuditFields(summary, g.Purpose != "", g.Labels)
	}
	return summary
}

// revokeAgentDelegationGrantBySystem revokes grant and its credentials
// with reason, as the hub itself, writing the revocation's mutation audit
// in the same transaction. It is best effort: a failure is logged and the
// caller still refuses the request.
func (s *Server) revokeAgentDelegationGrantBySystem(ctx context.Context, grant *store.AgentDelegationGrant, reason string) {
	if grant == nil || grant.RevokedAt != nil || s.store == nil {
		return
	}
	now := time.Now().UTC()
	auditID := uuid.New().String()
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		revoked, err := tx.RevokeAgentDelegationGrant(ctx, grant.ID, agentDelegationSystemActor, reason, auditID, now)
		if err != nil || !revoked {
			return err
		}
		before, _ := json.Marshal(map[string]string{"grant_id": grant.ID, "revoke_reason": reason})
		record := &store.MutationAuditRecord{
			ID:                 auditID,
			Timestamp:          now,
			MutationType:       mutationAgentDelegationGrantRevoke,
			ActorPrincipalKind: "system",
			ActorPrincipalID:   agentDelegationSystemActor,
			TargetType:         auditTargetAgentDelegationGrant,
			TargetID:           grant.ID,
			BeforeSummary:      string(before),
			AfterSummary:       "revoked",
			CorrelationID:      auditActorFromContext(ctx).CorrelationID,
			ActorAgentID:       grant.AgentID,
			AuthorizingUserID:  grant.IssuerUserID,
			SourceGrantID:      grant.ID,
		}
		return tx.CreateMutationAudit(ctx, record)
	})
	if err != nil {
		slog.Error("agent delegation grant revocation failed", "grant_id", grant.ID, "reason", reason, "error", err)
	}
}
