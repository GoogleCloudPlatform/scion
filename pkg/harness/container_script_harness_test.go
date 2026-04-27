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
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func newTestContainerScriptHarness(t *testing.T) (*ContainerScriptHarness, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: testharness\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")
	entry := config.HarnessConfigEntry{
		Harness:          "testharness",
		Image:            "scion-test:latest",
		ConfigDir:        ".test",
		SkillsDir:        ".test/skills",
		InterruptKey:     "Escape",
		InstructionsFile: ".test/INSTRUCTIONS.md",
		SystemPromptFile: ".test/system.md",
		SystemPromptMode: "native",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
			Timeout:          "10s",
			LifecycleEvents:  []string{"pre-start"},
		},
		Command: &config.HarnessCommandConfig{
			Base:         []string{"testcli"},
			ResumeFlag:   "--resume",
			TaskFlag:     "--prompt",
			TaskPosition: "after_base_args",
		},
		EnvTemplate: map[string]string{
			"TEST_AGENT": "{{ .AgentName }}",
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}
	return h, dir
}

func TestContainerScriptHarness_RejectsNonContainerScriptType(t *testing.T) {
	dir := t.TempDir()
	entry := config.HarnessConfigEntry{
		Harness:     "claude",
		Provisioner: &config.HarnessProvisionerConfig{Type: "builtin"},
	}
	if _, err := NewContainerScriptHarness(dir, entry); err == nil {
		t.Fatal("expected error for non container-script provisioner")
	}
}

func TestContainerScriptHarness_BasicGetters(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	if h.Name() != "testharness" {
		t.Errorf("Name=%q", h.Name())
	}
	if h.DefaultConfigDir() != ".test" {
		t.Errorf("DefaultConfigDir=%q", h.DefaultConfigDir())
	}
	if h.SkillsDir() != ".test/skills" {
		t.Errorf("SkillsDir=%q", h.SkillsDir())
	}
	if h.GetInterruptKey() != "Escape" {
		t.Errorf("GetInterruptKey=%q", h.GetInterruptKey())
	}
	cmd := h.GetCommand("hello", false, []string{"--debug"})
	want := []string{"testcli", "--debug", "--prompt", "hello"}
	if strings.Join(cmd, " ") != strings.Join(want, " ") {
		t.Errorf("GetCommand=%v want %v", cmd, want)
	}
	cmd2 := h.GetCommand("", true, nil)
	want2 := []string{"testcli", "--resume"}
	if strings.Join(cmd2, " ") != strings.Join(want2, " ") {
		t.Errorf("GetCommand resume=%v want %v", cmd2, want2)
	}
}

func TestContainerScriptHarness_GetEnvTemplating(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	env := h.GetEnv("agent42", "/home/scion", "scion")
	if env["SCION_AGENT_NAME"] != "agent42" {
		t.Errorf("SCION_AGENT_NAME=%q", env["SCION_AGENT_NAME"])
	}
	if env["TEST_AGENT"] != "agent42" {
		t.Errorf("TEST_AGENT=%q want agent42", env["TEST_AGENT"])
	}
}

func TestContainerScriptHarness_StagesBundle(t *testing.T) {
	h, configDir := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()

	// Stage some inputs first to verify the manifest references them.
	if err := h.InjectAgentInstructions(agentHome, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := h.InjectSystemPrompt(agentHome, []byte("system")); err != nil {
		t.Fatal(err)
	}

	if err := h.Provision(context.Background(), "agent1", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bundle := filepath.Join(agentHome, ".scion", "harness")
	for _, want := range []string{
		"config.yaml",
		"provision.py",
		"manifest.json",
		"inputs/instructions.md",
		"inputs/system-prompt.md",
		"outputs",
		"secrets",
	} {
		full := filepath.Join(bundle, want)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("missing staged file %s: %v", want, err)
		}
	}

	// Manifest should be valid JSON and reference container-side paths.
	manifestData, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ProvisionManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Errorf("schema_version=%d", manifest.SchemaVersion)
	}
	if manifest.HarnessConfig.Provisioner == nil || manifest.HarnessConfig.Provisioner.Type != "container-script" {
		t.Errorf("manifest.HarnessConfig.Provisioner unset or wrong type")
	}
	if !strings.HasPrefix(manifest.HarnessBundleDir, "$HOME/.scion/harness") {
		t.Errorf("HarnessBundleDir=%q does not target $HOME", manifest.HarnessBundleDir)
	}
	if !strings.HasSuffix(manifest.Inputs.Instructions, "instructions.md") {
		t.Errorf("manifest.Inputs.Instructions=%q", manifest.Inputs.Instructions)
	}
	if !strings.HasSuffix(manifest.Outputs.Env, "env.json") {
		t.Errorf("manifest.Outputs.Env=%q", manifest.Outputs.Env)
	}

	// Hook wrapper should be staged and executable.
	wrapper := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "20-harness-provision")
	info, err := os.Stat(wrapper)
	if err != nil {
		t.Fatalf("missing hook wrapper: %v", err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Errorf("hook wrapper not executable: mode=%v", info.Mode())
	}
	wrapperData, _ := os.ReadFile(wrapper)
	if !strings.Contains(string(wrapperData), "sciontool harness provision --manifest") {
		t.Errorf("hook wrapper missing sciontool invocation: %s", wrapperData)
	}

	// Verify provision.py was copied (executable).
	if _, err := os.Stat(filepath.Join(bundle, "provision.py")); err != nil {
		t.Fatalf("provision.py not staged: %v", err)
	}

	// Cleanup hint — silence unused variable.
	_ = configDir
}

