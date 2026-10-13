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
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gitEvidenceProjectID = "11112222-3333-4444-5555-666677778888"

// gitEvidenceFixture is a split-storage git project: a repo with an in-repo
// .scion directory holding a project-id file, and its external config.
type gitEvidenceFixture struct {
	home      string
	repo      string
	inRepo    string // <repo>/.scion
	configDir string // ~/.scion/project-configs/<slug>__<short-id>/.scion
}

// newGitEvidenceFixture creates the fixture under a fresh HOME. settings is
// the content of the external settings.yaml; agents is the number of agent
// directories in the external config.
func newGitEvidenceFixture(t *testing.T, settings string, agents int) gitEvidenceFixture {
	t.Helper()
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_PROJECT"} {
		if _, ok := os.LookupEnv(e); ok {
			t.Setenv(e, "")
			require.NoError(t, os.Unsetenv(e))
		}
	}
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))

	repo := filepath.Join(home, "src", "myrepo")
	inRepo := filepath.Join(repo, DotScion)
	require.NoError(t, os.MkdirAll(inRepo, 0o755))
	require.NoError(t, WriteProjectID(inRepo, gitEvidenceProjectID))
	configDir, err := GetGitProjectExternalConfigDir(inRepo)
	require.NoError(t, err)
	require.NotEmpty(t, configDir)
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "agents"), 0o755))
	for i := 0; i < agents; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(configDir, "agents", "agent"+string(rune('a'+i)), "home"), 0o755))
	}
	if settings != "" {
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "settings.yaml"), []byte(settings), 0o644))
	}
	return gitEvidenceFixture{home: home, repo: repo, inRepo: inRepo, configDir: configDir}
}

func recordedSettings(workspace string) string {
	return "schema_version: \"1\"\nproject_type: git\nworkspace_path: " + workspace + "\n"
}

func (f gitEvidenceFixture) discover(t *testing.T) ProjectInfo {
	t.Helper()
	projects, err := DiscoverProjects()
	require.NoError(t, err)
	for _, p := range projects {
		if p.Name == "myrepo" {
			return p
		}
	}
	t.Fatalf("project myrepo not discovered in %+v", projects)
	return ProjectInfo{}
}

func orphanedNames(t *testing.T) []string {
	t.Helper()
	orphaned, err := FindOrphanedProjectConfigs()
	require.NoError(t, err)
	var names []string
	for _, p := range orphaned {
		names = append(names, p.Name)
	}
	return names
}

func TestGitEvidence_LiveRepoWithoutAgentsNotOrphaned(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"), []byte(recordedSettings(f.repo)), 0o644))

	p := f.discover(t)
	assert.Equal(t, ProjectTypeGit, p.Type)
	assert.Equal(t, ProjectStatusOK, p.Status)
	assert.Equal(t, f.repo, p.WorkspacePath)
	assert.NotContains(t, orphanedNames(t), "myrepo")
}

func TestGitEvidence_NoRecordedPathIsUnverifiedAndNotPruned(t *testing.T) {
	f := newGitEvidenceFixture(t, "schema_version: \"1\"\n", 0)

	p := f.discover(t)
	assert.Equal(t, ProjectTypeGit, p.Type)
	assert.Equal(t, ProjectStatusUnverified, p.Status)
	assert.Empty(t, p.WorkspacePath)
	assert.NotContains(t, orphanedNames(t), "myrepo")
}

func TestGitEvidence_RecordedPathDeletedIsOrphaned(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"), []byte(recordedSettings(f.repo)), 0o644))
	require.NoError(t, os.RemoveAll(f.repo))

	p := f.discover(t)
	assert.Equal(t, ProjectTypeGit, p.Type)
	assert.Equal(t, ProjectStatusOrphaned, p.Status)
	assert.Equal(t, []string{"myrepo"}, orphanedNames(t))
}

func TestGitEvidence_RepoRepointedToAnotherConfigIsOrphaned(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"), []byte(recordedSettings(f.repo)), 0o644))
	// Re-initialized: the repo's project-id now names another config.
	require.NoError(t, WriteProjectID(f.inRepo, "99998888-7777-6666-5555-444433332222"))

	p := f.discover(t)
	assert.Equal(t, ProjectStatusOrphaned, p.Status)
	assert.Equal(t, []string{"myrepo"}, orphanedNames(t))
}

func TestGitEvidence_RepoRepointedThroughMarkerFileIsOrphaned(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"), []byte(recordedSettings(f.repo)), 0o644))
	require.NoError(t, os.RemoveAll(f.inRepo))
	require.NoError(t, WriteWorkspaceMarker(f.repo, "99998888-7777-6666-5555-444433332222", "myrepo", "myrepo"))

	assert.Equal(t, ProjectStatusOrphaned, f.discover(t).Status)
}

func TestGitEvidence_SettingsProjectIDDifferentFromRepoIDStillOK(t *testing.T) {
	// hub link may record a settings project_id that differs from the repo's
	// project-id file; that alone is not evidence of an orphan.
	f := newGitEvidenceFixture(t, "", 0)
	settings := recordedSettings(f.repo) + "hub:\n  project_id: 99998888-7777-6666-5555-444433332222\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"), []byte(settings), 0o644))

	assert.Equal(t, ProjectStatusOK, f.discover(t).Status)
}

