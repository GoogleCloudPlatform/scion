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
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func invokeActivity(actionData map[string]string) *Activity {
	dataJSON, _ := json.Marshal(actionData)
	return &Activity{
		Type: "invoke",
		Name: "adaptiveCard/action",
		ID:   "invoke-1",
		From: ChannelAccount{
			ID:          "user-1",
			Name:        "Test User",
			AadObjectID: "aad-user-1",
		},
		Conversation: ConversationAccount{
			ID:               "conv-1",
			ConversationType: "channel",
		},
		Recipient: ChannelAccount{
			ID:   "test-bot-id",
			Name: "Scion",
		},
		ServiceURL: "https://smba.trafficmanager.net/test/",
		Value:      json.RawMessage(dataJSON),
	}
}

func TestCallbackHandler_AskResponse_Valid(t *testing.T) {
	// Set up a hub that accepts inbound delivery.
	var receivedBody []byte
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/broker/inbound" {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			receivedBody = buf[:n]
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	broker, _ := testBrokerWithStore(t, hubHandler)
	linkTestUser(t, broker)

	// Create a pending ask-user request.
	err := broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-1",
		ActivityID:     "act-ask-1",
		ConversationID: "conv-1",
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve", "reject"},
		ExpiresAt:      time.Now().Add(10 * time.Minute),
		Responded:      false,
	})
	require.NoError(t, err)

	activity := invokeActivity(map[string]string{
		"action":     "ask_response",
		"request_id": "req-1",
		"choice":     "approve",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)

	// Verify the ask-user was marked as responded.
	pending, err := broker.store.GetPendingAskUser(context.Background(), "req-1")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.True(t, pending.Responded)

	// Verify something was sent to the hub.
	assert.NotEmpty(t, receivedBody, "expected inbound delivery to hub")
}

func TestCallbackHandler_AskResponse_Expired(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

	// Create an expired ask-user request.
	err := broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-expired",
		ActivityID:     "act-ask-1",
		ConversationID: "conv-1",
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve", "reject"},
		ExpiresAt:      time.Now().Add(-5 * time.Minute), // already expired
		Responded:      false,
	})
	require.NoError(t, err)

	activity := invokeActivity(map[string]string{
		"action":     "ask_response",
		"request_id": "req-expired",
		"choice":     "approve",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)

	// Should NOT be marked as responded (was expired).
	pending, err := broker.store.GetPendingAskUser(context.Background(), "req-expired")
	require.NoError(t, err)
	assert.False(t, pending.Responded)
}

func TestCallbackHandler_AskResponse_UnknownRequestID(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

	activity := invokeActivity(map[string]string{
		"action":     "ask_response",
		"request_id": "nonexistent",
		"choice":     "approve",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)
}

func TestCallbackHandler_AskResponse_AlreadyResponded(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

	err := broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-responded",
		ConversationID: "conv-1",
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve"},
		ExpiresAt:      time.Now().Add(10 * time.Minute),
		Responded:      true, // already responded
	})
	require.NoError(t, err)

	activity := invokeActivity(map[string]string{
		"action":     "ask_response",
		"request_id": "req-responded",
		"choice":     "approve",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)
}

func TestCallbackHandler_AskInput(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

	err := broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-input",
		ConversationID: "conv-1",
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve"},
		ExpiresAt:      time.Now().Add(10 * time.Minute),
		Responded:      false,
	})
	require.NoError(t, err)

	activity := invokeActivity(map[string]string{
		"action":     "ask_input",
		"request_id": "req-input",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)

	// The response body should contain an Adaptive Card with an Input.Text field.
	bodyMap, ok := resp.Body.(map[string]interface{})
	require.True(t, ok, "expected response body to be a map")
	assert.Equal(t, "application/vnd.microsoft.card.adaptive", bodyMap["type"])

	// Parse the card value to verify Input.Text is present.
	cardRaw, ok := bodyMap["value"].(json.RawMessage)
	require.True(t, ok, "expected card value to be json.RawMessage")
	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(cardRaw, &card))

	body, ok := card["body"].([]interface{})
	require.True(t, ok, "expected card body to be an array")
	hasInputText := false
	for _, elem := range body {
		if m, ok := elem.(map[string]interface{}); ok {
			if m["type"] == "Input.Text" {
				hasInputText = true
				assert.Equal(t, "reply_text", m["id"])
				break
			}
		}
	}
	assert.True(t, hasInputText, "expected card body to contain an Input.Text element")
}

