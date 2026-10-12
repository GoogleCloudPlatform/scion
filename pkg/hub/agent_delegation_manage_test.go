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

//go:build !no_sqlite && (!hubshard || hubshard_1)

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Helpers
// =============================================================================

func adtGrantPath(agentID, grantID string) string {
	return "/api/v1/agents/" + agentID + "/delegations/" + grantID
}

func adtCredentialPath(agentID, grantID, credentialID string) string {
	return adtGrantPath(agentID, grantID) + "/credentials/" + credentialID
}

func (f *adtFixture) readGrant(t *testing.T, token, agentID, grantID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodGet, adtGrantPath(agentID, grantID), nil, adtBearer(token))
}

func (f *adtFixture) revokeGrant(t *testing.T, token, agentID, grantID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodDelete, adtGrantPath(agentID, grantID), nil, adtBearer(token))
}

func (f *adtFixture) revokeCredential(t *testing.T, token, agentID, grantID, credentialID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodDelete, adtCredentialPath(agentID, grantID, credentialID), nil, adtBearer(token))
}

// listGrantIDs lists the grants at path with token and returns their IDs.
func (f *adtFixture) listGrantIDs(t *testing.T, token, path string) []string {
	t.Helper()
	rec := f.do(t, http.MethodGet, path, nil, adtBearer(token))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out ListAgentDelegationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := make([]string, 0, len(out.Delegations))
	for _, g := range out.Delegations {
		ids = append(ids, g.ID)
	}
	return ids
}

// projectGrant issues Alice's agent:read grant for agent A bounded to
// projectID.
func (f *adtFixture) projectGrant(t *testing.T, projectID string) AgentDelegationGrantResponse {
	t.Helper()
	rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "project", "projectId": projectID}, "permissions": []string{"agent:read"}, "name": "project-read",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	return grant
}

// projectRoleUser creates a hub member bound to roleName in projectID.
func (f *adtFixture) projectRoleUser(t *testing.T, name, projectID, roleName string) *store.User {
	t.Helper()
	u := hubMemberUser(t, f.store, name)
	adtGrantRole(t, f.store, u.ID, projectID, roleName)
	return u
}

// =============================================================================
// Control authority
// =============================================================================

// TestAgentDelegationManagement_ControlAuthority drives read, list, grant
// revoke and credential revoke for every actor kind of §7, for and against.
func TestAgentDelegationManagement_ControlAuthority(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt")
	alice := f.session(t, f.alice)
	carol := f.session(t, f.carol)
	bob := f.session(t, f.bob)
	hubAdmin := f.session(t, newHubAdminRoleUser(t, f.store, "adt-mgmt-hub-admin"))
	p1Admin := f.session(t, f.projectRoleUser(t, "adt-mgmt-p1-admin", f.proj.ID, store.ProjectRoleAdmin))
	p1Owner := f.session(t, f.projectRoleUser(t, "adt-mgmt-p1-owner", f.proj.ID, store.ProjectRoleOwner))
	p2Admin := f.session(t, f.projectRoleUser(t, "adt-mgmt-p2-admin", f.other.ID, store.ProjectRoleAdmin))
	p1Member := f.session(t, f.projectRoleUser(t, "adt-mgmt-p1-member", f.proj.ID, store.ProjectRoleMember))

	hubGrant := f.hubGrant(t)
	p1Grant := f.projectGrant(t, f.proj.ID)
	p2Grant := f.projectGrant(t, f.other.ID)
	listPath := "/api/v1/agents/" + f.agentA.ID + "/delegations"
	credentialOf := map[string]string{}
	for _, id := range []string{hubGrant.ID, p1Grant.ID, p2Grant.ID} {
		credentialOf[id] = f.delegated(t, id).CredentialID
	}

	cases := []struct {
		name    string
		token   string
		visible []string
	}{
		{"issuer and owner", alice, []string{hubGrant.ID, p1Grant.ID, p2Grant.ID}},
		{"hub admin", hubAdmin, []string{hubGrant.ID, p1Grant.ID, p2Grant.ID}},
		{"dev session", testDevToken, []string{hubGrant.ID, p1Grant.ID, p2Grant.ID}},
		{"project admin of the agent's project", p1Admin, []string{p1Grant.ID}},
		{"project owner of the agent's project", p1Owner, []string{p1Grant.ID}},
		{"project admin of the boundary project only", p2Admin, []string{p2Grant.ID}},
		{"project member", p1Member, nil},
		{"member of the agent's project who is not its owner", carol, nil},
		{"member of another project", bob, nil},
	}
	all := []string{hubGrant.ID, p1Grant.ID, p2Grant.ID}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.ElementsMatch(t, tc.visible, f.listGrantIDs(t, tc.token, listPath), "list leaves out grants outside the caller's authority")
			for _, id := range all {
				rec := f.readGrant(t, tc.token, f.agentA.ID, id)
				if slices.Contains(tc.visible, id) {
					assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				} else {
					adtAssertAPIError(t, rec, http.StatusNotFound, errCodeGrantNotFound)
					adtAssertAPIError(t, f.revokeGrant(t, tc.token, f.agentA.ID, id), http.StatusNotFound, errCodeGrantNotFound)
					adtAssertAPIError(t, f.revokeCredential(t, tc.token, f.agentA.ID, id, credentialOf[id]), http.StatusNotFound, errCodeGrantNotFound)
				}
			}
		})
	}

	t.Run("a grant under another agent's path is not found", func(t *testing.T) {
		adtAssertAPIError(t, f.readGrant(t, alice, f.agentC.ID, hubGrant.ID), http.StatusNotFound, errCodeGrantNotFound)
		adtAssertAPIError(t, f.readGrant(t, alice, f.agentA.ID, uuid.New().String()), http.StatusNotFound, errCodeGrantNotFound)
		adtAssertAPIError(t, f.readGrant(t, alice, f.agentA.ID, "not-a-uuid"), http.StatusNotFound, errCodeGrantNotFound)
	})

	t.Run("the project admin revokes only the project-bounded grant", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, f.revokeGrant(t, p1Admin, f.agentA.ID, p1Grant.ID).Code)
		adtAssertAPIError(t, f.revokeGrant(t, p1Admin, f.agentA.ID, hubGrant.ID), http.StatusNotFound, errCodeGrantNotFound)
		g, err := f.store.GetAgentDelegationGrant(context.Background(), hubGrant.ID)
		require.NoError(t, err)
		assert.Nil(t, g.RevokedAt)
	})

	t.Run("the hub admin revokes a hub-bounded grant", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, f.revokeGrant(t, hubAdmin, f.agentA.ID, hubGrant.ID).Code)
	})

	t.Run("the dev session revokes", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, f.revokeGrant(t, testDevToken, f.agentA.ID, p2Grant.ID).Code)
	})
}

