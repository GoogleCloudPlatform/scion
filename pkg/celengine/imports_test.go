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

package celengine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/GoogleCloudPlatform/scion"

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

// TestNoDomainImports verifies that pkg/celengine remains completely consumer-neutral
// and imports zero in-module packages (no pkg/hub, pkg/store, pkg/messaging, etc.).
func TestNoDomainImports(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "pkg", "celengine")
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
			if strings.HasPrefix(imp, modulePath+"/") {
				t.Errorf("pkg/celengine/%s must not import in-module package %q", ent.Name(), imp)
			}
		}
	}
}

// TestOnlyCelEngineImportsCelGo enforces stage1-design.md §7.0: pkg/celengine
// is the ONLY importer of github.com/google/cel-go in the entire repository.
func TestOnlyCelEngineImportsCelGo(t *testing.T) {
	root := moduleRoot(t)
	allowedDir := filepath.Join(root, "pkg", "celengine")
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "build" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if filepath.Dir(path) == allowedDir {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if imp == "github.com/google/cel-go" || strings.HasPrefix(imp, "github.com/google/cel-go/") ||
				imp == "cel.dev/cel-go" || strings.HasPrefix(imp, "cel.dev/cel-go/") {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports %q; only pkg/celengine may import cel-go", rel, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPublicAPIDoesNotExposeCelGo verifies no exported declaration in pkg/celengine
// leaks a cel-go type in its fields, parameters, or results.
func TestPublicAPIDoesNotExposeCelGo(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "pkg", "celengine")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, ent.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		celAliases := map[string]string{}
		for _, spec := range f.Imports {
			imp, _ := strconv.Unquote(spec.Path.Value)
			if imp == "github.com/google/cel-go" || strings.HasPrefix(imp, "github.com/google/cel-go/") ||
				imp == "cel.dev/cel-go" || strings.HasPrefix(imp, "cel.dev/cel-go/") {
				alias := filepath.Base(imp)
				if spec.Name != nil {
					alias = spec.Name.Name
				}
				celAliases[alias] = imp
			}
		}
		checkExpr := func(contextDesc string, expr ast.Expr) {
			ast.Inspect(expr, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if ok {
					if imp, isCel := celAliases[id.Name]; isCel {
						t.Errorf("%s/%s: %s exposes cel-go type %s.%s (%s)", ent.Name(), contextDesc, contextDesc, id.Name, sel.Sel.Name, imp)
					}
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Type.Params != nil {
					for _, p := range d.Type.Params.List {
						checkExpr("func "+d.Name.Name+" param", p.Type)
					}
				}
				if d.Type.Results != nil {
					for _, r := range d.Type.Results.List {
						checkExpr("func "+d.Name.Name+" result", r.Type)
					}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					switch st := ts.Type.(type) {
					case *ast.StructType:
						for _, fld := range st.Fields.List {
							exported := len(fld.Names) == 0
							for _, nm := range fld.Names {
								if nm.IsExported() {
									exported = true
								}
							}
							if exported {
								checkExpr("struct "+ts.Name.Name+" field", fld.Type)
							}
						}
					case *ast.InterfaceType:
						for _, m := range st.Methods.List {
							checkExpr("interface "+ts.Name.Name+" method", m.Type)
						}
					default:
						checkExpr("type "+ts.Name.Name, ts.Type)
					}
				}
			}
		}
	}
}
