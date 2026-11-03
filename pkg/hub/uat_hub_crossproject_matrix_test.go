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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file drive real user access tokens through the full
// server handler. Every state change under test (a mint, a membership
// removal, a demotion, a suspension) goes through its HTTP route over an
// interactive session; only the starting fixture is written to the store.

// xpMintOverSession mints a token for user through POST /api/v1/auth/tokens
// over an interactive session and returns the secret and the token ID.
func xpMintOverSession(t *testing.T, srv *Server, user *store.User, body map[string]interface{}) (key, tokenID string) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/auth/tokens", body)
	require.Equal(t, http.StatusCreated, rec.Code, "session mint: %s", rec.Body.String())
	raw, resp := decodeTokenCreate(t, rec.Body.Bytes())
	require.NotEmpty(t, raw.Token)
	return raw.Token, resp.ID
}

// xpMintHub mints a hub-boundary token carrying scopes over a session.
func xpMintHub(t *testing.T, srv *Server, user *store.User, name string, scopes ...string) string {
	t.Helper()
	key, _ := xpMintOverSession(t, srv, user, map[string]interface{}{
		"name": name, "boundary": map[string]string{"kind": "hub"}, "scopes": scopes,
	})
	return key
}

// xpRequireAgentKept asserts that the agent still exists.
func xpRequireAgentKept(t *testing.T, s store.Store, agentID, label string) {
	t.Helper()
	_, err := s.GetAgent(context.Background(), agentID)
	assert.NoError(t, err, "%s: a refused request leaves the agent in place", label)
}

// xpAgentIDsByProject lists GET /api/v1/agents with key and returns the
// listed agent IDs by project ID.
func xpAgentIDsByProject(t *testing.T, srv *Server, key string) map[string][]string {
	t.Helper()
	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Agents []struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	out := map[string][]string{}
	for _, a := range body.Agents {
		out[a.ProjectID] = append(out[a.ProjectID], a.ID)
	}
	return out
}

// xpSuperAdminBindingID returns the ID of userID's system super-admin
// binding.
func xpSuperAdminBindingID(t *testing.T, s store.Store, userID string) string {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID && b.ScopeType == store.RoleScopeSystem {
			return b.ID
		}
	}
	t.Fatalf("user %s holds no super-admin binding", userID)
	return ""
}

// xpProjectBindingID returns the ID of userID's direct binding in projectID.
func xpProjectBindingID(t *testing.T, s store.Store, userID, projectID string) string {
	t.Helper()
	bindings := mmrBindingsFor(t, s, store.RoleBindingPrincipalUser, userID, projectID)
	require.Len(t, bindings, 1, "user %s holds one binding in %s", userID, projectID)
	return bindings[0].ID
}

// xpDenyRecords returns the captured deny decisions for resourceID, or for
// every resource when resourceID is empty. It is the record form of
// deniedAuditReasons.
func xpDenyRecords(e *capturingAuditEmitter, resourceID string) []*store.DecisionAuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*store.DecisionAuditRecord
	for _, r := range e.records {
		if r.Result == "deny" && (resourceID == "" || r.ResourceID == resourceID) {
			out = append(out, r)
		}
	}
	return out
}

// xpDeniedReasons returns the reasons of every captured deny decision.
func xpDeniedReasons(e *capturingAuditEmitter) []string {
	var reasons []string
	for _, r := range xpDenyRecords(e, "") {
		reasons = append(reasons, r.Reason)
	}
	return reasons
}

// ---------------------------------------------------------------------------
// Cross-project rules
// ---------------------------------------------------------------------------

