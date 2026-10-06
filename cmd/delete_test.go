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
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	projectPath    string
	preserveBranch bool
	noHub          bool
	autoConfirm    bool
	deleteStopped  bool
	deleteForce    bool
}

func saveDeleteTestState() deleteTestState {
	return deleteTestState{
		home:           os.Getenv("HOME"),
		projectPath:    projectPath,
		preserveBranch: preserveBranch,
		noHub:          noHub,
		autoConfirm:    autoConfirm,
		deleteStopped:  deleteStopped,
		deleteForce:    deleteForce,
	}
}

func (s deleteTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	preserveBranch = s.preserveBranch
	noHub = s.noHub
	autoConfirm = s.autoConfirm
	deleteStopped = s.deleteStopped
	deleteForce = s.deleteForce
}

// createAgentDir creates a minimal agent directory at <projectDir>/agents/<name>
// to simulate a locally provisioned agent.
func createAgentDir(t *testing.T, projectDir, name string) string {
	t.Helper()
	agentDir := filepath.Join(projectDir, "agents", name)
	require.NoError(t, os.MkdirAll(agentDir, 0755))
	// Write a marker file so the directory isn't empty
	require.NoError(t, os.WriteFile(
		filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness":"claude"}`),
		0644,
	))
	return agentDir
}

// newDeleteMockHubServer creates a mock Hub server that handles project-scoped
// agent DELETE requests. Returns the server and a pointer to a slice that
// records which agent names were deleted.
func newDeleteMockHubServer(t *testing.T, projectID string) (*httptest.Server, *[]string) {
	t.Helper()
	var deletedAgents []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodDelete:
			// Extract agent name from path: /api/v1/projects/<projectID>/agents/<agentName>
			prefix := "/api/v1/projects/" + projectID + "/agents/"
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
	_ = os.Setenv("HOME", tmpHome)
	noHub = true

	// Set up project directory without any agent
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	err := deleteAgentLocal("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteAgentLocal_ExistingAgentSucceeds(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = true
	preserveBranch = true

	// Set up project directory with an agent
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	agentDir := createAgentDir(t, projectDir, "real-agent")

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
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true // skip branch operations since there's no real git repo

	projectID := "project-del-123"
	server, deletedAgents := newDeleteMockHubServer(t, projectID)
	defer server.Close()

	// Set up project directory with an agent
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir

	agentDir := createAgentDir(t, projectDir, "test-agent")

	// Verify agent dir exists before deletion
	_, err := os.Stat(agentDir)
	require.NoError(t, err, "agent directory should exist before deletion")

	// Create hub client and context
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
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
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "project-multi-456"
	server, deletedAgents := newDeleteMockHubServer(t, projectID)
	defer server.Close()

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir

	agent1Dir := createAgentDir(t, projectDir, "agent-one")
	agent2Dir := createAgentDir(t, projectDir, "agent-two")

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
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
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "project-fail-789"

	// Server that returns 404 for all agent deletes
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "not_found",
				"message": "Resource not found",
			},
		})
	}))
	defer server.Close()

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	projectPath = projectDir

	agentDir := createAgentDir(t, projectDir, "missing-agent")

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
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
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "project-nolocal-101"
	server, deletedAgents := newDeleteMockHubServer(t, projectID)
	defer server.Close()

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	// Don't create any agent directory - simulates agent existing only on Hub

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
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
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true

	projectID := "project-stale-202"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/"+projectID+"/agents/stale-agent":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	createAgentDir(t, projectDir, "stale-agent")

	// Force local cleanup to fail while keeping hubCtx.ProjectPath valid for state checkpointing.
	projectPath = filepath.Join(tmpHome, "nonexistent-project-path")

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:      client,
		Endpoint:    server.URL,
		ProjectID:   projectID,
		ProjectPath: projectDir,
		IsGlobal:    false,
	}

	err = deleteAgentsViaHub(hubCtx, []string{"stale-agent"})
	require.NoError(t, err)

	state, err := config.LoadProjectState(projectDir)
	require.NoError(t, err)
	require.NotEmpty(t, state.LastSyncedAt, "expected watermark checkpoint after hub delete")

	syncCtx := &hubsync.HubContext{
		Client:      client,
		ProjectID:   projectID,
		BrokerID:    "",
		ProjectPath: projectDir,
		IsGlobal:    false,
		Settings:    &config.Settings{},
	}
	result, err := hubsync.CompareAgents(context.Background(), syncCtx)
	require.NoError(t, err)
	assert.Empty(t, result.ToRegister, "stale local artifact should not be forced into ToRegister")
	assert.Contains(t, result.StaleLocal, "stale-agent")
	assert.True(t, result.IsInSync(), "stale-local-only result should still be in sync")
}

