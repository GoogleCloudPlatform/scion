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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for G1 (ptone/scion#1713): 'scion templates update --scope global'
// with --hub and a hub token refreshes a global template without reading
// any project, so a token without project:read can run it, inside or
// outside a project directory.

const tuGlobalTemplateID = "tmpl-global-1"

// tuHub is a fake hub with one global and one project template named
// "base". Project reads answer 404, as they do for a hub token without
// project:read, except for the project tuProjectID.
type tuHub struct {
	mu          sync.Mutex
	requests    []string
	reimported  []string
	projectHits []string
}

const tuProjectID = "tu-project-id"

func (h *tuHub) snapshot() (requests, reimported, projectHits []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...), append([]string(nil), h.reimported...), append([]string(nil), h.projectHits...)
}

func newTUHub(t *testing.T) (*tuHub, *httptest.Server) {
	t.Helper()
	h := &tuHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects") {
			h.projectHits = append(h.projectHits, r.Method+" "+r.URL.RequestURI())
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+tuProjectID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": tuProjectID, "name": "tu", "slug": "tu"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "project not found"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates":
			all := []map[string]string{
				{"id": tuGlobalTemplateID, "name": "base", "slug": "base", "scope": "global", "status": "active",
					"sourceUrl": "https://github.com/acme/repo/tree/main/.scion/templates/base"},
				{"id": "tmpl-project-1", "name": "base", "slug": "base", "scope": "project", "scopeId": tuProjectID,
					"projectId": tuProjectID, "status": "active",
					"sourceUrl": "https://github.com/acme/repo/tree/main/.scion/templates/base"},
			}
			out := []map[string]string{}
			for _, tmpl := range all {
				if s := r.URL.Query().Get("scope"); s != "" && tmpl["scope"] != s {
					continue
				}
				out = append(out, tmpl)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": out})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/templates/") && strings.HasSuffix(r.URL.Path, "/reimport"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/templates/"), "/reimport")
			h.mu.Lock()
			h.reimported = append(h.reimported, id)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": []string{"base"}, "count": 1})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "not found"}})
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// tuSetup isolates HOME and the working directory, sets a hub token and no
// SCION_HUB_ENDPOINT, and restores the package flag state afterwards.
func tuSetup(t *testing.T) (cwd string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "SCION_") {
			t.Setenv(name, "")
			require.NoError(t, os.Unsetenv(name))
		}
	}
	t.Setenv("SCION_HUB_TOKEN", "scion_pat_tutest")
	// The fake hub listens on localhost; inside an agent container this
	// tells the container guard it is reachable.
	t.Setenv("SCION_NETWORK_MODE", "host")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(cwd)

	origProject, origGlobal, origHub, origSource := projectPath, globalMode, hubEndpoint, hubSource
	origAuto, origNonInt, origProfile := autoConfirm, nonInteractive, hubProfileFlag
	t.Cleanup(func() {
		projectPath, globalMode, hubEndpoint, hubSource = origProject, origGlobal, origHub, origSource
		autoConfirm, nonInteractive, hubProfileFlag = origAuto, origNonInt, origProfile
	})
	return cwd
}

// runTemplatesUpdateRoot runs 'scion <args>' through the root command, so
// root's project gate and target resolution apply, then resets the flags
// it may have set.
func runTemplatesUpdateRoot(t *testing.T, args ...string) error {
	t.Helper()
	restoreAllSilenceUsage(t)
	reset := func() {
		for _, name := range []string{"scope", "url", "all"} {
			f := templatesUpdateCmd.Flags().Lookup(name)
			require.NotNil(t, f, name)
			require.NoError(t, f.Value.Set(f.DefValue))
			f.Changed = false
		}
		for _, name := range []string{"global", "hub", "project", "yes", "non-interactive"} {
			if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
				require.NoError(t, f.Value.Set(f.DefValue))
				f.Changed = false
			}
		}
		rootCmd.SetArgs(nil)
		rootCmd.SetErr(nil)
	}
	reset()
	t.Cleanup(reset)
	rootCmd.SetErr(&strings.Builder{})
	rootCmd.SetArgs(args)
	var err error
	_ = captureStdout(t, func() { err = rootCmd.Execute() })
	return err
}

func TestTemplatesUpdate_GlobalScopeWithHubTokenSkipsProject(t *testing.T) {
	for name, args := range map[string][]string{
		"--scope global":          {"templates", "update", "base", "--scope", "global"},
		"--global":                {"--global", "templates", "update", "base"},
		"--global --scope global": {"--global", "templates", "update", "base", "--scope", "global"},
	} {
		for _, inProject := range []bool{false, true} {
			label := name + " outside a project"
			if inProject {
				label = name + " inside a project"
			}
			t.Run(label, func(t *testing.T) {
				hub, srv := newTUHub(t)
				cwd := tuSetup(t)
				if inProject {
					require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
				}
				err := runTemplatesUpdateRoot(t, append([]string{"--hub", srv.URL, "--yes", "--non-interactive"}, args...)...)
				requests, reimported, projectHits := hub.snapshot()
				require.NoError(t, err, "requests: %v", requests)
				assert.Equal(t, []string{tuGlobalTemplateID}, reimported)
				assert.Empty(t, projectHits, "a global-scope refresh must not read any project")
			})
		}
	}
}

// TestTemplatesUpdate_ProjectScopeStillLinksProject: a project-scope
// refresh keeps the project admission flow: the project is looked up on
// the hub, and a project the token cannot read stops the command before
// any reimport.
func TestTemplatesUpdate_ProjectScopeStillLinksProject(t *testing.T) {
	t.Run("unreadable project fails before reimport", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, "unreadable-project-id")
		err := runTemplatesUpdateRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"templates", "update", "base", "--scope", "project")
		require.Error(t, err)
		_, reimported, projectHits := hub.snapshot()
		assert.Empty(t, reimported)
		assert.NotEmpty(t, projectHits, "a project-scope refresh must look up the project")
	})

	t.Run("linked project refreshes its template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		err := runTemplatesUpdateRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"templates", "update", "base", "--scope", "project")
		requests, reimported, projectHits := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		assert.Equal(t, []string{"tmpl-project-1"}, reimported)
		assert.Contains(t, projectHits, "GET /api/v1/projects/"+tuProjectID)
	})

	t.Run("outside a project the gate still applies", func(t *testing.T) {
		hub, srv := newTUHub(t)
		tuSetup(t)
		err := runTemplatesUpdateRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
			"templates", "update", "base", "--scope", "project")
		require.Error(t, err)
		_, reimported, _ := hub.snapshot()
		assert.Empty(t, reimported)
	})
}

func TestTemplatesUpdate_GlobalConflictsWithScope(t *testing.T) {
	for _, scope := range []string{"project", "user"} {
		t.Run(scope, func(t *testing.T) {
			hub, srv := newTUHub(t)
			tuSetup(t)
			err := runTemplatesUpdateRoot(t, "--hub", srv.URL, "--yes", "--non-interactive",
				"--global", "templates", "update", "base", "--scope", scope)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--global conflicts with --scope "+scope)
			requests, _, _ := hub.snapshot()
			assert.Empty(t, requests, "the conflict is rejected before any hub call")
		})
	}
}

// writeTUProjectSettings makes cwd a scion project linked to projectID on
// the hub.
func writeTUProjectSettings(t *testing.T, cwd, projectID string) {
	t.Helper()
	dir := filepath.Join(cwd, config.DotScion)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(map[string]any{
		"hub": map[string]any{"enabled": true, "project_id": projectID},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o644))
}
