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
	"go/format"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// analysis carries the state of one move computation.
type analysis struct {
	cfg       *Config
	fset      *token.FileSet
	mod       *moduleInfo
	srcName   string // source package name
	dstImport string
	files     []*srcFile // every Go file in the source directory
	byPath    map[string]*srcFile
	checked   []*srcFile // files of the type-checked unit (package + in-package tests)
	pkg       *types.Package
	info      *types.Info
	plan      *Plan

	uses []identUse // every Defs/Uses entry of the checked unit, sorted by position

	// forward[obj] lists the remaining-side files that use a moved
	// package-level object (by type information or, for files excluded by
	// build constraints, by name).
	forward map[types.Object][]*srcFile
	// pkgRename maps moved package-level objects to their exported names.
	pkgRename map[types.Object]string
	// memberRename maps field and method objects to their exported names.
	memberRename map[types.Object]string
	// embedFollow lists embedded fields whose name follows a renamed type.
	embedFollow map[types.Object]string

	// xtestSels lists, per moved external test file, the selectors on the
	// source import that must be re-qualified with the target import.
	xtestSels map[*srcFile][]*ast.SelectorExpr
	// excludedCallFuns marks call-position identifiers of the excluded file
	// being scanned.
	excludedCallFuns map[*ast.Ident]bool
	// callFuns marks identifiers in call position in the checked files.
	// Staying uses of moved funcs outside call position are func values; they
	// are rewritten to the target (like vars) so func identity is preserved.
	callFuns map[*ast.Ident]bool
	// reachMemo and funcDecls back the static call graph (reach).
	reachMemo map[*types.Func]*reachInfo
	funcDecls map[*types.Func]*ast.FuncDecl
	// modPkgs caches the module package listing.
	modPkgs []listedPackage
	// hazards caches wrapperHazard results.
	hazards map[*types.Func]string
	// assets are non-Go files or directories (base names) moved verbatim.
	assets []string

	edits map[*srcFile]*fileEdits
}

type identUse struct {
	id   *ast.Ident
	obj  types.Object
	file *srcFile
	def  bool
}

