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

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/store"
)

// AuthenticatedBrokerClient is an HTTP-based RuntimeBrokerClient that signs
// outgoing requests with HMAC authentication. This allows the Hub to make
// authenticated requests to Runtime Brokers.
type AuthenticatedBrokerClient struct {
	httpClient *http.Client
	store      store.Store
	debug      bool
}

// NewAuthenticatedBrokerClient creates a new authenticated broker client.
func NewAuthenticatedBrokerClient(s store.Store, debug bool) *AuthenticatedBrokerClient {
	return &AuthenticatedBrokerClient{
		httpClient: &http.Client{
			Timeout: 120 * time.Second, // Agent creation can take a while
		},
		store: s,
		debug: debug,
	}
}

// getBrokerSecret retrieves the secret key for a broker from the store.
func (c *AuthenticatedBrokerClient) getBrokerSecret(ctx context.Context, brokerID string) ([]byte, error) {
	secret, err := c.store.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get broker secret: %w", err)
	}

	if secret.Status != store.BrokerSecretStatusActive {
		return nil, fmt.Errorf("broker secret is %s", secret.Status)
	}

	if !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt) {
		return nil, fmt.Errorf("broker secret has expired")
	}

	return secret.SecretKey, nil
}

// signRequest signs an HTTP request with HMAC authentication.
func (c *AuthenticatedBrokerClient) signRequest(ctx context.Context, req *http.Request, brokerID string) error {
	secret, err := c.getBrokerSecret(ctx, brokerID)
	if err != nil {
		return err
	}

	// Use the shared HMAC auth implementation
	auth := &apiclient.HMACAuth{
		BrokerID:    brokerID,
		SecretKey: secret,
	}

	return auth.ApplyAuth(req)
}

// doRequest performs an HTTP request with HMAC signing.
func (c *AuthenticatedBrokerClient) doRequest(ctx context.Context, brokerID, method, endpoint string, body []byte) (*http.Response, error) {
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
	if err := c.signRequest(ctx, req, brokerID); err != nil {
		if c.debug {
			slog.Warn("Failed to sign request", "brokerID", brokerID, "error", err)
		}
		// Continue without authentication - the broker may reject or allow depending on its config
	} else if c.debug {
		slog.Debug("Signed request for broker", "brokerID", brokerID)
	}

	if c.debug {
		slog.Debug("Outgoing request to broker", "method", method, "endpoint", endpoint)
	}

	return c.httpClient.Do(req)
}

// CreateAgent creates an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
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

// StartAgent starts an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
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

// StopAgent stops an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/stop", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
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

// RestartAgent restarts an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/restart", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
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

// DeleteAgent deletes an agent from a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?deleteFiles=%t&removeBranch=%t",
		strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID), deleteFiles, removeBranch)

	resp, err := c.doRequest(ctx, brokerID, http.MethodDelete, endpoint, nil)
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

// MessageAgent sends a message to an agent on a remote runtime broker with HMAC authentication.
func (c *AuthenticatedBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/message", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	body, err := json.Marshal(map[string]interface{}{
		"message":   message,
		"interrupt": interrupt,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
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

// CheckAgentPrompt checks if an agent has a non-empty prompt.md file on a remote runtime broker.
func (c *AuthenticatedBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/has-prompt", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))

	resp, err := c.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
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
