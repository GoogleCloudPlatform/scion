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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtIssueGrant issues Alice's grant for agent A with boundary and
// selectors and returns it.
func (f *adtFixture) adtIssueGrant(t *testing.T, boundary map[string]string, selectors ...string) AgentDelegationGrantResponse {
	t.Helper()
	rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
		"boundary": boundary, "permissions": selectors, "name": "use",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	return grant
}

func (f *adtFixture) getProject(t *testing.T, token, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodGet, "/api/v1/projects/"+projectID, nil, adtBearer(token))
}

// adtSetPolicyNarrowing gives the server DB-backed operational settings
// whose agent_delegation_policy section holds doc (absent when doc is
// empty), for the rest of the test.
func adtSetPolicyNarrowing(t *testing.T, srv *Server, doc string) {
	t.Helper()
	fake := newFakeHubSettingStore()
	if doc != "" {
		fake.seed(opsettings.AgentDelegationPolicySectionName, json.RawMessage(doc))
	}
	ops := NewOperationalSettings(fake, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	prev := srv.GetOperationalSettings()
	srv.SetOperationalSettings(ops)
	t.Cleanup(func() { srv.SetOperationalSettings(prev) })
	require.True(t, srv.experimentEnabled(experiments.AgentDelegation), "the settings swap must not turn the experiment off")
}

// TestAgentDelegatedProjectRead_HubGrantAcrossProjects: with a hub grant
// for project:read, agent A reads P2, where Alice is a member. Capabilities
// hold only the route's primary action. The agent's own JWT still cannot
// read P2.
func TestAgentDelegatedProjectRead_HubGrantAcrossProjects(t *testing.T) {
	f := newADTFixture(t, "adt-pread")
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read").ID)

	rec := f.getProject(t, cred.Token, f.other.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		ID  string        `json:"id"`
		Cap *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, f.other.ID, body.ID)
	require.NotNil(t, body.Cap)
	assert.Equal(t, []string{string(ActionRead)}, body.Cap.Actions, "capabilities are the primary action only")

	// The {uuid}__{slug} form resolves to the same project.
	rec = f.getProject(t, cred.Token, f.other.ID+"__"+f.other.Slug)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The same grant does not read agents: agent.read is not in it.
	adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
}

// TestAgentDelegatedProjectRead_Denials covers the §17.2 flows for the
// project read route: a project-bounded grant on another project, a grant
// without project:read, the issuer losing access, a missing project, and
// the response convention (the handler's own 404, no reason text).
func TestAgentDelegatedProjectRead_Denials(t *testing.T) {
	f := newADTFixture(t, "adt-pdeny")
	ctx := context.Background()
	decisions := f.captureDecisions(t)

	t.Run("project-bounded grant on another project", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "project:read").ID)
		assert.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.proj.ID).Code, "the boundary project is readable")
		decisions.reset()
		rec := f.getProject(t, cred.Token, f.other.ID)
		adtAssertAPIError(t, rec, http.StatusNotFound, ErrCodeNotFound)
		assert.NotContains(t, rec.Body.String(), "agent delegation")
		decisions.assertDelegatedDeny(t, agentDelegationCodeOutsideBoundary)
	})
	t.Run("permission outside the ceiling", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID) // agent:read only
		decisions.reset()
		adtAssertAPIError(t, f.getProject(t, cred.Token, f.other.ID), http.StatusNotFound, ErrCodeNotFound)
		decisions.assertDelegatedDeny(t, agentDelegationCodeOutsideCeiling)
	})
	t.Run("missing project", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read").ID)
		adtAssertAPIError(t, f.getProject(t, cred.Token, tid("adt-pdeny-missing")), http.StatusNotFound, ErrCodeNotFound)
	})
	t.Run("issuer loses access to the target project", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read").ID)
		require.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)
		require.NoError(t, f.store.DeleteRoleBinding(ctx, f.aliceP2Binding))
		decisions.reset()
		adtAssertAPIError(t, f.getProject(t, cred.Token, f.other.ID), http.StatusNotFound, ErrCodeNotFound)
		decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerProjectAccess)
		// The agent's own project is still readable; revocation takes
		// effect at the next use, with no cache in between.
		assert.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.proj.ID).Code)
	})
}

