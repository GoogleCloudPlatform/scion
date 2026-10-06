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

// Tests for the common relationship stage (ptone/scion#2119): every
// relationship candidate passes the relationship policy, hub-attested
// ancestry, relationship fact, source activity and request restrictions.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decidePerm(authz *AuthzService, identity Identity, resource Resource, action Action, permissionID string, explain bool) Decision {
	return authz.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   resource,
		Action:     action,
		Permission: permissionID,
		Explain:    explain,
	})
}

func relationshipResult(t *testing.T, d Decision, rule RelationshipRuleID) RelationshipCandidateResult {
	t.Helper()
	require.NotNil(t, d.Provenance, "explain provenance")
	for _, r := range d.Provenance.Relationships {
		if r.Rule == rule {
			return r
		}
	}
	t.Fatalf("no %s candidate in provenance: %+v", rule, d.Provenance.Relationships)
	return RelationshipCandidateResult{}
}

func setUserStatus(t *testing.T, s store.Store, id, status string) {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	u.Status = status
	require.NoError(t, s.UpdateUser(context.Background(), u))
}

// A permission not listed for the relationship is denied even when the
// relationship holds: the owner of an agent does not receive permissions of
// other resource types, nor an unregistered permission on its own type.
func TestRelationshipRules_UnlistedPermissionDenied(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relrule-owner"))
	grantProjectAccessOnly(t, s, owner.ID(), tid("relrule-proj"))
	agent := agentResource(&store.Agent{ID: tid("relrule-agent"), ProjectID: tid("relrule-proj"), OwnerID: owner.ID()})
	tpl := templateResource(&store.Template{ID: tid("relrule-tpl"), OwnerID: owner.ID(), Scope: store.TemplateScopeUser, ScopeID: owner.ID()})

	for _, tc := range []struct {
		name     string
		resource Resource
		action   Action
		perm     string
	}{
		{"cross-type registered permission", agent, ActionUpdate, "hub.config.update"},
		{"cross-type project permission", agent, ActionRead, "project.read"},
		{"unregistered permission on owned type", tpl, Action("frobnicate"), "template.frobnicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decidePerm(authz, owner, tc.resource, tc.action, tc.perm, true)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			r := relationshipResult(t, d, RelationshipRuleOwner)
			assert.False(t, r.Accepted)
			assert.Equal(t, RelationshipRejectPolicy, r.RejectedBy)
		})
	}

	// The listed permission on the same resource is still admitted.
	d := decidePerm(authz, owner, agent, ActionRead, "agent.read", false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "relationship grant: resource owner", d.Reason)
}

// An agent ancestor's candidate passes through the agent token scope
// restriction: a missing scope rejects it and the reason names the kind.
func TestRelationshipRules_CredentialScopeRestrictsAncestor(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ancestorID := tid("relrule-anc")
	ancestor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ancestorID},
		ProjectID: tid("relrule-anc-proj"),
		Scopes:    []AgentTokenScope{ScopeAgentNotify},
	}}
	desc := agentResource(&store.Agent{
		ID: tid("relrule-desc"), ProjectID: tid("relrule-desc-proj"),
		Ancestry: []string{tid("relrule-root"), ancestorID},
	})

	d := decidePerm(authz, ancestor, desc, ActionLifecycle, "agent.lifecycle", true)
	assert.False(t, d.Allowed)
	assert.Equal(t, "relationship grant restricted by credential_scope", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleAncestor)
	assert.Equal(t, "credential_scope", r.RejectedBy)
	require.NotEmpty(t, d.Provenance.DenyReasons)
	assert.Equal(t, d.Reason, d.Provenance.DenyReasons[0])

	d = decidePerm(authz, ancestor, desc, Action("notify"), "agent.notify", false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "relationship grant: ancestor access", d.Reason)
}

// A token scoped to attach only cannot reach lifecycle through the owner
// relationship.
func TestRelationshipRules_AttachOnlyTokenCannotReachLifecycle(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relrule-uat-owner"))
	projectID := tid("relrule-uat-proj")
	agent := agentResource(&store.Agent{ID: tid("relrule-uat-agent"), ProjectID: projectID, OwnerID: owner.ID()})
	scoped := NewScopedUserIdentity(owner, projectID, []string{"agent:attach"})

	d := decidePerm(authz, scoped, agent, ActionLifecycle, "agent.lifecycle", false)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	d = decidePerm(authz, scoped, agent, ActionDelete, "agent.delete", false)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
}

// An access constraint on the principal limits relationship candidates the
// same way it limits role bindings.
func TestRelationshipRules_AccessConstraintRestrictsOwner(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relrule-ac-owner"))
	grantProjectAccessOnly(t, s, owner.ID(), tid("relrule-ac-proj"))
	agent := agentResource(&store.Agent{ID: tid("relrule-ac-agent"), ProjectID: tid("relrule-ac-proj"), OwnerID: owner.ID()})

	userType, userID := "user", owner.ID()
	_, err := s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		ID: api.NewUUID(), Name: "cap-owner", SubjectKind: "principal",
		SubjectPrincipalType: &userType, SubjectPrincipalID: &userID,
		ScopeType: store.RoleScopeSystem, MaximumPermissions: []string{"agent.read"},
		Purpose: "test", CreatedBy: "test",
	})
	require.NoError(t, err)

	d := decidePerm(authz, owner, agent, ActionDelete, "agent.delete", true)
	assert.False(t, d.Allowed)
	assert.Equal(t, "relationship grant restricted by access_constraint", d.Reason)
	assert.Equal(t, "access_constraint", relationshipResult(t, d, RelationshipRuleOwner).RejectedBy)

	d = decidePerm(authz, owner, agent, ActionRead, "agent.read", false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
}

