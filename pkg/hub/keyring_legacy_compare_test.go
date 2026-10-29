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

// TestValidateKeyringConfig_LegacyEntryComparedInConstantTime pins how
// validateKeyringConfig compares SharedSigningSecret with the keyring's
// "legacy" signing entry: through exactly one crypto/subtle
// ConstantTimeCompare call, never with == or !=. A plain comparison returns
// the same results, so no behavioural test can tell the two apart, and the
// timing difference is not measurable reliably in a unit test; the
// comparison form is therefore asserted on the source.
//
// A == or != counts only when an operand is the field itself (or a
// conversion of it), so the result check on the constant-time call
// (ConstantTimeCompare(...) != 1) is not a plain comparison.
func TestValidateKeyringConfig_LegacyEntryComparedInConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "keyring.go", nil, parser.ImportsOnly|parser.ParseComments)
	require.NoError(t, err)
	// The package name ConstantTimeCompare is called through must be
	// crypto/subtle, imported under its own name.
	subtleImported := false
	for _, imp := range f.Imports {
		if imp.Path.Value == `"crypto/subtle"` && (imp.Name == nil || imp.Name.Name == "subtle") {
			subtleImported = true
		}
	}
	require.True(t, subtleImported, "keyring.go must import crypto/subtle as subtle")

	f, err = parser.ParseFile(fset, "keyring.go", nil, 0)
	require.NoError(t, err)
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "validateKeyringConfig" {
			fn = fd
		}
	}
	require.NotNil(t, fn, "validateKeyringConfig not found in keyring.go")

	// isField reports whether e is the SharedSigningSecret field itself,
	// possibly parenthesized or inside one conversion ([]byte(x), string(x)).
	isField := func(e ast.Expr) bool {
		e = ast.Unparen(e)
		if call, ok := e.(*ast.CallExpr); ok && len(call.Args) == 1 {
			switch fun := ast.Unparen(call.Fun).(type) {
			case *ast.ArrayType:
				e = ast.Unparen(call.Args[0])
			case *ast.Ident:
				if fun.Name == "string" {
					e = ast.Unparen(call.Args[0])
				}
			}
		}
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "SharedSigningSecret"
	}

	constantTime, plain := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ConstantTimeCompare" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "subtle" && len(x.Args) == 2 && (isField(x.Args[0]) || isField(x.Args[1])) {
				constantTime++
			}
		case *ast.BinaryExpr:
			if (x.Op == token.EQL || x.Op == token.NEQ) && (isField(x.X) || isField(x.Y)) {
				plain++
			}
		}
		return true
	})
	assert.Equal(t, 1, constantTime, "SharedSigningSecret must be compared with one subtle.ConstantTimeCompare call")
	assert.Zero(t, plain, "SharedSigningSecret must never be compared with == or !=")
}
