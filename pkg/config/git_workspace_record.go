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
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
)

// RecordGitWorkspace records, in the external config of the split-storage
// git project whose in-repo .scion directory is projectDir, the repo root
// as workspace_path and project_type: git. Project discovery uses them to
// tell a config whose repo still exists from an orphaned one.
//
// workspace_path is written when it is unset, or when the recorded path no
// longer resolves to this config (the repo moved): a recorded path that
// still resolves, such as a second live clone, is kept. project_type is
// written only next to this root. Nothing is written when:
//   - the project is not a split-storage git project, or its external config
//     has no settings file yet;
//   - the settings file is empty or in the legacy format (it is not migrated
//     as a side effect);
//   - the root is not a repository's main checkout: <root>/.git is not a
//     directory (a linked worktree has a .git file), or the root lies inside
//     a .scion directory (an agent worktree).
func RecordGitWorkspace(projectDir string) error {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absDir); err != nil || !info.IsDir() || IsGlobalProjectDir(absDir) {
		return nil
	}
	root := filepath.Dir(absDir)
	if !isRecordableGitRoot(root) {
		return nil
	}
	extDir, err := GetGitProjectExternalConfigDir(absDir)
	if err != nil || extDir == "" {
		return err
	}
	settingsPath := GetSettingsPath(extDir)
	if settingsPath == "" {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	if version, _ := DetectSettingsFormat(data); version == "" {
		return nil
	}
	vs, err := LoadSingleFileVersioned(extDir)
	if err != nil {
		return err
	}
	if vs.ProjectType != "" && vs.ProjectType != string(ProjectTypeGit) {
		return nil
	}
	switch {
	case vs.WorkspacePath != "" && resolvePathForComparison(vs.WorkspacePath) == resolvePathForComparison(root):
	case vs.WorkspacePath != "" && gitWorkspaceResolvesTo(vs.WorkspacePath, extDir):
		return nil
	default:
		if err := UpdateSetting(absDir, "workspace_path", root, false); err != nil {
			return err
		}
	}
	if vs.ProjectType == "" {
		return setProjectTypeGit(extDir)
	}
	return nil
}

// isRecordableGitRoot reports whether root may be recorded as a git
// project's workspace: <root>/.git is a directory, and no component of root
// is .scion.
func isRecordableGitRoot(root string) bool {
	for p := root; ; {
		if filepath.Base(p) == DotScion {
			return false
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	info, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil && info.IsDir()
}

// healedGitWorkspaces holds the in-repo .scion directories that
// healGitWorkspaceOnce has already handled in this process.
var healedGitWorkspaces sync.Map

// healGitWorkspaceOnce runs RecordGitWorkspace for projectDir at most once
// per process, ignoring errors: it backfills the record for a project
// initialized before it was written, when the CLI resolves the project.
//
// It does nothing while internal/writesonread is set: a read-only command
// does not write the record.
func healGitWorkspaceOnce(projectDir string) {
	if writesonread.Disabled() {
		return
	}
	if _, done := healedGitWorkspaces.LoadOrStore(projectDir, true); done {
		return
	}
	_ = RecordGitWorkspace(projectDir)
}

// setProjectTypeGit records project_type: git in the settings file of the
// project config at configDir. project_type is scion-written bookkeeping, so
// it is not one of the keys UpdateSetting accepts. A YAML file is edited in
// place; a JSON file, or YAML the in-place edit cannot handle, goes through
// the struct round trip. A legacy-format file is not changed.
func setProjectTypeGit(configDir string) error {
	lock, err := LockSettingsDir(configDir)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	settingsPath := GetSettingsPath(configDir)
	if settingsPath == "" {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	if version, _ := DetectSettingsFormat(data); version == "" {
		// A legacy-format file is left alone here; UpdateSetting migrates
		// it when RecordGitWorkspace writes workspace_path.
		return nil
	}
	if filepath.Ext(settingsPath) != ".json" {
		edit := versionedSettingEdit{path: []string{"project_type"}, value: newYAMLStringScalar(string(ProjectTypeGit))}
		err := updateVersionedSettingYAML(lock, configDir, settingsPath, edit)
		if !errors.Is(err, errYAMLEditThroughAlias) {
			return err
		}
	}
	vs, err := loadSingleFileVersioned(lock, configDir)
	if err != nil {
		return err
	}
	if vs.ProjectType == string(ProjectTypeGit) {
		return nil
	}
	vs.ProjectType = string(ProjectTypeGit)
	return saveVersionedSettingsInPlace(lock, configDir, vs)
}