// TestHubUATCrossProject_MemberRemovedThroughAPIDeniedNextRequest pins that
// a hub token reaches a project only while its holder is a current member:
// removing the holder through the project members API denies the next
// request in that project, the holder's other project keeps working, and the
// removed project then answers a project mint and an eligibility request
// exactly as a project that does not exist.
func TestHubUATCrossProject_MemberRemovedThroughAPIDeniedNextRequest(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectP, ownerP := tid("xp-removed-project-p"), tid("xp-removed-owner-p")
	projectQ, ownerQ := tid("xp-removed-project-q"), tid("xp-removed-owner-q")
	userID := tid("xp-removed-user")
	createRS1Project(t, s, projectP, ownerP)
	createRS1Project(t, s, projectQ, ownerQ)
	uatpMember(t, s, projectP, userID)
	uatpMember(t, s, projectQ, userID)
	agentP := uatpAgent(t, s, projectP, ownerP, "xp-removed-p", ownerP)
	agentQ := uatpAgent(t, s, projectQ, ownerQ, "xp-removed-q", ownerQ)
	user := mustGetUser(t, s, userID)

	key := xpMintHub(t, srv, user, "xp-removed", "agent:read", "agent:list")

	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agentQ.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "a member reads an agent in Q: %s", rec.Body.String())
	listed := xpAgentIDsByProject(t, srv, key)
	require.Contains(t, listed[projectP], agentP.ID)
	require.Contains(t, listed[projectQ], agentQ.ID)

	bindingQ := xpProjectBindingID(t, s, userID, projectQ)
	rec = doRequestAsUser(t, srv, mustGetUser(t, s, ownerQ), http.MethodDelete, pinMemberPath(projectQ, bindingQ), nil)
	require.Less(t, rec.Code, 300, "the owner of Q removes the member: %d %s", rec.Code, rec.Body.String())
	require.Empty(t, mmrBindingsFor(t, s, store.RoleBindingPrincipalUser, userID, projectQ), "the member holds no binding in Q")

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agentQ.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "the next request in Q is refused: %s", rec.Body.String())
	assert.Contains(t, deniedAuditReasons(emitter, agentQ.ID), bearerReasonProjectAccessDenied,
		"the refusal comes from the project access stage")

	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agentP.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the holder's other project keeps working: %s", rec.Body.String())
	listed = xpAgentIDsByProject(t, srv, key)
	assert.Contains(t, listed[projectP], agentP.ID)
	assert.Empty(t, listed[projectQ], "the list holds nothing from Q")

	// A former member's project mint and eligibility request for Q are
	// answered exactly as for a project that does not exist.
	nonexistent := tid("xp-removed-nonexistent-project")
	mintQ := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name": "xp-removed-q", "projectId": projectQ, "scopes": []string{"agent:read"},
	})
	mintMissing := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name": "xp-removed-q", "projectId": nonexistent, "scopes": []string{"agent:read"},
	})
	assert.Equal(t, http.StatusForbidden, mintQ.Code, mintQ.Body.String())
	assert.Equal(t, mintQ.Code, mintMissing.Code)
	assert.JSONEq(t, `{"error":{"code":"forbidden","message":"forbidden"}}`, mintQ.Body.String())
	assert.JSONEq(t, mintQ.Body.String(), mintMissing.Body.String())

	scopesQ := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/auth/scopes?projectId="+projectQ, nil)
	scopesMissing := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/auth/scopes?projectId="+nonexistent, nil)
	assert.Equal(t, http.StatusForbidden, scopesQ.Code, scopesQ.Body.String())
	assert.Equal(t, scopesQ.Code, scopesMissing.Code)
	assert.JSONEq(t, scopesQ.Body.String(), scopesMissing.Body.String())

	count, err := s.CountUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "a refused mint stores no token")
}

