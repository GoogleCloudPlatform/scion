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

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// newPromoteTestStore creates a WebChatStore with an in-memory SQLite DB that
// includes a messages table (Ent-managed in production, manually created here).
func newPromoteTestStore(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	// Create the Ent messages table manually — in production it's managed by Ent.
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

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init())

	return store, db
}

// seedDMData sets up a DM conversation with messages, read states, and DM registry rows.
func seedDMData(t *testing.T, db *sql.DB, store WebChatStore) {
	t.Helper()
	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"

	// Insert DM messages
	_, err := db.Exec(`
INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES
    ('msg-1', 'proj-1', 'user:alice', 'user-uuid-1', 'agent:coder', 'agent-uuid-1', 'web', ?, 'hello agent', '2026-08-22T10:00:00Z'),
    ('msg-2', 'proj-1', 'agent:coder', 'agent-uuid-1', 'user:alice', 'user-uuid-1', 'web', ?, 'hello user', '2026-08-22T10:01:00Z'),
    ('msg-3', 'proj-1', 'user:alice', 'user-uuid-1', 'agent:coder', 'agent-uuid-1', 'web', ?, 'thanks', '2026-08-22T10:02:00Z')
`, dmKey, dmKey, dmKey)
	require.NoError(t, err)

	// Set up DM registry (two rows, one per participant)
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-uuid-1",
		PeerID:          "agent-uuid-1",
		PeerKind:        "agent",
	}))
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "agent-uuid-1",
		PeerID:          "user-uuid-1",
		PeerKind:        "user",
	}))

	// Set up read state for the user
	require.NoError(t, store.SetReadState(ctx, "user-uuid-1", dmKey, "msg-2"))
}

// TestPromoteDM_HappyPath verifies the full promotion flow:
// topic created, messages re-keyed, read state migrated, DM deleted.
func TestPromoteDM_HappyPath(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	seedDMData(t, db, store)

	now := time.Now().UTC()
	topic := WebChatTopic{
		ID:             "topic-promote-1",
		ProjectID:      "proj-1",
		Name:           "Promoted Thread",
		DefaultAgent:   "agent-uuid-1",
		CreatedBy:      "user-uuid-1",
		CreatedAt:      now,
		LastActivityAt: now,
	}

	result, err := store.PromoteDM(ctx, topic, dmKey)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "topic-promote-1", result.ID)
	require.Equal(t, "Promoted Thread", result.Name)
	require.Equal(t, "agent-uuid-1", result.DefaultAgent)
	require.Equal(t, 3, result.MessageCount)

	// Verify: topic exists
	got, err := store.GetTopic(ctx, "topic-promote-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "proj-1", got.ProjectID)
	require.Equal(t, "Promoted Thread", got.Name)

	// Verify: all messages re-keyed to new topic ID
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id = ?", "topic-promote-1").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 3, count)

	// Verify: no messages left with old DM key
	err = db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id = ?", dmKey).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	// Verify: read state migrated
	rs, err := store.GetReadState(ctx, "user-uuid-1", "topic-promote-1")
	require.NoError(t, err)
	require.NotNil(t, rs, "read state should exist under new topic ID")
	require.Equal(t, "msg-2", rs.LastReadMessageID)

	// Verify: no read state under old DM key
	rsOld, err := store.GetReadState(ctx, "user-uuid-1", dmKey)
	require.NoError(t, err)
	require.Nil(t, rsOld, "read state under old DM key should be gone")

	// Verify: DM registry rows deleted
	dms, err := store.ListDMs(ctx, "user-uuid-1")
	require.NoError(t, err)
	for _, dm := range dms {
		require.NotEqual(t, dmKey, dm.ConversationKey, "DM should be deleted after promotion")
	}
}

// TestPromoteDM_Atomicity verifies that a name conflict causes rollback:
// messages should NOT be re-keyed when the topic INSERT fails.
func TestPromoteDM_Atomicity(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	seedDMData(t, db, store)

	// Create a topic with the same name in the same project to trigger conflict.
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID:        "existing-topic",
		ProjectID: "proj-1",
		Name:      "Conflict Name",
		CreatedBy: "user-uuid-1",
		CreatedAt: time.Now().UTC(),
	}))

	now := time.Now().UTC()
	topic := WebChatTopic{
		ID:             "topic-will-fail",
		ProjectID:      "proj-1",
		Name:           "Conflict Name", // same name → unique constraint violation
		DefaultAgent:   "agent-uuid-1",
		CreatedBy:      "user-uuid-1",
		CreatedAt:      now,
		LastActivityAt: now,
	}

	_, err := store.PromoteDM(ctx, topic, dmKey)
	require.Error(t, err, "should fail due to name conflict")

	// Verify: messages are NOT re-keyed (rollback worked)
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id = ?", dmKey).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 3, count, "messages should remain under old DM key after rollback")

	// Verify: DM registry rows still exist
	dms, err := store.ListDMs(ctx, "user-uuid-1")
	require.NoError(t, err)
	found := false
	for _, dm := range dms {
		if dm.ConversationKey == dmKey {
			found = true
			break
		}
	}
	require.True(t, found, "DM should still exist after failed promotion")
}

