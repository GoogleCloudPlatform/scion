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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project self-leave (ptone/scion#7494): POST /api/v1/projects/{id}/leave.

// enableProjectLeave turns hub.project_self_leave on for srv.
func enableProjectLeave(t *testing.T, srv *Server) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{experiments.ProjectSelfLeave: true})
}

func leavePath(projectID string) string { return "/api/v1/projects/" + projectID + "/leave" }

// leaveAs sends a leave (or a dry run) as user over a web session.
func leaveAs(t *testing.T, srv *Server, user *store.User, projectID string, dryRun bool) *httptest.ResponseRecorder {
	t.Helper()
	var body interface{}
	if dryRun {
		body = map[string]bool{"dryRun": true}
	}
	return doRequestAsUser(t, srv, user, http.MethodPost, leavePath(projectID), body)
}

// leaveRequestFunc prepares a leave as user over a web session and returns
// a function that sends it, safe to call from another goroutine.
func leaveRequestFunc(t *testing.T, srv *Server, user *store.User, projectID string) func() int {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	return func() int {
		req := httptest.NewRequest(http.MethodPost, leavePath(projectID), bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
}

func decodeLeaveResult(t *testing.T, rec *httptest.ResponseRecorder) LeaveResult {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res LeaveResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	return res
}

// leaveErrorBody decodes an error response.
func leaveErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Error.Code, resp.Error.Details
}

// directProjectBindings returns the IDs of userID's direct bindings on
// projectID, sorted.
func directProjectBindings(t *testing.T, s store.Store, userID, projectID string) []string {
	t.Helper()
	rbs, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	var ids []string
	for _, rb := range rbs {
		if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == projectID {
			ids = append(ids, rb.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// lossChecksFor returns the pending membership loss checks of userID.
func lossChecksFor(t *testing.T, s store.Store, userID string) []*store.MembershipLossCheck {
	t.Helper()
	var out []*store.MembershipLossCheck
	for _, c := range pendingChecks(t, s) {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out
}

// leaveUser creates an active user; hubMember also adds them to the
// hub-members group.
func leaveUser(t *testing.T, s store.Store, name string, hubMember bool) *store.User {
	t.Helper()
	ctx := context.Background()
	id := tid(t.Name() + "-" + name)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@test.com", DisplayName: name, Role: "member", Status: store.UserStatusActive,
	}))
	if hubMember {
		ensureHubMembership(ctx, s, id)
	}
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

// leaveGroup creates an explicit group owned by ownerID.
func leaveGroup(t *testing.T, s store.Store, name, ownerID string) *store.Group {
	t.Helper()
	g := &store.Group{
		ID: tid(t.Name() + "-group-" + name), Name: name, Slug: tidSlugSafe(t.Name() + "-group-" + name),
		GroupType: store.GroupTypeExplicit, OwnerID: ownerID, CreatedBy: ownerID,
	}
	require.NoError(t, s.CreateGroup(context.Background(), g))
	return g
}

func leaveAddGroupMember(t *testing.T, s store.Store, groupID, memberType, memberID string) {
	t.Helper()
	require.NoError(t, s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: groupID, MemberType: memberType, MemberID: memberID, Role: store.GroupMemberRoleMember,
	}))
}

// TestProjectLeave_ExperimentOffIs404 pins that the hub, not the browser,
// gates the route: with hub.project_self_leave off the route answers 404
// and nothing changes; GET answers 405 either way.
func TestProjectLeave_ExperimentOffIs404(t *testing.T) {
	f := setupMMRFixture(t)
	setExperimentOverrides(t, f.srv, map[string]bool{experiments.ProjectSelfLeave: false})
	before := directProjectBindings(t, f.store, f.member.ID, f.projectID)

	rec := leaveAs(t, f.srv, f.member, f.projectID, false)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, before, directProjectBindings(t, f.store, f.member.ID, f.projectID))
	assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))

	rec = doRequestAsUser(t, f.srv, f.member, http.MethodGet, leavePath(f.projectID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())

	enableProjectLeave(t, f.srv)
	rec = doRequestAsUser(t, f.srv, f.member, http.MethodGet, leavePath(f.projectID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}

// TestProjectLeave_MemberLeaves: a member leaves; the binding is gone, one
// project_member_leave record (actor = target) and one member_leave loss
// check exist, and a retry gets the not-found answer.
func TestProjectLeave_MemberLeaves(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	f.srv.membershipService.onMembershipLoss = nil

	res := decodeLeaveResult(t, leaveAs(t, f.srv, f.member, f.projectID, false))
	assert.Equal(t, f.projectID, res.ProjectID)
	assert.False(t, res.DryRun)
	assert.Equal(t, []string{store.ProjectRoleMember}, res.RemovedRoles)
	assert.Equal(t, leaveRemainingNone, res.RemainingAccess)
	assert.Empty(t, directProjectBindings(t, f.store, f.member.ID, f.projectID))

	recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: "project_member_leave", TargetID: f.projectID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, f.member.ID, recs[0].ActorPrincipalID, "actor = target")
	assert.Contains(t, recs[0].BeforeSummary, f.member.ID)
	assert.Contains(t, recs[0].BeforeSummary, store.ProjectRoleMember)

	checks := lossChecksFor(t, f.store, f.member.ID)
	require.Len(t, checks, 1)
	assert.Equal(t, store.MembershipLossTriggerMemberLeave, checks[0].Trigger)
	assert.Equal(t, f.projectID, checks[0].ProjectID)

	// A retry: the caller no longer has a binding and the project must not
	// be revealed.
	rec := leaveAs(t, f.srv, f.member, f.projectID, false)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, 1, countAudits(t, f.store, "project_member_leave", f.projectID))
}

