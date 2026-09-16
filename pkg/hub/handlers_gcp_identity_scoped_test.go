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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Coverage for the top-level /api/v1/gcp-service-accounts route (P4 item C).
//
// The validation cases carry most of the weight. Each one is a request the
// server could plausibly have repaired instead of refused, and the repair would
// have produced a plausible-looking 200 for a question the client did not ask.

func topLevelSAEmails(t *testing.T, srv *Server, user *store.User, query string) []string {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/gcp-service-accounts?"+query, nil)
	require.Equal(t, http.StatusOK, rec.Code, "list failed: %s", rec.Body.String())

	var resp ListGCPServiceAccountsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	emails := make([]string, 0, len(resp.Items))
	for _, item := range resp.Items {
		emails = append(emails, item.Email)
	}
	return emails
}

func TestGCPSA_TopLevel_ListByProjectScope(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	mine, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner,
		fmt.Sprintf("scope=project&scopeId=%s", project.ID))
	require.ElementsMatch(t, []string{mine}, emails,
		"scope=project must return only that project's SAs; hub-scoped %q leaked", hub)
}

func TestGCPSA_TopLevel_ListByProjectScope_IncludeHubScoped(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	mine, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner,
		fmt.Sprintf("scope=project&scopeId=%s&includeHubScoped=true", project.ID))
	require.ElementsMatch(t, []string{mine, hub}, emails,
		"the union must add hub-scoped SAs without reaching other projects")
}

// scope=hub carries no scopeId, which is the shape P5's picker sends. The
// assertion that matters is the negative half: no project-scoped account
// appears, so hub scope is a real filter and not a synonym for "everything".
func TestGCPSA_TopLevel_ListByHubScope_NoScopeIDRequired(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	_, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, owner, "scope=hub")
	require.ElementsMatch(t, []string{hub}, emails,
		"scope=hub must return hub-scoped SAs and only those")
}

// An ordinary hub member, not just the project owner, can list hub-scoped
// accounts. This is the list-side counterpart of the accepted exposure pinned
// in TestGCPSA_Get_HubScoped_HubMemberCanRead: hub-member-read-all grants
// read+list at hub scope to every user. P5's picker depends on it, so a change
// that narrows it should fail here rather than surface as an empty dropdown.
func TestGCPSA_TopLevel_ListByHubScope_HubMemberCanList(t *testing.T) {
	srv, s, owner, member, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	_, hub := seedListMix(t, ctx, s, owner, project)

	emails := topLevelSAEmails(t, srv, member, "scope=hub")
	require.ElementsMatch(t, []string{hub}, emails,
		"an ordinary hub member must be able to list hub-scoped SAs")
}

// Validation. Grouped because the shared property is what matters: each of
// these is refused rather than repaired.
func TestGCPSA_TopLevel_ScopeValidation(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()

	seedListMix(t, ctx, s, owner, project)

	cases := []struct {
		name  string
		query string
		why   string
	}{
		{
			name:  "MissingScope",
			query: "",
			why: "no default scope: an unfiltered list would be a cross-project " +
				"enumeration of every SA on the hub, which no existing route offers",
		},
		{
			name:  "UnknownScope",
			query: "scope=global",
			why: "hub scope is spelled \"hub\". \"global\" is the template " +
				"vocabulary and is not silently translated",
		},
		{
			name:  "UserScopeNotSupported",
			query: "scope=user",
			why:   "a real store.Scope value, but not one this route serves",
		},
		{
			name:  "ProjectScopeWithoutScopeID",
			query: "scope=project",
			why:   "would otherwise select every project's SAs at once",
		},
		{
			name:  "HubScopeWithClientSuppliedScopeID",
			query: "scope=hub&scopeId=some-other-hub",
			why: "the server resolves the hub's ID; accepting one from the client " +
				"would let a request name a hub that is not this one",
		},
		{
			name:  "HubScopeWithEmptyScopeID",
			query: "scope=hub&scopeId=",
			why: "presence is what is checked, not emptiness -- otherwise " +
				"?scopeId= slips past a rule that ?scopeId=x does not",
		},
		{
			name:  "IncludeHubScopedWithHubScope",
			query: "scope=hub&includeHubScoped=true",
			why:   "already implied; accepting it would make a no-op look meaningful",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, owner, http.MethodGet,
				"/api/v1/gcp-service-accounts?"+tc.query, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s: %s (got %s)", tc.name, tc.why, rec.Body.String())
		})
	}
}

