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

// Package hub — the runtime user-scope secret read (check 8) and the agent
// secret list under the agent's delegation chain: a chain with recorded
// provenance is bounded by its recorded effect ceilings and the
// user-scope secret parent ceiling; a chain without recorded provenance is
// authorized by the progeny relationship alone. The by-key read and the
// list agree on every shape.
package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const userScopeCeilingKey = "USER_SCOPE_CEILING_KEY"

// userScopeCeilingFixture is a material fixture in production-like state:
// the delegation-edge backfill is complete, the agent has one recorded edge
// from its root user (session source, project scope, full role) carrying the
// given effect ceiling, and the root has shared a user-scope secret with its
// progeny (AllowProgeny, CreatedBy the root).
type userScopeCeilingFixture struct {
	*materialFixture
	// mixedParent is the upper agent of the chain mixedChain builds.
	mixedParent string
}

func newUserScopeCeilingFixture(t *testing.T, name string, ceiling store.EffectCeiling) *userScopeCeilingFixture {
	t.Helper()
	f := newUserScopeChainFixture(t, name)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, ceiling, nil)
	return f
}

// newUserScopeChainFixture is the fixture with the backfill complete and the
// shared secret, but no edge for the agent yet.
func newUserScopeChainFixture(t *testing.T, name string) *userScopeCeilingFixture {
	t.Helper()
	f := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, name)}
	setBackfillCompleted(t, f.Store)
	f.shareUserSecret(t, f.UserID, userScopeCeilingKey, f.UserID)
	return f
}

func (f *userScopeCeilingFixture) shareUserSecret(t *testing.T, ownerID, key, createdBy string) {
	t.Helper()
	_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
		Name: key, Value: "v", SecretType: store.SecretTypeEnvironment, Target: key,
		Scope: store.ScopeUser, ScopeID: ownerID, AllowProgeny: true, CreatedBy: createdBy, UpdatedBy: createdBy,
	})
	require.NoError(t, err)
}

// recordEdge records an active edge from delegator to delegate in the
// fixture project, with version 1 provenance whose source is the delegator.
// mutate, when non-nil, changes the edge before it is stored.
func (f *userScopeCeilingFixture) recordEdge(t *testing.T, delegatorType, delegatorID, delegateID string, ceiling store.EffectCeiling, mutate func(*store.DelegationEdge)) {
	t.Helper()
	cred := store.SourceCredentialSession
	if delegatorType == store.DelegationPrincipalAgent {
		cred = store.SourceCredentialAgent
	}
	e := &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
		ScopeType: store.RoleScopeProject, ScopeID: f.ProjectID, Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: cred,
		},
		EffectCeiling: ceiling,
	}
	if mutate != nil {
		mutate(e)
	}
	require.NoError(t, f.Store.CreateDelegationEdge(context.Background(), e))
}

// childAgent stores an agent in the fixture project with the given ancestry
// and role, and returns its ID and a recorded token for it.
func (f *userScopeCeilingFixture) childAgent(t *testing.T, name string, ancestry []string, role AgentRole) (string, string) {
	t.Helper()
	id := tid("usc-agent-" + name)
	require.NoError(t, f.Store.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "slug-" + id[:8], Name: "name-" + id[:8], ProjectID: f.ProjectID, Phase: "running",
		CreatedBy: ancestry[len(ancestry)-1], OwnerID: ancestry[0], StateVersion: 1,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)}, Ancestry: ancestry,
	}))
	return id, mintRecordedMaterialToken(t, f.Server, f.Store, id, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, ancestry)
}

// mixedChain builds a chain that mixes recorded and unrecorded hops: an
// agent P (ancestry [U]) whose edge from U carries no recorded provenance,
// and below it an agent C (ancestry [U, P]) with a recorded edge from P
// carrying ceiling. It returns C's ID and token; P's ID is f.mixedParent.
func (f *userScopeCeilingFixture) mixedChain(t *testing.T, name string, ceiling store.EffectCeiling) (string, string) {
	t.Helper()
	parent, _ := f.childAgent(t, name+"-parent", []string{f.UserID}, AgentRoleFull)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, parent,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	child, token := f.childAgent(t, name+"-child", []string{f.UserID, parent}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalAgent, parent, child, ceiling, nil)
	f.mixedParent = parent
	return child, token
}

