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

package bridge

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAdminOverlay_FullConfig(t *testing.T) {
	cfg := map[string]string{
		"external_url":         "https://a2a.example.com",
		"auth_scheme":          "apiKey",
		"api_key":              "secret123",
		"uat_cache_ttl":        "120s",
		"rate_limit_enabled":   "true",
		"rate_limit_rps":       "50",
		"rate_limit_burst":     "100",
		"send_message_timeout": "60s",
		"sse_keepalive":        "15s",
		"push_retry_max":       "5",
		"provider_org":         "Test Org",
		"provider_url":         "https://test.com",
		"projects_json":        `[{"slug":"proj1","default_template":"default","auto_provision":true,"exposed_agents":["a1","a2"]}]`,
	}

	overlay, err := ParseAdminOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if overlay.ExternalURL != "https://a2a.example.com" {
		t.Errorf("ExternalURL = %q, want %q", overlay.ExternalURL, "https://a2a.example.com")
	}
	if overlay.AuthScheme != "apiKey" {
		t.Errorf("AuthScheme = %q, want %q", overlay.AuthScheme, "apiKey")
	}
	if overlay.APIKey != "secret123" {
		t.Errorf("APIKey = %q, want %q", overlay.APIKey, "secret123")
	}
	if overlay.UATCacheTTL != 120*time.Second {
		t.Errorf("UATCacheTTL = %v, want %v", overlay.UATCacheTTL, 120*time.Second)
	}
	if overlay.RateLimitEnabled == nil || !*overlay.RateLimitEnabled {
		t.Error("RateLimitEnabled should be true")
	}
	if overlay.RateLimitRPS != 50 {
		t.Errorf("RateLimitRPS = %v, want 50", overlay.RateLimitRPS)
	}
	if overlay.RateLimitBurst != 100 {
		t.Errorf("RateLimitBurst = %v, want 100", overlay.RateLimitBurst)
	}
	if overlay.SendMessageTimeout != 60*time.Second {
		t.Errorf("SendMessageTimeout = %v, want 60s", overlay.SendMessageTimeout)
	}
	if overlay.SSEKeepalive != 15*time.Second {
		t.Errorf("SSEKeepalive = %v, want 15s", overlay.SSEKeepalive)
	}
	if overlay.PushRetryMax != 5 {
		t.Errorf("PushRetryMax = %v, want 5", overlay.PushRetryMax)
	}
	if overlay.ProviderOrg != "Test Org" {
		t.Errorf("ProviderOrg = %q, want %q", overlay.ProviderOrg, "Test Org")
	}
	if overlay.ProviderURL != "https://test.com" {
		t.Errorf("ProviderURL = %q, want %q", overlay.ProviderURL, "https://test.com")
	}
	if len(overlay.Projects) != 1 {
		t.Fatalf("Projects = %d, want 1", len(overlay.Projects))
	}
	if overlay.Projects[0].Slug != "proj1" {
		t.Errorf("Projects[0].Slug = %q, want %q", overlay.Projects[0].Slug, "proj1")
	}
	if !overlay.Projects[0].AutoProvision {
		t.Error("Projects[0].AutoProvision should be true")
	}
	if len(overlay.Projects[0].ExposedAgents) != 2 {
		t.Errorf("Projects[0].ExposedAgents = %d, want 2", len(overlay.Projects[0].ExposedAgents))
	}
}

func TestParseAdminOverlay_MinimalConfig(t *testing.T) {
	cfg := map[string]string{
		"external_url": "https://minimal.example.com",
	}

	overlay, err := ParseAdminOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if overlay.ExternalURL != "https://minimal.example.com" {
		t.Errorf("ExternalURL = %q, want %q", overlay.ExternalURL, "https://minimal.example.com")
	}
	if overlay.AuthScheme != "" {
		t.Errorf("AuthScheme = %q, want empty", overlay.AuthScheme)
	}
	if overlay.RateLimitEnabled != nil {
		t.Error("RateLimitEnabled should be nil (absent)")
	}
	if overlay.RateLimitBurst != -1 {
		t.Errorf("RateLimitBurst = %d, want -1 (not set)", overlay.RateLimitBurst)
	}
	if overlay.PushRetryMax != -1 {
		t.Errorf("PushRetryMax = %d, want -1 (not set)", overlay.PushRetryMax)
	}

	// Verify IsPresent
	if !overlay.IsPresent("external_url") {
		t.Error("expected external_url to be present")
	}
	if overlay.IsPresent("auth_scheme") {
		t.Error("expected auth_scheme to NOT be present")
	}
}

func TestParseAdminOverlay_EmptyConfig(t *testing.T) {
	cfg := map[string]string{}

	overlay, err := ParseAdminOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overlay.ExternalURL != "" {
		t.Errorf("ExternalURL = %q, want empty", overlay.ExternalURL)
	}
}

func TestParseAdminOverlay_InvalidAuthScheme(t *testing.T) {
	cfg := map[string]string{
		"auth_scheme": "bogus",
	}

	_, err := ParseAdminOverlay(cfg)
	if err == nil {
		t.Fatal("expected error for invalid auth_scheme")
	}
}

func TestParseAdminOverlay_ValidAuthSchemes(t *testing.T) {
	schemes := []string{"apiKey", "bearer", "none", "hubUAT", "hubJWT"}
	for _, scheme := range schemes {
		cfg := map[string]string{"auth_scheme": scheme}
		overlay, err := ParseAdminOverlay(cfg)
		if err != nil {
			t.Errorf("scheme %q: unexpected error: %v", scheme, err)
		}
		if overlay.AuthScheme != scheme {
			t.Errorf("scheme %q: got %q", scheme, overlay.AuthScheme)
		}
	}
}

func TestParseAdminOverlay_InvalidDuration(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"uat_cache_ttl", "not_a_duration"},
		{"send_message_timeout", "abc"},
		{"sse_keepalive", "xyz"},
	}
	for _, tt := range tests {
		cfg := map[string]string{tt.key: tt.value}
		_, err := ParseAdminOverlay(cfg)
		if err == nil {
			t.Errorf("key %q value %q: expected error", tt.key, tt.value)
		}
	}
}

