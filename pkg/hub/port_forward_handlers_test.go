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
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
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

	// Simulate tunnel disconnect by calling onDisconnect directly
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

func doAgentTokenRequest(t *testing.T, srv *Server, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
