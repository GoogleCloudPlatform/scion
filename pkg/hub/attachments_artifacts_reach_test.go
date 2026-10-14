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

package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOpenBoundAttachmentReachableOnlyFromSendPath pins that the artifact
// service's OpenBoundAttachment, which reads an attachment's bytes without
// checking any caller, is reached from exactly one place in the hub: the
// send path, after the send is authorized and the message stored. The call
// chain must be
//
//	sendAgentRouted -> stageChatAttachments -> stageArtifactAttachment -> OpenBoundAttachment
//
// and nothing else in the package (no handler, no route) may call any link
// of it; nor may any other package of the module reference the opener. A
// new caller fails this test and needs a security review.
func TestOpenBoundAttachmentReachableOnlyFromSendPath(t *testing.T) {
	callers := callersInPackage(t, ".")
	want := map[string][]string{
		"OpenBoundAttachment":     {"stageArtifactAttachment"},
		"stageArtifactAttachment": {"stageChatAttachments"},
		"stageChatAttachments":    {"sendAgentRouted"},
	}
	for callee, wantCallers := range want {
		got := callers[callee]
		if strings.Join(got, ",") != strings.Join(wantCallers, ",") {
			t.Errorf("%s is called from %v; want only %v", callee, got, wantCallers)
		}
	}
	// The functions of the chain, the opener included, are never used as
	// values (a handler or method value made from one would escape the
	// call check above).
	for name, n := range valueUsesInPackage(t, ".", []string{"OpenBoundAttachment", "stageArtifactAttachment", "stageChatAttachments"}) {
		if n > 0 {
			t.Errorf("%s is used as a value %d times; it may only be called", name, n)
		}
	}
	// The opener is an exported method of the artifact service, so the rest
	// of the module is checked too: outside its own declaration, the only
	// reference to it in non-test code is the call above.
	refs := moduleSymbolReferences(t, "../..", "OpenBoundAttachment")
	if want := []string{"pkg/hub/attachments_artifacts.go:stageArtifactAttachment"}; strings.Join(refs, ",") != strings.Join(want, ",") {
		t.Errorf("OpenBoundAttachment is referenced at %v; want only %v", refs, want)
	}
}

// moduleSymbolReferences returns, sorted, "<file>:<function>" for every reference
// to name (an identifier or a selected name, called or not) in the non-test
// Go files of the module rooted at root, skipping nested modules, hidden
// directories, testdata, vendor and node_modules. A function's own
// declaration name is not a reference; a reference outside any function is
// reported with the function "<file scope>".
func moduleSymbolReferences(t *testing.T, root, name string) []string {
	t.Helper()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(absRoot, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root: %v", absRoot, err)
	}
	var out []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if path != absRoot {
				if strings.HasPrefix(base, ".") || base == "testdata" || base == "vendor" || base == "node_modules" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		declNames := map[ast.Node]bool{}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				declNames[fn.Name] = true
			}
		}
		seen := map[ast.Node]bool{}
		record := func(fn string, n ast.Node) {
			ast.Inspect(n, func(m ast.Node) bool {
				id, ok := m.(*ast.Ident)
				if !ok || id.Name != name || declNames[id] || seen[id] {
					return true
				}
				seen[id] = true
				out = append(out, rel+":"+fn)
				return true
			})
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				record(fn.Name.Name, fn.Body)
			}
		}
		record("<file scope>", f)
		return nil
	})
	if err != nil {
		t.Fatalf("scan module: %v", err)
	}
	sort.Strings(out)
	return out
}

// nonTestGoFiles parses the package's non-test Go files in dir.
func nonTestGoFiles(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return fset, files
}

// callersInPackage maps each called function or method name to the sorted,
// distinct names of the functions whose bodies call it.
func callersInPackage(t *testing.T, dir string) map[string][]string {
	t.Helper()
	_, files := nonTestGoFiles(t, dir)
	seen := map[string]map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				}
				if name != "" {
					if seen[name] == nil {
						seen[name] = map[string]bool{}
					}
					seen[name][fn.Name.Name] = true
				}
				return true
			})
		}
	}
	out := map[string][]string{}
	for callee, set := range seen {
		for caller := range set {
			out[callee] = append(out[callee], caller)
		}
		sort.Strings(out[callee])
	}
	return out
}

// valueUsesInPackage counts, for each name, the selector or identifier
// references to it that are not the function of a call expression.
func valueUsesInPackage(t *testing.T, dir string, names []string) map[string]int {
	t.Helper()
	_, files := nonTestGoFiles(t, dir)
	want := map[string]bool{}
	out := map[string]int{}
	for _, n := range names {
		want[n] = true
		out[n] = 0
	}
	for _, f := range files {
		// called holds every call's function expression. selected holds
		// the selected name of every selector (counted through the
		// selector itself) and the name of every function declaration.
		called := map[ast.Node]bool{}
		selected := map[ast.Node]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				called[x.Fun] = true
			case *ast.SelectorExpr:
				selected[x.Sel] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				// The declaration's own name is not a use.
				selected[fn.Name] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			countValueUse(n, called, selected, want, out)
			return true
		})
	}
	return out
}

func countValueUse(n ast.Node, called, selected map[ast.Node]bool, want map[string]bool, out map[string]int) {
	switch x := n.(type) {
	case *ast.SelectorExpr:
		if want[x.Sel.Name] && !called[x] {
			out[x.Sel.Name]++
		}
	case *ast.Ident:
		if want[x.Name] && !called[x] && !selected[x] {
			out[x.Name]++
		}
	}
}
