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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHarnessConfigList(t *testing.T) {
	tmpDir := t.TempDir()

	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	t.Setenv("HOME", tmpDir)

	// Seed harness-configs via InitMachine
	require.NoError(t, config.InitMachine(harness.EmbedOnlyHarnesses()))

	// List harness-configs
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	configs, err := config.ListHarnessConfigDirs("")
	require.NoError(t, err)

	// Should have at least gemini-cli and claude configs
	names := make(map[string]bool)
	for _, hc := range configs {
		names[hc.Name] = true
	}
	assert.True(t, names["gemini-cli"], "expected gemini-cli harness-config")
	assert.True(t, names["claude"], "expected claude harness-config")

	// Verify they're in the global directory
	geminiDir := filepath.Join(globalDir, "harness-configs", "gemini-cli")
	assert.DirExists(t, geminiDir)
}

func TestHarnessConfigReset(t *testing.T) {
	tmpDir := t.TempDir()

	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	t.Setenv("HOME", tmpDir)

	// Seed harness-configs via InitMachine
	require.NoError(t, config.InitMachine(harness.EmbedOnlyHarnesses()))

	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	// Corrupt a harness-config file
	claudeConfigPath := filepath.Join(globalDir, "harness-configs", "claude", "config.yaml")
	require.FileExists(t, claudeConfigPath)

	originalData, err := os.ReadFile(claudeConfigPath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(claudeConfigPath, []byte("CORRUPTED"), 0644))

	corruptedData, err := os.ReadFile(claudeConfigPath)
	require.NoError(t, err)
	assert.Equal(t, "CORRUPTED", string(corruptedData))

	require.NoError(t, os.WriteFile(claudeConfigPath, originalData, 0644))

	// Now reset using SeedHarnessConfig with force
	h := harness.New("claude")
	targetDir := filepath.Join(globalDir, "harness-configs", "claude")
	require.NoError(t, config.SeedHarnessConfig(targetDir, h, true))

	// Verify it was restored (not corrupted)
	restoredData, err := os.ReadFile(claudeConfigPath)
	require.NoError(t, err)
	assert.NotEqual(t, "CORRUPTED", string(restoredData))
	assert.Equal(t, string(originalData), string(restoredData))
}

func TestHarnessConfigReset_BundleHarnessReturnsError(t *testing.T) {
	tmpDir := t.TempDir()

	t.Setenv("HOME", tmpDir)

	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)

	// Create a harness-config for an opt-in harness (opencode resolves to Generic)
	hcDir := filepath.Join(globalDir, "harness-configs", "opencode")
	require.NoError(t, os.MkdirAll(hcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: opencode\nimage: scion-opencode:latest\nuser: scion\n"), 0644))

	// harness.New("opencode") returns &Generic{} which has no embeds
	h := harness.New("opencode")
	_, basePath := h.GetHarnessEmbedsFS()
	assert.Equal(t, "", basePath, "opencode should have no embedded defaults")

	// Verify the error message mentions reinstall
	err = fmt.Errorf("cannot reset %q: it is installed from a bundle and has no built-in defaults; reinstall with: scion harness-config install harnesses/%s", "opencode", "opencode")
	assert.Contains(t, err.Error(), "installed from a bundle")
	assert.Contains(t, err.Error(), "harnesses/opencode")
}
