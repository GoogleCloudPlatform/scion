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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listSkillCapabilities lists skills as user with the given query string and
// returns the list-level capability actions.
func listSkillCapabilities(t *testing.T, srv *Server, user *store.User, query string) []string {
	t.Helper()
	path := "/api/v1/skills"
	if query != "" {
		path += "?" + query
	}
	rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, "got: %s", rec.Body.String())
	var resp ListSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Capabilities, "list response must carry _capabilities")
	return resp.Capabilities.Actions
}

// createSkillStatus attempts a skill create as user and returns the status.
func createSkillStatus(t *testing.T, srv *Server, user *store.User, name, scope, scopeID string) int {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name: name, Scope: scope, ScopeID: scopeID,
	})
	return rec.Code
}

func TestListSkills_Capabilities_Unscoped(t *testing.T) {
	srv, _, alice, bob, _ := setupSkillAuthzTest(t)

	// Any authenticated user can create in their own user scope, so the
	// union over all scopes includes create for both users.
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, alice, ""))
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, bob, ""))
	assert.Equal(t, http.StatusCreated, createSkillStatus(t, srv, bob, "bob-own", store.SkillScopeUser, ""))
}

func TestListSkills_Capabilities_ScopeFilterAllowed(t *testing.T) {
	srv, _, alice, _, project := setupSkillAuthzTest(t)

	assert.Equal(t, []string{"create"},
		listSkillCapabilities(t, srv, alice, "scope=project&scopeId="+project.ID))
	assert.Equal(t, []string{"create"},
		listSkillCapabilities(t, srv, alice, "scope=user"))
	assert.Equal(t, []string{"create"},
		listSkillCapabilities(t, srv, alice, "scope=user&scopeId="+alice.ID))
	assert.Equal(t, http.StatusCreated,
		createSkillStatus(t, srv, alice, "alice-project", store.SkillScopeProject, project.ID))
}

func TestListSkills_Capabilities_ScopeFilterDenied(t *testing.T) {
	srv, _, alice, bob, project := setupSkillAuthzTest(t)

	cases := []struct {
		name  string
		user  *store.User
		query string
	}{
		{"non-member in project", bob, "scope=project&scopeId=" + project.ID},
		{"member in global", alice, "scope=global"},
		{"member in core", alice, "scope=core"},
		{"another user's scope", alice, "scope=user&scopeId=" + bob.ID},
		{"unknown scope", alice, "scope=nonexistent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, listSkillCapabilities(t, srv, tc.user, tc.query))
		})
	}

	// The capability matches what createSkill decides.
	assert.Equal(t, http.StatusForbidden,
		createSkillStatus(t, srv, bob, "bob-project", store.SkillScopeProject, project.ID))
	assert.Equal(t, http.StatusForbidden,
		createSkillStatus(t, srv, alice, "alice-global", store.SkillScopeGlobal, ""))
}

// newSkillCapProject creates a project owned by ownerID with its member
// group and owner binding seeded.
func newSkillCapProject(t *testing.T, srv *Server, s store.Store, name, ownerID string) *store.Project {
	t.Helper()
	p := &store.Project{
		ID: tid(name), Name: name, Slug: name,
		OwnerID: ownerID, CreatedBy: ownerID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(context.Background(), p))
	srv.seedProjectCreatorMembership(context.Background(), p)
	return p
}

func TestListSkills_Capabilities_MixedProjectSet(t *testing.T) {
	srv, s, alice, bob, _ := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Two projects owned by alice. The caller's candidate projects are
	// checked in sorted ID order, so "first" is the one whose ID sorts
	// first.
	pa := newSkillCapProject(t, srv, s, "skillcap-a", alice.ID)
	pb := newSkillCapProject(t, srv, s, "skillcap-b", alice.ID)
	first, second := pa, pb
	if second.ID < first.ID {
		first, second = second, first
	}

	// Bob is a plain member of the first project (project.list, but no
	// skill.create) and an admin of the second (skill.create). His project
	// set mixes a project he cannot create in with one he can, and the one
	// he cannot is checked first.
	ensureHubMembership(ctx, s, bob.ID)
	addProjectMemberWithRole(t, s, first, bob.ID, store.GroupMemberRoleMember)
	addProjectMemberWithRole(t, s, second, bob.ID, store.GroupMemberRoleAdmin)

	// Carol is only a plain member of the first project.
	carol := createNamedTestUser(t, s, "skillcap-carol", store.UserRoleMember)
	ensureHubMembership(ctx, s, carol.ID)
	addProjectMemberWithRole(t, s, first, carol.ID, store.GroupMemberRoleMember)

	// Project scope without a scopeId is the union over the caller's projects.
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, bob, "scope=project"))
	assert.Empty(t, listSkillCapabilities(t, srv, carol, "scope=project"))

	// Per project, the capability matches what createSkill decides.
	assert.Empty(t, listSkillCapabilities(t, srv, bob, "scope=project&scopeId="+first.ID))
	assert.Equal(t, http.StatusForbidden, createSkillStatus(t, srv, bob, "bob-first", store.SkillScopeProject, first.ID))
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, bob, "scope=project&scopeId="+second.ID))
	assert.Equal(t, http.StatusCreated, createSkillStatus(t, srv, bob, "bob-second", store.SkillScopeProject, second.ID))
	assert.Equal(t, http.StatusForbidden, createSkillStatus(t, srv, carol, "carol-first", store.SkillScopeProject, first.ID))
}

