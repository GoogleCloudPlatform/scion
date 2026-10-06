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

// Tests for the progeny exact-pair branch (ptone/scion#2119 follow-up for
// ptone/scion#2129): a progeny candidate carries a read action, or one of
// the reviewed (permission, action) pairs in progenyExactPairs, keyed on the
// exact canonical permission.

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scopeProjectSecretRead AgentTokenScope = "project:secret:read"

func progenyPairAgent(subject, projectID string, ancestry []string, scopes []AgentTokenScope) *agentIdentityWrapper {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: subject},
		ProjectID: projectID,
		Ancestry:  ancestry,
		Scopes:    scopes,
	}}
}

// seedProgenyPairAgent stores agentID as a real agent row in projectID (so
// Stage 2b's agent lookup and project match succeed), and sets a stub
// ExecutionSourceResolver (a.sourceResolver = stubSourceResolver{user:
// sourceUserID's live store.User}) so Stage 2b resolves a live, admitted
// source without a real delegation edge. With no edge recorded, Decide Step
// 10 (the delegation ceiling) runs on its own pre-backfill allowance and
// is a no-op for this principal.
//
// A real edge (as seedExecutionAgent creates) carries a role, e.g.
// AgentRoleFull, through Step 10, which checks the edge's role- or
// relationship-derived authority for the exact permission. secret.use and
// the other progeny-exact-pair permissions are granted only through the
// progeny relationship (opt-in sharing), never through a role or through a
// plain user's ownership relationship, so a real edge makes Step 10 deny
// them regardless of the progeny grant these tests exercise.
//
// TestProgenyPair_DelegationCeilingFailsClosed uses this helper once, to
// reach the ceiling exactly like every other test here, then injects its own
// edge faults on top through a fresh AuthzService and the same stub.
// TestProgenyPair_SecretUseWithRecordedEdgeDeniedAtCeiling does not use it
// at all: it needs the real ExecutionSourceResolver and a real recorded
// edge end to end, so it seeds the agent (via seedExecutionAgent) and the
// edge directly, with no stub.
func seedProgenyPairAgent(t *testing.T, f *goldenFixture, a *AuthzService, agentID, projectID, sourceUserID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "pp-" + agentID[:8], Name: "pp-" + agentID[:8],
		ProjectID: projectID, Phase: "running",
		OwnerID: sourceUserID, CreatedBy: sourceUserID, Ancestry: []string{sourceUserID},
	}))
	source, err := f.store.GetUser(ctx, sourceUserID)
	require.NoError(t, err)
	prevResolver := a.sourceResolver
	a.sourceResolver = stubSourceResolver{user: source}
	t.Cleanup(func() { a.sourceResolver = prevResolver })
}

// createAlwaysEnvVar stores an opted-in user-scope env var with the always
// injection mode, the mode ListProgenyEnvVars serves.
func createAlwaysEnvVar(t *testing.T, s store.Store, id, ownerID string) {
	t.Helper()
	require.NoError(t, s.CreateEnvVar(context.Background(), &store.EnvVar{
		ID: id, Key: "PP_" + id[:8], Value: "v", Scope: "user", ScopeID: ownerID,
		InjectionMode: store.InjectionModeAlways, AllowProgeny: true, CreatedBy: ownerID,
	}))
}

// secret.use with ActionUse is admitted by progeny for an agent holding the
// secret-read token scope, whose ancestry includes the active owner of an
// opted-in user-scope secret.
func TestProgenyPair_SecretUseAdmitted(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("pp-use-agent")
	agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})
	// Stage 2b (execution-project admission) requires a real stored agent
	// whose authoritative source user holds live admission to the agent's
	// own project; projectAlpha is the project f.projectOwnerID actually
	// owns (see TestExecutionProject_ProgenyParity).
	seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
	res := Resource{Type: "secret", ID: f.secretID}

	d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "relationship grant: progeny_secret_read", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted)
	assert.Equal(t, "secret.use", r.Permission)

	d = decidePerm(f.authz, agent, res, ActionUse, "secret.use", false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
}

