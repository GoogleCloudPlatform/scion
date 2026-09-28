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

package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webchatTopicDDL is the schema for the webchat_topic table, used in tests.
// Matches the production DDL in pkg/hub/webchannel_store_postgres.go:74.
const webchatTopicDDL = `
CREATE TABLE IF NOT EXISTS webchat_topic (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    name          TEXT NOT NULL,
    is_general    BOOLEAN NOT NULL DEFAULT FALSE,
    default_agent TEXT,
    conversation_id TEXT,
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_message_id TEXT,
    last_activity_at TEXT,
    deleted_at    TEXT
)
`

// testDB extracts the raw *sql.DB from the store for creating the
// webchat_topic table and inserting test data. Uses the same interface
// assertion pattern as cmd/server_foreground.go:596.
func testDB(t *testing.T, s store.Store) *sql.DB {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not provide DB()")
	db := dbProvider.DB()
	require.NotNil(t, db, "DB() returned nil")
	return db
}

// setupWebchatTopicTable creates the webchat_topic table in the test DB.
func setupWebchatTopicTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(webchatTopicDDL)
	require.NoError(t, err, "failed to create webchat_topic table")
}

// insertWebchatTopic inserts a test webchat_topic row.
func insertWebchatTopic(t *testing.T, db *sql.DB, topicID, projectID, conversationID, name string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO webchat_topic (id, project_id, name, conversation_id, created_by, created_at)
		 VALUES ($1, $2, $3, $4, 'test', $5)`,
		topicID, projectID, name, conversationID, time.Now().Format(time.RFC3339),
	)
	require.NoError(t, err, "failed to insert webchat_topic")
}

// createGroupConversation creates a kind='group' conversation with the given
// external_ref (use "" for broken rows). Returns the created conversation.
func createGroupConversation(t *testing.T, ctx context.Context, s store.Store, projectID, externalRef string) store.Conversation {
	t.Helper()
	conv := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "group",
		Surface:     "native",
		ExternalRef: externalRef,
		DriftState:  "active",
		ProjectID:   &projectID,
	}
	err := s.CreateConversation(ctx, conv)
	require.NoError(t, err, "failed to create group conversation")
	return *conv
}

// ---------------------------------------------------------------------------
// Fixture (a): clean repairable row — one linked topic, no collision
// ---------------------------------------------------------------------------

func TestGroupRefRepair_CleanRepairable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()
	conv := createGroupConversation(t, ctx, s, projectID, "")
	topicID := uuid.NewString()
	insertWebchatTopic(t, db, topicID, projectID, conv.ID, "test-topic")

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Verify the conversation was repaired.
	got, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)

	expectedRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)
	assert.Equal(t, expectedRef, got.ExternalRef,
		"conversation external_ref should be repaired to thread:<projectID>:<topicID>")

	// Verify pass completed and marker was written.
	assert.Contains(t, logOutput, "pass completed")
	assert.Contains(t, logOutput, "repaired=1")

	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done, "migration marker should be written after successful pass")
}

// ---------------------------------------------------------------------------
// Fixture (b): zero linked topics — skip and log
// ---------------------------------------------------------------------------

func TestGroupRefRepair_NoLinkedTopic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()
	conv := createGroupConversation(t, ctx, s, projectID, "")
	// No topic inserted for this conversation.

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Verify the conversation was NOT changed.
	got, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.ExternalRef,
		"conversation with no linked topic should remain empty")

	// Verify the skip was logged.
	assert.Contains(t, logOutput, "no linked webchat_topic")
	assert.Contains(t, logOutput, conv.ID)

	// Marker should still be written — row-level refusal does NOT block it.
	assert.Contains(t, logOutput, "skipped_no_topic=1")
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written despite row-level skip")
}

// ---------------------------------------------------------------------------
// Fixture (c): two linked topics — skip and log
// ---------------------------------------------------------------------------

func TestGroupRefRepair_MultipleLinkedTopics(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()
	conv := createGroupConversation(t, ctx, s, projectID, "")
	insertWebchatTopic(t, db, uuid.NewString(), projectID, conv.ID, "topic-1")
	insertWebchatTopic(t, db, uuid.NewString(), projectID, conv.ID, "topic-2")

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Verify the conversation was NOT changed.
	got, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.ExternalRef,
		"conversation with multiple linked topics should remain empty")

	// Verify the skip was logged.
	assert.Contains(t, logOutput, "multiple linked webchat_topics")
	assert.Contains(t, logOutput, conv.ID)

	// Marker should still be written.
	assert.Contains(t, logOutput, "skipped_multi_topic=1")
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written despite row-level skip")
}

// ---------------------------------------------------------------------------
// Fixture (d): unique constraint collision — skip and log
// ---------------------------------------------------------------------------

func TestGroupRefRepair_UniqueConstraintCollision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()
	topicID := uuid.NewString()

	// Compute the ref that the migration will try to set.
	expectedRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
	require.NoError(t, err)

	// Create the "shadow" conversation that already holds this ref.
	// This is the conversation that blocks the UPDATE via the partial unique
	// index on (surface, external_ref) where external_ref <> ''.
	createGroupConversation(t, ctx, s, projectID, expectedRef)

	// Create the broken conversation (empty external_ref) and link a topic.
	conv := createGroupConversation(t, ctx, s, projectID, "")
	insertWebchatTopic(t, db, topicID, projectID, conv.ID, "collision-topic")

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Verify the broken conversation was NOT changed (collision prevented it).
	got, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.ExternalRef,
		"conversation with unique constraint collision should remain empty")

	// Verify the collision was logged.
	assert.Contains(t, logOutput, "unique constraint collision")
	assert.Contains(t, logOutput, conv.ID)

	// Marker should still be written — collision is a row-level refusal.
	assert.Contains(t, logOutput, "skipped_collision=1")
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written despite collision skip")
}

// ---------------------------------------------------------------------------
// No-op test: zero broken rows — marker still written
// ---------------------------------------------------------------------------

func TestGroupRefRepair_NoBrokenRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	// Create a group conversation with a valid external_ref — no broken rows.
	projectID := uuid.NewString()
	createGroupConversation(t, ctx, s, projectID, "thread:"+projectID+":"+uuid.NewString())

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Verify the migration logged "0 found" or equivalent.
	assert.Contains(t, logOutput, "count=0")

	// Marker should be written — an empty pass is a completed pass.
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written for empty pass")

	// No WARN should have been logged.
	assert.NotContains(t, logOutput, "level=WARN")
}

// ---------------------------------------------------------------------------
// Idempotence: second boot is a no-op
// ---------------------------------------------------------------------------

func TestGroupRefRepair_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()
	conv := createGroupConversation(t, ctx, s, projectID, "")
	topicID := uuid.NewString()
	insertWebchatTopic(t, db, topicID, projectID, conv.ID, "test-topic")

	// First boot: repairs the row.
	runGroupRefRepair(ctx, s)

	// Second boot: should skip entirely.
	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "already complete, skipping")
}

// ---------------------------------------------------------------------------
// kind='direct' rows are never touched
// ---------------------------------------------------------------------------

func TestGroupRefRepair_DirectConversationsUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	// Create a direct conversation with an empty external_ref.
	// Direct conversations require a non-empty external_ref in
	// CreateConversation, so we first create with a ref, then
	// verify the migration query never matches it.
	// Actually, CreateConversation for kind='direct' rejects empty
	// external_ref. So we create a direct with a real ref, plus
	// a broken group conversation, and verify only the group is touched.
	projectID := uuid.NewString()
	topicID := uuid.NewString()

	// Create a broken group conversation (the migration target).
	groupConv := createGroupConversation(t, ctx, s, projectID, "")
	insertWebchatTopic(t, db, topicID, projectID, groupConv.ID, "test-topic")

	// Create a direct conversation (not a migration target).
	directConv := &store.Conversation{
		ID:          uuid.NewString(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "dm:" + uuid.NewString() + ":" + uuid.NewString(),
		DriftState:  "active",
	}
	err := s.CreateConversation(ctx, directConv)
	require.NoError(t, err)

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Only 1 broken row should have been found (the group one).
	assert.Contains(t, logOutput, "count=1")
	assert.Contains(t, logOutput, "repaired=1")

	// Direct conversation unchanged.
	gotDirect, err := s.GetConversation(ctx, directConv.ID)
	require.NoError(t, err)
	assert.Equal(t, directConv.ExternalRef, gotDirect.ExternalRef)
}

// ---------------------------------------------------------------------------
// gteam-shaped test: 38 clean + 3 colliding
// ---------------------------------------------------------------------------

func TestGroupRefRepair_GteamShape(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()

	// Create 38 clean repairable conversations.
	for i := 0; i < 38; i++ {
		conv := createGroupConversation(t, ctx, s, projectID, "")
		topicID := uuid.NewString()
		insertWebchatTopic(t, db, topicID, projectID, conv.ID, fmt.Sprintf("topic-%d", i))
	}

	// Create 3 colliding conversations: each has a shadow twin holding
	// the same computed ref.
	for i := 0; i < 3; i++ {
		topicID := uuid.NewString()
		expectedRef, err := messaging.ThreadConversationExternalRef(projectID, topicID)
		require.NoError(t, err)

		// Shadow conversation already holds the ref.
		createGroupConversation(t, ctx, s, projectID, expectedRef)

		// Broken conversation linked to the same topic.
		conv := createGroupConversation(t, ctx, s, projectID, "")
		insertWebchatTopic(t, db, topicID, projectID, conv.ID, fmt.Sprintf("collision-topic-%d", i))
	}

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// Verify counts match gteam shape.
	assert.Contains(t, logOutput, "count=41",
		"should find 41 broken conversations (38 clean + 3 colliding)")
	assert.Contains(t, logOutput, "repaired=38")
	assert.Contains(t, logOutput, "skipped_collision=3")

	// Marker should be written.
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written after gteam-shaped pass")

	// Verify all 38 clean ones now have non-empty external_ref.
	result, err := s.ListConversations(ctx, store.ConversationFilter{
		Kind: "group",
	}, store.ListOptions{Limit: 500})
	require.NoError(t, err)

	emptyRefCount := 0
	for _, c := range result.Items {
		if c.ExternalRef == "" {
			emptyRefCount++
		}
	}
	// 3 colliding + 0 clean remaining = 3 still empty.
	assert.Equal(t, 3, emptyRefCount,
		"only the 3 colliding conversations should still have empty external_ref")
}

// ---------------------------------------------------------------------------
// Mixed outcomes test: all three row-level outcomes in one pass
// ---------------------------------------------------------------------------

func TestGroupRefRepair_MixedOutcomes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db := testDB(t, s)
	setupWebchatTopicTable(t, db)

	projectID := uuid.NewString()

	// (a) Clean repairable.
	cleanConv := createGroupConversation(t, ctx, s, projectID, "")
	cleanTopicID := uuid.NewString()
	insertWebchatTopic(t, db, cleanTopicID, projectID, cleanConv.ID, "clean-topic")

	// (b) No linked topic.
	noTopicConv := createGroupConversation(t, ctx, s, projectID, "")

	// (c) Two linked topics.
	multiConv := createGroupConversation(t, ctx, s, projectID, "")
	insertWebchatTopic(t, db, uuid.NewString(), projectID, multiConv.ID, "multi-topic-1")
	insertWebchatTopic(t, db, uuid.NewString(), projectID, multiConv.ID, "multi-topic-2")

	// (d) Unique constraint collision.
	collisionTopicID := uuid.NewString()
	collisionRef, err := messaging.ThreadConversationExternalRef(projectID, collisionTopicID)
	require.NoError(t, err)
	createGroupConversation(t, ctx, s, projectID, collisionRef)
	collisionConv := createGroupConversation(t, ctx, s, projectID, "")
	insertWebchatTopic(t, db, collisionTopicID, projectID, collisionConv.ID, "collision-topic")

	buf, restore := captureSlog(t)
	defer restore()

	runGroupRefRepair(ctx, s)

	logOutput := buf.String()

	// All 4 broken rows found.
	assert.Contains(t, logOutput, "count=4")
	// 1 repaired, 1 no-topic, 1 multi-topic, 1 collision.
	assert.Contains(t, logOutput, "repaired=1")
	assert.Contains(t, logOutput, "skipped_no_topic=1")
	assert.Contains(t, logOutput, "skipped_multi_topic=1")
	assert.Contains(t, logOutput, "skipped_collision=1")

	// Verify clean one was repaired.
	got, err := s.GetConversation(ctx, cleanConv.ID)
	require.NoError(t, err)
	expectedRef, err := messaging.ThreadConversationExternalRef(projectID, cleanTopicID)
	require.NoError(t, err)
	assert.Equal(t, expectedRef, got.ExternalRef)

	// Verify skip cases remain empty.
	gotNoTopic, err := s.GetConversation(ctx, noTopicConv.ID)
	require.NoError(t, err)
	assert.Equal(t, "", gotNoTopic.ExternalRef)

	gotMulti, err := s.GetConversation(ctx, multiConv.ID)
	require.NoError(t, err)
	assert.Equal(t, "", gotMulti.ExternalRef)

	gotCollision, err := s.GetConversation(ctx, collisionConv.ID)
	require.NoError(t, err)
	assert.Equal(t, "", gotCollision.ExternalRef)

	// Marker written.
	done, err := IsMigrationComplete(ctx, s, MigrationGroupRefRepair)
	require.NoError(t, err)
	assert.True(t, done)
}
