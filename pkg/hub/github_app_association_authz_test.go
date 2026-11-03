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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// --- fixtures ----------------------------------------------------------------

// fakeGitHubForAssoc is a fake GitHub API: GET /repos/{owner}/{repo} answers
// from per-token permissions (the creator access check), and POST
// /app/installations/{id}/access_tokens records each mint request.
type fakeGitHubForAssoc struct {
	srv *httptest.Server

	mu         sync.Mutex
	access     map[string]map[string]githubapp.RepoAccess // token -> lower owner/repo -> access
	repoStatus int                                        // when set, /repos answers this status
	repoCalls  int
	mints      []fakeMintRequest
}

type fakeMintRequest struct {
	InstallationID string
	Repositories   []string          `json:"repositories"`
	Permissions    map[string]string `json:"permissions"`
}

func newFakeGitHubForAssoc(t *testing.T) *fakeGitHubForAssoc {
	t.Helper()
	f := &fakeGitHubForAssoc{access: map[string]map[string]githubapp.RepoAccess{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.repoCalls++
		if f.repoStatus != 0 {
			w.WriteHeader(f.repoStatus)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ownerRepo := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/repos/"))
		a, ok := f.access[token][ownerRepo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"full_name": ownerRepo, "permissions": a})
	})
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		var req fakeMintRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		req.InstallationID = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		f.mu.Lock()
		f.mints = append(f.mints, req)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_minted_installation_token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// grant lets the user who owns token see ownerRepo with access.
func (f *fakeGitHubForAssoc) grant(token, ownerRepo string, access githubapp.RepoAccess) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access[token] == nil {
		f.access[token] = map[string]githubapp.RepoAccess{}
	}
	f.access[token][strings.ToLower(ownerRepo)] = access
}

func (f *fakeGitHubForAssoc) setRepoStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoStatus = status
}

func (f *fakeGitHubForAssoc) counts() (repoCalls int, mints []fakeMintRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoCalls, append([]fakeMintRequest(nil), f.mints...)
}

// remote returns an https remote for owner/repo on the fake's host, which
// is the GitHub host the server derives from the fake's API base URL.
func (f *fakeGitHubForAssoc) remote(owner, repo string) string {
	return f.srv.URL + "/" + owner + "/" + repo
}

// configureFakeGitHubApp points srv's GitHub App at f with a usable key.
func configureFakeGitHubApp(t *testing.T, srv *Server, f *fakeGitHubForAssoc) {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.config.GitHubAppConfig.APIBaseURL = f.srv.URL
	srv.config.GitHubAppConfig.AppID = 1
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
}

// giveUserGitHubToken stores token as userID's user-scope GITHUB_TOKEN and
// installs a local secret backend over st.
func giveUserGitHubToken(t *testing.T, srv *Server, st store.Store, userID, token string) {
	t.Helper()
	require.NoError(t, st.CreateSecret(context.Background(), &store.Secret{
		ID:             tid("gh-user-token-" + userID),
		Key:            "GITHUB_TOKEN",
		EncryptedValue: token,
		SecretType:     store.SecretTypeEnvironment,
		Target:         "GITHUB_TOKEN",
		Scope:          store.ScopeUser,
		ScopeID:        userID,
	}))
	srv.SetSecretBackend(secret.NewLocalBackend(st, "test-hub-id", "test-secret"))
}

// fakeCreatorAccess answers creator access checks without a GitHub server,
// for tests whose remotes are on github.com.
func fakeCreatorAccess(srv *Server, byTokenRepo map[string]githubapp.RepoAccess) *int {
	calls := new(int)
	var mu sync.Mutex
	srv.githubRepoAccessCheck = func(_ context.Context, _, token, owner, repo string) (githubapp.RepoAccess, error) {
		mu.Lock()
		*calls++
		mu.Unlock()
		a, ok := byTokenRepo[token+"|"+strings.ToLower(owner+"/"+repo)]
		if !ok {
			return githubapp.RepoAccess{}, githubapp.ErrRepoAccessDenied
		}
		return a, nil
	}
	return calls
}

func createAssocInstallation(t *testing.T, st store.Store, id int64, account string, repos ...string) {
	t.Helper()
	require.NoError(t, st.CreateGitHubInstallation(context.Background(), &store.GitHubInstallation{
		InstallationID: id,
		AccountLogin:   account,
		AccountType:    "Organization",
		AppID:          1,
		Repositories:   repos,
		Status:         store.GitHubInstallationStatusActive,
	}))
}

func createAssocProject(t *testing.T, st store.Store, id, remote, createdBy string, installationID *int64) *store.Project {
	t.Helper()
	p := &store.Project{
		ID: tid(id), Name: id, Slug: tid(id),
		GitRemote: remote, CreatedBy: createdBy,
		GitHubInstallationID: installationID,
		Created:              time.Now(), Updated: time.Now(),
	}
	require.NoError(t, st.CreateProject(context.Background(), p))
	return p
}

// createAssocAdmin creates an active user holding the system super-admin
// role binding, which grants the installation binding permission.
func createAssocAdmin(t *testing.T, st store.Store, id string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: tid(id), Email: id + "@test.com", DisplayName: id,
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, u))
	rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	return u
}

// setAdminBinding records an admin binding on p directly in the store, as
// handleProjectGitHubInstallation does.
func setAdminBinding(t *testing.T, st store.Store, p *store.Project, installationID int64, remote, boundBy string) {
	t.Helper()
	raw, err := json.Marshal(githubInstallationBinding{
		InstallationID: installationID, Remote: remote, BoundBy: boundBy, BoundAt: time.Now(),
	})
	require.NoError(t, err)
	stored, err := st.GetProject(context.Background(), p.ID)
	require.NoError(t, err)
	if stored.Annotations == nil {
		stored.Annotations = map[string]string{}
	}
	stored.Annotations[githubInstallationBindingAnnotation] = string(raw)
	require.NoError(t, st.UpdateProject(context.Background(), stored))
	*p = *stored
}

func reloadProject(t *testing.T, st store.Store, id string) *store.Project {
	t.Helper()
	p, err := st.GetProject(context.Background(), id)
	require.NoError(t, err)
	return p
}

func ptrInt64(v int64) *int64 { return &v }

// --- association: a creator without access gets nothing --------------------

// A creator who names another organization's repository, and whose own
// GitHub identity cannot see it, gets no association at create time, from
// the installation webhook or from discovery, and no token when an
// association exists anyway. Revert check: before the fix, the owner/repo
// string match alone associated the project and the mint succeeded.
func TestGitHubAssociation_NonMemberCreatorNoAssociationNoToken(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5101, "other-org", "other-org/private-repo")
	giveUserGitHubToken(t, srv, st, "outsider", "ghp_outsider")
	// The outsider's token cannot see the repository: /repos answers 404.

	p := createAssocProject(t, st, "assoc-nonmember", gh.remote("other-org", "private-repo"), "outsider", nil)

	srv.autoAssociateGitHubInstallation(ctx, p)
	assert.Nil(t, p.GitHubInstallationID, "create-time auto-association must not associate")
	assert.Nil(t, reloadProject(t, st, p.ID).GitHubInstallationID)

	matched := srv.matchProjectsToInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 5101, AccountLogin: "other-org", Repositories: []string{"other-org/private-repo"},
	})
	assert.Empty(t, matched, "webhook/discovery matching must not associate")
	assert.Nil(t, reloadProject(t, st, p.ID).GitHubInstallationID)

	// Force the association (as a pre-fix record would be): no token.
	stored := reloadProject(t, st, p.ID)
	stored.GitHubInstallationID = ptrInt64(5101)
	require.NoError(t, st.UpdateProject(ctx, stored))

	token, _, err := srv.MintGitHubAppTokenForProject(ctx, stored)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errGitHubAssociationUnauthorized), "err = %v", err)
	assert.Empty(t, token)
	_, mints := gh.counts()
	assert.Empty(t, mints, "no installation token may be requested")

	status := reloadProject(t, st, p.ID).GitHubAppStatus
	require.NotNil(t, status)
	assert.Equal(t, store.GitHubAppStateError, status.State)
	assert.Equal(t, githubAppErrAssociationUnauthorized, status.ErrorCode)
}