// The compatibility pair project.secret_read with ActionRead keeps
// admitting the same progeny read.
func TestProgenyPair_CompatibilityReadAdmitted(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("pp-compat-agent")
	agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})
	seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
	d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: f.secretID}, ActionRead, permissionProjectSecretRead, false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "relationship grant: progeny_secret_read", d.Reason)
}

// Deliver pairs are rejected by the delivery credential gate for an agent
// token and for a credential kind the pipeline does not recognise. With the
// agent token kind placed in the delivery set, the pairs pass every
// relationship stage up to the request restrictions, and the agent token
// restriction denies them: a deliver permission has no agent JWT scope.
func TestProgenyPair_DeliverUnreachableForAgentToken(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("pp-deliver-agent")
	agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
	envID := tid("pp-deliver-env")
	createAlwaysEnvVar(t, f.store, envID, f.projectOwnerID)
	cases := []struct {
		res  Resource
		perm string
	}{
		{Resource{Type: "secret", ID: f.secretID}, "secret.deliver"},
		{Resource{Type: "env_var", ID: envID}, "env_var.deliver"},
	}
	for _, tc := range cases {
		t.Run(tc.perm, func(t *testing.T) {
			d := decidePerm(f.authz, agent, tc.res, ActionDeliver, tc.perm, true)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, deliveryGateReason, d.Reason)
			require.NotNil(t, d.Provenance)
			assert.Empty(t, d.Provenance.Relationships, "the gate precedes relationship evaluation")

			for _, kind := range []CredentialKind{"unrecognized", CredentialKindAgentJWT} {
				d = f.authz.Decide(context.Background(), AuthzRequest{
					Principal:  principalContextForIdentity(agent),
					Credential: CredentialContext{Kind: kind},
					Resource:   tc.res,
					Action:     ActionDeliver,
					Permission: tc.perm,
				})
				// CredentialKindAgentJWT matches the agent's own derived
				// kind and reaches the gate, denying with its reason.
				// "unrecognized" mismatches and denies at the entry
				// classification check (ptone/scion#2123) instead; either
				// way the request is not admitted.
				if kind == CredentialKindAgentJWT {
					assert.False(t, d.Allowed, "credential kind %q: reason %q", kind, d.Reason)
					assert.Equal(t, deliveryGateReason, d.Reason)
					continue
				}
				assertRequestNotAdmitted(t, d, "credential kind "+string(kind))
			}
		})
	}

	withDeliveryCredentialKinds(t, CredentialKindAgentJWT)
	for _, tc := range cases {
		t.Run(tc.perm+"/restriction stage", func(t *testing.T) {
			d := decidePerm(f.authz, agent, tc.res, ActionDeliver, tc.perm, true)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, "relationship grant restricted by credential_scope", d.Reason)
			r := relationshipResult(t, d, RelationshipRuleProgeny)
			assert.False(t, r.Accepted)
			assert.Equal(t, "credential_scope", r.RejectedBy)
			require.NotNil(t, r.Source, "the sharing source resolved before the restriction stage")
		})
	}
}

