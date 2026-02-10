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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/ptone/scion-agent/pkg/wsprotocol"
)

// ControlChannelConfig holds configuration for the control channel.
type ControlChannelConfig struct {
	// PingInterval is how often to send pings to connected brokers.
	PingInterval time.Duration
	// PongWait is how long to wait for a pong response.
	PongWait time.Duration
	// WriteWait is the timeout for writing messages.
	WriteWait time.Duration
	// MaxMessageSize is the maximum message size in bytes.
	MaxMessageSize int64
	// RequestTimeout is the timeout for tunneled HTTP requests.
	RequestTimeout time.Duration
	// Debug enables verbose logging.
	Debug bool
}

// DefaultControlChannelConfig returns the default control channel configuration.
func DefaultControlChannelConfig() ControlChannelConfig {
	return ControlChannelConfig{
		PingInterval:   30 * time.Second,
		PongWait:       60 * time.Second,
		WriteWait:      10 * time.Second,
		MaxMessageSize: 64 * 1024, // 64KB
		RequestTimeout: 120 * time.Second,
		Debug:          false,
	}
}

// ControlChannelManager manages WebSocket connections from Runtime Brokers.
type ControlChannelManager struct {
	connections  map[string]*BrokerConnection // brokerID -> connection
	mu           sync.RWMutex
	config       ControlChannelConfig
	upgrader     websocket.Upgrader
	onDisconnect func(brokerID string)
}

// NewControlChannelManager creates a new control channel manager.
func NewControlChannelManager(config ControlChannelConfig) *ControlChannelManager {
	return &ControlChannelManager{
		connections: make(map[string]*BrokerConnection),
		config:      config,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				// Auth is already verified by middleware
				return true
			},
		},
	}
}

// SetOnDisconnect sets a callback that is invoked when a broker disconnects.
// The callback is called asynchronously after the connection is removed.
func (m *ControlChannelManager) SetOnDisconnect(fn func(brokerID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDisconnect = fn
}

// BrokerConnection represents an active control channel connection to a Runtime Broker.
type BrokerConnection struct {
	brokerID    string
	sessionID string
	conn      *wsprotocol.Connection
	config    ControlChannelConfig

	// Pending requests waiting for responses
	pendingRequests map[string]chan *wsprotocol.ResponseEnvelope
	pendingMu       sync.RWMutex

	// Active streams (for PTY, events, etc.)
	streams   map[string]*StreamProxy
	streamsMu sync.RWMutex

	// Connection state
	connectedAt time.Time
	lastPingAt  time.Time
	lastPongAt  time.Time

	// Cancellation
	ctx    context.Context
	cancel context.CancelFunc
}

// StreamProxy represents a multiplexed stream over the control channel.
type StreamProxy struct {
	streamID   string
	streamType string
	agentID    string
	dataCh     chan []byte
	closeCh    chan struct{}
	closed     bool
	closeMu    sync.Mutex
}

// NewStreamProxy creates a new stream proxy.
func NewStreamProxy(streamID, streamType, agentID string) *StreamProxy {
	return &StreamProxy{
		streamID:   streamID,
		streamType: streamType,
		agentID:    agentID,
		dataCh:     make(chan []byte, 256), // Buffer for data frames
		closeCh:    make(chan struct{}),
	}
}

// Write sends data to the stream.
func (s *StreamProxy) Write(data []byte) error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return errors.New("stream closed")
	}
	s.closeMu.Unlock()

	select {
	case s.dataCh <- data:
		return nil
	case <-s.closeCh:
		return errors.New("stream closed")
	}
}

// Read reads data from the stream.
func (s *StreamProxy) Read(ctx context.Context) ([]byte, error) {
	select {
	case data, ok := <-s.dataCh:
		if !ok {
			return nil, io.EOF
		}
		return data, nil
	case <-s.closeCh:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close closes the stream.
func (s *StreamProxy) Close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.closeCh)
	}
}

