package discord

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

// --- queryListChannels tests ---

func TestQueryListChannels(t *testing.T) {
	t.Run("EmptyProject", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		params, _ := json.Marshal(listChannelsRequest{ProjectID: "proj-empty"})
		result, err := b.queryListChannels(ctx, params)
		require.NoError(t, err)

		var resp listChannelsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		assert.Empty(t, resp.Channels)
	})

	t.Run("ProjectWithChannels", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		// Create two channel links for the same project.
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID:    "ch-100",
			GuildID:      "guild-1",
			GuildName:    "Test Server",
			ProjectID:    "proj-1",
			ProjectSlug:  "my-project",
			DefaultAgent: "coder",
			Active:       true,
			LinkedAt:     time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
		}))
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID:    "ch-200",
			GuildID:      "guild-1",
			GuildName:    "Test Server",
			ProjectID:    "proj-1",
			ProjectSlug:  "my-project",
			DefaultAgent: "reviewer",
			Active:       false,
			LinkedAt:     time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC),
		}))

		params, _ := json.Marshal(listChannelsRequest{ProjectID: "proj-1"})
		result, err := b.queryListChannels(ctx, params)
		require.NoError(t, err)

		var resp listChannelsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		require.Len(t, resp.Channels, 2)

		// Verify fields of the first channel.
		found := map[string]channelInfo{}
		for _, ch := range resp.Channels {
			found[ch.ChannelID] = ch
		}

		ch100 := found["ch-100"]
		assert.Equal(t, "guild-1", ch100.GuildID)
		assert.Equal(t, "Test Server", ch100.GuildName)
		assert.Equal(t, "my-project", ch100.ProjectSlug)
		assert.Equal(t, "coder", ch100.DefaultAgent)
		assert.True(t, ch100.Active)
		assert.Equal(t, "2026-01-15T10:00:00Z", ch100.LinkedAt)

		ch200 := found["ch-200"]
		assert.Equal(t, "reviewer", ch200.DefaultAgent)
		assert.False(t, ch200.Active)
	})

	t.Run("MissingProjectID", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		params := json.RawMessage(`{}`)
		_, err := b.queryListChannels(ctx, params)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "project_id is required")
	})

	t.Run("MalformedParams", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		params := json.RawMessage(`invalid json`)
		_, err := b.queryListChannels(ctx, params)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid params")
	})
}

// --- queryListThreads tests ---

func TestQueryListThreads(t *testing.T) {
	t.Run("EmptyProject", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		params, _ := json.Marshal(listThreadsRequest{ProjectID: "proj-empty"})
		result, err := b.queryListThreads(ctx, params)
		require.NoError(t, err)

		var resp listThreadsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		assert.Empty(t, resp.Threads)
		assert.Empty(t, resp.ChannelDefaults)
	})

	t.Run("WithThreadDefaults", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		// Create a channel link.
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID:    "ch-1",
			GuildID:      "guild-1",
			ProjectID:    "proj-1",
			DefaultAgent: "coder",
			Active:       true,
			LinkedAt:     time.Now().UTC(),
		}))
		// Set thread defaults.
		require.NoError(t, store.SetThreadDefault(ctx, "ch-1", "thread-1", "agent-a"))
		require.NoError(t, store.SetThreadDefault(ctx, "ch-1", "thread-2", "agent-b"))

		params, _ := json.Marshal(listThreadsRequest{ProjectID: "proj-1"})
		result, err := b.queryListThreads(ctx, params)
		require.NoError(t, err)

		var resp listThreadsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		assert.Len(t, resp.Threads, 2)

		// Verify thread info.
		threadMap := map[string]threadInfo{}
		for _, th := range resp.Threads {
			threadMap[th.ThreadID] = th
		}
		assert.Equal(t, "agent-a", threadMap["thread-1"].AgentSlug)
		assert.Equal(t, "agent-b", threadMap["thread-2"].AgentSlug)
	})

	t.Run("ChannelIDFilter", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		// Create two channel links.
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID: "ch-1",
			GuildID:   "guild-1",
			ProjectID: "proj-1",
			Active:    true,
			LinkedAt:  time.Now().UTC(),
		}))
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID: "ch-2",
			GuildID:   "guild-1",
			ProjectID: "proj-1",
			Active:    true,
			LinkedAt:  time.Now().UTC(),
		}))
		// Set thread defaults for both channels.
		require.NoError(t, store.SetThreadDefault(ctx, "ch-1", "thread-1", "agent-a"))
		require.NoError(t, store.SetThreadDefault(ctx, "ch-2", "thread-2", "agent-b"))

		// Filter by ch-1 only.
		params, _ := json.Marshal(listThreadsRequest{ProjectID: "proj-1", ChannelID: "ch-1"})
		result, err := b.queryListThreads(ctx, params)
		require.NoError(t, err)

		var resp listThreadsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		require.Len(t, resp.Threads, 1)
		assert.Equal(t, "ch-1", resp.Threads[0].ChannelID)
		assert.Equal(t, "thread-1", resp.Threads[0].ThreadID)
		assert.Equal(t, "agent-a", resp.Threads[0].AgentSlug)
	})

	t.Run("ChannelDefaultsIncluded", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		// Create a channel link with a default agent.
		require.NoError(t, store.CreateChannelLink(ctx, &ChannelLink{
			ChannelID:    "ch-1",
			GuildID:      "guild-1",
			ProjectID:    "proj-1",
			DefaultAgent: "coder",
			Active:       true,
			LinkedAt:     time.Now().UTC(),
		}))

		params, _ := json.Marshal(listThreadsRequest{ProjectID: "proj-1"})
		result, err := b.queryListThreads(ctx, params)
		require.NoError(t, err)

		var resp listThreadsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		require.Len(t, resp.ChannelDefaults, 1)
		assert.Equal(t, "ch-1", resp.ChannelDefaults[0].ChannelID)
		assert.Equal(t, "coder", resp.ChannelDefaults[0].DefaultAgent)
	})
}

// --- BrokerQuery dispatch tests ---

func TestBrokerQueryDispatch(t *testing.T) {
	t.Run("ListChannelsDispatches", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		params, _ := json.Marshal(listChannelsRequest{ProjectID: "proj-1"})
		result, err := b.BrokerQuery(ctx, "list-channels", params)
		require.NoError(t, err)

		var resp listChannelsResponse
		require.NoError(t, json.Unmarshal(result, &resp))
		assert.Empty(t, resp.Channels)
	})

	t.Run("UnknownOperationReturnsError", func(t *testing.T) {
		store := newTestStore(t)
		b := &DiscordBroker{store: store, log: discardLogger()}
		ctx := context.Background()

		_, err := b.BrokerQuery(ctx, "unknown-op", json.RawMessage(`{}`))
		require.Error(t, err)
		assert.ErrorIs(t, err, plugin.ErrUnsupportedOperation)
	})
}
