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

package opsettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// --- Registry completeness tests ---

func TestRegistryHasAllSections(t *testing.T) {
	expected := []string{"access", "lifecycle", "maintenance", "telemetry",
		"agent_defaults", "endpoints", "github_app", "notifications"}
	for _, name := range expected {
		if SectionByName(name) == nil {
			t.Errorf("section %q not found in registry", name)
		}
	}
}

func TestRegistryNewProducesNonNil(t *testing.T) {
	for _, sec := range Registry {
		v := sec.New()
		if v == nil {
			t.Errorf("section %q: New() returned nil", sec.Name)
		}
	}
}

func TestSectionMarshalUnmarshal(t *testing.T) {
	for _, sec := range Registry {
		v := sec.New()
		data, err := json.Marshal(v)
		if err != nil {
			t.Errorf("section %q: marshal failed: %v", sec.Name, err)
			continue
		}
		v2 := sec.New()
		if err := json.Unmarshal(data, v2); err != nil {
			t.Errorf("section %q: unmarshal failed: %v", sec.Name, err)
		}
	}
}

func TestSectionHasKoanfPaths(t *testing.T) {
	for _, sec := range Registry {
		if sec.Name == "maintenance" {
			if len(sec.KoanfPaths) != 0 {
				t.Errorf("maintenance section should have empty KoanfPaths, got %v", sec.KoanfPaths)
			}
			continue
		}
		if len(sec.KoanfPaths) == 0 {
			t.Errorf("section %q has no koanf paths", sec.Name)
		}
	}
}

// --- Lookup tests ---

func TestSectionByNameUnknown(t *testing.T) {
	if s := SectionByName("nonexistent"); s != nil {
		t.Errorf("expected nil for unknown section, got %v", s)
	}
}

