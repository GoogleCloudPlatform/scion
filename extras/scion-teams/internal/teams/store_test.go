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

package teams

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
			ConversationID:     "conv-123",
			TeamID:             "team-456",
			TeamName:           "Engineering",
			ChannelName:        "general",
			ProjectID:          "proj-1",
			ProjectSlug:        "my-project",
			DefaultAgent:       "coder",
			LinkedBy:           "user-aad-object-id",
			LinkedAt:           time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
			Active:             true,
			ShowAgentToAgent:   false,
			ShowAssistantReply: true,
			ShowStateChanges:   true,
			ChatOnly:           false,
		}

		require.NoError(t, store.CreateChannelLink(ctx, link))

		got, err := store.GetChannelLink(ctx, "conv-123")
		require.NoError(t, err)
		require.NotNil(t, got)

		assert.Equal(t, "conv-123", got.ConversationID)
		assert.Equal(t, "team-456", got.TeamID)
		assert.Equal(t, "Engineering", got.TeamName)
		assert.Equal(t, "general", got.ChannelName)
		assert.Equal(t, "proj-1", got.ProjectID)
		assert.Equal(t, "my-project", got.ProjectSlug)
		assert.Equal(t, "coder", got.DefaultAgent)
		assert.Equal(t, "user-aad-object-id", got.LinkedBy)
		assert.True(t, got.Active)
		assert.False(t, got.ShowAgentToAgent)
		assert.True(t, got.ShowAssistantReply)
		assert.True(t, got.ShowStateChanges)
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
			ConversationID: "conv-123",
			TeamID:         "team-456",
			ProjectID:      "proj-1",
			DefaultAgent:   "coder",
			LinkedAt:       time.Now().UTC(),
			Active:         true,
		}
		require.NoError(t, store.CreateChannelLink(ctx, link))

		link.DefaultAgent = "reviewer"
		link.ProjectSlug = "updated-slug"
		link.ShowAgentToAgent = true
		require.NoError(t, store.CreateChannelLink(ctx, link))

		got, err := store.GetChannelLink(ctx, "conv-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.DefaultAgent)
		assert.Equal(t, "updated-slug", got.ProjectSlug)
		assert.True(t, got.ShowAgentToAgent)
	})

	t.Run("GetByProject", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		convIDs := []string{"conv-100", "conv-200", "conv-300"}
		for i, convID := range convIDs {
			projID := "proj-1"
			if i == 2 {
				projID = "proj-2"
			}
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ConversationID: convID,
				TeamID:         "team-1",
				ProjectID:      projID,
				LinkedAt:       time.Now().UTC(),
				Active:         true,
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

	t.Run("GetByProjectSlugFallback", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		// Create a link where ProjectID differs from ProjectSlug.
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ConversationID: "conv-slug",
			TeamID:         "team-1",
			ProjectID:      "proj-123",
			ProjectSlug:    "my-project",
			LinkedAt:       time.Now().UTC(),
			Active:         true,
		}))

		// Query by slug should find the link via the slug fallback.
		links, err := store.GetChannelLinksForProject(ctx, "my-project")
		require.NoError(t, err)
		assert.Len(t, links, 1)
		assert.Equal(t, "conv-slug", links[0].ConversationID)
		assert.Equal(t, "proj-123", links[0].ProjectID)

		// Query by project ID should also still work.
		links, err = store.GetChannelLinksForProject(ctx, "proj-123")
		require.NoError(t, err)
		assert.Len(t, links, 1)
		assert.Equal(t, "conv-slug", links[0].ConversationID)

		// Inactive links should not be returned.
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ConversationID: "conv-inactive",
			TeamID:         "team-1",
			ProjectID:      "proj-123",
			ProjectSlug:    "my-project",
			LinkedAt:       time.Now().UTC(),
			Active:         false,
		}))

		links, err = store.GetChannelLinksForProject(ctx, "proj-123")
		require.NoError(t, err)
		assert.Len(t, links, 1, "inactive links should be excluded")
	})

	t.Run("GetAll", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		links, err := store.GetAllChannelLinks(ctx)
		require.NoError(t, err)
		assert.Len(t, links, 0)

		for _, convID := range []string{"conv-100", "conv-200", "conv-300"} {
			require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
				ConversationID: convID,
				TeamID:         "team-1",
				ProjectID:      "proj-1",
				LinkedAt:       time.Now().UTC(),
				Active:         true,
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
			ConversationID:     "conv-111",
			TeamID:             "team-999",
			ProjectID:          "proj-1",
			DefaultAgent:       "coder",
			LinkedAt:           time.Now().UTC(),
			Active:             true,
			ShowAssistantReply: true,
		}
		require.NoError(t, store.CreateChannelLink(ctx, link))

		link.DefaultAgent = "reviewer"
		link.ChatOnly = true
		require.NoError(t, store.UpdateChannelLink(ctx, link))

		got, err := store.GetChannelLink(ctx, "conv-111")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.DefaultAgent)
		assert.True(t, got.ChatOnly)
	})

	t.Run("Delete", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ConversationID: "conv-100",
			TeamID:         "team-1",
			ProjectID:      "proj-1",
			LinkedAt:       time.Now().UTC(),
			Active:         true,
		}))

		require.NoError(t, store.DeleteChannelLink(ctx, "conv-100"))

		got, err := store.GetChannelLink(ctx, "conv-100")
		require.NoError(t, err)
		assert.Nil(t, got)

		// Delete non-existent is not an error.
		require.NoError(t, store.DeleteChannelLink(ctx, "nonexistent"))
	})
}

