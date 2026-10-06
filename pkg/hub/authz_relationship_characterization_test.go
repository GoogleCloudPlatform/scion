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

// Characterization of the named relationship grants (ptone/scion#2119).
//
// relationshipCharacterizedAllowlist is the frozen table of which registered
// permissions each relationship admits, per principal kind and resource type.
// It is the single source the permissions.RelationshipPolicies rows are
// checked against (TestRelationshipPolicy_MatchesCharacterization), so a
// relationship can only admit a permission that is listed here.
//
// The tests in this file evaluate every same-type registry permission through
// Decide for a principal whose only role binding, where a project-scoped
// target needs one, grants no permissions (grantProjectAccessOnly), so the only
// possible grant source is the relationship under test.

import (
	"context"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relationshipAllowKey identifies one (relationship, principal kind,
// resource type) cell of the characterization table. PrincipalKind uses the
// relationship-policy vocabulary ("user", "agent").
type relationshipAllowKey struct {
	Relationship  string
	PrincipalKind string
	ResourceType  string
}

// relationshipUnregisteredPermissions lists permission IDs that a production
// call site derives from a (resource type, action) pair with no registry
// entry, and that a relationship admits. Each entry names its call sites.
// These IDs are accepted by the permission resolver and by the relationship
// policy consistency test only through this list.
var relationshipUnregisteredPermissions = map[string][]string{}

// relationshipCrossTypePermissions lists relationship cells whose resource
// type differs from the registry resource of the permission they admit. The
// value is the production call site that makes that request.
var relationshipCrossTypePermissions = map[relationshipAllowKey]map[string]string{
	{Relationship: "progeny", PrincipalKind: "agent", ResourceType: "secret"}: {
		"project.secret_read": "httpdispatcher.go (agent secret resolution: Resource{Type: \"secret\"}, Permission: project.secret_read)",
	},
}

// relationshipCharacterizedAllowlist is the frozen per-cell permission list.
var relationshipCharacterizedAllowlist = map[relationshipAllowKey][]string{
	// Resource owner (user principals). Every same-type registry permission.
	// Hub-scoped gcp_service_account assign is excluded by the rule shape
	// (see TestRelationshipCharacterization_Owner) and is covered by
	// hub_member_sa_assign instead.
	{"owner", "user", "agent"}: {
		"agent.create", "agent.read", "agent.list", "agent.update", "agent.delete",
		"agent.attach", "agent.lifecycle", "agent.port_access", "agent.stop_all",
		"agent.message", "agent.set_message_mode", "agent.grant_hub_mode",
		"agent.status_update", "agent.log_append", "agent.notify",
		"agent.token_refresh", "agent.port_forward", "agent.identity_token",
	},
	// No {"owner", "user", "project"} cell: Project.OwnerID grants nothing
	// (ptone/scion#2586); see TestRelationshipCharacterization_OwnerProjectGrantsNothing.
	{"owner", "user", "template"}: {
		"template.create", "template.read", "template.update", "template.delete", "template.list",
	},
	{"owner", "user", "harness_config"}: {
		"harness_config.create", "harness_config.read", "harness_config.update",
		"harness_config.delete", "harness_config.list",
	},
	{"owner", "user", "group"}: {
		"group.create", "group.read", "group.update", "group.delete", "group.list",
		"group.addMember", "group.removeMember",
	},
	{"owner", "user", "broker"}: {
		"broker.create", "broker.read", "broker.update", "broker.delete", "broker.list", "broker.dispatch",
	},
	{"owner", "user", "gcp_service_account"}: {
		"gcp_service_account.create", "gcp_service_account.read", "gcp_service_account.delete",
		"gcp_service_account.list", "gcp_service_account.verify", "gcp_service_account.mint",
		"gcp_service_account.assign",
	},
	{"owner", "user", "skill"}: {
		"skill.create", "skill.create_global", "skill.read", "skill.update", "skill.delete",
		"skill.list", "skill.register",
	},

	// Ancestor (a principal in the agent resource's creation chain).
	{"ancestor", "user", "agent"}: {
		"agent.create", "agent.read", "agent.list", "agent.update", "agent.delete",
		"agent.attach", "agent.lifecycle", "agent.port_access", "agent.stop_all",
		"agent.message", "agent.set_message_mode", "agent.grant_hub_mode",
		"agent.status_update", "agent.log_append", "agent.notify",
		"agent.token_refresh", "agent.port_forward", "agent.identity_token",
	},
	// Agent ancestors are further limited by their JWT scopes; this cell is
	// the set reachable when the agent holds every registered agent scope.
	{"ancestor", "agent", "agent"}: {
		"agent.create", "agent.delete", "agent.attach", "agent.lifecycle",
		"agent.set_message_mode", "agent.status_update", "agent.log_append",
		"agent.notify", "agent.token_refresh", "agent.port_forward", "agent.identity_token",
	},

	// Progeny read of an ancestor's opted-in user-scoped secret, plus the
	// reviewed exact pairs (reviewedProgenyExactPairs): runtime use and launch
	// delivery of opted-in user-scope secrets and env vars.
	{"progeny", "agent", "secret"}:  {"project.secret_read", "secret.use", "secret.deliver"},
	{"progeny", "agent", "env_var"}: {"env_var.deliver"},

	// Current hub members may assign hub-scoped service accounts.
	{"hub_member_sa_assign", "user", "gcp_service_account"}: {"gcp_service_account.assign"},

	// An agent may read its origin user's personal (user-scoped) skills,
	// through the same progeny relationship as opted-in secrets, keyed on
	// the skill's owning bucket rather than a per-record creator field.
	{"progeny", "agent", "skill"}: {"skill.read"},
}

// sameTypeRegistryPermissions returns every registry permission ID whose
// Resource is resourceType, in registry order.
func sameTypeRegistryPermissions(resourceType string) []permissions.Permission {
	var out []permissions.Permission
	for _, p := range permissions.Registry {
		if p.Resource == resourceType {
			out = append(out, p)
		}
	}
	return out
}

func characterizedSet(key relationshipAllowKey) map[string]bool {
	set := map[string]bool{}
	for _, id := range relationshipCharacterizedAllowlist[key] {
		set[id] = true
	}
	return set
}

// allRegisteredAgentScopes returns every agent JWT scope referenced by the
// registry, so an agent carrying them is limited only by the relationship.
func allRegisteredAgentScopes() []AgentTokenScope {
	seen := map[string]bool{}
	var out []AgentTokenScope
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if !seen[s] {
				seen[s] = true
				out = append(out, AgentTokenScope(s))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func decideExplicit(t *testing.T, authz *AuthzService, identity Identity, resource Resource, p permissions.Permission) Decision {
	t.Helper()
	return authz.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   resource,
		Action:     Action(p.Action),
		Permission: p.ID,
	})
}

func createCharacterizationUser(t *testing.T, s store.Store, id string) UserIdentity {
	t.Helper()
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: id, Email: id + "@relchar.test", DisplayName: id, Role: "member", Status: "active",
	}))
	return NewAuthenticatedUser(id, id+"@relchar.test", id, "member", "api")
}

