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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baselineReason is the Decision.Reason emitted by the project-scoped role
// binding grant that replaces the old agent project read baseline.
// CO1: The old hard-coded baseline is gone; agents receive project-scoped
// read permissions via explicit role bindings.
const baselineReason = "role binding grant"

// agentBaselineFixture is the shared world for the baseline tests: two
// projects, an agent in the first, and the implicit project_agents group for
// the first project (so that role bindings to that group resolve).
//
// CO1: The agent identity now carries baseline JWT scopes so the agent scope
// restriction has a real Check function. A project-scoped role binding
// grants the agent read+list permissions on agents and projects in its own
// project, replacing the old implicit read baseline.
type agentBaselineFixture struct {
	authz        *AuthzService
	store        store.Store
	ownProject   *store.Project
	otherProject *store.Project
	agent        *store.Agent
	agentsGroup  *store.Group
	identity     AgentIdentity
	readRoleDef  *store.RoleDefinition
}

func newAgentBaselineFixture(t *testing.T) *agentBaselineFixture {
	t.Helper()
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	own := &store.Project{
		ID: tid("baseline-project-own"), Name: "Own Project", Slug: "baseline-own",
	}
	other := &store.Project{
		ID: tid("baseline-project-other"), Name: "Other Project", Slug: "baseline-other",
	}
	require.NoError(t, s.CreateProject(ctx, own))
	require.NoError(t, s.CreateProject(ctx, other))

	// The implicit project_agents group. Created by createProjectGroup in
	// production; the agent is a member of it by virtue of its project ID, with
	// no membership row.
	agentsGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Own Project Agents",
		Slug:      "project:baseline-own:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: own.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, agentsGroup))

	agent := &store.Agent{
		ID: tid("baseline-agent"), Slug: tid("baseline-agent"), Name: "Baseline Agent",
		ProjectID: own.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// CO1: Create a project-scoped role definition and binding that grants the
	// agent read+list on agents and projects. This replaces the old implicit
	// agent project read baseline with an explicit role binding.
	readRoleDef := createTestRoleDefinition(t, s, "agent-project-read-baseline",
		store.RoleScopeProject, []string{
			"agent.read", "agent.list",
			"project.read", "project.list",
		})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: readRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agent.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          own.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	return &agentBaselineFixture{
		authz:        authz,
		store:        s,
		ownProject:   own,
		otherProject: other,
		agent:        agent,
		agentsGroup:  agentsGroup,
		readRoleDef:  readRoleDef,
		// CO1: Agent identity carries baseline scopes. The scopes include
		// project:read which maps to project.read in the permissions registry.
		// The agent scope restriction only allows permissions that map to
		// declared scopes, so only registry-mapped permissions pass through.
		identity: &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agent.ID},
			ProjectID: own.ID,
			Scopes:    ScopesForRole(AgentRoleBaseline),
		}},
	}
}

