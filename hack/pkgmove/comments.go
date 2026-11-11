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
	"go/token"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rewriteComments renames identifiers inside comments so that the comments
// keep matching the code after the move (see "Comments" in the README):
//
//   - the doc comment of a renamed declaration whose first word is the old
//     name gets the new name (the go/doc convention "// writeError writes
//     ..." above what is now WriteError);
//   - in moved files, every comment gets whole-word occurrences of renamed
//     identifiers (package-level and member renames) rewritten;
//   - in staying files, only the doc comments of renamed declarations (the
//     members that a rename group exports) are rewritten, with the member
//     renames (package-level names keep their old name there, as aliases).
//
// A name that is a single lower-case word (run, start, handler) is rewritten
// only where it is spelled like code: the leading word of its own doc
// comment, or next to '.', '(', '[', ']' or a backquote. Prose such as "run
// the tests" is left alone. Directives (//go:..., //line, //export) are
// never touched, and nor are string literals (they are not comments).
func (a *analysis) rewriteComments() {
	pkgNames := map[string]string{}
	memberNames := map[string]string{}
	conflict := map[string]bool{}
	put := func(m map[string]string, old, n string) {
		if n == "" || old == n {
			return
		}
		if prev, ok := m[old]; ok && prev != n {
			conflict[old] = true
		}
		m[old] = n
	}
	for obj, n := range a.pkgRename {
		put(pkgNames, obj.Name(), n)
	}
	for obj, n := range a.memberRename {
		put(memberNames, obj.Name(), n)
	}
	for obj, n := range a.embedFollow {
		put(memberNames, obj.Name(), n)
	}
	movedNames := map[string]string{}
	for _, m := range []map[string]string{pkgNames, memberNames} {
		for old, n := range m {
			put(movedNames, old, n)
		}
	}
	// A name renamed to two different names (only possible across kinds)
	// is ambiguous in prose: leave it alone.
	for old := range conflict {
		delete(movedNames, old)
		delete(memberNames, old)
	}
	if len(movedNames) == 0 {
		return
	}
	for _, f := range a.files {
		switch {
		case f.Moved:
			lead := a.renamedDocs(f, true)
			for _, cg := range f.AST.Comments {
				for _, c := range cg.List {
					a.rewriteComment(f, c, movedNames, lead[c])
				}
			}
		case f.Included && !f.XTest && len(memberNames) > 0:
			lead := a.renamedDocs(f, false)
			var docs []*ast.CommentGroup
			seen := map[*ast.CommentGroup]bool{}
			for _, rd := range a.renamedDocList(f, false) {
				if !seen[rd.doc] {
					seen[rd.doc] = true
					docs = append(docs, rd.doc)
				}
			}
			sort.Slice(docs, func(i, j int) bool { return docs[i].Pos() < docs[j].Pos() })
			for _, cg := range docs {
				for _, c := range cg.List {
					a.rewriteComment(f, c, memberNames, lead[c])
				}
			}
		}
	}
}

// renamedDoc is a doc comment of a renamed declaration.
type renamedDoc struct {
	doc      *ast.CommentGroup
	old, new string
}

// renamedDocList lists the doc comments of the declarations of f that are
// renamed (package-level ones only when pkgLevel is set: in moved files).
func (a *analysis) renamedDocList(f *srcFile, pkgLevel bool) []renamedDoc {
	var out []renamedDoc
	newOf := func(id *ast.Ident) string {
		obj := a.info.Defs[id]
		if obj == nil {
			return ""
		}
		if n, ok := a.memberRename[obj]; ok {
			return n
		}
		if n, ok := a.embedFollow[obj]; ok {
			return n
		}
		if pkgLevel {
			return a.pkgRename[obj]
		}
		return ""
	}
	add := func(doc *ast.CommentGroup, id *ast.Ident) {
		if doc == nil || id == nil {
			return
		}
		if n := newOf(id); n != "" {
			out = append(out, renamedDoc{doc, id.Name, n})
		}
	}
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			add(d.Doc, d.Name)
		case *ast.GenDecl:
			for _, s := range d.Specs {
				var names []*ast.Ident
				var doc *ast.CommentGroup
				switch s := s.(type) {
				case *ast.TypeSpec:
					names, doc = []*ast.Ident{s.Name}, s.Doc
				case *ast.ValueSpec:
					names, doc = s.Names, s.Doc
				}
				for _, id := range names {
					add(doc, id)
					// The declaration's own doc comment, or a group doc
					// that starts with this name.
					add(d.Doc, id)
				}
			}
		}
	}
	// Fields and interface methods (any nesting).
	ast.Inspect(f.AST, func(n ast.Node) bool {
		fl, ok := n.(*ast.Field)
		if !ok {
			return true
		}
		if len(fl.Names) == 0 {
			// An embedded field: its name is the type name.
			if id := embeddedIdent(fl.Type); id != nil {
				if obj := a.info.Defs[id]; obj != nil {
					if nn, ok := a.embedFollow[obj]; ok && fl.Doc != nil {
						out = append(out, renamedDoc{fl.Doc, id.Name, nn})
					}
				}
			}
			return true
		}
		for _, id := range fl.Names {
			add(fl.Doc, id)
		}
		return true
	})
	return out
}