// TestHubUATCrossProject_SelectedPermissionAndLiveAuthorityBothRequired
// pins that a cross-project action needs both the permission selected in
// the token and the holder's live access to the project: with either one
// missing the request is refused by the stage that is missing, and the
// target is kept. The holder owns the agents in Q, so only project access
// can refuse there.
func TestHubUATCrossProject_SelectedPermissionAndLiveAuthorityBothRequired(t *testing.T) {
	srv, s := testServer(t)
	projectP, userID := tid("xp-2x2-project-p"), tid("xp-2x2-user")
	projectQ, ownerQ := tid("xp-2x2-project-q"), tid("xp-2x2-owner-q")
	createRS1Project(t, s, projectP, userID)
	createRS1Project(t, s, projectQ, ownerQ)
	user := mustGetUser(t, s, userID)
	agentP1 := uatpAgent(t, s, projectP, userID, "xp-2x2-p1", userID)
	agentP2 := uatpAgent(t, s, projectP, userID, "xp-2x2-p2", userID)
	agentQ1 := uatpAgent(t, s, projectQ, userID, "xp-2x2-q1", userID)
	agentQ2 := uatpAgent(t, s, projectQ, userID, "xp-2x2-q2", userID)

	withSelector := xpMintHub(t, srv, user, "xp-2x2-sel", "agent:delete")
	withoutSelector := xpMintHub(t, srv, user, "xp-2x2-nosel", "agent:read")
	ceilingReason := "token does not have scope: agent:delete"

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	t.Run("selector and access", func(t *testing.T) {
		rec := doRequestWithUAT(t, srv, withSelector, http.MethodDelete, "/api/v1/agents/"+agentP1.ID, nil)
		require.Less(t, rec.Code, 300, "%d %s", rec.Code, rec.Body.String())
		_, err := s.GetAgent(context.Background(), agentP1.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "the agent is gone")
	})
	t.Run("access without selector", func(t *testing.T) {
		rec := doRequestWithUAT(t, srv, withoutSelector, http.MethodDelete, "/api/v1/agents/"+agentP2.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, deniedAuditReasons(emitter, agentP2.ID), ceilingReason, "the ceiling refuses")
		xpRequireAgentKept(t, s, agentP2.ID, "access without selector")
	})
	t.Run("selector without access", func(t *testing.T) {
		rec := doRequestWithUAT(t, srv, withSelector, http.MethodDelete, "/api/v1/agents/"+agentQ1.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, deniedAuditReasons(emitter, agentQ1.ID), bearerReasonProjectAccessDenied, "project access refuses")
		xpRequireAgentKept(t, s, agentQ1.ID, "selector without access")
	})
	t.Run("neither", func(t *testing.T) {
		// The ceiling stage runs before the project access stage, so the
		// ceiling refuses first.
		rec := doRequestWithUAT(t, srv, withoutSelector, http.MethodDelete, "/api/v1/agents/"+agentQ2.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		reasons := deniedAuditReasons(emitter, agentQ2.ID)
		assert.Contains(t, reasons, ceilingReason, "the ceiling refuses")
		assert.NotContains(t, reasons, bearerReasonProjectAccessDenied, "the ceiling refuses before project access is checked")
		xpRequireAgentKept(t, s, agentQ2.ID, "neither")
	})
}

// TestHubUATCrossProject_SuperAdminDeletionThenDemotionThroughAPI pins that
// a super-admin's hub token acts in projects its holder is not a member of
// only while the holder's system binding holds: removing the binding
// through the role-binding API refuses the next request in every project,
// even for agents the holder owns.
func TestHubUATCrossProject_SuperAdminDeletionThenDemotionThroughAPI(t *testing.T) {
	srv, s := testServer(t)
	projectP, ownerP := tid("xp-demote-project-p"), tid("xp-demote-owner-p")
	projectQ, ownerQ := tid("xp-demote-project-q"), tid("xp-demote-owner-q")
	createRS1Project(t, s, projectP, ownerP)
	createRS1Project(t, s, projectQ, ownerQ)
	adminA := hubConfigTokenUser(t, s, "xp-demote-admin-a", store.SystemRoleSuperAdmin)
	adminB := hubConfigTokenUser(t, s, "xp-demote-admin-b", store.SystemRoleSuperAdmin)
	p1 := uatpAgent(t, s, projectP, ownerP, "xp-demote-p1", ownerP)
	q1 := uatpAgent(t, s, projectQ, ownerQ, "xp-demote-q1", ownerQ)
	p2 := uatpAgent(t, s, projectP, adminA, "xp-demote-p2", adminA)
	q2 := uatpAgent(t, s, projectQ, adminA, "xp-demote-q2", adminA)

	key := xpMintHub(t, srv, mustGetUser(t, s, adminA), "xp-demote", "agent:delete")

	for _, a := range []*store.Agent{p1, q1} {
		rec := doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
		require.Less(t, rec.Code, 300, "delete %s in a non-member project: %d %s", a.ID, rec.Code, rec.Body.String())
		_, err := s.GetAgent(context.Background(), a.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}

	bindingID := xpSuperAdminBindingID(t, s, adminA)
	rec := doRequestAsUser(t, srv, mustGetUser(t, s, adminB), http.MethodDelete, "/api/v1/admin/role-bindings/"+bindingID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "a second super-admin removes the binding: %s", rec.Body.String())
	audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: "role_binding_delete", TargetID: bindingID, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, audits, 1, "the removal is audited")

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	for _, a := range []*store.Agent{p2, q2} {
		rec = doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "after demotion: %s", rec.Body.String())
		assert.Contains(t, deniedAuditReasons(emitter, a.ID), bearerReasonProjectAccessDenied)
		xpRequireAgentKept(t, s, a.ID, "after demotion")
	}
}

