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

package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// seedOpenCodeDir seeds the embedded OpenCode harness-config into a temp dir
// using the same code path operators run during scion init / harness-config
// upgrade. It returns the absolute target dir so tests can inspect it.
func seedOpenCodeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := config.SeedHarnessConfig(dir, &OpenCode{}, false); err != nil {
		t.Fatalf("SeedHarnessConfig: %v", err)
	}
	return dir
}

// TestOpenCodeEmbedsSeedRootSupportFiles verifies the new provision.py and
// the existing opencode.json land where Phase 1 said they should: provision.py
// at the harness-config root, opencode.json under home/.config/opencode/.
func TestOpenCodeEmbedsSeedRootSupportFiles(t *testing.T) {
	dir := seedOpenCodeDir(t)

	// provision.py is a root-level support file (Phase 1 allowlist).
	provPath := filepath.Join(dir, "provision.py")
	if _, err := os.Stat(provPath); err != nil {
		t.Fatalf("expected provision.py at harness-config root: %v", err)
	}

	// opencode.json is the harness-native settings file; it lives under home.
	opencodeJSON := filepath.Join(dir, "home", ".config", "opencode", "opencode.json")
	if _, err := os.Stat(opencodeJSON); err != nil {
		t.Fatalf("expected opencode.json under home/.config/opencode/: %v", err)
	}

	// config.yaml at the root must be valid and declare the builtin provisioner
	// — Phase 4 ships the script but does not flip the type until activation.
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	if hc.Config.Provisioner == nil {
		t.Fatal("expected provisioner block in seeded config.yaml")
	}
	if hc.Config.Provisioner.Type != "builtin" {
		t.Errorf("provisioner.type=%q want builtin (script must opt in)", hc.Config.Provisioner.Type)
	}
	if len(hc.Config.Provisioner.Command) == 0 {
		t.Error("expected provisioner.command to be staged for future activation")
	}
}