// HandleUpgrade upgrades an HTTP connection to a WebSocket control channel.
func (m *ControlChannelManager) HandleUpgrade(w http.ResponseWriter, r *http.Request, brokerID string) error {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("websocket upgrade failed: %w", err)
	}

	wsConn := wsprotocol.NewConnection(conn, wsprotocol.ConnectionConfig{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		PingInterval:    m.config.PingInterval,
		PongWait:        m.config.PongWait,
		WriteWait:       m.config.WriteWait,
		MaxMessageSize:  m.config.MaxMessageSize,
	})

	ctx, cancel := context.WithCancel(context.Background())
	sessionID := uuid.New().String()

	brokerConn := &BrokerConnection{
		brokerID:          brokerID,
		sessionID:       sessionID,
		conn:            wsConn,
		config:          m.config,
		pendingRequests: make(map[string]chan *wsprotocol.ResponseEnvelope),
		streams:         make(map[string]*StreamProxy),
		connectedAt:     time.Now(),
		ctx:             ctx,
		cancel:          cancel,
	}

	// Register the connection
	m.mu.Lock()
	if existing, ok := m.connections[brokerID]; ok {
		// Close existing connection
		existing.Close()
	}
	m.connections[brokerID] = brokerConn
	m.mu.Unlock()

	slog.Info("Broker control channel connected", "brokerID", brokerID, "sessionID", sessionID)

	// Start message handler
	go m.handleConnection(brokerConn)

	// Send connected message
	connectedMsg := wsprotocol.NewConnectedMessage(brokerID, sessionID, int(m.config.PingInterval.Milliseconds()))
	if err := wsConn.WriteJSON(connectedMsg); err != nil {
		slog.Error("Failed to send connected message", "brokerID", brokerID, "error", err)
		brokerConn.Close()
		m.removeConnection(brokerID)
		return err
	}

	return nil
}

// handleConnection handles messages from a connected broker.
func (m *ControlChannelManager) handleConnection(hc *BrokerConnection) {
	defer func() {
		hc.Close()
		m.removeConnection(hc.brokerID)
		slog.Info("Broker control channel disconnected", "brokerID", hc.brokerID)
	}()

	// Set up pong handler
	hc.conn.SetPongHandler(func(appData string) error {
		hc.lastPongAt = time.Now()
		if err := hc.conn.SetReadDeadline(time.Now().Add(m.config.PongWait)); err != nil {
			return err
		}
		return nil
	})

	// Start ping ticker
	go m.pingLoop(hc)

	// Set initial read deadline
	if err := hc.conn.SetReadDeadline(time.Now().Add(m.config.PongWait)); err != nil {
		slog.Error("Failed to set read deadline", "brokerID", hc.brokerID, "error", err)
		return
	}

	for {
		select {
		case <-hc.ctx.Done():
			return
		default:
		}

		_, data, err := hc.conn.ReadMessage()
		if err != nil {
			if wsprotocol.IsUnexpectedCloseError(err, wsprotocol.CloseGoingAway, wsprotocol.CloseNormalClosure) {
				slog.Error("Control channel read error", "brokerID", hc.brokerID, "error", err)
			}
			return
		}

		if err := m.handleMessage(hc, data); err != nil {
			slog.Error("Control channel message handling error", "brokerID", hc.brokerID, "error", err)
		}
	}
}

// handleMessage processes a single message from a broker.
func (m *ControlChannelManager) handleMessage(hc *BrokerConnection, data []byte) error {
	env, err := wsprotocol.ParseEnvelope(data)
	if err != nil {
		return fmt.Errorf("failed to parse message: %w", err)
	}

	switch env.Type {
	case wsprotocol.TypeConnect:
		// Client sent connect message after we already sent connected.
		// This is expected - just acknowledge we received it.
		if m.config.Debug {
			slog.Debug("Received connect message from broker (already connected)", "brokerID", hc.brokerID)
		}
		return nil
	case wsprotocol.TypeResponse:
		return m.handleResponse(hc, data)
	case wsprotocol.TypeStream:
		return m.handleStreamData(hc, data)
	case wsprotocol.TypeStreamClose:
		return m.handleStreamClose(hc, data)
	case wsprotocol.TypeEvent:
		return m.handleEvent(hc, data)
	case wsprotocol.TypePong:
		hc.lastPongAt = time.Now()
		return nil
	default:
		if m.config.Debug {
			slog.Debug("Unknown message type from broker", "brokerID", hc.brokerID, "type", env.Type)
		}
		return nil
	}
}

// handleResponse processes a response message from a broker.
func (m *ControlChannelManager) handleResponse(hc *BrokerConnection, data []byte) error {
	var resp wsprotocol.ResponseEnvelope
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	hc.pendingMu.RLock()
	ch, ok := hc.pendingRequests[resp.RequestID]
	hc.pendingMu.RUnlock()

	if !ok {
		if m.config.Debug {
			slog.Debug("Response for unknown request", "requestID", resp.RequestID)
		}
		return nil
	}

	select {
	case ch <- &resp:
	default:
		slog.Warn("Response channel full", "requestID", resp.RequestID)
	}

	return nil
}

