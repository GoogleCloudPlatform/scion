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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedProjectCreatorMembership is the test fixture for a freshly created
// project: it grants the creator the project-owner binding (as the create,
// register and clone handlers do) and then ensures the members group.
// createProjectMembersGroup no longer grants the binding itself
// (ptone/scion#2554). Errors are ignored, matching the best-effort behaviour
// fixtures relied on before.
func (s *Server) seedProjectCreatorMembership(ctx context.Context, project *store.Project) {
	if project.CreatedBy != "" {
		_ = s.createProjectOwnerRoleBinding(ctx, project.ID, project.CreatedBy)
	}
	s.createProjectMembersGroup(ctx, project)
}

// projectBindingsFor returns the project-scoped role bindings held directly
// by the given user.
func projectBindingsFor(t *testing.T, s store.Store, projectID, userID string) []*store.RoleBinding {
	t.Helper()
	all, err := s.ListRoleBindingsForScope(context.Background(), store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range all {
		if b.PrincipalType == store.RoleBindingPrincipalUser && b.PrincipalID == userID {
			out = append(out, b)
		}
	}
	return out
}

// requireSingleOwnerBinding asserts that the project has exactly one
// project-owner binding and that it belongs to wantUserID.
func requireSingleOwnerBinding(t *testing.T, s store.Store, projectID, wantUserID string) {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	all, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var owners []*store.RoleBinding
	for _, b := range all {
		if b.RoleDefinitionID == ownerRD.ID {
			owners = append(owners, b)
		}
	}
	require.Len(t, owners, 1, "expected exactly one project-owner binding")
	assert.Equal(t, store.RoleBindingPrincipalUser, owners[0].PrincipalType)
	assert.Equal(t, wantUserID, owners[0].PrincipalID)
}

// removeAllProjectBindings deletes every binding the user holds on the
// project through the members API, acting as remover.
func removeAllProjectBindings(t *testing.T, srv *Server, s store.Store, remover *store.User, projectID, userID string) {
	t.Helper()
	for _, b := range projectBindingsFor(t, s, projectID, userID) {
		rec := doRequestAsUser(t, srv, remover, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+b.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}
	require.Empty(t, projectBindingsFor(t, s, projectID, userID))
}

// transferAndRemoveCreator transfers ownership alice -> bob and then has bob
// remove all of alice's remaining bindings.
func transferAndRemoveCreator(t *testing.T, srv *Server, s store.Store, alice, bob *store.User, project *store.Project) {
	t.Helper()
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects/"+project.ID+"/transfer-ownership",
		map[string]string{"newOwnerId": bob.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	removeAllProjectBindings(t, srv, s, bob, project.ID, alice.ID)
}

// addProjectOwner gives user a project-owner binding through the members API.
func addProjectOwner(t *testing.T, srv *Server, s store.Store, actor, user *store.User, projectID string) {
	t.Helper()
	ownerRD, err := s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	rec := doRequestAsUser(t, srv, actor, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		map[string]string{"roleDefinitionId": ownerRD.ID, "principalType": "user", "principalId": user.ID})
	require.Less(t, rec.Code, 300, rec.Body.String())
}

// A creator removed by a co-owner (no transfer, so project.OwnerID is still
// the creator) must not become owner again by reading the project, and has no
// access through OwnerID: Project.OwnerID grants nothing on a project resource
// (ptone/scion#2586), so the GET itself is denied.
func TestGetProject_RemovedCreatorSelfGETStaysRemoved(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)

	addProjectOwner(t, srv, s, alice, bob, project.ID)
	removeAllProjectBindings(t, srv, s, bob, project.ID, alice.ID)

	got, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	require.Equal(t, alice.ID, got.OwnerID, "precondition: no transfer, OwnerID still names the creator")

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
	assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, rec.Code,
		"removed creator named only in OwnerID must not read the project; body=%s", rec.Body.String())

	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"GET by a removed creator must not re-grant any project binding")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

// A GET by the new owner after transfer + removal must not re-grant the
// creator.
func TestGetProject_NewOwnerGETDoesNotRegrantCreator(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	transferAndRemoveCreator(t, srv, s, alice, bob, project)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"GET by the new owner must not re-grant the removed creator")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

// After a transfer, project.OwnerID points at the new owner, so the old owner
// with no remaining bindings has no read access (and no re-grant).
func TestTransferOwnership_OldOwnerWithoutBindingsCannotGET(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	transferAndRemoveCreator(t, srv, s, alice, bob, project)

	got, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, bob.ID, got.OwnerID, "transfer must update project.OwnerID")
	assert.Equal(t, alice.ID, got.CreatedBy, "transfer must not rewrite CreatedBy")

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
	assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, rec.Code,
		"old owner with no bindings must not read the project; body=%s", rec.Body.String())
	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID))
}

