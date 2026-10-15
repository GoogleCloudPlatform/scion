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
	}, serverEnvCanonicalOverrides)

	for path, want := range map[string]string{
		"server.hub.public_url":                          "SCION_SERVER_HUB_ENDPOINT",
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
