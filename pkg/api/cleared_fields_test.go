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

package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClearableConfigPath(t *testing.T) {
	for path, want := range map[string]bool{
		"max_turns":         true,
		"max_model_calls":   true,
		"max_duration":      true,
		"thinking_level":    true,
		"resources":         true,
		"system_prompt":     true,
		"env.FOO":           true,
		"env.":              false,
		"env":               false,
		"model":             false,
		"image":             false,
		"auth_selectedType": false,
		"user":              false,
		"kubernetes":        false,
		"command_args":      false,
		"task_flag":         false,
		"task":              false,
		"volumes":           false,
		"mcp_servers":       false,
		"skills":            false,
		"telemetry":         false,
		"harness":           false,
		"cleared_fields":    false,
		"message_mode":      false,
		"":                  false,
	} {
		if got := ClearableConfigPath(path); got != want {
			t.Errorf("ClearableConfigPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// Every clearable key must name a real ScionConfig json key, or a clear
// would silently do nothing.
func TestClearableConfigKeysAreScionConfigKeys(t *testing.T) {
	for k := range clearableConfigKeys {
		if _, ok := scionConfigFieldIndex[k]; !ok {
			t.Errorf("clearable key %q is not an api.ScionConfig json key", k)
		}
	}
}

func clearedTestConfig() *ScionConfig {
	level := 2
	return &ScionConfig{
		Harness:       "claude",
		Model:         "m",
		MaxTurns:      IntPtr(9),
		MaxDuration:   "2h",
		ThinkingLevel: &level,
		Resources:     &ResourceSpec{Requests: ResourceList{CPU: "2"}},
		SystemPrompt:  "be terse",
		Env:           map[string]string{"FOO": "1", "BAR": "2"},
	}
}

func TestApplyClearedFields_Zeroes(t *testing.T) {
	cfg := clearedTestConfig()
	ignored := ApplyClearedFields(cfg, []string{"max_turns", "thinking_level", "resources", "system_prompt", "env.FOO", "env.MISSING", "model", "harness"}, nil)
	if !reflect.DeepEqual(ignored, []string{"model", "harness"}) {
		t.Errorf("ignored = %v", ignored)
	}
	want := &ScionConfig{Harness: "claude", Model: "m", MaxDuration: "2h", Env: map[string]string{"BAR": "2"}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestApplyClearedFields_Inherits(t *testing.T) {
	cfg := clearedTestConfig()
	inherit := &ScionConfig{MaxTurns: IntPtr(5), Resources: &ResourceSpec{Limits: ResourceList{Memory: "1Gi"}}, Env: map[string]string{"FOO": "template"}}
	ApplyClearedFields(cfg, []string{"max_turns", "resources", "thinking_level", "max_duration", "env.FOO"}, inherit)
	if !IntPtrEqual(cfg.MaxTurns, IntPtr(5)) {
		t.Errorf("max_turns = %s, want the inherited 5", FormatCountLimit(cfg.MaxTurns))
	}
	if !reflect.DeepEqual(cfg.Resources, inherit.Resources) {
		t.Errorf("resources = %+v, want the inherited value", cfg.Resources)
	}
	if cfg.ThinkingLevel != nil || cfg.MaxDuration != "" {
		t.Errorf("a key the inherited config leaves unset is unset: thinking_level=%v max_duration=%q", cfg.ThinkingLevel, cfg.MaxDuration)
	}
	if _, ok := cfg.Env["FOO"]; ok {
		t.Errorf("env keys are removed, never copied: %v", cfg.Env)
	}
}

func TestApplyClearedFields_NilConfig(t *testing.T) {
	if got := ApplyClearedFields(nil, []string{"max_turns"}, nil); got != nil {
		t.Errorf("nil config: ignored = %v", got)
	}
}

func TestScionConfigClearedFieldsWireOnly(t *testing.T) {
	var fromJSON ScionConfig
	if err := json.Unmarshal([]byte(`{"cleared_fields":["max_turns","env.FOO"]}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJSON.ClearedFields, []string{"max_turns", "env.FOO"}) {
		t.Errorf("json ClearedFields = %v", fromJSON.ClearedFields)
	}
	out, err := json.Marshal(ScionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cleared_fields") {
		t.Errorf("empty ClearedFields marshalled: %s", out)
	}

	var fromYAML ScionConfig
	if err := yaml.Unmarshal([]byte("cleared_fields: [max_turns]\nmax_turns: 3\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML.ClearedFields != nil {
		t.Errorf("a YAML template set ClearedFields: %v", fromYAML.ClearedFields)
	}
}

// TestRerenderableConfigKeys pins the provision-rendered keys a
// changed_fields entry may name: exactly the system prompt, the agent
// instructions, the skills and the services.
func TestRerenderableConfigKeys(t *testing.T) {
	want := []string{"agent_instructions", "services", "skills", "system_prompt"}
	if got := RerenderableConfigKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RerenderableConfigKeys() = %v, want %v", got, want)
	}
	for _, k := range want {
		if _, ok := scionConfigFieldIndex[k]; !ok {
			t.Errorf("%q is not a ScionConfig json key", k)
		}
	}
}

// TestRerenderConfigKeys: the keys a start re-renders are the rerenderable
// entries of changed_fields and of cleared_fields, deduplicated and sorted;
// anything else is dropped.
func TestRerenderConfigKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *ScionConfig
		want []string
	}{
		{"nil", nil, nil},
		{"none", &ScionConfig{SystemPrompt: "x"}, nil},
		{"changed", &ScionConfig{ChangedFields: []string{"skills", "system_prompt"}}, []string{"skills", "system_prompt"}},
		{"cleared rerenderable", &ScionConfig{ClearedFields: []string{"system_prompt", "max_turns", "env.FOO"}}, []string{"system_prompt"}},
		{"union and dedupe", &ScionConfig{ChangedFields: []string{"system_prompt", "services"}, ClearedFields: []string{"system_prompt"}}, []string{"services", "system_prompt"}},
		{"other keys dropped", &ScionConfig{ChangedFields: []string{"model", "secrets", "agent_plugins", ""}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RerenderConfigKeys(tc.cfg); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RerenderConfigKeys = %v, want %v", got, tc.want)
			}
		})
	}
}
