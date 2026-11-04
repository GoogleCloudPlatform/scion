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
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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
	isText bool   // matched by text (shell, Makefile, YAML, hack/**/*.txt, unparsable Go)

	// For -rewrite-outrefs: the file, and the byte range and value of the
	// matched Go string literal (quote is its opening quote) or text line
	// (line ending excluded). Unset for path-element references.
	path  string
	src   []byte
	start int
	end   int
	value string
	quote byte
	// fn is the sibling Function field when the literal is the File field of
	// a keyed composite literal (the authzop catalog shape), else nil.
	fn *fnField
}

// fnField is the Function field next to a File field in a composite
// literal. lit is false when its value is not a string literal.
type fnField struct {
	lit        bool
	line       int
	start, end int
	value      string
	quote      byte
	text       string // the value's source text, shortened
}

// outFileEdits are the -rewrite-outrefs edits of a file outside the source
// package.
type outFileEdits struct {
	src   []byte
	edits []edit
}

// outsideRefFindings reports references to the moved files from outside
// the move set. pkgmove rewrites Go references to moved symbols, but code
// that names a moved file by path or by name (a classification table keyed
// on "pkg/hub/x.go", a test keyed on "x.go", a shell guard exempting a path)
// silently stops matching after the move, or fails only later in CI.
//
// Scope: every .go file of the module tree except the moved ones (the
// source package's staying files included), hack/**/*.sh, hack/**/*.txt,
// the root Makefile and .github/workflows/*.yml. Directories named .design, docs-site,
// node_modules or vendor, and other dot directories, are skipped, and so are
// Markdown files; testdata directories are scanned (a fixture that names a
// moved path is reported), except hack/**/*.txt under testdata (golden
// text, which cannot carry a ref-ok marker). In Go files only string literals count (go/ast BasicLit),
// so comments do not: a literal that contains a moved file's repo path, a
// call whose consecutive string arguments are the trailing elements of that
// path (filepath.Join("pkg", "hub", "x.go"); at least the directory and the
// file name), or a literal that contains the file name as a whole path
// element, alone or after the source directory's name ("x.go", "x.go:fn",
// "../hub/x.go"; not "src/x.go"). Other files match by text, the
// same forms on any line.
//
// Each reference is HIGH (an error under -strict) unless the line above it
// is a "pkgmove:ref-ok <reason>" comment, which lists it as INFO. With
// -rewrite-outrefs, the exact repo-path forms are rewritten instead (see
// outRefRewrite) and listed as INFO.
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
		case strings.HasPrefix(rel, "hack/") && strings.HasSuffix(rel, ".txt") && strings.Contains("/"+rel, "/testdata/"):
			// Fixture and golden text (pkgmove's own want/ trees) cannot carry a
			// ref-ok marker without changing what it fixes; Go testdata stays in scope.
			return nil
		case strings.HasPrefix(rel, "hack/") && (strings.HasSuffix(rel, ".sh") || strings.HasSuffix(rel, ".txt")),
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
		found, ok := []outsideRef(nil), false
		if !text {
			found, ok = a.goOutsideRefs(rel, src, cand)
		}
		if !ok {
			found = textOutsideRefs(rel, src, cand, strings.HasSuffix(rel, ".go"))
		}
		for i := range found {
			found[i].path, found[i].src = p, src
		}
		refs = append(refs, found...)
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
		if a.cfg.RewriteOutRefs {
			text, shown, why := a.outRefRewrite(r)
			if text != "" {
				a.addOutRefEdit(r, text)
				a.plan.OutRefRewrites = append(a.plan.OutRefRewrites, varRewrite{Pos: r.pos, Old: r.text, New: shown})
				a.plan.add(levelInfo, "outside reference to a moved file rewritten (-rewrite-outrefs)", r.pos,
					"%s %s names moved %s by its %s; rewritten to %s", how, r.text, r.moved.repo, r.form, shown)
				if r.fn != nil {
					a.rewriteFunctionField(r)
				}
				continue
			}
			note = why
		}
		if r.noWhy != "" {
			note += " (the pkgmove:ref-ok marker at " + r.noWhy + " has no reason, so it accepts nothing)"
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
		lit  *ast.BasicLit // the single literal (nil for path elements)
		val  string        // its value
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
				hits = append(hits, hit{fset.Position(call.Args[i-k+1].Pos()).Line, m, refElems, strings.Join(parts, ", "), nil, ""})
			}
		}
		return true
	})
	// File fields of keyed composite literals and their Function siblings.
	fnOf := map[*ast.BasicLit]*fnField{}
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		var fileLit *ast.BasicLit
		var fnExpr ast.Expr
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "File":
				fileLit, _ = kv.Value.(*ast.BasicLit)
			case "Function":
				fnExpr = kv.Value
			}
		}
		if fileLit == nil || fnExpr == nil {
			return true
		}
		start, end := fset.Position(fnExpr.Pos()).Offset, fset.Position(fnExpr.End()).Offset
		fn := &fnField{line: fset.Position(fnExpr.Pos()).Line, start: start, end: end, text: shortenRef(string(src[start:end]))}
		if bl, v, ok := strLit(fnExpr); ok {
			fn.lit, fn.value, fn.quote = true, v, bl.Value[0]
		}
		fnOf[fileLit] = fn
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
			hits = append(hits, hit{fset.Position(bl.Pos()).Line, m, form, bl.Value, bl, v})
		}
		return true
	})
	for _, h := range hits {
		r := outsideRef{pos: rel + ":" + strconv.Itoa(h.line), moved: h.m, form: h.form, text: shortenRef(h.text)}
		if h.lit != nil {
			r.start, r.end = fset.Position(h.lit.Pos()).Offset, fset.Position(h.lit.End()).Offset
			r.value, r.quote = h.val, h.lit.Value[0]
			r.fn = fnOf[h.lit]
		}
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
	off := 0
	for i, line := range lines {
		start := off
		off += len(line) + 1
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
			r.value = strings.TrimSuffix(line, "\r")
			r.start, r.end = start, start+len(r.value)
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

// outRefRewrite returns the -rewrite-outrefs replacement of an outside
// reference: text replaces the bytes r.start:r.end, and shown is the new
// literal or line for the report. Only the exact repo-path forms qualify:
//
//   - a Go string literal equal to the moved file's repo path,
//     "pkg/hub/x.go", or to the path, a colon and an identifier,
//     "pkg/hub/x.go:name";
//   - a line of hack/**/*.txt that starts with the repo path (optionally
//     followed by a colon and an identifier) and continues with a blank or
//     ends there ("pkg/hub/x.go | 3", "pkg/hub/x.go unprotected").
//
// The path becomes the moved file's new path, and the identifier its new
// name when the move renames it (an export), so the reference names the same
// file and symbol as before. Anything else (base names, path elements, a
// path inside a longer literal or line, shell, Makefile and workflow lines)
// returns text "" and stays report-only; why explains a refusal that would
// otherwise look like a rewritable form.
func (a *analysis) outRefRewrite(r outsideRef) (text, shown, why string) {
	m := r.moved
	if r.form != refRepoPath || r.src == nil || r.end <= r.start {
		return "", "", ""
	}
	rel := a.rel(r.path)
	switch {
	case !r.isText:
		v := r.value
		if v != m.repo {
			name, ok := strings.CutPrefix(v, m.repo+":")
			if !ok || !token.IsIdentifier(name) {
				return "", "", ""
			}
			n, why := a.outRefName(name, m)
			if why != "" {
				return "", "", why
			}
			v = m.dst + ":" + n
		} else {
			v = m.dst
		}
		if r.quote == '`' {
			return "`" + v + "`", "`" + v + "`", ""
		}
		q := strconv.Quote(v)
		return q, q, ""
	case strings.HasPrefix(rel, "hack/") && strings.HasSuffix(rel, ".txt"):
		rest, ok := strings.CutPrefix(r.value, m.repo)
		if !ok {
			return "", "", ""
		}
		head := m.dst
		if after, isSym := strings.CutPrefix(rest, ":"); isSym {
			k := 0
			for k < len(after) && isIdentByte(after[k]) {
				k++
			}
			if !token.IsIdentifier(after[:k]) {
				return "", "", ""
			}
			n, why := a.outRefName(after[:k], m)
			if why != "" {
				return "", "", why
			}
			head += ":" + n
			rest = after[k:]
		}
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			return "", "", ""
		}
		if containsPathElem(rest, m.repo) {
			return "", "", " (not rewritten: the line names the path more than once)"
		}
		// A column naming a function (store-delete-callers-allowlist.txt,
		// time-literals-allowlist.txt: "<path> | <function> | ...") would go
		// stale if the move renames it.
		for _, tok := range strings.FieldsFunc(rest, func(c rune) bool { return c >= utf8.RuneSelf || !isIdentByte(byte(c)) }) {
			if news := a.renamesOf(tok); len(news) > 0 {
				return "", "", fmt.Sprintf(" (not rewritten: the line also names %s, which the move renames to %s)", tok, strings.Join(news, " or "))
			}
		}
		line := head + rest
		return line, strconv.Quote(shortenRef(strings.TrimSpace(line))), ""
	}
	return "", "", ""
}

// rewriteFunctionField handles the Function field next to a File field that
// -rewrite-outrefs rewrote (the authzop catalog shape, keyed by file and
// enclosing function). The value is rewritten only when it is a string
// literal naming a function or method declared in the moved file that the
// move renames, to one new name. A name the move does not rename there stays
// as it is. Anything else that could leave the entry stale is a HIGH: a
// value that is not a string literal, a name renamed only in another file,
// an ambiguous name, or a moved file that was not type-checked.
func (a *analysis) rewriteFunctionField(r outsideRef) {
	fn := r.fn
	pos := a.rel(r.path) + ":" + strconv.Itoa(fn.line)
	high := func(why string) {
		a.plan.add(levelHigh, "Function field next to a rewritten File field not rewritten (-rewrite-outrefs)", pos,
			"Function %s of the entry whose File names moved %s: %s; check it by hand", fn.text, r.moved.repo, why)
	}
	if !fn.lit {
		high("the value is not a string literal")
		return
	}
	if !token.IsIdentifier(fn.value) {
		return // not a function name; nothing the move renames
	}
	f := a.byPath[filepath.Join(a.cfg.SrcDir, r.moved.base)]
	if f == nil || !f.Moved {
		return // a moved asset, not a Go file
	}
	news := map[string]bool{}
	unknown := false
	for _, d := range f.AST.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn.value {
			continue
		}
		obj := a.info.Defs[fd.Name]
		if obj == nil {
			unknown = true
			continue
		}
		n, renamed := a.pkgRename[obj]
		if !renamed {
			n, renamed = a.memberRename[obj]
		}
		if !renamed {
			n = fn.value
		}
		news[n] = true
	}
	if unknown {
		high(fmt.Sprintf("%s in %s was not type-checked (build tags or an external test), so its rename is unknown", fn.value, r.moved.repo))
		return
	}
	if len(news) == 0 {
		if where := a.renamedFiles(fn.value); len(where) > 0 {
			high(fmt.Sprintf("%s is not declared in %s; the move renames it in %s", fn.value, r.moved.repo, strings.Join(where, ", ")))
		}
		return
	}
	if len(news) > 1 {
		var all []string
		for n := range news {
			all = append(all, n)
		}
		sort.Strings(all)
		high(fmt.Sprintf("%s is ambiguous after the move, it becomes %s", fn.value, strings.Join(all, " or ")))
		return
	}
	var n string
	for k := range news {
		n = k
	}
	if n == fn.value {
		return // not renamed
	}
	text := strconv.Quote(n)
	if fn.quote == '`' {
		text = "`" + n + "`"
	}
	a.addOutRefEdit(outsideRef{path: r.path, src: r.src, start: fn.start, end: fn.end}, text)
	a.plan.OutRefRewrites = append(a.plan.OutRefRewrites, varRewrite{Pos: pos, Old: fn.text, New: text})
	a.plan.add(levelInfo, "outside reference to a moved file rewritten (-rewrite-outrefs)", pos,
		"Function %s of the entry whose File names moved %s; rewritten to %s (renamed by the move)", fn.text, r.moved.repo, text)
}

