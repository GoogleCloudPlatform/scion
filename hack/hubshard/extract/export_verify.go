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
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// exportReport is the result of verifyExport.
type exportReport struct {
	moved      int
	tokenDiffs []string // moved decls whose tokens differ beyond the rename map
	unmatched  []string // target decls with no moved origin, or moved decls missing from the target
	pkg        *verifyReport
	pkgIssues  []string // source package differences not explained by the export
	regenDiffs []string // files that differ from a fresh generator run
}

func (r *exportReport) ok() bool {
	return len(r.tokenDiffs) == 0 && len(r.unmatched) == 0 && len(r.pkgIssues) == 0 && len(r.regenDiffs) == 0
}

func (r *exportReport) write(w io.Writer) {
	_, _ = fmt.Fprintf(w, "moved declarations checked token by token (modulo the rename map): %d\n", r.moved)
	_, _ = fmt.Fprintf(w, "token differences: %d\n", len(r.tokenDiffs))
	for _, s := range r.tokenDiffs {
		_, _ = fmt.Fprintf(w, "  %s\n", s)
	}
	_, _ = fmt.Fprintf(w, "unmatched declarations: %d\n", len(r.unmatched))
	for _, s := range r.unmatched {
		_, _ = fmt.Fprintf(w, "  %s\n", s)
	}
	_, _ = fmt.Fprintf(w, "source package: declarations before=%d after=%d (moved and alias declarations excluded)\n", r.pkg.declsBefore, r.pkg.declsAfter)
	_, _ = fmt.Fprintf(w, "source package: Test/Benchmark/Fuzz/Example funcs before=%d after=%d, differences=%d\n", r.pkg.testsBefore, r.pkg.testsAfter, len(r.pkg.testDiffs))
	_, _ = fmt.Fprintf(w, "source package differences not explained by the export: %d\n", len(r.pkgIssues))
	for _, s := range r.pkgIssues {
		_, _ = fmt.Fprintf(w, "  %s\n", s)
	}
	_, _ = fmt.Fprintf(w, "files differing from a fresh generator run: %d\n", len(r.regenDiffs))
	for _, s := range r.regenDiffs {
		_, _ = fmt.Fprintf(w, "  %s\n", s)
	}
	if r.ok() {
		_, _ = fmt.Fprintln(w, "RESULT: OK")
	} else {
		_, _ = fmt.Fprintln(w, "RESULT: FAIL")
	}
}