func (a *analysis) rel(path string) string {
	r, err := filepath.Rel(a.mod.ModDir, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

func (a *analysis) posOf(pos token.Pos) string {
	p := a.fset.Position(pos)
	return fmt.Sprintf("%s:%d", a.rel(p.Filename), p.Line)
}

func (a *analysis) fileOf(pos token.Pos) *srcFile {
	if !pos.IsValid() {
		return nil
	}
	return a.byPath[a.fset.Position(pos).Filename]
}

// home returns the source file that declares obj, or nil when obj is not
// declared in the source package.
func (a *analysis) home(obj types.Object) *srcFile {
	if obj == nil || obj.Pkg() != a.pkg {
		return nil
	}
	return a.fileOf(obj.Pos())
}

func (a *analysis) isPkgLevel(obj types.Object) bool {
	return obj.Parent() == a.pkg.Scope()
}

// origin maps instantiated generic members to their declarations.
func origin(obj types.Object) types.Object {
	switch o := obj.(type) {
	case *types.Func:
		return o.Origin()
	case *types.Var:
		return o.Origin()
	}
	return obj
}

func isMember(obj types.Object) bool {
	switch o := obj.(type) {
	case *types.Func:
		return o.Signature().Recv() != nil
	case *types.Var:
		return o.IsField()
	}
	return false
}

func kindOf(obj types.Object) string {
	switch o := obj.(type) {
	case *types.TypeName:
		return "type"
	case *types.Const:
		return "const"
	case *types.Var:
		if o.IsField() {
			return "field"
		}
		return "var"
	case *types.Func:
		if o.Signature().Recv() != nil {
			return "method"
		}
		return "func"
	}
	return "object"
}

// exportName returns the exported form of an identifier, or "" when it
// cannot be exported by upper-casing its first letter.
func exportName(name string) string {
	r, size := utf8.DecodeRuneInString(name)
	if !unicode.IsLower(r) {
		return ""
	}
	up := unicode.ToUpper(r)
	if up == r || !unicode.IsUpper(up) {
		return ""
	}
	return string(up) + name[size:]
}

// ownerName describes the receiver or owner of a member for messages.
func ownerName(obj types.Object) string {
	if f, ok := obj.(*types.Func); ok && f.Signature().Recv() != nil {
		t := f.Signature().Recv().Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		switch t := t.(type) {
		case *types.Named:
			return t.Obj().Name()
		case *types.Alias:
			return t.Obj().Name()
		}
		return "interface"
	}
	return ""
}

// recvTypeName returns the named receiver base type of a concrete method.
func recvTypeName(f *types.Func) *types.TypeName {
	t := f.Signature().Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	t = types.Unalias(t)
	if n, ok := t.(*types.Named); ok {
		if _, isIface := n.Underlying().(*types.Interface); isIface {
			return nil
		}
		return n.Origin().Obj()
	}
	return nil
}

// Methods whose exported names have well-known dynamic meaning (fmt, encoding,
// io, database/sql, net/http, errors, sort, log/slog). Exporting a method to
// one of these names can change behaviour without a compile error.
var dynamicMethodNames = map[string]bool{
	"String": true, "GoString": true, "Format": true, "Error": true, "Unwrap": true,
	"Is": true, "As": true, "MarshalJSON": true, "UnmarshalJSON": true, "MarshalText": true,
	"UnmarshalText": true, "MarshalBinary": true, "UnmarshalBinary": true, "MarshalYAML": true,
	"UnmarshalYAML": true, "GobEncode": true, "GobDecode": true, "Scan": true, "Value": true,
	"Read": true, "Write": true, "Close": true, "ReadFrom": true, "WriteTo": true,
	"ReadAt": true, "WriteAt": true, "Seek": true, "ServeHTTP": true, "Len": true,
	"Less": true, "Swap": true, "LogValue": true, "Timeout": true, "Temporary": true,
	"Cause": true, "Reset": true, "ProtoMessage": true, "IsZero": true, "Equal": true,
	"Flush": true, "Hijack": true, "AppendText": true, "AppendBinary": true,
	"MarshalJSONTo": true, "UnmarshalJSONFrom": true,
}

func analyze(cfg *Config) (*analysis, error) {
	a := &analysis{
		cfg:          cfg,
		fset:         token.NewFileSet(),
		byPath:       map[string]*srcFile{},
		forward:      map[types.Object][]*srcFile{},
		pkgRename:    map[types.Object]string{},
		memberRename: map[types.Object]string{},
		embedFollow:  map[types.Object]string{},
		edits:        map[*srcFile]*fileEdits{},
		xtestSels:    map[*srcFile][]*ast.SelectorExpr{},
		plan:         &Plan{},
	}
	var err error
	a.mod, err = resolveDir(cfg.SrcDir, cfg.Tags)
	if err != nil {
		return nil, err
	}
	a.srcName = a.mod.PkgName
	if a.srcName == "main" {
		return nil, fmt.Errorf("source package is package main; it cannot import the target")
	}
	relDst, err := filepath.Rel(a.mod.ModDir, cfg.DstDir)
	if err != nil || strings.HasPrefix(relDst, "..") {
		return nil, fmt.Errorf("target %s is outside module %s", cfg.DstDir, a.mod.ModDir)
	}
	a.dstImport = a.mod.ModPath + "/" + filepath.ToSlash(relDst)
	if !token.IsIdentifier(cfg.PkgName) || cfg.PkgName == "main" {
		return nil, fmt.Errorf("invalid target package name %q", cfg.PkgName)
	}
	a.plan.SrcImport, a.plan.DstImport, a.plan.PkgName = a.mod.ImportPath, a.dstImport, cfg.PkgName

	ctx := buildContext(cfg.Tags)
	a.files, err = readDir(a.fset, cfg.SrcDir, ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range a.files {
		a.byPath[f.Path] = f
	}
	if err := a.selectFiles(); err != nil {
		return nil, err
	}
	dstFiles, err := readDir(token.NewFileSet(), cfg.DstDir, ctx)
	if err != nil {
		return nil, err
	}
	if len(dstFiles) > 0 {
		return nil, fmt.Errorf("target directory %s already contains Go files; moving into an existing package is not supported", a.rel(cfg.DstDir))
	}
	if len(a.plan.Errors) > 0 {
		a.plan.normalize()
		return a, nil
	}

	for _, f := range a.files {
		if f.Included && !f.XTest {
			a.checked = append(a.checked, f)
		}
	}
	imp, err := newExportImporter(a.fset, cfg.SrcDir, cfg.Tags, importsOf(a.checked, a.mod.ImportPath))
	if err != nil {
		return nil, err
	}
	a.pkg, a.info, err = typeCheck(a.fset, a.mod.ImportPath, a.checked, imp, a.mod.GoVersion)
	if err != nil {
		return nil, err
	}

	a.collectUses()
	a.checkMethods()
	a.checkReferences()
	a.scanExcluded()
	a.planMembers()
	a.planPackageRenames()
	a.checkXTests()
	a.scanModule()
	if err := a.renderAliases(); err != nil {
		return nil, err
	}
	a.checkEmbeddedExports()
	a.checkPkgCollisions()
	a.safetyFindings()
	a.checkTestMain()
	if cfg.Strict {
		for _, f := range a.plan.Findings {
			if f.Level == levelHigh {
				a.plan.errorf("%s: -strict: %s: %s", f.Pos, f.Category, f.Msg)
			}
		}
	}
	if len(a.plan.Errors) == 0 {
		if err := a.buildEdits(); err != nil {
			return nil, err
		}
	}
	a.plan.normalize()
	return a, nil
}

// selectFiles validates the move set and marks the moved files.
func (a *analysis) selectFiles() error {
	if len(a.cfg.Files) == 0 {
		return fmt.Errorf("no files to move")
	}
	seen := map[string]bool{}
	for _, name := range a.cfg.Files {
		base := filepath.Base(name)
		if seen[base] {
			continue
		}
		seen[base] = true
		f := a.byPath[filepath.Join(a.cfg.SrcDir, base)]
		if f == nil {
			path := filepath.Join(a.cfg.SrcDir, base)
			if _, err := os.Stat(path); err != nil || strings.HasSuffix(base, ".go") {
				return fmt.Errorf("%s: no such file in %s", base, a.rel(a.cfg.SrcDir))
			}
			// A non-Go asset (for example a go:embed file) moves verbatim.
			a.assets = append(a.assets, base)
			a.plan.Moves = append(a.plan.Moves, fileMove{From: a.rel(path), To: a.rel(filepath.Join(a.cfg.DstDir, base))})
			continue
		}
		f.Moved = true
		to := filepath.Join(a.cfg.DstDir, base)
		a.plan.Moves = append(a.plan.Moves, fileMove{From: a.rel(f.Path), To: a.rel(to)})
		switch {
		case f.CGo:
			a.plan.errorf("%s uses cgo (import \"C\"); moving cgo files is not supported", a.rel(f.Path))
		case !f.Included:
			a.plan.errorf("%s is excluded by build constraints under the analysis tags %v; re-run with -tags that include it (moving files of other build configurations is not supported)", a.rel(f.Path), a.cfg.Tags)
		}
	}
	// Assembly and precompiled objects implement Go declarations by package
	// path; they cannot be moved or re-pointed by this tool.
	if entries, err := os.ReadDir(a.cfg.SrcDir); err == nil {
		for _, e := range entries {
			if n := e.Name(); !e.IsDir() && (strings.HasSuffix(n, ".s") || strings.HasSuffix(n, ".syso")) {
				a.plan.errorf("%s: the source package has assembly or object files; moves out of it are not supported (like cgo)", a.rel(filepath.Join(a.cfg.SrcDir, n)))
			}
		}
	}
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body == nil {
				a.plan.errorf("%s: func %s has no body (implemented in assembly or by go:linkname); moving it is not supported", a.posOf(fd.Pos()), fd.Name.Name)
			}
		}
	}
	// Point out source files whose test companion is left behind (and vice versa).
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		var companion string
		if f.IsTest {
			companion = strings.TrimSuffix(f.Name, "_test.go") + ".go"
		} else {
			companion = strings.TrimSuffix(f.Name, ".go") + "_test.go"
		}
		if c := a.byPath[filepath.Join(a.cfg.SrcDir, companion)]; c != nil && !c.Moved {
			a.plan.add(levelInfo, "test companion not in the move set", a.rel(c.Path), "%s moves but %s stays", f.Name, c.Name)
		}
	}
	return nil
}

