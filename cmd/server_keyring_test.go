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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic fixtures only.
const (
	cmdKRLegacySecret = "test-only-cmd-legacy-secret"
	cmdKRGen1Secret   = "test-only-cmd-keyring-generation-1-0123456789"
)

// writeCmdKeyring writes a pilot-valid keyring file (legacy entry plus an
// active gen1) and returns its path.
func writeCmdKeyring(t *testing.T, mutate func(doc map[string]any)) string {
	t.Helper()
	doc := map[string]any{
		"schema":   1,
		"revision": "cmd-test-rev",
		"signing": map[string]any{
			"active": "gen1",
			"emit":   "legacy",
			"keys": []any{
				map[string]any{"id": "legacy", "secret": cmdKRLegacySecret, "sign_until": nil, "verify_until": nil},
				map[string]any{"id": "gen1", "secret": cmdKRGen1Secret, "sign_until": nil, "verify_until": nil},
			},
			// The active gen1 must read ID-less values too (phase 4).
			"legacy_read_ids": []any{"legacy", "gen1"},
		},
		"encryption": map[string]any{
			"active": "legacy", "write_format": "v1",
			"keys":               []any{map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(cmdKRLegacySecret))}},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
	if mutate != nil {
		mutate(doc)
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "keyring.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// isolateKeyring resets the keyring globals and the server flags it reads.
func isolateKeyring(t *testing.T) {
	t.Helper()
	isolateServerSecretEnv(t)
	oldHub, oldWeb, oldKR := enableHub, enableWeb, serverKeyring
	enableHub, enableWeb, serverKeyring = true, true, nil
	t.Cleanup(func() { enableHub, enableWeb, serverKeyring = oldHub, oldWeb, oldKR })
}

func assertNoCmdKRSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{cmdKRLegacySecret, cmdKRGen1Secret} {
		assert.NotContains(t, s, secret)
	}
}

func TestLoadServerKeyring_LegacyModeUnchanged(t *testing.T) {
	isolateKeyring(t)
	webSessionSecret = "test-only-flag-secret"
	logs := captureSecretEnvSlog(t)
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{}))
	assert.Nil(t, serverKeyringFromFile())
	assert.Equal(t, "test-only-flag-secret", resolveSessionSecret(), "flag > env resolution unchanged")
	assert.Equal(t, "shared_secret", webSessionKeySource(resolveSessionSecret()))
	assert.Nil(t, serverSigningRingFromFile(), "the web server keeps the legacy adapter")
	assert.Contains(t, logs.String(), "legacy single-secret mode")
	assert.Contains(t, logs.String(), "agent JWTs", "the notice names the coupled uses")
	assert.NotContains(t, logs.String(), "test-only-flag-secret")
}

func TestLoadServerKeyring_RingMode(t *testing.T) {
	isolateKeyring(t)
	logs := captureSecretEnvSlog(t)
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}))
	k := serverKeyringFromFile()
	require.NotNil(t, k)
	assert.Equal(t, "gen1", serverSigningRingFromFile().ActiveID())
	// Every other purpose keeps the legacy entry.
	assert.Equal(t, cmdKRLegacySecret, resolveSessionSecret())
	assert.Equal(t, cmdKRLegacySecret, sessionSecretValue())
	assert.Equal(t, hub.SigningKeySourceKeyring, webSessionKeySource(resolveSessionSecret()))
	assert.Contains(t, logs.String(), "session cookies, JWTs and those capabilities only")
	assertNoCmdKRSecret(t, logs.String())
}

