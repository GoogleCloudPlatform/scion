// Command checkrootopen flags direct (*os.Root).Open, (*os.Root).OpenFile,
// (*os.Root).Create, and os.OpenInRoot uses outside an explicit allowlist.
//
// Use the shared helper in pkg/util/rootopen (OpenAt, OpenRegularForRead,
// OpenFileForWrite, OpenDir, MkdirAll, OpenFileForWriteInDir, UnlinkInDir) so
// relative path opens under an *os.Root enforce no-follow on every component
// and non-blocking leaf opens in the kernel.
//
// Every justified exception is listed in hack/root-open-allowlist.txt as:
//
//	<file> | <function> | <count> | <reason>
//
// Exit codes (see hack/LINT-CONVENTIONS.md):
//
//	0  analysed, no violations
//	1  violations, or a malformed or stale allowlist entry
//	3  could not analyse (parse error, unreadable file)
//	4  scan root missing, allowlist missing, or zero counted calls found
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const toolName = "check-root-open"

// targetMethods are the *os.Root methods flagged outside the allowlist.
var targetMethods = map[string]bool{
	"Open":     true,
	"OpenFile": true,
	"Create":   true,
}

type typeRef struct {
	pkg  string
	name string
}

type valInfo struct {
	isRoot bool
	tr     typeRef
}

type sigIndex struct {
	pkgFuncs     map[string]map[string][]valInfo            // pkgKey -> funcName -> return slots
	pkgMethods   map[string]map[string]map[string][]valInfo // pkgKey -> recvType -> methodName -> return slots
	structFields map[string]map[string]map[string]valInfo   // pkgKey -> structName -> fieldName -> valInfo
	pkgVars      map[string]map[string]valInfo              // pkgKey -> varName -> valInfo
	rootTypes    map[string]map[string]bool                 // pkgKey -> local type alias name -> true
	pkgKeys      map[string]bool                            // all scanned relative package dirs
}

func newSigIndex() *sigIndex {
	return &sigIndex{
		pkgFuncs:     map[string]map[string][]valInfo{},
		pkgMethods:   map[string]map[string]map[string][]valInfo{},
		structFields: map[string]map[string]map[string]valInfo{},
		pkgVars:      map[string]map[string]valInfo{},
		rootTypes:    map[string]map[string]bool{},
		pkgKeys:      map[string]bool{},
	}
}

type parsedFile struct {
	relPath string
	pkgKey  string
	file    *ast.File
	osAlias string
	imports map[string]string // localName -> importPath
}

// collectFiles walks <root>/pkg and <root>/cmd for non-test .go files.
func collectFiles(root string) ([]string, error) {
	var roots []string
	for _, sub := range []string{"pkg", "cmd"} {
		p := filepath.Join(root, sub)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			roots = append(roots, sub)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	var files []string
	for _, sub := range roots {
		absSub := filepath.Join(root, sub)
		err := filepath.WalkDir(absSub, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || (path != absSub && strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), "_test.go") {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func readModulePath(root string) string {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

func parseImports(file *ast.File) (osAlias string, imports map[string]string) {
	imports = map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		var local string
		if imp.Name != nil {
			if imp.Name.Name == "_" {
				continue
			}
			local = imp.Name.Name
		} else {
			if i := strings.LastIndex(path, "/"); i >= 0 {
				local = path[i+1:]
			} else {
				local = path
			}
		}
		if path == "os" {
			osAlias = local
		}
		if local != "." {
			imports[local] = path
		}
	}
	return osAlias, imports
}

func (idx *sigIndex) resolveImportPkg(impPath, modPath string) string {
	if modPath != "" && strings.HasPrefix(impPath, modPath+"/") {
		rel := strings.TrimPrefix(impPath, modPath+"/")
		if idx.pkgKeys[rel] {
			return rel
		}
	}
	// Fallback for fixtures without go.mod or with custom module paths:
	// match the longest suffix against scanned package directories.
	parts := strings.Split(impPath, "/")
	for i := range parts {
		cand := strings.Join(parts[i:], "/")
		if idx.pkgKeys[cand] {
			return cand
		}
	}
	return ""
}

func isRootTypeExpr(expr ast.Expr, osAlias string, rootTypes map[string]bool) bool {
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		default:
			goto check
		}
	}
check:
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		return ok && osAlias != "" && osAlias != "." && id.Name == osAlias && e.Sel.Name == "Root"
	case *ast.Ident:
		if osAlias == "." && e.Name == "Root" {
			return true
		}
		return rootTypes[e.Name]
	}
	return false
}

func (idx *sigIndex) resolveType(expr ast.Expr, pf *parsedFile, modPath string) valInfo {
	if expr == nil {
		return valInfo{}
	}
	if isRootTypeExpr(expr, pf.osAlias, idx.rootTypes[pf.pkgKey]) {
		return valInfo{isRoot: true}
	}
	for {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		default:
			goto check
		}
	}
check:
	switch e := expr.(type) {
	case *ast.Ident:
		return valInfo{tr: typeRef{pkg: pf.pkgKey, name: e.Name}}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if impPath, ok := pf.imports[id.Name]; ok {
				if pkgKey := idx.resolveImportPkg(impPath, modPath); pkgKey != "" {
					if idx.rootTypes[pkgKey][e.Sel.Name] {
						return valInfo{isRoot: true}
					}
					return valInfo{tr: typeRef{pkg: pkgKey, name: e.Sel.Name}}
				}
			}
		}
	}
	return valInfo{}
}

