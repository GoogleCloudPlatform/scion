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

func TestContainerScriptHarness_GetInterruptSequence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: seqtest\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")

	// No sequence configured — should return nil.
	entry := config.HarnessConfigEntry{
		Harness:      "seqtest",
		Image:        "scion-test:latest",
		InterruptKey: "C-c",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatal(err)
	}
	if seq := h.GetInterruptSequence(); seq != nil {
		t.Errorf("expected nil sequence, got %v", seq)
	}

	// With interrupt_sequence and interrupt_signal=sequence.
	entry.InterruptSequence = []string{"Escape", "Escape", "Escape"}
	entry.InterruptSignal = "sequence"
	h2, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatal(err)
	}
	seq := h2.GetInterruptSequence()
	if len(seq) != 3 {
		t.Fatalf("expected 3-key sequence, got %v", seq)
	}
	for i, want := range []string{"Escape", "Escape", "Escape"} {
		if seq[i] != want {
			t.Errorf("seq[%d]=%q, want %q", i, seq[i], want)
		}
	}

	// With interrupt_sequence populated but no explicit interrupt_signal — still returns sequence.
	entry.InterruptSignal = ""
	h3, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatal(err)
	}
	if seq := h3.GetInterruptSequence(); len(seq) != 3 {
		t.Errorf("expected sequence even without signal field, got %v", seq)
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
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-xxx"},
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

func TestContainerScriptHarness_ApplyAuthSettings_ExplicitTypeFromResolvedAuth(t *testing.T) {
	// Regression test: when SCION_HARNESS_SELECTED_AUTH is present in the
	// resolved auth's env vars, explicit_type in auth-candidates.json must
	// come from it — not from c.entry.AuthSelectedType (which may be empty).
	// This ensures the container-side provisioner uses the Go side's resolved
	// auth method instead of falling back to auto-detection, which can pick a
	// wrong higher-priority method (e.g., api-key over vertex-ai).
	// entry.AuthSelectedType is intentionally empty here (zero value from
	// newTestContainerScriptHarness) to isolate the resolved-auth signal.
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()

	// Simulate the scenario: resolved auth knows it's vertex-ai
	// (SCION_HARNESS_SELECTED_AUTH), but c.entry.AuthSelectedType is empty
	// (harness config metadata doesn't have it). Both ANTHROPIC_API_KEY and
	// GOOGLE_CLOUD_PROJECT are present as candidates.
	resolved := &api.ResolvedAuth{
		Method: "container-script",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "vertex-ai",
			"ANTHROPIC_API_KEY":           "sk-test",
			"GOOGLE_CLOUD_PROJECT":        "my-project",
			"GOOGLE_CLOUD_REGION":         "us-central1",
		},
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
	if payload["explicit_type"] != "vertex-ai" {
		t.Errorf("explicit_type=%v, want vertex-ai", payload["explicit_type"])
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_ExplicitTypeFallsBackToEntry(t *testing.T) {
	// When SCION_HARNESS_SELECTED_AUTH is absent from resolved env vars,
	// explicit_type should fall back to c.entry.AuthSelectedType.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: testharness\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")
	entry := config.HarnessConfigEntry{
		Harness:          "testharness",
		Image:            "scion-test:latest",
		AuthSelectedType: "api-key",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
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
	if payload["explicit_type"] != "api-key" {
		t.Errorf("explicit_type=%v, want api-key", payload["explicit_type"])
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_RejectsHarnessImplementationName(t *testing.T) {
	// Regression test for data-corruption bug: if a prior backfill incorrectly
	// wrote the harness implementation name ("container-script") into
	// SCION_HARNESS_SELECTED_AUTH, ApplyAuthSettings must discard it and fall
	// back to c.entry.AuthSelectedType rather than propagating it as
	// explicit_type — which would crash the container-side provisioner with
	// "unknown auth type 'container-script'".
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: testharness\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")
	entry := config.HarnessConfigEntry{
		Harness:          "testharness",
		Image:            "scion-test:latest",
		AuthSelectedType: "vertex-ai",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	// Simulate corrupted state: SCION_HARNESS_SELECTED_AUTH contains the
	// harness implementation name instead of a valid auth type.
	resolved := &api.ResolvedAuth{
		Method: "container-script",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "container-script",
			"ANTHROPIC_API_KEY":           "sk-test",
		},
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
	// Must fall back to c.entry.AuthSelectedType, NOT use "container-script".
	if payload["explicit_type"] != "vertex-ai" {
		t.Errorf("explicit_type=%v, want vertex-ai (should reject container-script)", payload["explicit_type"])
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_RejectsAllImplementationNames(t *testing.T) {
	// Verify that all known harness implementation names are rejected.
	for _, implName := range []string{"container-script", "generic", "builtin", "passthrough"} {
		t.Run(implName, func(t *testing.T) {
			h, _ := newTestContainerScriptHarness(t)
			agentHome := t.TempDir()
			resolved := &api.ResolvedAuth{
				Method: "container-script",
				EnvVars: map[string]string{
					"SCION_HARNESS_SELECTED_AUTH": implName,
				},
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
			// explicit_type must NOT be the implementation name.
			if payload["explicit_type"] == implName {
				t.Errorf("explicit_type=%v — should have been rejected", implName)
			}
		})
	}
}

func TestIsHarnessImplementationName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"container-script", true},
		{"generic", true},
		{"builtin", true},
		{"passthrough", true},
		{"vertex-ai", false},
		{"api-key", false},
		{"oauth-token", false},
		{"auth-file", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsHarnessImplementationName(tt.name); got != tt.want {
				t.Errorf("IsHarnessImplementationName(%q)=%v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func newClaudeHarness(t *testing.T) *ContainerScriptHarness {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: claude\nimage: scion-claude:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")
	entry := config.HarnessConfigEntry{
		Harness: "claude",
		Image:   "scion-claude:latest",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "builtin",
			InterfaceVersion: 1,
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}
	return h
}

func TestContainerScriptHarness_ResolveAuth_VertexAICredentialTranslation(t *testing.T) {
	// When the harness is "claude" and auth type is "vertex-ai", ResolveAuth
	// must translate GCP env vars into Anthropic-specific env vars that Claude
	// Code requires. This translation is normally done by the Python
	// provisioner, but when provisioner type is "builtin" (no command), the
	// Python script never runs and Go must handle it.
	h := newClaudeHarness(t)

	resolved, err := h.ResolveAuth(api.AuthConfig{
		SelectedType:       "vertex-ai",
		GoogleCloudProject: "my-project",
		GoogleCloudRegion:  "us-central1",
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}

	// Must set Anthropic-specific Vertex AI env vars.
	if got := resolved.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"]; got != "my-project" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID=%q, want %q", got, "my-project")
	}
	if got := resolved.EnvVars["CLOUD_ML_REGION"]; got != "us-central1" {
		t.Errorf("CLOUD_ML_REGION=%q, want %q", got, "us-central1")
	}
	if got := resolved.EnvVars["CLAUDE_CODE_USE_VERTEX"]; got != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX=%q, want %q", got, "1")
	}

	// Must also still have the raw GCP env vars.
	if got := resolved.EnvVars["GOOGLE_CLOUD_PROJECT"]; got != "my-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT=%q, want %q", got, "my-project")
	}
	if got := resolved.EnvVars["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION=%q, want %q", got, "us-central1")
	}
}

func TestContainerScriptHarness_ResolveAuth_VertexAIInferredFromProject(t *testing.T) {
	// When SelectedType is empty but GoogleCloudProject is set, ResolveAuth
	// should infer vertex-ai and apply the credential translation.
	h := newClaudeHarness(t)

	resolved, err := h.ResolveAuth(api.AuthConfig{
		GoogleCloudProject: "inferred-project",
		GoogleCloudRegion:  "europe-west1",
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}

	if got := resolved.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"]; got != "inferred-project" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID=%q, want %q", got, "inferred-project")
	}
	if got := resolved.EnvVars["CLAUDE_CODE_USE_VERTEX"]; got != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX=%q, want %q", got, "1")
	}
}

func TestContainerScriptHarness_ResolveAuth_NoVertexTranslationForNonClaude(t *testing.T) {
	// Vertex AI credential translation must NOT happen for non-Claude harnesses.
	h, _ := newTestContainerScriptHarness(t) // harness name is "testharness"

	resolved, err := h.ResolveAuth(api.AuthConfig{
		SelectedType:       "vertex-ai",
		GoogleCloudProject: "my-project",
		GoogleCloudRegion:  "us-central1",
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}

	// Must NOT set Anthropic-specific vars for non-Claude harness.
	if _, ok := resolved.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"]; ok {
		t.Error("ANTHROPIC_VERTEX_PROJECT_ID should not be set for non-Claude harness")
	}
	if _, ok := resolved.EnvVars["CLAUDE_CODE_USE_VERTEX"]; ok {
		t.Error("CLAUDE_CODE_USE_VERTEX should not be set for non-Claude harness")
	}
	if _, ok := resolved.EnvVars["CLOUD_ML_REGION"]; ok {
		t.Error("CLOUD_ML_REGION should not be set for non-Claude harness")
	}
}

func TestContainerScriptHarness_ResolveAuth_NoVertexTranslationForAPIKey(t *testing.T) {
	// When auth type is api-key, Vertex AI translation must not happen
	// even for Claude harness.
	h := newClaudeHarness(t)

	resolved, err := h.ResolveAuth(api.AuthConfig{
		SelectedType: "api-key",
		EnvVars:      map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"},
	})
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}

	if _, ok := resolved.EnvVars["ANTHROPIC_VERTEX_PROJECT_ID"]; ok {
		t.Error("ANTHROPIC_VERTEX_PROJECT_ID should not be set for api-key auth")
	}
	if _, ok := resolved.EnvVars["CLAUDE_CODE_USE_VERTEX"]; ok {
		t.Error("CLAUDE_CODE_USE_VERTEX should not be set for api-key auth")
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_StagesFileSecrets(t *testing.T) {
	// Harness entry with a required_files declaration matching Codex auth-file.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: codex\nimage: scion-codex:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")

	entry := config.HarnessConfigEntry{
		Harness: "codex",
		Image:   "scion-codex:latest",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
		},
		Auth: &config.HarnessAuthMetadata{
			Types: map[string]config.HarnessAuthTypeMetadata{
				"auth-file": {
					RequiredFiles: []config.HarnessAuthFileRequirement{
						{
							Name:         "CODEX_AUTH",
							Type:         "file",
							TargetSuffix: "/.codex/auth.json",
							Field:        "CodexAuthFile",
						},
					},
				},
			},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()

	// Write a fake auth.json on the host.
	hostAuthFile := filepath.Join(t.TempDir(), "auth.json")
	writeFile(t, hostAuthFile, `{"auth_mode":"oauth","token":"tok-xxx"}`)

	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{},
		Files: []api.FileMapping{
			{SourcePath: hostAuthFile, ContainerPath: "~/.codex/auth.json"},
		},
	}

	if err := h.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings: %v", err)
	}

	// The FileMapping should have been removed from resolved.Files.
	if len(resolved.Files) != 0 {
		t.Errorf("expected resolved.Files to be empty after staging; got %+v", resolved.Files)
	}

	// The secret file should be written at secrets/CODEX_AUTH with mode 0600.
	secretPath := filepath.Join(agentHome, ".scion", "harness", "secrets", "CODEX_AUTH")
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("staged secret not found at %s: %v", secretPath, err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("secret mode=%o, want 0600", info.Mode().Perm())
	}
	content, _ := os.ReadFile(secretPath)
	if string(content) != `{"auth_mode":"oauth","token":"tok-xxx"}` {
		t.Errorf("secret content=%q", content)
	}

	// auth-candidates.json should have file_secret_files.CODEX_AUTH set.
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	fsf, ok := payload["file_secret_files"].(map[string]interface{})
	if !ok {
		t.Fatalf("file_secret_files missing or wrong type: %T", payload["file_secret_files"])
	}
	codexAuthPath, ok := fsf["CODEX_AUTH"].(string)
	if !ok || codexAuthPath == "" {
		t.Errorf("file_secret_files.CODEX_AUTH missing or empty: %v", fsf)
	}
	if !strings.HasPrefix(codexAuthPath, "$HOME/.scion/harness/secrets/") {
		t.Errorf("file_secret_files.CODEX_AUTH=%q does not have expected prefix", codexAuthPath)
	}

	// The files array should be empty (bind-mount removed).
	filesRaw, _ := payload["files"].([]interface{})
	if len(filesRaw) != 0 {
		t.Errorf("files array should be empty; got %v", filesRaw)
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_StagesFileSecrets_AbsolutePath(t *testing.T) {
	// Identical harness setup to StagesFileSecrets, but the FileMapping uses an
	// absolute container path (/home/scion/.codex/auth.json) instead of the
	// tilde form (~/.codex/auth.json). HasSuffix matching must handle both.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: codex\nimage: scion-codex:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")

	entry := config.HarnessConfigEntry{
		Harness: "codex",
		Image:   "scion-codex:latest",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
		},
		Auth: &config.HarnessAuthMetadata{
			Types: map[string]config.HarnessAuthTypeMetadata{
				"auth-file": {
					RequiredFiles: []config.HarnessAuthFileRequirement{
						{
							Name:         "CODEX_AUTH",
							Type:         "file",
							TargetSuffix: "/.codex/auth.json",
							Field:        "CodexAuthFile",
						},
					},
				},
			},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	hostAuthFile := filepath.Join(t.TempDir(), "auth.json")
	writeFile(t, hostAuthFile, `{"auth_mode":"oauth","token":"tok-abs"}`)

	// Provide an absolute container path — the suffix matcher must still match.
	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{},
		Files: []api.FileMapping{
			{SourcePath: hostAuthFile, ContainerPath: "/home/scion/.codex/auth.json"},
		},
	}

	if err := h.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings (absolute path): %v", err)
	}

	// The FileMapping must have been consumed (not left as a bind-mount).
	if len(resolved.Files) != 0 {
		t.Errorf("expected resolved.Files to be empty after staging; got %+v", resolved.Files)
	}

	// Secret file should be written.
	secretPath := filepath.Join(agentHome, ".scion", "harness", "secrets", "CODEX_AUTH")
	content, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("staged secret not found at %s: %v", secretPath, err)
	}
	if string(content) != `{"auth_mode":"oauth","token":"tok-abs"}` {
		t.Errorf("secret content=%q", content)
	}

	// auth-candidates.json must carry file_secret_files.CODEX_AUTH.
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	fsf, ok := payload["file_secret_files"].(map[string]interface{})
	if !ok {
		t.Fatalf("file_secret_files missing or wrong type: %T", payload["file_secret_files"])
	}
	if _, ok := fsf["CODEX_AUTH"]; !ok {
		t.Errorf("file_secret_files.CODEX_AUTH missing; got %v", fsf)
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_BrokerModePopulatesFileSecretFiles(t *testing.T) {
	// In broker mode, file content is staged via SCION_STAGED_SECRETS by
	// stagedsecrets.Write(), so FileMappings arrive with SourcePath=""
	// (cleared by run.go). stageFileSecretFiles must still populate the
	// file_secret_files map entry pointing to the ContainerPath so the
	// container-side provisioner can find the file. No secret file should
	// be written to the secrets dir (the broker already handled that).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: codex\nimage: scion-codex:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")

	entry := config.HarnessConfigEntry{
		Harness: "codex",
		Image:   "scion-codex:latest",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
		},
		Auth: &config.HarnessAuthMetadata{
			Types: map[string]config.HarnessAuthTypeMetadata{
				"auth-file": {
					RequiredFiles: []config.HarnessAuthFileRequirement{
						{
							Name:         "CODEX_AUTH",
							Type:         "file",
							TargetSuffix: "/.codex/auth.json",
							Field:        "CodexAuthFile",
						},
					},
				},
			},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()

	// Broker mode: SourcePath is empty (cleared by run.go), ContainerPath
	// is preserved so the map entry can be populated.
	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{},
		Files: []api.FileMapping{
			{SourcePath: "", ContainerPath: "~/.codex/auth.json"},
		},
	}

	if err := h.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings: %v", err)
	}

	// The FileMapping should have been consumed — NOT kept as a bind-mount.
	if len(resolved.Files) != 0 {
		t.Errorf("expected resolved.Files to be empty after staging; got %+v", resolved.Files)
	}

	// No secret file should be staged (broker handles content staging).
	secretPath := filepath.Join(agentHome, ".scion", "harness", "secrets", "CODEX_AUTH")
	if _, err := os.Stat(secretPath); err == nil {
		t.Errorf("secret file should NOT be staged in broker mode; found at %s", secretPath)
	}

	// auth-candidates.json must have file_secret_files.CODEX_AUTH pointing
	// to the container path where the broker stages the file.
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	fsf, ok := payload["file_secret_files"].(map[string]interface{})
	if !ok {
		t.Fatalf("file_secret_files missing or wrong type: %T", payload["file_secret_files"])
	}
	codexAuthPath, ok := fsf["CODEX_AUTH"].(string)
	if !ok || codexAuthPath == "" {
		t.Errorf("file_secret_files.CODEX_AUTH missing or empty: %v", fsf)
	}
	// The path should be the normalized ContainerPath ($HOME/... form).
	if codexAuthPath != "$HOME/.codex/auth.json" {
		t.Errorf("file_secret_files.CODEX_AUTH=%q, want $HOME/.codex/auth.json", codexAuthPath)
	}

	// The files array should be empty (no bind-mounts).
	filesRaw, _ := payload["files"].([]interface{})
	if len(filesRaw) != 0 {
		t.Errorf("files array should be empty; got %v", filesRaw)
	}
}

func TestContainerScriptHarness_ApplyAuthSettings_NonFileCredentialKeptAsBindMount(t *testing.T) {
	// FileMappings for credentials without a required_files declaration should
	// remain as bind-mounts (not staged as secrets).
	h, _ := newTestContainerScriptHarness(t) // no Auth metadata
	agentHome := t.TempDir()

	hostFile := filepath.Join(t.TempDir(), "gcloud.json")
	writeFile(t, hostFile, `{"type":"service_account"}`)

	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{},
		Files: []api.FileMapping{
			{SourcePath: hostFile, ContainerPath: "~/.config/gcloud/application_default_credentials.json"},
		},
	}

	if err := h.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings: %v", err)
	}

	// No Auth metadata → no required_files → file should remain in resolved.Files.
	if len(resolved.Files) != 1 {
		t.Errorf("expected 1 file to remain as bind-mount; got %+v", resolved.Files)
	}

	// No secret should be staged.
	secretDir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	entries, _ := os.ReadDir(secretDir)
	for _, e := range entries {
		t.Errorf("unexpected secret staged: %s", e.Name())
	}
}

func TestContainerScriptHarness_ApplyMCPSettings_WritesInput(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()

	servers := map[string]api.MCPServerConfig{
		"chrome-devtools": {
			Transport: api.MCPTransportStdio,
			Command:   "chrome-devtools-mcp",
			Args:      []string{"--headless"},
		},
		"remote_api": {
			Transport: api.MCPTransportSSE,
			URL:       "http://localhost:8080/mcp/sse",
		},
	}
	if err := h.ApplyMCPSettings(agentHome, servers); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "mcp-servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload["schema_version"] != float64(1) {
		t.Errorf("schema_version=%v", payload["schema_version"])
	}
	got, ok := payload["mcp_servers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp_servers is not an object: %T", payload["mcp_servers"])
	}
	if len(got) != 2 {
		t.Errorf("expected 2 servers, got %d", len(got))
	}
}

