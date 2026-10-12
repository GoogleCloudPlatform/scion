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
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestTeamsChannel creates a TeamsChannel wired to test servers for the
// token endpoint and the Bot Connector API.
func newTestTeamsChannel(t *testing.T, activityHandler http.HandlerFunc) *TeamsChannel {
	t.Helper()

	// Mock token endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(teamsTokenResponse{
			AccessToken: "test-token-12345",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	t.Cleanup(tokenServer.Close)

	// Mock Bot Connector API.
	apiServer := httptest.NewServer(activityHandler)
	t.Cleanup(apiServer.Close)

	ch := &TeamsChannel{
		appID:          "test-app-id",
		appSecret:      "test-app-secret",
		tenantID:       "test-tenant-id",
		conversationID: "conv-123",
		serviceURL:     apiServer.URL,
		client:         apiServer.Client(),
	}
	ch.tokenProvider = newTeamsTokenProvider(ch.appID, ch.appSecret, ch.tenantID, ch.client)
	ch.tokenProvider.tokenEndpoint = tokenServer.URL
	ch.tokenProvider.client = tokenServer.Client()

	return ch
}

func TestTeamsChannel_Name(t *testing.T) {
	ch := &TeamsChannel{}
	assert.Equal(t, "teams", ch.Name())
}

func TestTeamsChannel_Validate_Success(t *testing.T) {
	ch := NewTeamsChannel(map[string]string{
		"app_id":          "test-app",
		"app_secret":      "test-secret",
		"tenant_id":       "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"conversation_id": "conv-123",
		"service_url":     "https://smba.trafficmanager.net/test/",
	})
	assert.NoError(t, ch.Validate())
}

func TestTeamsChannel_Validate_MissingParams(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		wantErr string
	}{
		{
			name:    "missing app_id",
			params:  map[string]string{"app_secret": "s", "tenant_id": "t", "conversation_id": "c", "service_url": "u"},
			wantErr: "app_id",
		},
		{
			name:    "missing app_secret",
			params:  map[string]string{"app_id": "a", "tenant_id": "t", "conversation_id": "c", "service_url": "u"},
			wantErr: "app_secret",
		},
		{
			name:    "missing tenant_id",
			params:  map[string]string{"app_id": "a", "app_secret": "s", "conversation_id": "c", "service_url": "u"},
			wantErr: "tenant_id",
		},
		{
			name:    "missing conversation_id",
			params:  map[string]string{"app_id": "a", "app_secret": "s", "tenant_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890", "service_url": "https://example.com"},
			wantErr: "conversation_id",
		},
		{
			name:    "missing service_url",
			params:  map[string]string{"app_id": "a", "app_secret": "s", "tenant_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890", "conversation_id": "c"},
			wantErr: "service_url",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := NewTeamsChannel(tc.params)
			err := ch.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestTeamsChannel_Validate_ServiceURLMustBeHTTPS(t *testing.T) {
	tests := []struct {
		name       string
		serviceURL string
		wantErr    string
	}{
		{
			name:       "http scheme rejected",
			serviceURL: "http://smba.trafficmanager.net/test/",
			wantErr:    "must use https://",
		},
		{
			name:       "ftp scheme rejected",
			serviceURL: "ftp://smba.trafficmanager.net/test/",
			wantErr:    "must use https://",
		},
		{
			name:       "empty scheme rejected",
			serviceURL: "://smba.trafficmanager.net/test/",
			wantErr:    "not a valid URL",
		},
		{
			name:       "no scheme rejected",
			serviceURL: "smba.trafficmanager.net/test/",
			wantErr:    "must use https://",
		},
		{
			name:       "https accepted",
			serviceURL: "https://smba.trafficmanager.net/test/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := NewTeamsChannel(map[string]string{
				"app_id":          "test-app",
				"app_secret":      "test-secret",
				"tenant_id":       "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
				"conversation_id": "conv-123",
				"service_url":     tc.serviceURL,
			})
			err := ch.Validate()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTeamsChannel_Validate_TenantIDMustBeUUID(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		wantErr  bool
	}{
		{
			name:     "valid UUID lowercase",
			tenantID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			wantErr:  false,
		},
		{
			name:     "valid UUID uppercase",
			tenantID: "A1B2C3D4-E5F6-7890-ABCD-EF1234567890",
			wantErr:  false,
		},
		{
			name:     "valid UUID mixed case",
			tenantID: "a1B2c3D4-E5f6-7890-AbCd-eF1234567890",
			wantErr:  false,
		},
		{
			name:     "path traversal",
			tenantID: "../../other",
			wantErr:  true,
		},
		{
			name:     "contains slash",
			tenantID: "a1b2c3d4/e5f6-7890-abcd-ef1234567890",
			wantErr:  true,
		},
		{
			name:     "not a UUID",
			tenantID: "my-tenant",
			wantErr:  true,
		},
		{
			name:     "UUID without hyphens",
			tenantID: "a1b2c3d4e5f67890abcdef1234567890",
			wantErr:  true,
		},
		{
			name:     "contains whitespace",
			tenantID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890 ",
			wantErr:  true,
		},
		{
			name:     "too short",
			tenantID: "a1b2c3d4",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := NewTeamsChannel(map[string]string{
				"app_id":          "test-app",
				"app_secret":      "test-secret",
				"tenant_id":       tc.tenantID,
				"conversation_id": "conv-123",
				"service_url":     "https://smba.trafficmanager.net/test/",
			})
			err := ch.Validate()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "tenant_id must be a valid UUID")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTeamsChannel_Deliver_Success(t *testing.T) {
	var receivedActivity teamsActivity
	var receivedAuth string

	ch := newTestTeamsChannel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/v3/conversations/")
		assert.Contains(t, r.URL.Path, "/activities")

		receivedAuth = r.Header.Get("Authorization")

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &receivedActivity))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "activity-1"})
	}))

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-01-01T00:00:00Z",
		Sender:    "dev-agent",
		Msg:       "Build completed successfully",
		Type:      messages.TypeStateChange,
	}

	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	assert.Equal(t, "message", receivedActivity.Type)
	assert.Contains(t, receivedActivity.Text, "Build completed successfully")
	assert.Contains(t, receivedActivity.Text, "dev-agent")
	assert.Equal(t, "Bearer test-token-12345", receivedAuth)
}

