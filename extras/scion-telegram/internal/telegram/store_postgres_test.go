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

package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPostgresTestStore(t *testing.T) Store {
	t.Helper()
	dbURL := os.Getenv("TELEGRAM_TEST_POSTGRES_URL")
	if dbURL == "" {
		t.Skip("TELEGRAM_TEST_POSTGRES_URL not set, skipping Postgres store tests")
	}

	store, err := NewPostgresStore(dbURL)
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanPostgresTables(t, dbURL)
		store.Close()
	})

	cleanPostgresTables(t, dbURL)
	return store
}

func cleanPostgresTables(t *testing.T, dbURL string) {
	t.Helper()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open postgres for cleanup: %v", err)
	}
	defer db.Close()

	tables := []string{
		"telegram_group_links",
		"telegram_conversation_context",
		"telegram_project_agents",
		"telegram_user_mappings",
		"telegram_pending_ask_users",
		"telegram_callback_lookups",
		"telegram_notification_prefs",
		"telegram_topic_defaults",
	}
	for _, table := range tables {
		db.Exec(fmt.Sprintf("DELETE FROM %s", table))
	}
}

// --- Postgres GroupLink CRUD ---

func TestPostgres_GroupLink_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	link := &GroupLink{
		ChatID:             -100123,
		ChatTitle:          "Test Group",
		ProjectID:          "proj-1",
		ProjectSlug:        "my-project",
		DefaultAgent:       "coder",
		LinkedBy:           "456",
		LinkedAt:           time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		Active:             true,
		ShowAgentToAgent:   false,
		NotifyInGroup:      true,
		ShowAssistantReply: true,
	}

	require.NoError(t, store.SaveGroupLink(ctx, link))

	got, err := store.GetGroupLink(ctx, -100123)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, int64(-100123), got.ChatID)
	assert.Equal(t, "Test Group", got.ChatTitle)
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, "my-project", got.ProjectSlug)
	assert.Equal(t, "coder", got.DefaultAgent)
	assert.True(t, got.Active)
	assert.False(t, got.ShowAgentToAgent)
	assert.True(t, got.NotifyInGroup)
	assert.True(t, got.ShowAssistantReply)
}

func TestPostgres_GroupLink_GetNotFound(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	got, err := store.GetGroupLink(ctx, -999999)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPostgres_GroupLink_Upsert(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	link := &GroupLink{
		ChatID:       -100,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}
	require.NoError(t, store.SaveGroupLink(ctx, link))

	link.DefaultAgent = "reviewer"
	link.ChatTitle = "Updated Title"
	link.ShowAgentToAgent = true
	require.NoError(t, store.SaveGroupLink(ctx, link))

	got, err := store.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "reviewer", got.DefaultAgent)
	assert.Equal(t, "Updated Title", got.ChatTitle)
	assert.True(t, got.ShowAgentToAgent)
}

func TestPostgres_GroupLink_GetByProject(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	for i, chatID := range []int64{-100, -200, -300} {
		projID := "proj-1"
		if i == 2 {
			projID = "proj-2"
		}
		require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
			ChatID:    chatID,
			ProjectID: projID,
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))
	}

	links, err := store.GetGroupLinksForProject(ctx, "proj-1")
	require.NoError(t, err)
	assert.Len(t, links, 2)

	links, err = store.GetGroupLinksForProject(ctx, "proj-2")
	require.NoError(t, err)
	assert.Len(t, links, 1)
}

func TestPostgres_GroupLink_GetAll(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	links, err := store.GetAllGroupLinks(ctx)
	require.NoError(t, err)
	assert.Len(t, links, 0)

	for _, chatID := range []int64{-100, -200, -300} {
		require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
			ChatID:    chatID,
			ProjectID: "proj-1",
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))
	}

	links, err = store.GetAllGroupLinks(ctx)
	require.NoError(t, err)
	assert.Len(t, links, 3)
}