func (idx *sigIndex) returnSlots(results *ast.FieldList, pf *parsedFile, modPath string) []valInfo {
	if results == nil {
		return nil
	}
	var slots []valInfo
	for _, field := range results.List {
		vi := idx.resolveType(field.Type, pf, modPath)
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			slots = append(slots, vi)
		}
	}
	return slots
}

func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		default:
			if id, ok := expr.(*ast.Ident); ok {
				return id.Name
			}
			return ""
		}
	}
}

func (idx *sigIndex) indexFiles(files []*parsedFile, modPath string) {
	// Sub-pass 1a: collect package keys and local type aliases to os.Root.
	for _, pf := range files {
		idx.pkgKeys[pf.pkgKey] = true
		if idx.rootTypes[pf.pkgKey] == nil {
			idx.rootTypes[pf.pkgKey] = map[string]bool{}
		}
		for _, decl := range pf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if isRootTypeExpr(ts.Type, pf.osAlias, idx.rootTypes[pf.pkgKey]) {
					idx.rootTypes[pf.pkgKey][ts.Name.Name] = true
				}
			}
		}
	}

	// Sub-pass 1b: collect functions, methods, struct fields, and package vars.
	for _, pf := range files {
		pkg := pf.pkgKey
		if idx.pkgFuncs[pkg] == nil {
			idx.pkgFuncs[pkg] = map[string][]valInfo{}
			idx.pkgMethods[pkg] = map[string]map[string][]valInfo{}
			idx.structFields[pkg] = map[string]map[string]valInfo{}
			idx.pkgVars[pkg] = map[string]valInfo{}
		}
		for _, decl := range pf.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				slots := idx.returnSlots(d.Type.Results, pf, modPath)
				if len(slots) == 0 {
					continue
				}
				if d.Recv == nil {
					idx.pkgFuncs[pkg][d.Name.Name] = slots
				} else if recvName := receiverTypeName(d.Recv); recvName != "" {
					if idx.pkgMethods[pkg][recvName] == nil {
						idx.pkgMethods[pkg][recvName] = map[string][]valInfo{}
					}
					idx.pkgMethods[pkg][recvName][d.Name.Name] = slots
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok || st.Fields == nil {
							continue
						}
						if idx.structFields[pkg][ts.Name.Name] == nil {
							idx.structFields[pkg][ts.Name.Name] = map[string]valInfo{}
						}
						for _, f := range st.Fields.List {
							vi := idx.resolveType(f.Type, pf, modPath)
							if len(f.Names) == 0 {
								// Embedded field: field name is the type's base name.
								if vi.tr.name != "" {
									idx.structFields[pkg][ts.Name.Name][vi.tr.name] = vi
								}
								continue
							}
							for _, name := range f.Names {
								idx.structFields[pkg][ts.Name.Name][name.Name] = vi
							}
						}
					}
				case token.VAR:
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok || vs.Type == nil {
							continue
						}
						vi := idx.resolveType(vs.Type, pf, modPath)
						for _, name := range vs.Names {
							idx.pkgVars[pkg][name.Name] = vi
						}
					}
				}
			}
		}
	}
}

type varScope struct {
	vars   map[string]valInfo
	parent *varScope
}

func newScope(parent *varScope) *varScope {
	return &varScope{vars: map[string]valInfo{}, parent: parent}
}

