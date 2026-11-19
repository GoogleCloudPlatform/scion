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
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func epCreateUser(t *testing.T, s store.Store, slug, status string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid("ep-user-" + slug),
		Email:       slug + "@ep-test.example.com",
		DisplayName: "EP " + slug,
		Role:        store.UserRoleMember,
		Status:      status,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func epBindSystemRole(t *testing.T, s store.Store, roleID, principalType, principalID string, notBefore, expiresAt *time.Time) *store.RoleBinding {
	t.Helper()
	createdBy := "system"
	if rd, err := s.GetRoleDefinition(context.Background(), roleID); err == nil && rd != nil && rd.Name == store.SystemRoleSuperAdmin {
		createdBy = store.SystemReconcileCreatedBy
	}
	rb := &store.RoleBinding{
		ID:               tid("ep-rb-" + roleID + "-" + principalType + "-" + principalID),
		RoleDefinitionID: roleID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeSystem,
		NotBefore:        notBefore,
		ExpiresAt:        expiresAt,
		CreatedBy:        createdBy,
	}
	_, err := s.CreateRoleBinding(context.Background(), rb)
	require.NoError(t, err)
	return rb
}

func epCreateCustomSystemRole(t *testing.T, s store.Store, name string, perms []string) *store.RoleDefinition {
	t.Helper()
	rd := &store.RoleDefinition{
		ID:          tid("ep-role-" + name),
		Name:        name,
		Description: "Custom role " + name,
		ScopeType:   store.RoleScopeSystem,
		Permissions: perms,
		System:      false,
	}
	created, err := s.CreateRoleDefinition(context.Background(), rd)
	require.NoError(t, err)
	return created
}

func TestAdminEffectivePermissions_CallerGates(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	ctx := context.Background()

	target := epCreateUser(t, s, "gate-target", store.UserStatusActive)
	ensureHubMembership(ctx, s, target.ID)

	// Super-admin user
	superAdmin := epCreateUser(t, s, "gate-super", store.UserStatusActive)
	superAdmin.Role = store.UserRoleAdmin
	require.NoError(t, s.UpdateUser(ctx, superAdmin))
	superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, superAdmin.ID, nil, nil)

	// Hub-viewer user
	viewer := epCreateUser(t, s, "gate-viewer", store.UserStatusActive)
	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, viewerRole.ID, store.RoleBindingPrincipalUser, viewer.ID, nil, nil)

	// Custom role with user.update ONLY (no role_binding.read)
	userUpdateRole := epCreateCustomSystemRole(t, s, "user-updater-only", []string{"user.read", "user.update"})
	userUpdater := epCreateUser(t, s, "gate-updater", store.UserStatusActive)
	epBindSystemRole(t, s, userUpdateRole.ID, store.RoleBindingPrincipalUser, userUpdater.ID, nil, nil)

	// Custom role with role_binding.read ONLY
	rbReaderRole := epCreateCustomSystemRole(t, s, "rb-reader-only", []string{"role_binding.read"})
	rbReader := epCreateUser(t, s, "gate-rbreader", store.UserStatusActive)
	epBindSystemRole(t, s, rbReaderRole.ID, store.RoleBindingPrincipalUser, rbReader.ID, nil, nil)

	url := "/api/v1/admin/effective-permissions?principalType=user&principalId=" + target.ID

	t.Run("super-admin gets 200", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, superAdmin, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("hub-admin gets 200", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("custom role with role_binding.read gets 200", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, rbReader, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("hub-member gets 403", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, member, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("hub-viewer gets 403", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, viewer, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("custom role with user.update but not role_binding.read gets 403", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, userUpdater, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("unauthenticated gets 401", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})

	t.Run("HEAD short-circuits with 200 for authorized and 403 for unauthorized", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, rbReader, http.MethodHead, "/api/v1/admin/effective-permissions?principalType=user&principalId=nonexistent", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Body.Bytes())

		recDenied := doRequestAsUser(t, srv, member, http.MethodHead, url, nil)
		assert.Equal(t, http.StatusForbidden, recDenied.Code)
	})

	t.Run("unsupported method returns 405 with Allow header", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, url, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Contains(t, rec.Header().Get("Allow"), "GET")
	})

	t.Run("validation 400 and 404 rules", func(t *testing.T) {
		recMissingParams := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/effective-permissions", nil)
		assert.Equal(t, http.StatusBadRequest, recMissingParams.Code)

		recAgent := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/effective-permissions?principalType=agent&principalId="+target.ID, nil)
		assert.Equal(t, http.StatusBadRequest, recAgent.Code)
		assert.Contains(t, recAgent.Body.String(), "unsupported principal type")

		recFederated := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/effective-permissions?principalType=federated_user&principalId="+target.ID, nil)
		assert.Equal(t, http.StatusBadRequest, recFederated.Code)
		assert.Contains(t, recFederated.Body.String(), "unsupported principal type")

		recProject := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, url+"&projectId="+tid("some-proj"), nil)
		assert.Equal(t, http.StatusBadRequest, recProject.Code)

		recNotFound := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/effective-permissions?principalType=user&principalId="+tid("missing-user"), nil)
		assert.Equal(t, http.StatusNotFound, recNotFound.Code)
		assert.Contains(t, recNotFound.Body.String(), "User not found")
	})
}

