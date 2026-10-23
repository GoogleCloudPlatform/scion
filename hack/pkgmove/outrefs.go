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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// refOKPrefix starts the comment that accepts one outside reference to a
// moved file, on the line above it:
//
//	// pkgmove:ref-ok <reason>
//	# pkgmove:ref-ok <reason>      (shell, Makefile, YAML)
//
// Like scan-covers, the marker must be a whole comment line, with one space
// after "//" (or any blanks after "#"), and the reason is required.
const refOKPrefix = "pkgmove:ref-ok"

// Forms of an outside reference, most specific first.
const (
	refRepoPath = "repo path"
	refElems    = "path elements"
	refBase     = "base name"
)

// movedRef is a moved file (or asset) as outside code may name it.
type movedRef struct {
	base  string   // file name, "x.go"
	repo  string   // module-relative path, "pkg/hub/x.go"
	elems []string // repo split at "/"
	dst   string   // module-relative path after the move
	dir   string   // last element of the source directory ("hub")
}

// outsideRef is one reference found by outsideRefFindings.
type outsideRef struct {
	pos    string // module-relative file:line
	moved  *movedRef
	form   string
	text   string // the matched literal or line, trimmed
	ok     string // ref-ok reason, "" when not accepted
	noWhy  string // file:line of a ref-ok marker without a reason
	isText bool   // matched by text (shell, Makefile, YAML, unparsable Go)
}

// outsideRefFindings reports references to the moved files from outside
// the move set. pkgmove rewrites Go references to moved symbols, but code
// that names a moved file by path or by name (a classification table keyed
// on "pkg/hub/x.go", a test keyed on "x.go", a shell guard exempting a path)
// silently stops matching after the move, or fails only later in CI.
//
// Scope: every .go file of the module tree except the moved ones (the
// source package's staying files included), hack/**/*.sh, the root Makefile
// and .github/workflows/*.yml. Directories named .design, docs-site,
// node_modules or vendor, and other dot directories, are skipped, and so are
// Markdown files; testdata directories are scanned (a fixture that names a
// moved path is reported). In Go files only string literals count (go/ast BasicLit),
// so comments do not: a literal that contains a moved file's repo path, a
// call whose consecutive string arguments are the trailing elements of that
// path (filepath.Join("pkg", "hub", "x.go"); at least the directory and the
// file name), or a literal that contains the file name as a whole path
// element, alone or after the source directory's name ("x.go", "x.go:fn",
// "../hub/x.go"; not "src/x.go"). Other files match by text, the
// same forms on any line.
//
// Each reference is HIGH (an error under -strict) unless the line above it
// is a "pkgmove:ref-ok <reason>" comment, which lists it as INFO.
func (a *analysis) outsideRefFindings() {
	var moved []*movedRef
	skip := map[string]bool{}
	dstRel := a.rel(a.cfg.DstDir)
	add := func(name string) {
		repo := a.rel(filepath.Join(a.cfg.SrcDir, name))
		skip[repo] = true
		moved = append(moved, &movedRef{base: name, repo: repo, elems: strings.Split(repo, "/"), dst: path.Join(dstRel, name), dir: filepath.Base(a.cfg.SrcDir)})
	}
	for _, f := range a.files {
		if f.Moved {
			add(f.Name)
		}
	}
	for _, name := range a.assets {
		// A moved directory (testdata) is named everywhere for other
		// reasons; only file assets are looked up.
		if info, err := os.Stat(filepath.Join(a.cfg.SrcDir, name)); err == nil && !info.IsDir() {
			add(name)
		}
	}
	if len(moved) == 0 {
		return
	}
	sort.Slice(moved, func(i, j int) bool { return moved[i].repo < moved[j].repo })

	var refs []outsideRef
	root := a.mod.ModDir
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped
		}
		rel := a.rel(p)
		if d.IsDir() {
			name := d.Name()
			if p != root && (name == "node_modules" || name == "vendor" || name == "docs-site" ||
				(strings.HasPrefix(name, ".") && rel != ".github")) {
				return filepath.SkipDir
			}
			return nil
		}
		if skip[rel] {
			return nil
		}
		var text bool
		switch {
		case strings.HasSuffix(rel, ".go"):
		case strings.HasPrefix(rel, "hack/") && strings.HasSuffix(rel, ".sh"),
			rel == "Makefile",
			path.Dir(rel) == ".github/workflows" && (strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")):
			text = true
		default:
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var cand []*movedRef
		for _, m := range moved {
			if bytes.Contains(src, []byte(m.base)) {
				cand = append(cand, m)
			}
		}
		if len(cand) == 0 {
			return nil
		}
		if !text {
			if found, ok := a.goOutsideRefs(rel, src, cand); ok {
				refs = append(refs, found...)
				return nil
			}
		}
		refs = append(refs, textOutsideRefs(rel, src, cand, strings.HasSuffix(rel, ".go"))...)
		return nil
	})

	for _, r := range refs {
		where := "update it to " + r.moved.dst
		switch r.form {
		case refBase:
			where = "check what it matches after the move (a key, an exemption or a non-recursive glob may need " + r.moved.dst + ")"
		case refElems:
			where = "update the elements to " + r.moved.dst
		}
		how := "Go string literal"
		switch {
		case r.isText:
			how = "line"
		case r.form == refElems:
			how = "Go string literals"
		}
		if r.ok != "" {
			a.plan.add(levelInfo, "outside reference to a moved file accepted (pkgmove:ref-ok)", r.pos,
				"%s %s names moved %s by its %s; ref-ok: %q", how, r.text, r.moved.repo, r.form, r.ok)
			continue
		}
		note := ""
		if r.noWhy != "" {
			note = " (the pkgmove:ref-ok marker at " + r.noWhy + " has no reason, so it accepts nothing)"
		}
		a.plan.add(levelHigh, "outside reference to a moved file", r.pos,
			"%s %s names moved %s by its %s: after the move it no longer matches the file - %s, or accept it with a %s comment on the line above%s",
			how, r.text, r.moved.repo, r.form, where, refOKPrefix+" <reason>", note)
	}
}

