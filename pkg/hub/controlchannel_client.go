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
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/google/uuid"
)

// maxControlChannelBodySize is the maximum body size (in bytes) that can be
// sent through the WebSocket control channel without exceeding the broker's
// read limit. The RequestEnvelope.Body field is base64-encoded in JSON, adding
// ~33% overhead, so the safe body threshold for a 1MB wire limit is ~768KB.
// Requests exceeding this limit are rejected before encoding so callers
// (e.g. HybridBrokerClient) can detect ErrPayloadTooLarge and fall back to
// direct HTTP (issue #165).
const maxControlChannelBodySize = 768 * 1024 // 768KB → ~1MB on wire after base64

// ErrPayloadTooLarge is returned by the control channel client when a request
// body exceeds maxControlChannelBodySize. Callers should use errors.As to
// detect this condition and fall back to direct HTTP rather than propagating
// the error.
type ErrPayloadTooLarge struct {
	Method string
	Path   string
	Size   int
	Limit  int
}

func (e *ErrPayloadTooLarge) Error() string {
	return fmt.Sprintf(
		"control channel payload too large: %s %s body is %d bytes (limit %d); "+
			"use direct HTTP to reach this broker instead (issue #165)",
		e.Method, e.Path, e.Size, e.Limit,
	)
}

// ControlChannelBrokerClient implements RuntimeBrokerClient by tunneling requests
// through the control channel WebSocket connection.
type ControlChannelBrokerClient struct {
	manager controlChannelTunnel
	debug   bool
	signer  brokerRequestSigner
}

type controlChannelTunnel interface {
	IsConnected(brokerID string) bool
	TunnelRequest(ctx context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error)
}

// NewControlChannelBrokerClient creates a new control channel broker client.
func NewControlChannelBrokerClient(manager *ControlChannelManager, signer brokerRequestSigner, debug bool) *ControlChannelBrokerClient {
	return &ControlChannelBrokerClient{
		manager: manager,
		debug:   debug,
		signer:  signer,
	}
}

// CreateAgent creates an agent via control channel.
func (c *ControlChannelBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	_ = brokerEndpoint // Unused - we tunnel through control channel

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, brokerID, "POST", "/api/v1/agents", "", body)
	if err != nil {
		return nil, err
	}

	var result RemoteAgentResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// StartAgent starts an agent via control channel.
func (c *ControlChannelBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool) (*RemoteAgentResponse, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/start", url.PathEscape(agentID))
	if projectID != "" {
		path += "?projectId=" + url.QueryEscape(projectID)
	}

	payload := map[string]interface{}{}
	if task != "" {
		payload["task"] = task
	}
	if projectPath != "" {
		payload["projectPath"] = projectPath
	}
	if projectSlug != "" {
		payload["projectSlug"] = projectSlug
	}
	if harnessConfig != "" {
		payload["harnessConfig"] = harnessConfig
	}
	if len(resolvedEnv) > 0 {
		payload["resolvedEnv"] = resolvedEnv
	}
	if len(resolvedSecrets) > 0 {
		payload["resolvedSecrets"] = resolvedSecrets
	}
	if inlineConfig != nil {
		payload["inlineConfig"] = inlineConfig
	}
	if len(sharedDirs) > 0 {
		payload["sharedDirs"] = sharedDirs
	}
	if sharedWorkspace {
		payload["sharedWorkspace"] = true
	}
	if resume {
		payload["resume"] = true
	}

	var body []byte
	if len(payload) > 0 {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
	}

	resp, err := c.doRequest(ctx, brokerID, "POST", path, "", body)
	if err != nil {
		return nil, err
	}

	var result RemoteAgentResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, nil
	}

	return &result, nil
}

// StopAgent stops an agent via control channel.
func (c *ControlChannelBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/stop", url.PathEscape(agentID))
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}
	_, err := c.doRequest(ctx, brokerID, "POST", path, query, nil)
	return err
}

// RestartAgent restarts an agent via control channel.
func (c *ControlChannelBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/restart", url.PathEscape(agentID))
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}
	var body []byte
	if len(resolvedEnv) > 0 {
		payload := map[string]interface{}{
			"resolvedEnv": resolvedEnv,
		}
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to marshal restart request: %w", err)
		}
	}
	_, err := c.doRequest(ctx, brokerID, "POST", path, query, body)
	return err
}

