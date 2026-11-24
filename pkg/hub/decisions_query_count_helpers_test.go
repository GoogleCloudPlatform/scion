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
	"sync"
	"testing"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// countingDialectDriver wraps an ent dialect.Driver and counts every SQL
// statement (Query and Exec, both top-level and inside transactions) issued
// through it. Reusable across Decisions scaling query-count regression tests
// (design-scale §7 item 4).
type countingDialectDriver struct {
	dialect.Driver
	mu      sync.Mutex
	queries []string
}

func newCountingDialectDriver(drv dialect.Driver) *countingDialectDriver {
	return &countingDialectDriver{Driver: drv}
}

// Unwrap lets entc.SQLDriver reach the underlying *sql.Driver for raw DB access.
func (d *countingDialectDriver) Unwrap() dialect.Driver {
	return d.Driver
}

func (d *countingDialectDriver) record(q string) {
	d.mu.Lock()
	d.queries = append(d.queries, q)
	d.mu.Unlock()
}

func (d *countingDialectDriver) Reset() {
	d.mu.Lock()
	d.queries = nil
	d.mu.Unlock()
}

func (d *countingDialectDriver) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queries)
}

func (d *countingDialectDriver) Statements() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.queries))
	copy(out, d.queries)
	return out
}

func (d *countingDialectDriver) Exec(ctx context.Context, query string, args, v any) error {
	d.record(query)
	return d.Driver.Exec(ctx, query, args, v)
}

func (d *countingDialectDriver) Query(ctx context.Context, query string, args, v any) error {
	d.record(query)
	return d.Driver.Query(ctx, query, args, v)
}

func (d *countingDialectDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &countingDialectTx{Tx: tx, parent: d}, nil
}

func (d *countingDialectDriver) BeginTx(ctx context.Context, opts *sql.TxOptions) (dialect.Tx, error) {
	if starter, ok := d.Driver.(interface {
		BeginTx(context.Context, *sql.TxOptions) (dialect.Tx, error)
	}); ok {
		tx, err := starter.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &countingDialectTx{Tx: tx, parent: d}, nil
	}
	return d.Tx(ctx)
}

type countingDialectTx struct {
	dialect.Tx
	parent *countingDialectDriver
}

func (t *countingDialectTx) Exec(ctx context.Context, query string, args, v any) error {
	t.parent.record(query)
	return t.Tx.Exec(ctx, query, args, v)
}

func (t *countingDialectTx) Query(ctx context.Context, query string, args, v any) error {
	t.parent.record(query)
	return t.Tx.Query(ctx, query, args, v)
}

// newCountingDecisionsFixture builds a decisionsFixture backed by a
// countingDialectDriver so tests can assert exact or constant SQL query counts
// for profile, limit=0, bell recompute, and topic expansion paths.
func newCountingDecisionsFixture(t *testing.T) (*decisionsFixture, *entadapter.CompositeStore, *countingDialectDriver) {
	t.Helper()
	base := enttest.NewClient(t)
	counter := newCountingDialectDriver(base.Driver())
	cs := entadapter.NewCompositeStore(ent.NewClient(ent.Driver(counter)))
	srv, s := testServerOnMigratedStore(t, cs)
	alice, bob, project := setupDemoPolicyOn(t, srv, s)
	f := newDecisionsFixtureOn(t, srv, s, alice, bob, project)
	return f, cs, counter
}
