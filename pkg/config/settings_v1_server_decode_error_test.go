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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadGlobalConfig_ServerDecodeErrorFailsWithoutFallback verifies that a
// decode error anywhere in a present settings.yaml "server:" section returns a
// clear error naming the offending field and never falls back to a --config
// settings.yaml or legacy server.yaml (ptone/scion#6187).
func TestLoadGlobalConfig_ServerDecodeErrorFailsWithoutFallback(t *testing.T) {
	tests := []struct {
		name           string
		settingsYAML   string
		wantField      string
		forbidInErrMsg string
	}{
		{
			name: "string where list expected",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
  hub:
    port: 9810
    admin_emails: "admin@example.com"
`,
			wantField:      "server.hub.admin_emails",
			forbidInErrMsg: "admin@example.com",
		},
		{
			name: "map where scalar expected",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
  database:
    driver: postgres
    url:
      host: db.example.internal
      password: super-secret-db-password
`,
			wantField:      "server.database.url",
			forbidInErrMsg: "super-secret-db-password",
		},
		{
			name: "string where integer expected",
			settingsYAML: `schema_version: "1"
server:
  broker:
    port: "nine-thousand"
`,
			wantField:      "server.broker.port",
			forbidInErrMsg: "nine-thousand",
		},
		{
			name: "string where boolean expected",
			settingsYAML: `schema_version: "1"
server:
  auth:
    dev_mode: "maybe"
`,
			wantField:      "server.auth.dev_mode",
			forbidInErrMsg: "maybe",
		},
		{
			name: "invalid duration scalar",
			settingsYAML: `schema_version: "1"
server:
  oidc:
    enabled: true
    token_lifetime: "not-a-duration"
`,
			wantField:      "server.oidc.token_lifetime",
			forbidInErrMsg: "not-a-duration",
		},
		{
			name: "scalar at server root instead of mapping",
			settingsYAML: `schema_version: "1"
server: "not-a-mapping"
`,
			wantField:      "server",
			forbidInErrMsg: "not-a-mapping",
		},
		{
			name: "mistyped nested slice element field",
			settingsYAML: `schema_version: "1"
server:
  broker:
    instances:
      - key: local-docker
        name: example-docker
        runtime_target: "docker"
`,
			wantField:      "server.broker.instances[0].runtime_target",
			forbidInErrMsg: `"docker"`,
		},
		{
			name: "mismatched explicit YAML tag on secret scalar in server",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
  database:
    driver: postgres
    url: !!int "postgres://scion:super-secret-db-password@db.example.internal/scion"
`,
			wantField:      "server.database.url",
			forbidInErrMsg: "super-secret-db-password",
		},
		{
			name: "sequence map key inside mistyped server scalar field",
			settingsYAML: `schema_version: "1"
server:
  auth:
    dev_token:
      ["super-secret-dev-token"]: true
`,
			wantField:      "server.auth.dev_token",
			forbidInErrMsg: "super-secret-dev-token",
		},
		{
			name: "mistyped map entry value inside server map field",
			settingsYAML: `schema_version: "1"
server:
  notification_channels:
    - type: webhook
      params:
        webhook_url:
          - "https://hooks.example.internal/super-secret-hook"
`,
			wantField:      "server.notification_channels[0].params.webhook_url",
			forbidInErrMsg: "super-secret-hook",
		},
		{
			name: "non-scalar map key inside server map field",
			settingsYAML: `schema_version: "1"
server:
  notification_channels:
    - type: webhook
      params:
        ["super-secret-webhook-key"]: "https://hooks.example.internal/super-secret-webhook-val"
`,
			wantField:      "server.notification_channels[0].params",
			forbidInErrMsg: "super-secret-webhook",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)

			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			settingsPath := filepath.Join(globalDir, "settings.yaml")
			require.NoError(t, os.WriteFile(settingsPath, []byte(tc.settingsYAML), 0o644))

			// Populate BOTH a legacy server.yaml in globalDir AND a valid
			// --config directory so any silent fallback would succeed with
			// recognizable fallback values instead of failing.
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte(`logLevel: warn
hub:
  port: 7777
`), 0o644))

			cfgDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  hub:
    port: 6666
`), 0o644))

			gc, err := LoadGlobalConfig(cfgDir)
			require.Error(t, err, "LoadGlobalConfig must fail rather than falling back, got config=%+v", gc)
			assert.Nil(t, gc)
			assert.Contains(t, err.Error(), settingsPath)
			assert.Contains(t, err.Error(), tc.wantField)
			if tc.forbidInErrMsg != "" {
				assert.NotContains(t, err.Error(), tc.forbidInErrMsg, "error must name the field without echoing raw values")
			}
		})
	}
}