// TestAgentDelegatedProjectRead_OneRecordPerCheckAndNoReason: the delegated
// project.read allow is recorded once even with allow sampling at 0, and
// names the grant; a deny carries denied_by agent_delegation.
func TestAgentDelegatedProjectRead_OneRecordPerCheckAndNoReason(t *testing.T) {
	f := newADTFixture(t, "adt-precords")
	decisions := f.captureDecisions(t)
	f.srv.authzService.DecisionAuditSampleRate = 0
	t.Cleanup(func() { f.srv.authzService.DecisionAuditSampleRate = 1.0 })
	grant := f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read")
	cred := f.delegated(t, grant.ID)

	decisions.reset()
	rec := f.getProject(t, cred.Token, f.other.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var allows []*store.DecisionAuditRecord
	for _, r := range decisions.delegated() {
		if r.Result == "allow" {
			allows = append(allows, r)
		}
	}
	require.Len(t, allows, 1, "one record for the delegated project.read check")
	assert.Equal(t, "agent_delegation:"+grant.ID, allows[0].MatchedGrant)
	assert.Equal(t, f.agentA.ID, allows[0].PrincipalID, "the actor is the agent")
	assert.NotContains(t, rec.Body.String(), "agent delegation")
}

// TestAgentDelegatedProjectRead_AttributionOnTheDecision: the decision for
// a delegated project read carries the agent as actor, Alice as authorizing
// user, the grant and the exchange agent credential, and is AlwaysAudit.
func TestAgentDelegatedProjectRead_AttributionOnTheDecision(t *testing.T) {
	f := newADTFixture(t, "adt-pattr")
	ctx := context.Background()
	grant := f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read")
	cred := f.delegated(t, grant.ID)
	st := adtLoadedState(t, f, cred.Token)
	entry, ok := agentDelegationAdmissionFor(http.MethodGet, projectResourceRouteID)
	require.True(t, ok)
	dctx := withStandingMemo(contextWithDelegatedAdmission(contextWithDelegatedState(ctx, st), entry))

	d := f.srv.authzService.Decide(dctx, AuthzRequest{Principal: PrincipalContext{Identity: st.identity}, Resource: projectResource(f.other), Action: ActionRead})
	require.True(t, d.Allowed, d.Reason)
	assert.True(t, d.AlwaysAudit)
	require.NotNil(t, d.AgentDelegation)
	assert.Equal(t, actorKindAgentDelegated, d.AgentDelegation.ActorKind)
	assert.Equal(t, f.agentA.ID, d.AgentDelegation.ActorAgentID)
	assert.Equal(t, f.alice.ID, d.AgentDelegation.AuthorizingUserID)
	assert.Equal(t, grant.ID, d.AgentDelegation.SourceGrantID)
	assert.Equal(t, st.credential.ExchangeAgentCredentialID, d.AgentDelegation.ExchangeAgentCredentialID)
	assert.Empty(t, d.AgentDelegation.AgentDelegationCode)

	// The agent.read entry does not admit a project.read check: the
	// permission is not that operation's own and not a listed secondary.
	agentEntry, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRouteRoot))
	require.True(t, ok)
	actx := withStandingMemo(contextWithDelegatedAdmission(contextWithDelegatedState(ctx, st), agentEntry))
	d = f.srv.authzService.Decide(actx, AuthzRequest{Principal: PrincipalContext{Identity: st.identity}, Resource: projectResource(f.other), Action: ActionRead})
	assert.False(t, d.Allowed)
	require.NotNil(t, d.AgentDelegation)
	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, d.AgentDelegation.AgentDelegationCode)
	assert.Equal(t, DeniedByAgentDelegation, d.DeniedBy)
	assert.True(t, d.AlwaysAudit)

	// An entry that is not the table's own for its route never admits.
	forged := agentDelegationAdmission{Method: http.MethodGet, RouteID: projectResourceRouteID, Operation: opAgentRead}
	fctx := withStandingMemo(contextWithDelegatedAdmission(contextWithDelegatedState(ctx, st), forged))
	d = f.srv.authzService.Decide(fctx, AuthzRequest{Principal: PrincipalContext{Identity: st.identity}, Resource: agentResource(f.agentB), Action: ActionRead})
	assert.False(t, d.Allowed)
	require.NotNil(t, d.AgentDelegation)
	assert.Equal(t, agentDelegationCodeCredentialNotAdmitted, d.AgentDelegation.AgentDelegationCode)
}

