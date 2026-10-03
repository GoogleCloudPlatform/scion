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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubTokenCreateResponse decodes POST /api/v1/auth/tokens, keeping the raw
// accessToken object so tests can check which keys are present.
type hubTokenCreateResponse struct {
	Token       string                     `json:"token"`
	AccessToken map[string]json.RawMessage `json:"accessToken"`
}

func decodeTokenCreate(t *testing.T, body []byte) (hubTokenCreateResponse, TokenResponse) {
	t.Helper()
	var raw hubTokenCreateResponse
	require.NoError(t, json.Unmarshal(body, &raw))
	var typed struct {
		AccessToken TokenResponse `json:"accessToken"`
	}
	require.NoError(t, json.Unmarshal(body, &typed))
	return raw, typed.AccessToken
}

func mustGetUser(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func deleteSystemBindings(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: "user", ID: userID}}, []string{store.RoleScopeSystem}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, bindings)
	for _, b := range bindings {
		require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
	}
}

// TestHubUAT_SuperAdminDeletesAgentInAnyProjectWhileAuthorityHolds is the
// end-to-end rule for a hub-boundary token through the real middleware and
// routes: a super-admin mints a hub token carrying agent:delete over a
// session, deletes an agent in a project it is not a member of, and is
// denied once its super-admin binding is removed.
func TestHubUAT_SuperAdminDeletesAgentInAnyProjectWhileAuthorityHolds(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectQ := tid("hubuat-slice-project-q")
	ownerQ := tid("hubuat-slice-owner-q")
	adminID := tid("hubuat-slice-admin")
	createRS1Project(t, s, projectQ, ownerQ)
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	rec := doRequestAsUser(t, srv, mustGetUser(t, s, adminID), http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name":     "hubuat-slice",
		"boundary": map[string]string{"kind": "hub"},
		"scopes":   []string{"agent:delete"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "session mint of a hub token: %s", rec.Body.String())
	raw, resp := decodeTokenCreate(t, rec.Body.Bytes())
	require.NotEmpty(t, raw.Token)
	assert.Equal(t, string(BoundaryKindHub), resp.Boundary.Kind)

	first := uatpAgent(t, s, projectQ, ownerQ, "slice-first", ownerQ)
	rec = doRequestWithUAT(t, srv, raw.Token, http.MethodDelete, "/api/v1/agents/"+first.ID, nil)
	require.Less(t, rec.Code, 300, "hub token must delete an agent in a project the super-admin is not a member of; got %d: %s", rec.Code, rec.Body.String())
	_, err := s.GetAgent(ctx, first.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the agent must be gone")

	deleteSystemBindings(t, s, adminID)

	second := uatpAgent(t, s, projectQ, ownerQ, "slice-second", ownerQ)
	rec = doRequestWithUAT(t, srv, raw.Token, http.MethodDelete, "/api/v1/agents/"+second.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "after demotion the same hub token must be denied; got: %s", rec.Body.String())
	_, err = s.GetAgent(ctx, second.ID)
	assert.NoError(t, err, "a denied delete must leave the agent in place")
}

// TestHubUAT_CeilingWithoutPermissionDenies pins that a hub token can do
// only what its ceiling names: a super-admin's hub token carrying
// agent:read cannot delete an agent.
func TestHubUAT_CeilingWithoutPermissionDenies(t *testing.T) {
	srv, s := testServer(t)
	projectQ := tid("hubuat-ceiling-project-q")
	ownerQ := tid("hubuat-ceiling-owner-q")
	adminID := tid("hubuat-ceiling-admin")
	createRS1Project(t, s, projectQ, ownerQ)
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-ceiling", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	agent := uatpAgent(t, s, projectQ, ownerQ, "ceiling", ownerQ)
	rec := doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestHubUAT_MemberReachesOnlyProjectsWithAccess pins that a member's hub
// token reaches its own project and is denied in a project the member
// cannot access.
func TestHubUAT_MemberReachesOnlyProjectsWithAccess(t *testing.T) {
	srv, s := testServer(t)
	projectP := tid("hubuat-member-project-p")
	projectQ := tid("hubuat-member-project-q")
	ownerP := tid("hubuat-member-owner-p")
	ownerQ := tid("hubuat-member-owner-q")
	createRS1Project(t, s, projectP, ownerP)
	createRS1Project(t, s, projectQ, ownerQ)

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerP), CreateTokenParams{
		UserID: ownerP, Name: "hubuat-member", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	own := uatpAgent(t, s, projectP, ownerP, "member-own", ownerP)
	other := uatpAgent(t, s, projectQ, ownerQ, "member-other", ownerQ)

	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+own.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+other.ID, nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
}

// TestCreateToken_HubBoundaryRequiresLiveAuthority pins that a hub
// boundary is not authority: a hub-admin whose roles do not carry
// agent.delete cannot mint a hub token carrying agent:delete, and the
// denial is the uniform forbidden error.
func TestCreateToken_HubBoundaryRequiresLiveAuthority(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("hubuat-mint-hubadmin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "member", store.SystemRoleHubAdmin)

	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-mint-hubadmin", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:delete"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUATProjectForbidden)
}

// TestCreateToken_HubBoundaryPersistsKindWithoutProject pins the stored
// form of a hub token and its mint audit: kind hub, no project ID.
func TestCreateToken_HubBoundaryPersistsKindWithoutProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	adminID := tid("hubuat-persist-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	_, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-persist", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	stored, err := s.GetUserAccessToken(ctx, token.ID)
	require.NoError(t, err)
	assert.Equal(t, string(BoundaryKindHub), stored.BoundaryKind)
	assert.Empty(t, stored.ProjectID)
	assert.NotEmpty(t, stored.KeyHash)

	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "credential_create", TargetID: token.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, records, 1)
	var summary map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(records[0].AfterSummary), &summary))
	assert.Equal(t, "hub", summary["boundary_kind"])
	assert.NotContains(t, summary, "project_id", "a hub token's audit carries no project ID")
}

