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

// The sqlite-only tag: these tests use declReceiverName from authz_sqlite_helpers_test.go.
//go:build !no_sqlite

package hub

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// grandfatheredWrite is one production write of a delegation edge's
// Grandfathered flag: the file (relative to the repository root), the
// enclosing function (receiver.name) and the kind of write.
type grandfatheredWrite struct{ file, fn, kind string }

// grandfatheredColumn matches the stored column's name as a word.
var grandfatheredColumn = regexp.MustCompile(`(?i)\bgrandfathered\b`)

// messageCallPackages and messageCallMethods name the calls whose string
// arguments are messages, not writes: fmt, errors and log/slog formatting
// and logging (at any depth of the argument, so a Sprintf inside another
// call is covered).
var (
	messageCallPackages = map[string]bool{"fmt": true, "errors": true, "log": true, "slog": true}
	messageCallMethods  = map[string]bool{
		"Debug": true, "Info": true, "Warn": true, "Error": true,
		"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
		"Log": true, "LogAttrs": true, "Printf": true, "Println": true, "Errorf": true,
	}
)

// sqlShaped matches a string that looks like SQL: it is never skipped as a
// message, even under a fmt call (fmt.Sprintf builds SQL too).
var sqlShaped = regexp.MustCompile(`(?i)\b(UPDATE|INSERT|SET|WHERE|VALUES|ALTER|CREATE)\b`)

// isQueryCall reports whether call runs SQL (Exec*, Query*, QueryRow*): a
// message skip never descends into one.
func isQueryCall(call *ast.CallExpr) bool {
	name := ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	}
	return strings.HasPrefix(name, "Exec") || strings.HasPrefix(name, "Query")
}

// isMessageCall reports whether call formats or logs a message.
func isMessageCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && messageCallPackages[pkg.Name] {
		return true
	}
	return messageCallMethods[sel.Sel.Name]
}

// grandfatheredWritesIn returns the writes of the Grandfathered flag in
// one parsed file:
//   - a Grandfathered field in a composite literal (field_true, field_false,
//     field_copy of another edge's flag, or field_other);
//   - a SetGrandfathered or SetNillableGrandfathered call (set_* and
//     setnillable_*, with the same value kinds);
//   - an assignment to a .Grandfathered field (assign);
//   - a string literal naming the grandfathered column: as a key of a
//     composite literal (column_* with the value kind), or anywhere else,
//     for example in raw SQL or a schema definition (raw_string).
//
// Two contexts are not writes and are skipped: struct tags, and string
// arguments of fmt, errors and log/slog calls (isMessageCall). A
// SQL-shaped literal is never skipped (fmt.Sprintf builds SQL too), and
// the skip does not descend into a nested Exec*/Query* call. The same
// literal anywhere else, for example a map key or an Exec argument, is
// still reported.
func grandfatheredWritesIn(file *ast.File, rel string) []grandfatheredWrite {
	var out []grandfatheredWrite
	kindOf := func(v ast.Expr) string {
		switch x := v.(type) {
		case *ast.Ident:
			if x.Name == "true" || x.Name == "false" {
				return x.Name
			}
		case *ast.SelectorExpr:
			if x.Sel.Name == "Grandfathered" {
				return "copy"
			}
		}
		return "other"
	}
	isColumn := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(lit.Value)
		return err == nil && grandfatheredColumn.MatchString(v)
	}
	visit := func(fn string, root ast.Node) {
		keys := map[ast.Expr]bool{}
		// skip marks the message literals under e: not SQL-shaped ones, and
		// nothing inside a nested query call.
		skip := func(e ast.Node) {
			ast.Inspect(e, func(m ast.Node) bool {
				switch y := m.(type) {
				case *ast.CallExpr:
					if isQueryCall(y) {
						return false
					}
				case *ast.BasicLit:
					if v, err := strconv.Unquote(y.Value); y.Kind != token.STRING || err != nil || !sqlShaped.MatchString(v) {
						keys[y] = true
					}
				}
				return true
			})
		}
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Field:
				if x.Tag != nil {
					keys[x.Tag] = true // a struct tag is not a write
				}
			case *ast.CallExpr:
				if isMessageCall(x) {
					for _, a := range x.Args {
						skip(a)
					}
				}
			}
			switch x := n.(type) {
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Grandfathered" {
					out = append(out, grandfatheredWrite{rel, fn, "field_" + kindOf(x.Value)})
				}
				if isColumn(x.Key) {
					keys[x.Key] = true
					out = append(out, grandfatheredWrite{rel, fn, "column_" + kindOf(x.Value)})
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					prefix := ""
					switch sel.Sel.Name {
					case "SetGrandfathered":
						prefix = "set_"
					case "SetNillableGrandfathered":
						prefix = "setnillable_"
					}
					if prefix != "" {
						kind := prefix + "other"
						if len(x.Args) == 1 {
							kind = prefix + kindOf(x.Args[0])
						}
						out = append(out, grandfatheredWrite{rel, fn, kind})
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Grandfathered" {
						out = append(out, grandfatheredWrite{rel, fn, "assign"})
					}
				}
			case *ast.BasicLit:
				if !keys[x] && isColumn(x) {
					out = append(out, grandfatheredWrite{rel, fn, "raw_string"})
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				visit(declReceiverName(d), d.Body)
			}
		default:
			visit("", d)
		}
	}
	return out
}

