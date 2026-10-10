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
}

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
	vars := map[string]bool{}
	for _, name := range a.pkg.Scope().Names() {
		obj := a.pkg.Scope().Lookup(name)
		if h := a.home(obj); h != nil && h.Moved && obj.Exported() && kindOf(obj) == "var" {
			vars[name] = true
		}
	}
	if len(vars) == 0 {
		return
	}
	out, err := goList(a.mod.ModDir, a.cfg.Tags, "-e", "-json=ImportPath,Dir,GoFiles,TestGoFiles,XTestGoFiles,Imports,TestImports,XTestImports", "./...")
	if err != nil {
		a.plan.errorf("scanning the module for uses of moved exported vars: %v", err)
		return
	}
	pkgs, err := decodeList(out)
	if err != nil {
		a.plan.errorf("scanning the module: %v", err)
		return
	}
	fset := token.NewFileSet()
	for _, p := range pkgs {
		if p.ImportPath == a.mod.ImportPath {
			continue // the source package and its external tests are analysed directly
		}
		if !contains(p.Imports, a.mod.ImportPath) && !contains(p.TestImports, a.mod.ImportPath) && !contains(p.XTestImports, a.mod.ImportPath) {
			continue
		}
		var names []string
		names = append(names, p.GoFiles...)
		names = append(names, p.TestGoFiles...)
		names = append(names, p.XTestGoFiles...)
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
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && vars[sel.Sel.Name] {
						pos := fset.Position(sel.Pos())
						a.plan.errorf("%s:%d: %s.%s refers to a moved exported var; vars cannot be aliased - rewrite the reference to the target package first",
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
		if f.Included && !f.XTest {
			a.initFindings(f)
		}
	}
	a.reflectionFindings()
	// Remaining var initialisers that read moved symbols.
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
				var refs []string
				for _, v := range vs.Values {
					ast.Inspect(v, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok {
							if obj := a.info.Uses[id]; obj != nil {
								if h := a.home(origin(obj)); h != nil && h.Moved && a.isPkgLevel(obj) {
									refs = append(refs, obj.Name())
								}
							}
						}
						return true
					})
				}
				if len(refs) > 0 {
					a.plan.add(levelInfo, "staying var initialiser reads moved symbols", a.posOf(vs.Pos()),
						"%s reads %s, now initialised in package %s (before all of %s)", identNames(vs.Names), strings.Join(dedupStrings(sortedCopy(refs)), ", "), a.cfg.PkgName, a.srcName)
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

func (a *analysis) initFindings(f *srcFile) {
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == "init" {
				a.plan.add(levelHigh, "init() in moved file", a.posOf(d.Pos()),
					"init() now runs when package %s initialises, before every var initialiser and init() of %s", a.cfg.PkgName, a.srcName)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs := spec.(*ast.ValueSpec)
				var calls, otherCalls, deps []string
				for _, v := range vs.Values {
					ast.Inspect(v, func(n ast.Node) bool {
						switch n := n.(type) {
						case *ast.FuncLit:
							return false // body does not run at init unless called
						case *ast.CallExpr:
							a.classifyCall(f, n, &calls, &otherCalls)
						case *ast.Ident:
							obj := a.info.Uses[n]
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
						}
						return true
					})
				}
				names := identNames(vs.Names)
				if len(calls) > 0 {
					a.plan.add(levelHigh, "package-level var initialiser calls package code", a.posOf(vs.Pos()),
						"%s = ... calls %s", names, strings.Join(dedupStrings(sortedCopy(calls)), ", "))
				}
				if len(deps) > 0 {
					a.plan.add(levelWarn, "package-level var initialiser depends on other files", a.posOf(vs.Pos()),
						"%s = ... reads %s", names, strings.Join(dedupStrings(sortedCopy(deps)), ", "))
				}
				if len(otherCalls) > 0 {
					a.plan.add(levelInfo, "package-level var initialiser calls other packages", a.posOf(vs.Pos()),
						"%s = ... calls %s (imported packages initialise first in both layouts)", names, strings.Join(dedupStrings(sortedCopy(otherCalls)), ", "))
				}
			}
		}
	}
}

