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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// delegationFixture is a hub with two projects. owner owns projectP and has
// no role in projectQ.
type delegationFixture struct {
	authz    *AuthzService
	store    store.Store
	projectP string
	projectQ string
	owner    string
}

func newDelegationFixture(t *testing.T, name string) delegationFixture {
	t.Helper()
	authz, s := setupCanDelegateTest(t)
	f := delegationFixture{
		authz:    authz,
		store:    s,
		projectP: tid("deleg-" + name + "-p"),
		projectQ: tid("deleg-" + name + "-q"),
		owner:    tid("deleg-" + name + "-owner"),
	}
	createRS1Project(t, s, f.projectP, f.owner)
	createRS1Project(t, s, f.projectQ, tid("deleg-"+name+"-owner-q"))
	return f
}

func delegationHubToken(t *testing.T, userID string, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(userID), hubBoundary(), selectors, tid("deleg-hub-cred-"+userID), bearerCeiling(t, selectors...), nil)
}

func delegationProjectToken(t *testing.T, userID, projectID string, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(userID), projectBoundary(projectID), selectors, tid("deleg-proj-cred-"+userID), bearerCeiling(t, selectors...), nil)
}

func agentDelegationGrant(projectID string) GrantDescriptor {
	return GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleBaseline),
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	}
}

func projectRoleBindingGrant(projectID string, perms ...string) GrantDescriptor {
	return GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: perms,
		ScopeType:       store.RoleScopeProject,
		ScopeID:         projectID,
	}
}

// TestCanDelegate_ProjectBoundaryRequiresMatchingGrantProject pins that a
// project-boundary UAT delegates only into its own project, and that a
// project grant naming no project is denied for every UAT boundary kind.
func TestCanDelegate_ProjectBoundaryRequiresMatchingGrantProject(t *testing.T) {
	f := newDelegationFixture(t, "proj-match")
	ctx := context.Background()
	projectToken := delegationProjectToken(t, f.owner, f.projectP, "agent:create")
	hubToken := delegationHubToken(t, f.owner, "agent:create")

	d := f.authz.CanDelegate(ctx, projectToken, agentDelegationGrant(f.projectP))
	assert.True(t, d.Allowed, "a grant in the boundary project is allowed: %s", d.Reason)

	d = f.authz.CanDelegate(ctx, projectToken, agentDelegationGrant(f.projectQ))
	assert.False(t, d.Allowed, "a grant in another project is denied")
	assert.Contains(t, d.Reason, "outside its project")

	for name, actor := range map[string]*ScopedUserIdentity{"project boundary": projectToken, "hub boundary": hubToken} {
		for _, grant := range []GrantDescriptor{
			{Type: GrantTypeAgentDelegation, AgentRole: string(AgentRoleBaseline), ScopeType: store.RoleScopeProject},
			{Type: GrantTypeRoleBinding, RolePermissions: []string{"agent.read"}, ScopeType: store.RoleScopeProject},
			{Type: GrantTypeCustomRole, CustomRolePermissions: []string{"agent.read"}, ScopeType: store.RoleScopeProject},
		} {
			d := f.authz.CanDelegate(ctx, actor, grant)
			assert.False(t, d.Allowed, "%s: a %s project grant naming no project is denied", name, grant.Type)
			assert.Contains(t, d.Reason, "names no project", "%s: %s", name, grant.Type)
		}
	}
}

