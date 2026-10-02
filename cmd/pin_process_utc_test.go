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
	"os"
	"sort"
	"strings"
	"testing"
)

// pinProcessUTCAllowedFuncs is the exact set of run functions allowed to
// call the pinProcessUTC seam, as the first statement of their body.
// runServerStart covers `scion server start` (foreground and daemon) and
// `scion runtime-broker start --foreground`. The rest are offline
// store-writing subcommands, including the two hub secret-migrate commands,
// which open the hub database directly.
var pinProcessUTCAllowedFuncs = map[string]bool{
	"runServerStart":        true,
	"runServerMigrate":      true,
	"runMigrateStorage":     true,
	"runServerDMMigration":  true,
	"runServerBackfill":     true,
	"runRecoverAuthz":       true,
	"runSecretMigrate":      true,
	"runSecretMigrateNames": true,
}

// TestPinProcessUTC_CallSitesAreExactlyTheAllowList is a static (AST) test
// that enforces where and how the pinProcessUTC seam may be called, which a
// plain ordering test cannot: ordering alone does not rule out a stray call
// from a CLI command, a PersistentPreRun(E), or a position later than the
// function's first statement.
//
// It parses every non-test cmd/ source file and asserts:
//   - util.PinProcessUTC is referenced exactly once in the whole package,
//     as the value of `var pinProcessUTC = util.PinProcessUTC`, so nothing
//     can bypass the seam (which TestMain disables for this test binary) by
//     calling util.PinProcessUTC() directly;
//   - every call to the pinProcessUTC() seam is the first statement
//     (Body.List[0]) of one of pinProcessUTCAllowedFuncs' functions; and
//   - no call sits inside any *ast.FuncLit (a closure), which catches a
//     PersistentPreRun(E) literal, a cobra RunE literal, or any other
//     closure — the production commands in this package assign RunE to a
//     named top-level function, never a literal, which is what makes the
//     Body.List[0] check meaningful.
//
// A call that satisfies none of these (wrong function, not first statement,
// or inside a closure) is reported as a violation; a function present in
// pinProcessUTCAllowedFuncs with no valid call is reported as missing. Both
// keep this test failing shut: it catches a dropped call as readily as a
// misplaced one.
func TestPinProcessUTC_CallSitesAreExactlyTheAllowList(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading cmd/ directory: %v", err)
	}

	fset := token.NewFileSet()
	seamInitializers := 0
	funcsWithCall := map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)

			if sel, ok := n.(*ast.SelectorExpr); ok && isUtilPinProcessUTCSelector(sel) {
				if isPinProcessUTCSeamInitializer(stack, sel) {
					seamInitializers++
				} else {
					t.Errorf("%s: found a util.PinProcessUTC reference outside the `pinProcessUTC = util.PinProcessUTC` seam initializer; call the pinProcessUTC seam instead so the cmd test binary's TestMain can disable it", name)
				}
				return true
			}

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "pinProcessUTC" {
				return true
			}

			// Walk outward from the call (excluding the call itself, the
			// top of the stack) to find the nearest enclosing *ast.FuncDecl,
			// noting whether a *ast.FuncLit (closure) sits between the call
			// and it -- or between the call and file scope, if it is never
			// inside any FuncDecl at all (e.g. a RunE literal assigned
			// directly in a top-level `var fooCmd = &cobra.Command{...}`).
			var enclosing *ast.FuncDecl
			inFuncLit := false
			for i := len(stack) - 2; i >= 0; i-- {
				if _, ok := stack[i].(*ast.FuncLit); ok {
					inFuncLit = true
				}
				if fd, ok := stack[i].(*ast.FuncDecl); ok {
					enclosing = fd
					break
				}
			}

			switch {
			case inFuncLit:
				where := "<package scope>"
				if enclosing != nil {
					where = enclosing.Name.Name
				}
				t.Errorf("%s: pinProcessUTC() is called inside a closure (context %q); it must be a direct statement of a named function's body, never inside PersistentPreRun(E), a RunE literal, or any other closure", name, where)
			case enclosing == nil:
				t.Errorf("%s: pinProcessUTC() is called outside of any function declaration", name)
			case !pinProcessUTCAllowedFuncs[enclosing.Name.Name]:
				t.Errorf("%s: %s calls pinProcessUTC() but is not in pinProcessUTCAllowedFuncs; a CLI command must never call it, and a new offline store-writing command must be added to the allow-list", name, enclosing.Name.Name)
			default:
				exprStmt, ok := stack[len(stack)-2].(*ast.ExprStmt)
				isFirst := ok && len(enclosing.Body.List) > 0 && enclosing.Body.List[0] == ast.Stmt(exprStmt)
				if !isFirst {
					t.Errorf("%s: %s calls pinProcessUTC() but not as the first statement of its body", name, enclosing.Name.Name)
					break
				}
				funcsWithCall[enclosing.Name.Name] = true
			}
			return true
		})
	}

	if seamInitializers != 1 {
		t.Errorf("expected exactly one `pinProcessUTC = util.PinProcessUTC` seam initializer in cmd/, found %d", seamInitializers)
	}

	var missing []string
	for name := range pinProcessUTCAllowedFuncs {
		if !funcsWithCall[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("%s is in pinProcessUTCAllowedFuncs but cmd/ has no valid pinProcessUTC() call in it (renamed, removed, or the call was dropped or misplaced?)", name)
	}
}

// isUtilPinProcessUTCSelector reports whether sel is the selector expression
// util.PinProcessUTC (as a call's Fun or as a bare value reference).
func isUtilPinProcessUTCSelector(sel *ast.SelectorExpr) bool {
	if sel.Sel.Name != "PinProcessUTC" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "util"
}

// isPinProcessUTCSeamInitializer reports whether sel, whose immediate parent
// in stack is an *ast.ValueSpec, is the value of
// `var pinProcessUTC = util.PinProcessUTC`.
func isPinProcessUTCSeamInitializer(stack []ast.Node, sel *ast.SelectorExpr) bool {
	if len(stack) < 2 {
		return false
	}
	vs, ok := stack[len(stack)-2].(*ast.ValueSpec)
	if !ok {
		return false
	}
	for i, v := range vs.Values {
		if v == ast.Expr(sel) && i < len(vs.Names) && vs.Names[i].Name == "pinProcessUTC" {
			return true
		}
	}
	return false
}
