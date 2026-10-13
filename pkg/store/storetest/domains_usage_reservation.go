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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedReservationLimit creates a limit definition for the usage reservation
// conformance tests and returns its ID.
func seedReservationLimit(t *testing.T, ctx context.Context, s store.Store) string {
	t.Helper()
	ld, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         "conformance_limit_" + uuid.NewString()[:8],
		ResourceType: "agent",
		Unit:         "count",
		System:       true,
	})
	require.NoError(t, err)
	return ld.ID
}

// newBrokerReservation builds an active broker-scoped reservation of
// resourceID on brokerID.
func newBrokerReservation(limitDefID, brokerID, resourceID string) *store.UsageReservation {
	return &store.UsageReservation{
		LimitDefinitionID: limitDefID,
		SubjectID:         brokerID,
		ScopeType:         store.QuotaScopeBroker,
		ScopeID:           brokerID,
		ResourceID:        resourceID,
		Reserved:          1,
	}
}

// UsageReservationConformance checks GetActiveReservation and the typed
// unique violation of CreateUsageReservation (store.ErrActiveReservationExists)
// on whichever backend factory selects.
func UsageReservationConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("usage_reservation", func(t *testing.T) {
		t.Run("get active returns not found when none is held", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)

			_, err := s.GetActiveReservation(ctx, def, uuid.NewString())
			assert.ErrorIs(t, err, store.ErrNotFound)
		})

		t.Run("get active returns the held reservation's scope", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			brokerA, resource := uuid.NewString(), uuid.NewString()

			_, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, brokerA, resource))
			require.NoError(t, err)

			got, err := s.GetActiveReservation(ctx, def, resource)
			require.NoError(t, err)
			assert.Equal(t, def, got.LimitDefinitionID)
			assert.Equal(t, store.QuotaScopeBroker, got.ScopeType)
			assert.Equal(t, brokerA, got.ScopeID)
			assert.Equal(t, brokerA, got.SubjectID)
			assert.Equal(t, resource, got.ResourceID)
			assert.Nil(t, got.ReleasedAt)
		})

		t.Run("a second active row for the resource is a typed violation", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			brokerA, brokerB, resource := uuid.NewString(), uuid.NewString(), uuid.NewString()

			_, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, brokerA, resource))
			require.NoError(t, err)

			// Another scope and the same scope are both refused by the
			// unique active (limit_definition_id, resource_id) index.
			for _, scope := range []string{brokerB, brokerA} {
				_, err = s.CreateUsageReservation(ctx, newBrokerReservation(def, scope, resource))
				assert.ErrorIs(t, err, store.ErrActiveReservationExists)
				assert.ErrorIs(t, err, store.ErrAlreadyExists)
			}

			// The first row is unchanged and is still the only active one.
			got, err := s.GetActiveReservation(ctx, def, resource)
			require.NoError(t, err)
			assert.Equal(t, brokerA, got.ScopeID)
			n, err := s.CountActiveReservations(ctx, def, brokerB, store.QuotaScopeBroker, brokerB)
			require.NoError(t, err)
			assert.Zero(t, n)
		})

		t.Run("a released reservation is ignored and does not block a fresh one", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			brokerA, brokerB, resource := uuid.NewString(), uuid.NewString(), uuid.NewString()

			_, err := s.CreateUsageReservation(ctx, newBrokerReservation(def, brokerA, resource))
			require.NoError(t, err)
			require.NoError(t, s.ReleaseReservation(ctx, def, resource))

			_, err = s.GetActiveReservation(ctx, def, resource)
			assert.ErrorIs(t, err, store.ErrNotFound)

			_, err = s.CreateUsageReservation(ctx, newBrokerReservation(def, brokerB, resource))
			require.NoError(t, err)
			got, err := s.GetActiveReservation(ctx, def, resource)
			require.NoError(t, err)
			assert.Equal(t, brokerB, got.ScopeID)
		})

		t.Run("limits are independent", func(t *testing.T) {
			s := factory(t)
			def1 := seedReservationLimit(t, ctx, s)
			def2 := seedReservationLimit(t, ctx, s)
			brokerA, resource := uuid.NewString(), uuid.NewString()

			_, err := s.CreateUsageReservation(ctx, newBrokerReservation(def1, brokerA, resource))
			require.NoError(t, err)
			_, err = s.GetActiveReservation(ctx, def2, resource)
			assert.ErrorIs(t, err, store.ErrNotFound)
			_, err = s.CreateUsageReservation(ctx, newBrokerReservation(def2, brokerA, resource))
			require.NoError(t, err)
		})

		t.Run("a duplicate primary key is not reported as an active reservation", func(t *testing.T) {
			s := factory(t)
			def := seedReservationLimit(t, ctx, s)
			brokerA := uuid.NewString()
			id := uuid.NewString()

			first := newBrokerReservation(def, brokerA, uuid.NewString())
			first.ID = id
			_, err := s.CreateUsageReservation(ctx, first)
			require.NoError(t, err)

			second := newBrokerReservation(def, brokerA, uuid.NewString())
			second.ID = id
			_, err = s.CreateUsageReservation(ctx, second)
			require.Error(t, err)
			assert.ErrorIs(t, err, store.ErrAlreadyExists)
			assert.False(t, errors.Is(err, store.ErrActiveReservationExists),
				"a primary key collision on a different resource must not read as an active reservation: %v", err)
		})
	})
}