// adtLoadedState loads the request state the middleware builds for token.
func adtLoadedState(t *testing.T, f *adtFixture, token string) *delegatedRequestState {
	t.Helper()
	ctx := context.Background()
	row, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(token))
	require.NoError(t, err)
	grant, err := f.store.GetAgentDelegationGrant(ctx, row.GrantID)
	require.NoError(t, err)
	issuer, err := f.store.GetUser(ctx, grant.IssuerUserID)
	require.NoError(t, err)
	agent, err := f.store.GetAgent(ctx, grant.AgentID)
	require.NoError(t, err)
	ac, err := f.store.GetAgentCredentialByID(ctx, row.ExchangeAgentCredentialID)
	require.NoError(t, err)
	return &delegatedRequestState{
		identity: newDelegatedAgentIdentity(row, grant), credential: row, grant: grant,
		issuer: issuer, agent: agent, exchangeCred: ac, issuerPC: issuerPrincipal(issuer), memo: &ProjectAdmissionCache{},
	}
}

// TestAgentDelegatedProjectRead_OrdinaryAgentTokenGainsNothing: agent A's
// own JWT reads no other project, before or after Alice issues and A
// exchanges a hub grant for project:read; only the delegated credential
// reads it.
func TestAgentDelegatedProjectRead_OrdinaryAgentTokenGainsNothing(t *testing.T) {
	f := newADTFixture(t, "adt-pgolden")
	agentToken := f.agentJWT(t, f.agentA)
	read := func(projectID string) int {
		return f.do(t, http.MethodGet, "/api/v1/projects/"+projectID, nil, map[string]string{"X-Scion-Agent-Token": agentToken}).Code
	}
	beforeOther, beforeOwn := read(f.other.ID), read(f.proj.ID)
	require.NotEqual(t, http.StatusOK, beforeOther, "the agent JWT cannot read another project")

	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read").ID)
	assert.Equal(t, beforeOther, read(f.other.ID), "the agent JWT gains nothing from the grant")
	assert.Equal(t, beforeOwn, read(f.proj.ID))
	assert.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)

	// Both headers: the valid agent token wins and the bearer adds nothing.
	rec := f.do(t, http.MethodGet, "/api/v1/projects/"+f.other.ID, nil, map[string]string{
		"X-Scion-Agent-Token": agentToken, "Authorization": "Bearer " + cred.Token,
	})
	assert.Equal(t, beforeOther, rec.Code, rec.Body.String())
}

// TestAgentDelegatedProjectRead_ExperimentAndRevocation: the route refuses
// the credential while the experiment is off and serves it again when it
// is back on; a revoked credential is refused at the next use.
func TestAgentDelegatedProjectRead_ExperimentAndRevocation(t *testing.T) {
	f := newADTFixture(t, "adt-pexp")
	ctx := context.Background()
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read").ID)

	adtSetExperiment(t, f.srv, false)
	assert.Equal(t, http.StatusUnauthorized, f.getProject(t, cred.Token, f.other.ID).Code)
	adtSetExperiment(t, f.srv, true)
	require.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)

	row, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
	require.NoError(t, err)
	_, err = f.store.RevokeAgentDelegatedCredential(ctx, row.ID, "test", time.Now())
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, f.getProject(t, cred.Token, f.other.ID).Code)
}

