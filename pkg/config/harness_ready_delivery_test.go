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
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHarnessReadyConfig_Validate(t *testing.T) {
	var nilCfg *HarnessReadyConfig
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil config should validate: %v", err)
	}

	valid := map[string]HarnessReadyConfig{
		"empty":                {},
		"delay":                {Strategy: "delay", Settle: "8s"},
		"hook full":            {Strategy: "hook", Settle: "1.5s", Timeout: "90s"},
		"hook zero settle":     {Strategy: "hook", Settle: "0s"},
		"settle at max":        {Settle: "60s"},
		"timeout at min":       {Strategy: "hook", Timeout: "10s"},
		"timeout at max":       {Strategy: "hook", Timeout: "10m"},
		"hook default timeout": {Strategy: "hook"},
	}
	for name, cfg := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}

	invalid := map[string]struct {
		cfg     HarnessReadyConfig
		wantErr string
	}{
		"delay with timeout":         {HarnessReadyConfig{Strategy: "delay", Timeout: "30s"}, "ready.timeout is not allowed"},
		"default strategy + timeout": {HarnessReadyConfig{Timeout: "30s"}, "ready.timeout is not allowed"},
		"settle 61s":                 {HarnessReadyConfig{Settle: "61s"}, "ready.settle must be between"},
		"negative settle":            {HarnessReadyConfig{Settle: "-1s"}, "ready.settle must be between"},
		"unparseable settle":         {HarnessReadyConfig{Settle: "soon"}, "ready.settle: invalid duration"},
		"unknown strategy":           {HarnessReadyConfig{Strategy: "pane"}, "ready.strategy must be"},
		"timeout below min":          {HarnessReadyConfig{Strategy: "hook", Timeout: "9s"}, "ready.timeout must be between"},
		"timeout above max":          {HarnessReadyConfig{Strategy: "hook", Timeout: "11m"}, "ready.timeout must be between"},
	}
	for name, tc := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestHarnessDeliveryConfig_Validate(t *testing.T) {
	var nilCfg *HarnessDeliveryConfig
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil config should validate: %v", err)
	}

	valid := map[string]HarnessDeliveryConfig{
		"empty":               {},
		"zero submits":        {ExtraSubmits: intPtr(0)},
		"five submits":        {ExtraSubmits: intPtr(5)},
		"four keys":           {SubmitKeys: []string{"Escape", "Enter", "C-m", "C-j"}},
		"inner dash key":      {SubmitKeys: []string{"M-Enter"}},
		"inner semicolon key": {SubmitKeys: []string{"a;b"}},
		"one-byte key":        {SubmitKeys: []string{"q"}},
		"zero delay":          {ExtraSubmitDelay: "0s"},
		"max delay":           {ExtraSubmitDelay: "5s"},
		"32-byte key":         {SubmitKeys: []string{strings.Repeat("k", 32)}},
		"defaults spelt":      {SubmitKeys: []string{"Enter"}, ExtraSubmits: intPtr(2), ExtraSubmitDelay: "300ms"},
	}
	for name, cfg := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}

	invalid := map[string]struct {
		cfg     HarnessDeliveryConfig
		wantErr string
	}{
		"six submits":            {HarnessDeliveryConfig{ExtraSubmits: intPtr(6)}, "delivery.extra_submits must be between 0 and 5"},
		"negative submits":       {HarnessDeliveryConfig{ExtraSubmits: intPtr(-1)}, "delivery.extra_submits must be between 0 and 5"},
		"five keys":              {HarnessDeliveryConfig{SubmitKeys: []string{"a", "b", "c", "d", "e"}}, "delivery.submit_keys must have 1 to 4 entries"},
		"key with a space":       {HarnessDeliveryConfig{SubmitKeys: []string{"C-m Enter"}}, "must not contain whitespace"},
		"key with a tab":         {HarnessDeliveryConfig{SubmitKeys: []string{"C-m\tx"}}, "must not contain whitespace"},
		"key with control":       {HarnessDeliveryConfig{SubmitKeys: []string{"C-m\x1b"}}, "control characters"},
		"empty key":              {HarnessDeliveryConfig{SubmitKeys: []string{"Enter", ""}}, "delivery.submit_keys[1]"},
		"33-byte key":            {HarnessDeliveryConfig{SubmitKeys: []string{strings.Repeat("k", 33)}}, "1 to 32 bytes"},
		"leading dash key":       {HarnessDeliveryConfig{SubmitKeys: []string{"Enter", "-R"}}, `must not start with "-"`},
		"trailing semicolon key": {HarnessDeliveryConfig{SubmitKeys: []string{"Enter;"}}, `must not end with ";"`},
		"lone semicolon key":     {HarnessDeliveryConfig{SubmitKeys: []string{"Enter", ";"}}, `delivery.submit_keys[1]: key ";" must not end with ";"`},
		"6s delay":               {HarnessDeliveryConfig{ExtraSubmitDelay: "6s"}, "delivery.extra_submit_delay must be between"},
		"negative delay":         {HarnessDeliveryConfig{ExtraSubmitDelay: "-1ms"}, "delivery.extra_submit_delay must be between"},
		"unparseable delay":      {HarnessDeliveryConfig{ExtraSubmitDelay: "fast"}, "invalid duration"},
	}
	for name, tc := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolvedDelivery_Defaults(t *testing.T) {
	want := DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, ExtraSubmitDelay: 300 * time.Millisecond}

	var nilEntry *HarnessConfigEntry
	for name, e := range map[string]*HarnessConfigEntry{
		"nil entry":   nilEntry,
		"nil block":   {Harness: "x"},
		"empty block": {Harness: "x", Delivery: &HarnessDeliveryConfig{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := e.ResolvedDelivery(); !reflect.DeepEqual(got, want) {
				t.Fatalf("ResolvedDelivery() = %+v, want %+v", got, want)
			}
		})
	}
	if got := DefaultDeliverySpec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultDeliverySpec() = %+v, want %+v", got, want)
	}
}