func TestContainerScriptHarness_ResolveAuth_StagesCandidateEnv(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	resolved, err := h.ResolveAuth(api.AuthConfig{
		AnthropicAPIKey: "sk-ant-xxx",
		ClaudeAuthFile:  "/tmp/.credentials.json",
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if resolved.Method != "container-script" {
		t.Errorf("Method=%q", resolved.Method)
	}
	if resolved.EnvVars["ANTHROPIC_API_KEY"] != "sk-ant-xxx" {
		t.Errorf("missing ANTHROPIC_API_KEY in env")
	}
	if len(resolved.Files) != 1 || !strings.Contains(resolved.Files[0].ContainerPath, ".claude/.credentials.json") {
		t.Errorf("unexpected file mappings: %+v", resolved.Files)
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_WritesCandidates(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()
	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "sk-x"},
	}
	if err := h.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload["resolved_method"] != "container-script" {
		t.Errorf("resolved_method=%v", payload["resolved_method"])
	}
}

func TestResolve_ContainerScriptDispatch(t *testing.T) {
	home := t.TempDir()
	configsDir := filepath.Join(home, ".scion", "harness-configs")
	hcDir := filepath.Join(configsDir, "scripted")
	writeFile(t, filepath.Join(hcDir, "config.yaml"), `harness: scripted
image: scion-test:latest
provisioner:
  type: container-script
  interface_version: 1
  command: ["python3", "/home/scion/.scion/harness/provision.py"]
`)
	writeFile(t, filepath.Join(hcDir, "provision.py"), "#!/usr/bin/env python3\n")

	t.Setenv("HOME", home)

	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "scripted"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Implementation != "container-script" {
		t.Errorf("Implementation=%q want container-script", resolved.Implementation)
	}
	if _, ok := resolved.Harness.(*ContainerScriptHarness); !ok {
		t.Errorf("expected *ContainerScriptHarness, got %T", resolved.Harness)
	}
}

func TestResolve_BuiltinFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// No on-disk harness-config — Resolve should still return the built-in.
	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "claude"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Implementation != "builtin" {
		t.Errorf("Implementation=%q want builtin", resolved.Implementation)
	}
	if _, ok := resolved.Harness.(*ClaudeCode); !ok {
		t.Errorf("expected *ClaudeCode, got %T", resolved.Harness)
	}
}

func TestResolve_DeclarativeGenericFromConfig(t *testing.T) {
	home := t.TempDir()
	configsDir := filepath.Join(home, ".scion", "harness-configs")
	hcDir := filepath.Join(configsDir, "custom-cli")
	writeFile(t, filepath.Join(hcDir, "config.yaml"), `harness: custom-cli
image: scion-base:latest
config_dir: .custom
command:
  base: ["customcli", "run"]
  task_position: after_base_args
`)
	t.Setenv("HOME", home)

	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "custom-cli"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Implementation != "generic" {
		t.Errorf("Implementation=%q", resolved.Implementation)
	}
	if _, ok := resolved.Harness.(*DeclarativeGenericHarness); !ok {
		t.Errorf("expected DeclarativeGenericHarness, got %T", resolved.Harness)
	}
	cmd := resolved.Harness.GetCommand("hello", false, nil)
	if strings.Join(cmd, " ") != "customcli run hello" {
		t.Errorf("GetCommand=%v", cmd)
	}
}
