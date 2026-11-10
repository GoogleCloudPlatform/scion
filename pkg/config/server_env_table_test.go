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
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serverEnvEntry(t *testing.T, path string) ServerEnvEntry {
	t.Helper()
	for _, e := range ServerEnvEntries() {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no env mapping table entry for %s", path)
	return ServerEnvEntry{}
}

// TestServerEnvTable_NoCollisions: no env name is claimed by two settings.
func TestServerEnvTable_NoCollisions(t *testing.T) {
	assert.Empty(t, getServerEnvTable().collisions)
	if n := len(ServerEnvEntries()); n < 200 {
		t.Fatalf("env mapping table has only %d entries; the schema walk is broken", n)
	}
}

// TestServerEnvTable_CanonicalOverrides pins the documented names that
// differ from the derived spelling (serverEnvCanonicalOverrides).
func TestServerEnvTable_CanonicalOverrides(t *testing.T) {
	assert.Equal(t, []serverEnvCanonicalOverride{
		{Path: "server.hub.public_url", Env: "HUB_ENDPOINT"},
		{Path: "server.broker", Env: "RUNTIMEBROKER", Subtree: true},
		{Path: "server.keyring_file", Env: "KEYRING_FILE"},
		{Path: "server.keyring_file_allow_world_readable", Env: "KEYRING_FILE_ALLOW_WORLD_READABLE"},
	}, serverEnvCanonicalOverrides)

	for path, want := range map[string]string{
		"server.hub.public_url":                          "SCION_SERVER_HUB_ENDPOINT",
		"server.keyring_file":                            "SCION_SERVER_KEYRING_FILE",
		"server.keyring_file_allow_world_readable":       "SCION_SERVER_KEYRING_FILE_ALLOW_WORLD_READABLE",
		"server.broker.port":                             "SCION_SERVER_RUNTIMEBROKER_PORT",
		"server.broker.hub_endpoint":                     "SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT",
		"server.broker.broker_id":                        "SCION_SERVER_RUNTIMEBROKER_BROKERID",
		"server.broker.cors.enabled":                     "SCION_SERVER_RUNTIMEBROKER_CORS_ENABLED",
		"server.broker.allow_container_script_harnesses": "SCION_SERVER_RUNTIMEBROKER_ALLOWCONTAINERSCRIPTHARNESSES",
		// Derived spellings.
		"server.hub.admin_emails":     "SCION_SERVER_HUB_ADMINEMAILS",
		"server.hub.cors.max_age":     "SCION_SERVER_HUB_CORS_MAXAGE",
		"server.log_level":            "SCION_SERVER_LOGLEVEL",
		"default_model":               "SCION_SERVER_DEFAULTMODEL",
		"telemetry.cloud.tls.enabled": "SCION_SERVER_TELEMETRY_CLOUD_TLS_ENABLED",
	} {
		got, ok := ServerEnvKey(path)
		assert.True(t, ok, path)
		assert.Equal(t, want, got, path)
	}

	// The broker names under the settings section name are aliases.
	for _, alias := range []string{
		"SCION_SERVER_BROKER_PORT",
		"SCION_SERVER_BROKER_BROKER_ID",
		"SCION_SERVER_BROKER_BROKERID",
		"SCION_SERVER_RUNTIMEBROKER_BROKER_ID",
		"SCION_SERVER_RUNTIMEBROKER_CORSENABLED",
	} {
		path, isAlias, ok := KeyForServerEnv(alias)
		assert.True(t, ok, alias)
		assert.True(t, isAlias, alias)
		assert.True(t, strings.HasPrefix(path, "server.broker."), alias)
	}

	// The derived spellings of server.hub.public_url are inert.
	for _, name := range []string{"SCION_SERVER_HUB_PUBLICURL", "SCION_SERVER_HUB_PUBLIC_URL"} {
		_, _, ok := KeyForServerEnv(name)
		assert.False(t, ok, name)
		assert.Equal(t, "", serverEnvToOpsettingsKey(strings.TrimPrefix(name, serverEnvPrefix)), name)
		assert.Equal(t, "", envKeyToConfigKey(strings.TrimPrefix(name, serverEnvPrefix)), name)
		assert.Equal(t, "", versionedEnvKeyMapper(name), name)
	}
}

// TestServerEnvTable_AliasesMapLikeCanonical: in every env mapper, each alias
// of a setting maps exactly as the canonical name does.
func TestServerEnvTable_AliasesMapLikeCanonical(t *testing.T) {
	aliases := 0
	for _, e := range ServerEnvEntries() {
		canonRest := strings.TrimPrefix(e.Canonical, serverEnvPrefix)
		for _, a := range e.Aliases {
			aliases++
			rest := strings.TrimPrefix(a, serverEnvPrefix)
			path, isAlias, ok := KeyForServerEnv(a)
			assert.True(t, ok && isAlias && path == e.Path, "%s: KeyForServerEnv = %q, %v, %v", a, path, isAlias, ok)
			assert.Equal(t, serverEnvToOpsettingsKey(canonRest), serverEnvToOpsettingsKey(rest), a)
			assert.Equal(t, envKeyToConfigKey(canonRest), envKeyToConfigKey(rest), a)
			if strings.HasPrefix(e.Path, "server.") {
				// The versioned mapper reads only server settings from
				// SCION_SERVER_* names.
				assert.Equal(t, e.Path, versionedEnvKeyMapper(a), a)
			}
			seedPrefix := ""
			if strings.HasPrefix(e.Path, "server.") {
				seedPrefix = "SERVER_"
			}
			assert.Equal(t, seedEnvToOpsettingsKey(seedPrefix+canonRest), seedEnvToOpsettingsKey(seedPrefix+rest), a)
		}
		path, isAlias, ok := KeyForServerEnv(e.Canonical)
		assert.True(t, ok && !isAlias && path == e.Path, "%s: KeyForServerEnv = %q, %v, %v", e.Canonical, path, isAlias, ok)
		// The name is matched without regard to case.
		path, _, ok = KeyForServerEnv(strings.ToLower(e.Canonical))
		assert.True(t, ok && path == e.Path, "lower-case %s", e.Canonical)
	}
	if aliases < 100 {
		t.Fatalf("only %d aliases; the alias derivation is broken", aliases)
	}
}

// TestServerEnvTable_AdminEmailsBothSpellings: the canonical
// SCION_SERVER_HUB_ADMINEMAILS and the legacy SCION_SERVER_HUB_ADMIN_EMAILS
// reach server.hub.admin_emails in every consumer.
func TestServerEnvTable_AdminEmailsBothSpellings(t *testing.T) {
	e := serverEnvEntry(t, "server.hub.admin_emails")
	require.Equal(t, "SCION_SERVER_HUB_ADMINEMAILS", e.Canonical)
	require.Contains(t, e.Aliases, "SCION_SERVER_HUB_ADMIN_EMAILS")
	for _, name := range []string{"SCION_SERVER_HUB_ADMINEMAILS", "SCION_SERVER_HUB_ADMIN_EMAILS"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(name, "ops@example.com")
			assert.Equal(t, "ops@example.com", LoadEnvKoanf().String("server.hub.admin_emails"), "opsettings env layer")
			assert.Equal(t, []string{"ops@example.com"}, LoadBootstrapKoanf().Strings("server.hub.admin_emails"), "bootstrap merge")

			seed := "SCION_SEED_SERVER_" + strings.TrimPrefix(name, serverEnvPrefix)
			t.Setenv(seed, "seed@example.com")
			assert.Equal(t, "seed@example.com", LoadSeedEnvKoanf().String("server.hub.admin_emails"), "seed layer")

			configPath := filepath.Join(home, "settings.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\n"), 0o644))
			gc, err := LoadGlobalConfig(configPath)
			require.NoError(t, err)
			assert.Equal(t, []string{"ops@example.com"}, gc.Hub.AdminEmails, "GlobalConfig")

			vs, err := LoadVersionedSettings("")
			require.NoError(t, err)
			require.NotNil(t, vs.Server)
			require.NotNil(t, vs.Server.Hub)
			assert.Equal(t, []string{"ops@example.com"}, vs.Server.Hub.AdminEmails, "VersionedSettings")
		})
	}
}

