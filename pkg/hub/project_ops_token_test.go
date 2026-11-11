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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectOpsProject creates a project and returns its ID.
func projectOpsProject(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: id, Name: name, Slug: id}))
	return id
}

// projectOpsUser creates an active user with hub membership. With
// superAdmin set, the user also holds the super-admin system role, while
// its user record role stays "member", so it is not a local unscoped hub
// admin.
func projectOpsUser(t *testing.T, s store.Store, name string, superAdmin bool) string {
	t.Helper()
	id := tid(name)
	if superAdmin {
		createTestUserWithRole(t, s, id, id+"@test.com", "member", store.SystemRoleSuperAdmin)
	} else {
		require.NoError(t, s.CreateUser(context.Background(), &store.User{
			ID: id, Email: id + "@test.com", DisplayName: id, Role: "member", Status: "active",
		}))
	}
	ensureHubMembership(context.Background(), s, id)
	return id
}

// projectOpsGrant binds userID to a project-scoped built-in role.
func projectOpsGrant(t *testing.T, s store.Store, userID, projectID, role string) {
	t.Helper()
	createTestUserWithProjectRole(t, s, userID, userID+"@test.com", projectID, role)
}

// projectOpsMint mints a real token for userID through
// UserAccessTokenService over a session.
func projectOpsMint(t *testing.T, srv *Server, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "pot-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key
}

// projectOpsSession sends a request to the project routes as userID over an
// interactive session.
func projectOpsSession(t *testing.T, srv *Server, userID, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	identity := NewAuthenticatedUser(userID, userID+"@test.com", userID, "member", string(ClientTypeAPI))
	ctx := contextWithIdentity(context.Background(), identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "pot-session"})
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleProjectRoutes(rec, req)
	return rec
}

// messagingPolicyBody returns a PUT body that sets the inbound policy to
// value at the project's current revision.
func messagingPolicyBody(t *testing.T, s store.Store, projectID, value string) map[string]interface{} {
	t.Helper()
	p, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	return map[string]interface{}{"crossProjectInbound": value, "expectedRevision": p.CrossProjectInboundRevision}
}

func messagingPolicyPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/messaging-policy"
}

// TestProjectMessagingPolicyPut_RequiresSetMessagingPolicySelector requires
// a token that changes a project's messaging policy to carry the
// project:set_messaging_policy selector: a direct owner's token with other
// project selectors is refused, and the same owner's project or hub token
// with the selector changes the policy.
func TestProjectMessagingPolicyPut_RequiresSetMessagingPolicySelector(t *testing.T) {
	srv, s := testServer(t)
	project := projectOpsProject(t, s, "pmp-sel")
	owner := projectOpsUser(t, s, "pmp-sel-owner", false)
	projectOpsGrant(t, s, owner, project, store.ProjectRoleOwner)

	without := projectOpsMint(t, srv, owner, projectBoundary(project), "project:read", "project:update", "project:manage")
	rec := doRequestWithToken(t, srv, without, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, store.CrossProjectInboundMembers))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	p, err := s.GetProject(context.Background(), project)
	require.NoError(t, err)
	assert.NotEqual(t, store.CrossProjectInboundMembers, p.CrossProjectInbound, "a refused change writes nothing")

	for _, boundary := range []TokenBoundary{projectBoundary(project), hubBoundary()} {
		key := projectOpsMint(t, srv, owner, boundary, "project:set_messaging_policy")
		value := store.CrossProjectInboundMembers
		if boundary.Kind == BoundaryKindHub {
			value = store.CrossProjectInboundAny
		}
		rec := doRequestWithToken(t, srv, key, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, value))
		require.Equal(t, http.StatusOK, rec.Code, "%s token with the selector: %s", boundary.Kind, rec.Body.String())
		p, err := s.GetProject(context.Background(), project)
		require.NoError(t, err)
		assert.Equal(t, value, p.CrossProjectInbound)
	}
}

