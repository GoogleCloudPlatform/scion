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

// Command checkagentmutators is the module-wide half of the WP1 agent
// writer guard (design 2.8, r2 N5, r3 N5): no package outside pkg/store/...
// and pkg/ent/... may mutate agent rows directly, because only the store's
// writers run the lifecycle observer. It type-checks every package that
// imports pkg/ent (non-test files) and reports each call of a mutating
// method (Create, CreateBulk, Update, UpdateOne, UpdateOneID, Delete,
// DeleteOne, DeleteOneID) on a *ent.AgentClient, and each call of
// (*ent.Agent).Update. The per-writer classification inside pkg/store is
// TestAgentWritersAreClassified; this check only keeps writers from
// appearing elsewhere.
//
// Exit codes: 0 no finding, 1 findings, 2 could not analyse (load or type
// errors). It runs from make ci-full (hack/check-agent-mutators.sh), not
// as a unit test, because it type-checks pkg/hub.
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	modulePath = "github.com/GoogleCloudPlatform/scion"
	entPkg     = modulePath + "/pkg/ent"
)

var mutators = map[string]bool{
	"Create": true, "CreateBulk": true, "Update": true, "UpdateOne": true,
	"UpdateOneID": true, "Delete": true, "DeleteOne": true, "DeleteOneID": true,
}

// allowed reports whether pkgPath may mutate agents: the store and the
// generated ent packages.
func allowed(pkgPath string) bool {
	for _, p := range []string{modulePath + "/pkg/store", entPkg} {
		if pkgPath == p || strings.HasPrefix(pkgPath, p+"/") {
			return true
		}
	}
	return false
}

func isNamed(t types.Type, pkgPath, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkgPath && n.Obj().Name() == name
}

// findings returns "file:line: what" for every agent mutation in pkgs.
func findings(pkgs []*packages.Package) []string {
	var out []string
	for _, pkg := range pkgs {
		if allowed(pkg.PkgPath) {
			continue
		}
		for _, f := range pkg.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !mutators[sel.Sel.Name] {
					return true
				}
				xt := pkg.TypesInfo.TypeOf(sel.X)
				if xt == nil {
					return true
				}
				var what string
				switch {
				case isNamed(xt, entPkg, "AgentClient"):
					what = "AgentClient." + sel.Sel.Name
				case sel.Sel.Name == "Update" && isNamed(xt, entPkg, "Agent"):
					what = "Agent.Update"
				default:
					return true
				}
				out = append(out, fmt.Sprintf("%s: %s in %s", pkg.Fset.Position(call.Pos()), what, pkg.PkgPath))
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// importsEnt reports whether pkg imports pkg/ent directly.
func importsEnt(pkg *packages.Package) bool {
	_, ok := pkg.Imports[entPkg]
	return ok
}

// load type-checks the packages matching patterns that import pkg/ent.
func load(dir string, patterns ...string) ([]*packages.Package, error) {
	list, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedImports, Dir: dir}, patterns...)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, p := range list {
		if importsEnt(p) && !allowed(p.PkgPath) {
			targets = append(targets, p.PkgPath)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  dir,
		Fset: token.NewFileSet(),
	}, targets...)
	if err != nil {
		return nil, err
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return nil, fmt.Errorf("%d package errors", n)
	}
	return pkgs, nil
}

func main() {
	patterns := os.Args[1:]
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	pkgs, err := load(".", patterns...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ABORT: check-agent-mutators could not analyse: %v\n", err)
		os.Exit(2)
	}
	fs := findings(pkgs)
	for _, f := range fs {
		fmt.Println(f)
	}
	if len(fs) > 0 {
		fmt.Fprintf(os.Stderr, "check-agent-mutators: %d agent mutation(s) outside pkg/store; route them through a store method that runs the lifecycle observer (WP1 design 2.8)\n", len(fs))
		os.Exit(1)
	}
	fmt.Printf("check-agent-mutators: %d package(s) importing pkg/ent checked, no agent mutation outside pkg/store\n", len(pkgs))
}
