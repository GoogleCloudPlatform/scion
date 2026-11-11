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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const configSetPolicySecret = "s3cr3t-value-must-not-leak"

// readDirFiles maps each file name in dir to its content.
func readDirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()+"/"] = ""
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out[e.Name()] = string(data)
	}
	return out
}

// runConfigSet runs "config set key value" through the command's RunE at
// the given layer and returns its stdout, stderr and error.
func runConfigSet(t *testing.T, global bool, key, value string) (stdout, stderr string, err error) {
	t.Helper()
	origGlobal := configGlobal
	t.Cleanup(func() { configGlobal = origGlobal })
	configGlobal = global
	stdout, stderr = captureStdoutStderr(t, func() {
		err = configSetCmd.RunE(configSetCmd, []string{key, value})
	})
	return stdout, stderr, err
}

// TestConfigSet_RejectsNotWritableKeys checks "config set" for every key
// whose write would be accepted and then never read at that layer: the
// command fails, the error names the key, the value appears in neither the
// error nor the output, and the settings files are unchanged.
func TestConfigSet_RejectsNotWritableKeys(t *testing.T) {
	cases := []struct {
		key    string
		global bool
	}{
		{"hub.token", true},
		{"hub.token", false},
		{"hub.apiKey", true},
		{"hub.apiKey", false},
		{"hub.lastSyncedAt", false},
		{"bucket.provider", false},
		{"bucket.name", true},
		{"bucket.prefix", false},
		{"hub_connections.prod.endpoint", true},
		{"hub_connections.prod.endpoint", false},
		{"server.auth.display_name", false},
		{"server.auth.email", false},
		{"server.auth.username", false},
	}
	for _, tc := range cases {
		name := "project/" + tc.key
		if tc.global {
			name = "global/" + tc.key
		}
		t.Run(name, func(t *testing.T) {
			root := setupProjectSettings(t, &config.VersionedSettings{SchemaVersion: "1", ActiveProfile: "local"})
			origProjectPath := projectPath
			t.Cleanup(func() { projectPath = origProjectPath })
			projectPath = root
			home, err := os.UserHomeDir()
			require.NoError(t, err)
			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			require.NoError(t, config.SaveVersionedSettings(globalDir, &config.VersionedSettings{SchemaVersion: "1"}))
			projectDir := filepath.Join(root, ".scion")
			beforeProject, beforeGlobal := readDirFiles(t, projectDir), readDirFiles(t, globalDir)

			stdout, stderr, runErr := runConfigSet(t, tc.global, tc.key, configSetPolicySecret)
			require.Error(t, runErr)
			assert.True(t, errors.Is(runErr, config.ErrSettingNotWritable), "error: %v", runErr)
			assert.Contains(t, runErr.Error(), tc.key)
			assert.NotContains(t, runErr.Error(), configSetPolicySecret)
			assert.NotContains(t, stdout, configSetPolicySecret)
			assert.NotContains(t, stderr, configSetPolicySecret)
			assert.Equal(t, beforeProject, readDirFiles(t, projectDir), "project settings changed")
			assert.Equal(t, beforeGlobal, readDirFiles(t, globalDir), "global settings changed")
		})
	}
}

// TestConfigSet_ServerAuthGlobalStillWritable checks that the project-layer
// rule does not block the global write.
func TestConfigSet_ServerAuthGlobalStillWritable(t *testing.T) {
	root := setupProjectSettings(t, &config.VersionedSettings{SchemaVersion: "1"})
	origProjectPath := projectPath
	t.Cleanup(func() { projectPath = origProjectPath })
	projectPath = root

	stdout, _, runErr := runConfigSet(t, true, "server.auth.display_name", "Dev User")
	require.NoError(t, runErr)
	assert.Contains(t, stdout, "Dev User")

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	vs, err := config.LoadSingleFileVersioned(filepath.Join(home, ".scion"))
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Auth)
	assert.Equal(t, "Dev User", vs.Server.Auth.DisplayName)
}

