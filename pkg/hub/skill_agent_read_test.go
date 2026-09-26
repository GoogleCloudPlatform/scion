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

// ptone/scion#1968: agents read skills. The granted set for an agent A in
// project P, created by user U, is
//
//	G(A) = {global, core skills} ∪ {project skills of P} ∪ {U's own user skills}
//
// gated by the agent JWT carrying project:read and by the delegation ceiling
// (U must still hold skill.read). U is the agent's origin user (the human at
// the root of its creation chain). Other users' user-scoped skills and other
// projects' skills are never in G(A).
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
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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

// A1/A2: baseline and read-only agents read the hub catalog, their own
// project's skills and their creator's user skills — nothing else. In
// particular Su (U's) is granted and Sv (V's) is not.
func TestAgentSkillRead_BaselineAndReadOnlySeeGrantedSet(t *testing.T) {
	for _, role := range []AgentRole{AgentRoleBaseline, AgentRoleReadOnly, AgentRoleFull} {
		t.Run(string(role), func(t *testing.T) {
			f := setupAgentSkillFixture(t)
			token := f.newAgent(t, "as-agent-"+string(role), f.p.ID, f.u.ID, ScopesForRole(role))
			f.assertAgentSees(t, token, agentSkillSet(f.sg, f.sc, f.sp, f.su))
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

// A4 for the user bucket: a creator who no longer holds skill.read (no hub
// membership, no project role) gets no creator user-skill access either —
// the delegation ceiling applies to the relationship grant too — even for a
// user skill the creator owns.
func TestAgentSkillRead_CeilingDeniesCreatorUserSkill(t *testing.T) {
	f := setupAgentSkillFixture(t)
	sb := createTestSkill(t, f.s, "as-user-bob", store.SkillScopeUser, f.bob.ID, f.bob.ID)
	token := f.newAgent(t, "as-agent-ceiling-user", f.p.ID, f.bob.ID, ScopesForRole(AgentRoleBaseline))
	f.assertAgentSees(t, token, agentSkillSet())
	code, _ := f.agentGet(t, token, "/api/v1/skills/"+sb.ID)
	assert.Equal(t, http.StatusNotFound, code, "the creator's own user skill must be denied when the ceiling is empty")
	listed, total := f.agentListAll(t, token, "", 10)
	assert.Empty(t, listed)
	assert.Zero(t, total)
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
	// Su is not project-scoped, so a constraint on P does not reach it.
	f.assertAgentSees(t, token, agentSkillSet(f.sg, f.sc, f.su))

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
	granted := agentSkillSet(f.sg, f.sc, f.sp, f.su)
	for _, extra := range []string{
		"&scopeId=" + f.q.ID,
		"&scope=project&scopeId=" + f.q.ID,
		"&scope=user",
		"&scope=user&scopeId=" + f.u.ID,
		"&ownerId=" + f.u.ID,
		"&scope=user&scopeId=" + f.v.ID,
		"&ownerId=" + f.v.ID,
		"&name=as-user-v",
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
// never nil (unfiltered). Its granted set is the hub catalog plus its
// creator's user skills: the delegation ceiling walks with an empty scope
// (it logs "no project scope" but does not itself deny), and post-backfill
// reads by a hub-attested agent with no edge are allowed. With no origin user
// there is no user bucket. Pinned here, together with probe ⇔ per-row
// agreement on every fixture skill.
func TestAgentSkillRead_ProjectlessAgent(t *testing.T) {
	f := setupAgentSkillFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		ancestry []string
		wantUser string
	}{
		{"with-creator", []string{f.u.ID}, f.u.ID},
		{"no-ancestry", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ident := &agentIdentityWrapper{&AgentTokenClaims{
				Claims:   jwt.Claims{Subject: tid("as-agent-projectless")},
				Scopes:   ScopesForRole(AgentRoleBaseline),
				Ancestry: tc.ancestry,
			}}
			scope := f.srv.agentSkillAccessScope(ctx, ident)
			require.NotNil(t, scope, "an agent predicate must never be nil (unfiltered)")
			assert.Empty(t, scope.ProjectIDs)
			assert.Equal(t, tc.wantUser, scope.CallerID)
			assert.True(t, scope.IncludeHubScope, "project-less agent: G includes the hub catalog (design A17)")
			for _, sk := range f.all() {
				allowed := f.srv.authzService.CheckAccess(ctx, ident, skillResource(sk), ActionRead).Allowed
				assert.Equal(t, agentPredicateMatches(scope, sk), allowed,
					"probe and per-row decision must agree for %s (%s)", sk.Name, sk.Scope)
			}
		})
	}
}

// agentPredicateMatches mirrors skillAccessScopePredicate for one row.
func agentPredicateMatches(scope *store.SkillAccessScope, sk *store.Skill) bool {
	switch sk.Scope {
	case store.SkillScopeGlobal, store.SkillScopeCore:
		return scope.IncludeHubScope
	case store.SkillScopeUser:
		return scope.CallerID != "" && sk.ScopeID == scope.CallerID
	case store.SkillScopeProject:
		for _, id := range scope.ProjectIDs {
			if sk.ScopeID == id {
				return true
			}
		}
	}
	return false
}

// The creator user-skill grant matches only a read of a user-scoped skill
// owned by a hub-attested agent's origin user. A child agent (ancestry
// [U, parent]) gets U's bucket, never its parent's or anyone else's.
func TestAgentCreatorUserSkillGrant_Conditions(t *testing.T) {
	u, v, parent := tid("cus-user-u"), tid("cus-user-v"), tid("cus-parent")
	agent := func(ancestry ...string) PrincipalContext {
		ident := &agentIdentityWrapper{&AgentTokenClaims{
			Claims: jwt.Claims{Subject: tid("cus-agent")}, ProjectID: tid("cus-p"),
			Scopes: ScopesForRole(AgentRoleBaseline), Ancestry: ancestry,
		}}
		return PrincipalContext{ID: ident.ID(), Kind: PrincipalKindAgent, Identity: ident}
	}
	su := skillScopeResource(store.SkillScopeUser, u)
	sv := skillScopeResource(store.SkillScopeUser, v)
	fed := NewFederatedAgentIdentity("https://other.example", tid("cus-fed"), tid("cus-p"), "fed", u, []string{u}, ScopesForRole(AgentRoleBaseline))

	cases := []struct {
		name      string
		principal PrincipalContext
		resource  Resource
		action    Action
		want      bool
	}{
		{"creator skill", agent(u), su, ActionRead, true},
		{"child agent gets origin user's skill", agent(u, parent), su, ActionRead, true},
		{"other user's skill", agent(u), sv, ActionRead, false},
		{"parent agent id is not a user bucket", agent(u, parent), skillScopeResource(store.SkillScopeUser, parent), ActionRead, false},
		{"no ancestry", agent(), su, ActionRead, false},
		{"update", agent(u), su, ActionUpdate, false},
		{"delete", agent(u), su, ActionDelete, false},
		{"global skill", agent(u), skillScopeResource(store.SkillScopeGlobal, ""), ActionRead, false},
		{"project skill", agent(u), skillScopeResource(store.SkillScopeProject, u), ActionRead, false},
		{"user scope without owner", agent(u), skillScopeResource(store.SkillScopeUser, ""), ActionRead, false},
		{"no scope kind", agent(u), Resource{Type: "skill", ScopeUserID: u}, ActionRead, false},
		{"not a skill", agent(u), Resource{Type: "secret", ScopeKind: store.SkillScopeUser, ScopeUserID: u}, ActionRead, false},
		{"federated agent", PrincipalContext{ID: fed.ID(), Kind: PrincipalKindAgent, Identity: fed}, su, ActionRead, false},
		{"user principal", PrincipalContext{ID: u, Kind: PrincipalKindUser}, su, ActionRead, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := agentCreatorUserSkillGrant(tc.principal, tc.resource, tc.action)
			assert.Equal(t, tc.want, ok)
		})
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
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.su), ActionRead).Allowed,
		"an agent with no origin user reads no user skill")
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sq), ActionUpdate).Allowed)
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sg), ActionUpdate).Allowed,
		"the catalog grant is read-only")
	assert.False(t, authz.CheckAccess(ctx, ident, skillResource(f.sg), ActionDelete).Allowed)

	// The creator user-skill grant is read-only and reaches only the
	// creator's own skills.
	// newAgent also records the agent's delegation edge from U.
	f.newAgent(t, "as-agent-a15u", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))
	withCreator := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: tid("as-agent-a15u")}, ProjectID: f.p.ID,
		Scopes: ScopesForRole(AgentRoleBaseline), Ancestry: []string{f.u.ID},
	}}
	assert.True(t, authz.CheckAccess(ctx, withCreator, skillResource(f.su), ActionRead).Allowed)
	assert.False(t, authz.CheckAccess(ctx, withCreator, skillResource(f.sv), ActionRead).Allowed)
	assert.False(t, authz.CheckAccess(ctx, withCreator, skillResource(f.su), ActionUpdate).Allowed)
	assert.False(t, authz.CheckAccess(ctx, withCreator, skillResource(f.su), ActionDelete).Allowed)
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
	want := agentSkillSet(f.sg, f.sc, f.sp, f.su)
	for i := 0; i < hubN; i++ {
		want[createTestSkill(t, f.s, fmt.Sprintf("as-scale-global-%04d", i), store.SkillScopeGlobal, "", f.v.ID).ID] = true
	}
	for i := 0; i < 50; i++ {
		createTestSkill(t, f.s, fmt.Sprintf("as-scale-q-%02d", i), store.SkillScopeProject, f.q.ID, f.v.ID)
		want[createTestSkill(t, f.s, fmt.Sprintf("as-scale-u-%02d", i), store.SkillScopeUser, f.u.ID, f.u.ID).ID] = true
		createTestSkill(t, f.s, fmt.Sprintf("as-scale-v-%02d", i), store.SkillScopeUser, f.v.ID, f.v.ID)
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

// Every per-skill read surface uses the same authorization as GET
// /skills/{id}, so it must agree with it for an agent: skills in G are
// readable on each surface, and skills outside G get the same not-found
// response as a nonexistent skill ID on that surface.
func TestAgentSkillRead_OtherReadSurfacesAgreeWithGet(t *testing.T) {
	f := setupAgentSkillFixture(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	f.srv.SetStorage(stor)

	versions := map[string]string{}
	for _, sk := range f.all() {
		addLocalSkillVersion(t, f.srv, stor, sk, "1.0.0", map[string][]byte{"SKILL.md": []byte("# " + sk.Name)})
		sv, err := f.s.GetSkillVersionByNumber(context.Background(), sk.ID, "1.0.0")
		require.NoError(t, err)
		versions[sk.ID] = sv.ID
	}
	granted := agentSkillSet(f.sg, f.sc, f.sp, f.su)
	token := f.newAgent(t, "as-agent-surfaces", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))

	surfaces := []struct {
		name string
		path func(skillID, versionID string) string
	}{
		{"versions", func(id, _ string) string { return "/api/v1/skills/" + id + "/versions" }},
		{"version", func(id, vid string) string { return "/api/v1/skills/" + id + "/versions/" + vid }},
		{"download", func(id, _ string) string { return "/api/v1/skills/" + id + "/download?version=1.0.0" }},
		{"resolve", func(id, _ string) string { return "/api/v1/skills/" + id + "/resolve?version=1.0.0" }},
		{"files", func(id, _ string) string { return "/api/v1/skills/" + id + "/files/SKILL.md?version=1.0.0" }},
	}
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			_, missingBody := f.agentGet(t, token, surface.path(api.NewUUID(), api.NewUUID()))
			for _, sk := range f.all() {
				code, body := f.agentGet(t, token, surface.path(sk.ID, versions[sk.ID]))
				if granted[sk.ID] {
					assert.Equal(t, http.StatusOK, code, "%s (%s) should be readable: %s", sk.Name, sk.Scope, body)
				} else {
					assert.Equal(t, http.StatusNotFound, code, "%s (%s) must be a 404: %s", sk.Name, sk.Scope, body)
					assert.Equal(t, missingBody, body, "%s (%s): not-found response must be consistent", sk.Name, sk.Scope)
				}
			}
		})
	}

	t.Run("batch-resolve", func(t *testing.T) {
		uris := map[string]string{
			f.sg.ID: "skill://scion/global/" + f.sg.Slug,
			f.sc.ID: "skill://scion/core/" + f.sc.Slug,
			f.sp.ID: "skill://scion/project/" + f.p.ID + "/" + f.sp.Slug,
			f.sq.ID: "skill://scion/project/" + f.q.ID + "/" + f.sq.Slug,
			f.su.ID: "skill://scion/user/" + f.u.ID + "/" + f.su.Slug,
			f.sv.ID: "skill://scion/user/" + f.v.ID + "/" + f.sv.Slug,
		}
		for _, sk := range f.all() {
			rec := doAgentTokenRequest(t, f.srv, http.MethodPost, "/api/v1/skills/resolve",
				ResolveSkillsRequest{Skills: []ResolveSkillRef{{URI: uris[sk.ID]}}}, token)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp ResolveSkillsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			if granted[sk.ID] {
				assert.Len(t, resp.Resolved, 1, "%s (%s) should resolve: %s", sk.Name, sk.Scope, rec.Body.String())
				assert.Empty(t, resp.Errors, sk.Name)
			} else {
				assert.Empty(t, resp.Resolved, "%s (%s) must not resolve", sk.Name, sk.Scope)
				require.Len(t, resp.Errors, 1, sk.Name)
				assert.Equal(t, "not_found", resp.Errors[0].Code, sk.Name)
			}
		}
	})
}
