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
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"
)

// Test-only moves into an existing package.
//
// When the target directory already holds a package, only _test.go files
// (and assets) may move into it. Nothing in the source package is aliased or
// renamed: the moved tests keep their package kind (in-package tests join the
// target package, external tests join <target>_test), their references to
// source aliases of target symbols become bare names (or <target>.X in
// external tests), and their references to other packages stay qualified.

// declRef is one package-level declaration (func, or a spec of a GenDecl).
type declRef struct {
	name string
	kind string // func, type, var, const
	file *srcFile
	node ast.Node // *ast.FuncDecl, *ast.TypeSpec or *ast.ValueSpec
	gen  *ast.GenDecl
}

// pkgDecls returns the package-level declarations of a file (no methods,
// no blank names, no init functions).
func pkgDecls(f *srcFile) []declRef {
	var out []declRef
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name != "_" && d.Name.Name != "init" {
				out = append(out, declRef{name: d.Name.Name, kind: "func", file: f, node: d})
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					out = append(out, declRef{name: s.Name.Name, kind: "type", file: f, node: s, gen: d})
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name != "_" {
							out = append(out, declRef{name: n.Name, kind: strings.ToLower(d.Tok.String()), file: f, node: s, gen: d})
						}
					}
				}
			}
		}
	}
	return out
}

// existingPkgName returns the package name of the Go files already in dir,
// or "" when it has none.
func existingPkgName(dir string, tags []string) (string, error) {
	files, err := readDir(token.NewFileSet(), dir, buildContext(tags))
	if err != nil {
		return "", err
	}
	// Files the build excludes (such as a //go:build ignore generator in
	// package main) may belong to another package; prefer included files.
	for _, included := range []bool{true, false} {
		for _, f := range files {
			if f.Included == included && !f.XTest {
				return f.PkgName, nil
			}
		}
	}
	for _, f := range files {
		return strings.TrimSuffix(f.PkgName, "_test"), nil
	}
	return "", nil
}

// setupIntoExisting validates a move into an existing package and records
// the target's declarations. It runs before the source is type-checked.
func (a *analysis) setupIntoExisting(dstFiles []*srcFile) error {
	var nonTest []string
	for _, f := range a.files {
		if f.Moved && !f.IsTest {
			nonTest = append(nonTest, f.Name)
		}
	}
	if len(nonTest) > 0 {
		return fmt.Errorf("target directory %s already contains Go files; moving into an existing package is supported only for _test.go file sets (and assets), but the move set includes %s - source moves always create a new package",
			a.rel(a.cfg.DstDir), strings.Join(nonTest, ", "))
	}
	a.intoExisting = true
	a.dstScope = map[string]bool{}
	a.dstDecls = map[string]declRef{}
	a.dstXDecls = map[string]declRef{}
	for _, f := range dstFiles {
		want := a.cfg.PkgName
		if f.XTest {
			want += "_test"
		}
		if f.PkgName != want {
			if !f.Included {
				continue // not part of the package (for example a //go:build ignore program)
			}
			return fmt.Errorf("%s is in package %s, but the move targets package %s (pass -name to match the existing package)", a.rel(f.Path), f.PkgName, want)
		}
		a.dstFiles = append(a.dstFiles, f)
		for _, d := range pkgDecls(f) {
			m := a.dstDecls
			if f.XTest {
				m = a.dstXDecls
			} else {
				a.dstScope[d.name] = true
			}
			if _, dup := m[d.name]; !dup {
				m[d.name] = d
			}
		}
	}
	return nil
}

// dependsOnDst reports whether importing path from the target package would
// create an import cycle.
func (a *analysis) dependsOnDst(path string) bool {
	return path == a.dstImport || contains(a.imp.deps[path], a.dstImport)
}

