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
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
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

// snapshotSkillsTree records every path under dir (relative, with "/" separators)
// and the content of each regular file, without following symbolic links.
func snapshotSkillsTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "link:" + target
		case fi.Mode().IsRegular():
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[rel] = "file:" + string(data)
		default:
			out[rel] = "dir"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// TestCarryOverSkillsDir_StaysWithinAgentHome verifies that the carry-over
// only reads and writes inside the agent home: a symbolic link for the old
// skills dir, for a file inside a skill, or for a parent component of the
// new skills dir is not followed, while the regular files still copy.
func TestCarryOverSkillsDir_StaysWithinAgentHome(t *testing.T) {
	t.Run("old skills dir is a symbolic link", func(t *testing.T) {
		base := t.TempDir()
		home, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
		writeSkill(t, filepath.Join(outside, "skills"), "elsewhere", "outside")
		symlink(t, filepath.Join(outside, "skills"), filepath.Join(home, ".a/skills"))
		before := snapshotSkillsTree(t, outside)

		copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
		if err != nil {
			t.Fatal(err)
		}
		if len(copied) != 0 {
			t.Errorf("copied = %v through a symbolic link, want nothing", copied)
		}
		if _, err := os.Lstat(filepath.Join(home, ".b")); !os.IsNotExist(err) {
			t.Errorf("new skills dir created from a symbolic-link source (err=%v)", err)
		}
		if after := snapshotSkillsTree(t, outside); len(after) != len(before) {
			t.Errorf("outside dir changed: %v -> %v", before, after)
		}
	})

	t.Run("old skills dir has a symbolic-link parent", func(t *testing.T) {
		base := t.TempDir()
		home, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
		writeSkill(t, filepath.Join(outside, "skills"), "elsewhere", "outside")
		symlink(t, outside, filepath.Join(home, ".a"))

		copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
		if err != nil || len(copied) != 0 {
			t.Errorf("copied=%v err=%v through a symbolic-link parent, want nothing", copied, err)
		}
		if _, err := os.Lstat(filepath.Join(home, ".b")); !os.IsNotExist(err) {
			t.Errorf("new skills dir created from a symbolic-link parent (err=%v)", err)
		}
	})

	t.Run("file inside a skill is a symbolic link", func(t *testing.T) {
		base := t.TempDir()
		home, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0644); err != nil {
			t.Fatal(err)
		}
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "1")
		symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(home, ".a/skills/one/linked.txt"))
		symlink(t, outside, filepath.Join(home, ".a/skills/one/linked-dir"))

		copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
		if err != nil {
			t.Fatal(err)
		}
		if len(copied) != 1 {
			t.Fatalf("copied = %v, want the one skill", copied)
		}
		if got, ok := readSkill(t, filepath.Join(home, ".b/skills"), "one"); !ok || got != "1" {
			t.Errorf("regular SKILL.md not copied (got %q, %v)", got, ok)
		}
		for _, n := range []string{"linked.txt", "linked-dir"} {
			if _, err := os.Lstat(filepath.Join(home, ".b/skills/one", n)); !os.IsNotExist(err) {
				t.Errorf("symbolic link %s carried over (err=%v)", n, err)
			}
		}
	})

	t.Run("parent component of the new skills dir is a symbolic link", func(t *testing.T) {
		base := t.TempDir()
		home, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0755); err != nil {
			t.Fatal(err)
		}
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "1")
		symlink(t, outside, filepath.Join(home, ".b"))

		copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
		if err != nil {
			t.Logf("carry-over returned %v", err)
		}
		if len(copied) != 0 {
			t.Errorf("copied = %v through a symbolic-link parent, want nothing", copied)
		}
		if after := snapshotSkillsTree(t, outside); len(after) != 1 {
			t.Errorf("wrote outside the agent home: %v", after)
		}
		if _, ok := readSkill(t, filepath.Join(home, ".a/skills"), "one"); !ok {
			t.Error("old skill removed")
		}
	})

	t.Run("new skills dir is a symbolic link to an empty dir", func(t *testing.T) {
		base := t.TempDir()
		home, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0755); err != nil {
			t.Fatal(err)
		}
		writeSkill(t, filepath.Join(home, ".a/skills"), "one", "1")
		symlink(t, outside, filepath.Join(home, ".b/skills"))

		if copied, _ := carryOverSkillsDir(home, ".a/skills", ".b/skills"); len(copied) != 0 {
			t.Errorf("copied = %v through a symbolic link, want nothing", copied)
		}
		if after := snapshotSkillsTree(t, outside); len(after) != 1 {
			t.Errorf("wrote outside the agent home: %v", after)
		}
	})
}

