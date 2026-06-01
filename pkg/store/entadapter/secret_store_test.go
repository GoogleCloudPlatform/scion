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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSecretStore(t *testing.T) *SecretStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewSecretStore(client)
}

// =============================================================================
// Secret tests
// =============================================================================

func TestCreateAndGetSecret(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	sec := &store.Secret{
		ID:             uuid.New().String(),
		Key:            "API_KEY",
		EncryptedValue: "enc-value",
		Scope:          store.ScopeUser,
		ScopeID:        uuid.New().String(),
		Description:    "an api key",
		AllowProgeny:   true,
		CreatedBy:      uuid.New().String(),
	}
	require.NoError(t, ss.CreateSecret(ctx, sec))

	// Defaults are applied on create.
	assert.Equal(t, store.SecretTypeEnvironment, sec.SecretType)
	assert.Equal(t, store.InjectionModeAsNeeded, sec.InjectionMode)
	assert.Equal(t, "API_KEY", sec.Target, "empty target should default to key")
	assert.Equal(t, 1, sec.Version)
	assert.False(t, sec.Created.IsZero())

	got, err := ss.GetSecret(ctx, "API_KEY", store.ScopeUser, sec.ScopeID)
	require.NoError(t, err)
	assert.Equal(t, sec.ID, got.ID)
	assert.Equal(t, "enc-value", got.EncryptedValue)
	assert.Equal(t, "API_KEY", got.Target)
	assert.True(t, got.AllowProgeny)
	assert.Equal(t, 1, got.Version)
}

func TestGetSecretNotFound(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	_, err := ss.GetSecret(ctx, "missing", store.ScopeUser, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateSecretDuplicate(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	sec := &store.Secret{ID: uuid.New().String(), Key: "DUP", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: scopeID}
	require.NoError(t, ss.CreateSecret(ctx, sec))

	dup := &store.Secret{ID: uuid.New().String(), Key: "DUP", EncryptedValue: "v2", Scope: store.ScopeUser, ScopeID: scopeID}
	err := ss.CreateSecret(ctx, dup)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestUpdateSecretIncrementsVersion(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	sec := &store.Secret{ID: uuid.New().String(), Key: "K", EncryptedValue: "v1", Scope: store.ScopeProject, ScopeID: scopeID}
	require.NoError(t, ss.CreateSecret(ctx, sec))
	require.Equal(t, 1, sec.Version)

	sec.EncryptedValue = "v2"
	require.NoError(t, ss.UpdateSecret(ctx, sec))
	assert.Equal(t, 2, sec.Version)

	got, err := ss.GetSecret(ctx, "K", store.ScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, "v2", got.EncryptedValue)
	assert.Equal(t, 2, got.Version)
}

func TestUpdateSecretNotFound(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	sec := &store.Secret{Key: "ghost", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: uuid.New().String()}
	err := ss.UpdateSecret(ctx, sec)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpsertSecret(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	sec := &store.Secret{ID: uuid.New().String(), Key: "UP", EncryptedValue: "v1", Scope: store.ScopeUser, ScopeID: scopeID, CreatedBy: "creator"}

	created, err := ss.UpsertSecret(ctx, sec)
	require.NoError(t, err)
	assert.True(t, created, "first upsert should create")

	// Second upsert updates, preserving identity and creation metadata.
	upd := &store.Secret{Key: "UP", EncryptedValue: "v2", Scope: store.ScopeUser, ScopeID: scopeID}
	created, err = ss.UpsertSecret(ctx, upd)
	require.NoError(t, err)
	assert.False(t, created, "second upsert should update")
	assert.Equal(t, sec.ID, upd.ID, "ID preserved across upsert")
	assert.Equal(t, "creator", upd.CreatedBy, "createdBy preserved across upsert")

	got, err := ss.GetSecret(ctx, "UP", store.ScopeUser, scopeID)
	require.NoError(t, err)
	assert.Equal(t, "v2", got.EncryptedValue)
	assert.Equal(t, 2, got.Version)
}

func TestDeleteSecret(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	sec := &store.Secret{ID: uuid.New().String(), Key: "DEL", EncryptedValue: "v", Scope: store.ScopeUser, ScopeID: scopeID}
	require.NoError(t, ss.CreateSecret(ctx, sec))

	require.NoError(t, ss.DeleteSecret(ctx, "DEL", store.ScopeUser, scopeID))

	_, err := ss.GetSecret(ctx, "DEL", store.ScopeUser, scopeID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Deleting again returns ErrNotFound.
	assert.ErrorIs(t, ss.DeleteSecret(ctx, "DEL", store.ScopeUser, scopeID), store.ErrNotFound)
}

func TestDeleteSecretsByScope(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	for _, k := range []string{"A", "B", "C"} {
		require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
			ID: uuid.New().String(), Key: k, EncryptedValue: "v", Scope: store.ScopeProject, ScopeID: scopeID,
		}))
	}
	// A secret in a different scope must survive.
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "OTHER", EncryptedValue: "v", Scope: store.ScopeProject, ScopeID: uuid.New().String(),
	}))

	n, err := ss.DeleteSecretsByScope(ctx, store.ScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	remaining, err := ss.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeProject, ScopeID: scopeID})
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestListSecretsExcludesEncryptedValue(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "LIST", EncryptedValue: "super-secret", Scope: store.ScopeUser, ScopeID: scopeID,
	}))

	list, err := ss.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser, ScopeID: scopeID})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Empty(t, list[0].EncryptedValue, "ListSecrets must not expose encrypted value")
	assert.Equal(t, "LIST", list[0].Key)
}

