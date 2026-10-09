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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mintSAID is the service account the mint tests' agents are assigned.
const mintSAID = "mint-sa-0001"

// assignModeChild creates a session child of the fixture user under chain
// ceiling c, whose applied identity assigns mintSAID.
func assignModeChild(t *testing.T, f provenanceFixture, name string, c store.EffectCeiling) *store.Agent {
	t.Helper()
	ag := f.userChild(t, name, c)
	ag.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: mintSAID}
	require.NoError(t, f.store.UpdateAgent(context.Background(), ag))
	stored, err := f.store.GetAgent(context.Background(), ag.ID)
	require.NoError(t, err)
	return stored
}

// mintRow records an assignment for agentID: the fixture user under a
// session credential and a principal ceiling for mintSAID, then mutate.
func mintRow(t *testing.T, s store.Store, agentID, userID string, mutate func(*store.AgentServiceAccountAssignment)) {
	t.Helper()
	a := &store.AgentServiceAccountAssignment{
		AgentID:             agentID,
		ServiceAccountID:    mintSAID,
		Origin:              store.SAAssignmentOriginUpdate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, userID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(context.Background(), a))
}

// withoutScope returns scopes less one scope.
func withoutScope(scopes []AgentTokenScope, drop AgentTokenScope) []AgentTokenScope {
	out := make([]AgentTokenScope, 0, len(scopes))
	for _, s := range scopes {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// assertGCPScope asserts whether the minted scopes hold the GCP token scope
// of mintSAID, that every other candidate scope is kept, and that the
// contract's scopes equal the minted scopes.
func assertGCPScope(t *testing.T, a *AuthzService, ag *store.Agent, want bool) {
	t.Helper()
	gcp := GCPTokenScopeForSA(mintSAID)
	candidates := a.mintCandidateScopes(ag)
	require.Contains(t, candidates, gcp, "precondition: the agent is in assign mode")
	minted, err := a.ceilingFilteredAgentScopes(context.Background(), ag, candidates)
	require.NoError(t, err)
	if want {
		assert.Contains(t, minted, gcp)
	} else {
		assert.NotContains(t, minted, gcp)
	}
	contract, _, err := a.EffectiveAgentAuthority(context.Background(), ag, AgentAuthorityOptions{})
	require.NoError(t, err)
	assert.Equal(t, minted, contract, "contract scopes equal minted scopes")
	chainOnly := filterScopes(candidates, mustChain(t, a, ag).Ceiling, ScopeCeilings{})
	assert.Equal(t, withoutScope(chainOnly, gcp), withoutScope(minted, gcp), "only the GCP token scope is affected")
}

func mustChain(t *testing.T, a *AuthzService, ag *store.Agent) ChainCeiling {
	t.Helper()
	c, err := a.chainEffectCeiling(context.Background(), ag)
	require.NoError(t, err)
	return c
}

func TestSAParentCeiling_GCPTokenScopeNeedsAssignmentCeiling(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*store.AgentServiceAccountAssignment) // nil with noRow: no assignment
		noRow  bool
		twoRow bool
		want   bool
	}{
		{name: "no row: chain rule only", noRow: true, want: true},
		{name: "principal row", want: true},
		{name: "bounded row with assign", want: true, mutate: func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialUAT
			a.EffectCeiling = boundedCeiling("agent.create", "gcp_service_account.assign")
		}},
		{name: "bounded row without assign", want: false, mutate: func(a *store.AgentServiceAccountAssignment) {
			a.SourceCredentialKind = store.SourceCredentialUAT
			a.EffectCeiling = boundedCeiling("agent.create")
		}},
		{name: "row for another account", want: false, mutate: func(a *store.AgentServiceAccountAssignment) {
			a.ServiceAccountID = "mint-sa-0002"
		}},
		{name: "row for a prefix of the account", want: false, mutate: func(a *store.AgentServiceAccountAssignment) {
			a.ServiceAccountID = mintSAID[:len(mintSAID)-1]
		}},
		{name: "provenance version 0", want: false, mutate: func(a *store.AgentServiceAccountAssignment) { a.ProvenanceVersion = 0 }},
		{name: "provenance version 2", want: false, mutate: func(a *store.AgentServiceAccountAssignment) { a.ProvenanceVersion = 2 }},
		{name: "unrecorded row", want: false, mutate: func(a *store.AgentServiceAccountAssignment) { a.EffectCeiling = store.EffectCeiling{} }},
		{name: "two active rows", twoRow: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProvenanceFixture(t, "gcpscope", false)
			ag := assignModeChild(t, f, "gcpscope", ceilPrincip)
			if !tc.noRow {
				mintRow(t, f.store, ag.ID, f.userID, tc.mutate)
			}
			a := f.a
			if tc.twoRow {
				a = f.authz(&spcTwoRowsStore{Store: f.store}, false, false)
			}
			assertGCPScope(t, a, ag, tc.want)
		})
	}
}

