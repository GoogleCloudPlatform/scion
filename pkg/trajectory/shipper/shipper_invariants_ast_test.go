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
//     - GenerationStartOffset and Generation may only be written in startGenerationLocked.
//  2. The run baseline is write-once:
//     - BaselineSet, BaselineOffset, BaselineGuardHash, and BaselineGuardLen
//     may only be written in setBaselineOnceLocked.
//
// The check inspects assignments (=, +=, ...), IncDecStmt (++, --), keyed
// composite literals, address-of (&x.Field) escapes on guarded fields, and
// whole-struct dereference assignments (*x = ...) outside UnmarshalJSON.
func TestStructuralOwnershipRules(t *testing.T) {
	allowedWriters := map[string]map[string]bool{
		"RawCommittedOffset": {
			"commitSegmentLocked":   true,
			"startGenerationLocked": true,
		},
		"GenerationStartOffset": {
			"startGenerationLocked": true,
		},
		"Generation": {
			"startGenerationLocked": true,
		},
		"BaselineSet": {
			"setBaselineOnceLocked": true,
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
			nonArtifactPtrs := collectNonArtifactStatePtrs(fn)

			checkSelector := func(sel *ast.SelectorExpr, kind string) {
				if allowed, guarded := allowedWriters[sel.Sel.Name]; guarded && !allowed[fnName] {
					pos := fset.Position(sel.Pos())
					t.Errorf("%s: forbidden %s on %s inside %s (structural ownership rule)", pos, kind, sel.Sel.Name, fnName)
				}
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						switch target := lhs.(type) {
						case *ast.SelectorExpr:
							checkSelector(target, "assignment")
						case *ast.StarExpr:
							// Flag whole-struct deref assignments (*e = *o) outside UnmarshalJSON,
							// allowing only identifiers explicitly typed as non-ArtifactState pointers
							// (e.g. *int64, *[]RefusedPath).
							if fnName == "UnmarshalJSON" {
								break
							}
							ident, isIdent := target.X.(*ast.Ident)
							if !isIdent || !nonArtifactPtrs[ident.Name] {
								pos := fset.Position(target.Pos())
								t.Errorf("%s: forbidden pointer-dereference assignment inside %s (may overwrite ArtifactState outside designated writers)", pos, fnName)
							}
						}
					}
				case *ast.IncDecStmt:
					if sel, ok := node.X.(*ast.SelectorExpr); ok {
						checkSelector(sel, "inc/dec")
					}
				case *ast.UnaryExpr:
					if node.Op == token.AND {
						if sel, ok := node.X.(*ast.SelectorExpr); ok {
							checkSelector(sel, "address-of (&)")
						}
					}
				case *ast.CompositeLit:
					// Only inspect ArtifactState composite literals (other structs such as
					// Segment, SegmentParams, and ArtifactKey legitimately carry a
					// read-only Generation field).
					litIdent, isIdent := node.Type.(*ast.Ident)
					if !isIdent || litIdent.Name != "ArtifactState" {
						break
					}
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

// collectNonArtifactStatePtrs returns the names of receivers and parameters of
// fn whose explicit type is a pointer to something other than ArtifactState.
func collectNonArtifactStatePtrs(fn *ast.FuncDecl) map[string]bool {
	out := make(map[string]bool)
	recordList := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			star, ok := field.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "ArtifactState" {
				continue
			}
			for _, name := range field.Names {
				out[name.Name] = true
			}
		}
	}
	recordList(fn.Recv)
	if fn.Type != nil {
		recordList(fn.Type.Params)
	}
	return out
}