func TestPostgres_GroupLink_Delete(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))

	require.NoError(t, store.DeleteGroupLink(ctx, -100))

	got, err := store.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPostgres_GroupLink_MigrateGroupLink(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100,
		ChatTitle: "Old Group",
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))
	require.NoError(t, store.SetTopicDefault(ctx, -100, 42, "coder"))
	require.NoError(t, store.SetTopicDefault(ctx, -100, 43, "reviewer"))

	require.NoError(t, store.MigrateGroupLink(ctx, -100, -200))

	old, err := store.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	assert.Nil(t, old)

	newLink, err := store.GetGroupLink(ctx, -200)
	require.NoError(t, err)
	require.NotNil(t, newLink)
	assert.Equal(t, "Old Group", newLink.ChatTitle)
	assert.Equal(t, "proj-1", newLink.ProjectID)

	slug, err := store.GetTopicDefault(ctx, -200, 42)
	require.NoError(t, err)
	assert.Equal(t, "coder", slug)

	slug, err = store.GetTopicDefault(ctx, -200, 43)
	require.NoError(t, err)
	assert.Equal(t, "reviewer", slug)

	slug, err = store.GetTopicDefault(ctx, -100, 42)
	require.NoError(t, err)
	assert.Equal(t, "", slug)
}

// --- Postgres ConversationContext ---

func TestPostgres_ConversationContext_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	cc := &ConversationContext{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		LastChatID:     -100,
		LastMessageAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.SaveConversationContext(ctx, cc))

	got, err := store.GetConversationContext(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "456", got.TelegramUserID)
	assert.Equal(t, int64(-100), got.LastChatID)
}

func TestPostgres_ConversationContext_GetLatest(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveConversationContext(ctx, &ConversationContext{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		LastChatID:     -100,
		LastMessageAt:  time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC),
	}))
	require.NoError(t, store.SaveConversationContext(ctx, &ConversationContext{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "reviewer",
		LastChatID:     -100,
		LastMessageAt:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
	}))

	got, err := store.GetLatestConversationContext(ctx, "456", "proj-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "reviewer", got.AgentSlug)
}

// --- Postgres ProjectAgents ---

func TestPostgres_ProjectAgents_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	pa := &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder", Activity: "executing"}, {Slug: "reviewer"}},
		RefreshedAt: time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.SaveProjectAgents(ctx, pa))

	got, err := store.GetProjectAgents(ctx, "proj-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Len(t, got.Agents, 2)
	assert.Equal(t, "coder", got.Agents[0].Slug)
}

// --- Postgres UserMapping ---

func TestPostgres_UserMapping_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	mapping := &TelegramUserMapping{
		TelegramUserID:   "456",
		TelegramUsername: "alice",
		ScionUserID:      "user-123",
		ScionEmail:       "alice@example.com",
		LinkedAt:         time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.SaveUserMapping(ctx, mapping))

	got, err := store.GetUserMapping(ctx, "456")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "alice", got.TelegramUsername)
	assert.Equal(t, "alice@example.com", got.ScionEmail)
}

func TestPostgres_UserMapping_GetByEmail(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	got, err := store.GetUserMappingByEmail(ctx, "alice@example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "456", got.TelegramUserID)

	got, err = store.GetUserMappingByEmail(ctx, "nobody@example.com")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestPostgres_UserMapping_GetByUsername_CaseInsensitive(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID:   "456",
		TelegramUsername: "Alice",
		LinkedAt:         time.Now().UTC(),
	}))

	got, err := store.GetUserMappingByUsername(ctx, "alice")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "456", got.TelegramUserID)

	got, err = store.GetUserMappingByUsername(ctx, "ALICE")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestPostgres_UserMapping_GetAll(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"100", "200", "300"} {
		require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
			TelegramUserID: id,
			ScionEmail:     id + "@test.com",
			LinkedAt:       time.Now().UTC(),
		}))
	}

	mappings, err := store.GetAllUserMappings(ctx)
	require.NoError(t, err)
	assert.Len(t, mappings, 3)
}