func TestAdminEffectivePermissions_ProvenanceDirectAndGroup(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	ctx := context.Background()

	target := epCreateUser(t, s, "prov-target", store.UserStatusActive)
	ensureHubMembership(ctx, s, target.ID)

	// Create nested group hierarchy: target -> frontendGroup -> engGroup
	engGroup := &store.Group{
		ID:        tid("ep-grp-eng"),
		Name:      "Engineering",
		Slug:      "engineering",
		GroupType: store.GroupTypeExplicit,
	}
	require.NoError(t, s.CreateGroup(ctx, engGroup))

	frontendGroup := &store.Group{
		ID:        tid("ep-grp-fe"),
		Name:      "Frontend",
		Slug:      "frontend",
		GroupType: store.GroupTypeExplicit,
	}
	require.NoError(t, s.CreateGroup(ctx, frontendGroup))

	// Frontend is a child member of Engineering; target is a member of Frontend.
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    engGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   frontendGroup.ID,
		Role:       store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    frontendGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   target.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Bind a custom system role to Engineering granting user.read, role.read, and role_binding.read.
	engRole := epCreateCustomSystemRole(t, s, "eng-auditor", []string{"user.read", "role.read", "role_binding.read"})
	engBinding := epBindSystemRole(t, s, engRole.ID, store.RoleBindingPrincipalGroup, engGroup.ID, nil, nil)

	// Also bind a direct role to target with an expiration timestamp granting role.read and role_binding.read.
	exp := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	directRole := epCreateCustomSystemRole(t, s, "direct-ops", []string{"role.read", "role_binding.read"})
	directBinding := epBindSystemRole(t, s, directRole.ID, store.RoleBindingPrincipalUser, target.ID, nil, &exp)

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
		"/api/v1/admin/effective-permissions?principalType=user&principalId="+target.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Verify frozen §3.1 JSON keys (null vs empty array semantics).
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Nil(t, raw["statusGate"], "statusGate must serialize as null for active users")
	assert.NotNil(t, raw["permissions"], "permissions must serialize as []")
	assert.NotNil(t, raw["removed"], "removed must serialize as []")

	var resp EffectivePermissionSet
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "user", resp.Principal.Type)
	assert.Equal(t, target.ID, resp.Principal.ID)
	assert.Equal(t, "active", resp.Principal.Status)
	assert.Equal(t, ScopeTypeSystem, resp.Scope.Type)
	assert.Empty(t, resp.Scope.ID)
	_, tsErr := time.Parse(time.RFC3339, resp.EvaluatedAt)
	assert.NoError(t, tsErr, "evaluatedAt must be RFC3339")

	byPerm := make(map[string]EffectivePermissionEntry, len(resp.Permissions))
	for _, p := range resp.Permissions {
		byPerm[p.ID] = p
	}

	// role_binding.read comes from BOTH directBinding and engBinding (via nested Engineering group).
	rbRead, ok := byPerm["role_binding.read"]
	require.True(t, ok, "role_binding.read should be in effective permissions")
	assert.Equal(t, "role_binding", rbRead.Resource)
	assert.Equal(t, "read", rbRead.Action)
	assert.NotEmpty(t, rbRead.Description)
	require.Len(t, rbRead.Grants, 2, "both direct and group-derived grants must be listed")

	grantByBinding := map[string]EffectivePermissionGrant{}
	for _, g := range rbRead.Grants {
		grantByBinding[g.BindingID] = g
	}

	// role.read comes from all 3 sources: direct-ops, eng-auditor, and hub-member.
	roleRead, ok := byPerm["role.read"]
	require.True(t, ok, "role.read should be in effective permissions")
	require.Len(t, roleRead.Grants, 3, "all three contributing grants must be listed")

	directGrant, hasDirect := grantByBinding[directBinding.ID]
	require.True(t, hasDirect, "direct binding should be present")
	assert.Equal(t, directRole.ID, directGrant.Role.ID)
	assert.Equal(t, "direct-ops", directGrant.Role.Name)
	assert.False(t, directGrant.Role.System)
	assert.Equal(t, ScopeTypeSystem, directGrant.Scope.Type)
	assert.Nil(t, directGrant.Via, "direct grant must have via=null")
	require.NotNil(t, directGrant.ExpiresAt)
	assert.Equal(t, exp.Format(time.RFC3339), *directGrant.ExpiresAt)

	groupGrant, hasGroup := grantByBinding[engBinding.ID]
	require.True(t, hasGroup, "nested group binding should be present")
	assert.Equal(t, engRole.ID, groupGrant.Role.ID)
	assert.Equal(t, "eng-auditor", groupGrant.Role.Name)
	assert.False(t, groupGrant.Role.System)
	require.NotNil(t, groupGrant.Via)
	assert.Equal(t, engGroup.ID, groupGrant.Via.GroupID)
	assert.Equal(t, "Engineering", groupGrant.Via.GroupName)
	assert.Equal(t, "engineering", groupGrant.Via.GroupSlug)
	assert.Empty(t, groupGrant.Via.Path, "D2: shallow provenance omits via.path")
	assert.Nil(t, groupGrant.ExpiresAt)

	// user.read comes from hub-member (via hub-members) AND eng-auditor (via Engineering).
	userRead, ok := byPerm["user.read"]
	require.True(t, ok, "user.read should be in effective permissions")
	assert.GreaterOrEqual(t, len(userRead.Grants), 2)
}

