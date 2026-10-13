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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPeerSecret is a syntactically valid relay-peer secret for tests.
const testPeerSecret = "0123456789abcdefghijklmnopqrstuvwxyzABCD"

// relaySettingCase is one serverEnvPathAliases key with a valid value and
// accessors for the value each struct consumer loaded.
type relaySettingCase struct {
	path  string
	value string
	gc    func(*GlobalConfig) string
	vs    func(*VersionedSettings) string
}

func vsConduit(vs *VersionedSettings) *V1ServerHubConduitConfig {
	if vs.Server == nil || vs.Server.Hub == nil || vs.Server.Hub.Conduit == nil {
		return &V1ServerHubConduitConfig{}
	}
	return vs.Server.Hub.Conduit
}

func vsRelay(vs *VersionedSettings) *V1ServerRelayConfig {
	if vs.Server == nil || vs.Server.Relay == nil {
		return &V1ServerRelayConfig{}
	}
	return vs.Server.Relay
}

var relaySettingCases = []relaySettingCase{
	{"server.hub.conduit.peer_secret", testPeerSecret,
		func(gc *GlobalConfig) string { return gc.Hub.Conduit.PeerSecret },
		func(vs *VersionedSettings) string { return vsConduit(vs).PeerSecret }},
	{"server.hub.conduit.relay_mode", ConduitRelayModeExternal,
		func(gc *GlobalConfig) string { return gc.Hub.Conduit.RelayMode },
		func(vs *VersionedSettings) string { return vsConduit(vs).RelayMode }},
	{"server.relay.hub_internal_url", "http://hub-internal.scion.svc:9811",
		func(gc *GlobalConfig) string { return gc.Relay.HubInternalURL },
		func(vs *VersionedSettings) string { return vsRelay(vs).HubInternalURL }},
	{"server.relay.public_url", "https://relay-0.example.com",
		func(gc *GlobalConfig) string { return gc.Relay.PublicURL },
		func(vs *VersionedSettings) string { return vsRelay(vs).PublicURL }},
}

// TestRelaySettingCasesCoverAliases keeps relaySettingCases and
// serverEnvPathAliases in step.
func TestRelaySettingCasesCoverAliases(t *testing.T) {
	var cases, aliases []string
	for _, c := range relaySettingCases {
		cases = append(cases, c.path)
	}
	for _, a := range serverEnvPathAliases {
		aliases = append(aliases, a.Path)
	}
	assert.ElementsMatch(t, aliases, cases)
}

// TestServerEnvPathAliasesBindBothSpellings: the canonical and the
// underscored env spelling of each aliased key reach the same field in the
// opsettings env layer, GlobalConfig and VersionedSettings. The seed
// mapper is out of scope: it seeds Layer-1 keys only, and these keys are
// Layer 0.
func TestServerEnvPathAliasesBindBothSpellings(t *testing.T) {
	for _, c := range relaySettingCases {
		a, ok := lookupServerEnvPathAlias(strings.TrimPrefix(c.path, "server."))
		require.True(t, ok, c.path)
		canonical, underscored := a.envSpellings()
		for _, spelling := range []string{canonical, underscored} {
			env := "SCION_SERVER_" + strings.ToUpper(spelling)
			t.Run(env, func(t *testing.T) {
				assert.Equal(t, c.path, versionedEnvKeyMapper(env))
				assert.Equal(t, c.path, serverEnvToOpsettingsKey(strings.ToUpper(spelling)))
				assert.Equal(t, a.GlobalConfig, envKeyToConfigKey(strings.ToUpper(spelling)))

				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv(env, c.value)
				assert.Equal(t, c.value, LoadEnvKoanf().String(c.path), "opsettings env layer")

				configPath := filepath.Join(home, "settings.yaml")
				require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\n"), 0o644))
				gc, err := LoadGlobalConfig(configPath)
				require.NoError(t, err)
				assert.Equal(t, c.value, c.gc(gc), "GlobalConfig")

				vs, err := LoadVersionedSettings("")
				require.NoError(t, err)
				assert.Equal(t, c.value, c.vs(vs), "VersionedSettings")
			})
		}
	}
}

