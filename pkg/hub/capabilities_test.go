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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type authorizedListHandlerFailingStore struct {
	store.Store
	listCalls      int
	wrongListOpts  bool
	failList       func() error
	failPolicyRead error
}

func (s *authorizedListHandlerFailingStore) checkListOptions(opts store.ListOptions) {
	s.listCalls++
	if !opts.SkipTotalCount || opts.Limit != authorizedListBatchSize {
		s.wrongListOpts = true
	}
}

func (s *authorizedListHandlerFailingStore) GetPoliciesForPrincipals(ctx context.Context, principals []store.PrincipalRef) ([]store.Policy, error) {
	if s.failPolicyRead != nil {
		return nil, s.failPolicyRead
	}
	return s.Store.GetPoliciesForPrincipals(ctx, principals)
}

func (s *authorizedListHandlerFailingStore) ListTemplates(ctx context.Context, filter store.TemplateFilter, opts store.ListOptions) (*store.ListResult[store.Template], error) {
	s.checkListOptions(opts)
	if s.failList != nil && s.listCalls > 1 {
		return nil, s.failList()
	}
	return s.Store.ListTemplates(ctx, filter, opts)
}

func (s *authorizedListHandlerFailingStore) ListHarnessConfigs(ctx context.Context, filter store.HarnessConfigFilter, opts store.ListOptions) (*store.ListResult[store.HarnessConfig], error) {
	s.checkListOptions(opts)
	if s.failList != nil && s.listCalls > 1 {
		return nil, s.failList()
	}
	return s.Store.ListHarnessConfigs(ctx, filter, opts)
}

func (s *authorizedListHandlerFailingStore) ListGroups(ctx context.Context, filter store.GroupFilter, opts store.ListOptions) (*store.ListResult[store.Group], error) {
	s.checkListOptions(opts)
	if s.failList != nil && s.listCalls > 1 {
		return nil, s.failList()
	}
	return s.Store.ListGroups(ctx, filter, opts)
}

func expectedAgentResourceActions() []string {
	actions := make([]string, len(ResourceActions["agent"]))
	for i, action := range ResourceActions["agent"] {
		actions[i] = string(action)
	}
	return actions
}

func TestComputeCapabilities_AdminGetsAllActions(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Super-admin (role=admin) gets all actions via CheckAccess/Decide step-1 bypass
	// even after the IsUnscopedLocalPlatformAdmin short-circuit was removed.
	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "agent", ID: "some-agent"}

	caps := srv.authzService.ComputeCapabilities(ctx, admin, resource)
	assert.Equal(t, expectedAgentResourceActions(), caps.Actions)
}

func TestComputeCapabilities_HubAdminGetsOnlyPolicyGrantedActions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user who has a policy granting only "read" and "update" on agents.
	// This simulates a hub-admin who has role bindings but is not a super-admin.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-hubadmin-cap"), Email: "hubadmin-cap@test.com", DisplayName: "HubAdmin", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-hubadmin-cap"), Name: "Hub Admin Agent Read/Update", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read", "update"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-hubadmin-cap"), PrincipalType: "user", PrincipalID: tid("user-hubadmin-cap"),
	}))

	user := NewAuthenticatedUser(tid("user-hubadmin-cap"), "hubadmin-cap@test.com", "HubAdmin", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	// Hub-admin should get exactly the policy-granted actions, not all and not zero.
	assert.Equal(t, []string{"read", "update"}, caps.Actions)
}

func TestComputeScopeCapabilities_AdminGetsAllScopeActions(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Super-admin still gets all scope actions after short-circuit removal.
	admin := NewAuthenticatedUser("admin-scope-regr", "admin-scope-regr@example.com", "Admin", "admin", "api")
	caps := srv.authzService.ComputeScopeCapabilities(ctx, admin, "", "", "agent")
	assert.Equal(t, []string{"create", "list", "stop_all", "message"}, caps.Actions)
}

func TestComputeCapabilitiesBatch_AdminGetsAllAfterConversion(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Super-admin still gets all actions per resource in batch after short-circuit removal.
	admin := NewAuthenticatedUser("admin-batch-regr", "admin-batch-regr@example.com", "Admin", "admin", "api")
	resources := []Resource{
		{Type: "agent", ID: tid("agent-1")},
		{Type: "agent", ID: tid("agent-2")},
	}
	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, admin, resources, "agent")
	require.Len(t, caps, 2)
	for _, cap := range caps {
		assert.Equal(t, expectedAgentResourceActions(), cap.Actions)
	}
}

