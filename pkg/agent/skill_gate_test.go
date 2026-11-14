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
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// Experiment IDs used by the fixtures. They are real, registered IDs read
// from the registry-backed gate (skillExperimentGate); the evaluator itself
// names none.
const (
	gateTestOn  = "web.chat_choices"
	gateTestOff = "hub.artifacts"
)

func onlyOn(ids ...string) func(string) bool {
	return func(id string) bool {
		for _, want := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
}

// captureWarnings swaps the default slog logger for one that records
// messages, for the duration of the test.
func captureWarnings(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestGateMarkdown(t *testing.T) {
	block := func(id string) string {
		return "intro\n<!-- scion:experiment " + id + " -->\ngated line\n<!-- scion:end-experiment -->\noutro\n"
	}
	tests := []struct {
		name     string
		in       string
		wantKeep bool
		want     string
	}{
		{"ungated file is unchanged", "# Title\n\nplain `scion:` text\n", true, "# Title\n\nplain `scion:` text\n"},
		{"block on keeps lines, drops markers", block(gateTestOn), true, "intro\ngated line\noutro\n"},
		{"block off drops lines and markers", block(gateTestOff), true, "intro\noutro\n"},
		{"indented markers", "a\n  <!--  scion:experiment " + gateTestOff + "  -->\nb\n  <!-- scion:end-experiment -->\nc\n", true, "a\nc\n"},
		{"file gate on", "---\nexperiment: " + gateTestOn + "\n---\nbody\n", true, "---\nexperiment: " + gateTestOn + "\n---\nbody\n"},
		{"file gate off", "---\nexperiment: " + gateTestOff + "\n---\nbody\n", false, ""},
		{"file gate on, block off", "---\nexperiment: " + gateTestOn + "\n---\n" + block(gateTestOff), true, "---\nexperiment: " + gateTestOn + "\n---\nintro\noutro\n"},
		{"CRLF block on", "a\r\n<!-- scion:experiment " + gateTestOn + " -->\r\nb\r\n<!-- scion:end-experiment -->\r\nc\r\n", true, "a\r\nb\r\nc\r\n"},
		{"CRLF block off", "a\r\n<!-- scion:experiment " + gateTestOff + " -->\r\nb\r\n<!-- scion:end-experiment -->\r\nc\r\n", true, "a\r\nc\r\n"},
		{"CRLF file gate off", "---\r\nexperiment: " + gateTestOff + "\r\n---\r\nbody\r\n", false, ""},
		{"no trailing newline", "a\n<!-- scion:experiment " + gateTestOn + " -->\nb\n<!-- scion:end-experiment -->", true, "a\nb\n"},
		{"unclosed block drops to end of file", "a\n<!-- scion:experiment " + gateTestOn + " -->\nb\nc\n", true, "a\nb\nc\n"},
		{"unclosed block off drops to end of file", "a\n<!-- scion:experiment " + gateTestOff + " -->\nb\nc\n", true, "a\n"},
		{"nested block drops the rest", "a\n<!-- scion:experiment " + gateTestOn + " -->\nb\n<!-- scion:experiment " + gateTestOn + " -->\nc\n<!-- scion:end-experiment -->\nd\n", true, "a\nb\n"},
		{"stray end marker is removed", "a\n<!-- scion:end-experiment -->\nb\n", true, "a\nb\n"},
		{"marker text inside a line is content", "see `<!-- scion:experiment x -->` here\n", true, "see `<!-- scion:experiment x -->` here\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, keep := gateMarkdown("f.md", []byte(tt.in), onlyOn(gateTestOn))
			if keep != tt.wantKeep {
				t.Fatalf("keep = %v, want %v", keep, tt.wantKeep)
			}
			if keep && string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGateMarkdown_WarnsOnMalformedBlocks(t *testing.T) {
	logs := captureWarnings(t)
	gateMarkdown("unclosed.md", []byte("<!-- scion:experiment "+gateTestOn+" -->\nx\n"), onlyOn(gateTestOn))
	gateMarkdown("stray.md", []byte("<!-- scion:end-experiment -->\n"), onlyOn())
	out := logs()
	for _, want := range []string{"unclosed experiment block", "unclosed.md", "without an opener", "stray.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("warnings do not contain %q:\n%s", want, out)
		}
	}
}

func TestGateMarkdown_UngatedIsSameBytes(t *testing.T) {
	in := []byte("---\nname: x\n---\n# no gates\r\nmixed endings\n")
	got, keep := gateMarkdown("x.md", in, onlyOn())
	if !keep || &got[0] != &in[0] {
		t.Errorf("ungated content should be returned as is, got keep=%v %q", keep, got)
	}
}

func TestSkillExperimentGate_UnknownIsOffWithWarning(t *testing.T) {
	logs := captureWarnings(t)
	gate := skillExperimentGate(func(string) bool { return true })
	if gate("web.no_such_experiment") {
		t.Error("an unknown experiment must be off")
	}
	if !strings.Contains(logs(), "web.no_such_experiment") {
		t.Errorf("no warning names the unknown experiment:\n%s", logs())
	}
	if !gate(gateTestOn) {
		t.Error("a registered experiment that isOn reports on must be on")
	}
	if skillExperimentGate(nil)(gateTestOn) {
		t.Error("a nil state source must turn every gate off")
	}
}

func gateFixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"plain/SKILL.md":                {Data: []byte("---\nname: plain\n---\n# Plain\n")},
		"plain/references/notes.md":     {Data: []byte("notes\n")},
		"plain/script.sh":               {Data: []byte("#!/bin/sh\n<!-- scion:experiment " + gateTestOff + " -->\n"), Mode: 0o755},
		"mixed/SKILL.md":                {Data: []byte("---\nname: mixed\n---\nA\n<!-- scion:experiment " + gateTestOff + " -->\nB\n<!-- scion:end-experiment -->\n<!-- scion:experiment " + gateTestOn + " -->\nC\n<!-- scion:end-experiment -->\n")},
		"mixed/references/on.md":        {Data: []byte("---\nexperiment: " + gateTestOn + "\n---\non\n")},
		"mixed/references/off.md":       {Data: []byte("---\nexperiment: " + gateTestOff + "\n---\noff\n")},
		"whole-off/SKILL.md":            {Data: []byte("---\nname: whole-off\nexperiment: " + gateTestOff + "\n---\n# Off\n")},
		"whole-off/references/extra.md": {Data: []byte("extra\n")},
	}
}

func TestGatedView(t *testing.T) {
	base := gateFixtureFS()
	view := gatedView(base, onlyOn(gateTestOn))

	t.Run("ungated files read byte-identical", func(t *testing.T) {
		for _, name := range []string{"plain/SKILL.md", "plain/references/notes.md", "plain/script.sh", "mixed/references/on.md"} {
			got, err := fs.ReadFile(view, name)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !bytes.Equal(got, base[name].Data) {
				t.Errorf("%s = %q, want %q", name, got, base[name].Data)
			}
		}
	})

	t.Run("blocks resolved", func(t *testing.T) {
		got, err := fs.ReadFile(view, "mixed/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		if want := "---\nname: mixed\n---\nA\nC\n"; string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
		fi, err := fs.Stat(view, "mixed/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != int64(len(got)) {
			t.Errorf("Stat size = %d, want %d", fi.Size(), len(got))
		}
	})

	t.Run("gated-off files are absent", func(t *testing.T) {
		for _, name := range []string{"mixed/references/off.md", "whole-off/SKILL.md"} {
			if _, err := fs.Stat(view, name); !errorsIsNotExist(err) {
				t.Errorf("%s: Stat err = %v, want not exist", name, err)
			}
		}
		des, err := fs.ReadDir(view, "mixed/references")
		if err != nil {
			t.Fatal(err)
		}
		if len(des) != 1 || des[0].Name() != "on.md" {
			t.Errorf("mixed/references lists %v, want only on.md", entryNames(des))
		}
	})

	t.Run("ReadDir sizes match content", func(t *testing.T) {
		des, err := fs.ReadDir(view, "mixed")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range des {
			if d.Name() != "SKILL.md" {
				continue
			}
			fi, err := d.Info()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := fs.ReadFile(view, "mixed/SKILL.md")
			if fi.Size() != int64(len(data)) {
				t.Errorf("ReadDir size = %d, want %d", fi.Size(), len(data))
			}
		}
	})

	t.Run("walk agrees with fstest", func(t *testing.T) {
		if err := fstest.TestFS(view, "plain/SKILL.md", "plain/script.sh", "mixed/SKILL.md", "mixed/references/on.md", "whole-off/references/extra.md"); err != nil {
			t.Error(err)
		}
	})
}

func errorsIsNotExist(err error) bool { return err != nil && os.IsNotExist(err) }

func entryNames(des []fs.DirEntry) []string {
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

func TestExperimentOn(t *testing.T) {
	t.Run("broker mode reads the dispatch experiments only", func(t *testing.T) {
		ctx := api.ContextWithBrokerMode(context.Background())
		ctx = api.ContextWithHubAgentDefaults(ctx, &api.HubAgentDefaults{Experiments: []string{gateTestOn}})
		settings := &config.VersionedSettings{LocalExperiments: []string{gateTestOff}}
		on := experimentOn(ctx, settings)
		if !on(gateTestOn) || on(gateTestOff) {
			t.Errorf("broker mode: on(%s)=%v on(%s)=%v, want true/false", gateTestOn, on(gateTestOn), gateTestOff, on(gateTestOff))
		}
	})
	t.Run("broker mode without hub defaults is all off", func(t *testing.T) {
		on := experimentOn(api.ContextWithBrokerMode(context.Background()), nil)
		if on(gateTestOn) {
			t.Error("no HubAgentDefaults must turn every experiment off")
		}
	})
	t.Run("local mode reads local_experiments", func(t *testing.T) {
		on := experimentOn(context.Background(), &config.VersionedSettings{LocalExperiments: []string{gateTestOff}})
		if !on(gateTestOff) || on(gateTestOn) {
			t.Error("local mode must follow local_experiments")
		}
		if experimentOn(context.Background(), nil)(gateTestOn) {
			t.Error("nil settings must be all off")
		}
	})
}

func TestInjectPlatformSkills_ExperimentGates(t *testing.T) {
	const skillsDir = ".claude/skills"
	read := func(t *testing.T, home, rel string) (string, bool) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(skillsDir), filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(data), true
	}

	t.Run("gated-on whole skill is installed, files and blocks resolved", func(t *testing.T) {
		home := t.TempDir()
		fixture := gateFixtureFS()
		fixture["whole-on/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: whole-on\nexperiment: " + gateTestOn + "\n---\n# On\n")}
		if err := injectPlatformSkills(fixture, home, skillsDir, workspaceSkillsInjectionContext{ExperimentOn: onlyOn(gateTestOn)}); err != nil {
			t.Fatal(err)
		}
		if _, ok := read(t, home, "whole-on/SKILL.md"); !ok {
			t.Error("gated-on whole skill was not installed")
		}
		if _, ok := read(t, home, "whole-off"); ok {
			t.Error("gated-off whole skill was installed")
		}
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(skillsDir), "whole-off")); !os.IsNotExist(err) {
			t.Errorf("gated-off whole skill directory exists: %v", err)
		}
		if got, _ := read(t, home, "mixed/SKILL.md"); got != "---\nname: mixed\n---\nA\nC\n" {
			t.Errorf("mixed/SKILL.md = %q", got)
		}
		if _, ok := read(t, home, "mixed/references/on.md"); !ok {
			t.Error("gated-on file missing")
		}
		if _, ok := read(t, home, "mixed/references/off.md"); ok {
			t.Error("gated-off file installed")
		}
		for _, name := range []string{"plain/SKILL.md", "plain/references/notes.md", "plain/script.sh"} {
			if got, _ := read(t, home, name); got != string(fixture[name].Data) {
				t.Errorf("ungated %s not byte-identical: %q", name, got)
			}
		}
	})

	t.Run("nil state source turns every gate off", func(t *testing.T) {
		home := t.TempDir()
		if err := injectPlatformSkills(gateFixtureFS(), home, skillsDir, workspaceSkillsInjectionContext{}); err != nil {
			t.Fatal(err)
		}
		if got, _ := read(t, home, "mixed/SKILL.md"); got != "---\nname: mixed\n---\nA\n" {
			t.Errorf("mixed/SKILL.md = %q", got)
		}
		if _, ok := read(t, home, "mixed/references/on.md"); ok {
			t.Error("gated file installed with no state source")
		}
	})

	t.Run("reincarnate after off removes a platform-owned copy", func(t *testing.T) {
		home := t.TempDir()
		fixture := gateFixtureFS()
		on := workspaceSkillsInjectionContext{ExperimentOn: onlyOn(gateTestOn, gateTestOff)}
		if err := injectPlatformSkills(fixture, home, skillsDir, on); err != nil {
			t.Fatal(err)
		}
		if _, ok := read(t, home, "whole-off/references/extra.md"); !ok {
			t.Fatal("setup: whole skill not installed while on")
		}
		// Restart/resume (no ForceOverwrite) keeps what was provisioned.
		if err := injectPlatformSkills(fixture, home, skillsDir, workspaceSkillsInjectionContext{ExperimentOn: onlyOn(gateTestOn)}); err != nil {
			t.Fatal(err)
		}
		if _, ok := read(t, home, "whole-off/SKILL.md"); !ok {
			t.Error("a plain re-run removed the skill; only reincarnation should")
		}
		off := workspaceSkillsInjectionContext{ExperimentOn: onlyOn(gateTestOn), ForceOverwrite: true}
		if err := injectPlatformSkills(fixture, home, skillsDir, off); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(home, filepath.FromSlash(skillsDir), "whole-off")); !os.IsNotExist(err) {
			t.Errorf("gated-off platform skill survived reincarnation: %v", err)
		}
		if got, _ := read(t, home, "mixed/SKILL.md"); got != "---\nname: mixed\n---\nA\nC\n" {
			t.Errorf("reincarnation did not refresh the gated blocks: %q", got)
		}
	})

	t.Run("reincarnate after off keeps a template-owned copy", func(t *testing.T) {
		home := t.TempDir()
		tplDir := filepath.Join(home, filepath.FromSlash(skillsDir), "whole-off")
		if err := os.MkdirAll(tplDir, 0o755); err != nil {
			t.Fatal(err)
		}
		const tplContent = "template version"
		if err := os.WriteFile(filepath.Join(tplDir, "SKILL.md"), []byte(tplContent), 0o644); err != nil {
			t.Fatal(err)
		}
		ctx := workspaceSkillsInjectionContext{
			ForceOverwrite:     true,
			TemplateSkillNames: map[string]bool{"whole-off": true},
			ExperimentOn:       onlyOn(gateTestOn),
		}
		if err := injectPlatformSkills(gateFixtureFS(), home, skillsDir, ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := read(t, home, "whole-off/SKILL.md"); got != tplContent {
			t.Errorf("template-owned skill changed: %q", got)
		}
	})
}

