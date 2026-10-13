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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

func TestMergeScionConfig_AgentPlugins(t *testing.T) {
	parent := &api.ScionConfig{AgentPlugins: []api.AgentPluginReference{{Path: "plugins/a"}}}
	child := &api.ScionConfig{AgentPlugins: []api.AgentPluginReference{{Path: "plugins/b", Optional: true}, {Path: "./plugins/a"}}}
	merged := MergeScionConfig(parent, child)
	want := []api.AgentPluginReference{
		{Path: "plugins/b", Optional: true, Scope: "template"},
		{Path: "./plugins/a", Scope: "template"},
	}
	if !slices.Equal(merged.AgentPlugins, want) {
		t.Fatalf("merged = %+v, want %+v", merged.AgentPlugins, want)
	}
	if child.AgentPlugins[0].Scope != "" || len(parent.AgentPlugins) != 1 {
		t.Fatal("inputs were modified")
	}

	// Re-merging the persisted result over the chain (GetAgent) does not grow it.
	again := MergeScionConfig(MergeScionConfig(parent, child), merged)
	if !slices.Equal(again.AgentPlugins, want) {
		t.Fatalf("re-merge = %+v, want %+v", again.AgentPlugins, want)
	}
	twice := MergeScionConfig(again, merged)
	if len(twice.AgentPlugins) != 2 {
		t.Fatalf("second re-merge grew the list: %+v", twice.AgentPlugins)
	}

	// No override entries: base kept.
	if got := MergeScionConfig(parent, &api.ScionConfig{}); !slices.Equal(got.AgentPlugins, parent.AgentPlugins) {
		t.Fatalf("base not kept: %+v", got.AgentPlugins)
	}
}

func TestAgentSchema_AgentPlugins(t *testing.T) {
	valid := []string{
		"agent_plugins:\n  - path: plugins/x\n",
		"agent_plugins:\n  - path: plugins/x\n    optional: true\n    scope: template\n",
		"agent_plugins: []\n",
	}
	for _, doc := range valid {
		errs, err := ValidateAgentConfig([]byte(doc), "1")
		if err != nil || len(errs) != 0 {
			t.Errorf("valid %q: errs=%v err=%v", doc, errs, err)
		}
	}
	invalid := []string{
		"agent_plugins:\n  - optional: true\n",
		"agent_plugins:\n  - path: \"\"\n",
		"agent_plugins:\n  - path: plugins/x\n    source: github\n",
		"agent_plugins:\n  - path: plugins/x\n    optional: \"yes\"\n",
		"agent_plugins: plugins/x\n",
	}
	for _, doc := range invalid {
		errs, err := ValidateAgentConfig([]byte(doc), "1")
		if err != nil || len(errs) == 0 {
			t.Errorf("invalid %q accepted: errs=%v err=%v", doc, errs, err)
		}
	}
}

func TestSettingsSchema_LocalExperimentsAndCapability(t *testing.T) {
	errs, err := ValidateSettings([]byte("schema_version: \"1\"\nlocal_experiments:\n  - hub.agent_plugins\n  - hub.unknown_thing\n"), "1")
	if err != nil || len(errs) != 0 {
		t.Errorf("local_experiments: errs=%v err=%v", errs, err)
	}
	errs, err = ValidateSettings([]byte("schema_version: \"1\"\nlocal_experiments:\n  - Not A Name\n"), "1")
	if err != nil || len(errs) == 0 {
		t.Errorf("bad local_experiments name accepted: errs=%v err=%v", errs, err)
	}
	hc := "harness: generic\nimage: scion-base:test\ncapabilities:\n  agent_plugins: { support: \"partial\", reason: \"x\" }\n"
	errs, err = ValidateHarnessConfig([]byte(hc))
	if err != nil || len(errs) != 0 {
		t.Errorf("agent_plugins capability: errs=%v err=%v", errs, err)
	}
	errs, err = ValidateHarnessConfig([]byte("harness: generic\nimage: scion-base:test\ncapabilities:\n  agent_plugins: { support: \"maybe\" }\n"))
	if err != nil || len(errs) == 0 {
		t.Errorf("bad support level accepted: errs=%v err=%v", errs, err)
	}
}

func TestLoadVersionedSettings_LocalExperiments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "schema_version: \"1\"\nlocal_experiments:\n  - hub.agent_plugins\n  - hub.not_registered\n"
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	vs, err := LoadVersionedSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vs.LocalExperiments, []string{"hub.agent_plugins", "hub.not_registered"}) {
		t.Fatalf("LocalExperiments = %v", vs.LocalExperiments)
	}
}

// The local_experiments item pattern in the settings schema is a copy of
// experiments.NamePattern; keep them identical.
func TestLocalExperimentsPattern_MatchesNamePattern(t *testing.T) {
	raw, err := GetSettingsSchemaJSON("1")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Properties map[string]struct {
			Items struct {
				Pattern string `json:"pattern"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	if got := root.Properties["local_experiments"].Items.Pattern; got != experiments.NamePattern {
		t.Fatalf("local_experiments pattern = %q, want experiments.NamePattern %q", got, experiments.NamePattern)
	}
}