// TestConfigSet_UnknownServerAuthKeyNoGlobalHint checks the flow from
// ptone/scion#5481: a server.auth key that config set cannot write (for
// example dev_mode) fails as an unknown key at the project layer, with no
// hint to use --global, and fails the same way with --global. No settings
// file changes.
func TestConfigSet_UnknownServerAuthKeyNoGlobalHint(t *testing.T) {
	for _, key := range []string{"server.auth.dev_mode", "server.auth.authorized_domains"} {
		for _, global := range []bool{false, true} {
			name := "project/" + key
			if global {
				name = "global/" + key
			}
			t.Run(name, func(t *testing.T) {
				root := setupProjectSettings(t, &config.VersionedSettings{SchemaVersion: "1", ActiveProfile: "local"})
				origProjectPath := projectPath
				t.Cleanup(func() { projectPath = origProjectPath })
				projectPath = root
				home, err := os.UserHomeDir()
				require.NoError(t, err)
				globalDir := filepath.Join(home, ".scion")
				require.NoError(t, os.MkdirAll(globalDir, 0o755))
				require.NoError(t, config.SaveVersionedSettings(globalDir, &config.VersionedSettings{SchemaVersion: "1"}))
				projectDir := filepath.Join(root, ".scion")
				beforeProject, beforeGlobal := readDirFiles(t, projectDir), readDirFiles(t, globalDir)

				_, _, runErr := runConfigSet(t, global, key, "true")
				require.Error(t, runErr)
				assert.False(t, errors.Is(runErr, config.ErrSettingNotWritable), "error: %v", runErr)
				assert.Contains(t, runErr.Error(), "unknown or complex setting key: "+key)
				assert.NotContains(t, runErr.Error(), "--global")
				assert.Equal(t, beforeProject, readDirFiles(t, projectDir), "project settings changed")
				assert.Equal(t, beforeGlobal, readDirFiles(t, globalDir), "global settings changed")
			})
		}
	}
}

// TestConfigSet_MasksSecretValueInOutput checks that a credential key's
// value is written but shown masked, in text and JSON output, while a
// non-credential value is shown as is.
func TestConfigSet_MasksSecretValueInOutput(t *testing.T) {
	for _, format := range []string{"", "json"} {
		t.Run("format="+format, func(t *testing.T) {
			root := setupProjectSettings(t, &config.VersionedSettings{SchemaVersion: "1"})
			origProjectPath, origFormat := projectPath, outputFormat
			t.Cleanup(func() { projectPath, outputFormat = origProjectPath, origFormat })
			projectPath = root
			outputFormat = format

			stdout, stderr, runErr := runConfigSet(t, true, "hub.brokerToken", configSetPolicySecret)
			require.NoError(t, runErr)
			assert.NotContains(t, stdout, configSetPolicySecret)
			assert.NotContains(t, stderr, configSetPolicySecret)
			assert.Contains(t, stdout, "********")

			home, err := os.UserHomeDir()
			require.NoError(t, err)
			vs, err := config.LoadSingleFileVersioned(filepath.Join(home, ".scion"))
			require.NoError(t, err)
			require.NotNil(t, vs.Server)
			require.NotNil(t, vs.Server.Broker)
			assert.Equal(t, configSetPolicySecret, vs.Server.Broker.BrokerToken, "the value itself is written")

			stdout, _, runErr = runConfigSet(t, true, "hub.brokerId", "broker-123")
			require.NoError(t, runErr)
			assert.Contains(t, stdout, "broker-123")
		})
	}
}

func TestConfigSetDisplayValue(t *testing.T) {
	assert.Equal(t, "********", configSetDisplayValue("hub.brokerToken", "x"))
	assert.Equal(t, "", configSetDisplayValue("hub.brokerToken", ""))
	assert.Equal(t, "v", configSetDisplayValue("image_registry", "v"))
}

// TestConfigSet_ServerAuthRootGlobalForms checks that every form that
// writes the global settings file (the subcommand --global, the root
// --global and -g global, which resolve the project to the global
// directory) counts as the global layer for server.auth.*.
func TestConfigSet_ServerAuthRootGlobalForms(t *testing.T) {
	for name, args := range map[string][]string{
		"subcommand flag": {"config", "set", "--global", "server.auth.email", "dev@example.com"},
		"root placement":  {"--global", "config", "set", "server.auth.email", "dev@example.com"},
		"-g global":       {"-g", "global", "config", "set", "server.auth.email", "dev@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreAllSilenceUsage(t)
			resetConfigSetFlags(t)

			rootCmd.SetArgs(args)
			require.NoError(t, rootCmd.Execute())

			vs, err := config.LoadSingleFileVersioned(filepath.Join(os.Getenv("HOME"), ".scion"))
			require.NoError(t, err)
			require.NotNil(t, vs.Server)
			require.NotNil(t, vs.Server.Auth)
			assert.Equal(t, "dev@example.com", vs.Server.Auth.Email)
		})
	}
}