// derefEdgeCopiesIn returns, for one parsed file, each CreateDelegationEdge
// call whose edge argument is a dereference copy of another edge: &*x, or
// a local assigned from *x (ne := *old, var ne = *old, ne = *old). Such a
// copy carries the Grandfathered flag through the store's pass-through
// without naming the field, so the writer pin would not see it.
func derefEdgeCopiesIn(file *ast.File, rel string) []grandfatheredWrite {
	var out []grandfatheredWrite
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		copies := map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, lhs := range x.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							if _, deref := x.Rhs[i].(*ast.StarExpr); deref {
								copies[id.Name] = true
							}
						}
					}
				}
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if i < len(x.Values) {
						if _, deref := x.Values[i].(*ast.StarExpr); deref {
							copies[name.Name] = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "CreateDelegationEdge" {
				return true
			}
			for _, arg := range call.Args {
				target := arg
				if u, ok := target.(*ast.UnaryExpr); ok && u.Op == token.AND {
					target = u.X
				}
				if p, ok := target.(*ast.ParenExpr); ok {
					target = p.X
				}
				deref := false
				switch x := target.(type) {
				case *ast.StarExpr:
					deref = true
				case *ast.Ident:
					deref = copies[x.Name]
				}
				if deref {
					out = append(out, grandfatheredWrite{rel, declReceiverName(fd), "deref_copy"})
				}
			}
			return true
		})
	}
	return out
}

// grandfatheredSQLWrites returns one raw_sql write for a SQL file that
// names the grandfathered column.
func grandfatheredSQLWrites(src []byte, rel string) []grandfatheredWrite {
	if grandfatheredColumn.Match(src) {
		return []grandfatheredWrite{{rel, "", "raw_sql"}}
	}
	return nil
}