// TestCanDelegate_HubBoundaryProjectGrantRequiresCurrentAccess pins that a
// hub-boundary UAT delegates into a project only while its holder currently
// has access to that project: the boundary admits any project, and project
// access decides. A user with hub membership only, and a former member, are
// denied.
func TestCanDelegate_HubBoundaryProjectGrantRequiresCurrentAccess(t *testing.T) {
	f := newDelegationFixture(t, "hub-access")
	ctx := context.Background()
	hubToken := delegationHubToken(t, f.owner, "agent:create", "project:manage")

	d := f.authz.CanDelegate(ctx, hubToken, agentDelegationGrant(f.projectP))
	assert.True(t, d.Allowed, "an agent delegation in a project the holder owns is allowed: %s", d.Reason)
	d = f.authz.CanDelegate(ctx, hubToken, projectRoleBindingGrant(f.projectP, "agent.create"))
	assert.True(t, d.Allowed, "a role binding in a project the holder owns is allowed: %s", d.Reason)

	for name, grant := range map[string]GrantDescriptor{
		"agent delegation":   agentDelegationGrant(f.projectQ),
		"role binding":       projectRoleBindingGrant(f.projectQ, "agent.read"),
		"project membership": {Type: GrantTypeProjectMembership, ProjectID: f.projectQ, ScopeType: store.RoleScopeProject, ScopeID: f.projectQ},
	} {
		d := f.authz.CanDelegate(ctx, hubToken, grant)
		assert.False(t, d.Allowed, "%s in a project without access is denied", name)
		assert.Contains(t, d.Reason, "requires current access", name)
	}

	hubMemberOnly := tid("deleg-hub-access-hub-member")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubMemberOnly, Email: hubMemberOnly + "@test.com", DisplayName: "Hub Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubMemberOnly)
	d = f.authz.CanDelegate(ctx, delegationHubToken(t, hubMemberOnly, "agent:create", "project:manage"), agentDelegationGrant(f.projectP))
	assert.False(t, d.Allowed, "hub membership alone does not admit a project")
	assert.Contains(t, d.Reason, "requires current access")

	uatpDeleteProjectBinding(t, f.store, f.owner, f.projectP)
	d = f.authz.CanDelegate(ctx, hubToken, agentDelegationGrant(f.projectP))
	assert.False(t, d.Allowed, "a former member is denied on the next delegation")
	assert.Contains(t, d.Reason, "requires current access")
}

// TestCanDelegate_HubBoundarySuperAdminAdmittedBySystemGrant pins that a
// hub-boundary UAT held by a super-admin with no project membership is
// admitted to a project through its system grant of the exact permission
// the operation enforces: agent.create for an agent delegation and
// project.manage for a project role binding.
func TestCanDelegate_HubBoundarySuperAdminAdmittedBySystemGrant(t *testing.T) {
	f := newDelegationFixture(t, "super-admin")
	ctx := context.Background()
	admin := tid("deleg-super-admin")
	createTestUserWithRole(t, f.store, admin, admin+"@test.com", "member", store.SystemRoleSuperAdmin)
	hubToken := delegationHubToken(t, admin, "agent:create", "agent:read", "project:manage")

	d := f.authz.CanDelegate(ctx, hubToken, agentDelegationGrant(f.projectQ))
	assert.True(t, d.Allowed, "agent delegation is admitted through the system agent.create grant: %s", d.Reason)
	d = f.authz.CanDelegate(ctx, hubToken, projectRoleBindingGrant(f.projectQ, "agent.read"))
	assert.True(t, d.Allowed, "a project role binding is admitted through the system project.manage grant: %s", d.Reason)
}

// TestCanDelegate_HubBoundarySystemAuthorityAdmitsOnlyItsPermission pins
// that system authority admits a hub-boundary UAT to a project only for the
// exact permission the delegating operation enforces: a system grant of
// agent.create admits an agent delegation, and does not admit a project
// role binding, which requires project.manage.
func TestCanDelegate_HubBoundarySystemAuthorityAdmitsOnlyItsPermission(t *testing.T) {
	f := newDelegationFixture(t, "sys-exact")
	ctx := context.Background()
	user := tid("deleg-sys-exact-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: user, Email: user + "@test.com", DisplayName: "Sys", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, user)
	grantPermissionViaRoleBinding(t, f.store, user, "agent.create", store.RoleScopeSystem, "")
	hubToken := delegationHubToken(t, user, "agent:create", "project:manage")

	d := f.authz.CanDelegate(ctx, hubToken, agentDelegationGrant(f.projectQ))
	assert.True(t, d.Allowed, "a system agent.create grant admits an agent delegation: %s", d.Reason)

	d = f.authz.CanDelegate(ctx, hubToken, projectRoleBindingGrant(f.projectQ, "agent.create"))
	assert.False(t, d.Allowed, "a system agent.create grant does not admit a project role binding")
	assert.Contains(t, d.Reason, "requires current access")
}

// TestCanDelegate_HubBoundarySystemGrantDenied pins that a hub-boundary UAT
// never creates a system-scoped grant, even for a super-admin holder.
func TestCanDelegate_HubBoundarySystemGrantDenied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()
	admin := tid("deleg-hub-sys-admin")
	createTestUserWithRole(t, s, admin, admin+"@test.com", "member", store.SystemRoleSuperAdmin)
	hubToken := delegationHubToken(t, admin, "agent:read")

	for _, grant := range []GrantDescriptor{
		{Type: GrantTypeRoleBinding, RolePermissions: []string{"agent.read"}, ScopeType: store.RoleScopeSystem},
		{Type: GrantTypeCustomRole, CustomRolePermissions: []string{"agent.read"}, ScopeType: store.RoleScopeSystem},
	} {
		d := authz.CanDelegate(ctx, hubToken, grant)
		assert.False(t, d.Allowed, "a %s system grant is denied", grant.Type)
		assert.Contains(t, d.Reason, "system-scoped grants")
	}
}

