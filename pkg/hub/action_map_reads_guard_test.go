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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Guard for ptone/scion#4943: every read of the ResourceActions and
// ScopeActions maps in a non-test file of pkg/hub (and of the
// guardScanSubdirs packages) is in a function listed in
// actionMapReadAllowList, with the pinned number of reads. The capability
// helpers are listed, so the action-map check of
// TestResourceTypeLiterals_AllInRegistry covers the types they read; any
// other direct read is listed with a reason.
//
// Scope: non-test files of pkg/hub and guardScanSubdirs only; test files
// read the maps to state expected actions and are not scanned.

// actionMapNames are the package-level maps the guard tracks.
var actionMapNames = map[string]bool{"ResourceActions": true, "ScopeActions": true}

// actionMapAllow is one allow-list entry: the number of reads of the map
// in the function, and why the function reads the map directly.
type actionMapAllow struct {
	reads  int
	reason string
}

// actionMapReadAllowList lists, as "function map", each function that
// reads an action map directly. A method is written (*T).M or T.M. Each
// entry must match exactly its pinned number of reads, so an entry with no
// read fails and the list cannot go stale.
var actionMapReadAllowList = map[string]actionMapAllow{
	"(*AuthzService).ComputeCapabilities ResourceActions": {1,
		"capability helper: the actions for one resource"},
	"(*AuthzService).ComputeScopeCapabilities ScopeActions": {1,
		"capability helper: the scope actions for a resource type"},
	"(*AuthzService).ComputeCapabilitiesBatch ResourceActions": {1,
		"capability helper: the actions for a list of resources"},
	"(*Server).listProjectAgentsSorted ResourceActions": {1,
		"splits the agent actions into read and the rest for ComputeCapabilitiesForActions"},
	"(*Server).projectGCPServiceAccountCapabilities ResourceActions": {1,
		"action order for overrideCapabilities on project-scoped service accounts"},
	"(*Server).projectGCPServiceAccountCapabilities ScopeActions": {1,
		"action order for overrideCapabilities on a project's service account collection"},
}

// actionMapRef is one reference to an action map name.
type actionMapRef struct {
	pos   string // file:line
	place string // function, "closure in F" or "package level"
	name  string // ResourceActions or ScopeActions
	// kind is index, range, len or value for a read, or shadow for a
	// declaration of the name other than the package-level map itself.
	kind string
}

// funcPlace names a function declaration as (*T).M, T.M or F.
func funcPlace(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := types.ExprString(fd.Recv.List[0].Type)
	if strings.HasPrefix(recv, "*") {
		recv = "(" + recv + ")"
	}
	return recv + "." + fd.Name.Name
}

// scanActionMapRefs returns every reference to an action map name in
// files, keyed by file name. Only bare identifiers count: a selector such
// as x.ResourceActions names a field or another package's symbol, and
// these packages cannot import package hub. The package-level var
// declarations of the maps in package hub (files keyed without a
// directory) are definitions, not reads. Any other declaration of either
// name is reported as kind shadow, and reads through it still count, so a
// local or parameter of the same name cannot hide a read.
func scanActionMapRefs(fset *token.FileSet, files map[string]*ast.File) []actionMapRef {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var refs []actionMapRef
	for _, name := range names {
		f := files[name]
		defs := map[*ast.Ident]bool{}
		if path.Dir(name) == "." {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					for _, id := range spec.(*ast.ValueSpec).Names {
						if actionMapNames[id.Name] {
							defs[id] = true
						}
					}
				}
			}
		}
		isMap := func(e ast.Expr) *ast.Ident {
			if id, ok := e.(*ast.Ident); ok && actionMapNames[id.Name] {
				return id
			}
			return nil
		}
		// Pass 1: the kind of each read in an index, range or len
		// form. Parentheses around the map do not hide the form.
		kinds := map[*ast.Ident]string{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.IndexExpr:
				if id := isMap(ast.Unparen(n.X)); id != nil {
					kinds[id] = "index"
				}
			case *ast.RangeStmt:
				if id := isMap(ast.Unparen(n.X)); id != nil {
					kinds[id] = "range"
				}
			case *ast.CallExpr:
				if fn, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && fn.Name == "len" && len(n.Args) == 1 {
					if id := isMap(ast.Unparen(n.Args[0])); id != nil {
						kinds[id] = "len"
					}
				}
			}
			return true
		})
		// Pass 2: every identifier with its place. Any other use is a
		// value read (an alias, an argument, a return value).
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			id, ok := n.(*ast.Ident)
			if !ok || !actionMapNames[id.Name] || defs[id] {
				return true
			}
			if len(stack) >= 2 {
				if sel, ok := stack[len(stack)-2].(*ast.SelectorExpr); ok && sel.Sel == id {
					return true
				}
			}
			place := "package level"
			for i := len(stack) - 2; i >= 0; i-- {
				if _, ok := stack[i].(*ast.FuncLit); ok {
					place = "closure in package level"
					for j := i - 1; j >= 0; j-- {
						if fd, ok := stack[j].(*ast.FuncDecl); ok {
							place = "closure in " + funcPlace(fd)
							break
						}
					}
					break
				}
				if fd, ok := stack[i].(*ast.FuncDecl); ok {
					place = funcPlace(fd)
					break
				}
			}
			kind := kinds[id]
			switch {
			case id.Obj != nil && id.Obj.Pos() == id.Pos():
				kind = "shadow"
			case kind == "":
				kind = "value"
			}
			refs = append(refs, actionMapRef{
				pos:   fmt.Sprintf("%s:%d", name, fset.Position(id.Pos()).Line),
				place: place,
				name:  id.Name,
				kind:  kind,
			})
			return true
		})
	}
	return refs
}

