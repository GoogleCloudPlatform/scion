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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// repoRoot returns the repository root directory. It walks up from the current
// working directory looking for go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := util.RepoRoot()
	if err != nil {
		t.Fatalf("failed to find repo root: %v", err)
	}
	return root
}

// bundlePath returns the absolute path to a harness bundle under harnesses/.
func bundlePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "harnesses", name)
}

// TestBundleInstall_OpenCode validates that the harnesses/opencode/ bundle
// can be installed via the opt-in path and produces the same seeded layout
// as the current Go embeds. This is the Phase A.4 safety net that replaces
// the parity oracle before Decision 3 removes the Go implementations.
func TestBundleInstall_OpenCode(t *testing.T) {
	src := bundlePath(t, "opencode")

	// 1. LoadHarnessConfigDir must succeed — config.yaml parses and validates.
	hc, err := config.LoadHarnessConfigDir(src)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir(%s): %v", src, err)
	}
	if hc.Config.Harness != "opencode" {
		t.Errorf("harness=%q want opencode", hc.Config.Harness)
	}
	if hc.Config.Provisioner == nil || hc.Config.Provisioner.Type != "container-script" {
		t.Fatalf("expected provisioner.type=container-script, got %+v", hc.Config.Provisioner)
	}

	// 2. Simulate install: CopyDir from bundle source to a temp target
	// (mirrors cmd/harness_config_install.go installLocally).
	installDir := filepath.Join(t.TempDir(), "opencode-test")
	if err := util.CopyDir(src, installDir); err != nil {
		t.Fatalf("CopyDir (install): %v", err)
	}

	// Installed dir must also load and validate.
	installedHC, err := config.LoadHarnessConfigDir(installDir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir (installed): %v", err)
	}
	if installedHC.Config.Harness != "opencode" {
		t.Errorf("installed harness=%q want opencode", installedHC.Config.Harness)
	}

	// 3. Assert the home/ file layout — these are the golden paths that must
	// match the implicit mapEmbedFileToHomePath placement.
	wantHomeFiles := []string{
		"home/.config/opencode/opencode.json",
	}
	for _, rel := range wantHomeFiles {
		full := filepath.Join(installDir, filepath.FromSlash(rel))
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s in installed bundle: %v", rel, err)
		}
	}

	// 4. Assert bundle root files.
	for _, name := range []string{"config.yaml", "provision.py"} {
		if _, err := os.Stat(filepath.Join(installDir, name)); err != nil {
			t.Errorf("expected %s at bundle root: %v", name, err)
		}
	}

	// 5. Provision from the installed bundle — verify staging produces the
	// expected bundle structure in agent home.
	scripted, err := NewContainerScriptHarness(installDir, installedHC.Config)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	if err := scripted.Provision(context.Background(), "test-agent", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bundle := filepath.Join(agentHome, ".scion", "harness")
	for _, name := range []string{"provision.py", "config.yaml", "manifest.json", "scion_harness.py"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Errorf("expected %s in staged bundle: %v", name, err)
		}
	}
	hookWrapper := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "20-harness-provision")
	wrapperBytes, err := os.ReadFile(hookWrapper)
	if err != nil {
		t.Fatalf("hook wrapper missing after provision: %v", err)
	}
	if !strings.Contains(string(wrapperBytes), "sciontool harness provision") {
		t.Errorf("hook wrapper does not invoke sciontool harness provision")
	}
}

// TestBundleInstall_Codex validates the harnesses/codex/ bundle install path
// and seeded layout parity with the Go embeds.
func TestBundleInstall_Codex(t *testing.T) {
	src := bundlePath(t, "codex")

	// 1. LoadHarnessConfigDir must succeed.
	hc, err := config.LoadHarnessConfigDir(src)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir(%s): %v", src, err)
	}
	if hc.Config.Harness != "codex" {
		t.Errorf("harness=%q want codex", hc.Config.Harness)
	}
	if hc.Config.Provisioner == nil || hc.Config.Provisioner.Type != "container-script" {
		t.Fatalf("expected provisioner.type=container-script, got %+v", hc.Config.Provisioner)
	}

	// 2. Simulate install.
	installDir := filepath.Join(t.TempDir(), "codex-test")
	if err := util.CopyDir(src, installDir); err != nil {
		t.Fatalf("CopyDir (install): %v", err)
	}

	installedHC, err := config.LoadHarnessConfigDir(installDir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir (installed): %v", err)
	}

	// 3. Assert the home/ file layout (golden paths).
	wantHomeFiles := []string{
		"home/.bashrc",
		"home/.codex/config.toml",
		"home/.codex/scion_notify.sh",
	}
	for _, rel := range wantHomeFiles {
		full := filepath.Join(installDir, filepath.FromSlash(rel))
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected %s in installed bundle: %v", rel, err)
		}
	}

	// 4. Assert bundle root files.
	for _, name := range []string{"config.yaml", "provision.py"} {
		if _, err := os.Stat(filepath.Join(installDir, name)); err != nil {
			t.Errorf("expected %s at bundle root: %v", name, err)
		}
	}

	// 5. Provision from the installed bundle.
	scripted, err := NewContainerScriptHarness(installDir, installedHC.Config)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	if err := scripted.Provision(context.Background(), "test-agent", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bundle := filepath.Join(agentHome, ".scion", "harness")
	for _, name := range []string{"provision.py", "config.yaml", "manifest.json", "scion_harness.py"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Errorf("expected %s in staged bundle: %v", name, err)
		}
	}
	hookWrapper := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "20-harness-provision")
	if _, err := os.Stat(hookWrapper); err != nil {
		t.Fatalf("hook wrapper missing after provision: %v", err)
	}
}