func TestResolvedDelivery_ExplicitValues(t *testing.T) {
	e := &HarnessConfigEntry{Harness: "x", Delivery: &HarnessDeliveryConfig{
		SubmitKeys:       []string{"C-m"},
		ExtraSubmits:     intPtr(0),
		ExtraSubmitDelay: "0s",
	}}
	want := DeliverySpec{SubmitKeys: []string{"C-m"}, ExtraSubmits: 0, ExtraSubmitDelay: 0}
	got := e.ResolvedDelivery()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolvedDelivery() = %+v, want %+v", got, want)
	}
	// The resolved keys must not alias the config's slice.
	got.SubmitKeys[0] = "mutated"
	if e.Delivery.SubmitKeys[0] != "C-m" {
		t.Fatalf("ResolvedDelivery returned a slice aliasing the config")
	}
}

func TestResolvedDelivery_InvalidFieldFallsBack(t *testing.T) {
	cases := map[string]struct {
		cfg  HarnessDeliveryConfig
		want DeliverySpec
	}{
		"bad keys, good rest": {
			HarnessDeliveryConfig{SubmitKeys: []string{"a b"}, ExtraSubmits: intPtr(1), ExtraSubmitDelay: "1s"},
			DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 1, ExtraSubmitDelay: time.Second},
		},
		"too many keys": {
			HarnessDeliveryConfig{SubmitKeys: []string{"a", "b", "c", "d", "e"}},
			DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, ExtraSubmitDelay: 300 * time.Millisecond},
		},
		"bad submits, good rest": {
			HarnessDeliveryConfig{SubmitKeys: []string{"C-m"}, ExtraSubmits: intPtr(9), ExtraSubmitDelay: "0s"},
			DeliverySpec{SubmitKeys: []string{"C-m"}, ExtraSubmits: 2, ExtraSubmitDelay: 0},
		},
		"bad delay, good rest": {
			HarnessDeliveryConfig{SubmitKeys: []string{"C-m"}, ExtraSubmits: intPtr(0), ExtraSubmitDelay: "6s"},
			DeliverySpec{SubmitKeys: []string{"C-m"}, ExtraSubmits: 0, ExtraSubmitDelay: 300 * time.Millisecond},
		},
		"unparseable delay": {
			HarnessDeliveryConfig{ExtraSubmitDelay: "later"},
			DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 2, ExtraSubmitDelay: 300 * time.Millisecond},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := tc.cfg
			e := &HarnessConfigEntry{Harness: "x", Delivery: &cfg}
			if got := e.ResolvedDelivery(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResolvedDelivery() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolvedReady_StrategyFallbackWarnsOnce checks that an invalid strategy
// produces only the strategy warning, not a second warning about a timeout
// under the fallback strategy the author never wrote.
func TestResolvedReady_StrategyFallbackWarnsOnce(t *testing.T) {
	const harness = "warn-once-strategy-fallback"
	entry := &HarnessConfigEntry{Harness: harness, Ready: &HarnessReadyConfig{Strategy: "pane", Timeout: "30s"}}
	entry.ResolvedReady()

	var warnings []string
	resolveWarned.Range(func(k, _ any) bool {
		if rest, ok := strings.CutPrefix(k.(string), harness+"\x00"); ok {
			warnings = append(warnings, rest)
		}
		return true
	})
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ready.strategy must be") {
		t.Fatalf("warnings for %s = %q, want only the ready.strategy warning", harness, warnings)
	}
}