func TestContainerScriptHarness_ApplyMCPSettings_NoOpEmpty(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()
	if err := h.ApplyMCPSettings(agentHome, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(agentHome, ".scion", "harness", "inputs", "mcp-servers.json")); !os.IsNotExist(err) {
		t.Errorf("empty mcp servers should not write file; stat err=%v", err)
	}
}

func TestContainerScriptHarness_StagesScionHarnessHelper(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()
	if err := h.Provision(context.Background(), "agent1", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(agentHome, ".scion", "harness", "scion_harness.py")
	staged, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("scion_harness.py not staged: %v", err)
	}
	if string(staged) != string(SharedHarnessHelperSource()) {
		t.Errorf("staged scion_harness.py does not match embedded source")
	}
}

func TestContainerScriptHarness_VendoredLibStagesFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: testharness\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")
	libContent := "# vendored lib\nLIB_VERSION = \"2026-07-05\"\n"
	writeFile(t, filepath.Join(dir, "scion_harness.py"), libContent)
	entry := config.HarnessConfigEntry{
		Harness:   "testharness",
		Image:     "scion-test:latest",
		ConfigDir: ".test",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Lib:              "vendored",
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
			Timeout:          "10s",
			LifecycleEvents:  []string{"pre-start"},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}
	agentHome := t.TempDir()
	if err := h.Provision(context.Background(), "agent1", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(agentHome, ".scion", "harness", "scion_harness.py")
	staged, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("scion_harness.py not staged: %v", err)
	}
	if string(staged) != libContent {
		t.Errorf("vendored lib: got %q, want %q", string(staged), libContent)
	}
}

