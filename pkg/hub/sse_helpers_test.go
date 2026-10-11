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

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sseAgentStore struct {
	*mockAuthzStore
	agents    map[string]*store.Agent
	lookupErr error
	authzErr  error
}

func sseAgentRequest(ctx context.Context, userID, role string, subjects ...string) *http.Request {
	query := url.Values{"sub": subjects}
	req := httptest.NewRequest(http.MethodGet, "/events?"+query.Encode(), nil)
	user := &webSessionUser{UserID: userID, Role: role}
	return req.WithContext(context.WithValue(ctx, webUserContextKey{}, user))
}

// mockSuperAdminStore returns a mockAuthzStore pre-configured with a
// super-admin role binding for the given user ID. This allows the AK1
// kernel to grant all permissions without a real database.
func mockSuperAdminStore(userID string) *mockAuthzStore {
	rdID := "mock-rd-super-admin"
	return &mockAuthzStore{
		roleBindings: []*store.RoleBinding{{
			ID:               "mock-rb-super-admin",
			RoleDefinitionID: rdID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        store.SystemReconcileCreatedBy,
		}},
		roleDefinitions: map[string]*store.RoleDefinition{
			rdID: {
				ID:          rdID,
				Name:        store.SystemRoleSuperAdmin,
				ScopeType:   store.RoleScopeSystem,
				Permissions: allPermissionIDs(),
			},
		},
	}
}

// mockAuthzStore satisfies store.Store for NewAuthzService. Only the methods
// called by ComputeCapabilitiesBatch's dependency chain are implemented;
// the embedded interface satisfies everything else at the signature level.
//
// Optional fields allow tests to inject role bindings and role definitions
// so the CO1 kernel can resolve permissions without a real database.
// projectMemberships is served the way the store serves it: as a view over
// project-scoped bindings to the seeded project roles, which the mock also
// returns from the role-binding and role-definition lookups.
type mockAuthzStore struct {
	store.Store // embed to satisfy interface

	roleBindings          []*store.RoleBinding
	roleDefinitions       map[string]*store.RoleDefinition
	projects              []store.Project                     // injectable project list for wildcard expansion tests
	projectMemberships    map[string]*store.ProjectMembership // key: "projectID:userID"
	listProjectsReturnNil bool                                // when true, ListProjects returns (nil, nil)
}

// sseMessageIDs returns the message ids of the events written to an SSE body.
func sseMessageIDs(t *testing.T, body string) []string {
	t.Helper()
	ids := []string{}
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &frame))
		ids = append(ids, frame.Data.ID)
	}
	return ids
}

// sseFlushWriter injects client activity at the first header flush, exactly
// where EventSource can announce an open connection to a snapshot consumer.
type sseFlushWriter struct {
	*httptest.ResponseRecorder
	onFirstFlush func()
	onWrite      func() error
	flushed      bool
}

func newSSEOrderingRequest(t *testing.T) (*WebServer, *ChannelEventPublisher, *http.Request) {
	t.Helper()
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	ws := &WebServer{
		events:       pub,
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest(http.MethodGet, "/events?sub=user.user-1.message", nil)
	user := &webSessionUser{UserID: "user-1", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))
	return ws, pub, req
}

func assertNoSSESubscribers(t *testing.T, pub *ChannelEventPublisher) {
	t.Helper()
	pub.mu.RLock()
	defer pub.mu.RUnlock()
	for pattern, subscribers := range pub.subscribers {
		assert.Empty(t, subscribers, "subscription leaked for %s", pattern)
	}
}

// GetUser returns an active user for any ID, for the live project-access
// check on relationship grants.
func (s *sseAgentStore) GetUser(_ context.Context, id string) (*store.User, error) {
	return &store.User{ID: id, Email: id + "@test.com", Role: "member", Status: store.UserStatusActive}, nil
}

func (s *sseAgentStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if a, ok := s.agents[id]; ok {
		return a, nil
	}
	return nil, store.ErrNotFound
}

func (s *sseAgentStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopes, ids []string) ([]*store.RoleBinding, error) {
	if s.authzErr != nil {
		return nil, s.authzErr
	}
	return s.mockAuthzStore.ListRoleBindingsForPrincipals(ctx, principals, scopes, ids)
}

