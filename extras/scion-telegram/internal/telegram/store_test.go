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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

// --- GroupLink CRUD ---

func TestStore_GroupLink_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	link := &GroupLink{
		ChatID:           -100123,
		ChatTitle:        "Test Group",
		ProjectID:        "proj-1",
		ProjectSlug:      "my-project",
		DefaultAgent:     "coder",
		LinkedBy:         "456",
		LinkedAt:         time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		Active:           true,
		ShowAgentToAgent: false,
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
	assert.Equal(t, "456", got.LinkedBy)
	assert.True(t, got.Active)
	assert.False(t, got.ShowAgentToAgent)
	assert.Equal(t, 2026, got.LinkedAt.Year())
}

func TestStore_GroupLink_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetGroupLink(ctx, -999999)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_GroupLink_Upsert(t *testing.T) {
	store := newTestStore(t)
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

func TestStore_GroupLink_GetByProject(t *testing.T) {
	store := newTestStore(t)
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

	links, err = store.GetGroupLinksForProject(ctx, "proj-nonexistent")
	require.NoError(t, err)
	assert.Len(t, links, 0)
}

func TestStore_GroupLink_GetAll(t *testing.T) {
	store := newTestStore(t)
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

func TestStore_GroupLink_Delete(t *testing.T) {
	store := newTestStore(t)
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

	// Delete non-existent is not an error.
	require.NoError(t, store.DeleteGroupLink(ctx, -999))
}

// --- ConversationContext ---

func TestStore_ConversationContext_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
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
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, "coder", got.AgentSlug)
	assert.Equal(t, int64(-100), got.LastChatID)
	assert.Equal(t, 2026, got.LastMessageAt.Year())
}

func TestStore_ConversationContext_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetConversationContext(ctx, "unknown", "proj-1", "coder")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_ConversationContext_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cc := &ConversationContext{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		LastChatID:     -100,
		LastMessageAt:  time.Now().UTC(),
	}
	require.NoError(t, store.SaveConversationContext(ctx, cc))

	cc.LastChatID = -200
	cc.LastMessageAt = time.Now().UTC().Add(time.Hour)
	require.NoError(t, store.SaveConversationContext(ctx, cc))

	got, err := store.GetConversationContext(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(-200), got.LastChatID)
}

func TestStore_ConversationContext_MultipleKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for _, slug := range []string{"coder", "reviewer"} {
		require.NoError(t, store.SaveConversationContext(ctx, &ConversationContext{
			TelegramUserID: "456",
			ProjectID:      "proj-1",
			AgentSlug:      slug,
			LastChatID:     -100,
			LastMessageAt:  now,
		}))
	}

	got1, err := store.GetConversationContext(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, got1)

	got2, err := store.GetConversationContext(ctx, "456", "proj-1", "reviewer")
	require.NoError(t, err)
	require.NotNil(t, got2)

	assert.Equal(t, "coder", got1.AgentSlug)
	assert.Equal(t, "reviewer", got2.AgentSlug)
}

func TestStore_ConversationContext_GetLatest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Save two contexts with different timestamps — "reviewer" is more recent.
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

func TestStore_ConversationContext_GetLatest_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetLatestConversationContext(ctx, "999", "proj-unknown")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// --- ProjectAgents ---

func TestStore_ProjectAgents_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pa := &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder", Activity: "executing"}, {Slug: "reviewer", Activity: "idle"}, {Slug: "tester"}},
		RefreshedAt: time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.SaveProjectAgents(ctx, pa))

	got, err := store.GetProjectAgents(ctx, "proj-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, []AgentInfo{{Slug: "coder", Activity: "executing"}, {Slug: "reviewer", Activity: "idle"}, {Slug: "tester"}}, got.Agents)
	assert.Equal(t, 2026, got.RefreshedAt.Year())
}

func TestStore_ProjectAgents_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetProjectAgents(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_ProjectAgents_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pa := &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}},
		RefreshedAt: time.Now().UTC(),
	}
	require.NoError(t, store.SaveProjectAgents(ctx, pa))

	pa.Agents = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	pa.RefreshedAt = time.Now().UTC().Add(time.Hour)
	require.NoError(t, store.SaveProjectAgents(ctx, pa))

	got, err := store.GetProjectAgents(ctx, "proj-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}, got.Agents)
}

func TestStore_ProjectAgents_EmptySlice(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pa := &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{},
		RefreshedAt: time.Now().UTC(),
	}
	require.NoError(t, store.SaveProjectAgents(ctx, pa))

	got, err := store.GetProjectAgents(ctx, "proj-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []AgentInfo{}, got.Agents)
}

// --- TelegramUserMapping ---

func TestStore_UserMapping_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
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
	assert.Equal(t, "456", got.TelegramUserID)
	assert.Equal(t, "alice", got.TelegramUsername)
	assert.Equal(t, "user-123", got.ScionUserID)
	assert.Equal(t, "alice@example.com", got.ScionEmail)
}

