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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubRefHome sets up an isolated HOME (and a working directory outside any
// project) whose global settings are settingsYAML, and returns the global dir.
func hubRefHome(t *testing.T, settingsYAML string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	t.Chdir(t.TempDir())
	return globalDir
}

const hubRefEndpointOnly = "schema_version: \"1\"\nhub:\n  endpoint: https://hub.example.com\n"

func loggedIn(v bool) func(*config.Settings, string) bool {
	return func(*config.Settings, string) bool { return v }
}

// TestEnsureHubModeForHubProjectRef covers ptone/scion#3533.
func TestEnsureHubModeForHubProjectRef(t *testing.T) {
	t.Run("logged in and interactive: prompt and enable", func(t *testing.T) {
		globalDir := hubRefHome(t, hubRefEndpointOnly)
		var out bytes.Buffer
		asked := 0
		err := ensureHubModeForHubProjectRef(&out, "my-proj", hubRefEnableOptions{
			Interactive: true,
			Confirm:     func(string) bool { asked++; return true },
			LoggedIn:    loggedIn(true),
		})
		require.NoError(t, err)
		assert.Equal(t, 1, asked)
		s, err := config.LoadSettingsFromDir(globalDir)
		require.NoError(t, err)
		assert.True(t, s.IsHubEnabled())
		assert.Contains(t, out.String(), "Hub mode enabled (global scope)")
	})

	t.Run("declined: hint names hub enable", func(t *testing.T) {
		globalDir := hubRefHome(t, hubRefEndpointOnly)
		var out bytes.Buffer
		err := ensureHubModeForHubProjectRef(&out, "my-proj", hubRefEnableOptions{
			Interactive: true,
			Confirm:     func(string) bool { return false },
			LoggedIn:    loggedIn(true),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scion hub enable")
		assert.NotContains(t, err.Error(), "config set")
		s, lerr := config.LoadSettingsFromDir(globalDir)
		require.NoError(t, lerr)
		assert.False(t, s.IsHubEnabled())
	})

	t.Run("non-interactive: never enables silently", func(t *testing.T) {
		globalDir := hubRefHome(t, hubRefEndpointOnly)
		err := ensureHubModeForHubProjectRef(&bytes.Buffer{}, "my-proj", hubRefEnableOptions{
			Interactive: false,
			Confirm:     func(string) bool { t.Fatal("must not prompt"); return true },
			LoggedIn:    loggedIn(true),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Enable with: scion hub enable")
		s, lerr := config.LoadSettingsFromDir(globalDir)
		require.NoError(t, lerr)
		assert.False(t, s.IsHubEnabled())
	})

	t.Run("not logged in: no prompt", func(t *testing.T) {
		hubRefHome(t, hubRefEndpointOnly)
		err := ensureHubModeForHubProjectRef(&bytes.Buffer{}, "my-proj", hubRefEnableOptions{
			Interactive: true,
			Confirm:     func(string) bool { t.Fatal("must not prompt"); return true },
			LoggedIn:    loggedIn(false),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scion hub enable")
	})

	t.Run("no endpoint: hint names login then hub enable", func(t *testing.T) {
		hubRefHome(t, "schema_version: \"1\"\n")
		err := ensureHubModeForHubProjectRef(&bytes.Buffer{}, "my-proj", hubRefEnableOptions{
			Interactive: true,
			Confirm:     func(string) bool { t.Fatal("must not prompt"); return true },
			LoggedIn:    loggedIn(true),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scion hub auth login")
		assert.Contains(t, err.Error(), "scion hub enable")
	})

	t.Run("hub already enabled: no-op", func(t *testing.T) {
		hubRefHome(t, "schema_version: \"1\"\nhub:\n  enabled: true\n  endpoint: https://hub.example.com\n")
		err := ensureHubModeForHubProjectRef(&bytes.Buffer{}, "my-proj", hubRefEnableOptions{
			Interactive: true,
			Confirm:     func(string) bool { t.Fatal("must not prompt"); return true },
			LoggedIn:    loggedIn(true),
		})
		require.NoError(t, err)
	})
}
