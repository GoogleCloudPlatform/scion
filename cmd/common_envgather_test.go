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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	allowEnvSend(t)

	// When the key is available in the local env, non-interactive mode
	// with --send-env gathers and submits it.
	_ = os.Setenv("TEST_SECRET_KEY", "secret-value")
	defer func() { _ = os.Unsetenv("TEST_SECRET_KEY") }()

	// Set up mock Hub server
	projectID := tid("project-1")
	server, captured := newEnvGatherMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
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
			HubHas:   []hubclient.EnvSource{{Key: "GEMINI_API_KEY", Scope: "project"}},
			Needs:    []string{},
		},
	}

	result, err := gatherAndSubmitEnv(context.Background(), nil, tid("project-1"), resp)
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
	_ = os.Unsetenv("KEY_A")
	_ = os.Unsetenv("KEY_B")

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-3"},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  "agent-3",
			Required: []string{"KEY_A", "KEY_B"},
			Needs:    []string{"KEY_A", "KEY_B"},
		},
	}

	_, err := gatherAndSubmitEnv(context.Background(), nil, tid("project-1"), resp)
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
	_ = os.Unsetenv("MISSING_KEY")

	agentID := "agent-cleanup-1"
	projectID := "project-cleanup"
	var deleteCalled bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
			// CreateAgent — return 202 with env-gather requirements
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"projects": []map[string]interface{}{{"id": projectID, "name": "test"}},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = startAgentViaHub(nil, hubCtx, "test-agent", "", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "env-gather failed")
	assert.True(t, deleteCalled, "expected provisioning agent to be deleted on env-gather failure")
}

