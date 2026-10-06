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
	"go/types"
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

// Guard for ptone/scion#2140: it rejects a Resource{Type: ...} literal in
// this package that names a type the permission registry does not define,
// unless the type is in one of the reviewed unregistered pairs in
// unregisteredResourcePermissions. Types it cannot resolve statically are
// pinned in unresolvedResourceTypeValues so a new one needs review.

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

// isResourceType reports whether e is the type Resource or *Resource.
func isResourceType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == "Resource"
}

// isResourceValue reports whether e is a Resource{...} or &Resource{...}
// literal.
func isResourceValue(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	return ok && lit.Type != nil && isResourceType(lit.Type)
}

// declaredAsResource reports whether the variable x was declared in this
// file with type Resource or *Resource, or from a Resource literal.
func declaredAsResource(x *ast.Ident) bool {
	if x.Obj == nil || x.Obj.Kind != ast.Var {
		return false
	}
	switch d := x.Obj.Decl.(type) {
	case *ast.Field:
		return isResourceType(d.Type)
	case *ast.ValueSpec:
		if d.Type != nil {
			return isResourceType(d.Type)
		}
		for i, name := range d.Names {
			if name.Name == x.Name && i < len(d.Values) {
				return isResourceValue(d.Values[i])
			}
		}
	case *ast.AssignStmt:
		for i, lhs := range d.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == x.Name && len(d.Rhs) == len(d.Lhs) {
				return isResourceValue(d.Rhs[i])
			}
		}
	}
	return false
}

// collectResourceTypeLiterals returns every Resource type value in files
// whose value is a string literal, a string constant of this package, or a
// permissions.<Const> selector. It scans Resource{Type: X} and
// &Resource{Type: X} literals, literals whose Resource element type is
// elided inside a Resource slice, array or map literal, and assignments
// x.Type = X where x is declared in the same file as a Resource. Values it
// cannot resolve statically (a variable, a call, a concatenation, or an
// identifier that shadows a package constant) are returned in unresolved
// as "file: expr". Literals passed directly to a logOnlyResourceFuncs
// function are skipped. Not scanned: Type values set through any other
// path, such as a Resource returned by a call or a field of another struct.
//
// The guard is type-level only: it checks that each type is one the
// registry defines, not that each (type, action) pair resolves. Pair
// resolution is covered by the permission resolver tests.
func collectResourceTypeLiterals(fset *token.FileSet, files map[string]*ast.File, localConsts, permConsts map[string]string) (found []resourceTypeLiteral, unresolved []string) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		record := func(value ast.Expr) {
			s, resolved := "", false
			switch v := value.(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if u, err := strconv.Unquote(v.Value); err == nil {
						s, resolved = u, true
					}
				}
			case *ast.Ident:
				// A name the parser resolved in this file is declared
				// here and shadows a package constant of the same name;
				// only a constant declaration resolves to a value.
				if v.Obj == nil {
					s, resolved = localConsts[v.Name]
				} else if vs, ok := v.Obj.Decl.(*ast.ValueSpec); ok && v.Obj.Kind == ast.Con {
					s, resolved = constValue(vs, v.Name)
				}
			case *ast.SelectorExpr:
				if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "permissions" && pkg.Obj == nil {
					s, resolved = permConsts[v.Sel.Name]
				}
			}
			pos := fset.Position(value.Pos())
			file := filepath.Base(pos.Filename)
			if !resolved {
				unresolved = append(unresolved, file+": "+types.ExprString(value))
				return
			}
			found = append(found, resourceTypeLiteral{
				pos:      file + ":" + strconv.Itoa(pos.Line),
				typeName: s,
			})
		}

		logOnly := map[*ast.CompositeLit]bool{}
		elided := map[*ast.CompositeLit]bool{}
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
			if as, ok := n.(*ast.AssignStmt); ok {
				for i, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Type" || len(as.Rhs) != len(as.Lhs) {
						continue
					}
					if x, ok := sel.X.(*ast.Ident); ok && declaredAsResource(x) {
						record(as.Rhs[i])
					}
				}
				return true
			}
			lit, ok := n.(*ast.CompositeLit)
			if !ok || logOnly[lit] {
				return true
			}
			// Mark element literals whose Resource type is elided.
			var elt ast.Expr
			switch t := lit.Type.(type) {
			case *ast.ArrayType:
				elt = t.Elt
			case *ast.MapType:
				elt = t.Value
			}
			if elt != nil && isResourceType(elt) {
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						e = kv.Value
					}
					if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
						e = u.X
					}
					if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
						elided[inner] = true
					}
				}
			}
			if !elided[lit] && (lit.Type == nil || !isResourceType(lit.Type)) {
				return true
			}
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Type" {
					record(kv.Value)
				}
			}
			return true
		})
	}
	return found, unresolved
}

