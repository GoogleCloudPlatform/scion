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

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func epSetExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.EffectivePermissions {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.EffectivePermissions))
}

func TestAdminEffectivePermissions_ExperimentGate(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	ctx := context.Background()

	target := epCreateUser(t, s, "exp-gate-target", store.UserStatusActive)
	ensureHubMembership(ctx, s, target.ID)

	url := "/api/v1/admin/effective-permissions?principalType=user&principalId=" + target.ID

	t.Run("default off returns 404 for GET and HEAD", func(t *testing.T) {
		assert.False(t, srv.experimentEnabled(experiments.EffectivePermissions))

		recGet := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusNotFound, recGet.Code, recGet.Body.String())

		recHead := doRequestAsUser(t, srv, hubAdmin, http.MethodHead, url, nil)
		assert.Equal(t, http.StatusNotFound, recHead.Code, recHead.Body.String())
	})

	t.Run("enabled returns 200 for GET and HEAD", func(t *testing.T) {
		epSetExperiment(t, srv, true)

		recGet := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusOK, recGet.Code, recGet.Body.String())

		recHead := doRequestAsUser(t, srv, hubAdmin, http.MethodHead, url, nil)
		assert.Equal(t, http.StatusOK, recHead.Code, recHead.Body.String())
	})

	t.Run("disabled again returns 404", func(t *testing.T) {
		epSetExperiment(t, srv, false)

		recGet := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusNotFound, recGet.Code, recGet.Body.String())
	})
}

// TestAdminEffectivePermissions_ScopedTokens pins bearer selector and boundary
// enforcement for GET and HEAD /api/v1/admin/effective-permissions while the
// hub.effective_permissions experiment defaults to off in the generic matrix.
func TestAdminEffectivePermissions_ScopedTokens(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	epSetExperiment(t, srv, true)
	ctx := context.Background()

	target := epCreateUser(t, s, "tok-target", store.UserStatusActive)
	ensureHubMembership(ctx, s, target.ID)
	proj := epCreateProject(t, srv, s, "ep-tok-proj", "EP Token Project", hubAdmin.ID)

	mintHub := func(scopes ...string) string {
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(hubAdmin.ID), CreateTokenParams{
			UserID: hubAdmin.ID, Name: "ep-hub-" + tid("tok"), Boundary: hubBoundary(), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}
	mintProj := func(scopes ...string) string {
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(hubAdmin.ID), CreateTokenParams{
			UserID: hubAdmin.ID, Name: "ep-proj-" + tid("tok"), Boundary: projectBoundary(proj.ID), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}

	// Project-bounded tokens cannot mint hub-only role_binding:read.
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(hubAdmin.ID), CreateTokenParams{
		UserID: hubAdmin.ID, Name: "ep-proj-rb-" + tid("tok"), Boundary: projectBoundary(proj.ID), Scopes: []string{"role_binding:read"},
	})
	require.Error(t, err, "project-bounded token must not mint role_binding:read")

	rbReadHubTok := mintHub("role_binding:read")
	rbAndProjReadHubTok := mintHub("role_binding:read", "project:read")
	unrelatedHubTok := mintHub("template:read")
	projTok := mintProj("project:read")

	hubURL := "/api/v1/admin/effective-permissions?principalType=user&principalId=" + target.ID
	projURL := hubURL + "&projectId=" + proj.ID

	// GET requires hub boundary + role_binding:read.
	rec := doRequestWithToken(t, srv, rbReadHubTok, http.MethodGet, hubURL, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp EffectivePermissionSet
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, target.ID, resp.Principal.ID)

	rec = doRequestWithToken(t, srv, unrelatedHubTok, http.MethodGet, hubURL, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, projTok, http.MethodGet, hubURL, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Project-scoped GET additionally requires project:read visibility.
	rec = doRequestWithToken(t, srv, rbReadHubTok, http.MethodGet, projURL, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, rbAndProjReadHubTok, http.MethodGet, projURL, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// HEAD requires hub boundary + role_binding:read.
	rec = doRequestWithToken(t, srv, rbReadHubTok, http.MethodHead, hubURL, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, unrelatedHubTok, http.MethodHead, hubURL, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, projTok, http.MethodHead, hubURL, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestAdminEffectivePermissions_HubAppliesInProjectsCatalogFilter(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	epSetExperiment(t, srv, true)
	ctx := context.Background()

	viewer := epCreateUser(t, s, "applies-viewer", store.UserStatusActive)
	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, viewerRole.ID, store.RoleBindingPrincipalUser, viewer.ID, nil, nil)

	fetchSystem := func(t *testing.T, targetID string) map[string]EffectivePermissionEntry {
		t.Helper()
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+targetID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		byPerm := make(map[string]EffectivePermissionEntry, len(resp.Permissions))
		for _, p := range resp.Permissions {
			byPerm[p.ID] = p
		}
		return byPerm
	}

	t.Run("hub-member curated catalog grants report appliesInProjects=false", func(t *testing.T) {
		byPerm := fetchSystem(t, member.ID)
		for _, id := range []string{"skill.read", "template.read", "harness_config.read"} {
			entry, ok := byPerm[id]
			require.True(t, ok, "hub-member should hold %s at hub scope", id)
			assert.False(t, entry.AppliesInProjects,
				"hub-member %s must report appliesInProjects=false because applyHubWideScopeFilters strips it in projects", id)
		}
	})

	t.Run("hub-viewer curated catalog grants report appliesInProjects=false", func(t *testing.T) {
		byPerm := fetchSystem(t, viewer.ID)
		for _, id := range []string{"skill.read", "template.read", "harness_config.read"} {
			entry, ok := byPerm[id]
			require.True(t, ok, "hub-viewer should hold %s at hub scope", id)
			assert.False(t, entry.AppliesInProjects,
				"hub-viewer %s must report appliesInProjects=false because applyHubWideScopeFilters strips it in projects", id)
		}
	})

	t.Run("hub-admin project-applicable grants report appliesInProjects=true and hub-only false", func(t *testing.T) {
		byPerm := fetchSystem(t, hubAdmin.ID)
		for _, id := range []string{"project.read", "skill.read"} {
			entry, ok := byPerm[id]
			require.True(t, ok, "hub-admin should hold %s at hub scope", id)
			assert.True(t, entry.AppliesInProjects,
				"hub-admin %s must report appliesInProjects=true", id)
		}
		userRead, ok := byPerm["user.read"]
		require.True(t, ok, "hub-admin should hold user.read at hub scope")
		assert.False(t, userRead.AppliesInProjects, "hub-only user.read must report appliesInProjects=false")
	})

	t.Run("super-admin project-applicable grants report appliesInProjects=true", func(t *testing.T) {
		superAdmin := epCreateUser(t, s, "applies-super", store.UserStatusActive)
		superAdmin.Role = store.UserRoleAdmin
		require.NoError(t, s.UpdateUser(ctx, superAdmin))
		superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, superAdmin.ID, nil, nil)

		byPerm := fetchSystem(t, superAdmin.ID)
		for _, id := range []string{"agent.create", "template.read", "harness_config.read"} {
			entry, ok := byPerm[id]
			require.True(t, ok, "super-admin should hold %s at hub scope", id)
			assert.True(t, entry.AppliesInProjects, "super-admin %s must report appliesInProjects=true", id)
		}
	})
}

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

func epCreateCustomProjectRole(t *testing.T, s store.Store, name string, perms []string) *store.RoleDefinition {
	t.Helper()
	rd := &store.RoleDefinition{
		ID:          tid("ep-proj-role-" + name),
		Name:        name,
		Description: "Custom project role " + name,
		ScopeType:   store.RoleScopeProject,
		Permissions: perms,
		System:      false,
	}
	created, err := s.CreateRoleDefinition(context.Background(), rd)
	require.NoError(t, err)
	return created
}

func epCreateProject(t *testing.T, srv *Server, s store.Store, slug, name, ownerID string) *store.Project {
	t.Helper()
	now := time.Now().UTC()
	p := &store.Project{
		ID:        tid("ep-proj-" + slug),
		Name:      name,
		Slug:      slug,
		OwnerID:   ownerID,
		CreatedBy: ownerID,
		Created:   now,
		Updated:   now,
	}
	require.NoError(t, s.CreateProject(context.Background(), p))
	srv.seedProjectCreatorMembership(context.Background(), p)
	return p
}

func epBindProjectRole(t *testing.T, s store.Store, projectID, roleID, principalType, principalID string, notBefore, expiresAt *time.Time) *store.RoleBinding {
	t.Helper()
	rb := &store.RoleBinding{
		ID:               tid("ep-prb-" + projectID + "-" + roleID + "-" + principalType + "-" + principalID),
		RoleDefinitionID: roleID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        notBefore,
		ExpiresAt:        expiresAt,
		CreatedBy:        "system",
	}
	_, err := s.CreateRoleBinding(context.Background(), rb)
	require.NoError(t, err)
	return rb
}

func TestAdminEffectivePermissions_CallerGates(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	epSetExperiment(t, srv, true)
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

		recProject := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, url+"&projectId="+tid("some-missing-proj"), nil)
		assert.Equal(t, http.StatusNotFound, recProject.Code)
		assert.Contains(t, recProject.Body.String(), "Project not found")

		recNotFound := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/effective-permissions?principalType=user&principalId="+tid("missing-user"), nil)
		assert.Equal(t, http.StatusNotFound, recNotFound.Code)
		assert.Contains(t, recNotFound.Body.String(), "User not found")
	})
}

