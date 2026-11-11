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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossMemberAttach_Matrix verifies that project owners/admins cannot
// attach to agents owned by other project members, since those agents run
// with their owner's user-scoped secrets, while they can open those agents'
// forwarded ports through the agent.port_access their role carries. Owners
// keep full access to their own agents and progeny through relationship
// grants; plain members reach neither on another member's agent.
func TestCrossMemberAttach_Matrix(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	owner := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Project Owner", "member", "api")
	admin := NewAuthenticatedUser(f.projectAdminID, "proj-admin@golden.test", "Project Admin", "member", "api")
	member := NewAuthenticatedUser(f.memberAlphaID, "member-alpha@golden.test", "Member Alpha", "member", "api")
	superAdmin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")

	agentRes := func(id, ownerID string, ancestry ...string) Resource {
		return Resource{
			Type: "agent", ID: id, OwnerID: ownerID,
			ParentType: "project", ParentID: f.projectAlpha.ID,
			Ancestry: ancestry,
		}
	}
	ownerAgent := agentRes(f.agentAlpha.ID, f.projectOwnerID, f.projectOwnerID)
	// Progeny: spawned by the owner's agent, owned by that agent, with the
	// owner in its creation chain.
	ownerProgeny := agentRes("owner-progeny", f.agentAlpha.ID, f.projectOwnerID, f.agentAlpha.ID)
	memberAgent := agentRes("member-agent", f.memberAlphaID, f.memberAlphaID)
	adminAgent := agentRes("admin-agent", f.projectAdminID, f.projectAdminID)

	cases := []struct {
		name     string
		identity UserIdentity
		resource Resource
		attach   bool
		port     bool // owner/admin carry agent.port_access
	}{
		{"owner on own agent", owner, ownerAgent, true, true},
		{"owner on own progeny", owner, ownerProgeny, true, true},
		{"owner on member's agent", owner, memberAgent, false, true},
		{"admin on own agent", admin, adminAgent, true, true},
		{"admin on member's agent", admin, memberAgent, false, true},
		{"admin on owner's agent", admin, ownerAgent, false, true},
		{"member on own agent", member, memberAgent, true, true},
		{"member on other's agent", member, ownerAgent, false, false},
		{"super-admin on member's agent", superAdmin, memberAgent, true, true},
	}
	for _, tc := range cases {
		for action, want := range map[Action]bool{ActionAttach: tc.attach, ActionPortAccess: tc.port} {
			t.Run(tc.name+"/"+string(action), func(t *testing.T) {
				d := f.authz.CheckAccess(ctx, tc.identity, tc.resource, action)
				assert.Equal(t, want, d.Allowed, "reason: %s", d.Reason)
			})
		}
	}

	t.Run("owner and admin keep lifecycle on member's agent", func(t *testing.T) {
		for _, ident := range []UserIdentity{owner, admin} {
			d := f.authz.CheckAccess(ctx, ident, memberAgent, ActionLifecycle)
			assert.True(t, d.Allowed, "%s should have lifecycle on member's agent: %s", ident.ID(), d.Reason)
		}
		d := f.authz.CheckAccess(ctx, member, ownerAgent, ActionLifecycle)
		assert.False(t, d.Allowed, "member must not have lifecycle on another's agent")
		d = f.authz.CheckAccess(ctx, member, memberAgent, ActionLifecycle)
		assert.True(t, d.Allowed, "member keeps lifecycle on own agent: %s", d.Reason)
	})

	t.Run("owner non-attach actions on member's agent still allowed", func(t *testing.T) {
		for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionMessage, ActionLifecycle} {
			d := f.authz.CheckAccess(ctx, owner, memberAgent, action)
			assert.True(t, d.Allowed, "owner should have %s on member's agent: %s", action, d.Reason)
		}
	})

	t.Run("capabilities omit attach for member's agent only", func(t *testing.T) {
		caps := f.authz.ComputeCapabilities(ctx, owner, memberAgent)
		require.NotNil(t, caps)
		assert.NotContains(t, caps.Actions, string(ActionAttach))
		assert.Contains(t, caps.Actions, string(ActionPortAccess))
		assert.Contains(t, caps.Actions, string(ActionRead))
		assert.Contains(t, caps.Actions, string(ActionDelete))
		assert.Contains(t, caps.Actions, string(ActionLifecycle))

		caps = f.authz.ComputeCapabilities(ctx, owner, ownerAgent)
		assert.Contains(t, caps.Actions, string(ActionAttach))
		assert.Contains(t, caps.Actions, string(ActionPortAccess))

		batch := f.authz.ComputeCapabilitiesBatch(ctx, admin, []Resource{memberAgent, adminAgent}, "agent")
		require.Len(t, batch, 2)
		assert.NotContains(t, batch[0].Actions, string(ActionAttach))
		assert.Contains(t, batch[0].Actions, string(ActionPortAccess))
		assert.Contains(t, batch[1].Actions, string(ActionAttach))
		assert.Contains(t, batch[1].Actions, string(ActionPortAccess))
	})
}