// userProjectsHub serves the given projects from the user-scoped project list.
func userProjectsHub(t *testing.T, projects ...hubProject) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" {
			t.Errorf("unexpected hub request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.Equal(t, "user:user@example.com", r.Header.Get("X-Scion-On-Behalf-Of"))
		out := projects
		if slug := r.URL.Query().Get("slug"); slug != "" {
			out = nil
			for _, p := range projects {
				if p.Slug == slug {
					out = append(out, p)
				}
			}
		}
		json.NewEncoder(w).Encode(hubProjectsResponse{Projects: out})
	}
}

func TestCallbackHandler_SetupConfirm(t *testing.T) {
	broker, _ := testBrokerWithStore(t, userProjectsHub(t, hubProject{ID: "proj-1", Name: "My Project", Slug: "my-project"}))
	linkTestUser(t, broker)

	activity := invokeActivity(map[string]string{
		"action":       "setup_confirm",
		"project_slug": "my-project",
		"project_id":   "proj-1",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)

	// Verify channel link was created.
	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, "my-project", link.ProjectSlug)
	assert.Equal(t, "proj-1", link.ProjectID)
	assert.True(t, link.Active)
}

func TestCallbackHandler_SetupConfirm_AlreadyLinked(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	linkTestUser(t, broker)

	// Pre-create a link.
	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-existing",
		ProjectSlug:    "existing",
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	activity := invokeActivity(map[string]string{
		"action":       "setup_confirm",
		"project_slug": "new-project",
		"project_id":   "proj-new",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)

	// Link should still be the existing one.
	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, "existing", link.ProjectSlug)
}

func TestCallbackHandler_UnknownAction(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

	activity := invokeActivity(map[string]string{
		"action": "unknown_action",
	})

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status) // should return 200 OK, no error
}

func TestCallbackHandler_NonAdaptiveCardInvoke(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

	activity := &Activity{
		Type: "invoke",
		Name: "other/invoke",
		ID:   "invoke-other",
		From: ChannelAccount{ID: "user-1"},
		Conversation: ConversationAccount{
			ID: "conv-1",
		},
	}

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)
}

func TestCallbackHandler_WrappedActionData(t *testing.T) {
	broker, _ := testBrokerWithStore(t, userProjectsHub(t, hubProject{ID: "proj-wrapped", Slug: "wrapped-project"}))
	linkTestUser(t, broker)

	// Test with wrapped action data format:
	// {"action": {"type": "Action.Execute", "data": {"action": "setup_confirm", ...}}}
	wrappedData := map[string]interface{}{
		"action": map[string]interface{}{
			"type": "Action.Execute",
			"data": map[string]interface{}{
				"action":       "setup_confirm",
				"project_slug": "wrapped-project",
				"project_id":   "proj-wrapped",
			},
		},
	}
	dataJSON, _ := json.Marshal(wrappedData)

	activity := &Activity{
		Type: "invoke",
		Name: "adaptiveCard/action",
		ID:   "invoke-wrapped",
		From: ChannelAccount{
			ID:          "user-1",
			Name:        "Test User",
			AadObjectID: "aad-user-1",
		},
		Conversation: ConversationAccount{
			ID:               "conv-1",
			ConversationType: "channel",
		},
		ServiceURL: "https://smba.trafficmanager.net/test/",
		Value:      json.RawMessage(dataJSON),
	}

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.Status)

	// Verify channel link was created from the wrapped data.
	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, "wrapped-project", link.ProjectSlug)
}

// askUserPayloadFor runs an ask_response through the callback handler and
// returns the inbound payload delivered to the hub.
func askUserPayloadFor(t *testing.T) inboundPayload {
	t.Helper()
	var payload inboundPayload
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		w.WriteHeader(http.StatusOK)
	})
	linkTestUser(t, broker)
	require.NoError(t, broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-1",
		ActivityID:     "act-ask-1",
		ConversationID: "conv-1",
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve", "reject"},
		ExpiresAt:      time.Now().Add(10 * time.Minute),
	}))

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action":     "ask_response",
		"request_id": "req-1",
		"choice":     "approve",
	}))
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, payload.Message, "expected inbound delivery to hub")
	return payload
}

