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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// DiscordChannel delivers notifications via a Discord incoming webhook.
// Uses Discord's native webhook format (embeds, allowed_mentions) — not the
// Slack-compatible /slack endpoint.
type DiscordChannel struct {
	webhookURL      string
	mentionOnUrgent string // e.g. "<@&9876543210>" for a role or "<@12345>" for a user
	username        string // optional override of the webhook's default username
	avatarURL       string // optional override of the webhook's default avatar
	client          *http.Client
}

// NewDiscordChannel creates a DiscordChannel from params.
// Supported params:
//   - webhook_url: Discord incoming webhook URL (required)
//   - mention_on_urgent: mention string for urgent messages using Discord syntax
//     (e.g. "<@&9876543210>" for a role, "<@12345>" for a user). @here and
//     @everyone are intentionally not allowed — the channel strips them from
//     allowed_mentions so Discord will not actually deliver them even if present
//     in Content.
//   - username: override the webhook's default username (optional)
//   - avatar_url: override the webhook's default avatar (optional)
func NewDiscordChannel(params map[string]string) *DiscordChannel {
	return &DiscordChannel{
		webhookURL:      params["webhook_url"],
		mentionOnUrgent: params["mention_on_urgent"],
		username:        params["username"],
		avatarURL:       params["avatar_url"],
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (d *DiscordChannel) Name() string { return "discord" }

// allowedDiscordHosts is the closed set of Discord webhook hostnames.
var allowedDiscordHosts = map[string]bool{
	"discord.com":        true,
	"discordapp.com":     true,
	"ptb.discord.com":    true,
	"canary.discord.com": true,
}

func (d *DiscordChannel) Validate() error {
	if d.webhookURL == "" {
		return fmt.Errorf("discord channel requires a 'webhook_url' param")
	}
	u, err := url.Parse(d.webhookURL)
	if err != nil {
		return fmt.Errorf("discord webhook_url is not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("discord webhook_url must use https:// (got %q)", u.Scheme)
	}
	if !allowedDiscordHosts[u.Host] {
		return fmt.Errorf("discord webhook_url host %q is not a recognised Discord domain (expected one of: discord.com, discordapp.com, ptb.discord.com, canary.discord.com)", u.Host)
	}
	if !strings.HasPrefix(u.Path, "/api/webhooks/") {
		return fmt.Errorf("discord webhook_url path must begin with /api/webhooks/ (got %q)", u.Path)
	}
	// The whole point of this channel type is to NOT use the Slack-compat
	// endpoint. Reject it explicitly so users migrate to the native format.
	if strings.HasSuffix(u.Path, "/slack") || strings.HasSuffix(u.Path, "/slack/") {
		return fmt.Errorf("discord webhook_url must not end with /slack — use the native Discord webhook endpoint (remove the /slack suffix) with type: discord")
	}
	return nil
}

// Discord embed colour constants (decimal RGB, matching Discord API).
const (
	discordColorStateChange = 0x3498db // blue   — informational
	discordColorInputNeeded = 0xf1c40f // yellow — needs attention
	discordColorInstruction = 0x95a5a6 // grey   — routine
	discordColorUrgent      = 0xe74c3c // red    — urgent (overrides type colour)
)

// Discord embed size limits (from the webhook API docs).
const (
	discordMaxDescription = 2048 // per-embed description cap
	discordMaxEmbedTotal  = 6000 // total chars across title+description+fields+footer
)

// discordPayload is the native Discord webhook request body.
// Ref: https://discord.com/developers/docs/resources/webhook#execute-webhook
type discordPayload struct {
	Content         string                  `json:"content,omitempty"`
	Username        string                  `json:"username,omitempty"`
	AvatarURL       string                  `json:"avatar_url,omitempty"`
	Embeds          []discordEmbed          `json:"embeds,omitempty"`
	AllowedMentions *discordAllowedMentions `json:"allowed_mentions,omitempty"`
}

type discordEmbed struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Color       int    `json:"color,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// discordAllowedMentions locks down what mentions Discord will honour.
// We explicitly set Parse to an empty slice so @everyone/@here are never
// resolved, and list role/user IDs we DO want resolved.
type discordAllowedMentions struct {
	Parse []string `json:"parse"` // always []string{} — not omitempty
	Roles []string `json:"roles,omitempty"`
	Users []string `json:"users,omitempty"`
}

// reDiscordRole matches <@&NNN> role mentions.
var reDiscordRole = regexp.MustCompile(`<@&(\d+)>`)

// reDiscordUser matches <@NNN> user mentions (not <@&NNN>).
var reDiscordUser = regexp.MustCompile(`<@(\d+)>`)

// extractDiscordRoleIDs finds all <@&NNN> role mentions in s and returns the NNNs.
func extractDiscordRoleIDs(s string) []string {
	matches := reDiscordRole.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m[1])
	}
	return ids
}

// extractDiscordUserIDs finds all <@NNN> user mentions (not <@&NNN>) in s and returns the NNNs.
func extractDiscordUserIDs(s string) []string {
	// Remove role mentions first so we don't match their digits.
	stripped := reDiscordRole.ReplaceAllString(s, "")
	matches := reDiscordUser.FindAllStringSubmatch(stripped, -1)
	if len(matches) == 0 {
		return nil
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m[1])
	}
	return ids
}

func discordColorForMessage(msg *messages.StructuredMessage) int {
	if msg.Urgent {
		return discordColorUrgent
	}
	switch msg.Type {
	case messages.TypeStateChange:
		return discordColorStateChange
	case messages.TypeInputNeeded:
		return discordColorInputNeeded
	case messages.TypeInstruction:
		return discordColorInstruction
	default:
		return discordColorStateChange
	}
}

// truncateForDiscordEmbed trims s so it fits within Discord's per-description
// cap, appending an ellipsis marker if truncation occurred. Rune-safe.
func truncateForDiscordEmbed(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	const ellipsis = "\n…(truncated)"
	ellipsisRunes := []rune(ellipsis)
	cut := max - len(ellipsisRunes)
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + ellipsis
}

// formatDiscordPayload builds a discordPayload from a StructuredMessage.
// Extracted from Deliver so tests can exercise it without spinning up an
// httptest.Server.
func formatDiscordPayload(msg *messages.StructuredMessage, mentionOnUrgent string) discordPayload {
	title := fmt.Sprintf("[%s] from %s", msg.Type, msg.Sender)
	if len([]rune(title)) > 256 { // Discord embed title cap
		title = string([]rune(title)[:253]) + "..."
	}

	description := truncateForDiscordEmbed(msg.Msg, discordMaxDescription)

	embed := discordEmbed{
		Title:       title,
		Description: description,
		Color:       discordColorForMessage(msg),
		Timestamp:   msg.Timestamp,
	}

	payload := discordPayload{
		Embeds:          []discordEmbed{embed},
		AllowedMentions: &discordAllowedMentions{Parse: []string{}},
	}

	if msg.Urgent && mentionOnUrgent != "" {
		payload.Content = mentionOnUrgent
		// Extract role and user IDs from <@&123> / <@123> patterns so Discord
		// actually resolves them (Parse: [] alone would suppress them).
		payload.AllowedMentions.Roles = extractDiscordRoleIDs(mentionOnUrgent)
		payload.AllowedMentions.Users = extractDiscordUserIDs(mentionOnUrgent)
	}

	return payload
}

// Deliver sends a notification to the Discord webhook.
func (d *DiscordChannel) Deliver(ctx context.Context, msg *messages.StructuredMessage) error {
	payload := formatDiscordPayload(msg, d.mentionOnUrgent)

	if d.username != "" {
		payload.Username = d.username
	}
	if d.avatarURL != "" {
		payload.AvatarURL = d.avatarURL
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	// Discord returns 204 No Content on success; also accept 200 for test servers.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("discord webhook returned status %d", resp.StatusCode)
	}

	return nil
}
