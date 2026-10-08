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
	"time"
)

// TestLoadSeedEnvKoanf verifies that LoadSeedEnvKoanf loads SCION_SEED_*
// environment variables and maps them to snake_case koanf keys matching
// the opsettings registry (not camelCase like LoadEnvKoanf).
func TestLoadSeedEnvKoanf(t *testing.T) {
	// SCION_SEED_SERVER_HUB_ADMINEMAILS → strip prefix → SERVER_HUB_ADMINEMAILS
	// → envKeyToOpsettingsKey → server.hub.admin_emails (snake_case)
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "seed@example.com")
	t.Setenv("SCION_SEED_SERVER_AUTH_USERACCESSMODE", "invite")
	t.Setenv("SCION_SEED_SERVER_HUB_PORT", "9999")

	k := LoadSeedEnvKoanf()

	if v := k.String("server.hub.admin_emails"); v != "seed@example.com" {
		t.Errorf("expected server.hub.admin_emails = 'seed@example.com', got %q", v)
	}
	if v := k.String("server.auth.user_access_mode"); v != "invite" {
		t.Errorf("expected server.auth.user_access_mode = 'invite', got %q", v)
	}
	if v := k.Int("server.hub.port"); v != 9999 {
		t.Errorf("expected server.hub.port = 9999, got %d", v)
	}
}

// TestLoadEnvKoanf_OpsettingsKeyspace verifies that LoadEnvKoanf maps
// SCION_SERVER_* env vars to the opsettings registry keyspace (snake_case
// with server.* prefix for server sub-keys).
func TestLoadEnvKoanf_OpsettingsKeyspace(t *testing.T) {
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "admin@test.com")
	t.Setenv("SCION_SERVER_AUTH_USERACCESSMODE", "open")

	k := LoadEnvKoanf()

	// Should produce server.hub.admin_emails (not hub.adminEmails).
	if !k.Exists("server.hub.admin_emails") {
		t.Errorf("SCION_SERVER_HUB_ADMINEMAILS should map to server.hub.admin_emails; keys: %v", k.Keys())
	}
	if !k.Exists("server.auth.user_access_mode") {
		t.Errorf("SCION_SERVER_AUTH_USERACCESSMODE should map to server.auth.user_access_mode; keys: %v", k.Keys())
	}

	// camelCase key should NOT exist.
	if k.Exists("hub.adminEmails") {
		t.Error("camelCase key hub.adminEmails should not exist")
	}
}

// TestLoadEnvKoanf_NonServerKeys verifies that non-server keys (telemetry,
// default_*) do not get the server.* prefix.
func TestLoadEnvKoanf_NonServerKeys(t *testing.T) {
	t.Setenv("SCION_SERVER_TELEMETRY_ENABLED", "true")
	t.Setenv("SCION_SERVER_DEFAULTTEMPLATE", "my-template")

	k := LoadEnvKoanf()

	if !k.Exists("telemetry.enabled") {
		t.Errorf("SCION_SERVER_TELEMETRY_ENABLED should map to telemetry.enabled; keys: %v", k.Keys())
	}
	if k.Exists("server.telemetry.enabled") {
		t.Error("telemetry.enabled should NOT have server. prefix")
	}
	if !k.Exists("default_template") {
		t.Errorf("SCION_SERVER_DEFAULTTEMPLATE should map to default_template; keys: %v", k.Keys())
	}
}

