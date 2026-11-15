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

package durablequeue_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scionModulePath  = "github.com/GoogleCloudPlatform/scion"
	durableQueuePath = scionModulePath + "/pkg/durablequeue"
	storePath        = scionModulePath + "/pkg/store"
	hubPath          = scionModulePath + "/pkg/hub"
	messagingPath    = scionModulePath + "/pkg/messaging"
)

// TestImportAndGeneralityGuard enforces stage1-design §6.1 / §16.5:
// pkg/durablequeue must not import pkg/hub or pkg/messaging, its only in-module
// direct import may be pkg/store, and its non-test files must contain no
// trigger- or webhook-specific types or identifiers.
func TestImportAndGeneralityGuard(t *testing.T) {
	root := findModuleRoot(t)
	pkgDir := filepath.Join(root, "pkg", "durablequeue")

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
		file, perr := parser.ParseFile(fset, fullPath, src, parser.ParseComments)
		require.NoError(t, perr)

		for _, impSpec := range file.Imports {
			imp, uerr := strconv.Unquote(impSpec.Path.Value)
			require.NoError(t, uerr)
			if imp == hubPath || strings.HasPrefix(imp, hubPath+"/") {
				t.Errorf("%s directly imports forbidden hub package %q", name, imp)
			}
			if imp == messagingPath || strings.HasPrefix(imp, messagingPath+"/") {
				t.Errorf("%s directly imports forbidden messaging package %q", name, imp)
			}
			if imp == scionModulePath || strings.HasPrefix(imp, scionModulePath+"/") {
				assert.Equal(t, storePath, imp,
					"%s imports unexpected in-module package %q (only pkg/store is allowed)", name, imp)
			}
			lowerImp := strings.ToLower(imp)
			assert.NotContains(t, lowerImp, "trigger", "%s imports non-generic package %q", name, imp)
			assert.NotContains(t, lowerImp, "webhook", "%s imports non-generic package %q", name, imp)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			lower := strings.ToLower(id.Name)
			assert.NotContains(t, lower, "trigger", "non-generic identifier %q in %s", id.Name, name)
			assert.NotContains(t, lower, "webhook", "non-generic identifier %q in %s", id.Name, name)
			return true
		})
	}
	require.Positive(t, prodFiles, "expected production .go files in pkg/durablequeue")

	// Also verify the transitive in-module import graph never reaches pkg/hub,
	// pkg/hub/trigger, or pkg/messaging (or any pkg/messaging subpackage).
	for _, target := range []string{hubPath, hubPath + "/trigger", messagingPath} {
		chain, cerr := findTransitiveImport(root, durableQueuePath, target)
		require.NoError(t, cerr)
		if chain != nil {
			t.Errorf("%s must not transitively import %s, found chain: %s",
				durableQueuePath, target, strings.Join(chain, " -> "))
		}
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
			if imp == target || (target == messagingPath && strings.HasPrefix(imp, messagingPath+"/")) {
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
		f, err := parser.ParseFile(fset, n, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, s := range f.Imports {
			ip, err := strconv.Unquote(s.Path.Value)
			if err != nil {
				return nil, err
			}
			if ip == scionModulePath || strings.HasPrefix(ip, scionModulePath+"/") {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}
