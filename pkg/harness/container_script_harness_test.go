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