// TestServerEnvTable_ResidueNeverReachesTableSetting: a misspelt name the
// table does not hold never lands on a table setting through the residue
// rules, while a leaf under a named map entry still maps.
func TestServerEnvTable_ResidueNeverReachesTableSetting(t *testing.T) {
	// A half-collapsed spelling of server.hub.port_proxy.response_header_timeout.
	const misspelt = "HUB_PORT_PROXY_RESPONSEHEADERTIMEOUT"
	_, _, ok := KeyForServerEnv(serverEnvPrefix + misspelt)
	require.False(t, ok)
	assert.False(t, ownsServerEnvPath(serverEnvToOpsettingsKey(misspelt)), serverEnvToOpsettingsKey(misspelt))
	assert.False(t, ownsServerEnvPath(versionedEnvKeyMapper(serverEnvPrefix+misspelt)))
	assert.False(t, ownsGlobalConfigEnvPath(envKeyToConfigKey(misspelt)))

	for envKey, want := range map[string]string{
		"PROFILES_LOCAL_RUNTIME":                "profiles.local.runtime",
		"RUNTIMES_DOCKER_TYPE":                  "runtimes.docker.type",
		"HARNESSCONFIGS_CLAUDE_IMAGEPULLPOLICY": "harness_configs.claude.image_pull_policy",
	} {
		assert.Equal(t, want, serverEnvToOpsettingsKey(envKey), envKey)
	}
}