// TestAgentDelegationManagement_OwnerWhoIsNotTheIssuer: the agent's current
// owner manages a grant an ancestor issued; a former owner does not.
func TestAgentDelegationManagement_OwnerWhoIsNotTheIssuer(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-owner")
	d, dave, _ := f.transferredAgent(t, "adt-mgmt-owner")
	grant := f.grantFor(t, f.session(t, f.alice), d.ID)
	daveSession := f.session(t, dave)

	require.Equal(t, http.StatusOK, f.readGrant(t, daveSession, d.ID, grant.ID).Code)
	assert.Equal(t, []string{grant.ID}, f.listGrantIDs(t, daveSession, "/api/v1/agents/"+d.ID+"/delegations"))

	// Ownership moves to Carol: Dave loses the owner path, Carol gains it.
	restore := f.mutateAgent(t, d.ID, func(a *store.Agent) { a.OwnerID = f.carol.ID })
	adtAssertAPIError(t, f.readGrant(t, daveSession, d.ID, grant.ID), http.StatusNotFound, errCodeGrantNotFound)
	require.Equal(t, http.StatusOK, f.readGrant(t, f.session(t, f.carol), d.ID, grant.ID).Code)
	// The issuer keeps the issuer path.
	require.Equal(t, http.StatusOK, f.readGrant(t, f.session(t, f.alice), d.ID, grant.ID).Code)
	restore()

	require.Equal(t, http.StatusNoContent, f.revokeGrant(t, daveSession, d.ID, grant.ID).Code)
}

