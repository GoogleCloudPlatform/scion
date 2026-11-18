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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Tests for ptone/scion#8318: 'scion templates delete' (and singular
// 'template delete') defaults to project scope (the narrowest scope), requires
// --scope global or --global to delete a global template, and refuses residual
// multi-scope matches under auto-confirm in ResolveTemplateForDelete.

func createProjectTestTemplate(t *testing.T, projectRoot, name string) string {
	t.Helper()
	templateDir := filepath.Join(projectRoot, config.DotScion, "templates", name)
	require.NoError(t, os.MkdirAll(templateDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(templateDir, "scion-agent.json"),
		[]byte(`{"harness":"claude"}`),
		0o644,
	))
	return templateDir
}

// newGlobalOnlyTUHub returns a fake Hub where "glob-only" exists only in
// global scope and tuProjectID has no templates.
func newGlobalOnlyTUHub(t *testing.T) (*tuHub, *httptest.Server) {
	t.Helper()
	h := &tuHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.RequestURI())
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
				{"id": tuGlobalTemplateID, "name": "glob-only", "slug": "glob-only", "scope": "global", "status": "active"},
			}
			out := []map[string]string{}
			for _, tmpl := range all {
				if s := r.URL.Query().Get("scope"); s != "" && tmpl["scope"] != s {
					continue
				}
				out = append(out, tmpl)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": out})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/templates/"):
			h.mu.Lock()
			h.deleted = append(h.deleted, strings.TrimPrefix(r.URL.Path, "/api/v1/templates/"))
			h.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": "not found"}})
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// TestTemplatesDelete_GlobalOnlyNotDeletedWithoutGlobalScope verifies that an
// unscoped auto-confirm delete inside a project never widens to a global-only
// template: it deletes nothing and reports how to use --scope global.
func TestTemplatesDelete_GlobalOnlyNotDeletedWithoutGlobalScope(t *testing.T) {
	t.Run("--non-interactive on Hub", func(t *testing.T) {
		hub, srv := newGlobalOnlyTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "glob-only")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.Contains(t, err.Error(), "--scope global")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted, "unscoped delete in a project must not delete a global template")
	})

	t.Run("--yes on Hub", func(t *testing.T) {
		hub, srv := newGlobalOnlyTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--yes", "templates", "delete", "glob-only")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.Contains(t, err.Error(), "--scope global")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted)
	})

	t.Run("cli.interactive_disabled setting on Hub", func(t *testing.T) {
		hub, srv := newGlobalOnlyTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			filepath.Join(home, config.DotScion, "settings.yaml"),
			[]byte("schema_version: \"1\"\ncli:\n  interactive_disabled: true\n"),
			0o644,
		))

		err = runTemplatesRoot(t, "templates", "delete", "glob-only")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.Contains(t, err.Error(), "--scope global")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted)
	})

	t.Run("agent mode on Hub", func(t *testing.T) {
		hub, srv := newGlobalOnlyTUHub(t)
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_PROJECT_ID", tuProjectID)
		t.Setenv("SCION_AUTH_TOKEN", "agent-token-test")

		err := runTemplatesRoot(t, "templates", "delete", "glob-only")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.Contains(t, err.Error(), "--scope global")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted, "agent mode must not delete a global-only template without --scope global")
	})

	t.Run("singular 'template delete' alias on Hub", func(t *testing.T) {
		hub, srv := newGlobalOnlyTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "template", "delete", "glob-only")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.Contains(t, err.Error(), "--scope global")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted)
	})

	t.Run("local global-only template inside a project under --local", func(t *testing.T) {
		for _, agentMode := range []bool{false, true} {
			name := "--non-interactive"
			args := []string{"--non-interactive", "templates", "delete", "glob-only", "--local"}
			if agentMode {
				name = "agent mode"
				args = []string{"templates", "delete", "glob-only", "--local"}
			}
			t.Run(name, func(t *testing.T) {
				cwd := tuSetup(t)
				require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion, "templates"), 0o755))
				home, err := os.UserHomeDir()
				require.NoError(t, err)
				globTpl := createTestTemplate(t, home, "glob-only")
				if agentMode {
					t.Setenv("SCION_CLI_MODE", "agent")
				}

				err = runTemplatesRoot(t, args...)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "not found")
				assert.Contains(t, err.Error(), "--scope global")
				assert.DirExists(t, globTpl, "global template must remain untouched")
			})
		}
	})
}

