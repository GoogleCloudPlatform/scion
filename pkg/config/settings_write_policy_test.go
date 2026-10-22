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
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notStoredKeys are the keys a v1 settings file has no field for. A write
// of any of them used to be accepted and dropped.
var notStoredKeys = []string{
	"hub.token", "hub.apiKey", "hub.lastSyncedAt",
	"bucket.provider", "bucket.name", "bucket.prefix",
}

// globalOnlyKeys are rejected at the project layer only.
var globalOnlyKeys = []string{
	"server.auth.display_name", "server.auth.email", "server.auth.username",
}

// writePolicySecret is the value every rejected write carries; no error
// may contain it.
const writePolicySecret = "s3cr3t-value-must-not-leak"

// writePolicyFixtures are the settings file states a rejected write must
// leave exactly as they were. The legacy one would be auto-migrated (with
// a backup) by any write UpdateSetting goes on with.
var writePolicyFixtures = []struct {
	name    string
	file    string
	content string
}{
	{name: "no-file"},
	{name: "v1-yaml", file: "settings.yaml", content: "# keep this comment\nschema_version: \"1\"\nactive_profile: local\nhub:\n  endpoint: https://hub.example.com\nunknown_key: kept\n"},
	{name: "v1-json", file: "settings.json", content: "{\n  \"schema_version\": \"1\",\n  \"active_profile\": \"local\"\n}\n"},
	{name: "legacy", file: "settings.yaml", content: "active_profile: local\nharnesses:\n  claude:\n    image: example/claude\n"},
}

// dirSnapshot maps each file name in dir to its content.
func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
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

func assertNotWritable(t *testing.T, err error, key string) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSettingNotWritable), "error does not match ErrSettingNotWritable: %v", err)
	var nw *SettingNotWritableError
	require.True(t, errors.As(err, &nw), "error is not a *SettingNotWritableError: %v", err)
	assert.Equal(t, key, nw.Key)
	assert.Contains(t, err.Error(), key)
	assert.NotContains(t, err.Error(), writePolicySecret)
}

// TestUpdateSetting_RejectsNotStoredKeys checks every key a v1 settings
// file cannot hold, at both layers and for every file state: the write
// fails with ErrSettingNotWritable, the error names the key and not the
// value, and the directory is unchanged (no migration, no backup, no new
// file).
func TestUpdateSetting_RejectsNotStoredKeys(t *testing.T) {
	for _, global := range []bool{true, false} {
		for _, fx := range writePolicyFixtures {
			for _, key := range notStoredKeys {
				name := "project"
				if global {
					name = "global"
				}
				t.Run(name+"/"+fx.name+"/"+key, func(t *testing.T) {
					home := t.TempDir()
					t.Setenv("HOME", home)
					dir := filepath.Join(home, ".scion")
					projectPath := ""
					if !global {
						dir = filepath.Join(home, "proj", ".scion")
						projectPath = dir
					}
					require.NoError(t, os.MkdirAll(dir, 0755))
					if fx.file != "" {
						require.NoError(t, os.WriteFile(filepath.Join(dir, fx.file), []byte(fx.content), 0644))
					}
					before := dirSnapshot(t, dir)

					err := UpdateSetting(projectPath, key, writePolicySecret, global)
					assertNotWritable(t, err, key)
					assert.Equal(t, before, dirSnapshot(t, dir), "a rejected write changed the settings directory")
				})
			}
		}
	}
}

// TestUpdateSetting_RejectsGlobalOnlyKeysAtProjectLayer checks that
// server.auth.* is rejected in a project's settings, leaving them as they
// were, and still written to the global settings file.
func TestUpdateSetting_RejectsGlobalOnlyKeysAtProjectLayer(t *testing.T) {
	for _, fx := range writePolicyFixtures {
		for _, key := range globalOnlyKeys {
			t.Run(fx.name+"/"+key, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				dir := filepath.Join(home, "proj", ".scion")
				require.NoError(t, os.MkdirAll(dir, 0755))
				if fx.file != "" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, fx.file), []byte(fx.content), 0644))
				}
				before := dirSnapshot(t, dir)

				err := UpdateSetting(dir, key, writePolicySecret, false)
				assertNotWritable(t, err, key)
				assert.Contains(t, err.Error(), "--global")
				assert.Equal(t, before, dirSnapshot(t, dir), "a rejected write changed the settings directory")
			})
		}
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, UpdateSetting("", "server.auth.display_name", "Dev User", true))
	// A project path that is the global directory itself is the global
	// layer too (the CLI's root --global resolves to it).
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, UpdateSetting(globalDir, "server.auth.email", "dev@example.com", false))
	vs, err := LoadSingleFileVersioned(globalDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Auth)
	assert.Equal(t, "Dev User", vs.Server.Auth.DisplayName)
	assert.Equal(t, "dev@example.com", vs.Server.Auth.Email)
}

// TestUpdateVersionedSetting_RejectsNotStoredKeys checks the direct entry
// point too, on the YAML (in-place) and JSON (struct) paths.
func TestUpdateVersionedSetting_RejectsNotStoredKeys(t *testing.T) {
	for _, fx := range writePolicyFixtures {
		for _, key := range notStoredKeys {
			t.Run(fx.name+"/"+key, func(t *testing.T) {
				dir := t.TempDir()
				if fx.file != "" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, fx.file), []byte(fx.content), 0644))
				}
				before := dirSnapshot(t, dir)
				err := UpdateVersionedSetting(dir, key, writePolicySecret)
				assertNotWritable(t, err, key)
				assert.Equal(t, before, dirSnapshot(t, dir))
			})
		}
	}
}