// Pairs outside the reviewed list are denied, and the progeny candidate is
// not built for them. Cases carrying a deliver action or a deliver
// permission are denied first by the delivery credential gate; to test the
// progeny pair check itself, they run a second time with the agent token
// kind placed in the delivery set, so the gate passes and the pair check
// in progenyActionAdmitted is the stage that rejects them.
func TestProgenyPair_UnreviewedPairDenied(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("pp-unreviewed-agent")
	agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
	secret := Resource{Type: "secret", ID: f.secretID}
	skill := Resource{Type: "skill_injection", ID: f.skillInjectionID}
	cases := []struct {
		name   string
		res    Resource
		action Action
		perm   string
		// gated states whether the delivery credential gate applies: the
		// action is deliver or the permission is a deliver permission. It
		// is written out per case so the expectation does not depend on
		// the production isDeliverRequest.
		gated bool
	}{
		{"use permission with read action", secret, ActionRead, "secret.use", false},
		{"use permission with deliver action", secret, ActionDeliver, "secret.use", true},
		{"deliver permission with use action", secret, ActionUse, "secret.deliver", true},
		{"deliver permission with read action", secret, ActionRead, "secret.deliver", true},
		{"compatibility permission with use action", secret, ActionUse, permissionProjectSecretRead, false},
		{"env deliver with use action", Resource{Type: "env_var", ID: f.envVarID}, ActionUse, "env_var.deliver", true},
		{"use with an unrelated action", secret, ActionUpdate, "secret.use", false},
		{"skill injection deliver is not a progeny pair", skill, ActionDeliver, "skill_injection.deliver", true},
	}
	assertNoProgeny := func(t *testing.T, d Decision) {
		t.Helper()
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		require.NotNil(t, d.Provenance)
		for _, r := range d.Provenance.Relationships {
			assert.NotEqual(t, RelationshipRuleProgeny, r.Rule, "no progeny candidate for an unreviewed pair")
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decidePerm(f.authz, agent, tc.res, tc.action, tc.perm, true)
			assertNoProgeny(t, d)
			if tc.gated {
				assert.Equal(t, deliveryGateReason, d.Reason, "the delivery gate rejects first")
			} else {
				assert.NotEqual(t, deliveryGateReason, d.Reason)
			}
		})
	}

	withDeliveryCredentialKinds(t, CredentialKindAgentJWT)
	for _, tc := range cases {
		if !tc.gated {
			continue
		}
		t.Run(tc.name+"/pair check", func(t *testing.T) {
			d := decidePerm(f.authz, agent, tc.res, tc.action, tc.perm, true)
			assert.NotEqual(t, deliveryGateReason, d.Reason, "the gate passes, so the pair check is tested")
			assertNoProgeny(t, d)
		})
	}

	// Positive control for the second pass: with the same set, a reviewed
	// deliver pair builds the progeny candidate and clears the
	// relationship_policy, untrusted_ancestry and relationship_fact stages
	// (the store adapter serves secret.deliver). It is then rejected by the
	// restriction stage: this agent's JWT scopes do not map to
	// secret.deliver (agentScopeRestriction), so RejectedBy names that
	// restriction rather than an earlier relationship stage.
	d := decidePerm(f.authz, agent, secret, ActionDeliver, "secret.deliver", true)
	result := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.Equal(t, "secret.deliver", result.Permission)
	assert.Equal(t, "credential_scope", result.RejectedBy, "reason %q", result.Detail)
}

