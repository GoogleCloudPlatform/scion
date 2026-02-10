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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
		wantErr  bool
	}{
		{
			name:     "connect message",
			input:    `{"type":"connect","brokerId":"host-1"}`,
			wantType: TypeConnect,
		},
		{
			name:     "request message",
			input:    `{"type":"request","requestId":"req-1"}`,
			wantType: TypeRequest,
		},
		{
			name:     "stream message",
			input:    `{"type":"stream","streamId":"stream-1"}`,
			wantType: TypeStream,
		},
		{
			name:    "invalid json",
			input:   `{invalid`,
			wantErr: true,
		},
		{
			name:     "empty type",
			input:    `{"type":""}`,
			wantType: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := ParseEnvelope([]byte(tt.input))
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, env.Type)
		})
	}
}

func TestConnectMessage(t *testing.T) {
	msg := NewConnectMessage("host-123", "1.0.0", []string{"grove-1", "grove-2"})

	assert.Equal(t, TypeConnect, msg.Type)
	assert.Equal(t, "host-123", msg.BrokerID)
	assert.Equal(t, "1.0.0", msg.Version)
	assert.Equal(t, []string{"grove-1", "grove-2"}, msg.Groves)
	assert.Greater(t, msg.Timestamp, int64(0))

	// Test JSON marshaling
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	var parsed ConnectMessage
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)
	assert.Equal(t, msg.BrokerID, parsed.BrokerID)
	assert.Equal(t, msg.Version, parsed.Version)
}

func TestConnectedMessage(t *testing.T) {
	msg := NewConnectedMessage("host-123", "session-456", 30000)

	assert.Equal(t, TypeConnected, msg.Type)
	assert.Equal(t, "host-123", msg.BrokerID)
	assert.Equal(t, "session-456", msg.SessionID)
	assert.Equal(t, 30000, msg.PingIntervalMs)
}

func TestRequestEnvelope(t *testing.T) {
	headers := map[string]string{"Content-Type": "application/json"}
	body := []byte(`{"key":"value"}`)

	msg := NewRequestEnvelope("req-1", "POST", "/api/v1/agents", "grove=test", headers, body)

	assert.Equal(t, TypeRequest, msg.Type)
	assert.Equal(t, "req-1", msg.RequestID)
	assert.Equal(t, "POST", msg.Method)
	assert.Equal(t, "/api/v1/agents", msg.Path)
	assert.Equal(t, "grove=test", msg.Query)
	assert.Equal(t, headers, msg.Headers)
	assert.Equal(t, body, msg.Body)

	// Test JSON roundtrip
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	var parsed RequestEnvelope
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)
	assert.Equal(t, msg.RequestID, parsed.RequestID)
	assert.Equal(t, msg.Body, parsed.Body)
}

func TestResponseEnvelope(t *testing.T) {
	headers := map[string]string{"Content-Type": "application/json"}
	body := []byte(`{"id":"agent-1"}`)

	msg := NewResponseEnvelope("req-1", 201, headers, body)

	assert.Equal(t, TypeResponse, msg.Type)
	assert.Equal(t, "req-1", msg.RequestID)
	assert.Equal(t, 201, msg.StatusCode)
	assert.Equal(t, headers, msg.Headers)
	assert.Equal(t, body, msg.Body)
}

func TestStreamOpenMessage(t *testing.T) {
	msg := NewStreamOpenMessage("stream-1", StreamTypePTY, "agent-123", 120, 40)

	assert.Equal(t, TypeStreamOpen, msg.Type)
	assert.Equal(t, "stream-1", msg.StreamID)
	assert.Equal(t, StreamTypePTY, msg.StreamType)
	assert.Equal(t, "agent-123", msg.Slug)
	assert.Equal(t, 120, msg.Cols)
	assert.Equal(t, 40, msg.Rows)
}

func TestStreamFrame(t *testing.T) {
	data := []byte("hello world")
	msg := NewStreamFrame("stream-1", data)

	assert.Equal(t, TypeStream, msg.Type)
	assert.Equal(t, "stream-1", msg.StreamID)
	assert.Equal(t, data, msg.Data)

	// Test JSON roundtrip (bytes are base64 encoded)
	jsonData, err := json.Marshal(msg)
	require.NoError(t, err)

	var parsed StreamFrame
	err = json.Unmarshal(jsonData, &parsed)
	require.NoError(t, err)
	assert.Equal(t, data, parsed.Data)
}