// TestPreviousHarnessSkillsDir_MatchesResolve pins previousHarnessSkillsDir
// to the SkillsDir of the harness harness.Resolve builds for the same
// harness-config, for each harness type and config shape, so the two cannot
// drift apart silently.
func TestPreviousHarnessSkillsDir_MatchesResolve(t *testing.T) {
	const provisioner = "provisioner:\n  type: container-script\n  interface_version: 1\n  command: [\"python3\", \"/home/scion/.scion/harness/provision.py\"]\n"
	shapes := map[string]string{
		"container-script with skills_dir":    "skills_dir: .native/skills\n" + provisioner,
		"container-script without skills_dir": provisioner,
		"declarative with skills_dir":         "skills_dir: .decl/skills\n",
		"declarative without skills_dir":      "command:\n  base: [\"run-agent\"]\n",
		"plain generic":                       "",
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)

	type row struct{ name, implementation string }
	var rows []row
	for _, harnessType := range []string{"claude", "gemini", "codex", "opencode", "generic"} {
		names := make([]string, 0, len(shapes))
		for shape := range shapes {
			names = append(names, shape)
		}
		sort.Strings(names)
		for i, shape := range names {
			name := harnessType + "-" + string(rune('a'+i))
			hcDir := filepath.Join(home, ".scion", "harness-configs", name)
			if err := os.MkdirAll(hcDir, 0755); err != nil {
				t.Fatal(err)
			}
			body := "harness: " + harnessType + "\nimage: test-image:latest\nuser: scion\n" + shapes[shape]
			if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0755); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row{name: name, implementation: harnessType + "/" + shape})
		}
	}

	for _, r := range rows {
		t.Run(r.implementation, func(t *testing.T) {
			resolved, err := harness.Resolve(context.Background(), harness.ResolveOptions{Name: r.name})
			if err != nil {
				t.Fatalf("harness.Resolve(%q): %v", r.name, err)
			}
			want := resolved.Harness.SkillsDir()
			if got := previousHarnessSkillsDir(r.name, "", nil, nil, ""); got != want {
				t.Errorf("previousHarnessSkillsDir(%q) = %q, harness.Resolve (%s) SkillsDir = %q",
					r.name, got, resolved.Implementation, want)
			}
		})
	}
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
			t.Chdir(tmpDir)
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

func TestCarryOverSkillsDir_FailurePartwayIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for root")
	}
	for _, tc := range []struct {
		name      string
		newExists bool
	}{
		{"new dir missing", false},
		{"new dir empty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			oldDir, newDir := filepath.Join(home, ".a/skills"), filepath.Join(home, ".b/skills")
			// Entries are listed in name order: "a-good" copies, then "b-bad" fails.
			writeSkill(t, oldDir, "a-good", "good")
			writeSkill(t, oldDir, "b-bad", "bad")
			unreadable := filepath.Join(oldDir, "b-bad", "SKILL.md")
			if err := os.Chmod(unreadable, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(unreadable, 0644) })
			if tc.newExists {
				if err := os.MkdirAll(newDir, 0755); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := carryOverSkillsDir(home, ".a/skills", ".b/skills"); err == nil {
				t.Fatal("want an error for the unreadable skill file")
			}
			if tc.newExists {
				entries, err := os.ReadDir(newDir)
				if err != nil {
					t.Fatalf("pre-existing new dir removed: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("new dir holds %d entries after the failed copy, want 0", len(entries))
				}
			} else if _, err := os.Lstat(newDir); !os.IsNotExist(err) {
				t.Fatalf("new dir left behind after the failed copy (err=%v)", err)
			}

			if err := os.Chmod(unreadable, 0644); err != nil {
				t.Fatal(err)
			}
			copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(copied)
			if len(copied) != 2 || copied[0] != "a-good" || copied[1] != "b-bad" {
				t.Fatalf("retry copied = %v, want [a-good b-bad]", copied)
			}
			for name, want := range map[string]string{"a-good": "good", "b-bad": "bad"} {
				if got, ok := readSkill(t, newDir, name); !ok || got != want {
					t.Errorf("skill %s = %q (present %v), want %q", name, got, ok, want)
				}
			}
		})
	}
}

func TestCarryOverSkillsDir_SkipsPythonBytecode(t *testing.T) {
	home := t.TempDir()
	oldDir, newDir := filepath.Join(home, ".a/skills"), filepath.Join(home, ".b/skills")
	writeSkill(t, oldDir, "one", "1")
	for _, d := range []string{"one/__pycache__", "one/lib", "__pycache__"} {
		if err := os.MkdirAll(filepath.Join(oldDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"one/__pycache__/x.cpython-311.pyc", "one/lib/mod.pyc", "one/lib/mod.py", "__pycache__/y.pyc"} {
		if err := os.WriteFile(filepath.Join(oldDir, f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	copied, err := carryOverSkillsDir(home, ".a/skills", ".b/skills")
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || copied[0] != "one" {
		t.Fatalf("copied = %v, want [one]", copied)
	}
	got := snapshotSkillsTree(t, newDir)
	want := map[string]string{
		".":              "dir",
		"one":            "dir",
		"one/SKILL.md":   "file:1",
		"one/lib":        "dir",
		"one/lib/mod.py": "file:x",
	}
	if len(got) != len(want) {
		t.Fatalf("new dir = %v, want %v", got, want)
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("new dir = %v, want %v", got, want)
			break
		}
	}
}
