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

package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// DiscordService provides Discord-related operations for a project.
type DiscordService interface {
	// ListChannels returns linked Discord channels for the project.
	ListChannels(ctx context.Context) (json.RawMessage, error)
	// ListThreads returns thread-agent mappings for the project.
	ListThreads(ctx context.Context, channelID string) (json.RawMessage, error)
	// SetDefault sets the default agent for a channel or thread.
	SetDefault(ctx context.Context, req SetDefaultRequest) (json.RawMessage, error)
	// ChannelHistory fetches recent messages from a channel.
	ChannelHistory(ctx context.Context, channelID string, opts HistoryOptions) (json.RawMessage, error)
	// SendDM sends a direct message to a registered user.
	SendDM(ctx context.Context, req SendDMRequest) (json.RawMessage, error)
}

// SetDefaultRequest contains parameters for setting a default agent.
type SetDefaultRequest struct {
	ChannelID string `json:"channel_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	AgentSlug string `json:"agent_slug"`
}

// HistoryOptions contains parameters for fetching channel history.
type HistoryOptions struct {
	Limit      int
	Before     string
	After      string
	HumansOnly bool
}

// SendDMRequest contains parameters for sending a direct message.
type SendDMRequest struct {
	RecipientEmail string `json:"recipient_email"`
	Message        string `json:"message"`
}

type discordService struct {
	c         *client
	projectID string
}

func (s *discordService) ListChannels(ctx context.Context) (json.RawMessage, error) {
	path := fmt.Sprintf("/api/v1/projects/%s/discord/channels", url.PathEscape(s.projectID))
	resp, err := s.c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeResponse[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return *raw, nil
}

func (s *discordService) ListThreads(ctx context.Context, channelID string) (json.RawMessage, error) {
	path := fmt.Sprintf("/api/v1/projects/%s/discord/threads", url.PathEscape(s.projectID))
	var query url.Values
	if channelID != "" {
		query = url.Values{"channel_id": {channelID}}
	}
	resp, err := s.c.getWithQuery(ctx, path, query, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeResponse[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return *raw, nil
}

func (s *discordService) SetDefault(ctx context.Context, req SetDefaultRequest) (json.RawMessage, error) {
	path := fmt.Sprintf("/api/v1/projects/%s/discord/default", url.PathEscape(s.projectID))
	resp, err := s.c.put(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeResponse[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return *raw, nil
}

func (s *discordService) ChannelHistory(ctx context.Context, channelID string, opts HistoryOptions) (json.RawMessage, error) {
	path := fmt.Sprintf("/api/v1/projects/%s/discord/channels/%s/history", url.PathEscape(s.projectID), url.PathEscape(channelID))
	query := url.Values{}
	if opts.Limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	if opts.Before != "" {
		query.Set("before", opts.Before)
	}
	if opts.After != "" {
		query.Set("after", opts.After)
	}
	if opts.HumansOnly {
		query.Set("humans_only", "true")
	}
	resp, err := s.c.getWithQuery(ctx, path, query, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeResponse[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return *raw, nil
}

func (s *discordService) SendDM(ctx context.Context, req SendDMRequest) (json.RawMessage, error) {
	path := fmt.Sprintf("/api/v1/projects/%s/discord/dm", url.PathEscape(s.projectID))
	resp, err := s.c.post(ctx, path, req, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeResponse[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return *raw, nil
}
