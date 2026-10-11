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
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// srcFile is one parsed Go file of the package directory.
type srcFile struct {
	name       string // base name
	src        []byte
	ast        *ast.File
	constraint string // the //go:build expression, "" if none
	isTest     bool
	blanks     int // blank-identifier value specs seen so far (for stable keys)
}

// declInfo is one top-level declaration.
type declInfo struct {
	key  string // name for funcs/types/values, "Recv.name" for methods
	file *srcFile
	node ast.Decl
	recv string // receiver base type name for methods, "" otherwise
}

// span returns the byte range of the declaration including its doc comment
// and any comment that trails it on its last line.
func (d *declInfo) span(fset *token.FileSet) (int, int) {
	return declSpan(fset, d.file, d.node)
}

func declSpan(fset *token.FileSet, f *srcFile, n ast.Decl) (int, int) {
	start := n.Pos()
	switch n := n.(type) {
	case *ast.FuncDecl:
		if n.Doc != nil {
			start = n.Doc.Pos()
		}
	case *ast.GenDecl:
		if n.Doc != nil {
			start = n.Doc.Pos()
		}
	}
	s := fset.Position(start).Offset
	e := fset.Position(n.End()).Offset
	// Include a trailing same-line comment.
	for _, cg := range f.ast.Comments {
		cs := fset.Position(cg.Pos()).Offset
		if cs >= e && !bytes.Contains(f.src[e:cs], []byte("\n")) {
			e = fset.Position(cg.End()).Offset
		}
	}
	return s, e
}

// text returns the verbatim source of the declaration.
func (d *declInfo) text(fset *token.FileSet) string {
	s, e := d.span(fset)
	return string(d.file.src[s:e])
}

// pkgInfo is the parsed package directory.
type pkgInfo struct {
	dir   string
	fset  *token.FileSet
	files []*srcFile // sorted by name
	byKey map[string][]*declInfo
	decls []*declInfo // in file then source order
	pkg   string      // package name of the internal test files
}

func loadPackage(dir string) (*pkgInfo, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	p := &pkgInfo{dir: dir, fset: token.NewFileSet(), byKey: map[string][]*declInfo{}}
	for _, n := range names {
		src, err := os.ReadFile(n)
		if err != nil {
			return nil, err
		}
		af, err := parser.ParseFile(p.fset, n, src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(n)
		if strings.HasSuffix(af.Name.Name, "_test") {
			continue // external test package: a different package
		}
		if p.pkg == "" {
			p.pkg = af.Name.Name
		} else if p.pkg != af.Name.Name {
			return nil, fmt.Errorf("%s: package %s, want %s", base, af.Name.Name, p.pkg)
		}
		f := &srcFile{name: base, src: src, ast: af, isTest: strings.HasSuffix(base, "_test.go")}
		f.constraint, err = buildConstraint(af)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", base, err)
		}
		p.files = append(p.files, f)
		for _, d := range af.Decls {
			for _, di := range declInfos(f, d) {
				p.byKey[di.key] = append(p.byKey[di.key], di)
				p.decls = append(p.decls, di)
			}
		}
	}
	return p, nil
}

// declInfos returns one declInfo per top-level declaration. A GenDecl that
// declares several names yields one entry per name, all sharing the node.
func declInfos(f *srcFile, d ast.Decl) []*declInfo {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			r := recvTypeName(d.Recv.List[0].Type)
			return []*declInfo{{key: r + "." + d.Name.Name, file: f, node: d, recv: r}}
		}
		return []*declInfo{{key: d.Name.Name, file: f, node: d}}
	case *ast.GenDecl:
		var out []*declInfo
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				out = append(out, &declInfo{key: s.Name.Name, file: f, node: d})
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.Name == "_" {
						// Blank names get a key stable across edits of other files.
						f.blanks++
						out = append(out, &declInfo{key: "_@" + f.name + "#" + strconv.Itoa(f.blanks), file: f, node: d})
						continue
					}
					out = append(out, &declInfo{key: n.Name, file: f, node: d})
				}
			}
		}
		return out
	}
	return nil
}

func recvTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return fmt.Sprintf("%T", e)
		}
	}
}

func buildConstraint(f *ast.File) (string, error) {
	for _, cg := range f.Comments {
		if cg.Pos() >= f.Package {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				x, err := constraint.Parse(c.Text)
				if err != nil {
					return "", err
				}
				return x.String(), nil
			}
		}
	}
	return "", nil
}

// refs returns the package-level names a declaration refers to, using the
// parser's syntactic object resolution: an identifier counts unless it is a
// selector's field/method name or resolves to a local (non top-level)
// object. Struct field names in composite literal keys are counted too,
// which only over-approximates (safe for the "used elsewhere" test).
func (p *pkgInfo) refs(d *declInfo) map[string]bool {
	top := topLevelObjs(d.file.ast)
	out := map[string]bool{}
	var own []*ast.Ident
	switch n := d.node.(type) {
	case *ast.FuncDecl:
		own = append(own, n.Name)
	case *ast.GenDecl:
		for _, s := range n.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				own = append(own, s.Name)
			case *ast.ValueSpec:
				own = append(own, s.Names...)
			}
		}
	}
	isOwn := func(id *ast.Ident) bool {
		for _, o := range own {
			if o == id {
				return true
			}
		}
		return false
	}
	ast.Inspect(d.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			ast.Inspect(n.X, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok {
					p.addRef(out, id, top, isOwn)
				}
				return true
			})
			return false
		case *ast.Field:
			// Field, parameter and result names declare locals.
			ast.Inspect(n.Type, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok {
					p.addRef(out, id, top, isOwn)
				}
				return true
			})
			return false
		case *ast.Ident:
			p.addRef(out, n, top, isOwn)
		}
		return true
	})
	return out
}

func (p *pkgInfo) addRef(out map[string]bool, id *ast.Ident, top map[*ast.Object]bool, isOwn func(*ast.Ident) bool) { //nolint:staticcheck // SA1019: syntax-only resolution via ast.Object is intended here; no type info is loaded.
	if isOwn(id) {
		return
	}
	if id.Obj != nil && !top[id.Obj] {
		return // local
	}
	if _, ok := p.byKey[id.Name]; ok {
		out[id.Name] = true
	}
}

func topLevelObjs(f *ast.File) map[*ast.Object]bool { //nolint:staticcheck // SA1019: file-scope objects from the parser, as above.
	m := map[*ast.Object]bool{} //nolint:staticcheck // SA1019: as above.
	if f.Scope != nil {
		for _, o := range f.Scope.Objects {
			m[o] = true
		}
	}
	return m
}

// importName guesses the package name an import path binds when it has no
// explicit name. Callers validate the guess against actual use.
var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

func importName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	ip, _ := strconv.Unquote(spec.Path.Value)
	base := path.Base(ip)
	if majorVersion.MatchString(base) && strings.Contains(ip, "/") {
		base = path.Base(path.Dir(ip))
	}
	if i := strings.Index(base, ".v"); i > 0 {
		base = base[:i]
	}
	base = strings.TrimPrefix(base, "go-")
	base = strings.TrimSuffix(base, "-go")
	return strings.ReplaceAll(base, "-", "")
}

// usedImportNames returns the names used as the X of a selector that do not
// resolve to a local or top-level object (i.e. package qualifiers).
func usedImportNames(n ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(n, func(m ast.Node) bool {
		if se, ok := m.(*ast.SelectorExpr); ok {
			if id, ok := se.X.(*ast.Ident); ok && id.Obj == nil {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}
