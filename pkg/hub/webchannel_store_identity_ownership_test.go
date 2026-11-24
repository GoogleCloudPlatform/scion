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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// topicConvIdentitySnapshot captures the identity columns across both
// conversations (authoritative owner) and webchat_topic (native projection).
type topicConvIdentitySnapshot struct {
	topicName      string
	topicAgent     string
	topicDeletedAt *time.Time
	convName       string
	convAgentID    string
	convDeletedAt  *time.Time
}

func readIdentitySnapshot(t *testing.T, db *sql.DB, dialect, topicID, convID string) topicConvIdentitySnapshot {
	t.Helper()
	var snap topicConvIdentitySnapshot
	if dialect == "postgres" {
		err := db.QueryRow(
			`SELECT name, COALESCE(default_agent, ''), deleted_at FROM webchat_topic WHERE id = $1`,
			topicID).Scan(&snap.topicName, &snap.topicAgent, &snap.topicDeletedAt)
		require.NoError(t, err)
		err = db.QueryRow(
			`SELECT COALESCE(display_name, ''), COALESCE(default_agent_id::text, ''), deleted_at FROM conversations WHERE id = $1`,
			convID).Scan(&snap.convName, &snap.convAgentID, &snap.convDeletedAt)
		require.NoError(t, err)
		return snap
	}

	var tDelRaw, cDelRaw any
	err := db.QueryRow(
		`SELECT name, COALESCE(default_agent, ''), deleted_at FROM webchat_topic WHERE id = ?`,
		topicID).Scan(&snap.topicName, &snap.topicAgent, &tDelRaw)
	require.NoError(t, err)
	snap.topicDeletedAt = parseSQLiteNullableTime(tDelRaw)

	err = db.QueryRow(
		`SELECT COALESCE(display_name, ''), COALESCE(default_agent_id, ''), deleted_at FROM conversations WHERE id = ?`,
		convID).Scan(&snap.convName, &snap.convAgentID, &cDelRaw)
	require.NoError(t, err)
	snap.convDeletedAt = parseSQLiteNullableTime(cDelRaw)
	return snap
}

func installMirrorFaultTrigger(t *testing.T, db *sql.DB, dialect string) {
	t.Helper()
	if dialect == "postgres" {
		_, err := db.Exec(`
CREATE OR REPLACE FUNCTION fail_webchat_topic_mirror_fn() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'injected mirror failure';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER fail_webchat_topic_mirror
BEFORE UPDATE ON webchat_topic
FOR EACH ROW EXECUTE FUNCTION fail_webchat_topic_mirror_fn();
`)
		require.NoError(t, err)
		return
	}
	_, err := db.Exec(`
CREATE TRIGGER fail_webchat_topic_mirror
BEFORE UPDATE ON webchat_topic
BEGIN
    SELECT RAISE(ABORT, 'injected mirror failure');
END;
`)
	require.NoError(t, err)
}

func removeMirrorFaultTrigger(t *testing.T, db *sql.DB, dialect string) {
	t.Helper()
	if dialect == "postgres" {
		_, err := db.Exec(`DROP TRIGGER IF EXISTS fail_webchat_topic_mirror ON webchat_topic`)
		require.NoError(t, err)
		_, err = db.Exec(`DROP FUNCTION IF EXISTS fail_webchat_topic_mirror_fn()`)
		require.NoError(t, err)
		return
	}
	_, err := db.Exec(`DROP TRIGGER IF EXISTS fail_webchat_topic_mirror`)
	require.NoError(t, err)
}

