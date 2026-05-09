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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadProjectState_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	state, err := LoadProjectState(tmpDir)
	require.NoError(t, err)
	assert.Equal(t, "", state.LastSyncedAt)
}

func TestSaveAndLoadProjectState_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0755))

	// Save state
	state := &ProjectState{LastSyncedAt: "2026-02-16T10:30:00Z"}
	err := SaveProjectState(projectPath, state)
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(filepath.Join(projectPath, "state.yaml"))
	require.NoError(t, err)

	// Load state back
	loaded, err := LoadProjectState(projectPath)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-16T10:30:00Z", loaded.LastSyncedAt)
}

func TestSaveProjectState_OverwritesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0755))

	// Save initial state
	state1 := &ProjectState{LastSyncedAt: "2026-02-16T10:30:00Z"}
	require.NoError(t, SaveProjectState(projectPath, state1))

	// Save updated state
	state2 := &ProjectState{LastSyncedAt: "2026-02-16T11:00:00Z"}
	require.NoError(t, SaveProjectState(projectPath, state2))

	// Verify updated value
	loaded, err := LoadProjectState(projectPath)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-16T11:00:00Z", loaded.LastSyncedAt)
}

func TestSaveProjectState_CreatesDirectoryIfNeeded(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, "nested", "path", ".scion")

	state := &ProjectState{LastSyncedAt: "2026-02-16T10:30:00Z"}
	err := SaveProjectState(projectPath, state)
	require.NoError(t, err)

	loaded, err := LoadProjectState(projectPath)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-16T10:30:00Z", loaded.LastSyncedAt)
}

func TestLoadProjectState_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0755))

	// Write empty file
	require.NoError(t, os.WriteFile(filepath.Join(projectPath, "state.yaml"), []byte(""), 0644))

	state, err := LoadProjectState(projectPath)
	require.NoError(t, err)
	assert.Equal(t, "", state.LastSyncedAt)
}
