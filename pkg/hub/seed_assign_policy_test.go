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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the per-project service-account assign policy (svc-accnt P0.4,
// arm 2 — the human half). See projectAssignPolicyName in seed.go.
//
// It restores for project members the reach that hub-member-read-all gives
// them today, once the SA-assignment gate moves from ActionRead to
// ActionAssign. Without it that conversion denies every project member who is
// neither the account's creator nor a project owner or admin.

// assignPolicyTestProject creates a project and runs the members-group path
// over it, which is what a real project creation does.
func assignPolicyTestProject(t *testing.T, srv *Server, slug string) *store.Project {
	t.Helper()
	ctx := context.Background()
	p := &store.Project{
		ID:         api.NewUUID(),
		Name:       slug + " project",
		Slug:       slug,
		Visibility: "private",
	}
	require.NoError(t, srv.store.CreateProject(ctx, p))
	srv.createProjectMembersGroupAndPolicy(ctx, p)
	return p
}

func assignPolicyByName(t *testing.T, s store.Store, name string) store.Policy {
	t.Helper()
	res, err := s.ListPolicies(context.Background(),
		store.PolicyFilter{Name: name}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1, "expected exactly one policy named %q", name)
	return res.Items[0]
}

// TestProjectAssignPolicy_Shape pins the policy's breadth and its scope. Both
// are load-bearing: the wildcard is what hub-member-read-all can afford and
// assign cannot, and project scope is what keeps the grant off hub-scoped
// service accounts.
func TestProjectAssignPolicy_Shape(t *testing.T) {
	srv, s := testServer(t)
	project := assignPolicyTestProject(t, srv, "assign-shape")

	p := assignPolicyByName(t, s, projectAssignPolicyName(project.Slug))

	assert.Equal(t, "project", p.ScopeType,
		"hub scope would grant assign on every hub-scoped service account to every member; see projectAssignPolicyName")
	assert.Equal(t, project.ID, p.ScopeID)
	assert.Equal(t, "allow", p.Effect)

	assert.Equal(t, "gcp_service_account", p.ResourceType,
		"the assign policy must name the resource type exactly; a wildcard here would grant assign on every resource type")
	assert.NotEqual(t, "*", p.ResourceType)

	assert.Equal(t, []string{"assign"}, p.Actions,
		"the assign policy must grant assign alone")
	assert.NotContains(t, p.Actions, "*")
}

// TestProjectAssignPolicy_BoundToMembersGroup confirms the policy reaches the
// project's members — the population whose reach it is restoring, and the same
// group the project's agent-create policy is bound to.
func TestProjectAssignPolicy_BoundToMembersGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := assignPolicyTestProject(t, srv, "assign-bound")

	group, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)

	policies, err := s.GetPoliciesForPrincipals(ctx, []store.PrincipalRef{
		{Type: "group", ID: group.ID},
	})
	require.NoError(t, err)

	assignID := assignPolicyByName(t, s, projectAssignPolicyName(project.Slug)).ID
	createID := assignPolicyByName(t, s, "project:"+project.Slug+":member-create-agents").ID

	var foundAssign, foundCreate bool
	for _, p := range policies {
		switch p.ID {
		case assignID:
			foundAssign = true
		case createID:
			foundCreate = true
		}
	}
	assert.True(t, foundCreate,
		"fixture precondition: the agent-create policy is bound to the project members group")
	assert.True(t, foundAssign,
		"the assign policy must be bound to the same group as agent create — that group is the population it restores")
}

// TestProjectAssignPolicy_CannotReachHubScopedSA is the reason this policy is
// project-scoped, expressed as behaviour rather than as a field assertion.
//
// A hub-scoped service account is parentless, so projectIDForResource yields
// "" and matchesResource refuses to match any project-scoped policy against it
// (#595, fail closed rather than fall through). The confinement is structural:
// there is no code-side guard revoking this grant, and none is needed.
func TestProjectAssignPolicy_CannotReachHubScopedSA(t *testing.T) {
	srv, s := testServer(t)
	project := assignPolicyTestProject(t, srv, "assign-reach")
	other := assignPolicyTestProject(t, srv, "assign-reach-other")

	p := assignPolicyByName(t, s, projectAssignPolicyName(project.Slug))

	ownSA := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID: tid("assign-own-sa"), Scope: store.ScopeProject,
		ScopeID: project.ID, Email: "own@example.iam.gserviceaccount.com",
	})
	otherSA := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID: tid("assign-other-sa"), Scope: store.ScopeProject,
		ScopeID: other.ID, Email: "other@example.iam.gserviceaccount.com",
	})
	hubSA := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID: tid("assign-hub-sa"), Scope: store.ScopeHub,
		ScopeID: tid("assign-hub"), Email: "hub@example.iam.gserviceaccount.com",
	})

	assert.True(t, matchesResource(p, ownSA),
		"the policy must reach service accounts in its own project")
	assert.False(t, matchesResource(p, otherSA),
		"the policy must not reach another project's service accounts")
	assert.False(t, matchesResource(p, hubSA),
		"a hub-scoped service account is parentless and must not match a project-scoped policy (#595) — "+
			"this is what keeps Goal 2's cross-project assign out of the policy layer until task #19 is ruled")

	assert.True(t, matchesAction(p, ActionAssign))

	// Other resource types in the same project are out of reach.
	for _, r := range []Resource{
		agentResource(&store.Agent{ID: tid("assign-agent"), ProjectID: project.ID}),
		templateResource(&store.Template{ID: tid("assign-template")}),
		projectResource(project),
	} {
		assert.False(t, matchesResource(p, r),
			"the assign policy must not reach resource type %q", r.Type)
	}

	// Other actions on service accounts are out of reach.
	for _, a := range []Action{ActionDelete, ActionVerify, ActionMint, ActionRead, ActionList} {
		assert.False(t, matchesAction(p, a),
			"the assign policy must not grant action %q", a)
	}
}