// grantProjectAccessOnly binds userID in projectID to a project-scoped role
// with no permissions. The binding is project membership (active project
// access, which owner and ancestor relationships on project targets
// require; ptone/scion#2141) without granting any permission through the
// role, so the relationship stays the only grant source under test.
func grantProjectAccessOnly(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	name := "relchar-access-only"
	rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		rd, err = s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: api.NewUUID(), Name: name, Description: "project access, no permissions",
			ScopeType: store.RoleScopeProject, Permissions: []string{},
		})
		require.NoError(t, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// TestRelationshipCharacterization_Owner pins the owner relationship for a
// user whose only role binding is an access-only binding in the project of
// the project-scoped targets, across every resource type whose constructor
// sets OwnerID.
func TestRelationshipCharacterization_Owner(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relchar-owner"))
	other := createCharacterizationUser(t, s, tid("relchar-other"))
	projectID := tid("relchar-project")
	grantProjectAccessOnly(t, s, owner.ID(), projectID)

	resources := []struct {
		name     string
		resource Resource
	}{
		{"agent", agentResource(&store.Agent{ID: tid("relchar-agent"), ProjectID: projectID, OwnerID: owner.ID()})},
		{"template", templateResource(&store.Template{ID: tid("relchar-tpl"), OwnerID: owner.ID(), Scope: store.TemplateScopeUser, ScopeID: owner.ID()})},
		{"harness_config", harnessConfigResource(&store.HarnessConfig{ID: tid("relchar-hc"), OwnerID: owner.ID(), Scope: store.HarnessConfigScopeUser, ScopeID: owner.ID()})},
		{"group", groupResource(&store.Group{ID: tid("relchar-group"), OwnerID: owner.ID()})},
		{"broker", brokerResource(&store.RuntimeBroker{ID: tid("relchar-broker"), CreatedBy: owner.ID()})},
		{"gcp_service_account", gcpServiceAccountResource(&store.GCPServiceAccount{ID: tid("relchar-sa"), CreatedBy: owner.ID(), Scope: store.ScopeProject, ScopeID: projectID})},
		{"skill", skillResource(&store.Skill{ID: tid("relchar-skill"), OwnerID: owner.ID(), Scope: store.SkillScopeUser, ScopeID: owner.ID()})},
	}

	for _, tc := range resources {
		want := characterizedSet(relationshipAllowKey{"owner", "user", tc.resource.Type})
		require.NotEmpty(t, want, "no characterized owner cell for %s", tc.resource.Type)
		for _, p := range sameTypeRegistryPermissions(tc.resource.Type) {
			t.Run(tc.name+"/"+p.ID, func(t *testing.T) {
				d := decideExplicit(t, authz, owner, tc.resource, p)
				assert.Equal(t, want[p.ID], d.Allowed, "owner %s: reason %q", p.ID, d.Reason)
				if want[p.ID] {
					assert.Equal(t, "relationship grant: resource owner", d.Reason)
				}
				// A different user is not the owner and has no bindings.
				d = decideExplicit(t, authz, other, tc.resource, p)
				assert.False(t, d.Allowed, "non-owner %s: reason %q", p.ID, d.Reason)
			})
		}
	}

	// Hub-scoped service accounts: the owner relationship does not admit
	// assign; every other same-type permission is admitted.
	hubSA := gcpServiceAccountResource(&store.GCPServiceAccount{ID: tid("relchar-hub-sa"), CreatedBy: owner.ID(), Scope: store.ScopeHub, ScopeID: "hub"})
	require.Empty(t, hubSA.ParentType)
	want := characterizedSet(relationshipAllowKey{"owner", "user", "gcp_service_account"})
	for _, p := range sameTypeRegistryPermissions("gcp_service_account") {
		d := decideExplicit(t, authz, owner, hubSA, p)
		if p.ID == "gcp_service_account.assign" {
			assert.False(t, d.Allowed, "hub-scoped assign requires hub membership: reason %q", d.Reason)
			continue
		}
		assert.Equal(t, want[p.ID], d.Allowed, "hub-scoped SA owner %s: reason %q", p.ID, d.Reason)
	}
}

// TestRelationshipCharacterization_OwnerProjectGrantsNothing pins
// ptone/scion#2586: Project.OwnerID is not an authorization source. A user
// named as the project's OwnerID but holding no project role binding (for
// example a creator removed without an ownership transfer) is denied every
// project permission, and no owner relationship candidate is accepted.
func TestRelationshipCharacterization_OwnerProjectGrantsNothing(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("relchar-proj-owner"))
	project := projectResource(&store.Project{ID: tid("relchar-proj"), OwnerID: owner.ID()})

	assert.Empty(t, characterizedSet(relationshipAllowKey{"owner", "user", "project"}))
	perms := sameTypeRegistryPermissions("project")
	require.NotEmpty(t, perms)
	for _, p := range perms {
		t.Run(p.ID, func(t *testing.T) {
			d := decideExplicit(t, authz, owner, project, p)
			assert.False(t, d.Allowed, "OwnerID-only %s: reason %q", p.ID, d.Reason)
			assert.NotEqual(t, "owner", d.MatchedGrant, "no owner relationship grant may match")
			assert.NotEqual(t, ScopeTypeRelationship, d.Scope, "no relationship rule may decide")
		})
	}
}