// TestProjectLeave_RemovesEveryDirectRole: an admin who also holds a
// custom role leaves in one step; both bindings go, other principals'
// bindings stay.
func TestProjectLeave_RemovesEveryDirectRole(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	grpBind(t, f.store, store.RoleBindingPrincipalUser, f.admin.ID, f.withinCeiling.ID, f.projectID)
	require.Len(t, directProjectBindings(t, f.store, f.admin.ID, f.projectID), 2)
	memberBefore := directProjectBindings(t, f.store, f.member.ID, f.projectID)
	ownerBefore := directProjectBindings(t, f.store, f.owner.ID, f.projectID)

	res := decodeLeaveResult(t, leaveAs(t, f.srv, f.admin, f.projectID, false))
	want := []string{f.withinCeiling.Name, store.ProjectRoleAdmin}
	sort.Strings(want)
	assert.Equal(t, want, res.RemovedRoles)
	assert.Empty(t, directProjectBindings(t, f.store, f.admin.ID, f.projectID))
	assert.Equal(t, memberBefore, directProjectBindings(t, f.store, f.member.ID, f.projectID))
	assert.Equal(t, ownerBefore, directProjectBindings(t, f.store, f.owner.ID, f.projectID))
}

// TestProjectLeave_NonMemberAndMissingProjectIdentical404: a non-member
// and a missing project get byte-identical 404s.
func TestProjectLeave_NonMemberAndMissingProjectIdentical404(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)

	nonMember := leaveAs(t, f.srv, f.member, f.otherProjectID, false)
	missing := leaveAs(t, f.srv, f.member, tid(t.Name()+"-no-such-project"), false)
	require.Equal(t, http.StatusNotFound, nonMember.Code, nonMember.Body.String())
	require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
	assert.Equal(t, missing.Body.String(), nonMember.Body.String())
	code, _ := leaveErrorBody(t, missing)
	assert.Equal(t, ErrCodeNotFound, code)
}

// TestProjectLeave_HubAdminNoDirectBinding404 (R3, 6b): the hub-level
// override does not apply. A super-admin with no direct binding gets the
// missing-project 404 and nothing changes; with a direct binding only that
// binding goes and the remaining access is hub.
func TestProjectLeave_HubAdminNoDirectBinding404(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	f.srv.membershipService.onMembershipLoss = nil
	super := mtSuperAdmin(t, f.store, "super")
	ownerBefore := directProjectBindings(t, f.store, f.owner.ID, f.projectID)

	published := 0
	orig := projectLeavePublish
	projectLeavePublish = func(s *Server, m conduitAuthzMatch) { published++; orig(s, m) }
	t.Cleanup(func() { projectLeavePublish = orig })

	rec := leaveAs(t, f.srv, super, f.projectID, false)
	missing := leaveAs(t, f.srv, super, tid(t.Name()+"-no-such-project"), false)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, missing.Body.String(), rec.Body.String())
	assert.Equal(t, ownerBefore, directProjectBindings(t, f.store, f.owner.ID, f.projectID))
	assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))
	assert.Empty(t, lossChecksFor(t, f.store, super.ID))
	assert.Zero(t, published, "no conduit publish for a refused leave")

	// Variant: a direct binding plus hub authority.
	grpBind(t, f.store, store.RoleBindingPrincipalUser, super.ID, f.memberRD.ID, f.projectID)
	res := decodeLeaveResult(t, leaveAs(t, f.srv, super, f.projectID, false))
	assert.Equal(t, []string{store.ProjectRoleMember}, res.RemovedRoles)
	assert.Equal(t, leaveRemainingHub, res.RemainingAccess)
	assert.Empty(t, directProjectBindings(t, f.store, super.ID, f.projectID))
	assert.Equal(t, ownerBefore, directProjectBindings(t, f.store, f.owner.ID, f.projectID))
	assert.Equal(t, 1, published)
}