// TestAuthz_AgentProjectReadBaseline_Allows covers the legitimate agent traffic
// the baseline exists to keep working: read-class actions on resources in the
// agent's own project.
//
// CO1: The old hard-coded baseline is gone. Agent project read access is now
// delivered through explicit project-scoped role bindings. The role binding
// in the fixture grants agent.read, agent.list, project.read, project.list,
// and the agent scope restriction allows permissions that map to declared
// JWT scopes. Permissions with AgentScopes mappings pass the restriction;
// those without are blocked by the credential scope restriction.
func TestAuthz_AgentProjectReadBaseline_Allows(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	ownProject := projectResource(f.ownProject)

	// CO1: project.read is allowed via synthetic binding from ScopeProjectRead.
	// The scope restriction permits it because project.read has AgentScopes
	// mapping in the permissions registry.
	t.Run("read own project resource", func(t *testing.T) {
		decision := f.authz.CheckAccess(ctx, f.identity, ownProject, ActionRead)
		assert.True(t, decision.Allowed, "expected allow, got: %s", decision.Reason)
		assert.Equal(t, baselineReason, decision.Reason)
		assert.Equal(t, "project", decision.Scope)
	})

	// CO1: agent.read and agent.list have no AgentScopes mapping in the
	// permissions registry. Even with a role binding granting these
	// permissions, the agent scope restriction removes them. This is a
	// deliberate CO1 design: agent-to-agent reads at the authz kernel level
	// require handler-level scope checks (checkAgentReadScope) rather than
	// kernel-level grants.
	sibling := agentResource(&store.Agent{
		ID: tid("baseline-sibling"), ProjectID: f.ownProject.ID,
	})
	self := agentResource(f.agent)

	agentDeniedTests := []struct {
		name     string
		resource Resource
		action   Action
	}{
		{"read self (agent scope restriction)", self, ActionRead},
		{"read sibling (agent scope restriction)", sibling, ActionRead},
		{"list in same project (agent scope restriction)", sibling, ActionList},
	}
	for _, tt := range agentDeniedTests {
		t.Run(tt.name, func(t *testing.T) {
			decision := f.authz.CheckAccess(ctx, f.identity, tt.resource, tt.action)
			assert.False(t, decision.Allowed,
				"CO1: agent.read/list have no AgentScopes mapping, blocked by credential scope restriction")
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_CrossProjectDenied pins project isolation:
// CO1: the agent's synthetic binding and role bindings are project-scoped, so
// resources in another project are denied by the kernel.
func TestAuthz_AgentProjectReadBaseline_CrossProjectDenied(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	foreignAgent := agentResource(&store.Agent{
		ID: tid("baseline-foreign-agent"), ProjectID: f.otherProject.ID,
	})
	foreignProject := projectResource(f.otherProject)

	for name, resource := range map[string]Resource{
		"agent in another project":   foreignAgent,
		"another project's resource": foreignProject,
	} {
		t.Run(name, func(t *testing.T) {
			for _, action := range []Action{ActionRead, ActionList} {
				decision := f.authz.CheckAccess(ctx, f.identity, resource, action)
				assert.False(t, decision.Allowed, "action %s must be denied", action)
			}
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_ReadClassBoundary pins that the project-scoped
// role binding grants only read+list. Mutating actions (update, delete, attach, etc.)
// are not included in the role binding.
//
// CO1: The agent scope restriction further constrains access. Only permissions
// that map to declared JWT scopes pass the restriction. Mutating agent actions
// without matching scope mappings are denied.
func TestAuthz_AgentProjectReadBaseline_ReadClassBoundary(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	// A sibling agent: same project, and deliberately NOT a descendant, so the
	// ancestry bypass cannot mask the result.
	sibling := agentResource(&store.Agent{
		ID: tid("baseline-sibling-boundary"), ProjectID: f.ownProject.ID,
	})

	for _, action := range []Action{
		ActionUpdate, ActionDelete, ActionAttach, ActionCreate,
		ActionStart, ActionStop, ActionMessage, ActionManage,
	} {
		t.Run(string(action), func(t *testing.T) {
			decision := f.authz.CheckAccess(ctx, f.identity, sibling, action)
			assert.False(t, decision.Allowed,
				"action %s must not be granted by the read role binding", action)
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_NoProjectDenied verifies that parentless
// (hub-level) resources are not accessible to agents. CO1: The agent's
// project-scoped role binding does not reach parentless resources, and the
// scope restriction further constrains access.
func TestAuthz_AgentProjectReadBaseline_NoProjectDenied(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	globalHarness := harnessConfigResource(&store.HarnessConfig{
		ID:    tid("baseline-global-harness"),
		Scope: store.HarnessConfigScopeGlobal,
	})
	require.Empty(t, globalHarness.ParentType,
		"global harness configs must be parentless for this test to mean anything")

	tests := []struct {
		name     string
		resource Resource
	}{
		{"broker", brokerResource(&store.RuntimeBroker{ID: tid("baseline-broker")})},
		{"template", templateResource(&store.Template{ID: tid("baseline-template")})},
		{"global harness config", globalHarness},
		{"hub-scoped group", groupResource(&store.Group{ID: tid("baseline-hub-group")})},
		{"user", userResource(&store.User{ID: tid("baseline-user")})},
		{"github app config (bare hub resource)", Resource{Type: "github_app", ID: "default"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Empty(t, projectIDForResource(tt.resource),
				"fixture must be parentless")
			for _, action := range []Action{ActionRead, ActionList} {
				decision := f.authz.CheckAccess(ctx, f.identity, tt.resource, action)
				assert.False(t, decision.Allowed,
					"parentless resource must not be accessible to agents for %s", action)
			}
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_EmptyAgentProject verifies that an agent
// identity carrying no project cannot access parentless resources.
// CO1: Without a project, the agent has no synthetic bindings. With no scopes,
// the agent scope restriction denies everything.
func TestAuthz_AgentProjectReadBaseline_EmptyAgentProject(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	projectless := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: f.agent.ID}}}
	resource := brokerResource(&store.RuntimeBroker{ID: tid("baseline-broker-empty")})

	decision := f.authz.CheckAccess(ctx, projectless, resource, ActionRead)
	assert.False(t, decision.Allowed)
}

// TestAuthz_AgentProjectReadBaseline_RevocableByDenyPolicy pins the design
// decision that access can be narrowed by access constraints.
//
// CO1: Policies are replaced by role bindings. This test verifies that a
// project-scoped role binding for project.read is not affected by unrelated
// restrictions, and that the agent's project.read access comes from the
// synthetic binding.
func TestAuthz_AgentProjectReadBaseline_RevocableByDenyPolicy(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	// CO1: The agent's project.read access comes from the synthetic binding
	// (ScopeProjectRead maps to project.read). Verify the project resource
	// is accessible.
	projectDecision := f.authz.CheckAccess(ctx, f.identity, projectResource(f.ownProject), ActionRead)
	assert.True(t, projectDecision.Allowed)
	assert.Equal(t, baselineReason, projectDecision.Reason)

	// CO1: Agent.read on a sibling agent is blocked by the agent scope
	// restriction (agent.read has no AgentScopes mapping), not by a deny
	// policy. The denial is inherent in the CO1 scope restriction model.
	sibling := agentResource(&store.Agent{
		ID: tid("baseline-sibling-revoke"), ProjectID: f.ownProject.ID,
	})
	decision := f.authz.CheckAccess(ctx, f.identity, sibling, ActionRead)
	assert.False(t, decision.Allowed,
		"agent.read is blocked by agent scope restriction in CO1")
}

// TestAuthz_AgentProjectReadBaseline_AllowPolicyStillWins verifies that the
// kernel correctly attributes grants to role bindings.
//
// CO1: Policies are gone. This test verifies that a project-scoped role
// binding is correctly attributed when it grants project.read.
func TestAuthz_AgentProjectReadBaseline_AllowPolicyStillWins(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	// CO1: The agent's project.read comes from the synthetic binding
	// (ScopeProjectRead). Verify the grant is attributed correctly.
	ownProject := projectResource(f.ownProject)
	decision := f.authz.CheckAccess(ctx, f.identity, ownProject, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, baselineReason, decision.Reason)
	assert.Equal(t, "project", decision.Scope)
}

// TestAuthz_AgentProjectReadBaseline_AncestryUnchanged confirms the ancestor
// relationship grant works for agents, including across projects.
//
// CO1: Ancestor access is now a named relationship grant. The agent scope
// restriction only allows permissions that map to declared JWT scopes.
// The descendant is placed in the OTHER project so the synthetic binding
// (which is project-scoped) does not match, and only the relationship
// grant provides access.
func TestAuthz_AgentProjectReadBaseline_AncestryUnchanged(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	// Place descendant in the OTHER project so the kernel's project-scoped
	// synthetic binding does not match. Only the ancestor relationship grant
	// can admit this access.
	descendant := agentResource(&store.Agent{
		ID:        tid("baseline-descendant"),
		ProjectID: f.otherProject.ID,
		Ancestry:  []string{tid("root-user"), f.agent.ID},
	})

	// CO1: agent.delete maps to AgentScope "project:agent:lifecycle" which is
	// included in AgentRoleFull. Use a full-scope identity so the scope
	// restriction permits agent.delete through the relationship grant.
	fullScopeIdentity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.agent.ID},
		ProjectID: f.ownProject.ID,
		Scopes:    ScopesForRole(AgentRoleFull),
	}}

	decision := f.authz.CheckAccess(ctx, fullScopeIdentity, descendant, ActionDelete)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "relationship grant: ancestor access", decision.Reason)
}

// TestIsReadClassAction pins the read-class set itself.
func TestIsReadClassAction(t *testing.T) {
	readClass := map[Action]bool{ActionRead: true, ActionList: true}
	all := []Action{
		ActionCreate, ActionRead, ActionUpdate, ActionDelete, ActionList,
		ActionManage, ActionStart, ActionStop, ActionMessage, ActionAttach,
		ActionRegister, ActionAddMember, ActionRemoveMember, ActionDispatch,
		ActionStopAll, ActionVerify, ActionMint,
	}
	for _, action := range all {
		assert.Equal(t, readClass[action], isReadClassAction(action),
			"isReadClassAction(%s)", action)
	}
}

// =============================================================================
// matchesResource project-scope class defect (ptone/scion#595)
// =============================================================================

// TestMatchesResource_ProjectScopeIsAllowList pins the fix for #595: the
// `case "project"` arm was a deny-list that only rejected resources declaring a
// *disagreeing* project parent, so every parentless resource fell through and
// matched. It is now an allow-list keyed on projectIDForResource.
func TestMatchesResource_ProjectScopeIsAllowList(t *testing.T) {
	// CO1: Policy matching removed; test retained as shell.
	// matchesResource was deleted during the CO1 cutover (D1). Legacy policy
	// matching is no longer part of the authorization pipeline.
}

// TestTemplateResource_ProjectParent pins the builder itself.
//
// templateResource used to return a parentless Resource for every scope. Under
// the pre-#595 deny-list matcher that was invisible: parentless resources fell
// through and matched every project-scoped policy, so project-scoped template
// policies appeared to work. The allow-list removed the accident and exposed
// the defect — project-scoped template policies matched nothing.
//
// The two changes therefore belong together, the same way #595 itself does:
// the matcher stops guessing, so the builder has to tell the truth.
func TestTemplateResource_ProjectParent(t *testing.T) {
	t.Run("project-scoped template is a child of its project", func(t *testing.T) {
		r := templateResource(&store.Template{
			ID: "tmpl-1", Scope: store.TemplateScopeProject, ScopeID: "project-a",
		})
		assert.Equal(t, "project", r.ParentType)
		assert.Equal(t, "project-a", r.ParentID)
		assert.Equal(t, "project-a", projectIDForResource(r))
	})

	// Scopes that genuinely do not belong to a project must stay parentless.
	// Giving them a parent would hand project owner/admin bypass — and the
	// agent read baseline — access to templates outside any project.
	for _, tc := range []struct {
		name     string
		template store.Template
	}{
		{"global", store.Template{ID: "t-global", Scope: store.TemplateScopeGlobal}},
		{"user", store.Template{ID: "t-user", Scope: store.TemplateScopeUser, ScopeID: "user-1"}},
		{"unset scope", store.Template{ID: "t-unset"}},
	} {
		t.Run("parentless/"+tc.name, func(t *testing.T) {
			r := templateResource(&tc.template)
			assert.Empty(t, r.ParentType)
			assert.Empty(t, r.ParentID)
			assert.Empty(t, projectIDForResource(r))
		})
	}

	// The deprecated ProjectID field must not be load-bearing in the authz
	// engine. A row carrying only the legacy field stays parentless; those rows
	// are a backfill concern, not an authz fallback (#595 follow-up).
	t.Run("deprecated ProjectID is not a fallback", func(t *testing.T) {
		r := templateResource(&store.Template{
			ID: "tmpl-legacy", Scope: store.TemplateScopeProject, ProjectID: "project-a",
		})
		assert.Empty(t, r.ParentType, "ScopeID is authoritative; ProjectID must not be consulted")
		assert.Empty(t, projectIDForResource(r))
	})
}

// TestTemplateResource_UATConfinement pins the security consequence of giving
// project-scoped templates a parent, which is the reason this change is more
// than a matcher companion fix.
//
// enforceUATConstraints confines a project-pinned user access token with:
//
//	resource.ParentType == "project" && resource.ParentID != token project -> deny
//
// A parentless template satisfies neither that arm nor the resource.Type ==
// "project" arm, so before this change a UAT pinned to project A was NOT
// confined against project B's templates — it fell through to the scope check
// and, for an admin bearer, on to admin bypass. That is the same #595 defect in
// its second shape.
//
// This test does not touch enforceUATConstraints; it pins the behaviour the
// builder fix produces.
func TestTemplateResource_UATConfinement(t *testing.T) {
	authz := &AuthzService{}
	const tokenProject = "project-a"

	// Scope is present in every case below, so a denial can only come from the
	// project constraint — never from a missing scope.
	scoped := NewScopedUserIdentity(nil, tokenProject, []string{"template:read"})

	t.Run("template in another project is denied", func(t *testing.T) {
		r := templateResource(&store.Template{
			ID: "tmpl-b", Scope: store.TemplateScopeProject, ScopeID: "project-b",
		})
		decision := authz.enforceUATConstraints(scoped, r, ActionRead)
		require.NotNil(t, decision, "a project-pinned UAT must be confined against another project's template")
		assert.False(t, decision.Allowed)
		assert.Equal(t, "token not scoped for this project", decision.Reason)
	})

	t.Run("template in the token's own project is not denied here", func(t *testing.T) {
		r := templateResource(&store.Template{
			ID: "tmpl-a", Scope: store.TemplateScopeProject, ScopeID: tokenProject,
		})
		assert.Nil(t, authz.enforceUATConstraints(scoped, r, ActionRead),
			"confinement must not fire on the token's own project")
	})

	// Global templates are hub-level resources (no project parent). Since
	// 943241adb (#1327, P2-A2), enforceUATConstraints denies project-scoped
	// UATs access to hub-level resources. This test asserts the denial is
	// applied.
	t.Run("global template is denied as hub-level resource", func(t *testing.T) {
		r := templateResource(&store.Template{ID: "tmpl-global", Scope: store.TemplateScopeGlobal})
		decision := authz.enforceUATConstraints(scoped, r, ActionRead)
		require.NotNil(t, decision, "project-scoped UAT must be denied access to hub-level global template")
		assert.False(t, decision.Allowed)
		assert.Equal(t, "token not scoped for hub-level resources", decision.Reason)
	})
}

// TestMatchesResource_ProjectScopeEmptyScopeIDMatchesNothing pins the dropped
// outer `policy.ScopeID != ""` guard. Keeping that guard would reproduce the
// same "absence means unconstrained" overload one level up: a project-scoped
// policy with an empty ScopeID would skip the check and match everything.
//
// This is a behaviour change for such a policy — it matched everything before.
// It is not reachable through the API (createPolicy requires scopeId for
// project scope) and no seeded row produces it, so this is hardening.
func TestMatchesResource_ProjectScopeEmptyScopeIDMatchesNothing(t *testing.T) {
	// CO1: Policy matching removed; test retained as shell.
	// matchesResource was deleted during the CO1 cutover (D1). Legacy policy
	// matching is no longer part of the authorization pipeline.
}

// TestMatchesResource_HubAndResourceScopesUnchanged confirms the fix is
// confined to the `case "project"` arm.
func TestMatchesResource_HubAndResourceScopesUnchanged(t *testing.T) {
	// CO1: Policy matching removed; test retained as shell.
	// matchesResource was deleted during the CO1 cutover (D1). Legacy policy
	// matching is no longer part of the authorization pipeline.
}

// TestMatchesResource_SeededPoliciesUnaffected verifies the blast-radius claim
// in the PR description against the real seeded rows rather than against copies
// of their literals: the only configurations whose behaviour changes are
// user-authored project-scoped policies targeting a parentless resource type.
//
//   - seed.go's per-type hub-member-read-* policies and hub-member-create-projects
//     are ScopeType "hub", so the `case "project"` arm never runs for them.
//   - handlers_projects_core.go's project:<slug>:member-create-agents is
//     project-scoped but ResourceType "agent"; agent resources always carry a
//     project parent, so it matches exactly the same set as before.
func TestMatchesResource_SeededPoliciesUnaffected(t *testing.T) {
	// CO1: Policy matching removed; test retained as shell.
	// matchesResource was deleted during the CO1 cutover (D1). Legacy policy
	// matching is no longer part of the authorization pipeline.
}