// collectCallFuns records every identifier used as the function of a call
// (including explicit instantiations f[T](...)) in the checked files.
func (a *analysis) collectCallFuns() {
	a.callFuns = map[*ast.Ident]bool{}
	for _, f := range a.checked {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun := ast.Unparen(call.Fun)
			switch ix := fun.(type) {
			case *ast.IndexExpr:
				fun = ix.X
			case *ast.IndexListExpr:
				fun = ix.X
			}
			switch fn := fun.(type) {
			case *ast.Ident:
				a.callFuns[fn] = true
			case *ast.SelectorExpr:
				a.callFuns[fn.Sel] = true
			}
			return true
		})
	}
}

func (a *analysis) collectUses() {
	a.collectCallFuns()
	for id, obj := range a.info.Defs {
		if obj != nil {
			a.uses = append(a.uses, identUse{id: id, obj: origin(obj), file: a.fileOf(id.Pos()), def: true})
		}
	}
	for id, obj := range a.info.Uses {
		a.uses = append(a.uses, identUse{id: id, obj: origin(obj), file: a.fileOf(id.Pos())})
	}
	sort.Slice(a.uses, func(i, j int) bool {
		if a.uses[i].id.Pos() != a.uses[j].id.Pos() {
			return a.uses[i].id.Pos() < a.uses[j].id.Pos()
		}
		return a.uses[i].def && !a.uses[j].def
	})
}

// checkMethods rejects methods that would end up in a different package from
// their receiver type (Go forbids methods on non-local types).
func (a *analysis) checkMethods() {
	for _, u := range a.uses {
		if !u.def {
			continue
		}
		f, ok := u.obj.(*types.Func)
		if !ok || f.Signature().Recv() == nil || u.file == nil {
			continue
		}
		tn := recvTypeName(f)
		if tn == nil {
			continue
		}
		th := a.home(tn)
		if th == nil || th.Moved == u.file.Moved {
			continue
		}
		if u.file.Moved {
			a.plan.errorf("%s: method %s.%s moves but its receiver type %s stays in %s (declared at %s); Go forbids methods on non-local types - move %s too or keep this file",
				a.posOf(u.id.Pos()), tn.Name(), f.Name(), tn.Name(), a.srcName, a.posOf(tn.Pos()), th.Name)
		} else {
			a.plan.errorf("%s: method %s.%s stays in %s but its receiver type %s moves (declared at %s); Go forbids methods on non-local types - move %s too",
				a.posOf(u.id.Pos()), tn.Name(), f.Name(), a.srcName, tn.Name(), a.posOf(tn.Pos()), u.file.Name)
		}
	}
}

// checkReferences classifies every cross-boundary reference.
func (a *analysis) checkReferences() {
	for _, u := range a.uses {
		if u.def || u.file == nil {
			continue
		}
		h := a.home(u.obj)
		if h == nil || h.Moved == u.file.Moved {
			continue
		}
		if !a.isPkgLevel(u.obj) && !isMember(u.obj) {
			continue
		}
		if u.file.Moved {
			// Backward reference: a moved file uses something that stays.
			switch {
			case h.IsTest:
				a.plan.errorf("%s: moved test file uses %s %s declared in the staying test file %s; the move would separate the test from its helper - move %s too",
					a.posOf(u.id.Pos()), kindOf(u.obj), u.obj.Name(), h.Name, h.Name)
			default:
				a.plan.errorf("%s: moved file uses %s %s, which stays in %s (%s); the target cannot import %s (the alias file makes %s import the target) - move it too, or invert the dependency first",
					a.posOf(u.id.Pos()), kindOf(u.obj), u.obj.Name(), a.srcName, a.posOf(u.obj.Pos()), a.srcName, a.srcName)
			}
			continue
		}
		// Forward reference: a staying file uses something that moves.
		if h.IsTest {
			a.plan.errorf("%s: %s %s is declared in the moved test file %s but used by the staying file %s; test-only symbols cannot be aliased - move %s too or keep %s",
				a.posOf(u.id.Pos()), kindOf(u.obj), u.obj.Name(), h.Name, u.file.Name, u.file.Name, h.Name)
			continue
		}
		if a.isPkgLevel(u.obj) {
			a.forward[u.obj] = append(a.forward[u.obj], u.file)
		}
	}
}

