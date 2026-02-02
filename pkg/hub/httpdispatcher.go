// Package hub provides the Scion Hub API server.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/ptone/scion-agent/pkg/store"
)

// HTTPRuntimeHostClient is an HTTP-based implementation of RuntimeHostClient.
// It communicates with remote runtime hosts via their REST API.
type HTTPRuntimeHostClient struct {
	client *http.Client
	debug  bool
}

// NewHTTPRuntimeHostClient creates a new HTTP runtime host client.
func NewHTTPRuntimeHostClient() *HTTPRuntimeHostClient {
	return &HTTPRuntimeHostClient{
		client: &http.Client{
			Timeout: 120 * time.Second, // Agent creation can take a while
		},
	}
}

// NewHTTPRuntimeHostClientWithDebug creates a new HTTP runtime host client with debug logging.
func NewHTTPRuntimeHostClientWithDebug(debug bool) *HTTPRuntimeHostClient {
	return &HTTPRuntimeHostClient{
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		debug: debug,
	}
}

// CreateAgent creates an agent on a remote runtime host.
// Note: hostID is unused in this unauthenticated client but is part of the
// RuntimeHostClient interface for compatibility with AuthenticatedHostClient.
func (c *HTTPRuntimeHostClient) CreateAgent(ctx context.Context, hostID, hostEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	_ = hostID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents", hostEndpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if c.debug {
		log.Printf("[HTTPDispatcher] POST %s", endpoint)
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
		return nil, fmt.Errorf("runtime host returned error %d: %s", resp.StatusCode, string(respBody))
	}

	var result RemoteAgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// StartAgent starts an agent on a remote runtime host.
