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

package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// TestEnsureSSHDirOnlyUsesTheHandleWalk: ensureSSHDir makes no path-based
// open or location decision of its own. Its body may call only
// privatefs.OpenTree (the anchor, home), Tree.MkdirAllEach (on Windows the
// handle-relative walk, guarded by privatefs's TestTreeWalkIsHandleRelative;
// on unix os.MkdirAll, as before), Tree.Close and privatefs.ForMode.
// Anything else (os.MkdirAll, os.Stat, filepath.EvalSymlinks,
// pathcanon.Within, ...) fails this test.
func TestEnsureSSHDirOnlyUsesTheHandleWalk(t *testing.T) {
	allowed := map[string]bool{"OpenTree": true, "MkdirAllEach": true, "Close": true, "ForMode": true}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ssh_config_files.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	var calls, bad []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ensureSSHDir" || fn.Recv != nil {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch x := call.Fun.(type) {
			case *ast.Ident:
				name = x.Name
			case *ast.SelectorExpr:
				name = x.Sel.Name
			case *ast.FuncLit:
				return true
			}
			calls = append(calls, name)
			if !allowed[name] {
				bad = append(bad, fset.Position(call.Pos()).String()+" "+name)
			}
			return true
		})
	}
	if !found {
		t.Fatal("ensureSSHDir not found in ssh_config_files.go")
	}
	sort.Strings(calls)
	if len(calls) < 3 {
		t.Fatalf("ensureSSHDir calls %v: expected OpenTree, MkdirAllEach and Close (the guard would pass vacuously)", calls)
	}
	if len(bad) > 0 {
		t.Fatalf("ensureSSHDir may only open home as a privatefs tree and walk it by handle; it calls: %v", bad)
	}
}
