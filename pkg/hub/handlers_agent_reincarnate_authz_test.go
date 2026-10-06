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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design §3.8 Amendment A24 (1b gteam live UAT finding, approved by ptone):
// authorizeAgentReincarnate's user/dev path checked ActionUpdate.
// permissions.Registry's agent.update entry has no UATScope, so the UAT
// project-constraint gate (AuthzService.enforceUATConstraints) denies every
// User Access Token outright for that action -- including a PAT scoped to
// agent:lifecycle or agent:manage. Only session/OAuth users and agent tokens
// could ever reincarnate. Fix: check ActionLifecycle instead, the same
// permission stop/start/restart already use (authorizeAgentLifecycle). The
// built-in roles grant agent.update and agent.lifecycle together, so no
// built-in role gains access it didn't already have.
//
// These tests exercise the real authz stack end to end -- a real
// *AuthzService backed by a real store, real RoleBinding rows, and a real
// ScopedUserIdentity (the production representation of a PAT) -- through the
// actual HTTP handler, not a mock of the CheckAccess decision.

// reincarnateAuthzFixture builds a project, a broker capable of
// reincarnation, and an agent within that project, reusing the existing
// reincarnate test helpers (setupReincarnateTestServer /
// newReincarnateTestAgent) from handlers_agent_reincarnate_test.go.
func reincarnateAuthzFixture(t *testing.T) (*Server, store.Store, *store.Project, *store.Agent) {
	t.Helper()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	return srv, s, project, agent
}

// newReincarnateAuthzUser creates and persists a real, non-admin member user
// with no role bindings of its own -- callers grant only what each test needs.
func newReincarnateAuthzUser(t *testing.T, s store.Store, idSuffix string) *store.User {
	t.Helper()
	ctx := context.Background()
	user := &store.User{
		ID:          tid("reincarnate-authz-" + idSuffix),
		Email:       "reincarnate-authz-" + idSuffix + "@test.com",
		DisplayName: "Reincarnate Authz " + idSuffix,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	return user
}

// grantAgentLifecycleAtProject grants userID the agent.lifecycle permission
// at project scope, via a dedicated single-permission role binding -- the
// same mechanism uat_enforcement_test.go's grantPermissionViaRoleBinding
// uses, kept local so this grant is independent of exactly which built-in
// role bundles agent.lifecycle (project-owner and project-admin both do,
// but that is incidental to what this test needs to prove).
func grantAgentLifecycleAtProject(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	grantPermissionViaRoleBinding(t, s, userID, "agent.lifecycle", store.RoleScopeProject, projectID)
}

// grantAgentDelegationAtProject grants userID agent.create at project scope.
// A reincarnation requested by another principal re-records the agent's
// authority under the requester, which requires delegation authority
// (CanDelegate) in addition to agent.lifecycle.
func grantAgentDelegationAtProject(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	grantPermissionViaRoleBinding(t, s, userID, "agent.create", store.RoleScopeProject, projectID)
}

// grantProjectRole binds userID to a real, named, seeded project-scoped role
// (e.g. store.ProjectRoleAdmin), for the "a session user with the role" case
// -- as distinct from the PAT tests, which grant the bare permission
// directly and are not about role bundles at all.
func grantProjectRole(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// scopedIdentityFor wraps a real store.User as the UserIdentity a
// ScopedUserIdentity (the production PAT representation) decorates.
func scopedIdentityFor(user *store.User, projectID string, scopes []string) *ScopedUserIdentity {
	base := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	return NewScopedUserIdentity(base, projectID, scopes)
}

// TestReincarnateAgent_PATWithLifecycleScope_Allowed is the core A24
// regression test: a PAT scoped to agent:lifecycle, for a user who holds
// agent.lifecycle on the project, must be allowed. Verified to FAIL (403)
// against the pre-fix code (ActionUpdate) and PASS after the fix
// (ActionLifecycle) -- see the round-3 dev report for the before/after proof.
func TestReincarnateAgent_PATWithLifecycleScope_Allowed(t *testing.T) {
	srv, s, project, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "lifecycle-pat")
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	grantAgentDelegationAtProject(t, s, user.ID, project.ID)

	// minimalSelectors (agent:create and the read selectors) covers the
	// target's baseline role, which the delegation check requires.
	identity := scopedIdentityFor(user, project.ID, append(minimalSelectors(t), "agent:lifecycle"))
	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, "dry run: body: %s", rec.Body.String())
}

// TestReincarnateAgent_PATWithManageScope_Allowed covers the agent:manage
// UAT alias, which expands (at real token-mint time) to every non-excluded
// agent UAT scope, agent:lifecycle included -- permissions.UATManageScopes()
// reproduces that expansion for the test, the same way
// TestUATEnforcement_AgentManageAlias does.
func TestReincarnateAgent_PATWithManageScope_Allowed(t *testing.T) {
	srv, s, project, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "manage-pat")
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	grantAgentDelegationAtProject(t, s, user.ID, project.ID)

	identity := scopedIdentityFor(user, project.ID, append(permissions.UATManageScopes(), minimalSelectors(t)...))
	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, "dry run: body: %s", rec.Body.String())
}

