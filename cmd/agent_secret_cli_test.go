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

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSecretScope_AgentFlag(t *testing.T) {
	state := saveSecretTestState()
	origAgent := secretAgentScope
	t.Cleanup(func() {
		state.restore()
		secretAgentScope = origAgent
	})

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&secretScope, "scope", "", "")
	cmd.Flags().StringVar(&secretProjectScope, "project", "", "")
	cmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
	cmd.Flags().StringVar(&secretAgentScope, "agent", "", "")

	require.NoError(t, cmd.Flags().Set("agent", "my-agent"))
	target, err := resolveSecretScope(cmd, &config.Settings{})
	require.NoError(t, err)
	assert.Equal(t, "agent", target.scope)
	assert.Equal(t, "my-agent", target.id)

	// Mutual exclusivity: --agent + --project fails.
	require.NoError(t, cmd.Flags().Set("project", "my-proj"))
	_, err = resolveSecretScope(cmd, &config.Settings{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify more than one")
}

func TestHubSecretClearCmd_HasDeleteAlias(t *testing.T) {
	assert.Contains(t, hubSecretClearCmd.Aliases, "delete")
}

func TestAgentSecretGetValue_WithScopeFlag(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key":    "API_KEY",
			"value":  base64.StdEncoding.EncodeToString([]byte("scoped-secret-val")),
			"type":   "environment",
			"target": "API_KEY",
			"scope":  "agent",
		})
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)
	origScope := agentSecretGetScope
	t.Cleanup(func() { agentSecretGetScope = origScope })

	agentSecretGetValue = true
	agentSecretGetScope = "agent"

	out, err := runSecretGetTest(t, "API_KEY")
	require.NoError(t, err)
	assert.Equal(t, "scoped-secret-val", out)
	assert.Equal(t, "scope=agent", gotQuery)
}

func TestAgentSecretList_InsideAgentShowsScopeColumn(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/"+secretGetTestAgentID+"/secrets", r.URL.Path)
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secrets": []map[string]string{
				{"key": "AGENT_KEY", "type": "environment", "target": "AGENT_KEY", "scope": "agent"},
				{"key": "PROJ_KEY", "type": "file", "target": "~/.ssh/id", "scope": "project"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)
	origListScope := agentSecretListScope
	t.Cleanup(func() { agentSecretListScope = origListScope })
	agentSecretListScope = ""

	c := &cobra.Command{}
	c.SetContext(context.Background())
	var out bytes.Buffer
	c.SetOut(&out)
	require.NoError(t, runAgentSecretList(c, nil))

	assert.Equal(t, "scope=all", gotQuery)
	text := out.String()
	assert.Contains(t, text, "SCOPE")
	assert.Contains(t, text, "AGENT_KEY")
	assert.Contains(t, text, "agent")
	assert.Contains(t, text, "PROJ_KEY")
	assert.Contains(t, text, "project")
}