func TestParseAdminOverlay_NegativeDuration(t *testing.T) {
	cfg := map[string]string{"uat_cache_ttl": "-5s"}
	_, err := ParseAdminOverlay(cfg)
	if err == nil {
		t.Fatal("expected error for negative uat_cache_ttl")
	}
}

func TestParseAdminOverlay_InvalidProjectsJSON(t *testing.T) {
	cfg := map[string]string{"projects_json": "not valid json"}
	_, err := ParseAdminOverlay(cfg)
	if err == nil {
		t.Fatal("expected error for invalid projects_json")
	}
}

func TestParseAdminOverlay_NegativeRateLimits(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"rate_limit_rps", "-5"},
		{"rate_limit_rps", "0"},
		{"rate_limit_burst", "-1"},
		{"push_retry_max", "-1"},
	}
	for _, tt := range tests {
		cfg := map[string]string{tt.key: tt.value}
		_, err := ParseAdminOverlay(cfg)
		if err == nil {
			t.Errorf("key %q value %q: expected error", tt.key, tt.value)
		}
	}
}

func TestParseAdminOverlay_ZeroBurst(t *testing.T) {
	// Zero burst is valid (means use default 20).
	cfg := map[string]string{"rate_limit_burst": "0"}
	overlay, err := ParseAdminOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overlay.RateLimitBurst != 0 {
		t.Errorf("RateLimitBurst = %d, want 0", overlay.RateLimitBurst)
	}
}

func TestParseAdminOverlay_UATCacheTTLExceedsMax(t *testing.T) {
	cfg := map[string]string{"uat_cache_ttl": "600s"}
	_, err := ParseAdminOverlay(cfg)
	if err == nil {
		t.Fatal("expected error for uat_cache_ttl > 300s")
	}
}

func TestPersistAndLoadOverlay_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	rlEnabled := true
	overlay := &AdminOverlay{
		ExternalURL:        "https://a2a.example.com",
		AuthScheme:         "hubUAT",
		APIKey:             "should-not-persist",
		UATCacheTTL:        90 * time.Second,
		RateLimitEnabled:   &rlEnabled,
		RateLimitRPS:       25,
		RateLimitBurst:     50,
		SendMessageTimeout: 60 * time.Second,
		SSEKeepalive:       10 * time.Second,
		PushRetryMax:       7,
		ProviderOrg:        "Test Org",
		ProviderURL:        "https://test.com",
		Projects: []ProjectConfig{
			{Slug: "p1", DefaultTemplate: "default", AutoProvision: true, ExposedAgents: []string{"a1"}},
		},
		presentKeys: map[string]bool{
			"external_url":         true,
			"auth_scheme":          true,
			"api_key":              true,
			"uat_cache_ttl":        true,
			"rate_limit_enabled":   true,
			"rate_limit_rps":       true,
			"rate_limit_burst":     true,
			"send_message_timeout": true,
			"sse_keepalive":        true,
			"push_retry_max":       true,
			"provider_org":         true,
			"provider_url":         true,
			"projects_json":        true,
		},
	}

	if err := PersistOverlay(dir, overlay); err != nil {
		t.Fatalf("PersistOverlay: %v", err)
	}

	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil overlay")
	}

	// Verify values round-tripped.
	if loaded.ExternalURL != overlay.ExternalURL {
		t.Errorf("ExternalURL = %q, want %q", loaded.ExternalURL, overlay.ExternalURL)
	}
	if loaded.AuthScheme != overlay.AuthScheme {
		t.Errorf("AuthScheme = %q, want %q", loaded.AuthScheme, overlay.AuthScheme)
	}
	if loaded.UATCacheTTL != overlay.UATCacheTTL {
		t.Errorf("UATCacheTTL = %v, want %v", loaded.UATCacheTTL, overlay.UATCacheTTL)
	}
	if loaded.RateLimitEnabled == nil || *loaded.RateLimitEnabled != true {
		t.Error("RateLimitEnabled should be true")
	}
	if loaded.RateLimitRPS != overlay.RateLimitRPS {
		t.Errorf("RateLimitRPS = %v, want %v", loaded.RateLimitRPS, overlay.RateLimitRPS)
	}
	if loaded.RateLimitBurst != overlay.RateLimitBurst {
		t.Errorf("RateLimitBurst = %v, want %v", loaded.RateLimitBurst, overlay.RateLimitBurst)
	}
	if loaded.SendMessageTimeout != overlay.SendMessageTimeout {
		t.Errorf("SendMessageTimeout = %v, want %v", loaded.SendMessageTimeout, overlay.SendMessageTimeout)
	}
	if loaded.SSEKeepalive != overlay.SSEKeepalive {
		t.Errorf("SSEKeepalive = %v, want %v", loaded.SSEKeepalive, overlay.SSEKeepalive)
	}
	if loaded.PushRetryMax != overlay.PushRetryMax {
		t.Errorf("PushRetryMax = %v, want %v", loaded.PushRetryMax, overlay.PushRetryMax)
	}
	if loaded.ProviderOrg != overlay.ProviderOrg {
		t.Errorf("ProviderOrg = %q, want %q", loaded.ProviderOrg, overlay.ProviderOrg)
	}
	if loaded.ProviderURL != overlay.ProviderURL {
		t.Errorf("ProviderURL = %q, want %q", loaded.ProviderURL, overlay.ProviderURL)
	}
	if len(loaded.Projects) != 1 {
		t.Fatalf("Projects = %d, want 1", len(loaded.Projects))
	}
	if loaded.Projects[0].Slug != "p1" {
		t.Errorf("Projects[0].Slug = %q, want %q", loaded.Projects[0].Slug, "p1")
	}

	// Verify present keys are restored (except api_key).
	if loaded.IsPresent("external_url") != true {
		t.Error("expected external_url to be present")
	}
	if loaded.IsPresent("auth_scheme") != true {
		t.Error("expected auth_scheme to be present")
	}
}

