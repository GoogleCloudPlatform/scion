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
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type edit struct {
	start, end int
	text       string
}

type fileEdits struct {
	edits []edit
	// imports are added by flushImports, in one block per file.
	imports []newImport
	// cleanImports drops imports that the edited file no longer uses;
	// importNames maps each import path of the file to its package name.
	cleanImports bool
	importNames  map[string]string
	// drops remove whole declarations; edits inside them are discarded.
	drops []edit
}

type newImport struct{ name, path, realName string }

func (a *analysis) editsFor(f *srcFile) *fileEdits {
	fe := a.edits[f]
	if fe == nil {
		fe = &fileEdits{}
		a.edits[f] = fe
	}
	return fe
}

func (a *analysis) offset(pos token.Pos) int { return a.fset.Position(pos).Offset }

func (a *analysis) replaceIdent(f *srcFile, id *ast.Ident, text string) {
	fe := a.editsFor(f)
	fe.edits = append(fe.edits, edit{a.offset(id.Pos()), a.offset(id.End()), text})
}

// srcImportName returns the local name of the source import in an external
// test file, or "".
func (a *analysis) srcImportName(f *srcFile) string {
	for _, spec := range f.AST.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p == a.mod.ImportPath {
			if spec.Name != nil {
				return spec.Name.Name
			}
			return a.srcName
		}
	}
	return ""
}

// checkXTests inspects the external test files (package <name>_test) of the
// source directory syntactically.
func (a *analysis) checkXTests() {
	for _, f := range a.files {
		if !f.XTest {
			continue
		}
		local := a.srcImportName(f)
		if local == "" || local == "_" || local == "." {
			if local == "." {
				a.plan.errorf("%s dot-imports %s; not supported", a.rel(f.Path), a.mod.ImportPath)
			}
			continue
		}
		keepsSource := false
		calls := callSelectors(f.AST)
		embeddedSels := embeddedSelectors(f.AST)
		ast.Inspect(f.AST, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != local {
				return true
			}
			obj := a.pkg.Scope().Lookup(sel.Sel.Name)
			h := a.home(obj)
			if h == nil {
				keepsSource = true
				return true
			}
			if al := a.aliases[obj]; al != nil && f.Moved && !h.Moved {
				// A moved external test that names a staying alias refers to
				// its target directly, unless that would change a func value's
				// identity (a wrapper) or an embedded field's name.
				if (al.kind == "func" && !calls[sel]) || (al.name != obj.Name() && embeddedSels[sel]) {
					keepsSource = true
				} else {
					a.xtestAliasSels[f] = append(a.xtestAliasSels[f], sel)
				}
				return true
			}
			pos := a.posOf(sel.Pos())
			switch {
			case f.Moved && h.Moved:
				a.xtestSels[f] = append(a.xtestSels[f], sel)
			case f.Moved && h.IsTest:
				a.plan.errorf("%s: moved external test uses %s.%s from the staying test file %s; move %s too", pos, local, sel.Sel.Name, h.Name, h.Name)
			case f.Moved:
				keepsSource = true
			case h.Moved && h.IsTest:
				a.plan.errorf("%s: staying external test uses %s.%s, declared in the moved test file %s; test-only symbols cannot be aliased", pos, local, sel.Sel.Name, h.Name)
			case h.Moved && kindOf(obj) == "var":
				a.plan.errorf("%s: staying external test uses moved var %s.%s; vars cannot be aliased - rewrite the reference to the target package first", pos, local, sel.Sel.Name)
			case h.Moved && isGenericFunc(obj) && !calls[sel]:
				a.plan.errorf("%s: staying external test uses the moved generic func %s.%s as a value; generic funcs are aliased by wrappers, which have a different identity - rewrite the reference to the target package first", pos, local, sel.Sel.Name)
			}
			return true
		})
		if f.Moved && keepsSource {
			a.plan.add(levelInfo, "moved external test still imports the source package", a.rel(f.Path),
				"the target's external test will import %s (no cycle, but the test binary still links it)", a.mod.ImportPath)
		}
	}
}

