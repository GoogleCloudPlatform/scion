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
// It catches the two most likely accidental breaks:
//   - m4: total removal — guard body replaced with "return nil"
//   - m5: result discard — call present but assigned to _ instead of checked
//
// Remaining limitation: an "if err := checkDMParticipantKey(...); err != nil { }"
// with an empty body would still slip past, as would many other deliberate
// evasion patterns. The tightening targets the accidental case (refactor
// at 5pm on a Friday), not the adversarial case (attacker editing the
// store layer). The only real fix is getting the behavioural tests
// (conversation_store_test.go, behind //go:build !no_sqlite) into CI.
// That fix is tracked and is not this team's work. This file should be
// deleted the day the behavioural tests run in CI.

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

// bodyDiscardsCallResult reports whether any call to the named function has
// its result assigned to the blank identifier (_).
func bodyDiscardsCallResult(body *ast.BlockStmt, fnName string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// Check if RHS contains a call to fnName.
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != fnName {
				continue
			}
			// RHS calls fnName — check if any LHS is the blank identifier.
			for _, lhs := range assign.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
					found = true
					return false
				}
			}
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
			t.Fatalf("AddParticipant does not call checkDMParticipantKey — " +
				"the D-1 immutability guard has been severed. " +
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
			t.Fatalf("EnsureParticipant does not call checkDMParticipantKey — " +
				"the D-1 immutability guard has been severed. " +
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
			t.Fatalf("checkDMParticipantKey does not call messages.CheckDMParticipantKey — " +
				"the delegation to the shared predicate in pkg/messages has been severed. " +
				"See conversation_store_test.go for the behavioural tests.")
		}
	})

	// 4. Guard result must be consumed, not discarded (catches mutation m5).
	t.Run("guard_result_consumed_not_discarded", func(t *testing.T) {
		for _, fn := range []string{"AddParticipant", "EnsureParticipant"} {
			body := funcBody(file, fn)
			if body == nil {
				t.Fatalf("function %s not found in %s", fn, conversationStoreSource)
			}
			if bodyDiscardsCallResult(body, "checkDMParticipantKey") {
				t.Fatalf("%s calls checkDMParticipantKey but discards its result (assigns to _). "+
					"The guard is present but dead — its error is never checked. "+
					"See conversation_store_test.go for the behavioural tests.", fn)
			}
		}
	})
}
