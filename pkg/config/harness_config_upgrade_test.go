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
	"strings"
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

// upgradeFixture writes a harness-config directory named dirName (with
// harness: myh) and returns it together with a matching harnesses/ FS.
func upgradeFixture(t *testing.T, dirName string, existing map[string]string) (string, fstest.MapFS) {
	t.Helper()
	hcDir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range existing {
		if err := os.WriteFile(filepath.Join(hcDir, rel), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	harnessesFS := fstest.MapFS{
		"myh/config.yaml":      &fstest.MapFile{Data: []byte(upgradeFixtureConfig)},
		"myh/provision.py":     &fstest.MapFile{Data: []byte("# bundled provision v2")},
		"myh/scion_harness.py": &fstest.MapFile{Data: []byte("# bundled lib v2")},
		"myh/capture_auth.py":  &fstest.MapFile{Data: []byte("# bundled capture v2")},
		"myh/dialect.yaml":     &fstest.MapFile{Data: []byte("# bundled dialect")},
	}
	return hcDir, harnessesFS
}

const upgradeFixtureConfig = "harness: myh\nimage: img:latest\nuser: scion\n"

var staleScripts = map[string]string{
	"provision.py":     "# stale provision v1",
	"scion_harness.py": "# stale lib v1",
	"capture_auth.py":  "# stale capture v1",
}

func actionsOfType(plan *HarnessConfigUpgradePlan, typ string) map[string]HarnessConfigUpgradeAction {
	out := map[string]HarnessConfigUpgradeAction{}
	for _, a := range plan.Actions {
		if a.Type == typ {
			out[a.Path] = a
		}
	}
	return out
}

func assertFileContents(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	for rel, wantContent := range want {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != wantContent {
			t.Errorf("%s = %q, want %q", rel, string(data), wantContent)
		}
	}
}

// TestUpgradeHarnessConfig_RefreshesProvisionerScriptsWithoutForce verifies
// that a non-force upgrade of a bundled harness-config (directory name equals
// the harness name) replaces stale provisioner scripts with the bundled
// copies, backs each one up first, reports refresh_file actions, and
// preserves other existing files.
func TestUpgradeHarnessConfig_RefreshesProvisionerScriptsWithoutForce(t *testing.T) {
	existing := map[string]string{
		"config.yaml":  upgradeFixtureConfig,
		"dialect.yaml": "# user dialect",
	}
	for rel, content := range staleScripts {
		existing[rel] = content
	}
	hcDir, harnessesFS := upgradeFixture(t, "myh", existing)
	h := &MockHarness{NameVal: "myh"}

	// Dry run reports the refresh without writing anything.
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
	refreshed := actionsOfType(plan, "refresh_file")
	if len(refreshed) != 3 {
		t.Errorf("refresh_file actions = %v, want provision.py, scion_harness.py and capture_auth.py", refreshed)
	}
	for rel := range staleScripts {
		a, ok := refreshed[rel]
		if !ok {
			t.Errorf("missing refresh_file action for %s", rel)
			continue
		}
		if strings.Contains(a.Detail, "replaced") || strings.Contains(a.Detail, "backup") {
			t.Errorf("dry-run detail for %s should be neutral, got %q", rel, a.Detail)
		}
	}
	if len(plan.Backups) != 0 {
		t.Errorf("dry run should not write backups, got %v", plan.Backups)
	}
	assertFileContents(t, hcDir, staleScripts)

	plan, err = UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
		Now:         fixedTime,
		HarnessesFS: harnessesFS,
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	assertFileContents(t, hcDir, map[string]string{
		"provision.py":     "# bundled provision v2",
		"scion_harness.py": "# bundled lib v2",
		"capture_auth.py":  "# bundled capture v2",
		"dialect.yaml":     "# user dialect",
	})

	// Each replaced script was backed up with the same naming as config.yaml.
	if len(plan.Backups) != 3 {
		t.Fatalf("backups = %v, want 3", plan.Backups)
	}
	ts := fixedTime().UTC().Format("20060102T150405Z")
	for rel, staleContent := range staleScripts {
		backup := filepath.Join(hcDir, rel+".bak."+ts)
		data, err := os.ReadFile(backup)
		if err != nil {
			t.Errorf("missing backup for %s: %v", rel, err)
			continue
		}
		if string(data) != staleContent {
			t.Errorf("backup of %s = %q, want %q", rel, string(data), staleContent)
		}
		if a := actionsOfType(plan, "refresh_file")[rel]; !strings.Contains(a.Detail, filepath.Base(backup)) {
			t.Errorf("refresh_file detail for %s = %q, want it to name the backup", rel, a.Detail)
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

// TestUpgradeHarnessConfig_CustomNamedConfigKeepsScripts is the regression
// test for a custom-named harness-config that reuses a bundled harness type
// (e.g. "install --name my-myh" with harness: myh). Upgrade must keep its own
// provisioner scripts, while still running its other actions such as adding
// missing support files.
func TestUpgradeHarnessConfig_CustomNamedConfigKeepsScripts(t *testing.T) {
	custom := map[string]string{
		"provision.py":     "# my customised provisioner",
		"scion_harness.py": "# my lib",
		"capture_auth.py":  "# my capture",
	}
	existing := map[string]string{"config.yaml": upgradeFixtureConfig}
	for rel, content := range custom {
		existing[rel] = content
	}
	hcDir, harnessesFS := upgradeFixture(t, "my-myh", existing)
	h := &MockHarness{NameVal: "myh"}

	for _, dryRun := range []bool{true, false} {
		plan, err := UpgradeHarnessConfig(hcDir, h, HarnessConfigUpgradeOptions{
			DryRun:      dryRun,
			Now:         fixedTime,
			HarnessesFS: harnessesFS,
		})
		if err != nil {
			t.Fatalf("UpgradeHarnessConfig (dryRun=%v) failed: %v", dryRun, err)
		}
		if got := actionsOfType(plan, "refresh_file"); len(got) != 0 {
			t.Errorf("dryRun=%v: custom-named config got refresh_file actions %v", dryRun, got)
		}
		if len(plan.Backups) != 0 {
			t.Errorf("dryRun=%v: unexpected backups %v", dryRun, plan.Backups)
		}
		if _, ok := actionsOfType(plan, "add_file")["dialect.yaml"]; !ok {
			t.Errorf("dryRun=%v: expected add_file dialect.yaml, got %+v", dryRun, plan.Actions)
		}
		assertFileContents(t, hcDir, custom)
	}
	assertFileContents(t, hcDir, map[string]string{"dialect.yaml": "# bundled dialect"})
}

// TestUpgradeHarnessConfig_SkipsSymlinkedProvisionerScript verifies that
// upgrade neither writes through nor replaces a symlinked provisioner script,
// and reports it as a skip_file action.
func TestUpgradeHarnessConfig_SkipsSymlinkedProvisionerScript(t *testing.T) {
	hcDir, harnessesFS := upgradeFixture(t, "myh", map[string]string{
		"config.yaml":      upgradeFixtureConfig,
		"scion_harness.py": "# bundled lib v2",
		"capture_auth.py":  "# bundled capture v2",
		"dialect.yaml":     "# bundled dialect",
	})
	linkTarget := filepath.Join(t.TempDir(), "my-provision.py")
	if err := os.WriteFile(linkTarget, []byte("# my linked provision"), 0644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(hcDir, "provision.py")
	if err := os.Symlink(linkTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	plan, err := UpgradeHarnessConfig(hcDir, &MockHarness{NameVal: "myh"}, HarnessConfigUpgradeOptions{
		Now:         fixedTime,
		HarnessesFS: harnessesFS,
	})
	if err != nil {
		t.Fatalf("UpgradeHarnessConfig failed: %v", err)
	}
	if _, ok := actionsOfType(plan, "skip_file")["provision.py"]; !ok {
		t.Errorf("expected skip_file provision.py, got %+v", plan.Actions)
	}
	if got := actionsOfType(plan, "refresh_file"); len(got) != 0 {
		t.Errorf("unexpected refresh_file actions %v", got)
	}
	if plan.Changed {
		t.Errorf("skipping a symlink should not count as a change, got %+v", plan.Actions)
	}
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("provision.py symlink was replaced")
	}
	assertFileContents(t, filepath.Dir(linkTarget), map[string]string{"my-provision.py": "# my linked provision"})
}
