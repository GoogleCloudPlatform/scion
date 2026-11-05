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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nonComponentNames are non-empty names that are not exactly one local path
// component. (A lookup resolves the empty name as the default template:
// TestEmptyNameLookupsKeepTheirMeaning.)
func nonComponentNames(abs string) []string {
	return []string{"../x", "../../x", "a/b", "..", ".", abs}
}

func writeNamedHarnessConfigDir(t *testing.T, dir, image string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "harness: claude\nimage: " + image + "\nuser: scion\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindHarnessConfigDirRefusesNonComponentName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath := filepath.Join(tmp, "project", DotScion)
	tplPath := filepath.Join(tmp, "tpl")

	// Valid harness-config dirs reachable by each escaping name: "../x"
	// from harness-configs is <tier>/x, "../../x" is the tier's parent's x.
	writeNamedHarnessConfigDir(t, filepath.Join(projectPath, "x"), "outside-image:1")
	writeNamedHarnessConfigDir(t, filepath.Join(tmp, "project", "x"), "outside-image:2")
	writeNamedHarnessConfigDir(t, filepath.Join(projectPath, harnessConfigsDirName, "a", "b"), "outside-image:3")
	outsideAbs := filepath.Join(tmp, "outside")
	writeNamedHarnessConfigDir(t, outsideAbs, "outside-image:4")

	for _, name := range nonComponentNames(outsideAbs) {
		hc, err := FindHarnessConfigDir(name, projectPath, tplPath)
		if !errors.Is(err, ErrInvalidResourceName) || hc != nil {
			t.Errorf("FindHarnessConfigDir(%q) = %v, %v; want ErrInvalidResourceName", name, hc, err)
		}
		if err != nil && strings.Contains(err.Error(), name) {
			t.Errorf("FindHarnessConfigDir(%q) error echoes the name: %v", name, err)
		}
		hc, err = ResolveHarnessConfigDir("", name, projectPath, tplPath)
		if !errors.Is(err, ErrInvalidResourceName) || hc != nil {
			t.Errorf("ResolveHarnessConfigDir(%q) = %v, %v; want ErrInvalidResourceName", name, hc, err)
		}
	}

	// A hydrated copy with a bad name is refused too; with no name it loads.
	hydrated := filepath.Join(tmp, "hydrated")
	writeNamedHarnessConfigDir(t, hydrated, "hydrated-image:1")
	if _, err := ResolveHarnessConfigDir(hydrated, "../x", ""); !errors.Is(err, ErrInvalidResourceName) {
		t.Errorf("ResolveHarnessConfigDir(hydrated, ../x) err = %v; want ErrInvalidResourceName", err)
	}
	if hc, err := ResolveHarnessConfigDir(hydrated, "", ""); err != nil || hc.Config.Image != "hydrated-image:1" {
		t.Errorf("ResolveHarnessConfigDir(hydrated, \"\") = %v, %v; want the hydrated copy", hc, err)
	}
}

func TestFindHarnessConfigDirSlugInEachTier(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath := filepath.Join(tmp, "project", DotScion)
	tplPath := filepath.Join(tmp, "tpl")
	writeNamedHarnessConfigDir(t, filepath.Join(tplPath, harnessConfigsDirName, "tier-tpl"), "tpl-image:1")
	writeNamedHarnessConfigDir(t, filepath.Join(projectPath, harnessConfigsDirName, "tier-project"), "project-image:1")
	writeNamedHarnessConfigDir(t, filepath.Join(tmp, GlobalDir, harnessConfigsDirName, "Tier_Global.v2"), "global-image:1")

	for _, tc := range []struct {
		name, image string
		source      HarnessConfigSource
	}{
		{"tier-tpl", "tpl-image:1", HarnessConfigSourceTemplateBundled},
		{"tier-project", "project-image:1", HarnessConfigSourceBrokerLocal},
		{"Tier_Global.v2", "global-image:1", HarnessConfigSourceBrokerLocal},
	} {
		hc, err := FindHarnessConfigDir(tc.name, projectPath, tplPath)
		if err != nil {
			t.Fatalf("FindHarnessConfigDir(%q): %v", tc.name, err)
		}
		if hc.Config.Image != tc.image || hc.Source != tc.source || hc.Name != tc.name {
			t.Errorf("FindHarnessConfigDir(%q) = image %q source %q name %q", tc.name, hc.Config.Image, hc.Source, hc.Name)
		}
		if !filepath.IsAbs(hc.Path) || filepath.Base(hc.Path) != tc.name {
			t.Errorf("FindHarnessConfigDir(%q).Path = %q", tc.name, hc.Path)
		}
	}
}

