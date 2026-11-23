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
		case "all":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"secrets": []map[string]string{
					{"key": "BOTH_KEY", "type": "file", "target": "/project/path", "scope": "project"},
					{"key": "PROJ_ONLY", "type": "environment", "target": "PROJ_ONLY", "scope": "project"},
					{"key": "AGENT_ONLY", "type": "environment", "target": "AGENT_ONLY", "scope": "agent"},
					{"key": "BOTH_KEY", "type": "file", "target": "/agent/path", "scope": "agent"},
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
	origFetch := fetchProjectSecretMeta
	t.Cleanup(func() {
		agentSecretGetScope = origScope
		fetchProjectSecretMeta = origFetch
	})
	fetchProjectSecretMeta = func(_ *cobra.Command, key string) (*hubclient.Secret, error) {
		return &hubclient.Secret{
			Key:         key,
			Scope:       "project",
			SecretType:  "file",
			Target:      "/project/path",
			Version:     3,
			Description: "project secret metadata",
		}, nil
	}

	// 1. --scope agent resolves agent-only key.
	agentSecretGetScope = "agent"
	out, err := runSecretGetTest(t, "AGENT_ONLY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: AGENT_ONLY")
	assert.Contains(t, out, "Scope:   agent")

	// 2. Unset --scope probes scope=all and resolves agent-only key.
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

	// 4. Unset --scope for a project-only key fetches full project metadata (Version/Description).
	agentSecretGetScope = ""
	out, err = runSecretGetTest(t, "PROJ_ONLY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: PROJ_ONLY")
	assert.Contains(t, out, "Scope:   project")
	assert.Contains(t, out, "Version: 3")
	assert.Contains(t, out, "Description: project secret metadata")

	// 5. --scope project pins to project scope and preserves full project metadata.
	agentSecretGetScope = "project"
	out, err = runSecretGetTest(t, "BOTH_KEY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: BOTH_KEY")
	assert.Contains(t, out, "Scope:   project")
	assert.Contains(t, out, "Target:  /project/path")
	assert.Contains(t, out, "Version: 3")
	assert.Contains(t, out, "Description: project secret metadata")
}

func TestAgentSecretGetMetadata_ExperimentOff(t *testing.T) {
	// Mock returns the Hub's exact experiment-off 400 responses for scope=all
	// and scope=agent (pinned by TestAgentSecrets_ExperimentOffRejectedEverywhere).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/"+secretGetTestAgentID+"/secrets", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		switch r.URL.Query().Get("scope") {
		case "all":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "validation_error",
					"message": `scope must be "project" or "user"`,
				},
			})
		case "agent":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "invalid_request",
					"message": "invalid scope: agent",
				},
			})
		}
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)
	t.Setenv("SCION_AGENT_ID", secretGetTestAgentID)
	origScope := agentSecretGetScope
	origFetch := fetchProjectSecretMeta
	t.Cleanup(func() {
		agentSecretGetScope = origScope
		fetchProjectSecretMeta = origFetch
	})
	fetchProjectSecretMeta = func(_ *cobra.Command, key string) (*hubclient.Secret, error) {
		return &hubclient.Secret{
			Key:         key,
			Scope:       "project",
			SecretType:  "environment",
			Version:     2,
			Description: "fallback project secret",
		}, nil
	}

	// With --scope unset and experiment off, scope=all returns 400 and we fall
	// back cleanly to fetchProjectSecretMeta.
	agentSecretGetScope = ""
	out, err := runSecretGetTest(t, "MY_KEY")
	require.NoError(t, err)
	assert.Contains(t, out, "Secret: MY_KEY")
	assert.Contains(t, out, "Scope:   project")
	assert.Contains(t, out, "Version: 2")
	assert.Contains(t, out, "Description: fallback project secret")

	// With --scope agent and experiment off, return a clear ErrAgentScopeUnavailable error.
	agentSecretGetScope = "agent"
	_, err = runSecretGetTest(t, "MY_KEY")
	require.Error(t, err)
	assert.ErrorIs(t, err, sciontoolhub.ErrAgentScopeUnavailable)
}

