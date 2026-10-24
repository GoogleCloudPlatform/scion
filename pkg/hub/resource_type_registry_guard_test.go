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
	"path"
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
// unregisteredResourcePermissions. It applies the same check to the
// resource type arguments of the capability helpers ComputeCapabilities,
// ComputeScopeCapabilities and ComputeCapabilitiesBatch at each call.
// Types it cannot resolve statically are pinned in
// unresolvedResourceTypeValues so a new one needs review.

// resourceTypeLiteral is one Resource{Type: ...} literal whose type could be
// resolved to a string constant.
type resourceTypeLiteral struct {
	pos      string
	typeName string
	// fn is the package-level function the value is in, or "" when it
	// is in a method or outside a function.
	fn string
	// helper is the capability helper that reads the value, or "" for
	// a value no helper reads.
	helper string
	// site names, for a value built by a Resource constructor passed to
	// ComputeCapabilities, the constructor and its call sites.
	site string
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

// parseGoDir parses every non-test .go file in dir and, with withGuardSubdirs,
// in each existing guardScanSubdirs directory under dir (keyed by relative
// path).
func parseGoDir(t *testing.T, fset *token.FileSet, dir string, withGuardSubdirs bool) map[string]*ast.File {
	t.Helper()
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards Resource type literals (pkg/hub only; pkg/hub/permissions is parsed alone).
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards Resource type literals (pkg/hub only; pkg/hub/permissions is parsed alone).
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards Resource type literals (pkg/hub only; pkg/hub/permissions is parsed alone).
	read := os.ReadDir
	if withGuardSubdirs {
		read = readGuardScanDirs
	}
	entries, err := read(dir)
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
// log record and never evaluate it. Literals, or pointers to literals,
// passed directly to them are not scanned.
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

// capabilityHelperTypeArgs are the AuthzService capability helpers whose
// last argument is a resource type string. The guard records that argument
// at every call. A Resource literal inside one of these helpers whose Type
// is that parameter is covered by the call-site check, unless the helper
// writes the parameter.
var capabilityHelperTypeArgs = map[string]bool{
	"ComputeCapabilitiesBatch": true,
	"ComputeScopeCapabilities": true,
}

// capabilityResourceHelper is the AuthzService capability helper that
// takes a Resource rather than a resource type string.
const capabilityResourceHelper = "ComputeCapabilities"

// otherComputeCapabilitiesReceivers pins, as "file: receiver", the calls to
// a ComputeCapabilities method of another type (CapabilitiesService and
// ProjectMembershipService), which take no resource type. The guard skips
// these calls and checks a call on any other receiver as
// AuthzService.ComputeCapabilities. A pin that matches no call fails the
// guard. Keep it sorted.
var otherComputeCapabilitiesReceivers = []string{
	"access_constraint_capabilities.go: cs",
	"handlers_access_constraints.go: s.capabilitiesService",
	"handlers_project_members.go: s.membershipService",
}

// resourceTypeScan is the result of collectResourceTypeLiterals.
type resourceTypeScan struct {
	// found holds the type values resolved to a string.
	found []resourceTypeLiteral
	// unresolved holds, as "file: expr", the values the guard cannot
	// resolve statically.
	unresolved []string
	// helperCalls counts the checked call sites of each capability
	// helper, keyed by method name.
	helperCalls map[string]int
	// helperValues holds the type values each capability helper reads,
	// with helper set: the resolved type argument of a call, the Type
	// of a Resource literal passed to ComputeCapabilities, and every
	// value built by a Resource constructor passed to it.
	helperValues []resourceTypeLiteral
	// helperTagged counts the helper calls whose argument resolved to
	// at least one value in helperValues, and helperUnresolved the
	// calls whose argument is in unresolved, keyed by method name.
	helperTagged, helperUnresolved map[string]int
	// unfollowed holds each Resource constructor passed to
	// ComputeCapabilities that reaches no resolved type value, with its
	// call sites.
	unfollowed []string
	// otherReceivers holds the skip pins that matched a call, as
	// "file: receiver", sorted and without duplicates.
	otherReceivers []string
}

// isUnsetStringVar reports whether vs declares string variables with no
// initial value, as in "var x string".
func isUnsetStringVar(vs *ast.ValueSpec) bool {
	ident, ok := vs.Type.(*ast.Ident)
	return ok && ident.Name == "string" && len(vs.Values) == 0
}

// enclosingFuncBody returns the body of the innermost function declaration
// or function literal in f that contains n, or nil if there is none.
func enclosingFuncBody(f *ast.File, n ast.Node) *ast.BlockStmt {
	var body *ast.BlockStmt
	ast.Inspect(f, func(m ast.Node) bool {
		if m == nil || m.Pos() > n.Pos() || m.End() < n.End() {
			return false
		}
		switch fn := m.(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		}
		return true
	})
	return body
}

// varWrites returns the right-hand side of each plain assignment to the
// variable obj in body. ok is false when body writes obj in a form the
// guard does not follow: obj's address is taken, obj is assigned inside a
// function literal, by a compound, defining or multi-value assignment, or
// as a range variable. A parenthesised operand, as in (x) = v or &(x),
// is the same variable.
func varWrites(body *ast.BlockStmt, obj *ast.Object) (rhs []ast.Expr, ok bool) { //nolint:staticcheck // SA1019: syntax-only resolution via ast.Object; no type info is loaded.
	ok = true
	is := func(e ast.Expr) bool {
		id, isIdent := ast.Unparen(e).(*ast.Ident)
		return isIdent && id.Obj == obj
	}
	var walk func(n ast.Node, inClosure bool)
	walk = func(n ast.Node, inClosure bool) {
		ast.Inspect(n, func(m ast.Node) bool {
			switch m := m.(type) {
			case *ast.FuncLit:
				walk(m.Body, true)
				return false
			case *ast.UnaryExpr:
				if m.Op == token.AND && is(m.X) {
					ok = false
				}
			case *ast.RangeStmt:
				if is(m.Key) || is(m.Value) {
					ok = false
				}
			case *ast.AssignStmt:
				for i, lhs := range m.Lhs {
					if !is(lhs) {
						continue
					}
					if inClosure || m.Tok != token.ASSIGN || len(m.Lhs) != len(m.Rhs) {
						ok = false
						continue
					}
					rhs = append(rhs, m.Rhs[i])
				}
			}
			return true
		})
	}
	if body != nil {
		walk(body, false)
	}
	return rhs, ok
}

// collectResourceTypeLiterals returns every Resource type value in files
// whose value is a string literal, a string constant of this package, or a
// permissions.<Const> selector. It scans Resource{Type: X} and
// &Resource{Type: X} literals, literals whose Resource element type is
// elided inside a Resource slice, array or map literal, and assignments
// x.Type = X where x is declared in the same file as a Resource. In a
// multi-value assignment such as x.Type, err = f(), the single right-hand
// expression is recorded. Values it cannot resolve statically (a
// variable, a call, a concatenation, or an identifier that shadows a
// package constant) are returned in unresolved as "file: expr". Literals,
// or pointers to literals, passed directly to a logOnlyResourceFuncs
// function are skipped. Not scanned: unkeyed (positional) Resource
// literals, and Type values set through any other path, such as a
// Resource returned by a call, a field of another struct, an index
// expression, a range variable over a Resource slice, or new(Resource).
//
// Two kinds of variable resolve. A local "var x string" with no initial
// value resolves to every value assigned to x in its enclosing function;
// each must be a literal or constant, and x must not have its address
// taken or be assigned in a function literal, else x is unresolved. The
// resource type parameter of a capabilityHelperTypeArgs helper is covered
// by the call-site check below, unless the helper writes it.
//
// It also checks the capability helpers at each call. The last argument
// of a capabilityHelperTypeArgs call is recorded like a Type value. The
// Resource argument of a ComputeCapabilities call must be a Resource
// literal (scanned as above) or a call to a package-level function with a
// Resource result (whose literals are scanned where it builds them); any
// other argument is unresolved. Calls on a receiver in skipReceivers
// ("file: receiver") are skipped and reported in otherReceivers. A helper
// referenced other than as the function of a call (a method value or
// method expression) is unresolved, since its callers cannot be scanned.
// The guard scans pkg/hub and its guardScanSubdirs only; no code outside
// them calls these helpers.
//
// Each value a helper reads is also recorded in helperValues, tagged
// with the helper, for the action-map check. A Resource literal passed
// to ComputeCapabilities with no Type key is recorded with type "". For
// a Resource constructor passed to ComputeCapabilities, every value
// built in that constructor, or in a package-level Resource constructor
// it calls (followed transitively), is recorded. A constructor that
// reaches no resolved value, or an x.Type assignment the scan does not
// follow, is reported in unfollowed. A helper argument is never
// resolved through the helper parameter form; such an argument is
// unresolved.
//
// The guard is type-level only: it checks that each type is one the
// registry defines, not that each (type, action) pair resolves. Pair
// resolution is covered by the permission resolver tests.
func collectResourceTypeLiterals(fset *token.FileSet, files map[string]*ast.File, localConsts, permConsts map[string]string, skipReceivers []string) resourceTypeScan {
	scan := resourceTypeScan{
		helperCalls:      map[string]int{},
		helperTagged:     map[string]int{},
		helperUnresolved: map[string]int{},
	}
	// ctorSites holds the call sites of each Resource constructor passed
	// to ComputeCapabilities; ctorCalls holds, for each package-level
	// function, the Resource constructors it calls.
	ctorSites := map[string][]string{}
	ctorCalls := map[string]map[string]bool{}
	// skippedTypeWrites holds, for each package-level function, the
	// x.Type assignments the scan does not follow because x is not
	// declared in the file as a Resource, such as r := otherCtor()
	// followed by r.Type = expr.
	skippedTypeWrites := map[string][]string{}
	// helperLits holds the Resource literals passed to
	// ComputeCapabilities, recorded at the call.
	helperLits := map[*ast.CompositeLit]bool{}
	// curFn is the package-level function being scanned, or "".
	var curFn string
	skip := map[string]bool{}
	for _, r := range skipReceivers {
		skip[r] = true
	}
	matched := map[string]bool{}

	// Package-level functions with a single Resource result, and the
	// resource type parameter of each capability helper, keyed by its
	// declaring field.
	resourceFuncs := map[string]bool{}
	helperParams := map[*ast.Field]*ast.FuncDecl{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			results := fd.Type.Results
			if fd.Recv == nil && results != nil && len(results.List) == 1 &&
				len(results.List[0].Names) <= 1 && isResourceType(results.List[0].Type) {
				resourceFuncs[fd.Name.Name] = true
			}
			if fd.Recv != nil && len(fd.Recv.List) == 1 &&
				types.ExprString(fd.Recv.List[0].Type) == "*AuthzService" &&
				capabilityHelperTypeArgs[fd.Name.Name] && len(fd.Type.Params.List) > 0 {
				helperParams[fd.Type.Params.List[len(fd.Type.Params.List)-1]] = fd
			}
		}
	}

	resolve := func(value ast.Expr) (string, bool) {
		switch v := value.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if u, err := strconv.Unquote(v.Value); err == nil {
					return u, true
				}
			}
		case *ast.Ident:
			// A name the parser resolved in this file is declared
			// here and shadows a package constant of the same name;
			// only a constant declaration resolves to a value.
			if v.Obj == nil {
				s, ok := localConsts[v.Name]
				return s, ok
			}
			if vs, ok := v.Obj.Decl.(*ast.ValueSpec); ok && v.Obj.Kind == ast.Con {
				return constValue(vs, v.Name)
			}
		case *ast.SelectorExpr:
			if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "permissions" && pkg.Obj == nil {
				s, ok := permConsts[v.Sel.Name]
				return s, ok
			}
		}
		return "", false
	}
	fileOf := func(e ast.Node) string {
		return filepath.Base(fset.Position(e.Pos()).Filename)
	}
	unresolvedAt := func(e ast.Expr) {
		scan.unresolved = append(scan.unresolved, fileOf(e)+": "+types.ExprString(e))
	}
	posOf := func(e ast.Node) string {
		return fileOf(e) + ":" + strconv.Itoa(fset.Position(e.Pos()).Line)
	}
	foundAt := func(e ast.Expr, s, helper string) {
		l := resourceTypeLiteral{pos: posOf(e), typeName: s, fn: curFn}
		scan.found = append(scan.found, l)
		if helper != "" {
			l.helper = helper
			scan.helperValues = append(scan.helperValues, l)
		}
	}
	isCtorCall := func(e ast.Expr) (string, bool) {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return "", false
		}
		fn, ok := c.Fun.(*ast.Ident)
		if !ok || !resourceFuncs[fn.Name] || (fn.Obj != nil && fn.Obj.Kind != ast.Fun) {
			return "", false
		}
		return fn.Name, true
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := files[name]

		// resolveVar resolves a variable through the two forms
		// described above. It reports whether id was handled. A helper
		// argument (helper != "") is not resolved through the helper
		// parameter form, so it is unresolved and its call fails the
		// call count check.
		resolveVar := func(id *ast.Ident, helper string) bool {
			if id.Obj == nil || id.Obj.Kind != ast.Var {
				return false
			}
			switch d := id.Obj.Decl.(type) {
			case *ast.Field:
				fd := helperParams[d]
				if helper != "" || fd == nil || d.Names[len(d.Names)-1].Name != id.Name {
					return false
				}
				writes, ok := varWrites(fd.Body, id.Obj)
				return ok && len(writes) == 0
			case *ast.ValueSpec:
				if !isUnsetStringVar(d) {
					return false
				}
				writes, ok := varWrites(enclosingFuncBody(file, d), id.Obj)
				if !ok || len(writes) == 0 {
					return false
				}
				values := make([]string, len(writes))
				for i, w := range writes {
					s, resolved := resolve(w)
					if !resolved {
						return false
					}
					values[i] = s
				}
				for i, w := range writes {
					foundAt(w, values[i], helper)
				}
				return true
			}
			return false
		}
		// record records value, read by helper ("" for none), and
		// reports whether it resolved.
		record := func(value ast.Expr, helper string) bool {
			if s, ok := resolve(value); ok {
				foundAt(value, s, helper)
				return true
			}
			if id, ok := value.(*ast.Ident); ok && resolveVar(id, helper) {
				return true
			}
			unresolvedAt(value)
			return false
		}
		countArg := func(method string, resolved bool) {
			if resolved {
				scan.helperTagged[method]++
			} else {
				scan.helperUnresolved[method]++
			}
		}
		checkHelperCall := func(call *ast.CallExpr) {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return
			}
			switch method := sel.Sel.Name; {
			case capabilityHelperTypeArgs[method]:
				scan.helperCalls[method]++
				if len(call.Args) == 0 || call.Ellipsis.IsValid() {
					unresolvedAt(call)
					countArg(method, false)
					return
				}
				countArg(method, record(call.Args[len(call.Args)-1], method))
			case method == capabilityResourceHelper:
				key := fileOf(call) + ": " + types.ExprString(sel.X)
				if skip[key] {
					matched[key] = true
					return
				}
				scan.helperCalls[method]++
				if len(call.Args) != 3 || call.Ellipsis.IsValid() {
					unresolvedAt(call)
					countArg(method, false)
					return
				}
				arg := call.Args[2]
				if isResourceValue(arg) {
					// Record the literal's Type here, tagged with the
					// helper; the literal scan below skips it.
					if u, ok := arg.(*ast.UnaryExpr); ok {
						arg = u.X
					}
					lit := arg.(*ast.CompositeLit)
					helperLits[lit] = true
					for _, e := range lit.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Type" {
							countArg(method, record(kv.Value, method))
							return
						}
					}
					foundAt(lit, "", method)
					countArg(method, true)
					return
				}
				if fn, ok := isCtorCall(arg); ok {
					// Counted once the constructor is followed, after
					// every file is scanned.
					ctorSites[fn] = append(ctorSites[fn], posOf(call))
					return
				}
				unresolvedAt(arg)
				countArg(method, false)
			}
		}

		logOnly := map[*ast.CompositeLit]bool{}
		callFuns := map[*ast.SelectorExpr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				callFuns[sel] = true
			}
			if fn, ok := call.Fun.(*ast.Ident); ok && logOnlyResourceFuncs[fn.Name] {
				for _, arg := range call.Args {
					if u, ok := arg.(*ast.UnaryExpr); ok && u.Op == token.AND {
						arg = u.X
					}
					if lit, ok := arg.(*ast.CompositeLit); ok {
						logOnly[lit] = true
					}
				}
			}
			return true
		})
		elided := map[*ast.CompositeLit]bool{}
		visit := func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				method := sel.Sel.Name
				if (capabilityHelperTypeArgs[method] || method == capabilityResourceHelper) && !callFuns[sel] {
					unresolvedAt(sel)
				}
				return true
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if fn, ok := isCtorCall(call); ok && curFn != "" {
					if ctorCalls[curFn] == nil {
						ctorCalls[curFn] = map[string]bool{}
					}
					ctorCalls[curFn][fn] = true
				}
				checkHelperCall(call)
				return true
			}
			if as, ok := n.(*ast.AssignStmt); ok {
				for i, lhs := range as.Lhs {
					// A parenthesised operand, as in (r).Type = v or
					// (r.Type) = v, is the same write.
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Type" {
						continue
					}
					x, ok := ast.Unparen(sel.X).(*ast.Ident)
					if !ok || !declaredAsResource(x) {
						// A Type write on a value not declared as a
						// Resource fails closed in a followed constructor.
						if curFn != "" {
							skippedTypeWrites[curFn] = append(skippedTypeWrites[curFn],
								posOf(lhs)+": "+types.ExprString(lhs))
						}
						continue
					}
					switch {
					case len(as.Rhs) == len(as.Lhs):
						record(as.Rhs[i], "")
					case len(as.Rhs) == 1:
						// Multi-value assignment, such as x.Type, err = f().
						record(as.Rhs[0], "")
					}
				}
				return true
			}
			lit, ok := n.(*ast.CompositeLit)
			if !ok || logOnly[lit] || helperLits[lit] {
				return true
			}
			// Mark element literals whose Resource type is elided. This
			// is one level deep only: a nested elided literal, such as
			// an element of a slice of Resource slices, is not scanned.
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
					record(kv.Value, "")
				}
			}
			return true
		}
		for _, decl := range file.Decls {
			curFn = ""
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
				curFn = fd.Name.Name
			}
			ast.Inspect(decl, visit)
		}
		curFn = ""
	}

	// Follow each Resource constructor passed to ComputeCapabilities,
	// and the package-level constructors it calls, to the values they
	// build. Fail closed: a constructor that reaches no resolved value,
	// or reaches an x.Type assignment the scan does not follow, is
	// reported in unfollowed and its calls are not counted. A Resource
	// literal with no Type key, such as the Resource{} the nil guards
	// of harnessConfigResource and gcpServiceAccountResource return, is
	// not a value: it has type "", for which every helper returns no
	// actions, and the constructor's typed literal is what is checked.
	ctors := make([]string, 0, len(ctorSites))
	for c := range ctorSites {
		ctors = append(ctors, c)
	}
	sort.Strings(ctors)
	for _, c := range ctors {
		reach := map[string]bool{c: true}
		queue := []string{c}
		for len(queue) > 0 {
			fn := queue[0]
			queue = queue[1:]
			for callee := range ctorCalls[fn] {
				if !reach[callee] {
					reach[callee] = true
					queue = append(queue, callee)
				}
			}
		}
		sites := strings.Join(ctorSites[c], ", ")
		reached := make([]string, 0, len(reach))
		for fn := range reach {
			reached = append(reached, fn)
		}
		sort.Strings(reached)
		skipped := false
		for _, fn := range reached {
			for _, w := range skippedTypeWrites[fn] {
				scan.unfollowed = append(scan.unfollowed,
					c+", called at "+sites+": Type write not followed at "+w)
				skipped = true
			}
		}
		n := 0
		for _, l := range scan.found {
			if l.fn == "" || !reach[l.fn] {
				continue
			}
			via := l.fn
			if l.fn != c {
				via += " via " + c
			}
			l.helper = capabilityResourceHelper
			l.site = via + ", called at " + sites
			scan.helperValues = append(scan.helperValues, l)
			n++
		}
		if n == 0 {
			scan.unfollowed = append(scan.unfollowed, c+", called at "+sites)
			continue
		}
		if skipped {
			continue
		}
		scan.helperTagged[capabilityResourceHelper] += len(ctorSites[c])
	}
	for key := range matched {
		scan.otherReceivers = append(scan.otherReceivers, key)
	}
	sort.Strings(scan.otherReceivers)
	return scan
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

