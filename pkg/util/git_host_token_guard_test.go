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

package util

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoUnscopedHostGitCredentials is a static guard: host-side git must
// authenticate only through a host-scoped credential helper
// (ScopedCredentialHelperEnv), never an unscoped credential.helper or a token
// embedded in a URL. It parses the host git packages (comments are ignored)
// and fails if a string literal reintroduces either pattern, or if the retired
// TokenCredentialHelperEnv is declared again.
//
// Out of scope by design (not scanned): pkg/config/remote_templates.go (the
// template cache; its host fetch/checkout carry the pins and skip-smudge), the
// in-container agent credential helper written to the agent's own .gitconfig,
// and the in-container sciontool init path.
func TestNoUnscopedHostGitCredentials(t *testing.T) {
	root := repoRootForGuard(t)
	pkgs := []string{"pkg/util", "pkg/provision", "pkg/agent", "pkg/runtimebroker"}
	for _, pkg := range pkgs {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			scanGoFileForTokenPatterns(t, filepath.Join(dir, name))
		}
	}
}

func scanGoFileForTokenPatterns(t *testing.T, path string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0) // no comments
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == "TokenCredentialHelperEnv" {
				t.Errorf("%s: TokenCredentialHelperEnv is retired; use ScopedCredentialHelperEnv", path)
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			v := x.Value // includes quotes/backticks; fine for substring checks
			// An unscoped credential.helper key (the scoped form is
			// "credential.%s://%s.helper", which does not contain this).
			// git_host.go's ScopedCredentialHelperEnv legitimately emits an
			// empty `credential.helper` reset before the scoped helper; that is
			// the one sanctioned use.
			if strings.Contains(v, "credential.helper") && filepath.Base(path) != "git_host.go" {
				t.Errorf("%s:%d: unscoped 'credential.helper' literal; host git must use ScopedCredentialHelperEnv", path, fset.Position(x.Pos()).Line)
			}
			// A token embedded in a URL (userinfo), for host git.
			if strings.Contains(v, "oauth2:") || strings.Contains(v, "x-access-token:") {
				t.Errorf("%s:%d: token-in-URL construction; host git must not embed credentials in the URL", path, fset.Position(x.Pos()).Line)
			}
		case *ast.SelectorExpr:
			// url.UserPassword(...) embeds credentials in a URL.
			if x.Sel.Name == "UserPassword" {
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "url" {
					t.Errorf("%s:%d: url.UserPassword builds a credential-bearing URL; host git must not embed credentials in the URL", path, fset.Position(x.Pos()).Line)
				}
			}
		}
		return true
	})
}

func repoRootForGuard(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Tests run in pkg/util; the module root is two levels up.
	root := filepath.Join(wd, "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate repo root from %s: %v", wd, err)
	}
	return root
}
