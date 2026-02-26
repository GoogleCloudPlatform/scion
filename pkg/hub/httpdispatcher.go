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

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/secret"
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
func (c *HTTPRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, task, grovePath, groveSlug string) (*RemoteAgentResponse, error) {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	if c.debug {
		slog.Debug("Dispatcher request", "method", "POST", "endpoint", endpoint)
	}

	payload := map[string]string{}
	if task != "" {
		payload["task"] = task
	}
	if grovePath != "" {
		payload["grovePath"] = grovePath
	}
	if groveSlug != "" {
		payload["groveSlug"] = groveSlug
	}

	var body io.Reader
	if len(payload) > 0 {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if len(payload) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}

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
		// If the broker doesn't return a parseable response, that's OK — return nil
		return nil, nil
	}

	return &result, nil
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
func (c *HTTPRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	_ = brokerID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?deleteFiles=%t&removeBranch=%t",
		strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID), deleteFiles, removeBranch)
	if softDelete {
		endpoint += fmt.Sprintf("&softDelete=true&deletedAt=%s", url.QueryEscape(deletedAt.Format(time.RFC3339)))
	}

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

// CreateAgentWithGather creates an agent and handles 202 env-gather responses.
func (c *HTTPRuntimeBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	_ = brokerID
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))

	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if c.debug {
		slog.Debug("Dispatcher request (gather)", "method", "POST", "endpoint", endpoint)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
	}

	if resp.StatusCode == http.StatusAccepted {
		var envReqs RemoteEnvRequirementsResponse
		if err := json.NewDecoder(resp.Body).Decode(&envReqs); err != nil {
			return nil, nil, fmt.Errorf("failed to decode env requirements: %w", err)
		}
		return nil, &envReqs, nil
	}

	var result RemoteAgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil, nil
}

// FinalizeEnv sends gathered env vars to a broker to complete agent creation.
// Note: brokerID is unused in this unauthenticated client.
func (c *HTTPRuntimeBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	_ = brokerID
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/finalize-env", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	body, err := json.Marshal(map[string]interface{}{
		"env": env,
	})
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

// AgentTokenGenerator generates JWT tokens for agents.
type AgentTokenGenerator interface {
	GenerateAgentToken(agentID, groveID string, additionalScopes ...AgentTokenScope) (string, error)
}