func (s *varScope) lookup(name string) (valInfo, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if vi, ok := cur.vars[name]; ok {
			return vi, true
		}
	}
	return valInfo{}, false
}

func (s *varScope) set(name string, vi valInfo) {
	if name == "" || name == "_" {
		return
	}
	s.vars[name] = vi
}

func (idx *sigIndex) evalExpr(expr ast.Expr, scope *varScope, pf *parsedFile, modPath string) (valInfo, []valInfo) {
	if expr == nil {
		return valInfo{}, nil
	}
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return idx.evalExpr(e.X, scope, pf, modPath)
	case *ast.UnaryExpr:
		return idx.evalExpr(e.X, scope, pf, modPath)
	case *ast.StarExpr:
		return idx.evalExpr(e.X, scope, pf, modPath)
	case *ast.Ident:
		if vi, ok := scope.lookup(e.Name); ok {
			return vi, nil
		}
		if vi, ok := idx.pkgVars[pf.pkgKey][e.Name]; ok {
			return vi, nil
		}
		return valInfo{}, nil
	case *ast.CompositeLit:
		return idx.resolveType(e.Type, pf, modPath), nil
	case *ast.TypeAssertExpr:
		vi := idx.resolveType(e.Type, pf, modPath)
		return vi, []valInfo{vi, {}}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if impPath, ok := pf.imports[id.Name]; ok {
				if pkgKey := idx.resolveImportPkg(impPath, modPath); pkgKey != "" {
					if vi, ok := idx.pkgVars[pkgKey][e.Sel.Name]; ok {
						return vi, nil
					}
				}
				return valInfo{}, nil
			}
		}
		xvi, _ := idx.evalExpr(e.X, scope, pf, modPath)
		if xvi.tr.name != "" {
			if fields := idx.structFields[xvi.tr.pkg][xvi.tr.name]; fields != nil {
				if fvi, ok := fields[e.Sel.Name]; ok {
					return fvi, nil
				}
			}
		}
		return valInfo{}, nil
	case *ast.CallExpr:
		slots := idx.evalCallSlots(e, scope, pf, modPath)
		if len(slots) > 0 {
			return slots[0], slots
		}
		return valInfo{}, nil
	}
	return valInfo{}, nil
}

func (idx *sigIndex) evalCallSlots(call *ast.CallExpr, scope *varScope, pf *parsedFile, modPath string) []valInfo {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if pf.osAlias == "." && fun.Name == "OpenRoot" {
			return []valInfo{{isRoot: true}, {}}
		}
		if slots := idx.pkgFuncs[pf.pkgKey][fun.Name]; len(slots) > 0 {
			return slots
		}
	case *ast.SelectorExpr:
		// Package-qualified call: os.OpenRoot(...) or pkg.Func(...)
		if id, ok := fun.X.(*ast.Ident); ok {
			if pf.osAlias != "" && pf.osAlias != "." && id.Name == pf.osAlias && fun.Sel.Name == "OpenRoot" {
				return []valInfo{{isRoot: true}, {}}
			}
			if impPath, ok := pf.imports[id.Name]; ok {
				if pkgKey := idx.resolveImportPkg(impPath, modPath); pkgKey != "" {
					if slots := idx.pkgFuncs[pkgKey][fun.Sel.Name]; len(slots) > 0 {
						return slots
					}
				}
				return nil
			}
		}
		// Method call on receiver: r.OpenRoot(...) or resolved.Open(...)
		recvInfo, _ := idx.evalExpr(fun.X, scope, pf, modPath)
		if recvInfo.isRoot && fun.Sel.Name == "OpenRoot" {
			return []valInfo{{isRoot: true}, {}}
		}
		if recvInfo.tr.name != "" {
			if methods := idx.pkgMethods[recvInfo.tr.pkg][recvInfo.tr.name]; methods != nil {
				if slots := methods[fun.Sel.Name]; len(slots) > 0 {
					return slots
				}
			}
		}
	}
	return nil
}

func (idx *sigIndex) bindFieldList(fl *ast.FieldList, scope *varScope, pf *parsedFile, modPath string) {
	if fl == nil {
		return
	}
	for _, field := range fl.List {
		vi := idx.resolveType(field.Type, pf, modPath)
		for _, name := range field.Names {
			scope.set(name.Name, vi)
		}
	}
}

