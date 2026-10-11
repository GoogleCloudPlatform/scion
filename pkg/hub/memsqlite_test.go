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