// TestHubUATCrossProject_ListAndMessageSpanOnlyAccessibleProjects pins that
// a hub token lists and messages agents only in projects its holder can
// access: an agent in an inaccessible project is left out of the list and a
// message to it is refused and stored nowhere, even where the holder owns
// the agent.
func TestHubUATCrossProject_ListAndMessageSpanOnlyAccessibleProjects(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectP, userID := tid("xp-msg-project-p"), tid("xp-msg-user")
	projectQ, ownerQ := tid("xp-msg-project-q"), tid("xp-msg-owner-q")
	projectR, ownerR := tid("xp-msg-project-r"), tid("xp-msg-owner-r")
	createRS1Project(t, s, projectP, userID)
	createRS1Project(t, s, projectQ, ownerQ)
	createRS1Project(t, s, projectR, ownerR)
	uatpMember(t, s, projectQ, userID)
	agentP := uatpAgent(t, s, projectP, userID, "xp-msg-p", userID)
	agentQ := uatpAgent(t, s, projectQ, userID, "xp-msg-q", userID)
	ownedR := uatpAgent(t, s, projectR, userID, "xp-msg-r-owned", userID)
	otherR := uatpAgent(t, s, projectR, ownerR, "xp-msg-r-other", ownerR)

	key := xpMintHub(t, srv, mustGetUser(t, s, userID), "xp-msg", "agent:list", "agent:read", "agent:message")

	listed := xpAgentIDsByProject(t, srv, key)
	assert.Contains(t, listed[projectP], agentP.ID)
	assert.Contains(t, listed[projectQ], agentQ.ID)
	assert.Empty(t, listed[projectR], "the list holds nothing from the inaccessible project")

	// The agent in P is stopped: an admitted message reaches the delivery
	// checks and is answered agent_not_running, after authorization.
	body := map[string]interface{}{"message": "xp-msg hello"}
	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/agents/"+agentP.ID+"/message", body)
	assert.Equal(t, http.StatusConflict, rec.Code, "a message in an accessible project passes authorization: %s", rec.Body.String())
	assert.Equal(t, ErrCodeAgentNotRunning, mtErrCode(rec), rec.Body.String())

	// The 403s below, and the project access reason for the holder's own
	// agent, show each send is refused. One message per agent in R is in the
	// store first, so the per-agent count, found by the filter, guards
	// against a write before the refusal.
	storedFor := func(a *store.Agent) int {
		t.Helper()
		stored, err := s.ListMessages(ctx, store.MessageFilter{AgentID: a.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		return len(stored.Items)
	}
	for _, a := range []*store.Agent{ownedR, otherR} {
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: tid("xp-msg-seed-" + a.Slug), ProjectID: projectR, AgentID: a.ID, RecipientID: a.ID,
			Sender: "user:" + ownerR, SenderID: ownerR, Recipient: "agent:" + a.Slug, Msg: "xp-msg seed",
			Type: messages.TypeInstruction, DispatchState: store.MessageDispatchDispatched, CreatedAt: time.Now().UTC(),
		}))
		require.Equal(t, 1, storedFor(a), "the seeded message for %s is found", a.Slug)
	}

	for _, a := range []*store.Agent{ownedR, otherR} {
		rec = doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/agents/"+a.ID+"/message", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "message to %s: %s", a.Slug, rec.Body.String())
		assert.Equal(t, ErrCodeMessageDenied, mtErrCode(rec), rec.Body.String())
		assert.Equal(t, 1, storedFor(a), "no message is added for %s before the refusal", a.Slug)
	}
	// The message route authorizes a token through the message rules; the
	// same token, resolved from its key, is refused for the holder's own
	// agent in R by the project access stage.
	identity, err := srv.uatService.ValidateToken(ctx, key)
	require.NoError(t, err)
	allowed, reason, _ := srv.authorizeAgentMessage(ctx, identity, ownedR, false)
	assert.False(t, allowed, "the holder's own agent in R")
	assert.Contains(t, reason, bearerReasonProjectAccessDenied, "the holder's own agent in R is refused by the project access stage")
}

