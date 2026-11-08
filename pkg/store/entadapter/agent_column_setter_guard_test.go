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

package entadapter

import (
	"fmt"
	"go/ast"
	"go/types"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// singleWriterColumns maps each single-writer agents column (WP1 slice 3
// addendum v12 section 1) to its ent setter family and the only function
// ("file:Recv.Method") allowed to call it. The family covers every ent
// type in pkg/ent that has the method: the create, update and update-one
// builders, the bulk and upsert builders, and the mutation.
var singleWriterColumns = map[string]struct {
	setters []string
	writer  string
}{
	"effective_config": {
		setters: []string{"SetEffectiveConfig", "SetNillableEffectiveConfig", "ClearEffectiveConfig", "UpdateEffectiveConfig"},
		writer:  "effective_config_store.go:AgentStore.SetAgentEffectiveConfig",
	},
	"launch_run_id": {
		setters: []string{"SetLaunchRunID", "SetNillableLaunchRunID", "ClearLaunchRunID", "UpdateLaunchRunID"},
		writer:  "launch_store.go:AgentStore.beginLaunch",
	},
}

// singleWriterSetterCalls returns, for each column, the functions that
// call a setter of its family on an ent type, with the call positions.
func singleWriterSetterCalls(t *testing.T, patterns ...string) map[string]map[string][]string {
	t.Helper()
	setterColumn := map[string]string{}
	for col, c := range singleWriterColumns {
		for _, s := range c.setters {
			setterColumn[s] = col
		}
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  ".",
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("load %v: %v", patterns, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("%d package load errors", n)
	}
	out := map[string]map[string][]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := funcKey(pkg.Fset, fd)
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					col, ok := setterColumn[sel.Sel.Name]
					if !ok {
						return true
					}
					xt := pkg.TypesInfo.TypeOf(sel.X)
					if p, ok := xt.(*types.Pointer); ok {
						xt = p.Elem()
					}
					named, ok := xt.(*types.Named)
					if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != entPkgPath {
						return true
					}
					if out[col] == nil {
						out[col] = map[string][]string{}
					}
					out[col][key] = append(out[col][key], fmt.Sprintf("%s.%s at %s", named.Obj().Name(), sel.Sel.Name, pkg.Fset.Position(sel.Pos())))
					return true
				})
			}
		}
	}
	return out
}

// TestAgentColumnSetterGuard: only SetAgentEffectiveConfig writes
// agents.effective_config, and only beginLaunch writes
// agents.launch_run_id (addendum v12 section 1; review r1 N4).
func TestAgentColumnSetterGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks pkg/store and pkg/store/entadapter")
	}
	calls := singleWriterSetterCalls(t, guardedPackages...)
	cols := make([]string, 0, len(singleWriterColumns))
	for col := range singleWriterColumns {
		cols = append(cols, col)
	}
	sort.Strings(cols)
	for _, col := range cols {
		want := singleWriterColumns[col].writer
		if len(calls[col][want]) == 0 {
			t.Errorf("%s: its writer %s calls no setter of the family; the guard or the writer is broken", col, want)
		}
		for fn, sites := range calls[col] {
			if fn != want {
				t.Errorf("%s is written outside %s: %s (%s)", col, want, fn, strings.Join(sites, "; "))
			}
		}
	}
}

// TestAgentColumnSetterGuardProof: the guard reports a setter call on an
// ent builder outside the writer.
func TestAgentColumnSetterGuardProof(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks a testdata package")
	}
	calls := singleWriterSetterCalls(t, "./testdata/writerguardproof")
	for col, want := range map[string]string{
		"effective_config": "AgentUpdateOne.SetEffectiveConfig",
		"launch_run_id":    "AgentUpdateOne.ClearLaunchRunID",
	} {
		sites := strings.Join(calls[col]["proof.go:setSingleWriterColumns"], "; ")
		if !strings.Contains(sites, want) {
			t.Errorf("%s: guard missed %s in proof.go:setSingleWriterColumns; got %q", col, want, sites)
		}
	}
}