// HTTPAgentDispatcher dispatches agent operations to remote runtime brokers via HTTP.
// It looks up the runtime broker endpoint from the store and uses HTTPRuntimeBrokerClient
// to make the actual API calls.
type HTTPAgentDispatcher struct {
	store          store.Store
	client         RuntimeBrokerClient
	tokenGenerator AgentTokenGenerator
	secretBackend  secret.SecretBackend
	hubEndpoint    string // Hub endpoint URL for agents to call back
	devAuthToken   string // Dev auth token to inject into agent env (dev-auth mode only)
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

// SetSecretBackend sets the secret backend for resolving secrets.
func (d *HTTPAgentDispatcher) SetSecretBackend(b secret.SecretBackend) {
	d.secretBackend = b
}

// SetDevAuthToken sets the dev auth token to inject into agent containers.
// When set, agents receive SCION_DEV_TOKEN as a fallback authentication method.
func (d *HTTPAgentDispatcher) SetDevAuthToken(token string) {
	d.devAuthToken = token
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

// buildCreateRequest builds a RemoteCreateAgentRequest from the agent's store record.
// This is shared between DispatchAgentCreate and DispatchAgentProvision.
func (d *HTTPAgentDispatcher) buildCreateRequest(ctx context.Context, agent *store.Agent, callerName string) (*RemoteCreateAgentRequest, error) {
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

	// Propagate attach mode from applied config
	if agent.AppliedConfig != nil {
		req.Attach = agent.AppliedConfig.Attach
	}

	// Propagate creator name for SCION_CREATOR env var
	if agent.AppliedConfig != nil && agent.AppliedConfig.CreatorName != "" {
		req.CreatorName = agent.AppliedConfig.CreatorName
	}

	// Pass workspace storage path for GCS bootstrap (non-git workspaces)
	if agent.AppliedConfig != nil && agent.AppliedConfig.WorkspaceStoragePath != "" {
		req.WorkspaceStoragePath = agent.AppliedConfig.WorkspaceStoragePath
	}

	// For hub-native groves (no git remote AND no local provider path on this
	// broker), propagate the grove slug so the broker can create the workspace
	// at the conventional path (~/.scion/groves/<slug>/).
	// When the broker has a local provider path, the grove exists on its
	// filesystem already and should use that path instead.
	if agent.GroveID != "" && grovePath == "" {
		grove, err := d.store.GetGrove(ctx, agent.GroveID)
		if err == nil && grove.GitRemote == "" {
			req.GroveSlug = grove.Slug
		}
	}

	if d.debug {
		slog.Debug(callerName,
			"agentName", agent.Name,
			"hubEndpoint", d.hubEndpoint,
			"hasTokenGenerator", d.tokenGenerator != nil,
		)
	}

	// Generate agent token if token generator is available
	if d.tokenGenerator != nil {
		// Convert hub access scopes from AppliedConfig to AgentTokenScope
		var additionalScopes []AgentTokenScope
		if agent.AppliedConfig != nil {
			for _, s := range agent.AppliedConfig.HubAccessScopes {
				additionalScopes = append(additionalScopes, AgentTokenScope(s))
			}
		}
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.GroveID, additionalScopes...)
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
		workspace := agent.AppliedConfig.Workspace
		// When the broker has a local provider path for this grove, the
		// workspace is derived from the grove path (not a hub-native path).
		// Clear the hub-native workspace that populateAgentConfig may have set.
		if grovePath != "" {
			workspace = ""
		}
		req.Config = &RemoteAgentConfig{
			Template:     agent.Template,
			Image:        agent.AppliedConfig.Image,
			Task:         agent.AppliedConfig.Task,
			Workspace:    workspace,
			Profile:      agent.AppliedConfig.Profile,
			TemplateID:   agent.AppliedConfig.TemplateID,
			TemplateHash: agent.AppliedConfig.TemplateHash,
			GitClone:     agent.AppliedConfig.GitClone,
		}
		req.ResolvedEnv = agent.AppliedConfig.Env
		if d.debug {
			slog.Debug("buildCreateRequest: config sent to broker",
				"template", agent.Template,
				"image", agent.AppliedConfig.Image,
				"harness", agent.AppliedConfig.Harness,
				"profile", agent.AppliedConfig.Profile,
				"templateID", agent.AppliedConfig.TemplateID,
				"grovePath", req.GrovePath,
			)
		}
	}

	// Include template secrets declarations for broker env-gather
	if agent.AppliedConfig != nil && agent.AppliedConfig.TemplateID != "" {
		tmpl, err := d.store.GetTemplate(ctx, agent.AppliedConfig.TemplateID)
		if err == nil && tmpl != nil && tmpl.Config != nil && len(tmpl.Config.Secrets) > 0 {
			req.RequiredSecrets = make([]api.RequiredSecret, len(tmpl.Config.Secrets))
			for i, s := range tmpl.Config.Secrets {
				req.RequiredSecrets[i] = api.RequiredSecret{
					Key:         s.Key,
					Description: s.Description,
					Type:        s.Type,
					Target:      s.Target,
				}
			}
		}
	}

	// Resolve type-aware secrets from all applicable scopes
	resolvedSecrets, err := d.resolveSecrets(ctx, agent)
	if err != nil {
		if d.debug {
			slog.Warn("Failed to resolve secrets", "error", err)
		}
		// Continue without secrets rather than failing agent creation
	} else if len(resolvedSecrets) > 0 {
		req.ResolvedSecrets = resolvedSecrets
		if d.debug {
			slog.Debug("Resolved secrets for agent", "count", len(resolvedSecrets))
		}
	}

	// In dev-auth mode, inject the dev token so agents can use it as fallback auth
	if d.devAuthToken != "" {
		if req.ResolvedEnv == nil {
			req.ResolvedEnv = make(map[string]string)
		}
		req.ResolvedEnv["SCION_DEV_TOKEN"] = d.devAuthToken
	}

	return req, nil
}

// applyBrokerResponse updates agent fields from the broker's response.
func (d *HTTPAgentDispatcher) applyBrokerResponse(agent *store.Agent, resp *RemoteAgentResponse) {
	if resp.Agent != nil {
		if d.debug {
			slog.Debug("applyBrokerResponse: applying broker status",
				"agentName", agent.Name,
				"previousStatus", agent.Status,
				"brokerStatus", resp.Agent.Status,
				"containerStatus", resp.Agent.ContainerStatus,
				"brokerAgentID", resp.Agent.ID,
			)
		}
		agent.Status = resp.Agent.Status
		agent.ContainerStatus = resp.Agent.ContainerStatus
		if resp.Agent.ID != "" {
			agent.RuntimeState = "container:" + resp.Agent.ID
		}
		// Capture template, harness, and runtime from the broker response
		if resp.Agent.Template != "" {
			agent.Template = resp.Agent.Template
		}
		if resp.Agent.HarnessConfig != "" && agent.AppliedConfig != nil {
			agent.AppliedConfig.Harness = resp.Agent.HarnessConfig
		}
		if resp.Agent.Runtime != "" {
			agent.Runtime = resp.Agent.Runtime
		}
	} else if d.debug {
		slog.Debug("applyBrokerResponse: broker response has nil Agent",
			"agentName", agent.Name,
		)
	}
}

// DispatchAgentCreate creates and starts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreate")
	if err != nil {
		return err
	}

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return err
	}

	d.applyBrokerResponse(agent, resp)
	return nil
}

