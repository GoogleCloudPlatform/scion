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
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// exportSpec is an export request: move the named helper declarations out
// of the package into a non-test support package.
type exportSpec struct {
	names   []string                     // requested top-level names, in file order
	renames map[string]string            // explicit export names (tid=TID)
	fields  map[string]map[string]string // type -> unexported field -> exported name
}

// readExportSpec parses an export family file. Blank lines and #-comments
// are ignored. "rename old=New" sets the exported name of a top-level
// declaration (default: first letter upper-cased). "field T.f=F" exports
// field f of moved struct type T as F. Every other line is one top-level
// name to move.
func readExportSpec(path string) (exportSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return exportSpec{}, err
	}
	defer func() { _ = f.Close() }()
	spec := exportSpec{renames: map[string]string{}, fields: map[string]map[string]string{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "rename "); ok {
			o, n, ok := strings.Cut(strings.TrimSpace(rest), "=")
			if !ok || !token.IsIdentifier(o) || !token.IsExported(n) || !token.IsIdentifier(n) {
				return exportSpec{}, fmt.Errorf("%s: bad rename %q, want old=New", path, rest)
			}
			spec.renames[o] = n
			continue
		}
		if rest, ok := strings.CutPrefix(line, "field "); ok {
			tf, n, ok := strings.Cut(strings.TrimSpace(rest), "=")
			t, fl, ok2 := strings.Cut(tf, ".")
			if !ok || !ok2 || !token.IsIdentifier(t) || !token.IsIdentifier(fl) || token.IsExported(fl) || !token.IsExported(n) {
				return exportSpec{}, fmt.Errorf("%s: bad field %q, want T.field=Field", path, rest)
			}
			if spec.fields[t] == nil {
				spec.fields[t] = map[string]string{}
			}
			spec.fields[t][fl] = n
			continue
		}
		if !token.IsIdentifier(line) {
			return exportSpec{}, fmt.Errorf("%s: bad name %q", path, line)
		}
		spec.names = append(spec.names, line)
	}
	if err := sc.Err(); err != nil {
		return exportSpec{}, err
	}
	if len(spec.names) == 0 {
		return exportSpec{}, fmt.Errorf("%s: no names", path)
	}
	return spec, nil
}

// aliasKind says how a staying reference reaches an exported declaration.
type aliasKind int

const (
	aliasType     aliasKind = iota // type x = pkg.X
	aliasConst                     // const x = pkg.X
	aliasVarValue                  // var x = pkg.X (slice or map value: shares the backing store)
	aliasVarPtr                    // var x = &pkg.X (state holder declared with a type and no value)
	aliasFunc                      // func x(params) results { return pkg.X(args) }
)

func (k aliasKind) String() string {
	return [...]string{"type alias", "const alias", "var alias (value)", "var alias (pointer)", "wrapper func"}[k]
}

// exportPlan is the computed export.
type exportPlan struct {
	p          *pkgInfo
	spec       exportSpec
	targetPkg  string // package name of the target
	importPath string
	aliasFile  string
	selected   map[*declInfo]bool
	order      []*declInfo       // selected, in package order
	exported   map[string]string // staying-visible key -> exported name
	kinds      map[string]aliasKind
	helperCall map[string]string // func key -> receiver of the t.Helper call it makes ("" = none)
	fieldSites []fieldSite       // staying selectors rewritten for field exports
	curFile    *ast.File         // file being scanned by findFieldSites
	notes      []string          // review notes
	targetSrc  map[string][]byte // target file base name -> content
	changed    map[string][]byte // source package file -> new content (nil = delete)
}

// fieldSite is one staying selector x.f rewritten to x.F.
type fieldSite struct {
	file   string
	offset int
	old    string
	new    string
	line   int
}

// renameMap returns every identifier rename the export applies inside moved
// code: top-level exports plus field exports.
func (ep *exportPlan) renameMap() map[string]string {
	m := map[string]string{}
	for k, v := range ep.exported {
		m[k] = v
	}
	for _, fs := range ep.spec.fields {
		for f, n := range fs {
			m[f] = n
		}
	}
	return m
}

func upperFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// planExport selects the declarations, computes the exports and aliases, and
// renders every output file without writing anything.
func planExport(p *pkgInfo, spec exportSpec, targetDir, importPath, aliasFile string) (*exportPlan, error) {
	ep := &exportPlan{
		p: p, spec: spec, importPath: importPath, aliasFile: aliasFile,
		targetPkg:  filepath.Base(targetDir),
		selected:   map[*declInfo]bool{},
		exported:   map[string]string{},
		kinds:      map[string]aliasKind{},
		helperCall: map[string]string{},
		targetSrc:  map[string][]byte{},
		changed:    map[string][]byte{},
	}
	if !token.IsIdentifier(ep.targetPkg) {
		return nil, fmt.Errorf("target directory %q is not a valid package name", targetDir)
	}
	for _, f := range p.files {
		if f.name == aliasFile {
			return nil, fmt.Errorf("alias file %s already exists; export from a tree that does not have it", aliasFile)
		}
	}

	// 1. Requested names, then the methods of requested types.
	for _, n := range spec.names {
		ds := p.byKey[n]
		if len(ds) != 1 {
			return nil, fmt.Errorf("%s: declared %d times, want exactly once", n, len(ds))
		}
		d := ds[0]
		if !strings.HasSuffix(d.file.name, helperSuffix) {
			return nil, fmt.Errorf("%s: declared in %s, not in a %s file", n, d.file.name, helperSuffix)
		}
		if d.recv != "" {
			return nil, fmt.Errorf("%s: methods move with their type, not by name", n)
		}
		if isTestFunc(n) {
			return nil, fmt.Errorf("%s: test entry points never move (the test list must not change)", n)
		}
		ep.selected[d] = true
	}
	for _, d := range p.decls {
		if d.recv == "" {
			continue
		}
		owner := p.byKey[d.recv]
		if len(owner) == 1 && ep.selected[owner[0]] {
			if !strings.HasSuffix(d.file.name, helperSuffix) {
				return nil, fmt.Errorf("type %s has method %s in %s, not in a helper file", d.recv, d.key, d.file.name)
			}
			ep.selected[d] = true
		}
	}
	for _, d := range p.decls {
		if ep.selected[d] {
			ep.order = append(ep.order, d)
		}
	}

	// 2. Grouped declarations move whole.
	for _, d := range ep.order {
		if g, ok := d.node.(*ast.GenDecl); ok {
			for _, o := range p.decls {
				if o.node == g && !ep.selected[o] {
					return nil, fmt.Errorf("%s shares a declaration group with %s, which is not requested", d.key, o.key)
				}
			}
		}
	}

	// 3. Moved code may use only moved declarations of the package.
	for _, d := range ep.order {
		for _, r := range sortedKeys(p.refs(d)) {
			for _, rd := range p.byKey[r] {
				if !ep.selected[rd] {
					return nil, fmt.Errorf("%s uses %s (%s), which stays; request it too or leave %s in place", d.key, r, rd.file.name, d.key)
				}
			}
		}
	}

	// 4. Exports: every moved top-level name a staying declaration uses,
	// plus the names in the signatures of exported funcs and typed vars.
	users := p.users()
	for _, d := range ep.order {
		if d.recv != "" {
			continue
		}
		for u := range users[d.key] {
			if !ep.selected[u] {
				ep.exported[d.key] = ""
				break
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, d := range ep.order {
			if _, ok := ep.exported[d.key]; !ok || d.recv != "" {
				continue
			}
			for _, n := range signatureRefs(p, d) {
				if _, ok := ep.exported[n]; !ok {
					ep.exported[n] = ""
					changed = true
				}
			}
		}
	}
	taken := map[string]string{}
	for _, d := range ep.order {
		if d.recv == "" {
			taken[d.key] = d.key
		}
	}
	for _, k := range sortedKeys(ep.exported) {
		n := spec.renames[k]
		if n == "" {
			n = upperFirst(k)
		}
		if n == k {
			return nil, fmt.Errorf("%s is already exported; nothing to rename", k)
		}
		if o, ok := taken[n]; ok {
			return nil, fmt.Errorf("export name %s for %s collides with %s", n, k, o)
		}
		taken[n] = k
		ep.exported[k] = n
	}
	for k := range spec.renames {
		if _, ok := ep.exported[k]; !ok {
			return nil, fmt.Errorf("rename %s: not exported (no staying user)", k)
		}
	}

	// 5. Alias kinds; refuse vars whose alias would not behave like the original.
	for _, k := range sortedKeys(ep.exported) {
		kind, err := ep.classify(p.byKey[k][0])
		if err != nil {
			return nil, err
		}
		ep.kinds[k] = kind
	}

	// 6. Field exports.
	for t, fs := range spec.fields {
		ds := p.byKey[t]
		if len(ds) != 1 || !ep.selected[ds[0]] || !isType(ds[0]) {
			return nil, fmt.Errorf("field %s.*: %s is not a moved type", t, t)
		}
		for f := range fs {
			if !structHasField(ds[0], t, f) {
				return nil, fmt.Errorf("field %s.%s: no such field", t, f)
			}
		}
	}
	if err := ep.findFieldSites(); err != nil {
		return nil, err
	}

	// 7. Constraints: target files are unconstrained (one untagged alias
	// file), so a moved declaration must not need anything constrained.
	var moves []move
	for _, d := range ep.order {
		moves = append(moves, move{d: d, reason: "requested"})
	}
	if err := checkConstraints(p, moves, "", ep.selected); err != nil {
		return nil, err
	}

	// 8. Render.
	if err := ep.renderTargets(targetDir); err != nil {
		return nil, err
	}
	bySrc := map[*srcFile][]*declInfo{}
	for _, d := range ep.order {
		bySrc[d.file] = append(bySrc[d.file], d)
	}
	for f, ds := range bySrc {
		out, err := removeDecls(p, f, ds)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		ep.changed[f.name] = out
	}
	if err := ep.applyFieldSites(); err != nil {
		return nil, err
	}
	alias, err := ep.renderAlias()
	if err != nil {
		return nil, err
	}
	ep.changed[aliasFile] = alias
	ep.collectNotes()
	return ep, nil
}

// signatureRefs returns the moved type names that staying code can reach
// through an exported declaration, and that must therefore be exported too:
// those in a func's type parameters, parameters and results; in a var's
// declared type; and, for a type, in the types of its exported fields and in
// the signatures of its exported methods (GitSparseCheckoutStub.Calls returns
// []gitSparseCheckoutCall, so that type is exported as well).
func signatureRefs(p *pkgInfo, d *declInfo) []string {
	var nodes []ast.Node
	switch n := d.node.(type) {
	case *ast.FuncDecl:
		nodes = append(nodes, n.Type)
	case *ast.GenDecl:
		switch n.Tok {
		case token.VAR:
			for _, s := range n.Specs {
				if vs := s.(*ast.ValueSpec); vs.Type != nil && hasName(vs.Names, d.key) {
					nodes = append(nodes, vs.Type)
				}
			}
		case token.TYPE:
			if ts := typeSpec(d, d.key); ts != nil {
				if st, ok := ts.Type.(*ast.StructType); ok {
					for _, f := range st.Fields.List {
						exported := len(f.Names) == 0 // embedded: promoted members are reachable
						for _, fn := range f.Names {
							if token.IsExported(fn.Name) {
								exported = true
							}
						}
						if exported {
							nodes = append(nodes, f.Type)
						}
					}
				} else {
					// Interfaces, named slices, func types, ...: everything is spelled
					// in the type itself.
					nodes = append(nodes, ts.Type)
				}
			}
			for _, m := range p.decls {
				if fd, ok := m.node.(*ast.FuncDecl); ok && m.recv == d.key && fd.Name.IsExported() {
					nodes = append(nodes, fd.Type)
				}
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		ast.Inspect(n, func(m ast.Node) bool {
			if se, ok := m.(*ast.SelectorExpr); ok {
				ast.Inspect(se.X, func(x ast.Node) bool { addSigRef(p, x, seen, &out); return true })
				return false
			}
			addSigRef(p, m, seen, &out)
			return true
		})
	}
	sort.Strings(out)
	return out
}

func addSigRef(p *pkgInfo, n ast.Node, seen map[string]bool, out *[]string) {
	id, ok := n.(*ast.Ident)
	if !ok || seen[id.Name] {
		return
	}
	if ds := p.byKey[id.Name]; len(ds) == 1 && ds[0].recv == "" && isType(ds[0]) {
		seen[id.Name] = true
		*out = append(*out, id.Name)
	}
}

func hasName(ids []*ast.Ident, n string) bool {
	for _, id := range ids {
		if id.Name == n {
			return true
		}
	}
	return false
}

// classify picks the alias form for an exported declaration. A var alias is
// a copy, so it behaves like the original only if nobody ever assigns the
// var: whole-var assignment, element assignment, ++/--, or taking its
// address are refused anywhere in the package. A var declared with a type
// and no value is a state holder (atomic.Bool, a mutex): it is aliased
// through a pointer, so method calls reach the one instance. A var whose
// value is a slice or map literal is aliased by value: the copy shares the
// backing store. Any other var is refused for a human decision.
func (ep *exportPlan) classify(d *declInfo) (aliasKind, error) {
	switch n := d.node.(type) {
	case *ast.FuncDecl:
		h, err := helperReceiver(n)
		if err != nil {
			return 0, err
		}
		ep.helperCall[d.key] = h
		return aliasFunc, nil
	case *ast.GenDecl:
		switch n.Tok {
		case token.TYPE:
			return aliasType, nil
		case token.CONST:
			return aliasConst, nil
		case token.VAR:
			if sites := ep.assignments(d.key); len(sites) > 0 {
				return 0, fmt.Errorf("var %s is assigned (%s); an alias copy would diverge, so it cannot be exported", d.key, strings.Join(sites, ", "))
			}
			for _, s := range n.Specs {
				vs := s.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if id.Name != d.key {
						continue
					}
					if len(vs.Values) == 0 && vs.Type != nil {
						return aliasVarPtr, nil
					}
					if i < len(vs.Values) {
						if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
							switch t := cl.Type.(type) {
							case *ast.MapType:
								return aliasVarValue, nil
							case *ast.ArrayType:
								if t.Len == nil {
									return aliasVarValue, nil
								}
							}
						}
					}
				}
			}
			return 0, fmt.Errorf("var %s: cannot tell whether a copy behaves like the original (only slice/map literals or typed zero-value state holders are aliased)", d.key)
		}
	}
	return 0, fmt.Errorf("%s: unsupported declaration", d.key)
}

// helperReceiver returns the name of the parameter on which a func calls
// Helper() as a top-level statement of its body, or "". A Helper() call
// anywhere else (a nested block, a closure, a non-parameter receiver, a
// second call) is refused: the wrapper's own Helper() call could not mirror
// it, so failure attribution might change.
func helperReceiver(fd *ast.FuncDecl) (string, error) {
	if fd.Body == nil {
		return "", nil
	}
	params := map[string]bool{}
	for _, f := range fd.Type.Params.List {
		for _, n := range f.Names {
			params[n.Name] = true
		}
	}
	recv := ""
	var top *ast.CallExpr
	for _, st := range fd.Body.List {
		es, ok := st.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok || !isHelperCall(call) {
			continue
		}
		if id, ok := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident); ok && params[id.Name] && top == nil {
			recv, top = id.Name, call
		}
	}
	var stray []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && call != top && isHelperCall(call) {
			stray = append(stray, fmt.Sprint(call.Pos()))
		}
		return true
	})
	if len(stray) > 0 {
		return "", fmt.Errorf("%s calls Helper() other than as a top-level statement on a parameter; a wrapper cannot mirror that, so it is not exported", fd.Name.Name)
	}
	return recv, nil
}

