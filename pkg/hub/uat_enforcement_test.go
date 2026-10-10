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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers for UAT enforcement tests
// ---------------------------------------------------------------------------

// uatTestSetup creates a test server with seeded role definitions, a non-admin
// user, a project, and an authz service. The user has NO role bindings by
// default — callers add only what each test needs.
func uatTestSetup(t *testing.T) (authz *AuthzService, s store.Store, userID, projectID string) {
	t.Helper()
	_, s = testServer(t)
	ctx := context.Background()

	seedRoleDefinitions(ctx, s)

	projectID = tid("uat-project-1")
	project := &store.Project{
		ID:      projectID,
		Name:    "UAT Test Project",
		Slug:    "uat-test-project",
		OwnerID: tid("project-owner"),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	userID = tid("uat-test-user")
	user := &store.User{
		ID:          userID,
		Email:       "uat-user@test.com",
		DisplayName: "UAT User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))

	authz = NewAuthzService(s, nil)
	return authz, s, userID, projectID
}

// grantPermissionViaRoleBinding creates a custom role definition with a single
// permission and binds it to the user at the given scope. Returns the role
// binding ID for cleanup if needed.
func grantPermissionViaRoleBinding(t *testing.T, s store.Store, userID, permissionID, scopeType, scopeID string) string {
	t.Helper()
	ctx := context.Background()

	rdName := "test-role-" + permissionID + "-" + userID
	rd := &store.RoleDefinition{
		Name:        rdName,
		Description: "Test role granting " + permissionID,
		ScopeType:   scopeType,
		Permissions: []string{permissionID},
		System:      false,
	}
	created, err := s.CreateRoleDefinition(ctx, rd)
	require.NoError(t, err, "creating role definition for %s", permissionID)

	rb := &store.RoleBinding{
		RoleDefinitionID: created.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
		CreatedBy:        "test",
	}
	binding, err := s.CreateRoleBinding(ctx, rb)
	require.NoError(t, err, "creating role binding for %s", permissionID)
	return binding.ID
}

// makeScopedIdentity creates a ScopedUserIdentity wrapping a non-admin member user.
// projectBoundaryEligible reports whether permissionID's allowed boundary
// kinds include the project boundary.
func projectBoundaryEligible(permissionID string) bool {
	kinds, _ := permissions.SelectorAllowedBoundaries(permissionID)
	for _, k := range kinds {
		if k == permissions.BoundaryKindProject {
			return true
		}
	}
	return false
}

// makeHubScopedIdentity builds a hub-boundary token identity whose ceiling
// carries exactly the given selectors.
func makeHubScopedIdentity(t *testing.T, userID string, scopes []string) *ScopedUserIdentity {
	t.Helper()
	ceiling, ok := permissions.BuildCeilingFromSelectors(scopes)
	require.True(t, ok, "selectors must resolve: %v", scopes)
	base := NewAuthenticatedUser(userID, "uat-user@test.com", "UAT User", store.UserRoleMember, "api")
	return NewScopedUserIdentityWithBoundaryAndDecoration(base, TokenBoundary{Kind: BoundaryKindHub}, scopes, "", ceiling, nil)
}

func makeScopedIdentity(userID, projectID string, scopes []string) *ScopedUserIdentity {
	base := NewAuthenticatedUser(userID, "uat-user@test.com", "UAT User", store.UserRoleMember, "api")
	return NewScopedUserIdentity(base, projectID, scopes)
}

// ---------------------------------------------------------------------------
// Group 1: Scope Match + Role Binding -> Allowed
// ---------------------------------------------------------------------------

// decideAsUAT builds an AuthzRequest that mirrors how real handlers invoke
// authz: a ScopedUserIdentity (UAT) with the Permission field set so the
// role-binding evaluation path fires.
func decideAsUAT(ctx context.Context, authz *AuthzService, userID, projectID string, scopes []string, resource Resource, action Action, permissionID string) Decision {
	scoped := makeScopedIdentity(userID, projectID, scopes)
	return authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(scoped),
		Resource:   resource,
		Action:     action,
		Permission: permissionID,
	})
}

