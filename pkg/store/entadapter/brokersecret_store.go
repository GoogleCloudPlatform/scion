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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerjointoken"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokersecret"
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
		BrokerID:  t.ID.String(),
		TokenHash: t.TokenHash,
		ExpiresAt: t.ExpiresAt,
		CreatedAt: t.Created,
		CreatedBy: t.CreatedBy,
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

	create := s.client.BrokerJoinToken.Create().
		SetID(uid).
		SetTokenHash(token.TokenHash).
		SetExpiresAt(token.ExpiresAt).
		SetCreatedBy(token.CreatedBy).
		SetCreated(token.CreatedAt)

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
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

// CleanExpiredJoinTokens removes all expired join tokens.
func (s *BrokerSecretStore) CleanExpiredJoinTokens(ctx context.Context) error {
	_, err := s.client.BrokerJoinToken.Delete().
		Where(brokerjointoken.ExpiresAtLT(time.Now())).
		Exec(ctx)
	return err
}