// TestDelegationEdgeGrandfatheredWriters pins the writers of a delegation
// edge's Grandfathered flag, provenance metadata that marks an edge written
// by the backfill or copied by adoption: in production code (pkg, cmd and
// internal; of pkg/ent only the hand-written schema, the rest is generated
// from it) a delegation edge's Grandfathered flag is set to true only by
// the delegation-edge backfill, copied onto a new edge only by adoption
// (delegationadoption.AdoptedEdge), and otherwise only passed through by
// the store (the create of a caller's edge and the read conversion). The
// column is named only by its schema definition and by the fixture
// generator (as false), and no SQL file names it. Struct tags and message
// strings of fmt, errors and log/slog calls are not writes. No CreateDelegationEdge
// call receives a dereference copy of another edge, which would carry the
// flag without naming it. A planted write of each kind is detected
// (negative control).
func TestDelegationEdgeGrandfatheredWriters(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	fset := token.NewFileSet()
	var got []grandfatheredWrite
	files := 0
	for _, top := range []string{"pkg", "cmd", "internal"} {
		// pkgmove:scan-covers pkg/hub/...
		// recursive walk of pkg (so every pkg/hub subdirectory), cmd and internal; only testdata and node_modules are skipped
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(rel, "pkg/ent/") && !strings.HasPrefix(rel, "pkg/ent/schema/") {
				return nil
			}
			isSQL := strings.HasSuffix(path, ".sql")
			if !isSQL && (!strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go")) {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			files++
			if isSQL {
				got = append(got, grandfatheredSQLWrites(src, rel)...)
				return nil
			}
			if !grandfatheredColumn.Match(src) && !bytes.Contains(src, []byte("CreateDelegationEdge")) {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			got = append(got, grandfatheredWritesIn(file, rel)...)
			got = append(got, derefEdgeCopiesIn(file, rel)...)
			return nil
		})
		require.NoError(t, err)
	}
	require.Greater(t, files, 100, "the scan covers the production tree")

	want := []grandfatheredWrite{
		{"internal/fixturegen/spec.go", "Spec", "column_false"},
		{"pkg/delegationadoption/apply.go", "AdoptedEdge", "field_copy"},
		{"pkg/ent/schema/delegationedge.go", "DelegationEdge.Fields", "raw_string"},
		{"pkg/store/entadapter/composite.go", "CompositeStore.BackfillDelegationEdges", "set_true"},
		{"pkg/store/entadapter/delegation_edge_store.go", "DelegationEdgeStore.CreateDelegationEdge", "set_copy"},
		{"pkg/store/entadapter/delegation_edge_store.go", "entDelegationEdgeToStore", "field_copy"},
	}
	sortWrites := func(s []grandfatheredWrite) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].file != s[j].file {
				return s[i].file < s[j].file
			}
			if s[i].fn != s[j].fn {
				return s[i].fn < s[j].fn
			}
			return s[i].kind < s[j].kind
		})
	}
	sortWrites(got)
	assert.Equal(t, want, got, "a delegation edge's Grandfathered flag is written only by the backfill, adoption and the store pass-through")

	// Negative control: a planted writer of each kind is detected.
	planted, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package p\n"+
		"func reissue(old, e *Edge, b *Builder, db *DB, flag *bool) {\n"+
		"\t_ = &Edge{Grandfathered: true}\n"+
		"\t_ = &Edge{Grandfathered: old.Grandfathered}\n"+
		"\tb.SetGrandfathered(true)\n"+
		"\tb.SetNillableGrandfathered(flag)\n"+
		"\te.Grandfathered = true\n"+
		"\t_ = map[string]any{\"grandfathered\": true}\n"+
		"\tdb.Exec(\"UPDATE delegation_edges SET grandfathered = 1\")\n"+
		"}\n", parser.SkipObjectResolution)
	require.NoError(t, err)
	gotPlanted := grandfatheredWritesIn(planted, "planted.go")
	sortWrites(gotPlanted)
	wantPlanted := []grandfatheredWrite{
		{"planted.go", "reissue", "assign"},
		{"planted.go", "reissue", "column_true"},
		{"planted.go", "reissue", "field_copy"},
		{"planted.go", "reissue", "field_true"},
		{"planted.go", "reissue", "raw_string"},
		{"planted.go", "reissue", "set_true"},
		{"planted.go", "reissue", "setnillable_other"},
	}
	assert.Equal(t, wantPlanted, gotPlanted)

	// Excluded contexts: a struct tag and message strings of fmt, errors and
	// log/slog calls are skipped, while the same literal as a map key or an
	// Exec argument in the same file is still reported.
	contexts, err := parser.ParseFile(token.NewFileSet(), "contexts.go", "package p\n"+
		"type Stored struct {\n"+
		"\tGrandfathered bool `json:\"grandfathered\"`\n"+
		"}\n"+
		"func describe(ctx context.Context, db *DB, e *Edge) error {\n"+
		"\taddStep(\"step\", fmt.Sprintf(\"edge %s has grandfathered provenance (audit only)\", e.ID))\n"+
		"\tslog.WarnContext(ctx, \"grandfathered edge\", \"grandfathered\", true)\n"+
		"\tlogger.Info(\"grandfathered\")\n"+
		"\t_ = errors.New(\"grandfathered edge\")\n"+
		"\t_ = map[string]any{\"grandfathered\": true}\n"+
		"\tdb.Exec(\"UPDATE delegation_edges SET grandfathered = 1\")\n"+
		"\tdb.Exec(fmt.Sprintf(\"UPDATE delegation_edges SET grandfathered = %d\", v))\n"+
		"\tslog.Info(\"x\", \"r\", db.Exec(\"UPDATE delegation_edges SET grandfathered = 1\"))\n"+
		"\treturn fmt.Errorf(\"grandfathered: %w\", errX)\n"+
		"}\n", parser.SkipObjectResolution)
	require.NoError(t, err)
	gotContexts := grandfatheredWritesIn(contexts, "contexts.go")
	sortWrites(gotContexts)
	assert.Equal(t, []grandfatheredWrite{
		{"contexts.go", "describe", "column_true"},
		{"contexts.go", "describe", "raw_string"},
		{"contexts.go", "describe", "raw_string"},
		{"contexts.go", "describe", "raw_string"},
	}, gotContexts, "the tag and the message strings are skipped; the map key, the Exec argument, SQL built with fmt.Sprintf and an Exec nested in a log call are reported")
	copies, err := parser.ParseFile(token.NewFileSet(), "copies.go", "package p\n"+
		"func reRecord(ctx context.Context, tx Tx, old *Edge) {\n"+
		"\tne := *old\n"+
		"\tne.ID = \"\"\n"+
		"\t_ = tx.CreateDelegationEdge(ctx, &ne)\n"+
		"\tvar ve = *old\n"+
		"\t_ = tx.CreateDelegationEdge(ctx, &ve)\n"+
		"\t_ = tx.CreateDelegationEdge(ctx, &(*old))\n"+
		"\t_ = tx.CreateDelegationEdge(ctx, &Edge{ID: \"x\"})\n"+
		"\t_ = tx.CreateDelegationEdge(ctx, old)\n"+
		"}\n", parser.SkipObjectResolution)
	require.NoError(t, err)
	assert.Equal(t, []grandfatheredWrite{
		{"copies.go", "reRecord", "deref_copy"},
		{"copies.go", "reRecord", "deref_copy"},
		{"copies.go", "reRecord", "deref_copy"},
	}, derefEdgeCopiesIn(copies, "copies.go"), "three dereference copies reach the create; a literal and a passed pointer do not")
	assert.Equal(t, []grandfatheredWrite{{"planted.sql", "", "raw_sql"}},
		grandfatheredSQLWrites([]byte("UPDATE delegation_edges SET Grandfathered = TRUE;"), "planted.sql"))
	assert.Empty(t, grandfatheredSQLWrites([]byte("UPDATE agents SET agent_role = 'full';"), "clean.sql"),
		"a SQL file that does not name the column is not reported")
}

