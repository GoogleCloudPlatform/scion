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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// superAdminBindingCount counts system-scoped super-admin bindings for a user.
func superAdminBindingCount(t *testing.T, s store.Store, userID string) int {
	t.Helper()
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)

	count := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			count++
		}
	}
	return count
}

// hubMemberBindingCount counts system-scoped hub-member bindings for a user.
func hubMemberBindingCount(t *testing.T, s store.Store, userID string) int {
	t.Helper()
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)

	count := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			count++
		}
	}
	return count
}

// getDevUser triggers provisioning and returns the dev user record.
func getDevUser(t *testing.T, srv *Server, s store.Store) *store.User {
	t.Helper()
	ctx := context.Background()

	// Trigger dev user creation.
	doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)

	result, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	for i := range result.Items {
		if result.Items[i].Email == "dev@localhost" {
			return &result.Items[i]
		}
	}
	t.Fatal("dev user not found")
	return nil
}

// ---------------------------------------------------------------------------
// Promotion tests
// ---------------------------------------------------------------------------

func TestPromoteUser_CreatesRoleBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("promote-target"),
		Email:       "promote@example.com",
		DisplayName: "Promote Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code, "promote should succeed")

	// Verify User.Role is synced.
	updated, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "admin", updated.Role, "User.Role should be admin")

	// Verify super-admin binding exists.
	assert.Equal(t, 1, superAdminBindingCount(t, s, user.ID),
		"exactly one super-admin binding should exist")
}

func TestPromoteUser_Idempotent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("promote-idempotent"),
		Email:       "idempotent@example.com",
		DisplayName: "Idempotent",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Promote twice.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify still exactly one binding.
	assert.Equal(t, 1, superAdminBindingCount(t, s, user.ID),
		"repeated promotion should not create duplicate bindings")
}

// ---------------------------------------------------------------------------
// Demotion tests
// ---------------------------------------------------------------------------

func TestDemoteUser_RemovesBindingAndCreatesHubMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("demote-target"),
		Email:       "demote@example.com",
		DisplayName: "Demote Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Promote first.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Ensure a second admin exists so this isn't the last admin.
	admin2 := &store.User{
		ID:          tid("admin2"),
		Email:       "admin2@example.com",
		DisplayName: "Admin Two",
		Role:        "admin",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin2))
	srv.ensureSuperAdminBinding(ctx, admin2.ID)

	// Demote.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify User.Role is "member".
	updated, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "member", updated.Role)

	// Verify super-admin binding is gone.
	assert.Equal(t, 0, superAdminBindingCount(t, s, user.ID),
		"super-admin binding should be deleted after demotion")

	// Verify hub-member binding was created.
	assert.Equal(t, 1, hubMemberBindingCount(t, s, user.ID),
		"hub-member binding should exist after demotion")
}

// ---------------------------------------------------------------------------
// Last-admin guard
// ---------------------------------------------------------------------------

func TestDemoteUser_LastSuperAdminBlocked(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// The dev user is admin. Ensure it has a binding.
	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	// Create another admin with binding.
	target := &store.User{
		ID:          tid("target-admin"),
		Email:       "target@example.com",
		DisplayName: "Target Admin",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	// Promote via API.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+target.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Demote target: should succeed since dev user still has binding.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+target.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusOK, rec.Code, "demoting non-last admin should succeed")

	// Direct test of checkLastSuperAdminTx for the sole remaining admin.
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	err = srv.checkLastSuperAdminTx(ctx, s, devUser.ID, superAdminRD)
	assert.ErrorIs(t, err, errLastSuperAdmin,
		"checkLastSuperAdminTx should reject demotion of the last admin")
}

// ---------------------------------------------------------------------------
// Self-lockout
// ---------------------------------------------------------------------------

func TestDemoteUser_SelfLockoutBlocked(t *testing.T) {
	srv, s := testServer(t)

	devUser := getDevUser(t, srv, s)

	// Try to demote self.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusConflict, rec.Code, "self-demotion should be rejected")
}

// ---------------------------------------------------------------------------
// Role validation
// ---------------------------------------------------------------------------

func TestUpdateUser_UnsupportedRoleRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("bad-role"),
		Email:       "badrole@example.com",
		DisplayName: "Bad Role",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "superuser"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unsupported role should be rejected")
}