// scanModule reports references from other packages of the module to moved
// exported vars, which cannot be aliased.
func (a *analysis) scanModule() {
	vars := map[string]bool{}     // moved exported vars: any use is an error
	generics := map[string]bool{} // moved exported generic funcs: value uses are errors
	for _, name := range a.pkg.Scope().Names() {
		obj := a.pkg.Scope().Lookup(name)
		if h := a.home(obj); h != nil && h.Moved && obj.Exported() {
			switch {
			case kindOf(obj) == "var":
				vars[name] = true
			case isGenericFunc(obj):
				generics[name] = true
			}
		}
	}
	if len(vars) == 0 && len(generics) == 0 {
		return
	}
	pkgs, err := a.modulePackages()
	if err != nil {
		a.plan.errorf("scanning the module for uses of moved exported vars: %v", err)
		return
	}
	fset := token.NewFileSet()
	for _, p := range pkgs {
		if p.ImportPath == a.mod.ImportPath {
			continue // the source package and its external tests are analysed directly
		}
		// Files excluded by build constraints (IgnoredGoFiles) do not show up
		// in Imports, so they are always scanned; each file's own imports
		// decide below.
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
			calls := callSelectors(f)
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					x, ok := sel.X.(*ast.Ident)
					if !ok || x.Name != local {
						return true
					}
					pos := fset.Position(sel.Pos())
					switch {
					case vars[sel.Sel.Name]:
						a.plan.errorf("%s:%d: %s.%s refers to a moved exported var; vars cannot be aliased - rewrite the reference to the target package first",
							a.rel(pos.Filename), pos.Line, local, sel.Sel.Name)
					case generics[sel.Sel.Name] && !calls[sel]:
						a.plan.errorf("%s:%d: %s.%s uses a moved generic func as a value; its wrapper alias has a different identity - rewrite the reference to the target package first",
							a.rel(pos.Filename), pos.Line, local, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// safetyFindings adds the init-order and directive findings.
func (a *analysis) safetyFindings() {
	movedSideEffects := false
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		if f.Constraint != "" {
			a.plan.add(levelInfo, "moved file has a build constraint", a.rel(f.Path), "constraint %q (its alias entries carry the same constraint)", f.Constraint)
		}
		if f.XTest {
			a.plan.add(levelInfo, "moved external test package", a.rel(f.Path), "package %s becomes %s_test", f.PkgName, a.cfg.PkgName)
		}
		if f.AST.Doc != nil && !f.IsTest {
			a.plan.add(levelWarn, "moved file carries the package doc comment", a.posOf(f.AST.Doc.Pos()),
				"the doc comment moves to package %s; %s may need a new one", a.cfg.PkgName, a.srcName)
		}
		for _, cg := range f.AST.Comments {
			for _, c := range cg.List {
				switch {
				case strings.HasPrefix(c.Text, "//go:linkname"):
					a.plan.add(levelHigh, "go:linkname directive", a.posOf(c.Pos()), "%s (symbol paths include the package path)", c.Text)
				case strings.HasPrefix(c.Text, "//go:embed"):
					a.plan.add(levelHigh, "go:embed directive", a.posOf(c.Pos()), "%s (patterns resolve relative to the package directory)", c.Text)
					a.checkEmbed(c)
				case strings.HasPrefix(c.Text, "//go:generate"):
					a.plan.add(levelWarn, "go:generate directive", a.posOf(c.Pos()), "%s (runs in the new directory)", c.Text)
				}
			}
		}
		for _, n := range []string{"%T", "reflect.TypeOf", "gob.Register", "runtime.FuncForPC", "runtime.Caller", "debug.Stack", "runtime.Stack"} {
			if strings.Contains(string(f.Src), n) {
				extra := ""
				switch n {
				case "runtime.Caller":
					extra = "; functions that inspect their caller are aliased as vars, not wrappers"
				case "debug.Stack", "runtime.Stack":
					extra = "; captured stacks (and panic traces checked by tests) show the new package path and any wrapper frames"
				}
				a.plan.add(levelWarn, "runtime type/function names change package qualifier", a.rel(f.Path),
					"uses %s: names of moved types and functions now print as %s.X instead of %s.X%s", n, a.cfg.PkgName, a.srcName, extra)
			}
		}
		if f.Included && !f.XTest && a.initFindings(f) {
			movedSideEffects = true
		}
	}
	if movedSideEffects {
		a.stayingSideEffects()
	}
	a.typeNameFindings()
	a.funcValueFindings()
	a.testdataFindings()
	a.sourceScanFindings()
	a.scanLinknames()
	a.reflectionFindings()
	// Staying var initialisers that use moved code. Calls run moved code
	// whose package-level state is now initialised before every initialiser
	// of the source package (a registry filled by staying initialisers looks
	// empty to it, and vice versa): HIGH. Reads of moved vars: WARN.
	for _, f := range a.checked {
		if f.Moved {
			continue
		}
		for _, d := range f.AST.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				var calls, vars, dynamic []string
				compileTime := false
				for _, v := range vs.Values {
					runsAtInit(v, func(n ast.Node) {
						switch n := n.(type) {
						case *ast.CallExpr:
							fn, dyn := a.calleeOf(n)
							switch {
							case dyn:
								dynamic = append(dynamic, exprString(n.Fun))
							case fn == nil || fn.Pkg() != a.pkg:
							case a.home(fn) != nil && a.home(fn).Moved:
								calls = append(calls, funcLabel(fn))
							default:
								r := a.reach(fn)
								if r.moved != "" {
									calls = append(calls, funcLabel(fn)+" (which reaches moved "+r.moved+")")
								} else if r.dynamic {
									dynamic = append(dynamic, funcLabel(fn)+" (which makes dynamic calls)")
								}
							}
						case *ast.Ident:
							obj := a.info.Uses[n]
							if obj == nil {
								return
							}
							obj = origin(obj)
							h := a.home(obj)
							if h == nil || !h.Moved {
								return
							}
							switch o := obj.(type) {
							case *types.Var:
								if !o.IsField() && a.isPkgLevel(o) {
									vars = append(vars, o.Name())
								}
							case *types.Const, *types.TypeName:
								compileTime = true
							}
						}
					})
				}
				if len(dynamic) > 0 && a.movedHasState() && len(calls) == 0 {
					a.plan.add(levelWarn, "staying var initialiser makes dynamic calls (func values or interfaces)", a.posOf(vs.Pos()),
						"%s = ... calls %s; the moved files have package-level state, and a dynamic call may reach moved code, which is now initialised before every initialiser of %s",
						identNames(vs.Names), strings.Join(dedupStrings(sortedCopy(dynamic)), ", "), a.srcName)
				}
				names := identNames(vs.Names)
				switch {
				case len(calls) > 0:
					a.plan.add(levelHigh, "staying var initialiser calls moved code", a.posOf(vs.Pos()),
						"%s = ... calls %s: the moved package is initialised first, so its state no longer sees (or is no longer seen by) the initialisers of %s in their old order", names, strings.Join(dedupStrings(sortedCopy(calls)), ", "), a.srcName)
				case len(vars) > 0:
					a.plan.add(levelWarn, "staying var initialiser reads moved vars", a.posOf(vs.Pos()),
						"%s = ... reads %s, now initialised in package %s before all of %s (values set by %s initialisers or init() are no longer visible)", names, strings.Join(dedupStrings(sortedCopy(vars)), ", "), a.cfg.PkgName, a.srcName, a.srcName)
				case compileTime:
					a.plan.add(levelInfo, "staying var initialiser uses moved consts, types or func values only", a.posOf(vs.Pos()),
						"%s = ... (no moved code runs and no moved var is read at init)", names)
				}
			}
		}
	}
}

// checkEmbed requires every file matched by a go:embed pattern of a moved
// file to be in the move set.
func (a *analysis) checkEmbed(c *ast.Comment) {
	moved := map[string]bool{}
	for _, name := range a.assets {
		moved[name] = true
	}
	for _, pat := range strings.Fields(strings.TrimPrefix(c.Text, "//go:embed")) {
		if uq, err := strconv.Unquote(pat); err == nil {
			pat = uq
		}
		pat = strings.TrimPrefix(pat, "all:")
		matches, _ := filepath.Glob(filepath.Join(a.cfg.SrcDir, filepath.FromSlash(pat)))
		if len(matches) == 0 {
			a.plan.errorf("%s: go:embed pattern %q matches nothing in %s", a.posOf(c.Pos()), pat, a.rel(a.cfg.SrcDir))
		}
		for _, m := range matches {
			r, _ := filepath.Rel(a.cfg.SrcDir, m)
			top := strings.Split(filepath.ToSlash(r), "/")[0]
			if !moved[top] {
				a.plan.errorf("%s: go:embed pattern %q matches %s, which is not in the move set; add %s to the file list", a.posOf(c.Pos()), pat, a.rel(m), top)
			}
		}
	}
}

func identNames(ids []*ast.Ident) string {
	var s []string
	for _, id := range ids {
		s = append(s, id.Name)
	}
	return strings.Join(s, ", ")
}

// initFindings reports init() functions and package-level var initialisers of
// a moved file. It returns true when the file has code that runs at init and
// may depend on or affect process state (so staying initialisers with side
// effects are worth listing too).
func (a *analysis) initFindings(f *srcFile) bool {
	sideEffects := false
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == "init" {
				sideEffects = true
				a.plan.add(levelHigh, "init() in moved file", a.posOf(d.Pos()),
					"init() now runs when package %s initialises, before every var initialiser and init() of %s", a.cfg.PkgName, a.srcName)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs := spec.(*ast.ValueSpec)
				var deps, otherVars []string
				for _, v := range vs.Values {
					ast.Inspect(v, func(n ast.Node) bool {
						if _, ok := n.(*ast.FuncLit); ok {
							return false
						}
						id, ok := n.(*ast.Ident)
						if !ok {
							return true
						}
						obj := a.info.Uses[id]
						if v, ok := obj.(*types.Var); ok && v.Pkg() != nil && v.Pkg() != a.pkg && v.Parent() == v.Pkg().Scope() {
							otherVars = append(otherVars, v.Pkg().Name()+"."+v.Name())
							return true
						}
						if obj == nil || !a.isPkgLevel(obj) {
							return true
						}
						h := a.home(origin(obj))
						if h == nil || h == f {
							return true
						}
						switch obj.(type) {
						case *types.Var, *types.Func:
							deps = append(deps, fmt.Sprintf("%s (%s)", obj.Name(), h.Name))
						}
						return true
					})
				}
				c := a.varInitCalls(vs)
				names := identNames(vs.Names)
				if len(c.pkg) > 0 {
					sideEffects = true
					a.plan.add(levelHigh, "package-level var initialiser calls package code", a.posOf(vs.Pos()),
						"%s = ... calls %s", names, strings.Join(dedupStrings(sortedCopy(c.pkg)), ", "))
				}
				if len(c.other) > 0 {
					sideEffects = true
					a.plan.add(levelWarn, "package-level var initialiser calls other packages", a.posOf(vs.Pos()),
						"%s = ... calls %s: it now runs before every var initialiser and init() of %s, so state they set (environment, defaults, registries) is no longer visible to it, and its own effects happen earlier",
						names, strings.Join(dedupStrings(sortedCopy(c.other)), ", "), a.srcName)
				}
				if len(otherVars) > 0 {
					sideEffects = true
					a.plan.add(levelWarn, "package-level var initialiser reads other packages' vars", a.posOf(vs.Pos()),
						"%s = ... reads %s: it now runs before every initialiser of %s, so changes they make to those vars are no longer visible to it",
						names, strings.Join(dedupStrings(sortedCopy(otherVars)), ", "), a.srcName)
				}
				if len(deps) > 0 {
					a.plan.add(levelWarn, "package-level var initialiser depends on other files", a.posOf(vs.Pos()),
						"%s = ... reads %s", names, strings.Join(dedupStrings(sortedCopy(deps)), ", "))
				}
				if len(c.pure) > 0 && len(c.pkg) == 0 && len(c.other) == 0 && len(otherVars) == 0 {
					a.plan.add(levelInfo, "package-level var initialiser calls pure constructors only", a.posOf(vs.Pos()),
						"%s = ... calls %s (allow-listed, with arguments that cannot run package code)", names, strings.Join(dedupStrings(sortedCopy(c.pure)), ", "))
				}
			}
		}
	}
	return sideEffects
}