func TestOwningSection(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"server.hub.admin_emails", "access"},
		{"server.auth.user_access_mode", "access"},
		{"server.auth.authorized_domains", "access"},
		{"server.hub.auto_suspend_stalled", "lifecycle"},
		{"server.hub.soft_delete_retention", "lifecycle"},
		{"server.hub.soft_delete_retain_files", "lifecycle"},
		{"telemetry.enabled", "telemetry"},
		{"telemetry.cloud.endpoint", "telemetry"},
		{"telemetry.hub.enabled", "telemetry"},
		{"telemetry.filter.sampling.rates", "telemetry"},
		{"default_template", "agent_defaults"},
		{"default_max_turns", "agent_defaults"},
		{"default_resources", "agent_defaults"},
		{"server.hub.public_url", "endpoints"},
		{"image_registry", "endpoints"},
		{"server.github_app.app_id", "github_app"},
		{"server.github_app.webhooks_enabled", "github_app"},
		{"server.notification_channels", "notifications"},
	}
	for _, tt := range tests {
		got := OwningSection(tt.key)
		if got != tt.want {
			t.Errorf("OwningSection(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestMaintenanceHasNoOwnedKeys(t *testing.T) {
	keys := []string{
		"server.hub.admin_mode",
		"server.hub.maintenance_message",
	}
	for _, key := range keys {
		if sec := OwningSection(key); sec != "" {
			t.Errorf("maintenance has no KoanfPaths, but OwningSection(%q) returned %q", key, sec)
		}
	}
}

func TestLayer0KeyNotOwned(t *testing.T) {
	layer0Keys := []string{
		"server.database.driver",
		"server.database.url",
		"server.hub.port",
		"server.hub.host",
		"server.auth.mode",
		"server.auth.dev_token",
		"server.broker.enabled",
		"server.oauth.web.google.client_id",
		"server.storage.provider",
		"server.secrets.backend",
		"server.log_level",
		"server.log_format",
		"server.mode",
		"hub.endpoint",
		"schema_version",
		"active_profile",
	}
	for _, key := range layer0Keys {
		if sec := OwningSection(key); sec != "" {
			t.Errorf("expected Layer-0 key %q to be unowned, but got section %q", key, sec)
		}
	}
}

func TestIsLayer1Key(t *testing.T) {
	if !IsLayer1Key("server.hub.admin_emails") {
		t.Error("expected admin_emails to be Layer 1")
	}
	if IsLayer1Key("server.database.driver") {
		t.Error("expected database.driver to be Layer 0")
	}
}

// --- Schema validation tests ---

func TestValidateValidDoc(t *testing.T) {
	if schemaCompileErr != nil {
		t.Fatalf("schema compilation error: %v", schemaCompileErr)
	}

	tests := []struct {
		section string
		doc     string
	}{
		{"access", `{"admin_emails":["admin@example.com"],"user_access_mode":"open"}`},
		{"lifecycle", `{"auto_suspend_stalled":true,"soft_delete_retention":"72h"}`},
		{"maintenance", `{"admin_mode":true,"maintenance_message":"upgrading"}`},
		{"telemetry", `{"enabled":true}`},
		{"agent_defaults", `{"default_max_turns":100}`},
		{"endpoints", `{"public_url":"https://hub.example.com","image_registry":"gcr.io/my-project"}`},
		{"github_app", `{"app_id":12345,"webhooks_enabled":true}`},
		{"notifications", `{"notification_channels":[{"type":"slack"}]}`},
	}
	for _, tt := range tests {
		errs := Validate(tt.section, json.RawMessage(tt.doc))
		if len(errs) > 0 {
			t.Errorf("Validate(%q, %s) returned errors: %v", tt.section, tt.doc, errs)
		}
	}
}

func TestValidateInvalidDoc(t *testing.T) {
	tests := []struct {
		section string
		doc     string
		desc    string
	}{
		{"access", `{"admin_emails":"not-an-array"}`, "wrong type for admin_emails"},
		{"lifecycle", `{"auto_suspend_stalled":"yes"}`, "wrong type for boolean"},
		{"maintenance", `{"admin_mode":"yes"}`, "wrong type for boolean"},
		{"agent_defaults", `{"default_max_turns":"not-a-number"}`, "wrong type for int"},
		{"github_app", `{"app_id":"not-a-number"}`, "wrong type for int64"},
	}
	for _, tt := range tests {
		errs := Validate(tt.section, json.RawMessage(tt.doc))
		if len(errs) == 0 {
			t.Errorf("Validate(%q) for %s: expected errors, got none", tt.section, tt.desc)
		}
	}
}

func TestValidateUnknownSection(t *testing.T) {
	errs := Validate("nonexistent", json.RawMessage(`{}`))
	if len(errs) == 0 {
		t.Error("expected error for unknown section")
	}
}

func TestValidateInvalidJSON(t *testing.T) {
	errs := Validate("access", json.RawMessage(`{invalid`))
	if len(errs) == 0 {
		t.Error("expected error for invalid JSON")
	}
}

// --- Seeding converter tests ---

func TestExtractSectionFromKoanf(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":             []string{"admin@test.com"},
		"server.auth.user_access_mode":        "open",
		"server.auth.authorized_domains":      []string{"test.com"},
		"server.hub.auto_suspend_stalled":     true,
		"server.hub.soft_delete_retention":    "72h",
		"server.hub.soft_delete_retain_files": true,
		"server.hub.public_url":               "https://hub.test.com",
		"image_registry":                      "gcr.io/test",
		"default_template":                    "default",
		"default_max_turns":                   100,
		"default_max_model_calls":             200,
		"default_max_duration":                "1h",
		"default_harness_config":              "base",
		"telemetry.enabled":                   true,
		"telemetry.cloud.endpoint":            "https://otel.test.com",
		"server.github_app.app_id":            12345,
		"server.github_app.webhooks_enabled":  true,
		"server.github_app.api_base_url":      "https://api.github.com",
		"server.notification_channels": []interface{}{
			map[string]interface{}{"type": "slack", "params": map[string]interface{}{"url": "https://hooks.slack.com/test"}},
		},
		"server.database.driver": "postgres",
		"server.hub.port":        9810,
	}, "."), nil)
	if err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	tests := []struct {
		section string
		check   func(t *testing.T, doc map[string]interface{})
	}{
		{"access", func(t *testing.T, doc map[string]interface{}) {
			if doc["user_access_mode"] != "open" {
				t.Errorf("expected user_access_mode=open, got %v", doc["user_access_mode"])
			}
			if doc["admin_emails"] == nil {
				t.Error("expected admin_emails to be present")
			}
		}},
		{"lifecycle", func(t *testing.T, doc map[string]interface{}) {
			if doc["soft_delete_retention"] != "72h" {
				t.Errorf("expected soft_delete_retention=72h, got %v", doc["soft_delete_retention"])
			}
		}},
		{"maintenance", func(t *testing.T, doc map[string]interface{}) {
			if len(doc) != 0 {
				t.Errorf("maintenance section should produce empty doc (no koanf paths), got %v", doc)
			}
		}},
		{"endpoints", func(t *testing.T, doc map[string]interface{}) {
			if doc["public_url"] != "https://hub.test.com" {
				t.Errorf("expected public_url, got %v", doc["public_url"])
			}
			if doc["image_registry"] != "gcr.io/test" {
				t.Errorf("expected image_registry, got %v", doc["image_registry"])
			}
		}},
		{"agent_defaults", func(t *testing.T, doc map[string]interface{}) {
			if doc["default_template"] != "default" {
				t.Errorf("expected default_template=default, got %v", doc["default_template"])
			}
		}},
		{"telemetry", func(t *testing.T, doc map[string]interface{}) {
			if doc["enabled"] != true {
				t.Errorf("expected enabled=true, got %v", doc["enabled"])
			}
		}},
		{"github_app", func(t *testing.T, doc map[string]interface{}) {
			if doc["webhooks_enabled"] != true {
				t.Errorf("expected webhooks_enabled=true, got %v", doc["webhooks_enabled"])
			}
		}},
		{"notifications", func(t *testing.T, doc map[string]interface{}) {
			if doc["notification_channels"] == nil {
				t.Error("expected notification_channels")
			}
		}},
	}

	for _, tt := range tests {
		raw, err := ExtractSectionFromKoanf(k, tt.section)
		if err != nil {
			t.Errorf("ExtractSectionFromKoanf(%q): %v", tt.section, err)
			continue
		}
		var doc map[string]interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("unmarshal section %q doc: %v", tt.section, err)
			continue
		}
		tt.check(t, doc)
	}
}

