//go:build !no_sqlite

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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newFederationTestServer(t *testing.T, mock *httptest.Server) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fedClient := mock.Client()
	fedClient.Timeout = federationTimeout
	fedClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	srv := &Server{store: s, federationClient: fedClient}
	return srv, s
}

func newFederationMockRegistry(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestFederateResolve_TrustedHappyPath(t *testing.T) {
	mockResp := ResolveSkillsResponse{
		Resolved: []ResolvedSkillResponse{{
			URI:             "skill://ext-registry/core/test-skill@1.0",
			Name:            "test-skill",
			ResolvedVersion: "1.0.0",
			ContentHash:     "sha256:abc123",
		}},
	}

	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills/resolve" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:        "ext-registry",
		Endpoint:    mock.URL,
		Type:        "hub",
		TrustLevel:  "trusted",
		ResolvePath: "/api/v1/skills/resolve",
		Status:      "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	resolved, resolveErr := srv.federateResolve(t.Context(), "ext-registry", ResolveSkillRef{URI: "skill://ext-registry/core/test-skill@1.0"})
	if resolveErr != nil {
		t.Fatalf("unexpected error: %s", resolveErr.Message)
	}
	if resolved.Name != "test-skill" {
		t.Errorf("expected name test-skill, got %s", resolved.Name)
	}
	if resolved.ContentHash != "sha256:abc123" {
		t.Errorf("expected hash sha256:abc123, got %s", resolved.ContentHash)
	}
}

func TestFederateResolve_PinnedHappyPath(t *testing.T) {
	mockResp := ResolveSkillsResponse{
		Resolved: []ResolvedSkillResponse{{
			URI:             "skill://ext-registry/core/pinned-skill@1.0",
			Name:            "pinned-skill",
			ResolvedVersion: "1.0.0",
			ContentHash:     "sha256:matchme",
		}},
	}

	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:        "ext-registry",
		Endpoint:    mock.URL,
		Type:        "hub",
		TrustLevel:  "pinned",
		ResolvePath: "/api/v1/skills/resolve",
		Status:      "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	if err := s.PinSkillHash(t.Context(), registry.ID, "skill://ext-registry/core/pinned-skill@1.0", "sha256:matchme"); err != nil {
		t.Fatalf("failed to pin hash: %v", err)
	}

	resolved, resolveErr := srv.federateResolve(t.Context(), "ext-registry", ResolveSkillRef{URI: "skill://ext-registry/core/pinned-skill@1.0"})
	if resolveErr != nil {
		t.Fatalf("unexpected error: %s", resolveErr.Message)
	}
	if resolved.ContentHash != "sha256:matchme" {
		t.Errorf("expected hash sha256:matchme, got %s", resolved.ContentHash)
	}
}

func TestFederateResolve_PinnedHashMismatch(t *testing.T) {
	mockResp := ResolveSkillsResponse{
		Resolved: []ResolvedSkillResponse{{
			URI:             "skill://ext-registry/core/bad-skill@1.0",
			Name:            "bad-skill",
			ResolvedVersion: "1.0.0",
			ContentHash:     "sha256:different",
		}},
	}

	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:        "ext-registry",
		Endpoint:    mock.URL,
		Type:        "hub",
		TrustLevel:  "pinned",
		ResolvePath: "/api/v1/skills/resolve",
		Status:      "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}
	if err := s.PinSkillHash(t.Context(), registry.ID, "skill://ext-registry/core/bad-skill@1.0", "sha256:expected"); err != nil {
		t.Fatalf("failed to pin hash: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "ext-registry", ResolveSkillRef{URI: "skill://ext-registry/core/bad-skill@1.0"})
	if resolveErr == nil {
		t.Fatal("expected trust_violation error")
	}
	if resolveErr.Code != "trust_violation" {
		t.Errorf("expected code trust_violation, got %s", resolveErr.Code)
	}
	if !strings.Contains(resolveErr.Message, "content hash mismatch") {
		t.Errorf("expected mismatch message, got: %s", resolveErr.Message)
	}
}

func TestFederateResolve_NoPinConfigured(t *testing.T) {
	mockResp := ResolveSkillsResponse{
		Resolved: []ResolvedSkillResponse{{
			URI:             "skill://ext-registry/core/unpinned@1.0",
			Name:            "unpinned",
			ResolvedVersion: "1.0.0",
			ContentHash:     "sha256:any",
		}},
	}

	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:        "ext-registry",
		Endpoint:    mock.URL,
		Type:        "hub",
		TrustLevel:  "pinned",
		ResolvePath: "/api/v1/skills/resolve",
		Status:      "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "ext-registry", ResolveSkillRef{URI: "skill://ext-registry/core/unpinned@1.0"})
	if resolveErr == nil {
		t.Fatal("expected trust_violation error for missing pin")
	}
	if resolveErr.Code != "trust_violation" {
		t.Errorf("expected code trust_violation, got %s", resolveErr.Code)
	}
	if !strings.Contains(resolveErr.Message, "no pinned hash") {
		t.Errorf("expected 'no pinned hash' message, got: %s", resolveErr.Message)
	}
}

func TestFederateResolve_UnknownRegistry(t *testing.T) {
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {})
	srv, _ := newFederationTestServer(t, mock)

	_, resolveErr := srv.federateResolve(t.Context(), "nonexistent", ResolveSkillRef{URI: "skill://nonexistent/core/test@1.0"})
	if resolveErr == nil {
		t.Fatal("expected unknown_registry error")
	}
	if resolveErr.Code != "unknown_registry" {
		t.Errorf("expected code unknown_registry, got %s", resolveErr.Code)
	}
}

func TestFederateResolve_DisabledRegistry(t *testing.T) {
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {})
	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:       "disabled-reg",
		Endpoint:   "https://example.com",
		Type:       "hub",
		TrustLevel: "trusted",
		Status:     "disabled",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "disabled-reg", ResolveSkillRef{URI: "skill://disabled-reg/core/test@1.0"})
	if resolveErr == nil {
		t.Fatal("expected registry_disabled error")
	}
	if resolveErr.Code != "registry_disabled" {
		t.Errorf("expected code registry_disabled, got %s", resolveErr.Code)
	}
}