// TestRelationshipCharacterization_UserAncestor pins the ancestor
// relationship for a user in an agent's creation chain.
func TestRelationshipCharacterization_UserAncestor(t *testing.T) {
	authz, s := authzTestSetup(t)
	root := createCharacterizationUser(t, s, tid("relchar-root"))
	outsider := createCharacterizationUser(t, s, tid("relchar-outsider"))
	grantProjectAccessOnly(t, s, root.ID(), tid("relchar-desc-project"))
	descendant := agentResource(&store.Agent{
		ID:        tid("relchar-desc"),
		ProjectID: tid("relchar-desc-project"),
		Ancestry:  []string{root.ID(), tid("relchar-parent-agent")},
	})

	want := characterizedSet(relationshipAllowKey{"ancestor", "user", "agent"})
	for _, p := range sameTypeRegistryPermissions("agent") {
		t.Run(p.ID, func(t *testing.T) {
			d := decideExplicit(t, authz, root, descendant, p)
			assert.Equal(t, want[p.ID], d.Allowed, "ancestor %s: reason %q", p.ID, d.Reason)
			if want[p.ID] {
				assert.Equal(t, "relationship grant: ancestor access", d.Reason)
			}
			d = decideExplicit(t, authz, outsider, descendant, p)
			assert.False(t, d.Allowed, "outsider %s: reason %q", p.ID, d.Reason)
		})
	}
}