// ResetAuthAgent injects a fresh auth token into a running agent via the control channel.
func (c *ControlChannelBrokerClient) ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/reset-auth", url.PathEscape(agentID))
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return fmt.Errorf("failed to marshal reset-auth request: %w", err)
	}
	_, err = c.doRequest(ctx, brokerID, "POST", path, query, body)
	return err
}

// DeleteAgent deletes an agent via control channel.
func (c *ControlChannelBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s", url.PathEscape(agentID))
	query := fmt.Sprintf("deleteFiles=%t&removeBranch=%t", deleteFiles, removeBranch)
	if projectID != "" {
		query += "&projectId=" + url.QueryEscape(projectID)
	}
	if softDelete {
		query += fmt.Sprintf("&softDelete=true&deletedAt=%s", url.QueryEscape(deletedAt.Format(time.RFC3339)))
	}
	resp, err := c.doRequest(ctx, brokerID, "DELETE", path, query, nil)
	if err != nil {
		return err
	}
	// Allow 404 for idempotent delete
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return nil
}

// MessageAgent sends a message to an agent via control channel.
func (c *ControlChannelBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/message", url.PathEscape(agentID))
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}

	// Build the request body with structured message if available
	reqBody := map[string]interface{}{
		"interrupt": interrupt,
	}
	if projectID != "" {
		reqBody["project_id"] = projectID
	}
	if structuredMsg != nil {
		reqBody["structured_message"] = structuredMsg
	} else {
		reqBody["message"] = message
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	_, err = c.doRequest(ctx, brokerID, "POST", path, query, body)
	return err
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file via control channel.
func (c *ControlChannelBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/has-prompt", url.PathEscape(agentID))
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}

	resp, err := c.doRequest(ctx, brokerID, "POST", path, query, nil)
	if err != nil {
		return false, err
	}

	var result HasPromptResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return false, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.HasPrompt, nil
}

// CreateAgentWithGather creates an agent and handles 202 env-gather responses via control channel.
func (c *ControlChannelBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	_ = brokerEndpoint

	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequestRaw(ctx, brokerID, "POST", "/api/v1/agents", "", body)
	if err != nil {
		return nil, nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(resp.Body))
	}

	if resp.StatusCode == http.StatusAccepted {
		var envReqs RemoteEnvRequirementsResponse
		if err := json.Unmarshal(resp.Body, &envReqs); err != nil {
			return nil, nil, fmt.Errorf("failed to decode env requirements: %w", err)
		}
		return nil, &envReqs, nil
	}

	var result RemoteAgentResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil, nil
}

// GetAgentLogs retrieves agent.log content from a remote runtime broker via control channel.
func (c *ControlChannelBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/logs", url.PathEscape(agentID))
	query := ""
	if tail > 0 {
		query = fmt.Sprintf("tail=%d", tail)
	}
	if projectID != "" {
		if query != "" {
			query += "&"
		}
		query += "projectId=" + url.QueryEscape(projectID)
	}
	resp, err := c.doRequest(ctx, brokerID, "GET", path, query, nil)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// ExecAgent executes a command in an agent via control channel.
func (c *ControlChannelBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/exec", url.PathEscape(agentID))
	query := ""
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}

	body, err := json.Marshal(map[string]interface{}{
		"command": command,
		"timeout": timeout,
	})
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, brokerID, "POST", path, query, body)
	if err != nil {
		return "", 0, err
	}

	var result struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %w", err)
	}
	return result.Output, result.ExitCode, nil
}

func (c *ControlChannelBrokerClient) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/projects/%s", url.PathEscape(projectSlug))
	if projectID != "" {
		path += "?project_id=" + url.QueryEscape(projectID)
	}
	resp, err := c.doRequest(ctx, brokerID, "DELETE", path, "", nil)
	if err != nil {
		return err
	}
	// Allow 404 for idempotent cleanup
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return nil
}

// ImageStatus queries a broker's local image state via control channel.
func (c *ControlChannelBrokerClient) ImageStatus(ctx context.Context, brokerID, shortImage, longImage string) (*BrokerImageStatusResponse, error) {
	query := url.Values{}
	if shortImage != "" {
		query.Set("short", shortImage)
	}
	if longImage != "" {
		query.Set("long", longImage)
	}

	resp, err := c.doRequestRaw(ctx, brokerID, "GET", "/api/v1/images/status", query.Encode(), nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, &BrokerUnsupportedError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(resp.Body))
	}

	var result BrokerImageStatusResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode image status response: %w", err)
	}
	return &result, nil
}

