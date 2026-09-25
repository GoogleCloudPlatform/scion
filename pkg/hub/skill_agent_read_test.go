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

// ptone/scion#1968: agents read skills. The granted set for an agent A in
// project P, created by user U, is
//
//	G(A) = {global, core skills} ∪ {project skills of P}
//
// gated by the agent JWT carrying project:read and by the delegation ceiling
// (U must still hold skill.read). U's own user-scoped skills, other users'
// user-scoped skills and other projects' skills are never in G(A).
//
// Row labels (A1, A2, ...) refer to the phase-1 test matrix.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type agentSkillFixture struct {
	srv  *Server
	s    store.Store
	u    *store.User // creator: hub member, project-member of P and Q
	v    *store.User // another hub member
	bob  *store.User // not a hub member, no project roles
	p, q *store.Project

	sg, sc, sp, sq, su, sv *store.Skill
}

func (f *agentSkillFixture) all() []*store.Skill {
	return []*store.Skill{f.sg, f.sc, f.sp, f.sq, f.su, f.sv}
}

func setupAgentSkillFixture(t *testing.T) *agentSkillFixture {
	t.Helper()
	srv, s, alice, bob, p := setupSkillAuthzTest(t)
	ctx := context.Background()

	u := createNamedTestUser(t, s, "agentskill-u", store.UserRoleMember)
	ensureHubMembership(ctx, s, u.ID)
	v := createNamedTestUser(t, s, "agentskill-v", store.UserRoleMember)
	ensureHubMembership(ctx, s, v.ID)

	q := &store.Project{
		ID: tid("agentskill-project-q"), Name: "Agent Skill Q", Slug: "agent-skill-q",
		OwnerID: alice.ID, CreatedBy: alice.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, q))
	srv.createProjectMembersGroup(ctx, q)

	createTestUserWithProjectRole(t, s, u.ID, u.Email, p.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, u.ID, u.Email, q.ID, store.ProjectRoleMember)

	// The delegation ceiling must be live (post-backfill), so that agent
	// reads are checked against a real delegation edge rather than the
	// pre-backfill temporary allow.
	_, err := s.UpsertHubSetting(ctx, "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)

	// Skills are owned by alice (not U) so the resource-owner relationship
	// grant never confounds U's results, except Su which is U's own.
	f := &agentSkillFixture{srv: srv, s: s, u: u, v: v, bob: bob, p: p, q: q}
	f.sg = createTestSkill(t, s, "as-global", store.SkillScopeGlobal, "", alice.ID)
	f.sc = createTestSkill(t, s, "as-core", store.SkillScopeCore, "", alice.ID)
	f.sp = createTestSkill(t, s, "as-project-p", store.SkillScopeProject, p.ID, alice.ID)
	f.sq = createTestSkill(t, s, "as-project-q", store.SkillScopeProject, q.ID, alice.ID)
	f.su = createTestSkill(t, s, "as-user-u", store.SkillScopeUser, u.ID, u.ID)
	f.sv = createTestSkill(t, s, "as-user-v", store.SkillScopeUser, v.ID, v.ID)
	return f
}

// newAgent creates agent name in projectID, delegated by delegatorID (edge at
// project scope), and returns an agent token carrying scopes.
func (f *agentSkillFixture) newAgent(t *testing.T, name, projectID, delegatorID string, scopes []AgentTokenScope) string {
	t.Helper()
	ctx := context.Background()
	agent := &store.Agent{
		ID: tid(name), Slug: tid(name), Name: name,
		ProjectID: projectID, Phase: string(state.PhaseRunning),
		CreatedBy: delegatorID, OwnerID: delegatorID, Ancestry: []string{delegatorID},
	}
	require.NoError(t, f.s.CreateAgent(ctx, agent))
	require.NoError(t, f.s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: agent.ID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
		Role: string(AgentRoleBaseline), Active: true,
	}))
	token, err := f.srv.GetAgentTokenService().GenerateAgentToken(agent.ID, projectID, scopes, []string{delegatorID})
	require.NoError(t, err)
	return token
}