// decideSecretUse is the decision check 8 makes for a recorded chain:
// secret.use with ActionUse for the agent's token on the shared item.
func (f *userScopeCeilingFixture) decideSecretUse(t *testing.T) Decision {
	t.Helper()
	ctx := context.Background()
	meta, err := f.Server.secretBackend.GetMeta(ctx, userScopeCeilingKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	require.NotNil(t, meta)
	agent := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	return f.Server.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   userSecretResource(meta.ID, meta.ScopeID),
		Action:     ActionUse,
		Permission: "secret.use",
		Explain:    true,
	})
}

// readStatus is the runtime read through the by-key endpoint (check 8).
func (f *userScopeCeilingFixture) readStatus(t *testing.T) (int, string) {
	t.Helper()
	return f.readAs(t, f.AgentID, f.Token, userScopeCeilingKey)
}

func (f *userScopeCeilingFixture) readAs(t *testing.T, agentID, token, key string) (int, string) {
	t.Helper()
	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
		"/api/v1/agents/"+agentID+"/secrets/"+key+"?scope=user", nil, token)
	return rec.Code, rec.Body.String()
}

// listKeys is the agent secret list; scope is "" (both scopes) or "user".
func (f *userScopeCeilingFixture) listKeys(t *testing.T, agentID, token, scope string) (int, []string) {
	t.Helper()
	path := "/api/v1/agents/" + agentID + "/secrets"
	if scope != "" {
		path += "?scope=" + scope
	}
	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, path, nil, token)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var resp AgentListSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	keys := make([]string, 0, len(resp.Secrets))
	for _, s := range resp.Secrets {
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	return rec.Code, keys
}

func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// assertReadAndListAgree asserts the by-key read and the user list agree on
// key for the agent, and that the read's outcome is want (allowed or not).
func (f *userScopeCeilingFixture) assertReadAndListAgree(t *testing.T, agentID, token, key string, want bool) {
	t.Helper()
	code, body := f.readAs(t, agentID, token, key)
	read := code == http.StatusOK
	require.Equal(t, want, read, "read %d: %s", code, body)
	for _, scope := range []string{"", store.ScopeUser} {
		lc, keys := f.listKeys(t, agentID, token, scope)
		require.Equal(t, http.StatusOK, lc, "list scope %q", scope)
		assert.Equal(t, read, containsKey(keys, key), "list scope %q must agree with the read (%d): %v", scope, code, keys)
	}
}

