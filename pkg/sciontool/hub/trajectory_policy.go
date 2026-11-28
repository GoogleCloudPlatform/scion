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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

const maxAgentPolicyResponseBytes = 1 << 20

// GetTrajectoryCapturePolicy fetches this agent's effective Track 2 trajectory
// capture policy from GET /api/v1/agents/{id}. Returns (nil, nil) when the
// client is not configured or when the Hub omits trajectory_capture (experiment
// off). Callers should pass a bounded context so startup never blocks on Hub
// latency.
func (c *Client) GetTrajectoryCapturePolicy(ctx context.Context) (*telemetrycontract.TrajectoryCapturePolicyStatus, error) {
	if c == nil || !c.IsConfigured() {
		return nil, nil
	}
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s", c.hubURL, url.PathEscape(c.agentID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create trajectory policy request: %w", err)
	}
	c.setAgentAuth(req.Header, c.AuthToken())

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch trajectory policy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentPolicyResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read trajectory policy response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var payload struct {
		TrajectoryCapture *telemetrycontract.TrajectoryCapturePolicyStatus `json:"trajectory_capture,omitempty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse trajectory policy response: %w", err)
	}
	return payload.TrajectoryCapture, nil
}
