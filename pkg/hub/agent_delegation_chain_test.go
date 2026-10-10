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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtDecisionCapture is a decision-audit emitter that keeps every record.
type adtDecisionCapture struct {
	mu   sync.Mutex
	recs []*store.DecisionAuditRecord
}

func (c *adtDecisionCapture) EmitDecisionAudit(_ context.Context, r *store.DecisionAuditRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

func (c *adtDecisionCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = nil
}

func (c *adtDecisionCapture) records() []*store.DecisionAuditRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*store.DecisionAuditRecord(nil), c.recs...)
}

// delegated returns the records of agent delegation decisions.
func (c *adtDecisionCapture) delegated() []*store.DecisionAuditRecord {
	var out []*store.DecisionAuditRecord
	for _, r := range c.records() {
		if strings.HasPrefix(r.Reason, "agent delegation") {
			out = append(out, r)
		}
	}
	return out
}

// assertDelegatedDeny asserts that the last agent delegation decision was a
// deny by agent delegation with code.
func (c *adtDecisionCapture) assertDelegatedDeny(t *testing.T, code string) {
	t.Helper()
	recs := c.delegated()
	require.NotEmpty(t, recs, "no agent delegation decision was recorded")
	last := recs[len(recs)-1]
	assert.Equal(t, "deny", last.Result)
	assert.Equal(t, string(DeniedByAgentDelegation), last.DeniedBy)
	assert.Equal(t, "agent delegation: "+code, last.Reason)
}

// captureDecisions installs a capturing decision-audit emitter for the test.
func (f *adtFixture) captureDecisions(t *testing.T) *adtDecisionCapture {
	t.Helper()
	c := &adtDecisionCapture{}
	prev := f.srv.authzService.decisionAuditEmitter
	f.srv.authzService.SetDecisionAuditEmitter(c)
	t.Cleanup(func() { f.srv.authzService.SetDecisionAuditEmitter(prev) })
	return c
}

// mutateAgent applies change to the stored agent and returns a function
// that restores the fields change touched.
func (f *adtFixture) mutateAgent(t *testing.T, agentID string, change func(a *store.Agent)) func() {
	t.Helper()
	ctx := context.Background()
	before, err := f.store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	orig := *before
	change(before)
	require.NoError(t, f.store.UpdateAgent(ctx, before))
	return func() {
		now, err := f.store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		now.Phase, now.OwnerID, now.Generation, now.ReincarnationState = orig.Phase, orig.OwnerID, orig.Generation, orig.ReincarnationState
		require.NoError(t, f.store.UpdateAgent(ctx, now))
	}
}

// transferredAgent creates agent D through the handler as Alice (so Alice
// stays its root user) and makes Dave, a member of P1 and P2, its owner.
// Dave is then a controller who is not the root user, so his account state
// is checked by the issuer checks rather than by the agent's standing.
func (f *adtFixture) transferredAgent(t *testing.T, name string) (*store.Agent, *store.User, string) {
	t.Helper()
	dave := hubMemberUser(t, f.store, name+"-dave")
	daveP1 := adtGrantRole(t, f.store, dave.ID, f.proj.ID, store.ProjectRoleMember)
	adtGrantRole(t, f.store, dave.ID, f.other.ID, store.ProjectRoleMember)
	d, _ := f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: name + "-d"}), name+"-d")
	f.mutateAgent(t, d.ID, func(a *store.Agent) { a.OwnerID = dave.ID })
	stored, err := f.store.GetAgent(context.Background(), d.ID)
	require.NoError(t, err)
	require.Equal(t, dave.ID, stored.OwnerID)
	require.Equal(t, f.alice.ID, stored.Ancestry[0])
	return stored, dave, daveP1
}

