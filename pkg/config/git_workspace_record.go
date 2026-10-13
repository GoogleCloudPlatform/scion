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
)

// RecordGitWorkspace records, in the external config of the split-storage
// git project whose in-repo .scion directory is projectDir, the repo root
// as workspace_path and project_type: git. Project discovery uses them to
// tell a config whose repo still exists from an orphaned one. Only missing
// values are written: a workspace_path that is already set, to this root or
// any other, is kept, and project_type is written only next to this root.
// It does nothing for a project that is not a split-storage git project, or
// whose external config has no settings file yet.
func RecordGitWorkspace(projectDir string) error {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absDir); err != nil || !info.IsDir() || IsGlobalProjectDir(absDir) {
		return nil
	}
	extDir, err := GetGitProjectExternalConfigDir(absDir)
	if err != nil || extDir == "" || GetSettingsPath(extDir) == "" {
		return err
	}
	vs, err := LoadSingleFileVersioned(extDir)
	if err != nil {
		return err
	}
	if vs.ProjectType != "" && vs.ProjectType != string(ProjectTypeGit) {
		return nil
	}
	root := filepath.Dir(absDir)
	if vs.WorkspacePath == "" {
		if err := UpdateSetting(absDir, "workspace_path", root, false); err != nil {
			return err
		}
	} else if resolvePathForComparison(vs.WorkspacePath) != resolvePathForComparison(root) {
		return nil
	}
	if vs.ProjectType == "" {
		return setProjectTypeGit(extDir)
	}
	return nil
}

// healedGitWorkspaces holds the in-repo .scion directories that
// healGitWorkspaceOnce has already handled in this process.
var healedGitWorkspaces sync.Map

// healGitWorkspaceOnce runs RecordGitWorkspace for projectDir at most once
// per process, ignoring errors: it backfills the record for a project
// initialized before it was written, when the CLI resolves the project.
func healGitWorkspaceOnce(projectDir string) {
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
	unlock := LockSettingsFile()
	defer unlock()

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
		err := updateVersionedSettingYAML(configDir, settingsPath, edit)
		if !errors.Is(err, errYAMLEditThroughAlias) {
			return err
		}
	}
	vs, err := LoadSingleFileVersioned(configDir)
	if err != nil {
		return err
	}
	if vs.ProjectType == string(ProjectTypeGit) {
		return nil
	}
	vs.ProjectType = string(ProjectTypeGit)
	return saveVersionedSettingsInPlace(configDir, vs)
}
