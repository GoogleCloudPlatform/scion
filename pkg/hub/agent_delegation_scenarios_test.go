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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End-to-end agent delegation scenarios (.design/agent-delegation.md §17)
// through the real middleware, route guard and handlers, for the flows the
// per-slice tests leave below the HTTP layer or do not reach.

// adtHubReadBody is the body of a hub-bounded agent:read issuance.
func adtHubReadBody() map[string]interface{} {
	return map[string]interface{}{"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "g"}
}

// TestAgentDelegationScenario_IssuerWithoutProjectGrants (§17.2 flow 8):
// an issuer who owns the agent but holds only the hub-member role, or the
// hub-member role and an unrelated system permission, and no project
// binding, can neither issue nor exchange, and use of an earlier grant on a
// project target denies.
func TestAgentDelegationScenario_IssuerWithoutProjectGrants(t *testing.T) {
	f := newADTFixture(t, "adt-sc-member")
	ctx := context.Background()
	decisions := f.captureDecisions(t)
	ivy := hubMemberUser(t, f.store, "adt-sc-member-ivy")
	ivyP1 := adtGrantRole(t, f.store, ivy.ID, f.proj.ID, store.ProjectRoleMember)
	d, _ := f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: "adt-sc-member-d"}), "adt-sc-member-d")
	f.mutateAgent(t, d.ID, func(a *store.Agent) { a.OwnerID = ivy.ID })
	stored, err := f.store.GetAgent(ctx, d.ID)
	require.NoError(t, err)
	ivySession := f.session(t, ivy)

	grant := f.grantFor(t, ivySession, d.ID)
	cred := f.credentialFor(t, stored, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentC.ID).Code, "a P1 member's grant reads a P1 agent")
	require.NoError(t, f.store.DeleteRoleBinding(ctx, ivyP1))

	check := func(t *testing.T) {
		t.Helper()
		issued := len(adtAudits(t, f.store, mutationAgentDelegationGrantCreate))
		rec := f.issue(t, ivySession, d.ID, adtHubReadBody())
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationGrantCreate), issued, "no grant was written")

		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentC.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerProjectAccess)
		adtAssertAPIError(t, f.exchangeFor(t, stored, grant.ID), http.StatusForbidden, errCodeIssuerProjectAccess)
	}
	t.Run("only the hub-member role", check)

	role, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "adt-sc-unrelated", ScopeType: store.RoleScopeSystem, Permissions: []string{"hub.metrics.read"},
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: role.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: ivy.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	t.Run("an unrelated system permission as well", check)
}

// TestAgentDelegationScenario_FederatedIssuerDenies (§17.2 flow 5): a grant
// whose issuer row is a federated account denies at use and at exchange,
// although the account controls the agent.
func TestAgentDelegationScenario_FederatedIssuerDenies(t *testing.T) {
	f := newADTFixture(t, "adt-sc-fed")
	ctx := context.Background()
	decisions := f.captureDecisions(t)
	issuer, email := "https://hub-b.example.com", "fed@corp.example"
	fed := &store.User{
		DisplayName: "Federated issuer", Kind: store.UserKindFederated, Status: store.UserStatusActive,
		HomeIssuer: &issuer, FederatedEmail: &email,
	}
	require.NoError(t, f.store.CreateFederatedUser(ctx, fed))
	d, _ := f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: "adt-sc-fed-d"}), "adt-sc-fed-d")
	f.mutateAgent(t, d.ID, func(a *store.Agent) { a.OwnerID = fed.ID })
	stored, err := f.store.GetAgent(ctx, d.ID)
	require.NoError(t, err)

	grant := f.storeGrantWith(t, stored, func(g *store.AgentDelegationGrant) { g.IssuerUserID = fed.ID })
	token := f.insertCredential(t, stored, grant.ID, f.srv.agentDelegationAudience(), time.Now().Add(30*time.Minute))

	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, token, d.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerTestIdentity)
	adtAssertAPIError(t, f.exchangeFor(t, stored, grant.ID), http.StatusForbidden, errCodeIssuerInvalid)
}

