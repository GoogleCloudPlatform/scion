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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realTokenContext authenticates a minted token key the way the auth
// middleware does and returns a request context carrying its identity,
// credential and auth type.
func realTokenContext(t *testing.T, srv *Server, key string) context.Context {
	t.Helper()
	scoped, err := srv.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), userContextKey{}, scoped)
	ctx = contextWithIdentity(ctx, scoped)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(scoped))
	return contextWithAuthType(ctx, AuthTypeUAT)
}

// requestWithContext builds a request for a direct handler call.
func requestWithContext(ctx context.Context, method, path string, body interface{}) *http.Request {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func requireSessionOnlyRefusal(t *testing.T, rec *httptest.ResponseRecorder, want authzop.SessionOnlyReason, label string) {
	t.Helper()
	reason, credential := sessionOnlyDetailsOf(rec)
	assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	assert.Equal(t, string(want), reason, "%s: details.reason: %s", label, rec.Body.String())
	assert.Equal(t, sessionRequiredCredential, credential, "%s: details.credential: %s", label, rec.Body.String())
}

// TestSessionOnlyGate_ReasonIsReported pins that every session-only site
// refuses a real user access token with 403 and the site's reason in
// details.reason, plus details.credential = session_required. The token is
// a super-admin's hub token carrying every mintable selector, so no ceiling
// or authority check can be the cause of the refusal.
func TestSessionOnlyGate_ReasonIsReported(t *testing.T) {
	m := newBearerMatrixFixture(t)
	key, _ := m.everySelectorHubToken(t)
	f := m.ids

	httpCases := []struct {
		name   string
		method string
		path   string
		body   interface{}
		want   authzop.SessionOnlyReason
	}{
		{"own profile update", http.MethodPatch, "/api/v1/users/" + m.adminID, map[string]interface{}{"displayName": "x"}, authzop.ReasonInteractiveState},
		{"other user update", http.MethodPatch, "/api/v1/users/" + f.user, map[string]interface{}{"displayName": "x"}, authzop.ReasonGovernancePending},
		{"user delete", http.MethodDelete, "/api/v1/users/" + f.userDel, nil, authzop.ReasonGovernancePending},
		{"revoke sessions", http.MethodPost, "/api/v1/users/" + f.user + "/revoke-sessions", map[string]interface{}{}, authzop.ReasonSessionRecovery},
		{"terminal workspace read", http.MethodGet, "/api/v1/users/me/terminal-workspace", nil, authzop.ReasonInteractiveState},
		{"terminal workspace write", http.MethodPut, "/api/v1/users/me/terminal-workspace", map[string]interface{}{"agentIds": []string{}}, authzop.ReasonInteractiveState},
		{"token list", http.MethodGet, "/api/v1/auth/tokens", nil, authzop.ReasonCredentialManagement},
		{"token read", http.MethodGet, "/api/v1/auth/tokens/" + f.uatRevokeDelete, nil, authzop.ReasonCredentialManagement},
		{"token create", http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{"name": "x", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:read"}}, authzop.ReasonCredentialManagement},
		{"token revoke", http.MethodPost, "/api/v1/auth/tokens/" + f.uatRevokePost + "/revoke", map[string]interface{}{}, authzop.ReasonCredentialManagement},
		{"token delete", http.MethodDelete, "/api/v1/auth/tokens/" + f.uatRevokeDelete, nil, authzop.ReasonCredentialManagement},
		{"project member add", http.MethodPost, "/api/v1/projects/" + f.project + "/members", map[string]interface{}{
			"roleDefinitionId": f.projectMemberRoleID, "principalType": "user", "principalId": f.user,
		}, authzop.ReasonGovernancePending},
		{"project member remove", http.MethodDelete, "/api/v1/projects/" + f.project + "/members/" + f.projectMembership, nil, authzop.ReasonGovernancePending},
		{"port registration", http.MethodPost, "/api/v1/agents/" + f.agent + "/ports", map[string]interface{}{"port": 18081}, authzop.ReasonGovernancePending},
		{"message mode", http.MethodPost, "/api/v1/agents/" + f.agent + "/set_message_mode", map[string]interface{}{"mode": "project"}, authzop.ReasonGovernancePending},
		{"scheduled event create", http.MethodPost, "/api/v1/projects/" + f.project + "/scheduled-events", map[string]interface{}{"eventType": "dispatch_agent", "fireIn": "1h", "agentName": "x"}, authzop.ReasonGovernancePending},
		{"schedule create", http.MethodPost, "/api/v1/projects/" + f.project + "/schedules", map[string]interface{}{"name": "x", "cronExpr": "0 * * * *", "eventType": "message", "agentName": "x", "message": "x"}, authzop.ReasonGovernancePending},
		{"schedule update", http.MethodPatch, "/api/v1/projects/" + f.project + "/schedules/" + f.schedule, map[string]interface{}{"name": "x"}, authzop.ReasonGovernancePending},
		{"schedule resume", http.MethodPost, "/api/v1/projects/" + f.project + "/schedules/" + f.schedule + "/resume", nil, authzop.ReasonGovernancePending},
	}
	for _, tc := range httpCases {
		rec := doRequestWithUAT(t, m.srv, key, tc.method, tc.path, tc.body)
		requireSessionOnlyRefusal(t, rec, tc.want, tc.name)
	}

	// Sites a token cannot reach through the route layer, because the
	// role_binding permissions carry no selector, are called directly with
	// the same minted token, authenticated as the middleware does. Project
	// delete is pinned at the service by TestRS3_ProjectDeleteScopedUATDenied.
	// The scheduled authoring functions are also called directly, so the
	// refusal is pinned at each function as well as at the routes above.
	ctx := realTokenContext(t, m.srv, key)

	rec := httptest.NewRecorder()
	ok := m.srv.authorizeScheduledDispatchAgentAuthoring(rec, requestWithContext(ctx, http.MethodPost, "/api/v1/projects/"+f.project+"/scheduled-events", nil))
	assert.False(t, ok)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "scheduled dispatch authoring")

	// Scheduled message authoring is not session-only either: the revision
	// records the token's ceiling, and each fire re-checks the token under
	// resolveScheduledAuthority. Whatever the send preview decides, the
	// token is not refused as session-only.
	rec = httptest.NewRecorder()
	payload := `{"agentId":"` + f.agent + `","message":"x"}`
	m.srv.authorizeScheduledMessageAuthoring(rec, requestWithContext(ctx, http.MethodPost, "/api/v1/projects/"+f.project+"/scheduled-events", nil), f.project, payload, "", "")
	_, credential := sessionOnlyDetailsOf(rec)
	assert.NotEqual(t, sessionRequiredCredential, credential, "scheduled message authoring: refused as session-only: %s", rec.Body.String())

	bindings, err := m.store.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, m.adminID)
	require.NoError(t, err)
	superAdmin, err := m.store.GetRoleDefinitionByName(context.Background(), store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	var binding *store.RoleBinding
	for _, b := range bindings {
		if b.RoleDefinitionID == superAdmin.ID {
			binding = b
		}
	}
	require.NotNil(t, binding, "the fixture super-admin has a super-admin binding")
	actor := GetUserIdentityFromContext(ctx)
	require.NotNil(t, actor)
	rec = httptest.NewRecorder()
	m.srv.deleteSystemSuperAdminBinding(rec, requestWithContext(ctx, http.MethodDelete, "/api/v1/admin/role-bindings/"+binding.ID, nil), binding, actor, superAdmin)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "super-admin binding delete")
	_, err = m.store.GetRoleBinding(context.Background(), binding.ID)
	assert.NoError(t, err, "the refused delete leaves the binding in place")
}

// TestSessionOnlyGate_AllowsInteractiveAndDevOnly pins the credential rule
// of requireSessionCredentialFor: interactive session and dev credentials
// pass; every other credential kind, including an empty one, is refused
// with 403 and the reason. A missing identity is 401, and an identity that
// is not a user is 403 without the reason.
func TestSessionOnlyGate_AllowsInteractiveAndDevOnly(t *testing.T) {
	srv, _ := testServer(t)
	user := NewAuthenticatedUser(tid("sog-user"), "sog@test.com", "SOG", "member", string(ClientTypeAPI))

	for _, kind := range []CredentialKind{CredentialKindInteractive, CredentialKindDev} {
		ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), user), CredentialContext{Kind: kind})
		rec := httptest.NewRecorder()
		actor, ok := srv.requireSessionCredentialFor(rec, ctx, authzop.ReasonInteractiveState)
		assert.True(t, ok, "%s passes", kind)
		assert.Equal(t, user.ID(), actor.ID())
	}

	refused := []CredentialKind{"", CredentialKindUAT, CredentialKindAgentJWT, CredentialKindFederation, CredentialKindBroker, CredentialKindHubDelivery, "unknown"}
	for _, kind := range refused {
		ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), user), CredentialContext{Kind: kind})
		rec := httptest.NewRecorder()
		_, ok := srv.requireSessionCredentialFor(rec, ctx, authzop.ReasonGovernancePending)
		assert.False(t, ok, "%q is refused", kind)
		requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "credential kind "+string(kind))
		assert.Contains(t, rec.Body.String(), ErrCodeForbidden)
	}

	rec := httptest.NewRecorder()
	_, ok := srv.requireSessionCredentialFor(rec, context.Background(), authzop.ReasonGovernancePending)
	assert.False(t, ok)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	nonUser := NewFederatedServiceIdentity("https://issuer.example", "svc", "svc@test.com", nil)
	ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), nonUser), CredentialContext{Kind: CredentialKindInteractive})
	rec = httptest.NewRecorder()
	_, ok = srv.requireSessionCredentialFor(rec, ctx, authzop.ReasonGovernancePending)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	reason, _ := sessionOnlyDetailsOf(rec)
	assert.Empty(t, reason, "a non-user identity is refused without a session-only reason")
}

