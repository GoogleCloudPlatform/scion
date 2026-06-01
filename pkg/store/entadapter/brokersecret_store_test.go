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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBrokerSecretStore(t *testing.T) *BrokerSecretStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewBrokerSecretStore(client)
}

func TestBrokerSecret_CreateGet(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	secret := &store.BrokerSecret{
		BrokerID:  uuid.NewString(),
		SecretKey: []byte("super-secret-hmac-key"),
	}
	require.NoError(t, bs.CreateBrokerSecret(ctx, secret))
	// Defaults applied.
	assert.Equal(t, store.BrokerSecretAlgorithmHMACSHA256, secret.Algorithm)
	assert.Equal(t, store.BrokerSecretStatusActive, secret.Status)
	assert.False(t, secret.CreatedAt.IsZero())

	got, err := bs.GetBrokerSecret(ctx, secret.BrokerID)
	require.NoError(t, err)
	assert.Equal(t, secret.BrokerID, got.BrokerID)
	assert.Equal(t, []byte("super-secret-hmac-key"), got.SecretKey)
	assert.Equal(t, store.BrokerSecretAlgorithmHMACSHA256, got.Algorithm)
	assert.Equal(t, store.BrokerSecretStatusActive, got.Status)
}

func TestBrokerSecret_CreateMissingID(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	err := bs.CreateBrokerSecret(context.Background(), &store.BrokerSecret{SecretKey: []byte("k")})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestBrokerSecret_CreateDuplicate(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, bs.CreateBrokerSecret(ctx, &store.BrokerSecret{BrokerID: id, SecretKey: []byte("k1")}))
	err := bs.CreateBrokerSecret(ctx, &store.BrokerSecret{BrokerID: id, SecretKey: []byte("k2")})
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestBrokerSecret_GetNotFound(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	_, err := bs.GetBrokerSecret(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestBrokerSecret_GetActiveSecrets(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, bs.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: id, SecretKey: []byte("k"), Status: store.BrokerSecretStatusActive,
	}))

	active, err := bs.GetActiveSecrets(ctx, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, store.BrokerSecretStatusActive, active[0].Status)

	// A revoked secret is excluded from the active set.
	require.NoError(t, bs.UpdateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID: id, SecretKey: []byte("k"), Algorithm: store.BrokerSecretAlgorithmHMACSHA256, Status: store.BrokerSecretStatusRevoked,
	}))
	active, err = bs.GetActiveSecrets(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, active)
}

func TestBrokerSecret_Update(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, bs.CreateBrokerSecret(ctx, &store.BrokerSecret{BrokerID: id, SecretKey: []byte("k1")}))

	rotated := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, bs.UpdateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  id,
		SecretKey: []byte("k2"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusDeprecated,
		RotatedAt: rotated,
	}))

	got, err := bs.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []byte("k2"), got.SecretKey)
	assert.Equal(t, store.BrokerSecretStatusDeprecated, got.Status)
	assert.False(t, got.RotatedAt.IsZero())
}

func TestBrokerSecret_Delete(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	id := uuid.NewString()
	require.NoError(t, bs.CreateBrokerSecret(ctx, &store.BrokerSecret{BrokerID: id, SecretKey: []byte("k")}))
	require.NoError(t, bs.DeleteBrokerSecret(ctx, id))
	_, err := bs.GetBrokerSecret(ctx, id)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, bs.DeleteBrokerSecret(ctx, id), store.ErrNotFound)
}

// =============================================================================
// Broker Join Tokens
// =============================================================================

func TestJoinToken_CreateGet(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	token := &store.BrokerJoinToken{
		BrokerID:  uuid.NewString(),
		TokenHash: "hash-" + uuid.NewString(),
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedBy: uuid.NewString(),
	}
	require.NoError(t, bs.CreateJoinToken(ctx, token))
	assert.False(t, token.CreatedAt.IsZero())

	byHash, err := bs.GetJoinToken(ctx, token.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, token.BrokerID, byHash.BrokerID)

	byBroker, err := bs.GetJoinTokenByBrokerID(ctx, token.BrokerID)
	require.NoError(t, err)
	assert.Equal(t, token.TokenHash, byBroker.TokenHash)
}

func TestJoinToken_CreateInvalid(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()
	assert.ErrorIs(t, bs.CreateJoinToken(ctx, &store.BrokerJoinToken{TokenHash: "h"}), store.ErrInvalidInput)
	assert.ErrorIs(t, bs.CreateJoinToken(ctx, &store.BrokerJoinToken{BrokerID: uuid.NewString()}), store.ErrInvalidInput)
}

func TestJoinToken_GetNotFound(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()
	_, err := bs.GetJoinToken(ctx, "missing")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = bs.GetJoinTokenByBrokerID(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestJoinToken_Delete(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	token := &store.BrokerJoinToken{
		BrokerID:  uuid.NewString(),
		TokenHash: "hash-" + uuid.NewString(),
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedBy: uuid.NewString(),
	}
	require.NoError(t, bs.CreateJoinToken(ctx, token))
	require.NoError(t, bs.DeleteJoinToken(ctx, token.BrokerID))
	_, err := bs.GetJoinTokenByBrokerID(ctx, token.BrokerID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, bs.DeleteJoinToken(ctx, token.BrokerID), store.ErrNotFound)
}

func TestJoinToken_CleanExpired(t *testing.T) {
	bs := newTestBrokerSecretStore(t)
	ctx := context.Background()

	expired := &store.BrokerJoinToken{
		BrokerID:  uuid.NewString(),
		TokenHash: "expired-" + uuid.NewString(),
		ExpiresAt: time.Now().Add(-time.Hour),
		CreatedBy: uuid.NewString(),
	}
	valid := &store.BrokerJoinToken{
		BrokerID:  uuid.NewString(),
		TokenHash: "valid-" + uuid.NewString(),
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedBy: uuid.NewString(),
	}
	require.NoError(t, bs.CreateJoinToken(ctx, expired))
	require.NoError(t, bs.CreateJoinToken(ctx, valid))

	require.NoError(t, bs.CleanExpiredJoinTokens(ctx))

	_, err := bs.GetJoinTokenByBrokerID(ctx, expired.BrokerID)
	assert.ErrorIs(t, err, store.ErrNotFound, "expired token should be cleaned")
	_, err = bs.GetJoinTokenByBrokerID(ctx, valid.BrokerID)
	assert.NoError(t, err, "valid token should remain")
}