func TestCallbackHandler_AskResponse_UsesCanonicalTopicAndSenderFields(t *testing.T) {
	payload := askUserPayloadFor(t)

	assert.Equal(t, "scion.project.proj-1.agent.dev-1.messages", payload.Topic)
	assert.Equal(t, "user:user@example.com", payload.Message.Sender)
	assert.Equal(t, "aad-user-1", payload.Message.SenderID)
	assert.Equal(t, "agent:dev-1", payload.Message.Recipient)
	assert.Equal(t, "approve", payload.Message.Msg)
}

func TestCallbackHandler_AskResponse_UnlinkedUserGetsRegisterHint(t *testing.T) {
	hubCalled := false
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		hubCalled = true
		w.WriteHeader(http.StatusOK)
	})
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
	assert.Contains(t, string(body), "`register`")
	assert.False(t, hubCalled, "hub should not be called for an unlinked user")

	pending, err := broker.store.GetPendingAskUser(context.Background(), "req-1")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.False(t, pending.Responded, "request stays open so it can be answered after registering")
}

func TestCallbackHandler_SetupConfirm_RequiresLinkedUser(t *testing.T) {
	broker, _ := testBrokerWithStore(t, userProjectsHub(t, hubProject{ID: "proj-1", Slug: "my-project"}))

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action":       "setup_confirm",
		"project_slug": "my-project",
		"project_id":   "proj-1",
	}))
	require.NoError(t, err)
	body, _ := json.Marshal(resp.Body)
	assert.Contains(t, string(body), "register")

	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	assert.Nil(t, link)
}

func TestCallbackHandler_SetupConfirm_RejectsProjectOutsideUserProjects(t *testing.T) {
	broker, _ := testBrokerWithStore(t, userProjectsHub(t, hubProject{ID: "proj-1", Slug: "my-project"}))
	linkTestUser(t, broker)

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action":       "setup_confirm",
		"project_slug": "other-project",
		"project_id":   "proj-other",
	}))
	require.NoError(t, err)
	body, _ := json.Marshal(resp.Body)
	assert.Contains(t, string(body), "not found among your Scion projects")

	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	assert.Nil(t, link)
}

func TestCallbackHandler_SetupConfirm_UnlinkedUserInLinkedChannelGetsRegisterHint(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	require.NoError(t, broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-existing",
		ProjectSlug:    "existing",
		LinkedAt:       time.Now(),
		Active:         true,
	}))

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action":       "setup_confirm",
		"project_slug": "new-project",
		"project_id":   "proj-new",
	}))
	require.NoError(t, err)
	body, _ := json.Marshal(resp.Body)
	assert.Contains(t, string(body), "`register`")
	assert.NotContains(t, string(body), "existing")
}

func TestCallbackHandler_AskResponse_RetryableFailuresKeepCard(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, broker *TeamsBroker)
		hub   http.HandlerFunc
		want  string
	}{
		{
			name: "unlinked user",
			hub:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
			want: registerHint,
		},
		{
			name: "link lookup error",
			setup: func(t *testing.T, broker *TeamsBroker) {
				broker.store = mappingErrorStore{Store: broker.store}
			},
			hub:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
			want: linkCheckFailedText,
		},
		{
			name:  "delivery failure",
			setup: linkTestUser,
			hub: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
			},
			want: "Failed to deliver your response. Please try again.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, _ := testBrokerWithStore(t, tt.hub)
			require.NoError(t, broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
				RequestID:      "req-1",
				ConversationID: "conv-1",
				AgentSlug:      "dev-1",
				ProjectID:      "proj-1",
				Choices:        []string{"approve"},
				ExpiresAt:      time.Now().Add(10 * time.Minute),
			}))
			if tt.setup != nil {
				tt.setup(t, broker)
			}

			resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
				"action":     "ask_response",
				"request_id": "req-1",
				"choice":     "approve",
			}))
			require.NoError(t, err)
			require.NotNil(t, resp)

			body, ok := resp.Body.(map[string]interface{})
			require.True(t, ok, "unexpected body %T", resp.Body)
			assert.Equal(t, "application/vnd.microsoft.activity.message", body["type"], "card must not be replaced")
			assert.NotEqual(t, "application/vnd.microsoft.card.adaptive", body["type"])
			assert.Equal(t, tt.want, body["value"])

			pending, err := broker.store.GetPendingAskUser(context.Background(), "req-1")
			require.NoError(t, err)
			require.NotNil(t, pending)
			assert.False(t, pending.Responded)
		})
	}
}