// TestProjectLeave_LastOwner409: the last usable direct owner is refused
// with guidance; an expired or scheduled second owner does not count; a
// usable second owner lets the first leave.
func TestProjectLeave_LastOwner409(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	ctx := context.Background()
	ownerBefore := directProjectBindings(t, f.store, f.owner.ID, f.projectID)

	requireLastOwner := func() {
		t.Helper()
		rec := leaveAs(t, f.srv, f.owner, f.projectID, false)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		code, details := leaveErrorBody(t, rec)
		assert.Equal(t, ErrCodeLastOwner, code)
		assert.NotEmpty(t, details["guidance"])
		assert.Equal(t, ownerBefore, directProjectBindings(t, f.store, f.owner.ID, f.projectID))
		assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))
	}
	requireLastOwner()

	// A dry run gives the same refusal.
	rec := leaveAs(t, f.srv, f.owner, f.projectID, true)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	expired := leaveUser(t, f.store, "expired-owner", true)
	_, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.ownerRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: expired.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, ExpiresAt: &past, CreatedBy: "test",
	})
	require.NoError(t, err)
	scheduled := leaveUser(t, f.store, "scheduled-owner", true)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.ownerRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: scheduled.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, NotBefore: &future, CreatedBy: "test",
	})
	require.NoError(t, err)
	requireLastOwner()

	second := leaveUser(t, f.store, "second-owner", true)
	grpBind(t, f.store, store.RoleBindingPrincipalUser, second.ID, f.ownerRD.ID, f.projectID)
	res := decodeLeaveResult(t, leaveAs(t, f.srv, f.owner, f.projectID, false))
	assert.Equal(t, []string{store.ProjectRoleOwner}, res.RemovedRoles)
	assert.Empty(t, directProjectBindings(t, f.store, f.owner.ID, f.projectID))

	// The second owner is now the last usable one.
	rec = leaveAs(t, f.srv, second, f.projectID, false)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// TestProjectLeave_GroupDerived409_DetailsOnlyReadableGroups (R3, 6d): a
// caller who reaches the project only through groups is refused with 409;
// details.groups lists only the group the caller may read (A, which they
// own), never group B (reached through nesting, owned by someone else).
// When no group is readable the list is empty.
func TestProjectLeave_GroupDerived409_DetailsOnlyReadableGroups(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	caller := leaveUser(t, f.store, "caller", false)
	gA := leaveGroup(t, f.store, "Leave Group Alpha", caller.ID)
	gB := leaveGroup(t, f.store, "Leave Group Hidden", f.owner.ID)
	leaveAddGroupMember(t, f.store, gA.ID, store.GroupMemberTypeUser, caller.ID)
	leaveAddGroupMember(t, f.store, gB.ID, store.GroupMemberTypeGroup, gA.ID)
	grpBind(t, f.store, store.RoleBindingPrincipalGroup, gA.ID, f.memberRD.ID, f.projectID)
	grpBind(t, f.store, store.RoleBindingPrincipalGroup, gB.ID, f.memberRD.ID, f.projectID)

	rec := leaveAs(t, f.srv, caller, f.projectID, false)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, details := leaveErrorBody(t, rec)
	assert.Equal(t, ErrCodeMembershipGroupDerived, code)
	groups, ok := details["groups"].([]interface{})
	require.True(t, ok, "details.groups is a list: %s", rec.Body.String())
	require.Len(t, groups, 1)
	g := groups[0].(map[string]interface{})
	assert.Equal(t, gA.ID, g["id"])
	assert.Equal(t, gA.Name, g["name"])
	assert.NotContains(t, rec.Body.String(), gB.ID)
	assert.NotContains(t, rec.Body.String(), gB.Name)

	// A caller in group B only: nothing readable, a generic message.
	other := leaveUser(t, f.store, "other", false)
	leaveAddGroupMember(t, f.store, gB.ID, store.GroupMemberTypeUser, other.ID)
	rec = leaveAs(t, f.srv, other, f.projectID, false)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, details = leaveErrorBody(t, rec)
	assert.Equal(t, ErrCodeMembershipGroupDerived, code)
	assert.Equal(t, []interface{}{}, details["groups"])
	assert.NotContains(t, rec.Body.String(), gB.ID)
	assert.NotContains(t, rec.Body.String(), gB.Name)
}

