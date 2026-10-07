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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfRow is a self-scoped record with the project it belongs to (empty
// for a record with no project).
type selfRow struct {
	id        string
	projectID string
}

func selfRowProject(r selfRow) string { return r.projectID }

func selfRowIDs(rows []selfRow) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.id)
	}
	return ids
}

// Store-backed IDs for the self-scope tests that check live membership.
var (
	selfUserID   = tid("self-user")
	selfProjectA = tid("self-proj-a")
	selfProjectB = tid("self-proj-b")
)

// seedSelfScopeMember creates projectID and makes userID an active member
// of it, so the live membership check for project tokens passes.
func seedSelfScopeMember(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetProject(ctx, projectID); err != nil {
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: projectID, Slug: projectID}))
	}
	createTestUserWithProjectRole(t, s, userID, userID+"@test.com", projectID, store.ProjectRoleMember)
}

// selfToken builds a token identity with the given boundary and selectors.
func selfToken(t *testing.T, userID string, boundary TokenBoundary, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(userID), boundary, selectors, tid("self-cred-"+userID), bearerCeiling(t, selectors...), nil)
}

// callAuthorizeSelfScoped runs Server.authorizeSelfScoped for identity and
// returns its result and the response status it wrote (200 when it wrote
// nothing).
func callAuthorizeSelfScoped(srv *Server, identity Identity, permissionID, rowProjectID string) (bool, int) {
	req := httptest.NewRequest(http.MethodGet, "/self", nil)
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}
	rec := httptest.NewRecorder()
	ok := srv.authorizeSelfScoped(rec, req, permissionID, rowProjectID)
	return ok, rec.Code
}

// TestSelfScopedPermission_ProjectTokenSeesOnlyItsProject pins that a
// project token acts only on its own records in its boundary project, and
// that list totals and cursors count only the rows it may see. A hub token
// and a session see every row.
func TestSelfScopedPermission_ProjectTokenSeesOnlyItsProject(t *testing.T) {
	srv, s := testServer(t)
	projectA, projectB := selfProjectA, selfProjectB
	seedSelfScopeMember(t, s, selfUserID, projectA)
	token := selfToken(t, selfUserID, projectBoundary(projectA), "inbox:read")

	ok, code := callAuthorizeSelfScoped(srv, token, "inbox.read", projectA)
	assert.True(t, ok)
	assert.Equal(t, http.StatusOK, code)
	ok, code = callAuthorizeSelfScoped(srv, token, "inbox.read", projectB)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, code)

	rows := []selfRow{{"a1", projectA}, {"b1", projectB}, {"a2", projectA}, {"n1", ""}, {"a3", projectA}, {"b2", projectB}}
	assert.Equal(t, []string{"a1", "a2", "a3"}, selfRowIDs(filterSelfScopedRows(srv.newSelfScopeCheck(context.Background(), token, "inbox.read"), rows, selfRowProject)))

	page, total, next, err := pageSelfScopedRows(srv.newSelfScopeCheck(context.Background(), token, "inbox.read"), rows, selfRowProject, "", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"a1", "a2"}, selfRowIDs(page))
	assert.Equal(t, 3, total, "the total counts only visible rows")
	require.NotEmpty(t, next)
	page, total, next, err = pageSelfScopedRows(srv.newSelfScopeCheck(context.Background(), token, "inbox.read"), rows, selfRowProject, next, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"a3"}, selfRowIDs(page), "the cursor indexes visible rows")
	assert.Equal(t, 3, total)
	assert.Empty(t, next, "no cursor after the last visible row")
	_, _, _, err = pageSelfScopedRows(srv.newSelfScopeCheck(context.Background(), token, "inbox.read"), rows, selfRowProject, "9", 2)
	assert.ErrorIs(t, err, errInvalidSelfScopedCursor)

	hub := selfToken(t, selfUserID, hubBoundary(), "inbox:read")
	assert.Len(t, filterSelfScopedRows(srv.newSelfScopeCheck(context.Background(), hub, "inbox.read"), rows, selfRowProject), len(rows))
	_, total, _, err = pageSelfScopedRows(srv.newSelfScopeCheck(context.Background(), hub, "inbox.read"), rows, selfRowProject, "", 10)
	require.NoError(t, err)
	assert.Equal(t, len(rows), total)

	session := bearerUser(selfUserID)
	assert.Len(t, filterSelfScopedRows(srv.newSelfScopeCheck(context.Background(), session, "inbox.read"), rows, selfRowProject), len(rows))
	dev := &DevUser{id: "dev-user"}
	assert.Len(t, filterSelfScopedRows(srv.newSelfScopeCheck(context.Background(), dev, "inbox.read"), rows, selfRowProject), len(rows))
}