// agentSkillCapabilities lists skills with an agent token and returns the
// list-level capability actions.
func agentSkillCapabilities(t *testing.T, srv *Server, token, query string) []string {
	t.Helper()
	path := "/api/v1/skills"
	if query != "" {
		path += "?" + query
	}
	rec := doAgentTokenRequestSkills(t, srv, path, token)
	require.Equal(t, http.StatusOK, rec.Code, "got: %s", rec.Body.String())
	var resp ListSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Capabilities, "list response must carry _capabilities")
	return resp.Capabilities.Actions
}

// agentCreateSkillStatus attempts a skill create with an agent token and
// returns the status.
func agentCreateSkillStatus(t *testing.T, srv *Server, token, name, scope, scopeID string) int {
	t.Helper()
	body, err := json.Marshal(CreateSkillRequest{Name: name, Scope: scope, ScopeID: scopeID})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestListSkills_Capabilities_Agent(t *testing.T) {
	f := setupAgentSkillFixture(t)

	creator := f.newAgent(t, "skillcap-agent-full", f.p.ID, f.u.ID, ScopesForRole(AgentRoleFull))
	require.Contains(t, ScopesForRole(AgentRoleFull), ScopeAgentCreate)
	plain := f.newAgent(t, "skillcap-agent-baseline", f.p.ID, f.u.ID, ScopesForRole(AgentRoleBaseline))
	require.NotContains(t, ScopesForRole(AgentRoleBaseline), ScopeAgentCreate)

	// An agent holding the create scope can create in its own project only.
	assert.Equal(t, []string{"create"}, agentSkillCapabilities(t, f.srv, creator, ""))
	assert.Equal(t, []string{"create"}, agentSkillCapabilities(t, f.srv, creator, "scope=project"))
	assert.Equal(t, []string{"create"}, agentSkillCapabilities(t, f.srv, creator, "scope=project&scopeId="+f.p.ID))
	for _, query := range []string{
		"scope=project&scopeId=" + f.q.ID,
		"scope=user",
		"scope=global",
		"scope=core",
	} {
		assert.Empty(t, agentSkillCapabilities(t, f.srv, creator, query), "query %q", query)
	}

	// Without the create scope an agent can create nowhere.
	assert.Empty(t, agentSkillCapabilities(t, f.srv, plain, ""))
	assert.Empty(t, agentSkillCapabilities(t, f.srv, plain, "scope=project&scopeId="+f.p.ID))

	// The capability matches what createSkill decides.
	assert.Equal(t, http.StatusCreated, agentCreateSkillStatus(t, f.srv, creator, "agent-own", store.SkillScopeProject, f.p.ID))
	assert.Equal(t, http.StatusForbidden, agentCreateSkillStatus(t, f.srv, creator, "agent-other", store.SkillScopeProject, f.q.ID))
	assert.Equal(t, http.StatusForbidden, agentCreateSkillStatus(t, f.srv, plain, "agent-plain", store.SkillScopeProject, f.p.ID))
}

func TestListSkills_Capabilities_ProjectScopeWithoutProjects(t *testing.T) {
	srv, _, _, bob, _ := setupSkillAuthzTest(t)

	// Bob belongs to no project, so project scope offers nothing to create
	// in, even though the unscoped union still includes his user scope.
	assert.Empty(t, listSkillCapabilities(t, srv, bob, "scope=project"))
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, bob, ""))
}

func TestListSkills_Capabilities_SuperAdmin(t *testing.T) {
	srv, s, _, _, project := setupSkillAuthzTest(t)

	adminID := tid("skill-super-admin")
	createTestUserWithRole(t, s, adminID, "skill-super-admin@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	admin, err := s.GetUser(context.Background(), adminID)
	require.NoError(t, err)

	// An unrestricted caller's projects are not enumerated; project scope
	// without a scopeId is still reported as creatable.
	for _, query := range []string{"", "scope=global", "scope=core", "scope=project", "scope=project&scopeId=" + project.ID} {
		assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, admin, query), "query %q", query)
	}
	assert.Equal(t, http.StatusCreated,
		createSkillStatus(t, srv, admin, "admin-global", store.SkillScopeGlobal, ""))
}