// checkActionMapRefs returns, sorted, each problem with refs against
// allow: a declaration that shadows a map name, a read in a place with no
// entry, an entry with no reason, an entry whose read count differs, and
// an entry with no read.
func checkActionMapRefs(refs []actionMapRef, allow map[string]actionMapAllow) []string {
	var problems []string
	reads := map[string]int{}
	for _, r := range refs {
		if r.kind == "shadow" {
			problems = append(problems, fmt.Sprintf("%s: %s %s: declares a name that shadows the package-level map", r.pos, r.place, r.name))
			continue
		}
		key := r.place + " " + r.name
		if _, ok := allow[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %s %s %s: not in the allow list", r.pos, r.place, r.name, r.kind))
			continue
		}
		reads[key]++
	}
	for key, a := range allow {
		if strings.TrimSpace(a.reason) == "" {
			problems = append(problems, fmt.Sprintf("%s: allow-list entry has no reason", key))
		}
		switch n := reads[key]; {
		case n == 0:
			problems = append(problems, fmt.Sprintf("%s: allow-list entry matches no read", key))
		case n != a.reads:
			problems = append(problems, fmt.Sprintf("%s: %d reads, allow list pins %d", key, n, a.reads))
		}
	}
	sort.Strings(problems)
	return problems
}

// TestActionMapReads_OnlyInAllowList fails when a non-test file in pkg/hub
// or a guardScanSubdirs package reads ResourceActions or ScopeActions
// outside actionMapReadAllowList, or when an entry's read count differs.
func TestActionMapReads_OnlyInAllowList(t *testing.T) {
	fset := token.NewFileSet()
	files := parseGoDir(t, fset, findHubDir(t), true)
	refs := scanActionMapRefs(fset, files)
	require.NotEmpty(t, refs, "scanner found no action map reads; the matcher is broken")

	counts := map[string]int{}
	for _, r := range refs {
		counts[r.kind]++
	}
	t.Logf("action map reads: total=%d index=%d range=%d len=%d value=%d shadow=%d",
		len(refs), counts["index"], counts["range"], counts["len"], counts["value"], counts["shadow"])

	assert.Empty(t, checkActionMapRefs(refs, actionMapReadAllowList),
		"a non-test file reads ResourceActions or ScopeActions outside "+
			"actionMapReadAllowList. Use the capability helpers "+
			"(ComputeCapabilities, ComputeCapabilitiesBatch, "+
			"ComputeScopeCapabilities), or add the function to the allow "+
			"list with its read count and a reason")
}

// TestActionMapReads_ReportsUnlistedAndStale proves the guard reports each
// form of read outside the allow list, a closure, a package-level
// initialiser, a shadowing declaration, an entry with no reason, a count
// mismatch and a stale
// entry, so a passing TestActionMapReads_OnlyInAllowList is not a scanner
// that matches nothing.
func TestActionMapReads_ReportsUnlistedAndStale(t *testing.T) {
	const src = `package hub

var ResourceActions = map[string][]Action{}

var ScopeActions = map[string][]Action{}

var initRead = len(ResourceActions)

func allowed() {
	_ = ResourceActions["agent"]
	_ = permissions.ResourceActions()
	_ = func() { _ = ScopeActions["agent"] }
}

func twice() {
	_ = ScopeActions["a"]
	_ = (ScopeActions)["b"]
}

func (s *Server) unlisted() {
	for range ResourceActions {
	}
	_ = len((ScopeActions))
	use(ResourceActions)
	m := ResourceActions
	_ = m
}

func shadowParam(ResourceActions map[string][]Action) {
	_ = ResourceActions["x"]
}

func shadowLocal() {
	ScopeActions := 1
	_ = ScopeActions
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	refs := scanActionMapRefs(fset, map[string]*ast.File{"sample.go": f})
	allow := map[string]actionMapAllow{
		"allowed ResourceActions": {1, "listed"},
		"twice ScopeActions":      {1, "pinned below its count"},
		"stale ScopeActions":      {1, "no such read"},
		"unlisted ScopeActions":   {1, " "},
	}
	assert.Equal(t, []string{
		"sample.go:12: closure in allowed ScopeActions index: not in the allow list",
		"sample.go:21: (*Server).unlisted ResourceActions range: not in the allow list",
		"sample.go:23: (*Server).unlisted ScopeActions len: not in the allow list",
		"sample.go:24: (*Server).unlisted ResourceActions value: not in the allow list",
		"sample.go:25: (*Server).unlisted ResourceActions value: not in the allow list",
		"sample.go:29: shadowParam ResourceActions: declares a name that shadows the package-level map",
		"sample.go:30: shadowParam ResourceActions index: not in the allow list",
		"sample.go:34: shadowLocal ScopeActions: declares a name that shadows the package-level map",
		"sample.go:35: shadowLocal ScopeActions value: not in the allow list",
		"sample.go:7: package level ResourceActions len: not in the allow list",
		"stale ScopeActions: allow-list entry matches no read",
		"twice ScopeActions: 2 reads, allow list pins 1",
		"unlisted ScopeActions: allow-list entry has no reason",
		"unlisted ScopeActions: allow-list entry matches no read",
	}, checkActionMapRefs(refs, allow))

	kinds := map[string]string{}
	for _, r := range refs {
		kinds[r.pos] = r.kind
	}
	assert.Equal(t, "index", kinds["sample.go:17"],
		"a parenthesised index read must be kind index")
}