// verifyExport checks an export independently of how it was rendered:
//
//   - every moved declaration (with its doc comment) has the same token
//     stream in the target, comments included, except identifiers that the
//     rename map allows to change (old -> new, nothing else);
//   - the target has no other declarations in its non-test files;
//   - in the source package, every other declaration is byte-identical,
//     except field-export call sites, whose tokens may differ only by the
//     field renames; the test entry points and free-floating comments are
//     unchanged;
//   - the alias file and the target files are exactly what a fresh run of
//     the generator on the before tree produces (no hand edits).
func verifyExport(before, after *pkgInfo, targetDir, importPath string, spec exportSpec, aliasFile string) (*exportReport, error) {
	ep, err := planExport(before, spec, filepath.Join(os.TempDir(), "extract-verify-no-such-dir", filepath.Base(targetDir)), importPath, aliasFile)
	if err != nil {
		return nil, fmt.Errorf("re-planning on the before tree: %w", err)
	}
	r := &exportReport{}
	renames := ep.renameMap()

	// Moved declarations against the target.
	target, err := loadPackage(targetDir)
	if err != nil {
		return nil, err
	}
	matched := map[ast.Decl]bool{}
	seen := map[ast.Decl]bool{}
	for _, d := range ep.order {
		if seen[d.node] {
			continue
		}
		seen[d.node] = true
		r.moved++
		key := d.key
		if d.recv != "" {
			key = renamed(renames, d.recv) + "." + d.key[len(d.recv)+1:]
		} else {
			key = renamed(ep.exported, d.key)
		}
		var td *declInfo
		for _, c := range target.byKey[key] {
			if !c.file.isTest {
				td = c
			}
		}
		if td == nil {
			r.unmatched = append(r.unmatched, fmt.Sprintf("%s (%s): not found in the target as %s", d.key, d.file.name, key))
			continue
		}
		matched[td.node] = true
		if want := targetFileName(d.file.name); td.file.name != want {
			r.unmatched = append(r.unmatched, fmt.Sprintf("%s: in target file %s, want %s", d.key, td.file.name, want))
		}
		if diff := tokenDiff(d.text(before.fset), td.text(target.fset), renames); diff != "" {
			r.tokenDiffs = append(r.tokenDiffs, fmt.Sprintf("%s (%s -> %s): %s", d.key, d.file.name, td.file.name, diff))
		}
	}
	for _, td := range target.decls {
		if !td.file.isTest && !matched[td.node] {
			r.unmatched = append(r.unmatched, fmt.Sprintf("target declaration %s (%s) has no moved origin", td.key, td.file.name))
		}
	}

	// The source package, without the moved declarations and the alias file.
	movedKeys := map[string]bool{}
	for _, d := range ep.order {
		movedKeys[d.key] = true
	}
	b := filterPkg(before, func(d *declInfo) bool { return !ep.selected[d] }, "")
	a := filterPkg(after, func(d *declInfo) bool { return d.file.name != aliasFile }, aliasFile)
	r.pkg = verify(b, a)
	r.pkgIssues = append(r.pkgIssues, r.pkg.missing...)
	for _, s := range r.pkg.added {
		r.pkgIssues = append(r.pkgIssues, "added "+s)
	}
	r.pkgIssues = append(r.pkgIssues, r.pkg.badConstraintChanges...)
	r.pkgIssues = append(r.pkgIssues, r.pkg.testDiffs...)
	for _, s := range r.pkg.floatingDiffs {
		r.pkgIssues = append(r.pkgIssues, "free-floating comment "+s)
	}
	for _, s := range r.pkg.moved {
		r.pkgIssues = append(r.pkgIssues, "moved within the package: "+s)
	}
	fieldMap := map[string]string{}
	for _, fs := range spec.fields {
		for f, n := range fs {
			fieldMap[f] = n
		}
	}
	bs, as := snapshot(b), snapshot(a)
	for _, k := range sortedKeys(bs) {
		if len(bs[k]) != 1 || len(as[k]) != 1 || bs[k][0].text == as[k][0].text {
			continue
		}
		if diff := tokenDiff(bs[k][0].text, as[k][0].text, fieldMap); diff != "" {
			r.pkgIssues = append(r.pkgIssues, fmt.Sprintf("%s (%s): %s", k, as[k][0].file, diff))
		}
	}
	for _, s := range r.pkg.textDiffs {
		var k string
		_, _ = fmt.Sscanf(s, "%s", &k)
		if len(bs[k]) != 1 || len(as[k]) != 1 {
			r.pkgIssues = append(r.pkgIssues, "text differs: "+s)
		}
	}

	// A fresh generator run must reproduce the alias file, the edited source
	// files and the target files exactly.
	for name, want := range ep.changed {
		got, err := os.ReadFile(filepath.Join(after.dir, name))
		switch {
		case want == nil && err == nil:
			r.regenDiffs = append(r.regenDiffs, name+": should have been deleted")
		case want != nil && err != nil:
			r.regenDiffs = append(r.regenDiffs, name+": "+err.Error())
		case want != nil && !bytes.Equal(got, want):
			r.regenDiffs = append(r.regenDiffs, name+": differs from the generator output")
		}
	}
	for name, want := range ep.targetSrc {
		got, err := os.ReadFile(filepath.Join(targetDir, name))
		if err != nil || !bytes.Equal(got, want) {
			r.regenDiffs = append(r.regenDiffs, filepath.Join(filepath.Base(targetDir), name)+": differs from the generator output")
		}
	}
	sort.Strings(r.regenDiffs)
	return r, nil
}

func renamed(m map[string]string, k string) string {
	if n, ok := m[k]; ok && n != "" {
		return n
	}
	return k
}

// filterPkg returns a view of p with only the declarations keep accepts and
// without the file named skip.
func filterPkg(p *pkgInfo, keep func(*declInfo) bool, skip string) *pkgInfo {
	q := &pkgInfo{dir: p.dir, fset: p.fset, byKey: map[string][]*declInfo{}, pkg: p.pkg}
	for _, f := range p.files {
		if f.name != skip {
			q.files = append(q.files, f)
		}
	}
	for _, d := range p.decls {
		if keep(d) {
			q.decls = append(q.decls, d)
			q.byKey[d.key] = append(q.byKey[d.key], d)
		}
	}
	return q
}

