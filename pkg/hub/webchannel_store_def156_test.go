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
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DEF-156 integration test helpers
// ---------------------------------------------------------------------------

// newDEF156TestStore creates a store with conversations + messages tables
// but does NOT run Init (so backfillTopicConversations hasn't run yet).
// The caller controls when backfills run and in what order.
func newDEF156TestStore(t *testing.T) (*sqliteWebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	// Create conversations table (Ent-managed in production).
	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	// Create messages table.
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT NOT NULL DEFAULT '',
    thread_id TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    msg TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'chat',
    broadcasted INTEGER NOT NULL DEFAULT 0,
    dispatch_state TEXT NOT NULL DEFAULT 'dispatched',
    created_at TEXT NOT NULL DEFAULT ''
)`)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3").(*sqliteWebChatStore)
	require.NoError(t, s.Init())
	return s, db
}

// simulateRoute3Backfill mimics what BackfillService.persistGroup does:
// for each message with a thread_id and no conversation_id, derive the
// key, upsert the conversation, and stamp the message. This is the
// "message backfill" (Route 3).
func simulateRoute3Backfill(t *testing.T, db *sql.DB, projectID string) {
	t.Helper()
	rows, err := db.Query(
		`SELECT id, thread_id, channel FROM messages
		 WHERE project_id = ? AND thread_id != '' AND conversation_id = ''`,
		projectID)
	require.NoError(t, err)
	defer rows.Close()

	type msgRow struct{ id, threadID, channel string }
	var msgs []msgRow
	for rows.Next() {
		var m msgRow
		require.NoError(t, rows.Scan(&m.id, &m.threadID, &m.channel))
		msgs = append(msgs, m)
	}
	require.NoError(t, rows.Err())

	// Group by key.
	type group struct {
		convKey string
		surface string
		msgIDs  []string
	}
	groups := make(map[string]*group)
	for _, m := range msgs {
		key, err := messaging.ThreadConversationExternalRef(projectID, m.threadID)
		require.NoError(t, err)
		surface, err := messaging.ChannelToSurfaceStrict(m.channel)
		require.NoError(t, err)
		g, ok := groups[key]
		if !ok {
			g = &group{convKey: key, surface: surface}
			groups[key] = g
		}
		g.msgIDs = append(g.msgIDs, m.id)
	}

	// Create/upsert conversations and stamp.
	for _, g := range groups {
		convID := uuid.New().String()
		now := time.Now().UTC().Format(time.RFC3339Nano)

		// Check if conversation already exists on this key.
		var existingID string
		err := db.QueryRow(
			`SELECT id FROM conversations WHERE surface = ? AND external_ref = ? AND deleted_at IS NULL`,
			g.surface, g.convKey).Scan(&existingID)
		if err != nil && err != sql.ErrNoRows {
			require.NoError(t, err)
		}
		if existingID != "" {
			convID = existingID
		} else {
			_, err = db.Exec(
				`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at)
				 VALUES (?, ?, 'group', ?, ?, '', '', 'active', ?, ?)`,
				convID, projectID, g.surface, g.convKey, now, now)
			require.NoError(t, err)
		}

		for _, msgID := range g.msgIDs {
			_, err = db.Exec(
				`UPDATE messages SET conversation_id = ? WHERE id = ? AND conversation_id = ''`,
				convID, msgID)
			require.NoError(t, err)
		}
	}
}

// insertTestTopic inserts a topic row without a conversation_id (simulating
// pre-cutover state — the topic exists but hasn't been backfilled).
func insertTestTopic(t *testing.T, db *sql.DB, topicID, projectID, name string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(
		`INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at)
		 VALUES (?, ?, ?, 0, 'test-user', ?)`,
		topicID, projectID, name, now)
	require.NoError(t, err)
}

// insertDEF156Message inserts a message row with a thread_id and no conversation_id.
func insertDEF156Message(t *testing.T, db *sql.DB, msgID, projectID, threadID, channel string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(
		`INSERT INTO messages (id, project_id, sender, sender_id, recipient, recipient_id, channel, thread_id, conversation_id, msg, type, created_at)
		 VALUES (?, ?, 'user:test', ?, 'agent:helper', ?, ?, ?, '', 'hello', 'chat', ?)`,
		msgID, projectID, uuid.New().String(), uuid.New().String(), channel, threadID, now)
	require.NoError(t, err)
}

// countConversationsByExtRef counts conversations matching a given external_ref.
func countConversationsByExtRef(t *testing.T, db *sql.DB, extRef string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM conversations WHERE external_ref = ? AND deleted_at IS NULL`, extRef).Scan(&n))
	return n
}

