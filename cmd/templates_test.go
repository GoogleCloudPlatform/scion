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
	projectPath string
}

func saveTemplateTestState() templateTestState {
	return templateTestState{
		home:        os.Getenv("HOME"),
		globalMode:  globalMode,
		noHub:       noHub,
		autoConfirm: autoConfirm,
		projectPath: projectPath,
	}
}

func (s templateTestState) restore() {
	_ = os.Setenv("HOME", s.home)
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
	_ = os.Setenv("HOME", tmpHome)
	globalMode = true
	autoConfirm = true

	// Create empty templates dir so the path resolves
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	err := runTemplateDelete(localTemplateCmd(t), []string{"nonexistent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRunTemplateDelete_LocalOnly_AutoConfirm(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	globalMode = true
	autoConfirm = true

	templateDir := createTestTemplate(t, tmpHome, "test-tpl")

	// Verify exists
	_, err := os.Stat(templateDir)
	require.NoError(t, err)

	err = runTemplateDelete(localTemplateCmd(t), []string{"test-tpl"})
	require.NoError(t, err)

	// Verify deleted
	_, err = os.Stat(templateDir)
	assert.True(t, os.IsNotExist(err), "template directory should be deleted")
}

func TestRunTemplateDelete_ProtectedTemplate(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	globalMode = true
	autoConfirm = true

	createTestTemplate(t, tmpHome, "default")

	err := runTemplateDelete(localTemplateCmd(t), []string{"default"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete protected template")
}

// newMockHubServer creates a mock Hub server that handles the endpoints
// required by requireHub and template operations.
// projectID is the project ID to recognize. templates is the list of templates to return.
// Returns the server and a pointer to a bool that tracks if delete was called.
func newMockHubServer(t *testing.T, projectID string, templates []map[string]interface{}) (*httptest.Server, *bool) {
	t.Helper()
	deleteCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// Health check
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		// Project lookup.
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "test-project",
			})

		// Template list
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": templates,
			})

		// Template delete
		case strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && r.Method == http.MethodDelete:
			deleteCalled = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &deleteCalled
}

// setupHubProject creates a project directory with settings pointing to the given hub endpoint.
func setupHubProject(t *testing.T, home, endpoint, projectID string) string {
	t.Helper()
	projectDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))

	settings := map[string]interface{}{
		"project_id": projectID,
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

func TestRunTemplateDelete_HubOnly_AutoConfirm(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	// Clear SCION_HUB_ENDPOINT to prevent it overriding the mock server URL
	// in settings loaded via koanf env provider
	origHubEndpoint := os.Getenv("SCION_HUB_ENDPOINT")
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	defer func() { _ = os.Setenv("SCION_HUB_ENDPOINT", origHubEndpoint) }()
	globalMode = true
	autoConfirm = true
	noHub = false

	// Create empty local templates so FindTemplate doesn't find anything
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	projectID := "project-test-123"
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
	_ = os.Setenv("HOME", tmpHome)
	// Clear SCION_HUB_ENDPOINT to prevent it overriding the mock server URL
	origHubEndpoint2 := os.Getenv("SCION_HUB_ENDPOINT")
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	defer func() { _ = os.Setenv("SCION_HUB_ENDPOINT", origHubEndpoint2) }()
	globalMode = true
	autoConfirm = true
	noHub = false

	templateDir := createTestTemplate(t, tmpHome, "both-tpl")

	projectID := "project-test-789"
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

func TestRunTemplateSync_RequiresArgOrAll(t *testing.T) {
	// Calling sync with no args and no --all should error
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("all", false, "")
	err := templateSyncArgs(cmd, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a template name argument or --all flag")
}

func TestRunTemplateSync_AllAndArgConflict(t *testing.T) {
	// Create a command with --all flag set
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("all", true, "")

	err := templateSyncArgs(cmd, []string{"some-template"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot specify both a template name and --all")
}

func TestRunTemplateSync_AllAndNameConflict(t *testing.T) {
	// Create a command with --all and --name flags set
	cmd := &cobra.Command{}
	cmd.Flags().String("name", "custom-name", "")
	cmd.Flags().Bool("all", true, "")

	err := templateSyncArgs(cmd, []string{})
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
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   projectID,
				"name": "test-project",
			})

		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": existingTemplates,
			})

		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"template": map[string]interface{}{
					"id":   "new-tpl-id",
					"name": "test-tpl",
				},
			})

		case strings.HasSuffix(r.URL.Path, "/download") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{
					{
						"path": "scion-agent.json",
						"hash": "sha256:old-hash-value",
						"url":  "http://example.com/download",
					},
				},
			})

		case strings.HasSuffix(r.URL.Path, "/upload") && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"uploadUrls": []interface{}{},
			})

		case strings.HasSuffix(r.URL.Path, "/finalize") && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
	_ = os.Setenv("HOME", tmpHome)
	origHubEndpoint := os.Getenv("SCION_HUB_ENDPOINT")
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	defer func() { _ = os.Setenv("SCION_HUB_ENDPOINT", origHubEndpoint) }()
	globalMode = true
	autoConfirm = true
	noHub = false

	// Create a local template
	createTestTemplate(t, tmpHome, "update-tpl")

	projectID := "project-update-123"

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
	_ = os.Setenv("HOME", tmpHome)
	globalMode = true
	autoConfirm = true

	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	// Status requires a hub, so it fails with no hub configured
	err := runTemplateStatus(nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errHubNotConfigured)
}