// constValue returns the string value of the constant name in vs.
func constValue(vs *ast.ValueSpec, name string) (string, bool) {
	for i, n := range vs.Names {
		if n.Name != name || i >= len(vs.Values) {
			continue
		}
		if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				return s, true
			}
		}
	}
	return "", false
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

	found, unresolved := collectResourceTypeLiterals(fset, hubFiles, localConsts, permConsts)
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
		"Resource literals use a type that is not in permissions.Registry. "+
			"Either use the registry resource type (see "+
			"pkg/hub/permissions/registry.go), or add a reviewed pair to "+
			"unregisteredResourcePermissions (authz_permission_resolver.go), "+
			"naming its call sites, and update "+
			"TestUnregisteredResourcePermissions_ExactSet")

	sort.Strings(unresolved)
	assert.Equal(t, unresolvedResourceTypeValues, unresolved,
		"the set of Resource type values the guard cannot resolve statically "+
			"changed. Resolve the type statically (a string literal or a "+
			"permissions constant), or confirm the value can only hold "+
			"registry types and update unresolvedResourceTypeValues")
}

// unresolvedResourceTypeValues pins the Resource type values, as
// "file: expr", that the guard cannot resolve statically. These were
// present when the pin was added; a new entry needs a review that the
// value can only hold registry types. Keep it sorted.
var unresolvedResourceTypeValues = []string{
	"audit_authz.go: req.Resource.Type",
	"authorized_list.go: resourceType",
	"authz_relationship_rules.go: kind",
	"capabilities.go: resourceType",
	"handlers_resource_import.go: authzResourceType",
	"handlers_resource_import.go: authzResourceType",
	"handlers_resource_import.go: authzType",
	"handlers_resource_import.go: authzType",
	"route_metadata.go: meta.Resource",
}

// TestResourceTypeLiterals_DetectsUnknownType proves the guard reports an
// unknown type, so a passing TestResourceTypeLiterals_AllInRegistry is not
// a scanner that silently matches nothing.
func TestResourceTypeLiterals_DetectsUnknownType(t *testing.T) {
	const src = `package hub

const localType = "not_a_registry_type"

func f(param string) {
	_ = Resource{Type: "another_unknown_type", ID: "x"}
	_ = Resource{Type: localType, ID: "x"}
	_ = Resource{Type: permissions.ResourceBroker, ID: "x"}
	_ = Resource{Type: "broker", ID: "x"}
	logAuthzDenial(nil, nil, Resource{Type: "log_label_only"}, "read", "r")
	_ = []Resource{{Type: "elided_slice_type"}}
	_ = map[string]*Resource{"k": {Type: "elided_map_type"}}
	var r Resource
	r.Type = "assigned_type"
	_ = Resource{Type: param}
}

func g() {
	localType := pick()
	_ = Resource{Type: localType}
	other.Type = "not_a_resource_field"
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	files := map[string]*ast.File{"sample.go": f}
	permConsts := map[string]string{"ResourceBroker": permissions.ResourceBroker}

	found, unresolved := collectResourceTypeLiterals(fset, files, stringConsts(f), permConsts)
	assert.Equal(t, []string{"sample.go: param", "sample.go: localType"}, unresolved)
	assert.Len(t, found, 7)
	assert.Equal(t, []string{
		`sample.go:6: "another_unknown_type"`,
		`sample.go:7: "not_a_registry_type"`,
		`sample.go:11: "elided_slice_type"`,
		`sample.go:12: "elided_map_type"`,
		`sample.go:14: "assigned_type"`,
	}, unknownResourceTypeLiterals(found, knownResourceTypes()))
}

// TestUnregisteredResourcePermissions_ExactSet pins the reviewed unregistered
// pairs. Adding one widens what the guard above accepts, so it needs a
// deliberate change here.
func TestUnregisteredResourcePermissions_ExactSet(t *testing.T) {
	assert.Equal(t, map[resourceActionKey]string{
		{ResourceType: "runtime_broker", Action: ActionRead}:   "runtime_broker.read",
		{ResourceType: "runtime_broker", Action: ActionUpdate}: "runtime_broker.update",
	}, unregisteredResourcePermissions,
		"unregistered pairs changed; confirm the new pair is needed "+
			"(see ptone/scion#2140) and update this pin")
}
