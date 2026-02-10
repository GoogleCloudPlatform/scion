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

package wsprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Default configuration values
const (
	DefaultReadBufferSize  = 4096
	DefaultWriteBufferSize = 4096
	DefaultPingInterval    = 30 * time.Second
	DefaultPongWait        = 60 * time.Second
	DefaultWriteWait       = 10 * time.Second
	DefaultMaxMessageSize  = 64 * 1024 // 64KB
)

// ConnectionConfig holds configuration for a WebSocket connection.
type ConnectionConfig struct {
	ReadBufferSize  int
	WriteBufferSize int
	PingInterval    time.Duration
	PongWait        time.Duration
	WriteWait       time.Duration
	MaxMessageSize  int64
}

// DefaultConnectionConfig returns the default connection configuration.
func DefaultConnectionConfig() ConnectionConfig {
	return ConnectionConfig{
		ReadBufferSize:  DefaultReadBufferSize,
		WriteBufferSize: DefaultWriteBufferSize,
		PingInterval:    DefaultPingInterval,
		PongWait:        DefaultPongWait,
		WriteWait:       DefaultWriteWait,
		MaxMessageSize:  DefaultMaxMessageSize,
	}
}

// Connection wraps a gorilla/websocket connection with thread-safe helpers.
type Connection struct {
	conn     *websocket.Conn
	config   ConnectionConfig
	writeMu  sync.Mutex
	closed   bool
	closedMu sync.RWMutex
}

// NewConnection creates a new Connection wrapper around an existing websocket.Conn.
func NewConnection(conn *websocket.Conn, config ConnectionConfig) *Connection {
	if config.MaxMessageSize > 0 {
		conn.SetReadLimit(config.MaxMessageSize)
	}
	return &Connection{
		conn:   conn,
		config: config,
	}
}

// Upgrader creates a new WebSocket upgrader with the given configuration.
func Upgrader(config ConnectionConfig, checkOrigin func(r *http.Request) bool) websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  config.ReadBufferSize,
		WriteBufferSize: config.WriteBufferSize,
		CheckOrigin:     checkOrigin,
	}
}

// DefaultUpgrader returns an upgrader that allows all origins.
// Use only when authentication is handled by middleware.
func DefaultUpgrader() websocket.Upgrader {
	return Upgrader(DefaultConnectionConfig(), func(r *http.Request) bool {
		return true // Auth already verified by middleware
	})
}

// IsClosed returns whether the connection has been closed.
func (c *Connection) IsClosed() bool {
	c.closedMu.RLock()
	defer c.closedMu.RUnlock()
	return c.closed
}

// ReadMessage reads a message from the WebSocket connection.
// Returns the message type and payload, or an error.
func (c *Connection) ReadMessage() (messageType int, p []byte, err error) {
	if c.IsClosed() {
		return 0, nil, errors.New("connection closed")
	}
	return c.conn.ReadMessage()
}

// ReadJSON reads a JSON message and unmarshals it into v.
func (c *Connection) ReadJSON(v interface{}) error {
	if c.IsClosed() {
		return errors.New("connection closed")
	}
	return c.conn.ReadJSON(v)
}

// WriteMessage writes a message to the WebSocket connection.
// This method is thread-safe.
func (c *Connection) WriteMessage(messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.IsClosed() {
		return errors.New("connection closed")
	}

	if c.config.WriteWait > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteWait)); err != nil {
			return err
		}
	}
	return c.conn.WriteMessage(messageType, data)
}

// WriteJSON marshals v to JSON and writes it to the WebSocket connection.
// This method is thread-safe.
func (c *Connection) WriteJSON(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.IsClosed() {
		return errors.New("connection closed")
	}

	if c.config.WriteWait > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.config.WriteWait)); err != nil {
			return err
		}
	}
	return c.conn.WriteJSON(v)
}

// Close closes the WebSocket connection gracefully.
func (c *Connection) Close() error {
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return nil
	}
	c.closed = true
	c.closedMu.Unlock()

	// Send close message
	c.writeMu.Lock()
	_ = c.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(c.config.WriteWait),
	)
	c.writeMu.Unlock()

	return c.conn.Close()
}

// CloseWithError closes the connection with an error code and message.
func (c *Connection) CloseWithError(code int, message string) error {
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return nil
	}
	c.closed = true
	c.closedMu.Unlock()

	c.writeMu.Lock()
	_ = c.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, message),
		time.Now().Add(c.config.WriteWait),
	)
	c.writeMu.Unlock()

	return c.conn.Close()
}

// SetPongHandler sets a handler for pong messages.
func (c *Connection) SetPongHandler(h func(appData string) error) {
	c.conn.SetPongHandler(h)
}

