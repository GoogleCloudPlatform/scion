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
	"bytes"
	"log/slog"
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
        super-secret-webhook-key:
          - "https://hooks.example.internal/super-secret-hook"
`,
			wantField:      "server.notification_channels[0].params[entry 0]",
			forbidInErrMsg: "super-secret-",
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
			wantField:      "server.notification_channels[0].params[entry 0]",
			forbidInErrMsg: "super-secret-webhook",
		},
		{
			name: "duplicate mapping key inside server struct mapping",
			settingsYAML: `schema_version: "1"
server:
  auth:
    dev_token: "super-secret-token-one"
    dev_token: "super-secret-token-two"
`,
			wantField:      "server.auth: line 5: duplicate mapping key",
			forbidInErrMsg: "super-secret-token",
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

// TestLoadGlobalConfig_UnreadableSettingsYAMLFailsWithoutFallback verifies that
// when settings.yaml exists on disk and os.ReadFile fails with a non-ENOENT
// error (such as EISDIR), LoadGlobalConfig fails closed rather than silently
// falling back to server.yaml or --config.
func TestLoadGlobalConfig_UnreadableSettingsYAMLFailsWithoutFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.MkdirAll(settingsPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

	cfgDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "settings.yaml"), []byte("schema_version: \"1\"\nserver:\n  hub:\n    port: 6666\n"), 0o644))

	gc, err := LoadGlobalConfig(cfgDir)
	require.Error(t, err)
	assert.Nil(t, gc)
	assert.Contains(t, err.Error(), "reading "+settingsPath)
}

// TestLoadGlobalConfig_DuplicateAndAliasedServerKeys verifies that
// mappingHasServerOrMergeKey inspects every top-level entry and unwraps YAML
// aliases so a leading null "server:" placeholder cannot mask a later
// "server:" or "<<" block and an aliased "server" key/value is honored.
func TestLoadGlobalConfig_DuplicateAndAliasedServerKeys(t *testing.T) {
	const canary = "CANARY_DUP_ALIAS_6187"

	t.Run("leading null server followed by valid server block fails closed on duplicate key", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		settingsPath := filepath.Join(globalDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
server:
profiles: {}
server:
  mode: hosted
  auth:
    dev_token: "`+canary+`"
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		gc, err := LoadGlobalConfig(globalDir)
		require.Error(t, err, "leading null server: must not hide a second server: block")
		assert.Nil(t, gc)
		assert.Contains(t, err.Error(), settingsPath)
		assert.Contains(t, err.Error(), "duplicate mapping key")
		assert.NotContains(t, err.Error(), canary)
	})

	t.Run("leading null server followed by mistyped server block fails closed", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		settingsPath := filepath.Join(globalDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
server: null
server:
  hub:
    admin_emails: "`+canary+`"
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		gc, err := LoadGlobalConfig(globalDir)
		require.Error(t, err)
		assert.Nil(t, gc)
		assert.Contains(t, err.Error(), settingsPath)
		assert.Contains(t, err.Error(), "duplicate mapping key")
		assert.NotContains(t, err.Error(), canary)
	})

	t.Run("leading merge key followed by null server uses explicit null fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
_base: &base
  server:
    hub:
      port: 9123
server: null
<<: *base
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		// In YAML 1.2, explicit "server: null" overrides merged keys, unless
		// the merged map itself fails YAML decoding. Verify a mistyped merge
		// target after "server: null" still fails closed rather than being
		// skipped by an early return on "server: null".
		gc, err := LoadGlobalConfig(globalDir)
		require.NoError(t, err)
		assert.Equal(t, 7777, gc.Hub.Port)

		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
server: null
<<: "`+canary+`"
`), 0o644))
		gc, err = LoadGlobalConfig(globalDir)
		require.Error(t, err, "leading server: null must not skip later invalid << merge")
		assert.Nil(t, gc)
		assert.Contains(t, err.Error(), "map merge requires mapping")
		assert.NotContains(t, err.Error(), canary)
	})

	t.Run("aliased server key loads valid server config", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
_key: &srv_key server
*srv_key:
  mode: hosted
  hub:
    port: 9123
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		gc, err := LoadGlobalConfig(globalDir)
		require.NoError(t, err)
		assert.Equal(t, "hosted", gc.Mode)
		assert.Equal(t, 9123, gc.Hub.Port)
	})

	t.Run("aliased server key with mistyped field fails closed without fallback or echo", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		settingsPath := filepath.Join(globalDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
_key: &`+canary+` server
*`+canary+`:
  hub:
    port: "`+canary+`"
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		gc, err := LoadGlobalConfig(globalDir)
		require.Error(t, err)
		assert.Nil(t, gc)
		assert.Contains(t, err.Error(), settingsPath)
		assert.Contains(t, err.Error(), "server.hub.port")
		assert.NotContains(t, err.Error(), canary)
	})

	t.Run("aliased null server value falls back to server.yaml", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(`schema_version: "1"
_empty: &empty_srv null
server: *empty_srv
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		gc, err := LoadGlobalConfig(globalDir)
		require.NoError(t, err)
		assert.Equal(t, 7777, gc.Hub.Port)
	})

	t.Run("aliased null server key followed by explicit server block fails closed on duplicate key", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		settingsPath := filepath.Join(globalDir, "settings.yaml")
		require.NoError(t, os.WriteFile(settingsPath, []byte(`schema_version: "1"
_key: &`+canary+` server
*`+canary+`: null
server:
  mode: hosted
  auth:
    dev_token: "`+canary+`"
`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

		gc, err := LoadGlobalConfig(globalDir)
		require.Error(t, err)
		assert.Nil(t, gc)
		assert.Contains(t, err.Error(), settingsPath)
		assert.Contains(t, err.Error(), "duplicate mapping key")
		assert.NotContains(t, err.Error(), canary)
	})
}

// TestLoadGlobalConfig_CanaryNeverEchoedAcrossAllFailurePaths exercises every
// error and log formatting site in the server-settings load path with a canary
// string in scalar, map key, YAML tag, alias/anchor name, and path-component
// positions, asserting the canary never appears in the returned error or log.
func TestLoadGlobalConfig_CanaryNeverEchoedAcrossAllFailurePaths(t *testing.T) {
	const canary = "CANARY_SECRET_6187_DO_NOT_ECHO"

	cases := []struct {
		name         string
		settingsYAML string
		wantSubstr   string
	}{
		{
			name:         "syntax error unclosed flow sequence with canary scalar",
			settingsYAML: "server:\n  hub:\n    admin_emails: [" + canary + "\n",
			wantSubstr:   "line ",
		},
		{
			name:         "syntax error invalid escape in double-quoted canary scalar",
			settingsYAML: "server:\n  auth:\n    dev_token: \"\\z" + canary + "\"\n",
			wantSubstr:   "unknown escape character",
		},
		{
			name:         "syntax error tab indentation before canary key and value",
			settingsYAML: "server:\n\t" + canary + ": " + canary + "\n",
			wantSubstr:   "cannot start any token",
		},
		{
			name:         "parser unknown alias token in server scalar",
			settingsYAML: "server:\n  auth:\n    dev_token: *" + canary + "\n",
			wantSubstr:   "unknown anchor referenced",
		},
		{
			name:         "parser unknown alias token in non-server section",
			settingsYAML: "server:\n  mode: hosted\nprofiles:\n  local:\n    env:\n      KEY: *" + canary + "\n",
			wantSubstr:   "unknown anchor referenced",
		},
		{
			name:         "top-level duplicate key named canary",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\n" + canary + ": 1\n" + canary + ": 2\n",
			wantSubstr:   "duplicate mapping key",
		},
		{
			name:         "top-level non-scalar sequence key containing canary",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\n[\"" + canary + "\"]: true\n",
			wantSubstr:   "invalid non-scalar mapping key (sequence)",
		},
		{
			name:         "top-level mismatched explicit tag on canary key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\n!!int \"" + canary + "\": true\n",
			wantSubstr:   "cannot decode scalar value as integer",
		},
		{
			name:         "top-level invalid merge target scalar containing canary",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\n<<: \"" + canary + "\"\n",
			wantSubstr:   "map merge requires mapping",
		},
		{
			name:         "server root scalar with custom canary tag and canary value",
			settingsYAML: "schema_version: \"1\"\nserver: !" + canary + " \"" + canary + "\"\n",
			wantSubstr:   "server: cannot decode scalar into mapping",
		},
		{
			name:         "server struct field scalar mismatch with unknown sibling canary key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  hub:\n    " + canary + ": \"" + canary + "\"\n    port: \"" + canary + "\"\n",
			wantSubstr:   "server.hub.port: cannot decode string into integer",
		},
		{
			name:         "server struct field custom tag on canary scalar",
			settingsYAML: "schema_version: \"1\"\nserver:\n  hub:\n    port: !" + canary + " \"" + canary + "\"\n",
			wantSubstr:   "server.hub.port: cannot decode scalar into integer",
		},
		{
			name:         "server struct field invalid binary tag with canary",
			settingsYAML: "schema_version: \"1\"\nserver:\n  auth:\n    dev_token: !!binary \"" + canary + "***\"\n",
			wantSubstr:   "server.auth.dev_token: cannot decode binary into string",
		},
		{
			name:         "server path-valued field keyring_file with mismatched tag and canary path component",
			settingsYAML: "schema_version: \"1\"\nserver:\n  keyring_file: !!int \"/etc/scion/" + canary + "/keyring.json\"\n",
			wantSubstr:   "server.keyring_file: cannot decode integer into string",
		},
		{
			name:         "server path-valued field broker.state_dir with sequence containing canary path component",
			settingsYAML: "schema_version: \"1\"\nserver:\n  broker:\n    state_dir:\n      - \"/var/lib/scion/" + canary + "\"\n",
			wantSubstr:   "server.broker.state_dir: cannot decode sequence into string",
		},
		{
			name:         "server slice element mismatch with canary mapping key and value",
			settingsYAML: "schema_version: \"1\"\nserver:\n  hub:\n    admin_emails:\n      - \"ops@example.com\"\n      - " + canary + ": \"" + canary + "\"\n",
			wantSubstr:   "server.hub.admin_emails[1]: cannot decode mapping into string",
		},
		{
			name:         "server nested slice custom unmarshaler clone_depth with canary",
			settingsYAML: "schema_version: \"1\"\nserver:\n  broker:\n    instances:\n      - key: local\n        name: local\n        agent_defaults:\n          clone_depth:\n            - \"" + canary + "\"\n",
			wantSubstr:   "server.broker.instances[0].agent_defaults.clone_depth: cannot decode sequence into string",
		},
		{
			name:         "reflect.Map notification_channels.params canary key with sequence value",
			settingsYAML: "schema_version: \"1\"\nserver:\n  notification_channels:\n    - type: webhook\n      params:\n        " + canary + ":\n          - \"" + canary + "\"\n",
			wantSubstr:   "server.notification_channels[0].params[entry 0]: cannot decode sequence into string",
		},
		{
			name:         "reflect.Map notification_channels.params canary key with mismatched tag value",
			settingsYAML: "schema_version: \"1\"\nserver:\n  notification_channels:\n    - type: webhook\n      params:\n        " + canary + ": !!int \"" + canary + "\"\n",
			wantSubstr:   "server.notification_channels[0].params[entry 0]: cannot decode integer into string",
		},
		{
			name:         "reflect.Map plugins.broker canary key with mistyped struct child field",
			settingsYAML: "schema_version: \"1\"\nserver:\n  plugins:\n    broker:\n      " + canary + ":\n        self_managed: \"" + canary + "\"\n",
			wantSubstr:   "server.plugins.broker[entry 0].self_managed: cannot decode string into boolean",
		},
		{
			name:         "reflect.Map nested harness_overrides canary outer key and env canary inner key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  broker:\n    instances:\n      - key: local\n        name: local\n        agent_defaults:\n          harness_overrides:\n            " + canary + ":\n              env:\n                " + canary + ":\n                  nested: \"" + canary + "\"\n",
			wantSubstr:   "server.broker.instances[0].agent_defaults.harness_overrides[entry 0].env[entry 0]: cannot decode mapping into string",
		},
		{
			name:         "reflect.Map runtime_target.node_selector canary key with sequence value",
			settingsYAML: "schema_version: \"1\"\nserver:\n  broker:\n    instances:\n      - key: local\n        name: local\n        runtime_target:\n          node_selector:\n            " + canary + ":\n              - \"" + canary + "\"\n",
			wantSubstr:   "server.broker.instances[0].runtime_target.node_selector[entry 0]: cannot decode sequence into string",
		},
		{
			name:         "reflect.Map non-scalar canary map key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  notification_channels:\n    - type: webhook\n      params:\n        [\"" + canary + "\"]: \"" + canary + "\"\n",
			wantSubstr:   "server.notification_channels[0].params[entry 0]: cannot decode map key sequence into string",
		},
		{
			name:         "reflect.Map explicit tag mismatch on canary map key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  notification_channels:\n    - type: webhook\n      params:\n        !!int \"" + canary + "\": \"ok\"\n",
			wantSubstr:   "server.notification_channels[0].params[entry 0]: cannot decode map key integer into string",
		},
		{
			name:         "reflect.Map valid merge key followed by failing canary key",
			settingsYAML: "schema_version: \"1\"\n_defaults: &" + canary + "_def\n  ok: \"1\"\nserver:\n  notification_channels:\n    - type: webhook\n      params:\n        <<: *" + canary + "_def\n        " + canary + ":\n          - \"" + canary + "\"\n",
			wantSubstr:   "server.notification_channels[0].params[entry 1]: cannot decode sequence into string",
		},
		{
			name:         "reflect.Map duplicate canary map keys",
			settingsYAML: "schema_version: \"1\"\nserver:\n  notification_channels:\n    - type: webhook\n      params:\n        " + canary + ": \"one\"\n        " + canary + ": \"two\"\n",
			wantSubstr:   "server.notification_channels[0].params: line 7: duplicate mapping key",
		},
		{
			name:         "server struct mapping duplicate canary keys",
			settingsYAML: "schema_version: \"1\"\nserver:\n  auth:\n    " + canary + ": \"one\"\n    " + canary + ": \"two\"\n",
			wantSubstr:   "server.auth: line 5: duplicate mapping key",
		},
		{
			name:         "server struct mapping invalid merge target scalar with canary",
			settingsYAML: "schema_version: \"1\"\nserver:\n  auth:\n    <<: \"" + canary + "\"\n",
			wantSubstr:   "server.auth: line 4: map merge requires mapping",
		},
		{
			name:         "server struct mapping non-scalar canary key next to merge key (yaml.v3 panic recovery)",
			settingsYAML: "schema_version: \"1\"\n_base: &base\n  dev_mode: false\nserver:\n  auth:\n    <<: *base\n    [\"" + canary + "\"]: true\n",
			wantSubstr:   "server.auth: line 7: invalid non-scalar mapping key (sequence)",
		},
		{
			name:         "server struct merged anchor with mistyped field and canary anchor/scalar",
			settingsYAML: "schema_version: \"1\"\n_hub: &" + canary + "\n  admin_emails: \"" + canary + "\"\nserver:\n  hub:\n    <<: *" + canary + "\n",
			wantSubstr:   "server.hub.admin_emails: cannot decode string into list of strings",
		},
		{
			name:         "server cyclic self-alias anchored with canary name",
			settingsYAML: "schema_version: \"1\"\nserver:\n  hub:\n    admin_emails: &" + canary + "\n      - *" + canary + "\n",
			wantSubstr:   "server.hub.admin_emails",
		},
		{
			name:         "non-server explicit tag mismatch with canary key and value",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\nprofiles:\n  " + canary + ":\n    env:\n      " + canary + ": !!int \"" + canary + "\"\n",
			wantSubstr:   "cannot decode scalar value as integer",
		},
		{
			name:         "non-server duplicate canary key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\nprofiles:\n  local:\n    env:\n      " + canary + ": \"1\"\n      " + canary + ": \"2\"\n",
			wantSubstr:   "duplicate mapping key",
		},
		{
			name:         "non-server non-scalar canary key",
			settingsYAML: "schema_version: \"1\"\nserver:\n  mode: hosted\nprofiles:\n  local:\n    env:\n      [\"" + canary + "\"]: \"1\"\n",
			wantSubstr:   "invalid non-scalar mapping key (sequence)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			settingsPath := filepath.Join(globalDir, "settings.yaml")
			require.NoError(t, os.WriteFile(settingsPath, []byte(tc.settingsYAML), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte("hub:\n  port: 7777\n"), 0o644))

			var logBuf bytes.Buffer
			prevLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			t.Cleanup(func() { slog.SetDefault(prevLogger) })

			gc, err := LoadGlobalConfig(globalDir)
			require.Error(t, err, "LoadGlobalConfig must fail closed")
			assert.Nil(t, gc)
			assert.Contains(t, err.Error(), settingsPath)
			assert.Contains(t, err.Error(), tc.wantSubstr)
			assert.NotContains(t, err.Error(), canary, "error text must never contain canary")
			assert.NotContains(t, logBuf.String(), canary, "log output must never contain canary")

			_ = LoadBootstrapKoanfWithConfigPath(globalDir)
			_ = LoadFileOnlyKoanf()
			assert.NotContains(t, logBuf.String(), canary, "bootstrap/file-only koanf log output must never contain canary")
		})
	}

	t.Run("serverless settings.yaml decode errors never echo canary in bootstrap or file-only koanf logs", func(t *testing.T) {
		for _, body := range []string{
			"schema_version: \"1\"\nprofiles:\n  local:\n    env:\n      KEY: !!int \"" + canary + "\"\n",
			"schema_version: \"1\"\nprofiles:\n  local:\n    env:\n      [\"" + canary + "\"]: \"1\"\n",
			"schema_version: \"1\"\nprofiles:\n  local:\n    env:\n      " + canary + ": \"1\"\n      " + canary + ": \"2\"\n",
			"schema_version: \"1\"\nprofiles:\n  local:\n    env:\n      KEY: *" + canary + "\n",
		} {
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(body), 0o644))

			var logBuf bytes.Buffer
			prevLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))

			_ = LoadBootstrapKoanfWithConfigPath(globalDir)
			_ = LoadFileOnlyKoanf()
			slog.SetDefault(prevLogger)

			assert.Contains(t, logBuf.String(), "failed to load settings file")
			assert.NotContains(t, logBuf.String(), canary, "logged settings error must never contain canary")
		}
	})
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
