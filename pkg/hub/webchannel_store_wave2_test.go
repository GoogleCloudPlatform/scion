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

// newTestWebChatStoreV2 creates a WebChatStore backed by an in-memory SQLite DB
// for testing wave-2 features. The caller should close the returned *sql.DB.
func newTestWebChatStoreV2(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init())

	return store, db
}

// --- Init idempotency ---

func TestWave2_Init_Idempotent(t *testing.T) {
	_, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	// Init again should not fail (CREATE TABLE IF NOT EXISTS + CREATE INDEX IF NOT EXISTS).
	store2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store2.Init())

	// Verify all wave-2 tables exist.
	for _, table := range []string{"webchat_topic", "webchat_read_state", "webchat_user_prefs", "webchat_dm", "webchat_migrations"} {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		require.NoError(t, err, "table %s should exist", table)
	}
}

// --- Topic CRUD ---

func TestWave2_CreateTopic(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	topic := WebChatTopic{
		ID:        "topic-1",
		ProjectID: "proj-1",
		Name:      "design-review",
		CreatedBy: "user-1",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, store.CreateTopic(ctx, topic))

	got, err := store.GetTopic(ctx, "topic-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "topic-1", got.ID)
	require.Equal(t, "proj-1", got.ProjectID)
	require.Equal(t, "design-review", got.Name)
	require.False(t, got.IsGeneral)
	require.Empty(t, got.DefaultAgent)
	require.Equal(t, "user-1", got.CreatedBy)
	require.Nil(t, got.DeletedAt)
}

func TestWave2_CreateTopic_WithDefaultAgent(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	topic := WebChatTopic{
		ID:           "topic-2",
		ProjectID:    "proj-1",
		Name:         "dev-chat",
		DefaultAgent: "agent-uuid-1",
		CreatedBy:    "user-1",
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, store.CreateTopic(ctx, topic))

	got, err := store.GetTopic(ctx, "topic-2")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "agent-uuid-1", got.DefaultAgent)
}

func TestWave2_GetTopic_NotFound(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	got, err := store.GetTopic(context.Background(), "nonexistent")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestWave2_CreateTopic_DuplicateName(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC()

	// Create a topic.
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t1", ProjectID: "proj-1", Name: "design", CreatedBy: "u1", CreatedAt: now,
	}))

	// Duplicate name in same project should fail (unique index).
	err := store.CreateTopic(ctx, WebChatTopic{
		ID: "t2", ProjectID: "proj-1", Name: "design", CreatedBy: "u1", CreatedAt: now,
	})
	require.Error(t, err, "duplicate topic name in same project should be rejected")

	// Same name in a different project should succeed.
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t3", ProjectID: "proj-2", Name: "design", CreatedBy: "u1", CreatedAt: now,
	}))

	// Soft-delete the original, then reuse the name — should succeed.
	require.NoError(t, store.DeleteTopic(ctx, "t1"))
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t4", ProjectID: "proj-1", Name: "design", CreatedBy: "u1", CreatedAt: now,
	}))

	// Verify the new topic is the one returned.
	got, err := store.GetTopic(ctx, "t4")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "design", got.Name)
}

func TestWave2_ListTopics(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Create topics for two different projects.
	topics := []WebChatTopic{
		{ID: "t1", ProjectID: "proj-1", Name: "general", IsGeneral: true, CreatedBy: "u1", CreatedAt: now},
		{ID: "t2", ProjectID: "proj-1", Name: "design", CreatedBy: "u1", CreatedAt: now},
		{ID: "t3", ProjectID: "proj-2", Name: "general", IsGeneral: true, CreatedBy: "u1", CreatedAt: now},
	}
	for _, topic := range topics {
		require.NoError(t, store.CreateTopic(ctx, topic))
	}

	// Touch activity on t2 to make it sort first.
	require.NoError(t, store.TouchTopicActivity(ctx, "t2", "msg-1"))

	result, err := store.ListTopics(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, result, 2)

	// proj-2 topics should not appear.
	for _, r := range result {
		require.Equal(t, "proj-1", r.ProjectID)
	}
}

