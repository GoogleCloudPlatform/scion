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

package secret

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// moduleRoot is the repository root, two levels above pkg/secret.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}

// selectorCallSites returns "<file>:<enclosing func>" for every reference
// to a method or function named name in the non-test Go files of the
// module: any selector x.name (a call, a method value or a method
// expression, inside a function or a package-level func literal) and any
// direct call name(...). extras (separate modules) and web are excluded.
func selectorCallSites(t *testing.T, name string) []string {
	t.Helper()
	root := moduleRoot(t)
	var sites []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch rel {
			case ".git", "extras", "web", "node_modules", "docs-site", ".scion":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		sites = append(sites, referenceSites(f, filepath.ToSlash(rel), name)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(sites)
	return sites
}

// decryptForMigrationCallers is the allow-list of non-test call sites of
// ValueCipher.DecryptForMigration ("<file>:<func>"). Plaintext admission is
// migration-only (ptone/scion#1530 §7): only the administrative migration
// reader (phase 6) may call it, never a runtime path. Adding an entry is a
// security-relevant change.
var decryptForMigrationCallers = map[string]bool{}

func TestDecryptForMigrationCallers(t *testing.T) {
	for _, site := range selectorCallSites(t, "DecryptForMigration") {
		if !decryptForMigrationCallers[site] {
			t.Errorf("DecryptForMigration called outside the migration allow-list: %s", site)
		}
	}
}

// The scanner itself must see references: a known caller of a known name.
func TestSelectorCallSitesFindsKnownCaller(t *testing.T) {
	sites := selectorCallSites(t, "decryptPayload")
	found := false
	for _, s := range sites {
		if s == "pkg/secret/localbackend.go:Resolve" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scanner did not find LocalBackend.Resolve calling decryptPayload: %v", sites)
	}
}

// referenceSites returns "<rel>:<enclosing func>" for every reference to
// name in f. Every reference counts, not only calls: a method value, a
// method expression or a use inside a package-level func literal reaches
// the function as well. Declarations are not references (a FuncDecl name
// is never a selector).
func referenceSites(f *ast.File, rel, name string) []string {
	var sites []string
	enclosing := "<package scope>"
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			prev := enclosing
			enclosing = x.Name.Name
			if x.Body != nil {
				ast.Inspect(x.Body, walk)
			}
			enclosing = prev
			return false
		case *ast.SelectorExpr:
			if x.Sel.Name == name {
				sites = append(sites, rel+":"+enclosing)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == name {
				sites = append(sites, rel+":"+enclosing)
			}
		}
		return true
	}
	for _, decl := range f.Decls {
		ast.Inspect(decl, walk)
	}
	return sites
}

// The scanner sees every reference form, not only calls.
func TestReferenceSitesFindsEveryForm(t *testing.T) {
	const src = `package p
var pkgLit = func(c *T) { _, _ = c.DecryptForMigration("", nil) }
var methodExpr = (*T).DecryptForMigration
func call(c *T)        { c.DecryptForMigration("", nil) }
func methodValue(c *T) { f := c.DecryptForMigration; _ = f }
func (T) DecryptForMigration(string, any) (string, error) { return "", nil }
func unrelated(c *T)   { c.Decrypt("") }
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := referenceSites(f, "p.go", "DecryptForMigration")
	want := []string{"p.go:<package scope>", "p.go:<package scope>", "p.go:call", "p.go:methodValue"}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("referenceSites = %v, want %v", got, want)
	}
}

// TestNoDirectScalarCipherCallers is the call-site audit of
// ptone/scion#1530 Phase 2: every locally stored Hub ciphertext goes
// through the shared ValueCipher, so no non-test code calls the legacy
// scalar helpers. They remain only as compatibility fixtures (and are
// pinned to the cipher's v1 envelope by the golden tests).
func TestNoDirectScalarCipherCallers(t *testing.T) {
	for _, name := range []string{"EncryptValue", "DecryptValue", "DeriveLocalEncryptionKey"} {
		for _, site := range selectorCallSites(t, name) {
			t.Errorf("direct scalar-secret cipher call %s at %s: use the shared secret.ValueCipher", name, site)
		}
	}
}