// scanExcluded looks, by name only, at source-directory files that the build
// context excludes: they cannot be type-checked, so any identifier matching a
// moved package-level name is treated as a use (conservatively aliased).
func (a *analysis) scanExcluded() {
	moved := map[string]types.Object{}
	for _, name := range a.pkg.Scope().Names() {
		obj := a.pkg.Scope().Lookup(name)
		if h := a.home(obj); h != nil && h.Moved {
			moved[name] = obj
		}
	}
	members := map[string]bool{}
	methods := map[string]bool{} // unexported methods of moved types and interfaces
	for _, u := range a.uses {
		if u.def && isMember(u.obj) {
			if h := a.home(u.obj); h != nil && h.Moved {
				members[u.obj.Name()] = true
				if _, isFunc := u.obj.(*types.Func); isFunc && !u.obj.Exported() {
					methods[u.obj.Name()] = true
				}
			}
		}
	}
	for _, f := range a.files {
		if f.Included || f.XTest || f.Moved {
			continue
		}
		// Methods declared under other build tags may satisfy a moved
		// interface (or match a moved type's method) dynamically; that cannot
		// be checked without type information for those tags.
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && methods[fd.Name.Name] {
				a.plan.add(levelWarn, "file excluded by build constraints (not type-checked)", a.posOf(fd.Name.Pos()),
					"method %s matches an unexported method of a moved type or interface; interface satisfaction under these tags cannot be checked (an unexported method cannot satisfy an interface across packages) - verify with -tags", fd.Name.Name)
			}
		}
		// Interface method specs (named or anonymous, e.g. in a type
		// assertion v.(interface{ run() string })) that a moved type may have
		// satisfied dynamically.
		ast.Inspect(f.AST, func(n ast.Node) bool {
			it, ok := n.(*ast.InterfaceType)
			if !ok || it.Methods == nil {
				return true
			}
			for _, field := range it.Methods.List {
				for _, name := range field.Names {
					if methods[name.Name] {
						a.plan.add(levelWarn, "file excluded by build constraints (not type-checked)", a.posOf(name.Pos()),
							"interface method %s matches an unexported method of a moved type or interface; a moved type can no longer satisfy it (unexported methods do not cross packages), and renames are not applied here - verify with -tags", name.Name)
					}
				}
			}
			return true
		})
		// Identifiers used as the function of a call (all others that name a
		// moved func are func values, whose identity a wrapper alias changes).
		a.excludedCallFuns = map[*ast.Ident]bool{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				fun := ast.Unparen(call.Fun)
				if ix, ok := fun.(*ast.IndexExpr); ok {
					fun = ix.X
				}
				if ix, ok := fun.(*ast.IndexListExpr); ok {
					fun = ix.X
				}
				if id, ok := fun.(*ast.Ident); ok {
					a.excludedCallFuns[id] = true
				}
			}
			return true
		})
		var hits []string
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if members[n.Sel.Name] && !ast.IsExported(n.Sel.Name) {
					a.plan.add(levelWarn, "file excluded by build constraints (not type-checked)", a.posOf(n.Sel.Pos()),
						"selector .%s matches a member of a moved type; verify it under its build tags", n.Sel.Name)
				}
				ast.Inspect(n.X, func(m ast.Node) bool { return a.excludedIdent(f, m, moved, &hits) })
				return false
			default:
				return a.excludedIdent(f, n, moved, &hits)
			}
		})
		if len(hits) > 0 {
			a.plan.add(levelWarn, "file excluded by build constraints (not type-checked)", a.rel(f.Path),
				"references moved names by name (aliased conservatively, not type-checked): %s - verify with -tags", strings.Join(dedupStrings(sortedCopy(hits)), ", "))
		} else {
			a.plan.add(levelInfo, "file excluded by build constraints (not type-checked)", a.rel(f.Path), "no references to moved names")
		}
	}
}

