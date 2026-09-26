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
	"errors"
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

	// Check authentication - support both Bearer token and ticket parameter
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		// Check for ticket parameter (for browser clients)
		ticket := r.URL.Query().Get("ticket")
		if ticket != "" {
			// Validate ticket (single-use token)
			identity = s.validatePTYTicket(ctx, ticket)
			if identity != nil {
				// Carry the ticket identity on the request so the authorization
				// helper below gates the principal this handler authenticated
				// rather than finding no identity and rejecting the connection.
				ctx = contextWithIdentity(ctx, identity)
				r = r.WithContext(ctx)
			}
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

	// Enforce authorization for every caller kind. A user passes on ActionAttach
	// against the agent; an agent passes on ScopeAgentLifecycle within its own
	// project. Attaching a PTY is not read-class, so the agent project read
	// baseline deliberately does not reach it.
	if !s.authorizeAgentLifecycle(w, r, agent, ActionAttach) {
		return
	}

	// The broker checks run for both WebSocket and preflight requests.
	// Browsers cannot read the HTTP status of a failed WebSocket handshake
	// (they only observe 1006), so the preflight is where a web client learns
	// "no broker, stop" (422) versus "broker down, retry" (503).

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

	// An authorized non-WS request whose broker is connected is a preflight
	// check: return 200 to signal "you have permission and the agent is
	// attachable". Auth errors are already handled above for both kinds.
	if !isWebSocketUpgrade(r) {
		w.WriteHeader(http.StatusOK)
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
		_, _ = fmt.Sscanf(c, "%d", &cols)
	}
	if rowStr := r.URL.Query().Get("rows"); rowStr != "" {
		_, _ = fmt.Sscanf(rowStr, "%d", &rows)
	}

	// Create PTY session
	// Use agent.Slug for the stream since that's what the broker uses to look up containers
	// (containers are labeled with scion.name=<slug>)
	session := newPTYSession(ctx, agent.Slug, agent.ProjectID, agent.RuntimeBrokerID, conn, s.controlChannel, cols, rows)
	defer session.Close()

	slog.Info("PTY session started", "agent_id", agentID, "slug", agent.Slug, "user", identity.ID())

	// Run the session
	if err := session.Run(); err != nil && !isExpectedPTYEnd(err) {
		slog.Error("PTY session error", "agent_id", agentID, "slug", agent.Slug, "error", err)
	}

	code, reason := session.CloseCause()
	slog.Info("PTY session ended", "agent_id", agentID, "slug", agent.Slug,
		"close_code", code, "close_reason", reason)
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
	closeCode   int
	closeReason string
	closeMu     sync.Mutex
}

// Session-ending errors, classified by Run into a close code.
type (
	// ptyClientReadError: reading from the client WebSocket failed.
	ptyClientReadError struct{ err error }
	// ptyClientWriteError: writing to the client WebSocket failed.
	ptyClientWriteError struct{ err error }
	// ptyBrokerWriteError: forwarding client input to the broker failed.
	ptyBrokerWriteError struct{ err error }
	// ptyStreamOpenError: opening the broker stream failed.
	ptyStreamOpenError struct{ err error }
)

func (e *ptyClientReadError) Error() string  { return "client read: " + e.err.Error() }
func (e *ptyClientReadError) Unwrap() error  { return e.err }
func (e *ptyClientWriteError) Error() string { return "client write: " + e.err.Error() }
func (e *ptyClientWriteError) Unwrap() error { return e.err }
func (e *ptyBrokerWriteError) Error() string { return "broker write: " + e.err.Error() }
func (e *ptyBrokerWriteError) Unwrap() error { return e.err }
func (e *ptyStreamOpenError) Error() string  { return "stream open: " + e.err.Error() }
func (e *ptyStreamOpenError) Unwrap() error  { return e.err }

