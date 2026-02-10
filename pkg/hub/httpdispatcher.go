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

// Package hub provides the Scion Hub API server.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/store"
)

// HTTPRuntimeBrokerClient is an HTTP-based implementation of RuntimeBrokerClient.
// It communicates with remote runtime brokers via their REST API.
type HTTPRuntimeBrokerClient struct {
	client *http.Client
	debug  bool
}

// NewHTTPRuntimeBrokerClient creates a new HTTP runtime broker client.
func NewHTTPRuntimeBrokerClient() *HTTPRuntimeBrokerClient {
	return &HTTPRuntimeBrokerClient{
		client: &http.Client{
			Timeout: 120 * time.Second, // Agent creation can take a while
		},
	}
}

// NewHTTPRuntimeBrokerClientWithDebug creates a new HTTP runtime broker client with debug logging.
func NewHTTPRuntimeBrokerClientWithDebug(debug bool) *HTTPRuntimeBrokerClient {
	return &HTTPRuntimeBrokerClient{
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		debug: debug,
	}
}

// CreateAgent creates an agent on a remote runtime broker.
// Note: brokerID is unused in this unauthenticated client but is part of the
// RuntimeBrokerClient interface for compatibility with AuthenticatedBrokerClient.
func (c *HTTPRuntimeBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	var result RemoteAgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// StartAgent starts an agent on a remote runtime broker.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// StopAgent stops an agent on a remote runtime broker.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/stop", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// RestartAgent restarts an agent on a remote runtime broker.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/restart", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// DeleteAgent deletes an agent from a remote runtime broker.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?deleteFiles=%t&removeBranch=%t",
		strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID), deleteFiles, removeBranch)

	if c.debug {
		slog.Debug("Dispatcher request", "method", "DELETE", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// MessageAgent sends a message to an agent on a remote runtime broker.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/message", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	body, err := json.Marshal(map[string]interface{}{
		"message":   message,
		"interrupt": interrupt,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// HasPromptResponse is the response from the has-prompt action.
type HasPromptResponse struct {
	HasPrompt bool `json:"hasPrompt"`
}

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error) {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/has-prompt", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	var result HasPromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.HasPrompt, nil
}

// AgentTokenGenerator generates JWT tokens for agents.
type AgentTokenGenerator interface {
	GenerateAgentToken(agentID, groveID string) (string, error)
}

// HTTPAgentDispatcher dispatches agent operations to remote runtime brokers via HTTP.
// It looks up the runtime broker endpoint from the store and uses HTTPRuntimeBrokerClient
// to make the actual API calls.
type HTTPAgentDispatcher struct {
	store          store.Store
	client         RuntimeBrokerClient
	tokenGenerator AgentTokenGenerator
	hubEndpoint    string // Hub endpoint URL for agents to call back
	debug          bool
}

// NewHTTPAgentDispatcher creates a new HTTP-based agent dispatcher.
func NewHTTPAgentDispatcher(s store.Store, debug bool) *HTTPAgentDispatcher {
	return &HTTPAgentDispatcher{
		store:  s,
		client: NewHTTPRuntimeBrokerClientWithDebug(debug),
		debug:  debug,
	}
}

// NewHTTPAgentDispatcherWithClient creates a new HTTP-based agent dispatcher with a custom client.
func NewHTTPAgentDispatcherWithClient(s store.Store, client RuntimeBrokerClient, debug bool) *HTTPAgentDispatcher {
	return &HTTPAgentDispatcher{
		store:  s,
		client: client,
		debug:  debug,
	}
}

// SetTokenGenerator sets the token generator for agent authentication.
func (d *HTTPAgentDispatcher) SetTokenGenerator(gen AgentTokenGenerator) {
	d.tokenGenerator = gen
}

// SetHubEndpoint sets the Hub endpoint URL that agents will use to call back.
func (d *HTTPAgentDispatcher) SetHubEndpoint(endpoint string) {
	d.hubEndpoint = endpoint
}

// getBrokerEndpoint retrieves the endpoint URL for a runtime broker.
func (d *HTTPAgentDispatcher) getBrokerEndpoint(ctx context.Context, brokerID string) (string, error) {
	broker, err := d.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		return "", fmt.Errorf("failed to get runtime broker: %w", err)
	}

	if broker.Endpoint == "" {
		// Fall back to constructing endpoint from broker info
		// This assumes the broker is reachable at its default port
		return fmt.Sprintf("http://localhost:9800"), nil
	}

	return broker.Endpoint, nil
}

