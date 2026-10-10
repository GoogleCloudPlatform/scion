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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setAgentDelegationExperiment turns hub.agent_delegation on or off on srv
// by replacing its registry; every other experiment keeps its registered
// default.
func setAgentDelegationExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.AgentDelegation {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.AgentDelegation))
}

// delegationFixture is the agent delegation world: Alice, a member of P1
// and P2, owns agent A in P1 (created through the handler, so it has a
// recorded chain). Agent B lives in P2 and is owned by Bob, a member of P2
// only. Agent C in P1 is owned by Carol, a member of P1 only.
type delegationFixture struct {
	*uatCreateFixture
	alice, bob, carol *store.User
	aliceP2Binding    string
	agentA            *store.Agent
	agentB            *store.Agent
	agentC            *store.Agent
}

func newDelegationFixture(t *testing.T, name string) *delegationFixture {
	t.Helper()
	ctx := context.Background()
	f := &delegationFixture{uatCreateFixture: newUATCreateFixture(t, name)}
	setAgentDelegationExperiment(t, f.srv, true)
	f.alice = f.creator
	f.aliceP2Binding = delegationGrantRole(t, f.store, f.alice.ID, f.other.ID, store.ProjectRoleMember)

	f.bob = hubMemberUser(t, f.store, name+"-bob")
	delegationGrantRole(t, f.store, f.bob.ID, f.other.ID, store.ProjectRoleMember)
	f.carol = hubMemberUser(t, f.store, name+"-carol")
	delegationGrantRole(t, f.store, f.carol.ID, f.proj.ID, store.ProjectRoleMember)

	f.agentA, _ = f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: name + "-a"}), name+"-a")

	f.agentB = &store.Agent{
		ID: tid(name + "-agent-b"), Name: name + "-b", Slug: name + "-b", ProjectID: f.other.ID,
		OwnerID: f.bob.ID, Ancestry: []string{f.bob.ID},
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"DELEGATION_ENV_PROBE": "env-probe-value"}},
	}
	require.NoError(t, f.store.CreateAgent(ctx, f.agentB))
	f.agentC = &store.Agent{
		ID: tid(name + "-agent-c"), Name: name + "-c", Slug: name + "-c", ProjectID: f.proj.ID,
		OwnerID: f.carol.ID, Ancestry: []string{f.carol.ID},
	}
	require.NoError(t, f.store.CreateAgent(ctx, f.agentC))
	return f
}

// delegationGrantRole binds userID to roleName in projectID and returns the
// binding ID.
func delegationGrantRole(t *testing.T, s store.Store, userID, projectID, roleName string) string {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return rb.ID
}

// session returns a session JWT for u.
func (f *delegationFixture) session(t *testing.T, u *store.User) string {
	t.Helper()
	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	return token
}

// agentJWT returns a production agent token for a, with its credential row.
func (f *delegationFixture) agentJWT(t *testing.T, a *store.Agent) string {
	t.Helper()
	stored, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	token, err := f.srv.issueAgentTokenForTest(context.Background(), stored)
	require.NoError(t, err)
	return token
}

// do sends a request through the full handler chain (middleware, route
// guard, dispatch). headers are set as given.
func (f *delegationFixture) do(t *testing.T, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func (f *delegationFixture) issue(t *testing.T, token, agentID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPost, "/api/v1/agents/"+agentID+"/delegations", body, bearer(token))
}

func (f *delegationFixture) exchange(t *testing.T, agentToken, agentID, grantID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPost, "/api/v1/agents/"+agentID+"/delegations/"+grantID+"/exchange", body, bearer(agentToken))
}

// hubGrant issues Alice's hub-bounded agent:read grant for agent A.
func (f *delegationFixture) hubGrant(t *testing.T) AgentDelegationGrantResponse {
	t.Helper()
	rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "hub-read",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	return grant
}

