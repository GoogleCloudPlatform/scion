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

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scopeGlobalHubID = "global-hub-project-id"
	scopeLocalID     = "0b5e6a1c-0000-4000-8000-0000000000a1"
)

// setupHubIDScopeHome points HOME at a fresh directory whose global settings
// are linked to a hub project (hub.project_id = scopeGlobalHubID, or the
// given raw hub block), and clears the project-ID environment. It returns
// the global directory.
func setupHubIDScopeHome(t *testing.T, globalHub string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS", "SCION_PROJECT",
		projectkeys.EnvProjectID, projectkeys.EnvHubProjectID)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	if globalHub == "" {
		globalHub = "  project_id: \"" + scopeGlobalHubID + "\"\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  enabled: true\n  linked: true\n"+globalHub), 0o644))
	return globalDir
}

// setupHubIDScopeSplitProject creates a git repo whose .scion has a
// project-id file (split storage) and settings that are linked but have no
// hub project ID of their own, the state 'scion hub unlink' plus an older
// 'scion hub link' left behind. It returns the in-repo .scion path and the
// external settings directory.
func setupHubIDScopeSplitProject(t *testing.T, home string) (scionDir, extDir string) {
	t.Helper()
	repo := filepath.Join(home, "work", "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	scionDir = filepath.Join(repo, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, projectkeys.ProjectIDFile), []byte(scopeLocalID+"\n"), 0o644))
	extDir = GetProjectConfigDir(scionDir)
	require.NotEqual(t, scionDir, extDir, "split storage puts settings outside the repo")
	require.NoError(t, UpdateSetting(scionDir, "hub.enabled", "true", false))
	require.NoError(t, UpdateSetting(scionDir, "hub.linked", "true", false))
	return scionDir, extDir
}

// TestLoadSettings_GlobalHubProjectIDDoesNotLeakIntoProject checks that a
// project with no hub project ID of its own does not inherit the one in the
// global settings: the hub project ID is empty and the project resolves to
// its local project_id. It covers an explicit project path and a project
// found from the working directory, with both settings loaders.
func TestLoadSettings_GlobalHubProjectIDDoesNotLeakIntoProject(t *testing.T) {
	globalDir := setupHubIDScopeHome(t, "")
	scionDir, _ := setupHubIDScopeSplitProject(t, filepath.Dir(globalDir))

	for name, path := range map[string]string{"explicit path": scionDir, "working directory": ""} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(filepath.Dir(scionDir))
			s, err := LoadSettings(path)
			require.NoError(t, err)
			assert.Empty(t, s.GetHubProjectID(), "the global hub project ID must not leak into the project")
			assert.Equal(t, scopeLocalID, s.ProjectID)

			vs, err := LoadVersionedSettings(path)
			require.NoError(t, err)
			require.NotNil(t, vs.Hub)
			assert.Equal(t, scopeLocalID, vs.Hub.ProjectID)
		})
	}
}

// TestLoadSettings_ProjectOwnHubProjectIDWins checks that the project's own
// hub project ID is used: from the external directory of a split-storage
// project (where 'scion hub link' writes it), and from the in-repo settings
// of a plain project.
func TestLoadSettings_ProjectOwnHubProjectIDWins(t *testing.T) {
	t.Run("split storage external dir", func(t *testing.T) {
		globalDir := setupHubIDScopeHome(t, "")
		scionDir, extDir := setupHubIDScopeSplitProject(t, filepath.Dir(globalDir))
		require.NoError(t, UpdateSetting(scionDir, "hub.projectId", "own-hub-id", false))
		own, err := LoadSettingsFromDir(extDir)
		require.NoError(t, err)
		require.Equal(t, "own-hub-id", own.GetHubProjectID(), "the link writes to the external directory")

		s, err := LoadSettings(scionDir)
		require.NoError(t, err)
		assert.Equal(t, "own-hub-id", s.GetHubProjectID())
		assert.Equal(t, scopeLocalID, s.ProjectID, "the local project_id is unchanged")
	})

	t.Run("plain project in-repo settings", func(t *testing.T) {
		globalDir := setupHubIDScopeHome(t, "")
		projectDir := filepath.Join(filepath.Dir(globalDir), "plain", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nhub:\n  enabled: true\n  project_id: \"plain-own-id\"\n"), 0o644))

		s, err := LoadSettings(projectDir)
		require.NoError(t, err)
		assert.Equal(t, "plain-own-id", s.GetHubProjectID())
	})
}

