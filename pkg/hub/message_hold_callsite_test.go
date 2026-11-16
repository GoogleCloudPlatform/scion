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
	"path/filepath"
	"strings"
	"testing"
)

// TestHoldOrDispatchCallSites (ptone/scion#7881, AC A2-3) scans every non-test
// .go file in pkg/hub (and guardScanSubdirs) and verifies that every call to
// dispatchWithBrokerRetry or DispatchAgentMessage is either:
//  1. Structurally enclosed inside a call to holdOrDispatch / holdOrDispatchMessage
//     (so persisted messages queue behind an active awaiting_session hold in
//     creation order instead of racing ahead of held messages),
//  2. The held-message re-drive dispatcher itself (message_hold_redrive.go), or
//  3. In nonRowDispatchExemptions with a documented reason why it never
//     dispatches a persisted store.Message row.
func TestHoldOrDispatchCallSites(t *testing.T) {
	nonRowDispatchExemptions := map[string]string{
		"broker_routing.go:dispatchWithBrokerRetry:DispatchAgentMessage":                  "primitive retry wrapper around DispatchAgentMessage",
		"handlers_agent_messaging.go:publishBroadcastDeliveryFailed:DispatchAgentMessage": "ephemeral delivery-failure notice to sender; no store.Message row",
		"messagebroker.go:publishDeliveryFailed:DispatchAgentMessage":                     "ephemeral delivery-failure notice to sender; no store.Message row",
		"messagebroker.go:publishDeliveryDeferred:DispatchAgentMessage":                   "ephemeral delivery-deferred notice to sender; no store.Message row",
		"artifacts_review.go:deliverArtifactReview:dispatchWithBrokerRetry":               "ephemeral artifact review system notice; no store.Message row",
		"notifications.go:dispatchToAgent:dispatchWithBrokerRetry":                        "status/inbox notification fan-out (has its own notification_sweep retry); no store.Message row",
		"notification_sweep.go:retryDispatch:dispatchWithBrokerRetry":                     "notification fan-out retry; no store.Message row",
		"reconcile.go:deliverMessage:DispatchAgentMessage":                                "dead code seam (ptone/scion#7949); never invoked in production",
	}

	hubDir := findHubDir(t)
	fset := token.NewFileSet()

	// pkgmove:scan-covers guardScanSubdirs
	// scanned through readGuardScanDirs; guards message-hold call sites across pkg/hub and subpackages.
	entries, err := readGuardScanDirs(hubDir)
	if err != nil {
		t.Fatalf("failed to read hub directory: %v", err)
	}

	seenExemptions := make(map[string]bool)
	gatedSites := 0

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

		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			stack = append(stack, n)

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sym := extractCallSymbol(call)
			if sym != "dispatchWithBrokerRetry" && sym != "DispatchAgentMessage" {
				return true
			}

			pos := fset.Position(call.Pos())
			fn := enclosingFuncName(fset, f, pos.Offset)
			key := fmt.Sprintf("%s:%s:%s", name, fn, sym)

			// 1. Structurally gated by holdOrDispatch / holdOrDispatchMessage?
			if hasHoldGateAncestor(stack[:len(stack)-1]) {
				if _, alsoExempt := nonRowDispatchExemptions[key]; alsoExempt {
					t.Errorf("%s (line %d) is inside holdOrDispatch AND listed in nonRowDispatchExemptions — remove the exemption", key, pos.Line)
				}
				gatedSites++
				return true
			}

			// 2. Held-message re-drive loop itself?
			if name == "message_hold_redrive.go" && fn == "dispatchClaimedHeldMessage" {
				return true
			}

			// 3. Explicit non-row exemption?
			if _, exempt := nonRowDispatchExemptions[key]; exempt {
				seenExemptions[key] = true
				return true
			}

			t.Errorf("UNGATED message dispatch call site: %s at %s:%d\n"+
				"  Every persisted-message dispatch site must wrap %s in s.holdOrDispatch "+
				"(or p.holdOrDispatchMessage), or be justified in nonRowDispatchExemptions.",
				key, name, pos.Line, sym)
			return true
		})
	}

	if gatedSites < 13 {
		t.Errorf("expected at least 13 holdOrDispatch-gated call sites across pkg/hub, found %d", gatedSites)
	}
	for k := range nonRowDispatchExemptions {
		if !seenExemptions[k] {
			t.Errorf("STALE nonRowDispatchExemptions entry: %s — call site no longer exists or is now gated", k)
		}
	}
}

func hasHoldGateAncestor(ancestors []ast.Node) bool {
	for i := len(ancestors) - 1; i >= 0; i-- {
		call, ok := ancestors[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		switch extractCallSymbol(call) {
		case "holdOrDispatch", "holdOrDispatchMessage":
			return true
		}
	}
	return false
}
