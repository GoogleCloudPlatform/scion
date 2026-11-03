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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UserDeletionConformance verifies the forced user deletion contract
// (ptone/scion#2143) against a backend: the UserDeletionStore tombstone and
// agent records, the deleting status (MarkUserDeleting and the
// UpdateUser/CreateUser refusals), the tombstoned-ID refusal and
// RevokeUserAccessTokensForUser. It is hand-written rather than a Domain[T]
// descriptor because the tombstone is keyed by user, is never deleted and
// changes state only by compare-and-set.
func UserDeletionConformance(t *testing.T, factory Factory) {
	t.Helper()
	ctx := context.Background()

	seedUser := func(t *testing.T, s store.Store) *store.User {
		t.Helper()
		id := uuid.NewString()
		u := &store.User{
			ID:          id,
			Email:       fmt.Sprintf("user-deletion-%s@example.com", id[:8]),
			DisplayName: "User Deletion",
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
		}
		require.NoError(t, s.CreateUser(ctx, u))
		return u
	}
	newDeletion := func(userID string, at time.Time) *store.UserDeletion {
		return &store.UserDeletion{
			UserID:          userID,
			RequestedByKind: "user",
			RequestedByID:   uuid.NewString(),
			CredentialKind:  "session",
			RequestedAt:     at,
			BeforeSummary:   `{"role":"member","status":"active","kind":"human"}`,
		}
	}

	t.Run("CreateAndGet", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		at := time.Now().UTC().Truncate(time.Millisecond)
		d := newDeletion(u.ID, at)
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		require.NotEmpty(t, d.ID, "the ID is generated")
		assert.Equal(t, store.UserDeletionStateStopping, d.State)

		got, err := s.GetUserDeletion(ctx, d.ID)
		require.NoError(t, err)
		assert.Equal(t, d.ID, got.ID)
		assert.Equal(t, u.ID, got.UserID)
		assert.Equal(t, store.UserDeletionStateStopping, got.State)
		assert.Equal(t, d.RequestedByKind, got.RequestedByKind)
		assert.Equal(t, d.RequestedByID, got.RequestedByID)
		assert.Equal(t, d.CredentialKind, got.CredentialKind)
		assert.WithinDuration(t, at, got.RequestedAt, time.Second)
		assert.Nil(t, got.CompletedAt)
		assert.Nil(t, got.LastReconciledAt)
		assert.Equal(t, d.BeforeSummary, got.BeforeSummary)

		byUser, err := s.GetUserDeletionByUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, d.ID, byUser.ID)
	})

	t.Run("NotFound", func(t *testing.T) {
		s := factory(t)
		_, err := s.GetUserDeletion(ctx, uuid.NewString())
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetUserDeletion(ctx, "not-a-uuid")
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetUserDeletionByUser(ctx, uuid.NewString())
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.CompleteUserDeletion(ctx, uuid.NewString(), time.Now())
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("InvalidInput", func(t *testing.T) {
		s := factory(t)
		assert.ErrorIs(t, s.CreateUserDeletion(ctx, newDeletion("not-a-uuid", time.Now())), store.ErrInvalidInput)
		assert.ErrorIs(t, s.CreateUserDeletion(ctx, newDeletion(uuid.NewString(), time.Time{})), store.ErrInvalidInput)
	})

	t.Run("OnePerUser", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		require.NoError(t, s.CreateUserDeletion(ctx, newDeletion(u.ID, time.Now())))
		err := s.CreateUserDeletion(ctx, newDeletion(u.ID, time.Now()))
		assert.ErrorIs(t, err, store.ErrAlreadyExists, "user_id is unique: a user has at most one deletion")
	})

	t.Run("CompleteIsCompareAndSet", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		d := newDeletion(u.ID, time.Now())
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		at := time.Now().UTC()
		changed, err := s.CompleteUserDeletion(ctx, d.ID, at)
		require.NoError(t, err)
		assert.True(t, changed)
		changed, err = s.CompleteUserDeletion(ctx, d.ID, at.Add(time.Minute))
		require.NoError(t, err)
		assert.False(t, changed, "only one completion takes effect")
		got, err := s.GetUserDeletion(ctx, d.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserDeletionStateCompleted, got.State)
		require.NotNil(t, got.CompletedAt)
		assert.WithinDuration(t, at, *got.CompletedAt, time.Second)
	})

	t.Run("ListByStateAndTouch", func(t *testing.T) {
		s := factory(t)
		base := time.Now().UTC().Add(-time.Hour)
		var ids []string
		for i := 0; i < 3; i++ {
			u := seedUser(t, s)
			d := newDeletion(u.ID, base.Add(time.Duration(i)*time.Minute))
			require.NoError(t, s.CreateUserDeletion(ctx, d))
			ids = append(ids, d.ID)
		}
		_, err := s.CompleteUserDeletion(ctx, ids[1], time.Now())
		require.NoError(t, err)

		stopping, err := s.ListUserDeletionsByState(ctx, store.UserDeletionStateStopping, 0)
		require.NoError(t, err)
		require.Len(t, stopping, 2)
		assert.Equal(t, ids[0], stopping[0].ID, "oldest request first")
		assert.Equal(t, ids[2], stopping[1].ID)
		limited, err := s.ListUserDeletionsByState(ctx, store.UserDeletionStateStopping, 1)
		require.NoError(t, err)
		require.Len(t, limited, 1)

		at := time.Now().UTC()
		require.NoError(t, s.TouchUserDeletionReconciled(ctx, ids[0], at, "broker unreachable"))
		got, err := s.GetUserDeletion(ctx, ids[0])
		require.NoError(t, err)
		require.NotNil(t, got.LastReconciledAt)
		assert.WithinDuration(t, at, *got.LastReconciledAt, time.Second)
		assert.Equal(t, "broker unreachable", got.LastError)

		// Least recently reconciled first: the deletion just reconciled
		// goes after the one never reconciled, whatever their request
		// times, and a reconcile of that one puts it last in turn.
		stopping, err = s.ListUserDeletionsByState(ctx, store.UserDeletionStateStopping, 0)
		require.NoError(t, err)
		require.Len(t, stopping, 2)
		assert.Equal(t, ids[2], stopping[0].ID, "never reconciled first")
		assert.Equal(t, ids[0], stopping[1].ID)
		require.NoError(t, s.TouchUserDeletionReconciled(ctx, ids[2], at.Add(time.Minute), ""))
		stopping, err = s.ListUserDeletionsByState(ctx, store.UserDeletionStateStopping, 1)
		require.NoError(t, err)
		require.Len(t, stopping, 1)
		assert.Equal(t, ids[0], stopping[0].ID, "least recently reconciled first")

		// Unknown IDs are not found; a completed deletion is left as it is.
		assert.ErrorIs(t, s.TouchUserDeletionReconciled(ctx, uuid.NewString(), at, ""), store.ErrNotFound)
		assert.ErrorIs(t, s.TouchUserDeletionReconciled(ctx, "not-a-uuid", at, ""), store.ErrNotFound)
		require.NoError(t, s.TouchUserDeletionReconciled(ctx, ids[1], at, "late"))
		done, err := s.GetUserDeletion(ctx, ids[1])
		require.NoError(t, err)
		assert.Empty(t, done.LastError, "a completed deletion takes no reconcile error")
		assert.Nil(t, done.LastReconciledAt)
	})

	t.Run("FinalOutcomesAreKept", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		d := newDeletion(u.ID, time.Now())
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		rec := func(agentID, outcome string, attempts int) *store.UserDeletionAgent {
			return &store.UserDeletionAgent{DeletionID: d.ID, AgentID: agentID, Slug: "s", ProjectID: "p1",
				Outcome: outcome, Attempts: attempts}
		}
		get := func(agentID string) store.UserDeletionAgent {
			t.Helper()
			all, err := s.ListUserDeletionAgents(ctx, d.ID)
			require.NoError(t, err)
			for _, r := range all {
				if r.AgentID == agentID {
					return r
				}
			}
			t.Fatalf("no record of agent %s", agentID)
			return store.UserDeletionAgent{}
		}

		stopped, removed := uuid.NewString(), uuid.NewString()
		require.NoError(t, s.UpsertUserDeletionAgent(ctx, rec(stopped, store.UserDeletionOutcomeStopping, 1)))
		require.NoError(t, s.UpsertUserDeletionAgent(ctx, rec(stopped, store.UserDeletionOutcomeStopped, 1)))
		require.NoError(t, s.UpsertUserDeletionAgent(ctx, rec(removed, store.UserDeletionOutcomeRemoved, 1)))

		// A writer holding an older copy cannot replace a final outcome.
		for _, o := range []string{store.UserDeletionOutcomePending, store.UserDeletionOutcomeStopping,
			store.UserDeletionOutcomeFailed, store.UserDeletionOutcomeRemoved} {
			require.NoError(t, s.UpsertUserDeletionAgent(ctx, rec(stopped, o, 7)), o)
			got := get(stopped)
			assert.Equal(t, store.UserDeletionOutcomeStopped, got.Outcome, "stopped is kept over %s", o)
			assert.Equal(t, 1, got.Attempts)
		}
		require.NoError(t, s.UpsertUserDeletionAgent(ctx, rec(removed, store.UserDeletionOutcomeStopped, 7)))
		assert.Equal(t, store.UserDeletionOutcomeRemoved, get(removed).Outcome, "removed is kept")

		// Reopen moves a final record back, and creates a missing one.
		require.NoError(t, s.ReopenUserDeletionAgent(ctx, rec(stopped, store.UserDeletionOutcomePending, 1)))
		assert.Equal(t, store.UserDeletionOutcomePending, get(stopped).Outcome)
		require.NoError(t, s.UpsertUserDeletionAgent(ctx, rec(stopped, store.UserDeletionOutcomeStopping, 2)))
		assert.Equal(t, store.UserDeletionOutcomeStopping, get(stopped).Outcome, "a reopened record takes upserts")
		fresh := uuid.NewString()
		require.NoError(t, s.ReopenUserDeletionAgent(ctx, rec(fresh, store.UserDeletionOutcomePending, 0)))
		assert.Equal(t, store.UserDeletionOutcomePending, get(fresh).Outcome)

		assert.ErrorIs(t, s.ReopenUserDeletionAgent(ctx, rec(fresh, "gone", 0)), store.ErrInvalidInput)
		other := rec(fresh, store.UserDeletionOutcomePending, 0)
		other.DeletionID = uuid.NewString()
		assert.ErrorIs(t, s.ReopenUserDeletionAgent(ctx, other), store.ErrNotFound)
	})

	t.Run("AgentWritesInsideTransaction", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		d := newDeletion(u.ID, time.Now())
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		agentID := uuid.NewString()
		// Every write path inside one transaction, the record existing and
		// final for the later ones: no statement may fail (on PostgreSQL a
		// failed statement aborts the transaction), and the commit keeps
		// the final outcome.
		err := s.WithTx(ctx, func(tx store.Store) error {
			for _, o := range []string{store.UserDeletionOutcomePending, store.UserDeletionOutcomeStopped,
				store.UserDeletionOutcomePending} {
				if err := tx.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
					DeletionID: d.ID, AgentID: agentID, Outcome: o}); err != nil {
					return err
				}
			}
			return nil
		})
		require.NoError(t, err)
		got, err := s.ListUserDeletionAgents(ctx, d.ID)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, store.UserDeletionOutcomeStopped, got[0].Outcome)
	})

	t.Run("RequestRollsBackTogether", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		tokenID := uuid.NewString()
		require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
			ID: tokenID, UserID: u.ID, Name: "t", Prefix: tokenID[:8], KeyHash: "hash-" + tokenID,
			BoundaryKind: "hub", Scopes: []string{"agent:read"},
		}))
		d := newDeletion(u.ID, time.Now())
		errRollback := fmt.Errorf("rollback")
		// The writes of a force request, in one transaction that fails at
		// the end: none of them may persist.
		err := s.WithTx(ctx, func(tx store.Store) error {
			require.NoError(t, tx.MarkUserDeleting(ctx, u.ID))
			n, err := tx.RevokeUserAccessTokensForUser(ctx, u.ID)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			require.NoError(t, tx.CreateUserDeletion(ctx, d))
			require.NoError(t, tx.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
				DeletionID: d.ID, AgentID: uuid.NewString(), Outcome: store.UserDeletionOutcomePending}))
			return errRollback
		})
		require.ErrorIs(t, err, errRollback)

		after, err := s.GetUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusActive, after.Status)
		left, err := s.CountUserAccessTokens(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, left, "the token revoke rolled back")
		_, err = s.GetUserDeletionByUser(ctx, u.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		recs, err := s.ListUserDeletionAgents(ctx, d.ID)
		require.NoError(t, err)
		assert.Empty(t, recs)
	})

	t.Run("Agents", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		d := newDeletion(u.ID, time.Now())
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		agentA, agentB := uuid.NewString(), uuid.NewString()
		if agentB < agentA {
			agentA, agentB = agentB, agentA
		}
		for _, id := range []string{agentB, agentA} {
			require.NoError(t, s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
				DeletionID: d.ID, AgentID: id, Slug: "slug-" + id[:4], ProjectID: "p1",
				Outcome: store.UserDeletionOutcomePending,
			}))
		}
		// Replace one record.
		require.NoError(t, s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
			DeletionID: d.ID, AgentID: agentA, Slug: "slug-" + agentA[:4], ProjectID: "p1",
			Outcome: store.UserDeletionOutcomeFailed, Attempts: 2, LastError: "boom",
		}))
		got, err := s.ListUserDeletionAgents(ctx, d.ID)
		require.NoError(t, err)
		require.Len(t, got, 2, "an upsert replaces the record of the same agent")
		assert.Equal(t, agentA, got[0].AgentID, "ordered by agent ID")
		assert.Equal(t, store.UserDeletionOutcomeFailed, got[0].Outcome)
		assert.Equal(t, 2, got[0].Attempts)
		assert.Equal(t, "boom", got[0].LastError)
		assert.False(t, got[0].UpdatedAt.IsZero())
		assert.Equal(t, store.UserDeletionOutcomePending, got[1].Outcome)

		forAgent, err := s.ListUserDeletionAgentsForAgent(ctx, agentA)
		require.NoError(t, err)
		require.Len(t, forAgent, 1)
		assert.Equal(t, d.ID, forAgent[0].DeletionID)

		// A completed deletion is not found from its agents.
		_, err = s.CompleteUserDeletion(ctx, d.ID, time.Now())
		require.NoError(t, err)
		forAgent, err = s.ListUserDeletionAgentsForAgent(ctx, agentA)
		require.NoError(t, err)
		assert.Empty(t, forAgent)

		err = s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{DeletionID: d.ID, AgentID: agentA, Outcome: "gone"})
		assert.ErrorIs(t, err, store.ErrInvalidInput)
		err = s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{DeletionID: uuid.NewString(), AgentID: agentA, Outcome: store.UserDeletionOutcomePending})
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("TombstoneOutlivesUserAndBlocksReuse", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		d := newDeletion(u.ID, time.Now())
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		require.NoError(t, s.DeleteUser(ctx, u.ID))

		got, err := s.GetUserDeletionByUser(ctx, u.ID)
		require.NoError(t, err, "the tombstone answers after the user row is gone")
		assert.Equal(t, d.ID, got.ID)

		again := &store.User{ID: u.ID, Email: "reuse-" + u.Email, DisplayName: "Reuse",
			Role: store.UserRoleMember, Status: store.UserStatusActive}
		assert.ErrorIs(t, s.CreateUser(ctx, again), store.ErrAlreadyExists, "a tombstoned user ID is never reused")
		_, err = s.GetUser(ctx, u.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("DeletingStatus", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		require.NoError(t, s.MarkUserDeleting(ctx, u.ID))
		got, err := s.GetUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusDeleting, got.Status)

		// UpdateUser refuses a deleting row, whatever it writes, and the
		// status stays deleting.
		got.Status = store.UserStatusActive
		assert.ErrorIs(t, s.UpdateUser(ctx, got), store.ErrUserDeleting)
		got.Status = ""
		got.DisplayName = "Renamed"
		assert.ErrorIs(t, s.UpdateUser(ctx, got), store.ErrUserDeleting)
		after, err := s.GetUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusDeleting, after.Status)
		assert.Equal(t, u.DisplayName, after.DisplayName)

		// UpdateUser never writes deleting, and CreateUser never creates it.
		other := seedUser(t, s)
		other.Status = store.UserStatusDeleting
		assert.ErrorIs(t, s.UpdateUser(ctx, other), store.ErrUserDeleting)
		otherAfter, err := s.GetUser(ctx, other.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusActive, otherAfter.Status)
		id := uuid.NewString()
		assert.ErrorIs(t, s.CreateUser(ctx, &store.User{ID: id, Email: "created-deleting-" + id[:8] + "@example.com",
			DisplayName: "x", Role: store.UserRoleMember, Status: store.UserStatusDeleting}), store.ErrUserDeleting)

		assert.ErrorIs(t, s.MarkUserDeleting(ctx, uuid.NewString()), store.ErrNotFound)
	})

	t.Run("RevokeUserAccessTokensForUser", func(t *testing.T) {
		s := factory(t)
		u := seedUser(t, s)
		other := seedUser(t, s)
		mk := func(userID string, revoked bool) string {
			id := uuid.NewString()
			require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
				ID: id, UserID: userID, Name: "t", Prefix: id[:8], KeyHash: "hash-" + id,
				BoundaryKind: "hub", Scopes: []string{"agent:read"}, Revoked: revoked,
			}))
			return id
		}
		mk(u.ID, false)
		mk(u.ID, false)
		mk(u.ID, true)
		mk(other.ID, false)

		n, err := s.RevokeUserAccessTokensForUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, n, "only unrevoked tokens are counted")
		left, err := s.CountUserAccessTokens(ctx, u.ID)
		require.NoError(t, err)
		assert.Zero(t, left)
		otherLeft, err := s.CountUserAccessTokens(ctx, other.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, otherLeft, "another user's tokens are untouched")
		n, err = s.RevokeUserAccessTokensForUser(ctx, u.ID)
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}
