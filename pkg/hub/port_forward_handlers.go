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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/portforward"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	maxExposedPortsPerAgent = 10
	maxProxyRequestBody     = 32 << 20
	portForwardTimeout      = 60 * time.Second
	maxConcurrentStreams    = 64
)

// deniedExposedPorts lists infrastructure ports that agents may not expose.
// Port 8080 is intentionally excluded: the reverse tunnel architecture means
// the agent-side port never collides with the hub listener, and in deployments
// where the hub API is served behind a load balancer on a different port,
// path-based forwarding on 8080 is valid.
var deniedExposedPorts = map[int]string{
	9810:  "scion hub API",
	18380: "scion metadata server",
}

var errNoPortTunnel = errors.New("no active port-forward tunnel")
var errTunnelBusy = errors.New("too many concurrent port-forward requests")

var portTunnelUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type PortTunnelManager struct {
	mu           sync.RWMutex
	sessions     map[string]*PortTunnelSession
	onDisconnect func(agentID string) // called when a tunnel session disconnects
}

func NewPortTunnelManager() *PortTunnelManager {
	return &PortTunnelManager{sessions: make(map[string]*PortTunnelSession)}
}

func (m *PortTunnelManager) Register(agentID string, conn *websocket.Conn, remoteAddr string) *PortTunnelSession {
	conn.SetReadLimit(96 << 20)
	s := &PortTunnelSession{
		agentID:  agentID,
		conn:     conn,
		pending:  make(map[string]chan portforward.Response),
		inflight: make(chan struct{}, maxConcurrentStreams),
		done:     make(chan struct{}),
	}

	m.mu.Lock()
	if old := m.sessions[agentID]; old != nil {
		old.close()
	}
	m.sessions[agentID] = s
	m.mu.Unlock()

	slog.Info("Port-forward tunnel connected", "agent_id", agentID, "remote_addr", remoteAddr)

	go s.readLoop(func() {
		m.mu.Lock()
		isCurrent := m.sessions[agentID] == s
		if isCurrent {
			delete(m.sessions, agentID)
		}
		onDisconnect := m.onDisconnect
		m.mu.Unlock()

		slog.Info("Port-forward tunnel disconnected", "agent_id", agentID)

		if isCurrent && onDisconnect != nil {
			onDisconnect(agentID)
		}
	})
	return s
}

func (m *PortTunnelManager) Do(ctx context.Context, agentID string, req portforward.Request) (*portforward.Response, error) {
	m.mu.RLock()
	s := m.sessions[agentID]
	m.mu.RUnlock()
	if s == nil {
		return nil, errNoPortTunnel
	}
	return s.do(ctx, req)
}

type PortTunnelSession struct {
	agentID  string
	conn     *websocket.Conn
	writeMu  sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan portforward.Response
	inflight chan struct{}
	done     chan struct{}
	once     sync.Once
}

func (s *PortTunnelSession) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close()
		s.mu.Lock()
		for id, ch := range s.pending {
			delete(s.pending, id)
			close(ch)
		}
		s.mu.Unlock()
	})
}

func (s *PortTunnelSession) readLoop(onClose func()) {
	defer func() {
		s.close()
		onClose()
	}()
	for {
		var msg portforward.Message
		if err := s.conn.ReadJSON(&msg); err != nil {
			return
		}
		if msg.Type != portforward.MessageTypeResponse && msg.Type != portforward.MessageTypeError {
			continue
		}
		if msg.Response == nil || msg.Response.StreamID == "" {
			continue
		}
		s.mu.Lock()
		ch := s.pending[msg.Response.StreamID]
		delete(s.pending, msg.Response.StreamID)
		s.mu.Unlock()
		if ch != nil {
			ch <- *msg.Response
			close(ch)
		}
	}
}

func (s *PortTunnelSession) do(ctx context.Context, req portforward.Request) (*portforward.Response, error) {
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		return nil, errTunnelBusy
	}

	ch := make(chan portforward.Response, 1)
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return nil, errNoPortTunnel
	default:
		s.pending[req.StreamID] = ch
	}
	s.mu.Unlock()

	s.writeMu.Lock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := s.conn.WriteJSON(portforward.Message{Type: portforward.MessageTypeRequest, Request: &req})
	s.writeMu.Unlock()
	if err != nil {
		s.mu.Lock()
		delete(s.pending, req.StreamID)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errNoPortTunnel
		}
		return &resp, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, req.StreamID)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-s.done:
		return nil, errNoPortTunnel
	}
}

type registerPortRequest struct {
	Port  int    `json:"port"`
	Label string `json:"label,omitempty"`
	Host  string `json:"host,omitempty"`
}

type exposedPortResponse struct {
	store.ExposedPort
	URL      string `json:"url"`
	BasePath string `json:"basePath"`
}