// isHelperCall reports whether call is x.Helper() with no arguments.
func isHelperCall(call *ast.CallExpr) bool {
	se, ok := call.Fun.(*ast.SelectorExpr)
	return ok && se.Sel.Name == "Helper" && len(call.Args) == 0
}

// assignments lists the places in the package that write the top-level var
// name: assignment (also to an element or field of it), ++/--, or &name.
func (ep *exportPlan) assignments(name string) []string {
	var out []string
	for _, f := range ep.p.files {
		top := topLevelObjs(f.ast)
		isVar := func(e ast.Expr) bool {
			for {
				switch x := e.(type) {
				case *ast.IndexExpr:
					e = x.X
				case *ast.SelectorExpr:
					e = x.X
				case *ast.ParenExpr:
					e = x.X
				case *ast.StarExpr:
					e = x.X
				case *ast.Ident:
					return x.Name == name && (x.Obj == nil || top[x.Obj])
				default:
					return false
				}
			}
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			var hit ast.Node
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if isVar(l) {
						hit = x
					}
				}
			case *ast.IncDecStmt:
				if isVar(x.X) {
					hit = x
				}
			case *ast.UnaryExpr:
				if x.Op == token.AND && isVar(x.X) {
					hit = x
				}
			case *ast.RangeStmt:
				if x.Key != nil && isVar(x.Key) || x.Value != nil && isVar(x.Value) {
					hit = x
				}
			}
			if hit != nil {
				pos := ep.p.fset.Position(hit.Pos())
				out = append(out, fmt.Sprintf("%s:%d", f.name, pos.Line))
			}
			return true
		})
	}
	return out
}

