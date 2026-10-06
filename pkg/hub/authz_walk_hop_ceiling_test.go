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

// walkHop runs the step-10 walk for agentID in f's project scope and returns
// the outcome and cause.
func (f ceilingFixture) walkHop(t *testing.T, authz *AuthzService, agentID, permissionID string, resource Resource, action Action) (bool, DenyCause) {
	t.Helper()
	var cause DenyCause
	allowed, _, err := authz.walkDelegationChainWithCause(context.Background(), resource, action, permissionID, agentID,
		true, store.RoleScopeProject, f.projectID, nil, &cause)
	require.NoError(t, err)
	return allowed, cause
}

// newAdminDelegatorFixture is a ceilingFixture whose user delegator is a
// system admin, so the delegator-authority check passes for every
// permission and the hop's frozen ceiling decides.
func newAdminDelegatorFixture(t *testing.T, name string) ceilingFixture {
	t.Helper()
	f := newCeilingFixture(t, name)
	f.userID = tid("ec-admin-" + name)
	createTestUserWithRole(t, f.store, f.userID, "ec-admin-"+name+"@test.com", "admin", store.SystemRoleSuperAdmin)
	return f
}

// A permission outside a bounded hop's ceiling is denied by the walk, at the
// agent's own hop and at an ancestor's hop, with DeniedBy delegation_ceiling
// and cause ceiling_effect_exceeded through Decide.
func TestWalkDeniesPermissionOutsideHopCeiling(t *testing.T) {
	readCoverage := agentScopeCoverage([]AgentTokenScope{ScopeProjectRead})

	t.Run("own hop through Decide", func(t *testing.T) {
		f := newCeilingFixture(t, "walk-own")
		a := f.agent(t, "walk-own", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, boundedCeiling(readCoverage...), provSession)
		authz := f.authz(f.store, false, false)
		resource := Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}
		d := decidePerm(authz, dcAgentIdentity(a.ID, f.projectID, AgentRoleFull), resource, ActionCreate, "agent.create", false)
		assert.False(t, d.Allowed)
		assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)

		read := decidePerm(authz, dcAgentIdentity(a.ID, f.projectID, AgentRoleFull),
			Resource{Type: "project", ID: f.projectID}, ActionRead, "project.read", false)
		assert.True(t, read.Allowed, "a permission inside the ceiling passes: %s", read.Reason)
	})

	t.Run("minimal UAT ceiling denies template.create", func(t *testing.T) {
		f := newCeilingFixture(t, "walk-tmpl")
		a := f.agent(t, "walk-tmpl", AgentRoleFull)
		minimal := uatCeilingFromSelectors(t, minimalSelectors(t)...).PermissionIDs
		require.NotContains(t, minimal, "template.create")
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, boundedCeiling(minimal...), provSession)
		d := decidePerm(f.authz(f.store, false, false), dcAgentIdentity(a.ID, f.projectID, AgentRoleFull),
			Resource{Type: "template", ParentType: "project", ParentID: f.projectID}, ActionCreate, "template.create", false)
		assert.False(t, d.Allowed)
		assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)
	})

	t.Run("control: permission inside the ceiling", func(t *testing.T) {
		f := newCeilingFixture(t, "walk-ctl")
		a := f.agent(t, "walk-ctl", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, boundedCeiling(append(readCoverage, "agent.create")...), provSession)
		d := decidePerm(f.authz(f.store, false, false), dcAgentIdentity(a.ID, f.projectID, AgentRoleFull),
			Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate, "agent.create", false)
		assert.True(t, d.Allowed, d.Reason)
	})

	t.Run("ancestor hop", func(t *testing.T) {
		f := newCeilingFixture(t, "walk-anc")
		p := f.agent(t, "walk-anc-p", AgentRoleFull)
		c := f.agent(t, "walk-anc-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, boundedCeiling(readCoverage...), provSession)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, ceilPrincip, provAgent)
		d := decidePerm(f.authz(f.store, false, false), dcAgentIdentity(c.ID, f.projectID, AgentRoleFull),
			Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate, "agent.create", false)
		assert.False(t, d.Allowed)
		assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause)
	})

	t.Run("agent hop", func(t *testing.T) {
		f := newCeilingFixture(t, "walk-ah")
		p := f.agent(t, "walk-ah-p", AgentRoleFull)
		c := f.agent(t, "walk-ah-c", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, p.ID, ceilPrincip, provSession)
		f.edge(t, store.DelegationPrincipalAgent, p.ID, c.ID, boundedCeiling(readCoverage...), provAgent)
		allowed, cause := f.walkHop(t, f.authz(f.store, false, false), c.ID, "agent.create",
			Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate)
		assert.False(t, allowed)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, cause)
	})
}

