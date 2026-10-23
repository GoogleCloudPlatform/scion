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

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// newProjectsDirFixture returns a projects directory with one hub workspace
// below it (a long name, so it gets an 8.3 short name) and a sibling
// directory next to it.
func newProjectsDirFixture(t *testing.T) (projects, workspace, sibling string) {
	t.Helper()
	base := t.TempDir()
	projects = filepath.Join(base, "projects")
	workspace = filepath.Join(projects, "longworkspacename-8dot3-probe")
	sibling = filepath.Join(base, "projects-sibling", "longworkspacename-8dot3-probe")
	for _, d := range []string{workspace, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return projects, workspace, sibling
}

// A hub workspace is below the projects directory however it is spelled:
// as created, in a different case, and by its 8.3 short name. Control: the
// projects directory itself and a sibling outside it are not below it.
func TestWindowsHubProjectsDir_AliasFormsAreUnder(t *testing.T) {
	projects, workspace, sibling := newProjectsDirFixture(t)
	if !projectsDirContains(projects, workspace) {
		t.Fatalf("%s: not below %s", workspace, projects)
	}
	upper := strings.ToUpper(workspace)
	if upper == workspace {
		t.Fatalf("case variant %q equals the original", upper)
	}
	if !projectsDirContains(projects, upper) {
		t.Errorf("case variant %s: not below %s", upper, projects)
	}
	short := wintest.ShortPath(t, workspace)
	if !strings.Contains(short, "~") {
		t.Fatalf("no 8.3 short name was generated for %s (%s): the Windows job must enable 8dot3 name creation on the test volume; this test fails rather than passing vacuously", workspace, short)
	}
	if !projectsDirContains(projects, short) {
		t.Errorf("8.3 short name %s: not below %s", short, projects)
	}
	if projectsDirContains(projects, sibling) {
		t.Errorf("control: sibling %s is reported below %s", sibling, projects)
	}
	if projectsDirContains(projects, projects) {
		t.Errorf("control: %s is reported below itself", projects)
	}
	if projectsDirContains(projects, strings.ToUpper(projects)) {
		t.Errorf("control: %s (case variant of the projects dir) is reported below it", strings.ToUpper(projects))
	}
}

// When a path cannot be resolved (here a dangling symlink below the
// projects directory: pathcanon.Within returns an error), it is classified
// as below the projects directory, so its settings file is not loaded as
// a project settings layer. Control: the same link name pointing at an
// existing directory outside is reported outside.
func TestWindowsHubProjectsDir_UnresolvableIsUnder(t *testing.T) {
	projects, _, sibling := newProjectsDirFixture(t)
	dangling := filepath.Join(projects, "dangling")
	wintest.MustSymlink(t, filepath.Join(projects, "missing-target"), dangling)
	if !projectsDirContains(projects, filepath.Join(dangling, "ws")) {
		t.Errorf("unresolvable %s: not classified below %s", dangling, projects)
	}
	out := filepath.Join(projects, "out")
	wintest.MustSymlink(t, sibling, out)
	if projectsDirContains(projects, out) {
		t.Errorf("control: %s (a link to %s outside) is reported below %s", out, sibling, projects)
	}
}

// writeProjectLayerFixture gives dir a settings.yaml (default_template
// from-<tag>), a templates/<tag> template and a harness-configs/<tag>
// harness-config: the three project-layer sources of a project path.
func writeProjectLayerFixture(t *testing.T, dir, tag string) {
	t.Helper()
	mkTemplate(t, filepath.Join(dir, "templates", tag))
	hc := filepath.Join(dir, "harness-configs", tag)
	if err := os.MkdirAll(hc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hc, "config.yaml"), []byte("harness: claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("schema_version: \"1\"\ndefault_template: from-"+tag+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// projectLayerLoaded reports, for each of the three consumers of the hub
// workspace decision, whether dir's own files were used as its project
// layer: the settings layer, the project template tier and the
// harness-config project tier.
func projectLayerLoaded(t *testing.T, dir, tag string) (settings, template, harnessConfig bool) {
	t.Helper()
	vs, err := LoadVersionedSettings(dir)
	if err != nil {
		t.Fatalf("LoadVersionedSettings(%s): %v", dir, err)
	}
	settings = vs.DefaultTemplate == "from-"+tag
	if tpl, err := FindTemplateInProjectPath(tag, dir); err == nil && tpl.Scope == "project" {
		template = true
	}
	if _, err := FindHarnessConfigDir(tag, dir); err == nil {
		harnessConfig = true
	}
	return settings, template, harnessConfig
}

// A hub workspace root without .scion, below %USERPROFILE%\.scion\projects
// and named in a different case or by its 8.3 short name, is below the
// projects directory for all three consumers: its settings.yaml,
// templates/ and harness-configs/ are not its project layer. Control: the
// same files in a sibling outside the projects directory are loaded by
// each consumer.
func TestWindowsHubWorkspace_AliasFormsSkipProjectLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	projects := filepath.Join(home, ".scion", "projects")
	ws := filepath.Join(projects, "longworkspacename-8dot3-probe")
	writeProjectLayerFixture(t, ws, "ws")
	short := wintest.ShortPath(t, ws)
	if !strings.Contains(short, "~") {
		t.Fatalf("no 8.3 short name was generated for %s (%s): the Windows job must enable 8dot3 name creation on the test volume; this test fails rather than passing vacuously", ws, short)
	}
	for _, p := range []string{ws, strings.ToUpper(ws), short} {
		s, tp, hc := projectLayerLoaded(t, p, "ws")
		if s || tp || hc {
			t.Errorf("%s: project layer loaded (settings %v, template %v, harness-config %v); want none", p, s, tp, hc)
		}
	}
	sibling := filepath.Join(home, "elsewhere", "longworkspacename-8dot3-probe")
	writeProjectLayerFixture(t, sibling, "sib")
	if s, tp, hc := projectLayerLoaded(t, sibling, "sib"); !s || !tp || !hc {
		t.Errorf("control %s: settings %v, template %v, harness-config %v; want all loaded", sibling, s, tp, hc)
	}
}

// When the projects directory cannot be resolved (a dangling symlink:
// pathcanon.Within returns an error), every path without .scion is
// classified as below it, so none of the three consumers loads its own
// files as a project layer; the boolean Within returns with the error is
// not used. A path with a .scion entry is still a real project (the .scion
// probe is unchanged). Control: with a resolvable projects directory the
// same path is outside and loaded.
func TestWindowsHubWorkspace_UnresolvableProjectsDirSkipsProjectLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	scionDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(scionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(scionDir, "projects")
	wintest.MustSymlink(t, filepath.Join(scionDir, "missing-target"), projects)
	dir := filepath.Join(home, "repo")
	writeProjectLayerFixture(t, dir, "repo")
	if _, err := pathcanon.Within(projects, dir); err == nil {
		t.Fatalf("pathcanon.Within(%s, %s) did not fail; the fixture does not reach the error path", projects, dir)
	}
	if s, tp, hc := projectLayerLoaded(t, dir, "repo"); s || tp || hc {
		t.Errorf("%s with an unresolvable projects dir: settings %v, template %v, harness-config %v; want none loaded", dir, s, tp, hc)
	}
	if err := os.Mkdir(filepath.Join(dir, DotScion), 0o700); err != nil {
		t.Fatal(err)
	}
	if isHubWorkspaceRootWithoutScion(dir) {
		t.Errorf("%s has a .scion entry but is treated as a hub workspace root", dir)
	}
	if err := os.Remove(filepath.Join(dir, DotScion)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(projects); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	if s, tp, hc := projectLayerLoaded(t, dir, "repo"); !s || !tp || !hc {
		t.Errorf("control %s: settings %v, template %v, harness-config %v; want all loaded", dir, s, tp, hc)
	}
}

// With no home directory (GetGlobalDir fails), a path is classified as
// below the projects directory.
func TestWindowsHubProjectsDir_UnknownHomeIsUnder(t *testing.T) {
	t.Setenv("USERPROFILE", "")
	if _, err := GetGlobalDir(); err == nil {
		t.Fatal("GetGlobalDir succeeded with USERPROFILE empty; the test does not reach the error path")
	}
	if !isUnderHubProjectsDir(t.TempDir()) {
		t.Error("with no home directory, a path is not classified as below the projects directory")
	}
}
