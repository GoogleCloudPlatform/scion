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

// Project members groups are system-managed: they cannot be the principal of
// a role binding (any scope, any route) or be nested as a child of another
// group. These tests cover the hub refusals (principal_ineligible with
// details.reason=project_members_group, or a validation error on the group
// API), the store backstop mapping, and that existing bindings can still be
// deleted.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// membersGroupGuardFixture is a project X whose creator was removed without
// an ownership transfer (so X's members group still lists the creator), and
// a second project Y owned by X's remaining owner.
type membersGroupGuardFixture struct {
	staleOwnerFixture
	// canonical is X's members group, marked with the canonical key.
	canonical *store.Group
	// legacy is a group of X marked only with the legacy key. The creator is
	// a member.
	legacy *store.Group
	y      *store.Project
}

func setupMembersGroupGuardFixture(t *testing.T) membersGroupGuardFixture {
	t.Helper()
	f := setupStaleOwnerFixture(t)
	ctx := context.Background()

	f.srv.createProjectMembersGroup(ctx, f.project)
	canonical := membersGroupFor(t, f.s, f.project)

	legacy := &store.Group{
		ID: tid("mgguard-legacy"), Name: "MG Guard Legacy Members", Slug: "mgguard-legacy-members",
		GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
		Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
	}
	require.NoError(t, f.s.CreateGroup(ctx, legacy))
	require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: legacy.ID, MemberType: store.GroupMemberTypeUser,
		MemberID: f.creator.ID, Role: store.GroupMemberRoleMember,
	}))

	y := &store.Project{
		ID: tid("mgguard-y"), Name: "MG Guard Y", Slug: "mgguard-y",
		OwnerID: f.coOwner.ID, CreatedBy: f.coOwner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.s.CreateProject(ctx, y))
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, y.ID, f.coOwner.ID))

	return membersGroupGuardFixture{staleOwnerFixture: f, canonical: canonical, legacy: legacy, y: y}
}

// markedGroups returns the canonical-key and legacy-key groups by name.
func (f membersGroupGuardFixture) markedGroups() map[string]*store.Group {
	return map[string]*store.Group{"canonical": f.canonical, "legacy": f.legacy}
}

func projectRoleDef(t *testing.T, s store.Store, name string) *store.RoleDefinition {
	t.Helper()
	rd, err := s.GetRoleDefinitionByName(context.Background(), name, store.RoleScopeProject)
	require.NoError(t, err)
	return rd
}

// requireMembersGroupRefusal asserts the hub refusal for a project members
// group principal: 400, the given code and details.reason.
func requireMembersGroupRefusal(t *testing.T, rec *httptest.ResponseRecorder, wantCode, groupID string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, details := errorBody(t, rec)
	assert.Equal(t, wantCode, code, rec.Body.String())
	require.NotNil(t, details, "details must be present: %s", rec.Body.String())
	assert.Equal(t, projectMembersGroupDetailReason, details["reason"], rec.Body.String())
	assert.Equal(t, groupID, details["groupId"], rec.Body.String())
}

// requireNoGroupBindings asserts the group is the principal of no binding.
func requireNoGroupBindings(t *testing.T, s store.Store, groupID string) {
	t.Helper()
	bindings, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalGroup, groupID)
	require.NoError(t, err)
	assert.Empty(t, bindings, "no binding may name the members group")
}

// requireCreatorHasNoAccessToY asserts the removed creator of X gained no
// access to Y.
func requireCreatorHasNoAccessToY(t *testing.T, f membersGroupGuardFixture) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, f.creator, http.MethodGet, "/api/v1/projects/"+f.y.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "creator must not see Y: %s", rec.Body.String())
	assert.Empty(t, f.srv.membershipService.projectEffectiveRole(context.Background(), f.creator.ID, f.y.ID))
}

// seedExistingMembersGroupBinding creates a group of X that already holds
// bindings on Y and then marks it as a project members group, returning the
// group and its bindings in roleDefs order.
//
// The store refuses a binding for an already-marked group, and the API
// refuses to add or change marker annotations, so the only way to model a
// binding created before this rule existed is to bind an unmarked group
// first and set the marker afterwards directly in the store.
func seedExistingMembersGroupBinding(t *testing.T, f membersGroupGuardFixture, name, markerKey string, roleDefs ...*store.RoleDefinition) (*store.Group, []*store.RoleBinding) {
	t.Helper()
	ctx := context.Background()
	g := &store.Group{
		ID: tid("mgguard-existing-" + name), Name: "MG Guard Existing " + name, Slug: "mgguard-existing-" + name,
		GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
	}
	require.NoError(t, f.s.CreateGroup(ctx, g))
	var bindings []*store.RoleBinding
	for _, rd := range roleDefs {
		rb, err := f.s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: g.ID,
			ScopeType: store.RoleScopeProject, ScopeID: f.y.ID, CreatedBy: "test-seed",
		})
		require.NoError(t, err)
		bindings = append(bindings, rb)
	}
	stored, err := f.s.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	stored.Annotations = map[string]string{markerKey: "true"}
	require.NoError(t, f.s.UpdateGroup(ctx, stored))
	marked, err := f.s.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "true", marked.Annotations[markerKey], "seeded group must carry the marker")
	return marked, bindings
}

