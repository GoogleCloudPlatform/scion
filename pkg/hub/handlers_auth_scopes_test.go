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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// Every selector follows the format selectorForPermissionID derives
	// from its permission ID and ends in ":" plus the permission's action.
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
		if !strings.HasSuffix(scope.ID, ":"+scope.Action) {
			t.Errorf("scope ID %q does not end in the action %q", scope.ID, ":"+scope.Action)
		}
		if expected := selectorForPermissionID(scope.PermissionID); scope.ID != expected {
			t.Errorf("scope ID %q of permission %q does not match the derived selector %q", scope.ID, scope.PermissionID, expected)
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

// TestHandleAuthScopes_NonUserIdentity pins that the selector catalog and
// the per-project eligibility admit only the credentials the
// user.self.scopes catalog operation declares: an agent identity, whose
// credential is an agent JWT, is refused both.
func TestHandleAuthScopes_NonUserIdentity(t *testing.T) {
	srv, _ := testServer(t)
	handler := srv.guarded("/api/v1/auth/scopes", srv.handleAuthScopes)
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("authscopes-agent-identity")},
		ProjectID: tid("authscopes-agent-identity-project"),
	}}

	t.Run("catalog with no params refuses an agent identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/scopes", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agent))

		rr := httptest.NewRecorder()
		handler(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	})

	t.Run("eligibility with projectId refuses an agent identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/scopes?projectId="+tid("authscopes-agent-identity-project"), nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agent))

		rr := httptest.NewRecorder()
		handler(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	})
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
		"role:read", "role_binding:read", "access_constraint:read", "hub_audit:read",
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
//
// user.update and user.suspend have hub-only selectors, pinned by
// TestUATScopes_UserUpdateAndSuspendAreHubOnly; user.promote and
// user.delete by TestUATScopes_UserPromoteAndDeleteAreHubOnly.
func TestUATScopes_NoAuthorityEscalationScopes(t *testing.T) {
	forbidden := map[string]bool{
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

// TestUATScopes_UserUpdateAndSuspendAreHubOnly pins the selectors of
// user.update and user.suspend: user:update and user:suspend, each allowed
// only on a hub boundary and only on the hub-level user record.
func TestUATScopes_UserUpdateAndSuspendAreHubOnly(t *testing.T) {
	for id, selector := range map[string]string{"user.update": "user:update", "user.suspend": "user:suspend"} {
		var scope string
		for _, perm := range permissions.Registry {
			if perm.ID == id {
				scope = perm.UATScope
			}
		}
		assert.Equal(t, selector, scope, "%s selector", id)
		assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, permissions.PermissionAllowedBoundaries[id], "%s boundaries", id)
		assert.Equal(t, []permissions.TargetClassKind{permissions.TargetClassKindHubResource}, permissions.SupportedTargetClasses[id], "%s target classes", id)
	}
}

// TestUATScopes_UserPromoteAndDeleteAreHubOnly pins the selectors of
// user.promote and user.delete: user:promote and user:delete, each allowed
// only on a hub boundary and only on the hub-level user record.
func TestUATScopes_UserPromoteAndDeleteAreHubOnly(t *testing.T) {
	for id, selector := range map[string]string{"user.promote": "user:promote", "user.delete": "user:delete"} {
		var scope string
		for _, perm := range permissions.Registry {
			if perm.ID == id {
				scope = perm.UATScope
			}
		}
		assert.Equal(t, selector, scope, "%s selector", id)
		assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, permissions.PermissionAllowedBoundaries[id], "%s boundaries", id)
		assert.Equal(t, []permissions.TargetClassKind{permissions.TargetClassKindHubResource}, permissions.SupportedTargetClasses[id], "%s target classes", id)
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

// selectorForPermissionID derives the token selector of a permission from
// its ID: every dot-separated segment but the last joined with "_", then
// ":" and the last segment. agent.create gives agent:create and
// hub.config.read gives hub_config:read, so permissions that share a
// resource and action (hub.config.read and hub.settings.read) still get
// distinct selectors.
func selectorForPermissionID(id string) string {
	segments := strings.Split(id, ".")
	if len(segments) < 2 {
		return ""
	}
	last := len(segments) - 1
	return strings.Join(segments[:last], "_") + ":" + segments[last]
}

// TestUATScopes_FormatConsistency requires every selector to equal the one
// selectorForPermissionID derives from its permission ID and to end in ":"
// plus the permission's action.
func TestUATScopes_FormatConsistency(t *testing.T) {
	for _, perm := range permissions.Registry {
		if perm.UATScope == "" {
			continue
		}
		if !strings.HasSuffix(perm.UATScope, ":"+perm.Action) {
			t.Errorf("permission %q has UATScope %q, which does not end in %q", perm.ID, perm.UATScope, ":"+perm.Action)
		}
		if expected := selectorForPermissionID(perm.ID); perm.UATScope != expected {
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

// ---------------------------------------------------------------------------
// ptone/scion#2122: project-boundary eligibility surfaced through
// GET /api/v1/auth/scopes?projectId=.
// ---------------------------------------------------------------------------

// TestAuthScopes_ProjectEligibilityForMember verifies that an ordinary
// project member's eligibility is reported correctly before they own any
// agent: relationship-eligible selectors (agent:attach, agent:port_access)
// are eligible, and a flat selector their role does not hold (agent:delete)
// is ineligible with a stable reason -- never a target list.
func TestAuthScopes_ProjectEligibilityForMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-member-project")
	ownerID := tid("authscopes-member-owner")
	memberID := tid("authscopes-member-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)

	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp AuthScopesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byID := map[string]AuthScopeEntry{}
	for _, entry := range resp.Scopes {
		byID[entry.ID] = entry
	}

	for _, id := range []string{"agent:attach", "agent:port_access"} {
		entry, ok := byID[id]
		require.True(t, ok, "scope %s missing from response", id)
		require.NotNil(t, entry.Eligibility, "scope %s missing eligibility", id)
		assert.True(t, entry.Eligibility.Eligible,
			"member should be eligible to select %s before owning an agent: reason %q", id, entry.Eligibility.Reason)
		assert.Equal(t, string(permissions.MintEligibilityRelationship), entry.EligibilityKind)
		assert.ElementsMatch(t, []string{"owner", "ancestor"}, entry.Relationships)
		assert.False(t, entry.RequiresExistingTarget)
		assert.Empty(t, entry.Eligibility.Reason)
		assert.NotEmpty(t, entry.Eligibility.Note)
	}
	assert.Equal(t, "checked on each target: your own agents and their descendants",
		byID["agent:attach"].Eligibility.Note)
	assert.Contains(t, byID["agent:port_access"].Eligibility.Note,
		"plus agents in projects where your role grants agent.port_access",
		"port_access note should cover the reach of roles that grant it")

	deleteEntry, ok := byID["agent:delete"]
	require.True(t, ok)
	require.NotNil(t, deleteEntry.Eligibility)
	assert.False(t, deleteEntry.Eligibility.Eligible)
	assert.Equal(t, string(MintDenialFlatRoleInsufficient), deleteEntry.Eligibility.Reason)
	assert.Equal(t, string(permissions.MintEligibilityFlatRole), deleteEntry.EligibilityKind)

	// Eligibility answers only "may you select this restriction": it must
	// never enumerate a target (no agent has been created in this test).
	assert.NotContains(t, rec.Body.String(), "uatp-agent")
}

// TestAuthScopes_ProjectEligibilityRequiresProjectAccess verifies the
// all-denied case: an outsider with no membership and no exact-permission
// system authority for anything gets the same oracle-resistant 403 as
// token mint, byte-identical to a nonexistent project -- the endpoint must
// not become a project-existence oracle. See
// TestAuthScopes_ProjectEligibility_PartialSystemAuthorityAnswersPerEntry
// for the other half: once at least one selector is genuinely admitted,
// the response answers per-entry instead.
func TestAuthScopes_ProjectEligibilityRequiresProjectAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-noaccess-project")
	ownerID := tid("authscopes-noaccess-owner")
	outsiderID := tid("authscopes-noaccess-outsider")
	createRS1Project(t, s, projectID, ownerID)
	outsider := &store.User{
		ID: outsiderID, Email: outsiderID + "@test.com", DisplayName: "Outsider", Role: "member", Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, s, outsiderID)

	recNonMember := doRequestAsUser(t, srv, outsider, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectID, nil)

	nonexistentProjectID := tid("authscopes-nonexistent-project")
	recNonexistent := doRequestAsUser(t, srv, outsider, http.MethodGet, "/api/v1/auth/scopes?projectId="+nonexistentProjectID, nil)

	require.Equal(t, http.StatusForbidden, recNonMember.Code, recNonMember.Body.String())
	assert.Equal(t, recNonMember.Code, recNonexistent.Code,
		"non-member and nonexistent-project eligibility must return the same HTTP status")
	assert.JSONEq(t, recNonMember.Body.String(), recNonexistent.Body.String(),
		"non-member and nonexistent-project eligibility must return the byte-identical error body")

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(recNonMember.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	assert.Equal(t, "forbidden", resp.Error.Message)
	assert.Nil(t, resp.Error.Details)
}

// TestAuthScopes_ProjectEligibilityCanMintSelectorErrorFailsClosed verifies
// that a CanMintSelector error (as opposed to a per-selector denial) on the
// scopes-listing path gives the same whole-response, oracle-resistant 403 as
// mint's own CanMintSelector-error handling -- never a 200 with the full
// catalog, and never a per-entry reason. A project admin is used so the 403
// cannot come from the all-denied aggregate path exercised by
// TestAuthScopes_ProjectEligibilityRequiresProjectAccess above: without the
// injected error this user would get a 200.
func TestAuthScopes_ProjectEligibilityCanMintSelectorErrorFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-cle-project")
	ownerID := tid("authscopes-cle-owner")
	adminID := tid("authscopes-cle-admin")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	ensureHubMembership(ctx, s, adminID)
	u, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)
	restore := installFailStore(srv, &r2FailingStore{failListConstraints: fmt.Errorf("injected: constraint load failure")})
	defer restore()
	rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectID, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":{"code":"forbidden","message":"forbidden"}}`, rec.Body.String())
}

// TestAuthScopes_ProjectEligibility_PartialSystemAuthorityAnswersPerEntry
// verifies the other case: a non-member holding EXACT system authority for
// exactly one permission (agent.attach, via a dedicated system-scope role
// binding, not membership and not a broader role) is genuinely admitted
// for that one selector -- 200, eligible=true
// -- while every other project-applicable selector they lack authority for
// reports eligible=false, reason="project_access_required" verbatim. The
// whole response must NOT collapse to the oracle-resistant 403 here: since
// at least one selector was admitted, the caller already knows the project
// exists, so showing the rest of their own authority is not a new oracle.
func TestAuthScopes_ProjectEligibility_PartialSystemAuthorityAnswersPerEntry(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-partial-project")
	ownerID := tid("authscopes-partial-owner")
	userID := tid("authscopes-partial-user")
	createRS1Project(t, s, projectID, ownerID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, userID)
	grantPermissionViaRoleBinding(t, s, userID, "agent.attach", store.RoleScopeSystem, "")

	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp AuthScopesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byID := map[string]AuthScopeEntry{}
	for _, entry := range resp.Scopes {
		byID[entry.ID] = entry
	}

	attach, ok := byID["agent:attach"]
	require.True(t, ok, "agent:attach missing from response")
	require.NotNil(t, attach.Eligibility)
	assert.True(t, attach.Eligibility.Eligible,
		"exact system authority for agent.attach should admit and relationship-mint-eligible it: %+v", attach.Eligibility)
	assert.Empty(t, attach.Eligibility.Reason)

	// None of these permissions were granted, and this user has no project
	// membership: every one must be denied specifically for project
	// access, not any other reason (flat_role_insufficient would wrongly
	// suggest admission succeeded).
	for _, id := range []string{"agent:read", "agent:delete", "agent:port_access", "project:read"} {
		entry, ok := byID[id]
		require.True(t, ok, "scope %s missing from response", id)
		require.NotNil(t, entry.Eligibility, "scope %s missing eligibility", id)
		assert.False(t, entry.Eligibility.Eligible, "scope %s should be denied for project access", id)
		assert.Equal(t, string(MintDenialProjectAccessRequired), entry.Eligibility.Reason, "scope %s", id)
	}
}

// TestAuthScopes_ListingAgreesWithMint pins the consistency requirement
// between the listing and mint end to end through both real HTTP
// endpoints, for the same principal and selectors: a selector the listing
// reports eligible for a project also mints, and one it reports
// ineligible for does not.
func TestAuthScopes_ListingAgreesWithMint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-agree-project")
	ownerID := tid("authscopes-agree-owner")
	memberID := tid("authscopes-agree-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)

	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp AuthScopesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byID := map[string]AuthScopeEntry{}
	for _, entry := range resp.Scopes {
		byID[entry.ID] = entry
	}

	eligible, ok := byID["agent:attach"]
	require.True(t, ok)
	require.NotNil(t, eligible.Eligibility)
	require.True(t, eligible.Eligibility.Eligible, "precondition: listing must say agent:attach is eligible")

	ineligible, ok := byID["agent:delete"]
	require.True(t, ok)
	require.NotNil(t, ineligible.Eligibility)
	require.False(t, ineligible.Eligibility.Eligible, "precondition: listing must say agent:delete is ineligible")

	mintEligible := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "agree-eligible", "projectId": projectID, "scopes": []string{"agent:attach"},
	})
	assert.Equal(t, http.StatusCreated, mintEligible.Code,
		"listing said agent:attach eligible, mint should succeed: %s", mintEligible.Body.String())

	mintIneligible := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "agree-ineligible", "projectId": projectID, "scopes": []string{"agent:delete"},
	})
	assert.Equal(t, http.StatusForbidden, mintIneligible.Code,
		"listing said agent:delete ineligible, mint should fail: %s", mintIneligible.Body.String())
}