func TestExtractUnknownSection(t *testing.T) {
	k := koanf.New(".")
	_, err := ExtractSectionFromKoanf(k, "nonexistent")
	if err == nil {
		t.Error("expected error for unknown section")
	}
}

// B2: github_app must not leak secret material
func TestGitHubAppExtractExcludesSecrets(t *testing.T) {
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.github_app.app_id":           42,
		"server.github_app.api_base_url":     "https://api.github.com",
		"server.github_app.webhooks_enabled": true,
		"server.github_app.installation_url": "https://github.com/apps/test",
		"server.github_app.private_key_path": "/etc/keys/gh.pem",
		"server.github_app.private_key":      "-----BEGIN RSA PRIVATE KEY-----\nSECRET",
		"server.github_app.webhook_secret":   "whsec_supersecret",
	}, "."), nil)

	raw, err := ExtractSectionFromKoanf(k, "github_app")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := doc["private_key"]; ok {
		t.Error("section doc must NOT contain private_key (secret material)")
	}
	if _, ok := doc["webhook_secret"]; ok {
		t.Error("section doc must NOT contain webhook_secret (secret material)")
	}

	if doc["app_id"] == nil {
		t.Error("expected app_id in section doc")
	}
	if doc["private_key_path"] == nil {
		t.Error("expected private_key_path in section doc (path only, not secret)")
	}
}

// --- Round-trip tests ---

