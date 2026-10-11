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
	"io"
	"sort"
	"strings"
)

// verifyReport compares every top-level declaration of two package
// directories (before and after a move).
type verifyReport struct {
	declsBefore, declsAfter int
	moved                   []string // "key: from -> to"
	textDiffs               []string
	missing, added          []string
	constraintChanges       []string // moved decls whose file constraint changed
	badConstraintChanges    []string // narrowing or a change other than to ""
	testsBefore, testsAfter int
	testDiffs               []string // test funcs added, removed or re-constrained
	floatingBefore          int      // comments outside declarations, after the package clause
	floatingDiffs           []string // floating comments lost or gained
}

func (r *verifyReport) ok() bool {
	return len(r.textDiffs) == 0 && len(r.missing) == 0 && len(r.added) == 0 &&
		len(r.badConstraintChanges) == 0 && len(r.testDiffs) == 0 && len(r.floatingDiffs) == 0
}

type declSnap struct {
	text, file, constraint string
}

func snapshot(p *pkgInfo) map[string][]declSnap {
	// A GenDecl declaring several names is one node: key it by all of them.
	// Same-named declarations in mutually exclusive files share a key and
	// are told apart by their file's build constraint when matched.
	names := map[ast.Decl][]string{}
	var nodes []*declInfo
	for _, d := range p.decls {
		if _, ok := names[d.node]; !ok {
			nodes = append(nodes, d)
		}
		names[d.node] = append(names[d.node], d.key)
	}
	out := map[string][]declSnap{}
	for _, d := range nodes {
		k := strings.Join(names[d.node], ",")
		out[k] = append(out[k], declSnap{text: d.text(p.fset), file: d.file.name, constraint: d.file.constraint})
	}
	return out
}

// isTestFunc reports whether a declaration key names a func that go test
// would list (Test, Benchmark, Fuzz, Example prefixes; signature not
// checked, since it is compared verbatim anyway).
func isTestFunc(k string) bool {
	if strings.Contains(k, ".") || strings.Contains(k, ",") {
		return false
	}
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// floatingComments returns the multiset of comment groups that sit after the
// package clause but outside every declaration (e.g. section banners).
func floatingComments(p *pkgInfo) map[string]int {
	out := map[string]int{}
	for _, f := range p.files {
		pkgOff := p.fset.Position(f.ast.Package).Offset
		var spans [][2]int
		for _, d := range f.ast.Decls {
			s, e := declSpan(p.fset, f, d)
			spans = append(spans, [2]int{s, e})
		}
		for _, cg := range f.ast.Comments {
			cs := p.fset.Position(cg.Pos()).Offset
			if cs < pkgOff {
				continue
			}
			in := false
			for _, sp := range spans {
				if cs >= sp[0] && cs < sp[1] {
					in = true
					break
				}
			}
			if !in {
				out[string(f.src[cs:p.fset.Position(cg.End()).Offset])]++
			}
		}
	}
	return out
}

func verify(before, after *pkgInfo) *verifyReport {
	b, a := snapshot(before), snapshot(after)
	r := &verifyReport{}
	fb, fa := floatingComments(before), floatingComments(after)
	for t, n := range fb {
		r.floatingBefore += n
		if fa[t] != n {
			r.floatingDiffs = append(r.floatingDiffs, fmt.Sprintf("%dx -> %dx: %.60q", n, fa[t], t))
		}
	}
	for t, n := range fa {
		if _, ok := fb[t]; !ok {
			r.floatingDiffs = append(r.floatingDiffs, fmt.Sprintf("0x -> %dx: %.60q", n, t))
		}
	}
	sort.Strings(r.floatingDiffs)
	for _, v := range b {
		r.declsBefore += len(v)
	}
	for _, v := range a {
		r.declsAfter += len(v)
	}
	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	ks := make([]string, 0, len(keys))
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		bs, as := b[k], a[k]
		if isTestFunc(k) {
			r.testsBefore += len(bs)
			r.testsAfter += len(as)
		}
		// Match by constraint first, then by text.
		used := make([]bool, len(as))
		for _, bd := range bs {
			match := -1
			for i, ad := range as {
				if !used[i] && ad.text == bd.text && (ad.constraint == bd.constraint || len(bs) == 1) {
					match = i
					break
				}
			}
			if match < 0 {
				// Same key but different text?
				for i, ad := range as {
					if !used[i] && (ad.constraint == bd.constraint || len(bs) == 1) {
						match = i
						r.textDiffs = append(r.textDiffs, fmt.Sprintf("%s (%s -> %s)", k, bd.file, ad.file))
						break
					}
				}
			}
			if match < 0 {
				r.missing = append(r.missing, fmt.Sprintf("%s (%s)", k, bd.file))
				if isTestFunc(k) {
					r.testDiffs = append(r.testDiffs, "removed "+k)
				}
				continue
			}
			used[match] = true
			ad := as[match]
			if ad.file != bd.file {
				r.moved = append(r.moved, fmt.Sprintf("%s: %s -> %s", k, bd.file, ad.file))
			}
			if ad.constraint != bd.constraint {
				c := fmt.Sprintf("%s: %q -> %q", k, bd.constraint, ad.constraint)
				r.constraintChanges = append(r.constraintChanges, c)
				if ad.constraint != "" {
					r.badConstraintChanges = append(r.badConstraintChanges, c)
				}
				if isTestFunc(k) {
					r.testDiffs = append(r.testDiffs, "re-constrained "+c)
				}
			}
		}
		for i, ad := range as {
			if !used[i] {
				r.added = append(r.added, fmt.Sprintf("%s (%s)", k, ad.file))
				if isTestFunc(k) {
					r.testDiffs = append(r.testDiffs, "added "+k)
				}
			}
		}
	}
	return r
}

