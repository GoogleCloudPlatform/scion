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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type projectGitHubFakeAPI struct {
	srv *httptest.Server

	mu             sync.Mutex
	appSlug        string
	appPermissions map[string]string
	appStatus      int
	repoStatus     int
	repoAccess     map[string]map[string]githubapp.RepoAccess
	appCalls       int
	repoCalls      int
	mintPerms      []map[string]string
}

func newProjectGitHubFakeAPI(t *testing.T, slug string, appPerms map[string]string) *projectGitHubFakeAPI {
	t.Helper()
	f := &projectGitHubFakeAPI{
		appSlug:        slug,
		appPermissions: appPerms,
		repoAccess:     map[string]map[string]githubapp.RepoAccess{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.appCalls++
		appStatus := f.appStatus
		perms := make(map[string]string, len(f.appPermissions))
		for k, v := range f.appPermissions {
			perms[k] = v
		}
		s := f.appSlug
		f.mu.Unlock()
		if appStatus != 0 {
			w.WriteHeader(appStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":          1,
			"slug":        s,
			"permissions": perms,
		})
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.repoCalls++
		if f.repoStatus != 0 {
			w.WriteHeader(f.repoStatus)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		repo := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		access, ok := f.repoAccess[tok][repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"full_name":   repo,
			"permissions": access,
		})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.mintPerms = append(f.mintPerms, body.Permissions)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_test_minted_token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *projectGitHubFakeAPI) grant(token, ownerRepo string, access githubapp.RepoAccess) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repoAccess[token] == nil {
		f.repoAccess[token] = map[string]githubapp.RepoAccess{}
	}
	f.repoAccess[token][strings.ToLower(ownerRepo)] = access
}

func (f *projectGitHubFakeAPI) callCounts() (appCalls, repoCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appCalls, f.repoCalls
}

func (f *projectGitHubFakeAPI) lastMintPerms() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mintPerms) == 0 {
		return nil
	}
	return f.mintPerms[len(f.mintPerms)-1]
}

func setupProjectGitHubTestHub(t *testing.T, fakeAPI *projectGitHubFakeAPI) (*Server, store.Store, *store.User) {
	t.Helper()
	srv, st := testServer(t)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 1
	srv.config.GitHubAppConfig.APIBaseURL = fakeAPI.srv.URL
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	srv.config.GitHubAppConfig.InstallationURL = "https://github.com/apps/test-scion-bot/installations/new"
	srv.mu.Unlock()

	// Seed the App identity cache once (not during GET /projects/{id}/github).
	_, ok := srv.GitHubAppIdentity(context.Background())
	require.True(t, ok, "expected fake GET /app to populate identity cache")

	admin := &store.User{
		ID:          tid("pg-hub-admin"),
		Email:       "pg-admin@example.com",
		DisplayName: "PG Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, st.CreateUser(context.Background(), admin))

	// Grant the hub admin role binding so routePermissionDecision and githubBindingUserIsAdmin allow bind.
	if roleDef, err := st.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubAdmin, store.RoleScopeSystem); err == nil && roleDef != nil {
		_, _ = st.CreateRoleBinding(context.Background(), &store.RoleBinding{
			RoleDefinitionID: roleDef.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      admin.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        admin.ID,
			CreatedAt:        time.Now(),
		})
	}

	return srv, st, admin
}

