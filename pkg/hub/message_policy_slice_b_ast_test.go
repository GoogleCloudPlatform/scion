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

func TestCopyIntent_ASTGuard(t *testing.T) {
	fset, files := parseNonTestHubFiles(t)

	var (
		derivedAdmissionMintHits int
		copyIntentAssignHits     int
	)

	for fileName, f := range files {
		for _, decl := range f.Decls {
			// N1: catch package-level `var x derivedAdmission` value declarations.
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						if ident, ok := vs.Type.(*ast.Ident); ok && ident.Name == "derivedAdmission" {
							t.Errorf("package-level derivedAdmission value declared outside mintCopyAdmission in %s at %s",
								fileName, fset.Position(vs.Pos()))
						}
					}
				}
				continue
			}
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcName := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					if ident, ok := node.Type.(*ast.Ident); ok && ident.Name == "derivedAdmission" {
						if funcName != "mintCopyAdmission" {
							t.Errorf("derivedAdmission literal constructed outside mintCopyAdmission in %s (%s) at %s",
								funcName, fileName, fset.Position(node.Pos()))
						} else {
							derivedAdmissionMintHits++
						}
					}
				case *ast.CallExpr:
					// N1: catch `new(derivedAdmission)` outside mintCopyAdmission.
					if fun, ok := node.Fun.(*ast.Ident); ok && fun.Name == "new" && len(node.Args) == 1 {
						if arg, ok := node.Args[0].(*ast.Ident); ok && arg.Name == "derivedAdmission" && funcName != "mintCopyAdmission" {
							t.Errorf("new(derivedAdmission) called outside mintCopyAdmission in %s (%s) at %s",
								funcName, fileName, fset.Position(node.Pos()))
						}
					}
				case *ast.ValueSpec:
					// N1: catch `var x derivedAdmission` value declarations outside mintCopyAdmission.
					if ident, ok := node.Type.(*ast.Ident); ok && ident.Name == "derivedAdmission" && funcName != "mintCopyAdmission" {
						t.Errorf("derivedAdmission value declared outside mintCopyAdmission in %s (%s) at %s",
							funcName, fileName, fset.Position(node.Pos()))
					}
				case *ast.KeyValueExpr:
					if ident, ok := node.Key.(*ast.Ident); ok && ident.Name == "copyIntent" {
						if funcName != "deliverAgentMentionCopy" {
							t.Errorf("copyIntent field assigned outside deliverAgentMentionCopy in %s (%s) at %s",
								funcName, fileName, fset.Position(node.Pos()))
						} else {
							copyIntentAssignHits++
						}
					}
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "copyIntent" {
							if funcName != "deliverAgentMentionCopy" {
								t.Errorf("copyIntent assigned outside deliverAgentMentionCopy in %s (%s) at %s",
									funcName, fileName, fset.Position(node.Pos()))
							} else {
								copyIntentAssignHits++
							}
						}
					}
				}
				return true
			})
		}
	}

	assert.Equal(t, 1, derivedAdmissionMintHits, "mintCopyAdmission must construct derivedAdmission exactly once")
	assert.Equal(t, 1, copyIntentAssignHits, "deliverAgentMentionCopy must assign copyIntent exactly once")
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
