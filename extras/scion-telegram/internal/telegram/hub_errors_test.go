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

package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func forbiddenListAgents() *HubError {
	return &HubError{
		Op:         "list agents",
		StatusCode: http.StatusForbidden,
		Code:       "forbidden",
		Message:    "You don't have permission to perform this action",
		Details:    map[string]interface{}{"denied_action": "list", "resource_type": "agent"},
	}
}

func staleLinkError(message string) *HubError {
	return &HubError{Op: "list agents", StatusCode: http.StatusForbidden, Code: "forbidden", Message: message}
}

const permissionText = "Your Scion account (alice@example.com) doesn't have permission to list agents in my-project. Ask a project owner."

func TestHubErrorText(t *testing.T) {
	const fallback = "fallback"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"denied action names account, action and project", forbiddenListAgents(), permissionText},
		{"wrapped denial is recognised", fmt.Errorf("refresh: %w", forbiddenListAgents()), permissionText},
		{"unknown linked user asks to re-register", staleLinkError("on-behalf-of principal not found"), staleLinkText},
		{"inactive linked user asks to re-register", staleLinkError("on-behalf-of principal is not active (status: suspended)"), staleLinkText},
		{"forbidden without denied action uses fallback", &HubError{StatusCode: 403, Code: "forbidden"}, fallback},
		{"server error uses fallback", &HubError{StatusCode: 500, Code: "internal_error"}, fallback},
		{"transport error uses fallback", errors.New("connection refused"), fallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hubErrorText(tt.err, "alice@example.com", "my-project", fallback))
		})
	}
}

func TestHubErrorText_MissingProjectAndEmail(t *testing.T) {
	assert.Equal(t,
		"Your Scion account doesn't have permission to create agents in this project. Ask a project owner.",
		hubErrorText(&HubError{StatusCode: 403, Code: "forbidden", Details: map[string]interface{}{"denied_action": "create", "resource_type": "agent"}}, "", "", "x"))
}

func TestDeniedActionPhrase(t *testing.T) {
	assert.Equal(t, "list agents", deniedActionPhrase("list", "agent"))
	assert.Equal(t, "read policies", deniedActionPhrase("read", "policy"))
	assert.Equal(t, "update projects", deniedActionPhrase("update", "projects"))
	assert.Equal(t, "manage", deniedActionPhrase("manage", ""))
	assert.Equal(t, "", deniedActionPhrase("", "agent"))
}

func TestHTTPHubClient_DecodesHubErrorEnvelope(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"denied","details":{"denied_action":"list","resource_type":"agent"}}}`))
	}))
	defer hub.Close()

	_, err := NewHTTPHubClient(hub.URL, "", "", nil).ListAgents(context.Background(), "p1", "user:alice@example.com")
	var he *HubError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusForbidden, he.StatusCode)
	assert.Equal(t, "forbidden", he.Code)
	assert.Equal(t, "list", he.detail("denied_action"))
	assert.Equal(t, "agent", he.detail("resource_type"))
	assert.Contains(t, err.Error(), "list agents returned status 403")
}

func TestHTTPHubClient_NonJSONErrorBody(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer hub.Close()

	_, err := NewHTTPHubClient(hub.URL, "", "", nil).ListProjectsForUser(context.Background(), "user:alice@example.com")
	var he *HubError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusBadGateway, he.StatusCode)
	assert.Empty(t, he.Code)
}

// --- handler text ---

func TestCommandHandler_Agents_DeniedShowsPermissionText(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	saveTestGroupLink(t, store, -100, "proj-1", "my-project", "")
	linkTestUser(t, store, 42, "alice@example.com")
	hub.listAgentsErr = forbiddenListAgents()

	h.HandleCommand(&TGMessage{Text: "/agents", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, permissionText, sent[0].Text)
}

func TestCommandHandler_Agents_StaleLinkShowsReregisterText(t *testing.T) {
	for _, msg := range []string{"on-behalf-of principal not found", "on-behalf-of principal is not active (status: suspended)"} {
		t.Run(msg, func(t *testing.T) {
			h, tgSrv, hub, store := newTestCommandHandler(t)
			saveTestGroupLink(t, store, -100, "proj-1", "my-project", "")
			linkTestUser(t, store, 42, "alice@example.com")
			hub.listAgentsErr = staleLinkError(msg)

			h.HandleCommand(&TGMessage{Text: "/agents", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

			sent := tgSrv.getSentMessages()
			require.Len(t, sent, 1)
			assert.Equal(t, staleLinkText, sent[0].Text)
		})
	}
}

func TestCommandHandler_Agents_LinkWithoutEmailShowsReregisterText(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	saveTestGroupLink(t, store, -100, "proj-1", "my-project", "")
	require.NoError(t, store.SaveUserMapping(context.Background(), &TelegramUserMapping{
		TelegramUserID: "42", ScionUserID: "u-42", LinkedAt: time.Now().UTC(),
	}))

	h.HandleCommand(&TGMessage{Text: "/agents", Chat: TGChat{ID: -100, Type: "group"}, From: &TGUser{ID: 42}})

	assert.Empty(t, hub.agentCalls())
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, staleLinkText, sent[0].Text)
}

func TestCommandHandler_Notifications_StaleLinkShowsReregisterText(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	saveTestGroupLink(t, store, -100, "proj-1", "my-project", "")
	linkTestUser(t, store, 42, "alice@example.com")
	hub.listUserProjectsErr = staleLinkError("on-behalf-of principal not found")

	h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, staleLinkText, sent[0].Text)
}

func TestCommandHandler_Notifications_DeniedProjectNotShownFromCache(t *testing.T) {
	h, tgSrv, hub, store := newTestCommandHandler(t)
	saveTestGroupLink(t, store, -100, "proj-1", "my-project", "")
	saveStaleAgentCache(t, store, "user:alice@example.com", "proj-1", "coder")
	linkTestUser(t, store, 42, "alice@example.com")
	hub.projects = []ProjectOption{{ID: "proj-1", Slug: "my-project"}}
	hub.listAgentsErr = forbiddenListAgents()

	h.HandleCommand(&TGMessage{Text: "/notifications", Chat: TGChat{ID: 42, Type: "private"}, From: &TGUser{ID: 42}})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "No agents found")
}

func TestCallbackHandler_SetupProject_DeniedShowsPermissionText(t *testing.T) {
	h, tgSrv, hub, store := newTestCallbackHandler(t)
	h.SetProjects([]ProjectOption{{ID: "p1", Slug: "my-project"}})
	linkTestUser(t, store, 42, "alice@example.com")
	hub.listAgentsErr = forbiddenListAgents()

	_, _ = h.HandleCallback(context.Background(), setupCallback("setup:proj:p1", 42))

	answered := tgSrv.getAnsweredCallbacks()
	require.Len(t, answered, 1)
	assert.Equal(t, permissionText, answered[0].Text)
}

func TestV2_GroupMessage_DeniedListShowsPermissionText(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = forbiddenListAgents()
	linkTestUser(t, b.store, 456, "alice@example.com")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, permissionText, sent[0].Text)
}

func TestV2_GroupMessage_StaleLinkShowsReregisterText(t *testing.T) {
	b, tgSrv, hub := newRoutingTestBroker(t)
	hub.listAgentsErr = staleLinkError("on-behalf-of principal is not active (status: suspended)")
	linkTestUser(t, b.store, 456, "alice@example.com")

	b.handleGroupMessage(plainGroupMessage(456, "hello"))

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, staleLinkText, sent[0].Text)
}