func TestProjectGitHub_ReadModelStatesAndNoGitHubCalls(t *testing.T) {
	fakeAPI := newProjectGitHubFakeAPI(t, "test-scion-bot", map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	})
	srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
	ctx := context.Background()

	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 4412,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))

	// 1. Unassociated project.
	unassoc := &store.Project{
		ID:        tid("pg-unassoc"),
		Name:      "pg-unassoc",
		Slug:      tid("pg-unassoc"),
		GitRemote: fakeAPI.srv.URL + "/acme/api",
		CreatedBy: admin.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, unassoc))

	appBefore, repoBefore := fakeAPI.callCounts()
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+unassoc.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Ensure omitted M3/M1.4 fields are absent.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "extra_repos")
	assert.NotContains(t, raw, "triggers_readiness")

	var resp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, unassoc.ID, resp.ProjectID)
	assert.True(t, resp.App.Configured)
	assert.Equal(t, "test-scion-bot", resp.App.Slug)
	assert.Equal(t, "acme", resp.Remote.Owner)
	assert.Equal(t, "api", resp.Remote.Repo)
	assert.True(t, resp.Remote.OnAppHost)
	assert.Nil(t, resp.Association.InstallationID)
	assert.Nil(t, resp.Association.Basis)
	assert.False(t, resp.Association.Authorized)
	assert.Equal(t, "default", resp.Permissions.Mode)
	assert.Empty(t, resp.Permissions.Effective)
	assert.True(t, resp.Can.Bind)
	assert.True(t, resp.Can.EditPermissions)
	assert.True(t, resp.Can.Discover)

	appAfter, repoAfter := fakeAPI.callCounts()
	assert.Equal(t, appBefore, appAfter, "GET /github must not call GitHub /app")
	assert.Equal(t, repoBefore, repoAfter, "GET /github must not call GitHub /repos")

	// MethodNotAllowed on POST.
	recMethod := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/"+unassoc.ID+"/github", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, recMethod.Code)

	// 2. Admin-bound project (via PUT /github-installation).
	bindRec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+unassoc.ID+"/github-installation", map[string]int64{
		"installation_id": 4412,
	})
	require.Equal(t, http.StatusOK, bindRec.Code, bindRec.Body.String())

	appBefore, repoBefore = fakeAPI.callCounts()
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+unassoc.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Association.InstallationID)
	assert.Equal(t, int64(4412), *resp.Association.InstallationID)
	assert.Equal(t, "acme", resp.Association.AccountLogin)
	require.NotNil(t, resp.Association.Basis)
	assert.Equal(t, githubAssociationBasisAdminBinding, *resp.Association.Basis)
	assert.True(t, resp.Association.WriteAllowed)
	assert.True(t, resp.Association.Authorized)
	assert.True(t, resp.Association.Verified)
	assert.Equal(t, admin.ID, resp.Association.BoundBy)
	assert.Equal(t, admin.DisplayName, resp.Association.BoundByName)
	assert.NotEmpty(t, resp.Association.BoundAt)
	assert.True(t, resp.Permissions.AppGrantKnown)
	assert.Equal(t, "https://github.com/apps/test-scion-bot/installations/new", resp.App.InstallationURL)

	// When the binding admin has no display name, bound_by_name falls back to
	// a neutral label and must NOT expose the admin's email address.
	admin.DisplayName = ""
	require.NoError(t, st.UpdateUser(ctx, admin))
	recNoName := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+unassoc.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recNoName.Code, recNoName.Body.String())
	var noNameResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recNoName.Body.Bytes(), &noNameResp))
	assert.Equal(t, "a hub admin", noNameResp.Association.BoundByName)
	assert.NotContains(t, noNameResp.Association.BoundByName, admin.Email)
	admin.DisplayName = "PG Admin"
	require.NoError(t, st.UpdateUser(ctx, admin))
	assert.Equal(t, map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	}, resp.Permissions.Effective)
	assert.Empty(t, resp.Permissions.Capped)
	assert.Empty(t, resp.Permissions.CapReasons)

	appAfter, repoAfter = fakeAPI.callCounts()
	assert.Equal(t, appBefore, appAfter)
	assert.Equal(t, repoBefore, repoAfter)

	// 3. Creator-proof project (populated in association report cache).
	creator := &store.User{
		ID:          tid("pg-creator"),
		Email:       "pg-creator@example.com",
		DisplayName: "PG Creator",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, st.CreateUser(ctx, creator))
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID:             tid("pg-creator-tok"),
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_creator_push",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        creator.ID,
	}))
	srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))
	fakeAPI.grant("ghp_creator_push", "acme/web", githubapp.RepoAccess{Pull: true, Push: true})

	instID := int64(4412)
	creatorProj := &store.Project{
		ID:                   tid("pg-creator-proj"),
		Name:                 "pg-creator-proj",
		Slug:                 tid("pg-creator-proj"),
		GitRemote:            fakeAPI.srv.URL + "/acme/web",
		CreatedBy:            creator.ID,
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, creatorProj))

	// Populate the association report cache once (simulating background/admin report).
	_, _, err := srv.githubAssociationReport(ctx, false)
	require.NoError(t, err)

	appBefore, repoBefore = fakeAPI.callCounts()
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+creatorProj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var creatorResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &creatorResp))
	require.NotNil(t, creatorResp.Association.Basis)
	assert.Equal(t, githubAssociationBasisCreatorPush, *creatorResp.Association.Basis)
	assert.True(t, creatorResp.Association.Authorized)
	assert.True(t, creatorResp.Association.WriteAllowed)
	assert.Empty(t, creatorResp.Association.BoundBy)
	assert.Empty(t, creatorResp.Association.BoundAt)

	appAfter, repoAfter = fakeAPI.callCounts()
	assert.Equal(t, appBefore, appAfter, "GET /github must not call GitHub /app")
	assert.Equal(t, repoBefore, repoAfter, "GET /github must use cached report entry without calling GitHub /repos")
}

