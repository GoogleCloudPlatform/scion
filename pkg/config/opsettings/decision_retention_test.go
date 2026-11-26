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
	"testing"
)

func TestParseDecisionRetentionDoc_Defaults(t *testing.T) {
	cfg, err := ParseDecisionRetentionDoc(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ParseDecisionRetentionDoc({}): %v", err)
	}
	want := DefaultDecisionRetentionConfig()
	if !cfg.Equal(want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	if cfg.Enabled {
		t.Fatal("default Enabled must be false")
	}
	if _, ok := cfg.MinEffectiveDays(); cfg.HasWindow() || ok {
		t.Fatal("default HasWindow and MinEffectiveDays must be false")
	}
}

func TestParseDecisionRetentionDoc_ValidBoundsAndRoundTrip(t *testing.T) {
	raw := json.RawMessage(`{"enabled":true,"retention_days":30,"sweep":{"page_size":50,"tick_budget_seconds":1,"tick_interval_minutes":1}}`)
	cfg, err := ParseDecisionRetentionDoc(raw)
	if err != nil {
		t.Fatalf("min bounds: %v", err)
	}
	if !cfg.Enabled || cfg.RetentionDays != 30 || cfg.PageSize != 50 || cfg.TickBudgetSeconds != 1 || cfg.TickIntervalMinutes != 1 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	doc, err := json.Marshal(cfg.Settings())
	if err != nil {
		t.Fatalf("marshal Settings: %v", err)
	}
	if errs := Validate(DecisionRetentionSectionName, doc); len(errs) > 0 {
		t.Fatalf("schema Validate failed: %v", errs)
	}
	roundTrip, err := ParseDecisionRetentionDoc(doc)
	if err != nil || !roundTrip.Equal(cfg) {
		t.Fatalf("round-trip mismatch: got %+v (err=%v), want %+v", roundTrip, err, cfg)
	}

	rawMax := json.RawMessage(`{"enabled":true,"retention_days":36500,"sweep":{"page_size":1000,"tick_budget_seconds":40,"tick_interval_minutes":1440}}`)
	cfgMax, err := ParseDecisionRetentionDoc(rawMax)
	if err != nil {
		t.Fatalf("max bounds: %v", err)
	}
	if cfgMax.RetentionDays != 36500 || cfgMax.PageSize != 1000 || cfgMax.TickBudgetSeconds != 40 || cfgMax.TickIntervalMinutes != 1440 {
		t.Fatalf("unexpected max config: %+v", cfgMax)
	}
	if errs := Validate(DecisionRetentionSectionName, rawMax); len(errs) > 0 {
		t.Fatalf("schema Validate max failed: %v", errs)
	}
}

func TestParseDecisionRetentionDoc_InvalidAndFailClosed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"malformed JSON", `{`},
		{"retention_days below min", `{"enabled":true,"retention_days":29}`},
		{"retention_days zero", `{"enabled":true,"retention_days":0}`},
		{"retention_days negative", `{"enabled":true,"retention_days":-1}`},
		{"retention_days above max", `{"enabled":true,"retention_days":36501}`},
		{"page_size below min", `{"sweep":{"page_size":49}}`},
		{"page_size above max", `{"sweep":{"page_size":1001}}`},
		{"tick_budget below min", `{"sweep":{"tick_budget_seconds":0}}`},
		{"tick_budget above max", `{"sweep":{"tick_budget_seconds":41}}`},
		{"tick_interval below min", `{"sweep":{"tick_interval_minutes":0}}`},
		{"tick_interval above max", `{"sweep":{"tick_interval_minutes":1441}}`},
		{"unknown top-level key", `{"enabled":true,"extra":1}`},
		{"unknown sweep key", `{"sweep":{"chunk_size":100}}`},
		{"override non-uuid key", `{"project_overrides":{"not-a-uuid":{"mode":"keep_forever"}}}`},
		{"override uppercase uuid key", `{"project_overrides":{"11111111-1111-1111-1111-11111111111A":{"mode":"keep_forever"}}}`},
		{"override invalid mode", `{"project_overrides":{"11111111-1111-1111-1111-111111111111":{"mode":"archive"}}}`},
		{"override days missing days", `{"project_overrides":{"11111111-1111-1111-1111-111111111111":{"mode":"days"}}}`},
		{"override days below min", `{"project_overrides":{"11111111-1111-1111-1111-111111111111":{"mode":"days","days":29}}}`},
		{"override days above max", `{"project_overrides":{"11111111-1111-1111-1111-111111111111":{"mode":"days","days":36501}}}`},
		{"override keep_forever with days", `{"project_overrides":{"11111111-1111-1111-1111-111111111111":{"mode":"keep_forever","days":30}}}`},
		{"override unknown field", `{"project_overrides":{"11111111-1111-1111-1111-111111111111":{"mode":"keep_forever","extra":1}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseDecisionRetentionDoc(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("expected error for %s, got %+v", tc.raw, cfg)
			}
			if _, ok := cfg.MinEffectiveDays(); cfg.Enabled || cfg.HasWindow() || ok {
				t.Fatalf("failed parse must return disabled/no-window zero config, got %+v", cfg)
			}
			if tc.name != "malformed JSON" {
				if errs := Validate(DecisionRetentionSectionName, json.RawMessage(tc.raw)); len(errs) == 0 {
					t.Fatalf("expected JSON schema Validate error for %s", tc.raw)
				}
			}
		})
	}
}

func TestParseDecisionRetentionDoc_ProjectOverridesAndEffectiveWindow(t *testing.T) {
	const (
		projDays = "11111111-1111-1111-1111-111111111111"
		projHold = "22222222-2222-2222-2222-222222222222"
		projDef  = "33333333-3333-3333-3333-333333333333"
	)

	// 1. Hub default 90d + override 45d + override keep_forever.
	raw := json.RawMessage(`{
		"enabled": true,
		"retention_days": 90,
		"project_overrides": {
			"11111111-1111-1111-1111-111111111111": {"mode": "days", "days": 45},
			"22222222-2222-2222-2222-222222222222": {"mode": "keep_forever"}
		}
	}`)
	if errs := Validate(DecisionRetentionSectionName, raw); len(errs) > 0 {
		t.Fatalf("schema Validate failed: %v", errs)
	}
	cfg, err := ParseDecisionRetentionDoc(raw)
	if err != nil {
		t.Fatalf("ParseDecisionRetentionDoc: %v", err)
	}
	if !cfg.HasWindow() {
		t.Fatalf("expected HasWindow true")
	}
	if d, ok := cfg.EffectiveDays(projDays); !ok || d != 45 {
		t.Fatalf("EffectiveDays(projDays) = (%d, %v), want (45, true)", d, ok)
	}
	if d, ok := cfg.EffectiveDays(projHold); ok || d != 0 {
		t.Fatalf("EffectiveDays(projHold) = (%d, %v), want (0, false)", d, ok)
	}
	if d, ok := cfg.EffectiveDays(projDef); !ok || d != 90 {
		t.Fatalf("EffectiveDays(projDef) = (%d, %v), want (90, true)", d, ok)
	}
	if minD, ok := cfg.MinEffectiveDays(); !ok || minD != 45 {
		t.Fatalf("MinEffectiveDays() = (%d, %v), want (45, true)", minD, ok)
	}

	// Round-trip via Settings().
	doc, err := json.Marshal(cfg.Settings())
	if err != nil {
		t.Fatalf("marshal Settings: %v", err)
	}
	if errs := Validate(DecisionRetentionSectionName, doc); len(errs) > 0 {
		t.Fatalf("schema Validate round-trip failed: %v", errs)
	}
	rt, err := ParseDecisionRetentionDoc(doc)
	if err != nil || !rt.Equal(cfg) {
		t.Fatalf("round-trip mismatch: got %+v (err=%v), want %+v", rt, err, cfg)
	}

	// 2. No hub default, only a days override -> HasWindow=false, MinEffectiveDays=(60, true).
	rawOverrideOnly := json.RawMessage(`{
		"enabled": true,
		"project_overrides": {
			"11111111-1111-1111-1111-111111111111": {"mode": "days", "days": 60}
		}
	}`)
	cfg2, err := ParseDecisionRetentionDoc(rawOverrideOnly)
	if err != nil {
		t.Fatalf("override-only parse: %v", err)
	}
	if cfg2.HasWindow() {
		t.Fatal("expected HasWindow false when retention_days absent")
	}
	if d, ok := cfg2.EffectiveDays(projDays); !ok || d != 60 {
		t.Fatalf("EffectiveDays(projDays) = (%d, %v), want (60, true)", d, ok)
	}
	if d, ok := cfg2.EffectiveDays(projDef); ok || d != 0 {
		t.Fatalf("EffectiveDays(projDef) = (%d, %v), want (0, false)", d, ok)
	}
	if minD, ok := cfg2.MinEffectiveDays(); !ok || minD != 60 {
		t.Fatalf("MinEffectiveDays() = (%d, %v), want (60, true)", minD, ok)
	}

	// 3. No hub default, only keep_forever override -> MinEffectiveDays=(0, false).
	rawHoldOnly := json.RawMessage(`{
		"enabled": true,
		"project_overrides": {
			"22222222-2222-2222-2222-222222222222": {"mode": "keep_forever"}
		}
	}`)
	cfg3, err := ParseDecisionRetentionDoc(rawHoldOnly)
	if err != nil {
		t.Fatalf("hold-only parse: %v", err)
	}
	if minD, ok := cfg3.MinEffectiveDays(); ok || minD != 0 {
		t.Fatalf("expected MinEffectiveDays (0, false) when only keep_forever overrides exist, got (%d, %v)", minD, ok)
	}
}