func TestPostgres_UserMapping_Delete(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, store.DeleteUserMapping(ctx, "456"))

	got, err := store.GetUserMapping(ctx, "456")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// --- Postgres PendingAskUser ---

func TestPostgres_PendingAskUser_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	pending := &PendingAskUser{
		RequestID: "req-123",
		MessageID: 42,
		ChatID:    -100,
		AgentSlug: "coder",
		ProjectID: "proj-1",
		Choices:   []string{"Yes", "No", "Maybe"},
		ExpiresAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Responded: false,
	}
	require.NoError(t, store.SavePendingAskUser(ctx, pending))

	got, err := store.GetPendingAskUser(ctx, "req-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "req-123", got.RequestID)
	assert.Equal(t, int64(42), got.MessageID)
	assert.Equal(t, []string{"Yes", "No", "Maybe"}, got.Choices)
	assert.False(t, got.Responded)
}

func TestPostgres_PendingAskUser_MarkResponded(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "req-123",
		MessageID: 42,
		ChatID:    -100,
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))

	require.NoError(t, store.MarkPendingAskUserResponded(ctx, "req-123"))

	got, err := store.GetPendingAskUser(ctx, "req-123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Responded)
}

func TestPostgres_PendingAskUser_CleanExpired(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "expired",
		MessageID: 1,
		ChatID:    -100,
		ExpiresAt: time.Now().Add(-time.Hour).UTC(),
	}))
	require.NoError(t, store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "active",
		MessageID: 2,
		ChatID:    -100,
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))

	require.NoError(t, store.CleanExpiredPendingAskUsers(ctx))

	got, err := store.GetPendingAskUser(ctx, "expired")
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = store.GetPendingAskUser(ctx, "active")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- Postgres CallbackLookup ---

func TestPostgres_CallbackLookup_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	lookup := &CallbackLookup{
		ShortID:   "abc123",
		FullData:  "setup:proj:long-project-id",
		ExpiresAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.SaveCallbackLookup(ctx, lookup))

	got, err := store.GetCallbackLookup(ctx, "abc123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "abc123", got.ShortID)
	assert.Equal(t, "setup:proj:long-project-id", got.FullData)
}

func TestPostgres_CallbackLookup_CleanExpired(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveCallbackLookup(ctx, &CallbackLookup{
		ShortID:   "expired",
		FullData:  "old",
		ExpiresAt: time.Now().Add(-time.Hour).UTC(),
	}))
	require.NoError(t, store.SaveCallbackLookup(ctx, &CallbackLookup{
		ShortID:   "active",
		FullData:  "new",
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))

	require.NoError(t, store.CleanExpiredCallbackLookups(ctx))

	got, err := store.GetCallbackLookup(ctx, "expired")
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = store.GetCallbackLookup(ctx, "active")
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- Postgres NotificationPref ---

func TestPostgres_NotificationPref_SaveAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	pref := &NotificationPref{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		Enabled:        true,
	}
	require.NoError(t, store.SaveNotificationPref(ctx, pref))

	got, err := store.GetNotificationPref(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Enabled)
}

func TestPostgres_NotificationPref_Toggle(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveNotificationPref(ctx, &NotificationPref{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		Enabled:        true,
	}))

	require.NoError(t, store.SaveNotificationPref(ctx, &NotificationPref{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		Enabled:        false,
	}))

	got, err := store.GetNotificationPref(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.Enabled)
}

func TestPostgres_NotificationPref_GetAll(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	for _, slug := range []string{"coder", "reviewer", "tester"} {
		require.NoError(t, store.SaveNotificationPref(ctx, &NotificationPref{
			TelegramUserID: "456",
			ProjectID:      "proj-1",
			AgentSlug:      slug,
			Enabled:        slug != "reviewer",
		}))
	}

	prefs, err := store.GetNotificationPrefs(ctx, "456")
	require.NoError(t, err)
	assert.Len(t, prefs, 3)
}