func TestProjectGitHub_AuthzAndCapabilities(t *testing.T) {
	fakeAPI := newProjectGitHubFakeAPI(t, "test-scion-bot", map[string]string{
		"contents": "write",
		"metadata": "read",
	})
	srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
	ctx := context.Background()

	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 7711,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))

	projAdmin := &store.User{
		ID:          tid("pg-proj-admin"),
		Email:       "pg-proj-admin@example.com",
		DisplayName: "Project Admin",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	outsider := &store.User{
		ID:          tid("pg-outsider"),
		Email:       "pg-outsider@example.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	projViewer := &store.User{
		ID:          tid("pg-proj-viewer"),
		Email:       "pg-proj-viewer@example.com",
		DisplayName: "Project Viewer",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, st.CreateUser(ctx, projAdmin))
	require.NoError(t, st.CreateUser(ctx, outsider))
	require.NoError(t, st.CreateUser(ctx, projViewer))

	proj := &store.Project{
		ID:        tid("pg-authz"),
		Name:      "pg-authz",
		Slug:      tid("pg-authz"),
		GitRemote: fakeAPI.srv.URL + "/acme/authz-repo",
		OwnerID:   projAdmin.ID,
		CreatedBy: projAdmin.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	// Give projAdmin and admin the project-admin role on proj so admin can also mint a project-scoped UAT.
	if roleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject); err == nil && roleDef != nil {
		for _, uid := range []string{projAdmin.ID, admin.ID} {
			_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: roleDef.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      uid,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          proj.ID,
				CreatedBy:        admin.ID,
				CreatedAt:        time.Now(),
			})
			require.NoError(t, err)
		}
	}

	// Give projViewer the read-only project-viewer role on proj.
	if viewerRole, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject); err == nil && viewerRole != nil {
		_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: viewerRole.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      projViewer.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          proj.ID,
			CreatedBy:        admin.ID,
			CreatedAt:        time.Now(),
		})
		require.NoError(t, err)
	}

	// Non-member gets 403 or 404.
	recOut := doRequestAsUser(t, srv, outsider, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, recOut.Code, "outsider should be denied: %s", recOut.Body.String())

	// Read-only project viewer gets 200 with all write capabilities false and DELETE /github-installation 403.
	recViewer := doRequestAsUser(t, srv, projViewer, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recViewer.Code, recViewer.Body.String())
	var viewerResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recViewer.Body.Bytes(), &viewerResp))
	assert.False(t, viewerResp.Can.Bind, "project viewer must not have can.bind")
	assert.False(t, viewerResp.Can.Unbind, "project viewer must not have can.unbind")
	assert.False(t, viewerResp.Can.Check, "project viewer must not have can.check")
	assert.False(t, viewerResp.Can.EditPermissions, "project viewer must not have can.edit_permissions")
	assert.False(t, viewerResp.Can.Discover, "project viewer must not have can.discover")

	recViewerDel := doRequestAsUser(t, srv, projViewer, http.MethodDelete, "/api/v1/projects/"+proj.ID+"/github-installation", nil)
	assert.Equal(t, http.StatusForbidden, recViewerDel.Code, "project viewer DELETE /github-installation must be 403: %s", recViewerDel.Body.String())

	// Non-hub-admin project admin gets 200 with can.bind=false, can.unbind=true, can.check=true, can.edit_permissions=true, can.discover=false.
	recMember := doRequestAsUser(t, srv, projAdmin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recMember.Code, recMember.Body.String())
	var resp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recMember.Body.Bytes(), &resp))
	assert.False(t, resp.Can.Bind, "non-hub-admin must not have can.bind")
	assert.False(t, resp.Can.Discover, "non-hub-admin must not have can.discover")
	assert.True(t, resp.Can.Unbind, "project admin should have can.unbind")
	assert.True(t, resp.Can.Check, "project admin should have can.check")
	assert.True(t, resp.Can.EditPermissions, "project admin should have can.edit_permissions")
	assert.Empty(t, resp.App.InstallationURL, "non-discover caller should not receive installation_url")

	// Non-admin PUT bind still returns 403.
	recPut := doRequestAsUser(t, srv, projAdmin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/github-installation", map[string]int64{
		"installation_id": 7711,
	})
	assert.Equal(t, http.StatusForbidden, recPut.Code, "non-hub-admin PUT bind must be 403: %s", recPut.Body.String())

	// Hub admin gets can.bind=true, can.unbind=true, can.check=true, can.discover=true.
	recAdmin := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recAdmin.Code, recAdmin.Body.String())
	require.NoError(t, json.Unmarshal(recAdmin.Body.Bytes(), &resp))
	assert.True(t, resp.Can.Bind)
	assert.True(t, resp.Can.Unbind)
	assert.True(t, resp.Can.Check)
	assert.True(t, resp.Can.Discover)
	assert.True(t, resp.Can.EditPermissions)
	assert.Equal(t, "https://github.com/apps/test-scion-bot/installations/new", resp.App.InstallationURL)

	// Hub admin using a project-scoped UAT must get can.bind=false (hub boundary required) and PUT 403.
	tokRec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name":     "proj-scoped-admin-uat",
		"boundary": map[string]string{"kind": "project", "projectId": proj.ID},
		"scopes":   []string{"project:read", "project:update"},
	})
	require.Equal(t, http.StatusCreated, tokRec.Code, tokRec.Body.String())
	var tokResp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(tokRec.Body.Bytes(), &tokResp))
	require.NotEmpty(t, tokResp.Token)

	recUAT := doRequestWithUAT(t, srv, tokResp.Token, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recUAT.Code, recUAT.Body.String())
	var uatResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recUAT.Body.Bytes(), &uatResp))
	assert.False(t, uatResp.Can.Bind, "project-scoped UAT must never get can.bind=true")
	assert.False(t, uatResp.Can.Discover, "project-scoped UAT must never get can.discover=true")
	assert.True(t, uatResp.Can.Unbind)
	assert.True(t, uatResp.Can.Check)

	recUATPut := doRequestWithUAT(t, srv, tokResp.Token, http.MethodPut, "/api/v1/projects/"+proj.ID+"/github-installation", map[string]int64{
		"installation_id": 7711,
	})
	assert.Equal(t, http.StatusForbidden, recUATPut.Code, "project-scoped UAT PUT bind must be 403: %s", recUATPut.Body.String())
}