// delegated exchanges grantID with A's agent token and returns the
// delegated credential.
func (f *delegationFixture) delegated(t *testing.T, grantID string) ExchangeAgentDelegationResponse {
	t.Helper()
	rec := f.exchange(t, f.agentJWT(t, f.agentA), f.agentA.ID, grantID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (f *delegationFixture) getAgent(t *testing.T, token, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodGet, "/api/v1/agents/"+agentID, nil, bearer(token))
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) APIError {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	apiErr := decodeTargetAPIError(t, rec)
	assert.Equal(t, code, apiErr.Code, rec.Body.String())
	return apiErr
}

func delegationAudits(t *testing.T, s store.Store, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	return recs
}

// =============================================================================
// Issuance
// =============================================================================

func TestAgentDelegationIssuance_IssuesGrantWithAudit(t *testing.T) {
	f := newDelegationFixture(t, "adt-issue")
	before := time.Now()
	rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"},
		"name": "nightly-report", "purpose": "reporting", "labels": map[string]string{"team": "reports"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), delegatedCredentialPrefix, "issuance returns no credential")

	var resp AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, f.agentA.ID, resp.AgentID)
	assert.Equal(t, f.alice.ID, resp.IssuerUserID)
	assert.Equal(t, "hub", resp.Boundary.Kind)
	assert.Equal(t, []string{"agent.read"}, resp.Permissions)
	assert.Equal(t, 1, resp.CeilingVersion)
	assert.Equal(t, agentDelegationCredentialDefaultTTL, resp.MaxCredentialTTLSeconds)
	assert.Equal(t, "active", resp.Status)
	// An omitted expiry defaults to seven days.
	assert.WithinDuration(t, before.Add(agentDelegationGrantDefaultLifetime), resp.ExpiresAt, time.Minute)

	grant, err := f.store.GetAgentDelegationGrant(context.Background(), resp.ID)
	require.NoError(t, err)
	assert.Equal(t, f.agentA.ProjectID, grant.AgentProjectID)
	assert.Equal(t, f.agentA.Generation, grant.AgentGeneration)
	assert.False(t, grant.AllowSubdelegation)
	assert.Empty(t, grant.ParentGrantID)
	assert.Zero(t, grant.Depth)
	assert.Equal(t, map[string]string{"team": "reports"}, grant.Labels)

	audits := delegationAudits(t, f.store, mutationAgentDelegationGrantCreate)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, grant.IssuanceAuditID, a.ID)
	assert.Equal(t, grant.ID, a.TargetID)
	assert.Equal(t, f.alice.ID, a.ActorPrincipalID)
	assert.Equal(t, string(CredentialKindInteractive), a.ActorCredentialType)
	assert.Equal(t, f.agentA.ID, a.ActorAgentID)
	assert.Equal(t, f.alice.ID, a.AuthorizingUserID)
	assert.Equal(t, grant.ID, a.SourceGrantID)
	assert.Contains(t, a.AfterSummary, `"agent.read"`)
	assert.NotContains(t, a.AfterSummary, "reporting", "purpose values are not audited")
}

func TestAgentDelegationIssuance_RefusesNonSessionCredentials(t *testing.T) {
	f := newDelegationFixture(t, "adt-cred")
	body := map[string]interface{}{"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "n"}

	// A UAT: the session-only refusal with its reason details.
	uat := scopedIdentityFor(f.alice, f.proj.ID, []string{"agent:read"})
	rec := requestWithCredential(t, f.srv, uat, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/delegations", body)
	apiErr := assertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
	assert.Equal(t, "CREDENTIAL_MANAGEMENT", apiErr.Details["reason"])
	assert.Equal(t, "session_required", apiErr.Details["credential"])

	// An agent JWT: the plain 403 for a non-user identity.
	rec = f.issue(t, f.agentJWT(t, f.agentA), f.agentA.ID, body)
	apiErr = assertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
	assert.Empty(t, apiErr.Details)

	// A dev session.
	rec = f.issue(t, testDevToken, f.agentA.ID, body)
	assertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)

	// A delegated credential is refused at the route gate.
	cred := f.delegated(t, f.hubGrant(t).ID)
	rec = f.issue(t, cred.Token, f.agentA.ID, body)
	assertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)

	// Only the grant issued above exists.
	assert.Len(t, delegationAudits(t, f.store, mutationAgentDelegationGrantCreate), 1)
}

