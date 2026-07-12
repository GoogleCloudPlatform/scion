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

package opsettings_test

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// TestSeedEquivalentRoundTrip verifies that every SeedEquivalent produced by
// DetectDeprecatedServerEnv maps back through LoadSeedEnvKoanf to the original
// KoanfKey. This catches prefix mismatches — e.g. SCION_SEED_SERVER_ being
// used for non-server.* keys like telemetry.enabled (which would map to
// server.telemetry.enabled instead of telemetry.enabled).
func TestSeedEquivalentRoundTrip(t *testing.T) {
	// Build a koanf with a mix of server.* and non-server.* Layer-1 keys.
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails":      []string{"admin@test.com"},
		"server.auth.user_access_mode": "invite",
		"server.hub.public_url":        "https://hub.test.com",
		"telemetry.enabled":            true,
		"default_max_turns":            100,
		"image_registry":               "gcr.io/test",
	}, "."), nil)

	deprecated := opsettings.DetectDeprecatedServerEnv(k)
	if len(deprecated) == 0 {
		t.Fatal("expected deprecated vars for round-trip test")
	}

	for _, d := range deprecated {
		d := d
		t.Run(d.KoanfKey, func(t *testing.T) {
			t.Setenv(d.SeedEquivalent, "test-value")
			seedK := config.LoadSeedEnvKoanf()

			if !seedK.Exists(d.KoanfKey) {
				t.Errorf("SeedEquivalent round-trip failed:\n  SeedEquivalent: %s\n  expected key:   %s\n  actual keys:    %v",
					d.SeedEquivalent, d.KoanfKey, seedK.Keys())
			}
		})
	}
}

// TestSeedEquivalentRoundTrip_TelemetrySpecific is a focused test for
// telemetry.enabled, the specific key that triggered the B2 fix.
func TestSeedEquivalentRoundTrip_TelemetrySpecific(t *testing.T) {
	k := koanf.New(".")
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"telemetry.enabled": true,
	}, "."), nil)

	deprecated := opsettings.DetectDeprecatedServerEnv(k)
	if len(deprecated) != 1 {
		t.Fatalf("expected 1 deprecated var for telemetry.enabled, got %d", len(deprecated))
	}

	d := deprecated[0]
	if d.SeedEquivalent != "SCION_SEED_TELEMETRY_ENABLED" {
		t.Fatalf("expected SeedEquivalent SCION_SEED_TELEMETRY_ENABLED, got %q", d.SeedEquivalent)
	}

	t.Setenv("SCION_SEED_TELEMETRY_ENABLED", "true")
	seedK := config.LoadSeedEnvKoanf()

	if !seedK.Exists("telemetry.enabled") {
		t.Errorf("SCION_SEED_TELEMETRY_ENABLED did not map to telemetry.enabled; keys: %v", seedK.Keys())
	}
	if seedK.Exists("server.telemetry.enabled") {
		t.Error("SCION_SEED_TELEMETRY_ENABLED should NOT map to server.telemetry.enabled")
	}
}