// TestCrossMemberAttach_HTTPRoutes verifies the route-level split: a project
// owner may run lifecycle actions on a member's agent but is refused the
// observation actions (exec, env) that expose the member's secrets.
func TestCrossMemberAttach_HTTPRoutes(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-xattach"), "Bob", store.GroupMemberRoleOwner)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, project.ID, store.ProjectRoleOwner)

	agent := &store.Agent{
		ID: tid("alice-agent-xattach"), Slug: "alice-agent-xattach", Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	for _, action := range []string{"exec", "env"} {
		rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, map[string]any{})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"project owner must be forbidden from %s on a member's agent: %s", action, rec.Body.String())
	}
	for _, action := range []string{"stop", "restart"} {
		rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
		assert.Equal(t, http.StatusOK, rec.Code,
			"project owner must pass lifecycle authorization for %s on a member's agent: %s", action, rec.Body.String())
	}

	// Control: the agent's creator passes attach authorization for exec, so
	// the owner's 403 above comes from the attach gate, not the handler.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents/"+agent.ID+"/exec", map[string]any{})
	assert.NotEqual(t, http.StatusForbidden, rec.Code, "creator must pass attach authorization: %s", rec.Body.String())
}

// TestCrossMemberAttach_UATScopes verifies the UAT side of the split:
// agent:manage does not require agent.attach (so project owners can mint
// it), and a token that carries only agent:attach — however it was minted —
// does not gain agent.lifecycle: no scope implies another.
func TestCrossMemberAttach_UATScopes(t *testing.T) {
	manage := permissions.UATManageScopes()
	assert.Contains(t, manage, "agent:lifecycle")
	assert.NotContains(t, manage, "agent:attach")
	assert.NotContains(t, manage, "agent:port_access")
	assert.True(t, permissions.UATValidScopes()["agent:attach"], "agent:attach stays explicitly mintable")

	attachOnly := ceilingRestriction(permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionUnspecified,
		PermissionIDs: permissions.NormalizeLegacyUATScopes([]string{"agent:attach"}),
	})
	assert.False(t, attachOnly.Check("agent.lifecycle"), "attach-only tokens must not gain lifecycle")
	assert.True(t, attachOnly.Check("agent.attach"))

	readOnly := ceilingRestriction(permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionUnspecified,
		PermissionIDs: permissions.NormalizeLegacyUATScopes([]string{"agent:read"}),
	})
	assert.False(t, readOnly.Check("agent.lifecycle"))

	lifecycleOnly := ceilingRestriction(permissions.FrozenPermissionCeiling{
		Version:       permissions.CeilingVersionUnspecified,
		PermissionIDs: permissions.NormalizeLegacyUATScopes([]string{"agent:lifecycle"}),
	})
	assert.False(t, lifecycleOnly.Check("agent.attach"), "lifecycle must not imply attach")

	t.Run("project owner can mint agent:manage", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		project := &store.Project{ID: tid("uat-xattach-project"), Name: "UAT XAttach", Slug: "uat-xattach"}
		require.NoError(t, s.CreateProject(ctx, project))
		ownerID := tid("uat-xattach-owner")
		createTestUserWithProjectRole(t, s, ownerID, "uat-owner@test.com", project.ID, store.ProjectRoleOwner)

		_, _, err := srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "ci", project.ID,
			[]string{store.UATScopeAgentManage}, nil)
		require.NoError(t, err)
	})

	// ptone/scion#2092: explicit attach selection is relationship-eligible
	// (owner/ancestor) and requires no existing target, so a project owner
	// can mint an explicit agent:attach token for their own agents.
	t.Run("project owner can mint explicit attach for own agents", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		project := &store.Project{ID: tid("uat-xattach-project-2"), Name: "UAT XAttach 2", Slug: "uat-xattach-2"}
		require.NoError(t, s.CreateProject(ctx, project))
		ownerID := tid("uat-xattach-owner-2")
		createTestUserWithProjectRole(t, s, ownerID, "uat-owner-2@test.com", project.ID, store.ProjectRoleOwner)

		_, _, err := srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "attach", project.ID,
			[]string{"agent:attach"}, nil)
		require.NoError(t, err, "project owner should be able to select explicit attach for their own agents")

		// An explicit port-access token is likewise mintable by a project
		// owner. (Mint eligibility for this selector is governed by the
		// relationship-based mint eligibility rules, not pinned here.)
		_, _, err = srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "ports", project.ID,
			[]string{"agent:port_access"}, nil)
		assert.NoError(t, err, "project owner should be able to select explicit port_access")
	})
}