func structHasField(d *declInfo, typeName, field string) bool {
	g := d.node.(*ast.GenDecl)
	for _, s := range g.Specs {
		ts := s.(*ast.TypeSpec)
		if ts.Name.Name != typeName {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, f := range st.Fields.List {
			if hasName(f.Names, field) {
				return true
			}
		}
	}
	return false
}

// findFieldSites finds staying selectors x.f that need the field export:
// those whose x is a call of a moved func that returns T or *T. Other
// selectors named f are left alone (and listed in the notes); the
// compiler rejects any missed access to an unexported field of T.
func (ep *exportPlan) findFieldSites() error {
	if len(ep.spec.fields) == 0 {
		return nil
	}
	returns := map[string]string{} // moved func -> type it returns
	for _, d := range ep.order {
		fd, ok := d.node.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
			continue
		}
		t := fd.Type.Results.List[0].Type
		if s, ok := t.(*ast.StarExpr); ok {
			t = s.X
		}
		if id, ok := t.(*ast.Ident); ok && ep.spec.fields[id.Name] != nil {
			returns[d.key] = id.Name
		}
	}
	fieldNames := map[string]bool{}
	for _, fs := range ep.spec.fields {
		for f := range fs {
			fieldNames[f] = true
		}
	}
	for _, f := range ep.p.files {
		ep.curFile = f.ast
		movedSpans := ep.movedSpans(f)
		ast.Inspect(f.ast, func(n ast.Node) bool {
			se, ok := n.(*ast.SelectorExpr)
			if !ok || !fieldNames[se.Sel.Name] {
				return true
			}
			off := ep.p.fset.Position(se.Sel.Pos()).Offset
			for _, sp := range movedSpans {
				if off >= sp[0] && off < sp[1] {
					return true
				}
			}
			pos := ep.p.fset.Position(se.Sel.Pos())
			if t := ep.operandType(se.X, returns); t != "" {
				if nn, ok := ep.spec.fields[t][se.Sel.Name]; ok {
					ep.fieldSites = append(ep.fieldSites, fieldSite{file: f.name, offset: off, old: se.Sel.Name, new: nn, line: pos.Line})
					return true
				}
			}
			ep.notes = append(ep.notes, fmt.Sprintf("INFO selector .%s at %s:%d not rewritten (operand is not known to have a field-exported type; the compiler checks any real field access)", se.Sel.Name, f.name, pos.Line))
			return true
		})
	}
	return nil
}

