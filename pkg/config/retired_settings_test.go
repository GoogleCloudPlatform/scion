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

func TestRetiredServerSettingWarnings_AsyncAgentLaunch(t *testing.T) {
	want := []string{RetiredAsyncAgentLaunchWarning}

	// setup isolates HOME, clears the env var, and writes name with content
	// into a fresh config dir. It returns the config path to load.
	setup := func(t *testing.T, name, content string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(retiredAsyncAgentLaunchEnvVar, "")
		dir := t.TempDir()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		return path
	}

	t.Run("settings.yaml server block without the key", func(t *testing.T) {
		path := setup(t, "settings.yaml", "schema_version: \"1\"\nserver:\n  hub:\n    port: 9999\n")
		assert.Empty(t, RetiredServerSettingWarnings(path))
	})

	t.Run("settings.yaml server block with the key set true", func(t *testing.T) {
		path := setup(t, "settings.yaml", "schema_version: \"1\"\nserver:\n  hub:\n    async_agent_launch: true\n")
		assert.Equal(t, want, RetiredServerSettingWarnings(path))
	})

	t.Run("settings.yaml server block with the key set false", func(t *testing.T) {
		path := setup(t, "settings.yaml", "schema_version: \"1\"\nserver:\n  hub:\n    async_agent_launch: false\n")
		assert.Equal(t, want, RetiredServerSettingWarnings(path))
	})

	t.Run("the global settings.yaml server block with the key", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(retiredAsyncAgentLaunchEnvVar, "")
		globalDir := filepath.Join(home, GlobalDir)
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nserver:\n  hub:\n    async_agent_launch: true\n"), 0o644))
		assert.Equal(t, want, RetiredServerSettingWarnings(""))
	})

	t.Run("legacy server.yaml with the key", func(t *testing.T) {
		path := setup(t, "server.yaml", "hub:\n  asyncAgentLaunch: true\n")
		assert.Equal(t, want, RetiredServerSettingWarnings(path))
	})

	t.Run("legacy server.yaml without the key", func(t *testing.T) {
		path := setup(t, "server.yaml", "hub:\n  port: 9999\n")
		assert.Empty(t, RetiredServerSettingWarnings(path))
	})

	t.Run("environment variable", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(retiredAsyncAgentLaunchEnvVar, "true")
		assert.Equal(t, want, RetiredServerSettingWarnings(""))
	})

	t.Run("nothing set", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv(retiredAsyncAgentLaunchEnvVar, "")
		assert.Empty(t, RetiredServerSettingWarnings(""))
	})
}
