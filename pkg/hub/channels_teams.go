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
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// tenantIDPattern matches a valid UUID (Azure AD tenant IDs are always UUIDs).
var tenantIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// TeamsChannel delivers notifications to a Microsoft Teams conversation
// via the Bot Connector REST API. This is an outbound-only notification
// channel — it does NOT handle inbound messages, commands, or cards.
type TeamsChannel struct {
	appID          string
	appSecret      string
	tenantID       string
	conversationID string
	serviceURL     string
	tokenProvider  *teamsTokenProvider
	client         *http.Client
}

// NewTeamsChannel creates a TeamsChannel from params.
// Supported params:
//   - app_id: Azure App Registration client ID (required)
//   - app_secret: Azure App Registration client secret (required)
//   - tenant_id: Azure AD tenant ID (required)
//   - conversation_id: Teams conversation ID to send to (required)
//   - service_url: Bot Framework service URL (required)
func NewTeamsChannel(params map[string]string) *TeamsChannel {
	ch := &TeamsChannel{
		appID:          params["app_id"],
		appSecret:      params["app_secret"],
		tenantID:       params["tenant_id"],
		conversationID: params["conversation_id"],
		serviceURL:     strings.TrimRight(params["service_url"], "/"),
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
	ch.tokenProvider = newTeamsTokenProvider(ch.appID, ch.appSecret, ch.tenantID, ch.client)
	return ch
}

func (c *TeamsChannel) Name() string { return "teams" }

func (c *TeamsChannel) Validate() error {
	if c.appID == "" {
		return fmt.Errorf("teams channel requires an 'app_id' param")
	}
	if c.appSecret == "" {
		return fmt.Errorf("teams channel requires an 'app_secret' param")
	}
	if c.tenantID == "" {
		return fmt.Errorf("teams channel requires a 'tenant_id' param")
	}
	if !tenantIDPattern.MatchString(c.tenantID) {
		return fmt.Errorf("teams channel tenant_id must be a valid UUID")
	}
	if c.conversationID == "" {
		return fmt.Errorf("teams channel requires a 'conversation_id' param")
	}
	if c.serviceURL == "" {
		return fmt.Errorf("teams channel requires a 'service_url' param")
	}
	u, err := url.Parse(c.serviceURL)
	if err != nil {
		return fmt.Errorf("teams channel service_url is not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("teams channel service_url must use https:// (got %q)", u.Scheme)
	}
	return nil
}

// teamsActivity is the Bot Framework Activity payload for sending messages.
type teamsActivity struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Deliver sends a notification to the Teams conversation.
func (c *TeamsChannel) Deliver(ctx context.Context, msg *messages.StructuredMessage) error {
	token, err := c.tokenProvider.getToken(ctx)
	if err != nil {
		return fmt.Errorf("teams channel: acquire token: %w", err)
	}

	text := formatTeamsNotification(msg)

	activity := teamsActivity{
		Type: "message",
		Text: text,
	}

	body, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("teams channel: marshal activity: %w", err)
	}

	activityURL := fmt.Sprintf("%s/v3/conversations/%s/activities",
		c.serviceURL, url.PathEscape(c.conversationID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, activityURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("teams channel: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("teams channel: send activity: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("teams channel: API returned status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(resBody)))
	}

	return nil
}

// formatTeamsNotification builds a plain text notification string from a
// StructuredMessage. Teams supports basic Markdown in text activities.
func formatTeamsNotification(msg *messages.StructuredMessage) string {
	var sb strings.Builder

	if msg.Urgent {
		sb.WriteString("**[URGENT]** ")
	}

	fmt.Fprintf(&sb, "**[%s]** from **%s**", msg.Type, msg.Sender)
	if msg.Recipient != "" {
		fmt.Fprintf(&sb, " to %s", msg.Recipient)
	}
	sb.WriteString("\n\n")
	sb.WriteString(msg.Msg)

	return sb.String()
}

// --- Simple OAuth2 token provider for the notification channel ---

// teamsTokenProvider acquires OAuth2 tokens via the client_credentials grant.
// It caches tokens and refreshes them before expiry.
type teamsTokenProvider struct {
	mu        sync.RWMutex
	token     string
	expiresAt time.Time

	appID     string
	appSecret string
	tenantID  string

	client        *http.Client
	tokenEndpoint string
}

const (
	teamsChannelTokenScope         = "https://api.botframework.com/.default"
	teamsChannelTokenRefreshWindow = 5 * time.Minute
)

func newTeamsTokenProvider(appID, appSecret, tenantID string, client *http.Client) *teamsTokenProvider {
	return &teamsTokenProvider{
		appID:         appID,
		appSecret:     appSecret,
		tenantID:      tenantID,
		client:        client,
		tokenEndpoint: fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID),
	}
}

// teamsTokenResponse is the JSON response from the Azure AD token endpoint.
type teamsTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func (tp *teamsTokenProvider) getToken(ctx context.Context) (string, error) {
	tp.mu.RLock()
	if tp.token != "" && time.Now().Add(teamsChannelTokenRefreshWindow).Before(tp.expiresAt) {
		t := tp.token
		tp.mu.RUnlock()
		return t, nil
	}
	tp.mu.RUnlock()

	return tp.refresh(ctx)
}

func (tp *teamsTokenProvider) refresh(ctx context.Context) (string, error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	// Double-check after acquiring write lock.
	if tp.token != "" && time.Now().Add(teamsChannelTokenRefreshWindow).Before(tp.expiresAt) {
		return tp.token, nil
	}

	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {tp.appID},
		"client_secret": {tp.appSecret},
		"scope":         {teamsChannelTokenScope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tp.tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := tp.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result teamsTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	if result.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned empty access_token")
	}

	tp.token = result.AccessToken
	tp.expiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)

	return tp.token, nil
}