func TestAdminEffectivePermissions_ProvenanceDirectAndGroup(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	epSetExperiment(t, srv, true)
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
	assert.True(t, rbRead.AppliesInProjects, "role_binding.read applies in projects")
	require.Len(t, rbRead.Grants, 2, "both direct and group-derived grants must be listed")

	grantByBinding := map[string]EffectivePermissionGrant{}
	for _, g := range rbRead.Grants {
		grantByBinding[g.BindingID] = g
	}

	// role.read comes from all 3 sources: direct-ops, eng-auditor, and hub-member.
	roleRead, ok := byPerm["role.read"]
	require.True(t, ok, "role.read should be in effective permissions")
	assert.False(t, roleRead.AppliesInProjects, "role.read is hub-only")
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
	epSetExperiment(t, srv, true)
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
	epSetExperiment(t, srv, true)
	ctx := context.Background()

	// Super-admin caller (holds role_binding.read + access_constraint.read).
	superAdmin := epCreateUser(t, s, "rm-super", store.UserStatusActive)
	superAdmin.Role = store.UserRoleAdmin
	require.NoError(t, s.UpdateUser(ctx, superAdmin))
	superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, superAdmin.ID, nil, nil)

	// Custom caller with role_binding.read + project.read, but NOT access_constraint.read.
	rbNoACRole := epCreateCustomSystemRole(t, s, "rb-no-ac-reader", []string{"role_binding.read", "project.read"})
	rbNoACCaller := epCreateUser(t, s, "rm-rb-no-ac", store.UserStatusActive)
	epBindSystemRole(t, s, rbNoACRole.ID, store.RoleBindingPrincipalUser, rbNoACCaller.ID, nil, nil)

	// Target with project.read and agent.delete at system scope.
	target := epCreateUser(t, s, "constrained-target", store.UserStatusActive)
	role := epCreateCustomSystemRole(t, s, "ops-plus-delete", []string{"project.read", "agent.delete"})
	epBindSystemRole(t, s, role.ID, store.RoleBindingPrincipalUser, target.ID, nil, nil)

	// Create a project so we can also verify mixed system + project constraint deduplication.
	projOwner := epCreateUser(t, s, "rm-proj-owner", store.UserStatusActive)
	proj := epCreateProject(t, srv, s, "rm-proj", "Removals Project", projOwner.ID)

	pType := store.RoleBindingPrincipalUser
	sysCapA, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "contractors-cap-a",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &target.ID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"project.read"},
		Purpose:              "cap agent.delete (system A)",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)

	sysCapB, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "contractors-cap-b",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &target.ID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"project.read"},
		Purpose:              "cap agent.delete (system B)",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)

	projCapA, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "proj-cap-a",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &target.ID,
		ScopeType:            store.RoleScopeProject,
		ScopeID:              proj.ID,
		MaximumPermissions:   []string{"project.read"},
		Purpose:              "cap agent.delete (project A)",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)

	projCapB, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "proj-cap-b",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &target.ID,
		ScopeType:            store.RoleScopeProject,
		ScopeID:              proj.ID,
		MaximumPermissions:   []string{"project.read"},
		Purpose:              "cap agent.delete (project B)",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)

	// Direct resolver check: populates unredacted boundary metadata for both system constraints.
	resolved, err := srv.authzService.ResolveEffectivePermissions(ctx,
		PrincipalContext{Kind: PrincipalKindUser, ID: target.ID}, EffectiveScope{})
	require.NoError(t, err)
	require.Len(t, resolved.Permissions, 1)
	assert.Equal(t, "project.read", resolved.Permissions[0].ID)
	require.Len(t, resolved.Removed, 1)
	assert.Equal(t, "agent.delete", resolved.Removed[0].ID)
	require.Len(t, resolved.Removed[0].RemovedBy, 2)
	assert.Equal(t, "contractors-cap-a", resolved.Removed[0].RemovedBy[0].Name)
	assert.Equal(t, "contractors-cap-b", resolved.Removed[0].RemovedBy[1].Name)

	sysURL := "/api/v1/admin/effective-permissions?principalType=user&principalId=" + target.ID
	projURL := sysURL + "&projectId=" + proj.ID

	t.Run("caller without access_constraint.read gets redacted and deduplicated system boundaries", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, rbNoACCaller, http.MethodGet, sysURL, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		bodyStr := rec.Body.String()
		assert.NotContains(t, bodyStr, "contractors-cap-a", "redacted response must not leak constraint names")
		assert.NotContains(t, bodyStr, "contractors-cap-b", "redacted response must not leak constraint names")
		assert.NotContains(t, bodyStr, sysCapA.ID, "redacted response must not leak constraint IDs")
		assert.NotContains(t, bodyStr, sysCapB.ID, "redacted response must not leak constraint IDs")

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Permissions, 1)
		assert.Equal(t, "project.read", resp.Permissions[0].ID)
		require.Len(t, resp.Removed, 1)
		assert.Equal(t, "agent.delete", resp.Removed[0].ID)
		assert.Equal(t, "agent", resp.Removed[0].Resource)
		assert.Equal(t, "delete", resp.Removed[0].Action)
		assert.NotEmpty(t, resp.Removed[0].Description)
		assert.True(t, resp.Removed[0].AppliesInProjects, "agent.delete applies in projects")
		require.Len(t, resp.Removed[0].Grants, 1)
		assert.Equal(t, "ops-plus-delete", resp.Removed[0].Grants[0].Role.Name)

		// Two system constraints must collapse to a single redacted entry.
		require.Len(t, resp.Removed[0].RemovedBy, 1, "identical redacted boundaries must be deduplicated")
		b := resp.Removed[0].RemovedBy[0]
		assert.True(t, b.Redacted)
		assert.Empty(t, b.ID)
		assert.Empty(t, b.Name)
		assert.Empty(t, b.ScopeID)
		assert.Equal(t, ScopeTypeSystem, b.ScopeType)
	})

	t.Run("caller without access_constraint.read at project scope deduplicates per scopeType and clears scopeId", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, rbNoACCaller, http.MethodGet, projURL, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		bodyStr := rec.Body.String()
		assert.NotContains(t, bodyStr, "contractors-cap-a")
		assert.NotContains(t, bodyStr, "proj-cap-a")
		assert.NotContains(t, bodyStr, projCapA.ID)
		assert.NotContains(t, bodyStr, projCapB.ID)

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Removed, 1)
		assert.Equal(t, "agent.delete", resp.Removed[0].ID)

		// 2 project constraints + 2 system constraints -> 2 redacted entries preserving scopeType distinction.
		require.Len(t, resp.Removed[0].RemovedBy, 2, "redacted deduplication must preserve distinct scopeTypes")
		assert.Equal(t, EffectivePermissionBoundaryRef{
			ID:        "",
			Name:      "",
			ScopeType: ScopeTypeProject,
			ScopeID:   "",
			Redacted:  true,
		}, resp.Removed[0].RemovedBy[0])
		assert.Equal(t, EffectivePermissionBoundaryRef{
			ID:        "",
			Name:      "",
			ScopeType: ScopeTypeSystem,
			ScopeID:   "",
			Redacted:  true,
		}, resp.Removed[0].RemovedBy[1])
	})

	t.Run("hub-admin sees revealed constraint names, IDs, and scopeIds", func(t *testing.T) {
		recSys := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, sysURL, nil)
		require.Equal(t, http.StatusOK, recSys.Code, recSys.Body.String())

		var respSys EffectivePermissionSet
		require.NoError(t, json.Unmarshal(recSys.Body.Bytes(), &respSys))
		require.Len(t, respSys.Removed, 1)
		require.Len(t, respSys.Removed[0].RemovedBy, 2)
		assert.Equal(t, EffectivePermissionBoundaryRef{
			ID:        sysCapA.ID,
			Name:      "contractors-cap-a",
			ScopeType: ScopeTypeSystem,
			ScopeID:   "",
			Redacted:  false,
		}, respSys.Removed[0].RemovedBy[0])
		assert.Equal(t, EffectivePermissionBoundaryRef{
			ID:        sysCapB.ID,
			Name:      "contractors-cap-b",
			ScopeType: ScopeTypeSystem,
			ScopeID:   "",
			Redacted:  false,
		}, respSys.Removed[0].RemovedBy[1])

		recProj := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, projURL, nil)
		require.Equal(t, http.StatusOK, recProj.Code, recProj.Body.String())

		var respProj EffectivePermissionSet
		require.NoError(t, json.Unmarshal(recProj.Body.Bytes(), &respProj))
		require.Len(t, respProj.Removed, 1)
		require.Len(t, respProj.Removed[0].RemovedBy, 4)
		byName := make(map[string]EffectivePermissionBoundaryRef, 4)
		for _, b := range respProj.Removed[0].RemovedBy {
			byName[b.Name] = b
		}
		assert.Equal(t, EffectivePermissionBoundaryRef{
			ID:        projCapA.ID,
			Name:      "proj-cap-a",
			ScopeType: ScopeTypeProject,
			ScopeID:   proj.ID,
			Redacted:  false,
		}, byName["proj-cap-a"])
		assert.Equal(t, EffectivePermissionBoundaryRef{
			ID:        projCapB.ID,
			Name:      "proj-cap-b",
			ScopeType: ScopeTypeProject,
			ScopeID:   proj.ID,
			Redacted:  false,
		}, byName["proj-cap-b"])
	})

	t.Run("super-admin sees revealed constraint names and IDs", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, superAdmin, http.MethodGet, sysURL, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Removed, 1)
		require.Len(t, resp.Removed[0].RemovedBy, 2)
		assert.False(t, resp.Removed[0].RemovedBy[0].Redacted)
		assert.Equal(t, "contractors-cap-a", resp.Removed[0].RemovedBy[0].Name)
		assert.Equal(t, sysCapA.ID, resp.Removed[0].RemovedBy[0].ID)
		assert.False(t, resp.Removed[0].RemovedBy[1].Redacted)
		assert.Equal(t, "contractors-cap-b", resp.Removed[0].RemovedBy[1].Name)
		assert.Equal(t, sysCapB.ID, resp.Removed[0].RemovedBy[1].ID)
	})
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