// TestTemplatesDelete_UnscopedDefaultsToProjectScope verifies Expected #1 of
// ptone/scion#8318: when both a project template and a global template share a
// name, an unscoped delete defaults to project scope and removes only the
// project template, leaving the global template intact.
func TestTemplatesDelete_UnscopedDefaultsToProjectScope(t *testing.T) {
	t.Run("--non-interactive on Hub removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("--yes on Hub removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--yes", "templates", "delete", "base")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("cli.interactive_disabled setting on Hub removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			filepath.Join(home, config.DotScion, "settings.yaml"),
			[]byte("schema_version: \"1\"\ncli:\n  interactive_disabled: true\n"),
			0o644,
		))

		err = runTemplatesRoot(t, "templates", "delete", "base")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("agent mode on Hub removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_PROJECT_ID", tuProjectID)
		t.Setenv("SCION_AUTH_TOKEN", "agent-token-test")

		err := runTemplatesRoot(t, "templates", "delete", "base")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("singular 'template delete' alias removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "template", "delete", "base")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("local project and local global templates under --local", func(t *testing.T) {
		for _, agentMode := range []bool{false, true} {
			name := "--non-interactive"
			args := []string{"--non-interactive", "templates", "delete", "shared-role", "--local"}
			if agentMode {
				name = "agent mode"
				args = []string{"templates", "delete", "shared-role", "--local"}
			}
			t.Run(name, func(t *testing.T) {
				cwd := tuSetup(t)
				home, err := os.UserHomeDir()
				require.NoError(t, err)
				projTpl := createProjectTestTemplate(t, cwd, "shared-role")
				globTpl := createTestTemplate(t, home, "shared-role")
				if agentMode {
					t.Setenv("SCION_CLI_MODE", "agent")
				}
				require.NoError(t, runTemplatesRoot(t, args...))
				assert.NoDirExists(t, projTpl)
				assert.DirExists(t, globTpl, "unscoped delete must leave local global template untouched")
			})
		}
	})

	t.Run("both local and Hub project copies deleted while global copies stay", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		projTpl := createProjectTestTemplate(t, cwd, "base")
		globTpl := createTestTemplate(t, home, "base")

		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base"))
		assert.NoDirExists(t, projTpl)
		assert.DirExists(t, globTpl)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})
}

// TestResolveTemplateForDelete_RefusesResidualMultiScopeUnderAutoConfirm
// verifies defense-in-depth in ResolveTemplateForDelete: if ResolveOpts allows
// multiple scopes and matches span >1 scope under AutoConfirm, it refuses with
// an ambiguity error naming --scope instead of deleting across scopes.
func TestResolveTemplateForDelete_RefusesResidualMultiScopeUnderAutoConfirm(t *testing.T) {
	t.Run("local multi-scope match refused under AutoConfirm", func(t *testing.T) {
		cwd := tuSetup(t)
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		createProjectTestTemplate(t, cwd, "shared-role")
		createTestTemplate(t, home, "shared-role")

		matches, err := ResolveTemplateForDelete(context.Background(), "shared-role", nil, &ResolveOpts{
			LocalOnly:   true,
			AutoConfirm: true,
		})
		require.Error(t, err)
		assert.Nil(t, matches)
		assert.Contains(t, err.Error(), "exists in several scopes (project, global)")
		assert.Contains(t, err.Error(), "--scope")
	})

	t.Run("Hub multi-scope match refused under AutoConfirm", func(t *testing.T) {
		_, srv := newTUHub(t)
		tuSetup(t)
		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)
		hubCtx := &HubContext{
			Client:    client,
			Endpoint:  srv.URL,
			ProjectID: tuProjectID,
		}

		matches, err := ResolveTemplateForDelete(context.Background(), "base", hubCtx, &ResolveOpts{
			HubOnly:     true,
			AutoConfirm: true,
		})
		require.Error(t, err)
		assert.Nil(t, matches)
		assert.Contains(t, err.Error(), "exists in several scopes (project, global)")
		assert.Contains(t, err.Error(), "--scope")
	})
}

