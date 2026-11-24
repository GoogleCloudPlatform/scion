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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// moveBundleBrokerClient is the optional broker-client interface for the
// portable move-bundle endpoints (design §3.4.1, §3.5). Kept separate from
// RuntimeBrokerClient so existing test doubles of RuntimeBrokerClient are not
// broken; every production broker client implements it (verified by the
// compile-time assertions below).
type moveBundleBrokerClient interface {
	PrecheckMoveBundle(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundlePrecheckRequest) (*api.MoveBundlePrecheckResponse, error)
	StartMoveBundleExport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleExportRequest) (*api.MoveBundleJobStatus, error)
	GetMoveBundleExportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error)
	StartMoveBundleImport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleImportRequest) (*api.MoveBundleJobStatus, error)
	GetMoveBundleImportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error)
}

var (
	_ moveBundleBrokerClient = (*HTTPRuntimeBrokerClient)(nil)
	_ moveBundleBrokerClient = (*AuthenticatedBrokerClient)(nil)
	_ moveBundleBrokerClient = (*ControlChannelBrokerClient)(nil)
	_ moveBundleBrokerClient = (*HybridBrokerClient)(nil)
)

const maxMoveBundleBrokerResponseBytes = 8 << 20

func moveBundleQuery(ctx context.Context, projectID string) string {
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}
	return withRecordedRuntimeQuery(ctx, query)
}

func moveBundleHTTPURL(brokerEndpoint, path, query string) string {
	u := strings.TrimSuffix(brokerEndpoint, "/") + path
	if query != "" {
		u += "?" + query
	}
	return u
}

func decodeMoveBundleJSON(brokerID string, statusCode int, body []byte, retryAfter string, dst any) error {
	if statusCode >= 400 {
		return newBrokerStatusError(brokerID, statusCode, body, retryAfter)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("failed to decode move-bundle response: %w", err)
	}
	return nil
}

// --- brokerHTTPTransport ---

func (t *brokerHTTPTransport) doMoveBundleJSON(ctx context.Context, brokerID, brokerEndpoint, method, path, query string, reqBody any, respOut any) error {
	var body []byte
	if reqBody != nil {
		var err error
		body, err = json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal move-bundle request: %w (%w)", err, errStartRequestNotSent)
		}
	}
	resp, err := t.doRequest(ctx, brokerID, method, moveBundleHTTPURL(brokerEndpoint, path, query), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxMoveBundleBrokerResponseBytes))
	if err != nil {
		return fmt.Errorf("failed to read move-bundle response: %w", err)
	}
	return decodeMoveBundleJSON(brokerID, resp.StatusCode, respBody, resp.Header.Get("Retry-After"), respOut)
}