// TestCreateToken_ProjectBoundaryAuditNamesProject pins the mint audit of a
// project token: kind project and the project ID.
func TestCreateToken_ProjectBoundaryAuditNamesProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("hubuat-projaudit-project")
	ownerID := tid("hubuat-projaudit-owner")
	createRS1Project(t, s, projectID, ownerID)

	_, token, err := srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "hubuat-projaudit", projectID, []string{"agent:read"}, nil)
	require.NoError(t, err)
	assert.Equal(t, string(BoundaryKindProject), token.BoundaryKind)
	assert.Equal(t, projectID, token.ProjectID)

	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "credential_create", TargetID: token.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, records, 1)
	var summary map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(records[0].AfterSummary), &summary))
	assert.Equal(t, "project", summary["boundary_kind"])
	assert.Equal(t, projectID, summary["project_id"])
}

// TestCreateTokenAPI_BoundaryForms pins the request forms that name a token
// boundary: the project ID shorthand, an explicit project boundary (alone or
// with an agreeing shorthand), and an explicit hub boundary. Hub responses
// omit projectId.
func TestCreateTokenAPI_BoundaryForms(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("hubuat-forms-project")
	ownerID := tid("hubuat-forms-owner")
	createRS1Project(t, s, projectID, ownerID)
	owner := mustGetUser(t, s, ownerID)

	cases := []struct {
		name        string
		body        map[string]interface{}
		wantKind    BoundaryKind
		wantProject string
	}{
		{"project shorthand", map[string]interface{}{"projectId": projectID}, BoundaryKindProject, projectID},
		{"explicit project boundary", map[string]interface{}{"boundary": map[string]string{"kind": "project", "projectId": projectID}}, BoundaryKindProject, projectID},
		{"explicit project boundary with agreeing shorthand", map[string]interface{}{"projectId": projectID, "boundary": map[string]string{"kind": "project", "projectId": projectID}}, BoundaryKindProject, projectID},
		{"explicit hub boundary", map[string]interface{}{"boundary": map[string]string{"kind": "hub"}}, BoundaryKindHub, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]interface{}{"name": "hubuat-forms-" + string(rune('a'+i)), "scopes": []string{"agent:read"}}
			for k, v := range tc.body {
				body[k] = v
			}
			rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/auth/tokens", body)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			raw, resp := decodeTokenCreate(t, rec.Body.Bytes())
			assert.Equal(t, string(tc.wantKind), resp.Boundary.Kind)
			assert.Equal(t, tc.wantProject, resp.Boundary.ProjectID)
			assert.Equal(t, tc.wantProject, resp.ProjectID)
			_, hasProjectID := raw.AccessToken["projectId"]
			assert.Equal(t, tc.wantKind == BoundaryKindProject, hasProjectID, "projectId is present only for project tokens")
			_, hasToken := raw.AccessToken["token"]
			assert.False(t, hasToken, "the access-token object never carries the secret")

			got := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/auth/tokens/"+resp.ID, nil)
			require.Equal(t, http.StatusOK, got.Code, got.Body.String())
			assert.NotContains(t, got.Body.String(), raw.Token, "GET never returns the secret")
			var fetched TokenResponse
			require.NoError(t, json.Unmarshal(got.Body.Bytes(), &fetched))
			assert.Equal(t, string(tc.wantKind), fetched.Boundary.Kind)
		})
	}
}

