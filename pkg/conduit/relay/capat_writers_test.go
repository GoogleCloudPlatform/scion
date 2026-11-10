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

package relay_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The reduce-only invariant: a session's lifetime cap (entry.capAt) is set
// once, when Serve starts the session, and never moved afterwards. This
// test fails on any new writer of capAt in the relay package's production
// code.

// capAtWriter is one place that writes a capAt field.
type capAtWriter struct {
	fn   string // enclosing function ("" outside any function)
	kind string
	pos  token.Position
}

// capAtWriters returns every write of a field named capAt in file: an
// assignment or ++/-- whose left side is x.capAt, a composite literal key
// capAt, or &x.capAt (a pointer that could be written through).
func capAtWriters(fset *token.FileSet, file *ast.File) []capAtWriter {
	var out []capAtWriter
	isCapAt := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "capAt"
	}
	visit := func(fn string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if isCapAt(lhs) {
						out = append(out, capAtWriter{fn, "assign", fset.Position(lhs.Pos())})
					}
				}
			case *ast.IncDecStmt:
				if isCapAt(x.X) {
					out = append(out, capAtWriter{fn, "incdec", fset.Position(x.Pos())})
				}
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "capAt" {
					out = append(out, capAtWriter{fn, "literal", fset.Position(x.Pos())})
				}
			case *ast.UnaryExpr:
				if x.Op == token.AND && isCapAt(x.X) {
					out = append(out, capAtWriter{fn, "address", fset.Position(x.Pos())})
				}
			}
			return true
		})
	}
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok {
						name = "(*" + id.Name + ")." + name
					}
				}
			}
			visit(name, fd)
			continue
		}
		visit("", d)
	}
	return out
}

// TestCapAt_SingleWriter: across the relay package's production files,
// capAt has exactly one writer, the assignment in (*Relay).Serve. Zero
// writers fails too, so a scan that finds nothing cannot pass.
func TestCapAt_SingleWriter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no relay sources found (%v)", err)
	}
	var writers []capAtWriter
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		writers = append(writers, capAtWriters(fset, f)...)
	}
	if len(writers) != 1 || writers[0].fn != "(*Relay).Serve" || writers[0].kind != "assign" {
		for _, w := range writers {
			t.Errorf("capAt written in %q (%s) at %s", w.fn, w.kind, w.pos)
		}
		t.Fatalf("capAt has %d writers, want exactly 1: the assignment in (*Relay).Serve (the cap is set once and never moved)", len(writers))
	}
}

// TestCapAt_ScannerFindsEveryWriterKind checks the scanner itself on a
// source with one writer of each kind it must catch.
func TestCapAt_ScannerFindsEveryWriterKind(t *testing.T) {
	const src = `package x

func (r *Relay) Serve(e *entry) { e.capAt = now() }

func (r *Relay) extend(e *entry) {
	e.capAt, ok = later(), true
	e.capAt++
	p := &e.capAt
	_ = &entry{capAt: later()}
	_ = p
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, w := range capAtWriters(fset, f) {
		kinds[w.fn+" "+w.kind]++
	}
	want := map[string]int{
		"(*Relay).Serve assign":   1,
		"(*Relay).extend assign":  1,
		"(*Relay).extend incdec":  1,
		"(*Relay).extend address": 1,
		"(*Relay).extend literal": 1,
	}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("scanner found %d %q writers, want %d (all: %v)", kinds[k], k, n, kinds)
		}
	}
	if len(kinds) != len(want) {
		t.Errorf("scanner found unexpected writers: %v", kinds)
	}
}