// PullImage asks a broker to pull an image via control channel.
// Note: inherits the tunnel's RequestTimeout (default 120s). Large image pulls
// may exceed this timeout and report an error even though the pull continues
// broker-side. A future async-pull mechanism would address this limitation.
func (c *ControlChannelBrokerClient) PullImage(ctx context.Context, brokerID, image string) error {
	body, err := json.Marshal(map[string]string{"image": image})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	_, err = c.doRequest(ctx, brokerID, "POST", "/api/v1/images/pull", "", body)
	return err
}

// DeleteImage asks a broker to remove a local image via control channel.
func (c *ControlChannelBrokerClient) DeleteImage(ctx context.Context, brokerID, image string) error {
	body, err := json.Marshal(map[string]string{"image": image})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	_, err = c.doRequest(ctx, brokerID, "DELETE", "/api/v1/images/local", "", body)
	return err
}

// checkBodySize returns an *ErrPayloadTooLarge if the body is too large to
// tunnel safely through the control channel WebSocket without exceeding the
// broker's read limit. The Body field is base64-encoded in the RequestEnvelope
// JSON, so the actual wire size is roughly len(body)*4/3 plus envelope
// metadata. Callers should use errors.As(err, &ErrPayloadTooLarge{}) to detect
// this condition and fall back to direct HTTP.
func checkBodySize(method, path string, body []byte) error {
	if len(body) > maxControlChannelBodySize {
		return &ErrPayloadTooLarge{
			Method: method,
			Path:   path,
			Size:   len(body),
			Limit:  maxControlChannelBodySize,
		}
	}
	return nil
}

// doRequestRaw tunnels an HTTP request through the control channel without
// treating non-2xx status codes as errors. Callers are responsible for
// inspecting resp.StatusCode themselves.
func (c *ControlChannelBrokerClient) doRequestRaw(ctx context.Context, brokerID, method, path, query string, body []byte) (*wsprotocol.ResponseEnvelope, error) {
	if !c.manager.IsConnected(brokerID) {
		return nil, fmt.Errorf("broker %s not connected via control channel", brokerID)
	}

	if err := checkBodySize(method, path, body); err != nil {
		return nil, err
	}

	headers, err := c.buildRequestHeaders(ctx, brokerID, method, path, query, body)
	if err != nil {
		return nil, err
	}

	req := wsprotocol.NewRequestEnvelope(uuid.New().String(), method, path, query, headers, body)
	resp, err := c.manager.TunnelRequest(ctx, brokerID, req)
	if err != nil {
		return nil, fmt.Errorf("control channel request failed: %w", err)
	}

	return resp, nil
}

