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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"
)

// okHandler is a simple handler that returns 200 for middleware pass-through tests.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// TestRequireFederationAccess_LocalAgentWithScope verifies that a local agent
// identity with the required scope passes through the middleware.
func TestRequireFederationAccess_LocalAgentWithScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)

	claims := &AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "local-agent-1"},
		ProjectID: "project-1",
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	}
	identity := &agentIdentityWrapper{claims}

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRequireFederationAccess_LocalAgentWithoutScope verifies that a local agent
// identity without the required scope is rejected with 403.
func TestRequireFederationAccess_LocalAgentWithoutScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeProjectSecretRead)

	claims := &AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "local-agent-1"},
		ProjectID: "project-1",
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
	}
	identity := &agentIdentityWrapper{claims}

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing required scope") {
		t.Errorf("expected 'missing required scope' in body, got: %s", rec.Body.String())
	}
}

// TestRequireFederationAccess_FederatedAgentWithScope verifies that a federated
// agent identity with the required scope passes through the middleware.
func TestRequireFederationAccess_FederatedAgentWithScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)

	identity := NewFederatedAgentIdentity(
		"https://hub-a.example.com",
		"agent-123",
		"project-alpha",
		"worker-1",
		"user:alice",
		[]string{"user:alice", "agent:root"},
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRequireFederationAccess_FederatedAgentWithoutScope verifies that a federated
// agent identity without the required scope is rejected with 403.
func TestRequireFederationAccess_FederatedAgentWithoutScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeProjectSecretRead)

	identity := NewFederatedAgentIdentity(
		"https://hub-a.example.com",
		"agent-123",
		"project-alpha",
		"worker-1",
		"user:alice",
		[]string{"user:alice", "agent:root"},
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing required scope") {
		t.Errorf("expected 'missing required scope' in body, got: %s", rec.Body.String())
	}
}

// TestRequireFederationAccess_NoIdentity verifies that a request with no
// identity on the context is rejected with 401.
func TestRequireFederationAccess_NoIdentity(t *testing.T) {
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	// No identity on context

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication required") {
		t.Errorf("expected 'authentication required' in body, got: %s", rec.Body.String())
	}
}

// TestRequireFederationAccess_UserIdentity verifies that a user identity
// (not an agent) on the context is rejected with 403, since
// UserIdentity does not implement scopeChecker.
func TestRequireFederationAccess_UserIdentity(t *testing.T) {
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)

	user := NewAuthenticatedUser("user-1", "alice@example.com", "Alice", "admin", "web")

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), user))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not support scope-based access") {
		t.Errorf("expected 'does not support scope-based access' in body, got: %s", rec.Body.String())
	}
}

// TestRequireFederationAccess_FederatedServiceWithScope verifies that a federated
// service identity with the required scope passes through the middleware.
func TestRequireFederationAccess_FederatedServiceWithScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)

	identity := NewFederatedServiceIdentity(
		"https://accounts.google.com",
		"123456789",
		"my-sa@my-project.iam.gserviceaccount.com",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRequireFederationAccess_FederatedServiceWithoutScope verifies that a federated
// service identity without the required scope is rejected with 403.
func TestRequireFederationAccess_FederatedServiceWithoutScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeProjectSecretRead)

	identity := NewFederatedServiceIdentity(
		"https://accounts.google.com",
		"123456789",
		"my-sa@my-project.iam.gserviceaccount.com",
		[]AgentTokenScope{ScopeAgentStatusUpdate},
	)

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing required scope") {
		t.Errorf("expected 'missing required scope' in body, got: %s", rec.Body.String())
	}
}

// TestRequireFederationAccess_FederatedUserWithScope verifies that a federated
// user identity with the required scope passes through the middleware.
func TestRequireFederationAccess_FederatedUserWithScope(t *testing.T) {
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)

	identity := NewFederatedUserIdentity(
		"https://securetoken.google.com/my-project",
		"abcdef123456",
		"user@example.com",
		"Test User",
		"viewer",
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRequireFederationAccess_DefaultFederationScopes verifies that a federated
// agent with DefaultFederationScopes has ScopeAgentStatusUpdate but not
// ScopeProjectSecretRead.
func TestRequireFederationAccess_DefaultFederationScopes(t *testing.T) {
	identity := NewFederatedAgentIdentity(
		"https://hub-a.example.com",
		"agent-123",
		"project-alpha",
		"worker-1",
		"user:alice",
		[]string{"user:alice"},
		DefaultFederationScopes,
	)

	// HasScope(ScopeAgentStatusUpdate) should return true
	if !identity.HasScope(ScopeAgentStatusUpdate) {
		t.Error("expected HasScope(ScopeAgentStatusUpdate) to return true with DefaultFederationScopes")
	}

	// HasScope(ScopeProjectSecretRead) should return false
	if identity.HasScope(ScopeProjectSecretRead) {
		t.Error("expected HasScope(ScopeProjectSecretRead) to return false with DefaultFederationScopes")
	}

	// Verify via middleware: ScopeAgentStatusUpdate -> 200
	middleware := RequireFederationAccess(ScopeAgentStatusUpdate)
	handler := middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 for default scope, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify via middleware: ScopeProjectSecretRead -> 403
	middleware2 := RequireFederationAccess(ScopeProjectSecretRead)
	handler2 := middleware2(okHandler())

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	req2 = req2.WithContext(contextWithIdentity(req2.Context(), identity))

	rec2 := httptest.NewRecorder()
	handler2.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 for non-default scope, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// Verify error response contains the scope name
	var errResp ErrorResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	if !strings.Contains(errResp.Error.Message, string(ScopeProjectSecretRead)) {
		t.Errorf("expected scope name in error message, got: %s", errResp.Error.Message)
	}
}