// stayingSideEffects lists staying var initialisers whose calls may read or
// write state. They used to be ordered with the moved initialisers by
// dependency and declaration order; now every moved initialiser runs first.
func (a *analysis) stayingSideEffects() {
	for _, f := range a.checked {
		if f.Moved {
			continue
		}
		for _, d := range f.AST.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if c := a.varInitCalls(vs); len(c.other)+len(c.pkg) > 0 {
					all := append(append([]string(nil), c.other...), c.pkg...)
					a.plan.add(levelWarn, "staying var initialiser with side-effecting calls (moved initialisers now run before it)", a.posOf(vs.Pos()),
						"%s = ... calls %s", identNames(vs.Names), strings.Join(dedupStrings(sortedCopy(all)), ", "))
				}
			}
		}
	}
}

// A call in a package-level var initialiser is treated as pure (order
// independent) only when the callee is allow-listed below AND isPureCall
// accepts its arguments. Everything else is reported at WARN or higher.
var pureInitPackages = map[string]bool{
	"errors": true, "regexp": true, "strings": true, "strconv": true,
	"unicode": true, "unicode/utf8": true, "math": true, "bytes": true,
}

var pureInitFuncs = map[string]bool{
	"fmt.Errorf": true, "fmt.Sprintf": true, "fmt.Sprint": true, "fmt.Sprintln": true,
	"reflect.TypeOf": true, "reflect.TypeFor": true,
	"time.Date": true, "time.Unix": true, "time.UnixMilli": true, "time.UnixMicro": true,
	"path.Join": true, "path/filepath.Join": true, "slices.Clone": true, "maps.Clone": true,
}

// isPureCall applies the argument rules: no func-typed argument (it would run
// arbitrary code), no non-empty interface argument (dynamic methods), no
// argument whose method set includes methods from non-standard packages, and
// for fmt only basic-typed arguments (fmt calls String/Error/Format).
func (a *analysis) isPureCall(call *ast.CallExpr, fn *types.Func) bool {
	for _, arg := range call.Args {
		t := a.info.TypeOf(arg)
		if t == nil {
			return false
		}
		if fn.Pkg().Path() == "fmt" {
			if _, ok := t.Underlying().(*types.Basic); !ok {
				return false
			}
			if _, named := types.Unalias(t).(*types.Named); named {
				return false // a named basic type may have String/Error/Format
			}
			continue
		}
		switch u := t.Underlying().(type) {
		case *types.Signature:
			return false
		case *types.Interface:
			if !u.Empty() {
				return false
			}
		}
		for _, tt := range []types.Type{t, types.NewPointer(t)} {
			ms := types.NewMethodSet(tt)
			for i := 0; i < ms.Len(); i++ {
				if p := ms.At(i).Obj().Pkg(); p != nil && !isStdPath(p.Path()) {
					return false
				}
			}
		}
	}
	return true
}

// initCalls classifies the calls of one package-level var initialiser.
type initCalls struct {
	pkg   []string // calls into the source package (or func values, func literals)
	other []string // calls into other packages that may read or write state
	pure  []string // allow-listed pure constructors
}

func (a *analysis) classifyCall(call *ast.CallExpr, c *initCalls) {
	fun := ast.Unparen(call.Fun)
	if tv, ok := a.info.Types[fun]; ok && tv.IsType() {
		return // conversion
	}
	var id *ast.Ident
	switch fn := fun.(type) {
	case *ast.Ident:
		id = fn
	case *ast.SelectorExpr:
		id = fn.Sel
	case *ast.IndexExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			id = x
		}
	case *ast.FuncLit:
		c.pkg = append(c.pkg, "an immediately-invoked func literal")
		return
	}
	if id == nil {
		c.pkg = append(c.pkg, "a computed function value")
		return
	}
	switch obj := a.info.Uses[id].(type) {
	case *types.Builtin:
		return
	case *types.Func:
		switch {
		case obj.Pkg() == a.pkg:
			c.pkg = append(c.pkg, obj.Name())
		case obj.Pkg() == nil:
			c.other = append(c.other, obj.Name())
		default:
			name := obj.Pkg().Name() + "." + obj.Name()
			if recv := obj.Signature().Recv(); recv != nil {
				name = obj.Pkg().Name() + "." + types.TypeString(recv.Type(), func(p *types.Package) string { return "" }) + "." + obj.Name()
			}
			allowed := obj.Signature().Recv() == nil && (pureInitPackages[obj.Pkg().Path()] || pureInitFuncs[obj.Pkg().Path()+"."+obj.Name()])
			if allowed && a.isPureCall(call, obj) {
				c.pure = append(c.pure, name)
			} else {
				c.other = append(c.other, name)
			}
		}
	case *types.Var:
		if obj.Pkg() == a.pkg {
			c.pkg = append(c.pkg, obj.Name()+" (func value)")
		} else {
			c.other = append(c.other, obj.Name()+" (func value)")
		}
	default:
		c.pkg = append(c.pkg, id.Name)
	}
}

// varInitCalls collects the calls made by a var spec's initialisers (not
// descending into func literal bodies, which do not run at init).
func (a *analysis) varInitCalls(vs *ast.ValueSpec) *initCalls {
	c := &initCalls{}
	for _, v := range vs.Values {
		ast.Inspect(v, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.CallExpr:
				a.classifyCall(n, c)
			}
			return true
		})
	}
	return c
}

// chooseImportName picks a name for the target import in file f that does
// not collide with package-level names, the file's imports, or (at the given
// positions) any local declaration.
func (a *analysis) chooseImportName(f *srcFile, at []token.Pos, extraReserved map[string]bool) string {
	return a.chooseImportNameBase(f, a.cfg.PkgName, at, extraReserved)
}

// chooseImportNameBase is chooseImportName for an arbitrary base name.
func (a *analysis) chooseImportNameBase(f *srcFile, base string, at []token.Pos, extraReserved map[string]bool) string {
	reserved := map[string]bool{}
	for k := range extraReserved {
		reserved[k] = true
	}
	for _, spec := range f.AST.Imports {
		reserved[importName(spec)] = true
	}
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name += strconv.Itoa(i)
		}
		if reserved[name] {
			continue
		}
		if a.pkg != nil && !f.XTest {
			if a.pkg.Scope().Lookup(name) != nil {
				continue
			}
			clash := false
			for _, pos := range at {
				if s := a.pkg.Scope().Innermost(pos); s != nil {
					if _, obj := s.LookupParent(name, pos); obj != nil {
						clash = true
						break
					}
				}
			}
			if clash {
				continue
			}
		}
		return name
	}
}

