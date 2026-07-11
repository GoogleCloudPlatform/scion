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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

// seedClaudeDir seeds the embedded Claude harness-config into a temp dir
// from the harnesses/ embed FS. It returns the absolute target dir so tests
// can inspect it.
func seedClaudeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := config.SeedHarnessConfigFromDir(dir, harnessesEmbed.FS, "claude", false); err != nil {
		t.Fatalf("SeedHarnessConfigFromDir: %v", err)
	}
	return dir
}

// TestClaudeEmbedsSeedRootSupportFiles verifies the new provision.py and
// the existing .claude.json land where they should: provision.py at the
// harness-config root, .claude.json under home/.
func TestClaudeEmbedsSeedRootSupportFiles(t *testing.T) {
	dir := seedClaudeDir(t)

	// provision.py is a root-level support file.
	provPath := filepath.Join(dir, "provision.py")
	if _, err := os.Stat(provPath); err != nil {
		t.Fatalf("expected provision.py at harness-config root: %v", err)
	}

	// .claude.json is the harness-native settings file; it lives under home.
	claudeJSON := filepath.Join(dir, "home", ".claude.json")
	if _, err := os.Stat(claudeJSON); err != nil {
		t.Fatalf("expected .claude.json under home/: %v", err)
	}

	// config.yaml at the root must be valid and declare the container-script
	// provisioner so the in-container provision.py runs during pre-start.
	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	if hc.Config.Provisioner == nil {
		t.Fatal("expected provisioner block in seeded config.yaml")
	}
	if hc.Config.Provisioner.Type != "container-script" {
		t.Errorf("provisioner.type=%q want container-script", hc.Config.Provisioner.Type)
	}
	if len(hc.Config.Provisioner.Command) == 0 {
		t.Error("expected provisioner.command in config.yaml")
	}
}

// TestClaudeContainerScriptHarnessStagesScript verifies Provision() copies
// the seeded provision.py into the agent bundle and writes a wrapper that
// targets sciontool harness provision.
func TestClaudeContainerScriptHarnessStagesScript(t *testing.T) {
	dir := seedClaudeDir(t)

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

	// The staged script must be byte-identical to the source.
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

// TestClaudeContainerScriptReconcilesMissingBundle verifies that calling
// Provision() on an agent home that lacks the container-script bundle stages
// the hook wrapper, provision.py, and manifest.
func TestClaudeContainerScriptReconcilesMissingBundle(t *testing.T) {
	dir := seedClaudeDir(t)

	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()

	// Simulate an agent home created by the old builtin harness path:
	// the config dir exists but there is no .scion/harness/ bundle.
	configDir := filepath.Join(agentHome, ".claude")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, ".claude.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Confirm the hook wrapper does NOT exist yet.
	hookWrapper := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "20-harness-provision")
	if _, err := os.Stat(hookWrapper); err == nil {
		t.Fatal("hook wrapper should not exist before reconciliation")
	}

	// Call Provision (the reconciliation path).
	if err := scripted.Provision(context.Background(), "migrated-agent", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision (reconciliation): %v", err)
	}

	// Hook wrapper must now exist.
	wrapperBytes, err := os.ReadFile(hookWrapper)
	if err != nil {
		t.Fatalf("hook wrapper not staged after reconciliation: %v", err)
	}
	if !strings.Contains(string(wrapperBytes), "sciontool harness provision") {
		t.Errorf("wrapper does not invoke sciontool harness provision: %s", wrapperBytes)
	}

	// provision.py must be staged.
	if _, err := os.Stat(filepath.Join(agentHome, ".scion", "harness", "provision.py")); err != nil {
		t.Errorf("provision.py not staged: %v", err)
	}

	// manifest.json must be present.
	if _, err := os.Stat(filepath.Join(agentHome, ".scion", "harness", "manifest.json")); err != nil {
		t.Errorf("manifest.json not staged: %v", err)
	}

	// Pre-existing .claude.json must be preserved.
	if _, err := os.Stat(filepath.Join(agentHome, ".claude.json")); err != nil {
		t.Errorf("pre-existing .claude.json was removed: %v", err)
	}
}