func TestAdminEffectivePermissions_ActivationStatusAndTestIdentityClamps(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	ctx := context.Background()

	t.Run("expired and future bindings contribute nothing", func(t *testing.T) {
		u := epCreateUser(t, s, "timing-user", store.UserStatusActive)
		past := time.Now().UTC().Add(-1 * time.Hour)
		future := time.Now().UTC().Add(1 * time.Hour)

		expiredRole := epCreateCustomSystemRole(t, s, "expired-role", []string{"broker.read"})
		futureRole := epCreateCustomSystemRole(t, s, "future-role", []string{"quota.read"})
		activeRole := epCreateCustomSystemRole(t, s, "active-role", []string{"role.read"})

		epBindSystemRole(t, s, expiredRole.ID, store.RoleBindingPrincipalUser, u.ID, nil, &past)
		epBindSystemRole(t, s, futureRole.ID, store.RoleBindingPrincipalUser, u.ID, &future, nil)
		epBindSystemRole(t, s, activeRole.ID, store.RoleBindingPrincipalUser, u.ID, nil, nil)

		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+u.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Permissions, 1)
		assert.Equal(t, "role.read", resp.Permissions[0].ID)
	})

	t.Run("suspended and invited users return statusGate with empty lists", func(t *testing.T) {
		hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
		require.NoError(t, err)

		for _, status := range []string{store.UserStatusSuspended, store.UserStatusInvited} {
			u := epCreateUser(t, s, "inactive-"+status, status)
			epBindSystemRole(t, s, hubAdminRole.ID, store.RoleBindingPrincipalUser, u.ID, nil, nil)

			rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
				"/api/v1/admin/effective-permissions?principalType=user&principalId="+u.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			var resp EffectivePermissionSet
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, status, resp.Principal.Status)
			require.NotNil(t, resp.StatusGate)
			assert.Equal(t, status, resp.StatusGate.Reason)
			assert.Empty(t, resp.Permissions)
			assert.Empty(t, resp.Removed)
		}
	})

	t.Run("test-fixture user has non-member system grants clamped", func(t *testing.T) {
		fxExp := time.Now().UTC().Add(30 * time.Minute)
		fx := &store.User{
			ID:        tid("ep-test-fixture"),
			Email:     "fx@" + store.TestFixtureEmailDomain,
			Role:      store.UserRoleMember,
			Status:    store.UserStatusActive,
			Kind:      store.UserKindTestFixture,
			ExpiresAt: &fxExp,
			IssuedBy:  &hubAdmin.ID,
			Created:   time.Now().UTC(),
		}
		require.NoError(t, s.CreateTestFixtureUser(ctx, fx))
		ensureHubMembership(ctx, s, fx.ID)

		// Attach a custom system role to a group containing fx — testFixtureGrantClamp must strip it!
		fxExtraGroup := &store.Group{
			ID:        tid("ep-fx-extra-grp"),
			Name:      "Fixture Extra",
			Slug:      "fixture-extra",
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, fxExtraGroup))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    fxExtraGroup.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   fx.ID,
			Role:       store.GroupMemberRoleMember,
		}))
		extraRole := epCreateCustomSystemRole(t, s, "fx-extra", []string{"role.create"})
		epBindSystemRole(t, s, extraRole.ID, store.RoleBindingPrincipalGroup, fxExtraGroup.ID, nil, nil)

		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+fx.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for _, p := range resp.Permissions {
			assert.NotEqual(t, "role.create", p.ID, "clamped test_fixture must not show custom system role permissions")
			for _, g := range p.Grants {
				assert.Contains(t, []string{store.SystemRoleHubMember, store.SystemRoleHubViewer}, g.Role.Name)
			}
		}
		assert.NotEmpty(t, resp.Permissions, "hub-member permissions should remain")
	})

	t.Run("caged test_super_admin lacks deny-classified permissions", func(t *testing.T) {
		setAuthzTestAdminTier(srv.authzService, func(kind string) bool {
			return kind == store.UserKindTestSuperAdmin
		}, nil)
		t.Cleanup(func() {
			setAuthzTestAdminTier(srv.authzService, nil, nil)
		})

		adminExp := time.Now().UTC().Add(20 * time.Minute)
		purpose := "effective permissions test"
		issuance := tid("ep-test-super-issuance")
		testSuper := &store.User{
			ID:         tid("ep-test-super"),
			Email:      "testsuper@" + store.TestAdminEmailDomain(store.UserKindTestSuperAdmin),
			Role:       store.TestAdminRole(store.UserKindTestSuperAdmin),
			Status:     store.UserStatusActive,
			Kind:       store.UserKindTestSuperAdmin,
			ExpiresAt:  &adminExp,
			IssuedBy:   &hubAdmin.ID,
			Purpose:    &purpose,
			IssuanceID: &issuance,
			Created:    time.Now().UTC(),
		}
		superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		_, err = s.CreateTestAdminUser(ctx, testSuper, superRole.ID)
		require.NoError(t, err)

		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+testSuper.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.NotEmpty(t, resp.Permissions)
		for _, p := range resp.Permissions {
			assert.False(t, testAdminPermissionDenied(p.ID),
				"caged test_super_admin must not list deny-classified permission %s", p.ID)
		}
	})
}

