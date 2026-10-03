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
	"encoding/json"
	"net/http"
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

func TestListSkills_Capabilities_MixedProjectSet(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Bob is a hub member who owns a second project. Alice is a member of
	// the first project only, so her project set mixes a project she can
	// create in with one she cannot see.
	ensureHubMembership(ctx, s, bob.ID)
	other := &store.Project{
		ID:        tid("skill-project-other"),
		Name:      "Other Skill Project",
		Slug:      "other-skill-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, other))
	srv.seedProjectCreatorMembership(ctx, other)

	// Project scope without a scopeId is the union over the caller's projects.
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, alice, "scope=project"))
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, bob, "scope=project"))

	// Each user can create only in their own project.
	assert.Empty(t, listSkillCapabilities(t, srv, alice, "scope=project&scopeId="+other.ID))
	assert.Empty(t, listSkillCapabilities(t, srv, bob, "scope=project&scopeId="+project.ID))
	assert.Equal(t, []string{"create"}, listSkillCapabilities(t, srv, bob, "scope=project&scopeId="+other.ID))
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
