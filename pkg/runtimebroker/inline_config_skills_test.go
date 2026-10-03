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

package runtimebroker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func writeAgentConfig(t *testing.T, projectDir, agentName string, cfg api.ScionConfig) string {
	t.Helper()
	agentDir := config.GetAgentDir(projectDir, agentName, false)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}
	return cfgPath
}

func readAgentSkills(t *testing.T, cfgPath string) []api.SkillReference {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read scion-agent.json: %v", err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to parse scion-agent.json: %v", err)
	}
	return cfg.Skills
}

// TestApplyInlineConfigUpdate_RepeatedStartsDoNotGrowSkills verifies that
// applying the same inline config on every start leaves the skill list in
// scion-agent.json unchanged instead of appending another copy each time.
func TestApplyInlineConfigUpdate_RepeatedStartsDoNotGrowSkills(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "skills-agent"

	inline := &api.ScionConfig{Skills: []api.SkillReference{
		{URI: "skill://scion/global/alpha", Scope: "hub"},
		{URI: "gh://example/repo/skills/beta"},
		// Same URI under a different install name is a separate install.
		{URI: "gh://example/repo/skills/beta", As: "beta-2"},
	}}

	// The provisioned file already holds the skills from the create-time
	// inline config (plus one template skill the Hub does not resend).
	provisioned := config.MergeScionConfig(&api.ScionConfig{
		Harness: "claude",
		Skills:  []api.SkillReference{{URI: "skill://scion/global/from-template", Scope: "template"}},
	}, inline)
	cfgPath := writeAgentConfig(t, projectDir, agentName, *provisioned)
	want := readAgentSkills(t, cfgPath)
	if len(want) != 4 {
		t.Fatalf("fixture: expected 4 provisioned skills, got %d: %+v", len(want), want)
	}

	for i := 0; i < 3; i++ { // start, restart, restart
		srv.applyInlineConfigUpdate(agentName, projectDir, inline, false)
		got := readAgentSkills(t, cfgPath)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after apply #%d: skills changed\n got: %+v\nwant: %+v", i+1, got, want)
		}
	}
}

// TestApplyInlineConfigUpdate_CollapsesExistingDuplicates verifies that a
// scion-agent.json already carrying repeated skill entries is reduced to one
// entry per URI and install name on the next start.
func TestApplyInlineConfigUpdate_CollapsesExistingDuplicates(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "dup-agent"

	alpha := api.SkillReference{URI: "skill://scion/global/alpha", Scope: "template"}
	beta := api.SkillReference{URI: "skill://scion/global/beta", Scope: "template"}
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{
		Skills: []api.SkillReference{alpha, beta, alpha, beta, alpha, beta},
	})

	srv.applyInlineConfigUpdate(agentName, projectDir, &api.ScionConfig{
		Skills: []api.SkillReference{{URI: alpha.URI}, {URI: beta.URI}},
	}, false)

	got := readAgentSkills(t, cfgPath)
	if want := []api.SkillReference{alpha, beta}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skills not collapsed\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDedupeSkillReferences(t *testing.T) {
	tests := []struct {
		name string
		in   []api.SkillReference
		want []api.SkillReference
	}{
		{name: "nil", in: nil, want: nil},
		{
			name: "single",
			in:   []api.SkillReference{{URI: "a"}},
			want: []api.SkillReference{{URI: "a"}},
		},
		{
			name: "distinct URIs kept in order",
			in:   []api.SkillReference{{URI: "b"}, {URI: "a"}},
			want: []api.SkillReference{{URI: "b"}, {URI: "a"}},
		},
		{
			name: "same URI different As kept",
			in:   []api.SkillReference{{URI: "a"}, {URI: "a", As: "x"}, {URI: "a", As: "y"}},
			want: []api.SkillReference{{URI: "a"}, {URI: "a", As: "x"}, {URI: "a", As: "y"}},
		},
		{
			name: "lower-ranked later duplicate does not replace (template over hub)",
			in: []api.SkillReference{
				{URI: "a", Scope: "template"},
				{URI: "b"},
				{URI: "a", Optional: true, Scope: "hub"},
			},
			want: []api.SkillReference{
				{URI: "a", Scope: "template"},
				{URI: "b"},
			},
		},
		{
			name: "lower-ranked later duplicate does not replace (project over template)",
			in: []api.SkillReference{
				{URI: "a", Scope: "project"},
				{URI: "a", Optional: true, Scope: "template"},
			},
			want: []api.SkillReference{
				{URI: "a", Scope: "project"},
			},
		},
		{
			name: "higher-ranked later duplicate replaces in first position",
			in: []api.SkillReference{
				{URI: "a", Scope: "hub"},
				{URI: "b"},
				{URI: "a", Optional: true, Scope: "project"},
			},
			want: []api.SkillReference{
				{URI: "a", Optional: true, Scope: "project"},
				{URI: "b"},
			},
		},
		{
			name: "equal rank goes to the later entry",
			in: []api.SkillReference{
				{URI: "a", Scope: "template"},
				{URI: "a", Optional: true, Scope: "template"},
			},
			want: []api.SkillReference{
				{URI: "a", Optional: true, Scope: "template"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dedupeSkillReferences(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
