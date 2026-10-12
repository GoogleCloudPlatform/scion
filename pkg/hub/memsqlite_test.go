//go:build !hubshard || hubshard_6

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

package hub

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unsafeMemoryDSN reports whether dsn opens a per-connection private
// in-memory SQLite database: ":memory:" or "mode=memory" without
// "cache=shared". Each pooled connection to such a DSN sees its own empty
// database.
func unsafeMemoryDSN(dsn string) bool {
	if strings.Contains(dsn, "cache=shared") {
		return false
	}
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

// rawMemorySQLiteOpens parses Go source files and returns every sql.Open or
// ent.Open call whose DSN argument is a string literal, or a package-level or
// local string constant, that unsafeMemoryDSN flags. It works on the AST, so
// calls split across lines, arguments containing parentheses and DSNs held in
// constants are all seen, and comments are ignored. The one sanctioned call,
// inside openTestMemorySQLite, is exempt.
//
// Known blind spot: a DSN built at run time (fmt.Sprintf, concatenation with a
// variable) is not evaluated.
func rawMemorySQLiteOpens(files map[string][]byte) ([]string, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	consts := map[string]string{} // package-level string constants
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		parsed[name] = f
		for _, decl := range f.Decls {
			collectStringConsts(decl, consts)
		}
	}
	var offenders []string
	for _, f := range parsed {
		for _, decl := range f.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if isFunc && fn.Recv == nil && fn.Name.Name == "openTestMemorySQLite" {
				continue
			}
			local := map[string]string{}
			ast.Inspect(decl, func(n ast.Node) bool {
				if ds, ok := n.(*ast.DeclStmt); ok {
					collectStringConsts(ds.Decl, local)
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Open" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || (pkg.Name != "sql" && pkg.Name != "ent") {
					return true
				}
				var dsn string
				switch a := call.Args[1].(type) {
				case *ast.BasicLit:
					if a.Kind == token.STRING {
						dsn, _ = strconv.Unquote(a.Value)
					}
				case *ast.Ident:
					if v, ok := local[a.Name]; ok {
						dsn = v
					} else {
						dsn = consts[a.Name]
					}
				}
				if unsafeMemoryDSN(dsn) {
					pos := fset.Position(call.Pos())
					offenders = append(offenders, pos.Filename+":"+strconv.Itoa(pos.Line)+": "+pkg.Name+".Open(..., "+strconv.Quote(dsn)+")")
				}
				return true
			})
		}
	}
	sort.Strings(offenders)
	return offenders, nil
}

// collectStringConsts records the string-literal constants declared by decl.
// A spec with no values in a const block implicitly repeats the previous
// spec's value list (const ( a = ":memory:"; b ) gives b ":memory:" too), so
// the last explicit list is carried forward.
func collectStringConsts(decl ast.Decl, into map[string]string) {
	gd, ok := decl.(*ast.GenDecl)
	if !ok || gd.Tok != token.CONST {
		return
	}
	var last []ast.Expr
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		values := vs.Values
		if len(values) == 0 {
			values = last
		} else {
			last = values
		}
		for i, n := range vs.Names {
			if i >= len(values) {
				break
			}
			if lit, ok := values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					into[n.Name] = v
				}
			}
		}
	}
}

