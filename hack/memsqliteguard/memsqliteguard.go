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

// Package memsqliteguard finds in-memory SQLite databases opened with a
// connection pool that is not pinned to one connection. The repository-wide
// check is TestRepoMemorySQLiteOpensArePinned in this package; it runs with
// the rest of the non-hub tests in CI (make test-fast-ci).
//
// Why one connection: each connection database/sql opens to a private
// ":memory:" database is a separate, empty database, so a second pooled
// connection fails with "no such table" (ptone/scion#2312). A shared-cache
// in-memory database ("mode=memory&cache=shared") is shared across
// connections, but two connections writing at once fail with SQLITE_LOCKED
// table-lock errors that the busy timeout does not retry (ptone/scion#5341).
// Production pins SQLite to MaxOpenConns=1 for the same reason.
//
// Rules:
//
//   - unpinned (every Go file in the tree): an in-memory SQLite open must be
//     pinned to one connection. For entc.OpenSQLite the pool argument must be
//     a PoolConfig literal with MaxOpenConns: 1. For sql.Open the returned
//     handle must get SetMaxOpenConns(1) in the same function; for
//     entgo.io/ent/dialect/sql.Open, its DB() must. ent.Open cannot be pinned
//     and is always reported.
//   - hub-raw (pkg/hub and its subpackages, including the hub test-utility
//     package): a private in-memory database (no cache=shared) must be opened
//     through hubtestutil.OpenTestMemorySQLite, even when pinned.
//
// The DSN is evaluated from string literals, constants, local variables
// (every value assigned in the enclosing function is checked), string
// concatenation and fmt.Sprintf format strings. Known blind spots: a DSN that
// comes from a function parameter, a struct field or another function's
// result is not evaluated, and pinning done in another function (for example
// by the caller of a helper that returns sql.Open's result) is not seen, so
// such a helper is reported.
package memsqliteguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Rule names used in findings.
const (
	RuleUnpinned = "unpinned"
	RuleHubRaw   = "hub-raw"
)

// SanctionedHubOpener is the one function allowed to open a private
// in-memory SQLite database inside pkg/hub (rule hub-raw).
const SanctionedHubOpener = "OpenTestMemorySQLite"

const (
	importSQL    = "database/sql"
	importEnt    = "github.com/GoogleCloudPlatform/scion/pkg/ent"
	importEntc   = "github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	importEntSQL = "entgo.io/ent/dialect/sql"
)

// Finding is one reported open.
type Finding struct {
	File string // slash-separated path as given to Scan
	Line int
	Rule string
	Call string // the call, as source text
	DSN  string // the evaluated DSN that made it an in-memory open
}

func (f Finding) String() string {
	return f.File + ":" + strconv.Itoa(f.Line) + ": " + f.Rule + ": " + f.Call + " (DSN " + strconv.Quote(f.DSN) + ")"
}

// Stats counts what a scan looked at, so a caller can tell a clean result
// from one that analysed nothing.
type Stats struct {
	Files       int // Go files parsed
	MemoryOpens int // in-memory SQLite opens seen, pinned or not
}

// unknown stands in for any part of a DSN that is not a compile-time string.
const unknown = "\x00"

func isMemoryDSN(dsn string) bool {
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

func isSharedCache(dsn string) bool { return strings.Contains(dsn, "cache=shared") }

// Scan analyses files, keyed by slash-separated path relative to the
// repository root. Files in one directory with the same package name form a
// package, whose package-level constants and variables are shared.
func Scan(files map[string][]byte) ([]Finding, Stats, error) {
	fset := token.NewFileSet()
	type pkgKey struct{ dir, name string }
	parsed := map[string]*ast.File{}
	pkgScope := map[pkgKey]map[string][]ast.Expr{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if err != nil {
			return nil, Stats{}, err
		}
		parsed[name] = f
		k := pkgKey{path.Dir(name), f.Name.Name}
		if pkgScope[k] == nil {
			pkgScope[k] = map[string][]ast.Expr{}
		}
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				collectGenDecl(gd, pkgScope[k])
			}
		}
	}
	stats := Stats{Files: len(parsed)}
	var out []Finding
	for _, name := range names {
		f := parsed[name]
		s := &fileScanner{
			fset:    fset,
			name:    name,
			file:    f,
			imports: importNames(f),
			pkg:     pkgScope[pkgKey{path.Dir(name), f.Name.Name}],
			inHub:   name == "pkg/hub" || strings.HasPrefix(name, "pkg/hub/"),
			inEntc:  path.Dir(name) == "pkg/ent/entc" && f.Name.Name == "entc",
		}
		for _, decl := range f.Decls {
			s.scanDecl(decl, &stats, &out)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out, stats, nil
}

type fileScanner struct {
	fset    *token.FileSet
	name    string
	file    *ast.File
	imports map[string]string // local name -> import path
	pkg     map[string][]ast.Expr
	inHub   bool
	inEntc  bool
}

// importNames maps each import's local name to its path. An unnamed import
// is known by the last path element, which holds for every import path this
// guard looks for.
func importNames(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := path.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		m[local] = p
	}
	return m
}