func TestResolvedReady_DefaultsPerStrategy(t *testing.T) {
	delayDefault := ReadySpec{Strategy: "delay", Settle: 8 * time.Second}
	hookDefault := ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second}

	var nilEntry *HarnessConfigEntry
	cases := map[string]struct {
		entry *HarnessConfigEntry
		want  ReadySpec
	}{
		"nil entry":                  {nilEntry, delayDefault},
		"nil block":                  {&HarnessConfigEntry{Harness: "x"}, delayDefault},
		"empty block":                {&HarnessConfigEntry{Ready: &HarnessReadyConfig{}}, delayDefault},
		"delay":                      {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "delay"}}, delayDefault},
		"hook":                       {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "hook"}}, hookDefault},
		"hook explicit":              {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "hook", Settle: "2s", Timeout: "30s"}}, ReadySpec{Strategy: "hook", Settle: 2 * time.Second, Timeout: 30 * time.Second}},
		"delay explicit":             {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "delay", Settle: "3s"}}, ReadySpec{Strategy: "delay", Settle: 3 * time.Second}},
		"unknown strategy":           {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "pane", Settle: "500ms"}}, ReadySpec{Strategy: "delay", Settle: 500 * time.Millisecond}},
		"bad settle hook":            {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "hook", Settle: "2m"}}, hookDefault},
		"bad timeout hook":           {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "hook", Timeout: "1s"}}, hookDefault},
		"delay + timeout":            {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "delay", Timeout: "30s"}}, delayDefault},
		"unknown strategy + timeout": {&HarnessConfigEntry{Ready: &HarnessReadyConfig{Strategy: "pane", Timeout: "30s"}}, delayDefault},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.entry.ResolvedReady(); got != tc.want {
				t.Fatalf("ResolvedReady() = %+v, want %+v", got, tc.want)
			}
		})
	}

	if got := DefaultReadySpec("hook"); got != hookDefault {
		t.Fatalf("DefaultReadySpec(hook) = %+v, want %+v", got, hookDefault)
	}
	for _, s := range []string{"delay", "", "unknown"} {
		if got := DefaultReadySpec(s); got != delayDefault {
			t.Fatalf("DefaultReadySpec(%q) = %+v, want %+v", s, got, delayDefault)
		}
	}
}

