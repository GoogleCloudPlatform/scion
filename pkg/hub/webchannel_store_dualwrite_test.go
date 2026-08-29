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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// conversationsTableDDL is the schema for the Ent-managed conversations table,
// reproduced here for testing the dual-write paths that depend on it.
const conversationsTableDDL = `
CREATE TABLE IF NOT EXISTS conversations (
    id              TEXT PRIMARY KEY,
    project_id      TEXT,
    kind            TEXT NOT NULL,
    surface         TEXT NOT NULL,
    external_ref    TEXT NOT NULL DEFAULT '',
    parent_ref      TEXT NOT NULL DEFAULT '',
    display_name    TEXT NOT NULL DEFAULT '',
    drift_state     TEXT NOT NULL DEFAULT 'active',
    default_agent_id TEXT,
    last_activity_at TEXT,
    created_at      TEXT,
    archived_at     TEXT,
    deleted_at      TEXT
)
`

// newTestWebChatStoreWithConversations creates a WebChatStore backed by an
// in-memory SQLite DB that includes the conversations table (Ent-managed in
// production). This enables testing the dual-write paths that require
// hasConversationsTable() to return true.
func newTestWebChatStoreWithConversations(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	// Create the conversations table BEFORE Init so migrations can see it.
	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	return s, db
}