type listPortsResponse struct {
	Ports []exposedPortResponse `json:"ports"`
}

func (s *Server) handleAgentPorts(w http.ResponseWriter, r *http.Request, agentID, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.listAgentPorts(w, r, agentID)
		case http.MethodPost:
			s.registerAgentPort(w, r, agentID)
		case http.MethodDelete:
			s.clearAgentPorts(w, r, agentID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	if rest == "tunnel" {
		s.handleAgentPortTunnel(w, r, agentID)
		return
	}

	portPart, subpath, _ := strings.Cut(rest, "/")
	port, err := strconv.Atoi(portPart)
	if err != nil {
		NotFound(w, "Port")
		return
	}
	if subpath == "proxy" || strings.HasPrefix(subpath, "proxy/") {
		s.proxyAgentPort(w, r, agentID, port, strings.TrimPrefix(subpath, "proxy"))
		return
	}
	if subpath != "" {
		NotFound(w, "Port")
		return
	}

	if r.Method == http.MethodDelete {
		s.deleteAgentPort(w, r, agentID, port)
		return
	}
	MethodNotAllowed(w)
}

func (s *Server) listAgentPorts(w http.ResponseWriter, r *http.Request, agentID string) {
	agent, ok := s.authorizePortAccess(w, r, agentID, ActionRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, listPortsResponse{Ports: exposedPortResponses(agent.ID, agent.ExposedPorts)})
}

func (s *Server) registerAgentPort(w http.ResponseWriter, r *http.Request, agentID string) {
	agent, ok := s.authorizePortRegistration(w, r, agentID)
	if !ok {
		return
	}
	var req registerPortRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.Host == "" {
		req.Host = "127.0.0.1"
	}
	if err := validateExposedPort(req.Port, req.Host); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}
	if len(agent.ExposedPorts) >= maxExposedPortsPerAgent && findExposedPort(agent.ExposedPorts, req.Port) == nil {
		ValidationError(w, "maximum exposed ports per agent exceeded", nil)
		return
	}

	ports := append([]store.ExposedPort(nil), agent.ExposedPorts...)
	if findExposedPort(ports, req.Port) != nil {
		Conflict(w, "Port already registered")
		return
	}
	now := time.Now().UTC()
	ports = append(ports, store.ExposedPort{
		Port:      req.Port,
		Label:     req.Label,
		Host:      req.Host,
		Mode:      "rw",
		ExposedAt: now,
		ExposedBy: "agent",
	})
	if err := s.store.UpdateAgentExposedPorts(r.Context(), agent.ID, ports); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// Re-read agent for committed state
	if updated, err := s.store.GetAgent(r.Context(), agent.ID); err == nil {
		s.events.PublishAgentPorts(r.Context(), updated)
	}
	slog.Info("Port registered", "agent_id", agent.ID, "port", req.Port, "label", req.Label, "caller", identityStringFromContext(r.Context()))
	writeJSON(w, http.StatusCreated, exposedPortResponses(agent.ID, ports)[len(ports)-1])
}

