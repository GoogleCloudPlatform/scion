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

// secretTestState captures and restores package-level vars for test isolation.
type secretTestState struct {
	home              string
	projectPath         string
	secretProjectScope  string
	secretBrokerScope string
	secretScope       string
	secretOutputJSON  bool
}

func saveSecretTestState() secretTestState {
	return secretTestState{
		home:              os.Getenv("HOME"),
		projectPath:         projectPath,
		secretProjectScope:  secretProjectScope,
		secretBrokerScope: secretBrokerScope,
		secretScope:       secretScope,
		secretOutputJSON:  secretOutputJSON,
	}
}

func (s secretTestState) restore() {
	os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	secretProjectScope = s.secretProjectScope
	secretBrokerScope = s.secretBrokerScope
	secretScope = s.secretScope
	secretOutputJSON = s.secretOutputJSON
}

// setupSecretProject creates a grove directory with settings pointing to the given hub endpoint.
func setupSecretProject(t *testing.T, home, endpoint string) string {
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

// newSecretListMockServer creates a mock Hub server that handles secret list requests.
func newSecretListMockServer(t *testing.T, secrets []map[string]interface{}) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.URL.Path == "/api/v1/secrets" && r.Method == http.MethodGet:
			scope := r.URL.Query().Get("scope")
			if scope == "" {
				scope = "user"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"secrets": secrets,
				"scope":   scope,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server
}

func TestHubSecretListCmd_Exists(t *testing.T) {
	// Verify the list subcommand is registered under hub secret.
	found := false
	for _, sub := range hubSecretCmd.Commands() {
		if sub.Use == "list" {
			found = true
			break
		}
	}
	assert.True(t, found, "hubSecretCmd should have a 'list' subcommand")
}

func TestHubSecretListCmd_Flags(t *testing.T) {
	// Verify required flags are present on the list command.
	assert.NotNil(t, hubSecretListCmd.Flags().Lookup("grove"), "list command should have --grove flag")
	assert.NotNil(t, hubSecretListCmd.Flags().Lookup("broker"), "list command should have --broker flag")
	assert.NotNil(t, hubSecretListCmd.Flags().Lookup("json"), "list command should have --json flag")
}

func TestHubSecretListCmd_NoArgs(t *testing.T) {
	// Verify the command accepts no arguments.
	assert.Equal(t, "list", hubSecretListCmd.Use)
}

func TestRunSecretList_WithResults(t *testing.T) {
	orig := saveSecretTestState()
	defer orig.restore()

	secrets := []map[string]interface{}{
		{"key": "API_KEY", "type": "environment", "scope": "user", "version": 1, "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z"},
		{"key": "DB_PASSWORD", "type": "environment", "scope": "user", "version": 2, "created": "2026-01-01T00:00:00Z", "updated": "2026-01-02T00:00:00Z"},
	}

	server := newSecretListMockServer(t, secrets)
	defer server.Close()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupSecretProject(t, tmpHome, server.URL)
	projectPath = groveDir

	secretOutputJSON = false
	secretProjectScope = ""
	secretBrokerScope = ""

	err := runSecretList(hubSecretListCmd, nil)
	assert.NoError(t, err)
}

func TestRunSecretList_Empty(t *testing.T) {
	orig := saveSecretTestState()
	defer orig.restore()

	server := newSecretListMockServer(t, []map[string]interface{}{})
	defer server.Close()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupSecretProject(t, tmpHome, server.URL)
	projectPath = groveDir

	secretOutputJSON = false
	secretProjectScope = ""
	secretBrokerScope = ""

	err := runSecretList(hubSecretListCmd, nil)
	assert.NoError(t, err)
}

func TestRunSecretList_JSON(t *testing.T) {
	orig := saveSecretTestState()
	defer orig.restore()

	secrets := []map[string]interface{}{
		{"key": "MY_SECRET", "type": "variable", "scope": "user", "version": 1, "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z"},
	}

	server := newSecretListMockServer(t, secrets)
	defer server.Close()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	groveDir := setupSecretProject(t, tmpHome, server.URL)
	projectPath = groveDir

	secretOutputJSON = true
	secretProjectScope = ""
	secretBrokerScope = ""

	err := runSecretList(hubSecretListCmd, nil)
	assert.NoError(t, err)
}

func TestResolveSecretScope_ScopeHub(t *testing.T) {
	orig := saveSecretTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&secretScope, "scope", "", "")
	testCmd.Flags().StringVar(&secretProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set --scope hub
	testCmd.Flags().Set("scope", "hub")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupSecretProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
	require.NoError(t, err)

	scope, scopeID, err := resolveSecretScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "hub", scope)
	assert.Equal(t, "", scopeID, "hub scope should return empty scopeID (server resolves it)")
}

func TestResolveSecretScope_GroveFallbackToProjectID(t *testing.T) {
	// When --grove is set without value and settings.Hub.ProjectID is empty,
	// it should fall back to settings.ProjectID (the top-level deterministic grove ID).
	orig := saveSecretTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&secretScope, "scope", "", "")
	testCmd.Flags().StringVar(&secretProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set --grove without a value (triggers inference)
	testCmd.Flags().Set("grove", scopeInferSentinel)

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	// setupSecretProject sets grove_id but NOT hub.groveId
	groveDir := setupSecretProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
	require.NoError(t, err)
	// Verify precondition: Hub.ProjectID is empty but ProjectID is set
	assert.Empty(t, settings.GetHubProjectID(), "hub grove ID should be empty for this test")
	assert.NotEmpty(t, settings.ProjectID, "top-level grove ID should be set")

	scope, scopeID, err := resolveSecretScope(testCmd, settings)
	assert.NoError(t, err)
	assert.Equal(t, "project", scope)
	assert.Equal(t, settings.ProjectID, scopeID, "should fall back to settings.ProjectID")
}

func TestResolveSecretScope_ScopeConflictsWithProject(t *testing.T) {
	orig := saveSecretTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&secretScope, "scope", "", "")
	testCmd.Flags().StringVar(&secretProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set both --scope and --grove
	testCmd.Flags().Set("scope", "hub")
	testCmd.Flags().Set("grove", "some-grove")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupSecretProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
	require.NoError(t, err)

	_, _, err = resolveSecretScope(testCmd, settings)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify more than one")
}

func TestResolveSecretScope_ScopeConflictsWithBroker(t *testing.T) {
	orig := saveSecretTestState()
	defer orig.restore()

	testCmd := &cobra.Command{Use: "test"}
	testCmd.Flags().StringVar(&secretScope, "scope", "", "")
	testCmd.Flags().StringVar(&secretProjectScope, "grove", "", "")
	testCmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
	testCmd.Flags().StringVar(&secretBrokerScope, "broker", "", "")
	testCmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel

	// Set both --scope and --broker
	testCmd.Flags().Set("scope", "hub")
	testCmd.Flags().Set("broker", "some-broker")

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	groveDir := setupSecretProject(t, tmpHome, "http://localhost:9999")
	projectPath = groveDir

	settings, err := config.LoadSettings(groveDir)
	require.NoError(t, err)

	_, _, err = resolveSecretScope(testCmd, settings)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify more than one")
}

func TestHubSecretListCmd_ScopeFlag(t *testing.T) {
	// Verify the --scope flag is registered on all secret subcommands.
	for _, cmd := range []*cobra.Command{hubSecretSetCmd, hubSecretGetCmd, hubSecretListCmd, hubSecretClearCmd} {
		f := cmd.Flags().Lookup("scope")
		assert.NotNil(t, f, "%s command should have --scope flag", cmd.Use)
	}
}