func TestWarnDeprecatedHubAccessScopes_JSON_WithScopes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scion-agent.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"harness": "claude",
		"hubAccess": {
			"scopes": ["project:read", "agent:create"]
		}
	}`), 0644))

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	warnDeprecatedHubAccessScopes("test-template", dir)

	_ = w.Close()
	os.Stderr = oldStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	assert.Contains(t, output, "deprecated hubAccess.scopes",
		"should warn about deprecated hubAccess.scopes")
	assert.Contains(t, output, "test-template",
		"warning should include template name")
}

func TestWarnDeprecatedHubAccessScopes_JSON_WithoutScopes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scion-agent.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"harness": "claude"
	}`), 0644))

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	warnDeprecatedHubAccessScopes("test-template", dir)

	_ = w.Close()
	os.Stderr = oldStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	assert.Empty(t, output,
		"should not warn when hubAccess.scopes is absent")
}

func TestWarnDeprecatedHubAccessScopes_YAML_WithScopes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scion-agent.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
harness: claude
hubAccess:
  scopes:
    - project:read
    - agent:create
`), 0644))

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	warnDeprecatedHubAccessScopes("yaml-template", dir)

	_ = w.Close()
	os.Stderr = oldStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	assert.Contains(t, output, "deprecated hubAccess.scopes",
		"should warn about deprecated hubAccess.scopes in YAML")
	assert.Contains(t, output, "yaml-template",
		"warning should include template name")
}

func TestWarnDeprecatedHubAccessScopes_EmptyScopes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "scion-agent.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"harness": "claude",
		"hubAccess": {
			"scopes": []
		}
	}`), 0644))

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	warnDeprecatedHubAccessScopes("empty-scopes", dir)

	_ = w.Close()
	os.Stderr = oldStderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	assert.Empty(t, output,
		"should not warn when hubAccess.scopes is empty array")
}

// localTemplateCmd returns a command carrying the template commands'
// --local and --hub flags, with --local set.
func localTemplateCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("local", false, "")
	cmd.Flags().Bool("hub", false, "")
	cmd.Flags().Bool("global", false, "")
	require.NoError(t, cmd.Flags().Set("local", "true"))
	return cmd
}

// TestTemplateCommands_NoHubReturnsError checks that without --local the
// template commands return the hub error instead of silently falling back
// to the filesystem (D5).
func TestTemplateCommands_NoHubReturnsError(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	hermeticCLIEnv(t)
	tmpHome := os.Getenv("HOME")
	globalMode = true
	autoConfirm = true
	templateDir := createTestTemplate(t, tmpHome, "local-tpl")

	for name, run := range map[string]func() error{
		"list":   func() error { return runTemplateList(nil, nil) },
		"show":   func() error { return runTemplateShow(nil, []string{"local-tpl"}) },
		"delete": func() error { return runTemplateDelete(nil, []string{"local-tpl"}) },
		"clone":  func() error { return runTemplateClone(nil, []string{"local-tpl", "copy-tpl"}) },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			require.Error(t, err)
			assert.ErrorIs(t, err, errHubNotConfigured)
		})
	}
	_, err := os.Stat(templateDir)
	assert.NoError(t, err, "the local template is untouched")
}

