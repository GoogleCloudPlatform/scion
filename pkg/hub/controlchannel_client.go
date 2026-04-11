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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/google/uuid"
)

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
func (c *ControlChannelBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, task, grovePath, groveSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir) (*RemoteAgentResponse, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/start", url.PathEscape(agentID))
	if groveID != "" {
		path += "?groveId=" + url.QueryEscape(groveID)
	}

	payload := map[string]interface{}{}
	if task != "" {
		payload["task"] = task
	}
	if grovePath != "" {
		payload["grovePath"] = grovePath
	}
	if groveSlug != "" {
		payload["groveSlug"] = groveSlug
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
func (c *ControlChannelBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/stop", url.PathEscape(agentID))
	query := ""
	if groveID != "" {
		query = "groveId=" + url.QueryEscape(groveID)
	}
	_, err := c.doRequest(ctx, brokerID, "POST", path, query, nil)
	return err
}

// RestartAgent restarts an agent via control channel.
func (c *ControlChannelBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, resolvedEnv map[string]string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/restart", url.PathEscape(agentID))
	query := ""
	if groveID != "" {
		query = "groveId=" + url.QueryEscape(groveID)
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

// DeleteAgent deletes an agent via control channel.
func (c *ControlChannelBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s", url.PathEscape(agentID))
	query := fmt.Sprintf("deleteFiles=%t&removeBranch=%t", deleteFiles, removeBranch)
	if groveID != "" {
		query += "&groveId=" + url.QueryEscape(groveID)
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
func (c *ControlChannelBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/message", url.PathEscape(agentID))
	query := ""
	if groveID != "" {
		query = "groveId=" + url.QueryEscape(groveID)
	}

	// Build the request body with structured message if available
	reqBody := map[string]interface{}{
		"interrupt": interrupt,
	}
	if groveID != "" {
		reqBody["grove_id"] = groveID
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
func (c *ControlChannelBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) (bool, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/has-prompt", url.PathEscape(agentID))
	query := ""
	if groveID != "" {
		query = "groveId=" + url.QueryEscape(groveID)
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
func (c *ControlChannelBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, tail int) (string, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/logs", url.PathEscape(agentID))
	query := ""
	if tail > 0 {
		query = fmt.Sprintf("tail=%d", tail)
	}
	if groveID != "" {
		if query != "" {
			query += "&"
		}
		query += "groveId=" + url.QueryEscape(groveID)
	}
	resp, err := c.doRequest(ctx, brokerID, "GET", path, query, nil)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// ExecAgent executes a command in an agent via control channel.
func (c *ControlChannelBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, command []string, timeout int) (string, int, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/exec", url.PathEscape(agentID))
	query := ""
	if groveID != "" {
		query = "groveId=" + url.QueryEscape(groveID)
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

func (c *ControlChannelBrokerClient) CleanupGrove(ctx context.Context, brokerID, brokerEndpoint, groveSlug string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/groves/%s", url.PathEscape(groveSlug))
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

// FinalizeEnv sends gathered env vars to a broker to complete agent creation via control channel.
func (c *ControlChannelBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/finalize-env", url.PathEscape(agentID))

	body, err := json.Marshal(map[string]interface{}{
		"env": env,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, brokerID, "POST", path, "", body)
	if err != nil {
		return nil, err
	}

	var result RemoteAgentResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// doRequestRaw tunnels an HTTP request through the control channel without
// treating non-2xx status codes as errors. This is needed for env-gather
// where 202 is a valid non-error response.
func (c *ControlChannelBrokerClient) doRequestRaw(ctx context.Context, brokerID, method, path, query string, body []byte) (*wsprotocol.ResponseEnvelope, error) {
	if !c.manager.IsConnected(brokerID) {
		return nil, fmt.Errorf("broker %s not connected via control channel", brokerID)
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

// doRequest tunnels an HTTP request through the control channel.
func (c *ControlChannelBrokerClient) doRequest(ctx context.Context, brokerID, method, path, query string, body []byte) (*wsprotocol.ResponseEnvelope, error) {
	if !c.manager.IsConnected(brokerID) {
		return nil, fmt.Errorf("broker %s not connected via control channel", brokerID)
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
	return c.controlChannel.manager.IsConnected(brokerID)
}

// CreateAgent creates an agent, preferring control channel.
func (c *HybridBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.CreateAgent(ctx, brokerID, brokerEndpoint, req)
	}
	return c.httpClient.CreateAgent(ctx, brokerID, brokerEndpoint, req)
}

// StartAgent starts an agent, preferring control channel.
func (c *HybridBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, task, grovePath, groveSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir) (*RemoteAgentResponse, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.StartAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, task, grovePath, groveSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs)
	}
	return c.httpClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, task, grovePath, groveSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs)
}

// StopAgent stops an agent, preferring control channel.
func (c *HybridBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.StopAgent(ctx, brokerID, brokerEndpoint, agentID, groveID)
	}
	return c.httpClient.StopAgent(ctx, brokerID, brokerEndpoint, agentID, groveID)
}

// RestartAgent restarts an agent, preferring control channel.
func (c *HybridBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, resolvedEnv map[string]string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, resolvedEnv)
	}
	return c.httpClient.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, resolvedEnv)
}

// DeleteAgent deletes an agent, preferring control channel.
func (c *HybridBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, deleteFiles, removeBranch, softDelete, deletedAt)
	}
	return c.httpClient.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, deleteFiles, removeBranch, softDelete, deletedAt)
}

// MessageAgent sends a message to an agent, preferring control channel.
func (c *HybridBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, message, interrupt, structuredMsg)
	}
	return c.httpClient.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, message, interrupt, structuredMsg)
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
func (c *HybridBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) (bool, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID, groveID)
	}
	return c.httpClient.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID, groveID)
}

// CreateAgentWithGather creates an agent with env-gather support, preferring control channel.
func (c *HybridBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
	}
	return c.httpClient.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
}

// GetAgentLogs retrieves agent.log content, preferring control channel.
func (c *HybridBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, tail int) (string, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, groveID, tail)
	}
	return c.httpClient.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, groveID, tail)
}

// ExecAgent executes a command in an agent, preferring control channel.
func (c *HybridBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, command []string, timeout int) (string, int, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, command, timeout)
	}
	return c.httpClient.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, groveID, command, timeout)
}

func (c *HybridBrokerClient) CleanupGrove(ctx context.Context, brokerID, brokerEndpoint, groveSlug string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.CleanupGrove(ctx, brokerID, brokerEndpoint, groveSlug)
	}
	return c.httpClient.CleanupGrove(ctx, brokerID, brokerEndpoint, groveSlug)
}

// FinalizeEnv sends gathered env vars to a broker, preferring control channel.
func (c *HybridBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.FinalizeEnv(ctx, brokerID, brokerEndpoint, agentID, env)
	}
	return c.httpClient.FinalizeEnv(ctx, brokerID, brokerEndpoint, agentID, env)
}