// opener describes one recognised open call.
type opener struct {
	kind    string // "sql", "ent", "entsql" or "entc"
	dsnArg  int
	poolArg int // entc only
}

func (s *fileScanner) classify(call *ast.CallExpr) (opener, bool) {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		if !ok {
			return opener{}, false
		}
		switch p := s.imports[pkg.Name]; {
		case p == importSQL && fn.Sel.Name == "Open":
			return opener{kind: "sql", dsnArg: 1}, true
		case p == importEnt && fn.Sel.Name == "Open":
			return opener{kind: "ent", dsnArg: 1}, true
		case p == importEntSQL && fn.Sel.Name == "Open":
			return opener{kind: "entsql", dsnArg: 1}, true
		case p == importEntc && fn.Sel.Name == "OpenSQLite":
			return opener{kind: "entc", dsnArg: 0, poolArg: 1}, true
		}
	case *ast.Ident:
		if s.inEntc && fn.Name == "OpenSQLite" {
			return opener{kind: "entc", dsnArg: 0, poolArg: 1}, true
		}
	}
	return opener{}, false
}

func (s *fileScanner) scanDecl(decl ast.Decl, stats *Stats, out *[]Finding) {
	fn, isFunc := decl.(*ast.FuncDecl)
	var body ast.Node = decl
	local := map[string][]ast.Expr{}
	exemptHub := false
	if isFunc {
		if fn.Body == nil {
			return
		}
		body = fn.Body
		collectLocals(fn.Body, local)
		exemptHub = fn.Recv == nil && fn.Name.Name == SanctionedHubOpener
	}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		op, ok := s.classify(call)
		if !ok || len(call.Args) <= op.dsnArg {
			return true
		}
		for _, dsn := range s.eval(call.Args[op.dsnArg], local, 0) {
			if !isMemoryDSN(dsn) {
				continue
			}
			stats.MemoryOpens++
			report := func(rule string) {
				*out = append(*out, Finding{
					File: s.name,
					Line: s.fset.Position(call.Pos()).Line,
					Rule: rule,
					Call: types.ExprString(call),
					DSN:  strings.ReplaceAll(dsn, unknown, "…"),
				})
			}
			if !s.pinned(op, call, body, local) {
				report(RuleUnpinned)
			}
			if s.inHub && !isSharedCache(dsn) && !exemptHub {
				report(RuleHubRaw)
			}
			break // one report per call
		}
		return true
	})
}

// pinned reports whether the open's pool is limited to one connection.
func (s *fileScanner) pinned(op opener, call *ast.CallExpr, body ast.Node, local map[string][]ast.Expr) bool {
	switch op.kind {
	case "entc":
		if len(call.Args) <= op.poolArg {
			return false
		}
		return s.poolPinned(call.Args[op.poolArg], local, 0)
	case "sql", "entsql":
		lhs := assignedTo(body, call)
		if lhs == "" {
			return false
		}
		recv := lhs
		if op.kind == "entsql" {
			recv = lhs + ".DB()"
		}
		return callsSetMaxOpenConnsOne(body, recv)
	}
	return false // ent.Open: no handle to pin
}

// poolPinned reports whether e is a PoolConfig literal (possibly through a
// local variable or constant) with MaxOpenConns: 1.
func (s *fileScanner) poolPinned(e ast.Expr, local map[string][]ast.Expr, depth int) bool {
	if depth > 8 {
		return false
	}
	switch v := e.(type) {
	case *ast.ParenExpr:
		return s.poolPinned(v.X, local, depth+1)
	case *ast.UnaryExpr:
		return s.poolPinned(v.X, local, depth+1)
	case *ast.Ident:
		vals := local[v.Name]
		if len(vals) == 0 {
			vals = s.pkg[v.Name]
		}
		if len(vals) == 0 {
			return false
		}
		for _, val := range vals {
			if !s.poolPinned(val, local, depth+1) {
				return false
			}
		}
		return true
	case *ast.CompositeLit:
		for _, elt := range v.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "MaxOpenConns" {
				lit, ok := kv.Value.(*ast.BasicLit)
				return ok && lit.Kind == token.INT && lit.Value == "1"
			}
		}
	}
	return false
}