// buildEdits computes every text edit of the move.
func (a *analysis) buildEdits() error {
	// Package clauses.
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		name := a.cfg.PkgName
		if f.XTest {
			name += "_test"
		}
		a.replaceIdent(f, f.AST.Name, name)
	}
	// Renames and var rewrites.
	varUses := map[*srcFile][]identUse{}
	for _, u := range a.uses {
		if u.file == nil {
			continue
		}
		if n, ok := a.memberRename[u.obj]; ok {
			a.replaceIdent(u.file, u.id, n)
			continue
		}
		if n, ok := a.embedFollow[u.obj]; ok && !u.def {
			if v, isVar := u.obj.(*types.Var); isVar && a.info.Uses[u.id] == types.Object(v) {
				a.replaceIdent(u.file, u.id, n)
			}
			continue
		}
		h := a.home(u.obj)
		if h == nil || !h.Moved || !a.isPkgLevel(u.obj) {
			continue
		}
		if u.file.Moved {
			if n := a.pkgRename[u.obj]; n != "" {
				a.replaceIdent(u.file, u.id, n)
			}
			continue
		}
		if u.def {
			continue
		}
		// Vars, and funcs used as values (not called): a var alias would be a
		// copy and a wrapper has a different identity (reflect Pointer,
		// FuncForPC), so these references are rewritten to the target.
		if kindOf(u.obj) == "var" || (kindOf(u.obj) == "func" && !a.callFuns[u.id]) {
			varUses[u.file] = append(varUses[u.file], u)
		}
	}
	var varFiles []*srcFile
	for f := range varUses {
		varFiles = append(varFiles, f)
	}
	sort.Slice(varFiles, func(i, j int) bool { return varFiles[i].Name < varFiles[j].Name })
	for _, f := range varFiles {
		var at []token.Pos
		for _, u := range varUses[f] {
			at = append(at, u.id.Pos())
		}
		q := a.chooseImportName(f, at, nil)
		for _, u := range varUses[f] {
			text := q + "." + a.newName(u.obj)
			a.replaceIdent(f, u.id, text)
			a.plan.VarRewrites = append(a.plan.VarRewrites, varRewrite{Pos: a.posOf(u.id.Pos()), Old: u.id.Name, New: text})
		}
		a.addImport(f, q, a.dstImport, a.cfg.PkgName)
	}
	// References from moved files to staying aliases: the alias targets.
	aliasByFile := map[*srcFile][]identUse{}
	for _, u := range a.aliasUses {
		aliasByFile[u.file] = append(aliasByFile[u.file], u)
	}
	for _, f := range sortedFileKeys(aliasByFile) {
		a.rewriteAliasUses(f, aliasByFile[f])
	}
	// Moved in-package tests joining an existing target: target.X becomes X.
	for _, f := range sortedFileKeys(a.dstSels) {
		for _, sel := range a.dstSels[f] {
			if a.inDropped(f, sel.Pos()) {
				continue
			}
			fe := a.editsFor(f)
			fe.edits = append(fe.edits, edit{a.offset(sel.Pos()), a.offset(sel.End()), sel.Sel.Name})
			a.plan.AliasRewrites = append(a.plan.AliasRewrites, varRewrite{Pos: a.posOf(sel.Pos()), Old: exprString(sel), New: sel.Sel.Name})
		}
		a.removeImport(f, a.dstImport)
	}
	// Equivalent duplicates of target declarations.
	for _, d := range a.drops {
		a.removeDecl(d)
	}
	// Moved external tests: re-qualify references to moved symbols, and
	// refer to alias targets directly.
	xset := map[*srcFile]bool{}
	for f := range a.xtestSels {
		xset[f] = true
	}
	for f := range a.xtestAliasSels {
		xset[f] = true
	}
	for _, f := range sortedFileKeys(xset) {
		sels := a.xtestSels[f]
		reserved := map[string]bool{}
		for _, g := range a.files {
			if g.XTest {
				for _, d := range g.AST.Decls {
					for _, n := range declNames(d) {
						reserved[n] = true
					}
				}
			}
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				reserved[id.Name] = true
			}
			return true
		})
		local := a.srcImportName(f)
		total := 0
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == local {
				total++
			}
			return true
		})
		names := map[string]string{} // import path -> name in this file
		qualifier := func(path, base string) string {
			if q, ok := names[path]; ok {
				return q
			}
			q := ""
			for _, spec := range f.AST.Imports {
				if importPath(spec) == path && spec.Name == nil && guessPkgName(path) == base {
					q = base
				} else if importPath(spec) == path && spec.Name != nil && spec.Name.Name != "_" && spec.Name.Name != "." {
					q = spec.Name.Name
				}
			}
			if q == "" {
				q = a.chooseImportNameBase(f, base, nil, reserved)
				a.addImport(f, q, path, base)
			}
			reserved[q] = true
			names[path] = q
			return q
		}
		if len(sels) > 0 {
			q := qualifier(a.dstImport, a.cfg.PkgName)
			for _, sel := range sels {
				a.replaceIdent(f, sel.X.(*ast.Ident), q)
			}
		}
		for _, sel := range a.xtestAliasSels[f] {
			al := a.aliases[a.pkg.Scope().Lookup(sel.Sel.Name)]
			text := qualifier(al.path, al.pkgName) + "." + al.name
			fe := a.editsFor(f)
			fe.edits = append(fe.edits, edit{a.offset(sel.Pos()), a.offset(sel.End()), text})
			a.plan.AliasRewrites = append(a.plan.AliasRewrites, varRewrite{Pos: a.posOf(sel.Pos()), Old: exprString(sel), New: text})
		}
		if total == len(sels)+len(a.xtestAliasSels[f]) {
			a.removeImport(f, a.mod.ImportPath)
		}
	}
	a.flushImports()
	for f := range a.edits {
		if !f.Moved {
			a.plan.TouchedFiles = append(a.plan.TouchedFiles, a.rel(f.Path))
		}
	}
	return nil
}

func declNames(d ast.Decl) []string {
	var out []string
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			out = append(out, d.Name.Name)
		}
	case *ast.GenDecl:
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.ValueSpec:
				for _, n := range s.Names {
					out = append(out, n.Name)
				}
			case *ast.TypeSpec:
				out = append(out, s.Name.Name)
			}
		}
	}
	return out
}

func importSpecText(name, path, realName string) string {
	if name == realName && realName == path[strings.LastIndex(path, "/")+1:] {
		return strconv.Quote(path)
	}
	return name + " " + strconv.Quote(path)
}

func (a *analysis) addImport(f *srcFile, name, path, realName string) {
	fe := a.editsFor(f)
	for _, imp := range fe.imports {
		if imp.path == path && imp.name == name {
			return
		}
	}
	fe.imports = append(fe.imports, newImport{name, path, realName})
}

// flushImports turns the queued imports of every file into one edit each.
func (a *analysis) flushImports() {
	for f, fe := range a.edits {
		if len(fe.imports) == 0 {
			continue
		}
		imps := append([]newImport(nil), fe.imports...)
		sort.Slice(imps, func(i, j int) bool {
			if isStdPath(imps[i].path) != isStdPath(imps[j].path) {
				return isStdPath(imps[i].path)
			}
			return imps[i].path < imps[j].path
		})
		fe.imports = nil
		a.insertImports(f, fe, imps)
	}
}