// TestProjectLeave_TokenRefusedSessionOnly (R2): every access token is
// refused, even one carrying project:manage, with the session-only reason;
// an agent credential is refused too. Nothing changes.
func TestProjectLeave_TokenRefusedSessionOnly(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	before := directProjectBindings(t, f.store, f.admin.ID, f.projectID)

	for name, key := range map[string]string{
		"project token with project:manage": mtMint(t, f.srv, f.admin.ID, projectBoundary(f.projectID), "project:manage", "project:read"),
		"hub token":                         mtMint(t, f.srv, f.admin.ID, hubBoundary(), "project:read", "agent:read"),
	} {
		rec := doRequestWithUAT(t, f.srv, key, http.MethodPost, leavePath(f.projectID), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		_, details := leaveErrorBody(t, rec)
		assert.Equal(t, string(leaveSessionOnlyReason), details["reason"], name)
		assert.Equal(t, sessionRequiredCredential, details["credential"], name)
	}
	assert.Equal(t, before, directProjectBindings(t, f.store, f.admin.ID, f.projectID))
	assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))

	// The service refuses a token context on its own, too.
	ctx := realTokenContext(t, f.srv, mtMint(t, f.srv, f.admin.ID, projectBoundary(f.projectID), "project:manage", "project:read"))
	_, decision := f.srv.membershipService.LeaveProject(ctx, NewAuthenticatedUser(f.admin.ID, f.admin.Email, "", "member", "api"), f.projectID, false)
	require.NotNil(t, decision)
	assert.Equal(t, http.StatusForbidden, decision.HTTPStatus)
	assert.Equal(t, before, directProjectBindings(t, f.store, f.admin.ID, f.projectID))
}

// TestProjectLeave_AgentCredentialRefused: an agent's credential cannot
// leave its user's project.
func TestProjectLeave_AgentCredentialRefused(t *testing.T) {
	f := newMSFixture(t, "leave-agent-cred")
	enableProjectLeave(t, f.srv)
	before := directProjectBindings(t, f.s, f.userID, f.projectID)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, leavePath(f.projectID), nil, f.agentToken(f.agentA))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, before, directProjectBindings(t, f.s, f.userID, f.projectID))
}

// TestProjectLeave_DryRunChangesNothingAndMatches: a dry run writes
// nothing (no binding, token, loss check, audit or publish) and its result
// equals the real leave's result.
func TestProjectLeave_DryRunChangesNothingAndMatches(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	f.srv.membershipService.onMembershipLoss = nil
	key := mtMint(t, f.srv, f.member.ID, projectBoundary(f.projectID), "project:read")
	before := directProjectBindings(t, f.store, f.member.ID, f.projectID)

	published := 0
	orig := projectLeavePublish
	projectLeavePublish = func(s *Server, m conduitAuthzMatch) { published++; orig(s, m) }
	t.Cleanup(func() { projectLeavePublish = orig })

	dry := decodeLeaveResult(t, leaveAs(t, f.srv, f.member, f.projectID, true))
	assert.True(t, dry.DryRun)
	assert.Equal(t, 1, dry.ProjectBoundTokens.Revoked, "the dry run reports the token it would revoke")
	assert.Equal(t, before, directProjectBindings(t, f.store, f.member.ID, f.projectID))
	assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))
	assert.Empty(t, lossChecksFor(t, f.store, f.member.ID))
	assert.Zero(t, published)
	_, err := f.srv.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err, "the dry run revokes nothing")

	done := decodeLeaveResult(t, leaveAs(t, f.srv, f.member, f.projectID, false))
	dry.DryRun = false
	assert.Equal(t, dry, done)
	assert.Equal(t, 1, published)
}