// TestSAParentCeiling_AssignmentCrossChain crosses the assignment rows with
// chain shapes: the GCP token scope is issued only when both the chain and
// the assignment allow gcp_service_account.assign, and the contract's scopes
// equal the minted scopes in every cell.
func TestSAParentCeiling_AssignmentCrossChain(t *testing.T) {
	chains := map[string]struct {
		c     store.EffectCeiling
		allow bool
	}{
		"principal chain":              {ceilPrincip, true},
		"bounded chain with assign":    {boundedCeiling(append(allRegistryIDs(), "gcp_service_account.assign")...), true},
		"bounded chain without assign": {boundedCeiling("project.read", "agent.create", "agent.status_update"), false},
	}
	rows := map[string]struct {
		mutate func(*store.AgentServiceAccountAssignment)
		noRow  bool
		allow  bool
	}{
		"no row":        {noRow: true, allow: true},
		"principal row": {allow: true},
		"bounded row with assign": {allow: true, mutate: func(a *store.AgentServiceAccountAssignment) {
			a.EffectCeiling = boundedCeiling("gcp_service_account.assign")
		}},
		"bounded row without": {allow: false, mutate: func(a *store.AgentServiceAccountAssignment) { a.EffectCeiling = boundedCeiling("agent.read") }},
		"other account":       {allow: false, mutate: func(a *store.AgentServiceAccountAssignment) { a.ServiceAccountID = "mint-sa-0002" }},
	}
	for cn, chain := range chains {
		for rn, row := range rows {
			t.Run(cn+"/"+rn, func(t *testing.T) {
				f := newProvenanceFixture(t, "cross", false)
				ag := assignModeChild(t, f, "cross", chain.c)
				if !row.noRow {
					mintRow(t, f.store, ag.ID, f.userID, row.mutate)
				}
				assertGCPScope(t, f.a, ag, chain.allow && row.allow)
				_, ceiling, err := f.a.EffectiveAgentAuthority(context.Background(), ag, AgentAuthorityOptions{})
				require.NoError(t, err)
				want, err := f.a.AgentEffectCeiling(context.Background(), ag.ID, AgentAuthorityOptions{})
				require.NoError(t, err)
				assert.Equal(t, want, ceiling, "the returned ceiling is the chain ceiling only")
			})
		}
	}
}

func devLocalMintRow(a *store.AgentServiceAccountAssignment) {
	a.AuthorityProvenance = recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal)
}

func schedulerDevLocalMintRow(a *store.AgentServiceAccountAssignment) {
	a.AuthorityProvenance = schedulerProv(store.DelegationPrincipalUser, DevUserID, store.InitiatorCredentialKindDevLocal)
}