// DispatchAgentProvision provisions an agent on the runtime broker without starting it.
func (d *HTTPAgentDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentProvision")
	if err != nil {
		return err
	}
	req.ProvisionOnly = true

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return err
	}

	d.applyBrokerResponse(agent, resp)
	return nil
}

// DispatchAgentCreateWithGather creates an agent with env-gather support.
// If the broker returns 202 with env requirements, it returns the requirements
// as the first value instead of an error.
func (d *HTTPAgentDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	if agent.RuntimeBrokerID == "" {
		return nil, fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return nil, err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreateWithGather")
	if err != nil {
		return nil, err
	}
	req.GatherEnv = true

	// Resolve env vars from Hub storage and merge with AppliedConfig.Env
	envSources, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil && d.debug {
		slog.Warn("Failed to resolve env vars from storage for gather", "error", err)
	}
	if len(envSources) > 0 {
		if req.ResolvedEnv == nil {
			req.ResolvedEnv = make(map[string]string)
		}
		for k, v := range envSources {
			if _, exists := req.ResolvedEnv[k]; !exists {
				req.ResolvedEnv[k] = v
			}
		}
	}

	// Track which scope provided each key
	req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

	resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return nil, err
	}

	if envReqs != nil {
		return envReqs, nil
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil, nil
}

// DispatchFinalizeEnv sends gathered env vars to the broker to complete agent creation.
func (d *HTTPAgentDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	resp, err := d.client.FinalizeEnv(ctx, agent.RuntimeBrokerID, endpoint, agent.Name, env)
	if err != nil {
		return err
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil
}

// resolveEnvFromStorage queries Hub env var storage for all applicable scopes
// and returns a merged map with precedence: user > grove > global.
func (d *HTTPAgentDispatcher) resolveEnvFromStorage(ctx context.Context, agent *store.Agent) (map[string]string, error) {
	result := make(map[string]string)

	// Query grove-scoped env vars
	if agent.GroveID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "grove", ScopeID: agent.GroveID})
		if err != nil {
			if d.debug {
				slog.Warn("Failed to list grove env vars", "error", err)
			}
		} else {
			for _, v := range vars {
				result[v.Key] = v.Value
			}
		}
	}

	// Query user-scoped env vars (higher precedence)
	if agent.OwnerID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID})
		if err != nil {
			if d.debug {
				slog.Warn("Failed to list user env vars", "error", err)
			}
		} else {
			for _, v := range vars {
				result[v.Key] = v.Value
			}
		}
	}

	// Query runtime_broker-scoped env vars (if applicable)
	if agent.RuntimeBrokerID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "runtime_broker", ScopeID: agent.RuntimeBrokerID})
		if err != nil {
			if d.debug {
				slog.Warn("Failed to list broker env vars", "error", err)
			}
		} else {
			for _, v := range vars {
				result[v.Key] = v.Value
			}
		}
	}

	return result, nil
}