func (f *agentSkillFixture) agentGet(t *testing.T, token, path string) (int, string) {
	t.Helper()
	rec := doAgentTokenRequestSkills(t, f.srv, path, token)
	return rec.Code, rec.Body.String()
}

// agentListAll walks every page of GET /skills for token with the given
// extra query and page size, checking the per-page count invariants, and
// returns the IDs seen and the (stable) totalCount.
func (f *agentSkillFixture) agentListAll(t *testing.T, token, extra string, limit int) (map[string]bool, int) {
	t.Helper()
	seen := map[string]bool{}
	total := -1
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 5000, "runaway pagination")
		path := fmt.Sprintf("/api/v1/skills?status=active&limit=%d%s", limit, extra)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := doAgentTokenRequestSkills(t, f.srv, path, token)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		page := decodeSkillsPageFromRecorder(t, rec)
		if total < 0 {
			total = page.TotalCount
		}
		require.Equal(t, total, page.TotalCount, "totalCount must be stable across pages")
		if page.NextCursor != "" {
			// A page that advertises more must be full.
			require.Len(t, page.Skills, limit, "a non-final page must be full; path %s", path)
		}
		for _, sk := range page.Skills {
			require.False(t, seen[sk.ID], "skill %s returned twice", sk.ID)
			seen[sk.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Equal(t, total, len(seen), "totalCount must equal the number of rows actually returned")
	return seen, total
}

func agentSkillSet(skills ...*store.Skill) map[string]bool {
	m := map[string]bool{}
	for _, s := range skills {
		m[s.ID] = true
	}
	return m
}

func agentSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertAgentSees checks, for one agent token, that GET /skills/{id} returns
// 200 exactly for want, a consistent 404 for everything else, and that LIST
// (walked with limit=1) returns exactly want: the list↔point-read
// consistency property on the fixture set.
func (f *agentSkillFixture) assertAgentSees(t *testing.T, token string, want map[string]bool) {
	t.Helper()
	_, missingBody := f.agentGet(t, token, "/api/v1/skills/"+api.NewUUID())
	pointOK := map[string]bool{}
	for _, sk := range f.all() {
		code, body := f.agentGet(t, token, "/api/v1/skills/"+sk.ID)
		if want[sk.ID] {
			assert.Equal(t, http.StatusOK, code, "%s (%s) should be readable: %s", sk.Name, sk.Scope, body)
		} else {
			assert.Equal(t, http.StatusNotFound, code, "%s (%s) must be a 404", sk.Name, sk.Scope)
			assert.Equal(t, missingBody, body, "%s (%s): not-found response must be consistent", sk.Name, sk.Scope)
		}
		if code == http.StatusOK {
			pointOK[sk.ID] = true
		}
	}
	listed, total := f.agentListAll(t, token, "", 1)
	assert.Equal(t, agentSortedKeys(want), agentSortedKeys(listed), "LIST must return exactly the granted set")
	assert.Equal(t, len(want), total)
	assert.Equal(t, agentSortedKeys(pointOK), agentSortedKeys(listed), "in LIST ⇔ GET 200")
}

// A1/A2: baseline and read-only agents read the hub catalog and their own
// project's skills — nothing else.
func TestAgentSkillRead_BaselineAndReadOnlySeeGrantedSet(t *testing.T) {
	for _, role := range []AgentRole{AgentRoleBaseline, AgentRoleReadOnly, AgentRoleFull} {
		t.Run(string(role), func(t *testing.T) {
			f := setupAgentSkillFixture(t)
			token := f.newAgent(t, "as-agent-"+string(role), f.p.ID, f.u.ID, ScopesForRole(role))
			f.assertAgentSees(t, token, agentSkillSet(f.sg, f.sc, f.sp))
		})
	}
}

// A3: an agent token without project:read reads nothing.
func TestAgentSkillRead_NoProjectReadScopeSeesNothing(t *testing.T) {
	f := setupAgentSkillFixture(t)
	token := f.newAgent(t, "as-agent-noscope", f.p.ID, f.u.ID, []AgentTokenScope{ScopeAgentStatusUpdate})
	// GET is gated by checkAgentReadScope (403) before any lookup, for
	// existing and nonexistent IDs alike.
	_, missingBody := f.agentGet(t, token, "/api/v1/skills/"+api.NewUUID())
	for _, sk := range f.all() {
		code, body := f.agentGet(t, token, "/api/v1/skills/"+sk.ID)
		assert.NotEqual(t, http.StatusOK, code, sk.Name)
		assert.Equal(t, missingBody, body, "%s: response must not depend on existence", sk.Name)
	}
	rec := doAgentTokenRequestSkills(t, f.srv, "/api/v1/skills?status=active", token)
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.Less(t, rec.Code, 500)
}

// A4: when the creator no longer holds skill.read (not a hub member, no
// project role), the delegation ceiling denies every skill, and LIST agrees.
func TestAgentSkillRead_CeilingDeniedSeesNothing(t *testing.T) {
	f := setupAgentSkillFixture(t)
	token := f.newAgent(t, "as-agent-ceiling", f.p.ID, f.bob.ID, ScopesForRole(AgentRoleBaseline))
	f.assertAgentSees(t, token, agentSkillSet())
}

// A7: an access constraint capping the agent below skill.read denies every
// skill; LIST agrees (the probe sees the same restriction).
func TestAgentSkillRead_AccessConstraintDeniesAll(t *testing.T) {
	f := setupAgentSkillFixture(t)
	token := f.newAgent(t, "as-agent-ac", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))
	agentType, agentID := "agent", tid("as-agent-ac")
	_, err := f.s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		ID: api.NewUUID(), Name: "cap-agent", SubjectKind: "principal",
		SubjectPrincipalType: &agentType, SubjectPrincipalID: &agentID,
		ScopeType: store.RoleScopeSystem, MaximumPermissions: []string{"project.read"},
		Purpose: "test", CreatedBy: "test",
	})
	require.NoError(t, err)
	f.assertAgentSees(t, token, agentSkillSet())
}

