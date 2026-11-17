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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bindProjectRoleForGitHubTriggerTest(t *testing.T, s store.Store, projectID, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
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

func createSystemUserForGitHubTriggerTest(t *testing.T, s store.Store, prefix string, systemRole string) *store.User {
	t.Helper()
	id := hubConfigTokenUser(t, s, prefix, systemRole)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func createUserForGitHubTriggerTest(t *testing.T, s store.Store, prefix string, legacyRole string) *store.User {
	t.Helper()
	id := tid(prefix)
	u := &store.User{
		ID:          id,
		Email:       id + "@example.com",
		DisplayName: prefix,
		Role:        legacyRole,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func doRawTriggerSettingsAsUser(t *testing.T, srv *Server, user *store.User, method, path string, rawBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(rawBody))
	if len(rawBody) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	authUser := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "api")
	ctx := contextWithCredentialContext(
		contextWithIdentity(req.Context(), authUser),
		CredentialContext{Kind: CredentialKindInteractive},
	)
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

func TestValidateUnmatchedCommandText_Unit(t *testing.T) {
	assert.NoError(t, validateUnmatchedCommandText(""))
	assert.NoError(t, validateUnmatchedCommandText(strings.Repeat("a", MaxGitHubTriggersUnmatchedTextBytes)))
	assert.Error(t, validateUnmatchedCommandText(strings.Repeat("a", MaxGitHubTriggersUnmatchedTextBytes+1)))
	assert.Error(t, validateUnmatchedCommandText("bad\x00nul"))
	assert.Error(t, validateUnmatchedCommandText(string([]byte{0xff, 0xfe})))
}

func TestGitHubTriggerPermissions_RoleMatrixAndTestAdminPolicy(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()

	owner := taUser(t, s, "ght-perm-owner", store.SystemRoleHubMember)
	admin := taUser(t, s, "ght-perm-admin", store.SystemRoleHubMember)
	member := taUser(t, s, "ght-perm-member", store.SystemRoleHubMember)
	viewer := taUser(t, s, "ght-perm-viewer", store.SystemRoleHubMember)
	outsider := taUser(t, s, "ght-perm-outsider", store.SystemRoleHubMember)
	humanSuper := taUser(t, s, "ght-perm-super", store.SystemRoleSuperAdmin)
	issuer := taUser(t, s, "ght-perm-issuer", store.SystemRoleSuperAdmin)

	projectID := tid("ght-perm-proj")
	taProject(t, s, projectID, owner.ID)
	bindProjectRoleForGitHubTriggerTest(t, s, projectID, admin.ID, store.ProjectRoleAdmin)
	bindProjectRoleForGitHubTriggerTest(t, s, projectID, member.ID, store.ProjectRoleMember)
	bindProjectRoleForGitHubTriggerTest(t, s, projectID, viewer.ID, store.ProjectRoleViewer)

	testSuper := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	testSuperIdent := tsaIdentity(t, s, testSuper.Identity.ID)

	proj, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	res := githubTriggerResource(proj)

	decideUser := func(u *store.User, action, permID string) Decision {
		ident := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "api")
		return srv.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(ident),
			Credential: CredentialContext{Kind: CredentialKindInteractive},
			Resource:   res,
			Action:     Action(action),
			Permission: permID,
		})
	}

	cases := []struct {
		name          string
		user          *store.User
		wantComment   bool
		wantConfigure bool
	}{
		{"project-owner", owner, true, true},
		{"project-admin", admin, true, true},
		{"project-member", member, true, false},
		{"project-viewer", viewer, false, false},
		{"non-member", outsider, false, false},
		{"human-super-admin", humanSuper, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dc := decideUser(tc.user, permissions.ActionComment, "github_trigger.comment")
			assert.Equal(t, tc.wantComment, dc.Allowed, "github_trigger.comment: %s", dc.Reason)

			dcfg := decideUser(tc.user, permissions.ActionConfigure, "github_trigger.configure")
			assert.Equal(t, tc.wantConfigure, dcfg.Allowed, "github_trigger.configure: %s", dcfg.Reason)
		})
	}

	// github_trigger.comment is allow under test-admin policy (like project.chat_post,
	// preserving group-based project-member delegation; exercise still needs
	// hub.github_triggers, which is deny for test admins), while github_trigger.configure
	// is denied at step 0c and remains grantable at project scope.
	dComment := srv.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(testSuperIdent),
		Credential: credentialContextForIdentity(testSuperIdent),
		Resource:   res,
		Action:     Action(permissions.ActionComment),
		Permission: "github_trigger.comment",
	})
	assert.True(t, dComment.Allowed, "test_super_admin github_trigger.comment should pass step 0c: %s", dComment.Reason)

	dConfigure := srv.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(testSuperIdent),
		Credential: credentialContextForIdentity(testSuperIdent),
		Resource:   res,
		Action:     Action(permissions.ActionConfigure),
		Permission: "github_trigger.configure",
	})
	assert.False(t, dConfigure.Allowed, "test_super_admin must be denied github_trigger.configure")
	assert.Equal(t, DeniedByTestAdminPolicy, dConfigure.DeniedBy, "test_super_admin github_trigger.configure denial reason: %s", dConfigure.Reason)

	for _, roleName := range []string{store.ProjectRoleOwner, store.ProjectRoleAdmin, store.ProjectRoleMember} {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		grantee := taUser(t, s, "ght-perm-grantee-"+roleName, store.SystemRoleHubMember)
		rec := doRequestWithToken(t, srv, testSuper.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
			addProjectMemberRequest{RoleDefinitionID: rd.ID, PrincipalType: "user", PrincipalID: grantee.ID})
		assert.Less(t, rec.Code, 300, "test_super_admin must still be able to grant %s: %s", roleName, rec.Body.String())
	}
}