func TestRoundTrip(t *testing.T) {
	k := koanf.New(".")
	original := map[string]interface{}{
		"server.hub.admin_emails":             []interface{}{"admin@test.com"},
		"server.auth.user_access_mode":        "invite",
		"server.auth.authorized_domains":      []interface{}{"example.com"},
		"server.hub.auto_suspend_stalled":     true,
		"server.hub.soft_delete_retention":    "48h",
		"server.hub.soft_delete_retain_files": false,
		"server.hub.public_url":               "https://hub.example.com",
		"image_registry":                      "gcr.io/project",
		"default_template":                    "tmpl",
		"default_harness_config":              "base",
		"default_max_turns":                   50,
		"default_max_model_calls":             100,
		"default_max_duration":                "30m",
		"default_resources": map[string]interface{}{
			"requests": map[string]interface{}{"cpu": "100m", "memory": "256Mi"},
			"limits":   map[string]interface{}{"cpu": "1", "memory": "1Gi"},
			"disk":     "10Gi",
		},
		"telemetry.enabled":                  true,
		"telemetry.cloud.endpoint":           "https://otel.example.com",
		"server.github_app.app_id":           99,
		"server.github_app.webhooks_enabled": true,
		"server.github_app.api_base_url":     "https://api.github.com",
		"server.github_app.installation_url": "https://github.com/apps/test",
		"server.github_app.private_key_path": "/etc/keys/gh.pem",
		"server.notification_channels": []interface{}{
			map[string]interface{}{"type": "slack"},
		},
		"server.database.driver": "postgres",
		"server.hub.port":        9810,
	}
	if err := k.Load(confmap.Provider(original, "."), nil); err != nil {
		t.Fatalf("load original: %v", err)
	}

	sections, err := ExtractAllSections(k)
	if err != nil {
		t.Fatalf("extract all sections: %v", err)
	}

	merged, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("load sections into koanf: %v", err)
	}

	checks := []struct {
		key  string
		want interface{}
	}{
		{"server.hub.admin_emails", nil},
		{"server.auth.user_access_mode", "invite"},
		{"server.hub.auto_suspend_stalled", true},
		{"server.hub.soft_delete_retention", "48h"},
		{"server.hub.public_url", "https://hub.example.com"},
		{"image_registry", "gcr.io/project"},
		{"default_template", "tmpl"},
		{"default_max_turns", nil},
		{"telemetry.enabled", true},
		{"telemetry.cloud.endpoint", "https://otel.example.com"},
		{"server.github_app.app_id", nil},
		{"server.github_app.webhooks_enabled", true},
	}

	for _, c := range checks {
		got := merged.Get(c.key)
		if c.want == nil {
			if got == nil {
				t.Errorf("round-trip: key %q missing in merged koanf", c.key)
			}
			continue
		}
		if got != c.want {
			t.Errorf("round-trip: key %q = %v (%T), want %v (%T)", c.key, got, got, c.want, c.want)
		}
	}

	if merged.Exists("server.database.driver") {
		t.Error("Layer-0 key server.database.driver should not appear in merged sections")
	}
	if merged.Exists("server.hub.port") {
		t.Error("Layer-0 key server.hub.port should not appear in merged sections")
	}
}

// N7: Verify default_resources (nested ResourceSpec) round-trips through JSON correctly.
func TestRoundTripDefaultResources(t *testing.T) {
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"default_resources": map[string]interface{}{
			"requests": map[string]interface{}{"cpu": "250m", "memory": "512Mi"},
			"limits":   map[string]interface{}{"cpu": "2", "memory": "4Gi"},
			"disk":     "20Gi",
		},
	}, "."), nil)

	raw, err := ExtractSectionFromKoanf(k, "agent_defaults")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	var settings AgentDefaultsSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("unmarshal into AgentDefaultsSettings: %v", err)
	}

	if settings.DefaultResources == nil {
		t.Fatal("expected DefaultResources to be non-nil")
	}
	if settings.DefaultResources.Requests.CPU != "250m" {
		t.Errorf("expected CPU=250m, got %q", settings.DefaultResources.Requests.CPU)
	}
	if settings.DefaultResources.Limits.Memory != "4Gi" {
		t.Errorf("expected Memory=4Gi, got %q", settings.DefaultResources.Limits.Memory)
	}
	if settings.DefaultResources.Disk != "20Gi" {
		t.Errorf("expected Disk=20Gi, got %q", settings.DefaultResources.Disk)
	}
}

