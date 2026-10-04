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

// Tests for the delegation ceiling (ptone/scion#2120): an agent action
// requires the exact permission to be held by every live delegator in the
// agent's typed delegation chain.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type parentCeilingFixture struct {
	authz     *AuthzService
	store     store.Store
	projectID string
	userID    string
}

func newParentCeilingFixture(t *testing.T, name string) parentCeilingFixture {
	t.Helper()
	authz, s := setupDelegationCeilingTest(t)
	f := parentCeilingFixture{
		authz:     authz,
		store:     s,
		projectID: tid("pc-proj-" + name),
		userID:    tid("pc-user-" + name),
	}
	createDCProject(t, s, f.projectID, "pc-"+name)
	createDCUser(t, s, f.userID, name+"@pc.test", f.projectID, store.ProjectRoleOwner)
	return f
}

// chain stores agents ids[0..n-1] in the fixture project: the user
// delegates to ids[0] and each agent delegates to the next.
func (f parentCeilingFixture) chain(t *testing.T, ids ...string) {
	t.Helper()
	prev, prevType := f.userID, store.DelegationPrincipalUser
	for _, id := range ids {
		createDCAgent(t, f.store, id, f.projectID, prev, AgentRoleFull)
		seedRecordedDelegationEdge(t, f.store, prevType, prev, store.DelegationPrincipalAgent, id,
			store.RoleScopeProject, f.projectID, string(AgentRoleFull))
		prev, prevType = id, store.DelegationPrincipalAgent
	}
}

func (f parentCeilingFixture) agent(id string) AgentIdentity {
	return dcAgentIdentity(id, f.projectID, AgentRoleFull)
}

func (f parentCeilingFixture) projectRead(t *testing.T, id string) Decision {
	t.Helper()
	return decidePerm(f.authz, f.agent(id), Resource{Type: "project", ID: f.projectID}, ActionRead, "project.read", false)
}

func (f parentCeilingFixture) agentCreate(t *testing.T, id string) Decision {
	t.Helper()
	return decidePerm(f.authz, f.agent(id), Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate, "agent.create", false)
}