// TestLoadGlobalConfig_ConfigDirServerDecodeErrorFailsWithoutFallback verifies
// that when global settings.yaml has no server key and --config points to a
// directory whose settings.yaml has a mistyped server field, LoadGlobalConfig
// fails naming that file and field instead of falling back to server.yaml.
func TestLoadGlobalConfig_ConfigDirServerDecodeErrorFailsWithoutFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
default_template: claude
`), 0o644))

	cfgDir := t.TempDir()
	cfgSettingsPath := filepath.Join(cfgDir, "settings.yaml")
	require.NoError(t, os.WriteFile(cfgSettingsPath, []byte(`schema_version: "1"
server:
  hub:
    cors:
      allowed_origins: "https://example.com"
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "server.yaml"), []byte(`hub:
  port: 5555
`), 0o644))

	gc, err := LoadGlobalConfig(cfgDir)
	require.Error(t, err)
	assert.Nil(t, gc)
	assert.Contains(t, err.Error(), cfgSettingsPath)
	assert.Contains(t, err.Error(), "server.hub.cors.allowed_origins")
}

// TestLoadGlobalConfig_MalformedSettingsYAMLFailsWithoutFallback verifies that
// invalid YAML syntax in settings.yaml fails LoadGlobalConfig instead of
// silently falling back to server.yaml.
func TestLoadGlobalConfig_MalformedSettingsYAMLFailsWithoutFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte("server:\n  hub: [unclosed\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

	gc, err := LoadGlobalConfig(globalDir)
	require.Error(t, err)
	assert.Nil(t, gc)
	assert.Contains(t, err.Error(), settingsPath)
}

// TestLoadGlobalConfig_ServerBearingYAMLDecodeErrorsNeverEchoValues verifies
// that YAML tag/key/anchor errors outside V1ServerConfig (such as bad explicit
// tags, non-scalar map keys, duplicate keys, or unknown anchors in non-server
// sections of a server-bearing settings.yaml) fail closed without echoing raw
// scalar values (ptone/scion#6187).
func TestLoadGlobalConfig_ServerBearingYAMLDecodeErrorsNeverEchoValues(t *testing.T) {
	tests := []struct {
		name           string
		settingsYAML   string
		forbidInErrMsg string
	}{
		{
			name: "mismatched explicit YAML tag in non-server section",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
profiles:
  local:
    env:
      API_KEY: !!int "sk-ant-super-secret-key"
`,
			forbidInErrMsg: "sk-ant-super-secret-key",
		},
		{
			name: "non-scalar map key in non-server section",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
profiles:
  local:
    env:
      ["sk-ant-super-secret-map-key"]: "present"
`,
			forbidInErrMsg: "sk-ant-super-secret-map-key",
		},
		{
			name: "duplicate mapping key in non-server section",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
profiles:
  local:
    env:
      "sk-ant-super-secret-dup": "one"
      "sk-ant-super-secret-dup": "two"
`,
			forbidInErrMsg: "sk-ant-super-secret-dup",
		},
		{
			name: "unknown alias from unquoted star scalar in server section",
			settingsYAML: `schema_version: "1"
server:
  auth:
    dev_token: *super-secret-star-token
`,
			forbidInErrMsg: "super-secret-star-token",
		},
		{
			name: "mismatched explicit YAML tag on top-level mapping key",
			settingsYAML: `schema_version: "1"
server:
  mode: hosted
!!int "sk-ant-super-secret-top-key": true
`,
			forbidInErrMsg: "sk-ant-super-secret-top-key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			settingsPath := filepath.Join(globalDir, "settings.yaml")
			require.NoError(t, os.WriteFile(settingsPath, []byte(tc.settingsYAML), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

			gc, err := LoadGlobalConfig(globalDir)
			require.Error(t, err, "LoadGlobalConfig must fail closed")
			assert.Nil(t, gc)
			assert.Contains(t, err.Error(), settingsPath)
			assert.NotContains(t, err.Error(), tc.forbidInErrMsg, "error must never echo raw scalar values")
		})
	}
}

// TestLoadGlobalConfig_NullServerKeyStillFallsBackToServerYAML preserves the
// behavior that an empty/null "server:" key (as opposed to a mistyped value)
// means no server section is configured in settings.yaml.
func TestLoadGlobalConfig_NullServerKeyStillFallsBackToServerYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("schema_version: \"1\"\nserver: null\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, 7777, gc.Hub.Port)
}

// TestLoadGlobalConfig_ServerlessMalformedSettingsAllowsFallback preserves
// LoadGlobalConfig when settings.yaml has no "server:" section (so `scion
// server preflight` can still open the DB and report non-server profile syntax
// errors cleanly).
func TestLoadGlobalConfig_ServerlessMalformedSettingsAllowsFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("schema_version: \"1\"\nprofiles:\n  a: [unclosed\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)
	assert.Equal(t, 7777, gc.Hub.Port)
}
