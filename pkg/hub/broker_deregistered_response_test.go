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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeHubErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), "body: %s", string(body))
	return resp.Error.Code
}

// TestBrokerAuth_DeregisteredBrokerGetsTerminalCode verifies that after a
// broker is deregistered, the Hub answers its heartbeat, control-channel, and
// conduit requests with 401 and ErrCodeBrokerDeregistered instead of generic
// broker_auth_failed, while an invalid signature for a live broker still
// returns ErrCodeBrokerAuthFailed.
func TestBrokerAuth_DeregisteredBrokerGetsTerminalCode(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)

	// Verify a live broker with a bad signature gets generic broker_auth_failed.
	t.Run("live broker bad signature stays broker_auth_failed", func(t *testing.T) {
		wrongKey := bytes.Repeat([]byte{0x42}, 32)
		req, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/runtime-brokers/"+f.brokerID+"/heartbeat", strings.NewReader(`{"status":"online"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		hmacAuth := &apiclient.HMACAuth{BrokerID: f.brokerID, SecretKey: wrongKey}
		require.NoError(t, hmacAuth.ApplyAuth(req))

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, ErrCodeBrokerAuthFailed, decodeHubErrorCode(t, body))
	})

	// Deregister the broker (deletes row + HMAC secret).
	require.NoError(t, f.srv.store.DeleteRuntimeBroker(context.Background(), f.brokerID))

	// 1. Heartbeat through full Hub middleware chain.
	t.Run("heartbeat returns broker_deregistered", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, f.public.URL+"/api/v1/runtime-brokers/"+f.brokerID+"/heartbeat", strings.NewReader(`{"status":"online"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		hmacAuth := &apiclient.HMACAuth{BrokerID: f.brokerID, SecretKey: f.secret}
		require.NoError(t, hmacAuth.ApplyAuth(req))

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, ErrCodeBrokerDeregistered, decodeHubErrorCode(t, body))
	})

	// 2. Control channel & conduit upgrades through both full handler and bare handlers.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runtime-brokers/connect", f.srv.handleRuntimeBrokerConnect)
	mux.HandleFunc("/api/v1/conduit", f.srv.handleConduit)
	bare := httptest.NewServer(mux)
	t.Cleanup(bare.Close)

	for _, target := range []struct {
		name string
		url  string
	}{
		{name: "full handler", url: f.public.URL},
		{name: "bare handlers", url: bare.URL},
	} {
		for _, path := range []string{"/api/v1/runtime-brokers/connect", "/api/v1/conduit"} {
			t.Run(target.name+" "+path, func(t *testing.T) {
				u := "ws" + strings.TrimPrefix(target.url, "http") + path
				c, resp, err := websocket.DefaultDialer.Dial(u, f.signedHeader(t, path, nil))
				if c != nil {
					_ = c.Close()
				}
				require.Error(t, err)
				require.NotNil(t, resp)
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
				assert.Equal(t, ErrCodeBrokerDeregistered, decodeHubErrorCode(t, body))
			})
		}
	}
}

// TestDeleteRuntimeBroker_DisconnectsActiveControlChannel verifies that
// deregistering a broker via DELETE /api/v1/runtime-brokers/{id} closes its
// active control-channel WebSocket so the broker immediately sees the close
// and receives broker_deregistered on its single reconnect attempt.
func TestDeleteRuntimeBroker_DisconnectsActiveControlChannel(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)

	// Connect the broker on the control channel.
	u := "ws" + strings.TrimPrefix(f.public.URL, "http") + "/api/v1/runtime-brokers/connect"
	conn, resp, err := websocket.DefaultDialer.Dial(u, f.signedHeader(t, "/api/v1/runtime-brokers/connect", nil))
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Send connect handshake message and read connected response.
	require.NoError(t, conn.WriteJSON(map[string]any{
		"type":      "connect",
		"broker_id": f.brokerID,
		"version":   "test",
	}))
	var reply map[string]any
	require.NoError(t, conn.ReadJSON(&reply))
	require.True(t, f.srv.controlChannel.IsConnected(f.brokerID), "broker must be connected before deregister")

	// Deregister the broker through the HTTP handler as admin.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/runtime-brokers/"+f.brokerID, nil)
	delReq.Header.Set("Authorization", "Bearer "+testDevToken)
	delRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	// The control channel must be disconnected immediately.
	assert.False(t, f.srv.controlChannel.IsConnected(f.brokerID), "deregistering broker must disconnect control channel")

	// The client WebSocket should observe EOF/close promptly.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, readErr := conn.ReadMessage()
	assert.Error(t, readErr, "client control-channel websocket must be closed on deregister")
}