// grantFor issues a hub agent:read grant for agentID with session.
func (f *adtFixture) grantFor(t *testing.T, session, agentID string) AgentDelegationGrantResponse {
	t.Helper()
	rec := f.issue(t, session, agentID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "g",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	return grant
}

// exchangeFor exchanges grantID with agent a's token.
func (f *adtFixture) exchangeFor(t *testing.T, a *store.Agent, grantID string) *httptest.ResponseRecorder {
	t.Helper()
	return f.exchange(t, f.agentJWT(t, a), a.ID, grantID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
}

func (f *adtFixture) credentialFor(t *testing.T, a *store.Agent, grantID string) ExchangeAgentDelegationResponse {
	t.Helper()
	rec := f.exchangeFor(t, a, grantID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// insertCredential writes a delegated credential row directly, exchanged
// with a live agent credential of a, and returns its bearer. It reaches row
// shapes the exchange handler never writes.
func (f *adtFixture) insertCredential(t *testing.T, a *store.Agent, grantID, audience string, expires time.Time) string {
	t.Helper()
	ctx := context.Background()
	claims, err := f.srv.agentTokenService.ValidateAgentToken(f.agentJWT(t, a))
	require.NoError(t, err)
	ac, err := f.store.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	require.NoError(t, err)
	token, hash, err := newDelegatedCredentialToken()
	require.NoError(t, err)
	require.NoError(t, f.store.CreateAgentDelegatedCredential(ctx, &store.AgentDelegatedCredential{
		GrantID: grantID, AgentID: a.ID, KeyHash: hash, Prefix: delegatedCredentialPrefix, Audience: audience,
		CeilingPermissionIDs: []string{"agent.read"}, ExchangeAgentCredentialID: ac.ID, ExpiresAt: expires,
	}))
	return token
}

// storeGrant writes a grant for agent a, issued by Alice, directly.
func (f *adtFixture) storeGrant(t *testing.T, a *store.Agent, expires time.Time) *store.AgentDelegationGrant {
	t.Helper()
	g := &store.AgentDelegationGrant{
		AgentID: a.ID, AgentProjectID: a.ProjectID, AgentGeneration: a.Generation, AgentStateVersion: a.StateVersion,
		IssuerUserID: f.alice.ID, BoundaryKind: "hub", CeilingVersion: 1, CeilingPermissionIDs: []string{"agent.read"},
		Name: "stored", MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL, ExpiresAt: expires,
		IssuanceAuditID: "00000000-0000-0000-0000-000000000001",
	}
	require.NoError(t, f.store.CreateAgentDelegationGrant(context.Background(), g))
	return g
}

// =============================================================================
// Issuer state, controller and reserved identities
// =============================================================================

func TestAgentDelegation_IssuerWhoIsNotTheRootUser(t *testing.T) {
	f := newADTFixture(t, "adt-issuer")
	decisions := f.captureDecisions(t)
	d, dave, _ := f.transferredAgent(t, "adt-issuer")
	daveSession := f.session(t, dave)

	t.Run("suspended issuer denies at use with the issuer check", func(t *testing.T) {
		cred := f.credentialFor(t, d, f.grantFor(t, daveSession, d.ID).ID)
		require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)
		adtSetUserStatus(t, f.store, dave.ID, store.UserStatusSuspended)
		defer adtSetUserStatus(t, f.store, dave.ID, store.UserStatusActive)
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerSuspended)
	})
	t.Run("suspended issuer cannot exchange", func(t *testing.T) {
		grant := f.grantFor(t, daveSession, d.ID)
		adtSetUserStatus(t, f.store, dave.ID, store.UserStatusSuspended)
		defer adtSetUserStatus(t, f.store, dave.ID, store.UserStatusActive)
		// Project admission (§8.2 step 7) refuses an inactive account
		// before the issuer-state step (step 9) is reached.
		adtAssertAPIError(t, f.exchangeFor(t, d, grant.ID), http.StatusForbidden, errCodeIssuerProjectAccess)
	})
	t.Run("reserved issuer: exchange record names the precise reason", func(t *testing.T) {
		// Reserved issuer at exchange step 9: the record says
		// reserved_identity, the response says issuer_invalid.
		grant := f.grantFor(t, daveSession, d.ID)
		f.srv.platformAuthSA = dave.Email
		defer func() { f.srv.platformAuthSA = "" }()
		decisions.reset()
		adtAssertAPIError(t, f.exchangeFor(t, d, grant.ID), http.StatusForbidden, errCodeIssuerInvalid)
		decisions.assertDelegatedDeny(t, agentDelegationCodeReservedIdentity)
	})
	t.Run("issuer no longer the controller", func(t *testing.T) {
		grant := f.grantFor(t, daveSession, d.ID)
		cred := f.credentialFor(t, d, grant.ID)
		restore := f.mutateAgent(t, d.ID, func(a *store.Agent) { a.OwnerID = f.alice.ID })
		defer restore()
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerNotController)
		adtAssertAPIError(t, f.exchangeFor(t, d, grant.ID), http.StatusForbidden, errCodeIssuerNotController)
	})
	t.Run("reserved issuer: exchange denies and revokes", func(t *testing.T) {
		grant := f.grantFor(t, daveSession, d.ID)
		f.srv.platformAuthSA = dave.Email
		defer func() { f.srv.platformAuthSA = "" }()
		adtAssertAPIError(t, f.exchangeFor(t, d, grant.ID), http.StatusForbidden, errCodeIssuerInvalid)
		stored, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.RevokedAt)
		assert.Equal(t, revokeReasonReservedIdentity, stored.RevokeReason)
		revokes := adtAudits(t, f.store, mutationAgentDelegationGrantRevoke)
		require.NotEmpty(t, revokes)
		assert.Equal(t, grant.ID, revokes[len(revokes)-1].SourceGrantID)
		assert.Equal(t, stored.RevocationAuditID, revokes[len(revokes)-1].ID)
	})
	t.Run("reserved issuer: use refuses and revokes", func(t *testing.T) {
		grant := f.grantFor(t, daveSession, d.ID)
		cred := f.credentialFor(t, d, grant.ID)
		f.srv.platformAuthSA = dave.Email
		defer func() { f.srv.platformAuthSA = "" }()
		assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code)
		stored, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.RevokedAt)
	})
	t.Run("reserved issuer: a failed revocation still refuses", func(t *testing.T) {
		grant := f.grantFor(t, daveSession, d.ID)
		cred := f.credentialFor(t, d, grant.ID)
		f.srv.platformAuthSA = dave.Email
		f.srv.store = &adtFaultStore{Store: f.store, revokeErr: errors.New("revoke failed")}
		defer func() { f.srv.platformAuthSA = ""; f.srv.store = f.store }()
		assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentB.ID).Code)
		stored, err := f.store.GetAgentDelegationGrant(context.Background(), grant.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.RevokedAt, "the revocation failed")
	})
	t.Run("reserved issuer cannot issue", func(t *testing.T) {
		f.srv.platformAuthSA = dave.Email
		defer func() { f.srv.platformAuthSA = "" }()
		rec := adtRequestWithCredential(t, f.srv, authUser(dave), http.MethodPost, "/api/v1/agents/"+d.ID+"/delegations", map[string]interface{}{
			"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "g",
		})
		adtAssertAPIError(t, rec, http.StatusForbidden, errCodeReservedIdentity)
	})
}

