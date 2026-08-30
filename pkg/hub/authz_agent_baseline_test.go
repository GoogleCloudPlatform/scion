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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baselineReason is the Decision.Reason emitted by the agent project read
// baseline. Asserted explicitly so operators (and later reviewers) can tell a
// baseline allow from a policy allow.
const baselineReason = "agent project read baseline"

// agentBaselineFixture is the shared world for the baseline tests: two
// projects, an agent in the first, and the implicit project_agents group for
// the first project (so that policy bindings to that group resolve).
type agentBaselineFixture struct {
	authz        *AuthzService
	store        store.Store
	ownProject   *store.Project
	otherProject *store.Project
	agent        *store.Agent
	agentsGroup  *store.Group
	identity     *evaluateAgentIdentity
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

	return &agentBaselineFixture{
		authz:        authz,
		store:        s,
		ownProject:   own,
		otherProject: other,
		agent:        agent,
		agentsGroup:  agentsGroup,
		identity:     &evaluateAgentIdentity{id: agent.ID, projectID: own.ID},
	}
}

// TestAuthz_AgentProjectReadBaseline_Allows covers the legitimate agent traffic
// the baseline exists to keep working: read-class actions on resources in the
// agent's own project.
func TestAuthz_AgentProjectReadBaseline_Allows(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sibling := agentResource(&store.Agent{
		ID: tid("baseline-sibling"), ProjectID: f.ownProject.ID,
	})
	self := agentResource(f.agent)
	ownProject := projectResource(f.ownProject)

	tests := []struct {
		name     string
		resource Resource
		action   Action
	}{
		// Self-read is NOT covered by the ancestry bypass: an agent does not
		// appear in its own ancestry. The baseline is what covers it.
		{"read self", self, ActionRead},
		{"read sibling in same project", sibling, ActionRead},
		{"list in same project", sibling, ActionList},
		{"read own project resource", ownProject, ActionRead},
		{"list own project resource", ownProject, ActionList},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := f.authz.CheckAccess(ctx, f.identity, tt.resource, tt.action)
			assert.True(t, decision.Allowed, "expected allow, got: %s", decision.Reason)
			assert.Equal(t, baselineReason, decision.Reason)
			assert.Equal(t, "project", decision.Scope)
			assert.Empty(t, decision.PolicyID, "baseline allow must not claim a policy")
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_CrossProjectDenied pins project isolation:
// the baseline compares against the agent's own ProjectID token claim.
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
				assert.Equal(t, "default deny", decision.Reason)
			}
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_ReadClassBoundary pins that the baseline is
// read + list only. ActionAttach in particular is excluded deliberately: PTY,
// exec and message mutate a running agent.
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
				"action %s must not be granted by the read baseline", action)
			assert.Equal(t, "default deny", decision.Reason)
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_NoProjectDenied pins the `pid != ""` guard.
// Without it, a parentless resource's "" project would equal an agent's empty
// project and the baseline would allow read on everything in the hub.
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
					"parentless resource must not get the baseline for %s", action)
				assert.Equal(t, "default deny", decision.Reason)
			}
		})
	}
}

// TestAuthz_AgentProjectReadBaseline_EmptyAgentProject is the other half of the
// `pid != ""` guard: an agent identity carrying no project must not match
// parentless resources either.
func TestAuthz_AgentProjectReadBaseline_EmptyAgentProject(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	projectless := &evaluateAgentIdentity{id: f.agent.ID, projectID: ""}
	resource := brokerResource(&store.RuntimeBroker{ID: tid("baseline-broker-empty")})

	decision := f.authz.CheckAccess(ctx, projectless, resource, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "default deny", decision.Reason)
}