// TestHubUATCrossProject_HolderSuspendedThroughAPIDeniedNextRequest pins
// that a hub token follows its holder's own account status: suspending the
// holder through the users API refuses the next request in every project
// with user_suspended and changes nothing, and reinstating the holder lets
// the same token work again.
func TestHubUATCrossProject_HolderSuspendedThroughAPIDeniedNextRequest(t *testing.T) {
	srv, s := testServer(t)
	projectP, userID := tid("xp-suspend-project-p"), tid("xp-suspend-user")
	projectQ, ownerQ := tid("xp-suspend-project-q"), tid("xp-suspend-owner-q")
	createRS1Project(t, s, projectP, userID)
	createRS1Project(t, s, projectQ, ownerQ)
	uatpMember(t, s, projectQ, userID)
	admin := mustGetUser(t, s, hubConfigTokenUser(t, s, "xp-suspend-admin", store.SystemRoleSuperAdmin))
	agentP := uatpAgent(t, s, projectP, userID, "xp-suspend-p", userID)
	agentQ := uatpAgent(t, s, projectQ, userID, "xp-suspend-q", userID)

	key := xpMintHub(t, srv, mustGetUser(t, s, userID), "xp-suspend", "agent:read", "agent:delete")
	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agentQ.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/users/"+userID, map[string]string{"status": store.UserStatusSuspended})
	require.Less(t, rec.Code, 300, "suspend through the users API: %d %s", rec.Code, rec.Body.String())
	require.Equal(t, store.UserStatusSuspended, mustGetUser(t, s, userID).Status)

	for _, a := range []*store.Agent{agentP, agentQ} {
		rec = doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Equal(t, "user_suspended", mtErrCode(rec), rec.Body.String())
		xpRequireAgentKept(t, s, a.ID, "suspended holder")
	}

	rec = doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/users/"+userID, map[string]string{"status": store.UserStatusActive})
	require.Less(t, rec.Code, 300, "reinstate through the users API: %d %s", rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/agents/"+agentQ.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the holder's status is read again on each request: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Hub control plane
// ---------------------------------------------------------------------------

// TestHubUATControlPlane_SuperAdminConfigReadEndsWhenBindingRemovedThroughAPI
// pins that a hub configuration token answers to its holder's live system
// authority: a super-admin's token reads the server configuration, and
// once another super-admin removes the holder's binding through the
// role-binding API the next read is refused.
func TestHubUATControlPlane_SuperAdminConfigReadEndsWhenBindingRemovedThroughAPI(t *testing.T) {
	srv, s := testServer(t)
	adminA := hubConfigTokenUser(t, s, "xp-config-admin-a", store.SystemRoleSuperAdmin)
	adminB := hubConfigTokenUser(t, s, "xp-config-admin-b", store.SystemRoleSuperAdmin)

	key := xpMintHub(t, srv, mustGetUser(t, s, adminA), "xp-config", "hub_config:read")
	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/admin/server-config", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	bindingID := xpSuperAdminBindingID(t, s, adminA)
	rec = doRequestAsUser(t, srv, mustGetUser(t, s, adminB), http.MethodDelete, "/api/v1/admin/role-bindings/"+bindingID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	rec = doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/admin/server-config", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "the next read is refused: %s", rec.Body.String())
	assert.Equal(t, ErrCodeForbidden, mtErrCode(rec), rec.Body.String())
	// The token passes its own stages (boundary, ceiling); the holder's live
	// authority refuses: the remaining hub-member binding does not carry
	// hub.config.read.
	reasons := xpDeniedReasons(emitter)
	require.NotEmpty(t, reasons, "the refusal is a recorded decision")
	assert.Contains(t, reasons, `active bindings do not include permission "hub.config.read"`, "the holder's live authority refuses")
	for _, reason := range reasons {
		// Every token-stage reason starts with "token " or is the boundary
		// eligibility reason.
		assert.False(t, strings.HasPrefix(reason, "token "), "the refusal is not a token stage: %s", reason)
		assert.NotEqual(t, bearerReasonBoundaryIneligible, reason, "the refusal is not a token stage")
	}
}

// TestHubUATControlPlane_ProjectTokenNeverReachesHubRoutes pins that a
// project token never reaches a hub-level route, whatever system role its
// holder has: a super-admin's project token is refused on hub routes by the
// token boundary, and still works inside its project.
func TestHubUATControlPlane_ProjectTokenNeverReachesHubRoutes(t *testing.T) {
	srv, s := testServer(t)
	adminID := hubConfigTokenUser(t, s, "xp-projtok-admin", store.SystemRoleSuperAdmin)
	projectP := tid("xp-projtok-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: projectP, Name: "XP Project Token", Slug: "xp-projtok-project"}))
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", projectP, store.ProjectRoleOwner)

	key, _ := xpMintOverSession(t, srv, mustGetUser(t, s, adminID), map[string]interface{}{
		"name": "xp-projtok", "projectId": projectP, "scopes": []string{"project:read", "agent:read"},
	})

	for _, path := range []string{"/api/v1/users", "/api/v1/admin/server-config", "/api/v1/admin/role-bindings"} {
		emitter := &capturingAuditEmitter{}
		srv.authzService.SetDecisionAuditEmitter(emitter)
		rec := doRequestWithUAT(t, srv, key, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
		assert.Contains(t, xpDeniedReasons(emitter), bearerReasonHubLevelResource,
			"%s: the token boundary refuses before the token's selectors are considered", path)
	}

	rec := doRequestWithUAT(t, srv, key, http.MethodGet, "/api/v1/projects/"+projectP, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the token works inside its project: %s", rec.Body.String())
}

// TestHubUATControlPlane_GovernanceSessionOnlyOperationsRefuseHubToken pins
// that governance operations that are session-only refuse every user access
// token, including a super-admin's hub token, with the GOV_PENDING reason,
// before any lookup and without effect. Each refusal flips when the
// governance gate for that operation lands.
func TestHubUATControlPlane_GovernanceSessionOnlyOperationsRefuseHubToken(t *testing.T) {
	f := newMSFixture(t, "xp-gov")
	srv, s := f.srv, f.s
	ctx := context.Background()
	admin := mustGetUser(t, s, hubConfigTokenUser(t, s, "xp-gov-admin", store.SystemRoleSuperAdmin))

	broker := &store.RuntimeBroker{
		ID: tid("xp-gov-broker"), Name: "xp-gov-broker", Slug: "xp-gov-broker",
		Status: store.BrokerStatusOnline, CreatedBy: admin.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	pType := "user"
	constraint, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "xp-gov-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(f.userID),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "xp-gov boundary",
		CreatedBy:            admin.ID,
	})
	require.NoError(t, err)
	// The holder of the hold is still a member, so a session may lift it.
	f.hold(f.agentA.ID, f.userID)
	require.True(t, f.held(f.agentA.ID))

	key := xpMintHub(t, srv, admin, "xp-gov",
		"agent:update", "access_constraint:admin", "access_constraint:read")

	missingBroker := "/api/v1/runtime-brokers/" + tid("xp-gov-missing-broker")
	missingAgent := "/api/v1/agents/" + tid("xp-gov-missing-agent") + "/hold/lift"
	cases := []struct {
		name, method, path string
		body               interface{}
		missingPath        string
	}{
		{"broker update", http.MethodPatch, "/api/v1/runtime-brokers/" + broker.ID, map[string]string{"name": "xp-gov-renamed"}, missingBroker},
		{"broker delete", http.MethodDelete, "/api/v1/runtime-brokers/" + broker.ID, nil, missingBroker},
		{"access constraint delete", http.MethodDelete, "/api/v1/admin/access-constraints/" + constraint.ID, nil, "/api/v1/admin/access-constraints/" + tid("xp-gov-missing-constraint")},
		{"agent hold lift", http.MethodPost, "/api/v1/agents/" + f.agentA.ID + "/hold/lift", nil, missingAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestWithUAT(t, srv, key, tc.method, tc.path, tc.body)
			requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, tc.name)
			missing := doRequestWithUAT(t, srv, key, tc.method, tc.missingPath, tc.body)
			assert.Equal(t, rec.Code, missing.Code, "%s: a missing target gets the same answer", tc.name)
			assert.JSONEq(t, rec.Body.String(), missing.Body.String(), "%s: a missing target gets the same body", tc.name)
		})
	}

	gotBroker, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err, "the broker is kept")
	assert.Equal(t, "xp-gov-broker", gotBroker.Name, "the broker keeps its name")
	_, err = s.GetAccessConstraint(ctx, constraint.ID)
	assert.NoError(t, err, "the access constraint is kept")
	assert.True(t, f.held(f.agentA.ID), "the hold stays")
	assert.Zero(t, countAudits(t, s, mutationTypeAgentHoldCleared, f.agentA.ID), "no hold is cleared")

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/hold/lift", nil)
	require.Equal(t, http.StatusOK, rec.Code, "a session lifts the same hold: %s", rec.Body.String())
	assert.False(t, f.held(f.agentA.ID))
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// xpAgentDeleteAudit returns the delete mutation audit of agentID.
func xpAgentDeleteAudit(t *testing.T, s store.Store, agentID string) *store.MutationAuditRecord {
	t.Helper()
	records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetID: agentID, Limit: 100})
	require.NoError(t, err)
	var found []*store.MutationAuditRecord
	for _, r := range records {
		if r.MutationType == mutationTypeAgentHardDelete || r.MutationType == mutationTypeAgentSoftDelete {
			found = append(found, r)
		}
	}
	require.Len(t, found, 1, "one delete audit for agent %s", agentID)
	return found[0]
}

// xpAllowRecord returns the last captured allow decision for resourceID.
func xpAllowRecord(t *testing.T, e *capturingAuditEmitter, resourceID string) *store.DecisionAuditRecord {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	var last *store.DecisionAuditRecord
	for _, r := range e.records {
		if r.ResourceID == resourceID && r.Result == "allow" {
			last = r
		}
	}
	require.NotNil(t, last, "an allow decision for %s", resourceID)
	return last
}

// xpLabels decodes an audit labels snapshot.
func xpLabels(t *testing.T, raw string) map[string]string {
	t.Helper()
	require.NotEmpty(t, raw, "a labels snapshot is recorded")
	var labels map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &labels))
	return labels
}

