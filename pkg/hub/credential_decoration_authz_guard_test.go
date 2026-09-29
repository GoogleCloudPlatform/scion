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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// E.1 guard: no production code outside the designated carriage points may
// reference credential decoration. Decoration is descriptive, server-derived
// metadata; authorization decisions must never branch on it.
//
// Review finding F2 (e1-review-1.md): the first version of this guard only
// scanned files named authz*.go/authorize*.go, and only matched the exact
// identifier "CredentialDecoration" — so a reference through the
// CredentialDecorationFromContext accessor, or a reference in any other file
// (capabilities.go, authorized_list.go, audit_authz.go, auth.go, ...), passed
// silently. This version scans every non-test production file in pkg/hub and
// flags the type, the accessor, the field/method selector, and the
// decoration-carrying constructor. TestScanDecorationReferences_CatchesEveryReferenceForm
// below is a self-test proving the scanner actually catches each reference
// form the review found undetected, so this guard cannot silently regress
// to a no-op again.
// ---------------------------------------------------------------------------

// decorationGuardAllowed lists the exact "file.go:function" locations
// permitted to reference credential decoration. These are the only carriage
// and rendering sites: where ValidateToken derives it, where
// credentialContextForIdentity copies it onto the credential context, the
// accessor/constructor definitions themselves in identity.go, and
// credential_decoration.go's own type/rendering methods (review-2 finding 1
// hardening: function-level entries, not a whole-file exemption, so a
// decision helper added to that file later is still scanned like any other
// function in the package).
//
// decorateDecision is deliberately NOT here: it references no decoration
// today, so pre-authorizing it would grant E.2's future access before E.2
// exists. E.2 adds its own entry here, explicitly, when it adds a real
// reference.
var decorationGuardAllowed = map[string]bool{
	"identity.go:NewScopedUserIdentityWithDecoration":          true,
	"identity.go:Decoration":                                   true,
	"useraccesstoken.go:ValidateToken":                         true,
	"authz.go:credentialContextForIdentity":                    true,
	"credential_decoration.go:IsZero":                          true,
	"credential_decoration.go:LogValue":                        true,
	"credential_decoration.go:clone":                           true,
	"credential_decoration.go:CredentialDecorationFromContext": true,

	// E.2a (ptone/scion#2127, plan §3.1-§3.3): these are rendering/audit
	// builders, not authorization decisions — they read decoration to log or
	// snapshot it, never to decide anything. See plan §2.1's rule: decoration
	// carriage/rendering is allowed; branching on it in authorization code is
	// not.
	"identity.go:requestAuthAttrs":            true,
	"audit_actor.go:auditActorFromContext":    true,
	"audit_authz.go:BuildDecisionAuditRecord": true,
	"audit.go:credentialLogAttr":              true,
}

// decorationHit is one reference to credential decoration found by
// scanDecorationReferences.
type decorationHit struct {
	file     string
	function string
	line     int
	symbol   string
}

// decorationSymbols are the identifiers/selectors that indicate a reference
// to E.1's credential decoration.
var decorationSymbols = map[string]bool{
	"CredentialDecoration":                true,
	"CredentialDecorationFromContext":     true,
	"NewScopedUserIdentityWithDecoration": true,
}

// scanDecorationReferences parses a single Go source file (given as a string
// so it can be unit-tested against synthetic fixtures, not just real files
// on disk) and returns every reference to credential decoration: the
// CredentialDecoration type, CredentialDecorationFromContext,
// NewScopedUserIdentityWithDecoration, or a selector matching "Decoration"
// case-insensitively (which catches the exported `.Decoration` field, the
// `Decoration()` accessor method call, AND — review-2 finding 1 — a direct
// read of the unexported `s.decoration` field on ScopedUserIdentity, or of a
// promoted `.decoration`/`.Decoration` field on any struct embedding
// *CredentialDecoration). References that occur inside a top-level type
// declaration (e.g. CredentialContext's own `Decoration *CredentialDecoration`
// field, or ScopedUserIdentity's `decoration *CredentialDecoration` field)
// are not reported: declaring a carriage field is expected and is not
// executable logic that can branch on the value.
func scanDecorationReferences(filename, src string) ([]decorationHit, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}

	type span struct{ start, end token.Pos }
	var typeDecls []span
	for _, decl := range f.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
			typeDecls = append(typeDecls, span{gd.Pos(), gd.End()})
		}
	}
	inTypeDecl := func(pos token.Pos) bool {
		for _, s := range typeDecls {
			if pos >= s.start && pos <= s.end {
				return true
			}
		}
		return false
	}

	var hits []decorationHit
	ast.Inspect(f, func(n ast.Node) bool {
		var symbol string
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if strings.EqualFold(node.Sel.Name, "Decoration") {
				symbol = node.Sel.Name
			}
		case *ast.Ident:
			if decorationSymbols[node.Name] {
				symbol = node.Name
			}
		}
		if symbol == "" {
			return true
		}
		if inTypeDecl(n.Pos()) {
			return true
		}
		pos := fset.Position(n.Pos())
		hits = append(hits, decorationHit{
			file:     filename,
			function: enclosingDeclName(f, n.Pos()),
			line:     pos.Line,
			symbol:   symbol,
		})
		return true
	})
	return hits, nil
}

