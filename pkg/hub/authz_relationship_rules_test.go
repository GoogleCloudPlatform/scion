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

package hub

// Tests for the common relationship stage (ptone/scion#2119): every
// relationship candidate passes the relationship policy, hub-attested
// ancestry, relationship fact, source activity and request restrictions.

import (
	"context"
	"errors"
	"testing"

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

	out := authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(fed), desc, Action("notify"), "agent.notify", nil, false)
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
	out = authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(fedUser), userDesc, ActionRead, "agent.read", nil, false)
	assert.Nil(t, out.accepted)
	require.NotEmpty(t, out.results)
	assert.Equal(t, RelationshipRuleAncestor, out.results[0].Rule)
	assert.Equal(t, RelationshipRejectUntrustedAncestry, out.results[0].RejectedBy)

	// The same shape with a hub-attested agent is accepted by the stage.
	local := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: tid("relrule-local")}, Scopes: allRegisteredAgentScopes()}}
	localDesc := agentResource(&store.Agent{ID: tid("relrule-local-desc"), Ancestry: []string{tid("relrule-fed-root"), local.ID()}})
	out = authz.evaluateRelationshipCandidates(ctx, principalContextForIdentity(local), localDesc, Action("notify"), "agent.notify", nil, false)
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
	}{
		"ancestor": {RelationshipRuleAncestor, agentResource(&store.Agent{
			ID: tid("relrule-fedrow-desc"), ProjectID: f.projectBeta.ID,
			Ancestry: []string{f.projectOwnerID, fed.ID()},
		}), Action("notify"), "agent.notify"},
		"creator_user_skill": {RelationshipRuleCreatorUserSkill, skillResource(&store.Skill{
			ID: tid("relrule-fedrow-skill"), Scope: store.SkillScopeUser, ScopeID: f.projectOwnerID,
		}), ActionRead, "skill.read"},
		"progeny": {RelationshipRuleProgeny, Resource{Type: "secret", ID: f.secretID}, ActionRead, permissionProjectSecretRead},
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
			out := f.authz.evaluateRelationshipCandidates(ctx, principal, tc.resource, tc.action, tc.perm, nil, false)
			assert.Nil(t, out.accepted)
			found := false
			for _, r := range out.results {
				if r.Rule == tc.rule {
					found = true
					assert.False(t, r.Accepted)
					assert.Equal(t, RelationshipRejectUntrustedAncestry, r.RejectedBy)
				}
			}
			assert.True(t, found, "candidate %q must be evaluated", tc.rule)
			assert.False(t, decidePerm(f.authz, fed, tc.resource, tc.action, tc.perm, false).Allowed)
		})
	}
}

// A progeny read requires the sharing source's owner to be active.
func TestRelationshipRules_ProgenySourceInactive(t *testing.T) {
	f := newGoldenFixture(t)
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-progeny-agent")},
		ProjectID: f.projectBeta.ID,
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

// The creator user-skill read requires an active origin user.
func TestRelationshipRules_CreatorSkillSourceInactive(t *testing.T) {
	f := newGoldenFixture(t)
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-skill-agent")},
		ProjectID: f.projectBeta.ID,
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
	assert.Equal(t, RelationshipRejectSourceInactive, relationshipResult(t, d, RelationshipRuleCreatorUserSkill).RejectedBy)
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
		RelationshipRuleHubMemberSAAssign, RelationshipRuleCreatorUserSkill,
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

// A registered adapter replaces the built-in store adapter for its kind.
// Opted-in sources of an active owner are readable; others are not; a
// lookup error denies.
func TestProgenyAdapter_RegisteredSourcesDecide(t *testing.T) {
	f := newGoldenFixture(t)
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-adapter-agent")},
		ProjectID: f.projectBeta.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	adapter := fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, sources: []SharingSource{
		{Kind: "secret", ID: "opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "not-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired},
	}}
	require.NoError(t, f.authz.RegisterProgenyAdapter(adapter))

	d := decidePerm(f.authz, agent, Resource{Type: "secret", ID: "opted"}, ActionRead, permissionProjectSecretRead, false)
	assert.True(t, d.Allowed, "reason %q", d.Reason)
	d = decidePerm(f.authz, agent, Resource{Type: "secret", ID: "not-opted"}, ActionRead, permissionProjectSecretRead, true)
	assert.False(t, d.Allowed)
	assert.Equal(t, RelationshipRejectFact, relationshipResult(t, d, RelationshipRuleProgeny).RejectedBy)

	failing, _ := authzTestSetup(t)
	require.NoError(t, failing.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, err: errors.New("unavailable")}))
	d = decidePerm(failing, agent, Resource{Type: "secret", ID: "opted"}, ActionRead, permissionProjectSecretRead, false)
	assert.False(t, d.Allowed, "a sharing-source lookup failure denies")
}

// List filtering and point reads agree for every fixture source.
func TestProgeny_ListAndPointReadConsistent(t *testing.T) {
	f := newGoldenFixture(t)
	suspendedID := tid("relrule-suspended-owner")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: suspendedID, Email: "suspended@relrule.test", DisplayName: "s", Role: "member", Status: "suspended",
	}))
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relrule-consistency-agent")},
		ProjectID: f.projectBeta.ID,
		Ancestry:  []string{f.projectOwnerID, suspendedID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	sources := []SharingSource{
		{Kind: "secret", ID: "s-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "s-not-opted", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired},
		{Kind: "secret", ID: "s-foreign", OwnerID: f.memberNoneID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "s-suspended", OwnerID: suspendedID, Policy: SharingPolicyOptInRequired, OptedIn: true},
		{Kind: "secret", ID: "s-origin", OwnerID: f.projectOwnerID, Policy: SharingPolicyOriginDescendants},
		{Kind: "secret", ID: "s-origin-mid", OwnerID: suspendedID, Policy: SharingPolicyOriginDescendants},
		{Kind: "secret", ID: "s-unknown-policy", OwnerID: f.projectOwnerID, Policy: SharingPolicy("other"), OptedIn: true},
	}
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "secret", perms: []string{permissionProjectSecretRead}, sources: sources}))

	ctx := context.Background()
	principal := principalContextForIdentity(agent)
	pred := f.authz.ProgenyListPredicate(ctx, principal, "secret")
	want := map[string]bool{"s-opted": true, "s-origin": true}
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

	// A kind without a progeny policy row gets a predicate that matches
	// nothing even with a registered adapter.
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "envvar", perms: []string{"skill.read"}, sources: sources}))
	assert.False(t, f.authz.ProgenyListPredicate(ctx, principal, "envvar").Matches(SharingSource{Kind: "envvar", ID: "e", OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true}))
}

// Actor and Purpose are recorded in provenance and do not change the
// decision.
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
			if d.Provenance != nil {
				assert.Equal(t, actor, d.Provenance.Actor)
				assert.Equal(t, "delivery", d.Provenance.Purpose)
			}
		}
	}
}