// Ancestry-derived relationships require hub-attested ancestry for every
// principal kind. Decide denies federated principals before relationship
// evaluation; the stage itself is checked directly as well.
func TestRelationshipRules_UntrustedAncestryRejected(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	fed := NewFederatedAgentIdentity("https://peer.example", tid("relrule-fed"), tid("relrule-fed-proj"),
		"fed", tid("relrule-fed-root"), []string{tid("relrule-fed-root")}, allRegisteredAgentScopes())
	desc := agentResource(&store.Agent{
		ID: tid("relrule-fed-desc"), ProjectID: tid("relrule-fed-proj"),
		Ancestry: []string{tid("relrule-fed-root"), fed.ID()},
	})
	assert.False(t, decidePerm(authz, fed, desc, Action("notify"), "agent.notify", false).Allowed)

	out := authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(fed), desc, Action("notify"), "agent.notify", nil, false, nil)
	assert.Nil(t, out.accepted)
	require.Len(t, out.results, 1)
	assert.Equal(t, RelationshipRuleAncestor, out.results[0].Rule)
	assert.Equal(t, RelationshipRejectUntrustedAncestry, out.results[0].RejectedBy)
	assert.Equal(t, RelationshipRejectUntrustedAncestry, out.restrictedBy)

	fedUser := NewFederatedUserIdentity("https://peer.example", tid("relrule-fed-user"), "fed-user@example.test", "Fed User", "member", nil)
	userDesc := agentResource(&store.Agent{
		ID: tid("relrule-fed-user-desc"), ProjectID: tid("relrule-fed-proj"),
		Ancestry: []string{fedUser.ID()},
	})
	out = authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(fedUser), userDesc, ActionRead, "agent.read", nil, false, nil)
	assert.Nil(t, out.accepted)
	require.NotEmpty(t, out.results)
	assert.Equal(t, RelationshipRuleAncestor, out.results[0].Rule)
	assert.Equal(t, RelationshipRejectUntrustedAncestry, out.results[0].RejectedBy)

	// The same shape with a hub-attested agent is accepted by the stage.
	local := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: tid("relrule-local")}, Scopes: allRegisteredAgentScopes()}}
	localDesc := agentResource(&store.Agent{ID: tid("relrule-local-desc"), Ancestry: []string{tid("relrule-fed-root"), local.ID()}})
	out = authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(local), localDesc, Action("notify"), "agent.notify", nil, false, nil)
	require.NotNil(t, out.accepted)
	assert.Equal(t, "relationship grant: ancestor access", out.accepted.Reason)
}

// permissions.RelationshipPrincipalKind maps federated_agent to "agent".
// Every candidate an agent-kind row can admit uses the hub-attested
// ancestry stage, so a federated agent matches no agent row even when the
// row, fact shape and scopes would otherwise hold.
func TestRelationshipRules_FederatedAgentMatchesNoAgentRow(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	require.Equal(t, "agent", permissions.RelationshipPrincipalKind(string(PrincipalKindFederatedAgent)))

	fed := NewFederatedAgentIdentity("https://peer.example", tid("relrule-fedrow"), f.projectBeta.ID,
		"fed", f.projectOwnerID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	principal := principalContextForIdentity(fed)
	require.Equal(t, PrincipalKindFederatedAgent, principal.Kind)

	cases := map[string]struct {
		rule     RelationshipRuleID
		resource Resource
		action   Action
		perm     string
		// absent marks a rule that builds no candidate at all for a
		// federated agent.
		absent bool
	}{
		// The launcher status read applies to local agents only.
		"launcher": {RelationshipRuleLauncher, agentStatusReadResource(&store.Agent{
			ID: tid("relrule-fedrow-launched"), ProjectID: f.projectBeta.ID,
			Ancestry: []string{f.projectOwnerID, fed.ID()},
		}), ActionRead, "agent.read", true},
		"ancestor": {RelationshipRuleAncestor, agentResource(&store.Agent{
			ID: tid("relrule-fedrow-desc"), ProjectID: f.projectBeta.ID,
			Ancestry: []string{f.projectOwnerID, fed.ID()},
		}), Action("notify"), "agent.notify", false},
		// ptone/scion#2128: personal skills are a progeny row too now
		// (skillProgenyAdapter), sharing RelationshipRuleProgeny with the
		// secret case below; kept as its own case for the skill shape.
		"progeny_skill": {RelationshipRuleProgeny, skillResource(&store.Skill{
			ID: tid("relrule-fedrow-skill"), Scope: store.SkillScopeUser, ScopeID: f.projectOwnerID,
		}), ActionRead, "skill.read", false},
		"progeny": {RelationshipRuleProgeny, Resource{Type: "secret", ID: f.secretID}, ActionRead, permissionProjectSecretRead, false},
	}

	// Every relationship with an agent-kind row has a case here.
	for _, row := range permissions.RelationshipPolicies {
		for _, kind := range row.PrincipalKinds {
			if kind == "agent" {
				_, ok := cases[row.Relationship]
				assert.True(t, ok, "agent-kind row %q/%s needs a federated-agent case", row.Relationship, row.ResourceType)
			}
		}
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out := f.authz.evaluateRelationshipCandidates(ctx, principal, tc.resource, tc.action, tc.perm, nil, false, nil)
			assert.Nil(t, out.accepted)
			found := false
			for _, r := range out.results {
				if r.Rule == tc.rule {
					found = true
					assert.False(t, r.Accepted)
					assert.Equal(t, RelationshipRejectUntrustedAncestry, r.RejectedBy)
				}
			}
			if tc.absent {
				assert.False(t, found, "candidate %q must not be built", tc.rule)
			} else {
				assert.True(t, found, "candidate %q must be evaluated", tc.rule)
			}
			assert.False(t, decidePerm(f.authz, fed, tc.resource, tc.action, tc.perm, false).Allowed)
		})
	}
}