func TestProjectGitHub_ColdAppIdentity(t *testing.T) {
	fakeAPI := newProjectGitHubFakeAPI(t, "cold-app", map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	})
	srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
	ctx := context.Background()

	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 8101,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))
	proj := &store.Project{
		ID:        tid("pg-cold-id"),
		Name:      "pg-cold-id",
		Slug:      tid("pg-cold-id"),
		GitRemote: fakeAPI.srv.URL + "/acme/cold-id-repo",
		CreatedBy: admin.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, proj))
	bindRec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/github-installation", map[string]int64{"installation_id": 8101})
	require.Equal(t, http.StatusOK, bindRec.Code)

	// Reset App identity cache (simulating hub restart): GET /github populates via ensureGitHubAppIdentity (1 /app call), no false app_grant caps.
	srv.githubAppIdentity.reset()
	appBefore, _ := fakeAPI.callCounts()

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Permissions.AppGrantKnown)
	assert.Equal(t, "cold-app", resp.App.Slug)
	assert.Equal(t, map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	}, resp.Permissions.Effective)
	assert.Empty(t, resp.Permissions.Capped)
	assert.Empty(t, resp.Permissions.CapReasons)

	appAfterFirst, _ := fakeAPI.callCounts()
	assert.Equal(t, appBefore+1, appAfterFirst, "cold identity cache should fetch GET /app once")

	// Second GET reuses cached App identity with 0 additional /app calls.
	rec2 := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	appAfterSecond, _ := fakeAPI.callCounts()
	assert.Equal(t, appAfterFirst, appAfterSecond, "warm identity cache must not call GET /app again")

	// When GET /app fails, app_grant_known=false, app_granted=null, and NO false app_grant caps are reported.
	fakeAPI.mu.Lock()
	fakeAPI.appStatus = http.StatusInternalServerError
	fakeAPI.mu.Unlock()
	srv.githubAppIdentity.reset()

	recFail := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recFail.Code, recFail.Body.String())
	var failResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recFail.Body.Bytes(), &failResp))
	assert.False(t, failResp.Permissions.AppGrantKnown)
	assert.Nil(t, failResp.Permissions.AppGranted)
	assert.Empty(t, failResp.Permissions.Effective)
	assert.Empty(t, failResp.Permissions.Capped, "unknown App permissions must not produce false app_grant caps")
	assert.Empty(t, failResp.Permissions.CapReasons)
}