func TestWave2_UpdateTopic_Rename(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t1", ProjectID: "proj-1", Name: "old-name", CreatedBy: "u1",
		CreatedAt: time.Now().UTC(),
	}))

	newName := "new-name"
	require.NoError(t, store.UpdateTopic(ctx, "t1", TopicUpdate{Name: &newName}))

	got, err := store.GetTopic(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, "new-name", got.Name)
}

func TestWave2_UpdateTopic_SetAndClearDefaultAgent(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t1", ProjectID: "proj-1", Name: "dev", CreatedBy: "u1",
		CreatedAt: time.Now().UTC(),
	}))

	// Set default agent.
	agent := "agent-uuid"
	require.NoError(t, store.UpdateTopic(ctx, "t1", TopicUpdate{DefaultAgent: &agent}))

	got, err := store.GetTopic(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, "agent-uuid", got.DefaultAgent)

	// Clear default agent.
	empty := ""
	require.NoError(t, store.UpdateTopic(ctx, "t1", TopicUpdate{DefaultAgent: &empty}))

	got, err = store.GetTopic(ctx, "t1")
	require.NoError(t, err)
	require.Empty(t, got.DefaultAgent)
}

func TestWave2_UpdateTopic_NoChanges(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t1", ProjectID: "proj-1", Name: "dev", CreatedBy: "u1",
		CreatedAt: time.Now().UTC(),
	}))

	// Empty update should be a no-op.
	require.NoError(t, store.UpdateTopic(ctx, "t1", TopicUpdate{}))
}

// --- Soft delete ---

func TestWave2_DeleteTopic_SoftDelete(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t1", ProjectID: "proj-1", Name: "temp-thread", CreatedBy: "u1",
		CreatedAt: time.Now().UTC(),
	}))

	require.NoError(t, store.DeleteTopic(ctx, "t1"))

	// GetTopic should return nil for soft-deleted topics.
	got, err := store.GetTopic(ctx, "t1")
	require.NoError(t, err)
	require.Nil(t, got)

	// The row should still exist in the database.
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM webchat_topic WHERE id = 't1'").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestWave2_DeleteTopic_SoftDeleteExcludedFromList(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "general", ProjectID: "proj-1", Name: "general", IsGeneral: true,
		CreatedBy: "u1", CreatedAt: now,
	}))
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t2", ProjectID: "proj-1", Name: "deletable",
		CreatedBy: "u1", CreatedAt: now,
	}))

	require.NoError(t, store.DeleteTopic(ctx, "t2"))

	topics, err := store.ListTopics(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, topics, 1)
	require.Equal(t, "general", topics[0].ID)
}

func TestWave2_DeleteTopic_RejectsGeneral(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "general", ProjectID: "proj-1", Name: "general", IsGeneral: true,
		CreatedBy: "u1", CreatedAt: time.Now().UTC(),
	}))

	err := store.DeleteTopic(ctx, "general")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot delete #general")
}

// --- EnsureGeneralTopic ---

func TestWave2_EnsureGeneralTopic(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	id1, created, err := store.EnsureGeneralTopic(ctx, "proj-1", "user-1")
	require.NoError(t, err)
	require.NotEmpty(t, id1)
	require.True(t, created, "first call should report created=true")

	// Verify the topic exists.
	got, err := store.GetTopic(ctx, id1)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.IsGeneral)
	require.Equal(t, "general", got.Name)
	require.Equal(t, "proj-1", got.ProjectID)
}

func TestWave2_EnsureGeneralTopic_Idempotent(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	id1, created1, err := store.EnsureGeneralTopic(ctx, "proj-1", "user-1")
	require.NoError(t, err)
	require.True(t, created1, "first call should report created=true")

	// Second call should return the same ID and created=false.
	id2, created2, err := store.EnsureGeneralTopic(ctx, "proj-1", "user-2")
	require.NoError(t, err)
	require.Equal(t, id1, id2)
	require.False(t, created2, "second call should report created=false (topic already exists)")

	// Only one row should exist.
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM webchat_topic WHERE project_id = 'proj-1' AND is_general = 1").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestWave2_ListTopics_LazyCreatesGeneral(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	// ListTopics on a project with no topics should lazily create #general.
	topics, err := store.ListTopics(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, topics, 1)
	require.True(t, topics[0].IsGeneral)
	require.Equal(t, "general", topics[0].Name)
}

