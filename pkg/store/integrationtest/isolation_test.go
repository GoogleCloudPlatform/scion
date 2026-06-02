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

//go:build integration

// Category 2 — Transaction isolation. These exercise Postgres MVCC behavior that
// the store relies on but that cannot be reproduced on SQLite: SERIALIZABLE
// conflict detection with the RunSerializable retry wrapper, REPEATABLE READ
// snapshot stability (no phantom rows), and READ COMMITTED dirty-read prevention.
package integrationtest

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsolation_SerializableRetryRecovers drives two concurrent SERIALIZABLE
// read-modify-write transactions into a genuine serialization conflict and
// verifies CompositeStore.RunSerializable transparently retries the aborted one
// so both increments ultimately land.
//
// Determinism: a two-party barrier makes both transactions perform their initial
// SELECT before either issues its UPDATE, so they read the same value. One commit
// then necessarily aborts with SQLSTATE 40001 (or 40P01); RunSerializable
// re-runs that closure against a fresh snapshot, which reads the now-committed
// value and commits cleanly. The barrier only applies on each transaction's first
// attempt, so a retry can never deadlock against it.
func TestIsolation_SerializableRetryRecovers(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db, "store must expose *sql.DB")

	_, err := db.ExecContext(ctx, `CREATE TABLE iso_counter (id int PRIMARY KEY, val int NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO iso_counter (id, val) VALUES (1, 0)`)
	require.NoError(t, err)

	var barrier sync.WaitGroup
	barrier.Add(2)
	var totalAttempts int64
	errs := make(chan error, 2)

	worker := func() {
		firstAttempt := true
		errs <- cs.RunSerializable(ctx, func(ctx context.Context, tx *sql.Tx) error {
			atomic.AddInt64(&totalAttempts, 1)
			var val int
			if err := tx.QueryRowContext(ctx, `SELECT val FROM iso_counter WHERE id=1`).Scan(&val); err != nil {
				return err
			}
			if firstAttempt {
				firstAttempt = false
				barrier.Done()
				barrier.Wait() // both transactions have now read the same val
			}
			_, err := tx.ExecContext(ctx, `UPDATE iso_counter SET val=$1 WHERE id=1`, val+1)
			return err
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); worker() }()
	go func() { defer wg.Done(); worker() }()
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e, "RunSerializable must recover the conflict, not surface it")
	}

	var val int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT val FROM iso_counter WHERE id=1`).Scan(&val))
	assert.Equal(t, 2, val, "both serializable increments must land — no lost update")
	assert.Greaterf(t, totalAttempts, int64(2),
		"expected at least one retry after a serialization failure, saw only %d attempts", totalAttempts)
	t.Logf("serializable retry: total fn attempts=%d (2 commits + retries)", totalAttempts)
}

// TestIsolation_RepeatableReadNoPhantom verifies a REPEATABLE READ transaction
// sees a stable snapshot: a row inserted by another connection AFTER the
// snapshot's first read is invisible to the transaction (no phantom), yet visible
// to a fresh read once the transaction ends.
func TestIsolation_RepeatableReadNoPhantom(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	_, err := db.ExecContext(ctx, `CREATE TABLE phantom_rows (id int PRIMARY KEY)`)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		_, err := db.ExecContext(ctx, `INSERT INTO phantom_rows (id) VALUES ($1)`, i)
		require.NoError(t, err)
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	var before int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM phantom_rows`).Scan(&before))
	require.Equal(t, 10, before, "snapshot established at 10 rows")

	// Concurrent committed insert on a different (pooled) connection.
	_, err = db.ExecContext(ctx, `INSERT INTO phantom_rows (id) VALUES (1000)`)
	require.NoError(t, err)

	var after int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM phantom_rows`).Scan(&after))
	assert.Equal(t, before, after, "REPEATABLE READ snapshot must not observe the concurrently-inserted phantom row")
	require.NoError(t, tx.Commit())

	var fresh int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM phantom_rows`).Scan(&fresh))
	assert.Equal(t, 11, fresh, "a fresh read after the snapshot ends must see the new row")
}

// TestIsolation_DirtyReadPrevention verifies the default isolation level prevents
// dirty reads: a row written but not yet committed in one transaction is
// invisible to other connections, and stays invisible if that transaction rolls
// back (and becomes visible only on commit).
func TestIsolation_DirtyReadPrevention(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	_, err := db.ExecContext(ctx, `CREATE TABLE dirty_rows (id int PRIMARY KEY)`)
	require.NoError(t, err)

	count := func() int {
		var c int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM dirty_rows`).Scan(&c))
		return c
	}

	// Uncommitted write is invisible to other connections.
	txRollback, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = txRollback.ExecContext(ctx, `INSERT INTO dirty_rows (id) VALUES (1)`)
	require.NoError(t, err)
	assert.Equal(t, 0, count(), "uncommitted insert must not be visible to another connection (no dirty read)")
	require.NoError(t, txRollback.Rollback())
	assert.Equal(t, 0, count(), "rolled-back insert must remain invisible")

	// Committed write becomes visible.
	txCommit, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = txCommit.ExecContext(ctx, `INSERT INTO dirty_rows (id) VALUES (2)`)
	require.NoError(t, err)
	require.NoError(t, txCommit.Commit())
	assert.Equal(t, 1, count(), "committed insert must be visible")
}