// TestSelfScopedPermission_ProjectlessRowsRequireHubBoundary pins that a
// record with no project, such as a direct message between two users, is
// reachable only with a hub token.
func TestSelfScopedPermission_ProjectlessRowsRequireHubBoundary(t *testing.T) {
	srv, s := testServer(t)
	seedSelfScopeMember(t, s, selfUserID, selfProjectA)
	project := selfToken(t, selfUserID, projectBoundary(selfProjectA), "inbox:read", "inbox:write")
	hub := selfToken(t, selfUserID, hubBoundary(), "inbox:read", "inbox:write")

	for _, permissionID := range []string{"inbox.read", "inbox.write"} {
		ok, code := callAuthorizeSelfScoped(srv, project, permissionID, "")
		assert.False(t, ok, permissionID)
		assert.Equal(t, http.StatusForbidden, code)
		ok, reason := selfScopedDecision(project, permissionID, "")
		assert.False(t, ok)
		assert.Equal(t, bearerReasonOutsideProject, reason)

		ok, code = callAuthorizeSelfScoped(srv, hub, permissionID, "")
		assert.True(t, ok, permissionID)
		assert.Equal(t, http.StatusOK, code)
	}
}

// TestSelfScopedPermission_CeilingWithoutSelectorDenies pins that a token
// needs the exact self selector and a boundary the permission allows; that
// only a self permission can be checked this way; and that credentials
// other than a session, a dev login or a user token are denied.
func TestSelfScopedPermission_CeilingWithoutSelectorDenies(t *testing.T) {
	srv, _ := testServer(t)

	unrelated := selfToken(t, "self-user", hubBoundary(), "agent:read", "project:read")
	ok, code := callAuthorizeSelfScoped(srv, unrelated, "inbox.read", "")
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, code)
	_, reason := selfScopedDecision(unrelated, "inbox.read", "")
	assert.Equal(t, selfScopeReasonCeiling, reason)

	readOnly := selfToken(t, "self-user", hubBoundary(), "inbox:read")
	ok, _ = selfScopedDecision(readOnly, "inbox.write", "")
	assert.False(t, ok, "inbox:read does not cover inbox.write")
	assert.Empty(t, filterSelfScopedRows(srv.newSelfScopeCheck(context.Background(), unrelated, "inbox.read"), []selfRow{{"n1", ""}}, selfRowProject))

	// user_skill_injection.update is hub-only: a project-boundary ceiling
	// carrying it is denied.
	projectCeiling := permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"user_skill_injection.update"}}
	projectInjection := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("self-user"), projectBoundary("proj-a"), nil, tid("self-inj"), projectCeiling, nil)
	ok, reason = selfScopedDecision(projectInjection, "user_skill_injection.update", "proj-a")
	assert.False(t, ok)
	assert.Equal(t, bearerReasonBoundaryIneligible, reason)
	hubInjection := selfToken(t, "self-user", hubBoundary(), "user_skill_injection:update")
	ok, _ = selfScopedDecision(hubInjection, "user_skill_injection.update", "")
	assert.True(t, ok)

	// A permission that is not self-scoped is never decided here, even for
	// a session.
	ok, reason = selfScopedDecision(bearerUser("self-user"), "agent.read", "proj-a")
	assert.False(t, ok)
	assert.Equal(t, selfScopeReasonNotSelfPermission, reason)

	// Agent credentials and a missing identity are denied.
	agent := &storedAgentIdentity{agent: &store.Agent{ID: "agent-x", ProjectID: "proj-a"}}
	ok, reason = selfScopedDecision(agent, "inbox.read", "proj-a")
	assert.False(t, ok)
	assert.Equal(t, selfScopeReasonCredential, reason)
	ok, code = callAuthorizeSelfScoped(srv, nil, "inbox.read", "proj-a")
	assert.False(t, ok)
	assert.Equal(t, http.StatusUnauthorized, code)
	var nilToken *ScopedUserIdentity
	ok, _ = selfScopedDecision(nilToken, "inbox.read", "")
	assert.False(t, ok)
}

