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
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A send receipt names the conversation the message was persisted in, on
// both agent-to-agent adapters, so the sender can reply into it.
func TestSendReceipt_CarriesConversationID(t *testing.T) {
	srv, s, _, sender, target, convID, _, _ := paritySetup(t)
	ctx := context.Background()

	for name, rr := range map[string]*httptest.ResponseRecorder{
		"outbound":   sendViaOutbound(t, srv, sender, convID, "receipt-outbound"),
		"structured": sendViaStructured(t, srv, sender, target, "receipt-structured"),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var resp struct {
				MessageID      string `json:"message_id"`
				ConversationID string `json:"conversation_id"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.MessageID)
			stored, err := s.GetMessage(ctx, resp.MessageID)
			require.NoError(t, err)
			require.NotEmpty(t, stored.ConversationID)
			assert.Equal(t, stored.ConversationID, resp.ConversationID,
				"the receipt names the conversation the message was persisted in")
		})
	}
}

// A deferred send (the recipient is mid-reincarnation) still names the
// conversation the stored message landed in.
func TestSendReceipt_DeferredCarriesConversationID(t *testing.T) {
	srv, s := testServer(t)
	_, primaryID, _ := setupR1TestAgents(t, s)
	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:r1-primary",
			Msg: "deferred receipt", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+primaryID+"/message", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "deferred", resp.Status)
	require.NotEmpty(t, resp.MessageID)
	stored, err := s.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.ConversationID)
	assert.Equal(t, stored.ConversationID, resp.ConversationID)
}

// A plain agent-to-user DM relayed through the message broker (no
// conversation named) gets a receipt naming the conversation the
// persisting subscriber stores the message in.
func TestSendReceipt_BrokerUserDMNamesStoredConversation(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	proxy := NewMessageBrokerProxy(inner, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	srv.SetMessageBrokerProxy(proxy)
	proxy.Start()
	t.Cleanup(proxy.Stop)
	require.NoError(t, proxy.EnsureProjectSubscriptions(context.Background(), project.ID))

	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "plain dm to a user")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var receipt struct {
		ConversationID string `json:"conversation_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &receipt))

	var rows []store.Message
	require.Eventually(t, func() bool {
		res, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: user.ID}, store.ListOptions{})
		if err != nil {
			return false
		}
		rows = res.Items
		return len(rows) > 0
	}, 5*time.Second, 10*time.Millisecond)
	require.Len(t, rows, 1)
	require.NotEmpty(t, rows[0].ConversationID, "the subscriber stores the DM in a conversation")
	assert.Equal(t, rows[0].ConversationID, receipt.ConversationID)
}
