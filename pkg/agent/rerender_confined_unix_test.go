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

//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// Start-time re-render writes are confined to the agent home: links the
// agent's container can plant in its home are replaced or refused, never
// written through.

// outsideDir is a directory outside the agent home holding one file, whose
// content the tests check is never changed.
func outsideDir(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "victim")
	writeFileString(t, file, "outside content")
	return dir, file
}

func assertOutsideUntouched(t *testing.T, dir, file string) {
	t.Helper()
	if got := readFileString(t, file); got != "outside content" {
		t.Fatalf("a file outside the agent home was written: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries created outside the agent home: %v", entries)
	}
}

// TestRerender_PlantedSymlinkLeafIsReplaced: a link at the instructions
// file is replaced by the rendered file; its target is untouched.
func TestRerender_PlantedSymlinkLeafIsReplaced(t *testing.T) {
	f := newRerenderFixture(t)
	dir, victim := outsideDir(t)
	if err := os.Symlink(victim, filepath.Join(f.agentHome, "agents.md")); err != nil {
		t.Fatal(err)
	}
	cfg := &api.ScionConfig{AgentInstructions: "new instructions"}
	inline := &api.ScionConfig{AgentInstructions: "new instructions", ChangedFields: []string{"agent_instructions"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"agent_instructions"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	assertOutsideUntouched(t, dir, victim)
	fi, err := os.Lstat(filepath.Join(f.agentHome, "agents.md"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("agents.md is not a regular file after the re-render (mode %v, err %v)", fi.Mode(), err)
	}
}

// TestRerender_PlantedSymlinkDirectoryIsRefused: a link at a directory
// component (.scion) is refused: the start fails and nothing is written
// through it.
func TestRerender_PlantedSymlinkDirectoryIsRefused(t *testing.T) {
	f := newRerenderFixture(t)
	dir, victim := outsideDir(t)
	if err := os.Symlink(dir, filepath.Join(f.agentHome, ".scion")); err != nil {
		t.Fatal(err)
	}
	cfg, inline := rerenderPromptConfig("new prompt")
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"system_prompt"}, cfg, inline, nil)); err == nil {
		t.Fatal("a re-render through a linked .scion directory succeeded")
	}
	assertOutsideUntouched(t, dir, victim)

	services := &api.ScionConfig{Services: []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"services"}, services, services, nil)); err == nil {
		t.Fatal("a services re-render through a linked .scion directory succeeded")
	}
	assertOutsideUntouched(t, dir, victim)
}

// TestRerender_ReplantedBetweenWritesIsRefused: each write re-checks its
// directories, so a link planted after one write is refused by the next.
func TestRerender_ReplantedBetweenWritesIsRefused(t *testing.T) {
	f := newRerenderFixture(t)
	cfg, inline := rerenderPromptConfig("first prompt")
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"system_prompt"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	dir, victim := outsideDir(t)
	scionDir := filepath.Join(f.agentHome, ".scion")
	if err := os.RemoveAll(scionDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, scionDir); err != nil {
		t.Fatal(err)
	}
	if err := rerenderServices(f.agentHome, []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}}); err == nil {
		t.Fatal("a write through a re-planted link succeeded")
	}
	if err := writeHomeFile(f.agentHome, ".scion/system_prompt.md", []byte("second"), 0o644); err == nil {
		t.Fatal("a write through a re-planted link succeeded")
	}
	assertOutsideUntouched(t, dir, victim)
}

// TestRerender_RemovalNeverFollowsLinks: removing a cleared prompt or the
// services file removes a planted link itself, not its target, and refuses
// a linked parent directory.
func TestRerender_RemovalNeverFollowsLinks(t *testing.T) {
	f := newRerenderFixture(t)
	dir, victim := outsideDir(t)
	if err := os.MkdirAll(filepath.Join(f.agentHome, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.agentHome, ".scion", servicesFileName)
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := rerenderServices(f.agentHome, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the planted link was not removed (err %v)", err)
	}
	assertOutsideUntouched(t, dir, victim)

	if err := os.RemoveAll(filepath.Join(f.agentHome, ".scion")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(f.agentHome, ".scion")); err != nil {
		t.Fatal(err)
	}
	if err := removeHomeFile(f.agentHome, ".scion/victim"); err == nil {
		t.Fatal("a removal through a linked directory succeeded")
	}
	assertOutsideUntouched(t, dir, victim)
}

// TestRerender_SkillsNeverFollowLinks: a linked skills directory is
// refused, and a skill directory replaced by a link is replaced by the
// installed skill without touching the link's target.
func TestRerender_SkillsNeverFollowLinks(t *testing.T) {
	h := &harness.Generic{}
	skills := []api.SkillReference{{URI: "hub://skills/review"}}
	cfg := &api.ScionConfig{Skills: skills}
	inline := &api.ScionConfig{Skills: skills, ChangedFields: []string{"skills"}}
	ctx := ContextWithSkillResolver(context.Background(), &countingSkillResolver{})

	t.Run("linked skills directory", func(t *testing.T) {
		f := newRerenderFixture(t)
		dir, victim := outsideDir(t)
		skillsDir := filepath.Join(f.agentHome, h.SkillsDir())
		if err := os.MkdirAll(filepath.Dir(skillsDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, skillsDir); err != nil {
			t.Fatal(err)
		}
		if _, err := rerenderProvisionedFiles(ctx, f.inputs(h, []string{"skills"}, cfg, inline, nil)); err == nil {
			t.Fatal("a skill install through a linked skills directory succeeded")
		}
		assertOutsideUntouched(t, dir, victim)
	})

	t.Run("linked skill directory", func(t *testing.T) {
		f := newRerenderFixture(t)
		dir, victim := outsideDir(t)
		skillsDir := filepath.Join(f.agentHome, h.SkillsDir())
		if err := os.MkdirAll(skillsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(skillsDir, "review")); err != nil {
			t.Fatal(err)
		}
		if _, err := rerenderProvisionedFiles(ctx, f.inputs(h, []string{"skills"}, cfg, inline, nil)); err != nil {
			t.Fatal(err)
		}
		assertOutsideUntouched(t, dir, victim)
		fi, err := os.Lstat(filepath.Join(skillsDir, "review"))
		if err != nil || !fi.IsDir() {
			t.Fatalf("the skill is not a real directory after the install (err %v)", err)
		}
		if _, err := os.Stat(filepath.Join(skillsDir, "review", "SKILL.md")); err != nil {
			t.Fatalf("skill not installed: %v", err)
		}
	})
}
