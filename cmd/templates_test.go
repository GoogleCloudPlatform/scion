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
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// templateTestState captures and restores package-level vars for test isolation.
type templateTestState struct {
	home        string
	globalMode  bool
	noHub       bool
	autoConfirm bool
	projectPath   string
}

func saveTemplateTestState() templateTestState {
	return templateTestState{
		home:        os.Getenv("HOME"),
		globalMode:  globalMode,
		noHub:       noHub,
		autoConfirm: autoConfirm,
		projectPath:   projectPath,
	}
}

func (s templateTestState) restore() {
	os.Setenv("HOME", s.home)
	globalMode = s.globalMode
	noHub = s.noHub
	autoConfirm = s.autoConfirm
	projectPath = s.projectPath
}

// createTestTemplate creates a template directory at $HOME/.scion/templates/<name>.
func createTestTemplate(t *testing.T, home, name string) string {
	t.Helper()
	templateDir := filepath.Join(home, ".scion", "templates", name)
	require.NoError(t, os.MkdirAll(templateDir, 0755))
	require.NoError(t, os.WriteFile(
		filepath.Join(templateDir, "scion-agent.json"),
		[]byte(`{"harness":"claude"}`),
		0644,
	))
	return templateDir
}

func TestRunTemplateDelete_NotFound(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	globalMode = true
	noHub = true
	autoConfirm = true

	// Create empty templates dir so the path resolves
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	err := runTemplateDelete(nil, []string{"nonexistent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRunTemplateDelete_LocalOnly_AutoConfirm(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	globalMode = true
	noHub = true
	autoConfirm = true

	templateDir := createTestTemplate(t, tmpHome, "test-tpl")

	// Verify exists
	_, err := os.Stat(templateDir)
	require.NoError(t, err)

	err = runTemplateDelete(nil, []string{"test-tpl"})
	require.NoError(t, err)

	// Verify deleted
	_, err = os.Stat(templateDir)
	assert.True(t, os.IsNotExist(err), "template directory should be deleted")
}

func TestRunTemplateDelete_ProtectedTemplate(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	globalMode = true
	noHub = true
	autoConfirm = true

	createTestTemplate(t, tmpHome, "default")

	err := runTemplateDelete(nil, []string{"default"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete protected template")
}

// newMockHubServer creates a mock Hub server that handles the endpoints
// required by CheckHubAvailabilityWithOptions and template operations.
// projectID is the grove ID to recognize. templates is the list of templates to return.
// Returns the server and a pointer to a bool that tracks if delete was called.
func newMockHubServer(t *testing.T, projectID string, templates []map[string]interface{}) (*httptest.Server, *bool) {
	t.Helper()
	deleteCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// Health check
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		// Project lookup (for isGroveRegistered)
		case strings.HasPrefix(r.URL.Path, "/api/v1/groves/") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "test-grove",
			})

		// Template list
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": templates,
			})

		// Template delete
		case strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && r.Method == http.MethodDelete:
			deleteCalled = true
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &deleteCalled
}