// A progeny read requires the sharing source's owner to be active.
func TestRelationshipRules_ProgenySourceInactive(t *testing.T) {
	f := newGoldenFixture(t)
	// The execution source is a different active member of the agent's
	// project, so the source-activity stage is the one under test.
	seedExecutionAgent(t, f.store, tid("relrule-progeny-agent"), f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectAdminID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-progeny-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	secretRes := Resource{Type: "secret", ID: f.secretID}

	d := decidePerm(f.authz, agent, secretRes, ActionRead, permissionProjectSecretRead, true)
	require.True(t, d.Allowed, "reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted)
	require.NotNil(t, r.Source)
	assert.Equal(t, "secret", r.Source.Kind)
	assert.Equal(t, f.secretID, r.Source.ID)
	assert.Equal(t, f.projectOwnerID, r.Source.OwnerID)

	setUserStatus(t, f.store, f.projectOwnerID, "suspended")
	d = decidePerm(f.authz, agent, secretRes, ActionRead, permissionProjectSecretRead, true)
	assert.False(t, d.Allowed)
	assert.Equal(t, "relationship grant restricted by source_inactive", d.Reason)
	r = relationshipResult(t, d, RelationshipRuleProgeny)
	assert.Equal(t, RelationshipRejectSourceInactive, r.RejectedBy)
	require.NotNil(t, r.Source)
	assert.Empty(t, r.Source.ID, "rejected candidates record the source kind only")
	assert.Empty(t, r.Source.OwnerID)
}

// The personal-skill progeny read requires an active origin user.
func TestRelationshipRules_SkillProgenySourceInactive(t *testing.T) {
	f := newGoldenFixture(t)
	// The execution source is a different active member of the agent's
	// project, so the source-activity stage is the one under test.
	seedExecutionAgent(t, f.store, tid("relrule-skill-agent"), f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectAdminID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-skill-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	res := skillResource(&store.Skill{ID: tid("relrule-skill"), Scope: store.SkillScopeUser, ScopeID: f.projectOwnerID})
	d := decidePerm(f.authz, agent, res, ActionRead, "skill.read", false)
	require.True(t, d.Allowed, "reason %q", d.Reason)

	setUserStatus(t, f.store, f.projectOwnerID, "suspended")
	d = decidePerm(f.authz, agent, res, ActionRead, "skill.read", true)
	assert.False(t, d.Allowed)
	assert.Equal(t, "relationship grant restricted by source_inactive", d.Reason)
	assert.Equal(t, RelationshipRejectSourceInactive, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)
}

// Explain lists relationship candidates on allow, including a kernel
// allow, and the accepted candidate matches the decision.
func TestRelationshipRules_ExplainListsCandidates(t *testing.T) {
	f := newGoldenFixture(t)
	owner := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Project Owner", "member", "api")

	// Relationship allow: the owner of alpha's agent (bound as project
	// owner too, so the kernel may also allow).
	d := decidePerm(f.authz, owner, agentResource(f.agentAlpha), ActionRead, "agent.read", true)
	require.True(t, d.Allowed, "reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleOwner)
	assert.True(t, r.Accepted)
	assert.Equal(t, "agent.read", r.Permission)
	r = relationshipResult(t, d, RelationshipRuleAncestor)
	assert.True(t, r.Accepted)

	// Without explain, provenance carries no relationship list.
	d = decidePerm(f.authz, owner, agentResource(f.agentAlpha), ActionRead, "agent.read", false)
	require.True(t, d.Allowed)
	if d.Provenance != nil {
		assert.Empty(t, d.Provenance.Relationships)
	}
}

// Every relationship rule ID is a known relationship name, and every
// policy row names one of them.
func TestRelationshipRules_RuleIDsMatchPolicyNames(t *testing.T) {
	ids := map[string]bool{}
	for _, id := range []RelationshipRuleID{
		RelationshipRuleOwner, RelationshipRuleAncestor, RelationshipRuleProgeny,
		RelationshipRuleHubMemberSAAssign, RelationshipRuleLauncher,
		RelationshipRuleProjectAssociation, RelationshipRuleHubAssociation, RelationshipRuleBrokerAssociation,
	} {
		ids[string(id)] = true
	}
	assert.Equal(t, knownRelationshipNames, ids)
	for _, row := range permissions.RelationshipPolicies {
		assert.True(t, ids[row.Relationship], "row relationship %q", row.Relationship)
	}
}

func TestRelationshipPolicyPrincipalKind(t *testing.T) {
	for kind, want := range map[PrincipalKind]string{
		PrincipalKindUser: "user", PrincipalKindDev: "user", PrincipalKindFederatedUser: "user",
		PrincipalKindAgent: "agent", PrincipalKindFederatedAgent: "agent",
	} {
		assert.Equal(t, want, permissions.RelationshipPrincipalKind(string(kind)), "kind %q", kind)
	}
	// Any other kind maps outside the row vocabulary and matches no row.
	for _, kind := range []PrincipalKind{"service", ""} {
		mapped := permissions.RelationshipPrincipalKind(string(kind))
		assert.False(t, relationshipPolicyPrincipalKinds[mapped], "kind %q must not map into the row vocabulary", kind)
		for _, row := range permissions.RelationshipPolicies {
			for _, id := range row.PermissionIDs {
				assert.False(t, permissions.RelationshipPolicyAllows(row.Relationship, mapped, row.ResourceType, id),
					"kind %q must match no row (%s/%s/%s)", kind, row.Relationship, row.ResourceType, id)
			}
		}
	}
}

// --- progeny adapters ---

type fakeProgenyAdapter struct {
	kind    string
	perms   []string
	sources []SharingSource
	err     error
}

func (f fakeProgenyAdapter) Kind() string              { return f.kind }
func (f fakeProgenyAdapter) ReadPermissions() []string { return f.perms }
func (f fakeProgenyAdapter) Sources(_ context.Context, q ProgenyQuery) ([]SharingSource, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []SharingSource
	for _, s := range f.sources {
		if q.ResourceID == "" || s.ID == q.ResourceID {
			out = append(out, s)
		}
	}
	return out, nil
}

func TestRegisterProgenyAdapter_Validation(t *testing.T) {
	authz, _ := authzTestSetup(t)
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(nil), errProgenyAdapter)
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "x"}), errProgenyAdapter)
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "x", perms: []string{"agent.delete"}}), errProgenyAdapter,
		"write-class permissions are refused")
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "x", perms: []string{"x.read"}}), errProgenyAdapter,
		"unregistered permissions are refused")
	require.NoError(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "x", perms: []string{"skill.read"}}))
	assert.ErrorIs(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "x", perms: []string{"skill.read"}}), errProgenyAdapter,
		"a kind registers once")
}

