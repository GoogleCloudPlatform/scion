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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keysTestEnv isolates a keys command run: a fresh HOME (so no real
// settings or database), no SCION_SERVER_* variables, no legacy session
// secret and the keys flag globals restored afterwards.
func keysTestEnv(t *testing.T) string {
	t.Helper()
	isolateServerSecretEnv(t)
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SCION_SERVER_") {
			t.Setenv(k, v) // restored at cleanup
			require.NoError(t, os.Unsetenv(k))
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	oldCfg, oldFile, oldWR := keysConfigPath, keysKeyringFile, keysAllowWorldReadable
	oldDB, oldNoStore, oldFormat := keysStatusDB, keysStatusNoStore, outputFormat
	t.Cleanup(func() {
		keysConfigPath, keysKeyringFile, keysAllowWorldReadable = oldCfg, oldFile, oldWR
		keysStatusDB, keysStatusNoStore, outputFormat = oldDB, oldNoStore, oldFormat
	})
	return home
}

// keysTestTree is a scion > server > keys tree with the real keys flags
// and run functions, so a test runs them without the root pre-run hooks.
func keysTestTree() *cobra.Command {
	root := &cobra.Command{Use: "scion", SilenceUsage: true, SilenceErrors: true}
	server := &cobra.Command{Use: "server"}
	keys := &cobra.Command{Use: "keys"}
	addKeysPersistentFlags(keys.PersistentFlags())
	check := &cobra.Command{Use: "check", Args: cobra.NoArgs, RunE: runServerKeysCheck}
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: runServerKeysStatus}
	addKeysStatusFlags(status.Flags())
	keys.AddCommand(check, status)
	server.AddCommand(keys)
	root.AddCommand(server)
	return root
}

// runKeys runs `scion server keys <args>` and returns stdout, the error and
// the exit status Execute would use.
func runKeys(t *testing.T, args ...string) (string, error, int) {
	t.Helper()
	root := keysTestTree()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"server", "keys"}, args...))
	err := root.Execute()
	return out.String(), err, exitStatusOf(err)
}

func exitStatusOf(err error) int {
	if err == nil {
		return 0
	}
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 1
}

// The keys group is human-mode only and Windows-hidden: no path under it
// is in agentAllowed or windowsAllowed, and agent mode removes it.
func TestServerKeys_CLIModeCensus(t *testing.T) {
	names := map[string]bool{}
	for _, c := range serverKeysCmd.Commands() {
		names[c.Name()] = true
	}
	assert.True(t, names["status"], "server keys status is registered")
	assert.True(t, names["check"], "server keys check is registered")
	assert.Equal(t, serverCmd, serverKeysCmd.Parent())

	paths := []string{"server", "server.keys"}
	for name := range names {
		paths = append(paths, "server.keys."+name)
	}
	for _, p := range paths {
		assert.False(t, agentAllowed[p], "agentAllowed must not list %s", p)
		assert.False(t, windowsAllowed[p], "windowsAllowed must not list %s", p)
	}
	// The top-level "keys" (agent keystrokes) is a different command.
	assert.True(t, agentAllowed["keys"])

	root := keysTestTree()
	applyAgentMode(root)
	assert.Empty(t, root.Commands(), "agent mode removes server keys")
}

// check: exit 0 for a valid file or legacy mode, exit 2 for every invalid
// configuration, and never prints key material.
func TestServerKeysCheck_ExitCodes(t *testing.T) {
	t.Run("legacy mode", func(t *testing.T) {
		keysTestEnv(t)
		out, err, code := runKeys(t, "check")
		require.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Contains(t, out, "legacy single-secret mode")
	})
	t.Run("valid file", func(t *testing.T) {
		keysTestEnv(t)
		out, err, code := runKeys(t, "check", "--keyring-file", writeCmdKeyring(t, nil))
		require.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Contains(t, out, `revision "cmd-test-rev"`)
		assertNoCmdKRSecret(t, out)
	})
	t.Run("valid file from the environment", func(t *testing.T) {
		keysTestEnv(t)
		t.Setenv(keyringFileEnv, writeCmdKeyring(t, nil))
		out, err, code := runKeys(t, "check")
		require.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Contains(t, out, "Keyring file is valid")
	})

	invalid := map[string]func(t *testing.T) []string{
		"missing file": func(t *testing.T) []string {
			return []string{"--keyring-file", filepath.Join(t.TempDir(), "absent.json")}
		},
		"malformed JSON": func(t *testing.T) []string {
			p := filepath.Join(t.TempDir(), "keyring.json")
			require.NoError(t, os.WriteFile(p, []byte(`{"schema": 1,`), 0o400))
			return []string{"--keyring-file", p}
		},
		"world writable": func(t *testing.T) []string {
			p := writeCmdKeyring(t, nil)
			require.NoError(t, os.Chmod(p, 0o602))
			return []string{"--keyring-file", p}
		},
		"emit versioned": func(t *testing.T) []string {
			return []string{"--keyring-file", writeCmdKeyring(t, func(doc map[string]any) {
				doc["signing"].(map[string]any)["emit"] = "versioned"
			})}
		},
		"active not a legacy reader": func(t *testing.T) []string {
			return []string{"--keyring-file", writeCmdKeyring(t, func(doc map[string]any) {
				doc["signing"].(map[string]any)["legacy_read_ids"] = []any{"legacy"}
			})}
		},
		"whitespace path": func(t *testing.T) []string {
			return []string{"--keyring-file", " " + writeCmdKeyring(t, nil)}
		},
		"legacy session secret alongside": func(t *testing.T) []string {
			t.Setenv(sessionSecretEnv, "test-only-env-secret")
			return []string{"--keyring-file", writeCmdKeyring(t, nil)}
		},
	}
	for name, args := range invalid {
		t.Run(name, func(t *testing.T) {
			keysTestEnv(t)
			out, err, code := runKeys(t, append([]string{"check"}, args(t)...)...)
			require.Error(t, err)
			assert.Equal(t, keysExitInvalid, code, err.Error())
			assertNoCmdKRSecret(t, out+err.Error())
			assert.NotContains(t, err.Error(), "test-only-env-secret")
		})
	}
}

// Review N5: the text output renders deadlines in the display zone
// (--tz/--utc), like every other CLI time.
func TestPrintKeyStatus_DeadlinesUseDisplayZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	old := clitime.Zone()
	clitime.SetZone(ny)
	t.Cleanup(func() { clitime.SetZone(old) })
	signUntil := time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)
	verifyUntil := signUntil.Add(48 * time.Hour)
	var out bytes.Buffer
	printKeyStatus(&out, hub.KeyStatusReport{Signing: hub.KeyStatusSigning{
		Generations: []hub.KeyStatusGeneration{{ID: "legacy", SignUntil: &signUntil, VerifyUntil: &verifyUntil}, {ID: "gen1"}},
	}})
	assert.Contains(t, out.String(), "legacy: sign_until 2026-10-30 10:00:00 EDT, verify_until 2026-11-01 09:00:00 EST")
	assert.Contains(t, out.String(), "gen1: sign_until none, verify_until none")
}
