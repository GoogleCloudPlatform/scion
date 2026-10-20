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

package hub

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
)

// TestNewTestStoreParallel opens several test stores from parallel
// subtests. A ":memory:" store is not migrated: newTestStore restores a
// private copy of the migrate-once template into it, so this test guards
// concurrent template restores, not concurrent migrations.
// TestMigrateSerializesConcurrentFreshStores guards those.
func TestNewTestStoreParallel(t *testing.T) {
	const stores = 8
	for i := 0; i < stores; i++ {
		t.Run(fmt.Sprintf("store%d", i), func(t *testing.T) {
			t.Parallel()
			if _, err := newTestStore(t, ":memory:"); err != nil {
				t.Fatalf("newTestStore: %v", err)
			}
		})
	}
}

// TestMigrateSerializesConcurrentFreshStores checks that concurrent
// CompositeStore.Migrate calls on fresh databases run their schema
// migrations one at a time. ent's schema migration writes the shared
// pkg/ent/migrate Tables on every call (Atlas.setupTables, in ent's
// dialect/sql/schema/atlas.go, fills each table's column map), so only
// entc.AutoMigrate's process lock keeps two migrations from writing them
// at once.
//
// The check is deterministic and needs no race detector. A holder store
// enters AutoMigrate and stops in its BeforeFKReconcile hook, which runs
// with the lock held. Fresh stores then start Migrate one at a time, and
// each must park waiting for the lock before the next one starts. If one
// finishes instead, its migration ran while the holder held the lock, and
// the test fails. Only one unserialized migration can then run besides the
// stopped holder, which has not reached the schema migration yet, so a
// broken lock fails here by name rather than crashing the process with
// "fatal error: concurrent map writes".
func TestMigrateSerializesConcurrentFreshStores(t *testing.T) {
	const fresh = 7
	ctx := context.Background()

	// The holder is a migrated store with one orphaned role binding, so
	// its next Migrate calls the BeforeFKReconcile hook.
	hs, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	holder, ok := hs.(*entadapter.CompositeStore)
	if !ok {
		t.Fatalf("newTestStore returned %T, want *entadapter.CompositeStore", hs)
	}
	seedOrphanRoleBinding(t, holder.DB())

	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
	var heldOnce sync.Once
	holder.SetBeforeFKReconcile(func(context.Context, *sql.DB, []entc.FKRepairGroup) error {
		heldOnce.Do(func() { close(held) })
		<-release
		return nil
	})

	type result struct {
		i   int
		err error
	}
	// Every Migrate goroutine sends to a buffered channel and is counted in
	// migrations. On every exit, a t.Fatalf included, this cleanup releases
	// the holder and waits for all of them before the fresh stores close; it
	// runs before the holder's own close, which newTestStore registered
	// earlier.
	var migrations sync.WaitGroup
	stores := make([]*entadapter.CompositeStore, 0, fresh)
	t.Cleanup(func() {
		releaseHolder()
		migrations.Wait()
		for _, s := range stores {
			_ = s.Close()
		}
	})
	holderErr := make(chan error, 1)
	migrations.Add(1)
	go func() {
		defer migrations.Done()
		holderErr <- holder.Migrate(ctx)
	}()
	select {
	case <-held:
	case err := <-holderErr:
		t.Fatalf("holder Migrate returned before its BeforeFKReconcile hook ran: %v", err)
	}

	done := make(chan result, fresh)
	started := 0
	finishedEarly := false
	for i := 0; i < fresh && !finishedEarly; i++ {
		client, err := openTestClient(fmt.Sprintf("file:hubmigrate%d?mode=memory&cache=shared", testStoreSeq.Add(1)))
		if err != nil {
			t.Fatalf("store%d: open: %v", i, err)
		}
		s := entadapter.NewCompositeStore(client)
		stores = append(stores, s)
		migrations.Add(1)
		go func() {
			defer migrations.Done()
			done <- result{i, s.Migrate(ctx)}
		}()
		started++

		deadline := time.After(time.Minute)
		tick := time.NewTicker(5 * time.Millisecond)
	wait:
		for {
			select {
			case r := <-done:
				started--
				finishedEarly = true
				if r.err != nil {
					t.Errorf("store%d: Migrate failed while the holder held the migration lock: %v", r.i, r.err)
				} else {
					t.Errorf("store%d: Migrate finished while the holder held the migration lock: entc.AutoMigrate does not serialize concurrent migrations", r.i)
				}
				break wait
			case <-tick.C:
				if n := autoMigrateLockWaiters(t.Name()); n == started {
					break wait
				}
			case <-deadline:
				tick.Stop()
				t.Fatalf("store%d: after a minute, %d of %d started Migrate calls are parked waiting for entc.AutoMigrate's lock (autoMigrateLockWaiters may no longer recognise the wait)",
					i, autoMigrateLockWaiters(t.Name()), started)
			}
		}
		tick.Stop()
	}

	releaseHolder()
	if err := <-holderErr; err != nil {
		t.Errorf("holder Migrate: %v", err)
	}
	for ; started > 0; started-- {
		if r := <-done; r.err != nil {
			t.Errorf("store%d: Migrate: %v", r.i, r.err)
		}
	}
	if t.Failed() {
		return
	}
	for i, s := range stores {
		var n int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			t.Errorf("store%d: users table missing after Migrate: %v", i, err)
		}
	}
}

// seedOrphanRoleBinding inserts a role binding whose role definition does
// not exist, with foreign key enforcement off, as a writer without the
// pragma would. Its ON DELETE SET NULL key is one the next Migrate's
// foreign key reconcile repairs, so Migrate calls BeforeFKReconcile.
func seedOrphanRoleBinding(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []struct {
		query string
		args  []any
	}{
		{query: `PRAGMA foreign_keys = OFF`},
		{
			query: `INSERT INTO role_bindings (id, principal_type, principal_id, scope_type, created, role_definition_id) VALUES (?, 'user', ?, 'system', ?, ?)`,
			args:  []any{uuid.NewString(), uuid.NewString(), time.Now().UTC(), uuid.NewString()},
		},
		{query: `PRAGMA foreign_keys = ON`},
	} {
		if _, err := conn.ExecContext(ctx, q.query, q.args...); err != nil {
			t.Fatalf("seed orphan: %s: %v", q.query, err)
		}
	}
}

// autoMigrateLockWaiters counts the goroutines started by the named test
// that are parked waiting for entc.AutoMigrate's process lock: blocked in a
// select whose innermost non-runtime frame is entc.AutoMigrate. AutoMigrate
// has no other select of its own, so a goroutine past the lock never
// matches. The default traceback level already hides runtime frames; they
// are skipped so the match also holds when the level shows them.
func autoMigrateLockWaiters(testName string) int {
	const autoMigrateFrame = "github.com/GoogleCloudPlatform/scion/pkg/ent/entc.AutoMigrate("
	ownFrame := "github.com/GoogleCloudPlatform/scion/pkg/hub." + testName + "."
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	waiters := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		lines := strings.Split(g, "\n")
		if len(lines) < 2 || !strings.Contains(lines[0], " [select") || !strings.Contains(g, ownFrame) {
			continue
		}
		// After the header, lines alternate: function, then file:line.
		for i := 1; i < len(lines); i += 2 {
			if strings.HasPrefix(lines[i], "runtime.") {
				continue
			}
			if strings.HasPrefix(lines[i], autoMigrateFrame) {
				waiters++
			}
			break
		}
	}
	return waiters
}