func TestProjectGitHubTriggerSettings_GatingAndAuthz(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := createUserForGitHubTriggerTest(t, s, "ght-auth-owner", store.UserRoleMember)
	admin := createUserForGitHubTriggerTest(t, s, "ght-auth-admin", store.UserRoleMember)
	member := createUserForGitHubTriggerTest(t, s, "ght-auth-member", store.UserRoleMember)
	viewer := createUserForGitHubTriggerTest(t, s, "ght-auth-viewer", store.UserRoleMember)
	outsider := createUserForGitHubTriggerTest(t, s, "ght-auth-outsider", store.UserRoleMember)
	super := createSystemUserForGitHubTriggerTest(t, s, "ght-auth-super", store.SystemRoleSuperAdmin)

	projID := tid("ght-auth-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:      projID,
		Name:    "auth-proj",
		Slug:    "auth-proj",
		Created: time.Now(),
		Updated: time.Now(),
	}))
	bindProjectRoleForGitHubTriggerTest(t, s, projID, owner.ID, store.ProjectRoleOwner)
	bindProjectRoleForGitHubTriggerTest(t, s, projID, admin.ID, store.ProjectRoleAdmin)
	bindProjectRoleForGitHubTriggerTest(t, s, projID, member.ID, store.ProjectRoleMember)
	bindProjectRoleForGitHubTriggerTest(t, s, projID, viewer.ID, store.ProjectRoleViewer)

	path := "/api/v1/projects/" + projID + "/trigger-sources/github"
	validPut := ProjectGitHubTriggerSettingsUpdateRequest{
		UnmatchedCommandReply: true,
		UnmatchedCommandText:  "Try /help",
	}

	// 1. Experiment off -> every HTTP method (including POST/DELETE/PATCH) returns 404 without leaking route existence via 405.
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rr := doRequestAsUser(t, srv, owner, m, path, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code, "method %s with hub.github_triggers off", m)
		assert.Empty(t, rr.Header().Get("Allow"), "method %s with hub.github_triggers off must not set Allow", m)
	}
	rr := doRequestAsUser(t, srv, owner, http.MethodPut, path, validPut)
	assert.Equal(t, http.StatusNotFound, rr.Code)

	// 2. Enable experiment -> unsupported methods answer 405 with Allow: GET, PUT, and unknown trigger-sources subpaths return 404.
	setGitHubTriggersExperimentForRouting(t, srv, true)
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rr = doRequestAsUser(t, srv, owner, m, path, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "method %s with hub.github_triggers on", m)
		assert.Contains(t, rr.Header().Get("Allow"), http.MethodGet)
		assert.Contains(t, rr.Header().Get("Allow"), http.MethodPut)
	}
	for _, unknownPath := range []string{
		"/api/v1/projects/" + projID + "/trigger-sources",
		"/api/v1/projects/" + projID + "/trigger-sources/gitlab",
	} {
		rr = doRequestAsUser(t, srv, owner, http.MethodGet, unknownPath, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code, unknownPath)
		assert.Contains(t, rr.Body.String(), "Trigger source")
	}

	// 4. Role matrix on GET and PUT with experiment enabled.
	for _, u := range []*store.User{owner, admin} {
		rr = doRequestAsUser(t, srv, u, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rr.Code, "GET for %s: %s", u.DisplayName, rr.Body.String())

		rr = doRequestAsUser(t, srv, u, http.MethodPut, path, validPut)
		assert.Equal(t, http.StatusOK, rr.Code, "PUT for %s: %s", u.DisplayName, rr.Body.String())
	}

	for _, u := range []*store.User{member, viewer} {
		rr = doRequestAsUser(t, srv, u, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rr.Code, "GET for %s: %s", u.DisplayName, rr.Body.String())

		rr = doRequestAsUser(t, srv, u, http.MethodPut, path, validPut)
		assert.Equal(t, http.StatusForbidden, rr.Code, "PUT for %s must be 403: %s", u.DisplayName, rr.Body.String())
	}

	rr = doRequestAsUser(t, srv, outsider, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	rr = doRequestAsUser(t, srv, outsider, http.MethodPut, path, validPut)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	// 5. Agent credentials are refused on both GET and PUT.
	agentTok, err := srv.agentTokenService.GenerateAgentToken(tid("ght-agent"), projID, []AgentTokenScope{ScopeProjectRead}, []string{owner.ID})
	require.NoError(t, err)
	rr = doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, agentTok)
	assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	rr = doRequestWithAgentToken(t, srv, http.MethodPut, path, validPut, agentTok)
	assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())

	// 6. Non-existent project returns 404 to a caller with system-wide read/configure.
	missingPath := "/api/v1/projects/" + tid("missing-proj") + "/trigger-sources/github"
	rr = doRequestAsUser(t, srv, super, http.MethodGet, missingPath, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = doRequestAsUser(t, srv, super, http.MethodPut, missingPath, validPut)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestProjectGitHubTriggerSettings_RoundTripValidationAndAudit(t *testing.T) {
	srv, s := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)
	ctx := context.Background()

	owner := createUserForGitHubTriggerTest(t, s, "ght-rt-owner", store.UserRoleMember)
	admin := createUserForGitHubTriggerTest(t, s, "ght-rt-admin", store.UserRoleMember)
	projID := tid("ght-rt-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:      projID,
		Name:    "rt-proj",
		Slug:    "rt-proj",
		Created: time.Now(),
		Updated: time.Now(),
	}))
	bindProjectRoleForGitHubTriggerTest(t, s, projID, owner.ID, store.ProjectRoleOwner)
	bindProjectRoleForGitHubTriggerTest(t, s, projID, admin.ID, store.ProjectRoleAdmin)

	path := "/api/v1/projects/" + projID + "/trigger-sources/github"

	// Initial defaults.
	rr := doRequestAsUser(t, srv, owner, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp ProjectGitHubTriggerSettingsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.False(t, resp.UnmatchedCommandReply)
	assert.Equal(t, "", resp.UnmatchedCommandText)
	assert.Empty(t, resp.Warnings)

	// PUT enables unmatched_command_reply + custom text, stamps updated-by, and audits.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, path, ProjectGitHubTriggerSettingsUpdateRequest{
		UnmatchedCommandReply: true,
		UnmatchedCommandText:  "Supported commands: /triage, /fix",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.True(t, resp.UnmatchedCommandReply)
	assert.Equal(t, "Supported commands: /triage, /fix", resp.UnmatchedCommandText)

	stored, err := s.GetProject(ctx, projID)
	require.NoError(t, err)
	assert.Equal(t, "true", stored.Annotations[AnnotationGitHubTriggersUnmatchedReply])
	assert.Equal(t, "Supported commands: /triage, /fix", stored.Annotations[AnnotationGitHubTriggersUnmatchedText])
	assert.Equal(t, owner.ID, stored.Annotations[AnnotationGitHubTriggersUpdatedBy])

	audits, total, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: githubTriggerSettingsAuditMutation,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, audits, 1)
	assert.Equal(t, projID, audits[0].TargetID)
	assert.Equal(t, owner.ID, audits[0].ActorPrincipalID)
	assert.Equal(t, "unmatched_command_reply=false,unmatched_command_text_bytes=0", audits[0].BeforeSummary)
	assert.Equal(t, "unmatched_command_reply=true,unmatched_command_text_bytes=33", audits[0].AfterSummary)

	// Second PUT by admin clears unmatched_command_text, updates updated-by, and writes a second audit record.
	rr = doRequestAsUser(t, srv, admin, http.MethodPut, path, ProjectGitHubTriggerSettingsUpdateRequest{
		UnmatchedCommandReply: false,
		UnmatchedCommandText:  "",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	stored, err = s.GetProject(ctx, projID)
	require.NoError(t, err)
	assert.Equal(t, "false", stored.Annotations[AnnotationGitHubTriggersUnmatchedReply])
	_, hasTextKey := stored.Annotations[AnnotationGitHubTriggersUnmatchedText]
	assert.False(t, hasTextKey, "empty unmatched_command_text must delete the annotation key")
	assert.Equal(t, admin.ID, stored.Annotations[AnnotationGitHubTriggersUpdatedBy])

	audits, total, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: githubTriggerSettingsAuditMutation,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, audits, 2)

	// Validation errors -> 400.
	rr = doRequestAsUser(t, srv, owner, http.MethodPut, path, ProjectGitHubTriggerSettingsUpdateRequest{
		UnmatchedCommandReply: true,
		UnmatchedCommandText:  strings.Repeat("x", MaxGitHubTriggersUnmatchedTextBytes+1),
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())

	rr = doRawTriggerSettingsAsUser(t, srv, owner, http.MethodPut, path, []byte(`{"unmatched_command_reply":true,"unmatched_command_text":"bad\u0000nul"}`))
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())

	rr = doRawTriggerSettingsAsUser(t, srv, owner, http.MethodPut, path, []byte(`{"unmatched_command_reply":"not-a-bool"}`))
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestProjectGitHubTriggerSettings_ProtectedAnnotationPatchGuardAndClone(t *testing.T) {
	srv, s := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)
	ctx := context.Background()

	super := createSystemUserForGitHubTriggerTest(t, s, "ght-patch-super", store.SystemRoleSuperAdmin)

	projID := tid("ght-patch-proj")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:      projID,
		Name:    "patch-proj",
		Slug:    "patch-proj",
		Created: time.Now(),
		Updated: time.Now(),
	}))
	bindProjectRoleForGitHubTriggerTest(t, s, projID, super.ID, store.ProjectRoleOwner)

	projectPath := "/api/v1/projects/" + projID
	settingsPath := projectPath + "/trigger-sources/github"

	// 1. Adding a scion.dev/github-triggers.* annotation via generic PATCH is refused 409 protected_annotation.
	rr := doRawTriggerSettingsAsUser(t, srv, super, http.MethodPatch, projectPath,
		[]byte(`{"name":"should-not-apply","annotations":{"scion.dev/github-triggers.unmatched-command-reply":"true"}}`))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeProtectedAnnotation)

	stored, err := s.GetProject(ctx, projID)
	require.NoError(t, err)
	assert.Equal(t, "patch-proj", stored.Name, "refused PATCH must not partially mutate project")
	assert.Empty(t, stored.Annotations)

	// 2. Configure trigger settings via dedicated PUT endpoint.
	rr = doRequestAsUser(t, srv, super, http.MethodPut, settingsPath, ProjectGitHubTriggerSettingsUpdateRequest{
		UnmatchedCommandReply: true,
		UnmatchedCommandText:  "Hello triggers",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 3. Modifying or removing scion.dev/github-triggers.* via generic PATCH ({} / null / changed value) -> 409 protected_annotation.
	for _, badPatch := range []string{
		`{"annotations":{"scion.dev/github-triggers.unmatched-command-reply":"false"}}`,
		`{"annotations":{}}`,
		`{"annotations":null}`,
		`{"annotations":{"unrelated":"value"}}`,
	} {
		rr = doRawTriggerSettingsAsUser(t, srv, super, http.MethodPatch, projectPath, []byte(badPatch))
		require.Equal(t, http.StatusConflict, rr.Code, "payload %s: %s", badPatch, rr.Body.String())
		assert.Contains(t, rr.Body.String(), ErrCodeProtectedAnnotation)
	}

	// 4. Generic PATCH omitting annotations succeeds and preserves scion.dev/github-triggers.* annotations.
	rr = doRawTriggerSettingsAsUser(t, srv, super, http.MethodPatch, projectPath, []byte(`{"name":"patch-proj-renamed"}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	stored, err = s.GetProject(ctx, projID)
	require.NoError(t, err)
	assert.Equal(t, "patch-proj-renamed", stored.Name)
	assert.Equal(t, "true", stored.Annotations[AnnotationGitHubTriggersUnmatchedReply])
	assert.Equal(t, "Hello triggers", stored.Annotations[AnnotationGitHubTriggersUnmatchedText])
	assert.Equal(t, super.ID, stored.Annotations[AnnotationGitHubTriggersUpdatedBy])

	// 5. Generic PATCH resending the exact stored scion.dev/github-triggers.* annotations succeeds.
	sameAnnotationsBody, err := json.Marshal(map[string]any{
		"name":        "patch-proj-same-ann",
		"annotations": stored.Annotations,
	})
	require.NoError(t, err)
	rr = doRawTriggerSettingsAsUser(t, srv, super, http.MethodPatch, projectPath, sameAnnotationsBody)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 6. Cloning the project does NOT propagate scion.dev/github-triggers.* annotations.
	rr = doRequestAsUser(t, srv, super, http.MethodPost, projectPath+"/clone", CloneProjectRequest{
		Name: "Cloned Trigger Project",
	})
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var cloned store.Project
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &cloned))
	storedClone, err := s.GetProject(ctx, cloned.ID)
	require.NoError(t, err)
	for k := range storedClone.Annotations {
		assert.False(t, strings.HasPrefix(k, AnnotationGitHubTriggersPrefix),
			"cloned project must not inherit %s annotation", k)
	}
}

func TestProjectGitHubTriggerSettings_ReadinessAndRepoRoutingWarning(t *testing.T) {
	srv, s := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)
	ctx := context.Background()

	// Serve GET /app via a local mock server to verify project-scoped readiness (§10.2).
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(rsaKey),
	}))
	var repoCheckCalls atomic.Int32
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			repoCheckCalls.Add(1)
			http.Error(w, "unexpected association repo check", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/app" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 99,
			"slug": "scion-triggers-bot",
			"events": ["issues", "pull_request", "pull_request_review", "pull_request_review_comment"],
			"permissions": {"contents": "write", "metadata": "read", "pull_requests": "write"}
		}`))
	}))
	t.Cleanup(mockGH.Close)

	srv.mu.Lock()
	srv.config.GitHubAppConfig = GitHubAppServerConfig{
		AppID:           99,
		PrivateKey:      keyPEM,
		APIBaseURL:      mockGH.URL,
		WebhooksEnabled: true,
		WebhookSecret:   "whsec-test-secret",
	}
	srv.mu.Unlock()

	super := createSystemUserForGitHubTriggerTest(t, s, "ght-route-super", store.SystemRoleSuperAdmin)
	memberA := createUserForGitHubTriggerTest(t, s, "ght-route-member-a", store.UserRoleMember)
	memberB := createUserForGitHubTriggerTest(t, s, "ght-route-member-b", store.UserRoleMember)
	otherCreator := createUserForGitHubTriggerTest(t, s, "ght-route-other-creator", store.UserRoleMember)

	instID := int64(7001)
	otherInstID := int64(7002)
	// Intentionally omit "Acme/Widgets" from cached Repositories and mark projB
	// degraded/repo_not_accessible to verify candidacy follows G5-core's
	// IsGitHubTriggerCandidate (§4.2 steps 1-3: installation ID + host-pinned remote).
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "Acme",
		AccountType:    "Organization",
		AppID:          99,
		Repositories:   []string{"Acme/OtherRepo"},
		Status:         store.GitHubInstallationStatusActive,
	}))
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: otherInstID,
		AccountLogin:   "OtherOrg",
		AccountType:    "Organization",
		AppID:          99,
		Repositories:   []string{"Acme/Widgets", "OtherOrg/Unrelated"},
		Status:         store.GitHubInstallationStatusActive,
	}))

	webHost := srv.githubAppWebHost()
	projA := tid("ght-route-proj-a")
	projB := tid("ght-route-proj-b")
	projC := tid("ght-route-proj-c")
	projOtherInst := tid("ght-route-proj-other-inst")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projA,
		Name:                 "alpha",
		Slug:                 "alpha",
		GitRemote:            "https://" + webHost + "/Acme/Widgets.git",
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projB,
		Name:                 "bravo",
		Slug:                 "bravo",
		GitRemote:            "https://" + webHost + "/acme/widgets.git",
		GitHubInstallationID: &instID,
		GitHubAppStatus: &store.GitHubAppProjectStatus{
			State:     store.GitHubAppStateDegraded,
			ErrorCode: "repo_not_accessible",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projC,
		Name:                 "charlie",
		Slug:                 "charlie",
		GitRemote:            "https://" + webHost + "/acme/widgets",
		GitHubInstallationID: &instID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}))
	// Project bound to a DIFFERENT installation (even with the same repo path and
	// a creator GITHUB_TOKEN that buildGitHubAssociationReport would probe): must
	// neither appear in OtherProjects nor be probed on a project-reader GET.
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:                   projOtherInst,
		Name:                 "delta-other-inst",
		Slug:                 "delta-other-inst",
		CreatedBy:            otherCreator.ID,
		OwnerID:              otherCreator.ID,
		GitRemote:            "https://" + webHost + "/Acme/Widgets.git",
		GitHubInstallationID: &otherInstID,
		Created:              time.Now(),
		Updated:              time.Now(),
	}))
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:             tid("ght-other-creator-pat"),
		Key:            "GITHUB_TOKEN",
		EncryptedValue: "ghp_other_creator_token",
		SecretType:     store.SecretTypeEnvironment,
		Scope:          store.ScopeUser,
		ScopeID:        otherCreator.ID,
		Version:        1,
		Created:        time.Now(),
		Updated:        time.Now(),
	}))
	bindProjectRoleForGitHubTriggerTest(t, s, projA, memberA.ID, store.ProjectRoleMember)
	bindProjectRoleForGitHubTriggerTest(t, s, projB, memberB.ID, store.ProjectRoleMember)

	pathA := "/api/v1/projects/" + projA + "/trigger-sources/github"
	pathB := "/api/v1/projects/" + projB + "/trigger-sources/github"

	// 1. Multiple candidate projects with no primary set -> multi_project_repo warning + sorted sibling project names.
	// Also verify a project-reader GET does NOT invoke buildGitHubAssociationReport.
	rr := doRequestAsUser(t, srv, memberA, http.MethodGet, pathA, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	srv.githubAssociationReportCache.mu.Lock()
	assocValid := srv.githubAssociationReportCache.valid
	assocLastFullBuild := srv.githubAssociationReportCache.lastFullBuild
	assocEntriesLen := len(srv.githubAssociationReportCache.entries)
	srv.githubAssociationReportCache.mu.Unlock()
	assert.False(t, assocValid, "project-reader GET must not build the hub-wide association report")
	assert.True(t, assocLastFullBuild.IsZero(), "project-reader GET must not set lastFullBuild on association report cache")
	assert.Zero(t, assocEntriesLen, "project-reader GET must not populate association report entries")
	assert.Zero(t, repoCheckCalls.Load(), "project-reader GET must not trigger outbound creator repo-access probes")

	// Ensure hub-admin-only webhook configuration booleans are never exposed on the project view (§10.2).
	var rawMap map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &rawMap))
	readinessMap, ok := rawMap["readiness"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, readinessMap, "webhooks_enabled")
	assert.NotContains(t, readinessMap, "has_webhook_secret")

	var respA ProjectGitHubTriggerSettingsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &respA))
	assert.False(t, respA.Readiness.Ready)
	assert.Equal(t, []string{"issue_comment"}, respA.Readiness.MissingEvents)
	assert.Equal(t, []string{"issues:write"}, respA.Readiness.MissingPermissions)
	assert.Equal(t, "scion-triggers-bot", respA.Readiness.AppSlug)
	assert.Nil(t, respA.Routing.PrimaryProject)
	assert.False(t, respA.Routing.IsPrimary)
	assert.Equal(t, []string{"bravo", "charlie"}, respA.Routing.OtherProjects)
	assert.Equal(t, []string{WarningGitHubTriggersMultiProjectRepo}, respA.Warnings)

	// 2. Configure projA as primary via the G5-core HTTP route (never writing store fields directly).
	rr = doRequestAsUser(t, srv, super, http.MethodPut, "/api/v1/github-app/repo-routing/Acme/Widgets", GitHubRepoRoutingPutRequest{
		InstallationID:   instID,
		PrimaryProjectID: projA,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// 3. projA now sees is_primary=true, primary_project={projA, "alpha"}, and no multi_project_repo warning.
	rr = doRequestAsUser(t, srv, memberA, http.MethodGet, pathA, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	respA = ProjectGitHubTriggerSettingsResponse{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &respA))
	require.NotNil(t, respA.Routing.PrimaryProject)
	assert.Equal(t, projA, respA.Routing.PrimaryProject.ID)
	assert.Equal(t, "alpha", respA.Routing.PrimaryProject.Name)
	assert.True(t, respA.Routing.IsPrimary)
	assert.Equal(t, []string{"bravo", "charlie"}, respA.Routing.OtherProjects)
	assert.Empty(t, respA.Warnings)

	// 4. projB sees is_primary=false, primary_project={projA, "alpha"}, other_projects=["alpha", "charlie"], and no warning.
	rr = doRequestAsUser(t, srv, memberB, http.MethodGet, pathB, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var respB ProjectGitHubTriggerSettingsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &respB))
	require.NotNil(t, respB.Routing.PrimaryProject)
	assert.Equal(t, projA, respB.Routing.PrimaryProject.ID)
	assert.Equal(t, "alpha", respB.Routing.PrimaryProject.Name)
	assert.False(t, respB.Routing.IsPrimary)
	assert.Equal(t, []string{"alpha", "charlie"}, respB.Routing.OtherProjects)
	assert.Empty(t, respB.Warnings)

	// 5. Stale primary (projA detached from installation) fails open: projB sees
	// primary_project=nil, other_projects=["charlie"], and multi_project_repo warning again.
	storedA, err := s.GetProject(ctx, projA)
	require.NoError(t, err)
	storedA.GitHubInstallationID = nil
	require.NoError(t, s.UpdateProject(ctx, storedA))
	srv.invalidateGitHubAssociation(projA)

	rr = doRequestAsUser(t, srv, memberB, http.MethodGet, pathB, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	respB = ProjectGitHubTriggerSettingsResponse{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &respB))
	assert.Nil(t, respB.Routing.PrimaryProject)
	assert.False(t, respB.Routing.IsPrimary)
	assert.Equal(t, []string{"charlie"}, respB.Routing.OtherProjects)
	assert.Equal(t, []string{WarningGitHubTriggersMultiProjectRepo}, respB.Warnings)
}