// newPromoteTestStoreWithConversations creates a WebChatStore with in-memory
// SQLite DB that includes both the messages table AND the conversations table.
func newPromoteTestStoreWithConversations(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	// Create the Ent messages table manually.
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT,
    thread_id TEXT,
    msg TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'chat',
    dispatch_state TEXT NOT NULL DEFAULT 'dispatched',
    created TEXT NOT NULL DEFAULT ''
)
`)
	require.NoError(t, err)

	// Create the conversations table.
	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	return s, db
}

// conversationRow holds the relevant columns from a conversations row for test assertions.
type conversationRow struct {
	id, projectID, kind, surface, displayName, driftState string
}

// getConversation reads a single conversations row by ID, returning nil if not found.
func getConversation(t *testing.T, db *sql.DB, convID string) *conversationRow {
	t.Helper()
	var c conversationRow
	err := db.QueryRow(
		`SELECT id, COALESCE(project_id,''), kind, surface, COALESCE(display_name,''), COALESCE(drift_state,'')
		   FROM conversations WHERE id = ?`, convID).
		Scan(&c.id, &c.projectID, &c.kind, &c.surface, &c.displayName, &c.driftState)
	if err == sql.ErrNoRows {
		return nil
	}
	require.NoError(t, err)
	return &c
}

// countConversations returns the number of rows in the conversations table.
func countConversations(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM conversations").Scan(&n))
	return n
}

// getTopicConvID reads conversation_id directly from the webchat_topic table.
func getTopicConvID(t *testing.T, db *sql.DB, topicID string) string {
	t.Helper()
	var convID string
	err := db.QueryRow("SELECT COALESCE(conversation_id, '') FROM webchat_topic WHERE id = ?", topicID).Scan(&convID)
	require.NoError(t, err)
	return convID
}

// ---------------------------------------------------------------------------
// TestCreateTopic_DualWrite_WritesConversation
// ---------------------------------------------------------------------------

func TestCreateTopic_DualWrite_WritesConversation(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	convID := uuid.New().String()
	topic := WebChatTopic{
		ID:             "topic-dw-1",
		ProjectID:      "proj-1",
		Name:           "dual-write-test",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.CreateTopic(ctx, topic))

	// Assert topic row has conversation_id.
	require.Equal(t, convID, getTopicConvID(t, db, "topic-dw-1"))

	// Assert conversation row exists with correct fields.
	c := getConversation(t, db, convID)
	require.NotNil(t, c, "conversations row should exist")
	require.Equal(t, "proj-1", c.projectID)
	require.Equal(t, "group", c.kind)
	require.Equal(t, "native", c.surface)
	require.Equal(t, "dual-write-test", c.displayName)
	require.Equal(t, "active", c.driftState)

	// Verify GetTopic returns ConversationID.
	got, err := s.GetTopic(ctx, "topic-dw-1")
	require.NoError(t, err)
	require.Equal(t, convID, got.ConversationID)
}

func TestCreateTopic_DualWrite_Atomicity(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	convID := uuid.New().String()

	// Insert a conversation row first to cause a duplicate ID conflict.
	_, err := db.Exec(
		`INSERT INTO conversations (id, project_id, kind, surface, display_name, drift_state)
		 VALUES (?, 'proj-x', 'group', 'native', 'pre-existing', 'active')`, convID)
	require.NoError(t, err)

	// CreateTopic with the same convID should fail on the conversation INSERT.
	err = s.CreateTopic(ctx, WebChatTopic{
		ID:             "topic-dw-atomic",
		ProjectID:      "proj-1",
		Name:           "should-rollback",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	})
	require.Error(t, err, "duplicate conversation_id should cause an error")

	// Neither the topic nor a second conversation should be committed.
	got, err := s.GetTopic(ctx, "topic-dw-atomic")
	require.NoError(t, err)
	require.Nil(t, got, "topic should not exist after rollback")

	// Only the pre-existing conversation should remain.
	require.Equal(t, 1, countConversations(t, db))
}

func TestCreateTopic_DualWrite_RequiresProjectID(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	err := s.CreateTopic(context.Background(), WebChatTopic{
		ID:             "topic-no-proj",
		ConversationID: uuid.New().String(),
		Name:           "test",
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "project_id is required")
}

func TestCreateTopic_LegacyPath_NoConversationID(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-legacy",
		ProjectID: "proj-1",
		Name:      "no-conv",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}))

	// Topic created, but no conversation.
	require.Empty(t, getTopicConvID(t, db, "topic-legacy"))
	require.Equal(t, 0, countConversations(t, db))
}

// ---------------------------------------------------------------------------
// TestEnsureGeneralTopic_DualWrite_WritesConversation
// ---------------------------------------------------------------------------

func TestEnsureGeneralTopic_DualWrite_WritesConversation(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	id, created, err := s.EnsureGeneralTopic(ctx, "proj-1", "user-1")
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, id)

	// Assert the topic has a conversation_id.
	convID := getTopicConvID(t, db, id)
	require.NotEmpty(t, convID, "general topic should have conversation_id")

	// Assert conversations row.
	c := getConversation(t, db, convID)
	require.NotNil(t, c)
	require.Equal(t, "proj-1", c.projectID)
	require.Equal(t, "group", c.kind)
	require.Equal(t, "native", c.surface)
	require.Equal(t, "general", c.displayName)
}

func TestEnsureGeneralTopic_DualWrite_Idempotent(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	id1, created1, err := s.EnsureGeneralTopic(ctx, "proj-1", "user-1")
	require.NoError(t, err)
	require.True(t, created1)

	id2, created2, err := s.EnsureGeneralTopic(ctx, "proj-1", "user-2")
	require.NoError(t, err)
	require.False(t, created2)
	require.Equal(t, id1, id2)

	// Only one conversation row should exist.
	require.Equal(t, 1, countConversations(t, db))
}

// ---------------------------------------------------------------------------
// TestPromoteDM_DualWrite_WritesConversation
// ---------------------------------------------------------------------------

func TestPromoteDM_DualWrite_WritesConversation(t *testing.T) {
	s, db := newPromoteTestStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-1:user:user-1"

	// Seed DM data.
	_, err := db.Exec(`
INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES
    ('msg-1', 'proj-1', 'user:alice', 'user-1', 'agent:coder', 'agent-1', 'web', ?, 'hello', '2026-08-22T10:00:00Z'),
    ('msg-2', 'proj-1', 'agent:coder', 'agent-1', 'user:alice', 'user-1', 'web', ?, 'hi back', '2026-08-22T10:01:00Z')
