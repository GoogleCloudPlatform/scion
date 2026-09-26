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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oldAgentSessionMetricsDDL is the physical DDL for the
// "agent_session_metrics" table as every deployed database has it: the
// project identifier column is named "grove_id", matching the ent schema's
// field.String("project_id").StorageKey("grove_id") declaration. This DDL
// is byte-identical to what AutoMigrate produces from that schema.
const oldAgentSessionMetricsDDL = "CREATE TABLE `agent_session_metrics` (" +
	"`id` uuid NOT NULL, `agent_id` text NOT NULL, `grove_id` text NOT NULL, " +
	"`session_id` text NOT NULL, `started_at` datetime NOT NULL, " +
	"`ended_at` datetime NULL, `status` text NULL, " +
	"`turn_count` integer NULL DEFAULT (0), `model` text NULL, " +
	"`tokens_input` integer NULL DEFAULT (0), `tokens_output` integer NULL DEFAULT (0), " +
	"`tokens_cached` integer NULL DEFAULT (0), `tokens_reasoning` integer NULL DEFAULT (0), " +
	"`tool_calls` json NULL, `languages` json NULL, " +
	"`created_at` datetime NOT NULL, PRIMARY KEY (`id`))"

const oldAgentSessionMetricsGroveIDIndexDDL = "CREATE INDEX `agentsessionmetrics_grove_id` " +
	"ON `agent_session_metrics` (`grove_id`)"

const oldAgentSessionMetricsAgentIDIndexDDL = "CREATE INDEX `agentsessionmetrics_agent_id` " +
	"ON `agent_session_metrics` (`agent_id`)"

const oldAgentSessionMetricsStartedAtIndexDDL = "CREATE INDEX `agentsessionmetrics_started_at` " +
	"ON `agent_session_metrics` (`started_at`)"

// sqliteObjectSQL returns the CREATE statement sqlite_master recorded for
// the given table or index name, or "" if it does not exist.
func sqliteObjectSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var got sql.NullString
	err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name = ?", name).Scan(&got)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return got.String
}

// TestAgentSessionMetricsAutoMigrate_ProjectIDKeepsGroveIDColumn proves that
// the "agent_session_metrics" table's physical column is named "grove_id"
// and its index "agentsessionmetrics_grove_id", while the ent Go field is
// ProjectID (field.String("project_id").StorageKey("grove_id")). Running
// AutoMigrate against the table must be a complete no-op (no ALTER TABLE,
// no DROP/CREATE INDEX), and data in the physical "grove_id" column must
// read back correctly through the Go ProjectID field.
func TestAgentSessionMetricsAutoMigrate_ProjectIDKeepsGroveIDColumn(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "old-agent-session-metrics.db")

	// Build a database with the agent_session_metrics table already
	// present, using the physical DDL that every deployed database has:
	// the "grove_id" column and its indexes.
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldAgentSessionMetricsDDL)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldAgentSessionMetricsAgentIDIndexDDL)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldAgentSessionMetricsGroveIDIndexDDL)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldAgentSessionMetricsStartedAtIndexDDL)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Insert a row directly through the physical "grove_id" column: it is
	// the only column the physical schema defines for the project
	// identifier.
	legacyProjectID := "legacy-" + uuid.NewString()
	legacySessionID := uuid.New()
	raw, err = sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO agent_session_metrics
		(id, agent_id, grove_id, session_id, started_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		legacySessionID.String(), "legacy-agent", legacyProjectID, "legacy-session",
		time.Now().UTC(), time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	// AutoMigrate is what every hub run performs on startup. The ent
	// schema declares the field as ProjectID via StorageKey("grove_id"),
	// so this must be a no-op against the existing physical table.
	require.NoError(t, entc.AutoMigrate(ctx, client))

	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	assert.Equal(t, oldAgentSessionMetricsDDL, sqliteObjectSQL(t, db, "agent_session_metrics"),
		"AutoMigrate must not alter the agent_session_metrics table shape")
	assert.Equal(t, oldAgentSessionMetricsGroveIDIndexDDL, sqliteObjectSQL(t, db, "agentsessionmetrics_grove_id"),
		"AutoMigrate must not alter the agentsessionmetrics_grove_id index")
	assert.Equal(t, oldAgentSessionMetricsAgentIDIndexDDL, sqliteObjectSQL(t, db, "agentsessionmetrics_agent_id"))
	assert.Equal(t, oldAgentSessionMetricsStartedAtIndexDDL, sqliteObjectSQL(t, db, "agentsessionmetrics_started_at"))
	assert.Empty(t, sqliteObjectSQL(t, db, "agentsessionmetrics_project_id"),
		"AutoMigrate must not create a new index for the ProjectID Go field")

	// Data in the physical "grove_id" column must be readable through the
	// Go ProjectID field.
	metricsStore := NewAgentSessionMetricsStore(client)
	got, err := metricsStore.GetAgentSessionMetrics(ctx, legacySessionID.String())
	require.NoError(t, err)
	assert.Equal(t, legacyProjectID, got.ProjectID,
		"data in the physical grove_id column must be readable via the Go ProjectID field")

	// New writes must also go through the same physical column, so both
	// rows are queried identically.
	newRow := &store.AgentSessionMetrics{
		AgentID:   "new-agent",
		ProjectID: "new-project",
		SessionID: "new-session",
		StartedAt: time.Now().UTC(),
	}
	require.NoError(t, metricsStore.CreateAgentSessionMetrics(ctx, newRow))

	byProject, err := metricsStore.ListAgentSessionMetricsByProject(ctx, "new-project")
	require.NoError(t, err)
	require.Len(t, byProject, 1)
	assert.Equal(t, "new-project", byProject[0].ProjectID)

	var storedGroveID string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT grove_id FROM agent_session_metrics WHERE id = ?", newRow.ID).Scan(&storedGroveID))
	assert.Equal(t, "new-project", storedGroveID,
		"new writes must still land in the physical grove_id column")
}