// TestNoRawMemorySQLiteOpen keeps pkg/hub tests on openTestMemorySQLite, so a
// new test cannot reintroduce the unpinned ":memory:" pool behind
// ptone/scion#2312.
func TestNoRawMemorySQLiteOpen(t *testing.T) {
	// pkgmove:scan-covers pkg/hub/apierr
	// unaffected: guards in-memory sql.Open/ent.Open in test files, which errors.go and json_response.go do not contain.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// unaffected: guards in-memory sql.Open/ent.Open in test files (no test file moves), which identity.go and identity_credential.go do not contain.
	names, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range names {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		files[f] = data
	}
	offenders, err := rawMemorySQLiteOpens(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("raw in-memory SQLite open in pkg/hub tests; use openTestMemorySQLite(t, driver) "+
			"(memsqlite_test.go) instead, which pins the pool to one connection so every query sees "+
			"the same database (ptone/scion#2312):\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestRawMemorySQLiteOpensDetector checks the guard against the spellings it
// must catch and the ones it must allow.
func TestRawMemorySQLiteOpensDetector(t *testing.T) {
	src := `package hub

import "database/sql"

const memDSN = ":memory:"

const (
	implicitBase = ":memory:"
	implicitDSN
)

func drv() string { return "sqlite3" }

func openTestMemorySQLite() { _, _ = sql.Open("sqlite3", ":memory:") } // exempt

func bad() {
	const localDSN = "file:x?mode=memory"
	_, _ = sql.Open("sqlite3", ":memory:")
	_, _ = sql.Open("sqlite3", memDSN)
	_, _ = sql.Open(
		"sqlite3",
		":memory:",
	)
	_, _ = sql.Open(drv(), ":memory:")
	_, _ = sql.Open("sqlite3", localDSN)
	_, _ = ent.Open("sqlite3", "file::memory:?_fk=1")
	_, _ = sql.Open("sqlite3", implicitDSN)
}

func good() {
	// _, _ = sql.Open("sqlite3", ":memory:")
	_, _ = sql.Open("sqlite3", "file::memory:?cache=shared")
	_, _ = sql.Open("sqlite3", "file:x?mode=memory&cache=shared")
	_, _ = sql.Open("sqlite3", "/tmp/db.sqlite")
}
`
	got, err := rawMemorySQLiteOpens(map[string][]byte{"probe_test.go": []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("want 7 offenders (literal, const, multi-line, paren arg, local mode=memory const, ent, implicit const), got %d:\n  %s",
			len(got), strings.Join(got, "\n  "))
	}
}

// mattnImportKey is the mattnDriverAllowed key that allows a file to import
// github.com/mattn/go-sqlite3. "import" is a Go keyword, so it never collides
// with a function name.
const mattnImportKey = "import"

// mattnDriverAllowed lists, by file, the functions that may keep opening
// the mattn/go-sqlite3 driver ("sqlite3") in a file that no_sqlite builds
// exclude, and (mattnImportKey) the files that may import the package.
// TestSQLiteOnlyTestsUseProductionDriver fails on an entry that no longer
// matches any use, so the list cannot go stale.
var mattnDriverAllowed = map[string]map[string]bool{
	// The fault injection needs an authorizer hook, which modernc.org/sqlite
	// does not expose.
	"webchannel_store_schema_test.go": {
		mattnImportKey: true,
		"TestSQLiteWebChatStore_TableExistsError_Surfaces": true,
	},
}

// excludedFromNoSQLite reports whether src carries a //go:build constraint
// that is false when the no_sqlite tag is set, whatever the other tags.
// Such a file is never compiled by "make test-fast" (go test -tags
// no_sqlite), so it can always use the production modernc.org/sqlite driver.
func excludedFromNoSQLite(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return false
		}
		// Try every assignment of the other tags the expression names.
		var tags []string
		collectConstraintTags(expr, map[string]bool{"no_sqlite": true}, &tags)
		for mask := 0; mask < 1<<len(tags); mask++ {
			on := map[string]bool{"no_sqlite": true}
			for i, tag := range tags {
				on[tag] = mask&(1<<i) != 0
			}
			if expr.Eval(func(tag string) bool { return on[tag] }) {
				return false
			}
		}
		return true
	}
	return false
}

// collectConstraintTags appends to tags each tag expr names that is not
// already in seen, in first-seen order.
func collectConstraintTags(expr constraint.Expr, seen map[string]bool, tags *[]string) {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		if !seen[e.Tag] {
			seen[e.Tag] = true
			*tags = append(*tags, e.Tag)
		}
	case *constraint.NotExpr:
		collectConstraintTags(e.X, seen, tags)
	case *constraint.AndExpr:
		collectConstraintTags(e.X, seen, tags)
		collectConstraintTags(e.Y, seen, tags)
	case *constraint.OrExpr:
		collectConstraintTags(e.X, seen, tags)
		collectConstraintTags(e.Y, seen, tags)
	}
}

// sqliteOpenPackages maps the import paths whose Open function takes a
// database/sql driver name as its first argument to their default package
// names.
var sqliteOpenPackages = map[string]string{
	"database/sql":                                 "sql",
	"entgo.io/ent/dialect/sql":                     "sql",
	"github.com/GoogleCloudPlatform/scion/pkg/ent": "ent",
}

// mattnDriverUses returns every use of the mattn/go-sqlite3 driver in files
// that no_sqlite builds exclude: an import of the package (blank, named or
// plain), or an openTestMemorySQLite call or an Open call of database/sql,
// entgo.io/ent/dialect/sql or pkg/ent (under any import name) whose driver
// argument is "sqlite3", spelled as a literal or as a string const or var
// declared in the package or in the enclosing function. Files built under
// no_sqlite are skipped: there modernc.org/sqlite is not linked and mattn is
// the only driver. Uses listed in allowed are skipped, and an allowed entry
// that matches no use is reported as stale.
//
// Known limits (none occur in the tree today):
//   - a driver name computed at run time is not evaluated;
//   - names are resolved through one flat map per top-level declaration,
//     filled only from string-literal const, var and := declarations. A
//     function parameter, a non-literal := (driver := pick()) or a later
//     plain = does not shadow a package-level name, so the package value is
//     used. That reports a false use (fails closed; fix it or allowlist it);
//   - a name declared in an inner block or closure stays visible to later
//     calls in the enclosing scope, which can hide a use;
//   - a dot-imported Open (a bare identifier call) is not matched.
func mattnDriverUses(files map[string][]byte, allowed map[string]map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	type parsedFile struct {
		name string
		file *ast.File
	}
	var scoped []parsedFile
	pkgStrings := map[string]string{} // package-level string consts and vars
	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			collectStringDecls(decl, pkgStrings)
		}
		if excludedFromNoSQLite(src) {
			scoped = append(scoped, parsedFile{name, f})
		}
	}
	used := map[string]map[string]bool{}
	allow := func(name, key string) bool {
		base := filepath.Base(name)
		if !allowed[base][key] {
			return false
		}
		if used[base] == nil {
			used[base] = map[string]bool{}
		}
		used[base][key] = true
		return true
	}
	var offenders []string
	for _, pf := range scoped {
		name, f := pf.name, pf.file
		openPkgs := map[string]bool{} // local names of the sqliteOpenPackages
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "github.com/mattn/go-sqlite3" && !allow(name, mattnImportKey) {
				offenders = append(offenders, fset.Position(imp.Pos()).String()+": import of github.com/mattn/go-sqlite3")
			}
			if def, ok := sqliteOpenPackages[path]; ok {
				local := def
				if imp.Name != nil {
					local = imp.Name.Name
				}
				openPkgs[local] = true
			}
		}
		for _, decl := range f.Decls {
			fnName := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				fnName = fn.Name.Name
			}
			local := map[string]string{}
			var hits []string
			ast.Inspect(decl, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.DeclStmt:
					collectStringDecls(s.Decl, local)
				case *ast.AssignStmt:
					if s.Tok == token.DEFINE && len(s.Lhs) == len(s.Rhs) {
						for i, lhs := range s.Lhs {
							id, ok := lhs.(*ast.Ident)
							lit, ok2 := s.Rhs[i].(*ast.BasicLit)
							if ok && ok2 && lit.Kind == token.STRING {
								if v, err := strconv.Unquote(lit.Value); err == nil {
									local[id.Name] = v
								}
							}
						}
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				driverArg := 0
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name != "openTestMemorySQLite" {
						return true
					}
					callee, driverArg = fun.Name, 1
				case *ast.SelectorExpr:
					pkg, ok := fun.X.(*ast.Ident)
					if !ok || fun.Sel.Name != "Open" || !openPkgs[pkg.Name] {
						return true
					}
					callee = pkg.Name + ".Open"
				default:
					return true
				}
				if len(call.Args) <= driverArg {
					return true
				}
				var driver string
				switch a := call.Args[driverArg].(type) {
				case *ast.BasicLit:
					if a.Kind == token.STRING {
						driver, _ = strconv.Unquote(a.Value)
					}
				case *ast.Ident:
					if v, ok := local[a.Name]; ok {
						driver = v
					} else {
						driver = pkgStrings[a.Name]
					}
				}
				if driver == "sqlite3" {
					hits = append(hits, fset.Position(call.Pos()).String()+": "+callee+` with driver "sqlite3"`)
				}
				return true
			})
			if len(hits) > 0 && !allow(name, fnName) {
				offenders = append(offenders, hits...)
			}
		}
	}
	for file, keys := range allowed {
		for key := range keys {
			if !used[file][key] {
				offenders = append(offenders, fmt.Sprintf("stale mattnDriverAllowed entry %s: %q matches no mattn use; remove it", file, key))
			}
		}
	}
	sort.Strings(offenders)
	return offenders, nil
}

