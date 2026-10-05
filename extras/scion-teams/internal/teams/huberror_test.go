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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	forbiddenListAgentsBody = `{"error":{"code":"forbidden","message":"Insufficient permissions","details":{"denied_action":"list","resource_type":"agent"}}}`
	oboNotFoundBody         = `{"error":{"code":"forbidden","message":"on-behalf-of principal not found"}}`
	oboInactiveBody         = `{"error":{"code":"forbidden","message":"on-behalf-of principal is not active (status: suspended)"}}`
	messageDeniedBody       = `{"error":{"code":"message_denied","message":"Message delivery denied","details":{"reason":"not_allowed"}}}`
	senderInactiveBody      = `{"error":{"code":"forbidden","message":"sender identity is not active","details":{"status":"suspended"}}}`
)

func hubErr(status int, body string) error {
	return readHubError("test", &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))})
}

func TestUserFacingHubError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		project string
		want    string
		ok      bool
	}{
		{
			name:    "permission denied names account action and project",
			err:     hubErr(403, forbiddenListAgentsBody),
			project: "web-app",
			want:    "Your Scion account (alice@example.com) doesn't have permission to list agents in **web-app**. Ask a project owner.",
			ok:      true,
		},
		{
			name: "read action is shown as view",
			err:  hubErr(403, `{"error":{"code":"forbidden","message":"x","details":{"denied_action":"read","resource_type":"project"}}}`),
			want: "Your Scion account (alice@example.com) doesn't have permission to view projects in this hub. Ask a project owner.",
			ok:   true,
		},
		{
			name:    "message denied",
			err:     hubErr(403, messageDeniedBody),
			project: "web-app",
			want:    "Your Scion account (alice@example.com) doesn't have permission to message agents in **web-app**. Ask a project owner.",
			ok:      true,
		},
		{name: "linked user not found", err: hubErr(403, oboNotFoundBody), want: staleLinkText, ok: true},
		{name: "linked user not active", err: hubErr(403, oboInactiveBody), want: staleLinkText, ok: true},
		{name: "sender not active", err: hubErr(403, senderInactiveBody), want: staleLinkText, ok: true},
		{name: "wrapped error", err: fmt.Errorf("deliver: %w", hubErr(403, oboNotFoundBody)), want: staleLinkText, ok: true},
		{name: "forbidden without denied action", err: hubErr(403, `{"error":{"code":"forbidden","message":"nope"}}`)},
		{name: "non-403", err: hubErr(500, `{"error":{"code":"internal_error","message":"boom"}}`)},
		{name: "plain error", err: errors.New("network down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := userFacingHubError(tt.err, "alice@example.com", tt.project)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStaleLinkText_UsesTeamsCommandNames(t *testing.T) {
	assert.Contains(t, staleLinkText, "`unregister`")
	assert.Contains(t, staleLinkText, "`register`")
}

func TestHubClient_ReturnsHubErrorWithEnvelope(t *testing.T) {
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(forbiddenListAgentsBody))
	})
	_, err := broker.hubClient.ListAgents(context.Background(), "proj-1", "user:alice@example.com")
	var he *HubError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, 403, he.StatusCode)
	assert.Equal(t, "forbidden", he.Code)
	assert.Equal(t, "list", he.Details["denied_action"])
	assert.Contains(t, err.Error(), "list agents returned status 403")
}

func TestAgentsCommand_HubDenialShowsActionableText(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"permission denied", forbiddenListAgentsBody, "Your Scion account (user@example.com) doesn't have permission to list agents in **test-project**. Ask a project owner."},
		{"stale link", oboNotFoundBody, staleLinkText},
		{"inactive link", oboInactiveBody, staleLinkText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, ms := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(tt.body))
			})
			linkTestUser(t, broker)
			linkTestChannel(t, broker)

			handled, err := broker.commandHandler.Handle(context.Background(), testActivity("agents"))
			assert.True(t, handled)
			assert.NoError(t, err)
			require.Len(t, ms.sent, 1)
			assert.Equal(t, tt.want, ms.sent[0].Text)
		})
	}
}

func TestBroker_HandleMessage_HubDenialShowsActionableText(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"message denied", messageDeniedBody, "Your Scion account (user@example.com) doesn't have permission to message agents in **test-project**. Ask a project owner."},
		{"sender inactive", senderInactiveBody, staleLinkText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, ms := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(tt.body))
			})
			linkTestUser(t, broker)
			linkDefaultAgentChannel(t, broker)

			err := broker.handleMessage(context.Background(), testActivity("Please take a look"))
			require.Error(t, err)
			require.Len(t, ms.sent, 1)
			assert.Equal(t, tt.want, ms.sent[0].Text)
		})
	}
}

func TestCallbackHandler_AskResponse_HubDenialShowsActionableText(t *testing.T) {
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(senderInactiveBody))
	})
	linkTestUser(t, broker)
	require.NoError(t, broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-1",
		ConversationID: "conv-1",
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve"},
		ExpiresAt:      time.Now().Add(10 * time.Minute),
	}))

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action":     "ask_response",
		"request_id": "req-1",
		"choice":     "approve",
	}))
	require.NoError(t, err)
	body, err := json.Marshal(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "no longer active")
}