// TestProjectMessagingPolicyPut_ProjectBoundaryLimitedToItsProject requires
// a project token with project:set_messaging_policy to change only its own
// project's policy, even when its holder owns another project too.
func TestProjectMessagingPolicyPut_ProjectBoundaryLimitedToItsProject(t *testing.T) {
	srv, s := testServer(t)
	home := projectOpsProject(t, s, "pmp-home")
	other := projectOpsProject(t, s, "pmp-other")
	owner := projectOpsUser(t, s, "pmp-bound-owner", false)
	projectOpsGrant(t, s, owner, home, store.ProjectRoleOwner)
	projectOpsGrant(t, s, owner, other, store.ProjectRoleOwner)

	key := projectOpsMint(t, srv, owner, projectBoundary(home), "project:set_messaging_policy")
	rec := doRequestWithToken(t, srv, key, http.MethodPut, messagingPolicyPath(other), messagingPolicyBody(t, s, other, store.CrossProjectInboundAny))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	p, err := s.GetProject(context.Background(), other)
	require.NoError(t, err)
	assert.NotEqual(t, store.CrossProjectInboundAny, p.CrossProjectInbound, "the other project's policy is not changed")

	rec = doRequestWithToken(t, srv, key, http.MethodPut, messagingPolicyPath(home), messagingPolicyBody(t, s, home, store.CrossProjectInboundAny))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestProjectMessagingPolicyPut_OwnerRuleStillApplies requires every
// credential that passes the project.set_messaging_policy check to also
// satisfy the owner rule: a direct owner's session changes the policy, a
// project admin's session is refused, and a super-admin who is not a
// direct owner is refused with a session and with a token carrying the
// selector, while a direct owner's token with the selector is admitted.
func TestProjectMessagingPolicyPut_OwnerRuleStillApplies(t *testing.T) {
	srv, s := testServer(t)
	project := projectOpsProject(t, s, "pmp-rule")

	owner := projectOpsUser(t, s, "pmp-rule-owner", false)
	projectOpsGrant(t, s, owner, project, store.ProjectRoleOwner)
	rec := projectOpsSession(t, srv, owner, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, store.CrossProjectInboundMembers))
	require.Equal(t, http.StatusOK, rec.Code, "direct owner session: %s", rec.Body.String())

	admin := projectOpsUser(t, s, "pmp-rule-admin", false)
	projectOpsGrant(t, s, admin, project, store.ProjectRoleAdmin)
	rec = projectOpsSession(t, srv, admin, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, store.CrossProjectInboundAny))
	require.Equal(t, http.StatusForbidden, rec.Code, "project admin session: %s", rec.Body.String())
	// The project admin lacks project.set_messaging_policy, so this is the
	// permission check's refusal.
	permissionRefusal := rec.Body.String()

	super := projectOpsUser(t, s, "pmp-rule-super", true)
	projectOpsGrant(t, s, super, project, store.ProjectRoleMember)
	allowed := srv.authzService.CheckAccess(context.Background(),
		NewAuthenticatedUser(super, super+"@test.com", super, "member", string(ClientTypeAPI)),
		Resource{Type: "project", ID: project}, ActionSetMessagingPolicy)
	require.True(t, allowed.Allowed, "the super-admin holds project.set_messaging_policy: %s", allowed.Reason)

	rec = projectOpsSession(t, srv, super, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, store.CrossProjectInboundAny))
	require.Equal(t, http.StatusForbidden, rec.Code, "super-admin session without direct ownership: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), messagingPolicyOwnerRuleMessage)
	assert.Equal(t, permissionRefusal, rec.Body.String(),
		"the owner rule's refusal is the same response as the permission check's refusal")

	key := projectOpsMint(t, srv, super, hubBoundary(), "project:set_messaging_policy")
	rec = doRequestWithToken(t, srv, key, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, store.CrossProjectInboundAny))
	require.Equal(t, http.StatusForbidden, rec.Code, "super-admin token without direct ownership: %s", rec.Body.String())
	assert.Equal(t, permissionRefusal, rec.Body.String(),
		"the owner rule's refusal of a token is the same response as the permission check's refusal")

	p, err := s.GetProject(context.Background(), project)
	require.NoError(t, err)
	assert.Equal(t, store.CrossProjectInboundMembers, p.CrossProjectInbound, "refused changes write nothing")

	ownerSuper := projectOpsUser(t, s, "pmp-rule-owner-super", true)
	projectOpsGrant(t, s, ownerSuper, project, store.ProjectRoleOwner)
	key = projectOpsMint(t, srv, ownerSuper, hubBoundary(), "project:set_messaging_policy")
	rec = doRequestWithToken(t, srv, key, http.MethodPut, messagingPolicyPath(project), messagingPolicyBody(t, s, project, store.CrossProjectInboundAny))
	require.Equal(t, http.StatusOK, rec.Code, "a direct owner's token with the selector: %s", rec.Body.String())
}