// Kinds served by the built-in store adapter cannot be registered unless a
// test releases the built-in adapter first.
func TestRegisterProgenyAdapter_BuiltinKindsRefused(t *testing.T) {
	authz, _ := authzTestSetup(t)
	for kind := range progenyOptInKinds {
		err := authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: kind, perms: []string{permissionProjectSecretRead}})
		assert.ErrorIs(t, err, errProgenyAdapter, "kind %q", kind)
		adapter, _ := authz.progenyAdapter(kind)
		_, isStore := adapter.(storeProgenyAdapter)
		assert.True(t, isStore, "kind %q keeps the built-in store adapter", kind)
	}
	releaseBuiltinProgenyAdapter(t, authz, "secret")
	require.NoError(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}}))
	adapter, _ := authz.progenyAdapter("secret")
	_, isFake := adapter.(fakeProgenyAdapter)
	assert.True(t, isFake)
}

// A registered adapter replaces the built-in store adapter for its kind.
// Opted-in sources of an active owner are readable; others are not; a
// lookup error denies.
func TestProgenyAdapter_RegisteredSourcesDecide(t *testing.T) {
	f := newGoldenFixture(t)
	seedExecutionAgent(t, f.store, tid("relrule-adapter-agent"), f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-adapter-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	adapter := fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, sources: []SharingSource{
		{Kind: "secret", ID: "opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "not-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired},
	}}
	releaseBuiltinProgenyAdapter(t, f.authz, "secret")
	require.NoError(t, f.authz.RegisterProgenyAdapter(adapter))

	d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: "opted"}, ActionRead, permissionProjectSecretRead, false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	d = decidePerm(f.authz, agent, Resource{Type: "secret", ID: "not-opted"}, ActionRead, permissionProjectSecretRead, true)
	assert.False(t, d.Allowed)
	assert.Equal(t, RelationshipRejectFact, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)

}

// A sharing-source lookup failure rejects the progeny candidate at the
// fact stage, with an active owner and every other stage satisfied.
func TestProgenyAdapter_SourcesErrorRejectsAtFactStage(t *testing.T) {
	f := newGoldenFixture(t)
	seedExecutionAgent(t, f.store, tid("relrule-adapter-err-agent"), f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-adapter-err-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	owner, err := f.store.GetUser(context.Background(), f.projectOwnerID)
	require.NoError(t, err)
	require.Equal(t, store.UserStatusActive, owner.Status, "the source owner is active")

	releaseBuiltinProgenyAdapter(t, f.authz, "secret")
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{
		kind: "secret", perms: []string{permissionProjectSecretRead}, err: errors.New("unavailable"),
	}))
	d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: f.secretID}, ActionRead, permissionProjectSecretRead, true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.False(t, r.Accepted)
	assert.Equal(t, RelationshipRejectFact, r.RejectedBy)
	assert.Equal(t, "sharing-source lookup failed", r.Detail)

	d = decidePerm(f.authz, agent, Resource{Type: "secret", ID: f.secretID}, ActionRead, permissionProjectSecretRead, false)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
}

// List filtering and point reads agree for every fixture source.
func TestProgeny_ListAndPointReadConsistent(t *testing.T) {
	f := newGoldenFixture(t)
	suspendedID := tid("relrule-suspended-owner")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: suspendedID, Email: "suspended@relrule.test", DisplayName: "s", Role: "member", Status: "suspended",
	}))
	seedExecutionAgent(t, f.store, tid("relrule-consistency-agent"), f.projectAlpha.ID, []string{f.projectOwnerID, suspendedID}, []string{f.projectOwnerID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-consistency-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID, suspendedID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	sources := []SharingSource{
		{Kind: "secret", ID: "s-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "s-not-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired},
		{Kind: "secret", ID: "s-foreign", OwnerID: f.memberNoneID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "s-suspended", OwnerID: suspendedID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		// Secrets are shared only through an opt-in, whatever policy the
		// adapter declares.
		{Kind: "secret", ID: "s-origin", OwnerID: f.projectOwnerID, Policy: SharingPolicyOriginDescendants, OptedIn: true},
		{Kind: "secret", ID: "s-unknown-policy", OwnerID: f.projectOwnerID, Policy: SharingPolicy("other"), OptedIn: true},
	}
	releaseBuiltinProgenyAdapter(t, f.authz, "secret")
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, sources: sources}))

	ctx := context.Background()
	principal := principalContextForIdentity(agent)
	pred := f.authz.ProgenyListPredicate(ctx, principal, "secret")
	want := map[string]bool{"s-opted": true}
	for _, src := range sources {
		listed := pred.Matches(src)
		point := decidePerm(f.authz, agent, Resource{Type: "secret", ID: src.ID}, ActionRead, permissionProjectSecretRead, false).Allowed
		assert.Equal(t, listed, point, "source %s: list and point read disagree", src.ID)
		assert.Equal(t, want[src.ID], listed, "source %s", src.ID)
		assert.Equal(t, listed, f.authz.EvaluateProgeny(ctx, principal, src), "source %s", src.ID)
	}

	// An unattested principal gets a predicate that matches nothing.
	fed := NewFederatedAgentIdentity("https://peer.example", tid("relrule-cons-fed"), f.projectBeta.ID,
		"fed", f.projectOwnerID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	fedPred := f.authz.ProgenyListPredicate(ctx, principalContextForIdentity(fed), "secret")
	for _, src := range sources {
		assert.False(t, fedPred.Matches(src), "source %s", src.ID)
		assert.False(t, decidePerm(f.authz, fed, Resource{Type: "secret", ID: src.ID}, ActionRead, permissionProjectSecretRead, false).Allowed)
	}

	// Origin-descendants sharing, on a kind that is not pinned to opt-in:
	// the predicate shares a source owned by the origin user and not one
	// owned by a later member of the chain. The kind has no progeny policy
	// row, so list filtering and point reads both admit nothing.
	origin := []SharingSource{
		{Kind: "template", ID: "t-origin", OwnerID: f.projectOwnerID, Policy: SharingPolicyOriginDescendants},
		{Kind: "template", ID: "t-origin-mid", OwnerID: suspendedID, Policy: SharingPolicyOriginDescendants},
	}
	shape := ProgenyPredicate{Kind: "template", AttestedAncestry: agent.Ancestry()}
	assert.True(t, shape.shared(origin[0]))
	assert.False(t, shape.shared(origin[1]))
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "template", perms: []string{"template.read"}, sources: origin}))
	tplPred := f.authz.ProgenyListPredicate(ctx, principal, "template")
	for _, src := range origin {
		listed := tplPred.Matches(src)
		point := decidePerm(f.authz, agent, Resource{Type: "template", ID: src.ID}, ActionRead, "template.read", false).Allowed
		assert.Equal(t, listed, point, "source %s: list and point read disagree", src.ID)
		assert.False(t, listed, "source %s: no progeny row for template", src.ID)
	}
}

