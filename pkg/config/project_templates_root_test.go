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
)

func mkTemplate(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("image: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// For a project path that is a workspace root holding a .scion directory
// (a hub-managed project), the project template tier is .scion/templates;
// the root's own templates/ directory is not a template source. A project
// path that is the .scion directory itself is unchanged.
func TestFindTemplateInProjectPath_WorkspaceRootUsesScionTemplates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(t.TempDir(), "project")
	mkTemplate(t, filepath.Join(root, ".scion", "templates", "in-scion"))
	mkTemplate(t, filepath.Join(root, "templates", "at-root"))
	resolvedScion, err := filepath.EvalSymlinks(filepath.Join(root, ".scion"))
	if err != nil {
		t.Fatal(err)
	}

	for _, projectPath := range []string{root, filepath.Join(root, ".scion")} {
		tpl, err := FindTemplateInProjectPath("in-scion", projectPath)
		if err != nil {
			t.Fatalf("%s: in-scion: %v", projectPath, err)
		}
		if tpl.Scope != "project" || !strings.HasPrefix(tpl.Path, resolvedScion) && !strings.HasPrefix(tpl.Path, filepath.Join(root, ".scion")) {
			t.Errorf("%s: in-scion = %+v, want the project template under .scion", projectPath, tpl)
		}
		if tpl, err := FindTemplateInProjectPath("at-root", projectPath); err == nil {
			t.Errorf("%s: at-root resolved to %+v; the workspace root's templates/ is not a template source", projectPath, tpl)
		}
	}

	// A workspace-root template does not shadow a global one of the same
	// name.
	mkTemplate(t, filepath.Join(home, ".scion", "templates", "at-root"))
	tpl, err := FindTemplateInProjectPath("at-root", root)
	if err != nil || tpl.Scope != "global" {
		t.Errorf("at-root = (%+v, %v), want the global template", tpl, err)
	}
}

// A project path without a .scion entry keeps its templates/ tier (the
// path is used as given).
func TestFindTemplateInProjectPath_PlainPathUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "configs")
	mkTemplate(t, filepath.Join(dir, "templates", "t1"))
	tpl, err := FindTemplateInProjectPath("t1", dir)
	if err != nil || tpl.Scope != "project" || tpl.Path != filepath.Join(dir, "templates", "t1") {
		t.Errorf("t1 = (%+v, %v)", tpl, err)
	}
}

func TestLegacyProjectTemplatesWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w := LegacyProjectTemplatesWarning(root); w != "" {
		t.Errorf("no templates dir: warning %q", w)
	}
	// Non-template content in templates/ does not warn.
	if err := os.MkdirAll(filepath.Join(root, "templates", "partials"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w := LegacyProjectTemplatesWarning(root); w != "" {
		t.Errorf("unrelated templates dir: warning %q", w)
	}
	mkTemplate(t, filepath.Join(root, "templates", "b"))
	mkTemplate(t, filepath.Join(root, "templates", "a"))
	w := LegacyProjectTemplatesWarning(root)
	for _, want := range []string{"Warning:", filepath.Join(root, "templates"), "(a, b)", filepath.Join(".scion", "templates")} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q lacks %q", w, want)
		}
	}
	// The .scion directory itself, an empty path, and a path without a
	// .scion entry never warn.
	for _, p := range []string{filepath.Join(root, ".scion"), "", filepath.Join(root, "templates")} {
		if w := LegacyProjectTemplatesWarning(p); w != "" {
			t.Errorf("%q: warning %q", p, w)
		}
	}
}
