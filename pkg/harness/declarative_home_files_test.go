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

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const outsideMarker = "outside-marker"

// homeWithOutside returns an agent home and, beside it (outside the home),
// a directory holding a marker file.
func homeWithOutside(t *testing.T) (agentHome, outsideDir, marker string) {
	t.Helper()
	base := t.TempDir()
	agentHome = filepath.Join(base, "home")
	outsideDir = filepath.Join(base, "outside")
	for _, d := range []string{agentHome, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker = filepath.Join(outsideDir, "marker.md")
	writeFile(t, marker, outsideMarker)
	return agentHome, outsideDir, marker
}

// assertOutsideUnchanged fails when outsideDir holds anything but the
// untouched marker file.
func assertOutsideUnchanged(t *testing.T, outsideDir string) {
	t.Helper()
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "marker.md" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("outside directory changed: %v", names)
	}
	data, err := os.ReadFile(filepath.Join(outsideDir, "marker.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != outsideMarker {
		t.Fatalf("outside marker changed: %q", data)
	}
}

// declarativeWrites are the declarative harness's writes into the agent
// home, each for an entry naming its target.
var declarativeWrites = map[string]func(entry config.HarnessConfigEntry, agentHome string) error{
	"instructions": func(e config.HarnessConfigEntry, home string) error {
		return NewDeclarativeGenericHarness(e).InjectAgentInstructions(home, []byte("instructions"))
	},
	"system prompt native": func(e config.HarnessConfigEntry, home string) error {
		e.SystemPromptMode = "native"
		return NewDeclarativeGenericHarness(e).InjectSystemPrompt(home, []byte("prompt"))
	},
	"system prompt prepend": func(e config.HarnessConfigEntry, home string) error {
		e.SystemPromptMode = "prepend_to_instructions"
		return NewDeclarativeGenericHarness(e).InjectSystemPrompt(home, []byte("prompt"))
	},
}

// entryTargeting returns an entry whose instructions and system prompt
// files are both rel.
func entryTargeting(rel string) config.HarnessConfigEntry {
	return config.HarnessConfigEntry{Harness: "decl", InstructionsFile: rel, SystemPromptFile: rel}
}

// A path that leaves the agent home is refused at write, even when the
// entry never went through config.yaml parsing, and nothing is written
// outside the home.
func TestDeclarativeGenericWritesRefuseNonLocalPaths(t *testing.T) {
	for name, write := range declarativeWrites {
		for _, rel := range []string{"../outside/written.md", "../outside/marker.md", "sub/../../outside/written.md"} {
			t.Run(name+"/"+rel, func(t *testing.T) {
				agentHome, outsideDir, _ := homeWithOutside(t)
				err := write(entryTargeting(rel), agentHome)
				if err == nil {
					t.Fatal("write accepted a path outside the agent home")
				}
				if strings.Contains(err.Error(), rel) {
					t.Errorf("error %q carries the value", err)
				}
				assertOutsideUnchanged(t, outsideDir)
			})
		}
	}
}

// A symbolic link as a directory component inside the agent home is refused
// at write, and nothing is written or created through it.
func TestDeclarativeGenericWritesRefuseLinkedDirectoryInHome(t *testing.T) {
	for name, write := range declarativeWrites {
		for _, rel := range []string{"linked/agents.md", "linked/new/agents.md"} {
			t.Run(name+"/"+rel, func(t *testing.T) {
				agentHome, outsideDir, _ := homeWithOutside(t)
				if err := os.Symlink(outsideDir, filepath.Join(agentHome, "linked")); err != nil {
					t.Fatal(err)
				}
				if err := write(entryTargeting(rel), agentHome); err == nil {
					t.Fatal("write went through a linked directory in the agent home")
				}
				assertOutsideUnchanged(t, outsideDir)
			})
		}
	}
}

// A symbolic link at the target file is replaced, not written through, and
// prepend mode does not read the linked file's content into the home.
func TestDeclarativeGenericWritesReplaceLinkedFileInHome(t *testing.T) {
	for name, write := range declarativeWrites {
		t.Run(name, func(t *testing.T) {
			agentHome, outsideDir, marker := homeWithOutside(t)
			target := filepath.Join(agentHome, "agents.md")
			if err := os.Symlink(marker, target); err != nil {
				t.Fatal(err)
			}
			werr := write(entryTargeting("agents.md"), agentHome)
			assertOutsideUnchanged(t, outsideDir)
			fi, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				// The link was left in place: the write must have failed.
				if werr == nil {
					t.Fatal("write reported success but the link is still in place")
				}
				return
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), outsideMarker) {
				t.Fatalf("content from outside the home was read into it: %q", data)
			}
		})
	}
}

