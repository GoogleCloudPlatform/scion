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

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteProjectPreStartHook_CreatesFile(t *testing.T) {
	agentHome := t.TempDir()
	script := "#!/bin/sh\necho hello\n"

	err := WriteProjectPreStartHook(agentHome, script)
	require.NoError(t, err)

	target := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", ProjectPreStartHookFilename)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, script, string(data))

	// File must be executable.
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, info.Mode()&0111 != 0, "file should be executable")
}

func TestWriteProjectPreStartHook_Idempotent(t *testing.T) {
	agentHome := t.TempDir()

	require.NoError(t, WriteProjectPreStartHook(agentHome, "#!/bin/sh\necho v1\n"))
	require.NoError(t, WriteProjectPreStartHook(agentHome, "#!/bin/sh\necho v2\n"))

	target := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", ProjectPreStartHookFilename)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\necho v2\n", string(data), "second write should overwrite first")
}

func TestWriteProjectPreStartHook_EmptyScript_NoOp(t *testing.T) {
	agentHome := t.TempDir()

	err := WriteProjectPreStartHook(agentHome, "")
	require.NoError(t, err)

	target := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", ProjectPreStartHookFilename)
	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "file should not be created for empty script")
}

func TestWriteProjectPreStartHook_CreatesDirectory(t *testing.T) {
	agentHome := t.TempDir()

	// pre-start.d does not exist yet — WriteProjectPreStartHook must create it.
	err := WriteProjectPreStartHook(agentHome, "#!/bin/sh\n")
	require.NoError(t, err)

	dir := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d")
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestWriteProjectPreStartHook_Filename(t *testing.T) {
	assert.Equal(t, "30-project-custom", ProjectPreStartHookFilename,
		"filename must be exactly '30-project-custom' to run after 20-harness-provision")
}
