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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentListTemplates_GlobalVisibility verifies that an agent with
// ScopeProjectRead can list global templates. Before the fix, global templates
// were parentless resources that could not match project-scoped agent bindings
// in AuthorizeReadBatch, causing agents to see zero results.
func TestAgentListTemplates_GlobalVisibility(t *testing.T) {
	srv, s, agent, _ := setupReadScopeTest(t)
	ctx := context.Background()

	// Create a global template.
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID:      tid("tmpl-agent-global"),
		Slug:    "agent-global-tmpl",
		Name:    "Agent Global Template",
		Harness: "claude",
		Scope:   "global",
		Status:  "active",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	// Agent with ScopeProjectRead should see the global template.
	scopes := ScopesForRole(AgentRoleBaseline)
	rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, "/api/v1/templates", scopes)
	assert.Equal(t, http.StatusOK, rec.Code,
		"agent with ScopeProjectRead should get 200; got %d: %s", rec.Code, rec.Body.String())

	var resp ListTemplatesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, len(resp.Templates), 1,
		"agent should see at least 1 global template, got %d", len(resp.Templates))

	found := false
	for _, tmpl := range resp.Templates {
		if tmpl.ID == tid("tmpl-agent-global") {
			found = true
			break
		}
	}
	assert.True(t, found, "agent should see the global template tmpl-agent-global")
}

// TestAgentListTemplates_NoReadScope_Forbidden verifies that an agent without
// ScopeProjectRead is rejected by checkAgentReadScope before reaching the
// list handler.
func TestAgentListTemplates_NoReadScope_Forbidden(t *testing.T) {
	srv, _, agent, _ := setupReadScopeTest(t)

	// Token with only ScopeAgentStatusUpdate — no ScopeProjectRead.
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}
	rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, "/api/v1/templates", scopes)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"agent without ScopeProjectRead should be forbidden; got %d: %s", rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project:read",
		"error should mention the missing scope")
}

// TestAgentListTemplates_Unauthenticated verifies that an unauthenticated
// caller is rejected with 401.
func TestAgentListTemplates_Unauthenticated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID:      tid("tmpl-unauth"),
		Slug:    "unauth-tmpl",
		Name:    "Unauth Template",
		Harness: "claude",
		Scope:   "global",
		Status:  "active",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	// No auth token at all — should be rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated caller should be rejected")
}

// TestAgentListHarnessConfigs_GlobalVisibility verifies that an agent with
// ScopeProjectRead can list global harness configs.
func TestAgentListHarnessConfigs_GlobalVisibility(t *testing.T) {
	srv, s, agent, _ := setupReadScopeTest(t)
	ctx := context.Background()

	// Create a global harness config.
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      tid("hc-agent-global"),
		Slug:    "agent-global-hc",
		Name:    "Agent Global HC",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}))

	// Agent with ScopeProjectRead should see the global harness config.
	scopes := ScopesForRole(AgentRoleBaseline)
	rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, "/api/v1/harness-configs", scopes)
	assert.Equal(t, http.StatusOK, rec.Code,
		"agent with ScopeProjectRead should get 200; got %d: %s", rec.Code, rec.Body.String())

	var resp ListHarnessConfigsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, len(resp.HarnessConfigs), 1,
		"agent should see at least 1 global harness config, got %d", len(resp.HarnessConfigs))

	found := false
	for _, hc := range resp.HarnessConfigs {
		if hc.ID == tid("hc-agent-global") {
			found = true
			break
		}
	}
	assert.True(t, found, "agent should see the global harness config hc-agent-global")
}

// TestAgentListHarnessConfigs_NoReadScope_Forbidden verifies that an agent
// without ScopeProjectRead is rejected.
func TestAgentListHarnessConfigs_NoReadScope_Forbidden(t *testing.T) {
	srv, _, agent, _ := setupReadScopeTest(t)

	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}
	rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, "/api/v1/harness-configs", scopes)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"agent without ScopeProjectRead should be forbidden; got %d: %s", rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project:read",
		"error should mention the missing scope")
}

// TestAgentGetTemplate_GlobalVisibility verifies that an agent with
// ScopeProjectRead can GET a global template by ID, matching what it already
// sees in list (TestAgentListTemplates_GlobalVisibility) — list and detail
// must agree.
func TestAgentGetTemplate_GlobalVisibility(t *testing.T) {
	srv, s, agent, _ := setupReadScopeTest(t)
	ctx := context.Background()

	tpl := &store.Template{
		ID: tid("tmpl-agent-get-global"), Slug: "agent-get-global-tmpl", Name: "Agent Get Global Template",
		Harness: "claude", Scope: store.TemplateScopeGlobal, Status: "active", Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateTemplate(ctx, tpl))

	scopes := ScopesForRole(AgentRoleBaseline)
	rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, "/api/v1/templates/"+tpl.ID, scopes)
	assert.Equal(t, http.StatusOK, rec.Code, "agent should GET a global template; got %d: %s", rec.Code, rec.Body.String())
}