// =============================================================================
// Agent changes, expiry, audience, ceiling version, policy, lookups
// =============================================================================

func TestAgentDelegation_AgentChangesDeny(t *testing.T) {
	f := newADTFixture(t, "adt-agentchange")
	decisions := f.captureDecisions(t)
	alice := f.session(t, f.alice)

	t.Run("reincarnation in flight", func(t *testing.T) {
		grant := f.hubGrant(t)
		cred := f.delegated(t, grant.ID)
		token := f.agentJWT(t, f.agentA)
		restore := f.mutateAgent(t, f.agentA.ID, func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStatePending })
		defer restore()
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeGrantAgentChanged)
		rec := f.exchange(t, token, f.agentA.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
		adtAssertAPIError(t, rec, http.StatusForbidden, errCodeGrantAgentChanged)
		rec = f.issue(t, alice, f.agentA.ID, map[string]interface{}{
			"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "g",
		})
		adtAssertAPIError(t, rec, http.StatusConflict, errCodeAgentReincarnating)
	})
	t.Run("generation changed", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID)
		restore := f.mutateAgent(t, f.agentA.ID, func(a *store.Agent) { a.Generation++ })
		defer restore()
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeGrantAgentChanged)
	})
}

func TestAgentDelegation_ExpiryAudienceAndLifetimeClamps(t *testing.T) {
	f := newADTFixture(t, "adt-expiry")
	decisions := f.captureDecisions(t)
	a := f.agentA
	stored, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)

	t.Run("expired grant cannot be exchanged", func(t *testing.T) {
		g := f.storeGrant(t, stored, time.Now().Add(-time.Minute))
		adtAssertAPIError(t, f.exchangeFor(t, stored, g.ID), http.StatusForbidden, errCodeGrantInactive)
	})
	t.Run("expired grant denies at use", func(t *testing.T) {
		g := f.storeGrant(t, stored, time.Now().Add(-time.Minute))
		token := f.insertCredential(t, stored, g.ID, f.srv.agentDelegationAudience(), time.Now().Add(10*time.Minute))
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeGrantInactive)
	})
	t.Run("credential for another hub's audience is refused", func(t *testing.T) {
		g := f.storeGrant(t, stored, time.Now().Add(time.Hour))
		token := f.insertCredential(t, stored, g.ID, "scion-hub:another-hub", time.Now().Add(10*time.Minute))
		assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, token, f.agentB.ID).Code)
	})
	t.Run("lifetime clamped to the grant expiry", func(t *testing.T) {
		g := f.storeGrant(t, stored, time.Now().Add(2*time.Minute))
		out := f.credentialFor(t, stored, g.ID)
		assert.WithinDuration(t, g.ExpiresAt, out.ExpiresAt, time.Second)
	})
	t.Run("lifetime clamped to the agent credential expiry", func(t *testing.T) {
		g := f.storeGrant(t, stored, time.Now().Add(time.Hour))
		grant, err := f.srv.AuthorizeAgentToken(context.Background(), stored)
		require.NoError(t, err)
		token, ac, err := f.srv.SignAgentToken(grant, stored.RunID)
		require.NoError(t, err)
		ac.ExpiresAt = time.Now().Add(90 * time.Second)
		require.NoError(t, f.store.CreateAgentCredential(context.Background(), ac))
		rec := f.exchange(t, token, stored.ID, g.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out ExchangeAgentDelegationResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		assert.WithinDuration(t, ac.ExpiresAt, out.ExpiresAt, time.Second)
	})
}