// Actor and Purpose are recorded on the decision (and in its provenance
// when present) and do not change the decision.
func TestDecide_ActorAndPurposeAreAuditOnly(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relrule-actor-owner"))
	other := createCharacterizationUser(t, s, tid("relrule-actor-other"))
	agent := agentResource(&store.Agent{ID: tid("relrule-actor-agent"), ProjectID: tid("relrule-actor-proj"), OwnerID: owner.ID()})
	actor := &DecisionActor{Kind: PrincipalKindUser, ID: owner.ID()}

	for _, ident := range []Identity{owner, other} {
		for _, explain := range []bool{false, true} {
			base := decidePerm(authz, ident, agent, ActionRead, "agent.read", explain)
			req := AuthzRequest{
				Principal:  principalContextForIdentity(ident),
				Credential: credentialContextForIdentity(ident),
				Resource:   agent, Action: ActionRead, Permission: "agent.read", Explain: explain,
				Actor: actor, Purpose: "delivery",
			}
			d := authz.Decide(context.Background(), req)
			assert.Equal(t, base.Allowed, d.Allowed)
			assert.Equal(t, base.Reason, d.Reason)
			assert.Equal(t, actor, d.Actor)
			assert.Equal(t, "delivery", d.Purpose)
			if d.Provenance != nil {
				assert.Equal(t, actor, d.Provenance.Actor)
				assert.Equal(t, "delivery", d.Provenance.Purpose)
			}
		}
	}
}

// releaseBuiltinProgenyAdapter lets a test register its own adapter for a
// kind served by the built-in store adapter.
func releaseBuiltinProgenyAdapter(t *testing.T, a *AuthzService, kind string) {
	t.Helper()
	require.True(t, progenyOptInKinds[kind], "kind %q has no built-in adapter", kind)
	a.progenyAdapters.mu.Lock()
	defer a.progenyAdapters.mu.Unlock()
	if a.progenyAdapters.builtinReleased == nil {
		a.progenyAdapters.builtinReleased = map[string]bool{}
	}
	a.progenyAdapters.builtinReleased[kind] = true
}

// --- sharing-source owner activity ---

// sourceOwnerStore serves configured users and agents for source-owner
// lookups and delegates everything else to the wrapped store. userErr
// makes GetUser fail for an ID with an error other than store.ErrNotFound.
type sourceOwnerStore struct {
	store.Store
	users   map[string]*store.User
	agents  map[string]*store.Agent
	userErr map[string]bool
}

func (s *sourceOwnerStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.userErr[id] {
		return nil, errors.New("user lookup unavailable")
	}
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return s.Store.GetUser(ctx, id)
}

func (s *sourceOwnerStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if a, ok := s.agents[id]; ok {
		return a, nil
	}
	return s.Store.GetAgent(ctx, id)
}

