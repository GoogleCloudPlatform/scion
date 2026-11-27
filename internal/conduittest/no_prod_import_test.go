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

package conduittest_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath = "github.com/GoogleCloudPlatform/scion"
	selfPath   = modulePath + "/internal/conduittest"
)

// TestNoProductionBinaryImportsConduittest fails if either shipped binary
// (./cmd/scion or ./cmd/sciontool) reaches internal/conduittest through any
// chain of non-test in-module imports. It parses import clauses only (no go
// list, no type-checking) and inspects every non-test .go file regardless of
// build tags, so it over-approximates the real import graph across all OSes.
func TestNoProductionBinaryImportsConduittest(t *testing.T) {
	root := moduleRoot(t)
	for _, entry := range []string{
		modulePath + "/cmd/scion",
		modulePath + "/cmd/sciontool",
	} {
		files, err := nonTestGoFiles(root, entry)
		if err != nil {
			t.Fatalf("inspecting %s: %v", entry, err)
		}
		if len(files) == 0 {
			t.Fatalf("entry package %s has zero non-test .go files; update the guard if the binary moved", entry)
		}
		chain, err := findImport(root, entry, selfPath)
		if err != nil {
			t.Fatalf("walking %s: %v", entry, err)
		}
		if chain != nil {
			t.Fatalf("%s must not import %s, but reaches it via:\n  %s", entry, selfPath, strings.Join(chain, "\n  -> "))
		}
	}
}

// TestFindImportDetectsConduittest verifies that findImport flags both direct
// and transitive non-test imports of internal/conduittest while ignoring
// _test.go imports.
func TestFindImportDetectsConduittest(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entry := modulePath + "/cmd/scion"
	mid := modulePath + "/pkg/mid"

	write("cmd/scion/main.go", "package main\n\nimport _ \""+mid+"\"\n")
	write("cmd/scion/main_test.go", "package main\n\nimport _ \""+selfPath+"\"\n")
	write("pkg/mid/mid.go", "//go:build windows\n\npackage mid\n\nimport _ \""+selfPath+"\"\n")
	write("internal/conduittest/agent.go", "package conduittest\n")

	chain, err := findImport(root, entry, selfPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{entry, mid, selfPath}
	if strings.Join(chain, " ") != strings.Join(want, " ") {
		t.Fatalf("chain = %v, want %v", chain, want)
	}

	// Removing the non-test import leaves only the _test.go import, which is allowed.
	write("pkg/mid/mid.go", "package mid\n")
	chain, err = findImport(root, entry, selfPath)
	if err != nil {
		t.Fatal(err)
	}
	if chain != nil {
		t.Fatalf("chain = %v, want none (only _test.go imports conduittest)", chain)
	}

	// A missing/moved entry package must report zero non-test .go files so
	// TestNoProductionBinaryImportsConduittest cannot pass vacuously.
	missingFiles, err := nonTestGoFiles(root, modulePath+"/cmd/doesnotexist")
	if err != nil {
		t.Fatal(err)
	}
	if len(missingFiles) != 0 {
		t.Fatalf("missing entry returned %v, want empty", missingFiles)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

func findImport(root, from, target string) ([]string, error) {
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		imps, err := packageImports(root, pkg)
		if err != nil {
			return nil, err
		}
		for _, imp := range imps {
			if _, seen := prev[imp]; seen {
				continue
			}
			prev[imp] = pkg
			if imp == target {
				var chain []string
				for p := imp; p != ""; p = prev[p] {
					chain = append([]string{p}, chain...)
				}
				return chain, nil
			}
			queue = append(queue, imp)
		}
	}
	return nil, nil
}

func nonTestGoFiles(root, pkg string) ([]string, error) {
	dir := root
	if pkg != modulePath {
		dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, modulePath+"/")))
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	return out, nil
}

func packageImports(root, pkg string) ([]string, error) {
	names, err := nonTestGoFiles(root, pkg)
	if err != nil {
		return nil, err
	}
	var out []string
	fset := token.NewFileSet()
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, s := range f.Imports {
			ip, err := strconv.Unquote(s.Path.Value)
			if err != nil {
				return nil, err
			}
			if ip == modulePath || strings.HasPrefix(ip, modulePath+"/") {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}