func (r *verifyReport) write(w io.Writer) {
	_, _ = fmt.Fprintf(w, "top-level declarations: before=%d after=%d\n", r.declsBefore, r.declsAfter)
	_, _ = fmt.Fprintf(w, "moved: %d (byte-identical text required)\n", len(r.moved))
	for _, m := range r.moved {
		_, _ = fmt.Fprintf(w, "  %s\n", m)
	}
	_, _ = fmt.Fprintf(w, "text diffs: %d\n", len(r.textDiffs))
	for _, m := range r.textDiffs {
		_, _ = fmt.Fprintf(w, "  %s\n", m)
	}
	_, _ = fmt.Fprintf(w, "missing: %d, added: %d\n", len(r.missing), len(r.added))
	for _, m := range r.missing {
		_, _ = fmt.Fprintf(w, "  missing %s\n", m)
	}
	for _, m := range r.added {
		_, _ = fmt.Fprintf(w, "  added %s\n", m)
	}
	_, _ = fmt.Fprintf(w, "constraint changes (widening to unconstrained only): %d\n", len(r.constraintChanges))
	for _, m := range r.constraintChanges {
		_, _ = fmt.Fprintf(w, "  %s\n", m)
	}
	_, _ = fmt.Fprintf(w, "disallowed constraint changes: %d\n", len(r.badConstraintChanges))
	_, _ = fmt.Fprintf(w, "Test/Benchmark/Fuzz/Example funcs: before=%d after=%d, differences=%d\n", r.testsBefore, r.testsAfter, len(r.testDiffs))
	for _, m := range r.testDiffs {
		_, _ = fmt.Fprintf(w, "  %s\n", m)
	}
	_, _ = fmt.Fprintf(w, "free-floating comments: before=%d, differences=%d\n", r.floatingBefore, len(r.floatingDiffs))
	for _, m := range r.floatingDiffs {
		_, _ = fmt.Fprintf(w, "  %s\n", m)
	}
	if r.ok() {
		_, _ = fmt.Fprintln(w, "RESULT: OK")
	} else {
		_, _ = fmt.Fprintln(w, "RESULT: FAIL")
	}
}