// requestWithCredential sends a request directly to the mux with identity
// and its derived credential context, bypassing the authentication
// middleware.
func requestWithCredential(t *testing.T, srv *Server, identity Identity, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	if user, ok := identity.(UserIdentity); ok {
		ctx = context.WithValue(ctx, userContextKey{}, user)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func TestAgentDelegationIssuance_Refusals(t *testing.T) {
	f := newDelegationFixture(t, "adt-refuse")
	alice := f.session(t, f.alice)
	hubRead := func() map[string]interface{} {
		return map[string]interface{}{"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "n"}
	}

	t.Run("not the controller", func(t *testing.T) {
		rec := f.issue(t, f.session(t, f.carol), f.agentA.ID, hubRead())
		assertAPIError(t, rec, http.StatusForbidden, errCodeIssuerNotController)
	})
	t.Run("unknown agent", func(t *testing.T) {
		rec := f.issue(t, alice, tid("adt-refuse-missing"), hubRead())
		assertAPIError(t, rec, http.StatusNotFound, errCodeAgentNotFound)
	})
	t.Run("selector the issuer cannot mint", func(t *testing.T) {
		body := hubRead()
		body["boundary"] = map[string]string{"kind": "project", "projectId": f.proj.ID}
		body["permissions"] = []string{"project:manage"}
		rec := f.issue(t, alice, f.agentA.ID, body)
		apiErr := assertAPIError(t, rec, http.StatusForbidden, errCodeScopeViolation)
		assert.Equal(t, "project:manage", apiErr.Details["selector"])
		assert.NotEmpty(t, apiErr.Details["reason"])
	})
	t.Run("mintable but not delegable", func(t *testing.T) {
		body := hubRead()
		body["permissions"] = []string{"project:read"}
		rec := f.issue(t, alice, f.agentA.ID, body)
		assertAPIError(t, rec, http.StatusForbidden, errCodePermissionNotDelegable)
	})
	t.Run("project boundary on a project that does not exist", func(t *testing.T) {
		body := hubRead()
		body["boundary"] = map[string]string{"kind": "project", "projectId": tid("adt-refuse-no-project")}
		rec := f.issue(t, alice, f.agentA.ID, body)
		apiErr := assertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
		assert.Empty(t, apiErr.Details)
	})
	t.Run("boundary missing", func(t *testing.T) {
		body := hubRead()
		delete(body, "boundary")
		rec := f.issue(t, alice, f.agentA.ID, body)
		apiErr := assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		assert.Equal(t, "boundary", apiErr.Details["field"])
		assert.Equal(t, "boundary_required", apiErr.Details["reason"])
	})
	t.Run("boundary malformed", func(t *testing.T) {
		body := hubRead()
		body["boundary"] = map[string]string{"kind": "hub", "projectId": f.proj.ID}
		rec := f.issue(t, alice, f.agentA.ID, body)
		apiErr := assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		assert.Equal(t, "boundary_invalid", apiErr.Details["reason"])
	})
	t.Run("expiry beyond thirty days", func(t *testing.T) {
		body := hubRead()
		body["expiresAt"] = time.Now().Add(31 * 24 * time.Hour).UTC().Format(time.RFC3339)
		rec := f.issue(t, alice, f.agentA.ID, body)
		assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	})
	t.Run("expiry in the past", func(t *testing.T) {
		body := hubRead()
		body["expiresAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		rec := f.issue(t, alice, f.agentA.ID, body)
		assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	})
	t.Run("credential lifetime above the maximum", func(t *testing.T) {
		body := hubRead()
		body["maxCredentialTtlSeconds"] = 3601
		rec := f.issue(t, alice, f.agentA.ID, body)
		assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	})
	t.Run("subdelegation", func(t *testing.T) {
		body := hubRead()
		body["allowSubdelegation"] = true
		assertAPIError(t, f.issue(t, alice, f.agentA.ID, body), http.StatusBadRequest, errCodeSubdelegationNotSupported)
		body = hubRead()
		body["parentGrantId"] = "g"
		assertAPIError(t, f.issue(t, alice, f.agentA.ID, body), http.StatusBadRequest, errCodeSubdelegationNotSupported)
	})
	t.Run("reserved label key", func(t *testing.T) {
		body := hubRead()
		body["labels"] = map[string]string{"actor": "agent:x"}
		rec := f.issue(t, alice, f.agentA.ID, body)
		assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		assert.NotContains(t, rec.Body.String(), "agent:x")
	})
	t.Run("suspended agent", func(t *testing.T) {
		stored, err := f.store.GetAgent(context.Background(), f.agentA.ID)
		require.NoError(t, err)
		stored.Phase = "suspended"
		require.NoError(t, f.store.UpdateAgent(context.Background(), stored))
		t.Cleanup(func() {
			again, err := f.store.GetAgent(context.Background(), f.agentA.ID)
			require.NoError(t, err)
			again.Phase = f.agentA.Phase
			require.NoError(t, f.store.UpdateAgent(context.Background(), again))
		})
		assertAPIError(t, f.issue(t, alice, f.agentA.ID, hubRead()), http.StatusConflict, errCodeAgentNotEligible)
	})
	t.Run("experiment off", func(t *testing.T) {
		setAgentDelegationExperiment(t, f.srv, false)
		t.Cleanup(func() { setAgentDelegationExperiment(t, f.srv, true) })
		assertAPIError(t, f.issue(t, alice, f.agentA.ID, hubRead()), http.StatusNotFound, ErrCodeNotFound)
	})

	assert.Empty(t, delegationAudits(t, f.store, mutationAgentDelegationGrantCreate), "no refused request wrote a grant")
}

func TestAgentDelegationIssuance_AuditFailureRollsBack(t *testing.T) {
	f := newDelegationFixture(t, "adt-issue-audit")
	session := f.session(t, f.alice)
	f.srv.store = &rs4FailingStore{Store: f.store, createMutationAuditErr: errors.New("audit write failed")}
	t.Cleanup(func() { f.srv.store = f.store })

	rec := f.issue(t, session, f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "n",
	})
	assertAPIError(t, rec, http.StatusInternalServerError, errCodeAuditFailed)
	assert.Empty(t, delegationAudits(t, f.store, mutationAgentDelegationGrantCreate))
}

// =============================================================================
// Exchange
// =============================================================================

func TestAgentDelegationExchange_IssuesCredentialWithAudit(t *testing.T) {
	f := newDelegationFixture(t, "adt-exchange")
	grant := f.hubGrant(t)
	agentToken := f.agentJWT(t, f.agentA)
	before := time.Now()
	rec := f.exchange(t, agentToken, f.agentA.ID, grant.ID, map[string]interface{}{
		"audience": f.srv.agentDelegationAudience(), "permissions": []string{"agent:read"}, "ttlSeconds": 600,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var out ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.True(t, strings.HasPrefix(out.Token, delegatedCredentialPrefix))
	assert.True(t, wellFormedDelegatedCredential(out.Token))
	assert.Equal(t, grant.ID, out.GrantID)
	assert.Equal(t, []string{"agent.read"}, out.Permissions)
	assert.Equal(t, f.srv.agentDelegationAudience(), out.Audience)
	assert.WithinDuration(t, before.Add(600*time.Second), out.ExpiresAt, time.Minute)

	// Hash only at rest.
	cred, err := f.store.GetAgentDelegatedCredentialByKeyHash(context.Background(), hashDelegatedCredential(out.Token))
	require.NoError(t, err)
	assert.Equal(t, out.CredentialID, cred.ID)
	assert.NotEqual(t, out.Token, cred.KeyHash)
	assert.Equal(t, delegatedCredentialPrefix, cred.Prefix)
	assert.NotContains(t, cred.Prefix+cred.KeyHash+cred.Audience, out.Token[len(delegatedCredentialPrefix):])

	audits := delegationAudits(t, f.store, mutationAgentDelegationCredentialIssue)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, cred.ID, a.TargetID)
	assert.Equal(t, f.agentA.ID, a.ActorPrincipalID)
	assert.Equal(t, string(CredentialKindAgentJWT), a.ActorCredentialType)
	assert.Equal(t, f.agentA.ID, a.ActorAgentID)
	assert.Equal(t, f.alice.ID, a.AuthorizingUserID)
	assert.Equal(t, grant.ID, a.SourceGrantID)
	assert.Equal(t, cred.ExchangeAgentCredentialID, a.ExchangeAgentCredentialID)
	assert.Equal(t, actorKindAgentDelegated, a.ActorKind)
	assert.NotContains(t, a.AfterSummary, out.Token)

	stored, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.LastExchangedAt)

	// The default lifetime is the grant's maximum.
	out2 := f.delegated(t, grant.ID)
	assert.WithinDuration(t, time.Now().Add(agentDelegationCredentialDefaultTTL*time.Second), out2.ExpiresAt, time.Minute)
}

func TestAgentDelegationExchange_RefusesUATAndSession(t *testing.T) {
	f := newDelegationFixture(t, "adt-exch-cred")
	grant := f.hubGrant(t)
	body := map[string]interface{}{"audience": f.srv.agentDelegationAudience()}
	path := "/api/v1/agents/" + f.agentA.ID + "/delegations/" + grant.ID + "/exchange"

	assertAPIError(t, f.do(t, http.MethodPost, path, body, bearer(f.session(t, f.alice))), http.StatusForbidden, errCodeCredentialNotAdmitted)
	assertAPIError(t, f.do(t, http.MethodPost, path, body, bearer(testDevToken)), http.StatusForbidden, errCodeCredentialNotAdmitted)
	uat := scopedIdentityFor(f.alice, f.proj.ID, []string{"agent:read"})
	assertAPIError(t, requestWithCredential(t, f.srv, uat, http.MethodPost, path, body), http.StatusForbidden, errCodeCredentialNotAdmitted)
	assert.Empty(t, delegationAudits(t, f.store, mutationAgentDelegationCredentialIssue), "no refused exchange issued a credential")
}

func TestAgentDelegationExchange_Refusals(t *testing.T) {
	f := newDelegationFixture(t, "adt-exch-refuse")
	grant := f.hubGrant(t)
	aud := f.srv.agentDelegationAudience()
	tokenA := f.agentJWT(t, f.agentA)

	t.Run("another agent's token on its own path", func(t *testing.T) {
		otherAgent, _ := f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: "adt-exch-refuse-x"}), "adt-exch-refuse-x")
		rec := f.exchange(t, f.agentJWT(t, otherAgent), otherAgent.ID, grant.ID, map[string]interface{}{"audience": aud})
		assertAPIError(t, rec, http.StatusNotFound, errCodeGrantNotFound)
		// Identical to a grant that does not exist.
		rec = f.exchange(t, tokenA, f.agentA.ID, tid("adt-exch-refuse-nogrant"), map[string]interface{}{"audience": aud})
		assertAPIError(t, rec, http.StatusNotFound, errCodeGrantNotFound)
	})
	t.Run("path agent is not the token subject", func(t *testing.T) {
		rec := f.exchange(t, tokenA, f.agentC.ID, grant.ID, map[string]interface{}{"audience": aud})
		assertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
	})
	t.Run("wrong audience", func(t *testing.T) {
		rec := f.exchange(t, tokenA, f.agentA.ID, grant.ID, map[string]interface{}{"audience": "scion-hub:elsewhere"})
		assertAPIError(t, rec, http.StatusBadRequest, errCodeInvalidAudience)
	})
	t.Run("permission outside the grant", func(t *testing.T) {
		rec := f.exchange(t, tokenA, f.agentA.ID, grant.ID, map[string]interface{}{"audience": aud, "permissions": []string{"agent:delete"}})
		assertAPIError(t, rec, http.StatusForbidden, errCodeOutsideCeiling)
	})
	t.Run("unknown selector", func(t *testing.T) {
		rec := f.exchange(t, tokenA, f.agentA.ID, grant.ID, map[string]interface{}{"audience": aud, "permissions": []string{"agent:nonsense"}})
		assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	})
	t.Run("lifetime above the maximum", func(t *testing.T) {
		rec := f.exchange(t, tokenA, f.agentA.ID, grant.ID, map[string]interface{}{"audience": aud, "ttlSeconds": 3601})
		assertAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
	})
	t.Run("revoked agent credential", func(t *testing.T) {
		token := f.agentJWT(t, f.agentA)
		claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)
		ac, err := f.store.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
		require.NoError(t, err)
		require.NoError(t, f.store.RevokeAgentCredential(context.Background(), ac.ID, "test", "test"))
		rec := f.exchange(t, token, f.agentA.ID, grant.ID, map[string]interface{}{"audience": aud})
		// The middleware already refuses a revoked agent credential.
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})
	t.Run("revoked grant", func(t *testing.T) {
		other := f.hubGrant(t)
		_, err := f.store.RevokeAgentDelegationGrant(context.Background(), other.ID, "test", "test", "", time.Now())
		require.NoError(t, err)
		rec := f.exchange(t, tokenA, f.agentA.ID, other.ID, map[string]interface{}{"audience": aud})
		assertAPIError(t, rec, http.StatusForbidden, errCodeGrantInactive)
	})
	t.Run("issuer suspended", func(t *testing.T) {
		other := f.hubGrant(t)
		setUserStatus(t, f.store, f.alice.ID, store.UserStatusSuspended)
		t.Cleanup(func() { setUserStatus(t, f.store, f.alice.ID, store.UserStatusActive) })
		rec := f.exchange(t, tokenA, f.agentA.ID, other.ID, map[string]interface{}{"audience": aud})
		// The agent's root user is the issuer, so the actor-state check
		// (standing) refuses before the issuer-state check.
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, []string{errCodeGrantAgentChanged, errCodeIssuerInvalid}, decodeTargetAPIError(t, rec).Code)
	})
	t.Run("experiment off", func(t *testing.T) {
		setAgentDelegationExperiment(t, f.srv, false)
		t.Cleanup(func() { setAgentDelegationExperiment(t, f.srv, true) })
		rec := f.exchange(t, tokenA, f.agentA.ID, grant.ID, map[string]interface{}{"audience": aud})
		assertAPIError(t, rec, http.StatusNotFound, ErrCodeNotFound)
	})

	assert.Empty(t, delegationAudits(t, f.store, mutationAgentDelegationCredentialIssue), "no refused exchange issued a credential")
}