// TestValidateSettingWrite covers the policy table: rejected keys, the
// layer rule, and keys that stay writable.
func TestValidateSettingWrite(t *testing.T) {
	for _, key := range notStoredKeys {
		for _, global := range []bool{true, false} {
			assertNotWritable(t, ValidateSettingWrite(key, global), key)
		}
	}
	for _, key := range globalOnlyKeys {
		assertNotWritable(t, ValidateSettingWrite(key, false), key)
		assert.NoError(t, ValidateSettingWrite(key, true), key)
	}
	// Writable keys, including ones that only look close to a rejected
	// one, and hub_connections.* (left to UpdateSetting's internal callers).
	for _, key := range []string{
		"active_profile", "hub.endpoint", "hub.brokerToken", "hub.brokerId",
		"project_id", "hub.project_id", "hub.tokenx", "bucket", "server.authx",
		"hub_connections.prod.endpoint",
	} {
		for _, global := range []bool{true, false} {
			assert.NoError(t, ValidateSettingWrite(key, global), key)
		}
	}
}

// TestValidateConfigSetKey checks that "config set" also rejects
// hub_connections.*, which UpdateSetting itself accepts.
func TestValidateConfigSetKey(t *testing.T) {
	for _, global := range []bool{true, false} {
		assertNotWritable(t, ValidateConfigSetKey("hub_connections.prod.endpoint", global), "hub_connections.prod.endpoint")
		assertNotWritable(t, ValidateConfigSetKey("hub.token", global), "hub.token")
		assert.NoError(t, ValidateConfigSetKey("hub.endpoint", global))
	}
	assertNotWritable(t, ValidateConfigSetKey("server.auth.email", false), "server.auth.email")
	assert.NoError(t, ValidateConfigSetKey("server.auth.email", true))
}

// unknownGlobalOnlyKeys are server.auth keys that no writer accepts. They
// are refused as unknown at both layers, with no hint to use --global
// (ptone/scion#5481).
var unknownGlobalOnlyKeys = []string{
	"server.auth.dev_mode", "server.auth.authorized_domains", "server.auth.dev_token",
}

func assertUnknownKey(t *testing.T, err error, key string) {
	t.Helper()
	require.Error(t, err, key)
	assert.NotErrorIs(t, err, ErrSettingNotWritable, key)
	assert.Contains(t, err.Error(), "unknown or complex setting key: "+key)
	assert.NotContains(t, err.Error(), "--global", key)
	assert.NotContains(t, err.Error(), writePolicySecret, key)
}

func TestValidateConfigSetKey_UnknownGlobalOnlyKeys(t *testing.T) {
	for _, key := range unknownGlobalOnlyKeys {
		for _, global := range []bool{true, false} {
			assertUnknownKey(t, ValidateConfigSetKey(key, global), key)
			assertUnknownKey(t, ValidateSettingWrite(key, global), key)
		}
	}
}

// config set and config unset of such a key fail the same way at either
// layer, before any settings file is read or changed.
func TestUpdateAndUnsetSetting_UnknownGlobalOnlyKeys(t *testing.T) {
	for _, fx := range writePolicyFixtures {
		for _, key := range unknownGlobalOnlyKeys {
			for _, global := range []bool{false, true} {
				name := fx.name + "/" + key + "/project"
				if global {
					name = fx.name + "/" + key + "/global"
				}
				t.Run(name, func(t *testing.T) {
					home := t.TempDir()
					t.Setenv("HOME", home)
					dir := filepath.Join(home, "proj", ".scion")
					if global {
						dir = filepath.Join(home, ".scion")
					}
					require.NoError(t, os.MkdirAll(dir, 0755))
					if fx.file != "" {
						require.NoError(t, os.WriteFile(filepath.Join(dir, fx.file), []byte(fx.content), 0644))
					}
					before := dirSnapshot(t, dir)

					assertUnknownKey(t, UpdateSetting(dir, key, writePolicySecret, global), key)
					_, err := UnsetSetting(dir, key, global)
					assertUnknownKey(t, err, key)
					assert.Equal(t, before, dirSnapshot(t, dir), "a rejected write changed the settings directory")
				})
			}
		}
	}
}

// TestSettingNotWritableError_Reasons checks that every rejected key has a
// non-empty reason naming no value, and that the table and the test lists
// agree.
func TestSettingNotWritableError_Reasons(t *testing.T) {
	var got []string
	for k, reason := range settingsNotStoredKeys {
		got = append(got, k)
		assert.NotEmpty(t, strings.TrimSpace(reason), k)
	}
	want := append([]string(nil), notStoredKeys...)
	sort.Strings(got)
	sort.Strings(want)
	assert.Equal(t, want, got)
}

// TestIsSecretSettingKey pins which config keys are masked in output.
func TestIsSecretSettingKey(t *testing.T) {
	for _, k := range []string{"hub.brokerToken", "hub.token", "hub.apiKey", "x.password", "x.client_secret"} {
		assert.True(t, IsSecretSettingKey(k), k)
	}
	for _, k := range []string{"hub.brokerId", "hub.endpoint", "use_host_credentials", "active_profile"} {
		assert.False(t, IsSecretSettingKey(k), k)
	}
}