func TestAgentSecretList_ExperimentOffFallsBackToProjectList(t *testing.T) {
	// When hub.agent_secrets is off, /api/v1/agents/{id}/secrets?scope=all
	// returns HTTP 400 (ErrAgentScopeUnavailable) and scion secret list falls
	// back to the pre-existing project-scoped /api/v1/secrets endpoint.
	var agentEndpointHit, projectEndpointHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/agents/" + secretGetTestAgentID + "/secrets":
			agentEndpointHit = true
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "validation_error", "message": "scope must be \"project\" or \"user\""},
			})
		case "/api/v1/secrets":
			projectEndpointHit = true
			assert.Equal(t, "project", r.URL.Query().Get("scope"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"secrets": []map[string]any{
					{"key": "PROJ_ONLY", "type": "environment", "version": 1},
				},
				"scope": "project",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	state := saveSecretTestState()
	t.Cleanup(state.restore)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_PROJECT_ID", "test-project-id")
	projectPath = setupSecretProject(t, home, srv.URL)

	useSecretGetHub(t, srv)
	origListScope := agentSecretListScope
	t.Cleanup(func() { agentSecretListScope = origListScope })
	agentSecretListScope = ""

	c := &cobra.Command{}
	c.SetContext(context.Background())
	out := captureStdout(t, func() {
		require.NoError(t, runAgentSecretList(c, nil))
	})
	assert.True(t, agentEndpointHit, "expected probe of agent secrets endpoint")
	assert.True(t, projectEndpointHit, "expected fallback to project secrets endpoint when experiment is off")
	assert.Contains(t, out, "VERSION")
	assert.Contains(t, out, "UPDATED")
	assert.Contains(t, out, "PROJ_ONLY")
	assert.NotContains(t, out, "SCOPE")
}

func TestResolveAgentScopeID_PaginatesAndDetectsAmbiguity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/agents" && r.URL.Query().Get("cursor") == "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents":     []map[string]string{{"id": "11111111-1111-1111-1111-111111111111", "slug": "dup-agent", "name": "dup-agent"}},
				"nextCursor": "page-2",
			})
		case r.URL.Path == "/api/v1/agents" && r.URL.Query().Get("cursor") == "page-2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]string{
					{"id": "22222222-2222-2222-2222-222222222222", "slug": "page2-agent", "name": "page2-agent"},
					{"id": "33333333-3333-3333-3333-333333333333", "slug": "dup-agent", "name": "dup-agent"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	// Finds agent on page 2.
	id, err := resolveAgentScopeID(context.Background(), client, &config.Settings{}, false, "page2-agent")
	require.NoError(t, err)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", id)

	// Detects ambiguity across pages.
	_, err = resolveAgentScopeID(context.Background(), client, &config.Settings{}, false, "dup-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches multiple agents")
}

func TestAgentSecretList_InvalidScopeFlag(t *testing.T) {
	origListScope := agentSecretListScope
	t.Cleanup(func() { agentSecretListScope = origListScope })
	agentSecretListScope = "invalid"

	c := &cobra.Command{}
	c.SetContext(context.Background())
	err := runAgentSecretList(c, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --scope")
}

func TestAgentSecretGetMetadata_InsideAgentScopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/"+secretGetTestAgentID+"/secrets", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("scope") {
		case "agent":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"secrets": []map[string]string{
					{"key": "AGENT_ONLY", "type": "environment", "target": "AGENT_ONLY", "scope": "agent"},
					{"key": "BOTH_KEY", "type": "file", "target": "/agent/path", "scope": "agent"},
				},
			})
		case "project":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"secrets": []map[string]string{
					{"key": "BOTH_KEY", "type": "file", "target": "/project/path", "scope": "project"},
					{"key": "PROJ_ONLY", "type": "environment", "target": "PROJ_ONLY", "scope": "project"},
				},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)
	t.Setenv("SCION_AGENT_ID", secretGetTestAgentID)
	origScope := agentSecretGetScope
	t.Cleanup(func() { agentSecretGetScope = origScope })

	// 1. --scope agent resolves agent-only key.
	agentSecretGetScope = "agent"
	out, err := runSecretGetTest(t, "AGENT_ONLY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: AGENT_ONLY")
	assert.Contains(t, out, "Scope:   agent")

	// 2. Unset --scope resolves agent-only key via agent scope.
	agentSecretGetScope = ""
	out, err = runSecretGetTest(t, "AGENT_ONLY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: AGENT_ONLY")
	assert.Contains(t, out, "Scope:   agent")

	// 3. Unset --scope for a key present at both scopes prefers agent (matching --value).
	agentSecretGetScope = ""
	out, err = runSecretGetTest(t, "BOTH_KEY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: BOTH_KEY")
	assert.Contains(t, out, "Scope:   agent")
	assert.Contains(t, out, "Target:  /agent/path")

	// 4. --scope project pins to project scope even when key exists at agent scope.
	agentSecretGetScope = "project"
	out, err = runSecretGetTest(t, "BOTH_KEY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: BOTH_KEY")
	assert.Contains(t, out, "Scope:   project")
	assert.Contains(t, out, "Target:  /project/path")
}

var _ = sciontoolhub.NewClient // keep import explicit
