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
	"testing/fstest"
	"time"
)

func fixedTime() time.Time {
	return time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
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

// TestUpgradeHarnessConfig_RefreshesProvisionerScriptsWithoutForce verifies
// that a non-force upgrade from the harnesses/ FS replaces stale provisioner
// scripts with the bundled copies, reports them as refresh_file actions, and
// preserves other existing files.
func TestUpgradeHarnessConfig_RefreshesProvisionerScriptsWithoutForce(t *testing.T) {
	tmpDir := t.TempDir()
	hcDir := filepath.Join(tmpDir, "myh")

	configYAML := "harness: myh\nimage: img:latest\nuser: scion\n"
	harnessesFS := fstest.MapFS{
		"myh/config.yaml":      &fstest.MapFile{Data: []byte(configYAML)},
		"myh/provision.py":     &fstest.MapFile{Data: []byte("# bundled provision v2")},
		"myh/scion_harness.py": &fstest.MapFile{Data: []byte("# bundled lib v2")},
		"myh/dialect.yaml":     &fstest.MapFile{Data: []byte("# bundled dialect")},
	}
	existing := map[string]string{
		"config.yaml":      configYAML,
		"provision.py":     "# stale provision v1",
		"scion_harness.py": "# stale lib v1",
		"dialect.yaml":     "# user dialect",
	}
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range existing {
		if err := os.WriteFile(filepath.Join(hcDir, rel), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	h := &MockHarness{NameVal: "myh"}

	// Dry run reports the refresh without writing.
	plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		DryRun:      true,
		Now:         fixedTime,
		HarnessesFS: harnessesFS,
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig (dry run) failed: %v", err)
	}
	if !plan.Changed {
		t.Error("dry-run plan should report changes")
	}
	refreshed := map[string]bool{}
	for _, a := range plan.Actions {
		if a.Type == "refresh_file" {
			refreshed[a.Path] = true
		}
	}
	if !refreshed["provision.py"] || !refreshed["scion_harness.py"] || len(refreshed) != 2 {
		t.Errorf("refresh_file actions = %v, want provision.py and scion_harness.py", refreshed)
	}
	if data, _ := os.ReadFile(filepath.Join(hcDir, "provision.py")); string(data) != "# stale provision v1" {
		t.Errorf("dry run wrote provision.py: %q", string(data))
	}

	if _, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now:         fixedTime,
		HarnessesFS: harnessesFS,
	}); err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	want := map[string]string{
		"provision.py":     "# bundled provision v2",
		"scion_harness.py": "# bundled lib v2",
		"dialect.yaml":     "# user dialect",
	}
	for rel, wantContent := range want {
		data, err := os.ReadFile(filepath.Join(hcDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != wantContent {
			t.Errorf("%s = %q, want %q", rel, string(data), wantContent)
		}
	}

	// A second upgrade is a no-op once scripts match the bundle.
	plan, err = UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now:         fixedTime,
		HarnessesFS: harnessesFS,
	})
	if err != nil {
		t.Fatalf("second UpgradeHarnessConfig failed: %v", err)
	}
	if plan.Changed {
		t.Errorf("second upgrade should be a no-op, got actions %+v", plan.Actions)
	}
}