// TestAgentDelegationPolicyNarrowing_DeniesAtIssuanceExchangeAndUse: the
// agent_delegation_policy setting removes a permission for one boundary
// kind; issuance and exchange then refuse it and an existing credential
// is denied at its next use. The other boundary kind is unaffected. A
// setting the hub cannot read denies.
func TestAgentDelegationPolicyNarrowing_DeniesAtIssuanceExchangeAndUse(t *testing.T) {
	f := newADTFixture(t, "adt-narrow")
	decisions := f.captureDecisions(t)
	hubGrant := f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "project:read")
	cred := f.delegated(t, hubGrant.ID)
	projectCred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "project:read").ID)
	require.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)

	adtSetPolicyNarrowing(t, f.srv, `{"remove":{"hub":["project.read"]}}`)

	t.Run("use", func(t *testing.T) {
		decisions.reset()
		adtAssertAPIError(t, f.getProject(t, cred.Token, f.other.ID), http.StatusNotFound, ErrCodeNotFound)
		decisions.assertDelegatedDeny(t, agentDelegationCodePermissionNotDelegable)
		assert.Equal(t, http.StatusOK, f.getProject(t, projectCred.Token, f.proj.ID).Code, "project-bounded grants are not narrowed")
	})
	t.Run("exchange", func(t *testing.T) {
		rec := f.exchange(t, f.agentJWT(t, f.agentA), f.agentA.ID, hubGrant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
		adtAssertAPIError(t, rec, http.StatusForbidden, errCodePermissionNotDelegable)
	})
	t.Run("issuance", func(t *testing.T) {
		rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
			"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"project:read"}, "name": "n",
		})
		adtAssertAPIError(t, rec, http.StatusForbidden, errCodePermissionNotDelegable)
	})
	t.Run("a narrowing can never add a permission", func(t *testing.T) {
		adtSetPolicyNarrowing(t, f.srv, `{"remove":{"hub":["agent.delete"]}}`)
		assert.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)
		_, delegable := permissions.AgentDelegableRegistry["agent.delete"]
		assert.False(t, delegable)
	})
	t.Run("unreadable setting denies", func(t *testing.T) {
		adtSetPolicyNarrowing(t, f.srv, `not valid json`)
		decisions.reset()
		adtAssertAPIError(t, f.getProject(t, projectCred.Token, f.proj.ID), http.StatusNotFound, ErrCodeNotFound)
		decisions.assertDelegatedDeny(t, agentDelegationCodeLookupError)
		rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
			"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "n",
		})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		rec = f.exchange(t, f.agentJWT(t, f.agentA), f.agentA.ID, hubGrant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
		assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	})
	t.Run("removing the setting restores use", func(t *testing.T) {
		adtSetPolicyNarrowing(t, f.srv, "")
		assert.Equal(t, http.StatusOK, f.getProject(t, cred.Token, f.other.ID).Code)
	})
}

// TestAgentDelegablePermissions_EachExercisedThroughItsRoute: every key of
// the hub agent-delegation policy is allowed end to end, through the real
// middleware, route guard and handler, with a grant that holds only that
// permission. A new key fails here until it has a case.
func TestAgentDelegablePermissions_EachExercisedThroughItsRoute(t *testing.T) {
	f := newADTFixture(t, "adt-keys")
	f.adtMessageWorld(t)
	attachTarget, _ := f.adtAttachTarget(t, "adt-keys")
	decisions := f.captureDecisions(t)
	cases := map[string]struct {
		selector string
		request  func(token string) *httptest.ResponseRecorder
	}{
		"agent.read": {"agent:read", func(token string) *httptest.ResponseRecorder { return f.getAgent(t, token, f.agentB.ID) }},
		"project.read": {"project:read", func(token string) *httptest.ResponseRecorder {
			return f.getProject(t, token, f.other.ID)
		}},
		"agent.message": {"agent:message", func(token string) *httptest.ResponseRecorder {
			return f.sendMessage(t, token, agentMessagePath(f.agentB.ID), adtText("key check"))
		}},
		"agent.attach": {"agent:attach", func(token string) *httptest.ResponseRecorder {
			return f.do(t, http.MethodGet, adtPTYPath(attachTarget.ID), nil, adtBearer(token))
		}},
		"agent.list": {"agent:list", func(token string) *httptest.ResponseRecorder {
			return f.do(t, http.MethodGet, "/api/v1/agents", nil, adtBearer(token))
		}},
	}
	for perm := range permissions.AgentDelegableRegistry {
		tc, ok := cases[perm]
		if !assert.True(t, ok, "delegable permission %s has no end-to-end case", perm) {
			continue
		}
		t.Run(perm, func(t *testing.T) {
			cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, tc.selector).ID)
			decisions.reset()
			rec := tc.request(cred.Token)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			if perm == "agent.list" {
				// A list answers 200 whether the arm allows or refuses, so the arm's record must be the allow,
				// and the item filter (agent:read is not in the grant) refuses every item.
				var primary, agg []*store.DecisionAuditRecord
				for _, r := range decisions.delegated() {
					if r.RecordKind == store.DecisionAuditRecordKindListFilter {
						agg = append(agg, r)
					} else {
						primary = append(primary, r)
					}
				}
				require.Len(t, primary, 1)
				assert.Equal(t, "allow", primary[0].Result, "the list arm allows a grant holding agent:list")
				assert.Equal(t, "agent.list", primary[0].PermissionID)
				require.Len(t, agg, 1)
				assert.Positive(t, agg[0].ListFilter.Denied, "without agent:read every item is refused")
				assert.Zero(t, agg[0].ListFilter.Allowed)
			}
		})
	}
}