// The idempotent re-create (POST /projects with an existing id) and register
// of an existing project must not re-grant a removed creator either.
func TestIdempotentCreateAndRegister_DoNotRegrantRemovedCreator(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	transferAndRemoveCreator(t, srv, s, alice, bob, project)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/projects",
		map[string]string{"id": project.ID, "name": project.Name})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"idempotent create must not re-grant the removed creator")

	rec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/projects/register",
		RegisterProjectRequest{ID: project.ID, Name: project.Name})
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"register of an existing project must not re-grant the removed creator")

	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
}

// Genuine first creation still yields exactly one creator owner binding, on
// every create path.
func TestProjectFirstCreation_YieldsSingleCreatorOwnerBinding(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		srv, s, alice, _, _ := setupDemoPolicyTest(t)
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects",
			map[string]string{"name": "Fresh Create Project"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var p store.Project
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		requireSingleOwnerBinding(t, s, p.ID, alice.ID)
	})

	t.Run("register", func(t *testing.T) {
		srv, s, alice, _, _ := setupDemoPolicyTest(t)
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects/register",
			RegisterProjectRequest{Name: "Fresh Register Project"})
		require.Less(t, rec.Code, 300, rec.Body.String())
		var resp RegisterProjectResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Project)
		assert.True(t, resp.Created, "register must report a genuine creation")
		requireSingleOwnerBinding(t, s, resp.Project.ID, alice.ID)
	})

	t.Run("clone", func(t *testing.T) {
		srv, s, alice, _, src := setupDemoPolicyTest(t)
		rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
			map[string]string{"name": "Fresh Clone Project"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var p store.Project
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		requireSingleOwnerBinding(t, s, p.ID, alice.ID)
	})
}

// The startup backfill (run on every hub start) must not re-grant a removed
// creator, but must still backfill a legacy project that has no owner
// binding at all.
func TestBackfillProjectOwnerRoleBindings_SkipsProjectsWithAnOwner(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	transferAndRemoveCreator(t, srv, s, alice, bob, project)

	// Legacy project: CreatedBy set, no role bindings.
	legacy := &store.Project{
		ID:        tid("project-legacy-backfill"),
		Name:      "Legacy Backfill Project",
		Slug:      "legacy-backfill-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
	}
	require.NoError(t, s.CreateProject(ctx, legacy))

	// Simulate a restart.
	require.NoError(t, BackfillRoleBindings(ctx, s))

	assert.Empty(t, projectBindingsFor(t, s, project.ID, alice.ID),
		"startup backfill must not re-grant a removed creator")
	requireSingleOwnerBinding(t, s, project.ID, bob.ID)
	requireSingleOwnerBinding(t, s, legacy.ID, alice.ID)
}

// nilBindingScopeStore is a stub store whose ListRoleBindingsForScope
// returns a slice containing a nil element.
type nilBindingScopeStore struct {
	store.Store
	bindings []*store.RoleBinding
}

func (s nilBindingScopeStore) ListRoleBindingsForScope(context.Context, string, string) ([]*store.RoleBinding, error) {
	return s.bindings, nil
}

// TestProjectHasOwnerBinding_SkipsNilBindings covers the Gemini review on
// GoogleCloudPlatform/scion#2274: a nil element in the listed bindings must
// be skipped, not dereferenced.
func TestProjectHasOwnerBinding_SkipsNilBindings(t *testing.T) {
	ctx := context.Background()

	var has bool
	var err error
	require.NotPanics(t, func() {
		has, err = projectHasOwnerBinding(ctx, nilBindingScopeStore{bindings: []*store.RoleBinding{nil}}, "p1", "owner-rd")
	})
	require.NoError(t, err)
	assert.False(t, has)

	has, err = projectHasOwnerBinding(ctx, nilBindingScopeStore{bindings: []*store.RoleBinding{nil, {RoleDefinitionID: "owner-rd"}}}, "p1", "owner-rd")
	require.NoError(t, err)
	assert.True(t, has, "a real owner binding after a nil element must still be found")
}
