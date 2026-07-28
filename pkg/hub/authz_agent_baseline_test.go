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
