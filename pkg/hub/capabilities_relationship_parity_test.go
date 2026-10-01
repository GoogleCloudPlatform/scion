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

// Capabilities are computed from the common decision (ptone/scion#2119):
// every listed action equals Decide's answer for the same identity,
// resource and action.

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertCapabilitiesMatchDecide(t *testing.T, authz *AuthzService, identity Identity, resource Resource, caps *Capabilities, actions []Action) {
	t.Helper()
	require.NotNil(t, caps)
	for _, action := range actions {
		want := authz.CheckAccess(context.Background(), identity, resource, action).Allowed
		assert.Equal(t, want, capabilityAllows(caps, action), "%s %s: capability differs from the decision", resource.Type, action)
	}
}

// Project-scoped service accounts registered by another user: for the
// project owner, admin and member, the capability list equals the decision
// for every action, including read, delete, verify and assign.
func TestCapabilities_ProjectServiceAccountMatchesDecide(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	authz := srv.authzService

	sa := &store.GCPServiceAccount{
		ID: tid("sacap-sa"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "sacap@example.iam.gserviceaccount.com", ProjectID: "gcp-proj", CreatedBy: alice.ID,
	}
	res := gcpServiceAccountResource(sa)
	require.Equal(t, "project", res.ParentType)

	for _, tc := range []struct{ name, role string }{
		{"owner", store.GroupMemberRoleOwner},
		{"admin", store.GroupMemberRoleAdmin},
		{"member", store.GroupMemberRoleMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := makeProjectMemberUser(t, s, project, tid("sacap-"+tc.name), tc.name, tc.role)
			user := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, "member", "api")
			actions := ResourceActions["gcp_service_account"]
			for _, required := range []Action{ActionRead, ActionDelete, ActionVerify, ActionAssign} {
				require.Contains(t, actions, required)
			}

			assertCapabilitiesMatchDecide(t, authz, user, res, authz.ComputeCapabilities(ctx, user, res), actions)
			batch := authz.ComputeCapabilitiesBatch(ctx, user, []Resource{res}, "gcp_service_account")
			require.Len(t, batch, 1)
			assertCapabilitiesMatchDecide(t, authz, user, res, batch[0], actions)

			scope := Resource{Type: "gcp_service_account", ParentType: "project", ParentID: project.ID}
			assertCapabilitiesMatchDecide(t, authz, user, scope,
				authz.ComputeScopeCapabilities(ctx, user, "project", project.ID, "gcp_service_account"),
				ScopeActions["gcp_service_account"])
		})
	}
}

// Agents owned by another member and by the caller: single, batch and
// scope capabilities equal the decision for every action.
func TestCapabilities_AgentMatchesDecide(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	authz := srv.authzService

	for _, tc := range []struct{ name, role string }{
		{"owner", store.GroupMemberRoleOwner},
		{"admin", store.GroupMemberRoleAdmin},
		{"member", store.GroupMemberRoleMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := makeProjectMemberUser(t, s, project, tid("agcap-"+tc.name), tc.name, tc.role)
			user := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, "member", "api")
			others := &store.Agent{ID: tid("agcap-a-" + tc.name), Slug: tid("agcap-a-" + tc.name), Name: "A",
				ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning)}
			own := &store.Agent{ID: tid("agcap-b-" + tc.name), Slug: tid("agcap-b-" + tc.name), Name: "B",
				ProjectID: project.ID, OwnerID: u.ID, Phase: string(state.PhaseRunning)}
			require.NoError(t, s.CreateAgent(ctx, others))
			require.NoError(t, s.CreateAgent(ctx, own))
			resources := []Resource{agentResource(others), agentResource(own)}
			actions := ResourceActions["agent"]

			batch := authz.ComputeCapabilitiesBatch(ctx, user, resources, "agent")
			require.Len(t, batch, len(resources))
			for i, res := range resources {
				assertCapabilitiesMatchDecide(t, authz, user, res, authz.ComputeCapabilities(ctx, user, res), actions)
				assertCapabilitiesMatchDecide(t, authz, user, res, batch[i], actions)
			}
			// Attach follows the relationship to the agent, not the project
			// role. Owners and admins carry agent.port_access through their
			// role; members reach ports only on their own agents.
			assert.False(t, capabilityAllows(batch[0], ActionAttach), "attach on another member's agent")
			assert.Equal(t, tc.role != store.GroupMemberRoleMember, capabilityAllows(batch[0], ActionPortAccess),
				"port access on another member's agent")
			assert.True(t, capabilityAllows(batch[1], ActionAttach), "attach on own agent")

			scope := Resource{Type: "agent", ParentType: "project", ParentID: project.ID}
			assertCapabilitiesMatchDecide(t, authz, user, scope,
				authz.ComputeScopeCapabilities(ctx, user, "project", project.ID, "agent"), ScopeActions["agent"])
		})
	}
}
