package discord

import (
	"context"
	"encoding/json"
	"fmt"
)

// --- Request/Response types ---

// listChannelsRequest is the params for "list-channels".
type listChannelsRequest struct {
	ProjectID string `json:"project_id"`
}

// channelInfo is a single channel in the list-channels response.
type channelInfo struct {
	ChannelID    string `json:"channel_id"`
	GuildID      string `json:"guild_id"`
	GuildName    string `json:"guild_name"`
	ProjectSlug  string `json:"project_slug"`
	DefaultAgent string `json:"default_agent"`
	Active       bool   `json:"active"`
	LinkedAt     string `json:"linked_at"`
}

// listChannelsResponse is the response for "list-channels".
type listChannelsResponse struct {
	Channels []channelInfo `json:"channels"`
}

// listThreadsRequest is the params for "list-threads".
type listThreadsRequest struct {
	ProjectID string `json:"project_id"`
	ChannelID string `json:"channel_id,omitempty"`
}

// threadInfo is a single thread in the list-threads response.
type threadInfo struct {
	ChannelID string `json:"channel_id"`
	ThreadID  string `json:"thread_id"`
	AgentSlug string `json:"agent_slug"`
}

// channelDefaultInfo is a channel's default agent info.
type channelDefaultInfo struct {
	ChannelID    string `json:"channel_id"`
	DefaultAgent string `json:"default_agent"`
}

// listThreadsResponse is the response for "list-threads".
type listThreadsResponse struct {
	Threads         []threadInfo         `json:"threads"`
	ChannelDefaults []channelDefaultInfo `json:"channel_defaults"`
}

// --- Handlers ---

func (b *DiscordBroker) queryListChannels(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var req listChannelsRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}

	links, err := b.store.GetChannelLinksForProject(ctx, req.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("query channel links: %w", err)
	}

	resp := listChannelsResponse{
		Channels: make([]channelInfo, 0, len(links)),
	}
	for _, link := range links {
		resp.Channels = append(resp.Channels, channelInfo{
			ChannelID:    link.ChannelID,
			GuildID:      link.GuildID,
			GuildName:    link.GuildName,
			ProjectSlug:  link.ProjectSlug,
			DefaultAgent: link.DefaultAgent,
			Active:       link.Active,
			LinkedAt:     link.LinkedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	return json.Marshal(resp)
}

func (b *DiscordBroker) queryListThreads(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var req listThreadsRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}

	// Get all channel links for the project
	links, err := b.store.GetChannelLinksForProject(ctx, req.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("query channel links: %w", err)
	}

	var threads []threadInfo
	var channelDefaults []channelDefaultInfo

	for _, link := range links {
		// If channel_id filter is specified, skip non-matching channels
		if req.ChannelID != "" && link.ChannelID != req.ChannelID {
			continue
		}

		// Collect channel default
		if link.DefaultAgent != "" {
			channelDefaults = append(channelDefaults, channelDefaultInfo{
				ChannelID:    link.ChannelID,
				DefaultAgent: link.DefaultAgent,
			})
		}

		// Get thread defaults for this channel
		tds, err := b.store.ListThreadDefaultsForChannel(ctx, link.ChannelID)
		if err != nil {
			return nil, fmt.Errorf("query thread defaults for channel %s: %w", link.ChannelID, err)
		}
		for _, td := range tds {
			threads = append(threads, threadInfo{
				ChannelID: td.ChannelID,
				ThreadID:  td.ThreadID,
				AgentSlug: td.AgentSlug,
			})
		}
	}

	// Ensure non-nil slices for JSON marshaling
	if threads == nil {
		threads = []threadInfo{}
	}
	if channelDefaults == nil {
		channelDefaults = []channelDefaultInfo{}
	}

	resp := listThreadsResponse{
		Threads:         threads,
		ChannelDefaults: channelDefaults,
	}
	return json.Marshal(resp)
}
