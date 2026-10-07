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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMembershipLossCheck(userID, projectID string) *store.MembershipLossCheck {
	return &store.MembershipLossCheck{
		UserID:        userID,
		ProjectID:     projectID,
		Trigger:       store.MembershipLossTriggerMemberRemove,
		ActorKind:     "user",
		ActorID:       uuid.NewString(),
		CorrelationID: uuid.NewString(),
	}
}

// MembershipLossCheckConformance verifies the MembershipLossCheckStore
// contract against a store.Store backend.
func MembershipLossCheckConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	t.Run("membership_loss_check", func(t *testing.T) {
		t.Run("enqueue validates input", func(t *testing.T) {
			s := factory(t)
			err := s.EnqueueMembershipLossCheck(ctx, newMembershipLossCheck("", uuid.NewString()))
			assert.ErrorIs(t, err, store.ErrInvalidInput)
			c := newMembershipLossCheck(uuid.NewString(), "")
			c.Trigger = "other"
			assert.ErrorIs(t, s.EnqueueMembershipLossCheck(ctx, c), store.ErrInvalidInput)
			assert.ErrorIs(t, s.EnqueueMembershipLossCheck(ctx, newMembershipLossCheck("not-a-uuid", "")), store.ErrInvalidInput)
			assert.ErrorIs(t, s.EnqueueMembershipLossCheck(ctx, newMembershipLossCheck(uuid.NewString(), "not-a-uuid")), store.ErrInvalidInput)
			none, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			assert.Empty(t, none, "a refused enqueue stores nothing")
			_, err = s.ClaimMembershipLossChecks(ctx, 0, time.Minute)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
			_, err = s.ClaimMembershipLossChecks(ctx, 1, 0)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})

		t.Run("enqueue stores canonical IDs", func(t *testing.T) {
			s := factory(t)
			user, project := uuid.New(), uuid.New()
			c := newMembershipLossCheck("{"+strings.ToUpper(user.String())+"}", strings.ToUpper(project.String()))
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, c))
			assert.Equal(t, user.String(), c.UserID)
			assert.Equal(t, project.String(), c.ProjectID)
			claimed, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			require.Len(t, claimed, 1)
			assert.Equal(t, user.String(), claimed[0].UserID)
			assert.Equal(t, project.String(), claimed[0].ProjectID)
		})

		t.Run("a failed enqueue leaves the check unchanged", func(t *testing.T) {
			s := factory(t)
			id := uuid.New()
			first := newMembershipLossCheck(uuid.NewString(), "")
			first.ID = id.String()
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, first))

			// Same ID in a non-canonical form, so the insert itself fails.
			dup := newMembershipLossCheck(strings.ToUpper(uuid.NewString()), strings.ToUpper(uuid.NewString()))
			dup.ID = strings.ToUpper(id.String())
			before := *dup
			require.Error(t, s.EnqueueMembershipLossCheck(ctx, dup))
			assert.Equal(t, before, *dup, "a call that returns an error leaves the check unchanged")
			assert.True(t, dup.CreatedAt.IsZero(), "CreatedAt stays zero after a failed enqueue")
		})

		t.Run("claim leases oldest rows and skips leased ones", func(t *testing.T) {
			s := factory(t)
			base := time.Now().Add(-time.Hour)
			var enqueued []*store.MembershipLossCheck
			for i := 0; i < 3; i++ {
				c := newMembershipLossCheck(uuid.NewString(), uuid.NewString())
				c.CreatedAt = base.Add(time.Duration(i) * time.Minute)
				require.NoError(t, s.EnqueueMembershipLossCheck(ctx, c))
				require.NotEmpty(t, c.ID)
				enqueued = append(enqueued, c)
			}
			// A check for every project (empty ProjectID) round-trips.
			all := newMembershipLossCheck(uuid.NewString(), "")
			all.Trigger = store.MembershipLossTriggerGroupChange
			all.CreatedAt = base.Add(10 * time.Minute)
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, all))

			first, err := s.ClaimMembershipLossChecks(ctx, 2, time.Hour)
			require.NoError(t, err)
			require.Len(t, first, 2)
			for i, c := range first {
				assert.Equal(t, enqueued[i].ID, c.ID)
				assert.Equal(t, enqueued[i].UserID, c.UserID)
				assert.Equal(t, enqueued[i].ProjectID, c.ProjectID)
				assert.Equal(t, enqueued[i].ActorID, c.ActorID)
				assert.Equal(t, enqueued[i].CorrelationID, c.CorrelationID)
				assert.Equal(t, store.MembershipLossTriggerMemberRemove, c.Trigger)
				assert.Equal(t, 1, c.Attempts)
				require.NotNil(t, c.LeaseUntil)
				assert.True(t, c.LeaseUntil.After(time.Now()))
			}

			second, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			require.Len(t, second, 2)
			assert.Equal(t, enqueued[2].ID, second[0].ID)
			assert.Equal(t, all.ID, second[1].ID)
			assert.Equal(t, "", second[1].ProjectID)
			assert.Equal(t, store.MembershipLossTriggerGroupChange, second[1].Trigger)

			none, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			assert.Empty(t, none)
		})

		t.Run("complete deletes, fail records and keeps the lease", func(t *testing.T) {
			s := factory(t)
			done := newMembershipLossCheck(uuid.NewString(), uuid.NewString())
			failed := newMembershipLossCheck(uuid.NewString(), uuid.NewString())
			failed.CreatedAt = time.Now().Add(time.Second)
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, done))
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, failed))

			const lease = 2 * time.Second
			claimed, err := s.ClaimMembershipLossChecks(ctx, 10, lease)
			require.NoError(t, err)
			require.Len(t, claimed, 2)
			require.Equal(t, done.ID, claimed[0].ID)
			require.Equal(t, failed.ID, claimed[1].ID)
			claim := claimed[0].Attempts
			require.Equal(t, 1, claim)

			require.NoError(t, s.CompleteMembershipLossCheck(ctx, done.ID, claim))
			assert.ErrorIs(t, s.CompleteMembershipLossCheck(ctx, done.ID, claim), store.ErrClaimLost, "a completed check is gone")
			require.NoError(t, s.FailMembershipLossCheck(ctx, failed.ID, claimed[1].Attempts, "store unavailable"))
			assert.ErrorIs(t, s.FailMembershipLossCheck(ctx, done.ID, claim, "x"), store.ErrClaimLost)

			// Still leased: nothing to claim.
			none, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			assert.Empty(t, none)

			// After the lease expires the failed check is claimable again.
			time.Sleep(lease + 500*time.Millisecond)
			again, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			require.Len(t, again, 1)
			assert.Equal(t, failed.ID, again[0].ID)
			assert.Equal(t, 2, again[0].Attempts)
			assert.Equal(t, "store unavailable", again[0].LastError)
		})

		t.Run("a stale claim cannot complete or fail the check", func(t *testing.T) {
			s := factory(t)
			c := newMembershipLossCheck(uuid.NewString(), uuid.NewString())
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, c))
			first, err := s.ClaimMembershipLossChecks(ctx, 1, 50*time.Millisecond)
			require.NoError(t, err)
			require.Len(t, first, 1)
			time.Sleep(100 * time.Millisecond)
			second, err := s.ClaimMembershipLossChecks(ctx, 1, 300*time.Millisecond)
			require.NoError(t, err)
			require.Len(t, second, 1, "the expired claim is claimed again")
			require.Equal(t, c.ID, second[0].ID)
			require.Equal(t, first[0].Attempts+1, second[0].Attempts)

			require.NoError(t, s.FailMembershipLossCheck(ctx, c.ID, second[0].Attempts, "current claim"))
			assert.ErrorIs(t, s.FailMembershipLossCheck(ctx, c.ID, first[0].Attempts, "stale claim"), store.ErrClaimLost)
			assert.ErrorIs(t, s.CompleteMembershipLossCheck(ctx, c.ID, first[0].Attempts), store.ErrClaimLost)

			// The row is still there, with the current claim's error.
			time.Sleep(400 * time.Millisecond)
			third, err := s.ClaimMembershipLossChecks(ctx, 1, time.Hour)
			require.NoError(t, err)
			require.Len(t, third, 1, "a stale complete must leave the check in place")
			assert.Equal(t, c.ID, third[0].ID)
			assert.Equal(t, "current claim", third[0].LastError, "a stale fail must not overwrite the error")
			require.NoError(t, s.CompleteMembershipLossCheck(ctx, c.ID, third[0].Attempts))
		})

		t.Run("long error text is cut to a valid prefix", func(t *testing.T) {
			s := factory(t)
			c := newMembershipLossCheck(uuid.NewString(), uuid.NewString())
			require.NoError(t, s.EnqueueMembershipLossCheck(ctx, c))
			claimed, err := s.ClaimMembershipLossChecks(ctx, 1, time.Millisecond)
			require.NoError(t, err)
			require.Len(t, claimed, 1)
			require.NoError(t, s.FailMembershipLossCheck(ctx, c.ID, claimed[0].Attempts, strings.Repeat("é", 3000)))
			time.Sleep(20 * time.Millisecond)
			again, err := s.ClaimMembershipLossChecks(ctx, 1, time.Hour)
			require.NoError(t, err)
			require.Len(t, again, 1)
			assert.NotEmpty(t, again[0].LastError)
			assert.LessOrEqual(t, len(again[0].LastError), 2000)
			assert.True(t, strings.HasPrefix(strings.Repeat("é", 3000), again[0].LastError))
		})

		t.Run("enqueue and claim participate in WithTx", func(t *testing.T) {
			s := factory(t)
			errRollback := fmt.Errorf("rollback")
			err := s.WithTx(ctx, func(tx store.Store) error {
				require.NoError(t, tx.EnqueueMembershipLossCheck(ctx, newMembershipLossCheck(uuid.NewString(), uuid.NewString())))
				claimed, err := tx.ClaimMembershipLossChecks(ctx, 10, time.Hour)
				require.NoError(t, err)
				require.Len(t, claimed, 1)
				return errRollback
			})
			require.ErrorIs(t, err, errRollback)
			none, err := s.ClaimMembershipLossChecks(ctx, 10, time.Hour)
			require.NoError(t, err)
			assert.Empty(t, none, "a rolled-back enqueue leaves nothing to claim")
		})
	})
}