// checkIntoExisting reports collisions with the target's declarations,
// import cycles and renames for a test-only move into an existing package,
// and decides which duplicate helpers are dropped as equivalent.
func (a *analysis) checkIntoExisting() {
	if len(a.plan.Renames) > 0 {
		var rs []string
		for _, r := range a.plan.Renames {
			rs = append(rs, r.Owner+"."+r.Old)
		}
		sort.Strings(rs)
		a.plan.errorf("a test-only move into an existing package must not rename source symbols, but the move needs %s (moved test types share unexported method names with staying interfaces) - restructure first",
			strings.Join(dedupStrings(rs), ", "))
	}
	moved := map[string]declRef{}  // in-package moved declarations
	movedX := map[string]declRef{} // external moved declarations
	var order []declRef
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		for _, d := range pkgDecls(f) {
			order = append(order, d)
			if f.XTest {
				movedX[d.name] = d
			} else {
				moved[d.name] = d
			}
		}
	}
	for _, d := range order {
		pos := a.posOf(d.node.Pos())
		target := a.dstDecls
		if d.file.XTest {
			target = a.dstXDecls
		}
		t, ok := target[d.name]
		if !ok {
			continue
		}
		where := a.posOf(t.node.Pos())
		if !d.file.XTest && (d.kind == "func" || d.kind == "const") && a.helperEquivalent(d.name) {
			a.drops = append(a.drops, d)
			a.plan.Dropped = append(a.plan.Dropped, fmt.Sprintf("%s: %s %s (the target already declares an equivalent one at %s)", pos, d.kind, d.name, where))
			continue
		}
		why := "only funcs and consts of in-package tests can be reused"
		if !d.file.XTest && (d.kind == "func" || d.kind == "const") {
			why = "the declarations are not equivalent" + a.equivalenceNote(d.name)
		}
		a.plan.errorf("%s: moved %s %s collides with the existing %s %s at %s (%s) - rename one of them first",
			pos, d.kind, d.name, t.kind, t.name, where, why)
	}
	dropped := map[string]bool{}
	for _, d := range a.drops {
		dropped[d.name] = true
	}
	// A file-scope import name may not also be declared at package level.
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		target := a.dstDecls
		if f.XTest {
			target = a.dstXDecls
		}
		for _, spec := range f.AST.Imports {
			name := a.fileImportName(spec)
			if t, ok := target[name]; ok && name != "_" && name != "." {
				a.plan.errorf("%s: the import name %s collides with %s %s declared in the target at %s", a.posOf(spec.Pos()), name, t.kind, t.name, a.posOf(t.node.Pos()))
			}
		}
	}
	for _, f := range a.dstFiles {
		mine := moved
		if f.XTest {
			mine = movedX
		}
		for _, spec := range f.AST.Imports {
			name := importName(spec)
			if d, ok := mine[name]; ok && !dropped[name] {
				a.plan.errorf("%s: moved %s %s collides with the import name %s in %s", a.posOf(d.node.Pos()), d.kind, d.name, name, a.rel(f.Path))
			}
		}
	}
	// In-package tests become part of the target package: they cannot import
	// it, or anything that imports it.
	for _, f := range a.files {
		if !f.Moved || f.XTest {
			continue
		}
		for _, spec := range f.AST.Imports {
			p := importPath(spec)
			if p != a.dstImport && a.dependsOnDst(p) {
				a.plan.errorf("%s: the moved in-package test imports %s, which imports %s - the move would create an import cycle; move it as an external test (package %s_test) or keep it",
					a.posOf(spec.Pos()), p, a.dstImport, a.srcName)
			}
		}
	}
}

// fileImportName is the name an import spec of a checked file declares.
func (a *analysis) fileImportName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	if pn, ok := a.info.Implicits[spec].(*types.PkgName); ok {
		return pn.Name()
	}
	return importName(spec)
}

func importPath(spec *ast.ImportSpec) string {
	p, _ := strconv.Unquote(spec.Path.Value)
	return p
}