func TestPersistAndLoadOverlay_SentinelPreservation(t *testing.T) {
	// Regression: sentinel values (-1) for RateLimitBurst and PushRetryMax must
	// survive a persist→load round-trip. Previously, -1 was skipped during
	// persist (correct), but the zero value 0 was serialized instead, and on
	// load 0 replaced the sentinel — causing ApplyOverlay to override the base
	// YAML value with 0.
	dir := t.TempDir()
	overlay := &AdminOverlay{
		ExternalURL:    "https://example.com",
		RateLimitBurst: -1, // sentinel: not set
		PushRetryMax:   -1, // sentinel: not set
		presentKeys: map[string]bool{
			"external_url": true,
		},
	}

	if err := PersistOverlay(dir, overlay); err != nil {
		t.Fatalf("PersistOverlay: %v", err)
	}

	// Verify the JSON does not contain the sentinel fields.
	data, err := os.ReadFile(filepath.Join(dir, overlayFileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := raw["rate_limit_burst"]; ok {
		t.Error("rate_limit_burst should NOT be present in JSON when sentinel")
	}
	if _, ok := raw["push_retry_max"]; ok {
		t.Error("push_retry_max should NOT be present in JSON when sentinel")
	}

	// Load and verify sentinels are restored.
	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded.RateLimitBurst != -1 {
		t.Errorf("loaded RateLimitBurst = %d, want -1 (sentinel)", loaded.RateLimitBurst)
	}
	if loaded.PushRetryMax != -1 {
		t.Errorf("loaded PushRetryMax = %d, want -1 (sentinel)", loaded.PushRetryMax)
	}

	// Verify ApplyOverlay does NOT override base values when sentinels are preserved.
	base := Config{
		RateLimit: RateLimitConfig{Burst: 42},
		Timeouts:  TimeoutConfig{PushRetryMax: 7},
	}
	result := ApplyOverlay(base, loaded, discardLogger(), nil)
	if result.RateLimit.Burst != 42 {
		t.Errorf("ApplyOverlay burst = %d, want 42 (base preserved)", result.RateLimit.Burst)
	}
	if result.Timeouts.PushRetryMax != 7 {
		t.Errorf("ApplyOverlay push_retry_max = %d, want 7 (base preserved)", result.Timeouts.PushRetryMax)
	}
}

func TestPersistOverlay_APIKeyNotPersisted(t *testing.T) {
	dir := t.TempDir()
	overlay := &AdminOverlay{
		APIKey:      "super-secret",
		presentKeys: map[string]bool{"api_key": true},
	}

	if err := PersistOverlay(dir, overlay); err != nil {
		t.Fatalf("PersistOverlay: %v", err)
	}

	// Read the raw JSON to verify api_key is not present.
	data, err := os.ReadFile(filepath.Join(dir, overlayFileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if _, ok := raw["api_key"]; ok {
		t.Error("api_key should NOT be present in persisted overlay")
	}

	// api_key should also not be in present_keys.
	if keys, ok := raw["present_keys"]; ok {
		for _, k := range keys.([]interface{}) {
			if k.(string) == "api_key" {
				t.Error("api_key should NOT be in present_keys")
			}
		}
	}

	// Load and verify APIKey is empty.
	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded.APIKey != "" {
		t.Errorf("loaded APIKey = %q, want empty", loaded.APIKey)
	}
}

func TestLoadPersistedOverlay_NoFile(t *testing.T) {
	dir := t.TempDir()
	overlay, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overlay != nil {
		t.Error("expected nil overlay when no file exists")
	}
}

func TestApplyOverlay_Precedence(t *testing.T) {
	base := Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://base.example.com",
			Provider:    ProviderConfig{Organization: "Base Org", URL: "https://base.com"},
		},
		Auth: AuthConfig{
			Scheme: "apiKey",
			APIKey: "base-key",
		},
		RateLimit: RateLimitConfig{
			Enabled:        false,
			RequestsPerSec: 5,
			Burst:          10,
		},
		Timeouts: TimeoutConfig{
			SendMessage:  60 * time.Second,
			SSEKeepalive: 15 * time.Second,
			PushRetryMax: 2,
		},
		Projects: []ProjectConfig{
			{Slug: "base-proj"},
		},
	}

	rlEnabled := true
	overlay := &AdminOverlay{
		ExternalURL:        "https://overlay.example.com",
		AuthScheme:         "hubUAT",
		RateLimitEnabled:   &rlEnabled,
		RateLimitRPS:       25,
		RateLimitBurst:     -1, // not set — keep base
		SendMessageTimeout: 90 * time.Second,
		PushRetryMax:       -1, // not set — keep base
		ProviderOrg:        "Overlay Org",
		presentKeys: map[string]bool{
			"external_url":         true,
			"auth_scheme":          true,
			"rate_limit_enabled":   true,
			"rate_limit_rps":       true,
			"send_message_timeout": true,
			"provider_org":         true,
		},
	}

	result := ApplyOverlay(base, overlay, discardLogger(), nil)

	// Overlay wins for present keys.
	if result.Bridge.ExternalURL != "https://overlay.example.com" {
		t.Errorf("ExternalURL = %q, want overlay value", result.Bridge.ExternalURL)
	}
	if result.Auth.Scheme != "hubUAT" {
		t.Errorf("Auth.Scheme = %q, want overlay value", result.Auth.Scheme)
	}
	if !result.RateLimit.Enabled {
		t.Error("RateLimit.Enabled should be true (overlay)")
	}
	if result.RateLimit.RequestsPerSec != 25 {
		t.Errorf("RateLimit.RequestsPerSec = %v, want 25 (overlay)", result.RateLimit.RequestsPerSec)
	}
	if result.Timeouts.SendMessage != 90*time.Second {
		t.Errorf("Timeouts.SendMessage = %v, want 90s (overlay)", result.Timeouts.SendMessage)
	}
	if result.Bridge.Provider.Organization != "Overlay Org" {
		t.Errorf("Provider.Organization = %q, want overlay value", result.Bridge.Provider.Organization)
	}

	// Base wins for absent overlay keys.
	if result.Auth.APIKey != "base-key" {
		t.Errorf("Auth.APIKey = %q, want base value", result.Auth.APIKey)
	}
	if result.RateLimit.Burst != 10 {
		t.Errorf("RateLimit.Burst = %d, want 10 (base, overlay sentinel -1)", result.RateLimit.Burst)
	}
	if result.Timeouts.SSEKeepalive != 15*time.Second {
		t.Errorf("Timeouts.SSEKeepalive = %v, want 15s (base)", result.Timeouts.SSEKeepalive)
	}
	if result.Timeouts.PushRetryMax != 2 {
		t.Errorf("Timeouts.PushRetryMax = %d, want 2 (base)", result.Timeouts.PushRetryMax)
	}
	if result.Bridge.Provider.URL != "https://base.com" {
		t.Errorf("Provider.URL = %q, want base value", result.Bridge.Provider.URL)
	}
	if len(result.Projects) != 1 || result.Projects[0].Slug != "base-proj" {
		t.Error("Projects should be base value (overlay absent)")
	}
}

func TestApplyOverlay_NilOverlay(t *testing.T) {
	base := Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
	}

	result := ApplyOverlay(base, nil, discardLogger(), nil)
	if result.Bridge.ExternalURL != "https://base.example.com" {
		t.Errorf("ExternalURL = %q, want base value", result.Bridge.ExternalURL)
	}
}

// TestEffectiveAuthScheme exercises the pure pinning decision directly.
func TestEffectiveAuthScheme(t *testing.T) {
	tests := []struct {
		name       string
		yamlScheme string
		current    string
		pushed     string
		want       string
		wantWarn   bool
	}{
		{"pinned hubBearer scheme, none push is ignored", "hubBearer", "hubBearer", "none", "hubBearer", true},
		{"pinned geGoogle scheme, none push is ignored", "geGoogle", "geGoogle", "none", "geGoogle", true},
		{"pinned federation scheme, none push is ignored (not dropdown-representable despite being a valid scheme)", "federation", "federation", "none", "federation", true},
		{"pinned scheme, empty push keeps current, no warning", "hubBearer", "hubBearer", "", "hubBearer", false},
		{"pinned scheme, push matches yaml exactly: no-op, no warning", "hubBearer", "hubBearer", "hubBearer", "hubBearer", false},
		{"UI-representable yaml scheme is not pinned", "apiKey", "apiKey", "none", "none", false},
		{"no yaml scheme captured (admin-only config): push always wins", "", "none", "hubUAT", "hubUAT", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))

			got := EffectiveAuthScheme(tt.yamlScheme, tt.current, tt.pushed, log, nil)
			if got != tt.want {
				t.Errorf("EffectiveAuthScheme(%q, %q, %q) = %q, want %q", tt.yamlScheme, tt.current, tt.pushed, got, tt.want)
			}

			logged := buf.Len() > 0
			if logged != tt.wantWarn {
				t.Errorf("warning logged = %v, want %v (log: %q)", logged, tt.wantWarn, buf.String())
			}
			if tt.wantWarn {
				out := buf.String()
				if !strings.Contains(out, "level=WARN") {
					t.Errorf("expected a Warn-level log, got: %q", out)
				}
				if !strings.Contains(out, tt.yamlScheme) || !strings.Contains(out, tt.pushed) {
					t.Errorf("warning must name both schemes, got: %q", out)
				}
			}
		})
	}
}