// A project that does not exist is a 404, not an empty list. The distinction is
// invisible to the caller otherwise, and a typo'd project ID reading as "this
// project has no service accounts" is the kind of answer that gets believed.
func TestGCPSA_TopLevel_UnknownProjectIs404(t *testing.T) {
	srv, _, owner, _, _, _ := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/gcp-service-accounts?scope=project&scopeId="+tid("project-does-not-exist"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"unknown project should 404, not return an empty list; got: %s", rec.Body.String())
}

// The top-level create for project scope must be the same operation as the
// nested one, not a parallel implementation of it. Asserted through
// authorization, since that is where a second implementation would most likely
// have diverged: the nested route requires project ActionManage, so this one
// must deny an ordinary member too.
func TestGCPSA_TopLevel_CreateProjectScope_MatchesNestedAuthz(t *testing.T) {
	srv, _, owner, member, _, project := setupGCPAuthzTest(t)

	body := map[string]any{
		"email":     "new-sa@p.iam.gserviceaccount.com",
		"projectId": "gcp-proj",
	}

	rec := doRequestAsUser(t, srv, member, http.MethodPost,
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", project.ID), body)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a project member must not create a project-scoped SA here, same as the nested route; got: %s",
		rec.Body.String())

	rec = doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/gcp-service-accounts?scope=project&scopeId=%s", project.ID), body)
	require.Equal(t, http.StatusCreated, rec.Code,
		"a project owner should be able to create through the top-level route; got: %s",
		rec.Body.String())

	// And it lands at project scope, not somewhere the scope parameter implied
	// but the handler ignored.
	emails := topLevelSAEmails(t, srv, owner, fmt.Sprintf("scope=project&scopeId=%s", project.ID))
	require.Contains(t, emails, "new-sa@p.iam.gserviceaccount.com")
}

// P9: TestGCPSA_TopLevel_CreateHubScope_NotEnabled removed.
// The tripwire was a deliberate hold until hub-scoped BYO registration could
// safely open. P9 completes that: hub-scoped creation is now enabled for hub
// members (BYO registration), guarded at assignment by mode coupling + actAs.

func TestGCPSA_TopLevel_MethodNotAllowed(t *testing.T) {
	srv, _, owner, _, _, _ := setupGCPAuthzTest(t)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/gcp-service-accounts?scope=hub", nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}

// ============================================================================
// Hub-scope mint tests
// ============================================================================

// setupHubMintTest creates a test server with minting configured, an admin user
// (super-admin role binding, so gcp_service_account.create is granted), and a
// regular hub member who should be denied.
func setupHubMintTest(t *testing.T) (*Server, store.Store, *mockGCPServiceAccountAdmin, *store.User, *store.User) {
	t.Helper()

	srv, s := testServer(t)
	mock := &mockGCPServiceAccountAdmin{}
	srv.SetGCPServiceAccountAdmin(mock)
	srv.SetGCPProjectID("test-hub-project")
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub-sa@test-hub-project.iam.gserviceaccount.com"})

	ctx := context.Background()

	admin := &store.User{
		ID:          tid("user-hub-admin"),
		Email:       "hub-admin@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	member := &store.User{
		ID:          tid("user-hub-member"),
		Email:       "hub-member@test.com",
		DisplayName: "Hub Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	for _, u := range []*store.User{admin, member} {
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
	}

	// Grant super-admin to the admin user so gcp_service_account.create is available.
	grantSuperAdminRole(t, s, admin.ID)

	return srv, s, mock, admin, member
}

func TestGCPSA_HubMint_Success(t *testing.T) {
	srv, _, mock, admin, _ := setupHubMintTest(t)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var sa store.GCPServiceAccount
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sa))
	assert.True(t, sa.Managed, "minted SA must be managed")
	assert.True(t, sa.Verified, "minted SA must be verified")
	assert.Equal(t, store.ScopeHub, sa.Scope, "minted SA scope must be hub")
	assert.Contains(t, sa.Email, "@test-hub-project.iam.gserviceaccount.com")
	assert.Contains(t, sa.Email, "scion-")
	assert.Equal(t, "test-hub-project", sa.ProjectID)
	assert.Equal(t, "Scion hub agent", sa.DisplayName)
	assert.Len(t, mock.createdSAs, 1)
}

func TestGCPSA_HubMint_AuthorizationDenied(t *testing.T) {
	srv, _, _, _, member := setupHubMintTest(t)

	rec := doRequestAsUser(t, srv, member, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"non-admin hub member must be denied; got: %s", rec.Body.String())
}