// TestAgentDelegationManagement_BoundAgentToken: the bound agent reads,
// lists and relinquishes its own grants with its agent token, after the
// exchange steps 1-3 row checks; another agent cannot.
func TestAgentDelegationManagement_BoundAgentToken(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-agent")
	grant := f.hubGrant(t)
	tokenA := f.agentJWT(t, f.agentA)
	tokenC := f.agentJWT(t, f.agentC)

	t.Run("own grants", func(t *testing.T) {
		require.Equal(t, http.StatusOK, f.readGrant(t, tokenA, f.agentA.ID, grant.ID).Code)
		assert.Equal(t, []string{grant.ID}, f.listGrantIDs(t, tokenA, "/api/v1/agents/"+f.agentA.ID+"/delegations"))
	})
	t.Run("another agent's path", func(t *testing.T) {
		adtAssertAPIError(t, f.readGrant(t, tokenC, f.agentA.ID, grant.ID), http.StatusForbidden, errCodeCredentialNotAdmitted)
		adtAssertAPIError(t, f.revokeGrant(t, tokenC, f.agentA.ID, grant.ID), http.StatusForbidden, errCodeCredentialNotAdmitted)
	})
	t.Run("another agent's grant under its own path", func(t *testing.T) {
		adtAssertAPIError(t, f.revokeGrant(t, tokenC, f.agentC.ID, grant.ID), http.StatusNotFound, errCodeGrantNotFound)
	})
	t.Run("the users/me route needs a session", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/users/me/delegations", nil, adtBearer(tokenA))
		apiErr := adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
		assert.Empty(t, apiErr.Details)
	})
	t.Run("credential row checks", func(t *testing.T) {
		ctx := context.Background()
		stored, err := f.store.GetAgent(ctx, f.agentA.ID)
		require.NoError(t, err)
		tokenGrant, err := f.srv.AuthorizeAgentToken(ctx, stored)
		require.NoError(t, err)

		// No credential row (a legacy token): refused in the handler.
		noRow, _, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
		require.NoError(t, err)
		adtAssertAPIError(t, f.readGrant(t, noRow, f.agentA.ID, grant.ID), http.StatusUnauthorized, errCodeAgentCredentialInvalid)
		adtAssertAPIError(t, f.revokeGrant(t, noRow, f.agentA.ID, grant.ID), http.StatusUnauthorized, errCodeAgentCredentialInvalid)

		// An expired row: refused in the handler.
		expired, ac, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
		require.NoError(t, err)
		ac.ExpiresAt = time.Now().Add(-time.Minute)
		require.NoError(t, f.store.CreateAgentCredential(ctx, ac))
		adtAssertAPIError(t, f.readGrant(t, expired, f.agentA.ID, grant.ID), http.StatusUnauthorized, errCodeAgentCredentialInvalid)

		// A revoked row: refused by the agent token middleware.
		revoked, ac, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
		require.NoError(t, err)
		require.NoError(t, f.store.CreateAgentCredential(ctx, ac))
		require.NoError(t, f.store.RevokeAgentCredential(ctx, ac.ID, "test", "test"))
		adtAssertAPIError(t, f.readGrant(t, revoked, f.agentA.ID, grant.ID), http.StatusUnauthorized, ErrCodeUnauthorized)

		g, err := f.store.GetAgentDelegationGrant(ctx, grant.ID)
		require.NoError(t, err)
		assert.Nil(t, g.RevokedAt)
	})
	t.Run("relinquish", func(t *testing.T) {
		fresh := f.agentJWT(t, f.agentA)
		require.Equal(t, http.StatusNoContent, f.revokeGrant(t, fresh, f.agentA.ID, grant.ID).Code)
		g, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		require.NotNil(t, g.RevokedAt)
		assert.Equal(t, revokeReasonRelinquished, g.RevokeReason)
		assert.Equal(t, "agent:"+f.agentA.ID, g.RevokedBy)

		audits := adtAudits(t, f.store, mutationAgentDelegationGrantRevoke)
		require.Len(t, audits, 1)
		assert.Equal(t, "agent", audits[0].ActorPrincipalKind)
		assert.Equal(t, f.agentA.ID, audits[0].ActorPrincipalID)
		assert.Equal(t, string(CredentialKindAgentJWT), audits[0].ActorCredentialType)
		assert.Equal(t, actorKindAgentDelegated, audits[0].ActorKind)
		assert.Equal(t, f.agentA.ID, audits[0].ActorAgentID)
		assert.Equal(t, f.alice.ID, audits[0].AuthorizingUserID)
		assert.Equal(t, grant.ID, audits[0].SourceGrantID)
	})
}

// TestAgentDelegationManagement_RefusedCredentials: a user access token
// gets the session-only refusal, a delegated credential is refused, and
// both lists refuse a token the same way.
func TestAgentDelegationManagement_RefusedCredentials(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-refuse")
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)

	uat := scopedIdentityFor(f.alice, f.proj.ID, []string{"agent:read"})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agents/" + f.agentA.ID + "/delegations"},
		{http.MethodGet, "/api/v1/users/me/delegations"},
		{http.MethodGet, adtGrantPath(f.agentA.ID, grant.ID)},
		{http.MethodDelete, adtGrantPath(f.agentA.ID, grant.ID)},
		{http.MethodDelete, adtCredentialPath(f.agentA.ID, grant.ID, cred.CredentialID)},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := adtRequestWithCredential(t, f.srv, uat, tc.method, tc.path, nil)
			apiErr := adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
			assert.Equal(t, "CREDENTIAL_MANAGEMENT", apiErr.Details["reason"])
			assert.Equal(t, "session_required", apiErr.Details["credential"])

			rec = f.do(t, tc.method, tc.path, nil, adtBearer(cred.Token))
			adtAssertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
		})
	}
	g, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
	require.NoError(t, err)
	assert.Nil(t, g.RevokedAt, "no refused request revoked anything")
}

// TestAgentDelegationManagement_ReadWithoutPermissionIsForbidden: a caller
// who may read a grant but lacks the revoke permission gets 403, not 404.
func TestAgentDelegationManagement_ReadWithoutPermissionIsForbidden(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-readonly")
	ctx := context.Background()
	grant := f.projectGrant(t, f.proj.ID)

	role, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "adt-delegation-reader", ScopeType: store.RoleScopeProject,
		Permissions: []string{agentDelegationReadPermission, agentDelegationListPermission},
	})
	require.NoError(t, err)
	reader := hubMemberUser(t, f.store, "adt-mgmt-reader")
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: reader.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	session := f.session(t, reader)

	require.Equal(t, http.StatusOK, f.readGrant(t, session, f.agentA.ID, grant.ID).Code)
	adtAssertAPIError(t, f.revokeGrant(t, session, f.agentA.ID, grant.ID), http.StatusForbidden, ErrCodeForbidden)
}