// TestEffectiveAuthScheme_NilLoggerDoesNotPanic: a nil logger falls back to
// slog.Default() instead of panicking on the pinned path.
func TestEffectiveAuthScheme_NilLoggerDoesNotPanic(t *testing.T) {
	got := EffectiveAuthScheme("hubBearer", "hubBearer", "none", nil, nil)
	if got != "hubBearer" {
		t.Errorf("got %q, want %q", got, "hubBearer")
	}
}

// TestAuthSchemeWarnDedupe: two pushes ignored for the same (yaml, pushed)
// pair log one Warn followed by Debug on repeats; a different pushed value
// for the same YAML scheme is a new pair and logs its own Warn.
func TestAuthSchemeWarnDedupe(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dedupe := NewAuthSchemeWarnDedupe()

	EffectiveAuthScheme("hubBearer", "hubBearer", "none", log, dedupe)
	EffectiveAuthScheme("hubBearer", "hubBearer", "none", log, dedupe)
	EffectiveAuthScheme("hubBearer", "hubBearer", "bearer", log, dedupe)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "pushed_scheme=none") {
		t.Errorf("line 1 = %q, want a Warn for pushed_scheme=none", lines[0])
	}
	if !strings.Contains(lines[1], "level=DEBUG") || !strings.Contains(lines[1], "pushed_scheme=none") {
		t.Errorf("line 2 (repeat of the same pair) = %q, want a Debug for pushed_scheme=none", lines[1])
	}
	if !strings.Contains(lines[2], "level=WARN") || !strings.Contains(lines[2], "pushed_scheme=bearer") {
		t.Errorf("line 3 (a new pushed value) = %q, want a Warn for pushed_scheme=bearer", lines[2])
	}
}

// TestApplyOverlay_PinsYAMLOnlyScheme: YAML hubBearer, overlay
// auth_scheme: none -> effective hubBearer, with a warning logged.
func TestApplyOverlay_PinsYAMLOnlyScheme(t *testing.T) {
	base := Config{
		Hub:  HubConfig{Endpoint: "https://hub.example.com"},
		Auth: AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	result := ApplyOverlay(base, overlay, log, nil)

	if result.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned)", result.Auth.Scheme, "hubBearer")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a warning to be logged, got: %q", buf.String())
	}
}

// TestApplyOverlay_GeGoogleSchemeAlsoPinned: the pin is not hubBearer-
// specific, it applies to any YAML scheme the admin UI cannot express.
func TestApplyOverlay_GeGoogleSchemeAlsoPinned(t *testing.T) {
	base := Config{
		Hub:  HubConfig{Endpoint: "https://hub.example.com"},
		Auth: AuthConfig{Scheme: "geGoogle", YAMLScheme: "geGoogle"},
	}
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}
	result := ApplyOverlay(base, overlay, discardLogger(), nil)
	if result.Auth.Scheme != "geGoogle" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned)", result.Auth.Scheme, "geGoogle")
	}
}

