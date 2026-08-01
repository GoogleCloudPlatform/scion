package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
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

// --- set-default types ---

type setDefaultRequest struct {
	ProjectID string `json:"project_id"`
	ChannelID string `json:"channel_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	AgentSlug string `json:"agent_slug"`
}

type setDefaultResponse struct {
	Status    string `json:"status"`
	Scope     string `json:"scope"` // "channel" or "thread"
	ChannelID string `json:"channel_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	AgentSlug string `json:"agent_slug"`
}

// --- channel-history types ---

type channelHistoryRequest struct {
	ChannelID  string `json:"channel_id"`
	ProjectID  string `json:"project_id"`
	Limit      int    `json:"limit,omitempty"`
	Before     string `json:"before,omitempty"`
	After      string `json:"after,omitempty"`
	HumansOnly bool   `json:"humans_only,omitempty"`
}

type messageAuthor struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type messageAttachment struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

type messageInfo struct {
	ID          string              `json:"id"`
	Author      messageAuthor       `json:"author"`
	Content     string              `json:"content"`
	Timestamp   string              `json:"timestamp"`
	Attachments []messageAttachment `json:"attachments,omitempty"`
	ReplyTo     string              `json:"reply_to,omitempty"`
}

type channelHistoryResponse struct {
	ChannelID string        `json:"channel_id"`
	Messages  []messageInfo `json:"messages"`
	HasMore   bool          `json:"has_more"`
}

// --- send-dm types ---

type sendDMRequest struct {
	RecipientEmail string `json:"recipient_email"`
	Message        string `json:"message"`
	ProjectID      string `json:"project_id"`
}

type sendDMResponse struct {
	Status         string `json:"status"`
	RecipientEmail string `json:"recipient_email"`
	DMChannelID    string `json:"dm_channel_id"`
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

func (b *DiscordBroker) querySetDefault(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var req setDefaultRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.ProjectID == "" || req.ChannelID == "" || req.AgentSlug == "" {
		return nil, fmt.Errorf("project_id, channel_id, and agent_slug are required")
	}

	// Verify channel belongs to this project.
	link, err := b.store.GetChannelLink(ctx, req.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("get channel link: %w", err)
	}
	if link == nil || !link.Active {
		return nil, plugin.ErrNotFound
	}
	if link.ProjectID != req.ProjectID {
		return nil, plugin.ErrForbidden
	}

	// Validate agent slug exists in the project.
	agents, err := b.hubClient.ListAgents(ctx, req.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	found := false
	// Case-insensitive match (following commands.go pattern).
	matchedSlug := req.AgentSlug
	for _, a := range agents {
		if strings.EqualFold(a.Slug, req.AgentSlug) {
			found = true
			matchedSlug = a.Slug // use canonical casing
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("agent %q not found in project: %w", req.AgentSlug, plugin.ErrNotFound)
	}

	scope := "channel"
	if req.ThreadID != "" {
		// Thread-level default.
		scope = "thread"
		if err := b.store.SetThreadDefault(ctx, req.ChannelID, req.ThreadID, matchedSlug); err != nil {
			return nil, fmt.Errorf("set thread default: %w", err)
		}
	} else {
		// Channel-level default.
		link.DefaultAgent = matchedSlug
		if err := b.store.UpdateChannelLink(ctx, link); err != nil {
			return nil, fmt.Errorf("update channel link: %w", err)
		}
	}

	resp := setDefaultResponse{
		Status:    "ok",
		Scope:     scope,
		ChannelID: req.ChannelID,
		ThreadID:  req.ThreadID,
		AgentSlug: matchedSlug,
	}
	return json.Marshal(resp)
}

func (b *DiscordBroker) queryChannelHistory(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var req channelHistoryRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.ChannelID == "" || req.ProjectID == "" {
		return nil, fmt.Errorf("channel_id and project_id are required")
	}

	// Verify channel belongs to this project.
	link, err := b.store.GetChannelLink(ctx, req.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("get channel link: %w", err)
	}
	if link == nil || !link.Active {
		return nil, plugin.ErrNotFound
	}
	if link.ProjectID != req.ProjectID {
		return nil, plugin.ErrForbidden
	}

	// Default and cap limit.
	limit := req.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}

	// Fetch messages from Discord API.
	// Request one extra to determine has_more.
	msgs, err := b.session.ChannelMessages(req.ChannelID, limit+1, req.Before, req.After, "")
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}

	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}

	// Convert to response format.
	var messages []messageInfo
	for _, m := range msgs {
		// Filter bot messages if humans_only.
		if req.HumansOnly && m.Author != nil && m.Author.Bot {
			continue
		}

		mi := messageInfo{
			ID:        m.ID,
			Content:   m.Content,
			Timestamp: m.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
		}
		if m.Author != nil {
			mi.Author = messageAuthor{
				ID:       m.Author.ID,
				Username: m.Author.Username,
				Bot:      m.Author.Bot,
			}
		}
		for _, a := range m.Attachments {
			mi.Attachments = append(mi.Attachments, messageAttachment{
				Filename: a.Filename,
				URL:      a.URL,
			})
		}
		if m.MessageReference != nil {
			mi.ReplyTo = m.MessageReference.MessageID
		}
		messages = append(messages, mi)
	}

	if messages == nil {
		messages = []messageInfo{}
	}

	resp := channelHistoryResponse{
		ChannelID: req.ChannelID,
		Messages:  messages,
		HasMore:   hasMore,
	}
	return json.Marshal(resp)
}

func (b *DiscordBroker) querySendDM(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var req sendDMRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if req.RecipientEmail == "" || req.Message == "" {
		return nil, fmt.Errorf("recipient_email and message are required")
	}

	// Look up Discord user by email.
	mapping, err := b.store.GetUserMappingByEmail(ctx, req.RecipientEmail)
	if err != nil {
		return nil, fmt.Errorf("get user mapping: %w", err)
	}
	if mapping == nil {
		return nil, plugin.ErrNotFound
	}

	// Check DM consent.
	if !mapping.DMEnabled {
		return nil, plugin.ErrForbidden
	}

	// Create/get DM channel (idempotent).
	dmChannel, err := b.session.UserChannelCreate(mapping.DiscordUserID)
	if err != nil {
		return nil, fmt.Errorf("create DM channel: %w", err)
	}

	// Send the message.
	_, err = b.session.ChannelMessageSend(dmChannel.ID, req.Message)
	if err != nil {
		return nil, fmt.Errorf("send DM: %w", err)
	}

	resp := sendDMResponse{
		Status:         "sent",
		RecipientEmail: req.RecipientEmail,
		DMChannelID:    dmChannel.ID,
	}
	return json.Marshal(resp)
}
