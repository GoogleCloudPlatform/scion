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
	"net/http"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/wsprotocol"
)

// ControlChannelBrokerClient implements RuntimeBrokerClient by tunneling requests
// through the control channel WebSocket connection.
type ControlChannelBrokerClient struct {
	manager *ControlChannelManager
	debug   bool
}

// NewControlChannelBrokerClient creates a new control channel broker client.
func NewControlChannelBrokerClient(manager *ControlChannelManager, debug bool) *ControlChannelBrokerClient {
	return &ControlChannelBrokerClient{
		manager: manager,
		debug:   debug,
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
func (c *ControlChannelBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/start", agentID)
	_, err := c.doRequest(ctx, brokerID, "POST", path, "", nil)
	return err
}

// StopAgent stops an agent via control channel.
func (c *ControlChannelBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/stop", agentID)
	_, err := c.doRequest(ctx, brokerID, "POST", path, "", nil)
	return err
}

// RestartAgent restarts an agent via control channel.
func (c *ControlChannelBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/restart", agentID)
	_, err := c.doRequest(ctx, brokerID, "POST", path, "", nil)
	return err
}

// DeleteAgent deletes an agent via control channel.
func (c *ControlChannelBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s", agentID)
	query := fmt.Sprintf("deleteFiles=%t&removeBranch=%t", deleteFiles, removeBranch)
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
func (c *ControlChannelBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/message", agentID)

	body, err := json.Marshal(map[string]interface{}{
		"message":   message,
		"interrupt": interrupt,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	_, err = c.doRequest(ctx, brokerID, "POST", path, "", body)
	return err
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file via control channel.
func (c *ControlChannelBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error) {
	_ = brokerEndpoint
	path := fmt.Sprintf("/api/v1/agents/%s/has-prompt", agentID)

	resp, err := c.doRequest(ctx, brokerID, "POST", path, "", nil)
	if err != nil {
		return false, err
	}

	var result HasPromptResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return false, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.HasPrompt, nil
}

// doRequest tunnels an HTTP request through the control channel.
func (c *ControlChannelBrokerClient) doRequest(ctx context.Context, brokerID, method, path, query string, body []byte) (*wsprotocol.ResponseEnvelope, error) {
	if !c.manager.IsConnected(brokerID) {
		return nil, fmt.Errorf("broker %s not connected via control channel", brokerID)
	}

	headers := map[string]string{
		"Content-Type": "application/json",
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

// HybridBrokerClient tries control channel first, falls back to HTTP.
type HybridBrokerClient struct {
	controlChannel *ControlChannelBrokerClient
	httpClient     RuntimeBrokerClient
	debug          bool
}

// NewHybridBrokerClient creates a hybrid client that prefers control channel.
func NewHybridBrokerClient(manager *ControlChannelManager, httpClient RuntimeBrokerClient, debug bool) *HybridBrokerClient {
	return &HybridBrokerClient{
		controlChannel: NewControlChannelBrokerClient(manager, debug),
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
func (c *HybridBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.StartAgent(ctx, brokerID, brokerEndpoint, agentID)
	}
	return c.httpClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID)
}

// StopAgent stops an agent, preferring control channel.
func (c *HybridBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.StopAgent(ctx, brokerID, brokerEndpoint, agentID)
	}
	return c.httpClient.StopAgent(ctx, brokerID, brokerEndpoint, agentID)
}

// RestartAgent restarts an agent, preferring control channel.
func (c *HybridBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.RestartAgent(ctx, brokerID, brokerEndpoint, agentID)
	}
	return c.httpClient.RestartAgent(ctx, brokerID, brokerEndpoint, agentID)
}

// DeleteAgent deletes an agent, preferring control channel.
func (c *HybridBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, deleteFiles, removeBranch)
	}
	return c.httpClient.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, deleteFiles, removeBranch)
}

// MessageAgent sends a message to an agent, preferring control channel.
func (c *HybridBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, message, interrupt)
	}
	return c.httpClient.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, message, interrupt)
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
func (c *HybridBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error) {
	if c.useControlChannel(brokerID) {
		return c.controlChannel.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID)
	}
	return c.httpClient.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID)
}
