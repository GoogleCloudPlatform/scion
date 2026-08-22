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
	"strconv"
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
	bob := makeProjectMemberUser(t, s, project, tid("user-bob-admin"), "Bob Admin", store.GroupMemberRoleAdmin)

	// Alice creates the agent.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent-1"), Slug: tid("alice-agent-1"), Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, tid("alice-agent-1"))
	require.NoError(t, err)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, agentResource(a), ActionDelete)
	assert.True(t, decision.Allowed, "project admin should be allowed to delete agents owned by other members; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_RegularMemberCannotUpdateProject(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := makeProjectMemberUser(t, s, project, tid("user-carol-member"), "Carol", store.GroupMemberRoleMember)

	user := NewAuthenticatedUser(carol.ID, carol.Email, carol.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.False(t, decision.Allowed, "regular member should NOT be allowed to update project; reason=%q", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_RegularMemberCannotDeleteOthersAgent(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := makeProjectMemberUser(t, s, project, tid("user-carol-member"), "Carol", store.GroupMemberRoleMember)

	// Alice creates the agent; carol is just a regular member.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent-2"), Slug: tid("alice-agent-2"), Name: "Alice Agent 2",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, tid("alice-agent-2"))
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

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-owner"), "Bob Owner", store.GroupMemberRoleOwner)

	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, groupResource(membersGroup), ActionAddMember)
	assert.True(t, decision.Allowed, "non-creator project owner should be allowed to add members; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_IgnoresNonCanonicalExplicitProjectGroups(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	outsider := &store.User{
		ID:          tid("user-noncanonical-owner"),
		Email:       "noncanonical-owner@test.com",
		DisplayName: "Noncanonical Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, s, outsider.ID)

	extraGroup := &store.Group{
		ID:        tid("group-noncanonical-owner"),
		Name:      "Noncanonical Project Group",
		Slug:      "noncanonical-project-group",
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, extraGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    extraGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   outsider.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	user := NewAuthenticatedUser(outsider.ID, outsider.Email, outsider.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.False(t, decision.Allowed, "owner of arbitrary project-scoped group must not become project owner; reason=%q", decision.Reason)
}

func TestAuthz_ProjectOwnerBypass_CanonicalLookupIgnoresListLimit(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-after-many-groups"), "Bob Many Groups", store.GroupMemberRoleOwner)
	for i := 0; i < 12; i++ {
		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID:        tid("group-project-extra-" + strconv.Itoa(i)),
			Name:      "Extra Project Group " + strconv.Itoa(i),
			Slug:      "extra-project-group-" + strconv.Itoa(i),
			GroupType: store.GroupTypeExplicit,
			ProjectID: project.ID,
		}))
	}

	user := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.True(t, decision.Allowed, "canonical project members group must be found even with more than 10 explicit groups; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

func TestProjectMembersGroup_SlugSquatDoesNotGrantFutureProjectOwnership(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	attacker := &store.User{
		ID:          tid("user-slug-squat-attacker"),
		Email:       "slug-squat-attacker@test.com",
		DisplayName: "Slug Squat Attacker",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	creator := &store.User{
		ID:          tid("user-slug-squat-creator"),
		Email:       "slug-squat-creator@test.com",
		DisplayName: "Slug Squat Creator",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, attacker))
	require.NoError(t, s.CreateUser(ctx, creator))
	ensureHubMembership(ctx, s, attacker.ID)
	ensureHubMembership(ctx, s, creator.ID)

	squatted := &store.Group{
		ID:        tid("group-slug-squat-members"),
		Name:      "Squatted Members",
		Slug:      "project:slug-squat-project:members",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   attacker.ID,
		CreatedBy: attacker.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, squatted))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    squatted.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   attacker.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	project := &store.Project{
		ID:        tid("project-slug-squat"),
		Name:      "Slug Squat Project",
		Slug:      "slug-squat-project",
		OwnerID:   creator.ID,
		CreatedBy: creator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	got, err := s.GetGroup(ctx, squatted.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ProjectID, "slug collision must not attach a user-created group to the new project")

	user := NewAuthenticatedUser(attacker.ID, attacker.Email, attacker.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, projectResource(project), ActionUpdate)
	assert.False(t, decision.Allowed, "slug squatter must not become owner of a later project; reason=%q", decision.Reason)
}

func TestProjectMembersGroup_AllowsExistingSystemGroupForSameProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	creator := &store.User{
		ID:          tid("user-system-group-creator"),
		Email:       "system-group-creator@test.com",
		DisplayName: "System Group Creator",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, creator))
	ensureHubMembership(ctx, s, creator.ID)

	project := &store.Project{
		ID:        tid("project-system-group"),
		Name:      "System Group Project",
		Slug:      "system-group-project",
		OwnerID:   creator.ID,
		CreatedBy: creator.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	membersGroup := &store.Group{
		ID:        tid("group-existing-system-members"),
		Name:      "System Group Project Members",
		Slug:      projectMembersGroupSlug(project.Slug),
		GroupType: store.GroupTypeExplicit,
		ProjectID: project.ID,
		Annotations: map[string]string{
			systemProjectMembersGroupAnnotation: "true",
		},
	}
	require.NoError(t, s.CreateGroup(ctx, membersGroup))

	srv.createProjectMembersGroupAndPolicy(ctx, project)

	membership, err := s.GetGroupMembership(ctx, membersGroup.ID, store.GroupMemberTypeUser, creator.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleOwner, membership.Role)
}

// =============================================================================
// ComputeCapabilities: project owner/admin gets all actions
// =============================================================================

func TestCapabilities_ProjectOwnerBypass_ProjectAllActions(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-cap"), "Bob", store.GroupMemberRoleOwner)

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

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-cap-a"), "Bob", store.GroupMemberRoleOwner)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent-cap"), Slug: tid("alice-agent-cap"), Name: "Alice Agent Cap",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, tid("alice-agent-cap"))
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

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-batch"), "Bob", store.GroupMemberRoleOwner)

	// Two agents: one owned by alice, one by bob.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("agent-alice-b"), Slug: tid("agent-alice-b"), Name: "AliceB",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("agent-bob-b"), Slug: tid("agent-bob-b"), Name: "BobB",
		ProjectID: project.ID, OwnerID: bob.ID, Phase: string(state.PhaseRunning),
	}))

	a1, err := s.GetAgent(ctx, tid("agent-alice-b"))
	require.NoError(t, err)
	a2, err := s.GetAgent(ctx, tid("agent-bob-b"))
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

	bob := makeProjectMemberUser(t, s, project, tid("user-bob-scope"), "Bob", store.GroupMemberRoleOwner)

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

	carol := makeProjectMemberUser(t, s, project, tid("user-carol-cap"), "Carol", store.GroupMemberRoleMember)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent-cap2"), Slug: tid("alice-agent-cap2"), Name: "Alice Agent Cap2",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	a, err := s.GetAgent(ctx, tid("alice-agent-cap2"))
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
	bob := makeProjectMemberUser(t, s, project, tid("user-bob-http-owner"), "Bob HTTP", store.GroupMemberRoleOwner)

	body := map[string]string{"description": "updated by bob"}
	rec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/projects/"+project.ID, body)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project owner (non-creator) should not get 403 on update; got: %s", rec.Body.String())
}

