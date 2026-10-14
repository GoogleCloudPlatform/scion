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

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// lockProbeDriver wraps a dialect.Driver and, on every Exec and Query issued
// outside a transaction, records whether autoMigrateMu was held at that moment.
type lockProbeDriver struct {
	dialect.Driver
	calls    atomic.Int64
	unlocked atomic.Int64
}

func (d *lockProbeDriver) probe() {
	d.calls.Add(1)
	if autoMigrateMu.TryLock() {
		autoMigrateMu.Unlock()
		d.unlocked.Add(1)
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

// TestAutoMigrate_HoldsProcessLock checks that AutoMigrate holds autoMigrateMu
// while it talks to the database: every statement ent's migration issues
// through the client driver must see the lock taken.
func TestAutoMigrate_HoldsProcessLock(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	probe := &lockProbeDriver{Driver: entsql.OpenDB(dialect.SQLite, db)}
	client := ent.NewClient(ent.Driver(probe))

	require.NoError(t, AutoMigrate(context.Background(), client))
	require.Positive(t, probe.calls.Load(), "AutoMigrate issued no statements through the client driver")
	require.Zero(t, probe.unlocked.Load(),
		"%d of %d AutoMigrate statements ran without autoMigrateMu held", probe.unlocked.Load(), probe.calls.Load())
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
			client, err := OpenSQLite(dsn, PoolConfig{})
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