// TestRelationshipCharacterization_AgentAncestor pins the ancestor
// relationship for a hub-attested agent in another agent's creation chain,
// with the descendant in a different project so no JWT-derived project
// binding applies.
func TestRelationshipCharacterization_AgentAncestor(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ancestorID := tid("relchar-anc-agent")
	ancestor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ancestorID},
		ProjectID: tid("relchar-anc-project"),
		Scopes:    allRegisteredAgentScopes(),
	}}
	require.True(t, AncestryIsHubAttested(ancestor))
	descendant := agentResource(&store.Agent{
		ID:        tid("relchar-anc-desc"),
		ProjectID: tid("relchar-other-project"),
		Ancestry:  []string{tid("relchar-anc-root"), ancestorID},
	})

	want := characterizedSet(relationshipAllowKey{"ancestor", "agent", "agent"})
	for _, p := range sameTypeRegistryPermissions("agent") {
		t.Run(p.ID, func(t *testing.T) {
			d := decideExplicit(t, authz, ancestor, descendant, p)
			assert.Equal(t, want[p.ID], d.Allowed, "agent ancestor %s: reason %q", p.ID, d.Reason)
			if want[p.ID] {
				assert.Equal(t, "relationship grant: ancestor access", d.Reason)
			}
		})
	}
}

// TestRelationshipCharacterization_AgentFullHistory pins the full message
// and log history check made by the message and log handlers: it is the
// exact registered permission agent.attach, admitted for the owner, a user
// ancestor and an agent ancestor under the registered ancestor rule, and
// not for an unrelated user. The
// (agent, manage) pair resolves to no permission and denies for every
// caller.
func TestRelationshipCharacterization_AgentFullHistory(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	owner := createCharacterizationUser(t, s, tid("relchar-m-owner"))
	root := createCharacterizationUser(t, s, tid("relchar-m-root"))
	other := createCharacterizationUser(t, s, tid("relchar-m-other"))
	grantProjectAccessOnly(t, s, owner.ID(), tid("relchar-m-project"))
	grantProjectAccessOnly(t, s, root.ID(), tid("relchar-m-project"))
	ancestorID := tid("relchar-m-anc-agent")
	ancestor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ancestorID},
		ProjectID: tid("relchar-m-anc-project"),
		Scopes:    allRegisteredAgentScopes(),
	}}
	res := agentResource(&store.Agent{
		ID:        tid("relchar-m-agent"),
		ProjectID: tid("relchar-m-project"),
		OwnerID:   owner.ID(),
		Ancestry:  []string{root.ID(), ancestorID},
	})

	var attach permissions.Permission
	for _, p := range permissions.Registry {
		if p.ID == "agent.attach" {
			attach = p
		}
	}
	require.Equal(t, "agent.attach", attach.ID, "agent.attach must be registered")
	assert.True(t, decideExplicit(t, authz, owner, res, attach).Allowed)
	assert.True(t, decideExplicit(t, authz, root, res, attach).Allowed)
	assert.True(t, decideExplicit(t, authz, ancestor, res, attach).Allowed)
	assert.False(t, decideExplicit(t, authz, other, res, attach).Allowed)

	for _, caller := range []Identity{owner, root, ancestor, other} {
		assert.False(t, authz.CheckAccess(ctx, caller, res, ActionManage).Allowed,
			"(agent, manage) resolves to no permission for %s", caller.ID())
	}
}