func (c *epCountingStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	c.bump()
	return c.Store.GetProject(ctx, id)
}

func TestAdminEffectivePermissions_NoAuditNoiseAndFixedQueryCount(t *testing.T) {
	rawStore, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	counting := &epCountingStore{Store: rawStore}
	srv, s := testServerOnMigratedStore(t, counting)
	epSetExperiment(t, srv, true)
	ctx := context.Background()

	// Create hub-admin caller and project P
	hubAdmin := epCreateUser(t, s, "count-caller", store.UserStatusActive)
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, hubAdminRole.ID, store.RoleBindingPrincipalUser, hubAdmin.ID, nil, nil)

	projOwner := epCreateUser(t, s, "count-proj-owner", store.UserStatusActive)
	proj := epCreateProject(t, srv, s, "count-proj", "Count Project", projOwner.ID)
	projMemberRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	projOwnerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Target A: 1 group, small system role + 1 group project role
	targetSmall := epCreateUser(t, s, "count-small", store.UserStatusActive)
	gSmall := &store.Group{ID: tid("ep-cnt-g-small"), Name: "Small", Slug: "small", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, gSmall))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: gSmall.ID, MemberType: store.GroupMemberTypeUser, MemberID: targetSmall.ID, Role: store.GroupMemberRoleMember,
	}))
	smallRole := epCreateCustomSystemRole(t, s, "small-role", []string{"role.read"})
	epBindSystemRole(t, s, smallRole.ID, store.RoleBindingPrincipalGroup, gSmall.ID, nil, nil)
	smallProjRole := epCreateCustomProjectRole(t, s, "small-proj-role", []string{"project.read"})
	epBindProjectRole(t, s, proj.ID, smallProjRole.ID, store.RoleBindingPrincipalGroup, gSmall.ID, nil, nil)

	// Target B: 15 groups + super-admin (~150 permissions) + project-owner + 15 group project bindings
	targetLarge := epCreateUser(t, s, "count-large", store.UserStatusActive)
	superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, targetLarge.ID, nil, nil)
	epBindProjectRole(t, s, proj.ID, projOwnerRole.ID, store.RoleBindingPrincipalUser, targetLarge.ID, nil, nil)
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
		epBindProjectRole(t, s, proj.ID, projMemberRole.ID, store.RoleBindingPrincipalGroup, g.ID, nil, nil)
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

	// Measure resolver store reads at system scope: Small vs Large
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

	assert.Equal(t, smallReads, largeReads, "system store-call count must be independent of permission and group count")
	assert.LessOrEqual(t, largeReads, int64(6), "system resolver should use at most 6 batch/point store reads")

	// Measure resolver store reads at project scope: Small vs Large
	counting.reads.Store(0)
	counting.counting.Store(true)
	resSmallProj, err := srv.authzService.ResolveEffectivePermissions(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: targetSmall.ID}, EffectiveScope{ProjectID: proj.ID})
	counting.counting.Store(false)
	require.NoError(t, err)
	smallProjReads := counting.reads.Load()
	require.Len(t, resSmallProj.Permissions, 1)

	counting.reads.Store(0)
	counting.counting.Store(true)
	resLargeProj, err := srv.authzService.ResolveEffectivePermissions(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: targetLarge.ID}, EffectiveScope{ProjectID: proj.ID})
	counting.counting.Store(false)
	require.NoError(t, err)
	largeProjReads := counting.reads.Load()
	assert.Greater(t, len(resLargeProj.Permissions), 50)

	assert.Equal(t, smallProjReads, largeProjReads, "project store-call count must be independent of permission and group count")
	assert.LessOrEqual(t, largeProjReads, int64(7), "project resolver should use at most 7 batch/point store reads (6 + GetProject)")
	assert.Empty(t, emitter.records, "project-scope ResolveEffectivePermissions must emit zero decision-audit records")

	// HTTP endpoint check (system scope): only the 1 caller route-guard audit record, zero target permission audit records.
	emitter.records = nil
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
		"/api/v1/admin/effective-permissions?principalType=user&principalId="+targetLarge.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, emitter.records, 1, "system HTTP call should emit only the single route-guard Decide record")
	assert.Equal(t, "role_binding.read", emitter.records[0].PermissionID)
	assert.Equal(t, "admin.effectivePermissions", emitter.records[0].Route)
	assert.Equal(t, hubAdmin.ID, emitter.records[0].PrincipalID)

	// HTTP endpoint check (project scope): only caller route-guard + project.read visibility Decide records, zero target permission audit records.
	emitter.records = nil
	recProj := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
		"/api/v1/admin/effective-permissions?principalType=user&principalId="+targetLarge.ID+"&projectId="+proj.ID, nil)
	require.Equal(t, http.StatusOK, recProj.Code, recProj.Body.String())
	require.Len(t, emitter.records, 2, "project HTTP call should emit only caller route-guard + project.read visibility Decide records")
	assert.Equal(t, "role_binding.read", emitter.records[0].PermissionID)
	assert.Equal(t, hubAdmin.ID, emitter.records[0].PrincipalID)
	assert.Equal(t, "project.read", emitter.records[1].PermissionID)
	assert.Equal(t, hubAdmin.ID, emitter.records[1].PrincipalID)
}

