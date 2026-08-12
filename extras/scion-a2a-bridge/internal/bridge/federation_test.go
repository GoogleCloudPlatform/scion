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

package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// buildTestJWT creates a minimal unsigned JWT from the given claims for testing.
// The signature segment is a placeholder — decodeFederationToken doesn't verify it.
func buildTestJWT(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".fakesig"
}

func TestDecodeFederationToken_Valid(t *testing.T) {
	token := buildTestJWT(t, map[string]interface{}{
		"iss":        "https://hub.example.com",
		"sub":        "agent-uuid-123",
		"aud":        []string{"https://bridge.example.com"},
		"project_id": "my-project",
		"agent_name": "my-agent",
		"ancestry":   []string{"parent-agent", "grandparent-agent"},
		"root_user":  "alice@example.com",
	})

	caller, err := decodeFederationToken(token)
	if err != nil {
		t.Fatalf("decodeFederationToken returned error: %v", err)
	}

	if caller.TokenType != "federation" {
		t.Errorf("TokenType = %q, want %q", caller.TokenType, "federation")
	}
	if caller.AgentID != "agent-uuid-123" {
		t.Errorf("AgentID = %q, want %q", caller.AgentID, "agent-uuid-123")
	}
	if caller.IssuerURL != "https://hub.example.com" {
		t.Errorf("IssuerURL = %q, want %q", caller.IssuerURL, "https://hub.example.com")
	}
	if caller.ProjectID != "my-project" {
		t.Errorf("ProjectID = %q, want %q", caller.ProjectID, "my-project")
	}
	if len(caller.Ancestry) != 2 || caller.Ancestry[0] != "parent-agent" {
		t.Errorf("Ancestry = %v, want [parent-agent grandparent-agent]", caller.Ancestry)
	}
	if caller.RawToken != token {
		t.Error("RawToken should match the input token")
	}
	if !caller.IsAgent() {
		t.Error("IsAgent() should return true for federation callers")
	}
}

func TestDecodeFederationToken_MinimalClaims(t *testing.T) {
	token := buildTestJWT(t, map[string]interface{}{
		"sub": "minimal-agent",
		"iss": "https://hub.example.com",
	})

	caller, err := decodeFederationToken(token)
	if err != nil {
		t.Fatalf("decodeFederationToken returned error: %v", err)
	}

	if caller.AgentID != "minimal-agent" {
		t.Errorf("AgentID = %q, want %q", caller.AgentID, "minimal-agent")
	}
	if caller.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty", caller.ProjectID)
	}
	if len(caller.Ancestry) != 0 {
		t.Errorf("Ancestry = %v, want empty", caller.Ancestry)
	}
}

func TestDecodeFederationToken_MissingSub(t *testing.T) {
	token := buildTestJWT(t, map[string]interface{}{
		"iss": "https://hub.example.com",
		// no "sub"
	})

	_, err := decodeFederationToken(token)
	if err == nil {
		t.Fatal("expected error for missing sub claim, got nil")
	}
}

func TestDecodeFederationToken_NotJWT(t *testing.T) {
	_, err := decodeFederationToken("not-a-jwt")
	if err == nil {
		t.Fatal("expected error for non-JWT string, got nil")
	}
}

func TestDecodeFederationToken_InvalidPayload(t *testing.T) {
	// Two segments but second segment is not valid base64
	_, err := decodeFederationToken("header.!!!invalid!!!.sig")
	if err == nil {
		t.Fatal("expected error for invalid base64 payload, got nil")
	}
}

func TestDecodeFederationToken_InvalidJSON(t *testing.T) {
	// Valid base64 but not valid JSON
	payload := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	token := "header." + payload + ".sig"

	_, err := decodeFederationToken(token)
	if err == nil {
		t.Fatal("expected error for invalid JSON payload, got nil")
	}
}

