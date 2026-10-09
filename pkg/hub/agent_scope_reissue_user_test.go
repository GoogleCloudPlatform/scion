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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userReissueFixture is a mint fixture with production minting and the
// backfill marker set.
func newUserReissueFixture(t *testing.T, name string) *reissueFixture {
	t.Helper()
	f := newMintFixture(t, name)
	setBackfillCompleted(t, f.store)
	f.srv.authzService.mintDevAuthOverride = false
	return &reissueFixture{
		mintFixture: f,
		operator:    reissueOperator{UserID: DevUserID, CredentialKind: store.InitiatorCredentialKindSession},
	}
}

func sessionProv(userID string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser,
		SourcePrincipalID: userID, SourceCredentialKind: store.SourceCredentialSession,
	}
}

// legacyUAT stores a project-bounded access token of the fixture user whose
// frozen ceiling is the legacy (pre-artifact) full-role coverage, and
// returns it with the edge ceiling creation records from it.
func (f *reissueFixture) legacyUAT(t *testing.T, id string) (*store.UserAccessToken, store.EffectCeiling) {
	t.Helper()
	ctx := context.Background()
	exp := time.Now().Add(24 * time.Hour)
	tok := &store.UserAccessToken{
		ID: tid(id), UserID: f.userID, Name: id, Prefix: "scion_pat_", KeyHash: "hash-" + id,
		BoundaryKind: string(permissions.BoundaryKindProject), ProjectID: f.projectID,
		Scopes:               []string{"project:agent:manage"},
		CeilingVersion:       permissions.CeilingVersionV1,
		CeilingPermissionIDs: legacyBoundedCeiling().PermissionIDs,
		ExpiresAt:            &exp, Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUserAccessToken(ctx, tok))
	u, err := f.store.GetUser(ctx, f.userID)
	require.NoError(t, err)
	identity := NewScopedUserIdentityWithBoundaryAndDecoration(
		NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeAPI)),
		TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}, tok.Scopes, tok.ID, tok.NormalizedCeiling(), nil)
	c, _, err := f.srv.authzService.sourceEffectCeiling(ctx, identity)
	require.NoError(t, err)
	return tok, c
}

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
		{"inactive user", func(t *testing.T, f *reissueFixture, a *store.Agent) {
			f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
			u, err := f.store.GetUser(context.Background(), f.userID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(context.Background(), u))
		}, string(DenyCauseCeilingDelegatorLacksPermission)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUserReissueFixture(t, "rsu-fc-"+itoa(i))
			a := f.agent(t, "rsu-fc-agent-"+itoa(i), AgentRoleFull, state.PhaseRunning)
			tc.setup(t, f, a)
			edges := f.allEdges(t, a)

			_, err := f.srv.runScopeReissue(context.Background(), f.reload(t, a), f.operator, false, "")
			require.Error(t, err)
			rec := httptest.NewRecorder()
			writeScopeReissueError(rec, err)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, edges, f.allEdges(t, a))
			assert.Empty(t, reissueAudits(t, f.store, a.ID, mutationTypeAgentScopesReissued))
			assertIssueDeniedAudit(t, f.store, a.ID, mintSiteReissue, tc.cause)
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
