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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agent delegation revoke-all (.design/agent-delegation.md §7, §10, §17.2
// flows 39 and 40).

const adtRevokeAllMine = "/api/v1/users/me/delegations/revoke-all"

func adtRevokeAllAgentPath(agentID string) string {
	return "/api/v1/agents/" + agentID + "/delegations/revoke-all"
}

// revokeAll posts to a revoke-all route with token and decodes the 200
// answer.
func (f *adtFixture) revokeAll(t *testing.T, token, path string) RevokeAllAgentDelegationsResponse {
	t.Helper()
	rec := f.do(t, http.MethodPost, path, nil, adtBearer(token))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out RevokeAllAgentDelegationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.Revoked, "revoked is always a list")
	require.NotNil(t, out.Skipped, "skipped is always a list")
	return out
}

// grantRevoked reports whether the stored grant id is revoked.
func (f *adtFixture) grantRevoked(t *testing.T, id string) bool {
	t.Helper()
	g, err := f.store.GetAgentDelegationGrant(context.Background(), id)
	require.NoError(t, err)
	return g.RevokedAt != nil
}

// adtAuthzChangedAgents subscribes to the authorization-change
// notification and returns a function that collects the agent IDs
// published until no event arrives for a short while.
func (f *adtFixture) adtAuthzChangedAgents(t *testing.T) func() []string {
	t.Helper()
	events := NewChannelEventPublisher()
	f.srv.SetEventPublisher(events)
	setConduitExperiment(t, f.srv, true)
	ch, unsubscribe := events.Subscribe(conduitAuthzChangedSubject)
	t.Cleanup(unsubscribe)
	return func() []string {
		var agents []string
		for {
			select {
			case evt := <-ch:
				var m conduitAuthzMatch
				require.NoError(t, json.Unmarshal(evt.Data, &m))
				agents = append(agents, m.AgentID)
			case <-time.After(500 * time.Millisecond):
				return agents
			}
		}
	}
}

// TestAgentDelegationRevokeAll_CallerGrants: revoke-all on
// /users/me/delegations revokes every unrevoked grant the caller issued,
// expired ones included, with their credentials, one audit row per grant
// and one authorization-change notification per agent. It leaves other
// issuers' grants alone, writes nothing on a repeat, and gives a dev
// session an empty answer.
func TestAgentDelegationRevokeAll_CallerGrants(t *testing.T) {
	f := newADTFixture(t, "adt-revall-mine")
	ctx := context.Background()
	d, dave, _ := f.transferredAgent(t, "adt-revall-mine")
	alice := f.session(t, f.alice)
	daveSession := f.session(t, dave)

	hubGrant := f.hubGrant(t)
	p1Grant := f.projectGrant(t, f.proj.ID)
	onD := f.grantFor(t, alice, d.ID)
	expired := f.storeGrant(t, f.agentA, time.Now().Add(-time.Minute))
	already := f.hubGrant(t)
	require.Equal(t, http.StatusNoContent, f.revokeGrant(t, alice, f.agentA.ID, already.ID).Code)
	daveGrant := f.grantFor(t, daveSession, d.ID)
	cred := f.delegated(t, hubGrant.ID)
	require.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentA.ID).Code)
	auditsBefore := len(adtAudits(t, f.store, mutationAgentDelegationGrantRevoke))
	changed := f.adtAuthzChangedAgents(t)

	out := f.revokeAll(t, alice, adtRevokeAllMine)
	want := []string{hubGrant.ID, p1Grant.ID, onD.ID, expired.ID}
	assert.ElementsMatch(t, want, out.Revoked)
	assert.Empty(t, out.Skipped, "the issuer may revoke every grant it issued")
	assert.ElementsMatch(t, []string{f.agentA.ID, d.ID}, changed(), "one notification per agent")

	for _, id := range want {
		g, err := f.store.GetAgentDelegationGrant(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, g.RevokedAt, id)
		assert.Equal(t, revokeReasonRevoked, g.RevokeReason)
		assert.Equal(t, "user:"+f.alice.ID, g.RevokedBy)
		assert.NotEmpty(t, g.RevocationAuditID)
	}
	assert.False(t, f.grantRevoked(t, daveGrant.ID), "another issuer's grant on the same agent is left alone")
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, cred.Token, f.agentA.ID).Code, "the credential is revoked with its grant")
	stored, err := f.store.GetAgentDelegatedCredential(ctx, cred.CredentialID)
	require.NoError(t, err)
	assert.Equal(t, revokeReasonRevoked, stored.RevokeReason)

	audits := adtAudits(t, f.store, mutationAgentDelegationGrantRevoke)
	require.Len(t, audits, auditsBefore+len(want), "one audit row per revoked grant")
	byGrant := map[string]*store.MutationAuditRecord{}
	for _, a := range audits {
		byGrant[a.TargetID] = a
	}
	for _, id := range want {
		a := byGrant[id]
		require.NotNil(t, a, id)
		g, err := f.store.GetAgentDelegationGrant(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, g.RevocationAuditID, a.ID)
		assert.Equal(t, f.alice.ID, a.ActorPrincipalID)
		assert.Equal(t, string(CredentialKindInteractive), a.ActorCredentialType)
		assert.Equal(t, g.AgentID, a.ActorAgentID)
		assert.Equal(t, f.alice.ID, a.AuthorizingUserID)
		assert.Equal(t, id, a.SourceGrantID)
		assert.Empty(t, a.ActorKind)
	}
	assert.Contains(t, byGrant[hubGrant.ID].BeforeSummary, cred.CredentialID)

	// A repeat revokes nothing and writes nothing.
	again := f.revokeAll(t, alice, adtRevokeAllMine)
	assert.Empty(t, again.Revoked)
	assert.Empty(t, again.Skipped)
	assert.Len(t, adtAudits(t, f.store, mutationAgentDelegationGrantRevoke), auditsBefore+len(want))

	// A dev session issues no grants: the answer is empty.
	dev := f.revokeAll(t, testDevToken, adtRevokeAllMine)
	assert.Empty(t, dev.Revoked)
	assert.Empty(t, dev.Skipped)
	assert.False(t, f.grantRevoked(t, daveGrant.ID))

	// Dave revokes his own.
	assert.Equal(t, []string{daveGrant.ID}, f.revokeAll(t, daveSession, adtRevokeAllMine).Revoked)
}

