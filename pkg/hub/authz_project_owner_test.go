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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addProjectMemberWithRole is a small helper that adds the given user to the
// project's members group with the requested role.
func addProjectMemberWithRole(t *testing.T, s store.Store, project *store.Project, userID, role string) {
	t.Helper()
	ctx := context.Background()
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       role,
	}))
}

// makeProjectMemberUser creates a user, adds them to hub-members, and adds them
// to the project's members group with the given role.
func makeProjectMemberUser(t *testing.T, s store.Store, project *store.Project, id, name, role string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          id,
		Email:       id + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, u.ID)
	addProjectMemberWithRole(t, s, project, u.ID, role)
	return u
}

// =============================================================================
// AuthzService.CheckAccess: project owner/admin bypass
// =============================================================================

func TestAuthz_ProjectOwnerBypass_NonCreatorOwnerCanUpdateProject(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Promote bob to owner of the project members group (without being the creator).
	addProjectMemberWithRole(t, s, project, bob.ID, store.GroupMemberRoleOwner)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.True(t, decision.Allowed, "non-creator owner should be allowed to update project; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_NonCreatorAdminCanDeleteAgent(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob joins the project as admin (not creator, not direct OwnerID).
	bob := makeProjectMemberUser(t, s, project, "user-bob-admin", "Bob Admin", store.GroupMemberRoleAdmin)

	// Alice creates the agent.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "alice-agent-1", Slug: "alice-agent-1", Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, "alice-agent-1")
	require.NoError(t, err)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, agentResource(a), ActionDelete)
	assert.True(t, decision.Allowed, "project admin should be allowed to delete agents owned by other members; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_RegularMemberCannotUpdateProject(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := makeProjectMemberUser(t, s, project, "user-carol-member", "Carol", store.GroupMemberRoleMember)

	user := NewAuthenticatedUser(carol.ID, carol.Email, carol.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.False(t, decision.Allowed, "regular member should NOT be allowed to update project; reason=%q", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_RegularMemberCannotDeleteOthersAgent(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := makeProjectMemberUser(t, s, project, "user-carol-member", "Carol", store.GroupMemberRoleMember)

	// Alice creates the agent; carol is just a regular member.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "alice-agent-2", Slug: "alice-agent-2", Name: "Alice Agent 2",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, "alice-agent-2")
	require.NoError(t, err)

	user := NewAuthenticatedUser(carol.ID, carol.Email, carol.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, agentResource(a), ActionDelete)
	assert.False(t, decision.Allowed, "regular member should NOT be allowed to delete another member's agent; reason=%q", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_CreatorOwnerStillWorks(t *testing.T) {
	srv, _, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	user := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.True(t, decision.Allowed, "project creator (direct OwnerID) should still be allowed; reason=%q", decision.Reason)
	// The OwnerID bypass is checked before the project owner/admin bypass.
	assert.Equal(t, "resource owner", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_AppliesToProjectMembersGroup(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, "user-bob-owner", "Bob Owner", store.GroupMemberRoleOwner)

	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, groupResource(membersGroup), ActionAddMember)
	assert.True(t, decision.Allowed, "non-creator project owner should be allowed to add members; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

// =============================================================================
// ComputeCapabilities: project owner/admin gets all actions
// =============================================================================

func TestCapabilities_ProjectOwnerBypass_ProjectAllActions(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, "user-bob-cap", "Bob", store.GroupMemberRoleOwner)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	caps := srv.authzService.ComputeCapabilities(ctx, user, projectResource(project))
	for _, action := range ResourceActions["project"] {
		assert.Contains(t, caps.Actions, string(action),
			"non-creator project owner should have %q on project", action)
	}
}

func TestCapabilities_ProjectOwnerBypass_AgentAllActions(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, "user-bob-cap-a", "Bob", store.GroupMemberRoleOwner)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "alice-agent-cap", Slug: "alice-agent-cap", Name: "Alice Agent Cap",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, "alice-agent-cap")
	require.NoError(t, err)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	caps := srv.authzService.ComputeCapabilities(ctx, user, agentResource(a))
	for _, action := range ResourceActions["agent"] {
		assert.Contains(t, caps.Actions, string(action),
			"project owner should have %q on another member's agent", action)
	}
}

func TestCapabilities_ProjectOwnerBypass_BatchAllActions(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, "user-bob-batch", "Bob", store.GroupMemberRoleOwner)

	// Two agents: one owned by alice, one by bob.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "agent-alice-b", Slug: "agent-alice-b", Name: "AliceB",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "agent-bob-b", Slug: "agent-bob-b", Name: "BobB",
		ProjectID: project.ID, OwnerID: bob.ID, Phase: string(state.PhaseRunning),
	}))

	a1, err := s.GetAgent(ctx, "agent-alice-b")
	require.NoError(t, err)
	a2, err := s.GetAgent(ctx, "agent-bob-b")
	require.NoError(t, err)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	resources := []Resource{agentResource(a1), agentResource(a2)}
	capsList := srv.authzService.ComputeCapabilitiesBatch(ctx, user, resources, "agent")
	require.Len(t, capsList, 2)
	for i, caps := range capsList {
		for _, action := range ResourceActions["agent"] {
			assert.Contains(t, caps.Actions, string(action),
				"agent[%d]: project owner should have %q in batch result", i, action)
		}
	}
}

func TestCapabilities_ProjectOwnerBypass_ScopeAllActions(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, "user-bob-scope", "Bob", store.GroupMemberRoleOwner)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	caps := srv.authzService.ComputeScopeCapabilities(ctx, user, "project", project.ID, "agent")
	for _, action := range ScopeActions["agent"] {
		assert.Contains(t, caps.Actions, string(action),
			"project owner should have scope action %q for agent in their project", action)
	}
}

