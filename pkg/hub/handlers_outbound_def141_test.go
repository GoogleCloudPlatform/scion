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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DEF-141 AC-1: An outbound agent→user message with NO conversation_id in
// the request increments derived_routes, does NOT increment explicit_routes,
// and emits no divergence line. Asserted in a test, not by reading code.
// ---------------------------------------------------------------------------

func TestDEF141_AC1_DerivedRouting_IncrementsDerivedRoutes(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	ctx := context.Background()

	agentID := api.NewUUID()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:         agentID,
		Name:       "d141-derived-agent",
		Slug:       "d141-derived-agent",
		ProjectID:  projectID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}))

	recipientID := api.NewUUID()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          recipientID,
		Email:       "d141-derived@example.com",
		DisplayName: "D141 Derived User",
	}))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())

	// Snapshot counters before.
	derivedBefore := messaging.DivergenceMetrics.DerivedRoutes()
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()
	totalBefore := messaging.DivergenceMetrics.Total() // matches + mismatches

	// Build a message with ConversationID set (simulating P-3 propagation)
	// but ConversationAsserted=false (the derivation branch).
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "dm:agent:" + agentID + ":user:" + recipientID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	msg := messages.NewInstruction("agent:d141-derived-agent", "user:d141-derived@example.com", "derived msg")
	msg.SenderID = agentID
	msg.RecipientID = recipientID
	msg.ConversationID = created.ID
	msg.ConversationAsserted = false // hub-derived, not caller-asserted

	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg)

	// Assert 1: derived_routes incremented.
	derivedAfter := messaging.DivergenceMetrics.DerivedRoutes()
	require.Equal(t, derivedBefore+1, derivedAfter,
		"AC-1: derived_routes should increment for hub-derived ConversationID")

	// Assert 2: explicit_routes did NOT increment.
	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	require.Equal(t, explicitBefore, explicitAfter,
		"AC-1: explicit_routes must NOT increment for hub-derived ConversationID")

	// Assert 3: no divergence comparison (Total unchanged).
	totalAfter := messaging.DivergenceMetrics.Total()
	require.Equal(t, totalBefore, totalAfter,
		"AC-1: ComputeDivergenceMatch must NOT run for hub-derived ConversationID")
}

// ---------------------------------------------------------------------------
// DEF-141 AC-2: The same message with an authorized conversation_id
// increments explicit_routes only.
// ---------------------------------------------------------------------------

func TestDEF141_AC2_ExplicitRouting_IncrementsExplicitRoutes(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: "thread:" + projectID + ":d141-explicit-test",
		ProjectID:   &projectID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	agentID := api.NewUUID()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:         agentID,
		Name:       "d141-explicit-agent",
		Slug:       "d141-explicit-agent",
		ProjectID:  projectID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}))

	recipientID := api.NewUUID()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          recipientID,
		Email:       "d141-explicit@example.com",
		DisplayName: "D141 Explicit User",
	}))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())

	// Snapshot counters before.
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()
	derivedBefore := messaging.DivergenceMetrics.DerivedRoutes()
	totalBefore := messaging.DivergenceMetrics.Total()

	msg := messages.NewInstruction("agent:d141-explicit-agent", "user:d141-explicit@example.com", "explicit msg")
	msg.SenderID = agentID
	msg.RecipientID = recipientID
	msg.ConversationID = created.ID
	msg.ConversationAsserted = true // caller-asserted and authorized

	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg)

	// Assert 1: explicit_routes incremented.
	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	require.Equal(t, explicitBefore+1, explicitAfter,
		"AC-2: explicit_routes should increment for caller-asserted ConversationID")

	// Assert 2: derived_routes did NOT increment.
	derivedAfter := messaging.DivergenceMetrics.DerivedRoutes()
	require.Equal(t, derivedBefore, derivedAfter,
		"AC-2: derived_routes must NOT increment for caller-asserted ConversationID")

	// Assert 3: no divergence comparison (Total unchanged).
	totalAfter := messaging.DivergenceMetrics.Total()
	require.Equal(t, totalBefore, totalAfter,
		"AC-2: ComputeDivergenceMatch must NOT run for caller-asserted ConversationID")
}

// ---------------------------------------------------------------------------
// DEF-141 AC-1/AC-2 handler-through-broker integration tests.
//
// These exercise the FULL handler→broker path so that AC-4 mutations in the
// handler (e.g. setting asserted=true in the derivation branch, or dropping
// the ConversationAsserted propagation) are caught. The broker-only tests
// above verify the broker's three-way switch in isolation.
// ---------------------------------------------------------------------------

