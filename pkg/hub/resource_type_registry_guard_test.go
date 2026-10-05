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
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Guard for ptone/scion#2140: every Resource{Type: ...} literal in this
// package must name a resource type that the permission registry defines,
// or one of the reviewed unregistered pairs in
// unregisteredResourcePermissions. A new literal with an unknown type would
// otherwise resolve to no permission and deny every caller.

// resourceTypeLiteral is one Resource{Type: ...} literal whose type could be
// resolved to a string constant.
type resourceTypeLiteral struct {
	pos      string
	typeName string
}

// stringConsts returns the package-level string constants declared in f,
// keyed by name.
func stringConsts(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) != len(vs.Names) {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = v
				}
			}
		}
	}
	return out
}

// parseGoDir parses every non-test .go file in dir.
func parseGoDir(t *testing.T, fset *token.FileSet, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "parse %s", path)
		files[name] = f
	}
	return files
}

// logOnlyResourceFuncs are functions that take a Resource only to label a
// log record and never evaluate it. Literals passed directly to them are not
// scanned.
var logOnlyResourceFuncs = map[string]bool{"logAuthzDenial": true}

// collectResourceTypeLiterals returns every Resource{Type: X} literal in
// files whose X is a string literal, a package-level string constant of this
// package, or a permissions.<Const> selector. Literals whose type comes from
// a variable or call are counted in unresolved. Literals passed directly to
// a logOnlyResourceFuncs function are skipped.
func collectResourceTypeLiterals(fset *token.FileSet, files map[string]*ast.File, localConsts, permConsts map[string]string) (found []resourceTypeLiteral, unresolved int) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		logOnly := map[*ast.CompositeLit]bool{}
		ast.Inspect(files[name], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); ok && logOnlyResourceFuncs[fn.Name] {
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.CompositeLit); ok {
						logOnly[lit] = true
					}
				}
			}
			return true
		})
		ast.Inspect(files[name], func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || logOnly[lit] {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "Resource" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Type" {
					continue
				}
				value, resolved := "", false
				switch v := kv.Value.(type) {
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil {
							value, resolved = s, true
						}
					}
				case *ast.Ident:
					value, resolved = localConsts[v.Name]
				case *ast.SelectorExpr:
					if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "permissions" {
						value, resolved = permConsts[v.Sel.Name]
					}
				}
				if !resolved {
					unresolved++
					continue
				}
				pos := fset.Position(kv.Value.Pos())
				found = append(found, resourceTypeLiteral{
					pos:      filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line),
					typeName: value,
				})
			}
			return true
		})
	}
	return found, unresolved
}

// knownResourceTypes returns the resource types the registry defines plus
// the types named by the reviewed unregistered pairs.
func knownResourceTypes() map[string]bool {
	known := map[string]bool{}
	for _, p := range permissions.Registry {
		known[p.Resource] = true
	}
	for k := range unregisteredResourcePermissions {
		known[k.ResourceType] = true
	}
	return known
}

func unknownResourceTypeLiterals(found []resourceTypeLiteral, known map[string]bool) []string {
	var bad []string
	for _, l := range found {
		if !known[l.typeName] {
			bad = append(bad, l.pos+": "+strconv.Quote(l.typeName))
		}
	}
	return bad
}

// TestResourceTypeLiterals_AllInRegistry fails when a non-test file in this
// package builds a Resource with a type that has no registry entry and is
// not one of the reviewed unregistered pairs.
func TestResourceTypeLiterals_AllInRegistry(t *testing.T) {
	dir := findHubDir(t)
	fset := token.NewFileSet()

	hubFiles := parseGoDir(t, fset, dir)
	permFiles := parseGoDir(t, fset, filepath.Join(dir, "permissions"))

	localConsts := map[string]string{}
	for _, f := range hubFiles {
		for k, v := range stringConsts(f) {
			localConsts[k] = v
		}
	}
	permConsts := map[string]string{}
	for _, f := range permFiles {
		for k, v := range stringConsts(f) {
			permConsts[k] = v
		}
	}

	found, _ := collectResourceTypeLiterals(fset, hubFiles, localConsts, permConsts)
	require.NotEmpty(t, found, "scanner found no Resource{Type: ...} literals; the matcher is broken")

	// Scanner self-check: brokerResource's own "broker" literal must be seen.
	sawBroker := false
	for _, l := range found {
		if l.typeName == permissions.ResourceBroker {
			sawBroker = true
			break
		}
	}
	require.True(t, sawBroker, "scanner did not see the broker resource literal; the matcher is broken")

	bad := unknownResourceTypeLiterals(found, knownResourceTypes())
	assert.Empty(t, bad,
		"Resource literals use a type that is not in permissions.Registry; "+
			"use the registry resource type (see pkg/hub/permissions/registry.go)")
}

// TestResourceTypeLiterals_DetectsUnknownType proves the guard reports an
// unknown type, so a passing TestResourceTypeLiterals_AllInRegistry is not
// a scanner that silently matches nothing.
func TestResourceTypeLiterals_DetectsUnknownType(t *testing.T) {
	const src = `package hub

const localType = "not_a_registry_type"

func f() {
	_ = Resource{Type: "another_unknown_type", ID: "x"}
	_ = Resource{Type: localType, ID: "x"}
	_ = Resource{Type: permissions.ResourceBroker, ID: "x"}
	_ = Resource{Type: "broker", ID: "x"}
	logAuthzDenial(nil, nil, Resource{Type: "log_label_only"}, "read", "r")
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	files := map[string]*ast.File{"sample.go": f}
	permConsts := map[string]string{"ResourceBroker": permissions.ResourceBroker}

	found, unresolved := collectResourceTypeLiterals(fset, files, stringConsts(f), permConsts)
	assert.Zero(t, unresolved)
	assert.Len(t, found, 4)
	assert.Equal(t, []string{
		`sample.go:6: "another_unknown_type"`,
		`sample.go:7: "not_a_registry_type"`,
	}, unknownResourceTypeLiterals(found, knownResourceTypes()))
}

// TestUnregisteredResourcePermissions_ExactSet pins the reviewed unregistered
// pairs. Adding one widens what the guard above accepts, so it needs a
// deliberate change here.
func TestUnregisteredResourcePermissions_ExactSet(t *testing.T) {
	assert.Equal(t, map[resourceActionKey]string{
		{ResourceType: "runtime_broker", Action: ActionRead}:   "runtime_broker.read",
		{ResourceType: "runtime_broker", Action: ActionUpdate}: "runtime_broker.update",
	}, unregisteredResourcePermissions)
}