// --- Postgres TopicDefault ---

func TestPostgres_TopicDefault_SetAndGet(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	slug, err := store.GetTopicDefault(ctx, -100, 42)
	require.NoError(t, err)
	assert.Equal(t, "", slug)

	require.NoError(t, store.SetTopicDefault(ctx, -100, 42, "coder"))
	slug, err = store.GetTopicDefault(ctx, -100, 42)
	require.NoError(t, err)
	assert.Equal(t, "coder", slug)

	slug, err = store.GetTopicDefault(ctx, -100, 99)
	require.NoError(t, err)
	assert.Equal(t, "", slug)
}

func TestPostgres_TopicDefault_Upsert(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SetTopicDefault(ctx, -100, 42, "coder"))
	require.NoError(t, store.SetTopicDefault(ctx, -100, 42, "reviewer"))

	slug, err := store.GetTopicDefault(ctx, -100, 42)
	require.NoError(t, err)
	assert.Equal(t, "reviewer", slug)
}

func TestPostgres_TopicDefault_Delete(t *testing.T) {
	store := newPostgresTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SetTopicDefault(ctx, -100, 42, "coder"))
	require.NoError(t, store.DeleteTopicDefault(ctx, -100, 42))

	slug, err := store.GetTopicDefault(ctx, -100, 42)
	require.NoError(t, err)
	assert.Equal(t, "", slug)
}

// --- Advisory Lock ---

func TestPostgres_AdvisoryLock_AcquireAndRelease(t *testing.T) {
	dbURL := os.Getenv("TELEGRAM_TEST_POSTGRES_URL")
	if dbURL == "" {
		t.Skip("TELEGRAM_TEST_POSTGRES_URL not set")
	}

	store, err := NewPostgresStore(dbURL)
	require.NoError(t, err)
	defer store.Close()

	locker, ok := store.(AdvisoryLocker)
	require.True(t, ok, "Postgres store must implement AdvisoryLocker")

	ctx := context.Background()
	acquired, handle, err := locker.TryAdvisoryLock(ctx, 0x5C10000A)
	require.NoError(t, err)
	assert.True(t, acquired)
	require.NotNil(t, handle)

	require.NoError(t, handle.Release())
}

func TestPostgres_AdvisoryLock_SecondInstanceBlocked(t *testing.T) {
	dbURL := os.Getenv("TELEGRAM_TEST_POSTGRES_URL")
	if dbURL == "" {
		t.Skip("TELEGRAM_TEST_POSTGRES_URL not set")
	}

	store1, err := NewPostgresStore(dbURL)
	require.NoError(t, err)
	defer store1.Close()

	store2, err := NewPostgresStore(dbURL)
	require.NoError(t, err)
	defer store2.Close()

	ctx := context.Background()

	locker1 := store1.(AdvisoryLocker)
	acquired1, handle1, err := locker1.TryAdvisoryLock(ctx, 0x5C10000A)
	require.NoError(t, err)
	assert.True(t, acquired1)
	defer handle1.Release()

	locker2 := store2.(AdvisoryLocker)
	acquired2, handle2, err := locker2.TryAdvisoryLock(ctx, 0x5C10000A)
	require.NoError(t, err)
	assert.False(t, acquired2, "second instance should not acquire the lock")
	assert.Nil(t, handle2)

	// Release first lock, second should now acquire.
	require.NoError(t, handle1.Release())

	acquired2, handle2, err = locker2.TryAdvisoryLock(ctx, 0x5C10000A)
	require.NoError(t, err)
	assert.True(t, acquired2, "second instance should acquire after first releases")
	require.NotNil(t, handle2)
	require.NoError(t, handle2.Release())
}