// doRequest tunnels an HTTP request through the control channel.
func (c *ControlChannelBrokerClient) doRequest(ctx context.Context, brokerID, method, path, query string, body []byte) (*wsprotocol.ResponseEnvelope, error) {
	if !c.manager.IsConnected(brokerID) {
		return nil, fmt.Errorf("broker %s not connected via control channel", brokerID)
	}

	if err := checkBodySize(method, path, body); err != nil {
		return nil, err
	}

	headers, err := c.buildRequestHeaders(ctx, brokerID, method, path, query, body)
	if err != nil {
		return nil, err
	}

	req := wsprotocol.NewRequestEnvelope(uuid.New().String(), method, path, query, headers, body)
	resp, err := c.manager.TunnelRequest(ctx, brokerID, req)
	if err != nil {
		return nil, fmt.Errorf("control channel request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(resp.Body))
	}

	return resp, nil
}

func (c *ControlChannelBrokerClient) buildRequestHeaders(ctx context.Context, brokerID, method, path, query string, body []byte) (map[string]string, error) {
	headers := map[string]string{
		"Content-Type": "application/json",
	}

	if c.signer == nil {
		return headers, nil
	}

	tunnelURL := "http://runtime-broker" + path
	if query != "" {
		tunnelURL += "?" + query
	}

	var requestBody io.Reader
	if len(body) > 0 {
		requestBody = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, tunnelURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to build control channel request for signing: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if err := c.signer.Sign(ctx, httpReq, brokerID); err != nil {
		return nil, fmt.Errorf("failed to sign control channel request: %w", err)
	}

	for key := range httpReq.Header {
		headers[key] = httpReq.Header.Get(key)
	}

	return headers, nil
}

// HybridBrokerClient tries control channel first, falls back to HTTP.
type HybridBrokerClient struct {
	controlChannel *ControlChannelBrokerClient
	httpClient     RuntimeBrokerClient
	debug          bool
	// statelessLocalBrokers identifies co-located broker identities that are
	// safe to serve from any replica through the local broker HTTP endpoint.
	// Store affinity for these IDs is replica health, not lifecycle ownership.
	statelessLocalBrokers map[string]struct{}
	// affinity returns the believed owning hub instanceID for a broker and
	// whether that owner is alive (last_heartbeat fresh). It is a routing HINT
	// only (correctness comes from durable intent + drain); injected so route()
	// is unit-testable. Nil means "no affinity info" (treated as no owner).
	affinity func(ctx context.Context, brokerID string) (owner string, alive bool)
}

// NewHybridBrokerClient creates a hybrid client that prefers control channel.
func NewHybridBrokerClient(manager *ControlChannelManager, httpClient RuntimeBrokerClient, signer brokerRequestSigner, debug bool) *HybridBrokerClient {
	return &HybridBrokerClient{
		controlChannel: NewControlChannelBrokerClient(manager, signer, debug),
		httpClient:     httpClient,
		debug:          debug,
	}
}

// useControlChannel returns true if we should use control channel for this broker.
func (c *HybridBrokerClient) useControlChannel(brokerID string) bool {
	if c.isStatelessLocalBroker(brokerID) {
		return false
	}
	return c.controlChannel.manager.IsConnected(brokerID)
}

// CreateAgent creates an agent, preferring control channel.
// If the control channel rejects the payload as too large (ErrPayloadTooLarge),
// it falls back to direct HTTP so the agent creation can still succeed.
func (c *HybridBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	if c.useControlChannel(brokerID) {
		resp, err := c.controlChannel.CreateAgent(ctx, brokerID, brokerEndpoint, req)
		if err == nil {
			return resp, nil
		}
		var payloadErr *ErrPayloadTooLarge
		if errors.As(err, &payloadErr) {
			// Payload too large for the WebSocket channel — fall back to HTTP.
			return c.httpClient.CreateAgent(ctx, brokerID, brokerEndpoint, req)
		}
		return nil, err
	}
	return c.httpClient.CreateAgent(ctx, brokerID, brokerEndpoint, req)
}

// StartAgent starts an agent, using route() to decide the delivery path.
// routeLocal uses the control-channel tunnel (unchanged fast path), routeHTTP
// falls back to the broker's HTTP endpoint, and routeForward/routeUndeliverable
// return ErrLifecycleDeferred so the caller can write durable intent + wait.
func (c *HybridBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool) (*RemoteAgentResponse, error) {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume)
	case routeHTTP:
		return c.httpClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume)
	default:
		return nil, ErrLifecycleDeferred
	}
}

// StopAgent stops an agent, using route() to decide the delivery path.
// routeLocal uses the control-channel tunnel, routeHTTP falls back to HTTP,
// and routeForward/routeUndeliverable return ErrLifecycleDeferred.
func (c *HybridBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.StopAgent(ctx, brokerID, brokerEndpoint, agentID, projectID)
	case routeHTTP:
		return c.httpClient.StopAgent(ctx, brokerID, brokerEndpoint, agentID, projectID)
	default:
		return ErrLifecycleDeferred
	}
}

// RestartAgent restarts an agent, using route() to decide the delivery path.
// routeLocal uses the control-channel tunnel, routeHTTP falls back to HTTP,
// and routeForward/routeUndeliverable return ErrLifecycleDeferred.
func (c *HybridBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv)
	case routeHTTP:
		return c.httpClient.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv)
	default:
		return ErrLifecycleDeferred
	}
}

// ResetAuthAgent injects a fresh auth token into a running agent, using route()
// to decide the delivery path.
func (c *HybridBrokerClient) ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token string) error {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.ResetAuthAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, token)
	case routeHTTP:
		return c.httpClient.ResetAuthAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, token)
	default:
		return ErrLifecycleDeferred
	}
}

// DeleteAgent deletes an agent, using route() to decide the delivery path.
// routeLocal uses the control-channel tunnel, routeHTTP falls back to HTTP,
// and routeForward/routeUndeliverable return ErrLifecycleDeferred.
func (c *HybridBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, deleteFiles, removeBranch, softDelete, deletedAt)
	case routeHTTP:
		return c.httpClient.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, deleteFiles, removeBranch, softDelete, deletedAt)
	default:
		return ErrLifecycleDeferred
	}
}

