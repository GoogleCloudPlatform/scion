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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// --- Registry completeness tests ---

func TestRegistryHasAllSections(t *testing.T) {
	expected := []string{"access", "lifecycle", "maintenance", "telemetry",
		"agent_defaults", "endpoints", "github_app", "notifications",
		"project_defaults", "auto_expose_ports", "federation"}
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
	// Sections that are DB-only (no settings.yaml representation).
	dbOnlySections := map[string]bool{
		"maintenance": true,
	}
	for _, sec := range Registry {
		if dbOnlySections[sec.Name] {
			if len(sec.KoanfPaths) != 0 {
				t.Errorf("%s section should have empty KoanfPaths, got %v", sec.Name, sec.KoanfPaths)
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
		{"auto_expose_ports.enabled", "auto_expose_ports"},
		{"server.federation.enabled", "federation"},
		{"server.federation.trusted_issuers", "federation"},
		{"server.federation.algorithms", "federation"},
		{"server.federation.refresh_interval", "federation"},
		{"server.federation.debounce_interval", "federation"},
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
		{"project_defaults", `{"default_scratchpad":true}`},
		{"project_defaults", `{"default_scratchpad":false}`},
		{"project_defaults", `{}`},
		{"auto_expose_ports", `{"enabled":true}`},
		{"auto_expose_ports", `{}`},
		{"federation", `{"enabled":true,"trusted_issuers":[{"issuer_url":"https://hub.example.com","issuer_type":"hub"}],"algorithms":["RS256"]}`},
		{"federation", `{"enabled":false}`},
		{"federation", `{}`},
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
		{"project_defaults", `{"default_scratchpad":"yes"}`, "wrong type for boolean"},
		{"project_defaults", `{"unknown_field":true}`, "additional property"},
		{"federation", `{"trusted_issuers":[{"issuer_url":""}]}`, "empty issuer_url (minLength)"},
		{"federation", `{"algorithms":["INVALID"]}`, "invalid algorithm enum"},
		{"federation", `{"trusted_issuers":[{"issuer_type":"unknown"}]}`, "invalid issuer_type enum"},
		{"federation", `{"unknown_field": true}`, "additional property"},
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

// --- FederationSettings section document tests ---

func TestFederationSettingsRoundTrip(t *testing.T) {
	enabled := true
	original := &FederationSettings{
		Enabled: &enabled,
		TrustedIssuers: []config.V1TrustedIssuerConfig{
			{
				IssuerURL:        "https://hub-a.example.com",
				JWKSURL:          "https://hub-a.example.com/.well-known/jwks.json",
				ExpectedAudience: "https://hub-b.example.com",
				AllowedProjects:  []string{"proj1"},
				AllowedRootUsers: []string{"admin@example.com"},
				DefaultScopes:    []string{"agent:status:update"},
				IssuerType:       "hub",
			},
		},
		Algorithms:       []string{"RS256"},
		RefreshInterval:  "1h",
		DebounceInterval: "5s",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored FederationSettings
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if *restored.Enabled != true {
		t.Errorf("Enabled: got %v, want true", *restored.Enabled)
	}
	if len(restored.TrustedIssuers) != 1 {
		t.Fatalf("TrustedIssuers: got %d, want 1", len(restored.TrustedIssuers))
	}
	if restored.TrustedIssuers[0].IssuerURL != "https://hub-a.example.com" {
		t.Errorf("IssuerURL: got %q, want %q", restored.TrustedIssuers[0].IssuerURL, "https://hub-a.example.com")
	}
	if restored.TrustedIssuers[0].IssuerType != "hub" {
		t.Errorf("IssuerType: got %q, want %q", restored.TrustedIssuers[0].IssuerType, "hub")
	}
	if restored.RefreshInterval != "1h" {
		t.Errorf("RefreshInterval: got %q, want %q", restored.RefreshInterval, "1h")
	}
	if restored.DebounceInterval != "5s" {
		t.Errorf("DebounceInterval: got %q, want %q", restored.DebounceInterval, "5s")
	}

	// Validate the JSON against the section schema
	errs := Validate("federation", json.RawMessage(data))
	if len(errs) > 0 {
		t.Errorf("section doc should validate against schema, got: %v", errs)
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
		"server.database.driver":    "postgres",
		"server.hub.port":           9810,
		"auto_expose_ports.enabled": true,
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
		{"auto_expose_ports", func(t *testing.T, doc map[string]interface{}) {
			if doc["enabled"] != true {
				t.Errorf("expected enabled=true, got %v", doc["enabled"])
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
		"server.database.driver":    "postgres",
		"server.hub.port":           9810,
		"auto_expose_ports.enabled": true,
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
		{"auto_expose_ports.enabled", true},
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
auto_expose_ports:
  enabled: true
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
	// After H1 generalization, all env keys are returned (not just Layer-1).
	if !found["server.database.driver"] {
		t.Error("expected server.database.driver in overrides (all env keys reported)")
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
		"runtimes",                // Layer-1 (runtimes)
		"schema_version",          // unclassified
		"profiles",                // Layer-1 (profiles)
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
	if len(l1["runtimes"]) != 1 || l1["runtimes"][0] != "runtimes" {
		t.Errorf("expected runtimes section to contain runtimes key, got %v", l1["runtimes"])
	}
	if len(l1["profiles"]) != 1 || l1["profiles"][0] != "profiles" {
		t.Errorf("expected profiles section to contain profiles key, got %v", l1["profiles"])
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
		"schema_version": true,
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
		"server.native_chat",
		"server.native_chat.enabled",
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

func TestClassifyKeys_MapSectionsAreLayer1(t *testing.T) {
	// runtimes, profiles, harness_configs are now Layer-1 sections.
	keys := []string{"runtimes", "profiles", "harness_configs"}
	l1, l0, unclassified := ClassifyKeys(keys)

	if len(l0) > 0 {
		t.Errorf("expected no Layer-0 keys, got %v", l0)
	}
	if len(unclassified) > 0 {
		t.Errorf("expected no unclassified keys, got %v", unclassified)
	}
	for _, sec := range []string{"runtimes", "profiles", "harness_configs"} {
		if len(l1[sec]) != 1 || l1[sec][0] != sec {
			t.Errorf("expected %s to be Layer-1, got %v", sec, l1[sec])
		}
	}
}

func TestOwningSection_MapSectionNestedKeys(t *testing.T) {
	// Prefix-only registration means nested keys are owned by the parent section.
	tests := []struct {
		key     string
		section string
	}{
		{"runtimes", "runtimes"},
		{"runtimes.cloudrun", "runtimes"},
		{"runtimes.cloudrun.type", "runtimes"},
		{"profiles", "profiles"},
		{"profiles.default.runtime", "profiles"},
		{"harness_configs", "harness_configs"},
		{"harness_configs.claude-code.harness", "harness_configs"},
	}
	for _, tt := range tests {
		got := OwningSection(tt.key)
		if got != tt.section {
			t.Errorf("OwningSection(%q) = %q, want %q", tt.key, got, tt.section)
		}
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
	// Verify project_defaults appears in the list.
	found := false
	for _, n := range names {
		if n == "project_defaults" {
			found = true
			break
		}
	}
	if !found {
		t.Error("project_defaults not found in SectionNames()")
	}
}

func TestGlobalDefaultsReserved(t *testing.T) {
	if SectionByName("global_defaults") != nil {
		t.Error("global_defaults should not be registered — it is reserved for future use")
	}
}

// --- DetectDeprecatedServerEnv tests ---

func TestDetectDeprecatedServerEnv_Layer1Keys(t *testing.T) {
	envK := koanf.New(".")
	_ = envK.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []string{"admin@test.com"},
		"server.auth.user_access_mode": "invite",
		"telemetry.enabled":            true,
	}, "."), nil)

	deprecated := DetectDeprecatedServerEnv(envK)
	if len(deprecated) != 3 {
		t.Fatalf("expected 3 deprecated vars, got %d: %+v", len(deprecated), deprecated)
	}

	found := make(map[string]DeprecatedEnvVar)
	for _, d := range deprecated {
		found[d.KoanfKey] = d
	}

	// Check admin_emails — koanfKeyToEnvSuffix strips "server." so suffix is HUB_ADMINEMAILS
	if d, ok := found["server.hub.admin_emails"]; !ok {
		t.Error("expected server.hub.admin_emails in deprecated list")
	} else {
		if d.EnvVar != "SCION_SERVER_HUB_ADMINEMAILS" {
			t.Errorf("expected env var SCION_SERVER_HUB_ADMINEMAILS, got %q", d.EnvVar)
		}
		if d.SeedEquivalent != "SCION_SEED_SERVER_HUB_ADMINEMAILS" {
			t.Errorf("expected seed equivalent SCION_SEED_SERVER_HUB_ADMINEMAILS, got %q", d.SeedEquivalent)
		}
	}

	// Check user_access_mode
	if d, ok := found["server.auth.user_access_mode"]; !ok {
		t.Error("expected server.auth.user_access_mode in deprecated list")
	} else {
		if d.EnvVar != "SCION_SERVER_AUTH_USERACCESSMODE" {
			t.Errorf("expected env var SCION_SERVER_AUTH_USERACCESSMODE, got %q", d.EnvVar)
		}
		if d.SeedEquivalent != "SCION_SEED_SERVER_AUTH_USERACCESSMODE" {
			t.Errorf("expected seed equivalent SCION_SEED_SERVER_AUTH_USERACCESSMODE, got %q", d.SeedEquivalent)
		}
	}

	// Check telemetry.enabled — no server. prefix, so SEED equivalent uses
	// SCION_SEED_ (not SCION_SEED_SERVER_) to map back to telemetry.enabled.
	if d, ok := found["telemetry.enabled"]; !ok {
		t.Error("expected telemetry.enabled in deprecated list")
	} else {
		if d.EnvVar != "SCION_SERVER_TELEMETRY_ENABLED" {
			t.Errorf("expected env var SCION_SERVER_TELEMETRY_ENABLED, got %q", d.EnvVar)
		}
		if d.SeedEquivalent != "SCION_SEED_TELEMETRY_ENABLED" {
			t.Errorf("expected seed equivalent SCION_SEED_TELEMETRY_ENABLED, got %q", d.SeedEquivalent)
		}
	}
}

func TestDetectDeprecatedServerEnv_NoLayer1Keys(t *testing.T) {
	envK := koanf.New(".")
	_ = envK.Load(confmap.Provider(map[string]interface{}{
		"server.database.driver": "postgres",
		"server.hub.port":        9810,
		"server.log_level":       "debug",
	}, "."), nil)

	deprecated := DetectDeprecatedServerEnv(envK)
	if len(deprecated) != 0 {
		t.Errorf("expected no deprecated vars for Layer-0 keys, got %d: %+v", len(deprecated), deprecated)
	}
}

func TestDetectDeprecatedServerEnv_Empty(t *testing.T) {
	envK := koanf.New(".")
	deprecated := DetectDeprecatedServerEnv(envK)
	if len(deprecated) != 0 {
		t.Errorf("expected no deprecated vars for empty koanf, got %d", len(deprecated))
	}
}

func TestDetectDeprecatedServerEnv_MixedKeys(t *testing.T) {
	envK := koanf.New(".")
	_ = envK.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails": []string{"admin@test.com"},
		"server.database.driver":  "postgres",
		"server.hub.port":         9810,
		"default_max_turns":       100,
	}, "."), nil)

	deprecated := DetectDeprecatedServerEnv(envK)

	// Only Layer-1 keys should be flagged: admin_emails and default_max_turns.
	if len(deprecated) != 2 {
		t.Fatalf("expected 2 deprecated vars, got %d: %+v", len(deprecated), deprecated)
	}

	foundKeys := make(map[string]bool)
	for _, d := range deprecated {
		foundKeys[d.KoanfKey] = true
	}
	if !foundKeys["server.hub.admin_emails"] {
		t.Error("expected server.hub.admin_emails in deprecated list")
	}
	if !foundKeys["default_max_turns"] {
		t.Error("expected default_max_turns in deprecated list")
	}
}

func TestKoanfPathFromSectionKey(t *testing.T) {
	tests := []struct {
		section string
		jsonKey string
		want    string
	}{
		{"access", "admin_emails", "server.hub.admin_emails"},
		{"access", "user_access_mode", "server.auth.user_access_mode"},
		{"github_app", "webhooks_enabled", "server.github_app.webhooks_enabled"},
		{"endpoints", "public_url", "server.hub.public_url"},
		{"access", "nonexistent_key", ""},
		{"nonexistent_section", "admin_emails", ""},
	}
	for _, tt := range tests {
		got := KoanfPathFromSectionKey(tt.section, tt.jsonKey)
		if got != tt.want {
			t.Errorf("KoanfPathFromSectionKey(%q, %q) = %q, want %q", tt.section, tt.jsonKey, got, tt.want)
		}
	}
}

// TestAutoExposePortsKoanfRoundTrip verifies that auto_expose_ports can be
// extracted from koanf and loaded back without data loss.
func TestAutoExposePortsKoanfRoundTrip(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"auto_expose_ports.enabled": true,
	}, "."), nil)
	if err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	// Extract the section.
	raw, err := ExtractSectionFromKoanf(k, "auto_expose_ports")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["enabled"] != true {
		t.Errorf("expected enabled=true in extracted doc, got %v", doc["enabled"])
	}

	// Reload into a fresh koanf.
	sections := map[string]json.RawMessage{
		"auto_expose_ports": raw,
	}
	reloaded, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("load sections: %v", err)
	}

	if !reloaded.Exists("auto_expose_ports.enabled") {
		t.Fatal("expected auto_expose_ports.enabled to exist in reloaded koanf")
	}
	if reloaded.Bool("auto_expose_ports.enabled") != true {
		t.Errorf("expected auto_expose_ports.enabled=true, got %v", reloaded.Get("auto_expose_ports.enabled"))
	}
}

// TestAutoExposePortsEmptyExtract verifies that ExtractSectionFromKoanf returns
// an empty doc when auto_expose_ports is not set.
func TestAutoExposePortsEmptyExtract(t *testing.T) {
	k := koanf.New(".")
	raw, err := ExtractSectionFromKoanf(k, "auto_expose_ports")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc) != 0 {
		t.Errorf("expected empty doc for absent auto_expose_ports, got %v", doc)
	}
}

// TestRuntimesKoanfRoundTrip verifies that runtimes can be extracted from
// koanf and loaded back without data loss (map-of-objects section).
func TestRuntimesKoanfRoundTrip(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"runtimes.docker.type":   "docker",
		"runtimes.cloudrun.type": "cloudrun-instances",
		"runtimes.cloudrun.gke":  true,
	}, "."), nil)
	if err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	// Extract.
	raw, err := ExtractSectionFromKoanf(k, "runtimes")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	docker, ok := doc["docker"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected docker entry in runtimes doc, got %v", doc)
	}
	if docker["type"] != "docker" {
		t.Errorf("expected docker.type=docker, got %v", docker["type"])
	}
	cloudrun, ok := doc["cloudrun"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected cloudrun entry in runtimes doc, got %v", doc)
	}
	if cloudrun["type"] != "cloudrun-instances" {
		t.Errorf("expected cloudrun.type=cloudrun-instances, got %v", cloudrun["type"])
	}

	// Reload into a fresh koanf.
	sections := map[string]json.RawMessage{"runtimes": raw}
	reloaded, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("load sections: %v", err)
	}

	if reloaded.String("runtimes.docker.type") != "docker" {
		t.Errorf("expected runtimes.docker.type=docker, got %v", reloaded.Get("runtimes.docker.type"))
	}
	if reloaded.String("runtimes.cloudrun.type") != "cloudrun-instances" {
		t.Errorf("expected runtimes.cloudrun.type=cloudrun-instances, got %v", reloaded.Get("runtimes.cloudrun.type"))
	}
}

