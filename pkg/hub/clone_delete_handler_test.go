//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCloneTestServer returns a Server with mock storage, a store, and a
// pre-created source harness config and template suitable for clone tests.
func setupCloneTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	now := time.Now()

	// Seed a source harness config (global).
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: "hc-source", Slug: "source-hc", Name: "Source HC",
		DisplayName: "Source Display", Description: "Source desc",
		Harness:    "claude",
		Config:     &store.HarnessConfigData{Harness: "claude", Image: "img:latest"},
		Scope:      store.HarnessConfigScopeGlobal,
		Visibility: store.VisibilityPublic,
		Status:     store.HarnessConfigStatusActive,
		Created:    now, Updated: now,
	}))

	// Seed a source template (global).
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: "tpl-source", Slug: "source-tpl", Name: "Source Template",
		DisplayName: "TPL Display", Description: "TPL desc",
		Harness:    "claude",
		Scope:      store.TemplateScopeGlobal,
		Visibility: store.VisibilityPublic,
		Status:     store.TemplateStatusActive,
		Created:    now, Updated: now,
	}))

	return srv, s
}

// TestHandleHarnessConfigClone_Success clones a harness config and verifies the
// new ID, slug, and copied fields.
func TestHandleHarnessConfigClone_Success(t *testing.T) {
	srv, _ := setupCloneTestServer(t)

	body := map[string]interface{}{
		"name":       "My Clone",
		"scope":      "global",
		"visibility": "private",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/hc-source/clone", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.HarnessConfig
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	assert.NotEqual(t, "hc-source", clone.ID, "clone must have a new ID")
	assert.Equal(t, "my-clone", clone.Slug)
	assert.Equal(t, "My Clone", clone.Name)
	assert.Equal(t, "Source Display", clone.DisplayName)
	assert.Equal(t, "Source desc", clone.Description)
	assert.Equal(t, "claude", clone.Harness)
	assert.Equal(t, "global", clone.Scope)
	assert.Equal(t, "private", clone.Visibility)
	assert.NotNil(t, clone.Config)
}

// TestHandleHarnessConfigClone_CrossScope clones a global harness config into a
// project scope.
func TestHandleHarnessConfigClone_CrossScope(t *testing.T) {
	srv, s := setupCloneTestServer(t)
	ctx := context.Background()

	// Create project owned by dev user so admin has access.
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: "project-clone", Name: "Clone Project", Slug: "clone-project",
		OwnerID: DevUserID, CreatedBy: DevUserID,
		Created: time.Now(), Updated: time.Now(),
	}))

	body := map[string]interface{}{
		"name":    "Project Clone",
		"scope":   "project",
		"scopeId": "project-clone",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/hc-source/clone", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.HarnessConfig
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	assert.Equal(t, "project", clone.Scope)
	assert.Equal(t, "project-clone", clone.ScopeID)
	assert.Equal(t, "claude", clone.Harness)
}

// TestDeleteTemplate_Authz_GlobalForbiddenForMember verifies that a non-admin
// member cannot delete a global template.
func TestDeleteTemplate_Authz_GlobalForbiddenForMember(t *testing.T) {
	srv, s := setupCloneTestServer(t)
	ctx := context.Background()

	member := &store.User{
		ID: "user-member-del", Email: "member@test.com",
		DisplayName: "Member", Role: store.UserRoleMember,
		Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodDelete, "/api/v1/templates/tpl-source", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "non-admin should get 403 on global template delete: %s", rec.Body.String())
}

// TestDeleteTemplate_Authz_GlobalAllowedForAdmin verifies that an admin can
// delete a global template.
func TestDeleteTemplate_Authz_GlobalAllowedForAdmin(t *testing.T) {
	srv, s := setupCloneTestServer(t)
	ctx := context.Background()

	admin := &store.User{
		ID: "user-admin-del", Email: "admin@test.com",
		DisplayName: "Admin", Role: store.UserRoleAdmin,
		Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/templates/tpl-source", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "admin should be able to delete global template: %s", rec.Body.String())

	// Verify gone.
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/templates/tpl-source", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestCloneTemplate_Authz_DestinationChecked verifies that cloning into a
// project checks ActionCreate against the destination project.
func TestCloneTemplate_Authz_DestinationChecked(t *testing.T) {
	srv, s := setupCloneTestServer(t)
	ctx := context.Background()

	// alice owns the project; bob does not.
	alice := &store.User{
		ID: "user-alice-clone", Email: "alice@test.com",
		DisplayName: "Alice", Role: store.UserRoleMember,
		Status: "active", Created: time.Now(),
	}
	bob := &store.User{
		ID: "user-bob-clone", Email: "bob@test.com",
		DisplayName: "Bob", Role: store.UserRoleMember,
		Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	require.NoError(t, s.CreateUser(ctx, bob))
	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	project := &store.Project{
		ID: "project-authz", Name: "Authz Project", Slug: "authz-project",
		OwnerID: alice.ID, CreatedBy: alice.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	body := map[string]interface{}{
		"name":    "Clone Into Project",
		"scope":   "project",
		"scopeId": project.ID,
	}

	// Bob is not a project member → should be forbidden.
	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/templates/tpl-source/clone", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "non-member should get 403: %s", rec.Body.String())

	// Alice is the project owner → should succeed.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates/tpl-source/clone", body)
	assert.Equal(t, http.StatusCreated, rec.Code, "project owner should be able to clone: %s", rec.Body.String())
}

// TestClone_SlugCollision_Returns409 verifies that cloning with a name that
// produces a duplicate slug in the same scope returns 409 Conflict.
func TestClone_SlugCollision_Returns409(t *testing.T) {
	srv, _ := setupCloneTestServer(t)

	body := map[string]interface{}{
		"name":  "Collision Clone",
		"scope": "global",
	}

	// First clone succeeds.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/hc-source/clone", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Second clone with same name → slug collision → 409.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/hc-source/clone", body)
	assert.Equal(t, http.StatusConflict, rec.Code, "duplicate slug should return 409: %s", rec.Body.String())
}