func TestAdminEffectivePermissions_ProjectScope(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	epSetExperiment(t, srv, true)
	ctx := context.Background()

	projOwner := epCreateUser(t, s, "proj-owner", store.UserStatusActive)
	ensureHubMembership(ctx, s, projOwner.ID)
	proj := epCreateProject(t, srv, s, "alpha-proj", "Alpha Project", projOwner.ID)
	secretProj := epCreateProject(t, srv, s, "secret-proj", "Secret Project", projOwner.ID)

	t.Run("nonexistent vs invisible project return identical 404 Project not found", func(t *testing.T) {
		// Put a project-scoped AccessConstraint on secretProj removing project.read from hubAdmin.
		pType := store.RoleBindingPrincipalUser
		_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
			Name:                 "hide-secret-proj-from-hub-admin",
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: &pType,
			SubjectPrincipalID:   &hubAdmin.ID,
			ScopeType:            store.RoleScopeProject,
			ScopeID:              secretProj.ID,
			MaximumPermissions:   []string{"agent.read"}, // omits project.read
			Purpose:              "test caller project visibility 404",
			CreatedBy:            projOwner.ID,
		})
		require.NoError(t, err)

		// Also create a custom-role caller holding role_binding.read at hub scope without project.read.
		rbOnlyRole := epCreateCustomSystemRole(t, s, "rb-only-caller", []string{"role_binding.read"})
		rbOnlyCaller := epCreateUser(t, s, "rb-only-user", store.UserStatusActive)
		epBindSystemRole(t, s, rbOnlyRole.ID, store.RoleBindingPrincipalUser, rbOnlyCaller.ID, nil, nil)

		missingID := tid("ep-nonexistent-project")
		recMissing := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+member.ID+"&projectId="+missingID, nil)
		recConstrained := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+member.ID+"&projectId="+secretProj.ID, nil)
		recNoProjRead := doRequestAsUser(t, srv, rbOnlyCaller, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+member.ID+"&projectId="+proj.ID, nil)

		assert.Equal(t, http.StatusNotFound, recMissing.Code)
		assert.Equal(t, http.StatusNotFound, recConstrained.Code)
		assert.Equal(t, http.StatusNotFound, recNoProjRead.Code)
		assert.Contains(t, recMissing.Body.String(), "Project not found")
		assert.Equal(t, recMissing.Body.String(), recConstrained.Body.String(),
			"nonexistent and constraint-hidden projects must return an identical 404 body")
		assert.Equal(t, recMissing.Body.String(), recNoProjRead.Body.String(),
			"nonexistent and role-invisible projects must return an identical 404 body")
	})

	t.Run("non-member hub-member in P lacks catalog and hub-only permissions", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+member.ID+"&projectId="+proj.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, ScopeTypeProject, resp.Scope.Type)
		assert.Equal(t, proj.ID, resp.Scope.ID)
		assert.Equal(t, "Alpha Project", resp.Scope.Name)
		assert.Equal(t, "alpha-proj", resp.Scope.Slug)

		gotIDs := make(map[string]bool, len(resp.Permissions))
		for _, p := range resp.Permissions {
			gotIDs[p.ID] = true
		}
		for _, forbidden := range []string{
			"skill.read", "skill.list",
			"template.read", "template.list",
			"harness_config.read", "harness_config.list",
			"user.read", "project.create", "project.list", "project.read", "role.read",
		} {
			assert.False(t, gotIDs[forbidden], "non-member hub-member must not hold %s in project P", forbidden)
		}
		// Only hub-member permissions that genuinely apply to project-parented
		// resources in Decide (group.read, gcp_service_account.read) may appear.
		for _, p := range resp.Permissions {
			applies, reviewed := permissions.AppliesToExistingProjectTarget(p.ID)
			assert.True(t, reviewed && applies, "unexpected non-project permission %s", p.ID)
		}
	})

	t.Run("hub-admin in P shows system-scope grants on project-applicable permissions only", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+hubAdmin.ID+"&projectId="+proj.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, ScopeTypeProject, resp.Scope.Type)
		assert.Equal(t, proj.ID, resp.Scope.ID)
		assert.Equal(t, "Alpha Project", resp.Scope.Name)
		assert.Equal(t, "alpha-proj", resp.Scope.Slug)
		assert.NotEmpty(t, resp.Permissions)

		byPerm := make(map[string]EffectivePermissionEntry, len(resp.Permissions))
		for _, p := range resp.Permissions {
			byPerm[p.ID] = p
			applies, reviewed := permissions.AppliesToExistingProjectTarget(p.ID)
			assert.True(t, reviewed && applies, "project response included non-project permission %s", p.ID)
		}

		projRead, ok := byPerm["project.read"]
		require.True(t, ok, "hub-admin must hold project.read in P")
		require.NotEmpty(t, projRead.Grants)
		assert.Equal(t, ScopeTypeSystem, projRead.Grants[0].Scope.Type)
		assert.Empty(t, projRead.Grants[0].Scope.ID)
		assert.Equal(t, store.SystemRoleHubAdmin, projRead.Grants[0].Role.Name)
		assert.True(t, projRead.Grants[0].Role.System)

		// Hub-only permissions held by hub-admin at system scope must be excluded at project scope.
		for _, hubOnly := range []string{"user.read", "project.create", "project.list", "role.read"} {
			_, present := byPerm[hubOnly]
			assert.False(t, present, "hub-only permission %s must be excluded at project scope", hubOnly)
		}
	})

	t.Run("hub-admin plus group project-member shows both grants on project.read with via.groupName", func(t *testing.T) {
		dualTarget := epCreateUser(t, s, "dual-hub-and-proj", store.UserStatusActive)
		ensureHubMembership(ctx, s, dualTarget.ID)

		hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		sysBinding := epBindSystemRole(t, s, hubAdminRole.ID, store.RoleBindingPrincipalUser, dualTarget.ID, nil, nil)

		projGroup := &store.Group{
			ID:        tid("ep-proj-eng-group"),
			Name:      "Project Eng",
			Slug:      "project-eng",
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, projGroup))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    projGroup.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   dualTarget.ID,
			Role:       store.GroupMemberRoleMember,
		}))

		projMemberRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		projBinding := epBindProjectRole(t, s, proj.ID, projMemberRole.ID, store.RoleBindingPrincipalGroup, projGroup.ID, nil, nil)

		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+dualTarget.ID+"&projectId="+proj.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		var projRead *EffectivePermissionEntry
		for i := range resp.Permissions {
			if resp.Permissions[i].ID == "project.read" {
				projRead = &resp.Permissions[i]
				break
			}
		}
		require.NotNil(t, projRead, "project.read must be present")
		require.Len(t, projRead.Grants, 2, "must list both project-scope and system-scope grants")

		// §3.1: grants sorted project-scope first, then system-scope.
		g0, g1 := projRead.Grants[0], projRead.Grants[1]
		assert.Equal(t, projBinding.ID, g0.BindingID)
		assert.Equal(t, ScopeTypeProject, g0.Scope.Type)
		assert.Equal(t, proj.ID, g0.Scope.ID)
		assert.Equal(t, store.ProjectRoleMember, g0.Role.Name)
		assert.True(t, g0.Role.System)
		require.NotNil(t, g0.Via)
		assert.Equal(t, projGroup.ID, g0.Via.GroupID)
		assert.Equal(t, "Project Eng", g0.Via.GroupName)
		assert.Equal(t, "project-eng", g0.Via.GroupSlug)

		assert.Equal(t, sysBinding.ID, g1.BindingID)
		assert.Equal(t, ScopeTypeSystem, g1.Scope.Type)
		assert.Empty(t, g1.Scope.ID)
		assert.Equal(t, store.SystemRoleHubAdmin, g1.Role.Name)
		assert.True(t, g1.Role.System)
		assert.Nil(t, g1.Via)
	})

	t.Run("suspended user at project scope preserves project scope metadata", func(t *testing.T) {
		suspended := epCreateUser(t, s, "proj-suspended", store.UserStatusSuspended)
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet,
			"/api/v1/admin/effective-permissions?principalType=user&principalId="+suspended.ID+"&projectId="+proj.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp EffectivePermissionSet
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, ScopeTypeProject, resp.Scope.Type)
		assert.Equal(t, proj.ID, resp.Scope.ID)
		assert.Equal(t, "Alpha Project", resp.Scope.Name)
		assert.Equal(t, "alpha-proj", resp.Scope.Slug)
		require.NotNil(t, resp.StatusGate)
		assert.Equal(t, store.UserStatusSuspended, resp.StatusGate.Reason)
		assert.Empty(t, resp.Permissions)
		assert.Empty(t, resp.Removed)
	})
}