func TestAgentDelegation_PolicyNarrowingDenies(t *testing.T) {
	f := newADTFixture(t, "adt-policy")
	decisions := f.captureDecisions(t)
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)

	saved := permissions.AgentDelegableRegistry
	permissions.AgentDelegableRegistry = map[string][]permissions.BoundaryKind{}
	t.Cleanup(func() { permissions.AgentDelegableRegistry = saved })

	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodePermissionNotDelegable)
	adtAssertAPIError(t, f.exchangeFor(t, f.agentA, grant.ID), http.StatusForbidden, errCodePermissionNotDelegable)
}

func TestAgentDelegation_LookupErrorAtUse(t *testing.T) {
	f := newADTFixture(t, "adt-lookup")
	cred := f.delegated(t, f.hubGrant(t).ID)
	f.srv.store = &adtFaultStore{Store: f.store, grantErr: errors.New("store unavailable")}
	t.Cleanup(func() { f.srv.store = f.store })
	assert.Equal(t, http.StatusServiceUnavailable, f.getAgent(t, cred.Token, f.agentB.ID).Code)
}

func TestAgentDelegationExchange_SuperAdminSessionNotAdmitted(t *testing.T) {
	f := newADTFixture(t, "adt-superadmin")
	grant := f.hubGrant(t)
	admin := &store.User{ID: tid("adt-superadmin-user"), Email: "adt-superadmin@target.test", DisplayName: "Admin",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, f.store.CreateUser(context.Background(), admin))
	path := "/api/v1/agents/" + f.agentA.ID + "/delegations/" + grant.ID + "/exchange"
	rec := f.do(t, http.MethodPost, path, map[string]interface{}{"audience": f.srv.agentDelegationAudience()}, adtBearer(f.session(t, admin)))
	adtAssertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
}