// operandType returns the field-exported moved type that the operand of a
// selector syntactically has, or "":
//   - a call F(...) of a moved func whose single result is T or *T;
//   - a local variable declared from such a call, x := F(...) or
//     var x = F(...) (one name, one value, no declared type), and never
//     written again (see reassigned).
//
// The analysis follows the parser's object resolution only, so anything
// else (a field, a parameter, a reassigned or multi-value variable) is
// left alone and reported; the compiler rejects any missed access.
func (ep *exportPlan) operandType(x ast.Expr, returns map[string]string) string {
	switch e := x.(type) {
	case *ast.ParenExpr:
		return ep.operandType(e.X, returns)
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok {
			return returns[id.Name]
		}
	case *ast.Ident:
		if e.Obj == nil || e.Obj.Kind != ast.Var || ep.curFile == nil || reassigned(ep.curFile, e.Obj) {
			return ""
		}
		switch d := e.Obj.Decl.(type) {
		case *ast.AssignStmt:
			if d.Tok == token.DEFINE && len(d.Lhs) == 1 && len(d.Rhs) == 1 {
				if lid, ok := d.Lhs[0].(*ast.Ident); ok && lid.Name == e.Name {
					if call, ok := d.Rhs[0].(*ast.CallExpr); ok {
						return ep.operandType(call, returns)
					}
				}
			}
		case *ast.ValueSpec:
			if len(d.Names) == 1 && len(d.Values) == 1 && d.Type == nil {
				if call, ok := d.Values[0].(*ast.CallExpr); ok {
					return ep.operandType(call, returns)
				}
			}
		}
	}
	return ""
}

// reassigned reports whether the local object obj is written in f after
// its declaration: assigned (=, op=), incremented, ranged into or taken
// by address. Such a local is not rewritten.
func reassigned(f *ast.File, obj *ast.Object) bool { //nolint:staticcheck // SA1019: syntax-only resolution, as elsewhere in this tool.
	is := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Obj == obj
	}
	hit := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x == obj.Decl {
				return true
			}
			for _, l := range x.Lhs {
				if is(l) {
					hit = true
				}
			}
		case *ast.IncDecStmt:
			hit = hit || is(x.X)
		case *ast.UnaryExpr:
			hit = hit || x.Op == token.AND && is(x.X)
		case *ast.RangeStmt:
			hit = hit || x.Key != nil && is(x.Key) || x.Value != nil && is(x.Value)
		}
		return !hit
	})
	return hit
}

func (ep *exportPlan) movedSpans(f *srcFile) [][2]int {
	var out [][2]int
	for _, d := range ep.order {
		if d.file == f {
			s, e := d.span(ep.p.fset)
			out = append(out, [2]int{s, e})
		}
	}
	return out
}

func (ep *exportPlan) applyFieldSites() error {
	byFile := map[string][]fieldSite{}
	for _, s := range ep.fieldSites {
		byFile[s.file] = append(byFile[s.file], s)
	}
	for name, sites := range byFile {
		if _, ok := ep.changed[name]; ok {
			return fmt.Errorf("%s: field-export call site in a file that also loses moved declarations; not supported", name)
		}
		var f *srcFile
		for _, x := range ep.p.files {
			if x.name == name {
				f = x
			}
		}
		if formatted, err := format.Source(f.src); err != nil || !bytes.Equal(formatted, f.src) {
			return fmt.Errorf("%s: not gofmt-clean before the move; refusing to touch it", name)
		}
		sort.Slice(sites, func(i, j int) bool { return sites[i].offset < sites[j].offset })
		var b bytes.Buffer
		prev := 0
		for _, s := range sites {
			b.Write(f.src[prev:s.offset])
			b.WriteString(s.new)
			prev = s.offset + len(s.old)
		}
		b.Write(f.src[prev:])
		out, err := format.Source(b.Bytes())
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		ep.changed[name] = out
	}
	return nil
}