func (a *analysis) excludedIdent(f *srcFile, n ast.Node, moved map[string]types.Object, hits *[]string) bool {
	if sel, ok := n.(*ast.SelectorExpr); ok {
		ast.Inspect(sel.X, func(m ast.Node) bool { return a.excludedIdent(f, m, moved, hits) })
		return false
	}
	id, ok := n.(*ast.Ident)
	if !ok {
		return true
	}
	obj, ok := moved[id.Name]
	if !ok {
		return true
	}
	*hits = append(*hits, id.Name)
	h := a.home(obj)
	switch {
	case h.IsTest:
		a.plan.errorf("%s: excluded file uses %s, declared in the moved test file %s; test-only symbols cannot be aliased", a.posOf(id.Pos()), id.Name, h.Name)
	case kindOf(obj) == "var":
		a.plan.errorf("%s: excluded file (not type-checked) uses moved var %s; vars cannot be aliased and the reference cannot be rewritten safely - re-run with -tags covering this file", a.posOf(id.Pos()), id.Name)
	default:
		if fn, isFunc := obj.(*types.Func); isFunc && !a.excludedCallFuns[id] && (!fn.Exported() || isGenericFunc(fn)) {
			a.plan.add(levelWarn, "moved func used as a value through a wrapper alias", a.posOf(id.Pos()),
				"%s is used as a func value in a file excluded by build constraints; through the wrapper alias its identity (reflect Pointer, runtime.FuncForPC name) differs from %s.%s - verify with -tags",
				id.Name, a.cfg.PkgName, exportedForm(obj))
		}
		a.forward[obj] = append(a.forward[obj], f)
	}
	return true
}

// exportedForm is the name obj will have in the target if it gets exported.
func exportedForm(obj types.Object) string {
	if n := exportName(obj.Name()); n != "" {
		return n
	}
	return obj.Name()
}