// ptyCloseCause maps the error that ended a PTY session to the close code and
// reason sent to the client. The rows are the close-code table in
// pkg/wsprotocol/pty_close.go.
func ptyCloseCause(err error) (int, string) {
	var streamClosed *StreamClosedError
	var clientRead *ptyClientReadError
	var clientWrite *ptyClientWriteError
	var brokerWrite *ptyBrokerWriteError
	var streamOpen *ptyStreamOpenError
	switch {
	case errors.As(err, &streamClosed):
		// Broker StreamClose (already mapped from legacy codes) or broker
		// connection loss (4503): pass the cause through unchanged.
		return streamClosed.Code, streamClosed.Reason
	case errors.As(err, &streamOpen):
		return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonStreamOpenFailed
	case errors.As(err, &brokerWrite):
		return wsprotocol.ClosePTYUpstreamUnavailable, wsprotocol.CloseReasonBrokerWriteFailed
	case errors.As(err, &clientRead):
		var ce *websocket.CloseError
		if errors.As(clientRead.err, &ce) {
			// Close-code row 1000: the client closed deliberately. (gorilla has
			// already echoed the client's own close frame, so this frame is
			// best effort.)
			return websocket.CloseNormalClosure, ""
		}
		// Read error without a close frame (pong timeout, TCP loss): the
		// client did not close deliberately, so never report 1000. The frame
		// is best effort; a client that still receives it may retry.
		return wsprotocol.ClosePTYInternalError, wsprotocol.CloseReasonClientReadFailed
	case errors.As(err, &clientWrite):
		return wsprotocol.ClosePTYInternalError, wsprotocol.CloseReasonClientWriteFailed
	default:
		return wsprotocol.ClosePTYInternalError, wsprotocol.CloseReasonInternalError
	}
}

// isExpectedPTYEnd reports whether err is an ordinary end of a PTY session
// (broker closed the stream, broker went away, client closed, the session's
// context was canceled) rather than a Hub-side failure worth logging as an
// error. A canceled context reaches here as the bare sentinel value from
// StreamProxy.Read or the readFromClient select loop, not wrapped around
// some other failure, so treating it as expected cannot hide a real one; the
// close code sent to the client is decided separately by ptyCloseCause,
// which this check does not influence.
func isExpectedPTYEnd(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var streamClosed *StreamClosedError
	if errors.As(err, &streamClosed) {
		return true
	}
	var ce *websocket.CloseError
	return errors.As(err, &ce)
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
		err = &ptyStreamOpenError{err: err}
		s.closeWith(ptyCloseCause(err))
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

	// Wait for either direction to fail; the first error decides the code.
	err = <-errCh
	s.closeWith(ptyCloseCause(err))
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
			return &ptyClientReadError{err: err}
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
				return &ptyBrokerWriteError{err: err}
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

		case wsprotocol.TypePing:
			// Application-level liveness probe from the client. Older Hubs
			// ignore unknown types, so clients can detect support by whether
			// a pong ever arrives.
			if err := s.writeToClient(wsprotocol.NewPongMessage()); err != nil {
				return &ptyClientWriteError{err: err}
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
			return &ptyClientWriteError{err: err}
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

// Close closes the PTY session with a normal closure. Close-code row 1000:
// Close is a deliberate close by the session's owner (the handler's deferred
// cleanup, which is a no-op after Run has chosen a code, or a test). Paths
// that end because of a failure use closeWith with the matching code.
func (s *PTYSession) Close() {
	s.closeWith(websocket.CloseNormalClosure, "")
}

// CloseCause returns the close code and reason sent to the client, or
// (0, "") if the session has not been closed.
func (s *PTYSession) CloseCause() (int, string) {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closeCode, s.closeReason
}

// closeWith closes the PTY session, sending code and reason to the client in
// the WebSocket close frame. Only the first call takes effect. A code that
// cannot appear on the wire is replaced with 1011, and the reason is
// truncated to the RFC 6455 limit.
func (s *PTYSession) closeWith(code int, reason string) {
	if !wsprotocol.IsSendableCloseCode(code) {
		code = wsprotocol.ClosePTYInternalError
	}
	reason = wsprotocol.TruncateCloseReason(reason)

	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	s.closeCode = code
	s.closeReason = reason
	s.closeMu.Unlock()

	s.cancel()

	// Close stream to broker
	if s.stream != nil {
		_ = s.controlChan.CloseStream(s.brokerID, s.stream.streamID, "session closed")
	}

	// Close client WebSocket
	s.writeMu.Lock()
	_ = s.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(ptyWriteWait),
	)
	s.writeMu.Unlock()
	_ = s.conn.Close()
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
