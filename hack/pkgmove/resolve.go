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

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// generatedAliasMarker is the comment pkgmove writes into every alias file
// (see aliasFile). Wrapper funcs and func var aliases are resolved only in
// files that carry it.
const generatedAliasMarker = "by hack/pkgmove, so existing references in package"

// aliasInfo describes a package-level declaration of the source package that
// only forwards to a package-level object of another package, so that a
// reference to it can be replaced by a direct reference to that object
// without changing behaviour.
type aliasInfo struct {
	obj       types.Object // the forwarding declaration
	kind      string       // type, const, var (func var alias) or func (wrapper)
	path      string       // import path of the package that defines the target
	pkgName   string       // its package name
	name      string       // the target object's name
	file      *srcFile
	generated bool // declared in a pkgmove-generated alias file

	// Declaration nodes, for removal (-rewrite-aliases).
	gen  *ast.GenDecl  // enclosing GenDecl of a type/const/var spec
	spec ast.Spec      // the spec, or nil for a wrapper
	fn   *ast.FuncDecl // the wrapper, or nil
}

// target is the qualified spelling of the alias target, for messages.
func (al *aliasInfo) target() string { return al.path + "." + al.name }

// isGeneratedAliasFile reports whether f is an alias file written by pkgmove.
func isGeneratedAliasFile(f *srcFile) bool {
	return strings.HasPrefix(f.Name, "zz_alias_") && strings.Contains(string(f.Src), generatedAliasMarker)
}

// qualifiedObj resolves expr when it is exactly pkg.Name, a package-level
// object of another package.
func (a *analysis) qualifiedObj(expr ast.Expr) (types.Object, *types.PkgName) {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return nil, nil
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil, nil
	}
	pn, ok := a.info.Uses[x].(*types.PkgName)
	if !ok {
		return nil, nil
	}
	obj := a.info.Uses[sel.Sel]
	if obj == nil || obj.Pkg() == nil || obj.Pkg() == a.pkg || obj.Parent() != obj.Pkg().Scope() || !obj.Exported() {
		return nil, nil
	}
	return obj, pn
}

// forwardedIndex unwraps X[T1, ..., Tn] when the indices are exactly the
// identifiers of names, in order; with no names it accepts only a plain X.
func forwardedIndex(expr ast.Expr, names []string) (ast.Expr, bool) {
	var x ast.Expr
	var idx []ast.Expr
	switch e := ast.Unparen(expr).(type) {
	case *ast.IndexExpr:
		x, idx = e.X, []ast.Expr{e.Index}
	case *ast.IndexListExpr:
		x, idx = e.X, e.Indices
	default:
		return expr, len(names) == 0
	}
	if len(idx) != len(names) {
		return nil, false
	}
	for i, e := range idx {
		id, ok := e.(*ast.Ident)
		if !ok || id.Name != names[i] {
			return nil, false
		}
	}
	return x, true
}

