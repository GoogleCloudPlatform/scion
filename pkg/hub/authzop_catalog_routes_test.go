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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// catalogEntryPoints returns authzop.Catalog's declared EntryPoints for
// operationID, so route pins are driven from the catalog itself rather than
// a second, independently hand-typed list of paths that a future catalog
// edit would not be caught by.
func catalogEntryPoints(t *testing.T, operationID string) []authzop.EntryPoint {
	t.Helper()
	for _, spec := range authzop.Catalog {
		if string(spec.ID) == operationID {
			return spec.EntryPoints
		}
	}
	t.Fatalf("authzop.Catalog has no entry for operation %q", operationID)
	return nil
}

// substitutePattern fills an authzop EntryPoint pattern's {id}/{port}/{subpath}
// placeholders. Empty replacement values leave the corresponding placeholder
// untouched (the caller doesn't use it for that pattern).
func substitutePattern(pattern, id, port, subpath string) string {
	s := pattern
	if id != "" {
		s = strings.ReplaceAll(s, "{id}", id)
	}
	if port != "" {
		s = strings.ReplaceAll(s, "{port}", port)
	}
	if subpath != "" {
		s = strings.ReplaceAll(s, "{subpath}", subpath)
	}
	return s
}

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	return rs4ExtractError(resp)
}

// TestCatalogRoute_AgentAttachIsPTYNotAttach is a route-backed pin, driven
// from authzop.Catalog's own agent.attach entry point (not a second,
// independently hand-typed path): GET on the live /pty path reaches
// handleAgentPTY specifically, proven by its EXACT signature for an
// authenticated, authorized, non-upgrade request against an agent with no
// runtime broker configured (422 ErrCodeNoRuntimeBroker) — a status/code
// pair which a GET on this path only produces when it reaches the PTY
// handler. The /attach pattern is not a recognized agent sub-action; it
// falls through to the generic, POST-only action dispatcher (405 for
// GET).
func TestCatalogRoute_AgentAttachIsPTYNotAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("car-pty-proj"), Name: "p", Slug: "car-pty-proj"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{ID: tid("car-pty-agent"), Slug: "car-pty-agent", Name: "a", ProjectID: project.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agent))

	eps := catalogEntryPoints(t, "agent.attach")
	require.Len(t, eps, 1, "test assumption broken: agent.attach is expected to declare exactly one entry point")
	require.Equal(t, authzop.EntryPointWebSocket, eps[0].Kind)
	path := substitutePattern(eps[0].Pattern, agent.ID, "", "")

	rec := doRequest(t, srv, eps[0].Method, path, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("GET %s: got %d, want %d (UnprocessableEntity -- only handleAgentPTY's no-runtime-broker check produces this)", path, rec.Code, http.StatusUnprocessableEntity)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != ErrCodeNoRuntimeBroker {
		t.Errorf("GET %s: error code = %q, want %q", path, code, ErrCodeNoRuntimeBroker)
	}

	// The /attach pattern is not a recognized agent sub-route
	// (deliberately NOT read from the catalog -- this proves the absence of
	// a catalog entry for it), so route resolution rejects it with 404 and
	// it never reaches the PTY handler.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/attach", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /attach: got %d, want %d (NotFound) -- /attach must not be treated as PTY", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), "Agent route not found") {
		t.Errorf("GET /attach: body = %s, want the unrecognized agent route error", rec.Body.String())
	}
}

// portProxyTestAgent creates a project and an agent within it, and returns
// both.
func portProxyTestAgent(t *testing.T, s store.Store, seed string) (project *store.Project, agent *store.Agent) {
	t.Helper()
	ctx := context.Background()
	project = &store.Project{ID: tid("cpp-proj-" + seed), Name: "p", Slug: "cpp-proj-" + seed}
	require.NoError(t, s.CreateProject(ctx, project))
	agent = &store.Agent{ID: tid("cpp-agent-" + seed), Slug: "cpp-agent-" + seed, Name: "a", ProjectID: project.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agent))
	return project, agent
}

