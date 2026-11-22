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

package source_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	scionModulePath      = "github.com/GoogleCloudPlatform/scion"
	hubPkgPath           = scionModulePath + "/pkg/hub"
	hubfirePkgPath       = hubPkgPath + "/hubfire"
	sourcePkgPath        = hubPkgPath + "/trigger/source"
	triggerGitHubPkgPath = hubPkgPath + "/trigger/github"
	githubAppPkgPath     = hubPkgPath + "/githubapp"
)

// TestImportBoundaryGuard enforces app-shaped-source-seams.md §1 (Stage 3
// design §16 item 17): pkg/hub/trigger/source must remain provider-free and
// must never import pkg/hub/trigger/github, pkg/hub/githubapp, or pkg/hub
// directly or transitively (only stdlib and the provider-free pkg/hub/hubfire
// types package are permitted).
func TestImportBoundaryGuard(t *testing.T) {
	root := findModuleRoot(t)
	pkgDir := filepath.Join(root, "pkg", "hub", "trigger", "source")

	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}

	fset := token.NewFileSet()
	var prodFiles int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		prodFiles++
		fullPath := filepath.Join(pkgDir, name)
		file, perr := parser.ParseFile(fset, fullPath, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", fullPath, perr)
		}
		for _, impSpec := range file.Imports {
			imp, uerr := strconv.Unquote(impSpec.Path.Value)
			if uerr != nil {
				t.Fatalf("unquote import in %s: %v", name, uerr)
			}
			if isForbiddenSourceImport(imp) {
				t.Errorf("%s directly imports forbidden package %q", name, imp)
			}
			if imp == hubfirePkgPath {
				continue
			}
			if strings.HasPrefix(imp, scionModulePath+"/") || strings.Contains(imp, ".") {
				t.Errorf("%s imports disallowed non-stdlib package %q (only stdlib and %q are permitted)", name, imp, hubfirePkgPath)
			}
		}
	}
	if prodFiles == 0 {
		t.Fatalf("expected production .go files in %s", pkgDir)
	}

	for _, forbidden := range []struct {
		path         string
		matchSubtree bool
	}{
		{hubPkgPath, false},
		{triggerGitHubPkgPath, true},
		{githubAppPkgPath, true},
	} {
		chain, cerr := findTransitiveImport(root, sourcePkgPath, forbidden.path, forbidden.matchSubtree)
		if cerr != nil {
			t.Fatalf("check transitive import %s -> %s: %v", sourcePkgPath, forbidden.path, cerr)
		}
		if chain != nil {
			t.Errorf("%s must not transitively import %s, found chain: %s",
				sourcePkgPath, forbidden.path, strings.Join(chain, " -> "))
		}
	}
}

func isForbiddenSourceImport(imp string) bool {
	return imp == hubPkgPath ||
		imp == triggerGitHubPkgPath || strings.HasPrefix(imp, triggerGitHubPkgPath+"/") ||
		imp == githubAppPkgPath || strings.HasPrefix(imp, githubAppPkgPath+"/")
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

func findTransitiveImport(root, from, target string, matchSubtree bool) ([]string, error) {
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		imps, err := readInModuleImports(root, pkg)
		if err != nil {
			return nil, err
		}
		for _, imp := range imps {
			if _, seen := prev[imp]; seen {
				continue
			}
			prev[imp] = pkg
			if imp == target || (matchSubtree && strings.HasPrefix(imp, target+"/")) {
				var chain []string
				for p := imp; p != ""; p = prev[p] {
					chain = append([]string{p}, chain...)
				}
				return chain, nil
			}
			queue = append(queue, imp)
		}
	}
	return nil, nil
}

func readInModuleImports(root, pkg string) ([]string, error) {
	dir := root
	if pkg != scionModulePath {
		dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, scionModulePath+"/")))
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	fset := token.NewFileSet()
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, n, nil, parser.ImportsOnly)
		if perr != nil {
			return nil, perr
		}
		for _, impSpec := range f.Imports {
			imp, uerr := strconv.Unquote(impSpec.Path.Value)
			if uerr != nil {
				return nil, uerr
			}
			if imp == scionModulePath || strings.HasPrefix(imp, scionModulePath+"/") {
				out = append(out, imp)
			}
		}
	}
	return out, nil
}