func TestContainerScriptHarness_VendoredLibMissingIsHardError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: testharness\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")
	// No scion_harness.py in the config dir.
	entry := config.HarnessConfigEntry{
		Harness:   "testharness",
		Image:     "scion-test:latest",
		ConfigDir: ".test",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Lib:              "vendored",
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
			Timeout:          "10s",
			LifecycleEvents:  []string{"pre-start"},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}
	agentHome := t.TempDir()
	err = h.Provision(context.Background(), "agent1", agentHome, agentHome, "/workspace")
	if err == nil {
		t.Fatal("expected error when vendored lib is missing, got nil")
	}
	if !strings.Contains(err.Error(), "provisioner.lib") {
		t.Errorf("error should mention provisioner.lib, got: %v", err)
	}
}

func TestContainerScriptHarness_ProvisionReferencesMCPInputInManifest(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()
	servers := map[string]api.MCPServerConfig{
		"x": {Transport: api.MCPTransportStdio, Command: "y"},
	}
	if err := h.ApplyMCPSettings(agentHome, servers); err != nil {
		t.Fatal(err)
	}
	if err := h.Provision(context.Background(), "a", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ProvisionManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(manifest.Inputs.MCPServers, "mcp-servers.json") {
		t.Errorf("manifest.Inputs.MCPServers=%q", manifest.Inputs.MCPServers)
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

func TestResolve_UnknownHarnessFallsToGeneric(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "nonexistent"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Implementation != "generic" {
		t.Errorf("Implementation=%q want generic", resolved.Implementation)
	}
	if _, ok := resolved.Harness.(*Generic); !ok {
		t.Errorf("expected *Generic, got %T", resolved.Harness)
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

func TestResolve_LegacyBuiltinOpencode(t *testing.T) {
	home := t.TempDir()
	configsDir := filepath.Join(home, ".scion", "harness-configs")
	hcDir := filepath.Join(configsDir, "opencode")

	// Legacy opencode config with provisioner.type: builtin — now treated as
	// container-script since provisioner.type is implicit.
	writeFile(t, filepath.Join(hcDir, "config.yaml"), `harness: opencode
image: scion-opencode:latest
user: scion
provisioner:
  type: builtin
  interface_version: 1
command:
  base: ["opencode"]
`)
	writeFile(t, filepath.Join(hcDir, "provision.py"), "#!/usr/bin/env python3\n")

	t.Setenv("HOME", home)

	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "opencode"})
	if err != nil {
		t.Fatalf("Resolve should not error for legacy-builtin config: %v", err)
	}
	if resolved.Implementation != "container-script" {
		t.Errorf("Implementation=%q want container-script", resolved.Implementation)
	}
	if _, ok := resolved.Harness.(*ContainerScriptHarness); !ok {
		t.Errorf("expected ContainerScriptHarness, got %T", resolved.Harness)
	}
}

func TestResolve_LegacyBuiltinCodexNoDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// No on-disk directory at all — should fall to Generic without error.
	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "codex"})
	if err != nil {
		t.Fatalf("Resolve should not error for missing codex: %v", err)
	}
	if resolved.Implementation != "generic" {
		t.Errorf("Implementation=%q want generic", resolved.Implementation)
	}
}

