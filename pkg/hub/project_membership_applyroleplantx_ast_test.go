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
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Review r1 (A-path) A-O1: applyRolePlanTx single-caller guard.
//
// applyRolePlanTx (project_membership_service.go) is the one purpose-named
// step SetMemberRoles (project_membership_set.go) uses to apply a rolePlan's
// delete-then-create inside a transaction, which is what lets the two
// authzop/catalog.go ExemptionInternalOnly entries for it describe a single,
// governed call site. Both TestMutationClassificationBidirectional (catalog
// entries vs. discovered CreateRoleBinding/DeleteRoleBinding call sites) and
// TestRS1_AST_BypassPathsDocumented (direct CreateRoleBinding/DeleteRoleBinding
// callers) key off the CreateRoleBinding/DeleteRoleBinding calls INSIDE
// applyRolePlanTx, not off calls TO applyRolePlanTx — so a second caller of
// applyRolePlanTx would pass both of those guards silently, while making the
// catalog's "step for SetMemberRoles" exemption reason wrong.
//
// This test closes that gap directly: it asserts every reference to
// applyRolePlanTx in non-test pkg/hub code (call, method value or method
// expression) is textually inside the (*ProjectMembershipService).
// SetMemberRoles method. It is in its own new file, not rs1_extended_test.go
// or any other rs*/d002*/pm1* file, per the brief.
// ---------------------------------------------------------------------------

const applyRolePlanTxSymbol = "applyRolePlanTx"

// applyRolePlanTxAllowedCaller is the only function permitted to reference
// applyRolePlanTx today: the SetMemberRoles method on
// *ProjectMembershipService, rendered as applyRolePlanTxEnclosingCaller
// renders it.
const applyRolePlanTxAllowedCaller = "(*ProjectMembershipService).SetMemberRoles"

// applyRolePlanTxEnclosingCaller returns the function whose body contains
// offset, qualified by its receiver type ("(*T).Name" or "(T).Name") for a
// method, so a same-named function or a method on another receiver never
// matches applyRolePlanTxAllowedCaller (review r5 R5-3). A reference
// outside any function body (e.g. a package-level var) yields "<unknown>".
func applyRolePlanTxEnclosingCaller(fset *token.FileSet, f *ast.File, offset int) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Pos()).Offset
		end := fset.Position(fn.Body.End()).Offset
		if offset < start || offset > end {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) != 1 {
			return fn.Name.Name
		}
		switch rt := fn.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if id, ok := rt.X.(*ast.Ident); ok {
				return "(*" + id.Name + ")." + fn.Name.Name
			}
		case *ast.Ident:
			return "(" + rt.Name + ")." + fn.Name.Name
		}
		return "<unrecognized receiver>." + fn.Name.Name
	}
	return "<unknown>"
}

func TestApplyRolePlanTx_OnlyCalledFromSetMemberRoles(t *testing.T) {
	hubDir := findHubDir(t)
	var sites []effectCallSite

	err := filepath.Walk(hubDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "testdata" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}

		relPath, _ := filepath.Rel(hubDir, path)
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("failed to parse %s: %v", relPath, parseErr)
		}

		// Every declaration NAME of applyRolePlanTx (the method's own
		// FuncDecl.Name) is a definition, not a reference; collect them so
		// the reference scan below can skip exactly those identifiers.
		declNames := map[*ast.Ident]bool{}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == applyRolePlanTxSymbol {
				declNames[fn.Name] = true
			}
		}

		// Every other identifier named applyRolePlanTx is a reference: the
		// Sel of a call (svc.applyRolePlanTx(...)), of a method value
		// (fn := svc.applyRolePlanTx) or of a method expression
		// ((*ProjectMembershipService).applyRolePlanTx), or a bare
		// identifier. Matching the *ast.Ident (rather than only a
		// *ast.CallExpr's callee) catches a reference that escapes
		// SetMemberRoles without being called there (review r5 R5-3).
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != applyRolePlanTxSymbol || declNames[id] {
				return true
			}
			pos := fset.Position(id.Pos())
			sites = append(sites, effectCallSite{
				file:     relPath,
				function: applyRolePlanTxEnclosingCaller(fset, f, pos.Offset),
				symbol:   applyRolePlanTxSymbol,
				line:     pos.Line,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk hub directory: %v", err)
	}

	if len(sites) == 0 {
		t.Fatal("found zero references to applyRolePlanTx — the scanner is broken or the function has been renamed")
	}

	for _, site := range sites {
		if site.function != applyRolePlanTxAllowedCaller {
			t.Errorf("UNEXPECTED reference to applyRolePlanTx: %s:%d, inside %s (want: only %s)\n"+
				"applyRolePlanTx is the one purpose-named step SetMemberRoles uses to apply a\n"+
				"rolePlan's delete-then-create inside a transaction; the authzop/catalog.go\n"+
				"ExemptionInternalOnly entries for it, and TestMutationClassificationBidirectional/\n"+
				"TestRS1_AST_BypassPathsDocumented, all assume exactly one caller. If a second\n"+
				"caller is intentional, it needs its own governance review and catalog entries.",
				site.file, site.line, site.function, applyRolePlanTxAllowedCaller)
		}
	}

	if !t.Failed() {
		t.Logf("Verified %d reference(s) to applyRolePlanTx, all inside %s", len(sites), applyRolePlanTxAllowedCaller)
	}
}
