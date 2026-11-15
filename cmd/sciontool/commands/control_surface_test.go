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

package commands

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestControlHandlerOnlyOnTheSession guards the exec amendment's third
// bound (and §3.7): sciontool's control handler, which serves exec-start
// and exec-status when registered, is reachable only as the RPC handler of
// the agent's own Conduit session. In the non-test sources of
// cmd/sciontool and pkg/sciontool, control.New is called exactly once, and
// its result is used only as control.RPCHandler's argument and for its
// Routes(); it is never served over a listener, a port forward or a
// tunnel.
func TestControlHandlerOnlyOnTheSession(t *testing.T) {
	root := findRepoRoot(t)
	var calls []string
	for _, dir := range []string{"cmd/sciontool", "pkg/sciontool"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			if strings.Contains(path, string(filepath.Separator)+"control"+string(filepath.Separator)) {
				return nil // the package itself
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			checkControlUses(t, fset, f, &calls)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "init_conduit.go") {
		t.Fatalf("control.New calls = %v, want exactly one, in init_conduit.go", calls)
	}
}

// checkControlUses records each control.New call in f and fails on any use
// of the variable it is assigned to other than control.RPCHandler(v) and
// v.Routes().
func checkControlUses(t *testing.T, fset *token.FileSet, f *ast.File, calls *[]string) {
	t.Helper()
	isControlNew := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "control" && sel.Sel.Name == "New"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncLit)
		var body *ast.BlockStmt
		if ok {
			body = fn.Body
		} else if fd, ok := n.(*ast.FuncDecl); ok {
			body = fd.Body
		}
		if body == nil {
			return true
		}
		for _, stmt := range body.List {
			as, ok := stmt.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 || !isControlNew(as.Rhs[0]) {
				continue
			}
			*calls = append(*calls, fset.Position(as.Pos()).Filename)
			v, ok := as.Lhs[0].(*ast.Ident)
			if !ok {
				t.Errorf("%s: control.New result not assigned to a variable", fset.Position(as.Pos()))
				continue
			}
			checkVarUses(t, fset, body, v.Name, as)
		}
		return true
	})
	// A control.New anywhere else (not a plain assignment in a body).
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(ast.Expr); ok && isControlNew(c) {
			found := false
			for _, p := range *calls {
				if p == fset.Position(c.Pos()).Filename {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: control.New used outside a checked assignment", fset.Position(c.Pos()))
			}
		}
		return true
	})
}

func checkVarUses(t *testing.T, fset *token.FileSet, body *ast.BlockStmt, name string, def *ast.AssignStmt) {
	t.Helper()
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "control" && sel.Sel.Name == "RPCHandler" && len(x.Args) == 1 {
					if id, ok := x.Args[0].(*ast.Ident); ok && id.Name == name {
						allowed[id] = true
					}
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == name && sel.Sel.Name == "Routes" {
					allowed[id] = true
				}
			}
		}
		return true
	})
	if def != nil {
		allowed[def.Lhs[0].(*ast.Ident)] = true
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name && !allowed[id] {
			t.Errorf("%s: the control handler %q is used other than as the session's RPC handler", fset.Position(id.Pos()), name)
		}
		return true
	})
}