// SetPingHandler sets a handler for ping messages.
func (c *Connection) SetPingHandler(h func(appData string) error) {
	c.conn.SetPingHandler(h)
}

// SetReadDeadline sets the read deadline on the underlying network connection.
func (c *Connection) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// WritePing sends a ping message.
func (c *Connection) WritePing() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.IsClosed() {
		return errors.New("connection closed")
	}

	return c.conn.WriteControl(
		websocket.PingMessage,
		[]byte{},
		time.Now().Add(c.config.WriteWait),
	)
}

// RemoteAddr returns the remote network address.
func (c *Connection) RemoteAddr() string {
	return c.conn.RemoteAddr().String()
}

// LocalAddr returns the local network address.
func (c *Connection) LocalAddr() string {
	return c.conn.LocalAddr().String()
}

// Config returns the connection configuration.
func (c *Connection) Config() ConnectionConfig {
	return c.config
}

// Underlying returns the underlying websocket.Conn.
// Use with caution - prefer the wrapped methods when possible.
func (c *Connection) Underlying() *websocket.Conn {
	return c.conn
}

// MessageRouter routes incoming messages to handlers based on message type.
type MessageRouter struct {
	handlers map[string]MessageHandler
	mu       sync.RWMutex
}

// MessageHandler is called when a message of the registered type is received.
type MessageHandler func(ctx context.Context, conn *Connection, data []byte) error

// NewMessageRouter creates a new message router.
func NewMessageRouter() *MessageRouter {
	return &MessageRouter{
		handlers: make(map[string]MessageHandler),
	}
}

// Handle registers a handler for a message type.
func (r *MessageRouter) Handle(msgType string, handler MessageHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[msgType] = handler
}

// Route routes a message to its handler.
func (r *MessageRouter) Route(ctx context.Context, conn *Connection, data []byte) error {
	env, err := ParseEnvelope(data)
	if err != nil {
		return err
	}

	r.mu.RLock()
	handler, ok := r.handlers[env.Type]
	r.mu.RUnlock()

	if !ok {
		return &UnknownMessageError{Type: env.Type}
	}

	return handler(ctx, conn, data)
}

// UnknownMessageError is returned when a message type has no handler.
type UnknownMessageError struct {
	Type string
}

func (e *UnknownMessageError) Error() string {
	return "unknown message type: " + e.Type
}

// ParseMessage parses raw JSON data into a specific message type.
func ParseMessage[T any](data []byte) (*T, error) {
	var msg T
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Dial creates a WebSocket connection to the given URL.
func Dial(ctx context.Context, url string, headers http.Header) (*Connection, *http.Response, error) {
	return DialWithConfig(ctx, url, headers, DefaultConnectionConfig())
}

// DialWithConfig creates a WebSocket connection with custom configuration.
func DialWithConfig(ctx context.Context, url string, headers http.Header, config ConnectionConfig) (*Connection, *http.Response, error) {
	dialer := websocket.Dialer{
		ReadBufferSize:  config.ReadBufferSize,
		WriteBufferSize: config.WriteBufferSize,
	}

	conn, resp, err := dialer.DialContext(ctx, url, headers)
	if err != nil {
		return nil, resp, err
	}

	return NewConnection(conn, config), resp, nil
}

// IsCloseError returns true if err is a close error with one of the given codes.
func IsCloseError(err error, codes ...int) bool {
	return websocket.IsCloseError(err, codes...)
}

// IsUnexpectedCloseError returns true if err is an unexpected close error.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {
	return websocket.IsUnexpectedCloseError(err, expectedCodes...)
}

// Close codes from gorilla/websocket
const (
	CloseNormalClosure           = websocket.CloseNormalClosure
	CloseGoingAway               = websocket.CloseGoingAway
	CloseProtocolError           = websocket.CloseProtocolError
	CloseUnsupportedData         = websocket.CloseUnsupportedData
	CloseNoStatusReceived        = websocket.CloseNoStatusReceived
	CloseAbnormalClosure         = websocket.CloseAbnormalClosure
	CloseInvalidFramePayloadData = websocket.CloseInvalidFramePayloadData
	ClosePolicyViolation         = websocket.ClosePolicyViolation
	CloseMessageTooBig           = websocket.CloseMessageTooBig
	CloseMandatoryExtension      = websocket.CloseMandatoryExtension
	CloseInternalServerErr       = websocket.CloseInternalServerErr
	CloseServiceRestart          = websocket.CloseServiceRestart
	CloseTryAgainLater           = websocket.CloseTryAgainLater
	CloseTLSHandshake            = websocket.CloseTLSHandshake
)

// TextMessage and BinaryMessage from gorilla/websocket
const (
	TextMessage   = websocket.TextMessage
	BinaryMessage = websocket.BinaryMessage
)