// TestRuntimesCloudRunInstancesKoanfRoundTrip verifies that the nested
// CloudRunInstances struct (cloudrun_instances.project_id, cloudrun_instances.region)
// survives the koanf extract → JSON → reload → unmarshal round-trip.
// This was the failing path described in issue #984.
func TestRuntimesCloudRunInstancesKoanfRoundTrip(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"runtimes.docker.type":                      "docker",
		"runtimes.cr.type":                          "cloudrun-instances",
		"runtimes.cr.cloudrun_instances.project_id": "my-gcp-project",
		"runtimes.cr.cloudrun_instances.region":     "us-central1",
	}, "."), nil)
	if err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	// Extract runtimes section as JSON.
	raw, err := ExtractSectionFromKoanf(k, "runtimes")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Verify nested struct is present in the extracted JSON.
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
	crEntry, ok := doc["cr"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected cr entry in runtimes doc, got %v", doc)
	}
	crInstances, ok := crEntry["cloudrun_instances"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected cloudrun_instances sub-object in cr entry, got %v", crEntry)
	}
	if crInstances["project_id"] != "my-gcp-project" {
		t.Errorf("expected cloudrun_instances.project_id=my-gcp-project, got %v", crInstances["project_id"])
	}
	if crInstances["region"] != "us-central1" {
		t.Errorf("expected cloudrun_instances.region=us-central1, got %v", crInstances["region"])
	}

	// Reload into fresh koanf and verify the round-trip preserves keys.
	sections := map[string]json.RawMessage{"runtimes": raw}
	reloaded, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("load sections: %v", err)
	}

	if reloaded.String("runtimes.cr.type") != "cloudrun-instances" {
		t.Errorf("expected runtimes.cr.type=cloudrun-instances, got %v", reloaded.Get("runtimes.cr.type"))
	}
	if reloaded.String("runtimes.cr.cloudrun_instances.project_id") != "my-gcp-project" {
		t.Errorf("expected runtimes.cr.cloudrun_instances.project_id=my-gcp-project, got %v",
			reloaded.Get("runtimes.cr.cloudrun_instances.project_id"))
	}
	if reloaded.String("runtimes.cr.cloudrun_instances.region") != "us-central1" {
		t.Errorf("expected runtimes.cr.cloudrun_instances.region=us-central1, got %v",
			reloaded.Get("runtimes.cr.cloudrun_instances.region"))
	}
}

