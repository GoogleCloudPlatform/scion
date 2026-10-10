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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// licenseHeader is written at the top of a newly created helper file.
const licenseHeader = `// Copyright 2026 Google LLC
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
`

// family is one extraction request: move the named declarations into dest.
type family struct {
	dest  string   // base name of the helper file, must end in _helpers_test.go
	names []string // requested top-level names, in output order
	// build is the //go:build expression for a new dest file ("" = none).
	// If set, an existing dest must already carry exactly this constraint.
	build string
}

// move is one declaration selected for moving.
type move struct {
	d      *declInfo
	reason string // "requested", "dependency of X", "method of X"
}

type result struct {
	moves   []move
	changed map[string][]byte // base name -> new content; nil content = delete
}

// plan computes the moves and new file contents without writing anything.
func plan(p *pkgInfo, fam family) (*result, error) {
	if !strings.HasSuffix(fam.dest, "_helpers_test.go") || filepath.Base(fam.dest) != fam.dest {
		return nil, fmt.Errorf("dest %q must be a base name ending in _helpers_test.go", fam.dest)
	}
	var destFile *srcFile
	for _, f := range p.files {
		if f.name == fam.dest {
			destFile = f
		}
	}

	// Resolve the requested names.
	selected := map[*declInfo]bool{}
	var order []move
	add := func(d *declInfo, why string) {
		if selected[d] {
			return
		}
		selected[d] = true
		order = append(order, move{d: d, reason: why})
	}
	for _, n := range fam.names {
		ds := p.byKey[n]
		if len(ds) != 1 {
			return nil, fmt.Errorf("%s: declared %d times, want exactly once", n, len(ds))
		}
		d := ds[0]
		if !d.file.isTest {
			return nil, fmt.Errorf("%s: declared in non-test file %s", n, d.file.name)
		}
		if d.recv != "" {
			return nil, fmt.Errorf("%s: methods move with their type, not by name", n)
		}
		add(d, "requested")
	}

	// Closure: methods of moved types, and test-file declarations used only
	// by moved declarations.
	users := p.users()
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(order); i++ {
			d := order[i].d
			if isType(d) {
				for _, m := range p.decls {
					if m.recv == d.key && !selected[m] {
						if !m.file.isTest {
							return nil, fmt.Errorf("type %s has method %s in non-test file %s", d.key, m.key, m.file.name)
						}
						add(m, "method of "+d.key)
						changed = true
					}
				}
			}
			for _, r := range sortedKeys(p.refs(d)) {
				ds := p.byKey[r]
				if len(ds) != 1 || selected[ds[0]] || !ds[0].file.isTest || ds[0].file == destFile {
					continue
				}
				only := true
				for u := range users[r] {
					if !selected[u] {
						only = false
						break
					}
				}
				if only {
					add(ds[0], "dependency of "+d.key)
					changed = true
				}
			}
		}
	}

	// Already in dest: nothing to move for those.
	var moves []move
	for _, m := range order {
		if m.d.file != destFile {
			moves = append(moves, m)
		}
	}
	if len(moves) == 0 {
		return &result{changed: map[string][]byte{}}, nil
	}

	// GenDecl groups must move whole.
	for _, m := range moves {
		if g, ok := m.d.node.(*ast.GenDecl); ok {
			for _, o := range p.decls {
				if o.node == g && !selected[o] {
					return nil, fmt.Errorf("%s shares a declaration group with %s, which is not moving", m.d.key, o.key)
				}
			}
		}
	}

	destConstraint := fam.build
	if destFile != nil {
		if fam.build != "" && destFile.constraint != fam.build {
			return nil, fmt.Errorf("dest %s has constraint %q, family wants %q", fam.dest, destFile.constraint, fam.build)
		}
		destConstraint = destFile.constraint
	}
	if err := checkConstraints(p, moves, destConstraint, selected); err != nil {
		return nil, err
	}

	res := &result{moves: moves, changed: map[string][]byte{}}

	// New dest content.
	destSrc, err := buildDest(p, destFile, moves, destConstraint)
	if err != nil {
		return nil, err
	}
	res.changed[fam.dest] = destSrc

	// Source files.
	bySrc := map[*srcFile][]*declInfo{}
	for _, m := range moves {
		bySrc[m.d.file] = append(bySrc[m.d.file], m.d)
	}
	for f, ds := range bySrc {
		out, err := removeDecls(p, f, ds)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		res.changed[f.name] = out
	}
	return res, nil
}

func isType(d *declInfo) bool {
	g, ok := d.node.(*ast.GenDecl)
	return ok && g.Tok == token.TYPE
}

// users maps each top-level key to the set of declarations (anywhere in the
// package, any build constraint) that refer to it.
func (p *pkgInfo) users() map[string]map[*declInfo]bool {
	u := map[string]map[*declInfo]bool{}
	for _, d := range p.decls {
		for r := range p.refs(d) {
			if u[r] == nil {
				u[r] = map[*declInfo]bool{}
			}
			u[r][d] = true
		}
		if d.recv != "" { // a method uses its receiver type
			if u[d.recv] == nil {
				u[d.recv] = map[*declInfo]bool{}
			}
			u[d.recv][d] = true
		}
	}
	return u
}

