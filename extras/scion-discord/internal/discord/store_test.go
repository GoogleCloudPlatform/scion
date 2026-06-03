package discord

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

// --- ChannelLink CRUD ---

func TestChannelLinkCRUD(t *testing.T) {
	t.Run("CreateAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		link := &ChannelLink{
			ChannelID:          "111222333444555666",
			GuildID:            "999888777666555444",
			ProjectID:          "proj-1",
			ProjectSlug:        "my-project",
			DefaultAgent:       "coder",
			LinkedBy:           "456789012345678901",
			LinkedAt:           time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			Active:             true,
			ShowAgentToAgent:   false,
			ShowAssistantReply: true,
			ShowStateChanges:   true,
			NotifyInGroup:      true,
			ChatOnly:           false,
		}

		require.NoError(t, store.CreateChannelLink(ctx, link))

		got, err := store.GetChannelLink(ctx, "111222333444555666")
		require.NoError(t, err)
		require.NotNil(t, got)

		assert.Equal(t, "111222333444555666", got.ChannelID)
		assert.Equal(t, "999888777666555444", got.GuildID)
		assert.Equal(t, "proj-1", got.ProjectID)
		assert.Equal(t, "my-project", got.ProjectSlug)
		assert.Equal(t, "coder", got.DefaultAgent)
		assert.Equal(t, "456789012345678901", got.LinkedBy)
		assert.True(t, got.Active)
		assert.False(t, got.ShowAgentToAgent)
		assert.True(t, got.ShowAssistantReply)
		assert.True(t, got.ShowStateChanges)
		assert.True(t, got.NotifyInGroup)
		assert.False(t, got.ChatOnly)
		assert.Equal(t, 2026, got.LinkedAt.Year())
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetChannelLink(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("Upsert", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		link := &ChannelLink{
			ChannelID:    "111222333",
			GuildID:      "999888777",
			ProjectID:    "proj-1",
			DefaultAgent: "coder",
			LinkedAt:     time.Now().UTC(),
			Active:       true,
		}
		require.NoError(t, store.CreateChannelLink(ctx, link))

		link.DefaultAgent = "reviewer"
		link.ProjectSlug = "updated-slug"
		link.ShowAgentToAgent = true
		require.NoError(t, store.CreateChannelLink(ctx, link))

		got, err := store.GetChannelLink(ctx, "111222333")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.DefaultAgent)
		assert.Equal(t, "updated-slug", got.ProjectSlug)
		assert.True(t, got.ShowAgentToAgent)
	})

	t.Run("GetByProject", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		channels := []string{"100", "200", "300"}
		for i, chID := range channels {
			projID := "proj-1"
			if i == 2 {
				projID = "proj-2"
			}
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ChannelID: chID,
				GuildID:   "guild-1",
				ProjectID: projID,
				LinkedAt:  time.Now().UTC(),
				Active:    true,
			}))
		}

		links, err := store.GetChannelLinksForProject(ctx, "proj-1")
		require.NoError(t, err)
		assert.Len(t, links, 2)

		links, err = store.GetChannelLinksForProject(ctx, "proj-2")
		require.NoError(t, err)
		assert.Len(t, links, 1)

		links, err = store.GetChannelLinksForProject(ctx, "proj-nonexistent")
		require.NoError(t, err)
		assert.Len(t, links, 0)
	})

	t.Run("GetAll", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		links, err := store.GetAllChannelLinks(ctx)
		require.NoError(t, err)
		assert.Len(t, links, 0)

		for _, chID := range []string{"100", "200", "300"} {
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ChannelID: chID,
				GuildID:   "guild-1",
				ProjectID: "proj-1",
				LinkedAt:  time.Now().UTC(),
				Active:    true,
			}))
		}

		links, err = store.GetAllChannelLinks(ctx)
		require.NoError(t, err)
		assert.Len(t, links, 3)
	})

	t.Run("Update", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		link := &ChannelLink{
			ChannelID:          "111",
			GuildID:            "999",
			ProjectID:          "proj-1",
			DefaultAgent:       "coder",
			LinkedAt:           time.Now().UTC(),
			Active:             true,
			ShowAssistantReply: true,
			NotifyInGroup:      true,
		}
		require.NoError(t, store.CreateChannelLink(ctx, link))

		link.DefaultAgent = "reviewer"
		link.ChatOnly = true
		require.NoError(t, store.UpdateChannelLink(ctx, link))

		got, err := store.GetChannelLink(ctx, "111")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.DefaultAgent)
		assert.True(t, got.ChatOnly)
	})

	t.Run("DeactivateForGuild", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		for _, chID := range []string{"100", "200"} {
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ChannelID: chID,
				GuildID:   "guild-1",
				ProjectID: "proj-1",
				LinkedAt:  time.Now().UTC(),
				Active:    true,
			}))
		}
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID: "300",
			GuildID:   "guild-2",
			ProjectID: "proj-2",
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))

		require.NoError(t, store.DeactivateLinksForGuild(ctx, "guild-1"))

		got1, err := store.GetChannelLink(ctx, "100")
		require.NoError(t, err)
		require.NotNil(t, got1)
		assert.False(t, got1.Active)

		got2, err := store.GetChannelLink(ctx, "200")
		require.NoError(t, err)
		require.NotNil(t, got2)
		assert.False(t, got2.Active)

		// Channel in different guild should remain active.
		got3, err := store.GetChannelLink(ctx, "300")
		require.NoError(t, err)
		require.NotNil(t, got3)
		assert.True(t, got3.Active)
	})

	t.Run("Delete", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID: "100",
			GuildID:   "guild-1",
			ProjectID: "proj-1",
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))

		require.NoError(t, store.DeleteChannelLink(ctx, "100"))

		got, err := store.GetChannelLink(ctx, "100")
		require.NoError(t, err)
		assert.Nil(t, got)

		// Delete non-existent is not an error.
		require.NoError(t, store.DeleteChannelLink(ctx, "nonexistent"))
	})
}