// A pre-existing association with no admin binding and a creator with no
// GitHub token is refused at mint (pre-fix associations fail closed with no
// migration). Revert check: before the fix, this minted a token.
func TestGitHubAssociation_PreExistingUnauthorizedAssociationNoTokenAtMint(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5102, "acme", "acme/widgets")
	p := createAssocProject(t, st, "assoc-prefix", gh.remote("acme", "widgets"), "legacy-creator", ptrInt64(5102))

	token, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
	require.Error(t, err)
	assert.Empty(t, token)
	repoCalls, mints := gh.counts()
	assert.Empty(t, mints)
	assert.Zero(t, repoCalls, "no creator token: GitHub is not even asked")
	assert.Equal(t, githubAppErrAssociationUnauthorized, reloadProject(t, st, p.ID).GitHubAppStatus.ErrorCode)

	// The skill path (direct mintGitHubAppToken caller) is refused too.
	_, _, err = srv.resolveGitHubToken(ctx, p.ID)
	require.Error(t, err)
	_, mints = gh.counts()
	assert.Empty(t, mints)
}

// --- host match ---------------------------------------------------------------

// A remote on any host other than the configured GitHub host never
// associates, even when an installation lists the same owner/repo. Revert
// check: before the fix the host was ignored.
func TestGitHubAssociation_NonGitHubHostNoAssociation(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	calls := fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
		"ghp_alice|acme/widgets": {Push: true, Pull: true},
	})
	giveUserGitHubToken(t, srv, st, "alice", "ghp_alice")
	createAssocInstallation(t, st, 5201, "acme", "acme/widgets")

	for i, remote := range []string{
		"https://gitlab.com/acme/widgets.git",
		"git@gitlab.com:acme/widgets.git",
		"https://github.com.other.example/acme/widgets",
		"https://github.com:8443/acme/widgets",
	} {
		p := createAssocProject(t, st, fmt.Sprintf("assoc-host-%d", i), remote, "alice", nil)
		srv.autoAssociateGitHubInstallation(ctx, p)
		assert.Nil(t, p.GitHubInstallationID, "remote %s must not associate", remote)
	}
	matched := srv.matchProjectsToInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 5201, AccountLogin: "acme", Repositories: []string{"acme/widgets"},
	})
	assert.Empty(t, matched)
	assert.Zero(t, *calls, "no access check for a remote on another host")
}

// A token is not minted for a project whose remote host does not match the
// GitHub host, even with an admin binding. Revert check: before the fix the
// mint used only the owner/repo path.
func TestGitHubAssociation_MismatchedHostNoToken(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)
	admin := createAssocAdmin(t, st, "assoc-host-admin")

	createAssocInstallation(t, st, 5202, "acme", "acme/widgets")
	remote := "https://gitlab.com/acme/widgets"
	p := createAssocProject(t, st, "assoc-host-mint", remote, "alice", ptrInt64(5202))
	setAdminBinding(t, st, p, 5202, "gitlab.com/acme/widgets", admin.ID)

	token, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
	require.Error(t, err)
	assert.Empty(t, token)
	_, mints := gh.counts()
	assert.Empty(t, mints)
}

// --- coverage: installation account must own the repository ------------------

// An installation of orgA cannot mint for a project whose remote is
// orgB/foo, even with an admin binding: the mint scopes by bare repository
// name, so this would otherwise hand out orgA/foo. Revert check: removing the
// account check mints.
func TestGitHubAssociation_InstallationAccountMustOwnRepo(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)
	admin := createAssocAdmin(t, st, "assoc-cov-admin")

	createAssocInstallation(t, st, 5301, "org-a", "org-a/foo")
	remote := gh.remote("org-b", "foo")
	p := createAssocProject(t, st, "assoc-cov", remote, "alice", ptrInt64(5301))
	setAdminBinding(t, st, p, 5301, normalizedRemoteForTest(remote), admin.ID)

	token, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
	require.Error(t, err)
	assert.Empty(t, token)
	_, mints := gh.counts()
	assert.Empty(t, mints)
}

// --- admin binding --------------------------------------------------------------

// A hub admin binds an installation; the binding is recorded and minting
// works without any creator token, repository-scoped with the requested
// (write) permissions. A non-admin project owner cannot bind.
func TestGitHubAssociation_AdminBindingWorks(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5401, "acme", "acme/widgets")
	p := createAssocProject(t, st, "assoc-admin-bind", gh.remote("acme", "widgets"), "nobody", nil)

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+p.ID+"/github-installation",
		map[string]int64{"installation_id": 5401})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	bound := reloadProject(t, st, p.ID)
	require.NotNil(t, bound.GitHubInstallationID)
	assert.Equal(t, int64(5401), *bound.GitHubInstallationID)
	require.NotEmpty(t, bound.Annotations[githubInstallationBindingAnnotation], "binding recorded")

	token, _, err := srv.MintGitHubAppTokenForProject(ctx, bound)
	require.NoError(t, err)
	assert.Equal(t, "ghs_minted_installation_token", token)
	repoCalls, mints := gh.counts()
	assert.Zero(t, repoCalls, "an admin binding needs no creator access check")
	require.Len(t, mints, 1)
	assert.Equal(t, "5401", mints[0].InstallationID)
	assert.Equal(t, []string{"widgets"}, mints[0].Repositories, "repository-scoped, never installation-wide")
	assert.Equal(t, "write", mints[0].Permissions["contents"])

	// DELETE clears the association and the binding.
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+p.ID+"/github-installation", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	cleared := reloadProject(t, st, p.ID)
	assert.Nil(t, cleared.GitHubInstallationID)
	assert.Empty(t, cleared.Annotations[githubInstallationBindingAnnotation])
}

// The PUT binding refuses a remote on another host and an installation of
// another account, even for an admin.
func TestGitHubAssociation_AdminBindingValidatesHostAndAccount(t *testing.T) {
	srv, st := testServer(t)
	createAssocInstallation(t, st, 5402, "acme", "acme/widgets")

	other := createAssocProject(t, st, "assoc-bind-host", "https://gitlab.com/acme/widgets", "nobody", nil)
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+other.ID+"/github-installation",
		map[string]int64{"installation_id": 5402})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
	assert.Nil(t, reloadProject(t, st, other.ID).GitHubInstallationID)

	wrongOwner := createAssocProject(t, st, "assoc-bind-owner", "https://github.com/someone-else/widgets", "nobody", nil)
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+wrongOwner.ID+"/github-installation",
		map[string]int64{"installation_id": 5402})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
	assert.Nil(t, reloadProject(t, st, wrongOwner.ID).GitHubInstallationID)
}

// Binding is admin-only: a project owner who is not a hub admin is refused
// and nothing changes. Revert check: before the fix project update access
// was enough.
func TestGitHubAssociation_NonAdminOwnerCannotBind(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	createAssocInstallation(t, s, 5403, "acme", "acme/widgets")
	stored := reloadProject(t, s, project.ID)
	stored.GitRemote = "https://github.com/acme/widgets"
	require.NoError(t, s.UpdateProject(ctx, stored))

	rec := doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/projects/"+project.ID+"/github-installation",
		map[string]int64{"installation_id": 5403})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	after := reloadProject(t, s, project.ID)
	assert.Nil(t, after.GitHubInstallationID)
	assert.Empty(t, after.Annotations[githubInstallationBindingAnnotation])
}