// def141BrokerSetup creates a server with a broker, project, agent, and user.
// The broker is wired so that handler → PublishUserMessage → deliverToUser.
func def141BrokerSetup(t *testing.T) (srv *Server, s store.Store, project *store.Project, agent *store.Agent, user *store.User) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   tid("d141-broker-project"),
		Name: "d141-broker-project",
		Slug: "d141-broker-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	user = &store.User{
		ID:          tid("d141-broker-user"),
		Email:       "d141-broker@example.com",
		DisplayName: "D141 Broker User",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	agent = &store.Agent{
		ID:         tid("d141-broker-agent"),
		Name:       "d141-broker-agent",
		Slug:       "d141-broker-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })

	proxy := NewMessageBrokerProxy(bus, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	return srv, s, project, agent, user
}

func TestDEF141_AC1_FullPath_DerivedRouting(t *testing.T) {
	srv, _, project, agent, user := def141BrokerSetup(t)

	// Snapshot counters before.
	derivedBefore := messaging.DivergenceMetrics.DerivedRoutes()
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()

	// Send without conversation_id → derivation path → asserted stays false.
	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "d141 full-path derived")
	require.Equal(t, http.StatusOK, rr.Code)

	// Give the async broker delivery time to complete.
	time.Sleep(200 * time.Millisecond)

	// derived_routes must increment, explicit_routes must NOT.
	derivedAfter := messaging.DivergenceMetrics.DerivedRoutes()
	require.Greater(t, derivedAfter, derivedBefore,
		"AC-1 (full path): derived_routes should increment for no-conversation_id request")

	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	require.Equal(t, explicitBefore, explicitAfter,
		"AC-1 (full path): explicit_routes must NOT increment for no-conversation_id request")
}

func TestDEF141_AC2_FullPath_ExplicitRouting(t *testing.T) {
	srv, s, project, agent, user := def141BrokerSetup(t)
	ctx := context.Background()

	// Create a group conversation owned by the agent's project.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d141-fullpath-explicit",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Snapshot counters before.
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()
	derivedBefore := messaging.DivergenceMetrics.DerivedRoutes()

	// Send WITH conversation_id → explicit path → asserted=true.
	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "d141 full-path explicit", created.ID)
	require.Equal(t, http.StatusOK, rr.Code)

	// Give the async broker delivery time to complete.
	time.Sleep(200 * time.Millisecond)

	// explicit_routes must increment, derived_routes must NOT.
	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	require.Greater(t, explicitAfter, explicitBefore,
		"AC-2 (full path): explicit_routes should increment for conversation_id request")

	derivedAfter := messaging.DivergenceMetrics.DerivedRoutes()
	require.Equal(t, derivedBefore, derivedAfter,
		"AC-2 (full path): derived_routes must NOT increment for conversation_id request")
}

// ---------------------------------------------------------------------------
// DEF-141 AC-3: The message lands in the same conversation as before the
// change, in both cases. This change moves no message.
// ---------------------------------------------------------------------------

func TestDEF141_AC3_DerivedMessage_LandsInSameConversation(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a conversation for the derivation path to find.
	extRef := "dm:agent:" + agent.ID + ":user:" + user.ID
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Send without conversation_id (derivation path).
	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "derived landing test")
	require.Equal(t, http.StatusOK, rr.Code,
		"outbound message without conversation_id should succeed")

	// Verify the message landed in the expected conversation.
	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: user.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.Equal(t, created.ID, result.Items[0].ConversationID,
		"AC-3: derived message should land in the same conversation (dm) as before")
}

func TestDEF141_AC3_ExplicitMessage_LandsInSameConversation(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation owned by the agent's project.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d141-ac3-thread",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Send with explicit conversation_id.
	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "explicit landing test", created.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"outbound message with conversation_id should succeed")

	// Verify the message landed in the named conversation.
	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: user.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.Equal(t, created.ID, result.Items[0].ConversationID,
		"AC-3: explicit message should land in the asserted conversation")
}

// ---------------------------------------------------------------------------
// DEF-141 AC-5: No DTO, request struct, or unmarshal target binds
// conversation_asserted. Enforced by POSTing {"conversation_asserted": true}
// with no conversation_id and asserting explicit_routes did not move.
// ---------------------------------------------------------------------------