`, dmKey, dmKey)
	require.NoError(t, err)

	require.NoError(t, s.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-1",
		PeerID:          "agent-1",
		PeerKind:        "agent",
	}))

	convID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Second)
	topic := WebChatTopic{
		ID:             "promoted-topic",
		ProjectID:      "proj-1",
		Name:           "Promoted Thread",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      now,
		LastActivityAt: now,
	}

	result, err := s.PromoteDM(ctx, topic, dmKey)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, result.MessageCount)

	// Assert topic has conversation_id.
	require.Equal(t, convID, getTopicConvID(t, db, "promoted-topic"))

	// Assert conversations row.
	c := getConversation(t, db, convID)
	require.NotNil(t, c)
	require.Equal(t, "proj-1", c.projectID)
	require.Equal(t, "group", c.kind)
	require.Equal(t, "native", c.surface)
	require.Equal(t, "Promoted Thread", c.displayName)
}

func TestPromoteDM_NoConversationID_SkipsDualWrite(t *testing.T) {
	s, db := newPromoteTestStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-2:user:user-2"

	_, err := db.Exec(`
INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES ('msg-10', 'proj-1', 'user:bob', 'user-2', 'agent:helper', 'agent-2', 'web', ?, 'test', '2026-08-22T10:00:00Z')
`, dmKey)
	require.NoError(t, err)

	require.NoError(t, s.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-2",
		PeerID:          "agent-2",
		PeerKind:        "agent",
	}))

	now := time.Now().UTC().Truncate(time.Second)
	result, err := s.PromoteDM(ctx, WebChatTopic{
		ID:             "promoted-no-conv",
		ProjectID:      "proj-1",
		Name:           "No Conv Thread",
		CreatedBy:      "user-2",
		CreatedAt:      now,
		LastActivityAt: now,
	}, dmKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	// No conversation should be created when ConversationID is empty.
	require.Equal(t, 0, countConversations(t, db))
}

// ---------------------------------------------------------------------------
// TestBackfillTopicConversations
// ---------------------------------------------------------------------------

func TestBackfillTopicConversations_CreatesConversations(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// Create conversations table first.
	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// The Init() call already ran backfillTopicConversations, but there were
	// no topics yet. Create topics WITHOUT conversation_id to simulate
	// pre-existing data. Use the legacy (no ConversationID) path.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
			ID:        "topic-" + name,
			ProjectID: "proj-1",
			Name:      name,
			CreatedBy: "user-1",
			CreatedAt: now,
		}))
	}

	// Verify no conversation_id set yet.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		require.Empty(t, getTopicConvID(t, db, "topic-"+name))
	}

	// Reset the migration marker so backfill re-runs.
	_, err = db.Exec("DELETE FROM webchat_migrations WHERE name = 'topic_conversation_backfill'")
	require.NoError(t, err)

	// Re-init triggers migrations including backfill.
	s2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s2.Init())

	// Verify all topics now have conversation_id and matching conversation rows.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		convID := getTopicConvID(t, db, "topic-"+name)
		require.NotEmpty(t, convID, "topic %s should have conversation_id after backfill", name)

		c := getConversation(t, db, convID)
		require.NotNil(t, c, "conversation row should exist for topic %s", name)
		require.Equal(t, "proj-1", c.projectID)
		require.Equal(t, "group", c.kind)
		require.Equal(t, "native", c.surface)
		require.Equal(t, name, c.displayName)
	}
}

func TestBackfillTopicConversations_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Create a topic without conversation_id.
	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-idem",
		ProjectID: "proj-1",
		Name:      "idempotent-test",
		CreatedBy: "user-1",
		CreatedAt: now,
	}))

	// Reset migration marker and re-run.
	_, err = db.Exec("DELETE FROM webchat_migrations WHERE name = 'topic_conversation_backfill'")
	require.NoError(t, err)
	s2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s2.Init())

	convID1 := getTopicConvID(t, db, "topic-idem")
	require.NotEmpty(t, convID1)
	count1 := countConversations(t, db)

	// Reset and run again — should produce no duplicates.
	_, err = db.Exec("DELETE FROM webchat_migrations WHERE name = 'topic_conversation_backfill'")
	require.NoError(t, err)
	s3 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s3.Init())

	convID2 := getTopicConvID(t, db, "topic-idem")
	require.Equal(t, convID1, convID2, "conversation_id should not change on re-run")
	require.Equal(t, count1, countConversations(t, db), "no duplicate conversations")
}

func TestBackfillTopicConversations_SkipsWithoutConversationsTable(t *testing.T) {
	// Without the conversations table, backfill should succeed (no-op).
	s, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-no-conv-table",
		ProjectID: "proj-1",
		Name:      "test",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}))

	// Reset and re-run — should not fail even without conversations table.
	_, err := db.Exec("DELETE FROM webchat_migrations WHERE name = 'topic_conversation_backfill'")
	require.NoError(t, err)
	s2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s2.Init())

	// Topic should still have no conversation_id.
	require.Empty(t, getTopicConvID(t, db, "topic-no-conv-table"))
}

// ---------------------------------------------------------------------------
// TestGetTopicConversationID
// ---------------------------------------------------------------------------

func TestGetTopicConversationID(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	convID := uuid.New().String()

	// Create topic with conversation_id.
	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:             "topic-conv-lookup",
		ProjectID:      "proj-1",
		Name:           "conv-lookup-test",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	}))

	// GetTopicConversationID returns the correct ID.
	got, err := s.GetTopicConversationID(ctx, "topic-conv-lookup")
	require.NoError(t, err)
	require.Equal(t, convID, got)

	// Non-existent topic returns store.ErrNotFound.
	_, err = s.GetTopicConversationID(ctx, "nonexistent")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestGetTopicConversationID_NoConversationID(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()

	// Create topic WITHOUT conversation_id.
	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:        "topic-no-conv",
		ProjectID: "proj-1",
		Name:      "no-conv",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC(),
	}))

	// Should return empty string, no error.
	got, err := s.GetTopicConversationID(ctx, "topic-no-conv")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestGetTopicConversationID_SoftDeleted(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	convID := uuid.New().String()

	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:             "topic-soft-del",
		ProjectID:      "proj-1",
		Name:           "will-be-deleted",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	}))

	// Soft-delete the topic.
	require.NoError(t, s.DeleteTopic(ctx, "topic-soft-del"))

	// GetTopicConversationID should return ErrNotFound for deleted topic.
	_, err := s.GetTopicConversationID(ctx, "topic-soft-del")
	require.ErrorIs(t, err, store.ErrNotFound)

	// GetTopicConversationIDIncludingDeleted should still return the ID.
	got, err := s.GetTopicConversationIDIncludingDeleted(ctx, "topic-soft-del")
	require.NoError(t, err)
	require.Equal(t, convID, got)
}

func TestGetTopicConversationIDIncludingDeleted_NotFound(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	_, err := s.GetTopicConversationIDIncludingDeleted(context.Background(), "totally-nonexistent")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// TestCreateTopic_DualWrite_UTX1_NoDeadlock
// ---------------------------------------------------------------------------

func TestCreateTopic_DualWrite_UTX1_NoDeadlock(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// MaxOpenConns=1 simulates the production SQLite pool constraint.
	// If hasConversationsTable() were called inside a tx, this would deadlock.
	db.SetMaxOpenConns(1)

	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	convID := uuid.New().String()
	err = s.CreateTopic(ctx, WebChatTopic{
		ID:             "topic-utx1",
		ProjectID:      "proj-1",
		Name:           "deadlock-test",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	})
	require.NoError(t, err, "CreateTopic should complete without deadlock")
}

func TestEnsureGeneralTopic_DualWrite_UTX1_NoDeadlock(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	db.SetMaxOpenConns(1)

	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err = s.EnsureGeneralTopic(ctx, "proj-1", "user-1")
	require.NoError(t, err, "EnsureGeneralTopic should complete without deadlock")
}

func TestPromoteDM_DualWrite_UTX1_NoDeadlock(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	db.SetMaxOpenConns(1)

	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	// Create the messages table too — PromoteDM needs it.
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT,
    thread_id TEXT,
    msg TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'chat',
    dispatch_state TEXT NOT NULL DEFAULT 'dispatched',
    created TEXT NOT NULL DEFAULT ''
)
`)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dmKey := "dm:agent:a1:user:u1"
	require.NoError(t, s.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "u1",
		PeerID:          "a1",
		PeerKind:        "agent",
	}))

	convID := uuid.New().String()
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.PromoteDM(ctx, WebChatTopic{
		ID:             "topic-utx1-promote",
		ProjectID:      "proj-1",
		Name:           "Promote Deadlock Test",
		ConversationID: convID,
		CreatedBy:      "u1",
		CreatedAt:      now,
		LastActivityAt: now,
	}, dmKey)
	require.NoError(t, err, "PromoteDM should complete without deadlock")
}