// TestOpenCodeActivateScriptFlipsProvisionerType ensures `harness-config
// upgrade --activate-script opencode` flips the type and produces a backup of
// the previous config.yaml. This is the operator-facing migration step.
func TestOpenCodeActivateScriptFlipsProvisionerType(t *testing.T) {
	dir := seedOpenCodeDir(t)

	plan, err := config.UpgradeHarnessConfig(dir, &OpenCode{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
		Now:            func() time.Time { return time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig --activate-script: %v", err)
	}
	if !plan.Changed {
		t.Fatal("expected activation to change config")
	}

	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir after activate: %v", err)
	}
	if hc.Config.Provisioner == nil || hc.Config.Provisioner.Type != "container-script" {
		t.Fatalf("provisioner.type after activate=%q want container-script", hc.Config.Provisioner.Type)
	}
	if len(plan.Backups) != 1 {
		t.Fatalf("expected one backup, got %v", plan.Backups)
	}
}

// TestOpenCodeContainerScriptHarnessParity asserts the ContainerScriptHarness
// wrapper produces the same observable command/env/capability/getter values as
// the compiled OpenCode harness for the embedded config. Parity is the
// acceptance gate from Phase 0; this test makes it executable for OpenCode.
func TestOpenCodeContainerScriptHarnessParity(t *testing.T) {
	dir := seedOpenCodeDir(t)

	// Activate script so NewContainerScriptHarness accepts the entry.
	if _, err := config.UpgradeHarnessConfig(dir, &OpenCode{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
	}); err != nil {
		t.Fatalf("activate script: %v", err)
	}
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}
	builtin := &OpenCode{}

	// 1. Name must match — both report "opencode" so dispatch logic stays consistent.
	if scripted.Name() != builtin.Name() {
		t.Errorf("Name parity: scripted=%q builtin=%q", scripted.Name(), builtin.Name())
	}
	if scripted.DefaultConfigDir() != builtin.DefaultConfigDir() {
		t.Errorf("DefaultConfigDir: scripted=%q builtin=%q", scripted.DefaultConfigDir(), builtin.DefaultConfigDir())
	}
	if scripted.SkillsDir() != builtin.SkillsDir() {
		t.Errorf("SkillsDir: scripted=%q builtin=%q", scripted.SkillsDir(), builtin.SkillsDir())
	}
	if scripted.GetInterruptKey() != builtin.GetInterruptKey() {
		t.Errorf("GetInterruptKey: scripted=%q builtin=%q", scripted.GetInterruptKey(), builtin.GetInterruptKey())
	}

	// 2. GetCommand must match across the three operative shapes.
	cases := []struct {
		name    string
		task    string
		resume  bool
		baseArg []string
	}{
		{"resume_no_task", "", true, nil},
		{"task_only", "fix the bug", false, nil},
		{"task_with_base_args", "do it", false, []string{"--debug"}},
	}
	for _, tc := range cases {
		t.Run("GetCommand_"+tc.name, func(t *testing.T) {
			gotS := scripted.GetCommand(tc.task, tc.resume, tc.baseArg)
			gotB := builtin.GetCommand(tc.task, tc.resume, tc.baseArg)
			if strings.Join(gotS, " ") != strings.Join(gotB, " ") {
				t.Errorf("scripted=%v builtin=%v", gotS, gotB)
			}
		})
	}

	// 3. AdvancedCapabilities must report the same shape; the embedded YAML
	// is the single source of truth for both, so any drift indicates a bug
	// in either the YAML mapping or the compiled getter.
	gotCaps := scripted.AdvancedCapabilities()
	wantCaps := builtin.AdvancedCapabilities()
	if gotCaps.Harness != wantCaps.Harness {
		t.Errorf("Capabilities.Harness: scripted=%q builtin=%q", gotCaps.Harness, wantCaps.Harness)
	}
	if gotCaps.Limits.MaxDuration.Support != wantCaps.Limits.MaxDuration.Support {
		t.Errorf("Capabilities.Limits.MaxDuration: scripted=%v builtin=%v", gotCaps.Limits.MaxDuration, wantCaps.Limits.MaxDuration)
	}
	if gotCaps.Auth.APIKey.Support != wantCaps.Auth.APIKey.Support {
		t.Errorf("Capabilities.Auth.APIKey: scripted=%v builtin=%v", gotCaps.Auth.APIKey, wantCaps.Auth.APIKey)
	}
	if gotCaps.Auth.AuthFile.Support != wantCaps.Auth.AuthFile.Support {
		t.Errorf("Capabilities.Auth.AuthFile: scripted=%v builtin=%v", gotCaps.Auth.AuthFile, wantCaps.Auth.AuthFile)
	}
	if gotCaps.Auth.VertexAI.Support != wantCaps.Auth.VertexAI.Support {
		t.Errorf("Capabilities.Auth.VertexAI: scripted=%v builtin=%v", gotCaps.Auth.VertexAI, wantCaps.Auth.VertexAI)
	}
	if gotCaps.Prompts.SystemPrompt.Support != wantCaps.Prompts.SystemPrompt.Support {
		t.Errorf("Capabilities.Prompts.SystemPrompt: scripted=%v builtin=%v", gotCaps.Prompts.SystemPrompt, wantCaps.Prompts.SystemPrompt)
	}
}

