// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func uatProv(userID, tokenID string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser,
		SourcePrincipalID: userID, SourceCredentialKind: store.SourceCredentialUAT, SourceCredentialID: tokenID,
	}
}

// T8: a session-rooted agent already receives the artifact scopes at
// refresh, with no re-issue; a re-issue of it is a no-op.
func TestScopeReissue_T8_SessionRootedNeedsNoReissue(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-t8")
	a := f.agent(t, "rsu-t8-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))

	tok := refreshedToken(t, f.refresh(t, a, a.Ancestry))
	for _, s := range artifactScopeStrings() {
		assert.Contains(t, scopeStrings(f.tokenClaims(t, tok).Scopes), s, "refresh issues %s already", s)
	}

	edges := f.allEdges(t, a)
	resp := f.run(t, a, false)
	assert.True(t, resp.Noop)
	assert.Empty(t, resp.Added)
	assert.Empty(t, resp.Removed)
	assert.Equal(t, store.DelegationPrincipalUser, resp.CeilingSource.DelegatorKind)
	assert.Equal(t, string(store.SourceCredentialSession), resp.CeilingSource.SourceCredentialKind)
	assert.Equal(t, string(store.EffectCeilingPrincipal), resp.CeilingSource.CeilingKind)
	assert.Equal(t, edges, f.allEdges(t, a), "no-op writes nothing")
}

// Session-rooted clawback: a lowered project maximum lowers the role and
// removes the scopes.
func TestScopeReissue_SessionRootedClawback(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-claw")
	ctx := context.Background()
	a := f.agent(t, "rsu-claw-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
	project, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingMaxAgentRole] = string(AgentRoleBaseline)
	require.NoError(t, f.store.UpdateProject(ctx, project))

	resp := f.run(t, a, false)
	assert.Equal(t, "baseline", resp.RoleAfter)
	assert.Contains(t, resp.Removed, string(ScopeAgentCreate))
	assert.Contains(t, resp.Kept, string(ScopeProjectArtifactWrite))
	newEdge := f.activeEdge(t, a)
	assert.Equal(t, store.EffectCeilingPrincipal, newEdge.Kind)
	assert.Equal(t, store.SourceCredentialSession, newEdge.SourceCredentialKind)
	assert.Equal(t, string(AgentRoleBaseline), newEdge.Role)
}

// T7: an agent rooted in an access token whose ceiling predates the
// artifact permissions gains nothing: the token's frozen ceiling is never
// re-derived.
func TestScopeReissue_T7_LegacyUATRootedGainsNothing(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-t7")
	a := f.agent(t, "rsu-t7-agent", AgentRoleFull, state.PhaseRunning)
	tok, c := f.legacyUAT(t, "rsu-t7-uat")
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, c, uatProv(f.userID, tok.ID))

	resp := f.run(t, a, true)
	assert.Empty(t, resp.Added, "a legacy access token's ceiling grants no new scope")
	for _, s := range artifactScopeStrings() {
		assert.NotContains(t, resp.Kept, s)
		assert.Contains(t, resp.Withheld, reissueWithheldScope{Scope: s, Cause: reissueWithheldCeiling})
	}
	assert.Equal(t, string(store.SourceCredentialUAT), resp.CeilingSource.SourceCredentialKind)
	assert.Equal(t, tok.ID, resp.CeilingSource.SourceCredentialID)
	assert.Equal(t, string(store.EffectCeilingBounded), resp.CeilingSource.CeilingKind)
}

// A current access token (its ceiling includes the artifact permissions)
// whose agent's edge was recorded under the legacy ceiling cannot gain them
// either: the edge is re-derived from the token, never widened past it.
// With a token that does carry them, the re-issue equals creation today.
func TestScopeReissue_UATRootedEqualsTokenCeiling(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-uat")
	ctx := context.Background()
	a := f.agent(t, "rsu-uat-agent", AgentRoleFull, state.PhaseRunning)
	tok, legacy := f.legacyUAT(t, "rsu-uat-tok")
	// Widen the stored token's ceiling to the full coverage, then record
	// the agent's edge under the narrower legacy ceiling.
	full := boundedCeiling(agentScopeCoverage(ScopesForRole(AgentRoleFull))...).PermissionIDs
	tok.CeilingPermissionIDs = full
	require.NoError(t, f.store.DeleteUserAccessToken(ctx, tok.ID))
	require.NoError(t, f.store.CreateUserAccessToken(ctx, tok))
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, legacy, uatProv(f.userID, tok.ID))

	resp := f.run(t, a, false)
	assert.Equal(t, artifactScopeStrings(), reissueSorted(resp.Added))
	edge := f.activeEdge(t, a)
	assert.Equal(t, store.SourceCredentialUAT, edge.SourceCredentialKind)
	assert.Equal(t, tok.ID, edge.SourceCredentialID)
	assert.ElementsMatch(t, full, edge.PermissionIDs)
}

