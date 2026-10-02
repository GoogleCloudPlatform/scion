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
// This test closes that gap directly: it asserts every call to
// applyRolePlanTx in non-test pkg/hub code is textually inside a function
// named SetMemberRoles. It is in its own new file, not rs1_extended_test.go
// or any other rs*/d002*/pm1* file, per the brief.
// ---------------------------------------------------------------------------

const applyRolePlanTxSymbol = "applyRolePlanTx"

// applyRolePlanTxAllowedCaller is the only function permitted to call
// applyRolePlanTx today.
const applyRolePlanTxAllowedCaller = "SetMemberRoles"

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

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if extractCallSymbol(call) != applyRolePlanTxSymbol {
				return true
			}
			pos := fset.Position(call.Pos())
			funcName := enclosingFuncName(fset, f, pos.Offset)
			// Exclude the method declaration itself (its body does not
			// call itself, but ast.Inspect also visits the FuncDecl.Name
			// identifier only as a declaration, not a CallExpr, so this
			// branch only ever sees genuine call sites).
			sites = append(sites, effectCallSite{
				file:     relPath,
				function: funcName,
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
		t.Fatal("found zero calls to applyRolePlanTx — the scanner is broken or the function has been renamed")
	}

	for _, site := range sites {
		if site.function != applyRolePlanTxAllowedCaller {
			t.Errorf("UNEXPECTED caller of applyRolePlanTx: %s:%d, inside %s (want: only %s)\n"+
				"applyRolePlanTx is the one purpose-named step SetMemberRoles uses to apply a\n"+
				"rolePlan's delete-then-create inside a transaction; the authzop/catalog.go\n"+
				"ExemptionInternalOnly entries for it, and TestMutationClassificationBidirectional/\n"+
				"TestRS1_AST_BypassPathsDocumented, all assume exactly one caller. If a second\n"+
				"caller is intentional, it needs its own governance review and catalog entries.",
				site.file, site.line, site.function, applyRolePlanTxAllowedCaller)
		}
	}

	t.Logf("Verified %d call(s) to applyRolePlanTx, all inside %s", len(sites), applyRolePlanTxAllowedCaller)
}
