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

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// TestLoadMandatoryPreamble_ExperimentGates: the mandatory preamble applies
// the platform-skill experiment gates (gateMarkdown) to each file.
func TestLoadMandatoryPreamble_ExperimentGates(t *testing.T) {
	fsys := fstest.MapFS{
		"a.md": &fstest.MapFile{Data: []byte("Always\n<!-- scion:experiment " + gateTestOn + " -->\nGated\n<!-- scion:end-experiment -->\nTail\n")},
		"b.md": &fstest.MapFile{Data: []byte("---\nexperiment: " + gateTestOn + "\n---\nWhole file\n")},
		"c.md": &fstest.MapFile{Data: []byte("<!-- scion:experiment hub.no_such_experiment -->\nUnknown\n<!-- scion:end-experiment -->\nLast\n")},
	}
	for _, tc := range []struct {
		name string
		isOn func(string) bool
		want string
	}{
		{"nil isOn: every gate off", nil, "Always\nTail\n\nLast"},
		{"experiment off", onlyOn(gateTestOff), "Always\nTail\n\nLast"},
		{"experiment on", onlyOn(gateTestOn, "hub.no_such_experiment"), "Always\nGated\nTail\n\n---\nexperiment: " + gateTestOn + "\n---\nWhole file\n\nLast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadMandatoryPreamble(fsys, tc.isOn)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

const (
	tocHeading          = "# Key Platform Skills"
	tocMessagingTrigger = "must read if you get a scion message"
)

// gatedTOCLines maps each experiment gating a line of the Key Platform
// Skills section to text only that line carries.
var gatedTOCLines = map[string]string{
	experiments.Decisions:       "send a decision prompt",
	experiments.AgentSecrets:    "send a secret request",
	experiments.CommandRequests: "command-request guidance",
}

// TestMandatoryBoilerplate_KeyPlatformSkillsTOC renders the real boilerplate
// for every combination of the experiments its Key Platform Skills section
// names: the section is last, appears once, always carries the messaging and
// decisions entries, and carries each gated entry only while its experiment
// is on, with no gate markers left. Run with -v to see each rendering.
func TestMandatoryBoilerplate_KeyPlatformSkillsTOC(t *testing.T) {
	ids := []string{experiments.Decisions, experiments.AgentSecrets, experiments.CommandRequests}
	for mask := 0; mask < 1<<len(ids); mask++ {
		var on []string
		for i, id := range ids {
			if mask&(1<<i) != 0 {
				on = append(on, id)
			}
		}
		t.Run(fmt.Sprintf("on=%v", on), func(t *testing.T) {
			got, err := loadMandatoryPreamble(resources.MandatoryBoilerplateFS(), onlyOn(on...))
			if err != nil {
				t.Fatal(err)
			}
			s := string(got)
			if n := strings.Count(s, tocHeading); n != 1 {
				t.Fatalf("%q appears %d times, want 1", tocHeading, n)
			}
			toc := s[strings.Index(s, tocHeading):]
			t.Logf("rendered section:\n%s", toc)
			if strings.Contains(toc[len(tocHeading):], "\n# ") {
				t.Errorf("Key Platform Skills is not the last section of the boilerplate:\n%s", toc)
			}
			if strings.Contains(s, "scion:") {
				t.Errorf("rendered boilerplate keeps a gate marker:\n%s", toc)
			}
			for _, want := range []string{tocMessagingTrigger, "`scion-messaging`", "**Decisions:**"} {
				if !strings.Contains(toc, want) {
					t.Errorf("section lacks always-visible %q:\n%s", want, toc)
				}
			}
			for id, text := range gatedTOCLines {
				wantIt := false
				for _, o := range on {
					wantIt = wantIt || o == id
				}
				if strings.Contains(s, text) != wantIt {
					t.Errorf("%s on=%v: boilerplate contains %q = %v", id, wantIt, text, !wantIt)
				}
			}
		})
	}
}

// TestProvisionAgent_BoilerplateFollowsLocalExperiments: provisioning
// renders the boilerplate gates from the agent's experiment state (here the
// local settings' local_experiments).
func TestProvisionAgent_BoilerplateFollowsLocalExperiments(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	settingsPath := filepath.Join(globalScionDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(data) == 0 {
		data = []byte("schema_version: \"1\"\n")
	}
	if data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, "local_experiments:\n  - "+experiments.AgentSecrets+"\n"...)
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(globalScionDir, "templates", "toc-tpl")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "generic"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chdir(filepath.Dir(projectScionDir))

	agentHome, _, _, err := ProvisionAgent(context.Background(), "toc-agent", "toc-tpl", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	got := readFileString(t, filepath.Join(agentHome, "agents.md"))
	if !strings.Contains(got, gatedTOCLines[experiments.AgentSecrets]) {
		t.Errorf("agents.md lacks the %s entry with the experiment on locally:\n%s", experiments.AgentSecrets, got)
	}
	for _, id := range []string{experiments.Decisions, experiments.CommandRequests} {
		if strings.Contains(got, gatedTOCLines[id]) {
			t.Errorf("agents.md carries the %s entry with the experiment off:\n%s", id, got)
		}
	}
}

// TestRerender_BoilerplateFollowsExperimentOn: the start-time prompt
// re-render applies the boilerplate gates from rerenderInputs.ExperimentOn.
func TestRerender_BoilerplateFollowsExperimentOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		isOn func(string) bool
		want bool
	}{
		{"off", nil, false},
		{"on", onlyOn(experiments.CommandRequests), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRerenderFixture(t)
			cfg := &api.ScionConfig{AgentInstructions: "do the thing"}
			inline := &api.ScionConfig{AgentInstructions: "do the thing", ChangedFields: []string{"agent_instructions"}}
			in := f.inputs(&harness.Generic{}, []string{"agent_instructions"}, cfg, inline, nil)
			in.ExperimentOn = tc.isOn
			if _, err := rerenderProvisionedFiles(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			got := readFileString(t, filepath.Join(f.agentHome, "agents.md"))
			if strings.Contains(got, gatedTOCLines[experiments.CommandRequests]) != tc.want {
				t.Errorf("agents.md contains the command-request entry = %v, want %v:\n%s", !tc.want, tc.want, got)
			}
		})
	}
}