func TestProjectGitHub_CreatorProofColdAndUnverified(t *testing.T) {
	fakeAPI := newProjectGitHubFakeAPI(t, "creator-app", map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	})
	srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
	ctx := context.Background()

	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 8201,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))

	creator := &store.User{
		ID:          tid("pg-cold-creator"),
		Email:       "pg-cold-creator@example.com",
		DisplayName: "Cold Creator",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, st.CreateUser(ctx, creator))
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID:             tid("pg-cold-creator-tok"),
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_cold_push",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        creator.ID,
	}))
	srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))
	fakeAPI.grant("ghp_cold_push", "acme/cold-creator-repo", githubapp.RepoAccess{Pull: true, Push: true})

	instID := int64(8201)
	proj := &store.Project{
		ID:                   tid("pg-cold-creator-proj"),
		Name:                 "pg-cold-creator-proj",
		Slug:                 tid("pg-cold-creator-proj"),
		GitRemote:            fakeAPI.srv.URL + "/acme/cold-creator-repo",
		CreatedBy:            creator.ID,
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	// Advance timeNow past the 10-minute access-cache TTL after an initial mint to exercise the cold path.
	now := time.Now().UTC()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = origTimeNow })

	_, _, err := srv.mintGitHubAppToken(ctx, proj)
	require.NoError(t, err)
	now = now.Add(11 * time.Minute)

	_, repoBefore := fakeAPI.callCounts()
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Association.Authorized)
	assert.True(t, resp.Association.Verified)
	require.NotNil(t, resp.Association.Basis)
	assert.Equal(t, githubAssociationBasisCreatorPush, *resp.Association.Basis)
	assert.Equal(t, map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	}, resp.Permissions.Effective)
	_, repoAfterFirst := fakeAPI.callCounts()
	assert.Equal(t, repoBefore+1, repoAfterFirst, "expired access cache should run bounded creator check once")

	// Immediate second GET reuses the access cache (0 additional /repos calls).
	rec2 := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	_, repoAfterSecond := fakeAPI.callCounts()
	assert.Equal(t, repoAfterFirst, repoAfterSecond, "warm access cache must not call GitHub /repos again")

	// When the GitHub check fails (500), return explicit unverified state (not unauthorized) with fixed safe text.
	now = now.Add(11 * time.Minute)
	fakeAPI.mu.Lock()
	fakeAPI.repoStatus = http.StatusInternalServerError
	fakeAPI.mu.Unlock()

	recUnverified := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recUnverified.Code, recUnverified.Body.String())
	var unvResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recUnverified.Body.Bytes(), &unvResp))
	assert.False(t, unvResp.Association.Authorized)
	assert.False(t, unvResp.Association.Verified)
	assert.Equal(t, githubAppErrAssociationUnverified, unvResp.Association.ErrorCode)
	assert.Equal(t, githubAppStatusMessage(githubAppErrAssociationCheckFailed), unvResp.Association.Reason)

	// Cached report entry with association_check_failed + internal error text must be sanitized to fixed text.
	srv.githubAssociationReportCache.mu.Lock()
	srv.githubAssociationReportCache.valid = true
	srv.githubAssociationReportCache.generatedAt = now
	srv.githubAssociationReportCache.entries = map[string]githubAssociationReportEntry{
		proj.ID: {
			ProjectID:      proj.ID,
			InstallationID: instID,
			Authorized:     false,
			ErrorCode:      githubAppErrAssociationCheckFailed,
			Reason:         "internal sqlite secret lookup boom: sensitive detail",
		},
	}
	srv.githubAssociationReportCache.dirty = nil
	srv.githubAssociationReportCache.mu.Unlock()

	recSanitized := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recSanitized.Code, recSanitized.Body.String())
	var sanResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recSanitized.Body.Bytes(), &sanResp))
	assert.False(t, sanResp.Association.Verified)
	assert.Equal(t, githubAppErrAssociationUnverified, sanResp.Association.ErrorCode)
	assert.NotContains(t, sanResp.Association.Reason, "sensitive detail")
	assert.Equal(t, githubAppStatusMessage(githubAppErrAssociationCheckFailed), sanResp.Association.Reason)

	// Missing creator token yields explicit unverified state (not plain unauthorized),
	// both when the association report cache is cold AND after the report cache is warmed.
	noTokProj := &store.Project{
		ID:                   tid("pg-no-creator-tok"),
		Name:                 "pg-no-creator-tok",
		Slug:                 tid("pg-no-creator-tok"),
		GitRemote:            fakeAPI.srv.URL + "/acme/no-tok-repo",
		CreatedBy:            admin.ID, // admin has no stored GITHUB_TOKEN
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, noTokProj))
	recNoTok := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+noTokProj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recNoTok.Code, recNoTok.Body.String())
	var noTokResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recNoTok.Body.Bytes(), &noTokResp))
	assert.False(t, noTokResp.Association.Authorized)
	assert.False(t, noTokResp.Association.Verified)
	assert.Equal(t, githubAppErrAssociationUnverified, noTokResp.Association.ErrorCode)

	// Warm the association report cache via githubAssociationReport(ctx, false) and
	// verify GET /github still returns verified=false, association_unverified.
	fakeAPI.mu.Lock()
	fakeAPI.repoStatus = 0
	fakeAPI.mu.Unlock()
	_, _, err = srv.githubAssociationReport(ctx, false)
	require.NoError(t, err)

	recNoTokWarm := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+noTokProj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recNoTokWarm.Code, recNoTokWarm.Body.String())
	var noTokWarmResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recNoTokWarm.Body.Bytes(), &noTokWarmResp))
	assert.False(t, noTokWarmResp.Association.Authorized)
	assert.False(t, noTokWarmResp.Association.Verified)
	assert.Equal(t, githubAppErrAssociationUnverified, noTokWarmResp.Association.ErrorCode)
	assert.Equal(t, noTokResp.Association.Reason, noTokWarmResp.Association.Reason)

	// Definitively denied creator: cold-cache GET and report-warm GET must match
	// and preserve the specific denial reason.
	deniedProj := &store.Project{
		ID:                   tid("pg-denied-creator-proj"),
		Name:                 "pg-denied-creator-proj",
		Slug:                 tid("pg-denied-creator-proj"),
		GitRemote:            fakeAPI.srv.URL + "/acme/denied-repo",
		CreatedBy:            creator.ID, // has ghp_cold_push, which is NOT granted on acme/denied-repo
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, deniedProj))

	recDeniedCold := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+deniedProj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recDeniedCold.Code, recDeniedCold.Body.String())
	var deniedColdResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recDeniedCold.Body.Bytes(), &deniedColdResp))
	assert.False(t, deniedColdResp.Association.Authorized)
	assert.True(t, deniedColdResp.Association.Verified)
	assert.Equal(t, githubAppErrAssociationUnauthorized, deniedColdResp.Association.ErrorCode)
	assert.Contains(t, deniedColdResp.Association.Reason, "acme/denied-repo")

	// Force-rebuild the report cache so deniedProj has a fresh non-authorized entry.
	_, _, err = srv.githubAssociationReport(ctx, true)
	require.NoError(t, err)

	recDeniedWarm := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+deniedProj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recDeniedWarm.Code, recDeniedWarm.Body.String())
	var deniedWarmResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recDeniedWarm.Body.Bytes(), &deniedWarmResp))
	assert.Equal(t, deniedColdResp.Association.Authorized, deniedWarmResp.Association.Authorized)
	assert.Equal(t, deniedColdResp.Association.Verified, deniedWarmResp.Association.Verified)
	assert.Equal(t, deniedColdResp.Association.ErrorCode, deniedWarmResp.Association.ErrorCode)
	assert.Equal(t, deniedColdResp.Association.Reason, deniedWarmResp.Association.Reason)
}

