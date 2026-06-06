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

// Category 3 — Connection pool stress. These open stores with a deliberately
// small MaxOpenConns and then saturate, block, and forcibly drop connections to
// verify the database/sql pool behaves under pressure: queued requests are served
// once capacity frees up, a saturated pool honors the caller's context deadline
// instead of hanging forever, a long-running transaction does not starve short
// queries on the remaining connections, and the pool transparently heals after
// its backends are killed server-side.
package integrationtest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// openPoolStore opens a CompositeStore against a fresh schema with an explicit
// pool size and optional application_name (used by the recovery test to target
// only its own backends for termination).
func openPoolStore(t *testing.T, maxOpen int, appName string) *entadapter.CompositeStore {
	t.Helper()
	dsn := enttest.NewSchemaURL(t)
	if appName != "" {
		var err error
		dsn, err = enttest.WithConnParam(dsn, "application_name", appName)
		require.NoError(t, err)
	}
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: maxOpen, MaxIdleConns: maxOpen})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestPool_ExhaustedRequestsEventuallySucceed launches far more concurrent
// connection-holding queries than the pool has slots and asserts every one
// eventually completes: requests beyond MaxOpenConns queue and are served as
// connections free up, rather than erroring.
func TestPool_ExhaustedRequestsEventuallySucceed(t *testing.T) {
	cs := newStoreWithPool(t, 4)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)
	require.Equal(t, 4, db.Stats().MaxOpenConnections, "pool sized to 4")

	const tasks = 32 // 8x the pool: most requests must queue
	errs := make(chan error, tasks)
	runConcurrently(tasks, func(int) {
		// pg_sleep holds the connection checked out long enough to force queueing.
		_, err := db.ExecContext(ctx, `SELECT pg_sleep(0.05)`)
		errs <- err
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "a queued request failed instead of waiting for a free connection")
	}
}

// TestPool_SaturatedPoolRespectsContextDeadline checks out every connection in a
// 2-connection pool and holds them, then issues a query with a short deadline.
// With no connection available the acquire must fail with the context deadline
// (a clean, bounded failure) rather than blocking forever; once the held
// connections are released, queries succeed again.
func TestPool_SaturatedPoolRespectsContextDeadline(t *testing.T) {
	cs := newStoreWithPool(t, 2)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	release := make(chan struct{})
	var holding sync.WaitGroup
	for i := 0; i < 2; i++ {
		holding.Add(1)
		go func() {
			defer holding.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				return
			}
			defer conn.Close()
			// Touch the connection so it is genuinely checked out, then hold it.
			_, _ = conn.ExecContext(ctx, `SELECT 1`)
			<-release
		}()
	}

	require.True(t, waitFor(t, 5*time.Second, func() bool { return db.Stats().InUse == 2 }),
		"both pool connections should be checked out")

	shortCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err := db.ExecContext(shortCtx, `SELECT 1`)
	cancel()
	require.Error(t, err, "query on a saturated pool must not hang; it should fail on the deadline")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	close(release)
	holding.Wait()

	_, err = db.ExecContext(ctx, `SELECT 1`)
	require.NoError(t, err, "pool must serve queries again once connections are released")
}

// TestPool_LongTxnDoesNotStarveShortQueries holds one connection of a 4-connection
// pool inside a long (1s) transaction and asserts a batch of short queries on the
// other connections all complete quickly, well before the long transaction does.
func TestPool_LongTxnDoesNotStarveShortQueries(t *testing.T) {
	cs := newStoreWithPool(t, 4)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	longDone := make(chan error, 1)
	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			longDone <- err
			return
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_sleep(1)`); err != nil {
			_ = tx.Rollback()
			longDone <- err
			return
		}
		longDone <- tx.Commit()
	}()

	// Let the long transaction grab its connection first.
	require.True(t, waitFor(t, 5*time.Second, func() bool { return db.Stats().InUse >= 1 }),
		"long transaction should have checked out a connection")

	start := time.Now()
	for i := 0; i < 10; i++ {
		shortCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		var one int
		err := db.QueryRowContext(shortCtx, `SELECT 1`).Scan(&one)
		cancel()
		require.NoErrorf(t, err, "short query %d starved by the long transaction", i)
		require.Equal(t, 1, one)
	}
	elapsed := time.Since(start)
	assert.Lessf(t, elapsed, 900*time.Millisecond,
		"10 short queries took %s — they appear to be waiting on the 1s transaction", elapsed)

	require.NoError(t, <-longDone, "long transaction itself must commit")
}

// TestPool_RecoveryAfterConnectionDrop warms several pooled connections, then
// terminates them server-side with pg_terminate_backend (simulating a CloudSQL
// connection reset / failover). The pool must transparently discard the dead
// connections and open fresh ones so subsequent queries succeed.
func TestPool_RecoveryAfterConnectionDrop(t *testing.T) {
	appName := "scion_pooltest_" + shortID()
	cs := openPoolStore(t, 4, appName)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	// Warm up multiple connections so there are siblings to kill.
	runConcurrently(4, func(int) { _, _ = db.ExecContext(ctx, `SELECT pg_sleep(0.05)`) })

	// Kill every backend with our application_name except the one running this
	// statement, scoping the blast radius to this test's own pool.
	_, err := db.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		   WHERE application_name = $1 AND pid <> pg_backend_pid()`, appName)
	require.NoError(t, err)

	// The pool must heal: each query that lands on a dead connection is retried on
	// a freshly opened one by database/sql.
	for i := 0; i < 20; i++ {
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
		require.NoErrorf(t, err, "query %d failed; pool did not heal after backend termination", i)
		require.Equal(t, 1, one)
	}
}