func (a *analysis) insertImports(f *srcFile, fe *fileEdits, imps []newImport) {
	specs := func(prevStd bool, indent string) string {
		var b strings.Builder
		for i, imp := range imps {
			std := isStdPath(imp.path)
			if (i == 0 && prevStd && !std) || (i > 0 && isStdPath(imps[i-1].path) && !std) {
				b.WriteString("\n")
			}
			b.WriteString(indent + importSpecText(imp.name, imp.path, imp.realName) + "\n")
		}
		return b.String()
	}
	var last *ast.GenDecl
	for _, d := range f.AST.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			last = gd
		}
	}
	switch {
	case last == nil && len(imps) == 1:
		off := a.offset(f.AST.Name.End())
		fe.edits = append(fe.edits, edit{off, off, "\n\nimport " + importSpecText(imps[0].name, imps[0].path, imps[0].realName) + "\n"})
	case last == nil:
		off := a.offset(f.AST.Name.End())
		fe.edits = append(fe.edits, edit{off, off, "\n\nimport (\n" + specs(true, "\t") + ")\n"})
	case last.Rparen.IsValid():
		off := a.offset(last.Rparen)
		i := off - 1
		for i >= 0 && (f.Src[i] == ' ' || f.Src[i] == '\t') {
			i--
		}
		prevStd := true
		if len(last.Specs) > 0 {
			p, _ := strconv.Unquote(last.Specs[len(last.Specs)-1].(*ast.ImportSpec).Path.Value)
			prevStd = isStdPath(p)
		}
		text := specs(prevStd, "\t")
		if i < 0 || f.Src[i] != '\n' {
			text = "\n" + text
		}
		fe.edits = append(fe.edits, edit{off, off, text})
	default:
		// A single unparenthesised import: turn it into a block.
		old := last.Specs[0].(*ast.ImportSpec)
		start, end := a.offset(old.Pos()), a.offset(old.End())
		if old.Comment != nil {
			end = a.offset(old.Comment.End())
		}
		p, _ := strconv.Unquote(old.Path.Value)
		fe.edits = append(fe.edits, edit{start, end, "(\n\t" + string(f.Src[start:end]) + "\n" + specs(isStdPath(p), "\t") + ")"})
	}
}

// sortedFileKeys returns the keys of a map keyed by file, sorted by name.
func sortedFileKeys[V any](m map[*srcFile]V) []*srcFile {
	var out []*srcFile
	for f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// embeddedSelectors returns the selector expressions that are the type of an
// embedded struct field (their name is the field name).
func embeddedSelectors(f *ast.File) map[*ast.SelectorExpr]bool {
	out := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			if len(fld.Names) > 0 {
				continue
			}
			t := fld.Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			switch ix := t.(type) {
			case *ast.IndexExpr:
				t = ix.X
			case *ast.IndexListExpr:
				t = ix.X
			}
			if sel, ok := t.(*ast.SelectorExpr); ok {
				out[sel] = true
			}
		}
		return true
	})
	return out
}

// rewriteAliasUses replaces the uses of aliases in f with direct references
// to their targets, adding imports as needed.
func (a *analysis) rewriteAliasUses(f *srcFile, uses []identUse) {
	at := map[string][]token.Pos{}
	base := map[string]string{}
	var paths []string
	var kept []identUse
	for _, u := range uses {
		if !a.inDropped(f, u.id.Pos()) {
			kept = append(kept, u)
		}
	}
	uses = kept
	for _, u := range uses {
		al := a.aliases[u.obj]
		if a.bareTarget(f, al) {
			continue
		}
		if _, ok := at[al.path]; !ok {
			paths = append(paths, al.path)
			base[al.path] = al.pkgName
		}
		at[al.path] = append(at[al.path], u.id.Pos())
	}
	sort.Strings(paths)
	names := map[string]string{}
	taken := map[string]bool{}
	for _, p := range paths {
		name, add := a.importNameFor(f, p, base[p], at[p], taken)
		taken[name] = true
		names[p] = name
		if add {
			a.addImport(f, name, p, base[p])
		}
	}
	for _, u := range uses {
		al := a.aliases[u.obj]
		q := ""
		if !a.bareTarget(f, al) {
			q = names[al.path]
		}
		text := aliasText(al, q)
		if text != u.id.Name {
			a.replaceIdent(f, u.id, text)
		}
		a.plan.AliasRewrites = append(a.plan.AliasRewrites, varRewrite{Pos: a.posOf(u.id.Pos()), Old: u.id.Name, New: text})
	}
}

// inDropped reports whether pos lies in a declaration of f that is dropped.
func (a *analysis) inDropped(f *srcFile, pos token.Pos) bool {
	for _, d := range a.drops {
		if d.file != f {
			continue
		}
		var n ast.Node = d.node
		if d.gen != nil && len(d.gen.Specs) == 1 {
			n = d.gen
		}
		if n.Pos() <= pos && pos < n.End() {
			return true
		}
	}
	return false
}

// bareTarget reports whether a reference to al in f becomes a bare
// identifier: f is an in-package test moving into al's package.
func (a *analysis) bareTarget(f *srcFile, al *aliasInfo) bool {
	return a.intoExisting && f.Moved && !f.XTest && al.path == a.dstImport
}

// removeDecl deletes a package-level declaration (a func, or a spec of a
// GenDecl, or the whole GenDecl when it is its only spec) and marks the file
// for unused-import cleanup.
func (a *analysis) removeDecl(d declRef) {
	f := d.file
	fe := a.editsFor(f)
	var start, end int
	switch n := d.node.(type) {
	case *ast.FuncDecl:
		start, end = a.offset(n.Pos()), a.offset(n.End())
		if n.Doc != nil {
			start = a.offset(n.Doc.Pos())
		}
	default:
		if d.gen != nil && len(d.gen.Specs) == 1 {
			start, end = a.offset(d.gen.Pos()), a.offset(d.gen.End())
			if d.gen.Doc != nil {
				start = a.offset(d.gen.Doc.Pos())
			}
		} else {
			start, end = a.offset(n.Pos()), a.offset(n.End())
			var doc, comment *ast.CommentGroup
			switch s := n.(type) {
			case *ast.ValueSpec:
				doc, comment = s.Doc, s.Comment
			case *ast.TypeSpec:
				doc, comment = s.Doc, s.Comment
			}
			if doc != nil {
				start = a.offset(doc.Pos())
			}
			if comment != nil {
				end = a.offset(comment.End())
			}
		}
	}
	for start > 0 && (f.Src[start-1] == ' ' || f.Src[start-1] == '\t') {
		start--
	}
	if end < len(f.Src) && f.Src[end] == '\n' {
		end++
	}
	fe.drops = append(fe.drops, edit{start, end, ""})
	a.markCleanImports(f)
}

// markCleanImports records the package names of f's imports and asks
// applyEdits to drop the ones the edited file no longer uses.
func (a *analysis) markCleanImports(f *srcFile) {
	fe := a.editsFor(f)
	fe.cleanImports = true
	if fe.importNames == nil {
		fe.importNames = map[string]string{}
	}
	for _, spec := range f.AST.Imports {
		name := importName(spec)
		if a.info != nil && f.Included && !f.XTest {
			name = a.fileImportName(spec)
		}
		fe.importNames[importPath(spec)] = name
	}
}