// --- UserMapping CRUD ---

func TestUserMappingCRUD(t *testing.T) {
	t.Run("CreateAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		mapping := &DiscordUserMapping{
			DiscordUserID:  "456789012345678901",
			DiscordUsername: "alice",
			ScionUserID:    "user-123",
			ScionEmail:     "alice@example.com",
			LinkedAt:       time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		}
		require.NoError(t, store.CreateUserMapping(ctx, mapping))

		got, err := store.GetUserMapping(ctx, "456789012345678901")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "456789012345678901", got.DiscordUserID)
		assert.Equal(t, "alice", got.DiscordUsername)
		assert.Equal(t, "user-123", got.ScionUserID)
		assert.Equal(t, "alice@example.com", got.ScionEmail)
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetUserMapping(ctx, "unknown")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("GetByEmail", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
			DiscordUserID: "456",
			ScionEmail:    "alice@example.com",
			LinkedAt:      time.Now().UTC(),
		}))

		got, err := store.GetUserMappingByEmail(ctx, "alice@example.com")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "456", got.DiscordUserID)

		got, err = store.GetUserMappingByEmail(ctx, "nobody@example.com")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("GetByScionUserID", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
			DiscordUserID: "456",
			ScionUserID:   "user-123",
			LinkedAt:      time.Now().UTC(),
		}))

		got, err := store.GetUserMappingByScionUserID(ctx, "user-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "456", got.DiscordUserID)

		got, err = store.GetUserMappingByScionUserID(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("Upsert", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
			DiscordUserID:  "456",
			DiscordUsername: "alice",
			ScionEmail:     "alice@old.com",
			LinkedAt:       time.Now().UTC(),
		}))

		require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
			DiscordUserID:  "456",
			DiscordUsername: "alice_new",
			ScionEmail:     "alice@new.com",
			LinkedAt:       time.Now().UTC(),
		}))

		got, err := store.GetUserMapping(ctx, "456")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "alice_new", got.DiscordUsername)
		assert.Equal(t, "alice@new.com", got.ScionEmail)
	})

	t.Run("Delete", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateUserMapping(ctx, &DiscordUserMapping{
			DiscordUserID: "456",
			ScionEmail:    "alice@example.com",
			LinkedAt:      time.Now().UTC(),
		}))

		require.NoError(t, store.DeleteUserMapping(ctx, "456"))

		got, err := store.GetUserMapping(ctx, "456")
		require.NoError(t, err)
		assert.Nil(t, got)

		// Delete non-existent is not an error.
		require.NoError(t, store.DeleteUserMapping(ctx, "nonexistent"))
	})
}