// TestReincarnateAgent_PATWithoutLifecycleScope_Denied is the scope-narrowing
// half of A24: even though the user holds agent.lifecycle on the project (so
// a suitably-scoped token would succeed -- see the Allowed tests above), a
// PAT minted with only agent:read and agent:list must still be denied. The
// UAT project-constraint gate narrows a token's own access down to its
// scopes; it can never be widened by the user's underlying role.
//
// DryRun is false (review p1b-2078-r4 Optional #2): a dry run never mutates
// the agent regardless of whether the request is allowed or denied, so a
// DryRun:true request would make the "agent must be untouched" assertions
// below vacuous -- they would pass even if the authz gate let the request
// through. A real (non-dry-run) request that reached the worker would flip
// ReincarnationState and bump StateVersion, so leaving those fields
// untouched here is a genuine assertion about the gate, not about the
// request shape.
func TestReincarnateAgent_PATWithoutLifecycleScope_Denied(t *testing.T) {
	srv, s, project, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "no-lifecycle-pat")
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	beforeVersion := agent.StateVersion

	identity := scopedIdentityFor(user, project.ID, []string{"agent:read", "agent:list"})
	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: false})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched by a denied request")
	assert.Equal(t, "", after.ReincarnationState)
}

// TestReincarnateAgent_LifecycleOnlyPATDenied: a PAT scoped to
// agent:lifecycle, for a user who holds agent.lifecycle but not agent.create
// on the project, passes the lifecycle gate and is refused at the delegation
// check, because a reincarnation by another principal re-records the agent's
// authority under the requester. A dry run gets the same answer as the real
// request, and nothing is claimed.
func TestReincarnateAgent_LifecycleOnlyPATDenied(t *testing.T) {
	srv, s, project, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "lifecycle-only-pat")
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	beforeVersion := agent.StateVersion

	identity := scopedIdentityFor(user, project.ID, []string{"agent:lifecycle"})
	for _, dryRun := range []bool{true, false} {
		req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code, "dryRun=%v body: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "Cannot delegate agent authority")
	}

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "nothing is claimed")
	assert.Equal(t, "", after.ReincarnationState)
	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestReincarnateAgent_SessionUserWithRole_Allowed is the non-PAT companion:
// a session/OAuth user (a plain UserIdentity, not wrapped in
// ScopedUserIdentity -- so the UAT project-constraint gate never applies at
// all) holding a real seeded project-admin role, which bundles
// agent.lifecycle, must be allowed. This path was never broken by the A24
// bug (ActionUpdate has no UATScope, but a session user's CheckAccess never
// goes through enforceUATConstraints in the first place); included as the
// regression guard that the ActionLifecycle switch doesn't break it.
func TestReincarnateAgent_SessionUserWithRole_Allowed(t *testing.T) {
	srv, s, project, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "session-admin")
	grantProjectRole(t, s, user.ID, project.ID, store.ProjectRoleAdmin)

	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, "dry run: body: %s", rec.Body.String())
}

// TestReincarnateAgent_SessionUserWithoutLifecycle_Denied: a session user
// with no role binding on the project at all gets 403, and the agent is
// untouched. DryRun is false, for the same reason as
// TestReincarnateAgent_PATWithoutLifecycleScope_Denied (review p1b-2078-r4
// Optional #2): otherwise the untouched assertion is vacuous.
func TestReincarnateAgent_SessionUserWithoutLifecycle_Denied(t *testing.T) {
	srv, s, _, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "session-nobody")
	beforeVersion := agent.StateVersion

	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: false})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched by a denied request")
}

// TestReincarnateAgent_SessionUserWithOnlyAgentUpdate_Denied is the review
// p1b-2078-r4 Optional #2 regression test that pins the intended policy
// shift on the non-PAT path: project-admin (used by
// TestReincarnateAgent_SessionUserWithRole_Allowed) bundles both
// agent.lifecycle and agent.update, so that test alone cannot distinguish
// which permission the handler actually checks. A session user bound to a
// custom role granting ONLY agent.update -- the exact permission A24
// replaced -- must be denied. This fails under the pre-A24 ActionUpdate
// mutation (verified; see the round-4 dev report) and passes at head.
func TestReincarnateAgent_SessionUserWithOnlyAgentUpdate_Denied(t *testing.T) {
	srv, s, project, agent := reincarnateAuthzFixture(t)
	user := newReincarnateAuthzUser(t, s, "session-update-only")
	grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
	beforeVersion := agent.StateVersion

	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h", DryRun: false})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched by a denied request")
}