// userRoleHolds reports whether userID holds perm through its roles in the
// project (a project target, so no named relationship applies).
func userRoleHolds(t *testing.T, a *AuthzService, userID, perm, projectID string) bool {
	t.Helper()
	ok, _, err := a.evaluateUserDelegatorAuthority(context.Background(), userID,
		Resource{Type: "project", ID: projectID}, ActionRead, perm, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	return ok
}

func softDeleteStoredAgent(t *testing.T, s store.Store, id string) {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

func assertCeilingDeny(t *testing.T, d Decision, msg string) {
	t.Helper()
	assert.False(t, d.Allowed, "%s: reason %q", msg, d.Reason)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "%s: reason %q", msg, d.Reason)
}

// assertCeilingDenyCause is assertCeilingDeny plus the DenyCause the
// ceiling recorded, which selects the SA-assign 403 text.
func assertCeilingDenyCause(t *testing.T, d Decision, cause DenyCause, msg string) {
	t.Helper()
	assertCeilingDeny(t, d, msg)
	assert.Equal(t, cause, d.DenyCause, "%s: reason %q", msg, d.Reason)
}

func TestParentCeiling_SoftDeletedImmediateAgentDelegatorDenies(t *testing.T) {
	f := newParentCeilingFixture(t, "softdel")
	parent, child := tid("pc-softdel-parent"), tid("pc-softdel-child")
	f.chain(t, parent, child)

	require.True(t, f.projectRead(t, child).Allowed, "live chain allows")
	require.True(t, f.agentCreate(t, child).Allowed, "live chain allows")

	softDeleteStoredAgent(t, f.store, parent)
	assertCeilingDenyCause(t, f.projectRead(t, child), DenyCauseCeilingOrphaned, "read with a soft-deleted parent")
	assertCeilingDenyCause(t, f.agentCreate(t, child), DenyCauseCeilingOrphaned, "create with a soft-deleted parent")
}

func TestParentCeiling_MissingAgentDelegatorDenies(t *testing.T) {
	f := newParentCeilingFixture(t, "purged")
	child := tid("pc-purged-child")
	createDCAgent(t, f.store, child, f.projectID, f.userID, AgentRoleFull)
	createDCEdge(t, f.store, store.DelegationPrincipalAgent, tid("pc-purged-parent-absent"),
		store.DelegationPrincipalAgent, child, store.RoleScopeProject, f.projectID, string(AgentRoleFull))

	assertCeilingDenyCause(t, f.projectRead(t, child), DenyCauseCeilingOrphaned, "read with a purged parent")
	assertCeilingDenyCause(t, f.agentCreate(t, child), DenyCauseCeilingOrphaned, "create with a purged parent")
}

func TestParentCeiling_MissingUserDelegatorDenies(t *testing.T) {
	f := newParentCeilingFixture(t, "nouser")
	child := tid("pc-nouser-child")
	createDCAgent(t, f.store, child, f.projectID, f.userID, AgentRoleFull)
	createDCEdge(t, f.store, store.DelegationPrincipalUser, tid("pc-nouser-absent"),
		store.DelegationPrincipalAgent, child, store.RoleScopeProject, f.projectID, string(AgentRoleFull))

	assertCeilingDenyCause(t, f.projectRead(t, child), DenyCauseCeilingOrphaned, "read with a missing user delegator")
}

func TestParentCeiling_DeeperDeletedDelegatorDenies(t *testing.T) {
	f := newParentCeilingFixture(t, "deep")
	a, b, c := tid("pc-deep-a"), tid("pc-deep-b"), tid("pc-deep-c")
	f.chain(t, a, b, c)
	require.True(t, f.projectRead(t, c).Allowed)

	softDeleteStoredAgent(t, f.store, a)
	assertCeilingDenyCause(t, f.projectRead(t, c), DenyCauseCeilingOrphaned, "grandchild with a deleted grandparent and a live parent")
	assertCeilingDenyCause(t, f.projectRead(t, b), DenyCauseCeilingOrphaned, "child with a deleted parent")
	assertCeilingDenyCause(t, f.agentCreate(t, c), DenyCauseCeilingOrphaned, "grandchild create with a deleted grandparent")
}

func TestParentCeiling_StoppedDelegatorAllows(t *testing.T) {
	f := newParentCeilingFixture(t, "stopped")
	parent, child := tid("pc-stopped-parent"), tid("pc-stopped-child")
	f.chain(t, parent, child)

	p, err := f.store.GetAgent(context.Background(), parent)
	require.NoError(t, err)
	p.Phase = "stopped"
	require.NoError(t, f.store.UpdateAgent(context.Background(), p))

	d := f.projectRead(t, child)
	assert.True(t, d.Allowed, "a stopped parent is live: reason %q", d.Reason)
	assert.Empty(t, d.DeniedBy)
	assert.True(t, f.agentCreate(t, child).Allowed)
}

func TestParentCeiling_MigrationSentinel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		delegatorType string
		delegatorID   string
		resource      func(projectID string) Resource
		action        Action
		perm          string
		allowed       bool
		// ceiling marks rows the delegation ceiling denies. Other denied
		// rows are denied by an earlier stage (kernel or scope). Every
		// ceiling row records DenyCauseCeilingOrphaned: the exact sentinel
		// denies as an unresolvable delegator, and a near-match ID or type
		// names a principal that does not exist.
		ceiling bool
	}{
		{"registered non-sensitive read", store.DelegationPrincipalUser, "system/migration",
			func(p string) Resource { return Resource{Type: "project", ID: p} }, ActionRead, "project.read", true, false},
		{"sensitive read", store.DelegationPrincipalUser, "system/migration",
			func(p string) Resource { return Resource{Type: "project", ID: p} }, ActionRead, permissionProjectSecretRead, false, true},
		{"attach", store.DelegationPrincipalUser, "system/migration",
			func(p string) Resource {
				return Resource{Type: "agent", ID: tid("pc-mig-target"), ParentType: "project", ParentID: p}
			}, ActionAttach, "agent.attach", false, true},
		{"create", store.DelegationPrincipalUser, "system/migration",
			func(p string) Resource { return Resource{Type: "agent", ParentType: "project", ParentID: p} }, ActionCreate, "agent.create", false, true},
		{"unmapped permission", store.DelegationPrincipalUser, "system/migration",
			func(p string) Resource { return Resource{Type: "project", ID: p} }, ActionRead, "project.unregistered_read", false, false},
		{"near-match id case", store.DelegationPrincipalUser, "System/Migration",
			func(p string) Resource { return Resource{Type: "project", ID: p} }, ActionRead, "project.read", false, true},
		{"near-match id suffix", store.DelegationPrincipalUser, "system/migration/",
			func(p string) Resource { return Resource{Type: "project", ID: p} }, ActionRead, "project.read", false, true},
		{"near-match delegator type", store.DelegationPrincipalAgent, "system/migration",
			func(p string) Resource { return Resource{Type: "project", ID: p} }, ActionRead, "project.read", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParentCeilingFixture(t, "mig")
			child := tid("pc-mig-child")
			createDCAgent(t, f.store, child, f.projectID, f.userID, AgentRoleFull)
			require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
				DelegatorType: tc.delegatorType, DelegatorID: tc.delegatorID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: child,
				ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
				Role: string(AgentRoleFull), Active: true, Grandfathered: true,
			}))
			d := decidePerm(f.authz, f.agent(child), tc.resource(f.projectID), tc.action, tc.perm, false)
			assert.Equal(t, tc.allowed, d.Allowed, "reason %q", d.Reason)
			if tc.ceiling {
				assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
				assert.Equal(t, DenyCauseCeilingOrphaned, d.DenyCause, "reason %q", d.Reason)
			}
		})
	}
}

