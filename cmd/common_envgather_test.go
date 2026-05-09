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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatherAndSubmitEnv_NonInteractiveGathersFromLocalEnv(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
	}()

	nonInteractive = true
	autoConfirm = true // --non-interactive implies --yes

	// When the key is available in the local env, non-interactive mode
	// should gather and submit it automatically (same as --yes).
	os.Setenv("TEST_SECRET_KEY", "secret-value")
	defer os.Unsetenv("TEST_SECRET_KEY")

	// Set up mock Hub server
	projectID := "grove-1"
	server, captured := newEnvGatherMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-1"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-1",
			Required: []string{"TEST_SECRET_KEY"},
			Needs:    []string{"TEST_SECRET_KEY"},
		},
	}

	result, err := gatherAndSubmitEnv(context.Background(), hubCtx, projectID, resp)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the key was submitted to the Hub
	assert.Equal(t, "secret-value", (*captured)["TEST_SECRET_KEY"])
}

func TestGatherAndSubmitEnv_NonInteractiveAllowsWhenAllSatisfied(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
	}()

	nonInteractive = true
	autoConfirm = true

	// When Needs is empty (Hub/Broker satisfied everything), non-interactive
	// should succeed.
	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-2"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-2",
			Required: []string{"GEMINI_API_KEY"},
			HubHas:   []hubclient.EnvSource{{Key: "GEMINI_API_KEY", Scope: "grove"}},
			Needs:    []string{},
		},
	}

	result, err := gatherAndSubmitEnv(context.Background(), nil, "grove-1", resp)
	require.NoError(t, err)
	// Should return the original response since no env was gathered
	assert.Equal(t, resp, result)
}

func TestGatherAndSubmitEnv_NonInteractiveMultipleKeysMissing(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
	}()

	nonInteractive = true
	autoConfirm = true
	outputFormat = "json" // Suppress stderr output for cleaner test

	// Keys are not in the local environment, so they can't be satisfied
	os.Unsetenv("KEY_A")
	os.Unsetenv("KEY_B")

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-3"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-3",
			Required: []string{"KEY_A", "KEY_B"},
			Needs:    []string{"KEY_A", "KEY_B"},
		},
	}

	_, err := gatherAndSubmitEnv(context.Background(), nil, "grove-1", resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot satisfy required environment variables")
	assert.Contains(t, err.Error(), "KEY_A")
	assert.Contains(t, err.Error(), "KEY_B")
}

// TestStartAgentViaHub_EnvGatherFailureCleansUp verifies that when env-gather
// cannot satisfy required variables, the provisioning agent is deleted on the Hub.
func TestStartAgentViaHub_EnvGatherFailureCleansUp(t *testing.T) {
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
	}()

	nonInteractive = true
	autoConfirm = true
	outputFormat = "json"

	// Keys not in local env so env-gather must fail
	os.Unsetenv("MISSING_KEY")

	agentID := "agent-cleanup-1"
	projectID := "grove-cleanup"
	var deleteCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/groves/"+projectID+"/agents":
			// CreateAgent — return 202 with env-gather requirements
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{"id": agentID, "name": "test-agent", "status": "provisioning"},
				"envGather": map[string]interface{}{
					"agentId":  agentID,
					"required": []string{"MISSING_KEY"},
					"needs":    []string{"MISSING_KEY"},
				},
			})

		case r.Method == http.MethodDelete:
			// Agent delete endpoint — record that cleanup happened
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groves":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves": []map[string]interface{}{{"id": projectID, "name": "test"}},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = startAgentViaHub(hubCtx, "test-agent", "", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env-gather failed")
	assert.True(t, deleteCalled, "expected provisioning agent to be deleted on env-gather failure")
}

func TestStartAgentViaHub_GlobalGroveSkipsWorkspaceBootstrap(t *testing.T) {
	origOutputFormat := outputFormat
	origTemplateName := templateName
	origHarnessConfigFlag := harnessConfigFlag
	origRuntimeBrokerID := runtimeBrokerID
	defer func() {
		outputFormat = origOutputFormat
		templateName = origTemplateName
		harnessConfigFlag = origHarnessConfigFlag
		runtimeBrokerID = origRuntimeBrokerID
	}()

	outputFormat = "json"
	templateName = ""
	harnessConfigFlag = "codex"
	runtimeBrokerID = "broker-1"

	globalDir := t.TempDir()
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte("hub:\n  enabled: true\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "state.yaml"), []byte("syncedAgents: []\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "large-local-file.txt"), []byte("should-not-upload"), 0644))

	projectID := "grove-global"
	var captured *hubclient.CreateAgentRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groves/"+projectID:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "global",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/groves/"+projectID+"/agents":
			var req hubclient.CreateAgentRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			captured = &req
			json.NewEncoder(w).Encode(&hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{
					ID:                "agent-1",
					Slug:              "agent-1",
					Name:              "agent-1",
					Status:            "running",
					Phase:             "running",
					RuntimeBrokerID:   "broker-1",
					RuntimeBrokerName: "scion",
					Created:           time.Now().UTC(),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID:   projectID,
		ProjectPath: settingsPath,
		IsGlobal:  true,
	}

	err = startAgentViaHub(hubCtx, "global-agent", "hello", false, nil)
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Empty(t, captured.WorkspaceFiles)
}