func TestListSecretsFilterByType(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "ENV", EncryptedValue: "v", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: scopeID,
	}))
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "FILE", EncryptedValue: "v", SecretType: store.SecretTypeFile, Target: "/etc/x", Scope: store.ScopeUser, ScopeID: scopeID,
	}))

	files, err := ss.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeUser, ScopeID: scopeID, Type: store.SecretTypeFile})
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "FILE", files[0].Key)
	assert.Equal(t, "/etc/x", files[0].Target)
}

func TestGetSecretValue(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "VAL", EncryptedValue: "the-value", Scope: store.ScopeUser, ScopeID: scopeID,
	}))

	v, err := ss.GetSecretValue(ctx, "VAL", store.ScopeUser, scopeID)
	require.NoError(t, err)
	assert.Equal(t, "the-value", v)

	_, err = ss.GetSecretValue(ctx, "nope", store.ScopeUser, scopeID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestScopePolymorphism verifies that identical keys in different scopes are
// independent records (no cross-scope collisions).
func TestSecretScopePolymorphism(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	userID := uuid.New().String()
	projectID := uuid.New().String()

	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{ID: uuid.New().String(), Key: "TOKEN", EncryptedValue: "user-val", Scope: store.ScopeUser, ScopeID: userID}))
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{ID: uuid.New().String(), Key: "TOKEN", EncryptedValue: "proj-val", Scope: store.ScopeProject, ScopeID: projectID}))

	u, err := ss.GetSecret(ctx, "TOKEN", store.ScopeUser, userID)
	require.NoError(t, err)
	assert.Equal(t, "user-val", u.EncryptedValue)

	p, err := ss.GetSecret(ctx, "TOKEN", store.ScopeProject, projectID)
	require.NoError(t, err)
	assert.Equal(t, "proj-val", p.EncryptedValue)
}

// TestListProgenySecretsInheritance verifies the transitive progeny-inheritance
// query: only user-scoped, allow_progeny=true secrets whose created_by is within
// the ancestor set are returned, and the encrypted value is never exposed.
func TestListProgenySecretsInheritance(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	ancestor1 := uuid.New().String()
	ancestor2 := uuid.New().String()
	stranger := uuid.New().String()

	// Eligible: user-scoped, allow_progeny, created by an ancestor.
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "INHERIT_1", EncryptedValue: "secret-1",
		Scope: store.ScopeUser, ScopeID: ancestor1, AllowProgeny: true, CreatedBy: ancestor1,
	}))
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "INHERIT_2", EncryptedValue: "secret-2",
		Scope: store.ScopeUser, ScopeID: ancestor2, AllowProgeny: true, CreatedBy: ancestor2,
	}))

	// Ineligible: allow_progeny=false.
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "NO_PROGENY", EncryptedValue: "x",
		Scope: store.ScopeUser, ScopeID: ancestor1, AllowProgeny: false, CreatedBy: ancestor1,
	}))
	// Ineligible: created by a non-ancestor.
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "STRANGER", EncryptedValue: "x",
		Scope: store.ScopeUser, ScopeID: stranger, AllowProgeny: true, CreatedBy: stranger,
	}))
	// Ineligible: wrong scope, even though allow_progeny + ancestor creator.
	require.NoError(t, ss.CreateSecret(ctx, &store.Secret{
		ID: uuid.New().String(), Key: "PROJ", EncryptedValue: "x",
		Scope: store.ScopeProject, ScopeID: uuid.New().String(), AllowProgeny: true, CreatedBy: ancestor1,
	}))

	got, err := ss.ListProgenySecrets(ctx, []string{ancestor1, ancestor2})
	require.NoError(t, err)
	require.Len(t, got, 2)

	keys := map[string]bool{}
	for _, s := range got {
		keys[s.Key] = true
		assert.Empty(t, s.EncryptedValue, "progeny secrets must not expose encrypted value")
	}
	assert.True(t, keys["INHERIT_1"])
	assert.True(t, keys["INHERIT_2"])
}