// A user delegator supplies authority only while it exists and is active,
// including a super-admin. A suspended user exists but holds no permission
// (ceiling_delegator_lacks_permission); a deleted user does not resolve
// (ceiling_orphaned).
func TestParentCeiling_UserDelegatorMustBeLive(t *testing.T) {
	suspend := func(t *testing.T, s store.Store, id string) { setUserStatus(t, s, id, store.UserStatusSuspended) }
	del := func(t *testing.T, s store.Store, id string) {
		require.NoError(t, s.DeleteUser(context.Background(), id))
	}
	for _, tc := range []struct {
		name       string
		superAdmin bool
		mutate     func(t *testing.T, s store.Store, userID string)
		allowed    bool
		cause      DenyCause
	}{
		{"active super-admin", true, func(*testing.T, store.Store, string) {}, true, ""},
		{"suspended super-admin", true, suspend, false, DenyCauseCeilingDelegatorLacksPermission},
		{"deleted super-admin", true, del, false, DenyCauseCeilingOrphaned},
		{"active project owner", false, func(*testing.T, store.Store, string) {}, true, ""},
		{"suspended project owner", false, suspend, false, DenyCauseCeilingDelegatorLacksPermission},
		{"deleted project owner", false, del, false, DenyCauseCeilingOrphaned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParentCeilingFixture(t, "admindel")
			adminID := f.userID
			if tc.superAdmin {
				adminID = tid("pc-admindel-admin")
				createTestUserWithRole(t, f.store, adminID, "admindel-admin@pc.test", "admin", store.SystemRoleSuperAdmin)
			}
			child := tid("pc-admindel-child")
			createDCAgent(t, f.store, child, f.projectID, adminID, AgentRoleFull)
			createDCEdge(t, f.store, store.DelegationPrincipalUser, adminID, store.DelegationPrincipalAgent, child,
				store.RoleScopeProject, f.projectID, string(AgentRoleFull))
			tc.mutate(t, f.store, adminID)

			for _, d := range []Decision{f.projectRead(t, child), f.agentCreate(t, child)} {
				assert.Equal(t, tc.allowed, d.Allowed, "reason %q", d.Reason)
				if !tc.allowed {
					assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
				}
				assert.Equal(t, tc.cause, d.DenyCause, "reason %q", d.Reason)
			}
		})
	}
}

func TestParentCeiling_CycleDenies(t *testing.T) {
	f := newParentCeilingFixture(t, "cycle")
	a, b := tid("pc-cycle-a"), tid("pc-cycle-b")
	createDCAgent(t, f.store, a, f.projectID, b, AgentRoleFull)
	createDCAgent(t, f.store, b, f.projectID, a, AgentRoleFull)
	createDCEdge(t, f.store, store.DelegationPrincipalAgent, b, store.DelegationPrincipalAgent, a,
		store.RoleScopeProject, f.projectID, string(AgentRoleFull))
	createDCEdge(t, f.store, store.DelegationPrincipalAgent, a, store.DelegationPrincipalAgent, b,
		store.RoleScopeProject, f.projectID, string(AgentRoleFull))

	d := f.projectRead(t, a)
	assertCeilingDeny(t, d, "cyclic chain")
	assert.Contains(t, d.Reason, "cycle")
}