func TestCallbackHandler_AskResponse_SuccessReplacesCard(t *testing.T) {
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
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
	body, ok := resp.Body.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "application/vnd.microsoft.card.adaptive", body["type"])
}

// assertKeepsCard checks that resp shows want as a message and leaves the
// card in place.
func assertKeepsCard(t *testing.T, resp *InvokeResponse, want string) {
	t.Helper()
	require.NotNil(t, resp)
	body, ok := resp.Body.(map[string]interface{})
	require.True(t, ok, "unexpected body %T", resp.Body)
	assert.Equal(t, "application/vnd.microsoft.activity.message", body["type"], "card must not be replaced")
	assert.Equal(t, want, body["value"])
}

// assertReplacesCard checks that resp replaces the card.
func assertReplacesCard(t *testing.T, resp *InvokeResponse) {
	t.Helper()
	require.NotNil(t, resp)
	body, ok := resp.Body.(map[string]interface{})
	require.True(t, ok, "unexpected body %T", resp.Body)
	assert.Equal(t, "application/vnd.microsoft.card.adaptive", body["type"])
}

func TestCallbackHandler_SetupConfirm_RetryableFailuresKeepCard(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, broker *TeamsBroker)
		hub   http.HandlerFunc
		data  map[string]string
		want  string
	}{
		{
			name: "unlinked user",
			hub:  userProjectsHub(t, hubProject{ID: "proj-1", Slug: "my-project"}),
			want: registerHint,
		},
		{
			name: "link lookup error",
			setup: func(t *testing.T, broker *TeamsBroker) {
				broker.store = mappingErrorStore{Store: broker.store}
			},
			hub:  userProjectsHub(t, hubProject{ID: "proj-1", Slug: "my-project"}),
			want: linkCheckFailedText,
		},
		{
			name:  "project lookup failure",
			setup: linkTestUser,
			hub: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
			},
			want: "Failed to look up the project. Please try again.",
		},
		{
			name:  "project lookup denied without slug names the hub, not the ID",
			setup: linkTestUser,
			hub: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":{"code":"forbidden","message":"x","details":{"denied_action":"list","resource_type":"project"}}}`))
			},
			data: map[string]string{"action": "setup_confirm", "project_id": "0b6f9c1e-uuid"},
			want: "Your Scion account (user@example.com) doesn't have permission to list projects in this hub. Ask a project owner.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker, _ := testBrokerWithStore(t, tt.hub)
			if tt.setup != nil {
				tt.setup(t, broker)
			}
			data := tt.data
			if data == nil {
				data = map[string]string{"action": "setup_confirm", "project_slug": "my-project", "project_id": "proj-1"}
			}

			resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(data))
			require.NoError(t, err)
			assertKeepsCard(t, resp, tt.want)

			link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
			require.NoError(t, err)
			assert.Nil(t, link)
		})
	}
}

func TestCallbackHandler_SetupConfirm_FinalOutcomesReplaceCard(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		broker, _ := testBrokerWithStore(t, userProjectsHub(t, hubProject{ID: "proj-1", Slug: "my-project"}))
		linkTestUser(t, broker)
		resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
			"action": "setup_confirm", "project_slug": "my-project", "project_id": "proj-1",
		}))
		require.NoError(t, err)
		assertReplacesCard(t, resp)
	})
	t.Run("not among the user's projects", func(t *testing.T) {
		broker, _ := testBrokerWithStore(t, userProjectsHub(t, hubProject{ID: "proj-1", Slug: "my-project"}))
		linkTestUser(t, broker)
		resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
			"action": "setup_confirm", "project_slug": "other", "project_id": "proj-other",
		}))
		require.NoError(t, err)
		assertReplacesCard(t, resp)
	})
	t.Run("already linked", func(t *testing.T) {
		broker, _ := testBrokerWithStore(t, nil)
		linkTestUser(t, broker)
		linkTestChannel(t, broker)
		resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
			"action": "setup_confirm", "project_slug": "my-project", "project_id": "proj-1",
		}))
		require.NoError(t, err)
		assertReplacesCard(t, resp)
	})
}

