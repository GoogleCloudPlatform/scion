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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for DELETE /api/v1/test-identities/{id} (ptone/scion#4240, Phase
// 2c). All identities here are synthetic fixtures in an in-memory store.

func tiDelete(t *testing.T, srv *Server, token, id string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, srv, token, http.MethodDelete, "/api/v1/test-identities/"+id, nil)
}

// tiDeleteAudits returns the test_identity_delete audit records for id.
func tiDeleteAudits(t *testing.T, s store.Store, id string) []store.MutationAuditRecord {
	t.Helper()
	audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: testIdentityDeleteMutation, TargetID: id})
	require.NoError(t, err)
	out := make([]store.MutationAuditRecord, 0, len(audits))
	for _, a := range audits {
		out = append(out, *a)
	}
	return out
}

// tiTokenSecret is the signature segment of a JWT: a substring that must
// never appear in an audit record or a response.
func tiTokenSecret(token string) string {
	return token[strings.LastIndex(token, ".")+1:]
}

// A fixture with no agents is deleted (204). Afterwards its token gets 401,
// GET /api/v1/users/{id} gets 404, and its role binding (viewer) and group
// membership (member) rows are gone. One test_identity_delete audit record
// per delete, attributed to the issuer's credential, with no token in it.
// A second delete gets 404 and writes no audit record.
func TestTestIdentity_Delete(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-del-issuer")
	issuer, err := s.GetUser(ctx, issuerID)
	require.NoError(t, err)

	for _, role := range []string{store.UserRoleMember, store.UserRoleViewer} {
		t.Run(role, func(t *testing.T) {
			fx := tiIssue(t, srv, issuerTok, map[string]string{"role": role, "purpose": "teardown"})
			id := fx.Identity.ID
			require.Equal(t, http.StatusOK, tiAuthMe(t, srv, fx.AccessToken).Code)

			bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
			require.NoError(t, err)
			groups, err := s.GetUserGroups(ctx, id)
			require.NoError(t, err)
			if role == store.UserRoleViewer {
				require.NotEmpty(t, bindings, "a viewer fixture holds a hub-viewer binding")
			} else {
				require.NotEmpty(t, groups, "a member fixture is in the hub-members group")
			}

			rec := tiDelete(t, srv, issuerTok, id)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Empty(t, rec.Body.String())

			// Token 401, user 404, binding and membership rows gone.
			rec = tiAuthMe(t, srv, fx.AccessToken)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/users/"+id, nil).Code)
			_, err = s.GetUser(ctx, id)
			assert.ErrorIs(t, err, store.ErrNotFound)
			bindings, err = s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
			require.NoError(t, err)
			assert.Empty(t, bindings)
			groups, err = s.GetUserGroups(ctx, id)
			require.NoError(t, err)
			assert.Empty(t, groups)

			// The list no longer shows it, even with includeExpired.
			rec = doRequestWithToken(t, srv, issuerTok, http.MethodGet, "/api/v1/test-identities?includeExpired=true", nil)
			require.Equal(t, http.StatusOK, rec.Code)
			assert.NotContains(t, rec.Body.String(), id)

			// One audit record, attributed to the issuer's token, no token.
			audits := tiDeleteAudits(t, s, id)
			require.Len(t, audits, 1)
			a := audits[0]
			assert.Equal(t, issuerID, a.ActorPrincipalID)
			assert.NotEmpty(t, a.ActorCredentialID, "the issuer credential ID is recorded")
			assert.NotEmpty(t, a.ActorCredentialType, "the issuer credential kind is recorded")
			assert.Equal(t, testIdentityAuditTargetType, a.TargetType)
			var before map[string]string
			require.NoError(t, json.Unmarshal([]byte(a.BeforeSummary), &before), a.BeforeSummary)
			assert.Equal(t, id, before["user_id"])
			assert.Equal(t, role, before["role"])
			assert.Equal(t, issuerID, before["issued_by"])
			assert.Equal(t, "teardown", before["purpose"])
			for _, field := range []string{a.BeforeSummary, a.AfterSummary} {
				assert.NotContains(t, field, tiTokenSecret(fx.AccessToken))
				assert.NotContains(t, strings.ToLower(field), "token\":\"ey")
			}

			// Idempotent: a second delete gets 404 and writes nothing.
			rec = tiDelete(t, srv, issuerTok, id)
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Len(t, tiDeleteAudits(t, s, id), 1)
		})
	}
}