// missingActionMapEntries returns, as "pos: helper: map: type", each
// helper value whose type has no entry in the action map that helper
// reads. A value built by a Resource constructor is followed by its
// constructor and call sites. A value the guard cannot resolve is not
// checked here. A helper argument that is unresolved fails
// TestResourceTypeLiterals_AllInRegistry through its helperUnresolved
// check, so no unresolvedResourceTypeValues pin exempts one; a value in
// a followed constructor that is unresolved is on that pin list.
func missingActionMapEntries(values []resourceTypeLiteral, resourceActions, scopeActions map[string][]Action) []string {
	var bad []string
	for _, v := range values {
		var actions map[string][]Action
		var name string
		switch v.helper {
		case capabilityResourceHelper:
			// capabilities.go:181 reads ResourceActions[resource.Type].
			actions, name = resourceActions, "ResourceActions"
		case "ComputeCapabilitiesBatch":
			// capabilities.go:276 reads ResourceActions[resourceType].
			actions, name = resourceActions, "ResourceActions"
		case "ComputeScopeCapabilities":
			// capabilities.go:220 reads ScopeActions[resourceType], the
			// last argument only; scopeType is not a map key and is
			// not checked.
			actions, name = scopeActions, "ScopeActions"
		default:
			bad = append(bad, v.pos+": no action map for helper "+strconv.Quote(v.helper))
			continue
		}
		if len(actions[v.typeName]) > 0 {
			continue
		}
		msg := v.pos + ": " + v.helper + ": " + name + ": " + strconv.Quote(v.typeName)
		if v.site != "" {
			msg += " (" + v.site + ")"
		}
		bad = append(bad, msg)
	}
	return bad
}