// TestAgentSecretRead_UserScopeCeilingApplied: a user-scope read by an
// agent with recorded provenance is bounded by the agent's delegation
// ceiling. An agent whose recorded edge carries a principal ceiling is
// admitted, both by Decide(secret.use, ActionUse) and by the runtime read;
// one whose bounded ceiling leaves out secret.use is denied by that ceiling
// (ceiling_effect_exceeded) and the runtime read is refused.
func TestAgentSecretRead_UserScopeCeilingApplied(t *testing.T) {
	principal := newUserScopeCeilingFixture(t, "usc-principal", store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
	d := principal.decideSecretUse(t)
	require.True(t, d.Allowed, "principal ceiling: the shared user secret must stay usable; reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	require.True(t, r.Accepted)
	code, body := principal.readStatus(t)
	require.Equal(t, http.StatusOK, code, "principal ceiling: the runtime read is admitted: %s", body)

	bounded := newUserScopeCeilingFixture(t, "usc-bounded", boundedCeiling("project.read", "agent.read"))
	d = bounded.decideSecretUse(t)
	require.False(t, d.Allowed, "bounded ceiling without secret.use must deny")
	require.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
	require.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause,
		"the bounded ceiling itself denies, not the delegator check; reason %q", d.Reason)
	code, body = bounded.readStatus(t)
	require.Equal(t, http.StatusNotFound, code, "bounded ceiling: the runtime read is refused: %s", body)

	withUse := newUserScopeCeilingFixture(t, "usc-bounded-use", boundedCeiling("project.read", "project.secret_read", "secret.use"))
	code, body = withUse.readStatus(t)
	require.Equal(t, http.StatusOK, code, "a bounded ceiling holding secret.use admits: %s", body)
}

// TestAgentSecretRead_UnrecordedChainKeepsProgenyRule pins that a chain
// without recorded provenance is authorized by the progeny relationship
// alone: each of these shapes reads and lists the shared secret.
func TestAgentSecretRead_UnrecordedChainKeepsProgenyRule(t *testing.T) {
	cases := []struct {
		name     string
		backfill bool
		setup    func(t *testing.T, f *userScopeCeilingFixture)
	}{
		{"no edge before the backfill", false, func(t *testing.T, f *userScopeCeilingFixture) {}},
		{"no edge after the backfill", true, func(t *testing.T, f *userScopeCeilingFixture) {}},
		{"edge without provenance", true, func(t *testing.T, f *userScopeCeilingFixture) {
			createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, f.AgentID,
				store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
		}},
		{"unrecorded ceiling, known version", true, func(t *testing.T, f *userScopeCeilingFixture) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}, nil)
		}},
		{"unknown provenance version", true, func(t *testing.T, f *userScopeCeilingFixture) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.ProvenanceVersion = 2
			})
		}},
		{"migration sentinel", true, func(t *testing.T, f *userScopeCeilingFixture) {
			f.recordEdge(t, store.DelegationPrincipalUser, migrationDelegatorID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}, func(e *store.DelegationEdge) {
				e.ProvenanceVersion = 0
			})
		}},
		{"recorded principal that does not type-check", true, func(t *testing.T, f *userScopeCeilingFixture) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.SourcePrincipalKind = store.DelegationPrincipalAgent
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, "uscl-"+strings.ReplaceAll(tc.name, " ", "-"))}
			if tc.backfill {
				setBackfillCompleted(t, f.Store)
			}
			f.shareUserSecret(t, f.UserID, userScopeCeilingKey, f.UserID)
			tc.setup(t, f)

			chain := f.Server.authzService.userSecretChainVerdict(context.Background(),
				newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead}), nil)
			require.False(t, chain.decision.Allowed)
			require.True(t, chain.unrecorded, "the chain has no recorded provenance: %q", chain.decision.Reason)

			f.assertReadAndListAgree(t, f.AgentID, f.Token, userScopeCeilingKey, true)
		})
	}

	// A recorded hop below a hop without recorded provenance: the recorded
	// hop passes, the walk then reaches the unrecorded hop, so the chain is
	// not recorded and the progeny rule applies. A failing recorded hop in
	// the same position is refused instead
	// (TestAgentSecretRead_MixedChainEnforcesRecordedHops).
	t.Run("recorded hop below an unrecorded hop", func(t *testing.T) {
		f := newUserScopeChainFixture(t, "uscl-mixed")
		child, token := f.mixedChain(t, "mixed", store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
		chain := f.Server.authzService.userSecretChainVerdict(context.Background(),
			newFullAgentIdentity(child, f.ProjectID, []string{f.UserID, f.mixedParent}, []AgentTokenScope{ScopeProjectSecretRead}), nil)
		require.False(t, chain.decision.Allowed)
		assert.True(t, chain.unrecorded, "the deny is the unrecorded upper hop: %q", chain.decision.Reason)
		assert.Equal(t, DenyCauseCeilingUnrecorded, chain.decision.DenyCause, "reason %q", chain.decision.Reason)
		f.assertReadAndListAgree(t, child, token, userScopeCeilingKey, true)
	})

	// The progeny rule still applies to these chains: an unshared secret is
	// not returned.
	t.Run("unshared secret", func(t *testing.T) {
		f := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, "uscl-unshared")}
		setBackfillCompleted(t, f.Store)
		_, _, err := f.Server.secretBackend.Set(context.Background(), &secret.SetSecretInput{
			Name: userScopeCeilingKey, Value: "v", SecretType: store.SecretTypeEnvironment, Target: userScopeCeilingKey,
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: false, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		f.assertReadAndListAgree(t, f.AgentID, f.Token, userScopeCeilingKey, false)
	})
}

