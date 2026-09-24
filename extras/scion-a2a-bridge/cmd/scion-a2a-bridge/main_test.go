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
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestApplyRuntimeConfig_PinsYAMLOnlyScheme is F4-2: YAML hubBearer,
// applyRuntimeConfig({"auth_scheme":"none"}) -> hubBearer.
func TestApplyRuntimeConfig_PinsYAMLOnlyScheme(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger())
	if cfg.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned)", cfg.Auth.Scheme, "hubBearer")
	}
}

// TestApplyRuntimeConfig_GeGoogleSchemeAlsoPinned is F4-4's applyRuntimeConfig
// half.
func TestApplyRuntimeConfig_GeGoogleSchemeAlsoPinned(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "geGoogle", YAMLScheme: "geGoogle"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger())
	if cfg.Auth.Scheme != "geGoogle" {
		t.Errorf("Auth.Scheme = %q, want %q (pinned)", cfg.Auth.Scheme, "geGoogle")
	}
}

// TestApplyRuntimeConfig_DoesNotPinUIRepresentableScheme is F4-5's
// applyRuntimeConfig half: guards against a mutant that pins every YAML
// scheme regardless of validAuthSchemes.
func TestApplyRuntimeConfig_DoesNotPinUIRepresentableScheme(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "apiKey", YAMLScheme: "apiKey"},
	}
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger())
	if cfg.Auth.Scheme != "none" {
		t.Errorf("Auth.Scheme = %q, want %q (not pinned, apiKey is UI-representable)", cfg.Auth.Scheme, "none")
	}
}

// TestApplyRuntimeConfig_PinSurvivesReconnect is F4-3: two successive
// applyRuntimeConfig calls simulate a reconnect. The first carries the
// admin-pushed "none"; the second has no auth_scheme key at all (the shape
// of a reconnect that re-reads config after the key was removed
// server-side). Neither may unpin the YAML scheme.
func TestApplyRuntimeConfig_PinSurvivesReconnect(t *testing.T) {
	cfg := &bridge.Config{
		Auth: bridge.AuthConfig{Scheme: "hubBearer", YAMLScheme: "hubBearer"},
	}

	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger())
	if cfg.Auth.Scheme != "hubBearer" {
		t.Fatalf("after first push: Auth.Scheme = %q, want %q (pinned)", cfg.Auth.Scheme, "hubBearer")
	}

	// Reconnect: a fresh runtime config read with the key absent entirely
	// (not merely empty) must still leave the pinned scheme alone.
	applyRuntimeConfig(cfg, map[string]string{}, discardLogger())
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
	applyRuntimeConfig(cfg, map[string]string{"auth_scheme": "none"}, discardLogger())
	if cfg.Auth.Scheme != "hubBearer" {
		t.Errorf("Auth.Scheme = %q, want %q (must pin against the captured YAML scheme, not the current live scheme)", cfg.Auth.Scheme, "hubBearer")
	}
}
