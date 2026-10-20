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

const (
	gcpHostEmailA   = "host-a@proj-a.iam.gserviceaccount.com"
	gcpHostEmailB   = "host-b@proj-b.iam.gserviceaccount.com"
	gcpHostProjectA = "proj-a"
	gcpHostProjectB = "proj-b"
)

func newGCPHostBroker(t *testing.T, ctx context.Context, s store.Store, email, project, source string) *store.RuntimeBroker {
	t.Helper()
	id := uuid.NewString()
	b := &store.RuntimeBroker{
		ID:                         id,
		Name:                       "gcp-host-" + id[:8],
		Slug:                       "gcp-host-" + id[:8],
		GCPHostServiceAccountEmail: email,
		GCPHostProjectID:           project,
		GCPHostSource:              source,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	return b
}

func requireGCPHost(t *testing.T, ctx context.Context, s store.Store, id, email, project, source string) {
	t.Helper()
	got, err := s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, email, got.GCPHostServiceAccountEmail, "email")
	assert.Equal(t, project, got.GCPHostProjectID, "project")
	assert.Equal(t, source, got.GCPHostSource, "source")
}

// RuntimeBrokerGCPHostWritersConformance checks the dedicated GCP host
// writers and that the generic UpdateRuntimeBroker leaves the pair and its
// source unchanged.
func RuntimeBrokerGCPHostWritersConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("RuntimeBrokerGCPHostWriters", func(t *testing.T) {
		t.Run("UpdateRuntimeBroker leaves the pair and source", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceAdmin)

			cp, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			cp.GCPHostServiceAccountEmail = ""
			cp.GCPHostProjectID = ""
			cp.GCPHostSource = ""
			cp.Version = "2.0.0"
			require.NoError(t, s.UpdateRuntimeBroker(ctx, cp))
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceAdmin)

			cp.GCPHostServiceAccountEmail = gcpHostEmailB
			cp.GCPHostProjectID = gcpHostProjectB
			require.NoError(t, s.UpdateRuntimeBroker(ctx, cp))
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceAdmin)

			got, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			assert.Equal(t, "2.0.0", got.Version, "the rest of the update applies")
		})

		t.Run("create records the source", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
		})

		t.Run("SetIfUnset fills an unset pair", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, "", "", "")
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.True(t, applied)
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
		})

		t.Run("SetIfUnset leaves a set email", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailB, gcpHostProjectB, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.False(t, applied)
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
		})

		t.Run("SetIfUnset leaves a set email with an unset project", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, "", store.GCPHostSourceRegistration)
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailB, gcpHostProjectB, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.False(t, applied)
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, "", store.GCPHostSourceRegistration)
		})

		t.Run("SetIfUnset leaves a set project with an unset email", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, "", gcpHostProjectA, store.GCPHostSourceRegistration)
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailB, gcpHostProjectB, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.False(t, applied)
			requireGCPHost(t, ctx, s, b.ID, "", gcpHostProjectA, store.GCPHostSourceRegistration)
		})

		t.Run("SetIfUnset leaves a pair cleared by the update API", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			_, err := s.SetRuntimeBrokerGCPHost(ctx, b.ID, "", "")
			require.NoError(t, err)
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailB, gcpHostProjectB, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.False(t, applied)
			requireGCPHost(t, ctx, s, b.ID, "", "", store.GCPHostSourceAdmin)
		})

		t.Run("SetIfUnset on a missing row is a no-op", func(t *testing.T) {
			s := factory(t)
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, uuid.NewString(), gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.False(t, applied)
		})

		t.Run("SetDetected replaces a value that is not from the update API", func(t *testing.T) {
			s := factory(t)
			for _, source := range []string{"", store.GCPHostSourceRegistration, store.GCPHostSourceDetected} {
				b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, source)
				applied, err := s.SetRuntimeBrokerGCPHostDetected(ctx, b.ID, gcpHostEmailB, gcpHostProjectB)
				require.NoError(t, err)
				assert.True(t, applied, "source %q", source)
				requireGCPHost(t, ctx, s, b.ID, gcpHostEmailB, gcpHostProjectB, store.GCPHostSourceDetected)
			}
		})

		t.Run("SetDetected leaves a value from the update API", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceAdmin)
			applied, err := s.SetRuntimeBrokerGCPHostDetected(ctx, b.ID, gcpHostEmailB, gcpHostProjectB)
			require.NoError(t, err)
			assert.False(t, applied)
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceAdmin)
		})

		t.Run("SetGCPHost sets, clears and returns the previous values", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)

			prev, err := s.SetRuntimeBrokerGCPHost(ctx, b.ID, gcpHostEmailB, gcpHostProjectB)
			require.NoError(t, err)
			assert.Equal(t, store.GCPHostSetting{Email: gcpHostEmailA, Project: gcpHostProjectA, Source: store.GCPHostSourceRegistration}, prev)
			requireGCPHost(t, ctx, s, b.ID, gcpHostEmailB, gcpHostProjectB, store.GCPHostSourceAdmin)

			prev, err = s.SetRuntimeBrokerGCPHost(ctx, b.ID, "", "")
			require.NoError(t, err)
			assert.Equal(t, store.GCPHostSetting{Email: gcpHostEmailB, Project: gcpHostProjectB, Source: store.GCPHostSourceAdmin}, prev)
			requireGCPHost(t, ctx, s, b.ID, "", "", store.GCPHostSourceAdmin)

			_, err = s.SetRuntimeBrokerGCPHost(ctx, uuid.NewString(), gcpHostEmailA, gcpHostProjectA)
			assert.ErrorIs(t, err, store.ErrNotFound)
		})

		t.Run("SetGCPHost returns the values a registration filled", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, "", "", "")
			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			require.True(t, applied)

			prev, err := s.SetRuntimeBrokerGCPHost(ctx, b.ID, gcpHostEmailB, gcpHostProjectB)
			require.NoError(t, err)
			assert.Equal(t, store.GCPHostSetting{Email: gcpHostEmailA, Project: gcpHostProjectA, Source: store.GCPHostSourceRegistration}, prev)
		})

		t.Run("SetSource releases the pair to registration", func(t *testing.T) {
			s := factory(t)
			b := newGCPHostBroker(t, ctx, s, "", "", "")
			_, err := s.SetRuntimeBrokerGCPHost(ctx, b.ID, "", "")
			require.NoError(t, err)
			prev, err := s.SetRuntimeBrokerGCPHostSource(ctx, b.ID, "")
			require.NoError(t, err)
			assert.Equal(t, store.GCPHostSetting{Source: store.GCPHostSourceAdmin}, prev)
			requireGCPHost(t, ctx, s, b.ID, "", "", "")

			applied, err := s.SetRuntimeBrokerGCPHostIfUnset(ctx, b.ID, gcpHostEmailA, gcpHostProjectA, store.GCPHostSourceRegistration)
			require.NoError(t, err)
			assert.True(t, applied)

			_, err = s.SetRuntimeBrokerGCPHostSource(ctx, uuid.NewString(), "")
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	})
}