// ---------------------------------------------------------------------------
// TestListTopics_ReturnsConversationID
// ---------------------------------------------------------------------------

func TestListTopics_ReturnsConversationID(t *testing.T) {
	s, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	convID := uuid.New().String()

	require.NoError(t, s.CreateTopic(ctx, WebChatTopic{
		ID:             "topic-list-conv",
		ProjectID:      "proj-list",
		Name:           "with-conv",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	}))

	topics, err := s.ListTopics(ctx, "proj-list")
	require.NoError(t, err)
	// Should have the created topic plus a lazily-created #general.
	var found bool
	for _, tp := range topics {
		if tp.ID == "topic-list-conv" {
			require.Equal(t, convID, tp.ConversationID)
			found = true
		}
	}
	require.True(t, found, "topic-list-conv should appear in ListTopics")
}

// ---------------------------------------------------------------------------
// TestMigration_UniqueIndexExists
// ---------------------------------------------------------------------------

// TestMigration_UniqueIndexExists verifies that the addTopicConversationID
// migration creates the unique partial index on webchat_topic(conversation_id).
// Without this index a concurrent backfill could silently assign the same
// conversation to two topics — the constraint is the last line of defence.
func TestMigration_UniqueIndexExists(t *testing.T) {
	_, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_webchat_topic_conversation'`,
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "unique index idx_webchat_topic_conversation must exist after migration")
}

