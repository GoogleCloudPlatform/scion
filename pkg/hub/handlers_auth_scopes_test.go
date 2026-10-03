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

// TestHandleAuthScopes_NonUserIdentity pins the parameterless catalog's
// backward compatibility: any authenticated identity, not only a user, gets
// it -- exactly as before this endpoint gained per-project eligibility.
// Eligibility itself is a per-user computation (CanMintSelector requires a
// local user principal), so a non-user identity requesting it gets 401
// instead of a partial or crafted response.
func TestHandleAuthScopes_NonUserIdentity(t *testing.T) {
	srv, _ := testServer(t)
	handler := srv.guarded("/api/v1/auth/scopes", srv.handleAuthScopes)
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("authscopes-agent-identity")},
		ProjectID: tid("authscopes-agent-identity-project"),
	}}

	t.Run("catalog with no params succeeds for a non-user identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/scopes", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agent))

		rr := httptest.NewRecorder()
		handler(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var resp AuthScopesResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.NotEmpty(t, resp.Scopes)
	})

	t.Run("eligibility with projectId requires a user identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/scopes?projectId="+tid("authscopes-agent-identity-project"), nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agent))

		rr := httptest.NewRecorder()
		handler(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
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

// TestAuthScopes_BoundaryParamValidation pins every 400 case for
// ?boundary=/?projectId=, independent of whether hub-boundary eligibility
// is enabled yet: when that single gate is flipped on, these structural
// validations must keep returning 400 unchanged.
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

	t.Run("boundary=hub alone is the single not-yet-enabled gate", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/scopes?boundary=hub", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "unsupported_boundary", resp.Error.Code)
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
