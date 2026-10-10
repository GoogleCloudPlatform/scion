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
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// analyzeRewriteAliases plans a -rewrite-aliases run: every reference in the
// source package (and its external tests) to a resolvable alias becomes a
// direct reference to the alias target, and unexported entries of
// pkgmove-generated alias files that are no longer referenced are removed
// (a file left without entries is deleted). With -to, only aliases of that
// package are rewritten. No file moves.
func (a *analysis) analyzeRewriteAliases() (*analysis, error) {
	a.plan.RewriteAliases = true
	if len(a.cfg.Files) > 0 {
		return nil, fmt.Errorf("-rewrite-aliases takes no files")
	}
	if err := a.typecheckSource(); err != nil {
		return nil, err
	}
	a.collectUses()
	a.collectAliases()
	selected := func(al *aliasInfo) bool { return al != nil && (a.dstImport == "" || al.path == a.dstImport) }

	remaining := map[types.Object]int{}
	users := map[types.Object][]types.Object{} // alias -> alias entries that use it
	why := map[types.Object][]string{}
	keep := func(obj types.Object, reason string) {
		remaining[obj]++
		why[obj] = append(why[obj], reason)
	}
	for _, u := range a.uses {
		if u.def || u.file == nil {
			continue
		}
		al := a.aliases[u.obj]
		if !selected(al) {
			continue
		}
		switch {
		case isGeneratedAliasFile(u.file):
			// Kept while the alias entry that uses it is kept (decided below).
			if user := a.enclosingAlias(u.file, u.id.Pos()); user != nil {
				if user != u.obj {
					users[u.obj] = append(users[u.obj], user)
				}
			} else {
				keep(u.obj, "used by "+u.file.Name)
			}
		case a.embeddedFieldAt(u.id) != nil && al.name != u.obj.Name():
			keep(u.obj, "embedded as a field")
			a.plan.add(levelInfo, "alias reference kept (an embedded field would be renamed)", a.posOf(u.id.Pos()),
				"%s embeds the alias %s; referring to %s directly would rename the field", u.file.Name, u.obj.Name(), al.target())
		case al.kind == "func" && !a.callFuns[u.id]:
			keep(u.obj, "used as a func value")
			a.plan.add(levelInfo, "alias reference kept (func value of a wrapper)", a.posOf(u.id.Pos()),
				"%s is a wrapper of %s used as a value; referring to %s directly would change the value's identity", u.obj.Name(), al.target(), al.target())
		default:
			a.aliasUses = append(a.aliasUses, u)
		}
	}

	// Names of selected aliases, for the files that are not type-checked.
	byName := map[string]types.Object{}
	for obj, al := range a.aliases {
		if selected(al) {
			byName[obj.Name()] = obj
		}
	}
	for _, f := range a.files {
		switch {
		case f.XTest && f.Included:
			a.collectXTestAliasSels(f, selected, keep)
		case !f.Included && !f.XTest:
			var hits []string
			ast.Inspect(f.AST, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if obj := byName[id.Name]; obj != nil {
						hits = append(hits, id.Name)
						keep(obj, "named in "+f.Name)
					}
				}
				return true
			})
			if len(hits) > 0 {
				a.plan.add(levelWarn, "file excluded by build constraints (not type-checked)", a.rel(f.Path),
					"names aliases (not rewritten; their entries are kept): %s - rewrite them by hand under their build tags", strings.Join(dedupStrings(sortedCopy(hits)), ", "))
			}
		}
	}

	// Remove unexported generated entries that nothing references any more.
	decls := map[*srcFile]int{}
	for _, f := range a.files {
		if isGeneratedAliasFile(f) {
			for _, d := range f.AST.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					decls[f]++
				case *ast.GenDecl:
					if d.Tok != token.IMPORT {
						decls[f] += len(d.Specs)
					}
				}
			}
		}
	}
	var objs []types.Object
	for obj, al := range a.aliases {
		if selected(al) {
			objs = append(objs, obj)
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Pos() < objs[j].Pos() })
	// A //go:linkname anywhere in the module may pull an entry by name.
	linked := a.linknamed()
	for _, obj := range objs {
		if pos, ok := linked[obj.Name()]; ok {
			keep(obj, "targeted by //go:linkname at "+pos)
		}
	}
	// Removable: unexported generated entries with no references left, other
	// than from alias entries that are removed too (a greatest fixpoint).
	cand := map[types.Object]bool{}
	for _, obj := range objs {
		if al := a.aliases[obj]; al.generated && !obj.Exported() && remaining[obj] == 0 {
			cand[obj] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, obj := range objs {
			if !cand[obj] {
				continue
			}
			for _, user := range users[obj] {
				if !cand[user] {
					delete(cand, obj)
					changed = true
					break
				}
			}
		}
	}
	removed := map[*srcFile][]declRef{}
	kept := map[string][]string{} // message -> names
	for _, obj := range objs {
		al := a.aliases[obj]
		if !cand[obj] && remaining[obj] == 0 && al.generated && !obj.Exported() {
			for _, user := range users[obj] {
				if !cand[user] {
					remaining[obj]++
					why[obj] = append(why[obj], "used by the kept alias entry "+user.Name())
				}
			}
		}
		switch {
		case !al.generated:
			kept["hand-written alias (never removed): "+a.rel(al.file.Path)] = append(kept["hand-written alias (never removed): "+a.rel(al.file.Path)], obj.Name())
		case obj.Exported():
			k := "exported (importers may use it): " + a.rel(al.file.Path)
			kept[k] = append(kept[k], obj.Name())
		case remaining[obj] > 0:
			a.plan.add(levelInfo, "alias entry kept (still referenced)", a.posOf(obj.Pos()),
				"%s: %s", obj.Name(), strings.Join(dedupStrings(sortedCopy(why[obj])), "; "))
		default:
			d := declRef{name: obj.Name(), kind: al.kind, file: al.file, gen: al.gen}
			if al.fn != nil {
				d.node = al.fn
			} else {
				d.node = al.spec
			}
			removed[al.file] = append(removed[al.file], d)
			a.plan.RemovedAliases = append(a.plan.RemovedAliases, fmt.Sprintf("%s: %s %s = %s", a.posOf(obj.Pos()), al.kind, obj.Name(), al.target()))
		}
	}
	for msg, names := range kept {
		a.plan.add(levelInfo, "alias entries kept", "", "%s: %s", msg, strings.Join(sortedCopy(names), ", "))
	}
	var whys []types.Object
	for obj := range a.unresolvedWhy {
		whys = append(whys, obj)
	}
	sort.Slice(whys, func(i, j int) bool { return whys[i].Pos() < whys[j].Pos() })
	for _, obj := range whys {
		a.plan.add(levelInfo, "forwarding declaration not resolved", a.posOf(obj.Pos()), "%s", a.unresolvedWhy[obj])
	}
	if len(a.plan.Errors) > 0 {
		a.plan.normalize()
		return a, nil
	}
	if err := a.buildEdits(); err != nil {
		return nil, err
	}
	for _, f := range sortedFileKeys(removed) {
		if len(removed[f]) == decls[f] {
			delete(a.edits, f)
			a.deletes = append(a.deletes, f)
			a.plan.DeletedFiles = append(a.plan.DeletedFiles, a.rel(f.Path))
			continue
		}
		for _, d := range removed[f] {
			a.removeDecl(d)
		}
	}
	a.plan.TouchedFiles = nil
	for f := range a.edits {
		a.plan.TouchedFiles = append(a.plan.TouchedFiles, a.rel(f.Path))
	}
	a.plan.normalize()
	return a, nil
}