// The walk examines at most maxDelegationDepth+1 delegates: a chain of that
// many agents below the user is evaluated, one more is denied.
func TestParentCeiling_DepthLimit(t *testing.T) {
	for _, tc := range []struct {
		agents  int
		allowed bool
	}{
		{maxDelegationDepth + 1, true},
		{maxDelegationDepth + 2, false},
	} {
		t.Run(fmt.Sprintf("%d agents", tc.agents), func(t *testing.T) {
			f := newParentCeilingFixture(t, "depth")
			ids := make([]string, tc.agents)
			for i := range ids {
				ids[i] = tid(fmt.Sprintf("pc-depth-%d", i))
			}
			f.chain(t, ids...)
			d := f.projectRead(t, ids[len(ids)-1])
			assert.Equal(t, tc.allowed, d.Allowed, "reason %q", d.Reason)
			if !tc.allowed {
				assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
				assert.Contains(t, d.Reason, "maximum depth")
			}
		})
	}
}

// The ceiling evaluates the exact permission of the request: a delegator
// holding project.read does not supply project.secret_read.
func TestParentCeiling_ExactPermission(t *testing.T) {
	f := newParentCeilingFixture(t, "exact")
	viewerID := tid("pc-exact-viewer")
	createDCUser(t, f.store, viewerID, "exact-viewer@pc.test", f.projectID, store.ProjectRoleMember)
	child := tid("pc-exact-child")
	createDCAgent(t, f.store, child, f.projectID, viewerID, AgentRoleFull)
	createDCEdge(t, f.store, store.DelegationPrincipalUser, viewerID, store.DelegationPrincipalAgent, child,
		store.RoleScopeProject, f.projectID, string(AgentRoleFull))

	project := Resource{Type: "project", ID: f.projectID}
	require.True(t, decidePerm(f.authz, f.agent(child), project, ActionRead, "project.read", false).Allowed)
	d := decidePerm(f.authz, f.agent(child), project, ActionRead, permissionProjectSecretRead, false)
	require.False(t, userRoleHolds(t, f.authz, viewerID, permissionProjectSecretRead, f.projectID),
		"the member role does not hold project.secret_read")
	assertCeilingDeny(t, d, "delegator lacks the exact permission")
}

// DeniedBy names the ceiling only for a ceiling denial.
func TestParentCeiling_DeniedByOnlyForCeilingDenials(t *testing.T) {
	f := newParentCeilingFixture(t, "deniedby")
	child := tid("pc-deniedby-child")
	f.chain(t, child)

	allow := f.projectRead(t, child)
	require.True(t, allow.Allowed)
	assert.Empty(t, allow.DeniedBy, "allow")

	outsider := NewAuthenticatedUser(tid("pc-deniedby-outsider"), "outsider@pc.test", "o", "member", "api")
	kernelDeny := f.authz.CheckAccess(context.Background(), outsider, Resource{Type: "project", ID: f.projectID}, ActionDelete)
	require.False(t, kernelDeny.Allowed)
	assert.Empty(t, kernelDeny.DeniedBy, "a role denial is not a ceiling denial")

	baseline := dcAgentIdentity(child, f.projectID, AgentRoleBaseline)
	scopeDeny := decidePerm(f.authz, baseline, Resource{Type: "agent", ID: tid("pc-deniedby-t"), ParentType: "project", ParentID: f.projectID}, ActionAttach, "agent.attach", false)
	require.False(t, scopeDeny.Allowed)
	assert.Empty(t, scopeDeny.DeniedBy, "an agent scope denial is not a ceiling denial")
}

// A user delegator's authority on the target includes its named
// relationships: a project owner, whose role does not hold agent.attach,
// supplies it for agents it is an ancestor of, and not for a sibling.
func TestParentCeiling_DelegatorRelationshipAuthority(t *testing.T) {
	f := newParentCeilingFixture(t, "rel")
	parent := tid("pc-rel-parent")
	f.chain(t, parent)

	require.False(t, userRoleHolds(t, f.authz, f.userID, "agent.attach", f.projectID),
		"the owner role does not hold agent.attach")

	descendant := agentResource(&store.Agent{ID: tid("pc-rel-desc"), ProjectID: f.projectID, Ancestry: []string{f.userID, parent}})
	sibling := agentResource(&store.Agent{ID: tid("pc-rel-sib"), ProjectID: f.projectID, Ancestry: []string{tid("pc-rel-other"), parent}})

	d := decidePerm(f.authz, f.agent(parent), descendant, ActionAttach, "agent.attach", false)
	assert.True(t, d.Allowed, "delegator is an ancestor of the target: reason %q", d.Reason)

	d = decidePerm(f.authz, f.agent(parent), sibling, ActionAttach, "agent.attach", false)
	assertCeilingDeny(t, d, "delegator has no relationship to the target")
}