// --- TouchTopicActivity ---

func TestWave2_TouchTopicActivity(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.CreateTopic(ctx, WebChatTopic{
		ID: "t1", ProjectID: "proj-1", Name: "dev", CreatedBy: "u1",
		CreatedAt: time.Now().UTC(),
	}))

	require.NoError(t, store.TouchTopicActivity(ctx, "t1", "msg-42"))

	got, err := store.GetTopic(ctx, "t1")
	require.NoError(t, err)
	require.Equal(t, "msg-42", got.LastMessageID)
	require.False(t, got.LastActivityAt.IsZero())
}

// --- Read state ---

func TestWave2_ReadState_GetAndSet(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()

	// No read state initially.
	rs, err := store.GetReadState(ctx, "user-1", "topic-uuid-1")
	require.NoError(t, err)
	require.Nil(t, rs)

	// Set read state.
	require.NoError(t, store.SetReadState(ctx, "user-1", "topic-uuid-1", "msg-10"))

	rs, err = store.GetReadState(ctx, "user-1", "topic-uuid-1")
	require.NoError(t, err)
	require.NotNil(t, rs)
	require.Equal(t, "user-1", rs.UserID)
	require.Equal(t, "topic-uuid-1", rs.ConversationKey)
	require.Equal(t, "msg-10", rs.LastReadMessageID)
	require.False(t, rs.LastReadAt.IsZero())
}

func TestWave2_ReadState_Upsert(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.SetReadState(ctx, "user-1", "conv-1", "msg-1"))
	require.NoError(t, store.SetReadState(ctx, "user-1", "conv-1", "msg-5"))

	rs, err := store.GetReadState(ctx, "user-1", "conv-1")
	require.NoError(t, err)
	require.Equal(t, "msg-5", rs.LastReadMessageID)
}

func TestWave2_ReadState_BatchGet(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.SetReadState(ctx, "user-1", "conv-1", "msg-1"))
	require.NoError(t, store.SetReadState(ctx, "user-1", "conv-2", "msg-2"))
	require.NoError(t, store.SetReadState(ctx, "user-1", "conv-3", "msg-3"))

	states, err := store.GetReadStates(ctx, "user-1", []string{"conv-1", "conv-3", "conv-missing"})
	require.NoError(t, err)
	require.Len(t, states, 2) // conv-missing not returned.
}

func TestWave2_ReadState_EmptyBatchGet(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	states, err := store.GetReadStates(context.Background(), "user-1", nil)
	require.NoError(t, err)
	require.Nil(t, states)
}

func TestWave2_ReadState_SetPinned(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.SetPinned(ctx, "user-1", "conv-1", true))

	rs, err := store.GetReadState(ctx, "user-1", "conv-1")
	require.NoError(t, err)
	require.NotNil(t, rs)
	require.True(t, rs.Pinned)

	// Unpin.
	require.NoError(t, store.SetPinned(ctx, "user-1", "conv-1", false))
	rs, err = store.GetReadState(ctx, "user-1", "conv-1")
	require.NoError(t, err)
	require.False(t, rs.Pinned)
}

func TestWave2_ReadState_SetMuted(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.SetMuted(ctx, "user-1", "conv-1", true))

	rs, err := store.GetReadState(ctx, "user-1", "conv-1")
	require.NoError(t, err)
	require.NotNil(t, rs)
	require.True(t, rs.Muted)
}

// --- User prefs ---

func TestWave2_UserPrefs_Defaults(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	prefs, err := store.GetUserPrefs(context.Background(), "user-1")
	require.NoError(t, err)
	require.NotNil(t, prefs)
	require.Equal(t, "user-1", prefs.UserID)
	require.Equal(t, "activity", prefs.SpaceSortMode)
	require.Equal(t, "activity", prefs.ThreadSortMode)
	require.Empty(t, prefs.SpaceOrder)
}