// TestServerEnvToOpsettingsKey verifies the mapper that re-adds "server."
// prefix for keys belonging to V1ServerConfig.
func TestServerEnvToOpsettingsKey(t *testing.T) {
	tests := []struct {
		envKey string
		want   string
	}{
		// Server sub-keys get server.* prefix.
		{"HUB_ADMINEMAILS", "server.hub.admin_emails"},
		{"HUB_PORT", "server.hub.port"},
		{"AUTH_USERACCESSMODE", "server.auth.user_access_mode"},
		{"DATABASE_DRIVER", "server.database.driver"},
		{"GITHUBAPP_APPID", "server.github_app.app_id"},
		{"LOGLEVEL", "server.log_level"},
		{"LOGFORMAT", "server.log_format"},
		{"NOTIFICATIONCHANNELS", "server.notification_channels"},
		{"STORAGE_PROVIDER", "server.storage.provider"},
		{"SECRETS_BACKEND", "server.secrets.backend"},
		{"OAUTH_CLI_GOOGLE_CLIENTID", "server.oauth.cli.google.clientid"},
		// Non-server keys pass through unchanged.
		{"TELEMETRY_ENABLED", "telemetry.enabled"},
		{"DEFAULTTEMPLATE", "default_template"},
		{"DEFAULTMAXTURNS", "default_max_turns"},
		{"IMAGEREGISTRY", "image_registry"},
		// ptone/scion#3836: federation is a server sub-key, and the
		// project_defaults / harness_configs segments map to snake_case.
		{"FEDERATION_ENABLED", "server.federation.enabled"},
		{"FEDERATION_TRUSTEDISSUERS", "server.federation.trusted_issuers"},
		{"FEDERATION_ALGORITHMS", "server.federation.algorithms"},
		{"FEDERATION_REFRESHINTERVAL", "server.federation.refresh_interval"},
		{"FEDERATION_DEBOUNCEINTERVAL", "server.federation.debounce_interval"},
		{"PROJECTDEFAULTS_DEFAULTSCRATCHPAD", "project_defaults.default_scratchpad"},
		{"HARNESSCONFIGS", "harness_configs"},
		{"HARNESSCONFIGS_CLAUDE_IMAGE", "harness_configs.claude.image"},
	}
	for _, tt := range tests {
		t.Run(tt.envKey, func(t *testing.T) {
			got := serverEnvToOpsettingsKey(tt.envKey)
			if got != tt.want {
				t.Errorf("serverEnvToOpsettingsKey(%q) = %q, want %q", tt.envKey, got, tt.want)
			}
		})
	}
}

// TestSeedEnvToOpsettingsKey_Issue3836 verifies the SCION_SEED_* spellings
// of the federation, project_defaults and harness_configs keys
// (ptone/scion#3836), which go through envKeyToOpsettingsKey with an explicit
// SERVER_ segment for server keys.
func TestSeedEnvToOpsettingsKey_Issue3836(t *testing.T) {
	for envKey, want := range map[string]string{
		"SERVER_FEDERATION_ENABLED":         "server.federation.enabled",
		"SERVER_FEDERATION_TRUSTEDISSUERS":  "server.federation.trusted_issuers",
		"SERVER_FEDERATION_REFRESHINTERVAL": "server.federation.refresh_interval",
		"PROJECTDEFAULTS_DEFAULTSCRATCHPAD": "project_defaults.default_scratchpad",
		"HARNESSCONFIGS_CLAUDE_IMAGE":       "harness_configs.claude.image",
		// Unchanged existing mappings.
		"SERVER_HUB_ADMINEMAILS":     "server.hub.admin_emails",
		"AUTOEXPOSEPORTS_ENABLED":    "auto_expose_ports.enabled",
		"SERVER_HUB_RECONNECTWINDOW": "server.hub.reconnect_window",
	} {
		if got := envKeyToOpsettingsKey(envKey); got != want {
			t.Errorf("envKeyToOpsettingsKey(%q) = %q, want %q", envKey, got, want)
		}
	}
}

// TestSeedAndServerEnv_AutoExposePorts verifies that the flat-lowercased
// AUTOEXPOSEPORTS segment maps to the Layer-1 key auto_expose_ports.enabled
// for both SCION_SEED_* and SCION_SERVER_*, and that the seed value reaches
// bootstrap material (ptone/scion#3052).
func TestSeedAndServerEnv_AutoExposePorts(t *testing.T) {
	t.Setenv("SCION_SEED_AUTOEXPOSEPORTS_ENABLED", "true")
	if k := LoadSeedEnvKoanf(); !k.Exists("auto_expose_ports.enabled") || !k.Bool("auto_expose_ports.enabled") {
		t.Errorf("SCION_SEED_AUTOEXPOSEPORTS_ENABLED should map to auto_expose_ports.enabled=true; keys: %v", k.Keys())
	}

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	if k := LoadBootstrapKoanf(); !k.Bool("auto_expose_ports.enabled") {
		t.Errorf("bootstrap material should carry auto_expose_ports.enabled=true from SCION_SEED_*; keys: %v", k.Keys())
	}

	if got := serverEnvToOpsettingsKey("AUTOEXPOSEPORTS_ENABLED"); got != "auto_expose_ports.enabled" {
		t.Errorf("serverEnvToOpsettingsKey(AUTOEXPOSEPORTS_ENABLED) = %q, want auto_expose_ports.enabled", got)
	}
}

