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
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	sciontoollog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	scionportforward "github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentPortRegistrationLifecycle(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port":  3000,
		"label": "dev",
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	var created exposedPortResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, 3000, created.Port)
	assert.Equal(t, "dev", created.Label)
	assert.Equal(t, "127.0.0.1", created.Host)
	assert.Equal(t, "/api/v1/agents/"+agent.ID+"/ports/3000/proxy/", created.BasePath)
	assert.Equal(t, "agent", created.ExposedBy, "a port the agent registers is recorded as exposed by the agent")

	rec = doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports", nil, token)
	require.Equal(t, http.StatusOK, rec.Code)
	var listed listPortsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed.Ports, 1)
	assert.Equal(t, 3000, listed.Ports[0].Port)

	rec = doAgentTokenRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"/ports/3000", nil, token)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts)
}

// TestAuthorizePortRegistration_FederatedUserNeedsHubLevelPortAccess pins
// that a federated user without agent.port_access at hub level is refused
// port management: with no grant on the agent, and also as the agent's
// owner with the project-owner role, which carry agent.port_access on the
// agent record only.
func TestAuthorizePortRegistration_FederatedUserNeedsHubLevelPortAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	caller := newBindableFederatedAdmin(tid("federated-port-admin"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: caller.ID(), Email: caller.Email(), DisplayName: caller.DisplayName(), Role: store.UserRoleMember, Status: "active",
	}))
	project := &store.Project{
		ID: tid("federated-port-project"), Name: "Federated Port Project", Slug: "federated-port-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("federated-port-agent"), Slug: "federated-port-agent", Name: "Federated Port Agent",
		ProjectID: project.ID, OwnerID: caller.ID(), Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	register := func() (*httptest.ResponseRecorder, bool) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", nil)
		r = r.WithContext(contextWithIdentity(r.Context(), caller))
		w := httptest.NewRecorder()
		_, ok := srv.authorizePortRegistration(w, r, agent.ID)
		return w, ok
	}

	w, ok := register()
	assert.False(t, ok, "a federated user with no hub-level grant is refused")
	assert.Equal(t, http.StatusForbidden, w.Code)

	createTestUserWithProjectRole(t, s, caller.ID(), caller.Email(), project.ID, store.ProjectRoleOwner)
	w, ok = register()
	assert.False(t, ok, "agent.port_access on the agent record alone does not admit port management")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "requires agent.port_access at hub level")
}

func TestAuthorizePortRegistrationAllowsUnscopedLocalAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("local-admin-port-project"), Name: "Local Admin Port Project", Slug: "local-admin-port-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("local-admin-port-agent"), Slug: "local-admin-port-agent", Name: "Local Admin Port Agent",
		ProjectID: project.ID, OwnerID: tid("different-owner"), Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	// CO1: Admin users need a super-admin role binding to pass authz checks.
	createTestUserWithRole(t, s, tid("local-port-admin"), "admin@example.com", "admin", store.SystemRoleSuperAdmin)
	localAdmin := NewAuthenticatedUser(tid("local-port-admin"), "admin@example.com", "Local Port Admin", store.UserRoleAdmin, "api")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", nil)
	r = r.WithContext(contextWithIdentity(r.Context(), localAdmin))
	w := httptest.NewRecorder()

	got, ok := srv.authorizePortRegistration(w, r, agent.ID)

	require.True(t, ok)
	assert.Equal(t, agent.ID, got.ID)
}