// isStdPath reports whether an import path looks like a standard-library path.
func isStdPath(path string) bool {
	first := path
	if i := strings.Index(path, "/"); i >= 0 {
		first = path[:i]
	}
	return !strings.Contains(first, ".")
}

func (a *analysis) removeImport(f *srcFile, path string) {
	fe := a.editsFor(f)
	for _, d := range f.AST.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, s := range gd.Specs {
			spec := s.(*ast.ImportSpec)
			if p, _ := strconv.Unquote(spec.Path.Value); p != path {
				continue
			}
			var start, end int
			if len(gd.Specs) == 1 {
				start, end = a.offset(gd.Pos()), a.offset(gd.End())
			} else {
				start, end = a.offset(spec.Pos()), a.offset(spec.End())
				if spec.Doc != nil {
					start = a.offset(spec.Doc.Pos())
				}
				if spec.Comment != nil {
					end = a.offset(spec.Comment.End())
				}
			}
			for start > 0 && (f.Src[start-1] == ' ' || f.Src[start-1] == '\t') {
				start--
			}
			if end < len(f.Src) && f.Src[end] == '\n' {
				end++
			}
			fe.edits = append(fe.edits, edit{start, end, ""})
			return
		}
	}
}

// applyEdits returns the gofmt-formatted content of f after its edits.
func applyEdits(f *srcFile, fe *fileEdits) ([]byte, error) {
	// Declaration removals discard the edits inside them.
	edits := append([]edit(nil), fe.drops...)
	for _, e := range fe.edits {
		inside := false
		for _, d := range fe.drops {
			if d.start <= e.start && e.end <= d.end {
				inside = true
				break
			}
		}
		if !inside {
			edits = append(edits, e)
		}
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end < edits[j].end
	})
	var out []byte
	pos := 0
	var prev *edit
	for i := range edits {
		e := &edits[i]
		if prev != nil && *prev == *e {
			continue // duplicate (e.g. an embedded field ident in Defs and Uses)
		}
		if e.start < pos {
			return nil, fmt.Errorf("%s: overlapping edits at offset %d", f.Path, e.start)
		}
		out = append(out, f.Src[pos:e.start]...)
		out = append(out, e.text...)
		pos = e.end
		prev = e
	}
	out = append(out, f.Src[pos:]...)
	if fe.cleanImports {
		var err error
		if out, err = dropUnusedImports(out, fe.importNames); err != nil {
			return nil, fmt.Errorf("%s: %v", f.Path, err)
		}
	}
	formatted, err := format.Source(out)
	if err != nil {
		return nil, fmt.Errorf("%s: rewritten file does not parse: %v", f.Path, err)
	}
	return formatted, nil
}

// dropUnusedImports removes the imports of src that no selector uses any
// more (blank and dot imports are kept). names maps import paths to package
// names (paths missing from it use guessPkgName).
func dropUnusedImports(src []byte, names map[string]string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("edited file does not parse: %v", err)
	}
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok {
				used[x.Name] = true
			}
		}
		return true
	})
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	var edits []edit
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		var drop []*ast.ImportSpec
		for _, s := range gd.Specs {
			spec := s.(*ast.ImportSpec)
			path := importPath(spec)
			name := names[path]
			if name == "" {
				name = guessPkgName(path)
			}
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name != "_" && name != "." && !used[name] {
				drop = append(drop, spec)
			}
		}
		if len(drop) == 0 {
			continue
		}
		if len(drop) == len(gd.Specs) {
			edits = append(edits, edit{off(gd.Pos()), off(gd.End()), ""})
			continue
		}
		for _, spec := range drop {
			start, end := off(spec.Pos()), off(spec.End())
			if spec.Doc != nil {
				start = off(spec.Doc.Pos())
			}
			if spec.Comment != nil {
				end = off(spec.Comment.End())
			}
			edits = append(edits, edit{start, end, ""})
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out []byte
	pos := 0
	for _, e := range edits {
		out = append(out, src[pos:e.start]...)
		pos = e.end
	}
	return append(out, src[pos:]...), nil
}

// reflectionFindings warns when a renamed method's old or new name appears
// where it may be looked up by name at run time: template strings
// ({{.Name}}), reflect MethodByName/FieldByName calls, and non-Go files of the
// source directory (templates). Exporting a method makes it visible to
// text/template, html/template, reflect and RPC-style dispatch.
func (a *analysis) reflectionFindings() {
	// name -> every "Owner.old -> New" rename it may refer to (sorted, unique).
	descs := map[string][]string{}
	for obj, n := range a.memberRename {
		desc := ownerName(obj)
		if desc == "" {
			desc = a.fieldOwner(obj)
		}
		desc += "." + obj.Name() + " -> " + n
		descs[obj.Name()] = append(descs[obj.Name()], desc)
		descs[n] = append(descs[n], desc)
	}
	if len(descs) == 0 {
		return
	}
	names := map[string]string{}
	for name, list := range descs {
		sort.Strings(list)
		names[name] = strings.Join(dedupStrings(list), ", ")
	}
	matchName := func(text string) []string {
		var hits []string
		for name := range names {
			for i := 0; ; {
				j := strings.Index(text[i:], name)
				if j < 0 {
					break
				}
				j += i
				before := j == 0 || text[j-1] == '.' || text[j-1] == '"'
				end := j + len(name)
				after := end == len(text) || !isIdentByte(text[end])
				if before && after {
					hits = append(hits, name)
					break
				}
				i = end
			}
		}
		sort.Strings(hits)
		return hits
	}
	for _, f := range a.files {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "MethodByName" && sel.Sel.Name != "FieldByName") || len(n.Args) != 1 {
					return true
				}
				if lit, ok := n.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if text, err := strconv.Unquote(lit.Value); err == nil && names[text] != "" {
						a.plan.add(levelWarn, "renamed method name appears in a template or reflection string", a.posOf(lit.Pos()),
							"%s(%q) (%s); reflect sees exported methods only", sel.Sel.Name, text, names[text])
					}
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(n.Value)
				if err != nil || !strings.Contains(text, "{{") {
					return true
				}
				for _, name := range matchName(text) {
					a.plan.add(levelWarn, "renamed method name appears in a template or reflection string", a.posOf(n.Pos()),
						"%q mentions %s (%s); templates call exported methods only", shorten(text), name, names[name])
				}
			}
			return true
		})
	}
	// Non-Go files of the source directory and its subdirectories (for
	// example templates/*.tmpl), in lexical order.
	_ = filepath.WalkDir(a.cfg.SrcDir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			if path != a.cfg.SrcDir && (strings.HasPrefix(e.Name(), ".") || e.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(e.Name(), ".go") || path == a.cfg.ReportPath {
			return nil
		}
		if info, err := e.Info(); err != nil || info.Size() > 4<<20 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(b, 0) >= 0 {
			return nil // unreadable or binary
		}
		for _, name := range matchName(string(b)) {
			a.plan.add(levelWarn, "renamed method name appears in a template or reflection string", a.rel(path),
				"mentions .%s (%s); check templates that call methods by name", name, names[name])
		}
		return nil
	})
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func shorten(s string) string {
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

// typeNameFindings lists every moved named type: its reflect-visible name
// changes everywhere (staying files and importers included), because an alias
// keeps the type identity but not its package. %T, reflect Type.String and
// PkgPath, gob registration names and messages that embed type names change
// from <src>.X to <target>.X. gob.Register calls in the source package that
// register a moved type are reported as HIGH (wire/persistence names).
func (a *analysis) typeNameFindings() {
	for _, name := range a.pkg.Scope().Names() {
		tn, ok := a.pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		h := a.home(tn)
		if h == nil || !h.Moved {
			continue
		}
		a.plan.add(levelWarn, "reflect-visible type name changes (all users, not only moved files)", a.posOf(tn.Pos()),
			"%s.%s becomes %s.%s for %%T, reflect Type.String/PkgPath, gob names and messages that print type names (the alias keeps identity, not the name)",
			a.srcName, name, a.cfg.PkgName, a.newName(tn))
	}
	for _, f := range a.checked {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
			if !ok {
				return true
			}
			fn, ok := a.info.Uses[sel.Sel].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "encoding/gob" || (fn.Name() != "Register" && fn.Name() != "RegisterName") {
				return true
			}
			arg := call.Args[len(call.Args)-1]
			for _, tn := range a.movedTypesIn(a.info.TypeOf(arg)) {
				if fn.Name() == "RegisterName" {
					a.plan.add(levelWarn, "gob registration of a moved type", a.posOf(call.Pos()),
						"gob.RegisterName registers %s under a fixed wire name (unchanged), but type identity checks and %%T output now use package %s", tn.Name(), a.cfg.PkgName)
				} else {
					a.plan.add(levelHigh, "gob registration of a moved type", a.posOf(call.Pos()),
						"gob.Register registers %s, whose gob name embeds the package path; encoded data and peers on the old name break", tn.Name())
				}
			}
			return true
		})
	}
}