// renamedText returns the declaration's verbatim source with the export
// renames applied to identifiers that refer to moved top-level names, to
// the declared fields of moved types, and to selectors and composite
// literal keys naming those fields.
func (ep *exportPlan) renamedText(d *declInfo) (string, error) {
	s, e := d.span(ep.p.fset)
	type edit struct {
		off int
		old string
		new string
	}
	var edits []edit
	top := topLevelObjs(d.file.ast)
	fieldNew := map[string]string{}
	for _, fs := range ep.spec.fields {
		for f, n := range fs {
			fieldNew[f] = n
		}
	}
	addIdent := func(id *ast.Ident, n string) {
		edits = append(edits, edit{off: ep.p.fset.Position(id.Pos()).Offset, old: id.Name, new: n})
	}
	// Declared names (the decl's own identifiers, receivers' types are refs).
	var visit func(n ast.Node) bool
	var err error
	keyIdents := map[*ast.Ident]bool{}
	selIdents := map[*ast.Ident]bool{}
	ast.Inspect(d.node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						keyIdents[id] = true
					}
				}
			}
		case *ast.SelectorExpr:
			selIdents[x.Sel] = true
		}
		return true
	})
	// Field declarations of moved struct types.
	if g, ok := d.node.(*ast.GenDecl); ok && g.Tok == token.TYPE {
		for _, sp := range g.Specs {
			ts := sp.(*ast.TypeSpec)
			fs := ep.spec.fields[ts.Name.Name]
			if st, ok := ts.Type.(*ast.StructType); ok && fs != nil {
				for _, f := range st.Fields.List {
					for _, id := range f.Names {
						if n, ok := fs[id.Name]; ok {
							addIdent(id, n)
						}
					}
				}
			}
		}
	}
	visit = func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if selIdents[id] {
			if nn, ok := fieldNew[id.Name]; ok {
				addIdent(id, nn)
			}
			return true
		}
		if keyIdents[id] {
			if nn, ok := fieldNew[id.Name]; ok {
				addIdent(id, nn)
				return true
			}
			if _, ok := ep.exported[id.Name]; ok {
				err = fmt.Errorf("%s: composite literal key %s matches an exported name; cannot tell a field key from a value reference", d.key, id.Name)
			}
			return true
		}
		if id.Obj != nil && !top[id.Obj] {
			return true // local
		}
		if nn, ok := ep.exported[id.Name]; ok {
			if ds := ep.p.byKey[id.Name]; len(ds) == 1 && ep.selected[ds[0]] {
				addIdent(id, nn)
			}
		}
		return true
	}
	ast.Inspect(d.node, visit)
	if err != nil {
		return "", err
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].off < edits[j].off })
	var b strings.Builder
	prev := s
	for i, ed := range edits {
		if i > 0 && ed.off == edits[i-1].off {
			continue
		}
		b.Write(d.file.src[prev:ed.off])
		b.WriteString(ed.new)
		prev = ed.off + len(ed.old)
	}
	b.Write(d.file.src[prev:e])
	return b.String(), nil
}

// targetFileName maps a helper file to its file in the target package:
// x_helpers_test.go -> x.go.
func targetFileName(origin string) string {
	return strings.TrimSuffix(origin, helperSuffix) + ".go"
}

func (ep *exportPlan) renderTargets(targetDir string) error {
	byFile := map[string][]*declInfo{}
	for _, d := range ep.order {
		byFile[targetFileName(d.file.name)] = append(byFile[targetFileName(d.file.name)], d)
	}
	for name, ds := range byFile {
		if _, err := os.Stat(filepath.Join(targetDir, name)); err == nil {
			return fmt.Errorf("target file %s already exists; export into a tree that does not have it", filepath.Join(targetDir, name))
		}
		var texts, seps []string
		var prevFile *srcFile
		prevEnd := -1
		imports := map[importKey]bool{}
		seen := map[ast.Decl]bool{}
		for _, d := range ds {
			if seen[d.node] {
				continue
			}
			seen[d.node] = true
			s, e := d.span(ep.p.fset)
			sep := "\n\n"
			if d.file == prevFile && prevEnd >= 0 && prevEnd <= s && adjacent(d.file.src[prevEnd:s]) {
				sep = "\n"
			}
			t, err := ep.renamedText(d)
			if err != nil {
				return err
			}
			texts = append(texts, t)
			seps = append(seps, sep)
			prevFile, prevEnd = d.file, e
			imps, err := neededImports(d)
			if err != nil {
				return err
			}
			for _, s := range imps {
				imports[specKey(s)] = true
			}
		}
		var b bytes.Buffer
		b.WriteString(licenseHeader)
		b.WriteString("\n")
		fmt.Fprintf(&b, "package %s\n\n", ep.targetPkg)
		writeImports(&b, imports)
		for i, t := range texts {
			if i > 0 {
				b.WriteString(seps[i])
			}
			b.WriteString(t)
		}
		b.WriteString("\n")
		out, err := format.Source(b.Bytes())
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		ep.targetSrc[name] = out
	}
	return nil
}