// enclosingDeclName returns the name of the function declaration containing
// pos, covering the WHOLE declaration (signature and body), not just the
// body. enclosingFuncName (ast_test_helpers_test.go) only matches inside a
// function's body block, so a decoration reference in a signature — for
// example the `decoration *CredentialDecoration` parameter or the
// `*CredentialDecoration` return type on the carriage constructor/accessor
// themselves — would otherwise resolve to "<unknown>" and be misreported as
// a violation instead of being attributed to the (allowed) declaring
// function.
func enclosingDeclName(f *ast.File, pos token.Pos) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if pos >= fn.Pos() && pos <= fn.End() {
			return fn.Name.Name
		}
	}
	return "<unknown>"
}

// scanDirForDecorationReferences walks dir (non-recursively skips testdata
// and vendor, like the other guard tests in this package) and runs
// scanDecorationReferences over every non-test .go file, including
// credential_decoration.go itself: its legitimate references are covered by
// function-level decorationGuardAllowed entries, not a whole-file exemption
// (review-2 finding 1 hardening), so a decision helper added to that file
// later is still scanned like any other function in the package.
func scanDirForDecorationReferences(dir string) ([]decorationHit, error) {
	var hits []decorationHit
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "testdata" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fileHits, scanErr := scanDecorationReferences(name, string(src))
		if scanErr != nil {
			return fmt.Errorf("parsing %s: %w", path, scanErr)
		}
		hits = append(hits, fileHits...)
		return nil
	})
	return hits, err
}

// TestCredentialDecorationNotReadByAuthzCode mechanically enforces the E.1
// rule that credential decoration never influences an authorization
// decision: no production code outside decorationGuardAllowed's specific
// function-level entries may reference it.
func TestCredentialDecorationNotReadByAuthzCode(t *testing.T) {
	hubDir := findHubDir(t)
	hits, err := scanDirForDecorationReferences(hubDir)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("found zero decoration references anywhere in pkg/hub — the scanner is broken (it should at least find the allowed carriage sites)")
	}
	for _, h := range hits {
		key := h.file + ":" + h.function
		if !decorationGuardAllowed[key] {
			t.Errorf("production code references credential decoration outside the designated carriage points: %s references %s at %s:%d\n"+
				"  Decoration is descriptive only; authorization decisions must not branch on it.\n"+
				"  Allowed locations: %s", h.function, h.symbol, h.file, h.line, strings.Join(allowedDecorationGuardKeys(), ", "))
		}
	}
}

// TestScanDecorationReferences_CatchesEveryReferenceForm is the guard's
// self-test: it feeds the scanner one fixture per reference form the review
// found undetected against the first version of this guard (e1-review-1.md,
// F2), so a future refactor of the scanner cannot silently narrow it back to
// a no-op.
func TestScanDecorationReferences_CatchesEveryReferenceForm(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		// F2 probe 1: a direct field read on the credential context, in a
		// file that (before the fix) would have matched the authz* pattern.
		"authz_direct_field.go": `package hub

func directFieldReference(req AuthzRequest) bool {
	return req.Credential.Decoration != nil
}
`,
		// F2 probe 2: reading decoration through the front-door accessor
		// instead of the field directly. The old guard only matched the
		// exact identifier "CredentialDecoration", not this accessor call.
		"authz_accessor.go": `package hub

import "context"

func accessorReference(ctx context.Context) bool {
	d, _ := CredentialDecorationFromContext(ctx)
	return d.Labels["x"] == "y"
}
`,
		// F2 probe 3: the same direct-field read, but in a file whose name
		// does not match authz*/authorize* at all — the old guard's
		// file-name filter never looked at this file.
		"capabilities_mut3.go": `package hub

func otherFileReference(req AuthzRequest) bool {
	return req.Credential.Decoration != nil
}
`,
		// Review-2 finding 1: a direct read of the unexported
		// ScopedUserIdentity.decoration field. Every file in pkg/hub shares
		// the package, so any of them can name this lowercase field
		// directly, without going through the exported Decoration()
		// accessor at all. The exact-case-only match in the first fix after
		// review-1 missed this (0 hits when probed).
		"lowercase_field.go": `package hub

func lowerFieldReference(id Identity) bool {
	if s, ok := id.(*ScopedUserIdentity); ok && s.decoration != nil {
		return s.decoration.Labels["role_hint"] == "admin"
	}
	return false
}
`,
	}
	want := map[string]string{
		"authz_direct_field.go:directFieldReference": "Decoration",
		"authz_accessor.go:accessorReference":        "CredentialDecorationFromContext",
		"capabilities_mut3.go:otherFileReference":    "Decoration",
		"lowercase_field.go:lowerFieldReference":     "decoration",
	}

	for name, src := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}

	hits, err := scanDirForDecorationReferences(dir)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	got := make(map[string]string, len(hits))
	for _, h := range hits {
		got[h.file+":"+h.function] = h.symbol
	}
	for key, wantSymbol := range want {
		gotSymbol, ok := got[key]
		if !ok {
			t.Errorf("scanner failed to catch expected reference form %s (all hits: %v)", key, got)
			continue
		}
		if gotSymbol != wantSymbol {
			t.Errorf("scanner caught %s but with symbol %q, want %q", key, gotSymbol, wantSymbol)
		}
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