// With agents present the delete answers 409 listing them and deletes
// nothing. Once the agent is gone, a project the fixture is the last owner
// of answers 409 last_owner listing it (the existing user-delete guard;
// Phase 3 purge removes such projects). Once that is gone too, 204.
func TestTestIdentity_DeleteOwnsAgentsAndLastOwner(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	srv.SetDispatcher(&deleteGuardDispatcher{})
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-del-agents-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member"})
	tok := fx.AccessToken

	projectID := tiCreateProject(t, srv, tok, "ti-delete-project")
	tiAddProvider(t, s, projectID)
	rec := doRequestWithToken(t, srv, tok, http.MethodPost, "/api/v1/projects/"+projectID+"/agents", CreateAgentRequest{Name: "ti-delete-agent"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create agent: %d %s", rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Agent)
	agentID := created.Agent.ID

	// 409 listing the agent; nothing deleted.
	rec = tiDelete(t, srv, issuerTok, fx.Identity.ID)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Agents   []ownedAgentRef       `json:"agents"`
				Projects []lastOwnerProjectRef `json:"projects"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	require.Len(t, resp.Error.Details.Agents, 1)
	assert.Equal(t, agentID, resp.Error.Details.Agents[0].ID)
	assert.Equal(t, projectID, resp.Error.Details.Agents[0].ProjectID)
	_, err := s.GetUser(ctx, fx.Identity.ID)
	require.NoError(t, err, "the identity is not deleted")
	_, err = s.GetAgent(ctx, agentID)
	require.NoError(t, err, "the agent is not touched")
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, tok).Code, "the token still works")
	assert.Empty(t, tiDeleteAudits(t, s, fx.Identity.ID))

	// The agent goes; the project the fixture solely owns still blocks.
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "delete agent: %d %s", rec.Code, rec.Body.String())
	// Finish the delete in the store too, whatever phase the dispatcher
	// left the agent in.
	if err := s.DeleteAgent(ctx, agentID); err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
	}
	rec = tiDelete(t, srv, issuerTok, fx.Identity.ID)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	resp.Error.Details.Agents = nil
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeLastOwner, resp.Error.Code)
	require.Len(t, resp.Error.Details.Projects, 1)
	assert.Equal(t, projectID, resp.Error.Details.Projects[0].ID)
	_, err = s.GetUser(ctx, fx.Identity.ID)
	require.NoError(t, err, "the identity is not deleted")
	assert.Empty(t, tiDeleteAudits(t, s, fx.Identity.ID))

	// The project goes; the delete succeeds.
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/projects/"+projectID, nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "delete project: %d %s", rec.Code, rec.Body.String())
	rec = tiDelete(t, srv, issuerTok, fx.Identity.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Len(t, tiDeleteAudits(t, s, fx.Identity.ID), 1)
}

// DELETE on a non-fixture user answers 404 and deletes nothing, for the
// issuer's token and for an admin session alike: a real human member, the
// caller itself, an unknown ID and a malformed ID.
func TestTestIdentity_DeleteRefusesNonFixture(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-del-nf-issuer")
	issuer, err := s.GetUser(ctx, issuerID)
	require.NoError(t, err)

	humanID := tid("ti-del-nf-human")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: humanID, Email: humanID + "@example.com", DisplayName: "Human member",
		Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, s, humanID)

	for _, id := range []string{humanID, issuerID, generateID(), "not-a-uuid"} {
		rec := tiDelete(t, srv, issuerTok, id)
		assert.Equal(t, http.StatusNotFound, rec.Code, "issuer token, id %s: %s", id, rec.Body.String())
		rec = doRequestAsUser(t, srv, issuer, http.MethodDelete, "/api/v1/test-identities/"+id, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "admin session, id %s: %s", id, rec.Body.String())
	}
	human, err := s.GetUser(ctx, humanID)
	require.NoError(t, err, "the human user is not deleted")
	assert.Equal(t, store.UserStatusActive, human.Status)
	groups, err := s.GetUserGroups(ctx, humanID)
	require.NoError(t, err)
	assert.NotEmpty(t, groups, "the human user's membership is untouched")
	_, err = s.GetUser(ctx, issuerID)
	require.NoError(t, err)
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDeleteMutation})
	require.NoError(t, err)
	assert.Empty(t, audits)
}

// Issuer B cannot delete issuer A's fixture (404, nothing deleted); an
// admin session can. A fixture cannot delete itself or anyone (403), and a
// caller without test_identity.issue is refused (403).
func TestTestIdentity_DeleteAuthorization(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	aID, aTok := tiIssuer(t, srv, s, "ti-del-authz-a")
	bID, bTok := tiIssuer(t, srv, s, "ti-del-authz-b")
	a1 := tiIssue(t, srv, aTok, nil)
	a2 := tiIssue(t, srv, aTok, nil)
	b1 := tiIssue(t, srv, bTok, nil)

	// B's issuer token on A's fixture: 404; and the reverse.
	assert.Equal(t, http.StatusNotFound, tiDelete(t, srv, bTok, a1.Identity.ID).Code)
	assert.Equal(t, http.StatusNotFound, tiDelete(t, srv, aTok, b1.Identity.ID).Code)
	for _, id := range []string{a1.Identity.ID, b1.Identity.ID} {
		_, err := s.GetUser(ctx, id)
		require.NoError(t, err)
	}

	// A fixture's own token: 403, on itself and on a sibling.
	assert.Equal(t, http.StatusForbidden, tiDelete(t, srv, a1.AccessToken, a1.Identity.ID).Code)
	assert.Equal(t, http.StatusForbidden, tiDelete(t, srv, a1.AccessToken, a2.Identity.ID).Code)

	// A member without test_identity.issue: 403.
	memberID := tid("ti-del-authz-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: memberID + "@example.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, s, memberID)
	member, err := s.GetUser(ctx, memberID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, srv, member, http.MethodDelete, "/api/v1/test-identities/"+a1.Identity.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// An admin session (B, a super-admin) deletes A's fixture.
	b, err := s.GetUser(ctx, bID)
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, b, http.MethodDelete, "/api/v1/test-identities/"+a1.Identity.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	audits := tiDeleteAudits(t, s, a1.Identity.ID)
	require.Len(t, audits, 1)
	assert.Equal(t, bID, audits[0].ActorPrincipalID)

	// The issuer deletes its own.
	require.Equal(t, http.StatusNoContent, tiDelete(t, srv, aTok, a2.Identity.ID).Code)
	audits = tiDeleteAudits(t, s, a2.Identity.ID)
	require.Len(t, audits, 1)
	assert.Equal(t, aID, audits[0].ActorPrincipalID)
}

// An expired fixture can still be deleted by its issuer.
func TestTestIdentity_DeleteExpired(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-del-exp-issuer")
	fx := tiStoreFixture(t, s, issuerID, time.Now().Add(-time.Minute))
	require.Equal(t, http.StatusNoContent, tiDelete(t, srv, issuerTok, fx.ID).Code)
	_, err := s.GetUser(context.Background(), fx.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A failing audit write rolls the delete back: the identity, its token,
// its membership and its binding all survive.
func TestTestIdentity_DeleteAuditFailureRollsBack(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-del-rb-issuer")
	member := tiIssue(t, srv, issuerTok, map[string]string{"role": "member"})
	viewer := tiIssue(t, srv, issuerTok, map[string]string{"role": "viewer"})

	srv.testIdentities.hooks.writeAudit = func(context.Context, store.Store, *store.MutationAuditRecord) error {
		return errors.New("audit unavailable")
	}
	for _, fx := range []TestIdentityTokenResponse{member, viewer} {
		groupsBefore, err := s.GetUserGroups(ctx, fx.Identity.ID)
		require.NoError(t, err)
		bindingsBefore, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fx.Identity.ID)
		require.NoError(t, err)

		rec := tiDelete(t, srv, issuerTok, fx.Identity.ID)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

		_, err = s.GetUser(ctx, fx.Identity.ID)
		require.NoError(t, err, "the identity survives a failed audit write")
		assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, fx.AccessToken).Code)
		groupsAfter, err := s.GetUserGroups(ctx, fx.Identity.ID)
		require.NoError(t, err)
		assert.Len(t, groupsAfter, len(groupsBefore))
		bindingsAfter, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fx.Identity.ID)
		require.NoError(t, err)
		assert.Len(t, bindingsAfter, len(bindingsBefore))
	}
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityDeleteMutation})
	require.NoError(t, err)
	assert.Empty(t, audits)

	// With the audit write restored the delete goes through.
	srv.testIdentities.hooks = newTestIdentityState(true).hooks
	assert.Equal(t, http.StatusNoContent, tiDelete(t, srv, issuerTok, member.Identity.ID).Code)
}

// Flag off: DELETE returns 404 for every caller and deletes nothing.
func TestTestIdentity_DeleteFlagOff(t *testing.T) {
	srv, s := newTestIdentityServer(t, false)
	ctx := context.Background()
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-del-off-issuer")
	issuer, err := s.GetUser(ctx, issuerID)
	require.NoError(t, err)
	fx := tiStoreFixture(t, s, issuerID, time.Now().Add(time.Hour))

	assert.Equal(t, http.StatusNotFound, tiDelete(t, srv, issuerTok, fx.ID).Code)
	rec := doRequestAsUser(t, srv, issuer, http.MethodDelete, "/api/v1/test-identities/"+fx.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	_, err = s.GetUser(ctx, fx.ID)
	require.NoError(t, err)
}

// tiMemberWithSystemRole creates a non-admin human member bound to a custom
// hub-scope role holding perms, and returns the user.
func tiMemberWithSystemRole(t *testing.T, s store.Store, name string, perms ...string) *store.User {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@example.com", DisplayName: name,
		Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, s, id)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID: api.NewUUID(), Name: name + "-role", Description: "test role",
		ScopeType: store.RoleScopeSystem, Permissions: perms,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

// The test_identity.issue plus user.delete branch of canDeleteTestIdentity:
// a non-admin member C holding both (through a custom hub-scope role)
// deletes issuer A's fixture. A member D holding only user.delete is
// refused by the test_identity.issue guard (403), and a member E holding
// only test_identity.issue, not the issuer, gets 404 on A's fixture.
func TestTestIdentity_DeleteViaUserDelete(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, aTok := tiIssuer(t, srv, s, "ti-del-ud-issuer-a")
	fx := tiIssue(t, srv, aTok, nil)

	c := tiMemberWithSystemRole(t, s, "ti-del-ud-c", permissionTestIdentityIssue, "user.delete")
	d := tiMemberWithSystemRole(t, s, "ti-del-ud-d", "user.delete")
	e := tiMemberWithSystemRole(t, s, "ti-del-ud-e", permissionTestIdentityIssue)
	for _, u := range []*store.User{c, d, e} {
		require.False(t, IsUnscopedLocalPlatformAdmin(NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")), "%s is not an admin", u.ID)
	}
	path := "/api/v1/test-identities/" + fx.Identity.ID

	// D: user.delete alone does not pass the test_identity.issue guard.
	rec := doRequestAsUser(t, srv, d, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// E: test_identity.issue alone, not the issuer: 404.
	rec = doRequestAsUser(t, srv, e, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	_, err := s.GetUser(ctx, fx.Identity.ID)
	require.NoError(t, err, "D and E delete nothing")
	assert.Empty(t, tiDeleteAudits(t, s, fx.Identity.ID))

	// C: test_identity.issue plus user.delete: 204, audited as C.
	rec = doRequestAsUser(t, srv, c, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = s.GetUser(ctx, fx.Identity.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	audits := tiDeleteAudits(t, s, fx.Identity.ID)
	require.Len(t, audits, 1)
	assert.Equal(t, c.ID, audits[0].ActorPrincipalID)

	// C still cannot delete a human user on this route.
	rec = doRequestAsUser(t, srv, c, http.MethodDelete, "/api/v1/test-identities/"+d.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	_, err = s.GetUser(ctx, d.ID)
	require.NoError(t, err)
}

// A transient store conflict during the delete answers 429 with
// Retry-After and details.reason issuance_busy (the reason shared with
// issuance, testIdentityReasonBusy); nothing is deleted.
func TestTestIdentity_DeleteTransientConflict(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	_, issuerTok := tiIssuer(t, srv, s, "ti-del-busy-issuer")
	fx := tiIssue(t, srv, issuerTok, nil)
	srv.testIdentities.hooks.writeAudit = func(context.Context, store.Store, *store.MutationAuditRecord) error {
		return fmt.Errorf("write audit: %w", store.ErrTransient)
	}
	rec := tiDelete(t, srv, issuerTok, fx.Identity.ID)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), testIdentityReasonBusy)
	_, err := s.GetUser(context.Background(), fx.Identity.ID)
	require.NoError(t, err, "a transient conflict deletes nothing")
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, fx.AccessToken).Code)
}