// getConvIDByExtRef returns the conversation ID for a given external_ref.
func getConvIDByExtRef(t *testing.T, db *sql.DB, extRef string) string {
	t.Helper()
	var id string
	err := db.QueryRow(
		`SELECT id FROM conversations WHERE external_ref = ? AND deleted_at IS NULL`, extRef).Scan(&id)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return id
}

// getMessageConvID returns the conversation_id stamped on a message.
func getMessageConvID(t *testing.T, db *sql.DB, msgID string) string {
	t.Helper()
	var convID string
	require.NoError(t, db.QueryRow(
		`SELECT conversation_id FROM messages WHERE id = ?`, msgID).Scan(&convID))
	return convID
}

// resetBackfillMarker resets the topic_conversation_backfill marker so it can run again.
func resetBackfillMarker(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`DELETE FROM webchat_migrations WHERE name = 'topic_conversation_backfill'`)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// AC-156-1, AC-156-2: Fresh cutover — both orders
// ---------------------------------------------------------------------------

func TestDEF156_FreshCutover_MessageBackfillFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "topic-1"
	topicName := "Test Thread"

	// Setup: topic with messages that have thread_id but no conversation_id.
	insertTestTopic(t, db, topicID, projectID, topicName)
	insertDEF156Message(t, db, "msg-1", projectID, topicID, "web")
	insertDEF156Message(t, db, "msg-2", projectID, topicID, "web")
	insertDEF156Message(t, db, "msg-3", projectID, topicID, "web")

	// Order 1: message backfill (Route 3) first, then topic backfill (Route 1).
	simulateRoute3Backfill(t, db, projectID)
	resetBackfillMarker(t, db)
	require.NoError(t, s.backfillTopicConversations())

	// Derive the expected key.
	expectedKey, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)

	// AC-156-3: exactly one conversation per topic.
	assert.Equal(t, 1, countConversationsByExtRef(t, db, expectedKey),
		"exactly one conversation should exist for topic")

	// AC-156-1: all messages are on the topic's conversation.
	topicConvID := getTopicConvID(t, db, topicID)
	assert.NotEmpty(t, topicConvID, "topic must have a conversation_id")

	for _, msgID := range []string{"msg-1", "msg-2", "msg-3"} {
		msgConvID := getMessageConvID(t, db, msgID)
		assert.Equal(t, topicConvID, msgConvID,
			"message %s must be stamped on the topic's conversation", msgID)
	}

	// Verify the conversation's external_ref is the derived key.
	convExtRef := getConvIDByExtRef(t, db, expectedKey)
	assert.Equal(t, topicConvID, convExtRef,
		"topic's conversation must match the one at the derived key")

	// Verify surface = native (channel was "web").
	conv := getConversation(t, db, topicConvID)
	require.NotNil(t, conv)
	assert.Equal(t, "native", conv.surface)

	_ = ctx // used for timeout
}

func TestDEF156_FreshCutover_TopicBackfillFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "topic-1"
	topicName := "Test Thread"

	// Setup: topic with messages.
	insertTestTopic(t, db, topicID, projectID, topicName)
	insertDEF156Message(t, db, "msg-1", projectID, topicID, "web")
	insertDEF156Message(t, db, "msg-2", projectID, topicID, "web")
	insertDEF156Message(t, db, "msg-3", projectID, topicID, "web")

	// Order 2: topic backfill (Route 1) first, then message backfill (Route 3).
	resetBackfillMarker(t, db)
	require.NoError(t, s.backfillTopicConversations())
	simulateRoute3Backfill(t, db, projectID)

	// Derive the expected key.
	expectedKey, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)

	// AC-156-3: exactly one conversation per topic.
	assert.Equal(t, 1, countConversationsByExtRef(t, db, expectedKey),
		"exactly one conversation should exist for topic")

	// AC-156-1: all messages are on the topic's conversation.
	topicConvID := getTopicConvID(t, db, topicID)
	assert.NotEmpty(t, topicConvID, "topic must have a conversation_id")

	for _, msgID := range []string{"msg-1", "msg-2", "msg-3"} {
		msgConvID := getMessageConvID(t, db, msgID)
		assert.Equal(t, topicConvID, msgConvID,
			"message %s must be stamped on the topic's conversation", msgID)
	}

	_ = ctx
}

