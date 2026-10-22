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
	"io/fs"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

func TestBuiltinTemplates(t *testing.T) {
	templates := BuiltinTemplates()
	if len(templates) < 1 {
		t.Fatalf("expected at least 1 template, got %d", len(templates))
	}

	found := false
	for _, tmpl := range templates {
		if tmpl.Name == "default" {
			found = true
			if tmpl.Kind != storage.ResourceKindTemplate {
				t.Errorf("default template kind = %q, want %q", tmpl.Kind, storage.ResourceKindTemplate)
			}
			if tmpl.Scope != "global" {
				t.Errorf("default template scope = %q, want %q", tmpl.Scope, "global")
			}
			if tmpl.ScopeID != "" {
				t.Errorf("default template scopeID = %q, want empty", tmpl.ScopeID)
			}
			assertSourceURL(t, tmpl)
			assertFileReadable(t, tmpl, "scion-agent.yaml")
		}
	}
	if !found {
		t.Error("default template not found in BuiltinTemplates()")
	}
}

func TestBuiltinHarnessConfigs(t *testing.T) {
	configs := BuiltinHarnessConfigs()

	if len(configs) == 0 {
		t.Fatal("expected at least 1 harness-config, got 0")
	}

	seen := make(map[string]bool)
	for _, cfg := range configs {
		if cfg.Kind != storage.ResourceKindHarnessConfig {
			t.Errorf("harness-config %q kind = %q, want %q", cfg.Name, cfg.Kind, storage.ResourceKindHarnessConfig)
		}
		if cfg.Scope != "global" {
			t.Errorf("harness-config %q scope = %q, want %q", cfg.Name, cfg.Scope, "global")
		}
		if cfg.ScopeID != "" {
			t.Errorf("harness-config %q scopeID = %q, want empty", cfg.Name, cfg.ScopeID)
		}
		assertSourceURL(t, cfg)
		if seen[cfg.Name] {
			t.Errorf("duplicate harness-config %q", cfg.Name)
		}
		seen[cfg.Name] = true
	}

	if !seen["claude"] {
		t.Error("expected harness-config \"claude\" not found")
	}
}

func TestBuiltinHarnessConfigsExcludesGemini(t *testing.T) {
	for _, cfg := range BuiltinHarnessConfigs() {
		if cfg.Name == "gemini" {
			t.Error("gemini should not be in BuiltinHarnessConfigs()")
		}
	}
}

func TestBuiltinHarnessConfigsReadConfigYAML(t *testing.T) {
	for _, cfg := range BuiltinHarnessConfigs() {
		assertFileReadable(t, cfg, "config.yaml")
	}
}

func TestBuiltinResources(t *testing.T) {
	all := BuiltinResources()
	templates := BuiltinTemplates()
	configs := BuiltinHarnessConfigs()

	want := len(templates) + len(configs)
	if len(all) != want {
		t.Errorf("BuiltinResources() returned %d entries, want %d (templates=%d + configs=%d)",
			len(all), want, len(templates), len(configs))
	}
}

// TestPlatformSkillsFS_ScionAgentManageLifecycleReferencesReincarnate is the
// design §3.9 / Amendment A25's "2b" bullet skill-embedding test: "the
// one-line pointer in the scion-agent-manage lifecycle reference." It
// exercises the actual embedded path (PlatformSkillsFS, the same accessor
// pkg/agent/provision.go and pkg/hub/platform_skills_seed.go use to inject
// this skill into agents), so it fails if the pointer only exists in the
// source tree but was never wired into go:embed, and it fails if the
// pointer stops naming the right command.
func TestPlatformSkillsFS_ScionAgentManageLifecycleReferencesReincarnate(t *testing.T) {
	data, err := fs.ReadFile(PlatformSkillsFS(), "scion-agent-manage/references/agent-lifecycle.md")
	if err != nil {
		t.Fatalf("reading embedded scion-agent-manage lifecycle reference: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "scion reincarnate") {
		t.Errorf("embedded scion-agent-manage/references/agent-lifecycle.md does not mention `scion reincarnate`; " +
			"coordinators need a pointer to it as an alternative to delete-and-recreate")
	}
}

// TestPlatformSkillsFS_ScionAgentManageLifecycleDescribesMove pins the
// embedded skill's move section (ptone/scion#2727 phase 3): agents learn
// the --broker command, its same-NFS-export requirement, the self-move
// limit and that the CLI does not set their status.
func TestPlatformSkillsFS_ScionAgentManageLifecycleDescribesMove(t *testing.T) {
	data, err := fs.ReadFile(PlatformSkillsFS(), "scion-agent-manage/references/agent-lifecycle.md")
	if err != nil {
		t.Fatalf("reading embedded scion-agent-manage lifecycle reference: %v", err)
	}
	// Compare with whitespace collapsed, so a reflow does not break the pins.
	content := strings.Join(strings.Fields(string(data)), " ")
	for _, want := range []string{
		"### Moving to another Runtime Broker",
		"scion reincarnate <agent> --broker <name|id>",
		"**same NFS export**",
		"already serves your project",
		"The CLI does not set your status",
		"An agent never links a new broker",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("embedded scion-agent-manage/references/agent-lifecycle.md does not contain %q", want)
		}
	}
}