func TestComputeCapabilities_OwnerGetsAllActions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-owner-cap"), Email: "owner-cap@test.com", DisplayName: "Owner", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-owner-cap"), "owner-cap@test.com", "Owner", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), OwnerID: tid("user-owner-cap")}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, expectedAgentResourceActions(), caps.Actions)
}

func TestComputeCapabilities_PolicySubset(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-readonly-cap"), Email: "readonly-cap@test.com", DisplayName: "ReadOnly", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-ro-cap"), Name: "Read Only", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-ro-cap"), PrincipalType: "user", PrincipalID: tid("user-readonly-cap"),
	}))

	user := NewAuthenticatedUser(tid("user-readonly-cap"), "readonly-cap@test.com", "ReadOnly", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, []string{"read"}, caps.Actions)
}

func TestComputeCapabilities_DefaultDenyEmpty(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-nopolicy-cap"), Email: "nopolicy-cap@test.com", DisplayName: "NoPolicy", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-nopolicy-cap"), "nopolicy-cap@test.com", "NoPolicy", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, []string{}, caps.Actions)
}

func TestComputeCapabilitiesBatch_AdminGetsAll(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-batch", "admin-batch@example.com", "Admin", "admin", "api")
	resources := []Resource{
		{Type: "agent", ID: tid("agent-1")},
		{Type: "agent", ID: tid("agent-2")},
		{Type: "agent", ID: tid("agent-3")},
	}

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, admin, resources, "agent")
	require.Len(t, caps, 3)
	for _, cap := range caps {
		assert.Equal(t, expectedAgentResourceActions(), cap.Actions)
	}
}

func TestComputeCapabilitiesBatch_ScopedAdminCannotReadCrossProjectResources(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := NewAuthenticatedUser(tid("scoped-cap-admin"), "admin@example.com", "Admin", store.UserRoleAdmin, "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: admin.ID(), Email: admin.Email(), DisplayName: admin.DisplayName(), Role: store.UserRoleAdmin, Status: "active"}))
	projectA := tid("scoped-cap-project-a")
	projectB := tid("scoped-cap-project-b")
	policy := &store.Policy{ID: tid("scoped-cap-read"), Name: "Scoped project read", ScopeType: "project", ScopeID: projectA, ResourceType: "agent", Actions: []string{"read"}, Effect: "allow"}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{PolicyID: policy.ID, PrincipalType: "user", PrincipalID: admin.ID()}))
	scoped := NewScopedUserIdentity(admin, projectA, []string{"agent:read"})
	ctx = contextWithIdentity(ctx, scoped)

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, scoped, []Resource{
		{Type: "agent", ID: tid("scoped-cap-agent-a"), ParentType: "project", ParentID: projectA},
		{Type: "agent", ID: tid("scoped-cap-agent-b"), ParentType: "project", ParentID: projectB},
	}, "agent")

	require.Len(t, caps, 2)
	assert.Contains(t, caps[0].Actions, "read")
	assert.NotContains(t, caps[1].Actions, "read")
}

func TestScopedAdminListsExcludeCrossProjectCapabilities(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := NewAuthenticatedUser(tid("scoped-list-admin"), "admin@example.com", "Admin", store.UserRoleAdmin, "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: admin.ID(), Email: admin.Email(), DisplayName: admin.DisplayName(), Role: store.UserRoleAdmin, Status: "active"}))
	projectA := &store.Project{ID: tid("scoped-list-project-a"), Name: "Project A", Slug: "scoped-list-project-a"}
	projectB := &store.Project{ID: tid("scoped-list-project-b"), Name: "Project B", Slug: "scoped-list-project-b"}
	require.NoError(t, s.CreateProject(ctx, projectA))
	require.NoError(t, s.CreateProject(ctx, projectB))
	agentA := &store.Agent{ID: tid("scoped-list-agent-a"), Name: "Agent A", Slug: "scoped-list-agent-a", ProjectID: projectA.ID, Phase: "running"}
	agentB := &store.Agent{ID: tid("scoped-list-agent-b"), Name: "Agent B", Slug: "scoped-list-agent-b", ProjectID: projectB.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.CreateAgent(ctx, agentB))
	for _, policy := range []*store.Policy{
		{ID: tid("scoped-list-agent-read"), Name: "Agent read", ScopeType: "project", ScopeID: projectA.ID, ResourceType: "agent", Actions: []string{"read"}, Effect: "allow"},
		{ID: tid("scoped-list-project-read"), Name: "Project read", ScopeType: "project", ScopeID: projectA.ID, ResourceType: "project", Actions: []string{"read"}, Effect: "allow"},
	} {
		require.NoError(t, s.CreatePolicy(ctx, policy))
		require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{PolicyID: policy.ID, PrincipalType: "user", PrincipalID: admin.ID()}))
	}
	scoped := NewScopedUserIdentity(admin, projectA.ID, []string{"agent:read", "project:read"})

	for _, handler := range []func(http.ResponseWriter, *http.Request){srv.listAgents, srv.listProjects} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil).WithContext(contextWithIdentity(ctx, scoped))
		rec := httptest.NewRecorder()
		handler(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), projectA.ID)
		assert.NotContains(t, rec.Body.String(), projectB.ID)
		assert.NotContains(t, rec.Body.String(), agentB.ID)
	}
}

