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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envTestState captures and restores package-level vars for test isolation.
type envTestState struct {
	home            string
	projectPath     string
	envProjectScope string
	envBrokerScope  string
	envScope        string
	envOutputJSON   bool
}

func saveEnvTestState() envTestState {
	return envTestState{
		home:            os.Getenv("HOME"),
		projectPath:     projectPath,
		envProjectScope: envProjectScope,
		envBrokerScope:  envBrokerScope,
		envScope:        envScope,
		envOutputJSON:   envOutputJSON,
	}
}

func (s envTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	envProjectScope = s.envProjectScope
	envBrokerScope = s.envBrokerScope
	envScope = s.envScope
	envOutputJSON = s.envOutputJSON
}

// setupEnvProject creates a project directory with settings pointing to the given hub endpoint.
func setupEnvProject(t *testing.T, home, endpoint string) string {
	t.Helper()
	projectDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	settings := map[string]interface{}{
		"grove_id": "test-grove",
		"hub": map[string]interface{}{
			"enabled":  true,
			"endpoint": endpoint,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))

	return projectDir
}

// newEnvListMockServer creates a mock Hub server that handles env list requests.
func newEnvListMockServer(t *testing.T, envVars []map[string]interface{}) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/env" && r.Method == http.MethodGet:
			scope := r.URL.Query().Get("scope")
			if scope == "" {
				scope = "user"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"envVars": envVars,
				"scope":   scope,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server
}

func TestHubEnvListCmd_Exists(t *testing.T) {
	// Verify the list subcommand is registered under hub env.
	found := false
	for _, sub := range hubEnvCmd.Commands() {
		if sub.Use == "list" {
			found = true
			break
		}
	}
	assert.True(t, found, "hubEnvCmd should have a 'list' subcommand")
}

func TestHubEnvListCmd_Flags(t *testing.T) {
	// Verify required flags are present on the list command.
	assert.NotNil(t, hubEnvListCmd.Flags().Lookup("project"), "list command should have --project flag")
	assert.NotNil(t, hubEnvListCmd.Flags().Lookup("broker"), "list command should have --broker flag")
	assert.NotNil(t, hubEnvListCmd.Flags().Lookup("json"), "list command should have --json flag")
}

func TestHubEnvListCmd_NoArgs(t *testing.T) {
	// Verify the command accepts no arguments.
	assert.Equal(t, "list", hubEnvListCmd.Use)
}

func TestRunEnvList_WithResults(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	envVars := []map[string]interface{}{
		{"key": "API_URL", "value": "https://api.example.com", "scope": "user", "injectionMode": "always"},
		{"key": "LOG_LEVEL", "value": "debug", "scope": "user", "injectionMode": "as_needed"},
	}

	server := newEnvListMockServer(t, envVars)
	defer server.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	projectDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = projectDir

	envOutputJSON = false
	envProjectScope = ""
	envBrokerScope = ""

	err := runEnvList(hubEnvListCmd, nil)
	assert.NoError(t, err)
}

func TestRunEnvList_Empty(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	server := newEnvListMockServer(t, []map[string]interface{}{})
	defer server.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	projectDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = projectDir

	envOutputJSON = false
	envProjectScope = ""
	envBrokerScope = ""

	err := runEnvList(hubEnvListCmd, nil)
	assert.NoError(t, err)
}

func TestRunEnvList_JSON(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	envVars := []map[string]interface{}{
		{"key": "MY_VAR", "value": "hello", "scope": "user"},
	}

	server := newEnvListMockServer(t, envVars)
	defer server.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	projectDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = projectDir

	envOutputJSON = true
	envProjectScope = ""
	envBrokerScope = ""

	err := runEnvList(hubEnvListCmd, nil)
	assert.NoError(t, err)
}

func TestHubEnvListCmd_ProjectFlagNoOptDefVal(t *testing.T) {
	// Verify the --project flag has NoOptDefVal set so bare --project works.
	f := hubEnvListCmd.Flags().Lookup("project")
	require.NotNil(t, f, "list command should have --project flag")
	assert.Equal(t, scopeInferSentinel, f.NoOptDefVal, "--project should have NoOptDefVal set to sentinel")
}

// setupEnvProjectWithHubProjectID creates a project directory with settings that include
// a hub project ID, endpoint, and enabled flag.
func setupEnvProjectWithHubProjectID(t *testing.T, home, endpoint, projectID string) string {
	t.Helper()
	projectDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	settings := map[string]interface{}{
		"grove_id": "test-grove",
		"hub": map[string]interface{}{
			"enabled":   true,
			"endpoint":  endpoint,
			"projectId": projectID,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))

	return projectDir
}

// newEnvProjectResolveMockServer creates a mock Hub server that handles both project
// resolution (by slug/name) and env list requests.
func newEnvProjectResolveMockServer(t *testing.T, projectID, projectName, projectSlug string, envVars []map[string]interface{}) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/projects/"+projectID && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": projectName,
				"slug": projectSlug,
			})

		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			slug := r.URL.Query().Get("slug")
			name := r.URL.Query().Get("name")
			var projects []map[string]interface{}
			if slug == projectSlug || name == projectName {
				projects = []map[string]interface{}{
					{"id": projectID, "name": projectName, "slug": projectSlug},
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"projects": projects,
			})

		case r.URL.Path == "/api/v1/env" && r.Method == http.MethodGet:
			scope := r.URL.Query().Get("scope")
			if scope == "" {
				scope = "user"
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"envVars": envVars,
				"scope":   scope,
				"scopeId": r.URL.Query().Get("scopeId"),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "not_found",
				"message": "Not found",
			})
		}
	}))

	return server
}

