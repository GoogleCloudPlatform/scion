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

// Package wsprotocol defines message types and constants used for WebSocket
// communication between the Hub and Runtime Brokers.
package wsprotocol

import (
	"encoding/json"
	"time"
)

// Control channel message types (Hub ↔ Runtime Broker)
const (
	// TypeConnect is sent by Runtime Broker to initiate connection
	TypeConnect = "connect"
	// TypeConnected is sent by Hub to confirm connection
	TypeConnected = "connected"
	// TypeRequest is sent by Hub to tunnel an HTTP request
	TypeRequest = "request"
	// TypeResponse is sent by Runtime Broker with HTTP response
	TypeResponse = "response"
	// TypeStream is sent for streaming data (e.g., PTY)
	TypeStream = "stream"
	// TypeStreamOpen is sent to open a new stream
	TypeStreamOpen = "stream_open"
	// TypeStreamClose is sent to close a stream
	TypeStreamClose = "stream_close"
	// TypeStreamResize is sent to resize a stream (e.g., PTY terminal resize)
	TypeStreamResize = "stream_resize"
	// TypeEvent is sent for async events (heartbeat, status updates)
	TypeEvent = "event"
	// TypePing is sent for keepalive
	TypePing = "ping"
	// TypePong is the response to ping
	TypePong = "pong"
	// TypeError is sent when an error occurs
	TypeError = "error"
)

// PTY message types (Client ↔ Hub)
const (
	// TypeData contains terminal data
	TypeData = "data"
	// TypeResize contains terminal resize info
	TypeResize = "resize"
)

// Event types for TypeEvent messages
const (
	EventHeartbeat    = "heartbeat"
	EventAgentStatus  = "agent_status"
	EventAgentOutput  = "agent_output"
	EventStreamReady  = "stream_ready"
	EventStreamClosed = "stream_closed"
)

// Stream types for TypeStreamOpen
const (
	StreamTypePTY    = "pty"
	StreamTypeEvents = "events"
	StreamTypeLogs   = "logs"
)

// Envelope is the base message structure for all WebSocket messages.
// All messages must have a Type field.
type Envelope struct {
	Type string `json:"type"`
}

// ConnectMessage is sent by Runtime Broker when establishing control channel.
type ConnectMessage struct {
	Type      string   `json:"type"` // Always "connect"
	BrokerID string   `json:"brokerId"`
	Version   string   `json:"version"`
	Groves    []string `json:"groves,omitempty"`    // Grove IDs this broker serves
	Timestamp int64    `json:"timestamp,omitempty"` // Unix timestamp
}

// ConnectedMessage is sent by Hub to confirm successful connection.
type ConnectedMessage struct {
	Type           string `json:"type"` // Always "connected"
	BrokerID string `json:"brokerId"`
	SessionID      string `json:"sessionId"`      // Unique session identifier
	PingIntervalMs int    `json:"pingIntervalMs"` // Expected ping interval
}

// RequestEnvelope tunnels an HTTP request through the control channel.
type RequestEnvelope struct {
	Type      string            `json:"type"` // Always "request"
	RequestID string            `json:"requestId"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     string            `json:"query,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      []byte            `json:"body,omitempty"` // Base64 encoded in JSON
}

// ResponseEnvelope carries an HTTP response back through the control channel.
type ResponseEnvelope struct {
	Type       string            `json:"type"` // Always "response"
	RequestID  string            `json:"requestId"`
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       []byte            `json:"body,omitempty"` // Base64 encoded in JSON
}

// StreamOpenMessage requests opening a new multiplexed stream.
type StreamOpenMessage struct {
	Type       string `json:"type"` // Always "stream_open"
	StreamID   string `json:"streamId"`
	StreamType string `json:"streamType"` // "pty", "events", "logs"
	Slug       string `json:"slug,omitempty"`
	Cols       int    `json:"cols,omitempty"` // For PTY streams
	Rows       int    `json:"rows,omitempty"` // For PTY streams
}

// StreamFrame carries data for a multiplexed stream.
type StreamFrame struct {
	Type     string `json:"type"` // Always "stream"
	StreamID string `json:"streamId"`
	Data     []byte `json:"data,omitempty"` // Base64 encoded in JSON
}

// StreamCloseMessage signals stream termination.
type StreamCloseMessage struct {
	Type     string `json:"type"` // Always "stream_close"
	StreamID string `json:"streamId"`
	Reason   string `json:"reason,omitempty"`
	Code     int    `json:"code,omitempty"` // Optional exit code
}

// StreamResizeMessage requests a stream resize (e.g., terminal window resize).
type StreamResizeMessage struct {
	Type     string `json:"type"` // Always "stream_resize"
	StreamID string `json:"streamId"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
}

// EventMessage carries async events.
type EventMessage struct {
	Type    string          `json:"type"` // Always "event"
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// HeartbeatPayload is the payload for heartbeat events.
type HeartbeatPayload struct {
	Timestamp    int64            `json:"timestamp"`
	ActiveAgents int              `json:"activeAgents"`
	CPUPercent   float64          `json:"cpuPercent,omitempty"`
	MemoryMB     int64            `json:"memoryMb,omitempty"`
	AgentStates  map[string]State `json:"agentStates,omitempty"`
}

// State represents agent state in heartbeat.
type State struct {
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updatedAt"`
}