// TestAgentDelegation_OneRecordPerCheckAndNoReasonInResponses: a delegated
// allow is recorded once even with allow sampling at 0, the record names
// the grant, and no delegated response carries the decision's reason.
func TestAgentDelegation_OneRecordPerCheckAndNoReasonInResponses(t *testing.T) {
	f := newADTFixture(t, "adt-records")
	decisions := f.captureDecisions(t)
	f.srv.authzService.DecisionAuditSampleRate = 0
	t.Cleanup(func() { f.srv.authzService.DecisionAuditSampleRate = 1.0 })
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)

	decisions.reset()
	rec := f.getAgent(t, cred.Token, f.agentB.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var allows []*store.DecisionAuditRecord
	for _, r := range decisions.delegated() {
		if r.Result == "allow" {
			allows = append(allows, r)
		}
	}
	require.Len(t, agentDelegationAdmittedRoutes, 1, "a new admitted route needs its own record and no-reason checks here")
	require.Len(t, allows, 1, "one record for the delegated agent.read check")
	assert.Equal(t, "agent_delegation:"+grant.ID, allows[0].MatchedGrant)
	assert.Equal(t, f.agentA.ID, allows[0].PrincipalID)
	assert.NotContains(t, rec.Body.String(), "agent delegation")

	// A denied delegated read: the handler's own 403, without the reason.
	require.NoError(t, f.store.RevokeAgentCredential(context.Background(), mustExchangeAgentCredential(t, f, cred.Token), "test", "test"))
	rec = f.getAgent(t, cred.Token, f.agentB.ID)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "agent delegation")
	assert.NotContains(t, rec.Body.String(), agentDelegationCodeAgentCredentialInvalid)
}

func mustExchangeAgentCredential(t *testing.T, f *adtFixture, token string) string {
	t.Helper()
	row, err := f.store.GetAgentDelegatedCredentialByKeyHash(context.Background(), hashDelegatedCredential(token))
	require.NoError(t, err)
	return row.ExchangeAgentCredentialID
}

// =============================================================================
// Rollback leaves no rows
// =============================================================================

func TestAgentDelegation_AuditFailureLeavesNoRows(t *testing.T) {
	f := newADTFixture(t, "adt-norows")
	session := f.session(t, f.alice)
	grant := f.hubGrant(t)
	token := f.agentJWT(t, f.agentA)

	fault := &adtFaultStore{Store: f.store, auditErr: errors.New("audit write failed")}
	f.srv.store = fault
	t.Cleanup(func() { f.srv.store = f.store })

	rec := f.issue(t, session, f.agentA.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "n",
	})
	adtAssertAPIError(t, rec, http.StatusInternalServerError, errCodeAuditFailed)
	rec = f.exchange(t, token, f.agentA.ID, grant.ID, map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	adtAssertAPIError(t, rec, http.StatusInternalServerError, errCodeAuditFailed)

	require.NotEmpty(t, fault.grantIDs, "issuance reached the grant insert")
	require.NotEmpty(t, fault.credentialHashes, "exchange reached the credential insert")
	for _, id := range fault.grantIDs {
		_, err := f.store.GetAgentDelegationGrant(context.Background(), id)
		assert.ErrorIs(t, err, store.ErrNotFound, "grant %s rolled back", id)
	}
	for _, h := range fault.credentialHashes {
		_, err := f.store.GetAgentDelegatedCredentialByKeyHash(context.Background(), h)
		assert.ErrorIs(t, err, store.ErrNotFound, "credential rolled back")
	}
}

// adtFaultStore wraps a store: it records the grant IDs and credential
// hashes written through it and injects the configured faults, inside and
// outside transactions.
type adtFaultStore struct {
	store.Store
	auditErr  error
	revokeErr error
	grantErr  error

	mu               *sync.Mutex
	grantIDs         []string
	credentialHashes []string
}

func (s *adtFaultStore) lock() func() {
	if s.mu == nil {
		s.mu = &sync.Mutex{}
	}
	s.mu.Lock()
	return s.mu.Unlock
}

func (s *adtFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&adtFaultTx{Store: tx, parent: s})
	})
}

func (s *adtFaultStore) GetAgentDelegationGrant(ctx context.Context, id string) (*store.AgentDelegationGrant, error) {
	if s.grantErr != nil {
		return nil, s.grantErr
	}
	return s.Store.GetAgentDelegationGrant(ctx, id)
}

// adtFaultTx is the transaction-scoped side of adtFaultStore.
type adtFaultTx struct {
	store.Store
	parent *adtFaultStore
}