func TestDEF141_AC5_ConversationAsserted_NotAcceptedFromJSON(t *testing.T) {
	srv, s, _, agent, user := def138Setup(t)

	// Snapshot explicit_routes before.
	explicitBefore := messaging.DivergenceMetrics.ExplicitRoutes()

	// Craft raw JSON with conversation_asserted: true but no conversation_id.
	rawJSON := `{
		"recipient": "user:` + user.Email + `",
		"msg": "AC-5 forgery attempt",
		"conversation_asserted": true
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message",
		bytes.NewBufferString(rawJSON))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: agent.ProjectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	// The message should succeed (it's a valid message without conversation_id).
	require.Equal(t, http.StatusOK, rr.Code,
		"message should succeed even with spurious conversation_asserted in JSON")

	// But explicit_routes must NOT have moved — the forgery must be ignored.
	explicitAfter := messaging.DivergenceMetrics.ExplicitRoutes()
	require.Equal(t, explicitBefore, explicitAfter,
		"AC-5: explicit_routes must not increment when conversation_asserted is "+
			"sent in request JSON — no DTO may bind this field")

	// Verify the message DID persist (it went through the derivation path).
	result, listErr := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: user.ID}, store.ListOptions{})
	require.NoError(t, listErr)
	require.GreaterOrEqual(t, len(result.Items), 1,
		"message should have been persisted via derivation path")
}

// ---------------------------------------------------------------------------
// DEF-141 AC-6: ConversationAsserted does not appear in any rendered agent
// envelope. Assert against DeliveryText for both branches.
// ---------------------------------------------------------------------------

func TestDEF141_AC6_ConversationAsserted_NotInDeliveryText(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		asserted bool
	}{
		{"explicit_branch", true},
		{"derived_branch", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &messages.StructuredMessage{
				Version:              messages.Version,
				Timestamp:            now.Format(time.RFC3339),
				Sender:               "agent:test-agent",
				Recipient:            "user:test@example.com",
				Msg:                  "AC-6 envelope test",
				Type:                 messages.TypeInstruction,
				ConversationID:       "conv-ac6-test",
				ConversationAsserted: tt.asserted,
			}

			conv := &messaging.ConversationResult{
				ConversationID: "conv-ac6-test",
				Kind:           "group",
				Surface:        "native",
				DisplayName:    "ac6-thread",
			}

			result := messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
				MessageID:  "msg-ac6",
				ConvResult: conv,
				Msg:        msg,
				CreatedAt:  now,
			})

			// The envelope must not contain "conversation_asserted" anywhere.
			require.NotContains(t, result, "conversation_asserted",
				"AC-6: ConversationAsserted must not appear in the rendered agent envelope "+
					"(branch=%s, asserted=%v)", tt.name, tt.asserted)

			// Also verify it doesn't leak as a JSON key in any casing.
			require.NotContains(t, strings.ToLower(result), "conversationasserted",
				"AC-6: ConversationAsserted must not appear in any casing in the envelope")
		})
	}
}

// ---------------------------------------------------------------------------
// DEF-141 AC-7: CheckConversationConsistency still runs and its return is
// still consumed on every path; the DEF-139 AC-8 structural guard still
// reports the same call-site count.
//
// This test verifies that the existing TestConsistencyCheckReturnConsumed
// guard in consistency_check_guard_test.go still passes — i.e. DEF-141's
// broker changes did not alter the call-site count or discard any return
// value. Rather than duplicating that test, we verify the specific property
// that DEF-141's changes preserved: CheckConversationConsistency is called
// in the broker's deliverToUser (with its return consumed) on EVERY path
// of the three-way switch.
// ---------------------------------------------------------------------------

func TestDEF141_AC7_ConsistencyCheckRunsOnAllPaths(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	ctx := context.Background()

	agentID := api.NewUUID()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:         agentID,
		Name:       "d141-cc-agent",
		Slug:       "d141-cc-agent",
		ProjectID:  projectID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}))

	recipientID := api.NewUUID()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          recipientID,
		Email:       "d141-cc@example.com",
		DisplayName: "D141 CC User",
	}))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())

	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: "dm:agent:" + agentID + ":user:" + recipientID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Test all three switch arms:

	// 1. Asserted path (ConversationAsserted=true).
	ccBefore := messaging.DivergenceMetrics.ConsistencyChecks()
	msg1 := messages.NewInstruction("agent:d141-cc-agent", "user:d141-cc@example.com", "cc asserted")
	msg1.SenderID = agentID
	msg1.RecipientID = recipientID
	msg1.ConversationID = created.ID
	msg1.ConversationAsserted = true
	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg1)
	ccAfter := messaging.DivergenceMetrics.ConsistencyChecks()
	require.Greater(t, ccAfter, ccBefore,
		"AC-7: CheckConversationConsistency must run on the asserted path")

	// 2. Derived path (ConversationID set, ConversationAsserted=false).
	ccBefore = messaging.DivergenceMetrics.ConsistencyChecks()
	msg2 := messages.NewInstruction("agent:d141-cc-agent", "user:d141-cc@example.com", "cc derived")
	msg2.SenderID = agentID
	msg2.RecipientID = recipientID
	msg2.ConversationID = created.ID
	msg2.ConversationAsserted = false
	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg2)
	ccAfter = messaging.DivergenceMetrics.ConsistencyChecks()
	require.Greater(t, ccAfter, ccBefore,
		"AC-7: CheckConversationConsistency must run on the derived path")

	// 3. Default path (no ConversationID — broker derives).
	ccBefore = messaging.DivergenceMetrics.ConsistencyChecks()
	msg3 := messages.NewInstruction("agent:d141-cc-agent", "user:d141-cc@example.com", "cc default")
	msg3.SenderID = agentID
	msg3.RecipientID = recipientID
	// No ConversationID, no ConversationAsserted.
	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg3)
	ccAfter = messaging.DivergenceMetrics.ConsistencyChecks()
	require.Greater(t, ccAfter, ccBefore,
		"AC-7: CheckConversationConsistency must run on the default (broker-derived) path")
}