// TestAuthz_AgentProjectReadBaseline_RevocableByDenyPolicy is the test that pins
// the design decision on placement. The baseline runs *after* policy
// evaluation, so an explicit deny bound to the project's implicit
// "project:<slug>:agents" group wins over it. If the baseline block is moved
// ahead of evaluatePolicies, this test fails.
func TestAuthz_AgentProjectReadBaseline_RevocableByDenyPolicy(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	denyPolicy := &store.Policy{
		ID:           tid("baseline-deny-policy"),
		Name:         "Deny agent reads in own project",
		ScopeType:    "project",
		ScopeID:      f.ownProject.ID,
		ResourceType: "agent",
		Actions:      []string{"read", "list"},
		Effect:       "deny",
	}
	require.NoError(t, f.store.CreatePolicy(ctx, denyPolicy))
	require.NoError(t, f.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      denyPolicy.ID,
		PrincipalType: "group",
		PrincipalID:   f.agentsGroup.ID,
	}))

	sibling := agentResource(&store.Agent{
		ID: tid("baseline-sibling-revoke"), ProjectID: f.ownProject.ID,
	})

	decision := f.authz.CheckAccess(ctx, f.identity, sibling, ActionRead)
	assert.False(t, decision.Allowed,
		"an explicit deny bound to project:<slug>:agents must override the baseline")
	assert.Equal(t, denyPolicy.ID, decision.PolicyID)

	// The project resource itself is a different resource type, so the deny
	// above does not cover it and the baseline still applies. This confirms the
	// deny above won on its merits and did not merely disable the baseline.
	projectDecision := f.authz.CheckAccess(ctx, f.identity, projectResource(f.ownProject), ActionRead)
	assert.True(t, projectDecision.Allowed)
	assert.Equal(t, baselineReason, projectDecision.Reason)
}

// TestAuthz_AgentProjectReadBaseline_AllowPolicyStillWins ensures the baseline
// does not shadow a matching allow policy: policy evaluation runs first and
// keeps its attribution (PolicyID), which the baseline path does not set.
func TestAuthz_AgentProjectReadBaseline_AllowPolicyStillWins(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	allowPolicy := &store.Policy{
		ID:           tid("baseline-allow-policy"),
		Name:         "Allow agent reads",
		ScopeType:    "hub",
		ResourceType: "agent",
		Actions:      []string{"read"},
		Effect:       "allow",
	}
	require.NoError(t, f.store.CreatePolicy(ctx, allowPolicy))
	require.NoError(t, f.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      allowPolicy.ID,
		PrincipalType: "group",
		PrincipalID:   f.agentsGroup.ID,
	}))

	sibling := agentResource(&store.Agent{
		ID: tid("baseline-sibling-allow"), ProjectID: f.ownProject.ID,
	})

	decision := f.authz.CheckAccess(ctx, f.identity, sibling, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, allowPolicy.ID, decision.PolicyID)
	assert.Equal(t, "policy match", decision.Reason)
}