func fieldNames(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, f := range fl.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// collectAliases finds the forwarding declarations of the source package:
//   - type aliases `type X = pkg.Y` and `type X[P any] = pkg.Y[P]`, anywhere;
//   - consts `const X = pkg.Y` (one name, no type, one value), anywhere;
//   - in pkgmove-generated alias files only: func var aliases `var X = pkg.F`
//     that nothing assigns or takes the address of, and wrappers
//     `func x(p0 T) R { return pkg.F(p0) }` whose signature is identical to
//     the target's.
//
// Wrappers elsewhere could hide behaviour (a recover() or a call-stack
// inspection in the target behaves differently through a wrapper frame); the
// generator never wraps such functions, so generated wrappers are safe to see
// through. Declarations that have the forwarding shape but are not resolved
// are recorded in unresolvedWhy, so the back-reference error can say why.
func (a *analysis) collectAliases() {
	a.aliases = map[types.Object]*aliasInfo{}
	a.unresolvedWhy = map[types.Object]string{}
	var varAliases []*aliasInfo
	for _, f := range a.checked {
		gen := isGeneratedAliasFile(f)
		for _, d := range f.AST.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if !s.Assign.IsValid() {
							continue
						}
						tn, ok := a.info.Defs[s.Name].(*types.TypeName)
						if !ok || !a.isPkgLevel(tn) {
							continue
						}
						x, ok := forwardedIndex(s.Type, fieldNames(s.TypeParams))
						if !ok {
							continue
						}
						if t, pn := a.qualifiedObj(x); t != nil {
							if _, isType := t.(*types.TypeName); isType {
								a.aliases[tn] = &aliasInfo{obj: tn, kind: "type", path: pn.Imported().Path(), pkgName: pn.Imported().Name(), name: t.Name(), file: f, generated: gen, gen: d, spec: s}
							}
						}
					case *ast.ValueSpec:
						if len(s.Names) != 1 || s.Type != nil || len(s.Values) != 1 || s.Names[0].Name == "_" {
							continue
						}
						obj := a.info.Defs[s.Names[0]]
						if obj == nil || !a.isPkgLevel(obj) {
							continue
						}
						t, pn := a.qualifiedObj(s.Values[0])
						if t == nil {
							continue
						}
						al := &aliasInfo{obj: obj, path: pn.Imported().Path(), pkgName: pn.Imported().Name(), name: t.Name(), file: f, generated: gen, gen: d, spec: s}
						switch d.Tok {
						case token.CONST:
							if _, ok := t.(*types.Const); ok {
								al.kind = "const"
								a.aliases[obj] = al
							}
						case token.VAR:
							fn, ok := t.(*types.Func)
							if !ok || fn.Signature().Recv() != nil || fn.Signature().TypeParams().Len() > 0 {
								continue
							}
							if !gen {
								a.unresolvedWhy[obj] = "var " + obj.Name() + " forwards to " + al.target() + " but is not in a pkgmove-generated alias file (a hand-written var may be a hook)"
								continue
							}
							al.kind = "var"
							varAliases = append(varAliases, al)
						}
					}
				}
			case *ast.FuncDecl:
				fn, pn, ok := a.wrapperTarget(d)
				if !ok {
					continue
				}
				obj := a.info.Defs[d.Name]
				al := &aliasInfo{obj: obj, kind: "func", path: pn.Imported().Path(), pkgName: pn.Imported().Name(), name: fn.Name(), file: f, generated: gen, fn: d}
				if !gen {
					a.unresolvedWhy[obj] = "func " + obj.Name() + " forwards to " + al.target() + " but is not in a pkgmove-generated alias file (a hand-written wrapper is not resolved: the target may call recover() or inspect its call stack)"
					continue
				}
				a.aliases[obj] = al
			}
		}
	}
	if len(varAliases) == 0 {
		return
	}
	objs := map[types.Object]bool{}
	for _, al := range varAliases {
		objs[al.obj] = true
	}
	assigned := a.assignedVars(objs)
	for _, al := range varAliases {
		if pos, ok := assigned[al.obj]; ok {
			a.unresolvedWhy[al.obj] = "var alias " + al.obj.Name() + " is assigned or has its address taken at " + pos + ", so it is not equivalent to " + al.target()
			continue
		}
		a.aliases[al.obj] = al
	}
}

// wrapperTarget recognises a function whose body only forwards its
// parameters, in order, to a package-level function of another package with
// an identical signature (explicitly instantiated with its own type
// parameters when generic).
func (a *analysis) wrapperTarget(fd *ast.FuncDecl) (*types.Func, *types.PkgName, bool) {
	if fd.Recv != nil || fd.Body == nil || len(fd.Body.List) != 1 || fd.Name.Name == "init" || fd.Name.Name == "_" {
		return nil, nil, false
	}
	self, ok := a.info.Defs[fd.Name].(*types.Func)
	if !ok || !a.isPkgLevel(self) {
		return nil, nil, false
	}
	sig := self.Signature()
	var call *ast.CallExpr
	switch st := fd.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if sig.Results().Len() == 0 || len(st.Results) != 1 {
			return nil, nil, false
		}
		call, _ = st.Results[0].(*ast.CallExpr)
	case *ast.ExprStmt:
		if sig.Results().Len() != 0 {
			return nil, nil, false
		}
		call, _ = st.X.(*ast.CallExpr)
	}
	if call == nil {
		return nil, nil, false
	}
	x, ok := forwardedIndex(call.Fun, fieldNames(fd.Type.TypeParams))
	if !ok {
		return nil, nil, false
	}
	t, pn := a.qualifiedObj(x)
	fn, ok := t.(*types.Func)
	if !ok || fn.Signature().Recv() != nil {
		return nil, nil, false
	}
	params := sig.Params()
	if len(call.Args) != params.Len() || call.Ellipsis.IsValid() != sig.Variadic() {
		return nil, nil, false
	}
	for i, arg := range call.Args {
		id, ok := arg.(*ast.Ident)
		if !ok || id.Name == "_" || a.info.Uses[id] != types.Object(params.At(i)) {
			return nil, nil, false
		}
	}
	inst, ok := a.info.TypeOf(call.Fun).(*types.Signature)
	if !ok || inst.Variadic() != sig.Variadic() || !types.Identical(inst.Params(), sig.Params()) || !types.Identical(inst.Results(), sig.Results()) {
		return nil, nil, false
	}
	return fn, pn, true
}

