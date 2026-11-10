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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The template named in agent-info.json (which the agent can write) is used
// by clearedFieldsInheritance only when it is a template slug: an absolute
// directory, a relative path or a remote source never contributes inherited
// values. Ignoring it logs a warning that names the agent and does not echo
// the value.
func TestClearedFieldsInheritanceIgnoresNonSlugAgentInfoTemplate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	projectDir := filepath.Join(base, "proj", config.DotScion)
	agentName := "cf-agent"
	home := config.GetAgentHomePath(projectDir, agentName)
	agentDir := config.GetAgentDir(projectDir, agentName, false)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTemplate := func(dir string, maxTurns int) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg, _ := json.Marshal(map[string]any{"max_turns": maxTurns})
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), cfg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(base, "host-dir")
	writeTemplate(outside, 77)
	writeTemplate(filepath.Join(projectDir, "templates", "named"), 55)

	s := &Server{}
	inherit := func(template string) *int {
		t.Helper()
		info, _ := json.Marshal(map[string]any{"template": template})
		if err := os.WriteFile(filepath.Join(home, "agent-info.json"), info, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := s.clearedFieldsInheritance(context.Background(), filepath.Dir(projectDir), projectDir, agentDir, agentName)
		if err != nil || cfg == nil {
			return nil
		}
		return cfg.MaxTurns
	}

	logs := captureLogs(t)
	const warning = "not a template name"

	// Control: a template slug from agent-info.json is used, without a warning.
	if got := inherit("named"); got == nil || *got != 55 {
		t.Fatalf("control: max_turns = %v, want 55 from the named template", got)
	}
	if strings.Contains(logs.String(), warning) {
		t.Fatalf("control: a template slug logged a warning: %s", logs.String())
	}
	rel, err := filepath.Rel(filepath.Join(projectDir, "templates"), outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{outside, rel, ":local:" + outside} {
		logs.Reset()
		if got := inherit(v); got != nil && *got == 77 {
			t.Fatalf("agent-info.json template %q contributed values from a directory outside the template roots", v)
		}
		out := logs.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, warning) || !strings.Contains(out, "agent="+agentName) {
			t.Errorf("agent-info.json template %q: no warning naming the agent: %s", v, out)
		}
		if strings.Contains(out, v) {
			t.Errorf("the warning echoes the agent-info.json template %q: %s", v, out)
		}
	}
}