func TestLoadServerKeyring_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *config.GlobalConfig
		want  string
	}{
		{"flag secret too", func(t *testing.T) *config.GlobalConfig {
			webSessionSecret = "test-only-flag-secret"
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}
		}, "cannot be combined with a legacy session secret"},
		{"env secret too", func(t *testing.T) *config.GlobalConfig {
			t.Setenv(sessionSecretEnv, "test-only-env-secret")
			scrubServerSecretEnv()
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}
		}, "cannot be combined with a legacy session secret"},
		{"deprecated env secret too", func(t *testing.T) *config.GlobalConfig {
			t.Setenv(legacySessionSecretEnv, "test-only-env-secret")
			scrubServerSecretEnv()
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}
		}, "cannot be combined with a legacy session secret"},
		{"missing file", func(t *testing.T) *config.GlobalConfig {
			return &config.GlobalConfig{KeyringFile: filepath.Join(t.TempDir(), "absent.json")}
		}, config.KeyringFileKey},
		{"duplicate ids", func(t *testing.T) *config.GlobalConfig {
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, func(d map[string]any) {
				keys := d["signing"].(map[string]any)["keys"].([]any)
				keys[1].(map[string]any)["id"] = "legacy"
			})}
		}, "duplicate signing key id: signing.keys[1] repeats signing.keys[0]"},
		{"unknown field", func(t *testing.T) *config.GlobalConfig {
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, func(d map[string]any) { d["extra"] = 1 })}
		}, "unknown field near byte"},
		{"pilot: versioned emit", func(t *testing.T) *config.GlobalConfig {
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, func(d map[string]any) {
				d["signing"].(map[string]any)["emit"] = "versioned"
			})}
		}, "not supported in this release"},
		{"whitespace-only path", func(t *testing.T) *config.GlobalConfig {
			return &config.GlobalConfig{KeyringFile: "   "}
		}, "whitespace"},
		{"path with trailing newline", func(t *testing.T) *config.GlobalConfig {
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil) + "\n"}
		}, "whitespace"},
		{"web without hub", func(t *testing.T) *config.GlobalConfig {
			enableHub = false
			return &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}
		}, "does not run the hub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateKeyring(t)
			err := loadServerKeyring(tc.setup(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assertNoCmdKRSecret(t, err.Error())
			assert.NotContains(t, err.Error(), "test-only-flag-secret")
			assert.NotContains(t, err.Error(), "test-only-env-secret")
			assert.Nil(t, serverKeyringFromFile(), "a refused keyring is never kept (no fallback either)")
		})
	}
}

// TestLoadServerKeyring_BrokerOnlyNeverOpensFile: a Runtime Broker-only
// server (no hub, no web) ignores server.keyring_file without reading it,
// even when the path is unreadable.
func TestLoadServerKeyring_BrokerOnlyNeverOpensFile(t *testing.T) {
	isolateKeyring(t)
	enableHub, enableWeb = false, false
	opened := false
	old := loadServerKeyringFile
	loadServerKeyringFile = func(string, keyring.LoadOptions) (*keyring.Keyring, error) {
		opened = true
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() { loadServerKeyringFile = old })
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: "/nonexistent/keyring.json"}))
	assert.False(t, opened)
	assert.Nil(t, serverKeyringFromFile())
}

func TestVerifyKeyringExperiment(t *testing.T) {
	isolateKeyring(t)
	// Legacy mode: no gate.
	require.NoError(t, verifyKeyringExperiment(&hub.Server{}))
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}))
	// A hub without operational settings resolves the default (off).
	err := verifyKeyringExperiment(&hub.Server{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errKeyringExperimentOff))
	assert.Contains(t, err.Error(), "server.secret_rotation")
}

// TestKeyringFileEnvScrubContract: every spelling of server.keyring_file is
// scrubbed from the process env before any child starts, refused on every
// host-to-agent path, and never expanded into agent-facing values.
func TestKeyringFileEnvScrubContract(t *testing.T) {
	isolateServerSecretEnv(t)
	var tableNames []string
	for _, e := range config.ServerEnvEntries() {
		if e.Path == config.KeyringFileKey {
			tableNames = append(append(tableNames, e.Canonical), e.Aliases...)
		}
	}
	require.NotEmpty(t, tableNames)
	for _, name := range tableNames {
		assert.Contains(t, serverSecretEnvNames, name, "every env spelling is scrubbed")
		assert.True(t, util.IsAgentRefusedHostEnvName(name), "agent/plugin propagation refuses %s", name)
		t.Setenv(name, "/run/secrets/test-keyring.json")
	}
	expanded, _, refused := util.ExpandEnvRefusing("x${SCION_SERVER_KEYRING_FILE}y", util.IsAgentRefusedHostEnvName)
	assert.Equal(t, "xy", expanded)
	assert.Equal(t, []string{"SCION_SERVER_KEYRING_FILE"}, refused)

	scrubServerSecretEnv()
	for _, name := range tableNames {
		_, ok := os.LookupEnv(name)
		assert.False(t, ok, "%s is still set after the scrub", name)
	}
	for _, kv := range util.EnvWithoutAgentRefusedNames([]string{"SCION_SERVER_KEYRING_FILE=/x", "PATH=/bin"}) {
		assert.False(t, strings.HasPrefix(kv, "SCION_SERVER_KEYRING_FILE="))
	}
}

