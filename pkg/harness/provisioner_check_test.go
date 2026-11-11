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
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func requireUnusable(t *testing.T, err error) *UnusableProvisionerError {
	t.Helper()
	var ue *UnusableProvisionerError
	if !errors.As(err, &ue) || !errors.Is(err, ErrUnusableProvisioner) {
		t.Fatalf("expected UnusableProvisionerError, got %v", err)
	}
	return ue
}

func assertContains(t *testing.T, label, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s %q does not contain %q", label, s, w)
		}
	}
}

func assertNotContains(t *testing.T, label, s string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(s, n) {
			t.Errorf("%s %q must not contain %q", label, s, n)
		}
	}
}

// Resolve refuses a harness-config whose provisioner cannot run, with a
// typed error naming the harness-config, the reason and a fix
// (ptone/scion#611). Global copy of a bundled harness type: `upgrade`
// repairs it.
func TestResolve_UnusableProvisioner_GlobalBundled(t *testing.T) {
	for _, tc := range []struct {
		name, provisioner, wantReason string
	}{
		{"builtin", "  type: builtin\n  interface_version: 1\n", `provisioner.type "builtin"`},
		{"builtin-with-command", "  type: builtin\n  command: [\"python3\", \"x.py\"]\n", `provisioner.type "builtin"`},
		{"empty-command", "  type: container-script\n  interface_version: 1\n", "provisioner.command is empty"},
		{"untyped-empty-command", "  interface_version: 1\n", "provisioner.command is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			hcDir := filepath.Join(home, ".scion", "harness-configs", "hc")
			writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: claude\nimage: img:test\nprovisioner:\n"+tc.provisioner)
			writeFile(t, filepath.Join(hcDir, "provision.py"), "#!/usr/bin/env python3\n")

			ue := requireUnusable(t, func() error { _, err := Resolve(context.Background(), ResolveOptions{Name: "hc"}); return err }())
			if ue.Name != "hc" || ue.HarnessType != "claude" || ue.Path != hcDir || ue.Scope != HarnessConfigScopeGlobal || !ue.Bundled {
				t.Errorf("error = %+v", ue)
			}
			assertContains(t, "error", ue.Error(), `"hc"`, hcDir, tc.wantReason,
				"scion harness-config upgrade hc --activate-script",
				"scion harness-config install --force --global --name hc <scion-checkout>/harnesses/claude")
			assertNotContains(t, "error", ue.Error(), "harness-config sync")
			pub := ue.PublicMessage()
			assertContains(t, "public message", pub, `"hc"`, tc.wantReason, "the broker's global copy",
				"Repair it on the broker host: run `scion harness-config upgrade hc --activate-script`",
				"upload a working copy to the hub with `scion harness-config sync hc`")
			assertNotContains(t, "public message", pub, home)
		})
	}
}

// Global copy of a harness type with no bundled harness-config: `upgrade`
// would be a no-op, so the fix is to edit config.yaml.
func TestResolve_UnusableProvisioner_GlobalNotBundled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hcDir := filepath.Join(home, ".scion", "harness-configs", "hc")
	writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: generic\nimage: img:test\nprovisioner:\n  type: builtin\n")

	ue := requireUnusable(t, func() error { _, err := Resolve(context.Background(), ResolveOptions{Name: "hc"}); return err }())
	if ue.Scope != HarnessConfigScopeGlobal || ue.Bundled {
		t.Errorf("error = %+v", ue)
	}
	assertContains(t, "error", ue.Error(), "To fix it, edit "+filepath.Join(hcDir, "config.yaml"), "provisioner.command", "cannot repair it")
	assertNotContains(t, "error", ue.Error(), "--activate-script", "harness-config install")
}