func (s *Server) deleteAgentPort(w http.ResponseWriter, r *http.Request, agentID string, port int) {
	agent, ok := s.authorizePortRegistration(w, r, agentID)
	if !ok {
		return
	}
	ports := make([]store.ExposedPort, 0, len(agent.ExposedPorts))
	found := false
	for _, p := range agent.ExposedPorts {
		if p.Port == port {
			found = true
			continue
		}
		ports = append(ports, p)
	}
	if !found {
		NotFound(w, "Port")
		return
	}
	if err := s.store.UpdateAgentExposedPorts(r.Context(), agent.ID, ports); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// Re-read agent for committed state
	if updated, err := s.store.GetAgent(r.Context(), agent.ID); err == nil {
		s.events.PublishAgentPorts(r.Context(), updated)
	}
	slog.Info("Port deregistered", "agent_id", agent.ID, "port", port, "caller", identityStringFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearAgentPorts(w http.ResponseWriter, r *http.Request, agentID string) {
	agent, ok := s.authorizePortRegistration(w, r, agentID)
	if !ok {
		return
	}
	portCount := len(agent.ExposedPorts)
	if err := s.store.UpdateAgentExposedPorts(r.Context(), agent.ID, nil); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	// Re-read agent for committed state
	if updated, err := s.store.GetAgent(r.Context(), agent.ID); err == nil {
		s.events.PublishAgentPorts(r.Context(), updated)
	}
	slog.Info("All ports cleared", "agent_id", agent.ID, "caller", identityStringFromContext(r.Context()), "count", portCount)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) proxyAgentPort(w http.ResponseWriter, r *http.Request, agentID string, port int, proxyPath string) {
	start := time.Now()
	agent, ok := s.authorizePortAccess(w, r, agentID, ActionPortAccess)
	if !ok {
		return
	}
	exposed := findExposedPort(agent.ExposedPorts, port)
	if exposed == nil {
		if isBrowserRequest(r) {
			writeProxyErrorHTML(w, http.StatusNotFound, "Port Not Available",
				"The requested port is not exposed on this agent.")
		} else {
			NotFound(w, "Port")
		}
		return
	}
	if isWebSocketUpgrade(r) {
		writeError(w, http.StatusNotImplemented, ErrCodeInvalidRequest, "WebSocket port forwarding is not supported in this revision", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyRequestBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodeInvalidRequest, "Request body too large", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), portForwardTimeout)
	defer cancel()
	reqPath := "/" + strings.TrimPrefix(proxyPath, "/")
	resp, err := s.portTunnels.Do(ctx, agent.ID, portforward.Request{
		StreamID: uuid.NewString(),
		Port:     exposed.Port,
		Host:     exposed.Host,
		Method:   r.Method,
		Path:     reqPath,
		Query:    r.URL.RawQuery,
		Header:   cloneForwardHeaders(r.Header),
		Body:     body,
	})
	if err != nil {
		if errors.Is(err, errNoPortTunnel) {
			if isBrowserRequest(r) {
				writeProxyErrorHTML(w, http.StatusServiceUnavailable, "Service Unavailable",
					"No active port-forward tunnel for this agent. The agent may not be running or the tunnel has not been established yet.")
			} else {
				writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeError, "No active port-forward tunnel for this agent", nil)
			}
			return
		}
		if errors.Is(err, errTunnelBusy) {
			writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeError, err.Error(), nil)
			return
		}
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, "Port-forward tunnel failed: "+err.Error(), nil)
		return
	}
	if resp.Error != "" {
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, resp.Error, nil)
		return
	}
	for k, vals := range resp.Header {
		if hopByHopHeader(k) {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
	slog.Debug("Proxy request",
		"agent_id", agent.ID,
		"port", port,
		"caller", identityStringFromContext(r.Context()),
		"method", r.Method,
		"path", reqPath,
		"status", resp.Status,
		"duration", time.Since(start),
	)
}

// writeProxyErrorHTML writes a self-contained HTML error page for browser requests
// to proxy endpoints. It delegates to the shared renderErrorPage template.
func writeProxyErrorHTML(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, proxyErrorPageHTML(title, message))
}

// proxyErrorPageHTML returns a self-contained HTML error page with the given
// title and message, using the shared renderErrorPage template.
func proxyErrorPageHTML(title, message string) string {
	return renderErrorPage("Scion - "+title, "&#9888;&#65039;", "error", title, message)
}

func (s *Server) handleAgentPortTunnel(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.Method != http.MethodGet || !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}
	agent, ok := s.authorizePortRegistration(w, r, agentID)
	if !ok {
		return
	}
	conn, err := portTunnelUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	session := s.portTunnels.Register(agent.ID, conn, r.RemoteAddr)
	<-session.done
}

func (s *Server) authorizePortAccess(w http.ResponseWriter, r *http.Request, agentID string, action Action) (*store.Agent, bool) {
	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		if agentIdent.ID() != agent.ID || agentIdent.ProjectID() != agent.ProjectID {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only access their own port registrations", nil)
			return nil, false
		}
		return agent, true
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		decision := s.authzService.CheckAccess(ctx, userIdent, agentResource(agent), action)
		if !decision.Allowed {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return nil, false
		}
		return agent, true
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, "This action requires user or agent authentication", nil)
	return nil, false
}

func (s *Server) authorizePortRegistration(w http.ResponseWriter, r *http.Request, agentID string) (*store.Agent, bool) {
	agent, ok := s.authorizePortAccess(w, r, agentID, ActionPortAccess)
	if !ok {
		return nil, false
	}
	if agentIdent := GetAgentIdentityFromContext(r.Context()); agentIdent != nil {
		if !agentIdent.HasScope(ScopeAgentPortForward) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Missing required scope: agent:port:forward", nil)
			return nil, false
		}
		return agent, true
	}
	if userIdent := GetUserIdentityFromContext(r.Context()); userIdent != nil {
		if _, scoped := userIdent.(*ScopedUserIdentity); scoped {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Scoped access tokens cannot manage exposed ports", nil)
			return nil, false
		}
		if s.authzService.Decide(r.Context(), AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   Resource{Type: "agent", ID: "hub"},
			Action:     Action("port_access"),
			Permission: "agent.port_access",
		}).Allowed {
			return agent, true
		}
	}
	writeError(w, http.StatusForbidden, ErrCodeForbidden, "Only the agent can manage its exposed ports", nil)
	return nil, false
}