// checkConstraints makes sure the move compiles under every build
// configuration. A declaration may move from a file with constraint S into
// dest with constraint D if S == D: it is then built under exactly the same
// tag sets as before, and the imports it brings are added to a file built
// under those same tag sets, so no user can gain or lose it. Or if D == ""
// (always built) and the declaration needs nothing that is not always built:
// each package-level name it uses is declared in unconstrained files (or
// moves too), and each import it needs is already imported by some
// unconstrained file.
func checkConstraints(p *pkgInfo, moves []move, dest string, selected map[*declInfo]bool) error {
	alwaysImported := map[string]bool{}
	for _, f := range p.files {
		if f.constraint != "" {
			continue
		}
		for _, s := range f.ast.Imports {
			ip, _ := strconv.Unquote(s.Path.Value)
			alwaysImported[ip] = true
		}
	}
	for _, m := range moves {
		s := m.d.file.constraint
		if s == dest {
			continue
		}
		if dest != "" {
			return fmt.Errorf("%s: source constraint %q differs from dest constraint %q", m.d.key, s, dest)
		}
		for r := range p.refs(m.d) {
			for _, rd := range p.byKey[r] {
				if rd.file.constraint != "" && !selected[rd] {
					return fmt.Errorf("%s: moving to an unconstrained file, but it uses %s declared in %s (constraint %q)", m.d.key, r, rd.file.name, rd.file.constraint)
				}
			}
		}
		imps, err := neededImports(m.d)
		if err != nil {
			return err
		}
		for _, s := range imps {
			ip, _ := strconv.Unquote(s.Path.Value)
			if !alwaysImported[ip] {
				return fmt.Errorf("%s: moving to an unconstrained file would add import %q to builds that do not import it today", m.d.key, ip)
			}
		}
	}
	return nil
}

// neededImports returns the import specs of d's file that d uses.
func neededImports(d *declInfo) ([]*ast.ImportSpec, error) {
	if err := validateImportNames(d.file); err != nil {
		return nil, err
	}
	used := usedImportNames(d.node)
	var out []*ast.ImportSpec
	for _, s := range d.file.ast.Imports {
		n := importName(s)
		if n == "_" {
			continue
		}
		if n == "." {
			return nil, fmt.Errorf("%s: dot imports are not supported", d.file.name)
		}
		if used[n] {
			out = append(out, s)
		}
	}
	return out, nil
}

// validateImportNames checks that every non-blank import's guessed name is
// used in the file, so a wrong guess fails loudly instead of dropping an
// import.
func validateImportNames(f *srcFile) error {
	used := usedImportNames(f.ast)
	for _, s := range f.ast.Imports {
		n := importName(s)
		if n == "_" || n == "." {
			continue
		}
		if !used[n] {
			return fmt.Errorf("%s: cannot tell the package name of import %s (guessed %q); give it an explicit name", f.name, s.Path.Value, n)
		}
	}
	return nil
}

type importKey struct{ name, path string }

func specKey(s *ast.ImportSpec) importKey {
	k := importKey{path: s.Path.Value}
	if s.Name != nil {
		k.name = s.Name.Name
	}
	return k
}

// buildDest renders the helper file: header, constraint, package clause,
// imports, then the existing dest declarations followed by the moved ones.
func buildDest(p *pkgInfo, destFile *srcFile, moves []move, constraint string) ([]byte, error) {
	var texts []string
	imports := map[importKey]bool{}
	if destFile != nil {
		if err := checkOnlyDecls(p, destFile); err != nil {
			return nil, err
		}
		for _, d := range destFile.ast.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
				continue
			}
			s, e := declSpan(p.fset, destFile, d)
			texts = append(texts, string(destFile.src[s:e]))
		}
		for _, s := range destFile.ast.Imports {
			imports[specKey(s)] = true
		}
	}
	seen := map[ast.Decl]bool{}
	for _, m := range moves {
		if seen[m.d.node] {
			continue // a multi-name GenDecl
		}
		seen[m.d.node] = true
		texts = append(texts, m.d.text(p.fset))
		imps, err := neededImports(m.d)
		if err != nil {
			return nil, err
		}
		for _, s := range imps {
			imports[specKey(s)] = true
		}
	}

	var b bytes.Buffer
	b.WriteString(licenseHeader)
	b.WriteString("\n")
	if constraint != "" {
		fmt.Fprintf(&b, "//go:build %s\n\n", constraint)
	}
	fmt.Fprintf(&b, "package %s\n\n", p.pkg)
	var std, other []importKey
	for k := range imports {
		ip, _ := strconv.Unquote(k.path)
		if strings.Contains(strings.SplitN(ip, "/", 2)[0], ".") {
			other = append(other, k)
		} else {
			std = append(std, k)
		}
	}
	if len(std)+len(other) > 0 {
		b.WriteString("import (\n")
		for i, grp := range [][]importKey{std, other} {
			sort.Slice(grp, func(a, c int) bool { return grp[a].path < grp[c].path })
			if i == 1 && len(std) > 0 && len(other) > 0 {
				b.WriteString("\n")
			}
			for _, k := range grp {
				if k.name != "" {
					fmt.Fprintf(&b, "\t%s %s\n", k.name, k.path)
				} else {
					fmt.Fprintf(&b, "\t%s\n", k.path)
				}
			}
		}
		b.WriteString(")\n\n")
	}
	b.WriteString(strings.Join(texts, "\n\n"))
	b.WriteString("\n")
	return format.Source(b.Bytes())
}