func TestResolveEffectivePermissions_ParityMatrixWithDecide(t *testing.T) {
	srv, s, hubAdmin, nonMemberHubMember := setupScopedAdminTest(t)
	ctx := context.Background()

	// Enable caged test_super_admin tier so the matrix exercises live test-admin caging.
	setAuthzTestAdminTier(srv.authzService, func(kind string) bool {
		return kind == store.UserKindTestSuperAdmin
	}, nil)
	t.Cleanup(func() {
		setAuthzTestAdminTier(srv.authzService, nil, nil)
	})

	projOwnerUser := epCreateUser(t, s, "matrix-proj-owner", store.UserStatusActive)
	ensureHubMembership(ctx, s, projOwnerUser.ID)
	projP := epCreateProject(t, srv, s, "matrix-proj-p", "Matrix Project P", projOwnerUser.ID)
	otherProj := epCreateProject(t, srv, s, "matrix-proj-other", "Matrix Other Project", projOwnerUser.ID)

	superRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	projMemberRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	projAdminRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	projOwnerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Give nonMemberHubMember owner access in otherProj to verify cross-project isolation in projP.
	epBindProjectRole(t, s, otherProj.ID, projOwnerRole.ID, store.RoleBindingPrincipalUser, nonMemberHubMember.ID, nil, nil)

	// 1. super-admin
	superAdmin := epCreateUser(t, s, "matrix-super", store.UserStatusActive)
	superAdmin.Role = store.UserRoleAdmin
	require.NoError(t, s.UpdateUser(ctx, superAdmin))
	epBindSystemRole(t, s, superRole.ID, store.RoleBindingPrincipalUser, superAdmin.ID, nil, nil)

	// 2. project-member direct in P
	projMemberDirect := epCreateUser(t, s, "matrix-pm-direct", store.UserStatusActive)
	ensureHubMembership(ctx, s, projMemberDirect.ID)
	epBindProjectRole(t, s, projP.ID, projMemberRole.ID, store.RoleBindingPrincipalUser, projMemberDirect.ID, nil, nil)

	// 3. project-member via nested group in P
	projMemberGroup := epCreateUser(t, s, "matrix-pm-group", store.UserStatusActive)
	ensureHubMembership(ctx, s, projMemberGroup.ID)
	parentGrp := &store.Group{ID: tid("ep-mat-grp-parent"), Name: "Matrix Parent", Slug: "matrix-parent", GroupType: store.GroupTypeExplicit}
	childGrp := &store.Group{ID: tid("ep-mat-grp-child"), Name: "Matrix Child", Slug: "matrix-child", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, parentGrp))
	require.NoError(t, s.CreateGroup(ctx, childGrp))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: parentGrp.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childGrp.ID, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: childGrp.ID, MemberType: store.GroupMemberTypeUser, MemberID: projMemberGroup.ID, Role: store.GroupMemberRoleMember,
	}))
	epBindProjectRole(t, s, projP.ID, projMemberRole.ID, store.RoleBindingPrincipalGroup, parentGrp.ID, nil, nil)

	// 4. hub-admin + group project-member in P
	hubAdminPlusGroupPM := epCreateUser(t, s, "matrix-ha-plus-gpm", store.UserStatusActive)
	ensureHubMembership(ctx, s, hubAdminPlusGroupPM.ID)
	epBindSystemRole(t, s, hubAdminRole.ID, store.RoleBindingPrincipalUser, hubAdminPlusGroupPM.ID, nil, nil)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: childGrp.ID, MemberType: store.GroupMemberTypeUser, MemberID: hubAdminPlusGroupPM.ID, Role: store.GroupMemberRoleMember,
	}))

	// 5. Expired/future bindings + system & project access constraints
	timingAndConstrained := epCreateUser(t, s, "matrix-timing-constrained", store.UserStatusActive)
	ensureHubMembership(ctx, s, timingAndConstrained.ID)
	past := time.Now().UTC().Add(-2 * time.Hour)
	future := time.Now().UTC().Add(2 * time.Hour)
	expSysRole := epCreateCustomSystemRole(t, s, "mat-exp-sys", []string{"quota.read", "broker.create"})
	futSysRole := epCreateCustomSystemRole(t, s, "mat-fut-sys", []string{"role.create"})
	actSysRole := epCreateCustomSystemRole(t, s, "mat-act-sys", []string{"role.read", "broker.read", "agent.delete"})
	expProjRole := epCreateCustomProjectRole(t, s, "mat-exp-proj", []string{"project.delete"})
	futProjRole := epCreateCustomProjectRole(t, s, "mat-fut-proj", []string{"project.manage_visitors"})
	epBindSystemRole(t, s, expSysRole.ID, store.RoleBindingPrincipalUser, timingAndConstrained.ID, nil, &past)
	epBindSystemRole(t, s, futSysRole.ID, store.RoleBindingPrincipalUser, timingAndConstrained.ID, &future, nil)
	epBindSystemRole(t, s, actSysRole.ID, store.RoleBindingPrincipalUser, timingAndConstrained.ID, nil, nil)
	epBindProjectRole(t, s, projP.ID, expProjRole.ID, store.RoleBindingPrincipalUser, timingAndConstrained.ID, nil, &past)
	epBindProjectRole(t, s, projP.ID, futProjRole.ID, store.RoleBindingPrincipalUser, timingAndConstrained.ID, &future, nil)
	epBindProjectRole(t, s, projP.ID, projAdminRole.ID, store.RoleBindingPrincipalUser, timingAndConstrained.ID, nil, nil)

	pType := store.RoleBindingPrincipalUser
	// System constraint allows role.read + project/agent basics, strips broker.read at hub and agent.delete everywhere.
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "mat-sys-cap",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &timingAndConstrained.ID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"role.read", "user.read", "project.read", "project.update", "agent.read", "agent.create", "skill.read"},
		Purpose:              "matrix system constraint",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)
	// Project constraint on P further caps timingAndConstrained to project.read + agent.read + skill.read (strips project.update & agent.create in P).
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "mat-proj-cap",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &timingAndConstrained.ID,
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projP.ID,
		MaximumPermissions:   []string{"role.read", "user.read", "project.read", "agent.read", "skill.read"},
		Purpose:              "matrix project constraint",
		CreatedBy:            hubAdmin.ID,
	})
	require.NoError(t, err)

	// 6. Caged test_super_admin (holds super-admin + direct project-member in P)
	adminExp := time.Now().UTC().Add(30 * time.Minute)
	purpose := "parity matrix test"
	issuance := tid("ep-mat-test-super-issuance")
	cagedTestAdmin := &store.User{
		ID:         tid("ep-mat-test-super"),
		Email:      "matsuper@" + store.TestAdminEmailDomain(store.UserKindTestSuperAdmin),
		Role:       store.TestAdminRole(store.UserKindTestSuperAdmin),
		Status:     store.UserStatusActive,
		Kind:       store.UserKindTestSuperAdmin,
		ExpiresAt:  &adminExp,
		IssuedBy:   &hubAdmin.ID,
		Purpose:    &purpose,
		IssuanceID: &issuance,
		Created:    time.Now().UTC(),
	}
	_, err = s.CreateTestAdminUser(ctx, cagedTestAdmin, superRole.ID)
	require.NoError(t, err)
	epBindProjectRole(t, s, projP.ID, projMemberRole.ID, store.RoleBindingPrincipalUser, cagedTestAdmin.ID, nil, nil)

	// 7. test_fixture target (with clamped extra system role via group + direct project-member in P)
	fxExp := time.Now().UTC().Add(30 * time.Minute)
	testFixtureUser := &store.User{
		ID:        tid("ep-mat-test-fixture"),
		Email:     "matfx@" + store.TestFixtureEmailDomain,
		Role:      store.UserRoleMember,
		Status:    store.UserStatusActive,
		Kind:      store.UserKindTestFixture,
		ExpiresAt: &fxExp,
		IssuedBy:  &hubAdmin.ID,
		Created:   time.Now().UTC(),
	}
	require.NoError(t, s.CreateTestFixtureUser(ctx, testFixtureUser))
	ensureHubMembership(ctx, s, testFixtureUser.ID)
	fxSysGrp := &store.Group{ID: tid("ep-mat-fx-grp"), Name: "FX Sys Grp", Slug: "fx-sys-grp", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, fxSysGrp))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: fxSysGrp.ID, MemberType: store.GroupMemberTypeUser, MemberID: testFixtureUser.ID, Role: store.GroupMemberRoleMember,
	}))
	fxExtraSysRole := epCreateCustomSystemRole(t, s, "mat-fx-extra", []string{"role.create", "quota.create"})
	epBindSystemRole(t, s, fxExtraSysRole.ID, store.RoleBindingPrincipalGroup, fxSysGrp.ID, nil, nil)
	epBindProjectRole(t, s, projP.ID, projMemberRole.ID, store.RoleBindingPrincipalUser, testFixtureUser.ID, nil, nil)

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

	projectResourceFor := func(p permissions.Permission, projectID string) Resource {
		if p.Resource == permissions.ResourceProject {
			return Resource{Type: permissions.ResourceProject, ID: projectID}
		}
		class := ContemplatedProjectClass(p.ID)
		return Resource{
			Type:       p.Resource,
			ID:         "proj-parity-target",
			ScopeKind:  class.ScopeKind,
			ParentType: "project",
			ParentID:   projectID,
		}
	}

	fixtures := []struct {
		name string
		user *store.User
	}{
		{"hub-admin", hubAdmin},
		{"hub-member-non-member-of-P", nonMemberHubMember},
		{"project-member-direct", projMemberDirect},
		{"project-member-via-group", projMemberGroup},
		{"hub-admin-plus-group-project-member", hubAdminPlusGroupPM},
		{"expired-future-and-constrained", timingAndConstrained},
		{"caged-test-admin", cagedTestAdmin},
		{"test-fixture-target", testFixtureUser},
		{"super-admin", superAdmin},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			sysSet, err := srv.authzService.ResolveEffectivePermissions(ctx,
				PrincipalContext{Kind: PrincipalKindUser, ID: fx.user.ID}, EffectiveScope{})
			require.NoError(t, err)

			projSet, err := srv.authzService.ResolveEffectivePermissions(ctx,
				PrincipalContext{Kind: PrincipalKindUser, ID: fx.user.ID}, EffectiveScope{ProjectID: projP.ID})
			require.NoError(t, err)

			resolvedSys := make(map[string]bool, len(sysSet.Permissions))
			for _, p := range sysSet.Permissions {
				resolvedSys[p.ID] = true
			}
			resolvedProj := make(map[string]bool, len(projSet.Permissions))
			for _, p := range projSet.Permissions {
				resolvedProj[p.ID] = true
			}

			ident := NewAuthenticatedUser(fx.user.ID, fx.user.Email, fx.user.DisplayName, fx.user.Role, "web")
			for _, perm := range permissions.Registry {
				if perm.IsReserved() {
					assert.False(t, resolvedSys[perm.ID], "system: reserved permission %s must never appear", perm.ID)
					assert.False(t, resolvedProj[perm.ID], "project: reserved permission %s must never appear", perm.ID)
					continue
				}

				// System-scope parity: resolver(system) ∋ p ⇔ Decide(hub resource for p)
				dSys := srv.authzService.Decide(ctx, AuthzRequest{
					Principal:  principalContextForIdentity(ident),
					Credential: credentialContextForIdentity(ident),
					Resource:   hubResourceFor(perm),
					Action:     Action(perm.Action),
					Permission: perm.ID,
				})
				if !(dSys.Allowed && dSys.BindingID == "") {
					assert.Equal(t, dSys.Allowed, resolvedSys[perm.ID],
						"system parity mismatch for %s on %s (Decide reason=%q binding=%q)",
						fx.name, perm.ID, dSys.Reason, dSys.BindingID)
				}

				// Project-scope parity: resolver(P) ∋ p ⇔ AppliesToExistingProjectTarget(p) ∧ Decide(resource of p's class in P)
				applies, reviewed := permissions.AppliesToExistingProjectTarget(perm.ID)
				if !reviewed || !applies {
					assert.False(t, resolvedProj[perm.ID],
						"project: non-applicable permission %s must not appear for %s", perm.ID, fx.name)
					continue
				}
				dProj := srv.authzService.Decide(ctx, AuthzRequest{
					Principal:  principalContextForIdentity(ident),
					Credential: credentialContextForIdentity(ident),
					Resource:   projectResourceFor(perm, projP.ID),
					Action:     Action(perm.Action),
					Permission: perm.ID,
				})
				if !(dProj.Allowed && dProj.BindingID == "") {
					assert.Equal(t, dProj.Allowed, resolvedProj[perm.ID],
						"project parity mismatch for %s on %s (Decide reason=%q binding=%q)",
						fx.name, perm.ID, dProj.Reason, dProj.BindingID)
				}
			}
		})
	}
}