// assignedTo returns the source text of the first left-hand side the call's
// result is assigned to, or "" if the result is not assigned (returned,
// passed on, or discarded).
func assignedTo(body ast.Node, call *ast.CallExpr) string {
	var lhs string
	ast.Inspect(body, func(n ast.Node) bool {
		if lhs != "" {
			return false
		}
		switch a := n.(type) {
		case *ast.AssignStmt:
			if len(a.Rhs) == 1 && a.Rhs[0] == call && len(a.Lhs) > 0 {
				lhs = types.ExprString(a.Lhs[0])
			}
		case *ast.ValueSpec:
			if len(a.Values) == 1 && a.Values[0] == call && len(a.Names) > 0 {
				lhs = a.Names[0].Name
			}
		}
		return true
	})
	if lhs == "_" {
		return ""
	}
	return lhs
}

// callsSetMaxOpenConnsOne reports whether body calls recv.SetMaxOpenConns(1).
func callsSetMaxOpenConnsOne(body ast.Node, recv string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetMaxOpenConns" || types.ExprString(sel.X) != recv {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.INT && lit.Value == "1" {
			found = true
		}
		return true
	})
	return found
}

// eval returns the possible compile-time values of a string expression, with
// unknown standing in for parts that are not compile-time strings.
func (s *fileScanner) eval(e ast.Expr, local map[string][]ast.Expr, depth int) []string {
	if depth > 8 {
		return []string{unknown}
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if u, err := strconv.Unquote(v.Value); err == nil {
				return []string{u}
			}
		}
	case *ast.ParenExpr:
		return s.eval(v.X, local, depth+1)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return cross(s.eval(v.X, local, depth+1), s.eval(v.Y, local, depth+1))
		}
	case *ast.Ident:
		vals := local[v.Name]
		if len(vals) == 0 {
			vals = s.pkg[v.Name]
		}
		var out []string
		for _, val := range vals {
			out = append(out, s.eval(val, local, depth+1)...)
		}
		if len(out) > 0 {
			return dedupe(out)
		}
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && len(v.Args) > 0 {
			if pkg, ok := sel.X.(*ast.Ident); ok && s.imports[pkg.Name] == "fmt" && sel.Sel.Name == "Sprintf" {
				// The format string carries the DSN options; arguments are
				// appended so a constant argument is still seen.
				out := s.eval(v.Args[0], local, depth+1)
				for _, a := range v.Args[1:] {
					out = cross(out, s.eval(a, local, depth+1))
				}
				return out
			}
		}
	}
	return []string{unknown}
}

const maxCandidates = 64

func cross(a, b []string) []string {
	out := make([]string, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			out = append(out, x+y)
			if len(out) >= maxCandidates {
				return dedupe(out)
			}
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// collectGenDecl records the values of the constants and variables declared
// by gd. A spec with no values in a const block implicitly repeats the
// previous spec's value list (const ( a = ":memory:"; b ) gives b ":memory:"
// too), so the last explicit list is carried forward.
func collectGenDecl(gd *ast.GenDecl, into map[string][]ast.Expr) {
	if gd.Tok != token.CONST && gd.Tok != token.VAR {
		return
	}
	var last []ast.Expr
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		values := vs.Values
		if gd.Tok == token.CONST {
			if len(values) == 0 {
				values = last
			} else {
				last = values
			}
		}
		if len(values) != len(vs.Names) {
			continue
		}
		for i, n := range vs.Names {
			into[n.Name] = append(into[n.Name], values[i])
		}
	}
}

// collectLocals records every value assigned to a name anywhere in body:
// const and var declarations, := and =. Names are not scoped, so two
// variables with the same name in one function share their values, which
// can only add candidates.
func collectLocals(body ast.Node, into map[string][]ast.Expr) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.GenDecl:
			collectGenDecl(v, into)
		case *ast.AssignStmt:
			if (v.Tok == token.DEFINE || v.Tok == token.ASSIGN) && len(v.Lhs) == len(v.Rhs) {
				for i, l := range v.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
						into[id.Name] = append(into[id.Name], v.Rhs[i])
					}
				}
			}
		}
		return true
	})
}
