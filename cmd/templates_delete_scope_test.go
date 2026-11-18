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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for ptone/scion#8318: 'scion templates delete' supports --scope
// (project, global) and refuses cross-scope matches under implicit or
// explicit auto-confirm instead of deleting every matching template across
// scopes.

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

// TestTemplatesDelete_SameNameAcrossScopesRefusedUnderAutoConfirm covers
// Expected #2 and #3 of ptone/scion#8318: when both a project template and a
// global template share a name, an unscoped auto-confirm delete (--non-interactive,
// --yes, cli.interactive_disabled, or agent mode) fails with an ambiguity
// error naming --scope and deletes neither template.
func TestTemplatesDelete_SameNameAcrossScopesRefusedUnderAutoConfirm(t *testing.T) {
	t.Run("--non-interactive on Hub", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--non-interactive", "templates", "delete", "base")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exists in several scopes")
		assert.Contains(t, err.Error(), "--scope")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted, "nothing may be deleted when scopes are ambiguous")
	})

	t.Run("--yes on Hub", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		writeTUProjectSettings(t, cwd, tuProjectID)
		tuHubSettings(t, srv.URL)

		err := runTemplatesRoot(t, "--yes", "templates", "delete", "base")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exists in several scopes")
		assert.Contains(t, err.Error(), "--scope")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted)
	})

	t.Run("cli.interactive_disabled setting on Hub", func(t *testing.T) {
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
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exists in several scopes")
		assert.Contains(t, err.Error(), "--scope")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted)
	})

	t.Run("agent mode on Hub", func(t *testing.T) {
		hub, srv := newTUHub(t)
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion), 0o755))
		t.Setenv("SCION_CLI_MODE", "agent")
		t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
		t.Setenv("SCION_PROJECT_ID", tuProjectID)
		t.Setenv("SCION_AUTH_TOKEN", "agent-token-test")

		err := runTemplatesRoot(t, "templates", "delete", "base")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exists in several scopes")
		assert.Contains(t, err.Error(), "--scope")
		hub.mu.Lock()
		defer hub.mu.Unlock()
		assert.Empty(t, hub.deleted, "agent mode must not delete same-named global template")
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
				err = runTemplatesRoot(t, args...)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "exists in several scopes")
				assert.Contains(t, err.Error(), "--scope")
				assert.DirExists(t, projTpl)
				assert.DirExists(t, globTpl)
			})
		}
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

func TestTemplatesDelete_UnscopedSingleScopeMatchSucceeds(t *testing.T) {
	t.Run("only project template exists", func(t *testing.T) {
		cwd := tuSetup(t)
		projTpl := createProjectTestTemplate(t, cwd, "proj-only")

		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "delete", "proj-only", "--local"))
		assert.NoDirExists(t, projTpl)
	})

	t.Run("only global template exists inside a project", func(t *testing.T) {
		cwd := tuSetup(t)
		require.NoError(t, os.MkdirAll(filepath.Join(cwd, config.DotScion, "templates"), 0o755))
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		globTpl := createTestTemplate(t, home, "glob-only")

		require.NoError(t, runTemplatesRoot(t, "--non-interactive", "templates", "delete", "glob-only", "--local"))
		assert.NoDirExists(t, globTpl)
	})
}