// buildEnvSources creates a map of env key -> scope for reporting to the CLI.
func (d *HTTPAgentDispatcher) buildEnvSources(ctx context.Context, agent *store.Agent, resolvedEnv map[string]string) map[string]string {
	sources := make(map[string]string)

	// Check grove scope
	if agent.GroveID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "grove", ScopeID: agent.GroveID})
		if err == nil {
			for _, v := range vars {
				if _, inResolved := resolvedEnv[v.Key]; inResolved {
					sources[v.Key] = "grove"
				}
			}
		}
	}

	// Check user scope (overrides grove)
	if agent.OwnerID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID})
		if err == nil {
			for _, v := range vars {
				if _, inResolved := resolvedEnv[v.Key]; inResolved {
					sources[v.Key] = "user"
				}
			}
		}
	}

	// Check config scope
	if agent.AppliedConfig != nil {
		for k := range agent.AppliedConfig.Env {
			if _, inResolved := resolvedEnv[k]; inResolved {
				sources[k] = "config"
			}
		}
	}

	return sources
}

// DispatchAgentStart starts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	// If no explicit task provided, fall back to the agent's applied config task
	if task == "" && agent.AppliedConfig != nil {
		task = agent.AppliedConfig.Task
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

	// For hub-native groves (no git remote AND no local provider path on this
	// broker), propagate the grove slug so the broker can resolve the workspace
	// at the conventional path (~/.scion/groves/<slug>/).
	var groveSlug string
	if agent.GroveID != "" && grovePath == "" {
		grove, err := d.store.GetGrove(ctx, agent.GroveID)
		if err == nil && grove.GitRemote == "" {
			groveSlug = grove.Slug
		}
	}

	// Use agent name as identifier (runtime broker uses name or ID)
	resp, err := d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name, task, grovePath, groveSlug)
	if err != nil {
		return err
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil
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
func (d *HTTPAgentDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent has no runtime broker assigned")
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Name, deleteFiles, removeBranch, softDelete, deletedAt)
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

// resolveSecrets queries secrets from all applicable scopes and merges them
// into a flat list. Higher scopes override lower: user < grove < runtime_broker.
func (d *HTTPAgentDispatcher) resolveSecrets(ctx context.Context, agent *store.Agent) ([]ResolvedSecret, error) {
	if d.secretBackend == nil {
		return nil, nil
	}
	resolved, err := d.secretBackend.Resolve(ctx, agent.OwnerID, agent.GroveID, agent.RuntimeBrokerID)
	if err != nil {
		return nil, err
	}
	result := make([]ResolvedSecret, len(resolved))
	for i, sv := range resolved {
		result[i] = ResolvedSecret{
			Name:   sv.Name,
			Type:   sv.SecretType,
			Target: sv.Target,
			Value:  sv.Value,
			Source: sv.Scope,
			Ref:    sv.SecretRef,
		}
	}
	return result, nil
}
