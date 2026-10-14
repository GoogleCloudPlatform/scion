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

package authzop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// writtenEventTypePattern matches an audit EventType that names a single
// mutation type (for example "role_binding_create"). Rows whose EventType
// matches it must name a mutation type that pkg/hub writes.
var writtenEventTypePattern = regexp.MustCompile(`^[a-z]+(_[a-z]+)+$`)

// writtenEventTypeRows lists rows whose EventType must name a mutation type
// that pkg/hub writes, whatever its form. A row listed here with a dotted or
// otherwise unwritten EventType fails TestCatalogEventTypesAreWritten.
var writtenEventTypeRows = []OperationID{
	"role.binding.create",
	"role.binding.delete",
	"group.member.add",
	"group.member.remove",
	"project.membership.add",
	"project.membership.update",
	"project.membership.remove",
	"project.membership.transfer",
}

// TestCatalogEventTypesAreWritten pins catalog AuditObligation EventType
// values to the mutation types pkg/hub writes. It fails when a row in
// writtenEventTypeRows, or any row whose EventType names a single mutation
// type, lists a string that no pkg/hub writer sets as
// store.MutationAuditRecord.MutationType.
func TestCatalogEventTypesAreWritten(t *testing.T) {
	written := hubWrittenMutationTypes(t)

	required := make(map[OperationID]bool, len(writtenEventTypeRows))
	for _, id := range writtenEventTypeRows {
		required[id] = true
	}

	seen := make(map[OperationID]bool, len(writtenEventTypeRows))
	for _, spec := range Catalog {
		if spec.AuditObligation == nil {
			if required[spec.ID] {
				t.Errorf("%s: listed in writtenEventTypeRows but has no audit obligation", spec.ID)
				seen[spec.ID] = true
			}
			continue
		}
		eventType := spec.AuditObligation.EventType
		if !required[spec.ID] && !writtenEventTypePattern.MatchString(eventType) {
			continue
		}
		seen[spec.ID] = true
		if !written[eventType] {
			t.Errorf("%s: audit EventType %q is not a mutation type written in pkg/hub", spec.ID, eventType)
		}
	}
	for _, id := range writtenEventTypeRows {
		if !seen[id] {
			t.Errorf("%s: listed in writtenEventTypeRows but not in the catalog", id)
		}
	}
}

// TestHubWrittenMutationTypesResolvesKnownWriters checks that the source
// scan finds mutation types written through each supported form, so that
// TestCatalogEventTypesAreWritten cannot pass on an empty or partial set.
func TestHubWrittenMutationTypesResolvesKnownWriters(t *testing.T) {
	written := hubWrittenMutationTypes(t)
	for _, want := range []string{
		"role_binding_create",           // string literal in a composite literal
		"agent_delegation_grant_create", // package-level constant
		"project_visitor_grant",         // constant passed to a writer's parameter
	} {
		if !written[want] {
			t.Errorf("source scan did not find mutation type %q", want)
		}
	}
	for mt := range written {
		if strings.Contains(mt, ".") {
			t.Errorf("source scan returned dotted mutation type %q", mt)
		}
	}
}

// hubWrittenMutationTypes returns the mutation types that non-test pkg/hub
// code sets as MutationType, in a composite literal (MutationType: x) or an
// assignment (r.MutationType = x). x resolves when it is a string literal,
// a package-level string constant, or a parameter of the enclosing function;
// a parameter resolves through the matching argument at each call of that
// function (one level). The source is read rather than imported because
// authzop must not import pkg/hub.
func hubWrittenMutationTypes(t *testing.T) map[string]bool {
	t.Helper()
	pkgDir := filepath.Join(findRepoRoot(t), "pkg", "hub")
	paths, err := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	if err != nil {
		t.Fatalf("cannot list pkg/hub sources: %v", err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("cannot parse %s: %v", path, parseErr)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("no pkg/hub sources found in %s", pkgDir)
	}

	consts := packageStringConsts(files)
	resolve := func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		case *ast.Ident:
			s, ok := consts[v.Name]
			return s, ok
		}
		return "", false
	}

	written := make(map[string]bool)
	// paramWriters maps a function name to the indexes of its parameters
	// that are written as MutationType.
	paramWriters := make(map[string][]int)
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			params := paramIndexes(fn.Type)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var value ast.Expr
				switch v := n.(type) {
				case *ast.KeyValueExpr:
					if key, ok := v.Key.(*ast.Ident); ok && key.Name == "MutationType" {
						value = v.Value
					}
				case *ast.AssignStmt:
					for i, lhs := range v.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "MutationType" && i < len(v.Rhs) {
							value = v.Rhs[i]
						}
					}
				}
				if value == nil {
					return true
				}
				if s, ok := resolve(value); ok {
					written[s] = true
				} else if id, ok := value.(*ast.Ident); ok {
					if idx, ok := params[id.Name]; ok {
						paramWriters[fn.Name.Name] = append(paramWriters[fn.Name.Name], idx)
					}
				}
				return true
			})
		}
	}

	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			for _, idx := range paramWriters[name] {
				if idx < len(call.Args) {
					if s, ok := resolve(call.Args[idx]); ok {
						written[s] = true
					}
				}
			}
			return true
		})
	}

	if len(written) == 0 {
		t.Fatal("no mutation types found in pkg/hub sources")
	}
	return written
}

// packageStringConsts returns the package-level constants in files whose
// value is a string literal, keyed by name.
func packageStringConsts(files []*ast.File) map[string]string {
	consts := make(map[string]string)
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if s, err := strconv.Unquote(lit.Value); err == nil {
						consts[name.Name] = s
					}
				}
			}
		}
	}
	return consts
}

// paramIndexes maps each named parameter of ft to its argument index.
func paramIndexes(ft *ast.FuncType) map[string]int {
	idx := make(map[string]int)
	if ft.Params == nil {
		return idx
	}
	i := 0
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, name := range field.Names {
			idx[name.Name] = i
			i++
		}
	}
	return idx
}