// portProxyEntryPoints returns authzop.Catalog's agent.portaccess entry
// points that reach proxyAgentPort (i.e. contain "/proxy"), asserting the
// exact expected method/pattern set so a catalog edit that dropped the
// proxy entries (leaving only the bare /ports list entry) would fail this
// assertion instead of silently iterating zero times.
func portProxyEntryPoints(t *testing.T) []authzop.EntryPoint {
	t.Helper()
	var proxy []authzop.EntryPoint
	for _, ep := range catalogEntryPoints(t, "agent.portaccess") {
		if strings.Contains(ep.Pattern, "/proxy") {
			proxy = append(proxy, ep)
		}
	}
	wantMethods := map[string]bool{"GET": false, "POST": false, "PUT": false, "DELETE": false}
	sawSubpath := false
	for _, ep := range proxy {
		if strings.Contains(ep.Pattern, "{subpath}") {
			sawSubpath = true
			continue
		}
		if _, ok := wantMethods[ep.Method]; ok {
			wantMethods[ep.Method] = true
		}
	}
	for method, seen := range wantMethods {
		if !seen {
			t.Fatalf("authzop.Catalog's agent.portaccess is missing a %s entry point on the bare .../proxy pattern", method)
		}
	}
	if !sawSubpath {
		t.Fatal("authzop.Catalog's agent.portaccess is missing a .../proxy/{subpath} entry point")
	}
	return proxy
}

// TestCatalogRoute_PortProxy_AgentSelfAccess drives every EntryPoint
// authzop.Catalog declares for agent.portaccess's "{port}/proxy..." shape
// (GET/POST/PUT/DELETE on the bare pattern, plus one subpath) through an
// agent identity accessing its OWN port registration -- the self-access
// path authorizePortAccess admits without ever calling CheckAccess/Decide
// (port_forward_handlers.go). All reach proxyAgentPort and fail the same
// way (503, no tunnel) once past authorization, proving reachability. A
// second, DIFFERENT agent is rejected before Decide too (403), since
// self-access compares agent identity to the target agent directly.
func TestCatalogRoute_PortProxy_AgentSelfAccess(t *testing.T) {
	srv, s := testServer(t)
	_, agent := portProxyTestAgent(t, s, "self")
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, err := tokenSvc.GenerateAgentToken(agent.ID, agent.ProjectID, []AgentTokenScope{ScopeAgentPortForward}, nil)
	require.NoError(t, err)

	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{"port": 4000}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	for _, ep := range portProxyEntryPoints(t) {
		ep := ep
		path := substitutePattern(ep.Pattern, agent.ID, "4000", "some/sub/path")
		t.Run(ep.Method+"_"+ep.Pattern, func(t *testing.T) {
			rec := doAgentTokenRequest(t, srv, ep.Method, path, nil, token)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s (self-access): got %d, want %d (ServiceUnavailable, proving it reached proxyAgentPort with no tunnel present)", ep.Method, path, rec.Code, http.StatusServiceUnavailable)
			}
		})
	}

	// A different agent (not the port's own agent) is rejected before
	// Decide/CheckAccess is ever reached.
	_, otherAgent := portProxyTestAgent(t, s, "self-other")
	otherToken, err := tokenSvc.GenerateAgentToken(otherAgent.ID, otherAgent.ProjectID, []AgentTokenScope{ScopeAgentPortForward}, nil)
	require.NoError(t, err)
	rec = doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/4000/proxy", nil, otherToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a different agent's token must be rejected before Decide: got %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestCatalogRoute_PortProxy_UserSession drives the same catalog entry