func markerKeys() map[string]string {
	return map[string]string{
		"canonical": store.AnnotationProjectMembersGroup,
		"legacy":    store.LegacyAnnotationProjectMembersGroup,
	}
}

// POST /projects/{id}/members, by ID and by slug, both marker keys.
func TestMembersGroupPrincipalGuard_AddMemberRefused(t *testing.T) {
	for kind := range markerKeys() {
		for _, addr := range []string{"id", "slug"} {
			t.Run(kind+"/"+addr, func(t *testing.T) {
				f := setupMembersGroupGuardFixture(t)
				g := f.markedGroups()[kind]
				principal := g.ID
				if addr == "slug" {
					principal = g.Slug
				}
				adminRD := projectRoleDef(t, f.s, store.ProjectRoleAdmin)
				rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPost, "/api/v1/projects/"+f.y.ID+"/members",
					map[string]interface{}{"principalType": "group", "principalId": principal, "roleDefinitionId": adminRD.ID})
				requireMembersGroupRefusal(t, rec, ErrCodePrincipalIneligible, g.ID)
				requireNoGroupBindings(t, f.s, g.ID)
				requireCreatorHasNoAccessToY(t, f)
			})
		}
	}
}

// Binding a project's own members group into that project is refused too.
func TestMembersGroupPrincipalGuard_SelfProjectBindingRefused(t *testing.T) {
	for kind := range markerKeys() {
		t.Run(kind, func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			g := f.markedGroups()[kind]
			memberRD := projectRoleDef(t, f.s, store.ProjectRoleMember)
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/members",
				map[string]interface{}{"principalType": "group", "principalId": g.ID, "roleDefinitionId": memberRD.ID})
			requireMembersGroupRefusal(t, rec, ErrCodePrincipalIneligible, g.ID)
			requireNoGroupBindings(t, f.s, g.ID)
			assert.Empty(t, f.srv.membershipService.projectEffectiveRole(context.Background(), f.creator.ID, f.project.ID))
		})
	}
}

// PUT …/members/principals/group/{id} creating a binding.
func TestMembersGroupPrincipalGuard_SetMemberRolesRefused(t *testing.T) {
	for kind := range markerKeys() {
		t.Run(kind, func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			g := f.markedGroups()[kind]
			adminRD := projectRoleDef(t, f.s, store.ProjectRoleAdmin)
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPut, mmrPrincipalPath(f.y.ID, "group", g.ID),
				map[string]interface{}{"roleDefinitionIds": []string{adminRD.ID}})
			requireMembersGroupRefusal(t, rec, ErrCodePrincipalIneligible, g.ID)
			requireNoGroupBindings(t, f.s, g.ID)
			requireCreatorHasNoAccessToY(t, f)
		})
	}
}

// POST /admin/role-bindings: built-in project role, custom project role and
// system role.
func TestMembersGroupPrincipalGuard_AdminRoleBindingRefused(t *testing.T) {
	for kind := range markerKeys() {
		for _, route := range []string{"builtin-project", "custom-project", "system"} {
			t.Run(kind+"/"+route, func(t *testing.T) {
				f := setupMembersGroupGuardFixture(t)
				g := f.markedGroups()[kind]
				body := map[string]interface{}{"principalType": "group", "principalId": g.ID}
				switch route {
				case "builtin-project":
					body["roleDefinitionId"] = projectRoleDef(t, f.s, store.ProjectRoleAdmin).ID
					body["scopeType"] = store.RoleScopeProject
					body["scopeId"] = f.y.ID
				case "custom-project":
					body["roleDefinitionId"] = createCustomRoleDef(t, f.s, "mgguard-custom", []string{"project.read"}).ID
					body["scopeType"] = store.RoleScopeProject
					body["scopeId"] = f.y.ID
				case "system":
					rd, err := f.s.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubViewer, store.RoleScopeSystem)
					require.NoError(t, err)
					body["roleDefinitionId"] = rd.ID
					body["scopeType"] = store.RoleScopeSystem
				}
				rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/admin/role-bindings", body)
				requireMembersGroupRefusal(t, rec, ErrCodePrincipalIneligible, g.ID)
				requireNoGroupBindings(t, f.s, g.ID)
				requireCreatorHasNoAccessToY(t, f)
			})
		}
	}
}