// delegationEdgeDecodeCall is one JSON decode, in a request-serving
// package, whose target holds a store.DelegationEdge.
type delegationEdgeDecodeCall struct{ file, fn, call string }

// decodeFuncs are the JSON decoders a handler can read a body with.
var decodeFuncs = map[string]bool{
	"Unmarshal": true, "Decode": true, "readJSON": true, "readJSONStrict": true, "decodeJSONNumbers": true,
}

// delegationEdgeDecodes returns, for the files of one package, the decode
// calls whose target's type holds a store.DelegationEdge: directly (a
// value, pointer, slice or map of it), or through the fields of a struct
// type declared in the package. A target is a variable declared in the
// enclosing function (var, :=, a parameter) or a composite literal.
func delegationEdgeDecodes(files map[string]*ast.File) []delegationEdgeDecodeCall {
	structs := map[string]*ast.StructType{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					if st, ok := ts.Type.(*ast.StructType); ok {
						structs[ts.Name.Name] = st
					}
				}
			}
		}
	}
	var holds func(e ast.Expr, seen map[string]bool) bool
	holds = func(e ast.Expr, seen map[string]bool) bool {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "store" && x.Sel.Name == "DelegationEdge" {
				return true
			}
		case *ast.StarExpr:
			return holds(x.X, seen)
		case *ast.ArrayType:
			return holds(x.Elt, seen)
		case *ast.MapType:
			return holds(x.Key, seen) || holds(x.Value, seen)
		case *ast.StructType:
			for _, fld := range x.Fields.List {
				if holds(fld.Type, seen) {
					return true
				}
			}
		case *ast.Ident:
			if st, ok := structs[x.Name]; ok && !seen[x.Name] {
				seen[x.Name] = true
				return holds(st, seen)
			}
		}
		return false
	}
	var out []delegationEdgeDecodeCall
	for rel, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			vars := map[string]ast.Expr{}
			if fd.Type.Params != nil {
				for _, p := range fd.Type.Params.List {
					for _, n := range p.Names {
						vars[n.Name] = p.Type
					}
				}
			}
			typeOfValue := func(v ast.Expr) ast.Expr {
				switch x := v.(type) {
				case *ast.CompositeLit:
					return x.Type
				case *ast.UnaryExpr:
					if cl, ok := x.X.(*ast.CompositeLit); ok {
						return cl.Type
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
						return x.Args[0]
					}
				}
				return nil
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.ValueSpec:
					for i, name := range x.Names {
						switch {
						case x.Type != nil:
							vars[name.Name] = x.Type
						case i < len(x.Values):
							if t := typeOfValue(x.Values[i]); t != nil {
								vars[name.Name] = t
							}
						}
					}
				case *ast.AssignStmt:
					if x.Tok == token.DEFINE && len(x.Lhs) == len(x.Rhs) {
						for i, lhs := range x.Lhs {
							if id, ok := lhs.(*ast.Ident); ok {
								if t := typeOfValue(x.Rhs[i]); t != nil {
									vars[id.Name] = t
								}
							}
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if !decodeFuncs[name] {
					return true
				}
				for _, arg := range call.Args {
					target := arg
					if u, ok := target.(*ast.UnaryExpr); ok && u.Op == token.AND {
						target = u.X
					}
					var typ ast.Expr
					switch x := target.(type) {
					case *ast.Ident:
						typ = vars[x.Name]
					case *ast.CompositeLit:
						typ = x.Type
					}
					if typ != nil && holds(typ, map[string]bool{}) {
						out = append(out, delegationEdgeDecodeCall{rel, declReceiverName(fd), name})
					}
				}
				return true
			})
		}
	}
	return out
}