// TestHubUATAudit_CrossProjectMutationRecordsHumanPrincipalAndDecoration
// pins that a hub token's cross-project mutation is attributed to the human
// holder and the token, with the token's name, hub boundary and labels and
// no executor, in both the mutation audit and the decision audit, and that
// the token's purpose text is recorded in neither.
func TestHubUATAudit_CrossProjectMutationRecordsHumanPrincipalAndDecoration(t *testing.T) {
	srv, s := testServer(t)
	srv.authzService.DecisionAuditSampleRate = 1.0
	projectQ, ownerQ := tid("xp-audit-project-q"), tid("xp-audit-owner-q")
	createRS1Project(t, s, projectQ, ownerQ)
	adminID := hubConfigTokenUser(t, s, "xp-audit-admin", store.SystemRoleSuperAdmin)
	agentQ := uatpAgent(t, s, projectQ, ownerQ, "xp-audit-q", ownerQ)

	const tokenName = "xp-audit-ops"
	const purpose = "xp audit purpose marker 7c1f"
	labels := map[string]string{"team": "ops", "ticket": "t-1"}
	key, tokenID := xpMintOverSession(t, srv, mustGetUser(t, s, adminID), map[string]interface{}{
		"name": tokenName, "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:delete"},
		"purpose": purpose, "labels": labels,
	})

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	rec := doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/agents/"+agentQ.ID, nil)
	require.Less(t, rec.Code, 300, "%d %s", rec.Code, rec.Body.String())

	mutation := xpAgentDeleteAudit(t, s, agentQ.ID)
	assert.Equal(t, string(PrincipalKindUser), mutation.ActorPrincipalKind)
	assert.Equal(t, adminID, mutation.ActorPrincipalID)
	assert.Equal(t, string(CredentialKindUAT), mutation.ActorCredentialType)
	assert.Equal(t, tokenID, mutation.ActorCredentialID)
	assert.Equal(t, tokenName, mutation.CredentialName)
	assert.Equal(t, string(BoundaryKindHub), mutation.CredentialBoundaryKind)
	assert.Empty(t, mutation.CredentialBoundaryProjectID)
	assert.Equal(t, labels, xpLabels(t, mutation.CredentialLabels))
	assert.Empty(t, mutation.ExecutorKind)
	assert.Empty(t, mutation.ExecutorID)

	decision := xpAllowRecord(t, emitter, agentQ.ID)
	assert.Equal(t, string(PrincipalKindUser), decision.PrincipalKind)
	assert.Equal(t, adminID, decision.PrincipalID)
	assert.Equal(t, tokenID, decision.CredentialID)
	assert.Equal(t, string(BoundaryKindHub), decision.CredentialBoundaryKind)
	assert.Empty(t, decision.CredentialBoundaryProjectID)
	assert.Equal(t, labels, xpLabels(t, decision.CredentialLabels))
	assert.Empty(t, decision.ExecutorKind)
	assert.Empty(t, decision.ExecutorID)

	for name, record := range map[string]interface{}{"mutation audit": mutation, "decision audit": decision} {
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), purpose, "%s never records the purpose", name)
	}
}