// secret.use is denied without a progeny relationship: unrelated ancestry,
// a source that is not opted in, an inactive owner, a missing token scope,
// and ancestry that is not hub-attested.
func TestProgenyPair_SecretUseRequiresProgeny(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	res := Resource{Type: "secret", ID: f.secretID}
	scopes := []AgentTokenScope{scopeProjectSecretRead}

	t.Run("unrelated ancestry", func(t *testing.T) {
		agentID := tid("pp-norel-agent")
		agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.memberNoneID}, scopes)
		// Stage 2b's execution source comes from the stub below
		// (f.projectOwnerID, admitted to projectAlpha), independent of the
		// JWT's Ancestry claim (f.memberNoneID) above: that Ancestry is
		// what the fact stage checks against the secret's opted-in
		// ancestry, and is deliberately unrelated so this case reaches
		// (and is rejected by) the fact stage it is named for.
		seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
		d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, RelationshipRejectFact, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})

	t.Run("missing token scope", func(t *testing.T) {
		agentID := tid("pp-noscope-agent")
		agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []AgentTokenScope{"project:read"})
		seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
		d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, "credential_scope", relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})

	t.Run("ancestry not hub-attested", func(t *testing.T) {
		fed := NewFederatedAgentIdentity("https://peer.example", tid("pp-fed-agent"), f.projectBeta.ID,
			"fed", f.projectOwnerID, []string{f.projectOwnerID}, scopes)
		d := decidePerm(f.authz, fed, res, ActionUse, "secret.use", false)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		out := f.authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(fed), res, ActionUse, "secret.use", nil, false, nil)
		assert.Nil(t, out.accepted)
	})

	t.Run("source not opted in", func(t *testing.T) {
		notOpted := tid("pp-not-opted-secret")
		require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
			ID: notOpted, Key: "pp-not-opted", Scope: "user", ScopeID: f.projectOwnerID, CreatedBy: f.projectOwnerID,
		}))
		agentID := tid("pp-notopted-agent")
		agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, scopes)
		seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
		d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: notOpted}, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, RelationshipRejectFact, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})

	t.Run("inactive source owner", func(t *testing.T) {
		agentID := tid("pp-inactive-agent")
		agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, scopes)
		// The fact stage's SharingSource owner (f.projectOwnerID, the
		// secret's creator) is the user being suspended below. Stage 2b's
		// execution source must resolve to someone else admitted to
		// projectAlpha, or it would also deny on inactivity
		// (executionProjectAdmission: "execution source user is not
		// active") before the fact stage's own inactive-owner check runs.
		// f.projectAdminID (alpha's project admin) stays active throughout.
		seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectAdminID)
		setUserStatus(t, f.store, f.projectOwnerID, "suspended")
		t.Cleanup(func() { setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusActive) })
		d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, RelationshipRejectSourceInactive, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})
}

// The same admitted fixture as TestProgenyPair_SecretUseAdmitted, except the
// agent is stored in projectBeta: f.projectOwnerID (the resolved execution
// source) holds no role there (only projectAlpha, see
// TestExecutionProject_ProgenyParity), so Stage 2b (execution-project
// admission) denies before the fact stage that admits the pair in the
// projectAlpha case is ever reached.
func TestProgenyPair_SecretUseRequiresExecutionProjectAdmission(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("pp-noadmission-agent")
	agent := progenyPairAgent(agentID, f.projectBeta.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})
	seedProgenyPairAgent(t, f, f.authz, agentID, f.projectBeta.ID, f.projectOwnerID)

	d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: f.secretID}, ActionUse, "secret.use", true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "execution_project", relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
}

// ActionUse and ActionDeliver are not read-only operations, so the
// delegation ceiling fails closed for them.
func TestProgenyPair_UseAndDeliverNotReadOnly(t *testing.T) {
	assert.False(t, isReadOnlyOperation(ActionUse))
	assert.False(t, isReadOnlyOperation(ActionDeliver))
	assert.False(t, relationshipReadClassActions[string(ActionUse)])
	assert.False(t, relationshipReadClassActions[string(ActionDeliver)])
}

