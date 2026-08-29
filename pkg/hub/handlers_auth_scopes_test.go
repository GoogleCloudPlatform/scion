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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// TestHandleAuthScopes_Authenticated verifies the scopes endpoint returns all
// valid UAT scopes for an authenticated user.
func TestHandleAuthScopes_Authenticated(t *testing.T) {
	srv, _ := testServer(t)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/auth/scopes", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AuthScopesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// Verify we have scopes
	if len(resp.Scopes) == 0 {
		t.Fatal("expected non-empty scopes list")
	}

	// Verify every scope follows resource:action format
	for _, scope := range resp.Scopes {
		if !strings.Contains(scope.ID, ":") {
			t.Errorf("scope %q does not follow resource:action format", scope.ID)
		}
		if scope.Resource == "" {
			t.Errorf("scope %q has empty resource", scope.ID)
		}
		if scope.Action == "" {
			t.Errorf("scope %q has empty action", scope.ID)
		}
		if scope.Description == "" {
			t.Errorf("scope %q has empty description", scope.ID)
		}
		// Verify the ID is resource:action
		expected := scope.Resource + ":" + scope.Action
		if scope.ID != expected {
			t.Errorf("scope ID %q does not match resource:action %q", scope.ID, expected)
		}
	}

	// Verify aliases include agent:manage
	if len(resp.Aliases) == 0 {
		t.Fatal("expected at least one alias (agent:manage)")
	}
	found := false
	for _, alias := range resp.Aliases {
		if alias.ID == "agent:manage" {
			found = true
			if len(alias.ExpandsTo) == 0 {
				t.Error("agent:manage alias has empty expands_to")
			}
			// Verify it expands to the correct agent scopes
			manageScopes := permissions.UATManageScopes()
			sort.Strings(alias.ExpandsTo)
			if strings.Join(alias.ExpandsTo, ",") != strings.Join(manageScopes, ",") {
				t.Errorf("agent:manage expands_to mismatch\ngot:  %v\nwant: %v", alias.ExpandsTo, manageScopes)
			}
		}
	}
	if !found {
		t.Error("agent:manage alias not found in response")
	}
}

// TestHandleAuthScopes_Unauthenticated verifies the scopes endpoint rejects
// unauthenticated requests.
func TestHandleAuthScopes_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/scopes", nil)
	// No Authorization header
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleAuthScopes_MethodNotAllowed verifies only GET is accepted.
func TestHandleAuthScopes_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := doRequest(t, srv, method, "/api/v1/auth/scopes", nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, rr.Code)
		}
	}
}