// points through a real user session (not an agent token), which is the
// path that actually reaches Decide/CheckAccess (authorizePortAccess,
// port_forward_handlers.go): an authorized user (holding a project-scoped
// agent.port_access grant) gets 503 (no tunnel, same as self-access, once
// past authorization); an unauthorized user (ordinary project member, no
// such grant) gets 403.
func TestCatalogRoute_PortProxy_UserSession(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	_, agent := portProxyTestAgent(t, s, "user")
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	agentToken, err := tokenSvc.GenerateAgentToken(agent.ID, agent.ProjectID, []AgentTokenScope{ScopeAgentPortForward}, nil)
	require.NoError(t, err)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{"port": 4001}, agentToken)
	require.Equal(t, http.StatusCreated, rec.Code)

	authorizedUser := &store.User{ID: tid("cpp-authorized-user"), Email: "cpp-auth@test.com", DisplayName: "Authorized", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, authorizedUser))
	createTestUserWithProjectRole(t, s, authorizedUser.ID, authorizedUser.Email, agent.ProjectID, store.ProjectRoleMember)
	grantPermissionViaRoleBinding(t, s, authorizedUser.ID, "agent.port_access", store.RoleScopeProject, agent.ProjectID)

	unauthorizedUser := &store.User{ID: tid("cpp-unauthorized-user"), Email: "cpp-unauth@test.com", DisplayName: "Unauthorized", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, unauthorizedUser))
	createTestUserWithProjectRole(t, s, unauthorizedUser.ID, unauthorizedUser.Email, agent.ProjectID, store.ProjectRoleMember)

	for _, ep := range portProxyEntryPoints(t) {
		ep := ep
		path := substitutePattern(ep.Pattern, agent.ID, "4001", "some/sub/path")

		t.Run(ep.Method+"_"+ep.Pattern+"/authorized_user", func(t *testing.T) {
			rec := doRequestAsUser(t, srv, authorizedUser, ep.Method, path, nil)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s (authorized user): got %d, want %d (ServiceUnavailable, proving Decide admitted it and it reached proxyAgentPort with no tunnel present)", ep.Method, path, rec.Code, http.StatusServiceUnavailable)
			}
		})

		t.Run(ep.Method+"_"+ep.Pattern+"/unauthorized_user", func(t *testing.T) {
			rec := doRequestAsUser(t, srv, unauthorizedUser, ep.Method, path, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s (unauthorized user): got %d, want %d (Forbidden, proving Decide was actually consulted and denied)", ep.Method, path, rec.Code, http.StatusForbidden)
			}
		})
	}
}

// TestCatalogRoute_DiagnosticsLogsStreamMethodGate is the route-backed half
// of the diagnostics-stream reclassification, driven from authzop.Catalog's
// own SSE entry point for hub.diagnostics.read: through the real server mux
// (not a direct handler call), GET reaches handleDiagnosticsLogsStream
// (forced to its documented 501 short-circuit, exact error code
// "not_implemented", by nilling out logQueryService, since attempting a
// real Cloud Logging tail would block indefinitely in a test) and every
// other method is rejected by the same handler's method gate (405).
// Verifying the literal "Content-Type: text/event-stream" header therefore
// still requires a real *logadmin.Client/*logv2.Client
// (handlers_diagnostics.go:149) and is not exercised end to end here;
// TestHandleDiagnosticsLogsStream_NoLogQueryService/_MethodNotAllowed
// (handlers_diagnostics_test.go) already pin the handler's own behavior
// directly, and this test adds the missing piece: that the live mux route
// for the catalog's declared pattern actually dispatches to that handler.
func TestCatalogRoute_DiagnosticsLogsStreamMethodGate(t *testing.T) {
	srv, _ := testServer(t)
	srv.logQueryService = nil

	var streamEntry *authzop.EntryPoint
	for _, ep := range catalogEntryPoints(t, "hub.diagnostics.read") {
		ep := ep
		if ep.Kind == authzop.EntryPointSSE {
			streamEntry = &ep
			break
		}
	}
	if streamEntry == nil {
		t.Fatal("authzop.Catalog's hub.diagnostics.read has no EntryPointSSE entry")
	}
	path := streamEntry.Pattern

	rec := doRequest(t, srv, streamEntry.Method, path, nil)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("GET %s: got %d, want %d (NotImplemented, logQueryService forced nil)", path, rec.Code, http.StatusNotImplemented)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "not_implemented" {
		t.Errorf("GET %s: error code = %q, want %q", path, code, "not_implemented")
	}

	rec = doRequest(t, srv, http.MethodPost, path, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST %s: got %d, want %d (MethodNotAllowed)", path, rec.Code, http.StatusMethodNotAllowed)
	}
}