// TestProjectLeave_RevokesProjectBoundTokensInTx (R1).
func TestProjectLeave_RevokesProjectBoundTokensInTx(t *testing.T) {
	ctx := context.Background()

	t.Run("no remaining access revokes only this project's tokens of the leaver", func(t *testing.T) {
		f := setupMMRFixture(t)
		enableProjectLeave(t, f.srv)
		grpBind(t, f.store, store.RoleBindingPrincipalUser, f.member.ID, f.memberRD.ID, f.otherProjectID)
		projTok := mtMint(t, f.srv, f.member.ID, projectBoundary(f.projectID), "project:read")
		otherProjTok := mtMint(t, f.srv, f.member.ID, projectBoundary(f.otherProjectID), "project:read")
		hubTok := mtMint(t, f.srv, f.member.ID, hubBoundary(), "project:read", "agent:read")
		adminTok := mtMint(t, f.srv, f.admin.ID, projectBoundary(f.projectID), "project:read")

		// The token works before the leave.
		rec := doRequestWithUAT(t, f.srv, projTok, http.MethodGet, "/api/v1/projects/"+f.projectID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		res := decodeLeaveResult(t, leaveAs(t, f.srv, f.member, f.projectID, false))
		assert.Equal(t, leaveRemainingNone, res.RemainingAccess)
		assert.Equal(t, 1, res.ProjectBoundTokens.Revoked)

		// Its next use fails, through the service and through the route:
		// the stored row is revoked, so there is no cache window.
		_, err := f.srv.uatService.ValidateToken(ctx, projTok)
		require.ErrorIs(t, err, ErrUATRevoked)
		rec = doRequestWithUAT(t, f.srv, projTok, http.MethodGet, "/api/v1/projects/"+f.projectID, nil)
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())

		for name, key := range map[string]string{"other project": otherProjTok, "hub": hubTok, "another user": adminTok} {
			_, err := f.srv.uatService.ValidateToken(ctx, key)
			assert.NoError(t, err, "%s token must be untouched", name)
		}

		recs, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "project_member_leave", TargetID: f.projectID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, recs, 1)
		assert.Contains(t, recs[0].BeforeSummary, "revokedTokenIds")

		// After a re-add the old token stays refused.
		grpBind(t, f.store, store.RoleBindingPrincipalUser, f.member.ID, f.memberRD.ID, f.projectID)
		_, err = f.srv.uatService.ValidateToken(ctx, projTok)
		require.ErrorIs(t, err, ErrUATRevoked)
		rec = doRequestWithUAT(t, f.srv, projTok, http.MethodGet, "/api/v1/projects/"+f.projectID, nil)
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())
	})

	t.Run("remaining group access revokes nothing", func(t *testing.T) {
		f := setupMMRFixture(t)
		enableProjectLeave(t, f.srv)
		g := leaveGroup(t, f.store, "keep-access", f.owner.ID)
		leaveAddGroupMember(t, f.store, g.ID, store.GroupMemberTypeUser, f.member.ID)
		grpBind(t, f.store, store.RoleBindingPrincipalGroup, g.ID, f.memberRD.ID, f.projectID)
		projTok := mtMint(t, f.srv, f.member.ID, projectBoundary(f.projectID), "project:read")

		res := decodeLeaveResult(t, leaveAs(t, f.srv, f.member, f.projectID, false))
		assert.Equal(t, leaveRemainingGroup, res.RemainingAccess)
		assert.Zero(t, res.ProjectBoundTokens.Revoked)
		_, err := f.srv.uatService.ValidateToken(ctx, projTok)
		assert.NoError(t, err)
	})

	t.Run("remaining hub access revokes nothing", func(t *testing.T) {
		f := setupMMRFixture(t)
		enableProjectLeave(t, f.srv)
		super := mtSuperAdmin(t, f.store, "super")
		grpBind(t, f.store, store.RoleBindingPrincipalUser, super.ID, f.memberRD.ID, f.projectID)
		projTok := mtMint(t, f.srv, super.ID, projectBoundary(f.projectID), "project:read")

		res := decodeLeaveResult(t, leaveAs(t, f.srv, super, f.projectID, false))
		assert.Equal(t, leaveRemainingHub, res.RemainingAccess)
		assert.Zero(t, res.ProjectBoundTokens.Revoked)
		_, err := f.srv.uatService.ValidateToken(ctx, projTok)
		assert.NoError(t, err)
	})

	t.Run("a revocation fault rolls back the delete and the audit", func(t *testing.T) {
		f := setupMMRFixture(t)
		enableProjectLeave(t, f.srv)
		f.srv.membershipService.onMembershipLoss = nil
		projTok := mtMint(t, f.srv, f.member.ID, projectBoundary(f.projectID), "project:read")
		before := directProjectBindings(t, f.store, f.member.ID, f.projectID)

		orig := revokeLeaveTokensTx
		revokeLeaveTokensTx = func(ctx context.Context, tx store.Store, userID, projectID string) ([]string, error) {
			if _, err := orig(ctx, tx, userID, projectID); err != nil {
				return nil, err
			}
			return nil, errors.New("injected revocation fault")
		}
		t.Cleanup(func() { revokeLeaveTokensTx = orig })

		rec := leaveAs(t, f.srv, f.member, f.projectID, false)
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		assert.Equal(t, before, directProjectBindings(t, f.store, f.member.ID, f.projectID))
		assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))
		assert.Empty(t, lossChecksFor(t, f.store, f.member.ID))
		_, err := f.srv.uatService.ValidateToken(ctx, projTok)
		assert.NoError(t, err, "the token revocation rolled back with the leave")
	})
}