// assignedVars returns, for each of objs, the first position where it is
// assigned, incremented, ranged into or has its address taken: in the checked
// files (by type information), in the source directory's files excluded by
// build constraints and its external tests (by name), and, for exported
// names, in every package of the module that imports the source package.
func (a *analysis) assignedVars(objs map[types.Object]bool) map[types.Object]string {
	out := map[types.Object]string{}
	note := func(obj types.Object, pos string) {
		if _, ok := out[obj]; !ok {
			out[obj] = pos
		}
	}
	byName := map[string]types.Object{}
	exported := map[string]types.Object{}
	for o := range objs {
		byName[o.Name()] = o
		if o.Exported() {
			exported[o.Name()] = o
		}
	}
	for _, f := range a.checked {
		visitAssigned(f.AST, func(e ast.Expr) {
			if id, ok := e.(*ast.Ident); ok && objs[a.info.Uses[id]] {
				note(a.info.Uses[id], a.posOf(id.Pos()))
			}
		})
	}
	for _, f := range a.files {
		if f.Included && !f.XTest {
			continue
		}
		local := ""
		if f.XTest {
			if local = a.srcImportName(f); local == "" {
				continue
			}
		}
		visitAssigned(f.AST, func(e ast.Expr) {
			switch e := e.(type) {
			case *ast.Ident:
				if o := byName[e.Name]; o != nil && !f.XTest {
					note(o, a.posOf(e.Pos()))
				}
			case *ast.SelectorExpr:
				if x, ok := e.X.(*ast.Ident); ok && f.XTest && x.Name == local {
					if o := exported[e.Sel.Name]; o != nil {
						note(o, a.posOf(e.Pos()))
					}
				}
			}
		})
	}
	if len(exported) == 0 {
		return out
	}
	pkgs, err := a.modulePackages()
	if err != nil {
		a.plan.errorf("scanning the module for assignments to var aliases: %v", err)
		return out
	}
	fset := token.NewFileSet()
	for _, p := range pkgs {
		if p.ImportPath == a.mod.ImportPath {
			continue
		}
		var names []string
		if contains(p.Imports, a.mod.ImportPath) || contains(p.TestImports, a.mod.ImportPath) || contains(p.XTestImports, a.mod.ImportPath) {
			names = append(names, p.GoFiles...)
			names = append(names, p.TestGoFiles...)
			names = append(names, p.XTestGoFiles...)
		}
		names = append(names, p.IgnoredGoFiles...)
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				continue
			}
			local := ""
			for _, spec := range f.Imports {
				if ip, _ := strconv.Unquote(spec.Path.Value); ip == a.mod.ImportPath {
					local = a.srcName
					if spec.Name != nil {
						local = spec.Name.Name
					}
				}
			}
			if local == "" {
				continue
			}
			visitAssigned(f, func(e ast.Expr) {
				if sel, ok := e.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
						if o := exported[sel.Sel.Name]; o != nil {
							pos := fset.Position(sel.Pos())
							note(o, a.rel(pos.Filename)+":"+strconv.Itoa(pos.Line))
						}
					}
				}
			})
		}
	}
	return out
}

// visitAssigned calls visit for every expression that is assigned to,
// incremented or decremented, ranged into, or has its address taken.
func visitAssigned(n ast.Node, visit func(ast.Expr)) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				visit(ast.Unparen(l))
			}
		case *ast.IncDecStmt:
			visit(ast.Unparen(n.X))
		case *ast.RangeStmt:
			if n.Tok == token.ASSIGN {
				if n.Key != nil {
					visit(ast.Unparen(n.Key))
				}
				if n.Value != nil {
					visit(ast.Unparen(n.Value))
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND {
				visit(ast.Unparen(n.X))
			}
		}
		return true
	})
}