func (a *analysis) classifyCall(f *srcFile, call *ast.CallExpr, calls, otherCalls *[]string) {
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
		*calls = append(*calls, "an immediately-invoked func literal")
		return
	}
	if id == nil {
		*calls = append(*calls, "a computed function value")
		return
	}
	obj := a.info.Uses[id]
	switch obj := obj.(type) {
	case *types.Builtin:
		return
	case *types.Func:
		if obj.Pkg() == a.pkg {
			*calls = append(*calls, obj.Name())
		} else if obj.Pkg() != nil {
			*otherCalls = append(*otherCalls, obj.Pkg().Name()+"."+obj.Name())
		}
	case *types.Var:
		if obj.Pkg() == a.pkg {
			*calls = append(*calls, obj.Name()+" (func value)")
		} else {
			*otherCalls = append(*otherCalls, obj.Name()+" (func value)")
		}
	default:
		*calls = append(*calls, id.Name)
	}
}

// chooseImportName picks a name for the target import in file f that does
// not collide with package-level names, the file's imports, or (at the given
// positions) any local declaration.
func (a *analysis) chooseImportName(f *srcFile, at []token.Pos, extraReserved map[string]bool) string {
	reserved := map[string]bool{}
	for k := range extraReserved {
		reserved[k] = true
	}
	for _, spec := range f.AST.Imports {
		reserved[importName(spec)] = true
	}
	for i := 1; ; i++ {
		name := a.cfg.PkgName
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
		if kindOf(u.obj) == "var" && !u.def {
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
	// Moved external tests: re-qualify references to moved symbols.
	var xfiles []*srcFile
	for f := range a.xtestSels {
		xfiles = append(xfiles, f)
	}
	sort.Slice(xfiles, func(i, j int) bool { return xfiles[i].Name < xfiles[j].Name })
	for _, f := range xfiles {
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
		q := a.chooseImportName(f, nil, reserved)
		local := a.srcImportName(f)
		total := 0
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == local {
				total++
			}
			return true
		})
		for _, sel := range sels {
			a.replaceIdent(f, sel.X.(*ast.Ident), q)
		}
		a.addImport(f, q, a.dstImport, a.cfg.PkgName)
		if total == len(sels) {
			a.removeImport(f, a.mod.ImportPath)
		}
	}
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
	spec := importSpecText(name, path, realName)
	fe := a.editsFor(f)
	var last *ast.GenDecl
	for _, d := range f.AST.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			last = gd
		}
	}
	switch {
	case last == nil:
		off := a.offset(f.AST.Name.End())
		fe.edits = append(fe.edits, edit{off, off, "\n\nimport " + spec + "\n"})
	case last.Rparen.IsValid():
		off := a.offset(last.Rparen)
		i := off - 1
		for i >= 0 && (f.Src[i] == ' ' || f.Src[i] == '\t') {
			i--
		}
		text := "\t" + spec + "\n"
		lastSpec := last.Specs[len(last.Specs)-1].(*ast.ImportSpec)
		if p, _ := strconv.Unquote(lastSpec.Path.Value); isStdPath(p) && !isStdPath(path) {
			text = "\n" + text // start a new (non-standard) group
		}
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
		sep := "\n\t"
		if p, _ := strconv.Unquote(old.Path.Value); isStdPath(p) && !isStdPath(path) {
			sep = "\n\n\t"
		}
		fe.edits = append(fe.edits, edit{start, end, "(\n\t" + string(f.Src[start:end]) + sep + spec + "\n)"})
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
	edits := append([]edit(nil), fe.edits...)
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
	formatted, err := format.Source(out)
	if err != nil {
		return nil, fmt.Errorf("%s: rewritten file does not parse: %v", f.Path, err)
	}
	return formatted, nil
}

// reflectionFindings warns when a renamed method's old or new name appears
// where it may be looked up by name at run time: template strings
// ({{.Name}}), reflect MethodByName/FieldByName calls, and non-Go files of the
// source directory (templates). Exporting a method makes it visible to
// text/template, html/template, reflect and RPC-style dispatch.
func (a *analysis) reflectionFindings() {
	names := map[string]string{} // name -> "old -> new"
	for obj, n := range a.memberRename {
		desc := ownerName(obj)
		if desc == "" {
			desc = a.fieldOwner(obj)
		}
		desc += "." + obj.Name() + " -> " + n
		names[obj.Name()] = desc
		names[n] = desc
	}
	if len(names) == 0 {
		return
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
	entries, err := os.ReadDir(a.cfg.SrcDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(a.cfg.SrcDir, e.Name())
		if info, err := e.Info(); err != nil || info.Size() > 4<<20 {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, name := range matchName(string(b)) {
			a.plan.add(levelWarn, "renamed method name appears in a template or reflection string", a.rel(path),
				"mentions .%s (%s); check templates that call methods by name", name, names[name])
		}
	}
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