func TestWave2_UserPrefs_SetAndGet(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.SetUserPrefs(ctx, "user-1", WebChatUserPrefs{
		UserID:         "user-1",
		SpaceSortMode:  "custom",
		SpaceOrder:     `["proj-1","proj-2"]`,
		ThreadSortMode: "alpha",
	}))

	prefs, err := store.GetUserPrefs(ctx, "user-1")
	require.NoError(t, err)
	require.Equal(t, "custom", prefs.SpaceSortMode)
	require.Equal(t, `["proj-1","proj-2"]`, prefs.SpaceOrder)
	require.Equal(t, "alpha", prefs.ThreadSortMode)
}

func TestWave2_UserPrefs_Upsert(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, store.SetUserPrefs(ctx, "user-1", WebChatUserPrefs{
		UserID: "user-1", SpaceSortMode: "alpha", ThreadSortMode: "alpha",
	}))
	require.NoError(t, store.SetUserPrefs(ctx, "user-1", WebChatUserPrefs{
		UserID: "user-1", SpaceSortMode: "activity", ThreadSortMode: "activity",
	}))

	prefs, err := store.GetUserPrefs(ctx, "user-1")
	require.NoError(t, err)
	require.Equal(t, "activity", prefs.SpaceSortMode)
}

// --- DM ---

func TestWave2_DM_UpsertAndList(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Upsert both sides of a DM.
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:agent:a1:user:u1",
		ParticipantID:   "u1",
		PeerID:          "a1",
		PeerKind:        "agent",
		LastMessageID:   "msg-1",
		LastActivityAt:  now,
	}))
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:agent:a1:user:u1",
		ParticipantID:   "a1",
		PeerID:          "u1",
		PeerKind:        "user",
		LastMessageID:   "msg-1",
		LastActivityAt:  now,
	}))

	// List from user side.
	dms, err := store.ListDMs(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, "dm:agent:a1:user:u1", dms[0].ConversationKey)
	require.Equal(t, "a1", dms[0].PeerID)
	require.Equal(t, "agent", dms[0].PeerKind)

	// List from agent side.
	dms, err = store.ListDMs(ctx, "a1")
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, "u1", dms[0].PeerID)
}

func TestWave2_DM_TouchActivity(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:agent:a1:user:u1",
		ParticipantID:   "u1",
		PeerID:          "a1",
		PeerKind:        "agent",
		LastMessageID:   "msg-1",
		LastActivityAt:  now,
	}))
	require.NoError(t, store.UpsertDM(ctx, WebChatDM{
		ConversationKey: "dm:agent:a1:user:u1",
		ParticipantID:   "a1",
		PeerID:          "u1",
		PeerKind:        "user",
		LastMessageID:   "msg-1",
		LastActivityAt:  now,
	}))

	// Touch activity — should update both rows.
	require.NoError(t, store.TouchDMActivity(ctx, "dm:agent:a1:user:u1", "msg-5"))

	dms, err := store.ListDMs(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, "msg-5", dms[0].LastMessageID)

	dms, err = store.ListDMs(ctx, "a1")
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, "msg-5", dms[0].LastMessageID)
}

func TestWave2_DM_ListEmpty(t *testing.T) {
	store, db := newTestWebChatStoreV2(t)
	defer db.Close() //nolint:errcheck

	dms, err := store.ListDMs(context.Background(), "nobody")
	require.NoError(t, err)
	require.Nil(t, dms)
}

// --- Migration: thread_id backfill ---