// Project copy: `upgrade` only operates on the global directory, so the
// fix names the project's config.yaml (and a project-scope reinstall for a
// bundled type), never `upgrade`.
func TestResolve_UnusableProvisioner_Project(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(t.TempDir(), ".scion")
	hcDir := filepath.Join(project, "harness-configs", "hc")
	writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: claude\nimage: img:test\nprovisioner:\n  type: builtin\n")
	// A usable global copy of the same name is shadowed by the project one.
	writeFile(t, filepath.Join(home, ".scion", "harness-configs", "hc", "config.yaml"), "harness: claude\nimage: img:test\n")

	ue := requireUnusable(t, func() error {
		_, err := Resolve(context.Background(), ResolveOptions{Name: "hc", ProjectPath: project})
		return err
	}())
	if ue.Scope != HarnessConfigScopeProject || ue.Path != hcDir {
		t.Errorf("error = %+v", ue)
	}
	assertContains(t, "error", ue.Error(), "To fix it, edit "+filepath.Join(hcDir, "config.yaml"))
	assertNotContains(t, "error", ue.Error(), "harness-config upgrade", "harness-config install", "--global")
	assertContains(t, "public message", ue.PublicMessage(), "the broker's project copy", "Repair it on the broker host: edit its config.yaml")
	assertNotContains(t, "public message", ue.PublicMessage(), project)
}

// Template-bundled copy: the template is what to fix.
func TestResolve_UnusableProvisioner_Template(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tpl := filepath.Join(t.TempDir(), "tpl")
	hcDir := filepath.Join(tpl, "harness-configs", "hc")
	writeFile(t, filepath.Join(hcDir, "config.yaml"), "harness: claude\nimage: img:test\nprovisioner:\n  type: container-script\n")

	ue := requireUnusable(t, func() error {
		_, err := Resolve(context.Background(), ResolveOptions{Name: "hc", TemplatePaths: []string{tpl}})
		return err
	}())
	if ue.Scope != HarnessConfigScopeTemplate {
		t.Errorf("scope = %q, want template", ue.Scope)
	}
	assertContains(t, "error", ue.Error(), filepath.Join(hcDir, "config.yaml"), "agent's template")
	assertContains(t, "public message", ue.PublicMessage(), "harness-configs/hc/config.yaml in the agent's template",
		"the broker's template copy", "repair the template itself")
	assertNotContains(t, "error", ue.Error(), "harness-config upgrade", "harness-config install")
}

// Hub-hydrated copy: repair a local copy and sync it to the hub.
func TestResolve_UnusableProvisioner_HubHydrated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hydrated := filepath.Join(t.TempDir(), "hc")
	writeFile(t, filepath.Join(hydrated, "config.yaml"), "harness: claude\nimage: img:test\nprovisioner:\n  type: builtin\n")

	ue := requireUnusable(t, func() error {
		_, err := Resolve(context.Background(), ResolveOptions{Name: "hc", ConfigDirPath: hydrated})
		return err
	}())
	if ue.Scope != HarnessConfigScopeHub {
		t.Errorf("scope = %q, want hub", ue.Scope)
	}
	assertContains(t, "public message", ue.PublicMessage(), "(hub)", "To fix it, pull it (`scion harness-config pull hc`)",
		"upload it to the scope it came from with `scion harness-config sync hc` (add `--global` for a global record)")
	assertNotContains(t, "public message", ue.PublicMessage(), "broker host")
	assertNotContains(t, "public message", ue.PublicMessage(), hydrated)
}

// A container-script provisioner with a command, and a harness-config with
// no provisioner block, still resolve.
func TestResolve_UsableProvisionerUnaffected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion", "harness-configs")
	writeFile(t, filepath.Join(dir, "cs", "config.yaml"), "harness: claude\nimage: img:test\nprovisioner:\n  type: container-script\n  command: [\"python3\", \"/home/scion/.scion/harness/provision.py\"]\n")
	writeFile(t, filepath.Join(dir, "cs", "provision.py"), "#!/usr/bin/env python3\n")
	writeFile(t, filepath.Join(dir, "decl", "config.yaml"), "harness: generic\nimage: img:test\n")

	if r, err := Resolve(context.Background(), ResolveOptions{Name: "cs"}); err != nil || r.Implementation != "container-script" {
		t.Errorf("container-script: r=%v err=%v", r, err)
	}
	if r, err := Resolve(context.Background(), ResolveOptions{Name: "decl"}); err != nil || r.Implementation != "generic" {
		t.Errorf("declarative: r=%v err=%v", r, err)
	}
}
