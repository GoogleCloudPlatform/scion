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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// SlackChannel delivers notifications via a Slack incoming webhook.
type SlackChannel struct {
	webhookURL      string
	channel         string
	mentionOnUrgent string
	client          *http.Client
}

// NewSlackChannel creates a SlackChannel from params.
// Supported params:
//   - webhook_url: Slack incoming webhook URL (required)
//   - channel: override channel (optional, uses webhook default if empty)
//   - mention_on_urgent: mention string for urgent messages (e.g., "@here", "@channel")
func NewSlackChannel(params map[string]string) *SlackChannel {
	return &SlackChannel{
		webhookURL:      params["webhook_url"],
		channel:         params["channel"],
		mentionOnUrgent: params["mention_on_urgent"],
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *SlackChannel) Name() string { return "slack" }

func (s *SlackChannel) Validate() error {
	if s.webhookURL == "" {
		return fmt.Errorf("slack channel requires a 'webhook_url' param")
	}
	if !strings.HasPrefix(s.webhookURL, "https://hooks.slack.com/") {
		return fmt.Errorf("slack webhook_url must start with https://hooks.slack.com/")
	}
	return nil
}

// slackPayload is the Slack incoming webhook request body.
type slackPayload struct {
	Channel string `json:"channel,omitempty"`
	Text    string `json:"text"`
}

func (s *SlackChannel) Deliver(ctx context.Context, msg *messages.StructuredMessage) error {
	text := formatSlackMessage(msg, s.mentionOnUrgent)

	payload := slackPayload{
		Channel: s.channel,
		Text:    text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack webhook request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}

	return nil
}

// formatSlackMessage builds a human-readable Slack message from a StructuredMessage.
func formatSlackMessage(msg *messages.StructuredMessage, mentionOnUrgent string) string {
	var b strings.Builder

	if msg.Urgent && mentionOnUrgent != "" {
		b.WriteString(mentionOnUrgent)
		b.WriteString(" ")
	}

	// Type emoji prefix
	switch msg.Type {
	case messages.TypeStateChange:
		b.WriteString(":information_source: ")
	case messages.TypeInputNeeded:
		b.WriteString(":raising_hand: ")
	default:
		b.WriteString(":speech_balloon: ")
	}

	b.WriteString("*[")
	b.WriteString(msg.Type)
	b.WriteString("]* ")

	b.WriteString("from `")
	b.WriteString(msg.Sender)
	b.WriteString("`: ")

	b.WriteString(msg.Msg)

	return b.String()
}