func TestGitEvidence_MissingWorkspaceWithAgentsStaysOK(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 2)
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"), []byte(recordedSettings(f.repo)), 0o644))
	require.NoError(t, os.RemoveAll(f.repo))

	p := f.discover(t)
	assert.Equal(t, ProjectTypeGit, p.Type)
	assert.Equal(t, ProjectStatusOK, p.Status)
	assert.Equal(t, 2, p.AgentCount)
	assert.Equal(t, f.repo, p.WorkspacePath, "the missing path is shown")
	assert.NotContains(t, orphanedNames(t), "myrepo")
}

func TestGitEvidence_WorkspacePathWithoutTypeRoutedAsGitAndHealed(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)
	require.NoError(t, os.WriteFile(filepath.Join(f.configDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nworkspace_path: "+f.repo+"\n"), 0o644))

	p := f.discover(t)
	assert.Equal(t, ProjectTypeGit, p.Type)
	assert.Equal(t, ProjectStatusOK, p.Status)

	vs, err := LoadSingleFileVersioned(f.configDir)
	require.NoError(t, err)
	assert.Equal(t, "git", vs.ProjectType)
	assert.Equal(t, f.repo, vs.WorkspacePath)
}

func TestGitEvidence_ExternalProjectUnchanged(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)
	// A non-git external project beside the git one: marker file workspace.
	ws := filepath.Join(f.home, "plain")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	marker := &ProjectMarker{ProjectID: "aaaabbbb-0000-0000-0000-000000000000", ProjectName: "plain", ProjectSlug: "plain"}
	require.NoError(t, WriteProjectMarker(filepath.Join(ws, DotScion), marker))
	extDir, err := marker.ExternalProjectPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(extDir, "agents"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(extDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nworkspace_path: "+ws+"\n"), 0o644))

	find := func() ProjectInfo {
		projects, err := DiscoverProjects()
		require.NoError(t, err)
		for _, p := range projects {
			if p.Name == "plain" {
				return p
			}
		}
		t.Fatal("plain not discovered")
		return ProjectInfo{}
	}
	p := find()
	assert.Equal(t, ProjectTypeExternal, p.Type)
	assert.Equal(t, ProjectStatusOK, p.Status)
	vs, err := LoadSingleFileVersioned(extDir)
	require.NoError(t, err)
	assert.Empty(t, vs.ProjectType, "an external project is not healed")

	require.NoError(t, os.RemoveAll(ws))
	p = find()
	assert.Equal(t, ProjectTypeExternal, p.Type)
	assert.Equal(t, ProjectStatusOrphaned, p.Status)
	assert.Contains(t, orphanedNames(t), "plain")
}

func TestRecordGitWorkspace_WritesOnceAndIsIdempotent(t *testing.T) {
	f := newGitEvidenceFixture(t, "schema_version: \"1\"\n# keep me\n", 0)

	require.NoError(t, RecordGitWorkspace(f.inRepo))
	vs, err := LoadSingleFileVersioned(f.configDir)
	require.NoError(t, err)
	assert.Equal(t, f.repo, vs.WorkspacePath)
	assert.Equal(t, "git", vs.ProjectType)

	settingsPath := filepath.Join(f.configDir, "settings.yaml")
	first, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(first), "# keep me", "the file is edited in place")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(settingsPath, past, past))

	require.NoError(t, RecordGitWorkspace(f.inRepo))
	second, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	info, err := os.Stat(settingsPath)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(past), "a second record rewrote the file")
}

func TestRecordGitWorkspace_KeepsExistingPath(t *testing.T) {
	other := filepath.Join(t.TempDir(), "elsewhere")
	f := newGitEvidenceFixture(t, "schema_version: \"1\"\nworkspace_path: "+other+"\n", 0)

	require.NoError(t, RecordGitWorkspace(f.inRepo))
	vs, err := LoadSingleFileVersioned(f.configDir)
	require.NoError(t, err)
	assert.Equal(t, other, vs.WorkspacePath)
	assert.Empty(t, vs.ProjectType, "project_type is written only next to this repo's root")
}

func TestRecordGitWorkspace_NoSettingsFileIsNoop(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)

	require.NoError(t, RecordGitWorkspace(f.inRepo))
	assert.Empty(t, GetSettingsPath(f.configDir))
}

func TestFindProjectRoot_HealsGitWorkspaceRecord(t *testing.T) {
	f := newGitEvidenceFixture(t, "schema_version: \"1\"\n", 0)
	healedGitWorkspaces.Delete(f.inRepo)
	t.Cleanup(func() { healedGitWorkspaces.Delete(f.inRepo) })
	t.Chdir(f.repo)

	root, ok := FindProjectRoot()
	require.True(t, ok)
	assert.Equal(t, f.inRepo, root)

	vs, err := LoadSingleFileVersioned(f.configDir)
	require.NoError(t, err)
	assert.Equal(t, f.repo, vs.WorkspacePath)
	assert.Equal(t, "git", vs.ProjectType)
	assert.Equal(t, ProjectStatusOK, f.discover(t).Status)
}

func TestInitInRepoProject_RecordsGitWorkspace(t *testing.T) {
	f := newGitEvidenceFixture(t, "", 0)

	require.NoError(t, initInRepoProject(f.inRepo, InitProjectOpts{SkipRuntimeCheck: true}))
	vs, err := LoadSingleFileVersioned(f.configDir)
	require.NoError(t, err)
	assert.Equal(t, f.repo, vs.WorkspacePath)
	assert.Equal(t, "git", vs.ProjectType)

	p := f.discover(t)
	assert.Equal(t, ProjectTypeGit, p.Type)
	assert.Equal(t, ProjectStatusOK, p.Status)
}
