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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentService_List_QueryParameters(t *testing.T) {
	projectID := "project-123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("projectId") != projectID {
			t.Errorf("expected projectId %q, got %q", projectID, query.Get("projectId"))
		}
		if query.Get("groveId") != projectID {
			t.Errorf("expected groveId %q, got %q", projectID, query.Get("groveId"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agents": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = client.Agents().List(context.Background(), &ListAgentsOptions{
		ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
}

func TestListAgentsPageLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		if limit != "25" {
			t.Errorf("expected limit=25 in query, got limit=%q", limit)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agents": [], "totalCount": 0}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	opts := &ListAgentsOptions{}
	opts.Page.Limit = 25
	_, err = client.Agents().List(context.Background(), opts)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
}

func TestListAgentsPageLimitZeroOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		if limit != "" {
			t.Errorf("expected no limit param when Limit=0, got limit=%q", limit)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agents": [], "totalCount": 0}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = client.Agents().List(context.Background(), &ListAgentsOptions{})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
}

func TestSubscriptionService_List_QueryParameters(t *testing.T) {
	projectID := "project-123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("projectId") != projectID {
			t.Errorf("expected projectId %q, got %q", projectID, query.Get("projectId"))
		}
		if query.Get("groveId") != projectID {
			t.Errorf("expected groveId %q, got %q", projectID, query.Get("groveId"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = client.Subscriptions().List(context.Background(), &ListSubscriptionsOptions{
		ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
}

func TestSubscriptionTemplateService_List_QueryParameters(t *testing.T) {
	projectID := "project-123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("projectId") != projectID {
			t.Errorf("expected projectId %q, got %q", projectID, query.Get("projectId"))
		}
		if query.Get("groveId") != projectID {
			t.Errorf("expected groveId %q, got %q", projectID, query.Get("groveId"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	_, err = client.SubscriptionTemplates().List(context.Background(), projectID)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// OutboundMessageRequest serialization
// ---------------------------------------------------------------------------

func TestOutboundMessageRequest_JSONRoundTrip(t *testing.T) {
	req := OutboundMessageRequest{
		Recipient:       "user:alice",
		RecipientID:     "uid-1",
		Msg:             "hello",
		Type:            "instruction",
		Urgent:          true,
		Attachments:     []string{"/tmp/a.txt"},
		Channel:         "web",
		ThreadID:        "dm:agent:x:agent:y",
		Metadata:        map[string]string{"k": "v"},
		ConversationID:  "conv-1",
		ConversationRef: "",
		Wake:            true,
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)

	// Verify key wire names match what the hub expects.
	assert.Contains(t, string(data), `"recipient":"user:alice"`)
	assert.Contains(t, string(data), `"recipient_id":"uid-1"`)
	assert.Contains(t, string(data), `"msg":"hello"`)
	assert.Contains(t, string(data), `"type":"instruction"`)
	assert.Contains(t, string(data), `"urgent":true`)
	assert.Contains(t, string(data), `"wake":true`)
	assert.Contains(t, string(data), `"conversation_id":"conv-1"`)

	// Round-trip.
	var decoded OutboundMessageRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, req, decoded)
}

func TestOutboundMessageRequest_OmitEmpty(t *testing.T) {
	// A minimal request should only carry the required msg field and omit
	// all zero-valued optional fields.
	req := OutboundMessageRequest{Msg: "hi"}
	data, err := json.Marshal(req)
	require.NoError(t, err)

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &m))

	// Required.
	assert.Equal(t, "hi", m["msg"])

	// Optional fields must be absent (omitempty).
	for _, key := range []string{
		"recipient", "recipient_id", "type", "urgent",
		"attachments", "channel", "thread_id", "metadata",
		"conversation_id", "conversation_ref", "wake",
	} {
		_, present := m[key]
		assert.False(t, present, "expected %q to be omitted from JSON", key)
	}
}

// ---------------------------------------------------------------------------
// OutboundMessageResult serialization
// ---------------------------------------------------------------------------

func TestOutboundMessageResult_JSONRoundTrip(t *testing.T) {
	result := OutboundMessageResult{
		MessageID:   "msg-uuid-1",
		Status:      "sent",
		Recipient:   "agent:builder",
		RecipientID: "aid-42",
	}
	data, err := json.Marshal(result)
	require.NoError(t, err)

	assert.Contains(t, string(data), `"message_id":"msg-uuid-1"`)
	assert.Contains(t, string(data), `"status":"sent"`)
	assert.Contains(t, string(data), `"recipient":"agent:builder"`)
	assert.Contains(t, string(data), `"recipient_id":"aid-42"`)

	var decoded OutboundMessageResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, result, decoded)
}

// ---------------------------------------------------------------------------
// SendOutboundMessage HTTP tests
// ---------------------------------------------------------------------------

func TestSendOutboundMessage_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request method and path.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agents/agent-1/outbound-message", r.URL.Path)

		// Verify request body includes all fields.
		var req OutboundMessageRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "hello world", req.Msg)
		assert.Equal(t, "instruction", req.Type)
		assert.True(t, req.Urgent)
		assert.True(t, req.Wake)
		assert.Equal(t, "conv:abc", req.ConversationRef)
		assert.Equal(t, []string{"/tmp/file.txt"}, req.Attachments)

		// Write success response matching hub's wire format.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message_id":   "msg-uuid-123",
			"status":       "sent",
			"recipient":    "user:alice",
			"recipient_id": "uid-alice",
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	result, err := client.Agents().SendOutboundMessage(context.Background(), "agent-1", &OutboundMessageRequest{
		Msg:             "hello world",
		Type:            "instruction",
		Urgent:          true,
		Wake:            true,
		ConversationRef: "conv:abc",
		Attachments:     []string{"/tmp/file.txt"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "msg-uuid-123", result.MessageID)
	assert.Equal(t, "sent", result.Status)
	assert.Equal(t, "user:alice", result.Recipient)
	assert.Equal(t, "uid-alice", result.RecipientID)
}

func TestSendOutboundMessage_ProjectScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the project-scoped path.
		assert.Equal(t, "/api/v1/projects/proj-42/agents/agent-1/outbound-message", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message_id":   "msg-uuid-456",
			"status":       "sent",
			"recipient":    "agent:target",
			"recipient_id": "aid-target",
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	result, err := client.ProjectAgents("proj-42").SendOutboundMessage(
		context.Background(), "agent-1", &OutboundMessageRequest{Msg: "ping"})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "msg-uuid-456", result.MessageID)
}

func TestSendOutboundMessage_NonOKError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "rate limited",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"code":"rate_limited","message":"send rate limit exceeded (30 messages per minute); retry in 12s"}}`,
			wantCode:   "rate_limited",
			wantMsg:    "send rate limit exceeded",
		},
		{
			name:       "forbidden",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"code":"message_denied","message":"Message delivery denied","details":{"reason":"mode_denied"}}}`,
			wantCode:   "message_denied",
			wantMsg:    "Message delivery denied",
		},
		{
			name:       "unprocessable entity",
			statusCode: http.StatusUnprocessableEntity,
			body:       `{"error":{"code":"validation_error","message":"message exceeds 100000 character limit"}}`,
			wantCode:   "validation_error",
			wantMsg:    "message exceeds 100000 character limit",
		},
		{
			name:       "internal server error",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":{"code":"internal_error","message":"Failed to persist message"}}`,
			wantCode:   "internal_error",
			wantMsg:    "Failed to persist message",
		},
		{
			name:       "bad request plain text body",
			statusCode: http.StatusBadRequest,
			body:       `not json at all`,
			wantCode:   "invalid_request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client, err := New(server.URL)
			require.NoError(t, err)

			result, err := client.Agents().SendOutboundMessage(
				context.Background(), "agent-1", &OutboundMessageRequest{Msg: "test"})
			assert.Nil(t, result)
			require.Error(t, err)

			// The error should be an *apiclient.APIError with the expected code.
			var apiErr *apiError
			if assert.ErrorAs(t, err, &apiErr) {
				assert.Equal(t, tt.statusCode, apiErr.StatusCode)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				if tt.wantMsg != "" {
					assert.Contains(t, apiErr.Message, tt.wantMsg)
				}
			}
		})
	}
}

// apiError is a local type alias so the test can assert on apiclient.APIError
// fields without importing apiclient (which would create a test-only import
// from within the hubclient package).
type apiError = apiclient.APIError

func TestCreateAgentRequest_GCPIdentity_JSONRoundTrip(t *testing.T) {
	req := CreateAgentRequest{
		GCPIdentity: &GCPIdentityConfig{
			MetadataMode:     "assign",
			ServiceAccountID: "sa-123",
		},
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	// Verify the JSON keys match what the hub expects.
	assert.Contains(t, string(data), `"metadata_mode":"assign"`)
	assert.Contains(t, string(data), `"service_account_id":"sa-123"`)
	// Round-trip.
	var decoded CreateAgentRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "assign", decoded.GCPIdentity.MetadataMode)
	assert.Equal(t, "sa-123", decoded.GCPIdentity.ServiceAccountID)
}