// Each sharing-source owner shape resolves to its own activity outcome;
// every lookup failure and every inactive shape reports inactive.
func TestRelationshipSourceActive_OwnerShapes(t *testing.T) {
	base, s := authzTestSetup(t)
	ctx := context.Background()
	activeRoot := tid("srcact-root-active")
	suspendedRoot := tid("srcact-root-suspended")
	st := &sourceOwnerStore{
		Store: s,
		users: map[string]*store.User{
			activeRoot:    {ID: activeRoot, Status: store.UserStatusActive},
			suspendedRoot: {ID: suspendedRoot, Status: "suspended"},
		},
		agents: map[string]*store.Agent{
			tid("srcact-agent-ok"):         {ID: tid("srcact-agent-ok"), Ancestry: []string{activeRoot, tid("srcact-agent-ok")}},
			tid("srcact-agent-root-susp"):  {ID: tid("srcact-agent-root-susp"), Ancestry: []string{suspendedRoot, tid("srcact-agent-root-susp")}},
			tid("srcact-agent-deleted"):    {ID: tid("srcact-agent-deleted"), Ancestry: []string{activeRoot}, DeletedAt: time.Now()},
			tid("srcact-agent-no-chain"):   {ID: tid("srcact-agent-no-chain")},
			tid("srcact-agent-self-root"):  {ID: tid("srcact-agent-self-root"), Ancestry: []string{tid("srcact-agent-self-root")}},
			tid("srcact-agent-root-error"): {ID: tid("srcact-agent-root-error"), Ancestry: []string{tid("srcact-root-error")}},
		},
		userErr: map[string]bool{tid("srcact-user-error"): true, tid("srcact-root-error"): true},
	}
	authz := NewAuthzService(st, base.logger)

	for _, tc := range []struct {
		name, ownerID, detail string
		active                bool
	}{
		{"active user", activeRoot, "", true},
		{"suspended user", suspendedRoot, "sharing source owner is not active", false},
		{"user lookup error", tid("srcact-user-error"), "sharing source owner lookup failed", false},
		{"unknown owner", tid("srcact-unknown"), "sharing source owner not found", false},
		{"agent with active root", tid("srcact-agent-ok"), "", true},
		{"agent with suspended root", tid("srcact-agent-root-susp"), "sharing source owner agent's root user is not active", false},
		{"deleted agent", tid("srcact-agent-deleted"), "sharing source owner agent is deleted", false},
		{"agent without ancestry", tid("srcact-agent-no-chain"), "sharing source owner agent has no root user", false},
		{"agent that is its own root", tid("srcact-agent-self-root"), "sharing source owner agent has no root user", false},
		{"agent whose root lookup fails", tid("srcact-agent-root-error"), "sharing source owner agent's root user is not active", false},
		{"no owner", "", "sharing source has no owner", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active, detail := authz.relationshipSourceActive(ctx, tc.ownerID)
			assert.Equal(t, tc.active, active)
			assert.Equal(t, tc.detail, detail)
		})
	}
}

// Through Decide, an owner lookup error and an agent-owned source whose
// root user is suspended each reject the progeny candidate as
// source_inactive, while the active controls are admitted.
func TestRelationshipRules_ProgenySourceOwnerLookupAndAgentOwner(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	activeRoot := f.projectOwnerID
	suspendedRoot := tid("srcdec-root-suspended")
	lookupErrOwner := tid("srcdec-owner-error")
	okAgent := tid("srcdec-agent-ok")
	suspAgent := tid("srcdec-agent-root-susp")
	st := &sourceOwnerStore{
		Store: f.store,
		users: map[string]*store.User{suspendedRoot: {ID: suspendedRoot, Status: "suspended"}},
		agents: map[string]*store.Agent{
			okAgent:   {ID: okAgent, ProjectID: f.projectAlpha.ID, Ancestry: []string{activeRoot, okAgent}},
			suspAgent: {ID: suspAgent, Ancestry: []string{suspendedRoot, suspAgent}},
		},
		userErr: map[string]bool{lookupErrOwner: true},
	}
	authz := NewAuthzService(st, f.authz.logger)
	releaseBuiltinProgenyAdapter(t, authz, "secret")
	optIn := func(id, owner string) SharingSource {
		return SharingSource{Kind: "secret", ID: id, OwnerID: owner, Policy: SharingPolicyOptInRequired, OptedIn: true}
	}
	require.NoError(t, authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, sources: []SharingSource{
		optIn("s-user-ok", activeRoot),
		optIn("s-user-error", lookupErrOwner),
		optIn("s-agent-ok", okAgent),
		optIn("s-agent-root-susp", suspAgent),
	}}))
	seedExecutionAgent(t, f.store, tid("srcdec-reader"), f.projectAlpha.ID,
		[]string{activeRoot, lookupErrOwner, okAgent, suspAgent}, []string{activeRoot})
	reader := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("srcdec-reader")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{activeRoot, lookupErrOwner, okAgent, suspAgent},
		Scopes:    allRegisteredAgentScopes(),
	}}
	principal := principalContextForIdentity(reader)
	pred := authz.ProgenyListPredicate(ctx, principal, "secret")

	for _, tc := range []struct {
		id, detail string
		allowed    bool
	}{
		{"s-user-ok", "", true},
		{"s-user-error", "sharing source owner lookup failed", false},
		{"s-agent-ok", "", true},
		{"s-agent-root-susp", "sharing source owner agent's root user is not active", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			res := Resource{Type: "secret", ID: tc.id}
			d := decidePerm(authz, reader, res, ActionRead, permissionProjectSecretRead, true)
			assert.Equal(t, tc.allowed, d.Allowed, "reason %q", d.Reason)
			r := relationshipResult(t, d, RelationshipRuleProgeny)
			if tc.allowed {
				assert.True(t, r.Accepted)
			} else {
				assert.Equal(t, "relationship grant restricted by source_inactive", d.Reason)
				assert.Equal(t, RelationshipRejectSourceInactive, r.RejectedBy)
				assert.Equal(t, tc.detail, r.Detail)
			}
			assert.Equal(t, tc.allowed, decidePerm(authz, reader, res, ActionRead, permissionProjectSecretRead, false).Allowed)
			assert.Equal(t, tc.allowed, pred.Matches(optIn(tc.id, map[string]string{
				"s-user-ok": activeRoot, "s-user-error": lookupErrOwner, "s-agent-ok": okAgent, "s-agent-root-susp": suspAgent,
			}[tc.id])), "list predicate agrees")
		})
	}
}

// --- progeny list/point parity for registered kinds ---