// TestAgentDelegationRevokeAll_AgentGrantsSkipsOutsideAuthority: per-agent
// revoke-all applies agentDelegationControlAllowed per grant with
// agent.delegation.revoke_all. The issuer and the agent's current owner
// need no permission; the hub path needs revoke_all on the hub, which only
// hub-admin and super-admin hold, and the project path revoke_all on the
// grant's boundary project, which no seeded project role holds. A grant
// the caller may read but not revoke this way is skipped and reported; a
// grant outside the caller's read authority is left out.
func TestAgentDelegationRevokeAll_AgentGrantsSkipsOutsideAuthority(t *testing.T) {
	f := newADTFixture(t, "adt-revall-agent")
	alice := f.session(t, f.alice)
	hubAdminUser := newHubAdminRoleUser(t, f.store, "adt-revall-hub-admin")
	hubAdmin := f.session(t, hubAdminUser)
	p1Admin := f.session(t, f.projectRoleUser(t, "adt-revall-p1-admin", f.proj.ID, store.ProjectRoleAdmin))
	p2Owner := f.session(t, f.projectRoleUser(t, "adt-revall-p2-owner", f.other.ID, store.ProjectRoleOwner))
	carol := f.session(t, f.carol)

	hubGrant := f.hubGrant(t)
	p1Grant := f.projectGrant(t, f.proj.ID)
	p1Single := f.projectGrant(t, f.proj.ID)
	p2Grant := f.projectGrant(t, f.other.ID)
	path := adtRevokeAllAgentPath(f.agentA.ID)

	t.Run("a project admin's bulk call skips the project-bounded grants it could revoke singly", func(t *testing.T) {
		out := f.revokeAll(t, p1Admin, path)
		assert.Empty(t, out.Revoked)
		assert.ElementsMatch(t, []AgentDelegationRevokeAllSkip{
			{GrantID: p1Grant.ID, AgentID: f.agentA.ID, Reason: revokeAllSkipForbidden},
			{GrantID: p1Single.ID, AgentID: f.agentA.ID, Reason: revokeAllSkipForbidden},
		}, out.Skipped, "the hub-bounded and P2-bounded grants are outside its read authority and are not reported")
		assert.False(t, f.grantRevoked(t, p1Grant.ID))
		require.Equal(t, http.StatusNoContent, f.revokeGrant(t, p1Admin, f.agentA.ID, p1Single.ID).Code, "the single revoke is admitted")
	})

	t.Run("a project owner of the boundary project is skipped too", func(t *testing.T) {
		out := f.revokeAll(t, p2Owner, path)
		assert.Empty(t, out.Revoked)
		assert.Equal(t, []AgentDelegationRevokeAllSkip{{GrantID: p2Grant.ID, AgentID: f.agentA.ID, Reason: revokeAllSkipForbidden}}, out.Skipped)
	})

	t.Run("a caller with no authority gets an empty answer", func(t *testing.T) {
		out := f.revokeAll(t, carol, path)
		assert.Empty(t, out.Revoked)
		assert.Empty(t, out.Skipped)
	})

	t.Run("the hub admin revokes every grant, including the project-bounded one", func(t *testing.T) {
		out := f.revokeAll(t, hubAdmin, path)
		assert.ElementsMatch(t, []string{hubGrant.ID, p1Grant.ID, p2Grant.ID}, out.Revoked, "the grant already revoked singly is not revoked again")
		assert.Empty(t, out.Skipped)
		g, err := f.store.GetAgentDelegationGrant(context.Background(), p1Grant.ID)
		require.NoError(t, err)
		assert.Equal(t, "user:"+hubAdminUser.ID, g.RevokedBy)
	})

	t.Run("the dev session counts as hub authority", func(t *testing.T) {
		g := f.hubGrant(t)
		assert.Equal(t, []string{g.ID}, f.revokeAll(t, testDevToken, path).Revoked)
	})

	t.Run("the issuer revokes every grant it issued on the agent", func(t *testing.T) {
		g1, g2 := f.hubGrant(t), f.projectGrant(t, f.other.ID)
		assert.ElementsMatch(t, []string{g1.ID, g2.ID}, f.revokeAll(t, alice, path).Revoked)
	})

	t.Run("the current owner who is not the issuer revokes", func(t *testing.T) {
		d, dave, _ := f.transferredAgent(t, "adt-revall-agent")
		g := f.grantFor(t, alice, d.ID)
		out := f.revokeAll(t, f.session(t, dave), adtRevokeAllAgentPath(d.ID))
		assert.Equal(t, []string{g.ID}, out.Revoked)
	})

	t.Run("an agent with no grants answers with empty lists", func(t *testing.T) {
		out := f.revokeAll(t, hubAdmin, adtRevokeAllAgentPath(f.agentC.ID))
		assert.Empty(t, out.Revoked)
		assert.Empty(t, out.Skipped)
	})
}

