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

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseNonTestHubFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, err, "parse %s", name)
		files[name] = f
	}
	require.NotEmpty(t, files)
	return fset, files
}

// TestCopyIntent_ASTGuard enforces that derivedAdmission construction is
// restricted to mintCopyAdmission and copyIntent assignment is restricted to
// deliverAgentMentionCopy.
//
// Note (O-d): this AST guard is defence-in-depth and NOT complete proof against
// arbitrary Go value copying. Per-send registry pointer identity
// (copyAdmissionRegistry.entries[adm]) is the runtime defence.
func TestCopyIntent_ASTGuard(t *testing.T) {
	fset, files := parseNonTestHubFiles(t)
	violations, mintHits, intentHits := inspectCopyIntentAST(fset, files)
	for _, v := range violations {
		t.Error(v)
	}
	assert.Equal(t, 1, mintHits, "mintCopyAdmission must construct derivedAdmission exactly once")
	assert.Equal(t, 1, intentHits, "deliverAgentMentionCopy must assign copyIntent exactly once")
}

// TestCopyIntent_ASTGuard_FixtureForms verifies that every forbidden
// derivedAdmission construction/copy form (O-d) is demonstrably flagged by the
// AST guard.
func TestCopyIntent_ASTGuard_FixtureForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "func_composite_lit",
			src:  `package hub; func forge() *derivedAdmission { return &derivedAdmission{} }`,
		},
		{
			name: "func_new",
			src:  `package hub; func forge() *derivedAdmission { return new(derivedAdmission) }`,
		},
		{
			name: "func_var_value",
			src:  `package hub; func forge() { var x derivedAdmission; _ = x }`,
		},
		{
			name: "pkg_var_value",
			src:  `package hub; var x derivedAdmission`,
		},
		{
			name: "pkg_var_composite_lit",
			src:  `package hub; var x = derivedAdmission{}`,
		},
		{
			name: "pkg_var_new",
			src:  `package hub; var x = new(derivedAdmission)`,
		},
		{
			name: "type_alias",
			src:  `package hub; type T = derivedAdmission; func forge() *T { return &T{} }`,
		},
		{
			name: "make_slice",
			src:  `package hub; func forge() []derivedAdmission { return make([]derivedAdmission, 1) }`,
		},
		{
			name: "array_element_type",
			src:  `package hub; func forge() { var s [1]derivedAdmission; _ = s }`,
		},
		{
			name: "dereference_copy",
			src:  `package hub; func forge(adm *derivedAdmission) { c := *adm; _ = c }`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.name+".go", tc.src, 0)
			require.NoError(t, err)
			violations, _, _ := inspectCopyIntentAST(fset, map[string]*ast.File{tc.name + ".go": f})
			assert.NotEmpty(t, violations, "AST guard must flag %s", tc.name)
		})
	}
}

func exprHasDerivedAdmissionValueType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "derivedAdmission"
	case *ast.ArrayType:
		return exprHasDerivedAdmissionValueType(t.Elt)
	default:
		return false
	}
}

func isDerivedAdmissionPtrType(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "derivedAdmission"
}