func exposedPortResponses(agentID string, ports []store.ExposedPort) []exposedPortResponse {
	responses := make([]exposedPortResponse, 0, len(ports))
	for _, p := range ports {
		base := fmt.Sprintf("/api/v1/agents/%s/ports/%d/proxy/", url.PathEscape(agentID), p.Port)
		responses = append(responses, exposedPortResponse{ExposedPort: p, URL: base, BasePath: base})
	}
	return responses
}

func findExposedPort(ports []store.ExposedPort, port int) *store.ExposedPort {
	for i := range ports {
		if ports[i].Port == port {
			return &ports[i]
		}
	}
	return nil
}

func validateExposedPort(port int, host string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("host must be loopback in this revision")
	}
	if reason := deniedExposedPorts[port]; reason != "" {
		return fmt.Errorf("port %d is reserved for %s", port, reason)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cloneForwardHeaders(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for k, vals := range in {
		if hopByHopHeader(k) || sensitiveHeader(k) {
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func hopByHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func sensitiveHeader(k string) bool {
	switch strings.ToLower(k) {
	case "authorization", "cookie", "x-scion-agent-token", "x-scion-broker-id", "x-scion-broker-hmac":
		return true
	default:
		return false
	}
}

// identityStringFromContext returns a human-readable identity string for the
// authenticated caller in the request context, for use in audit log entries.
func identityStringFromContext(ctx context.Context) string {
	if agent := GetAgentIdentityFromContext(ctx); agent != nil {
		return "agent:" + agent.ID()
	}
	if user := GetUserIdentityFromContext(ctx); user != nil {
		return "user:" + user.ID()
	}
	return "unknown"
}

// clearExposedPortsForAgent removes all exposed port registrations for an agent.
// It is best-effort: failures are logged but do not propagate, because the
// caller (stop, delete, tunnel disconnect) should not be blocked by cleanup.
func (s *Server) clearExposedPortsForAgent(ctx context.Context, agentID string) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		slog.Debug("clearExposedPortsForAgent: agent lookup failed", "agent_id", agentID, "error", err)
		return
	}
	if len(agent.ExposedPorts) == 0 {
		return
	}
	if err := s.store.UpdateAgentExposedPorts(ctx, agentID, nil); err != nil {
		slog.Warn("clearExposedPortsForAgent: failed to clear ports", "agent_id", agentID, "error", err)
		return
	}
	// Re-read agent for committed state (matches HTTP handler pattern)
	if updated, err := s.store.GetAgent(ctx, agentID); err == nil {
		s.events.PublishAgentPorts(ctx, updated)
	}
	slog.Info("Cleared exposed ports for agent", "agent_id", agentID, "count", len(agent.ExposedPorts))
}

// exposedPortsSweepHandler returns a recurring handler that cleans up stale
// exposed-port registrations. It lists all agents, identifies those with
// non-empty ExposedPorts whose phase is terminal (stopped, error) or that
// have been soft-deleted, and clears their port registrations. This catches
// any ports that were not cleared by active teardown (tunnel disconnect,
// explicit stop/delete).
func (s *Server) exposedPortsSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		// List all agents including soft-deleted ones so we catch every stale registration.
		// Limit: 500 is a deliberate bound — sufficient for typical deployments since
		// port-forwarding agents are a small subset of total agents. The sweep runs
		// every 5 minutes, so any overflow is caught on subsequent runs.
		result, err := s.store.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{Limit: 500})
		if err != nil {
			slog.Error("Scheduler: exposed-ports sweep failed to list agents", "error", err)
			return
		}

		cleared := 0
		for _, agent := range result.Items {
			if len(agent.ExposedPorts) == 0 {
				continue
			}

			// An agent's ports are stale if the agent is soft-deleted or in a
			// terminal phase (stopped, error). We are generous: running,
			// suspended, provisioning, starting, etc. are left alone.
			isDeleted := !agent.DeletedAt.IsZero()
			isTerminal := agent.Phase == string(state.PhaseStopped) || agent.Phase == string(state.PhaseError)

			if !isDeleted && !isTerminal {
				continue
			}

			if err := s.store.UpdateAgentExposedPorts(ctx, agent.ID, nil); err != nil {
				slog.Warn("Scheduler: exposed-ports sweep failed to clear ports",
					"agent_id", agent.ID, "error", err)
				continue
			}
			// Re-read agent for committed state (matches HTTP handler pattern)
			if updated, err := s.store.GetAgent(ctx, agent.ID); err == nil {
				s.events.PublishAgentPorts(ctx, updated)
			}
			cleared++
		}

		if cleared > 0 {
			slog.Info("Scheduler: exposed-ports sweep cleared stale registrations", "count", cleared)
		}
	}
}