func TestUpdateUser_UnsupportedRoleRejectedEvenIfMatching(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user with legacy "viewer" role to test that setting
	// role="viewer" is rejected even when it matches the current role.
	user := &store.User{
		ID:          tid("legacy-viewer"),
		Email:       "viewer@example.com",
		DisplayName: "Legacy Viewer",
		Role:        "viewer",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "viewer"})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"unsupported role 'viewer' should be rejected even when matching current role")
}

func TestUpdateUser_MetadataPreservedWhenRoleOmitted(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("metadata-test"),
		Email:       "metadata@example.com",
		DisplayName: "Original Name",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Update only displayName, omitting role entirely.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"displayName": "Updated Name"})
	assert.Equal(t, http.StatusOK, rec.Code)

	updated, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", updated.DisplayName)
	assert.Equal(t, "member", updated.Role, "role should be unchanged")
}

// ---------------------------------------------------------------------------
// Same-role repair (R2 item 4)
// ---------------------------------------------------------------------------

func TestSameRoleRepair_AdminWithMissingBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user who is role=admin but has NO super-admin binding
	// (simulates pre-migration state or orphaned record).
	user := &store.User{
		ID:          tid("orphan-admin"),
		Email:       "orphan@example.com",
		DisplayName: "Orphan Admin",
		Role:        "admin",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Verify no binding exists yet.
	assert.Equal(t, 0, superAdminBindingCount(t, s, user.ID))

	// PATCH with role=admin (same role) — should repair the missing binding.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Binding should now exist.
	assert.Equal(t, 1, superAdminBindingCount(t, s, user.ID),
		"same-role admin request should repair missing binding")
}

func TestSameRoleRepair_MemberWithStaleBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("stale-binding"),
		Email:       "stale@example.com",
		DisplayName: "Stale Binding",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Manually create a super-admin binding (simulates stale state).
	srv.ensureSuperAdminBinding(ctx, user.ID)
	assert.Equal(t, 1, superAdminBindingCount(t, s, user.ID))

	// PATCH with role=member (same role) — should clean up the stale binding.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Stale binding should be removed.
	assert.Equal(t, 0, superAdminBindingCount(t, s, user.ID),
		"same-role member request should clean up stale super-admin binding")
}

// ---------------------------------------------------------------------------
// Atomicity: binding + User.Role in same transaction
// ---------------------------------------------------------------------------

func TestPromotion_AtomicBindingAndRole(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("atomic-promote"),
		Email:       "atomic@example.com",
		DisplayName: "Atomic",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// After successful promotion, both binding AND User.Role must be updated.
	updated, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "admin", updated.Role)
	assert.Equal(t, 1, superAdminBindingCount(t, s, user.ID))
}

func TestDemotion_AtomicBindingAndRole(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("atomic-demote"),
		Email:       "atomicdemote@example.com",
		DisplayName: "Atomic Demote",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Promote first.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	require.Equal(t, http.StatusOK, rec.Code)

	// Add a second admin.
	admin2 := &store.User{
		ID:          tid("atomic-admin2"),
		Email:       "atomicadmin2@example.com",
		DisplayName: "Admin 2",
		Role:        "admin",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin2))
	srv.ensureSuperAdminBinding(ctx, admin2.ID)

	// Demote.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Both binding removal AND User.Role update must be atomic.
	updated, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "member", updated.Role)
	assert.Equal(t, 0, superAdminBindingCount(t, s, user.ID))
}

// ---------------------------------------------------------------------------
// Authorization: CanDelegate tests
// ---------------------------------------------------------------------------