// TestProfilesKoanfRoundTrip verifies that profiles can be extracted and reloaded.
func TestProfilesKoanfRoundTrip(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"profiles.default.runtime":          "cloudrun",
		"profiles.default.default_template": "medium",
		"profiles.dev.runtime":              "docker",
	}, "."), nil)
	if err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	raw, err := ExtractSectionFromKoanf(k, "profiles")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	defaultProfile, ok := doc["default"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected default entry in profiles doc, got %v", doc)
	}
	if defaultProfile["runtime"] != "cloudrun" {
		t.Errorf("expected default.runtime=cloudrun, got %v", defaultProfile["runtime"])
	}

	// Reload.
	sections := map[string]json.RawMessage{"profiles": raw}
	reloaded, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("load sections: %v", err)
	}
	if reloaded.String("profiles.default.runtime") != "cloudrun" {
		t.Errorf("expected profiles.default.runtime=cloudrun, got %v", reloaded.Get("profiles.default.runtime"))
	}
}

// TestHarnessConfigsKoanfRoundTrip verifies that harness_configs can be extracted and reloaded.
func TestHarnessConfigsKoanfRoundTrip(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"harness_configs.claude-code.harness": "claude-code",
		"harness_configs.claude-code.image":   "gcr.io/test/claude-code:latest",
		"harness_configs.aider.harness":       "aider",
	}, "."), nil)
	if err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	raw, err := ExtractSectionFromKoanf(k, "harness_configs")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	claudeCode, ok := doc["claude-code"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected claude-code entry in harness_configs doc, got %v", doc)
	}
	if claudeCode["harness"] != "claude-code" {
		t.Errorf("expected harness=claude-code, got %v", claudeCode["harness"])
	}

	// Reload.
	sections := map[string]json.RawMessage{"harness_configs": raw}
	reloaded, err := LoadSectionsIntoKoanf(sections)
	if err != nil {
		t.Fatalf("load sections: %v", err)
	}
	if reloaded.String("harness_configs.claude-code.harness") != "claude-code" {
		t.Errorf("expected harness_configs.claude-code.harness=claude-code, got %v", reloaded.Get("harness_configs.claude-code.harness"))
	}
}

