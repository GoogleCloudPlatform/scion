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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/portforward"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	maxExposedPortsPerAgent = 10
	maxProxyRequestBody     = 32 << 20
	portForwardTimeout      = 60 * time.Second
)

var deniedExposedPorts = map[int]string{
	18380: "scion metadata server",
}

var errNoPortTunnel = errors.New("no active port-forward tunnel")

var portTunnelUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type PortTunnelManager struct {
	mu       sync.RWMutex
	sessions map[string]*PortTunnelSession
}

func NewPortTunnelManager() *PortTunnelManager {
	return &PortTunnelManager{sessions: make(map[string]*PortTunnelSession)}
}

func (m *PortTunnelManager) Register(agentID string, conn *websocket.Conn) *PortTunnelSession {
	s := &PortTunnelSession{
		agentID: agentID,
		conn:    conn,
		pending: make(map[string]chan portforward.Response),
		done:    make(chan struct{}),
	}

	m.mu.Lock()
	if old := m.sessions[agentID]; old != nil {
		old.close()
	}
	m.sessions[agentID] = s
	m.mu.Unlock()

	go s.readLoop(func() {
		m.mu.Lock()
		if m.sessions[agentID] == s {
			delete(m.sessions, agentID)
		}
		m.mu.Unlock()
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
	agentID string
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan portforward.Response
	done    chan struct{}
	once    sync.Once
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
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearAgentPorts(w http.ResponseWriter, r *http.Request, agentID string) {
	agent, ok := s.authorizePortRegistration(w, r, agentID)
	if !ok {
		return
	}
	if err := s.store.UpdateAgentExposedPorts(r.Context(), agent.ID, nil); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) proxyAgentPort(w http.ResponseWriter, r *http.Request, agentID string, port int, proxyPath string) {
	agent, ok := s.authorizePortAccess(w, r, agentID, ActionPortAccess)
	if !ok {
		return
	}
	exposed := findExposedPort(agent.ExposedPorts, port)
	if exposed == nil {
		NotFound(w, "Port")
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
			writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeError, "No active port-forward tunnel for this agent", nil)
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
	session := s.portTunnels.Register(agent.ID, conn)
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
	if userIdent := GetUserIdentityFromContext(r.Context()); userIdent != nil && userIdent.Role() == "admin" {
		return agent, true
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
		if hopByHopHeader(k) {
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
