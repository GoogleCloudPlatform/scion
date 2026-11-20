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

func TestHarnessArgsIgnoredWarnings(t *testing.T) {
	t.Run("non-generic harness with args warns", func(t *testing.T) {
		hcs := map[string]HarnessConfigEntry{
			"my-claude": {
				Harness: "claude",
				Args:    []string{"--verbose"},
			},
		}
		got := HarnessArgsIgnoredWarnings(hcs)
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "harness_configs.my-claude.args")
		assert.Contains(t, got[0], `"claude"`)
		assert.Contains(t, got[0], "ignored")
	})

	t.Run("generic harness with args is silent", func(t *testing.T) {
		hcs := map[string]HarnessConfigEntry{
			"my-generic": {
				Harness: "generic",
				Args:    []string{"python3", "agent.py"},
			},
		}
		assert.Empty(t, HarnessArgsIgnoredWarnings(hcs))
	})

	t.Run("unset or empty args is silent", func(t *testing.T) {
		hcs := map[string]HarnessConfigEntry{
			"claude-nil":   {Harness: "claude"},
			"claude-empty": {Harness: "claude", Args: []string{}},
			"generic-nil":  {Harness: "generic"},
		}
		assert.Empty(t, HarnessArgsIgnoredWarnings(hcs))
	})

	t.Run("omitted harness field falls back to config name", func(t *testing.T) {
		hcs := map[string]HarnessConfigEntry{
			"generic": {Args: []string{"echo", "hi"}},
			"claude":  {Args: []string{"--flag"}},
		}
		got := HarnessArgsIgnoredWarnings(hcs)
		require.Len(t, got, 1)
		assert.Contains(t, got[0], "harness_configs.claude.args")
		assert.Contains(t, got[0], `"claude"`)
	})

	t.Run("multiple entries are sorted deterministically", func(t *testing.T) {
		hcs := map[string]HarnessConfigEntry{
			"z-codex": {Harness: "codex", Args: []string{"--a"}},
			"a-gem":   {Harness: "gemini-cli", Args: []string{"--b"}},
			"g-ok":    {Harness: "generic", Args: []string{"--c"}},
		}
		got := HarnessArgsIgnoredWarnings(hcs)
		require.Len(t, got, 2)
		assert.Contains(t, got[0], "harness_configs.a-gem.args")
		assert.Contains(t, got[1], "harness_configs.z-codex.args")
	})
}

func TestSettingsWarnings_HarnessArgs(t *testing.T) {
	t.Run("warns for non-generic harness with args", func(t *testing.T) {
		data := []byte(`schema_version: "1"
harness_configs:
  claude-custom:
    harness: claude
    args: ["--verbose"]
`)
		warnings := SettingsWarnings(data, "1")
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "harness_configs.claude-custom.args")
		assert.Contains(t, warnings[0], `"claude"`)
	})

	t.Run("silent for generic harness with args", func(t *testing.T) {
		data := []byte(`schema_version: "1"
harness_configs:
  custom-generic:
    harness: generic
    args: ["python3", "agent.py"]
`)
		assert.Empty(t, SettingsWarnings(data, "1"))
	})

	t.Run("silent when args is unset", func(t *testing.T) {
		data := []byte(`schema_version: "1"
harness_configs:
  claude-custom:
    harness: claude
    image: example.com/claude:latest
`)
		assert.Empty(t, SettingsWarnings(data, "1"))
	})
}

func TestLoadEffectiveSettings_HarnessArgsWarning(t *testing.T) {
	run := func(t *testing.T, yamlContent string) (*VersionedSettings, []string) {
		t.Helper()
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		projectDir := filepath.Join(tmpDir, "proj", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(yamlContent), 0o644))
		vs, warnings, err := LoadEffectiveSettings(projectDir)
		require.NoError(t, err)
		return vs, warnings
	}

	t.Run("non-generic harness with args warns and preserves args", func(t *testing.T) {
		vs, warnings := run(t, `schema_version: "1"
harness_configs:
  my-claude:
    harness: claude
    args: ["--verbose"]
`)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "harness_configs.my-claude.args")
		assert.Contains(t, warnings[0], `"claude"`)
		assert.Equal(t, []string{"--verbose"}, vs.HarnessConfigs["my-claude"].Args)
	})

	t.Run("generic harness with args is silent", func(t *testing.T) {
		vs, warnings := run(t, `schema_version: "1"
harness_configs:
  my-generic:
    harness: generic
    args: ["python3", "agent.py"]
`)
		assert.Empty(t, warnings)
		assert.Equal(t, []string{"python3", "agent.py"}, vs.HarnessConfigs["my-generic"].Args)
	})

	t.Run("non-generic harness without args is silent", func(t *testing.T) {
		_, warnings := run(t, `schema_version: "1"
harness_configs:
  my-claude:
    harness: claude
    image: example.com/claude:latest
`)
		assert.Empty(t, warnings)
	})
}