func TestHubSecretSet_StdinDash(t *testing.T) {
	var gotKey string
	var gotReq hubclient.SetSecretRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPut && len(r.URL.Path) > len("/api/v1/secrets/"):
			gotKey = r.URL.Path[len("/api/v1/secrets/"):]
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
			_ = json.NewEncoder(w).Encode(hubclient.SetSecretResponse{
				Created: true,
				Secret: &hubclient.Secret{
					Key:        gotKey,
					Scope:      gotReq.Scope,
					SecretType: "environment",
					Version:    1,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	state := saveSecretTestState()
	origAgent := secretAgentScope
	t.Cleanup(func() {
		state.restore()
		secretAgentScope = origAgent
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	projectPath = setupSecretProject(t, home, srv.URL)

	newTestSetCmd := func(stdin string) *cobra.Command {
		c := &cobra.Command{}
		c.Flags().StringVar(&secretScope, "scope", "", "")
		c.Flags().StringVar(&secretProjectScope, "project", "", "")
		c.Flags().StringVar(&secretBrokerScope, "broker", "", "")
		c.Flags().StringVar(&secretAgentScope, "agent", "", "")
		c.Flags().StringVar(&secretType, "type", "", "")
		c.Flags().StringVar(&secretTarget, "target", "", "")
		c.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "")
		c.SetIn(bytes.NewBufferString(stdin))
		return c
	}

	t.Run("trims trailing newline and keeps environment type", func(t *testing.T) {
		secretType = ""
		secretTarget = ""
		c := newTestSetCmd("super-secret-value\r\n")
		require.NoError(t, runSecretSet(c, []string{"MY_SECRET", "-"}))
		assert.Equal(t, "MY_SECRET", gotKey)
		assert.Equal(t, "super-secret-value", gotReq.Value)
		assert.Equal(t, "raw", gotReq.Encoding)
		assert.Equal(t, "", gotReq.Type)
	})

	t.Run("stdin value starting with @ is not treated as file path", func(t *testing.T) {
		secretType = ""
		secretTarget = ""
		c := newTestSetCmd("@not-a-file-password\n")
		require.NoError(t, runSecretSet(c, []string{"AT_SECRET", "-"}))
		assert.Equal(t, "@not-a-file-password", gotReq.Value)
		assert.Equal(t, "raw", gotReq.Encoding)
		assert.Equal(t, "", gotReq.Type)
	})

	t.Run("works with agent scope", func(t *testing.T) {
		secretType = ""
		secretTarget = ""
		c := newTestSetCmd("agent-val\n")
		require.NoError(t, c.Flags().Set("agent", "11111111-1111-1111-1111-111111111111"))
		require.NoError(t, runSecretSet(c, []string{"AGENT_KEY", "-"}))
		assert.Equal(t, "agent", gotReq.Scope)
		assert.Equal(t, "11111111-1111-1111-1111-111111111111", gotReq.ScopeID)
		assert.Equal(t, "agent-val", gotReq.Value)
		assert.Equal(t, "raw", gotReq.Encoding)
	})

	t.Run("rejects empty stdin", func(t *testing.T) {
		c := newTestSetCmd("\n")
		err := runSecretSet(c, []string{"EMPTY_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("rejects oversized stdin", func(t *testing.T) {
		big := bytes.Repeat([]byte("x"), 64*1024+1)
		c := newTestSetCmd(string(big))
		err := runSecretSet(c, []string{"BIG_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds 64KB limit")
	})
}

func TestResolveAgentScopeID_HubWideFallbackWarning(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	const projUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]string{
					{"id": "11111111-1111-1111-1111-111111111111", "slug": "solo-agent", "name": "solo-agent"},
				},
			})
		case "/api/v1/projects/" + projUUID + "/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]string{
					{"id": "22222222-2222-2222-2222-222222222222", "slug": "scoped-agent", "name": "scoped-agent"},
				},
			})
		case "/api/v1/projects/" + projUUID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": projUUID, "slug": "proj-1", "name": "proj-1"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	// No project context -> warns on stderr when falling back to hub-wide resolution.
	var resolvedID string
	var resolveErr error
	stderr := captureStderr(t, func() {
		resolvedID, resolveErr = resolveAgentScopeID(context.Background(), client, &config.Settings{}, false, "solo-agent")
	})
	require.NoError(t, resolveErr)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", resolvedID)
	assert.Contains(t, stderr, "Warning:")
	assert.Contains(t, stderr, "across all accessible projects")

	// Valid project context -> no hub-wide fallback warning.
	settingsWithProj := &config.Settings{
		Hub: &config.HubClientConfig{ProjectID: projUUID},
	}
	stderr = captureStderr(t, func() {
		resolvedID, resolveErr = resolveAgentScopeID(context.Background(), client, settingsWithProj, false, "scoped-agent")
	})
	require.NoError(t, resolveErr)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", resolvedID)
	assert.NotContains(t, stderr, "across all accessible projects")
}

var _ = sciontoolhub.NewClient // keep import explicit
