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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// PTY endpoint configuration
const (
	ptyReadBufferSize  = 4096
	ptyWriteBufferSize = 4096
	ptyPongWait        = 60 * time.Second
	ptyPingInterval    = 30 * time.Second
	ptyWriteWait       = 10 * time.Second
)

var ptyUpgrader = websocket.Upgrader{
	ReadBufferSize:  ptyReadBufferSize,
	WriteBufferSize: ptyWriteBufferSize,
	CheckOrigin: func(r *http.Request) bool {
		// Auth is checked before upgrade
		return true
	},
}

// handleAgentPTY handles WebSocket connections for PTY access to an agent.
// Route: GET /api/v1/agents/{id}/pty
func (s *Server) handleAgentPTY(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Extract agent ID from path
	agentID := extractAgentIDFromPTYPath(r.URL.Path)
	if agentID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid agent ID", nil)
		return
	}

	// Verify WebSocket upgrade
	if !isWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}

	// Check authentication - support both Bearer token and ticket parameter
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		// Check for ticket parameter (for browser clients)
		ticket := r.URL.Query().Get("ticket")
		if ticket != "" {
			// Validate ticket (single-use token)
			identity = s.validatePTYTicket(ctx, ticket)
		}
	}

	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Authentication required", nil)
		return
	}

	// Get agent details
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		NotFound(w, "Agent")
		return
	}

	// Enforce policy-based authorization: only the agent's creator (owner) or admins can access PTY
	if user := GetUserIdentityFromContext(ctx); user != nil {
		decision := s.authzService.CheckAccess(ctx, user, agentResource(agent), ActionAttach)
		if !decision.Allowed {
			slog.Warn("PTY access denied: policy check failed",
				"agent_id", agentID,
				"userID", user.ID(),
				"reason", decision.Reason)
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
			return
		}
	}

	// Check if agent has a runtime broker
	if agent.RuntimeBrokerID == "" {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker,
			"Agent has no runtime broker", nil)
		return
	}

	// Check if broker is connected via control channel
	if s.controlChannel == nil || !s.controlChannel.IsConnected(agent.RuntimeBrokerID) {
		writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
			"Runtime broker not connected", nil)
		return
	}

	// Upgrade to WebSocket
	conn, err := ptyUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed for agent", "agent_id", agentID, "error", err)
		return
	}

	// Get terminal size from query params
	cols := 80
	rows := 24
	if c := r.URL.Query().Get("cols"); c != "" {
		fmt.Sscanf(c, "%d", &cols)
	}
	if rowStr := r.URL.Query().Get("rows"); rowStr != "" {
		fmt.Sscanf(rowStr, "%d", &rows)
	}

	// Create PTY session
	// Use agent.Slug for the stream since that's what the broker uses to look up containers
	// (containers are labeled with scion.name=<slug>)
	session := newPTYSession(ctx, agent.Slug, agent.ProjectID, agent.RuntimeBrokerID, conn, s.controlChannel, cols, rows)
	defer session.Close()

	slog.Info("PTY session started", "agent_id", agentID, "slug", agent.Slug, "user", identity.ID())

	// Run the session
	if err := session.Run(); err != nil && err != io.EOF {
		slog.Error("PTY session error", "agent_id", agentID, "slug", agent.Slug, "error", err)
	}

	slog.Info("PTY session ended", "agent_id", agentID, "slug", agent.Slug)
}

// extractAgentIDFromPTYPath extracts the agent ID from a PTY path.
// Path format: /api/v1/agents/{id}/pty
func extractAgentIDFromPTYPath(path string) string {
	const prefix = "/api/v1/agents/"
	const suffix = "/pty"

	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}

	path = strings.TrimPrefix(path, prefix)
	path = strings.TrimSuffix(path, suffix)
	return path
}

// validatePTYTicket validates a single-use PTY ticket.
// Returns the identity associated with the ticket, or nil if invalid.
func (s *Server) validatePTYTicket(ctx context.Context, ticket string) Identity {
	// For now, tickets are not implemented - return nil
	// TODO: Implement ticket validation for browser clients
	_ = ctx
	_ = ticket
	return nil
}

// PTYSession manages a PTY WebSocket session.
type PTYSession struct {
	ctx         context.Context
	cancel      context.CancelFunc
	agentID     string
	projectID   string
	brokerID    string
	conn        *websocket.Conn
	controlChan *ControlChannelManager
	stream      *StreamProxy
	cols        int
	rows        int
	writeMu     sync.Mutex
	closed      bool
	closeMu     sync.Mutex
}