// checkOnlyDecls rejects an existing dest file holding comments outside its
// header and declarations, which a regeneration would drop.
func checkOnlyDecls(p *pkgInfo, f *srcFile) error {
	type rng struct{ s, e int }
	var covered []rng
	for _, d := range f.ast.Decls {
		s, e := declSpan(p.fset, f, d)
		covered = append(covered, rng{s, e})
	}
	pkgOff := p.fset.Position(f.ast.Package).Offset
	for _, cg := range f.ast.Comments {
		cs := p.fset.Position(cg.Pos()).Offset
		if cs < pkgOff {
			continue
		}
		in := false
		for _, r := range covered {
			if cs >= r.s && cs < r.e {
				in = true
			}
		}
		if !in {
			return fmt.Errorf("dest %s has a free-floating comment at offset %d; move it into a doc comment first", f.name, cs)
		}
	}
	return nil
}

// removeDecls deletes ds from f, drops imports f no longer uses, and gofmts
// the result. It returns nil content if f is left with no declarations.
func removeDecls(p *pkgInfo, f *srcFile, ds []*declInfo) ([]byte, error) {
	if err := validateImportNames(f); err != nil {
		return nil, err
	}
	if formatted, err := format.Source(f.src); err != nil || !bytes.Equal(formatted, f.src) {
		return nil, fmt.Errorf("not gofmt-clean before the move; refusing to touch it")
	}
	type rng struct{ s, e int }
	var cuts []rng
	seen := map[ast.Decl]bool{}
	for _, d := range ds {
		if seen[d.node] {
			continue
		}
		seen[d.node] = true
		s, e := d.span(p.fset)
		// Widen to whole lines.
		for s > 0 && f.src[s-1] != '\n' {
			s--
		}
		if i := bytes.IndexByte(f.src[e:], '\n'); i >= 0 {
			e += i + 1
		} else {
			e = len(f.src)
		}
		cuts = append(cuts, rng{s, e})
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].s < cuts[j].s })
	var b bytes.Buffer
	prev := 0
	for _, c := range cuts {
		b.Write(f.src[prev:c.s])
		prev = c.e
	}
	b.Write(f.src[prev:])
	out := b.Bytes()

	// Drop now-unused imports.
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, f.name, out, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("reparse after removal: %w", err)
	}
	remaining := 0
	for _, d := range af.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			continue
		}
		remaining++
	}
	if remaining == 0 {
		pkgOff := fset.Position(af.Package).Offset
		for _, cg := range af.Comments {
			if fset.Position(cg.Pos()).Offset > pkgOff {
				return nil, fmt.Errorf("would be left with only free-floating comments; not deleting it")
			}
		}
		return nil, nil
	}
	used := usedImportNames(af)
	var drop []rng
	for _, gd := range af.Decls {
		g, ok := gd.(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT {
			continue
		}
		kept := 0
		var specDrops []rng
		for _, sp := range g.Specs {
			s := sp.(*ast.ImportSpec)
			n := importName(s)
			if n == "_" || n == "." || used[n] {
				kept++
				continue
			}
			so := fset.Position(s.Pos()).Offset
			if s.Doc != nil {
				so = fset.Position(s.Doc.Pos()).Offset
			}
			eo := fset.Position(s.End()).Offset
			if s.Comment != nil {
				eo = fset.Position(s.Comment.End()).Offset
			}
			for so > 0 && out[so-1] != '\n' {
				so--
			}
			if i := bytes.IndexByte(out[eo:], '\n'); i >= 0 {
				eo += i + 1
			}
			specDrops = append(specDrops, rng{so, eo})
		}
		if kept == 0 && len(specDrops) > 0 {
			so := fset.Position(g.Pos()).Offset
			eo := fset.Position(g.End()).Offset
			if i := bytes.IndexByte(out[eo:], '\n'); i >= 0 {
				eo += i + 1
			}
			drop = append(drop, rng{so, eo})
		} else {
			drop = append(drop, specDrops...)
		}
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].s < drop[j].s })
	b.Reset()
	prev = 0
	for _, c := range drop {
		b.Write(out[prev:c.s])
		prev = c.e
	}
	b.Write(out[prev:])
	return format.Source(b.Bytes())
}

// apply writes the planned changes into p.dir.
func apply(p *pkgInfo, res *result) error {
	for _, name := range sortedKeys(res.changed) {
		path := filepath.Join(p.dir, name)
		if res.changed[name] == nil {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(path, res.changed[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
