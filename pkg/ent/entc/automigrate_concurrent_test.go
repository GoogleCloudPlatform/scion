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

package entc

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// lockProbeDriver wraps a dialect.Driver and, on every Exec and Query issued
// directly on it, records whether autoMigrateSem was held at that moment.
// Statements run inside a transaction go through the unwrapped dialect.Tx that
// the embedded driver's Tx returns, so they are not probed.
type lockProbeDriver struct {
	dialect.Driver
	calls    atomic.Int64
	unlocked atomic.Int64
}

func (d *lockProbeDriver) probe() {
	d.calls.Add(1)
	select {
	case autoMigrateSem <- struct{}{}:
		<-autoMigrateSem
		d.unlocked.Add(1)
	default:
	}
}

func (d *lockProbeDriver) Exec(ctx context.Context, query string, args, v any) error {
	d.probe()
	return d.Driver.Exec(ctx, query, args, v)
}

func (d *lockProbeDriver) Query(ctx context.Context, query string, args, v any) error {
	d.probe()
	return d.Driver.Query(ctx, query, args, v)
}

// TestAutoMigrate_HoldsProcessLock checks that AutoMigrate holds autoMigrateSem
// while it talks to the database: every statement issued directly on the
// client driver (outside a transaction) must see the lock taken. These include
// ent's dialect checks at the start of the migration, so the lock is held from
// entry; statements inside Atlas's DDL transaction and the decision_audits drop
// transaction are not probed.
func TestAutoMigrate_HoldsProcessLock(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	probe := &lockProbeDriver{Driver: entsql.OpenDB(dialect.SQLite, db)}
	client := ent.NewClient(ent.Driver(probe))

	require.NoError(t, AutoMigrate(context.Background(), client))
	require.Positive(t, probe.calls.Load(), "AutoMigrate issued no statements through the client driver")
	require.Zero(t, probe.unlocked.Load(),
		"%d of %d AutoMigrate statements ran without autoMigrateSem held", probe.unlocked.Load(), probe.calls.Load())
}

// TestAutoMigrate_ConcurrentCallers runs AutoMigrate from several goroutines at
// once, each against its own in-memory database. ent's migration mutates the
// package-level pkg/ent/migrate tables, so without serialization this crashes
// the test process with "fatal error: concurrent map writes" (ptone/scion#4354).
// That crash is likely but not guaranteed on every run;
// TestAutoMigrate_HoldsProcessLock is the deterministic check.
func TestAutoMigrate_ConcurrentCallers(t *testing.T) {
	const (
		rounds  = 2
		callers = 8
	)
	ctx := context.Background()
	for r := 0; r < rounds; r++ {
		clients := make([]*ent.Client, callers)
		for i := range clients {
			dsn := fmt.Sprintf("file:%s-%d-%d?mode=memory&cache=shared", t.Name(), r, i)
			client, err := OpenSQLite(dsn, PoolConfig{MaxOpenConns: 1})
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			clients[i] = client
		}

		start := make(chan struct{})
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i, client := range clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = AutoMigrate(ctx, client)
			}()
		}
		close(start)
		wg.Wait()

		for i, client := range clients {
			require.NoError(t, errs[i], "round %d caller %d", r, i)
			n, err := client.User.Query().Count(ctx)
			require.NoError(t, err, "round %d caller %d: users table missing after AutoMigrate", r, i)
			require.Zero(t, n)
		}
	}
}

// TestAutoMigrate_CancelledWhileWaiting checks that a caller whose context ends
// while another migration holds the lock returns ctx.Err() promptly and does
// not migrate. The lock is full for the whole call, so whether cancel lands
// before or during the wait, the only ready case is ctx.Done().
func TestAutoMigrate_CancelledWhileWaiting(t *testing.T) {
	client, err := OpenSQLite("file:"+t.Name()+"?mode=memory&cache=shared", PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	autoMigrateSem <- struct{}{} // stand in for a migration that is running
	held := true
	release := func() {
		if held {
			held = false
			<-autoMigrateSem
		}
	}
	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- AutoMigrate(ctx, client) }()
	cancel()

	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(30 * time.Second):
		t.Fatal("AutoMigrate did not return within 30s after its context was cancelled while waiting for the lock")
	}
	release()

	_, err = client.User.Query().Count(context.Background())
	require.Error(t, err, "users table exists: the cancelled caller migrated")
}