// TestApplyOverlay_FederationSchemeAlsoPinned: federation is a valid
// ParseAdminOverlay scheme (validAuthSchemes), but the admin dropdown
// (uiRepresentableSchemes) cannot express it either, so it must be pinned
// just like hubBearer/geGoogle.
func TestApplyOverlay_FederationSchemeAlsoPinned(t *testing.T) {
	base := Config{
		Hub:  HubConfig{Endpoint: "https://hub.example.com"},
		Auth: AuthConfig{Scheme: "federation", YAMLScheme: "federation"},
	}
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}
	result := ApplyOverlay(base, overlay, discardLogger(), nil)
	if result.Auth.Scheme != "federation" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned: federation is not in uiRepresentableSchemes)", result.Auth.Scheme, "federation")
	}
}

// TestApplyOverlay_DoesNotPinUIRepresentableScheme: a YAML scheme the admin
// UI can express (apiKey) is not pinned, guarding against an over-broad pin
// that ignores every push regardless of uiRepresentableSchemes.
func TestApplyOverlay_DoesNotPinUIRepresentableScheme(t *testing.T) {
	base := Config{
		Auth: AuthConfig{Scheme: "apiKey", YAMLScheme: "apiKey", APIKey: "k"},
	}
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	result := ApplyOverlay(base, overlay, log, nil)

	if result.Auth.Scheme != "none" {
		t.Errorf("Auth.Scheme = %q, want %q (not pinned, apiKey is UI-representable)", result.Auth.Scheme, "none")
	}
	if buf.Len() > 0 {
		t.Errorf("expected no warning for a UI-representable YAML scheme, got: %q", buf.String())
	}
}

// TestApplyOverlay_UsesCapturedYAMLScheme_NotCurrent proves ApplyOverlay
// pins against the captured base.Auth.YAMLScheme, not against whatever the
// live/current scheme happens to be. base.Auth.Scheme is deliberately set to
// a different, UI-representable value than base.Auth.YAMLScheme: a
// mutant that passed cfg.Auth.Scheme (or base.Auth.Scheme) instead of
// base.Auth.YAMLScheme to EffectiveAuthScheme would see "apiKey" (matches
// validAuthSchemes) and fail to pin, letting the push through as "none".
func TestApplyOverlay_UsesCapturedYAMLScheme_NotCurrent(t *testing.T) {
	base := Config{
		Hub:  HubConfig{Endpoint: "https://hub.example.com"},
		Auth: AuthConfig{Scheme: "apiKey", YAMLScheme: "hubBearer"},
	}
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}
	result := ApplyOverlay(base, overlay, discardLogger(), nil)
	if result.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (must pin against the captured YAML scheme, not the current live scheme)", result.Auth.Scheme, "hubBearer")
	}
}

// TestApplyOverlay_PersistedOverlayCannotUnpinYAMLScheme covers bridge
// startup with a persisted overlay containing auth_scheme: none: the exact
// sequence cmd/scion-a2a-bridge's main() runs at boot
// (LoadPersistedOverlay -> ApplyOverlay), reproduced here against the
// bridge package's own primitives.
func TestApplyOverlay_PersistedOverlayCannotUnpinYAMLScheme(t *testing.T) {
	dir := t.TempDir()
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}
	if err := PersistOverlay(dir, overlay); err != nil {
		t.Fatalf("PersistOverlay: %v", err)
	}

	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected a persisted overlay to load")
	}

	base := Config{
		Hub:  HubConfig{Endpoint: "https://hub.example.com"},
		Auth: AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	effective := ApplyOverlay(base, loaded, discardLogger(), nil)
	if effective.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (a persisted none overlay must not unpin a YAML-only scheme at boot)", effective.Auth.Scheme, "hubBearer")
	}
}

// TestApplyOverlay_PinnedSchemeSurvivesBuildSnapshot: after a pinned push,
// the production snapshot/validator wiring (BuildSnapshot ->
// BuildAuthValidators), not just the raw Config field, must reflect the
// pinned scheme.
func TestApplyOverlay_PinnedSchemeSurvivesBuildSnapshot(t *testing.T) {
	base := Config{
		Hub:  HubConfig{Endpoint: "https://hub.example.com"},
		Auth: AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	overlay := &AdminOverlay{
		AuthScheme:  "none",
		presentKeys: map[string]bool{"auth_scheme": true},
	}
	effective := ApplyOverlay(base, overlay, discardLogger(), nil)

	snap := BuildSnapshot(effective)
	if snap.Auth.Scheme != "hubBearer" {
		t.Errorf("snapshot Auth.Scheme = %q, want %q", snap.Auth.Scheme, "hubBearer")
	}
	if snap.Auth.UATValidator == nil {
		t.Error("snapshot UATValidator is nil, want non-nil: production wiring must reflect the pinned hubBearer scheme, not just the Config field")
	}
}

// TestConfigure_PinsYAMLOnlyScheme covers the Hub-driven Restart/Configure
// push path end to end: BrokerServer.Configure -> ApplyOverlay -> snapshot.
// This is the real production route for both the non-HA admin-UI Reconnect
// push and an HA Restart push that carries auth_scheme.
func TestConfigure_PinsYAMLOnlyScheme(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	baseCfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "test-user"},
		Auth:   AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	dir := t.TempDir()
	broker.SetAdminConfig(baseCfg, snap, dir)

	// The admin UI's dropdown default: this is exactly what a routine save
	// (or an HA Restart push carrying the integration's stored settings)
	// sends.
	pushedConfig := map[string]string{
		"auth_scheme": "none",
	}
	if err := broker.Configure(pushedConfig); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	current := snap.Load()
	if current.Config.Auth.Scheme != "hubBearer" {
		t.Errorf("Config.Auth.Scheme = %q, want %q (pinned)", current.Config.Auth.Scheme, "hubBearer")
	}
	if current.Auth.Scheme != "hubBearer" {
		t.Errorf("snapshot Auth.Scheme = %q, want %q (pinned)", current.Auth.Scheme, "hubBearer")
	}
	if current.Auth.UATValidator == nil {
		t.Error("snapshot UATValidator is nil, want non-nil: production wiring must reflect the pinned hubBearer scheme")
	}

	// The persisted overlay itself still records the operator's raw push
	// (so the admin UI shows what was actually sent); it is the effective
	// config, not the persisted overlay, that is pinned. A restart re-runs
	// ApplyOverlay against this same persisted overlay and base YAML, and
	// pins again — the persisted overlay can never durably unpin a YAML-only
	// scheme.
	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded == nil || loaded.AuthScheme != "none" {
		t.Fatalf("persisted overlay AuthScheme = %+v, want \"none\"", loaded)
	}
	reapplied := ApplyOverlay(*baseCfg, loaded, discardLogger(), nil)
	if reapplied.Auth.Scheme != "hubBearer" {
		t.Errorf("re-applying the persisted overlay after a restart gave Auth.Scheme = %q, want %q (still pinned)", reapplied.Auth.Scheme, "hubBearer")
	}
}

