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
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func writeSkill(t *testing.T, skillsDir, name, content string) {
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readSkill(t *testing.T, skillsDir, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(skillsDir, name, "SKILL.md"))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func TestCarryOverSkillsDir(t *testing.T) {
	t.Run("copies into a missing dir and keeps the old one", func(t *testing.T) {
		home := t.TempDir()
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "1")
		writeSkill(t, filepath.Join(home, ".a/skills"), "two", "2")
		copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
		if err != nil {
			t.Fatal(err)
		}
		if len(copied) != 2 {
			t.Fatalf("copied = %v, want 2 skills", copied)
		}
		for _, n := range []string{"one", "two"} {
			if _, ok := readSkill(t, filepath.Join(home, ".b/skills"), n); !ok {
				t.Errorf("skill %s not copied to the new dir", n)
			}
			if _, ok := readSkill(t, filepath.Join(home, ".a/skills"), n); !ok {
				t.Errorf("skill %s removed from the old dir", n)
			}
		}
	})
	t.Run("copies into an empty dir", func(t *testing.T) {
		home := t.TempDir()
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "1")
		_ = os.MkdirAll(filepath.Join(home, ".b/skills"), 0755)
		if _, err := carryOverSkillsDir(home, ".a/skills", ".b/skills"); err != nil {
			t.Fatal(err)
		}
		if _, ok := readSkill(t, filepath.Join(home, ".b/skills"), "one"); !ok {
			t.Error("skill not copied into the empty dir")
		}
	})
	t.Run("leaves a non-empty dir alone", func(t *testing.T) {
		home := t.TempDir()
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "old")
		writeSkill(t, filepath.Join(home, ".b/skills"), "other", "b")
		copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
		if err != nil || len(copied) != 0 {
			t.Fatalf("copied=%v err=%v, want nothing", copied, err)
		}
		if _, ok := readSkill(t, filepath.Join(home, ".b/skills"), "one"); ok {
			t.Error("skill copied into a non-empty dir")
		}
	})
	t.Run("no-ops", func(t *testing.T) {
		home := t.TempDir()
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "1")
		for _, tc := range []struct{ old, new string }{
			{".a/skills", ".a/skills"},
			{".a/skills/", "./.a/skills"},
			{"", ".b/skills"},
			{".a/skills", ""},
			{".missing/skills", ".b/skills"},
			{".a/skills", "../outside/skills"},
			{"../.a/skills", ".b/skills"},
		} {
			copied, err := carryOverSkillsDir(home, tc.old, tc.new)
			if err != nil || len(copied) != 0 {
				t.Errorf("carryOverSkillsDir(%q, %q) = %v, %v; want no copy", tc.old, tc.new, copied, err)
			}
		}
		if _, err := os.Stat(filepath.Join(home, ".b")); !os.IsNotExist(err) {
			t.Error("a no-op created the new skills dir")
		}
	})
}

// TestStart_HarnessConfigSwitchCarriesSkillsOver verifies that starting an
// existing agent with a --harness-config whose skills_dir differs from the
// provisioned harness-config's copies the provisioned skills into the new
// skills dir, leaving the old one, and that a start without a switch does
// not (ptone/scion#3129).
func TestStart_HarnessConfigSwitchCarriesSkillsOver(t *testing.T) {
	for _, tc := range []struct {
		name          string
		harnessConfig string
		wantInB       bool
	}{
		{name: "switch to a different skills_dir", harnessConfig: "hc-b", wantInB: true},
		{name: "no switch", harnessConfig: "", wantInB: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			_ = os.Chdir(tmpDir)
			defer func() { _ = os.Chdir(oldWd) }()
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			for name, skillsDir := range map[string]string{"hc-a": ".a/skills", "hc-b": ".b/skills"} {
				hcDir := filepath.Join(globalScionDir, "harness-configs", name)
				_ = os.MkdirAll(hcDir, 0755)
				_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"),
					[]byte("harness: generic\nuser: scion\nimage: test-image:latest\nskills_dir: "+skillsDir+"\n"), 0644)
			}
			tplDir := filepath.Join(globalScionDir, "templates", "default")
			_ = os.MkdirAll(tplDir, 0755)
			_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "hc-a"}`), 0644)
			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			_ = os.MkdirAll(projectScionDir, 0755)
			agentDir := filepath.Join(projectScionDir, "agents", "switcher")
			agentHome := filepath.Join(agentDir, "home")
			writeSkill(t, filepath.Join(agentHome, ".a/skills"), "my-skill", "provisioned")
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
				[]byte(`{"harness": "generic", "harness_config": "hc-a"}`), 0644)

			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					return "mock-id", nil
				},
			}
			if _, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:          "switcher",
				ProjectPath:   projectScionDir,
				HarnessConfig: tc.harnessConfig,
				BrokerMode:    true,
				NoAuth:        true,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			got, inB := readSkill(t, filepath.Join(agentHome, ".b/skills"), "my-skill")
			if inB != tc.wantInB {
				t.Fatalf("skill under .b/skills = %v, want %v", inB, tc.wantInB)
			}
			if inB && got != "provisioned" {
				t.Errorf("copied skill content = %q, want %q", got, "provisioned")
			}
			if _, ok := readSkill(t, filepath.Join(agentHome, ".a/skills"), "my-skill"); !ok {
				t.Error("skill removed from the previously provisioned skills dir")
			}
		})
	}
}