// collectDstSels finds, in moved in-package test files, selectors on an
// import of the target package; they become bare identifiers.
func (a *analysis) collectDstSels() {
	a.dstSels = map[*srcFile][]*ast.SelectorExpr{}
	for _, f := range a.files {
		if !f.Moved || f.XTest || !f.Included {
			continue
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			pn, ok := a.info.Uses[x].(*types.PkgName)
			if !ok || pn.Imported().Path() != a.dstImport {
				return true
			}
			if a.shadowedAt(sel.Sel.Name, []token.Pos{sel.Pos()}, false) {
				a.plan.errorf("%s: %s.%s would become the bare name %s, which a local declaration shadows here - rename the local first", a.posOf(sel.Pos()), x.Name, sel.Sel.Name, sel.Sel.Name)
			}
			a.dstSels[f] = append(a.dstSels[f], sel)
			return false
		})
	}
}

// ---- Helper equivalence (reuse) ----

// helperEquivalent reports whether the source package's test-level func or
// const name (declared in an in-package test file, moved or staying) is
// equivalent to the target's in-package test declaration of the same name:
// the same kind, the same build constraint, and the same token sequence once
// every identifier is replaced by what it denotes (aliases of the source are
// seen through, test helpers they use must be equivalent in turn).
func (a *analysis) helperEquivalent(name string) bool {
	if a.equiv == nil {
		a.computeEquivalence()
	}
	return a.equiv[name]
}

// equivalenceNote explains why name can never be reused, or returns "".
func (a *analysis) equivalenceNote(name string) string {
	if a.equiv == nil {
		a.computeEquivalence()
	}
	return a.equivNote[name]
}

// computeEquivalence decides helper equivalence for every name declared both
// by an in-package test file of the source and by one of the target, as a
// greatest fixpoint over the helpers they use.
func (a *analysis) computeEquivalence() {
	a.equiv = map[string]bool{}
	// Every in-package declaration of each name, on each side, whatever its
	// build tags. Equivalence is decided in the analysis configuration only,
	// so a name declared more than once (build-tag variants such as
	// limit_unix_test.go and limit_other_test.go) or in a file the analysis
	// tags exclude is never equivalent: the other variants cannot be
	// compared. Callers then report a collision or separation ERROR.
	all := func(files []*srcFile) map[string][]declRef {
		m := map[string][]declRef{}
		for _, f := range files {
			if !f.XTest {
				for _, d := range pkgDecls(f) {
					m[d.name] = append(m[d.name], d)
				}
			}
		}
		return m
	}
	srcAll, dstAll := all(a.files), all(a.dstFiles)
	src := map[string]declRef{}
	var cands []string
	a.equivNote = map[string]string{}
	for name, ts := range dstAll {
		ss := srcAll[name]
		if len(ss) == 0 {
			continue
		}
		if len(ss) != 1 || len(ts) != 1 || !ss[0].file.Included || !ts[0].file.Included {
			a.equivNote[name] = " (" + name + " has build-tag variants or is declared in a file the analysis tags exclude, so it is never reused)"
			continue
		}
		s, t := ss[0], ts[0]
		if !s.file.Included || !t.file.Included || !s.file.IsTest || !t.file.IsTest {
			continue
		}
		if s.kind == t.kind && (s.kind == "func" || s.kind == "const") {
			src[name] = s
			cands = append(cands, name)
		}
	}
	if len(cands) == 0 {
		return
	}
	sort.Strings(cands)
	tinfo, tpkg, err := a.typecheckDst()
	if err != nil {
		a.plan.errorf("type-checking the existing target package (for helper reuse): %v", err)
		return
	}
	deps := map[string][]string{}
	for _, name := range cands {
		s, t := src[name], dstAll[name][0]
		if s.file.Constraint != t.file.Constraint {
			continue
		}
		if vs, ok := s.node.(*ast.ValueSpec); ok && (len(vs.Names) != 1 || len(vs.Values) != 1) {
			continue
		}
		if vs, ok := t.node.(*ast.ValueSpec); ok && (len(vs.Names) != 1 || len(vs.Values) != 1) {
			continue
		}
		st, sdeps := a.canonTokens(s.file, s.node, a.srcResolver())
		tt, _ := a.canonTokens(t.file, t.node, a.dstResolver(tinfo, tpkg))
		if strings.Join(st, " ") != strings.Join(tt, " ") || contains(st, "U(iota)") {
			continue
		}
		a.equiv[name] = true
		deps[name] = sdeps
	}
	for changed := true; changed; {
		changed = false
		for _, name := range cands {
			if !a.equiv[name] {
				continue
			}
			for _, d := range deps[name] {
				if !a.equiv[d] {
					a.equiv[name] = false
					changed = true
					break
				}
			}
		}
	}
}