// TestConfigure_DedupesWarningAcrossConfigurePushes covers the dedupe's
// per-BrokerServer ownership and lifetime: pushing the same ignored
// auth_scheme through Configure twice must not double the Warn, because
// BrokerServer keeps one dedupe instance for its whole lifetime rather than
// creating a new one per push. Each Configure call itself runs ApplyOverlay
// twice (once to validate, once to apply the snapshot), so the first push
// alone already produces a Warn plus a Debug; three Debug lines total are
// therefore expected across the two pushes in this trace.
func TestConfigure_DedupesWarningAcrossConfigurePushes(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	broker := NewBrokerServer(nil, log, nil)
	baseCfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "test-user"},
		Auth:   AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	broker.SetAdminConfig(baseCfg, snap, "")

	pushedConfig := map[string]string{"auth_scheme": "none"}
	if err := broker.Configure(pushedConfig); err != nil {
		t.Fatalf("first Configure: %v", err)
	}
	if err := broker.Configure(pushedConfig); err != nil {
		t.Fatalf("second Configure: %v", err)
	}

	out := buf.String()
	if warnCount := strings.Count(out, "level=WARN"); warnCount != 1 {
		t.Errorf("got %d Warn line(s) across two identical Configure pushes, want 1 (the dedupe must be shared across pushes, not reset per Configure call):\n%s", warnCount, out)
	}
	if debugCount := strings.Count(out, "level=DEBUG"); debugCount != 3 {
		t.Errorf("got %d Debug line(s), want 3 (each Configure call runs ApplyOverlay twice, so push 1 yields Warn+Debug and push 2 yields Debug+Debug):\n%s", debugCount, out)
	}
}

func TestBuildSnapshot_RateLimitDisabled(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{Enabled: false},
	}
	snap := BuildSnapshot(cfg)
	if snap.Limiter != nil {
		t.Error("limiter should be nil when rate limiting is disabled")
	}
}

func TestBuildSnapshot_RateLimitEnabled(t *testing.T) {
	cfg := Config{
		RateLimit: RateLimitConfig{Enabled: true, RequestsPerSec: 10, Burst: 20},
	}
	snap := BuildSnapshot(cfg)
	if snap.Limiter == nil {
		t.Error("limiter should not be nil when rate limiting is enabled")
	}
}

func TestBuildAuthValidators_Schemes(t *testing.T) {
	tests := []struct {
		scheme    string
		expectUAT bool
		expectKey bool
	}{
		{"apiKey", false, true},
		{"bearer", false, true},
		{"none", false, false},
		{"hubUAT", true, false},
		{"hubBearer", true, false},
		{"hubJWT", false, false},
		{"", false, true}, // default legacy scheme
	}

	for _, tt := range tests {
		cfg := &Config{
			Auth: AuthConfig{
				Scheme: tt.scheme,
				APIKey: "test-key",
			},
			Hub: HubConfig{Endpoint: "https://hub.example.com"},
		}
		av := BuildAuthValidators(cfg)
		if av.Scheme != tt.scheme {
			t.Errorf("scheme %q: Scheme = %q", tt.scheme, av.Scheme)
		}
		if (av.UATValidator != nil) != tt.expectUAT {
			t.Errorf("scheme %q: UATValidator present = %v, want %v", tt.scheme, av.UATValidator != nil, tt.expectUAT)
		}
		if (av.APIKey != "") != tt.expectKey {
			t.Errorf("scheme %q: APIKey present = %v, want %v", tt.scheme, av.APIKey != "", tt.expectKey)
		}
	}
}

func TestSnapshotHolder_AtomicSwap(t *testing.T) {
	cfg1 := Config{Bridge: BridgeConfig{ExternalURL: "https://v1.example.com"}}
	cfg2 := Config{Bridge: BridgeConfig{ExternalURL: "https://v2.example.com"}}

	snap1 := BuildSnapshot(cfg1)
	holder := NewSnapshotHolder(snap1)

	if got := holder.Load().Config.Bridge.ExternalURL; got != "https://v1.example.com" {
		t.Errorf("initial Load = %q, want v1", got)
	}

	snap2 := BuildSnapshot(cfg2)
	holder.Store(snap2)

	if got := holder.Load().Config.Bridge.ExternalURL; got != "https://v2.example.com" {
		t.Errorf("after Store = %q, want v2", got)
	}
}

func TestConfigure_RejectsBadConfig(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	baseCfg := &Config{}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	broker.SetAdminConfig(baseCfg, snap, "")

	badConfig := map[string]string{
		"auth_scheme": "bogus",
	}
	err := broker.Configure(badConfig)
	if err == nil {
		t.Fatal("expected error for invalid config")
	}

	// Verify the snapshot was NOT updated (last-good preserved).
	if snap.Load().Config.Auth.Scheme != "" {
		t.Errorf("snapshot auth scheme should remain empty after rejected push")
	}
}