// TestAgentDelegationScenario_SuperAdminAcrossProjectsUntilDemoted (§17.1
// flow 5, §17.2 flow 5): a super-admin's hub agent:read grant reads an
// agent in a project where the super-admin holds no role. Once the
// super-admin binding is removed, the next use on that target denies,
// while a target in the issuer's own project stays readable.
func TestAgentDelegationScenario_SuperAdminAcrossProjectsUntilDemoted(t *testing.T) {
	f := newADTFixture(t, "adt-sc-sa")
	ctx := context.Background()
	decisions := f.captureDecisions(t)
	admin := hubAdminUser(t, f.bypassAgentsFixture)
	adtGrantRole(t, f.store, admin.ID, f.proj.ID, store.ProjectRoleMember)
	adminAgent, _ := f.createdAgent(t, f.create(t, authUser(admin), CreateAgentRequest{Name: "adt-sc-sa-s"}), "adt-sc-sa-s")
	grant := f.grantFor(t, f.session(t, admin), adminAgent.ID)
	cred := f.credentialFor(t, adminAgent, grant.ID)

	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code, "the super-admin holds no role in P2")

	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, admin.ID)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.NotZero(t, removed, "the super-admin binding was found")

	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerProjectAccess)
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentC.ID).Code, "a P1 target is still readable")
}

// TestAgentDelegationScenario_ExchangeStatusLookupFaultIs503 (§17.2 flow
// 3): when the agent token middleware cannot read the agent credential's
// status, exchange answers 503 unavailable and issues nothing.
func TestAgentDelegationScenario_ExchangeStatusLookupFaultIs503(t *testing.T) {
	f := newADTFixture(t, "adt-sc-503")
	grant := f.hubGrant(t)
	token := f.agentJWT(t, f.agentA)
	issued := len(adtAudits(t, f.store, mutationAgentDelegationCredentialIssue))

	fault := &storeFaultSwitch{}
	fault.Arm()
	prev := f.srv.authConfig.CredentialStore
	f.srv.authConfig.CredentialStore = &erroringCredentialStore{Store: f.store, err: errors.New("credential store down"), fault: fault}
	restore := func() { f.srv.authConfig.CredentialStore = prev }
	t.Cleanup(restore)

	rec := f.exchange(t, token, f.agentA.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	adtAssertAPIError(t, rec, http.StatusServiceUnavailable, ErrCodeUnavailable)
	assert.NotContains(t, rec.Body.String(), delegatedCredentialPrefix)
	assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationCredentialIssue), issued, "no credential was issued")

	restore()
	rec = f.exchange(t, token, f.agentA.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestAgentDelegationScenario_UseAfterExchangeAgentCredentialExpiry
// (§17.2 flow 34): a delegated credential whose exchange agent credential
// has expired denies at use, even before its own expiry.
func TestAgentDelegationScenario_UseAfterExchangeAgentCredentialExpiry(t *testing.T) {
	f := newADTFixture(t, "adt-sc-acexp")
	ctx := context.Background()
	decisions := f.captureDecisions(t)
	grant := f.hubGrant(t)
	stored, err := f.store.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)
	tokenGrant, err := f.srv.AuthorizeAgentToken(ctx, stored)
	require.NoError(t, err)
	_, ac, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
	require.NoError(t, err)
	ac.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, f.store.CreateAgentCredential(ctx, ac))

	token, hash, err := newDelegatedCredentialToken()
	require.NoError(t, err)
	require.NoError(t, f.store.CreateAgentDelegatedCredential(ctx, &store.AgentDelegatedCredential{
		GrantID: grant.ID, AgentID: f.agentA.ID, KeyHash: hash, Prefix: delegatedCredentialPrefix,
		Audience: f.srv.agentDelegationAudience(), CeilingPermissionIDs: []string{"agent.read"},
		ExchangeAgentCredentialID: ac.ID, ExpiresAt: time.Now().Add(30 * time.Minute),
	}))

	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodeAgentCredentialInvalid)
}

