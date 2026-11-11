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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// placeholderDefaultTemplate returns a template laid out like the shipped
// default: system_prompt names system-prompt.md, which holds "# Placeholder".
func placeholderDefaultTemplate(t *testing.T) *config.Template {
	t.Helper()
	dir := t.TempDir()
	writeFileString(t, filepath.Join(dir, "system-prompt.md"), "# Placeholder\n")
	return &config.Template{Name: "default", Path: dir}
}

func newPlaceholderTestHarness(t *testing.T) api.Harness {
	t.Helper()
	h, err := harness.NewContainerScriptHarness("/fake/harness-config-dir", config.HarnessConfigEntry{
		Harness:     "claude",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func stagedSystemPromptPath(agentHome string) string {
	return filepath.Join(agentHome, ".scion", "harness", "inputs", "system-prompt.md")
}

// TestInjectPromptFiles_PlaceholderSystemPrompt: the default template's
// placeholder, inherited by a template without its own system prompt (or
// used alone), is not staged as a system prompt (ptone/scion#7288); a real
// prompt from the template or from inline config still is.
func TestInjectPromptFiles_PlaceholderSystemPrompt(t *testing.T) {
	realTemplate := func(t *testing.T) *config.Template {
		dir := t.TempDir()
		writeFileString(t, filepath.Join(dir, "system-prompt.md"), "You are a reviewer.\n")
		return &config.Template{Name: "reviewer", Path: dir}
	}
	bareTemplate := func(t *testing.T) *config.Template {
		return &config.Template{Name: "bare", Path: t.TempDir()}
	}

	tests := []struct {
		name         string
		chain        func(t *testing.T) []*config.Template
		systemPrompt string
		inline       bool
		want         string // staged content; "" means nothing staged
	}{
		{
			name: "non-default template without a system prompt",
			chain: func(t *testing.T) []*config.Template {
				return []*config.Template{placeholderDefaultTemplate(t), bareTemplate(t)}
			},
			systemPrompt: "system-prompt.md",
		},
		{
			name:         "default template alone",
			chain:        func(t *testing.T) []*config.Template { return []*config.Template{placeholderDefaultTemplate(t)} },
			systemPrompt: "system-prompt.md",
		},
		{
			name: "template with its own system prompt",
			chain: func(t *testing.T) []*config.Template {
				return []*config.Template{placeholderDefaultTemplate(t), realTemplate(t)}
			},
			systemPrompt: "system-prompt.md",
			want:         "You are a reviewer.\n",
		},
		{
			name: "inline system prompt",
			chain: func(t *testing.T) []*config.Template {
				return []*config.Template{placeholderDefaultTemplate(t), bareTemplate(t)}
			},
			systemPrompt: "You are inline.\nBe brief.",
			inline:       true,
			want:         "You are inline.\nBe brief.",
		},
		{
			// Accepted edge: inline content is staged as given, even when
			// it is literally the placeholder; readSystemPrompt drops it
			// at launch.
			name: "inline system prompt that is the placeholder",
			chain: func(t *testing.T) []*config.Template {
				return []*config.Template{placeholderDefaultTemplate(t), bareTemplate(t)}
			},
			systemPrompt: "# Placeholder",
			inline:       true,
			want:         "# Placeholder",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agentHome := t.TempDir()
			cfg := &api.ScionConfig{SystemPrompt: tc.systemPrompt}
			var inline *api.ScionConfig
			if tc.inline {
				inline = &api.ScionConfig{SystemPrompt: tc.systemPrompt}
			}
			rendered, err := injectPromptFiles(newPlaceholderTestHarness(t), agentHome, tc.chain(t), cfg, inline)
			if err != nil {
				t.Fatal(err)
			}
			staged := stagedSystemPromptPath(agentHome)
			if tc.want == "" {
				if rendered {
					t.Errorf("rendered = true, want false for placeholder content")
				}
				if _, err := os.Stat(staged); !os.IsNotExist(err) {
					t.Errorf("placeholder staged as a system prompt (err %v): %q", err, readFileString(t, staged))
				}
				return
			}
			if !rendered {
				t.Errorf("rendered = false, want true")
			}
			if got := readFileString(t, staged); got != tc.want {
				t.Errorf("staged system prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRerender_PlaceholderSystemPromptLeavesInputsRecord: a start-time
// re-render of an agent whose recorded system prompt is the placeholder
// (provisioned before ptone/scion#7288 was fixed) removes it from the
// control-plane inputs record instead of restaging it.
func TestRerender_PlaceholderSystemPromptLeavesInputsRecord(t *testing.T) {
	f := newRerenderFixture(t)
	record := filepath.Join(f.agentDir, controlPlaneInputsDirName, "system-prompt.md")
	writeFileString(t, record, "# Placeholder\n")

	cfg := &api.ScionConfig{SystemPrompt: "system-prompt.md"}
	chain := []*config.Template{placeholderDefaultTemplate(t)}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(newPlaceholderTestHarness(t), []string{"system_prompt"}, cfg, nil, chain)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("placeholder system prompt kept in the inputs record (err %v)", err)
	}
}