func (idx *sigIndex) bindValues(lhs []ast.Expr, rhs []ast.Expr, scope *varScope, pf *parsedFile, modPath string) {
	if len(rhs) == 1 && len(lhs) > 1 {
		_, slots := idx.evalExpr(rhs[0], scope, pf, modPath)
		for i, l := range lhs {
			id, ok := l.(*ast.Ident)
			if !ok {
				continue
			}
			if i < len(slots) {
				scope.set(id.Name, slots[i])
			} else {
				scope.set(id.Name, valInfo{})
			}
		}
		return
	}
	for i := 0; i < len(lhs) && i < len(rhs); i++ {
		id, ok := lhs[i].(*ast.Ident)
		if !ok {
			continue
		}
		vi, _ := idx.evalExpr(rhs[i], scope, pf, modPath)
		scope.set(id.Name, vi)
	}
}

func (idx *sigIndex) scanNode(node ast.Node, scope *varScope, pf *parsedFile, modPath string, count func()) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *ast.BlockStmt:
		sub := newScope(scope)
		for _, st := range n.List {
			idx.scanNode(st, sub, pf, modPath, count)
		}
	case *ast.IfStmt:
		sub := newScope(scope)
		idx.scanNode(n.Init, sub, pf, modPath, count)
		idx.scanNode(n.Cond, sub, pf, modPath, count)
		idx.scanNode(n.Body, sub, pf, modPath, count)
		idx.scanNode(n.Else, sub, pf, modPath, count)
	case *ast.ForStmt:
		sub := newScope(scope)
		idx.scanNode(n.Init, sub, pf, modPath, count)
		idx.scanNode(n.Cond, sub, pf, modPath, count)
		idx.scanNode(n.Post, sub, pf, modPath, count)
		idx.scanNode(n.Body, sub, pf, modPath, count)
	case *ast.RangeStmt:
		sub := newScope(scope)
		idx.scanNode(n.X, sub, pf, modPath, count)
		idx.scanNode(n.Body, sub, pf, modPath, count)
	case *ast.SwitchStmt:
		sub := newScope(scope)
		idx.scanNode(n.Init, sub, pf, modPath, count)
		idx.scanNode(n.Tag, sub, pf, modPath, count)
		idx.scanNode(n.Body, sub, pf, modPath, count)
	case *ast.TypeSwitchStmt:
		sub := newScope(scope)
		idx.scanNode(n.Init, sub, pf, modPath, count)
		idx.scanNode(n.Assign, sub, pf, modPath, count)
		idx.scanNode(n.Body, sub, pf, modPath, count)
	case *ast.CaseClause:
		sub := newScope(scope)
		for _, e := range n.List {
			idx.scanNode(e, sub, pf, modPath, count)
		}
		for _, st := range n.Body {
			idx.scanNode(st, sub, pf, modPath, count)
		}
	case *ast.AssignStmt:
		// Scan RHS first (so e.g. `r, err := r.Open("x")` evaluates old r),
		// then bind LHS for subsequent statements.
		for _, e := range n.Rhs {
			idx.scanNode(e, scope, pf, modPath, count)
		}
		for _, e := range n.Lhs {
			if _, ok := e.(*ast.Ident); !ok {
				idx.scanNode(e, scope, pf, modPath, count)
			}
		}
		idx.bindValues(n.Lhs, n.Rhs, scope, pf, modPath)
	case *ast.DeclStmt:
		idx.scanNode(n.Decl, scope, pf, modPath, count)
	case *ast.GenDecl:
		if n.Tok != token.VAR && n.Tok != token.CONST {
			return
		}
		for _, spec := range n.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, v := range vs.Values {
				idx.scanNode(v, scope, pf, modPath, count)
			}
			if vs.Type != nil {
				vi := idx.resolveType(vs.Type, pf, modPath)
				for _, name := range vs.Names {
					scope.set(name.Name, vi)
				}
			} else if len(vs.Values) > 0 {
				lhs := make([]ast.Expr, len(vs.Names))
				for i, name := range vs.Names {
					lhs[i] = name
				}
				idx.bindValues(lhs, vs.Values, scope, pf, modPath)
			}
		}
	case *ast.FuncLit:
		sub := newScope(scope)
		idx.bindFieldList(n.Type.Params, sub, pf, modPath)
		idx.bindFieldList(n.Type.Results, sub, pf, modPath)
		idx.scanNode(n.Body, sub, pf, modPath, count)
	case *ast.SelectorExpr:
		// First inspect receiver/X in case of chained expressions like
		// r.OpenRoot("sub").Open("x") or get().Open("x").
		idx.scanNode(n.X, scope, pf, modPath, count)

		// Rule 1: os.OpenInRoot (via any import alias of "os").
		if n.Sel.Name == "OpenInRoot" {
			if id, ok := n.X.(*ast.Ident); ok && pf.osAlias != "" && pf.osAlias != "." && id.Name == pf.osAlias {
				count()
				return
			}
		}
		// Rule 2: Method expression (*os.Root).Open / (os.Root).OpenFile / (*os.Root).Create.
		if targetMethods[n.Sel.Name] && isRootTypeExpr(n.X, pf.osAlias, idx.rootTypes[pf.pkgKey]) {
			count()
			return
		}
		// Rule 3: Method call or method value on an *os.Root receiver expression.
		if targetMethods[n.Sel.Name] {
			xvi, _ := idx.evalExpr(n.X, scope, pf, modPath)
			if xvi.isRoot {
				count()
				return
			}
		}
	case *ast.Ident:
		// Dot-imported OpenInRoot from `import . "os"`.
		if pf.osAlias == "." && n.Name == "OpenInRoot" {
			count()
		}
	default:
		// Generic traversal for remaining expression/statement nodes.
		ast.Inspect(node, func(child ast.Node) bool {
			if child == nil || child == node {
				return true
			}
			switch child.(type) {
			case *ast.BlockStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
				*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.CaseClause,
				*ast.AssignStmt, *ast.DeclStmt, *ast.GenDecl, *ast.FuncLit,
				*ast.SelectorExpr, *ast.Ident:
				idx.scanNode(child, scope, pf, modPath, count)
				return false
			}
			return true
		})
	}
}