// TestAgentDelegationScenario_RefreshThenReexchange (§17.1 flow 4, §17.2
// flow 24): after a real agent token refresh, credentials exchanged with
// the old agent token deny at their next use, the old token no longer
// exchanges, and a re-exchange with the refreshed token succeeds.
func TestAgentDelegationScenario_RefreshThenReexchange(t *testing.T) {
	f := newADTFixture(t, "adt-sc-refresh")
	decisions := f.captureDecisions(t)
	grant := f.hubGrant(t)
	aud := map[string]interface{}{"audience": f.srv.agentDelegationAudience()}
	oldToken := f.agentJWT(t, f.agentA)

	rec := f.exchange(t, oldToken, f.agentA.ID, grant.ID, aud)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var before ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &before))
	require.Equal(t, http.StatusOK, f.getAgent(t, before.Token, f.agentB.ID).Code)

	rec = f.do(t, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/token/refresh", map[string]interface{}{}, adtBearer(oldToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var refreshed struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refreshed))
	require.NotEmpty(t, refreshed.Token)

	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, before.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodeAgentCredentialInvalid)
	adtAssertAPIError(t, f.exchange(t, oldToken, f.agentA.ID, grant.ID, aud), http.StatusUnauthorized, agentTokenRefusedCode)

	rec = f.exchange(t, refreshed.Token, f.agentA.ID, grant.ID, aud)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var after ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &after))
	assert.Equal(t, http.StatusOK, f.getAgent(t, after.Token, f.agentB.ID).Code)
}

// TestAgentDelegationScenario_AttachGrantStillRedactsEnvironment (§17.2
// flow 29): with a grant that includes agent:attach, issued by the owner
// who sees the agent's environment unredacted, the delegated read of that
// agent is redacted and its capabilities hold only the read action.
func TestAgentDelegationScenario_AttachGrantStillRedactsEnvironment(t *testing.T) {
	f := newADTFixture(t, "adt-sc-env")
	f.mutateAgent(t, f.agentA.ID, func(a *store.Agent) {
		if a.AppliedConfig == nil {
			a.AppliedConfig = &store.AgentAppliedConfig{}
		}
		a.AppliedConfig.Env = map[string]string{"DELEGATION_ENV_PROBE_A": "env-probe-a-value"}
	})
	rec := f.getAgent(t, f.session(t, f.alice), f.agentA.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "env-probe-a-value", "the issuer sees the environment unredacted")

	grant := f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:read", "agent:attach")
	cred := f.delegated(t, grant.ID)
	rec = f.getAgent(t, cred.Token, f.agentA.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "env-probe-a-value", "a delegated response never carries the environment")
	var resp struct {
		Cap *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Cap)
	assert.Equal(t, []string{string(ActionRead)}, resp.Cap.Actions, "capabilities never include attach")
}

// TestAgentDelegationScenario_ActorLabelsOnAccessTokensRefused (§17.2 flow
// 17): an access token mint that sets an actor-shaped label key, such as
// actor=agent:<id>, is refused as a reserved key; no token is minted.
func TestAgentDelegationScenario_ActorLabelsOnAccessTokensRefused(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("adt-sc-label-p")
	rs4Project(t, s, projectID, tid("adt-sc-label-o"))
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)
	for _, key := range []string{"actor", "verified_actor", "actor_agent_id", "authorizing_user_id", "source_grant_id"} {
		t.Run(key, func(t *testing.T) {
			code, resp := e1MintTokenViaAPI(t, srv, map[string]interface{}{
				"name": "actor-label", "projectId": projectID, "scopes": []string{"project:read"},
				"labels": map[string]string{key: "agent:" + tid("adt-sc-label-a")},
			})
			assert.Equal(t, http.StatusBadRequest, code, "%+v", resp)
			assert.Nil(t, resp["token"])
		})
	}
}

// TestAgentDelegationScenario_NoDelegationEdgeAfterIssuance (§17.2 flow
// 16): issuance and exchange write no delegation edge, so the agent's
// edges and the issuer's edges are unchanged, and the agent's own agent
// token gains nothing.
func TestAgentDelegationScenario_NoDelegationEdgeAfterIssuance(t *testing.T) {
	f := newADTFixture(t, "adt-sc-edge")
	ctx := context.Background()
	edgeIDs := func(edges []*store.DelegationEdge, err error) []string {
		t.Helper()
		require.NoError(t, err)
		ids := make([]string, 0, len(edges))
		for _, e := range edges {
			ids = append(ids, e.ID)
		}
		return ids
	}
	agentBefore := edgeIDs(f.store.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.agentA.ID))
	issuerBefore := edgeIDs(f.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, f.alice.ID))
	plainBefore := f.getAgent(t, f.agentJWT(t, f.agentA), f.agentB.ID).Code

	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)

	assert.ElementsMatch(t, agentBefore, edgeIDs(f.store.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.agentA.ID)))
	assert.ElementsMatch(t, issuerBefore, edgeIDs(f.store.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, f.alice.ID)))
	plainAfter := f.getAgent(t, f.agentJWT(t, f.agentA), f.agentB.ID).Code
	assert.Equal(t, plainBefore, plainAfter, "the agent's own token answers as before")
	assert.NotEqual(t, http.StatusOK, plainAfter, "the agent's own token does not read an agent in another project")
}

