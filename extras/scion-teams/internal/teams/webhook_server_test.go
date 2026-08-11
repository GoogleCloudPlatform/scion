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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockActivityHandler records activities for test assertions.
type mockActivityHandler struct {
	mu         sync.Mutex
	activities []*Activity
	response   *InvokeResponse
	err        error
	done       chan struct{} // closed after HandleActivity completes (for async tests)
}

func newMockActivityHandler() *mockActivityHandler {
	return &mockActivityHandler{
		done: make(chan struct{}, 1),
	}
}

func (m *mockActivityHandler) HandleActivity(_ context.Context, activity *Activity) (*InvokeResponse, error) {
	m.mu.Lock()
	m.activities = append(m.activities, activity)
	m.mu.Unlock()
	select {
	case m.done <- struct{}{}:
	default:
	}
	return m.response, m.err
}

// waitForActivity blocks until HandleActivity has been called at least once.
func (m *mockActivityHandler) waitForActivity(t *testing.T) {
	t.Helper()
	<-m.done
}

// getActivities returns a copy of the recorded activities.
func (m *mockActivityHandler) getActivities() []*Activity {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*Activity, len(m.activities))
	copy(cp, m.activities)
	return cp
}

// testWebhookServer creates a webhook server with a test JWKS for validation.
func testWebhookServer(t *testing.T, handler ActivityHandler) (*httptest.Server, *testJWKS) {
	t.Helper()

	tj := newTestJWKS(t)

	appID := "test-app-id"
	validator := NewJWTValidator(appID)
	validator.openIDMetadataURL = tj.server.URL + "/.well-known/openidconfiguration"
	validator.httpClient = tj.server.Client()

	ws := NewWebhookServer(":0", validator, handler, slog.Default())

	// Create a test server that wraps the webhook server's handler.
	ts := httptest.NewServer(ws.server.Handler)

	return ts, tj
}

func TestWebhookServer_ValidActivity(t *testing.T) {
	handler := newMockActivityHandler()
	ts, tj := testWebhookServer(t, handler)
	defer ts.Close()
	defer tj.close()

	activity := Activity{
		Type: "message",
		ID:   "act-123",
		Text: "Hello bot!",
		From: ChannelAccount{
			ID:   "user-1",
			Name: "Test User",
		},
		Conversation: ConversationAccount{
			ID: "conv-1",
		},
		ServiceURL: "https://smba.trafficmanager.net/amer/",
	}

	body, err := json.Marshal(activity)
	require.NoError(t, err)

	token := tj.signToken(tj.validClaims("test-app-id"))

	req, err := http.NewRequest("POST", ts.URL+"/api/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Non-invoke activities are dispatched asynchronously; wait for completion.
	handler.waitForActivity(t)
	activities := handler.getActivities()
	require.Len(t, activities, 1)
	assert.Equal(t, "message", activities[0].Type)
	assert.Equal(t, "Hello bot!", activities[0].Text)
	assert.Equal(t, "Test User", activities[0].From.Name)
}

func TestWebhookServer_MissingAuthorization(t *testing.T) {
	handler := newMockActivityHandler()
	ts, tj := testWebhookServer(t, handler)
	defer ts.Close()
	defer tj.close()

	body := []byte(`{"type":"message","text":"hello"}`)

	req, err := http.NewRequest("POST", ts.URL+"/api/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, handler.activities)
}

func TestWebhookServer_InvalidJWT(t *testing.T) {
	handler := newMockActivityHandler()
	ts, tj := testWebhookServer(t, handler)
	defer ts.Close()
	defer tj.close()

	body := []byte(`{"type":"message","text":"hello"}`)

	req, err := http.NewRequest("POST", ts.URL+"/api/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer invalid.jwt.token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Empty(t, handler.activities)
}

func TestWebhookServer_MalformedJSON(t *testing.T) {
	handler := newMockActivityHandler()
	ts, tj := testWebhookServer(t, handler)
	defer ts.Close()
	defer tj.close()

	token := tj.signToken(tj.validClaims("test-app-id"))

	req, err := http.NewRequest("POST", ts.URL+"/api/messages", bytes.NewReader([]byte("not json")))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Empty(t, handler.activities)
}

func TestWebhookServer_ActivityTypeRouting(t *testing.T) {
	tests := []struct {
		name         string
		activityType string
	}{
		{"message", "message"},
		{"conversationUpdate", "conversationUpdate"},
		{"invoke", "invoke"},
		{"messageReaction", "messageReaction"},
		{"unknown type", "customType"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newMockActivityHandler()
			ts, tj := testWebhookServer(t, handler)
			defer ts.Close()
			defer tj.close()

			activity := Activity{
				Type: tt.activityType,
				ID:   "act-1",
				From: ChannelAccount{ID: "user-1"},
				Conversation: ConversationAccount{
					ID: "conv-1",
				},
			}

			body, err := json.Marshal(activity)
			require.NoError(t, err)

			token := tj.signToken(tj.validClaims("test-app-id"))

			req, err := http.NewRequest("POST", ts.URL+"/api/messages", bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			// Non-invoke activities are dispatched asynchronously.
			handler.waitForActivity(t)
			activities := handler.getActivities()
			require.Len(t, activities, 1)
			assert.Equal(t, tt.activityType, activities[0].Type)
		})
	}
}

func TestWebhookServer_InvokeResponse(t *testing.T) {
	handler := newMockActivityHandler()
	handler.response = &InvokeResponse{
		Status: http.StatusOK,
		Body:   map[string]string{"result": "success"},
	}
	ts, tj := testWebhookServer(t, handler)
	defer ts.Close()
	defer tj.close()

	activity := Activity{
		Type: "invoke",
		ID:   "inv-1",
		Name: "composeExtension/query",
		From: ChannelAccount{ID: "user-1"},
		Conversation: ConversationAccount{
			ID: "conv-1",
		},
	}

	body, err := json.Marshal(activity)
	require.NoError(t, err)

	token := tj.signToken(tj.validClaims("test-app-id"))

	req, err := http.NewRequest("POST", ts.URL+"/api/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
}

func TestWebhookServer_HealthEndpoint(t *testing.T) {
	handler := newMockActivityHandler()
	ts, tj := testWebhookServer(t, handler)
	defer ts.Close()
	defer tj.close()

	resp, err := http.Get(ts.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]string
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.Equal(t, "ok", result["status"])
}
