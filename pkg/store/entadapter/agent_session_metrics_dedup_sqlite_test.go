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
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A database written before the unique (agent_id, session_id, started_at)
// index can already hold repeated rows for a session segment. Migrate
// removes the repeats, keeping the earliest stored row, so the index can be
// created, and the store then refuses new repeats. Rows of another segment
// of the same session (same ID, later started_at: a resumed session) are
// not repeats and are kept.
func TestMigrate_DeduplicatesAgentSessionMetricsBeforeUniqueIndex(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "dup-session-metrics.db")

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	for _, stmt := range []string{
		oldAgentSessionMetricsDDL,
		oldAgentSessionMetricsAgentIDIndexDDL,
		oldAgentSessionMetricsGroveIDIndexDDL,
		oldAgentSessionMetricsStartedAtIndexDDL,
	} {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	resumed := base.Add(time.Hour) // a later segment of session-1
	firstID, segmentID := uuid.NewString(), uuid.NewString()
	for _, r := range []struct {
		id, agent, session string
		turns              int
		created            time.Time

		started time.Time
	}{
		{uuid.NewString(), "agent-a", "session-1", 9, base.Add(2 * time.Minute), base},
		{firstID, "agent-a", "session-1", 3, base, base},
		{uuid.NewString(), "agent-a", "session-1", 5, base.Add(time.Minute), base},
		{segmentID, "agent-a", "session-1", 4, base.Add(time.Hour), resumed},
		{uuid.NewString(), "agent-a", "session-2", 1, base, base},
		{uuid.NewString(), "agent-b", "session-1", 2, base, base},
	} {
		_, err := raw.ExecContext(ctx, `INSERT INTO agent_session_metrics
			(id, agent_id, grove_id, session_id, started_at, turn_count, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.agent, "project-1", r.session, r.started, r.turns, r.created)
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())

	cs := newTestCompositeStoreFromDSN(t, dsn)
	require.NoError(t, cs.Migrate(ctx))

	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	assert.Contains(t, sqliteObjectSQL(t, db, "agentsessionmetrics_agent_id_session_id_started_at"), "UNIQUE",
		"the unique index must be created")

	rows, err := cs.ListAgentSessionMetricsByProject(ctx, "project-1")
	require.NoError(t, err)
	require.Len(t, rows, 4, "one row per agent, session and segment")
	var keptSegment bool
	for _, r := range rows {
		if r.AgentID == "agent-a" && r.SessionID == "session-1" {
			if r.ID == segmentID {
				keptSegment = true
				assert.Equal(t, 4, r.TurnCount)
				continue
			}
			assert.Equal(t, firstID, r.ID, "the earliest stored row of the first segment is kept")
			assert.Equal(t, 3, r.TurnCount)
		}
	}
	assert.True(t, keptSegment, "the resumed segment's row must not be removed")

	err = cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: "agent-a", ProjectID: "project-1", SessionID: "session-2", StartedAt: base,
	})
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	// Idempotent: a second boot finds nothing to remove.
	require.NoError(t, cs.Migrate(ctx))
	rows, err = cs.ListAgentSessionMetricsByProject(ctx, "project-1")
	require.NoError(t, err)
	assert.Len(t, rows, 4)
}