func TestUATEnforcement_ScopeAndBinding_Allowed(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	tests := []struct {
		name         string
		permissionID string
		uatScope     string
		resource     Resource
		action       Action
	}{
		{
			name:         "skill:read scope + user has skill.read binding",
			permissionID: "skill.read",
			uatScope:     "skill:read",
			resource: skillResource(&store.Skill{
				ID: tid("skill-1"), Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:         "template:read scope + user has template.read binding",
			permissionID: "template.read",
			uatScope:     "template:read",
			resource: templateResource(&store.Template{
				ID: tid("tmpl-1"), Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:         "harness_config:read scope + user has harness_config.read binding",
			permissionID: "harness_config.read",
			uatScope:     "harness_config:read",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: tid("hc-1"), Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:         "skill:create scope + user has skill.create binding",
			permissionID: "skill.create",
			uatScope:     "skill:create",
			resource: skillResource(&store.Skill{
				ID: tid("skill-2"), Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionCreate,
		},
		{
			name:         "group:read scope + user has group.read binding",
			permissionID: "group.read",
			uatScope:     "group:read",
			resource: groupResource(&store.Group{
				ID: tid("group-1"), ProjectID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:         "gcp_service_account:read scope + user has gcp_service_account.read binding",
			permissionID: "gcp_service_account.read",
			uatScope:     "gcp_service_account:read",
			resource: gcpServiceAccountResource(&store.GCPServiceAccount{
				ID: tid("sa-int-1"), Scope: store.ScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Grant the permission via a system-scoped role binding
			grantPermissionViaRoleBinding(t, s, userID, tc.permissionID, store.RoleScopeSystem, "")

			decision := decideAsUAT(ctx, authz, userID, projectID, []string{tc.uatScope}, tc.resource, tc.action, tc.permissionID)
			if !projectBoundaryEligible(tc.permissionID) {
				// The permission is not eligible for a project token; a
				// hub token with the same selector and binding is allowed.
				assert.False(t, decision.Allowed)
				assert.Equal(t, "permission is not eligible for this token boundary", decision.Reason)
				hub := makeHubScopedIdentity(t, userID, []string{tc.uatScope})
				decision = authz.Decide(ctx, AuthzRequest{
					Principal:  principalContextForIdentity(hub),
					Resource:   tc.resource,
					Action:     tc.action,
					Permission: tc.permissionID,
				})
			}
			assert.True(t, decision.Allowed, "expected allowed when UAT scope (%s) AND role binding (%s) both present; reason: %s",
				tc.uatScope, tc.permissionID, decision.Reason)
		})
	}
}

// ---------------------------------------------------------------------------
// Group 2: Scope Match + No Role Binding -> Denied (narrows, never widens)
// ---------------------------------------------------------------------------

func TestUATEnforcement_ScopeWithoutBinding_Denied(t *testing.T) {
	authz, _, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	// User has NO role bindings — the UAT scope alone must NOT grant access.
	// The permission ID is passed so the role-binding path is evaluated (and finds nothing).
	tests := []struct {
		name         string
		permissionID string
		uatScope     string
		resource     Resource
		action       Action
	}{
		{
			name:         "skill:read scope but user has NO skill.read binding",
			permissionID: "skill.read",
			uatScope:     "skill:read",
			resource: skillResource(&store.Skill{
				ID: tid("skill-denied-1"), Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:         "template:create scope but user has NO template.create binding",
			permissionID: "template.create",
			uatScope:     "template:create",
			resource: templateResource(&store.Template{
				ID: tid("tmpl-denied-1"), Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionCreate,
		},
		{
			name:         "group:addMember scope but user has NO group.addMember binding",
			permissionID: "group.addMember",
			uatScope:     "group:addMember",
			resource: groupResource(&store.Group{
				ID: tid("group-denied-1"), ProjectID: projectID,
			}),
			action: ActionAddMember,
		},
		{
			name:         "harness_config:delete scope but user has NO harness_config.delete binding",
			permissionID: "harness_config.delete",
			uatScope:     "harness_config:delete",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: tid("hc-denied-1"), Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionDelete,
		},
		{
			name:         "skill:update scope but user has NO skill.update binding",
			permissionID: "skill.update",
			uatScope:     "skill:update",
			resource: skillResource(&store.Skill{
				ID: tid("skill-denied-2"), Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionUpdate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := decideAsUAT(ctx, authz, userID, projectID, []string{tc.uatScope}, tc.resource, tc.action, tc.permissionID)
			assert.False(t, decision.Allowed, "UAT scope alone must NOT grant access without matching role binding; scope=%s", tc.uatScope)
		})
	}
}

// ---------------------------------------------------------------------------
// Group 3: No Scope Match -> Denied (regardless of bindings)
// ---------------------------------------------------------------------------

func TestUATEnforcement_BindingWithoutScope_Denied(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	t.Run("user has skill.read binding but UAT lacks skill:read scope", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "skill.read", store.RoleScopeSystem, "")

		// UAT has template:read, NOT skill:read
		resource := skillResource(&store.Skill{
			ID: tid("skill-noscope-1"), Scope: store.SkillScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"template:read"}, resource, ActionRead, "skill.read")
		assert.False(t, decision.Allowed, "access must be denied when UAT lacks the required scope")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})

	t.Run("user has template.create binding but UAT has only template:read scope", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "template.create", store.RoleScopeSystem, "")

		// UAT has template:read, NOT template:create
		resource := templateResource(&store.Template{
			ID: tid("tmpl-noscope-1"), Scope: store.TemplateScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"template:read"}, resource, ActionCreate, "template.create")
		assert.False(t, decision.Allowed, "access must be denied when UAT scope does not cover the action")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})

	t.Run("user has group.read binding but UAT has skill:read scope", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "group.read", store.RoleScopeSystem, "")

		resource := groupResource(&store.Group{
			ID: tid("group-noscope-1"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"skill:read"}, resource, ActionRead, "group.read")
		assert.False(t, decision.Allowed, "cross-resource scope must not grant access")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})
}

// ---------------------------------------------------------------------------
// Group 4: Project Constraint Tests
// ---------------------------------------------------------------------------

func TestUATEnforcement_ProjectConstraints(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	// Grant skill.read so the user has the binding.
	grantPermissionViaRoleBinding(t, s, userID, "skill.read", store.RoleScopeSystem, "")

	t.Run("UAT for project X, resource in project X -> allowed", func(t *testing.T) {
		resource := skillResource(&store.Skill{
			ID: tid("skill-projx-1"), Scope: store.SkillScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"skill:read"}, resource, ActionRead, "skill.read")
		assert.True(t, decision.Allowed, "should allow access when project matches; reason: %s", decision.Reason)
	})

	t.Run("UAT for project X, resource in project Y -> denied", func(t *testing.T) {
		otherProjectID := tid("other-project")
		resource := skillResource(&store.Skill{
			ID: tid("skill-projy-1"), Scope: store.SkillScopeProject, ScopeID: otherProjectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"skill:read"}, resource, ActionRead, "skill.read")
		assert.False(t, decision.Allowed, "project mismatch must deny access")
		assert.Contains(t, decision.Reason, "token not scoped for this project")
	})

	t.Run("UAT for project X, hub-level resource -> denied", func(t *testing.T) {
		// User resources are hub-level (no project parent)
		resource := userResource(&store.User{ID: tid("hub-user-1")})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"user:read"}, resource, ActionRead, "user.read")
		assert.False(t, decision.Allowed, "UATs must not access hub-level resources")
		assert.Contains(t, decision.Reason, "token not scoped for hub-level resources")
	})

	t.Run("UAT for project X, project resource matches -> allowed", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "project.read", store.RoleScopeSystem, "")

		resource := projectResource(&store.Project{ID: projectID})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"project:read"}, resource, ActionRead, "project.read")
		assert.True(t, decision.Allowed, "should allow project resource when IDs match; reason: %s", decision.Reason)
	})

	t.Run("UAT for project X, different project resource -> denied", func(t *testing.T) {
		otherProjectID := tid("other-project-2")
		resource := projectResource(&store.Project{ID: otherProjectID})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"project:read"}, resource, ActionRead, "project.read")
		assert.False(t, decision.Allowed, "must deny project resource when IDs don't match")
		assert.Contains(t, decision.Reason, "token not scoped for this project")
	})
}

// ---------------------------------------------------------------------------
// Group 5: Alias and Special Cases
// ---------------------------------------------------------------------------

func TestUATEnforcement_AgentManageAlias(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	// Grant agent permissions so the user has them through bindings.
	for _, perm := range []string{"agent.create", "agent.read", "agent.list", "agent.delete", "agent.attach", "agent.port_access"} {
		grantPermissionViaRoleBinding(t, s, userID, perm, store.RoleScopeSystem, "")
	}

	// In production, agent:manage is expanded by expandScopes() at token creation
	// time into the concrete agent:* scopes. We simulate the same expansion here.
	expandedManageScopes := permissions.UATManageScopes()

	t.Run("expanded agent:manage allows agent:create", func(t *testing.T) {
		resource := agentResource(&store.Agent{
			ID: tid("agent-manage-1"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, expandedManageScopes, resource, ActionCreate, "agent.create")
		assert.True(t, decision.Allowed, "expanded agent:manage should include agent:create; reason: %s", decision.Reason)
	})

	t.Run("expanded agent:manage allows agent:read", func(t *testing.T) {
		resource := agentResource(&store.Agent{
			ID: tid("agent-manage-2"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, expandedManageScopes, resource, ActionRead, "agent.read")
		assert.True(t, decision.Allowed, "expanded agent:manage should include agent:read; reason: %s", decision.Reason)
	})

	t.Run("expanded agent:manage does NOT grant skill:read", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "skill.read", store.RoleScopeSystem, "")
		resource := skillResource(&store.Skill{
			ID: tid("skill-manage-1"), Scope: store.SkillScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, expandedManageScopes, resource, ActionRead, "skill.read")
		assert.False(t, decision.Allowed, "agent:manage must NOT grant access to non-agent resources")
	})

	t.Run("expanded agent:manage does NOT grant template:read", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "template.read", store.RoleScopeSystem, "")
		resource := templateResource(&store.Template{
			ID: tid("tmpl-manage-1"), Scope: store.TemplateScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, expandedManageScopes, resource, ActionRead, "template.read")
		assert.False(t, decision.Allowed, "agent:manage must NOT grant access to template resources")
	})
}

func TestUATEnforcement_NoPolicyScopesExist(t *testing.T) {
	validScopes := permissions.UATValidScopes()

	t.Run("no policy UAT scopes", func(t *testing.T) {
		for scope := range validScopes {
			assert.NotContains(t, scope, "policy:", "ValidUATScopes must not include any policy:* entries, found: %s", scope)
		}
	})

	t.Run("no role UAT scopes", func(t *testing.T) {
		for scope := range validScopes {
			assert.NotContains(t, scope, "role:", "ValidUATScopes must not include any role:* entries, found: %s", scope)
		}
	})

	t.Run("no role_binding UAT scopes", func(t *testing.T) {
		for scope := range validScopes {
			assert.NotContains(t, scope, "role_binding:", "ValidUATScopes must not include any role_binding:* entries, found: %s", scope)
		}
	})
}

func TestUATEnforcement_AgentManageExpansionContents(t *testing.T) {
	expanded := permissions.UATManageScopes()

	t.Run("agent:manage expands to only agent scopes", func(t *testing.T) {
		require.NotEmpty(t, expanded, "agent:manage must expand to at least one scope")
		for _, scope := range expanded {
			assert.True(t, strings.HasPrefix(scope, "agent:"), "agent:manage must only expand to agent:* scopes, got: %s", scope)
		}
	})

	t.Run("agent:manage includes expected concrete scopes", func(t *testing.T) {
		scopeSet := make(map[string]bool)
		for _, s := range expanded {
			scopeSet[s] = true
		}
		assert.True(t, scopeSet["agent:create"], "agent:manage should include agent:create")
		assert.True(t, scopeSet["agent:read"], "agent:manage should include agent:read")
		assert.True(t, scopeSet["agent:list"], "agent:manage should include agent:list")
		assert.True(t, scopeSet["agent:delete"], "agent:manage should include agent:delete")
	})

	t.Run("agent:manage does not include non-agent scopes", func(t *testing.T) {
		scopeSet := make(map[string]bool)
		for _, s := range expanded {
			scopeSet[s] = true
		}
		assert.False(t, scopeSet["skill:read"], "agent:manage must not include skill:read")
		assert.False(t, scopeSet["template:read"], "agent:manage must not include template:read")
		assert.False(t, scopeSet["group:read"], "agent:manage must not include group:read")
	})
}

// ---------------------------------------------------------------------------
// Group 6: Cross-resource Scope Isolation
// ---------------------------------------------------------------------------

func TestUATEnforcement_CrossResourceScopeIsolation(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	// Grant the user all relevant bindings so denials are purely from UAT scope.
	for _, perm := range []string{"skill.read", "template.read", "group.create", "group.delete", "harness_config.update"} {
		grantPermissionViaRoleBinding(t, s, userID, perm, store.RoleScopeSystem, "")
	}

	t.Run("UAT with skill:read cannot access templates", func(t *testing.T) {
		resource := templateResource(&store.Template{
			ID: tid("tmpl-cross-1"), Scope: store.TemplateScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"skill:read"}, resource, ActionRead, "template.read")
		assert.False(t, decision.Allowed, "skill:read scope must not grant template access")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})

	t.Run("UAT with template:read cannot access skills", func(t *testing.T) {
		resource := skillResource(&store.Skill{
			ID: tid("skill-cross-1"), Scope: store.SkillScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"template:read"}, resource, ActionRead, "skill.read")
		assert.False(t, decision.Allowed, "template:read scope must not grant skill access")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})

	t.Run("UAT with group:create cannot access group:delete", func(t *testing.T) {
		resource := groupResource(&store.Group{
			ID: tid("group-cross-1"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"group:create"}, resource, ActionDelete, "group.delete")
		assert.False(t, decision.Allowed, "group:create scope must not grant group:delete access")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})

	t.Run("UAT with harness_config:read cannot access harness_config:update", func(t *testing.T) {
		resource := harnessConfigResource(&store.HarnessConfig{
			ID: tid("hc-cross-1"), Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"harness_config:read"}, resource, ActionUpdate, "harness_config.update")
		assert.False(t, decision.Allowed, "harness_config:read scope must not grant harness_config:update access")
		assert.Contains(t, decision.Reason, "token does not have scope")
	})
}

// ---------------------------------------------------------------------------
// Group 7: Regression — Existing Agent/Project Scopes Still Work
// ---------------------------------------------------------------------------

func TestUATEnforcement_ExistingScopesRegression(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

	t.Run("agent:create scope still works", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "agent.create", store.RoleScopeSystem, "")

		resource := agentResource(&store.Agent{
			ID: tid("agent-regression-1"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"agent:create"}, resource, ActionCreate, "agent.create")
		assert.True(t, decision.Allowed, "existing agent:create scope must still work; reason: %s", decision.Reason)
	})

	t.Run("project:read scope still works", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "project.read", store.RoleScopeSystem, "")

		resource := projectResource(&store.Project{ID: projectID})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"project:read"}, resource, ActionRead, "project.read")
		assert.True(t, decision.Allowed, "existing project:read scope must still work; reason: %s", decision.Reason)
	})

	t.Run("agent:read scope still works", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "agent.read", store.RoleScopeSystem, "")

		resource := agentResource(&store.Agent{
			ID: tid("agent-regression-2"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"agent:read"}, resource, ActionRead, "agent.read")
		assert.True(t, decision.Allowed, "existing agent:read scope must still work; reason: %s", decision.Reason)
	})

	t.Run("agent:delete scope still works", func(t *testing.T) {
		grantPermissionViaRoleBinding(t, s, userID, "agent.delete", store.RoleScopeSystem, "")

		resource := agentResource(&store.Agent{
			ID: tid("agent-regression-3"), ProjectID: projectID,
		})
		decision := decideAsUAT(ctx, authz, userID, projectID, []string{"agent:delete"}, resource, ActionDelete, "agent.delete")
		assert.True(t, decision.Allowed, "existing agent:delete scope must still work; reason: %s", decision.Reason)
	})
}

