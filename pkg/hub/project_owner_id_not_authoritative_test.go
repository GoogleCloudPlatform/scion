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

// ptone/scion#2586: Project.OwnerID is display metadata, not an
// authorization source. Project authority comes only from project-scoped
// role bindings, so a creator removed from the project without an ownership
// transfer (OwnerID still names them) has no project access.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncLogBuffer is a mutex-guarded log sink. The backfill tests swap the
// process-global slog logger, and parallel tests or background goroutines in
// package hub may log through it concurrently, so a bare bytes.Buffer would
// race.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type staleOwnerFixture struct {
	srv     *Server
	s       store.Store
	creator *store.User
	coOwner *store.User
	project *store.Project
}

func createStaleOwnerUser(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: id, Email: email, DisplayName: email,
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, u.ID)
	return u
}

// setupStaleOwnerFixture creates a project whose creator (OwnerID and
// CreatedBy) holds the project-owner binding, adds a co-owner, and then has
// the co-owner remove the creator through the members API with no
// ownership transfer. Project.OwnerID still names the creator afterwards.
func setupStaleOwnerFixture(t *testing.T) staleOwnerFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	creator := createStaleOwnerUser(t, s, tid("stale-owner-creator"), "stale-creator@test.com")
	coOwner := createStaleOwnerUser(t, s, tid("stale-owner-coowner"), "stale-coowner@test.com")

	project := &store.Project{
		ID:        tid("stale-owner-project"),
		Name:      "Stale Owner Project",
		Slug:      "stale-owner-project",
		GitRemote: "github.com/test/stale-owner",
		OwnerID:   creator.ID,
		CreatedBy: creator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, creator.ID))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, coOwner.ID))

	creatorBinding := projectOwnerBindingFor(t, s, creator.ID, project.ID)
	require.NotNil(t, creatorBinding, "creator must start with a project-owner binding")

	rec := doRequestAsUser(t, srv, coOwner, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+creatorBinding.ID, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code,
		"co-owner removes creator: %s", rec.Body.String())
	require.Nil(t, projectOwnerBindingFor(t, s, creator.ID, project.ID),
		"creator's project-owner binding must be gone")

	stored, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, creator.ID, stored.OwnerID, "precondition: OwnerID still names the removed creator")

	return staleOwnerFixture{srv: srv, s: s, creator: creator, coOwner: coOwner, project: stored}
}

// projectOwnerBindingFor returns the user's project-owner binding on the
// project, or nil.
func projectOwnerBindingFor(t *testing.T, s store.Store, userID, projectID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for i := range bindings {
		b := bindings[i]
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == ownerRD.ID {
			return b
		}
	}
	return nil
}

// listProjectIDs returns the project IDs a user sees at path.
func listProjectIDs(t *testing.T, srv *Server, user *store.User, path string) []string {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	ids := make([]string, 0, len(resp.Projects))
	for _, p := range resp.Projects {
		ids = append(ids, p.ID)
	}
	return ids
}

// projectSecretReadAllowed evaluates project.secret_read for a user
// principal against the project, built with its OwnerID as handlers do.
func projectSecretReadAllowed(srv *Server, user *store.User, project *store.Project) Decision {
	ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	return srv.authzService.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   Resource{Type: "project", ID: project.ID, OwnerID: project.OwnerID},
		Action:     actionProjectSecretRead,
		Permission: "project.secret_read",
	})
}

