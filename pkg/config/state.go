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

	"gopkg.in/yaml.v3"
)

// ProjectState holds runtime-managed state for a project.
// This is stored in state.yaml, separate from user-editable configuration.
type ProjectState struct {
	LastSyncedAt string   `yaml:"last_synced_at,omitempty"`
	SyncedAgents []string `yaml:"synced_agents,omitempty"`
}

// LoadProjectState reads project state from state.yaml in the given project path.
// Returns an empty ProjectState if the file doesn't exist.
func LoadProjectState(projectPath string) (*ProjectState, error) {
	statePath := filepath.Join(projectPath, "state.yaml")

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectState{}, nil
		}
		return nil, err
	}

	var state ProjectState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// SaveProjectState writes project state to state.yaml in the given project
// path, atomically and under the settings lock for that directory (see
// LockSettingsDir).
func SaveProjectState(projectPath string, state *ProjectState) error {
	lock, err := LockSettingsDir(projectPath)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return saveProjectStateLocked(lock, projectPath, state)
}

// saveProjectStateLocked is SaveProjectState for a caller that holds the
// settings lock for projectPath.
func saveProjectStateLocked(lock *SettingsLock, projectPath string, state *ProjectState) error {
	statePath := filepath.Join(projectPath, "state.yaml")

	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(state)
	if err != nil {
		return err
	}

	return writeSettingsFileAtomic(lock, statePath, data)
}

// UpdateProjectState loads state.yaml in projectPath, calls modify and saves
// the result, under the settings lock for projectPath from the read to the
// write (see LockSettingsDir), so a concurrent update from another process
// is not lost. When state.yaml cannot be read or parsed, modify gets an
// empty state and the error as loadErr and decides: return ErrSkipSave to
// write nothing, or nil to replace the file. ErrSkipSave from modify ends
// the update without writing and without error.
func UpdateProjectState(projectPath string, modify func(state *ProjectState, loadErr error) error) error {
	lock, err := LockSettingsDir(projectPath)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	state, loadErr := LoadProjectState(projectPath)
	if loadErr != nil {
		state = &ProjectState{}
	}
	if err := modify(state, loadErr); err != nil {
		if errors.Is(err, ErrSkipSave) {
			return nil
		}
		return err
	}
	return saveProjectStateLocked(lock, projectPath, state)
}