func TestAdminEffectivePermissions_AccessConstraintRemovals(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	ctx := context.Background()

	target := epCreateUser(t, s, "constrained-target", store.UserStatusActive)
	role := epCreateCustomSystemRole(t, s, "ops-plus-delete", []string{"project.read", "agent.delete"})
	epBindSystemRole(t, s, role.ID, store.RoleBindingPrincipalUser, target.ID, nil, nil)

	pType := store.RoleBindingPrincipalUser
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "contractors-cap",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &target.ID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"project.read"},
		Purpose:              "cap agent.delete",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)

	// Direct resolver check: populates unredacted boundary metadata.
	resolved, err := srv.authzService.ResolveEffectivePermissions(ctx,
		PrincipalContext{Kind: PrincipalKindUser, ID: target.ID}, EffectiveScope{})
	require.NoError(t, err)
	require.Len(t, resolved.Permissions, 1)
	assert.Equal(t, "project.read", resolved.Permissions[0].ID)
	require.Len(t, resolved.Removed, 1)
	assert.Equal(t, "agent.delete", resolved.Removed[0].ID)
	require.Len(t, resolved.Removed[0].RemovedBy, 1)
	assert.Equal(t, "contractors-cap", resolved.Removed[0].RemovedBy[0].Name)
	assert.Equal(t, ScopeTypeSystem, resolved.Removed[0].RemovedBy[0].ScopeType)

	// HTTP check: includes removed entry in response.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
		"/api/v1/admin/effective-permissions?principalType=user&principalId="+target.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp EffectivePermissionSet
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Permissions, 1)
	assert.Equal(t, "project.read", resp.Permissions[0].ID)
	require.Len(t, resp.Removed, 1)
	assert.Equal(t, "agent.delete", resp.Removed[0].ID)
}