// TestLoadSeedEnvKoanf_Empty verifies that LoadSeedEnvKoanf returns an empty
// koanf instance when no SCION_SEED_* vars are set.
func TestLoadSeedEnvKoanf_Empty(t *testing.T) {
	for _, e := range os.Environ() {
		if len(e) > 11 && e[:11] == "SCION_SEED_" {
			key := e[:indexOf(e, '=')]
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}

	k := LoadSeedEnvKoanf()
	if len(k.Keys()) != 0 {
		t.Errorf("expected no keys, got %v", k.Keys())
	}
}

// TestLoadBootstrapKoanf_MergeOrder verifies the full merge order:
//
//	coded defaults → SCION_SEED_* → settings.yaml → SCION_SERVER_*
//
// All layers produce snake_case koanf keys in the opsettings keyspace.
func TestLoadBootstrapKoanf_MergeOrder(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Use server.hub.port — both SEED env and yaml map to "server.hub.port".
	settingsContent := `schema_version: "1"
server:
  hub:
    port: 8080
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("write settings.yaml: %v", err)
	}

	// Case 1: SEED + yaml → yaml wins (loaded after SEED).
	t.Setenv("SCION_SEED_SERVER_HUB_PORT", "1111")

	k := LoadBootstrapKoanf()
	if v := k.Int("server.hub.port"); v != 8080 {
		t.Errorf("yaml should override SEED: expected 8080, got %d", v)
	}

	// Case 2: Remove yaml → SEED wins.
	_ = os.Remove(filepath.Join(scionDir, "settings.yaml"))
	k2 := LoadBootstrapKoanf()
	if v := k2.Int("server.hub.port"); v != 1111 {
		t.Errorf("without yaml, SEED should provide value: expected 1111, got %d", v)
	}

	// Case 3: SERVER env overrides SEED — both map to server.hub.port.
	t.Setenv("SCION_SERVER_HUB_PORT", "3333")
	k3 := LoadBootstrapKoanf()
	if v := k3.Int("server.hub.port"); v != 3333 {
		t.Errorf("SERVER env should override SEED at server.hub.port: expected 3333, got %d", v)
	}
}

// TestLoadBootstrapKoanf_ServerOverridesYaml verifies that SCION_SERVER_*
// overrides yaml when both target the same koanf key. SCION_SERVER_HUB_PORT
// maps directly to server.hub.port (no need for double-SERVER).
func TestLoadBootstrapKoanf_ServerOverridesYaml(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	settingsContent := `schema_version: "1"
server:
  hub:
    port: 8080
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("write settings.yaml: %v", err)
	}

	t.Setenv("SCION_SERVER_HUB_PORT", "5555")

	k := LoadBootstrapKoanf()
	if v := k.Int("server.hub.port"); v != 5555 {
		t.Errorf("SERVER env should override yaml at server.hub.port: expected 5555, got %d", v)
	}
}

