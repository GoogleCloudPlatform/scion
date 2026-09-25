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

// TestSkillResourceLiterals_AllUseCanonicalConstructor is the durable guard
// for ptone/scion#1901 finding F3/F4.
//
// canUseProjectGitHubToken built its own Resource{Type: "skill", ...}
// literal and forgot ScopeKind. filterHubWideSkillGrants, at the time,
// only narrowed the curated hub-member/hub-viewer system-scoped
// skill.read/skill.list grant when ScopeKind was exactly "user" or
// "project" — so the ScopeKind-less literal fell through unfiltered, and
// any hub member could make the Hub mint any project's GitHub App token.
//
// Two fixes closed this: (1) filterHubWideSkillGrants now fails closed
// (filters unless ScopeKind is explicitly "global"/"core"), and (2) every
// ad hoc "skill" Resource is built through skillResource (from a real
// store.Skill) or skillScopeResource (from a bare scope/scopeID pair), both
// of which always set ScopeKind. This test is the guard against a third
// call site reintroducing a hand-built literal that skips both.
//
// It scans every non-test .go file in this package for composite literals
// of the form Resource{... Type: "skill" ...} and fails if one appears
// outside skillResource/skillScopeResource themselves. It fails on zero
// matches too (scanner self-check — see msg_containment_callsite_test.go
// for the same pattern), so it cannot silently stop scanning anything.
func TestSkillResourceLiterals_AllUseCanonicalConstructor(t *testing.T) {
	dir := findHubDir(t)

	allowedFuncs := map[string]bool{
		"skillResource":      true,
		"skillScopeResource": true,
	}

	fset := token.NewFileSet()
	totalSkillResourceLiterals := 0

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
			if !literalHasSkillType(lit) {
				return true
			}
			totalSkillResourceLiterals++

			fn := enclosingFuncName(fset, f, fset.Position(lit.Pos()).Offset)
			if !allowedFuncs[fn] {
				pos := fset.Position(lit.Pos())
				t.Errorf(
					"%s:%d: hand-built Resource{Type: \"skill\", ...} literal in %s — "+
						"build it via skillResource(skill) or skillScopeResource(scope, scopeID) instead, "+
						"or filterHubWideSkillGrants's fail-closed check (ptone/scion#1901 F4) will silently "+
						"deny every non-admin caller (and if ScopeKind is left unset, review whether a future "+
						"relaxation of that check could reopen F3)",
					name, pos.Line, fn)
			}
			return true
		})
	}

	if totalSkillResourceLiterals == 0 {
		t.Fatal("scanner found zero Resource{Type: \"skill\", ...} literals — " +
			"the AST matcher is broken (skillResource/skillScopeResource themselves must match)")
	}
}

// literalHasSkillType reports whether a Resource{...} composite literal sets
// Type: "skill".
func literalHasSkillType(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Type" {
			continue
		}
		val, ok := kv.Value.(*ast.BasicLit)
		if !ok || val.Kind != token.STRING {
			continue
		}
		unquoted := strings.Trim(val.Value, `"`)
		return unquoted == "skill"
	}
	return false
}