func TestStartAgentViaHub_GlobalProjectSkipsWorkspaceBootstrap(t *testing.T) {
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
	runtimeBrokerID = tid("broker-1")

	globalDir := t.TempDir()
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte("hub:\n  enabled: true\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "state.yaml"), []byte("syncedAgents: []\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "large-local-file.txt"), []byte("should-not-upload"), 0644))

	projectID := "project-global"
	var captured *hubclient.CreateAgentRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "global",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
			var req hubclient.CreateAgentRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			captured = &req
			_ = json.NewEncoder(w).Encode(&hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{
					ID:                tid("agent-1"),
					Slug:              tid("agent-1"),
					Name:              tid("agent-1"),
					Status:            "running",
					Phase:             "running",
					RuntimeBrokerID:   tid("broker-1"),
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
		Client:      client,
		Endpoint:    server.URL,
		ProjectID:   projectID,
		ProjectPath: settingsPath,
		IsGlobal:    true,
	}

	err = startAgentViaHub(nil, hubCtx, "global-agent", "hello", false, nil)
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodPost && r.URL.Path != "":
			// SubmitEnv endpoint
			var body struct {
				Env map[string]string `json:"env"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body.Env {
				captured[k] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]interface{}{"id": tid("agent-1"), "status": "running"},
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
	projectID := "project-prompt"
	server, captured := newEnvGatherMockHubServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
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
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
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

	_, err := gatherAndSubmitEnv(context.Background(), nil, tid("project-1"), resp)
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
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
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

	_, err := gatherAndSubmitEnv(context.Background(), nil, tid("project-1"), resp)
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
	_ = os.Unsetenv("SECRET_A")
	_ = os.Unsetenv("ENV_B")

	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
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

	_, err := gatherAndSubmitEnv(context.Background(), nil, tid("project-1"), resp)
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
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
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

	_, err := gatherAndSubmitEnv(context.Background(), nil, tid("project-1"), resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MY_SECRET")
}

// An env submit that the hub answers after the hub client's usual timeout
// still succeeds: the submit carries the launch and gets the create
// request's longer timeout. The timeouts are scaled down; the usual one is
// 50ms here, as hubClientTimeout (30s) is in production.
func TestGatherAndSubmitEnv_SlowSubmitOutlastsClientTimeout(t *testing.T) {
	origNonInteractive, origAutoConfirm := nonInteractive, autoConfirm
	origTimeout := hubCreateRequestTimeout
	t.Cleanup(func() {
		nonInteractive, autoConfirm = origNonInteractive, origAutoConfirm
		hubCreateRequestTimeout = origTimeout
	})
	nonInteractive, autoConfirm = true, true
	allowEnvSend(t)
	hubCreateRequestTimeout = 10 * time.Second
	t.Setenv("TEST_SLOW_SUBMIT_KEY", "slow-value")

	projectID := tid("project-1")
	var submits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		submits.Add(1)
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"agent": map[string]interface{}{"id": tid("agent-1"), "status": "running"},
		})
	}))
	t.Cleanup(server.Close)
	client, err := hubclient.New(server.URL, hubclient.WithTimeout(50*time.Millisecond))
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
	resp := &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: tid("agent-1")},
		EnvGather: &hubclient.EnvGatherResponse{
			AgentID:  tid("agent-1"),
			Required: []string{"TEST_SLOW_SUBMIT_KEY"},
			Needs:    []string{"TEST_SLOW_SUBMIT_KEY"},
		},
	}

	result, err := gatherAndSubmitEnv(context.Background(), hubCtx, projectID, resp)
	require.NoError(t, err, "an env submit slower than the client timeout must not be cut off")
	require.NotNil(t, result)
	require.NotNil(t, result.Agent)
	assert.Equal(t, tid("agent-1"), result.Agent.ID)
	assert.Equal(t, int32(1), submits.Load(), "the submit is sent once")
}

// TestStartAgentViaHub_EnvGatherPreservesWarnings verifies that advisory
// warnings returned on the initial 202 env-gather response (such as template
// drift warnings) are carried forward into the finalized CLI text stderr and
// --json output after env-submit completes, without duplication.
func TestStartAgentViaHub_EnvGatherPreservesWarnings(t *testing.T) {
	const driftWarning = `template "my-tmpl" may be out of date: it was imported from github.com/o/r/.scion/templates/my-tmpl at abc1234, and that folder has changed on "main" since. Run "scion templates update my-tmpl" (or Refresh from Source) to update.`

	runCase := func(t *testing.T, jsonMode bool, submitWarnings []string) (stdout, stderr string) {
		t.Helper()
		origNonInteractive := nonInteractive
		origAutoConfirm := autoConfirm
		origOutputFormat := outputFormat
		origTemplateName := templateName
		origHarnessConfigFlag := harnessConfigFlag
		origRuntimeBrokerID := runtimeBrokerID
		t.Cleanup(func() {
			nonInteractive = origNonInteractive
			autoConfirm = origAutoConfirm
			outputFormat = origOutputFormat
			templateName = origTemplateName
			harnessConfigFlag = origHarnessConfigFlag
			runtimeBrokerID = origRuntimeBrokerID
		})

		nonInteractive = true
		autoConfirm = true
		if jsonMode {
			outputFormat = "json"
		} else {
			outputFormat = ""
		}
		templateName = ""
		harnessConfigFlag = "codex"
		runtimeBrokerID = tid("broker-1")
		allowEnvSend(t)
		t.Setenv("TEST_DRIFT_ENV_KEY", "secret-val")

		agentID := tid("agent-drift-1")
		projectID := tid("project-drift")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"id":   projectID,
					"name": "drift-proj",
				})
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(&hubclient.CreateAgentResponse{
					Agent: &hubclient.Agent{
						ID:     agentID,
						Slug:   "drift-agent",
						Name:   "drift-agent",
						Status: "provisioning",
						Phase:  "provisioning",
					},
					Warnings: []string{driftWarning},
					EnvGather: &hubclient.EnvGatherResponse{
						AgentID:  agentID,
						Required: []string{"TEST_DRIFT_ENV_KEY"},
						Needs:    []string{"TEST_DRIFT_ENV_KEY"},
					},
				})
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents/"+agentID+"/env":
				_ = json.NewEncoder(w).Encode(&hubclient.CreateAgentResponse{
					Agent: &hubclient.Agent{
						ID:                agentID,
						Slug:              "drift-agent",
						Name:              "drift-agent",
						Status:            "running",
						Phase:             "running",
						RuntimeBrokerID:   tid("broker-1"),
						RuntimeBrokerName: "scion",
						Created:           time.Now().UTC(),
					},
					Warnings: submitWarnings,
				})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)

		client, err := hubclient.New(server.URL)
		require.NoError(t, err)

		globalDir := t.TempDir()
		settingsPath := filepath.Join(globalDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte("hub:\n  enabled: true\n"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "state.yaml"), []byte("syncedAgents: []\n"), 0644))

		hubCtx := &HubContext{
			Client:      client,
			Endpoint:    server.URL,
			ProjectID:   projectID,
			ProjectPath: settingsPath,
			IsGlobal:    true,
		}

		// Capture stdout and stderr.
		origStdout, origStderr := os.Stdout, os.Stderr
		outR, outW, err := os.Pipe()
		require.NoError(t, err)
		errR, errW, err := os.Pipe()
		require.NoError(t, err)
		os.Stdout, os.Stderr = outW, errW
		t.Cleanup(func() {
			os.Stdout, os.Stderr = origStdout, origStderr
		})

		runErr := startAgentViaHub(nil, hubCtx, "drift-agent", "hello", false, nil)
		_ = outW.Close()
		_ = errW.Close()
		os.Stdout, os.Stderr = origStdout, origStderr

		outBytes, readOutErr := io.ReadAll(outR)
		require.NoError(t, readOutErr)
		errBytes, readErrErr := io.ReadAll(errR)
		require.NoError(t, readErrErr)
		_ = outR.Close()
		_ = errR.Close()

		require.NoError(t, runErr)
		return string(outBytes), string(errBytes)
	}

	t.Run("text mode prints 202 warning once on stderr", func(t *testing.T) {
		_, stderr := runCase(t, false, nil)
		assert.Equal(t, 1, strings.Count(stderr, "Warning: "+driftWarning+"\n"),
			"expected drift warning once on stderr, got: %q", stderr)
	})

	t.Run("json mode carries 202 warning once and deduplicates submit warnings", func(t *testing.T) {
		stdout, _ := runCase(t, true, []string{driftWarning, "extra submit warning"})
		var res ActionResult
		require.NoError(t, json.Unmarshal([]byte(stdout), &res), "stdout: %s", stdout)
		assert.Equal(t, []string{driftWarning, "extra submit warning"}, res.Warnings)
	})
}
