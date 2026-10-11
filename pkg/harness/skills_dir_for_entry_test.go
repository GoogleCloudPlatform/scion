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

package harness

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestSkillsDirForEntry pins the entry-to-skills-dir rule over the entry
// shapes Resolve distinguishes, and checks it against the SkillsDir of the
// harness Resolve's paths build from the same entry (ptone/scion#4158).
func TestSkillsDirForEntry(t *testing.T) {
	usable := &config.HarnessProvisionerConfig{
		Type:             "container-script",
		InterfaceVersion: 1,
		Command:          []string{"python3", "/home/scion/.scion/harness/provision.py"},
	}
	builtin := &config.HarnessProvisionerConfig{Type: LegacyBuiltinProvisionerType}
	noCommand := &config.HarnessProvisionerConfig{Type: "container-script", InterfaceVersion: 1}

	tests := []struct {
		name  string
		entry config.HarnessConfigEntry
		want  string
		// buildable is false for entries Resolve rejects (unusable
		// provisioner), which build no harness to compare with.
		buildable bool
	}{
		{"zero entry", config.HarnessConfigEntry{}, GenericSkillsDir, true},
		{"harness name only", config.HarnessConfigEntry{Harness: "claude"}, GenericSkillsDir, true},
		{"declarative with skills_dir", config.HarnessConfigEntry{Harness: "gemini", SkillsDir: ".decl/skills"}, ".decl/skills", true},
		{"declarative command without skills_dir", config.HarnessConfigEntry{Harness: "gemini", Command: &config.HarnessCommandConfig{Base: []string{"run"}}}, GenericSkillsDir, true},
		{"declarative config_dir without skills_dir", config.HarnessConfigEntry{Harness: "gemini", ConfigDir: ".gem"}, GenericSkillsDir, true},
		{"container-script with skills_dir", config.HarnessConfigEntry{Harness: "claude", SkillsDir: ".claude/skills", Provisioner: usable}, ".claude/skills", true},
		{"container-script without skills_dir", config.HarnessConfigEntry{Harness: "claude", Provisioner: usable}, "", true},
		{"container-script with config_dir only", config.HarnessConfigEntry{Harness: "claude", ConfigDir: ".claude", Provisioner: usable}, "", true},
		{"unusable builtin provisioner with skills_dir", config.HarnessConfigEntry{Harness: "claude", SkillsDir: ".claude/skills", Provisioner: builtin}, ".claude/skills", false},
		{"unusable builtin provisioner without skills_dir", config.HarnessConfigEntry{Harness: "claude", Provisioner: builtin}, "", false},
		{"provisioner without command with skills_dir", config.HarnessConfigEntry{Harness: "claude", SkillsDir: ".x/skills", Provisioner: noCommand}, ".x/skills", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SkillsDirForEntry(tt.entry); got != tt.want {
				t.Errorf("SkillsDirForEntry = %q, want %q", got, tt.want)
			}
			if !tt.buildable {
				return
			}
			// Build the harness the way Resolve's paths do.
			var built interface{ SkillsDir() string }
			switch {
			case tt.entry.Provisioner != nil:
				h, err := NewContainerScriptHarness(t.TempDir(), tt.entry)
				if err != nil {
					t.Fatalf("NewContainerScriptHarness: %v", err)
				}
				built = h
			case hasDeclarativeMetadata(tt.entry):
				built = NewDeclarativeGenericHarness(tt.entry)
			default:
				built = &Generic{}
			}
			if got, want := SkillsDirForEntry(tt.entry), built.SkillsDir(); got != want {
				t.Errorf("SkillsDirForEntry = %q, built harness SkillsDir = %q", got, want)
			}
		})
	}
}
