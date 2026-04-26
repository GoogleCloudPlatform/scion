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

// Package config_test holds tests that depend on pkg/harness. They live in
// an external test package so pkg/config production code can import what it
// needs from pkg/harness's shared types (via pkg/api) without creating an
// in-package import cycle with pkg/harness during testing.
package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestSeedHarnessConfig_CodexNotifyScript(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "codex")

	err := config.SeedHarnessConfig(targetDir, &harness.Codex{}, false)
	if err != nil {
		t.Fatalf("SeedHarnessConfig failed: %v", err)
	}

	scriptPath := filepath.Join(targetDir, "home", ".codex", "scion_notify.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("expected notify script to be seeded at %s: %v", scriptPath, err)
	}
}

func TestUpgradeHarnessConfig_AdditiveMergeAndBackup(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "codex")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	current := `harness: codex
image: custom-codex:latest
user: developer
env:
  CUSTOM: "1"
`
	if err := os.WriteFile(filepath.Join(targetDir, "config.yaml"), []byte(current), 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := config.UpgradeHarnessConfig(targetDir, &harness.Codex{}, config.HarnessConfigUpgradeOptions{
		Now: func() time.Time { return time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	if !plan.Changed {
		t.Fatal("expected upgrade to report changes")
	}
	if len(plan.Backups) != 1 {
		t.Fatalf("expected one backup, got %v", plan.Backups)
	}
	if _, err := os.Stat(plan.Backups[0]); err != nil {
		t.Fatalf("expected backup file: %v", err)
	}

	hc, err := config.LoadHarnessConfigDir(targetDir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir failed after upgrade: %v", err)
	}
	if hc.Config.Image != "custom-codex:latest" || hc.Config.User != "developer" {
		t.Fatalf("user-owned fields were not preserved: %#v", hc.Config)
	}
	if hc.Config.Provisioner == nil || hc.Config.Provisioner.Type != "builtin" {
		t.Fatalf("expected additive metadata to include builtin provisioner, got %#v", hc.Config.Provisioner)
	}
	if hc.Config.Env["CUSTOM"] != "1" || hc.Config.Env["SCION_CODEX_NOTIFY_AUTO_COMPLETE"] != "true" {
		t.Fatalf("expected env map to preserve custom values and add defaults, got %#v", hc.Config.Env)
	}
}

func TestUpgradeHarnessConfig_DryRunDoesNotWrite(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "opencode")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	current := []byte("harness: opencode\nimage: custom:latest\n")
	configPath := filepath.Join(targetDir, "config.yaml")
	if err := os.WriteFile(configPath, current, 0644); err != nil {
		t.Fatal(err)
	}

	plan, err := config.UpgradeHarnessConfig(targetDir, &harness.OpenCode{}, config.HarnessConfigUpgradeOptions{DryRun: true})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig dry-run failed: %v", err)
	}
	if !plan.Changed {
		t.Fatal("expected dry-run to report planned changes")
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(current) {
		t.Fatalf("dry-run wrote config.yaml:\n%s", after)
	}
}