// mintDevUser creates the local development user with status.
func mintDevUser(t *testing.T, s store.Store, status string) {
	t.Helper()
	ctx := context.Background()
	if u, err := s.GetUser(ctx, DevUserID); err == nil {
		u.Status = status
		require.NoError(t, s.UpdateUser(ctx, u))
		return
	}
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Dev", Role: "admin", Status: status}))
}

// devUserLookupStore counts GetUser calls for DevUserID and can fail them.
type devUserLookupStore struct {
	store.Store
	devCalls int
	fail     bool
}

func (s *devUserLookupStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == DevUserID {
		s.devCalls++
		if s.fail {
			return nil, errSPCInjected
		}
	}
	return s.Store.GetUser(ctx, id)
}

func TestSAParentCeiling_DevLocalAssignmentWithheldAtMintWhenDevAuthDisabled(t *testing.T) {
	for name, row := range map[string]func(*store.AgentServiceAccountAssignment){
		"dev_local row":  devLocalMintRow,
		"scheduler row":  schedulerDevLocalMintRow,
		"GetUser errors": devLocalMintRow,
	} {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "devoff", false)
			mintDevUser(t, f.store, store.UserStatusActive)
			ag := assignModeChild(t, f, "devoff", ceilPrincip)
			mintRow(t, f.store, ag.ID, f.userID, row)
			counting := &devUserLookupStore{Store: f.store, fail: name == "GetUser errors"}
			a := f.authz(counting, false, false)
			assertGCPScope(t, a, ag, false)
			assert.Zero(t, counting.devCalls, "dev authority off decides before any user lookup")
		})
	}
}

func TestSAParentCeiling_DevLocalAssignmentIssuedAtMintWhenDevAuthEnabled(t *testing.T) {
	for name, row := range map[string]func(*store.AgentServiceAccountAssignment){
		"dev_local row": devLocalMintRow,
		"scheduler row": schedulerDevLocalMintRow,
	} {
		t.Run(name, func(t *testing.T) {
			f := newProvenanceFixture(t, "devon", true)
			mintDevUser(t, f.store, store.UserStatusActive)
			ag := assignModeChild(t, f, "devon", ceilPrincip)
			mintRow(t, f.store, ag.ID, f.userID, row)
			assertGCPScope(t, f.a, ag, true)
		})
	}
}

func TestSAParentCeiling_DevLocalAssignmentWithheldAtMintWhenDevUserInactive(t *testing.T) {
	t.Run("dev user suspended", func(t *testing.T) {
		f := newProvenanceFixture(t, "devinactive", true)
		mintDevUser(t, f.store, "suspended")
		ag := assignModeChild(t, f, "devinactive", ceilPrincip)
		mintRow(t, f.store, ag.ID, f.userID, devLocalMintRow)
		assertGCPScope(t, f.a, ag, false)
	})
	t.Run("source is not the dev user", func(t *testing.T) {
		f := newProvenanceFixture(t, "devother", true)
		mintDevUser(t, f.store, store.UserStatusActive)
		ag := assignModeChild(t, f, "devother", ceilPrincip)
		mintRow(t, f.store, ag.ID, f.userID, func(a *store.AgentServiceAccountAssignment) {
			a.AuthorityProvenance = recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialDevLocal)
		})
		assertGCPScope(t, f.a, ag, false)
	})
	t.Run("dev user lookup error fails the mint", func(t *testing.T) {
		f := newProvenanceFixture(t, "deverr", true)
		mintDevUser(t, f.store, store.UserStatusActive)
		ag := assignModeChild(t, f, "deverr", ceilPrincip)
		mintRow(t, f.store, ag.ID, f.userID, devLocalMintRow)
		a := f.authz(&devUserLookupStore{Store: f.store, fail: true}, true, false)
		_, err := a.ceilingFilteredAgentScopes(context.Background(), ag, a.mintCandidateScopes(ag))
		require.Error(t, err)
		assert.ErrorIs(t, err, errSPCInjected)
		_, structural := ceilingDenyCauseForError(err)
		assert.False(t, structural, "a lookup fault is not a policy deny (503 at the mint sites)")
	})
}