// movedTypesIn returns the moved named types reachable from t through
// pointers, slices, arrays, maps and channels (the types gob derives names
// from), in a deterministic order.
func (a *analysis) movedTypesIn(t types.Type) []*types.TypeName {
	var out []*types.TypeName
	seen := map[types.Type]bool{}
	var walk func(t types.Type)
	walk = func(t types.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		switch u := types.Unalias(t).(type) {
		case *types.Pointer:
			walk(u.Elem())
		case *types.Slice:
			walk(u.Elem())
		case *types.Array:
			walk(u.Elem())
		case *types.Map:
			walk(u.Key())
			walk(u.Elem())
		case *types.Chan:
			walk(u.Elem())
		case *types.Named:
			if h := a.home(u.Origin().Obj()); h != nil && h.Moved {
				out = append(out, u.Origin().Obj())
			}
			for i := 0; i < u.TypeArgs().Len(); i++ {
				walk(u.TypeArgs().At(i))
			}
		}
	}
	walk(t)
	return out
}

// runsAtInit visits the nodes of expr that execute while the initialiser
// runs: everything except the bodies of func literals that are not invoked
// immediately (an immediately-invoked func literal's body is visited).
func runsAtInit(expr ast.Node, visit func(ast.Node)) {
	ast.Inspect(expr, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if fl, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
				for _, arg := range call.Args {
					runsAtInit(arg, visit)
				}
				runsAtInit(fl.Body, visit)
				return false
			}
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		visit(n)
		return true
	})
}

// calleeOf resolves the function a call invokes. dynamic is true for calls
// through func values or interface methods (the callee is not static).
// Conversions and builtins return (nil, false).
func (a *analysis) calleeOf(call *ast.CallExpr) (fn *types.Func, dynamic bool) {
	fun := ast.Unparen(call.Fun)
	if tv, ok := a.info.Types[fun]; ok && tv.IsType() {
		return nil, false
	}
	switch ix := fun.(type) {
	case *ast.IndexExpr:
		fun = ix.X
	case *ast.IndexListExpr:
		fun = ix.X
	}
	var obj types.Object
	switch f := fun.(type) {
	case *ast.Ident:
		obj = a.info.Uses[f]
	case *ast.SelectorExpr:
		if sel, ok := a.info.Selections[f]; ok {
			obj = sel.Obj()
		} else {
			obj = a.info.Uses[f.Sel]
		}
	case *ast.FuncLit:
		return nil, false // handled by the caller (runsAtInit / body walk)
	default:
		return nil, true
	}
	switch o := obj.(type) {
	case *types.Builtin:
		return nil, false
	case *types.Func:
		if recv := o.Signature().Recv(); recv != nil {
			if _, isIface := recv.Type().Underlying().(*types.Interface); isIface {
				return nil, true
			}
		}
		return origin(o).(*types.Func), false
	}
	return nil, true
}

type reachInfo struct {
	moved   string // a moved function reachable from here ("" if none)
	dynamic bool   // makes calls through func values or interfaces
	done    bool
}

// reach reports what a function of the source package can reach through
// static calls (its whole body, including closures, conservatively).
func (a *analysis) reach(fn *types.Func) reachInfo {
	if a.reachMemo == nil {
		a.reachMemo = map[*types.Func]*reachInfo{}
		a.funcDecls = map[*types.Func]*ast.FuncDecl{}
		for _, f := range a.checked {
			for _, d := range f.AST.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					if o, ok := a.info.Defs[fd.Name].(*types.Func); ok {
						a.funcDecls[o] = fd
					}
				}
			}
		}
	}
	if r, ok := a.reachMemo[fn]; ok {
		return *r // done, or in progress (a cycle contributes nothing new)
	}
	r := &reachInfo{}
	a.reachMemo[fn] = r
	if h := a.home(fn); h != nil && h.Moved {
		r.moved = funcLabel(fn)
		r.done = true
		return *r
	}
	fd := a.funcDecls[fn]
	if fd == nil {
		r.done = true
		return *r
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if r.moved != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, dyn := a.calleeOf(call)
		if dyn {
			r.dynamic = true
			return true
		}
		if callee == nil || callee.Pkg() != a.pkg {
			return true
		}
		sub := a.reach(callee)
		if sub.moved != "" {
			r.moved = sub.moved
		}
		r.dynamic = r.dynamic || sub.dynamic
		return true
	})
	r.done = true
	return *r
}

// movedHasState reports whether the moved files declare package-level vars.
func (a *analysis) movedHasState() bool {
	for _, name := range a.pkg.Scope().Names() {
		obj := a.pkg.Scope().Lookup(name)
		if h := a.home(obj); h != nil && h.Moved && kindOf(obj) == "var" {
			return true
		}
	}
	return false
}

func funcLabel(fn *types.Func) string {
	if fn.Signature().Recv() != nil {
		return ownerName(fn) + "." + fn.Name()
	}
	return fn.Name()
}

func exprString(e ast.Expr) string {
	var b bytes.Buffer
	_ = format.Node(&b, token.NewFileSet(), e)
	return b.String()
}

func isGenericFunc(obj types.Object) bool {
	f, ok := obj.(*types.Func)
	return ok && f.Signature().Recv() == nil && f.Signature().TypeParams().Len() > 0
}

// callSelectors returns the selector expressions used as the function of a
// call (including explicit instantiations) in a file.
func callSelectors(f *ast.File) map[*ast.SelectorExpr]bool {
	calls := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			fun := ast.Unparen(call.Fun)
			switch ix := fun.(type) {
			case *ast.IndexExpr:
				fun = ix.X
			case *ast.IndexListExpr:
				fun = ix.X
			}
			if sel, ok := fun.(*ast.SelectorExpr); ok {
				calls[sel] = true
			}
		}
		return true
	})
	return calls
}

// modulePackages lists every package of the module (cached).
func (a *analysis) modulePackages() ([]listedPackage, error) {
	if a.modPkgs != nil {
		return a.modPkgs, nil
	}
	out, err := goList(a.mod.ModDir, a.cfg.Tags, "-e", "-json=ImportPath,Dir,GoFiles,TestGoFiles,XTestGoFiles,IgnoredGoFiles,Imports,TestImports,XTestImports", "./...")
	if err != nil {
		return nil, err
	}
	pkgs, err := decodeList(out)
	if err != nil {
		return nil, err
	}
	a.modPkgs = pkgs
	return pkgs, nil
}

