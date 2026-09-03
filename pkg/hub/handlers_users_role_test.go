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

// TestPromoteUser_CreatesRoleBinding verifies that promoting a user to admin
// creates exactly one system-scoped super-admin role binding and syncs the
// compatibility User.Role field.
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
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)

	var superAdminCount int
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			superAdminCount++
		}
	}
	assert.Equal(t, 1, superAdminCount, "exactly one super-admin binding should exist")
}

// TestPromoteUser_Idempotent verifies that repeated promotion does not
// create duplicate role bindings.
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
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)

	var superAdminCount int
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			superAdminCount++
		}
	}
	assert.Equal(t, 1, superAdminCount, "repeated promotion should not create duplicate bindings")
}

// TestDemoteUser_RemovesRoleBinding verifies that demoting an admin removes
// the system-scoped super-admin role binding and syncs User.Role.
func TestDemoteUser_RemovesRoleBinding(t *testing.T) {
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

	// First promote.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Ensure a second admin exists to avoid last-admin guard.
	admin2 := &store.User{
		ID:          tid("admin2"),
		Email:       "admin2@example.com",
		DisplayName: "Admin Two",
		Role:        "admin",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin2))
	srv.ensureSuperAdminBinding(ctx, admin2.ID)

	// Now demote.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+user.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify User.Role is "member".
	updated, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "member", updated.Role)

	// Verify super-admin binding is gone.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)

	for _, b := range bindings {
		assert.False(t, b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID,
			"super-admin binding should be deleted after demotion")
	}
}

// TestDemoteUser_LastSuperAdminBlocked verifies that demoting the last
// super-admin is rejected to prevent lockout.
func TestDemoteUser_LastSuperAdminBlocked(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// The dev user is auto-created as admin. Trigger creation first.
	doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)

	// Find the dev user and ensure they have a super-admin binding.
	result, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	var devUser *store.User
	for i := range result.Items {
		if result.Items[i].Email == "dev@localhost" {
			devUser = &result.Items[i]
			break
		}
	}
	require.NotNil(t, devUser)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	// Create another admin.
	targetAdmin := &store.User{
		ID:          tid("target-admin"),
		Email:       "target@example.com",
		DisplayName: "Target Admin",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, targetAdmin))

	// Promote via API.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+targetAdmin.ID,
		map[string]string{"role": "admin"})
	assert.Equal(t, http.StatusOK, rec.Code)

	// Both dev user and target admin have super-admin bindings.
	// Demoting target admin should succeed (dev user still has one).
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+targetAdmin.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusOK, rec.Code, "demoting non-last admin should succeed")

	// Verify the checkLastSuperAdmin logic works via direct unit test.
	// Remove all super-admin bindings except one user's.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	allBindings, err := s.ListAllRoleBindings(ctx, store.RoleBindingListOptions{Limit: 0})
	require.NoError(t, err)

	var superAdminCount int
	for _, b := range allBindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			superAdminCount++
		}
	}
	// Only dev user should have a super-admin binding now.
	assert.Equal(t, 1, superAdminCount, "only dev user should have super-admin binding")

	// Direct unit test: checkLastSuperAdmin should return error for the sole admin.
	err = srv.checkLastSuperAdmin(ctx, devUser.ID)
	assert.Error(t, err, "checkLastSuperAdmin should reject demotion of the last admin")
	assert.Contains(t, err.Error(), "last super-admin")
}

// TestDemoteUser_SelfLockoutBlocked verifies that an admin cannot demote
// themselves.
func TestDemoteUser_SelfLockoutBlocked(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// The dev user is automatically admin. Get their user record.
	// The dev auth user has email "dev@localhost" and is auto-created.
	// First, trigger user creation by making any request.
	doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)

	// Find the dev user.
	result, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 100})
	require.NoError(t, err)

	var devUser *store.User
	for i := range result.Items {
		if result.Items[i].Email == "dev@localhost" {
			devUser = &result.Items[i]
			break
		}
	}
	require.NotNil(t, devUser, "dev user should exist")

	// Try to demote self.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+devUser.ID,
		map[string]string{"role": "member"})
	assert.Equal(t, http.StatusConflict, rec.Code, "self-demotion should be rejected")
}

// TestPromoteUser_UnsupportedRoleRejected verifies that setting an unsupported
// role value is rejected.
func TestPromoteUser_UnsupportedRoleRejected(t *testing.T) {
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

// TestRoleBindingList_IncludesRoleName verifies that listed role bindings
// include the human-readable roleName field.
func TestRoleBindingList_IncludesRoleName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user and promote to admin.
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

// TestAdminEffectiveAccess_RequiresAuth verifies the effective-access endpoint
// rejects unauthenticated requests.
func TestAdminEffectiveAccess_RequiresAuth(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/admin/effective-access?principalType=user&principalId=foo", nil)
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"unauthenticated request should be rejected, got %d", rec.Code)
}

// TestAdminEffectiveAccess_Success verifies the effective-access endpoint
// returns valid data for an existing user.
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
	assert.Equal(t, "user", resp.PrincipalType)
	assert.Equal(t, user.ID, resp.PrincipalID)
	assert.GreaterOrEqual(t, resp.PotentialPermissionCount, 0)
	assert.GreaterOrEqual(t, resp.EffectivePermissionCount, 0)
	assert.NotNil(t, resp.Boundaries, "boundaries should be non-nil (empty array)")
}