func TestValidateHarnessConfig_ReadyDeliveryBlocks(t *testing.T) {
	accepted := map[string]string{
		"both blocks": "harness: x\nready:\n  strategy: hook\n  settle: 1.5s\n  timeout: 90s\n" +
			"delivery:\n  submit_keys: [Enter]\n  extra_submits: 2\n  extra_submit_delay: 300ms\n",
		"empty blocks":        "harness: x\nready: {}\ndelivery: {}\n",
		"delay only":          "harness: x\nready:\n  strategy: delay\n  settle: 8s\n",
		"zero extra":          "harness: x\ndelivery:\n  submit_keys: [C-m]\n  extra_submits: 0\n",
		"compound duration":   "harness: x\nready:\n  strategy: hook\n  timeout: 1m30s\n",
		"one-char key":        "harness: x\ndelivery:\n  submit_keys: [q]\n",
		"inner semicolon key": "harness: x\ndelivery:\n  submit_keys: ['a;b']\n",
		"inner dash key":      "harness: x\ndelivery:\n  submit_keys: [M-Enter]\n",
	}
	for name, doc := range accepted {
		t.Run("accepted/"+name, func(t *testing.T) {
			errs, err := ValidateHarnessConfig([]byte(doc))
			if err != nil {
				t.Fatalf("ValidateHarnessConfig: %v", err)
			}
			if len(errs) != 0 {
				t.Fatalf("expected valid, got %v", errs)
			}
		})
	}

	rejected := map[string]string{
		"ready.bogus":                   "harness: x\nready:\n  bogus: 1\n",
		"delivery.bogus":                "harness: x\ndelivery:\n  bogus: 1\n",
		"pane strategy (P5)":            "harness: x\nready:\n  strategy: pane\n",
		"pane_regex (P5)":               "harness: x\nready:\n  pane_regex: '>'\n",
		"hook_events (unbuilt)":         "harness: x\nready:\n  hook_events: [SessionStart]\n",
		"verify (deferred)":             "harness: x\ndelivery:\n  verify: true\n",
		"settle not duration":           "harness: x\nready:\n  settle: soon\n",
		"settle negative":               "harness: x\nready:\n  settle: -1s\n",
		"settle integer":                "harness: x\nready:\n  settle: 8\n",
		"extra_submits 6":               "harness: x\ndelivery:\n  extra_submits: 6\n",
		"extra_submits string":          "harness: x\ndelivery:\n  extra_submits: two\n",
		"submit_keys empty list":        "harness: x\ndelivery:\n  submit_keys: []\n",
		"submit_keys five":              "harness: x\ndelivery:\n  submit_keys: [a, b, c, d, e]\n",
		"submit key with space":         "harness: x\ndelivery:\n  submit_keys: ['C-m Enter']\n",
		"submit key too long":           "harness: x\ndelivery:\n  submit_keys: [" + strings.Repeat("k", 33) + "]\n",
		"submit key leading dash":       "harness: x\ndelivery:\n  submit_keys: ['-X']\n",
		"submit key trailing semicolon": "harness: x\ndelivery:\n  submit_keys: ['Enter;']\n",
		"submit key lone semicolon":     "harness: x\ndelivery:\n  submit_keys: [';']\n",
		"ready not object":              "harness: x\nready: hook\n",
	}
	for name, doc := range rejected {
		t.Run("rejected/"+name, func(t *testing.T) {
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

func TestLoadHarnessConfigDir_ReadyDeliveryBlocks(t *testing.T) {
	yaml := `harness: codex
ready:
  strategy: hook
  settle: 2s
delivery:
  submit_keys: [C-m]
  extra_submits: 0
`
	hc, err := LoadHarnessConfigDir(writeHarnessConfigDir(t, yaml))
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	if hc.Config.Ready == nil || hc.Config.Ready.Strategy != "hook" || hc.Config.Ready.Settle != "2s" {
		t.Fatalf("Ready = %#v", hc.Config.Ready)
	}
	d := hc.Config.Delivery
	if d == nil || !reflect.DeepEqual(d.SubmitKeys, []string{"C-m"}) || d.ExtraSubmits == nil || *d.ExtraSubmits != 0 {
		t.Fatalf("Delivery = %#v", d)
	}
}

// Rules the schema cannot express (ranges, cross-field) must still fail at
// load, through the Go Validate calls in LoadHarnessConfigDir.
func TestLoadHarnessConfigDir_ReadyDeliverySemanticErrorsRejected(t *testing.T) {
	cases := map[string]struct {
		yaml    string
		wantErr string
	}{
		"delay with timeout": {"harness: codex\nready:\n  strategy: delay\n  timeout: 30s\n", "ready.timeout is not allowed"},
		"settle 61s":         {"harness: codex\nready:\n  settle: 61s\n", "ready.settle must be between"},
		"timeout 11m":        {"harness: codex\nready:\n  strategy: hook\n  timeout: 11m\n", "ready.timeout must be between"},
		"delay 6s":           {"harness: codex\ndelivery:\n  extra_submit_delay: 6s\n", "delivery.extra_submit_delay must be between"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadHarnessConfigDir(writeHarnessConfigDir(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadHarnessConfigDir() error = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
