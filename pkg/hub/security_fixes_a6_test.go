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
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// HIGH-1: listGroups — unauthenticated requests must NOT return all groups
// =============================================================================

func TestListGroups_UnauthenticatedReturnsEmptyList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed groups that should not be visible to unauthenticated callers.
	for i := 0; i < 3; i++ {
		slug := tid("unauthz-group-" + string(rune('a'+i)))
		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID:        slug,
			Name:      "Group " + string(rune('A'+i)),
			Slug:      slug,
			GroupType: store.GroupTypeExplicit,
		}))
	}

	// Make an unauthenticated request (no identity in context).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	rec := httptest.NewRecorder()
	srv.listGroups(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListGroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Groups, "unauthenticated caller must NOT see any groups")
	assert.Equal(t, 0, resp.TotalCount, "unauthenticated caller totalCount must be 0")
}

// =============================================================================
// HIGH-2: listProjects — unauthenticated requests must NOT return all projects
// =============================================================================

func TestListProjects_UnauthenticatedReturnsEmptyList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed projects that should not be visible to unauthenticated callers.
	for i := 0; i < 3; i++ {
		slug := tid("unauthz-project-" + string(rune('a'+i)))
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID:   slug,
			Name: "Project " + string(rune('A'+i)),
			Slug: slug,
		}))
	}

	// Make an unauthenticated request (no identity in context).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	srv.listProjects(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Projects, "unauthenticated caller must NOT see any projects")
	assert.Equal(t, 0, resp.TotalCount, "unauthenticated caller totalCount must be 0")
}

// =============================================================================
// MEDIUM-1: setHubInjectedSkills — AgentIdentity should get 403, not 401
// =============================================================================

func TestSetHubInjectedSkills_AgentIdentityGetsForbidden(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	// Create an agent identity (authenticated, but not a UserIdentity).
	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "agent-123"},
		ProjectID: "some-project",
	}}

	body := `{"user_defined": []}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/hub/settings/injected-skills",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(ctx, agentIdent))

	rec := httptest.NewRecorder()
	srv.setHubInjectedSkills(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"authenticated non-user identity must get 403 Forbidden, not 401 Unauthorized")
}

func TestSetHubInjectedSkills_UnauthenticatedGetsUnauthorized(t *testing.T) {
	srv, _ := testServer(t)

	body := `{"user_defined": []}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/hub/settings/injected-skills",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.setHubInjectedSkills(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated caller must get 401 Unauthorized")
}

// =============================================================================
// MEDIUM-2: handleSetTemplate — AgentIdentity should get 403, not 401
// =============================================================================

func TestHandleSetTemplate_AgentIdentityGetsForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project for the handler to find.
	project := &store.Project{
		ID:        tid("template-agent-test-project"),
		Name:      "Template Agent Test",
		Slug:      "template-agent-test",
		CreatedBy: "some-user",
		Created:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create an agent identity (authenticated, but not a UserIdentity).
	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "agent-456"},
		ProjectID: project.ID,
	}}

	body := `{"isTemplate": true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/set-template",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(ctx, agentIdent))

	rec := httptest.NewRecorder()
	srv.handleSetTemplate(rec, req, project.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"authenticated non-user identity must get 403 Forbidden, not 401 Unauthorized")
}

func TestHandleSetTemplate_UnauthenticatedGetsUnauthorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        tid("template-unauth-test-project"),
		Name:      "Template Unauth Test",
		Slug:      "template-unauth-test",
		CreatedBy: "some-user",
		Created:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	body := `{"isTemplate": true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/set-template",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.handleSetTemplate(rec, req, project.ID)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated caller must get 401 Unauthorized")
}
