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
	"reflect"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canDelegateParityFixture is the shared store state for
// TestCanDelegate_ParityForOtherIdentityTypes: one project whose admin is a
// human user, and one group bound to a single-permission system role so the
// group-membership descriptor reaches the per-actor permission check with a
// deterministic reason.
type canDelegateParityFixture struct {
	projectID string
	userID    string
	groupID   string
}

func newCanDelegateParityFixture(t *testing.T, s store.Store) canDelegateParityFixture {
	t.Helper()
	ctx := context.Background()
	f := canDelegateParityFixture{
		projectID: tid("candelegate-parity-project"),
		userID:    tid("candelegate-parity-user"),
		groupID:   tid("candelegate-parity-group"),
	}

	createDelegateTestProject(t, s, f.projectID, "candelegate-parity-project", tid("candelegate-parity-owner"))
	createTestUserWithProjectRole(t, s, f.userID, "parity-user@test.com", f.projectID, store.ProjectRoleAdmin)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: f.groupID, Slug: "candelegate-parity-group", Name: "CanDelegate Parity Group",
	}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "candelegate-parity-system-role",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"agent.read"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      f.groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return f
}

// identities returns one fixture per key of identityInventoryExpectation
// other than hubDeliveryIdentity, keyed by the concrete type name.
func (f canDelegateParityFixture) identities() map[string]Identity {
	user := NewAuthenticatedUser(f.userID, "parity-user@test.com", "Parity User", "member", "api")
	storedAgent := func(name string) *store.Agent {
		return &store.Agent{ID: tid(name), ProjectID: f.projectID, Ancestry: []string{f.userID}}
	}
	return map[string]Identity{
		"AuthenticatedUser":  user,
		"ScopedUserIdentity": NewScopedUserIdentity(user, f.projectID, []string{"agent:read", "agent:create"}),
		"DevUser":            NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"}),
		"agentIdentityWrapper": &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: tid("candelegate-parity-jwt-agent"), ID: "candelegate-parity-jti"},
			ProjectID: f.projectID,
			Scopes:    ScopesForRole(AgentRoleFull),
		}},
		"storedAgentIdentity":  &storedAgentIdentity{agent: storedAgent("candelegate-parity-stored-agent")},
		"peerAgentIdentity":    &peerAgentIdentity{agent: storedAgent("candelegate-parity-peer-agent")},
		"explainAgentIdentity": newAgentIdentityFromStore(storedAgent("candelegate-parity-explain-agent")),
		"brokerIdentityImpl":   NewBrokerIdentity(tid("candelegate-parity-broker")),
		"FederatedUserIdentity": NewFederatedUserIdentity("https://issuer.example", "parity-sub",
			"parity-fed@example.com", "Parity Fed", "member", nil),
		"FederatedAgentIdentity": NewFederatedAgentIdentity("https://issuer.example", "parity-remote-agent",
			"parity-remote-project", "Parity Remote Agent", f.userID, []string{f.userID}, ScopesForRole(AgentRoleFull)),
		"FederatedServiceIdentity": NewFederatedServiceIdentity("https://issuer.example", "parity-sa-sub",
			"parity-sa@example.com", nil),
	}
}

// grants returns the fixed descriptor set: one per GrantType, with the
// agent-delegation descriptor requesting AgentRoleNone and no AgentScopes.
func (f canDelegateParityFixture) grants() map[GrantType]GrantDescriptor {
	return map[GrantType]GrantDescriptor{
		GrantTypeRoleBinding: {
			Type:            GrantTypeRoleBinding,
			RolePermissions: []string{"agent.read"},
			ScopeType:       store.RoleScopeProject,
			ScopeID:         f.projectID,
		},
		GrantTypeGroupMembership: {
			Type:    GrantTypeGroupMembership,
			GroupID: f.groupID,
		},
		GrantTypeAgentDelegation: {
			Type:      GrantTypeAgentDelegation,
			AgentRole: string(AgentRoleNone),
			ProjectID: f.projectID,
		},
		GrantTypeCustomRole: {
			Type:                  GrantTypeCustomRole,
			CustomRolePermissions: []string{"agent.read"},
			ScopeType:             store.RoleScopeProject,
			ScopeID:               f.projectID,
		},
		GrantTypeProjectMembership: {
			Type:      GrantTypeProjectMembership,
			ProjectID: f.projectID,
		},
	}
}

