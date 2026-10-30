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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyringFileEnvEntry(t *testing.T) ServerEnvEntry {
	t.Helper()
	for _, e := range ServerEnvEntries() {
		if e.Path == KeyringFileKey {
			return e
		}
	}
	t.Fatalf("no env table entry for %s", KeyringFileKey)
	return ServerEnvEntry{}
}

// TestKeyringFileSetting_Env: the design's name SCION_SERVER_KEYRING_FILE
// is canonical, and every spelling reaches GlobalConfig.KeyringFile and
// VersionedSettings (ptone/scion#1530 §2). Every spelling is reserved for
// the server, so no host value of it reaches an agent.
func TestKeyringFileSetting_Env(t *testing.T) {
	entry := keyringFileEnvEntry(t)
	require.Equal(t, "SCION_SERVER_KEYRING_FILE", entry.Canonical)
	const value = "/etc/scion/keyring/keyring.json"
	for _, env := range append([]string{entry.Canonical}, entry.Aliases...) {
		t.Run(env, func(t *testing.T) {
			assert.True(t, util.IsAgentRefusedHostEnvName(env), "agent-refused")
			assert.Equal(t, KeyringFileKey, versionedEnvKeyMapper(env))
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(env, value)
			configPath := filepath.Join(home, "settings.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\n"), 0o644))
			gc, err := LoadGlobalConfig(configPath)
			require.NoError(t, err)
			assert.Equal(t, value, gc.KeyringFile, "GlobalConfig")
			vs, err := LoadVersionedSettings("")
			require.NoError(t, err)
			require.NotNil(t, vs.Server)
			assert.Equal(t, value, vs.Server.KeyringFile, "VersionedSettings")
		})
	}
}

// TestKeyringFileSetting_FileAndRoundTrip: server.keyring_file in
// settings.yaml loads, and survives both conversion directions.
func TestKeyringFileSetting_FileAndRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(strings.Join([]string{
		`schema_version: "1"`,
		`server:`,
		`  keyring_file: /run/secrets/keyring.json`,
		``,
	}, "\n")), 0o644))
	gc, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "/run/secrets/keyring.json", gc.KeyringFile)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	assert.Equal(t, "/run/secrets/keyring.json", v1.KeyringFile)
	back := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "/run/secrets/keyring.json", back.KeyringFile)
}

// TestKeyringFileAllowWorldReadableSetting: the override binds from every env
// spelling (canonical SCION_SERVER_KEYRING_FILE_ALLOW_WORLD_READABLE) and from
// settings.yaml, defaults to false, and survives both conversions.
func TestKeyringFileAllowWorldReadableSetting(t *testing.T) {
	var entry ServerEnvEntry
	for _, e := range ServerEnvEntries() {
		if e.Path == KeyringFileAllowWorldReadableKey {
			entry = e
		}
	}
	require.Equal(t, "SCION_SERVER_KEYRING_FILE_ALLOW_WORLD_READABLE", entry.Canonical)
	for _, env := range append([]string{entry.Canonical}, entry.Aliases...) {
		t.Run(env, func(t *testing.T) {
			assert.Equal(t, KeyringFileAllowWorldReadableKey, versionedEnvKeyMapper(env))
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(env, "true")
			configPath := filepath.Join(home, "settings.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\n"), 0o644))
			gc, err := LoadGlobalConfig(configPath)
			require.NoError(t, err)
			assert.True(t, gc.KeyringFileAllowWorldReadable, "GlobalConfig")
		})
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\n"), 0o644))
	gc, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.False(t, gc.KeyringFileAllowWorldReadable, "default false")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  keyring_file_allow_world_readable: true\n"), 0o644))
	gc, err = LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.True(t, gc.KeyringFileAllowWorldReadable, "settings.yaml")
	v1 := ConvertGlobalToV1ServerConfig(gc)
	assert.True(t, v1.KeyringFileAllowWorldReadable)
	assert.True(t, ConvertV1ServerToGlobalConfig(v1).KeyringFileAllowWorldReadable)
}

// TestKeyringFileAllowWorldReadable_EnvOverYAML: the environment wins over
// settings.yaml (yaml false, env true -> true).
func TestKeyringFileAllowWorldReadable_EnvOverYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  keyring_file_allow_world_readable: false\n"), 0o644))
	t.Setenv("SCION_SERVER_KEYRING_FILE_ALLOW_WORLD_READABLE", "true")
	gc, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.True(t, gc.KeyringFileAllowWorldReadable)
}