// TestSessionOnlyPredicateSites_NonTokenUserIdentitiesKeepTheirResult pins
// that the sites which refuse a user access token by identity type (message
// mode, port registration, scheduled authoring) refuse only that identity:
// a federated user identity and a user identity presented by a broker on a
// user's behalf are not refused with the session-only details there.
func TestSessionOnlyPredicateSites_NonTokenUserIdentitiesKeepTheirResult(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("sop-project")
	ownerID := tid("sop-owner")
	createRS1Project(t, s, projectID, ownerID)
	agent := uatpAgent(t, s, projectID, ownerID, "sop-agent", ownerID)

	identities := []struct {
		name string
		ctx  context.Context
	}{
		{"federated user", contextWithCredentialContext(
			contextWithIdentity(ctx, NewFederatedUserIdentity("https://issuer.example", "fed-user", "fed@test.com", "Fed", "member", nil)),
			CredentialContext{Kind: CredentialKindFederation})},
		{"broker on behalf of a user", contextWithCredentialContext(
			contextWithIdentity(ctx, NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", string(ClientTypeAPI))),
			CredentialContext{Kind: CredentialKindBroker})},
	}
	notSessionRefused := func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		_, credential := sessionOnlyDetailsOf(rec)
		assert.NotEqual(t, sessionRequiredCredential, credential, "%s: refused as session-only: %s", label, rec.Body.String())
		assert.False(t, strings.Contains(rec.Body.String(), "Scoped"), "%s: refused by the token rule: %s", label, rec.Body.String())
	}

	for _, id := range identities {
		rec := httptest.NewRecorder()
		ok := srv.authorizeScheduledDispatchAgentAuthoring(rec, requestWithContext(id.ctx, http.MethodPost, "/", nil))
		assert.True(t, ok, "%s: scheduled dispatch authoring gate admits it: %s", id.name, rec.Body.String())

		rec = httptest.NewRecorder()
		srv.authorizeScheduledMessageAuthoring(rec, requestWithContext(id.ctx, http.MethodPost, "/", nil), projectID, `{"agentId":"`+agent.ID+`","message":"x"}`, "", "")
		notSessionRefused(t, rec, id.name+": scheduled message authoring")

		rec = httptest.NewRecorder()
		srv.authorizePortRegistration(rec, requestWithContext(id.ctx, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", nil), agent.ID)
		notSessionRefused(t, rec, id.name+": port registration")

		rec = httptest.NewRecorder()
		srv.handleSetMessageMode(rec, requestWithContext(id.ctx, http.MethodPost, "/api/v1/agents/"+agent.ID+"/set_message_mode", map[string]interface{}{"mode": "project"}), agent.ID)
		notSessionRefused(t, rec, id.name+": message mode")
	}
}