// TestProjectLeave_UnclassifiedFailsClosed (R3, 6a): a caller binding the
// classifier cannot classify fails the leave with 500
// leave_binding_unclassified, and no binding, token or audit row changes.
func TestProjectLeave_UnclassifiedFailsClosed(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	f.srv.membershipService.onMembershipLoss = nil
	grpBind(t, f.store, store.RoleBindingPrincipalUser, f.member.ID, f.withinCeiling.ID, f.projectID)
	key := mtMint(t, f.srv, f.member.ID, projectBoundary(f.projectID), "project:read")
	before := directProjectBindings(t, f.store, f.member.ID, f.projectID)

	orig := leaveBindingClassFunc
	leaveBindingClassFunc = func(rb *store.RoleBinding, rd *store.RoleDefinition, projectID, callerID string) leaveClass {
		if rd != nil && rd.ID == f.withinCeiling.ID {
			return leaveClassUnclassified
		}
		return orig(rb, rd, projectID, callerID)
	}
	t.Cleanup(func() { leaveBindingClassFunc = orig })

	for _, dry := range []bool{true, false} {
		rec := leaveAs(t, f.srv, f.member, f.projectID, dry)
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		code, _ := leaveErrorBody(t, rec)
		assert.Equal(t, ErrCodeLeaveBindingUnclassified, code)
	}
	assert.Equal(t, before, directProjectBindings(t, f.store, f.member.ID, f.projectID))
	assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))
	assert.Empty(t, lossChecksFor(t, f.store, f.member.ID))
	_, err := f.srv.uatService.ValidateToken(context.Background(), key)
	assert.NoError(t, err)
}

