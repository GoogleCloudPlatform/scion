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

package shipper

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStructuralOwnershipRules statically enforces the two shipper structural
// invariants across all non-test Go files in pkg/trajectory/shipper:
//
//  1. Generations are append-only:
//     - RawCommittedOffset may only be written in commitSegmentLocked or startGenerationLocked.
//     - GenerationStartOffset may only be written in startGenerationLocked.
//  2. The run baseline is write-once:
//     - BaselineOffset, BaselineGuardHash, and BaselineGuardLen may only be written in setBaselineOnceLocked.
func TestStructuralOwnershipRules(t *testing.T) {
	allowedWriters := map[string]map[string]bool{
		"RawCommittedOffset": {
			"commitSegmentLocked":   true,
			"startGenerationLocked": true,
		},
		"GenerationStartOffset": {
			"startGenerationLocked": true,
		},
		"BaselineOffset": {
			"setBaselineOnceLocked": true,
		},
		"BaselineGuardHash": {
			"setBaselineOnceLocked": true,
		},
		"BaselineGuardLen": {
			"setBaselineOnceLocked": true,
		},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	fset := token.NewFileSet()
	filesChecked := 0
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		filesChecked++
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fnName := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok {
							continue
						}
						if allowed, guarded := allowedWriters[sel.Sel.Name]; guarded && !allowed[fnName] {
							pos := fset.Position(sel.Pos())
							t.Errorf("%s: forbidden assignment to %s inside %s (structural ownership rule)", pos, sel.Sel.Name, fnName)
						}
					}
				case *ast.CompositeLit:
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						ident, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						if allowed, guarded := allowedWriters[ident.Name]; guarded && !allowed[fnName] {
							pos := fset.Position(ident.Pos())
							t.Errorf("%s: forbidden composite literal field %s inside %s (structural ownership rule)", pos, ident.Name, fnName)
						}
					}
				}
				return true
			})
		}
	}
	if filesChecked == 0 {
		t.Fatal("expected to check at least one non-test .go file")
	}
}
