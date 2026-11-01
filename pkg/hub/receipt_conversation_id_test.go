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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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
	startReceiptBrokerProxy(t, srv, s, project.ID, eventbus.NewInProcessEventBus(slog.Default()))

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

// startReceiptBrokerProxy wires a started message broker proxy over bus
// into srv, so outbound user messages take the broker path.
func startReceiptBrokerProxy(t *testing.T, srv *Server, s store.Store, projectID string, bus eventbus.EventBus) *MessageBrokerProxy {
	t.Helper()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	t.Cleanup(func() { _ = bus.Close() })
	proxy := NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	srv.SetMessageBrokerProxy(proxy)
	proxy.Start()
	t.Cleanup(proxy.Stop)
	require.NoError(t, proxy.EnsureProjectSubscriptions(context.Background(), projectID))
	return proxy
}

// routingClassifications is the number of routing classifications the
// divergence board has recorded (compared, explicit and derived).
func routingClassifications() int64 {
	m := messaging.DivergenceMetrics
	return m.Total() + m.ExplicitRoutes() + m.DerivedRoutes()
}

// A broker-relayed plain DM is classified, and checked for consistency,
// exactly once, although the handler resolves its conversation for the
// receipt.
func TestSendReceipt_BrokerUserDMClassifiedOnce(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	startReceiptBrokerProxy(t, srv, s, project.ID, eventbus.NewInProcessEventBus(slog.Default()))

	classified, derived := routingClassifications(), messaging.DivergenceMetrics.DerivedRoutes()
	checks := messaging.DivergenceMetrics.ConsistencyChecks()
	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "classified once")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Eventually(t, func() bool {
		res, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: user.ID}, store.ListOptions{})
		return err == nil && len(res.Items) == 1
	}, 5*time.Second, 10*time.Millisecond)

	assert.Equal(t, classified+1, routingClassifications(), "one routing classification for the message")
	assert.Equal(t, derived+1, messaging.DivergenceMetrics.DerivedRoutes(), "classified as handler-derived")
	assert.Equal(t, checks+1, messaging.DivergenceMetrics.ConsistencyChecks(), "one DEF-3 consistency check")
}

// The resolve step the handler uses counts nothing; the persisting
// subscriber's wrapper counts a write-deny refusal once.
func TestResolveUserConversation_RefusalCountedOnce(t *testing.T) {
	_, s, project, agent, _ := def138Setup(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })
	proxy := NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	proxy.writeDenyEnabled = func() bool { return true }
	ctx := context.Background()
	msg := &messages.StructuredMessage{
		Sender: "agent:" + agent.Slug, SenderID: agent.ID,
		Recipient: "user:someone@example.com", RecipientID: "not-a-canonical-uuid",
		Msg: "refused", Type: messages.TypeInstruction,
	}

	before := messaging.WriteDenialMetrics.Get("mb.user.dm")
	res := proxy.resolveUserConversation(ctx, project.ID, msg)
	assert.Equal(t, "mb.user.dm", res.refusal)
	assert.True(t, res.nonCanonical)
	assert.Equal(t, before, messaging.WriteDenialMetrics.Get("mb.user.dm"), "the resolve step counts nothing")

	ok := proxy.resolveUserMessageConversation(ctx, project.ID, &store.Message{ID: tid("refused-row")}, msg)
	assert.False(t, ok, "the subscriber refuses to store")
	assert.Equal(t, before+1, messaging.WriteDenialMetrics.Get("mb.user.dm"), "one refusal, counted once")
}

// A send naming the reserved channel is refused before its conversation is
// resolved, so the 400 leaves no DM conversation behind.
func TestSendReceipt_ReservedChannelCreatesNoConversation(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())},
		{Name: "chatplugin", ChannelID: eventbus.InProcessBusName, Bus: nullSpokeEventBus{}},
	}, slog.Default())
	startReceiptBrokerProxy(t, srv, s, project.ID, fanout)

	body, err := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email, Msg: "reserved", Channel: eventbus.InProcessBusName,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: agent.ID}, ProjectID: project.ID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	// The reserved-channel rejection itself, not the earlier
	// unregistered-channel validation, which also answers 400.
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Contains(t, resp.Error.Message, "reserved for internal use")

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)
	_, err = s.GetConversationByExternalRef(context.Background(), "native", dmKey)
	assert.Error(t, err, "no DM conversation was created")
}
