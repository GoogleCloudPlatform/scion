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

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLoadConfig_CapturesYAMLScheme proves loadConfig captures Auth.YAMLScheme
// from a real config file — not from a hand-built Config, which every other
// test in this package and internal/bridge uses — and that the captured
// value actually protects against a downgrade through ApplyOverlay, the same
// path main() runs at boot.
func TestLoadConfig_CapturesYAMLScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlContent := `
bridge:
  listen_address: ":8443"
  external_url: "https://a2a.example.com"
hub:
  endpoint: "https://hub.example.com"
  user: "a2a-bridge@example.com"
plugin:
  listen_address: "localhost:9090"
auth:
  scheme: "hubBearer"
projects:
  - slug: "my-project"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Auth.Scheme != "hubBearer" {
		t.Fatalf("Auth.Scheme = %q, want %q", cfg.Auth.Scheme, "hubBearer")
	}
	if cfg.Auth.YAMLScheme != "hubBearer" {
		t.Fatalf("Auth.YAMLScheme = %q, want %q", cfg.Auth.YAMLScheme, "hubBearer")
	}

	overlay, err := bridge.ParseAdminOverlay(map[string]string{"auth_scheme": "none"})
	if err != nil {
		t.Fatalf("ParseAdminOverlay: %v", err)
	}
	effective := bridge.ApplyOverlay(*cfg, overlay, discardLogger(), nil)
	if effective.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme after ApplyOverlay = %q, want %q (loadConfig's capture must survive into ApplyOverlay)", effective.Auth.Scheme, "hubBearer")
	}
}

// TestApplyRuntimeConfig_PinsYAMLOnlyScheme: YAML hubBearer,
// applyRuntimeConfig({"auth_scheme":"none"}) -> hubBearer.
func TestApplyRuntimeConfig_PinsYAMLOnlyScheme(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger(), nil)
	if cfg.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned)", cfg.Auth.Scheme, "hubBearer")
	}
}

// TestApplyRuntimeConfig_GeGoogleSchemeAlsoPinned covers the
// applyRuntimeConfig half of the same pin for geGoogle.
func TestApplyRuntimeConfig_GeGoogleSchemeAlsoPinned(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "geGoogle", YAMLScheme: "geGoogle"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger(), nil)
	if cfg.Auth.Scheme != "geGoogle" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned)", cfg.Auth.Scheme, "geGoogle")
	}
}

// TestApplyRuntimeConfig_DoesNotPinUIRepresentableScheme guards against a
// mutant that pins every YAML scheme regardless of uiRepresentableSchemes.
func TestApplyRuntimeConfig_DoesNotPinUIRepresentableScheme(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "apiKey", YAMLScheme: "apiKey"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger(), nil)
	if cfg.Auth.Scheme != "none" {
		t.Errorf("Auth.Scheme = %q, want %q (not pinned, apiKey is UI-representable)", cfg.Auth.Scheme, "none")
	}
}

// TestApplyRuntimeConfig_PinSurvivesReconnect: two successive
// applyRuntimeConfig calls simulate a reconnect. The first carries the
// admin-pushed "none"; the second has no auth_scheme key at all (the shape
// of a reconnect that re-reads config after the key was removed
// server-side). Neither may unpin the YAML scheme.
func TestApplyRuntimeConfig_PinSurvivesReconnect(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}

	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger(), nil)
	if cfg.Auth.Scheme != "hubBearer" {
		t.Fatalf("after first push: Auth.Scheme = %q, want %q (pinned)", cfg.Auth.Scheme, "hubBearer")
	}

	// Reconnect: a fresh runtime config read with the key absent entirely
	// (not merely empty) must still leave the pinned scheme alone.
	applyRuntimeConfig(cfg, map[string]string{}, discardLogger(), nil)
	if cfg.Auth.Scheme != "hubBearer" {
		t.Errorf("after reconnect: Auth.Scheme = %q, want %q (still pinned)", cfg.Auth.Scheme, "hubBearer")
	}
}

// TestApplyRuntimeConfig_UsesCapturedYAMLScheme_NotCurrent mirrors
// bridge.TestApplyOverlay_UsesCapturedYAMLScheme_NotCurrent for the
// applyRuntimeConfig call site: it must pin against cfg.Auth.YAMLScheme, not
// against cfg.Auth.Scheme.
func TestApplyRuntimeConfig_UsesCapturedYAMLScheme_NotCurrent(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "apiKey", YAMLScheme: "hubBearer"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger(), nil)
	if cfg.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (must pin against the captured YAML scheme, not the current live scheme)", cfg.Auth.Scheme, "hubBearer")
	}
}
