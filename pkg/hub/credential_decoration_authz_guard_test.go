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
// silently. That version was fixed to scan every non-test production file in
// pkg/hub.
//
// Review-3 finding 1 (e1-review-3.md, blocking): that fixed version still
// exempted every type declaration wholesale, and keyed its allow-list by
// bare function name (ignoring the receiver) and by base filename (ignoring
// the path). So a NEW carrier field, a type alias, or an embedded
// *CredentialDecoration let code read decoration with zero guard hits, and a
// same-named method on an unrelated receiver was silently allowed. This
// version:
//   - reports every field, embedded field, or alias/defined type inside a
//     type declaration whose type expression mentions CredentialDecoration,
//     keyed "file:Type.Field" or "file:type Alias";
//   - keys function-level hits by receiver, e.g. "(CredentialDecoration).clone"
//     or "(*ScopedUserIdentity).Decoration", not just the bare function name;
//   - keys files by their path relative to pkg/hub, not the base name.
//
// TestScanDecorationReferences_CatchesEveryReferenceForm is a self-test
// proving the scanner actually catches each reference form both reviews
// found undetected, so this guard cannot silently regress to a no-op again.
// ---------------------------------------------------------------------------

// decorationGuardAllowed lists the exact locations permitted to reference
// credential decoration: the two carrier fields, the type's own defining
// declaration, and the specific (possibly receiver-qualified) functions that
// derive, copy, or render it. Any new carrier field, alias, embedding, or
// same-named method on a different receiver fails until someone deliberately
// adds an entry here.
//
// decorateDecision is deliberately NOT here: it references no decoration
// today, so pre-authorizing it would grant E.2's future access before E.2
// exists. E.2 adds its own entry here, explicitly, when it adds a real
// reference.
var decorationGuardAllowed = map[string]bool{
	// Type-declaration carriage points.
	"authz.go:CredentialContext.Decoration":              true,
	"identity.go:ScopedUserIdentity.decoration":          true,
	"credential_decoration.go:type CredentialDecoration": true,

	// Function/method-level carriage and rendering points.
	"identity.go:NewScopedUserIdentityWithDecoration":            true,
	"identity.go:NewScopedUserIdentityWithCeilingAndDecoration":  true,
	"identity.go:(*ScopedUserIdentity).Decoration":               true,
	"useraccesstoken.go:(*UserAccessTokenService).ValidateToken": true,
	"authz.go:credentialContextForIdentity":                      true,
	"credential_decoration.go:(CredentialDecoration).IsZero":     true,
	"credential_decoration.go:(CredentialDecoration).LogValue":   true,
	"credential_decoration.go:(CredentialDecoration).clone":      true,
	"credential_decoration.go:CredentialDecorationFromContext":   true,

	// E.2a (ptone/scion#2127, plan §3.1-§3.3): these are rendering/audit
	// builders, not authorization decisions — they read decoration to log or
	// snapshot it, never to decide anything. See plan §2.1's rule: decoration
	// carriage/rendering is allowed; branching on it in authorization code is
	// not. None of these are methods, so they need no receiver qualification.
	"identity.go:requestAuthAttrs":            true,
	"audit_actor.go:auditActorFromContext":    true,
	"audit_authz.go:BuildDecisionAuditRecord": true,
	"audit.go:credentialLogAttr":              true,

	// E.2b (ptone/scion#2127, plan §3.5): same rule as the E.2a group above —
	// this renders a bounded, sanitized snapshot of decoration for the
	// initiator_credential_snapshot column, it never decides anything.
	"scheduled_initiator.go:initiatorCredentialSnapshotJSON": true,
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
	"CredentialDecoration":                          true,
	"CredentialDecorationFromContext":               true,
	"NewScopedUserIdentityWithDecoration":           true,
	"NewScopedUserIdentityWithCeilingAndDecoration": true,
}

// exprMentionsCredentialDecoration reports whether expr's type expression
// names CredentialDecoration anywhere within it (directly, through a
// pointer, or through a selector), so a field, embed, or alias of any shape
// is caught, not just a bare `CredentialDecoration`/`*CredentialDecoration`.
func exprMentionsCredentialDecoration(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "CredentialDecoration" {
			found = true
			return false
		}
		return true
	})
	return found
}