// --- ConversationReference ---

func TestConversationReferenceCRUD(t *testing.T) {
	t.Run("UpsertAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		ref := &ConversationReference{
			ConversationID:   "conv-123",
			ServiceURL:       "https://smba.trafficmanager.net/amer/",
			BotID:            "bot-app-id",
			BotName:          "ScionBot",
			TenantID:         "tenant-abc",
			ConversationType: "channel",
			TeamID:           "team-456",
			ChannelID:        "channel-789",
			UpdatedAt:        time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		}
		require.NoError(t, store.UpsertConversationReference(ctx, ref))

		got, err := store.GetConversationReference(ctx, "conv-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "conv-123", got.ConversationID)
		assert.Equal(t, "https://smba.trafficmanager.net/amer/", got.ServiceURL)
		assert.Equal(t, "bot-app-id", got.BotID)
		assert.Equal(t, "ScionBot", got.BotName)
		assert.Equal(t, "tenant-abc", got.TenantID)
		assert.Equal(t, "channel", got.ConversationType)
		assert.Equal(t, "team-456", got.TeamID)
		assert.Equal(t, "channel-789", got.ChannelID)
		assert.Equal(t, 2026, got.UpdatedAt.Year())
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetConversationReference(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("UpsertUpdatesExisting", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		ref := &ConversationReference{
			ConversationID: "conv-123",
			ServiceURL:     "https://old-url.example.com/",
			BotID:          "bot-1",
			UpdatedAt:      time.Now().UTC(),
		}
		require.NoError(t, store.UpsertConversationReference(ctx, ref))

		ref.ServiceURL = "https://new-url.example.com/"
		ref.BotName = "UpdatedBot"
		ref.UpdatedAt = time.Now().UTC().Add(time.Hour)
		require.NoError(t, store.UpsertConversationReference(ctx, ref))

		got, err := store.GetConversationReference(ctx, "conv-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "https://new-url.example.com/", got.ServiceURL)
		assert.Equal(t, "UpdatedBot", got.BotName)
	})

	t.Run("GetByTeam", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		for _, tc := range []struct {
			convID string
			teamID string
		}{
			{"conv-1", "team-A"},
			{"conv-2", "team-A"},
			{"conv-3", "team-B"},
		} {
			require.NoError(t, store.UpsertConversationReference(ctx, &ConversationReference{
				ConversationID: tc.convID,
				ServiceURL:     "https://svc.example.com/",
				TeamID:         tc.teamID,
				UpdatedAt:      time.Now().UTC(),
			}))
		}

		refs, err := store.GetConversationReferencesByTeam(ctx, "team-A")
		require.NoError(t, err)
		assert.Len(t, refs, 2)

		refs, err = store.GetConversationReferencesByTeam(ctx, "team-B")
		require.NoError(t, err)
		assert.Len(t, refs, 1)

		refs, err = store.GetConversationReferencesByTeam(ctx, "team-nonexistent")
		require.NoError(t, err)
		assert.Len(t, refs, 0)
	})
}

// --- UserMapping CRUD ---

func TestUserMappingCRUD(t *testing.T) {
	t.Run("CreateAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		mapping := &TeamsUserMapping{
			TeamsUserID:      "aad-object-id-123",
			TeamsDisplayName: "Alice Smith",
			ScionUserID:      "user-123",
			ScionEmail:       "alice@example.com",
			LinkedAt:         time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			AutoLinked:       false,
		}
		require.NoError(t, store.CreateUserMapping(ctx, mapping))

		got, err := store.GetUserMapping(ctx, "aad-object-id-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "aad-object-id-123", got.TeamsUserID)
		assert.Equal(t, "Alice Smith", got.TeamsDisplayName)
		assert.Equal(t, "user-123", got.ScionUserID)
		assert.Equal(t, "alice@example.com", got.ScionEmail)
		assert.False(t, got.AutoLinked)
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

		require.NoError(t, store.CreateUserMapping(ctx, &TeamsUserMapping{
			TeamsUserID: "aad-456",
			ScionEmail:  "alice@example.com",
			LinkedAt:    time.Now().UTC(),
		}))

		got, err := store.GetUserMappingByEmail(ctx, "alice@example.com")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "aad-456", got.TeamsUserID)

		got, err = store.GetUserMappingByEmail(ctx, "nobody@example.com")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("Upsert", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateUserMapping(ctx, &TeamsUserMapping{
			TeamsUserID:      "aad-456",
			TeamsDisplayName: "Alice",
			ScionEmail:       "alice@old.com",
			LinkedAt:         time.Now().UTC(),
		}))

		require.NoError(t, store.CreateUserMapping(ctx, &TeamsUserMapping{
			TeamsUserID:      "aad-456",
			TeamsDisplayName: "Alice Updated",
			ScionEmail:       "alice@new.com",
			LinkedAt:         time.Now().UTC(),
			AutoLinked:       true,
		}))

		got, err := store.GetUserMapping(ctx, "aad-456")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "Alice Updated", got.TeamsDisplayName)
		assert.Equal(t, "alice@new.com", got.ScionEmail)
		assert.True(t, got.AutoLinked)
	})

	t.Run("Delete", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateUserMapping(ctx, &TeamsUserMapping{
			TeamsUserID: "aad-456",
			ScionEmail:  "alice@example.com",
			LinkedAt:    time.Now().UTC(),
		}))

		require.NoError(t, store.DeleteUserMapping(ctx, "aad-456"))

		got, err := store.GetUserMapping(ctx, "aad-456")
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
			TeamsUserID:        "aad-456",
			ProjectID:          "proj-1",
			AgentSlug:          "coder",
			LastConversationID: "conv-111",
			LastActivityID:     "act-222",
			LastMessageAt:      time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		}
		require.NoError(t, store.SetConversationContext(ctx, cc))

		got, err := store.GetConversationContext(ctx, "aad-456", "proj-1", "coder")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "aad-456", got.TeamsUserID)
		assert.Equal(t, "proj-1", got.ProjectID)
		assert.Equal(t, "coder", got.AgentSlug)
		assert.Equal(t, "conv-111", got.LastConversationID)
		assert.Equal(t, "act-222", got.LastActivityID)
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
			TeamsUserID:        "aad-456",
			ProjectID:          "proj-1",
			AgentSlug:          "coder",
			LastConversationID: "conv-100",
			LastMessageAt:      time.Now().UTC(),
		}
		require.NoError(t, store.SetConversationContext(ctx, cc))

		cc.LastConversationID = "conv-200"
		cc.LastActivityID = "act-333"
		cc.LastMessageAt = time.Now().UTC().Add(time.Hour)
		require.NoError(t, store.SetConversationContext(ctx, cc))

		got, err := store.GetConversationContext(ctx, "aad-456", "proj-1", "coder")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "conv-200", got.LastConversationID)
		assert.Equal(t, "act-333", got.LastActivityID)
	})

	t.Run("MultipleKeys", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		now := time.Now().UTC()
		for _, slug := range []string{"coder", "reviewer"} {
			require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
				TeamsUserID:        "aad-456",
				ProjectID:          "proj-1",
				AgentSlug:          slug,
				LastConversationID: "conv-100",
				LastMessageAt:      now,
			}))
		}

		got1, err := store.GetConversationContext(ctx, "aad-456", "proj-1", "coder")
		require.NoError(t, err)
		require.NotNil(t, got1)

		got2, err := store.GetConversationContext(ctx, "aad-456", "proj-1", "reviewer")
		require.NoError(t, err)
		require.NotNil(t, got2)

		assert.Equal(t, "coder", got1.AgentSlug)
		assert.Equal(t, "reviewer", got2.AgentSlug)
	})

	t.Run("GetLatest", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		// Save two contexts with different timestamps — "reviewer" is more recent.
		require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
			TeamsUserID:        "aad-456",
			ProjectID:          "proj-1",
			AgentSlug:          "coder",
			LastConversationID: "conv-100",
			LastMessageAt:      time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC),
		}))
		require.NoError(t, store.SetConversationContext(ctx, &ConversationContext{
			TeamsUserID:        "aad-456",
			ProjectID:          "proj-1",
			AgentSlug:          "reviewer",
			LastConversationID: "conv-100",
			LastMessageAt:      time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
		}))

		got, err := store.GetLatestConversationContext(ctx, "aad-456", "proj-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.AgentSlug)
	})

	t.Run("GetLatestNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetLatestConversationContext(ctx, "aad-999", "proj-unknown")
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
			RequestID:      "req-123",
			ActivityID:     "act-456",
			ConversationID: "conv-789",
			AgentSlug:      "coder",
			ProjectID:      "proj-1",
			Choices:        []string{"Yes", "No", "Maybe"},
			ExpiresAt:      time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			Responded:      false,
		}
		require.NoError(t, store.CreatePendingAskUser(ctx, pending))

		got, err := store.GetPendingAskUser(ctx, "req-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "req-123", got.RequestID)
		assert.Equal(t, "act-456", got.ActivityID)
		assert.Equal(t, "conv-789", got.ConversationID)
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
			RequestID:      "req-123",
			ActivityID:     "act-42",
			ConversationID: "conv-100",
			ExpiresAt:      time.Now().Add(time.Hour).UTC(),
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
			RequestID:      "expired",
			ActivityID:     "act-1",
			ConversationID: "conv-100",
			ExpiresAt:      time.Now().Add(-time.Hour).UTC(),
		}))
		require.NoError(t, store.CreatePendingAskUser(ctx, &PendingAskUser{
			RequestID:      "active",
			ActivityID:     "act-2",
			ConversationID: "conv-100",
			ExpiresAt:      time.Now().Add(time.Hour).UTC(),
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
			RequestID:      "req-empty",
			ActivityID:     "act-1",
			ConversationID: "conv-100",
			Choices:        []string{},
			ExpiresAt:      time.Now().Add(time.Hour).UTC(),
		}))

		got, err := store.GetPendingAskUser(ctx, "req-empty")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, []string{}, got.Choices)
	})
}

