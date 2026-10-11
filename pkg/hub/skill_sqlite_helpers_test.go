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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	srv.seedProjectCreatorMembership(ctx, q)

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

// publishTestSkillVersion adds a published 1.0.0 version to skill.
func publishTestSkillVersion(t *testing.T, s store.Store, skill *store.Skill) {
	t.Helper()
	require.NoError(t, s.CreateSkillVersion(context.Background(), &store.SkillVersion{
		ID:          api.NewUUID(),
		SkillID:     skill.ID,
		Version:     "1.0.0",
		ContentHash: "sha256:test",
		Status:      store.SkillVersionStatusPublished,
		Created:     time.Now(),
	}))
}

// dispatchTestAgent returns an agent created by creatorID whose inline config
// declares refs as required skills.
func dispatchTestAgent(creatorID, projectID string, refs ...string) *store.Agent {
	skills := make([]api.SkillReference, len(refs))
	for i, r := range refs {
		skills[i] = api.SkillReference{URI: r, Scope: "template"}
	}
	return &store.Agent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		CreatedBy: creatorID,
		OwnerID:   creatorID,
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Skills: skills},
		},
	}
}

// doRawRequestAsUser sends a raw-body request to a hub URL (absolute or
// path-only), authenticated as user.
func doRawRequestAsUser(t *testing.T, srv *Server, user *store.User, method, rawURL string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(method, u.RequestURI(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// doAnonymousRequest sends a request directly to the mux, bypassing the auth
// middleware, so the handler sees a nil identity (defense-in-depth).
func doAnonymousRequest(srv *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// createNamedTestUser creates a user with a given name prefix and role.
func createNamedTestUser(t *testing.T, s store.Store, namePrefix, role string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid(namePrefix),
		Email:       namePrefix + "@test.com",
		DisplayName: namePrefix,
		Role:        role,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// setupSkillAuthzTest creates a test server with two users and a project.
// Alice is a hub member and project owner. Bob is NOT a hub member, so
// the seeded hub-member-read-all policy does not grant him read access.
func setupSkillAuthzTest(t *testing.T) (srv *Server, s store.Store, alice, bob *store.User, project *store.Project) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID:          tid("skill-alice"),
		Email:       "skill-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob = &store.User{
		ID:          tid("skill-bob"),
		Email:       "skill-bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	ensureHubMembership(ctx, s, alice.ID)
	// Bob is intentionally NOT added to hub-members, so default-deny applies.

	project = &store.Project{
		ID:        tid("skill-project"),
		Name:      "Skill Project",
		Slug:      "skill-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	return srv, s, alice, bob, project
}

// createTestSkill is a helper that inserts a skill directly into the store.
func createTestSkill(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.Skill {
	t.Helper()
	skill := &store.Skill{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      "active",
		StoragePath: fmt.Sprintf("skills/%s/%s", scope, api.Slugify(name)),
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateSkill(context.Background(), skill))
	return skill
}

// generateTestGitHubAppKey generates a throwaway RSA private key in PEM
// format, suitable for configuring a fake GitHub App client in tests.
func generateTestGitHubAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return string(pemBytes)
}

// doAgentTokenRequestSkills issues a GET to path authenticated as an agent
// token. Local helper (rather than reusing doAgentTokenRequest from
// port_forward_handlers_test.go) to keep this file self-contained.
func doAgentTokenRequestSkills(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	// Handler(), not mux directly: the agent-token auth middleware that
	// turns X-Scion-Agent-Token into an AgentIdentity in context wraps mux,
	// so bypassing it here would silently test the nil-identity path
	// instead of the agent path.
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeSkillsPageFromRecorder decodes a ListSkillsResponse from an
// already-issued response recorder (used where the request needed a
// hand-built context, so decodeSkillsPage's own request issuance doesn't
// apply).
func decodeSkillsPageFromRecorder(t *testing.T, rec *httptest.ResponseRecorder) ListSkillsResponse {
	t.Helper()
	var resp ListSkillsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// decodeSkillsPage performs one GET against path as user and decodes the
// ListSkillsResponse.
func decodeSkillsPage(t *testing.T, srv *Server, user *store.User, path string) ListSkillsResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// skillIDSet extracts the set of skill IDs from a page of results.
func skillIDSet(skills []SkillWithCapabilities) map[string]bool {
	set := make(map[string]bool, len(skills))
	for _, sk := range skills {
		set[sk.ID] = true
	}
	return set
}

// setupSkillScopeTest builds on setupSkillAuthzTest, adding carol: a hub
// member who is neither the resource owner nor a member of alice's project.
// Carol is the principal the pre-fix bug affected — unlike bob (not a hub
// member at all), carol's denial can only come from the scope boundary
// itself, not from missing hub membership.
func setupSkillScopeTest(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s, alice, _, project = setupSkillAuthzTest(t)
	carol = createNamedTestUser(t, s, "skillscope-carol", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, carol.ID)
	return srv, s, alice, carol, project
}

// createSkillScopeAdmin creates a user with an explicit hub-admin role
// binding. hub-admin (not just User.Role=="admin") is what actually grants
// authority under the AK1 kernel; see TestGlobalSkillCreate_HubAdminStillAllowed
// for the same pattern.
func createSkillScopeAdmin(t *testing.T, s store.Store, namePrefix string) *store.User {
	t.Helper()
	admin := createNamedTestUser(t, s, namePrefix, store.UserRoleMember)
	rd, err := s.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role should have been seeded")
	_, err = s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return admin
}

func createPublishedSkillVersion(t *testing.T, s store.Store, skillID string) *store.SkillVersion {
	t.Helper()
	sv := &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusPublished,
		Created: time.Now(),
	}
	require.NoError(t, s.CreateSkillVersion(context.Background(), sv))
	return sv
}

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

func agentSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
