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

package entc

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestNoDirectSchemaCreateOutsideEnt fails when Go code in this module, tests
// included, calls x.Schema.Create directly outside pkg/ent. Such a call skips
// the process-wide lock in AutoMigrate, so two of them in parallel tests can
// crash the test binary with "fatal error: concurrent map writes"
// (ptone/scion#4993). Call AutoMigrate instead. pkg/ent is exempt: it holds
// AutoMigrate itself, its own tests and the generated ent code.
func TestNoDirectSchemaCreateOutsideEnt(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	exempt := filepath.Join(root, "pkg", "ent")

	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if path == exempt || name[0] == '.' || name == "node_modules" || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			// A nested go.mod starts another module (extras/*).
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if !bytes.Contains(src, []byte("Schema.Create")) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, pos := range directSchemaCreateCalls(t, rel, src) {
			t.Errorf("%s: direct Schema.Create call; use entc.AutoMigrate", pos)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no Go files under %s", root)
	}
}

// TestDirectSchemaCreateCallsDetects keeps the guard above from passing
// vacuously: the detector must report a call and ignore a comment.
func TestDirectSchemaCreateCallsDetects(t *testing.T) {
	src := []byte(`package p

// client.Schema.Create(ctx) in a comment is not a call.
func f() {
	_ = client.Schema.Create(ctx)
	_ = cs.client.Schema.Create(ctx, opt)
	_ = AutoMigrate(ctx, client)
}
`)
	got := directSchemaCreateCalls(t, "p.go", src)
	want := []string{"p.go:5:6", "p.go:6:6"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// directSchemaCreateCalls returns the file:line:col of every call of the
// form x.Schema.Create(...) in src.
func directSchemaCreateCalls(t *testing.T, name string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Create" {
			return true
		}
		if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "Schema" {
			out = append(out, fset.Position(call.Pos()).String())
		}
		return true
	})
	return out
}
