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
	"net/http/httptest"
	"strings"
	"sync"
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
		perms := make(map[string]string, len(f.appPermissions))
		for k, v := range f.appPermissions {
			perms[k] = v
		}
		s := f.appSlug
		f.mu.Unlock()
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
	assert.Equal(t, admin.ID, resp.Association.BoundBy)
	assert.NotEmpty(t, resp.Association.BoundAt)
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
	require.NoError(t, st.CreateUser(ctx, projAdmin))
	require.NoError(t, st.CreateUser(ctx, outsider))

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

	// Give projAdmin the project-admin role on proj (not hub admin).
	if roleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject); err == nil && roleDef != nil {
		_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: roleDef.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      projAdmin.ID,
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

	// Non-hub-admin project admin gets 200 with can.bind=false, can.edit_permissions=true, can.discover=false.
	recMember := doRequestAsUser(t, srv, projAdmin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recMember.Code, recMember.Body.String())
	var resp ProjectGitHubResponse
	require.NoError(t, json.Unmarshal(recMember.Body.Bytes(), &resp))
	assert.False(t, resp.Can.Bind, "non-hub-admin must not have can.bind")
	assert.False(t, resp.Can.Discover, "non-hub-admin must not have can.discover")
	assert.True(t, resp.Can.EditPermissions, "project admin should have can.edit_permissions")

	// Non-admin PUT bind still returns 403.
	recPut := doRequestAsUser(t, srv, projAdmin, http.MethodPut, "/api/v1/projects/"+proj.ID+"/github-installation", map[string]int64{
		"installation_id": 7711,
	})
	assert.Equal(t, http.StatusForbidden, recPut.Code, "non-hub-admin PUT bind must be 403: %s", recPut.Body.String())

	// Hub admin gets can.bind=true.
	recAdmin := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects/"+proj.ID+"/github", nil)
	require.Equal(t, http.StatusOK, recAdmin.Code, recAdmin.Body.String())
	require.NoError(t, json.Unmarshal(recAdmin.Body.Bytes(), &resp))
	assert.True(t, resp.Can.Bind)
	assert.True(t, resp.Can.Discover)
	assert.True(t, resp.Can.EditPermissions)
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