// outRefName returns the name an identifier of a "path:name" reference has
// after the move. Only the objects of that name declared in, or used from,
// the named moved file count (type information of the checked unit): their
// single new name, or the name itself when none of them is renamed. why is
// set, and the reference is not rewritten, when the name is ambiguous there
// (renamed for one object and kept, or renamed differently, for another),
// when the move renames the name only in other files, or when the named file
// was not type-checked (build tags, external test) and the move renames the
// name somewhere.
func (a *analysis) outRefName(name string, m *movedRef) (string, string) {
	renamedAt := a.renamedFiles(name)
	f := a.byPath[filepath.Join(a.cfg.SrcDir, m.base)]
	checked := false
	for _, c := range a.checked {
		if c == f {
			checked = true
			break
		}
	}
	if f == nil || !checked {
		if len(renamedAt) > 0 {
			return "", fmt.Sprintf(" (not rewritten: %s was not type-checked, and the move renames %s in %s)", m.repo, name, strings.Join(renamedAt, ", "))
		}
		return name, ""
	}
	news := map[string]bool{}
	for _, u := range a.uses {
		if u.file != f || u.obj == nil || u.id.Name != name {
			continue
		}
		n, renamed := a.pkgRename[u.obj]
		if !renamed {
			n, renamed = a.memberRename[u.obj]
		}
		if !renamed {
			n = name
		}
		news[n] = true
	}
	if len(news) == 0 {
		if len(renamedAt) > 0 {
			return "", fmt.Sprintf(" (not rewritten: %s is renamed in %s, not in %s)", name, strings.Join(renamedAt, ", "), m.repo)
		}
		return name, ""
	}
	if len(news) > 1 {
		var all []string
		for n := range news {
			all = append(all, n)
		}
		sort.Strings(all)
		return "", fmt.Sprintf(" (not rewritten: %s is ambiguous after the move, it becomes %s)", name, strings.Join(all, " or "))
	}
	for n := range news {
		return n, ""
	}
	return name, ""
}