// typecheckDst type-checks the target package with its in-package tests
// (once).
func (a *analysis) typecheckDst() (*types.Info, *types.Package, error) {
	if a.dstChecked == nil {
		a.dstChecked = &dstCheck{}
		a.dstChecked.info, a.dstChecked.pkg, a.dstChecked.err = a.typecheckDstOnce()
	}
	return a.dstChecked.info, a.dstChecked.pkg, a.dstChecked.err
}

type dstCheck struct {
	info *types.Info
	pkg  *types.Package
	err  error
}

func (a *analysis) typecheckDstOnce() (*types.Info, *types.Package, error) {
	var files []*srcFile
	for _, f := range a.dstFiles {
		if f.Included && !f.XTest {
			files = append(files, f)
		}
	}
	imp, err := newExportImporter(a.fset, a.cfg.DstDir, a.cfg.Tags, importsOf(files, a.dstImport))
	if err != nil {
		return nil, nil, err
	}
	pkg, info, err := typeCheck(a.fset, a.dstImport, files, imp, a.mod.GoVersion)
	return info, pkg, err
}

// resolver canonicalises one identifier: "PKG(path)" for an import name
// (merged with the following selector into "Q(path.name)"), "Q(path.name)"
// for a package-level object of a package, "H(name)" for a test-level
// helper (dep reports it), "U(name)" for the universe, "S(name)" for a
// source object that cannot be referenced from the target, and "L(name)" for
// locals, fields and methods.
type resolver func(id *ast.Ident) (canon string, dep string)

func (a *analysis) srcResolver() resolver {
	return func(id *ast.Ident) (string, string) {
		obj := a.info.Uses[id]
		if obj == nil {
			obj = a.info.Defs[id]
		}
		if obj == nil {
			return "L(" + id.Name + ")", ""
		}
		if pn, ok := obj.(*types.PkgName); ok {
			return "PKG(" + pn.Imported().Path() + ")", ""
		}
		obj = origin(obj)
		switch {
		case obj.Parent() == types.Universe:
			return "U(" + id.Name + ")", ""
		case obj.Pkg() == a.pkg && a.isPkgLevel(obj):
			if al := a.aliases[obj]; al != nil {
				return "Q(" + al.target() + ")", ""
			}
			if h := a.home(obj); h != nil && h.IsTest {
				return "H(" + obj.Name() + ")", obj.Name()
			}
			return "S(" + obj.Name() + ")", ""
		case obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope():
			return "Q(" + obj.Pkg().Path() + "." + obj.Name() + ")", ""
		}
		return "L(" + id.Name + ")", ""
	}
}

func (a *analysis) dstResolver(info *types.Info, pkg *types.Package) resolver {
	testFile := map[string]bool{}
	for _, f := range a.dstFiles {
		testFile[f.Path] = f.IsTest
	}
	return func(id *ast.Ident) (string, string) {
		obj := info.Uses[id]
		if obj == nil {
			obj = info.Defs[id]
		}
		if obj == nil {
			return "L(" + id.Name + ")", ""
		}
		if pn, ok := obj.(*types.PkgName); ok {
			return "PKG(" + pn.Imported().Path() + ")", ""
		}
		obj = origin(obj)
		switch {
		case obj.Parent() == types.Universe:
			return "U(" + id.Name + ")", ""
		case obj.Pkg() == pkg && obj.Parent() == pkg.Scope():
			if testFile[a.fset.Position(obj.Pos()).Filename] {
				return "H(" + obj.Name() + ")", obj.Name()
			}
			return "Q(" + a.dstImport + "." + obj.Name() + ")", ""
		case obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope():
			return "Q(" + obj.Pkg().Path() + "." + obj.Name() + ")", ""
		}
		return "L(" + id.Name + ")", ""
	}
}