// embeddedIdent returns the identifier that names an embedded field type.
func embeddedIdent(t ast.Expr) *ast.Ident {
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.SelectorExpr:
			return x.Sel
		case *ast.Ident:
			return x
		default:
			return nil
		}
	}
}

// renamedDocs maps the first comment of each renamed declaration's doc to the
// rename whose old name is its leading word.
func (a *analysis) renamedDocs(f *srcFile, pkgLevel bool) map[*ast.Comment]*renamedDoc {
	if f.XTest || !f.Included {
		return nil // not type-checked; nothing in it is renamed
	}
	out := map[*ast.Comment]*renamedDoc{}
	list := a.renamedDocList(f, pkgLevel)
	for i := range list {
		rd := &list[i]
		c := rd.doc.List[0]
		if word, _ := leadingWord(c.Text); word == rd.old {
			if _, taken := out[c]; !taken {
				out[c] = rd
			}
		}
	}
	return out
}

// leadingWord returns the first word of a comment's text and its byte offset
// in the text ("" if the comment does not start with an identifier).
func leadingWord(text string) (string, int) {
	i := 2 // after "//" or "/*"
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	j := i
	for j < len(text) {
		r, size := utf8.DecodeRuneInString(text[j:])
		if !isIdentRune(r) {
			break
		}
		j += size
	}
	return text[i:j], i
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isDirective reports whether a comment is a compiler or tool directive
// (the rule of go/ast: //line, //extern, //export, or //[a-z0-9]+:[a-z0-9]).
func isDirective(text string) bool {
	if !strings.HasPrefix(text, "//") {
		return false
	}
	c := text[2:]
	for _, p := range []string{"line ", "extern ", "export "} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	colon := strings.Index(c, ":")
	if colon <= 0 || colon+1 >= len(c) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := c[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}

// plainWord reports whether a name is a single lower-case word, which may be
// ordinary prose in a comment.
func plainWord(name string) bool {
	for _, r := range name {
		if !unicode.IsLower(r) {
			return false
		}
	}
	return true
}

// otherPkgNames returns the names under which f imports packages other than
// the source and target packages: a comment word qualified by one of them
// (http.serve) names something in that package, not a renamed identifier.
func (a *analysis) otherPkgNames(f *srcFile) map[string]bool {
	out := map[string]bool{}
	for _, spec := range f.AST.Imports {
		p := importPath(spec)
		if p == a.mod.ImportPath || p == a.dstImport {
			continue
		}
		if n := importName(spec); n != "_" && n != "." {
			out[n] = true
		}
	}
	delete(out, a.srcName)
	delete(out, a.cfg.PkgName)
	return out
}

// qualifierBefore returns the identifier before a '.' that directly precedes
// text[i:], or "".
func qualifierBefore(text string, i int) string {
	if i == 0 || text[i-1] != '.' {
		return ""
	}
	k := i - 1
	for k > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:k])
		if !isIdentRune(r) {
			break
		}
		k -= size
	}
	return text[k : i-1]
}

// codeContext reports whether the word at text[i:j] is spelled like code:
// next to '.', '(', '[', ']' or a backquote.
func codeContext(text string, i, j int) bool {
	if i > 0 {
		switch text[i-1] {
		case '.', '[', '`':
			return true
		}
	}
	if j < len(text) {
		switch text[j] {
		case '(', ']', '`':
			return true
		}
	}
	return false
}

// rewriteComment rewrites whole-word occurrences of the names in c. lead, if
// set, is the rename of the declaration that c documents (its leading word is
// rewritten even when it is a plain word).
func (a *analysis) rewriteComment(f *srcFile, c *ast.Comment, names map[string]string, lead *renamedDoc) {
	if isDirective(c.Text) {
		return
	}
	text := c.Text
	leadAt := -1
	if lead != nil {
		_, leadAt = leadingWord(text)
	}
	base := a.offset(c.Pos())
	others := a.otherPkgNames(f)
	for i := 2; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !isIdentRune(r) {
			i += size
			continue
		}
		j := i
		for j < len(text) {
			r, size := utf8.DecodeRuneInString(text[j:])
			if !isIdentRune(r) {
				break
			}
			j += size
		}
		word := text[i:j]
		n, ok := names[word]
		switch {
		case i == leadAt:
			n, ok = lead.new, true
		case !ok:
		case others[qualifierBefore(text, i)]:
			ok = false // pkg.name of another package: not the renamed identifier
		case plainWord(word) && !codeContext(text, i, j):
			ok = false
		}
		if ok {
			fe := a.editsFor(f)
			fe.edits = append(fe.edits, edit{base + i, base + j, n})
			a.plan.CommentRewrites = append(a.plan.CommentRewrites, varRewrite{Pos: a.posOf(c.Pos() + token.Pos(i)), Old: word, New: n})
		}
		i = j
	}
}
