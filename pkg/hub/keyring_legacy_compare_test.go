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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateKeyringConfig_SharedSigningSecretOnlyCheckedEmpty pins how
// keyring mode treats SharedSigningSecret now that no purpose reads it with
// a keyring file: validateKeyringConfig only checks that it is empty (one
// comparison, against the empty string literal), it is never compared with
// any secret, and the release check (releaseKeyringCheck, behind the
// keyringReleaseCheck seam) does not reference it at all. A comparison with
// a secret value would bring back a dependence on the "legacy" entry, and
// no behavioural test can tell an emptiness check from an equality check
// that happens to agree, so the form is asserted on the source.
func TestValidateKeyringConfig_SharedSigningSecretOnlyCheckedEmpty(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "keyring.go", nil, 0)
	require.NoError(t, err)
	funcs := keyringGuardFuncs(f)
	for _, name := range []string{"releaseKeyringCheck", "validateKeyringConfig", "validateKeyringFile", "serverValueCipher"} {
		require.NotNil(t, funcs[name], "%s not found in keyring.go", name)
	}

	isField := isSharedSigningSecretField
	isEmptyString := func(e ast.Expr) bool {
		lit, ok := ast.Unparen(e).(*ast.BasicLit)
		return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
	}
	// scan counts, in fn, the references to the field, its comparisons with
	// the empty string, and every other use of it inside a comparison or a
	// call (subtle.ConstantTimeCompare, bytes.Equal, ...).
	scan := func(fn *ast.FuncDecl) (refs, emptyChecks, otherCompares int) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "SharedSigningSecret" {
					refs++
				}
			case *ast.BinaryExpr:
				if (x.Op == token.EQL || x.Op == token.NEQ) && (isField(x.X) || isField(x.Y)) {
					if isEmptyString(x.X) || isEmptyString(x.Y) {
						emptyChecks++
					} else {
						otherCompares++
					}
				}
			case *ast.CallExpr:
				for _, a := range x.Args {
					inner := ast.Unparen(a)
					if c, ok := inner.(*ast.CallExpr); ok && len(c.Args) == 1 {
						inner = c.Args[0] // []byte(field), string(field)
					}
					if isField(inner) {
						otherCompares++
					}
				}
			}
			return true
		})
		return refs, emptyChecks, otherCompares
	}

	// Across keyring.go, functions and methods alike, the field appears
	// only in validateKeyringConfig (the emptiness check) and
	// serverValueCipher (the legacy-mode cipher, built only without a
	// keyring file); everywhere else, the release check and
	// validateKeyringFile included, it is absent.
	for _, name := range sharedSecretReadersOutside(f, keyringGuardAllowed) {
		t.Errorf("%s references SharedSigningSecret; only validateKeyringConfig and serverValueCipher may", name)
	}
	refs, _, _ := scan(funcs["releaseKeyringCheck"])
	assert.Zero(t, refs, "releaseKeyringCheck must not reference SharedSigningSecret")
	refs, empty, other := scan(funcs["validateKeyringConfig"])
	assert.Equal(t, 1, refs, "validateKeyringConfig must reference SharedSigningSecret once, for the emptiness check")
	assert.Equal(t, 1, empty, "validateKeyringConfig must check that SharedSigningSecret is empty")
	assert.Zero(t, other, "validateKeyringConfig must never compare SharedSigningSecret with a value or pass it to a function")
}

// TestValidateTestAdminGateConfig_KeyringRefusalPrecedesSharedSecret pins
// that the test admin tier's construction re-check decides keyring mode
// (managedSigning) before it reads SharedSigningSecret, so keyring mode
// never reads the shared secret on that path: in validateTestAdminGateConfig
// the first managedSigning call comes before the first SharedSigningSecret
// reference, and the function returns in that case.
func TestValidateTestAdminGateConfig_KeyringRefusalPrecedesSharedSecret(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test_admin_gate.go", nil, 0)
	require.NoError(t, err)
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "validateTestAdminGateConfig" {
			fn = fd
		}
	}
	require.NotNil(t, fn, "validateTestAdminGateConfig not found")
	var managed, shared token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CaseClause:
			for _, e := range x.List {
				if c, ok := e.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "managedSigning" && managed == token.NoPos {
						managed = c.Pos()
						require.NotEmpty(t, x.Body, "the managedSigning case must refuse")
						_, isReturn := x.Body[0].(*ast.ReturnStmt)
						assert.True(t, isReturn, "the managedSigning case must return")
					}
				}
			}
		case *ast.SelectorExpr:
			if x.Sel.Name == "SharedSigningSecret" && shared == token.NoPos {
				shared = x.Pos()
			}
		}
		return true
	})
	require.NotEqual(t, token.NoPos, managed, "validateTestAdminGateConfig must refuse keyring mode (managedSigning)")
	require.NotEqual(t, token.NoPos, shared, "validateTestAdminGateConfig must still check SharedSigningSecret in legacy mode")
	assert.Less(t, managed, shared, "keyring mode must be refused before SharedSigningSecret is read")
}

// keyringGuardAllowed are the only keyring.go declarations that may
// reference SharedSigningSecret.
var keyringGuardAllowed = map[string]bool{"validateKeyringConfig": true, "serverValueCipher": true}

// keyringGuardFuncs returns every function and method declared in f, keyed
// by name; methods as "(*T).Name" or "T.Name".
func keyringGuardFuncs(f *ast.File) map[string]*ast.FuncDecl {
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			switch rt := fd.Recv.List[0].Type.(type) {
			case *ast.StarExpr:
				if id, ok := rt.X.(*ast.Ident); ok {
					name = "(*" + id.Name + ")." + name
				}
			case *ast.Ident:
				name = rt.Name + "." + name
			}
		}
		funcs[name] = fd
	}
	return funcs
}

// isSharedSigningSecretField reports whether e is a SharedSigningSecret
// selector (possibly parenthesized).
func isSharedSigningSecretField(e ast.Expr) bool {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "SharedSigningSecret"
}

// sharedSecretReadersOutside returns, sorted, the functions and methods of
// f that reference SharedSigningSecret and are not in allowed.
func sharedSecretReadersOutside(f *ast.File, allowed map[string]bool) []string {
	var out []string
	for name, fn := range keyringGuardFuncs(f) {
		if allowed[name] || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SharedSigningSecret" {
				found = true
			}
			return !found
		})
		if found {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// The keyring.go guard covers methods too: a planted *Server method (and a
// planted function) reading SharedSigningSecret is reported, while the
// allowed declarations are not.
func TestSharedSecretReadersOutside_DetectsPlantedMethod(t *testing.T) {
	const src = `package hub
func validateKeyringConfig(cfg ServerConfig) error { if cfg.SharedSigningSecret != "" { return nil }; return nil }
func (s *Server) plantedMethod() string { return s.config.SharedSigningSecret }
func plantedFunc(cfg ServerConfig) bool { return cfg.SharedSigningSecret == "x" }
func (c capabilityKeys) clean() {}
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"(*Server).plantedMethod", "plantedFunc"}, sharedSecretReadersOutside(f, keyringGuardAllowed))
}
