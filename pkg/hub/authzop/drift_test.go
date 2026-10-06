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

package authzop

import "testing"

// TestAgentAttachCatalogMatchesRoute pins the rule: the catalog must
// declare the route that actually exists. The live agent-attach route is a
// WebSocket handshake at "/api/v1/agents/{id}/pty" (pkg/hub/pty_handlers.go
// handleAgentPTY, invoked from handlers_agents_core.go's action == "pty"
// branch) — never "/attach".
func TestAgentAttachCatalogMatchesRoute(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"agent.attach": {
			{Kind: EntryPointWebSocket, Pattern: "/api/v1/agents/{id}/pty", Method: "GET"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 0 {
		t.Errorf("expected no drift for agent.attach against the live /pty route, got %+v", findings)
	}
}

// TestAgentAttachCatalogRejectsAttachPattern pins the other direction of
// the same rule: the catalog pattern must match the registered route, so
// if the discovered route were "/attach" instead of "/pty", the
// live/catalog comparison must report a mismatch rather than silently
// agreeing.
func TestAgentAttachCatalogRejectsAttachPattern(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"agent.attach": {
			{Kind: EntryPointWebSocket, Pattern: "/api/v1/agents/{id}/attach", Method: "GET"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 1 {
		t.Fatalf("expected exactly one drift finding for the /attach pattern, got %+v", findings)
	}
	if findings[0].Kind != DriftPatternMismatch {
		t.Errorf("expected DriftPatternMismatch, got %v", findings[0].Kind)
	}
}

// TestDiagnosticsLogsStreamCatalogMatchesRoute pins the rule:
// handleDiagnosticsLogsStream (pkg/hub/handlers_diagnostics.go) is an SSE
// entry point, not a plain HTTP route, even though it is reached via a
// normal http.HandleFunc registration -- it sets
// "Content-Type: text/event-stream" and streams incrementally.
func TestDiagnosticsLogsStreamCatalogMatchesRoute(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"hub.diagnostics.read": {
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/diagnostics/logs", Method: "GET"},
			{Kind: EntryPointSSE, Pattern: "/api/v1/admin/diagnostics/logs/stream", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/messaging/divergence", Method: "GET"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 0 {
		t.Errorf("expected no drift for hub.diagnostics.read against its live routes, got %+v", findings)
	}
}

// TestAgentPortAccessCatalogMatchesRoute pins agent.portaccess's declared
// entry points against the live port_forward_handlers.go routes: the bare
// list route, plus proxyAgentPort reachable via every listed method on
// ".../proxy" and one representative ".../proxy/{subpath}".
func TestAgentPortAccessCatalogMatchesRoute(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"agent.portaccess": {
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "GET"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "POST"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "PUT"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy", Method: "DELETE"},
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}", Method: "GET"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 0 {
		t.Errorf("expected no drift for agent.portaccess against its live routes, got %+v", findings)
	}
}

// TestCheckDrift_MissingFromCatalog proves a live entry point with no
// OperationSpec at all is reported.
func TestCheckDrift_MissingFromCatalog(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"totally.unknown.operation": {
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/nonexistent", Method: "GET"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 1 || findings[0].Kind != DriftMissingFromCatalog {
		t.Fatalf("expected exactly one DriftMissingFromCatalog finding, got %+v", findings)
	}
}

// TestCheckDrift_MissingRoute proves a catalog entry with no matching
// discovered entry point of any kind is reported as a missing route, not
// silently ignored.
func TestCheckDrift_MissingRoute(t *testing.T) {
	// agent.attach declares one EntryPointWebSocket; supply an empty
	// discovered list for it so nothing matches.
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"agent.attach": {},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 1 || findings[0].Kind != DriftMissingRoute {
		t.Fatalf("expected exactly one DriftMissingRoute finding, got %+v", findings)
	}
}

// TestCheckDrift_NoOpOnEmptyInput proves an empty discovered map produces no
// findings (CheckDrift only reports on operations the caller has actually
// inventoried).
func TestCheckDrift_NoOpOnEmptyInput(t *testing.T) {
	if findings := CheckDrift(nil); len(findings) != 0 {
		t.Errorf("expected no findings for nil input, got %+v", findings)
	}
}

// TestCheckDrift_MethodMismatch proves a method-only difference is reported
// as DriftPatternMismatch, the same as a pattern-only difference: same
// Kind, same Pattern, different Method must produce exactly one
// DriftPatternMismatch finding with both the catalog and the discovered
// (actual) entry point set. This is the regression the catalog's method
// drift (#2227) needed and did not have: DiscoveredEntryPoint.matches
// already compares Method (drift.go), but nothing pinned that comparison.
func TestCheckDrift_MethodMismatch(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		// agent.update declares PATCH (handlers_agents_core.go). Supply a
		// discovered entry point with the same kind and pattern but PUT,
		// as if the live route had moved off PATCH — the same shape as the
		// pre-#2227 catalog bug (agent.update wrongly declared PUT while
		// the live route was PATCH).
		"agent.update": {
			{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}", Method: "PUT"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 1 {
		t.Fatalf("expected exactly one drift finding for the method-only difference, got %+v", findings)
	}
	f := findings[0]
	if f.Kind != DriftPatternMismatch {
		t.Errorf("expected DriftPatternMismatch, got %v", f.Kind)
	}
	if f.Catalog == nil || f.Actual == nil {
		t.Fatalf("expected both Catalog and Actual entry points set, got Catalog=%v Actual=%v", f.Catalog, f.Actual)
	}
	if f.Catalog.Method == f.Actual.Method {
		t.Errorf("expected catalog and actual methods to differ, both were %q", f.Catalog.Method)
	}
	if f.Catalog.Pattern != f.Actual.Pattern {
		t.Errorf("expected catalog and actual patterns to match (method-only mismatch), got %q vs %q", f.Catalog.Pattern, f.Actual.Pattern)
	}
}