// =============================================================================
// Effects
// =============================================================================

// TestAgentDelegationRevoke_CascadesWithOneAuditRow: grant revoke revokes
// every credential, records revocation_audit_id and writes one audit row
// that lists them; a repeat answers 204 and writes nothing; the revoked
// credential no longer authenticates.
func TestAgentDelegationRevoke_CascadesWithOneAuditRow(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-revoke")
	ctx := context.Background()
	grant := f.hubGrant(t)
	cred1 := f.delegated(t, grant.ID)
	cred2 := f.delegated(t, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred1.Token, f.agentA.ID).Code)

	alice := f.session(t, f.alice)
	require.Equal(t, http.StatusNoContent, f.revokeGrant(t, alice, f.agentA.ID, grant.ID).Code)

	g, err := f.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	require.NotNil(t, g.RevokedAt)
	assert.Equal(t, revokeReasonRevoked, g.RevokeReason)
	assert.Equal(t, "user:"+f.alice.ID, g.RevokedBy)

	audits := adtAudits(t, f.store, mutationAgentDelegationGrantRevoke)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, g.RevocationAuditID, a.ID)
	assert.Equal(t, grant.ID, a.TargetID)
	assert.Equal(t, f.alice.ID, a.ActorPrincipalID)
	assert.Equal(t, string(CredentialKindInteractive), a.ActorCredentialType)
	assert.Equal(t, f.agentA.ID, a.ActorAgentID)
	assert.Equal(t, f.alice.ID, a.AuthorizingUserID)
	assert.Equal(t, grant.ID, a.SourceGrantID)
	assert.Empty(t, a.ActorKind, "a session caller is not the bound agent")
	assert.Contains(t, a.BeforeSummary, cred1.CredentialID)
	assert.Contains(t, a.BeforeSummary, cred2.CredentialID)

	for _, c := range []ExchangeAgentDelegationResponse{cred1, cred2} {
		stored, err := f.store.GetAgentDelegatedCredential(ctx, c.CredentialID)
		require.NoError(t, err)
		require.NotNil(t, stored.RevokedAt)
		assert.Equal(t, revokeReasonRevoked, stored.RevokeReason)
		assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, c.Token, f.agentA.ID).Code)
	}

	// A repeat revoke answers 204 and writes no new audit row.
	require.Equal(t, http.StatusNoContent, f.revokeGrant(t, alice, f.agentA.ID, grant.ID).Code)
	assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationGrantRevoke), 1)

	// The revoked grant is still listed and readable, with its revocation.
	rec := f.readGrant(t, alice, f.agentA.ID, grant.ID)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "revoked", resp.Status)
	require.NotNil(t, resp.RevokedAt)
	assert.Equal(t, revokeReasonRevoked, resp.RevokeReason)
	assert.Zero(t, resp.ActiveCredentialCount)

	// Exchange refuses the revoked grant.
	adtAssertAPIError(t, f.exchangeFor(t, f.agentA, grant.ID), http.StatusForbidden, errCodeGrantInactive)
}

// TestAgentDelegationRevoke_AuditFailureRollsBack: a failed audit write
// leaves the grant and its credentials unrevoked; a failed grant revoke
// write does the same.
func TestAgentDelegationRevoke_AuditFailureRollsBack(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-rollback")
	ctx := context.Background()
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	alice := f.session(t, f.alice)

	f.faults.inject(t, adtFaults{audit: errors.New("audit store down")})
	adtAssertAPIError(t, f.revokeGrant(t, alice, f.agentA.ID, grant.ID), http.StatusInternalServerError, errCodeAuditFailed)
	adtAssertAPIError(t, f.revokeCredential(t, alice, f.agentA.ID, grant.ID, cred.CredentialID), http.StatusInternalServerError, errCodeAuditFailed)
	f.faults.clear()

	f.faults.inject(t, adtFaults{revoke: errors.New("grant store down")})
	require.Equal(t, http.StatusInternalServerError, f.revokeGrant(t, alice, f.agentA.ID, grant.ID).Code)
	f.faults.clear()

	g, err := f.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	assert.Nil(t, g.RevokedAt)
	assert.Empty(t, g.RevocationAuditID)
	stored, err := f.store.GetAgentDelegatedCredential(ctx, cred.CredentialID)
	require.NoError(t, err)
	assert.Nil(t, stored.RevokedAt)
	assert.Empty(t, adtAudits(t, f.store, mutationAgentDelegationGrantRevoke))
	assert.Empty(t, adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke))
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentA.ID).Code, "the credential still works")
}