// TestClaudeProvisionScript_Integration_HappyPath runs the actual Python
// script against a synthetic manifest and validates outputs.
func TestClaudeProvisionScript_Integration_HappyPath(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedClaudeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "secrets"), 0700); err != nil {
		t.Fatal(err)
	}

	// Seed a .claude.json for the script to update.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "claude"},
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
		"schema_version":   1,
		"explicit_type":    "",
		"resolved_method":  "container-script",
		"env_vars":         []string{"ANTHROPIC_API_KEY"},
		"env_secret_files": map[string]string{},
		"files":            []any{},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision script failed: %v\noutput: %s", err, out)
	}

	// Verify resolved-auth.json
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
	if resolved["env_var"] != "ANTHROPIC_API_KEY" {
		t.Errorf("env_var=%v want ANTHROPIC_API_KEY", resolved["env_var"])
	}

	// Verify env.json contains the auth env var overlay.
	envBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "env.json"))
	if err != nil {
		t.Fatalf("env.json missing: %v", err)
	}
	var envOverlay map[string]any
	if err := json.Unmarshal(envBytes, &envOverlay); err != nil {
		t.Fatalf("env.json invalid: %v", err)
	}
	if envOverlay["ANTHROPIC_API_KEY"] != "${ANTHROPIC_API_KEY}" {
		t.Errorf("env.json ANTHROPIC_API_KEY=%v want ${ANTHROPIC_API_KEY}", envOverlay["ANTHROPIC_API_KEY"])
	}

	// Verify .claude.json was updated with project paths.
	claudeData, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf(".claude.json missing: %v", err)
	}
	var claudeCfg map[string]any
	if err := json.Unmarshal(claudeData, &claudeCfg); err != nil {
		t.Fatalf(".claude.json invalid: %v", err)
	}
	projects, ok := claudeCfg["projects"].(map[string]any)
	if !ok {
		t.Fatal("projects not found in .claude.json")
	}
	if _, ok := projects["/workspace"]; !ok {
		t.Errorf("expected project entry for /workspace, got keys: %v", func() []string {
			keys := make([]string, 0, len(projects))
			for k := range projects {
				keys = append(keys, k)
			}
			return keys
		}())
	}
}

// TestClaudeProvisionScript_Integration_NoCreds asserts the script exits
// non-zero with an actionable message when nothing is staged.
func TestClaudeProvisionScript_Integration_NoCreds(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedClaudeDir(t)
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
		"harness_config":     map[string]any{"harness": "claude"},
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
	cmd.Env = append(os.Environ(), "HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success. output: %s", out)
	}
	if !strings.Contains(string(out), "no valid auth method") {
		t.Errorf("expected actionable no-creds message, got: %s", out)
	}
}

// TestClaudeProvisionScript_Integration_APIKeyApproval runs the Python
// script with a staged API key secret and verifies the customApiKeyResponses
// fingerprint is written into .claude.json.
func TestClaudeProvisionScript_Integration_APIKeyApproval(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedClaudeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(bundle, "secrets")
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Seed .claude.json.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Stage a secret file with the API key value.
	apiKey := "sk-ant-api03-ABCDEFGHIJ1234567890abcdefghij"
	if err := os.WriteFile(filepath.Join(secretsDir, "ANTHROPIC_API_KEY"), []byte(apiKey), 0600); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "claude"},
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
		"env_vars":        []string{"ANTHROPIC_API_KEY"},
		"env_secret_files": map[string]string{
			"ANTHROPIC_API_KEY": filepath.Join(secretsDir, "ANTHROPIC_API_KEY"),
		},
		"files": []any{},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision script failed: %v\noutput: %s", err, out)
	}

	// Verify .claude.json has customApiKeyResponses with the fingerprint.
	claudeData, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf(".claude.json missing: %v", err)
	}
	var claudeCfg map[string]any
	if err := json.Unmarshal(claudeData, &claudeCfg); err != nil {
		t.Fatalf(".claude.json invalid: %v", err)
	}

	responses, ok := claudeCfg["customApiKeyResponses"].(map[string]any)
	if !ok {
		t.Fatal("customApiKeyResponses not found or wrong type")
	}
	approved, ok := responses["approved"].([]any)
	if !ok || len(approved) != 1 {
		t.Fatalf("expected 1 approved entry, got %v", responses["approved"])
	}
	// Last 20 chars of the key.
	wantFingerprint := apiKey[len(apiKey)-20:]
	if approved[0] != wantFingerprint {
		t.Errorf("approved fingerprint = %q, want %q", approved[0], wantFingerprint)
	}
}