// T3 (user delegator): a revoked or expired source token, a missing token
// record, and an inactive delegator user each refuse the operation and
// write nothing but the denial.
func TestScopeReissue_UserDelegatorFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *reissueFixture, a *store.Agent)
		cause string
	}{
		{"revoked token", func(t *testing.T, f *reissueFixture, a *store.Agent) {
			tok, c := f.legacyUAT(t, "rsu-fc-revoked")
			f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, c, uatProv(f.userID, tok.ID))
			require.NoError(t, f.store.RevokeUserAccessToken(context.Background(), tok.ID))
		}, string(DenyCauseCeilingSourceNotAllowed)},
		{"missing token", func(t *testing.T, f *reissueFixture, a *store.Agent) {
			_, c := f.legacyUAT(t, "rsu-fc-missing")
			f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, c, uatProv(f.userID, tid("no-such-token")))
		}, string(DenyCauseCeilingSourceNotAllowed)},
		{"expired token", func(t *testing.T, f *reissueFixture, a *store.Agent) {
			ctx := context.Background()
			tok, c := f.legacyUAT(t, "rsu-fc-expired")
			f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, c, uatProv(f.userID, tok.ID))
			past := time.Now().Add(-time.Hour)
			tok.ExpiresAt = &past
			require.NoError(t, f.store.DeleteUserAccessToken(ctx, tok.ID))
			require.NoError(t, f.store.CreateUserAccessToken(ctx, tok))
		}, string(DenyCauseCeilingSourceNotAllowed)},
		{"inactive user", func(t *testing.T, f *reissueFixture, a *store.Agent) {
			f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
			u, err := f.store.GetUser(context.Background(), f.userID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(context.Background(), u))
		}, string(DenyCauseCeilingDelegatorLacksPermission)},
		{"inactive local development user", func(t *testing.T, f *reissueFixture, a *store.Agent) {
			ctx := context.Background()
			f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, store.AuthorityProvenance{
				ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser,
				SourcePrincipalID: DevUserID, SourceCredentialKind: store.SourceCredentialDevLocal,
			})
			u, err := f.store.GetUser(ctx, DevUserID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(ctx, u))
		}, string(DenyCauseCeilingDelegatorLacksPermission)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUserReissueFixture(t, "rsu-fc-"+itoa(i))
			a := f.agent(t, "rsu-fc-agent-"+itoa(i), AgentRoleFull, state.PhaseRunning)
			tc.setup(t, f, a)
			assertUserRefusalWritesNothing(t, f, a, http.StatusForbidden, tc.cause)
		})
	}
}

// A dev_local-rooted agent re-issues from the local development user
// while local development authority is enabled.
func TestScopeReissue_DevLocalRooted(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-dev")
	require.True(t, f.srv.authzService.devLocalAuthorityEnabled(), "test server enables dev auth")
	a := f.agent(t, "rsu-dev-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, store.AuthorityProvenance{
		ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser,
		SourcePrincipalID: DevUserID, SourceCredentialKind: store.SourceCredentialDevLocal,
	})
	resp := f.run(t, a, true)
	assert.True(t, resp.Noop)
	assert.Equal(t, string(store.SourceCredentialDevLocal), resp.CeilingSource.SourceCredentialKind)

	f.srv.authzService.devLocalEnabled = false
	_, err := f.srv.runScopeReissue(context.Background(), f.reload(t, a), f.operator, true, "")
	require.Error(t, err)
	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteReissue, string(DenyCauseCeilingSourceNotAllowed))
}

