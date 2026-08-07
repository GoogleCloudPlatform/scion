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

//go:build !no_sqlite

// Package hub — tests for agent secret GET and LIST operations:
//
//   GET  /api/v1/agents/{agentID}/secrets/{key}   — retrieve a single secret
//   GET  /api/v1/agents/{agentID}/secrets          — list secret metadata

package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setupAgentSecretGetTest creates a test server with a project, agent, and a
// pre-existing secret for GET/LIST testing.
func setupAgentSecretGetTest(t *testing.T) (*Server, store.Store, string, string, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))
	ctx := context.Background()

	projectID := tid("project-agent-get-secret")
	project := &store.Project{
		ID: projectID, Name: "Agent Get Secret Project", Slug: "agent-get-secret-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-get-secret-1")
	agent := &store.Agent{
		ID: agentID, Slug: "get-secret-agent", Name: "Get Secret Agent",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, nil, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}

	return srv, s, agentID, projectID, agentToken
}

// seedSecret creates a secret in the backend for testing.
func seedSecret(t *testing.T, backend secret.SecretBackend, key, value, secretType, target, projectID string) {
	t.Helper()
	ctx := context.Background()
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	if target == "" {
		target = key
	}
	input := &secret.SetSecretInput{
		Name:       key,
		Value:      value,
		SecretType: secretType,
		Target:     target,
		Scope:      store.ScopeProject,
		ScopeID:    projectID,
		CreatedBy:  "test-user",
		UpdatedBy:  "test-user",
	}
	_, _, err := backend.Set(ctx, input)
	if err != nil {
		t.Fatalf("failed to seed secret %q: %v", key, err)
	}
}

// ============================================================================
// Agent GET secret tests
// ============================================================================

func TestAgentGetSecret_Success(t *testing.T) {
	srv, _, agentID, projectID, agentToken := setupAgentSecretGetTest(t)

	// Seed a secret.
	seedSecret(t, srv.secretBackend, "GET_KEY", "my-secret-value", "", "", projectID)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets/GET_KEY", nil, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AgentGetSecretResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Key != "GET_KEY" {
		t.Errorf("expected key GET_KEY, got %q", resp.Key)
	}
	// Value should be base64-encoded.
	decoded, err := base64.StdEncoding.DecodeString(resp.Value)
	if err != nil {
		t.Fatalf("failed to decode base64 value: %v", err)
	}
	if string(decoded) != "my-secret-value" {
		t.Errorf("expected value %q, got %q", "my-secret-value", string(decoded))
	}
	if resp.Type != store.SecretTypeEnvironment {
		t.Errorf("expected type %q, got %q", store.SecretTypeEnvironment, resp.Type)
	}
	if resp.Target != "GET_KEY" {
		t.Errorf("expected target GET_KEY, got %q", resp.Target)
	}
}

