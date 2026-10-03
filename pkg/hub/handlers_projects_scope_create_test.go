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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// The projects list response must carry scope capabilities even when the
// caller can list no projects at all.
//
// project.list resolves to None for a hub member who belongs to no project,
// which is correct. project.create, however, is hub-scoped and held through a
// system-scope role binding. Dropping capabilities on the None path conflated
// the two and hid the "Create Project" button from every user who did not
// already have a project — the one state in which they most need it.

// seedHubMemberNoProjects creates an active member user and puts them in the
// hub-members group, exactly as a newly signed-in user would be. It
// deliberately creates no project.
func seedHubMemberNoProjects(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	ctx := context.Background()

	user := &store.User{
		ID:          tid(name + "-user"),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	return user
}

func TestListProjects_ScopeCapabilitiesPresentWhenCallerHasNoProjects(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := seedHubMemberNoProjects(t, s, "projscope-noprojects")
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName,
		store.UserRoleMember, "web")

	// Precondition: this is genuinely the None path, not an incidental empty list.
	scopeResult, err := srv.authzService.ResolveListScopes(ctx, identity, "project.list")
	require.NoError(t, err)
	require.True(t, scopeResult.Scopes.IsNone(),
		"precondition: a hub member in no projects resolves project.list to None")

	// Precondition: the kernel does grant project.create at hub scope.
	decision := srv.authzService.CheckAccess(ctx, identity, Resource{Type: "project"}, ActionCreate)
	require.True(t, decision.Allowed,
		"precondition: hub-member grants project.create; got reason=%q", decision.Reason)

	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Empty(t, resp.Projects, "caller should see no projects")
	require.NotNil(t, resp.Capabilities,
		"_capabilities must be present on the empty-list path; the web client gates "+
			"the Create Project button on it and fails closed when it is absent")
	require.True(t, scopeCapsHasAction(resp.Capabilities, "create"),
		"scope capabilities should report create; got %v", resp.Capabilities.Actions)
}

// The empty-list path must not invent capabilities either: a caller who does
// not hold project.create must not be told they do.
func TestListProjects_ScopeCapabilitiesOmitCreateWithoutPermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// An active user who is NOT in the hub-members group holds no hub role.
	user := &store.User{
		ID:          tid("projscope-outsider-user"),
		Email:       "projscope-outsider@test.com",
		DisplayName: "projscope-outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName,
		store.UserRoleMember, "web")
	decision := srv.authzService.CheckAccess(ctx, identity, Resource{Type: "project"}, ActionCreate)
	require.False(t, decision.Allowed,
		"precondition: a user with no hub role should not hold project.create")

	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	if resp.Capabilities != nil {
		require.False(t, scopeCapsHasAction(resp.Capabilities, "create"),
			"a caller without project.create must not be offered create; got %v",
			resp.Capabilities.Actions)
	}
}