// embeddedFieldName derives the field name Go itself would assign to an
// embedded field from its type expression (e.g. "CredentialDecoration" for
// an embedded `*CredentialDecoration`).
func embeddedFieldName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return embeddedFieldName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	default:
		return "?"
	}
}

// scanTypeDecls reports every field, embedded field, or alias/defined type
// inside a type declaration whose type expression mentions
// CredentialDecoration, plus the type's own defining declaration
// (`type CredentialDecoration ...`) wherever it appears. Review-3 finding 1:
// these were previously exempt from the scan entirely.
func scanTypeDecls(fset *token.FileSet, f *ast.File, filename string) []decorationHit {
	var hits []decorationHit
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if ts.Name.Name == "CredentialDecoration" {
				hits = append(hits, decorationHit{
					file: filename, function: "type CredentialDecoration",
					line: fset.Position(ts.Pos()).Line, symbol: "type-def",
				})
			}
			switch st := ts.Type.(type) {
			case *ast.StructType:
				if st.Fields == nil {
					continue
				}
				for _, field := range st.Fields.List {
					if !exprMentionsCredentialDecoration(field.Type) {
						continue
					}
					line := fset.Position(field.Pos()).Line
					if len(field.Names) == 0 {
						// Embedded field: no explicit name in source.
						name := embeddedFieldName(field.Type)
						hits = append(hits, decorationHit{
							file: filename, function: ts.Name.Name + "." + name,
							line: line, symbol: "type-field",
						})
						continue
					}
					for _, n := range field.Names {
						hits = append(hits, decorationHit{
							file: filename, function: ts.Name.Name + "." + n.Name,
							line: line, symbol: "type-field",
						})
					}
				}
			default:
				// A type alias (`type X = CredentialDecoration`) or a defined
				// type (`type X CredentialDecoration`) over it. Excludes the
				// CredentialDecoration definition itself, already handled above.
				if ts.Name.Name != "CredentialDecoration" && exprMentionsCredentialDecoration(ts.Type) {
					hits = append(hits, decorationHit{
						file: filename, function: "type " + ts.Name.Name,
						line: fset.Position(ts.Pos()).Line, symbol: "type-alias",
					})
				}
			}
		}
	}
	return hits
}

// scanDecorationReferences parses a single Go source file (given as a string
// so it can be unit-tested against synthetic fixtures, not just real files
// on disk) and returns every reference to credential decoration:
//   - the CredentialDecoration type, CredentialDecorationFromContext, or
//     NewScopedUserIdentityWithDecoration identifiers, wherever they appear
//     outside a type declaration;
//   - a selector matching "Decoration" case-insensitively (the exported
//     `.Decoration` field, the `Decoration()` accessor call, or a direct
//     read of the unexported `s.decoration` field, by that exact name);
//   - (review-3 finding 1) every field, embedded field, or alias/defined
//     type declared inside a type declaration whose type expression names
//     CredentialDecoration — reported at the declaration itself, once,
//     regardless of what the field is named or how many places later read
//     through it.
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

	hits := scanTypeDecls(fset, f, filename)

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
			// Declaring a carrier field/alias is reported by scanTypeDecls
			// above (with a Type.Field-style key), not here.
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

// recvTypeString renders a method receiver's type expression as it would be
// written in Go source (e.g. "CredentialDecoration" or "*ScopedUserIdentity").
func recvTypeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + recvTypeString(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return "?"
	}
}