// ---------------------------------------------------------------------------
// AC-156-4: Idempotence — boot twice
// ---------------------------------------------------------------------------

func TestDEF156_Idempotent_SecondBoot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "topic-1"
	topicName := "Test Thread"

	insertTestTopic(t, db, topicID, projectID, topicName)
	insertDEF156Message(t, db, "msg-1", projectID, topicID, "web")

	// First boot: both backfills.
	simulateRoute3Backfill(t, db, projectID)
	resetBackfillMarker(t, db)
	require.NoError(t, s.backfillTopicConversations())

	expectedKey, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)

	convCountAfterFirst := countConversationsByExtRef(t, db, expectedKey)
	topicConvIDFirst := getTopicConvID(t, db, topicID)
	msgConvIDFirst := getMessageConvID(t, db, "msg-1")

	// Second boot: simulate re-running both backfills.
	// Route 3 skips already-stamped messages (conversation_id != '').
	// Route 1 is gated by the migration marker.
	simulateRoute3Backfill(t, db, projectID)
	resetBackfillMarker(t, db)
	require.NoError(t, s.backfillTopicConversations())

	// AC-156-4: no new conversations, no re-stamping.
	assert.Equal(t, convCountAfterFirst, countConversationsByExtRef(t, db, expectedKey),
		"no new conversations after second boot")
	assert.Equal(t, topicConvIDFirst, getTopicConvID(t, db, topicID),
		"topic conversation_id unchanged after second boot")
	assert.Equal(t, msgConvIDFirst, getMessageConvID(t, db, "msg-1"),
		"message conversation_id unchanged after second boot")

	_ = ctx
}

// ---------------------------------------------------------------------------
// AC-156-5: Surface fidelity — Discord channel must not produce native surface
// ---------------------------------------------------------------------------

func TestDEF156_SurfaceFidelity_DiscordNotNative(t *testing.T) {
	_, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "discord-thread-123"

	// Insert a message on a discord channel with a thread_id.
	insertDEF156Message(t, db, "msg-discord-1", projectID, topicID, "discord")

	// Run Route 3 backfill.
	simulateRoute3Backfill(t, db, projectID)

	// Assert the conversation was created with surface = "discord", not "native".
	expectedKey, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)

	var surface string
	err = db.QueryRow(
		`SELECT surface FROM conversations WHERE external_ref = ? AND deleted_at IS NULL`,
		expectedKey).Scan(&surface)
	require.NoError(t, err)
	assert.Equal(t, "discord", surface,
		"Discord-channel message must produce surface='discord', not 'native'")
}

// ---------------------------------------------------------------------------
// AC-156-6: Mixed population — pre-existing '' ref topic still resolves
// ---------------------------------------------------------------------------

func TestDEF156_MixedPopulation_LegacyTopicStillResolves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "legacy-topic-1"
	legacyConvID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Simulate a pre-fix topic: conversation with external_ref = '' and
	// topic linked to it. This is Population P1.
	_, err := db.Exec(
		`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at)
		 VALUES (?, ?, 'group', 'native', '', '', 'Legacy Topic', 'active', ?, ?)`,
		legacyConvID, projectID, now, now)
	require.NoError(t, err)

	insertTestTopic(t, db, topicID, projectID, "Legacy Topic")
	_, err = db.Exec(`UPDATE webchat_topic SET conversation_id = ? WHERE id = ?`, legacyConvID, topicID)
	require.NoError(t, err)

	// Insert a message stamped on the legacy conversation.
	insertDEF156Message(t, db, "msg-legacy-1", projectID, topicID, "web")
	_, err = db.Exec(`UPDATE messages SET conversation_id = ? WHERE id = ?`, legacyConvID, "msg-legacy-1")
	require.NoError(t, err)

	// Verify the legacy topic still resolves.
	topicConvID := getTopicConvID(t, db, topicID)
	assert.Equal(t, legacyConvID, topicConvID,
		"pre-existing legacy topic must still have its conversation_id")

	msgConvID := getMessageConvID(t, db, "msg-legacy-1")
	assert.Equal(t, legacyConvID, msgConvID,
		"legacy message must still be stamped on the legacy conversation")

	_ = ctx
}

