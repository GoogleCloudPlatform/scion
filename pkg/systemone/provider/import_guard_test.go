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

package provider_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scionModulePath = "github.com/GoogleCloudPlatform/scion"
	systemOnePkg    = scionModulePath + "/pkg/systemone"
	providerPkg     = systemOnePkg + "/provider"
	egressPkg       = systemOnePkg + "/egress"
)

// TestImportGuards_SystemOneBoundaries enforces the P1 brief §4 / AC-2 package
// dependency boundaries across the entire pkg/systemone/... tree:
//  1. pkg/systemone/... must not import pkg/hub, pkg/messaging, pkg/store, any
//     trigger package, or (in P1a) pkg/ruleengine / pkg/celengine.
//  2. pkg/systemone/provider/... must not import pkg/secret, pkg/store, or
//     pkg/systemone/runtime.
//  3. Only pkg/systemone/egress may construct an outbound http.Client in
//     non-test Go files under pkg/systemone/...
func TestImportGuards_SystemOneBoundaries(t *testing.T) {
	root := findModuleRoot(t)
	pkgs, err := discoverSubpackages(root, filepath.Join(root, "pkg", "systemone"))
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)

	systemOneForbidden := func(imp string) (string, bool) {
		prefixes := []string{
			scionModulePath + "/pkg/hub",
			scionModulePath + "/pkg/messaging",
			scionModulePath + "/pkg/store",
			scionModulePath + "/pkg/triggers",
			scionModulePath + "/pkg/trigger",
			scionModulePath + "/pkg/ruleengine",
			scionModulePath + "/pkg/celengine",
		}
		for _, p := range prefixes {
			if imp == p || strings.HasPrefix(imp, p+"/") {
				return p, true
			}
		}
		return "", false
	}

	providerForbidden := func(imp string) (string, bool) {
		prefixes := []string{
			scionModulePath + "/pkg/secret",
			scionModulePath + "/pkg/store",
			systemOnePkg + "/runtime",
		}
		for _, p := range prefixes {
			if imp == p || strings.HasPrefix(imp, p+"/") {
				return p, true
			}
		}
		return "", false
	}

	for _, pkg := range pkgs {
		chain, err := findForbiddenImportChain(root, pkg, systemOneForbidden)
		require.NoError(t, err)
		if len(chain) > 0 {
			t.Errorf("%s imports forbidden package via:\n  %s", pkg, strings.Join(chain, "\n  -> "))
		}

		if pkg == providerPkg || strings.HasPrefix(pkg, providerPkg+"/") {
			pChain, err := findForbiddenImportChain(root, pkg, providerForbidden)
			require.NoError(t, err)
			if len(pChain) > 0 {
				t.Errorf("provider package %s imports forbidden package via:\n  %s", pkg, strings.Join(pChain, "\n  -> "))
			}
		}
	}

	httpViolations, err := scanUnauthorizedHTTPClientConstruction(filepath.Join(root, "pkg", "systemone"))
	require.NoError(t, err)
	require.Empty(t, httpViolations, "only pkg/systemone/egress may construct outbound http.Client:\n  %s",
		strings.Join(httpViolations, "\n  "))
}

func TestImportGuards_SelfTestDetectsViolations(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
	}

	write("pkg/systemone/provider/fake/a.go", "package fake\nimport _ \""+scionModulePath+"/pkg/mid\"\n")
	write("pkg/mid/m.go", "package mid\nimport _ \""+scionModulePath+"/pkg/secret\"\n")
	write("pkg/secret/s.go", "package secret\n")

	chain, err := findForbiddenImportChain(tmp, providerPkg+"/fake", func(imp string) (string, bool) {
		if imp == scionModulePath+"/pkg/secret" {
			return imp, true
		}
		return "", false
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		providerPkg + "/fake",
		scionModulePath + "/pkg/mid",
		scionModulePath + "/pkg/secret",
	}, chain)

	// HTTP client AST scanner allows pkg/systemone/egress and _test.go, but flags adapter construction.
	write("pkg/systemone/egress/client.go", "package egress\nimport \"net/http\"\nfunc New() *http.Client { return &http.Client{} }\n")
	write("pkg/systemone/provider/ok_test.go", "package provider\nimport \"net/http\"\nvar _ = &http.Client{}\n")
	vClean, err := scanUnauthorizedHTTPClientConstruction(filepath.Join(tmp, "pkg", "systemone"))
	require.NoError(t, err)
	assert.Empty(t, vClean)

	write("pkg/systemone/provider/bad.go", "package provider\nimport \"net/http\"\nfunc bad() *http.Client { _ = http.DefaultClient; return new(http.Client) }\n")
	vBad, err := scanUnauthorizedHTTPClientConstruction(filepath.Join(tmp, "pkg", "systemone"))
	require.NoError(t, err)
	assert.Len(t, vBad, 2)
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
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