// A binding stops authorizing when the remote no longer matches, when it
// names another installation, or when the binding admin is no longer an
// active admin.
func TestGitHubAssociation_AdminBindingRevalidatedAtMint(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)
	admin := createAssocAdmin(t, st, "assoc-reval-admin")
	createAssocInstallation(t, st, 5404, "acme", "acme/widgets", "acme/other")

	remote := gh.remote("acme", "widgets")

	t.Run("binding for another remote", func(t *testing.T) {
		p := createAssocProject(t, st, "assoc-reval-remote", remote, "nobody", ptrInt64(5404))
		setAdminBinding(t, st, p, 5404, normalizedRemoteForTest(gh.remote("acme", "other")), admin.ID)
		_, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
		require.Error(t, err)
	})
	t.Run("binding for another installation", func(t *testing.T) {
		p := createAssocProject(t, st, "assoc-reval-inst", remote, "nobody", ptrInt64(5404))
		setAdminBinding(t, st, p, 9999, normalizedRemoteForTest(remote), admin.ID)
		_, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
		require.Error(t, err)
	})
	t.Run("binding by an unknown user", func(t *testing.T) {
		p := createAssocProject(t, st, "assoc-reval-unknown", remote, "nobody", ptrInt64(5404))
		setAdminBinding(t, st, p, 5404, normalizedRemoteForTest(remote), "not-a-user")
		_, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
		require.Error(t, err)
	})
	t.Run("binding by a non-admin member", func(t *testing.T) {
		member := &store.User{ID: tid("assoc-reval-member"), Email: "m@test.com", DisplayName: "m",
			Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
		require.NoError(t, st.CreateUser(ctx, member))
		p := createAssocProject(t, st, "assoc-reval-member", remote, "nobody", ptrInt64(5404))
		setAdminBinding(t, st, p, 5404, normalizedRemoteForTest(remote), member.ID)
		_, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
		require.Error(t, err)
	})
	t.Run("binding admin no longer active", func(t *testing.T) {
		former := createAssocAdmin(t, st, "assoc-reval-former")
		p := createAssocProject(t, st, "assoc-reval-former", remote, "nobody", ptrInt64(5404))
		setAdminBinding(t, st, p, 5404, normalizedRemoteForTest(remote), former.ID)
		_, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
		require.NoError(t, err, "valid binding mints")

		former.Status = store.UserStatusSuspended
		require.NoError(t, st.UpdateUser(ctx, former))
		_, _, err = srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, p.ID))
		require.Error(t, err, "a binding by a user who is no longer an active admin no longer authorizes")
	})
	_, mints := gh.counts()
	assert.Len(t, mints, 1, "only the valid binding minted")
}

// --- creator access proof -----------------------------------------------------

// A creator whose own token has push access gets the association at create
// time and a write-capable, repository-scoped token at mint.
func TestGitHubAssociation_CreatorWithPushAccessWorks(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5501, "acme", "acme/widgets")
	giveUserGitHubToken(t, srv, st, "alice", "ghp_alice")
	gh.grant("ghp_alice", "acme/widgets", githubapp.RepoAccess{Push: true, Pull: true})

	p := createAssocProject(t, st, "assoc-creator-push", gh.remote("acme", "widgets"), "alice", nil)
	srv.autoAssociateGitHubInstallation(ctx, p)
	require.NotNil(t, p.GitHubInstallationID)
	assert.Equal(t, int64(5501), *p.GitHubInstallationID)

	token, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, p.ID))
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	_, mints := gh.counts()
	require.Len(t, mints, 1)
	assert.Equal(t, []string{"widgets"}, mints[0].Repositories)
	assert.Equal(t, "write", mints[0].Permissions["contents"])
	assert.Equal(t, "write", mints[0].Permissions["pull_requests"])
}

// A pull-only proof (for example a public repository the creator cannot
// push to) yields a read-only token: the minted permissions never exceed
// what the creator proved. Revert check: without the scoping the token was
// write-capable.
func TestGitHubAssociation_CreatorPullOnlyGetsReadOnlyToken(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5502, "acme", "acme/public")
	giveUserGitHubToken(t, srv, st, "bob", "ghp_bob")
	gh.grant("ghp_bob", "acme/public", githubapp.RepoAccess{Pull: true})

	p := createAssocProject(t, st, "assoc-creator-pull", gh.remote("acme", "public"), "bob", ptrInt64(5502))
	token, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	_, mints := gh.counts()
	require.Len(t, mints, 1)
	require.NotEmpty(t, mints[0].Permissions)
	for name, level := range mints[0].Permissions {
		assert.Equal(t, "read", level, "permission %s must be read-only", name)
	}
}

// An inconclusive access check (GitHub 5xx or a rate limit) mints nothing,
// is surfaced as association_check_failed, and is not cached.
func TestGitHubAssociation_InconclusiveCheckFailsClosed(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5503, "acme", "acme/widgets")
	giveUserGitHubToken(t, srv, st, "carol", "ghp_carol")
	gh.grant("ghp_carol", "acme/widgets", githubapp.RepoAccess{Push: true, Pull: true})
	gh.setRepoStatus(http.StatusBadGateway)

	p := createAssocProject(t, st, "assoc-inconclusive", gh.remote("acme", "widgets"), "carol", ptrInt64(5503))
	token, _, err := srv.MintGitHubAppTokenForProject(ctx, p)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errGitHubAssociationCheckFailed), "err = %v", err)
	assert.Empty(t, token)
	assert.Equal(t, githubAppErrAssociationCheckFailed, reloadProject(t, st, p.ID).GitHubAppStatus.ErrorCode)

	// GitHub recovers: the next mint asks again (nothing was cached) and works.
	gh.setRepoStatus(0)
	token, _, err = srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, p.ID))
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	repoCalls, mints := gh.counts()
	assert.Equal(t, 2, repoCalls)
	assert.Len(t, mints, 1)

	// A grant is cached: another mint does not ask GitHub again.
	_, _, err = srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, p.ID))
	require.NoError(t, err)
	repoCalls, _ = gh.counts()
	assert.Equal(t, 2, repoCalls, "a cached grant is reused")
}

// Webhook/discovery matching associates a project only when its creator
// proves access, never on the repo-name match alone.
func TestGitHubAssociation_MatchProjectsRequiresCreatorAccess(t *testing.T) {
	srv, st := webhookTestServer(t)
	ctx := context.Background()
	fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
		"ghp_alice|acme/widgets": {Push: true, Pull: true},
	})
	giveUserGitHubToken(t, srv, st, "alice", "ghp_alice")
	giveUserGitHubToken(t, srv, st, "outsider", "ghp_outsider")

	ok := createAssocProject(t, st, "assoc-match-ok", "https://github.com/acme/widgets.git", "alice", nil)
	bad := createAssocProject(t, st, "assoc-match-bad", "https://github.com/acme/widgets", "outsider", nil)
	none := createAssocProject(t, st, "assoc-match-none", "https://github.com/acme/widgets", "", nil)

	installation := &store.GitHubInstallation{
		InstallationID: 5601, AccountLogin: "acme", Repositories: []string{"acme/widgets"},
		Status: store.GitHubInstallationStatusActive,
	}
	require.NoError(t, st.CreateGitHubInstallation(ctx, installation))

	matched := srv.matchProjectsToInstallation(ctx, installation)
	assert.Equal(t, []string{ok.ID}, matched)
	assert.NotNil(t, reloadProject(t, st, ok.ID).GitHubInstallationID)
	assert.Nil(t, reloadProject(t, st, bad.ID).GitHubInstallationID)
	assert.Nil(t, reloadProject(t, st, none.ID).GitHubInstallationID)
}

// --- admin report --------------------------------------------------------------