func TestValidateRelayRoleFlags_RefusesKeyringFile(t *testing.T) {
	f := pflag.NewFlagSet("server start", pflag.ContinueOnError)
	var path string
	f.StringVar(&path, "keyring-file", "", "")
	require.NoError(t, validateRelayRoleFlags(f))
	require.NoError(t, f.Parse([]string{"--keyring-file=/run/secrets/k.json"}))
	err := validateRelayRoleFlags(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--keyring-file")
}

// TestRunRelayRoleNeverLoadsKeyring: the relay role never opens the keyring
// file, whether server.keyring_file comes from settings or the env; its
// spellings are unset like every other server secret name.
func TestRunRelayRoleNeverLoadsKeyring(t *testing.T) {
	isolateKeyring(t)
	t.Setenv(keyringFileEnv, "/nonexistent/env-keyring.json")
	t.Setenv(keyringFileAliasEnv, "/nonexistent/env-keyring.json")
	called := false
	oldLoad := loadServerKeyringFile
	loadServerKeyringFile = func(string, keyring.LoadOptions) (*keyring.Keyring, error) {
		called = true
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() { loadServerKeyringFile = oldLoad })
	prevOpen := openRelayDB
	t.Cleanup(func() { openRelayDB = prevOpen })
	errStop := errors.New("stop before the database")
	openRelayDB = func(context.Context, relaydb.Options) (*relaydb.DB, error) { return nil, errStop }

	cfg := relayRoleTestConfig()
	cfg.KeyringFile = "/nonexistent/settings-keyring.json"
	require.ErrorIs(t, runRelayRole(context.Background(), cfg), errStop)
	assert.False(t, called, "the relay opened the keyring file")
	assert.Nil(t, serverKeyringFromFile())
	for _, name := range []string{keyringFileEnv, keyringFileAliasEnv} {
		_, ok := os.LookupEnv(name)
		assert.False(t, ok, "%s still set in the relay process", name)
	}
}

// TestLoadServerKeyring_WorldReadablePolicy: a world-readable keyring file
// refuses startup by default and loads with the operator override; world
// write is refused under both. Errors carry the path and mode, no material.
func TestLoadServerKeyring_WorldReadablePolicy(t *testing.T) {
	cases := []struct {
		mode  os.FileMode
		allow bool
		ok    bool
	}{
		{0o400, false, true}, {0o644, false, false}, {0o444, false, false},
		{0o644, true, true}, {0o444, true, true}, {0o646, true, false}, {0o602, false, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%#o/allow=%v", tc.mode, tc.allow), func(t *testing.T) {
			isolateKeyring(t)
			path := writeCmdKeyring(t, nil)
			require.NoError(t, os.Chmod(path, tc.mode))
			err := loadServerKeyring(&config.GlobalConfig{KeyringFile: path, KeyringFileAllowWorldReadable: tc.allow})
			if tc.ok {
				require.NoError(t, err)
				assert.NotNil(t, serverKeyringFromFile())
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("%#o", tc.mode))
			assertNoCmdKRSecret(t, err.Error())
			assert.Nil(t, serverKeyringFromFile())
		})
	}
}

// TestLoadServerKeyring_UnreadableRingNeverSelectsLegacy (ptone/scion#1530
// Phase 3): a configured keyring file that is missing, unreadable or not a
// regular file refuses startup; no keyring is kept and the legacy-mode
// notice is never printed (missing managed material never selects legacy
// mode). hub.New refuses the same configuration on its own through
// KeyringConfigured.
func TestLoadServerKeyring_UnreadableRingNeverSelectsLegacy(t *testing.T) {
	unreadable := writeCmdKeyring(t, nil)
	require.NoError(t, os.Chmod(unreadable, 0))
	if f, err := os.Open(unreadable); err == nil {
		_ = f.Close()
		t.Skip("running with privileges that read mode-000 files")
	}
	for name, path := range map[string]string{
		"missing":    filepath.Join(t.TempDir(), "absent.json"),
		"unreadable": unreadable,
		"directory":  t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			isolateKeyring(t)
			logs := captureSecretEnvSlog(t)
			cfg := &config.GlobalConfig{KeyringFile: path}
			err := loadServerKeyring(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), config.KeyringFileKey)
			assert.Nil(t, serverKeyringFromFile())
			assert.NotContains(t, logs.String(), "legacy single-secret mode")
			hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
			assert.True(t, hubCfg.KeyringConfigured)
			assert.Nil(t, hubCfg.Keyring)
		})
	}
}

