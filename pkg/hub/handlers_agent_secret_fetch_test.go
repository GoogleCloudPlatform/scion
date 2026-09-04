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

// Package hub — tests for the agent self-service secret fetch endpoint:
//
//	POST /api/v1/agent/secrets

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setupAgentSecretFetchTest creates a test server with a project, agent, and
// optional pre-seeded secrets for the POST /api/v1/agent/secrets endpoint.
func setupAgentSecretFetchTest(t *testing.T) (*Server, store.Store, string, string, string) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-agent-fetch-secret")
	project := &store.Project{
		ID: projectID, Name: "Agent Fetch Secret Project", Slug: "agent-fetch-secret-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-fetch-secret-1")
	agent := &store.Agent{
		ID: agentID, Slug: "fetch-secret-agent", Name: "Fetch Secret Agent",
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

func TestAgentSecretFetch_Success(t *testing.T) {
	srv, _, _, projectID, agentToken := setupAgentSecretFetchTest(t)

	// Seed secrets.
	seedSecret(t, srv.secretBackend, "SECRET_A", "value-a", "", "", projectID)
	seedSecret(t, srv.secretBackend, "SECRET_B", "value-b", "", "", projectID)

	body := secretFetchRequest{
		Keys: []string{"SECRET_A", "SECRET_B"},
	}

	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agent/secrets", body, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp secretFetchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(resp.Secrets))
	}

	resultMap := make(map[string]secretFetchResult)
	for _, r := range resp.Secrets {
		resultMap[r.Key] = r
	}

	a := resultMap["SECRET_A"]
	if a.Status != "ok" {
		t.Errorf("expected SECRET_A status ok, got %q", a.Status)
	}
	if a.Value != "value-a" {
		t.Errorf("expected SECRET_A value %q, got %q", "value-a", a.Value)
	}

	b := resultMap["SECRET_B"]
	if b.Status != "ok" {
		t.Errorf("expected SECRET_B status ok, got %q", b.Status)
	}
	if b.Value != "value-b" {
		t.Errorf("expected SECRET_B value %q, got %q", "value-b", b.Value)
	}
}

func TestAgentSecretFetch_NotFound(t *testing.T) {
	srv, _, _, projectID, agentToken := setupAgentSecretFetchTest(t)

	// Seed one secret, request two (one that doesn't exist).
	seedSecret(t, srv.secretBackend, "EXISTS_KEY", "value", "", "", projectID)

	body := secretFetchRequest{
		Keys: []string{"EXISTS_KEY", "MISSING_KEY"},
	}

	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agent/secrets", body, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp secretFetchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Secrets) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Secrets))
	}

	resultMap := make(map[string]secretFetchResult)
	for _, r := range resp.Secrets {
		resultMap[r.Key] = r
	}

	exists := resultMap["EXISTS_KEY"]
	if exists.Status != "ok" {
		t.Errorf("expected EXISTS_KEY status ok, got %q", exists.Status)
	}

	missing := resultMap["MISSING_KEY"]
	if missing.Status != "not_found" {
		t.Errorf("expected MISSING_KEY status not_found, got %q", missing.Status)
	}
	if missing.Error != "secret not found" {
		t.Errorf("expected error message %q, got %q", "secret not found", missing.Error)
	}
	if missing.Value != "" {
		t.Errorf("expected no value for not_found key, got %q", missing.Value)
	}
}

func TestAgentSecretFetch_NoAuth(t *testing.T) {
	srv, _ := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id", "test-secret"))

	body := secretFetchRequest{Keys: []string{"MY_KEY"}}

	rec := doRequestNoAuth(t, srv, http.MethodPost,
		"/api/v1/agent/secrets", body)
	// The route guard rejects unauthenticated requests before the handler runs.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecretFetch_EmptyKeys(t *testing.T) {
	srv, _, _, _, agentToken := setupAgentSecretFetchTest(t)

	body := secretFetchRequest{Keys: []string{}}

	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agent/secrets", body, agentToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty keys, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecretFetch_MethodNotAllowed(t *testing.T) {
	srv, _, _, _, agentToken := setupAgentSecretFetchTest(t)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet,
		"/api/v1/agent/secrets", nil, agentToken)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentSecretFetch_CrossProjectIsolation(t *testing.T) {
	srv, s, _, _, agentToken := setupAgentSecretFetchTest(t)
	ctx := context.Background()

	// Create a different project with a secret.
	otherProjectID := tid("project-other-fetch")
	otherProject := &store.Project{
		ID: otherProjectID, Name: "Other Fetch Project", Slug: "other-fetch-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, otherProject); err != nil {
		t.Fatalf("failed to create other project: %v", err)
	}

	// Seed a secret in the OTHER project.
	seedSecret(t, srv.secretBackend, "OTHER_SECRET", "other-value", "", "", otherProjectID)

	// The agent (whose token is for the original project) should NOT see the
	// other project's secret.
	body := secretFetchRequest{Keys: []string{"OTHER_SECRET"}}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agent/secrets", body, agentToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp secretFetchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Secrets) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Secrets))
	}
	if resp.Secrets[0].Status != "not_found" {
		t.Errorf("expected not_found for cross-project secret, got %q", resp.Secrets[0].Status)
	}
}

func TestAgentSecretFetch_NoSecretBackend(t *testing.T) {
	srv, s := testServer(t)
	// Deliberately do NOT set a secret backend.
	ctx := context.Background()

	projectID := tid("project-no-backend-fetch")
	project := &store.Project{
		ID: projectID, Name: "No Backend Fetch Project", Slug: "no-backend-fetch-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agentID := tid("agent-no-backend-fetch")
	agent := &store.Agent{
		ID: agentID, Slug: "no-backend-fetch-agent", Name: "No Backend Fetch Agent",
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

	body := secretFetchRequest{Keys: []string{"MY_KEY"}}
	rec := doRequestWithAgentToken(t, srv, http.MethodPost,
		"/api/v1/agent/secrets", body, agentToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when secret backend is nil, got %d: %s", rec.Code, rec.Body.String())
	}
}