// TestAuthz_AgentProjectReadBaseline_AncestryUnchanged confirms the ancestor
// bypass is untouched: mutating actions on a descendant still pass, including
// across projects, exactly as before.
func TestAuthz_AgentProjectReadBaseline_AncestryUnchanged(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	descendant := agentResource(&store.Agent{
		ID:        tid("baseline-descendant"),
		ProjectID: f.ownProject.ID,
		Ancestry:  []string{tid("root-user"), f.agent.ID},
	})

	decision := f.authz.CheckAccess(ctx, f.identity, descendant, ActionDelete)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "ancestor access", decision.Reason)
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
	const projectA = "project-a"
	const projectB = "project-b"

	policy := func(resourceType string) store.Policy {
		return store.Policy{
			ResourceType: resourceType,
			ScopeType:    "project",
			ScopeID:      projectA,
			Actions:      []string{"*"},
			Effect:       "allow",
		}
	}

	// Every builder in capabilities.go that can produce a parentless resource.
	// Each of these matched a project-scoped policy before the fix.
	parentless := []struct {
		name     string
		resource Resource
	}{
		{"global template", templateResource(&store.Template{
			ID: "tmpl-global", Scope: store.TemplateScopeGlobal,
		})},
		{"user template", templateResource(&store.Template{
			ID: "tmpl-user", Scope: store.TemplateScopeUser, ScopeID: "user-1",
		})},
		{"project template with empty ScopeID", templateResource(&store.Template{
			ID: "tmpl-noscope", Scope: store.TemplateScopeProject,
		})},
		{"broker", brokerResource(&store.RuntimeBroker{ID: "broker-1"})},
		{"user", userResource(&store.User{ID: "user-1"})},
		{"hub-scoped group", groupResource(&store.Group{ID: "group-1"})},
		{"hub-scoped policy", policyResource(&store.Policy{ID: "policy-1", ScopeType: "hub"})},
		{"global harness config", harnessConfigResource(&store.HarnessConfig{
			ID: "hc-global", Scope: store.HarnessConfigScopeGlobal,
		})},
		{"user harness config", harnessConfigResource(&store.HarnessConfig{
			ID: "hc-user", Scope: store.HarnessConfigScopeUser, ScopeID: "user-1",
		})},
	}

	for _, tt := range parentless {
		t.Run("parentless/"+tt.name, func(t *testing.T) {
			require.Empty(t, projectIDForResource(tt.resource),
				"fixture must be parentless for this case to be meaningful")
			assert.False(t, matchesResource(policy(tt.resource.Type), tt.resource),
				"a project-scoped policy must not reach a parentless resource")
			assert.False(t, matchesResource(policy("*"), tt.resource),
				"nor via a wildcard resourceType")
		})
	}

	// Resources that do resolve to a project behave as before.
	t.Run("same-project child matches", func(t *testing.T) {
		r := agentResource(&store.Agent{ID: "agent-1", ProjectID: projectA})
		assert.True(t, matchesResource(policy("agent"), r))
		assert.True(t, matchesResource(policy("*"), r))
	})

	t.Run("other-project child does not match", func(t *testing.T) {
		r := agentResource(&store.Agent{ID: "agent-2", ProjectID: projectB})
		assert.False(t, matchesResource(policy("agent"), r))
		assert.False(t, matchesResource(policy("*"), r))
	})

	// The project resource itself must still match its own project-scoped
	// policy — this is the behaviour the old code got right only by accident
	// (it matched every project), and it must survive the fix.
	t.Run("the project itself matches", func(t *testing.T) {
		r := projectResource(&store.Project{ID: projectA})
		assert.True(t, matchesResource(policy("project"), r))
	})

	t.Run("a different project does not match", func(t *testing.T) {
		r := projectResource(&store.Project{ID: projectB})
		assert.False(t, matchesResource(policy("project"), r))
	})

	// Project-scoped conditional builders, on the affirmative side.
	t.Run("project-scoped group matches its project", func(t *testing.T) {
		r := groupResource(&store.Group{ID: "group-2", ProjectID: projectA})
		assert.True(t, matchesResource(policy("group"), r))
	})

	t.Run("project-scoped harness config matches its project", func(t *testing.T) {
		r := harnessConfigResource(&store.HarnessConfig{
			ID: "hc-proj", Scope: store.HarnessConfigScopeProject, ScopeID: projectA,
		})
		assert.True(t, matchesResource(policy("harness_config"), r))
	})

	t.Run("project-scoped policy resource matches its project", func(t *testing.T) {
		r := policyResource(&store.Policy{ID: "policy-2", ScopeType: "project", ScopeID: projectA})
		assert.True(t, matchesResource(policy("policy"), r))
	})

	t.Run("project-scoped template matches its project", func(t *testing.T) {
		r := templateResource(&store.Template{
			ID: "tmpl-proj", Scope: store.TemplateScopeProject, ScopeID: projectA,
		})
		assert.True(t, matchesResource(policy("template"), r))
		assert.True(t, matchesResource(policy("*"), r))
	})

	t.Run("project-scoped template does not match another project", func(t *testing.T) {
		r := templateResource(&store.Template{
			ID: "tmpl-proj", Scope: store.TemplateScopeProject, ScopeID: projectA,
		})
		policyForB := store.Policy{
			ResourceType: "template",
			ScopeType:    "project",
			ScopeID:      projectB,
			Actions:      []string{"*"},
			Effect:       "allow",
		}
		assert.False(t, matchesResource(policyForB, r))
	})
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
	policy := store.Policy{
		ResourceType: "*",
		ScopeType:    "project",
		ScopeID:      "",
		Actions:      []string{"*"},
		Effect:       "allow",
	}

	resources := []Resource{
		agentResource(&store.Agent{ID: "agent-1", ProjectID: "project-a"}),
		projectResource(&store.Project{ID: "project-a"}),
		templateResource(&store.Template{ID: "tmpl-1"}),
		brokerResource(&store.RuntimeBroker{ID: "broker-1"}),
		userResource(&store.User{ID: "user-1"}),
		{},
	}

	for _, r := range resources {
		assert.False(t, matchesResource(policy, r),
			"project-scoped policy with empty ScopeID must match nothing (type=%q)", r.Type)
	}
}