// TestCreateTokenAPI_RejectsMissingOrConflictingBoundary pins that a token
// request must name exactly one boundary: no boundary, a blank project ID
// in either form,
// a boundary without a kind, an unknown kind, a project boundary without a
// project, disagreeing forms, and a hub boundary with any project ID are
// each rejected with 400 and create no token.
func TestCreateTokenAPI_RejectsMissingOrConflictingBoundary(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("hubuat-reject-project")
	otherProject := tid("hubuat-reject-other")
	ownerID := tid("hubuat-reject-owner")
	createRS1Project(t, s, projectID, ownerID)
	owner := mustGetUser(t, s, ownerID)

	cases := []struct {
		name       string
		body       map[string]interface{}
		wantReason string
	}{
		{"no boundary and no project ID", map[string]interface{}{}, "boundary_required"},
		{"empty project ID", map[string]interface{}{"projectId": ""}, "boundary_required"},
		{"blank project ID", map[string]interface{}{"projectId": "   "}, "boundary_invalid"},
		{"boundary without kind", map[string]interface{}{"boundary": map[string]string{}}, "boundary_invalid"},
		{"boundary without kind beside project ID", map[string]interface{}{"projectId": projectID, "boundary": map[string]string{"projectId": projectID}}, "boundary_invalid"},
		{"unknown kind", map[string]interface{}{"boundary": map[string]string{"kind": "org"}}, "boundary_invalid"},
		{"project boundary without project", map[string]interface{}{"boundary": map[string]string{"kind": "project"}}, "boundary_invalid"},
		{"project boundary without project beside shorthand", map[string]interface{}{"projectId": projectID, "boundary": map[string]string{"kind": "project"}}, "boundary_invalid"},
		{"project boundary disagreeing with shorthand", map[string]interface{}{"projectId": otherProject, "boundary": map[string]string{"kind": "project", "projectId": projectID}}, "boundary_invalid"},
		{"hub boundary with shorthand project ID", map[string]interface{}{"projectId": projectID, "boundary": map[string]string{"kind": "hub"}}, "boundary_invalid"},
		{"hub boundary with nested project ID", map[string]interface{}{"boundary": map[string]string{"kind": "hub", "projectId": projectID}}, "boundary_invalid"},
		{"project boundary with blank project ID", map[string]interface{}{"boundary": map[string]string{"kind": "project", "projectId": "   "}}, "boundary_invalid"},
		{"hub boundary with blank project ID", map[string]interface{}{"boundary": map[string]string{"kind": "hub", "projectId": "   "}}, "boundary_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]interface{}{"name": "hubuat-reject", "scopes": []string{"agent:read"}}
			for k, v := range tc.body {
				body[k] = v
			}
			rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/auth/tokens", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, tc.wantReason, errResp.Error.Details["reason"], rec.Body.String())
		})
	}

	count, err := s.CountUserAccessTokens(ctx, ownerID)
	require.NoError(t, err)
	assert.Zero(t, count, "a rejected request creates no token")
}

// TestCreateTokenAPI_HubTokenCannotManageTokens pins that a hub token
// cannot mint further tokens.
func TestCreateTokenAPI_HubTokenCannotManageTokens(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("hubuat-manage-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-manage", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name": "hubuat-manage-child", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:read"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestCreateToken_HubBoundaryExpiryLimits pins that hub tokens obey the
// same expiry limits as project tokens.
func TestCreateToken_HubBoundaryExpiryLimits(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("hubuat-expiry-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	past := time.Unix(1, 0).UTC()
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-expiry-past", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"}, ExpiresAt: &past,
	})
	assert.ErrorIs(t, err, ErrUATExpiryPast)

	far := time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)
	_, _, err = srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-expiry-far", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:read"}, ExpiresAt: &far,
	})
	assert.ErrorIs(t, err, ErrUATExpiryTooLong)
}

// TestResolveTokenBoundary pins the service-level boundary resolution
// rules.
func TestResolveTokenBoundary(t *testing.T) {
	p := tid("resolve-boundary-project")
	q := tid("resolve-boundary-other")
	hub := TokenBoundary{Kind: BoundaryKindHub}
	proj := TokenBoundary{Kind: BoundaryKindProject, ProjectID: p}

	cases := []struct {
		name      string
		boundary  TokenBoundary
		projectID string
		want      TokenBoundary
		wantErr   error
	}{
		{"shorthand", TokenBoundary{}, p, proj, nil},
		{"nothing", TokenBoundary{}, "", TokenBoundary{}, ErrUATBoundaryRequired},
		{"explicit project", proj, "", proj, nil},
		{"explicit project with agreeing shorthand", proj, p, proj, nil},
		{"explicit project with disagreeing shorthand", proj, q, TokenBoundary{}, ErrUATBoundaryInvalid},
		{"explicit hub", hub, "", hub, nil},
		{"explicit hub with shorthand", hub, p, TokenBoundary{}, ErrUATBoundaryInvalid},
		{"hub carrying project", TokenBoundary{Kind: BoundaryKindHub, ProjectID: p}, "", TokenBoundary{}, ErrUATBoundaryInvalid},
		{"project without ID", TokenBoundary{Kind: BoundaryKindProject}, "", TokenBoundary{}, ErrUATBoundaryInvalid},
		{"unknown kind", TokenBoundary{Kind: "org"}, "", TokenBoundary{}, ErrUATBoundaryInvalid},
		{"blank shorthand", TokenBoundary{}, "   ", TokenBoundary{}, ErrUATBoundaryInvalid},
		{"project with blank ID", TokenBoundary{Kind: BoundaryKindProject, ProjectID: " \t"}, "", TokenBoundary{}, ErrUATBoundaryInvalid},
		{"explicit project with blank shorthand", proj, "  ", TokenBoundary{}, ErrUATBoundaryInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTokenBoundary(tc.boundary, tc.projectID)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
