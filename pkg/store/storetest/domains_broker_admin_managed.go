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

package storetest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newAdminManagedBroker(t *testing.T, ctx context.Context, s store.Store, adminManaged bool) *store.RuntimeBroker {
	t.Helper()
	id := uuid.NewString()
	b := &store.RuntimeBroker{
		ID:           id,
		Name:         "admin-managed-" + id[:8],
		Slug:         "admin-managed-" + id[:8],
		AdminManaged: adminManaged,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	return b
}

// RuntimeBrokerAdminManagedWriterConformance checks SetRuntimeBrokerAdminManaged
// and that the generic UpdateRuntimeBroker leaves admin_managed unchanged.
func RuntimeBrokerAdminManagedWriterConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("RuntimeBrokerAdminManagedWriter", func(t *testing.T) {
		t.Run("create records initial admin_managed", func(t *testing.T) {
			s := factory(t)
			bTrue := newAdminManagedBroker(t, ctx, s, true)
			got, err := s.GetRuntimeBroker(ctx, bTrue.ID)
			require.NoError(t, err)
			assert.True(t, got.AdminManaged)

			bFalse := newAdminManagedBroker(t, ctx, s, false)
			got, err = s.GetRuntimeBroker(ctx, bFalse.ID)
			require.NoError(t, err)
			assert.False(t, got.AdminManaged)
		})

		t.Run("UpdateRuntimeBroker leaves admin_managed unchanged", func(t *testing.T) {
			s := factory(t)
			b := newAdminManagedBroker(t, ctx, s, true)

			cp, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			cp.AdminManaged = false
			cp.Version = "2.0.0"
			require.NoError(t, s.UpdateRuntimeBroker(ctx, cp))

			got, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			assert.True(t, got.AdminManaged, "UpdateRuntimeBroker must not clear admin_managed")
			assert.Equal(t, "2.0.0", got.Version, "the rest of the update applies")

			bFalse := newAdminManagedBroker(t, ctx, s, false)
			cp2, err := s.GetRuntimeBroker(ctx, bFalse.ID)
			require.NoError(t, err)
			cp2.AdminManaged = true
			cp2.Version = "2.1.0"
			require.NoError(t, s.UpdateRuntimeBroker(ctx, cp2))

			got2, err := s.GetRuntimeBroker(ctx, bFalse.ID)
			require.NoError(t, err)
			assert.False(t, got2.AdminManaged, "UpdateRuntimeBroker must not set admin_managed")
			assert.Equal(t, "2.1.0", got2.Version, "the rest of the update applies")
		})

		t.Run("SetRuntimeBrokerAdminManaged toggles and returns previous value", func(t *testing.T) {
			s := factory(t)
			b := newAdminManagedBroker(t, ctx, s, false)

			prev, err := s.SetRuntimeBrokerAdminManaged(ctx, b.ID, true)
			require.NoError(t, err)
			assert.False(t, prev)
			got, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			assert.True(t, got.AdminManaged)

			prev, err = s.SetRuntimeBrokerAdminManaged(ctx, b.ID, true)
			require.NoError(t, err)
			assert.True(t, prev)

			prev, err = s.SetRuntimeBrokerAdminManaged(ctx, b.ID, false)
			require.NoError(t, err)
			assert.True(t, prev)
			got, err = s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			assert.False(t, got.AdminManaged)

			_, err = s.SetRuntimeBrokerAdminManaged(ctx, uuid.NewString(), true)
			assert.ErrorIs(t, err, store.ErrNotFound)
		})

		t.Run("stale whole-row write-back cannot undo revocation", func(t *testing.T) {
			s := factory(t)
			b := newAdminManagedBroker(t, ctx, s, true)

			// Whole-row writer reads a copy while admin_managed is true.
			stale, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			require.True(t, stale.AdminManaged)

			// Admin revokes admin_managed through the dedicated writer.
			prev, err := s.SetRuntimeBrokerAdminManaged(ctx, b.ID, false)
			require.NoError(t, err)
			assert.True(t, prev)

			// Stale whole-row write-back lands afterward.
			stale.Version = "3.0.0"
			require.NoError(t, s.UpdateRuntimeBroker(ctx, stale))

			got, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			assert.False(t, got.AdminManaged, "stale whole-row write-back must not restore admin_managed=true")
			assert.Equal(t, "3.0.0", got.Version, "non-admin_managed fields still update")
		})
	})
}
