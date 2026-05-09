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
	home           string
	projectPath      string
	envProjectScope  string
	envBrokerScope string
	envScope       string
	envOutputJSON  bool
}

func saveEnvTestState() envTestState {
	return envTestState{
		home:           os.Getenv("HOME"),
		projectPath:      projectPath,
		envProjectScope:  envProjectScope,
		envBrokerScope: envBrokerScope,
		envScope:       envScope,
		envOutputJSON:  envOutputJSON,
	}
}

func (s envTestState) restore() {
	os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	envProjectScope = s.envProjectScope
	envBrokerScope = s.envBrokerScope
	envScope = s.envScope
	envOutputJSON = s.envOutputJSON
}

// setupEnvProject creates a grove directory with settings pointing to the given hub endpoint.
func setupEnvProject(t *testing.T, home, endpoint string) string {
	t.Helper()
	groveDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	settings := map[string]interface{}{
		"grove_id": "test-grove",
		"hub": map[string]interface{}{
			"enabled":  true,
			"endpoint": endpoint,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.json"), data, 0644))

	return groveDir
}

// newEnvListMockServer creates a mock Hub server that handles env list requests.
func newEnvListMockServer(t *testing.T, envVars []map[string]interface{}) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/env" && r.Method == http.MethodGet:
			scope := r.URL.Query().Get("scope")
			if scope == "" {
				scope = "user"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
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
	assert.NotNil(t, hubEnvListCmd.Flags().Lookup("grove"), "list command should have --grove flag")
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
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = groveDir

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
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = groveDir

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
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = groveDir

	envOutputJSON = true
	envProjectScope = ""
	envBrokerScope = ""

	err := runEnvList(hubEnvListCmd, nil)
	assert.NoError(t, err)
}

func TestHubEnvListCmd_GroveFlagNoOptDefVal(t *testing.T) {
	// Verify the --grove flag has NoOptDefVal set so bare --grove works.
	f := hubEnvListCmd.Flags().Lookup("grove")
	require.NotNil(t, f, "list command should have --grove flag")
	assert.Equal(t, scopeInferSentinel, f.NoOptDefVal, "--grove should have NoOptDefVal set to sentinel")
}

// setupEnvProjectWithHubProjectID creates a grove directory with settings that include
// a hub grove ID, endpoint, and enabled flag.
func setupEnvProjectWithHubProjectID(t *testing.T, home, endpoint, projectID string) string {
	t.Helper()
	groveDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	settings := map[string]interface{}{
		"grove_id": "test-grove",
		"hub": map[string]interface{}{
			"enabled":  true,
			"endpoint": endpoint,
			"groveId":  projectID,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(groveDir, "settings.json"), data, 0644))

	return groveDir
}

// newEnvProjectResolveMockServer creates a mock Hub server that handles both grove
// resolution (by slug/name) and env list requests.
func newEnvProjectResolveMockServer(t *testing.T, projectID, projectName, projectSlug string, envVars []map[string]interface{}) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/groves/"+projectID && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": projectName,
				"slug": projectSlug,
			})

		case r.URL.Path == "/api/v1/groves" && r.Method == http.MethodGet:
			slug := r.URL.Query().Get("slug")
			name := r.URL.Query().Get("name")
			var groves []map[string]interface{}
			if slug == projectSlug || name == projectName {
				groves = []map[string]interface{}{
					{"id": projectID, "name": projectName, "slug": projectSlug},
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"groves": groves,
			})

		case r.URL.Path == "/api/v1/env" && r.Method == http.MethodGet:
			scope := r.URL.Query().Get("scope")
			if scope == "" {
				scope = "user"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"envVars": envVars,
				"scope":   scope,
				"scopeId": r.URL.Query().Get("scopeId"),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    "not_found",
				"message": "Not found",
			})
		}
	}))

	return server
}

func TestRunEnvList_BareGroveFlag(t *testing.T) {
	// Test that bare --grove (sentinel value) infers grove ID from settings.
	orig := saveEnvTestState()
	defer orig.restore()

	groveUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	envVars := []map[string]interface{}{
		{"key": "GROVE_VAR", "value": "grove-value", "scope": "grove"},
	}

	server := newEnvProjectResolveMockServer(t, groveUUID, "My Project", "my-grove", envVars)
	defer server.Close()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupEnvProjectWithHubProjectID(t, tmpHome, server.URL, groveUUID)
	projectPath = groveDir

	envOutputJSON = false
	envBrokerScope = ""
	// Simulate bare --grove: set sentinel value and mark flag as changed
	envProjectScope = scopeInferSentinel
	hubEnvListCmd.Flags().Set("grove", scopeInferSentinel)
	defer hubEnvListCmd.Flags().Set("grove", "")

	err := runEnvList(hubEnvListCmd, nil)
	assert.NoError(t, err)
}

func TestRunEnvList_GroveByName(t *testing.T) {
	// Test that --grove=<name> resolves the grove name to a UUID.
	orig := saveEnvTestState()
	defer orig.restore()

	groveUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	envVars := []map[string]interface{}{
		{"key": "GROVE_VAR", "value": "grove-value", "scope": "grove"},
	}

	server := newEnvProjectResolveMockServer(t, groveUUID, "Hub Local", "hub-local", envVars)
	defer server.Close()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupEnvProject(t, tmpHome, server.URL)
	projectPath = groveDir

	envOutputJSON = false
	envBrokerScope = ""
	// Simulate --grove=hub-local
	envProjectScope = "hub-local"
	hubEnvListCmd.Flags().Set("grove", "hub-local")
	defer hubEnvListCmd.Flags().Set("grove", "")

	err := runEnvList(hubEnvListCmd, nil)
	assert.NoError(t, err)
}

func TestResolveEnvScope_SentinelInfersFromSettings(t *testing.T) {
	// Test that resolveEnvScope treats sentinel as "infer from settings".
	orig := saveEnvTestState()
	defer orig.restore()

	groveUUID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Create a temporary command to isolate flag state
	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set bare --grove (sentinel)
	testCmd.Flags().Set("grove", scopeInferSentinel)

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupEnvProjectWithHubProjectID(t, tmpHome, "http://localhost:9999", groveUUID)
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
	require.NoError(t, err)

	scope, scopeID, err := resolveEnvScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "project", scope)
	assert.Equal(t, groveUUID, scopeID, "should infer grove ID from settings when bare --grove is used")
}

func TestResolveEnvScope_ExplicitGroveValue(t *testing.T) {
	// Test that resolveEnvScope passes through an explicit grove name.
	orig := saveEnvTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set --grove=hub-local
	testCmd.Flags().Set("grove", "hub-local")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
	require.NoError(t, err)

	scope, scopeID, err := resolveEnvScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "project", scope)
	assert.Equal(t, "hub-local", scopeID, "should pass through the explicit grove name for later resolution")
}

func TestResolveEnvScope_ScopeHub(t *testing.T) {
	orig := saveEnvTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&envScope, "scope", "", "")
	testCmd.Flags().StringVar(&envProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set --scope hub
	testCmd.Flags().Set("scope", "hub")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
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
	testCmd.Flags().StringVar(&envProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set both --scope and --grove
	testCmd.Flags().Set("scope", "hub")
	testCmd.Flags().Set("grove", "some-grove")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
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
	testCmd.Flags().StringVar(&envProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&envBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set both --scope and --broker
	testCmd.Flags().Set("scope", "hub")
	testCmd.Flags().Set("broker", "some-broker")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupEnvProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
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