// TestServerEnvTable_ExcludedKeys: the documented exclusions are not in the
// table.
func TestServerEnvTable_ExcludedKeys(t *testing.T) {
	keys := make([]string, 0, len(ServerEnvExcludedKeys))
	for k := range ServerEnvExcludedKeys {
		keys = append(keys, k)
		_, ok := ServerEnvKey(k)
		assert.False(t, ok, "%s is excluded but has a table entry", k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"default_harness_auth", "roles"}, keys)
}

// TestServerEnvTable_OutsideKoanfFieldsFromEnv: a SCION_SERVER_* variable of
// a setting GlobalConfig holds in a koanf:"-" field overrides the file
// value and leaves the rest of the section in place.
func TestServerEnvTable_OutsideKoanfFieldsFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(`schema_version: "1"
server:
  mode: workstation
default_timezone: Europe/Paris
telemetry:
  cloud:
    endpoint: file.example.com:4317
    protocol: grpc
`), 0o644))
	t.Setenv("SCION_SERVER_DEFAULTTIMEZONE", "America/New_York")
	t.Setenv("SCION_SERVER_TELEMETRY_CLOUD_ENDPOINT", "env.example.com:4317")
	gc, err := LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, "America/New_York", gc.DefaultTimezone)
	require.NotNil(t, gc.TelemetryConfig)
	require.NotNil(t, gc.TelemetryConfig.Cloud)
	assert.Equal(t, "env.example.com:4317", gc.TelemetryConfig.Cloud.Endpoint)
	assert.Equal(t, "grpc", gc.TelemetryConfig.Cloud.Protocol, "the rest of the section stays")

	// An empty value is unset: the file value stays.
	t.Setenv("SCION_SERVER_DEFAULTTIMEZONE", "")
	gc, err = LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, "Europe/Paris", gc.DefaultTimezone)
}

// writeServerSettings writes a settings.yaml with a server key under home
// (the settings.yaml load path of LoadGlobalConfig).
func writeServerSettings(t *testing.T, home, extra string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  mode: workstation\n"+extra), 0o644))
}