func TestUpdateProject_RegularMemberDenied(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	carol := makeProjectMemberUser(t, s, project, tid("user-carol-http"), "Carol HTTP", store.GroupMemberRoleMember)

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

// =============================================================================
// GCP service account scope → parent mapping (P0.2)
//
// gcpServiceAccountResource sets ParentType="project" only for project-scoped
// accounts. These tests pin the consequence: the project owner/admin bypass in
// checkAccessForUser reaches project-scoped accounts and nothing else.
// =============================================================================

func TestAuthz_GCPServiceAccount_ProjectScoped_OwnerBypassApplies(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob owns the SA; alice owns the project it is scoped to.
	sa := &store.GCPServiceAccount{
		ID:        tid("sa-project-scoped"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "sa-proj@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("user-bob"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	user := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, gcpServiceAccountResource(sa), ActionDelete)
	assert.True(t, decision.Allowed,
		"project owner should reach a project-scoped SA via the bypass; reason=%q", decision.Reason)
	assert.Equal(t, "project owner/admin", decision.Reason)
}

// The regression this task exists to prevent. Goal 2 introduces hub-scoped SAs,
// whose ScopeID is a hub instance ID drawn from a different namespace than
// project IDs. Under the previous unconditional ParentType="project" the two
// namespaces were conflated, so whoever owned the project that happened to
// share an ID with the hub would inherit full access to a hub-wide credential.
// The test forces the collision directly rather than hoping it never happens.
func TestAuthz_GCPServiceAccount_HubScoped_NoProjectOwnerBypass(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:      tid("sa-hub-scoped"),
		Scope:   store.ScopeHub,
		ScopeID: project.ID, // deliberate collision: a hub ID equal to a project ID
		Email:   "sa-hub@example.iam.gserviceaccount.com",
		// CreatedBy is deliberately not alice: the owner short-circuit would
		// otherwise mask whether the parent bypass fired.
		ProjectID: "gcp-proj",
		CreatedBy: tid("user-bob"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	resource := gcpServiceAccountResource(sa)
	require.Empty(t, resource.ParentType, "hub-scoped SA must not carry a project parent")

	user := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, user, resource, ActionDelete)
	assert.False(t, decision.Allowed,
		"project owner must NOT reach a hub-scoped SA whose ScopeID collides with their project ID; reason=%q",
		decision.Reason)
	assert.NotEqual(t, "project owner/admin", decision.Reason)
}

// ⚠️ SECURITY-RELEVANT (P9 update): This test is modified by P9 to reflect
// the D5 hub member baseline. Alice is a current hub member, so the baseline
// now grants ActionAssign on hub-scoped SAs. The project-owner bypass still
// does NOT fire for hub-scoped SAs (the engine property that confines it is
// unchanged), but the hub member baseline adds assign to the allowed set.
//
// The assertion for hub-scoped SAs changes from ["read"] to ["read", "assign"]
// because alice is a hub member and the D5 baseline applies. The project-scoped
// control is unchanged.
func TestCapabilities_GCPServiceAccount_HubScoped_NoProjectOwnerBypass(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        tid("sa-hub-scoped-caps"),
		Scope:     store.ScopeHub,
		ScopeID:   project.ID,
		Email:     "sa-hub-caps@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("user-bob"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	// A project-scoped sibling, identical but for Scope, is the control.
	saProject := &store.GCPServiceAccount{
		ID:        tid("sa-project-scoped-caps"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "sa-proj-caps@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("user-bob"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saProject))

	user := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, "member", "api")

	// P9: "read" still comes from hub-member-read-all. "assign" now comes
	// from the D5 hub member baseline for hub-scoped SAs (alice is a current
	// hub member). The project-owner bypass still does NOT fire for hub-scoped
	// SAs — that engine property is unchanged.
	hubCaps := srv.authzService.ComputeCapabilities(ctx, user, gcpServiceAccountResource(sa))
	assert.Equal(t, []string{"read", "assign"}, hubCaps.Actions,
		"hub member should see read (hub-member-read-all) + assign (D5 baseline) on hub-scoped SA")

	projectCaps := srv.authzService.ComputeCapabilities(ctx, user, gcpServiceAccountResource(saProject))
	assert.Equal(t, []string{"read", "delete", "verify", "assign"}, projectCaps.Actions,
		"the project-scoped control should still get the full bypass")
}
