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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/hubtestutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// topicsBatchRow is one webchat_topic row seeded directly, so the test
// controls last_activity_at (including NULL) and deleted_at exactly.
type topicsBatchRow struct {
	id, projectID, name string
	activity            *time.Time
	lastMessageID       string
	deleted             bool
}

// seedTopicsBatchRows inserts rows into webchat_topic. Rows are inserted
// with SQL rather than CreateTopic because the Postgres CreateTopic also
// mints a row in the Ent-managed conversations table, which a bare
// webchat store schema does not have.
func seedTopicsBatchRows(t *testing.T, db *sql.DB, dialect string, rows []topicsBatchRow) {
	t.Helper()
	created := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	for _, r := range rows {
		var activity, deleted, createdAt interface{}
		var lastMsg interface{}
		if r.lastMessageID != "" {
			lastMsg = r.lastMessageID
		}
		q := `INSERT INTO webchat_topic (id, project_id, name, created_by, created_at, last_message_id, last_activity_at, deleted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
		if dialect == "postgres" {
			q = `INSERT INTO webchat_topic (id, project_id, name, created_by, created_at, last_message_id, last_activity_at, deleted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
			createdAt = created
			if r.activity != nil {
				activity = *r.activity
			}
			if r.deleted {
				deleted = created
			}
		} else {
			createdAt = created.Format(time.RFC3339Nano)
			if r.activity != nil {
				activity = r.activity.UTC().Format(time.RFC3339Nano)
			}
			if r.deleted {
				deleted = created.Format(time.RFC3339Nano)
			}
		}
		_, err := db.Exec(q, r.id, r.projectID, r.name, "user-1", createdAt, lastMsg, activity, deleted)
		require.NoError(t, err, "seed topic %s", r.id)
	}
}

// assertListTopicsByProjectsParity checks that ListTopicsByProjects returns
// exactly the union of ListTopics over the requested projects: the same
// rows in the same per-project order, grouped by project ID, excluding
// soft-deleted topics and unrequested projects, with NULL activity times
// read back as the zero time.
func assertListTopicsByProjectsParity(t *testing.T, db *sql.DB, dialect string, wcs WebChatStore) {
	t.Helper()
	ctx := context.Background()
	at := func(h int) *time.Time {
		ts := time.Date(2026, 3, 1, h, 0, 0, 0, time.UTC)
		return &ts
	}
	seedTopicsBatchRows(t, db, dialect, []topicsBatchRow{
		{id: "a-old", projectID: "proj-a", name: "old", activity: at(1), lastMessageID: "m1"},
		{id: "a-new", projectID: "proj-a", name: "new", activity: at(5), lastMessageID: "m2"},
		{id: "a-quiet", projectID: "proj-a", name: "quiet"},
		{id: "a-gone", projectID: "proj-a", name: "gone", activity: at(9), lastMessageID: "m3", deleted: true},
		{id: "b-only", projectID: "proj-b", name: "only", activity: at(3), lastMessageID: "m4"},
		{id: "d-unasked", projectID: "proj-d", name: "unasked", activity: at(4), lastMessageID: "m5"},
	})

	empty, err := wcs.ListTopicsByProjects(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "no projects, no topics")

	requested := []string{"proj-b", "proj-c", "proj-a"}
	got, err := wcs.ListTopicsByProjects(ctx, requested)
	require.NoError(t, err)

	byProject := map[string][]WebChatTopic{}
	for i, tp := range got {
		if i > 0 {
			require.LessOrEqual(t, got[i-1].ProjectID, tp.ProjectID, "rows must be grouped by project_id")
		}
		byProject[tp.ProjectID] = append(byProject[tp.ProjectID], tp)
	}
	assert.NotContains(t, byProject, "proj-d", "unrequested project must not appear")

	total := 0
	for _, pid := range requested {
		want, err := wcs.ListTopics(ctx, pid)
		require.NoError(t, err)
		total += len(want)
		require.Len(t, byProject[pid], len(want), "project %s", pid)
		for i := range want {
			assert.Equal(t, want[i].ID, byProject[pid][i].ID, "project %s row %d", pid, i)
			assert.Equal(t, want[i].LastMessageID, byProject[pid][i].LastMessageID, "project %s row %d", pid, i)
			assert.True(t, want[i].LastActivityAt.Equal(byProject[pid][i].LastActivityAt), "project %s row %d", pid, i)
		}
	}
	assert.Len(t, got, total)
	assert.Len(t, byProject["proj-a"], 3, "soft-deleted topic excluded")

	for _, tp := range byProject["proj-a"] {
		if tp.ID == "a-quiet" {
			assert.True(t, tp.LastActivityAt.IsZero(), "NULL last_activity_at reads as the zero time")
		}
	}
}

func TestListTopicsByProjects_SQLite(t *testing.T) {
	db := hubtestutil.OpenTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	assertListTopicsByProjectsParity(t, db, "sqlite", wcs)
}

