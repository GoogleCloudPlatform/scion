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

//go:build unix

package harness

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Start-path writes and removals under the agent home made by this package
// are confined to the home: a link at a leaf is replaced or removed itself
// (never written through or followed) and a linked directory component is
// refused. Each test plants links to an "outside" directory and checks that
// directory is left exactly as it was.

type confinedHomeFixture struct {
	home, outside, before string
}

func newConfinedHomeFixture(t *testing.T) *confinedHomeFixture {
	t.Helper()
	base := t.TempDir()
	f := &confinedHomeFixture{home: filepath.Join(base, "home"), outside: filepath.Join(base, "outside")}
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"victim", "sub/inner", "config.yaml", "provision.py", "manifest.json",
		"scion_harness.py", "capture_auth.py", "20-harness-provision", "30-project-custom",
		"API_KEY", "CODEX_AUTH", "auth-candidates.json", "mcp-servers.json", "telemetry.json",
		"capture-auth-config.json",
	} {
		writeFile(t, filepath.Join(f.outside, rel), "outside "+rel)
	}
	f.before = f.snapshot(t)
	return f
}

func (f *confinedHomeFixture) snapshot(t *testing.T) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(f.outside, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.outside, p)
		line := rel + " " + d.Type().String()
		if d.Type().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			line += " " + string(b)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (f *confinedHomeFixture) checkOutside(t *testing.T) {
	t.Helper()
	if got := f.snapshot(t); got != f.before {
		t.Fatalf("outside directory changed:\n%s\nwant:\n%s", got, f.before)
	}
}

// link replaces whatever is at rel in the home with a link to target
// (relative to the outside directory).
func (f *confinedHomeFixture) link(t *testing.T, target, rel string) {
	t.Helper()
	p := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.outside, target), p); err != nil {
		t.Fatal(err)
	}
}

func requireRegularFile(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%s = %v, %v; want a regular file", path, fi, err)
	}
}

// newConfinedTestHarness is newTestContainerScriptHarness with a
// capture_auth.py and a file-type credential declaration, so Provision and
// ApplyAuthSettings stage every file they can.
func newConfinedTestHarness(t *testing.T) *ContainerScriptHarness {
	t.Helper()
	h, dir := newTestContainerScriptHarness(t)
	writeFile(t, filepath.Join(dir, "capture_auth.py"), "#!/usr/bin/env python3\n")
	writeFile(t, filepath.Join(dir, "dialect.yaml"), "dialect: test\n")
	h.entry.Auth = &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"auth-file": {RequiredFiles: []config.HarnessAuthFileRequirement{
				{Name: "CODEX_AUTH", Type: "file", TargetSuffix: "/.codex/auth.json", Field: "CodexAuthFile"},
			}},
		},
	}
	return h
}

var provisionStagedLeaves = []string{
	".scion/harness/config.yaml",
	".scion/harness/provision.py",
	".scion/harness/dialect.yaml",
	".scion/harness/manifest.json",
	".scion/harness/scion_harness.py",
	".scion/harness/capture_auth.py",
	".scion/harness/inputs/capture-auth-config.json",
	".scion/hooks/pre-start.d/" + HarnessProvisionHookFilename,
}