func (t *brokerHTTPTransport) PrecheckMoveBundle(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundlePrecheckRequest) (*api.MoveBundlePrecheckResponse, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/precheck", url.PathEscape(agentSlug))
	var out api.MoveBundlePrecheckResponse
	if err := t.doMoveBundleJSON(ctx, brokerID, brokerEndpoint, http.MethodPost, path, moveBundleQuery(ctx, projectID), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (t *brokerHTTPTransport) StartMoveBundleExport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleExportRequest) (*api.MoveBundleJobStatus, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export", url.PathEscape(agentSlug))
	var out api.MoveBundleJobStatus
	if err := t.doMoveBundleJSON(ctx, brokerID, brokerEndpoint, http.MethodPost, path, moveBundleQuery(ctx, projectID), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (t *brokerHTTPTransport) GetMoveBundleExportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export/%s", url.PathEscape(agentSlug), url.PathEscape(reincarnationID))
	var out api.MoveBundleJobStatus
	if err := t.doMoveBundleJSON(ctx, brokerID, brokerEndpoint, http.MethodGet, path, moveBundleQuery(ctx, projectID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (t *brokerHTTPTransport) StartMoveBundleImport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleImportRequest) (*api.MoveBundleJobStatus, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/import", url.PathEscape(agentSlug))
	var out api.MoveBundleJobStatus
	if err := t.doMoveBundleJSON(ctx, brokerID, brokerEndpoint, http.MethodPost, path, moveBundleQuery(ctx, projectID), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (t *brokerHTTPTransport) GetMoveBundleImportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/import/%s", url.PathEscape(agentSlug), url.PathEscape(reincarnationID))
	var out api.MoveBundleJobStatus
	if err := t.doMoveBundleJSON(ctx, brokerID, brokerEndpoint, http.MethodGet, path, moveBundleQuery(ctx, projectID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- HTTPRuntimeBrokerClient ---

func (c *HTTPRuntimeBrokerClient) PrecheckMoveBundle(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundlePrecheckRequest) (*api.MoveBundlePrecheckResponse, error) {
	return c.transport.PrecheckMoveBundle(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *HTTPRuntimeBrokerClient) StartMoveBundleExport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleExportRequest) (*api.MoveBundleJobStatus, error) {
	return c.transport.StartMoveBundleExport(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *HTTPRuntimeBrokerClient) GetMoveBundleExportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	return c.transport.GetMoveBundleExportStatus(ctx, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID)
}

func (c *HTTPRuntimeBrokerClient) StartMoveBundleImport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleImportRequest) (*api.MoveBundleJobStatus, error) {
	return c.transport.StartMoveBundleImport(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *HTTPRuntimeBrokerClient) GetMoveBundleImportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	return c.transport.GetMoveBundleImportStatus(ctx, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID)
}

// --- AuthenticatedBrokerClient ---

func (c *AuthenticatedBrokerClient) PrecheckMoveBundle(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundlePrecheckRequest) (*api.MoveBundlePrecheckResponse, error) {
	return c.transport.PrecheckMoveBundle(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *AuthenticatedBrokerClient) StartMoveBundleExport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleExportRequest) (*api.MoveBundleJobStatus, error) {
	return c.transport.StartMoveBundleExport(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *AuthenticatedBrokerClient) GetMoveBundleExportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	return c.transport.GetMoveBundleExportStatus(ctx, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID)
}

func (c *AuthenticatedBrokerClient) StartMoveBundleImport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleImportRequest) (*api.MoveBundleJobStatus, error) {
	return c.transport.StartMoveBundleImport(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *AuthenticatedBrokerClient) GetMoveBundleImportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	return c.transport.GetMoveBundleImportStatus(ctx, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID)
}

// --- ControlChannelBrokerClient ---

func (c *ControlChannelBrokerClient) doMoveBundleJSON(ctx context.Context, brokerID, method, path, query string, reqBody any, respOut any) error {
	var body []byte
	if reqBody != nil {
		var err error
		body, err = json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal move-bundle request: %w (%w)", err, errStartRequestNotSent)
		}
	}
	resp, err := c.doRequestRaw(ctx, brokerID, method, path, query, body)
	if err != nil {
		return err
	}
	return decodeMoveBundleJSON(brokerID, resp.StatusCode, resp.Body, resp.Headers["Retry-After"], respOut)
}

func (c *ControlChannelBrokerClient) PrecheckMoveBundle(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundlePrecheckRequest) (*api.MoveBundlePrecheckResponse, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/precheck", url.PathEscape(agentSlug))
	var out api.MoveBundlePrecheckResponse
	if err := c.doMoveBundleJSON(ctx, brokerID, http.MethodPost, path, moveBundleQuery(ctx, projectID), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *ControlChannelBrokerClient) StartMoveBundleExport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleExportRequest) (*api.MoveBundleJobStatus, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export", url.PathEscape(agentSlug))
	var out api.MoveBundleJobStatus
	if err := c.doMoveBundleJSON(ctx, brokerID, http.MethodPost, path, moveBundleQuery(ctx, projectID), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *ControlChannelBrokerClient) GetMoveBundleExportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/export/%s", url.PathEscape(agentSlug), url.PathEscape(reincarnationID))
	var out api.MoveBundleJobStatus
	if err := c.doMoveBundleJSON(ctx, brokerID, http.MethodGet, path, moveBundleQuery(ctx, projectID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *ControlChannelBrokerClient) StartMoveBundleImport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleImportRequest) (*api.MoveBundleJobStatus, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/import", url.PathEscape(agentSlug))
	var out api.MoveBundleJobStatus
	if err := c.doMoveBundleJSON(ctx, brokerID, http.MethodPost, path, moveBundleQuery(ctx, projectID), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *ControlChannelBrokerClient) GetMoveBundleImportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/move-bundle/import/%s", url.PathEscape(agentSlug), url.PathEscape(reincarnationID))
	var out api.MoveBundleJobStatus
	if err := c.doMoveBundleJSON(ctx, brokerID, http.MethodGet, path, moveBundleQuery(ctx, projectID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- HybridBrokerClient ---

func (c *HybridBrokerClient) moveBundleClient(ctx context.Context, brokerID, brokerEndpoint string) (moveBundleBrokerClient, error) {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel, nil
	case routeHTTP:
		mbHTTP, ok := c.httpClient.(moveBundleBrokerClient)
		if !ok {
			return nil, fmt.Errorf("HTTP broker client does not support move-bundle dispatch")
		}
		return mbHTTP, nil
	default:
		return nil, fmt.Errorf("no immediate route to broker %s for move-bundle: %w", brokerID, ErrLifecycleDeferred)
	}
}

func (c *HybridBrokerClient) PrecheckMoveBundle(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundlePrecheckRequest) (*api.MoveBundlePrecheckResponse, error) {
	bc, err := c.moveBundleClient(ctx, brokerID, brokerEndpoint)
	if err != nil {
		return nil, err
	}
	return bc.PrecheckMoveBundle(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *HybridBrokerClient) StartMoveBundleExport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleExportRequest) (*api.MoveBundleJobStatus, error) {
	bc, err := c.moveBundleClient(ctx, brokerID, brokerEndpoint)
	if err != nil {
		return nil, err
	}
	return bc.StartMoveBundleExport(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *HybridBrokerClient) GetMoveBundleExportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	bc, err := c.moveBundleClient(ctx, brokerID, brokerEndpoint)
	if err != nil {
		return nil, err
	}
	return bc.GetMoveBundleExportStatus(ctx, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID)
}

func (c *HybridBrokerClient) StartMoveBundleImport(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID string, req *api.MoveBundleImportRequest) (*api.MoveBundleJobStatus, error) {
	bc, err := c.moveBundleClient(ctx, brokerID, brokerEndpoint)
	if err != nil {
		return nil, err
	}
	return bc.StartMoveBundleImport(ctx, brokerID, brokerEndpoint, agentSlug, projectID, req)
}

func (c *HybridBrokerClient) GetMoveBundleImportStatus(ctx context.Context, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID string) (*api.MoveBundleJobStatus, error) {
	bc, err := c.moveBundleClient(ctx, brokerID, brokerEndpoint)
	if err != nil {
		return nil, err
	}
	return bc.GetMoveBundleImportStatus(ctx, brokerID, brokerEndpoint, agentSlug, projectID, reincarnationID)
}
