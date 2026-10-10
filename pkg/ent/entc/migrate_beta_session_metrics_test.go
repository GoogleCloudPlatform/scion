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
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentsessionmetrics"
)

// newNamedTestClient opens a migrated in-memory SQLite database private to
// this test and name, so a test can hold a source and a destination.
func newNamedTestClient(t *testing.T, name string) *ent.Client {
	t.Helper()
	client, err := OpenSQLite("file:"+t.Name()+"-"+name+"?mode=memory&cache=shared", PoolConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, AutoMigrate(context.Background(), client))
	return client
}

func createSessionMetrics(t *testing.T, c *ent.Client, id uuid.UUID, agentID, sessionID string, startedAt, createdAt time.Time, turns int) {
	t.Helper()
	_, err := c.AgentSessionMetrics.Create().
		SetID(id).
		SetAgentID(agentID).
		SetProjectID("project-1").
		SetSessionID(sessionID).
		SetStartedAt(startedAt).
		SetCreatedAt(createdAt).
		SetTurnCount(turns).
		SetToolCalls(map[string]any{"bash": float64(turns)}).
		Save(context.Background())
	require.NoError(t, err)
}

func sessionMetricsResult(t *testing.T, r *MigrateReport) EntityResult {
	t.Helper()
	for _, e := range r.Entities {
		if e.Entity == "AgentSessionMetrics" {
			return e
		}
	}
	t.Fatal("no AgentSessionMetrics entry in the migration report")
	return EntityResult{}
}

// TestMigrateData_SessionMetricsCopied: without collisions every session
// metrics row is copied with its fields, other entities copy as before, and a
// rerun copies nothing more.
func TestMigrateData_SessionMetricsCopied(t *testing.T) {
	ctx := context.Background()
	src := newNamedTestClient(t, "src")
	dst := newNamedTestClient(t, "dst")

	start := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	created := start.Add(time.Hour)
	idA, idB, idC := uuid.New(), uuid.New(), uuid.New()
	createSessionMetrics(t, src, idA, "agent-a", "s1", start, created, 1)
	// Same session, later segment (resumed after a restart): its own row.
	createSessionMetrics(t, src, idB, "agent-a", "s1", start.Add(time.Hour), created.Add(time.Hour), 2)
	createSessionMetrics(t, src, idC, "agent-b", "s1", start, created, 3)
	// Another entity copies as before, unaffected by the session metrics filter.
	user, err := src.User.Create().SetEmail("user@example.com").SetDisplayName("User").Save(ctx)
	require.NoError(t, err)

	report, err := MigrateData(ctx, src, dst, MigrateOptions{BatchSize: 2})
	require.NoError(t, err)
	for _, e := range report.Entities {
		require.Zero(t, e.Duplicates, "%s reported duplicates", e.Entity)
		require.Equal(t, e.Source, e.Dest, "%s source/dest mismatch", e.Entity)
		if e.Entity == "User" {
			require.Equal(t, 1, e.Inserted)
		}
	}
	gotUser, err := dst.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, "user@example.com", gotUser.Email)
	res := sessionMetricsResult(t, report)
	require.Equal(t, EntityResult{Entity: "AgentSessionMetrics", Source: 3, Inserted: 3, Dest: 3}, res)

	got, err := dst.AgentSessionMetrics.Get(ctx, idB)
	require.NoError(t, err)
	require.Equal(t, "agent-a", got.AgentID)
	require.Equal(t, "project-1", got.ProjectID)
	require.Equal(t, "s1", got.SessionID)
	require.True(t, got.StartedAt.Equal(start.Add(time.Hour)), "started_at = %v", got.StartedAt)
	require.True(t, got.CreatedAt.Equal(created.Add(time.Hour)), "created_at = %v", got.CreatedAt)
	require.Equal(t, 2, got.TurnCount)
	require.Equal(t, map[string]any{"bash": float64(2)}, got.ToolCalls)

	report2, err := MigrateData(ctx, src, dst, MigrateOptions{})
	require.NoError(t, err)
	require.Equal(t, EntityResult{Entity: "AgentSessionMetrics", Source: 3, Skipped: 3, Dest: 3}, sessionMetricsResult(t, report2))
}

// TestMigrateData_SessionMetricsCollisionsKeepEarliest: a source written
// before the unique (agent_id, session_id, started_at) index can hold several
// rows for one segment. The copy does not abort; it keeps the earliest stored
// row (by created_at, then id) and reports the rest as duplicates. A rerun
// stays clean.
func TestMigrateData_SessionMetricsCollisionsKeepEarliest(t *testing.T) {
	ctx := context.Background()
	src := newNamedTestClient(t, "src")
	dst := newNamedTestClient(t, "dst")

	// Recreate a source from before the unique index existed.
	drv, ok := src.Driver().(*entsql.Driver)
	require.True(t, ok)
	_, err := drv.DB().ExecContext(ctx, "DROP INDEX agentsessionmetrics_agent_id_session_id_started_at")
	require.NoError(t, err)

	start := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	created := start.Add(time.Hour)
	// One segment reported three times; the earliest stored row is inserted
	// last so source order does not decide the winner.
	resent := uuid.New()
	retried := uuid.New()
	first := uuid.New()
	createSessionMetrics(t, src, resent, "agent-a", "s1", start, created.Add(2*time.Minute), 30)
	createSessionMetrics(t, src, retried, "agent-a", "s1", start, created.Add(time.Minute), 20)
	createSessionMetrics(t, src, first, "agent-a", "s1", start, created, 10)
	// Same created_at: the lower id wins.
	tieLow := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	tieHigh := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	createSessionMetrics(t, src, tieHigh, "agent-a", "s2", start, created, 2)
	createSessionMetrics(t, src, tieLow, "agent-a", "s2", start, created, 1)
	// A different segment of s1 is not a duplicate.
	segment2 := uuid.New()
	createSessionMetrics(t, src, segment2, "agent-a", "s1", start.Add(time.Hour), created.Add(time.Hour), 5)

	report, err := MigrateData(ctx, src, dst, MigrateOptions{BatchSize: 2})
	require.NoError(t, err)
	require.Equal(t, EntityResult{Entity: "AgentSessionMetrics", Source: 6, Inserted: 3, Duplicates: 3, Dest: 3},
		sessionMetricsResult(t, report))

	rows, err := dst.AgentSessionMetrics.Query().Order(agentsessionmetrics.ByCreatedAt()).All(ctx)
	require.NoError(t, err)
	got := map[uuid.UUID]int{}
	for _, r := range rows {
		got[r.ID] = r.TurnCount
	}
	require.Equal(t, map[uuid.UUID]int{first: 10, tieLow: 1, segment2: 5}, got)

	report2, err := MigrateData(ctx, src, dst, MigrateOptions{})
	require.NoError(t, err)
	require.Equal(t, EntityResult{Entity: "AgentSessionMetrics", Source: 6, Skipped: 3, Duplicates: 3, Dest: 3},
		sessionMetricsResult(t, report2))
}