// Note: hostID is unused in this unauthenticated client.
func (c *HTTPRuntimeHostClient) StartAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error {
	_ = hostID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", hostEndpoint, url.PathEscape(agentID))

	if c.debug {
		log.Printf("[HTTPDispatcher] POST %s", endpoint)
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
		return fmt.Errorf("runtime host returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// StopAgent stops an agent on a remote runtime host.
// Note: hostID is unused in this unauthenticated client.
func (c *HTTPRuntimeHostClient) StopAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error {
	_ = hostID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/stop", hostEndpoint, url.PathEscape(agentID))

	if c.debug {
		log.Printf("[HTTPDispatcher] POST %s", endpoint)
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
		return fmt.Errorf("runtime host returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// RestartAgent restarts an agent on a remote runtime host.
// Note: hostID is unused in this unauthenticated client.
func (c *HTTPRuntimeHostClient) RestartAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error {
	_ = hostID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/restart", hostEndpoint, url.PathEscape(agentID))

	if c.debug {
		log.Printf("[HTTPDispatcher] POST %s", endpoint)
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
		return fmt.Errorf("runtime host returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// DeleteAgent deletes an agent from a remote runtime host.
// Note: hostID is unused in this unauthenticated client.
func (c *HTTPRuntimeHostClient) DeleteAgent(ctx context.Context, hostID, hostEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	_ = hostID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?deleteFiles=%t&removeBranch=%t",
		hostEndpoint, url.PathEscape(agentID), deleteFiles, removeBranch)

	if c.debug {
		log.Printf("[HTTPDispatcher] DELETE %s", endpoint)
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
		return fmt.Errorf("runtime host returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// MessageAgent sends a message to an agent on a remote runtime host.
// Note: hostID is unused in this unauthenticated client.
func (c *HTTPRuntimeHostClient) MessageAgent(ctx context.Context, hostID, hostEndpoint, agentID, message string, interrupt bool) error {
	_ = hostID // Unused in unauthenticated client
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/message", hostEndpoint, url.PathEscape(agentID))

	body, err := json.Marshal(map[string]interface{}{
		"message":   message,
		"interrupt": interrupt,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	if c.debug {
		log.Printf("[HTTPDispatcher] POST %s", endpoint)
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
		return fmt.Errorf("runtime host returned error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// AgentTokenGenerator generates JWT tokens for agents.
type AgentTokenGenerator interface {
	GenerateAgentToken(agentID, groveID string) (string, error)
}

// HTTPAgentDispatcher dispatches agent operations to remote runtime hosts via HTTP.
// It looks up the runtime host endpoint from the store and uses HTTPRuntimeHostClient
// to make the actual API calls.
type HTTPAgentDispatcher struct {
	store          store.Store
	client         RuntimeHostClient
	tokenGenerator AgentTokenGenerator
	hubEndpoint    string // Hub endpoint URL for agents to call back
	debug          bool
}

// NewHTTPAgentDispatcher creates a new HTTP-based agent dispatcher.
func NewHTTPAgentDispatcher(s store.Store, debug bool) *HTTPAgentDispatcher {
	return &HTTPAgentDispatcher{
		store:  s,
		client: NewHTTPRuntimeHostClientWithDebug(debug),
		debug:  debug,
	}
}

// NewHTTPAgentDispatcherWithClient creates a new HTTP-based agent dispatcher with a custom client.
func NewHTTPAgentDispatcherWithClient(s store.Store, client RuntimeHostClient, debug bool) *HTTPAgentDispatcher {
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

// getHostEndpoint retrieves the endpoint URL for a runtime host.
func (d *HTTPAgentDispatcher) getHostEndpoint(ctx context.Context, hostID string) (string, error) {
	host, err := d.store.GetRuntimeHost(ctx, hostID)
	if err != nil {
		return "", fmt.Errorf("failed to get runtime host: %w", err)
	}

	if host.Endpoint == "" {
		// Fall back to constructing endpoint from host info
		// This assumes the host is reachable at its default port
		return fmt.Sprintf("http://localhost:9800"), nil
	}

	return host.Endpoint, nil
}

// DispatchAgentCreate creates an agent on the runtime host.
func (d *HTTPAgentDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeHostID == "" {
		return fmt.Errorf("agent has no runtime host assigned")
	}

	endpoint, err := d.getHostEndpoint(ctx, agent.RuntimeHostID)
	if err != nil {
		return err
	}

	// Build the remote create request
	req := &RemoteCreateAgentRequest{
		AgentID:     agent.ID,
		Name:        agent.Name,
		GroveID:     agent.GroveID,
		UserID:      agent.OwnerID,
		HubEndpoint: d.hubEndpoint,
	}

	// Generate agent token if token generator is available
	if d.tokenGenerator != nil {
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.GroveID)
		if err != nil {
			if d.debug {
				log.Printf("[HTTPDispatcher] Warning: failed to generate agent token: %v", err)
			}
			// Continue without token - agent will operate in unauthenticated mode
		} else {
			req.AgentToken = token
		}
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

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeHostID, endpoint, req)
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
	}

	return nil
}

// DispatchAgentStart starts an agent on the runtime host.
func (d *HTTPAgentDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeHostID == "" {
		return fmt.Errorf("agent has no runtime host assigned")
	}

	endpoint, err := d.getHostEndpoint(ctx, agent.RuntimeHostID)
	if err != nil {
		return err
	}

	// Use agent name as identifier (runtime host uses name or ID)
	return d.client.StartAgent(ctx, agent.RuntimeHostID, endpoint, agent.Name)
}

// DispatchAgentStop stops an agent on the runtime host.
func (d *HTTPAgentDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeHostID == "" {
		return fmt.Errorf("agent has no runtime host assigned")
	}

	endpoint, err := d.getHostEndpoint(ctx, agent.RuntimeHostID)
	if err != nil {
		return err
	}

	return d.client.StopAgent(ctx, agent.RuntimeHostID, endpoint, agent.Name)
}

// DispatchAgentRestart restarts an agent on the runtime host.
func (d *HTTPAgentDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
	if agent.RuntimeHostID == "" {
		return fmt.Errorf("agent has no runtime host assigned")
	}

	endpoint, err := d.getHostEndpoint(ctx, agent.RuntimeHostID)
	if err != nil {
		return err
	}

	return d.client.RestartAgent(ctx, agent.RuntimeHostID, endpoint, agent.Name)
}

// DispatchAgentDelete deletes an agent from the runtime host.
func (d *HTTPAgentDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch bool) error {
	if agent.RuntimeHostID == "" {
		return fmt.Errorf("agent has no runtime host assigned")
	}

	endpoint, err := d.getHostEndpoint(ctx, agent.RuntimeHostID)
	if err != nil {
		return err
	}

	return d.client.DeleteAgent(ctx, agent.RuntimeHostID, endpoint, agent.Name, deleteFiles, removeBranch)
}

// DispatchAgentMessage sends a message to an agent on the runtime host.
func (d *HTTPAgentDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool) error {
	if agent.RuntimeHostID == "" {
		return fmt.Errorf("agent has no runtime host assigned")
	}

	endpoint, err := d.getHostEndpoint(ctx, agent.RuntimeHostID)
	if err != nil {
		return err
	}

	return d.client.MessageAgent(ctx, agent.RuntimeHostID, endpoint, agent.Name, message, interrupt)
}
