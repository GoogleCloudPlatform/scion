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
)

// createTestState captures and restores package-level vars for test isolation.
type createTestState struct {
	home        string
	projectPath string
	noHub       bool
}

func saveCreateTestState() createTestState {
	return createTestState{
		home:        os.Getenv("HOME"),
		projectPath: projectPath,
		noHub:       noHub,
	}
}

func (s createTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	noHub = s.noHub
}

func TestCreateCmd_HarnessFlagDefaultsToEmpty(t *testing.T) {
	// Regression test: the --harness flag on createCmd must default to ""
	// (not "h"). Previously StringVar was called with "h" as the default
	// value instead of the empty string, causing harnessConfigFlag to be
	// non-empty even when neither --harness nor --harness-config was passed.
	f := createCmd.Flags().Lookup("harness")
	require.NotNil(t, f, "--harness flag must be registered on createCmd")
	assert.Equal(t, "", f.DefValue, "--harness default value must be empty string")
}

func TestCreateAgent_DuplicateReturnsError(t *testing.T) {
	orig := saveCreateTestState()
	defer orig.restore()

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	noHub = true

	// Set up project directory with an existing agent
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0755))
	projectPath = projectDir

	createAgentDir(t, projectDir, "my-agent")

	// Attempt to create an agent with the same name — should fail
	err := createCmd.RunE(createCmd, []string{"my-agent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}
