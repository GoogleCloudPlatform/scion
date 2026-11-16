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

package triggertemplate_test

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
	scionModulePath     = "github.com/GoogleCloudPlatform/scion"
	triggerTemplatePath = scionModulePath + "/pkg/triggertemplate"
	hubPath             = scionModulePath + "/pkg/hub"
	storePath           = scionModulePath + "/pkg/store"
	messagingPath       = scionModulePath + "/pkg/messaging"
)

// TestImportGuard enforces stage2-design §3 / §16.14:
// pkg/triggertemplate is a pure leaf package and must never directly or
// transitively import pkg/hub, pkg/store, pkg/messaging, or cel-go.
func TestImportGuard(t *testing.T) {
	root := findModuleRoot(t)
	pkgDir := filepath.Join(root, "pkg", "triggertemplate")

	allowedInModule := map[string]bool{
		scionModulePath + "/pkg/api":    true,
		scionModulePath + "/pkg/gitref": true,
		scionModulePath + "/pkg/labels": true,
	}

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
		file, perr := parser.ParseFile(fset, fullPath, nil, parser.ImportsOnly)
		require.NoError(t, perr)

		for _, impSpec := range file.Imports {
			imp, uerr := strconv.Unquote(impSpec.Path.Value)
			require.NoError(t, uerr)

			if imp == hubPath || strings.HasPrefix(imp, hubPath+"/") {
				t.Errorf("%s directly imports forbidden hub package %q", name, imp)
			}
			if imp == storePath || strings.HasPrefix(imp, storePath+"/") {
				t.Errorf("%s directly imports forbidden store package %q", name, imp)
			}
			if imp == messagingPath || strings.HasPrefix(imp, messagingPath+"/") {
				t.Errorf("%s directly imports forbidden messaging package %q", name, imp)
			}
			if strings.HasPrefix(imp, "github.com/google/cel-go") {
				t.Errorf("%s directly imports forbidden cel-go package %q", name, imp)
			}
			if imp == scionModulePath || strings.HasPrefix(imp, scionModulePath+"/") {
				if !allowedInModule[imp] {
					t.Errorf("%s imports unexpected in-module package %q", name, imp)
				}
			} else if strings.Contains(imp, ".") {
				t.Errorf("%s imports unexpected external package %q", name, imp)
			}
		}
	}
	require.Positive(t, prodFiles, "expected production .go files in pkg/triggertemplate")

	for _, forbiddenPrefix := range []string{hubPath, storePath, messagingPath} {
		chain, cerr := findTransitiveImportPrefix(root, triggerTemplatePath, forbiddenPrefix)
		require.NoError(t, cerr)
		if chain != nil {
			t.Errorf("%s must not transitively import %s, found chain: %s",
				triggerTemplatePath, forbiddenPrefix, strings.Join(chain, " -> "))
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

func findTransitiveImportPrefix(root, from, forbiddenPrefix string) ([]string, error) {
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
			if imp == forbiddenPrefix || strings.HasPrefix(imp, forbiddenPrefix+"/") {
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