// The admin report lists every associated project with whether it would
// mint now, and changes nothing.
func TestGitHubAssociation_AdminReport(t *testing.T) {
	srv, st := testServer(t)
	admin := createAssocAdmin(t, st, "assoc-report-admin")
	createAssocInstallation(t, st, 5701, "acme", "acme/widgets", "acme/legacy")

	good := createAssocProject(t, st, "assoc-report-good", "https://github.com/acme/widgets", "nobody", ptrInt64(5701))
	setAdminBinding(t, st, good, 5701, "github.com/acme/widgets", admin.ID)
	legacy := createAssocProject(t, st, "assoc-report-legacy", "https://github.com/acme/legacy", "nobody", ptrInt64(5701))
	createAssocProject(t, st, "assoc-report-unassociated", "https://github.com/acme/other", "nobody", nil)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app/installations/association-report", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp struct {
		Associations []githubAssociationReportEntry `json:"associations"`
		Total        int                            `json:"total"`
		Unauthorized int                            `json:"unauthorized"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	byID := map[string]githubAssociationReportEntry{}
	for _, e := range resp.Associations {
		byID[e.ProjectID] = e
	}
	require.Contains(t, byID, good.ID)
	require.Contains(t, byID, legacy.ID)
	assert.True(t, byID[good.ID].Authorized)
	assert.Equal(t, githubAssociationBasisAdminBinding, byID[good.ID].Basis)
	assert.False(t, byID[legacy.ID].Authorized)
	assert.Equal(t, githubAppErrAssociationUnauthorized, byID[legacy.ID].ErrorCode)
	assert.GreaterOrEqual(t, resp.Unauthorized, 1)

	// Read-only: the legacy association is left in place, status untouched.
	after := reloadProject(t, st, legacy.ID)
	require.NotNil(t, after.GitHubInstallationID)
	assert.Nil(t, after.GitHubAppStatus)
}

// --- token-source project label ---------------------------------------------

// recordingSourceMinter records mints and answers source authorization from
// allowSource.
type recordingSourceMinter struct {
	allowSource bool
	authorized  []string
	minted      []string
}

func (m *recordingSourceMinter) MintGitHubAppTokenForProject(_ context.Context, project *store.Project) (string, string, error) {
	m.minted = append(m.minted, project.ID)
	return "github-app-token", "2026-09-14T09:00:00Z", nil
}

func (m *recordingSourceMinter) AuthorizeGitHubTokenSourceProject(_ context.Context, _ *store.Agent, sourceProject *store.Project) error {
	m.authorized = append(m.authorized, sourceProject.ID)
	if !m.allowSource {
		return errGitHubTokenSourceDenied
	}
	return nil
}

// The dispatcher honours the token-source label only when the minter
// authorizes the source for the agent; otherwise no token is minted. Revert
// check: before the fix any label value was honoured.
func TestGitHubTokenSourceLabel_RequiresAuthorization(t *testing.T) {
	ctx := t.Context()
	st := createTestStore(t)
	installID := int64(5801)
	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID, AccountLogin: "other-org", AccountType: "Organization",
		AppID: 1, Status: store.GitHubInstallationStatusActive,
	}))
	source := &store.Project{ID: tid("label-source"), Name: "label-source", Slug: "label-source", GitHubInstallationID: &installID}
	require.NoError(t, st.CreateProject(ctx, source))
	target := &store.Project{ID: tid("label-target"), Name: "label-target", Slug: "label-target"}
	require.NoError(t, st.CreateProject(ctx, target))

	agent := &store.Agent{
		ID:        tid("label-agent"),
		ProjectID: target.ID,
		Labels:    map[string]string{githubTokenSourceProjectLabel: source.ID},
	}

	for _, tc := range []struct {
		name  string
		allow bool
	}{{"denied", false}, {"allowed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			minter := &recordingSourceMinter{allowSource: tc.allow}
			dispatcher := NewHTTPAgentDispatcherWithClient(st, &mockRuntimeBrokerClient{}, false, slog.Default())
			dispatcher.SetGitHubAppMinter(minter)
			env := map[string]string{}
			var classes map[string]api.EnvKind
			dispatcher.injectLifecycleGitHubToken(ctx, agent, env, &classes, "DispatchAgentStart")

			assert.Equal(t, []string{source.ID}, minter.authorized)
			if tc.allow {
				assert.Equal(t, []string{source.ID}, minter.minted)
				assert.Equal(t, "github-app-token", env["GITHUB_TOKEN"])
			} else {
				assert.Empty(t, minter.minted, "no mint for an unauthorized source")
				assert.Empty(t, env["GITHUB_TOKEN"])
			}
		})
	}
}

// Server-side source authorization fails closed: an agent without an
// authoritative source user, or with no ID, is refused.
func TestAuthorizeGitHubTokenSourceProject_FailsClosed(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	source := createAssocProject(t, st, "src-authz-source", "https://github.com/acme/widgets", "nobody", nil)

	err := srv.AuthorizeGitHubTokenSourceProject(ctx, &store.Agent{ProjectID: "x"}, source)
	assert.ErrorIs(t, err, errGitHubTokenSourceDenied)

	err = srv.AuthorizeGitHubTokenSourceProject(ctx, &store.Agent{ID: tid("src-authz-orphan"), ProjectID: tid("src-authz-other")}, source)
	assert.ErrorIs(t, err, errGitHubTokenSourceDenied, "no provenance: no source user")
}

// --- helpers --------------------------------------------------------------------

func normalizedRemoteForTest(remote string) string {
	return util.NormalizeGitRemote(remote)
}

// bindProjectForTest authorizes project's association with installationID
// the way production does for an explicit binding: the remote is set to
// owner/repo on the configured GitHub host (configure APIBaseURL first) and
// the dev user, seeded as super-admin, is recorded as the binding admin.
// The installation must exist with account owner.
func bindProjectForTest(t *testing.T, srv *Server, st store.Store, project *store.Project, installationID int64, owner, repo string) {
	t.Helper()
	remote := "https://" + srv.githubAppWebHost() + "/" + owner + "/" + repo
	stored, err := st.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	stored.GitRemote = remote
	stored.GitHubInstallationID = ptrInt64(installationID)
	require.NoError(t, st.UpdateProject(context.Background(), stored))
	setAdminBinding(t, st, stored, installationID, normalizedRemoteForTest(remote), DevUserID)
	*project = *stored
}

// --- token permissions: never empty, never above the proof -------------------

var allowedGitHubTokenPermissionFields = map[string]bool{
	"contents": true, "pull_requests": true, "issues": true,
	"metadata": true, "checks": true, "actions": true,
}

// A pull-only proof with an empty stored permission set ({} from PUT
// github-permissions) still sends an explicit, read-only permissions map:
// an access_tokens request without permissions would get every installation
// permission. Revert check: before the fix the request had no permissions
// key at all.
func TestGitHubAssociation_PullProofEmptyStoredPermissionsExplicitReadOnly(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5901, "acme", "acme/public")
	giveUserGitHubToken(t, srv, st, "puller", "ghp_puller")
	gh.grant("ghp_puller", "acme/public", githubapp.RepoAccess{Pull: true})

	p := createAssocProject(t, st, "perm-pull-empty", gh.remote("acme", "public"), "puller", ptrInt64(5901))
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+p.ID+"/github-permissions", map[string]string{})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	_, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, p.ID))
	require.NoError(t, err)
	_, mints := gh.counts()
	require.Len(t, mints, 1)
	require.NotEmpty(t, mints[0].Permissions, "the request must carry an explicit permissions map")
	assert.Equal(t, "read", mints[0].Permissions["metadata"])
	for name, lvl := range mints[0].Permissions {
		assert.True(t, allowedGitHubTokenPermissionFields[name], "unexpected permission %s", name)
		assert.Equal(t, "read", lvl, "permission %s must not be write under a pull-only proof", name)
	}
}

// A push proof with an empty stored set gets exactly the default set, and
// nothing outside the six known fields. Revert check: before the fix the
// request had no permissions key (every installation permission).
func TestGitHubAssociation_PushProofEmptyStoredPermissionsDefaultsOnly(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)

	createAssocInstallation(t, st, 5902, "acme", "acme/widgets")
	giveUserGitHubToken(t, srv, st, "pusher", "ghp_pusher")
	gh.grant("ghp_pusher", "acme/widgets", githubapp.RepoAccess{Push: true, Pull: true})

	p := createAssocProject(t, st, "perm-push-empty", gh.remote("acme", "widgets"), "pusher", ptrInt64(5902))
	stored := reloadProject(t, st, p.ID)
	stored.GitHubPermissions = &store.GitHubTokenPermissions{}
	require.NoError(t, st.UpdateProject(ctx, stored))

	_, _, err := srv.MintGitHubAppTokenForProject(ctx, reloadProject(t, st, p.ID))
	require.NoError(t, err)
	_, mints := gh.counts()
	require.Len(t, mints, 1)
	assert.Equal(t, map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
	}, mints[0].Permissions)
}

// PUT github-permissions accepts only the known fields with "", "read" or
// "write"; anything else is refused and nothing is stored. Revert check:
// before the fix any body was stored.
func TestProjectGitHubPermissions_PutValidated(t *testing.T) {
	srv, st := testServer(t)
	p := createAssocProject(t, st, "perm-put-validate", "https://github.com/acme/widgets", "nobody", nil)
	path := "/api/v1/projects/" + p.ID + "/github-permissions"

	for name, body := range map[string]interface{}{
		"unknown key":          map[string]string{"administration": "write"},
		"unknown key and good": map[string]string{"contents": "read", "secrets": "write"},
		"admin level":          map[string]string{"contents": "admin"},
		"bogus level":          map[string]string{"actions": "all"},
		"non-object":           []string{"contents"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut, path, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Nil(t, reloadProject(t, st, p.ID).GitHubPermissions, "nothing stored")
		})
	}

	rec := doRequest(t, srv, http.MethodPut, path, map[string]string{"contents": "read", "metadata": "read", "issues": ""})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got := reloadProject(t, st, p.ID).GitHubPermissions
	require.NotNil(t, got)
	assert.Equal(t, "read", got.Contents)
}

// githubTokenPermissionsForGrant never adds a field the stored set does not
// name (unless nothing usable is stored), never raises a field above the
// proof, drops unknown levels, and never returns an empty set.
func TestGitHubTokenPermissionsForGrant(t *testing.T) {
	push := githubAssociationGrant{WriteAllowed: true}
	pull := githubAssociationGrant{WriteAllowed: false}
	tests := []struct {
		name   string
		stored *store.GitHubTokenPermissions
		grant  githubAssociationGrant
		want   githubapp.TokenPermissions
	}{
		{"push nil -> defaults", nil, push, githubapp.DefaultTokenPermissions()},
		{"push empty -> defaults", &store.GitHubTokenPermissions{}, push, githubapp.DefaultTokenPermissions()},
		{"pull nil -> defaults lowered", nil, pull,
			githubapp.TokenPermissions{Contents: "read", PullRequests: "read", Issues: "read", Metadata: "read"}},
		{"pull empty -> defaults lowered", &store.GitHubTokenPermissions{}, pull,
			githubapp.TokenPermissions{Contents: "read", PullRequests: "read", Issues: "read", Metadata: "read"}},
		{"push keeps stored write", &store.GitHubTokenPermissions{Contents: "write", Actions: "write"}, push,
			githubapp.TokenPermissions{Contents: "write", Actions: "write"}},
		{"pull lowers stored write", &store.GitHubTokenPermissions{Contents: "write", Actions: "write"}, pull,
			githubapp.TokenPermissions{Contents: "read", Actions: "read"}},
		{"stored read stays read under push", &store.GitHubTokenPermissions{Contents: "read"}, push,
			githubapp.TokenPermissions{Contents: "read"}},
		{"unknown level dropped", &store.GitHubTokenPermissions{Contents: "read", Checks: "admin"}, push,
			githubapp.TokenPermissions{Contents: "read"}},
		{"only unknown levels -> defaults scoped", &store.GitHubTokenPermissions{Checks: "admin"}, pull,
			githubapp.TokenPermissions{Contents: "read", PullRequests: "read", Issues: "read", Metadata: "read"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := githubTokenPermissionsForGrant(tt.stored, tt.grant)
			assert.Equal(t, tt.want, got)
			assert.NotEqual(t, githubapp.TokenPermissions{}, got, "never empty")
		})
	}
}

// --- token-source project: the agent.create decision --------------------------

// AuthorizeGitHubTokenSourceProject allows only an active source user who may
// create agents in the SOURCE project. Revert check: removing the
// agentCreateDecision call allows the reader and the other-project member.
func TestAuthorizeGitHubTokenSourceProject_AgentCreateOnSource(t *testing.T) {
	srv, s, alice, _, ownProject := setupTemplateAuthzTest(t)
	ctx := context.Background()

	source := &store.Project{ID: tid("src-gate-source"), Name: "src-gate-source", Slug: "src-gate-source",
		OwnerID: alice.ID, CreatedBy: alice.ID, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, source))
	srv.seedProjectCreatorMembership(ctx, source)

	newUser := func(id string) *store.User {
		u := &store.User{ID: tid(id), Email: id + "@test.com", DisplayName: id,
			Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
		return u
	}
	creator := newUser("src-gate-creator")
	grantProjectRole(t, s, creator.ID, source.ID, store.ProjectRoleMember)
	reader := newUser("src-gate-reader")
	grantPermissionViaRoleBinding(t, s, reader.ID, "project.read", store.RoleScopeProject, source.ID)
	ownOnly := newUser("src-gate-own")
	grantProjectRole(t, s, ownOnly.ID, ownProject.ID, store.ProjectRoleMember)
	inactive := newUser("src-gate-inactive")
	grantProjectRole(t, s, inactive.ID, source.ID, store.ProjectRoleMember)
	inactive.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, inactive))

	agent := &store.Agent{ID: tid("src-gate-agent"), ProjectID: ownProject.ID}
	for _, tc := range []struct {
		name  string
		user  *store.User
		allow bool
	}{
		{"creator on source allowed", creator, true},
		{"reader on source denied", reader, false},
		{"agent.create on own project only denied", ownOnly, false},
		{"inactive user denied", inactive, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.authzService.sourceResolver = stubSourceResolver{user: tc.user}
			t.Cleanup(func() { srv.authzService.sourceResolver = nil })
			err := srv.AuthorizeGitHubTokenSourceProject(ctx, agent, source)
			if tc.allow {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, errGitHubTokenSourceDenied)
			}
		})
	}
}

// --- the binding annotation is server-owned ---------------------------------

// Project clone copies only projectSettingKeys annotations, and the settings
// endpoint writes only those keys, so neither can carry or write an admin
// binding. Pins both: the key is not a settings key, and a settings PUT that
// names it leaves the stored binding unchanged.
func TestGitHubInstallationBindingAnnotation_NotWritableViaSettingsOrClone(t *testing.T) {
	for _, k := range projectSettingKeys {
		assert.NotEqual(t, githubInstallationBindingAnnotation, k,
			"the binding must not be a project settings key (clone copies and settings writes these)")
	}

	srv, st := testServer(t)
	admin := createAssocAdmin(t, st, "binding-settings-admin")
	createAssocInstallation(t, st, 6001, "acme", "acme/widgets")
	p := createAssocProject(t, st, "binding-settings", "https://github.com/acme/widgets", "nobody", ptrInt64(6001))
	setAdminBinding(t, st, p, 6001, "github.com/acme/widgets", admin.ID)
	before := reloadProject(t, st, p.ID).Annotations[githubInstallationBindingAnnotation]
	require.NotEmpty(t, before)

	submitted := `{"installation_id":6001,"remote":"github.com/acme/widgets","bound_by":"someone-else"}`
	for _, body := range []interface{}{
		map[string]interface{}{"annotations": map[string]string{githubInstallationBindingAnnotation: submitted}},
		map[string]interface{}{githubInstallationBindingAnnotation: submitted},
	} {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+p.ID+"/settings", body)
		// Settings ignores keys it does not know, so the request succeeds;
		// the binding must still be unchanged.
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, before, reloadProject(t, st, p.ID).Annotations[githubInstallationBindingAnnotation],
			"a settings PUT must not change the binding")
	}
}

// --- token refresh resolves its source exactly like create --------------------

// The agent token-refresh endpoint resolves the token source through the
// same resolver as agent create and the lifecycle injection: the agent's own
// project, or the token-source project label only when the agent's source
// user may create agents there, and in either case only when that project's
// association is authorized at mint. For each case the refresh outcome
// matches the lifecycle injection's. Revert check: with the refresh reading
// only the agent's own project, the authorized-source case gets 400 while
// create delivers a token; with the label honoured without the source gate,
// the outsider case mints.
func TestGitHubTokenRefresh_ResolvesSourceLikeCreate(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	gh := newFakeGitHubForAssoc(t)
	configureFakeGitHubApp(t, srv, gh)
	require.NotNil(t, srv.agentTokenService, "the test server issues agent tokens")

	createAssocInstallation(t, st, 5901, "acme", "acme/widgets")
	source := createAssocProject(t, st, "refresh-src-source", "", "nobody", nil)
	bindProjectForTest(t, srv, st, source, 5901, "acme", "widgets")
	target := createAssocProject(t, st, "refresh-src-target", "", "nobody", nil)

	newUser := func(id string) *store.User {
		u := &store.User{ID: tid(id), Email: id + "@test.com", DisplayName: id,
			Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
		require.NoError(t, st.CreateUser(ctx, u))
		ensureHubMembership(ctx, st, u.ID)
		return u
	}
	member := newUser("refresh-src-member")
	grantProjectRole(t, st, member.ID, source.ID, store.ProjectRoleMember)
	outsider := newUser("refresh-src-outsider")
	grantProjectRole(t, st, outsider.ID, target.ID, store.ProjectRoleMember)

	agent := &store.Agent{
		ID: tid("refresh-src-agent"), Name: "refresh-src-agent", Slug: "refresh-src-agent",
		ProjectID: target.ID,
		Labels:    map[string]string{githubTokenSourceProjectLabel: source.ID},
	}
	require.NoError(t, st.CreateAgent(ctx, agent))
	token, err := srv.agentTokenService.GenerateAgentToken(agent.ID, target.ID,
		[]AgentTokenScope{ScopeAgentTokenRefresh}, nil)
	require.NoError(t, err)

	// refreshAndCreate runs the refresh endpoint and the create-time
	// lifecycle injection for agent and reports both outcomes and the
	// number of mints the refresh made.
	refreshAndCreate := func(t *testing.T) (refreshCode int, refreshToken, createToken string, refreshMints int) {
		t.Helper()
		_, before := gh.counts()
		rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/refresh-token", nil, token)
		_, after := gh.counts()
		if rec.Code == http.StatusOK {
			var resp struct {
				Token string `json:"token"`
			}
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			refreshToken = resp.Token
		}
		dispatcher := NewHTTPAgentDispatcherWithClient(st, &mockRuntimeBrokerClient{}, false, slog.Default())
		dispatcher.SetGitHubAppMinter(srv)
		env := map[string]string{}
		var classes map[string]api.EnvKind
		stored, err := st.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		dispatcher.injectLifecycleGitHubToken(ctx, stored, env, &classes, "DispatchAgentStart")
		return rec.Code, refreshToken, env["GITHUB_TOKEN"], len(after) - len(before)
	}
	useSourceUser := func(t *testing.T, u *store.User) {
		srv.authzService.sourceResolver = stubSourceResolver{user: u}
		t.Cleanup(func() { srv.authzService.sourceResolver = nil })
	}

	t.Run("source user without agent create on the source", func(t *testing.T) {
		useSourceUser(t, outsider)
		code, refreshed, created, mints := refreshAndCreate(t)
		assert.Equal(t, http.StatusBadRequest, code, "no installation to mint with")
		assert.Empty(t, refreshed)
		assert.Empty(t, created, "create delivers nothing either")
		assert.Zero(t, mints, "an unauthorized source label mints nothing")
	})

	t.Run("source user with agent create on the source", func(t *testing.T) {
		useSourceUser(t, member)
		_, before := gh.counts()
		code, refreshed, created, mints := refreshAndCreate(t)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "ghs_minted_installation_token", refreshed)
		assert.Equal(t, "ghs_minted_installation_token", created, "create delivers the same source's token")
		require.Equal(t, 1, mints)
		_, after := gh.counts()
		for _, m := range after[len(before):] {
			assert.Equal(t, "5901", m.InstallationID, "minted from the source project's installation")
			assert.Equal(t, []string{"widgets"}, m.Repositories)
		}
	})

	t.Run("authorized source whose association is no longer authorized", func(t *testing.T) {
		useSourceUser(t, member)
		// The binding names another remote, so it no longer authorizes.
		setAdminBinding(t, st, source, 5901, "github.com/acme/other", DevUserID)
		code, refreshed, created, mints := refreshAndCreate(t)
		assert.NotEqual(t, http.StatusOK, code, "the mint-time association check refuses")
		assert.Empty(t, refreshed)
		assert.Empty(t, created)
		assert.Zero(t, mints)
	})
}

// --- the admin association report is bounded ---------------------------------

// ageAssociationReport moves the cached report's build times back by d, as
// if d had passed.
func ageAssociationReport(srv *Server, d time.Duration) {
	c := &srv.githubAssociationReportCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generatedAt = c.generatedAt.Add(-d)
	c.lastFullBuild = c.lastFullBuild.Add(-d)
}

type associationReportResponse struct {
	Associations []githubAssociationReportEntry `json:"associations"`
	Total        int                            `json:"total"`
	Unauthorized int                            `json:"unauthorized"`
	GeneratedAt  string                         `json:"generated_at"`
	Truncated    bool                           `json:"truncated"`
}

func getAssociationReport(t *testing.T, srv *Server, query string) (*httptest.ResponseRecorder, associationReportResponse, map[string]githubAssociationReportEntry) {
	t.Helper()
	path := "/api/v1/github-app/installations/association-report"
	if query != "" {
		path += "?" + query
	}
	rec := doRequest(t, srv, http.MethodGet, path, nil)
	var resp associationReportResponse
	byID := map[string]githubAssociationReportEntry{}
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for _, e := range resp.Associations {
			byID[e.ProjectID] = e
		}
	}
	return rec, resp, byID
}

// The association report is served from a cache: a change made behind the
// association paths is not seen until the TTL or an explicit refresh, while
// a change made through them (bind, unbind) is seen at the next read through
// the same invalidation as the mint-path cache, with no full rebuild. A
// full rebuild runs at most once per interval: an explicit refresh inside it
// gets 429 with Retry-After. Revert check: without the cache the
// behind-the-back change shows at once; without the shared invalidation the
// unbound project stays listed; without the rate limit the early refresh
// gets 200.
func TestGitHubAssociationReport_CachedRateLimitedAndInvalidatedLikeMint(t *testing.T) {
	srv, st := testServer(t)
	admin := createAssocAdmin(t, st, "assoc-bound-admin")
	createAssocInstallation(t, st, 5951, "acme", "acme/widgets", "acme/legacy", "acme/later")

	bound := createAssocProject(t, st, "assoc-bound-bound", "https://github.com/acme/widgets", "nobody", ptrInt64(5951))
	setAdminBinding(t, st, bound, 5951, "github.com/acme/widgets", admin.ID)
	legacy := createAssocProject(t, st, "assoc-bound-legacy", "https://github.com/acme/legacy", "nobody", ptrInt64(5951))
	later := createAssocProject(t, st, "assoc-bound-later", "https://github.com/acme/later", "nobody", nil)

	rec, first, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, byID, bound.ID)
	require.Contains(t, byID, legacy.ID)
	assert.NotContains(t, byID, later.ID)
	assert.True(t, byID[bound.ID].Authorized)
	assert.False(t, byID[legacy.ID].Authorized)
	assert.NotEmpty(t, first.GeneratedAt)
	assert.False(t, first.Truncated)

	// Behind the association paths: bind legacy directly in the store. The
	// cached report still shows it unauthorized.
	setAdminBinding(t, st, legacy, 5951, "github.com/acme/legacy", admin.ID)
	rec, second, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.False(t, byID[legacy.ID].Authorized, "served from the cache")
	assert.Equal(t, first.GeneratedAt, second.GeneratedAt)

	// Through the association paths: unbind bound, bind later. Both show
	// at the next read, inside the rebuild interval and without a rebuild
	// (legacy is still the cached outcome).
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+bound.ID+"/github-installation", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+later.ID+"/github-installation",
		map[string]int64{"installation_id": 5951})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rec, third, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, byID, bound.ID, "unbinding invalidates the project's entry")
	require.Contains(t, byID, later.ID, "binding invalidates the project's entry")
	assert.True(t, byID[later.ID].Authorized)
	assert.Equal(t, githubAssociationBasisAdminBinding, byID[later.ID].Basis)
	assert.False(t, byID[legacy.ID].Authorized, "no full rebuild")
	assert.Equal(t, first.GeneratedAt, third.GeneratedAt)

	// An explicit refresh inside the rebuild interval is refused.
	rec, _, _ = getAssociationReport(t, srv, "refresh=true")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body: %s", rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	rec, _, _ = getAssociationReport(t, srv, "refresh=maybe")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	// Past the interval, the explicit refresh rebuilds and sees legacy's
	// binding.
	ageAssociationReport(srv, githubAssociationReportMinRebuildInterval+time.Second)
	rec, _, byID = getAssociationReport(t, srv, "refresh=true")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.True(t, byID[legacy.ID].Authorized, "the refresh rebuilt the report")

	// Past the TTL, a plain read rebuilds too.
	setAdminBinding(t, st, legacy, 5951, "github.com/acme/elsewhere", admin.ID)
	rec, _, byID = getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, byID[legacy.ID].Authorized, "still cached")
	ageAssociationReport(srv, githubAssociationReportTTL+time.Second)
	rec, _, byID = getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, byID[legacy.ID].Authorized, "rebuilt after the TTL")
}

// Concurrent reads of a cold report share one build: the creator access
// check behind an entry runs once. Revert check: without the build lock
// each read builds, and the check runs once per read.
func TestGitHubAssociationReport_ConcurrentReadsShareOneBuild(t *testing.T) {
	srv, st := testServer(t)
	createAssocInstallation(t, st, 5961, "acme", "acme/widgets")
	createAssocProject(t, st, "assoc-shared-build", "https://github.com/acme/widgets", "assoc-shared-creator", ptrInt64(5961))
	giveUserGitHubToken(t, srv, st, "assoc-shared-creator", "creator-token")

	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	srv.githubRepoAccessCheck = func(_ context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		// Inconclusive, so the mint-path access cache keeps nothing and
		// only the report build lock can dedupe.
		return githubapp.RepoAccess{}, errors.New("upstream unavailable")
	}

	const readers = 4
	var wg, ready sync.WaitGroup
	codes := make([]int, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		ready.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app/installations/association-report", nil)
			codes[i] = rec.Code
		}(i)
	}
	ready.Wait()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 1
	}, 5*time.Second, 10*time.Millisecond)
	// While the first build is held, no other reader starts a build of its
	// own: without the build lock every started reader reaches the check.
	assert.Never(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls > 1
	}, 300*time.Millisecond, 10*time.Millisecond, "a second build started while the first was running")
	close(release)
	wg.Wait()

	for _, c := range codes {
		assert.Equal(t, http.StatusOK, c)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls, "one build for all concurrent readers")
}

// --- the bind operation: admin-only, hub boundary only -----------------------

// bindFixture is the bearer matrix fixture with an installation of acme and
// the fixture project on acme/widgets.
func bindFixture(t *testing.T) (*bearerMatrixFixture, string) {
	t.Helper()
	m := newBearerMatrixFixture(t)
	createAssocInstallation(t, m.store, 5971, "acme", "acme/widgets")
	return m, setBindRemote(t, m, m.ids.project)
}

// setBindRemote puts projectID on acme/widgets and returns its bind path.
func setBindRemote(t *testing.T, m *bearerMatrixFixture, projectID string) string {
	t.Helper()
	ctx := context.Background()
	p, err := m.store.GetProject(ctx, projectID)
	require.NoError(t, err)
	p.GitRemote = "https://github.com/acme/widgets"
	require.NoError(t, m.store.UpdateProject(ctx, p))
	return "/api/v1/projects/" + projectID + "/github-installation"
}

func requireNotBound(t *testing.T, m *bearerMatrixFixture, projectID string) {
	t.Helper()
	p := reloadProject(t, m.store, projectID)
	assert.Nil(t, p.GitHubInstallationID, "no association")
	assert.Empty(t, p.Annotations[githubInstallationBindingAnnotation], "no binding")
}

// Negative matrix row: a token carrying project:update but not
// hub_github_app:update is refused on PUT .../github-installation, as is one
// carrying only hub_github_app:update; one carrying both binds. Revert
// check: without the handler's hub.github_app.update check the
// project:update-only token binds.
func TestGitHubInstallationBind_RequiresHubGitHubAppUpdate(t *testing.T) {
	m, path := bindFixture(t)
	projectUpdate := bearerMatrixSelector("project.update")
	githubAppUpdate := bearerMatrixSelector("hub.github_app.update")
	require.Equal(t, "project:update", projectUpdate)
	require.Equal(t, "hub_github_app:update", githubAppUpdate)
	body := map[string]int64{"installation_id": 5971}

	rec := doRequestWithUAT(t, m.srv, m.mint(t, hubBoundary(), []string{projectUpdate}), http.MethodPut, path, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project:update without hub_github_app:update: %s", rec.Body.String())
	requireNotBound(t, m, m.ids.project)

	rec = doRequestWithUAT(t, m.srv, m.mint(t, hubBoundary(), []string{githubAppUpdate}), http.MethodPut, path, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub_github_app:update without project:update: %s", rec.Body.String())
	requireNotBound(t, m, m.ids.project)

	rec = doRequestWithUAT(t, m.srv, m.mint(t, hubBoundary(), []string{githubAppUpdate, projectUpdate}), http.MethodPut, path, body)
	require.Equal(t, http.StatusOK, rec.Code, "both selectors: %s", rec.Body.String())
	p := reloadProject(t, m.store, m.ids.project)
	require.NotNil(t, p.GitHubInstallationID)
	assert.Equal(t, int64(5971), *p.GitHubInstallationID)
	var b githubInstallationBinding
	require.NoError(t, json.Unmarshal([]byte(p.Annotations[githubInstallationBindingAnnotation]), &b))
	assert.Equal(t, m.adminID, b.BoundBy)
}

// The bind operation is reachable only on the hub-bearer boundary: a
// project token for the very project is refused (and cannot carry
// hub_github_app:update at all); an agent credential of an agent in the
// project is refused. A hub token with both selectors binds. The catalog
// side (the hub boundary alone) is pinned by
// authzop.TestCatalog_GitHubInstallationBindHubBoundaryOnly. Revert check:
// without the handler's hub.github_app.update check the project token
// binds.
func TestGitHubInstallationBind_HubBearerBoundaryOnly(t *testing.T) {
	m, path := bindFixture(t)
	ctx := context.Background()
	body := map[string]int64{"installation_id": 5971}
	both := []string{"hub_github_app:update", "project:update"}

	// A project token for a project the admin owns, on that project.
	ownedPath := setBindRemote(t, m, m.otherProject)
	projectKey := m.mint(t, projectBoundary(m.otherProject), []string{"project:update"})
	rec := doRequestWithUAT(t, m.srv, projectKey, http.MethodPut, ownedPath, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project boundary token: %s", rec.Body.String())
	requireNotBound(t, m, m.otherProject)
	if key := m.tryMint(projectBoundary(m.otherProject), both); key != "" {
		rec = doRequestWithUAT(t, m.srv, key, http.MethodPut, ownedPath, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "project boundary token with both selectors: %s", rec.Body.String())
		requireNotBound(t, m, m.otherProject)
	} else {
		t.Logf("a project token cannot carry hub_github_app:update; the boundary is enforced at mint")
	}

	// An agent credential of an agent in the project.
	require.NotNil(t, m.srv.agentTokenService, "the test server issues agent tokens")
	agent, err := m.store.GetAgent(ctx, m.ids.agent)
	require.NoError(t, err)
	agentToken, err := m.srv.agentTokenService.GenerateAgentToken(agent.ID, agent.ProjectID, []AgentTokenScope{
		ScopeProjectRead, ScopeAgentCreate, ScopeAgentLifecycle, ScopeProjectTemplateWrite, ScopeAgentTokenRefresh,
	}, nil)
	require.NoError(t, err)
	rec = doRequestWithAgentToken(t, m.srv, http.MethodPut, "/api/v1/projects/"+agent.ProjectID+"/github-installation", body, agentToken)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, "agent credential: %s", rec.Body.String())
	requireNotBound(t, m, agent.ProjectID)

	// The hub boundary with both selectors binds, on either project.
	rec = doRequestWithUAT(t, m.srv, m.mint(t, hubBoundary(), both), http.MethodPut, path, body)
	require.Equal(t, http.StatusOK, rec.Code, "hub boundary token: %s", rec.Body.String())
	require.NotNil(t, reloadProject(t, m.store, m.ids.project).GitHubInstallationID)
	rec = doRequestWithUAT(t, m.srv, m.mint(t, hubBoundary(), both), http.MethodPut, ownedPath, body)
	require.Equal(t, http.StatusOK, rec.Code, "hub boundary token on the owned project: %s", rec.Body.String())
	require.NotNil(t, reloadProject(t, m.store, m.otherProject).GitHubInstallationID)
}

// A report build whose request is cancelled commits nothing: no cached
// report, and the rebuild interval is not used up, so the next read builds
// at once with real outcomes. Revert check: without the context check the
// cancelled build is cached (entries refused with the cancellation) and the
// next explicit refresh gets 429.
func TestGitHubAssociationReport_CancelledBuildCommitsNothing(t *testing.T) {
	srv, st := testServer(t)
	createAssocInstallation(t, st, 5981, "acme", "acme/widgets")
	p := createAssocProject(t, st, "assoc-cancel-build", "https://github.com/acme/widgets", "assoc-cancel-creator", ptrInt64(5981))
	giveUserGitHubToken(t, srv, st, "assoc-cancel-creator", "cancel-token")

	ctx, cancel := context.WithCancel(context.Background())
	srv.githubRepoAccessCheck = func(c context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		cancel()
		return githubapp.RepoAccess{}, c.Err()
	}
	_, _, err := srv.githubAssociationReport(ctx, false)
	require.ErrorIs(t, err, context.Canceled)
	c := &srv.githubAssociationReportCache
	c.mu.Lock()
	valid, lastBuild := c.valid, c.lastFullBuild
	c.mu.Unlock()
	assert.False(t, valid, "nothing cached")
	assert.True(t, lastBuild.IsZero(), "the rebuild interval is not used up")

	srv.githubRepoAccessCheck = func(context.Context, string, string, string, string) (githubapp.RepoAccess, error) {
		return githubapp.RepoAccess{Pull: true, Push: true}, nil
	}
	rec, _, byID := getAssociationReport(t, srv, "refresh=true")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, byID, p.ID)
	assert.True(t, byID[p.ID].Authorized)
}

// updateHookStore runs onUpdate before each UpdateProject it forwards, while
// its fault switch is armed.
type updateHookStore struct {
	store.Store
	fault    *storeFaultSwitch
	onUpdate func(p *store.Project)
}

func (s *updateHookStore) UpdateProject(ctx context.Context, p *store.Project) error {
	if s.fault.Active() && s.onUpdate != nil {
		s.onUpdate(p)
	}
	return s.Store.UpdateProject(ctx, p)
}

// A report read that lands between the bind's first invalidation and its
// store write re-evaluates the project from the pre-bind state; the second
// invalidation, after the write, makes the next read pick up the binding.
// The read is forced into that window by a store wrapper installed on the
// server (installStoreFault). Revert check:
// without the post-write invalidation the bound project stays missing from
// the report.
func TestGitHubAssociationReport_BindInvalidatesAfterWrite(t *testing.T) {
	srv, st, hook, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *updateHookStore {
		return &updateHookStore{Store: inner, fault: f}
	})
	createAssocInstallation(t, st, 5991, "acme", "acme/widgets")
	p := createAssocProject(t, st, "assoc-postwrite-bind", "https://github.com/acme/widgets", "nobody", nil)

	rec, _, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.NotContains(t, byID, p.ID)

	inWindow := 0
	hook.onUpdate = func(u *store.Project) {
		if u.ID != p.ID || inWindow > 0 {
			return
		}
		inWindow++
		// The pre-write invalidation is pending; this read consumes it
		// against the stored, still unbound project.
		_, _, err := srv.githubAssociationReport(context.Background(), false)
		require.NoError(t, err)
	}
	fault.Arm()

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+p.ID+"/github-installation",
		map[string]int64{"installation_id": 5991})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, 1, inWindow, "a report read ran between invalidation and write")

	rec, _, byID = getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, byID, p.ID, "the post-write invalidation re-evaluates the bound project")
	assert.True(t, byID[p.ID].Authorized)
	assert.Equal(t, githubAssociationBasisAdminBinding, byID[p.ID].Basis)
}

// An incremental report update (re-evaluating invalidated projects of a
// fresh report) whose request is cancelled commits nothing: the cached
// entry keeps its last outcome and the invalidation stays pending, so the
// next read evaluates the project for real. Revert check: without the
// context check in updateGitHubAssociationReport the cancelled outcome is
// cached, the invalidation is dropped and the call returns no error.
func TestGitHubAssociationReport_CancelledUpdateCommitsNothing(t *testing.T) {
	srv, st := testServer(t)
	createAssocInstallation(t, st, 5992, "acme", "acme/widgets")
	p := createAssocProject(t, st, "assoc-cancel-update", "https://github.com/acme/widgets", "assoc-cancel-update-creator", ptrInt64(5992))
	giveUserGitHubToken(t, srv, st, "assoc-cancel-update-creator", "cancel-update-token")
	srv.githubRepoAccessCheck = func(context.Context, string, string, string, string) (githubapp.RepoAccess, error) {
		return githubapp.RepoAccess{Pull: true, Push: true}, nil
	}

	rec, first, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.True(t, byID[p.ID].Authorized)
	assert.Equal(t, githubAssociationBasisCreatorPush, byID[p.ID].Basis)

	// Invalidate the project, then cancel the incremental update mid-check.
	srv.invalidateGitHubAssociation(p.ID)
	ctx, cancel := context.WithCancel(context.Background())
	srv.githubRepoAccessCheck = func(c context.Context, _, _, _, _ string) (githubapp.RepoAccess, error) {
		cancel()
		return githubapp.RepoAccess{}, c.Err()
	}
	_, _, err := srv.githubAssociationReport(ctx, false)
	require.ErrorIs(t, err, context.Canceled)
	c := &srv.githubAssociationReportCache
	c.mu.Lock()
	entry, pending := c.entries[p.ID], c.dirty[p.ID]
	c.mu.Unlock()
	assert.True(t, entry.Authorized, "the cached entry keeps its last outcome")
	assert.NotZero(t, pending, "the invalidation stays pending")

	// The next read re-evaluates the project: now with read-only access.
	srv.githubRepoAccessCheck = func(context.Context, string, string, string, string) (githubapp.RepoAccess, error) {
		return githubapp.RepoAccess{Pull: true}, nil
	}
	rec, second, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, first.GeneratedAt, second.GeneratedAt, "an incremental update, not a rebuild")
	assert.True(t, byID[p.ID].Authorized)
	assert.Equal(t, githubAssociationBasisCreatorPull, byID[p.ID].Basis, "re-evaluated after the cancelled update")
}

// The report pages through every project up to its cap: 1001 projects span
// more than one ListProjects page (and more than the store's 1000-row page
// cap), and the oldest project, listed last, is still reported and the
// report is not truncated. With the cap below the project count the build
// stops at the cap and says so; at exactly the project count it does not.
// Revert check: with a single ListProjects call the oldest project is
// missing (the store returns at most one page) and the report is truncated.
func TestGitHubAssociationReport_PagesUpToCap(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	admin := createAssocAdmin(t, st, "assoc-pages-admin")
	createAssocInstallation(t, st, 5993, "acme", "acme/widgets")

	// Created first, so newest-first listing returns it last.
	oldest := createAssocProject(t, st, "assoc-pages-oldest", "https://github.com/acme/widgets", "nobody", ptrInt64(5993))
	setAdminBinding(t, st, oldest, 5993, "github.com/acme/widgets", admin.ID)
	base := time.Now().Add(time.Hour)
	all, err := st.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	existing := len(all.Items)
	if all.NextCursor != "" {
		t.Fatalf("the test server starts with more than one page of projects")
	}
	const total = 1001
	for i := existing; i < total; i++ {
		id := fmt.Sprintf("assoc-pages-%04d", i)
		require.NoError(t, st.CreateProject(ctx, &store.Project{
			ID: tid(id), Name: id, Slug: tid(id),
			Created: base.Add(time.Duration(i) * time.Second), Updated: base,
		}))
	}

	rec, resp, byID := getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, byID, oldest.ID, "the last page is read")
	assert.True(t, byID[oldest.ID].Authorized)
	assert.False(t, resp.Truncated, "%d projects are under the cap", total)

	// Exactly at the cap: everything listed, not truncated.
	srv.githubAssociationReportCache.maxProjects = total
	ageAssociationReport(srv, githubAssociationReportTTL+time.Second)
	rec, resp, byID = getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, byID, oldest.ID)
	assert.False(t, resp.Truncated, "exactly at the cap")

	// Below the project count: stops at the cap and says so.
	srv.githubAssociationReportCache.maxProjects = 600
	ageAssociationReport(srv, githubAssociationReportTTL+time.Second)
	rec, resp, byID = getAssociationReport(t, srv, "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, byID, oldest.ID, "past the cap")
	assert.True(t, resp.Truncated, "more projects exist past the cap")
}