// POST /admin/role-bindings with a built-in project role is authorized by the
// membership service. An actor with no role on the project gets the ordinary
// 403 for a members-group principal, not the members-group refusal, and the
// body does not name the group.
func TestMembersGroupPrincipalGuard_AdminRoleBindingUnauthorizedActor(t *testing.T) {
	for kind := range markerKeys() {
		t.Run(kind, func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			g := f.markedGroups()[kind]
			require.Empty(t, f.srv.membershipService.projectEffectiveRole(context.Background(), f.creator.ID, f.y.ID),
				"the actor must hold no role on Y")
			rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPost, "/api/v1/admin/role-bindings",
				map[string]interface{}{
					"principalType": "group", "principalId": g.Slug,
					"roleDefinitionId": projectRoleDef(t, f.s, store.ProjectRoleAdmin).ID,
					"scopeType":        store.RoleScopeProject, "scopeId": f.y.ID,
				})
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			_, details := errorBody(t, rec)
			assert.NotContains(t, details, "groupId", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), g.ID, "the body must not name the group")
			requireNoGroupBindings(t, f.s, g.ID)
		})
	}
}

// POST /groups/{G}/members nesting a members group, by ID and by slug.
func TestMembersGroupPrincipalGuard_NestedGroupRefused(t *testing.T) {
	for kind := range markerKeys() {
		for _, addr := range []string{"id", "slug"} {
			t.Run(kind+"/"+addr, func(t *testing.T) {
				f := setupMembersGroupGuardFixture(t)
				ctx := context.Background()
				g := f.markedGroups()[kind]

				// An ordinary group that holds project-admin on Y.
				parent := &store.Group{ID: tid("mgguard-parent"), Name: "MG Guard Parent", Slug: "mgguard-parent"}
				require.NoError(t, f.s.CreateGroup(ctx, parent))
				_, err := f.s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: projectRoleDef(t, f.s, store.ProjectRoleAdmin).ID,
					PrincipalType:    store.RoleBindingPrincipalGroup, PrincipalID: parent.ID,
					ScopeType: store.RoleScopeProject, ScopeID: f.y.ID, CreatedBy: "test-seed",
				})
				require.NoError(t, err)

				member := g.ID
				if addr == "slug" {
					member = g.Slug
				}
				rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/groups/"+parent.ID+"/members",
					map[string]interface{}{"memberType": "group", "memberId": member, "role": "member"})
				requireMembersGroupRefusal(t, rec, ErrCodeValidationError, g.ID)

				groups, err := f.s.GetEffectiveGroups(ctx, f.creator.ID)
				require.NoError(t, err)
				assert.NotContains(t, groups, parent.ID, "creator must not inherit the parent group")
				requireCreatorHasNoAccessToY(t, f)
			})
		}
	}
}

