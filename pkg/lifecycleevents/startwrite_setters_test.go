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

package lifecycleevents

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// pinnedStartWriteSetters are the hub functions that set
// AgentStatusUpdate.StartWrite, with the number of sites in each (design
// 2.4a E(i), r6 N3). StartWrite exists for the delete-hold rule; the
// lifecycle observer also reads it as start evidence, so a new setter must
// be a start path's own post-dispatch write, or the evidence rule must be
// revisited. Changing this set is a deliberate decision.
var pinnedStartWriteSetters = map[string]int{
	"handlers_agent_lifecycle.go:Server.handleAgentLifecycle": 1, // the start/restart final write
	"start_claim.go:Server.writeStartedStatus":                1,
	"wake_dm.go:Server.wakeAgentForDM":                        2, // the starting write (not E: not running) and the running write
}

// TestStartWriteSettersArePinned parses pkg/hub (non-test files, no
// type-check, so it stays light) and lists every StartWrite: value in a
// composite literal and every x.StartWrite = assignment by enclosing
// function.
func TestStartWriteSettersArePinned(t *testing.T) {
	dir := filepath.Join("..", "hub")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	got := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := name + ":" + fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				rt := fd.Recv.List[0].Type
				if st, ok := rt.(*ast.StarExpr); ok {
					rt = st.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					key = name + ":" + id.Name + "." + fd.Name.Name
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.KeyValueExpr:
					if id, ok := n.Key.(*ast.Ident); ok && id.Name == "StartWrite" {
						got[key]++
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "StartWrite" {
							got[key]++
						}
					}
				}
				return true
			})
		}
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range pinnedStartWriteSetters {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if got[k] != pinnedStartWriteSetters[k] {
			t.Errorf("%s sets StartWrite %d times, pinned %d: a StartWrite setter is start evidence E(i) for the lifecycle observer; review design 2.4a before changing the pinned set", k, got[k], pinnedStartWriteSetters[k])
		}
	}
}
