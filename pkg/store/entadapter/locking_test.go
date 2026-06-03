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
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On SQLite the advisory lock is a no-op that always succeeds, because the
// single-writer model already makes the guarded work effectively singleton.
func TestTryAdvisoryLock_SQLiteAlwaysAcquires(t *testing.T) {
	c := newTestCompositeStore(t)
	ctx := context.Background()

	acquired, release, err := c.TryAdvisoryLock(ctx, store.LockScheduleEvaluator)
	require.NoError(t, err)
	assert.True(t, acquired, "SQLite advisory lock must always acquire")
	require.NotNil(t, release)
	require.NoError(t, release())

	// A second concurrent acquisition also succeeds (no real lock on SQLite).
	acquired2, release2, err := c.TryAdvisoryLock(ctx, store.LockScheduleEvaluator)
	require.NoError(t, err)
	assert.True(t, acquired2)
	require.NoError(t, release2())
}

// The store satisfies the optional AdvisoryLocker capability used by the hub
// scheduler's singleton gating.
func TestCompositeStore_ImplementsAdvisoryLocker(t *testing.T) {
	var _ store.AdvisoryLocker = newTestCompositeStore(t)
}

// RunSerializable runs the closure inside a transaction and commits it on
// SQLite (no isolation escalation, no retry).
func TestRunSerializable_CommitsOnSQLite(t *testing.T) {
	c := newTestCompositeStore(t)
	ctx := context.Background()

	calls := 0
	err := c.RunSerializable(ctx, func(ctx context.Context, tx *sql.Tx) error {
		calls++
		// A trivial read proves the tx is usable.
		var one int
		return tx.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "SQLite must run the closure exactly once (no retry)")
}

// A non-serialization error from the closure is returned verbatim and the
// transaction is rolled back (no retry loop on SQLite).
func TestRunSerializable_PropagatesError(t *testing.T) {
	c := newTestCompositeStore(t)
	ctx := context.Background()

	sentinel := errors.New("boom")
	calls := 0
	err := c.RunSerializable(ctx, func(ctx context.Context, tx *sql.Tx) error {
		calls++
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, calls)
}

// isSerializationFailure recognizes the Postgres serialization/deadlock SQLSTATEs
// (used to drive the retry loop) without depending on the pgx error type.
func TestIsSerializationFailure(t *testing.T) {
	assert.False(t, isSerializationFailure(nil))
	assert.False(t, isSerializationFailure(errors.New("syntax error")))
	assert.True(t, isSerializationFailure(errors.New("pq: could not serialize access due to concurrent update (SQLSTATE 40001)")))
	assert.True(t, isSerializationFailure(errors.New("ERROR: deadlock detected (SQLSTATE 40P01)")))
}

// ClaimScheduledEvent is the multi-replica dedup primitive: exactly one caller
// wins the pending->fired transition; a second attempt loses.
func TestClaimScheduledEvent_ExactlyOnce(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()

	evt := newTestScheduledEvent(uuid.NewString())
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	won, err := s.ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	assert.True(t, won, "first claim must win")

	// Second claim loses: the event is no longer pending.
	won2, err := s.ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	assert.False(t, won2, "second claim must lose")

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	assert.NotNil(t, got.FiredAt)
}

// Claiming a non-existent event is a clean loss, not an error.
func TestClaimScheduledEvent_MissingLoses(t *testing.T) {
	s := newTestScheduleStore(t)
	won, err := s.ClaimScheduledEvent(context.Background(), uuid.NewString(), store.ScheduledEventFired)
	require.NoError(t, err)
	assert.False(t, won)
}

// On SQLite the two-int advisory lock is a no-op that always succeeds.
func TestTryAdvisoryLockObject_SQLiteAlwaysAcquires(t *testing.T) {
	c := newTestCompositeStore(t)
	ctx := context.Background()

	acquired, release, err := c.TryAdvisoryLockObject(ctx, store.LockWorkspaceProvision, 42)
	require.NoError(t, err)
	assert.True(t, acquired, "SQLite two-int advisory lock must always acquire")
	require.NotNil(t, release)
	require.NoError(t, release())

	// A second concurrent acquisition on the same (classID, objID) also succeeds.
	acquired2, release2, err := c.TryAdvisoryLockObject(ctx, store.LockWorkspaceProvision, 42)
	require.NoError(t, err)
	assert.True(t, acquired2)
	require.NoError(t, release2())
}

// Two-int locks with different objIDs are independent.
func TestTryAdvisoryLockObject_SQLiteDifferentObjIDsIndependent(t *testing.T) {
	c := newTestCompositeStore(t)
	ctx := context.Background()

	acq1, rel1, err := c.TryAdvisoryLockObject(ctx, store.LockWorkspaceProvision, 1)
	require.NoError(t, err)
	assert.True(t, acq1)

	acq2, rel2, err := c.TryAdvisoryLockObject(ctx, store.LockWorkspaceProvision, 2)
	require.NoError(t, err)
	assert.True(t, acq2)

	require.NoError(t, rel1())
	require.NoError(t, rel2())
}

// Under concurrent claims of the same event, exactly one wins. This mirrors two
// replicas recovering the same pending event on startup.
func TestClaimScheduledEvent_ConcurrentSingleWinner(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()

	evt := newTestScheduledEvent(uuid.NewString())
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			won, err := s.ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
			if err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins, "exactly one concurrent claim must win")
}