func TestAgentGetSecret_NotFound(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretGetTest(t)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets/NONEXISTENT_KEY", nil, agentToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentGetSecret_AgentIDMismatch(t *testing.T) {
	srv, _, _, _, agentToken := setupAgentSecretGetTest(t)

	// Use a different agentID in the URL than what's in the token.
	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+tid("wrong-agent-get")+"/secrets/SOME_KEY", nil, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for agent ID mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentGetSecret_NoAuth(t *testing.T) {
	srv, _ := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id"))

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/agents/some-agent/secrets/MY_KEY", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentGetSecret_UserTokenRejected(t *testing.T) {
	srv, _ := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id"))

	// Using dev token (user auth) should be rejected — agent-only endpoint.
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/agents/some-agent/secrets/MY_KEY", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for user token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentGetSecret_CrossProject(t *testing.T) {
	srv, s, _, _, agentToken := setupAgentSecretGetTest(t)
	ctx := context.Background()

	// Create a different project and seed a secret in it.
	otherProjectID := tid("project-other-get")
	otherProject := &store.Project{
		ID: otherProjectID, Name: "Other Project", Slug: "other-project-get",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, otherProject); err != nil {
		t.Fatalf("failed to create other project: %v", err)
	}

	// Create a second agent + token in the second project.
	otherAgentID := tid("agent-other-get")
	otherAgent := &store.Agent{
		ID: otherAgentID, Slug: "other-get-agent", Name: "Other Get Agent",
		ProjectID: otherProjectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, otherAgent); err != nil {
		t.Fatalf("failed to create other agent: %v", err)
	}

	// Seed a secret in the other project.
	seedSecret(t, srv.secretBackend, "OTHER_KEY", "other-value", "", "", otherProjectID)

	// The original agent's token should NOT be able to read a secret from
	// a different project (URL has a different agentID).
	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+otherAgentID+"/secrets/OTHER_KEY", nil, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-project access, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentGetSecret_FileType(t *testing.T) {
	srv, _, agentID, projectID, agentToken := setupAgentSecretGetTest(t)

	seedSecret(t, srv.secretBackend, "CERT_FILE", "cert-content", "file", "/etc/ssl/cert.pem", projectID)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets/CERT_FILE", nil, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AgentGetSecretResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Type != "file" {
		t.Errorf("expected type file, got %q", resp.Type)
	}
	if resp.Target != "/etc/ssl/cert.pem" {
		t.Errorf("expected target /etc/ssl/cert.pem, got %q", resp.Target)
	}
}

// ============================================================================
// Agent LIST secrets tests
// ============================================================================

func TestAgentListSecrets_ReturnsMetadataNoValues(t *testing.T) {
	srv, _, agentID, projectID, agentToken := setupAgentSecretGetTest(t)

	// Seed multiple secrets.
	seedSecret(t, srv.secretBackend, "LIST_KEY_1", "secret-1", "environment", "", projectID)
	seedSecret(t, srv.secretBackend, "LIST_KEY_2", "secret-2", "variable", "LIST_KEY_2", projectID)
	seedSecret(t, srv.secretBackend, "LIST_KEY_3", "secret-3", "file", "/tmp/file.txt", projectID)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets", nil, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Capture the body before the decoder consumes it.
	bodyBytes := rec.Body.Bytes()

	var resp AgentListSecretsResponse
	if err := json.Unmarshal(bodyBytes, &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Secrets) != 3 {
		t.Fatalf("expected 3 secrets, got %d", len(resp.Secrets))
	}

	// Verify no values are returned — check the raw JSON to ensure no
	// "value" field is present in any secret entry.
	var raw map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		t.Fatalf("failed to re-parse body: %v", err)
	}
	secrets, ok := raw["secrets"].([]interface{})
	if !ok {
		t.Fatal("expected secrets array in response")
	}
	for _, s := range secrets {
		sm, ok := s.(map[string]interface{})
		if !ok {
			t.Fatalf("expected secret entry to be a JSON object, got %T", s)
		}
		if _, exists := sm["value"]; exists {
			t.Errorf("list response should not contain a 'value' field, got: %v", sm)
		}
	}

	// Verify metadata is present.
	keyMap := make(map[string]AgentSecretMeta)
	for _, s := range resp.Secrets {
		keyMap[s.Key] = s
	}
	if s, ok := keyMap["LIST_KEY_1"]; !ok || s.Type != "environment" {
		t.Errorf("expected LIST_KEY_1 with type environment, got %+v", keyMap["LIST_KEY_1"])
	}
	if s, ok := keyMap["LIST_KEY_2"]; !ok || s.Type != "variable" {
		t.Errorf("expected LIST_KEY_2 with type variable, got %+v", keyMap["LIST_KEY_2"])
	}
	if s, ok := keyMap["LIST_KEY_3"]; !ok || s.Type != "file" || s.Target != "/tmp/file.txt" {
		t.Errorf("expected LIST_KEY_3 with type file and target /tmp/file.txt, got %+v", keyMap["LIST_KEY_3"])
	}
}

func TestAgentListSecrets_Empty(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretGetTest(t)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets", nil, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AgentListSecretsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Secrets) != 0 {
		t.Errorf("expected 0 secrets, got %d", len(resp.Secrets))
	}
}

func TestAgentListSecrets_NoAuth(t *testing.T) {
	srv, _ := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id"))

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/agents/some-agent/secrets", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentListSecrets_AgentIDMismatch(t *testing.T) {
	srv, _, _, _, agentToken := setupAgentSecretGetTest(t)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+tid("wrong-agent-list")+"/secrets", nil, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for agent ID mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentGetSecret_NoSecretBackend(t *testing.T) {
	srv, s := testServer(t)
	// Deliberately do NOT set a secret backend.
	ctx := context.Background()

	projectID := tid("project-no-backend-get")
	project := &store.Project{
		ID: projectID, Name: "No Backend Get Project", Slug: "no-backend-get-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-no-backend-get")
	agent := &store.Agent{
		ID: agentID, Slug: "no-backend-get-agent", Name: "No Backend Get Agent",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, nil, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", nil, agentToken)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 when secret backend is nil, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Existing PUT tests still pass (regression)
// ============================================================================

func TestAgentPutSecret_StillWorks(t *testing.T) {
	srv, _, agentID, projectID, agentToken := setupAgentSecretGetTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("my-new-secret")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/PUT_REG_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AgentSetSecretResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Key != "PUT_REG_KEY" {
		t.Errorf("expected key PUT_REG_KEY, got %q", resp.Key)
	}
	if resp.Scope != "project" {
		t.Errorf("expected scope project, got %q", resp.Scope)
	}
	if resp.ScopeID != projectID {
		t.Errorf("expected scopeId %q, got %q", projectID, resp.ScopeID)
	}

	// Now GET the secret we just PUT to verify the round trip.
	rec2 := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets/PUT_REG_KEY", nil, agentToken)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET after PUT, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var getResp AgentGetSecretResponse
	if err := json.NewDecoder(rec2.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(getResp.Value)
	if err != nil {
		t.Fatalf("failed to decode base64 value: %v", err)
	}
	if string(decoded) != "my-new-secret" {
		t.Errorf("GET after PUT: expected value %q, got %q", "my-new-secret", string(decoded))
	}
}

func TestAgentPutSecret_MethodNotAllowed_POST(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretGetTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentPutSecret_MethodNotAllowed_DELETE(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretGetTest(t)

	rec := doRequestWithAgentToken(t, srv, http.MethodDelete,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", nil, agentToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for DELETE, got %d: %s", rec.Code, rec.Body.String())
	}
}
