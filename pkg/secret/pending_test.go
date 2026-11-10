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

func TestGCPBackend_PendingNotImplemented(t *testing.T) {
	backend, _ := createTestGCPBackend(t)
	ctx := context.Background()
	_, err := backend.HoldPending(ctx, "pending-1", []byte("test-value-1"))
	assert.ErrorIs(t, err, ErrPendingNotImplemented)
	_, err = backend.ReadPending(ctx, "gcpsm:x")
	assert.ErrorIs(t, err, ErrPendingNotImplemented)
	assert.ErrorIs(t, backend.DropPending(ctx, "gcpsm:x"), ErrPendingNotImplemented)
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