// TestRunTemplateList_LocalWithoutHub checks that 'templates list --local'
// lists the filesystem templates in a temp HOME with no hub configured.
func TestRunTemplateList_LocalWithoutHub(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	hermeticCLIEnv(t)
	tmpHome := os.Getenv("HOME")
	globalMode = true
	createTestTemplate(t, tmpHome, "local-tpl")

	var err error
	stdout, _ := captureStdIO(t, func() { err = runTemplateList(localTemplateCmd(t), nil) })
	require.NoError(t, err)
	assert.Contains(t, stdout, "local-tpl")
	assert.NotContains(t, stdout, "Hub Templates")
}

// TestRunTemplateList_LocalMakesNoHubRequest checks that --local makes no
// hub request even when a hub is configured.
func TestRunTemplateList_LocalMakesNoHubRequest(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	hub := withFakeHub(t)
	tmpHome := os.Getenv("HOME")
	globalMode = true
	createTestTemplate(t, tmpHome, "local-tpl")

	var err error
	stdout, _ := captureStdIO(t, func() { err = runTemplateList(localTemplateCmd(t), nil) })
	require.NoError(t, err)
	assert.Contains(t, stdout, "local-tpl")
	assert.Empty(t, hub.Requests(), "--local must not call the hub")
}

// templateListHub is a hub-connected test hub whose template list answers
// per scope: a scope in failScopes gets a 500, any other scope one template
// named "<scope>-tpl". It serves nothing else, so callers must not target
// the global project explicitly (that looks up the hub's Global project).
func templateListHub(t *testing.T, failScopes ...string) {
	t.Helper()
	hermeticCLIEnv(t)
	fail := map[string]bool{}
	for _, s := range failScopes {
		fail[s] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			scope := r.URL.Query().Get("scope")
			if fail[scope] {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{"code": "internal", "message": scope + " list failed"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": []map[string]string{{"id": scope + "-id", "name": scope + "-tpl"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_tpl")
	t.Setenv("SCION_PROJECT_ID", "tpl-project")
}

// TestRunTemplateList_HubListErrors: a failed global-scope list is returned
// (no empty hub section next to the local templates); a failed project or
// user list alone is a warning and the rest is still listed.
func TestRunTemplateList_HubListErrors(t *testing.T) {
	t.Run("global scope failure fails the command", func(t *testing.T) {
		orig := saveTemplateTestState()
		defer orig.restore()
		templateListHub(t, "global")
		// No explicit project: the hub-connected path takes the project ID
		// from SCION_PROJECT_ID. (An explicit --global here would look up
		// the hub's Global project, which this test hub does not serve.)
		globalMode, projectPath = false, ""
		t.Chdir(t.TempDir())

		var err error
		captureStdIO(t, func() { err = runTemplateList(nil, nil) })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to list hub templates")
	})
	t.Run("project and user failures are warnings", func(t *testing.T) {
		orig := saveTemplateTestState()
		defer orig.restore()
		templateListHub(t, "project", "user")
		// No explicit project: the hub-connected path takes the project ID
		// from SCION_PROJECT_ID. (An explicit --global here would look up
		// the hub's Global project, which this test hub does not serve.)
		globalMode, projectPath = false, ""
		t.Chdir(t.TempDir())

		var err error
		stdout, stderr := captureStdIO(t, func() { err = runTemplateList(nil, nil) })
		require.NoError(t, err)
		assert.Contains(t, stdout, "global-tpl")
		assert.Contains(t, stderr, "Warning: could not list project templates on the hub")
		assert.Contains(t, stderr, "Warning: could not list user templates on the hub")
	})
}