func TestGCPSA_HubMint_PerHubQuota(t *testing.T) {
	srv, _, _, admin, _ := setupHubMintTest(t)
	srv.config.GCPMintCapPerHub = 2

	// Mint first two — should succeed
	for i := 0; i < 2; i++ {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost,
			"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
		require.Equal(t, http.StatusCreated, rec.Code, "mint %d: %s", i+1, rec.Body.String())
	}

	// Third mint should be rejected
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
	require.Equal(t, http.StatusConflict, rec.Code, "expected per-hub cap enforcement: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "per-hub mint limit")
}

func TestGCPSA_HubMint_GlobalQuota(t *testing.T) {
	srv, _, _, admin, _ := setupHubMintTest(t)
	srv.config.GCPMintCapGlobal = 2

	// Mint two at hub scope — should succeed
	for i := 0; i < 2; i++ {
		rec := doRequestAsUser(t, srv, admin, http.MethodPost,
			"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
		require.Equal(t, http.StatusCreated, rec.Code, "mint %d: %s", i+1, rec.Body.String())
	}

	// Third mint should hit global cap
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
	require.Equal(t, http.StatusConflict, rec.Code, "expected global cap enforcement: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "global mint limit")
}

func TestGCPSA_HubMint_NotConfigured(t *testing.T) {
	srv, s, _, _, _ := setupHubMintTest(t)
	// Remove the IAM admin to simulate minting not configured
	srv.SetGCPServiceAccountAdmin(nil)

	ctx := context.Background()
	admin := &store.User{
		ID:          tid("user-hub-admin-noconfig"),
		Email:       "admin-noconfig@test.com",
		DisplayName: "Admin NoConfig",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)
	grantSuperAdminRole(t, s, admin.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=hub", map[string]string{})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"minting not configured should return 503: %s", rec.Body.String())
}

func TestGCPSA_HubMint_ProjectScopeViaFlatRoute(t *testing.T) {
	srv, _, _, admin, _ := setupHubMintTest(t)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/gcp-service-accounts/mint?scope=project&scopeId=some-project", map[string]string{})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"project-scope via flat mint route should be refused: %s", rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "use /api/v1/projects/{id}/gcp-service-accounts/mint")
}

func TestGCPSA_HubMint_MethodNotAllowed(t *testing.T) {
	srv, _, _, admin, _ := setupHubMintTest(t)

	rec := doRequestAsUser(t, srv, admin, http.MethodGet,
		"/api/v1/gcp-service-accounts/mint?scope=hub", nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}

func TestGCPSA_HubScopeList_IncludesMintQuota(t *testing.T) {
	srv, s, _, admin, _ := setupHubMintTest(t)
	ctx := context.Background()

	// Seed a managed hub-scoped SA to see non-zero quota
	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID:        tid("sa-hub-managed"),
		Scope:     store.ScopeHub,
		ScopeID:   "test-hub-id",
		Email:     "managed@test-hub-project.iam.gserviceaccount.com",
		ProjectID: "test-hub-project",
		CreatedBy: admin.ID,
		CreatedAt: time.Now(),
		Managed:   true,
	}))

	srv.config.GCPMintCapPerHub = 5
	srv.config.GCPMintCapGlobal = 10

	rec := doRequestAsUser(t, srv, admin, http.MethodGet,
		"/api/v1/gcp-service-accounts?scope=hub", nil)
	require.Equal(t, http.StatusOK, rec.Code, "list failed: %s", rec.Body.String())

	var resp ListGCPServiceAccountsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MintQuota, "hub-scope list should include mint_quota when minting is configured")
	assert.Equal(t, 1, resp.MintQuota.HubMinted, "hub_minted should count managed hub-scoped SAs")
	assert.Equal(t, 5, resp.MintQuota.HubCap, "hub_cap should reflect GCPMintCapPerHub")
	assert.Equal(t, 1, resp.MintQuota.GlobalMinted, "global_minted should count all managed SAs")
	assert.Equal(t, 10, resp.MintQuota.GlobalCap, "global_cap should reflect GCPMintCapGlobal")
	// Project fields should be zero-omitted
	assert.Equal(t, 0, resp.MintQuota.ProjectMinted, "project_minted should be zero at hub scope")
	assert.Equal(t, 0, resp.MintQuota.ProjectCap, "project_cap should be zero at hub scope")
}