// renamesOf returns the sorted new names the move gives objects named name
// (package-level objects and members, in any file).
func (a *analysis) renamesOf(name string) []string {
	set := map[string]bool{}
	for _, ren := range []map[types.Object]string{a.pkgRename, a.memberRename} {
		for obj, n := range ren {
			if obj.Name() == name && n != name {
				set[n] = true
			}
		}
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// renamedFiles returns the sorted module-relative files declaring an object
// named name that the move renames.
func (a *analysis) renamedFiles(name string) []string {
	var where []string
	for _, ren := range []map[types.Object]string{a.pkgRename, a.memberRename} {
		for obj, n := range ren {
			if obj.Name() == name && n != name {
				if hf := a.home(obj); hf != nil {
					where = append(where, a.rel(hf.Path))
				}
			}
		}
	}
	sort.Strings(where)
	return dedupStrings(where)
}

// addOutRefEdit records a -rewrite-outrefs edit: through the file edits for
// a staying file of the source package (which may have other edits), else in
// outEdits.
func (a *analysis) addOutRefEdit(r outsideRef, text string) {
	e := edit{r.start, r.end, text}
	if f := a.byPath[r.path]; f != nil {
		fe := a.editsFor(f)
		fe.edits = append(fe.edits, e)
		return
	}
	if a.outEdits == nil {
		a.outEdits = map[string]*outFileEdits{}
	}
	oe := a.outEdits[r.path]
	if oe == nil {
		oe = &outFileEdits{src: r.src}
		a.outEdits[r.path] = oe
	}
	oe.edits = append(oe.edits, e)
}

// applyOutEdits returns the content of an outside file after its
// -rewrite-outrefs edits; Go files are gofmt-ed (a longer literal can move
// the alignment of a table).
func applyOutEdits(path string, oe *outFileEdits) ([]byte, error) {
	if strings.HasSuffix(path, ".go") {
		return applyEdits(&srcFile{Path: path, Src: oe.src}, &fileEdits{edits: oe.edits})
	}
	edits := append([]edit(nil), oe.edits...)
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out []byte
	pos := 0
	for _, e := range edits {
		if e.start < pos {
			return nil, fmt.Errorf("%s: overlapping edits at offset %d", path, e.start)
		}
		out = append(out, oe.src[pos:e.start]...)
		out = append(out, e.text...)
		pos = e.end
	}
	return append(out, oe.src[pos:]...), nil
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
	return truncateUTF8(s, 120)
}

// truncateUTF8 returns s if it is at most limit bytes long, and otherwise
// its longest prefix that ends on a rune boundary and leaves room for "...",
// followed by "...". It never splits a multi-byte UTF-8 sequence, and the
// result is at most limit bytes when limit is at least 3 (below that it is
// "..." alone).
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len("...")
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
