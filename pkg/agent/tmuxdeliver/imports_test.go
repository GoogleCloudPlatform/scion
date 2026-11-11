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

package tmuxdeliver_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestImports_LeafPackage: the package's non-test files import only the
// standard library and pkg/agent/state, so sciontool can use it without the
// agent manager (design v2.22 §3.4). Test files may import more.
func TestImports_LeafPackage(t *testing.T) {
	const allowed = "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range af.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			stdlib := !strings.Contains(first, ".")
			if !stdlib && path != allowed {
				t.Errorf("%s imports %q: only the standard library and %s are allowed", f, path, allowed)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test files checked")
	}
}