// N1: Stronger round-trip using a real settings.yaml through the koanf YAML
// parser, matching the actual config loader chain.
func TestRoundTripFromYAMLFile(t *testing.T) {
	const sampleYAML = `schema_version: "1"
server:
  hub:
    port: 9810
    public_url: "https://hub.example.com"
    admin_emails:
      - "admin@example.com"
    soft_delete_retention: "72h"
    soft_delete_retain_files: true
    auto_suspend_stalled: true
  auth:
    mode: oauth
    user_access_mode: invite
    authorized_domains:
      - "example.com"
  database:
    driver: postgres
    url: "postgres://localhost/scion"
  github_app:
    app_id: 42
    api_base_url: "https://api.github.com"
    webhooks_enabled: true
    installation_url: "https://github.com/apps/myapp"
    private_key_path: "/etc/keys/gh.pem"
    private_key: "SECRET_KEY_MATERIAL"
    webhook_secret: "SECRET_WEBHOOK"
  notification_channels:
    - type: slack
      params:
        url: "https://hooks.slack.com/services/xxx"
telemetry:
  enabled: true
  cloud:
    endpoint: "https://otel.example.com"
    protocol: grpc
default_template: "standard"
default_harness_config: "base"
default_max_turns: 50
default_max_model_calls: 200
default_max_duration: "1h"
default_resources:
  requests:
    cpu: "100m"
    memory: "256Mi"
  limits:
    cpu: "1"
    memory: "1Gi"
  disk: "5Gi"
image_registry: "gcr.io/my-project"
`

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte(sampleYAML), 0644); err != nil {
		t.Fatalf("write settings.yaml: %v", err)
	}

	// Load through the real koanf YAML parser (same as pkg/config loader).
	original := koanf.New(".")
	if err := original.Load(file.Provider(settingsPath), yaml.Parser()); err != nil {
		t.Fatalf("load YAML via koanf: %v", err)
	}

	// Extract all sections.
	sections, err := ExtractAllSections(original)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Reload sections into a fresh koanf.
	reloaded, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Verify all Layer-1 leaf keys match between original and reloaded.
	// Skip parent/container keys (e.g. "server.github_app") — extraction
	// may intentionally filter child keys (like secrets), so comparing the
	// entire subtree would fail. Only compare leaf koanf paths that map
	// directly to section document fields.
	parentKeys := map[string]bool{
		"server.github_app": true,
		"telemetry.cloud":   true, "telemetry.cloud.tls": true, "telemetry.cloud.batch": true,
		"telemetry.hub": true, "telemetry.local": true,
		"telemetry.filter": true, "telemetry.filter.events": true,
		"telemetry.filter.attributes": true, "telemetry.filter.sampling": true,
	}
	for _, sec := range Registry {
		for _, kp := range sec.KoanfPaths {
			if parentKeys[kp] {
				continue
			}
			origVal := original.Get(kp)
			reloadVal := reloaded.Get(kp)
			if origVal == nil && reloadVal == nil {
				continue
			}
			if origVal == nil || reloadVal == nil {
				t.Errorf("[%s] key %q: original=%v, reloaded=%v", sec.Name, kp, origVal, reloadVal)
				continue
			}
			origJSON, _ := json.Marshal(origVal)
			reloadJSON, _ := json.Marshal(reloadVal)
			if string(origJSON) != string(reloadJSON) {
				t.Errorf("[%s] key %q mismatch:\n  original: %s\n  reloaded: %s", sec.Name, kp, origJSON, reloadJSON)
			}
		}
	}

	// Verify Layer-0 keys are NOT in the reloaded koanf.
	layer0Keys := []string{"server.database.driver", "server.hub.port", "server.auth.mode", "schema_version"}
	for _, key := range layer0Keys {
		if reloaded.Exists(key) {
			t.Errorf("Layer-0 key %q should not appear in reloaded sections", key)
		}
	}

	// Verify github_app secrets were excluded.
	ghDoc := sections["github_app"]
	var ghMap map[string]interface{}
	if err := json.Unmarshal(ghDoc, &ghMap); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if _, ok := ghMap["private_key"]; ok {
		t.Error("github_app section must not contain private_key")
	}
	if _, ok := ghMap["webhook_secret"]; ok {
		t.Error("github_app section must not contain webhook_secret")
	}
}