func TestProjectGitHub_EffectivePermissionsMatchMint(t *testing.T) {
	t.Run("default mode on full App grant", func(t *testing.T) {
		fakeAPI := newProjectGitHubFakeAPI(t, "full-app", map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"metadata":      "read",
		})
		srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
		ctx := context.Background()

		require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: 8001,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          1,
			Status:         store.GitHubInstallationStatusActive,
		}))
		proj := &store.Project{
			ID:        tid("pg-eff-full"),
			Name:      "pg-eff-full",
			Slug:      tid("pg-eff-full"),
			GitRemote: fakeAPI.srv.URL + "/acme/full-repo",
			CreatedBy: admin.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, st.CreateProject(ctx, proj))
		bindRec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/github-installation", map[string]int64{"installation_id": 8001})
		require.Equal(t, http.StatusOK, bindRec.Code, bindRec.Body.String())

		reloaded, err := st.GetProject(ctx, proj.ID)
		require.NoError(t, err)
		_, _, err = srv.mintGitHubAppToken(ctx, reloaded)
		require.NoError(t, err)
		mintedPerms := fakeAPI.lastMintPerms()
		require.NotEmpty(t, mintedPerms)

		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ProjectGitHubResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "default", resp.Permissions.Mode)
		assert.Equal(t, mintedPerms, resp.Permissions.Effective)
		assert.Empty(t, resp.Permissions.Capped)
		assert.Empty(t, resp.Permissions.CapReasons)
	})

	t.Run("default mode on git-only App grant", func(t *testing.T) {
		fakeAPI := newProjectGitHubFakeAPI(t, "git-only-app", map[string]string{
			"contents": "write",
		})
		srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
		ctx := context.Background()

		require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: 8002,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          1,
			Status:         store.GitHubInstallationStatusActive,
		}))
		proj := &store.Project{
			ID:        tid("pg-eff-gitonly"),
			Name:      "pg-eff-gitonly",
			Slug:      tid("pg-eff-gitonly"),
			GitRemote: fakeAPI.srv.URL + "/acme/gitonly-repo",
			CreatedBy: admin.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, st.CreateProject(ctx, proj))
		bindRec := doRequestAsUser(t, srv, admin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/github-installation", map[string]int64{"installation_id": 8002})
		require.Equal(t, http.StatusOK, bindRec.Code, bindRec.Body.String())

		reloaded, err := st.GetProject(ctx, proj.ID)
		require.NoError(t, err)
		_, _, err = srv.mintGitHubAppToken(ctx, reloaded)
		require.NoError(t, err)
		mintedPerms := fakeAPI.lastMintPerms()
		require.Equal(t, map[string]string{"contents": "write"}, mintedPerms)

		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ProjectGitHubResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "default", resp.Permissions.Mode)
		assert.Equal(t, mintedPerms, resp.Permissions.Effective)
		assert.Equal(t, []string{"pull_requests", "issues", "metadata"}, resp.Permissions.Capped)
		assert.Equal(t, map[string]string{
			"pull_requests": "app_grant",
			"issues":        "app_grant",
			"metadata":      "app_grant",
		}, resp.Permissions.CapReasons)
	})

	t.Run("custom contents write on creator-pull association", func(t *testing.T) {
		fakeAPI := newProjectGitHubFakeAPI(t, "full-app", map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"metadata":      "read",
		})
		srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
		ctx := context.Background()

		require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: 8003,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          1,
			Status:         store.GitHubInstallationStatusActive,
		}))

		creator := &store.User{
			ID:          tid("pg-pull-creator"),
			Email:       "pg-pull-creator@example.com",
			DisplayName: "Pull Creator",
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
		}
		require.NoError(t, st.CreateUser(ctx, creator))
		require.NoError(t, st.CreateSecret(ctx, &store.Secret{
			ID:             tid("pg-pull-creator-tok"),
			Key:            "GITHUB_TOKEN",
			EncryptedValue: "ghp_pull_only",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "GITHUB_TOKEN",
			Scope:          store.ScopeUser,
			ScopeID:        creator.ID,
		}))
		srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))
		fakeAPI.grant("ghp_pull_only", "acme/pull-repo", githubapp.RepoAccess{Pull: true, Push: false})

		instID := int64(8003)
		proj := &store.Project{
			ID:                   tid("pg-eff-pull"),
			Name:                 "pg-eff-pull",
			Slug:                 tid("pg-eff-pull"),
			GitRemote:            fakeAPI.srv.URL + "/acme/pull-repo",
			CreatedBy:            creator.ID,
			GitHubInstallationID: &instID,
			GitHubPermissions:    &store.GitHubTokenPermissions{Contents: "write"},
			Created:              time.Now(),
			Updated:              time.Now(),
		}
		require.NoError(t, st.CreateProject(ctx, proj))

		// Minting populates the access cache and exercises mintGitHubAppTokenInternal.
		_, _, err := srv.mintGitHubAppToken(ctx, proj)
		require.NoError(t, err)
		mintedPerms := fakeAPI.lastMintPerms()
		require.Equal(t, map[string]string{"contents": "read"}, mintedPerms)

		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ProjectGitHubResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Association.Basis)
		assert.Equal(t, githubAssociationBasisCreatorPull, *resp.Association.Basis)
		assert.False(t, resp.Association.WriteAllowed)
		assert.Equal(t, "custom", resp.Permissions.Mode)
		assert.Equal(t, mintedPerms, resp.Permissions.Effective)
		assert.Equal(t, []string{"contents"}, resp.Permissions.Capped)
		assert.Equal(t, map[string]string{"contents": "association_read_only"}, resp.Permissions.CapReasons)
	})
}

