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

// TestLoadSeedEnvKoanf_PathMapping verifies that SCION_SEED_* uses the
// snake_case envKeyToOpsettingsKey mapper (matching the opsettings registry),
// while SCION_SERVER_* uses the camelCase envKeyToConfigKey mapper (matching
// GlobalConfig struct tags). The resulting koanf key paths differ in both
// prefix depth and casing convention.
//
// SCION_SEED_SERVER_HUB_ADMINEMAILS → server.hub.admin_emails (snake_case)
// SCION_SERVER_HUB_ADMINEMAILS      → hub.adminEmails          (camelCase)
func TestLoadSeedEnvKoanf_PathMapping(t *testing.T) {
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "seed-value")
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "server-value")

	seedK := LoadSeedEnvKoanf()
	envK := LoadEnvKoanf()

	if len(seedK.Keys()) == 0 {
		t.Fatal("LoadSeedEnvKoanf returned no keys")
	}
	if len(envK.Keys()) == 0 {
		t.Fatal("LoadEnvKoanf returned no keys")
	}

	// SEED maps to server.hub.admin_emails (snake_case, SERVER_ is a key segment).
	if !seedK.Exists("server.hub.admin_emails") {
		t.Errorf("SCION_SEED_ did not map to server.hub.admin_emails; keys: %v", seedK.Keys())
	}
	// SERVER maps to hub.adminEmails (camelCase, no server. prefix).
	if !envK.Exists("hub.adminEmails") {
		t.Errorf("SCION_SERVER_ did not map to hub.adminEmails; keys: %v", envK.Keys())
	}

	if seedK.String("server.hub.admin_emails") != "seed-value" {
		t.Errorf("SEED value mismatch: got %q", seedK.String("server.hub.admin_emails"))
	}
	if envK.String("hub.adminEmails") != "server-value" {
		t.Errorf("SERVER value mismatch: got %q", envK.String("hub.adminEmails"))
	}
}

// TestLoadSeedEnvKoanf_Empty verifies that LoadSeedEnvKoanf returns an empty
// koanf instance when no SCION_SEED_* vars are set.
func TestLoadSeedEnvKoanf_Empty(t *testing.T) {
	for _, e := range os.Environ() {
		if len(e) > 11 && e[:11] == "SCION_SEED_" {
			key := e[:indexOf(e, '=')]
			t.Setenv(key, "")
			os.Unsetenv(key)
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
// All layers produce snake_case koanf keys via envKeyToOpsettingsKey, matching
// the opsettings registry. SEED and yaml both map to "server.hub.port", so we
// can test their precedence directly.
//
// SERVER env keys (in bootstrap context) also use the snake_case mapper, but
// map to a different koanf namespace (no "server." prefix), so they coexist
// with yaml keys rather than overriding them directly.
func TestLoadBootstrapKoanf_MergeOrder(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Use server.hub.port — both SEED env and yaml map to "server.hub.port".
	// SEED: SCION_SEED_SERVER_HUB_PORT → server.hub.port
	// yaml: server.hub.port → server.hub.port (identical koanf key)
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
	os.Remove(filepath.Join(scionDir, "settings.yaml"))
	k2 := LoadBootstrapKoanf()
	if v := k2.Int("server.hub.port"); v != 1111 {
		t.Errorf("without yaml, SEED should provide value: expected 1111, got %d", v)
	}

	// Case 3: SERVER env is in a different namespace (hub.port vs server.hub.port).
	// Verify SERVER value exists at its own key path.
	t.Setenv("SCION_SERVER_HUB_PORT", "3333")
	k3 := LoadBootstrapKoanf()
	if v := k3.Int("hub.port"); v != 3333 {
		t.Errorf("SERVER env should set hub.port: expected 3333, got %d", v)
	}
	// SEED value should still be at its key.
	if v := k3.Int("server.hub.port"); v != 1111 {
		t.Errorf("SEED should still be at server.hub.port: expected 1111, got %d", v)
	}
}

// TestLoadBootstrapKoanf_ServerOverridesYaml_SameKey verifies that SCION_SERVER_*
// overrides yaml when both produce the same koanf key. To target the
// "server.hub.port" koanf key from SCION_SERVER_*, the env var must be
// SCION_SERVER_SERVER_HUB_PORT (the first SERVER is the env prefix, the
// second is the "server." key segment).
func TestLoadBootstrapKoanf_ServerOverridesYaml_SameKey(t *testing.T) {
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

	// SCION_SERVER_SERVER_HUB_PORT → strip SCION_SERVER_ → SERVER_HUB_PORT
	// → envKeyToOpsettingsKey → server.hub.port
	t.Setenv("SCION_SERVER_SERVER_HUB_PORT", "5555")

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

	// SEED env: SCION_SEED_SERVER_HUB_PORT → server.hub.port = 1111
	// yaml: server.hub.port = 2222
	// Expected: yaml wins (2222).
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

	// Verify SEED value is NOT present (yaml merged on top).
	// Actually, koanf.Merge replaces keys, so server.hub.port should be 2222.
}

// TestLoadBootstrapKoanf_CompoundWordKey verifies that compound-word fields
// (e.g. admin_emails) from SEED env, yaml, and SERVER env all merge into the
// same snake_case koanf key, proving that ExtractSectionFromKoanf will find
// values from any layer. This is the test that would have caught the original
// camelCase/snake_case mismatch (review finding #1, #5).
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
	// SCION_SERVER_SERVER_HUB_ADMINEMAILS → server.hub.admin_emails
	// Note: env provider loads as a string, not a slice, so use String() accessor.
	t.Setenv("SCION_SERVER_SERVER_HUB_ADMINEMAILS", "server@example.com")
	k2 := LoadBootstrapKoanf()

	serverVal := k2.String("server.hub.admin_emails")
	if serverVal != "server@example.com" {
		t.Errorf("SERVER env should override yaml for admin_emails: expected 'server@example.com', got %q", serverVal)
	}

	// Case 3: Verify the camelCase key does NOT exist (proving no namespace split).
	if k2.Exists("server.hub.adminEmails") {
		t.Error("camelCase key server.hub.adminEmails should not exist — bootstrap uses snake_case")
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
