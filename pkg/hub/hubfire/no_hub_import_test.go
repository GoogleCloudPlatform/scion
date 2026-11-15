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

package hubfire_test

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
	modulePath    = "github.com/GoogleCloudPlatform/scion"
	hubPath       = modulePath + "/pkg/hub"
	selfPath      = hubPath + "/hubfire"
	storePath     = modulePath + "/pkg/store"
	principalPath = hubPath + "/internal/principal"
)

// TestNoHubImport verifies that pkg/hub/hubfire stays a strict leaf: its
// production files may only import standard library packages, pkg/store, and
// pkg/hub/internal/principal, and it never reaches pkg/hub through any chain
// of in-module imports (including test imports of hubfire itself).
func TestNoHubImport(t *testing.T) {
	root := moduleRoot(t)
	allowedInModule := map[string]bool{
		storePath:     true,
		principalPath: true,
	}
	dir := filepath.Join(root, "pkg", "hub", "hubfire")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, ent.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(imp, modulePath+"/") && !allowedInModule[imp] {
				t.Fatalf("%s/%s imports disallowed in-module package %q (hubfire must remain a leaf of store + internal/principal)", selfPath, ent.Name(), imp)
			}
			if strings.Contains(imp, ".") && !strings.HasPrefix(imp, modulePath+"/") {
				t.Fatalf("%s/%s imports disallowed external package %q", selfPath, ent.Name(), imp)
			}
		}
	}

	chain, err := findImport(root, selfPath, hubPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if chain != nil {
		t.Fatalf("%s must not import %s, but reaches it via:\n  %s", selfPath, hubPath, strings.Join(chain, "\n  -> "))
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
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

func findImport(root, from, target string, includeRootTests bool) ([]string, error) {
	type node struct {
		pkg  string
		prev int
	}
	seen := map[string]bool{from: true}
	queue := []node{{pkg: from, prev: -1}}
	fset := token.NewFileSet()

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		rel := strings.TrimPrefix(cur.pkg, modulePath+"/")
		dir := filepath.Join(root, filepath.FromSlash(rel))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") {
				continue
			}
			if strings.HasSuffix(ent.Name(), "_test.go") && !(includeRootTests && cur.pkg == from) {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, ent.Name()), nil, parser.ImportsOnly)
			if err != nil {
				return nil, err
			}
			for _, spec := range f.Imports {
				imp, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return nil, err
				}
				if imp == target {
					chain := []string{target}
					for i := head; i >= 0; i = queue[i].prev {
						chain = append([]string{queue[i].pkg}, chain...)
						if queue[i].prev < 0 {
							break
						}
					}
					return chain, nil
				}
				if !strings.HasPrefix(imp, modulePath+"/") || seen[imp] {
					continue
				}
				seen[imp] = true
				queue = append(queue, node{pkg: imp, prev: head})
			}
		}
	}
	return nil, nil
}
