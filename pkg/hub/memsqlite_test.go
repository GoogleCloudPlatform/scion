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

// mattnDriverAllowed lists the functions, by file, that may keep using the
// mattn/go-sqlite3 driver in a file excluded from no_sqlite builds.
var mattnDriverAllowed = map[string]map[string]bool{
	// The fault injection needs an authorizer hook, which modernc.org/sqlite
	// does not expose.
	"webchannel_store_schema_test.go": {"TestSQLiteWebChatStore_TableExistsError_Surfaces": true},
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

// mattnDriverUses returns every use of the mattn/go-sqlite3 driver in files
// that no_sqlite builds exclude: a blank import of the package, or an
// openTestMemorySQLite, sql.Open or ent.Open call whose driver argument is
// the string literal "sqlite3". Files built under no_sqlite are skipped:
// there modernc.org/sqlite is not linked and mattn is the only driver.
func mattnDriverUses(files map[string][]byte) ([]string, error) {
	fset := token.NewFileSet()
	var offenders []string
	for name, src := range files {
		if !excludedFromNoSQLite(src) {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			if imp.Name != nil && imp.Name.Name == "_" && imp.Path.Value == `"github.com/mattn/go-sqlite3"` {
				offenders = append(offenders, fset.Position(imp.Pos()).String()+": blank import of github.com/mattn/go-sqlite3")
			}
		}
		for _, decl := range f.Decls {
			fnName := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				fnName = fn.Name.Name
			}
			if mattnDriverAllowed[filepath.Base(name)][fnName] {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
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
					if !ok || fun.Sel.Name != "Open" || (pkg.Name != "sql" && pkg.Name != "ent") {
						return true
					}
					callee = pkg.Name + ".Open"
				default:
					return true
				}
				if len(call.Args) <= driverArg {
					return true
				}
				lit, ok := call.Args[driverArg].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || lit.Value != `"sqlite3"` {
					return true
				}
				offenders = append(offenders, fset.Position(call.Pos()).String()+": "+callee+` with driver "sqlite3"`)
				return true
			})
		}
	}
	sort.Strings(offenders)
	return offenders, nil
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
	offenders, err := mattnDriverUses(files)
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

	_ "github.com/mattn/go-sqlite3"
)

func bad(t *testing.T) {
	_ = openTestMemorySQLite(t, "sqlite3")
	_, _ = sql.Open("sqlite3", "file:x.db")
	_, _ = ent.Open(
		"sqlite3",
		"file:x.db",
	)
}

func good(t *testing.T) {
	// _ = openTestMemorySQLite(t, "sqlite3")
	_ = openTestMemorySQLite(t, "sqlite")
	_, _ = sql.Open("sqlite", "file:x.db")
	_ = NewWebChatStore(nil, "sqlite3")
}
`
	const allowed = sqliteOnly + `package hub

func TestSQLiteWebChatStore_TableExistsError_Surfaces(t *testing.T) {
	_ = openTestMemorySQLite(t, "sqlite3")
}

func other(t *testing.T) {
	_ = openTestMemorySQLite(t, "sqlite3")
}
`
	got, err := mattnDriverUses(map[string][]byte{
		"sqlite_only_test.go":             []byte(sqliteOnly + body),
		"untagged_test.go":                []byte(body),
		"shard_only_test.go":              []byte("//go:build !hubshard || hubshard_1\n\n" + body),
		"webchannel_store_schema_test.go": []byte(allowed),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 offenders (blank import, helper, sql.Open, multi-line ent.Open in sqlite_only_test.go; other() in the allowlisted file), got %d:\n  %s",
			len(got), strings.Join(got, "\n  "))
	}
	for _, o := range got {
		if !strings.HasPrefix(o, "sqlite_only_test.go:") && !strings.HasPrefix(o, "webchannel_store_schema_test.go:") {
			t.Errorf("unexpected offender outside the no_sqlite-excluded files: %s", o)
		}
	}
}