// Core/global coupling: listSkills uses one probe for both
// global and core, so no constraint may split them. Access constraints are
// keyed on system vs project scope, and both hub scopes resolve to system:
// a project-scoped constraint on P removes Sp but leaves Sg and Sc together.
func TestAgentSkillRead_CoreAndGlobalCannotDiverge(t *testing.T) {
	f := setupAgentSkillFixture(t)
	token := f.newAgent(t, "as-agent-ac-proj", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))
	agentType, agentID := "agent", tid("as-agent-ac-proj")
	_, err := f.s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		ID: api.NewUUID(), Name: "cap-agent-in-p", SubjectKind: "principal",
		SubjectPrincipalType: &agentType, SubjectPrincipalID: &agentID,
		ScopeType: store.RoleScopeProject, ScopeID: f.p.ID, MaximumPermissions: []string{"project.read"},
		Purpose: "test", CreatedBy: "test",
	})
	require.NoError(t, err)
	f.assertAgentSees(t, token, agentSkillSet(f.sg, f.sc))

	// And at the Decide level: for every agent condition in this file,
	// global and core decisions are identical.
	authz := f.srv.authzService
	ctx := context.Background()
	for _, ident := range []AgentIdentity{
		dcAgentIdentity(tid("as-agent-ac-proj"), f.p.ID, AgentRoleBaseline),
		dcAgentIdentity(tid("as-agent-ac-proj"), f.p.ID, AgentRoleNone),
		dcAgentIdentity(tid("as-agent-ac-proj"), "", AgentRoleBaseline),
	} {
		g := authz.CheckAccess(ctx, ident, skillScopeResource(store.SkillScopeGlobal, ""), ActionRead).Allowed
		c := authz.CheckAccess(ctx, ident, skillScopeResource(store.SkillScopeCore, ""), ActionRead).Allowed
		assert.Equal(t, g, c, "global and core must decide identically for an agent")
	}
}