func (m *mockAuthzStore) GetEffectiveGroups(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockAuthzStore) GetEffectiveGroupsForAgent(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (m *mockAuthzStore) ListGroups(_ context.Context, _ store.GroupFilter, _ store.ListOptions) (*store.ListResult[store.Group], error) {
	return &store.ListResult[store.Group]{}, nil
}

func (m *mockAuthzStore) GetProject(_ context.Context, _ string) (*store.Project, error) {
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetGroupMembership(_ context.Context, _, _ string, _ string) (*store.GroupMember, error) {
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetProjectMembership(_ context.Context, projectID, userID string) (*store.ProjectMembership, error) {
	if m.projectMemberships != nil {
		key := projectID + ":" + userID
		if pm, ok := m.projectMemberships[key]; ok {
			return pm, nil
		}
	}
	return nil, store.ErrNotFound
}

// membershipBindings returns the project-scoped role bindings the
// projectMemberships view is derived from, in a stable order.
func (m *mockAuthzStore) membershipBindings() []*store.RoleBinding {
	keys := make([]string, 0, len(m.projectMemberships))
	for key := range m.projectMemberships {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*store.RoleBinding, 0, len(keys))
	for _, key := range keys {
		pm := m.projectMemberships[key]
		id := pm.RoleBindingID
		if id == "" {
			id = "mock-rb-" + key
		}
		out = append(out, &store.RoleBinding{
			ID:               id,
			RoleDefinitionID: mockProjectRoleDefinitionID(pm.Role),
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      pm.UserID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          pm.ProjectID,
		})
	}
	return out
}

func (m *mockAuthzStore) roleDefinition(id string) (*store.RoleDefinition, bool) {
	if rd, ok := m.roleDefinitions[id]; ok {
		return rd, true
	}
	if len(m.projectMemberships) > 0 {
		if rd, ok := mockProjectRoleDefinitions()[id]; ok {
			return rd, true
		}
	}
	return nil, false
}

func (m *mockAuthzStore) ListRoleBindingsForPrincipal(_ context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	var out []*store.RoleBinding
	for _, rb := range m.membershipBindings() {
		if rb.PrincipalType == principalType && rb.PrincipalID == principalID {
			out = append(out, rb)
		}
	}
	return out, nil
}

func (m *mockAuthzStore) GetRoleDefinition(_ context.Context, id string) (*store.RoleDefinition, error) {
	if rd, ok := m.roleDefinition(id); ok {
		return rd, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetRoleDefinitionByName(_ context.Context, name, scopeType string) (*store.RoleDefinition, error) {
	for _, defs := range []map[string]*store.RoleDefinition{m.roleDefinitions, mockProjectRoleDefinitions()} {
		for _, rd := range defs {
			if rd.Name == name && rd.ScopeType == scopeType {
				return rd, nil
			}
		}
	}
	return nil, store.ErrNotFound
}

func (m *mockAuthzStore) GetRoleDefinitionsByIDs(_ context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	result := make(map[string]*store.RoleDefinition, len(ids))
	for _, id := range ids {
		if rd, ok := m.roleDefinition(id); ok {
			result[id] = rd
		}
	}
	return result, nil
}

// ListRoleBindingsForPrincipals returns the injected roleBindings plus the
// membership bindings of the requested principals.
func (m *mockAuthzStore) ListRoleBindingsForPrincipals(_ context.Context, principals []store.PrincipalRef, _ []string, _ []string) ([]*store.RoleBinding, error) {
	out := append([]*store.RoleBinding(nil), m.roleBindings...)
	for _, rb := range m.membershipBindings() {
		for _, p := range principals {
			if rb.PrincipalType == p.Type && rb.PrincipalID == p.ID {
				out = append(out, rb)
				break
			}
		}
	}
	return out, nil
}

func (m *mockAuthzStore) ListAccessConstraints(_ context.Context, _, _ int) ([]*store.AccessConstraint, error) {
	return nil, nil
}

func (m *mockAuthzStore) ListProjects(_ context.Context, _ store.ProjectFilter, _ store.ListOptions) (*store.ListResult[store.Project], error) {
	if m.listProjectsReturnNil {
		return nil, nil
	}
	items := m.projects
	if items == nil {
		items = []store.Project{}
	}
	return &store.ListResult[store.Project]{Items: items, TotalCount: len(items)}, nil
}

func (w *sseFlushWriter) Flush() {
	w.ResponseRecorder.Flush()
	if !w.flushed {
		w.flushed = true
		w.onFirstFlush()
	}
}

func (w *sseFlushWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		if err := w.onWrite(); err != nil {
			return 0, err
		}
	}
	return w.ResponseRecorder.Write(p)
}

// mockProjectRoleDefinitions are the seeded project-scoped roles
// (seed.go), keyed by ID. Project memberships are views over bindings to
// these roles, so the mock serves them alongside roleDefinitions.
func mockProjectRoleDefinitions() map[string]*store.RoleDefinition {
	defs := map[string]*store.RoleDefinition{}
	for name, perms := range map[string][]string{
		store.ProjectRoleOwner:  projectOwnerPermissionIDs(),
		store.ProjectRoleAdmin:  projectAdminPermissionIDs(),
		store.ProjectRoleMember: projectMemberCuratedPermissionIDs(),
	} {
		id := mockProjectRoleDefinitionID(name)
		defs[id] = &store.RoleDefinition{ID: id, Name: name, ScopeType: store.RoleScopeProject, Permissions: perms, System: true}
	}
	return defs
}

// mockProjectRoleDefinitionID is the role definition ID the mock uses for a
// seeded project role name.
func mockProjectRoleDefinitionID(roleName string) string {
	return "mock-rd-" + roleName
}