// TestBundleInstall_Antigravity validates the harnesses/antigravity/ bundle
// install path, config.yaml schema acceptance (including mcp, oauth-token,
// vertex-ai auth types, and dialect.yaml), and provisioning staging.
func TestBundleInstall_Antigravity(t *testing.T) {
	src := bundlePath(t, "antigravity")

	// 1. LoadHarnessConfigDir must succeed — config.yaml parses and validates.
	hc, err := config.LoadHarnessConfigDir(src)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir(%s): %v", src, err)
	}
	if hc.Config.Harness != "antigravity" {
		t.Errorf("harness=%q want antigravity", hc.Config.Harness)
	}
	if hc.Config.Provisioner == nil || hc.Config.Provisioner.Type != "container-script" {
		t.Fatalf("expected provisioner.type=container-script, got %+v", hc.Config.Provisioner)
	}

	// Verify schema-critical fields parsed correctly.
	if hc.Config.MCP == nil {
		t.Error("expected mcp block to be parsed, got nil")
	} else {
		if hc.Config.MCP.GlobalConfigFile != ".gemini/config/mcp_config.json" {
			t.Errorf("mcp.global_config_file=%q want .gemini/config/mcp_config.json", hc.Config.MCP.GlobalConfigFile)
		}
	}
	if hc.Config.Auth == nil {
		t.Error("expected auth block to be parsed, got nil")
	} else {
		if _, ok := hc.Config.Auth.Types["oauth-token"]; !ok {
			t.Error("expected auth.types to contain oauth-token")
		}
		if _, ok := hc.Config.Auth.Types["vertex-ai"]; !ok {
			t.Error("expected auth.types to contain vertex-ai")
		}
	}

	// 2. Simulate install: CopyDir from bundle source to a temp target.
	installDir := filepath.Join(t.TempDir(), "antigravity-test")
	if err := util.CopyDir(src, installDir); err != nil {
		t.Fatalf("CopyDir (install): %v", err)
	}

	installedHC, err := config.LoadHarnessConfigDir(installDir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir (installed): %v", err)
	}
	if installedHC.Config.Harness != "antigravity" {
		t.Errorf("installed harness=%q want antigravity", installedHC.Config.Harness)
	}

	// 3. Assert bundle root files.
	for _, name := range []string{"config.yaml", "provision.py", "dialect.yaml"} {
		if _, err := os.Stat(filepath.Join(installDir, name)); err != nil {
			t.Errorf("expected %s at bundle root: %v", name, err)
		}
	}

	// 4. Assert skills directory exists.
	if _, err := os.Stat(filepath.Join(installDir, "skills", ".gitkeep")); err != nil {
		t.Errorf("expected skills/.gitkeep in installed bundle: %v", err)
	}

	// 5. Provision from the installed bundle — verify staging.
	scripted, err := NewContainerScriptHarness(installDir, installedHC.Config)
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	agentHome := t.TempDir()
	if err := scripted.Provision(context.Background(), "test-agent", agentHome, agentHome, "/workspace"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	bundle := filepath.Join(agentHome, ".scion", "harness")
	for _, name := range []string{"provision.py", "config.yaml", "manifest.json", "scion_harness.py"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Errorf("expected %s in staged bundle: %v", name, err)
		}
	}

	// Verify dialect.yaml is staged into the bundle (B.4).
	dialectPath := filepath.Join(bundle, "dialect.yaml")
	if _, err := os.Stat(dialectPath); err != nil {
		t.Fatalf("expected dialect.yaml in staged bundle: %v", err)
	}
	dialectContent, err := os.ReadFile(dialectPath)
	if err != nil {
		t.Fatalf("read staged dialect.yaml: %v", err)
	}
	if !strings.Contains(string(dialectContent), "dialect: antigravity") {
		t.Errorf("staged dialect.yaml does not contain expected 'dialect: antigravity' header")
	}

	hookWrapper := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "20-harness-provision")
	if _, err := os.Stat(hookWrapper); err != nil {
		t.Fatalf("hook wrapper missing after provision: %v", err)
	}
}