func setUserStatus(t *testing.T, s store.Store, userID, status string) {
	t.Helper()
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	u.Status = status
	require.NoError(t, s.UpdateUser(context.Background(), u))
}

func TestAgentDelegationExchange_AuditFailureRollsBack(t *testing.T) {
	f := newDelegationFixture(t, "adt-exch-audit")
	grant := f.hubGrant(t)
	token := f.agentJWT(t, f.agentA)
	f.srv.store = &rs4FailingStore{Store: f.store, createMutationAuditErr: errors.New("audit write failed")}
	t.Cleanup(func() { f.srv.store = f.store })

	rec := f.exchange(t, token, f.agentA.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	assertAPIError(t, rec, http.StatusInternalServerError, errCodeAuditFailed)
	assert.NotContains(t, rec.Body.String(), delegatedCredentialPrefix)
	stored, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.LastExchangedAt, "the exchange rolled back")
}

// =============================================================================
// Use
// =============================================================================

func TestAgentDelegation_DelegatedReadAcrossProjectsIsRedacted(t *testing.T) {
	f := newDelegationFixture(t, "adt-read")
	cred := f.delegated(t, f.hubGrant(t).ID)

	rec := f.getAgent(t, cred.Token, f.agentB.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "env-probe-value", "a delegated response never carries the agent environment")
	var resp struct {
		ID             string        `json:"id"`
		Cap            *Capabilities `json:"_capabilities"`
		Messageability *struct {
			CanMessage     bool `json:"canMessage"`
			CanReachViewer bool `json:"canReachViewer"`
		} `json:"_messageability"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, f.agentB.ID, resp.ID)
	require.NotNil(t, resp.Cap)
	assert.Equal(t, []string{string(ActionRead)}, resp.Cap.Actions, "only the route's primary action")
	require.NotNil(t, resp.Messageability)
	assert.False(t, resp.Messageability.CanMessage)
	assert.False(t, resp.Messageability.CanReachViewer)

	// Another member's agent in P1, which Alice may read as a member.
	rec = f.getAgent(t, cred.Token, f.agentC.ID)
	require.Equal(t, http.StatusOK, rec.Code, "Alice is a member of P1 and may read C: %s", rec.Body.String())
	// A missing agent answers 404.
	rec = f.getAgent(t, cred.Token, tid("adt-read-missing"))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestAgentDelegation_ProjectBoundaryDeniesOtherProject(t *testing.T) {
	f := newDelegationFixture(t, "adt-boundary")
	rec := f.issue(t, f.session(t, f.alice), f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "project", "projectId": f.proj.ID}, "permissions": []string{"agent:read"}, "name": "p1",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	cred := f.delegated(t, grant.ID)

	assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentC.ID).Code, "a P1 target is inside the boundary")
	rec = f.getAgent(t, cred.Token, f.agentB.ID)
	assertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
}

func TestAgentDelegation_RouteGateDeniesEverythingElse(t *testing.T) {
	f := newDelegationFixture(t, "adt-deny")
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	rowsBefore := len(delegationAudits(t, f.store, mutationAgentDelegationCredentialIssue))

	cases := []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/agents/" + f.agentB.ID},
		{http.MethodDelete, "/api/v1/agents/" + f.agentB.ID},
		{http.MethodGet, "/api/v1/agents"},
		{http.MethodGet, "/api/v1/agents/" + f.agentB.ID + "/pty"},
		{http.MethodPost, "/api/v1/agents/" + f.agentB.ID + "/env"},
		{http.MethodPost, "/api/v1/agents/" + f.agentB.ID + "/exec"},
		{http.MethodPost, "/api/v1/agents/" + f.agentB.ID + "/stop"},
		{http.MethodPost, "/api/v1/agents/" + f.agentB.ID + "/message"},
		{http.MethodGet, "/api/v1/agents/" + f.agentB.ID + "/messages"},
		{http.MethodGet, "/api/v1/projects/" + f.other.ID},
		{http.MethodGet, "/api/v1/projects/" + f.other.ID + "/agents/" + f.agentB.ID},
		{http.MethodGet, "/api/v1/projects/" + f.other.ID + "/agents"},
		{http.MethodGet, "/api/v1/auth/tokens"},
		{http.MethodPost, "/api/v1/agents/" + f.agentA.ID + "/token/refresh"},
		{http.MethodPost, "/api/v1/agents/" + f.agentA.ID + "/delegations"},
		{http.MethodPost, "/api/v1/agents/" + f.agentA.ID + "/delegations/" + grant.ID + "/exchange"},
		{http.MethodGet, "/api/v1/agents/" + f.agentB.ID + "/no-such-route"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := f.do(t, tc.method, tc.path, map[string]interface{}{"audience": f.srv.agentDelegationAudience()}, bearer(cred.Token))
			assertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
			assert.NotContains(t, rec.Body.String(), delegatedCredentialPrefix)
		})
	}
	assert.Len(t, delegationAudits(t, f.store, mutationAgentDelegationCredentialIssue), rowsBefore, "no denied request issued a credential")
}

func TestAgentDelegation_RevocationChainAndExperimentGate(t *testing.T) {
	f := newDelegationFixture(t, "adt-chain")
	ctx := context.Background()

	t.Run("experiment off refuses, back on works again", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID)
		setAgentDelegationExperiment(t, f.srv, false)
		assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code)
		setAgentDelegationExperiment(t, f.srv, true)
		assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
	})
	t.Run("revoked grant", func(t *testing.T) {
		grant := f.hubGrant(t)
		cred := f.delegated(t, grant.ID)
		require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
		_, err := f.store.RevokeAgentDelegationGrant(ctx, grant.ID, "test", "test", "", time.Now())
		require.NoError(t, err)
		// Revoking the grant revokes its credentials too.
		assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code)
	})
	t.Run("exchange agent credential revoked", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID)
		row, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
		require.NoError(t, err)
		require.NoError(t, f.store.RevokeAgentCredential(ctx, row.ExchangeAgentCredentialID, "test", "refresh"))
		assertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	})
	t.Run("issuer suspended", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID)
		setUserStatus(t, f.store, f.alice.ID, store.UserStatusSuspended)
		defer setUserStatus(t, f.store, f.alice.ID, store.UserStatusActive)
		assertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	})
	t.Run("agent suspended", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID)
		stored, err := f.store.GetAgent(ctx, f.agentA.ID)
		require.NoError(t, err)
		phase := stored.Phase
		stored.Phase = "suspended"
		require.NoError(t, f.store.UpdateAgent(ctx, stored))
		defer func() {
			again, err := f.store.GetAgent(ctx, f.agentA.ID)
			require.NoError(t, err)
			again.Phase = phase
			require.NoError(t, f.store.UpdateAgent(ctx, again))
		}()
		assertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	})
	t.Run("issuer loses access to the target's project", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID)
		require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
		require.NoError(t, f.store.DeleteRoleBinding(ctx, f.aliceP2Binding))
		assertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		// The P1 target is still readable.
		assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentC.ID).Code)
	})
}

func TestAgentDelegation_HeaderRules(t *testing.T) {
	f := newDelegationFixture(t, "adt-headers")
	cred := f.delegated(t, f.hubGrant(t).ID)
	agentToken := f.agentJWT(t, f.agentA)

	// A valid agent token header wins: the plain agent, which cannot read
	// an agent in another project, and gains nothing from the bearer.
	rec := f.do(t, http.MethodGet, "/api/v1/agents/"+f.agentB.ID, nil, map[string]string{
		"X-Scion-Agent-Token": agentToken, "Authorization": "Bearer " + cred.Token,
	})
	assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, rec.Code, rec.Body.String())

	// An invalid agent token header with the bearer: 401.
	rec = f.do(t, http.MethodGet, "/api/v1/agents/"+f.agentB.ID, nil, map[string]string{
		"X-Scion-Agent-Token": "not-a-token", "Authorization": "Bearer " + cred.Token,
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// A delegated credential in the agent token header is an invalid agent
	// token.
	rec = f.do(t, http.MethodGet, "/api/v1/agents/"+f.agentB.ID, nil, map[string]string{"X-Scion-Agent-Token": cred.Token})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// An unknown or malformed delegated credential: 401.
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, delegatedCredentialPrefix+strings.Repeat("A", 43), f.agentB.ID).Code)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, delegatedCredentialPrefix+"short", f.agentB.ID).Code)
}

func TestAgentDelegation_OrdinaryAgentTokenGainsNothing(t *testing.T) {
	f := newDelegationFixture(t, "adt-golden")
	agentToken := f.agentJWT(t, f.agentA)
	read := func() int {
		return f.do(t, http.MethodGet, "/api/v1/agents/"+f.agentB.ID, nil, map[string]string{"X-Scion-Agent-Token": agentToken}).Code
	}
	before := read()
	cred := f.delegated(t, f.hubGrant(t).ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
	assert.Equal(t, before, read(), "an issued grant adds nothing to the agent's own token")
	assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, before)
}