// --- ConversationContext ---

func TestConversationContext(t *testing.T) {
	t.Run("SetAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		cc := &ConversationContext{
			DiscordUserID: "456",
			ProjectID:     "proj-1",
			AgentSlug:     "coder",
			LastChannelID: "111222333",
			LastMessageAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		}
		require.NoError(t, store.SetConversationContext(ctx, cc))

		got, err := store.GetConversationContext(ctx, "456", "proj-1", "coder")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "456", got.DiscordUserID)
		assert.Equal(t, "proj-1", got.ProjectID)
		assert.Equal(t, "coder", got.AgentSlug)
		assert.Equal(t, "111222333", got.LastChannelID)
		assert.Equal(t, 2026, got.LastMessageAt.Year())
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetConversationContext(ctx, "unknown", "proj-1", "coder")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("Upsert", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		cc := &ConversationContext{
			DiscordUserID: "456",
			ProjectID:     "proj-1",
			AgentSlug:     "coder",
			LastChannelID: "100",
			LastMessageAt: time.Now().UTC(),
		}
		require.NoError(t, store.SetConversationContext(ctx, cc))

		cc.LastChannelID = "200"
		cc.LastMessageAt = time.Now().UTC().Add(time.Hour)
		require.NoError(t, store.SetConversationContext(ctx, cc))

		got, err := store.GetConversationContext(ctx, "456", "proj-1", "coder")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "200", got.LastChannelID)
	})

	t.Run("MultipleKeys", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		now := time.Now().UTC()
		for _, slug := range []string{"coder", "reviewer"} {
			require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
				DiscordUserID: "456",
				ProjectID:     "proj-1",
				AgentSlug:     slug,
				LastChannelID: "100",
				LastMessageAt: now,
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
	})

	t.Run("GetLatest", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		// Save two contexts with different timestamps -- "reviewer" is more recent.
		require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
			DiscordUserID: "456",
			ProjectID:     "proj-1",
			AgentSlug:     "coder",
			LastChannelID: "100",
			LastMessageAt: time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC),
		}))
		require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
			DiscordUserID: "456",
			ProjectID:     "proj-1",
			AgentSlug:     "reviewer",
			LastChannelID: "100",
			LastMessageAt: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		}))

		got, err := store.GetLatestConversationContext(ctx, "456", "proj-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.AgentSlug)
	})

	t.Run("GetLatestNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetLatestConversationContext(ctx, "999", "proj-unknown")
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// --- ProjectAgents ---

func TestProjectAgents(t *testing.T) {
	t.Run("SetAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		pa := &ProjectAgents{
			ProjectID:   "proj-1",
			AgentSlugs:  []string{"coder", "reviewer", "tester"},
			RefreshedAt: time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC),
		}
		require.NoError(t, store.SetProjectAgents(ctx, pa))

		got, err := store.GetProjectAgents(ctx, "proj-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "proj-1", got.ProjectID)
		assert.Equal(t, []string{"coder", "reviewer", "tester"}, got.AgentSlugs)
		assert.Equal(t, 2026, got.RefreshedAt.Year())
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetProjectAgents(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("Upsert", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		pa := &ProjectAgents{
			ProjectID:   "proj-1",
			AgentSlugs:  []string{"coder"},
			RefreshedAt: time.Now().UTC(),
		}
		require.NoError(t, store.SetProjectAgents(ctx, pa))

		pa.AgentSlugs = []string{"coder", "reviewer"}
		pa.RefreshedAt = time.Now().UTC().Add(time.Hour)
		require.NoError(t, store.SetProjectAgents(ctx, pa))

		got, err := store.GetProjectAgents(ctx, "proj-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, []string{"coder", "reviewer"}, got.AgentSlugs)
	})

	t.Run("EmptySlice", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		pa := &ProjectAgents{
			ProjectID:   "proj-1",
			AgentSlugs:  []string{},
			RefreshedAt: time.Now().UTC(),
		}
		require.NoError(t, store.SetProjectAgents(ctx, pa))

		got, err := store.GetProjectAgents(ctx, "proj-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, []string{}, got.AgentSlugs)
	})
}

// --- PendingAskUser ---

func TestPendingAskUser(t *testing.T) {
	t.Run("CreateAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		pending := &PendingAskUser{
			RequestID: "req-123",
			MessageID: "111222333444555666",
			ChannelID: "999888777666555444",
			AgentSlug: "coder",
			ProjectID: "proj-1",
			Choices:   []string{"Yes", "No", "Maybe"},
			ExpiresAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			Responded: false,
		}
		require.NoError(t, store.CreatePendingAskUser(ctx, pending))

		got, err := store.GetPendingAskUser(ctx, "req-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "req-123", got.RequestID)
		assert.Equal(t, "111222333444555666", got.MessageID)
		assert.Equal(t, "999888777666555444", got.ChannelID)
		assert.Equal(t, "coder", got.AgentSlug)
		assert.Equal(t, "proj-1", got.ProjectID)
		assert.Equal(t, []string{"Yes", "No", "Maybe"}, got.Choices)
		assert.False(t, got.Responded)
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetPendingAskUser(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("MarkResponded", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreatePendingAskUser(ctx, &PendingAskUser{
			RequestID: "req-123",
			MessageID: "42",
			ChannelID: "100",
			ExpiresAt: time.Now().Add(time.Hour).UTC(),
		}))

		require.NoError(t, store.MarkAskUserResponded(ctx, "req-123"))

		got, err := store.GetPendingAskUser(ctx, "req-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.True(t, got.Responded)
	})

	t.Run("DeleteExpired", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		// Save one expired and one active.
		require.NoError(t, store.CreatePendingAskUser(ctx, &PendingAskUser{
			RequestID: "expired",
			MessageID: "1",
			ChannelID: "100",
			ExpiresAt: time.Now().Add(-time.Hour).UTC(),
		}))
		require.NoError(t, store.CreatePendingAskUser(ctx, &PendingAskUser{
			RequestID: "active",
			MessageID: "2",
			ChannelID: "100",
			ExpiresAt: time.Now().Add(time.Hour).UTC(),
		}))

		n, err := store.DeleteExpiredAskUsers(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		got, err := store.GetPendingAskUser(ctx, "expired")
		require.NoError(t, err)
		assert.Nil(t, got)

		got, err = store.GetPendingAskUser(ctx, "active")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})

	t.Run("EmptyChoices", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreatePendingAskUser(ctx, &PendingAskUser{
			RequestID: "req-empty",
			MessageID: "1",
			ChannelID: "100",
			Choices:   []string{},
			ExpiresAt: time.Now().Add(time.Hour).UTC(),
		}))

		got, err := store.GetPendingAskUser(ctx, "req-empty")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, []string{}, got.Choices)
	})
}

// --- Store lifecycle ---

func TestStore_OpenInvalidPath(t *testing.T) {
	_, err := NewSQLiteStore("/nonexistent/dir/test.db")
	assert.Error(t, err)
}