// ---------------------------------------------------------------------------
// enforceUATConstraints unit tests — direct tests on the constraint gate
// ---------------------------------------------------------------------------

func TestEnforceUATConstraints_NewResourceTypes(t *testing.T) {
	// enforceUATConstraints now also requires live project access
	// (ProjectTargetAdmission, ptone/scion#2092), so this needs a real
	// store-backed AuthzService and genuine project membership for its
	// principal -- previously a bare &AuthzService{} sufficed because the
	// function never touched the store. The membership binding is
	// deliberately permission-agnostic (ProjectMembershipEvidence does not
	// care which permissions a role carries), so every case below continues
	// to test ONLY the scope/project confinement logic this test is named
	// for, not project access itself. (group:read and gcp_service_account:read
	// specifically are exercised for the SEPARATE system-authority-only
	// behavior covered in TestUATProjectAdmission_SystemAuthorityForExactPermission,
	// in uat_project_relationship_test.go.)
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	testUserID := tid("test-constraint-user")
	projectID := tid("test-project-1")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "Test Project 1", Slug: "test-project-1"}))
	createTestUserWithProjectRole(t, s, testUserID, testUserID+"@test.com", projectID, store.ProjectRoleMember)
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: testUserID}

	// All new resource types with their UAT scopes from D1.
	newResources := []struct {
		name     string
		scope    string
		resource Resource
		action   Action
	}{
		{
			name:  "skill:read",
			scope: "skill:read",
			resource: skillResource(&store.Skill{
				ID: "skill-1", Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:  "skill:create",
			scope: "skill:create",
			resource: skillResource(&store.Skill{
				ID: "skill-2", Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionCreate,
		},
		{
			name:  "skill:update",
			scope: "skill:update",
			resource: skillResource(&store.Skill{
				ID: "skill-3", Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionUpdate,
		},
		{
			name:  "skill:delete",
			scope: "skill:delete",
			resource: skillResource(&store.Skill{
				ID: "skill-4", Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionDelete,
		},
		{
			name:  "skill:list",
			scope: "skill:list",
			resource: skillResource(&store.Skill{
				ID: "skill-5", Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionList,
		},
		{
			name:  "skill:register",
			scope: "skill:register",
			resource: skillResource(&store.Skill{
				ID: "skill-6", Scope: store.SkillScopeProject, ScopeID: projectID,
			}),
			action: ActionRegister,
		},
		{
			name:  "template:create",
			scope: "template:create",
			resource: templateResource(&store.Template{
				ID: "tmpl-1", Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionCreate,
		},
		{
			name:  "template:read",
			scope: "template:read",
			resource: templateResource(&store.Template{
				ID: "tmpl-2", Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:  "template:update",
			scope: "template:update",
			resource: templateResource(&store.Template{
				ID: "tmpl-3", Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionUpdate,
		},
		{
			name:  "template:delete",
			scope: "template:delete",
			resource: templateResource(&store.Template{
				ID: "tmpl-4", Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionDelete,
		},
		{
			name:  "template:list",
			scope: "template:list",
			resource: templateResource(&store.Template{
				ID: "tmpl-5", Scope: store.TemplateScopeProject, ScopeID: projectID,
			}),
			action: ActionList,
		},
		{
			name:  "harness_config:create",
			scope: "harness_config:create",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: "hc-1", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionCreate,
		},
		{
			name:  "harness_config:read",
			scope: "harness_config:read",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: "hc-2", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:  "harness_config:update",
			scope: "harness_config:update",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: "hc-3", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionUpdate,
		},
		{
			name:  "harness_config:delete",
			scope: "harness_config:delete",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: "hc-4", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionDelete,
		},
		{
			name:  "harness_config:list",
			scope: "harness_config:list",
			resource: harnessConfigResource(&store.HarnessConfig{
				ID: "hc-5", Scope: store.HarnessConfigScopeProject, ScopeID: projectID,
			}),
			action: ActionList,
		},
		{
			name:  "group:create",
			scope: "group:create",
			resource: groupResource(&store.Group{
				ID: "grp-1", ProjectID: projectID,
			}),
			action: ActionCreate,
		},
		{
			name:  "group:read",
			scope: "group:read",
			resource: groupResource(&store.Group{
				ID: "grp-2", ProjectID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:  "group:update",
			scope: "group:update",
			resource: groupResource(&store.Group{
				ID: "grp-3", ProjectID: projectID,
			}),
			action: ActionUpdate,
		},
		{
			name:  "group:delete",
			scope: "group:delete",
			resource: groupResource(&store.Group{
				ID: "grp-4", ProjectID: projectID,
			}),
			action: ActionDelete,
		},
		{
			name:  "group:list",
			scope: "group:list",
			resource: groupResource(&store.Group{
				ID: "grp-5", ProjectID: projectID,
			}),
			action: ActionList,
		},
		{
			name:  "group:addMember",
			scope: "group:addMember",
			resource: groupResource(&store.Group{
				ID: "grp-6", ProjectID: projectID,
			}),
			action: ActionAddMember,
		},
		{
			name:  "group:removeMember",
			scope: "group:removeMember",
			resource: groupResource(&store.Group{
				ID: "grp-7", ProjectID: projectID,
			}),
			action: ActionRemoveMember,
		},
		// NOTE: broker:read and broker:list are excluded here because brokers
		// are hub-level resources (no project parent) and UATs always deny
		// hub-level resources. See TestEnforceUATConstraints_BrokerHubLevel.

		// gcp_service_account — project-scoped (has project parent)
		{
			name:  "gcp_service_account:read",
			scope: "gcp_service_account:read",
			resource: gcpServiceAccountResource(&store.GCPServiceAccount{
				ID: "sa-1", Scope: store.ScopeProject, ScopeID: projectID,
			}),
			action: ActionRead,
		},
		{
			name:  "gcp_service_account:list",
			scope: "gcp_service_account:list",
			resource: gcpServiceAccountResource(&store.GCPServiceAccount{
				ID: "sa-2", Scope: store.ScopeProject, ScopeID: projectID,
			}),
			action: ActionList,
		},
		{
			name:  "gcp_service_account:verify",
			scope: "gcp_service_account:verify",
			resource: gcpServiceAccountResource(&store.GCPServiceAccount{
				ID: "sa-3", Scope: store.ScopeProject, ScopeID: projectID,
			}),
			action: ActionVerify,
		},
		{
			name:  "gcp_service_account:assign",
			scope: "gcp_service_account:assign",
			resource: gcpServiceAccountResource(&store.GCPServiceAccount{
				ID: "sa-4", Scope: store.ScopeProject, ScopeID: projectID,
			}),
			action: ActionAssign,
		},
		{
			name:     "project:clone",
			scope:    "project:clone",
			resource: projectResource(&store.Project{ID: projectID}),
			action:   ActionClone,
		},
	}

	for _, tc := range newResources {
		t.Run(tc.name+"_scope_present_passes", func(t *testing.T) {
			scoped := makeScopedIdentity(testUserID, projectID, []string{tc.scope})
			permissionID, err := resolveResourcePermission(tc.resource.Type, tc.action)
			require.NoError(t, err)
			result := authz.enforceUATConstraints(ctx, principal, scoped, tc.resource, tc.action, permissionID)
			if !projectBoundaryEligible(permissionID) {
				// The permission is not eligible for this token boundary.
				require.NotNil(t, result, "a project token must not use %s", permissionID)
				assert.False(t, result.Allowed)
				assert.Equal(t, "permission is not eligible for this token boundary", result.Reason)
				return
			}
			assert.Nil(t, result, "enforceUATConstraints should pass (return nil) when scope %s is present", tc.scope)
		})

		t.Run(tc.name+"_scope_absent_denies", func(t *testing.T) {
			scoped := makeScopedIdentity(testUserID, projectID, []string{"unrelated:scope"})
			permissionID, err := resolveResourcePermission(tc.resource.Type, tc.action)
			require.NoError(t, err)
			result := authz.enforceUATConstraints(ctx, principal, scoped, tc.resource, tc.action, permissionID)
			require.NotNil(t, result, "enforceUATConstraints should deny when scope %s is absent", tc.scope)
			assert.False(t, result.Allowed)
			assert.Contains(t, result.Reason, "token does not have scope")
		})
	}
}

// TestEnforceUATConstraints_BrokerHubLevel verifies that broker resources
// (which are hub-level / parentless) are denied for UATs.
func TestEnforceUATConstraints_BrokerHubLevel(t *testing.T) {
	// A bare AuthzService is safe here (no store lookup needed): a broker
	// resource has no project parent, so the hub-level branch denies before
	// the live-project-access check (which would need a real store) is
	// ever reached.
	authz := &AuthzService{}
	ctx := context.Background()
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: "test-constraint-user"}

	scoped := makeScopedIdentity("test-constraint-user", "some-project", []string{"broker:read"})
	resource := brokerResource(&store.RuntimeBroker{ID: "broker-hub-1"})
	result := authz.enforceUATConstraints(ctx, principal, scoped, resource, ActionRead, "broker.read")

	// Broker resources are hub-level (no parent project), so UATs should deny them.
	require.NotNil(t, result, "broker resources are hub-level; UATs should deny them")
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Reason, "token not scoped for hub-level resources")
}

// TestDecide_TypedNilScopedUserIdentityDenied pins that a typed-nil
// *ScopedUserIdentity reaching Decide (for example from a caller that
// forwards a *ScopedUserIdentity-typed variable without checking whether
// ValidateToken returned one) is denied, not a panic. Decide's
// classification check treats a typed-nil identity as a missing principal
// and denies before step 1, so the step-1 nil deny in enforceUATConstraints
// is also exercised directly. A bare AuthzService is safe here: both denies
// are reached before any store lookup.
func TestDecide_TypedNilScopedUserIdentityDenied(t *testing.T) {
	authz := &AuthzService{}
	ctx := context.Background()

	var scoped *ScopedUserIdentity
	req := AuthzRequest{
		Principal: PrincipalContext{Kind: PrincipalKindUser, ID: "test-nil-identity-user", Identity: scoped},
		Resource:  Resource{Type: "agent", ID: "a1", ParentType: "project", ParentID: "p1"},
		Action:    ActionRead,
	}

	var result Decision
	require.NotPanics(t, func() {
		result = authz.Decide(ctx, req)
	}, "a typed-nil *ScopedUserIdentity must be denied, not cause a nil-pointer panic")

	assert.False(t, result.Allowed, "a typed-nil identity must never be treated as unconstrained")
	assert.Equal(t, "missing principal", result.Reason,
		"a typed-nil identity must be denied by Decide's classification check")

	var direct *Decision
	require.NotPanics(t, func() {
		direct = authz.enforceUATConstraints(ctx, req.Principal, scoped, req.Resource, req.Action, "agent.read")
	}, "enforceUATConstraints must deny a typed-nil *ScopedUserIdentity, not panic")
	require.NotNil(t, direct, "a typed-nil identity must be denied by the step-1 UAT gate")
	assert.False(t, direct.Allowed)
	assert.Equal(t, "token holder lacks active access to the target project", direct.Reason)
}

// TestEnforceUATConstraints_UserHubLevel verifies that user resources
// (which are hub-level / parentless) are denied for UATs.
func TestEnforceUATConstraints_UserHubLevel(t *testing.T) {
	// Bare AuthzService is safe here for the same reason as
	// TestEnforceUATConstraints_BrokerHubLevel: a user resource has no
	// project parent, so the hub-level branch denies before any store
	// lookup is needed.
	authz := &AuthzService{}
	ctx := context.Background()
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: "test-constraint-user"}

	scoped := makeScopedIdentity("test-constraint-user", "some-project", []string{"user:read"})
	resource := userResource(&store.User{ID: "user-hub-1"})
	result := authz.enforceUATConstraints(ctx, principal, scoped, resource, ActionRead, "user.read")

	require.NotNil(t, result, "user resources are hub-level; UATs should deny them")
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Reason, "token not scoped for hub-level resources")
}

// TestEnforceUATConstraints_GCPServiceAccountHubLevel verifies that hub-scoped
// gcp_service_account resources (no project parent) are denied for UATs.
// Project-scoped SAs are covered by TestEnforceUATConstraints_NewResourceTypes.
func TestEnforceUATConstraints_GCPServiceAccountHubLevel(t *testing.T) {
	// Bare AuthzService is safe here for the same reason as the broker/user
	// hub-level tests above: a hub-scoped SA has no project parent, so the
	// hub-level branch denies before any store lookup is needed.
	authz := &AuthzService{}
	ctx := context.Background()
	principal := PrincipalContext{Kind: PrincipalKindUser, ID: "test-constraint-user"}

	// Hub-scoped SA has no ParentType/ParentID — gcpServiceAccountResource only
	// sets those for ScopeProject.
	hubSA := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID:    "sa-hub-1",
		Scope: store.ScopeHub,
	})

	for _, action := range []struct {
		name         string
		action       Action
		scope        string
		permissionID string
	}{
		{"read", ActionRead, "gcp_service_account:read", "gcp_service_account.read"},
		{"list", ActionList, "gcp_service_account:list", "gcp_service_account.list"},
		{"verify", ActionVerify, "gcp_service_account:verify", "gcp_service_account.verify"},
		{"assign", ActionAssign, "gcp_service_account:assign", "gcp_service_account.assign"},
	} {
		t.Run(action.name, func(t *testing.T) {
			scoped := makeScopedIdentity("test-constraint-user", "some-project", []string{action.scope})
			result := authz.enforceUATConstraints(ctx, principal, scoped, hubSA, action.action, action.permissionID)

			require.NotNil(t, result, "hub-scoped gcp_service_account must be denied for UATs (action=%s)", action.name)
			assert.False(t, result.Allowed)
			assert.Contains(t, result.Reason, "token not scoped for hub-level resources")
		})
	}
}

// ---------------------------------------------------------------------------
// ValidUATScopes comprehensive validation
// ---------------------------------------------------------------------------

func TestValidUATScopes_Completeness(t *testing.T) {
	validScopes := permissions.UATValidScopes()

	t.Run("includes all D1 resource type scopes", func(t *testing.T) {
		expectedScopes := []string{
			// Skill scopes
			"skill:create", "skill:read", "skill:update", "skill:delete", "skill:list", "skill:register",
			// Template scopes
			"template:create", "template:read", "template:update", "template:delete", "template:list",
			// Harness config scopes
			"harness_config:create", "harness_config:read", "harness_config:update", "harness_config:delete", "harness_config:list",
			// Group scopes
			"group:create", "group:read", "group:update", "group:delete", "group:list", "group:addMember", "group:removeMember",
			// User scopes
			"user:read", "user:invite", "user:list",
			// Broker scopes
			"broker:read", "broker:list",
			// GCP service account scopes
			"gcp_service_account:read", "gcp_service_account:list", "gcp_service_account:verify", "gcp_service_account:assign",
			// Project scopes
			"project:clone",
			// Agent manage alias
			"agent:manage",
		}
		for _, scope := range expectedScopes {
			assert.True(t, validScopes[scope], "ValidUATScopes should include %s", scope)
		}
	})

	t.Run("excludes authority-escalation scopes", func(t *testing.T) {
		excludedPrefixes := []string{"policy:", "role:", "role_binding:", "quota:", "hub:"}
		for scope := range validScopes {
			for _, prefix := range excludedPrefixes {
				assert.False(t, strings.HasPrefix(scope, prefix),
					"ValidUATScopes must not include %s (authority-escalation scope)", scope)
			}
		}
	})
}
