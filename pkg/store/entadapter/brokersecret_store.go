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
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerjointoken"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokersecret"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// BrokerSecretStore implements store.BrokerSecretStore using Ent ORM. It backs
// runtime broker HMAC authentication: per-broker shared secrets (broker_secrets)
// and short-lived registration join tokens (broker_join_tokens).
//
// Both tables key their primary id on broker_id (one active secret / join token
// per broker), so the surrogate Ent id is stored directly in the broker_id
// column.
type BrokerSecretStore struct {
	client *ent.Client
}

// NewBrokerSecretStore creates a new Ent-backed BrokerSecretStore.
func NewBrokerSecretStore(client *ent.Client) *BrokerSecretStore {
	return &BrokerSecretStore{client: client}
}

// =============================================================================
// Broker Secret operations
// =============================================================================

// entBrokerSecretToStore converts an Ent BrokerSecret to a store model.
func entBrokerSecretToStore(b *ent.BrokerSecret) *store.BrokerSecret {
	secret := &store.BrokerSecret{
		BrokerID:  b.ID.String(),
		SecretKey: b.SecretKey,
		Algorithm: b.Algorithm,
		Status:    b.Status,
		CreatedAt: b.Created,
	}
	if b.RotatedAt != nil {
		secret.RotatedAt = *b.RotatedAt
	}
	if b.ExpiresAt != nil {
		secret.ExpiresAt = *b.ExpiresAt
	}
	return secret
}