// TestInjectPlatformSkills_EmbeddedUngatedSkillsByteIdentical installs the
// shipped platform skills with every experiment off and on, and checks that
// every file without a gate is byte-identical to its embedded source
// (ptone/scion#7830 acceptance criterion 3).
func TestInjectPlatformSkills_EmbeddedUngatedSkillsByteIdentical(t *testing.T) {
	src := resources.PlatformSkillsFS()
	for _, state := range []struct {
		name string
		on   func(string) bool
	}{
		{"all off", func(string) bool { return false }},
		{"all on", func(string) bool { return true }},
	} {
		t.Run(state.name, func(t *testing.T) {
			home := t.TempDir()
			const skillsDir = ".agents/skills"
			ctx := workspaceSkillsInjectionContext{IsGit: true, HubEnabled: true, ExperimentOn: state.on}
			if err := injectPlatformSkills(src, home, skillsDir, ctx); err != nil {
				t.Fatal(err)
			}
			err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				raw, err := fs.ReadFile(src, p)
				if err != nil {
					return err
				}
				if strings.HasSuffix(p, ".md") && (bytes.Contains(raw, []byte("scion:experiment")) || parseSkillFrontmatter(raw).Experiment != "") {
					return nil
				}
				got, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(skillsDir), filepath.FromSlash(p)))
				if err != nil {
					t.Errorf("%s: %v", p, err)
					return nil
				}
				if !bytes.Equal(got, raw) {
					t.Errorf("%s is not byte-identical to its embedded source", p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
