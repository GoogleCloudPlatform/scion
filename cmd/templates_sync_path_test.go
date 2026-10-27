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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTemplateSyncPathSource: a path argument names a template directory
// (so an agent whose project .scion is read-only can sync from its
// checkout); a bare name keeps the name lookup.
func TestTemplateSyncPathSource(t *testing.T) {
	root := t.TempDir()
	tplDir := filepath.Join(root, "repo", "templates", "planner")
	require.NoError(t, os.MkdirAll(tplDir, 0o755))
	notDir := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o644))

	t.Run("bare name is not a path", func(t *testing.T) {
		for _, name := range []string{"planner", "custom-claude", "my_team.v2"} {
			tpl, isPath, err := templateSyncPathSource(name)
			assert.False(t, isPath, name)
			assert.NoError(t, err, name)
			assert.Nil(t, tpl, name)
		}
	})

	t.Run("absolute directory", func(t *testing.T) {
		tpl, isPath, err := templateSyncPathSource(tplDir)
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Equal(t, tplDir, tpl.Path)
		assert.Equal(t, "planner", tpl.Name)
	})

	t.Run("relative forms", func(t *testing.T) {
		t.Chdir(filepath.Join(root, "repo"))
		for _, arg := range []string{"./templates/planner", "templates/planner"} {
			tpl, isPath, err := templateSyncPathSource(arg)
			require.NoError(t, err, arg)
			assert.True(t, isPath, arg)
			assert.Equal(t, tplDir, tpl.Path, arg)
			assert.Equal(t, "planner", tpl.Name, arg)
		}
		t.Chdir(tplDir)
		tpl, isPath, err := templateSyncPathSource("../planner")
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Equal(t, tplDir, tpl.Path)

		tpl, isPath, err = templateSyncPathSource(".")
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Equal(t, tplDir, tpl.Path)
		assert.Equal(t, "planner", tpl.Name)

		tpl, isPath, err = templateSyncPathSource("..")
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Equal(t, filepath.Dir(tplDir), tpl.Path)
		assert.Equal(t, "templates", tpl.Name)
	})

	t.Run("path that is not a directory", func(t *testing.T) {
		for _, arg := range []string{notDir, filepath.Join(root, "missing")} {
			_, isPath, err := templateSyncPathSource(arg)
			assert.True(t, isPath, arg)
			assert.Error(t, err, arg)
		}
	})

	t.Run("content-hash directory derives no name", func(t *testing.T) {
		hashDir := filepath.Join(root, strings.Repeat("ab", 32))
		require.NoError(t, os.MkdirAll(hashDir, 0o755))
		tpl, isPath, err := templateSyncPathSource(hashDir)
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Empty(t, tpl.Name)
	})
}

func TestCheckDerivedTemplateName(t *testing.T) {
	assert.NoError(t, checkDerivedTemplateName(&config.Template{Name: "planner", Path: "/x/planner"}))
	for _, name := range []string{"", "___", "/"} {
		err := checkDerivedTemplateName(&config.Template{Name: name, Path: "/x/" + name})
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "--name", name)
	}
}

// pathSyncHubCalls records what a sync sent to newMockHubServerForPathSync.
type pathSyncHubCalls struct {
	mu        sync.Mutex
	created   []string
	uploaded  []string
	finalized []string
}