// A dev_local hop needs dev auth and the delegator user:DevUserID. A
// suspended dev user is denied by the existing non-live-delegator check.
func TestWalkDevLocalHopRequiresDevUserDelegator(t *testing.T) {
	ctx := context.Background()

	t.Run("delegator is not the dev user", func(t *testing.T) {
		f := newCeilingFixture(t, "wdl-other")
		a := f.agent(t, "wdl-other", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provDevLocal)
		allowed, cause := f.walkHop(t, f.authz(f.store, true, false), a.ID, "agent.create",
			Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate)
		assert.False(t, allowed)
		assert.Equal(t, DenyCauseCeilingSourceNotAllowed, cause)
	})

	t.Run("dev user active, then suspended", func(t *testing.T) {
		f := newCeilingFixture(t, "wdl-susp")
		createDCUser(t, f.store, DevUserID, "dev@test.com", f.projectID, store.ProjectRoleOwner)
		a := f.agent(t, "wdl-susp", AgentRoleFull)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		res := Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}

		allowed, _ := f.walkHop(t, f.authz(f.store, true, false), a.ID, "agent.create", res, ActionCreate)
		assert.True(t, allowed, "control: active dev user with dev auth enabled")

		allowed, cause := f.walkHop(t, f.authz(f.store, false, false), a.ID, "agent.create", res, ActionCreate)
		assert.False(t, allowed, "dev auth disabled")
		assert.Equal(t, DenyCauseCeilingSourceNotAllowed, cause)

		u, err := f.store.GetUser(ctx, DevUserID)
		require.NoError(t, err)
		u.Status = "suspended"
		require.NoError(t, f.store.UpdateUser(ctx, u))
		allowed, cause = f.walkHop(t, f.authz(f.store, true, false), a.ID, "agent.create", res, ActionCreate)
		assert.False(t, allowed, "suspended dev user")
		assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, cause)
	})
}

// The self-operation exception applies only when the resource is the
// acting agent.
func TestWalkSelfOpAllowedOnlyOnSelf(t *testing.T) {
	f := newAdminDelegatorFixture(t, "wself")
	a := f.agent(t, "wself-a", AgentRoleFull)
	b := f.agent(t, "wself-b", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, boundedCeiling(), provSession)
	authz := f.authz(f.store, false, false)

	allowed, cause := f.walkHop(t, authz, a.ID, "agent.status_update",
		Resource{Type: "agent", ID: a.ID, ParentType: "project", ParentID: f.projectID}, "status_update")
	assert.True(t, allowed, "self: cause %q", cause)

	allowed, cause = f.walkHop(t, authz, a.ID, "agent.status_update",
		Resource{Type: "agent", ID: b.ID, ParentType: "project", ParentID: f.projectID}, "status_update")
	assert.False(t, allowed, "another agent")
	assert.Equal(t, DenyCauseCeilingEffectExceeded, cause)

	allowed, cause = f.walkHop(t, authz, a.ID, "agent.delete",
		Resource{Type: "agent", ID: a.ID, ParentType: "project", ParentID: f.projectID}, ActionDelete)
	assert.False(t, allowed, "a permission outside the self-operation table on self")
	assert.Equal(t, DenyCauseCeilingEffectExceeded, cause)
}