// TestProjectOwnerID_RemovedCreatorWithoutTransferHasNoAccess builds a fresh
// stale-owner fixture in every subtest, so each guard stands on its own and
// none depends on state left by an earlier subtest. (With one shared fixture
// and the owner relationship grant restored, a successful "GET project" ran
// getProject -> createProjectMembersGroup and re-granted the creator a
// binding that later subtests then saw.) "PUT messaging policy" and "list and
// scope=mine" are authorized by bindings, not by the owner relationship rule,
// so they are positive guards of those binding-based paths rather than locks
// on the rule: they pass even with the rule restored.
func TestProjectOwnerID_RemovedCreatorWithoutTransferHasNoAccess(t *testing.T) {
	baseOf := func(f staleOwnerFixture) string { return "/api/v1/projects/" + f.project.ID }

	t.Run("GET project", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		base := baseOf(f)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodGet, base, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("PATCH project", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		base := baseOf(f)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPatch, base,
			map[string]string{"name": "Renamed By Removed Creator"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		stored, err := f.s.GetProject(context.Background(), f.project.ID)
		require.NoError(t, err)
		assert.Equal(t, "Stale Owner Project", stored.Name, "PATCH must not mutate the project")
	})

	t.Run("GET project secrets", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		base := baseOf(f)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodGet, base+"/secrets", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("project.secret_read decision", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		d := projectSecretReadAllowed(f.srv, f.creator, f.project)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
	})

	t.Run("GET messaging policy", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		base := baseOf(f)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodGet, base+"/messaging-policy", nil)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
	})

	t.Run("PUT messaging policy", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		base := baseOf(f)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPut, base+"/messaging-policy",
			ProjectMessagingPolicyUpdateRequest{CrossProjectInbound: "any"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("register existing project", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPost, "/api/v1/projects/register",
			RegisterProjectRequest{ID: f.project.ID, Name: f.project.Name, GitRemote: f.project.GitRemote})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("clone", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		base := baseOf(f)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPost, base+"/clone",
			map[string]string{"name": "Clone By Removed Creator"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("list and scope=mine", func(t *testing.T) {
		f := setupStaleOwnerFixture(t)
		assert.NotContains(t, listProjectIDs(t, f.srv, f.creator, "/api/v1/projects"), f.project.ID)
		assert.NotContains(t, listProjectIDs(t, f.srv, f.creator, "/api/v1/projects?scope=mine"), f.project.ID)
	})
}

func TestProjectOwnerID_CurrentOwnerWithBindingUnaffected(t *testing.T) {
	f := setupStaleOwnerFixture(t)
	base := "/api/v1/projects/" + f.project.ID

	rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodPatch, base, map[string]string{"name": "Renamed"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodGet, base+"/secrets", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	d := projectSecretReadAllowed(f.srv, f.coOwner, f.project)
	assert.True(t, d.Allowed, "reason %q", d.Reason)

	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodGet, base+"/messaging-policy", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var policy ProjectMessagingPolicyResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&policy))
	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodPut, base+"/messaging-policy",
		ProjectMessagingPolicyUpdateRequest{CrossProjectInbound: "members", ExpectedRevision: policy.Revision})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Contains(t, listProjectIDs(t, f.srv, f.coOwner, "/api/v1/projects"), f.project.ID)
	assert.Contains(t, listProjectIDs(t, f.srv, f.coOwner, "/api/v1/projects?scope=mine"), f.project.ID)
}

// TestProjectOwnerID_LegacyProjectAccessComesFromBackfill pins that a legacy
// project with a creator but no bindings is reachable by its creator only
// through the startup owner backfill, not through OwnerID.
func TestProjectOwnerID_LegacyProjectAccessComesFromBackfill(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	creator := createStaleOwnerUser(t, s, tid("legacy-owner-creator"), "legacy-creator@test.com")
	project := &store.Project{
		ID:        tid("legacy-owner-project"),
		Name:      "Legacy Project",
		Slug:      "legacy-owner-project",
		OwnerID:   creator.ID,
		CreatedBy: creator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.Nil(t, projectOwnerBindingFor(t, s, creator.ID, project.ID), "legacy project has no bindings")
	base := "/api/v1/projects/" + project.ID

	// Before the backfill, OwnerID alone grants nothing.
	rec := doRequestAsUser(t, srv, creator, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "OwnerID alone must not grant read: %s", rec.Body.String())

	require.NoError(t, backfillProjectOwnerRoleBindings(ctx, s))
	require.NotNil(t, projectOwnerBindingFor(t, s, creator.ID, project.ID), "backfill grants the creator")

	rec = doRequestAsUser(t, srv, creator, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, creator, http.MethodPatch, base, map[string]string{"name": "Legacy Renamed"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, listProjectIDs(t, srv, creator, "/api/v1/projects?scope=mine"), project.ID)
}

// TestProjectOwnerID_BackfillWarnsOnOwnerOnlyLegacyProject pins that the
// startup backfill does not grant to OwnerID: a project with OwnerID set,
// no CreatedBy and no owner binding stays ownerless and is logged once.
func TestProjectOwnerID_BackfillWarnsOnOwnerOnlyLegacyProject(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	owner := createStaleOwnerUser(t, s, tid("owner-only-legacy-user"), "owner-only@test.com")
	project := &store.Project{
		ID:      tid("owner-only-legacy-project"),
		Name:    "Owner Only Legacy",
		Slug:    "owner-only-legacy-project",
		OwnerID: owner.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Do not add t.Parallel(): this test swaps the process-global slog
	// logger with slog.SetDefault to capture the warning.
	var buf syncLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	require.NoError(t, backfillProjectOwnerRoleBindings(ctx, s))

	assert.Nil(t, projectOwnerBindingFor(t, s, owner.ID, project.ID), "backfill must not grant to OwnerID")
	assert.Equal(t, 1, strings.Count(buf.String(), "project_id="+project.ID),
		"exactly one warning for the owner-only project: %s", buf.String())
	assert.Contains(t, buf.String(), "no project-owner binding")
}

// TestProjectOwnerID_BackfillNoWarningWhenNotOwnerOnly pins the negative
// cases of the owner-only warning: a project whose OwnerID already holds a
// project-owner binding (with CreatedBy empty or different), a project whose
// OwnerID equals CreatedBy, and a project with neither OwnerID nor CreatedBy
// produce no warning. The owner-only project alongside them still warns once.
func TestProjectOwnerID_BackfillNoWarningWhenNotOwnerOnly(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	bound := createStaleOwnerUser(t, s, tid("bound-owner-user"), "bound-owner@test.com")
	boundProject := &store.Project{
		ID:      tid("bound-owner-project"),
		Name:    "Bound Owner",
		Slug:    "bound-owner-project",
		OwnerID: bound.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, boundProject))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      bound.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          boundProject.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	unowned := &store.Project{
		ID:      tid("unowned-legacy-project"),
		Name:    "Unowned Legacy",
		Slug:    "unowned-legacy-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, unowned))

	ownerOnlyUser := createStaleOwnerUser(t, s, tid("owner-only-neg-user"), "owner-only-neg@test.com")
	ownerOnly := &store.Project{
		ID:      tid("owner-only-neg-project"),
		Name:    "Owner Only Neg",
		Slug:    "owner-only-neg-project",
		OwnerID: ownerOnlyUser.ID,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, ownerOnly))

	// Do not add t.Parallel(): this test swaps the process-global slog
	// logger with slog.SetDefault to capture the warning.
	// OwnerID equals CreatedBy: the backfill grants CreatedBy, no warning.
	sameUser := createStaleOwnerUser(t, s, tid("same-owner-creator-user"), "same-owner-creator@test.com")
	sameProject := &store.Project{
		ID:        tid("same-owner-creator-project"),
		Name:      "Same Owner Creator",
		Slug:      "same-owner-creator-project",
		OwnerID:   sameUser.ID,
		CreatedBy: sameUser.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, sameProject))

	// OwnerID differs from CreatedBy but OwnerID holds a project-owner
	// binding: no warning.
	mismatchCreator := createStaleOwnerUser(t, s, tid("bound-mismatch-creator"), "bound-mismatch-creator@test.com")
	mismatchOwner := createStaleOwnerUser(t, s, tid("bound-mismatch-owner"), "bound-mismatch-owner@test.com")
	boundMismatch := &store.Project{
		ID:        tid("bound-mismatch-project"),
		Name:      "Bound Mismatch",
		Slug:      "bound-mismatch-project",
		OwnerID:   mismatchOwner.ID,
		CreatedBy: mismatchCreator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, boundMismatch))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      mismatchOwner.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          boundMismatch.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	var buf syncLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	require.NoError(t, backfillProjectOwnerRoleBindings(ctx, s))

	logs := buf.String()
	assert.NotContains(t, logs, "project_id="+boundProject.ID,
		"no warning when OwnerID already holds a project-owner binding: %s", logs)
	assert.NotContains(t, logs, "project_id="+unowned.ID,
		"no warning when neither OwnerID nor CreatedBy is set: %s", logs)
	assert.NotContains(t, logs, "project_id="+sameProject.ID,
		"no warning when OwnerID equals CreatedBy: %s", logs)
	assert.NotContains(t, logs, "project_id="+boundMismatch.ID,
		"no warning when OwnerID differs from CreatedBy but holds a binding: %s", logs)
	assert.Equal(t, 1, strings.Count(logs, "project_id="+ownerOnly.ID),
		"exactly one warning for the owner-only project: %s", logs)
	assert.Equal(t, 1, strings.Count(logs, "no project-owner binding"),
		"exactly one owner-only warning overall: %s", logs)
}

// TestProjectOwnerID_BackfillWarnsOnOwnerIDCreatorMismatch pins the second
// warning shape: OwnerID set, CreatedBy set but different, and OwnerID holds
// no project-owner binding. The backfill grants CreatedBy as usual, grants
// nothing to OwnerID, and logs exactly one warning for the project.
func TestProjectOwnerID_BackfillWarnsOnOwnerIDCreatorMismatch(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	creator := createStaleOwnerUser(t, s, tid("mismatch-creator"), "mismatch-creator@test.com")
	owner := createStaleOwnerUser(t, s, tid("mismatch-owner"), "mismatch-owner@test.com")
	project := &store.Project{
		ID:        tid("mismatch-project"),
		Name:      "Mismatch Legacy",
		Slug:      "mismatch-project",
		OwnerID:   owner.ID,
		CreatedBy: creator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Do not add t.Parallel(): this test swaps the process-global slog
	// logger with slog.SetDefault to capture the warning.
	var buf syncLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	require.NoError(t, backfillProjectOwnerRoleBindings(ctx, s))

	assert.Nil(t, projectOwnerBindingFor(t, s, owner.ID, project.ID), "backfill must not grant to OwnerID")
	assert.NotNil(t, projectOwnerBindingFor(t, s, creator.ID, project.ID), "backfill grants CreatedBy")
	logs := buf.String()
	assert.Equal(t, 1, strings.Count(logs, "project_id="+project.ID),
		"exactly one warning for the mismatch project: %s", logs)
	assert.Contains(t, logs, "OwnerID differs from CreatedBy")
}