func TestPromoteUser_NonAdminDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user.
	actor := &store.User{
		ID:          tid("non-admin-actor"),
		Email:       "nonadmin@example.com",
		DisplayName: "Non Admin",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, actor))

	target := &store.User{
		ID:          tid("promote-target-2"),
		Email:       "target2@example.com",
		DisplayName: "Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	// Request as non-admin — should be denied by CanDelegate.
	rec := doRequestAsUser(t, srv, actor, http.MethodPatch,
		"/api/v1/users/"+target.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-admin user should not be able to promote")
}

func TestDemoteUser_SuperAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	target := &store.User{
		ID:          tid("sa-demote-target"),
		Email:       "sademote@example.com",
		DisplayName: "SA Demote Target",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, target))

	// Promote via dev user (super-admin).
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+target.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code, "super-admin should be able to promote")
}

// ---------------------------------------------------------------------------
// Role binding name enrichment
// ---------------------------------------------------------------------------

func TestRoleBindingList_IncludesRoleName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("role-name-test"),
		Email:       "rolename@example.com",
		DisplayName: "Role Name Test",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// List bindings for this user.
	rec = doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/role-bindings/user/"+user.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Items []struct {
			RoleName         string `json:"roleName"`
			RoleDefinitionID string `json:"roleDefinitionId"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	found := false
	for _, item := range resp.Items {
		if item.RoleName == store.SystemRoleSuperAdmin {
			found = true
			assert.NotEmpty(t, item.RoleDefinitionID, "roleDefinitionId should be present")
			break
		}
	}
	assert.True(t, found, "binding list should include roleName=%q", store.SystemRoleSuperAdmin)
}

// ---------------------------------------------------------------------------
// Effective-access endpoint tests
// ---------------------------------------------------------------------------

func TestAdminEffectiveAccess_RequiresAuth(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/admin/effective-access?principalType=user&principalId=foo", nil)
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"unauthenticated request should be rejected, got %d", rec.Code)
}

func TestAdminEffectiveAccess_Success(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("eff-access-user"),
		Email:       "effaccess@example.com",
		DisplayName: "Effective Access",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/effective-access?principalType=user&principalId="+user.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp adminEffectiveAccessResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Response uses system scope, not fabricated permission counts.
	assert.Equal(t, store.RoleScopeSystem, resp.ScopeType)
	assert.GreaterOrEqual(t, resp.ActiveBindingCount, 0)
	assert.NotNil(t, resp.Boundaries, "boundaries should be non-nil (empty array)")
}

func TestAdminEffectiveAccess_NoPrincipalIDExposure(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("privacy-test"),
		Email:       "privacy@example.com",
		DisplayName: "Privacy Test",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/effective-access?principalType=user&principalId="+user.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Parse response as raw JSON — principalId and principalType should NOT
	// be present in the response body.
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))

	_, hasPrincipalID := raw["principalId"]
	_, hasPrincipalType := raw["principalType"]
	assert.False(t, hasPrincipalID, "response should not echo principalId")
	assert.False(t, hasPrincipalType, "response should not echo principalType")
}

func TestAdminEffectiveAccess_SystemScopeOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("scope-test"),
		Email:       "scope@example.com",
		DisplayName: "Scope Test",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/effective-access?principalType=user&principalId="+user.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp adminEffectiveAccessResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Endpoint must report system scope.
	assert.Equal(t, store.RoleScopeSystem, resp.ScopeType,
		"effective-access endpoint should report system scope")
}

func TestAdminEffectiveAccess_FailsClosedWithoutAuthz(t *testing.T) {
	// This test verifies that if authzService is nil, the endpoint fails
	// closed rather than bypassing authorization.
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("no-authz-user"),
		Email:       "noauthz@example.com",
		DisplayName: "No Authz",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Save and nil out authzService.
	savedAuthz := srv.authzService
	srv.authzService = nil
	defer func() { srv.authzService = savedAuthz }()

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/effective-access?principalType=user&principalId="+user.ID, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"endpoint should fail closed when authzService is nil")
}

// ---------------------------------------------------------------------------
// Role mutation fails closed without authzService
// ---------------------------------------------------------------------------

func TestRoleMutation_FailsClosedWithoutAuthz(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("no-authz-role"),
		Email:       "noauthzrole@example.com",
		DisplayName: "No Authz Role",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Save and nil out authzService.
	savedAuthz := srv.authzService
	srv.authzService = nil
	defer func() { srv.authzService = savedAuthz }()

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"role mutation should fail closed when authzService is nil")
}
