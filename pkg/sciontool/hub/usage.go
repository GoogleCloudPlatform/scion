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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// ErrUsageIngestUnsupported is returned by ProbeUsageCapabilities when the Hub
// responds with HTTP 404 to GET /api/v1/agents/{id}/usage/capabilities,
// indicating an older Hub or that the tokenomics.usage_ledger experiment is off.
var ErrUsageIngestUnsupported = errors.New("hub does not support usage ingest capabilities")

// UsageIngestHTTPError represents a non-2xx HTTP response from the Hub usage
// ingest endpoint.
type UsageIngestHTTPError struct {
	StatusCode int
	Body       string
}

func (e *UsageIngestHTTPError) Error() string {
	return fmt.Sprintf("hub usage ingest returned HTTP %d: %s", e.StatusCode, e.Body)
}

// IsPermanent reports whether retrying the exact same batch payload is futile.
// 401/403 (token may refresh), 404 (transient route/deploy), 408 (timeout),
// 429 (rate limit), and 5xx are treated as transient.
func (e *UsageIngestHTTPError) IsPermanent() bool {
	if e == nil {
		return false
	}
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// ProbeUsageCapabilities queries GET /api/v1/agents/{id}/usage/capabilities
// with the agent token to determine whether the Hub accepts usage event
// batches from sciontool. Returns an error wrapping ErrUsageIngestUnsupported
// on HTTP 404.
func (c *Client) ProbeUsageCapabilities(ctx context.Context) (*telemetrycontract.UsageCapabilitiesResponse, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("hub client not configured")
	}

	endpoint := strings.TrimSuffix(c.hubURL, "/") + telemetrycontract.UsageCapabilitiesAgentPath(c.agentID)

	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create capabilities request: %w", err)
	}
	c.setAgentAuth(req.Header, currentToken)

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send capabilities request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read capabilities response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("hub returned 404: %w", ErrUsageIngestUnsupported)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned error %d: %s", resp.StatusCode, string(respBody))
	}

	var caps telemetrycontract.UsageCapabilitiesResponse
	if err := json.Unmarshal(respBody, &caps); err != nil {
		return nil, fmt.Errorf("failed to parse capabilities response: %w", err)
	}
	return &caps, nil
}

// PostUsageEvents sends a usage ingest batch (or empty heartbeat) to
// POST /api/v1/agents/{id}/usage/events using the agent token. It makes a
// single HTTP attempt; the analytics drainer owns retry backoff.
func (c *Client) PostUsageEvents(ctx context.Context, payload telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error) {
	if !c.IsConfigured() {
		return nil, fmt.Errorf("hub client not configured")
	}

	endpoint := strings.TrimSuffix(c.hubURL, "/") + telemetrycontract.UsageIngestAgentPath(c.agentID)

	if payload.Events == nil {
		payload.Events = []telemetrycontract.UsageEvent{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal usage ingest payload: %w", err)
	}

	c.tokenMu.RLock()
	currentToken := c.token
	c.tokenMu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create usage ingest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAgentAuth(req.Header, currentToken)

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send usage ingest request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read usage ingest response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &UsageIngestHTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var result telemetrycontract.UsageIngestResponse
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("failed to parse usage ingest response: %w", err)
		}
	}
	return &result, nil
}