func writeImports(b *bytes.Buffer, imports map[importKey]bool) {
	var std, other []importKey
	for k := range imports {
		ip, _ := strconv.Unquote(k.path)
		if strings.Contains(strings.SplitN(ip, "/", 2)[0], ".") {
			other = append(other, k)
		} else {
			std = append(std, k)
		}
	}
	if len(std)+len(other) == 0 {
		return
	}
	b.WriteString("import (\n")
	for i, grp := range [][]importKey{std, other} {
		sort.Slice(grp, func(a, c int) bool { return grp[a].path < grp[c].path })
		if i == 1 && len(std) > 0 && len(other) > 0 {
			b.WriteString("\n")
		}
		for _, k := range grp {
			if k.name != "" {
				fmt.Fprintf(b, "\t%s %s\n", k.name, k.path)
			} else {
				fmt.Fprintf(b, "\t%s\n", k.path)
			}
		}
	}
	b.WriteString(")\n\n")
}

// renderAlias writes the staying package's alias file: one declaration per
// exported name, sorted by name, so staying code needs no edits.
func (ep *exportPlan) renderAlias() ([]byte, error) {
	q := ep.targetPkg
	imports := map[importKey]bool{{path: strconv.Quote(ep.importPath)}: true}
	var decls []string
	for _, k := range sortedKeys(ep.exported) {
		d := ep.p.byKey[k][0]
		n := ep.exported[k]
		switch ep.kinds[k] {
		case aliasType:
			ts := typeSpec(d, k)
			tp, args := "", ""
			if ts.TypeParams != nil {
				tp = srcText(ep.p, d.file, ts.TypeParams.Opening, ts.TypeParams.Closing+1)
				args = "[" + strings.Join(fieldNames(ts.TypeParams), ", ") + "]"
				imps, err := neededImports(d)
				if err != nil {
					return nil, err
				}
				used := usedImportNames(ts.TypeParams)
				for _, s := range imps {
					if used[importName(s)] {
						imports[specKey(s)] = true
					}
				}
			}
			decls = append(decls, fmt.Sprintf("type %s%s = %s.%s%s", k, tp, q, n, args))
		case aliasConst:
			decls = append(decls, fmt.Sprintf("const %s = %s.%s", k, q, n))
		case aliasVarValue:
			decls = append(decls, fmt.Sprintf("var %s = %s.%s", k, q, n))
		case aliasVarPtr:
			decls = append(decls, fmt.Sprintf("var %s = &%s.%s", k, q, n))
		case aliasFunc:
			fd := d.node.(*ast.FuncDecl)
			text, used, err := ep.wrapper(d, fd, k, n)
			if err != nil {
				return nil, err
			}
			decls = append(decls, text)
			imps, err := neededImports(d)
			if err != nil {
				return nil, err
			}
			for _, s := range imps {
				if used[importName(s)] {
					imports[specKey(s)] = true
				}
			}
		}
	}
	var b bytes.Buffer
	b.WriteString(licenseHeader)
	b.WriteString("\n")
	fmt.Fprintf(&b, "package %s\n\n", ep.p.pkg)
	writeImports(&b, imports)
	fmt.Fprintf(&b, "// The declarations below keep the names of test helpers that moved to\n"+
		"// %s\n"+
		"// (generated by hack/hubshard/extract export), so the tests of this\n"+
		"// package need no edits. Code outside this package uses %s directly.\n\n", ep.importPath, q)
	for i, t := range decls {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t)
	}
	b.WriteString("\n")
	return format.Source(b.Bytes())
}

func typeSpec(d *declInfo, name string) *ast.TypeSpec {
	for _, s := range d.node.(*ast.GenDecl).Specs {
		if ts := s.(*ast.TypeSpec); ts.Name.Name == name {
			return ts
		}
	}
	return nil
}

