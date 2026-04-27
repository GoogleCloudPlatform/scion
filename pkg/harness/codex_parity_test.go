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

// seedCodexDir seeds the embedded Codex harness-config into a temp dir using
// the same code path operators run during scion init / harness-config upgrade.
func seedCodexDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := config.SeedHarnessConfig(dir, &Codex{}, false); err != nil {
		t.Fatalf("SeedHarnessConfig: %v", err)
	}
	return dir
}

// TestCodexEmbedsSeedRootSupportFiles verifies provision.py and config.toml
// land in the right places. provision.py is a root-level support file (Phase 1
// allowlist); config.toml is a harness-native settings file under home/.codex/.
func TestCodexEmbedsSeedRootSupportFiles(t *testing.T) {
	dir := seedCodexDir(t)

	provPath := filepath.Join(dir, "provision.py")
	if _, err := os.Stat(provPath); err != nil {
		t.Fatalf("expected provision.py at harness-config root: %v", err)
	}

	configToml := filepath.Join(dir, "home", ".codex", "config.toml")
	if _, err := os.Stat(configToml); err != nil {
		t.Fatalf("expected config.toml under home/.codex/: %v", err)
	}

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

// TestCodexActivateScriptFlipsProvisionerType is the operator-facing
// activation step: --activate-script flips type to container-script and
// produces a backup of the previous config.yaml.
func TestCodexActivateScriptFlipsProvisionerType(t *testing.T) {
	dir := seedCodexDir(t)

	plan, err := config.UpgradeHarnessConfig(dir, &Codex{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
		Now:            func() time.Time { return time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC) },
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

// TestCodexContainerScriptHarnessParity covers Name/DefaultConfigDir/SkillsDir/
// InterruptKey/AdvancedCapabilities. GetCommand parity has its own test
// because Codex's resume_flag is the first multi-token flag (`resume --last`)
// — single-token assertions wouldn't catch the split-on-whitespace gap.
func TestCodexContainerScriptHarnessParity(t *testing.T) {
	dir := seedCodexDir(t)
	if _, err := config.UpgradeHarnessConfig(dir, &Codex{}, config.HarnessConfigUpgradeOptions{
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
	builtin := &Codex{}

	if scripted.Name() != builtin.Name() {
		t.Errorf("Name: scripted=%q builtin=%q", scripted.Name(), builtin.Name())
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

	gotCaps := scripted.AdvancedCapabilities()
	wantCaps := builtin.AdvancedCapabilities()
	if gotCaps.Harness != wantCaps.Harness {
		t.Errorf("Capabilities.Harness: scripted=%q builtin=%q", gotCaps.Harness, wantCaps.Harness)
	}
	if gotCaps.Telemetry.NativeEmitter.Support != wantCaps.Telemetry.NativeEmitter.Support {
		t.Errorf("Capabilities.Telemetry.NativeEmitter: scripted=%v builtin=%v", gotCaps.Telemetry.NativeEmitter, wantCaps.Telemetry.NativeEmitter)
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

// TestCodexContainerScriptGetCommandParity exercises the three operative
// command shapes. The resume case is the new ground that Phase 5 adds: Codex's
// "resume --last" is two argv tokens, so a missing whitespace-split in the
// container-script GetCommand would silently produce a single bogus arg.
func TestCodexContainerScriptGetCommandParity(t *testing.T) {
	dir := seedCodexDir(t)
	if _, err := config.UpgradeHarnessConfig(dir, &Codex{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatal(err)
	}
	builtin := &Codex{}

	cases := []struct {
		name    string
		task    string
		resume  bool
		baseArg []string
	}{
		{"resume_no_task", "", true, nil},
		{"task_only", "fix the bug", false, nil},
		{"task_with_base_args", "do it", false, []string{"--debug"}},
		{"no_task_with_base_args", "", false, []string{"--debug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotS := scripted.GetCommand(tc.task, tc.resume, tc.baseArg)
			gotB := builtin.GetCommand(tc.task, tc.resume, tc.baseArg)
			if strings.Join(gotS, "|") != strings.Join(gotB, "|") {
				t.Errorf("scripted=%v builtin=%v", gotS, gotB)
			}
		})
	}
}

// TestCodexContainerScriptHarnessStagesScript verifies Provision() stages
// provision.py byte-identically and emits the trusted hook wrapper.
func TestCodexContainerScriptHarnessStagesScript(t *testing.T) {
	dir := seedCodexDir(t)
	if _, err := config.UpgradeHarnessConfig(dir, &Codex{}, config.HarnessConfigUpgradeOptions{
		ActivateScript: true,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatal(err)
	}

	agentHome := t.TempDir()
	if err := scripted.Provision(context.Background(), "researcher", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bundle := filepath.Join(agentHome, ".scion", "harness")
	stagedScript := filepath.Join(bundle, "provision.py")
	stagedBytes, err := os.ReadFile(stagedScript)
	if err != nil {
		t.Fatalf("provision.py not staged: %v", err)
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
		t.Errorf("wrapper missing expected command: %s", wrapperBytes)
	}
}

// TestCodexContainerScriptApplyAuthSettingsStagesSecretFiles is new for Phase
// 5: Codex needs the API key VALUE inside the container script, but
// sciontool harness provision strips secret env vars. The host stages each
// resolved env value as a 0600 file under .scion/harness/secrets/<NAME> and
// records the path in auth-candidates.json's env_secret_files map.
func TestCodexContainerScriptApplyAuthSettingsStagesSecretFiles(t *testing.T) {
	dir := seedCodexDir(t)
	if _, err := config.UpgradeHarnessConfig(dir, &Codex{}, config.HarnessConfigUpgradeOptions{
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

	agentHome := t.TempDir()
	resolved := &api.ResolvedAuth{
		Method: "container-script",
		EnvVars: map[string]string{
			"CODEX_API_KEY": "codex-test-secret-value",
			"INVALID-KEY":   "should-be-skipped",
		},
	}
	if err := scripted.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings: %v", err)
	}

	secretPath := filepath.Join(agentHome, ".scion", "harness", "secrets", "CODEX_API_KEY")
	data, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("secret file missing: %v", err)
	}
	if string(data) != "codex-test-secret-value" {
		t.Errorf("secret value = %q, want %q", data, "codex-test-secret-value")
	}
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("secret file perm = %o, want 0600", perm)
	}

	// Invalid env names must not write a file (defends against caller-supplied
	// "../../etc/passwd" style names).
	if _, err := os.Stat(filepath.Join(agentHome, ".scion", "harness", "secrets", "INVALID-KEY")); !os.IsNotExist(err) {
		t.Errorf("INVALID-KEY should not produce a secret file")
	}

	candPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json")
	candBytes, err := os.ReadFile(candPath)
	if err != nil {
		t.Fatalf("auth-candidates.json missing: %v", err)
	}
	var cand map[string]any
	if err := json.Unmarshal(candBytes, &cand); err != nil {
		t.Fatalf("auth-candidates.json invalid: %v", err)
	}
	envSecretFiles, ok := cand["env_secret_files"].(map[string]any)
	if !ok {
		t.Fatalf("env_secret_files missing or wrong type: %T", cand["env_secret_files"])
	}
	if envSecretFiles["CODEX_API_KEY"] != "$HOME/.scion/harness/secrets/CODEX_API_KEY" {
		t.Errorf("env_secret_files[CODEX_API_KEY]=%v want $HOME-prefixed container path", envSecretFiles["CODEX_API_KEY"])
	}
	// The auth-candidates JSON itself must NOT carry the secret value — that
	// file is mode 0644 and would leak through normal log/diff tooling.
	if strings.Contains(string(candBytes), "codex-test-secret-value") {
		t.Errorf("auth-candidates.json must not embed the secret value: %s", candBytes)
	}
}

// TestCodexProvisionScript_Integration_APIKey runs the actual Python script
// against a synthetic manifest with a CODEX_API_KEY secret staged and
// verifies that .codex/auth.json is written in the format Codex expects.
func TestCodexProvisionScript_Integration_APIKey(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	dir := seedCodexDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	for _, sub := range []string{"inputs", "outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Stage the secret VALUE file the way ApplyAuthSettings would.
	secretValue := "sk-codex-test-12345"
	if err := os.WriteFile(filepath.Join(bundle, "secrets", "CODEX_API_KEY"), []byte(secretValue), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "codex"},
		"inputs":             map[string]any{},
		"outputs": map[string]any{
			"env":           filepath.Join(bundle, "outputs", "env.json"),
			"resolved_auth": filepath.Join(bundle, "outputs", "resolved-auth.json"),
		},
		"platform": map[string]any{"goos": "linux"},
	}
	manifestPath := filepath.Join(bundle, "manifest.json")
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Auth candidates: explicit api-key, CODEX_API_KEY available with secret file.
	candidates := map[string]any{
		"schema_version":  1,
		"explicit_type":   "",
		"resolved_method": "container-script",
		"env_vars":        []string{"CODEX_API_KEY"},
		"env_secret_files": map[string]string{
			"CODEX_API_KEY": filepath.Join(bundle, "secrets", "CODEX_API_KEY"),
		},
		"files": []any{},
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

	// Verify .codex/auth.json was written with the API key value.
	authPath := filepath.Join(home, ".codex", "auth.json")
	authBytes, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("auth.json missing: %v\nscript output: %s", err, out)
	}
	var auth map[string]string
	if err := json.Unmarshal(authBytes, &auth); err != nil {
		t.Fatalf("auth.json invalid: %v", err)
	}
	if auth["auth_mode"] != "apikey" {
		t.Errorf("auth_mode=%q want apikey", auth["auth_mode"])
	}
	// Compiled harness writes OPENAI_API_KEY regardless of source — match parity.
	if auth["OPENAI_API_KEY"] != secretValue {
		t.Errorf("OPENAI_API_KEY=%q want %q", auth["OPENAI_API_KEY"], secretValue)
	}
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("auth.json perm=%o want 0600", perm)
	}

	resolvedBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "resolved-auth.json"))
	if err != nil {
		t.Fatalf("resolved-auth.json missing: %v", err)
	}
	var resolved map[string]any
	if err := json.Unmarshal(resolvedBytes, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved["method"] != "api-key" {
		t.Errorf("method=%v want api-key", resolved["method"])
	}
	if resolved["env_var"] != "CODEX_API_KEY" {
		t.Errorf("env_var=%v want CODEX_API_KEY", resolved["env_var"])
	}
	// Defense-in-depth: resolved-auth.json must NOT contain the secret value.
	if strings.Contains(string(resolvedBytes), secretValue) {
		t.Errorf("resolved-auth.json leaked secret value: %s", resolvedBytes)
	}
}

// TestCodexProvisionScript_Integration_TelemetryEnabled exercises the TOML
// reconciliation path. We seed a config.toml with a custom key that must be
// preserved, then verify the [otel] block is added with the resolved
// endpoint/headers/log_user_prompt.
func TestCodexProvisionScript_Integration_TelemetryEnabled(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	dir := seedCodexDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	codexDir := filepath.Join(home, ".codex")
	for _, sub := range []string{"inputs", "outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing config with a custom key the user authored; the script
	// must preserve it while updating [otel].
	initialToml := `approval_policy = "never"
custom_key = "keep-me"

[projects."/workspace"]
trust_level = "trusted"
`
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(initialToml), 0644); err != nil {
		t.Fatal(err)
	}

	// Stage an api-key secret so the script's auth path also runs cleanly.
	if err := os.WriteFile(filepath.Join(bundle, "secrets", "OPENAI_API_KEY"), []byte("sk-test"), 0600); err != nil {
		t.Fatal(err)
	}

	telemetryPayload := map[string]any{
		"schema_version": 1,
		"telemetry": map[string]any{
			"enabled": true,
			"cloud": map[string]any{
				"endpoint": "collector.example.com:4317",
				"protocol": "grpc",
				"headers":  map[string]string{"x-api-key": "test123"},
			},
		},
	}
	telBytes, _ := json.MarshalIndent(telemetryPayload, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "telemetry.json"), telBytes, 0644); err != nil {
		t.Fatal(err)
	}

	candidates := map[string]any{
		"schema_version": 1,
		"env_vars":       []string{"OPENAI_API_KEY"},
		"env_secret_files": map[string]string{
			"OPENAI_API_KEY": filepath.Join(bundle, "secrets", "OPENAI_API_KEY"),
		},
		"files": []any{},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "codex"},
		"inputs":             map[string]any{},
		"outputs": map[string]any{
			"env":           filepath.Join(bundle, "outputs", "env.json"),
			"resolved_auth": filepath.Join(bundle, "outputs", "resolved-auth.json"),
		},
	}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	manifestPath := filepath.Join(bundle, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\noutput: %s", err, out)
	}

	tomlBytes, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tomlStr := string(tomlBytes)
	for _, want := range []string{
		`custom_key = "keep-me"`,
		`[otel]`,
		`enabled = true`,
		`log_user_prompt = false`,
		`exporter = { otlp-grpc = {`,
		`endpoint = "collector.example.com:4317"`,
		`headers = { "x-api-key" = "test123" }`,
	} {
		if !strings.Contains(tomlStr, want) {
			t.Errorf("config.toml missing %q\ngot:\n%s", want, tomlStr)
		}
	}
}

// TestCodexProvisionScript_Integration_TelemetryDisabled verifies the [otel]
// section is stripped when telemetry is disabled, even if the seeded TOML had
// one. This matches the compiled harness's reconcileConfig behavior.
func TestCodexProvisionScript_Integration_TelemetryDisabled(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	dir := seedCodexDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	codexDir := filepath.Join(home, ".codex")
	for _, sub := range []string{"inputs", "outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Seed a config that already has [otel] — we expect it to be stripped.
	initialToml := `approval_policy = "never"

[otel]
enabled = false
exporter = { otlp-grpc = {
  endpoint = "localhost:4317"
}}
`
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(initialToml), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "secrets", "OPENAI_API_KEY"), []byte("sk-test"), 0600); err != nil {
		t.Fatal(err)
	}

	telemetryPayload := map[string]any{
		"schema_version": 1,
		"telemetry": map[string]any{
			"enabled": false,
		},
	}
	telBytes, _ := json.Marshal(telemetryPayload)
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "telemetry.json"), telBytes, 0644); err != nil {
		t.Fatal(err)
	}
	candidates := map[string]any{
		"env_vars": []string{"OPENAI_API_KEY"},
		"env_secret_files": map[string]string{
			"OPENAI_API_KEY": filepath.Join(bundle, "secrets", "OPENAI_API_KEY"),
		},
	}
	candBytes, _ := json.Marshal(candidates)
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "codex"},
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

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\noutput: %s", err, out)
	}

	tomlBytes, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tomlStr := string(tomlBytes)
	if strings.Contains(tomlStr, "[otel]") {
		t.Errorf("config.toml still contains [otel] when telemetry is disabled:\n%s", tomlStr)
	}
	if !strings.Contains(tomlStr, `approval_policy = "never"`) {
		t.Errorf("config.toml lost the user's approval_policy line:\n%s", tomlStr)
	}
}

// TestCodexProvisionScript_Integration_LogUserPromptFromFilter exercises the
// telemetry filter precedence (exclude beats include), matching the compiled
// harness's behavior in TestCodexApplyTelemetrySettings_LogUserPromptFromFilter.
func TestCodexProvisionScript_Integration_LogUserPromptFromFilter(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	dir := seedCodexDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	runOnce := func(t *testing.T, filter map[string]any, wantLogUserPrompt string) {
		t.Helper()
		home := t.TempDir()
		bundle := filepath.Join(home, ".scion", "harness")
		codexDir := filepath.Join(home, ".codex")
		for _, sub := range []string{"inputs", "outputs", "secrets"} {
			if err := os.MkdirAll(filepath.Join(bundle, sub), 0755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(codexDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, "secrets", "OPENAI_API_KEY"), []byte("sk-test"), 0600); err != nil {
			t.Fatal(err)
		}

		telemetryPayload := map[string]any{
			"schema_version": 1,
			"telemetry": map[string]any{
				"enabled": true,
				"cloud": map[string]any{
					"endpoint": "collector.example.com:4317",
					"protocol": "grpc",
				},
				"filter": map[string]any{
					"events": filter,
				},
			},
		}
		telBytes, _ := json.Marshal(telemetryPayload)
		if err := os.WriteFile(filepath.Join(bundle, "inputs", "telemetry.json"), telBytes, 0644); err != nil {
			t.Fatal(err)
		}
		candidates := map[string]any{
			"env_vars": []string{"OPENAI_API_KEY"},
			"env_secret_files": map[string]string{
				"OPENAI_API_KEY": filepath.Join(bundle, "secrets", "OPENAI_API_KEY"),
			},
		}
		candBytes, _ := json.Marshal(candidates)
		if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
			t.Fatal(err)
		}
		manifest := map[string]any{
			"schema_version":     1,
			"command":            "provision",
			"agent_name":         "test-agent",
			"agent_home":         home,
			"agent_workspace":    "/workspace",
			"harness_bundle_dir": bundle,
			"harness_config":     map[string]any{"harness": "codex"},
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

		cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\noutput: %s", err, out)
		}
		tomlBytes, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(tomlBytes), "log_user_prompt = "+wantLogUserPrompt) {
			t.Errorf("expected log_user_prompt = %s, got:\n%s", wantLogUserPrompt, tomlBytes)
		}
	}

	t.Run("include_only_enables", func(t *testing.T) {
		runOnce(t, map[string]any{"include": []string{"agent.user.prompt"}}, "true")
	})
	t.Run("exclude_overrides_include", func(t *testing.T) {
		runOnce(t, map[string]any{
			"include": []string{"agent.user.prompt"},
			"exclude": []string{"agent.user.prompt"},
		}, "false")
	})
}

// TestCodexProvisionScript_Integration_NoCreds asserts the script exits
// non-zero with an actionable message when no auth is staged. Mirrors the
// compiled harness's pre-launch failure mode and matches the OpenCode parity
// test's no-creds case.
func TestCodexProvisionScript_Integration_NoCreds(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	dir := seedCodexDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	for _, sub := range []string{"inputs", "outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "codex"},
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
	candidates := map[string]any{"env_vars": []string{}, "files": []any{}}
	candBytes, _ := json.Marshal(candidates)
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success: %s", out)
	}
	if !strings.Contains(string(out), "no valid auth method") {
		t.Errorf("expected actionable no-creds message, got: %s", out)
	}
}