// newMockHubServerForPathSync serves a Hub with no templates and records the
// name of each created template and the files uploaded and finalized.
func newMockHubServerForPathSync(t *testing.T, projectID string, calls *pathSyncHubCalls) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls.mu.Lock()
		defer calls.mu.Unlock()

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID, "name": "test-project"})

		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"templates": []interface{}{}})

		case r.URL.Path == "/api/v1/templates" && r.Method == http.MethodPost:
			var req hubclient.CreateTemplateRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			calls.created = append(calls.created, req.Name)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"template": map[string]interface{}{"id": "new-tpl-id", "name": req.Name},
			})

		case strings.HasSuffix(r.URL.Path, "/upload") && r.Method == http.MethodPost:
			var req struct {
				Files []hubclient.FileUploadRequest `json:"files"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			for _, f := range req.Files {
				calls.uploaded = append(calls.uploaded, f.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"uploadUrls": []interface{}{}})

		case strings.HasSuffix(r.URL.Path, "/finalize") && r.Method == http.MethodPost:
			var req hubclient.FinalizeRequest
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.Manifest != nil {
				for _, f := range req.Manifest.Files {
					calls.finalized = append(calls.finalized, f.Path)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "new-tpl-id", "name": "planner", "status": "active", "contentHash": "sha256:abc123",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestRunTemplateSync_PathArgument: 'templates sync ./dir' uploads that
// directory, not a same-named template from the templates directories, and
// the Hub name defaults to the directory's base name; --name overrides it.
func TestRunTemplateSync_PathArgument(t *testing.T) {
	hashName := strings.Repeat("ef", 32)
	for _, tc := range []struct {
		name     string
		dirName  string // template directory name under checkout/templates
		inDir    bool   // run from inside the template directory
		arg      string
		flagName string
		wantName string
	}{
		{name: "hub name defaults to base name", dirName: "planner", arg: "./templates/planner", wantName: "planner"},
		{name: "--name overrides", dirName: "planner", arg: "./templates/planner", flagName: "team-planner", wantName: "team-planner"},
		{name: "dot from inside the template directory", dirName: "planner", inDir: true, arg: ".", wantName: "planner"},
		{name: "content-hash directory with --name", dirName: hashName, arg: "./templates/" + hashName, flagName: "planner", wantName: "planner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := saveTemplateTestState()
			defer orig.restore()

			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			t.Setenv("SCION_HUB_ENDPOINT", "")
			require.NoError(t, os.Unsetenv("SCION_HUB_ENDPOINT"))
			globalMode = true
			autoConfirm = true

			// A same-named template in the global templates directory, which
			// a bare-name sync would pick, holds only scion-agent.json.
			createTestTemplate(t, tmpHome, "planner")

			// The template edited in a checkout has an extra file.
			checkout := filepath.Join(tmpHome, "checkout")
			tplDir := filepath.Join(checkout, "templates", tc.dirName)
			writeTemplateFile(t, tplDir, "scion-agent.json", `{"harness":"claude"}`)
			writeTemplateFile(t, tplDir, "agents.md", "# planner\n")

			var calls pathSyncHubCalls
			server := newMockHubServerForPathSync(t, "project-path-123", &calls)
			defer server.Close()
			projectPath = setupHubProject(t, tmpHome, server.URL, "project-path-123")

			if tc.inDir {
				t.Chdir(tplDir)
			} else {
				t.Chdir(checkout)
			}
			cmd := &cobra.Command{}
			cmd.Flags().String("name", tc.flagName, "")
			cmd.Flags().Bool("all", false, "")

			captureStdout(t, func() {
				require.NoError(t, runTemplateSync(cmd, []string{tc.arg}))
			})

			calls.mu.Lock()
			defer calls.mu.Unlock()
			assert.Equal(t, []string{tc.wantName}, calls.created)
			assert.ElementsMatch(t, []string{"scion-agent.json", "agents.md"}, calls.uploaded)
			assert.ElementsMatch(t, []string{"scion-agent.json", "agents.md"}, calls.finalized)
		})
	}
}

// TestRunTemplateSync_PathArgumentRefusedBeforeHub: a path that is not a
// directory, or one whose base name cannot be a template name, is refused
// before any Hub call. A reachable Hub is configured, so a check that ran
// after contacting it would show up as a recorded request.
func TestRunTemplateSync_PathArgumentRefusedBeforeHub(t *testing.T) {
	orig := saveTemplateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	require.NoError(t, os.Unsetenv("SCION_HUB_ENDPOINT"))
	globalMode = true
	autoConfirm = true

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	projectPath = setupHubProject(t, tmpHome, server.URL, "project-refused-123")

	hashDir := filepath.Join(tmpHome, strings.Repeat("cd", 32))
	writeTemplateFile(t, hashDir, "scion-agent.json", `{"harness":"claude"}`)

	cmd := &cobra.Command{}
	cmd.Flags().String("name", "", "")
	cmd.Flags().Bool("all", false, "")

	err := runTemplateSync(cmd, []string{filepath.Join(tmpHome, "missing")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found or not a directory")

	err = runTemplateSync(cmd, []string{hashDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set one with --name")

	assert.Zero(t, requests.Load(), "a refused path must not reach the Hub")
}