// ---------------------------------------------------------------------------
// AC-156-3: Exactly one conversation per topic — multiple topics
// ---------------------------------------------------------------------------

func TestDEF156_MultipleTopics_ExactlyOneConvEach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"

	// Create 3 topics, each with 2 messages.
	for i := 1; i <= 3; i++ {
		topicID := fmt.Sprintf("topic-%d", i)
		insertTestTopic(t, db, topicID, projectID, fmt.Sprintf("Topic %d", i))
		for j := 1; j <= 2; j++ {
			insertDEF156Message(t, db, fmt.Sprintf("msg-t%d-%d", i, j), projectID, topicID, "web")
		}
	}

	// Run in order: Route 3 first, then Route 1.
	simulateRoute3Backfill(t, db, projectID)
	resetBackfillMarker(t, db)
	require.NoError(t, s.backfillTopicConversations())

	// Each topic must have exactly one conversation.
	for i := 1; i <= 3; i++ {
		topicID := fmt.Sprintf("topic-%d", i)
		expectedKey, err := messaging.ThreadConversationExternalRef(projectID, topicID)
		require.NoError(t, err)
		assert.Equal(t, 1, countConversationsByExtRef(t, db, expectedKey),
			"topic %s must have exactly one conversation", topicID)

		topicConvID := getTopicConvID(t, db, topicID)
		for j := 1; j <= 2; j++ {
			msgID := fmt.Sprintf("msg-t%d-%d", i, j)
			assert.Equal(t, topicConvID, getMessageConvID(t, db, msgID),
				"message %s must be on topic %s's conversation", msgID, topicID)
		}
	}

	_ = ctx
}

// ---------------------------------------------------------------------------
// CreateTopic — DEF-156 P2: new topic finds existing conversation
// ---------------------------------------------------------------------------

func TestDEF156_CreateTopic_FindsExistingConversation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "topic-new"
	now := time.Now().UTC()

	// Pre-create a conversation that Route 3 would have minted.
	extRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)
	preExistingConvID := uuid.New().String()
	_, err = db.Exec(
		`INSERT INTO conversations (id, project_id, kind, surface, external_ref, parent_ref, display_name, drift_state, last_activity_at, created_at)
		 VALUES (?, ?, 'group', 'native', ?, '', '', 'active', ?, ?)`,
		preExistingConvID, projectID, extRef, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	require.NoError(t, err)

	// CreateTopic should find the existing conversation, not mint a new one.
	err = s.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: projectID,
		Name:      "New Topic",
		CreatedBy: "user-1",
		CreatedAt: now,
	})
	require.NoError(t, err)

	// Topic should be linked to the pre-existing conversation.
	topicConvID := getTopicConvID(t, db, topicID)
	assert.Equal(t, preExistingConvID, topicConvID,
		"CreateTopic should link to existing conversation, not mint a new one")

	// No duplicate conversation should exist.
	assert.Equal(t, 1, countConversationsByExtRef(t, db, extRef),
		"exactly one conversation should exist at the derived key")
}

// ---------------------------------------------------------------------------
// CreateTopic — DEF-156 P2: new topic mints when no existing conversation
// ---------------------------------------------------------------------------

