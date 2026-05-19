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

	"gopkg.in/yaml.v3"
)

// ProjectState holds runtime-managed state for a grove.
// This is stored in state.yaml, separate from user-editable configuration.
type ProjectState struct {
	LastSyncedAt string   `yaml:"last_synced_at,omitempty"`
	SyncedAgents []string `yaml:"synced_agents,omitempty"`
}

// LoadProjectState reads grove state from state.yaml in the given grove path.
// Returns an empty ProjectState if the file doesn't exist.
func LoadProjectState(grovePath string) (*ProjectState, error) {
	statePath := filepath.Join(grovePath, "state.yaml")

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

// SaveProjectState writes grove state to state.yaml in the given grove path.
func SaveProjectState(grovePath string, state *ProjectState) error {
	statePath := filepath.Join(grovePath, "state.yaml")

	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(state)
	if err != nil {
		return err
	}

	return os.WriteFile(statePath, data, 0644)
}