// ---------------------------------------------------------------------------
// TestMigration_UniqueIndexRejectsDuplicateConversationID
// ---------------------------------------------------------------------------

// TestMigration_UniqueIndexRejectsDuplicateConversationID proves the unique
// index actually enforces one-topic-per-conversation by inserting two topics
// with the same conversation_id and expecting a constraint violation on the
// second insert.
func TestMigration_UniqueIndexRejectsDuplicateConversationID(t *testing.T) {
	_, db := newTestWebChatStoreWithConversations(t)
	defer db.Close() //nolint:errcheck

	now := time.Now().UTC().Format(time.RFC3339Nano)
	sharedConvID := "conv-dup-test"

	// First topic with conversation_id succeeds.
	_, err := db.Exec(
		`INSERT INTO webchat_topic (id, project_id, name, conversation_id, created_by, created_at, last_activity_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"topic-a", "proj-dup", "Topic A", sharedConvID, "user-1", now, now)
	require.NoError(t, err, "first insert should succeed")

	// Second topic with the SAME conversation_id must fail.
	_, err = db.Exec(
		`INSERT INTO webchat_topic (id, project_id, name, conversation_id, created_by, created_at, last_activity_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"topic-b", "proj-dup", "Topic B", sharedConvID, "user-1", now, now)
	require.Error(t, err, "second insert with duplicate conversation_id must be rejected by unique index")
	require.Contains(t, err.Error(), "UNIQUE constraint failed", "error should be a constraint violation")
}