func TestProvision_LinkLeavesReplaced(t *testing.T) {
	f := newConfinedHomeFixture(t)
	h := newConfinedTestHarness(t)
	for _, rel := range provisionStagedLeaves {
		f.link(t, filepath.Base(rel), rel)
	}
	if err := h.Provision(context.Background(), "a", t.TempDir(), f.home, "/workspace"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range provisionStagedLeaves {
		requireRegularFile(t, filepath.Join(f.home, rel))
	}
	if fi, _ := os.Lstat(filepath.Join(f.home, ".scion/harness/provision.py")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("provision.py mode = %v, want 0755", fi.Mode())
	}
	f.checkOutside(t)
}

var provisionDirComponents = []string{
	".scion", ".scion/harness", ".scion/harness/inputs", ".scion/harness/outputs",
	".scion/harness/secrets", ".scion/hooks", ".scion/hooks/pre-start.d",
}

func TestProvision_RefusesLinkedComponent(t *testing.T) {
	for _, rel := range provisionDirComponents {
		t.Run(rel, func(t *testing.T) {
			f := newConfinedHomeFixture(t)
			h := newConfinedTestHarness(t)
			f.link(t, "sub", rel)
			if err := h.Provision(context.Background(), "a", t.TempDir(), f.home, "/workspace"); err == nil {
				t.Fatal("expected a linked directory component to be refused")
			}
			f.checkOutside(t)
		})
	}
}

func TestProvision_LinkReplantedBetweenStartsIsRefused(t *testing.T) {
	f := newConfinedHomeFixture(t)
	h := newConfinedTestHarness(t)
	if err := h.Provision(context.Background(), "a", t.TempDir(), f.home, "/workspace"); err != nil {
		t.Fatal(err)
	}
	f.link(t, "sub", ".scion/hooks/pre-start.d")
	if err := h.Provision(context.Background(), "a", t.TempDir(), f.home, "/workspace"); err == nil {
		t.Fatal("expected the re-planted link to be refused")
	}
	f.checkOutside(t)
}

func TestStageCaptureAuthAssets_LinkLeafAndComponent(t *testing.T) {
	h := newConfinedTestHarness(t)
	f := newConfinedHomeFixture(t)
	f.link(t, "capture_auth.py", ".scion/harness/capture_auth.py")
	f.link(t, "capture-auth-config.json", ".scion/harness/inputs/capture-auth-config.json")
	if err := StageCaptureAuthAssets(f.home, h.configDirPath, h.entry.Auth); err != nil {
		t.Fatal(err)
	}
	requireRegularFile(t, filepath.Join(f.home, ".scion/harness/capture_auth.py"))
	requireRegularFile(t, filepath.Join(f.home, ".scion/harness/inputs/capture-auth-config.json"))
	f.checkOutside(t)

	f2 := newConfinedHomeFixture(t)
	f2.link(t, "sub", ".scion/harness/inputs")
	if err := StageCaptureAuthAssets(f2.home, h.configDirPath, h.entry.Auth); err == nil {
		t.Fatal("expected a linked directory component to be refused")
	}
	f2.checkOutside(t)
}

func confinedResolvedAuth(t *testing.T) *api.ResolvedAuth {
	t.Helper()
	hostAuth := filepath.Join(t.TempDir(), "auth.json")
	writeFile(t, hostAuth, `{"token":"x"}`)
	return &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{"API_KEY": "secret"},
		Files:   []api.FileMapping{{SourcePath: hostAuth, ContainerPath: "~/.codex/auth.json"}},
	}
}

func TestApplyAuthSettings_LinkLeavesReplaced(t *testing.T) {
	f := newConfinedHomeFixture(t)
	h := newConfinedTestHarness(t)
	leaves := []string{".scion/harness/secrets/API_KEY", ".scion/harness/secrets/CODEX_AUTH", ".scion/harness/inputs/auth-candidates.json"}
	for _, rel := range leaves {
		f.link(t, filepath.Base(rel), rel)
	}
	if err := h.ApplyAuthSettings(f.home, confinedResolvedAuth(t)); err != nil {
		t.Fatal(err)
	}
	for _, rel := range leaves {
		requireRegularFile(t, filepath.Join(f.home, rel))
	}
	f.checkOutside(t)
}

func TestApplyAuthSettings_RefusesLinkedComponent(t *testing.T) {
	for _, rel := range []string{".scion", ".scion/harness", ".scion/harness/secrets", ".scion/harness/inputs"} {
		t.Run(rel, func(t *testing.T) {
			f := newConfinedHomeFixture(t)
			h := newConfinedTestHarness(t)
			f.link(t, "sub", rel)
			if err := h.ApplyAuthSettings(f.home, confinedResolvedAuth(t)); err == nil {
				t.Fatal("expected a linked directory component to be refused")
			}
			f.checkOutside(t)
		})
	}
}