func TestRunEnvList_BareProjectFlag(t *testing.T) {
	// Test that bare --project (sentinel value) infers project ID from settings.
	orig := saveEnvTestState()
	defer orig.restore()

	projectUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	envVars := []map[string]interface{}{
		{"key": "PROJECT_VAR", "value": "project-value", "scope": "project"},
	}

	server := newEnvProjectResolveMockServer(t, projectUUID, "My Project", "my-project", envVars)
	defer server.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	projectDir := setupEnvProjectWithHubProjectID(t, tmpHome, server.URL, projectUUID)
	projectPath = projectDir

	envOutputJSON = false
	envBrokerScope = ""
	envProjectScope = scopeInferSentinel

	// Use a temporary command instead of the shared hubEnvListCmd: pflag's
	// Set always leaves Changed permanently true, even after a later
	// Set("project", "") resets the value, so mutating hubEnvListCmd's real
	// flags here would leak Changed=true into any later test that inspects
	// them (TestRunEnvList_WithResults/_Empty/_JSON call
	// runEnvList(hubEnvListCmd) and read Changed("project")).
	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	// Simulate bare --project: set sentinel value and mark flag as changed.
	_ = testCmd.Flags().Set("project", scopeInferSentinel)

	err := runEnvList(testCmd, nil)
	assert.NoError(t, err)
}

func TestRunEnvList_ProjectByName(t *testing.T) {
	// Test that --project=<name> resolves the project name to a UUID.
	orig := saveEnvTestState()
	defer orig.restore()

	projectUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	envVars := []map[string]interface{}{
		{"key": "PROJECT_VAR", "value": "project-value", "scope": "project"},
	}

	server := newEnvProjectResolveMockServer(t, projectUUID, "Hub Local", "hub-local", envVars)
	defer server.Close()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	projectDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = projectDir

	envOutputJSON = false
	envBrokerScope = ""
	envProjectScope = "hub-local"

	// Use a temporary command instead of the shared hubEnvListCmd: see the
	// comment in TestRunEnvList_BareProjectFlag above.
	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	// Simulate --project=hub-local.
	_ = testCmd.Flags().Set("project", "hub-local")

	err := runEnvList(testCmd, nil)
	assert.NoError(t, err)
}

func TestResolveEnvScope_SentinelInfersFromSettings(t *testing.T) {
	// Test that resolveEnvScope treats sentinel as "infer from settings".
	orig := saveEnvTestState()
	defer orig.restore()

	projectUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create a temporary command to isolate flag state
	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set bare --project (sentinel)
	_ = testCmd.Flags().Set("project", scopeInferSentinel)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupEnvProjectWithHubProjectID(t, tmpHome, "http://localhost:9999", projectUUID)
	projectPath = projectDir

	settings, err := config.LoadSettings(projectDir)
	require.NoError(t, err)

	scope, scopeID, err := resolveEnvScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "project", scope)
	assert.Equal(t, projectUUID, scopeID, "should infer project ID from settings when bare --project is used")
}

func TestResolveEnvScope_ExplicitProjectValue(t *testing.T) {
	// Test that resolveEnvScope passes through an explicit project name.
	orig := saveEnvTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set --project=hub-local
	_ = testCmd.Flags().Set("project", "hub-local")

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = projectDir

	settings, err := config.LoadSettings(projectDir)
	require.NoError(t, err)

	scope, scopeID, err := resolveEnvScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "project", scope)
	assert.Equal(t, "hub-local", scopeID, "should pass through the explicit project name for later resolution")
}

func TestResolveEnvScope_ScopeHub(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envScope, "scope", "", "")
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set --scope hub
	_ = testCmd.Flags().Set("scope", "hub")

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = projectDir

	settings, err := config.LoadSettings(projectDir)
	require.NoError(t, err)

	scope, scopeID, err := resolveEnvScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "hub", scope)
	assert.Equal(t, "", scopeID, "hub scope should return empty scopeID (server resolves it)")
}

func TestResolveEnvScope_ScopeConflictsWithProject(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envScope, "scope", "", "")
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set both --scope and --project
	_ = testCmd.Flags().Set("scope", "hub")
	_ = testCmd.Flags().Set("project", "some-project")

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = projectDir

	settings, err := config.LoadSettings(projectDir)
	require.NoError(t, err)

	_, _, err = resolveEnvScope(testCmd, settings)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify more than one")
}

func TestResolveEnvScope_ScopeConflictsWithBroker(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envScope, "scope", "", "")
	testCmd.Flags().StringVar(&envProjectScope, "project", "", "")
	testCmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set both --scope and --broker
	_ = testCmd.Flags().Set("scope", "hub")
	_ = testCmd.Flags().Set("broker", "some-broker")

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = projectDir

	settings, err := config.LoadSettings(projectDir)
	require.NoError(t, err)

	_, _, err = resolveEnvScope(testCmd, settings)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify more than one")
}

func TestHubEnvListCmd_ScopeFlag(t *testing.T) {
	// Verify the --scope flag is registered on all env subcommands.
	for _, cmd := range []*cobra.Command{hubEnvSetCmd, hubEnvGetCmd, hubEnvListCmd, hubEnvClearCmd} {
		f := cmd.Flags().Lookup("scope")
		assert.NotNil(t, f, "%s command should have --scope flag", cmd.Use)
	}
}
