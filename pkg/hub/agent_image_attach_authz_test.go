//go:build !no_sqlite

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

package hub

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeHubRoleUser(t *testing.T, s store.Store, id, name, userRole, hubRoleName string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          id,
		Email:       id + "@test.example.com",
		DisplayName: name,
		Role:        userRole,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, u))
	if hubRoleName != "" {
		rd, err := s.GetRoleDefinitionByName(ctx, hubRoleName, store.RoleScopeSystem)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeSystem,
			ScopeID:          "",
			CreatedBy:        store.SystemReconcileCreatedBy,
		})
		require.NoError(t, err)
	}
	return u
}

func recordImageTestDelegationEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, projectID string) {
	t.Helper()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  delegateType,
		DelegateID:    delegateID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
}

// TestPatchAgent_ImageEditRequiresAttach verifies ptone/scion#3402:
// changing an agent's image via PATCH (global or project-scoped, live or held)
// requires agent.attach in addition to agent.update, matching the agent.attach
// rule for every caller kind.
func TestPatchAgent_ImageEditRequiresAttach(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	srv.createProjectMembersGroup(ctx, project)

	otherProject := &store.Project{
		ID:   tid("other-proj"),
		Name: "Other Project",
		Slug: tid("other-proj"),
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	srv.createProjectMembersGroup(ctx, otherProject)

	projOwner := makeProjectMemberUser(t, s, project, tid("img-proj-owner"), "Proj Owner", store.GroupMemberRoleOwner)
	projAdmin := makeProjectMemberUser(t, s, project, tid("img-proj-admin"), "Proj Admin", store.GroupMemberRoleAdmin)
	agentOwner := makeProjectMemberUser(t, s, project, tid("img-agent-owner"), "Agent Owner", store.GroupMemberRoleMember)
	explicitAttachUser := makeProjectMemberUser(t, s, project, tid("img-explicit-attach"), "Explicit Attach", store.GroupMemberRoleMember)
	grantPermissionViaRoleBinding(t, s, explicitAttachUser.ID, "agent.update", store.RoleScopeProject, project.ID)
	grantPermissionViaRoleBinding(t, s, explicitAttachUser.ID, "agent.attach", store.RoleScopeProject, project.ID)
	otherProjOwner := makeProjectMemberUser(t, s, otherProject, tid("img-other-proj"), "Other Proj Owner", store.GroupMemberRoleOwner)
	hubAdmin := makeHubRoleUser(t, s, tid("img-hub-admin"), "Hub Admin", store.UserRoleMember, "hub-admin")
	superAdmin := makeHubRoleUser(t, s, tid("img-super-admin"), "Super Admin", store.UserRoleAdmin, store.SystemRoleSuperAdmin)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Name = "original-name"
		a.Phase = string(state.PhaseStopped)
		a.OwnerID = agentOwner.ID
		a.CreatedBy = agentOwner.ID
		a.Ancestry = []string{agentOwner.ID}
		a.AppliedConfig.Image = "orig-image:v1"
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness: "claude",
			Image:   "orig-image:v1",
			Model:   "old-model",
		}
	})

	// Preconditions: project owner and admin hold update but NOT attach on
	// another member's agent; agent owner, explicit-attach user, and super-admin
	// hold both; hub-admin and other-project owner hold neither attach nor update.
	for _, tc := range []struct {
		name   string
		user   *store.User
		update bool
		attach bool
	}{
		{"project owner", projOwner, true, false},
		{"project admin", projAdmin, true, false},
		{"agent owner", agentOwner, true, true},
		{"explicit attach user", explicitAttachUser, true, true},
		{"super admin", superAdmin, true, true},
		{"hub admin", hubAdmin, false, false},
		{"other project owner", otherProjOwner, false, false},
	} {
		ident := NewAuthenticatedUser(tc.user.ID, tc.user.Email, tc.user.DisplayName, tc.user.Role, "session")
		assert.Equal(t, tc.update, srv.authzService.CheckAccess(ctx, ident, agentResource(agent), ActionUpdate).Allowed, "%s update", tc.name)
		assert.Equal(t, tc.attach, srv.authzService.CheckAccess(ctx, ident, agentResource(agent), ActionAttach).Allowed, "%s attach", tc.name)
	}

	// Positive control: project owner and admin can still PATCH non-image fields.
	for _, caller := range []*store.User{projOwner, projAdmin} {
		rec := doRequestAsUser(t, srv, caller, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"model": "updated-model"},
		})
		require.Equal(t, http.StatusOK, rec.Code, "%s non-image PATCH: %s", caller.DisplayName, rec.Body.String())
	}

	// Refused callers: project owner, project admin, hub admin, other-project
	// owner, UAT without agent:attach, UAT scoped to another project, and agent
	// callers are all refused with 403 and nothing in the agent changes.
	assertUnchanged := func(t *testing.T) {
		t.Helper()
		stored, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "original-name", stored.Name, "name must not mutate on refused image PATCH")
		assert.Equal(t, "orig-image:v1", stored.AppliedConfig.Image)
		assert.Equal(t, "orig-image:v1", stored.AppliedConfig.InlineConfig.Image)
	}

	for _, caller := range []*store.User{projOwner, projAdmin, hubAdmin, otherProjOwner} {
		for _, tc := range []struct {
			name string
			path string
			body map[string]interface{}
		}{
			{
				name: "global set image + rename",
				path: "/api/v1/agents/" + agent.ID,
				body: map[string]interface{}{
					"name":   "should-not-stick",
					"config": map[string]interface{}{"image": "evil-image:v2"},
				},
			},
			{
				name: "global echo existing image",
				path: "/api/v1/agents/" + agent.ID,
				body: map[string]interface{}{
					"config": map[string]interface{}{"image": "orig-image:v1"},
				},
			},
			{
				name: "project-scoped set image",
				path: "/api/v1/projects/" + project.ID + "/agents/" + agent.ID,
				body: map[string]interface{}{
					"config": map[string]interface{}{"image": "evil-image:v2"},
				},
			},
		} {
			t.Run(caller.DisplayName+"/"+tc.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, caller, http.MethodPatch, tc.path, tc.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), "orig-image:v1", "403 must not leak current image")
				assert.NotContains(t, rec.Body.String(), "evil-image:v2", "403 must not leak requested image")
				assertUnchanged(t)
			})
		}
	}

	t.Run("owner UAT without agent:attach is refused", func(t *testing.T) {
		uat := scopedIdentityFor(agentOwner, project.ID, append(minimalSelectors(t), "agent:update"))
		rec := doRequestAsIdentity(t, srv, uat, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"image": "uat-no-attach:v1"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertUnchanged(t)
	})

	t.Run("owner UAT bounded to another project is refused", func(t *testing.T) {
		uat := scopedIdentityFor(agentOwner, otherProject.ID, append(minimalSelectors(t), "agent:update", "agent:attach"))
		rec := doRequestAsIdentity(t, srv, uat, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"image": "uat-other-proj:v1"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertUnchanged(t)
	})

	t.Run("agent caller is refused on PATCH", func(t *testing.T) {
		callerAgent := agentIdentityFor(tid("caller-agent"), project.ID, ScopeAgentLifecycle)
		rec := doRequestAsIdentity(t, srv, callerAgent, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"image": "agent-caller:v1"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertUnchanged(t)
	})

	// Held edits path enforces ActionAttach before saving pending config.
	t.Run("held edits refused without attach", func(t *testing.T) {
		cur, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		cur.Phase = string(state.PhaseRunning)
		require.NoError(t, s.UpdateAgent(ctx, cur))
		var active []experiments.Experiment
		for _, e := range experiments.Default().All() {
			if e.Name == experiments.AgentEditHeld {
				e.Default = true
			}
			active = append(active, e)
		}
		reg, err := experiments.NewRegistry(active, nil)
		require.NoError(t, err)
		prevReg := srv.experiments
		srv.experiments = reg
		defer func() {
			srv.experiments = prevReg
			cur, _ := s.GetAgent(ctx, agent.ID)
			cur.Phase = string(state.PhaseStopped)
			_ = s.UpdateAgent(ctx, cur)
		}()

		rec := doRequestAsUser(t, srv, projOwner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"image": "held-evil:v1"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		stored, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.True(t, stored.PendingConfig.IsEmpty(), "no pending config saved on refused image PATCH")
	})

	// Admitted callers: agent owner, owner UAT with agent:attach, non-owner
	// with explicit agent.attach, and super-admin.
	for _, tc := range []struct {
		name  string
		ident Identity
		image string
	}{
		{
			name:  "agent owner session",
			ident: NewAuthenticatedUser(agentOwner.ID, agentOwner.Email, agentOwner.DisplayName, agentOwner.Role, "session"),
			image: "owner-image:v2",
		},
		{
			name:  "owner UAT with agent:update and agent:attach",
			ident: scopedIdentityFor(agentOwner, project.ID, append(minimalSelectors(t), "agent:update", "agent:attach")),
			image: "owner-uat-image:v3",
		},
		{
			name:  "non-owner with explicit agent.attach",
			ident: NewAuthenticatedUser(explicitAttachUser.ID, explicitAttachUser.Email, explicitAttachUser.DisplayName, explicitAttachUser.Role, "session"),
			image: "explicit-attach-image:v4",
		},
		{
			name:  "super-admin",
			ident: NewAuthenticatedUser(superAdmin.ID, superAdmin.Email, superAdmin.DisplayName, superAdmin.Role, "session"),
			image: "super-admin-image:v5",
		},
	} {
		t.Run("admitted/"+tc.name, func(t *testing.T) {
			rec := doRequestAsIdentity(t, srv, tc.ident, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"image": tc.image},
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			stored, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.image, stored.AppliedConfig.Image)
		})
	}
}

// TestPendingReauth_ImageEditRequiresAttach verifies ptone/scion#3402 +
// Invariant ruling A: a saver who held agent.attach when saving a pending
// config.image edit, and loses agent.attach before fold (keeping only
// agent.update via project-admin), has config.image dropped with
// saver_unauthorized rather than applied.
func TestPendingReauth_ImageEditRequiresAttach(t *testing.T) {
	srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
	a := effectUser(t, s, agent.ProjectID, "img-saver")
	setOwner := func(owner string) {
		cur := pendingReloadAgent(t, s, agent.ID)
		cur.OwnerID = owner
		require.NoError(t, s.UpdateAgent(context.Background(), cur))
	}
	setOwner(a.ID)

	resp := effectPatchAs(t, srv, a, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"image": "held-new-image:v2"},
	})
	assert.Equal(t, []string{"config.image"}, resp.Disposition.Held)

	// Another user becomes the owner: A keeps project-admin (agent.update)
	// but no longer holds agent.attach.
	setOwner(effectUser(t, s, agent.ProjectID, "img-new-owner").ID)

	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.NotEqual(t, "held-new-image:v2", starts[0].inline.Image, "image edit must be dropped once saver loses attach")
	assert.Contains(t, warnings, effectDropWarning(pendingDiscardTriggerSaverUnauthorized, "config.image"))
	assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.image"}}, effectDiscards(t, s, agent.ID, 1))
}