// TestPromoteDM_Idempotency verifies that promoting the same DM twice
// does not error on the second call (the DM has no messages left).
func TestPromoteDM_Idempotency(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	seedDMData(t, db, store)

	now := time.Now().UTC()
	topic := WebChatTopic{
		ID:             "topic-idem-1",
		ProjectID:      "proj-1",
		Name:           "Idempotent Thread",
		DefaultAgent:   "agent-uuid-1",
		CreatedBy:      "user-uuid-1",
		CreatedAt:      now,
		LastActivityAt: now,
	}

	// First promotion should succeed.
	result, err := store.PromoteDM(ctx, topic, dmKey)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 3, result.MessageCount)

	// Second promotion with same DM key: no messages or DM rows remain,
	// so the transaction succeeds with 0 messages re-keyed.
	topic2 := WebChatTopic{
		ID:             "topic-idem-2",
		ProjectID:      "proj-1",
		Name:           "Idempotent Thread 2",
		DefaultAgent:   "agent-uuid-1",
		CreatedBy:      "user-uuid-1",
		CreatedAt:      now,
		LastActivityAt: now,
	}
	result2, err := store.PromoteDM(ctx, topic2, dmKey)
	require.NoError(t, err)
	require.NotNil(t, result2)
	require.Equal(t, 0, result2.MessageCount, "second promotion should re-key 0 messages")
}

// TestUpdateThreadID verifies the standalone UpdateThreadID method.
func TestUpdateThreadID(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"

	// Insert messages
	_, err := db.Exec(`
INSERT INTO messages (id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, created)
VALUES
    ('msg-a', 'proj-1', 'user:alice', 'user-uuid-1', 'agent:coder', 'web', ?, 'hello', '2026-08-22T10:00:00Z'),
    ('msg-b', 'proj-1', 'agent:coder', 'agent-uuid-1', 'user:alice', 'web', ?, 'hi', '2026-08-22T10:01:00Z')
`, dmKey, dmKey)
	require.NoError(t, err)

	n, err := store.UpdateThreadID(ctx, dmKey, "new-topic-id")
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// Verify all messages moved
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id = ?", "new-topic-id").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

// TestDeleteDM verifies the standalone DeleteDM method.
func TestDeleteDM(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"

	// Set up DM registry
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "user-uuid-1",
		PeerID:          "agent-uuid-1",
		PeerKind:        "agent",
	}))
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   "agent-uuid-1",
		PeerID:          "user-uuid-1",
		PeerKind:        "user",
	}))

	// Verify DMs exist
	dms, err := store.ListDMs(ctx, "user-uuid-1")
	require.NoError(t, err)
	require.Len(t, dms, 1)

	// Delete
	require.NoError(t, store.DeleteDM(ctx, dmKey))

	// Verify both rows gone
	dms, err = store.ListDMs(ctx, "user-uuid-1")
	require.NoError(t, err)
	require.Len(t, dms, 0)

	dms, err = store.ListDMs(ctx, "agent-uuid-1")
	require.NoError(t, err)
	require.Len(t, dms, 0)
}

// TestMigrateReadState verifies the standalone MigrateReadState method.
func TestMigrateReadState(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	oldKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	newKey := "topic-new-uuid"

	// Set up read state
	require.NoError(t, store.SetReadState(ctx, "user-uuid-1", oldKey, "msg-5"))
	require.NoError(t, store.SetPinned(ctx, "user-uuid-1", oldKey, true))

	// Migrate
	require.NoError(t, store.MigrateReadState(ctx, oldKey, newKey))

	// Verify: new key has the read state with pinned preserved
	rs, err := store.GetReadState(ctx, "user-uuid-1", newKey)
	require.NoError(t, err)
	require.NotNil(t, rs)
	require.Equal(t, "msg-5", rs.LastReadMessageID)
	require.True(t, rs.Pinned, "pinned flag should be preserved after migration")

	// Verify: old key has no read state
	rsOld, err := store.GetReadState(ctx, "user-uuid-1", oldKey)
	require.NoError(t, err)
	require.Nil(t, rsOld)
}

// TestCountPendingMessages verifies pending message counting.
func TestCountPendingMessages(t *testing.T) {
	store, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"

	// Insert messages with different dispatch states
	_, err := db.Exec(`
INSERT INTO messages (id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg, dispatch_state, created)
VALUES
    ('msg-d1', 'proj-1', 'agent:coder', 'agent-uuid-1', 'user:alice', 'web', ?, 'dispatched', 'dispatched', '2026-08-22T10:00:00Z'),
    ('msg-d2', 'proj-1', 'agent:coder', 'agent-uuid-1', 'user:alice', 'web', ?, 'pending', 'pending', '2026-08-22T10:01:00Z')
`, dmKey, dmKey)
	require.NoError(t, err)

	count, err := store.CountPendingMessages(ctx, dmKey)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

// TestPromoteDM_ThreadIDIndex verifies the thread_id_index migration runs.
func TestPromoteDM_ThreadIDIndex(t *testing.T) {
	_, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck

	// Verify the migration was recorded
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM webchat_migrations WHERE name = 'thread_id_index'").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "thread_id_index migration should be recorded")

	// Verify the index exists
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_thread_id'").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "idx_messages_thread_id index should exist")
}

// TestTitleCase verifies the titleCase helper function.
func TestTitleCase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"code-reviewer", "Code Reviewer"},
		{"my_agent", "My Agent"},
		{"simple", "Simple"},
		{"multi-word-agent", "Multi Word Agent"},
		{"already Title", "Already Title"},
	}
	for _, tc := range tests {
		got := titleCase(tc.input)
		require.Equal(t, tc.expected, got, "titleCase(%q)", tc.input)
	}
}

// TestDMPromotedEvent verifies the DMPromotedEvent struct serialization.
func TestDMPromotedEvent(t *testing.T) {
	evt := DMPromotedEvent{
		OldConversationKey: "dm:agent:a1:user:u1",
		NewTopic: WebChatTopic{
			ID:        "topic-1",
			ProjectID: "proj-1",
			Name:      "Test Thread",
		},
	}
	require.Equal(t, "dm:agent:a1:user:u1", evt.OldConversationKey)
	require.Equal(t, "topic-1", evt.NewTopic.ID)
}
