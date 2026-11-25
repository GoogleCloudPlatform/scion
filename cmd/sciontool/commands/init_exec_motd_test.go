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

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteExecContainerMOTD_ShellOnly(t *testing.T) {
	for _, harness := range []string{"", "claude", "gemini", "generic"} {
		t.Run("skipped_for_"+harness, func(t *testing.T) {
			dir := t.TempDir()
			motdPath := filepath.Join(dir, "motd")
			profileDir := filepath.Join(dir, "profile.d")
			require.NoError(t, os.MkdirAll(profileDir, 0755))
			scriptPath := filepath.Join(profileDir, "scion-exec-container.sh")

			env := map[string]string{
				"SCION_HARNESS":         harness,
				"SCION_AGENT_NAME":      "worker-1",
				"SCION_HOME_PERSISTENT": "false",
			}
			err := writeExecContainerMOTDAt(func(k string) string { return env[k] }, motdPath, scriptPath)
			require.NoError(t, err)

			_, statErr := os.Stat(motdPath)
			assert.True(t, os.IsNotExist(statErr), "MOTD must not be written for harness %q", harness)
			_, scriptErr := os.Stat(scriptPath)
			assert.True(t, os.IsNotExist(scriptErr), "profile script must not be written for harness %q", harness)
		})
	}
}

func TestWriteExecContainerMOTD_PersistentAndNonPersistentVariants(t *testing.T) {
	t.Run("persistent_true", func(t *testing.T) {
		dir := t.TempDir()
		motdPath := filepath.Join(dir, "motd")
		profileDir := filepath.Join(dir, "profile.d")
		require.NoError(t, os.MkdirAll(profileDir, 0755))
		scriptPath := filepath.Join(profileDir, "scion-exec-container.sh")

		env := map[string]string{
			"SCION_HARNESS":         "shell",
			"SCION_AGENT_NAME":      "ops-box",
			"SCION_HOME_PERSISTENT": "true",
		}
		require.NoError(t, writeExecContainerMOTDAt(func(k string) string { return env[k] }, motdPath, scriptPath))

		data, err := os.ReadFile(motdPath)
		require.NoError(t, err)
		got := string(data)

		assert.Contains(t, got, "This is your execution container")
		assert.Contains(t, got, "Log in to the CLIs")
		assert.Contains(t, got, "$HOME")
		assert.Contains(t, got, "never in /workspace")
		assert.Contains(t, got, "scion exec-container set ops-box")
		assert.Contains(t, got, "Home persistence: enabled")
		assert.NotContains(t, got, "WARNING: $HOME is NOT persistent")
		assert.LessOrEqual(t, strings.Count(got, "\n"), 20, "MOTD must fit on one screen")

		scriptData, err := os.ReadFile(scriptPath)
		require.NoError(t, err)
		assert.Contains(t, string(scriptData), "cat /etc/motd")
		assert.Contains(t, string(scriptData), "/tmp/.scion-exec-motd-shown")
	})

	t.Run("persistent_false_warns", func(t *testing.T) {
		dir := t.TempDir()
		motdPath := filepath.Join(dir, "motd")
		profileDir := filepath.Join(dir, "profile.d")
		require.NoError(t, os.MkdirAll(profileDir, 0755))
		scriptPath := filepath.Join(profileDir, "scion-exec-container.sh")

		env := map[string]string{
			"SCION_HARNESS":         "shell",
			"SCION_AGENT_SLUG":      "ephemeral-ops",
			"SCION_HOME_PERSISTENT": "false",
		}
		require.NoError(t, writeExecContainerMOTDAt(func(k string) string { return env[k] }, motdPath, scriptPath))

		data, err := os.ReadFile(motdPath)
		require.NoError(t, err)
		got := string(data)

		assert.Contains(t, got, "This is your execution container")
		assert.Contains(t, got, "never in /workspace")
		assert.Contains(t, got, "scion exec-container set ephemeral-ops")
		assert.Contains(t, got, "WARNING: $HOME is NOT persistent")
		assert.Contains(t, got, "lost when this container stops or restarts")
		assert.LessOrEqual(t, strings.Count(got, "\n"), 20, "MOTD must fit on one screen")
	})

	t.Run("sanitizes_control_chars_and_shell_quotes_agent_name", func(t *testing.T) {
		env := map[string]string{
			"SCION_HARNESS":         "shell",
			"SCION_AGENT_NAME":      "ops\x1b[31m\n;rm -rf /",
			"SCION_HOME_PERSISTENT": "true",
		}
		got := buildExecContainerMOTD(func(k string) string { return env[k] })
		assert.NotContains(t, got, "\x1b")
		assert.NotContains(t, got, "ops\n")
		assert.Contains(t, got, "scion exec-container set 'ops[31m;rm -rf /'")
	})
}