// TestMapSectionsEmptyExtract verifies that extracting map-of-objects sections
// from an empty koanf produces empty documents.
func TestMapSectionsEmptyExtract(t *testing.T) {
	k := koanf.New(".")
	for _, sec := range []string{"runtimes", "profiles", "harness_configs"} {
		raw, err := ExtractSectionFromKoanf(k, sec)
		if err != nil {
			t.Fatalf("extract %s: %v", sec, err)
		}
		var doc map[string]interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal %s: %v", sec, err)
		}
		if len(doc) != 0 {
			t.Errorf("expected empty doc for absent %s, got %v", sec, doc)
		}
	}
}

// TestMapSectionsSchemaValidation verifies that the JSON schemas validate
// map-of-objects section documents correctly.
func TestMapSectionsSchemaValidation(t *testing.T) {
	// Valid runtimes doc.
	errs := Validate("runtimes", json.RawMessage(`{"docker": {"type": "docker"}}`))
	if len(errs) > 0 {
		t.Errorf("expected valid runtimes doc, got errors: %v", errs)
	}

	// Valid profiles doc.
	errs = Validate("profiles", json.RawMessage(`{"default": {"runtime": "cloudrun"}}`))
	if len(errs) > 0 {
		t.Errorf("expected valid profiles doc, got errors: %v", errs)
	}

	// Valid harness_configs doc.
	errs = Validate("harness_configs", json.RawMessage(`{"claude-code": {"harness": "claude-code", "image": "test:latest"}}`))
	if len(errs) > 0 {
		t.Errorf("expected valid harness_configs doc, got errors: %v", errs)
	}

	// Valid empty docs.
	for _, sec := range []string{"runtimes", "profiles", "harness_configs"} {
		errs = Validate(sec, json.RawMessage(`{}`))
		if len(errs) > 0 {
			t.Errorf("expected valid empty %s doc, got errors: %v", sec, errs)
		}
	}

	// Invalid: harness_configs entry missing required "harness" field.
	errs = Validate("harness_configs", json.RawMessage(`{"bad": {"image": "test:latest"}}`))
	if len(errs) == 0 {
		t.Error("expected validation error for harness_configs entry missing 'harness' field")
	}
}

// TestMapSectionsInSectionNames verifies that the three new sections appear
// in the SectionNames() output.
func TestMapSectionsInSectionNames(t *testing.T) {
	names := SectionNames()
	found := make(map[string]bool)
	for _, n := range names {
		found[n] = true
	}
	for _, sec := range []string{"runtimes", "profiles", "harness_configs"} {
		if !found[sec] {
			t.Errorf("expected %q in SectionNames(), not found", sec)
		}
	}
}

func TestKoanfKeyToEnvSuffix(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		// server. prefix is stripped — it maps to the SCION_SERVER_ env prefix
		{"server.hub.admin_emails", "HUB_ADMINEMAILS"},
		{"server.auth.user_access_mode", "AUTH_USERACCESSMODE"},
		{"server.hub.public_url", "HUB_PUBLICURL"},
		// non-server keys are unchanged
		{"telemetry.enabled", "TELEMETRY_ENABLED"},
		{"default_max_turns", "DEFAULTMAXTURNS"},
	}
	for _, tt := range tests {
		got := koanfKeyToEnvSuffix(tt.key)
		if got != tt.want {
			t.Errorf("koanfKeyToEnvSuffix(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