// A kind with a registered adapter gets a progeny candidate on the point
// path, so point reads and ProgenyListPredicate evaluate the same kinds.
// The candidate carries the requested permission only when the adapter
// serves it.
func TestProgeny_RegisteredKindListAndPointParity(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-regkind-agent")},
		ProjectID: f.projectBeta.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	principal := principalContextForIdentity(agent)

	// No adapter for the kind: no progeny candidate.
	d := decidePerm(f.authz, agent, Resource{Type: "template", ID: "t-opted"}, ActionRead, "template.read", true)
	require.NotNil(t, d.Provenance)
	for _, r := range d.Provenance.Relationships {
		assert.NotEqual(t, RelationshipRuleProgeny, r.Rule)
	}

	sources := []SharingSource{
		{Kind: "template", ID: "t-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "template", ID: "t-not-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired},
	}
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "template", perms: []string{"template.read"}, sources: sources}))
	pred := f.authz.ProgenyListPredicate(ctx, principal, "template")
	for _, src := range sources {
		d := decidePerm(f.authz, agent, Resource{Type: "template", ID: src.ID}, ActionRead, "template.read", true)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.Equal(t, RelationshipRejectPolicy, r.RejectedBy, "source %s: template has no progeny row", src.ID)
		assert.Equal(t, pred.Matches(src), d.Allowed, "source %s: list and point read disagree", src.ID)
		assert.False(t, d.Allowed, "source %s", src.ID)
	}

	// A permission the adapter does not serve is rejected at the fact
	// stage, and the list predicate for the kind matches nothing.
	other := newGoldenFixture(t)
	releaseBuiltinProgenyAdapter(t, other.authz, "secret")
	secretSrc := SharingSource{Kind: "secret", ID: other.secretID, OwnerID: other.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
	require.NoError(t, other.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{"skill.read"}, sources: []SharingSource{secretSrc}}))
	seedExecutionAgent(t, other.store, tid("relrule-regkind-agent-2"), other.projectAlpha.ID,
		[]string{other.projectOwnerID}, []string{other.projectOwnerID})
	otherAgent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-regkind-agent-2")},
		ProjectID: other.projectAlpha.ID,
		Ancestry:  []string{other.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	d = decidePerm(other.authz, otherAgent, Resource{Type: "secret", ID: other.secretID}, ActionRead, permissionProjectSecretRead, true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.Equal(t, RelationshipRejectFact, r.RejectedBy)
	assert.Equal(t, "permission is not served by the sharing-source adapter", r.Detail)
	assert.False(t, other.authz.ProgenyListPredicate(ctx, principalContextForIdentity(otherAgent), "secret").Matches(secretSrc))
}

// changingPermsProgenyAdapter returns its perms slice itself, so a test
// can change the set the adapter reports after registration.
type changingPermsProgenyAdapter struct {
	fakeProgenyAdapter
}

func (c *changingPermsProgenyAdapter) ReadPermissions() []string { return c.perms }

// The read permissions validated at registration are the ones decisions
// use. An adapter that reports a different set after registration does not
// change the point or the list decision.
func TestProgeny_ReadPermissionsFixedAtRegistration(t *testing.T) {
	ctx := context.Background()
	newAgent := func(f *goldenFixture, name string) *agentIdentityWrapper {
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: tid(name)},
			ProjectID: f.projectAlpha.ID,
			Ancestry:  []string{f.projectOwnerID},
			Scopes:    allRegisteredAgentScopes(),
		}}
	}

	t.Run("registered set keeps serving", func(t *testing.T) {
		f := newGoldenFixture(t)
		agent := newAgent(f, "relrule-fixedperms-agent")
		seedExecutionAgent(t, f.store, tid("relrule-fixedperms-agent"), f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
		src := SharingSource{Kind: "secret", ID: "fixed-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
		adapter := &changingPermsProgenyAdapter{fakeProgenyAdapter{
			kind: "secret", perms: []string{permissionProjectSecretRead}, sources: []SharingSource{src},
		}}
		releaseBuiltinProgenyAdapter(t, f.authz, "secret")
		require.NoError(t, f.authz.RegisterProgenyAdapter(adapter))
		// Change the reported set in place and by replacement.
		adapter.perms[0] = "skill.read"
		adapter.perms = []string{"skill.read"}

		d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: src.ID}, ActionRead, permissionProjectSecretRead, true)
		assert.True(t, d.Allowed, "point read uses the registered set; reason %q", d.Reason)
		assert.True(t, f.authz.ProgenyListPredicate(ctx, principalContextForIdentity(agent), "secret").Matches(src),
			"list predicate uses the registered set")
	})

	t.Run("later set is not served", func(t *testing.T) {
		f := newGoldenFixture(t)
		agent := newAgent(f, "relrule-fixedperms-agent-2")
		seedExecutionAgent(t, f.store, tid("relrule-fixedperms-agent-2"), f.projectAlpha.ID, []string{f.projectOwnerID}, []string{f.projectOwnerID})
		src := SharingSource{Kind: "secret", ID: "fixed-opted-2", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
		adapter := &changingPermsProgenyAdapter{fakeProgenyAdapter{
			kind: "secret", perms: []string{"skill.read"}, sources: []SharingSource{src},
		}}
		releaseBuiltinProgenyAdapter(t, f.authz, "secret")
		require.NoError(t, f.authz.RegisterProgenyAdapter(adapter))
		adapter.perms[0] = permissionProjectSecretRead
		adapter.perms = []string{permissionProjectSecretRead}

		d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: src.ID}, ActionRead, permissionProjectSecretRead, true)
		assert.False(t, d.Allowed, "reason %q", d.Reason)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.Equal(t, RelationshipRejectFact, r.RejectedBy)
		assert.Equal(t, "permission is not served by the sharing-source adapter", r.Detail)
		assert.False(t, f.authz.ProgenyListPredicate(ctx, principalContextForIdentity(agent), "secret").Matches(src))
	})
}

// --- Actor and Purpose on every decision ---

// actorPathFailingStore fails the role-binding or role-definition lookup
// used by Decide steps 3 and 4.
type actorPathFailingStore struct {
	store.Store
	failBindings error
	failRoleDefs error
}