func TestWave2_Migration_ThreadIDBackfill(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// Create a minimal messages table.
	_, err = db.Exec(`
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT,
    thread_id TEXT,
    msg TEXT NOT NULL DEFAULT ''
)
`)
	require.NoError(t, err)

	// Insert test messages in both directions.
	_, err = db.Exec(`
INSERT INTO messages (id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg) VALUES
    ('m1', 'user:alice', 'alice-uuid', 'agent:coder', 'coder-uuid', 'web', 'agent:coder-uuid', 'hello'),
    ('m2', 'agent:coder', 'coder-uuid', 'user:alice', 'alice-uuid', 'web', 'agent:coder-uuid', 'hi back'),
    ('m3', 'user:bob', 'bob-uuid', 'agent:reviewer', 'reviewer-uuid', 'web', 'agent:reviewer-uuid', 'review this'),
    ('m4', 'user:alice', 'alice-uuid', 'agent:coder', 'coder-uuid', 'discord', 'agent:coder-uuid', 'discord msg'),
    ('m5', 'user:alice', 'alice-uuid', 'agent:coder', 'coder-uuid', 'web', 'dm:agent:coder-uuid:user:alice-uuid', 'already migrated')
`)
	require.NoError(t, err)

	// Initialize the store (triggers migration).
	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init())

	// Verify: m1, m2, m3 should be backfilled.
	var threadID string
	err = db.QueryRow("SELECT thread_id FROM messages WHERE id = 'm1'").Scan(&threadID)
	require.NoError(t, err)
	require.Equal(t, "dm:agent:coder-uuid:user:alice-uuid", threadID)

	err = db.QueryRow("SELECT thread_id FROM messages WHERE id = 'm2'").Scan(&threadID)
	require.NoError(t, err)
	require.Equal(t, "dm:agent:coder-uuid:user:alice-uuid", threadID)

	err = db.QueryRow("SELECT thread_id FROM messages WHERE id = 'm3'").Scan(&threadID)
	require.NoError(t, err)
	require.Equal(t, "dm:agent:reviewer-uuid:user:bob-uuid", threadID)

	// m4 should be unchanged (discord channel, not web).
	err = db.QueryRow("SELECT thread_id FROM messages WHERE id = 'm4'").Scan(&threadID)
	require.NoError(t, err)
	require.Equal(t, "agent:coder-uuid", threadID)

	// m5 should be unchanged (already migrated, doesn't match 'agent:%' pattern).
	err = db.QueryRow("SELECT thread_id FROM messages WHERE id = 'm5'").Scan(&threadID)
	require.NoError(t, err)
	require.Equal(t, "dm:agent:coder-uuid:user:alice-uuid", threadID)
}

func TestWave2_Migration_ThreadIDBackfill_Batching(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// Create a minimal messages table.
	_, err = db.Exec(`
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT,
    thread_id TEXT,
    msg TEXT NOT NULL DEFAULT ''
)
`)
	require.NoError(t, err)

	// Insert 5 messages, then use batch size 2 to verify batching works.
	for i := 0; i < 5; i++ {
		_, err = db.Exec(`INSERT INTO messages (id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg)
			VALUES (?, 'user:alice', 'alice-uuid', 'agent:coder', 'coder-uuid', 'web', 'agent:coder-uuid', 'msg')`,
			"m-"+string(rune('a'+i)))
		require.NoError(t, err)
	}

	// Create tables first (needed for migration marker).
	storeImpl := &sqliteWebChatStore{db: db}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS webchat_migrations (
    name TEXT PRIMARY KEY,
    completed_at TEXT
)
`)
	require.NoError(t, err)

	// Run migration with small batch size.
	require.NoError(t, storeImpl.migrateThreadIDs(2))

	// All 5 should be migrated.
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id LIKE 'agent:%'").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	var migrated int
	err = db.QueryRow("SELECT COUNT(*) FROM messages WHERE thread_id LIKE 'dm:%'").Scan(&migrated)
	require.NoError(t, err)
	require.Equal(t, 5, migrated)
}

func TestWave2_Migration_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// Create messages table.
	_, err = db.Exec(`
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT,
    thread_id TEXT,
    msg TEXT NOT NULL DEFAULT ''
)
`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO messages (id, sender, sender_id, recipient, recipient_id, channel, thread_id, msg)
		VALUES ('m1', 'user:alice', 'alice-uuid', 'agent:coder', 'coder-uuid', 'web', 'agent:coder-uuid', 'hello')`)
	require.NoError(t, err)

	// First init.
	store1 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store1.Init())

	// Second init should be a no-op (migration already marked complete).
	store2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store2.Init())

	// Should still have the correct thread_id.
	var threadID string
	err = db.QueryRow("SELECT thread_id FROM messages WHERE id = 'm1'").Scan(&threadID)
	require.NoError(t, err)
	require.Equal(t, "dm:agent:coder-uuid:user:alice-uuid", threadID)
}

// --- Wave-1 seeding ---

