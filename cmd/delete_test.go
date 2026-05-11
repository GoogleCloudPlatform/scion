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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteTestState captures and restores package-level vars for test isolation.
type deleteTestState struct {
	home           string
	projectPath      string
	preserveBranch bool
	noHub          bool
	autoConfirm    bool
	deleteStopped  bool
}

func saveDeleteTestState() deleteTestState {
	return deleteTestState{
		home:           os.Getenv("HOME"),
		projectPath:      projectPath,
		preserveBranch: preserveBranch,
		noHub:          noHub,
		autoConfirm:    autoConfirm,
		deleteStopped:  deleteStopped,
	}
}

func (s deleteTestState) restore() {
	os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	preserveBranch = s.preserveBranch
	noHub = s.noHub
	autoConfirm = s.autoConfirm
	deleteStopped = s.deleteStopped
}

// createAgentDir creates a minimal agent directory at <groveDir>/agents/<name>
// to simulate a locally provisioned agent.
func createAgentDir(t *testing.T, groveDir, name string) string {
	t.Helper()
	agentDir := filepath.Join(groveDir, "agents", name)
	require.NoError(t, os.MkdirAll(agentDir, 0755))
	// Write a marker file so the directory isn't empty
	require.NoError(t, os.WriteFile(
		filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness":"claude"}`),
		0644,
	))
	return agentDir
}

// newDeleteMockHubServer creates a mock Hub server that handles grove-scoped
// agent DELETE requests. Returns the server and a pointer to a slice that
// records which agent names were deleted.
func newDeleteMockHubServer(t *testing.T, projectID string) (*httptest.Server, *[]string) {
	t.Helper()
	var deletedAgents []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodDelete:
			// Extract agent name from path: /api/v1/groves/<projectID>/agents/<agentName>
			prefix := "/api/v1/groves/" + projectID + "/agents/"
			agentName := r.URL.Path[len(prefix):]
			deletedAgents = append(deletedAgents, agentName)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &deletedAgents
}

func TestDeleteAgentLocal_NonExistentAgentReturnsError(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	noHub = true

	// Set up grove directory without any agent
	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(groveDir, "agents"), 0755))
	projectPath = groveDir

	err := deleteAgentLocal("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteAgentLocal_ExistingAgentSucceeds(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	noHub = true
	preserveBranch = true

	// Set up grove directory with an agent
	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(groveDir, "agents"), 0755))
	projectPath = groveDir

	agentDir := createAgentDir(t, groveDir, "real-agent")

	// Verify agent dir exists
	_, err := os.Stat(agentDir)
	require.NoError(t, err)

	err = deleteAgentLocal("real-agent")
	require.NoError(t, err)

	// Agent directory should be cleaned up
	_, err = os.Stat(agentDir)
	assert.True(t, os.IsNotExist(err), "agent directory should be deleted")
}

func TestDeleteAgentsViaHub_CleansUpLocalFiles(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	preserveBranch = true // skip branch operations since there's no real git repo

	projectID := "grove-del-123"
	server, deletedAgents := newDeleteMockHubServer(t, projectID)
	defer server.Close()

	// Set up grove directory with an agent
	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))
	projectPath = groveDir

	agentDir := createAgentDir(t, groveDir, "test-agent")

	// Verify agent dir exists before deletion
	_, err := os.Stat(agentDir)
	require.NoError(t, err, "agent directory should exist before deletion")

	// Create hub client and context
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	// Run the function under test
	err = deleteAgentsViaHub(hubCtx, []string{"test-agent"})
	require.NoError(t, err)

	// Verify Hub API was called
	require.Len(t, *deletedAgents, 1)
	assert.Equal(t, "test-agent", (*deletedAgents)[0])

	// Verify local agent directory was cleaned up
	_, err = os.Stat(agentDir)
	assert.True(t, os.IsNotExist(err), "agent directory should be deleted after Hub deletion")
}

func TestDeleteAgentsViaHub_MultipleAgents(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "grove-multi-456"
	server, deletedAgents := newDeleteMockHubServer(t, projectID)
	defer server.Close()

	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))
	projectPath = groveDir

	agent1Dir := createAgentDir(t, groveDir, "agent-one")
	agent2Dir := createAgentDir(t, groveDir, "agent-two")

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = deleteAgentsViaHub(hubCtx, []string{"agent-one", "agent-two"})
	require.NoError(t, err)

	// Both agents should be deleted on Hub
	require.Len(t, *deletedAgents, 2)

	// Both local directories should be cleaned up
	_, err = os.Stat(agent1Dir)
	assert.True(t, os.IsNotExist(err), "agent-one directory should be deleted")
	_, err = os.Stat(agent2Dir)
	assert.True(t, os.IsNotExist(err), "agent-two directory should be deleted")
}