// A valid progeny secret.use reaches the delegation ceiling, which denies a
// delegated agent on an edge lookup error, a missing edge after the
// backfill, duplicate active edges, and a delegator lacking the permission.
// The compatibility read is denied by the same lookup error and missing
// edge too: Step 10 fails closed on a ceiling lookup error for every
// action (no read exemption), and "secret" is excluded from the missing-edge
// read allowance (sensitiveReadResourceTypes, authz_delegation_ceiling.go) —
// read-only classification buys secret.use's compatibility pair nothing
// here.
func TestProgenyPair_DelegationCeilingFailsClosed(t *testing.T) {
	f := newGoldenFixture(t)
	res := Resource{Type: "secret", ID: f.secretID}
	agentID := tid("pp-ceiling-agent")
	agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})

	// Stage 2b (execution-project admission) and the delegation-ceiling walk
	// below (Step 10) both resolve delegation edges through the same store
	// call, GetDelegationEdgesForDelegate: the edge faults these subtests
	// inject to exercise the ceiling would also fail Stage 2b for the same
	// reason, before the ceiling ever runs. seedProgenyPairAgent's stub
	// source resolver decouples them — it gives Stage 2b a fixed, real,
	// admitted user without calling the store — so only the ceiling's own
	// walk below exercises each injected fault. f.authz is exclusive to
	// this test (a fresh newGoldenFixture), so mutating its sourceResolver
	// here is safe.
	seedProgenyPairAgent(t, f, f.authz, agentID, f.projectAlpha.ID, f.projectOwnerID)
	owner, err := f.store.GetUser(context.Background(), f.projectOwnerID)
	require.NoError(t, err)

	decideWith := func(a *AuthzService, action Action, perm string) Decision {
		return decidePerm(a, agent, res, action, perm, true)
	}
	assertReachedCeiling := func(t *testing.T, d Decision) {
		t.Helper()
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.True(t, r.Accepted, "the relationship admitted the pair before the ceiling")
	}

	t.Run("edge lookup error", func(t *testing.T) {
		fs := &materialFailingStore{Store: f.store, getDelegationEdgesForDelegateErr: errors.New("edge store unavailable")}
		a := NewAuthzService(fs, slog.Default())
		a.sourceResolver = stubSourceResolver{user: owner}
		d := decideWith(a, ActionUse, "secret.use")
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "delegation ceiling check failed (fail-closed)")

		d = decideWith(a, ActionRead, permissionProjectSecretRead)
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "delegation ceiling check failed (fail-closed)")
	})

	t.Run("duplicate active edges", func(t *testing.T) {
		edge := func(id string) *store.DelegationEdge {
			return &store.DelegationEdge{
				ID: id, DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.projectOwnerID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: agentID,
				ScopeType: store.RoleScopeProject, ScopeID: f.projectAlpha.ID, Role: string(AgentRoleFull), Active: true,
			}
		}
		fs := &materialFailingStore{Store: f.store, delegationEdgesOverride: []*store.DelegationEdge{edge(tid("pp-e1")), edge(tid("pp-e2"))}}
		a := NewAuthzService(fs, slog.Default())
		a.sourceResolver = stubSourceResolver{user: owner}
		d := decideWith(a, ActionUse, "secret.use")
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "multiple active delegation edges")
	})

	t.Run("missing edge after backfill", func(t *testing.T) {
		setBackfillCompleted(t, f.store)
		d := decideWith(f.authz, ActionUse, "secret.use")
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "no delegation edge")

		d = decideWith(f.authz, ActionRead, permissionProjectSecretRead)
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "no delegation edge for agent:"+agentID, "secret is excluded from the read allowance (sensitiveReadResourceTypes)")
	})

	t.Run("delegator lacks the permission", func(t *testing.T) {
		createDCEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, store.DelegationPrincipalAgent, agentID,
			store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))
		d := decideWith(f.authz, ActionUse, "secret.use")
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "does not hold secret.use")
	})
}

// TestProgenyPair_SecretUseWithRecordedEdgeDeniedAtCeiling characterizes the
// real (non-stub) end-to-end interaction TestProgenyPair_DelegationCeilingFailsClosed's
// stub resolver exists to isolate: with a genuine recorded delegation edge
// (seedExecutionAgent, real ExecutionSourceResolver, no stub), the delegator
// (f.projectOwnerID) holds the edge's role (AgentRoleFull) but not
// secret.use itself — no role and no relationship grants it to a plain
// user, and the ceiling's delegator-authority check
// (evaluateUserDelegatorAuthority) does not consult the progeny relationship
// grant that admitted the pair above it. The progeny relationship still
// admits the pair (Accepted is true), but Decide Step 10 (the delegation
// ceiling) denies it anyway, because the recorded edge's authority does not
// cover this specific permission. A per-relationship deliver/use ceiling
// that would admit this case is out of scope here; it comes with the
// recorded-provenance work in ptone/scion#2121 (B.3). Step 10 is unchanged.
func TestProgenyPair_SecretUseWithRecordedEdgeDeniedAtCeiling(t *testing.T) {
	f := newGoldenFixture(t)
	agentID := tid("pp-recorded-edge-agent")
	agent := progenyPairAgent(agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})
	seedExecutionAgent(t, f.store, agentID, f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})

	d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: f.secretID}, ActionUse, "secret.use", true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted, "the progeny relationship admits the pair before the ceiling")
	assert.Contains(t, d.Reason, "delegator "+f.projectOwnerID+" does not hold secret.use")
}