// handleStreamData processes stream data from a broker.
func (m *ControlChannelManager) handleStreamData(hc *BrokerConnection, data []byte) error {
	var frame wsprotocol.StreamFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("failed to parse stream frame: %w", err)
	}

	hc.streamsMu.RLock()
	stream, ok := hc.streams[frame.StreamID]
	hc.streamsMu.RUnlock()

	if !ok {
		if m.config.Debug {
			slog.Debug("Data for unknown stream", "streamID", frame.StreamID)
		}
		return nil
	}

	return stream.Write(frame.Data)
}

// handleStreamClose processes a stream close message.
func (m *ControlChannelManager) handleStreamClose(hc *BrokerConnection, data []byte) error {
	var close wsprotocol.StreamCloseMessage
	if err := json.Unmarshal(data, &close); err != nil {
		return fmt.Errorf("failed to parse stream close: %w", err)
	}

	hc.streamsMu.Lock()
	stream, ok := hc.streams[close.StreamID]
	if ok {
		delete(hc.streams, close.StreamID)
	}
	hc.streamsMu.Unlock()

	if stream != nil {
		stream.Close()
	}

	if m.config.Debug {
		slog.Debug("Control channel stream closed", "streamID", close.StreamID, "reason", close.Reason)
	}

	return nil
}

// handleEvent processes an event message from a broker.
func (m *ControlChannelManager) handleEvent(hc *BrokerConnection, data []byte) error {
	var event wsprotocol.EventMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("failed to parse event: %w", err)
	}

	switch event.Event {
	case wsprotocol.EventHeartbeat:
		// Update last activity time
		hc.lastPongAt = time.Now()
		if m.config.Debug {
			slog.Debug("Control channel heartbeat from broker", "brokerID", hc.brokerID)
		}
	case wsprotocol.EventAgentStatus:
		// TODO: Forward to interested clients
		if m.config.Debug {
			slog.Debug("Agent status update via control channel", "brokerID", hc.brokerID)
		}
	default:
		if m.config.Debug {
			slog.Debug("Unknown control channel event", "brokerID", hc.brokerID, "event", event.Event)
		}
	}

	return nil
}

// pingLoop sends periodic pings to keep the connection alive.
func (m *ControlChannelManager) pingLoop(hc *BrokerConnection) {
	ticker := time.NewTicker(m.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.ctx.Done():
			return
		case <-ticker.C:
			hc.lastPingAt = time.Now()
			if err := hc.conn.WritePing(); err != nil {
				slog.Error("Failed to ping broker", "brokerID", hc.brokerID, "error", err)
				hc.cancel()
				return
			}
		}
	}
}

// removeConnection removes a broker connection from the manager.
func (m *ControlChannelManager) removeConnection(brokerID string) {
	m.mu.Lock()
	delete(m.connections, brokerID)
	cb := m.onDisconnect
	m.mu.Unlock()

	if cb != nil {
		go cb(brokerID)
	}
}

// GetConnection returns the connection for a broker, or nil if not connected.
func (m *ControlChannelManager) GetConnection(brokerID string) *BrokerConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connections[brokerID]
}

// IsConnected returns true if the broker has an active control channel.
func (m *ControlChannelManager) IsConnected(brokerID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.connections[brokerID]
	return ok
}

// TunnelRequest sends an HTTP request through the control channel.
func (m *ControlChannelManager) TunnelRequest(ctx context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	hc := m.GetConnection(brokerID)
	if hc == nil {
		return nil, fmt.Errorf("broker %s not connected", brokerID)
	}

	return hc.TunnelRequest(ctx, req)
}

// OpenStream opens a new multiplexed stream to a broker.
func (m *ControlChannelManager) OpenStream(ctx context.Context, brokerID, streamType, agentID string, cols, rows int) (*StreamProxy, error) {
	hc := m.GetConnection(brokerID)
	if hc == nil {
		return nil, fmt.Errorf("broker %s not connected", brokerID)
	}

	return hc.OpenStream(ctx, streamType, agentID, cols, rows)
}

// SendStreamData sends data on an existing stream.
func (m *ControlChannelManager) SendStreamData(brokerID, streamID string, data []byte) error {
	hc := m.GetConnection(brokerID)
	if hc == nil {
		return fmt.Errorf("broker %s not connected", brokerID)
	}

	return hc.SendStreamData(streamID, data)
}

// CloseStream closes a stream.
func (m *ControlChannelManager) CloseStream(brokerID, streamID, reason string) error {
	hc := m.GetConnection(brokerID)
	if hc == nil {
		return fmt.Errorf("broker %s not connected", brokerID)
	}

	return hc.CloseStream(streamID, reason)
}

