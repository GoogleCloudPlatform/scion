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

package teams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubClient_DeliverInbound(t *testing.T) {
	var receivedBody inboundPayload
	var receivedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		receivedHeaders = r.Header.Clone()

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		err = json.Unmarshal(body, &receivedBody)
		require.NoError(t, err)

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	hmacKey := base64.StdEncoding.EncodeToString([]byte("test-secret-key-1234"))
	client := NewHubClient(ts.URL, hmacKey, "teams-broker-1", slog.Default())
	client.httpClient = ts.Client()

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-01-01T00:00:00Z",
		Sender:    "Test User",
		SenderID:  "user-aad-id",
		Msg:       "Hello from Teams",
		Type:      "chat",
		Channel:   "my-project",
		Metadata: map[string]string{
			"teams_conversation_id": "conv-123",
		},
	}

	err := client.DeliverInbound(context.Background(), "teams.message", msg)
	require.NoError(t, err)

	assert.Equal(t, "teams.message", receivedBody.Topic)
	assert.Equal(t, "Hello from Teams", receivedBody.Message.Msg)
	assert.Equal(t, "Test User", receivedBody.Message.Sender)

	// Verify HMAC headers are present.
	assert.NotEmpty(t, receivedHeaders.Get("X-Scion-Broker-ID"))
	assert.Equal(t, "teams-broker-1", receivedHeaders.Get("X-Scion-Broker-ID"))
	assert.NotEmpty(t, receivedHeaders.Get("X-Scion-Signature"))
	assert.NotEmpty(t, receivedHeaders.Get("X-Scion-Timestamp"))
	assert.NotEmpty(t, receivedHeaders.Get("X-Scion-Nonce"))
}

func TestHubClient_DeliverInbound_HubError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer ts.Close()

	hmacKey := base64.StdEncoding.EncodeToString([]byte("test-key"))
	client := NewHubClient(ts.URL, hmacKey, "broker-1", slog.Default())
	client.httpClient = ts.Client()

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Msg:     "test",
		Type:    "chat",
	}

	err := client.DeliverInbound(context.Background(), "test", msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestHubClient_DeliverInbound_Hub4xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer ts.Close()

	hmacKey := base64.StdEncoding.EncodeToString([]byte("key"))
	client := NewHubClient(ts.URL, hmacKey, "broker", slog.Default())
	client.httpClient = ts.Client()

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Msg:     "test",
		Type:    "chat",
	}

	err := client.DeliverInbound(context.Background(), "test", msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "400")
}

func TestHubClient_DeliverCallback(t *testing.T) {
	var receivedData map[string]interface{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/broker/callback", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var payload callbackPayload
		err = json.Unmarshal(body, &payload)
		require.NoError(t, err)
		receivedData = payload.Data

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	hmacKey := base64.StdEncoding.EncodeToString([]byte("secret"))
	client := NewHubClient(ts.URL, hmacKey, "broker", slog.Default())
	client.httpClient = ts.Client()

	data := map[string]interface{}{
		"action": "submit",
		"value":  "test",
	}

	err := client.DeliverCallback(context.Background(), data)
	require.NoError(t, err)
	assert.Equal(t, "submit", receivedData["action"])
}

func TestHubClient_SignRequest_NoCredentials(t *testing.T) {
	// When no HMAC credentials are set, signing should be a no-op.
	client := NewHubClient("http://example.com", "", "", slog.Default())

	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	err := client.signRequest(req)
	assert.NoError(t, err)
	assert.Empty(t, req.Header.Get("X-Scion-Signature"))
}