// pendingAskErrorStore fails every pending ask-user lookup.
type pendingAskErrorStore struct {
	Store
}

func (pendingAskErrorStore) GetPendingAskUser(context.Context, string) (*PendingAskUser, error) {
	return nil, errors.New("database unavailable")
}

func TestCallbackHandler_AskInput_RetryableFailuresKeepCard(t *testing.T) {
	data := map[string]string{"action": "ask_input", "request_id": "req-1"}

	t.Run("store error", func(t *testing.T) {
		broker, _ := testBrokerWithStore(t, nil)
		broker.store = pendingAskErrorStore{Store: broker.store}
		resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(data))
		require.NoError(t, err)
		assertKeepsCard(t, resp, "An error occurred loading this request. Please try again.")
	})
	t.Run("store not initialized", func(t *testing.T) {
		broker, _ := testBrokerWithStore(t, nil)
		real := broker.store
		broker.store = nil
		t.Cleanup(func() { broker.store = real })
		resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(data))
		require.NoError(t, err)
		assertKeepsCard(t, resp, "Store not initialized.")
	})
	t.Run("unknown request replaces card", func(t *testing.T) {
		broker, _ := testBrokerWithStore(t, nil)
		resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(data))
		require.NoError(t, err)
		assertReplacesCard(t, resp)
	})
}

// pendingAsk stores an open ask-user request req-1 for dev-1 in proj-1,
// first posted to conversationID.
func pendingAsk(t *testing.T, store Store, conversationID string) {
	t.Helper()
	require.NoError(t, store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID:      "req-1",
		ConversationID: conversationID,
		AgentSlug:      "dev-1",
		ProjectID:      "proj-1",
		Choices:        []string{"approve", "reject"},
		ExpiresAt:      time.Now().Add(10 * time.Minute),
	}))
}

func askResponse(choice string) *Activity {
	return invokeActivity(map[string]string{"action": "ask_response", "request_id": "req-1", "choice": choice})
}

func TestCallbackHandler_AskResponse_RoutesToAnsweringConversation(t *testing.T) {
	var payload inboundPayload
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		w.WriteHeader(http.StatusOK)
	})
	linkTestUser(t, broker)
	pendingAsk(t, broker.store, "conv-first")

	activity := askResponse("approve")
	activity.Conversation.ID = "conv-1;messageid=123"
	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), activity)
	require.NoError(t, err)
	assertReplacesCard(t, resp)

	require.NotNil(t, payload.Message)
	assert.Equal(t, "conv-1", payload.Message.ThreadID)
	assert.Equal(t, "conv-1", payload.Message.Metadata["teams_conversation_id"])
}

func TestCallbackHandler_AskResponse_InvalidChoiceKeepsCard(t *testing.T) {
	tests := []struct {
		name     string
		activity *Activity
		want     string
	}{
		{"choice not offered", askResponse("delete-everything"), "That choice isn't available for this question. Please use one of the buttons."},
		{"empty custom reply", invokeActivity(map[string]string{"action": "ask_response", "request_id": "req-1", "choice": "custom", "reply_text": "  "}), "Please type a reply before sending."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hubCalled := false
			broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
				hubCalled = true
				w.WriteHeader(http.StatusOK)
			})
			linkTestUser(t, broker)
			pendingAsk(t, broker.store, "conv-1")

			resp, err := broker.callbackHandler.HandleInvoke(context.Background(), tt.activity)
			require.NoError(t, err)
			assertKeepsCard(t, resp, tt.want)
			assert.False(t, hubCalled)

			pending, err := broker.store.GetPendingAskUser(context.Background(), "req-1")
			require.NoError(t, err)
			assert.False(t, pending.Responded)
		})
	}
}