// CreateBrokerSecret creates a new broker secret record.
func (s *BrokerSecretStore) CreateBrokerSecret(ctx context.Context, secret *store.BrokerSecret) error {
	if secret.BrokerID == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(secret.BrokerID)
	if err != nil {
		return err
	}

	if secret.CreatedAt.IsZero() {
		secret.CreatedAt = time.Now()
	}
	if secret.Algorithm == "" {
		secret.Algorithm = store.BrokerSecretAlgorithmHMACSHA256
	}
	if secret.Status == "" {
		secret.Status = store.BrokerSecretStatusActive
	}

	create := s.client.BrokerSecret.Create().
		SetID(uid).
		SetSecretKey(secret.SecretKey).
		SetAlgorithm(secret.Algorithm).
		SetStatus(secret.Status).
		SetCreated(secret.CreatedAt)
	if !secret.RotatedAt.IsZero() {
		create.SetRotatedAt(secret.RotatedAt)
	}
	if !secret.ExpiresAt.IsZero() {
		create.SetExpiresAt(secret.ExpiresAt)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetBrokerSecret retrieves a broker secret by broker ID.
func (s *BrokerSecretStore) GetBrokerSecret(ctx context.Context, brokerID string) (*store.BrokerSecret, error) {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	b, err := s.client.BrokerSecret.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entBrokerSecretToStore(b), nil
}

// GetActiveSecrets retrieves all active and deprecated secrets for a broker,
// newest first. This supports dual-secret validation during rotation grace
// periods. Because broker_id is the primary key, there is at most one row.
func (s *BrokerSecretStore) GetActiveSecrets(ctx context.Context, brokerID string) ([]*store.BrokerSecret, error) {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.BrokerSecret.Query().
		Where(
			brokersecret.IDEQ(uid),
			brokersecret.StatusIn(store.BrokerSecretStatusActive, store.BrokerSecretStatusDeprecated),
		).
		Order(ent.Desc(brokersecret.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	secrets := make([]*store.BrokerSecret, 0, len(rows))
	for _, b := range rows {
		secrets = append(secrets, entBrokerSecretToStore(b))
	}
	return secrets, nil
}

// UpdateBrokerSecret updates an existing broker secret.
func (s *BrokerSecretStore) UpdateBrokerSecret(ctx context.Context, secret *store.BrokerSecret) error {
	uid, err := parseUUID(secret.BrokerID)
	if err != nil {
		return err
	}

	update := s.client.BrokerSecret.UpdateOneID(uid).
		SetSecretKey(secret.SecretKey).
		SetAlgorithm(secret.Algorithm).
		SetStatus(secret.Status)
	if secret.RotatedAt.IsZero() {
		update.ClearRotatedAt()
	} else {
		update.SetRotatedAt(secret.RotatedAt)
	}
	if secret.ExpiresAt.IsZero() {
		update.ClearExpiresAt()
	} else {
		update.SetExpiresAt(secret.ExpiresAt)
	}

	if _, err := update.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteBrokerSecret removes a broker secret.
func (s *BrokerSecretStore) DeleteBrokerSecret(ctx context.Context, brokerID string) error {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return err
	}
	if err := s.client.BrokerSecret.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// =============================================================================
// Broker Join Token operations
// =============================================================================

// entJoinTokenToStore converts an Ent BrokerJoinToken to a store model.
func entJoinTokenToStore(t *ent.BrokerJoinToken) *store.BrokerJoinToken {
	return &store.BrokerJoinToken{
		BrokerID:   t.ID.String(),
		TokenHash:  t.TokenHash,
		ExpiresAt:  t.ExpiresAt,
		CreatedAt:  t.Created,
		CreatedBy:  t.CreatedBy,
		IssuanceID: t.IssuanceID,
	}
}

// CreateJoinToken creates a new join token for broker registration.
func (s *BrokerSecretStore) CreateJoinToken(ctx context.Context, token *store.BrokerJoinToken) error {
	if token.BrokerID == "" || token.TokenHash == "" {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(token.BrokerID)
	if err != nil {
		return err
	}

	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}
	if err := shapeJoinToken(ctx, s.client, token); err != nil {
		return err
	}

	create := s.client.BrokerJoinToken.Create().
		SetID(uid).
		SetTokenHash(token.TokenHash).
		SetExpiresAt(token.ExpiresAt).
		SetCreatedBy(token.CreatedBy).
		SetCreated(token.CreatedAt).
		SetNillableIssuanceID(token.IssuanceID)

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// UpsertJoinToken stores a join token for token.BrokerID, replacing any
// existing token for that broker. The existence check and the upsert run in
// one transaction (the caller's, when called inside store.WithTx). The
// upsert itself is a single INSERT ... ON CONFLICT (broker_id) DO UPDATE, so
// concurrent calls for the same broker never fail on the primary key.
func (s *BrokerSecretStore) UpsertJoinToken(ctx context.Context, token *store.BrokerJoinToken) (bool, error) {
	if token.BrokerID == "" || token.TokenHash == "" {
		return false, store.ErrInvalidInput
	}
	uid, err := parseUUID(token.BrokerID)
	if err != nil {
		return false, err
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		if !errors.Is(err, ent.ErrTxStarted) {
			return false, err
		}
		// Already inside store.WithTx: run on the caller's transaction.
		return upsertJoinToken(ctx, s.client, uid, token)
	}
	replaced, err := upsertJoinToken(ctx, tx.Client(), uid, token)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("broker secret store: commit UpsertJoinToken: %w", err)
	}
	return replaced, nil
}

// shapeJoinToken applies grant shaping (admin tier A3) to a join token
// about to be stored: minted by an admin-kind test identity, it expires no
// later than the identity and carries its issuance ID; any other token
// carries no issuance ID.
func shapeJoinToken(ctx context.Context, client *ent.Client, token *store.BrokerJoinToken) error {
	shaping, err := grantShapingFor(ctx, client, token.CreatedBy)
	if err != nil {
		return err
	}
	if shaping != nil {
		token.ExpiresAt = *shaping.ClampExpiry(&token.ExpiresAt)
	}
	token.IssuanceID = issuanceStamp(shaping)
	return nil
}

func upsertJoinToken(ctx context.Context, client *ent.Client, uid uuid.UUID, token *store.BrokerJoinToken) (bool, error) {
	if err := shapeJoinToken(ctx, client, token); err != nil {
		return false, err
	}
	existing, err := client.BrokerJoinToken.Query().
		Where(brokerjointoken.IDEQ(uid)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return false, mapError(err)
	}
	replaced := existing != nil
	// issuance_id is Immutable (admin tier A3): the upsert below never
	// writes it. A re-mint whose stamp differs from the stored row's
	// removes that row first, so the token inserted carries its own stamp.
	if existing != nil && !sameIssuanceStamp(existing.IssuanceID, token.IssuanceID) {
		if err := client.BrokerJoinToken.DeleteOneID(uid).Exec(ctx); err != nil && !ent.IsNotFound(err) {
			return false, mapError(err)
		}
	}
	// The conflict update replaces every column except issuance_id,
	// including created and created_by, so a re-minted token records its
	// own mint time and minter.
	err = client.BrokerJoinToken.Create().
		SetID(uid).
		SetTokenHash(token.TokenHash).
		SetExpiresAt(token.ExpiresAt).
		SetCreatedBy(token.CreatedBy).
		SetCreated(token.CreatedAt).
		SetNillableIssuanceID(token.IssuanceID).
		OnConflict(
			entsql.ConflictColumns(brokerjointoken.FieldID),
			entsql.ResolveWith(func(u *entsql.UpdateSet) {
				u.SetExcluded(brokerjointoken.FieldTokenHash)
				u.SetExcluded(brokerjointoken.FieldExpiresAt)
				u.SetExcluded(brokerjointoken.FieldCreatedBy)
				u.SetExcluded(brokerjointoken.FieldCreated)
			}),
		).
		Exec(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return replaced, nil
}

// ConsumeJoinToken deletes the join token with tokenHash if it belongs to
// brokerID and expires after now. The check and the delete are one DELETE
// statement, so of several concurrent callers only one deletes the row: on
// Postgres a second DELETE waits on the row lock and then matches nothing,
// and SQLite serializes writers.
func (s *BrokerSecretStore) ConsumeJoinToken(ctx context.Context, tokenHash, brokerID string, now time.Time) error {
	if tokenHash == "" {
		return store.ErrNotFound
	}
	uid, err := parseUUID(brokerID)
	if err != nil {
		// No token can belong to a broker ID that is not a UUID.
		return store.ErrNotFound
	}
	n, err := s.client.BrokerJoinToken.Delete().
		Where(
			brokerjointoken.TokenHashEQ(tokenHash),
			brokerjointoken.IDEQ(uid),
			brokerjointoken.ExpiresAtGT(now),
		).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteExpiredJoinToken deletes the join token with tokenHash if it has
// expired at now, in one DELETE statement.
func (s *BrokerSecretStore) DeleteExpiredJoinToken(ctx context.Context, tokenHash string, now time.Time) error {
	if tokenHash == "" {
		return store.ErrNotFound
	}
	n, err := s.client.BrokerJoinToken.Delete().
		Where(
			brokerjointoken.TokenHashEQ(tokenHash),
			brokerjointoken.ExpiresAtLTE(now),
		).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetJoinToken retrieves a join token by token hash.
func (s *BrokerSecretStore) GetJoinToken(ctx context.Context, tokenHash string) (*store.BrokerJoinToken, error) {
	t, err := s.client.BrokerJoinToken.Query().
		Where(brokerjointoken.TokenHashEQ(tokenHash)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entJoinTokenToStore(t), nil
}

// GetJoinTokenByBrokerID retrieves a join token by broker ID.
func (s *BrokerSecretStore) GetJoinTokenByBrokerID(ctx context.Context, brokerID string) (*store.BrokerJoinToken, error) {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return nil, err
	}
	t, err := s.client.BrokerJoinToken.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entJoinTokenToStore(t), nil
}

// DeleteJoinToken removes a join token by broker ID.
func (s *BrokerSecretStore) DeleteJoinToken(ctx context.Context, brokerID string) error {
	uid, err := parseUUID(brokerID)
	if err != nil {
		return err
	}
	if err := s.client.BrokerJoinToken.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteOrphanBrokerCredentials removes every broker_secrets and
// broker_join_tokens row whose broker ID has no runtime_brokers row (neither
// table has an FK to it), in one transaction (the caller's, when called
// inside store.WithTx), and returns how many of each it removed.
func (s *BrokerSecretStore) DeleteOrphanBrokerCredentials(ctx context.Context) (int, int, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		if !errors.Is(err, ent.ErrTxStarted) {
			return 0, 0, err
		}
		// Already inside store.WithTx: run on the caller's transaction.
		return deleteOrphanBrokerCredentials(ctx, s.client)
	}
	// The transaction is rolled back on every path that does not commit,
	// including a panic inside deleteOrphanBrokerCredentials.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	secrets, tokens, err := deleteOrphanBrokerCredentials(ctx, tx.Client())
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("broker secret store: commit DeleteOrphanBrokerCredentials: %w", err)
	}
	committed = true
	return secrets, tokens, nil
}

// noRuntimeBrokerRow matches a row whose column col (the broker ID) is not
// the ID of any runtime_brokers row. runtime_brokers.id is a non-null
// primary key, so NOT IN has no NULL case.
func noRuntimeBrokerRow(col string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.NotIn(s.C(col),
			entsql.Select(runtimebroker.FieldID).From(entsql.Table(runtimebroker.Table))))
	}
}

func deleteOrphanBrokerCredentials(ctx context.Context, client *ent.Client) (int, int, error) {
	secrets, err := client.BrokerSecret.Delete().Where(noRuntimeBrokerRow(brokersecret.FieldID)).Exec(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("delete orphan broker secrets: %w", mapError(err))
	}
	tokens, err := client.BrokerJoinToken.Delete().Where(noRuntimeBrokerRow(brokerjointoken.FieldID)).Exec(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("delete orphan broker join tokens: %w", mapError(err))
	}
	return secrets, tokens, nil
}

// CleanExpiredJoinTokens removes all expired join tokens and returns how
// many were removed.
func (s *BrokerSecretStore) CleanExpiredJoinTokens(ctx context.Context) (int, error) {
	n, err := s.client.BrokerJoinToken.Delete().
		Where(brokerjointoken.ExpiresAtLTE(time.Now())).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// sameIssuanceStamp reports whether two issuance stamps are equal (both
// nil, or the same ID).
func sameIssuanceStamp(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