// TestHubUATAudit_AgentShapedLabelsNeverYieldAgentActor pins that token
// labels only describe the token: labels whose values name an agent never
// make an agent the recorded actor, and never change an authorization
// result. Label keys reserved for actor fields are refused at issuance.
func TestHubUATAudit_AgentShapedLabelsNeverYieldAgentActor(t *testing.T) {
	srv, s := testServer(t)
	srv.authzService.DecisionAuditSampleRate = 1.0
	ctx := context.Background()
	projectP, userID := tid("xp-labels-project-p"), tid("xp-labels-user")
	projectQ, ownerQ := tid("xp-labels-project-q"), tid("xp-labels-owner-q")
	createRS1Project(t, s, projectP, userID)
	createRS1Project(t, s, projectQ, ownerQ)
	user := mustGetUser(t, s, userID)
	named := uatpAgent(t, s, projectP, userID, "xp-labels-named", userID)
	ownAgent := uatpAgent(t, s, projectP, userID, "xp-labels-own", userID)
	target := uatpAgent(t, s, projectQ, ownerQ, "xp-labels-target", ownerQ, named.ID)

	for _, reserved := range []string{"agent", "actor", "principal_kind"} {
		rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
			"name": "xp-labels-reserved", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:read"},
			"labels": map[string]string{reserved: named.ID},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "label key %q: %s", reserved, rec.Body.String())
	}
	count, err := s.CountUserAccessTokens(ctx, userID)
	require.NoError(t, err)
	require.Zero(t, count, "a refused mint stores no token")

	scopes := []string{"agent:read", "agent:delete"}
	labelled, _ := xpMintOverSession(t, srv, user, map[string]interface{}{
		"name": "xp-labels-agent-shaped", "boundary": map[string]string{"kind": "hub"}, "scopes": scopes,
		"labels": map[string]string{"runs-as": "agent:" + named.ID, "kind": "agent", "via": named.Slug},
	})
	plain := xpMintHub(t, srv, user, "xp-labels-plain", scopes...)

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	rec := doRequestWithUAT(t, srv, labelled, http.MethodDelete, "/api/v1/agents/"+ownAgent.ID, nil)
	require.Less(t, rec.Code, 300, "%d %s", rec.Code, rec.Body.String())
	mutation := xpAgentDeleteAudit(t, s, ownAgent.ID)
	assert.Equal(t, string(PrincipalKindUser), mutation.ActorPrincipalKind)
	assert.Equal(t, userID, mutation.ActorPrincipalID)
	assert.Empty(t, mutation.ExecutorKind)
	assert.Empty(t, mutation.ExecutorID)
	decision := xpAllowRecord(t, emitter, ownAgent.ID)
	assert.Equal(t, string(PrincipalKindUser), decision.PrincipalKind)
	assert.Equal(t, userID, decision.PrincipalID)
	assert.Empty(t, decision.ExecutorKind)
	assert.Empty(t, decision.ExecutorID)

	// The target in Q descends from the agent the labels name. The labelled
	// token gets exactly the plain token's refusal.
	recPlain := doRequestWithUAT(t, srv, plain, http.MethodDelete, "/api/v1/agents/"+target.ID, nil)
	assert.Equal(t, http.StatusForbidden, recPlain.Code, recPlain.Body.String())
	plainReasons := deniedAuditReasons(emitter, target.ID)
	require.NotEmpty(t, plainReasons)

	labelledEmitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(labelledEmitter)
	recLabelled := doRequestWithUAT(t, srv, labelled, http.MethodDelete, "/api/v1/agents/"+target.ID, nil)
	assert.Equal(t, recPlain.Code, recLabelled.Code, recLabelled.Body.String())
	assert.JSONEq(t, recPlain.Body.String(), recLabelled.Body.String())
	assert.ElementsMatch(t, plainReasons, deniedAuditReasons(labelledEmitter, target.ID))
	denies := xpDenyRecords(labelledEmitter, target.ID)
	require.NotEmpty(t, denies)
	for _, d := range denies {
		assert.Equal(t, string(PrincipalKindUser), d.PrincipalKind)
		assert.Equal(t, userID, d.PrincipalID)
		assert.Empty(t, d.ExecutorKind)
		assert.Empty(t, d.ExecutorID)
	}
	xpRequireAgentKept(t, s, target.ID, "agent-shaped labels")
}

