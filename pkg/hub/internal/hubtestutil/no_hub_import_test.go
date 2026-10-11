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

package hubtestutil_test

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
	modulePath = "github.com/GoogleCloudPlatform/scion"
	hubPath    = modulePath + "/pkg/hub"
	selfPath   = hubPath + "/internal/hubtestutil"
)

// TestNoHubImport fails if this package reaches pkg/hub through any chain of
// in-module imports. pkg/hub/<area> packages are imported by pkg/hub, so
// their tests cannot import pkg/hub; they can use these helpers only while
// this package stays independent of it.
//
// It parses import clauses only (no go list, no type-checking) and counts
// every non-test .go file of each package regardless of build tags, so it
// over-approximates the real import graph.
func TestNoHubImport(t *testing.T) {
	root := moduleRoot(t)
	chain, err := findImport(root, selfPath, hubPath)
	if err != nil {
		t.Fatal(err)
	}
	if chain != nil {
		t.Fatalf("%s must not import %s, but it does via:\n  %s", selfPath, hubPath, strings.Join(chain, "\n  -> "))
	}
}

// TestFindImportDetectsHub checks the walker against a fixture module that
// reaches its "pkg/hub" through an intermediate package.
func TestFindImportDetectsHub(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/hub/internal/hubtestutil/a.go", "package hubtestutil\n\nimport (\n\t\"fmt\"\n\t\""+modulePath+"/pkg/mid\"\n)\n")
	write("pkg/hub/internal/hubtestutil/a_test.go", "package hubtestutil\n\nimport _ \""+hubPath+"\"\n")
	write("pkg/mid/m.go", "//go:build linux\n\npackage mid\n\nimport _ \""+hubPath+"\"\n")
	write("pkg/hub/h.go", "package hub\n")
	chain, err := findImport(root, selfPath, hubPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{selfPath, modulePath + "/pkg/mid", hubPath}
	if strings.Join(chain, " ") != strings.Join(want, " ") {
		t.Fatalf("chain = %v, want %v", chain, want)
	}

	// Test files do not count: only a.go's imports are followed.
	write("pkg/mid/m.go", "package mid\n")
	chain, err = findImport(root, selfPath, hubPath)
	if err != nil {
		t.Fatal(err)
	}
	if chain != nil {
		t.Fatalf("chain = %v, want none (the only hub import is in a _test.go file)", chain)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// findImport walks in-module imports breadth-first from "from" and returns
// the first chain that reaches "target", or nil.
func findImport(root, from, target string) ([]string, error) {
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		imps, err := packageImports(root, pkg)
		if err != nil {
			return nil, err
		}
		for _, imp := range imps {
			if _, seen := prev[imp]; seen {
				continue
			}
			prev[imp] = pkg
			if imp == target {
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

// packageImports returns the in-module imports of the non-test .go files of
// the package with import path pkg.
func packageImports(root, pkg string) ([]string, error) {
	dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, modulePath+"/")))
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
		f, err := parser.ParseFile(fset, n, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, s := range f.Imports {
			ip, err := strconv.Unquote(s.Path.Value)
			if err != nil {
				return nil, err
			}
			if ip == modulePath || strings.HasPrefix(ip, modulePath+"/") {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}