// TestCanDelegate_HubBoundaryGroupWithSystemBindingsDenied pins that a
// hub-boundary UAT cannot add a member to a group that carries a
// system-scoped role binding.
func TestCanDelegate_HubBoundaryGroupWithSystemBindingsDenied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()
	admin := tid("deleg-hub-group-admin")
	createTestUserWithRole(t, s, admin, admin+"@test.com", "member", store.SystemRoleSuperAdmin)

	groupID := tid("deleg-hub-group-sys")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupID, Slug: "deleg-hub-group-sys", Name: "System Group"}))
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	d := authz.CanDelegate(ctx, delegationHubToken(t, admin, "agent:read"), GrantDescriptor{Type: GrantTypeGroupMembership, GroupID: groupID})
	assert.False(t, d.Allowed, "system-scoped group authority is not delegable by a hub-boundary UAT")
	assert.Contains(t, d.Reason, "system-scoped group authority")
}

// TestCanDelegate_UnknownGrantScopeDenied pins that a grant whose scope type
// is neither empty, project nor system is denied for every UAT boundary
// kind.
func TestCanDelegate_UnknownGrantScopeDenied(t *testing.T) {
	f := newDelegationFixture(t, "unknown-scope")
	ctx := context.Background()
	for name, actor := range map[string]*ScopedUserIdentity{
		"project boundary": delegationProjectToken(t, f.owner, f.projectP, "agent:create"),
		"hub boundary":     delegationHubToken(t, f.owner, "agent:create"),
	} {
		for _, scopeType := range []string{"org", "hub", "PROJECT"} {
			d := f.authz.CanDelegate(ctx, actor, GrantDescriptor{
				Type:            GrantTypeRoleBinding,
				RolePermissions: []string{"agent.read"},
				ScopeType:       scopeType,
				ScopeID:         f.projectP,
			})
			assert.False(t, d.Allowed, "%s: scope type %q is denied", name, scopeType)
			assert.Contains(t, d.Reason, "unknown scope type", "%s: %q", name, scopeType)
		}
	}
}

// TestCanDelegate_HubBoundaryAttachOnlyCeilingCannotDelegateLifecycle pins
// that a hub-boundary UAT whose ceiling holds only agent.attach cannot
// delegate agent.lifecycle in a project its holder owns, matching the
// decision for the same token on the lifecycle operation itself.
func TestCanDelegate_HubBoundaryAttachOnlyCeilingCannotDelegateLifecycle(t *testing.T) {
	f := newDelegationFixture(t, "attach-only")
	ctx := context.Background()
	ceiling := permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionUnspecified,
		PermissionIDs: permissions.NormalizeLegacyUATScopes([]string{"agent:attach"}),
	}
	require.True(t, ceiling.Allows("agent.attach"))
	require.False(t, ceiling.Allows("agent.lifecycle"))
	hubToken := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(f.owner), hubBoundary(), []string{"agent:attach"}, tid("deleg-attach-only-cred"), ceiling, nil)

	full := f.authz.CanDelegate(ctx, bearerUser(f.owner), projectRoleBindingGrant(f.projectP, "agent.lifecycle"))
	require.True(t, full.Allowed, "the holder's session can delegate agent.lifecycle: %s", full.Reason)

	d := f.authz.CanDelegate(ctx, hubToken, projectRoleBindingGrant(f.projectP, "agent.lifecycle"))
	assert.False(t, d.Allowed, "an attach-only ceiling cannot delegate agent.lifecycle")
	assert.Contains(t, d.Reason, "actor lacks permission for delegation: agent.lifecycle")

	agent := uatpAgent(t, f.store, f.projectP, f.owner, "deleg-attach-only", f.owner)
	access := f.authz.CheckAccess(ctx, hubToken, agentResource(agent), ActionLifecycle)
	assert.False(t, access.Allowed, "the same token is denied the lifecycle operation")
}
