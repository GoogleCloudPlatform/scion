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

package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Kubernetes shared-repository refusal lives in Run, the single
// production caller of buildPod, so every start / restart / wake / reincarnate /
// reconcile path that reaches a pod goes through it. This guard fails if any
// non-test function other than Run calls buildPod, so a new caller cannot skip
// the refusal without either routing through Run or being told to add the check.
func TestBuildPodHasOnlyRunAsCaller(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "buildPod" {
					return true
				}
				if fn.Name.Name != "Run" {
					callers = append(callers, fn.Name.Name+" in "+name)
				}
				return true
			})
		}
	}
	if len(callers) > 0 {
		t.Errorf("buildPod is called outside Run by %v; route the call through Run or apply "+
			"k8sSharedRepoShapeRefusal there too, so the refusal is not skipped", callers)
	}
}