// TestAgentSecretRead_MixedChainEnforcesRecordedHops pins the boundary for
// a chain that mixes recorded and unrecorded hops. The chain verdict stops
// at its first failing clause, so a recorded hop below the first unrecorded
// hop is enforced: S3b, S3c and S3d run on it before the walk reaches the
// unrecorded hop. When that recorded hop fails, the read and the list
// refuse the secret and the chain is not classed unrecorded. An unrecorded
// hop by itself never refuses (the passing case is "recorded hop below an
// unrecorded hop" in TestAgentSecretRead_UnrecordedChainKeepsProgenyRule).
func TestAgentSecretRead_MixedChainEnforcesRecordedHops(t *testing.T) {
	cases := []struct {
		name    string
		ceiling store.EffectCeiling
		after   func(t *testing.T, f *userScopeCeilingFixture)
		cause   DenyCause
	}{
		{"recorded bounded hop without secret.use", boundedCeiling("project.read", "agent.read", "project.secret_read"),
			func(t *testing.T, f *userScopeCeilingFixture) {}, DenyCauseCeilingEffectExceeded},
		{"upper agent held", store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			func(t *testing.T, f *userScopeCeilingFixture) {
				holdAgent(t, f.Store, f.mixedParent, f.ProjectID, f.UserID)
			}, DenyCauseCeilingOrphaned},
		{"upper agent role without the scope", store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			func(t *testing.T, f *userScopeCeilingFixture) {
				setAgentRole(t, f.Store, f.mixedParent, AgentRoleNone)
			}, DenyCauseCeilingDelegatorLacksPermission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "uscm-" + strings.ReplaceAll(tc.name, " ", "-")
			f := newUserScopeChainFixture(t, name)
			child, token := f.mixedChain(t, name, tc.ceiling)
			tc.after(t, f)

			chain := f.Server.authzService.userSecretChainVerdict(context.Background(),
				newFullAgentIdentity(child, f.ProjectID, []string{f.UserID, f.mixedParent}, []AgentTokenScope{ScopeProjectSecretRead}), nil)
			require.False(t, chain.decision.Allowed)
			assert.False(t, chain.unrecorded, "the deny is the recorded lower hop: %q", chain.decision.Reason)
			assert.Equal(t, tc.cause, chain.decision.DenyCause, "reason %q", chain.decision.Reason)

			f.assertReadAndListAgree(t, child, token, userScopeCeilingKey, false)
		})
	}
}

