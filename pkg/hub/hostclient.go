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

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/store"
)

// AuthenticatedHostClient is an HTTP-based RuntimeHostClient that signs
// outgoing requests with HMAC authentication. This allows the Hub to make
// authenticated requests to Runtime Hosts.
type AuthenticatedHostClient struct {
	httpClient *http.Client
	store      store.Store
	debug      bool
}

// NewAuthenticatedHostClient creates a new authenticated host client.
func NewAuthenticatedHostClient(s store.Store, debug bool) *AuthenticatedHostClient {
	return &AuthenticatedHostClient{
		httpClient: &http.Client{
			Timeout: 120 * time.Second, // Agent creation can take a while
		},
		store: s,
		debug: debug,
	}
}

// getHostSecret retrieves the secret key for a host from the store.
func (c *AuthenticatedHostClient) getHostSecret(ctx context.Context, hostID string) ([]byte, error) {
	secret, err := c.store.GetHostSecret(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("failed to get host secret: %w", err)
	}

	if secret.Status != store.HostSecretStatusActive {
		return nil, fmt.Errorf("host secret is %s", secret.Status)
	}

	if !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt) {
		return nil, fmt.Errorf("host secret has expired")
	}

	return secret.SecretKey, nil
}

// signRequest signs an HTTP request with HMAC authentication.
func (c *AuthenticatedHostClient) signRequest(ctx context.Context, req *http.Request, hostID string) error {
	secret, err := c.getHostSecret(ctx, hostID)
	if err != nil {
		return err
	}

	// Use the shared HMAC auth implementation
	auth := &apiclient.HMACAuth{
		HostID:    hostID,
		SecretKey: secret,
	}

	return auth.ApplyAuth(req)
}

// doRequest performs an HTTP request with HMAC signing.
func (c *AuthenticatedHostClient) doRequest(ctx context.Context, hostID, method, endpoint string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Sign the request
	if err := c.signRequest(ctx, req, hostID); err != nil {
		if c.debug {
			log.Printf("[AuthHostClient] Warning: failed to sign request: %v (continuing without auth)", err)
		}
		// Continue without authentication - the host may reject or allow depending on its config
	} else if c.debug {
		log.Printf("[AuthHostClient] Signed request for host %s", hostID)
	}

	if c.debug {
		log.Printf("[AuthHostClient] %s %s", method, endpoint)
	}

	return c.httpClient.Do(req)
}

// CreateAgent creates an agent on a remote runtime host with HMAC authentication.
func (c *AuthenticatedHostClient) CreateAgent(ctx context.Context, hostID, hostEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents", hostEndpoint)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, hostID, http.MethodPost, endpoint, body)
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

// StartAgent starts an agent on a remote runtime host with HMAC authentication.
func (c *AuthenticatedHostClient) StartAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", hostEndpoint, url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, hostID, http.MethodPost, endpoint, nil)
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

// StopAgent stops an agent on a remote runtime host with HMAC authentication.
func (c *AuthenticatedHostClient) StopAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/stop", hostEndpoint, url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, hostID, http.MethodPost, endpoint, nil)
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

// RestartAgent restarts an agent on a remote runtime host with HMAC authentication.
func (c *AuthenticatedHostClient) RestartAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/restart", hostEndpoint, url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, hostID, http.MethodPost, endpoint, nil)
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

// DeleteAgent deletes an agent from a remote runtime host with HMAC authentication.
func (c *AuthenticatedHostClient) DeleteAgent(ctx context.Context, hostID, hostEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?deleteFiles=%t&removeBranch=%t",
		hostEndpoint, url.PathEscape(agentID), deleteFiles, removeBranch)

	resp, err := c.doRequest(ctx, hostID, http.MethodDelete, endpoint, nil)
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

// MessageAgent sends a message to an agent on a remote runtime host with HMAC authentication.
func (c *AuthenticatedHostClient) MessageAgent(ctx context.Context, hostID, hostEndpoint, agentID, message string, interrupt bool) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/message", hostEndpoint, url.PathEscape(agentID))

	body, err := json.Marshal(map[string]interface{}{
		"message":   message,
		"interrupt": interrupt,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, hostID, http.MethodPost, endpoint, body)
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
