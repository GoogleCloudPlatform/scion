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

package ruleengine_test

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
	celEnginePath = modulePath + "/pkg/celengine"
)

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

// TestLeafImports verifies that pkg/ruleengine only imports pkg/celengine and
// standard library packages (never pkg/hub, pkg/store, pkg/messaging, or cel-go).
func TestLeafImports(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "pkg", "ruleengine")
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
			if strings.HasPrefix(imp, modulePath+"/") && imp != celEnginePath {
				t.Errorf("pkg/ruleengine/%s imports disallowed in-module package %q", ent.Name(), imp)
			}
			if strings.Contains(imp, ".") && !strings.HasPrefix(imp, modulePath+"/") {
				t.Errorf("pkg/ruleengine/%s imports disallowed external package %q", ent.Name(), imp)
			}
		}
	}
}