// TestAgentDelegationCredentialRevoke_RevokesOneWithAudit: credential
// revoke ends one credential with its own audit action and leaves the grant
// and its other credential alone; a repeat writes nothing; a credential of
// another grant is not found.
func TestAgentDelegationCredentialRevoke_RevokesOneWithAudit(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-cred")
	ctx := context.Background()
	grant := f.hubGrant(t)
	other := f.hubGrant(t)
	cred1 := f.delegated(t, grant.ID)
	cred2 := f.delegated(t, grant.ID)
	otherCred := f.delegated(t, other.ID)
	alice := f.session(t, f.alice)

	for _, id := range []string{otherCred.CredentialID, uuid.New().String(), "not-a-uuid"} {
		adtAssertAPIError(t, f.revokeCredential(t, alice, f.agentA.ID, grant.ID, id), http.StatusNotFound, ErrCodeNotFound)
	}

	require.Equal(t, http.StatusNoContent, f.revokeCredential(t, alice, f.agentA.ID, grant.ID, cred1.CredentialID).Code)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred1.Token, f.agentA.ID).Code)
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred2.Token, f.agentA.ID).Code)
	g, err := f.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
	assert.Nil(t, g.RevokedAt)

	audits := adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, cred1.CredentialID, a.TargetID)
	assert.Equal(t, auditTargetAgentDelegatedCredential, a.TargetType)
	assert.Equal(t, f.alice.ID, a.ActorPrincipalID)
	assert.Equal(t, f.agentA.ID, a.ActorAgentID)
	assert.Equal(t, f.alice.ID, a.AuthorizingUserID)
	assert.Equal(t, grant.ID, a.SourceGrantID)
	assert.NotEmpty(t, a.ExchangeAgentCredentialID)
	assert.Contains(t, a.BeforeSummary, revokeReasonRevoked)

	require.Equal(t, http.StatusNoContent, f.revokeCredential(t, alice, f.agentA.ID, grant.ID, cred1.CredentialID).Code)
	assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke), 1, "a repeat writes no audit row")

	// The bound agent may revoke its own credential with its agent token.
	require.Equal(t, http.StatusNoContent, f.revokeCredential(t, f.agentJWT(t, f.agentA), f.agentA.ID, grant.ID, cred2.CredentialID).Code)
	stored, err := f.store.GetAgentDelegatedCredential(ctx, cred2.CredentialID)
	require.NoError(t, err)
	assert.Equal(t, revokeReasonRelinquished, stored.RevokeReason)
}

// =============================================================================
// Responses
// =============================================================================