// countCalls scans <root>/pkg and <root>/cmd and returns per-(file|func) counts.
func countCalls(root string) (map[string]int, error) {
	relFiles, err := collectFiles(root)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	parsed := make([]*parsedFile, 0, len(relFiles))
	for _, rel := range relFiles {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		file, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", rel, err)
		}
		osAlias, imports := parseImports(file)
		parsed = append(parsed, &parsedFile{
			relPath: rel,
			pkgKey:  filepath.ToSlash(filepath.Dir(rel)),
			file:    file,
			osAlias: osAlias,
			imports: imports,
		})
	}

	modPath := readModulePath(root)
	idx := newSigIndex()
	idx.indexFiles(parsed, modPath)

	counts := map[string]int{}
	for _, pf := range parsed {
		for _, decl := range pf.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				fnName := d.Name.Name
				key := pf.relPath + "|" + fnName
				scope := newScope(nil)
				idx.bindFieldList(d.Recv, scope, pf, modPath)
				idx.bindFieldList(d.Type.Params, scope, pf, modPath)
				idx.bindFieldList(d.Type.Results, scope, pf, modPath)
				idx.scanNode(d.Body, scope, pf, modPath, func() {
					counts[key]++
				})
			case *ast.GenDecl:
				if d.Tok != token.VAR && d.Tok != token.CONST {
					continue
				}
				key := pf.relPath + "|-"
				scope := newScope(nil)
				idx.scanNode(d, scope, pf, modPath, func() {
					counts[key]++
				})
			}
		}
	}
	return counts, nil
}

func parseAllowlist(path string) (map[string]int, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	expected := map[string]int{}
	var parseErrs []string
	sc := bufio.NewScanner(f)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: allowlist parse error at line %d: want \"<file> | <function> | <count> | <reason>\", got: %s", toolName, lineNo, line))
			continue
		}
		file := strings.TrimSpace(parts[0])
		fn := strings.TrimSpace(parts[1])
		cntStr := strings.TrimSpace(parts[2])
		reason := strings.TrimSpace(parts[3])
		cnt, err := strconv.Atoi(cntStr)
		if file == "" || fn == "" || err != nil || cnt < 0 || reason == "" {
			parseErrs = append(parseErrs, fmt.Sprintf("%s: allowlist parse error at line %d: want \"<file> | <function> | <count> | <reason>\", got: %s", toolName, lineNo, line))
			continue
		}
		key := file + "|" + fn
		expected[key] += cnt
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return expected, parseErrs, nil
}