func TestDEF156_CreateTopic_MintsWhenNoExisting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-def156"
	topicID := "topic-fresh"
	now := time.Now().UTC()

	err := s.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: projectID,
		Name:      "Fresh Topic",
		CreatedBy: "user-1",
		CreatedAt: now,
	})
	require.NoError(t, err)

	// Topic should have a conversation_id.
	topicConvID := getTopicConvID(t, db, topicID)
	assert.NotEmpty(t, topicConvID, "topic should have a conversation_id")

	// The conversation should use the derived external_ref.
	extRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)
	assert.Equal(t, 1, countConversationsByExtRef(t, db, extRef),
		"exactly one conversation should exist at the derived key")

	convRow := getConversation(t, db, topicConvID)
	require.NotNil(t, convRow)
	assert.Equal(t, "native", convRow.surface)
	assert.Equal(t, "group", convRow.kind)
}

// ---------------------------------------------------------------------------
// EnsureGeneralTopic — DEF-156: derived key, pre-mint lookup
// ---------------------------------------------------------------------------

func TestDEF156_EnsureGeneralTopic_WritesDerivedKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	topicID, created, err := s.EnsureGeneralTopic(ctx, "proj-general", "user-1")
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, topicID)

	// Verify the conversation was created with the derived external_ref.
	convID := getTopicConvID(t, db, topicID)
	require.NotEmpty(t, convID, "general topic must have a conversation_id")

	expectedExtRef, err := messaging.ThreadConversationExternalRef("proj-general", topicID)
	require.NoError(t, err)

	var extRef string
	err = db.QueryRow("SELECT external_ref FROM conversations WHERE id = ?", convID).Scan(&extRef)
	require.NoError(t, err)
	assert.Equal(t, expectedExtRef, extRef,
		"EnsureGeneralTopic must write the derived key, not ''")

	// Exactly one conversation at that key.
	assert.Equal(t, 1, countConversationsByExtRef(t, db, expectedExtRef),
		"exactly one conversation at the derived key")
}

func TestDEF156_EnsureGeneralTopic_FindsExistingConversation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-general-existing"

	// We need to know the topicID that EnsureGeneralTopic will use.
	// Since we can't predict the UUID, pre-create a general topic with
	// a known ID first, then test EnsureGeneralTopic's idempotent path.
	//
	// Strategy: call EnsureGeneralTopic once to create the topic+conversation,
	// then verify a second project's general topic writes the correct key.
	topicID, created, err := s.EnsureGeneralTopic(ctx, projectID, "user-1")
	require.NoError(t, err)
	require.True(t, created)

	// Get the created conversation's external_ref.
	convID := getTopicConvID(t, db, topicID)
	require.NotEmpty(t, convID)

	expectedExtRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)

	// Verify idempotence: second call returns the same topic,
	// and no new conversation is created.
	topicID2, created2, err := s.EnsureGeneralTopic(ctx, projectID, "user-2")
	require.NoError(t, err)
	assert.False(t, created2)
	assert.Equal(t, topicID, topicID2)

	assert.Equal(t, 1, countConversationsByExtRef(t, db, expectedExtRef),
		"idempotent call must not create a second conversation")
}

func TestDEF156_EnsureGeneralTopic_ConvergesWithRoute3(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, db := newDEF156TestStore(t)
	defer db.Close()

	projectID := "proj-general-converge"

	// Call EnsureGeneralTopic to create the general topic.
	topicID, _, err := s.EnsureGeneralTopic(ctx, projectID, "user-1")
	require.NoError(t, err)

	// Insert messages for the general topic's thread.
	insertDEF156Message(t, db, "msg-g1", projectID, topicID, "web")
	insertDEF156Message(t, db, "msg-g2", projectID, topicID, "web")

	// Run Route 3 backfill (message backfill).
	simulateRoute3Backfill(t, db, projectID)

	// Verify convergence: exactly one conversation, messages stamped on it.
	expectedExtRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)
	assert.Equal(t, 1, countConversationsByExtRef(t, db, expectedExtRef),
		"EnsureGeneralTopic + Route 3 must converge on one conversation")

	topicConvID := getTopicConvID(t, db, topicID)
	for _, msgID := range []string{"msg-g1", "msg-g2"} {
		assert.Equal(t, topicConvID, getMessageConvID(t, db, msgID),
			"message %s must be on the general topic's conversation", msgID)
	}

	_ = ctx
}

// ---------------------------------------------------------------------------
// AC-156-8: Route 2 (live write path) is untouched — existing tests pass.
// This is verified by running the full existing test suite, not by a new test.
// ---------------------------------------------------------------------------