func (t *adtFaultTx) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if t.parent.auditErr != nil {
		return t.parent.auditErr
	}
	return t.Store.CreateMutationAudit(ctx, r)
}

func (t *adtFaultTx) CreateAgentDelegationGrant(ctx context.Context, g *store.AgentDelegationGrant) error {
	err := t.Store.CreateAgentDelegationGrant(ctx, g)
	defer t.parent.lock()()
	t.parent.grantIDs = append(t.parent.grantIDs, g.ID)
	return err
}

func (t *adtFaultTx) CreateAgentDelegatedCredential(ctx context.Context, c *store.AgentDelegatedCredential) error {
	err := t.Store.CreateAgentDelegatedCredential(ctx, c)
	defer t.parent.lock()()
	t.parent.credentialHashes = append(t.parent.credentialHashes, c.KeyHash)
	return err
}

func (t *adtFaultTx) RevokeAgentDelegationGrant(ctx context.Context, id, revokedBy, reason, auditID string, at time.Time) (bool, error) {
	if t.parent.revokeErr != nil {
		return false, t.parent.revokeErr
	}
	return t.Store.RevokeAgentDelegationGrant(ctx, id, revokedBy, reason, auditID, at)
}

// =============================================================================
// Pins from §12.3
// =============================================================================

func TestAgentDelegation_MaintenanceModeRefuses(t *testing.T) {
	state := NewMaintenanceState(true, "")
	mw := adminModeMiddleware(state)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/a", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), adtTestIdentity("agent-x", "project-x")))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestAgentDelegation_NoUserOrAgentShortcutsApply(t *testing.T) {
	var id Identity = adtTestIdentity("agent-x", "project-x")
	_, isUser := id.(UserIdentity)
	_, isAgent := id.(AgentIdentity)
	assert.False(t, isUser, "never a user identity, so IsUnscopedLocalPlatformAdmin cannot see it")
	assert.False(t, isAgent, "never an agent identity, so the reincarnation self-exemption cannot match it")
	ctx := contextWithIdentity(context.Background(), id)
	assert.False(t, IsUnscopedLocalPlatformAdmin(GetUserIdentityFromContext(ctx)))
	assert.Nil(t, GetAgentIdentityFromContext(ctx))
}

func TestAgentDelegation_ReincarnateRouteRefused(t *testing.T) {
	f := newADTFixture(t, "adt-reinc")
	cred := f.delegated(t, f.hubGrant(t).ID)
	rec := f.do(t, http.MethodPost, "/api/v1/agents/"+f.agentA.ID+"/reincarnate", map[string]interface{}{}, adtBearer(cred.Token))
	adtAssertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
}

// TestAgentDelegation_IssuerLeavesTheAgentsProject: an issuer who stays the
// agent's owner but leaves its project cannot exchange or use a hub grant,
// even on a target in another project they can still reach (§17.2 #7).
// Issuance is refused too; the owner relationship that rule 2 evaluates
// itself requires project access, so it refuses first.
func TestAgentDelegation_IssuerLeavesTheAgentsProject(t *testing.T) {
	f := newADTFixture(t, "adt-leave")
	decisions := f.captureDecisions(t)
	d, dave, daveP1 := f.transferredAgent(t, "adt-leave")
	daveSession := f.session(t, dave)
	grant := f.grantFor(t, daveSession, d.ID)
	cred := f.credentialFor(t, d, grant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentB.ID).Code)

	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), daveP1))

	decisions.reset()
	adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
	decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerProjectAccess)
	adtAssertAPIError(t, f.exchangeFor(t, d, grant.ID), http.StatusForbidden, errCodeIssuerProjectAccess)
	rec := f.issue(t, daveSession, d.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "g",
	})
	adtAssertAPIError(t, rec, http.StatusForbidden, errCodeIssuerNotController)
}