func TestStore_UserMapping_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetUserMapping(ctx, "unknown")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_UserMapping_GetByEmail(t *testing.T) {
	store := newTestStore(t)
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

func TestStore_UserMapping_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID:   "456",
		TelegramUsername: "alice",
		ScionEmail:       "alice@old.com",
		LinkedAt:         time.Now().UTC(),
	}))

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID:   "456",
		TelegramUsername: "alice_new",
		ScionEmail:       "alice@new.com",
		LinkedAt:         time.Now().UTC(),
	}))

	got, err := store.GetUserMapping(ctx, "456")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "alice_new", got.TelegramUsername)
	assert.Equal(t, "alice@new.com", got.ScionEmail)
}

func TestStore_UserMapping_Delete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	require.NoError(t, store.DeleteUserMapping(ctx, "456"))

	got, err := store.GetUserMapping(ctx, "456")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Delete non-existent is not an error.
	require.NoError(t, store.DeleteUserMapping(ctx, "nonexistent"))
}

func TestStore_UserMapping_GetAll(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mappings, err := store.GetAllUserMappings(ctx)
	require.NoError(t, err)
	assert.Len(t, mappings, 0)

	for _, id := range []string{"100", "200", "300"} {
		require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
			TelegramUserID: id,
			ScionEmail:     id + "@test.com",
			LinkedAt:       time.Now().UTC(),
		}))
	}

	mappings, err = store.GetAllUserMappings(ctx)
	require.NoError(t, err)
	assert.Len(t, mappings, 3)
}

// --- PendingAskUser ---

func TestStore_PendingAskUser_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
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
	assert.Equal(t, int64(-100), got.ChatID)
	assert.Equal(t, "coder", got.AgentSlug)
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, []string{"Yes", "No", "Maybe"}, got.Choices)
	assert.False(t, got.Responded)
}

func TestStore_PendingAskUser_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetPendingAskUser(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_PendingAskUser_MarkResponded(t *testing.T) {
	store := newTestStore(t)
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

func TestStore_PendingAskUser_CleanExpired(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Save one expired and one active.
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

func TestStore_PendingAskUser_EmptyChoices(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "req-empty",
		MessageID: 1,
		ChatID:    -100,
		Choices:   []string{},
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))

	got, err := store.GetPendingAskUser(ctx, "req-empty")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{}, got.Choices)
}

// --- CallbackLookup ---

func TestStore_CallbackLookup_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	lookup := &CallbackLookup{
		ShortID:   "abc123",
		FullData:  "setup:proj:long-project-id-that-exceeds-64-bytes",
		ExpiresAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, store.SaveCallbackLookup(ctx, lookup))

	got, err := store.GetCallbackLookup(ctx, "abc123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "abc123", got.ShortID)
	assert.Equal(t, "setup:proj:long-project-id-that-exceeds-64-bytes", got.FullData)
}

func TestStore_CallbackLookup_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetCallbackLookup(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_CallbackLookup_Upsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveCallbackLookup(ctx, &CallbackLookup{
		ShortID:   "abc123",
		FullData:  "old-data",
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))

	require.NoError(t, store.SaveCallbackLookup(ctx, &CallbackLookup{
		ShortID:   "abc123",
		FullData:  "new-data",
		ExpiresAt: time.Now().Add(2 * time.Hour).UTC(),
	}))

	got, err := store.GetCallbackLookup(ctx, "abc123")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "new-data", got.FullData)
}

func TestStore_CallbackLookup_CleanExpired(t *testing.T) {
	store := newTestStore(t)
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

// --- NotificationPref ---

func TestStore_NotificationPref_SaveAndGet(t *testing.T) {
	store := newTestStore(t)
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
	assert.Equal(t, "456", got.TelegramUserID)
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, "coder", got.AgentSlug)
	assert.True(t, got.Enabled)
}

func TestStore_NotificationPref_GetNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetNotificationPref(ctx, "456", "proj-1", "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_NotificationPref_Toggle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pref := &NotificationPref{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		Enabled:        true,
	}
	require.NoError(t, store.SaveNotificationPref(ctx, pref))

	pref.Enabled = false
	require.NoError(t, store.SaveNotificationPref(ctx, pref))

	got, err := store.GetNotificationPref(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.Enabled)
}

func TestStore_NotificationPref_GetAll(t *testing.T) {
	store := newTestStore(t)
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

	prefMap := make(map[string]bool)
	for _, p := range prefs {
		prefMap[p.AgentSlug] = p.Enabled
	}
	assert.True(t, prefMap["coder"])
	assert.False(t, prefMap["reviewer"])
	assert.True(t, prefMap["tester"])
}

func TestStore_NotificationPref_GetAllEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	prefs, err := store.GetNotificationPrefs(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Len(t, prefs, 0)
}

func TestStore_GroupLink_NotifyInGroup(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	link := &GroupLink{
		ChatID:        -100,
		ProjectID:     "proj-1",
		LinkedAt:      time.Now().UTC(),
		Active:        true,
		NotifyInGroup: true,
	}
	require.NoError(t, store.SaveGroupLink(ctx, link))

	got, err := store.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.NotifyInGroup)

	link.NotifyInGroup = false
	require.NoError(t, store.SaveGroupLink(ctx, link))

	got, err = store.GetGroupLink(ctx, -100)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.NotifyInGroup)
}

// --- Store lifecycle ---

func TestStore_OpenInvalidPath(t *testing.T) {
	_, err := NewSQLiteStore("/nonexistent/dir/test.db")
	assert.Error(t, err)
}
