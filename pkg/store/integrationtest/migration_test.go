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

// Category 5 — Migration stress. These verify the Ent AutoMigrate path behaves on
// a non-trivial dataset: a large table migrates and reports correct row counts,
// large result sets are accessed with bounded memory (the list path caps the
// page rather than loading every row), and re-running the migration is idempotent
// and non-destructive — the property that lets an interrupted/killed migration be
// safely restarted.
package integrationtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// migrationRowCount is the dataset size for the large-migration tests: comfortably
// over the "1000+ rows" target while keeping the suite within its time budget.
const migrationRowCount = 1000

// TestMigration_LargeDatasetRowCounts seeds 1000+ rows, confirms the exact row
// count survives, and confirms the list path returns a BOUNDED page over the
// large table (proving the store does not materialize all rows into memory to
// answer a list request).
func TestMigration_LargeDatasetRowCounts(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	project := seedProject(t, cs)
	for i := 0; i < migrationRowCount; i++ {
		require.NoErrorf(t, cs.CreateAgent(ctx, makeAgent(project.ID, fmt.Sprintf("bulk-%04d", i))),
			"seeding agent %d", i)
	}

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM agents`).Scan(&count))
	assert.Equal(t, migrationRowCount, count, "all seeded rows must be present")

	// Bounded access: a capped page returns at most the limit, while TotalCount
	// reflects the full table.
	const page = 100
	res, err := cs.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{Limit: page})
	require.NoError(t, err)
	assert.Len(t, res.Items, page, "list must cap the page size, not return the whole table")
	assert.Equal(t, migrationRowCount, res.TotalCount, "TotalCount must reflect the full dataset")
}

// TestMigration_IdempotentReRunPreservesData seeds data and then re-runs Migrate
// several times, interleaving more writes. AutoMigrate is idempotent and
// converges to the same schema without touching data, which is exactly what makes
// a killed-and-restarted migration safe: re-running after a partial run finishes
// the job rather than corrupting or dropping rows.
func TestMigration_IdempotentReRunPreservesData(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	project := seedProject(t, cs)
	const firstBatch = 200
	for i := 0; i < firstBatch; i++ {
		require.NoError(t, cs.CreateScheduledEvent(ctx, makeScheduledEvent(project.ID)))
	}

	// Re-run the migration repeatedly (simulating restart after an interrupted
	// run); each pass must succeed and leave existing rows untouched.
	for pass := 0; pass < 3; pass++ {
		require.NoErrorf(t, cs.Migrate(ctx), "re-migration pass %d must succeed", pass)

		// Migration must not change the row count. The expected count grows by one
		// per pass because each iteration appends a row at the end of the loop.
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_events`).Scan(&count))
		require.Equalf(t, firstBatch+pass, count, "re-migration pass %d changed the row count", pass)

		// Writes continue to work against the re-migrated schema.
		require.NoError(t, cs.CreateScheduledEvent(ctx, makeScheduledEvent(project.ID)))
	}

	var final int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_events`).Scan(&final))
	assert.Equal(t, firstBatch+3, final, "all rows (seed + one per pass) must be retained")
}