// TestAgentDelegationList_LeavesOutGrantsOutsideAuthority: the per-user list
// returns the caller's issued grants only, with their active credentials;
// the per-agent list leaves out, and does not count, grants the caller may
// not see.
func TestAgentDelegationList_LeavesOutGrantsOutsideAuthority(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-list")
	d, dave, _ := f.transferredAgent(t, "adt-mgmt-list")
	aliceGrant := f.hubGrant(t)
	cred := f.delegated(t, aliceGrant.ID)
	daveGrant := f.grantFor(t, f.session(t, dave), d.ID)
	aliceOnD := f.grantFor(t, f.session(t, f.alice), d.ID)

	assert.ElementsMatch(t, []string{aliceGrant.ID, aliceOnD.ID}, f.listGrantIDs(t, f.session(t, f.alice), "/api/v1/users/me/delegations"))
	assert.ElementsMatch(t, []string{daveGrant.ID}, f.listGrantIDs(t, f.session(t, dave), "/api/v1/users/me/delegations"))
	assert.Empty(t, f.listGrantIDs(t, f.session(t, f.carol), "/api/v1/users/me/delegations"))
	assert.Empty(t, f.listGrantIDs(t, testDevToken, "/api/v1/users/me/delegations"), "a dev session issues no grants")

	// Carol owns neither agent: the per-agent list is empty, not an error.
	rec := f.do(t, http.MethodGet, "/api/v1/agents/"+f.agentA.ID+"/delegations", nil, adtBearer(f.session(t, f.carol)))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"delegations":[]}`, rec.Body.String())

	rec = f.do(t, http.MethodGet, "/api/v1/users/me/delegations", nil, adtBearer(f.session(t, f.alice)))
	require.Equal(t, http.StatusOK, rec.Code)
	var out ListAgentDelegationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	for _, g := range out.Delegations {
		if g.ID != aliceGrant.ID {
			continue
		}
		assert.Equal(t, 1, g.ActiveCredentialCount)
		require.Len(t, g.ActiveCredentials, 1)
		assert.Equal(t, cred.CredentialID, g.ActiveCredentials[0].ID)
		assert.WithinDuration(t, cred.ExpiresAt, g.ActiveCredentials[0].ExpiresAt, time.Second)
		assert.Equal(t, []string{"name", "purpose", "labels"}, g.UserSuppliedFields)
	}
}

// TestAgentDelegationManagement_NoSecretFieldsInResponses: no management
// response carries a credential, its hash or a prefix beyond scion_adt_.
func TestAgentDelegationManagement_NoSecretFieldsInResponses(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-secret")
	ctx := context.Background()
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	stored, err := f.store.GetAgentDelegatedCredential(ctx, cred.CredentialID)
	require.NoError(t, err)
	alice := f.session(t, f.alice)

	bodies := []string{
		f.readGrant(t, alice, f.agentA.ID, grant.ID).Body.String(),
		f.do(t, http.MethodGet, "/api/v1/agents/"+f.agentA.ID+"/delegations", nil, adtBearer(alice)).Body.String(),
		f.do(t, http.MethodGet, "/api/v1/users/me/delegations", nil, adtBearer(alice)).Body.String(),
		f.readGrant(t, f.agentJWT(t, f.agentA), f.agentA.ID, grant.ID).Body.String(),
	}
	body := strings.TrimPrefix(cred.Token, delegatedCredentialPrefix)
	for _, b := range bodies {
		assert.Contains(t, b, cred.CredentialID)
		assert.NotContains(t, b, cred.Token)
		assert.NotContains(t, b, body[:8], "no part of the credential body")
		assert.NotContains(t, b, stored.KeyHash)
		assert.NotContains(t, b, delegatedCredentialPrefix)
		for _, field := range []string{"keyHash", "key_hash", "token", "prefix"} {
			assert.NotContains(t, b, `"`+field+`"`)
		}
	}
}

// TestAgentDelegationManagement_AvailableWhileExperimentOff: list, read,
// grant revoke and credential revoke answer while hub.agent_delegation is
// off; issuance and exchange do not; a credential revoked while off stays
// refused when it is turned back on.
func TestAgentDelegationManagement_AvailableWhileExperimentOff(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-off")
	grant := f.hubGrant(t)
	keep := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	keepCred := f.delegated(t, keep.ID)
	alice := f.session(t, f.alice)

	adtSetExperiment(t, f.srv, false)
	assert.ElementsMatch(t, []string{grant.ID, keep.ID}, f.listGrantIDs(t, alice, "/api/v1/agents/"+f.agentA.ID+"/delegations"))
	assert.ElementsMatch(t, []string{grant.ID, keep.ID}, f.listGrantIDs(t, alice, "/api/v1/users/me/delegations"))
	require.Equal(t, http.StatusOK, f.readGrant(t, alice, f.agentA.ID, grant.ID).Code)
	require.Equal(t, http.StatusNoContent, f.revokeCredential(t, alice, f.agentA.ID, keep.ID, keepCred.CredentialID).Code)
	require.Equal(t, http.StatusNoContent, f.revokeGrant(t, alice, f.agentA.ID, grant.ID).Code)
	adtAssertAPIError(t, f.issue(t, alice, f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "n",
	}), http.StatusNotFound, ErrCodeNotFound)
	adtAssertAPIError(t, f.exchangeFor(t, f.agentA, keep.ID), http.StatusNotFound, ErrCodeNotFound)

	adtSetExperiment(t, f.srv, true)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentA.ID).Code)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, keepCred.Token, f.agentA.ID).Code)
	require.Equal(t, http.StatusOK, f.exchangeFor(t, f.agentA, keep.ID).Code, "the unrevoked grant works again")
}

// TestAgentDelegationManagement_MethodsAndRoutes: unsupported methods answer
// 405 and extra segments 404 on every management route.
func TestAgentDelegationManagement_MethodsAndRoutes(t *testing.T) {
	f := newADTFixture(t, "adt-mgmt-methods")
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	alice := f.session(t, f.alice)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/agents/" + f.agentA.ID + "/delegations"},
		{http.MethodPost, adtGrantPath(f.agentA.ID, grant.ID)},
		{http.MethodGet, adtCredentialPath(f.agentA.ID, grant.ID, cred.CredentialID)},
		{http.MethodPost, "/api/v1/users/me/delegations"},
	} {
		assert.Equal(t, http.StatusMethodNotAllowed, f.do(t, tc.method, tc.path, nil, adtBearer(alice)).Code, tc.method+" "+tc.path)
	}
	for _, path := range []string{
		adtGrantPath(f.agentA.ID, grant.ID) + "/x",
		adtCredentialPath(f.agentA.ID, grant.ID, cred.CredentialID) + "/x",
		adtGrantPath(f.agentA.ID, grant.ID) + "/credentials",
	} {
		assert.Equal(t, http.StatusNotFound, f.do(t, http.MethodGet, path, nil, adtBearer(alice)).Code, path)
	}
}

// =============================================================================
// Caps and purge
// =============================================================================

// TestAgentDelegationIssuance_GrantCaps: at most 10 active grants per agent
// and 50 per issuer; revoked and expired grants do not count.
func TestAgentDelegationIssuance_GrantCaps(t *testing.T) {
	f := newADTFixture(t, "adt-caps")
	ctx := context.Background()
	alice := f.session(t, f.alice)
	body := map[string]interface{}{"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "cap"}

	var first string
	for i := 0; i < agentDelegationMaxActiveGrantsPerAgent; i++ {
		g := f.grantFor(t, alice, f.agentA.ID)
		if i == 0 {
			first = g.ID
		}
	}
	adtAssertAPIError(t, f.issue(t, alice, f.agentA.ID, body), http.StatusConflict, errCodeGrantLimitReached)

	// An expired grant does not count; a revoked grant frees its place.
	f.storeGrant(t, f.agentA, time.Now().Add(-time.Minute))
	adtAssertAPIError(t, f.issue(t, alice, f.agentA.ID, body), http.StatusConflict, errCodeGrantLimitReached)
	require.Equal(t, http.StatusNoContent, f.revokeGrant(t, alice, f.agentA.ID, first).Code)
	f.grantFor(t, alice, f.agentA.ID)
	adtAssertAPIError(t, f.issue(t, alice, f.agentA.ID, body), http.StatusConflict, errCodeGrantLimitReached)
	assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationGrantCreate), agentDelegationMaxActiveGrantsPerAgent+1,
		"no refused issuance wrote a grant")

	// The per-issuer cap: fill Alice's remaining places with stored grants
	// on agent C (the store does not apply the caps), then issue on a
	// fresh agent of hers, which is under the per-agent cap.
	active, err := f.store.CountActiveAgentDelegationGrants(ctx, store.AgentDelegationGrantFilter{IssuerUserID: f.alice.ID}, time.Now())
	require.NoError(t, err)
	for i := active; i < agentDelegationMaxActiveGrantsPerIssuer-1; i++ {
		f.storeGrant(t, f.agentC, time.Now().Add(time.Hour))
	}
	fresh, _ := f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: "adt-caps-fresh"}), "adt-caps-fresh")
	f.grantFor(t, alice, fresh.ID)
	adtAssertAPIError(t, f.issue(t, alice, fresh.ID, body), http.StatusConflict, errCodeGrantLimitReached)

	// Another issuer is not affected by Alice's count.
	d, dave, _ := f.transferredAgent(t, "adt-caps")
	f.grantFor(t, f.session(t, dave), d.ID)
}

// TestAgentDelegationExchange_SupersedesBeyondTwoCredentials: a third
// exchange revokes the oldest active credential (superseded, with its own
// audit row in the same transaction); a failed supersede audit rolls the
// exchange back.
func TestAgentDelegationExchange_SupersedesBeyondTwoCredentials(t *testing.T) {
	f := newADTFixture(t, "adt-supersede")
	ctx := context.Background()
	grant := f.hubGrant(t)
	cred1 := f.delegated(t, grant.ID)
	cred2 := f.delegated(t, grant.ID)
	assert.Empty(t, adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke))

	// A failed supersede audit: nothing changes, nothing is issued.
	f.faults.inject(t, adtFaults{audit: errors.New("audit store down")})
	adtAssertAPIError(t, f.exchangeFor(t, f.agentA, grant.ID), http.StatusInternalServerError, errCodeAuditFailed)
	f.faults.clear()
	stored, err := f.store.GetAgentDelegatedCredential(ctx, cred1.CredentialID)
	require.NoError(t, err)
	assert.Nil(t, stored.RevokedAt)

	cred3 := f.delegated(t, grant.ID)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred1.Token, f.agentA.ID).Code, "the oldest is superseded")
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred2.Token, f.agentA.ID).Code)
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred3.Token, f.agentA.ID).Code)
	stored, err = f.store.GetAgentDelegatedCredential(ctx, cred1.CredentialID)
	require.NoError(t, err)
	assert.Equal(t, revokeReasonSuperseded, stored.RevokeReason)

	audits := adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke)
	require.Len(t, audits, 1)
	assert.Equal(t, cred1.CredentialID, audits[0].TargetID)
	assert.Equal(t, f.agentA.ID, audits[0].ActorPrincipalID)
	assert.Equal(t, actorKindAgentDelegated, audits[0].ActorKind)
	assert.Contains(t, audits[0].BeforeSummary, revokeReasonSuperseded)

	active, err := f.store.ListActiveAgentDelegatedCredentials(ctx, []string{grant.ID}, time.Now())
	require.NoError(t, err)
	assert.Len(t, active, agentDelegationMaxActiveCredentialsPerGrant)
}

// TestAgentDelegationExchange_GrantEndedUnderTheLock: a grant revoked, or
// expired, after exchange's checks but before its write is seen under the
// grant lock. The exchange answers 403 grant_inactive and writes no
// credential, no issue row and no supersede row.
func TestAgentDelegationExchange_GrantEndedUnderTheLock(t *testing.T) {
	f := newADTFixture(t, "adt-lock-recheck")
	ctx := context.Background()
	grant := f.hubGrant(t)
	cred1 := f.delegated(t, grant.ID)
	f.delegated(t, grant.ID) // a third exchange would supersede cred1

	unrevoked := func() int {
		creds, err := f.store.ListActiveAgentDelegatedCredentials(ctx, []string{grant.ID}, time.Time{})
		require.NoError(t, err)
		return len(creds)
	}
	assertNothingWritten := func(t *testing.T) {
		t.Helper()
		assert.Equal(t, 2, unrevoked(), "no credential written, none superseded")
		assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationCredentialIssue), 2, "no issue row")
		assert.Empty(t, adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke), "no supersede row")
		stored, err := f.store.GetAgentDelegatedCredential(ctx, cred1.CredentialID)
		require.NoError(t, err)
		assert.Nil(t, stored.RevokedAt)
	}

	t.Run("revoked inside the transaction", func(t *testing.T) {
		f.faults.inject(t, adtFaults{beforeGrantLock: func(ctx context.Context, tx store.Store, id string) error {
			_, err := tx.RevokeAgentDelegationGrant(ctx, id, "test", revokeReasonRevoked, "", time.Now())
			return err
		}})
		adtAssertAPIError(t, f.exchangeFor(t, f.agentA, grant.ID), http.StatusForbidden, errCodeGrantInactive)
		f.faults.clear()
		assertNothingWritten(t)
		g, err := f.store.GetAgentDelegationGrant(ctx, grant.ID)
		require.NoError(t, err)
		assert.Nil(t, g.RevokedAt, "the in-transaction revoke rolled back with the exchange")
	})

	t.Run("expired by the time of the lock", func(t *testing.T) {
		f.faults.inject(t, adtFaults{afterGrantLock: func(g *store.AgentDelegationGrant) {
			g.ExpiresAt = time.Now().Add(-time.Second)
		}})
		adtAssertAPIError(t, f.exchangeFor(t, f.agentA, grant.ID), http.StatusForbidden, errCodeGrantInactive)
		f.faults.clear()
		assertNothingWritten(t)
	})

	// Without the fault the same exchange succeeds and supersedes cred1.
	require.Equal(t, http.StatusOK, f.exchangeFor(t, f.agentA, grant.ID).Code)
	assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationCredentialRevoke), 1)
}

// TestAgentDelegatedCredentialPurge_InjectedClock: the purge job deletes
// credential rows whose expiry is more than 24 hours before its clock,
// revoked or not, and keeps younger rows and every grant.
func TestAgentDelegatedCredentialPurge_InjectedClock(t *testing.T) {
	f := newADTFixture(t, "adt-purge")
	ctx := context.Background()
	grant := f.storeGrant(t, f.agentA, time.Now().Add(time.Hour))
	now := time.Now()
	old := f.insertCredential(t, f.agentA, grant.ID, f.srv.agentDelegationAudience(), now.Add(-25*time.Hour))
	young := f.insertCredential(t, f.agentA, grant.ID, f.srv.agentDelegationAudience(), now.Add(-23*time.Hour))
	live := f.insertCredential(t, f.agentA, grant.ID, f.srv.agentDelegationAudience(), now.Add(30*time.Minute))
	oldRevoked := f.insertCredential(t, f.agentA, grant.ID, f.srv.agentDelegationAudience(), now.Add(-26*time.Hour))
	revokedRow, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(oldRevoked))
	require.NoError(t, err)
	_, err = f.store.RevokeAgentDelegatedCredential(ctx, revokedRow.ID, revokeReasonRevoked, now.Add(-27*time.Hour))
	require.NoError(t, err)

	f.srv.agentDelegatedCredentialPurgeHandler(func() time.Time { return now })(ctx)

	exists := func(token string) bool {
		_, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(token))
		if errors.Is(err, store.ErrNotFound) {
			return false
		}
		require.NoError(t, err)
		return true
	}
	assert.False(t, exists(old))
	assert.False(t, exists(oldRevoked))
	assert.True(t, exists(young))
	assert.True(t, exists(live))

	// Two days later the young row goes too; the live one has expired by
	// then as well. The grant is kept.
	f.srv.agentDelegatedCredentialPurgeHandler(func() time.Time { return now.Add(48 * time.Hour) })(ctx)
	assert.False(t, exists(young))
	assert.False(t, exists(live))
	_, err = f.store.GetAgentDelegationGrant(ctx, grant.ID)
	require.NoError(t, err)
}

// =============================================================================
// Seeded roles
// =============================================================================

// TestAgentDelegationManagementRoles_Seeded: hub-admin, project-owner and
// project-admin hold exactly the four management permissions; no other
// built-in role except super-admin holds any agent.delegation permission,
// and none but super-admin holds issuance or exchange.
func TestAgentDelegationManagementRoles_Seeded(t *testing.T) {
	management := []string{
		agentDelegationRevokePermission, agentDelegationCredentialRevokePermission,
		agentDelegationListPermission, agentDelegationReadPermission,
	}
	holders := map[string]bool{
		store.SystemRoleHubAdmin: true, store.ProjectRoleOwner: true, store.ProjectRoleAdmin: true,
	}
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleSuperAdmin {
			for _, id := range append(management, agentDelegationCreatePermission, agentDelegationExchangePermission) {
				assert.Contains(t, role.Permissions, id)
			}
			continue
		}
		var held []string
		for _, p := range role.Permissions {
			if strings.HasPrefix(p, "agent.delegation.") {
				held = append(held, p)
			}
		}
		if holders[role.Name] {
			assert.ElementsMatch(t, management, held, role.Name)
		} else {
			assert.Empty(t, held, role.Name)
		}
	}
	for _, id := range management {
		p, ok := registryPermission(id)
		require.True(t, ok, id)
		assert.Empty(t, p.UATScope, id)
		assert.Empty(t, p.AgentScopes, id)
	}
}