// TestAgentGetHarnessConfig_GlobalVisibility is the harness-config twin of
// TestAgentGetTemplate_GlobalVisibility.
func TestAgentGetHarnessConfig_GlobalVisibility(t *testing.T) {
	srv, s, agent, _ := setupReadScopeTest(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID: tid("hc-agent-get-global"), Slug: "agent-get-global-hc", Name: "Agent Get Global HC",
		Harness: "claude", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	scopes := ScopesForRole(AgentRoleBaseline)
	rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, "/api/v1/harness-configs/"+hc.ID, scopes)
	assert.Equal(t, http.StatusOK, rec.Code, "agent should GET a global harness config; got %d: %s", rec.Code, rec.Body.String())
}

// ============================================================================
// ptone/scion#1916 follow-up (Q1): removing hasCatalogWideListAccess's agent
// branch closes the wide-open list, but list must also agree with GET (which
// already 404s a same-project agent on another user's user-scoped entry) for
// every filter combination a caller might use to route around a plain list.
// ============================================================================

func TestAgentListTemplates_UserScopeExcluded(t *testing.T) {
	srv, s, agent, _ := setupReadScopeTest(t)
	ctx := context.Background()

	otherUserID := tid("agent-list-other-user")
	private := &store.Template{
		ID: tid("tmpl-agent-list-user-scope"), Slug: "agent-list-private-user-tmpl", Name: "Private User Template",
		Harness: "claude", Scope: store.TemplateScopeUser, ScopeID: otherUserID, OwnerID: otherUserID,
		Status: "active", Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateTemplate(ctx, private))

	scopes := ScopesForRole(AgentRoleBaseline)
	cases := []struct {
		name string
		path string
	}{
		{"plain", "/api/v1/templates"},
		{"filtered by name", "/api/v1/templates?name=agent-list-private-user-tmpl"},
		{"filtered by scopeId", "/api/v1/templates?scopeId=" + otherUserID},
		{"searched", "/api/v1/templates?search=Private+User"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, tc.path, scopes)
			require.Equal(t, http.StatusOK, rec.Code, "got: %s", rec.Body.String())

			var resp ListTemplatesResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			for _, tpl := range resp.Templates {
				assert.NotEqual(t, private.ID, tpl.ID,
					"a same-project agent must not see another user's user-scoped template in list (%s)", tc.name)
			}
		})
	}
}

func TestAgentListHarnessConfigs_UserScopeExcluded(t *testing.T) {
	srv, s, agent, _ := setupReadScopeTest(t)
	ctx := context.Background()

	otherUserID := tid("agent-list-hc-other-user")
	private := &store.HarnessConfig{
		ID: tid("hc-agent-list-user-scope"), Slug: "agent-list-private-user-hc", Name: "Private User HC",
		Harness: "claude", Scope: store.HarnessConfigScopeUser, ScopeID: otherUserID, OwnerID: otherUserID,
		Status: store.HarnessConfigStatusActive, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, private))

	scopes := ScopesForRole(AgentRoleBaseline)
	cases := []struct {
		name string
		path string
	}{
		{"plain", "/api/v1/harness-configs"},
		{"filtered by name", "/api/v1/harness-configs?name=agent-list-private-user-hc"},
		{"filtered by scopeId", "/api/v1/harness-configs?scopeId=" + otherUserID},
		{"searched", "/api/v1/harness-configs?search=Private+User"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, agent.ProjectID, tc.path, scopes)
			require.Equal(t, http.StatusOK, rec.Code, "got: %s", rec.Body.String())

			var resp ListHarnessConfigsResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			for _, hc := range resp.HarnessConfigs {
				assert.NotEqual(t, private.ID, hc.ID,
					"a same-project agent must not see another user's user-scoped harness config in list (%s)", tc.name)
			}
		})
	}
}

// TestAgentListHarnessConfigs_Unauthenticated verifies that an unauthenticated
// caller is rejected with 401.
func TestAgentListHarnessConfigs_Unauthenticated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      tid("hc-unauth"),
		Slug:    "unauth-hc",
		Name:    "Unauth HC",
		Harness: "claude",
		Scope:   "global",
		Status:  store.HarnessConfigStatusActive,
		Created: time.Now(),
		Updated: time.Now(),
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated caller should be rejected")
}