func TestCallbackHandler_AskResponse_CustomReplyIsDelivered(t *testing.T) {
	var payload inboundPayload
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		w.WriteHeader(http.StatusOK)
	})
	linkTestUser(t, broker)
	pendingAsk(t, broker.store, "conv-1")

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action": "ask_response", "request_id": "req-1", "choice": "custom", "reply_text": "ship it tomorrow",
	}))
	require.NoError(t, err)
	assertReplacesCard(t, resp)
	require.NotNil(t, payload.Message)
	assert.Equal(t, "ship it tomorrow", payload.Message.Msg)
}

func TestCallbackHandler_AskResponse_ConcurrentClicksDeliverOnce(t *testing.T) {
	var deliveries int32
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deliveries, 1)
		w.WriteHeader(http.StatusOK)
	})
	// Use a file-backed store so concurrent clicks share one database.
	fileStore, err := NewSQLiteStore(filepath.Join(t.TempDir(), "teams.db"))
	require.NoError(t, err)
	t.Cleanup(func() { fileStore.Close() })
	broker.store = fileStore
	linkTestUser(t, broker)
	pendingAsk(t, broker.store, "conv-1")

	const clicks = 8
	responses := make([]*InvokeResponse, clicks)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < clicks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := broker.callbackHandler.HandleInvoke(context.Background(), askResponse("approve"))
			assert.NoError(t, err)
			responses[i] = resp
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&deliveries), "exactly one click is delivered")
	answered, already := 0, 0
	for _, resp := range responses {
		raw, err := json.Marshal(resp.Body)
		require.NoError(t, err)
		switch {
		case strings.Contains(string(raw), "Responded:"):
			answered++
		case strings.Contains(string(raw), "already been responded to"):
			already++
			assertReplacesCard(t, resp)
		default:
			t.Errorf("unexpected response %s", raw)
		}
	}
	assert.Equal(t, 1, answered)
	assert.Equal(t, clicks-1, already)
}

func TestCallbackHandler_AskInput_SendReplyHasVerb(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	pendingAsk(t, broker.store, "conv-1")

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), invokeActivity(map[string]string{
		"action": "ask_input", "request_id": "req-1",
	}))
	require.NoError(t, err)
	raw, err := json.Marshal(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"type":"Action.Execute"`)
	assert.Contains(t, string(raw), `"verb":"ask_response"`)
}

func TestCallbackHandler_AskResponse_CancelledContextStillReopensRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		// The click's context is cancelled while the answer is being delivered.
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	})
	linkTestUser(t, broker)
	pendingAsk(t, broker.store, "conv-1")

	resp, err := broker.callbackHandler.HandleInvoke(ctx, askResponse("approve"))
	require.NoError(t, err)
	assertKeepsCard(t, resp, "Failed to deliver your response. Please try again.")

	pending, err := broker.store.GetPendingAskUser(context.Background(), "req-1")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.False(t, pending.Responded, "request is reopened so it can be answered again")
}

func TestCallbackHandler_ProjectSlugFor_RequiresMatchingProject(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	linkTestChannel(t, broker) // conv-1 -> proj-1 (test-project)
	ctx := context.Background()

	assert.Equal(t, "test-project", broker.callbackHandler.projectSlugFor(ctx, "conv-1;messageid=9", "proj-1"))
	assert.Equal(t, "", broker.callbackHandler.projectSlugFor(ctx, "conv-1", "proj-other"))
	assert.Equal(t, "", broker.callbackHandler.projectSlugFor(ctx, "conv-unlinked", "proj-1"))
}

func TestCallbackHandler_AskResponse_DenialInOtherProjectChannelNamesHub(t *testing.T) {
	broker, _ := testBrokerWithStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(messageDeniedBody))
	})
	linkTestUser(t, broker)
	linkTestChannel(t, broker) // conv-1 is linked to proj-1
	require.NoError(t, broker.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID: "req-1", ConversationID: "conv-1", AgentSlug: "dev-1", ProjectID: "proj-other",
		Choices: []string{"approve"}, ExpiresAt: time.Now().Add(10 * time.Minute),
	}))

	resp, err := broker.callbackHandler.HandleInvoke(context.Background(), askResponse("approve"))
	require.NoError(t, err)
	assertKeepsCard(t, resp, "Your Scion account (user@example.com) doesn't have permission to message agents in this hub. Ask a project owner.")
}