// TestProjectAssignPolicy_Idempotent covers the several project touch paths
// that call createProjectMembersGroupAndPolicy on an existing project.
func TestProjectAssignPolicy_Idempotent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := assignPolicyTestProject(t, srv, "assign-idem")

	srv.createProjectMembersGroupAndPolicy(ctx, project)
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	res, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: projectAssignPolicyName(project.Slug)}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount, "repeated project touches must not duplicate the assign policy")
}

// TestBackfillProjectAssignPolicies covers the projects the create path never
// reaches again. Without the backfill their members silently lose assign the
// moment the ActionAssign conversion lands, and the conversion stops being
// reachability-preserving on existing hubs.
func TestBackfillProjectAssignPolicies(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := assignPolicyTestProject(t, srv, "assign-backfill")

	// Simulate a project that predates the policy: group and agent-create
	// policy present, assign policy absent.
	existing := assignPolicyByName(t, s, projectAssignPolicyName(project.Slug))
	require.NoError(t, s.DeletePolicy(ctx, existing.ID))
	res, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: projectAssignPolicyName(project.Slug)}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 0, res.TotalCount, "fixture precondition: assign policy removed")

	backfillProjectAssignPolicies(ctx, s)

	p := assignPolicyByName(t, s, projectAssignPolicyName(project.Slug))
	assert.Equal(t, "project", p.ScopeType)
	assert.Equal(t, project.ID, p.ScopeID)
	assert.Equal(t, []string{"assign"}, p.Actions)

	group, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	policies, err := s.GetPoliciesForPrincipals(ctx, []store.PrincipalRef{{Type: "group", ID: group.ID}})
	require.NoError(t, err)
	var bound bool
	for _, bp := range policies {
		if bp.ID == p.ID {
			bound = true
		}
	}
	assert.True(t, bound, "the backfilled policy must be bound to the project members group, not just created")
}

// TestBackfillProjectAssignPolicies_Idempotent pins that the backfill runs on
// every startup without accumulating policies or bindings.
func TestBackfillProjectAssignPolicies_Idempotent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := assignPolicyTestProject(t, srv, "assign-backfill-idem")

	backfillProjectAssignPolicies(ctx, s)
	backfillProjectAssignPolicies(ctx, s)
	backfillProjectAssignPolicies(ctx, s)

	res, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: projectAssignPolicyName(project.Slug)}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount, "repeated startups must not duplicate the assign policy")
}

// TestBackfillProjectAssignPolicies_SkipsGrouplessProject pins the skip rather
// than leaving it to chance: a project with no members group has nothing to
// bind to, and creating an unbound policy would grant nobody anything while
// consuming the name the create path needs later.
func TestBackfillProjectAssignPolicies_SkipsGrouplessProject(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "groupless project",
		Slug:       "assign-groupless",
		Visibility: "private",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	backfillProjectAssignPolicies(ctx, s)

	res, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: projectAssignPolicyName(project.Slug)}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalCount,
		"a project with no members group must be skipped; the create path makes both together on next touch")
}

// TestBackfillProjectAssignPolicies_MultipleProjects covers the loop itself —
// a hub with several untouched projects must have all of them backfilled, not
// just the first.
func TestBackfillProjectAssignPolicies_MultipleProjects(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	slugs := []string{"assign-multi-a", "assign-multi-b", "assign-multi-c"}
	projects := make([]*store.Project, 0, len(slugs))
	for _, slug := range slugs {
		p := assignPolicyTestProject(t, srv, slug)
		projects = append(projects, p)
		existing := assignPolicyByName(t, s, projectAssignPolicyName(slug))
		require.NoError(t, s.DeletePolicy(ctx, existing.ID))
	}

	backfillProjectAssignPolicies(ctx, s)

	for _, p := range projects {
		got := assignPolicyByName(t, s, projectAssignPolicyName(p.Slug))
		assert.Equal(t, p.ID, got.ScopeID,
			"each project's policy must be scoped to that project, not to whichever was processed first")
	}
}