// canonTokens renders a declaration as a token sequence in which every
// identifier is replaced by what it denotes (see resolver), so that two
// declarations in different packages compare equal exactly when they mean
// the same thing. Comments, layout, trailing commas and the semicolons
// before a closing bracket are ignored. It also returns the helpers used.
func (a *analysis) canonTokens(f *srcFile, node ast.Node, resolve resolver) ([]string, []string) {
	var start, end int
	prefix := ""
	switch n := node.(type) {
	case *ast.FuncDecl:
		start, end = a.offset(n.Pos()), a.offset(n.End())
	case *ast.ValueSpec:
		start, end = a.offset(n.Pos()), a.offset(n.End())
		prefix = "const"
	default:
		start, end = a.offset(node.Pos()), a.offset(node.End())
	}
	idents := map[int]*ast.Ident{}
	ast.Inspect(node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			idents[a.offset(id.Pos())] = id
		}
		return true
	})
	type tok struct {
		t    token.Token
		lit  string // canonical form
		name string // identifier as written
	}
	var raw []tok
	if prefix != "" {
		raw = append(raw, tok{t: token.CONST})
	}
	fs := token.NewFileSet()
	src := f.Src[start:end]
	file := fs.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var deps []string
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		if t == token.IDENT {
			c := "L(" + lit + ")"
			if id := idents[start+file.Offset(pos)]; id != nil {
				var dep string
				c, dep = resolve(id)
				if dep != "" {
					deps = append(deps, dep)
				}
			}
			raw = append(raw, tok{t, c, lit})
			continue
		}
		if t == token.SEMICOLON {
			lit = ""
		}
		raw = append(raw, tok{t: t, lit: lit})
	}
	var out []string
	for i := 0; i < len(raw); i++ {
		r := raw[i]
		next := token.EOF
		if i+1 < len(raw) {
			next = raw[i+1].t
		}
		if (r.t == token.SEMICOLON || r.t == token.COMMA) && (next == token.RBRACE || next == token.RPAREN || next == token.RBRACK) {
			continue
		}
		if r.t == token.IDENT && strings.HasPrefix(r.lit, "PKG(") && i+2 < len(raw) && raw[i+1].t == token.PERIOD && raw[i+2].t == token.IDENT {
			path := strings.TrimSuffix(strings.TrimPrefix(r.lit, "PKG("), ")")
			out = append(out, "Q("+path+"."+raw[i+2].name+")")
			i += 2
			continue
		}
		if r.lit != "" {
			out = append(out, r.lit)
		} else {
			out = append(out, r.t.String())
		}
	}
	sort.Strings(deps)
	return out, dedupStrings(deps)
}

// ---- TestMain ----

// testMainRef is one TestMain declaration.
type testMainRef struct {
	f  *srcFile
	fd *ast.FuncDecl
}

// testMainDecls finds every TestMain function in files (whatever their build
// tags), in file order.
func testMainDecls(files []*srcFile) []testMainRef {
	var out []testMainRef
	for _, f := range files {
		if !f.IsTest {
			continue
		}
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "TestMain" {
				out = append(out, testMainRef{f, fd})
			}
		}
	}
	return out
}

// delegatingTestMain is the canonical token sequence of
// func TestMain(m *testing.M) { os.Exit(<support>.RunTestMain(m)) }.
func delegatingTestMain(support, param string) string {
	return strings.Join([]string{"func", "H(TestMain)", "(", "L(" + param + ")", "*", "Q(testing.M)", ")", "{",
		"Q(os.Exit)", "(", "Q(" + support + ".RunTestMain)", "(", "L(" + param + ")", ")", ")", "}"}, " ")
}