// enclosingDeclName returns the name of the function or method declaration
// containing pos, covering the WHOLE declaration (signature and body), not
// just the body. enclosingFuncName (ast_test_helpers_test.go) only matches
// inside a function's body block, so a decoration reference in a signature —
// for example the `decoration *CredentialDecoration` parameter or the
// `*CredentialDecoration` return type on the carriage constructor/accessor
// themselves — would otherwise resolve to "<unknown>" and be misreported as
// a violation instead of being attributed to the (allowed) declaring
// function.
//
// Review-3 finding 1: a method's name is now qualified by its receiver type,
// e.g. "(CredentialDecoration).clone" or "(*ScopedUserIdentity).Decoration",
// so a same-named method on an unrelated receiver is a distinct, unallowed
// key rather than colliding with the allowed one. Free functions are
// unqualified, as before.
func enclosingDeclName(f *ast.File, pos token.Pos) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if pos >= fn.Pos() && pos <= fn.End() {
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				return "(" + recvTypeString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
			}
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
//
// Review-3 finding 1: files are keyed by their path relative to dir, not
// their base name, so a same-named file in a subdirectory cannot collide
// with (or be mistaken for) one at the top level of pkg/hub.
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
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fileHits, scanErr := scanDecorationReferences(rel, string(src))
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
// locations may reference it.
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
// self-test: it feeds the scanner one fixture per reference form the reviews
// found undetected, so a future refactor of the scanner cannot silently
// narrow it back to a no-op.
func TestScanDecorationReferences_CatchesEveryReferenceForm(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		// F2 probe 1 (e1-review-1.md): a direct field read on the credential
		// context, in a file that (before the fix) would have matched the
		// authz* pattern.
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
		// accessor at all.
		"lowercase_field.go": `package hub

func lowerFieldReference(id Identity) bool {
	if s, ok := id.(*ScopedUserIdentity); ok && s.decoration != nil {
		return s.decoration.Labels["role_hint"] == "admin"
	}
	return false
}
`,
		// Review-3 finding 1, probe A: a renamed carrier field on an
		// unrelated struct. The old guard exempted every type declaration,
		// so this produced zero hits no matter what the field was named.
		"renamed_field_carrier.go": `package hub

type view struct {
	Meta *CredentialDecoration
}

func useView(v view) bool {
	return v.Meta.Labels["role_hint"] == "admin"
}
`,
		// Review-3 finding 1, probe B: a type alias over CredentialDecoration.
		"alias_carrier.go": `package hub

type credMeta = CredentialDecoration

func useAlias(m credMeta) bool {
	return m.Labels["role_hint"] == "admin"
}
`,
		// Review-3 finding 1, probe C: an embedded *CredentialDecoration.
		// Expect a hit at the type declaration itself (the embed), keyed by
		// the field name Go assigns to it ("CredentialDecoration").
		"embed_carrier.go": `package hub

type carrier struct {
	*CredentialDecoration
}
`,
		// Review-3 finding 1, probe D: a same-named method on an unrelated
		// receiver. enclosingDeclName qualifies a method's key by its
		// receiver ("(other).clone" vs "(CredentialDecoration).clone"), so
		// this distinct key must NOT be allowed even if this file were
		// named credential_decoration.go.
		"other_receiver_method.go": `package hub

type other struct{}

func (other) clone(cc CredentialContext) bool {
	return cc.Decoration.Labels["a"] == "b"
}
`,
	}
	want := map[string]string{
		"authz_direct_field.go:directFieldReference":    "Decoration",
		"authz_accessor.go:accessorReference":           "CredentialDecorationFromContext",
		"capabilities_mut3.go:otherFileReference":       "Decoration",
		"lowercase_field.go:lowerFieldReference":        "decoration",
		"renamed_field_carrier.go:view.Meta":            "type-field",
		"alias_carrier.go:type credMeta":                "type-alias",
		"embed_carrier.go:carrier.CredentialDecoration": "type-field",
		"other_receiver_method.go:(other).clone":        "Decoration",
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

	// None of the new probes' keys may appear in the production allow-list —
	// that would defeat the point of the fixture (proving an unallowed
	// carrier is caught, not proving it is pre-authorized).
	for _, key := range []string{
		"renamed_field_carrier.go:view.Meta",
		"alias_carrier.go:type credMeta",
		"embed_carrier.go:carrier.CredentialDecoration",
		"other_receiver_method.go:(other).clone",
	} {
		if decorationGuardAllowed[key] {
			t.Errorf("fixture key %s must not be present in decorationGuardAllowed", key)
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