// TestNoHandlerDecodesDelegationEdge: no request-serving package (the hub
// and the runtime broker) decodes a store.DelegationEdge, or a type that
// holds one, from JSON. Its Grandfathered field carries a JSON tag, so a
// decoded request body could set the flag
// (TestDelegationEdgeGrandfatheredWriters pins only Go writes). Planted
// handlers that decode one directly and through a request struct are
// detected; a decode into another type is not (negative controls).
func TestNoHandlerDecodesDelegationEdge(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	// pkg/hub and each guardScanSubdirs directory under it, then pkg/runtimebroker, one package per scan
	// (delegationEdgeDecodes indexes the struct types of one package).
	hubDir := filepath.Join("pkg", "hub")
	var dirs []string
	for _, rel := range mustGuardScanDirs(t, filepath.Join(root, hubDir)) {
		dirs = append(dirs, filepath.ToSlash(filepath.Join(hubDir, rel)))
	}
	if !slices.Contains(dirs, "pkg/hub") {
		dirs = append([]string{"pkg/hub"}, dirs...)
	}
	dirs = append(dirs, "pkg/runtimebroker")
	require.Contains(t, dirs, "pkg/hub", "the decode scan covers pkg/hub itself")
	require.Contains(t, dirs, "pkg/runtimebroker", "the decode scan covers pkg/runtimebroker")
	for _, dir := range dirs {
		fset := token.NewFileSet()
		// pkgmove:scan-covers guardScanSubdirs
		// pkg/hub and every guardScanSubdirs directory (mustGuardScanDirs above), one ParseDir per package; pkg/runtimebroker is outside pkg/hub
		pkgs, err := parser.ParseDir(fset, filepath.Join(root, filepath.FromSlash(dir)), func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, parser.SkipObjectResolution)
		require.NoError(t, err)
		files := map[string]*ast.File{}
		for _, p := range pkgs {
			for path, f := range p.Files {
				rel, rerr := filepath.Rel(root, path)
				require.NoError(t, rerr)
				files[filepath.ToSlash(rel)] = f
			}
		}
		if dir == "pkg/hub" || dir == "pkg/runtimebroker" {
			require.Greater(t, len(files), 10, "%s: the scan covers the package", dir)
		}
		assert.Empty(t, delegationEdgeDecodes(files), "%s decodes a delegation edge from JSON", dir)
	}

	planted, err := parser.ParseFile(token.NewFileSet(), "planted.go", `package p
type importEdgesRequest struct {
	Edges []*store.DelegationEdge `+"`json:\"edges\"`"+`
}
type otherRequest struct{ Name string }
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importEdgesRequest
	_ = readJSON(r, &req)
}
func (s *Server) handleEdge(w http.ResponseWriter, r *http.Request) {
	e := &store.DelegationEdge{}
	_ = json.NewDecoder(r.Body).Decode(e)
}
func (s *Server) handleOther(w http.ResponseWriter, r *http.Request) {
	var o otherRequest
	_ = json.NewDecoder(r.Body).Decode(&o)
}
`, parser.SkipObjectResolution)
	require.NoError(t, err)
	got := delegationEdgeDecodes(map[string]*ast.File{"planted.go": planted})
	sort.Slice(got, func(i, j int) bool { return got[i].fn < got[j].fn })
	assert.Equal(t, []delegationEdgeDecodeCall{
		{"planted.go", "Server.handleEdge", "Decode"},
		{"planted.go", "Server.handleImport", "readJSON"},
	}, got)
}