func TestCapabilities_RegularMember_AgentLimitedActions(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := makeProjectMemberUser(t, s, project, "user-carol-cap", "Carol", store.GroupMemberRoleMember)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "alice-agent-cap2", Slug: "alice-agent-cap2", Name: "Alice Agent Cap2",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, "alice-agent-cap2")
	require.NoError(t, err)

	user := NewAuthenticatedUser(carol.ID, carol.Email, carol.DisplayName, "member", "api")
	caps := srv.authzService.ComputeCapabilities(ctx, user, agentResource(a))
	assert.NotContains(t, caps.Actions, string(ActionDelete),
		"regular member should NOT get delete on another member's agent")
	assert.NotContains(t, caps.Actions, string(ActionUpdate),
		"regular member should NOT get update on another member's agent")
}

// =============================================================================
// HTTP-level checks: closes the latent open-update bug on /projects/{id}.
// =============================================================================

func TestUpdateProject_NonCreatorOwnerAllowed(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	bob := makeProjectMemberUser(t, s, project, "user-bob-http-owner", "Bob HTTP", store.GroupMemberRoleOwner)

	body := map[string]string{"description": "updated by bob"}
	rec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/projects/"+project.ID, body)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project owner (non-creator) should not get 403 on update; got: %s", rec.Body.String())
}

func TestUpdateProject_RegularMemberDenied(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	carol := makeProjectMemberUser(t, s, project, "user-carol-http", "Carol HTTP", store.GroupMemberRoleMember)

	body := map[string]string{"description": "updated by carol"}
	rec := doRequestAsUser(t, srv, carol, http.MethodPatch, "/api/v1/projects/"+project.ID, body)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"regular member should be denied PATCH /projects; got: %s body=%s", http.StatusText(rec.Code), rec.Body.String())
}

func TestUpdateProject_OutsiderDenied(t *testing.T) {
	srv, _, _, bob, project := setupDemoPolicyTest(t)
	// Bob is a hub-member but NOT a project member at all.
	body := map[string]string{"description": "updated by bob (outsider)"}
	rec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/projects/"+project.ID, body)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-project user should be denied PATCH /projects; got: %s body=%s", http.StatusText(rec.Code), rec.Body.String())
}

func TestUpdateProject_CreatorOwnerAllowed(t *testing.T) {
	srv, _, alice, _, project := setupDemoPolicyTest(t)
	body := map[string]string{"description": "updated by alice (creator)"}
	rec := doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/projects/"+project.ID, body)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"creator should still be allowed PATCH /projects; got: %s", rec.Body.String())

	// Best-effort: parse to confirm the response is well-formed JSON.
	if rec.Code == http.StatusOK {
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
}