// "env_var" is served by the built-in env var adapter: the direct
// compatibility resolver reads it, Decide serves env_var.deliver, and list
// filtering stays closed because deliver is not a read.
func TestProgenyPair_EnvVarMapping(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	agent := progenyPairAgent(tid("pp-env-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	envID := tid("pp-map-env")
	createAlwaysEnvVar(t, f.store, envID, f.projectOwnerID)

	for _, kind := range []string{"env_var", "envvar"} {
		got := NewRelationshipGrantResolver(f.store).CheckProgenyAccess(ctx, agent, Resource{Type: kind, ID: envID}, ActionRead)
		assert.True(t, got.Allowed, "%s: %s", kind, got.DenyReason)
		assert.Equal(t, RelProgenyEnvVarRead, got.RelationshipType)
	}

	adapter, perms := f.authz.progenyAdapter("env_var")
	_, isStore := adapter.(storeProgenyAdapter)
	assert.True(t, isStore)
	assert.Equal(t, []string{"env_var.deliver"}, perms)
	_, envvarPerms := f.authz.progenyAdapter("envvar")
	assert.Empty(t, envvarPerms, "the original env string carries no Decide permission")

	src, ok, detail := f.authz.progenySourceFor(ctx, agent, "env_var", envID, "env_var.deliver")
	assert.True(t, ok, detail)
	require.NotNil(t, src)
	assert.Equal(t, f.projectOwnerID, src.OwnerID)

	pred := f.authz.ProgenyListPredicate(ctx, principalContextForIdentity(agent), "env_var")
	assert.False(t, pred.Matches(*src), "deliver does not make env vars listable")
}

// RegisterProgenyAdapter accepts a reviewed pair only on its own resource
// type, and refuses use or deliver permissions that are not reviewed pairs.
func TestRegisterProgenyAdapter_ExactPairs(t *testing.T) {
	authz, _ := authzTestSetup(t)
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "x", perms: []string{"secret.use"}}), errProgenyAdapter,
		"a reviewed pair on another kind is refused")
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "gcp_service_account", perms: []string{"gcp_service_account.use"}}), errProgenyAdapter,
		"an unreviewed use permission is refused")
	releaseBuiltinProgenyAdapter(t, authz, "skill_injection")
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "skill_injection", perms: []string{"skill_injection.deliver"}}), errProgenyAdapter,
		"skill_injection.deliver is not a progeny pair")
	releaseBuiltinProgenyAdapter(t, authz, "secret")
	require.NoError(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead, "secret.use", "secret.deliver"}}))
}

// The action gate admits reads outside the exact list and exactly the
// reviewed pairs.
func TestProgenyActionAdmitted(t *testing.T) {
	assert.True(t, progenyActionAdmitted(permissionProjectSecretRead, ActionRead))
	assert.True(t, progenyActionAdmitted("secret.use", ActionUse))
	assert.True(t, progenyActionAdmitted("secret.deliver", ActionDeliver))
	assert.True(t, progenyActionAdmitted("env_var.deliver", ActionDeliver))
	assert.False(t, progenyActionAdmitted("secret.use", ActionRead))
	assert.False(t, progenyActionAdmitted("env_var.deliver", ActionRead))
	assert.False(t, progenyActionAdmitted("skill_injection.deliver", ActionDeliver))
	assert.False(t, progenyActionAdmitted("gcp_service_account.use", ActionUse))
	assert.False(t, progenyActionAdmitted(permissionProjectSecretRead, ActionList))
	assert.False(t, progenyActionAdmitted("", ActionUse))
}
