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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// Scope-level agent capabilities must report "create" for a caller who may
// create an agent in at least one project they can reach.
//
// agent.create lives in project-scoped roles, not in hub-member or hub-admin,
// so a hub-scope capability check answers "no" for everyone except a
// super-admin. That hid the "New Agent" button on the Agents page from every
// ordinary user, while the dashboard offered the same action ungated and the
// API happily authorized it per project.

// seedProjectOwner creates a user and a project, and binds the user as
// project-owner on it. Returns the user ID and project ID.
func seedProjectOwner(t *testing.T, s store.Store, name string) (string, string) {
	t.Helper()
	ctx := context.Background()

	userID := tid(name + "-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
	}))

	projectID := tid(name + "-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    name,
		Slug:    name,
		OwnerID: userID,
	}))

	ownerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	require.NotNil(t, ownerRole, "project-owner role definition should be seeded")

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	return userID, projectID
}

func scopeCapsHasAction(caps *Capabilities, action string) bool {
	for _, a := range caps.Actions {
		if a == action {
			return true
		}
	}
	return false
}

func TestAgentScopeCreate_GrantedWhenAProjectAllows(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID, _ := seedProjectOwner(t, s, "scopecreate-owner")
	identity := NewAuthenticatedUser(userID, "scopecreate-owner@test.com", "Owner",
		store.UserRoleMember, "web")

	// Baseline: at hub scope the model has no answer, which is the defect.
	base := srv.authzService.ComputeScopeCapabilities(ctx, identity, "", "", "agent")
	require.False(t, scopeCapsHasAction(base, "create"),
		"precondition: hub-scope agent.create is not held by an ordinary member")

	caps := &Capabilities{Actions: append([]string{}, base.Actions...)}
	srv.addAgentCreateIfAnyProjectAllows(ctx, identity, caps)

	require.True(t, scopeCapsHasAction(caps, "create"),
		"a project owner may create agents in their project, so the page must offer it")
}

func TestAgentScopeCreate_WithheldWhenNoProjectAllows(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// A user with no project bindings at all.
	userID := tid("scopecreate-stranger")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "stranger@test.com",
		DisplayName: "Stranger",
		Role:        store.UserRoleMember,
		Status:      "active",
	}))
	identity := NewAuthenticatedUser(userID, "stranger@test.com", "Stranger",
		store.UserRoleMember, "web")

	caps := &Capabilities{Actions: []string{}}
	srv.addAgentCreateIfAnyProjectAllows(ctx, identity, caps)

	require.False(t, scopeCapsHasAction(caps, "create"),
		"nothing to create into, so the affordance stays hidden")
}

func TestAgentScopeCreate_DoesNotDuplicateExistingCreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID, _ := seedProjectOwner(t, s, "scopecreate-dup")
	identity := NewAuthenticatedUser(userID, "scopecreate-dup@test.com", "Dup",
		store.UserRoleMember, "web")

	// A caller who already holds create at hub scope (a super-admin, or a
	// future role carrying it) must be left alone — and must not accumulate a
	// second entry, which would show up in the API response.
	caps := &Capabilities{Actions: []string{"create", "list"}}
	srv.addAgentCreateIfAnyProjectAllows(ctx, identity, caps)

	count := 0
	for _, a := range caps.Actions {
		if a == "create" {
			count++
		}
	}
	require.Equal(t, 1, count, "create must appear exactly once")
	require.Len(t, caps.Actions, 2, "no other action should be added")
}

func TestAgentScopeCreate_IgnoresNonUserIdentities(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Agents reach creation through the delegation path, which applies its own
	// ceiling checks. This helper must not widen what an agent is told.
	caps := &Capabilities{Actions: []string{}}
	srv.addAgentCreateIfAnyProjectAllows(ctx, nil, caps)

	require.False(t, scopeCapsHasAction(caps, "create"))
}

func TestAgentScopeCreate_NilCapabilitiesIsSafe(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// ComputeScopeCapabilities returns a pointer; a nil must not panic inside
	// the list handler.
	require.NotPanics(t, func() {
		srv.addAgentCreateIfAnyProjectAllows(ctx, nil, nil)
	})
}