// TestResourceTypeLiterals_AllInRegistry fails when a non-test file in this
// package builds a Resource with a type that has no registry entry and is
// not one of the reviewed unregistered pairs.
func TestResourceTypeLiterals_AllInRegistry(t *testing.T) {
	dir := findHubDir(t)
	fset := token.NewFileSet()

	hubFiles := parseGoDir(t, fset, dir, true)
	permFiles := parseGoDir(t, fset, filepath.Join(dir, "permissions"), false)

	permConsts := map[string]string{}
	for _, f := range permFiles {
		for k, v := range stringConsts(f) {
			permConsts[k] = v
		}
	}

	// Group the files by package directory ("." for pkg/hub, or the
	// guardScanSubdirs entry a file is in), so each file's identifiers
	// resolve only against the string constants of its own package.
	byPkg := map[string]map[string]*ast.File{}
	for name, f := range hubFiles {
		pkg := path.Dir(name)
		if byPkg[pkg] == nil {
			byPkg[pkg] = map[string]*ast.File{}
		}
		byPkg[pkg][name] = f
	}
	pkgs := make([]string, 0, len(byPkg))
	for pkg := range byPkg {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	var found, helperValues []resourceTypeLiteral
	var unresolved, receivers, unfollowed []string
	helperCalls, helperTagged, helperUnresolved := map[string]int{}, map[string]int{}, map[string]int{}
	for _, pkg := range pkgs {
		localConsts := map[string]string{}
		for _, f := range byPkg[pkg] {
			for k, v := range stringConsts(f) {
				localConsts[k] = v
			}
		}
		scan := collectResourceTypeLiterals(fset, byPkg[pkg], localConsts, permConsts, otherComputeCapabilitiesReceivers)
		found = append(found, scan.found...)
		unresolved = append(unresolved, scan.unresolved...)
		receivers = append(receivers, scan.otherReceivers...)
		helperValues = append(helperValues, scan.helperValues...)
		unfollowed = append(unfollowed, scan.unfollowed...)
		for method, n := range scan.helperCalls {
			helperCalls[method] += n
		}
		for method, n := range scan.helperTagged {
			helperTagged[method] += n
		}
		for method, n := range scan.helperUnresolved {
			helperUnresolved[method] += n
		}
	}
	require.NotEmpty(t, found, "scanner found no Resource{Type: ...} literals; the matcher is broken")
	for _, method := range []string{"ComputeCapabilities", "ComputeCapabilitiesBatch", "ComputeScopeCapabilities"} {
		require.NotZero(t, helperCalls[method],
			"scanner saw no %s call sites; the matcher is broken", method)
	}

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
		"Resource literals or capability helper arguments use a type that "+
			"is not in permissions.Registry. "+
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

	assert.Equal(t, otherComputeCapabilitiesReceivers, receivers,
		"the ComputeCapabilities calls on other types changed. Pin a new "+
			"receiver of another type in otherComputeCapabilitiesReceivers, "+
			"or remove a pin that matches no call")

	// Action-map check. Each helper must read at least one resolved
	// value, so a renamed helper cannot leave the check with nothing to
	// check, and each helper call must resolve. perHelper counts values
	// (a constructor call can reach several, and one constructor value
	// serves all its calls); helperTagged and helperUnresolved count
	// calls.
	perHelper := map[string]int{}
	for _, v := range helperValues {
		perHelper[v.helper]++
	}
	for _, method := range []string{"ComputeCapabilities", "ComputeCapabilitiesBatch", "ComputeScopeCapabilities"} {
		require.NotZero(t, perHelper[method],
			"no %s argument resolved to a type; the action-map check has nothing to check", method)
		assert.Equal(t, helperCalls[method], helperTagged[method]+helperUnresolved[method],
			"each %s call must be counted once, as resolved or unresolved", method)
		assert.Zero(t, helperUnresolved[method],
			"a %s argument is unresolved, so its type cannot be checked against "+
				"the action map. Pass a string literal, a permissions constant or "+
				"a Resource constructor the guard can follow", method)
	}
	t.Logf("capability helper type values: ComputeCapabilities=%d ComputeCapabilitiesBatch=%d ComputeScopeCapabilities=%d",
		perHelper["ComputeCapabilities"], perHelper["ComputeCapabilitiesBatch"], perHelper["ComputeScopeCapabilities"])
	assert.Empty(t, unfollowed,
		"a Resource constructor passed to ComputeCapabilities builds no "+
			"Resource type the guard can resolve. Build the Resource with a "+
			"literal Type in that constructor or in a package-level "+
			"constructor it calls")
	assert.Empty(t, missingActionMapEntries(helperValues, ResourceActions, ScopeActions),
		"a capability helper reads a resource type that has no entry in "+
			"the action map it uses (ResourceActions for ComputeCapabilities "+
			"and ComputeCapabilitiesBatch, ScopeActions for the resource "+
			"type of ComputeScopeCapabilities), so the helper returns no "+
			"actions for it. Use a type with capability actions in "+
			"permissions.Registry, or give the type the CapabilityKind entries "+
			"the helper needs")
}

// unresolvedResourceTypeValues pins the Resource type values, as
// "file: expr", that the guard cannot resolve statically. These were
// present when the pin was added; a new entry needs a review that the
// value can only hold registry types. Keep it sorted.
var unresolvedResourceTypeValues = []string{
	"audit_authz.go: req.Resource.Type",
	"authorized_list.go: resourceType",
	"authz_hub_target.go: meta.Resource",
	"authz_hub_target.go: meta.Resource",
	"authz_hub_target.go: resourceType",
	"authz_relationship_rules.go: kind",
	// materialDeliveryResource: kind is a MaterialKind, which launch
	// selection sets only to MaterialKindSecret or MaterialKindEnvVar
	// ("secret", "env_var"), both registry resource types.
	"material_resources.go: string(kind)",
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

	scan := collectResourceTypeLiterals(fset, files, stringConsts(f), permConsts, nil)
	found := scan.found
	assert.Equal(t, []string{"sample.go: param", "sample.go: localType"}, scan.unresolved)
	assert.Len(t, found, 7)
	assert.Equal(t, []string{
		`sample.go:6: "another_unknown_type"`,
		`sample.go:7: "not_a_registry_type"`,
		`sample.go:11: "elided_slice_type"`,
		`sample.go:12: "elided_map_type"`,
		`sample.go:14: "assigned_type"`,
	}, unknownResourceTypeLiterals(found, knownResourceTypes()))
}

// TestResourceTypeLiterals_PointerLiteralsAndMultiValueAssignments covers
// two forms: a pointer literal passed to a logOnlyResourceFuncs function is
// skipped like a value literal, and a Type set by a multi-value assignment
// is reported as unresolved.
func TestResourceTypeLiterals_PointerLiteralsAndMultiValueAssignments(t *testing.T) {
	const src = `package hub

func f() {
	logAuthzDenial(nil, nil, &Resource{Type: "pointer_log_label_only"}, "read", "r")
	var r Resource
	var err error
	r.Type, err = pick()
	_ = err
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	files := map[string]*ast.File{"sample.go": f}

	scan := collectResourceTypeLiterals(fset, files, stringConsts(f), nil, nil)
	assert.Empty(t, scan.found, "a pointer literal passed to a log-only function must be skipped")
	assert.Equal(t, []string{"sample.go: pick()"}, scan.unresolved,
		"a Type set by a multi-value assignment must be reported as unresolved")
}

// TestCapabilityHelperArgs_DetectsUnknownType proves the guard checks the
// arguments of the capability helpers: an unknown resource type string is
// reported, a value it cannot resolve and a helper method value are
// unresolved, and a pinned receiver of another type is skipped and
// reported as matched.
func TestCapabilityHelperArgs_DetectsUnknownType(t *testing.T) {
	const src = `package hub

const localType = "not_a_registry_type"

func f(s *Server, rt string, res Resource) {
	_ = s.authzService.ComputeCapabilitiesBatch(nil, nil, nil, "batch_unknown_type")
	_ = s.authzService.ComputeScopeCapabilities(nil, nil, "", "", localType)
	_ = s.authzService.ComputeScopeCapabilities(nil, nil, "", "", permissions.ResourceBroker)
	_ = s.authzService.ComputeCapabilitiesBatch(nil, nil, nil, rt)
	_ = s.authzService.ComputeCapabilities(nil, nil, sampleResource())
	_ = s.authzService.ComputeCapabilities(nil, nil, Resource{Type: "compute_literal_type"})
	_ = s.authzService.ComputeCapabilities(nil, nil, res)
	_ = s.membershipService.ComputeCapabilities(nil, "u", "p")
	scope := s.authzService.ComputeScopeCapabilities
	_ = scope
}

func sampleResource() Resource { return Resource{Type: "agent"} }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	files := map[string]*ast.File{"sample.go": f}
	permConsts := map[string]string{"ResourceBroker": permissions.ResourceBroker}
	skip := []string{"sample.go: s.membershipService", "sample.go: s.unusedService"}

	scan := collectResourceTypeLiterals(fset, files, stringConsts(f), permConsts, skip)
	assert.Equal(t, []string{
		"sample.go: rt",
		"sample.go: res",
		"sample.go: s.authzService.ComputeScopeCapabilities",
	}, scan.unresolved, "a non-constant helper argument or a helper method value must be unresolved")
	assert.Len(t, scan.found, 5)
	assert.Equal(t, []string{
		`sample.go:6: "batch_unknown_type"`,
		`sample.go:7: "not_a_registry_type"`,
		`sample.go:11: "compute_literal_type"`,
	}, unknownResourceTypeLiterals(scan.found, knownResourceTypes()))
	assert.Equal(t, map[string]int{
		"ComputeCapabilities":      3,
		"ComputeCapabilitiesBatch": 2,
		"ComputeScopeCapabilities": 2,
	}, scan.helperCalls)
	assert.Equal(t, []string{"sample.go: s.membershipService"}, scan.otherReceivers,
		"only a pin that matches a call is reported, so a stale pin fails the guard")
}

// TestCapabilityHelperTypes_DetectsMissingActionMapEntry proves the
// action-map check reads each helper's type against the map that helper
// uses: ResourceActions for ComputeCapabilitiesBatch and
// ComputeCapabilities, and ScopeActions for the last argument of
// ComputeScopeCapabilities. The scopeType argument and a Resource literal
// passed to no helper are not checked. A constructor is followed into the
// constructors it calls. A constructor that builds no resolved type, or
// sets Type in a form the scan does not follow, is reported.
func TestCapabilityHelperTypes_DetectsMissingActionMapEntry(t *testing.T) {
	const src = `package hub

func f(s *Server, a *AuthzService) {
	_ = s.authzService.ComputeCapabilitiesBatch(nil, nil, nil, "res_type")
	_ = s.authzService.ComputeCapabilitiesBatch(nil, nil, nil, "scope_type")
	_ = s.authzService.ComputeScopeCapabilities(nil, nil, "unchecked_scope", "", "scope_type")
	_ = s.authzService.ComputeScopeCapabilities(nil, nil, "", "", "res_type")
	_ = a.ComputeCapabilities(nil, nil, Resource{Type: "res_type"})
	_ = a.ComputeCapabilities(nil, nil, &Resource{Type: "scope_type"})
	_ = a.ComputeCapabilities(nil, nil, Resource{ID: "x"})
	_ = a.ComputeCapabilities(nil, nil, outerResource())
	_ = a.ComputeCapabilities(nil, nil, outerResource())
	_ = Resource{Type: "plain_only"}
	var rt string
	rt = "scope_type"
	_ = s.authzService.ComputeCapabilitiesBatch(nil, nil, nil, rt)
}

func outerResource() Resource {
	if pick() {
		return Resource{Type: "scope_type"}
	}
	return innerResource()
}

func innerResource() Resource { return Resource{Type: "res_type"} }

func emptyResource() Resource { return Resource{} }

func g(a *AuthzService) {
	_ = a.ComputeCapabilities(nil, nil, emptyResource())
	_ = a.ComputeCapabilities(nil, nil, a.methodResource())
	_ = a.ComputeCapabilities(nil, nil, overriddenResource())
	_ = a.ComputeCapabilities(nil, nil, parenTypedResource())
}

func overriddenResource() Resource {
	r := innerResource()
	r.Type = pickType()
	(r).Type = pickType()
	(r.Type) = pickType()
	return r
}

func parenTypedResource() Resource {
	var r Resource
	(r).Type = "res_type"
	return r
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	files := map[string]*ast.File{"sample.go": f}

	scan := collectResourceTypeLiterals(fset, files, stringConsts(f), nil, nil)
	resourceActions := map[string][]Action{"res_type": {ActionRead}}
	scopeActions := map[string][]Action{"scope_type": {ActionList}}

	assert.Equal(t, []string{
		`sample.go:5: ComputeCapabilitiesBatch: ResourceActions: "scope_type"`,
		`sample.go:7: ComputeScopeCapabilities: ScopeActions: "res_type"`,
		`sample.go:9: ComputeCapabilities: ResourceActions: "scope_type"`,
		`sample.go:10: ComputeCapabilities: ResourceActions: ""`,
		`sample.go:15: ComputeCapabilitiesBatch: ResourceActions: "scope_type"`,
		`sample.go:21: ComputeCapabilities: ResourceActions: "scope_type" (outerResource, called at sample.go:11, sample.go:12)`,
	}, missingActionMapEntries(scan.helperValues, resourceActions, scopeActions))
	require.Len(t, scan.helperValues, 12,
		"helper values: 8 at calls (not the plain literal at line 13) and 4 from constructors")
	assert.Equal(t, `innerResource via outerResource, called at sample.go:11, sample.go:12`,
		scan.helperValues[9].site, "a constructor is followed into the constructors it calls")
	assert.Equal(t, resourceTypeLiteral{
		pos:      "sample.go:47",
		typeName: "res_type",
		fn:       "parenTypedResource",
		helper:   "ComputeCapabilities",
		site:     "parenTypedResource, called at sample.go:34",
	}, scan.helperValues[11], "a (r).Type write on a Resource variable is a resolved value")
	assert.Equal(t, []string{
		"emptyResource, called at sample.go:31",
		"overriddenResource, called at sample.go:33: Type write not followed at sample.go:39: r.Type",
		"overriddenResource, called at sample.go:33: Type write not followed at sample.go:40: (r).Type",
		"overriddenResource, called at sample.go:33: Type write not followed at sample.go:41: (r.Type)",
	}, scan.unfollowed,
		"a constructor that builds no resolved type, or sets Type in a form the scan does not follow, must be reported")
	assert.Equal(t, []string{"sample.go: a.methodResource()"}, scan.unresolved,
		"a method call passed to ComputeCapabilities must be unresolved")
	assert.Equal(t, map[string]int{
		"ComputeCapabilities":      9,
		"ComputeCapabilitiesBatch": 3,
		"ComputeScopeCapabilities": 2,
	}, scan.helperCalls)
	assert.Equal(t, map[string]int{
		"ComputeCapabilities":      6,
		"ComputeCapabilitiesBatch": 3,
		"ComputeScopeCapabilities": 2,
	}, scan.helperTagged, "the unfollowed constructor calls must not be counted")
	assert.Equal(t, map[string]int{"ComputeCapabilities": 1}, scan.helperUnresolved)
	assert.Equal(t, []string{
		`sample.go:99: no action map for helper "Unknown"`,
	}, missingActionMapEntries([]resourceTypeLiteral{{pos: "sample.go:99", typeName: "res_type", helper: "Unknown"}},
		resourceActions, scopeActions), "a helper with no action map must be reported")
}

// TestResourceTypeValues_LocalVarAndHelperParam covers the two variable
// forms the guard resolves: a local string variable resolves to each value
// assigned to it in its function, and a capability helper's resource type
// parameter is covered by the call-site check. Each form is unresolved
// when the variable is written in a form the guard does not follow.
func TestResourceTypeValues_LocalVarAndHelperParam(t *testing.T) {
	const src = `package hub

const constType = "agent"

func (a *AuthzService) ComputeScopeCapabilities(ctx context.Context, identity Identity, scopeType, scopeID, resourceType string) *Capabilities {
	_ = Resource{Type: resourceType}
	return nil
}

func (a *AuthzService) ComputeCapabilitiesBatch(ctx context.Context, identity Identity, resources []Resource, resourceType string) []*Capabilities {
	resourceType = "reassigned_type"
	_ = Resource{Type: resourceType}
	return nil
}

func literalAssignments(k int) {
	var t string
	switch k {
	case 1:
		t = "local_unknown_type"
	case 2:
		t = constType
	}
	_ = Resource{Type: t}
}

func callAssignment() {
	var viaCall string
	viaCall = pick()
	_ = Resource{Type: viaCall}
}

func addressTaken() {
	var viaAddr string
	viaAddr = "agent"
	p := &viaAddr
	_ = p
	_ = Resource{Type: viaAddr}
}

func closureAssigned() {
	var viaClosure string
	viaClosure = "agent"
	func() { viaClosure = "closure_type" }()
	_ = Resource{Type: viaClosure}
}

func neverAssigned() {
	var unset string
	_ = Resource{Type: unset}
}

func parenAssigned() {
	var viaParen string
	viaParen = "agent"
	(viaParen) = pick()
	_ = Resource{Type: viaParen}
}

func parenAddressTaken() {
	var viaParenAddr string
	viaParenAddr = "agent"
	p := &(viaParenAddr)
	_ = p
	_ = Resource{Type: viaParenAddr}
}

func parenRange(names []string) {
	var viaParenRange string
	viaParenRange = "agent"
	for _, (viaParenRange) = range names {
	}
	_ = Resource{Type: viaParenRange}
}

func compoundAssigned() {
	var viaCompound string
	viaCompound = "agent"
	viaCompound += "_suffix"
	_ = Resource{Type: viaCompound}
}

func redeclared() {
	var viaDefine string
	viaDefine = "agent"
	viaDefine, err := "agent", error(nil)
	_ = err
	_ = Resource{Type: viaDefine}
}

func multiValue() {
	var viaMulti string
	viaMulti = "agent"
	viaMulti, _ = pick2()
	_ = Resource{Type: viaMulti}
}

func rangeAssigned(names []string) {
	var viaRange string
	viaRange = "agent"
	for _, viaRange = range names {
	}
	_ = Resource{Type: viaRange}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	require.NoError(t, err)
	files := map[string]*ast.File{"sample.go": f}

	scan := collectResourceTypeLiterals(fset, files, stringConsts(f), nil, nil)
	assert.Equal(t, []string{
		"sample.go: resourceType",
		"sample.go: viaCall",
		"sample.go: viaAddr",
		"sample.go: viaClosure",
		"sample.go: unset",
		"sample.go: viaParen",
		"sample.go: viaParenAddr",
		"sample.go: viaParenRange",
		"sample.go: viaCompound",
		"sample.go: viaDefine",
		"sample.go: viaMulti",
		"sample.go: viaRange",
	}, scan.unresolved, "a written helper parameter or an unfollowed local variable must be unresolved")
	assert.Len(t, scan.found, 2)
	assert.Equal(t, []string{
		`sample.go:20: "local_unknown_type"`,
	}, unknownResourceTypeLiterals(scan.found, knownResourceTypes()),
		"each value assigned to a local variable is checked at its assignment")
}

// TestUnregisteredResourcePermissions_ExactSet pins the reviewed unregistered
// pairs. Adding one widens what the guard above accepts, so it needs a
// deliberate change here.
func TestUnregisteredResourcePermissions_ExactSet(t *testing.T) {
	assert.Equal(t, map[resourceActionKey]string{}, unregisteredResourcePermissions,
		"unregistered pairs changed; confirm the new pair is needed "+
			"(see ptone/scion#2140) and update this pin")
}