// TestUserSecretWriters_RecordScopeOwnerAsCreator (T13b): every writer of a
// user-scope secret records the scope owner as the creator (CreatedBy ==
// ScopeID), the attribution S7 relies on. The writers are the user secret
// endpoint, the user env endpoint with secret storage, and the agent
// secret endpoint (harness credential capture); TestUserSecretWriters_Inventory
// keeps the list complete.
func TestUserSecretWriters_RecordScopeOwnerAsCreator(t *testing.T) {
	f := newUserScopeChainFixture(t, "uscw-writers")
	ctx := context.Background()
	assertOwnerCreated := func(t *testing.T, key, scopeID string) {
		t.Helper()
		meta, err := f.Server.secretBackend.GetMeta(ctx, key, store.ScopeUser, scopeID)
		require.NoError(t, err)
		require.NotNil(t, meta)
		assert.Equal(t, scopeID, meta.ScopeID)
		assert.Equal(t, meta.ScopeID, meta.CreatedBy, "%s: the creator is the scope owner", key)
	}

	t.Run("user secret endpoint", func(t *testing.T) {
		rec := doRequest(t, f.Server, http.MethodPut, "/api/v1/secrets/USCW_USER_SECRET", SetSecretRequest{
			Value: base64.StdEncoding.EncodeToString([]byte("v")), Scope: store.ScopeUser, AllowProgeny: true,
		})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
		assertOwnerCreated(t, "USCW_USER_SECRET", DevUserID)
	})
	t.Run("user env endpoint with secret storage", func(t *testing.T) {
		rec := doRequest(t, f.Server, http.MethodPut, "/api/v1/env/USCW_USER_ENV_SECRET", SetEnvVarRequest{
			Value: "v", Scope: store.ScopeUser, Secret: true,
		})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
		assertOwnerCreated(t, "USCW_USER_ENV_SECRET", DevUserID)
	})
	t.Run("agent secret endpoint", func(t *testing.T) {
		allow := true
		rec := doRequestWithAgentToken(t, f.Server, http.MethodPut, "/api/v1/agents/"+f.AgentID+"/secrets/USCW_AGENT_SECRET",
			AgentSetSecretRequest{Value: base64.StdEncoding.EncodeToString([]byte("v")), Scope: store.ScopeUser, AllowProgeny: &allow}, f.Token)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assertOwnerCreated(t, "USCW_AGENT_SECRET", f.UserID)
	})
}

// TestAgentSecretRead_UserScopeMultiHopBounded: user U1 delegates to agent
// A1 (principal ceiling), A1 creates A2 with a bounded ceiling that leaves
// out secret.use, and A2 reads U1's shared user-scope secret. The read and
// the list both refuse it; A1 itself still reads it. A2 has no way to name
// another user's scope: the read and the list resolve user-scope keys only
// in the scope of A2's ancestry root, so another user's shared secret with
// the same or a different key is never returned, to A2 or to A1.
func TestAgentSecretRead_UserScopeMultiHopBounded(t *testing.T) {
	f := newUserScopeChainFixture(t, "usc-reach")
	a1, a1Token := f.childAgent(t, "reach-a1", []string{f.UserID}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, a1, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	a2, a2Token := f.childAgent(t, "reach-a2", []string{f.UserID, a1}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalAgent, a1, a2, boundedCeiling("project.read", "agent.read", "project.secret_read"), nil)

	f.assertReadAndListAgree(t, a1, a1Token, userScopeCeilingKey, true)
	f.assertReadAndListAgree(t, a2, a2Token, userScopeCeilingKey, false)

	// The deny is the recorded ceiling of A2's own hop.
	ident := newFullAgentIdentity(a2, f.ProjectID, []string{f.UserID, a1}, []AgentTokenScope{ScopeProjectSecretRead})
	chain := f.Server.authzService.userSecretChainVerdict(context.Background(), ident, nil)
	require.False(t, chain.decision.Allowed)
	assert.False(t, chain.unrecorded)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, chain.decision.DenyCause, "reason %q", chain.decision.Reason)

	// Another user U2 in the same project shares secrets with their own
	// progeny, one under the same key as U1's.
	u2 := tid("usc-reach-u2")
	createDCUser(t, f.Store, u2, "usc-reach-u2@test.com", f.ProjectID, store.ProjectRoleOwner)
	f.shareUserSecret(t, u2, "USC_REACH_U2_KEY", u2)
	f.shareUserSecret(t, u2, userScopeCeilingKey, u2)
	for _, who := range []struct{ id, token string }{{a1, a1Token}, {a2, a2Token}} {
		code, body := f.readAs(t, who.id, who.token, "USC_REACH_U2_KEY")
		assert.Equal(t, http.StatusNotFound, code, "another user's secret is never resolved: %s", body)
		for _, scope := range []string{"", store.ScopeUser} {
			_, keys := f.listKeys(t, who.id, who.token, scope)
			assert.False(t, containsKey(keys, "USC_REACH_U2_KEY"), "list scope %q: %v", scope, keys)
		}
	}
	// A1's read of the shared key still resolves U1's row (the value comes
	// from the ancestry root's scope), and A2's stays refused.
	code, _ := f.readAs(t, a1, a1Token, userScopeCeilingKey)
	assert.Equal(t, http.StatusOK, code)
	code, _ = f.readAs(t, a2, a2Token, userScopeCeilingKey)
	assert.Equal(t, http.StatusNotFound, code)

	// At the evaluator, a request naming U2 as the parent finds no source in
	// A1's lineage.
	meta, err := f.Server.secretBackend.GetMeta(context.Background(), "USC_REACH_U2_KEY", store.ScopeUser, u2)
	require.NoError(t, err)
	a1Ident := newFullAgentIdentity(a1, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	d := f.Server.authzService.EvaluateUserSecretParentCeiling(context.Background(), a1Ident, userSecretResource(meta.ID, u2))
	require.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingResourceMissing, d.DenyCause)
}