// TestProjectGitHubTriggerSettings_ScopedTokens pins bearer-token boundary and
// selector enforcement for GET and PUT /api/v1/projects/{id}/trigger-sources/github.
func TestProjectGitHubTriggerSettings_ScopedTokens(t *testing.T) {
	srv, s := testServer(t)
	setGitHubTriggersExperimentForRouting(t, srv, true)

	ownerID := tid("ght-tok-owner")
	projID := tid("ght-tok-proj")
	otherProjID := tid("ght-tok-other")
	createRS1Project(t, s, projID, ownerID)
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{
		ID:      otherProjID,
		Name:    "ght-tok-other",
		Slug:    "ght-tok-other",
		Created: time.Now(),
		Updated: time.Now(),
	}))
	bindProjectRoleForGitHubTriggerTest(t, s, otherProjID, ownerID, store.ProjectRoleOwner)

	mintHub := func(scopes ...string) string {
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
			UserID: ownerID, Name: "ght-hub-" + tid("tok"), Boundary: hubBoundary(), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}
	mintProj := func(targetProjID string, scopes ...string) string {
		key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
			UserID: ownerID, Name: "ght-proj-" + tid("tok"), Boundary: projectBoundary(targetProjID), Scopes: scopes,
		})
		require.NoError(t, err)
		return key
	}

	projReadTok := mintProj(projID, "project:read")
	projConfigureTok := mintProj(projID, "github_trigger:configure")
	otherProjBothTok := mintProj(otherProjID, "project:read", "github_trigger:configure")
	hubReadTok := mintHub("project:read")
	hubConfigureTok := mintHub("github_trigger:configure")
	hubUnrelatedTok := mintHub("template:read")

	path := "/api/v1/projects/" + projID + "/trigger-sources/github"
	putBody := ProjectGitHubTriggerSettingsUpdateRequest{
		UnmatchedCommandReply: true,
		UnmatchedCommandText:  "Scoped token update",
	}

	// GET requires project:read on a covering boundary (projID or hub).
	rec := doRequestWithToken(t, srv, projReadTok, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, hubReadTok, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, projConfigureTok, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "configure selector without project:read must fail GET: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, otherProjBothTok, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "other-project boundary must fail GET: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, hubUnrelatedTok, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "unrelated hub token must fail GET: %s", rec.Body.String())

	// PUT requires github_trigger:configure on a covering boundary (projID or hub).
	rec = doRequestWithToken(t, srv, projReadTok, http.MethodPut, path, putBody)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project:read without github_trigger:configure must fail PUT: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, otherProjBothTok, http.MethodPut, path, putBody)
	assert.Equal(t, http.StatusForbidden, rec.Code, "other-project boundary must fail PUT: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, hubUnrelatedTok, http.MethodPut, path, putBody)
	assert.Equal(t, http.StatusForbidden, rec.Code, "unrelated hub token must fail PUT: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, projConfigureTok, http.MethodPut, path, putBody)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, hubConfigureTok, http.MethodPut, path, putBody)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