// ResizeStream sends a resize message for a stream.
func (m *ControlChannelManager) ResizeStream(brokerID, streamID string, cols, rows int) error {
	hc := m.GetConnection(brokerID)
	if hc == nil {
		return fmt.Errorf("broker %s not connected", brokerID)
	}

	return hc.ResizeStream(streamID, cols, rows)
}

// ListConnectedBrokers returns a list of currently connected broker IDs.
func (m *ControlChannelManager) ListConnectedBrokers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	brokers := make([]string, 0, len(m.connections))
	for brokerID := range m.connections {
		brokers = append(brokers, brokerID)
	}
	return brokers
}

// Shutdown closes all connections and stops the manager.
func (m *ControlChannelManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for brokerID, conn := range m.connections {
		conn.Close()
		delete(m.connections, brokerID)
	}
}

// BrokerConnection methods

// TunnelRequest sends an HTTP request through the control channel and waits for a response.
func (hc *BrokerConnection) TunnelRequest(ctx context.Context, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	// Generate request ID if not set
	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}

	// Create response channel
	respCh := make(chan *wsprotocol.ResponseEnvelope, 1)

	hc.pendingMu.Lock()
	hc.pendingRequests[req.RequestID] = respCh
	hc.pendingMu.Unlock()

	defer func() {
		hc.pendingMu.Lock()
		delete(hc.pendingRequests, req.RequestID)
		hc.pendingMu.Unlock()
	}()

	// Send the request
	if err := hc.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Wait for response with timeout
	timeout := hc.config.RequestTimeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("request timeout after %v", timeout)
	case <-hc.ctx.Done():
		return nil, fmt.Errorf("connection closed")
	}
}

// OpenStream opens a new multiplexed stream.
func (hc *BrokerConnection) OpenStream(ctx context.Context, streamType, agentID string, cols, rows int) (*StreamProxy, error) {
	streamID := uuid.New().String()
	stream := NewStreamProxy(streamID, streamType, agentID)

	hc.streamsMu.Lock()
	hc.streams[streamID] = stream
	hc.streamsMu.Unlock()

	// Send stream open message
	openMsg := wsprotocol.NewStreamOpenMessage(streamID, streamType, agentID, cols, rows)
	if err := hc.conn.WriteJSON(openMsg); err != nil {
		hc.streamsMu.Lock()
		delete(hc.streams, streamID)
		hc.streamsMu.Unlock()
		return nil, fmt.Errorf("failed to open stream: %w", err)
	}

	return stream, nil
}

// SendStreamData sends data on an existing stream.
func (hc *BrokerConnection) SendStreamData(streamID string, data []byte) error {
	frame := wsprotocol.NewStreamFrame(streamID, data)
	return hc.conn.WriteJSON(frame)
}

// CloseStream closes a stream.
func (hc *BrokerConnection) CloseStream(streamID, reason string) error {
	hc.streamsMu.Lock()
	stream, ok := hc.streams[streamID]
	if ok {
		delete(hc.streams, streamID)
	}
	hc.streamsMu.Unlock()

	if stream != nil {
		stream.Close()
	}

	closeMsg := wsprotocol.NewStreamCloseMessage(streamID, reason, 0)
	return hc.conn.WriteJSON(closeMsg)
}

// ResizeStream sends a resize message for a stream.
func (hc *BrokerConnection) ResizeStream(streamID string, cols, rows int) error {
	resizeMsg := wsprotocol.NewStreamResizeMessage(streamID, cols, rows)
	return hc.conn.WriteJSON(resizeMsg)
}

// Close closes the broker connection.
func (hc *BrokerConnection) Close() {
	hc.cancel()

	// Close all streams
	hc.streamsMu.Lock()
	for _, stream := range hc.streams {
		stream.Close()
	}
	hc.streams = make(map[string]*StreamProxy)
	hc.streamsMu.Unlock()

	// Cancel all pending requests
	hc.pendingMu.Lock()
	for _, ch := range hc.pendingRequests {
		close(ch)
	}
	hc.pendingRequests = make(map[string]chan *wsprotocol.ResponseEnvelope)
	hc.pendingMu.Unlock()

	// Close WebSocket connection
	hc.conn.Close()
}

// GetSessionID returns the session ID.
func (hc *BrokerConnection) GetSessionID() string {
	return hc.sessionID
}

// GetBrokerID returns the broker ID.
func (hc *BrokerConnection) GetBrokerID() string {
	return hc.brokerID
}

// GetConnectedAt returns when the connection was established.
func (hc *BrokerConnection) GetConnectedAt() time.Time {
	return hc.connectedAt
}
