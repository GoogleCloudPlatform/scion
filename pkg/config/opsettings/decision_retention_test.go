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
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	if cfg.Enabled {
		t.Fatal("default Enabled must be false")
	}
	if cfg.HasWindow() {
		t.Fatal("default HasWindow must be false")
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
	if err != nil || roundTrip != cfg {
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
		{"project_overrides not accepted in PR-1", `{"enabled":true,"project_overrides":{}}`},
		{"unknown sweep key", `{"sweep":{"chunk_size":100}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseDecisionRetentionDoc(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("expected error for %s, got %+v", tc.raw, cfg)
			}
			if cfg.Enabled || cfg.HasWindow() {
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
