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

package entadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentdelegatedcredential"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentdelegationgrant"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentDelegationStore implements store.AgentDelegationStore using Ent ORM.
type AgentDelegationStore struct {
	client *ent.Client
}

// NewAgentDelegationStore creates a new Ent-backed AgentDelegationStore.
func NewAgentDelegationStore(client *ent.Client) *AgentDelegationStore {
	return &AgentDelegationStore{client: client}
}

// encodePermissionIDs encodes a permission ID list as a JSON array.
func encodePermissionIDs(ids []string) (string, error) {
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodePermissionIDs decodes a JSON array of permission IDs. A value that
// does not decode yields nil, which every ceiling check treats as empty and
// so denies.
func decodePermissionIDs(raw string) []string {
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

func entAgentDelegationGrantToStore(g *ent.AgentDelegationGrant) *store.AgentDelegationGrant {
	out := &store.AgentDelegationGrant{
		ID:                      g.ID.String(),
		AgentID:                 g.AgentID,
		AgentProjectID:          g.AgentProjectID,
		AgentGeneration:         g.AgentGeneration,
		AgentStateVersion:       g.AgentStateVersion,
		IssuerUserID:            g.IssuerUserID,
		BoundaryKind:            g.BoundaryKind,
		CeilingVersion:          int(g.CeilingVersion),
		CeilingPermissionIDs:    decodePermissionIDs(g.CeilingPermissionIds),
		Name:                    g.Name,
		AllowSubdelegation:      g.AllowSubdelegation,
		Depth:                   g.Depth,
		MaxCredentialTTLSeconds: g.MaxCredentialTTLSeconds,
		ExpiresAt:               g.ExpiresAt,
		Created:                 g.Created,
		LastExchangedAt:         g.LastExchangedAt,
		RevokedAt:               g.RevokedAt,
		IssuanceAuditID:         g.IssuanceAuditID,
	}
	if g.BoundaryProjectID != nil {
		out.BoundaryProjectID = *g.BoundaryProjectID
	}
	if g.Purpose != nil {
		out.Purpose = *g.Purpose
	}
	if g.Labels != nil {
		var labels map[string]string
		if err := json.Unmarshal([]byte(*g.Labels), &labels); err == nil {
			out.Labels = labels
		}
	}
	if g.ParentGrantID != nil {
		out.ParentGrantID = *g.ParentGrantID
	}
	if g.RevokedBy != nil {
		out.RevokedBy = *g.RevokedBy
	}
	if g.RevokeReason != nil {
		out.RevokeReason = *g.RevokeReason
	}
	if g.RevocationAuditID != nil {
		out.RevocationAuditID = *g.RevocationAuditID
	}
	return out
}

func entAgentDelegatedCredentialToStore(c *ent.AgentDelegatedCredential) *store.AgentDelegatedCredential {
	out := &store.AgentDelegatedCredential{
		ID:                        c.ID.String(),
		GrantID:                   c.GrantID,
		AgentID:                   c.AgentID,
		KeyHash:                   c.KeyHash,
		Prefix:                    c.Prefix,
		Audience:                  c.Audience,
		CeilingPermissionIDs:      decodePermissionIDs(c.CeilingPermissionIds),
		ExchangeAgentCredentialID: c.ExchangeAgentCredentialID,
		IssuedAt:                  c.IssuedAt,
		ExpiresAt:                 c.ExpiresAt,
		RevokedAt:                 c.RevokedAt,
		LastSeenAt:                c.LastSeenAt,
	}
	if c.RevokeReason != nil {
		out.RevokeReason = *c.RevokeReason
	}
	return out
}

// CreateAgentDelegationGrant inserts grant after validating its boundary,
// ceiling and subdelegation shape (store.AgentDelegationGrant.Validate).
func (s *AgentDelegationStore) CreateAgentDelegationGrant(ctx context.Context, grant *store.AgentDelegationGrant) error {
	if err := grant.Validate(); err != nil {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	ceiling, err := encodePermissionIDs(grant.CeilingPermissionIDs)
	if err != nil {
		return err
	}
	builder := s.client.AgentDelegationGrant.Create().
		SetAgentID(grant.AgentID).
		SetAgentProjectID(grant.AgentProjectID).
		SetAgentGeneration(grant.AgentGeneration).
		SetAgentStateVersion(grant.AgentStateVersion).
		SetIssuerUserID(grant.IssuerUserID).
		SetBoundaryKind(grant.BoundaryKind).
		SetNillableBoundaryProjectID(nullableString(grant.BoundaryProjectID)).
		SetCeilingVersion(int32(grant.CeilingVersion)).
		SetCeilingPermissionIds(ceiling).
		SetName(grant.Name).
		SetNillablePurpose(nullableString(grant.Purpose)).
		SetAllowSubdelegation(false).
		SetDepth(0).
		SetMaxCredentialTTLSeconds(grant.MaxCredentialTTLSeconds).
		SetExpiresAt(grant.ExpiresAt).
		SetIssuanceAuditID(grant.IssuanceAuditID)
	if len(grant.Labels) > 0 {
		b, err := json.Marshal(grant.Labels)
		if err != nil {
			return err
		}
		builder.SetLabels(string(b))
	}
	if !grant.Created.IsZero() {
		builder.SetCreated(grant.Created)
	}
	if grant.ID != "" {
		uid, err := parseUUID(grant.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}
	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	grant.ID = created.ID.String()
	grant.Created = created.Created
	return nil
}

// GetAgentDelegationGrant returns the grant with id, or store.ErrNotFound.
func (s *AgentDelegationStore) GetAgentDelegationGrant(ctx context.Context, id string) (*store.AgentDelegationGrant, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	g, err := s.client.AgentDelegationGrant.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentDelegationGrantToStore(g), nil
}

// MarkAgentDelegationGrantExchanged sets the grant's last_exchanged_at.
func (s *AgentDelegationStore) MarkAgentDelegationGrantExchanged(ctx context.Context, id string, at time.Time) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	if err := s.client.AgentDelegationGrant.UpdateOneID(uid).SetLastExchangedAt(at).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// RevokeAgentDelegationGrant revokes the grant and its unrevoked
// credentials. The grant update is conditional on revoked_at being NULL, so
// of two concurrent revocations exactly one reports revoked=true and the
// other changes nothing. The caller runs it inside WithTx together with the
// revocation's mutation audit, so the two commit or roll back together.
func (s *AgentDelegationStore) RevokeAgentDelegationGrant(ctx context.Context, id, revokedBy, reason, auditID string, at time.Time) (bool, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return false, err
	}
	upd := s.client.AgentDelegationGrant.Update().
		Where(agentdelegationgrant.IDEQ(uid), agentdelegationgrant.RevokedAtIsNil()).
		SetRevokedAt(at).
		SetRevokedBy(revokedBy).
		SetRevokeReason(reason)
	if auditID != "" {
		upd.SetRevocationAuditID(auditID)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if n == 0 {
		// Already revoked, or no such grant.
		exists, err := s.client.AgentDelegationGrant.Query().Where(agentdelegationgrant.IDEQ(uid)).Exist(ctx)
		if err != nil {
			return false, mapError(err)
		}
		if !exists {
			return false, store.ErrNotFound
		}
		return false, nil
	}
	if _, err := s.client.AgentDelegatedCredential.Update().
		Where(
			agentdelegatedcredential.GrantIDEQ(uid.String()),
			agentdelegatedcredential.RevokedAtIsNil(),
		).
		SetRevokedAt(at).
		SetRevokeReason(reason).
		Save(ctx); err != nil {
		return false, mapError(err)
	}
	return true, nil
}

// CreateAgentDelegatedCredential inserts cred.
func (s *AgentDelegationStore) CreateAgentDelegatedCredential(ctx context.Context, cred *store.AgentDelegatedCredential) error {
	if len(cred.CeilingPermissionIDs) == 0 {
		return fmt.Errorf("%w: empty delegated credential ceiling", store.ErrInvalidInput)
	}
	ceiling, err := encodePermissionIDs(cred.CeilingPermissionIDs)
	if err != nil {
		return err
	}
	builder := s.client.AgentDelegatedCredential.Create().
		SetGrantID(cred.GrantID).
		SetAgentID(cred.AgentID).
		SetKeyHash(cred.KeyHash).
		SetPrefix(cred.Prefix).
		SetAudience(cred.Audience).
		SetCeilingPermissionIds(ceiling).
		SetExchangeAgentCredentialID(cred.ExchangeAgentCredentialID).
		SetExpiresAt(cred.ExpiresAt)
	if !cred.IssuedAt.IsZero() {
		builder.SetIssuedAt(cred.IssuedAt)
	}
	if cred.ID != "" {
		uid, err := parseUUID(cred.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}
	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	cred.ID = created.ID.String()
	cred.IssuedAt = created.IssuedAt
	return nil
}

// GetAgentDelegatedCredentialByKeyHash returns the credential with keyHash.
func (s *AgentDelegationStore) GetAgentDelegatedCredentialByKeyHash(ctx context.Context, keyHash string) (*store.AgentDelegatedCredential, error) {
	if keyHash == "" {
		return nil, store.ErrNotFound
	}
	c, err := s.client.AgentDelegatedCredential.Query().
		Where(agentdelegatedcredential.KeyHashEQ(keyHash)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentDelegatedCredentialToStore(c), nil
}

// UpdateAgentDelegatedCredentialLastSeen sets the credential's last_seen_at.
func (s *AgentDelegationStore) UpdateAgentDelegatedCredentialLastSeen(ctx context.Context, id string, at time.Time) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	if err := s.client.AgentDelegatedCredential.UpdateOneID(uid).SetLastSeenAt(at).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// agentDelegationGrantFilterPredicates turns filter into query predicates.
// An empty filter is refused, so a caller can never list every grant by
// accident.
func agentDelegationGrantFilterPredicates(filter store.AgentDelegationGrantFilter) ([]predicate.AgentDelegationGrant, error) {
	var preds []predicate.AgentDelegationGrant
	if filter.AgentID != "" {
		preds = append(preds, agentdelegationgrant.AgentIDEQ(filter.AgentID))
	}
	if filter.IssuerUserID != "" {
		preds = append(preds, agentdelegationgrant.IssuerUserIDEQ(filter.IssuerUserID))
	}
	if len(preds) == 0 {
		return nil, fmt.Errorf("%w: agent delegation grant filter names no agent and no issuer", store.ErrInvalidInput)
	}
	return preds, nil
}

// ListAgentDelegationGrants returns the grants that match filter, newest
// first.
func (s *AgentDelegationStore) ListAgentDelegationGrants(ctx context.Context, filter store.AgentDelegationGrantFilter) ([]*store.AgentDelegationGrant, error) {
	preds, err := agentDelegationGrantFilterPredicates(filter)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.AgentDelegationGrant.Query().
		Where(preds...).
		Order(ent.Desc(agentdelegationgrant.FieldCreated), ent.Desc(agentdelegationgrant.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.AgentDelegationGrant, 0, len(rows))
	for _, g := range rows {
		out = append(out, entAgentDelegationGrantToStore(g))
	}
	return out, nil
}

// CountActiveAgentDelegationGrants counts the unrevoked grants that match
// filter and expire after now.
func (s *AgentDelegationStore) CountActiveAgentDelegationGrants(ctx context.Context, filter store.AgentDelegationGrantFilter, now time.Time) (int, error) {
	preds, err := agentDelegationGrantFilterPredicates(filter)
	if err != nil {
		return 0, err
	}
	preds = append(preds, agentdelegationgrant.RevokedAtIsNil(), agentdelegationgrant.ExpiresAtGT(now))
	n, err := s.client.AgentDelegationGrant.Query().Where(preds...).Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// LockAgentDelegationIssuer locks the issuer's user row (SELECT ... FOR
// UPDATE on PostgreSQL; SQLite already serializes writes, so a plain
// existence check gives the same ordering there).
func (s *AgentDelegationStore) LockAgentDelegationIssuer(ctx context.Context, userID string) error {
	uid, err := parseGetID(userID)
	if err != nil {
		return err
	}
	q := s.client.User.Query().Where(user.IDEQ(uid))
	if s.client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	exists, err := q.Exist(ctx)
	if err != nil {
		return fmt.Errorf("lock agent delegation issuer: %w", mapError(err))
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

// LockAgentDelegationGrant locks the grant row the same way and returns the
// grant as read under the lock.
func (s *AgentDelegationStore) LockAgentDelegationGrant(ctx context.Context, id string) (*store.AgentDelegationGrant, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	q := s.client.AgentDelegationGrant.Query().Where(agentdelegationgrant.IDEQ(uid))
	if s.client.Driver().Dialect() == dialect.Postgres {
		q = q.ForUpdate()
	}
	g, err := q.Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentDelegationGrantToStore(g), nil
}

// GetAgentDelegatedCredential returns the delegated credential with id.
func (s *AgentDelegationStore) GetAgentDelegatedCredential(ctx context.Context, id string) (*store.AgentDelegatedCredential, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	c, err := s.client.AgentDelegatedCredential.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentDelegatedCredentialToStore(c), nil
}

// ListActiveAgentDelegatedCredentials returns the unrevoked credentials of
// grantIDs that expire after now, oldest first.
func (s *AgentDelegationStore) ListActiveAgentDelegatedCredentials(ctx context.Context, grantIDs []string, now time.Time) ([]*store.AgentDelegatedCredential, error) {
	if len(grantIDs) == 0 {
		return nil, nil
	}
	rows, err := s.client.AgentDelegatedCredential.Query().
		Where(
			agentdelegatedcredential.GrantIDIn(grantIDs...),
			agentdelegatedcredential.RevokedAtIsNil(),
			agentdelegatedcredential.ExpiresAtGT(now),
		).
		Order(ent.Asc(agentdelegatedcredential.FieldIssuedAt), ent.Asc(agentdelegatedcredential.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.AgentDelegatedCredential, 0, len(rows))
	for _, c := range rows {
		out = append(out, entAgentDelegatedCredentialToStore(c))
	}
	return out, nil
}

// RevokeAgentDelegatedCredential revokes the credential with id. The update
// is conditional on revoked_at being NULL, so of two concurrent revocations
// exactly one reports revoked=true.
func (s *AgentDelegationStore) RevokeAgentDelegatedCredential(ctx context.Context, id, reason string, at time.Time) (bool, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return false, err
	}
	n, err := s.client.AgentDelegatedCredential.Update().
		Where(agentdelegatedcredential.IDEQ(uid), agentdelegatedcredential.RevokedAtIsNil()).
		SetRevokedAt(at).
		SetRevokeReason(reason).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if n > 0 {
		return true, nil
	}
	exists, err := s.client.AgentDelegatedCredential.Query().Where(agentdelegatedcredential.IDEQ(uid)).Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if !exists {
		return false, store.ErrNotFound
	}
	return false, nil
}

// PurgeAgentDelegatedCredentials deletes every delegated credential whose
// expiry is before expiredBefore.
func (s *AgentDelegationStore) PurgeAgentDelegatedCredentials(ctx context.Context, expiredBefore time.Time) (int, error) {
	n, err := s.client.AgentDelegatedCredential.Delete().
		Where(agentdelegatedcredential.ExpiresAtLT(expiredBefore)).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