func TestTeamsChannel_Deliver_UrgentMessage(t *testing.T) {
	var receivedActivity teamsActivity

	ch := newTestTeamsChannel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedActivity)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "activity-1"})
	}))

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "monitor",
		Msg:     "Server is down!",
		Type:    messages.TypeStateChange,
		Urgent:  true,
	}

	err := ch.Deliver(context.Background(), msg)
	require.NoError(t, err)

	assert.Contains(t, receivedActivity.Text, "[URGENT]")
}

func TestTeamsChannel_Deliver_AuthError(t *testing.T) {
	// Mock token endpoint that returns an error.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer tokenServer.Close()

	ch := &TeamsChannel{
		appID:          "bad-app",
		appSecret:      "bad-secret",
		tenantID:       "test-tenant",
		conversationID: "conv-123",
		serviceURL:     "https://unused.example.com",
		client:         tokenServer.Client(),
	}
	ch.tokenProvider = newTeamsTokenProvider(ch.appID, ch.appSecret, ch.tenantID, ch.client)
	ch.tokenProvider.tokenEndpoint = tokenServer.URL
	ch.tokenProvider.client = tokenServer.Client()

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "test",
		Msg:     "test message",
		Type:    messages.TypeStateChange,
	}

	err := ch.Deliver(context.Background(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestTeamsChannel_Deliver_APIError(t *testing.T) {
	ch := newTestTeamsChannel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"BotNotInConversation","message":"The bot is not part of the conversation roster."}}`))
	}))

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "test",
		Msg:     "test message",
		Type:    messages.TypeStateChange,
	}

	err := ch.Deliver(context.Background(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestTeamsChannel_TokenCaching(t *testing.T) {
	var tokenRequests int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenRequests, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(teamsTokenResponse{
			AccessToken: "cached-token",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "act-1"})
	}))
	defer apiServer.Close()

	ch := &TeamsChannel{
		appID:          "test-app",
		appSecret:      "test-secret",
		tenantID:       "test-tenant",
		conversationID: "conv-123",
		serviceURL:     apiServer.URL,
		client:         apiServer.Client(),
	}
	ch.tokenProvider = newTeamsTokenProvider(ch.appID, ch.appSecret, ch.tenantID, ch.client)
	ch.tokenProvider.tokenEndpoint = tokenServer.URL
	ch.tokenProvider.client = tokenServer.Client()

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "test",
		Msg:     "test",
		Type:    messages.TypeStateChange,
	}

	// Send multiple messages — token should be fetched only once.
	for i := 0; i < 3; i++ {
		err := ch.Deliver(context.Background(), msg)
		require.NoError(t, err)
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&tokenRequests),
		"token should be cached and fetched only once")
}

func TestFormatTeamsNotification(t *testing.T) {
	tests := []struct {
		name     string
		msg      *messages.StructuredMessage
		contains []string
	}{
		{
			name: "basic message",
			msg: &messages.StructuredMessage{
				Sender: "dev-1",
				Msg:    "Task completed",
				Type:   messages.TypeStateChange,
			},
			contains: []string{"dev-1", "Task completed", messages.TypeStateChange},
		},
		{
			name: "urgent message",
			msg: &messages.StructuredMessage{
				Sender: "monitor",
				Msg:    "Alert!",
				Type:   messages.TypeStateChange,
				Urgent: true,
			},
			contains: []string{"[URGENT]", "monitor", "Alert!"},
		},
		{
			name: "message with recipient",
			msg: &messages.StructuredMessage{
				Sender:    "dev-1",
				Recipient: "reviewer",
				Msg:       "Ready for review",
				Type:      messages.TypeInputNeeded,
			},
			contains: []string{"dev-1", "reviewer", "Ready for review"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := formatTeamsNotification(tc.msg)
			for _, s := range tc.contains {
				assert.Contains(t, result, s)
			}
		})
	}
}
