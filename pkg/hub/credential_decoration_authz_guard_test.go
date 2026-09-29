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

package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// decorationGuardAllowed lists the exact "file.go:function" locations
// permitted to reference CredentialDecoration or a Decoration field/method in
// authz*.go / authorize*.go production files. Decoration is descriptive,
// server-derived metadata (E.1): authorization decisions must never branch on
// it. Only the designated carriage points may touch it — the place that
// copies it from an identity onto the credential context
// (credentialContextForIdentity), and the place that copies it from the
// credential context onto a Decision for audit (decorateDecision, and any
// future E.2 audit builder added here explicitly).
var decorationGuardAllowed = map[string]bool{
	"authz.go:credentialContextForIdentity": true,
	"authz.go:decorateDecision":             true,
}

// TestCredentialDecorationNotReadByAuthzCode mechanically enforces the E.1
// rule that credential decoration (token name/purpose/labels) never
// influences an authorization decision: no authz*.go / authorize*.go
// production code outside decorationGuardAllowed may reference
// CredentialDecoration or a Decoration field/method access.
func TestCredentialDecorationNotReadByAuthzCode(t *testing.T) {
	hubDir := findHubDir(t)
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("failed to read hub directory: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !(strings.HasPrefix(name, "authz") || strings.HasPrefix(name, "authorize")) {
			continue
		}
		checked++

		path := filepath.Join(hubDir, name)
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("failed to parse %s: %v", name, parseErr)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			var hit bool
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if node.Sel.Name == "Decoration" || node.Sel.Name == "CredentialDecoration" {
					hit = true
				}
			case *ast.Ident:
				if node.Name == "CredentialDecoration" {
					hit = true
				}
			}
			if !hit {
				return true
			}
			pos := fset.Position(n.Pos())
			fn := enclosingFuncName(fset, f, pos.Offset)
			if fn == "<unknown>" {
				// Outside any function body: a type/struct-field declaration
				// (e.g. CredentialContext.Decoration's own field type), not
				// executable authorization logic. Declaring the carriage
				// field is expected; only reading it to make a decision is
				// the thing this guard forbids.
				return true
			}
			key := name + ":" + fn
			if !decorationGuardAllowed[key] {
				t.Errorf("authorization code references credential decoration outside the designated carriage points: %s (line %d)\n"+
					"  Decoration is descriptive only; authorization decisions must not branch on it.\n"+
					"  Allowed locations: %s", key, pos.Line, strings.Join(allowedDecorationGuardKeys(), ", "))
			}
			return true
		})
	}

	if checked == 0 {
		t.Fatal("found zero authz*.go/authorize*.go files — the scanner is broken or the files were renamed")
	}
}

func allowedDecorationGuardKeys() []string {
	keys := make([]string, 0, len(decorationGuardAllowed))
	for k := range decorationGuardAllowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