type tok struct {
	t   token.Token
	lit string
}

func scanTokens(src string) ([]tok, error) {
	fset := token.NewFileSet()
	f := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	var errs []string
	s.Init(f, []byte(src), func(_ token.Position, msg string) { errs = append(errs, msg) }, scanner.ScanComments)
	var out []tok
	for {
		_, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		if t == token.SEMICOLON && lit == "\n" {
			lit = ";" // automatic semicolon: same as an explicit one
		}
		out = append(out, tok{t, lit})
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("scan: %s", errs[0])
	}
	return out, nil
}

// tokenDiff returns "" when b and a have the same token stream, where an
// identifier may differ only as renames allows (old -> renames[old]).
func tokenDiff(b, a string, renames map[string]string) string {
	bt, err := scanTokens(b)
	if err != nil {
		return err.Error()
	}
	at, err := scanTokens(a)
	if err != nil {
		return err.Error()
	}
	if len(bt) != len(at) {
		return fmt.Sprintf("%d tokens before, %d after", len(bt), len(at))
	}
	for i := range bt {
		if bt[i] == at[i] {
			continue
		}
		if bt[i].t == token.IDENT && at[i].t == token.IDENT && renames[bt[i].lit] == at[i].lit {
			continue
		}
		return fmt.Sprintf("token %d: %s %q -> %s %q", i, bt[i].t, bt[i].lit, at[i].t, at[i].lit)
	}
	return ""
}

// commentOnlyDiff compares the .go files of two directories and returns one
// line per file whose Go tokens differ when comments are ignored, or that
// exists on only one side. An empty result means the change between the
// directories touches comments (and whitespace) only.
func commentOnlyDiff(before, after string) ([]string, error) {
	list := func(dir string) (map[string]bool, error) {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for _, n := range names {
			m[filepath.Base(n)] = true
		}
		return m, nil
	}
	b, err := list(before)
	if err != nil {
		return nil, err
	}
	a, err := list(after)
	if err != nil {
		return nil, err
	}
	var out []string
	for n := range a {
		if !b[n] {
			out = append(out, n+": only after")
		}
	}
	for _, n := range sortedKeys(b) {
		if !a[n] {
			out = append(out, n+": only before")
			continue
		}
		bs, err := os.ReadFile(filepath.Join(before, n))
		if err != nil {
			return nil, err
		}
		as, err := os.ReadFile(filepath.Join(after, n))
		if err != nil {
			return nil, err
		}
		bt, err := scanTokens(string(bs))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		at, err := scanTokens(string(as))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		bc, err := importsC(string(bs))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		ac, err := importsC(string(as))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		cgo := bc || ac
		bt, at = dropComments(bt, cgo), dropComments(at, cgo)
		if len(bt) != len(at) {
			out = append(out, fmt.Sprintf("%s: %d code tokens before, %d after", n, len(bt), len(at)))
			continue
		}
		for i := range bt {
			if bt[i] != at[i] {
				out = append(out, fmt.Sprintf("%s: code token %d: %s %q -> %s %q", n, i, bt[i].t, bt[i].lit, at[i].t, at[i].lit))
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// dropComments removes ordinary comment tokens. Comments that the toolchain
// reads stay in the stream, so a change to them is reported as a code
// change: build constraints (//go:build, // +build), other //go: directives
// (//go:embed, //go:linkname, //go:generate, ...), line directives
// (//line, /*line), and cgo's //export. With cgo set (the file imports
// "C" on either side) every comment stays, because the cgo preamble is a
// comment.
func dropComments(ts []tok, cgo bool) []tok {
	var out []tok
	for _, t := range ts {
		if t.t != token.COMMENT || cgo || isDirectiveComment(t.lit) {
			out = append(out, t)
		}
	}
	return out
}

// isDirectiveComment reports whether a comment is read by the Go toolchain.
func isDirectiveComment(c string) bool {
	for _, p := range []string{"//go:", "// +build", "//line ", "/*line ", "//export "} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// importsC reports whether a Go source file imports "C", in any import
// declaration form (single or grouped).
func importsC(src string) (bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return false, err
	}
	for _, s := range f.Imports {
		if s.Path.Value == `"C"` {
			return true, nil
		}
	}
	return false, nil
}
