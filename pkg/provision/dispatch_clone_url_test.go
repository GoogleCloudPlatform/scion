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

package provision

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CheckDispatchCloneURL passes an empty URL and the https, ssh and git forms,
// refuses every form CheckHostCloneURL refuses, and passes a local repository
// only while a running test has it registered (AllowLocalCloneFixture).
func TestCheckDispatchCloneURL(t *testing.T) {
	for _, u := range []string{"", "https://github.com/org/repo.git", "ssh://git@github.com/org/repo.git", "git@github.com:org/repo.git", "git://github.com/org/repo.git"} {
		if err := CheckDispatchCloneURL(u); err != nil {
			t.Errorf("CheckDispatchCloneURL(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range []string{"/srv/repo", "file:///srv/repo", "http://git.example.com/org/repo.git", "ext::sh -c x", "-uoption"} {
		if err := CheckDispatchCloneURL(u); !errors.Is(err, ErrHostCloneURLRefused) {
			t.Errorf("CheckDispatchCloneURL(%q) = %v, want ErrHostCloneURLRefused", u, err)
		}
	}

	local := filepath.Join(t.TempDir(), "fixture-repo")
	t.Run("registered", func(t *testing.T) {
		AllowLocalCloneFixture(t, local)
		if err := CheckDispatchCloneURL(local); err != nil {
			t.Errorf("a registered fixture must pass, got %v", err)
		}
		if err := CheckDispatchCloneURL("file://" + local); !errors.Is(err, ErrHostCloneURLRefused) {
			t.Errorf("only the exact registered URL passes; file:// form = %v", err)
		}
	})
	if err := CheckDispatchCloneURL(local); !errors.Is(err, ErrHostCloneURLRefused) {
		t.Errorf("the registration ends with the test that made it; got %v", err)
	}
}

// The local clone fixture exemption is unreachable from production code: no
// non-test Go file in the module except clone_fixture.go (which defines it)
// references AllowLocalCloneFixture, the registry or its lookup. Production
// code reaches the exemption only through CheckDispatchCloneURL /
// checkProvisionCloneURL, whose registry lookup is false outside a test
// binary (testing.Testing). A future production caller fails this test.
func TestLocalCloneFixture_UnreachableFromProduction(t *testing.T) {
	// Tests run in their package directory (pkg/provision), so the module
	// root is two levels up; no git checkout is needed.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locate the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	// Go scoping decides what can reach the registry: the unexported
	// localCloneFixtures / isLocalCloneFixture only from package provision
	// itself, the exported AllowLocalCloneFixture from any package under any
	// import name (or a dot-import). Other packages may declare unrelated
	// unexported names of their own, for example pkg/hub's local clone
	// fixture seam.
	unexported := map[string]bool{"localCloneFixtures": true, "isLocalCloneFixture": true, "AllowLocalCloneFixture": true}
	provisionDir := filepath.Join(root, "pkg", "provision")
	definition := filepath.Join(provisionDir, "clone_fixture.go")
	scanned, found := 0, 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			// Another Go module (extras/*) is outside this module.
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil // not buildable source of this module
		}
		scanned++
		rel := strings.TrimPrefix(path, root+"/")
		inProvision := filepath.Dir(path) == provisionDir
		ast.Inspect(file, func(n ast.Node) bool {
			x, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case path == definition && unexported[x.Name]:
				found++
			case inProvision && unexported[x.Name]:
				t.Errorf("non-test file %s references %s: the local clone fixture exemption must stay test-only", rel, x.Name)
			case !inProvision && x.Name == "AllowLocalCloneFixture":
				// Any qualifier (an aliased import) or a dot-import: the
				// exported name appears as an identifier either way.
				t.Errorf("non-test file %s references AllowLocalCloneFixture: the local clone fixture exemption must stay test-only", rel)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	// Fixture checks: the scan saw the module and the definition itself.
	if scanned < 500 {
		t.Fatalf("scanned only %d non-test files; the walk did not cover the module", scanned)
	}
	if found == 0 {
		t.Fatalf("the definition file %s was not seen; the scan is not looking at the right tree", definition)
	}
}
