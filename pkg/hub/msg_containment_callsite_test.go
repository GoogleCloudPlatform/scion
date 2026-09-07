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

// ExternalEffectCallSiteClassification is the durable call-site guard for
// messaging and agent-dispatch external effects (C3 containment).
//
// Every non-test call to dispatchWithBrokerRetry, DispatchAgentMessage,
// DispatchAgentCreate, or DispatchAgentStart in pkg/hub (including
// subpackages) must be classified as "guarded" or "exempt" with a truthful
// reason. The test fails on:
//   - an unclassified new site (prevents silent bypass introduction),
//   - a stale classification (prevents drift), and
//   - zero matches (scanner self-check).
//
// This is the mechanism that would have caught the B1 scheduled-message
// bypass and will catch the next one.

// effectCallSiteEntry describes one classified external-effect call site.
type effectCallSiteEntry struct {
	file     string // base filename (e.g. "server.go")
	function string // enclosing function name
	symbol   string // called function/method name
	class    string // "guarded" or "exempt"
	reason   string // why this classification is correct
}

// effectCallSiteClassifications is the authoritative classification map.
// Every production call site of the four external-effect primitives must
// appear here. "guarded" means messaging/dispatch authorization occurs
// before the call; "exempt" means the call is intentionally unguarded
// with a documented reason.
var effectCallSiteClassifications = []effectCallSiteEntry{
	// ---- dispatchWithBrokerRetry ----

	// broker_routing.go: the primitive itself — calls DispatchAgentMessage
	// in its retry loop. This is the wrapper, not a call site.
	{file: "broker_routing.go", function: "dispatchWithBrokerRetry", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "primitive wrapper: the retry loop implementation itself"},

	// handlers_agent_messaging.go: handleAgentMessage — guarded by routers
	// at handlers_agents_core.go:2700 and handlers_projects_core.go:2529.
	{file: "handlers_agent_messaging.go", function: "handleAgentMessage", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage called in both routers before this handler"},

	// handlers_agent_messaging.go: handleGroupMessage — guarded at :1286.
	{file: "handlers_agent_messaging.go", function: "handleGroupMessage", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_agent_messaging.go:1286"},

	// handlers_agent_messaging.go: broadcastDirect — guarded per-recipient
	// at handlers_agent_messaging.go:1644.
	{file: "handlers_agent_messaging.go", function: "broadcastDirect", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage per-recipient at handlers_agent_messaging.go:1644"},

	// handlers_agent_messaging.go: publishBroadcastDeliveryFailed — derivative
	// notice back to the original sender agent.
	{file: "handlers_agent_messaging.go", function: "publishBroadcastDeliveryFailed", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "derivative: delivery-failure notice to original sender, not attacker-chosen target"},

	// handlers_agent_messaging.go: processMentions — guarded at :1852.
	{file: "handlers_agent_messaging.go", function: "processMentions", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_agent_messaging.go:1852"},

	// handlers_agent_messaging.go: wake-on-message DispatchAgentStart —
	// guarded by the calling routers. Fragile derivative (see F-RS6-16).
	{file: "handlers_agent_messaging.go", function: "handleAgentMessage", symbol: "DispatchAgentStart",
		class: "guarded", reason: "guarded by calling routers; fragile derivative (F-RS6-16)"},

	// handlers_broker_inbound.go: guarded at :164 (authorizeAgentMessage).
	{file: "handlers_broker_inbound.go", function: "handleBrokerInbound", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_broker_inbound.go:164"},

	// handlers_chat_v2.go: sendAgentRouted primary — guarded at :1125.
	{file: "handlers_chat_v2.go", function: "sendAgentRouted", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "authorizeAgentMessage at handlers_chat_v2.go:1125"},

	// messagebroker.go: deliverToAgent — UNGUARDED internal surface.
	// No authorizeAgentMessage call anywhere in this function.
	// Classified as exempt pending full RS6 remediation.
	{file: "messagebroker.go", function: "deliverToAgent", symbol: "dispatchWithBrokerRetry",
		class: "exempt", reason: "UNGUARDED internal surface (F-RS6-07); no external reach identified; deferred to RS6/AH"},

	// messagebroker.go: publishDeliveryFailed — derivative notice.
	{file: "messagebroker.go", function: "publishDeliveryFailed", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "derivative: delivery-failure notice to original sender"},

	// notifications.go: dispatchToAgent — UNGUARDED notification fan-out.
	// Subscription-only authorization; revocation not re-evaluated.
	{file: "notifications.go", function: "dispatchToAgent", symbol: "dispatchWithBrokerRetry",
		class: "exempt", reason: "UNGUARDED notification fan-out (F-RS6-08); subscription-only authz; deferred to RS6/AH"},

	// server.go: messageEventHandler — GUARDED after C1 containment.
	// authorizeScheduledMessageFire calls authorizeAgentMessage before dispatch.
	{file: "server.go", function: "messageEventHandler", symbol: "dispatchWithBrokerRetry",
		class: "guarded", reason: "C1 containment: authorizeScheduledMessageFire before dispatch"},

	// server.go: dispatchAgentEventHandler — guarded at server.go:3103
	// (authorizeScheduledAgentCreate).
	{file: "server.go", function: "dispatchAgentEventHandler", symbol: "DispatchAgentCreate",
		class: "guarded", reason: "authorizeScheduledAgentCreate at server.go fire-time authorization"},

	// reconcile.go: deliverMessage — dead code. The seam is assigned but
	// never invoked in production.
	{file: "reconcile.go", function: "deliverMessage", symbol: "DispatchAgentMessage",
		class: "exempt", reason: "dead code: seam assigned at server.go but never invoked (F-RS6-18)"},

	// reconcile.go: execDispatchStart — durable-intent replay. Replays
	// a previously authorized dispatch.
	{file: "reconcile.go", function: "execDispatchStart", symbol: "DispatchAgentStart",
		class: "exempt", reason: "durable-intent replay of previously authorized dispatch"},

	// reconcile.go: execDispatchCreate (via DispatchAgentCreateWithGather) —
	// durable-intent replay.
	{file: "reconcile.go", function: "execDispatchCreate", symbol: "DispatchAgentCreateWithGather",
		class: "exempt", reason: "durable-intent replay of previously authorized dispatch"},

	// handlers_agent_create_helpers.go: DispatchAgentStart calls in
	// handleExistingAgent (multiple start calls for existing agent reuse).
	{file: "handlers_agent_create_helpers.go", function: "handleExistingAgent", symbol: "DispatchAgentStart",
		class: "guarded", reason: "called after authorization in the agent-create flow"},

	// handlers_agent_lifecycle.go: DispatchAgentStart in handleAgentLifecycle.
	{file: "handlers_agent_lifecycle.go", function: "handleAgentLifecycle", symbol: "DispatchAgentStart",
		class: "guarded", reason: "authorizeAgentLifecycle at handlers_agent_lifecycle.go"},

	// handlers_agents_core.go: DispatchAgentCreateWithGather in createAgentInProject.
	{file: "handlers_agents_core.go", function: "createAgentInProject", symbol: "DispatchAgentCreateWithGather",
		class: "guarded", reason: "authorizeAgentCreate at handlers_agents_core.go"},

	// workspace_handlers.go: DispatchAgentCreate in handleWorkspaceSyncToFinalize.
	{file: "workspace_handlers.go", function: "handleWorkspaceSyncToFinalize", symbol: "DispatchAgentCreate",
		class: "guarded", reason: "workspace agent creation with project authorization"},
}

// targetSymbols is the set of function/method names that constitute
// external-effect emit primitives.
var targetSymbols = map[string]bool{
	"dispatchWithBrokerRetry":       true,
	"DispatchAgentMessage":          true,
	"DispatchAgentCreate":           true,
	"DispatchAgentStart":            true,
	"DispatchAgentCreateWithGather": true,
}

func TestExternalEffectCallSiteClassification(t *testing.T) {
	// Build the classified set for lookup.
	type classKey struct{ file, function, symbol string }
	classified := make(map[classKey]bool)
	for _, e := range effectCallSiteClassifications {
		k := classKey{e.file, e.function, e.symbol}
		if classified[k] {
			t.Errorf("duplicate classification: %s:%s (%s)", e.file, e.function, e.symbol)
		}
		classified[k] = true
	}

	// Discover all call sites in production code.
	discovered := discoverEffectCallSites(t)

	// Scanner self-check: zero matches means the scanner is broken.
	if len(discovered) == 0 {
		t.Fatal("found zero external-effect call sites — the scanner is broken or the target symbols have been renamed")
	}

	// Check every discovered site is classified.
	discoveredKeys := make(map[classKey]bool)
	for _, site := range discovered {
		k := classKey{site.file, site.function, site.symbol}
		discoveredKeys[k] = true
		if !classified[k] {
			t.Errorf("UNCLASSIFIED external-effect call site: %s in %s:%s (line %d)\n"+
				"  Every call to %s must be in effectCallSiteClassifications as 'guarded' or 'exempt'.",
				site.symbol, site.file, site.function, site.line, site.symbol)
		}
	}

	// Check no stale classifications exist.
	for _, e := range effectCallSiteClassifications {
		k := classKey{e.file, e.function, e.symbol}
		if !discoveredKeys[k] {
			t.Errorf("STALE classification: %s in %s:%s — call site no longer exists in production code.\n"+
				"  Remove or update this entry in effectCallSiteClassifications.",
				e.symbol, e.file, e.function)
		}
	}

	// Report summary.
	guardedCount := 0
	exemptCount := 0
	for _, e := range effectCallSiteClassifications {
		switch e.class {
		case "guarded":
			guardedCount++
		case "exempt":
			exemptCount++
		}
	}
	t.Logf("Verified %d external-effect call sites: %d guarded, %d exempt",
		len(discovered), guardedCount, exemptCount)
}

type effectCallSite struct {
	file     string // base filename
	function string // enclosing function name
	symbol   string // called function/method name
	line     int
}

// discoverEffectCallSites finds all production call sites of the target
// symbols in non-test .go files under pkg/hub, including subpackages.
// This fixes the directory-walk blind spot in create_message_enumeration_test.go
// which skips subdirectories.
func discoverEffectCallSites(t *testing.T) []effectCallSite {
	t.Helper()
	hubDir := findHubDir(t)
	var sites []effectCallSite

	// Walk pkg/hub and all subdirectories to avoid the subpackage blind spot.
	err := filepath.Walk(hubDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// Skip test fixtures and vendored directories.
			base := filepath.Base(path)
			if base == "testdata" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}

		relPath, _ := filepath.Rel(hubDir, path)
		// Use just the base filename for top-level files, or subdir/file for subpackages.
		// For top-level pkg/hub files, relPath is just the filename.
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Logf("warning: failed to parse %s: %v", relPath, parseErr)
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sym := extractCallSymbol(call)
			if !targetSymbols[sym] {
				return true
			}
			pos := fset.Position(call.Pos())
			funcName := enclosingFuncName(fset, f, pos.Offset)
			// Skip function declarations that ARE the interface/primitive itself.
			// We want call sites, not definitions.
			sites = append(sites, effectCallSite{
				file:     relPath,
				function: funcName,
				symbol:   sym,
				line:     pos.Line,
			})
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk hub directory: %v", err)
	}

	return sites
}

// extractCallSymbol returns the function/method name from a call expression.
func extractCallSymbol(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}
