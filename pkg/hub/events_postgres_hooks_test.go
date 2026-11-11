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
	"strings"
	"testing"
)

// postgresEventPublisherTestHooks are the PostgresEventPublisher fields that
// only tests may set. Production code reads them (nil means no hook) and
// never assigns them.
var postgresEventPublisherTestHooks = map[string]bool{
	"testHookBeforeConnect":   true,
	"testHookInUnsubscribe":   true,
	"testHookBeforeCloseSubs": true,
}

// TestPostgresEventPublisher_TestHooksSetOnlyInTests: no non-test file of
// package hub (and its guardScanSubdirs subpackages) assigns a
// PostgresEventPublisher test hook, as an assignment or as a
// composite-literal key, so every hook is nil outside tests.
func TestPostgresEventPublisher_TestHooksSetOnlyInTests(t *testing.T) {
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards PostgresEventPublisher test hook assignments.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards PostgresEventPublisher test hook assignments.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards PostgresEventPublisher test hook assignments.
	entries, err := readGuardScanDirs(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && postgresEventPublisherTestHooks[sel.Sel.Name] {
						t.Errorf("%s assigns %s outside a test file", fset.Position(x.Pos()), sel.Sel.Name)
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && postgresEventPublisherTestHooks[id.Name] {
					t.Errorf("%s sets %s in a composite literal outside a test file", fset.Position(x.Pos()), id.Name)
				}
			case *ast.UnaryExpr:
				if sel, ok := x.X.(*ast.SelectorExpr); ok && x.Op == token.AND && postgresEventPublisherTestHooks[sel.Sel.Name] {
					t.Errorf("%s takes the address of %s outside a test file", fset.Position(x.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no non-test Go files scanned")
	}
}
