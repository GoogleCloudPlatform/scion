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

package secret

import (
	"context"
	"strings"
	"testing"
	"time"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestLocalBackend_HoldReadDropPending(t *testing.T) {
	backend, _ := createTestBackend(t)
	ctx := context.Background()
	value := []byte("test-value-1")

	ref, err := backend.HoldPending(ctx, "pending-1", value)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ref, "enc:v1:"), "the local reference is the sealed value")
	assert.NotContains(t, ref, string(value))

	got, err := backend.ReadPending(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, value, got)

	require.NoError(t, backend.DropPending(ctx, ref))
	require.NoError(t, backend.DropPending(ctx, ref), "dropping twice is not an error")

	_, err = backend.HoldPending(ctx, "pending-2", nil)
	assert.Error(t, err, "an empty value is refused")
	_, err = backend.ReadPending(ctx, "")
	assert.Error(t, err)
	_, err = backend.ReadPending(ctx, "gcpsm:projects/p/secrets/s")
	assert.Error(t, err, "a Secret Manager reference is never read as a plaintext value")

	other := NewLocalBackend(createTestStore(t), "test-hub-id", "another-shared-secret")
	_, err = other.ReadPending(ctx, ref)
	assert.Error(t, err, "a reference sealed under another key does not open")
}

func TestGCPBackend_HoldReadDropPending(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()
	value := []byte("test-value-1")
	id := api.NewUUID()

	ref, err := backend.HoldPending(ctx, id, value)
	require.NoError(t, err)
	wantPath := "projects/test-project/secrets/" + backend.secretNamePrefix() + "pending-" + id
	assert.Equal(t, "gcpsm:"+wantPath, ref)
	assert.NotContains(t, ref, string(value))
	require.Contains(t, mock.secrets, wantPath)
	assert.Equal(t, "true", mock.secrets[wantPath].Labels["scion-pending"])
	assert.Equal(t, value, mock.versions[wantPath])

	got, err := backend.ReadPending(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, value, got)

	// The held value is not a secret of the hub: nothing is listed or
	// resolved, and no metadata row is written.
	metas, err := backend.List(ctx, Filter{})
	require.NoError(t, err)
	assert.Empty(t, metas)

	require.NoError(t, backend.DropPending(ctx, ref))
	assert.NotContains(t, mock.secrets, wantPath, "dropping deletes the Secret Manager secret")
	require.NoError(t, backend.DropPending(ctx, ref), "dropping twice is not an error")
	_, err = backend.ReadPending(ctx, ref)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGCPBackend_HoldPendingRefusals(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	_, err := backend.HoldPending(ctx, api.NewUUID(), nil)
	assert.Error(t, err, "an empty value is refused")
	for _, id := range []string{"", "a/b", "../x", strings.Repeat("a", maxPendingIDLen+1)} {
		_, err := backend.HoldPending(ctx, id, []byte("test-value-1"))
		assert.Error(t, err, "id %q", id)
	}
	assert.Empty(t, mock.secrets)

	id := api.NewUUID()
	_, err = backend.HoldPending(ctx, id, []byte("test-value-1"))
	require.NoError(t, err)
	_, err = backend.HoldPending(ctx, id, []byte("test-value-2"))
	assert.Error(t, err, "an existing pending secret is never given a second version")
	path := "projects/test-project/secrets/" + backend.secretNamePrefix() + "pending-" + id
	assert.Equal(t, []byte("test-value-1"), mock.versions[path])
}

// TestGCPBackend_PendingRefOnlyNamesPendingValues: ReadPending and
// DropPending act only on references naming a pending value of this hub,
// never on an approved secret or another hub's pending value.
func TestGCPBackend_PendingRefOnlyNamesPendingValues(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	_, _, err := backend.Set(ctx, &SetSecretInput{
		Name:       "API_KEY",
		Value:      "test-value-approved",
		SecretType: store.SecretTypeEnvironment,
		Scope:      store.ScopeUser,
		ScopeID:    "user-1",
	})
	require.NoError(t, err)
	approvedPath := "projects/test-project/secrets/" + backend.gcpSecretName("API_KEY", store.ScopeUser, "user-1")
	require.Contains(t, mock.secrets, approvedPath)

	other := NewGCPBackendWithClient(backend.store, mock, "test-project", "other-hub-id")
	otherRef, err := other.HoldPending(ctx, api.NewUUID(), []byte("test-value-other"))
	require.NoError(t, err)

	for _, ref := range []string{
		"",
		"enc:v1:abc",
		"gcpsm:" + approvedPath,
		otherRef,
		"gcpsm:projects/another-project/secrets/" + backend.secretNamePrefix() + "pending-" + api.NewUUID(),
		"gcpsm:projects/test-project/secrets/" + backend.secretNamePrefix() + "pending-",
		"gcpsm:projects/test-project/secrets/" + backend.secretNamePrefix() + "pending-a/versions/1",
	} {
		_, err := backend.ReadPending(ctx, ref)
		assert.Error(t, err, "read %q", ref)
		assert.Error(t, backend.DropPending(ctx, ref), "drop %q", ref)
	}
	assert.Contains(t, mock.secrets, approvedPath, "the approved secret is untouched")
	got, err := other.ReadPending(ctx, otherRef)
	require.NoError(t, err)
	assert.Equal(t, []byte("test-value-other"), got)
}

// TestGCPBackend_PendingNameNeverMatchesApprovedNames: an approved
// secret's Secret Manager name, current or legacy, never falls under the
// pending name prefix, whatever its scope, scope id or key.
func TestGCPBackend_PendingNameNeverMatchesApprovedNames(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	pendingPrefix := backend.secretNamePrefix() + "pending-"
	for _, scope := range []string{store.ScopeHub, store.ScopeUser, store.ScopeProject, store.ScopeRuntimeBroker} {
		for _, key := range []string{"API_KEY", "pending", "pending-" + api.NewUUID()} {
			name := backend.gcpSecretName(key, scope, "scope-1")
			assert.False(t, strings.HasPrefix(name, pendingPrefix), "%s/%s -> %s", scope, key, name)
			legacy := backend.legacyGCPSecretName(key, scope, "scope-1")
			assert.False(t, strings.HasPrefix(legacy, pendingPrefix), "legacy %s/%s -> %s", scope, key, legacy)
		}
	}
}

// TestLocalBackend_PendingRowsNeverDelivered: delivery reads the secrets
// table only. Pending rows in the database (as an older binary, which does
// not know the pending table, would also see them) are not resolved,
// listed or read by name, while an approved row of the same name stays in
// effect with its own value.
func TestLocalBackend_PendingRowsNeverDelivered(t *testing.T) {
	ctx := context.Background()
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(ctx))
	backend := NewLocalBackend(s, "test-hub-id", "test-shared-secret")

	const owner = "owner-1"
	_, _, err = backend.Set(ctx, &SetSecretInput{
		Name: "APPROVED_KEY", Value: "test-value-approved", SecretType: TypeEnvironment, Target: "APPROVED_KEY",
		Scope: ScopeUser, ScopeID: owner, CreatedBy: owner, UpdatedBy: owner,
	})
	require.NoError(t, err)

	for _, key := range []string{"PENDING_ONLY_KEY", "APPROVED_KEY"} {
		ref, err := backend.HoldPending(ctx, key, []byte("test-value-pending"))
		require.NoError(t, err)
		_, err = s.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
			ID: api.NewUUID(), Key: key, SecretType: TypeEnvironment, Target: key,
			Scope: ScopeUser, ScopeID: owner, AllowProgeny: true,
			ValueRef: ref, ValueLen: len("test-value-pending"),
			AgentID: "agent-1", AgentSlug: "agent-one", ProjectID: "project-1", ProjectName: "Project One",
		}, time.Now())
		require.NoError(t, err)
	}

	resolved, err := backend.Resolve(ctx, owner, "project-1", "", &ResolveOpts{
		AgentAncestry: []string{owner},
		AuthzCheck:    func(SecretMeta) bool { return true },
	})
	require.NoError(t, err)
	byName := map[string]string{}
	for _, r := range resolved {
		byName[r.Name] = r.Value
	}
	assert.NotContains(t, byName, "PENDING_ONLY_KEY")
	assert.Equal(t, "test-value-approved", byName["APPROVED_KEY"], "the approved value stays in effect")

	list, err := backend.List(ctx, Filter{Scope: ScopeUser, ScopeID: owner})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "APPROVED_KEY", list[0].Name)

	_, err = backend.Get(ctx, "PENDING_ONLY_KEY", ScopeUser, owner)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = backend.GetMeta(ctx, "PENDING_ONLY_KEY", ScopeUser, owner)
	assert.ErrorIs(t, err, store.ErrNotFound)
	got, err := backend.Get(ctx, "APPROVED_KEY", ScopeUser, owner)
	require.NoError(t, err)
	assert.Equal(t, "test-value-approved", got.Value)

	progeny, err := s.ListProgenySecrets(ctx, []string{owner})
	require.NoError(t, err)
	for _, p := range progeny {
		assert.NotEqual(t, "PENDING_ONLY_KEY", p.Key)
	}
}