func TestConfigure_AppliesGoodConfig(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	baseCfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "test-user"},
		Auth:   AuthConfig{Scheme: "none"},
	}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	dir := t.TempDir()
	broker.SetAdminConfig(baseCfg, snap, dir)

	goodConfig := map[string]string{
		"external_url": "https://overlay.example.com",
		"auth_scheme":  "apiKey",
		"api_key":      "new-key",
	}
	err := broker.Configure(goodConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the snapshot was updated.
	current := snap.Load()
	if current.Config.Bridge.ExternalURL != "https://overlay.example.com" {
		t.Errorf("ExternalURL = %q, want overlay value", current.Config.Bridge.ExternalURL)
	}
	if current.Config.Auth.Scheme != "apiKey" {
		t.Errorf("Auth.Scheme = %q, want apiKey", current.Config.Auth.Scheme)
	}
	if current.Auth.APIKey != "new-key" {
		t.Errorf("Auth.APIKey = %q, want new-key", current.Auth.APIKey)
	}

	// Verify overlay was persisted.
	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected persisted overlay")
	}
	if loaded.ExternalURL != "https://overlay.example.com" {
		t.Errorf("persisted ExternalURL = %q, want overlay value", loaded.ExternalURL)
	}
	// API key should NOT be in persisted overlay.
	if loaded.APIKey != "" {
		t.Errorf("persisted APIKey = %q, want empty", loaded.APIKey)
	}
}

func TestConfigure_KeepsLastGoodOnReject(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	baseCfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "test-user"},
		Auth:   AuthConfig{Scheme: "none"},
	}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	broker.SetAdminConfig(baseCfg, snap, "")

	// Apply good config first.
	goodConfig := map[string]string{
		"auth_scheme": "apiKey",
		"api_key":     "key1",
	}
	if err := broker.Configure(goodConfig); err != nil {
		t.Fatalf("first configure failed: %v", err)
	}
	if snap.Load().Config.Auth.Scheme != "apiKey" {
		t.Fatal("expected apiKey after first configure")
	}

	// Now push bad config.
	badConfig := map[string]string{
		"auth_scheme": "invalid",
	}
	err := broker.Configure(badConfig)
	if err == nil {
		t.Fatal("expected error for invalid config")
	}

	// Verify last-good is preserved.
	if snap.Load().Config.Auth.Scheme != "apiKey" {
		t.Errorf("auth scheme should remain apiKey after rejected push, got %q", snap.Load().Config.Auth.Scheme)
	}
}

func TestConfigure_NoAdminConfigWired(t *testing.T) {
	// When admin config management is not wired, Configure is a no-op.
	broker := NewBrokerServer(nil, discardLogger(), nil)

	err := broker.Configure(map[string]string{"auth_scheme": "none"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthCheck_EnrichedDetails(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	cfg := Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://a2a.example.com",
			Provider:    ProviderConfig{Organization: "Test Org"},
		},
		Auth: AuthConfig{Scheme: "hubUAT"},
		Projects: []ProjectConfig{
			{Slug: "p1"},
			{Slug: "p2"},
		},
	}
	snap := NewSnapshotHolder(BuildSnapshot(cfg))
	broker.SetAdminConfig(&cfg, snap, "")
	broker.configured = true

	hs, err := broker.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if hs.Status != "healthy" {
		t.Errorf("Status = %q, want healthy", hs.Status)
	}
	if hs.Details["auth_scheme"] != "hubUAT" {
		t.Errorf("Details[auth_scheme] = %q, want hubUAT", hs.Details["auth_scheme"])
	}
	if hs.Details["external_url"] != "https://a2a.example.com" {
		t.Errorf("Details[external_url] = %q, want https://a2a.example.com", hs.Details["external_url"])
	}
	if hs.Details["exposed_projects"] != "2" {
		t.Errorf("Details[exposed_projects] = %q, want 2", hs.Details["exposed_projects"])
	}
	if hs.Details["provider_org"] != "Test Org" {
		t.Errorf("Details[provider_org] = %q, want Test Org", hs.Details["provider_org"])
	}
}

func TestParseAdminOverlay_EmptyNonStringFieldsNotPresent(t *testing.T) {
	// Regression: empty string values for non-string fields (durations, ints,
	// bools, projects_json) must NOT be marked as present — otherwise
	// ApplyOverlay overwrites base YAML values with zero values.
	cfg := map[string]string{
		"uat_cache_ttl":        "",
		"send_message_timeout": "",
		"sse_keepalive":        "",
		"rate_limit_enabled":   "",
		"rate_limit_rps":       "",
		"rate_limit_burst":     "",
		"push_retry_max":       "",
		"projects_json":        "",
	}

	overlay, err := ParseAdminOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nonStringKeys := []string{
		"uat_cache_ttl", "send_message_timeout", "sse_keepalive",
		"rate_limit_enabled", "rate_limit_rps", "rate_limit_burst",
		"push_retry_max", "projects_json",
	}
	for _, key := range nonStringKeys {
		if overlay.IsPresent(key) {
			t.Errorf("%q should NOT be marked as present when value is empty", key)
		}
	}

	// Verify ApplyOverlay does not override base values.
	base := Config{
		Auth:     AuthConfig{UATCacheTTL: 90 * time.Second},
		Timeouts: TimeoutConfig{SendMessage: 60 * time.Second, SSEKeepalive: 15 * time.Second, PushRetryMax: 5},
	}
	result := ApplyOverlay(base, overlay, discardLogger(), nil)
	if result.Auth.UATCacheTTL != 90*time.Second {
		t.Errorf("UATCacheTTL = %v, want 90s (base preserved)", result.Auth.UATCacheTTL)
	}
	if result.Timeouts.SendMessage != 60*time.Second {
		t.Errorf("SendMessage = %v, want 60s (base preserved)", result.Timeouts.SendMessage)
	}
	if result.Timeouts.SSEKeepalive != 15*time.Second {
		t.Errorf("SSEKeepalive = %v, want 15s (base preserved)", result.Timeouts.SSEKeepalive)
	}
	if result.Timeouts.PushRetryMax != 5 {
		t.Errorf("PushRetryMax = %d, want 5 (base preserved)", result.Timeouts.PushRetryMax)
	}
}

func TestParseAdminOverlay_EmptyStringFieldsStillPresent(t *testing.T) {
	// String fields (external_url, auth_scheme, api_key, provider_org,
	// provider_url) can be explicitly cleared by setting them to empty. They
	// should still be marked as present so ApplyOverlay writes the empty
	// value. auth_scheme is the one exception: EffectiveAuthScheme treats an
	// empty pushed scheme as "no scheme in this push" and keeps the current
	// scheme rather than clearing it (see TestEffectiveAuthScheme).
	cfg := map[string]string{
		"external_url": "",
		"auth_scheme":  "",
		"api_key":      "",
		"provider_org": "",
		"provider_url": "",
	}

	overlay, err := ParseAdminOverlay(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stringKeys := []string{"external_url", "auth_scheme", "api_key", "provider_org", "provider_url"}
	for _, key := range stringKeys {
		if !overlay.IsPresent(key) {
			t.Errorf("%q should be marked as present even when value is empty", key)
		}
	}

	// Verify ApplyOverlay clears base values, except auth_scheme.
	base := Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://base.example.com",
			Provider:    ProviderConfig{Organization: "Base Org", URL: "https://base.com"},
		},
		Auth: AuthConfig{
			Scheme: "apiKey",
			APIKey: "base-key",
		},
	}
	result := ApplyOverlay(base, overlay, discardLogger(), nil)
	if result.Bridge.ExternalURL != "" {
		t.Errorf("ExternalURL = %q, want empty (cleared)", result.Bridge.ExternalURL)
	}
	if result.Auth.Scheme != "apiKey" {
		t.Errorf("Auth.Scheme = %q, want %q (an empty push keeps the current scheme, it does not clear it)", result.Auth.Scheme, "apiKey")
	}
	if result.Auth.APIKey != "" {
		t.Errorf("Auth.APIKey = %q, want empty (cleared)", result.Auth.APIKey)
	}
	if result.Bridge.Provider.Organization != "" {
		t.Errorf("Provider.Organization = %q, want empty (cleared)", result.Bridge.Provider.Organization)
	}
	if result.Bridge.Provider.URL != "" {
		t.Errorf("Provider.URL = %q, want empty (cleared)", result.Bridge.Provider.URL)
	}
}