// TestServerEnvTable_DirectNamesNotMapped: a name read directly with
// os.Getenv (directServerEnvNames) never binds a setting, so a value only
// its direct reader accepts (SCION_SERVER_ADMIN_MODE=yes) does not fail
// LoadGlobalConfig on either load path.
func TestServerEnvTable_DirectNamesNotMapped(t *testing.T) {
	for _, mode := range []string{"legacy", "settings"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if mode == "settings" {
				writeServerSettings(t, home, "")
			}
			t.Setenv("SCION_SERVER_ADMIN_MODE", "yes")
			t.Setenv("SCION_SERVER_MAINTENANCE_MESSAGE", "down for maintenance")
			gc, err := LoadGlobalConfig(t.TempDir())
			require.NoError(t, err)
			assert.False(t, gc.AdminMode, "the env var is read directly at startup, not through GlobalConfig")
		})
	}
	for name := range directServerEnvNames {
		if _, _, ok := KeyForServerEnv(name); ok {
			continue // also a table setting (HUB_HUBNAME, HUB_HUBID)
		}
		rest := strings.TrimPrefix(name, serverEnvPrefix)
		assert.Equal(t, "", envKeyToConfigKey(rest), name)
		assert.Equal(t, "", serverEnvToOpsettingsKey(rest), name)
		assert.Equal(t, "", versionedEnvKeyMapper(name), name)
	}
}

// setEnvInOrder unsets names, then sets them in the given order, so they
// appear in os.Environ in that order.
func setEnvInOrder(t *testing.T, kv [][2]string) {
	t.Helper()
	for _, p := range kv {
		t.Setenv(p[0], "")
		require.NoError(t, os.Unsetenv(p[0]))
	}
	for _, p := range kv {
		t.Setenv(p[0], p[1])
	}
}

// TestServerEnvTable_CanonicalBeatsAlias: when a canonical name and its
// alias are both set, the canonical value wins in every consumer, whatever
// their order in the process environment; among aliases the first in sort
// order wins.
func TestServerEnvTable_CanonicalBeatsAlias(t *testing.T) {
	const canon, alias = "SCION_SERVER_HUB_ADMINEMAILS", "SCION_SERVER_HUB_ADMIN_EMAILS"
	orders := map[string][][2]string{
		"canonical first": {{canon, "canon@example.com"}, {alias, "legacy@example.com"}},
		"alias first":     {{alias, "legacy@example.com"}, {canon, "canon@example.com"}},
	}
	for name, kv := range orders {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			setEnvInOrder(t, kv)
			assert.Equal(t, "canon@example.com", LoadEnvKoanf().String("server.hub.admin_emails"), "opsettings env layer")
			gc, err := LoadGlobalConfig(t.TempDir())
			require.NoError(t, err)
			assert.Equal(t, []string{"canon@example.com"}, gc.Hub.AdminEmails, "GlobalConfig (legacy path)")
			writeServerSettings(t, home, "")
			gc, err = LoadGlobalConfig(t.TempDir())
			require.NoError(t, err)
			assert.Equal(t, []string{"canon@example.com"}, gc.Hub.AdminEmails, "GlobalConfig (settings path)")
			vs, err := LoadVersionedSettings("")
			require.NoError(t, err)
			require.NotNil(t, vs.Server)
			require.NotNil(t, vs.Server.Hub)
			assert.Equal(t, []string{"canon@example.com"}, vs.Server.Hub.AdminEmails, "VersionedSettings")

			var seed [][2]string
			for _, p := range kv {
				seed = append(seed, [2]string{"SCION_SEED_SERVER_" + strings.TrimPrefix(p[0], serverEnvPrefix), p[1]})
			}
			setEnvInOrder(t, seed)
			assert.Equal(t, "canon@example.com", LoadSeedEnvKoanf().String("server.hub.admin_emails"), "seed layer")
		})
	}

	// koanf:"-" settings: the canonical name wins in the overlay too.
	t.Run("outside koanf", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeServerSettings(t, home, "")
		setEnvInOrder(t, [][2]string{{"SCION_SERVER_DEFAULT_TIMEZONE", "Europe/Paris"}, {"SCION_SERVER_DEFAULTTIMEZONE", "America/New_York"}})
		gc, err := LoadGlobalConfig(t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, "America/New_York", gc.DefaultTimezone)
	})

	// Two aliases, no canonical name: the alias that sorts first wins.
	e := serverEnvEntry(t, "server.broker.hub_endpoint")
	require.Contains(t, e.Aliases, "SCION_SERVER_BROKER_HUBENDPOINT")
	require.Contains(t, e.Aliases, "SCION_SERVER_BROKER_HUB_ENDPOINT")
	for name, kv := range map[string][][2]string{
		"aliases, sorted first set first": {{"SCION_SERVER_BROKER_HUBENDPOINT", "http://first:8080"}, {"SCION_SERVER_BROKER_HUB_ENDPOINT", "http://second:8080"}},
		"aliases, sorted first set last":  {{"SCION_SERVER_BROKER_HUB_ENDPOINT", "http://second:8080"}, {"SCION_SERVER_BROKER_HUBENDPOINT", "http://first:8080"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			setEnvInOrder(t, kv)
			assert.Equal(t, "http://first:8080", LoadEnvKoanf().String("server.broker.hub_endpoint"))
		})
	}
}

