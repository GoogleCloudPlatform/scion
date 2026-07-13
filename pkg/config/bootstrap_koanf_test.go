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

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