func TestApplyAuthSettings_LinkReplantedBetweenStartsIsRefused(t *testing.T) {
	f := newConfinedHomeFixture(t)
	h := newConfinedTestHarness(t)
	if err := h.ApplyAuthSettings(f.home, confinedResolvedAuth(t)); err != nil {
		t.Fatal(err)
	}
	f.link(t, "sub", ".scion/harness/secrets")
	if err := h.ApplyAuthSettings(f.home, confinedResolvedAuth(t)); err == nil {
		t.Fatal("expected the re-planted link to be refused")
	}
	f.checkOutside(t)
}

func TestStagedInputs_LinkLeafAndComponent(t *testing.T) {
	stage := map[string]func(h *ContainerScriptHarness, home string) error{
		"mcp-servers.json": func(h *ContainerScriptHarness, home string) error {
			return h.ApplyMCPSettings(home, map[string]api.MCPServerConfig{"s": {Command: "x"}})
		},
		"telemetry.json": func(h *ContainerScriptHarness, home string) error {
			return h.ApplyTelemetrySettings(home, &api.TelemetryConfig{}, nil)
		},
	}
	for name, fn := range stage {
		t.Run(name, func(t *testing.T) {
			h := newConfinedTestHarness(t)
			f := newConfinedHomeFixture(t)
			rel := ".scion/harness/inputs/" + name
			f.link(t, name, rel)
			if err := fn(h, f.home); err != nil {
				t.Fatal(err)
			}
			requireRegularFile(t, filepath.Join(f.home, rel))
			f.checkOutside(t)

			f2 := newConfinedHomeFixture(t)
			f2.link(t, "sub", ".scion/harness/inputs")
			if err := fn(h, f2.home); err == nil {
				t.Fatal("expected a linked directory component to be refused")
			}
			f2.checkOutside(t)
		})
	}
}

func TestWriteProjectPreStartHook_LinkLeafAndComponent(t *testing.T) {
	rel := ".scion/hooks/pre-start.d/" + ProjectPreStartHookFilename

	// Write: a link at the hook is replaced.
	f := newConfinedHomeFixture(t)
	f.link(t, ProjectPreStartHookFilename, rel)
	if err := WriteProjectPreStartHook(f.home, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	requireRegularFile(t, filepath.Join(f.home, rel))
	if fi, _ := os.Lstat(filepath.Join(f.home, rel)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", fi.Mode())
	}
	f.checkOutside(t)

	// Removal: a link at the hook is removed itself.
	f.link(t, ProjectPreStartHookFilename, rel)
	if err := WriteProjectPreStartHook(f.home, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, rel)); !os.IsNotExist(err) {
		t.Fatalf("hook link still present: %v", err)
	}
	f.checkOutside(t)

	// A linked directory component is refused for both.
	for _, comp := range []string{".scion", ".scion/hooks", ".scion/hooks/pre-start.d"} {
		for _, content := range []string{"#!/bin/sh\n", ""} {
			f := newConfinedHomeFixture(t)
			f.link(t, "sub", comp)
			if err := WriteProjectPreStartHook(f.home, content); err == nil {
				t.Fatalf("%s (content %q): expected a linked directory component to be refused", comp, content)
			}
			f.checkOutside(t)
		}
	}
}

func TestSetManifestAgentWorkspace_LinksNotFollowed(t *testing.T) {
	f := newConfinedHomeFixture(t)
	f.link(t, "manifest.json", ".scion/harness/manifest.json")
	if err := SetManifestAgentWorkspace(f.home, "/repo-root/x"); err == nil {
		t.Fatal("expected a link at the manifest to be refused")
	}
	f.checkOutside(t)

	f2 := newConfinedHomeFixture(t)
	f2.link(t, "sub", ".scion/harness")
	if err := SetManifestAgentWorkspace(f2.home, "/repo-root/x"); err == nil {
		t.Fatal("expected a linked directory component to be refused")
	}
	f2.checkOutside(t)

	// A well-formed manifest is rewritten in place.
	f3 := newConfinedHomeFixture(t)
	writeFile(t, filepath.Join(f3.home, ".scion/harness/manifest.json"), `{"schema_version":1}`)
	if err := SetManifestAgentWorkspace(f3.home, "/repo-root/x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f3.home, ".scion/harness/manifest.json"))
	if !strings.Contains(string(b), "/repo-root/x") {
		t.Fatalf("manifest = %s", b)
	}
}