// TestReincarnateAgent_ImageEditRequiresAttach verifies ptone/scion#3402:
// changing an agent's image via POST .../reincarnate (global or project-scoped,
// dryRun or live) requires agent.attach, matching the agent.attach rule for
// every caller kind.
func TestReincarnateAgent_ImageEditRequiresAttach(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	srv.createProjectMembersGroup(ctx, project)

	otherProject := &store.Project{
		ID:   tid("reinc-other-proj"),
		Name: "Other Project",
		Slug: tid("reinc-other-proj"),
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	srv.createProjectMembersGroup(ctx, otherProject)

	projOwner := makeProjectMemberUser(t, s, project, tid("reinc-img-proj-owner"), "Proj Owner", store.GroupMemberRoleOwner)
	projAdmin := makeProjectMemberUser(t, s, project, tid("reinc-img-proj-admin"), "Proj Admin", store.GroupMemberRoleAdmin)
	agentOwner := makeProjectMemberUser(t, s, project, tid("reinc-img-agent-owner"), "Agent Owner", store.GroupMemberRoleMember)
	explicitAttachUser := makeProjectMemberUser(t, s, project, tid("reinc-img-explicit-attach"), "Explicit Attach", store.GroupMemberRoleMember)
	grantAgentLifecycleAtProject(t, s, explicitAttachUser.ID, project.ID)
	grantPermissionViaRoleBinding(t, s, explicitAttachUser.ID, "agent.update", store.RoleScopeProject, project.ID)
	grantPermissionViaRoleBinding(t, s, explicitAttachUser.ID, "agent.attach", store.RoleScopeProject, project.ID)
	grantAgentDelegationAtProject(t, s, explicitAttachUser.ID, project.ID)
	grantAgentDelegationAtProject(t, s, projOwner.ID, project.ID)
	grantAgentDelegationAtProject(t, s, projAdmin.ID, project.ID)
	grantAgentDelegationAtProject(t, s, agentOwner.ID, project.ID)

	otherProjOwner := makeProjectMemberUser(t, s, otherProject, tid("reinc-img-other-proj"), "Other Proj Owner", store.GroupMemberRoleOwner)
	hubAdmin := makeHubRoleUser(t, s, tid("reinc-img-hub-admin"), "Hub Admin", store.UserRoleMember, "hub-admin")
	superAdmin := makeHubRoleUser(t, s, tid("reinc-img-super-admin"), "Super Admin", store.UserRoleAdmin, store.SystemRoleSuperAdmin)

	// Parent agent owned by agentOwner; child agent created by parentAgent.
	parentAgent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("reinc-parent-agent")
		a.Slug = "reinc-parent-agent"
		a.Name = "reinc-parent-agent"
		a.OwnerID = agentOwner.ID
		a.CreatedBy = agentOwner.ID
		a.Ancestry = []string{agentOwner.ID}
	})
	otherMemberAgent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("reinc-other-member-agent")
		a.Slug = "reinc-other-member-agent"
		a.Name = "reinc-other-member-agent"
		a.OwnerID = projOwner.ID
		a.CreatedBy = projOwner.ID
		a.Ancestry = []string{projOwner.ID}
	})
	otherProjAgent := newReincarnateTestAgent(t, s, otherProject, broker, func(a *store.Agent) {
		a.ID = tid("reinc-other-proj-agent")
		a.Slug = "reinc-other-proj-agent"
		a.Name = "reinc-other-proj-agent"
		a.OwnerID = otherProjOwner.ID
		a.CreatedBy = otherProjOwner.ID
		a.Ancestry = []string{otherProjOwner.ID}
	})

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.OwnerID = agentOwner.ID
		a.CreatedBy = parentAgent.ID
		a.Ancestry = []string{agentOwner.ID, parentAgent.ID}
	})
	recordImageTestDelegationEdge(t, s, store.DelegationPrincipalUser, agentOwner.ID, store.DelegationPrincipalAgent, parentAgent.ID, project.ID)
	recordImageTestDelegationEdge(t, s, store.DelegationPrincipalAgent, parentAgent.ID, store.DelegationPrincipalAgent, agent.ID, project.ID)
	recordImageTestDelegationEdge(t, s, store.DelegationPrincipalUser, projOwner.ID, store.DelegationPrincipalAgent, otherMemberAgent.ID, project.ID)

	// Positive control: project owner and admin can still reincarnate another
	// member's agent without --image (plain or with non-image patch like --model).
	for _, caller := range []*store.User{projOwner, projAdmin} {
		for _, body := range []ReincarnateAgentRequest{
			{DryRun: true},
			{DryRun: true, Model: "other-model"},
		} {
			rec := doRequestAsUser(t, srv, caller, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", body)
			require.Equal(t, http.StatusOK, rec.Code, "%s %+v: %s", caller.DisplayName, body, rec.Body.String())
		}
	}

	// Refused user callers (global and project-scoped, dryRun and live).
	before := snapshotAgent(t, s, agent.ID)
	for _, caller := range []*store.User{projOwner, projAdmin, hubAdmin, otherProjOwner} {
		for _, tc := range []struct {
			name string
			path string
			body ReincarnateAgentRequest
		}{
			{"global live", "/api/v1/agents/" + agent.ID + "/reincarnate", ReincarnateAgentRequest{Image: "evil:v1"}},
			{"global dryRun", "/api/v1/agents/" + agent.ID + "/reincarnate", ReincarnateAgentRequest{DryRun: true, Image: "evil:v1"}},
			{"project-scoped dryRun", "/api/v1/projects/" + project.ID + "/agents/" + agent.ID + "/reincarnate", ReincarnateAgentRequest{DryRun: true, Image: "evil:v1"}},
		} {
			t.Run(caller.DisplayName+"/"+tc.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, caller, http.MethodPost, tc.path, tc.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), "evil:v1", "403 must not leak requested image")
			})
		}
	}

	// Refused UAT callers: missing agent:attach, or bounded to another project.
	t.Run("owner UAT without agent:attach is refused", func(t *testing.T) {
		uat := scopedIdentityFor(agentOwner, project.ID, append(minimalSelectors(t), "agent:lifecycle", "agent:update"))
		rec := doRequestAsIdentity(t, srv, uat, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, Image: "uat-no-attach:v1"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})
	t.Run("owner UAT bounded to another project is refused", func(t *testing.T) {
		uat := scopedIdentityFor(agentOwner, otherProject.ID, append(minimalSelectors(t), "agent:lifecycle", "agent:update", "agent:attach"))
		rec := doRequestAsIdentity(t, srv, uat, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, Image: "uat-other-proj:v1"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	// Refused agent callers: another member's agent in the same project
	// (delegated from projOwner who lacks agent.attach on agent), other-project
	// agent, parent without lifecycle scope, and self with --image.
	for _, tc := range []struct {
		name  string
		ident Identity
	}{
		{"other-member agent with full scopes", agentIdentityFor(otherMemberAgent.ID, project.ID, ScopesForRole(AgentRoleFull)...)},
		{"other-project agent with full scopes", agentIdentityFor(otherProjAgent.ID, otherProject.ID, ScopesForRole(AgentRoleFull)...)},
		{"parent agent without lifecycle scope", agentIdentityFor(parentAgent.ID, project.ID)},
		{"self agent with full scopes", agentIdentityFor(agent.ID, project.ID, ScopesForRole(AgentRoleFull)...)},
	} {
		t.Run("agent/"+tc.name, func(t *testing.T) {
			rec := doRequestAsIdentity(t, srv, tc.ident, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, Image: "agent-denied:v1"})
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
	assertAgentUntouched(t, s, disp, agent.ID, before)

	// Admitted callers: agent owner, owner UAT with attach, explicit-attach
	// non-owner, super-admin, and parent/ancestor agent with lifecycle scope.
	for _, tc := range []struct {
		name  string
		ident Identity
	}{
		{
			name:  "agent owner session",
			ident: NewAuthenticatedUser(agentOwner.ID, agentOwner.Email, agentOwner.DisplayName, agentOwner.Role, "session"),
		},
		{
			name:  "owner UAT with lifecycle, update, and attach",
			ident: scopedIdentityFor(agentOwner, project.ID, append(minimalSelectors(t), "agent:lifecycle", "agent:update", "agent:attach")),
		},
		{
			name:  "non-owner with explicit agent.attach",
			ident: NewAuthenticatedUser(explicitAttachUser.ID, explicitAttachUser.Email, explicitAttachUser.DisplayName, explicitAttachUser.Role, "session"),
		},
		{
			name:  "super-admin",
			ident: NewAuthenticatedUser(superAdmin.ID, superAdmin.Email, superAdmin.DisplayName, superAdmin.Role, "session"),
		},
		{
			name:  "parent ancestor agent with full scopes",
			ident: agentIdentityFor(parentAgent.ID, project.ID, ScopesForRole(AgentRoleFull)...),
		},
	} {
		t.Run("admitted/"+tc.name, func(t *testing.T) {
			rec := doRequestAsIdentity(t, srv, tc.ident, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, Image: "allowed:v2"})
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}