func TestListProgenySecretsEmptyAncestors(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	got, err := ss.ListProgenySecrets(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// =============================================================================
// EnvVar tests
// =============================================================================

func TestCreateAndGetEnvVar(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	ev := &store.EnvVar{
		ID:          uuid.New().String(),
		Key:         "LOG_LEVEL",
		Value:       "debug",
		Scope:       store.ScopeProject,
		ScopeID:     scopeID,
		Description: "logging",
		Sensitive:   true,
	}
	require.NoError(t, ss.CreateEnvVar(ctx, ev))
	assert.Equal(t, store.InjectionModeAsNeeded, ev.InjectionMode, "empty injection mode normalized")
	assert.False(t, ev.Created.IsZero())

	got, err := ss.GetEnvVar(ctx, "LOG_LEVEL", store.ScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, "debug", got.Value)
	assert.True(t, got.Sensitive)
	assert.Equal(t, "logging", got.Description)
}

func TestCreateEnvVarDuplicate(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	require.NoError(t, ss.CreateEnvVar(ctx, &store.EnvVar{ID: uuid.New().String(), Key: "X", Value: "1", Scope: store.ScopeUser, ScopeID: scopeID}))
	err := ss.CreateEnvVar(ctx, &store.EnvVar{ID: uuid.New().String(), Key: "X", Value: "2", Scope: store.ScopeUser, ScopeID: scopeID})
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestUpdateEnvVar(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	ev := &store.EnvVar{ID: uuid.New().String(), Key: "U", Value: "1", Scope: store.ScopeUser, ScopeID: scopeID}
	require.NoError(t, ss.CreateEnvVar(ctx, ev))

	ev.Value = "2"
	require.NoError(t, ss.UpdateEnvVar(ctx, ev))

	got, err := ss.GetEnvVar(ctx, "U", store.ScopeUser, scopeID)
	require.NoError(t, err)
	assert.Equal(t, "2", got.Value)
}

func TestUpdateEnvVarNotFound(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	err := ss.UpdateEnvVar(ctx, &store.EnvVar{Key: "ghost", Value: "v", Scope: store.ScopeUser, ScopeID: uuid.New().String()})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpsertEnvVar(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	ev := &store.EnvVar{ID: uuid.New().String(), Key: "UP", Value: "1", Scope: store.ScopeUser, ScopeID: scopeID, CreatedBy: "creator"}

	created, err := ss.UpsertEnvVar(ctx, ev)
	require.NoError(t, err)
	assert.True(t, created)

	upd := &store.EnvVar{Key: "UP", Value: "2", Scope: store.ScopeUser, ScopeID: scopeID}
	created, err = ss.UpsertEnvVar(ctx, upd)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, ev.ID, upd.ID)
	assert.Equal(t, "creator", upd.CreatedBy)

	got, err := ss.GetEnvVar(ctx, "UP", store.ScopeUser, scopeID)
	require.NoError(t, err)
	assert.Equal(t, "2", got.Value)
}

func TestDeleteEnvVar(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	require.NoError(t, ss.CreateEnvVar(ctx, &store.EnvVar{ID: uuid.New().String(), Key: "D", Value: "1", Scope: store.ScopeUser, ScopeID: scopeID}))
	require.NoError(t, ss.DeleteEnvVar(ctx, "D", store.ScopeUser, scopeID))
	_, err := ss.GetEnvVar(ctx, "D", store.ScopeUser, scopeID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, ss.DeleteEnvVar(ctx, "D", store.ScopeUser, scopeID), store.ErrNotFound)
}

func TestDeleteEnvVarsByScope(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	for _, k := range []string{"A", "B"} {
		require.NoError(t, ss.CreateEnvVar(ctx, &store.EnvVar{ID: uuid.New().String(), Key: k, Value: "v", Scope: store.ScopeProject, ScopeID: scopeID}))
	}
	n, err := ss.DeleteEnvVarsByScope(ctx, store.ScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestListEnvVarsOrderedByKey(t *testing.T) {
	ss := newTestSecretStore(t)
	ctx := context.Background()

	scopeID := uuid.New().String()
	for _, k := range []string{"ZEBRA", "ALPHA", "MIKE"} {
		require.NoError(t, ss.CreateEnvVar(ctx, &store.EnvVar{ID: uuid.New().String(), Key: k, Value: "v", Scope: store.ScopeUser, ScopeID: scopeID}))
	}

	list, err := ss.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeUser, ScopeID: scopeID})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"ALPHA", "MIKE", "ZEBRA"}, []string{list[0].Key, list[1].Key, list[2].Key})
}