func inspectCopyIntentAST(fset *token.FileSet, files map[string]*ast.File) (violations []string, mintHits int, intentHits int) {
	inspectExprs := func(fileName, scopeName string, root ast.Node, ptrVars map[string]bool) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if ident, ok := node.Type.(*ast.Ident); ok && ident.Name == "derivedAdmission" {
					if scopeName != "mintCopyAdmission" {
						violations = append(violations, "derivedAdmission literal constructed outside mintCopyAdmission in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
					} else {
						mintHits++
					}
				}
			case *ast.CallExpr:
				if fun, ok := node.Fun.(*ast.Ident); ok {
					if fun.Name == "new" && len(node.Args) == 1 {
						if exprHasDerivedAdmissionValueType(node.Args[0]) && scopeName != "mintCopyAdmission" {
							violations = append(violations, "new(derivedAdmission) called outside mintCopyAdmission in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
						}
					}
					if fun.Name == "make" && len(node.Args) >= 1 {
						if exprHasDerivedAdmissionValueType(node.Args[0]) && scopeName != "mintCopyAdmission" {
							violations = append(violations, "make([]derivedAdmission, ...) called outside mintCopyAdmission in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
						}
					}
				}
			case *ast.ValueSpec:
				if exprHasDerivedAdmissionValueType(node.Type) && scopeName != "mintCopyAdmission" {
					violations = append(violations, "derivedAdmission value declared outside mintCopyAdmission in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
				}
				if isDerivedAdmissionPtrType(node.Type) {
					for _, name := range node.Names {
						ptrVars[name.Name] = true
					}
				}
			case *ast.TypeSpec:
				if node.Name.Name != "derivedAdmission" && exprHasDerivedAdmissionValueType(node.Type) {
					violations = append(violations, "type alias/definition for derivedAdmission declared in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
				}
			case *ast.StarExpr:
				if scopeName != "mintCopyAdmission" {
					switch x := node.X.(type) {
					case *ast.Ident:
						if ptrVars[x.Name] {
							violations = append(violations, "derivedAdmission dereference copy in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
						}
					case *ast.SelectorExpr:
						if x.Sel.Name == "copyAdmission" || x.Sel.Name == "CopyAdmission" {
							violations = append(violations, "derivedAdmission field dereference copy in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
						}
					}
				}
			case *ast.KeyValueExpr:
				if ident, ok := node.Key.(*ast.Ident); ok && ident.Name == "copyIntent" {
					if scopeName != "deliverAgentMentionCopy" {
						violations = append(violations, "copyIntent field assigned outside deliverAgentMentionCopy in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
					} else {
						intentHits++
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "copyIntent" {
						if scopeName != "deliverAgentMentionCopy" {
							violations = append(violations, "copyIntent assigned outside deliverAgentMentionCopy in "+scopeName+" ("+fileName+") at "+fset.Position(node.Pos()).String())
						} else {
							intentHits++
						}
					}
				}
			}
			return true
		})
	}

	for fileName, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				inspectExprs(fileName, "<package>", d, map[string]bool{})
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				ptrVars := map[string]bool{}
				if d.Recv != nil {
					for _, fld := range d.Recv.List {
						if isDerivedAdmissionPtrType(fld.Type) {
							for _, name := range fld.Names {
								ptrVars[name.Name] = true
							}
						}
					}
				}
				if d.Type != nil && d.Type.Params != nil {
					for _, fld := range d.Type.Params.List {
						if isDerivedAdmissionPtrType(fld.Type) {
							for _, name := range fld.Names {
								ptrVars[name.Name] = true
							}
						}
					}
				}
				inspectExprs(fileName, d.Name.Name, d.Body, ptrVars)
			}
		}
	}
	return violations, mintHits, intentHits
}

func TestSingleResolution_DeliveryPathASTGuard(t *testing.T) {
	fset, files := parseNonTestHubFiles(t)

	forbiddenCalls := map[string]map[string]bool{
		"deliverResolvedAgentMentions": {
			"ResolveMentions":           true,
			"resolveMentionCopyTargets": true,
			"resolveGroupDefaultAgent":  true,
		},
		"deliverAgentMentionCopy": {
			"ResolveMentions":           true,
			"resolveMentionCopyTargets": true,
			"resolveGroupDefaultAgent":  true,
		},
		"wakeGroupDefaultAgent": {
			"resolveGroupDefaultAgent":     true,
			"resolveDefaultWakeCopyTarget": true,
		},
		"handleAgentOutboundMessage": {
			"fanOutAgentMentions": true,
		},
		"handleAgentMessage": {
			"fanOutAgentMentions": true,
		},
	}

	seenFuncs := make(map[string]bool)
	for fileName, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			forbid, watched := forbiddenCalls[fn.Name.Name]
			if !watched {
				continue
			}
			seenFuncs[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var called string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					called = fun.Name
				case *ast.SelectorExpr:
					called = fun.Sel.Name
				}
				if forbid[called] {
					t.Errorf("forbidden re-resolution call %q inside %s (%s) at %s",
						called, fn.Name.Name, fileName, fset.Position(call.Pos()))
				}
				return true
			})
		}
	}

	for fnName := range forbiddenCalls {
		assert.True(t, seenFuncs[fnName], "expected function %q to exist in pkg/hub", fnName)
	}
}

func TestCoverage_PinnedToGatedSites(t *testing.T) {
	cov := messagepolicy.Coverage()
	assert.Equal(t, []string{
		messagepolicy.SurfaceAgentToUser,
		messagepolicy.SurfaceAgentToConversation,
		messagepolicy.SurfaceAgentDM,
		messagepolicy.SurfaceMentionDelivery,
	}, cov.EnforcedSurfaces)

	for _, covered := range cov.EnforcedSurfaces {
		assert.True(t, messagepolicy.IsCoveredSurface(covered), "surface %q must be covered", covered)
	}
	for _, uncovered := range []string{
		messagepolicy.SurfaceGroupPost,
		messagepolicy.SurfaceGroupSet,
		messagepolicy.SurfaceMentionFanout,
		"broadcast",
	} {
		assert.False(t, messagepolicy.IsCoveredSurface(uncovered), "surface %q must stay uncovered", uncovered)
		assert.Contains(t, cov.UncoveredAgentPaths, uncovered)
	}

	_, files := parseNonTestHubFiles(t)

	type siteCheck struct {
		funcName string
		needSel  []string
		found    map[string]bool
	}
	checks := []*siteCheck{
		{funcName: "deriveH2FactSnapshot", needSel: []string{"SurfaceAgentToUser", "SurfaceAgentToConversation"}, found: map[string]bool{}},
		{funcName: "ExecuteAgentDM", needSel: []string{"SurfaceAgentDM", "evaluateOriginMessagePolicies", "verifyCopyAdmissionAtDelivery"}, found: map[string]bool{}},
		{funcName: "handleAgentMessage", needSel: []string{"SurfaceAgentDM", "evaluateOriginMessagePolicies"}, found: map[string]bool{}},
		{funcName: "buildCopyTargetSnapshot", needSel: []string{"SurfaceMentionDelivery"}, found: map[string]bool{}},
	}

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, chk := range checks {
				if fn.Name.Name != chk.funcName {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok {
						for _, want := range chk.needSel {
							if sel.Sel.Name == want {
								chk.found[want] = true
							}
						}
					}
					return true
				})
			}
		}
	}

	for _, chk := range checks {
		for _, want := range chk.needSel {
			assert.True(t, chk.found[want], "gated site %s must reference %s", chk.funcName, want)
		}
	}
}