// checkEmbeddedExports treats every embedded field whose name changes with
// its renamed type as a field export: the field becomes exported, which
// changes encoding/json, gob, cmp, text/template, %+v and reflect behaviour.
func (a *analysis) checkEmbeddedExports() {
	var fields []types.Object
	for v := range a.embedFollow {
		fields = append(fields, v)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Pos() < fields[j].Pos() })
	for _, v := range fields {
		owner := a.fieldOwner(v)
		newName := a.embedFollow[v]
		if v.Exported() {
			continue
		}
		a.plan.Renames = append(a.plan.Renames, renameEntry{Kind: "field", Owner: owner, Old: v.Name(), New: newName, Pos: a.posOf(v.Pos())})
		if !a.cfg.AllowFieldExport {
			a.plan.errorf("%s: embedded field %s.%s becomes the exported field %s because its type is exported; exporting a field changes reflection, encoding (encoding/json, gob, cmp), text/template and %%+v behaviour - restructure first, or pass -allow-field-export and review the HIGH finding",
				a.posOf(v.Pos()), owner, v.Name(), newName)
			continue
		}
		a.plan.add(levelHigh, "exported struct field (reflection/encoding visibility changes)", a.posOf(v.Pos()),
			"embedded %s.%s -> %s (follows the exported type): reflection, encoding, templates and %%+v now see this field (types in other packages that embed %s are not checked for shadowing)", owner, v.Name(), newName, owner)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// unionFind groups method objects that must keep the same name.
type unionFind map[types.Object]types.Object

func (u unionFind) find(x types.Object) types.Object {
	p, ok := u[x]
	if !ok || p == x {
		return x
	}
	r := u.find(p)
	u[x] = r
	return r
}

func (u unionFind) union(x, y types.Object) {
	rx, ry := u.find(x), u.find(y)
	if rx != ry {
		u[rx] = ry
	}
}

// planMembers decides which fields and methods must be exported: unexported
// members used across the boundary, plus every method that shares a name
// with them through interface satisfaction inside the source package.
func (a *analysis) planMembers() {
	uf := unionFind{}
	// Interfaces with unexported methods of the source package, keyed by AST
	// position for determinism.
	type ifaceAt struct {
		pos   token.Pos
		iface *types.Interface
	}
	var ifaces []ifaceAt
	for _, f := range a.checked {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			it, ok := n.(*ast.InterfaceType)
			if !ok {
				return true
			}
			tv, ok := a.info.Types[it]
			if !ok {
				return true
			}
			iface, ok := tv.Type.Underlying().(*types.Interface)
			if !ok {
				return true
			}
			for i := 0; i < iface.NumMethods(); i++ {
				m := iface.Method(i)
				if !m.Exported() && m.Pkg() == a.pkg {
					ifaces = append(ifaces, ifaceAt{it.Pos(), iface})
					break
				}
			}
			return true
		})
	}
	var named []*types.Named
	for _, name := range a.pkg.Scope().Names() {
		tn, ok := a.pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		n, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		if _, isIface := n.Underlying().(*types.Interface); isIface {
			continue
		}
		if n.TypeParams().Len() == 0 {
			named = append(named, n)
			continue
		}
		// Generic types: types.Implements is unspecified for uninstantiated
		// generic types, and any instantiation may satisfy an interface
		// dynamically (e.g. box[int] returned as any and asserted to
		// interface{ run() string }). Group conservatively by name: each
		// unexported method is tied to every unexported interface method of
		// the same name in the package.
		for i := 0; i < n.NumMethods(); i++ {
			m := n.Method(i)
			if m.Exported() {
				continue
			}
			for _, ia := range ifaces {
				for j := 0; j < ia.iface.NumMethods(); j++ {
					if im := ia.iface.Method(j); im.Name() == m.Name() && !im.Exported() {
						uf.union(origin(im), origin(m))
					}
				}
			}
		}
	}
	for _, ia := range ifaces {
		for _, n := range named {
			var impl types.Type
			if types.Implements(n, ia.iface) {
				impl = n
			} else if types.Implements(types.NewPointer(n), ia.iface) {
				impl = types.NewPointer(n)
			} else {
				continue
			}
			for i := 0; i < ia.iface.NumMethods(); i++ {
				m := ia.iface.Method(i)
				if m.Exported() {
					continue
				}
				if cm, _, _ := types.LookupFieldOrMethod(impl, false, a.pkg, m.Name()); cm != nil {
					uf.union(origin(m), origin(cm))
				}
			}
		}
		for _, ib := range ifaces {
			if ia.iface != ib.iface && types.Implements(ia.iface, ib.iface) {
				for i := 0; i < ib.iface.NumMethods(); i++ {
					m := ib.iface.Method(i)
					if m.Exported() {
						continue
					}
					if cm, _, _ := types.LookupFieldOrMethod(ia.iface, false, a.pkg, m.Name()); cm != nil {
						uf.union(origin(m), origin(cm))
					}
				}
			}
		}
	}

	// Seeds: unexported members used across the boundary.
	seeds := map[types.Object]bool{}
	for _, u := range a.uses {
		if u.def || u.file == nil || !isMember(u.obj) || u.obj.Exported() {
			continue
		}
		if h := a.home(u.obj); h != nil && h.Moved != u.file.Moved && !u.file.Moved {
			seeds[u.obj] = true
		}
	}
	// Collect every member object of the source package (declared ones).
	groups := map[types.Object][]types.Object{}
	for _, u := range a.uses {
		if !u.def || !isMember(u.obj) || u.obj.Exported() || a.home(u.obj) == nil {
			continue
		}
		r := uf.find(u.obj)
		groups[r] = append(groups[r], u.obj)
	}
	var roots []types.Object
	for r := range groups {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Pos() < roots[j].Pos() })
	for _, r := range roots {
		members := groups[r]
		sort.Slice(members, func(i, j int) bool { return members[i].Pos() < members[j].Pos() })
		var seeded, moved, staying bool
		for _, m := range members {
			seeded = seeded || seeds[m]
			if a.home(m).Moved {
				moved = true
			} else {
				staying = true
			}
		}
		if !seeded && (!moved || !staying) {
			continue
		}
		name := members[0].Name()
		newName := exportName(name)
		for _, m := range members {
			if v, ok := m.(*types.Var); ok && v.Embedded() {
				// The field name is the type name: export the type instead.
				if tn := embeddedTypeName(v); tn != nil {
					if h := a.home(tn); h != nil && h.Moved && a.isPkgLevel(tn) {
						a.pkgRename[tn] = "" // resolved in planPackageRenames
						continue
					}
				}
				a.plan.errorf("%s: embedded field %s is used across the boundary but its type cannot be exported by this tool", a.posOf(m.Pos()), m.Name())
				continue
			}
			if newName == "" {
				a.plan.errorf("%s: %s %s must be exported but its name cannot be upper-cased", a.posOf(m.Pos()), kindOf(m), name)
				continue
			}
			kind := kindOf(m)
			owner := ownerName(m)
			if kind == "field" {
				owner = a.fieldOwner(m)
				if !a.cfg.AllowFieldExport {
					a.plan.errorf("%s: field %s.%s is used across the boundary and would have to be exported as %s; exporting a field changes reflection and encoding (encoding/json, yaml, gob, cmp) visibility - restructure first, or pass -allow-field-export and review the HIGH finding",
						a.posOf(m.Pos()), owner, name, newName)
				} else {
					a.plan.add(levelHigh, "exported struct field (reflection/encoding visibility changes)", a.posOf(m.Pos()),
						"%s.%s -> %s: encoding/json, yaml, gob and reflection now see this field; check for json/yaml tags and serialisation of %s (types in other packages that embed %s are not checked for shadowing)", owner, name, newName, owner, owner)
				}
			} else {
				if dynamicMethodNames[newName] {
					a.plan.errorf("%s: method %s.%s would be exported as %s, a name with dynamic meaning (fmt/encoding/io/errors/...); this can silently change behaviour - rename it by hand first",
						a.posOf(m.Pos()), owner, name, newName)
				} else {
					a.plan.add(levelWarn, "exported method (may newly satisfy interfaces)", a.posOf(m.Pos()),
						"%s.%s -> %s: check dynamic interface assertions that could now match; types in other packages that embed %s are not checked for shadowing or newly promoted methods", owner, name, newName, owner)
				}
			}
			a.memberRename[m] = newName
			a.plan.Renames = append(a.plan.Renames, renameEntry{Kind: kind, Owner: owner, Old: name, New: newName, Pos: a.posOf(m.Pos())})
		}
	}
	a.checkMemberShadowing()
}

func embeddedTypeName(v *types.Var) *types.TypeName {
	t := v.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	switch t := t.(type) {
	case *types.Alias:
		return t.Obj()
	case *types.Named:
		return t.Origin().Obj()
	}
	return nil
}

// fieldOwner finds the named struct type that declares field v.
func (a *analysis) fieldOwner(v types.Object) string {
	for _, name := range a.pkg.Scope().Names() {
		tn, ok := a.pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		if st, ok := tn.Type().Underlying().(*types.Struct); ok {
			for i := 0; i < st.NumFields(); i++ {
				if st.Field(i) == v {
					return tn.Name()
				}
			}
		}
	}
	return "struct"
}