// scanLinknames reports //go:linkname directives anywhere in the module
// (including test and build-excluded files) that target a moved symbol by
// its old path: the linker symbol name changes with the package path.
func (a *analysis) scanLinknames() {
	pkgs, err := a.modulePackages()
	if err != nil {
		a.plan.errorf("scanning the module for go:linkname: %v", err)
		return
	}
	prefix := a.mod.ImportPath + "."
	for _, p := range pkgs {
		var names []string
		names = append(names, p.GoFiles...)
		names = append(names, p.TestGoFiles...)
		names = append(names, p.XTestGoFiles...)
		names = append(names, p.IgnoredGoFiles...)
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(p.Dir, name)
			b, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(b, []byte("go:linkname")) {
				continue
			}
			for i, line := range strings.Split(string(b), "\n") {
				fields := strings.Fields(strings.TrimSpace(line))
				if len(fields) < 3 || fields[0] != "//go:linkname" || !strings.HasPrefix(fields[2], prefix) {
					continue
				}
				sym := strings.TrimPrefix(fields[2], prefix)
				sym = strings.TrimLeft(sym, "(*")
				first := sym
				if j := strings.IndexAny(sym, ".)"); j >= 0 {
					first = sym[:j]
				}
				obj := a.pkg.Scope().Lookup(first)
				if h := a.home(obj); h != nil && h.Moved {
					a.plan.errorf("%s:%d: %s targets %s, which moves (its linker name becomes %s.%s) - update the directive first",
						a.rel(path), i+1, strings.TrimSpace(line), fields[2], a.dstImport, strings.TrimPrefix(fields[2], prefix))
				}
			}
		}
	}
}

// funcValueFindings warns once per moved func whose value is observable:
// taken as a value anywhere in the source package, or aliased as a var
// (exported funcs, which importers may take as values). runtime.FuncForPC and
// stack traces name it by its new package path.
func (a *analysis) funcValueFindings() {
	values := map[types.Object]bool{}
	for _, u := range a.uses {
		if u.def || a.callFuns[u.id] {
			continue
		}
		if fn, ok := u.obj.(*types.Func); ok && (a.isPkgLevel(fn) || fn.Signature().Recv() != nil) {
			if h := a.home(fn); h != nil && h.Moved {
				values[fn] = true // funcs, method values and method expressions
			}
		}
	}
	for _, e := range a.plan.Aliases {
		if e.Kind == "var" {
			if obj := a.pkg.Scope().Lookup(e.Old); obj != nil {
				values[obj] = true
			}
		}
	}
	var objs []types.Object
	for o := range values {
		objs = append(objs, o)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Pos() < objs[j].Pos() })
	for _, o := range objs {
		oldName, newName := o.Name(), a.newName(o)
		if fn := o.(*types.Func); fn.Signature().Recv() != nil {
			oldName = ownerName(fn) + "." + fn.Name()
			newName = ownerName(fn) + "." + firstNonEmpty(a.memberRename[fn], fn.Name())
		}
		a.plan.add(levelWarn, "func name seen through its value changes (runtime.FuncForPC, stack traces)", a.posOf(o.Pos()),
			"%s.%s is now %s.%s for runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name() and panic traces", a.mod.ImportPath, oldName, a.dstImport, newName)
	}
}

// testdataFindings warns when moved tests read files relative to the package
// directory (testdata/..., ./...) that do not move with them.
func (a *analysis) testdataFindings() {
	moved := map[string]bool{}
	for _, n := range a.assets {
		moved[n] = true
	}
	movedTests := false
	for _, f := range a.files {
		if !f.Moved || !f.IsTest {
			continue
		}
		movedTests = true
		ast.Inspect(f.AST, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil || (!strings.HasPrefix(text, "testdata/") && !strings.HasPrefix(text, "./") && text != ".." && !strings.HasPrefix(text, "../")) {
				return true
			}
			if text == ".." || strings.HasPrefix(text, "../") {
				a.plan.add(levelWarn, "moved test reads package-relative files", a.posOf(lit.Pos()),
					"%q is resolved against the package directory, which moves one level deeper to %s; adjust the path", text, a.rel(a.cfg.DstDir))
				return true
			}
			top := strings.Split(strings.TrimPrefix(text, "./"), "/")[0]
			if moved[top] {
				return true
			}
			a.plan.add(levelWarn, "moved test reads package-relative files", a.posOf(lit.Pos()),
				"%q is resolved against the package directory, which changes to %s; move it as an asset (list %s in the file set) or adjust the path", text, a.rel(a.cfg.DstDir), top)
			return true
		})
	}
	// Moved tests that locate files from the working directory.
	for _, f := range a.files {
		if !f.Moved || !f.IsTest {
			continue
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				if x, ok := fn.X.(*ast.Ident); ok {
					name = x.Name + "." + fn.Sel.Name
				}
			}
			lower := strings.ToLower(name)
			if name == "os.Getwd" || (strings.HasPrefix(lower, "find") && strings.HasSuffix(lower, "dir")) {
				a.plan.add(levelWarn, "moved test reads package-relative files", a.posOf(call.Pos()),
					"%s() resolves paths from the test's working directory, which becomes %s; check the paths derived from it", name, a.rel(a.cfg.DstDir))
			}
			return true
		})
	}
	if _, err := os.Stat(filepath.Join(a.cfg.SrcDir, "testdata")); err == nil && movedTests && !moved["testdata"] {
		a.plan.add(levelWarn, "moved test reads package-relative files", a.rel(filepath.Join(a.cfg.SrcDir, "testdata")),
			"tests move but the testdata directory stays; if the moved tests read it, list testdata (or its relevant files) in the file set")
	}
}

// sourceScanFindings reports test files of the source package (staying or
// moved, including external tests) that parse Go sources and enumerate files
// from the package directory. Such guard tests (forbidden literals,
// enumeration and call-site checks) silently stop covering the moved files,
// or cover the wrong set after moving, and still pass.
func (a *analysis) sourceScanFindings() {
	parsers := map[string]bool{"go/parser": true, "golang.org/x/tools/go/packages": true}
	enumerators := map[string]bool{
		"os.ReadDir": true, "os.Getwd": true, "ioutil.ReadDir": true, "filepath.Glob": true,
		"filepath.WalkDir": true, "filepath.Walk": true, "parser.ParseDir": true, "packages.Load": true,
		"fs.WalkDir": true, "fs.Glob": true, "fs.ReadDir": true,
	}
	for _, f := range a.files {
		if !f.IsTest {
			continue
		}
		usesParser := false
		for _, spec := range f.AST.Imports {
			if p, _ := strconv.Unquote(spec.Path.Value); parsers[p] {
				usesParser = true
			}
		}
		if !usesParser {
			continue
		}
		var calls []string
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && enumerators[x.Name+"."+sel.Sel.Name] {
						calls = append(calls, x.Name+"."+sel.Sel.Name)
					}
				}
			}
			return true
		})
		if len(calls) == 0 {
			continue
		}
		side := "staying"
		if f.Moved {
			side = "moved"
		}
		a.plan.add(levelHigh, "source-scanning test does not cover the target", a.rel(f.Path),
			"%s test parses Go sources and enumerates files (%s); after the move it no longer scans the moved files in %s (or scans the wrong set) and still passes - extend the scan to cover both directories",
			side, strings.Join(dedupStrings(sortedCopy(calls)), ", "), a.rel(a.cfg.DstDir))
	}
}
