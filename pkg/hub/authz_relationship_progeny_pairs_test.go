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
	agent := progenyPairAgent(tid("pp-use-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})
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
	agent := progenyPairAgent(tid("pp-compat-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})
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
	agent := progenyPairAgent(tid("pp-deliver-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
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
				assert.False(t, d.Allowed, "credential kind %q: reason %q", kind, d.Reason)
				assert.Equal(t, deliveryGateReason, d.Reason)
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
	agent := progenyPairAgent(tid("pp-unreviewed-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
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
	// deliver pair builds the progeny candidate.
	d := decidePerm(f.authz, agent, secret, ActionDeliver, "secret.deliver", true)
	assert.Equal(t, "secret.deliver", relationshipResult(t, d, RelationshipRuleProgeny).Permission)
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
		agent := progenyPairAgent(tid("pp-norel-agent"), f.projectBeta.ID, []string{f.memberNoneID}, scopes)
		d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, RelationshipRejectFact, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})

	t.Run("missing token scope", func(t *testing.T) {
		agent := progenyPairAgent(tid("pp-noscope-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, []AgentTokenScope{"project:read"})
		d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, "credential_scope", relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})

	t.Run("ancestry not hub-attested", func(t *testing.T) {
		fed := NewFederatedAgentIdentity("https://peer.example", tid("pp-fed-agent"), f.projectBeta.ID,
			"fed", f.projectOwnerID, []string{f.projectOwnerID}, scopes)
		d := decidePerm(f.authz, fed, res, ActionUse, "secret.use", false)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		out := f.authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(fed), res, ActionUse, "secret.use", nil, false)
		assert.Nil(t, out.accepted)
	})

	t.Run("source not opted in", func(t *testing.T) {
		notOpted := tid("pp-not-opted-secret")
		require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
			ID: notOpted, Key: "pp-not-opted", Scope: "user", ScopeID: f.projectOwnerID, CreatedBy: f.projectOwnerID,
		}))
		agent := progenyPairAgent(tid("pp-notopted-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, scopes)
		d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: notOpted}, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, RelationshipRejectFact, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})

	t.Run("inactive source owner", func(t *testing.T) {
		agent := progenyPairAgent(tid("pp-inactive-agent"), f.projectBeta.ID, []string{f.projectOwnerID}, scopes)
		setUserStatus(t, f.store, f.projectOwnerID, "suspended")
		t.Cleanup(func() { setUserStatus(t, f.store, f.projectOwnerID, store.UserStatusActive) })
		d := decidePerm(f.authz, agent, res, ActionUse, "secret.use", true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, RelationshipRejectSourceInactive, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
	})
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
// The compatibility read passes the same lookup error and missing edge,
// which pins the difference to the action's read-only classification.
func TestProgenyPair_DelegationCeilingFailsClosed(t *testing.T) {
	f := newGoldenFixture(t)
	res := Resource{Type: "secret", ID: f.secretID}
	agentID := tid("pp-ceiling-agent")
	agent := progenyPairAgent(agentID, f.projectBeta.ID, []string{f.projectOwnerID}, []AgentTokenScope{scopeProjectSecretRead})

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
		d := decideWith(a, ActionUse, "secret.use")
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "delegation ceiling check failed (fail-closed)")

		d = decideWith(a, ActionRead, permissionProjectSecretRead)
		assert.True(t, d.Allowed, "compatibility read on a lookup error: reason %q", d.Reason)
	})

	t.Run("duplicate active edges", func(t *testing.T) {
		edge := func(id string) *store.DelegationEdge {
			return &store.DelegationEdge{
				ID: id, DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.projectOwnerID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: agentID,
				ScopeType: store.RoleScopeProject, ScopeID: f.projectBeta.ID, Role: string(AgentRoleFull), Active: true,
			}
		}
		fs := &materialFailingStore{Store: f.store, delegationEdgesOverride: []*store.DelegationEdge{edge(tid("pp-e1")), edge(tid("pp-e2"))}}
		a := NewAuthzService(fs, slog.Default())
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
		assert.True(t, d.Allowed, "compatibility read without an edge: reason %q", d.Reason)
	})

	t.Run("delegator lacks the permission", func(t *testing.T) {
		createDCEdge(t, f.store, store.DelegationPrincipalUser, f.projectOwnerID, store.DelegationPrincipalAgent, agentID,
			store.RoleScopeProject, f.projectBeta.ID, string(AgentRoleFull))
		d := decideWith(f.authz, ActionUse, "secret.use")
		assertReachedCeiling(t, d)
		assert.Contains(t, d.Reason, "holds secret.use")
	})
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