// setupHubProject creates a grove directory with settings pointing to the given hub endpoint.
func setupHubProject(t *testing.T, home, endpoint, projectID string) string {
	t.Helper()
	groveDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	settings := map[string]interface{}{
		"grove_id": projectID,
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

func TestRunTemplateDelete_HubOnly_AutoConfirm(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	// Clear SCION_HUB_ENDPOINT to prevent it overriding the mock server URL
	// in settings loaded via koanf env provider
	origHubEndpoint := os.Getenv("SCION_HUB_ENDPOINT")
	os.Unsetenv("SCION_HUB_ENDPOINT")
	defer os.Setenv("SCION_HUB_ENDPOINT", origHubEndpoint)
	globalMode = true
	autoConfirm = true
	noHub = false

	// Create empty local templates so FindTemplate doesn't find anything
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	projectID := "grove-test-123"
	templateID := "hub-tpl-456"

	server, deleteCalled := newMockHubServer(t, projectID, []map[string]interface{}{
		{
			"id":     templateID,
			"name":   "hub-only-tpl",
			"slug":   "hub-only-tpl",
			"scope":  "global",
			"status": "active",
		},
	})
	defer server.Close()

	projectPath = setupHubProject(t, tmpHome, server.URL, projectID)

	err := runTemplateDelete(nil, []string{"hub-only-tpl"})
	require.NoError(t, err)
	assert.True(t, *deleteCalled, "hub delete API should have been called")
}

func TestRunTemplateDelete_Both_AutoConfirm(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	// Clear SCION_HUB_ENDPOINT to prevent it overriding the mock server URL
	origHubEndpoint2 := os.Getenv("SCION_HUB_ENDPOINT")
	os.Unsetenv("SCION_HUB_ENDPOINT")
	defer os.Setenv("SCION_HUB_ENDPOINT", origHubEndpoint2)
	globalMode = true
	autoConfirm = true
	noHub = false

	templateDir := createTestTemplate(t, tmpHome, "both-tpl")

	projectID := "grove-test-789"
	templateID := "hub-both-456"

	server, deleteCalled := newMockHubServer(t, projectID, []map[string]interface{}{
		{
			"id":     templateID,
			"name":   "both-tpl",
			"slug":   "both-tpl",
			"scope":  "global",
			"status": "active",
		},
	})
	defer server.Close()

	projectPath = setupHubProject(t, tmpHome, server.URL, projectID)

	err := runTemplateDelete(nil, []string{"both-tpl"})
	require.NoError(t, err)

	// Local template should be deleted
	_, err = os.Stat(templateDir)
	assert.True(t, os.IsNotExist(err), "local template directory should be deleted")

	// Hub delete should have been called
	assert.True(t, *deleteCalled, "hub delete API should have been called")
}

func TestRunTemplateDelete_NoHub_Flag(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	globalMode = true
	noHub = true // --no-hub set
	autoConfirm = true

	templateDir := createTestTemplate(t, tmpHome, "local-tpl")

	err := runTemplateDelete(nil, []string{"local-tpl"})
	require.NoError(t, err)

	// Verify deleted
	_, err = os.Stat(templateDir)
	assert.True(t, os.IsNotExist(err), "template directory should be deleted")
}

func TestRunTemplateSync_RequiresArgOrAll(t *testing.T) {
	// Calling sync with no args and no --all should error
	err := runTemplateSync(nil, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a template name argument or --all flag")
}

func TestRunTemplateSync_AllAndArgConflict(t *testing.T) {
	// Create a command with --all flag set
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("all", true, "")

	err := runTemplateSync(cmd, []string{"some-template"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify both a template name and --all")
}

func TestRunTemplateSync_AllAndNameConflict(t *testing.T) {
	// Create a command with --all and --name flags set
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "custom-name", "")
	cmd.Flags().Bool("all", true, "")

	err := runTemplateSync(cmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot use --name with --all")
}

// newMockHubServerForSync creates a mock Hub server that supports template
// list, create, upload, and finalize operations for testing sync.
func newMockHubServerForSync(t *testing.T, projectID string, existingTemplates []map[string]interface{}) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case strings.HasPrefix(r.URL.Path, "/api/v1/groves/") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "test-grove",
			})

		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": existingTemplates,
			})

		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"template": map[string]interface{}{
					"id":   "new-tpl-id",
					"name": "test-tpl",
				},
			})

		case strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{
					{
						"path": "scion-agent.json",
						"hash": "sha256:old-hash-value",
						"url":  "http://example.com/download",
					},
				},
			})

		case strings.HasSuffix(r.URL.Path, "/upload") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"uploadUrls": []interface{}{},
			})

		case strings.HasSuffix(r.URL.Path, "/finalize") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":          "new-tpl-id",
				"name":        "test-tpl",
				"status":      "active",
				"contentHash": "sha256:abc123",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestRunTemplateSync_UpdatesExistingTemplate(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	origHubEndpoint := os.Getenv("SCION_HUB_ENDPOINT")
	os.Unsetenv("SCION_HUB_ENDPOINT")
	defer os.Setenv("SCION_HUB_ENDPOINT", origHubEndpoint)
	globalMode = true
	autoConfirm = true
	noHub = false

	// Create a local template
	createTestTemplate(t, tmpHome, "update-tpl")

	projectID := "grove-update-123"

	// Hub server returns an existing template with a different hash
	server := newMockHubServerForSync(t, projectID, []map[string]interface{}{
		{
			"id":          "existing-tpl-id",
			"name":        "update-tpl",
			"slug":        "update-tpl",
			"scope":       "global",
			"status":      "active",
			"contentHash": "sha256:different-hash",
		},
	})
	defer server.Close()

	projectPath = setupHubProject(t, tmpHome, server.URL, projectID)

	// Sync should succeed without --force when content differs
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("all", false, "")

	err := runTemplateSync(cmd, []string{"update-tpl"})
	require.NoError(t, err)
}

func TestRunTemplateStatus_NoHub(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	globalMode = true
	noHub = true
	autoConfirm = true

	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	// Status requires Hub, so it should fail with no-hub
	err := runTemplateStatus(nil, nil)
	require.Error(t, err)
}