// TestTemplatesDelete_ScopedRemovesOnlyChosenScope covers Expected #1 and #3
// of ptone/scion#8318: --scope project and --scope global delete only the
// matching template in that scope, in both --non-interactive and agent mode,
// and on both 'templates delete' and the singular 'template delete' alias.
func TestTemplatesDelete_ScopedRemovesOnlyChosenScope(t *testing.T) {
	t.Run("--scope project under --non-interactive removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base", "--scope", "project")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("--scope project in agent mode removes only project template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_PROJECT_ID", tuProjectID)
		t.Setenv("SCION_AUTH_TOKEN", "agent-token-test")

		err := runTemplatesRoot(t, "templates", "delete", "base", "--scope", "project")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("singular 'template delete --scope project' alias", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "template", "delete", "base", "--scope", "project")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{"tmpl-project-1"}, hub.deleted)
	})

	t.Run("--scope global removes only global template and skips project lookup", func(t *testing.T) {
		for _, inProject := range []bool{false, true} {
			label := "outside a project"
			if inProject {
				label = "inside an unreadable linked project"
			}
			t.Run(label, func(t *testing.T) {
				hub, srv := newTUHub(t)
				cwd := tuSetup(t)
				if inProject {
					writeTUProjectSettings(t, cwd, "unreadable-project-id")
				}
				tuHubSettings(t, srv.URL)

				err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base", "--scope", "global")
				requests, _, projectHits := hub.snapshot()
				require.NoError(t, err, "requests: %v", requests)
				assert.Empty(t, projectHits, "a --scope global delete must not read any project")
				hub.mu.Lock()
				defer hub.mu.Unlock()
				assert.Equal(t, []string{tuGlobalTemplateID}, hub.deleted)
			})
		}
	})

	t.Run("--scope global in agent mode removes only global template", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_PROJECT_ID", tuProjectID)
		t.Setenv("SCION_AUTH_TOKEN", "agent-token-test")

		err := runTemplatesRoot(t, "templates", "delete", "base", "--scope", "global")
		requests, _, _ := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuGlobalTemplateID}, hub.deleted)
	})

	t.Run("local templates with --scope project and --scope global", func(t *testing.T) {
		cwd := tuSetup(t)
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		projTpl := createProjectTestTemplate(t, cwd, "shared-role")
		globTpl := createTestTemplate(t, home, "shared-role")

		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "delete", "shared-role", "--local", "--scope", "project"))
		assert.NoDirExists(t, projTpl)
		assert.DirExists(t, globTpl, "--scope project must leave local global template untouched")

		projTpl = createProjectTestTemplate(t, cwd, "shared-role")
		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "delete", "shared-role", "--local", "--scope", "global"))
		assert.DirExists(t, projTpl, "--scope global must leave local project template untouched")
		assert.NoDirExists(t, globTpl)
	})
}

func TestTemplatesDelete_ScopeValidationAndConflicts(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown scope":                {"--non-interactive", "templates", "delete", "base", "--scope", "galaxy"},
		"user scope not accepted":      {"--non-interactive", "templates", "delete", "base", "--scope", "user"},
		"--global conflicts w project": {"--non-interactive", "--global", "templates", "delete", "base", "--scope", "project"},
	} {
		t.Run(name, func(t *testing.T) {
			hub, srv := newTUHub(t)
			cwd := tuSetup(t)
			writeTUProjectSettings(t, cwd, tuProjectID)
			tuHubSettings(t, srv.URL)

			err := runTemplatesRoot(t, args...)
			require.Error(t, err)
			assert.True(t, isUsageError(err), "expected usage error, got: %v", err)
			requests, _, _ := hub.snapshot()
			assert.Empty(t, requests, "flag validation must fail before any Hub call")
		})
	}

	t.Run("--global with --scope global is accepted", func(t *testing.T) {
		hub, srv := newTUHub(t)
		tuSetup(t)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "--global", "templates", "delete", "base", "--scope", "global")
		requests, _, projectHits := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		assert.Empty(t, projectHits)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuGlobalTemplateID}, hub.deleted)
	})

	t.Run("--scope project outside a project still fails project gate", func(t *testing.T) {
		hub, srv := newTUHub(t)
		tuSetup(t)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base", "--scope", "project")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not in a scion project")
		requests, _, _ := hub.snapshot()
		assert.Empty(t, requests)
	})

	t.Run("singular 'template delete --scope global' runs outside a project", func(t *testing.T) {
		hub, srv := newTUHub(t)
		tuSetup(t)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "template", "delete", "base", "--scope", "global")
		requests, _, projectHits := hub.snapshot()
		require.NoError(t, err, "requests: %v", requests)
		assert.Empty(t, projectHits)
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Equal(t, []string{tuGlobalTemplateID}, hub.deleted)
	})
}

func TestTemplatesDelete_UnscopedProjectOnlyMatchSucceeds(t *testing.T) {
	cwd := tuSetup(t)
	projTpl := createProjectTestTemplate(t, cwd, "proj-only")

	require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "delete", "proj-only", "--local"))
	assert.NoDirExists(t, projTpl)
}