// TestProjectLeave_ConstraintAdminLockout: the last constraint admin of
// the project cannot leave.
func TestProjectLeave_ConstraintAdminLockout(t *testing.T) {
	srv, s := testServer(t)
	enableProjectLeave(t, srv)
	projectID, users, bindings := lockoutProjectAdmins(t, s, "leave-lockout", 1)
	memberRD, err := s.GetRoleDefinitionByName(context.Background(), store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	grpBind(t, s, store.RoleBindingPrincipalUser, users[0], memberRD.ID, projectID)
	u, err := s.GetUser(context.Background(), users[0])
	require.NoError(t, err)

	rec := leaveAs(t, srv, u, projectID, false)
	requireLockoutRefusal(t, rec.Code, rec.Body.Bytes())
	requireBindingExists(t, s, bindings[0].ID, true)
	assert.Len(t, directProjectBindings(t, s, users[0], projectID), 2)
}

// TestProjectLeave_DryRunAgentsCapped (R4): with more than 50 rooted
// agents the dry run lists exactly 50 items, sets truncated and keeps the
// count exact; each item has exactly the keys id, name and phase.
func TestProjectLeave_DryRunAgentsCapped(t *testing.T) {
	f := newMSFixture(t, "leave-cap")
	enableProjectLeave(t, f.srv)
	for i := 0; i < leaveAgentItemsMax+1; i++ {
		f.userAgent(fmt.Sprintf("cap-%02d", i), f.userID)
	}
	want := leaveAgentItemsMax + 1 + 2 // plus the fixture's agent A and child C
	user, err := f.s.GetUser(context.Background(), f.userID)
	require.NoError(t, err)

	rec := leaveAs(t, f.srv, user, f.projectID, true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw struct {
		Agents struct {
			Count     int                      `json:"count"`
			Truncated bool                     `json:"truncated"`
			Items     []map[string]interface{} `json:"items"`
		} `json:"agents"`
		WillBeHeld bool `json:"willBeHeld"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, want, raw.Agents.Count)
	assert.True(t, raw.Agents.Truncated)
	assert.True(t, raw.WillBeHeld)
	require.Len(t, raw.Agents.Items, leaveAgentItemsMax)
	for _, item := range raw.Agents.Items {
		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		assert.Equal(t, []string{"id", "name", "phase"}, keys)
	}
}

// TestProjectLeave_SweepHoldsRootedAgents: after a leave with no remaining
// access, the membership check (trigger member_leave) holds every agent the
// user roots, descendants included; a re-add does not lift the holds. With
// remaining group access nothing is held.
func TestProjectLeave_SweepHoldsRootedAgents(t *testing.T) {
	ctx := context.Background()

	t.Run("no remaining access holds the tree", func(t *testing.T) {
		f := newMSFixture(t, "leave-sweep")
		enableProjectLeave(t, f.srv)
		user, err := f.s.GetUser(ctx, f.userID)
		require.NoError(t, err)

		res := decodeLeaveResult(t, leaveAs(t, f.srv, user, f.projectID, false))
		assert.Equal(t, leaveRemainingNone, res.RemainingAccess)
		assert.Equal(t, 2, res.Agents.Count)
		assert.True(t, res.WillBeHeld)
		requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberLeave)

		f.requireTreeHeldAndRefused()
		recs, _, err := f.s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeMembershipLossProcessed, TargetID: f.userID, Limit: 10})
		require.NoError(t, err)
		sawLeave := false
		for _, r := range recs {
			if bytes.Contains([]byte(r.AfterSummary), []byte(`"trigger":"`+string(store.MembershipLossTriggerMemberLeave)+`"`)) {
				sawLeave = true
			}
		}
		assert.True(t, sawLeave, "membership_loss_processed names the member_leave trigger")

		f.addMember(f.userID, store.ProjectRoleMember)
		f.srv.drainMembershipLossChecks(ctx)
		assert.True(t, f.held(f.agentA.ID), "a re-add does not lift the hold")
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonAgentHeld)
	})

	t.Run("remaining group access holds nothing", func(t *testing.T) {
		f := newMSFixture(t, "leave-sweep-group")
		enableProjectLeave(t, f.srv)
		memberRD, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		g := leaveGroup(t, f.s, "sweep-keep", f.ownerID)
		leaveAddGroupMember(t, f.s, g.ID, store.GroupMemberTypeUser, f.userID)
		grpBind(t, f.s, store.RoleBindingPrincipalGroup, g.ID, memberRD.ID, f.projectID)
		user, err := f.s.GetUser(ctx, f.userID)
		require.NoError(t, err)

		res := decodeLeaveResult(t, leaveAs(t, f.srv, user, f.projectID, false))
		assert.Equal(t, leaveRemainingGroup, res.RemainingAccess)
		assert.False(t, res.WillBeHeld)
		f.srv.drainMembershipLossChecks(ctx)
		assert.False(t, f.held(f.agentA.ID))
		assert.False(t, f.held(f.childC.ID))
		require.NoError(t, f.srv.agentStanding(ctx, f.childC.ID))
	})
}

// TestProjectLeave_Concurrent: two leaves by one user serialize to 200 then
// 404; the last two owners leaving at once give exactly one 409.
func TestProjectLeave_Concurrent(t *testing.T) {
	run := func(fns ...func() int) []int {
		codes := make([]int, len(fns))
		var wg sync.WaitGroup
		for i, fn := range fns {
			wg.Add(1)
			go func(i int, fn func() int) {
				defer wg.Done()
				codes[i] = fn()
			}(i, fn)
		}
		wg.Wait()
		sort.Ints(codes)
		return codes
	}

	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	codes := run(leaveRequestFunc(t, f.srv, f.member, f.projectID), leaveRequestFunc(t, f.srv, f.member, f.projectID))
	assert.Equal(t, []int{http.StatusOK, http.StatusNotFound}, codes)
	assert.Equal(t, 1, countAudits(t, f.store, "project_member_leave", f.projectID))

	second := leaveUser(t, f.store, "second-owner", true)
	grpBind(t, f.store, store.RoleBindingPrincipalUser, second.ID, f.ownerRD.ID, f.projectID)
	codes = run(leaveRequestFunc(t, f.srv, f.owner, f.projectID), leaveRequestFunc(t, f.srv, second, f.projectID))
	assert.Equal(t, []int{http.StatusOK, http.StatusConflict}, codes)
}

// TestProjectLeave_MemberDeleteMatrixUnchanged: leave does not change the
// governance matrix. A member's DELETE on their own binding is still 403
// role_assignment_forbidden.
func TestProjectLeave_MemberDeleteMatrixUnchanged(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	binding := pinBindingID(t, f.store, f.member.ID, f.projectID)
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, "/api/v1/projects/"+f.projectID+"/members/"+binding, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.True(t, mtBindingExists(t, f.store, binding))
}

// TestProjectLeave_Capabilities: canLeave and leaveBlockedReason appear on
// the members list and the project while the experiment is on, and are
// left out while it is off.
func TestProjectLeave_Capabilities(t *testing.T) {
	f := setupMMRFixture(t)

	type caps struct {
		CanLeave           *bool  `json:"canLeave"`
		LeaveBlockedReason string `json:"leaveBlockedReason"`
	}
	membersCaps := func(u *store.User) caps {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, u, http.MethodGet, "/api/v1/projects/"+f.projectID+"/members", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Caps caps `json:"_capabilities"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Caps
	}
	projectCaps := func(u *store.User) caps {
		t.Helper()
		rec := doRequestAsUser(t, f.srv, u, http.MethodGet, "/api/v1/projects/"+f.projectID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Caps caps `json:"_capabilities"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.Caps
	}

	setExperimentOverrides(t, f.srv, map[string]bool{experiments.ProjectSelfLeave: false})
	assert.Nil(t, membersCaps(f.member).CanLeave)
	assert.Nil(t, projectCaps(f.member).CanLeave)

	enableProjectLeave(t, f.srv)
	for _, c := range []caps{membersCaps(f.member), projectCaps(f.member)} {
		require.NotNil(t, c.CanLeave)
		assert.True(t, *c.CanLeave)
		assert.Empty(t, c.LeaveBlockedReason)
	}
	for _, c := range []caps{membersCaps(f.owner), projectCaps(f.owner)} {
		require.NotNil(t, c.CanLeave)
		assert.False(t, *c.CanLeave)
		assert.Equal(t, leaveBlockedLastOwner, c.LeaveBlockedReason)
	}

	groupOnly := leaveUser(t, f.store, "group-only", true)
	g := leaveGroup(t, f.store, "caps-group", f.owner.ID)
	leaveAddGroupMember(t, f.store, g.ID, store.GroupMemberTypeUser, groupOnly.ID)
	grpBind(t, f.store, store.RoleBindingPrincipalGroup, g.ID, f.memberRD.ID, f.projectID)
	c := projectCaps(groupOnly)
	require.NotNil(t, c.CanLeave)
	assert.False(t, *c.CanLeave)
	assert.Equal(t, leaveBlockedGroupDerived, c.LeaveBlockedReason)
}

// TestProjectLeave_SelfOnlyCannotTargetAnother: the leave route acts only
// on the caller. A superior (the owner) and a peer (another member) that
// name another user in the body are refused with 400 and nothing changes;
// a superior's leave never touches another member's bindings.
func TestProjectLeave_SelfOnlyCannotTargetAnother(t *testing.T) {
	f := setupMMRFixture(t)
	enableProjectLeave(t, f.srv)
	peer := leaveUser(t, f.store, "peer", true)
	grpBind(t, f.store, store.RoleBindingPrincipalUser, peer.ID, f.memberRD.ID, f.projectID)
	memberBefore := directProjectBindings(t, f.store, f.member.ID, f.projectID)
	ownerBefore := directProjectBindings(t, f.store, f.owner.ID, f.projectID)
	peerBefore := directProjectBindings(t, f.store, peer.ID, f.projectID)
	binding := pinBindingID(t, f.store, f.member.ID, f.projectID)

	for name, actor := range map[string]*store.User{"superior": f.owner, "peer": peer} {
		for _, body := range []map[string]interface{}{
			{"userId": f.member.ID},
			{"principalId": f.member.ID, "principalType": "user"},
			{"bindingId": binding},
			{"dryRun": false, "memberId": f.member.ID},
		} {
			rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, leavePath(f.projectID), body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s %v: %s", name, body, rec.Body.String())
		}
	}
	assert.Equal(t, memberBefore, directProjectBindings(t, f.store, f.member.ID, f.projectID))
	assert.Equal(t, ownerBefore, directProjectBindings(t, f.store, f.owner.ID, f.projectID))
	assert.Equal(t, peerBefore, directProjectBindings(t, f.store, peer.ID, f.projectID))
	assert.Zero(t, countAudits(t, f.store, "project_member_leave", f.projectID))

	// The peer's own leave removes only the peer.
	res := decodeLeaveResult(t, leaveAs(t, f.srv, peer, f.projectID, false))
	assert.Equal(t, []string{store.ProjectRoleMember}, res.RemovedRoles)
	assert.Empty(t, directProjectBindings(t, f.store, peer.ID, f.projectID))
	assert.Equal(t, memberBefore, directProjectBindings(t, f.store, f.member.ID, f.projectID))
}