// TestAgentDelegationRevokeAll_RefusedCredentials: revoke-all is session
// only. A user access token gets the session-only refusal, a delegated
// credential credential_not_admitted, and the bound agent's own agent token
// 403 on the per-agent route; other methods answer 405. Nothing is revoked.
func TestAgentDelegationRevokeAll_RefusedCredentials(t *testing.T) {
	f := newADTFixture(t, "adt-revall-refuse")
	grant := f.hubGrant(t)
	cred := f.delegated(t, grant.ID)
	uat := scopedIdentityFor(f.alice, f.proj.ID, []string{"agent:read"})

	for _, path := range []string{adtRevokeAllMine, adtRevokeAllAgentPath(f.agentA.ID)} {
		t.Run(path, func(t *testing.T) {
			rec := adtRequestWithCredential(t, f.srv, uat, http.MethodPost, path, nil)
			apiErr := adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
			assert.Equal(t, "CREDENTIAL_MANAGEMENT", apiErr.Details["reason"])
			assert.Equal(t, "session_required", apiErr.Details["credential"])

			adtAssertAPIError(t, f.do(t, http.MethodPost, path, nil, adtBearer(cred.Token)), http.StatusForbidden, errCodeCredentialNotAdmitted)
			for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
				assert.Equal(t, http.StatusMethodNotAllowed, f.do(t, method, path, nil, adtBearer(f.session(t, f.alice))).Code, method)
			}
		})
	}
	rec := f.do(t, http.MethodPost, adtRevokeAllAgentPath(f.agentA.ID), nil, adtBearer(f.agentJWT(t, f.agentA)))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.False(t, f.grantRevoked(t, grant.ID), "no refused request revoked anything")
	assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentA.ID).Code)
}