// allGrantTypes lists every GrantType constant in authz_candelegate.go.
var allGrantTypes = []GrantType{
	GrantTypeRoleBinding,
	GrantTypeGroupMembership,
	GrantTypeAgentDelegation,
	GrantTypeCustomRole,
	GrantTypeProjectMembership,
}

type canDelegateOutcome struct {
	allowed bool
	reason  string
}

// canDelegateParityExpected holds CanDelegate's outcome for every non-delivery
// identity type and every descriptor in canDelegateParityFixture.grants().
// The values are literals recorded on the code before the hub_delivery deny
// arm was added to CanDelegate. They are not derived at test time: the test
// shows CanDelegate gives the same result for every other identity type
// with and without that arm.
var canDelegateParityExpected = map[string]map[GrantType]canDelegateOutcome{
	"AuthenticatedUser": {
		GrantTypeRoleBinding:       {true, "actor holds all required permissions"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent role has no permissions to delegate"},
		GrantTypeCustomRole:        {true, "actor holds all required permissions"},
		GrantTypeProjectMembership: {true, "project admin can manage membership (RS1 governance matrix)"},
	},
	"DevUser": {
		GrantTypeRoleBinding:       {true, "super-admin can delegate any authority"},
		GrantTypeGroupMembership:   {true, "super-admin can delegate any authority"},
		GrantTypeAgentDelegation:   {true, "super-admin can delegate any authority"},
		GrantTypeCustomRole:        {true, "super-admin can delegate any authority"},
		GrantTypeProjectMembership: {true, "super-admin can delegate any authority"},
	},
	"FederatedAgentIdentity": {
		GrantTypeRoleBinding:       {false, "failed to resolve actor permissions"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: failed to resolve actor permissions"},
		GrantTypeAgentDelegation:   {true, "agent holds all delegated scopes"},
		GrantTypeCustomRole:        {false, "failed to resolve actor permissions"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"FederatedServiceIdentity": {
		GrantTypeRoleBinding:       {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent role has no permissions to delegate"},
		GrantTypeCustomRole:        {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"FederatedUserIdentity": {
		GrantTypeRoleBinding:       {false, "failed to resolve actor permissions"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: failed to resolve actor permissions"},
		GrantTypeAgentDelegation:   {true, "agent role has no permissions to delegate"},
		GrantTypeCustomRole:        {false, "failed to resolve actor permissions"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"ScopedUserIdentity": {
		GrantTypeRoleBinding:       {true, "actor holds all required permissions"},
		GrantTypeGroupMembership:   {false, "scoped credential cannot delegate system-scoped group authority"},
		GrantTypeAgentDelegation:   {true, "agent role has no permissions to delegate"},
		GrantTypeCustomRole:        {true, "actor holds all required permissions"},
		GrantTypeProjectMembership: {true, "project admin can manage membership (RS1 governance matrix)"},
	},
	"agentIdentityWrapper": {
		GrantTypeRoleBinding:       {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent holds all delegated scopes"},
		GrantTypeCustomRole:        {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"brokerIdentityImpl": {
		GrantTypeRoleBinding:       {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent role has no permissions to delegate"},
		GrantTypeCustomRole:        {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"explainAgentIdentity": {
		GrantTypeRoleBinding:       {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent holds all delegated scopes"},
		GrantTypeCustomRole:        {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"peerAgentIdentity": {
		GrantTypeRoleBinding:       {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent holds all delegated scopes"},
		GrantTypeCustomRole:        {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
	"storedAgentIdentity": {
		GrantTypeRoleBinding:       {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeGroupMembership:   {false, "actor cannot delegate inherited system-scoped group authority: actor lacks permission for delegation: agent.read"},
		GrantTypeAgentDelegation:   {true, "agent holds all delegated scopes"},
		GrantTypeCustomRole:        {false, "actor lacks permission for delegation: agent.read"},
		GrantTypeProjectMembership: {false, "only project owners and admins can manage project membership"},
	},
}

// TestCanDelegate_ParityForOtherIdentityTypes pins CanDelegate's result for
// every identity type other than hubDeliveryIdentity against a fixed set of
// grant descriptors, one per GrantType. A new identity type in
// identityInventoryExpectation must have a fixture here.
func TestCanDelegate_ParityForOtherIdentityTypes(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()
	f := newCanDelegateParityFixture(t, s)
	identities := f.identities()
	grants := f.grants()

	t.Run("Completeness", func(t *testing.T) {
		var want []string
		for name := range identityInventoryExpectation {
			if name != "hubDeliveryIdentity" {
				want = append(want, name)
			}
		}
		var got []string
		for name, identity := range identities {
			got = append(got, name)
			assert.Equal(t, name, reflect.TypeOf(identity).Elem().Name(),
				"fixture %q must be a value of that concrete type", name)
		}
		var gotExpected []string
		for name := range canDelegateParityExpected {
			gotExpected = append(gotExpected, name)
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(gotExpected)
		require.Equal(t, want, got,
			"every identityInventoryExpectation key other than hubDeliveryIdentity needs a CanDelegate parity fixture")
		require.Equal(t, want, gotExpected,
			"every identityInventoryExpectation key other than hubDeliveryIdentity needs recorded CanDelegate parity outcomes")

		require.Len(t, grants, len(allGrantTypes), "one descriptor per GrantType")
		for _, gt := range allGrantTypes {
			require.Contains(t, grants, gt, "missing descriptor for GrantType %q", gt)
		}
		agentGrant := grants[GrantTypeAgentDelegation]
		assert.Equal(t, string(AgentRoleNone), agentGrant.AgentRole)
		assert.Nil(t, agentGrant.AgentScopes)
	})

	// An agent-typed actor on an allow path that checks scopes: the JWT
	// agent holds the full role's scopes and requests a sub-agent with the
	// full role, so every requested scope is compared and found. The
	// literal was recorded on the code before the hub_delivery deny arm was
	// added to CanDelegate, like canDelegateParityExpected.
	t.Run("agentIdentityWrapper/agent_delegation_full_role", func(t *testing.T) {
		grant := GrantDescriptor{
			Type:      GrantTypeAgentDelegation,
			AgentRole: string(AgentRoleFull),
			ProjectID: f.projectID,
		}
		require.NotEmpty(t, ScopesForRole(AgentRoleFull), "the row must compare a non-empty scope set")
		got := authz.CanDelegate(ctx, identities["agentIdentityWrapper"], grant)
		assert.True(t, got.Allowed, "Allowed")
		assert.Equal(t, "agent holds all delegated scopes", got.Reason, "Reason")
	})

	for _, name := range sortedIdentityNames(identities) {
		identity := identities[name]
		for _, gt := range allGrantTypes {
			t.Run(name+"/"+string(gt), func(t *testing.T) {
				want, ok := canDelegateParityExpected[name][gt]
				require.True(t, ok, "no recorded outcome for %s/%s", name, gt)
				got := authz.CanDelegate(ctx, identity, grants[gt])
				assert.Equal(t, want.allowed, got.Allowed, "Allowed")
				assert.Equal(t, want.reason, got.Reason, "Reason")
			})
		}
	}
}

func sortedIdentityNames(m map[string]Identity) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hubDeliveryDelegationScopes returns every agent JWT scope the hub knows:
// every scope referenced by the permissions registry, every scope of every
// stock AgentRole, and the declared scopes that neither references.
func hubDeliveryDelegationScopes() []AgentTokenScope {
	seen := map[AgentTokenScope]bool{}
	var out []AgentTokenScope
	add := func(scopes ...AgentTokenScope) {
		for _, s := range scopes {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	add(allRegisteredAgentScopes()...)
	for _, role := range []AgentRole{AgentRoleNone, AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		add(ScopesForRole(role)...)
	}
	add(ScopeAgentLogAppend, ScopeIdentityToken)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestCanDelegate_HubDeliveryActorDenied shows a hub_delivery actor cannot
// delegate any permission. It covers all five GrantTypes: role binding and
// custom role over every registry permission (and the empty permission
// set), group membership, project membership, and agent delegation with
// AgentRoleNone and no AgentScopes plus every AgentRole with every known
// scope. Every case is denied with "delivery credential cannot delegate",
// for a constructed actor and for a typed-nil *hubDeliveryIdentity actor.
func TestCanDelegate_HubDeliveryActorDenied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()
	f := newCanDelegateParityFixture(t, s)

	emptyGroupID := tid("candelegate-hd-empty-group")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: emptyGroupID, Slug: "candelegate-hd-empty-group", Name: "CanDelegate HD Empty Group",
	}))

	agentID := tid("candelegate-hd-agent")
	actor := &hubDeliveryIdentity{
		agentID:      agentID,
		projectID:    f.projectID,
		ancestry:     []string{f.userID},
		originUserID: f.userID,
		boundAgentID: agentID,
		evidence: &storedAgentIdentity{agent: &store.Agent{
			ID: agentID, ProjectID: f.projectID, Ancestry: []string{f.userID},
		}},
	}

	type deniedCase struct {
		name  string
		grant GrantDescriptor
	}
	var cases []deniedCase
	seenGrantTypes := map[GrantType]bool{}
	addCase := func(name string, grant GrantDescriptor) {
		seenGrantTypes[grant.Type] = true
		cases = append(cases, deniedCase{name: name, grant: grant})
	}

	addCase("role_binding/empty_permission_set", GrantDescriptor{
		Type: GrantTypeRoleBinding, ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
	})
	addCase("custom_role/empty_permission_set", GrantDescriptor{
		Type: GrantTypeCustomRole, ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
	})
	var allPermissionIDs []string
	for _, p := range permissions.Registry {
		allPermissionIDs = append(allPermissionIDs, p.ID)
		addCase("role_binding/"+p.ID, GrantDescriptor{
			Type: GrantTypeRoleBinding, RolePermissions: []string{p.ID},
			ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
		})
		addCase("custom_role/"+p.ID, GrantDescriptor{
			Type: GrantTypeCustomRole, CustomRolePermissions: []string{p.ID},
			ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
		})
	}
	require.NotEmpty(t, allPermissionIDs)
	addCase("role_binding/all_permissions_system", GrantDescriptor{
		Type: GrantTypeRoleBinding, RolePermissions: allPermissionIDs, ScopeType: store.RoleScopeSystem,
	})
	addCase("custom_role/all_permissions_system", GrantDescriptor{
		Type: GrantTypeCustomRole, CustomRolePermissions: allPermissionIDs, ScopeType: store.RoleScopeSystem,
	})

	addCase("group_membership/no_group", GrantDescriptor{Type: GrantTypeGroupMembership})
	addCase("group_membership/group_without_bindings", GrantDescriptor{Type: GrantTypeGroupMembership, GroupID: emptyGroupID})
	addCase("group_membership/group_with_system_binding", GrantDescriptor{Type: GrantTypeGroupMembership, GroupID: f.groupID})

	addCase("project_membership/own_project", GrantDescriptor{Type: GrantTypeProjectMembership, ProjectID: f.projectID})

	addCase("agent_delegation/none_role_nil_scopes", GrantDescriptor{
		Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleNone), ProjectID: f.projectID,
	})
	addCase("agent_delegation/empty_role_nil_scopes", GrantDescriptor{
		Type: GrantTypeAgentDelegation, ProjectID: f.projectID,
	})
	scopes := hubDeliveryDelegationScopes()
	require.NotEmpty(t, scopes)
	for _, role := range []AgentRole{AgentRoleNone, AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		addCase("agent_delegation/"+string(role)+"/role_only", GrantDescriptor{
			Type: GrantTypeAgentDelegation, AgentRole: string(role), ProjectID: f.projectID,
		})
		for _, scope := range scopes {
			addCase("agent_delegation/"+string(role)+"/"+string(scope), GrantDescriptor{
				Type: GrantTypeAgentDelegation, AgentRole: string(role),
				AgentScopes: []AgentTokenScope{scope}, ProjectID: f.projectID,
			})
		}
	}

	for _, gt := range allGrantTypes {
		require.True(t, seenGrantTypes[gt], "no denied case for GrantType %q", gt)
	}

	// A typed-nil *hubDeliveryIdentity is a non-nil Identity interface
	// value, so it passes the missing-actor check and is denied by the
	// concrete-type check that follows it.
	var typedNil *hubDeliveryIdentity
	actors := []struct {
		name  string
		actor Identity
	}{
		{"actor", actor},
		{"typed_nil_actor", typedNil},
	}

	for _, a := range actors {
		for _, tc := range cases {
			t.Run(a.name+"/"+tc.name, func(t *testing.T) {
				got := authz.CanDelegate(ctx, a.actor, tc.grant)
				assert.False(t, got.Allowed, "Allowed")
				assert.Equal(t, "delivery credential cannot delegate", got.Reason, "Reason")
			})
		}
	}
}