// TestRelationshipCharacterization_Progeny pins the progeny read of an
// ancestor's opted-in user-scoped secret, as requested by agent secret
// resolution.
func TestRelationshipCharacterization_Progeny(t *testing.T) {
	f := newGoldenFixture(t)
	seedExecutionAgent(t, f.store, tid("relchar-progeny-agent"), f.projectAlpha.ID,
		[]string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relchar-progeny-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	secretRes := Resource{Type: "secret", ID: f.secretID}
	p := permissions.Permission{ID: "project.secret_read", Action: string(ActionRead)}

	d := decideExplicit(t, f.authz, agent, secretRes, p)
	assert.True(t, d.Allowed, "progeny secret read: reason %q", d.Reason)
	assert.Equal(t, "relationship grant: progeny_secret_read", d.Reason)

	// An agent whose ancestry does not include the secret's creator.
	unrelated := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relchar-progeny-unrelated")},
		ProjectID: f.projectBeta.ID,
		Ancestry:  []string{f.memberNoneID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	d = decideExplicit(t, f.authz, unrelated, secretRes, p)
	assert.False(t, d.Allowed, "unrelated agent: reason %q", d.Reason)
}

// TestRelationshipCharacterization_HubMemberSAAssign pins assign on a
// hub-scoped service account for a current hub member who is not the
// account's creator.
func TestRelationshipCharacterization_HubMemberSAAssign(t *testing.T) {
	f := newGoldenFixture(t)
	member := NewAuthenticatedUser(f.memberNoneID, "member-none@golden.test", "Member None", "member", "api")
	hubSA := gcpServiceAccountResource(&store.GCPServiceAccount{ID: tid("relchar-hub-assign-sa"), CreatedBy: f.projectOwnerID, Scope: store.ScopeHub, ScopeID: "hub"})
	for _, p := range sameTypeRegistryPermissions("gcp_service_account") {
		if p.ID != "gcp_service_account.assign" {
			continue
		}
		d := decideExplicit(t, f.authz, member, hubSA, p)
		assert.True(t, d.Allowed, "hub member assign: reason %q", d.Reason)
		assert.Equal(t, "relationship grant: hub member hub-scoped assign", d.Reason)
	}
}

// TestRelationshipCharacterization_ProgenySkillRead pins an agent's read of
// its origin user's personal (user-scoped) skill through the common progeny
// grant (ptone/scion#2128 retired the dedicated creator-user-skill grant).
func TestRelationshipCharacterization_ProgenySkillRead(t *testing.T) {
	f := newGoldenFixture(t)
	seedExecutionAgent(t, f.store, tid("relchar-skill-agent"), f.projectAlpha.ID,
		[]string{f.projectOwnerID}, []string{f.projectOwnerID})
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("relchar-skill-agent")},
		ProjectID: f.projectAlpha.ID,
		Ancestry:  []string{f.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	res := skillScopeResource(store.SkillScopeUser, f.projectOwnerID)
	d := f.authz.CheckAccess(context.Background(), agent, res, ActionRead)
	assert.True(t, d.Allowed, "progeny skill read: reason %q", d.Reason)
	assert.Equal(t, "relationship grant: progeny_skill_read", d.Reason)

	other := skillScopeResource(store.SkillScopeUser, f.memberNoneID)
	d = f.authz.CheckAccess(context.Background(), agent, other, ActionRead)
	assert.False(t, d.Allowed, "another user's skill: reason %q", d.Reason)
}

// TestRelationshipCharacterization_TableCoversRegistry asserts that every
// characterized permission is registered, apart from the reviewed
// unregistered IDs, and that each same-type cell names only permissions of
// its own resource type apart from the reviewed cross-type cells.
func TestRelationshipCharacterization_TableCoversRegistry(t *testing.T) {
	registered := map[string]permissions.Permission{}
	for _, p := range permissions.Registry {
		registered[p.ID] = p
	}
	for key, ids := range relationshipCharacterizedAllowlist {
		for _, id := range ids {
			p, ok := registered[id]
			if !ok {
				_, reviewed := relationshipUnregisteredPermissions[id]
				assert.True(t, reviewed, "%v: %s is neither registered nor a reviewed unregistered ID", key, id)
				continue
			}
			if p.Resource != key.ResourceType {
				_, reviewed := relationshipCrossTypePermissions[key][id]
				assert.True(t, reviewed, "%v: %s has registry resource %s", key, id, p.Resource)
			}
		}
	}
}
