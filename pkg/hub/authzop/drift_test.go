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

// TestAgentAttachCatalogMatchesRoute pins the concrete drift this task
// closes: the live agent-attach route is a WebSocket handshake at
// "/api/v1/agents/{id}/pty" (pkg/hub/pty_handlers.go handleAgentPTY,
// invoked from handlers_agents_core.go's action == "pty" branch) — never
// "/attach". The catalog must declare the route that actually exists.
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

// TestAgentAttachCatalogDoesNotMatchOldPattern proves CheckDrift would have
// caught the original mismatch: if the discovered route were still the
// old, wrong "/attach" pattern, the live/catalog comparison must report a
// mismatch rather than silently agreeing (i.e. this test exercises the
// negative case that motivated the fix above).
func TestAgentAttachCatalogDoesNotMatchOldPattern(t *testing.T) {
	discovered := map[OperationID][]DiscoveredEntryPoint{
		"agent.attach": {
			{Kind: EntryPointWebSocket, Pattern: "/api/v1/agents/{id}/attach", Method: "GET"},
		},
	}
	findings := CheckDrift(discovered)
	if len(findings) != 1 {
		t.Fatalf("expected exactly one drift finding for a stale /attach pattern, got %+v", findings)
	}
	if findings[0].Kind != DriftPatternMismatch {
		t.Errorf("expected DriftPatternMismatch, got %v", findings[0].Kind)
	}
}

// TestDiagnosticsLogsStreamCatalogMatchesRoute pins the second concrete
// drift this task closes: handleDiagnosticsLogsStream
// (pkg/hub/handlers_diagnostics.go) sets "Content-Type: text/event-stream"
// and streams incrementally -- it is an SSE entry point, not a plain HTTP
// route, even though it is reached via a normal http.HandleFunc
// registration.
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