// TestApplyKeyringFlagOverrides: flags win over the loaded settings (which
// already put env over settings.yaml); an unset flag leaves them alone.
func TestApplyKeyringFlagOverrides(t *testing.T) {
	oldFile, oldAllow := serverKeyringFile, serverKeyringAllowWorldReadable
	t.Cleanup(func() { serverKeyringFile, serverKeyringAllowWorldReadable = oldFile, oldAllow })
	newFlags := func() *pflag.FlagSet {
		f := pflag.NewFlagSet("server start", pflag.ContinueOnError)
		f.StringVar(&serverKeyringFile, "keyring-file", "", "")
		f.BoolVar(&serverKeyringAllowWorldReadable, "keyring-file-allow-world-readable", false, "")
		return f
	}
	// Unset flags: the settings/env values stand.
	cfg := &config.GlobalConfig{KeyringFile: "/from/env.json", KeyringFileAllowWorldReadable: true}
	applyKeyringFlagOverrides(newFlags(), cfg)
	assert.Equal(t, "/from/env.json", cfg.KeyringFile)
	assert.True(t, cfg.KeyringFileAllowWorldReadable)
	// Set flags win, including an explicit false.
	f := newFlags()
	require.NoError(t, f.Parse([]string{"--keyring-file=/from/flag.json", "--keyring-file-allow-world-readable=false"}))
	applyKeyringFlagOverrides(f, cfg)
	assert.Equal(t, "/from/flag.json", cfg.KeyringFile)
	assert.False(t, cfg.KeyringFileAllowWorldReadable)
}

func TestValidateRelayRoleFlags_RefusesKeyringAllowWorldReadable(t *testing.T) {
	f := pflag.NewFlagSet("server start", pflag.ContinueOnError)
	var allow bool
	f.BoolVar(&allow, "keyring-file-allow-world-readable", false, "")
	require.NoError(t, f.Parse([]string{"--keyring-file-allow-world-readable"}))
	err := validateRelayRoleFlags(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--keyring-file-allow-world-readable")
}

// The one shared ValueCipher (ptone/scion#1530 §5): legacy mode wraps the
// resolved session secret; ring mode wraps the keyring file's encryption
// ring (managed, the very ring object), and an encryption ring that could
// not read back its own v1 writes fails startup without key material.
func TestBuildServerValueCipher(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		isolateKeyring(t)
		oldVC := serverValueCipher
		t.Cleanup(func() { serverValueCipher = oldVC })
		webSessionSecret = "test-only-flag-secret"
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{}))
		require.NoError(t, buildServerValueCipher())
		require.NotNil(t, serverValueCipher)
		assert.True(t, serverValueCipher.Enabled())
		assert.False(t, serverValueCipher.Managed())
		v, err := serverValueCipher.Encrypt("fake-value")
		require.NoError(t, err)
		// Byte-compatible with the legacy derivation of that secret.
		assert.Equal(t, keyring.LegacyID, serverValueCipher.WriterID())
		pt, err := secret.NewLegacyValueCipher("test-only-flag-secret").Decrypt(v)
		require.NoError(t, err)
		assert.Equal(t, "fake-value", pt)
	})
	t.Run("legacy without a secret is disabled", func(t *testing.T) {
		isolateKeyring(t)
		oldVC := serverValueCipher
		t.Cleanup(func() { serverValueCipher = oldVC })
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{}))
		require.NoError(t, buildServerValueCipher())
		require.NotNil(t, serverValueCipher)
		assert.False(t, serverValueCipher.Enabled())
	})
	t.Run("ring mode", func(t *testing.T) {
		isolateKeyring(t)
		oldVC := serverValueCipher
		t.Cleanup(func() { serverValueCipher = oldVC })
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}))
		require.NoError(t, buildServerValueCipher())
		require.NotNil(t, serverValueCipher)
		assert.True(t, serverValueCipher.Managed())
		assert.True(t, serverValueCipher.UsesRing(serverKeyringFromFile().Encryption()))
	})
	t.Run("ring mode writer not readable", func(t *testing.T) {
		isolateKeyring(t)
		oldVC := serverValueCipher
		t.Cleanup(func() { serverValueCipher = oldVC })
		path := writeCmdKeyring(t, func(doc map[string]any) {
			doc["encryption"].(map[string]any)["legacy_v1_read_ids"] = []any{}
		})
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: path}))
		err := buildServerValueCipher()
		require.ErrorIs(t, err, secret.ErrWriterNotReadable)
		assert.Nil(t, serverValueCipher)
		assertNoCmdKRSecret(t, err.Error())
	})
}

// TestBuildHubServerConfig_KeyringConfigured: the hub is told a keyring
// file is configured exactly when server.keyring_file is set.
func TestBuildHubServerConfig_KeyringConfigured(t *testing.T) {
	isolateKeyring(t)
	assert.False(t, buildHubServerConfig(&config.GlobalConfig{}, "https://hub.example.com", "", nil, false, "", nil).KeyringConfigured)
	cfg := &config.GlobalConfig{KeyringFile: writeCmdKeyring(t, nil)}
	require.NoError(t, loadServerKeyring(cfg))
	hubCfg := buildHubServerConfig(cfg, "https://hub.example.com", "", nil, false, "", nil)
	assert.True(t, hubCfg.KeyringConfigured)
	require.NotNil(t, hubCfg.Keyring)
	assert.Equal(t, "gen1", hubCfg.Keyring.Signing().ActiveID())
}