// goOutsideRefs finds the references in the string literals of a Go file.
// ok is false when the file does not parse (the caller scans it as text).
func (a *analysis) goOutsideRefs(rel string, src []byte, cand []*movedRef) (refs []outsideRef, ok bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	// ref-ok markers by line.
	markers := map[int]string{} // line -> reason ("" when missing)
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if reason, isMarker := refOKComment(c.Text); isMarker && startsLine(src, fset.Position(c.Pos()).Offset) && strings.HasPrefix(c.Text, "// ") {
				markers[fset.Position(c.Pos()).Line] = reason
			}
		}
	}
	type hit struct {
		line int
		m    *movedRef
		form string
		text string
	}
	var hits []hit
	consumed := map[*ast.BasicLit]bool{}
	strLit := func(e ast.Expr) (*ast.BasicLit, string, bool) {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return nil, "", false
		}
		v, err := strconv.Unquote(bl.Value)
		return bl, v, err == nil
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for i, arg := range call.Args {
			_, v, ok := strLit(arg)
			if !ok {
				continue
			}
			for _, m := range cand {
				if v != m.base {
					continue
				}
				// Walk back over the consecutive string arguments that
				// match the preceding path elements.
				k := 1
				for i-k >= 0 && len(m.elems)-1-k >= 0 {
					_, pv, ok := strLit(call.Args[i-k])
					if !ok || pv != m.elems[len(m.elems)-1-k] {
						break
					}
					k++
				}
				if k < 2 {
					continue
				}
				var parts []string
				for j := i - k + 1; j <= i; j++ {
					consumed[call.Args[j].(*ast.BasicLit)] = true
					parts = append(parts, call.Args[j].(*ast.BasicLit).Value)
				}
				hits = append(hits, hit{fset.Position(call.Args[i-k+1].Pos()).Line, m, refElems, strings.Join(parts, ", ")})
			}
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || consumed[bl] {
			return true
		}
		_, v, ok := strLit(bl)
		if !ok {
			return true
		}
		for _, m := range cand {
			form := ""
			switch {
			case containsPathElem(v, m.repo):
				form = refRepoPath
			case containsBaseRef(v, m.base, m.dir):
				form = refBase
			default:
				continue
			}
			hits = append(hits, hit{fset.Position(bl.Pos()).Line, m, form, bl.Value})
		}
		return true
	})
	for _, h := range hits {
		r := outsideRef{pos: rel + ":" + strconv.Itoa(h.line), moved: h.m, form: h.form, text: shortenRef(h.text)}
		if reason, has := markers[h.line-1]; has {
			if reason != "" {
				r.ok = reason
			} else {
				r.noWhy = rel + ":" + strconv.Itoa(h.line-1)
			}
		}
		refs = append(refs, r)
	}
	return refs, true
}