// TestOwnerPortAccess_OpenOnlyNotManage pins the scope of the port-access
// grant owners and admins carry: it opens a member's
// already-exposed ports through the proxy and nothing else. Registering,
// removing or tunnelling ports needs hub-level port_access, and the
// terminal-level actions stay behind agent.attach.
func TestOwnerPortAccess_OpenOnlyNotManage(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-portscope"), "Bob", store.GroupMemberRoleOwner)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, project.ID, store.ProjectRoleOwner)
	carol := makeProjectMemberUser(t, s, project, tid("user-carol-portscope"), "Carol", store.GroupMemberRoleMember)
	createTestUserWithProjectRole(t, s, carol.ID, carol.Email, project.ID, store.ProjectRoleMember)

	agent := &store.Agent{
		ID: tid("alice-agent-portscope"), Slug: "alice-agent-portscope", Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	base := "/api/v1/agents/" + agent.ID + "/ports"

	// Opening a port: the owner passes authorization (404 because nothing is
	// exposed on this test agent); a plain member is refused.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet, base+"/8080/proxy/", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "owner must pass port authorization: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, carol, http.MethodGet, base+"/8080/proxy/", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "member must not open another member's port: %s", rec.Body.String())

	// Managing ports stays out of reach for the owner.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, base},
		{http.MethodDelete, base},
		{http.MethodDelete, base + "/8080"},
	} {
		rec := doRequestAsUser(t, srv, bob, tc.method, tc.path, map[string]any{"port": 8080})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"owner must not manage a member's ports (%s %s): %s", tc.method, tc.path, rec.Body.String())
	}

	// The tunnel checks for a WebSocket upgrade before authorizing, so send
	// the handshake headers to reach the port-management gate.
	token, _, _, err := srv.userTokenService.GenerateTokenPair(bob.ID, bob.Email, bob.DisplayName, bob.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, base+"/tunnel", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	tunnel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(tunnel, req)
	assert.Equal(t, http.StatusForbidden, tunnel.Code, "owner must not open a member's port tunnel: %s", tunnel.Body.String())

	// And port access grants nothing terminal-level.
	for _, action := range []string{"exec", "env"} {
		rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, map[string]any{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "owner must not %s a member's agent: %s", action, rec.Body.String())
	}
}