// A11: filter parameters and cursors cannot widen an agent's view.
func TestAgentSkillRead_FilterParamsCannotWiden(t *testing.T) {
	f := setupAgentSkillFixture(t)
	token := f.newAgent(t, "as-agent-filters", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))
	granted := agentSkillSet(f.sg, f.sc, f.sp)
	for _, extra := range []string{
		"&scopeId=" + f.q.ID,
		"&scope=project&scopeId=" + f.q.ID,
		"&scope=user",
		"&scope=user&scopeId=" + f.u.ID,
		"&ownerId=" + f.u.ID,
		"&search=as-project-q",
		"&search=as-user",
		"&name=as-user-u",
	} {
		seen, _ := f.agentListAll(t, token, extra, 2)
		for id := range seen {
			assert.True(t, granted[id], "query %q returned out-of-set skill %s", extra, id)
		}
	}
	rec := doAgentTokenRequestSkills(t, f.srv, "/api/v1/skills?status=active&cursor=forged-cursor", token)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// A17: a project-less agent never gets a project bucket, and its predicate is
// never nil (unfiltered). Its granted set degenerates to the hub catalog:
// the delegation ceiling walks with an empty scope (it logs "no project
// scope" but does not itself deny), and post-backfill reads by a
// hub-attested agent with no edge are allowed. Pinned here, together with
// probe ⇔ per-row agreement on every fixture skill.
func TestAgentSkillRead_ProjectlessAgent(t *testing.T) {
	f := setupAgentSkillFixture(t)
	ctx := context.Background()
	ident := dcAgentIdentity(tid("as-agent-projectless"), "", AgentRoleBaseline)
	scope := f.srv.agentSkillAccessScope(ctx, ident)
	require.NotNil(t, scope, "an agent predicate must never be nil (unfiltered)")
	assert.Empty(t, scope.ProjectIDs)
	assert.Empty(t, scope.CallerID)
	assert.True(t, scope.IncludeHubScope, "project-less agent: G = hub catalog (design A17)")
	for _, sk := range f.all() {
		inPredicate := (scope.IncludeHubScope && (sk.Scope == store.SkillScopeGlobal || sk.Scope == store.SkillScopeCore)) ||
			(sk.Scope == store.SkillScopeProject && len(scope.ProjectIDs) == 1 && sk.ScopeID == scope.ProjectIDs[0])
		allowed := f.srv.authzService.CheckAccess(ctx, ident, skillResource(sk), ActionRead).Allowed
		assert.Equal(t, inPredicate, allowed, "probe and per-row decision must agree for %s (%s)", sk.Name, sk.Scope)
	}
}

// A15: the synthetic catalog grant applies to no
// non-hub scope kind, including empty and unknown values.
func TestAgentSkillCatalogBinding_FilteredOutsideHubScope(t *testing.T) {
	ident := dcAgentIdentity(tid("as-agent-a15"), tid("as-a15-project"), AgentRoleBaseline)
	cb, role := agentSkillCatalogBinding(ident)
	roleDefs := map[string]*RolePermissions{cb.RoleDefinitionID: role}
	for _, sc := range []string{store.SkillScopeGlobal, store.SkillScopeCore} {
		assert.Len(t, filterHubWideSkillGrants([]CandidateBinding{cb}, roleDefs, sc), 1, sc)
	}
	for _, sc := range []string{store.SkillScopeProject, store.SkillScopeUser, "", "future-scope"} {
		assert.Empty(t, filterHubWideSkillGrants([]CandidateBinding{cb}, roleDefs, sc), "scope %q", sc)
	}
	assert.Equal(t, ScopeTypeSystem, cb.ScopeType)
	assert.ElementsMatch(t, []string{"skill.read", "skill.list"}, agentSortedKeys(func() map[string]bool {
		m := map[string]bool{}
		for p := range role.Permissions {
			m[p] = true
		}
		return m
	}()), "the catalog role must be read-only")
}

