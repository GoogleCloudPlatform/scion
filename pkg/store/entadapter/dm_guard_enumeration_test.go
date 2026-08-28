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

// NO BUILD TAG — this file must run under -tags no_sqlite (CI mode).
//
// This is an AST enumeration test, not a correctness test.
//
// It proves that the D-1 immutability guard call sites are wired up:
//   - AddParticipant calls checkDMParticipantKey
//   - EnsureParticipant calls checkDMParticipantKey
//   - checkDMParticipantKey delegates to messages.CheckDMParticipantKey
//
// This makes mutation m4 (sever the delegation entirely — replace the
// guard body with "return nil") impossible to land while the behavioural
// tests are dark in CI.
//
// What this test is NOT: a correctness test. It would not catch subtle
// bugs in the guard logic (wrong field order, inverted condition, etc.).
// It does not prove the guard is correct — only that the call sites
// exist.
//
// The real behavioural coverage lives in conversation_store_test.go
// behind //go:build !no_sqlite. This file exists because CI runs with
// -tags no_sqlite, so the entadapter package contributes zero
// behavioural tests to the pipeline.

package entadapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

const conversationStoreSource = "conversation_store.go"

// funcBody finds the function declaration with the given name and returns its
// body. Returns nil if the function is not found.
func funcBody(file *ast.File, name string) *ast.BlockStmt {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == name {
			return fn.Body
		}
	}
	return nil
}

// bodyCallsIdent reports whether the function body contains a call expression
// whose callee is a plain identifier matching name (e.g. checkDMParticipantKey).
func bodyCallsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// bodyCallsSelector reports whether the function body contains a call expression
// whose callee is a selector expression X.Sel matching pkg.sel
// (e.g. messages.CheckDMParticipantKey).
func bodyCallsSelector(body *ast.BlockStmt, pkg, sel string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selExpr, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := selExpr.X.(*ast.Ident)
		if !ok {
			return true
		}
		if x.Name == pkg && selExpr.Sel.Name == sel {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestDMGuardCallSites_Enumeration is an AST enumeration test that verifies
// the D-1 immutability guard wiring in conversation_store.go. See the file-
// level comment for scope and limitations.
func TestDMGuardCallSites_Enumeration(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, conversationStoreSource, nil, 0)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", conversationStoreSource, err)
	}

	// 1. AddParticipant must call checkDMParticipantKey.
	t.Run("AddParticipant_calls_checkDMParticipantKey", func(t *testing.T) {
		body := funcBody(file, "AddParticipant")
		if body == nil {
			t.Fatalf("function AddParticipant not found in %s", conversationStoreSource)
		}
		if !bodyCallsIdent(body, "checkDMParticipantKey") {
			t.Fatalf("AddParticipant does not call checkDMParticipantKey — "+
				"the D-1 immutability guard has been severed. "+
				"See conversation_store_test.go for the behavioural tests.")
		}
	})

	// 2. EnsureParticipant must call checkDMParticipantKey.
	t.Run("EnsureParticipant_calls_checkDMParticipantKey", func(t *testing.T) {
		body := funcBody(file, "EnsureParticipant")
		if body == nil {
			t.Fatalf("function EnsureParticipant not found in %s", conversationStoreSource)
		}
		if !bodyCallsIdent(body, "checkDMParticipantKey") {
			t.Fatalf("EnsureParticipant does not call checkDMParticipantKey — "+
				"the D-1 immutability guard has been severed. "+
				"See conversation_store_test.go for the behavioural tests.")
		}
	})

	// 3. checkDMParticipantKey must delegate to messages.CheckDMParticipantKey.
	t.Run("checkDMParticipantKey_delegates_to_messages", func(t *testing.T) {
		body := funcBody(file, "checkDMParticipantKey")
		if body == nil {
			t.Fatalf("function checkDMParticipantKey not found in %s", conversationStoreSource)
		}
		if !bodyCallsSelector(body, "messages", "CheckDMParticipantKey") {
			t.Fatalf("checkDMParticipantKey does not call messages.CheckDMParticipantKey — "+
				"the delegation to the shared predicate in pkg/messages has been severed. "+
				"See conversation_store_test.go for the behavioural tests.")
		}
	})
}
