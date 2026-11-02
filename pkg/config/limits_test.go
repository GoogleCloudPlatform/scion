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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestMergeScionConfig_CountLimits: a set count limit wins the merge, an
// explicit 0 ("no limit") included, and a nil one keeps the base
// (ptone/scion#5605). The result never aliases the override.
func TestMergeScionConfig_CountLimits(t *testing.T) {
	base := &api.ScionConfig{MaxTurns: api.IntPtr(5), MaxModelCalls: api.IntPtr(9)}

	got := MergeScionConfig(base, &api.ScionConfig{})
	if !api.IntPtrEqual(got.MaxTurns, api.IntPtr(5)) || !api.IntPtrEqual(got.MaxModelCalls, api.IntPtr(9)) {
		t.Fatalf("nil override: got %s/%s, want 5/9", api.FormatCountLimit(got.MaxTurns), api.FormatCountLimit(got.MaxModelCalls))
	}

	override := &api.ScionConfig{MaxTurns: api.IntPtr(0), MaxModelCalls: api.IntPtr(7)}
	got = MergeScionConfig(base, override)
	if !api.IntPtrEqual(got.MaxTurns, api.IntPtr(0)) {
		t.Errorf("unlimited override: MaxTurns = %s, want 0", api.FormatCountLimit(got.MaxTurns))
	}
	if !api.IntPtrEqual(got.MaxModelCalls, api.IntPtr(7)) {
		t.Errorf("value override: MaxModelCalls = %s, want 7", api.FormatCountLimit(got.MaxModelCalls))
	}
	if got.MaxTurns == override.MaxTurns {
		t.Error("merge aliases the override's MaxTurns")
	}
}

// TestFillDefaultLimits: the hub default, then the broker settings default,
// fills only a nil count limit; an explicit 0 is never filled, and a
// negative one is treated as unset.
func TestFillDefaultLimits(t *testing.T) {
	hub := &api.HubAgentDefaults{MaxTurns: 50, MaxDuration: "50m"}
	for _, tc := range []struct {
		name         string
		cfg          api.ScionConfig
		hub          *api.HubAgentDefaults
		wantTurns    *int
		wantCalls    *int
		wantDuration string
	}{
		{name: "unset takes hub then settings", cfg: api.ScionConfig{}, hub: hub, wantTurns: api.IntPtr(50), wantCalls: api.IntPtr(20), wantDuration: "50m"},
		{name: "unset takes settings without hub", cfg: api.ScionConfig{}, wantTurns: api.IntPtr(10), wantCalls: api.IntPtr(20), wantDuration: "10m"},
		{name: "explicit unlimited is kept", cfg: api.ScionConfig{MaxTurns: api.IntPtr(0), MaxModelCalls: api.IntPtr(0), MaxDuration: "0"}, hub: hub, wantTurns: api.IntPtr(0), wantCalls: api.IntPtr(0), wantDuration: "0"},
		{name: "own value is kept", cfg: api.ScionConfig{MaxTurns: api.IntPtr(3)}, hub: hub, wantTurns: api.IntPtr(3), wantCalls: api.IntPtr(20), wantDuration: "50m"},
		{name: "negative is unset", cfg: api.ScionConfig{MaxTurns: api.IntPtr(-2)}, hub: hub, wantTurns: api.IntPtr(50), wantCalls: api.IntPtr(20), wantDuration: "50m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			FillDefaultLimits(&cfg, tc.hub, 10, 20, "10m")
			if !api.IntPtrEqual(cfg.MaxTurns, tc.wantTurns) || !api.IntPtrEqual(cfg.MaxModelCalls, tc.wantCalls) || cfg.MaxDuration != tc.wantDuration {
				t.Errorf("got %s/%s/%q, want %s/%s/%q",
					api.FormatCountLimit(cfg.MaxTurns), api.FormatCountLimit(cfg.MaxModelCalls), cfg.MaxDuration,
					api.FormatCountLimit(tc.wantTurns), api.FormatCountLimit(tc.wantCalls), tc.wantDuration)
			}
		})
	}
	FillDefaultLimits(nil, hub, 1, 1, "1m") // must not panic
	cfg := api.ScionConfig{}
	FillDefaultLimits(&cfg, nil, 0, 0, "")
	if cfg.MaxTurns != nil || cfg.MaxModelCalls != nil || cfg.MaxDuration != "" {
		t.Error("no defaults must leave the limits unset")
	}
}

// TestLoadConfig_CountLimitZeroYAML: a template's max_turns: 0 loads as an
// explicit "no limit", not as unset (ptone/scion#5605), and null or an
// absent key loads as unset.
func TestLoadConfig_CountLimitZeroYAML(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want *int
	}{
		{yaml: "max_turns: 0\n", want: api.IntPtr(0)},
		{yaml: "max_turns: 4\n", want: api.IntPtr(4)},
		{yaml: "max_turns: null\n", want: nil},
		{yaml: "model: m\n", want: nil},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := (&Template{Path: dir}).LoadConfig()
		if err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if !api.IntPtrEqual(cfg.MaxTurns, tc.want) {
			t.Errorf("%q: MaxTurns = %s, want %s", tc.yaml, api.FormatCountLimit(cfg.MaxTurns), api.FormatCountLimit(tc.want))
		}
	}
}
