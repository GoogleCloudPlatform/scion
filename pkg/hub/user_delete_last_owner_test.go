// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2598: deleting a user must not orphan a project, and
// a deleted user's role bindings must be removed with the user.

// requireLastOwnerDenial asserts a 409 last_owner response whose
// details.projects lists exactly the given project.
func requireLastOwnerDenial(t *testing.T, rec *httptest.ResponseRecorder, project *store.Project) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Projects []lastOwnerProjectRef `json:"projects"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeLastOwner, resp.Error.Code)
	assert.NotEmpty(t, resp.Error.Message)
	assert.Equal(t, []lastOwnerProjectRef{{ID: project.ID, Name: project.Name}}, resp.Error.Details.Projects)
}

// allBindingsFor returns every role binding (any scope) held by the user.
func allBindingsFor(t *testing.T, s store.Store, userID string) []*store.RoleBinding {
	t.Helper()
	got, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	return got
}

// grantSystemRole gives the user a system-scoped binding for the named
// system role directly in the store.
func grantSystemRole(t *testing.T, s store.Store, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func TestDeleteUser_SoleProjectOwnerDenied(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	requireSingleOwnerBinding(t, s, project.ID, alice.ID)
	before := allBindingsFor(t, s, alice.ID)
	require.NotEmpty(t, before)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
	requireLastOwnerDenial(t, rec, project)

	_, err := s.GetUser(ctx, alice.ID)
	require.NoError(t, err, "denied delete must keep the user")
	assert.Len(t, allBindingsFor(t, s, alice.ID), len(before), "denied delete must keep the bindings")
	requireSingleOwnerBinding(t, s, project.ID, alice.ID)
}

func TestDeleteUser_CoOwnerAllowedAndBindingsCascaded(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	addProjectOwner(t, srv, s, alice, bob, project.ID)
	grantSystemRole(t, s, alice.ID, store.SystemRoleHubViewer)
	grantSystemRole(t, s, alice.ID, store.SystemRoleGlobalCatalogAuthor)

	var scopes = map[string]bool{}
	for _, b := range allBindingsFor(t, s, alice.ID) {
		scopes[b.ScopeType] = true
	}
	require.True(t, scopes[store.RoleScopeProject] && scopes[store.RoleScopeSystem],
		"precondition: alice holds project and system/hub bindings")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err := s.GetUser(ctx, alice.ID)
	require.Error(t, err, "user should be deleted")
	assert.Empty(t, allBindingsFor(t, s, alice.ID), "deleted user's project, hub and system bindings must be removed")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

func TestDeleteUser_ExpiredOwnerBindingOnOwnerlessProjectDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	bob := &store.User{ID: tid("user-bob"), Email: "bob@test.com", DisplayName: "Bob",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, bob))
	project := &store.Project{ID: tid("project-expired"), Name: "Expired Owner Project", Slug: "expired-owner-project",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      bob.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		ExpiresAt:        &expired,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Deleting the expired binding would take the project to zero owner
	// bindings, which re-arms the startup backfill: deny.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+bob.ID, nil)
	requireLastOwnerDenial(t, rec, project)

	_, err = s.GetUser(ctx, bob.ID)
	require.NoError(t, err)
	assert.Len(t, projectBindingsFor(t, s, project.ID, bob.ID), 1)
}

// Regression lock for the interaction between ptone/scion#2598's two
// problems (from the investigator's TestRepro2598): the delete of a sole
// owner is denied, so the owner binding survives and a restart backfill does
// not re-grant the removed creator.
func TestDeleteUser_SoleOwnerThenRestartDoesNotRegrantCreator(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	transferAndRemoveCreator(t, srv, s, alice, bob, project)
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+bob.ID, nil)
	requireLastOwnerDenial(t, rec, project)
	_, err := s.GetUser(ctx, bob.ID)
	require.NoError(t, err)

	// Restart.
	require.NoError(t, BackfillRoleBindings(ctx, s))
	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"backfill must not re-grant the removed creator")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

// newInvitedUser creates a user in invited status, as the allow-list
// endpoints manage.
func newInvitedUser(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(id), Email: email, DisplayName: id,
		Role: store.UserRoleMember, Status: store.UserStatusInvited, Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func TestDeprecatedAllowListDelete_CascadesRoleBindings(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.ID,
		ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: alice.ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, allBindingsFor(t, s, carol.ID))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, err = s.GetUser(ctx, carol.ID)
	require.Error(t, err, "invited user should be deleted")
	assert.Empty(t, allBindingsFor(t, s, carol.ID), "invited user's bindings must be removed")
	requireSingleOwnerBinding(t, s, project.ID, alice.ID)
}

func TestDeprecatedAllowListDelete_SoleProjectOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")
	project := &store.Project{ID: tid("project-carol"), Name: "Carol Project", Slug: "carol-project",
		CreatedBy: carol.ID, OwnerID: carol.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	requireSingleOwnerBinding(t, s, project.ID, carol.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/"+carol.Email, nil)
	requireLastOwnerDenial(t, rec, project)

	_, err := s.GetUser(ctx, carol.ID)
	require.NoError(t, err, "denied delete must keep the invited user")
	requireSingleOwnerBinding(t, s, project.ID, carol.ID)
}
