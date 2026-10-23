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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// rerenderFixture is an agent directory with a home, for the re-render
// tests.
type rerenderFixture struct {
	agentDir  string
	agentHome string
}

func newRerenderFixture(t *testing.T) rerenderFixture {
	t.Helper()
	agentDir := t.TempDir()
	agentHome := filepath.Join(agentDir, "home")
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	return rerenderFixture{agentDir: agentDir, agentHome: agentHome}
}

func (f rerenderFixture) inputs(h api.Harness, keys []string, cfg, inline *api.ScionConfig, chain []*config.Template) rerenderInputs {
	return rerenderInputs{
		Keys:       keys,
		AgentName:  "a",
		AgentDir:   f.agentDir,
		AgentHome:  f.agentHome,
		ProjectDir: f.agentDir,
		Harness:    h,
		Chain:      chain,
		Config:     cfg,
		Inline:     inline,
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeFileString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rerenderPromptConfig(prompt string) (*api.ScionConfig, *api.ScionConfig) {
	return &api.ScionConfig{SystemPrompt: prompt}, &api.ScionConfig{SystemPrompt: prompt, ChangedFields: []string{"system_prompt"}}
}

// TestRerender_SystemPrompt_OncePerChange pins the fingerprint record: with
// no record the changed prompt is rendered (an agent provisioned before the
// record, or the first start after provisioning); an unchanged prompt is
// not written again, so an edit made inside the agent survives; a changed
// prompt is rendered once; and removing the record (as provisioning, and
// so reincarnation, does) renders it again.
func TestRerender_SystemPrompt_OncePerChange(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	keys := []string{"system_prompt"}

	cfg, inline := rerenderPromptConfig("prompt v2")
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, keys, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "prompt v2" {
		t.Fatalf("first render with no record: system prompt = %q, want %q", got, "prompt v2")
	}
	if _, err := os.Stat(rerenderRecordPath(f.agentDir)); err != nil {
		t.Fatalf("rerender record not written: %v", err)
	}

	// Unchanged value: no write.
	writeFileString(t, promptFile, "edited in the agent")
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, keys, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "edited in the agent" {
		t.Fatalf("unchanged fingerprint rewrote the system prompt: %q", got)
	}

	// Changed value: rendered once.
	cfg, inline = rerenderPromptConfig("prompt v3")
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, keys, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "prompt v3" {
		t.Fatalf("changed value: system prompt = %q, want %q", got, "prompt v3")
	}
	writeFileString(t, promptFile, "edited again")
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, keys, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "edited again" {
		t.Fatalf("second start after the change rewrote the system prompt: %q", got)
	}

	// Provisioning (and so reincarnation) resets the record.
	if err := resetRerenderRecord(f.agentDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, keys, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "prompt v3" {
		t.Fatalf("after a record reset: system prompt = %q, want %q", got, "prompt v3")
	}
}

// TestRerender_AgentInstructions renders changed agent instructions with
// the mandatory preamble, as provisioning does, and leaves the system
// prompt file alone when no system prompt is set.
func TestRerender_AgentInstructions(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	cfg := &api.ScionConfig{AgentInstructions: "do the thing"}
	inline := &api.ScionConfig{AgentInstructions: "do the thing", ChangedFields: []string{"agent_instructions"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, []string{"agent_instructions"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	got := readFileString(t, filepath.Join(f.agentHome, "agents.md"))
	if !strings.HasSuffix(strings.TrimSpace(got), "do the thing") {
		t.Fatalf("agents.md does not end with the new instructions: %q", got)
	}
	preamble, err := loadMandatoryPreamble(resources.MandatoryBoilerplateFS())
	if err != nil {
		t.Fatal(err)
	}
	if len(preamble) > 0 && !strings.Contains(got, strings.TrimSpace(string(preamble))) {
		t.Fatalf("agents.md lacks the mandatory preamble: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.agentHome, ".scion", "system_prompt.md")); !os.IsNotExist(err) {
		t.Fatalf("system prompt file written with no system prompt set (err %v)", err)
	}
}

// TestRerender_ClearedSystemPromptFromTemplate: a system prompt the hub
// cleared (the start's cleared_fields copied the template's value back,
// here none, so the convention file applies) is rendered from the
// template's system-prompt.md.
func TestRerender_ClearedSystemPromptFromTemplate(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	tplDir := t.TempDir()
	writeFileString(t, filepath.Join(tplDir, "system-prompt.md"), "template prompt")
	chain := []*config.Template{{Name: "tpl", Path: tplDir}}
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	writeFileString(t, promptFile, "old agent prompt")

	cfg := &api.ScionConfig{}
	inline := &api.ScionConfig{ClearedFields: []string{"system_prompt"}}
	keys := api.RerenderConfigKeys(inline)
	if len(keys) != 1 || keys[0] != "system_prompt" {
		t.Fatalf("RerenderConfigKeys(cleared system_prompt) = %v", keys)
	}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, keys, cfg, inline, chain)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "template prompt" {
		t.Fatalf("cleared system prompt: got %q, want the template's", got)
	}
}

// TestRerender_TemplateFileWithoutChain: a prompt value that names a
// template file is not rendered as content when the template does not
// resolve; the start warns and leaves the files alone.
func TestRerender_TemplateFileWithoutChain(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	writeFileString(t, promptFile, "provisioned prompt")
	cfg := &api.ScionConfig{SystemPrompt: "system-prompt.md"}
	inline := &api.ScionConfig{ClearedFields: []string{"system_prompt"}}
	warns, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, []string{"system_prompt"}, cfg, inline, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "next reincarnation") {
		t.Fatalf("warnings = %v, want one naming the next reincarnation", warns)
	}
	if got := readFileString(t, promptFile); got != "provisioned prompt" {
		t.Fatalf("system prompt changed without a template: %q", got)
	}
	if _, err := os.Stat(rerenderRecordPath(f.agentDir)); !os.IsNotExist(err) {
		t.Fatalf("record written for a group that was not rendered (err %v)", err)
	}
}

// TestRerender_ContainerScriptPromptsGoToInputsRecord: for a
// container-script harness the prompts are staged into the control-plane
// inputs record Start restages from, a prompt no longer set is removed
// from it, and other recorded inputs are kept.
func TestRerender_ContainerScriptPromptsGoToInputsRecord(t *testing.T) {
	f := newRerenderFixture(t)
	h, err := harness.NewContainerScriptHarness("/fake/harness-config-dir", config.HarnessConfigEntry{
		Harness:     "claude",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(f.agentDir, controlPlaneInputsDirName)
	writeFileString(t, filepath.Join(record, "system-prompt.md"), "old prompt")
	writeFileString(t, filepath.Join(record, resolvedSkillsInputName), "{}")

	cfg := &api.ScionConfig{AgentInstructions: "new instructions"}
	inline := &api.ScionConfig{AgentInstructions: "new instructions", ChangedFields: []string{"agent_instructions"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, []string{"agent_instructions"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, filepath.Join(record, "instructions.md")); !strings.HasSuffix(strings.TrimSpace(got), "new instructions") {
		t.Fatalf("recorded instructions = %q", got)
	}
	if _, err := os.Stat(filepath.Join(record, "system-prompt.md")); !os.IsNotExist(err) {
		t.Fatalf("a system prompt no longer set stayed in the record (err %v)", err)
	}
	if got := readFileString(t, filepath.Join(record, resolvedSkillsInputName)); got != "{}" {
		t.Fatalf("other recorded input changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.agentHome, ".scion", "harness", "inputs", "instructions.md")); !os.IsNotExist(err) {
		t.Fatalf("prompts written into the staged inputs instead of the record (err %v)", err)
	}
}

// TestRerender_Services writes changed services, and removes the services
// file when the agent's config has none.
func TestRerender_Services(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	servicesFile := filepath.Join(f.agentHome, ".scion", servicesFileName)
	cfg := &api.ScionConfig{Services: []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}}}
	inline := &api.ScionConfig{Services: cfg.Services, ChangedFields: []string{"services"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, []string{"services"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, servicesFile); !strings.Contains(got, "postgres") {
		t.Fatalf("services file = %q", got)
	}
	// Only a config with no services (a fresh generation; a start's merge
	// never empties them, see TestStart_ClearsTheMergeKeepsAreNotRendered)
	// removes the file.
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, []string{"services"}, &api.ScionConfig{}, &api.ScionConfig{ChangedFields: []string{"services"}}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(servicesFile); !os.IsNotExist(err) {
		t.Fatalf("services file kept for a config with no services (err %v)", err)
	}
}

// countingSkillResolver resolves every reference to a one-file skill
// (references sharing a URI collapse, as in the Hub resolver) and counts
// its calls.
type countingSkillResolver struct{ calls int }

func (r *countingSkillResolver) Resolve(_ context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	r.calls++
	out := &ResolveResult{}
	seen := map[string]bool{}
	for _, ref := range refs {
		// Like the Hub resolver, references that share a URI collapse.
		if seen[ref.URI] {
			continue
		}
		seen[ref.URI] = true
		name := ref.URI[strings.LastIndex(ref.URI, "/")+1:]
		out.Resolved = append(out.Resolved, ResolvedSkill{
			Name:  name,
			URI:   ref.URI,
			Files: []ResolvedFile{{Path: "SKILL.md", Content: []byte("# " + name + "\n")}},
		})
	}
	return out, nil
}

// TestRerender_Skills installs changed skills through the provisioning
// step, once per change of the skill list.
func TestRerender_Skills(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	resolver := &countingSkillResolver{}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	skills := []api.SkillReference{{URI: "hub://skills/review"}}
	cfg := &api.ScionConfig{Skills: skills}
	inline := &api.ScionConfig{Skills: skills, ChangedFields: []string{"skills"}}

	if _, err := rerenderProvisionedFiles(ctx, f.inputs(h, []string{"skills"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(f.agentHome, h.SkillsDir(), "review", "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("skill not installed: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if _, err := rerenderProvisionedFiles(ctx, f.inputs(h, []string{"skills"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("unchanged skill list resolved again (calls = %d)", resolver.calls)
	}
	cfg.Skills = append(cfg.Skills, api.SkillReference{URI: "hub://skills/test"})
	if _, err := rerenderProvisionedFiles(ctx, f.inputs(h, []string{"skills"}, cfg, inline, nil)); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 2 {
		t.Fatalf("changed skill list: resolver calls = %d, want 2", resolver.calls)
	}
}

// TestRerender_NoKeys does nothing without rerenderable keys.
func TestRerender_NoKeys(t *testing.T) {
	f := newRerenderFixture(t)
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"model"}, &api.ScionConfig{SystemPrompt: "x"}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rerenderRecordPath(f.agentDir)); !os.IsNotExist(err) {
		t.Fatalf("record written with no rerenderable keys (err %v)", err)
	}
}

// TestStart_RerendersChangedProvisionedFiles drives the re-render through
// AgentManager.Start for a container-script harness: an agent provisioned
// with the template's system prompt, then started with a hub inline config
// that changed it (StartOptions.RerenderKeys, as the broker sets it from
// changed_fields), launches with the new prompt staged; a later start with
// the same value restages it from the record; and a start without
// RerenderKeys (an older hub, or no change) keeps what is recorded.
func TestStart_RerendersChangedProvisionedFiles(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	tplDir := e.template(t, "tplr", "harness_config: hc-scripted\n")
	writeFileString(t, filepath.Join(tplDir, "system-prompt.md"), "template prompt")
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "rr", ProjectPath: e.scion, Template: "tplr", NoAuth: true}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("first start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "rr")
	staged := filepath.Join(home, ".scion", "harness", "inputs", "system-prompt.md")
	if got := readFileString(t, staged); got != "template prompt" {
		t.Fatalf("provisioned system prompt = %q", got)
	}

	inline := &api.ScionConfig{SystemPrompt: "edited prompt", ChangedFields: []string{"system_prompt"}}
	mergeIntoPersistedConfig(t, config.ResolveAgentDir(e.scion, "rr"), inline)
	edited := api.StartOptions{Name: "rr", ProjectPath: e.scion, NoAuth: true, InlineConfig: inline, RerenderKeys: api.RerenderConfigKeys(inline)}
	if _, err := mgr.Start(context.Background(), edited); err != nil {
		t.Fatalf("start after the edit: %v", err)
	}
	if got := readFileString(t, staged); got != "edited prompt" {
		t.Fatalf("system prompt after the edit = %q, want the edited one", got)
	}
	agentDir := config.ResolveAgentDir(e.scion, "rr")
	if got := readFileString(t, filepath.Join(agentDir, controlPlaneInputsDirName, "system-prompt.md")); got != "edited prompt" {
		t.Fatalf("recorded system prompt = %q", got)
	}

	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "rr", ProjectPath: e.scion, NoAuth: true}); err != nil {
		t.Fatalf("plain start: %v", err)
	}
	if got := readFileString(t, staged); got != "edited prompt" {
		t.Fatalf("system prompt after a plain start = %q", got)
	}
}

// TestProvisionAgent_RemovesRerenderRecord: provisioning (and so a
// reincarnation's reprovision) resets the rerender record, so the next
// re-render never matches a fingerprint of the previous generation.
func TestProvisionAgent_RemovesRerenderRecord(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-decl", policyTestDecl)
	agentDir := config.ResolveAgentDir(e.scion, "prov-rr")
	writeFileString(t, rerenderRecordPath(agentDir), `{"prompts":"stale"}`)
	if _, _, _, err := ProvisionAgent(context.Background(), "prov-rr", "", "", "hc-decl", e.scion, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if rec := readRerenderRecord(agentDir); len(rec.Fingerprints) != 0 {
		t.Fatalf("fingerprints of the previous generation survived provisioning: %v", rec.Fingerprints)
	}
}

// TestStart_CreatedAgentFirstStartRerenders: an agent provisioned without
// a start (the created phase) whose system prompt and services were changed
// before its first start launches with the changed values.
func TestStart_CreatedAgentFirstStartRerenders(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	tplDir := e.template(t, "tplc", "harness_config: hc-scripted\n")
	writeFileString(t, filepath.Join(tplDir, "system-prompt.md"), "template prompt")
	mgr := policyTestManager(nil)
	if _, err := mgr.Provision(context.Background(), api.StartOptions{Name: "created", ProjectPath: e.scion, Template: "tplc", NoAuth: true}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	inline := &api.ScionConfig{
		SystemPrompt:  "edited before the first start",
		Services:      []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}},
		ChangedFields: []string{"services", "system_prompt"},
	}
	// The broker merges the hub's inline config into the persisted config
	// before the start (applyInlineConfigUpdate).
	mergeIntoPersistedConfig(t, config.ResolveAgentDir(e.scion, "created"), inline)
	opts := api.StartOptions{Name: "created", ProjectPath: e.scion, NoAuth: true, InlineConfig: inline, RerenderKeys: api.RerenderConfigKeys(inline)}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("first start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "created")
	if got := readFileString(t, filepath.Join(home, ".scion", "harness", "inputs", "system-prompt.md")); got != "edited before the first start" {
		t.Fatalf("system prompt at first start = %q", got)
	}
	if got := readFileString(t, filepath.Join(home, ".scion", servicesFileName)); !strings.Contains(got, "postgres") {
		t.Fatalf("services file at first start = %q", got)
	}
}

// mergeIntoPersistedConfig merges inline into the agent's scion-agent.json,
// as the broker does before a start.
func mergeIntoPersistedConfig(t *testing.T, agentDir string, inline *api.ScionConfig) {
	t.Helper()
	path := filepath.Join(agentDir, "scion-agent.json")
	var existing api.ScionConfig
	if err := json.Unmarshal([]byte(readFileString(t, path)), &existing); err != nil {
		t.Fatal(err)
	}
	merged := config.MergeScionConfig(&existing, inline)
	data, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	writeFileString(t, path, string(data))
}

// TestStart_ClearsTheMergeKeepsAreNotRendered pins, through the broker's
// real merge of the hub's inline config into the persisted config, what a
// start can and cannot apply for the re-rendered keys: a cleared
// agent_instructions or services value and a removed skill reach neither
// the persisted config nor the rendered files (the merge keeps an empty
// value and appends skill references, and a re-render uninstalls nothing).
// The hub reports exactly these edits as held for reincarnation.
func TestStart_ClearsTheMergeKeepsAreNotRendered(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-skills", policyTestScripted+"skills_dir: .agents/skills\n")
	e.template(t, "tplk", "harness_config: hc-skills\n")
	mgr := policyTestManager(nil)
	resolver := &countingSkillResolver{}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	if _, err := mgr.Start(ctx, api.StartOptions{Name: "kept", ProjectPath: e.scion, Template: "tplk", NoAuth: true}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	agentDir := config.ResolveAgentDir(e.scion, "kept")
	home := config.GetAgentHomePath(e.scion, "kept")
	skillsDir := filepath.Join(home, ".agents", "skills")
	start := func(inline *api.ScionConfig) {
		t.Helper()
		mergeIntoPersistedConfig(t, agentDir, inline)
		opts := api.StartOptions{Name: "kept", ProjectPath: e.scion, NoAuth: true, InlineConfig: inline, RerenderKeys: api.RerenderConfigKeys(inline)}
		if _, err := mgr.Start(ctx, opts); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	changed := []string{"agent_instructions", "services", "skills"}
	start(&api.ScionConfig{
		AgentInstructions: "set instructions",
		Services:          []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}},
		Skills:            []api.SkillReference{{URI: "hub://skills/review"}, {URI: "hub://skills/test"}},
		ChangedFields:     changed,
	})
	instructions := filepath.Join(home, ".scion", "harness", "inputs", "instructions.md")
	services := filepath.Join(home, ".scion", servicesFileName)
	if got := readFileString(t, instructions); !strings.Contains(got, "set instructions") {
		t.Fatalf("instructions after the set = %q", got)
	}
	if got := readFileString(t, services); !strings.Contains(got, "postgres") {
		t.Fatalf("services after the set = %q", got)
	}
	for _, name := range []string{"review", "test"} {
		if _, err := os.Stat(filepath.Join(skillsDir, name, "SKILL.md")); err != nil {
			t.Fatalf("skill %s not installed: %v", name, err)
		}
	}

	// The hub clears agent_instructions and services and drops one skill.
	start(&api.ScionConfig{
		Skills:        []api.SkillReference{{URI: "hub://skills/review"}},
		ChangedFields: changed,
	})
	if got := readFileString(t, instructions); !strings.Contains(got, "set instructions") {
		t.Fatalf("a start applied the cleared instructions: %q", got)
	}
	if got := readFileString(t, services); !strings.Contains(got, "postgres") {
		t.Fatalf("a start applied the cleared services: %q", got)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "test", "SKILL.md")); err != nil {
		t.Fatalf("a start uninstalled the removed skill (the hub reports the removal held): %v", err)
	}
}

// TestRerender_ClearedPromptRemovedForNonContainerHarness: for a harness
// that writes prompts into the agent home, a system prompt file Scion
// rendered last time (recorded by provisioning or an earlier re-render)
// that the render no longer produces (cleared, no template default) is
// removed, while the instructions file is rewritten.
func TestRerender_ClearedPromptRemovedForNonContainerHarness(t *testing.T) {
	f := newRerenderFixture(t)
	h := &harness.Generic{}
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	writeFileString(t, promptFile, "old prompt")
	if err := resetRerenderRecord(f.agentDir, []string{".scion/system_prompt.md"}); err != nil {
		t.Fatal(err)
	}
	inline := &api.ScionConfig{ClearedFields: []string{"system_prompt"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(h, []string{"system_prompt"}, &api.ScionConfig{}, inline, []*config.Template{{Name: "t", Path: t.TempDir()}})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(promptFile); !os.IsNotExist(err) {
		t.Fatalf("a cleared system prompt Scion rendered stayed in the agent home (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(f.agentHome, "agents.md")); err != nil {
		t.Fatalf("instructions not rendered: %v", err)
	}
	if rec := readRerenderRecord(f.agentDir); len(rec.SystemPromptPaths) != 0 {
		t.Fatalf("record still lists system prompt paths: %v", rec.SystemPromptPaths)
	}
}

// TestRerender_ClearedPromptKeepsTemplateHomeFile: a system prompt file
// Scion did not render (one a template's home/ supplied) is kept when the
// system prompt is cleared, as a reincarnation keeps it.
func TestRerender_ClearedPromptKeepsTemplateHomeFile(t *testing.T) {
	f := newRerenderFixture(t)
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	writeFileString(t, promptFile, "template home prompt")
	// Provisioning rendered no system prompt.
	if err := resetRerenderRecord(f.agentDir, nil); err != nil {
		t.Fatal(err)
	}
	inline := &api.ScionConfig{ClearedFields: []string{"system_prompt"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"system_prompt"}, &api.ScionConfig{}, inline, []*config.Template{{Name: "t", Path: t.TempDir()}})); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "template home prompt" {
		t.Fatalf("a cleared system prompt removed or changed a template-home file: %q", got)
	}
}

// TestRerender_ClearedPromptLegacyAgentWithoutRecord: an agent provisioned
// before the record listed system-prompt paths has its prompt files counted
// as Scion-rendered, so a cleared system prompt with no template default
// still removes the old file.
func TestRerender_ClearedPromptLegacyAgentWithoutRecord(t *testing.T) {
	f := newRerenderFixture(t)
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	writeFileString(t, promptFile, "old prompt rendered by an older broker")
	inline := &api.ScionConfig{ClearedFields: []string{"system_prompt"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"system_prompt"}, &api.ScionConfig{}, inline, []*config.Template{{Name: "t", Path: t.TempDir()}})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(promptFile); !os.IsNotExist(err) {
		t.Fatalf("a legacy agent kept its cleared system prompt (err %v)", err)
	}
	if rec := readRerenderRecord(f.agentDir); !rec.SystemPromptPathsKnown {
		t.Fatalf("the prompt re-render did not mark the paths known: %+v", rec)
	}
}

// TestRerender_RecordWriteIsAtomic: the record is replaced by a rename, and
// no temporary file is left in the agent directory.
func TestRerender_RecordWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := writeRerenderRecord(dir, rerenderRecord{Fingerprints: map[string]string{"prompts": "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeRerenderRecord(dir, rerenderRecord{Fingerprints: map[string]string{"prompts": "b"}}); err != nil {
		t.Fatal(err)
	}
	if got := readRerenderRecord(dir).Fingerprints["prompts"]; got != "b" {
		t.Fatalf("record = %q, want b", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("agent directory holds %d entries, want only the record: %v", len(entries), entries)
	}
}

// TestProvisionAgent_RecordsSystemPromptPaths: provisioning a harness that
// reads prompts from the agent home records where it rendered the system
// prompt, so a later clear can remove exactly that file.
func TestProvisionAgent_RecordsSystemPromptPaths(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-decl", policyTestDecl)
	agentDir := config.ResolveAgentDir(e.scion, "prov-sp")
	if _, _, _, err := ProvisionAgent(context.Background(), "prov-sp", "", "", "hc-decl", e.scion, "", "", "", "", &api.ScionConfig{SystemPrompt: "be brief"}); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	rec := readRerenderRecord(agentDir)
	if len(rec.SystemPromptPaths) == 0 {
		t.Fatalf("provisioning recorded no system prompt paths: %+v", rec)
	}
	for _, rel := range rec.SystemPromptPaths {
		if got := readFileString(t, filepath.Join(config.GetAgentHomePath(e.scion, "prov-sp"), filepath.FromSlash(rel))); !strings.Contains(got, "be brief") {
			t.Fatalf("recorded path %s does not hold the rendered prompt: %q", rel, got)
		}
	}
	if len(rec.Fingerprints) != 0 {
		t.Fatalf("provisioning recorded fingerprints: %v", rec.Fingerprints)
	}
	if !rec.SystemPromptPathsKnown {
		t.Fatalf("provisioning did not mark the paths known: %+v", rec)
	}
}

func TestSystemPromptTargets(t *testing.T) {
	got, err := systemPromptTargets(&harness.Generic{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".scion/system_prompt.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("systemPromptTargets(generic) = %v, want %v", got, want)
	}
}

func TestRerenderSkippedWarning(t *testing.T) {
	got := rerenderSkippedWarning([]string{"skills", "system_prompt"})
	if !strings.HasPrefix(got, "skills, system_prompt: not re-rendered at this start") || !strings.Contains(got, "next reincarnation") {
		t.Fatalf("warning = %q", got)
	}
}

// TestRerender_InstructionsChangeKeepsTemplateHomePrompt: a system prompt
// file the render does not produce (here one a template's home/ supplied)
// is left alone when only the agent instructions change; only a start that
// clears the system prompt removes it.
func TestRerender_InstructionsChangeKeepsTemplateHomePrompt(t *testing.T) {
	f := newRerenderFixture(t)
	promptFile := filepath.Join(f.agentHome, ".scion", "system_prompt.md")
	writeFileString(t, promptFile, "template home prompt")
	cfg := &api.ScionConfig{AgentInstructions: "new instructions"}
	inline := &api.ScionConfig{AgentInstructions: "new instructions", ChangedFields: []string{"agent_instructions"}}
	if _, err := rerenderProvisionedFiles(context.Background(), f.inputs(&harness.Generic{}, []string{"agent_instructions"}, cfg, inline, []*config.Template{{Name: "t", Path: t.TempDir()}})); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, promptFile); got != "template home prompt" {
		t.Fatalf("an instructions-only re-render changed the system prompt file: %q", got)
	}
}

// versionedSkillResolver resolves "<base>@<version>" references to a skill
// named after the base, with a SKILL.md naming the version, and records
// the references of each call.
type versionedSkillResolver struct{ calls [][]string }

func (r *versionedSkillResolver) Resolve(_ context.Context, refs []api.SkillReference, _ ResolveOpts) (*ResolveResult, error) {
	var uris []string
	out := &ResolveResult{}
	for _, ref := range refs {
		uris = append(uris, ref.URI)
		base, version, _ := strings.Cut(ref.URI, "@")
		name := base[strings.LastIndex(base, "/")+1:]
		out.Resolved = append(out.Resolved, ResolvedSkill{
			Name:    name,
			URI:     ref.URI,
			Version: version,
			Files:   []ResolvedFile{{Path: "SKILL.md", Content: []byte("version " + version + "\n")}},
		})
	}
	r.calls = append(r.calls, uris)
	return out, nil
}

// TestStart_SkillVersionChangeInstallsNewVersion: a skill version change
// (same base URI and install name), which the hub reports applied at the
// next start, ends with the new version installed in place of the old.
// The persisted list holds only the new reference, as the broker's merge
// and dedupe leave it (pkg/runtimebroker
// TestApplyInlineConfigUpdate_VersionCollapse); the start then resolves
// only that reference and replaces the installed directory.
func TestStart_SkillVersionChangeInstallsNewVersion(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-skills", policyTestScripted+"skills_dir: .agents/skills\n")
	e.template(t, "tplv", "harness_config: hc-skills\n")
	mgr := policyTestManager(nil)
	resolver := &versionedSkillResolver{}
	ctx := ContextWithSkillResolver(context.Background(), resolver)
	if _, err := mgr.Start(ctx, api.StartOptions{Name: "ver", ProjectPath: e.scion, Template: "tplv", NoAuth: true}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	agentDir := config.ResolveAgentDir(e.scion, "ver")
	home := config.GetAgentHomePath(e.scion, "ver")
	skill := filepath.Join(home, ".agents", "skills", "a", "SKILL.md")

	setSkills := func(uri string) *api.ScionConfig {
		t.Helper()
		path := filepath.Join(agentDir, "scion-agent.json")
		var cfg api.ScionConfig
		if err := json.Unmarshal([]byte(readFileString(t, path)), &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.Skills = []api.SkillReference{{URI: uri, Scope: "template"}}
		data, err := json.Marshal(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		writeFileString(t, path, string(data))
		return &api.ScionConfig{Skills: []api.SkillReference{{URI: uri}}, ChangedFields: []string{"skills"}}
	}
	start := func(inline *api.ScionConfig) {
		t.Helper()
		if _, err := mgr.Start(ctx, api.StartOptions{Name: "ver", ProjectPath: e.scion, NoAuth: true, InlineConfig: inline, RerenderKeys: api.RerenderConfigKeys(inline)}); err != nil {
			t.Fatalf("start: %v", err)
		}
	}

	start(setSkills("hub://skills/a@v1"))
	if got := readFileString(t, skill); got != "version v1\n" {
		t.Fatalf("after v1: SKILL.md = %q", got)
	}
	start(setSkills("hub://skills/a@v2"))
	if got := readFileString(t, skill); got != "version v2\n" {
		t.Fatalf("after the version change: SKILL.md = %q, want v2", got)
	}
	last := resolver.calls[len(resolver.calls)-1]
	if len(last) != 1 || last[0] != "hub://skills/a@v2" {
		t.Fatalf("the start resolved %v, want only hub://skills/a@v2", last)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".agents", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "a") && e.Name() != "a" {
			t.Fatalf("a second copy of the skill is installed: %s", e.Name())
		}
		if strings.HasPrefix(e.Name(), ".a.") {
			t.Fatalf("a temporary skill directory was left behind: %s", e.Name())
		}
	}
	record := readFileString(t, filepath.Join(home, ".scion", resolvedSkillsInputName))
	if !strings.Contains(record, "hub://skills/a@v2") || strings.Contains(record, "hub://skills/a@v1") {
		t.Fatalf("the resolution record does not list only v2: %s", record)
	}
}