// embeddedFieldAt returns the embedded field that id declares, if any (the
// type name of an embedded field is both a use of the type and the
// definition of the field).
func (a *analysis) embeddedFieldAt(id *ast.Ident) *types.Var {
	if v, ok := a.info.Defs[id].(*types.Var); ok && v.IsField() && v.Embedded() {
		return v
	}
	return nil
}

// checkAliasUses validates the references from moved files to aliases of
// the source package; buildEdits rewrites them to the alias targets.
func (a *analysis) checkAliasUses() {
	for _, u := range a.aliasUses {
		al := a.aliases[u.obj]
		pos := a.posOf(u.id.Pos())
		if v := a.embeddedFieldAt(u.id); v != nil && al.name != u.obj.Name() {
			a.plan.errorf("%s: moved file embeds the alias %s (of %s); referring to the target directly renames the embedded field from %s to %s - rename the field by hand first",
				pos, u.obj.Name(), al.target(), u.obj.Name(), al.name)
			continue
		}
		if al.kind == "func" && !a.callFuns[u.id] {
			a.plan.add(levelWarn, "func value through a wrapper alias resolved to its target", pos,
				"%s is a wrapper of %s used as a value; the moved file now uses %s itself, so its identity (reflect Pointer, runtime.FuncForPC name) differs from staying uses of the wrapper",
				u.obj.Name(), al.target(), al.target())
		}
		if a.bareTarget(u.file, al) && a.shadowedAt(al.name, []token.Pos{u.id.Pos()}, false) {
			a.plan.errorf("%s: %s (an alias of %s) would become the bare name %s, which a local declaration shadows here - rename the local first",
				pos, u.obj.Name(), al.target(), al.name)
		}
		if a.intoExisting && !u.file.XTest && al.path != a.dstImport && a.dependsOnDst(al.path) {
			a.plan.errorf("%s: %s resolves to %s, but %s imports %s, so the moved test would create an import cycle - move it as an external test (package %s_test) or keep it",
				pos, u.obj.Name(), al.target(), al.path, a.dstImport, a.srcName)
		}
	}
}

// aliasText is the reference text that replaces a use of al in file f, given
// the import name q chosen for al.path ("" when al.path is the package f
// moves into, so the reference becomes a bare identifier).
func aliasText(al *aliasInfo, q string) string {
	if q == "" {
		return al.name
	}
	return q + "." + al.name
}

// importNameFor returns the name file f can use for path at the given
// positions: its existing import name if it imports path (and nothing
// shadows it there), or a fresh name based on base that does not collide
// with taken or the file's names. The second result reports whether an
// import must be added.
func (a *analysis) importNameFor(f *srcFile, path, base string, at []token.Pos, taken map[string]bool) (string, bool) {
	for _, spec := range f.AST.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p != path {
			continue
		}
		name := importName(spec)
		if spec.Name == nil {
			if pn, ok := a.info.Implicits[spec].(*types.PkgName); ok {
				name = pn.Name()
			}
		}
		if name == "_" || name == "." {
			continue
		}
		if !a.shadowedAt(name, at, true) {
			return name, false
		}
	}
	reserved := map[string]bool{}
	for k := range taken {
		reserved[k] = true
	}
	for k := range a.dstScope {
		if !f.XTest {
			reserved[k] = true
		}
	}
	return a.chooseImportNameBase(f, base, at, reserved), true
}

// shadowedAt reports whether name resolves to a non-package-level object at
// any of the positions (in the source package's scopes). With allowImport, a
// file-scope import name does not count (callers that reuse that import).
func (a *analysis) shadowedAt(name string, at []token.Pos, allowImport bool) bool {
	for _, pos := range at {
		s := a.pkg.Scope().Innermost(pos)
		if s == nil {
			continue
		}
		if _, obj := s.LookupParent(name, pos); obj != nil && obj.Parent() != a.pkg.Scope() && obj.Parent() != types.Universe {
			if _, isPkg := obj.(*types.PkgName); isPkg && allowImport {
				continue
			}
			return true
		}
	}
	return false
}
