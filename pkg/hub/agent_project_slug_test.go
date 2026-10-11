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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enrichAgent (with the project passed in or looked up) and enrichAgents
// set the project slug next to the project name.
func TestAgentProjectSlug_Enrich(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("slug-project"), Name: "Slug Project", Slug: "slug-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	passed := store.Agent{ID: "a1", ProjectID: project.ID}
	srv.enrichAgent(ctx, &passed, project, nil)
	assert.Equal(t, "Slug Project", passed.Project)
	assert.Equal(t, "slug-project", passed.ProjectSlug, "enrichAgent with the project")

	looked := store.Agent{ID: "a2", ProjectID: project.ID}
	srv.enrichAgent(ctx, &looked, nil, nil)
	assert.Equal(t, "slug-project", looked.ProjectSlug, "enrichAgent looking the project up")

	many := []store.Agent{{ID: "a3", ProjectID: project.ID}, {ID: "a4"}}
	srv.enrichAgents(ctx, many)
	assert.Equal(t, "Slug Project", many[0].Project)
	assert.Equal(t, "slug-project", many[0].ProjectSlug, "enrichAgents")
	assert.Empty(t, many[1].ProjectSlug, "an agent with no project has no slug")
}

// toCompact copies the project slug, and the compact view omits it when
// empty, like the project name.
func TestAgentProjectSlug_Compact(t *testing.T) {
	item := toCompact(AgentWithCapabilities{Agent: store.Agent{
		ID: "a", ProjectID: "p", Project: "Name", ProjectSlug: "the-slug",
	}})
	assert.Equal(t, "the-slug", item.ProjectSlug)

	b, err := json.Marshal(toCompact(AgentWithCapabilities{Agent: store.Agent{ID: "a"}}))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "projectSlug")
}

// The single-agent GET and the agent list carry projectSlug next to project.
func TestAgentProjectSlug_HTTP(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s
	agent := createSiteAgent(t, s, project, "slug-http", state.PhaseRunning, store.RunIntentRunning)

	type row struct {
		ID          string `json:"id"`
		Project     string `json:"project"`
		ProjectSlug string `json:"projectSlug"`
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got row
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, project.Name, got.Project)
	assert.Equal(t, project.Slug, got.ProjectSlug, "single-agent GET")

	for _, path := range []string{
		"/api/v1/agents?projectId=" + project.ID,
		"/api/v1/agents?sort=updated&limit=10",
		"/api/v1/agents?sort=updated&limit=10&view=compact",
		"/api/v1/agents?view=compact",
		"/api/v1/projects/" + project.ID + "/agents",
		"/api/v1/projects/" + project.ID + "/agents?view=compact",
		"/api/v1/projects/" + project.ID + "/agents?sort=updated&fit=500&view=compact",
	} {
		rec = doRequest(t, srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		var list struct {
			Agents []row `json:"agents"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list), path)
		found := false
		for _, a := range list.Agents {
			if a.ID == agent.ID {
				found = true
				assert.Equal(t, project.Slug, a.ProjectSlug, path)
			}
		}
		assert.True(t, found, "%s: agent missing from the list", path)
	}
}

// The project agent list read with an agent token carries projectSlug.
func TestAgentProjectSlug_AgentJWTList(t *testing.T) {
	f := sortedListSetup(t)
	self := f.createAgent(t, "slug-jwt", string(state.PhaseRunning), nil)
	tok := f.agentJWTFor(t, self.ID)

	for _, query := range []string{"sort=updated&fit=500", "sort=updated&fit=500&view=compact"} {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.listPath(query), nil, tok)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", query, rec.Body.String())
		var list struct {
			Agents []struct {
				ID          string `json:"id"`
				ProjectSlug string `json:"projectSlug"`
			} `json:"agents"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list), query)
		require.Len(t, list.Agents, 1, query)
		assert.Equal(t, f.project.Slug, list.Agents[0].ProjectSlug, query)
	}
}