// TestLoadSettings_EnvHubProjectIDStillApplies checks that only the global
// file's value is dropped: SCION_HUB_PROJECT_ID still applies on top.
func TestLoadSettings_EnvHubProjectIDStillApplies(t *testing.T) {
	globalDir := setupHubIDScopeHome(t, "")
	projectDir := filepath.Join(filepath.Dir(globalDir), "plain", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  enabled: true\n"), 0o644))
	t.Setenv(projectkeys.EnvHubProjectID, "env-hub-id")

	s, err := LoadSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "env-hub-id", s.ProjectID, "the legacy loader maps the variable to project_id")
	assert.NotEqual(t, scopeGlobalHubID, s.GetHubProjectID())

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "env-hub-id", vs.Hub.ProjectID)
}

// TestLoadSettings_GlobalLegacyHubKeyDoesNotLeak checks the legacy key name
// in the global file: it is migrated to hub.project_id when the global
// layer loads, and must not leak into a project either.
func TestLoadSettings_GlobalLegacyHubKeyDoesNotLeak(t *testing.T) {
	legacy := hubGroveIDRename[0].legacy
	globalDir := setupHubIDScopeHome(t, "  "+legacy+": \""+scopeGlobalHubID+"\"\n")
	scionDir, _ := setupHubIDScopeSplitProject(t, filepath.Dir(globalDir))

	s, err := LoadSettings(scionDir)
	require.NoError(t, err)
	assert.Empty(t, s.GetHubProjectID())
	assert.Equal(t, scopeLocalID, s.ProjectID)

	// The global project still sees its own (migrated) value.
	g, err := LoadSettings(globalDir)
	require.NoError(t, err)
	assert.Equal(t, scopeGlobalHubID, g.GetHubProjectID())
}

// TestLoadSettings_GlobalProjectKeepsHubProjectID checks that the global
// project, and a directory with no project of its own (which resolves as the
// global project), still use the global hub project ID.
func TestLoadSettings_GlobalProjectKeepsHubProjectID(t *testing.T) {
	globalDir := setupHubIDScopeHome(t, "")
	outside := t.TempDir()
	t.Chdir(outside)

	for name, path := range map[string]string{"global dir": globalDir, "global alias": "global", "no project": ""} {
		t.Run(name, func(t *testing.T) {
			s, err := LoadSettings(path)
			require.NoError(t, err)
			assert.Equal(t, scopeGlobalHubID, s.GetHubProjectID())

			vs, err := LoadVersionedSettings(path)
			require.NoError(t, err)
			require.NotNil(t, vs.Hub)
			assert.Equal(t, scopeGlobalHubID, vs.Hub.ProjectID)
		})
	}
}

func TestLoadsProjectLayer(t *testing.T) {
	const g = "/home/u/.scion"
	assert.False(t, loadsProjectLayer("", "", g))
	assert.False(t, loadsProjectLayer(g, g, g))
	assert.False(t, loadsProjectLayer("global", "", g))
	assert.False(t, loadsProjectLayer("home", "", g))
	assert.False(t, loadsProjectLayer("", g, g), "a working directory that resolves to the global dir")
	assert.True(t, loadsProjectLayer("/r/.scion", "/r/.scion", g))
	assert.True(t, loadsProjectLayer("/r/.scion", "/home/u/.scion/project-configs/r", g))
	assert.True(t, loadsProjectLayer("", "/r/.scion", g), "a project found from the working directory")
}
