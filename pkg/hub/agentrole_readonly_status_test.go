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
	"bytes"
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

// doReadonlyAgentRequest sends method/path with a JSON body as agentID,
// holding exactly the readonly role's scopes.
func doReadonlyAgentRequest(t *testing.T, srv *Server, agentID, projectID, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	token, err := srv.GetAgentTokenService().GenerateAgentToken(agentID, projectID, ScopesForRole(AgentRoleReadOnly), nil)
	require.NoError(t, err)

	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupReadonlyStatusTest returns a server, the readonly agent from
// setupReadScopeTest, and a second agent in the same project.
func setupReadonlyStatusTest(t *testing.T) (*Server, store.Store, *store.Agent, *store.Agent, *store.Project) {
	t.Helper()
	srv, s, self, project := setupReadScopeTest(t)
	other := &store.Agent{
		ID:        tid("agent-read-scope-other"),
		Slug:      "read-scope-other",
		Name:      "Read Scope Other",
		ProjectID: project.ID,
		Phase:     "running",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), other))
	return srv, s, self, other, project
}

// A readonly agent reports its own status (ptone/scion#7926).
func TestReadonlyAgent_SelfStatusUpdate_Allowed(t *testing.T) {
	srv, s, self, _, project := setupReadonlyStatusTest(t)

	rec := doReadonlyAgentRequest(t, srv, self.ID, project.ID, http.MethodPost,
		"/api/v1/agents/"+self.ID+"/status", map[string]interface{}{"taskSummary": "reading the repo"})
	require.Equal(t, http.StatusOK, rec.Code, "readonly agent must report its own status; got %s", rec.Body.String())

	got, err := s.GetAgent(context.Background(), self.ID)
	require.NoError(t, err)
	assert.Equal(t, "reading the repo", got.TaskSummary)
}

// The status grant is self-only: a readonly agent cannot update another
// agent's status.
func TestReadonlyAgent_OtherAgentStatusUpdate_Forbidden(t *testing.T) {
	srv, s, self, other, project := setupReadonlyStatusTest(t)

	rec := doReadonlyAgentRequest(t, srv, self.ID, project.ID, http.MethodPost,
		"/api/v1/agents/"+other.ID+"/status", map[string]interface{}{"taskSummary": "hijacked"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "got %s", rec.Body.String())

	got, err := s.GetAgent(context.Background(), other.ID)
	require.NoError(t, err)
	assert.Empty(t, got.TaskSummary, "another agent's status must not change")
}

// Adding the status scope widens nothing else: other writes, on the agent
// itself or elsewhere, stay refused for a readonly agent.
func TestReadonlyAgent_OtherWrites_Forbidden(t *testing.T) {
	srv, s, self, other, project := setupReadonlyStatusTest(t)

	cases := []struct {
		name, method, path string
		body               interface{}
	}{
		{"patch own agent", http.MethodPatch, "/api/v1/agents/" + self.ID, map[string]interface{}{"name": "renamed"}},
		{"patch other agent", http.MethodPatch, "/api/v1/agents/" + other.ID, map[string]interface{}{"name": "renamed"}},
		{"stop own agent", http.MethodPost, "/api/v1/agents/" + self.ID + "/stop", map[string]interface{}{}},
		{"stop other agent", http.MethodPost, "/api/v1/agents/" + other.ID + "/stop", map[string]interface{}{}},
		{"delete other agent", http.MethodDelete, "/api/v1/agents/" + other.ID, nil},
		{"create agent", http.MethodPost, "/api/v1/agents", map[string]interface{}{"name": "child", "projectId": project.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReadonlyAgentRequest(t, srv, self.ID, project.ID, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "got %s", rec.Body.String())
		})
	}

	for _, id := range []string{self.ID, other.ID} {
		got, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		assert.NotEqual(t, "renamed", got.Name)
		assert.Equal(t, "running", got.Phase)
	}
}