// TestClaudeProvisionScript_Integration_VertexAI verifies the script produces
// the correct env overlay for Vertex AI auth.
func TestClaudeProvisionScript_Integration_VertexAI(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedClaudeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}

	// Seed .claude.json and create ADC file so vertex-ai detection works.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	adcDir := filepath.Join(home, ".config", "gcloud")
	if err := os.MkdirAll(adcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adcDir, "application_default_credentials.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "claude"},
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
		"explicit_type":   "vertex-ai",
		"resolved_method": "container-script",
		"env_vars":        []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"},
		"files": []any{
			map[string]string{
				"container_path": "~/.config/gcloud/application_default_credentials.json",
			},
		},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision script failed: %v\noutput: %s", err, out)
	}

	// Verify resolved-auth.json.
	resolvedBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "resolved-auth.json"))
	if err != nil {
		t.Fatalf("resolved-auth.json missing: %v", err)
	}
	var resolved map[string]any
	if err := json.Unmarshal(resolvedBytes, &resolved); err != nil {
		t.Fatalf("resolved-auth.json invalid: %v", err)
	}
	if resolved["method"] != "vertex-ai" {
		t.Errorf("method=%v want vertex-ai", resolved["method"])
	}

	// Verify env.json has vertex-ai env vars.
	envBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "env.json"))
	if err != nil {
		t.Fatalf("env.json missing: %v", err)
	}
	var envOverlay map[string]any
	if err := json.Unmarshal(envBytes, &envOverlay); err != nil {
		t.Fatalf("env.json invalid: %v", err)
	}
	if envOverlay["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX=%v want 1", envOverlay["CLAUDE_CODE_USE_VERTEX"])
	}
	if envOverlay["ANTHROPIC_VERTEX_PROJECT_ID"] != "${GOOGLE_CLOUD_PROJECT}" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID=%v want ${GOOGLE_CLOUD_PROJECT}", envOverlay["ANTHROPIC_VERTEX_PROJECT_ID"])
	}
	if envOverlay["CLOUD_ML_REGION"] != "${GOOGLE_CLOUD_REGION}" {
		t.Errorf("CLOUD_ML_REGION=%v want ${GOOGLE_CLOUD_REGION}", envOverlay["CLOUD_ML_REGION"])
	}
}

// TestClaudeProvisionScript_Integration_VertexAI_NoADC verifies the script
// auto-detects vertex-ai auth even without a local ADC file — in GCP
// environments (GKE, Cloud Run, Compute Engine) the metadata server provides
// credentials via the attached service account.
func TestClaudeProvisionScript_Integration_VertexAI_NoADC(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedClaudeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}

	// Seed .claude.json but do NOT create ADC file — simulates GCP SA auth.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "claude"},
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

	// No explicit type, no ADC file — just project + region. Auto-detect
	// should resolve to vertex-ai via metadata server fallback.
	candidates := map[string]any{
		"schema_version":  1,
		"explicit_type":   "",
		"resolved_method": "container-script",
		"env_vars":        []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"},
		"files":           []any{},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", manifestPath)
	cmd.Env = append(os.Environ(), "HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision script failed (GCP SA / no ADC): %v\noutput: %s", err, out)
	}

	resolvedBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "resolved-auth.json"))
	if err != nil {
		t.Fatalf("resolved-auth.json missing: %v", err)
	}
	var resolved map[string]any
	if err := json.Unmarshal(resolvedBytes, &resolved); err != nil {
		t.Fatalf("resolved-auth.json invalid: %v", err)
	}
	if resolved["method"] != "vertex-ai" {
		t.Errorf("method=%v want vertex-ai (auto-detected from project+region without ADC)", resolved["method"])
	}

	envBytes, err := os.ReadFile(filepath.Join(bundle, "outputs", "env.json"))
	if err != nil {
		t.Fatalf("env.json missing: %v", err)
	}
	var envOverlay map[string]any
	if err := json.Unmarshal(envBytes, &envOverlay); err != nil {
		t.Fatalf("env.json invalid: %v", err)
	}
	if envOverlay["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX=%v want 1", envOverlay["CLAUDE_CODE_USE_VERTEX"])
	}
}