// TestAgentDelegationRevokeAll_FailureRevokesNothing: every revocation of
// one call runs in one transaction. An audit write failure answers 500
// audit_failed, and a store failure on a later grant 500; in both cases no
// grant, no credential and no audit row is left revoked or written.
func TestAgentDelegationRevokeAll_FailureRevokesNothing(t *testing.T) {
	f := newADTFixture(t, "adt-revall-rollback")
	g1 := f.hubGrant(t)
	g2 := f.hubGrant(t)
	cred := f.delegated(t, g1.ID)
	alice := f.session(t, f.alice)
	assertNothingRevoked := func(t *testing.T) {
		t.Helper()
		assert.False(t, f.grantRevoked(t, g1.ID))
		assert.False(t, f.grantRevoked(t, g2.ID))
		assert.Empty(t, adtAudits(t, f.store, mutationAgentDelegationGrantRevoke))
		assert.Equal(t, http.StatusOK, f.getAgent(t, cred.Token, f.agentA.ID).Code, "the credential still works")
	}

	f.faults.inject(t, adtFaults{audit: errors.New("audit store down")})
	adtAssertAPIError(t, f.do(t, http.MethodPost, adtRevokeAllMine, nil, adtBearer(alice)), http.StatusInternalServerError, errCodeAuditFailed)
	f.faults.clear()
	assertNothingRevoked(t)

	// The second grant's lock fails after the first grant was revoked in
	// the same transaction.
	var locks atomic.Int32
	f.faults.inject(t, adtFaults{beforeGrantLock: func(context.Context, store.Store, string) error {
		if locks.Add(1) == 2 {
			return errors.New("grant store down")
		}
		return nil
	}})
	rec := f.do(t, http.MethodPost, adtRevokeAllAgentPath(f.agentA.ID), nil, adtBearer(alice))
	f.faults.clear()
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Equal(t, int32(2), locks.Load(), "the failure hit the second grant")
	assertNothingRevoked(t)

	assert.ElementsMatch(t, []string{g1.ID, g2.ID}, f.revokeAll(t, alice, adtRevokeAllMine).Revoked)
}

// TestAgentDelegationRevokeAll_WhileExperimentOffEndsGrantsForGood
// (§17.2 flow 39): both revoke-all routes answer while hub.agent_delegation
// is off, and the grants and credentials they revoke stay revoked when it
// is turned back on, while a grant revoked by neither works again.
func TestAgentDelegationRevokeAll_WhileExperimentOffEndsGrantsForGood(t *testing.T) {
	f := newADTFixture(t, "adt-revall-off")
	d, dave, _ := f.transferredAgent(t, "adt-revall-off")
	e, erin, _ := f.transferredAgent(t, "adt-revall-off-e")
	alice := f.session(t, f.alice)
	hubAdmin := f.session(t, newHubAdminRoleUser(t, f.store, "adt-revall-off-admin"))
	mine := f.hubGrant(t)
	mineCred := f.delegated(t, mine.ID)
	perAgent := f.grantFor(t, f.session(t, dave), d.ID)
	perAgentCred := f.credentialFor(t, d, perAgent.ID)
	keep := f.grantFor(t, f.session(t, erin), e.ID)

	adtSetExperiment(t, f.srv, false)
	assert.Equal(t, []string{mine.ID}, f.revokeAll(t, alice, adtRevokeAllMine).Revoked)
	assert.Equal(t, []string{perAgent.ID}, f.revokeAll(t, hubAdmin, adtRevokeAllAgentPath(d.ID)).Revoked)

	adtSetExperiment(t, f.srv, true)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, mineCred.Token, f.agentA.ID).Code)
	assert.Equal(t, http.StatusUnauthorized, f.getAgent(t, perAgentCred.Token, d.ID).Code)
	adtAssertAPIError(t, f.exchangeFor(t, f.agentA, mine.ID), http.StatusForbidden, errCodeGrantInactive)
	adtAssertAPIError(t, f.exchangeFor(t, d, perAgent.ID), http.StatusForbidden, errCodeGrantInactive)
	require.Equal(t, http.StatusOK, f.exchangeFor(t, e, keep.ID).Code, "a grant neither call revoked works again")
}