// TestAgentDelegationExchange_AgentCredentialRowChecks: the exchange handler
// refuses an agent token with no credential row or an expired row, which
// the middleware still admits, with 401 agent_credential_invalid (§8.2
// step 2). A revoked row is refused by the middleware with 401
// unauthorized. None of them issues a credential.
func TestAgentDelegationExchange_AgentCredentialRowChecks(t *testing.T) {
	f := newADTFixture(t, "adt-acrow")
	ctx := context.Background()
	grant := f.hubGrant(t)
	aud := map[string]interface{}{"audience": f.srv.agentDelegationAudience()}
	stored, err := f.store.GetAgent(ctx, f.agentA.ID)
	require.NoError(t, err)
	tokenGrant, err := f.srv.AuthorizeAgentToken(ctx, stored)
	require.NoError(t, err)

	t.Run("no credential row", func(t *testing.T) {
		token, _, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
		require.NoError(t, err)
		rec := f.exchange(t, token, stored.ID, grant.ID, aud)
		adtAssertAPIError(t, rec, http.StatusUnauthorized, errCodeAgentCredentialInvalid)
	})
	t.Run("expired credential row", func(t *testing.T) {
		token, ac, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
		require.NoError(t, err)
		ac.ExpiresAt = time.Now().Add(-time.Minute)
		require.NoError(t, f.store.CreateAgentCredential(ctx, ac))
		rec := f.exchange(t, token, stored.ID, grant.ID, aud)
		adtAssertAPIError(t, rec, http.StatusUnauthorized, errCodeAgentCredentialInvalid)
	})
	t.Run("revoked credential row", func(t *testing.T) {
		token, ac, err := f.srv.SignAgentToken(tokenGrant, stored.RunID)
		require.NoError(t, err)
		require.NoError(t, f.store.CreateAgentCredential(ctx, ac))
		require.NoError(t, f.store.RevokeAgentCredential(ctx, ac.ID, "test", "test"))
		rec := f.exchange(t, token, stored.ID, grant.ID, aud)
		adtAssertAPIError(t, rec, http.StatusUnauthorized, ErrCodeUnauthorized)
	})
	assert.Empty(t, adtAudits(t, f.store, mutationAgentDelegationCredentialIssue), "no credential was issued")
}

// TestAgentDelegation_AgentAndIssuerStateAtExchangeAndUse covers the bound
// agent suspended, held or deleted, and the issuer deleted, at exchange and
// at use (§17.2 #4, #5), and the grant-lookup reasons (§17.2 #1).
func TestAgentDelegation_AgentAndIssuerStateAtExchangeAndUse(t *testing.T) {
	f := newADTFixture(t, "adt-state")
	ctx := context.Background()
	decisions := f.captureDecisions(t)
	aud := map[string]interface{}{"audience": f.srv.agentDelegationAudience()}

	t.Run("another agent's grant and a missing grant record different reasons", func(t *testing.T) {
		grant := f.hubGrant(t)
		other, _ := f.createdAgent(t, f.create(t, authUser(f.alice), CreateAgentRequest{Name: "adt-state-x"}), "adt-state-x")
		decisions.reset()
		adtAssertAPIError(t, f.exchangeFor(t, other, grant.ID), http.StatusNotFound, errCodeGrantNotFound)
		decisions.assertDelegatedDeny(t, agentDelegationReasonGrantOtherAgent)
		decisions.reset()
		adtAssertAPIError(t, f.exchangeFor(t, other, tid("adt-state-nogrant")), http.StatusNotFound, errCodeGrantNotFound)
		decisions.assertDelegatedDeny(t, errCodeGrantNotFound)
	})
	t.Run("agent suspended", func(t *testing.T) {
		grant := f.hubGrant(t)
		token := f.agentJWT(t, f.agentA)
		restore := f.mutateAgent(t, f.agentA.ID, func(a *store.Agent) { a.Phase = "suspended" })
		defer restore()
		adtAssertAPIError(t, f.exchange(t, token, f.agentA.ID, grant.ID, aud), http.StatusForbidden, errCodeGrantAgentChanged)
	})
	t.Run("agent held", func(t *testing.T) {
		d, _, _ := f.transferredAgent(t, "adt-state-held")
		grant := f.grantFor(t, f.session(t, f.alice), d.ID)
		cred := f.credentialFor(t, d, grant.ID)
		token := f.agentJWT(t, d)
		_, err := f.store.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID: d.ID, ProjectID: d.ProjectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: f.alice.ID,
			Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
		}})
		require.NoError(t, err)
		// The agent middleware refuses a held agent's token before exchange.
		adtAssertAPIError(t, f.exchange(t, token, d.ID, grant.ID, aud), http.StatusUnauthorized, ErrCodeUnauthorized)
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeGrantAgentChanged)
	})
	t.Run("agent deleted", func(t *testing.T) {
		grant := f.hubGrant(t)
		cred := f.delegated(t, grant.ID)
		token := f.agentJWT(t, f.agentA)
		restore := f.mutateAgentDeleted(t, f.agentA.ID)
		defer restore()
		adtAssertAPIError(t, f.exchange(t, token, f.agentA.ID, grant.ID, aud), http.StatusForbidden, errCodeGrantAgentChanged)
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeGrantAgentChanged)
	})
	t.Run("issuer deleted", func(t *testing.T) {
		d, dave, _ := f.transferredAgent(t, "adt-state-gone")
		grant := f.grantFor(t, f.session(t, dave), d.ID)
		cred := f.credentialFor(t, d, grant.ID)
		_, err := f.store.DeleteGroupMembershipsForUser(ctx, dave.ID)
		require.NoError(t, err)
		require.NoError(t, f.store.DeleteUser(ctx, dave.ID))
		decisions.reset()
		adtAssertAPIError(t, f.exchangeFor(t, d, grant.ID), http.StatusForbidden, errCodeIssuerInvalid)
		decisions.assertDelegatedDeny(t, agentDelegationReasonIssuerMissing)
		decisions.reset()
		adtAssertAPIError(t, f.getAgent(t, cred.Token, f.agentB.ID), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeIssuerInvalid)
	})
}

