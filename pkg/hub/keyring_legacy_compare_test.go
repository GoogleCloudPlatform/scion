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
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
			funcs[fd.Name.Name] = fd
		}
	}
	for _, name := range []string{"releaseKeyringCheck", "validateKeyringConfig", "validateKeyringFile", "serverValueCipher"} {
		require.NotNil(t, funcs[name], "%s not found in keyring.go", name)
	}

	isField := func(e ast.Expr) bool {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "SharedSigningSecret"
	}
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

	// Across keyring.go the field appears only in validateKeyringConfig
	// (the emptiness check) and serverValueCipher (the legacy-mode cipher,
	// built only without a keyring file); everywhere else, the release
	// check and validateKeyringFile included, it is absent.
	allowed := map[string]bool{"validateKeyringConfig": true, "serverValueCipher": true}
	for name, fn := range funcs {
		if r, _, _ := scan(fn); r > 0 && !allowed[name] {
			t.Errorf("%s references SharedSigningSecret; only validateKeyringConfig and serverValueCipher may", name)
		}
	}
	refs, _, _ := scan(funcs["releaseKeyringCheck"])
	assert.Zero(t, refs, "releaseKeyringCheck must not reference SharedSigningSecret")
	refs, empty, other := scan(funcs["validateKeyringConfig"])
	assert.Equal(t, 1, refs, "validateKeyringConfig must reference SharedSigningSecret once, for the emptiness check")
	assert.Equal(t, 1, empty, "validateKeyringConfig must check that SharedSigningSecret is empty")
	assert.Zero(t, other, "validateKeyringConfig must never compare SharedSigningSecret with a value or pass it to a function")
}
