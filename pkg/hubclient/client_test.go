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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	client, err := New("https://hub.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.Agents() == nil {
		t.Error("expected non-nil agents service")
	}
	if client.Groves() == nil {
		t.Error("expected non-nil groves service")
	}
	if client.RuntimeBrokers() == nil {
		t.Error("expected non-nil runtime brokers service")
	}
	if client.Templates() == nil {
		t.Error("expected non-nil templates service")
	}
	if client.Workspace() == nil {
		t.Error("expected non-nil workspace service")
	}
	if client.Users() == nil {
		t.Error("expected non-nil users service")
	}
	if client.Env() == nil {
		t.Error("expected non-nil env service")
	}
	if client.Secrets() == nil {
		t.Error("expected non-nil secrets service")
	}
	if client.Auth() == nil {
		t.Error("expected non-nil auth service")
	}
}

func TestHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("expected path /healthz, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(HealthResponse{
			Status:  "ok",
			Version: "1.0.0",
			Uptime:  "1h30m",
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", health.Status)
	}
	if health.Version != "1.0.0" {
		t.Errorf("expected version '1.0.0', got %q", health.Version)
	}
}

func TestAgentsList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents" {
			t.Errorf("expected path /api/v1/agents, got %s", r.URL.Path)
		}

		// Check query params
		if r.URL.Query().Get("groveId") != "grove-123" {
			t.Errorf("expected groveId=grove-123, got %s", r.URL.Query().Get("groveId"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agents": []Agent{
				{
					ID:      "uuid-1",
					Slug: "agent-1",
					Name:    "Test Agent 1",
					Status:  "running",
				},
				{
					ID:      "uuid-2",
					Slug: "agent-2",
					Name:    "Test Agent 2",
					Status:  "stopped",
				},
			},
			"totalCount": 2,
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Agents().List(context.Background(), &ListAgentsOptions{
		GroveID: "grove-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(resp.Agents))
	}
	if resp.Agents[0].Name != "Test Agent 1" {
		t.Errorf("expected name 'Test Agent 1', got %q", resp.Agents[0].Name)
	}
}

func TestAgentsGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/test-agent" {
			t.Errorf("expected path /api/v1/agents/test-agent, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Agent{
			ID:      "uuid-123",
			Slug: "test-agent",
			Name:    "Test Agent",
			Status:  "running",
			Created: time.Now(),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	agent, err := client.Agents().Get(context.Background(), "test-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Slug != "test-agent" {
		t.Errorf("expected agentId 'test-agent', got %q", agent.Slug)
	}
	if agent.Name != "Test Agent" {
		t.Errorf("expected name 'Test Agent', got %q", agent.Name)
	}
}

func TestAgentsCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var req CreateAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Name != "new-agent" {
			t.Errorf("expected name 'new-agent', got %q", req.Name)
		}
		if req.GroveID != "grove-123" {
			t.Errorf("expected groveId 'grove-123', got %q", req.GroveID)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(CreateAgentResponse{
			Agent: &Agent{
				ID:      "uuid-new",
				Slug: "new-agent",
				Name:    "new-agent",
				GroveID: "grove-123",
				Status:  "provisioning",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Agents().Create(context.Background(), &CreateAgentRequest{
		Name:    "new-agent",
		GroveID: "grove-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Agent.Slug != "new-agent" {
		t.Errorf("expected agentId 'new-agent', got %q", resp.Agent.Slug)
	}
}

func TestAgentsDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/agent-to-delete" {
			t.Errorf("expected path /api/v1/agents/agent-to-delete, got %s", r.URL.Path)
		}

		// Check query params
		if r.URL.Query().Get("deleteFiles") != "true" {
			t.Errorf("expected deleteFiles=true")
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, _ := New(server.URL)
	err := client.Agents().Delete(context.Background(), "agent-to-delete", &DeleteAgentOptions{
		DeleteFiles: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGrovesRegister(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/groves/register" {
			t.Errorf("expected path /api/v1/groves/register, got %s", r.URL.Path)
		}

		var req RegisterGroveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(RegisterGroveResponse{
			Grove: &Grove{
				ID:        "grove-uuid",
				Name:      req.Name,
				GitRemote: req.GitRemote,
			},
			Broker: &RuntimeBroker{
				ID:   "host-uuid",
				Name: req.Broker.Name,
			},
			Created:   true,
			BrokerToken: "secret-host-token",
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Groves().Register(context.Background(), &RegisterGroveRequest{
		Name:      "my-project",
		GitRemote: "git@github.com:org/repo.git",
		Path:      "/path/to/.scion",
		Broker: &BrokerInfo{
			Name:    "Dev Laptop",
			Version: "1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Created {
		t.Error("expected created=true")
	}
	if resp.BrokerToken != "secret-host-token" {
		t.Errorf("expected brokerToken 'secret-host-token', got %q", resp.BrokerToken)
	}
}

func TestWithBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-token" {
			t.Errorf("expected 'Bearer my-token', got %q", auth)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	}))
	defer server.Close()

	client, _ := New(server.URL, WithBearerToken("my-token"))
	_, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWithAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != "my-api-key" {
			t.Errorf("expected 'my-api-key', got %q", apiKey)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	}))
	defer server.Close()

	client, _ := New(server.URL, WithAPIKey("my-api-key"))
	_, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnvList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/env" {
			t.Errorf("expected path /api/v1/env, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("scope") != "grove" {
			t.Errorf("expected scope=grove, got %s", r.URL.Query().Get("scope"))
		}
		if r.URL.Query().Get("scopeId") != "grove-123" {
			t.Errorf("expected scopeId=grove-123, got %s", r.URL.Query().Get("scopeId"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ListEnvResponse{
			EnvVars: []EnvVar{
				{ID: "1", Key: "API_URL", Value: "https://api.example.com", Scope: "grove", ScopeID: "grove-123"},
				{ID: "2", Key: "LOG_LEVEL", Value: "debug", Scope: "grove", ScopeID: "grove-123"},
			},
			Scope:   "grove",
			ScopeID: "grove-123",
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Env().List(context.Background(), &ListEnvOptions{
		Scope:   "grove",
		ScopeID: "grove-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.EnvVars) != 2 {
		t.Errorf("expected 2 env vars, got %d", len(resp.EnvVars))
	}
	if resp.EnvVars[0].Key != "API_URL" {
		t.Errorf("expected key 'API_URL', got %q", resp.EnvVars[0].Key)
	}
}

func TestEnvGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/env/API_URL" {
			t.Errorf("expected path /api/v1/env/API_URL, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(EnvVar{
			ID:      "uuid-123",
			Key:     "API_URL",
			Value:   "https://api.example.com",
			Scope:   "user",
			Created: time.Now(),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	envVar, err := client.Env().Get(context.Background(), "API_URL", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if envVar.Key != "API_URL" {
		t.Errorf("expected key 'API_URL', got %q", envVar.Key)
	}
	if envVar.Value != "https://api.example.com" {
		t.Errorf("expected value 'https://api.example.com', got %q", envVar.Value)
	}
}

func TestEnvSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/env/LOG_LEVEL" {
			t.Errorf("expected path /api/v1/env/LOG_LEVEL, got %s", r.URL.Path)
		}

		var req SetEnvRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Value != "debug" {
			t.Errorf("expected value 'debug', got %q", req.Value)
		}
		if req.Scope != "grove" {
			t.Errorf("expected scope 'grove', got %q", req.Scope)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SetEnvResponse{
			EnvVar: &EnvVar{
				ID:      "uuid-new",
				Key:     "LOG_LEVEL",
				Value:   "debug",
				Scope:   "grove",
				ScopeID: "grove-123",
			},
			Created: true,
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Env().Set(context.Background(), "LOG_LEVEL", &SetEnvRequest{
		Value:   "debug",
		Scope:   "grove",
		ScopeID: "grove-123",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Created {
		t.Error("expected created=true")
	}
	if resp.EnvVar.Key != "LOG_LEVEL" {
		t.Errorf("expected key 'LOG_LEVEL', got %q", resp.EnvVar.Key)
	}
}

func TestEnvDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/env/OLD_VAR" {
			t.Errorf("expected path /api/v1/env/OLD_VAR, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("scope") != "user" {
			t.Errorf("expected scope=user, got %s", r.URL.Query().Get("scope"))
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, _ := New(server.URL)
	err := client.Env().Delete(context.Background(), "OLD_VAR", &EnvScopeOptions{Scope: "user"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSecretList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/secrets" {
			t.Errorf("expected path /api/v1/secrets, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ListSecretResponse{
			Secrets: []Secret{
				{ID: "1", Key: "API_KEY", Scope: "user", Version: 1},
				{ID: "2", Key: "DATABASE_PASSWORD", Scope: "user", Version: 3},
			},
			Scope: "user",
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Secrets().List(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Secrets) != 2 {
		t.Errorf("expected 2 secrets, got %d", len(resp.Secrets))
	}
	if resp.Secrets[0].Key != "API_KEY" {
		t.Errorf("expected key 'API_KEY', got %q", resp.Secrets[0].Key)
	}
	// Value should NOT be present (write-only)
}

func TestSecretGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/secrets/API_KEY" {
			t.Errorf("expected path /api/v1/secrets/API_KEY, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Secret{
			ID:      "uuid-123",
			Key:     "API_KEY",
			Scope:   "user",
			Version: 2,
			Created: time.Now(),
			Updated: time.Now(),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	secret, err := client.Secrets().Get(context.Background(), "API_KEY", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secret.Key != "API_KEY" {
		t.Errorf("expected key 'API_KEY', got %q", secret.Key)
	}
	if secret.Version != 2 {
		t.Errorf("expected version 2, got %d", secret.Version)
	}
}

func TestSecretSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/secrets/NEW_SECRET" {
			t.Errorf("expected path /api/v1/secrets/NEW_SECRET, got %s", r.URL.Path)
		}

		var req SetSecretRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Value != "secret-value" {
			t.Errorf("expected value 'secret-value', got %q", req.Value)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SetSecretResponse{
			Secret: &Secret{
				ID:      "uuid-new",
				Key:     "NEW_SECRET",
				Scope:   "user",
				Version: 1,
			},
			Created: true,
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Secrets().Set(context.Background(), "NEW_SECRET", &SetSecretRequest{
		Value: "secret-value",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Created {
		t.Error("expected created=true")
	}
	if resp.Secret.Key != "NEW_SECRET" {
		t.Errorf("expected key 'NEW_SECRET', got %q", resp.Secret.Key)
	}
}

func TestSecretDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/secrets/OLD_SECRET" {
			t.Errorf("expected path /api/v1/secrets/OLD_SECRET, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, _ := New(server.URL)
	err := client.Secrets().Delete(context.Background(), "OLD_SECRET", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