// TestPlatformSkillsFS_ScionPortServing pins the scion-port-serving platform
// skill (ptone/scion#5567). It reads the embedded copies, the ones agents
// get, so it fails if the skill or its web-apps reference is missing from
// go:embed, gated to a subset of agents, or loses its guidance: scion expose
// and the URL as printed, what the proxy carries, scion tunnel for full apps,
// and for pages one self-contained file and an honest browser check.
func TestPlatformSkillsFS_ScionPortServing(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(PlatformSkillsFS(), name)
		if err != nil {
			t.Fatalf("reading embedded %s: %v", name, err)
		}
		return strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	skillRaw := read("scion-port-serving/SKILL.md")
	webRaw := read("scion-port-serving/references/web-apps.md")

	front, _, ok := strings.Cut(strings.TrimPrefix(skillRaw, "---\n"), "\n---\n")
	if !strings.HasPrefix(skillRaw, "---\n") || !ok {
		t.Fatalf("embedded scion-port-serving/SKILL.md has no frontmatter block")
	}
	if !strings.Contains(front, "\nname: scion-port-serving\n") && !strings.HasPrefix(front, "name: scion-port-serving\n") {
		t.Errorf("frontmatter does not declare name: scion-port-serving:\n%s", front)
	}
	if !strings.Contains(front, "description:") {
		t.Errorf("frontmatter has no description")
	}
	if strings.Contains(front, "inject_when") {
		t.Errorf("scion-port-serving must reach every agent; frontmatter sets inject_when:\n%s", front)
	}
	if _, err := fs.Stat(PlatformSkillsFS(), "scion-web-serving"); err == nil {
		t.Errorf("the old scion-web-serving skill directory is still embedded")
	}

	// Compare with whitespace collapsed, so a reflow does not break the pins.
	skill := strings.Join(strings.Fields(skillRaw), " ")
	web := strings.Join(strings.Fields(webRaw), " ")
	pins := []struct {
		file, content string
		want          []string
	}{
		{"SKILL.md", skill, []string{
			"scion expose <port> --label",
			"scion expose --list",
			"scion unexpose <port>",
			"exactly as printed",
			"trailing `/`",
			"9810 and 18380 cannot be exposed",
			"at most 10 ports exposed",
			"may be exposed again on its next scan",
			"WebSockets and streamed responses",
			"a WebSocket request is refused",
			"Keep the port exposed with `scion expose <port>` (the tunnel only reaches exposed ports)",
			"scion tunnel <agent> --port <port>",
			"https://googlecloudplatform.github.io/scion/hosted/user/port-forwarding/",
			"references/web-apps.md",
		}},
		{"references/web-apps.md", web, []string{
			"one self-contained HTML file",
			"vite-plugin-singlefile",
			"data:",
			"scion expose <port>",
			"exactly as printed",
			"trailing `/`",
			"Keep the port exposed with `scion expose <port>` (the tunnel only reaches exposed ports)",
			"scion tunnel <agent> --port <port>",
			"https://googlecloudplatform.github.io/scion/hosted/user/port-forwarding/#multi-file-apps",
			"is NOT proof",
			"headless browser",
			"If you could not run a browser, say so",
			"Does not work: client-side routing by path",
			"`HashRouter`, `createWebHashHistory`",
			"Search your SOURCE (not the build) for `BrowserRouter`, `createBrowserRouter` and `createWebHistory`",
		}},
	}
	for _, p := range pins {
		for _, want := range p.want {
			if !strings.Contains(p.content, want) {
				t.Errorf("embedded scion-port-serving/%s does not contain %q", p.file, want)
			}
		}
	}

	// Pages served through the link cannot load separate files today, so
	// neither file may tell agents that relative URLs or a relative base
	// make a multi-file app work.
	for _, p := range pins {
		lower := strings.ToLower(p.content)
		for _, unwanted := range []string{
			"relative urls work",
			"only what the page can reach with relative urls",
			"base './'",
			"--base ./",
			`"homepage": "."`,
		} {
			if strings.Contains(lower, unwanted) {
				t.Errorf("embedded scion-port-serving/%s contains %q, guidance that does not hold for pages opened through the link today", p.file, unwanted)
			}
		}
	}
}

