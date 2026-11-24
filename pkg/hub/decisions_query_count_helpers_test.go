//go:build !no_sqlite

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

package hub

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// bellQueryCountingDriver wraps an ent dialect.Driver and counts SQL
// Query/Exec statements (both top-level and inside transactions) while active.
// Shared across Decisions scaling query-count regression tests (design-scale §7).
type bellQueryCountingDriver struct {
	dialect.Driver
	active atomic.Bool
	count  atomic.Int64
}

func newBellQueryCountingDriver(drv dialect.Driver) *bellQueryCountingDriver {
	return &bellQueryCountingDriver{Driver: drv}
}

// Unwrap lets entc.SQLDriver reach the underlying *entsql.Driver for raw DB access.
func (d *bellQueryCountingDriver) Unwrap() dialect.Driver {
	return d.Driver
}

func (d *bellQueryCountingDriver) Reset(active bool) {
	d.count.Store(0)
	d.active.Store(active)
}

func (d *bellQueryCountingDriver) Count() int64 {
	return d.count.Load()
}

func (d *bellQueryCountingDriver) Query(ctx context.Context, query string, args, v any) error {
	if d.active.Load() {
		d.count.Add(1)
	}
	return d.Driver.Query(ctx, query, args, v)
}

func (d *bellQueryCountingDriver) Exec(ctx context.Context, query string, args, v any) error {
	if d.active.Load() {
		d.count.Add(1)
	}
	return d.Driver.Exec(ctx, query, args, v)
}

func (d *bellQueryCountingDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &bellQueryCountingTx{Tx: tx, parent: d}, nil
}

func (d *bellQueryCountingDriver) BeginTx(ctx context.Context, opts *sql.TxOptions) (dialect.Tx, error) {
	if starter, ok := d.Driver.(interface {
		BeginTx(context.Context, *sql.TxOptions) (dialect.Tx, error)
	}); ok {
		tx, err := starter.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &bellQueryCountingTx{Tx: tx, parent: d}, nil
	}
	return d.Tx(ctx)
}

type bellQueryCountingTx struct {
	dialect.Tx
	parent *bellQueryCountingDriver
}

func (t *bellQueryCountingTx) Query(ctx context.Context, query string, args, v any) error {
	if t.parent.active.Load() {
		t.parent.count.Add(1)
	}
	return t.Tx.Query(ctx, query, args, v)
}

func (t *bellQueryCountingTx) Exec(ctx context.Context, query string, args, v any) error {
	if t.parent.active.Load() {
		t.parent.count.Add(1)
	}
	return t.Tx.Exec(ctx, query, args, v)
}

var countingFixtureSeq atomic.Int64

// newCountingDecisionsFixture builds a decisionsFixture backed by the shared
// bellQueryCountingDriver so tests can assert exact or constant SQL query
// counts for profile, limit=0, bell recompute, and topic expansion paths.
// Each call gets a fresh isolated SQLite file so multiple fixtures may be
// created inside the same subtest.
func newCountingDecisionsFixture(t *testing.T) (*decisionsFixture, *entadapter.CompositeStore, *bellQueryCountingDriver) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("counting-%d.db", countingFixtureSeq.Add(1)))
	base, err := entc.OpenSQLite(dbPath, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), base))
	counter := newBellQueryCountingDriver(base.Driver())
	cs := entadapter.NewCompositeStore(ent.NewClient(ent.Driver(counter)))
	srv, s := testServerOnMigratedStore(t, cs)
	alice, bob, project := setupDemoPolicyOn(t, srv, s)
	f := newDecisionsFixtureOn(t, srv, s, alice, bob, project)
	return f, cs, counter
}