// Condition 2 for a user delegator: a lookup fault on one covered
// permission withholds that scope, and the principal ceiling is recorded
// as a bounded one without the permission so refresh agrees.
func TestScopeReissue_UserDelegatorPerScopeWithhold(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-t4")
	a := f.agent(t, "rsu-t4-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
	prev := reissueLiveCheckFault
	reissueLiveCheckFault = func(perm string) error {
		if perm == "artifact.create" {
			return assert.AnError
		}
		return nil
	}
	t.Cleanup(func() { reissueLiveCheckFault = prev })

	resp := f.run(t, a, false)
	assert.Equal(t, []string{string(ScopeProjectArtifactWrite)}, resp.Removed)
	assert.Contains(t, resp.Withheld, reissueWithheldScope{Scope: string(ScopeProjectArtifactWrite), Cause: reissueWithheldLookup})
	edge := f.activeEdge(t, a)
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
	assert.NotContains(t, edge.PermissionIDs, "artifact.create")
	assert.NotContains(t, scopeStrings(f.grant(t, a)), string(ScopeProjectArtifactWrite))
}

// The re-issued set equals what agent creation by the same user issues
// today, for a session-rooted agent: same scopes and same ceiling.
func TestScopeReissue_SessionRootedEqualsCreateToday(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-eq")
	ctx := context.Background()
	a := f.agent(t, "rsu-eq-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
	f.run(t, a, false)
	reissued := f.grant(t, a)

	user, err := f.store.GetUser(ctx, f.userID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents",
		CreateAgentRequest{Name: "rsu-eq-sibling", AgentRole: string(AgentRoleFull)})
	require.Less(t, rec.Code, 300, rec.Body.String())
	sibling, err := f.store.GetAgentBySlug(ctx, f.projectID, "rsu-eq-sibling")
	require.NoError(t, err)

	assert.ElementsMatch(t, f.grant(t, sibling), reissued, "re-issue equals create-today")
	assert.Contains(t, scopeStrings(reissued), string(ScopeProjectArtifactWrite))
	siblingCeiling, agentCeiling := f.activeEdge(t, sibling).EffectCeiling, f.activeEdge(t, a).EffectCeiling
	assert.True(t, effectCeilingsEqual(siblingCeiling, agentCeiling), "created %+v, re-issued %+v", siblingCeiling, agentCeiling)
	assert.Equal(t, store.SourceCredentialSession, f.activeEdge(t, sibling).SourceCredentialKind)
}

// assertUserRefusalWritesNothing runs a re-issue of a that must be refused
// with code and cause, and checks that it changed nothing: no edge change,
// the agent's credential still active, the stored role unchanged, no
// agent_scopes_reissued row, and one scope-free denial row.
func assertUserRefusalWritesNothing(t *testing.T, f *reissueFixture, a *store.Agent, code int, cause string) {
	t.Helper()
	jti := a.ID + "-jti"
	insertTestAgentCredential(t, f.store, a.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	edges := f.allEdges(t, a)
	roleBefore, _ := agentRoleAndScopes(f.reload(t, a))
	child := f.reload(t, a)
	if f.faults != nil && (f.faults.failUserID != "" || f.faults.uatReadErr) {
		f.faults.arm()
	}

	_, err := f.srv.runScopeReissue(context.Background(), child, f.operator, false, "")
	require.Error(t, err)
	rec := httptest.NewRecorder()
	writeScopeReissueError(rec, err)
	assert.Equal(t, code, rec.Code, rec.Body.String())
	assert.Equal(t, edges, f.allEdges(t, a), "no edge change")
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	roleAfter, _ := agentRoleAndScopes(f.reload(t, a))
	assert.Equal(t, roleBefore, roleAfter, "stored role unchanged")
	assert.False(t, f.client.resetAuthCalled, "nothing pushed")
	assert.Empty(t, reissueAudits(t, f.store, a.ID, mutationTypeAgentScopesReissued))
	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteReissue, cause)
}

// A lookup error on the user path (the delegator user or the recorded
// access token) refuses with 503 and writes nothing.
func TestScopeReissue_UserDelegatorLookupFault(t *testing.T) {
	t.Run("user lookup", func(t *testing.T) {
		f := newUserReissueFixture(t, "rsu-lf-user")
		a := f.agent(t, "rsu-lf-user-agent", AgentRoleFull, state.PhaseRunning)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
		f.faults.failUserID = f.userID
		assertUserRefusalWritesNothing(t, f, a, http.StatusServiceUnavailable, mintErrorClassLookup)
	})
	t.Run("access token lookup", func(t *testing.T) {
		f := newUserReissueFixture(t, "rsu-lf-uat")
		a := f.agent(t, "rsu-lf-uat-agent", AgentRoleFull, state.PhaseRunning)
		tok, c := f.legacyUAT(t, "rsu-lf-uat-tok")
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, c, uatProv(f.userID, tok.ID))
		f.faults.uatReadErr = true
		assertUserRefusalWritesNothing(t, f, a, http.StatusServiceUnavailable, mintErrorClassLookup)
	})
}

// A session-rooted re-issue that changes the agent (the project maximum
// was lowered to baseline) still equals what the same user's create issues
// today at that role.
func TestScopeReissue_SessionRootedEqualsCreateTodayAfterChange(t *testing.T) {
	f := newUserReissueFixture(t, "rsu-eqc")
	ctx := context.Background()
	a := f.agent(t, "rsu-eqc-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
	project, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingMaxAgentRole] = string(AgentRoleBaseline)
	require.NoError(t, f.store.UpdateProject(ctx, project))

	resp := f.run(t, a, false)
	require.False(t, resp.Noop)
	assert.Equal(t, "baseline", resp.RoleAfter)
	reissued := f.grant(t, a)

	user, err := f.store.GetUser(ctx, f.userID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents",
		CreateAgentRequest{Name: "rsu-eqc-sibling", AgentRole: string(AgentRoleBaseline)})
	require.Less(t, rec.Code, 300, rec.Body.String())
	sibling, err := f.store.GetAgentBySlug(ctx, f.projectID, "rsu-eqc-sibling")
	require.NoError(t, err)
	siblingRole, _ := agentRoleAndScopes(sibling)
	assert.Equal(t, AgentRoleBaseline, siblingRole)

	assert.ElementsMatch(t, f.grant(t, sibling), reissued, "re-issue equals create-today after the change")
	assert.NotContains(t, scopeStrings(reissued), string(ScopeAgentCreate), "full-only scopes removed")
	siblingCeiling, agentCeiling := f.activeEdge(t, sibling).EffectCeiling, f.activeEdge(t, a).EffectCeiling
	assert.True(t, effectCeilingsEqual(siblingCeiling, agentCeiling), "created %+v, re-issued %+v", siblingCeiling, agentCeiling)
	assert.Equal(t, string(AgentRoleBaseline), f.activeEdge(t, a).Role)
}