// TestAuthScopes_AliasEligibleOnlyWhenAllMembersEligible verifies the
// agent:manage alias reports eligible only when every expanded member
// selector is individually eligible, and names the ineligible members
// otherwise instead of collapsing to a single opaque flag.
func TestAuthScopes_AliasEligibleOnlyWhenAllMembersEligible(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-alias-project")
	ownerID := tid("authscopes-alias-owner")
	memberID := tid("authscopes-alias-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)

	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp AuthScopesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var manage *AuthScopeAlias
	for i := range resp.Aliases {
		if resp.Aliases[i].ID == "agent:manage" {
			manage = &resp.Aliases[i]
		}
	}
	require.NotNil(t, manage, "agent:manage alias missing from response")
	require.NotNil(t, manage.Eligibility)
	assert.False(t, manage.Eligibility.Eligible,
		"the curated member role lacks agent.delete/lifecycle/message, so agent:manage must be ineligible")
	assert.Contains(t, manage.Eligibility.IneligibleMembers, "agent:delete")
	assert.Empty(t, manage.Eligibility.Reason, "an alias-level denial has no single reason code, only ineligibleMembers")
}

// TestAuthScopes_BoundaryParamValidation pins the ?boundary=/?projectId=
// forms: boundary=project needs a projectId, boundary=hub refuses one, and
// an unknown boundary value is refused, each with 400; boundary=hub alone
// answers hub eligibility; neither parameter gives the catalog alone.
func TestAuthScopes_BoundaryParamValidation(t *testing.T) {
	srv, s := testServer(t)

	cases := []struct {
		name string
		path string
	}{
		{"boundary=project without projectId", "/api/v1/auth/scopes?boundary=project"},
		{"boundary=hub with projectId", "/api/v1/auth/scopes?boundary=hub&projectId=" + tid("authscopes-boundary-hub-proj")},
		{"unknown boundary value", "/api/v1/auth/scopes?boundary=nonsense"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodGet, c.path, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}

	t.Run("boundary=hub alone answers hub eligibility", func(t *testing.T) {
		userID := hubConfigTokenUser(t, s, "authscopes-boundary-hub-user", store.SystemRoleHubMember)
		user, err := s.GetUser(context.Background(), userID)
		require.NoError(t, err)
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/auth/scopes?boundary=hub", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AuthScopesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotEmpty(t, resp.Scopes)
		for _, entry := range resp.Scopes {
			require.NotNil(t, entry.Eligibility, "scope %s missing eligibility", entry.ID)
			assert.Equal(t, TokenBoundaryDTO{Kind: "hub"}, entry.Eligibility.Boundary, "scope %s", entry.ID)
		}
	})

	t.Run("neither boundary nor projectId keeps the unchanged catalog", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/scopes", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp AuthScopesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for _, entry := range resp.Scopes {
			assert.Nil(t, entry.Eligibility, "catalog-only response must not include eligibility")
		}
		for _, alias := range resp.Aliases {
			assert.Nil(t, alias.Eligibility, "catalog-only response must not include eligibility")
		}
	})

	t.Run("boundary=project with projectId succeeds and pins the eligibility boundary shape", func(t *testing.T) {
		ctx := context.Background()
		projectID := tid("authscopes-boundary-positive-project")
		ownerID := tid("authscopes-boundary-positive-owner")
		memberID := tid("authscopes-boundary-positive-member")
		createRS1Project(t, s, projectID, ownerID)
		uatpMember(t, s, projectID, memberID)

		member, err := s.GetUser(ctx, memberID)
		require.NoError(t, err)

		rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/auth/scopes?boundary=project&projectId="+projectID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp AuthScopesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		var attach *AuthScopeEntry
		for i := range resp.Scopes {
			if resp.Scopes[i].ID == "agent:attach" {
				attach = &resp.Scopes[i]
			}
		}
		require.NotNil(t, attach, "agent:attach missing from response")
		require.NotNil(t, attach.Eligibility)
		assert.Equal(t, TokenBoundaryDTO{Kind: "project", ProjectID: projectID}, attach.Eligibility.Boundary)
	})
}