// TestServerEnvTable_OutsideKoanfBadValueFails: an unparseable value of a
// setting GlobalConfig holds in a koanf:"-" field fails LoadGlobalConfig,
// with an error that names the environment, as an unparseable koanf field
// value does.
func TestServerEnvTable_OutsideKoanfBadValueFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerSettings(t, home, "")
	t.Setenv("SCION_SERVER_QUOTAS_ENFORCEBROKERQUOTAS", "not-a-bool")
	_, err := LoadGlobalConfig(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SCION_SERVER_* environment")
}

// TestServerEnvTable_OutsideKoanfLegacyPath: on the legacy load path (no
// server key in settings.yaml) the env still overrides a koanf:"-" setting
// read from the top-level settings.yaml sections, and the rest of the
// section stays.
func TestServerEnvTable_OutsideKoanfLegacyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(`schema_version: "1"
telemetry:
  cloud:
    endpoint: file.example.com:4317
    protocol: grpc
`), 0o644))
	t.Setenv("SCION_SERVER_TELEMETRY_CLOUD_ENDPOINT", "env.example.com:4317")
	gc, err := LoadGlobalConfig(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, gc.TelemetryConfig)
	require.NotNil(t, gc.TelemetryConfig.Cloud)
	assert.Equal(t, "env.example.com:4317", gc.TelemetryConfig.Cloud.Endpoint)
	assert.Equal(t, "grpc", gc.TelemetryConfig.Cloud.Protocol)
}

// TestServerEnvTable_ImageRegistryNotVersioned: the SCION_SERVER_* and
// SCION_SEED_* spellings of image_registry are hub file-tier bootstrap
// values (the opsettings env layer and the seed layer); they never set the
// VersionedSettings image_registry key.
func TestServerEnvTable_ImageRegistryNotVersioned(t *testing.T) {
	for _, name := range []string{"SCION_SERVER_IMAGEREGISTRY", "SCION_SERVER_IMAGE_REGISTRY", "SCION_SEED_IMAGEREGISTRY", "SCION_SEED_IMAGE_REGISTRY"} {
		assert.NotEqual(t, "image_registry", versionedEnvKeyMapper(name), name)
	}
	assert.Equal(t, "image_registry", serverEnvToOpsettingsKey("IMAGEREGISTRY"))
	assert.Equal(t, "image_registry", seedEnvToOpsettingsKey("IMAGEREGISTRY"))
}

// TestServerEnvTable_AmbientEnvCleared: the package TestMain leaves no
// SCION_SERVER_* or SCION_SEED_* variable from the environment running the
// tests, so the alias mapping tests do not depend on it.
func TestServerEnvTable_AmbientEnvCleared(t *testing.T) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, serverEnvPrefix) || strings.HasPrefix(name, seedEnvPrefix) {
			t.Errorf("%s is set in the test process; TestMain must clear it", name)
		}
	}
}