func discoverSubpackages(root, baseDir string) ([]string, error) {
	seen := make(map[string]struct{})
	err := filepath.WalkDir(baseDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != baseDir && (n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		seen[scionModulePath+"/"+filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func findForbiddenImportChain(root, from string, isForbidden func(string) (string, bool)) ([]string, error) {
	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		imps, err := listInModuleImports(root, curr)
		if err != nil {
			return nil, err
		}
		for _, imp := range imps {
			if _, visited := prev[imp]; visited {
				continue
			}
			prev[imp] = curr
			if _, bad := isForbidden(imp); bad {
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

func listInModuleImports(root, pkg string) ([]string, error) {
	dir := root
	if pkg != scionModulePath {
		dir = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, scionModulePath+"/")))
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, spec := range parsed.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, err
			}
			if imp == scionModulePath || strings.HasPrefix(imp, scionModulePath+"/") {
				out = append(out, imp)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func scanUnauthorizedHTTPClientConstruction(systemOneDir string) ([]string, error) {
	egressDir := filepath.Join(systemOneDir, "egress")
	fset := token.NewFileSet()
	var violations []string

	err := filepath.WalkDir(systemOneDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p == egressDir || strings.HasPrefix(p, egressDir+string(filepath.Separator)) {
				return filepath.SkipDir
			}
			if p != systemOneDir && (n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}

		httpAliases := make(map[string]bool)
		for _, imp := range f.Imports {
			pathVal, err := strconv.Unquote(imp.Path.Value)
			if err != nil || pathVal != "net/http" {
				continue
			}
			if imp.Name != nil {
				if imp.Name.Name != "_" && imp.Name.Name != "." {
					httpAliases[imp.Name.Name] = true
				}
			} else {
				httpAliases["http"] = true
			}
		}
		if len(httpAliases) == 0 {
			return nil
		}

		isHTTPSelector := func(expr ast.Expr, selName string) bool {
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != selName {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			return ok && httpAliases[id.Name]
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if isHTTPSelector(node.Type, "Client") || isHTTPSelector(node.Type, "Transport") {
					pos := fset.Position(node.Pos())
					violations = append(violations, fmt.Sprintf("%s:%d: unauthorized net/http literal outside egress", pos.Filename, pos.Line))
				}
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "new" && len(node.Args) == 1 {
					if isHTTPSelector(node.Args[0], "Client") || isHTTPSelector(node.Args[0], "Transport") {
						pos := fset.Position(node.Pos())
						violations = append(violations, fmt.Sprintf("%s:%d: unauthorized new(http.Client/Transport) outside egress", pos.Filename, pos.Line))
					}
				}
				for _, fn := range []string{"Get", "Post", "PostForm", "Head"} {
					if isHTTPSelector(node.Fun, fn) {
						pos := fset.Position(node.Pos())
						violations = append(violations, fmt.Sprintf("%s:%d: direct http.%s bypassing egress Transport", pos.Filename, pos.Line, fn))
					}
				}
			case *ast.SelectorExpr:
				if id, ok := node.X.(*ast.Ident); ok && httpAliases[id.Name] {
					if node.Sel.Name == "DefaultClient" || node.Sel.Name == "DefaultTransport" {
						pos := fset.Position(node.Pos())
						violations = append(violations, fmt.Sprintf("%s:%d: reference to http.%s outside egress", pos.Filename, pos.Line, node.Sel.Name))
					}
				}
			}
			return true
		})
		return nil
	})
	return violations, err
}