// TestSelfScopedPermission_MintEligibleForActiveUserOnly pins the mint rule
// for self selectors: the issuer is an active user, with no role binding
// required. A project-boundary token still requires project membership,
// and a hub-only self selector cannot be minted on a project boundary.
func TestSelfScopedPermission_MintEligibleForActiveUserOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	selfSelectors := []string{"inbox:read", "inbox:write", "user_skill_injection:update"}

	// An active user with no role binding and no group membership.
	plainID := tid("selfmint-plain")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: plainID, Email: plainID + "@test.com", DisplayName: "Plain", Role: store.UserRoleMember, Status: "active"}))
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, plainID)
	require.NoError(t, err)
	require.Empty(t, bindings, "precondition: the issuer holds no role binding")

	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(plainID), CreateTokenParams{
		UserID: plainID, Name: "self", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: selfSelectors,
	})
	require.NoError(t, err, "an active user may mint self selectors on a hub boundary")
	identity, err := srv.uatService.ValidateToken(ctx, key)
	require.NoError(t, err)
	for _, id := range []string{"inbox.read", "inbox.write", "user_skill_injection.update"} {
		assert.True(t, identity.Ceiling().Allows(id), id)
	}

	principal := principalContextForIdentity(bearerUser(plainID))
	results, err := srv.authzService.CanMintSelector(ctx, principal, hubBoundary(), selfSelectors)
	require.NoError(t, err)
	for _, r := range results {
		assert.True(t, r.OK, "%s: %s", r.Selector, r.Reason)
	}
	// A non-self selector still needs authority the plain user lacks.
	results, err = srv.authzService.CanMintSelector(ctx, principal, hubBoundary(), []string{"group:create"})
	require.NoError(t, err)
	assert.False(t, results[0].OK)

	// A suspended user is not eligible.
	suspendedID := tid("selfmint-suspended")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: suspendedID, Email: suspendedID + "@test.com", DisplayName: "Suspended", Role: store.UserRoleMember, Status: store.UserStatusSuspended}))
	results, err = srv.authzService.CanMintSelector(ctx, principalContextForIdentity(bearerUser(suspendedID)), hubBoundary(), selfSelectors)
	require.NoError(t, err)
	for _, r := range results {
		assert.False(t, r.OK, "%s must not be mintable by a suspended user", r.Selector)
	}

	// Project boundary: a member may mint inbox selectors; a non-member may
	// not; the hub-only selector is refused on a project boundary.
	projectID := tid("selfmint-project")
	ownerID := tid("selfmint-owner")
	createRS1Project(t, s, projectID, ownerID)
	owner := principalContextForIdentity(bearerUser(ownerID))
	results, err = srv.authzService.CanMintSelector(ctx, owner, projectBoundary(projectID), []string{"inbox:read", "inbox:write", "user_skill_injection:update"})
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.True(t, results[0].OK, results[0].Reason)
	assert.True(t, results[1].OK, results[1].Reason)
	assert.False(t, results[2].OK)
	assert.Equal(t, MintDenialBoundaryNotAllowed, results[2].Reason)

	results, err = srv.authzService.CanMintSelector(ctx, principal, projectBoundary(projectID), []string{"inbox:read"})
	require.NoError(t, err)
	assert.False(t, results[0].OK, "a non-member cannot mint a project token")
	assert.Equal(t, MintDenialProjectAccessRequired, results[0].Reason)
}