// TestCreateToken_HubBoundaryAuditRecordsDecorationKeysNotValues pins the
// decoration in a hub token's mint audit: whether a purpose was set and the
// sorted label keys, and never the purpose or label values.
func TestCreateToken_HubBoundaryAuditRecordsDecorationKeysNotValues(t *testing.T) {
	srv, s := testServer(t)
	adminID := hubConfigTokenUser(t, s, "xp-mintaudit-admin", store.SystemRoleSuperAdmin)
	const purpose = "xp mint audit purpose marker 3b9e"
	_, tokenID := xpMintOverSession(t, srv, mustGetUser(t, s, adminID), map[string]interface{}{
		"name": "xp-mintaudit", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:read"},
		"purpose": purpose, "labels": map[string]string{"zeta": "value-zeta-41", "alpha": "value-alpha-17"},
	})

	records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: "credential_create", TargetID: tokenID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, records, 1)
	summaryJSON := records[0].AfterSummary
	var summary map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(summaryJSON), &summary))
	assert.Equal(t, true, summary["has_purpose"])
	assert.Equal(t, []interface{}{"alpha", "zeta"}, summary["label_keys"])
	for _, value := range []string{purpose, "value-zeta-41", "value-alpha-17"} {
		assert.False(t, strings.Contains(summaryJSON, value), "the mint audit never records %q", value)
	}
}

// ---------------------------------------------------------------------------
// Grant-effect protection
// ---------------------------------------------------------------------------

// TestHubUATGrantEffect_CrossProjectMemberAddCannotExceedTokenScopes pins
// that a hub token grants a project role only when its selectors cover
// every permission of the role and its holder can currently manage that project:
// a token with project:manage alone cannot grant project-member, a token
// that covers the role cannot grant it in a project its holder has no
// access to, and the same token grants it in the holder's project.
func TestHubUATGrantEffect_CrossProjectMemberAddCannotExceedTokenScopes(t *testing.T) {
	f := setupMMRFixture(t)
	manageOnly := xpMintHub(t, f.srv, f.owner, "xp-grant-manage", "project:manage")

	t1 := grpUser(t, f.store, t.Name()+"-t1", "T1")
	before := len(mmrAuditRows(t, f.store, f.projectID))
	rec := doRequestWithUAT(t, f.srv, manageOnly, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: t1.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeTargetRoleProtected, mtErrCode(rec), rec.Body.String())
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", t1.ID, f.projectID))
	assert.Len(t, mmrAuditRows(t, f.store, f.projectID), before, "a refused grant changes no membership")

	mtNarrowRole(t, f.store, f.memberRD, mtNarrowPerms...)
	covering := xpMintHub(t, f.srv, f.owner, "xp-grant-covering", mtNarrowSelectors...)

	t2 := grpUser(t, f.store, t.Name()+"-t2", "T2")
	beforeOther := len(mmrAuditRows(t, f.store, f.otherProjectID))
	rec = doRequestWithUAT(t, f.srv, covering, http.MethodPost, legacyMembersPath(f.otherProjectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: t2.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a project the holder cannot access: %s", rec.Body.String())
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", t2.ID, f.otherProjectID))
	assert.Len(t, mmrAuditRows(t, f.store, f.otherProjectID), beforeOther, "a refused grant changes no membership")

	t3 := grpUser(t, f.store, t.Name()+"-t3", "T3")
	rec = doRequestWithUAT(t, f.srv, covering, http.MethodPost, legacyMembersPath(f.projectID),
		addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: "user", PrincipalID: t3.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "user", t3.ID, f.projectID), 1)
}