func TestConfigure_ProjectsJSONRoundTrip(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	baseCfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "test-user"},
		Auth:   AuthConfig{Scheme: "none"},
	}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	dir := t.TempDir()
	broker.SetAdminConfig(baseCfg, snap, dir)

	// Step 1: push 2 projects.
	twoProjects := `[
		{"slug":"proj-alpha","default_template":"default","auto_provision":false,"exposed_agents":["agent1","agent2"]},
		{"slug":"proj-beta","default_template":"custom","auto_provision":true,"exposed_agents":[]}
	]`
	err := broker.Configure(map[string]string{
		"projects_json": twoProjects,
	})
	if err != nil {
		t.Fatalf("Configure with 2 projects: %v", err)
	}

	current := snap.Load()
	if len(current.Config.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(current.Config.Projects))
	}
	if current.Config.Projects[0].Slug != "proj-alpha" {
		t.Errorf("Projects[0].Slug = %q, want proj-alpha", current.Config.Projects[0].Slug)
	}
	if len(current.Config.Projects[0].ExposedAgents) != 2 {
		t.Errorf("Projects[0].ExposedAgents = %d, want 2", len(current.Config.Projects[0].ExposedAgents))
	}
	if current.Config.Projects[1].Slug != "proj-beta" {
		t.Errorf("Projects[1].Slug = %q, want proj-beta", current.Config.Projects[1].Slug)
	}
	if !current.Config.Projects[1].AutoProvision {
		t.Error("Projects[1].AutoProvision should be true")
	}
	if current.Config.Projects[1].DefaultTemplate != "custom" {
		t.Errorf("Projects[1].DefaultTemplate = %q, want custom", current.Config.Projects[1].DefaultTemplate)
	}

	// Step 2: push again with 1 project removed.
	oneProject := `[{"slug":"proj-alpha","default_template":"default","auto_provision":false,"exposed_agents":["agent1"]}]`
	err = broker.Configure(map[string]string{
		"projects_json": oneProject,
	})
	if err != nil {
		t.Fatalf("Configure with 1 project: %v", err)
	}

	current = snap.Load()
	if len(current.Config.Projects) != 1 {
		t.Fatalf("expected 1 project after update, got %d", len(current.Config.Projects))
	}
	if current.Config.Projects[0].Slug != "proj-alpha" {
		t.Errorf("Projects[0].Slug = %q, want proj-alpha", current.Config.Projects[0].Slug)
	}
	if len(current.Config.Projects[0].ExposedAgents) != 1 {
		t.Errorf("Projects[0].ExposedAgents = %d, want 1", len(current.Config.Projects[0].ExposedAgents))
	}

	// Verify persistence: the overlay file should reflect the latest state.
	loaded, err := LoadPersistedOverlay(dir)
	if err != nil {
		t.Fatalf("LoadPersistedOverlay: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected persisted overlay")
	}
	if len(loaded.Projects) != 1 {
		t.Fatalf("persisted Projects = %d, want 1", len(loaded.Projects))
	}
	if loaded.Projects[0].Slug != "proj-alpha" {
		t.Errorf("persisted Projects[0].Slug = %q, want proj-alpha", loaded.Projects[0].Slug)
	}
}

func TestConfigure_EmptyProjectsJSON(t *testing.T) {
	broker := NewBrokerServer(nil, discardLogger(), nil)
	baseCfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://base.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "test-user"},
		Auth:   AuthConfig{Scheme: "none"},
		Projects: []ProjectConfig{
			{Slug: "base-proj"},
		},
	}
	snap := NewSnapshotHolder(BuildSnapshot(*baseCfg))
	dir := t.TempDir()
	broker.SetAdminConfig(baseCfg, snap, dir)

	// Push empty projects_json → clears projects.
	err := broker.Configure(map[string]string{
		"projects_json": "[]",
	})
	if err != nil {
		t.Fatalf("Configure with empty projects: %v", err)
	}

	current := snap.Load()
	// Empty JSON array results in empty overlay Projects slice.
	// ApplyOverlay requires the key to be present AND overlay.Projects non-nil.
	// With "[]", ParseAdminOverlay sets overlay.Projects to an empty (non-nil) slice,
	// and the key is present, so the base projects are replaced.
	if len(current.Config.Projects) != 0 {
		t.Errorf("expected 0 projects after empty push, got %d", len(current.Config.Projects))
	}
}

func discardLogger() *slog.Logger {
	return slog.Default()
}
