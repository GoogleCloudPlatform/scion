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
	"strconv"
	"strings"
	"testing"
)

// TestDispatchStateEnumeration is the durable guard for nc-promote-busy
// (round 2, R2): it uses go/ast to find every `&store.Message{...}`
// composite literal in non-test .go files under pkg/hub and requires each
// one to set the DispatchState field explicitly.
//
// Why this matters: an unset DispatchState silently falls through to the
// Ent schema's "pending" default (pkg/ent/schema/message.go), and unless
// something later explicitly transitions the row, it is picked up by
// ExpireStuckPendingMessages (which flips it to "failed" after 24h) and then
// PurgeFailedMessages (which hard-deletes it after 7 days). Two production
// call sites (messagebroker.go:deliverToUser and
// handlers_agent_messaging.go's deliveryUserDirect fallback, plus
// notifications.go:createInboxMessage and the group-set-to-user site) had
// exactly this omission — the persisted row WAS the delivery, but nothing
// ever stamped it "dispatched" — which made "Promote to thread" fail
// forever and, worse, put every one of those rows on a silent 7-day
// deletion clock. Every current literal sets DispatchState explicitly
// (agent_dm_operation.go's ExecuteAgentDM is the one intentional exception
// in value, not presence: it writes MessageDispatchPending as a durable
// dispatch intent per #1689, transitioned forward once the broker actually
// accepts or rejects it). A new call site that omits the field entirely
// must fail this test, not ship silently.
func TestDispatchStateEnumeration(t *testing.T) {
	hubDir := findHubDir(t)
	fset := token.NewFileSet()

	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("failed to read hub directory: %v", err)
	}

	type litSite struct {
		file     string
		line     int
		funcName string
	}
	var unset []litSite
	total := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fullPath := filepath.Join(hubDir, name)
		f, err := parser.ParseFile(fset, fullPath, nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", name, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			unary, ok := n.(*ast.UnaryExpr)
			if !ok || unary.Op != token.AND {
				return true
			}
			lit, ok := unary.X.(*ast.CompositeLit)
			if !ok || !isStoreMessageType(lit.Type) {
				return true
			}

			total++
			pos := fset.Position(lit.Pos())

			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "DispatchState" {
					return true // found it on this literal — done
				}
			}

			unset = append(unset, litSite{
				file:     name,
				line:     pos.Line,
				funcName: enclosingFuncName(fset, f, pos.Offset),
			})
			return true
		})
	}

	if total == 0 {
		t.Fatal("found zero &store.Message{} composite literals — the scanner is broken")
	}

	if len(unset) > 0 {
		t.Errorf("Found %d &store.Message{} literal(s) that do not set DispatchState:\n", len(unset))
		for _, s := range unset {
			t.Errorf("  - %s:%s (line %s)", s.file, s.funcName, strconv.Itoa(s.line))
		}
		t.Error("\nAn unset DispatchState silently defaults to Ent's \"pending\" and, " +
			"unless something later transitions it, is eventually swept to \"failed\" " +
			"and then hard-deleted (nc-promote-busy). Stamp DispatchState explicitly " +
			"on every &store.Message{} literal.")
	}

	t.Logf("Verified %d &store.Message{} literal(s) all stamp DispatchState", total)
}

// isStoreMessageType returns true if the composite literal's type expression
// is store.Message (a SelectorExpr with package "store" and type "Message").
func isStoreMessageType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Message" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "store"
}
