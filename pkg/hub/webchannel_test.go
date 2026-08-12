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
	"log/slog"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// newTestWebChatStore creates a WebChatStore backed by an in-memory SQLite DB
// for testing. The caller should close the returned *sql.DB when done.
func newTestWebChatStore(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init())

	return store, db
}

// --- WebChatStore tests ---

func TestWebChatStore_Init_CreatesTables(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck
	_ = store

	// Verify tables exist by querying them.
	for _, table := range []string{"webchat_thread", "webchat_conversation_context", "webchat_thread_prefs"} {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		require.NoError(t, err, "table %s should exist", table)
		require.Equal(t, 0, count)
	}
}

func TestWebChatStore_Init_Idempotent(t *testing.T) {
	_, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	// Init again should not fail.
	store2 := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store2.Init())
}

func TestWebChatStore_TouchThread_Insert(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	err := store.TouchThread(ctx, "user1", "proj1", "agent1", "msg-123", now)
	require.NoError(t, err)

	// Verify the row was inserted. SQLite stores timestamps as TEXT, so
	// scan into a string rather than time.Time.
	var userID, projectID, agentID, messageID, activityAt string
	err = db.QueryRow(`SELECT user_id, project_id, agent_id, last_message_id, last_activity_at
		FROM webchat_thread WHERE user_id = 'user1'`).Scan(&userID, &projectID, &agentID, &messageID, &activityAt)
	require.NoError(t, err)
	require.Equal(t, "user1", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "agent1", agentID)
	require.Equal(t, "msg-123", messageID)
	require.NotEmpty(t, activityAt)
}

func TestWebChatStore_TouchThread_Upsert(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	t1 := time.Now().Truncate(time.Second)
	t2 := t1.Add(5 * time.Minute)

	err := store.TouchThread(ctx, "user1", "proj1", "agent1", "msg-1", t1)
	require.NoError(t, err)

	// Upsert with a newer message.
	err = store.TouchThread(ctx, "user1", "proj1", "agent1", "msg-2", t2)
	require.NoError(t, err)

	// Verify only one row exists and it has the updated values.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM webchat_thread WHERE user_id = 'user1'`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	var messageID string
	err = db.QueryRow(`SELECT last_message_id FROM webchat_thread WHERE user_id = 'user1'`).Scan(&messageID)
	require.NoError(t, err)
	require.Equal(t, "msg-2", messageID)
}

func TestWebChatStore_RecordChannel_Insert(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	err := store.RecordChannel(ctx, "user1", "proj1", "agent1", "web", now)
	require.NoError(t, err)

	var channel string
	err = db.QueryRow(`SELECT last_channel FROM webchat_conversation_context
		WHERE user_id = 'user1' AND project_id = 'proj1' AND agent_id = 'agent1'`).Scan(&channel)
	require.NoError(t, err)
	require.Equal(t, "web", channel)
}