// DispatchAgentCreate creates an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	// Look up the local path for this grove on the target runtime broker
	var grovePath string
	if agent.GroveID != "" && agent.RuntimeBrokerID != "" {
		provider, err := d.store.GetGroveProvider(ctx, agent.GroveID, agent.RuntimeBrokerID)
		if err != nil {
			if d.debug {
				slog.Warn("Failed to get grove provider for path lookup", "error", err)
			}
		} else if provider.LocalPath != "" {
			grovePath = provider.LocalPath
			if d.debug {
				slog.Debug("Found grove path for broker", "brokerID", agent.RuntimeBrokerID, "path", grovePath)
			}
		}
	}

	// Build the remote create request
	req := &RemoteCreateAgentRequest{
		ID:          agent.ID,
		Slug:        agent.Slug,
		Name:        agent.Name,
		GroveID:     agent.GroveID,
		UserID:      agent.OwnerID,
		HubEndpoint: d.hubEndpoint,
		GrovePath:   grovePath,
	}

	if d.debug {
		slog.Debug("DispatchAgentCreate",
			"agentName", agent.Name,
			"hubEndpoint", d.hubEndpoint,
			"hasTokenGenerator", d.tokenGenerator != nil,
		)
	}

	// Generate agent token if token generator is available
	if d.tokenGenerator != nil {
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.GroveID)
		if err != nil {
			if d.debug {
				slog.Warn("Failed to generate agent token", "error", err)
			}
			// Continue without token - agent will operate in unauthenticated mode
		} else {
			req.AgentToken = token
			if d.debug {
				slog.Debug("Generated agent token", "length", len(token))
			}
		}
	} else if d.debug {
		slog.Debug("No token generator configured - agent will not have Hub credentials")
	}

	// Add configuration if available
	if agent.AppliedConfig != nil {
		req.Config = &RemoteAgentConfig{
			Template:     agent.AppliedConfig.Harness,
			Image:        agent.AppliedConfig.Image,
			Task:         agent.AppliedConfig.Task,
			TemplateID:   agent.AppliedConfig.TemplateID,
			TemplateHash: agent.AppliedConfig.TemplateHash,
		}
		req.ResolvedEnv = agent.AppliedConfig.Env
	}

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return err
	}

	// Update agent with runtime info
	if resp.Agent != nil {
		agent.Status = resp.Agent.Status
		agent.ContainerStatus = resp.Agent.ContainerStatus
		if resp.Agent.ID != "" {
			agent.RuntimeState = "container:" + resp.Agent.ID
		}
		// Capture template and runtime from the broker response
		if resp.Agent.Template != "" {
			agent.Template = resp.Agent.Template
		}
		if resp.Agent.Runtime != "" {
			agent.Runtime = resp.Agent.Runtime
		}
	}

	return nil
}

// DispatchAgentStart starts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	// Use agent name as identifier (runtime broker uses name or ID)
	return d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name)
}

// DispatchAgentStop stops an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.StopAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name)
}

// DispatchAgentRestart restarts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.RestartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name)
}

// DispatchAgentDelete deletes an agent from the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch bool) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name, deleteFiles, removeBranch)
}

// DispatchAgentMessage sends a message to an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.MessageAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name, message, interrupt)
}

// DispatchCheckAgentPrompt checks if an agent has a non-empty prompt.md file.
func (d *HTTPAgentDispatcher) DispatchCheckAgentPrompt(ctx context.Context, agent *store.Agent) (bool, error) {
	if agent.RuntimeBrokerID == "" {
		return false, fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return false, err
	}

	return d.client.CheckAgentPrompt(ctx, agent.RuntimeBrokerID, endpoint, agent.Name)
}