// templateNameFixture makes a global templates dir and a project .scion
// with templates, plus directories that the escaping names "../x" (inside
// the config dir) and "../../x" (outside it) reach from either templates
// dir.
func templateNameFixture(t *testing.T) (tmp, projectPath string) {
	t.Helper()
	tmp = t.TempDir()
	t.Setenv("HOME", tmp)
	t.Chdir(tmp)
	globalTemplates := filepath.Join(tmp, GlobalDir, "templates")
	projectPath = filepath.Join(tmp, "project", DotScion)
	for _, d := range []string{
		filepath.Join(globalTemplates, "my-tpl"),
		filepath.Join(globalTemplates, "a", "b"),
		filepath.Join(tmp, GlobalDir, "x"),
		filepath.Join(tmp, "x"),
		filepath.Join(projectPath, "templates", "my-tpl"),
		filepath.Join(projectPath, "x"),
		filepath.Join(tmp, "project", "x"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tmp, projectPath
}

func TestFindTemplateRefusesNonComponentName(t *testing.T) {
	_, projectPath := templateNameFixture(t)

	for _, name := range []string{"../x", "../../x", "a/b", "..", "."} {
		if tpl, err := FindTemplate(name); !errors.Is(err, ErrInvalidResourceName) {
			t.Errorf("FindTemplate(%q) = %v, %v; want ErrInvalidResourceName", name, tpl, err)
		}
		for _, scope := range []string{"global", "project"} {
			if tpl := FindTemplateInScope(name, scope); tpl != nil {
				t.Errorf("FindTemplateInScope(%q, %s) = %+v; want nil", name, scope, tpl)
			}
		}
		if tpl, err := FindTemplateInProjectPath(context.Background(), name, projectPath); !errors.Is(err, ErrInvalidResourceName) {
			t.Errorf("FindTemplateInProjectPath(%q) = %v, %v; want ErrInvalidResourceName", name, tpl, err)
		}
	}
}

func TestFindTemplateSlugStillResolves(t *testing.T) {
	tmp, projectPath := templateNameFixture(t)

	tpl, err := FindTemplate("my-tpl")
	if err != nil || tpl.Path != filepath.Join(tmp, GlobalDir, "templates", "my-tpl") {
		t.Errorf("FindTemplate(my-tpl) = %+v, %v", tpl, err)
	}
	if tpl := FindTemplateInScope("my-tpl", "global"); tpl == nil {
		t.Error("FindTemplateInScope(my-tpl, global) = nil")
	}
	tpl, err = FindTemplateInProjectPath(context.Background(), "my-tpl", projectPath)
	if err != nil || tpl.Path != filepath.Join(projectPath, "templates", "my-tpl") {
		t.Errorf("FindTemplateInProjectPath(my-tpl) = %+v, %v", tpl, err)
	}
	// The CLI form still takes an absolute template directory.
	abs := filepath.Join(tmp, "x")
	if tpl, err := FindTemplate(abs); err != nil || tpl.Path != abs {
		t.Errorf("FindTemplate(abs) = %+v, %v", tpl, err)
	}
}

// The empty name is not a refused name in lookups: a template lookup resolves
// it as the default template (or finds nothing when there is no default), and
// never as a templates directory itself; no harness-config tier holds it (a
// harness-config lookup finds nothing). The CLI pulls, which build a directory
// from a name, still refuse it (CheckResourceName).
func TestEmptyNameLookupsKeepTheirMeaning(t *testing.T) {
	tmp, projectPath := templateNameFixture(t)
	ctx := context.Background()
	projectTemplates := filepath.Join(projectPath, "templates")
	globalTemplates := filepath.Join(tmp, GlobalDir, "templates")
	notTemplatesDir := func(what string, tpl *Template) {
		t.Helper()
		if tpl != nil && (tpl.Path == projectTemplates || tpl.Path == globalTemplates) {
			t.Errorf("%s resolved the templates directory %s as a template", what, tpl.Path)
		}
	}

	// No default template: the empty name finds nothing.
	tpl, err := FindTemplateInProjectPath(ctx, "", projectPath)
	notTemplatesDir("FindTemplateInProjectPath(\"\")", tpl)
	if tpl != nil || !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("FindTemplateInProjectPath(\"\") = %+v, %v; want ErrTemplateNotFound with no default template", tpl, err)
	}
	tpl, err = FindTemplate("")
	notTemplatesDir("FindTemplate(\"\")", tpl)
	if tpl != nil || err == nil || errors.Is(err, ErrInvalidResourceName) {
		t.Errorf("FindTemplate(\"\") = %+v, %v; want not found (not refused) with no default template", tpl, err)
	}
	for _, scope := range []string{"global", "project"} {
		if tpl := FindTemplateInScope("", scope); tpl != nil {
			t.Errorf("FindTemplateInScope(\"\", %s) = %+v; want nil with no default template", scope, tpl)
		}
	}
	if chain, err := GetTemplateChain(""); err != nil || len(chain) != 0 {
		t.Errorf("GetTemplateChain(\"\") = %+v, %v; want an empty chain with no default template", chain, err)
	}

	// With default templates: the empty name resolves the default template.
	projectDefault := filepath.Join(projectTemplates, "default")
	globalDefault := filepath.Join(globalTemplates, "default")
	for _, d := range []string{projectDefault, globalDefault} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if tpl, err := FindTemplateInProjectPath(ctx, "", projectPath); err != nil || tpl.Path != projectDefault {
		t.Errorf("FindTemplateInProjectPath(\"\") = %+v, %v; want the project default template", tpl, err)
	}
	if tpl, err := FindTemplate(""); err != nil || tpl.Path != globalDefault {
		t.Errorf("FindTemplate(\"\") = %+v, %v; want the global default template", tpl, err)
	}
	if tpl := FindTemplateInScope("", "global"); tpl == nil || tpl.Path != globalDefault {
		t.Errorf("FindTemplateInScope(\"\", global) = %+v; want the global default template", tpl)
	}
	if chain, err := GetTemplateChain(""); err != nil || len(chain) != 1 || chain[0].Path != globalDefault {
		t.Errorf("GetTemplateChain(\"\") = %+v, %v; want the global default template only", chain, err)
	}
	chain, err := GetTemplateChainInProject(ctx, "", projectPath)
	if err != nil || len(chain) != 1 || chain[0].Path != projectDefault {
		t.Errorf("GetTemplateChainInProject(\"\") = %+v, %v; want the project default template only", chain, err)
	}
	for _, c := range chain {
		notTemplatesDir("GetTemplateChainInProject(\"\")", c)
	}

	hc, err := FindHarnessConfigDir("", projectPath)
	if hc != nil || !errors.Is(err, ErrHarnessConfigNotFound) {
		t.Errorf("FindHarnessConfigDir(\"\") = %v, %v; want ErrHarnessConfigNotFound", hc, err)
	}
	if err := CheckResourceName(""); !errors.Is(err, ErrInvalidResourceName) {
		t.Errorf("CheckResourceName(\"\") = %v; want ErrInvalidResourceName", err)
	}
}