// --- CallbackLookup ---

func TestCallbackLookup(t *testing.T) {
	t.Run("CreateAndGet", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		lookup := &CallbackLookup{
			ShortID:   "abc123",
			FullData:  `{"action":"approve","request_id":"req-456"}`,
			ExpiresAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		}
		require.NoError(t, store.CreateCallbackLookup(ctx, lookup))

		got, err := store.GetCallbackLookup(ctx, "abc123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "abc123", got.ShortID)
		assert.Equal(t, `{"action":"approve","request_id":"req-456"}`, got.FullData)
	})

	t.Run("GetNotFound", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		got, err := store.GetCallbackLookup(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("Upsert", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateCallbackLookup(ctx, &CallbackLookup{
			ShortID:   "abc123",
			FullData:  "old-data",
			ExpiresAt: time.Now().Add(time.Hour).UTC(),
		}))

		require.NoError(t, store.CreateCallbackLookup(ctx, &CallbackLookup{
			ShortID:   "abc123",
			FullData:  "new-data",
			ExpiresAt: time.Now().Add(2 * time.Hour).UTC(),
		}))

		got, err := store.GetCallbackLookup(ctx, "abc123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "new-data", got.FullData)
	})

	t.Run("DeleteExpired", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		require.NoError(t, store.CreateCallbackLookup(ctx, &CallbackLookup{
			ShortID:   "expired",
			FullData:  "data",
			ExpiresAt: time.Now().Add(-time.Hour).UTC(),
		}))
		require.NoError(t, store.CreateCallbackLookup(ctx, &CallbackLookup{
			ShortID:   "active",
			FullData:  "data",
			ExpiresAt: time.Now().Add(time.Hour).UTC(),
		}))

		n, err := store.DeleteExpiredCallbacks(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		got, err := store.GetCallbackLookup(ctx, "expired")
		require.NoError(t, err)
		assert.Nil(t, got)

		got, err = store.GetCallbackLookup(ctx, "active")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})
}

// --- Advisory lock (SQLite stub — always acquired in single-node mode) ---

func TestAdvisoryLock_SQLiteAlwaysAcquired(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	acquired, handle, err := store.TryAdvisoryLock(ctx, 12345)
	require.NoError(t, err)
	assert.True(t, acquired)
	require.NotNil(t, handle)

	// Release is a no-op but should not error.
	require.NoError(t, handle.Release())

	// Verify is a no-op but should not error.
	require.NoError(t, handle.Verify(ctx))
}

// --- Store lifecycle ---

func TestStore_OpenInvalidPath(t *testing.T) {
	_, err := NewSQLiteStore("/nonexistent/dir/test.db")
	assert.Error(t, err)
}