func TestFederateResolve_WrongRegistryType(t *testing.T) {
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {})
	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:       "gcp-reg",
		Endpoint:   "https://example.com",
		Type:       "gcp",
		TrustLevel: "trusted",
		Status:     "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "gcp-reg", ResolveSkillRef{URI: "skill://gcp-reg/core/test@1.0"})
	if resolveErr == nil {
		t.Fatal("expected wrong_registry_type error")
	}
	if resolveErr.Code != "wrong_registry_type" {
		t.Errorf("expected code wrong_registry_type, got %s", resolveErr.Code)
	}
}

func TestFederateResolve_ExternalRegistryDown(t *testing.T) {
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:       "down-reg",
		Endpoint:   mock.URL,
		Type:       "hub",
		TrustLevel: "trusted",
		Status:     "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "down-reg", ResolveSkillRef{URI: "skill://down-reg/core/test@1.0"})
	if resolveErr == nil {
		t.Fatal("expected federation_error")
	}
	if resolveErr.Code != "federation_error" {
		t.Errorf("expected code federation_error, got %s", resolveErr.Code)
	}
}

func TestFederateResolve_AuthTokenSent(t *testing.T) {
	var receivedAuth string
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		resp := ResolveSkillsResponse{
			Resolved: []ResolvedSkillResponse{{
				URI:             "skill://auth-reg/core/test@1.0",
				Name:            "test",
				ResolvedVersion: "1.0.0",
				ContentHash:     "sha256:abc",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:       "auth-reg",
		Endpoint:   mock.URL,
		Type:       "hub",
		TrustLevel: "trusted",
		AuthToken:  "secret-token-123",
		Status:     "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "auth-reg", ResolveSkillRef{URI: "skill://auth-reg/core/test@1.0"})
	if resolveErr != nil {
		t.Fatalf("unexpected error: %s", resolveErr.Message)
	}
	if receivedAuth != "Bearer secret-token-123" {
		t.Errorf("expected 'Bearer secret-token-123', got %q", receivedAuth)
	}
}

func TestFederateResolve_CustomResolvePath(t *testing.T) {
	var receivedPath string
	mock := newFederationMockRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		resp := ResolveSkillsResponse{
			Resolved: []ResolvedSkillResponse{{
				URI:             "skill://custom-reg/core/test@1.0",
				Name:            "test",
				ResolvedVersion: "1.0.0",
				ContentHash:     "sha256:abc",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv, s := newFederationTestServer(t, mock)

	registry := &store.SkillRegistry{
		Name:        "custom-reg",
		Endpoint:    mock.URL,
		Type:        "hub",
		TrustLevel:  "trusted",
		ResolvePath: "/custom/resolve",
		Status:      "active",
	}
	if err := s.CreateSkillRegistry(t.Context(), registry); err != nil {
		t.Fatalf("failed to create registry: %v", err)
	}

	_, resolveErr := srv.federateResolve(t.Context(), "custom-reg", ResolveSkillRef{URI: "skill://custom-reg/core/test@1.0"})
	if resolveErr != nil {
		t.Fatalf("unexpected error: %s", resolveErr.Message)
	}
	if receivedPath != "/custom/resolve" {
		t.Errorf("expected /custom/resolve, got %s", receivedPath)
	}
}
