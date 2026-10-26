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

package delegation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// audiencePrefix and bareAudience form the hub's delegated-credential
	// audience: "scion-hub:<hub ID>", or "scion-hub" for a hub without an
	// ID (.design/agent-delegation.md §8.2 step 11).
	audiencePrefix = "scion-hub:"
	bareAudience   = "scion-hub"

	// healthzTimeout bounds the /healthz request.
	healthzTimeout = 10 * time.Second
)

// resolveAudience returns the hub's delegated-credential audience: the
// configured one, or the one resolved earlier, or one resolved now from
// the hub's public GET /healthz (its top-level hub_id, or hub.hub_id of a
// combined server's response). A hub health answer (one with a status)
// without a hub ID means a hub without an ID. /healthz is sent no credential.
func (h *Helper) resolveAudience(ctx context.Context) (string, error) {
	h.mu.Lock()
	aud := h.audience
	h.mu.Unlock()
	if aud != "" {
		return aud, nil
	}

	ctx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(h.client.HubURL(), "/") + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve hub audience: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("resolve hub audience: %w", err)
	}
	var health struct {
		Status string `json:"status"`
		HubID  string `json:"hub_id"`
		Hub    *struct {
			HubID string `json:"hub_id"`
		} `json:"hub"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		// /healthz is public and carries no credential, so its parse error
		// may be wrapped.
		return "", fmt.Errorf("resolve hub audience: /healthz did not answer JSON: %w", err)
	}
	id := strings.TrimSpace(health.HubID)
	if id == "" && health.Hub != nil {
		id = strings.TrimSpace(health.Hub.HubID)
	}
	if id == "" && health.Status == "" {
		// Not a hub health answer (for example a proxy's error body).
		return "", fmt.Errorf("resolve hub audience: /healthz (HTTP %d) is not a hub health answer", resp.StatusCode)
	}
	aud = bareAudience
	if id != "" {
		aud = audiencePrefix + id
	}
	h.mu.Lock()
	h.audience = aud
	h.mu.Unlock()
	return aud, nil
}