// TestTokenCreate_ScopeViolationNamesSelector verifies the 403
// scope_violation body from POST /api/v1/auth/tokens carries structured
// details naming the denied selector and reason, once project admission has
// already succeeded (never on the uniform, detail-free
// ErrUATProjectForbidden path -- see TestProjectUAT_MintForbiddenIsOracleResistant).
func TestTokenCreate_ScopeViolationNamesSelector(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-violation-project")
	ownerID := tid("authscopes-violation-owner")
	memberID := tid("authscopes-violation-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)

	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	body := map[string]any{"name": "violation-test", "projectId": projectID, "scopes": []string{"agent:delete"}}
	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/auth/tokens", body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "scope_violation", resp.Error.Code)
	require.NotNil(t, resp.Error.Details)
	assert.Equal(t, "agent:delete", resp.Error.Details["selector"])
	assert.Equal(t, string(MintDenialFlatRoleInsufficient), resp.Error.Details["reason"])
}

// ---------------------------------------------------------------------------
// GET /api/v1/auth/scopes?boundary=hub and hub-boundary mint denials.
// ---------------------------------------------------------------------------

// authScopesHubByID requests hub-boundary eligibility as user over a
// session and returns the entries by selector.
func authScopesHubByID(t *testing.T, srv *Server, user *store.User) map[string]AuthScopeEntry {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/auth/scopes?boundary=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp AuthScopesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byID := make(map[string]AuthScopeEntry, len(resp.Scopes))
	for _, entry := range resp.Scopes {
		byID[entry.ID] = entry
	}
	return byID
}

// assertHubEligibilityShape checks every hub-boundary entry: it carries a
// hub eligibility answer, a selector not allowed on a hub boundary is
// refused as boundary_not_allowed (or unknown_selector), and no entry
// reports project_access_required.
func assertHubEligibilityShape(t *testing.T, byID map[string]AuthScopeEntry) {
	t.Helper()
	require.NotEmpty(t, byID)
	for id, entry := range byID {
		require.NotNil(t, entry.Eligibility, "scope %s missing eligibility", id)
		assert.Equal(t, TokenBoundaryDTO{Kind: "hub"}, entry.Eligibility.Boundary, "scope %s", id)
		assert.NotEqual(t, string(MintDenialProjectAccessRequired), entry.Eligibility.Reason,
			"scope %s: a hub boundary names no project", id)
		hubAllowed := false
		for _, b := range entry.AllowedBoundaries {
			if b == string(BoundaryKindHub) {
				hubAllowed = true
			}
		}
		if !hubAllowed {
			assert.False(t, entry.Eligibility.Eligible, "scope %s is not allowed on a hub boundary", id)
			assert.Contains(t, []string{string(MintDenialBoundaryNotAllowed), string(MintDenialUnknownSelector)},
				entry.Eligibility.Reason, "scope %s", id)
		}
		if entry.Eligibility.Eligible {
			assert.Empty(t, entry.Eligibility.Reason, "scope %s", id)
		} else {
			assert.NotEmpty(t, entry.Eligibility.Reason, "scope %s", id)
		}
	}
}

// TestAuthScopes_HubEligibility_OrdinaryMember pins hub-boundary
// eligibility for an ordinary member of one project: a self selector is
// eligible, agent:attach is eligible through the relationship path,
// broker:create is eligible through the hub-member role, and hub_config:read
// and user:list, which the user holds through no role, are refused with
// authority_required.
func TestAuthScopes_HubEligibility_OrdinaryMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-hub-member-project")
	ownerID := tid("authscopes-hub-member-owner")
	memberID := tid("authscopes-hub-member-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	byID := authScopesHubByID(t, srv, member)
	assertHubEligibilityShape(t, byID)

	for _, id := range []string{"inbox:read", "agent:attach", "broker:create"} {
		entry, ok := byID[id]
		require.True(t, ok, "scope %s missing from response", id)
		assert.True(t, entry.Eligibility.Eligible, "scope %s: reason %q", id, entry.Eligibility.Reason)
	}
	assert.Equal(t, string(permissions.MintEligibilityRelationship), byID["agent:attach"].EligibilityKind)

	for _, id := range []string{"hub_config:read", "user:list"} {
		entry, ok := byID[id]
		require.True(t, ok, "scope %s missing from response", id)
		assert.False(t, entry.Eligibility.Eligible, "scope %s", id)
		assert.Equal(t, string(MintDenialAuthorityRequired), entry.Eligibility.Reason, "scope %s", id)
	}
}

// TestAuthScopes_HubEligibility_NeverCollapsesToForbidden pins that a hub
// eligibility request answers per entry with 200 for a user who holds no
// project binding: a hub boundary names no project to hide.
func TestAuthScopes_HubEligibility_NeverCollapsesToForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	userID := hubConfigTokenUser(t, s, "authscopes-hub-nobinding-user", store.SystemRoleHubMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	byID := authScopesHubByID(t, srv, user)
	assertHubEligibilityShape(t, byID)

	assert.True(t, byID["inbox:read"].Eligibility.Eligible, "a self selector is eligible for any active user")
	for _, id := range []string{"agent:attach", "agent:read", "hub_config:read"} {
		entry, ok := byID[id]
		require.True(t, ok, "scope %s missing from response", id)
		assert.False(t, entry.Eligibility.Eligible, "scope %s", id)
		assert.Equal(t, string(MintDenialAuthorityRequired), entry.Eligibility.Reason, "scope %s", id)
	}
}

// TestAuthScopes_HubListingAgreesWithMint pins that the hub listing and
// hub mint agree for the same principal over every catalog selector and
// alias: a selector or alias listed eligible mints; a selector listed
// ineligible is refused with 403 scope_violation naming that selector and
// the listed reason; an alias listed ineligible is refused with 403
// scope_violation naming one of its listed ineligible members.
func TestAuthScopes_HubListingAgreesWithMint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-hub-agree-project")
	ownerID := tid("authscopes-hub-agree-owner")
	memberID := tid("authscopes-hub-agree-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)

	byID := authScopesHubByID(t, srv, member)
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var sawEligible, sawAuthorityRequired bool
	for _, id := range ids {
		elig := byID[id].Eligibility
		require.NotNil(t, elig, "scope %s missing eligibility", id)
		rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/auth/tokens", map[string]any{
			"name": "hub-agree", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{id},
		})
		if elig.Eligible {
			sawEligible = true
			require.Equal(t, http.StatusCreated, rec.Code, "listing said %s eligible, mint should succeed: %s", id, rec.Body.String())
			var created TokenCreateResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
			require.NotNil(t, created.AccessToken)
			// Keep the holder under the per-user token cap.
			require.NoError(t, s.DeleteUserAccessToken(ctx, created.AccessToken.ID))
			continue
		}
		if elig.Reason == string(MintDenialAuthorityRequired) {
			sawAuthorityRequired = true
		}
		require.Equal(t, http.StatusForbidden, rec.Code, "listing said %s ineligible, mint should fail: %s", id, rec.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "scope_violation", resp.Error.Code, "scope %s", id)
		require.NotNil(t, resp.Error.Details, "scope %s", id)
		assert.Equal(t, id, resp.Error.Details["selector"], "scope %s", id)
		assert.Equal(t, elig.Reason, resp.Error.Details["reason"], "scope %s", id)
	}
	assert.True(t, sawEligible, "precondition: some selector is eligible")
	assert.True(t, sawAuthorityRequired, "precondition: some selector is refused with authority_required")

	// Aliases: an alias listed eligible mints; one listed ineligible is
	// refused with 403 scope_violation naming one of its listed ineligible
	// members.
	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/auth/scopes?boundary=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var full AuthScopesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &full))
	require.NotEmpty(t, full.Aliases)
	var sawIneligibleAlias bool
	for _, alias := range full.Aliases {
		require.NotNil(t, alias.Eligibility, "alias %s missing eligibility", alias.ID)
		mint := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/auth/tokens", map[string]any{
			"name": "hub-agree-alias", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{alias.ID},
		})
		if alias.Eligibility.Eligible {
			require.Equal(t, http.StatusCreated, mint.Code, "listing said alias %s eligible, mint should succeed: %s", alias.ID, mint.Body.String())
			var created TokenCreateResponse
			require.NoError(t, json.Unmarshal(mint.Body.Bytes(), &created))
			require.NotNil(t, created.AccessToken)
			require.NoError(t, s.DeleteUserAccessToken(ctx, created.AccessToken.ID))
			continue
		}
		sawIneligibleAlias = true
		require.Equal(t, http.StatusForbidden, mint.Code, "listing said alias %s ineligible, mint should fail: %s", alias.ID, mint.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(mint.Body.Bytes(), &resp))
		assert.Equal(t, "scope_violation", resp.Error.Code, "alias %s", alias.ID)
		require.NotNil(t, resp.Error.Details, "alias %s", alias.ID)
		assert.Contains(t, alias.Eligibility.IneligibleMembers, resp.Error.Details["selector"],
			"alias %s: the refused selector is one of the listed ineligible members", alias.ID)
		sel, _ := resp.Error.Details["selector"].(string)
		entry, ok := byID[sel]
		require.True(t, ok, "alias %s: refused selector %q is in the listing", alias.ID, sel)
		require.NotNil(t, entry.Eligibility, "alias %s: refused selector %q has eligibility", alias.ID, sel)
		assert.Equal(t, entry.Eligibility.Reason, resp.Error.Details["reason"],
			"alias %s: mint's reason for %s matches the listing's reason", alias.ID, sel)
	}
	assert.True(t, sawIneligibleAlias, "precondition: some alias is ineligible on a hub boundary")
}