// mutateAgentDeleted soft-deletes the agent row and returns a restore.
func (f *adtFixture) mutateAgentDeleted(t *testing.T, agentID string) func() {
	t.Helper()
	ctx := context.Background()
	a, err := f.store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, f.store.UpdateAgent(ctx, a))
	return func() {
		again, err := f.store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		again.DeletedAt = time.Time{}
		require.NoError(t, f.store.UpdateAgent(ctx, again))
	}
}

// TestAgentDelegation_PermissionUnresolvedDenies: a request with no
// explicit permission whose resource and action do not resolve to exactly
// one permission is denied (§17.2 #26).
func TestAgentDelegation_PermissionUnresolvedDenies(t *testing.T) {
	f := newADTFixture(t, "adt-unresolved")
	ctx := context.Background()
	cred := f.delegated(t, f.hubGrant(t).ID)
	row, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
	require.NoError(t, err)
	grant, err := f.store.GetAgentDelegationGrant(ctx, row.GrantID)
	require.NoError(t, err)
	issuer, err := f.store.GetUser(ctx, grant.IssuerUserID)
	require.NoError(t, err)
	agent, err := f.store.GetAgent(ctx, grant.AgentID)
	require.NoError(t, err)
	ac, err := f.store.GetAgentCredentialByID(ctx, row.ExchangeAgentCredentialID)
	require.NoError(t, err)
	st := &delegatedRequestState{
		identity: newDelegatedAgentIdentity(row, grant), credential: row, grant: grant,
		issuer: issuer, agent: agent, exchangeCred: ac, issuerPC: issuerPrincipal(issuer), memo: &ProjectAdmissionCache{},
	}
	dctx := withStandingMemo(contextWithDelegatedAdmission(contextWithDelegatedState(ctx, st), agentDelegationAdmittedRoutes[0]))
	target := agentResource(f.agentB)

	d := f.srv.authzService.Decide(dctx, AuthzRequest{Principal: PrincipalContext{Identity: st.identity}, Resource: target, Action: Action("no_such_action")})
	assert.False(t, d.Allowed)
	require.NotNil(t, d.AgentDelegation)
	assert.Equal(t, agentDelegationCodePermissionUnresolved, d.AgentDelegation.AgentDelegationCode)

	// Control: the same state and target with the read action is allowed.
	d = f.srv.authzService.Decide(dctx, AuthzRequest{Principal: PrincipalContext{Identity: st.identity}, Resource: target, Action: ActionRead})
	assert.True(t, d.Allowed, d.Reason)
}
