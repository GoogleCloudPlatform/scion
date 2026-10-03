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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

	// The second agent is owned by the admin, so the owner relationship
	// grant alone would admit the delete. The project access stage of the
	// bearer gate must deny it: a former super-admin with no membership in Q
	// has no current access to Q.
	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	second := uatpAgent(t, s, projectQ, adminID, "slice-second", adminID)
	rec = doRequestWithUAT(t, srv, raw.Token, http.MethodDelete, "/api/v1/agents/"+second.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "after demotion the same hub token must be denied; got: %s", rec.Body.String())
	assert.Contains(t, deniedAuditReasons(emitter, second.ID), bearerReasonProjectAccessDenied,
		"the deny must come from the project access stage of the bearer gate")
	_, err = s.GetAgent(ctx, second.ID)
	assert.NoError(t, err, "a denied delete must leave the agent in place")
}

// deniedAuditReasons returns the reasons of every captured deny decision
// whose resource ID is resourceID.
func deniedAuditReasons(e *capturingAuditEmitter, resourceID string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var reasons []string
	for _, r := range e.records {
		if r.ResourceID == resourceID && r.Result == "deny" {
			reasons = append(reasons, r.Reason)
		}
	}
	return reasons
}

// hubUATFixture is a super-admin holding a hub token carrying agent:delete,
// and an agent in a project the super-admin is not a member of.
type hubUATFixture struct {
	srv     *Server
	store   store.Store
	adminID string
	tokenID string
	key     string
	agent   *store.Agent
}

func newHubUATFixture(t *testing.T, name string) hubUATFixture {
	t.Helper()
	srv, s := testServer(t)
	projectQ := tid("hubuat-" + name + "-project-q")
	ownerQ := tid("hubuat-" + name + "-owner-q")
	adminID := tid("hubuat-" + name + "-admin")
	createRS1Project(t, s, projectQ, ownerQ)
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-" + name, Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:delete"},
	})
	require.NoError(t, err)
	return hubUATFixture{
		srv: srv, store: s, adminID: adminID, tokenID: token.ID, key: key,
		agent: uatpAgent(t, s, projectQ, ownerQ, name, ownerQ),
	}
}

// requireAgentKept asserts that the fixture agent still exists.
func (f hubUATFixture) requireAgentKept(t *testing.T) {
	t.Helper()
	_, err := f.store.GetAgent(context.Background(), f.agent.ID)
	assert.NoError(t, err, "a rejected request must leave the agent in place")
}

// TestHubUAT_RevokedTokenRejected pins that a revoked hub token is
// rejected with 401 before authorization.
func TestHubUAT_RevokedTokenRejected(t *testing.T) {
	f := newHubUATFixture(t, "revoked")
	require.NoError(t, f.srv.uatService.RevokeToken(rs4MintContext(f.adminID), f.adminID, f.tokenID))

	rec := doRequestWithUAT(t, f.srv, f.key, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	f.requireAgentKept(t)
}

// TestHubUAT_ExpiredTokenRejected pins that a hub token past its expiry is
// rejected with 401 before authorization.
func TestHubUAT_ExpiredTokenRejected(t *testing.T) {
	// Mint against a service clock set in 2020 with an expiry one day
	// later, so the stored token is expired when it is validated.
	mintClock := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := mintClock.Add(24 * time.Hour)

	srv, s := testServer(t)
	projectQ := tid("hubuat-expired-project-q")
	ownerQ := tid("hubuat-expired-owner-q")
	adminID := tid("hubuat-expired-admin")
	createRS1Project(t, s, projectQ, ownerQ)
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)

	serviceClock := srv.uatService.nowFunc
	srv.uatService.nowFunc = func() time.Time { return mintClock }
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "hubuat-expired", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"agent:delete"}, ExpiresAt: &expiresAt,
	})
	srv.uatService.nowFunc = serviceClock
	require.NoError(t, err)

	agent := uatpAgent(t, s, projectQ, ownerQ, "expired", ownerQ)
	rec := doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	_, err = s.GetAgent(context.Background(), agent.ID)
	assert.NoError(t, err, "a rejected request must leave the agent in place")
}