// checkMemberShadowing makes sure no renamed member collides with, or changes
// the resolution of, another field or method with the new name.
func (a *analysis) checkMemberShadowing() {
	if len(a.memberRename) == 0 {
		return
	}
	check := func(t types.Type, where string) {
		for old, newName := range a.memberRenameByName() {
			obj, _, _ := types.LookupFieldOrMethod(t, false, a.pkg, old)
			if obj == nil {
				continue
			}
			if _, renamed := a.memberRename[origin(obj)]; !renamed {
				continue
			}
			if other, _, _ := types.LookupFieldOrMethod(t, false, a.pkg, newName); other != nil {
				a.plan.errorf("%s: renaming %s to %s on %s collides with (or changes the resolution of) %s %s declared at %s",
					where, old, newName, types.TypeString(t, types.RelativeTo(a.pkg)), kindOf(other), other.Name(), a.posOf(other.Pos()))
			}
		}
	}
	for _, name := range a.pkg.Scope().Names() {
		tn, ok := a.pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		check(tn.Type(), a.posOf(tn.Pos()))
		if _, isIface := tn.Type().Underlying().(*types.Interface); !isIface {
			check(types.NewPointer(tn.Type()), a.posOf(tn.Pos()))
		}
	}
	var sels []*ast.SelectorExpr
	for sel, s := range a.info.Selections {
		if _, ok := a.memberRename[origin(s.Obj())]; ok {
			sels = append(sels, sel)
		}
	}
	sort.Slice(sels, func(i, j int) bool { return sels[i].Pos() < sels[j].Pos() })
	for _, sel := range sels {
		s := a.info.Selections[sel]
		newName := a.memberRename[origin(s.Obj())]
		if other, _, _ := types.LookupFieldOrMethod(s.Recv(), false, a.pkg, newName); other != nil {
			a.plan.errorf("%s: selector .%s would become .%s, which already resolves to %s %s declared at %s",
				a.posOf(sel.Sel.Pos()), sel.Sel.Name, newName, kindOf(other), other.Name(), a.posOf(other.Pos()))
		}
	}
}

func (a *analysis) memberRenameByName() map[string]string {
	m := map[string]string{}
	for obj, n := range a.memberRename {
		m[obj.Name()] = n
	}
	return m
}

// planPackageRenames exports moved package-level objects that the source
// package (or an embedded-field rename) needs.
func (a *analysis) planPackageRenames() {
	need := map[types.Object]bool{}
	for obj := range a.forward {
		need[obj] = true
	}
	for obj := range a.pkgRename {
		need[obj] = true
	}
	a.exportObjects(need)
}

// exportObjects records exported names for the unexported objects in need.
func (a *analysis) exportObjects(need map[types.Object]bool) {
	var objs []types.Object
	for obj := range need {
		objs = append(objs, obj)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Pos() < objs[j].Pos() })
	for _, obj := range objs {
		if obj.Exported() {
			continue
		}
		if n, done := a.pkgRename[obj]; done && n != "" {
			continue
		}
		newName := exportName(obj.Name())
		if newName == "" {
			a.plan.errorf("%s: %s %s must be exported but its name cannot be upper-cased", a.posOf(obj.Pos()), kindOf(obj), obj.Name())
			delete(a.pkgRename, obj)
			continue
		}
		a.pkgRename[obj] = newName
		a.plan.Renames = append(a.plan.Renames, renameEntry{Kind: kindOf(obj), Old: obj.Name(), New: newName, Pos: a.posOf(obj.Pos())})
	}
	// Embedded fields follow their type's new name.
	for _, u := range a.uses {
		if !u.def {
			continue
		}
		if v, ok := u.obj.(*types.Var); ok && v.IsField() && v.Embedded() && u.file != nil && u.file.Moved {
			// A field declared in a moved file is spelled with the renamed
			// type, so its name changes too (staying structs embed the
			// alias and keep the old field name).
			if tn := embeddedTypeName(v); tn != nil {
				if n := a.pkgRename[tn]; n != "" {
					a.embedFollow[v] = n
				}
			}
		}
	}
}

// newName returns the post-move name of a package-level object.
func (a *analysis) newName(obj types.Object) string {
	if n := a.pkgRename[obj]; n != "" {
		return n
	}
	return obj.Name()
}

// checkPkgCollisions verifies that renamed package-level names are unique in
// the target package and are not shadowed at any reference.
func (a *analysis) checkPkgCollisions() {
	names := map[string]types.Object{}
	var objs []types.Object
	for _, name := range a.pkg.Scope().Names() {
		obj := a.pkg.Scope().Lookup(name)
		if h := a.home(obj); h != nil && h.Moved {
			objs = append(objs, obj)
		}
	}
	for _, obj := range objs {
		n := a.newName(obj)
		if prev, ok := names[n]; ok {
			a.plan.errorf("%s: exporting %s as %s collides with %s declared at %s", a.posOf(obj.Pos()), obj.Name(), n, prev.Name(), a.posOf(prev.Pos()))
			continue
		}
		names[n] = obj
	}
	for _, f := range a.checked {
		if !f.Moved {
			continue
		}
		for _, spec := range f.AST.Imports {
			iname := importName(spec)
			if obj, ok := names[iname]; ok && a.pkgRename[obj] != "" {
				a.plan.errorf("%s: exporting %s as %s collides with an import name in %s", a.posOf(obj.Pos()), obj.Name(), iname, f.Name)
			}
		}
	}
	for _, u := range a.uses {
		n := a.pkgRename[u.obj]
		if n == "" || u.file == nil || !u.file.Moved || u.def {
			continue
		}
		scope := a.pkg.Scope().Innermost(u.id.Pos())
		if scope == nil {
			continue
		}
		if _, other := scope.LookupParent(n, u.id.Pos()); other != nil && other.Parent() != a.pkg.Scope() {
			a.plan.errorf("%s: renaming %s to %s here would be shadowed by %s %s declared at %s",
				a.posOf(u.id.Pos()), u.obj.Name(), n, kindOf(other), other.Name(), a.posOf(other.Pos()))
		}
	}
}

