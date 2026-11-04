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
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestContainerScriptHarness_GetCommand_PlaceholderSystemPrompt: the
// default template's placeholder never reaches the system prompt flag
// (ptone/scion#7288). A staged placeholder, left by an agent provisioned
// before the fix, falls through to the native file; a real staged prompt
// still wins over the native file.
func TestContainerScriptHarness_GetCommand_PlaceholderSystemPrompt(t *testing.T) {
	tests := []struct {
		name   string
		staged string // inputs/system-prompt.md; "" means absent
		native string // .claude/system-prompt.md; "" means absent
		want   []string
	}{
		{
			name:   "placeholder staged, real native file",
			staged: "# Placeholder\n",
			native: "Native prompt.\n",
			want:   []string{"claude", "--system-prompt", "Native prompt."},
		},
		{
			name:   "placeholder in both files",
			staged: "# Placeholder\n",
			native: "# placeholder",
			want:   []string{"claude"},
		},
		{
			name:   "real staged prompt",
			staged: "Staged prompt.\n",
			native: "Native prompt.\n",
			want:   []string{"claude", "--system-prompt", "Staged prompt."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewContainerScriptHarness(t.TempDir(), config.HarnessConfigEntry{
				Harness:          "claude",
				SystemPromptFile: ".claude/system-prompt.md",
				Provisioner:      &config.HarnessProvisionerConfig{Type: "container-script"},
				Command: &config.HarnessCommandConfig{
					Base:             []string{"claude"},
					SystemPromptFlag: "--system-prompt",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			h.agentHome = t.TempDir()
			if tc.staged != "" {
				writeFile(t, filepath.Join(h.agentHome, ".scion", "harness", "inputs", "system-prompt.md"), tc.staged)
			}
			if tc.native != "" {
				writeFile(t, filepath.Join(h.agentHome, ".claude", "system-prompt.md"), tc.native)
			}
			if got := h.GetCommand("", false, nil); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("GetCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}
