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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// HubClient delivers inbound messages to the Scion Hub.
type HubClient struct {
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
	log        *slog.Logger
}

// NewHubClient creates a new HubClient for delivering messages to the hub.
func NewHubClient(hubURL, hmacKey, brokerID string, log *slog.Logger) *HubClient {
	return &HubClient{
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		log:        log,
	}
}

// inboundPayload is the JSON body POSTed to the hub's inbound endpoint.
type inboundPayload struct {
	Topic   string                      `json:"topic"`
	Message *messages.StructuredMessage `json:"message"`
}

// DeliverInbound sends a structured message to the hub's inbound endpoint.
func (c *HubClient) DeliverInbound(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	payload := inboundPayload{
		Topic:   topic,
		Message: msg,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal inbound payload: %w", err)
	}

	url := c.hubURL + "/api/v1/broker/inbound"

	c.log.Debug("Delivering inbound message to hub",
		"url", url,
		"topic", topic,
		"sender", msg.Sender,
		"broker_id", c.brokerID,
	)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create inbound request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.signRequest(req); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("inbound delivery failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("hub returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// callbackPayload is the JSON body POSTed to the hub's callback endpoint.
type callbackPayload struct {
	Data map[string]interface{} `json:"data"`
}

// DeliverCallback sends callback data to the hub's callback endpoint.
func (c *HubClient) DeliverCallback(ctx context.Context, data map[string]interface{}) error {
	payload := callbackPayload{Data: data}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal callback payload: %w", err)
	}

	url := c.hubURL + "/api/v1/broker/callback"

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create callback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.signRequest(req); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("callback delivery failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("hub callback returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// --- Hub API query types ---

// AgentInfo holds information about a single agent returned by the hub API.
type AgentInfo struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Activity string `json:"activity,omitempty"`
	Phase    string `json:"phase,omitempty"`
}

// ProjectOption represents a project returned by the hub API.
type ProjectOption struct {
	ID   string
	Name string
	Slug string
}

// --- Hub API query responses ---

type hubProjectsResponse struct {
	Projects []hubProject `json:"projects"`
}

type hubProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type hubAgentsResponse struct {
	Agents []hubAgent `json:"agents"`
}

type hubAgent struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Activity string `json:"activity"`
	Phase    string `json:"phase"`
}

// --- Hub API query methods ---

// ListAgents returns the agents for a given project.
// GET /api/v1/projects/{projectID}/agents
func (c *HubClient) ListAgents(ctx context.Context, projectID string) ([]AgentInfo, error) {
	u := fmt.Sprintf("%s/api/v1/projects/%s/agents", c.hubURL, url.PathEscape(projectID))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create list agents request: %w", err)
	}
	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list agents request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("list agents returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result hubAgentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list agents response: %w", err)
	}

	agents := make([]AgentInfo, len(result.Agents))
	for i, a := range result.Agents {
		agents[i] = AgentInfo{ID: a.ID, Slug: a.Slug, Activity: a.Activity, Phase: a.Phase}
	}
	return agents, nil
}

// ListProjects returns all projects visible to the broker.
// GET /api/v1/projects
func (c *HubClient) ListProjects(ctx context.Context) ([]ProjectOption, error) {
	u := c.hubURL + "/api/v1/projects"

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create list projects request: %w", err)
	}
	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list projects request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("list projects returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result hubProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list projects response: %w", err)
	}

	projects := make([]ProjectOption, len(result.Projects))
	for i, p := range result.Projects {
		projects[i] = ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}
	}
	return projects, nil
}

// GetProjectStatus returns the details of a single project.
// GET /api/v1/projects/{projectID}
func (c *HubClient) GetProjectStatus(ctx context.Context, projectID string) (*ProjectOption, error) {
	u := fmt.Sprintf("%s/api/v1/projects/%s", c.hubURL, url.PathEscape(projectID))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("create get project request: %w", err)
	}
	if err := c.signRequest(req); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get project request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("get project returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var p hubProject
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode get project response: %w", err)
	}

	return &ProjectOption{ID: p.ID, Name: p.Name, Slug: p.Slug}, nil
}

// --- Hub API identity linking methods ---

// RegisterTeamsLink registers a pending identity link code with the hub.
// POST /api/v1/teams/link
func (c *HubClient) RegisterTeamsLink(ctx context.Context, teamsUserID string) (string, error) {
	code := generateLinkCode()

	payload := struct {
		Code        string `json:"code"`
		TeamsUserID string `json:"teamsUserId"`
	}{
		Code:        code,
		TeamsUserID: teamsUserID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal link request: %w", err)
	}

	u := c.hubURL + "/api/v1/teams/link"

	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create link request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := c.signRequest(req); err != nil {
		return "", fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("link request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", fmt.Errorf("hub link returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return code, nil
}

// CheckTeamsLinkStatus polls the hub for the status of a pending identity link.
// GET /api/v1/teams/link/status?teams_user_id=...
func (c *HubClient) CheckTeamsLinkStatus(ctx context.Context, teamsUserID string) (string, string, error) {
	u := fmt.Sprintf("%s/api/v1/teams/link/status?teams_user_id=%s",
		c.hubURL, url.QueryEscape(teamsUserID))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", "", fmt.Errorf("create link status request: %w", err)
	}

	if err := c.signRequest(req); err != nil {
		return "", "", fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("link status request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", "", fmt.Errorf("link status returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Status string `json:"status"`
		User   *struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode link status response: %w", err)
	}

	userID := ""
	if result.User != nil {
		userID = result.User.ID
	}

	return result.Status, userID, nil
}

// generateLinkCode produces a 6-character uppercase alphanumeric code using
// crypto/rand. Characters I, O, 0, and 1 are excluded to avoid confusion.
func generateLinkCode() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			// Fallback should never happen in practice.
			b[i] = chars[i%len(chars)]
			continue
		}
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

// signRequest adds HMAC authentication headers to the request.
func (c *HubClient) signRequest(req *http.Request) error {
	if c.brokerID == "" || c.hmacKey == "" {
		return nil
	}

	secretKey, err := decodeBase64(c.hmacKey)
	if err != nil {
		return fmt.Errorf("decode HMAC key: %w", err)
	}

	auth := &apiclient.HMACAuth{
		BrokerID:  c.brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}

// decodeBase64 tries standard and URL-safe base64 decoding, with and without
// padding, matching the Discord plugin's approach.
func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("invalid base64 encoding")
}