func scan(root, allowlist, label string) (int, string, string) {
	var stdout, stderr strings.Builder
	pkgDir := filepath.Join(root, "pkg")
	if info, err := os.Stat(pkgDir); err != nil || !info.IsDir() {
		fmt.Fprintf(&stderr, "%s: scan root %s is missing — NOTHING WAS ANALYSED\n", toolName, pkgDir)
		return 4, stdout.String(), stderr.String()
	}
	if info, err := os.Stat(allowlist); err != nil || info.IsDir() {
		fmt.Fprintf(&stderr, "%s: allowlist %s is missing — NOTHING WAS ANALYSED\n", toolName, allowlist)
		return 4, stdout.String(), stderr.String()
	}

	actual, err := countCalls(root)
	if err != nil {
		fmt.Fprintf(&stderr, "%s: could not analyse: %v\n", toolName, err)
		return 3, stdout.String(), stderr.String()
	}
	if len(actual) == 0 {
		fmt.Fprintf(&stderr, "%s: no (*os.Root).Open/OpenFile/Create or os.OpenInRoot call found under %s — NOTHING WAS ANALYSED\n", toolName, root)
		return 4, stdout.String(), stderr.String()
	}

	expected, parseErrs, err := parseAllowlist(allowlist)
	if err != nil {
		fmt.Fprintf(&stderr, "%s: could not read allowlist %s: %v\n", toolName, allowlist, err)
		return 3, stdout.String(), stderr.String()
	}
	if len(parseErrs) > 0 {
		for _, pe := range parseErrs {
			fmt.Fprintln(&stderr, pe)
		}
		fmt.Fprintf(&stderr, "Fix the malformed line(s) in %s.\n", allowlist)
		return 1, stdout.String(), stderr.String()
	}

	keySet := map[string]bool{}
	for k := range actual {
		keySet[k] = true
	}
	for k := range expected {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var over, under []string
	total := 0
	for _, k := range keys {
		a := actual[k]
		e := expected[k]
		total += a
		if a > e {
			over = append(over, fmt.Sprintf("  %s: %d (allowlist %d)", k, a, e))
		} else if a < e {
			under = append(under, fmt.Sprintf("  %s: %d (allowlist %d)", k, a, e))
		}
	}

	if label == "" {
		label = root
	}
	fmt.Fprintf(&stderr, "%s: analysed %s, %d direct (*os.Root).Open/OpenFile/Create or os.OpenInRoot call(s)\n", toolName, label, total)

	rc := 0
	if len(over) > 0 {
		fmt.Fprintf(&stderr, "%s: direct (*os.Root).Open/OpenFile/Create or os.OpenInRoot call(s) outside the allowlist (file|function):\n", toolName)
		for _, line := range over {
			fmt.Fprintln(&stderr, line)
		}
		fmt.Fprintln(&stderr)
		fmt.Fprintln(&stderr, "Do not call (*os.Root).Open, OpenFile, Create, or os.OpenInRoot directly on")
		fmt.Fprintln(&stderr, "untrusted paths. Use pkg/util/rootopen (OpenRegularForRead, OpenFileForWrite,")
		fmt.Fprintln(&stderr, "OpenDir, MkdirAll, or OpenAt) instead. If a direct call is genuinely safe (for")
		fmt.Fprintln(&stderr, "example, opening \".\" on an already-verified root or a non-unix fallback), add")
		fmt.Fprintln(&stderr, "an entry with a one-line reason to hack/root-open-allowlist.txt.")
		rc = 1
	}
	if len(under) > 0 {
		fmt.Fprintf(&stderr, "%s: allowlist is stale (fewer calls than recorded, file|function):\n", toolName)
		for _, line := range under {
			fmt.Fprintln(&stderr, line)
		}
		fmt.Fprintf(&stderr, "Lower the count, or delete the entry, in %s.\n", allowlist)
		rc = 1
	}
	if rc == 0 {
		fmt.Fprintf(&stdout, "%s: no (*os.Root).Open/OpenFile/Create or os.OpenInRoot call outside the allowlist\n", toolName)
	}
	return rc, stdout.String(), stderr.String()
}

func main() {
	root := flag.String("root", ".", "repository root containing pkg/ and cmd/")
	allowlist := flag.String("allowlist", "hack/root-open-allowlist.txt", "path to allowlist file")
	label := flag.String("label", "", "provenance label (commit sha)")
	flag.Parse()

	rc, out, errOut := scan(*root, *allowlist, *label)
	if out != "" {
		_, _ = fmt.Fprint(os.Stdout, out)
	}
	if errOut != "" {
		_, _ = fmt.Fprint(os.Stderr, errOut)
	}
	os.Exit(rc)
}