// importName returns the name under which an import spec is visible.
func importName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		return spec.Name.Name
	}
	p, _ := strconv.Unquote(spec.Path.Value)
	return guessPkgName(p)
}

// guessPkgName approximates the package name of an import path (only used for
// collision checks, where over-approximation is harmless).
func guessPkgName(path string) string {
	base := path[strings.LastIndex(path, "/")+1:]
	if len(base) > 1 && base[0] == 'v' && strings.Trim(base[1:], "0123456789") == "" {
		if i := strings.LastIndex(path, "/"); i > 0 {
			rest := path[:i]
			base = rest[strings.LastIndex(rest, "/")+1:]
		}
	}
	base = strings.TrimPrefix(base, "go-")
	base = strings.TrimSuffix(base, ".go")
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '.' {
			return '_'
		}
		return r
	}, base)
}

// checkTestMain handles TestMain separation. A package's TestMain wraps every
// test in its binary (pkg/hub's isolates HOME, clears ambient cloud env,
// installs hermetic git runners and runs memory and leak guards). Moved tests
// run in the target's test binary, which has no TestMain unless one is
// provided.
func (a *analysis) checkTestMain() {
	var staying []string
	movedTests := false
	for _, f := range a.files {
		if !f.IsTest {
			continue
		}
		if f.Moved {
			movedTests = true
		}
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "TestMain" {
				continue
			}
			if f.Moved {
				a.plan.errorf("%s: TestMain moves; the source package's remaining tests would lose it - keep %s in %s and give the target its own TestMain (see -testmain-support)", a.posOf(fd.Pos()), f.Name, a.srcName)
			} else {
				staying = append(staying, a.posOf(fd.Pos()))
			}
		}
	}
	if !movedTests || len(staying) == 0 {
		return
	}
	where := strings.Join(staying, ", ")
	if a.cfg.TestMainSupport != "" {
		a.plan.add(levelWarn, "TestMain separation (generated a delegating TestMain)", where,
			"moved tests leave the TestMain of %s; the target gets a TestMain that calls %s.RunTestMain - check that it does everything the source TestMain does", a.srcName, a.cfg.TestMainSupport)
		support := a.cfg.TestMainSupport
		name := support[strings.LastIndex(support, "/")+1:]
		content := fmt.Sprintf(`%spackage %s

import (
	"os"
	"testing"

	%s
)

// TestMain runs the target's tests under the same harness as package %s
// (generated by hack/pkgmove; see %s for what it sets up).
func TestMain(m *testing.M) {
	os.Exit(%s.RunTestMain(m))
}
`, a.licenseHeader(), a.cfg.PkgName, strconv.Quote(support), a.srcName, staying[0], name)
		out, err := formatGo([]byte(content))
		if err != nil {
			a.plan.errorf("generating the target TestMain: %v", err)
			return
		}
		path := filepath.Join(a.cfg.DstDir, "zz_testmain_test.go")
		a.plan.ExtraFiles = append(a.plan.ExtraFiles, generatedFile{Path: a.rel(path), Content: out})
		return
	}
	stub := filepath.Join(filepath.Dir(a.cfg.ReportPath), strings.TrimSuffix(filepath.Base(a.cfg.ReportPath), ".txt")+"_testmain.go.txt")
	a.plan.add(levelHigh, "TestMain separation", where,
		"moved tests leave the TestMain of %s, so they would run without it (HOME isolation, env clearing, fakes, leak/memory guards, ...); generate the target's TestMain with -testmain-support <pkg> (a test-support package exporting RunTestMain(m *testing.M) int that the source TestMain also calls), or start from the stub %s",
		a.srcName, a.rel(stub))
	a.plan.StubFiles = append(a.plan.StubFiles, generatedFile{Path: a.rel(stub), Content: []byte(fmt.Sprintf(`// Reference stub generated by hack/pkgmove (not compiled; rename to
// %s/zz_testmain_test.go once the helper exists).
//
// Package %s has a TestMain (%s). Its tests that moved to %s
// need the same harness. Recommended layout, so the logic is not duplicated:
//
//   - a test-support package (for example <src>/internal/<name>testmain)
//     exporting:
//
//       // RunTestMain sets up the package test harness, runs m and tears
//       // it down, returning the exit code.
//       func RunTestMain(m *testing.M) int
//
//   - %s's TestMain calls it:  func TestMain(m *testing.M) { os.Exit(x.RunTestMain(m)) }
//   - re-run pkgmove with -testmain-support <that import path>, which
//     generates the file below in the target.

package %s

import (
	"os"
	"testing"

	x "<test-support import path>"
)

func TestMain(m *testing.M) {
	os.Exit(x.RunTestMain(m))
}
`, a.rel(a.cfg.DstDir), a.srcName, where, a.dstImport, a.srcName, a.cfg.PkgName))})
}

func formatGo(src []byte) ([]byte, error) { return format.Source(src) }