func (s *actorPathFailingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failBindings != nil {
		return nil, s.failBindings
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *actorPathFailingStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	if s.failRoleDefs != nil {
		return nil, s.failRoleDefs
	}
	return s.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

// Decide records Actor and Purpose on the decision for these return paths:
// a missing principal, a principal kind that does not match the identity,
// a federated service, a broker, an unresolvable permission (with and
// without explain), a token project mismatch before the kernel, a
// principal resolution error, a role-binding lookup error, a role
// definition lookup error, a kernel role-binding allow, a relationship
// allow with and without explain, a relationship deny at the project-access
// stage, and a deny with explain.
func TestDecide_ActorAndPurposeOnEveryReturnPath(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relrule-actor-path-owner"))
	adminID := tid("relrule-actor-path-admin")
	createTestUserWithRole(t, s, adminID, "actor-path-admin@relrule.test", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "actor-path-admin@relrule.test", "Admin", "admin", "api")
	federatedUser := NewFederatedUserIdentity("https://issuer.example", "actor-path-user", "u@relrule.test", "User", "member", nil)
	federatedService := NewFederatedServiceIdentity("https://issuer.example", "actor-path-service", "svc@relrule.test", nil)
	broker := NewBrokerIdentity(tid("relrule-actor-path-broker"))
	lookupErr := errors.New("unavailable")
	bindingsFail := NewAuthzService(&actorPathFailingStore{Store: s, failBindings: lookupErr}, authz.logger)
	roleDefsFail := NewAuthzService(&actorPathFailingStore{Store: s, failRoleDefs: lookupErr}, authz.logger)
	projectID := tid("relrule-actor-path-proj")
	agent := agentResource(&store.Agent{ID: tid("relrule-actor-path-agent"), ProjectID: projectID, OwnerID: owner.ID()})
	// The owner relationship requires active project access
	// (ptone/scion#2141); the binding grants no permission itself.
	grantProjectAccessOnly(t, s, owner.ID(), projectID)
	// An owner without project access is denied at the project-access stage.
	formerOwner := createCharacterizationUser(t, s, tid("relrule-actor-path-former-owner"))
	formerAgent := agentResource(&store.Agent{ID: tid("relrule-actor-path-former-agent"), ProjectID: projectID, OwnerID: formerOwner.ID()})
	otherProjectToken := NewScopedUserIdentity(owner, tid("relrule-actor-path-other-proj"), []string{"agent:read"})
	actor := &DecisionActor{Kind: PrincipalKindAgent, ID: tid("relrule-actor-path-actor")}

	request := func(ident Identity, res Resource, action Action, perm string, explain bool) AuthzRequest {
		req := AuthzRequest{Resource: res, Action: action, Permission: perm, Explain: explain, Actor: actor, Purpose: "delivery"}
		if ident != nil {
			req.Principal = principalContextForIdentity(ident)
			req.Credential = credentialContextForIdentity(ident)
		}
		return req
	}
	kindMismatch := request(owner, agent, ActionRead, "agent.read", false)
	kindMismatch.Principal.Kind = PrincipalKindBroker
	for _, tc := range []struct {
		name    string
		authz   *AuthzService
		req     AuthzRequest
		allowed bool
		reason  string
	}{
		{"missing principal", nil, request(nil, agent, ActionRead, "agent.read", false), false, "missing principal"},
		{"principal kind mismatch", nil, kindMismatch, false, "principal kind does not match identity"},
		{"federated service", nil, request(federatedService, agent, ActionRead, "agent.read", false), false, "federated service identities are not supported"},
		{"broker", nil, request(broker, agent, ActionRead, "agent.read", false), false, "broker identities are not supported by authorization"},
		{"unresolvable permission", nil, request(owner, Resource{Type: "no_such_type", ID: "x"}, ActionRead, "", false), false, unresolvablePermissionReason},
		{"unresolvable permission explain", nil, request(owner, Resource{Type: "no_such_type", ID: "x"}, ActionRead, "", true), false, unresolvablePermissionReason},
		{"token project mismatch", nil, request(otherProjectToken, agent, ActionRead, "agent.read", false), false, "token not scoped for this project"},
		{"principal resolution error", nil, request(federatedUser, agent, ActionRead, "agent.read", true), false, "principal resolution error (fail-closed)"},
		{"role-binding lookup error", bindingsFail, request(admin, agent, ActionRead, "agent.read", true), false, "binding resolution error (fail-closed)"},
		{"role definition lookup error", roleDefsFail, request(admin, agent, ActionRead, "agent.read", true), false, "role resolution error (fail-closed)"},
		{"kernel role-binding allow", nil, request(admin, agent, ActionRead, "agent.read", false), true, "role binding grant"},
		{"relationship allow", nil, request(owner, agent, ActionRead, "agent.read", false), true, "relationship grant: resource owner"},
		{"relationship allow explain", nil, request(owner, agent, ActionRead, "agent.read", true), true, "relationship grant: resource owner"},
		{"relationship deny explain", nil, request(owner, agent, ActionUpdate, "hub.config.update", true), false, ""},
		{"relationship project-access deny", nil, request(formerOwner, formerAgent, ActionRead, "agent.read", false), false, "relationship grant restricted by " + RelationshipRejectProjectAccess},
		{"relationship project-access deny explain", nil, request(formerOwner, formerAgent, ActionRead, "agent.read", true), false, "relationship grant restricted by " + RelationshipRejectProjectAccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := authz
			if tc.authz != nil {
				svc = tc.authz
			}
			d := svc.Decide(context.Background(), tc.req)
			assert.Equal(t, tc.allowed, d.Allowed, "reason %q", d.Reason)
			if tc.reason != "" {
				assert.Equal(t, tc.reason, d.Reason)
			}
			assert.Equal(t, actor, d.Actor)
			assert.Equal(t, "delivery", d.Purpose)
			if d.Provenance != nil {
				assert.Equal(t, actor, d.Provenance.Actor)
				assert.Equal(t, "delivery", d.Provenance.Purpose)
			}
		})
	}
}