// TestMandatoryBoilerplate_PointsToPortServingSkill pins the one-line hint
// to the scion-port-serving skill in the boilerplate every agent receives
// (ptone/scion#5567).
func TestMandatoryBoilerplate_PointsToPortServingSkill(t *testing.T) {
	fsys := MandatoryBoilerplateFS()
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("reading mandatory boilerplate: %v", err)
	}
	var hits []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			if strings.Contains(line, "scion-port-serving") {
				hits = append(hits, e.Name()+": "+line)
			}
		}
	}
	if len(hits) != 1 {
		t.Fatalf("mandatory boilerplate has %d lines naming scion-port-serving, want exactly 1: %q", len(hits), hits)
	}
	if !strings.Contains(hits[0], "scion expose") {
		t.Errorf("scion-port-serving hint does not mention scion expose: %q", hits[0])
	}
}

func TestSourceURLFormat(t *testing.T) {
	const harnessPrefix = "https://github.com/GoogleCloudPlatform/scion/harnesses/"
	const builtinPrefix = "builtin://scion/"

	for _, r := range BuiltinResources() {
		if r.Kind == storage.ResourceKindHarnessConfig {
			if !strings.HasPrefix(r.SourceURL, harnessPrefix) {
				t.Errorf("%s %q SourceURL = %q, want prefix %q", r.Kind, r.Name, r.SourceURL, harnessPrefix)
				continue
			}
			name := strings.TrimPrefix(r.SourceURL, harnessPrefix)
			if name != r.Name {
				t.Errorf("%s %q SourceURL name = %q, want %q", r.Kind, r.Name, name, r.Name)
			}
		} else {
			if !strings.HasPrefix(r.SourceURL, builtinPrefix) {
				t.Errorf("%s %q SourceURL = %q, want prefix %q", r.Kind, r.Name, r.SourceURL, builtinPrefix)
				continue
			}
			parts := strings.Split(strings.TrimPrefix(r.SourceURL, builtinPrefix), "/")
			if len(parts) != 3 {
				t.Errorf("%s %q SourceURL = %q, want 3 path segments after prefix (version/kind/name)", r.Kind, r.Name, r.SourceURL)
				continue
			}
			if parts[0] == "" {
				t.Errorf("%s %q SourceURL has empty version segment", r.Kind, r.Name)
			}
			if parts[1] != string(r.Kind) {
				t.Errorf("%s %q SourceURL kind segment = %q, want %q", r.Kind, r.Name, parts[1], r.Kind)
			}
			if parts[2] != r.Name {
				t.Errorf("%s %q SourceURL name segment = %q, want %q", r.Kind, r.Name, parts[2], r.Name)
			}
		}
	}
}

func TestMandatoryBoilerplateFS(t *testing.T) {
	fsys := MandatoryBoilerplateFS()
	if fsys == nil {
		t.Fatal("MandatoryBoilerplateFS() returned nil")
	}
	data, err := fs.ReadFile(fsys, "agent-instructions-preamble.md")
	if err != nil {
		t.Fatalf("cannot read agent-instructions-preamble.md: %v", err)
	}
	if len(data) == 0 {
		t.Error("agent-instructions-preamble.md is empty")
	}
}

func assertSourceURL(t *testing.T, r BundledResource) {
	t.Helper()
	if r.Kind == storage.ResourceKindHarnessConfig {
		if !strings.HasPrefix(r.SourceURL, "https://github.com/GoogleCloudPlatform/scion/harnesses/") {
			t.Errorf("%s %q SourceURL = %q, want https://... prefix", r.Kind, r.Name, r.SourceURL)
		}
	} else if !strings.HasPrefix(r.SourceURL, "builtin://scion/") {
		t.Errorf("%s %q SourceURL = %q, want builtin://scion/... prefix", r.Kind, r.Name, r.SourceURL)
	}
}

func assertFileReadable(t *testing.T, r BundledResource, name string) {
	t.Helper()
	path := name
	if r.Root != "" && r.Root != "." {
		path = r.Root + "/" + name
	}
	data, err := fs.ReadFile(r.FS, path)
	if err != nil {
		t.Errorf("%s %q: cannot read %q: %v", r.Kind, r.Name, path, err)
		return
	}
	if len(data) == 0 {
		t.Errorf("%s %q: %q is empty", r.Kind, r.Name, path)
	}
}