// TestHubUAT_SuspendedUserRejected pins that a hub token whose user is
// suspended is rejected with 403 user_suspended before authorization.
func TestHubUAT_SuspendedUserRejected(t *testing.T) {
	f := newHubUATFixture(t, "suspended")
	ctx := context.Background()
	admin := mustGetUser(t, f.store, f.adminID)
	admin.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, admin))

	rec := doRequestWithUAT(t, f.srv, f.key, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, "user_suspended", errResp.Error.Code)
	f.requireAgentKept(t)
}

// TestHubUAT_FormerMemberRetainedAncestryDenied pins that a hub token
// reaches a project target only with current project access: a former
// member of Q who still owns, and is in the ancestry of, an agent in Q is
// denied read and attach at the project access stage, before relationship
// grants are considered.
func TestHubUAT_FormerMemberRetainedAncestryDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectQ := tid("hubuat-former-project-q")
	ownerQ := tid("hubuat-former-owner-q")
	memberID := tid("hubuat-former-member")
	createRS1Project(t, s, projectQ, ownerQ)
	uatpMember(t, s, projectQ, memberID)
	agent := uatpAgent(t, s, projectQ, memberID, "former", memberID)

	selectors := []string{"agent:read", "agent:attach"}
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(memberID), CreateTokenParams{
		UserID: memberID, Name: "hubuat-former", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: selectors,
	})
	require.NoError(t, err, "a member of Q may mint a hub token for its own agent")

	// While a member, the token reaches the agent.
	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	uatpDeleteProjectBinding(t, s, memberID, projectQ)

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "read: %s", rec.Body.String())
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "attach: %s", rec.Body.String())
	reasons := deniedAuditReasons(emitter, agent.ID)
	assert.NotEmpty(t, reasons)
	for _, reason := range reasons {
		assert.Equal(t, bearerReasonProjectAccessDenied, reason)
	}

	ceiling := bearerCeiling(t, selectors...)
	for _, permID := range []string{"agent.read", "agent.attach"} {
		eval := srv.authzService.EvaluateBearerCeiling(ctx, PrincipalContext{Identity: bearerUser(memberID)}, hubBoundary(), ceiling, permID, agentResource(agent), BearerOptions{})
		assert.False(t, eval.Decision.Allowed, "%s: %s", permID, eval.Decision.Reason)
		assert.Equal(t, BearerStageProjectAccess, eval.Stage, permID)
	}
}

// TestProjectRegisterEmbeddedBroker_HubUATDenied pins that a hub token
// carrying broker:create cannot register a project with an embedded
// broker: the request is denied with 403 and creates neither a broker nor
// a project.
func TestProjectRegisterEmbeddedBroker_HubUATDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("embedded-hubuat-project")
	ownerID := tid("embedded-hubuat-owner")
	createRS1Project(t, s, projectID, ownerID)
	grantPermissionViaRoleBinding(t, s, ownerID, "broker.create", store.RoleScopeSystem, "")
	grantPermissionViaRoleBinding(t, s, ownerID, "project.create", store.RoleScopeSystem, "")

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "embedded-hubuat", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"broker:create"},
	})
	require.NoError(t, err)

	const brokerName = "embedded-hubuat-broker"
	const projectName = "embedded-hubuat-new-project"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:   projectName,
		Broker: &RegisterProjectBrokerInfo{Name: brokerName, Version: "1.0.0"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "a denied response carries no secret")

	_, err = s.GetRuntimeBrokerByName(ctx, brokerName)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration creates no broker")
	_, err = s.GetProjectBySlugCaseInsensitive(ctx, api.Slugify(projectName))
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration creates no project")
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
	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+other.ID, nil)
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
	assert.Contains(t, deniedAuditReasons(emitter, other.ID), bearerReasonProjectAccessDenied,
		"the deny must come from the project access stage of the bearer gate")
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
