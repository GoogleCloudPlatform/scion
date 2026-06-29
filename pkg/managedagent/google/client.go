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

package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

const maxErrorBodySize = 64 * 1024

// Client is a thin HTTP client for the Google Managed Agents API.
type Client struct {
	baseURL          string
	apiKey           string
	httpClient       *http.Client
	streamHTTPClient *http.Client
}

// NewClient creates a new Google API client with the given API key.
func NewClient(apiKey string) *Client {
	return &Client{
		baseURL: defaultBaseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		streamHTTPClient: &http.Client{},
	}
}

// SetBaseURL overrides the API base URL (useful for testing).
func (c *Client) SetBaseURL(url string) {
	c.baseURL = url
}

// CreateAgent creates a new managed agent configuration.
func (c *Client) CreateAgent(ctx context.Context, req *CreateAgentRequest) (*Agent, error) {
	var agent Agent
	if err := c.do(ctx, http.MethodPost, "/agents", req, &agent); err != nil {
		return nil, fmt.Errorf("creating agent: %w", err)
	}
	return &agent, nil
}

// GetAgent retrieves an agent by ID.
func (c *Client) GetAgent(ctx context.Context, agentID string) (*Agent, error) {
	var agent Agent
	if err := c.do(ctx, http.MethodGet, "/agents/"+url.PathEscape(agentID), nil, &agent); err != nil {
		return nil, fmt.Errorf("getting agent %q: %w", agentID, err)
	}
	return &agent, nil
}

// DeleteAgent deletes an agent by ID.
func (c *Client) DeleteAgent(ctx context.Context, agentID string) error {
	if err := c.do(ctx, http.MethodDelete, "/agents/"+url.PathEscape(agentID), nil, nil); err != nil {
		return fmt.Errorf("deleting agent %q: %w", agentID, err)
	}
	return nil
}

// ListAgents lists agents with optional pagination.
func (c *Client) ListAgents(ctx context.Context, pageSize int, pageToken string) (*ListAgentsResponse, error) {
	path := "/agents"
	params := url.Values{}
	if pageSize > 0 {
		params.Set("page_size", fmt.Sprintf("%d", pageSize))
	}
	if pageToken != "" {
		params.Set("page_token", pageToken)
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp ListAgentsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	return &resp, nil
}

// CreateInteraction creates a new interaction (synchronous or background).
func (c *Client) CreateInteraction(ctx context.Context, req *CreateInteractionRequest) (*Interaction, error) {
	var interaction Interaction
	if err := c.do(ctx, http.MethodPost, "/interactions", req, &interaction); err != nil {
		return nil, fmt.Errorf("creating interaction: %w", err)
	}
	return &interaction, nil
}

// CreateInteractionStream creates a new streaming interaction and returns the
// raw SSE response body. The caller is responsible for closing the returned reader.
func (c *Client) CreateInteractionStream(ctx context.Context, req *CreateInteractionRequest) (io.ReadCloser, error) {
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/interactions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq, true)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executing streaming request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, c.parseError(resp)
	}

	return resp.Body, nil
}

// GetInteraction retrieves an interaction by ID.
func (c *Client) GetInteraction(ctx context.Context, interactionID string) (*Interaction, error) {
	var interaction Interaction
	if err := c.do(ctx, http.MethodGet, "/interactions/"+url.PathEscape(interactionID), nil, &interaction); err != nil {
		return nil, fmt.Errorf("getting interaction %q: %w", interactionID, err)
	}
	return &interaction, nil
}

// GetInteractionStream opens an SSE stream for a running or completed interaction.
// If lastEventID is non-empty, the stream resumes from that point.
func (c *Client) GetInteractionStream(ctx context.Context, interactionID string, lastEventID string) (io.ReadCloser, error) {
	params := url.Values{"stream": {"true"}}
	if lastEventID != "" {
		params.Set("last_event_id", lastEventID)
	}
	path := "/interactions/" + url.PathEscape(interactionID) + "?" + params.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("creating stream request: %w", err)
	}
	c.setHeaders(httpReq, false)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("opening interaction stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, c.parseError(resp)
	}

	return resp.Body, nil
}

// CancelInteraction cancels a running interaction.
func (c *Client) CancelInteraction(ctx context.Context, interactionID string) error {
	path := "/interactions/" + url.PathEscape(interactionID) + "/cancel"
	if err := c.do(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("cancelling interaction %q: %w", interactionID, err)
	}
	return nil
}

// DeleteInteraction deletes a stored interaction.
func (c *Client) DeleteInteraction(ctx context.Context, interactionID string) error {
	if err := c.do(ctx, http.MethodDelete, "/interactions/"+url.PathEscape(interactionID), nil, nil); err != nil {
		return fmt.Errorf("deleting interaction %q: %w", interactionID, err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, reqBody interface{}, respBody interface{}) error {
	var bodyReader io.Reader
	hasBody := reqBody != nil
	if hasBody {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshaling request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setHeaders(httpReq, hasBody)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.parseError(resp)
	}

	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}

	return nil
}

func (c *Client) setHeaders(req *http.Request, hasBody bool) {
	req.Header.Set("x-goog-api-key", c.apiKey)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
}

func (c *Client) parseError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if err != nil {
		return fmt.Errorf("API error (status %d, failed to read body: %w)", resp.StatusCode, err)
	}

	var apiErr APIError
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
		return fmt.Errorf("API error %d (%s): %s", apiErr.Error.Code, apiErr.Error.Status, apiErr.Error.Message)
	}

	return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
}