// TestHandleAuthScopes_NonAdmin verifies the endpoint is accessible to non-admin users.
func TestHandleAuthScopes_NonAdmin(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// The dev user is a member, not an admin. Use it directly.
	member := NewAuthenticatedUser("member-scopes", "member@test.com", "Member", "member", "api")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/scopes", nil)
	req = req.WithContext(contextWithIdentity(ctx, member))

	rr := httptest.NewRecorder()
	handler := srv.guarded("/api/v1/auth/scopes", srv.handleAuthScopes)
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-admin user, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleAuthScopes_ContainsNewScopes verifies the new resource type scopes
// are present in the response.
func TestHandleAuthScopes_ContainsNewScopes(t *testing.T) {
	srv, _ := testServer(t)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/auth/scopes", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AuthScopesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	scopeIDs := map[string]bool{}
	for _, s := range resp.Scopes {
		scopeIDs[s.ID] = true
	}

	// Verify new scopes are present
	expectedNewScopes := []string{
		"skill:read", "skill:create", "skill:list", "skill:update", "skill:delete", "skill:register",
		"template:read", "template:create", "template:list", "template:update", "template:delete",
		"harness_config:read", "harness_config:create", "harness_config:list", "harness_config:update", "harness_config:delete",
		"group:read", "group:create", "group:list", "group:update", "group:delete", "group:addMember", "group:removeMember",
		"user:read", "user:list",
		"broker:read", "broker:list",
		"gcp_service_account:read", "gcp_service_account:list", "gcp_service_account:verify", "gcp_service_account:assign",
	}

	for _, scope := range expectedNewScopes {
		if !scopeIDs[scope] {
			t.Errorf("expected scope %q not found in response", scope)
		}
	}

	// Verify existing agent/project scopes still present
	for _, scope := range []string{
		"agent:create", "agent:read", "agent:list", "agent:delete", "agent:attach", "agent:port_access",
		"project:read", "project:update", "project:clone",
	} {
		if !scopeIDs[scope] {
			t.Errorf("existing scope %q missing from response", scope)
		}
	}
}

// TestUATScopes_NoPolicyScopesExist verifies that no UAT scopes exist for the
// policy resource type (policy authoring stays super-admin-only).
func TestUATScopes_NoPolicyScopesExist(t *testing.T) {
	for _, perm := range permissions.Registry {
		if perm.Resource == permissions.ResourcePolicy && perm.UATScope != "" {
			t.Errorf("policy permission %q has UAT scope %q — policy must not have UAT scopes", perm.ID, perm.UATScope)
		}
	}
}

// TestUATScopes_NoAuthorityEscalationScopes verifies no UAT scopes for
// authority-escalation operations.
func TestUATScopes_NoAuthorityEscalationScopes(t *testing.T) {
	forbidden := map[string]bool{
		"user.suspend":            true,
		"user.promote":            true,
		"user.update":             true,
		"hub.maintenance.execute": true,
		"hub.admin_mode.update":   true,
		"hub.auth_reset.execute":  true,
	}
	for _, perm := range permissions.Registry {
		if forbidden[perm.ID] && perm.UATScope != "" {
			t.Errorf("authority-escalation permission %q has UAT scope %q — should not have UAT scope", perm.ID, perm.UATScope)
		}
	}
}

// TestUATScopes_ValidScopesIncludeNewResourceTypes verifies that ValidUATScopes()
// returns all expected scopes including the new resource types.
func TestUATScopes_ValidScopesIncludeNewResourceTypes(t *testing.T) {
	valid := permissions.UATValidScopes()

	newScopes := []string{
		"skill:read", "skill:create", "skill:list", "skill:update", "skill:delete", "skill:register",
		"template:read", "template:create", "template:list", "template:update", "template:delete",
		"harness_config:read", "harness_config:create", "harness_config:list", "harness_config:update", "harness_config:delete",
		"group:read", "group:create", "group:list", "group:update", "group:delete", "group:addMember", "group:removeMember",
		"user:read", "user:list",
		"broker:read", "broker:list",
		"gcp_service_account:read", "gcp_service_account:list", "gcp_service_account:verify", "gcp_service_account:assign",
	}
	for _, scope := range newScopes {
		if !valid[scope] {
			t.Errorf("new scope %q not found in ValidUATScopes()", scope)
		}
	}

	// Also check existing ones still present
	for _, scope := range []string{
		"agent:create", "agent:read", "agent:list", "agent:delete", "agent:attach", "agent:port_access",
		"project:read", "project:update", "project:clone",
		"agent:manage",
	} {
		if !valid[scope] {
			t.Errorf("existing scope %q missing from ValidUATScopes()", scope)
		}
	}
}

// TestUATScopes_FormatConsistency verifies all scopes follow resource:action format.
func TestUATScopes_FormatConsistency(t *testing.T) {
	for _, perm := range permissions.Registry {
		if perm.UATScope == "" {
			continue
		}
		expected := perm.Resource + ":" + perm.Action
		if perm.UATScope != expected {
			t.Errorf("permission %q has UATScope %q, expected %q", perm.ID, perm.UATScope, expected)
		}
	}
}

// TestUATScopes_AgentManageAliasStillExpands verifies the agent:manage alias
// still expands correctly to agent scopes only.
func TestUATScopes_AgentManageAliasStillExpands(t *testing.T) {
	manageScopes := permissions.UATManageScopes()
	if len(manageScopes) == 0 {
		t.Fatal("agent:manage alias expands to zero scopes")
	}

	// All expanded scopes must be agent:* scopes
	for _, scope := range manageScopes {
		if !strings.HasPrefix(scope, "agent:") {
			t.Errorf("agent:manage expanded to non-agent scope %q", scope)
		}
	}

	// Verify agent:manage is still valid
	valid := permissions.UATValidScopes()
	if !valid["agent:manage"] {
		t.Error("agent:manage not valid in UATValidScopes()")
	}
}

// TestHandleAuthScopes_AllManageAliasesPresent verifies that every manage alias
// from UATManageAliases appears in the GET /api/v1/auth/scopes response.
func TestHandleAuthScopes_AllManageAliasesPresent(t *testing.T) {
	srv, _ := testServer(t)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/auth/scopes", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp AuthScopesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	aliasMap := map[string]AuthScopeAlias{}
	for _, alias := range resp.Aliases {
		aliasMap[alias.ID] = alias
	}

	for aliasScope, resource := range permissions.UATManageAliases {
		alias, ok := aliasMap[aliasScope]
		if !ok {
			t.Errorf("manage alias %q not found in response", aliasScope)
			continue
		}
		if len(alias.ExpandsTo) == 0 {
			t.Errorf("manage alias %q has empty expands_to", aliasScope)
		}
		expectedScopes := permissions.UATManageScopesFor(resource)
		sort.Strings(alias.ExpandsTo)
		if strings.Join(alias.ExpandsTo, ",") != strings.Join(expectedScopes, ",") {
			t.Errorf("%s expands_to mismatch\ngot:  %v\nwant: %v", aliasScope, alias.ExpandsTo, expectedScopes)
		}
		// Verify all expanded scopes belong to the correct resource type.
		prefix := resource + ":"
		for _, s := range alias.ExpandsTo {
			if !strings.HasPrefix(s, prefix) {
				t.Errorf("%s expanded to non-%s scope %q", aliasScope, resource, s)
			}
		}
	}
}

// TestUATScopes_AllManageAliasesValid verifies all manage aliases are accepted
// by UATValidScopes.
func TestUATScopes_AllManageAliasesValid(t *testing.T) {
	valid := permissions.UATValidScopes()
	for alias := range permissions.UATManageAliases {
		if !valid[alias] {
			t.Errorf("manage alias %q not valid in UATValidScopes()", alias)
		}
	}
}