// TestLoadBootstrapKoanf_SeedBelowYaml verifies that yaml values override
// SCION_SEED_* values when both target the same koanf key.
func TestLoadBootstrapKoanf_SeedBelowYaml(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	settingsContent := `schema_version: "1"
server:
  hub:
    port: 2222
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("write settings.yaml: %v", err)
	}

	t.Setenv("SCION_SEED_SERVER_HUB_PORT", "1111")

	k := LoadBootstrapKoanf()

	if v := k.Int("server.hub.port"); v != 2222 {
		t.Errorf("yaml should override SEED: expected 2222, got %d", v)
	}
}

// TestLoadBootstrapKoanf_CompoundWordKey verifies that compound-word fields
// (e.g. admin_emails) from SEED env, yaml, and SERVER env all merge into the
// same snake_case koanf key, proving that ExtractSectionFromKoanf will find
// values from any layer.
func TestLoadBootstrapKoanf_CompoundWordKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Case 1: SEED env sets admin_emails, yaml overrides it.
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "seed@example.com")
	settingsContent := `schema_version: "1"
server:
  hub:
    admin_emails:
      - "yaml@example.com"
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("write settings.yaml: %v", err)
	}

	k := LoadBootstrapKoanf()

	// yaml wins over SEED — both target server.hub.admin_emails (snake_case).
	emails := k.Strings("server.hub.admin_emails")
	if len(emails) != 1 || emails[0] != "yaml@example.com" {
		t.Errorf("yaml should override SEED for admin_emails: expected [yaml@example.com], got %v", emails)
	}

	// Case 2: SERVER env overrides yaml for same compound-word key.
	// SCION_SERVER_HUB_ADMINEMAILS maps directly to server.hub.admin_emails.
	// After splitCommaSeparatedKoanfKeys, even a single value is wrapped as a slice.
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "server@example.com")
	k2 := LoadBootstrapKoanf()

	serverVal := k2.Get("server.hub.admin_emails")
	serverSlice, ok := serverVal.([]interface{})
	if !ok {
		t.Fatalf("SERVER env admin_emails should be []interface{}, got %T: %v", serverVal, serverVal)
	}
	if len(serverSlice) != 1 || serverSlice[0] != "server@example.com" {
		t.Errorf("SERVER env should override yaml for admin_emails: expected [server@example.com], got %v", serverSlice)
	}

	// Case 3: Verify the camelCase key does NOT exist (proving no namespace split).
	if k2.Exists("server.hub.adminEmails") {
		t.Error("camelCase key server.hub.adminEmails should not exist — bootstrap uses snake_case")
	}
}

// TestLoadBootstrapKoanf_NonServerEnv verifies that SCION_SERVER_* env vars
// for non-server keys (telemetry, defaults) map correctly without server.* prefix.
func TestLoadBootstrapKoanf_NonServerEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	t.Setenv("SCION_SERVER_TELEMETRY_ENABLED", "true")
	t.Setenv("SCION_SERVER_DEFAULTTEMPLATE", "my-template")

	k := LoadBootstrapKoanf()

	if !k.Exists("telemetry.enabled") {
		t.Errorf("expected telemetry.enabled to exist; keys: %v", k.Keys())
	}
	if k.Exists("server.telemetry.enabled") {
		t.Error("telemetry.enabled should NOT have server. prefix")
	}
	if !k.Exists("default_template") {
		t.Errorf("expected default_template to exist; keys: %v", k.Keys())
	}
}

// TestLoadBootstrapKoanf_CommaSplit verifies that comma-separated list values
// from env vars are split into slices.
func TestLoadBootstrapKoanf_CommaSplit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "a@test.com,b@test.com,c@test.com")

	k := LoadBootstrapKoanf()

	val := k.Get("server.hub.admin_emails")
	slice, ok := val.([]interface{})
	if !ok {
		t.Fatalf("expected server.hub.admin_emails to be a slice, got %T: %v", val, val)
	}
	if len(slice) != 3 {
		t.Errorf("expected 3 elements, got %d: %v", len(slice), slice)
	}
}

// TestLoadBootstrapKoanf_SingleValueListEnv verifies that a single-value
// (no comma) env var for a known list field produces an array, not a string.
func TestLoadBootstrapKoanf_SingleValueListEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "single@example.com")

	k := LoadBootstrapKoanf()

	val := k.Get("server.hub.admin_emails")
	slice, ok := val.([]interface{})
	if !ok {
		t.Fatalf("expected server.hub.admin_emails to be []interface{}, got %T: %v", val, val)
	}
	if len(slice) != 1 {
		t.Errorf("expected 1 element, got %d: %v", len(slice), slice)
	}
	if len(slice) > 0 {
		if s, ok := slice[0].(string); !ok || s != "single@example.com" {
			t.Errorf("expected slice[0] = 'single@example.com', got %v", slice[0])
		}
	}
}