// TestGCPBackend_ListHeldPending: only this hub's pending values are
// listed, with their pending id and creation time; approved secrets,
// another hub's pending values and secrets that only look like pending
// values are left out.
func TestGCPBackend_ListHeldPending(t *testing.T) {
	backend, mock := createTestGCPBackend(t)
	ctx := context.Background()

	id := api.NewUUID()
	ref, err := backend.HoldPending(ctx, id, []byte("test-value-1"))
	require.NoError(t, err)

	_, _, err = backend.Set(ctx, &SetSecretInput{
		Name: "API_KEY", Value: "test-value-approved", SecretType: store.SecretTypeEnvironment,
		Scope: store.ScopeUser, ScopeID: "user-1",
	})
	require.NoError(t, err)
	other := NewGCPBackendWithClient(backend.store, mock, "test-project", "other-hub-id")
	_, err = other.HoldPending(ctx, api.NewUUID(), []byte("test-value-other"))
	require.NoError(t, err)
	// A pending-named secret without the label, and a labelled secret
	// with an invalid id, are not this hub's held values.
	unlabelled := "projects/test-project/secrets/" + backend.secretNamePrefix() + "pending-" + api.NewUUID()
	mock.secrets[unlabelled] = &smpb.Secret{Name: unlabelled}
	badID := "projects/test-project/secrets/" + backend.secretNamePrefix() + "pending-"
	mock.secrets[badID] = &smpb.Secret{Name: badID, Labels: map[string]string{"scion-pending": "true"}}

	held, err := backend.ListHeldPending(ctx)
	require.NoError(t, err)
	require.Len(t, held, 1)
	assert.Equal(t, ref, held[0].Ref)
	assert.Equal(t, id, held[0].PendingID)
	assert.WithinDuration(t, time.Now(), held[0].Created, time.Minute)

	local, err := (&LocalBackend{}).ListHeldPending(ctx)
	require.NoError(t, err)
	assert.Empty(t, local, "the local backend holds values on the rows")
}