func TestProjectGitHub_CreatorCheckBoundedConcurrencyAndNegativeCache(t *testing.T) {
	fakeAPI := newProjectGitHubFakeAPI(t, "bounded-app", map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"issues":        "write",
		"metadata":      "read",
	})
	srv, st, admin := setupProjectGitHubTestHub(t, fakeAPI)
	ctx := context.Background()

	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 8301,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))

	creator := &store.User{
		ID:          tid("pg-bounded-creator"),
		Email:       "pg-bounded-creator@example.com",
		DisplayName: "Bounded Creator",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, st.CreateUser(ctx, creator))
	require.NoError(t, st.CreateSecret(ctx, &store.Secret{
		ID:             tid("pg-bounded-creator-tok"),
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_bounded_push",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        creator.ID,
	}))
	srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))

	instID := int64(8301)
	proj := &store.Project{
		ID:                   tid("pg-bounded-proj"),
		Name:                 "pg-bounded-proj",
		Slug:                 tid("pg-bounded-proj"),
		GitRemote:            fakeAPI.srv.URL + "/acme/bounded-repo",
		CreatedBy:            creator.ID,
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	// Use a mutex-protected fake clock so concurrent singleflight goroutines
	// are race-free under -race.
	var clockMu sync.Mutex
	now := time.Now().UTC()
	origTimeNow := timeNow
	timeNow = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	t.Cleanup(func() { timeNow = origTimeNow })
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		admin.ID, admin.Email, admin.DisplayName, admin.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	var calls atomic.Int32
	origCheck := srv.githubRepoAccessCheck
	t.Cleanup(func() { srv.githubRepoAccessCheck = origCheck })

	// 1. N concurrent cold GETs collapse into a single creator-token check.
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		return githubapp.RepoAccess{Pull: true, Push: true}, nil
	}

	const concurrency = 10
	type getResult struct {
		code int
		resp ProjectGitHubResponse
		err  error
	}
	results := make([]getResult, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			var r ProjectGitHubResponse
			uerr := json.Unmarshal(rec.Body.Bytes(), &r)
			results[idx] = getResult{code: rec.Code, resp: r, err: uerr}
		}(i)
	}
	<-started
	time.Sleep(25 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load(), "concurrent cold GETs must collapse into 1 creator-token call")
	for i, res := range results {
		require.NoError(t, res.err, "request %d decode", i)
		require.Equal(t, http.StatusOK, res.code, "request %d status", i)
		assert.True(t, res.resp.Association.Authorized, "request %d authorized", i)
		assert.True(t, res.resp.Association.Verified, "request %d verified", i)
	}

	// 2. N sequential GETs after an inconclusive error make only 1 creator-token
	// call within the negative-cache TTL, while minting still makes its own live call.
	advance(11 * time.Minute)
	srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		calls.Add(1)
		return githubapp.RepoAccess{}, errors.New("transient github 503")
	}

	for i := 0; i < 5; i++ {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ProjectGitHubResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.Association.Authorized, "sequential GET %d authorized", i)
		assert.False(t, resp.Association.Verified, "sequential GET %d verified", i)
		assert.Equal(t, githubAppErrAssociationUnverified, resp.Association.ErrorCode)
	}
	assert.Equal(t, int32(2), calls.Load(), "5 sequential GETs after inconclusive error must share 1 call within TTL")

	// Mint path is unchanged: bypasses the read-model negative cache and calls live.
	_, _, mintErr := srv.mintGitHubAppToken(ctx, proj)
	require.Error(t, mintErr)
	assert.Equal(t, int32(3), calls.Load(), "mint must still make its own live creator-token check")

	// After the negative-cache TTL expires, GET retries and recovers.
	advance(githubReadModelInconclusiveTTL + time.Second)
	srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		calls.Add(1)
		return githubapp.RepoAccess{Pull: true, Push: true}, nil
	}
	recRecovered := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recRecovered.Code, recRecovered.Body.String())
	var recovResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recRecovered.Body.Bytes(), &recovResp))
	assert.True(t, recovResp.Association.Authorized)
	assert.True(t, recovResp.Association.Verified)
	assert.Equal(t, int32(4), calls.Load(), "GET after negative-cache TTL expiry should retry once")

	// 3. Cancelling the caller's request context does not cancel the detached
	// creator-access check, so the check completes and populates the access cache.
	advance(11 * time.Minute)
	detachStarted := make(chan struct{})
	detachRelease := make(chan struct{})
	var detachOnce sync.Once
	var checkCtxErr atomic.Value
	srv.githubRepoAccessCheck = func(checkCtx context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		calls.Add(1)
		detachOnce.Do(func() { close(detachStarted) })
		<-detachRelease
		if err := checkCtx.Err(); err != nil {
			checkCtxErr.Store(err)
		}
		return githubapp.RepoAccess{Pull: true, Push: true}, nil
	}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil).WithContext(reqCtx)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
	}()

	<-detachStarted
	cancelReq()
	time.Sleep(15 * time.Millisecond)
	close(detachRelease)
	<-reqDone

	assert.Nil(t, checkCtxErr.Load(), "cancelling caller request context must not cancel the creator-access check")
	assert.Equal(t, int32(5), calls.Load())

	// Subsequent GET hits the warm access cache populated by the detached check.
	recWarm := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recWarm.Code, recWarm.Body.String())
	var warmResp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recWarm.Body.Bytes(), &warmResp))
	assert.True(t, warmResp.Association.Authorized)
	assert.True(t, warmResp.Association.Verified)
	assert.Equal(t, int32(5), calls.Load(), "warm cache after detached check must require 0 additional calls")
}