// assertIdentitySyncAndAtomicity exercises:
//   - UpdateTopic rename syncing conversations.display_name in-tx
//   - UpdateTopic default agent syncing conversations.default_agent_id in-tx
//   - DeleteTopic syncing conversations.deleted_at in-tx
//   - Fault injection on the mirror statement (UPDATE webchat_topic) for rename,
//     default agent, and delete: neither table changes when the mirror fails.
func assertIdentitySyncAndAtomicity(t *testing.T, db *sql.DB, dialect string, wcs WebChatStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID := uuid.NewString()
	keeperID := uuid.NewString()
	topicID := uuid.NewString()
	convID := uuid.NewString()
	agentID := uuid.NewString()
	agentSlug := "dev-agent"

	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:        keeperID,
		ProjectID: projectID,
		Name:      "keeper",
		CreatedBy: "user-1",
		CreatedAt: now,
	}))
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:             topicID,
		ProjectID:      projectID,
		Name:           "initial-name",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      now,
	}))

	// 1. Happy path: rename updates both conversations.display_name and webchat_topic.name.
	renamed := "renamed-topic"
	require.NoError(t, wcs.UpdateTopic(ctx, topicID, TopicUpdate{Name: &renamed}))
	snap := readIdentitySnapshot(t, db, dialect, topicID, convID)
	assert.Equal(t, renamed, snap.topicName)
	assert.Equal(t, renamed, snap.convName)
	assert.Nil(t, snap.topicDeletedAt)
	assert.Nil(t, snap.convDeletedAt)

	// 2. Happy path: default agent update sets both tables.
	require.NoError(t, wcs.UpdateTopic(ctx, topicID, TopicUpdate{
		DefaultAgent:   &agentSlug,
		DefaultAgentID: &agentID,
	}))
	snap = readIdentitySnapshot(t, db, dialect, topicID, convID)
	assert.Equal(t, agentSlug, snap.topicAgent)
	assert.Equal(t, agentID, snap.convAgentID)

	// 3. Fault injection on mirror statement: rename, default agent, and delete
	// must all roll back the authoritative conversations update too.
	installMirrorFaultTrigger(t, db, dialect)

	t.Run("fault_rename_rolls_back_both", func(t *testing.T) {
		badName := "should-rollback-name"
		err := wcs.UpdateTopic(ctx, topicID, TopicUpdate{Name: &badName})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "injected mirror failure")

		after := readIdentitySnapshot(t, db, dialect, topicID, convID)
		assert.Equal(t, renamed, after.topicName, "webchat_topic.name must remain unchanged")
		assert.Equal(t, renamed, after.convName, "conversations.display_name must roll back")
	})

	t.Run("fault_default_agent_rolls_back_both", func(t *testing.T) {
		otherSlug := "other-agent"
		otherID := uuid.NewString()
		err := wcs.UpdateTopic(ctx, topicID, TopicUpdate{
			DefaultAgent:   &otherSlug,
			DefaultAgentID: &otherID,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "injected mirror failure")

		after := readIdentitySnapshot(t, db, dialect, topicID, convID)
		assert.Equal(t, agentSlug, after.topicAgent, "webchat_topic.default_agent must remain unchanged")
		assert.Equal(t, agentID, after.convAgentID, "conversations.default_agent_id must roll back")
	})

	t.Run("fault_delete_rolls_back_both", func(t *testing.T) {
		err := wcs.DeleteTopic(ctx, topicID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "injected mirror failure")

		after := readIdentitySnapshot(t, db, dialect, topicID, convID)
		assert.Nil(t, after.topicDeletedAt, "webchat_topic.deleted_at must remain NULL")
		assert.Nil(t, after.convDeletedAt, "conversations.deleted_at must roll back to NULL")
	})

	removeMirrorFaultTrigger(t, db, dialect)

	// 4. Happy path: DeleteTopic sets deleted_at on both tables in the same tx.
	require.NoError(t, wcs.DeleteTopic(ctx, topicID))
	snap = readIdentitySnapshot(t, db, dialect, topicID, convID)
	require.NotNil(t, snap.topicDeletedAt, "webchat_topic.deleted_at must be set")
	require.NotNil(t, snap.convDeletedAt, "conversations.deleted_at must be set")
	assert.True(t, nullableTimesEqualMicro(snap.topicDeletedAt, snap.convDeletedAt),
		"deleted_at must match across tables (topic=%v conv=%v)", snap.topicDeletedAt, snap.convDeletedAt)

	// 5. Last-thread guard: deleting keeper fails and leaves both rows active.
	keeperConvID := ""
	if dialect == "postgres" {
		require.NoError(t, db.QueryRow(`SELECT conversation_id FROM webchat_topic WHERE id = $1`, keeperID).Scan(&keeperConvID))
	} else {
		keeperConvID = hubtestutil.GetTopicConvID(t, db, keeperID)
	}
	err := wcs.DeleteTopic(ctx, keeperID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last thread")
	keeperSnap := readIdentitySnapshot(t, db, dialect, keeperID, keeperConvID)
	assert.Nil(t, keeperSnap.topicDeletedAt)
	assert.Nil(t, keeperSnap.convDeletedAt)
}

// assertReconcileConvergenceAndIdempotence seeds drifted webchat_topic and
// conversations rows, runs the startup reconcile migration, asserts convergence
// (webchat_topic wins, values preserved without format rewrite), and verifies
// idempotence on both a normal re-Init and a forced re-run with the migration
// marker cleared.
func assertReconcileConvergenceAndIdempotence(t *testing.T, db *sql.DB, dialect string, newStore func() WebChatStore) {
	t.Helper()
	projectID := uuid.NewString()
	created := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	deletedAt := time.Date(2026, 5, 15, 12, 34, 56, 789000000, time.UTC)
	staleConvDel := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	rebind := func(q string) string {
		if dialect != "postgres" {
			return q
		}
		var out []byte
		n := 0
		for i := 0; i < len(q); i++ {
			if q[i] == '?' {
				n++
				out = append(out, fmt.Sprintf("$%d", n)...)
			} else {
				out = append(out, q[i])
			}
		}
		return string(out)
	}
	topicTime := func(ts *time.Time) any {
		if ts == nil {
			return nil
		}
		if dialect == "postgres" {
			return ts.UTC()
		}
		return ts.UTC().Format(time.RFC3339Nano)
	}
	convTime := func(ts *time.Time) any {
		if ts == nil {
			return nil
		}
		return ts.UTC()
	}

	seedPair := func(topicID, convID, topicName, convName string, tDel, cDel *time.Time) {
		var isGen any = 0
		if dialect == "postgres" {
			isGen = false
		}
		_, err := db.Exec(rebind(
			`INSERT INTO webchat_topic (id, project_id, name, is_general, conversation_id, created_by, created_at, deleted_at)
			 VALUES (?, ?, ?, ?, ?, 'user-1', ?, ?)`),
			topicID, projectID, topicName, isGen, convID, topicTime(&created), topicTime(tDel))
		require.NoError(t, err)
		_, err = db.Exec(rebind(
			`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at, deleted_at)
			 VALUES (?, ?, 'group', 'native', ?, '', ?, 'active', ?, ?, ?)`),
			convID, projectID, "thread:"+projectID+":"+topicID, convName, convTime(&created), convTime(&created), convTime(cDel))
		require.NoError(t, err)
	}

	// Case 1: active topic with drifted display_name.
	tRename, cRename := uuid.NewString(), uuid.NewString()
	seedPair(tRename, cRename, "New Topic Name", "Old Topic Name", nil, nil)

	// Case 2: soft-deleted topic whose conversation still has deleted_at=NULL and old name.
	tDel, cDel := uuid.NewString(), uuid.NewString()
	seedPair(tDel, cDel, "Deleted Topic", "Pre-Rename Deleted", &deletedAt, nil)

	// Case 3: active topic whose conversation was wrongly soft-deleted (webchat_topic wins).
	tLive, cLive := uuid.NewString(), uuid.NewString()
	seedPair(tLive, cLive, "Still Live", "Still Live", nil, &staleConvDel)

	// Case 4: already converged topic (must remain untouched).
	tSame, cSame := uuid.NewString(), uuid.NewString()
	seedPair(tSame, cSame, "Converged", "Converged", nil, nil)

	// Case 5: unrelated external conversation (must remain untouched).
	cExternal := uuid.NewString()
	_, err := db.Exec(rebind(
		`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at)
		 VALUES (?, ?, 'group', 'discord', 'discord:chan-1', '', 'Discord Room', 'active', ?, ?)`),
		cExternal, projectID, convTime(&created), convTime(&created))
	require.NoError(t, err)

	// Clear the reconcile marker so Init() runs reconcileTopicConversations against our drifted fixtures.
	_, err = db.Exec(rebind(`DELETE FROM webchat_migrations WHERE name = ?`), "topic_conversation_reconcile")
	require.NoError(t, err)

	// First run: reconcile must converge every drifted row.
	require.NoError(t, newStore().Init())

	assertConverged := func(label string) {
		t.Helper()
		var migCount int
		require.NoError(t, db.QueryRow(rebind(`SELECT COUNT(*) FROM webchat_migrations WHERE name = ?`),
			"topic_conversation_reconcile").Scan(&migCount))
		assert.Equal(t, 1, migCount, "%s: migration marker must be recorded once", label)

		sRename := readIdentitySnapshot(t, db, dialect, tRename, cRename)
		assert.Equal(t, "New Topic Name", sRename.convName, "%s: display_name converged", label)
		assert.Nil(t, sRename.convDeletedAt, "%s: active conversation stays active", label)

		sDel := readIdentitySnapshot(t, db, dialect, tDel, cDel)
		assert.Equal(t, "Deleted Topic", sDel.convName, "%s: deleted topic display_name converged", label)
		require.NotNil(t, sDel.convDeletedAt, "%s: deleted_at propagated to conversation", label)
		assert.True(t, nullableTimesEqualMicro(&deletedAt, sDel.convDeletedAt),
			"%s: deleted_at preserved exact instant (want=%v got=%v)", label, deletedAt, sDel.convDeletedAt)

		sLive := readIdentitySnapshot(t, db, dialect, tLive, cLive)
		assert.Equal(t, "Still Live", sLive.convName, "%s: live topic name preserved", label)
		assert.Nil(t, sLive.convDeletedAt, "%s: webchat_topic wins on deleted_at", label)

		sSame := readIdentitySnapshot(t, db, dialect, tSame, cSame)
		assert.Equal(t, "Converged", sSame.convName, "%s: converged row unchanged", label)
		assert.Nil(t, sSame.convDeletedAt, "%s: converged row stays active", label)

		var extName string
		require.NoError(t, db.QueryRow(rebind(`SELECT display_name FROM conversations WHERE id = ?`), cExternal).Scan(&extName))
		assert.Equal(t, "Discord Room", extName, "%s: external conversation untouched", label)
	}

	assertConverged("after first reconcile")

	// Second run (normal Init with marker present): idempotent no-op.
	require.NoError(t, newStore().Init())
	assertConverged("after second Init")

	// Third run (delete marker to force re-execution of the reconcile SQL itself): still idempotent.
	_, err = db.Exec(rebind(`DELETE FROM webchat_migrations WHERE name = ?`), "topic_conversation_reconcile")
	require.NoError(t, err)
	require.NoError(t, newStore().Init())
	assertConverged("after forced re-reconcile")
}

func TestTopicIdentityOwnership_SQLite(t *testing.T) {
	wcs, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	assertIdentitySyncAndAtomicity(t, db, "sqlite", wcs)
}

func TestTopicReconcile_SQLite(t *testing.T) {
	_, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	assertReconcileConvergenceAndIdempotence(t, db, "sqlite", func() WebChatStore {
		return NewWebChatStore(db, "sqlite")
	})
}

func setupPostgresWithConversations(t *testing.T) (*sql.DB, WebChatStore) {
	t.Helper()
	dsn := hubtestutil.RequirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	pgDropWebchatTables(t, db)
	_, err = db.Exec(`DROP TABLE IF EXISTS conversations CASCADE`)
	require.NoError(t, err)
	t.Cleanup(func() {
		removeMirrorFaultTrigger(t, db, "postgres")
		pgDropWebchatTables(t, db)
		_, _ = db.Exec(`DROP TABLE IF EXISTS conversations CASCADE`)
	})

	_, err = db.Exec(`
CREATE TABLE conversations (
    id               UUID PRIMARY KEY,
    project_id       UUID,
    kind             TEXT NOT NULL,
    surface          TEXT NOT NULL,
    external_ref     TEXT NOT NULL DEFAULT '',
    parent_ref       TEXT NOT NULL DEFAULT '',
    display_name     TEXT NOT NULL DEFAULT '',
    default_agent_id UUID,
    drift_state      TEXT NOT NULL DEFAULT 'active',
    last_activity_at TIMESTAMPTZ,
    last_message_seq BIGINT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ,
    archived_at      TIMESTAMPTZ,
    deleted_at       TIMESTAMPTZ
)`)
	require.NoError(t, err)

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	return db, wcs
}

func TestTopicIdentityOwnership_Postgres(t *testing.T) {
	db, wcs := setupPostgresWithConversations(t)
	assertIdentitySyncAndAtomicity(t, db, "postgres", wcs)
}

func TestTopicReconcile_Postgres(t *testing.T) {
	db, _ := setupPostgresWithConversations(t)
	assertReconcileConvergenceAndIdempotence(t, db, "postgres", func() WebChatStore {
		return NewWebChatStore(db, "postgres")
	})
}
