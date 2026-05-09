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
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type brokerRequestSigner interface {
	Sign(context.Context, *http.Request, string) error
}

type hmacBrokerSigner struct {
	store store.Store
}

func (s *hmacBrokerSigner) Sign(ctx context.Context, req *http.Request, brokerID string) error {
	secret, err := s.store.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		return fmt.Errorf("failed to get broker secret: %w", err)
	}
	if secret.Status != store.BrokerSecretStatusActive {
		return fmt.Errorf("broker secret is %s", secret.Status)
	}
	if !secret.ExpiresAt.IsZero() && time.Now().After(secret.ExpiresAt) {
		return fmt.Errorf("broker secret has expired")
	}

	auth := &apiclient.HMACAuth{
		BrokerID:  brokerID,
		SecretKey: secret.SecretKey,
	}
	return auth.ApplyAuth(req)
}

// brokerHTTPTransport centralizes broker HTTP dispatch and response handling.
// Optional signing is injected through brokerRequestSigner.
type brokerHTTPTransport struct {
	client *http.Client
	debug  bool
	signer brokerRequestSigner
}

func newBrokerHTTPTransport(debug bool, signer brokerRequestSigner) *brokerHTTPTransport {
	return &brokerHTTPTransport{
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
		debug:  debug,
		signer: signer,
	}
}

func (t *brokerHTTPTransport) doRequest(ctx context.Context, brokerID, method, endpoint string, body []byte) (*http.Response, error) {
	if endpoint == "" || !strings.Contains(endpoint, "://") {
		return nil, fmt.Errorf("runtime broker %q has no HTTP endpoint configured (control channel may be required)", brokerID)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if t.signer != nil {
		if err := t.signer.Sign(ctx, req, brokerID); err != nil {
			if t.debug {
				slog.Warn("Failed to sign request", "brokerID", brokerID, "error", err)
			}
			return nil, fmt.Errorf("failed to sign request: %w", err)
		}
	}

	if t.debug {
		slog.Debug("Outgoing request to broker", "method", method, "endpoint", endpoint)
	}
	return t.client.Do(req)
}

func (t *brokerHTTPTransport) decodeResponse(resp *http.Response, out interface{}) error {
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	return nil
}

func (t *brokerHTTPTransport) decodeResponseWithSnippet(resp *http.Response, out interface{}) error {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		bodySnippet := string(respBody)
		if len(bodySnippet) > 256 {
			bodySnippet = bodySnippet[:256] + "...(truncated)"
		}
		return fmt.Errorf("failed to decode response: %w (body=%q)", err, bodySnippet)
	}
	return nil
}

func brokerHTTPError(resp *http.Response) error {
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("runtime broker returned error %d: %s", resp.StatusCode, string(respBody))
}

func (t *brokerHTTPTransport) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, brokerHTTPError(resp)
	}
	var result RemoteAgentResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *brokerHTTPTransport) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace bool) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/start", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
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

	var body []byte
	if len(payload) > 0 {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
	}

	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, brokerHTTPError(resp)
	}

	var result RemoteAgentResponse
	if err := t.decodeResponseWithSnippet(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *brokerHTTPTransport) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/stop", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/restart", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
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
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s?deleteFiles=%t&removeBranch=%t",
		strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID), deleteFiles, removeBranch)
	if projectID != "" {
		endpoint += "&projectId=" + url.QueryEscape(projectID)
	}
	if softDelete {
		endpoint += fmt.Sprintf("&softDelete=true&deletedAt=%s", url.QueryEscape(deletedAt.Format(time.RFC3339)))
	}

	resp, err := t.doRequest(ctx, brokerID, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/message", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
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
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return brokerHTTPError(resp)
	}
	return nil
}

func (t *brokerHTTPTransport) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/has-prompt", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, brokerHTTPError(resp)
	}
	var result HasPromptResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return false, err
	}
	return result.HasPrompt, nil
}

func (t *brokerHTTPTransport) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents", strings.TrimSuffix(brokerEndpoint, "/"))
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, nil, brokerHTTPError(resp)
	}

	if resp.StatusCode == http.StatusAccepted {
		var envReqs RemoteEnvRequirementsResponse
		if err := t.decodeResponse(resp, &envReqs); err != nil {
			return nil, nil, fmt.Errorf("failed to decode env requirements: %w", err)
		}
		return nil, &envReqs, nil
	}

	var result RemoteAgentResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return nil, nil, err
	}
	return &result, nil, nil
}

func (t *brokerHTTPTransport) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/finalize-env", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	body, err := json.Marshal(map[string]interface{}{"env": env})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, brokerHTTPError(resp)
	}
	var result RemoteAgentResponse
	if err := t.decodeResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (t *brokerHTTPTransport) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/logs", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	sep := "?"
	if tail > 0 {
		endpoint += fmt.Sprintf("?tail=%d", tail)
		sep = "&"
	}
	if projectID != "" {
		endpoint += sep + "projectId=" + url.QueryEscape(projectID)
	}
	resp, err := t.doRequest(ctx, brokerID, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", brokerHTTPError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	return string(body), nil
}

func (t *brokerHTTPTransport) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agents/%s/exec", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(agentID))
	if projectID != "" {
		endpoint += "?projectId=" + url.QueryEscape(projectID)
	}

	body, err := json.Marshal(map[string]interface{}{
		"command": command,
		"timeout": timeout,
	})
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := t.doRequest(ctx, brokerID, http.MethodPost, endpoint, body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", 0, brokerHTTPError(resp)
	}

	var result struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}
	if err := t.decodeResponse(resp, &result); err != nil {
		return "", 0, err
	}
	return result.Output, result.ExitCode, nil
}

func (t *brokerHTTPTransport) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug string) error {
	endpoint := fmt.Sprintf("%s/api/v1/projects/%s", strings.TrimSuffix(brokerEndpoint, "/"), url.PathEscape(projectSlug))
	resp, err := t.doRequest(ctx, brokerID, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return brokerHTTPError(resp)
	}
	return nil
}