// PingMessage is a keepalive ping.
type PingMessage struct {
	Type      string `json:"type"` // Always "ping"
	Timestamp int64  `json:"timestamp"`
}

// PongMessage is the response to ping.
type PongMessage struct {
	Type      string `json:"type"` // Always "pong"
	Timestamp int64  `json:"timestamp"`
}

// ErrorMessage indicates an error occurred.
type ErrorMessage struct {
	Type      string `json:"type"` // Always "error"
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"` // If related to a request
	StreamID  string `json:"streamId,omitempty"`  // If related to a stream
}

// PTY-specific messages for client connections

// PTYDataMessage carries terminal I/O data.
type PTYDataMessage struct {
	Type string `json:"type"` // Always "data"
	Data []byte `json:"data"` // Base64 encoded in JSON
}

// PTYResizeMessage carries terminal resize events.
type PTYResizeMessage struct {
	Type string `json:"type"` // Always "resize"
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// Common error codes
const (
	ErrCodeInvalidMessage   = "invalid_message"
	ErrCodeAuthFailed       = "auth_failed"
	ErrCodeBrokerNotFound     = "broker_not_found"
	ErrCodeAgentNotFound    = "agent_not_found"
	ErrCodeStreamNotFound   = "stream_not_found"
	ErrCodeStreamFailed     = "stream_failed"
	ErrCodeTimeout          = "timeout"
	ErrCodeBrokerDisconnected = "broker_disconnected"
	ErrCodeInternalError    = "internal_error"
)

// ParseEnvelope extracts the message type from a raw JSON message.
func ParseEnvelope(data []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// NewConnectMessage creates a connect message for a runtime broker.
func NewConnectMessage(brokerID, version string, groves []string) *ConnectMessage {
	return &ConnectMessage{
		Type:      TypeConnect,
		BrokerID:    brokerID,
		Version:   version,
		Groves:    groves,
		Timestamp: time.Now().Unix(),
	}
}

// NewConnectedMessage creates a connected response.
func NewConnectedMessage(brokerID, sessionID string, pingIntervalMs int) *ConnectedMessage {
	return &ConnectedMessage{
		Type:           TypeConnected,
		BrokerID:         brokerID,
		SessionID:      sessionID,
		PingIntervalMs: pingIntervalMs,
	}
}

// NewRequestEnvelope creates a request envelope for HTTP tunneling.
func NewRequestEnvelope(requestID, method, path, query string, headers map[string]string, body []byte) *RequestEnvelope {
	return &RequestEnvelope{
		Type:      TypeRequest,
		RequestID: requestID,
		Method:    method,
		Path:      path,
		Query:     query,
		Headers:   headers,
		Body:      body,
	}
}

// NewResponseEnvelope creates a response envelope.
func NewResponseEnvelope(requestID string, statusCode int, headers map[string]string, body []byte) *ResponseEnvelope {
	return &ResponseEnvelope{
		Type:       TypeResponse,
		RequestID:  requestID,
		StatusCode: statusCode,
		Headers:    headers,
		Body:       body,
	}
}

// NewStreamOpenMessage creates a stream open request.
func NewStreamOpenMessage(streamID, streamType, slug string, cols, rows int) *StreamOpenMessage {
	return &StreamOpenMessage{
		Type:       TypeStreamOpen,
		StreamID:   streamID,
		StreamType: streamType,
		Slug:       slug,
		Cols:       cols,
		Rows:       rows,
	}
}

// NewStreamFrame creates a stream data frame.
func NewStreamFrame(streamID string, data []byte) *StreamFrame {
	return &StreamFrame{
		Type:     TypeStream,
		StreamID: streamID,
		Data:     data,
	}
}

// NewStreamCloseMessage creates a stream close message.
func NewStreamCloseMessage(streamID, reason string, code int) *StreamCloseMessage {
	return &StreamCloseMessage{
		Type:     TypeStreamClose,
		StreamID: streamID,
		Reason:   reason,
		Code:     code,
	}
}

// NewStreamResizeMessage creates a stream resize message.
func NewStreamResizeMessage(streamID string, cols, rows int) *StreamResizeMessage {
	return &StreamResizeMessage{
		Type:     TypeStreamResize,
		StreamID: streamID,
		Cols:     cols,
		Rows:     rows,
	}
}

// NewErrorMessage creates an error message.
func NewErrorMessage(code, message, requestID, streamID string) *ErrorMessage {
	return &ErrorMessage{
		Type:      TypeError,
		Code:      code,
		Message:   message,
		RequestID: requestID,
		StreamID:  streamID,
	}
}

// NewPingMessage creates a ping message.
func NewPingMessage() *PingMessage {
	return &PingMessage{
		Type:      TypePing,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewPongMessage creates a pong message.
func NewPongMessage() *PongMessage {
	return &PongMessage{
		Type:      TypePong,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewPTYDataMessage creates a PTY data message.
func NewPTYDataMessage(data []byte) *PTYDataMessage {
	return &PTYDataMessage{
		Type: TypeData,
		Data: data,
	}
}

// NewPTYResizeMessage creates a PTY resize message.
func NewPTYResizeMessage(cols, rows int) *PTYResizeMessage {
	return &PTYResizeMessage{
		Type: TypeResize,
		Cols: cols,
		Rows: rows,
	}
}