// Every permission in recordedProvenanceRequired is denied on an unrecorded
// hop, and on a hop whose provenance version is not understood.
func TestWalkUnrecordedEdgeSensitivePermissionDenied(t *testing.T) {
	f := newAdminDelegatorFixture(t, "wunrec")
	a := f.agent(t, "wunrec-a", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	v2 := f.agent(t, "wunrec-v2", AgentRoleFull)
	prov := provSession
	prov.ProvenanceVersion = 2
	f.edge(t, store.DelegationPrincipalUser, f.userID, v2.ID, ceilPrincip, prov)
	authz := f.authz(f.store, false, false)

	require.Contains(t, recordedProvenanceRequiredIDs, "secret.deliver")
	require.Contains(t, recordedProvenanceRequiredIDs, "env_var.deliver")
	require.Contains(t, recordedProvenanceRequiredIDs, "skill_injection.deliver")
	for _, permID := range recordedProvenanceRequiredIDs {
		p, ok := registryPermission(permID)
		require.True(t, ok, permID)
		res := Resource{Type: p.Resource, ParentType: "project", ParentID: f.projectID}
		for _, agentID := range []string{a.ID, v2.ID} {
			allowed, cause := f.walkHop(t, authz, agentID, permID, res, Action(p.Action))
			assert.False(t, allowed, permID)
			assert.Equal(t, DenyCauseCeilingUnrecorded, cause, permID)
		}
	}
}

// Permissions outside recordedProvenanceRequired are decided by the role
// grant alone on an unrecorded hop (frozen legacy characterization): the walk
// decides exactly as for a principal hop with the same delegator, except that
// a permission in legacyChainExcludedPermissions the delegator holds is
// denied with ceiling_unrecorded.
func TestWalkUnrecordedEdgeLegacyCharacterization(t *testing.T) {
	f := newCeilingFixture(t, "wlegacy")
	unrec := f.agent(t, "wlegacy-u", AgentRoleFull)
	princ := f.agent(t, "wlegacy-p", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, unrec.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	f.edge(t, store.DelegationPrincipalUser, f.userID, princ.ID, ceilPrincip, provSession)
	authz := f.authz(f.store, false, false)

	withheld := 0
	for _, p := range permissions.Registry {
		if recordedProvenanceRequired[p.ID] {
			continue
		}
		res := Resource{Type: p.Resource, ParentType: "project", ParentID: f.projectID}
		wantAllowed, wantCause := f.walkHop(t, authz, princ.ID, p.ID, res, Action(p.Action))
		gotAllowed, gotCause := f.walkHop(t, authz, unrec.ID, p.ID, res, Action(p.Action))
		if legacyChainExcludedPermissions[p.ID] && wantAllowed {
			// Permissions unrecorded chains never held (the artifact
			// permissions) are withheld from them even where the delegator
			// holds them. Where the delegator lacks one, the delegator
			// check denies first, exactly as for the principal hop below.
			assert.False(t, gotAllowed, p.ID)
			assert.Equal(t, DenyCauseCeilingUnrecorded, gotCause, p.ID)
			withheld++
			continue
		}
		assert.Equal(t, wantAllowed, gotAllowed, p.ID)
		assert.Equal(t, wantCause, gotCause, p.ID)
	}
	assert.Positive(t, withheld, "guard: the delegator holds at least one withheld permission, so the unrecorded-hop denial is exercised")
	allowed, _ := f.walkHop(t, authz, unrec.ID, "agent.create",
		Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate)
	assert.True(t, allowed, "guard: the owner delegator holds agent.create")
}

// The delivery permissions walk the target agent's chain with no
// self-operation exception: a bounded hop passes exactly when it carries the
// delivery ID.
func TestWalkHubDeliveryUsesDeliverPermission(t *testing.T) {
	f := newAdminDelegatorFixture(t, "wdeliver")
	with := f.agent(t, "wdeliver-with", AgentRoleFull)
	without := f.agent(t, "wdeliver-without", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, with.ID, boundedCeiling(hubDeliveryPermissionList...), provSession)
	f.edge(t, store.DelegationPrincipalUser, f.userID, without.ID, boundedCeiling("project.read"), provSession)
	authz := f.authz(f.store, false, false)

	for _, permID := range hubDeliveryPermissionList {
		p, ok := registryPermission(permID)
		require.True(t, ok, permID)
		res := Resource{Type: p.Resource, ParentType: "project", ParentID: f.projectID}

		allowed, cause := f.walkHop(t, authz, with.ID, permID, res, Action(p.Action))
		assert.True(t, allowed, "%s carried by the hop: cause %q", permID, cause)

		allowed, cause = f.walkHop(t, authz, without.ID, permID, res, Action(p.Action))
		assert.False(t, allowed, permID)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, cause, permID)

		selfRes := Resource{Type: "agent", ID: without.ID, ParentType: "project", ParentID: f.projectID}
		allowed, cause = f.walkHop(t, authz, without.ID, permID, selfRes, Action(p.Action))
		assert.False(t, allowed, "%s on the agent itself: no self-operation exception", permID)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, cause, permID)
	}
}