// Prepend mode reads nothing outside the agent home: an instructions file
// reached through a linked directory or hard-linked from outside is not
// read.
func TestDeclarativeGenericPrependReadsOnlyInsideHome(t *testing.T) {
	prepend := func(t *testing.T, agentHome, rel string) {
		t.Helper()
		e := entryTargeting(rel)
		e.SystemPromptMode = "prepend_to_instructions"
		if err := NewDeclarativeGenericHarness(e).InjectSystemPrompt(agentHome, []byte("prompt")); err == nil {
			t.Errorf("%s: prepend read a file that is not confined to the agent home", rel)
		}
	}
	t.Run("linked directory", func(t *testing.T) {
		agentHome, outsideDir, _ := homeWithOutside(t)
		if err := os.Symlink(outsideDir, filepath.Join(agentHome, "linked")); err != nil {
			t.Fatal(err)
		}
		prepend(t, agentHome, "linked/marker.md")
		assertOutsideUnchanged(t, outsideDir)
	})
	t.Run("hard link", func(t *testing.T) {
		agentHome, outsideDir, marker := homeWithOutside(t)
		if err := os.Link(marker, filepath.Join(agentHome, "hard.md")); err != nil {
			t.Skipf("hard link: %v", err)
		}
		prepend(t, agentHome, "hard.md")
		assertOutsideUnchanged(t, outsideDir)
	})
}

// HasSystemPrompt does not report a system_prompt_file outside the home or
// reached through a link.
func TestDeclarativeGenericHasSystemPromptOnlyInsideHome(t *testing.T) {
	agentHome, outsideDir, marker := homeWithOutside(t)
	if err := os.Symlink(outsideDir, filepath.Join(agentHome, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(marker, filepath.Join(agentHome, "prompt.md")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside/marker.md", "linked/marker.md", "prompt.md"} {
		if NewDeclarativeGenericHarness(entryTargeting(rel)).HasSystemPrompt(agentHome) {
			t.Errorf("HasSystemPrompt reported %s", rel)
		}
		c := &ContainerScriptHarness{entry: entryTargeting(rel)}
		if c.HasSystemPrompt(agentHome) {
			t.Errorf("container-script HasSystemPrompt reported %s", rel)
		}
	}
}

// Control: normal relative paths, nested or not, still work, including the
// defaults, the case-variant cleanup and prepend onto existing content.
func TestDeclarativeGenericWritesLocalPaths(t *testing.T) {
	agentHome := t.TempDir()
	writeFile(t, filepath.Join(agentHome, ".decl", "agents.md"), "old variant")

	d := NewDeclarativeGenericHarness(config.HarnessConfigEntry{
		Harness:          "decl",
		InstructionsFile: ".decl/AGENTS.md",
		SystemPromptFile: ".decl/prompt/system.md",
	})
	if err := d.InjectAgentInstructions(agentHome, []byte("instructions")); err != nil {
		t.Fatalf("InjectAgentInstructions: %v", err)
	}
	if err := d.InjectSystemPrompt(agentHome, []byte("prompt")); err != nil {
		t.Fatalf("InjectSystemPrompt: %v", err)
	}
	if !d.HasSystemPrompt(agentHome) {
		t.Error("HasSystemPrompt did not report the written prompt")
	}
	entries, err := os.ReadDir(filepath.Join(agentHome, ".decl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "agents.md" {
			t.Error("case-variant instructions file was not removed")
		}
	}
	assertFile(t, filepath.Join(agentHome, ".decl", "AGENTS.md"), "instructions")
	assertFile(t, filepath.Join(agentHome, ".decl", "prompt", "system.md"), "prompt")

	p := NewDeclarativeGenericHarness(config.HarnessConfigEntry{
		Harness:          "decl",
		InstructionsFile: ".decl/AGENTS.md",
		SystemPromptMode: "prepend_to_instructions",
	})
	if err := p.InjectSystemPrompt(agentHome, []byte("prompt")); err != nil {
		t.Fatalf("prepend: %v", err)
	}
	assertFile(t, filepath.Join(agentHome, ".decl", "AGENTS.md"), "# System Prompt\n\nprompt\n\n---\n\ninstructions")

	defaults := NewDeclarativeGenericHarness(config.HarnessConfigEntry{Harness: "decl"})
	fresh := filepath.Join(t.TempDir(), "new-home")
	if err := defaults.InjectAgentInstructions(fresh, []byte("i")); err != nil {
		t.Fatalf("default instructions: %v", err)
	}
	if err := defaults.InjectSystemPrompt(fresh, []byte("p")); err != nil {
		t.Fatalf("default prompt: %v", err)
	}
	assertFile(t, filepath.Join(fresh, "agents.md"), "i")
	assertFile(t, filepath.Join(fresh, ".scion", "system_prompt.md"), "p")
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