// --- Env-override detection tests ---

func TestEnvOverriddenLayer1Keys(t *testing.T) {
	envKeys := []string{
		"server.hub.admin_emails",
		"server.database.driver",
		"telemetry.enabled",
		"server.hub.port",
		"default_max_turns",
	}
	overridden := EnvOverriddenLayer1Keys(envKeys)

	expected := map[string]bool{
		"server.hub.admin_emails": true,
		"telemetry.enabled":       true,
		"default_max_turns":       true,
	}

	if len(overridden) != len(expected) {
		t.Errorf("expected %d overridden keys, got %d: %v", len(expected), len(overridden), overridden)
	}

	for _, key := range overridden {
		if !expected[key] {
			t.Errorf("unexpected overridden key: %q", key)
		}
	}
}

func TestDetectEnvOverrides(t *testing.T) {
	envK := koanf.New(".")
	_ = envK.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails": []string{"env@test.com"},
		"server.database.driver":  "sqlite3",
		"telemetry.enabled":       false,
	}, "."), nil)

	overridden := DetectEnvOverrides(envK)
	found := make(map[string]bool)
	for _, k := range overridden {
		found[k] = true
	}
	if !found["server.hub.admin_emails"] {
		t.Error("expected server.hub.admin_emails in overrides")
	}
	if !found["telemetry.enabled"] {
		t.Error("expected telemetry.enabled in overrides")
	}
	if found["server.database.driver"] {
		t.Error("server.database.driver should not be in Layer-1 overrides")
	}
}

// --- ClassifyKeys test ---

func TestClassifyKeys(t *testing.T) {
	keys := []string{
		"server.hub.admin_emails", // Layer-1 (access)
		"server.database.driver",  // Layer-0 (bootstrap)
		"telemetry.enabled",       // Layer-1 (telemetry)
		"server.hub.port",         // Layer-0 (bootstrap)
		"default_max_turns",       // Layer-1 (agent_defaults)
		"runtimes",                // unclassified
		"schema_version",          // unclassified
		"profiles",                // unclassified
	}
	l1, l0, unclassified := ClassifyKeys(keys)

	if len(l1["access"]) != 1 || l1["access"][0] != "server.hub.admin_emails" {
		t.Errorf("expected access to contain admin_emails, got %v", l1["access"])
	}
	if len(l1["telemetry"]) != 1 || l1["telemetry"][0] != "telemetry.enabled" {
		t.Errorf("expected telemetry to contain enabled, got %v", l1["telemetry"])
	}
	if len(l1["agent_defaults"]) != 1 || l1["agent_defaults"][0] != "default_max_turns" {
		t.Errorf("expected agent_defaults to contain default_max_turns, got %v", l1["agent_defaults"])
	}

	expectedL0 := map[string]bool{
		"server.database.driver": true,
		"server.hub.port":        true,
	}
	if len(l0) != len(expectedL0) {
		t.Errorf("expected %d Layer-0 keys, got %d: %v", len(expectedL0), len(l0), l0)
	}
	for _, k := range l0 {
		if !expectedL0[k] {
			t.Errorf("unexpected Layer-0 key: %q", k)
		}
	}

	// Unclassified keys — neither Layer-0 nor Layer-1.
	expectedUnclassified := map[string]bool{
		"runtimes":       true,
		"schema_version": true,
		"profiles":       true,
	}
	if len(unclassified) != len(expectedUnclassified) {
		t.Errorf("expected %d unclassified keys, got %d: %v", len(expectedUnclassified), len(unclassified), unclassified)
	}
	for _, k := range unclassified {
		if !expectedUnclassified[k] {
			t.Errorf("unexpected unclassified key: %q", k)
		}
	}
}

