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

package resources

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"gopkg.in/yaml.v3"
)

// Experiment gates in platform skills (ptone/scion#7830): `experiment: <id>`
// in a Markdown file's frontmatter, and blocks between
// `<!-- scion:experiment <id> -->` and `<!-- scion:end-experiment -->`.
// pkg/agent/skill_gate.go evaluates them at install and fails closed on
// anything malformed; these tests keep malformed gates out of the binary.

var (
	gateOpenRe       = regexp.MustCompile(`^<!--\s*scion:experiment\s+(\S+)\s*-->$`)
	gateEndRe        = regexp.MustCompile(`^<!--\s*scion:end-experiment\s*-->$`)
	frontExperiment  = regexp.MustCompile(`^experiment:\s*(.*)$`)
	gateMarkerTextRe = regexp.MustCompile(`scion:(end-)?experiment`)
)

// gatedLine is one body line of a platform skill Markdown file with the
// experiment that gates it ("" when ungated). Marker lines are not listed.
type gatedLine struct {
	n    int
	text string
	gate string
}

// skillMarkdown is a parsed platform skill Markdown file.
type skillMarkdown struct {
	name     string
	fileGate string
	lines    []gatedLine
	problems []string
}

func parseSkillMarkdown(name, raw string) skillMarkdown {
	sm := skillMarkdown{name: name}
	bad := func(n int, format string, args ...any) {
		sm.problems = append(sm.problems, fmt.Sprintf("%s:%d: ", name, n)+fmt.Sprintf(format, args...))
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	start := 0
	if len(lines) > 0 && lines[0] == "---" {
		for i := 1; i < len(lines); i++ {
			if lines[i] == "---" {
				start = i + 1
				break
			}
			if gateMarkerTextRe.MatchString(lines[i]) {
				bad(i+1, "experiment block marker inside YAML frontmatter")
			}
			if m := frontExperiment.FindStringSubmatch(lines[i]); m != nil {
				sm.fileGate = strings.Trim(strings.TrimSpace(m[1]), `"'`)
				if sm.fileGate == "" {
					bad(i+1, "empty experiment: in frontmatter")
				}
			}
		}
	}
	if start > 0 {
		// The runtime reads experiment: with a YAML parser
		// (pkg/agent parseSkillFrontmatter), which ignores the whole
		// frontmatter on a parse error: the gate would silently vanish.
		var fm struct {
			Experiment string `yaml:"experiment"`
		}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:start-1], "\n")), &fm); err != nil {
			bad(1, "frontmatter is not valid YAML (the runtime would ignore its experiment: gate): %v", err)
		} else if fm.Experiment != sm.fileGate {
			bad(1, "frontmatter experiment parses as %q in YAML but reads as %q line by line", fm.Experiment, sm.fileGate)
		}
	}
	open, openAt := "", 0
	for i := start; i < len(lines); i++ {
		n, line := i+1, lines[i]
		trimmed := strings.TrimSpace(line)
		if m := gateOpenRe.FindStringSubmatch(trimmed); m != nil {
			if open != "" {
				bad(n, "nested experiment block (block %q opened at line %d)", open, openAt)
			}
			open, openAt = m[1], n
			continue
		}
		if gateEndRe.MatchString(trimmed) {
			if open == "" {
				bad(n, "end-experiment marker without an opener")
			}
			open = ""
			continue
		}
		if gateMarkerTextRe.MatchString(line) {
			bad(n, "experiment marker text that is not a marker on its own line: %q", line)
		}
		gate := open
		if gate == "" {
			gate = sm.fileGate
		}
		sm.lines = append(sm.lines, gatedLine{n: n, text: line, gate: gate})
	}
	if open != "" {
		bad(openAt, "unclosed experiment block %q", open)
	}
	return sm
}

// platformSkillMarkdown parses every Markdown file of the embedded
// platform skills.
func platformSkillMarkdown(t *testing.T) []skillMarkdown {
	t.Helper()
	var out []skillMarkdown
	err := fs.WalkDir(PlatformSkillsFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return err
		}
		data, err := fs.ReadFile(PlatformSkillsFS(), p)
		if err != nil {
			return err
		}
		out = append(out, parseSkillMarkdown(p, string(data)))
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded platform skills: %v", err)
	}
	return out
}

