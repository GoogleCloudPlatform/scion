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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexThinkingYAML = `harness: codex
thinking:
  levels:
    - {max: 25,  value: low}
    - {max: 50,  value: medium}
    - {max: 75,  value: high}
    - {max: 100, value: xhigh}
  default: medium
`

func codexThinkingTable() *HarnessThinkingConfig {
	return &HarnessThinkingConfig{
		Levels: []HarnessThinkingLevel{
			{Max: 25, Value: "low"},
			{Max: 50, Value: "medium"},
			{Max: 75, Value: "high"},
			{Max: 100, Value: "xhigh"},
		},
		Default: "medium",
	}
}

func TestValidateHarnessConfig_ThinkingBlockAccepted(t *testing.T) {
	for name, doc := range map[string]string{
		"codex table": codexThinkingYAML,
		"no default":  "harness: x\nthinking:\n  levels:\n    - {max: 100, value: high}\n",
		"zero max":    "harness: x\nthinking:\n  levels:\n    - {max: 0, value: none}\n    - {max: 100, value: high}\n",
	} {
		t.Run(name, func(t *testing.T) {
			errs, err := ValidateHarnessConfig([]byte(doc))
			if err != nil {
				t.Fatalf("ValidateHarnessConfig: %v", err)
			}
			if len(errs) != 0 {
				t.Fatalf("expected valid thinking block, got %v", errs)
			}
		})
	}
}

func TestValidateHarnessConfig_ThinkingBlockRejected(t *testing.T) {
	cases := map[string]string{
		"max above 100":       "harness: x\nthinking:\n  levels:\n    - {max: 101, value: high}\n",
		"negative max":        "harness: x\nthinking:\n  levels:\n    - {max: -1, value: low}\n    - {max: 100, value: high}\n",
		"non-integer max":     "harness: x\nthinking:\n  levels:\n    - {max: 50.5, value: low}\n    - {max: 100, value: high}\n",
		"empty value":         "harness: x\nthinking:\n  levels:\n    - {max: 100, value: \"\"}\n",
		"missing value":       "harness: x\nthinking:\n  levels:\n    - {max: 100}\n",
		"missing max":         "harness: x\nthinking:\n  levels:\n    - {value: high}\n",
		"unknown level key":   "harness: x\nthinking:\n  levels:\n    - {max: 100, value: high, flag: --effort}\n",
		"unknown block key":   "harness: x\nthinking:\n  target: {env: X}\n  levels:\n    - {max: 100, value: high}\n",
		"empty levels":        "harness: x\nthinking:\n  levels: []\n",
		"missing levels":      "harness: x\nthinking:\n  default: medium\n",
		"empty default":       "harness: x\nthinking:\n  levels:\n    - {max: 100, value: high}\n  default: \"\"\n",
		"thinking not object": "harness: x\nthinking: high\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			errs, err := ValidateHarnessConfig([]byte(doc))
			if err != nil {
				t.Fatalf("ValidateHarnessConfig: %v", err)
			}
			if len(errs) == 0 {
				t.Fatalf("expected schema to reject %s", name)
			}
		})
	}
}

// The thinking_budget_* keys were never read and were removed in favour of
// the thinking block; the schema must now reject them like any unknown key.
func TestValidateHarnessConfig_ThinkingBudgetFieldsRejected(t *testing.T) {
	for _, doc := range []string{
		"harness: x\nthinking_budget_map:\n  some-model: 1024\n",
		"harness: x\nthinking_budget_flag: --thinking-budget\n",
		"harness: x\nthinking_budget_config_key: thinkingBudget\n",
	} {
		errs, err := ValidateHarnessConfig([]byte(doc))
		if err != nil {
			t.Fatalf("ValidateHarnessConfig: %v", err)
		}
		if len(errs) == 0 {
			t.Fatalf("expected schema to reject removed field in %q", doc)
		}
	}
}

