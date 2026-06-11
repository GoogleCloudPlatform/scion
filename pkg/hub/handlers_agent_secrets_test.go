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

func setupAgentSecretTest(t *testing.T) (*Server, store.Store, string, string, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id"))
	ctx := context.Background()

	projectID := tid("project-agent-secret")
	project := &store.Project{
		ID: projectID, Name: "Agent Secret Project", Slug: "agent-secret-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-secret-1")
	agent := &store.Agent{
		ID: agentID, Slug: "secret-agent", Name: "Secret Agent",
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

func TestAgentSecrets_CreateSuccess(t *testing.T) {
	srv, _, agentID, projectID, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("my-secret-value")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AgentSetSecretResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Key != "MY_KEY" {
		t.Errorf("expected key MY_KEY, got %q", resp.Key)
	}
	if resp.Scope != "project" {
		t.Errorf("expected scope project, got %q", resp.Scope)
	}
	if resp.ScopeID != projectID {
		t.Errorf("expected scopeId %q, got %q", projectID, resp.ScopeID)
	}
}

func TestAgentSecrets_FileTypeSuccess(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte(`{"token":"abc"}`)),
		Type:   "file",
		Target: "~/.claude/.credentials.json",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/CLAUDE_AUTH", body, agentToken)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_ConflictWithoutForce(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value-1")),
	}

	// First create should succeed.
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/DUP_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first create, got %d: %s", rec.Code, rec.Body.String())
	}

	// Second create without force should return 409.
	body.Value = base64.StdEncoding.EncodeToString([]byte("value-2"))
	rec = doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/DUP_KEY", body, agentToken)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_ForceOverwrite(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value-1")),
	}

	// Create secret.
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/FORCE_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Force overwrite should return 204.
	body.Value = base64.StdEncoding.EncodeToString([]byte("value-2"))
	body.Force = true
	rec = doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/FORCE_KEY", body, agentToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on force overwrite, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_NoAuth(t *testing.T) {
	srv, _ := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id"))

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	rec := doRequestNoAuth(t, srv, http.MethodPut,
		"/api/v1/agents/some-agent/secrets/MY_KEY", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_UserTokenRejected(t *testing.T) {
	srv, _ := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id"))

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	// Using dev token (user auth) should be rejected — agent-only endpoint.
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/agents/some-agent/secrets/MY_KEY", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for user token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_AgentIDMismatch(t *testing.T) {
	srv, _, _, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	// Use a different agentID in the URL than what's in the token.
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+tid("wrong-agent")+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for agent ID mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_EmptyValue(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: "",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty value, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_InvalidType(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
		Type:  "invalid",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid type, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_FileTypeNoAbsTarget(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value:  base64.StdEncoding.EncodeToString([]byte("data")),
		Type:   "file",
		Target: "relative/path",
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for relative file target, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_MethodNotAllowed(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	// POST should not be allowed.
	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_MissingKey(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	// URL with no key (just /secrets or /secrets/).
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_InvalidKeyChars(t *testing.T) {
	srv, _, agentID, _, agentToken := setupAgentSecretTest(t)

	// URL-encode keys that contain invalid characters. httptest.NewRequest
	// panics on raw spaces/tabs, so we use percent-encoding as a real client would.
	for _, tc := range []struct{ label, key string }{
		{"space", "MY%20KEY"},
		{"equals", "MY=KEY"},
	} {
		body := AgentSetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("value")),
		}
		rec := doRequestWithAgentToken(t, srv, http.MethodPut,
			"/api/v1/agents/"+agentID+"/secrets/"+tc.key, body, agentToken)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for key with %s, got %d: %s", tc.label, rec.Code, rec.Body.String())
		}
	}
}

func TestAgentSecrets_NoSecretBackend(t *testing.T) {
	srv, s := testServer(t)
	// Deliberately do NOT set a secret backend.
	ctx := context.Background()

	projectID := tid("project-no-backend")
	project := &store.Project{
		ID: projectID, Name: "No Backend Project", Slug: "no-backend-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-no-backend")
	agent := &store.Agent{
		ID: agentID, Slug: "no-backend-agent", Name: "No Backend Agent",
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

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("value")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/MY_KEY", body, agentToken)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 when secret backend is nil, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecrets_CreatedByIsAgent(t *testing.T) {
	srv, s, agentID, projectID, agentToken := setupAgentSecretTest(t)

	body := AgentSetSecretRequest{
		Value: base64.StdEncoding.EncodeToString([]byte("check-provenance")),
	}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut,
		"/api/v1/agents/"+agentID+"/secrets/PROV_KEY", body, agentToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the secret was stored with agent provenance.
	ctx := context.Background()
	stored, err := s.GetSecret(ctx, "PROV_KEY", store.ScopeProject, projectID)
	if err != nil {
		t.Fatalf("failed to get stored secret: %v", err)
	}
	expected := "agent:" + agentID
	if stored.CreatedBy != expected {
		t.Errorf("expected createdBy %q, got %q", expected, stored.CreatedBy)
	}
}