func TestListTopicsByProjects_Postgres(t *testing.T) {
	dsn := hubtestutil.RequirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	pgDropWebchatTables(t, db)
	t.Cleanup(func() { pgDropWebchatTables(t, db) })

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	assertListTopicsByProjectsParity(t, db, "postgres", wcs)
}

// assertListTopicsIncludingDeletedForProjectsParity checks that
// ListTopicsIncludingDeletedForProjects returns the exact union of
// ListTopicsIncludingDeleted over the requested projects (including
// soft-deleted topics and preserving per-project order), excluding unrequested
// projects and handling empty/duplicate project IDs.
func assertListTopicsIncludingDeletedForProjectsParity(t *testing.T, db *sql.DB, dialect string, wcs WebChatStore) {
	t.Helper()
	ctx := context.Background()
	at := func(h int) *time.Time {
		ts := time.Date(2026, 3, 1, h, 0, 0, 0, time.UTC)
		return &ts
	}
	seedTopicsBatchRows(t, db, dialect, []topicsBatchRow{
		{id: "a-old", projectID: "proj-a", name: "old", activity: at(1), lastMessageID: "m1"},
		{id: "a-new", projectID: "proj-a", name: "new", activity: at(5), lastMessageID: "m2"},
		{id: "a-quiet", projectID: "proj-a", name: "quiet"},
		{id: "a-gone", projectID: "proj-a", name: "gone", activity: at(9), lastMessageID: "m3", deleted: true},
		{id: "b-only", projectID: "proj-b", name: "only", activity: at(3), lastMessageID: "m4"},
		{id: "d-unasked", projectID: "proj-d", name: "unasked", activity: at(4), lastMessageID: "m5"},
	})

	empty, err := wcs.ListTopicsIncludingDeletedForProjects(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	requested := []string{"proj-b", "proj-c", "proj-a", "proj-a", ""}
	got, err := wcs.ListTopicsIncludingDeletedForProjects(ctx, requested)
	require.NoError(t, err)
	assert.NotContains(t, got, "proj-d", "unrequested project must not appear")
	assert.NotContains(t, got, "proj-c", "project without topics must not appear")

	for _, pid := range []string{"proj-a", "proj-b"} {
		want, err := wcs.ListTopicsIncludingDeleted(ctx, pid)
		require.NoError(t, err)
		require.Len(t, got[pid], len(want), "project %s", pid)
		for i := range want {
			assert.Equal(t, want[i].ID, got[pid][i].ID, "project %s row %d", pid, i)
			assert.Equal(t, want[i].Name, got[pid][i].Name, "project %s row %d", pid, i)
			assert.Equal(t, want[i].DeletedAt != nil, got[pid][i].DeletedAt != nil, "project %s row %d deleted", pid, i)
			if want[i].DeletedAt != nil && got[pid][i].DeletedAt != nil {
				assert.True(t, want[i].DeletedAt.Equal(*got[pid][i].DeletedAt), "project %s row %d deleted_at", pid, i)
			}
			assert.True(t, want[i].LastActivityAt.Equal(got[pid][i].LastActivityAt), "project %s row %d activity", pid, i)
		}
	}
	assert.Len(t, got["proj-a"], 4, "soft-deleted topic must be included")
}

func TestListTopicsIncludingDeletedForProjects_SQLite(t *testing.T) {
	db := hubtestutil.OpenTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	assertListTopicsIncludingDeletedForProjectsParity(t, db, "sqlite", wcs)

	// Verify chunking across > 500 project IDs.
	const numProjects = 515
	rows := make([]topicsBatchRow, numProjects)
	ids := make([]string, numProjects)
	for i := 0; i < numProjects; i++ {
		pid := fmt.Sprintf("chunk-proj-%04d", i)
		ids[i] = pid
		rows[i] = topicsBatchRow{id: fmt.Sprintf("tp-%04d", i), projectID: pid, name: "general", deleted: i%10 == 0}
	}
	seedTopicsBatchRows(t, db, "sqlite", rows)

	got, err := wcs.ListTopicsIncludingDeletedForProjects(context.Background(), ids)
	require.NoError(t, err)
	require.Len(t, got, numProjects)
	assert.NotNil(t, got["chunk-proj-0000"][0].DeletedAt)
	assert.Nil(t, got["chunk-proj-0001"][0].DeletedAt)
	assert.NotNil(t, got["chunk-proj-0510"][0].DeletedAt)
}

func TestListTopicsIncludingDeletedForProjects_Postgres(t *testing.T) {
	dsn := hubtestutil.RequirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	pgDropWebchatTables(t, db)
	t.Cleanup(func() { pgDropWebchatTables(t, db) })

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	assertListTopicsIncludingDeletedForProjectsParity(t, db, "postgres", wcs)
}
