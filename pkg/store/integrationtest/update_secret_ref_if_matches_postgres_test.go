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

//go:build integration

package integrationtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestUpdateSecretRefIfMatches_Postgres is the Postgres counterpart of
// pkg/store/entadapter's SQLite CAS-semantics tests for
// SecretStore.UpdateSecretRefIfMatches (ptone/scion#2152 round-3 review
// finding 7; version-awareness added for round-4 review finding 1): the same
// NULL-vs-empty-string, mismatch, match, and same-ref-bumped-version
// behavior must hold on both backends.
//
// It requires SCION_TEST_POSTGRES_URL — run with:
//
//	go test -tags integration ./pkg/store/integrationtest/... \
//	  -run TestUpdateSecretRefIfMatches_Postgres
//
// enttest.NewSchemaURL skips the test entirely (not a failure) when
// SCION_TEST_POSTGRES_URL is unset, which is the case in this sandbox and
// under the broker-01 resource hold (no Postgres/docker containers) — see
// the PR body for this run's disposition: build/vet/sqlite tests ran, this
// Postgres run is pending a Postgres-capable environment.
func TestUpdateSecretRefIfMatches_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t) // skips when Postgres is not configured

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	ss := entadapter.NewSecretStore(client)

	t.Run("NilRefMatchesEmptyExpected", func(t *testing.T) {
		scopeID := uuid.New().String()
		sec := &store.Secret{ID: uuid.New().String(), Key: "CAS_NIL", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: scopeID}
		require.NoError(t, ss.CreateSecret(ctx, sec))
		require.Empty(t, sec.SecretRef)

		applied, err := ss.UpdateSecretRefIfMatches(ctx, "CAS_NIL", store.ScopeUser, scopeID, "", 1, "gcpsm:projects/p/secrets/new")
		require.NoError(t, err)
		assert.True(t, applied, "CAS with expectedRef=\"\" must match a SQL-NULL secret_ref")

		got, err := ss.GetSecret(ctx, "CAS_NIL", store.ScopeUser, scopeID)
		require.NoError(t, err)
		assert.Equal(t, "gcpsm:projects/p/secrets/new", got.SecretRef)
		assert.Equal(t, 2, got.Version)
	})

	t.Run("EmptyExpectedDoesNotMatchNonEmptyRef", func(t *testing.T) {
		scopeID := uuid.New().String()
		sec := &store.Secret{ID: uuid.New().String(), Key: "CAS_SET", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: scopeID, SecretRef: "gcpsm:projects/p/secrets/legacy"}
		require.NoError(t, ss.CreateSecret(ctx, sec))

		applied, err := ss.UpdateSecretRefIfMatches(ctx, "CAS_SET", store.ScopeUser, scopeID, "", 1, "gcpsm:projects/p/secrets/new")
		require.NoError(t, err)
		assert.False(t, applied)

		got, err := ss.GetSecret(ctx, "CAS_SET", store.ScopeUser, scopeID)
		require.NoError(t, err)
		assert.Equal(t, "gcpsm:projects/p/secrets/legacy", got.SecretRef)
		assert.Equal(t, 1, got.Version)
	})

	t.Run("MismatchFails", func(t *testing.T) {
		scopeID := uuid.New().String()
		sec := &store.Secret{ID: uuid.New().String(), Key: "CAS_MISMATCH", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: scopeID, SecretRef: "gcpsm:projects/p/secrets/legacy"}
		require.NoError(t, ss.CreateSecret(ctx, sec))

		applied, err := ss.UpdateSecretRefIfMatches(ctx, "CAS_MISMATCH", store.ScopeUser, scopeID, "gcpsm:projects/p/secrets/stale-snapshot", 1, "gcpsm:projects/p/secrets/new")
		require.NoError(t, err)
		assert.False(t, applied)

		got, err := ss.GetSecret(ctx, "CAS_MISMATCH", store.ScopeUser, scopeID)
		require.NoError(t, err)
		assert.Equal(t, "gcpsm:projects/p/secrets/legacy", got.SecretRef)
		assert.Equal(t, 1, got.Version)
	})

	// TestUpdateSecretRefIfMatches_Postgres/SameRefBumpedVersionFails
	// reproduces round-4 review finding 1: a same-ref rotation (the ref
	// string is unchanged, but Version bumped) must not match a CAS keyed
	// on the pre-rotation Version, even though the ref string alone still
	// matches.
	t.Run("SameRefBumpedVersionFails", func(t *testing.T) {
		scopeID := uuid.New().String()
		sec := &store.Secret{ID: uuid.New().String(), Key: "CAS_ABA", EncryptedValue: "v1", Scope: store.ScopeUser, ScopeID: scopeID, SecretRef: "gcpsm:projects/p/secrets/legacy"}
		require.NoError(t, ss.CreateSecret(ctx, sec))
		require.Equal(t, 1, sec.Version)

		_, err := ss.UpsertSecret(ctx, &store.Secret{
			Key: "CAS_ABA", EncryptedValue: "v2-rotated", Scope: store.ScopeUser, ScopeID: scopeID, SecretRef: "gcpsm:projects/p/secrets/legacy",
		})
		require.NoError(t, err)

		applied, err := ss.UpdateSecretRefIfMatches(ctx, "CAS_ABA", store.ScopeUser, scopeID, "gcpsm:projects/p/secrets/legacy", 1, "gcpsm:projects/p/secrets/prefixed")
		require.NoError(t, err)
		assert.False(t, applied, "a same-ref CAS must not apply once Version has moved past the expected value")

		got, err := ss.GetSecret(ctx, "CAS_ABA", store.ScopeUser, scopeID)
		require.NoError(t, err)
		assert.Equal(t, "gcpsm:projects/p/secrets/legacy", got.SecretRef)
		assert.Equal(t, 2, got.Version)

		applied, err = ss.UpdateSecretRefIfMatches(ctx, "CAS_ABA", store.ScopeUser, scopeID, "gcpsm:projects/p/secrets/legacy", 2, "gcpsm:projects/p/secrets/prefixed")
		require.NoError(t, err)
		assert.True(t, applied)
	})

	t.Run("MatchApplies", func(t *testing.T) {
		scopeID := uuid.New().String()
		sec := &store.Secret{
			ID: uuid.New().String(), Key: "CAS_MATCH", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: scopeID,
			SecretRef: "gcpsm:projects/p/secrets/legacy", Description: "keep me",
		}
		require.NoError(t, ss.CreateSecret(ctx, sec))

		applied, err := ss.UpdateSecretRefIfMatches(ctx, "CAS_MATCH", store.ScopeUser, scopeID, "gcpsm:projects/p/secrets/legacy", 1, "gcpsm:projects/p/secrets/prefixed")
		require.NoError(t, err)
		assert.True(t, applied)

		got, err := ss.GetSecret(ctx, "CAS_MATCH", store.ScopeUser, scopeID)
		require.NoError(t, err)
		assert.Equal(t, "gcpsm:projects/p/secrets/prefixed", got.SecretRef)
		assert.Equal(t, 2, got.Version)
		assert.Equal(t, "v", got.EncryptedValue)
		assert.Equal(t, "keep me", got.Description)
	})

	t.Run("MissingRowReturnsNoError", func(t *testing.T) {
		applied, err := ss.UpdateSecretRefIfMatches(ctx, "ghost", store.ScopeUser, uuid.New().String(), "", 1, "gcpsm:projects/p/secrets/new")
		require.NoError(t, err)
		assert.False(t, applied)
	})

	t.Run("ScopeIsolation", func(t *testing.T) {
		userScope := uuid.New().String()
		projectScope := uuid.New().String()
		require.NoError(t, ss.CreateSecret(ctx, &store.Secret{ID: uuid.New().String(), Key: "SAME_KEY", EncryptedValue: "u", Scope: store.ScopeUser, ScopeID: userScope}))
		require.NoError(t, ss.CreateSecret(ctx, &store.Secret{ID: uuid.New().String(), Key: "SAME_KEY", EncryptedValue: "p", Scope: store.ScopeProject, ScopeID: projectScope}))

		applied, err := ss.UpdateSecretRefIfMatches(ctx, "SAME_KEY", store.ScopeUser, userScope, "", 1, "gcpsm:projects/p/secrets/user")
		require.NoError(t, err)
		assert.True(t, applied)

		proj, err := ss.GetSecret(ctx, "SAME_KEY", store.ScopeProject, projectScope)
		require.NoError(t, err)
		assert.Empty(t, proj.SecretRef)
		assert.Equal(t, 1, proj.Version)
	})
}