// TestAgentSecretList_AgreesWithReadForRecordedChains (T31): for each shape,
// the list includes the key exactly when the by-key read returns it.
func TestAgentSecretList_AgreesWithReadForRecordedChains(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, f *userScopeCeilingFixture) (string, string, string)
		want  bool
	}{
		{"principal chain", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return f.AgentID, f.Token, userScopeCeilingKey
		}, true},
		{"bounded without secret.use", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, boundedCeiling("project.read"), nil)
			return f.AgentID, f.Token, userScopeCeilingKey
		}, false},
		{"intermediate agent held", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			parent, _ := f.childAgent(t, "held-parent", []string{f.UserID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, parent, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			child, token := f.childAgent(t, "held-child", []string{f.UserID, parent}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, parent, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			holdAgent(t, f.Store, parent, f.ProjectID, f.UserID)
			return child, token, userScopeCeilingKey
		}, false},
		{"edge source differs from the ancestry root", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			other := tid("usct-other-root")
			createDCUser(t, f.Store, other, "usct-other-root@test.com", f.ProjectID, store.ProjectRoleOwner)
			f.recordEdge(t, store.DelegationPrincipalUser, other, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return f.AgentID, f.Token, userScopeCeilingKey
		}, false},
		{"agent under an access constraint", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			_, err := f.Store.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
				Name: "usct-agent", SubjectKind: store.ConstraintSubjectPrincipal,
				SubjectPrincipalType: pvStrPtr("agent"), SubjectPrincipalID: pvStrPtr(f.AgentID),
				ScopeType: store.RoleScopeSystem, MaximumPermissions: []string{"project.read", "project.secret_read"},
				Purpose: "usct agent constraint", CreatedBy: "usct-test",
			})
			require.NoError(t, err)
			return f.AgentID, f.Token, userScopeCeilingKey
		}, false},
		{"terminal user under an access constraint", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			_, err := f.Store.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
				Name: "usct-user", SubjectKind: store.ConstraintSubjectPrincipal,
				SubjectPrincipalType: pvStrPtr("user"), SubjectPrincipalID: pvStrPtr(f.UserID),
				ScopeType: store.RoleScopeSystem, MaximumPermissions: []string{"project.read", "project.secret_read", "secret.deliver"},
				Purpose: "usct user constraint", CreatedBy: "usct-test",
			})
			require.NoError(t, err)
			return f.AgentID, f.Token, userScopeCeilingKey
		}, false},
		{"out-of-lineage key", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			other := tid("usct-lineage-other")
			createDCUser(t, f.Store, other, "usct-lineage-other@test.com", f.ProjectID, store.ProjectRoleOwner)
			f.shareUserSecret(t, other, "USCT_OTHER_KEY", other)
			return f.AgentID, f.Token, "USCT_OTHER_KEY"
		}, false},
		{"edge without provenance", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, f.AgentID,
				store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
			return f.AgentID, f.Token, userScopeCeilingKey
		}, true},
		{"no edge after the backfill", func(t *testing.T, f *userScopeCeilingFixture) (string, string, string) {
			return f.AgentID, f.Token, userScopeCeilingKey
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUserScopeChainFixture(t, "usct-"+strings.ReplaceAll(tc.name, " ", "-"))
			agentID, token, key := tc.build(t, f)
			f.assertReadAndListAgree(t, agentID, token, key, tc.want)
		})
	}
}