// TestOpenCodeContainerScriptHarnessStagesScript verifies Provision() copies
// the seeded provision.py into the agent bundle and writes a wrapper that
// targets sciontool harness provision. The bundle is what the in-container
// hook actually runs.
func TestOpenCodeContainerScriptHarnessStagesScript(t *testing.T) {
	dir := seedOpenCodeDir(t)
	if _, err := config.UpgradeHarnessConfig(dir, &OpenCode{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
	}); err != nil {
		t.Fatalf("activate script: %v", err)
	}
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	if err := scripted.Provision(context.Background(), "researcher", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bundle := filepath.Join(agentHome, ".scion", "harness")
	stagedScript := filepath.Join(bundle, "provision.py")
	if _, err := os.Stat(stagedScript); err != nil {
		t.Fatalf("provision.py not staged into bundle: %v", err)
	}

	// The staged script must be byte-identical to the source-of-truth in the
	// seeded harness-config dir, otherwise upgrade workflows will silently
	// drift container behavior away from the hub artifact.
	stagedBytes, err := os.ReadFile(stagedScript)
	if err != nil {
		t.Fatal(err)
	}
	srcBytes, err := os.ReadFile(filepath.Join(dir, "provision.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stagedBytes) != string(srcBytes) {
		t.Error("staged provision.py differs from harness-config copy")
	}

	wrapper := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "20-harness-provision")
	wrapperBytes, err := os.ReadFile(wrapper)
	if err != nil {
		t.Fatalf("hook wrapper missing: %v", err)
	}
	if !strings.Contains(string(wrapperBytes), "sciontool harness provision") {
		t.Errorf("wrapper does not invoke sciontool harness provision: %s", wrapperBytes)
	}
}

// TestOpenCodeProvisionScript_Integration_HappyPath runs the actual Python
// script against a synthetic manifest and validates outputs. We skip when
// python3 is unavailable so the test is portable, and use a tightly-scoped
// $HOME to avoid leaking host paths into resolved-auth.json.
func TestOpenCodeProvisionScript_Integration_HappyPath(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedOpenCodeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "opencode"},
		"inputs":             map[string]any{},
		"outputs": map[string]any{
			"env":           filepath.Join(bundle, "outputs", "env.json"),
			"resolved_auth": filepath.Join(bundle, "outputs", "resolved-auth.json"),
		},
		"platform": map[string]any{"goos": "linux", "goarch": "amd64"},
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	candidates := map[string]any{
		"schema_version":  1,
		"explicit_type":   "",
		"resolved_method": "container-script",
		"env_vars":        []string{"OPENAI_API_KEY"},
		"files":           []any{},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision script failed: %v\noutput: %s", err, out)
	}

	resolvedBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "resolved-auth.json"))
	if err != nil {
		t.Fatalf("resolved-auth.json missing: %v\nscript output: %s", err, out)
	}
	var resolved map[string]any
	if err := json.Unmarshal(resolvedBytes, &resolved); err != nil {
		t.Fatalf("resolved-auth.json invalid: %v", err)
	}
	if resolved["method"] != "api-key" {
		t.Errorf("method=%v want api-key", resolved["method"])
	}
	if resolved["env_var"] != "OPENAI_API_KEY" {
		t.Errorf("env_var=%v want OPENAI_API_KEY (precedence: only OpenAI was offered)", resolved["env_var"])
	}

	envBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "env.json"))
	if err != nil {
		t.Fatalf("env.json missing: %v", err)
	}
	var envOverlay map[string]any
	if err := json.Unmarshal(envBytes, &envOverlay); err != nil {
		t.Fatalf("env.json invalid: %v", err)
	}
	if len(envOverlay) != 0 {
		t.Errorf("env.json should be empty for OpenCode (no overrides), got %v", envOverlay)
	}
}

// TestOpenCodeProvisionScript_Integration_NoCreds asserts the script exits
// non-zero with an actionable message when nothing is staged. This matches
// the compiled harness's pre-launch failure mode.
func TestOpenCodeProvisionScript_Integration_NoCreds(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedOpenCodeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "opencode"},
		"inputs":             map[string]any{},
		"outputs": map[string]any{
			"env":           filepath.Join(bundle, "outputs", "env.json"),
			"resolved_auth": filepath.Join(bundle, "outputs", "resolved-auth.json"),
		},
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(bundle, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	candidates := map[string]any{
		"schema_version":  1,
		"explicit_type":   "",
		"resolved_method": "container-script",
		"env_vars":        []string{},
		"files":           []any{},
	}
	candBytes, _ := json.Marshal(candidates)
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success. output: %s", out)
	}
	if !strings.Contains(string(out), "no valid auth method") {
		t.Errorf("expected actionable no-creds message, got: %s", out)
	}
}

// TestOpenCodeContainerScriptResolveAuthShape verifies the container-script
// ResolveAuth surfaces the values the script will need (env keys + files)
// while never returning the original Method strings the runtime gates on.
// This protects callers like applyResolvedAuth that branch on Method.
func TestOpenCodeContainerScriptResolveAuthShape(t *testing.T) {
	dir := seedOpenCodeDir(t)
	if _, err := config.UpgradeHarnessConfig(dir, &OpenCode{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatal(err)
	}

	// Pass both an Anthropic key and an auth file; the container-script
	// wrapper must surface BOTH so the in-container script can choose,
	// whereas the compiled harness would have collapsed to one.
	resolved, err := scripted.ResolveAuth(api.AuthConfig{
		AnthropicAPIKey:  "sk-ant-xx",
		OpenCodeAuthFile: "/tmp/auth.json",
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if resolved.Method != "container-script" {
		t.Errorf("Method=%q want container-script (final selection deferred to script)", resolved.Method)
	}
	if resolved.EnvVars["ANTHROPIC_API_KEY"] != "sk-ant-xx" {
		t.Errorf("expected ANTHROPIC_API_KEY to flow through, got %v", resolved.EnvVars)
	}
	foundOpenCodeAuthFile := false
	for _, f := range resolved.Files {
		if f.SourcePath == "/tmp/auth.json" && strings.HasSuffix(f.ContainerPath, "/auth.json") {
			foundOpenCodeAuthFile = true
		}
	}
	if !foundOpenCodeAuthFile {
		t.Errorf("expected OpenCode auth file in Files mapping, got %#v", resolved.Files)
	}
}