// TestLoadBootstrapKoanf_EmbeddedAgentDefaults_AppliesOnUnseededInstance is
// the regression test for ptone/scion#1306: on an un-seeded instance (no
// settings.yaml on disk, no SCION_SEED_*/SCION_SERVER_* env vars),
// LoadBootstrapKoanf must still produce default_template/default_harness_config
// from the embedded settings file, rather than leaving them absent. Before this
// fix, the coded-defaults layer never included these two keys at all, so an
// agent-create request that omitted harnessConfig resolved to an empty name
// and the broker returned a 502 for "harness-config \"\" not found" (or, once
// the value was seeded through some other path, the same failure for whatever
// name ended up there) instead of picking up the product default.
func TestLoadBootstrapKoanf_EmbeddedAgentDefaults_AppliesOnUnseededInstance(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	// Deliberately do NOT create .scion/ or a settings.yaml — this is the
	// un-seeded, first-boot state.

	wantTemplate, wantHarnessConfig := embeddedAgentDefaultsForTest(t)
	if wantHarnessConfig == "" {
		t.Fatal("test setup: embedded default_harness_config is empty; embeds/default_settings.yaml may have changed shape")
	}

	k := LoadBootstrapKoanf()

	if v := k.String("default_template"); v != wantTemplate {
		t.Errorf("expected default_template = %q (from embedded defaults), got %q", wantTemplate, v)
	}
	if v := k.String("default_harness_config"); v != wantHarnessConfig {
		t.Errorf("expected default_harness_config = %q (from embedded defaults), got %q", wantHarnessConfig, v)
	}
}

