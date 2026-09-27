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
	"strings"
	"testing"
)

// TestHarnessConfigResourceLiterals_AllUseCanonicalConstructor is the
// durable guard for ptone/scion#1916, mirroring
// TestSkillResourceLiterals_AllUseCanonicalConstructor (ptone/scion#1901
// finding F3/F4) for the "harness_config" resource type.
//
// filterHubWideHarnessConfigGrants only narrows the curated hub-member/hub-
// viewer system-scoped harness_config.read/harness_config.list grant when
// the target harness config's ScopeKind is populated. A hand-built
// Resource{Type: "harness_config", ...} literal that forgets ScopeKind
// would fall through that filter unfiltered, reopening the same class of
// leak the skill fix closed.
//
// It scans every non-test .go file in this package for composite literals
// of the form Resource{... Type: "harness_config" ...} and fails if one
// appears outside harnessConfigResource/harnessConfigScopeResource
// themselves. It fails on zero matches too (scanner self-check — see
// msg_containment_callsite_test.go for the same pattern), so it cannot
// silently stop scanning anything.
func TestHarnessConfigResourceLiterals_AllUseCanonicalConstructor(t *testing.T) {
	dir := findHubDir(t)

	allowedFuncs := map[string]bool{
		"harnessConfigResource":      true,
		"harnessConfigScopeResource": true,
	}

	fset := token.NewFileSet()
	total := 0

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read %s: %v", dir, err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", path, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "Resource" {
				return true
			}
			if !literalHasResourceType(lit, "harness_config") {
				return true
			}
			total++

			fn := enclosingFuncName(fset, f, fset.Position(lit.Pos()).Offset)
			if !allowedFuncs[fn] {
				pos := fset.Position(lit.Pos())
				t.Errorf(
					"%s:%d: hand-built Resource{Type: \"harness_config\", ...} literal in %s — "+
						"build it via harnessConfigResource(hc) or harnessConfigScopeResource(scope, scopeID) "+
						"instead, or filterHubWideHarnessConfigGrants's fail-closed check (ptone/scion#1916) "+
						"will silently deny every non-admin caller",
					name, pos.Line, fn)
			}
			return true
		})
	}

	if total == 0 {
		t.Fatal("scanner found zero Resource{Type: \"harness_config\", ...} literals — " +
			"the AST matcher is broken (harnessConfigResource/harnessConfigScopeResource themselves must match)")
	}
}
