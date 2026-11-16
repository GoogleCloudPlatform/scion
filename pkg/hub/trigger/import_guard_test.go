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

package trigger_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	scionModulePath = "github.com/GoogleCloudPlatform/scion"
	hubPkgPath      = scionModulePath + "/pkg/hub"
	triggerPkgPath  = scionModulePath + "/pkg/hub/trigger"
)

// TestNoHubImportGuard enforces stage1-design §11 D-S1-1(c): pkg/hub/trigger
// must never import pkg/hub directly or transitively (sharing types only via
// leaf packages such as pkg/hub/hubfire and pkg/hub/internal/principal).
func TestNoHubImportGuard(t *testing.T) {
	root := findModuleRoot(t)
	pkgDir := filepath.Join(root, "pkg", "hub", "trigger")

	entries, err := os.ReadDir(pkgDir)
	require.NoError(t, err)

	fset := token.NewFileSet()
	var prodFiles int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		prodFiles++
		fullPath := filepath.Join(pkgDir, name)
		src, rerr := os.ReadFile(fullPath)
		require.NoError(t, rerr)
		file, perr := parser.ParseFile(fset, fullPath, src, parser.ImportsOnly)
		require.NoError(t, perr)

		for _, impSpec := range file.Imports {
			imp, uerr := strconv.Unquote(impSpec.Path.Value)
			require.NoError(t, uerr)
			if imp == hubPkgPath {
				t.Errorf("%s directly imports forbidden parent package %q", name, imp)
			}
		}
	}
	require.Positive(t, prodFiles, "expected production .go files in pkg/hub/trigger")

	chain, err := findTransitiveImport(root, triggerPkgPath, hubPkgPath)
	require.NoError(t, err)
	if chain != nil {
		t.Errorf("%s must not transitively import %s, found chain: %s",
			triggerPkgPath, hubPkgPath, strings.Join(chain, " -> "))
	}
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found above working directory")
		dir = parent
	}
}

func findTransitiveImport(root, from, target string) ([]string, error) {
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		imps, err := readInModuleImports(root, pkg)
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

func readInModuleImports(root, pkg string) ([]string, error) {
	dir := root
	if pkg != scionModulePath {
		dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, scionModulePath+"/")))
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	fset := token.NewFileSet()
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(n)
		if rerr != nil {
			return nil, rerr
		}
		f, perr := parser.ParseFile(fset, n, src, parser.ImportsOnly)
		if perr != nil {
			return nil, perr
		}
		for _, impSpec := range f.Imports {
			imp, uerr := strconv.Unquote(impSpec.Path.Value)
			if uerr != nil {
				return nil, uerr
			}
			if imp == scionModulePath || strings.HasPrefix(imp, scionModulePath+"/") {
				out = append(out, imp)
			}
		}
	}
	return out, nil
}
