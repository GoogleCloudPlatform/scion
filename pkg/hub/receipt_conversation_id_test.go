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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