// TestClaudeProvisionScript_Integration_MCP runs the script with a staged
// mcp-servers.json input and asserts it translates entries into Claude Code's
// native mcpServers shape in .claude.json.
func TestClaudeProvisionScript_Integration_MCP(t *testing.T) {
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; skipping script integration test")
	}

	dir := seedClaudeDir(t)
	scriptPath := filepath.Join(dir, "provision.py")
	// Stage scion_harness.py next to provision.py so the import resolves.
	if err := os.WriteFile(filepath.Join(dir, "scion_harness.py"), SharedHarnessHelperSource(), 0644); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	bundle := filepath.Join(home, ".scion", "harness")
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "outputs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	// Stage the helper into the bundle too (mirrors production).
	if err := os.WriteFile(filepath.Join(bundle, "scion_harness.py"), SharedHarnessHelperSource(), 0644); err != nil {
		t.Fatal(err)
	}

	// Seed .claude.json with an existing project entry.
	claudeJSON := map[string]any{
		"projects": map[string]any{
			"/workspace": map[string]any{
				"allowedTools":               []any{},
				"mcpServers":                 map[string]any{},
				"hasTrustDialogAccepted":     true,
				"projectOnboardingSeenCount": 1,
			},
		},
	}
	claudeBytes, _ := json.MarshalIndent(claudeJSON, "", "  ")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), claudeBytes, 0644); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"schema_version":     1,
		"command":            "provision",
		"agent_name":         "test-agent",
		"agent_home":         home,
		"agent_workspace":    "/workspace",
		"harness_bundle_dir": bundle,
		"harness_config":     map[string]any{"harness": "claude"},
		"inputs":             map[string]any{},
		"outputs": map[string]any{
			"env":           filepath.Join(bundle, "outputs", "env.json"),
			"resolved_auth": filepath.Join(bundle, "outputs", "resolved-auth.json"),
		},
		"platform": map[string]any{"goos": "linux", "goarch": "amd64"},
	}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), manifestBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Auth candidates so auth phase succeeds.
	candidates := map[string]any{
		"schema_version":  1,
		"explicit_type":   "",
		"resolved_method": "container-script",
		"env_vars":        []string{"ANTHROPIC_API_KEY"},
		"files":           []any{},
	}
	candBytes, _ := json.MarshalIndent(candidates, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "auth-candidates.json"), candBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Stage MCP servers — exercise stdio and SSE.
	mcp := map[string]any{
		"schema_version": 1,
		"mcp_servers": map[string]any{
			"filesystem": map[string]any{
				"transport": "stdio",
				"command":   "mcp-filesystem",
				"args":      []string{"/workspace"},
				"env":       map[string]string{"DEBUG": "true"},
			},
			"remote_api": map[string]any{
				"transport": "sse",
				"url":       "http://localhost:8080/mcp/sse",
				"headers":   map[string]string{"Authorization": "Bearer xyz"},
			},
		},
	}
	mcpBytes, _ := json.MarshalIndent(mcp, "", "  ")
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "mcp-servers.json"), mcpBytes, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(pyPath, scriptPath, "--manifest", filepath.Join(bundle, "manifest.json"))
	cmd.Env = append(os.Environ(), "HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision script failed: %v\noutput: %s", err, out)
	}

	// Read the updated .claude.json.
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf(".claude.json not readable: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf(".claude.json invalid JSON: %v", err)
	}

	// MCP servers should be merged into mcpServers at the top level.
	mcpBlock, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers block missing or wrong type: %v", cfg["mcpServers"])
	}

	// filesystem: stdio -> stdio with command/args/env.
	fs, ok := mcpBlock["filesystem"].(map[string]any)
	if !ok {
		t.Fatal("filesystem entry missing from mcpServers")
	}
	if fs["type"] != "stdio" {
		t.Errorf("filesystem type=%v want stdio", fs["type"])
	}
	if fs["command"] != "mcp-filesystem" {
		t.Errorf("filesystem command=%v want mcp-filesystem", fs["command"])
	}

	// remote_api: sse with url and headers.
	remote, ok := mcpBlock["remote_api"].(map[string]any)
	if !ok {
		t.Fatal("remote_api entry missing from mcpServers")
	}
	if remote["type"] != "sse" {
		t.Errorf("remote_api type=%v want sse", remote["type"])
	}
	if remote["url"] != "http://localhost:8080/mcp/sse" {
		t.Errorf("remote_api url=%v", remote["url"])
	}

	if !strings.Contains(string(out), "applied 2 mcp server(s)") {
		t.Errorf("expected 'applied 2 mcp server(s)' summary, got: %s", out)
	}
}

// TestClaudeContainerScriptResolveAuthShape verifies the container-script
// ResolveAuth surfaces the values the script will need.
func TestClaudeContainerScriptResolveAuthShape(t *testing.T) {
	dir := seedClaudeDir(t)

	hc, err := config.LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	scripted, err := NewContainerScriptHarness(dir, hc.Config)
	if err != nil {
		t.Fatal(err)
	}

	// Pass both an Anthropic key and an auth file; the container-script
	// wrapper must surface BOTH so the in-container script can choose.
	resolved, err := scripted.ResolveAuth(api.AuthConfig{
		AnthropicAPIKey: "sk-ant-xx",
		ClaudeAuthFile:  "/tmp/credentials.json",
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
	foundClaudeAuthFile := false
	for _, f := range resolved.Files {
		if f.SourcePath == "/tmp/credentials.json" && strings.HasSuffix(f.ContainerPath, ".credentials.json") {
			foundClaudeAuthFile = true
		}
	}
	if !foundClaudeAuthFile {
		t.Errorf("expected Claude auth file in Files mapping, got %#v", resolved.Files)
	}
}