// TestServerEnvPathAliasesEmptyIsUnset: an exported but empty variable
// leaves each aliased key unset (so an empty peer_secret fails the checks
// that require it).
func TestServerEnvPathAliasesEmptyIsUnset(t *testing.T) {
	for _, c := range relaySettingCases {
		a, _ := lookupServerEnvPathAlias(strings.TrimPrefix(c.path, "server."))
		canonical, underscored := a.envSpellings()
		for _, spelling := range []string{canonical, underscored} {
			env := "SCION_SERVER_" + strings.ToUpper(spelling)
			t.Run(env, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv(env, "")
				assert.False(t, LoadEnvKoanf().Exists(c.path), "opsettings env layer")
				configPath := filepath.Join(home, "settings.yaml")
				require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\n"), 0o644))
				gc, err := LoadGlobalConfig(configPath)
				require.NoError(t, err)
				assert.Empty(t, c.gc(gc), "GlobalConfig")
				vs, err := LoadVersionedSettings("")
				require.NoError(t, err)
				assert.Empty(t, c.vs(vs), "VersionedSettings")
			})
		}
	}
}

// koanfTagPathExists reports whether t has a field at the dotted koanf
// tag path (exact tags).
func koanfTagPathExists(t reflect.Type, path string) bool {
	for _, seg := range strings.Split(path, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		found := false
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if strings.Split(f.Tag.Get("koanf"), ",")[0] == seg {
				t, found = f.Type, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestServerEnvPathAliasesPointAtSchemaFields: every alias names a field of
// VersionedSettings and of GlobalConfig, and a schema property whose
// x-env-var is the canonical spelling, so renaming a field breaks this
// test instead of silently orphaning the alias.
func TestServerEnvPathAliasesPointAtSchemaFields(t *testing.T) {
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	for _, a := range serverEnvPathAliases {
		assert.True(t, koanfTagPathExists(reflect.TypeOf(VersionedSettings{}), a.Path), "VersionedSettings has no field at %s", a.Path)
		assert.True(t, koanfTagPathExists(reflect.TypeOf(GlobalConfig{}), a.GlobalConfig), "GlobalConfig has no field at %s", a.GlobalConfig)
		prop := findSchemaProperty(t, root, strings.Split(a.Path, ".")...)
		canonical, _ := a.envSpellings()
		assert.Equal(t, "SCION_SERVER_"+strings.ToUpper(canonical), prop["x-env-var"], "schema x-env-var of %s", a.Path)
	}
}

// TestServerEnvPathAliasesExactPathOnly: an alias matches its whole path,
// never a field of the same name elsewhere.
func TestServerEnvPathAliasesExactPathOnly(t *testing.T) {
	for _, env := range []string{
		"SCION_SERVER_HUB_PUBLICURL",          // server.hub.public_url, not server.relay.public_url
		"SCION_SERVER_HUB_PUBLIC_URL",         // the same, underscored
		"SCION_SERVER_BROKER_PEERSECRET",      // no such key under broker
		"SCION_SERVER_HUB_CONDUIT_RELAYMODEX", // a longer name
	} {
		_, ok := lookupServerEnvPathAlias(strings.TrimPrefix(env, "SCION_SERVER_"))
		assert.False(t, ok, env)
	}
	assert.Equal(t, "server.hub.public_url", versionedEnvKeyMapper("SCION_SERVER_HUB_PUBLIC_URL"))
}

func TestHubConduitRelayModeAndPeerSecret(t *testing.T) {
	assert.Equal(t, ConduitRelayModeInProcess, HubConduitConfig{}.RelayModeValue())
	assert.False(t, HubConduitConfig{}.ExternalRelay())
	assert.True(t, HubConduitConfig{RelayMode: "external"}.ExternalRelay())
	assert.NoError(t, HubConduitConfig{RelayMode: "in_process"}.Validate())
	assert.NoError(t, HubConduitConfig{RelayMode: "external", PeerSecret: testPeerSecret}.Validate())

	err := HubConduitConfig{RelayMode: "sidecar"}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.hub.conduit.relay_mode")

	for name, secret := range map[string]string{
		"short":    "too-short-secret",
		"31 bytes": testPeerSecret[:31],
		"repeated": strings.Repeat("x", 40),
	} {
		t.Run(name, func(t *testing.T) {
			err := HubConduitConfig{PeerSecret: secret}.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "server.hub.conduit.peer_secret")
			assert.NotContains(t, err.Error(), secret, "the error must not carry the value")
		})
	}
	assert.NoError(t, ValidatePeerSecret(testPeerSecret[:32]), "32 octets is the minimum")
	assert.NoError(t, ValidatePeerSecret(""), "unset passes here; required modes check it")
	assert.False(t, HubConduitConfig{PeerSecret: testPeerSecret}.IsZero())
	assert.False(t, HubConduitConfig{RelayMode: "external"}.IsZero())
}

func TestRelayServerConfigValidate(t *testing.T) {
	assert.NoError(t, RelayServerConfig{}.Validate())
	assert.NoError(t, RelayServerConfig{HubInternalURL: "http://hub:9811", PublicURL: "https://relay.example.com/"}.Validate())
	for _, c := range []RelayServerConfig{
		{HubInternalURL: "hub:9811"},
		{HubInternalURL: "ftp://hub"},
		{PublicURL: "https://relay.example.com/?x=1"},
		{PublicURL: "https://user:pw@relay.example.com"},
	} {
		assert.Error(t, c.Validate(), "%+v", c)
	}
}

// TestRelaySettingsRoundTrip: the new keys survive both conversions.
func TestRelaySettingsRoundTrip(t *testing.T) {
	v1 := &V1ServerConfig{
		Hub:   &V1ServerHubConfig{Conduit: &V1ServerHubConduitConfig{RelayMode: "external", PeerSecret: testPeerSecret}},
		Relay: &V1ServerRelayConfig{HubInternalURL: "http://hub:9811", PublicURL: "https://relay.example.com"},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "external", gc.Hub.Conduit.RelayMode)
	assert.Equal(t, testPeerSecret, gc.Hub.Conduit.PeerSecret)
	assert.Equal(t, RelayServerConfig{HubInternalURL: "http://hub:9811", PublicURL: "https://relay.example.com"}, gc.Relay)
	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	require.NotNil(t, back.Hub.Conduit)
	assert.Equal(t, "external", back.Hub.Conduit.RelayMode)
	assert.Equal(t, testPeerSecret, back.Hub.Conduit.PeerSecret)
	assert.Equal(t, v1.Relay, back.Relay)
	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Relay, "an empty relay block is omitted")
}

// TestRelayServerConfigValidateHidesCredentials: a rejected URL carrying
// credentials is reported without them.
func TestRelayServerConfigValidateHidesCredentials(t *testing.T) {
	err := RelayServerConfig{PublicURL: "https://relay-user:hunter2-pw@relay.example.com"}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.relay.public_url")
	assert.NotContains(t, err.Error(), "hunter2-pw")
	err = RelayServerConfig{HubInternalURL: "http://u:pw-secret@[::1"}.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "pw-secret")
}