// TestAgentDelegationScenario_AccessTokenUnchangedByExperiment (§17.1 flow
// 6): the issuer's own user access token answers the same with
// hub.agent_delegation off and on.
func TestAgentDelegationScenario_AccessTokenUnchangedByExperiment(t *testing.T) {
	f := newADTFixture(t, "adt-sc-uat")
	rec := f.do(t, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
		"name": "sc-uat", "projectId": f.other.ID, "scopes": []string{"agent:read"},
	}, adtBearer(f.session(t, f.alice)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var minted struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &minted))
	require.NotEmpty(t, minted.Token)

	codes := func() (int, int) {
		return f.getAgent(t, minted.Token, f.agentB.ID).Code, f.getAgent(t, minted.Token, f.agentC.ID).Code
	}
	onB, onC := codes()
	assert.Equal(t, http.StatusOK, onB, "inside the token's project")
	assert.NotEqual(t, http.StatusOK, onC, "outside the token's project")
	adtSetExperiment(t, f.srv, false)
	offB, offC := codes()
	assert.Equal(t, onB, offB)
	assert.Equal(t, onC, offC)
}

// TestAgentDelegationScenario_MaintenanceModeOnAdmittedRoutes (§17.2 flow
// 38): with maintenance mode on, a delegated credential gets 503 on the
// admitted read routes through the full handler chain, and is served again
// once it is off.
func TestAgentDelegationScenario_MaintenanceModeOnAdmittedRoutes(t *testing.T) {
	f := newADTFixture(t, "adt-sc-maint")
	grant := f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:read", "project:read")
	cred := f.delegated(t, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)

	f.srv.maintenance.SetEnabled(true)
	t.Cleanup(func() { f.srv.maintenance.SetEnabled(false) })
	assert.Equal(t, http.StatusServiceUnavailable, f.getAgent(t, cred.Token, f.agentB.ID).Code)
	assert.Equal(t, http.StatusServiceUnavailable, f.getProject(t, cred.Token, f.other.ID).Code)

	f.srv.maintenance.SetEnabled(false)
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
	assert.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)
}

// TestAgentDelegationScenario_UnknownCeilingVersionDenies (§17.2 flow 22):
// the store refuses a grant at ceiling version 0, and a grant row at an
// unknown ceiling version denies at exchange and at use.
func TestAgentDelegationScenario_UnknownCeilingVersionDenies(t *testing.T) {
	f := newADTFixture(t, "adt-sc-ceilver")
	ctx := context.Background()
	decisions := f.captureDecisions(t)
	stored, err := f.store.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)

	zero := &store.AgentDelegationGrant{
		AgentID: stored.ID, AgentProjectID: stored.ProjectID, IssuerUserID: f.alice.ID, BoundaryKind: "hub",
		CeilingVersion: 0, CeilingPermissionIDs: []string{"agent.read"}, Name: "v0",
		MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL, ExpiresAt: time.Now().Add(time.Hour),
		IssuanceAuditID: "00000000-0000-0000-0000-000000000001",
	}
	assert.ErrorIs(t, f.store.CreateAgentDelegationGrant(ctx, zero), store.ErrInvalidInput)

	grant := f.storeGrantWith(t, stored, func(g *store.AgentDelegationGrant) { g.CeilingVersion = 2 })
	adtAssertAPIError(t, f.exchangeFor(t, stored, grant.ID), http.StatusForbidden, errCodeGrantInactive)
	token := f.insertCredential(t, stored, grant.ID, f.srv.agentDelegationAudience(), time.Now().Add(30*time.Minute))
	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodeGrantInactive)
}