// TestPlatformSkillGates_WellFormed: every embedded gate is well formed and
// names an active, registered experiment (acceptance criterion 5).
func TestPlatformSkillGates_WellFormed(t *testing.T) {
	reg := experiments.Default()
	for _, sm := range platformSkillMarkdown(t) {
		for _, p := range sm.problems {
			t.Error(p)
		}
		ids := map[string]bool{}
		if sm.fileGate != "" {
			ids[sm.fileGate] = true
		}
		for _, l := range sm.lines {
			if l.gate != "" {
				ids[l.gate] = true
			}
		}
		for id := range ids {
			if reg.IsRetired(id) {
				t.Errorf("%s: gate names retired experiment %q", sm.name, id)
			} else if _, ok := reg.Lookup(id); !ok {
				t.Errorf("%s: gate names unknown experiment %q", sm.name, id)
			}
		}
	}
}

// TestParseSkillMarkdown_RejectsMalformedGates checks the checker itself.
func TestParseSkillMarkdown_RejectsMalformedGates(t *testing.T) {
	for name, raw := range map[string]string{
		"unclosed":    "a\n<!-- scion:experiment web.decisions -->\nb\n",
		"nested":      "<!-- scion:experiment web.decisions -->\n<!-- scion:experiment web.decisions -->\n<!-- scion:end-experiment -->\n",
		"stray end":   "a\n<!-- scion:end-experiment -->\n",
		"inline":      "text <!-- scion:experiment web.decisions --> more\n",
		"frontmatter": "---\nname: x\n<!-- scion:experiment web.decisions -->\n---\n",
		"empty id":    "---\nexperiment:\n---\nbody\n",
		"bad yaml":    "---\nexperiment: web.decisions\ndescription: a: b\n---\nbody\n",
	} {
		if got := parseSkillMarkdown(name, raw); len(got.problems) == 0 {
			t.Errorf("%s: no problem reported for %q", name, raw)
		}
	}
	ok := parseSkillMarkdown("ok", "---\nexperiment: web.decisions\n---\na\r\n<!-- scion:experiment hub.artifacts -->\r\nb\r\n<!-- scion:end-experiment -->\r\n")
	if len(ok.problems) != 0 {
		t.Errorf("well-formed file reported problems: %v", ok.problems)
	}
	if ok.fileGate != experiments.Decisions || len(ok.lines) != 3 || ok.lines[1].gate != experiments.Artifacts {
		t.Errorf("parsed %+v", ok)
	}
}

// TestPlatformSkillGates_ChoicesClaimsAreGated: Choices guidance appears
// only inside web.decisions gates, so an agent on a hub without Decisions
// is never told to use it (design 3.6, as scion-web-serving does for the
// page view).
func TestPlatformSkillGates_ChoicesClaimsAreGated(t *testing.T) {
	claims := []string{"--choices", "scion decisions", "choice prompt", "choice-response", "references/choices.md"}
	found := 0
	for _, sm := range platformSkillMarkdown(t) {
		for _, l := range sm.lines {
			lower := strings.ToLower(l.text)
			for _, c := range claims {
				if !strings.Contains(lower, c) {
					continue
				}
				found++
				if l.gate != experiments.Decisions {
					t.Errorf("%s:%d mentions %q outside a %s gate: %q", sm.name, l.n, c, experiments.Decisions, l.text)
				}
			}
		}
	}
	if found == 0 {
		t.Error("no embedded platform skill mentions Choices; the scion-messaging content is missing")
	}
}

// TestPlatformSkillsFS_ScionMessagingChoices pins the Choices content of
// scion-messaging (ptone/scion#7830): the reference is file-gated, the two
// SKILL.md pointers are gated blocks, and the reference keeps the guidance
// agents need, including the full-set --update rule (ptone/scion#7841).
func TestPlatformSkillsFS_ScionMessagingChoices(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(PlatformSkillsFS(), name)
		if err != nil {
			t.Fatalf("reading embedded %s: %v", name, err)
		}
		return string(data)
	}
	ref := parseSkillMarkdown("references/choices.md", read("scion-messaging/references/choices.md"))
	if ref.fileGate != experiments.Decisions {
		t.Errorf("references/choices.md frontmatter gate = %q, want %q", ref.fileGate, experiments.Decisions)
	}
	skill := parseSkillMarkdown("SKILL.md", read("scion-messaging/SKILL.md"))
	if skill.fileGate != "" {
		t.Errorf("scion-messaging/SKILL.md must not be gated as a whole; got experiment %q", skill.fileGate)
	}
	blocks := 0
	prev := ""
	for _, l := range skill.lines {
		if l.gate == experiments.Decisions && prev != experiments.Decisions {
			blocks++
		}
		prev = l.gate
	}
	if blocks != 2 {
		t.Errorf("scion-messaging/SKILL.md has %d %s blocks, want 2 (decision line, flags bullet)", blocks, experiments.Decisions)
	}

	var body strings.Builder
	for _, l := range ref.lines {
		body.WriteString(l.text + " ")
	}
	content := strings.Join(strings.Fields(body.String()), " ")
	for _, want := range []string{
		"--choices-file <file>",
		"```json",
		"`--update`",
		"resend every still-pending key of that set",
		"the set is split",
		"there is no partial submit",
		"[choice-response]",
		"scion decisions list",
		"scion decisions show <key>",
		"scion decisions record <key> <choice-id> --from <message-id>",
		"At most 50 pending decisions per agent",
		"`default`",
		"send the question as ordinary Markdown",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("embedded scion-messaging/references/choices.md does not contain %q", want)
		}
	}
	// ptone/scion#7842 changes how a recommended choice is shown; the
	// reference must not describe that rendering.
	for _, unwanted := range []string{"preselect", "pre-select", "start selected", "outlined"} {
		if strings.Contains(strings.ToLower(content), unwanted) {
			t.Errorf("references/choices.md describes recommended-choice rendering (%q); keep it neutral", unwanted)
		}
	}
}