// mintAssignErrStore fails the assignment lookup.
type mintAssignErrStore struct{ store.Store }

func (s *mintAssignErrStore) GetActiveAgentServiceAccountAssignments(context.Context, string) ([]store.AgentServiceAccountAssignment, error) {
	return nil, errSPCInjected
}

func TestSAParentCeiling_AssignmentLookupErrorFailsMint(t *testing.T) {
	f := newProvenanceFixture(t, "assignerr", false)
	ag := assignModeChild(t, f, "assignerr", ceilPrincip)
	a := f.authz(&mintAssignErrStore{Store: f.store}, false, false)
	_, err := a.ceilingFilteredAgentScopes(context.Background(), ag, a.mintCandidateScopes(ag))
	require.ErrorIs(t, err, errSPCInjected, "a lookup error is never read as no assignment")
	_, _, err = a.EffectiveAgentAuthority(context.Background(), ag, AgentAuthorityOptions{})
	assert.ErrorIs(t, err, errSPCInjected)
}

// TestSAParentCeiling_ContractMatchesMintWithAssignmentCeiling: a
// session-created child whose host-passthrough account was set by a
// principal whose recorded access token lacks gcp_service_account:assign.
// Mint and the contract both omit the account's token scope; the returned
// ceiling is the chain's (principal).
func TestSAParentCeiling_ContractMatchesMintWithAssignmentCeiling(t *testing.T) {
	f := newProvenanceFixture(t, "contract", false)
	ag := assignModeChild(t, f, "contract", ceilPrincip)
	mintRow(t, f.store, ag.ID, f.userID, func(a *store.AgentServiceAccountAssignment) {
		a.Origin = store.SAAssignmentOriginHostPassthroughTranslation
		a.AuthorityProvenance = recordedProv(store.DelegationPrincipalUser, tid("broker-owner"), store.SourceCredentialUAT)
		a.EffectCeiling = boundedCeiling("agent.create", "agent.read")
	})
	assertGCPScope(t, f.a, ag, false)
	_, ceiling, err := f.a.EffectiveAgentAuthority(context.Background(), ag, AgentAuthorityOptions{})
	require.NoError(t, err)
	assert.Equal(t, store.EffectCeilingPrincipal, ceiling.Kind)
}

// TestSAParentCeiling_RefreshIntersectsAssignmentCeiling drives the refresh
// handler: the GCP token scope follows the recorded assignment, and an agent
// with no recorded assignment keeps it.
func TestSAParentCeiling_RefreshIntersectsAssignmentCeiling(t *testing.T) {
	gcp := GCPTokenScopeForSA(mintSAID)
	for name, tc := range map[string]struct {
		mutate func(*store.AgentServiceAccountAssignment)
		noRow  bool
		want   bool
	}{
		"no row":              {noRow: true, want: true},
		"principal row":       {want: true},
		"bounded row without": {want: false, mutate: func(a *store.AgentServiceAccountAssignment) { a.EffectCeiling = boundedCeiling("agent.read") }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMintFixture(t, "refresh-sa")
			ag := f.agent(t, "refresh-sa", AgentRoleFull, state.PhaseRunning)
			ag.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: mintSAID}
			require.NoError(t, f.store.UpdateAgent(context.Background(), ag))
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip,
				recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession))
			if !tc.noRow {
				mintRow(t, f.store, ag.ID, f.userID, tc.mutate)
			}
			claims := f.tokenClaims(t, refreshedToken(t, f.refresh(t, ag, ag.Ancestry)))
			if tc.want {
				assert.Contains(t, claims.Scopes, gcp)
			} else {
				assert.NotContains(t, claims.Scopes, gcp)
			}
			assert.Contains(t, claims.Scopes, ScopeAgentCreate, "other scopes are kept")
		})
	}
}
