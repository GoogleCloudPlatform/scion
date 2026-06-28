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

package main

import (
	"context"
	"path/filepath"
	"testing"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedTableCount is the number of domain tables in the hub schema
// (excluding the schema_migrations bookkeeping table). The fixture must cover
// every one of them.
const expectedTableCount = 37

// TestFixtureCoverage is the CI coverage gate: it generates the fixture and
// fails if any domain table has zero rows.
func TestFixtureCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	report, err := Generate(context.Background(), path)
	require.NoError(t, err)

	t.Logf("fixture covers %d domain tables", report.TotalTables())
	for _, c := range report.Counts {
		t.Logf("  %-32s %d row(s)", c.Table, c.Count)
	}

	assert.Equal(t, expectedTableCount, report.TotalTables(),
		"fixture should cover exactly the %d domain tables", expectedTableCount)
	assert.Empty(t, report.Missing,
		"every domain table must have at least one fixture row; missing: %v", report.Missing)
}

// TestFixtureLoadable verifies the generated database is a valid, openable
// SQLite store with the seeded data intact.
func TestFixtureLoadable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fixture.db")
	_, err := Generate(ctx, path)
	require.NoError(t, err)

	// Reopen as a fresh Ent client and confirm connectivity + seeded rows.
	client, err := entc.OpenSQLite("file:"+path, entc.PoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	db := client.Driver().(*entsql.Driver).DB()
	require.NoError(t, db.PingContext(ctx))

	var users int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&users))
	assert.Positive(t, users, "users table should have seeded rows")

	// The soft-deleted agent edge case must be present.
	var deletedAgents int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agents WHERE deleted_at IS NOT NULL").Scan(&deletedAgents))
	assert.Positive(t, deletedAgents, "fixture should include a soft-deleted agent")
}

// TestFixtureDeterministic verifies the spec produces a stable set of row
// counts across runs (no time.Now()/random values leaking in).
func TestFixtureDeterministic(t *testing.T) {
	ctx := context.Background()
	r1, err := Generate(ctx, filepath.Join(t.TempDir(), "a.db"))
	require.NoError(t, err)
	r2, err := Generate(ctx, filepath.Join(t.TempDir(), "b.db"))
	require.NoError(t, err)
	assert.Equal(t, r1.Counts, r2.Counts, "row counts should be identical across runs")
}
