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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func fixedTime() time.Time {
	return time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
}

func TestUpgradeHarnessConfig_LegacyBuiltinAutoActivate(t *testing.T) {
	tmpDir := t.TempDir()
	hcDir := filepath.Join(tmpDir, "opencode")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Legacy opencode config with provisioner.type: builtin.
	configYAML := `harness: opencode
image: scion-opencode:latest
user: scion
provisioner:
  type: builtin
  interface_version: 1
`
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Generic harness (what harness.New("opencode") returns after Phase D).
	h := &MockHarness{NameVal: "generic"}
	plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now: func() time.Time { return fixedTime() },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	if !plan.Changed {
		t.Fatal("expected plan to report changes")
	}

	foundActivate := false
	for _, action := range plan.Actions {
		if action.Type == "activate_script" {
			foundActivate = true
			if action.Detail != "auto-activated container-script (built-in removed)" {
				t.Errorf("unexpected detail: %s", action.Detail)
			}
		}
	}
	if !foundActivate {
		t.Error("expected activate_script action in upgrade plan")
	}

	// Verify config.yaml was actually updated on disk.
	data, err := os.ReadFile(filepath.Join(hcDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	prov, _ := cfg["provisioner"].(map[string]interface{})
	if prov == nil || prov["type"] != "container-script" {
		t.Errorf("expected provisioner.type=container-script, got %v", prov)
	}
}

func TestUpgradeHarnessConfig_LegacyBuiltinNoProvisionPy(t *testing.T) {
	tmpDir := t.TempDir()
	hcDir := filepath.Join(tmpDir, "codex")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}

	configYAML := `harness: codex
image: scion-codex:latest
user: scion
provisioner:
  type: builtin
  interface_version: 1
`
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}
	// No provision.py — should get a warning action, not auto-activation.

	h := &MockHarness{NameVal: "generic"}
	plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now: func() time.Time { return fixedTime() },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}

	foundWarning := false
	for _, action := range plan.Actions {
		if action.Type == "warning" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Error("expected warning action when provision.py is missing")
	}
	if plan.Changed {
		t.Error("config should not be changed when no provision.py exists")
	}
}

func TestUpgradeHarnessConfig_LegacyBuiltinMissingProvisioner(t *testing.T) {
	tmpDir := t.TempDir()
	hcDir := filepath.Join(tmpDir, "opencode")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Config with no provisioner field at all.
	configYAML := `harness: opencode
image: scion-opencode:latest
user: scion
`
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := &MockHarness{NameVal: "generic"}
	plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now: func() time.Time { return fixedTime() },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	if !plan.Changed {
		t.Fatal("expected plan to report changes for missing provisioner")
	}

	foundActivate := false
	for _, action := range plan.Actions {
		if action.Type == "activate_script" {
			foundActivate = true
		}
	}
	if !foundActivate {
		t.Error("expected activate_script action for config with missing provisioner")
	}
}

func TestUpgradeHarnessConfig_ContainerScriptUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	hcDir := filepath.Join(tmpDir, "opencode")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Already on container-script — should be a no-op.
	configYAML := `harness: opencode
image: scion-opencode:latest
user: scion
provisioner:
  type: container-script
  interface_version: 1
`
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := &MockHarness{NameVal: "generic"}
	plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now: func() time.Time { return fixedTime() },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	if plan.Changed {
		t.Error("container-script config should not be changed")
	}
	if len(plan.Actions) != 0 {
		t.Errorf("expected no actions, got %d", len(plan.Actions))
	}
}

func TestUpgradeHarnessConfig_DryRunNoFileChanges(t *testing.T) {
	tmpDir := t.TempDir()
	hcDir := filepath.Join(tmpDir, "opencode")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}

	originalConfig := `harness: opencode
image: scion-opencode:latest
user: scion
provisioner:
  type: builtin
  interface_version: 1
`
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(originalConfig), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	h := &MockHarness{NameVal: "generic"}
	plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		DryRun: true,
		Now:    func() time.Time { return fixedTime() },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	if !plan.Changed {
		t.Fatal("dry-run should still report changes")
	}

	// Verify config.yaml was NOT modified on disk.
	data, err := os.ReadFile(filepath.Join(hcDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != originalConfig {
		t.Error("dry-run should not modify config.yaml on disk")
	}
}