// linknamed returns the names of the source package that a //go:linkname
// directive anywhere in the module targets (with the first position).
func (a *analysis) linknamed() map[string]string {
	out := map[string]string{}
	pkgs, err := a.modulePackages()
	if err != nil {
		a.plan.errorf("scanning the module for go:linkname: %v", err)
		return out
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
				if j := strings.IndexAny(sym, ".)"); j >= 0 {
					sym = sym[:j]
				}
				if _, ok := out[sym]; !ok {
					out[sym] = a.rel(path) + ":" + strconv.Itoa(i+1)
				}
			}
		}
	}
	return out
}

// enclosingAlias returns the alias whose declaration in f contains pos.
func (a *analysis) enclosingAlias(f *srcFile, pos token.Pos) types.Object {
	for obj, al := range a.aliases {
		if al.file != f {
			continue
		}
		var n ast.Node = al.spec
		if al.fn != nil {
			n = al.fn
		}
		if n.Pos() <= pos && pos < n.End() {
			return obj
		}
	}
	return nil
}

// collectXTestAliasSels finds, in an external test file, the selectors on
// the source import that name a selected alias and can be rewritten.
func (a *analysis) collectXTestAliasSels(f *srcFile, selected func(*aliasInfo) bool, keep func(types.Object, string)) {
	local := a.srcImportName(f)
	if local == "" || local == "_" || local == "." {
		return
	}
	calls := callSelectors(f.AST)
	embedded := embeddedSelectors(f.AST)
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
		al := a.aliases[obj]
		if !selected(al) {
			return true
		}
		if (al.kind == "func" && !calls[sel]) || (al.name != obj.Name() && embedded[sel]) {
			keep(obj, "kept in "+f.Name)
			return true
		}
		a.xtestAliasSels[f] = append(a.xtestAliasSels[f], sel)
		return true
	})
}

// verifyRewrite runs the post-rewrite sanity checks of -rewrite-aliases.
func (a *analysis) verifyRewrite() error {
	out := a.cfg.Stdout
	printf(out, "\nsanity: go list %s\n", a.mod.ImportPath)
	if _, err := goList(a.mod.ModDir, a.cfg.Tags, "-test", a.mod.ImportPath); err != nil {
		return fmt.Errorf("post-rewrite go list failed: %v", err)
	}
	printf(out, "sanity: go list OK\n")
	if !a.cfg.Typecheck {
		return nil
	}
	printf(out, "sanity: type-checking the package (with in-package tests)\n")
	fset := token.NewFileSet()
	files, err := readDir(fset, a.cfg.SrcDir, buildContext(a.cfg.Tags))
	if err != nil {
		return err
	}
	var checked []*srcFile
	for _, f := range files {
		if f.Included && !f.XTest {
			checked = append(checked, f)
		}
	}
	imp, err := newExportImporter(fset, a.cfg.SrcDir, a.cfg.Tags, importsOf(checked, a.mod.ImportPath))
	if err != nil {
		return err
	}
	if _, _, err := typeCheck(fset, a.mod.ImportPath, checked, imp, a.mod.GoVersion); err != nil {
		return err
	}
	printf(out, "sanity: type-check OK\n")
	return nil
}