func TestFlexibleAudience(t *testing.T) {
	t.Run("string aud", func(t *testing.T) {
		// RFC 7519 section 4.1.3: aud may be a single string.
		token := buildTestJWT(t, map[string]interface{}{
			"iss": "https://hub.example.com",
			"sub": "agent-string-aud",
			"aud": "https://bridge.example.com",
		})

		caller, err := decodeFederationToken(token)
		if err != nil {
			t.Fatalf("decodeFederationToken returned error: %v", err)
		}
		if caller.AgentID != "agent-string-aud" {
			t.Errorf("AgentID = %q, want %q", caller.AgentID, "agent-string-aud")
		}
	})

	t.Run("array aud", func(t *testing.T) {
		// RFC 7519 section 4.1.3: aud may be an array of strings.
		token := buildTestJWT(t, map[string]interface{}{
			"iss": "https://hub.example.com",
			"sub": "agent-array-aud",
			"aud": []string{"https://bridge.example.com", "https://other.example.com"},
		})

		caller, err := decodeFederationToken(token)
		if err != nil {
			t.Fatalf("decodeFederationToken returned error: %v", err)
		}
		if caller.AgentID != "agent-array-aud" {
			t.Errorf("AgentID = %q, want %q", caller.AgentID, "agent-array-aud")
		}
	})

	t.Run("missing aud", func(t *testing.T) {
		// aud is optional — should still decode without error.
		token := buildTestJWT(t, map[string]interface{}{
			"iss": "https://hub.example.com",
			"sub": "agent-no-aud",
		})

		caller, err := decodeFederationToken(token)
		if err != nil {
			t.Fatalf("decodeFederationToken returned error: %v", err)
		}
		if caller.AgentID != "agent-no-aud" {
			t.Errorf("AgentID = %q, want %q", caller.AgentID, "agent-no-aud")
		}
	})
}

func TestCallerKey_Agent(t *testing.T) {
	tests := []struct {
		name    string
		caller  CallerIdentity
		wantKey string
	}{
		{
			name: "agent with issuer host",
			caller: CallerIdentity{
				AgentID:   "agent-123",
				IssuerURL: "https://hub.example.com",
				TokenType: "federation",
			},
			wantKey: "agent:hub.example.com:agent-123",
		},
		{
			name: "agent without issuer",
			caller: CallerIdentity{
				AgentID:   "agent-123",
				TokenType: "federation",
			},
			wantKey: "agent:agent-123",
		},
		{
			name: "agent with invalid issuer URL",
			caller: CallerIdentity{
				AgentID:   "agent-456",
				IssuerURL: "://bad-url",
				TokenType: "federation",
			},
			wantKey: "agent:agent-456",
		},
		{
			name: "user caller",
			caller: CallerIdentity{
				UserID:    "user-alice",
				Email:     "alice@example.com",
				TokenType: "uat",
			},
			wantKey: "user-alice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.caller.CallerKey()
			if got != tt.wantKey {
				t.Errorf("CallerKey() = %q, want %q", got, tt.wantKey)
			}
		})
	}
}

func TestIsAgent(t *testing.T) {
	agent := &CallerIdentity{AgentID: "agent-1", TokenType: "federation"}
	if !agent.IsAgent() {
		t.Error("IsAgent() should return true when AgentID is set")
	}

	user := &CallerIdentity{UserID: "user-1", TokenType: "uat"}
	if user.IsAgent() {
		t.Error("IsAgent() should return false when AgentID is empty")
	}
}

