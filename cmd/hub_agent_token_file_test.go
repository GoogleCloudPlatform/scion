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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearAgentAuthEnv isolates getAuthInfo from the environment and points
// HOME at a fresh directory containing an empty .scion directory.
func clearAgentAuthEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN", "")
	t.Setenv("SCION_DEV_TOKEN_FILE", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("SCION_AGENT_ID", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o700))
	return home
}

// TestReadAgentTokenFile_OversizedRefusedWithWarning: the CLI refuses an
// oversized agent token file, rather than sending a truncated value, says
// so on stderr, and falls through to the next credential source.
func TestReadAgentTokenFile_OversizedRefusedWithWarning(t *testing.T) {
	home := clearAgentAuthEnv(t)
	t.Setenv("SCION_AUTH_TOKEN", "env-token")
	big := strings.Repeat("a", 8193)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte(big), 0o600))

	var token string
	var info authInfo
	stderr := captureStderr(t, func() {
		token = readAgentTokenFile()
		info = getAuthInfo(&config.Settings{}, "https://hub.example.com")
	})
	assert.Equal(t, "", token, "an oversized token file must be refused")
	assert.Contains(t, stderr, "Warning: ignoring agent token file")
	assert.Contains(t, stderr, "size limit")
	assert.Equal(t, "SCION_AUTH_TOKEN env", info.Source, "a refused file must fall through to the next source")
}

// TestReadAgentTokenFile_RefusesLinkAndFIFO: a symbolic link or a FIFO at
// the token path reads as no token, and the FIFO does not block the CLI.
func TestReadAgentTokenFile_RefusesLinkAndFIFO(t *testing.T) {
	t.Run("symbolic link", func(t *testing.T) {
		home := clearAgentAuthEnv(t)
		other := filepath.Join(t.TempDir(), "other")
		require.NoError(t, os.WriteFile(other, []byte("other-content"), 0o600))
		require.NoError(t, os.Symlink(other, filepath.Join(home, ".scion", "scion-token")))

		var token string
		stderr := captureStderr(t, func() { token = readAgentTokenFile() })
		assert.Equal(t, "", token)
		assert.Contains(t, stderr, "Warning: ignoring agent token file")
	})

	t.Run("FIFO", func(t *testing.T) {
		home := clearAgentAuthEnv(t)
		if err := mkfifoForTest(filepath.Join(home, ".scion", "scion-token"), 0o600); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
		var token string
		blocked := false
		stderr := captureStderr(t, func() {
			done := make(chan string, 1)
			go func() { done <- readAgentTokenFile() }()
			select {
			case token = <-done:
			case <-time.After(5 * time.Second):
				blocked = true
			}
		})
		require.False(t, blocked, "readAgentTokenFile blocked on a FIFO")
		assert.Equal(t, "", token)
		assert.Contains(t, stderr, "Warning: ignoring agent token file")
	})
}

// TestReadAgentTokenFile_LinkedScionDir: with ~/.scion a link, a missing
// token file stays silent, and a present one is refused with a warning.
func TestReadAgentTokenFile_LinkedScionDir(t *testing.T) {
	t.Run("no token file is silent", func(t *testing.T) {
		home := clearAgentAuthEnv(t)
		require.NoError(t, os.Remove(filepath.Join(home, ".scion")))
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(home, ".scion")))

		var token string
		stderr := captureStderr(t, func() { token = readAgentTokenFile() })
		assert.Equal(t, "", token)
		assert.Equal(t, "", stderr)
	})

	t.Run("token file is refused with a warning", func(t *testing.T) {
		home := clearAgentAuthEnv(t)
		require.NoError(t, os.Remove(filepath.Join(home, ".scion")))
		other := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(other, "scion-token"), []byte("other-token"), 0o600))
		require.NoError(t, os.Symlink(other, filepath.Join(home, ".scion")))

		var token string
		stderr := captureStderr(t, func() { token = readAgentTokenFile() })
		assert.Equal(t, "", token)
		assert.Contains(t, stderr, "Warning: ignoring agent token file")
	})
}

// TestReadAgentTokenFile_ScionIsRegularFile: a regular file at ~/.scion
// means no token file can exist, so the CLI stays silent.
func TestReadAgentTokenFile_ScionIsRegularFile(t *testing.T) {
	home := clearAgentAuthEnv(t)
	require.NoError(t, os.Remove(filepath.Join(home, ".scion")))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion"), []byte("not a directory"), 0o600))

	var token string
	stderr := captureStderr(t, func() { token = readAgentTokenFile() })
	assert.Equal(t, "", token)
	assert.Equal(t, "", stderr)
}

// TestReadAgentTokenFile_LinkedHomePrefix: a home directory reached
// through a linked prefix (e.g. /home -> /var/home) still works.
func TestReadAgentTokenFile_LinkedHomePrefix(t *testing.T) {
	clearAgentAuthEnv(t)
	root := t.TempDir()
	realHome := filepath.Join(root, "var", "home", "user")
	require.NoError(t, os.MkdirAll(filepath.Join(realHome, ".scion"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(realHome, ".scion", "scion-token"), []byte("linked-home-token\n"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(root, "var", "home"), filepath.Join(root, "home")))
	t.Setenv("HOME", filepath.Join(root, "home", "user"))

	assert.Equal(t, "linked-home-token", readAgentTokenFile())
}