// TestAgentSecretList_RecordedChainLoads (T31): the constraint table is
// loaded once per list request for the user part, whatever the number of
// candidates, and a request-level load failure (the constraint table, or the
// terminal user's principal closure) lists no user key.
//
// The source check and the per-item constraint check do no load of their
// own, so there is no per-item load fault to inject: a per-item
// ceiling_error can only come from an inconsistent call and is covered by
// userSecretSourceCheck's own guard.
func TestAgentSecretList_RecordedChainLoads(t *testing.T) {
	// walks lists the agent's secrets with n shared user-scope candidates
	// and returns the number of constraint-table loads.
	walks := func(t *testing.T, n int, scope string) int {
		t.Helper()
		f := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, "uscw-"+scope+"-"+string(rune('0'+n)))}
		setBackfillCompleted(t, f.Store)
		f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, f.AgentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
		for i := 0; i < n; i++ {
			f.shareUserSecret(t, f.UserID, "USCW_KEY_"+string(rune('A'+i)), f.UserID)
		}
		counter := &uspStore{Store: f.Server.authzService.store}
		f.Server.authzService.store = counter
		code, keys := f.listKeys(t, f.AgentID, f.Token, scope)
		require.Equal(t, http.StatusOK, code)
		userKeys := 0
		for _, k := range keys {
			if strings.HasPrefix(k, "USCW_KEY_") {
				userKeys++
			}
		}
		require.Equal(t, n, userKeys, "every candidate is listed: %v", keys)
		return counter.callCount("ListAccessConstraints")
	}
	for _, scope := range []string{"", store.ScopeUser} {
		w2, w5 := walks(t, 2, scope), walks(t, 5, scope)
		assert.Equal(t, w2, w5, "scope %q: constraint loads do not depend on the number of candidates", scope)
		assert.Positive(t, w5)
	}
	t.Run("user section adds at most one load", func(t *testing.T) {
		// The root user is a member of the agent's project, so nothing
		// before the user section walks the table on ?scope=user.
		w0, w5 := walks(t, 0, store.ScopeUser), walks(t, 5, store.ScopeUser)
		assert.LessOrEqual(t, w5-w0, 1, "the user part loads the constraint table at most once (N=0: %d, N=5: %d)", w0, w5)
	})
	t.Run("constraint load failure lists no user key", func(t *testing.T) {
		f := newUserScopeCeilingFixture(t, "uscw-fault", store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
		f.Server.authzService.store = &uspStore{Store: f.Server.authzService.store, failConstraints: true}
		code, keys := f.listKeys(t, f.AgentID, f.Token, store.ScopeUser)
		assert.Equal(t, http.StatusInternalServerError, code)
		assert.Empty(t, keys)
	})
	t.Run("terminal user closure failure lists no user key", func(t *testing.T) {
		// The group lookup fails only after the constraint table is read:
		// the request-scoped load reads the table, then builds the terminal
		// user's closure, so the fault lands there and nowhere earlier.
		f := newUserScopeCeilingFixture(t, "uscw-closure-fault", store.EffectCeiling{Kind: store.EffectCeilingPrincipal})
		counter := &uspStore{Store: f.Server.authzService.store, failGroupsAfterConstraints: true}
		f.Server.authzService.store = counter
		code, keys := f.listKeys(t, f.AgentID, f.Token, store.ScopeUser)
		assert.Equal(t, http.StatusInternalServerError, code)
		assert.Empty(t, keys)
		counter.mu.Lock()
		faults := counter.groupFaults
		counter.mu.Unlock()
		assert.Positive(t, faults, "the closure fault was injected")
	})
}