func TestClassifyKeys_AllLayer0Prefixes(t *testing.T) {
	// Verify that all explicit Layer-0 bootstrap keys are correctly classified.
	layer0Keys := []string{
		"server.database",
		"server.database.driver",
		"server.hub.port",
		"server.hub.host",
		"server.hub.read_timeout",
		"server.hub.write_timeout",
		"server.broker",
		"server.broker.port",
		"server.auth.mode",
		"server.auth.dev_mode",
		"server.auth.dev_token",
		"server.auth.dev_token_file", // N4: was missing
		"server.auth.proxy",
		"server.auth.transport",
		"server.oauth",
		"server.secrets",
		"server.storage",
		"server.workspace_storage",
		"server.mode",
		"server.env",
		"server.hub.hub_id",
		"server.hub.gcp_project_id",
		"server.log_level",
		"server.log_format",
		"server.hub.cors",
		"server.message_broker",
		"server.plugins",
	}

	_, l0, unclassified := ClassifyKeys(layer0Keys)
	if len(unclassified) > 0 {
		t.Errorf("expected no unclassified keys, got %v", unclassified)
	}
	if len(l0) != len(layer0Keys) {
		t.Errorf("expected %d Layer-0 keys, got %d: %v", len(layer0Keys), len(l0), l0)
	}
}

func TestClassifyKeys_UnclassifiedKeys(t *testing.T) {
	// Keys that exist in the settings file but are not Layer-0 or Layer-1.
	unclassifiedKeys := []string{
		"runtimes",
		"harness_configs",
		"profiles",
		"schema_version",
		"active_profile",
		"workspace_path",
	}

	l1, l0, unclassified := ClassifyKeys(unclassifiedKeys)
	if len(l1) > 0 {
		t.Errorf("expected no Layer-1 keys, got %v", l1)
	}
	if len(l0) > 0 {
		t.Errorf("expected no Layer-0 keys, got %v", l0)
	}
	if len(unclassified) != len(unclassifiedKeys) {
		t.Errorf("expected %d unclassified keys, got %d: %v", len(unclassifiedKeys), len(unclassified), unclassified)
	}
}

// --- MergeSectionsIntoKoanf test ---

func TestMergeSectionsIntoKoanf(t *testing.T) {
	target := koanf.New(".")
	_ = target.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []string{"file@test.com"},
		"server.hub.port":              9810,
		"server.database.driver":       "postgres",
		"server.auth.user_access_mode": "closed",
	}, "."), nil)

	sections := map[string]json.RawMessage{
		"access": json.RawMessage(`{"admin_emails":["db@test.com"],"user_access_mode":"open"}`),
	}

	if err := MergeSectionsIntoKoanf(target, sections); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if mode := target.String("server.auth.user_access_mode"); mode != "open" {
		t.Errorf("expected user_access_mode=open after merge, got %q", mode)
	}
	if port := target.Int("server.hub.port"); port != 9810 {
		t.Errorf("expected port preserved, got %d", port)
	}
}

// --- SectionNames test ---

func TestSectionNames(t *testing.T) {
	names := SectionNames()
	if len(names) != len(Registry) {
		t.Errorf("expected %d names, got %d", len(Registry), len(names))
	}
}

func TestGlobalDefaultsReserved(t *testing.T) {
	if SectionByName("global_defaults") != nil {
		t.Error("global_defaults should not be registered — it is reserved for future use")
	}
}
