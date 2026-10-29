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

package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserSecretAttributionRepair_WritesMarkerAndSkipsOnNextBoot: the first
// pass attributes a legacy agent-captured user secret to its owner, logs the
// skip of a row whose agent belongs to another user at WARN, and writes the
// marker; the second pass sees the marker and leaves a legacy row added in
// between unchanged. No secret value is logged.
func TestUserSecretAttributionRepair_WritesMarkerAndSkipsOnNextBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, proj))
	newUser := func(email string) *store.User {
		u := &store.User{ID: uuid.NewString(), Email: email, DisplayName: email, Role: "member", Status: store.UserStatusActive}
		require.NoError(t, s.CreateUser(ctx, u))
		return u
	}
	owner := newUser("owner@test.com")
	other := newUser("other@test.com")
	newAgent := func(slug string, u *store.User) *store.Agent {
		a := &store.Agent{
			ID: uuid.NewString(), Slug: slug, Name: slug, ProjectID: proj.ID, Phase: "running",
			CreatedBy: u.ID, OwnerID: u.ID, Ancestry: []string{u.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	capturer := newAgent("capturer", owner)
	foreign := newAgent("foreign", other)

	newSecret := func(key, createdBy string) {
		require.NoError(t, s.CreateSecret(ctx, &store.Secret{
			ID: uuid.NewString(), Key: key, EncryptedValue: "enc-value-" + key,
			Scope: store.ScopeUser, ScopeID: owner.ID, AllowProgeny: true, CreatedBy: createdBy,
		}))
	}
	newSecret("LEGACY_KEY", "agent:"+capturer.ID)
	newSecret("FOREIGN_KEY", "agent:"+foreign.ID)

	buf, restore := captureSlog(t)
	defer restore()

	runUserSecretAttributionRepair(ctx, s)

	out := buf.String()
	assert.Contains(t, out, "User secret attribution repair: pass completed")
	assert.Contains(t, out, "attributed secret to its owner")
	assert.Contains(t, out, "LEGACY_KEY")
	assert.Contains(t, out, "secret left attributed to an agent")
	assert.Contains(t, out, "FOREIGN_KEY")
	assert.Contains(t, out, "agent_not_linked_to_scope_user")
	assert.NotContains(t, out, "enc-value-", "no secret value is logged")
	assert.NotContains(t, out, "failed to write completion marker")
	done, err := IsMigrationComplete(ctx, s, MigrationUserSecretAttributionRepair)
	require.NoError(t, err)
	assert.True(t, done)

	row, err := s.GetSecret(ctx, "LEGACY_KEY", store.ScopeUser, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, row.CreatedBy)
	row, err = s.GetSecret(ctx, "FOREIGN_KEY", store.ScopeUser, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent:"+foreign.ID, row.CreatedBy)

	newSecret("LATE_KEY", "agent:"+capturer.ID)
	runUserSecretAttributionRepair(ctx, s)
	row, err = s.GetSecret(ctx, "LATE_KEY", store.ScopeUser, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent:"+capturer.ID, row.CreatedBy, "the marker stops a second pass")
}

// usaFlakyListStore fails ListSecrets while failures > 0.
type usaFlakyListStore struct {
	store.Store
	failures int
}

func (f *usaFlakyListStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("fake list failure")
	}
	return f.Store.ListSecrets(ctx, filter)
}

// usaSwapMissStore makes every attribution swap inside a transaction miss.
type usaSwapMissStore struct{ store.Store }

func (w *usaSwapMissStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error { return fn(usaSwapMissTx{Store: tx}) })
}

type usaSwapMissTx struct{ store.Store }

func (usaSwapMissTx) AttributeUserSecretToOwner(context.Context, string, string) (bool, error) {
	return false, nil
}

// newUSALegacyRow stores an owner, the owner's agent and one user secret
// attributed to that agent, and returns the owner's ID and the agent ref.
func newUSALegacyRow(t *testing.T, s store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, proj))
	owner := &store.User{ID: uuid.NewString(), Email: "owner@test.com", DisplayName: "O", Role: "member", Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, owner))
	a := &store.Agent{
		ID: uuid.NewString(), Slug: "capturer", Name: "capturer", ProjectID: proj.ID, Phase: "running",
		CreatedBy: owner.ID, OwnerID: owner.ID, Ancestry: []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID: uuid.NewString(), Key: "LEGACY_KEY", EncryptedValue: "enc-value",
		Scope: store.ScopeUser, ScopeID: owner.ID, AllowProgeny: true, CreatedBy: "agent:" + a.ID,
	}))
	return owner.ID, "agent:" + a.ID
}

// TestUserSecretAttributionRepair_StoreErrorRetriesNextBoot: a store error
// aborts the pass with an ERROR and leaves the marker unwritten; the next
// boot repairs the row and writes the marker.
func TestUserSecretAttributionRepair_StoreErrorRetriesNextBoot(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	owner, _ := newUSALegacyRow(t, base)
	s := &usaFlakyListStore{Store: base, failures: 1}

	buf, restore := captureSlog(t)
	defer restore()

	runUserSecretAttributionRepair(ctx, s)
	assert.Contains(t, buf.String(), "pass did not complete; will retry next boot")
	done, err := IsMigrationComplete(ctx, s, MigrationUserSecretAttributionRepair)
	require.NoError(t, err)
	assert.False(t, done, "a failed pass must not write the marker")

	runUserSecretAttributionRepair(ctx, s)
	done, err = IsMigrationComplete(ctx, s, MigrationUserSecretAttributionRepair)
	require.NoError(t, err)
	assert.True(t, done)
	row, err := s.GetSecret(ctx, "LEGACY_KEY", store.ScopeUser, owner)
	require.NoError(t, err)
	assert.Equal(t, owner, row.CreatedBy, "the retry repairs the row")
}

// TestUserSecretAttributionRepair_RowChangedKeepsMarkerUnwritten: a row
// still changing after the retries is not a final outcome, so the marker
// stays unwritten and the next boot runs the pass again.
func TestUserSecretAttributionRepair_RowChangedKeepsMarkerUnwritten(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	owner, legacy := newUSALegacyRow(t, base)

	buf, restore := captureSlog(t)
	defer restore()

	runUserSecretAttributionRepair(ctx, &usaSwapMissStore{Store: base})
	out := buf.String()
	assert.Contains(t, out, "row_changed")
	assert.Contains(t, out, "marker not written, will retry next boot")
	done, err := IsMigrationComplete(ctx, base, MigrationUserSecretAttributionRepair)
	require.NoError(t, err)
	assert.False(t, done)
	row, err := base.GetSecret(ctx, "LEGACY_KEY", store.ScopeUser, owner)
	require.NoError(t, err)
	assert.Equal(t, legacy, row.CreatedBy)

	runUserSecretAttributionRepair(ctx, base)
	done, err = IsMigrationComplete(ctx, base, MigrationUserSecretAttributionRepair)
	require.NoError(t, err)
	assert.True(t, done)
	row, err = base.GetSecret(ctx, "LEGACY_KEY", store.ScopeUser, owner)
	require.NoError(t, err)
	assert.Equal(t, owner, row.CreatedBy)
}