// newPTYSession creates a new PTY session.
func newPTYSession(ctx context.Context, agentID, projectID, brokerID string, conn *websocket.Conn, cc *ControlChannelManager, cols, rows int) *PTYSession {
	ctx, cancel := context.WithCancel(ctx)
	return &PTYSession{
		ctx:         ctx,
		cancel:      cancel,
		agentID:     agentID,
		projectID:   projectID,
		brokerID:    brokerID,
		conn:        conn,
		controlChan: cc,
		cols:        cols,
		rows:        rows,
	}
}

// Run starts the PTY session and blocks until it ends.
func (s *PTYSession) Run() error {
	// Open stream to broker
	stream, err := s.controlChan.OpenStream(s.ctx, s.brokerID, wsprotocol.StreamTypePTY, s.agentID, s.projectID, s.cols, s.rows)
	if err != nil {
		return err
	}
	s.stream = stream

	// Set up ping/pong for client connection
	s.conn.SetPongHandler(func(appData string) error {
		return s.conn.SetReadDeadline(time.Now().Add(ptyPongWait))
	})

	// Start goroutines for bidirectional data flow
	errCh := make(chan error, 2)

	// Client -> Broker
	go func() {
		errCh <- s.readFromClient()
	}()

	// Broker -> Client
	go func() {
		errCh <- s.readFromBroker()
	}()

	// Start ping ticker
	go s.pingLoop()

	// Wait for either direction to fail
	err = <-errCh
	s.Close()
	return err
}

// readFromClient reads messages from the WebSocket client and forwards to broker.
func (s *PTYSession) readFromClient() error {
	if err := s.conn.SetReadDeadline(time.Now().Add(ptyPongWait)); err != nil {
		return err
	}

	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}

		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return err
		}

		// Parse the message
		env, err := wsprotocol.ParseEnvelope(data)
		if err != nil {
			continue // Ignore malformed messages
		}

		switch env.Type {
		case wsprotocol.TypeData:
			var msg wsprotocol.PTYDataMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			// Forward data to broker via stream
			if err := s.controlChan.SendStreamData(s.brokerID, s.stream.streamID, msg.Data); err != nil {
				return err
			}

		case wsprotocol.TypeResize:
			var msg wsprotocol.PTYResizeMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			// Forward resize to broker via control channel
			if err := s.controlChan.ResizeStream(s.brokerID, s.stream.streamID, msg.Cols, msg.Rows); err != nil {
				slog.Debug("PTY Resize forward failed", "agent_id", s.agentID, "error", err)
			}
		}
	}
}

// readFromBroker reads data from the broker stream and forwards to client.
func (s *PTYSession) readFromBroker() error {
	for {
		data, err := s.stream.Read(s.ctx)
		if err != nil {
			return err
		}

		msg := wsprotocol.NewPTYDataMessage(data)
		if err := s.writeToClient(msg); err != nil {
			return err
		}
	}
}

// writeToClient writes a message to the WebSocket client.
func (s *PTYSession) writeToClient(v interface{}) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.conn.SetWriteDeadline(time.Now().Add(ptyWriteWait)); err != nil {
		return err
	}
	return s.conn.WriteJSON(v)
}

// pingLoop sends periodic pings to the client.
func (s *PTYSession) pingLoop() {
	ticker := time.NewTicker(ptyPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.writeMu.Lock()
			err := s.conn.WriteControl(
				websocket.PingMessage,
				[]byte{},
				time.Now().Add(ptyWriteWait),
			)
			s.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// Close closes the PTY session.
func (s *PTYSession) Close() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	s.closeMu.Unlock()

	s.cancel()

	// Close stream to broker
	if s.stream != nil {
		s.controlChan.CloseStream(s.brokerID, s.stream.streamID, "session closed")
	}

	// Close client WebSocket
	s.writeMu.Lock()
	s.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(ptyWriteWait),
	)
	s.writeMu.Unlock()
	s.conn.Close()
}

// CreatePTYTicket creates a single-use ticket for PTY access.
// This is used for browser clients that can't send headers during WebSocket upgrade.
func (s *Server) CreatePTYTicket(ctx context.Context, userID, agentID string) (string, error) {
	// Generate a secure random ticket
	ticket := uuid.New().String()

	// TODO: Store ticket with expiration (e.g., 60 seconds)
	// For now, this is a placeholder
	_ = ctx
	_ = userID
	_ = agentID

	return ticket, nil
}