// TestLoadBootstrapKoanf_EmbeddedAgentDefaults_YamlOverrides verifies that the
// embedded agent-defaults layer sits at the bottom of the precedence chain:
// a value in settings.yaml must still win, exactly like every other coded
// default in LoadBootstrapKoanf.
func TestLoadBootstrapKoanf_EmbeddedAgentDefaults_YamlOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	settingsContent := `schema_version: "1"
default_harness_config: my-custom-hc
default_template: my-custom-template
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("write settings.yaml: %v", err)
	}

	k := LoadBootstrapKoanf()

	if v := k.String("default_harness_config"); v != "my-custom-hc" {
		t.Errorf("settings.yaml should override the embedded default: expected %q, got %q", "my-custom-hc", v)
	}
	if v := k.String("default_template"); v != "my-custom-template" {
		t.Errorf("settings.yaml should override the embedded default: expected %q, got %q", "my-custom-template", v)
	}
}

// TestLoadBootstrapKoanf_EmbeddedAgentDefaults_SeedEnvOverrides verifies that
// SCION_SEED_DEFAULTHARNESSCONFIG — the operator-facing seed knob — overrides
// the embedded default, consistent with every other coded default.
func TestLoadBootstrapKoanf_EmbeddedAgentDefaults_SeedEnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	t.Setenv("SCION_SEED_DEFAULTHARNESSCONFIG", "seed-hc")

	k := LoadBootstrapKoanf()

	if v := k.String("default_harness_config"); v != "seed-hc" {
		t.Errorf("SCION_SEED_DEFAULTHARNESSCONFIG should override the embedded default: expected %q, got %q", "seed-hc", v)
	}
}

// embeddedAgentDefaultsForTest reads the same embedded settings file
// LoadBootstrapKoanf uses and returns its default_template/default_harness_config
// values, so tests assert against the real embedded content instead of a
// hardcoded literal that would drift silently if the embed changed.
func embeddedAgentDefaultsForTest(t *testing.T) (template, harnessConfig string) {
	t.Helper()
	m := embeddedAgentDefaultsKoanfMap()
	template, _ = m["default_template"].(string)
	harnessConfig, _ = m["default_harness_config"].(string)
	return template, harnessConfig
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// writeConfigPathFixture writes ~/.scion/settings.yaml (global) and
// <tmp>/cfg/settings.yaml (the --config directory) and returns both dirs.
func writeConfigPathFixture(t *testing.T, global, cfg string) (scionDir, cfgDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir = filepath.Join(tmpDir, ".scion")
	cfgDir = filepath.Join(tmpDir, "cfg")
	for dir, content := range map[string]string{scionDir: global, cfgDir: cfg} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return scionDir, cfgDir
}

// assertStalledThresholdAgrees checks that LoadGlobalConfig (startup and the
// file-mode reload) and LoadBootstrapKoanfWithConfigPath (DB-tier bootstrap)
// resolve the Layer-1 key server.hub.stalled_threshold to the same value.
func assertStalledThresholdAgrees(t *testing.T, configPath, want string) {
	t.Helper()
	gc, err := LoadGlobalConfig(configPath)
	if err != nil {
		t.Fatalf("LoadGlobalConfig(%q): %v", configPath, err)
	}
	if got := gc.Hub.StalledThreshold.String(); got != want {
		t.Errorf("LoadGlobalConfig(%q): stalled_threshold = %s, want %s", configPath, got, want)
	}
	k := LoadBootstrapKoanfWithConfigPath(configPath)
	d, err := time.ParseDuration(k.String("server.hub.stalled_threshold"))
	if err != nil || d.String() != want {
		t.Errorf("LoadBootstrapKoanfWithConfigPath(%q): stalled_threshold = %q, want %s",
			configPath, k.String("server.hub.stalled_threshold"), want)
	}
}

// When the global settings.yaml has a server key, LoadGlobalConfig ignores
// the --config settings.yaml, so bootstrap must too (ptone/scion#3070).
func TestLoadBootstrapKoanfWithConfigPath_GlobalServerKeyWins(t *testing.T) {
	_, cfgDir := writeConfigPathFixture(t,
		"schema_version: \"1\"\nimage_registry: global.example.com\nserver:\n  hub:\n    stalled_threshold: 5m\n",
		"schema_version: \"1\"\nimage_registry: cfg.example.com\nserver:\n  hub:\n    stalled_threshold: 9m\n")

	for _, path := range []string{cfgDir, filepath.Join(cfgDir, "settings.yaml")} {
		assertStalledThresholdAgrees(t, path, "5m0s")
		if got := LoadBootstrapKoanfWithConfigPath(path).String("image_registry"); got != "global.example.com" {
			t.Errorf("%s: image_registry = %q, want global.example.com", path, got)
		}
	}
}

// When only the --config settings.yaml has a server key, LoadGlobalConfig
// reads it, so bootstrap layers it over the global file; SCION_SERVER_*
// stays on top (ptone/scion#3070).
func TestLoadBootstrapKoanfWithConfigPath_ConfigServerKeyUsedWhenGlobalHasNone(t *testing.T) {
	scionDir, cfgDir := writeConfigPathFixture(t,
		"schema_version: \"1\"\nimage_registry: global.example.com\n",
		"schema_version: \"1\"\nimage_registry: cfg.example.com\nserver:\n  hub:\n    stalled_threshold: 9m\n    public_url: https://cfg.example.com\n")
	t.Setenv("SCION_SERVER_HUB_PUBLICURL", "https://env.example.com")

	for _, path := range []string{cfgDir, filepath.Join(cfgDir, "settings.yaml")} {
		assertStalledThresholdAgrees(t, path, "9m0s")
		k := LoadBootstrapKoanfWithConfigPath(path)
		if got := k.String("image_registry"); got != "cfg.example.com" {
			t.Errorf("%s: image_registry = %q, want cfg.example.com from the --config file", path, got)
		}
		if got := k.String("server.hub.public_url"); got != "https://env.example.com" {
			t.Errorf("%s: public_url = %q, want SCION_SERVER_* on top", path, got)
		}
	}

	// No --config path, or one naming the global directory: global only.
	for _, path := range []string{"", scionDir} {
		if got := LoadBootstrapKoanfWithConfigPath(path).String("image_registry"); got != "global.example.com" {
			t.Errorf("%q: image_registry = %q, want global.example.com", path, got)
		}
	}
}

// TestLoadBootstrapKoanfWithConfigPath_LegacyFile verifies that a --config
// path naming a non-settings file is read as-is, as the legacy server
// config loader reads it.
func TestLoadBootstrapKoanfWithConfigPath_LegacyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(tmpDir, "hub-config.yaml")
	if err := os.WriteFile(cfgFile, []byte("image_registry: file.example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := LoadBootstrapKoanfWithConfigPath(cfgFile).String("image_registry"); got != "file.example.com" {
		t.Errorf("image_registry = %q, want file.example.com", got)
	}
}

// When the --config settings.yaml wins, LoadGlobalConfig takes the keys it
// models (here quotas and default_timezone) only from that file, so a value
// set only in the global settings.yaml must not reach bootstrap either. Keys
// LoadGlobalConfig does not model (here image_registry) still come from the
// global file, as the file-mode settings load reads them (ptone/scion#3070).
func TestLoadBootstrapKoanfWithConfigPath_ConfigWinsDropsGlobalModelledKeys(t *testing.T) {
	_, cfgDir := writeConfigPathFixture(t,
		"schema_version: \"1\"\nimage_registry: global.example.com\ndefault_timezone: Europe/Paris\nquotas:\n  enforce_broker_quotas: false\n",
		"schema_version: \"1\"\nserver:\n  hub:\n    stalled_threshold: 9m\n")

	gc, err := LoadGlobalConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	k := LoadBootstrapKoanfWithConfigPath(cfgDir)

	if gc.EnforceBrokerQuotas != nil {
		t.Errorf("LoadGlobalConfig: quotas.enforce_broker_quotas = %v, want unset", *gc.EnforceBrokerQuotas)
	}
	if k.Exists("quotas.enforce_broker_quotas") {
		t.Errorf("bootstrap: quotas.enforce_broker_quotas = %v, want unset like LoadGlobalConfig", k.Get("quotas.enforce_broker_quotas"))
	}
	if gc.DefaultTimezone != "" || k.String("default_timezone") != "" {
		t.Errorf("default_timezone: LoadGlobalConfig %q, bootstrap %q; want both unset", gc.DefaultTimezone, k.String("default_timezone"))
	}

	t.Chdir(filepath.Dir(cfgDir)) // no project settings from the working directory
	vs, _, err := LoadEffectiveSettings("")
	if err != nil {
		t.Fatal(err)
	}
	if got := k.String("image_registry"); got != "global.example.com" || vs.ImageRegistry != got {
		t.Errorf("image_registry: bootstrap %q, effective settings %q; want both global.example.com", got, vs.ImageRegistry)
	}
	assertStalledThresholdAgrees(t, cfgDir, "9m0s")
}

// topLevelSettingsSectionKeys must cover every key applyTopLevelSettingsSections
// reads; it only sees listed keys, so a missing entry drops that setting.
func TestApplyTopLevelSettingsSections_ReadsListedKeys(t *testing.T) {
	raw := map[string]interface{}{
		"telemetry":                 map[string]interface{}{"enabled": true},
		"project_defaults":          map[string]interface{}{"default_scratchpad": false},
		"quotas":                    map[string]interface{}{"enforce_broker_quotas": false},
		"agent_secrets":             map[string]interface{}{"user_scope_only": true},
		"default_harness_config":    "hc",
		"default_timezone":          "Europe/Paris",
		"default_gcp_identity_mode": "block",
		"default_gcp_identity_service_account_id": "sa-1",
	}
	if len(raw) != len(topLevelSettingsSectionKeys) {
		t.Fatalf("fixture covers %d keys, list has %d", len(raw), len(topLevelSettingsSectionKeys))
	}
	gc := &GlobalConfig{}
	applyTopLevelSettingsSections(gc, raw)
	if gc.TelemetryEnabled == nil || gc.DefaultScratchpad == nil || gc.EnforceBrokerQuotas == nil ||
		gc.AgentSecretsUserScopeOnly == nil || gc.DefaultHarnessConfig != "hc" || gc.DefaultTimezone != "Europe/Paris" ||
		gc.DefaultGCPIdentityMode != "block" || gc.DefaultGCPIdentityServiceAccountID != "sa-1" {
		t.Errorf("applyTopLevelSettingsSections dropped a listed key: %+v", gc)
	}
}