// MessageAgent sends a message to an agent, using route() to decide the
// delivery path (B3-2). routeLocal uses the control-channel tunnel (unchanged
// fast path), routeHTTP falls back to the broker's HTTP endpoint, and
// routeForward/routeUndeliverable return ErrMessageDeferred so the caller
// can emit a NOTIFY wakeup and return 202 (the message row is durable).
func (c *HybridBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, message, interrupt, structuredMsg)
	case routeHTTP:
		return c.httpClient.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, message, interrupt, structuredMsg)
	default:
		return ErrMessageDeferred
	}
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file, using
// route() to decide the delivery path. routeLocal uses the control-channel
// tunnel, routeHTTP falls back to HTTP, and routeForward/routeUndeliverable
// return ErrLifecycleDeferred so the caller can write durable intent + wait.
func (c *HybridBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		return c.controlChannel.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID, projectID)
	case routeHTTP:
		return c.httpClient.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID, projectID)
	default:
		return false, ErrLifecycleDeferred
	}
}

// CreateAgentWithGather creates an agent with env-gather support, using route()
// to decide the delivery path. routeLocal uses the control-channel tunnel,
// routeHTTP falls back to HTTP, and routeForward/routeUndeliverable return
// ErrLifecycleDeferred so the caller can write durable intent + wait.
// If the control channel rejects the payload as too large (ErrPayloadTooLarge),
// it falls back to direct HTTP regardless of the routing decision.
func (c *HybridBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal:
		resp, envReqs, err := c.controlChannel.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
		if err == nil {
			return resp, envReqs, nil
		}
		var payloadErr *ErrPayloadTooLarge
		if errors.As(err, &payloadErr) {
			// Payload too large for the WebSocket channel — fall back to HTTP.
			return c.httpClient.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
		}
		return nil, nil, err
	case routeHTTP:
		return c.httpClient.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
	default:
		return nil, nil, ErrLifecycleDeferred
	}
}

// GetAgentLogs retrieves agent.log content, preferring control channel.
func (c *HybridBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, projectID, tail)
	}
	return c.httpClient.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, projectID, tail)
}

// ExecAgent executes a command in an agent, preferring control channel.
func (c *HybridBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, command, timeout)
	}
	return c.httpClient.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, command, timeout)
}

func (c *HybridBrokerClient) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.CleanupProject(ctx, brokerID, brokerEndpoint, projectSlug, projectID)
	}
	return c.httpClient.CleanupProject(ctx, brokerID, brokerEndpoint, projectSlug, projectID)
}

// brokerImageClient is an optional interface implemented by RuntimeBrokerClient
// implementations that support image operations.
type brokerImageClient interface {
	ImageStatus(ctx context.Context, brokerID, brokerEndpoint, shortImage, longImage string) (*BrokerImageStatusResponse, error)
	PullImage(ctx context.Context, brokerID, brokerEndpoint, image string) error
	DeleteImage(ctx context.Context, brokerID, brokerEndpoint, image string) error
}

// ImageStatus queries a broker's local image state, preferring control channel.
func (c *HybridBrokerClient) ImageStatus(ctx context.Context, brokerID, brokerEndpoint, shortImage, longImage string) (*BrokerImageStatusResponse, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.ImageStatus(ctx, brokerID, shortImage, longImage)
	}
	if ic, ok := c.httpClient.(brokerImageClient); ok {
		return ic.ImageStatus(ctx, brokerID, brokerEndpoint, shortImage, longImage)
	}
	return nil, fmt.Errorf("HTTP client does not support image status queries")
}

// PullImage asks a broker to pull an image, preferring control channel.
func (c *HybridBrokerClient) PullImage(ctx context.Context, brokerID, brokerEndpoint, image string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.PullImage(ctx, brokerID, image)
	}
	if ic, ok := c.httpClient.(brokerImageClient); ok {
		return ic.PullImage(ctx, brokerID, brokerEndpoint, image)
	}
	return fmt.Errorf("HTTP client does not support image pull")
}

// DeleteImage asks a broker to remove a local image, preferring control channel.
func (c *HybridBrokerClient) DeleteImage(ctx context.Context, brokerID, brokerEndpoint, image string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.DeleteImage(ctx, brokerID, image)
	}
	if ic, ok := c.httpClient.(brokerImageClient); ok {
		return ic.DeleteImage(ctx, brokerID, brokerEndpoint, image)
	}
	return fmt.Errorf("HTTP client does not support image delete")
}