func TestScopedAdminListEndpointsFilterCrossProjectRowsAndCountAuthorizedMatches(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := NewAuthenticatedUser(tid("scoped-list-endpoint-admin"), "admin@example.com", "Admin", store.UserRoleAdmin, "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: admin.ID(), Email: admin.Email(), DisplayName: admin.DisplayName(), Role: store.UserRoleAdmin, Status: "active"}))
	projectA := &store.Project{ID: tid("scoped-list-endpoint-a"), Name: "Project A", Slug: "scoped-list-endpoint-a"}
	projectB := &store.Project{ID: tid("scoped-list-endpoint-b"), Name: "Project B", Slug: "scoped-list-endpoint-b"}
	require.NoError(t, s.CreateProject(ctx, projectA))
	require.NoError(t, s.CreateProject(ctx, projectB))
	now := time.Now()
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: tid("scoped-list-template-a"), Name: "Template A", Slug: "scoped-list-template-a", Scope: store.TemplateScopeProject, ScopeID: projectA.ID, Harness: "claude", Status: store.TemplateStatusActive, Created: now, Updated: now}))
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: tid("scoped-list-template-b"), Name: "Template B", Slug: "scoped-list-template-b", Scope: store.TemplateScopeProject, ScopeID: projectB.ID, Harness: "claude", Status: store.TemplateStatusActive, Created: now, Updated: now}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: tid("scoped-list-config-a"), Name: "Config A", Slug: "scoped-list-config-a", Scope: store.HarnessConfigScopeProject, ScopeID: projectA.ID, Harness: "claude", Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: tid("scoped-list-config-b"), Name: "Config B", Slug: "scoped-list-config-b", Scope: store.HarnessConfigScopeProject, ScopeID: projectB.ID, Harness: "claude", Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: tid("scoped-list-group-a"), Name: "Group A", Slug: "scoped-list-group-a", GroupType: store.GroupTypeExplicit, ProjectID: projectA.ID, OwnerID: admin.ID()}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: tid("scoped-list-group-b"), Name: "Group B", Slug: "scoped-list-group-b", GroupType: store.GroupTypeExplicit, ProjectID: projectB.ID, OwnerID: admin.ID()}))
	for i := 0; i < 50; i++ {
		suffix := fmt.Sprintf("%02d", i)
		require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: tid("scoped-list-template-extra-" + suffix), Name: "Template Extra " + suffix, Slug: "scoped-list-template-extra-" + suffix, Scope: store.TemplateScopeProject, ScopeID: projectA.ID, Harness: "claude", Status: store.TemplateStatusActive, Created: now, Updated: now}))
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: tid("scoped-list-config-extra-" + suffix), Name: "Config Extra " + suffix, Slug: "scoped-list-config-extra-" + suffix, Scope: store.HarnessConfigScopeProject, ScopeID: projectA.ID, Harness: "claude", Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
		require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: tid("scoped-list-group-extra-" + suffix), Name: "Group Extra " + suffix, Slug: "scoped-list-group-extra-" + suffix, GroupType: store.GroupTypeExplicit, ProjectID: projectA.ID, OwnerID: admin.ID()}))
	}
	for _, policy := range []*store.Policy{
		{ID: tid("scoped-list-template-read"), Name: "Template read", ScopeType: "project", ScopeID: projectA.ID, ResourceType: "template", Actions: []string{"read"}, Effect: "allow"},
		{ID: tid("scoped-list-config-read"), Name: "Config read", ScopeType: "project", ScopeID: projectA.ID, ResourceType: "harness_config", Actions: []string{"read"}, Effect: "allow"},
		{ID: tid("scoped-list-group-read"), Name: "Group read", ScopeType: "project", ScopeID: projectA.ID, ResourceType: "group", Actions: []string{"read"}, Effect: "allow"},
	} {
		require.NoError(t, s.CreatePolicy(ctx, policy))
		require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{PolicyID: policy.ID, PrincipalType: "user", PrincipalID: admin.ID()}))
	}
	scoped := NewScopedUserIdentity(admin, projectA.ID, []string{"template:read", "harness_config:read", "group:read"})
	for _, tc := range []struct {
		path, allowedID, deniedID string
		handler                   func(http.ResponseWriter, *http.Request)
		itemsKey                  string
		unscopedTotal             int
	}{
		{"/api/v1/templates", tid("scoped-list-template-a"), tid("scoped-list-template-b"), srv.listTemplatesV2, "templates", 52},
		{"/api/v1/harness-configs", tid("scoped-list-config-a"), tid("scoped-list-config-b"), srv.listHarnessConfigs, "harnessConfigs", 52},
		{"/api/v1/groups", tid("scoped-list-group-a"), tid("scoped-list-group-b"), srv.listGroups, "groups", 53}, // Includes the seeded hub-members group.
	} {
		t.Run(tc.path, func(t *testing.T) {
			request := func(identity Identity, path string) (string, int) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(contextWithIdentity(ctx, identity))
				tc.handler(rec, req)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				body := rec.Body.String()
				var response struct {
					TotalCount int `json:"totalCount"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				return body, response.TotalCount
			}

			body, totalCount := request(scoped, tc.path+"?limit=1")
			assert.NotContains(t, body, tc.deniedID)
			assert.Equal(t, 51, totalCount, "authorized total must not collapse to the current page length")

			body, totalCount = request(scoped, tc.path+"?projectId="+projectB.ID)
			assert.NotContains(t, body, tc.deniedID)
			assert.Equal(t, 0, totalCount)

			body, totalCount = request(scoped, tc.path+"?projectId="+projectA.ID+"&limit=100")
			assert.Contains(t, body, tc.allowedID)
			assert.Equal(t, 51, totalCount)
			var response map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(body), &response))
			var items []struct {
				ID  string        `json:"id"`
				Cap *Capabilities `json:"_capabilities"`
			}
			require.NoError(t, json.Unmarshal(response[tc.itemsKey], &items))
			for _, item := range items {
				if item.ID == tc.allowedID {
					require.NotNil(t, item.Cap)
					assert.Contains(t, item.Cap.Actions, string(ActionRead))
					break
				}
			}

			_, totalCount = request(admin, tc.path+"?limit=1")
			assert.Equal(t, tc.unscopedTotal, totalCount, "unscoped admin total must not collapse to the current page length")

			body, totalCount = request(admin, tc.path+"?limit=100")
			assert.Contains(t, body, tc.allowedID)
			assert.Contains(t, body, tc.deniedID)
			assert.Equal(t, tc.unscopedTotal, totalCount)

			body, totalCount = request(admin, tc.path+"?projectId="+projectB.ID)
			assert.Contains(t, body, tc.deniedID)
			assert.Equal(t, 1, totalCount)
		})
	}
}

func TestListEndpointsFailClosedOnStoreOrAuthorizationError(t *testing.T) {
	for _, tc := range []struct {
		path     string
		itemsKey string
		handler  func(*Server) func(http.ResponseWriter, *http.Request)
		seed     func(testing.TB, store.Store, time.Time)
	}{
		{
			path:     "/api/v1/templates",
			itemsKey: "templates",
			handler:  func(s *Server) func(http.ResponseWriter, *http.Request) { return s.listTemplatesV2 },
			seed: func(t testing.TB, s store.Store, now time.Time) {
				for i := range authorizedListBatchSize + 1 {
					require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{ID: tid(fmt.Sprintf("failed-list-template-%d", i)), Name: fmt.Sprintf("Template %d", i), Slug: fmt.Sprintf("failed-list-template-%d", i), Scope: store.TemplateScopeGlobal, Harness: "codex", Status: store.TemplateStatusActive, Created: now, Updated: now}))
				}
			},
		},
		{
			path:     "/api/v1/harness-configs",
			itemsKey: "harnessConfigs",
			handler:  func(s *Server) func(http.ResponseWriter, *http.Request) { return s.listHarnessConfigs },
			seed: func(t testing.TB, s store.Store, now time.Time) {
				for i := range authorizedListBatchSize + 1 {
					require.NoError(t, s.CreateHarnessConfig(context.Background(), &store.HarnessConfig{ID: tid(fmt.Sprintf("failed-list-config-%d", i)), Name: fmt.Sprintf("Config %d", i), Slug: fmt.Sprintf("failed-list-config-%d", i), Scope: store.HarnessConfigScopeGlobal, Harness: "codex", Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
				}
			},
		},
		{
			path:     "/api/v1/groups",
			itemsKey: "groups",
			handler:  func(s *Server) func(http.ResponseWriter, *http.Request) { return s.listGroups },
			seed: func(t testing.TB, s store.Store, now time.Time) {
				for i := range authorizedListBatchSize + 1 {
					require.NoError(t, s.CreateGroup(context.Background(), &store.Group{ID: tid(fmt.Sprintf("failed-list-group-%d", i)), Name: fmt.Sprintf("Group %d", i), Slug: fmt.Sprintf("failed-list-group-%d", i), GroupType: store.GroupTypeExplicit, Created: now, Updated: now}))
				}
			},
		},
	} {
		for _, failure := range []struct {
			name      string
			configure func(*authorizedListHandlerFailingStore)
		}{
			{name: "later page store error", configure: func(s *authorizedListHandlerFailingStore) {
				s.failList = func() error { return errors.New("store unavailable") }
			}},
			{name: "authorization read error", configure: func(s *authorizedListHandlerFailingStore) { s.failPolicyRead = errors.New("authorization unavailable") }},
		} {
			t.Run(tc.path+"/"+failure.name, func(t *testing.T) {
				srv, baseStore := testServer(t)
				tc.seed(t, baseStore, time.Now())
				failingStore := &authorizedListHandlerFailingStore{Store: baseStore}
				failure.configure(failingStore)
				srv.store = failingStore
				srv.authzService = NewAuthzService(failingStore, srv.authzService.logger)

				identity := NewScopedUserIdentity(NewAuthenticatedUser(tid("failed-list-user"), "failed-list@example.com", "Failed List", store.UserRoleAdmin, "api"), "", []string{"template:read", "harness_config:read", "group:read"})
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(contextWithIdentity(context.Background(), identity))
				tc.handler(srv)(rec, req)

				require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
				var response map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
				assert.Contains(t, response, "error")
				assert.NotContains(t, response, tc.itemsKey)
				assert.NotContains(t, response, "totalCount")
				if failure.name == "later page store error" {
					assert.GreaterOrEqual(t, failingStore.listCalls, 2)
				}
				assert.False(t, failingStore.wrongListOpts, "restricted list fetch must use bounded skip-count options")
			})
		}
	}
}

func TestListEndpointsRejectMalformedAndCrossBoundCursors(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := NewAuthenticatedUser(tid("cursor-admin"), "cursor-admin@example.com", "Admin", store.UserRoleAdmin, "api")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: admin.ID(), Email: admin.Email(), DisplayName: admin.DisplayName(), Role: store.UserRoleAdmin, Status: "active"}))
	projectA := &store.Project{ID: tid("cursor-project-a"), Name: "Cursor A", Slug: "cursor-a"}
	projectB := &store.Project{ID: tid("cursor-project-b"), Name: "Cursor B", Slug: "cursor-b"}
	require.NoError(t, s.CreateProject(ctx, projectA))
	require.NoError(t, s.CreateProject(ctx, projectB))
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("%d", i)
		require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: tid("cursor-template-" + id), Name: "template-" + id, Slug: "template-" + id, Harness: "codex", Scope: store.TemplateScopeProject, ScopeID: projectA.ID, Status: store.TemplateStatusActive}))
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: tid("cursor-config-" + id), Name: "config-" + id, Slug: "config-" + id, Harness: "codex", Scope: store.HarnessConfigScopeProject, ScopeID: projectA.ID, Status: store.HarnessConfigStatusActive}))
		require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: tid("cursor-group-" + id), Name: "group-" + id, Slug: "group-" + id, ProjectID: projectA.ID}))
	}
	for _, tc := range []struct {
		path    string
		handler func(http.ResponseWriter, *http.Request)
		other   func(http.ResponseWriter, *http.Request)
	}{
		{"/api/v1/templates", srv.listTemplatesV2, srv.listHarnessConfigs},
		{"/api/v1/harness-configs", srv.listHarnessConfigs, srv.listGroups},
		{"/api/v1/groups", srv.listGroups, srv.listTemplatesV2},
	} {
		t.Run(tc.path, func(t *testing.T) {
			request := func(path string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(contextWithIdentity(ctx, admin))
				tc.handler(rec, req)
				return rec
			}
			first := request(tc.path + "?limit=1&projectId=" + projectA.ID)
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			var page struct {
				NextCursor string `json:"nextCursor"`
			}
			require.NoError(t, json.Unmarshal(first.Body.Bytes(), &page))
			require.NotEmpty(t, page.NextCursor)
			assert.Equal(t, http.StatusBadRequest, request(tc.path+"?cursor=malformed").Code)
			assert.Equal(t, http.StatusBadRequest, request(tc.path+"?cursor="+page.NextCursor+"&projectId="+projectB.ID).Code)
			crossEndpoint := httptest.NewRecorder()
			crossRequest := httptest.NewRequest(http.MethodGet, "/api/v1/other?cursor="+page.NextCursor, nil).WithContext(contextWithIdentity(ctx, admin))
			tc.other(crossEndpoint, crossRequest)
			assert.Equal(t, http.StatusBadRequest, crossEndpoint.Code)
		})
	}
}

func TestComputeCapabilitiesBatch_MixedOwnership(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-mixed-cap"), Email: "mixed-cap@test.com", DisplayName: "Mixed", Role: "member", Status: "active",
	}))

	// Policy grants read-only on agents
	policy := &store.Policy{
		ID: tid("policy-mixed-cap"), Name: "Read Only", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-mixed-cap"), PrincipalType: "user", PrincipalID: tid("user-mixed-cap"),
	}))

	user := NewAuthenticatedUser(tid("user-mixed-cap"), "mixed-cap@test.com", "Mixed", "member", "api")
	resources := []Resource{
		{Type: "agent", ID: "agent-owned", OwnerID: tid("user-mixed-cap")},  // Owned
		{Type: "agent", ID: tid("agent-other"), OwnerID: tid("other-user")}, // Not owned
	}

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, user, resources, "agent")
	require.Len(t, caps, 2)

	// Owned resource gets all actions
	assert.Equal(t, expectedAgentResourceActions(), caps[0].Actions)

	// Non-owned resource gets only read from policy
	assert.Equal(t, []string{"read"}, caps[1].Actions)
}

func TestComputeCapabilities_AncestorGetsAllActions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-ancestor-cap"), Email: "ancestor-cap@test.com", DisplayName: "Ancestor", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-ancestor-cap"), "ancestor-cap@test.com", "Ancestor", "member", "api")
	resource := Resource{
		Type:     "agent",
		ID:       "agent-descendant",
		OwnerID:  "someone-else",
		Ancestry: []string{tid("user-ancestor-cap"), "agent-middle"},
	}

	caps := srv.authzService.ComputeCapabilities(ctx, user, resource)
	assert.Equal(t, expectedAgentResourceActions(), caps.Actions)
}

func TestComputeCapabilitiesBatch_AncestryAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-batch-ancestor"), Email: "batch-ancestor@test.com", DisplayName: "BatchAnc", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-batch-ancestor"), "batch-ancestor@test.com", "BatchAnc", "member", "api")
	resources := []Resource{
		{Type: "agent", ID: "agent-descendant-1", OwnerID: "other", Ancestry: []string{tid("user-batch-ancestor"), "agent-A"}},
		{Type: "agent", ID: "agent-unrelated", OwnerID: "other", Ancestry: []string{tid("other-user")}},
	}

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, user, resources, "agent")
	require.Len(t, caps, 2)

	// Descendant gets all actions via ancestry
	assert.Equal(t, expectedAgentResourceActions(), caps[0].Actions)

	// Unrelated agent gets empty (no policy, not owner, not ancestor)
	assert.Equal(t, []string{}, caps[1].Actions)
}

func TestComputeScopeCapabilities(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-scope-cap", "admin-scope@example.com", "Admin", "admin", "api")

	caps := srv.authzService.ComputeScopeCapabilities(ctx, admin, "", "", "agent")
	assert.Equal(t, []string{"create", "list", "stop_all", "message"}, caps.Actions)
}

func TestComputeScopeCapabilities_NoPolicy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-noscope-cap"), Email: "noscope-cap@test.com", DisplayName: "NoScope", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-noscope-cap"), "noscope-cap@test.com", "NoScope", "member", "api")
	caps := srv.authzService.ComputeScopeCapabilities(ctx, user, "", "", "agent")
	assert.Equal(t, []string{}, caps.Actions)
}

func TestAgentWithCapabilities_JSONStructure(t *testing.T) {
	awc := AgentWithCapabilities{
		Agent: store.Agent{
			ID:   "agent-json-1",
			Name: "Test Agent",
			Slug: "test-agent",
		},
		Cap: &Capabilities{
			Actions: []string{"read", "update"},
		},
	}

	data, err := json.Marshal(awc)
	require.NoError(t, err)

	// Verify flat JSON structure
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	// Agent fields should be at top level
	assert.Equal(t, "agent-json-1", result["id"])
	assert.Equal(t, "Test Agent", result["name"])
	assert.Equal(t, "test-agent", result["slug"])

	// _capabilities should be at top level (not nested under agent)
	capObj, ok := result["_capabilities"].(map[string]interface{})
	require.True(t, ok, "_capabilities should be a JSON object at the top level")
	actions, ok := capObj["actions"].([]interface{})
	require.True(t, ok, "actions should be an array")
	assert.Len(t, actions, 2)
	assert.Equal(t, "read", actions[0])
	assert.Equal(t, "update", actions[1])
}

func TestProjectWithCapabilities_JSONStructure(t *testing.T) {
	gwc := ProjectWithCapabilities{
		Project: store.Project{
			ID:   "project-json-1",
			Name: "Test Project",
		},
		Cap: &Capabilities{
			Actions: []string{"read", "manage"},
		},
	}

	data, err := json.Marshal(gwc)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	assert.Equal(t, "project-json-1", result["id"])
	assert.Equal(t, "Test Project", result["name"])

	capObj, ok := result["_capabilities"].(map[string]interface{})
	require.True(t, ok)
	actions, ok := capObj["actions"].([]interface{})
	require.True(t, ok)
	assert.Len(t, actions, 2)
}

func TestWithCapabilities_OmitsWhenNil(t *testing.T) {
	awc := AgentWithCapabilities{
		Agent: store.Agent{
			ID:   "agent-no-cap",
			Name: "No Caps",
		},
	}

	data, err := json.Marshal(awc)
	require.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result))

	_, exists := result["_capabilities"]
	assert.False(t, exists, "_capabilities should be omitted when nil")
}

// TestResourceActions_GCPServiceAccountDeclaresAssign pins the declaration of
// the "assign" action on the GCP service account resource. Nothing enforces it
// yet — the assignment call sites still check ActionRead — but the constant and
// the ResourceActions entry are the insertion point for that gate, and a policy
// author can write against it today.
func TestResourceActions_GCPServiceAccountDeclaresAssign(t *testing.T) {
	assert.Contains(t, ResourceActions["gcp_service_account"], ActionAssign)
	assert.NotContains(t, ScopeActions["gcp_service_account"], ActionAssign,
		"assign is an item-level action; it must not appear in ScopeActions")
}

func TestResourceActions_AgentLifecycleUsesAttachPermission(t *testing.T) {
	assert.Contains(t, ResourceActions["agent"], ActionAttach)
	assert.Contains(t, ResourceActions["agent"], ActionPortAccess)
	for _, action := range []Action{ActionStart, ActionStop, ActionMessage} {
		assert.False(t, slices.Contains(ResourceActions["agent"], action),
			"%s is enforced through ActionAttach today and must not be exposed as an independent capability", action)
	}
}

func TestComputeCapabilities_GCPServiceAccount_AdminSeesAssign(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-sa-assign", "admin-sa@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "gcp_service_account", ID: "sa-1"}

	caps := srv.authzService.ComputeCapabilities(ctx, admin, resource)
	assert.Equal(t, []string{"read", "delete", "verify", "assign"}, caps.Actions)
}

// A policy granting only "read" must not confer "assign" — the two are distinct
// permissions, which is the whole point of declaring the action separately.
func TestComputeCapabilities_GCPServiceAccount_ReadPolicyDoesNotGrantAssign(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-sa-reader"), Email: "sa-reader@test.com", DisplayName: "SA Reader",
		Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreatePolicy(ctx, &store.Policy{
		ID: tid("policy-sa-read"), Name: "SA Read Only", ScopeType: "hub",
		ResourceType: "gcp_service_account", Actions: []string{"read"}, Effect: "allow",
	}))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-sa-read"), PrincipalType: "user", PrincipalID: tid("user-sa-reader"),
	}))

	user := NewAuthenticatedUser(tid("user-sa-reader"), "sa-reader@test.com", "SA Reader", "member", "api")
	caps := srv.authzService.ComputeCapabilities(ctx, user, Resource{Type: "gcp_service_account", ID: "sa-2"})
	assert.Equal(t, []string{"read"}, caps.Actions)
}

func TestComputeCapabilities_UnknownResourceType(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-unk", "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "unknown", ID: "some-id"}

	caps := srv.authzService.ComputeCapabilities(ctx, admin, resource)
	assert.Equal(t, []string{}, caps.Actions)
}

func TestComputeCapabilitiesBatch_EmptyList(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-empty", "admin@example.com", "Admin", "admin", "api")

	caps := srv.authzService.ComputeCapabilitiesBatch(ctx, admin, nil, "agent")
	assert.Len(t, caps, 0)
}

func TestResourceBuilders(t *testing.T) {
	t.Run("agentResource", func(t *testing.T) {
		a := &store.Agent{ID: "a1", OwnerID: "u1", ProjectID: tid("g1"), Labels: map[string]string{"env": "prod"}, Ancestry: []string{"u1"}}
		r := agentResource(a)
		assert.Equal(t, "agent", r.Type)
		assert.Equal(t, "a1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
		assert.Equal(t, "project", r.ParentType)
		assert.Equal(t, tid("g1"), r.ParentID)
		assert.Equal(t, "prod", r.Labels["env"])
		assert.Equal(t, []string{"u1"}, r.Ancestry)
	})

	t.Run("projectResource", func(t *testing.T) {
		g := &store.Project{ID: tid("g1"), OwnerID: "u1"}
		r := projectResource(g)
		assert.Equal(t, "project", r.Type)
		assert.Equal(t, tid("g1"), r.ID)
		assert.Equal(t, "u1", r.OwnerID)
	})

	t.Run("templateResource", func(t *testing.T) {
		tmpl := &store.Template{ID: "t1", OwnerID: "u1"}
		r := templateResource(tmpl)
		assert.Equal(t, "template", r.Type)
		assert.Equal(t, "t1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
	})

	t.Run("harnessConfigResource", func(t *testing.T) {
		hc := &store.HarnessConfig{ID: "hc1", OwnerID: "u1"}
		r := harnessConfigResource(hc)
		assert.Equal(t, "harness_config", r.Type)
		assert.Equal(t, "hc1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
		assert.Empty(t, r.ParentType)
	})

	t.Run("harnessConfigResource project-scoped", func(t *testing.T) {
		hc := &store.HarnessConfig{ID: "hc2", OwnerID: "u1", Scope: store.HarnessConfigScopeProject, ScopeID: "p1"}
		r := harnessConfigResource(hc)
		assert.Equal(t, "harness_config", r.Type)
		assert.Equal(t, "project", r.ParentType)
		assert.Equal(t, "p1", r.ParentID)
	})

	t.Run("groupResource", func(t *testing.T) {
		g := &store.Group{ID: "grp1", OwnerID: "u1"}
		r := groupResource(g)
		assert.Equal(t, "group", r.Type)
		assert.Equal(t, "grp1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
	})

	t.Run("userResource", func(t *testing.T) {
		u := &store.User{ID: "u1"}
		r := userResource(u)
		assert.Equal(t, "user", r.Type)
		assert.Equal(t, "u1", r.ID)
	})

	t.Run("gcpServiceAccountResource project-scoped", func(t *testing.T) {
		sa := &store.GCPServiceAccount{ID: "sa1", CreatedBy: "u1", Scope: store.ScopeProject, ScopeID: tid("p1")}
		r := gcpServiceAccountResource(sa)
		assert.Equal(t, "gcp_service_account", r.Type)
		assert.Equal(t, "sa1", r.ID)
		assert.Equal(t, "u1", r.OwnerID)
		assert.Equal(t, "project", r.ParentType)
		assert.Equal(t, tid("p1"), r.ParentID)
	})

	t.Run("gcpServiceAccountResource hub-scoped has no parent", func(t *testing.T) {
		sa := &store.GCPServiceAccount{ID: "sa2", CreatedBy: "u1", Scope: store.ScopeHub, ScopeID: "hub-instance-1"}
		r := gcpServiceAccountResource(sa)
		assert.Equal(t, "gcp_service_account", r.Type)
		assert.Empty(t, r.ParentType, "hub-scoped SA must not claim a project parent")
		assert.Empty(t, r.ParentID)
	})

	t.Run("gcpServiceAccountResource user-scoped has no parent", func(t *testing.T) {
		sa := &store.GCPServiceAccount{ID: "sa3", CreatedBy: "u1", Scope: store.ScopeUser, ScopeID: "u1"}
		r := gcpServiceAccountResource(sa)
		assert.Empty(t, r.ParentType, "user-scoped SA must not claim a project parent")
		assert.Empty(t, r.ParentID)
	})

	t.Run("gcpServiceAccountResource project-scoped with empty scope ID", func(t *testing.T) {
		sa := &store.GCPServiceAccount{ID: "sa4", CreatedBy: "u1", Scope: store.ScopeProject}
		r := gcpServiceAccountResource(sa)
		assert.Empty(t, r.ParentType, "an empty ScopeID must not become a parent ID")
	})

	t.Run("gcpServiceAccountResource nil", func(t *testing.T) {
		assert.Equal(t, Resource{}, gcpServiceAccountResource(nil))
	})

	t.Run("policyResource", func(t *testing.T) {
		p := &store.Policy{ID: "p1", Labels: map[string]string{"team": "backend"}}
		r := policyResource(p)
		assert.Equal(t, "policy", r.Type)
		assert.Equal(t, "p1", r.ID)
		assert.Equal(t, "backend", r.Labels["team"])
	})
}
