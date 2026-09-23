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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossMemberAttach_Matrix verifies that project owners/admins cannot
// attach to (or reach ports of) agents owned by other project members, since
// those agents run with their owner's user-scoped secrets (miller79/scion#88).
// Owners keep access to their own agents and progeny through relationship
// grants, and non-attach actions still flow through the owner/admin roles.
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
		allowed  bool
	}{
		{"owner attaches to own agent", owner, ownerAgent, true},
		{"owner attaches to own progeny", owner, ownerProgeny, true},
		{"owner attaches to member's agent", owner, memberAgent, false},
		{"admin attaches to own agent", admin, adminAgent, true},
		{"admin attaches to member's agent", admin, memberAgent, false},
		{"admin attaches to owner's agent", admin, ownerAgent, false},
		{"member attaches to own agent", member, memberAgent, true},
		{"member attaches to other's agent", member, ownerAgent, false},
		{"super-admin attaches to member's agent", superAdmin, memberAgent, true},
	}
	for _, tc := range cases {
		for _, action := range []Action{ActionAttach, ActionPortAccess} {
			t.Run(tc.name+"/"+string(action), func(t *testing.T) {
				d := f.authz.CheckAccess(ctx, tc.identity, tc.resource, action)
				assert.Equal(t, tc.allowed, d.Allowed, "reason: %s", d.Reason)
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
		assert.NotContains(t, caps.Actions, string(ActionPortAccess))
		assert.Contains(t, caps.Actions, string(ActionRead))
		assert.Contains(t, caps.Actions, string(ActionDelete))
		assert.Contains(t, caps.Actions, string(ActionLifecycle))

		caps = f.authz.ComputeCapabilities(ctx, owner, ownerAgent)
		assert.Contains(t, caps.Actions, string(ActionAttach))
		assert.Contains(t, caps.Actions, string(ActionPortAccess))

		batch := f.authz.ComputeCapabilitiesBatch(ctx, admin, []Resource{memberAgent, adminAgent}, "agent")
		require.Len(t, batch, 2)
		assert.NotContains(t, batch[0].Actions, string(ActionAttach))
		assert.NotContains(t, batch[0].Actions, string(ActionPortAccess))
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
// agent:manage no longer requires agent.attach (so project owners can mint it),
// and tokens minted before the split that carry agent:attach keep lifecycle.
func TestCrossMemberAttach_UATScopes(t *testing.T) {
	manage := permissions.UATManageScopes()
	assert.Contains(t, manage, "agent:lifecycle")
	assert.NotContains(t, manage, "agent:attach")
	assert.NotContains(t, manage, "agent:port_access")
	assert.True(t, permissions.UATValidScopes()["agent:attach"], "agent:attach stays explicitly mintable")

	legacy := uatScopeRestriction([]string{"agent:attach"})
	assert.True(t, legacy.Check("agent.lifecycle"), "legacy agent:attach tokens keep lifecycle")
	assert.True(t, legacy.Check("agent.attach"))
	readOnly := uatScopeRestriction([]string{"agent:read"})
	assert.False(t, readOnly.Check("agent.lifecycle"))
	lifecycleOnly := uatScopeRestriction([]string{"agent:lifecycle"})
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

		_, _, err = srv.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "attach", project.ID,
			[]string{"agent:attach"}, nil)
		assert.ErrorIs(t, err, ErrUATScopeViolation,
			"owner role no longer carries agent.attach, so an explicit attach token exceeds issuer authority")
	})
}
