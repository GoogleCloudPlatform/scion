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

package transfer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test-only seams of the harness-config confinement change. Production
// code must never set them.
var (
	// seamCallNames are test-only functions: no call outside a _test.go file.
	seamCallNames = map[string]bool{"SetNormalizeBeforeReadForTest": true}
	// seamAssignNames are package variables replaced by tests: no assignment
	// outside a _test.go file (their declarations are not assignments).
	seamAssignNames = map[string]bool{"normalizeTempSuffix": true, "removeAllCache": true}
	// seamHookNames are atomic test hooks: no Store, Swap or CompareAndSwap
	// outside a _test.go file, except inside the function named in
	// seamHookOwners.
	seamHookNames  = map[string]bool{"normalizeBeforeRead": true, "walkRootBeforeOpenDir": true, "openRegularAfterLstat": true, "afterDirLstatHook": true}
	seamHookOwners = map[string]string{"normalizeBeforeRead": "SetNormalizeBeforeReadForTest"}
)

// hookSetters are the atomic.Pointer methods that set a hook.
var hookSetters = map[string]bool{"Store": true, "Swap": true, "CompareAndSwap": true}

// checkSeamUse returns a problem for every use of a test-only seam in one
// non-test Go source file.
func checkSeamUse(fset *token.FileSet, filename string, src any) ([]string, error) {
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var problems []string
	report := func(n ast.Node, msg string) {
		problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), msg))
	}
	name := func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		}
		return ""
	}
	for _, decl := range f.Decls {
		owner := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			owner = fd.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if seamCallNames[name(x.Fun)] {
					report(x, "calls the test-only "+name(x.Fun))
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && hookSetters[sel.Sel.Name] {
					if h := name(sel.X); seamHookNames[h] && seamHookOwners[h] != owner {
						report(x, "sets the test hook "+h)
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if seamAssignNames[name(lhs)] {
						report(x, "assigns the test seam "+name(lhs))
					}
				}
			}
			return true
		})
	}
	return problems, nil
}

// seamScanFiles returns the non-test Go files of the module rooted at root
// that the seam guard scans. It skips dot-directories below the root (a
// workstation checkout keeps other agents' worktrees, on other branches,
// under .scion/agents; .git is not source), directories holding their own
// go.mod (other modules), and node_modules, vendor and testdata.
func seamScanFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch d.Name() {
			case "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// No non-test Go file in the module calls or sets a test-only seam of this
// change.
func TestTestOnlySeamsUnusedInProduction(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	files, err := seamScanFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 100 {
		t.Fatalf("scanned only %d non-test Go files under %s", len(files), root)
	}
	fset := token.NewFileSet()
	for _, p := range files {
		problems, err := checkSeamUse(fset, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, pr := range problems {
			t.Error(pr)
		}
	}
}

// The scan selects the module's own non-test Go files: not other agents'
// worktrees under a dot-directory, not nested modules, not tests.
func TestSeamScanFiles(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"go.mod":                                 "module example.com/m\n",
		"pkg/a.go":                               "package pkg\n",
		"pkg/a_test.go":                          "package pkg\n",
		".scion/agents/other/workspace/pkg/b.go": "package pkg\n",
		".git/c.go":                              "package git\n",
		"extras/sub/go.mod":                      "module example.com/sub\n",
		"extras/sub/d.go":                        "package sub\n",
		"pkg/testdata/e.go":                      "package testdata\n",
		"web/node_modules/x/g.go":                "package x\n",
		"vendor/example.com/v/h.go":              "package v\n",
		"cmd/f.go":                               "package main\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := seamScanFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		got = append(got, filepath.ToSlash(rel))
	}
	if want := []string{"cmd/f.go", "pkg/a.go"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("scanned %v, want %v", got, want)
	}
}

// The checker reports each kind of seam use and allows the seam's own body,
// declarations and unrelated names.
func TestCheckSeamUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"call by package", "package p\nfunc f() { transfer.SetNormalizeBeforeReadForTest(nil) }\n", 1},
		{"call in package", "package transfer\nfunc f() { SetNormalizeBeforeReadForTest(nil) }\n", 1},
		{"assign suffix", "package transfer\nfunc f() { normalizeTempSuffix = nil }\n", 1},
		{"assign cache removal", "package config\nfunc f() { removeAllCache = nil }\n", 1},
		{"store hook", "package transfer\nfunc f() { walkRootBeforeOpenDir.Store(nil) }\n", 1},
		{"swap hook", "package hub\nfunc f() { afterDirLstatHook.Swap(nil) }\n", 1},
		{"compare-and-swap hook", "package transfer\nfunc f() { openRegularAfterLstat.CompareAndSwap(nil, nil) }\n", 1},
		{"owner may set its hook", "package transfer\nfunc SetNormalizeBeforeReadForTest() { normalizeBeforeRead.Swap(nil); func() { normalizeBeforeRead.Store(nil) }() }\n", 0},
		{"another function may not", "package transfer\nfunc g() { normalizeBeforeRead.Store(nil) }\n", 1},
		{"declarations and loads", "package transfer\nvar normalizeTempSuffix = func() {}\nvar removeAllCache = 1\nfunc f() { _ = walkRootBeforeOpenDir.Load() }\n", 0},
		{"unrelated names", "package p\nfunc f() { x.Store(nil); y = 1; SetOtherForTest() }\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems, err := checkSeamUse(token.NewFileSet(), "x.go", tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != tc.want {
				t.Errorf("problems = %v, want %d", problems, tc.want)
			}
		})
	}
}