func TestWave2_SeedFromWave1(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	// Manually create wave-1 tables and seed them.
	_, err = db.Exec(`
CREATE TABLE webchat_thread (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_message_id TEXT,
    last_activity_at TEXT,
    last_read_at TEXT,
    PRIMARY KEY (user_id, project_id, agent_id)
);
CREATE TABLE webchat_thread_prefs (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    visibility_mode TEXT DEFAULT 'conversation',
    show_state_changes INTEGER DEFAULT 0,
    show_agent_to_agent INTEGER DEFAULT 0,
    muted INTEGER DEFAULT 0,
    PRIMARY KEY (user_id, project_id, agent_id)
);
INSERT INTO webchat_thread VALUES ('user-1', 'proj-1', 'agent-1', 'msg-10', '2026-01-01T00:00:00Z', '2025-12-31T00:00:00Z');
INSERT INTO webchat_thread VALUES ('user-2', 'proj-1', 'agent-1', 'msg-20', '2026-01-02T00:00:00Z', NULL);
INSERT INTO webchat_thread_prefs VALUES ('user-1', 'proj-1', 'agent-1', 'conversation', 0, 0, 1);
`)
	require.NoError(t, err)

	// Init the store (creates wave-2 tables and runs seeding).
	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init())

	ctx := context.Background()

	// user-1 should have a DM entry.
	dms, err := store.ListDMs(ctx, "user-1")
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, "dm:agent:agent-1:user:user-1", dms[0].ConversationKey)
	require.Equal(t, "agent-1", dms[0].PeerID)
	require.Equal(t, "agent", dms[0].PeerKind)

	// agent-1 should have DM entries for both users.
	dms, err = store.ListDMs(ctx, "agent-1")
	require.NoError(t, err)
	require.Len(t, dms, 2)

	// user-1 should have read state with muted flag.
	rs, err := store.GetReadState(ctx, "user-1", "dm:agent:agent-1:user:user-1")
	require.NoError(t, err)
	require.NotNil(t, rs)
	require.True(t, rs.Muted)

	// user-2 should NOT have a read state (last_read_at was NULL).
	rs, err = store.GetReadState(ctx, "user-2", "dm:agent:agent-1:user:user-2")
	require.NoError(t, err)
	require.Nil(t, rs)
}

func TestWave2_SeedFromWave1_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck

	_, err = db.Exec(`
CREATE TABLE webchat_thread (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_message_id TEXT,
    last_activity_at TEXT,
    last_read_at TEXT,
    PRIMARY KEY (user_id, project_id, agent_id)
);
CREATE TABLE webchat_thread_prefs (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    visibility_mode TEXT DEFAULT 'conversation',
    show_state_changes INTEGER DEFAULT 0,
    show_agent_to_agent INTEGER DEFAULT 0,
    muted INTEGER DEFAULT 0,
    PRIMARY KEY (user_id, project_id, agent_id)
);
INSERT INTO webchat_thread VALUES ('user-1', 'proj-1', 'agent-1', 'msg-10', '2026-01-01T00:00:00Z', '2025-12-31T00:00:00Z');
`)
	require.NoError(t, err)

	store1 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store1.Init())

	// Second init should succeed (seed already complete).
	store2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store2.Init())

	// Verify DM still exists and there's only one.
	ctx := context.Background()
	dms, err := store2.ListDMs(ctx, "user-1")
	require.NoError(t, err)
	require.Len(t, dms, 1)
}

// --- Helper tests ---

func TestParseSQLiteTime(t *testing.T) {
	tests := []struct {
		name  string
		input string
		zero  bool
	}{
		{"empty", "", true},
		{"RFC3339Nano", "2026-01-01T00:00:00Z", false},
		{"RFC3339Nano with nanos", "2026-01-01T00:00:00.123456789Z", false},
		{"SQLite format with tz", "2026-01-01 00:00:00.000000000-07:00", false},
		{"SQLite format no tz", "2026-01-01 00:00:00.000000000", false},
		{"garbage", "not-a-time", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSQLiteTime(tt.input)
			require.Equal(t, tt.zero, result.IsZero(), "input: %q", tt.input)
		})
	}
}