type epCountingStore struct {
	store.Store
	counting atomic.Bool
	reads    atomic.Int64
}

func (c *epCountingStore) DB() *sql.DB {
	if p, ok := c.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (c *epCountingStore) Dialect() string {
	if p, ok := c.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (c *epCountingStore) bump() {
	if c.counting.Load() {
		c.reads.Add(1)
	}
}

func (c *epCountingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.bump()
	return c.Store.GetUser(ctx, id)
}

func (c *epCountingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	c.bump()
	return c.Store.GetEffectiveGroups(ctx, userID)
}

func (c *epCountingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	c.bump()
	return c.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (c *epCountingStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	c.bump()
	return c.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

func (c *epCountingStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	c.bump()
	return c.Store.GetRoleDefinition(ctx, id)
}

func (c *epCountingStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	c.bump()
	return c.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

func (c *epCountingStore) GetGroupsByIDs(ctx context.Context, ids []string) ([]store.Group, error) {
	c.bump()
	return c.Store.GetGroupsByIDs(ctx, ids)
}

func (c *epCountingStore) GetParentGroups(ctx context.Context, groupID string) ([]string, error) {
	c.bump()
	return c.Store.GetParentGroups(ctx, groupID)
}

func (c *epCountingStore) GetUserGroups(ctx context.Context, userID string) ([]store.GroupMember, error) {
	c.bump()
	return c.Store.GetUserGroups(ctx, userID)
}

func (c *epCountingStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	c.bump()
	return c.Store.ListAccessConstraints(ctx, limit, offset)
}

func TestAdminEffectivePermissions_NoAuditNoiseAndFixedQueryCount(t *testing.T) {
	rawStore, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	counting := &epCountingStore{Store: rawStore}
	srv, s := testServerOnMigratedStore(t, counting)
	ctx := context.Background()

	// Create hub-admin caller
	hubAdmin := epCreateUser(t, s, "count-caller", store.UserStatusActive)
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, hubAdminRole.ID, store.RoleBindingPrincipalUser, hubAdmin.ID, nil, nil)

	// Target A: 1 group, small role
	targetSmall := epCreateUser(t, s, "count-small", store.UserStatusActive)
	gSmall := &store.Group{ID: tid("ep-cnt-g-small"), Name: "Small", Slug: "small", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, gSmall))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: gSmall.ID, MemberType: store.GroupMemberTypeUser, MemberID: targetSmall.ID, Role: store.GroupMemberRoleMember,
	}))
	smallRole := epCreateCustomSystemRole(t, s, "small-role", []string{"role.read"})
	epBindSystemRole(t, s, smallRole.ID, store.RoleBindingPrincipalGroup, gSmall.ID, nil, nil)

	// Target B: 15 groups + super-admin (~150 permissions)
	targetLarge := epCreateUser(t, s, "count-large", store.UserStatusActive)
	superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, targetLarge.ID, nil, nil)
	for i := 0; i < 15; i++ {
		g := &store.Group{
			ID:        tid(fmt.Sprintf("ep-cnt-g-%d", i)),
			Name:      fmt.Sprintf("Group %d", i),
			Slug:      fmt.Sprintf("group-%d", i),
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, g))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: targetLarge.ID, Role: store.GroupMemberRoleMember,
		}))
		epBindSystemRole(t, s, smallRole.ID, store.RoleBindingPrincipalGroup, g.ID, nil, nil)
	}

	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	t.Cleanup(func() { srv.authzService.SetDecisionAuditEmitter(nil) })

	// Warm clamp kind cache once so cold-cache one-time kind read does not skew comparison.
	_, err = srv.authzService.ResolveEffectivePermissions(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: targetSmall.ID}, EffectiveScope{})
	require.NoError(t, err)
	_, err = srv.authzService.ResolveEffectivePermissions(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: targetLarge.ID}, EffectiveScope{})
	require.NoError(t, err)
	assert.Empty(t, emitter.records, "ResolveEffectivePermissions itself must emit zero decision-audit records")

	// Measure resolver store reads: Small vs Large
	counting.reads.Store(0)
	counting.counting.Store(true)
	resSmall, err := srv.authzService.ResolveEffectivePermissions(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: targetSmall.ID}, EffectiveScope{})
	counting.counting.Store(false)
	require.NoError(t, err)
	smallReads := counting.reads.Load()
	require.Len(t, resSmall.Permissions, 1)

	counting.reads.Store(0)
	counting.counting.Store(true)
	resLarge, err := srv.authzService.ResolveEffectivePermissions(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: targetLarge.ID}, EffectiveScope{})
	counting.counting.Store(false)
	require.NoError(t, err)
	largeReads := counting.reads.Load()
	assert.Greater(t, len(resLarge.Permissions), 100)

	assert.Equal(t, smallReads, largeReads, "store-call count must be independent of permission and group count")
	assert.LessOrEqual(t, largeReads, int64(6), "resolver should use at most 6 batch/point store reads")

	// HTTP endpoint check: only the 1 caller route-guard audit record, zero target permission audit records.
	emitter.records = nil
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
		"/api/v1/admin/effective-permissions?principalType=user&principalId="+targetLarge.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, emitter.records, 1, "HTTP call should emit only the single route-guard Decide record, never per-permission Decide records")
	assert.Equal(t, "role_binding.read", emitter.records[0].PermissionID)
	assert.Equal(t, "admin.effectivePermissions", emitter.records[0].Route)
	assert.Equal(t, hubAdmin.ID, emitter.records[0].PrincipalID)
}