func TestDeleteStopped_RequiresProjectContext(t *testing.T) {
	// Unset Hub context to avoid synthetic project root detection
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			t.Setenv(e, val) // registers restore of the original value via t.Cleanup
			_ = os.Unsetenv(e)
		}
	}

	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)

	// Set CWD to a directory without .scion so project resolution fails
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	noHub = true
	projectPath = ""
	deleteStopped = true

	// Running delete --stopped outside a project should error
	err := deleteCmd.RunE(deleteCmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in a scion project")
}

func TestDeleteStopped_AcceptsGlobalFlag(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)

	// Create global .scion directory
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "agents"), 0755))

	// Set CWD to a directory without .scion
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)

	// Verify that RequireProjectPath("global") resolves correctly even outside a project.
	// The full command flow requires Docker for runtime.List, so we test the project
	// resolution layer directly rather than the entire RunE.
	resolvedProject, isGlobal, err := config.RequireProjectPath("global")
	require.NoError(t, err)
	assert.True(t, isGlobal, "should resolve as global project")
	assert.Equal(t, globalDir, resolvedProject)
}

// newDeleteQueryRecordingHubServer creates a mock Hub server that records the
// raw query string of every agent DELETE request, keyed by agent name. It also
// serves the stopped-agent list used by delete --stopped.
func newDeleteQueryRecordingHubServer(t *testing.T, projectID string, stopped []string) (*httptest.Server, map[string]url.Values) {
	t.Helper()
	queries := map[string]url.Values{}
	prefix := "/api/v1/projects/" + projectID + "/agents"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == prefix:
			agents := make([]map[string]interface{}, 0, len(stopped))
			for _, name := range stopped {
				agents = append(agents, map[string]interface{}{"id": name, "name": name, "phase": "stopped"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     agents,
				"totalCount": len(agents),
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix+"/"):
			queries[strings.TrimPrefix(r.URL.Path, prefix+"/")] = r.URL.Query()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server, queries
}

func newDeleteForceHubContext(t *testing.T, serverURL, projectID string) *HubContext {
	t.Helper()
	client, err := hubclient.New(serverURL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: serverURL, ProjectID: projectID}
}

func TestDeleteCmd_ForceFlagRegistered(t *testing.T) {
	f := deleteCmd.Flags().Lookup("force")
	require.NotNil(t, f, "delete should expose --force")
	assert.Equal(t, "f", f.Shorthand)
	assert.Equal(t, "false", f.DefValue)
	assert.Contains(t, f.Usage, "broker")
}

func TestDeleteAgentsViaHub_ForceSendsForceQuery(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true
	deleteForce = true

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	projectID := "project-force-1"
	server, queries := newDeleteQueryRecordingHubServer(t, projectID, nil)
	defer server.Close()

	err := deleteAgentsViaHub(newDeleteForceHubContext(t, server.URL, projectID), []string{"agent-a", "agent-b"})
	require.NoError(t, err)

	require.Len(t, queries, 2, "every named agent should be deleted")
	for _, name := range []string{"agent-a", "agent-b"} {
		require.Contains(t, queries, name)
		assert.Equal(t, "true", queries[name].Get("force"), "force=true should be sent for %s", name)
	}
}

func TestDeleteAgentsViaHub_NoForceOmitsForceQuery(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true
	deleteForce = false

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	projectID := "project-force-2"
	server, queries := newDeleteQueryRecordingHubServer(t, projectID, nil)
	defer server.Close()

	err := deleteAgentsViaHub(newDeleteForceHubContext(t, server.URL, projectID), []string{"agent-a"})
	require.NoError(t, err)

	require.Contains(t, queries, "agent-a")
	_, present := queries["agent-a"]["force"]
	assert.False(t, present, "force must be absent when --force is not set")
}

// TestDeleteCmd_ForceWithStoppedRejected runs the real command tree so that
// cobra's Args validation is exercised before any hub work. --force with
// --stopped would be a bulk, permanent delete, so it must be rejected and no
// delete request may reach the hub.
func TestDeleteCmd_ForceWithStoppedRejected(t *testing.T) {
	restoreAllSilenceUsage(t)
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = false
	preserveBranch = true

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	projectID := "project-force-3"
	server, queries := newDeleteQueryRecordingHubServer(t, projectID, []string{"stopped-one", "stopped-two"})
	defer server.Close()
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_PROJECT_ID", projectID)
	// Clear SCION_HOST_UID so the root agent-container check does not intercept
	// the command; otherwise the zero-DELETE assertion is vacuous in containers.
	t.Setenv("SCION_HOST_UID", "")

	rootCmd.SetArgs([]string{"delete", "--stopped", "--force"})
	defer func() {
		rootCmd.SetArgs(nil)
		for _, name := range []string{"stopped", "force"} {
			if f := deleteCmd.Flags().Lookup(name); f != nil {
				f.Changed = false
			}
		}
	}()

	var err error
	_ = captureStderr(t, func() {
		err = rootCmd.Execute()
	})
	// Check the zero-DELETE invariant first so it is evaluated even when the
	// error assertion below fails.
	assert.Empty(t, queries, "no delete request may reach the hub")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force cannot be combined with --stopped")
}

func TestDeleteStoppedViaHub_NoForceOmitsForceQuery(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	preserveBranch = true
	deleteForce = false

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	projectID := "project-force-4"
	server, queries := newDeleteQueryRecordingHubServer(t, projectID, []string{"stopped-one", "stopped-two"})
	defer server.Close()

	err := deleteStoppedViaHub(newDeleteForceHubContext(t, server.URL, projectID))
	require.NoError(t, err)

	require.Len(t, queries, 2, "every stopped agent should be deleted")
	for name, q := range queries {
		_, present := q["force"]
		assert.False(t, present, "force must be absent for stopped agent %s", name)
	}
}

// TestDeleteCmd_ForceViaHubDoesNotWarnLocalMode drives RunE down the Hub path
// (hub-connected env) and checks that --force is sent and that the local-mode
// warning is not printed.
func TestDeleteCmd_ForceViaHubDoesNotWarnLocalMode(t *testing.T) {
	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = false
	preserveBranch = true
	deleteStopped = false
	deleteForce = true

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	projectID := "project-force-5"
	// projectPath stands in for an explicit --project flag, whose own
	// project ID wins over SCION_PROJECT_ID (ptone/scion#3123).
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "project-id"), []byte(projectID+"\n"), 0644))
	server, queries := newDeleteQueryRecordingHubServer(t, projectID, nil)
	defer server.Close()
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_PROJECT_ID", projectID)

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = deleteCmd.RunE(deleteCmd, []string{"hub-agent"})
	})
	require.NoError(t, runErr)
	assert.Contains(t, stderr, "Using hub:", "delete should take the Hub path")
	assert.NotContains(t, stderr, "--force has no effect without a Hub")
	require.Contains(t, queries, "hub-agent")
	assert.Equal(t, "true", queries["hub-agent"].Get("force"))
}

func TestDeleteCmd_ForceInLocalModeWarnsAndDeletes(t *testing.T) {
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			t.Setenv(e, val) // registers restore of the original value via t.Cleanup
			_ = os.Unsetenv(e)
		}
	}

	orig := saveDeleteTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = true
	preserveBranch = true
	deleteStopped = false
	deleteForce = true

	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir
	agentDir := createAgentDir(t, projectDir, "local-agent")

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = deleteCmd.RunE(deleteCmd, []string{"local-agent"})
	})
	require.NoError(t, runErr, "--force must not block a local delete")
	assert.Contains(t, stderr, "--force has no effect without a Hub")

	_, err := os.Stat(agentDir)
	assert.True(t, os.IsNotExist(err), "agent directory should be deleted in local mode")
}