func setTemplatePath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/set-template"
}

// TestSetTemplate_RequiresUpdateAndCloneOnTheProject requires a caller that
// marks a project as a template to pass both project.update and
// project.clone on that project. A token is admitted only when it carries
// project:update and project:clone and its holder passes both permissions
// on the project; missing either selector, or a holder who cannot pass
// project.clone on the project, is refused, and a project-boundary token
// cannot carry project:clone at all.
func TestSetTemplate_RequiresUpdateAndCloneOnTheProject(t *testing.T) {
	srv, s := testServer(t)
	project := projectOpsProject(t, s, "stp")
	other := projectOpsProject(t, s, "stp-other")
	body := map[string]interface{}{"isTemplate": true}

	// A super-admin who is a project member passes project.update and
	// project.clone on the project, so its token can mark it.
	super := projectOpsUser(t, s, "stp-super", true)
	projectOpsGrant(t, s, super, project, store.ProjectRoleMember)
	projectOpsGrant(t, s, super, other, store.ProjectRoleMember)

	for _, scopes := range [][]string{{"project:update"}, {"project:clone"}, {"project:read", "project:manage"}} {
		key := projectOpsMint(t, srv, super, hubBoundary(), scopes...)
		rec := doRequestWithToken(t, srv, key, http.MethodPost, setTemplatePath(project), body)
		require.Equal(t, http.StatusForbidden, rec.Code, "token with %v: %s", scopes, rec.Body.String())
	}
	// A project-boundary token carrying project:clone cannot be minted: a
	// project boundary takes its selectors from the holder's project role,
	// and no project role holds project.clone. The refusal names the
	// selector, so a project token never reaches set-template with it.
	_, _, mintErr := srv.uatService.CreateTokenWithParams(rs4MintContext(super), CreateTokenParams{
		UserID: super, Name: "pot-" + tid("other"), Boundary: projectBoundary(other), Scopes: []string{"project:clone"},
	})
	require.Error(t, mintErr, "a project token with project:clone must not mint")
	require.Contains(t, mintErr.Error(), `selector "project:clone" denied`)
	p, err := s.GetProject(context.Background(), project)
	require.NoError(t, err)
	require.NotEqual(t, "true", p.Labels[store.LabelTemplate], "refused calls write nothing")

	both := projectOpsMint(t, srv, super, hubBoundary(), "project:clone", "project:update")
	rec := doRequestWithToken(t, srv, both, http.MethodPost, setTemplatePath(project), body)
	require.Equal(t, http.StatusOK, rec.Code, "token with both selectors: %s", rec.Body.String())
	p, err = s.GetProject(context.Background(), project)
	require.NoError(t, err)
	assert.Equal(t, "true", p.Labels[store.LabelTemplate])

	// A super-admin with both selectors who is not a project member does
	// not pass project.clone on the project for a token.
	outsider := projectOpsUser(t, s, "stp-outsider", true)
	outsiderKey := projectOpsMint(t, srv, outsider, hubBoundary(), "project:clone", "project:update")
	rec = doRequestWithToken(t, srv, outsiderKey, http.MethodPost, setTemplatePath(other), body)
	require.Equal(t, http.StatusForbidden, rec.Code, "non-member token: %s", rec.Body.String())

	// A project owner holds project.update but not project.clone: its
	// session and its project:update token are refused.
	owner := projectOpsUser(t, s, "stp-owner", false)
	projectOpsGrant(t, s, owner, other, store.ProjectRoleOwner)
	rec = projectOpsSession(t, srv, owner, http.MethodPost, setTemplatePath(other), body)
	require.Equal(t, http.StatusForbidden, rec.Code, "owner session without project.clone: %s", rec.Body.String())
	ownerKey := projectOpsMint(t, srv, owner, projectBoundary(other), "project:update")
	rec = doRequestWithToken(t, srv, ownerKey, http.MethodPost, setTemplatePath(other), body)
	require.Equal(t, http.StatusForbidden, rec.Code, "owner token without project:clone: %s", rec.Body.String())

	// A super-admin session passes both permissions.
	rec = projectOpsSession(t, srv, outsider, http.MethodPost, setTemplatePath(other), body)
	require.Equal(t, http.StatusOK, rec.Code, "super-admin session: %s", rec.Body.String())
}

