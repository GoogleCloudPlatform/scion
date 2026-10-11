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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createCustomRoleDef creates a non-built-in project-scoped role definition.
func createCustomRoleDef(t *testing.T, s store.Store, name string, perms []string) *store.RoleDefinition {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name,
		ScopeType:   store.RoleScopeProject,
		Permissions: perms,
		System:      false,
	})
	require.NoError(t, err, "failed to create custom role definition %q", name)
	return rd
}

// createCustomBinding creates a custom project-scoped role binding directly
// in the store (bypassing the membership service, which only handles built-in
// roles).
func createCustomBinding(t *testing.T, s store.Store, roleDefID, principalID, projectID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test-custom",
	})
	require.NoError(t, err, "failed to create custom role binding")
	return rb
}

// listProjectBindings returns all project-scoped bindings for a principal in a
// project.
func listProjectBindings(t *testing.T, s store.Store, principalID, projectID string) []*store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	all, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, principalID)
	require.NoError(t, err)
	var result []*store.RoleBinding
	for _, b := range all {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			result = append(result, b)
		}
	}
	return result
}

// assertBindingPreserved verifies that a binding exists unchanged: same ID,
// same role definition, same metadata fields. Uses a fresh read from the
// store as the "before" snapshot to avoid SQLite timestamp-precision
// differences between the in-memory object returned by Create and a
// subsequent Get.
func assertBindingPreserved(t *testing.T, s store.Store, original *store.RoleBinding) {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetRoleBinding(ctx, original.ID)
	require.NoError(t, err, "custom binding %s must still exist", original.ID)
	assert.Equal(t, original.ID, got.ID, "binding ID must be unchanged")
	assert.Equal(t, original.RoleDefinitionID, got.RoleDefinitionID, "role definition must be unchanged")
	assert.Equal(t, original.PrincipalType, got.PrincipalType, "principal type must be unchanged")
	assert.Equal(t, original.PrincipalID, got.PrincipalID, "principal ID must be unchanged")
	assert.Equal(t, original.ScopeType, got.ScopeType, "scope type must be unchanged")
	assert.Equal(t, original.ScopeID, got.ScopeID, "scope ID must be unchanged")
	assert.Equal(t, original.CreatedBy, got.CreatedBy, "created_by must be unchanged")
	if original.NotBefore != nil {
		require.NotNil(t, got.NotBefore, "not_before must be preserved")
		assert.True(t, original.NotBefore.Equal(*got.NotBefore), "not_before must be unchanged")
	} else {
		assert.Nil(t, got.NotBefore, "not_before must remain nil")
	}
	if original.ExpiresAt != nil {
		require.NotNil(t, got.ExpiresAt, "expires_at must be preserved")
		assert.True(t, original.ExpiresAt.Equal(*got.ExpiresAt), "expires_at must be unchanged")
	} else {
		assert.Nil(t, got.ExpiresAt, "expires_at must remain nil")
	}
}

// snapshotBinding reads a binding from the store to get a consistent
// timestamp-precision snapshot. Use this immediately after CreateRoleBinding
// to get the "before" reference for assertBindingPreserved.
func snapshotBinding(t *testing.T, s store.Store, rb *store.RoleBinding) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err, "failed to snapshot binding %s", rb.ID)
	return got
}

// r2FailingStore wraps a real store.Store and injects failures at specific
// points in the authorization and list pipeline:
//   - failGetEffectiveGroups: principal/group closure failure
//   - failListBindings: role-binding load failure
//   - failGetRoleDefinition: role-definition load failure
//   - failListConstraints: constraint load failure
//   - failListProjects: resource list failure
//   - failListAgents: resource list failure
type r2FailingStore struct {
	store.Store
	failGetEffectiveGroups error
	failListBindings       error
	failGetRoleDefinition  error
	failListConstraints    error
	failListProjects       error
	failListAgents         error
}

// installFailStore swaps srv.store and srv.authzService.store with a failing
// wrapper and returns a restore function. Both must be swapped because the
// handler uses srv.store for the list query and srv.authzService uses its own
// store reference for scope resolution.
func installFailStore(srv *Server, fs *r2FailingStore) func() {
	origStore := srv.store
	origAuthzStore := srv.authzService.store
	fs.Store = origStore
	srv.store = fs
	srv.authzService.store = fs
	return func() {
		srv.store = origStore
		srv.authzService.store = origAuthzStore
	}
}

func (s *r2FailingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.failGetEffectiveGroups != nil {
		return nil, s.failGetEffectiveGroups
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *r2FailingStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if s.failGetEffectiveGroups != nil {
		return nil, s.failGetEffectiveGroups
	}
	return s.Store.GetEffectiveGroupsForAgent(ctx, agentID)
}

func (s *r2FailingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failListBindings != nil {
		return nil, s.failListBindings
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *r2FailingStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if s.failGetRoleDefinition != nil {
		return nil, s.failGetRoleDefinition
	}
	return s.Store.GetRoleDefinition(ctx, id)
}

func (s *r2FailingStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	if s.failGetRoleDefinition != nil {
		return nil, s.failGetRoleDefinition
	}
	return s.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

func (s *r2FailingStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if s.failListConstraints != nil {
		return nil, s.failListConstraints
	}
	return s.Store.ListAccessConstraints(ctx, limit, offset)
}

func (s *r2FailingStore) ListProjects(ctx context.Context, filter store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	if s.failListProjects != nil {
		return nil, s.failListProjects
	}
	return s.Store.ListProjects(ctx, filter, opts)
}

func (s *r2FailingStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if s.failListAgents != nil {
		return nil, s.failListAgents
	}
	return s.Store.ListAgents(ctx, filter, opts)
}