func TestStreamCloseMessage(t *testing.T) {
	msg := NewStreamCloseMessage("stream-1", "normal closure", 0)

	assert.Equal(t, TypeStreamClose, msg.Type)
	assert.Equal(t, "stream-1", msg.StreamID)
	assert.Equal(t, "normal closure", msg.Reason)
	assert.Equal(t, 0, msg.Code)
}

func TestErrorMessage(t *testing.T) {
	msg := NewErrorMessage(ErrCodeAgentNotFound, "agent not found", "req-1", "")

	assert.Equal(t, TypeError, msg.Type)
	assert.Equal(t, ErrCodeAgentNotFound, msg.Code)
	assert.Equal(t, "agent not found", msg.Message)
	assert.Equal(t, "req-1", msg.RequestID)
	assert.Empty(t, msg.StreamID)
}

func TestPingPongMessages(t *testing.T) {
	before := time.Now().UnixMilli()
	ping := NewPingMessage()
	pong := NewPongMessage()
	after := time.Now().UnixMilli()

	assert.Equal(t, TypePing, ping.Type)
	assert.GreaterOrEqual(t, ping.Timestamp, before)
	assert.LessOrEqual(t, ping.Timestamp, after)

	assert.Equal(t, TypePong, pong.Type)
	assert.GreaterOrEqual(t, pong.Timestamp, before)
	assert.LessOrEqual(t, pong.Timestamp, after)
}

func TestPTYMessages(t *testing.T) {
	t.Run("data message", func(t *testing.T) {
		data := []byte("ls -la\n")
		msg := NewPTYDataMessage(data)

		assert.Equal(t, TypeData, msg.Type)
		assert.Equal(t, data, msg.Data)
	})

	t.Run("resize message", func(t *testing.T) {
		msg := NewPTYResizeMessage(120, 40)

		assert.Equal(t, TypeResize, msg.Type)
		assert.Equal(t, 120, msg.Cols)
		assert.Equal(t, 40, msg.Rows)
	})
}

func TestParseMessage(t *testing.T) {
	t.Run("parse connect message", func(t *testing.T) {
		data := []byte(`{"type":"connect","brokerId":"host-1","version":"1.0.0"}`)
		msg, err := ParseMessage[ConnectMessage](data)
		require.NoError(t, err)
		assert.Equal(t, "host-1", msg.BrokerID)
		assert.Equal(t, "1.0.0", msg.Version)
	})

	t.Run("parse request envelope", func(t *testing.T) {
		data := []byte(`{"type":"request","requestId":"req-1","method":"GET","path":"/api/test"}`)
		msg, err := ParseMessage[RequestEnvelope](data)
		require.NoError(t, err)
		assert.Equal(t, "req-1", msg.RequestID)
		assert.Equal(t, "GET", msg.Method)
	})

	t.Run("invalid json", func(t *testing.T) {
		data := []byte(`{invalid}`)
		_, err := ParseMessage[ConnectMessage](data)
		assert.Error(t, err)
	})
}

func TestEventMessage(t *testing.T) {
	payload := HeartbeatPayload{
		Timestamp:    time.Now().Unix(),
		ActiveAgents: 5,
		CPUPercent:   45.5,
		MemoryMB:     1024,
	}

	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)

	msg := EventMessage{
		Type:    TypeEvent,
		Event:   EventHeartbeat,
		Payload: payloadBytes,
	}

	// Marshal and unmarshal
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	var parsed EventMessage
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	assert.Equal(t, TypeEvent, parsed.Type)
	assert.Equal(t, EventHeartbeat, parsed.Event)

	// Parse payload
	var parsedPayload HeartbeatPayload
	err = json.Unmarshal(parsed.Payload, &parsedPayload)
	require.NoError(t, err)
	assert.Equal(t, 5, parsedPayload.ActiveAgents)
	assert.Equal(t, 45.5, parsedPayload.CPUPercent)
}