// hopEffectCeilingDeny's order of checks.
func TestHopEffectCeilingDenyOrder(t *testing.T) {
	self := Resource{Type: "agent", ID: "a1"}
	other := Resource{Type: "agent", ID: "a2"}
	devEdge := &store.DelegationEdge{DelegatorType: store.DelegationPrincipalUser, DelegatorID: DevUserID,
		AuthorityProvenance: provDevLocal, EffectCeiling: ceilPrincip}

	c, _ := hopEffectCeilingDeny(devEdge, "agent.create", other, "a1", false)
	assert.Equal(t, DenyCauseCeilingSourceNotAllowed, c, "dev auth off")
	c, _ = hopEffectCeilingDeny(devEdge, "agent.create", other, "a1", true)
	assert.Empty(t, c, "dev auth on, dev delegator")
	otherDev := *devEdge
	otherDev.DelegatorID = "someone"
	c, _ = hopEffectCeilingDeny(&otherDev, "agent.create", other, "a1", true)
	assert.Equal(t, DenyCauseCeilingSourceNotAllowed, c)

	v0bounded := &store.DelegationEdge{EffectCeiling: boundedCeiling("project.secret_read")}
	c, _ = hopEffectCeilingDeny(v0bounded, "project.secret_read", other, "a1", false)
	assert.Equal(t, DenyCauseCeilingUnrecorded, c, "unknown provenance version before the ceiling")

	b := &store.DelegationEdge{AuthorityProvenance: provSession, EffectCeiling: boundedCeiling()}
	c, _ = hopEffectCeilingDeny(b, "agent.status_update", self, "a1", false)
	assert.Empty(t, c)
	c, _ = hopEffectCeilingDeny(b, "agent.status_update", other, "a1", false)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, c)
	c, _ = hopEffectCeilingDeny(b, "agent.status_update", Resource{Type: "agent"}, "", false)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, c, "empty IDs are never self")

	unknownKind := &store.DelegationEdge{AuthorityProvenance: provSession, EffectCeiling: store.EffectCeiling{Kind: "other"}}
	c, _ = hopEffectCeilingDeny(unknownKind, "project.read", other, "a1", false)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, c, "unknown kind fails closed")

	// A recorded provenance version with an empty (unrecorded) ceiling kind:
	// the kind switch alone denies the permissions that need recorded
	// provenance.
	v1unrec := &store.DelegationEdge{AuthorityProvenance: provSession, EffectCeiling: store.EffectCeiling{}}
	for _, p := range recordedProvenanceRequiredIDs {
		c, _ = hopEffectCeilingDeny(v1unrec, p, other, "a1", false)
		assert.Equal(t, DenyCauseCeilingUnrecorded, c, p)
	}
	c, _ = hopEffectCeilingDeny(v1unrec, "project.read", other, "a1", false)
	assert.Empty(t, c, "a permission outside recordedProvenanceRequired keeps the legacy outcome")
}