// TestTokenCreate_HubDenialNamesSelector pins the hub-boundary mint
// refusal: 403 scope_violation naming the selector and authority_required.
// A project-boundary refusal for a non-member stays the bare 403.
func TestTokenCreate_HubDenialNamesSelector(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("authscopes-hubdeny-project")
	ownerID := tid("authscopes-hubdeny-owner")
	createRS1Project(t, s, projectID, ownerID)
	userID := hubConfigTokenUser(t, s, "authscopes-hubdeny-user", store.SystemRoleHubMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "hubdeny", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"inbox:read", "hub_config:read"},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "scope_violation", resp.Error.Code)
	require.NotNil(t, resp.Error.Details)
	assert.Equal(t, "hub_config:read", resp.Error.Details["selector"])
	assert.Equal(t, string(MintDenialAuthorityRequired), resp.Error.Details["reason"])

	rec = doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/auth/tokens", map[string]any{
		"name": "projdeny", "projectId": projectID, "scopes": []string{"agent:read"},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":{"code":"forbidden","message":"forbidden"}}`, rec.Body.String())

	tokens, err := s.ListUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	assert.Empty(t, tokens, "a refused mint stores no token")
}

// TestAuthScopes_TokenCannotRequestBoundaryEligibility pins that a user
// access token reads the selector catalog only: a project token and a hub
// token naming a boundary or project (also an empty or unknown boundary)
// are refused with the session-only 403 before any parameter validation,
// and the same tokens read the catalog with 200.
func TestAuthScopes_TokenCannotRequestBoundaryEligibility(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("authscopes-token-elig-project")
	ownerID := tid("authscopes-token-elig-owner")
	createRS1Project(t, s, projectID, ownerID)
	ensureHubMembership(context.Background(), s, ownerID)

	tokens := map[string]string{
		"project token": mintHubConfigToken(t, srv, ownerID, projectBoundary(projectID), "agent:read"),
		"hub token":     mintHubConfigToken(t, srv, ownerID, hubBoundary(), "inbox:read"),
	}
	refused := []string{
		"/api/v1/auth/scopes?projectId=" + projectID,
		"/api/v1/auth/scopes?boundary=project&projectId=" + projectID,
		"/api/v1/auth/scopes?boundary=hub",
		"/api/v1/auth/scopes?boundary=hub&projectId=" + projectID,
		"/api/v1/auth/scopes?projectId=",
		"/api/v1/auth/scopes?boundary=",
		"/api/v1/auth/scopes?boundary=bogus",
	}
	for name, key := range tokens {
		t.Run(name, func(t *testing.T) {
			for _, path := range refused {
				rec := doRequestWithToken(t, srv, key, http.MethodGet, path, nil)
				require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.Equal(t, ErrCodeForbidden, resp.Error.Code, path)
				require.NotNil(t, resp.Error.Details, path)
				assert.Equal(t, "CREDENTIAL_MANAGEMENT", resp.Error.Details["reason"], path)
				assert.Equal(t, "session_required", resp.Error.Details["credential"], path)
				assert.NotContains(t, rec.Body.String(), `"scopes"`, path)
			}

			rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/auth/scopes", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp AuthScopesResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.Scopes)
			for _, entry := range resp.Scopes {
				assert.Nil(t, entry.Eligibility, "catalog-only response must not include eligibility")
			}
		})
	}
}