// textOutsideRefs finds the references on the lines of a non-Go file (or a
// Go file that does not parse; goComments selects "//" markers).
func textOutsideRefs(rel string, src []byte, cand []*movedRef, goComments bool) []outsideRef {
	var refs []outsideRef
	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		for _, m := range cand {
			form := ""
			switch {
			case containsPathElem(line, m.repo):
				form = refRepoPath
			case containsBaseRef(line, m.base, m.dir):
				form = refBase
			default:
				continue
			}
			r := outsideRef{pos: rel + ":" + strconv.Itoa(i+1), moved: m, form: form, text: strconv.Quote(shortenRef(strings.TrimSpace(line))), isText: true}
			if i > 0 {
				if reason, has := textRefOK(lines[i-1], goComments); has {
					if reason != "" {
						r.ok = reason
					} else {
						r.noWhy = rel + ":" + strconv.Itoa(i)
					}
				}
			}
			refs = append(refs, r)
		}
	}
	return refs
}

// refOKComment parses a "// pkgmove:ref-ok <reason>" comment.
func refOKComment(text string) (reason string, ok bool) {
	rest, found := strings.CutPrefix(text, "// "+refOKPrefix)
	if !found || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

var hashRefOK = regexp.MustCompile(`^\s*#\s*` + regexp.QuoteMeta(refOKPrefix) + `(\s+(.*))?$`)

// textRefOK parses a ref-ok marker line of a text file: "# pkgmove:ref-ok
// <reason>", or the "//" form for Go files scanned as text.
func textRefOK(line string, goComments bool) (reason string, ok bool) {
	line = strings.TrimRight(line, "\r")
	if goComments {
		return refOKComment(strings.TrimLeft(line, " \t"))
	}
	m := hashRefOK.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[2]), true
}

// containsPathElem reports whether s contains p starting at a path element
// boundary (start, or after a byte that cannot be part of a file name) and
// not followed by an identifier byte, so "x.go" matches "x.go", "hub/x.go"
// and "x.go:fn" but not "fix.go" or "x.gox".
func containsPathElem(s, p string) bool {
	for i := 0; i+len(p) <= len(s); {
		j := strings.Index(s[i:], p)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(p)
		if (j == 0 || !isFileNameByte(s[j-1])) && (end == len(s) || !isIdentByte(s[end])) {
			return true
		}
		i = j + 1
	}
	return false
}

// containsBaseRef reports whether s names base as a file of the source
// directory: base at a path element boundary (see containsPathElem) that is
// either not preceded by a directory ("x.go", "x.go:fn") or preceded by one
// whose last element is dir ("../hub/x.go"). A different directory
// ("src/x.go", "$fixture_dir/x.go") names another file of the same name.
func containsBaseRef(s, base, dir string) bool {
	for i := 0; i+len(base) <= len(s); {
		j := strings.Index(s[i:], base)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(base)
		if (j == 0 || !isFileNameByte(s[j-1])) && (end == len(s) || !isIdentByte(s[end])) {
			if j == 0 || s[j-1] != '/' {
				return true
			}
			k := j - 1
			for k > 0 && s[k-1] != '/' && isFileNameByte(s[k-1]) {
				k--
			}
			if s[k:j-1] == dir {
				return true
			}
		}
		i = j + 1
	}
	return false
}

func isFileNameByte(c byte) bool {
	return isIdentByte(c) || c == '-' || c == '.'
}

// shortenRef caps a reported literal or line at 120 bytes.
func shortenRef(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:117] + "..."
}