func TestAgentPortRegistrationRejectsNonLoopbackHost(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
		"host": "10.0.0.1",
	}, token)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAgentPortRegistrationRejectsReservedPort(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 18380,
	}, token)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAgentPortProxyNoTunnelReturns503(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/3000/proxy/", nil, token)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestProxyErrorContentNegotiation(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	// Expose port 3000 so we can test the "no tunnel" path; port 9999 is NOT
	// exposed, so it exercises the "port not found" path.
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	tests := []struct {
		name          string
		path          string
		accept        string
		wantStatus    int
		wantHTML      bool
		wantSubstring string
	}{
		{
			name:          "port_not_found_browser_gets_html",
			path:          "/api/v1/agents/" + agent.ID + "/ports/9999/proxy/",
			accept:        "text/html,application/xhtml+xml",
			wantStatus:    http.StatusNotFound,
			wantHTML:      true,
			wantSubstring: "Port Not Available",
		},
		{
			name:          "port_not_found_api_gets_json",
			path:          "/api/v1/agents/" + agent.ID + "/ports/9999/proxy/",
			accept:        "application/json",
			wantStatus:    http.StatusNotFound,
			wantHTML:      false,
			wantSubstring: `"not_found"`,
		},
		{
			name:          "port_not_found_no_accept_gets_json",
			path:          "/api/v1/agents/" + agent.ID + "/ports/9999/proxy/",
			accept:        "",
			wantStatus:    http.StatusNotFound,
			wantHTML:      false,
			wantSubstring: `"not_found"`,
		},
		{
			name:          "no_tunnel_browser_gets_html",
			path:          "/api/v1/agents/" + agent.ID + "/ports/3000/proxy/",
			accept:        "text/html,application/xhtml+xml",
			wantStatus:    http.StatusServiceUnavailable,
			wantHTML:      true,
			wantSubstring: "Service Unavailable",
		},
		{
			name:          "no_tunnel_api_gets_json",
			path:          "/api/v1/agents/" + agent.ID + "/ports/3000/proxy/",
			accept:        "application/json",
			wantStatus:    http.StatusServiceUnavailable,
			wantHTML:      false,
			wantSubstring: `"runtime_error"`,
		},
		{
			name:          "no_tunnel_no_accept_gets_json",
			path:          "/api/v1/agents/" + agent.ID + "/ports/3000/proxy/",
			accept:        "",
			wantStatus:    http.StatusServiceUnavailable,
			wantHTML:      false,
			wantSubstring: `"runtime_error"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Scion-Agent-Token", token)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)
			if tc.wantHTML {
				assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
			} else {
				assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
			}
			assert.Contains(t, w.Body.String(), tc.wantSubstring)
		})
	}
}

func TestAgentPortProxyThroughTunnel(t *testing.T) {
	srv, s := testServer(t)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/hello", r.URL.Path)
		assert.Equal(t, "1", r.URL.Query().Get("x"))
		w.Header().Set("X-App", "ok")
		_, _ = w.Write([]byte("hello from app"))
	}))
	t.Cleanup(app.Close)

	appURL, err := url.Parse(app.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(appURL.Host)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", portText)
	require.NoError(t, err)
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}

	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": port,
		"host": host,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// ptone/scion#2337: sciontool/log lazily calls Init() on first write, and Init()
	// permanently replaces slog.Default(). Trip that guard here and restore the globals.
	prevDefault, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	sciontoollog.Init()
	slog.SetDefault(prevDefault)
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)

	manager := scionportforward.NewManager(scionhub.NewClientWithConfig(hubHTTP.URL, token, agent.ID))
	go manager.Run(ctx)
	require.Eventually(t, func() bool {
		srv.portTunnels.mu.RLock()
		defer srv.portTunnels.mu.RUnlock()
		return srv.portTunnels.sessions[agent.ID] != nil
	}, 5*time.Second, 50*time.Millisecond)

	rec = doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/"+portText+"/proxy/hello?x=1", nil, token)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Header().Get("X-App"))
	assert.Equal(t, "hello from app", rec.Body.String())
}

// TestAgentPortProxyResponseIsSandboxed verifies the defensive invariant that
// content proxied from an agent's exposed port cannot set cookies on the hub
// origin and is always served under the hub's sandbox CSP, regardless of what
// the agent's own response headers say.
func TestAgentPortProxyResponseIsSandboxed(t *testing.T) {
	srv, s := testServer(t)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=agent-set; Path=/")
		w.Header().Set("Content-Security-Policy", "default-src *")
		for name, v := range agentPageHeaders {
			w.Header().Set(name, v)
		}
		w.Header().Set("X-Agent-Kept", "kept")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>agent content</html>"))
	}))
	t.Cleanup(app.Close)

	appURL, err := url.Parse(app.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(appURL.Host)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", portText)
	require.NoError(t, err)
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}

	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": port,
		"host": host,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager := scionportforward.NewManager(scionhub.NewClientWithConfig(hubHTTP.URL, token, agent.ID))
	go manager.Run(ctx)
	require.Eventually(t, func() bool {
		srv.portTunnels.mu.RLock()
		defer srv.portTunnels.mu.RUnlock()
		return srv.portTunnels.sessions[agent.ID] != nil
	}, 5*time.Second, 50*time.Millisecond)

	rec = doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/"+portText+"/proxy/", nil, token)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "<html>agent content</html>", rec.Body.String())
	assert.Equal(t, agentServedPageSandboxCSP, rec.Header().Get("Content-Security-Policy"),
		"the hub's sandbox CSP must be authoritative; the agent's own CSP must not pass through")
	// Without the web frontend in front (this recorder is the hub alone),
	// the response still refuses to be embedded.
	assert.Equal(t, []string{"'none'"}, cspDirective(rec.Header().Get("Content-Security-Policy"), "frame-ancestors"), "hub-only frame-ancestors")
	assert.Equal(t, untrustedContentPermissionsPolicy, rec.Header().Get("Permissions-Policy"),
		"the hub's Permissions-Policy must replace the agent's own (camera=*)")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Empty(t, rec.Header().Values("Set-Cookie"), "the agent must not be able to set cookies on the hub origin")

	// Behind the web server's security headers, which set the web pages'
	// policy first, the sandbox policy is still the only one.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/"+portText+"/proxy/", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	webRec := httptest.NewRecorder()
	(&WebServer{}).securityHeadersMiddleware(srv.Handler()).ServeHTTP(webRec, req)
	require.Equal(t, http.StatusOK, webRec.Code)
	assert.Equal(t, []string{agentServedPageSandboxCSP}, webRec.Header().Values("Content-Security-Policy"), "tunnel response policies")
	assert.Equal(t, []string{"nosniff"}, webRec.Header().Values("X-Content-Type-Options"), "tunnel response nosniff headers")

	// The hub's own page headers are not combined with the app's: each
	// has exactly the hub's single value. Positive control: a header the
	// hub does not set still passes through from the app.
	hub := proxiedPageHeaderValues(t)
	for _, name := range hubPageHeaderNames {
		assert.Equal(t, []string{hub[name]}, webRec.Header().Values(name), "tunnel page header "+name)
	}
	assert.Equal(t, "kept", webRec.Header().Get("X-Agent-Kept"))
}

// TestAgentPortProxyResponseDoesNotRelayHubOriginStateHeaders verifies the
// defensive invariant that proxied responses must not set or clear hub-origin
// state, and that CORS response headers on the hub origin come only from the
// hub's own CORS middleware (never duplicated by the agent's copies).
func TestAgentPortProxyResponseDoesNotRelayHubOriginStateHeaders(t *testing.T) {
	srv, s := testServer(t)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)

	agentHeaders := map[string]string{
		"Set-Cookie":                           "session=agent-set; Path=/",
		"Set-Cookie2":                          "legacy=agent-set; Version=1",
		"Clear-Site-Data":                      `"cookies", "storage", "cache"`,
		"Access-Control-Allow-Origin":          "https://agent.example",
		"Access-Control-Allow-Credentials":     "true",
		"Access-Control-Allow-Methods":         "GET, PUT",
		"Access-Control-Allow-Headers":         "X-Agent",
		"Access-Control-Allow-Private-Network": "true",
		"Access-Control-Expose-Headers":        "X-Agent",
		"Access-Control-Max-Age":               "1",
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range agentHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("X-Agent-Passthrough", "kept")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)

	appURL, err := url.Parse(app.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(appURL.Host)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", portText)
	require.NoError(t, err)
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}

	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": port,
		"host": host,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager := scionportforward.NewManager(scionhub.NewClientWithConfig(hubHTTP.URL, token, agent.ID))
	go manager.Run(ctx)
	require.Eventually(t, func() bool {
		srv.portTunnels.mu.RLock()
		defer srv.portTunnels.mu.RUnlock()
		return srv.portTunnels.sessions[agent.ID] != nil
	}, 5*time.Second, 50*time.Millisecond)

	const hubOrigin = "https://hub.example"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/"+portText+"/proxy/", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Origin", hubOrigin)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
	assert.Equal(t, "kept", rec.Header().Get("X-Agent-Passthrough"), "ordinary agent headers must still be relayed")

	// Headers the hub never sets on proxied responses: must be absent.
	for _, h := range []string{
		"Set-Cookie",
		"Set-Cookie2",
		"Clear-Site-Data",
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Private-Network",
		"Access-Control-Expose-Headers",
	} {
		t.Run("absent/"+h, func(t *testing.T) {
			assert.Empty(t, rec.Header().Values(h), "proxied responses must not relay %s onto the hub origin", h)
		})
	}

	// Headers owned by the hub's CORS middleware: exactly the hub's single value.
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"Access-Control-Allow-Origin", hubOrigin},
		{"Access-Control-Allow-Methods", strings.Join(srv.config.CORSAllowedMethods, ", ")},
		{"Access-Control-Allow-Headers", strings.Join(srv.config.CORSAllowedHeaders, ", ")},
		{"Access-Control-Max-Age", fmt.Sprintf("%d", srv.config.CORSMaxAge)},
	} {
		t.Run("hub-owned/"+tc.header, func(t *testing.T) {
			assert.Equal(t, []string{tc.want}, rec.Header().Values(tc.header),
				"%s must carry only the hub's value, not a duplicate from the proxied response", tc.header)
		})
	}
}

func TestAgentPortClearedOnTunnelDisconnect(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	// Register a port
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port":  4000,
		"label": "tunnel-test",
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Verify port is registered
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, got.ExposedPorts, 1)

	// Simulate tunnel disconnect by calling clearExposedPortsForAgent directly
	srv.clearExposedPortsForAgent(context.Background(), agent.ID)

	// Verify ports are cleared
	got, err = s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts)
}

func TestAgentPortClearedOnStop(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)

	// Register a port
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port":  5000,
		"label": "stop-test",
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Verify port is registered
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, got.ExposedPorts, 1)

	// Simulate hub setting agent to stopped (as handleAgentLifecycle does)
	srv.clearExposedPortsForAgent(context.Background(), agent.ID)
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{
		Phase:           string(state.PhaseStopped),
		ContainerStatus: "stopped",
	}))

	// Verify ports are cleared
	got, err = s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
}

func TestAgentPortSweepClearsStaleRegistrations(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("pf-sweep-project"),
		Name: "Port Sweep Test",
		Slug: "port-sweep-test",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a stopped agent with ports
	stoppedAgent := &store.Agent{
		ID:        tid("pf-stopped-agent"),
		Slug:      "pf-stopped-agent",
		Name:      "Stopped Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, stoppedAgent.ID, []store.ExposedPort{
		{Port: 3000, Label: "stale", Host: "127.0.0.1", Mode: "rw", ExposedAt: time.Now(), ExposedBy: "agent"},
	}))

	// Create an errored agent with ports (should also be cleared)
	erroredAgent := &store.Agent{
		ID:        tid("pf-errored-agent"),
		Slug:      "pf-errored-agent",
		Name:      "Errored Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseError),
	}
	require.NoError(t, s.CreateAgent(ctx, erroredAgent))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, erroredAgent.ID, []store.ExposedPort{
		{Port: 5000, Label: "error-stale", Host: "127.0.0.1", Mode: "rw", ExposedAt: time.Now(), ExposedBy: "agent"},
	}))

	// Create a running agent with ports (should not be cleared)
	runningAgent := &store.Agent{
		ID:        tid("pf-running-agent"),
		Slug:      "pf-running-agent",
		Name:      "Running Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, runningAgent))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, runningAgent.ID, []store.ExposedPort{
		{Port: 4000, Label: "active", Host: "127.0.0.1", Mode: "rw", ExposedAt: time.Now(), ExposedBy: "agent"},
	}))

	// Run the sweep
	handler := srv.exposedPortsSweepHandler()
	handler(ctx)

	// Stopped agent's ports should be cleared
	got, err := s.GetAgent(ctx, stoppedAgent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts, "stopped agent should have ports cleared")

	// Errored agent's ports should be cleared
	got, err = s.GetAgent(ctx, erroredAgent.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts, "errored agent should have ports cleared")

	// Running agent's ports should remain
	got, err = s.GetAgent(ctx, runningAgent.ID)
	require.NoError(t, err)
	assert.Len(t, got.ExposedPorts, 1, "running agent should keep its ports")
}

func createPortForwardAgent(t *testing.T, srv *Server, s store.Store) (*store.Agent, string) {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   tid("pf-project"),
		Name: "Port Forwarding",
		Slug: "port-forwarding",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("pf-agent"),
		Slug:      "pf-agent",
		Name:      "PF Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)
	token, err := tokenSvc.GenerateAgentToken(agent.ID, project.ID, []AgentTokenScope{ScopeAgentPortForward}, nil)
	require.NoError(t, err)
	return agent, token
}

// TestAgentPortProxyBareURLRedirect (ptone/scion#5566): on the legacy
// tunnel path, GET and HEAD of the bare .../proxy URL are redirected to
// .../proxy/ with the raw query kept; other methods, callers without
// access and ports that are not exposed get the same answer as before.
func TestAgentPortProxyBareURLRedirect(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)
	base := "/api/v1/agents/" + agent.ID + "/ports/3000/proxy"

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := doAgentTokenRequest(t, srv, method, base+"?x=1&y=a%2Fb", nil, token)
			require.Equal(t, http.StatusPermanentRedirect, rec.Code)
			assert.Equal(t, "proxy/?x=1&y=a%2Fb", rec.Header().Get("Location"))
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

			rec = doAgentTokenRequest(t, srv, method, base, nil, token)
			require.Equal(t, http.StatusPermanentRedirect, rec.Code)
			assert.Equal(t, "proxy/", rec.Header().Get("Location"))
		})
	}

	t.Run("relative_location_resolves_to_the_trailing_slash_form", func(t *testing.T) {
		rec := doAgentTokenRequest(t, srv, http.MethodGet, base+"?x=1", nil, token)
		require.Equal(t, http.StatusPermanentRedirect, rec.Code)
		from, err := url.Parse("https://hub.example" + base + "?x=1")
		require.NoError(t, err)
		to, err := url.Parse(rec.Header().Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "https://hub.example"+base+"/?x=1", from.ResolveReference(to).String())
	})

	// Other methods are proxied as before: with no tunnel, the same 503 as
	// the trailing-slash form.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method+"_unchanged", func(t *testing.T) {
			bare := doAgentTokenRequest(t, srv, method, base, nil, token)
			slash := doAgentTokenRequest(t, srv, method, base+"/", nil, token)
			assert.Equal(t, http.StatusServiceUnavailable, bare.Code)
			assert.Equal(t, slash.Code, bare.Code)
			assert.Empty(t, bare.Header().Get("Location"))
		})
	}

	t.Run("port_not_exposed_unchanged", func(t *testing.T) {
		bare := doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/9999/proxy", nil, token)
		slash := doAgentTokenRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/9999/proxy/", nil, token)
		assert.Equal(t, http.StatusNotFound, bare.Code)
		assert.Equal(t, slash.Code, bare.Code)
		assert.Equal(t, slash.Body.String(), bare.Body.String())
		assert.Empty(t, bare.Header().Get("Location"))
	})

	t.Run("caller_without_access_unchanged", func(t *testing.T) {
		ctx := context.Background()
		other := &store.Agent{
			ID:        tid("pf-other-agent"),
			Slug:      "pf-other-agent",
			Name:      "PF Other Agent",
			ProjectID: agent.ProjectID,
			Phase:     string(state.PhaseRunning),
		}
		require.NoError(t, s.CreateAgent(ctx, other))
		otherToken, err := srv.GetAgentTokenService().GenerateAgentToken(other.ID, agent.ProjectID,
			[]AgentTokenScope{ScopeAgentPortForward}, nil)
		require.NoError(t, err)

		for _, method := range []string{http.MethodGet, http.MethodHead} {
			bare := doAgentTokenRequest(t, srv, method, base+"?x=1", nil, otherToken)
			slash := doAgentTokenRequest(t, srv, method, base+"/?x=1", nil, otherToken)
			assert.NotEqual(t, http.StatusPermanentRedirect, bare.Code, method)
			assert.Equal(t, slash.Code, bare.Code, method)
			assert.Equal(t, slash.Body.String(), bare.Body.String(), method)
			assert.Empty(t, bare.Header().Get("Location"), method)
		}
	})
}

// TestAgentPortProxyNoCredentialsPlainText (ptone/scion#5566): on the
// legacy tunnel path, a page sub-request that reaches a proxy route with
// no credential gets the plain-text 401 with the diagnostic headers and
// is counted; a navigation keeps the hub's answer.
func TestAgentPortProxyNoCredentialsPlainText(t *testing.T) {
	srv, s := testServer(t)
	count := portProxyCounterNamed(t, srv, "scion.hub.port_proxy.sandboxed_subresource_unauth")
	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)
	base := "/api/v1/agents/" + agent.ID + "/ports/3000/proxy"

	tests := []struct {
		name     string
		method   string
		path     string
		header   map[string]string
		wantHint bool
	}{
		{"module_script_origin_null", http.MethodGet, base + "/app.js",
			map[string]string{"Origin": "null", "Accept": "*/*", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors"}, true},
		{"stylesheet_cross_site", http.MethodGet, base + "/site.css",
			map[string]string{"Accept": "text/css,*/*;q=0.1", "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}, true},
		{"fetch_post_origin_null", http.MethodPost, base + "/api/ping",
			map[string]string{"Origin": "null"}, true},
		{"image_same_origin", http.MethodGet, base + "/img/dot.png",
			map[string]string{"Accept": "image/png,image/*", "Sec-Fetch-Site": "same-origin"}, false},
		{"plain_client_no_headers", http.MethodGet, base + "/", nil, false},
		{"bare_url_no_headers", http.MethodGet, base, nil, false},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			requirePortProxyNoCredentials(t, w.Code, w.Header(), w.Body.String(), tc.wantHint)
			assert.Equal(t, int64(i+1), count())
		})
	}

	t.Run("head_has_headers_without_body", func(t *testing.T) {
		before := count()
		hubHTTP := httptest.NewServer(srv.Handler())
		t.Cleanup(hubHTTP.Close)
		req, err := http.NewRequest(http.MethodHead, hubHTTP.URL+base+"/app.js", nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "null")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Empty(t, body)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
		assert.Equal(t, "no-credentials", resp.Header.Get("X-Scion-Proxy-Error"))
		assert.Equal(t, "sandboxed-subresource", resp.Header.Get("X-Scion-Proxy-Hint"))
		assert.Equal(t, before+1, count())
	})

	t.Run("navigation_keeps_the_hub_answer", func(t *testing.T) {
		before := count()
		req := httptest.NewRequest(http.MethodGet, base+"/", nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
		assert.Empty(t, w.Header().Get("X-Scion-Proxy-Error"))
		assert.Equal(t, before, count())
	})

	t.Run("non_proxy_route_keeps_the_hub_answer", func(t *testing.T) {
		before := count()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports", nil)
		req.Header.Set("Origin", "null")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
		assert.Empty(t, w.Header().Get("X-Scion-Proxy-Error"))
		assert.Equal(t, before, count())
	})
}

// TestAgentPortProxyOtherCredentialsNotPlainText (ptone/scion#5566): a
// proxy sub-request that carries a credential other than an
// Authorization header is handled by that credential's own path; it
// never gets the plain-text no-credentials 401.
func TestAgentPortProxyOtherCredentialsNotPlainText(t *testing.T) {
	srv, s := testServer(t)
	count := portProxyCounterNamed(t, srv, "scion.hub.port_proxy.sandboxed_subresource_unauth")
	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)
	path := "/api/v1/agents/" + agent.ID + "/ports/3000/proxy/app.js"

	serve := func(t *testing.T, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "null")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	// Each credential's own path decides the answer (served, refused or
	// deferred); what is pinned here is that step 3g never answers.
	notPlainText := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		assert.Empty(t, w.Header().Get("X-Scion-Proxy-Error"), "status %d body %s", w.Code, w.Body.String())
		assert.NotEqual(t, portProxyNoCredentialsBody, w.Body.String())
	}

	t.Run("agent_token", func(t *testing.T) {
		// Authenticated as the agent: proxied, and with no tunnel the 503.
		w := serve(t, map[string]string{"X-Scion-Agent-Token": token})
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		notPlainText(t, w)
	})
	t.Run("invalid_agent_token", func(t *testing.T) {
		w := serve(t, map[string]string{"X-Scion-Agent-Token": "not-a-token"})
		notPlainText(t, w)
	})
	t.Run("broker_headers", func(t *testing.T) {
		w := serve(t, map[string]string{"X-Scion-Broker-ID": "broker-1"})
		notPlainText(t, w)
	})
	t.Run("federation_header", func(t *testing.T) {
		w := serve(t, map[string]string{FederationTokenHeader: "x"})
		notPlainText(t, w)
	})
	t.Run("trusted_proxy_user_headers", func(t *testing.T) {
		prev := srv.authConfig.TrustedProxies
		srv.authConfig.TrustedProxies = []string{"192.0.2.0/24"} // httptest.NewRequest's RemoteAddr
		t.Cleanup(func() { srv.authConfig.TrustedProxies = prev })
		w := serve(t, map[string]string{
			"X-Forwarded-User-Id":    "proxy-user-1",
			"X-Forwarded-User-Email": "proxy-user@example.com",
		})
		notPlainText(t, w)
	})
	t.Run("front_proxy_assertion_refused", func(t *testing.T) {
		prev := srv.authConfig.ProxyAuthenticator
		srv.authConfig.ProxyAuthenticator = portProxyStubAuthenticator{err: errors.New("bad assertion")}
		t.Cleanup(func() { srv.authConfig.ProxyAuthenticator = prev })
		w := serve(t, nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Contains(t, w.Body.String(), "invalid proxy assertion")
		notPlainText(t, w)
	})
	t.Run("front_proxy_assertion_user", func(t *testing.T) {
		prevAuth, prevProv := srv.authConfig.ProxyAuthenticator, srv.authConfig.ProxyUserProvisioner
		srv.authConfig.ProxyAuthenticator = portProxyStubAuthenticator{user: &ProxyUserInfo{
			Subject: "front-proxy-user-1", Email: "front-proxy-user@example.com",
		}}
		srv.authConfig.ProxyUserProvisioner = MakeProxyUserProvisioner(srv)
		t.Cleanup(func() {
			srv.authConfig.ProxyAuthenticator, srv.authConfig.ProxyUserProvisioner = prevAuth, prevProv
		})
		w := serve(t, nil)
		notPlainText(t, w)
	})
	assert.Zero(t, count())
}

// portProxyStubAuthenticator is a front-proxy authenticator that answers every
// request with user, or with err when it is set.
type portProxyStubAuthenticator struct {
	user *ProxyUserInfo
	err  error
}

func (a portProxyStubAuthenticator) Authenticate(*http.Request) (*ProxyUserInfo, error) {
	return a.user, a.err
}

func (portProxyStubAuthenticator) Name() string               { return "stub" }
func (portProxyStubAuthenticator) AssertionHeaders() []string { return nil }

// TestRedirectToProxyRootLocationIsOneLine (ptone/scion#5566): whatever
// bytes the raw query holds, the bare-URL redirect writes exactly one
// Location header whose value is a single line of printable ASCII, and no
// other header. Raw CR, LF and NUL never come from the HTTP parser; the
// handler is checked with them directly.
func TestRedirectToProxyRootLocationIsOneLine(t *testing.T) {
	tests := []struct {
		rawQuery string
		want     string
	}{
		{"x=1", "proxy/?x=1"},
		{"x=%0d%0aSet-Cookie:%20a=b", "proxy/?x=%0d%0aSet-Cookie:%20a=b"},
		{"x=\r\nSet-Cookie: a=b", "proxy/?x=%0D%0ASet-Cookie:%20a=b"},
		{"x=\nLocation: /evil", "proxy/?x=%0ALocation:%20/evil"},
		{"a=\x00\x01\x1f\x7f&b=\xff\xc3\xa9", "proxy/?a=%00%01%1F%7F&b=%FF%C3%A9"},
		{"t=\tq", "proxy/?t=%09q"},
		{"", "proxy/"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.rawQuery), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/a/ports/3000/proxy", nil)
			req.URL.RawQuery = tc.rawQuery
			w := httptest.NewRecorder()
			redirectToProxyRoot(w, req)
			require.Equal(t, http.StatusPermanentRedirect, w.Code)
			require.Equal(t, []string{tc.want}, w.Header().Values("Location"))
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			assert.Len(t, w.Header(), 2, "only Location and Cache-Control are written: %v", w.Header())
			for i := 0; i < len(tc.want); i++ {
				c := tc.want[i]
				assert.True(t, c > ' ' && c < 0x7f, "byte %#x at %d", c, i)
			}
		})
	}
}

// TestAgentPortProxyBareURLRedirectWire (ptone/scion#5566): over a real
// connection, bare-URL requests whose query holds encoded CR/LF and odd
// bytes are either refused by the HTTP server or redirected with one
// well-formed Location line; the response never carries an extra header
// line.
func TestAgentPortProxyBareURLRedirectWire(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)
	hubAddr := strings.TrimPrefix(hubHTTP.URL, "http://")
	bare := "/api/v1/agents/" + agent.ID + "/ports/3000/proxy"

	tests := []struct {
		query string
		want  string // the Location when the server answers 308
	}{
		{"x=%0d%0aSet-Cookie:%20a=b", "proxy/?x=%0d%0aSet-Cookie:%20a=b"},
		{"x=%0D%0ALocation:%20/evil&y=%00%ff%7f", "proxy/?x=%0D%0ALocation:%20/evil&y=%00%ff%7f"},
		{"x=\xff\xfe", "proxy/?x=%FF%FE"},
		{"x=\x01", ""},
		{"x=\x7f", ""},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.query), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", hubAddr, 5*time.Second)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
			_, err = io.WriteString(conn, "GET "+bare+"?"+tc.query+" HTTP/1.1\r\nHost: hub.test\r\n"+
				"X-Scion-Agent-Token: "+token+"\r\nConnection: close\r\n\r\n")
			require.NoError(t, err)
			raw, err := io.ReadAll(conn)
			require.NoError(t, err)

			head, _, found := strings.Cut(string(raw), "\r\n\r\n")
			require.True(t, found, "response %q", raw)
			lines := strings.Split(head, "\r\n")
			require.True(t, strings.HasPrefix(lines[0], "HTTP/1.1 "), "status line %q", lines[0])
			var locations []string
			for _, line := range lines[1:] {
				name, value, ok := strings.Cut(line, ": ")
				require.True(t, ok, "malformed header line %q", line)
				assert.NotContains(t, line, "\n")
				assert.NotContains(t, line, "\r")
				switch http.CanonicalHeaderKey(name) {
				case "Location":
					locations = append(locations, value)
				case "Set-Cookie":
					t.Errorf("unexpected Set-Cookie line %q", line)
				}
			}

			switch {
			case strings.HasPrefix(lines[0], "HTTP/1.1 308 "):
				require.Equal(t, []string{tc.want}, locations)
			case strings.HasPrefix(lines[0], "HTTP/1.1 400 "):
				assert.Empty(t, locations)
			default:
				t.Fatalf("status line %q: want 308 or 400", lines[0])
			}
		})
	}
}

// TestAgentPortProxyBareURLWebSocketUnchanged (ptone/scion#5566): on the
// legacy tunnel path, an admitted WebSocket upgrade to the bare .../proxy
// URL is not redirected; it keeps the answer it had before, the same as
// the trailing-slash form (WebSocket is not supported there).
func TestAgentPortProxyBareURLWebSocketUnchanged(t *testing.T) {
	srv, s := testServer(t)
	agent, token := createPortForwardAgent(t, srv, s)
	rec := doAgentTokenRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/ports", map[string]any{
		"port": 3000,
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code)

	send := func(path, tok string, upgrade bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Scion-Agent-Token", tok)
		if upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	base := "/api/v1/agents/" + agent.ID + "/ports/3000/proxy"
	bare, slash := send(base, token, true), send(base+"/", token, true)
	assert.Equal(t, http.StatusNotImplemented, bare.Code, bare.Body.String())
	assert.Equal(t, slash.Code, bare.Code)
	assert.Equal(t, slash.Body.String(), bare.Body.String())
	assert.Empty(t, bare.Header().Get("Location"))

	// A caller without access is refused before the upgrade is looked at:
	// the upgrade gets exactly the answer a plain request gets.
	t.Run("caller_without_access", func(t *testing.T) {
		ctx := context.Background()
		other := &store.Agent{
			ID:        tid("pf-ws-other-agent"),
			Slug:      "pf-ws-other-agent",
			Name:      "PF WS Other Agent",
			ProjectID: agent.ProjectID,
			Phase:     string(state.PhaseRunning),
		}
		require.NoError(t, s.CreateAgent(ctx, other))
		otherToken, err := srv.GetAgentTokenService().GenerateAgentToken(other.ID, agent.ProjectID,
			[]AgentTokenScope{ScopeAgentPortForward}, nil)
		require.NoError(t, err)

		up, plain := send(base, otherToken, true), send(base, otherToken, false)
		assert.Equal(t, http.StatusForbidden, up.Code, up.Body.String())
		assert.Equal(t, plain.Code, up.Code)
		assert.Equal(t, plain.Body.Bytes(), up.Body.Bytes())
		// X-Request-ID is per request; every other header must match.
		upHeader, plainHeader := up.Header().Clone(), plain.Header().Clone()
		upHeader.Del("X-Request-ID")
		plainHeader.Del("X-Request-ID")
		assert.Equal(t, plainHeader, upHeader)
		assert.Empty(t, up.Header().Get("Location"))
	})
}

// The sandbox token sets are pinned exactly, so widening either policy is a
// deliberate change to this test. Stored files and attachments get print and
// dialogs; agent-served apps also get forms, pointer lock, orientation lock
// and presentation. Neither policy may ever grant the hub's origin or launch
// desktop protocol handlers.
func TestSandboxCSPTokens(t *testing.T) {
	// tokens parses only the sandbox directive; the policy has others.
	tokens := func(csp string) []string {
		require.True(t, strings.HasPrefix(csp, "sandbox "), "CSP must start with the sandbox directive: %q", csp)
		return cspDirective(csp, "sandbox")
	}
	assert.ElementsMatch(t, []string{
		"allow-scripts", "allow-popups", "allow-popups-to-escape-sandbox", "allow-downloads", "allow-modals",
	}, tokens(untrustedContentSandboxCSP))
	assert.ElementsMatch(t, []string{
		"allow-scripts", "allow-popups", "allow-popups-to-escape-sandbox", "allow-downloads", "allow-modals",
		"allow-forms", "allow-pointer-lock", "allow-orientation-lock", "allow-presentation",
	}, tokens(agentServedPageSandboxCSP))
	for _, csp := range []string{untrustedContentSandboxCSP, agentServedPageSandboxCSP} {
		for _, forbidden := range []string{
			"allow-same-origin", "allow-top-navigation", "allow-top-navigation-by-user-activation",
			"allow-top-navigation-to-custom-protocols", "allow-storage-access-by-user-activation",
		} {
			assert.NotContains(t, tokens(csp), forbidden)
		}
	}
	assert.NotContains(t, tokens(untrustedContentSandboxCSP), "allow-forms", "stored files keep the stricter sandbox")
	for _, csp := range []string{untrustedContentSandboxCSP, agentServedPageSandboxCSP} {
		assert.Equal(t, []string{"'none'"}, cspDirective(csp, "frame-ancestors"), "frame-ancestors in %q", csp)
		assert.Len(t, strings.Split(csp, ";"), 2, "only the sandbox and frame-ancestors directives: %q", csp)
	}
}

// The Permissions-Policy denies every powerful feature it names and leaves
// clipboard writes allowed.
func TestUntrustedContentPermissionsPolicy(t *testing.T) {
	// directives splits a policy into its exact directives, so a check never
	// matches part of a longer directive.
	directives := func(policy string) map[string]bool {
		out := map[string]bool{}
		for _, d := range strings.Split(policy, ",") {
			out[strings.TrimSpace(d)] = true
		}
		return out
	}
	content := directives(untrustedContentPermissionsPolicy)
	for d := range content {
		assert.True(t, strings.HasSuffix(d, "=()"), "directive %q must deny the feature", d)
		assert.False(t, strings.HasPrefix(d, "clipboard-write="), "clipboard writes stay allowed: %q", d)
	}
	for _, f := range []string{"camera", "microphone", "geolocation", "display-capture", "clipboard-read"} {
		assert.True(t, content[f+"=()"], "%s must be denied", f)
	}
	for d := range directives(webPermissionsPolicy) {
		assert.True(t, content[d], "untrusted content must deny everything the hub-wide policy denies: %q", d)
	}
	h := http.Header{}
	setUntrustedContentHeaders(h, agentServedPageSandboxCSP)
	assert.Equal(t, agentServedPageSandboxCSP, h.Get("Content-Security-Policy"))
	assert.Equal(t, untrustedContentPermissionsPolicy, h.Get("Permissions-Policy"))
}
