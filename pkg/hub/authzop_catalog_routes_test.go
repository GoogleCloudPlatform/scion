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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestCatalogRoute_AgentAttachIsPTYNotAttach is a route-backed pin (through
// the real server mux, not two copies of catalog metadata compared to each
// other): GET on the live /pty path reaches handleAgentPTY, while the old
// /attach pattern is no longer a recognized agent sub-action at all and
// falls through to the generic, POST-only action dispatcher.
func TestCatalogRoute_AgentAttachIsPTYNotAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("car-pty-proj"), Name: "p", Slug: "car-pty-proj"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{ID: tid("car-pty-agent"), Slug: "car-pty-agent", Name: "a", ProjectID: project.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// A plain GET is never a valid WebSocket handshake, but it must reach
	// handleAgentPTY specifically -- proven by getting a response distinct
	// from MethodNotAllowed, which is what the generic action dispatcher
	// below would return for a GET on any action it recognizes.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	if rec.Code == http.StatusMethodNotAllowed {
		t.Errorf("GET /pty must not fall through to the generic POST-only action dispatcher: got %d", rec.Code)
	}
	if rec.Code == http.StatusNotFound {
		t.Errorf("GET /pty must be a recognized route, not 404: got %d", rec.Code)
	}

	// The old /attach pattern is not one of handleAgentByID's recognized
	// sub-resources, so it falls through to handleAgentAction, which is
	// POST-only and returns 405 for GET -- the opposite of /pty's behavior.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/attach", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /attach: got %d, want %d (MethodNotAllowed) -- /attach must not be treated as PTY", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestCatalogRoute_PortProxyReachesAuthorizePortAccessForEveryMethodAndSubpath
// is the route-backed pin for the port-proxy catalog entry: the live route
// (port_forward_handlers.go proxyAgentPort, via authorizePortAccess) serves
// every HTTP method and any "/proxy" or "/proxy/<subpath>" suffix, not just
// GET on the bare pattern. All of them reach the same authorization gate and
// fail the same way (503, no tunnel) once past it, proving reachability.
func TestCatalogRoute_PortProxyReachesAuthorizePortAccessForEveryMethodAndSubpath(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 4000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/agents/" + agent.ID + "/ports/4000/proxy"},
		{http.MethodPost, "/api/v1/agents/" + agent.ID + "/ports/4000/proxy"},
		{http.MethodPut, "/api/v1/agents/" + agent.ID + "/ports/4000/proxy/some/sub/path"},
		{http.MethodDelete, "/api/v1/agents/" + agent.ID + "/ports/4000/proxy/x"},
	}
	for _, tc := range cases {
		rec := doAgentTokenRequest(t, srv, tc.method, tc.path, nil, token)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: got %d, want %d (ServiceUnavailable, proving it reached authorizePortAccess/proxyAgentPort with no tunnel present)", tc.method, tc.path, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

// TestCatalogRoute_DiagnosticsLogsStreamMethodGate is the route-backed half
// of the diagnostics-stream reclassification: through the real server mux
// (not a direct handler call), GET reaches handleDiagnosticsLogsStream
// (forced to its documented 501 short-circuit by nilling out
// logQueryService, since attempting a real Cloud Logging tail would block
// indefinitely in a test) and every other method is rejected by the same
// handler's method gate (405).
// Verifying the literal "Content-Type: text/event-stream" header requires a
// configured *logadmin.Client/*logv2.Client (handlers_diagnostics.go:149),
// which needs real Cloud Logging credentials this test environment does not
// have; TestHandleDiagnosticsLogsStream_NoLogQueryService/_MethodNotAllowed
// (handlers_diagnostics_test.go) already pin the handler's own behavior
// directly, and this test adds the missing piece: that the live mux route
// for the catalog's declared pattern actually dispatches to that handler.
func TestCatalogRoute_DiagnosticsLogsStreamMethodGate(t *testing.T) {
	// testServer wires up a real logQueryService in this environment; force
	// it back to nil (testServerNoCloudLogs's exact pattern,
	// handlers_logs_test.go) so GET takes the documented 501 short-circuit
	// instead of attempting a real, indefinitely-blocking Cloud Logging tail.
	srv, _ := testServer(t)
	srv.logQueryService = nil

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/diagnostics/logs/stream", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("GET: got %d, want %d (NotImplemented, no Cloud Logging client configured)", rec.Code, http.StatusNotImplemented)
	}

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/diagnostics/logs/stream", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d, want %d (MethodNotAllowed)", rec.Code, http.StatusMethodNotAllowed)
	}
}