func TestHarnessThinkingConfigValidate(t *testing.T) {
	if err := codexThinkingTable().Validate(); err != nil {
		t.Fatalf("codex table should validate: %v", err)
	}
	var nilCfg *HarnessThinkingConfig
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil config should validate: %v", err)
	}

	cases := map[string]struct {
		cfg     HarnessThinkingConfig
		wantErr string
	}{
		"empty levels": {
			cfg:     HarnessThinkingConfig{},
			wantErr: "must not be empty",
		},
		"non-ascending": {
			cfg: HarnessThinkingConfig{Levels: []HarnessThinkingLevel{
				{Max: 50, Value: "medium"}, {Max: 25, Value: "low"}, {Max: 100, Value: "high"},
			}},
			wantErr: "must be greater than",
		},
		"duplicate max": {
			cfg: HarnessThinkingConfig{Levels: []HarnessThinkingLevel{
				{Max: 50, Value: "low"}, {Max: 50, Value: "medium"}, {Max: 100, Value: "high"},
			}},
			wantErr: "must be greater than",
		},
		"last max not 100": {
			cfg: HarnessThinkingConfig{Levels: []HarnessThinkingLevel{
				{Max: 25, Value: "low"}, {Max: 75, Value: "high"},
			}},
			wantErr: "last max must be 100",
		},
		"max out of range": {
			cfg: HarnessThinkingConfig{Levels: []HarnessThinkingLevel{
				{Max: -1, Value: "low"}, {Max: 100, Value: "high"},
			}},
			wantErr: "between 0 and 100",
		},
		"empty value": {
			cfg: HarnessThinkingConfig{Levels: []HarnessThinkingLevel{
				{Max: 100, Value: ""},
			}},
			wantErr: "must not be empty",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func writeHarnessConfigDir(t *testing.T, yaml string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadHarnessConfigDir_ThinkingBlock(t *testing.T) {
	hc, err := LoadHarnessConfigDir(writeHarnessConfigDir(t, codexThinkingYAML))
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	want := codexThinkingTable()
	got := hc.Config.Thinking
	if got == nil {
		t.Fatal("expected Thinking to be loaded")
	}
	if got.Default != want.Default || len(got.Levels) != len(want.Levels) {
		t.Fatalf("Thinking = %#v, want %#v", got, want)
	}
	for i := range want.Levels {
		if got.Levels[i] != want.Levels[i] {
			t.Fatalf("Levels[%d] = %#v, want %#v", i, got.Levels[i], want.Levels[i])
		}
	}
}

func TestLoadHarnessConfigDir_ThinkingNonAscendingRejected(t *testing.T) {
	yaml := `harness: codex
thinking:
  levels:
    - {max: 50,  value: medium}
    - {max: 25,  value: low}
    - {max: 100, value: high}
`
	_, err := LoadHarnessConfigDir(writeHarnessConfigDir(t, yaml))
	if err == nil {
		t.Fatal("expected non-ascending thinking levels to fail at load")
	}
	if !strings.Contains(err.Error(), "thinking.levels") {
		t.Fatalf("expected thinking.levels error, got %v", err)
	}
}

func TestLoadHarnessConfigDir_ThinkingLastMaxNot100Rejected(t *testing.T) {
	yaml := `harness: codex
thinking:
  levels:
    - {max: 25, value: low}
    - {max: 75, value: high}
`
	if _, err := LoadHarnessConfigDir(writeHarnessConfigDir(t, yaml)); err == nil {
		t.Fatal("expected last max != 100 to fail at load")
	}
}

func TestParseHarnessConfigYAML_ThinkingRoundTrip(t *testing.T) {
	yaml := `harness: x
thinking:
  levels:
    - {max: 0,   value: none}
    - {max: 100, value: high}
  default: high
`
	entry, err := ParseHarnessConfigYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseHarnessConfigYAML: %v", err)
	}
	if entry.Thinking == nil || len(entry.Thinking.Levels) != 2 {
		t.Fatalf("Thinking = %#v", entry.Thinking)
	}
	if entry.Thinking.Levels[0] != (HarnessThinkingLevel{Max: 0, Value: "none"}) {
		t.Fatalf("Levels[0] = %#v", entry.Thinking.Levels[0])
	}
	if entry.Thinking.Default != "high" {
		t.Fatalf("Default = %q", entry.Thinking.Default)
	}

	// JSON (the provision manifest encoding) must keep max: 0 too.
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"thinking":{"levels":[{"max":0,"value":"none"},{"max":100,"value":"high"}],"default":"high"}`) {
		t.Fatalf("unexpected JSON encoding: %s", data)
	}
	var back HarnessConfigEntry
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Thinking == nil || back.Thinking.Levels[0].Max != 0 || back.Thinking.Default != "high" {
		t.Fatalf("JSON round-trip lost data: %#v", back.Thinking)
	}
}
