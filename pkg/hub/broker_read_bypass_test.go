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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestBrokerCanGetTemplateByID verifies that an authenticated broker identity
// can read a template by ID. Brokers authenticate via HMAC middleware and are
// not user principals — the authorization kernel hard-denies them. The broker
// read bypass allows template hydration during agent creation.
func TestBrokerCanGetTemplateByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	tmpl := &store.Template{
		ID:         tid("tmpl_broker1"),
		Slug:       "broker-test-tmpl",
		Name:       "Broker Test Template",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Status:     store.TemplateStatusActive,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	brokerIdent := NewBrokerIdentity("test-broker-tmpl")

	// Build request with broker identity in context, mirroring what
	// BrokerAuthMiddleware does for a real HMAC-authenticated request.
	// The middleware sets both brokerIdentityContextKey and identityContextKey.
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/templates/%s", tid("tmpl_broker1")), nil)
	reqCtx := contextWithBrokerIdentity(req.Context(), brokerIdent)
	reqCtx = contextWithIdentity(reqCtx, brokerIdent)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 for broker GET template, got %d: %s", rec.Code, rec.Body.String())
	}

	var result TemplateWithCapabilities
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result.Template.Name != "Broker Test Template" {
		t.Errorf("expected template name %q, got %q", "Broker Test Template", result.Template.Name)
	}
}

// TestBrokerCanGetHarnessConfigByID verifies that an authenticated broker
// identity can read a harness config by ID. Same rationale as template reads.
func TestBrokerCanGetHarnessConfigByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:         tid("hc_broker1"),
		Slug:       "broker-test-hc",
		Name:       "Broker Test HC",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Status:     store.HarnessConfigStatusActive,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	brokerIdent := NewBrokerIdentity("test-broker-hc")

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/harness-configs/%s", tid("hc_broker1")), nil)
	reqCtx := contextWithBrokerIdentity(req.Context(), brokerIdent)
	reqCtx = contextWithIdentity(reqCtx, brokerIdent)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 for broker GET harness config, got %d: %s", rec.Code, rec.Body.String())
	}

	var result HarnessConfigWithCapabilities
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result.HarnessConfig.Name != "Broker Test HC" {
		t.Errorf("expected harness config name %q, got %q", "Broker Test HC", result.HarnessConfig.Name)
	}
}

// TestNonBrokerWithoutPermissionDeniedTemplate verifies that a non-broker
// identity without read permission is still denied access to templates.
// This ensures the SECURITY-GATE is preserved for non-broker callers.
func TestNonBrokerWithoutPermissionDeniedTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	tmpl := &store.Template{
		ID:         tid("tmpl_deny1"),
		Slug:       "deny-test-tmpl",
		Name:       "Deny Test Template",
		Scope:      "project",
		ScopeID:    tid("project_deny"),
		Visibility: store.VisibilityPrivate,
		Status:     store.TemplateStatusActive,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	// Create a non-admin user identity with no role bindings — the authz
	// kernel should deny read access.
	noPermsUser := NewAuthenticatedUser(tid("user_noperms"), "noperms@test", "No Perms", "member", "api")
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/templates/%s", tid("tmpl_deny1")), nil)
	reqCtx := contextWithIdentity(req.Context(), noPermsUser)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403 for non-admin user without read permission, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNonBrokerWithoutPermissionDeniedHarnessConfig verifies the same deny
// behavior for harness config endpoints.
func TestNonBrokerWithoutPermissionDeniedHarnessConfig(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:         tid("hc_deny1"),
		Slug:       "deny-test-hc",
		Name:       "Deny Test HC",
		Harness:    "claude",
		Scope:      "project",
		ScopeID:    tid("project_deny"),
		Visibility: store.VisibilityPrivate,
		Status:     store.HarnessConfigStatusActive,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateHarnessConfig(ctx, hc); err != nil {
		t.Fatalf("failed to create harness config: %v", err)
	}

	noPermsUser := NewAuthenticatedUser(tid("user_noperms"), "noperms@test", "No Perms", "member", "api")
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/harness-configs/%s", tid("hc_deny1")), nil)
	reqCtx := contextWithIdentity(req.Context(), noPermsUser)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403 for non-admin user without read permission, got %d: %s", rec.Code, rec.Body.String())
	}
}