// TestDiscoverExistingSecretFiles verifies that discoverExistingSecretFiles
// returns two maps (env-type and file-type) of secret name to container-relative
// path for non-empty files in the secrets directory, correctly categorised via
// the harness auth config's required_files declarations.
func TestDiscoverExistingSecretFiles(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()

	// No secrets dir yet — should return nil, nil
	envSecrets, fileSecrets := h.discoverExistingSecretFiles(agentHome)
	if envSecrets != nil {
		t.Errorf("expected nil env secrets for missing secrets dir, got %v", envSecrets)
	}
	if fileSecrets != nil {
		t.Errorf("expected nil file secrets for missing secrets dir, got %v", fileSecrets)
	}

	// Set up auth config with a file-type secret so categorisation can be tested.
	h.entry.Auth = &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"claude": {
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "CLAUDE_AUTH"},
				},
			},
		},
	}

	// Create secrets dir with env-type, file-type, and empty files
	secretDir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	if err := os.MkdirAll(secretDir, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(secretDir, "GOOGLE_CLOUD_PROJECT"), []byte("my-project"), 0600)
	_ = os.WriteFile(filepath.Join(secretDir, "GOOGLE_CLOUD_REGION"), []byte("us-central1"), 0600)
	_ = os.WriteFile(filepath.Join(secretDir, "CLAUDE_AUTH"), []byte("auth-token"), 0600)
	_ = os.WriteFile(filepath.Join(secretDir, "EMPTY_SECRET"), []byte(""), 0600) // empty — should be skipped

	envSecrets, fileSecrets = h.discoverExistingSecretFiles(agentHome)

	// Env-type: GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION
	if len(envSecrets) != 2 {
		t.Fatalf("expected 2 env secrets, got %d: %v", len(envSecrets), envSecrets)
	}
	if envSecrets["GOOGLE_CLOUD_PROJECT"] != "$HOME/.scion/harness/secrets/GOOGLE_CLOUD_PROJECT" {
		t.Errorf("GOOGLE_CLOUD_PROJECT path = %q", envSecrets["GOOGLE_CLOUD_PROJECT"])
	}
	if envSecrets["GOOGLE_CLOUD_REGION"] != "$HOME/.scion/harness/secrets/GOOGLE_CLOUD_REGION" {
		t.Errorf("GOOGLE_CLOUD_REGION path = %q", envSecrets["GOOGLE_CLOUD_REGION"])
	}

	// File-type: CLAUDE_AUTH (matches required_files in auth config)
	if len(fileSecrets) != 1 {
		t.Fatalf("expected 1 file secret, got %d: %v", len(fileSecrets), fileSecrets)
	}
	if fileSecrets["CLAUDE_AUTH"] != "$HOME/.scion/harness/secrets/CLAUDE_AUTH" {
		t.Errorf("CLAUDE_AUTH path = %q", fileSecrets["CLAUDE_AUTH"])
	}

	// Empty secret should not appear in either map
	if _, ok := envSecrets["EMPTY_SECRET"]; ok {
		t.Error("empty secret file should not be included in env secrets")
	}
	if _, ok := fileSecrets["EMPTY_SECRET"]; ok {
		t.Error("empty secret file should not be included in file secrets")
	}
}