func TestResolveEffectivePermissions_SystemParityWithDecide(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	ctx := context.Background()

	superAdmin := epCreateUser(t, s, "parity-super", store.UserStatusActive)
	superAdmin.Role = store.UserRoleAdmin
	require.NoError(t, s.UpdateUser(ctx, superAdmin))
	superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, superAdmin.ID, nil, nil)

	hubResourceFor := func(p permissions.Permission) Resource {
		res := Resource{Type: p.Resource, ID: "hub-parity-target"}
		switch p.Resource {
		case permissions.ResourceSkill:
			res.ScopeKind = store.SkillScopeGlobal
		case permissions.ResourceTemplate:
			res.ScopeKind = store.TemplateScopeGlobal
		case permissions.ResourceHarnessConfig:
			res.ScopeKind = store.HarnessConfigScopeGlobal
		}
		return res
	}

	for _, u := range []*store.User{member, hubAdmin, superAdmin} {
		t.Run(u.Email, func(t *testing.T) {
			set, err := srv.authzService.ResolveEffectivePermissions(ctx,
				PrincipalContext{Kind: PrincipalKindUser, ID: u.ID}, EffectiveScope{})
			require.NoError(t, err)

			resolved := make(map[string]bool, len(set.Permissions))
			for _, p := range set.Permissions {
				resolved[p.ID] = true
			}

			ident := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")
			for _, perm := range permissions.Registry {
				if perm.IsReserved() {
					assert.False(t, resolved[perm.ID], "reserved permission %s must never appear", perm.ID)
					continue
				}
				d := srv.authzService.Decide(ctx, AuthzRequest{
					Principal:  principalContextForIdentity(ident),
					Credential: credentialContextForIdentity(ident),
					Resource:   hubResourceFor(perm),
					Action:     Action(perm.Action),
					Permission: perm.ID,
				})
				// Relationship grants (e.g., hub_member_sa_assign on hub-scoped SAs)
				// are outside role-binding scope resolution (§2, §3.4).
				if d.Allowed && d.BindingID == "" {
					continue
				}
				assert.Equal(t, d.Allowed, resolved[perm.ID],
					"parity mismatch for %s on %s (Decide reason=%q binding=%q)",
					u.Email, perm.ID, d.Reason, d.BindingID)
			}
		})
	}
}