// A15 end to end: an agent in P must not read a project skill of another
// project through the system-scoped catalog binding, nor a skill whose
// Resource carries no scope kind.
func TestAgentSkillRead_CatalogGrantDoesNotReachNonHubResources(t *testing.T) {
	f := setupAgentSkillFixture(t)
	ident := dcAgentIdentity(tid("as-agent-a15e"), f.p.ID, AgentRoleBaseline)
	ctx := context.Background()
	authz := f.srv.authzService
	assert.False(t, authz.CheckAccess(ctx, ident, Resource{Type: "skill", ID: f.sg.ID}, ActionRead).Allowed,
		"a skill Resource with no ScopeKind must be denied")
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sq), ActionRead).Allowed)
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.su), ActionRead).Allowed)
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sq), ActionUpdate).Allowed)
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sg), ActionUpdate).Allowed,
		"the catalog grant is read-only")
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sg), ActionDelete).Allowed)
}

// A8/A9 regressions: the agent grant does not change what users see.
func TestAgentSkillRead_UserAndAdminUnchanged(t *testing.T) {
	f := setupAgentSkillFixture(t)
	resp := decodeSkillsPage(t, f.srv, f.u, "/api/v1/skills?status=active&limit=100")
	got := map[string]bool{}
	for _, sk := range resp.Skills {
		got[sk.ID] = true
	}
	assert.Equal(t, agentSortedKeys(agentSkillSet(f.sg, f.sc, f.sp, f.sq, f.su)), agentSortedKeys(got))
	assert.Equal(t, 5, resp.TotalCount)

	admin := createSkillScopeAdmin(t, f.s, "agentskill-admin")
	resp = decodeSkillsPage(t, f.srv, admin, "/api/v1/skills?status=active&limit=100")
	assert.Equal(t, 6, resp.TotalCount)
}

// A12 at scale: >1000 hub skills plus out-of-scope noise,
// walked at limit=1; count and page invariants hold on every page and LIST
// equals the point-read set row for row.
func TestAgentSkillRead_ConsistencyAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("large seed")
	}
	f := setupAgentSkillFixture(t)
	ctx := context.Background()
	const hubN = 1005
	want := agentSkillSet(f.sg, f.sc, f.sp)
	for i := 0; i < hubN; i++ {
		want[createTestSkill(t, f.s, fmt.Sprintf("as-scale-global-%04d", i), store.SkillScopeGlobal, "", f.v.ID).ID] = true
	}
	for i := 0; i < 50; i++ {
		createTestSkill(t, f.s, fmt.Sprintf("as-scale-q-%02d", i), store.SkillScopeProject, f.q.ID, f.v.ID)
		createTestSkill(t, f.s, fmt.Sprintf("as-scale-u-%02d", i), store.SkillScopeUser, f.u.ID, f.u.ID)
	}
	token := f.newAgent(t, "as-agent-scale", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))

	listed, total := f.agentListAll(t, token, "", 1)
	assert.Equal(t, len(want), total)
	assert.Equal(t, agentSortedKeys(want), agentSortedKeys(listed))

	// Row-for-row point reads over every seeded skill.
	all, err := f.s.ListSkills(ctx, store.SkillFilter{Status: "active"}, store.ListOptions{Limit: 5000})
	require.NoError(t, err)
	for _, sk := range all.Items {
		code, _ := f.agentGet(t, token, "/api/v1/skills/"+sk.ID)
		assert.Equal(t, listed[sk.ID], code == http.StatusOK, "in LIST ⇔ GET 200 for %s (%s)", sk.Name, sk.Scope)
	}
}