// TestMatchesResource_HubAndResourceScopesUnchanged confirms the fix is
// confined to the `case "project"` arm.
func TestMatchesResource_HubAndResourceScopesUnchanged(t *testing.T) {
	parentless := templateResource(&store.Template{ID: "tmpl-1"})
	child := agentResource(&store.Agent{ID: "agent-1", ProjectID: "project-a"})

	hub := store.Policy{ResourceType: "*", ScopeType: "hub"}
	assert.True(t, matchesResource(hub, parentless))
	assert.True(t, matchesResource(hub, child))

	resScope := store.Policy{ResourceType: "*", ScopeType: "resource", ScopeID: "tmpl-1"}
	assert.True(t, matchesResource(resScope, parentless))
	assert.False(t, matchesResource(resScope, child))
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
	srv, s := testServer(t)
	ctx := context.Background()

	seedDefaultPoliciesAndGroups(ctx, s)

	project := &store.Project{
		ID: tid("seed-check-project"), Name: "Seed Check", Slug: "seed-check",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	byName := func(name string) store.Policy {
		t.Helper()
		res, err := s.ListPolicies(ctx, store.PolicyFilter{Name: name}, store.ListOptions{Limit: 1})
		require.NoError(t, err)
		require.Len(t, res.Items, 1, "expected seeded policy %q to exist", name)
		return res.Items[0]
	}

	// Representative resources spanning parentless, own-project and
	// other-project. A hub-scoped policy must match all of them.
	sameProjectAgent := agentResource(&store.Agent{ID: tid("seed-agent"), ProjectID: project.ID})
	otherProjectAgent := agentResource(&store.Agent{ID: tid("seed-agent-other"), ProjectID: tid("seed-other-project")})
	parentlessTemplate := templateResource(&store.Template{ID: tid("seed-template")})
	ownProject := projectResource(project)

	// After narrowing hub-member-read-all, per-type read policies exist for
	// directory resources and hub-member-create-projects remains unchanged.
	for _, name := range []string{"hub-member-read-user", "hub-member-read-group", "hub-member-read-template", "hub-member-read-harness_config", "hub-member-read-broker", "hub-member-read-runtime_broker", "hub-member-read-gcp_service_account", "hub-member-read-policy", "hub-member-read-skill", "hub-member-read-quota", "hub-member-read-role", "hub-member-read-role_binding", "hub-member-read-hub", "hub-member-create-projects"} {
		t.Run(name, func(t *testing.T) {
			p := byName(name)
			require.Equal(t, "hub", p.ScopeType,
				"if this policy ever becomes project-scoped, the blast-radius claim must be re-evaluated")
			// Hub scope short-circuits the scope switch entirely; matching is
			// decided by resource type alone, exactly as before the fix.
			for _, r := range []Resource{sameProjectAgent, otherProjectAgent, parentlessTemplate, ownProject} {
				expected := p.ResourceType == "*" || p.ResourceType == r.Type
				assert.Equal(t, expected, matchesResource(p, r),
					"hub-scoped policy %q vs resource type %q", name, r.Type)
			}
		})
	}

	t.Run("project:<slug>:member-create-agents", func(t *testing.T) {
		p := byName("project:" + project.Slug + ":member-create-agents")
		require.Equal(t, "project", p.ScopeType)
		require.Equal(t, "agent", p.ResourceType,
			"the type check runs before scope matching; if this ever widens, the blast-radius claim must be re-evaluated")
		require.Equal(t, project.ID, p.ScopeID)

		// Agent resources always carry a project parent, so this policy's match
		// set is identical before and after the fix.
		assert.True(t, matchesResource(p, sameProjectAgent))
		assert.False(t, matchesResource(p, otherProjectAgent))
		// Non-agent resources are rejected on type, never reaching scope.
		assert.False(t, matchesResource(p, parentlessTemplate))
		assert.False(t, matchesResource(p, ownProject))
	})
}