func fieldNames(fl *ast.FieldList) []string {
	var out []string
	for _, f := range fl.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func srcText(p *pkgInfo, f *srcFile, from, to token.Pos) string {
	return string(f.src[p.fset.Position(from).Offset:p.fset.Position(to).Offset])
}

// wrapper renders a forwarding func with the original signature. Parameter
// types and results are copied verbatim (they name staying aliases or
// imported packages); unnamed or blank parameters get generated names. The
// wrapper calls Helper() on the same parameter the original does, so a
// failure is still attributed to the test's line. It also returns the
// package qualifiers its text uses.
func (ep *exportPlan) wrapper(d *declInfo, fd *ast.FuncDecl, k, n string) (string, map[string]bool, error) {
	var params, args []string
	used := map[string]bool{}
	gen := 0
	for _, f := range fd.Type.Params.List {
		typ := srcText(ep.p, d.file, f.Type.Pos(), f.Type.End())
		for k2, v := range usedImportNames(f.Type) {
			used[k2] = v
		}
		names := fieldNamesOf(f)
		if len(names) == 0 {
			names = []string{"_"}
		}
		_, variadic := f.Type.(*ast.Ellipsis)
		for i, nm := range names {
			if nm == "_" {
				nm = fmt.Sprintf("p%d", gen)
				gen++
				names[i] = nm
			}
			if variadic {
				args = append(args, nm+"...")
			} else {
				args = append(args, nm)
			}
		}
		// Keep the original grouping (what, s string).
		params = append(params, strings.Join(names, ", ")+" "+typ)
	}
	res := ""
	if fd.Type.Results != nil {
		res = " " + srcText(ep.p, d.file, fd.Type.Results.Pos(), fd.Type.Results.End())
		for k2, v := range usedImportNames(fd.Type.Results) {
			used[k2] = v
		}
	}
	tp, inst := "", ""
	if fd.Type.TypeParams != nil {
		tp = srcText(ep.p, d.file, fd.Type.TypeParams.Opening, fd.Type.TypeParams.Closing+1)
		inst = "[" + strings.Join(fieldNames(fd.Type.TypeParams), ", ") + "]"
		for k2, v := range usedImportNames(fd.Type.TypeParams) {
			used[k2] = v
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "func %s%s(%s)%s {\n", k, tp, strings.Join(params, ", "), res)
	if h := ep.helperCall[k]; h != "" {
		fmt.Fprintf(&b, "\t%s.Helper()\n", h)
	}
	call := fmt.Sprintf("%s.%s%s(%s)", ep.targetPkg, n, inst, strings.Join(args, ", "))
	if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
		fmt.Fprintf(&b, "\treturn %s\n", call)
	} else {
		fmt.Fprintf(&b, "\t%s\n", call)
	}
	b.WriteString("}")
	return b.String(), used, nil
}

func fieldNamesOf(f *ast.Field) []string {
	var out []string
	for _, n := range f.Names {
		out = append(out, n.Name)
	}
	return out
}

// collectNotes lists what a reviewer should check by hand.
func (ep *exportPlan) collectNotes() {
	for _, d := range ep.order {
		ast.Inspect(d.node, func(n ast.Node) bool {
			se, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := se.X.(*ast.Ident)
			if !ok || x.Obj != nil {
				return true
			}
			switch x.Name + "." + se.Sel.Name {
			case "runtime.Stack", "runtime.Caller", "runtime.FuncForPC", "debug.Stack", "pprof.Lookup", "reflect.TypeOf":
				ep.notes = append(ep.notes, fmt.Sprintf("WARN %s calls %s.%s: stacks and names now show package %s", d.key, x.Name, se.Sel.Name, ep.targetPkg))
			}
			return true
		})
	}
	// Source-scanning tests: they read this package's files, so moved code
	// leaves their view.
	for _, f := range ep.p.files {
		if !f.isTest {
			continue
		}
		parses := false
		for _, s := range f.ast.Imports {
			if ip, _ := strconv.Unquote(s.Path.Value); ip == "go/parser" || ip == "golang.org/x/tools/go/packages" {
				parses = true
			}
		}
		if !parses {
			continue
		}
		if bytes.Contains(f.src, []byte("filepath.Glob")) || bytes.Contains(f.src, []byte("os.ReadDir")) ||
			bytes.Contains(f.src, []byte("WalkDir")) || bytes.Contains(f.src, []byte("ParseDir")) || bytes.Contains(f.src, []byte("packages.Load")) {
			ep.notes = append(ep.notes, fmt.Sprintf("REVIEW source-scanning test file %s: moved declarations are no longer in its view", f.name))
		}
	}
	sort.Strings(ep.notes)
}

func (ep *exportPlan) write(targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	for _, name := range sortedKeys(ep.targetSrc) {
		if err := os.WriteFile(filepath.Join(targetDir, name), ep.targetSrc[name], 0o644); err != nil {
			return err
		}
	}
	return apply(ep.p, &result{changed: ep.changed})
}

func (ep *exportPlan) report(w *bytes.Buffer) {
	fmt.Fprintf(w, "moved declarations: %d\n", len(ep.order))
	for _, d := range ep.order {
		to := targetFileName(d.file.name)
		fmt.Fprintf(w, "  move %-36s %s -> %s\n", d.key, d.file.name, to)
	}
	fmt.Fprintf(w, "exports (renames applied in moved code, aliases in %s): %d\n", ep.aliasFile, len(ep.exported))
	for _, k := range sortedKeys(ep.exported) {
		extra := ""
		if h := ep.helperCall[k]; h != "" {
			extra = " (calls " + h + ".Helper())"
		}
		fmt.Fprintf(w, "  %-36s -> %-36s %s%s\n", k, ep.exported[k], ep.kinds[k], extra)
	}
	for _, t := range sortedKeys(ep.spec.fields) {
		for _, f := range sortedKeys(ep.spec.fields[t]) {
			fmt.Fprintf(w, "field export %s.%s -> %s\n", t, f, ep.spec.fields[t][f])
		}
	}
	for _, s := range ep.fieldSites {
		fmt.Fprintf(w, "  call site %s:%d .%s -> .%s\n", s.file, s.line, s.old, s.new)
	}
	for _, name := range sortedKeys(ep.changed) {
		if ep.changed[name] == nil {
			fmt.Fprintf(w, "delete %s (no declarations left)\n", name)
		}
	}
	for _, n := range ep.notes {
		fmt.Fprintln(w, n)
	}
}