func TestAuthMiddleware_Federation(t *testing.T) {
	token := buildTestJWT(t, map[string]interface{}{
		"iss":        "https://hub.example.com",
		"sub":        "fed-agent-1",
		"project_id": "test-project",
	})

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
		Auth:   AuthConfig{Scheme: "federation"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)

	// Use a handler that echoes back the CallerIdentity fields.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := callerIdentityFromContext(r.Context())
		if caller != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agent_id":   caller.AgentID,
				"issuer_url": caller.IssuerURL,
				"project_id": caller.ProjectID,
				"token_type": caller.TokenType,
				"is_agent":   caller.IsAgent(),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"caller":"none"}`))
	})

	srv := NewServer(b, cfg, nil, log, handler)
	mw := srv.authMiddleware(handler)

	t.Run("valid federation token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}

		var body map[string]interface{}
		json.NewDecoder(w.Body).Decode(&body)
		if body["agent_id"] != "fed-agent-1" {
			t.Errorf("agent_id = %v, want fed-agent-1", body["agent_id"])
		}
		if body["issuer_url"] != "https://hub.example.com" {
			t.Errorf("issuer_url = %v, want https://hub.example.com", body["issuer_url"])
		}
		if body["token_type"] != "federation" {
			t.Errorf("token_type = %v, want federation", body["token_type"])
		}
		if body["is_agent"] != true {
			t.Errorf("is_agent = %v, want true", body["is_agent"])
		}
	})

	t.Run("missing token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})

	t.Run("malformed token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/projects/p/agents/a/jsonrpc", nil)
		req.Header.Set("Authorization", "Bearer not-a-jwt")
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", w.Code)
		}
	})

	t.Run("public endpoint bypasses auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
		// No Authorization header
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("public endpoint: status = %d, want 200", w.Code)
		}
	})
}

func TestFederationHeaderTransport(t *testing.T) {
	// Create a test server that echoes back the federation token header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fedToken := r.Header.Get(FederationTokenHeader)
		json.NewEncoder(w).Encode(map[string]string{
			"federation_token": fedToken,
		})
	}))
	defer srv.Close()

	transport := &federationHeaderTransport{
		base:  http.DefaultTransport,
		token: "test-token-123",
	}
	client := &http.Client{Transport: transport}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)

	if body["federation_token"] != "test-token-123" {
		t.Errorf("federation_token = %q, want %q", body["federation_token"], "test-token-123")
	}
}

func TestValidateConfig_Federation(t *testing.T) {
	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
		Auth:   AuthConfig{Scheme: "federation"},
	}

	if err := ValidateConfig(cfg); err != nil {
		t.Errorf("federation scheme should be valid: %v", err)
	}
}

func TestPerAgentTaskIsolation(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "agent-iso-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://test"},
		Hub:    HubConfig{Endpoint: "http://hub", User: "admin@test"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)

	now := time.Now()

	// Create tasks for a federation agent caller and a user caller.
	agentCallerKey := "agent:hub.example.com:agent-123"
	store.CreateTask(context.Background(), &state.Task{
		ID: "task-agent", ContextID: "ctx-1", ProjectID: "proj-1",
		AgentSlug: "agent-1", State: "working", CallerUserID: agentCallerKey,
		CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})
	store.CreateTask(context.Background(), &state.Task{
		ID: "task-user", ContextID: "ctx-1", ProjectID: "proj-1",
		AgentSlug: "agent-1", State: "working", CallerUserID: "user-alice",
		CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	agentCtx := withCallerIdentity(context.Background(), &CallerIdentity{
		AgentID:   "agent-123",
		IssuerURL: "https://hub.example.com",
		TokenType: "federation",
	})
	userCtx := withCallerIdentity(context.Background(), &CallerIdentity{
		UserID:    "user-alice",
		Email:     "alice@test",
		TokenType: "uat",
	})

	// Agent caller can see its own task.
	result, err := b.GetTask(agentCtx, "task-agent")
	if err != nil {
		t.Fatalf("Agent GetTask own: %v", err)
	}
	if result == nil {
		t.Fatal("Agent should be able to get its own task")
	}

	// Agent caller cannot see user's task.
	result, err = b.GetTask(agentCtx, "task-user")
	if err != nil {
		t.Fatalf("Agent GetTask user: %v", err)
	}
	if result != nil {
		t.Fatal("Agent should NOT be able to get user's task")
	}

	// User caller cannot see agent's task.
	result, err = b.GetTask(userCtx, "task-agent")
	if err != nil {
		t.Fatalf("User GetTask agent: %v", err)
	}
	if result != nil {
		t.Fatal("User should NOT be able to get agent's task")
	}

	// User caller can see its own task.
	result, err = b.GetTask(userCtx, "task-user")
	if err != nil {
		t.Fatalf("User GetTask own: %v", err)
	}
	if result == nil {
		t.Fatal("User should be able to get its own task")
	}

	// ListTasks: Agent sees only its tasks.
	results, err := b.ListTasks(agentCtx, "ctx-1")
	if err != nil {
		t.Fatalf("Agent ListTasks: %v", err)
	}
	if len(results) != 1 || results[0].ID != "task-agent" {
		t.Errorf("Agent ListTasks = %d tasks, want 1 (task-agent)", len(results))
	}

	// ListTasks: User sees only their tasks.
	results, err = b.ListTasks(userCtx, "ctx-1")
	if err != nil {
		t.Fatalf("User ListTasks: %v", err)
	}
	if len(results) != 1 || results[0].ID != "task-user" {
		t.Errorf("User ListTasks = %d tasks, want 1 (task-user)", len(results))
	}

	// CancelTask: Agent cannot cancel user's task.
	cancelResult, err := b.CancelTask(agentCtx, "task-user")
	if err != nil {
		t.Fatalf("Agent CancelTask user: %v", err)
	}
	if cancelResult != nil {
		t.Fatal("Agent should NOT be able to cancel user's task")
	}
}

func TestFederationTokenHeaderMatchesHub(t *testing.T) {
	// The bridge and hub define FederationTokenHeader independently because
	// they live in different packages (internal/bridge vs pkg/hub). This test
	// catches silent drift between the two constants.
	const hubFederationTokenHeader = "X-Scion-Federation-Token" // from pkg/hub/federation_auth.go
	if FederationTokenHeader != hubFederationTokenHeader {
		t.Errorf("bridge.FederationTokenHeader = %q, want %q (must match pkg/hub/federation_auth.go)",
			FederationTokenHeader, hubFederationTokenHeader)
	}
}
