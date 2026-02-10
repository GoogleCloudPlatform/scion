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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// templateTestState captures and restores package-level vars for test isolation.
type templateTestState struct {
	home        string
	globalMode  bool
	noHub       bool
	autoConfirm bool
	grovePath   string
}

func saveTemplateTestState() templateTestState {
	return templateTestState{
		home:        os.Getenv("HOME"),
		globalMode:  globalMode,
		noHub:       noHub,
		autoConfirm: autoConfirm,
		grovePath:   grovePath,
	}
}

func (s templateTestState) restore() {
	os.Setenv("HOME", s.home)
	globalMode = s.globalMode
	noHub = s.noHub
	autoConfirm = s.autoConfirm
	grovePath = s.grovePath
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

	createTestTemplate(t, tmpHome, "claude")

	err := runTemplateDelete(nil, []string{"claude"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot delete protected template")
}

// newMockHubServer creates a mock Hub server that handles the endpoints
// required by CheckHubAvailabilityWithOptions and template operations.
// groveID is the grove ID to recognize. templates is the list of templates to return.
// Returns the server and a pointer to a bool that tracks if delete was called.
func newMockHubServer(t *testing.T, groveID string, templates []map[string]interface{}) (*httptest.Server, *bool) {
	t.Helper()
	deleteCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// Health check
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		// Grove lookup (for isGroveRegistered)
		case strings.HasPrefix(r.URL.Path, "/api/v1/groves/") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   groveID,
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

// setupHubGrove creates a grove directory with settings pointing to the given hub endpoint.
func setupHubGrove(t *testing.T, home, endpoint, groveID string) string {
	t.Helper()
	groveDir := filepath.Join(home, "project", ".scion")
	require.NoError(t, os.MkdirAll(groveDir, 0755))

	settings := map[string]interface{}{
		"grove_id": groveID,
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
	globalMode = true
	autoConfirm = true
	noHub = false

	// Create empty local templates so FindTemplate doesn't find anything
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".scion", "templates"), 0755))

	groveID := "grove-test-123"
	templateID := "hub-tpl-456"

	server, deleteCalled := newMockHubServer(t, groveID, []map[string]interface{}{
		{
			"id":     templateID,
			"name":   "hub-only-tpl",
			"slug":   "hub-only-tpl",
			"scope":  "global",
			"status": "active",
		},
	})
	defer server.Close()

	grovePath = setupHubGrove(t, tmpHome, server.URL, groveID)

	err := runTemplateDelete(nil, []string{"hub-only-tpl"})
	require.NoError(t, err)
	assert.True(t, *deleteCalled, "hub delete API should have been called")
}

func TestRunTemplateDelete_Both_AutoConfirm(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	globalMode = true
	autoConfirm = true
	noHub = false

	templateDir := createTestTemplate(t, tmpHome, "both-tpl")

	groveID := "grove-test-789"
	templateID := "hub-both-456"

	server, deleteCalled := newMockHubServer(t, groveID, []map[string]interface{}{
		{
			"id":     templateID,
			"name":   "both-tpl",
			"slug":   "both-tpl",
			"scope":  "global",
			"status": "active",
		},
	})
	defer server.Close()

	grovePath = setupHubGrove(t, tmpHome, server.URL, groveID)

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