func TestWebChatStore_RecordChannel_Upsert(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	t1 := time.Now().Truncate(time.Second)
	t2 := t1.Add(5 * time.Minute)

	err := store.RecordChannel(ctx, "user1", "proj1", "agent1", "web", t1)
	require.NoError(t, err)

	err = store.RecordChannel(ctx, "user1", "proj1", "agent1", "discord", t2)
	require.NoError(t, err)

	var channel string
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM webchat_conversation_context
		WHERE user_id = 'user1' AND project_id = 'proj1' AND agent_id = 'agent1'`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	err = db.QueryRow(`SELECT last_channel FROM webchat_conversation_context
		WHERE user_id = 'user1' AND project_id = 'proj1' AND agent_id = 'agent1'`).Scan(&channel)
	require.NoError(t, err)
	require.Equal(t, "discord", channel)
}

// --- WebChannelBus tests ---

func TestWebChannelBus_Subscribe_DiscardsHandler(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)

	handlerCalled := false
	handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) {
		handlerCalled = true
	}

	sub, err := bus.Subscribe("scion.project.*.agent.*.messages", handler)
	require.NoError(t, err)
	require.NotNil(t, sub)

	// The handler should never be called — it is discarded.
	require.False(t, handlerCalled)

	// Unsubscribe should be a no-op.
	require.NoError(t, sub.Unsubscribe())
}

func TestWebChannelBus_Publish_UpdatesState(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)

	ctx := context.Background()
	// Simulate an agent→user message on a user topic.
	topic := "scion.project.proj1.user.user1.messages"
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		SenderID:  "agent-uuid-1",
		Recipient: "user:alice",
		Msg:       "Hello from the agent",
		Type:      messages.TypeInstruction,
		Channel:   "web",
	}

	err := bus.Publish(ctx, topic, msg)
	require.NoError(t, err)

	// Verify webchat_thread was updated.
	var userID, projectID, agentID string
	err = db.QueryRow(`SELECT user_id, project_id, agent_id FROM webchat_thread`).Scan(&userID, &projectID, &agentID)
	require.NoError(t, err)
	require.Equal(t, "user1", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "agent-uuid-1", agentID)

	// Verify webchat_conversation_context was updated.
	var channel string
	err = db.QueryRow(`SELECT last_channel FROM webchat_conversation_context
		WHERE user_id = 'user1' AND project_id = 'proj1' AND agent_id = 'agent-uuid-1'`).Scan(&channel)
	require.NoError(t, err)
	require.Equal(t, "web", channel)
}

func TestWebChannelBus_Publish_NilMessage(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)

	// Nil message should not panic and should return nil.
	err := bus.Publish(context.Background(), "scion.project.proj1.user.user1.messages", nil)
	require.NoError(t, err)
}

func TestWebChannelBus_Publish_ObserverOnlyIgnored(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)

	msg := &messages.StructuredMessage{
		Version:      messages.Version,
		Sender:       "agent:coder",
		SenderID:     "agent-uuid-1",
		Recipient:    "agent:reviewer",
		RecipientID:  "agent-uuid-2",
		Msg:          "agent-to-agent observation",
		Type:         messages.TypeInstruction,
		ObserverOnly: true,
	}
	err := bus.Publish(context.Background(), "scion.project.proj1.user.user1.messages", msg)
	require.NoError(t, err)

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM webchat_thread").Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count, "ObserverOnly messages must not create webchat_thread rows")
}

func TestWebChannelBus_Publish_BroadcastIgnored(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)

	// Broadcast topic should be silently ignored (not conversation-scoped).
	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "system",
		Msg:     "broadcast",
		Type:    messages.TypeSystem,
	}
	err := bus.Publish(context.Background(), "scion.project.proj1.broadcast", msg)
	require.NoError(t, err)

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM webchat_thread`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestWebChannelBus_Publish_AgentTopic(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)

	ctx := context.Background()
	// Simulate a user→agent message on an agent topic.
	topic := "scion.project.proj1.agent.coder.messages"
	msg := &messages.StructuredMessage{
		Version:     messages.Version,
		Sender:      "user:alice",
		SenderID:    "alice-uuid",
		Recipient:   "agent:coder",
		RecipientID: "coder-uuid",
		Msg:         "Hello from user",
		Type:        messages.TypeInstruction,
		Channel:     "web",
	}

	err := bus.Publish(ctx, topic, msg)
	require.NoError(t, err)

	// For an agent topic, the agent slug comes from the topic, the user from SenderID.
	var userID, agentID string
	err = db.QueryRow(`SELECT user_id, agent_id FROM webchat_thread`).Scan(&userID, &agentID)
	require.NoError(t, err)
	require.Equal(t, "alice-uuid", userID)
	require.Equal(t, "coder", agentID)
}

func TestWebChannelBus_Close(t *testing.T) {
	store, db := newTestWebChatStore(t)
	defer db.Close() //nolint:errcheck

	log := slog.Default()
	bus := NewWebChannelBus(log, store)
	require.NoError(t, bus.Close())
}

// --- identityFromTopic tests ---

func TestIdentityFromTopic_UserTopic(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender:   "agent:coder",
		SenderID: "agent-uuid",
	}
	userID, projectID, agentID, ok := identityFromTopic("scion.project.proj1.user.user1.messages", msg)
	require.True(t, ok)
	require.Equal(t, "user1", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "agent-uuid", agentID)
}

func TestIdentityFromTopic_UserTopic_SenderIDFallback(t *testing.T) {
	// When SenderID is empty, fall back to stripping "agent:" prefix from Sender.
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
	}
	userID, projectID, agentID, ok := identityFromTopic("scion.project.proj1.user.user1.messages", msg)
	require.True(t, ok)
	require.Equal(t, "user1", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "coder", agentID)
}

func TestIdentityFromTopic_AgentTopic(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender:   "user:alice",
		SenderID: "alice-uuid",
	}
	userID, projectID, agentID, ok := identityFromTopic("scion.project.proj1.agent.coder.messages", msg)
	require.True(t, ok)
	require.Equal(t, "alice-uuid", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "coder", agentID)
}

func TestIdentityFromTopic_Broadcast_ReturnsFalse(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "system",
	}
	_, _, _, ok := identityFromTopic("scion.project.proj1.broadcast", msg)
	require.False(t, ok)
}

func TestIdentityFromTopic_MalformedTopic_ReturnsFalse(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
	}
	_, _, _, ok := identityFromTopic("not.a.valid.topic", msg)
	require.False(t, ok)
}

func TestIdentityFromTopic_MissingUserID_ReturnsFalse(t *testing.T) {
	// Agent topic, but sender has no SenderID and is not a user.
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
	}
	_, _, _, ok := identityFromTopic("scion.project.proj1.agent.coder.messages", msg)
	require.False(t, ok)
}
