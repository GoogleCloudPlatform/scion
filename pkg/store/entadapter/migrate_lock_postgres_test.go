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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Tests for the schema-migration advisory lock (ptone/scion#1078):
// CompositeStore.MigrateWithSchemaLock, the path cmd's migrateStore runs at
// Hub startup. Always compiled; they skip unless the enttest Postgres backend
// is built (-tags integration) and SCION_TEST_POSTGRES_URL is set:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -run 'TestMigrateWithSchemaLock_' ./pkg/store/entadapter/
//
// Manual negative check (not shipped, because an unguarded race is
// timing-dependent and would make a flaky test): change
// migrateLockedForTest below to call cs.Migrate instead.
//   - The externally-held-lock test then fails deterministically: the
//     migration completes without ever waiting on the lock (verified).
//   - The concurrent test then crashes the test binary with "fatal error:
//     concurrent map writes" in ent's Atlas setupTables, because concurrent
//     AutoMigrate calls in ONE process share ent's global table
//     definitions (verified). Separate replica processes do not share that
//     state; across processes the unguarded failure is the Postgres catalog
//     race (SQLSTATE 23505 on pg_type_typname_nsp_index) that the 42P07
//     skip hook does not cover, as measured in ptone/scion#1078. Either
//     way, the lock is what makes concurrent migration safe.

// migrateLockedForTest is the single call site both tests use for the code
// under test, so the manual negative check above is a one-line change.
func migrateLockedForTest(ctx context.Context, cs *CompositeStore) error {
	return cs.MigrateWithSchemaLock(ctx)
}

// openMigrateLockStore opens an independent CompositeStore (its own pool, as
// a separate Hub replica would have) against url.
func openMigrateLockStore(t *testing.T, url string) *CompositeStore {
	t.Helper()
	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 4, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// countSchemaTables returns the number of tables in the URL's current schema.
func countSchemaTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`).Scan(&n))
	return n
}

// TestMigrateWithSchemaLock_ConcurrentReplicasAllSucceed boots several
// "replicas" (independent stores and pools) against one fresh, empty schema
// at the same instant and requires every one of them to complete the full
// migration without error, leaving one fully provisioned schema.
func TestMigrateWithSchemaLock_ConcurrentReplicasAllSucceed(t *testing.T) {
	url := enttest.NewEmptySchemaURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const replicas = 6
	stores := make([]*CompositeStore, replicas)
	for i := range stores {
		stores[i] = openMigrateLockStore(t, url)
		// Open each pool's first connection up front so the goroutines
		// below race on the migration itself, not on connection setup.
		require.NoError(t, stores[i].DB().PingContext(ctx))
	}

	start := make(chan struct{})
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = migrateLockedForTest(ctx, stores[i])
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "replica %d failed to migrate", i)
	}

	// Every replica saw one consistent schema: the same table set a single
	// sequential migration produces.
	ref := openMigrateLockStore(t, enttest.NewEmptySchemaURL(t))
	require.NoError(t, ref.Migrate(ctx))
	want := countSchemaTables(t, ref.DB())
	require.Positive(t, want)
	require.Equal(t, want, countSchemaTables(t, stores[0].DB()),
		"concurrent locked migration must produce the same tables as a sequential one")

	// The lock is released afterwards: a later boot can take it again.
	require.NoError(t, migrateLockedForTest(ctx, stores[0]))
}

// TestMigrateWithSchemaLock_WaitsForExternallyHeldLock is the deterministic
// proof that the migration is gated by store.LockSchemaMigration: while
// another session holds the lock, the migration must block on it (visible as
// an ungranted advisory lock in pg_locks) and create nothing; once the lock
// is released it must complete. Without the lock the migration would finish
// while the lock is still held, which fails the test.
func TestMigrateWithSchemaLock_WaitsForExternallyHeldLock(t *testing.T) {
	url := enttest.NewEmptySchemaURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	holderDB, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holderDB.Close() })
	holder, err := holderDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close() })
	_, err = holder.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(store.LockSchemaMigration))
	require.NoError(t, err)
	held := true
	t.Cleanup(func() {
		if held {
			_, _ = holder.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(store.LockSchemaMigration))
		}
	})

	cs := openMigrateLockStore(t, url)
	done := make(chan error, 1)
	go func() { done <- migrateLockedForTest(ctx, cs) }()

	// A bigint advisory key appears in pg_locks as classid = high 32 bits,
	// objid = low 32 bits, objsubid = 1.
	key := int64(store.LockSchemaMigration)
	waiting := func() bool {
		var n int
		require.NoError(t, holderDB.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			   AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid
			   AND objsubid = 1`, key).Scan(&n))
		return n > 0
	}

	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
waitLoop:
	for {
		select {
		case err := <-done:
			t.Fatalf("migration completed (err=%v) while store.LockSchemaMigration was held by another session; it must wait for the lock", err)
		case <-deadline:
			t.Fatal("migration never queued on store.LockSchemaMigration within 30s")
		case <-tick.C:
			if waiting() {
				break waitLoop
			}
		}
	}
	require.Zero(t, countSchemaTables(t, holderDB),
		"no table may be created while the migration lock is held elsewhere")

	_, err = holder.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", int64(store.LockSchemaMigration))
	require.NoError(t, err)
	held = false

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("migration did not complete after the lock was released")
	}
	require.Positive(t, countSchemaTables(t, holderDB))
}
