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
	"net/http"
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

func TestCallbackHandler_SetupConfirm(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)

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
	broker, _ := testBrokerWithStore(t, nil)

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