func TestContainerScriptHarness_ResolveAuth_SelectedTypeFiltersFiles(t *testing.T) {
	// When a specific auth type is selected, ResolveAuth must only include
	// file mappings from that auth type — not from all types. This prevents
	// ValidateAuth from failing with "credential file does not exist" for
	// files belonging to unselected auth types (issue #1241).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "harness: testharness\nimage: scion-test:latest\n")
	writeFile(t, filepath.Join(dir, "provision.py"), "#!/usr/bin/env python3\nimport sys\nsys.exit(0)\n")

	entry := config.HarnessConfigEntry{
		Harness: "testharness",
		Image:   "scion-test:latest",
		Provisioner: &config.HarnessProvisionerConfig{
			Type:             "container-script",
			InterfaceVersion: 1,
			Command:          []string{"python3", "$HOME/.scion/harness/provision.py"},
			Timeout:          "10s",
		},
		Auth: &config.HarnessAuthMetadata{
			Types: map[string]config.HarnessAuthTypeMetadata{
				"vertex-ai": {
					// vertex-ai has no required_files
				},
				"auth-file": {
					RequiredFiles: []config.HarnessAuthFileRequirement{
						{
							Name:         "GROK_AUTH",
							Type:         "file",
							TargetSuffix: "/.grok/auth.json",
							Field:        "GrokAuthFile",
						},
					},
				},
			},
		},
	}
	h, err := NewContainerScriptHarness(dir, entry)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	t.Run("selected_type_excludes_other_files", func(t *testing.T) {
		resolved, err := h.ResolveAuth(api.AuthConfig{
			SelectedType: "vertex-ai",
			Files:        map[string]string{"GrokAuthFile": "/tmp/fake-auth.json"},
		})
		if err != nil {
			t.Fatalf("ResolveAuth: %v", err)
		}
		// With vertex-ai selected, the auth-file's GrokAuthFile mapping
		// must NOT appear in resolved.Files.
		for _, fm := range resolved.Files {
			if fm.ContainerPath == "~/.grok/auth.json" {
				t.Errorf("resolved.Files contains auth-file mapping %v, but vertex-ai was selected", fm)
			}
		}
	})

	t.Run("auto_detect_includes_all_files", func(t *testing.T) {
		resolved, err := h.ResolveAuth(api.AuthConfig{
			SelectedType: "", // auto-detect
			Files:        map[string]string{"GrokAuthFile": "/tmp/fake-auth.json"},
		})
		if err != nil {
			t.Fatalf("ResolveAuth: %v", err)
		}
		// With no selected type, all auth types' file mappings should be
		// included — the provisioner will decide which to use.
		found := false
		for _, fm := range resolved.Files {
			if fm.ContainerPath == "~/.grok/auth.json" {
				found = true
				break
			}
		}
		if !found {
			t.Error("resolved.Files should contain auth-file mapping in auto-detect mode")
		}
	})

	t.Run("unrecognized_selected_type_falls_back_to_all", func(t *testing.T) {
		resolved, err := h.ResolveAuth(api.AuthConfig{
			SelectedType: "unknown-type",
			Files:        map[string]string{"GrokAuthFile": "/tmp/fake-auth.json"},
		})
		if err != nil {
			t.Fatalf("ResolveAuth: %v", err)
		}
		// Unrecognized type should gracefully fall back to all types.
		found := false
		for _, fm := range resolved.Files {
			if fm.ContainerPath == "~/.grok/auth.json" {
				found = true
				break
			}
		}
		if !found {
			t.Error("resolved.Files should contain auth-file mapping for unrecognized selected type (graceful fallback)")
		}
	})
}
