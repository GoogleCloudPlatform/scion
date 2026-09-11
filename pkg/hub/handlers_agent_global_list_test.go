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
		ID:         tid("tmpl-agent-global"),
		Slug:       "agent-global-tmpl",
		Name:       "Agent Global Template",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Status:     "active",
		Created:    time.Now(),
		Updated:    time.Now(),
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
		ID:         tid("tmpl-unauth"),
		Slug:       "unauth-tmpl",
		Name:       "Unauth Template",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Status:     "active",
		Created:    time.Now(),
		Updated:    time.Now(),
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
		ID:         tid("hc-agent-global"),
		Slug:       "agent-global-hc",
		Name:       "Agent Global HC",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Status:     store.HarnessConfigStatusActive,
		Created:    time.Now(),
		Updated:    time.Now(),
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

// TestAgentListHarnessConfigs_Unauthenticated verifies that an unauthenticated
// caller is rejected with 401.
func TestAgentListHarnessConfigs_Unauthenticated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:         tid("hc-unauth"),
		Slug:       "unauth-hc",
		Name:       "Unauth HC",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Status:     store.HarnessConfigStatusActive,
		Created:    time.Now(),
		Updated:    time.Now(),
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated caller should be rejected")
}