// collectStringDecls records the string-literal constants and variables
// declared by decl. Constants follow collectStringConsts (including the
// implicit repetition in const blocks); variables need an explicit value.
func collectStringDecls(decl ast.Decl, into map[string]string) {
	collectStringConsts(decl, into)
	gd, ok := decl.(*ast.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return
	}
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, n := range vs.Names {
			if i >= len(vs.Values) {
				break
			}
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					into[n.Name] = v
				}
			}
		}
	}
}

// TestSQLiteOnlyTestsUseProductionDriver keeps pkg/hub test files that
// no_sqlite builds exclude on the driver production uses, modernc.org/sqlite
// ("sqlite"), rather than mattn/go-sqlite3 ("sqlite3"). The two drivers
// differ in time handling, DSN options and error types, so a test on mattn
// can pass where production fails (ptone/scion#4694). Files that "make
// test-fast" compiles stay on mattn, the only driver linked there.
func TestSQLiteOnlyTestsUseProductionDriver(t *testing.T) {
	names, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range names {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		files[f] = data
	}
	offenders, err := mattnDriverUses(files, mattnDriverAllowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("pkg/hub test files excluded from no_sqlite builds use the mattn driver; "+
			"open with the production driver \"sqlite\" (modernc.org/sqlite) instead, "+
			"or add the function to mattnDriverAllowed with the reason:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestMattnDriverUsesDetector checks the driver guard against the spellings
// it must catch and the ones it must allow.
func TestMattnDriverUsesDetector(t *testing.T) {
	const sqliteOnly = "//go:build !no_sqlite && (!hubshard || hubshard_1)\n\n"
	const body = `package hub

import (
	"database/sql"

	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/mattn/go-sqlite3"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

const mattnDriver = "sqlite3"

var mattnVar = "sqlite3"

const shadowed = "sqlite3"

func bad(t *testing.T) {
	_ = openTestMemorySQLite(t, "sqlite3")
	_, _ = sql.Open("sqlite3", "file:x.db")
	_, _ = ent.Open(
		"sqlite3",
		"file:x.db",
	)
	_, _ = entsql.Open("sqlite3", "file:x.db")
	_, _ = sql.Open(mattnDriver, "file:x.db")
	_ = openTestMemorySQLite(t, mattnVar)
	const localDriver = "sqlite3"
	_, _ = sql.Open(localDriver, "file:x.db")
	short := "sqlite3"
	_, _ = sql.Open(short, "file:x.db")
}

func good(t *testing.T) {
	// _ = openTestMemorySQLite(t, "sqlite3")
	_ = openTestMemorySQLite(t, "sqlite")
	_, _ = sql.Open("sqlite", "file:x.db")
	_, _ = entsql.Open("sqlite", "file:x.db")
	_ = NewWebChatStore(nil, "sqlite3")
	_, _ = other.Open("sqlite3", "file:x.db")
	// A local name shadows the package-level one of the same name.
	shadowed := "sqlite"
	_, _ = sql.Open(shadowed, "file:x.db")
}

func goodLocalConst(t *testing.T) {
	const shadowed = "sqlite"
	_ = openTestMemorySQLite(t, shadowed)
}
`
	// A named and a plain import are caught like a blank one.
	const namedImport = sqliteOnly + `package hub

import sqlite3 "github.com/mattn/go-sqlite3"
`
	const plainImport = sqliteOnly + `package hub

import "github.com/mattn/go-sqlite3"
`
	const allowedFile = sqliteOnly + `package hub

import sqlite3 "github.com/mattn/go-sqlite3"

func allowedFn(t *testing.T) {
	_ = openTestMemorySQLite(t, "sqlite3")
}

func other(t *testing.T) {
	_ = openTestMemorySQLite(t, "sqlite3")
}
`
	allowed := map[string]map[string]bool{
		"allowed_test.go": {mattnImportKey: true, "allowedFn": true},
		"stale_test.go":   {"gone": true},
	}
	got, err := mattnDriverUses(map[string][]byte{
		"sqlite_only_test.go":  []byte(sqliteOnly + body),
		"named_import_test.go": []byte(namedImport),
		"plain_import_test.go": []byte(plainImport),
		"untagged_test.go":     []byte(body),
		"shard_only_test.go":   []byte("//go:build !hubshard || hubshard_1\n\n" + body),
		"allowed_test.go":      []byte(allowedFile),
	}, allowed)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		// import, literal helper, sql.Open, multi-line ent.Open, entsql.Open,
		// package const, package var, local const, short var decl.
		"sqlite_only_test.go:":                         9,
		"named_import_test.go:":                        1,
		"plain_import_test.go:":                        1,
		"allowed_test.go:":                             1, // other(); the import and allowedFn are allowed
		"stale mattnDriverAllowed entry stale_test.go": 1,
	}
	gotBy := map[string]int{}
	for _, o := range got {
		matched := false
		for prefix := range want {
			if strings.HasPrefix(o, prefix) {
				gotBy[prefix]++
				matched = true
			}
		}
		if !matched {
			t.Errorf("unexpected offender: %s", o)
		}
	}
	for prefix, n := range want {
		if gotBy[prefix] != n {
			t.Errorf("%s: want %d offenders, got %d", prefix, n, gotBy[prefix])
		}
	}
	if t.Failed() {
		t.Logf("offenders:\n  %s", strings.Join(got, "\n  "))
	}
}