func TestDeleteAgentsViaHub_HubFailsSkipsLocalCleanup(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "grove-fail-789"

	// Server that returns 404 for all agent deletes
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "not_found",
				"message": "Resource not found",
			},
		})
	}))
	defer server.Close()

	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))
	projectPath = groveDir

	agentDir := createAgentDir(t, groveDir, "missing-agent")

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	err = deleteAgentsViaHub(hubCtx, []string{"missing-agent"})
	require.Error(t, err, "should return error when Hub delete fails")

	// Local files should NOT be cleaned up when Hub delete fails
	_, err = os.Stat(agentDir)
	assert.NoError(t, err, "agent directory should still exist when Hub deletion fails")
}

func TestDeleteAgentsViaHub_NoLocalFiles(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "grove-nolocal-101"
	server, deletedAgents := newDeleteMockHubServer(t, projectID)
	defer server.Close()

	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(groveDir, "agents"), 0755))
	projectPath = groveDir

	// Don't create any agent directory - simulates agent existing only on Hub

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:   client,
		Endpoint: server.URL,
		ProjectID:  projectID,
	}

	// Should succeed without error even when no local files exist
	err = deleteAgentsViaHub(hubCtx, []string{"hub-only-agent"})
	require.NoError(t, err)

	require.Len(t, *deletedAgents, 1)
	assert.Equal(t, "hub-only-agent", (*deletedAgents)[0])
}

func TestDeleteAgentsViaHub_LocalCleanupFailureCreatesStaleLocalNotToRegister(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "grove-stale-202"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/groves/"+projectID+"/agents/stale-agent":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groves/"+projectID+"/agents":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
				"totalCount": 0,
				"nextCursor": "",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	groveDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))
	createAgentDir(t, groveDir, "stale-agent")

	// Force local cleanup to fail while keeping hubCtx.ProjectPath valid for state checkpointing.
	projectPath = filepath.Join(tmpHome, "nonexistent-grove-path")

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID:   projectID,
		ProjectPath: groveDir,
		IsGlobal:  false,
	}

	err = deleteAgentsViaHub(hubCtx, []string{"stale-agent"})
	require.NoError(t, err)

	state, err := config.LoadProjectState(groveDir)
	require.NoError(t, err)
	require.NotEmpty(t, state.LastSyncedAt, "expected watermark checkpoint after hub delete")

	syncCtx := &hubsync.HubContext{
		Client:    client,
		ProjectID:   projectID,
		BrokerID:  "",
		ProjectPath: groveDir,
		IsGlobal:  false,
		Settings:  &config.Settings{},
	}
	result, err := hubsync.CompareAgents(context.Background(), syncCtx)
	require.NoError(t, err)
	assert.Empty(t, result.ToRegister, "stale local artifact should not be forced into ToRegister")
	assert.Contains(t, result.StaleLocal, "stale-agent")
	assert.True(t, result.IsInSync(), "stale-local-only result should still be in sync")
}

func TestDeleteStopped_RequiresGroveContext(t *testing.T) {
	// Unset Hub context to avoid synthetic project root detection
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_GROVE_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}

	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)

	// Set CWD to a directory without .scion so grove resolution fails
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	noHub = true
	projectPath = ""
	deleteStopped = true

	// Running delete --stopped outside a grove should error
	err := deleteCmd.RunE(deleteCmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")
}

func TestDeleteStopped_AcceptsGlobalFlag(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)

	// Create global .scion directory
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "agents"), 0755))

	// Set CWD to a directory without .scion
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	// Verify that RequireProjectPath("global") resolves correctly even outside a grove.
	// The full command flow requires Docker for runtime.List, so we test the grove
	// resolution layer directly rather than the entire RunE.
	resolvedProject, isGlobal, err := config.RequireProjectPath("global")
	require.NoError(t, err)
	assert.True(t, isGlobal, "should resolve as global grove")
	assert.Equal(t, globalDir, resolvedProject)
}