// newEnvGatherMockHubServer creates a mock Hub server that handles the SubmitEnv
// endpoint and captures the submitted environment variables.
func newEnvGatherMockHubServer(t *testing.T, projectID string) (*httptest.Server, *map[string]string) {
	t.Helper()
	captured := make(map[string]string)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodPost && r.URL.Path != "":
			// SubmitEnv endpoint
			var body struct {
				Env map[string]string `json:"env"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body.Env {
				captured[k] = v
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{"id": "agent-1", "status": "running"},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &captured
}

func TestGatherAndSubmitEnv_InteractiveSecretPrompt(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	origReadSecret := readSecretFunc
	origIsTerminal := isInteractiveTerminal
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
		readSecretFunc = origReadSecret
		isInteractiveTerminal = origIsTerminal
	}()

	nonInteractive = false
	autoConfirm = true // skip confirmation prompt
	outputFormat = "json"

	// Override isInteractiveTerminal to simulate interactive terminal in test
	isInteractiveTerminal = func() bool { return true }

	// Override readSecretFunc to return a test value
	readSecretFunc = func(fd int) ([]byte, error) {
		return []byte("test-secret-value"), nil
	}

	// Set up mock Hub server
	projectID := "grove-prompt"
	server, captured := newEnvGatherMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-1"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-1",
			Required: []string{"MY_SECRET"},
			Needs:    []string{"MY_SECRET"},
			SecretInfo: map[string]hubclient.SecretKeyInfo{
				"MY_SECRET": {
					Description: "A secret key",
					Source:      "settings",
				},
			},
		},
	}

	result, err := gatherAndSubmitEnv(context.Background(), hubCtx, projectID, resp)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Verify the secret was submitted to the Hub
	assert.Equal(t, "test-secret-value", (*captured)["MY_SECRET"])
}

func TestGatherAndSubmitEnv_FileSecretShowsGuidance(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
	}()

	nonInteractive = false
	autoConfirm = true
	outputFormat = "json" // suppress stderr

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-1"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-1",
			Required: []string{"FILE_CERT"},
			Needs:    []string{"FILE_CERT"},
			SecretInfo: map[string]hubclient.SecretKeyInfo{
				"FILE_CERT": {
					Description: "TLS certificate file",
					Source:      "template",
					Type:        "file",
				},
			},
		},
	}

	_, err := gatherAndSubmitEnv(context.Background(), nil, "grove-1", resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FILE_CERT")
}

func TestGatherAndSubmitEnv_MixedSecretAndEnvKeys(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
	}()

	nonInteractive = false
	autoConfirm = true
	outputFormat = "json"

	// ENV_ONLY is not in SecretInfo → it's an env-only key that can't be prompted
	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-1"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-1",
			Required: []string{"ENV_ONLY", "SECRET_KEY"},
			Needs:    []string{"ENV_ONLY", "SECRET_KEY"},
			SecretInfo: map[string]hubclient.SecretKeyInfo{
				"SECRET_KEY": {
					Description: "A secret",
					Source:      "settings",
				},
			},
		},
	}

	_, err := gatherAndSubmitEnv(context.Background(), nil, "grove-1", resp)
	require.Error(t, err)
	// Should fail because ENV_ONLY is not secret-eligible and not in local env
	assert.Contains(t, err.Error(), "ENV_ONLY")
}

func TestGatherAndSubmitEnv_NonInteractiveSecretsMissing(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
	}()

	nonInteractive = true
	autoConfirm = true
	outputFormat = "json" // suppress stderr

	// Neither key is in local env; SECRET_A is secret-eligible but can't be
	// prompted in non-interactive mode, ENV_B is env-only.
	os.Unsetenv("SECRET_A")
	os.Unsetenv("ENV_B")

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-1"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-1",
			Required: []string{"SECRET_A", "ENV_B"},
			Needs:    []string{"SECRET_A", "ENV_B"},
			SecretInfo: map[string]hubclient.SecretKeyInfo{
				"SECRET_A": {
					Description: "A secret key",
					Source:      "settings",
				},
			},
		},
	}

	_, err := gatherAndSubmitEnv(context.Background(), nil, "grove-1", resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot satisfy required environment variables")
}

func TestGatherAndSubmitEnv_InteractiveSecretEmptyInput(t *testing.T) {
	// Save and restore package-level state
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	origOutputFormat := outputFormat
	origReadSecret := readSecretFunc
	origIsTerminal := isInteractiveTerminal
	defer func() {
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
		outputFormat = origOutputFormat
		readSecretFunc = origReadSecret
		isInteractiveTerminal = origIsTerminal
	}()

	nonInteractive = false
	autoConfirm = true
	outputFormat = "json"

	// Override isInteractiveTerminal to simulate interactive terminal in test
	isInteractiveTerminal = func() bool { return true }

	// Override readSecretFunc to return empty (user pressed enter without typing)
	readSecretFunc = func(fd int) ([]byte, error) {
		return []byte(""), nil
	}

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-1"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-1",
			Required: []string{"MY_SECRET"},
			Needs:    []string{"MY_SECRET"},
			SecretInfo: map[string]hubclient.SecretKeyInfo{
				"MY_SECRET": {
					Description: "A secret key",
					Source:      "settings",
				},
			},
		},
	}

	_, err := gatherAndSubmitEnv(context.Background(), nil, "grove-1", resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MY_SECRET")
}
