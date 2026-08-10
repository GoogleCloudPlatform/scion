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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestTokenProvider creates a TokenProvider backed by a test server that
// always returns a valid token.
func newTestTokenProvider(t *testing.T) (*TokenProvider, *httptest.Server) {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "test-token-123",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))

	tp := NewTokenProvider("test-app", "test-secret", "test-tenant")
	tp.tokenEndpoint = ts.URL
	tp.httpClient = ts.Client()

	return tp, ts
}

func TestBuildAPIURL(t *testing.T) {
	tests := []struct {
		name           string
		serviceURL     string
		conversationID string
		activityID     string
		expected       string
	}{
		{
			name:           "send activity",
			serviceURL:     "https://smba.trafficmanager.net/amer",
			conversationID: "conv-123",
			activityID:     "",
			expected:       "https://smba.trafficmanager.net/amer/v3/conversations/conv-123/activities",
		},
		{
			name:           "update activity",
			serviceURL:     "https://smba.trafficmanager.net/amer/",
			conversationID: "conv-123",
			activityID:     "act-456",
			expected:       "https://smba.trafficmanager.net/amer/v3/conversations/conv-123/activities/act-456",
		},
		{
			name:           "trailing slash stripped",
			serviceURL:     "https://example.com/api/",
			conversationID: "c1",
			activityID:     "",
			expected:       "https://example.com/api/v3/conversations/c1/activities",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildAPIURL(tt.serviceURL, tt.conversationID, tt.activityID)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSender_SendActivity(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var receivedAuth string
	var receivedMethod string
	var receivedPath string

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "response-act-1"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{
		Type: "message",
		Text: "Hello from bot",
	}

	activityID, err := sender.sendActivity(context.Background(),
		apiServer.URL, "conv-1", activity)

	require.NoError(t, err)
	assert.Equal(t, "response-act-1", activityID)
	assert.Equal(t, "Bearer test-token-123", receivedAuth)
	assert.Equal(t, "POST", receivedMethod)
	assert.Equal(t, "/v3/conversations/conv-1/activities", receivedPath)
}

func TestSender_ReplyToActivity(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var receivedBody Activity
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "reply-act-1"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{
		Type: "message",
		Text: "Reply text",
	}

	activityID, err := sender.replyToActivity(context.Background(),
		apiServer.URL, "conv-1", "original-act-1", activity)

	require.NoError(t, err)
	assert.Equal(t, "reply-act-1", activityID)
	assert.Equal(t, "original-act-1", receivedBody.ReplyToID)
}

func TestSender_UpdateActivity(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var receivedMethod string
	var receivedPath string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "updated-act"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{
		Type: "message",
		Text: "Updated text",
	}

	err := sender.updateActivity(context.Background(),
		apiServer.URL, "conv-1", "act-1", activity)

	require.NoError(t, err)
	assert.Equal(t, "PUT", receivedMethod)
	assert.Equal(t, "/v3/conversations/conv-1/activities/act-1", receivedPath)
}

func TestSender_DeleteActivity(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var receivedMethod string
	var receivedPath string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	err := sender.deleteActivity(context.Background(),
		apiServer.URL, "conv-1", "act-1")

	require.NoError(t, err)
	assert.Equal(t, "DELETE", receivedMethod)
	assert.Equal(t, "/v3/conversations/conv-1/activities/act-1", receivedPath)
}

func TestSender_TokenRefreshOn401(t *testing.T) {
	var callCount int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			json.NewEncoder(w).Encode(tokenResponse{
				AccessToken: "old-token",
				ExpiresIn:   3600,
			})
		} else {
			json.NewEncoder(w).Encode(tokenResponse{
				AccessToken: "new-token",
				ExpiresIn:   3600,
			})
		}
	}))
	defer tokenServer.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.tokenEndpoint = tokenServer.URL
	tp.httpClient = tokenServer.Client()

	var requestCount int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count == 1 {
			// First request: return 401.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Second request (after token refresh): succeed.
		auth := r.Header.Get("Authorization")
		assert.Equal(t, "Bearer new-token", auth)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "retry-act"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{Type: "message", Text: "test"}
	activityID, err := sender.sendActivity(context.Background(),
		apiServer.URL, "conv-1", activity)

	require.NoError(t, err)
	assert.Equal(t, "retry-act", activityID)
	assert.Equal(t, int32(2), atomic.LoadInt32(&requestCount))
}

func TestSender_RateLimitError(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{Type: "message", Text: "test"}
	_, err := sender.sendActivity(context.Background(),
		apiServer.URL, "conv-1", activity)

	require.Error(t, err)
	var retryErr *RetryAfterError
	assert.ErrorAs(t, err, &retryErr)
	assert.Equal(t, "5s", retryErr.RetryAfter.String())
}

func TestSender_APIError(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "bad request"}`))
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{Type: "message", Text: "test"}
	_, err := sender.sendActivity(context.Background(),
		apiServer.URL, "conv-1", activity)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
}

func TestSender_401RetryNetworkFailure_NoPanic(t *testing.T) {
	// C2 + R6: When the first request returns 401, the token is refreshed,
	// but the retry request fails at the network level. This must not panic
	// (previously the deferred resp.Body.Close() panicked on nil resp).

	var requestCount int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	}))
	defer tokenServer.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.tokenEndpoint = tokenServer.URL
	tp.httpClient = tokenServer.Client()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// For the retry: close the connection abruptly to simulate network failure.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server doesn't support hijacking")
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	activity := &Activity{Type: "message", Text: "test"}
	_, err := sender.sendActivity(context.Background(),
		apiServer.URL, "conv-1", activity)

	// Should return an error, not panic.
	assert.Error(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&requestCount))
}

func TestSender_DeleteActivity_401Retry(t *testing.T) {
	// R4: Verify deleteActivity also retries on 401.

	var requestCount int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 0) // just need different tokens
		_ = count
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	}))
	defer tokenServer.Close()

	tp := NewTokenProvider("app", "secret", "tenant")
	tp.tokenEndpoint = tokenServer.URL
	tp.httpClient = tokenServer.Client()

	var apiRequestCount int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&apiRequestCount, 1)
		if count == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		assert.Equal(t, "DELETE", r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	err := sender.deleteActivity(context.Background(),
		apiServer.URL, "conv-1", "act-1")

	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&apiRequestCount))
}

func TestBuildAPIURL_URLEncoding(t *testing.T) {
	// R5: Verify that conversationID and activityID with special characters
	// are properly percent-encoded.

	tests := []struct {
		name           string
		serviceURL     string
		conversationID string
		activityID     string
		wantContains   string
		wantNotContain string
	}{
		{
			name:           "conversationID with spaces",
			serviceURL:     "https://api.example.com",
			conversationID: "conv id with spaces",
			activityID:     "",
			wantContains:   "conv%20id%20with%20spaces",
		},
		{
			name:           "activityID with slashes",
			serviceURL:     "https://api.example.com",
			conversationID: "conv-1",
			activityID:     "act/with/slashes",
			wantContains:   "act%2Fwith%2Fslashes",
		},
		{
			name:           "normal IDs unchanged",
			serviceURL:     "https://api.example.com",
			conversationID: "conv-123",
			activityID:     "act-456",
			wantContains:   "/v3/conversations/conv-123/activities/act-456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildAPIURL(tt.serviceURL, tt.conversationID, tt.activityID)
			assert.Contains(t, result, tt.wantContains)
		})
	}
}
