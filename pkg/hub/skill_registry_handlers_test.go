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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newRegistryTestServer creates a fully initialized test server with authzService
// so that route guards and Decide-based permission checks work.
func newRegistryTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	seedRoleDefinitions(context.Background(), s)
	return srv, s
}

func TestSkillRegistryCRUD(t *testing.T) {
	srv, _ := newRegistryTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	// Create
	body := `{"name":"my-reg","endpoint":"https://registry.example.com","description":"test registry"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var created store.SkillRegistry
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("create: invalid JSON: %v", err)
	}
	if created.Name != "my-reg" {
		t.Errorf("create: expected name my-reg, got %s", created.Name)
	}
	if created.Type != "hub" {
		t.Errorf("create: expected type hub, got %s", created.Type)
	}
	if created.TrustLevel != "pinned" {
		t.Errorf("create: expected trust pinned (default), got %s", created.TrustLevel)
	}
	if created.Status != "active" {
		t.Errorf("create: expected status active, got %s", created.Status)
	}

	// List
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var listResp store.ListResult[store.SkillRegistry]
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list: invalid JSON: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("list: expected 1 item, got %d", len(listResp.Items))
	}

	// Get by ID
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries/"+created.ID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Get by name
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries/my-reg", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("get by name: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Update
	updateBody := `{"status":"disabled","trustLevel":"trusted"}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/skill-registries/"+created.ID, strings.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var updated store.SkillRegistry
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("update: invalid JSON: %v", err)
	}
	if updated.Status != "disabled" {
		t.Errorf("update: expected status disabled, got %s", updated.Status)
	}
	if updated.TrustLevel != "trusted" {
		t.Errorf("update: expected trust trusted, got %s", updated.TrustLevel)
	}

	// Delete
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/skill-registries/"+created.ID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify deleted
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list after delete: invalid JSON: %v", err)
	}
	if len(listResp.Items) != 0 {
		t.Errorf("list after delete: expected 0 items, got %d", len(listResp.Items))
	}
}

func TestSkillRegistryCRUD_DuplicateName(t *testing.T) {
	srv, _ := newRegistryTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	body := `{"name":"dup-reg","endpoint":"https://registry.example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d", rr.Code)
	}

	// Attempt duplicate
	req = httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code == http.StatusCreated {
		t.Fatal("expected duplicate name to be rejected")
	}
}

func TestSkillRegistryCRUD_InvalidEndpoint(t *testing.T) {
	srv, _ := newRegistryTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	body := `{"name":"bad-endpoint","endpoint":"http://insecure.example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for HTTP endpoint, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestSkillRegistryRouteGuard_PermissionBased verifies that the skill registry
// route guard enforces skill.register permission. Hub-admins with skill.register
// in their role should be allowed; regular members should be denied.
func TestSkillRegistryRouteGuard_PermissionBased(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	// Create a hub-admin user
	hubAdminUser := &store.User{
		ID: tid("ha-sr"), Email: "ha-sr@test.com", DisplayName: "Hub Admin",
		Role: "member", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("mem-sr"), Email: "mem-sr@test.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, hubAdminUser); err != nil {
		t.Fatalf("create hub-admin: %v", err)
	}
	if err := s.CreateUser(ctx, memberUser); err != nil {
		t.Fatalf("create member: %v", err)
	}

	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("get hub-admin role definition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      hubAdminUser.ID,
		ScopeType:        store.RoleScopeSystem,
	}); err != nil {
		t.Fatalf("create hub-admin binding: %v", err)
	}

	okHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}

	meta := routeMetadataTable["/api/v1/skill-registries"]
	handler := srv.routeGuard(meta, okHandler)

	// Hub-admin should be allowed (skill.register is in hub-admin role)
	hubAdmin := NewAuthenticatedUser(tid("ha-sr"), "ha-sr@test.com", "Hub Admin", "member", "api")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries", nil)
	req = req.WithContext(contextWithIdentity(ctx, hubAdmin))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("hub-admin with skill.register: expected 200, got %d", rr.Code)
	}

	// Regular member should be denied
	member := NewAuthenticatedUser(tid("mem-sr"), "mem-sr@test.com", "Member", "member", "api")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries", nil)
	req = req.WithContext(contextWithIdentity(ctx, member))
	rr = httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("regular member: expected 403, got %d", rr.Code)
	}
}

func TestSkillRegistryCRUD_AuthTokenNotInResponse(t *testing.T) {
	srv, _ := newRegistryTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	body := `{"name":"secret-reg","endpoint":"https://registry.example.com","authToken":"super-secret"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	if strings.Contains(rr.Body.String(), "super-secret") {
		t.Error("auth token should not appear in create response")
	}
	if strings.Contains(rr.Body.String(), "authToken") {
		t.Error("authToken field should not appear in response (json:\"-\")")
	}

	// Also check GET
	var created store.SkillRegistry
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	req = httptest.NewRequest(http.MethodGet, "/api/v1/skill-registries/"+created.ID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if strings.Contains(rr.Body.String(), "super-secret") {
		t.Error("auth token should not appear in GET response")
	}
}

func TestSkillRegistryCRUD_ClearAuthTokenAndResolvePath(t *testing.T) {
	srv, st := newRegistryTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	// Create registry with non-empty AuthToken and ResolvePath
	body := `{"name":"clear-reg","endpoint":"https://registry.example.com","authToken":"my-token","resolvePath":"/custom/resolve"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var created store.SkillRegistry
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("create: invalid JSON: %v", err)
	}

	// Verify initial values via store (AuthToken is json:"-")
	got, err := st.GetSkillRegistry(req.Context(), created.ID)
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if got.AuthToken != "my-token" {
		t.Fatalf("expected authToken 'my-token', got %q", got.AuthToken)
	}
	if got.ResolvePath != "/custom/resolve" {
		t.Fatalf("expected resolvePath '/custom/resolve', got %q", got.ResolvePath)
	}

	// Update with explicit empty strings to clear both fields
	updateBody := `{"authToken":"","resolvePath":""}`
	req = httptest.NewRequest(http.MethodPut, "/api/v1/skill-registries/"+created.ID, strings.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify resolvePath is cleared in response (omitempty means absent when empty)
	var updatedResp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &updatedResp); err != nil {
		t.Fatalf("update response: invalid JSON: %v", err)
	}
	if _, ok := updatedResp["resolvePath"]; ok {
		t.Errorf("expected resolvePath absent from response (omitempty), got %v", updatedResp["resolvePath"])
	}

	// Verify fields are cleared via store (authoritative check)
	got, err = st.GetSkillRegistry(req.Context(), created.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.AuthToken != "" {
		t.Errorf("expected authToken cleared, got %q", got.AuthToken)
	}
	if got.ResolvePath != "" {
		t.Errorf("expected resolvePath cleared, got %q", got.ResolvePath)
	}
}

func TestSkillRegistryPin(t *testing.T) {
	srv, _ := newRegistryTestServer(t)
	admin := NewAuthenticatedUser("admin-1", "admin@test.com", "Admin", "admin", "cli")

	// Create registry
	body := `{"name":"pin-reg","endpoint":"https://registry.example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleSkillRegistries(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.Code)
	}

	var created store.SkillRegistry
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	// Pin
	pinBody := `{"uri":"skill://pin-reg/core/test@1.0","hash":"sha256:abc123"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/skill-registries/"+created.ID+"/pin", strings.NewReader(pinBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr = httptest.NewRecorder()
	srv.handleSkillRegistryByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("pin: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