// TestTemplateImport_RequiresTemplateCreate requires template discover and
// import on a project to check template.create, and harness-config discover
// and import to check harness_config.create: a token carrying agent:create
// (or the other kind's create selector) is refused, a token carrying the
// matching create selector passes authorization, and the matching selector
// on a token bound to another project, a non-member's hub token and a
// non-member session are refused.
func TestTemplateImport_RequiresTemplateCreate(t *testing.T) {
	srv, s := testServer(t)
	project := projectOpsProject(t, s, "tic")
	member := projectOpsUser(t, s, "tic-member", false)
	projectOpsGrant(t, s, member, project, store.ProjectRoleMember)
	// The member also belongs to a second project, so it can mint a token
	// bound there; an outsider belongs only to the second project.
	elsewhere := projectOpsProject(t, s, "tic-elsewhere")
	projectOpsGrant(t, s, member, elsewhere, store.ProjectRoleMember)
	outsider := projectOpsUser(t, s, "tic-outsider", false)
	projectOpsGrant(t, s, outsider, elsewhere, store.ProjectRoleMember)

	cases := []struct {
		route    string
		selector string
		wrong    []string
	}{
		{"discover-templates", "template:create", []string{"agent:create", "harness_config:create"}},
		{"import-templates", "template:create", []string{"agent:create", "harness_config:create"}},
		{"discover-harness-configs", "harness_config:create", []string{"agent:create", "template:create"}},
		{"import-harness-configs", "harness_config:create", []string{"agent:create", "template:create"}},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			path := "/api/v1/projects/" + project + "/" + tc.route
			for _, wrong := range tc.wrong {
				key := projectOpsMint(t, srv, member, projectBoundary(project), wrong)
				rec := doRequestWithToken(t, srv, key, http.MethodPost, path, map[string]interface{}{})
				require.Equal(t, http.StatusForbidden, rec.Code, "token with %s: %s", wrong, rec.Body.String())
			}
			for _, boundary := range []TokenBoundary{projectBoundary(project), hubBoundary()} {
				key := projectOpsMint(t, srv, member, boundary, tc.selector)
				rec := doRequestWithToken(t, srv, key, http.MethodPost, path, map[string]interface{}{})
				requirePassedImportAuthorization(t, rec, "%s token with %s", boundary.Kind, tc.selector)
			}
			rec := projectOpsSession(t, srv, member, http.MethodPost, path, map[string]interface{}{})
			requirePassedImportAuthorization(t, rec, "member session")

			// The matching selector does not reach a project outside the
			// token's boundary or the holder's membership.
			rec = doRequestWithToken(t, srv, projectOpsMint(t, srv, member, projectBoundary(elsewhere), tc.selector),
				http.MethodPost, path, map[string]interface{}{})
			require.Equal(t, http.StatusForbidden, rec.Code, "token bound to another project: %s", rec.Body.String())
			rec = doRequestWithToken(t, srv, projectOpsMint(t, srv, outsider, hubBoundary(), tc.selector),
				http.MethodPost, path, map[string]interface{}{})
			require.Equal(t, http.StatusForbidden, rec.Code, "non-member hub token: %s", rec.Body.String())
			rec = projectOpsSession(t, srv, outsider, http.MethodPost, path, map[string]interface{}{})
			require.Equal(t, http.StatusForbidden, rec.Code, "non-member session: %s", rec.Body.String())
		})
	}
}

// requirePassedImportAuthorization asserts that a discover or import request
// with an empty body passed authorization: discover answers 400 for the
// missing source, and import answers 503 because the test server configures
// no resource storage. A 401 or 403 fails.
func requirePassedImportAuthorization(t *testing.T, rec *httptest.ResponseRecorder, msg string, args ...interface{}) {
	t.Helper()
	require.Contains(t, []int{http.StatusBadRequest, http.StatusServiceUnavailable}, rec.Code,
		append([]interface{}{msg + ": %s"}, append(args, rec.Body.String())...)...)
}