// TestValidatePeerSecretOctets (design §3.9, lead ruling): peer_secret is
// raw octets. At least 32 of them after removing exactly one trailing
// newline, counted as octets (not characters), and at least 16 distinct.
func TestValidatePeerSecretOctets(t *testing.T) {
	sixteen := "0123456789abcdef" // 16 distinct octets
	for _, tc := range []struct {
		name   string
		secret string
		ok     bool
	}{
		{"31 octets", testPeerSecret[:31], false},
		{"32 octets", testPeerSecret[:32], true},
		{"32 octets plus one trailing newline", testPeerSecret[:32] + "\n", true},
		{"31 octets plus one trailing newline", testPeerSecret[:31] + "\n", false},
		{"31 octets plus two trailing newlines: only one is trimmed", testPeerSecret[:31] + "\n\n", true},
		{"only a newline", "\n", false},
		{"a trailing CRLF counts the CR", testPeerSecret[:31] + "\r\n", true},
		{"multi-byte characters count as octets", strings.Repeat("é", 8) + sixteen, true}, // 16 + 16 octets
		{"15 distinct octets", strings.Repeat(sixteen[:15], 3), false},
		{"16 distinct octets", strings.Repeat(sixteen, 2), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePeerSecret(tc.secret)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "server.hub.conduit.peer_secret")
			if strings.TrimSpace(tc.secret) != "" {
				assert.NotContains(t, err.Error(), strings.TrimSpace(tc.secret))
			}
		})
	}
	assert.Equal(t, "abc", NormalizePeerSecret("abc\n"))
	assert.Equal(t, "abc\n", NormalizePeerSecret("abc\n\n"))
	assert.Equal(t, "abc", HubConduitConfig{PeerSecret: "abc\n"}.PeerSecretValue())
}

// TestPeerSecretFileValueSurvivesEmptyEnv: an exported but empty env
// variable does not clear peer_secret set in settings.yaml.
func TestPeerSecretFileValueSurvivesEmptyEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".scion", "settings.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  hub:\n    conduit:\n      peer_secret: \""+testPeerSecret+"\"\n"), 0o600))
	for _, env := range []string{"SCION_SERVER_HUB_CONDUIT_PEERSECRET", "SCION_SERVER_HUB_CONDUIT_PEER_SECRET"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "")
			gc, err := LoadGlobalConfig(path)
			require.NoError(t, err)
			assert.Equal(t, testPeerSecret, gc.Hub.Conduit.PeerSecret, "GlobalConfig")
			vs, err := LoadVersionedSettings("")
			require.NoError(t, err)
			assert.Equal(t, testPeerSecret, vsConduit(vs).PeerSecret, "VersionedSettings")
		})
	}
}
