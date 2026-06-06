//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCloneTestServer returns a Server with mock storage, a store, and
// pre-created source harness config and template IDs suitable for clone tests.
func setupCloneTestServer(t *testing.T) (*Server, store.Store, string, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	now := time.Now()

	hcID := api.NewUUID()
	tplID := api.NewUUID()

	// Seed a source harness config (global).
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: hcID, Slug: "source-hc", Name: "Source HC",
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
		ID: tplID, Slug: "source-tpl", Name: "Source Template",
		DisplayName: "TPL Display", Description: "TPL desc",
		Harness:    "claude",
		Scope:      store.TemplateScopeGlobal,
		Visibility: store.VisibilityPublic,
		Status:     store.TemplateStatusActive,
		Created:    now, Updated: now,
	}))

	return srv, s, hcID, tplID
}

func TestHandleHarnessConfigClone_Success(t *testing.T) {
	srv, _, hcID, _ := setupCloneTestServer(t)

	body := map[string]interface{}{
		"name":       "My Clone",
		"scope":      "global",
		"visibility": "private",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hcID+"/clone", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.HarnessConfig
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	assert.NotEqual(t, hcID, clone.ID, "clone must have a new ID")
	assert.Equal(t, "my-clone", clone.Slug)
	assert.Equal(t, "My Clone", clone.Name)
	assert.Equal(t, "Source Display", clone.DisplayName)
	assert.Equal(t, "Source desc", clone.Description)
	assert.Equal(t, "claude", clone.Harness)
	assert.Equal(t, "global", clone.Scope)
	assert.Equal(t, "private", clone.Visibility)
	assert.NotNil(t, clone.Config)
}

func TestHandleHarnessConfigClone_CrossScope(t *testing.T) {
	srv, s, hcID, _ := setupCloneTestServer(t)
	ctx := context.Background()

	projectID := api.NewUUID()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Clone Project", Slug: "clone-project",
		OwnerID: DevUserID, CreatedBy: DevUserID,
		Created: time.Now(), Updated: time.Now(),
	}))

	body := map[string]interface{}{
		"name":    "Project Clone",
		"scope":   "project",
		"scopeId": projectID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hcID+"/clone", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var clone store.HarnessConfig
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	assert.Equal(t, "project", clone.Scope)
	assert.Equal(t, projectID, clone.ScopeID)
	assert.Equal(t, "claude", clone.Harness)
}

func TestDeleteTemplate_Authz_GlobalForbiddenForMember(t *testing.T) {
	srv, s, _, tplID := setupCloneTestServer(t)
	ctx := context.Background()

	member := &store.User{
		ID: api.NewUUID(), Email: "member-del@test.com",
		DisplayName: "Member", Role: store.UserRoleMember,
		Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodDelete, "/api/v1/templates/"+tplID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "non-admin should get 403 on global template delete: %s", rec.Body.String())
}

func TestDeleteTemplate_Authz_GlobalAllowedForAdmin(t *testing.T) {
	srv, s, _, tplID := setupCloneTestServer(t)
	ctx := context.Background()

	admin := &store.User{
		ID: api.NewUUID(), Email: "admin-del@test.com",
		DisplayName: "Admin", Role: store.UserRoleAdmin,
		Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/templates/"+tplID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "admin should be able to delete global template: %s", rec.Body.String())

	// Verify gone.
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/templates/"+tplID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCloneTemplate_Authz_DestinationChecked(t *testing.T) {
	srv, s, _, tplID := setupCloneTestServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID: api.NewUUID(), Email: "alice-clone@test.com",
		DisplayName: "Alice", Role: store.UserRoleMember,
		Status: "active", Created: time.Now(),
	}
	bob := &store.User{
		ID: api.NewUUID(), Email: "bob-clone@test.com",
		DisplayName: "Bob", Role: store.UserRoleMember,
		Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	require.NoError(t, s.CreateUser(ctx, bob))
	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	project := &store.Project{
		ID: api.NewUUID(), Name: "Authz Project", Slug: "authz-project",
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
	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/templates/"+tplID+"/clone", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "non-member should get 403: %s", rec.Body.String())

	// Alice is the project owner → should succeed.
	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates/"+tplID+"/clone", body)
	assert.Equal(t, http.StatusCreated, rec.Code, "project owner should be able to clone: %s", rec.Body.String())
}

func TestClone_SlugCollision_Returns409(t *testing.T) {
	srv, _, hcID, _ := setupCloneTestServer(t)

	body := map[string]interface{}{
		"name":  "Collision Clone",
		"scope": "global",
	}

	// First clone succeeds.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hcID+"/clone", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Second clone with same name → slug collision → 409.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+hcID+"/clone", body)
	assert.Equal(t, http.StatusConflict, rec.Code, "duplicate slug should return 409: %s", rec.Body.String())
}