// TestPlatformSkillsFS_ScionArtifacts pins the scion-artifacts platform skill
// (ptone/scion#7897): the whole skill is gated on hub.artifacts through its
// SKILL.md frontmatter, it covers the commands agents use and what they
// cannot do, and scion-messaging points to it from one hub.artifacts block.
func TestPlatformSkillsFS_ScionArtifacts(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(PlatformSkillsFS(), name)
		if err != nil {
			t.Fatalf("reading embedded %s: %v", name, err)
		}
		return string(data)
	}
	raw := read("scion-artifacts/SKILL.md")
	skill := parseSkillMarkdown("scion-artifacts/SKILL.md", raw)
	if skill.fileGate != experiments.Artifacts {
		t.Errorf("scion-artifacts/SKILL.md frontmatter gate = %q, want %q", skill.fileGate, experiments.Artifacts)
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		InjectWhen  string `yaml:"inject_when"`
	}
	front, _, _ := strings.Cut(strings.TrimPrefix(strings.ReplaceAll(raw, "\r\n", "\n"), "---\n"), "\n---\n")
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		t.Fatalf("scion-artifacts/SKILL.md frontmatter: %v", err)
	}
	if fm.Name != "scion-artifacts" || fm.Description == "" {
		t.Errorf("frontmatter name = %q, description = %q; want name scion-artifacts and a description", fm.Name, fm.Description)
	}
	if fm.InjectWhen != "" {
		t.Errorf("scion-artifacts is gated by its experiment only; frontmatter sets inject_when: %q", fm.InjectWhen)
	}
	entries, err := fs.ReadDir(PlatformSkillsFS(), "scion-artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("scion-artifacts holds %d entries, want only SKILL.md (other files would need their own gate)", len(entries))
	}

	content := strings.Join(strings.Fields(raw), " ")
	for _, want := range []string{
		"scion artifact publish design.md --title",
		"--key",
		"--note",
		"--version-of scion://artifact/<id>",
		"--entry index.html",
		"scion artifact publish plan.md --review scion://artifact/<id>",
		"{~~old~>new~~}",
		"{>>text<<}",
		"`unmarked_changes`",
		"`stale_review`",
		"scion artifact get scion://artifact/<id>",
		"--out",
		"--clean",
		"--accept",
		"--kind publish",
		"scion artifact versions scion://artifact/<id>",
		"scion artifact comments scion://artifact/<id>",
		"scion artifact list",
		"scion artifact view scion://artifact/<id> --check",
		"--show-link",
		"scion message @reviewer",
		"--artifact scion://artifact/<id>",
		"`scion artifact share`, `scion artifact grants` and `scion artifact delete` do not exist",
		`unknown command "share" for "scion artifact"`,
		"If its `Scopes:` line lacks `project:artifact:read`, that is the cause",
		"If its `Experiments:` line lacks `hub.artifacts`",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("embedded scion-artifacts/SKILL.md does not contain %q", want)
		}
	}

	msg := parseSkillMarkdown("scion-messaging/SKILL.md", read("scion-messaging/SKILL.md"))
	blocks, pointer := 0, false
	prev := ""
	for _, l := range msg.lines {
		if l.gate == experiments.Artifacts && prev != experiments.Artifacts {
			blocks++
		}
		prev = l.gate
		if strings.Contains(l.text, "scion-artifacts") {
			pointer = true
			if l.gate != experiments.Artifacts {
				t.Errorf("scion-messaging/SKILL.md:%d names the scion-artifacts skill outside a %s gate: %q", l.n, experiments.Artifacts, l.text)
			}
		}
	}
	if blocks != 1 || !pointer {
		t.Errorf("scion-messaging/SKILL.md has %d %s blocks (pointer found: %v), want 1 pointing to the scion-artifacts skill", blocks, experiments.Artifacts, pointer)
	}
}