// An ordinary group with a ProjectID but no marker is still a valid
// principal and child group.
func TestMembersGroupPrincipalGuard_UnmarkedGroupAllowed(t *testing.T) {
	f := setupMembersGroupGuardFixture(t)
	ctx := context.Background()
	g := &store.Group{
		ID: tid("mgguard-plain"), Name: "MG Guard Plain", Slug: "mgguard-plain",
		GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
		Annotations: map[string]string{store.AnnotationProjectMembersGroup: "false"},
	}
	require.NoError(t, f.s.CreateGroup(ctx, g))

	memberRD := projectRoleDef(t, f.s, store.ProjectRoleMember)
	rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPost, "/api/v1/projects/"+f.y.ID+"/members",
		map[string]interface{}{"principalType": "group", "principalId": g.ID, "roleDefinitionId": memberRD.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	parent := &store.Group{ID: tid("mgguard-plain-parent"), Name: "MG Guard Plain Parent", Slug: "mgguard-plain-parent"}
	require.NoError(t, f.s.CreateGroup(ctx, parent))
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/groups/"+parent.ID+"/members",
		map[string]interface{}{"memberType": "group", "memberId": g.ID, "role": "member"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// A binding that predates the rule cannot have its role changed through
// PATCH (it re-creates the binding), but can be deleted.
func TestMembersGroupPrincipalGuard_ExistingBindingPatchRefused(t *testing.T) {
	for kind, key := range markerKeys() {
		t.Run(kind, func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			memberRD := projectRoleDef(t, f.s, store.ProjectRoleMember)
			adminRD := projectRoleDef(t, f.s, store.ProjectRoleAdmin)
			g, bindings := seedExistingMembersGroupBinding(t, f, "patch", key, memberRD)
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPatch,
				"/api/v1/projects/"+f.y.ID+"/members/"+bindings[0].ID,
				map[string]interface{}{"roleDefinitionId": adminRD.ID})
			requireMembersGroupRefusal(t, rec, ErrCodePrincipalIneligible, g.ID)
			kept, err := f.s.GetRoleBinding(context.Background(), bindings[0].ID)
			require.NoError(t, err, "the existing binding must be left in place")
			assert.Equal(t, memberRD.ID, kept.RoleDefinitionID)
		})
	}
}

func TestMembersGroupPrincipalGuard_ExistingBindingDeleteAllowed(t *testing.T) {
	for kind, key := range markerKeys() {
		t.Run(kind+"/by-binding", func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			g, bindings := seedExistingMembersGroupBinding(t, f, "delbinding", key, projectRoleDef(t, f.s, store.ProjectRoleMember))
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodDelete,
				"/api/v1/projects/"+f.y.ID+"/members/"+bindings[0].ID, nil)
			require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
			requireNoGroupBindings(t, f.s, g.ID)
		})
		t.Run(kind+"/by-principal", func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			g, _ := seedExistingMembersGroupBinding(t, f, "delprincipal", key,
				projectRoleDef(t, f.s, store.ProjectRoleMember),
				createCustomRoleDef(t, f.s, "mgguard-del-custom", []string{"project.read"}))
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodDelete, mmrPrincipalPath(f.y.ID, "group", g.ID), nil)
			require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code, rec.Body.String())
			requireNoGroupBindings(t, f.s, g.ID)
		})
	}
}

// PUT on a principal with an existing binding: an unchanged set is an
// idempotent success, adding a role is refused, removing a role succeeds.
func TestMembersGroupPrincipalGuard_ExistingBindingSetMemberRoles(t *testing.T) {
	for kind, key := range markerKeys() {
		t.Run(kind+"/unchanged", func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			memberRD := projectRoleDef(t, f.s, store.ProjectRoleMember)
			g, bindings := seedExistingMembersGroupBinding(t, f, "put-same", key, memberRD)
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPut, mmrPrincipalPath(f.y.ID, "group", g.ID),
				map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID}})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			_, err := f.s.GetRoleBinding(context.Background(), bindings[0].ID)
			require.NoError(t, err)
		})
		t.Run(kind+"/add-custom", func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			memberRD := projectRoleDef(t, f.s, store.ProjectRoleMember)
			customRD := createCustomRoleDef(t, f.s, "mgguard-put-custom", []string{"project.read"})
			g, _ := seedExistingMembersGroupBinding(t, f, "put-add", key, memberRD)
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPut, mmrPrincipalPath(f.y.ID, "group", g.ID),
				map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID, customRD.ID}})
			requireMembersGroupRefusal(t, rec, ErrCodePrincipalIneligible, g.ID)
			bindings, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalGroup, g.ID)
			require.NoError(t, err)
			assert.Len(t, bindings, 1, "no binding may be added")
		})
		t.Run(kind+"/remove-role", func(t *testing.T) {
			f := setupMembersGroupGuardFixture(t)
			memberRD := projectRoleDef(t, f.s, store.ProjectRoleMember)
			customRD := createCustomRoleDef(t, f.s, "mgguard-put-remove", []string{"project.read"})
			g, _ := seedExistingMembersGroupBinding(t, f, "put-remove", key, memberRD, customRD)
			rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodPut, mmrPrincipalPath(f.y.ID, "group", g.ID),
				map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID}})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			bindings, err := f.s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalGroup, g.ID)
			require.NoError(t, err)
			require.Len(t, bindings, 1)
			assert.Equal(t, memberRD.ID, bindings[0].RoleDefinitionID)
		})
	}
}

// The store refusal maps to 400, never 500.
func TestMembersGroupPrincipalGuard_StoreErrorMapping(t *testing.T) {
	wrapped := fmt.Errorf("create replacement binding: %w", store.ErrProjectMembersGroupPrincipal)

	rec := httptest.NewRecorder()
	writeErrorFromErr(rec, wrapped, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeInvalidRequest, code)

	d := storeMembersGroupPrincipalDecision(wrapped)
	require.NotNil(t, d)
	assert.Equal(t, http.StatusBadRequest, d.HTTPStatus)
	assert.Equal(t, ErrCodeInvalidRequest, d.DenialCode)
	assert.Nil(t, storeMembersGroupPrincipalDecision(store.ErrInvalidInput))
}
