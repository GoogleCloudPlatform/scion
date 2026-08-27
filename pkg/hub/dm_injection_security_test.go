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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nullSpokeEventBus is a no-op EventBus used as a non-inprocess spoke in the
// FanOutEventBus so that ListChannels() returns "web" without triggering
// handler panics. It accepts nil handlers and discards published messages.
type nullSpokeEventBus struct{}

func (nullSpokeEventBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	return nil
}
func (nullSpokeEventBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}
func (nullSpokeEventBus) Close() error { return nil }

type nullSub struct{}

func (nullSub) Unsubscribe() error { return nil }

// TestDMKeyIngress_UnauthorizedAgentCanInjectIntoForeignDM reproduces a
// security defect: message ingress validates DM key format but never checks
// that the sending agent is a participant named in the key. An agent in
// project P1 can write into a DM conversation between agent B (in project P2)
// and user V, and user V will see the injected message when reading that
// conversation's history.
//
// The test asserts the correct security invariant (the injection must be
// blocked). It is expected to FAIL on code that lacks the membership check,
// proving the defect exists.
func TestDMKeyIngress_UnauthorizedAgentCanInjectIntoForeignDM(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// ---------------------------------------------------------------
	// Identities
	// ---------------------------------------------------------------

	// User V (the victim) — a distinct user, not the dev user.
	victimUser := &store.User{
		ID:          tid("victim-user"),
		Email:       "victim@test.com",
		DisplayName: "Victim User",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, victimUser))
	victimUserID := victimUser.ID

	// Project P1 — the attacker's project.
	p1 := &store.Project{
		ID:         api.NewUUID(),
		Name:       "attacker-project",
		Slug:       "attacker-project",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateProject(ctx, p1))

	// Project P2 — the victim's project (agent B lives here).
	p2 := &store.Project{
		ID:         api.NewUUID(),
		Name:       "victim-project",
		Slug:       "victim-project",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateProject(ctx, p2))

	// Agent A — attacker, lives in P1.
	agentA := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "attacker-agent",
		Slug:       "attacker-agent",
		ProjectID:  p1.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	// Agent B — legitimate, lives in P2.
	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "legit-agent",
		Slug:       "legit-agent",
		ProjectID:  p2.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// The DM conversation between agent B and user V.
	dmKey := fmt.Sprintf("dm:agent:%s:user:%s", agentB.ID, victimUserID)

	// ---------------------------------------------------------------
	// Broker setup — "web" spoke is required for channel validation.
	// ---------------------------------------------------------------
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())

	events := NewChannelEventPublisher()
	defer events.Close()

	proxy := NewMessageBrokerProxy(fanout, s, events, func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)

	srv.SetMessageBrokerProxy(proxy)

	// Ensure user-message subscriptions exist for both projects so that
	// deliverToUser is called and messages are persisted.
	proxy.subscribeProjectUserMessages(p1.ID)
	proxy.subscribeProjectUserMessages(p2.ID)

	// ---------------------------------------------------------------
	// Helper: send an outbound message as the given agent.
	// ---------------------------------------------------------------
	sendOutbound := func(agent *store.Agent, projectID, msg, channel, threadID string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(OutboundMessageRequest{
			RecipientID: victimUserID,
			Recipient:   "user:Victim User",
			Msg:         msg,
			Channel:     channel,
			ThreadID:    threadID,
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/"+agent.ID+"/outbound-message",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(),
			&agentIdentityWrapper{&AgentTokenClaims{
				Claims:    jwt.Claims{Subject: agent.ID},
				ProjectID: projectID,
			}}))
		rr := httptest.NewRecorder()
		srv.handleAgentOutboundMessage(rr, req, agent.ID)
		return rr
	}

	// ---------------------------------------------------------------
	// Helper: fetch conversation history for victim user.
	// ---------------------------------------------------------------
	fetchHistory := func() chatHistoryResponse {
		t.Helper()
		rec := doRequestAsUser(t, srv, victimUser, http.MethodGet,
			"/api/v1/chat/conversations/"+dmKey+"/messages", nil)
		require.Equal(t, http.StatusOK, rec.Code, "ConversationHistory: %s", rec.Body.String())

		var resp chatHistoryResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		return resp
	}

	// ---------------------------------------------------------------
	// Step 1: Agent B sends a legitimate message into the B↔V DM.
	// This is the "floor" — if V cannot read this, the test is broken.
	// ---------------------------------------------------------------
	rr := sendOutbound(agentB, p2.ID, "legitimate message from B", "web", dmKey)
	require.Equal(t, http.StatusOK, rr.Code,
		"Floor message (agent B → V): expected 200, got %d: %s", rr.Code, rr.Body.String())

	// Poll until the legitimate message is visible (replaces time.Sleep).
	require.Eventually(t, func() bool {
		resp := fetchHistory()
		for _, msg := range resp.Messages {
			if msg.Msg == "legitimate message from B" {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond,
		"Floor check failed: user V cannot see agent B's legitimate "+
			"message in the B↔V DM — test infrastructure is broken")

	// ---------------------------------------------------------------
	// Step 2: Agent A (project P1) injects a message into the B↔V DM.
	// A is NOT a participant in this DM. On correct code the write must
	// be rejected; on the current defective code it succeeds.
	// ---------------------------------------------------------------
	rr = sendOutbound(agentA, p1.ID, "INJECTED by attacker agent A", "web", dmKey)
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"Agent A is not a participant in the B↔V DM; the write must be rejected")

	// No async settle needed: the injection was rejected synchronously
	// (StatusBadRequest), so no message was created or delivered.

	// ---------------------------------------------------------------
	// Step 3: User V reads the B↔V conversation history.
	// ---------------------------------------------------------------
	histResp := fetchHistory()

	// ---------------------------------------------------------------
	// Floor assertion: V must see B's legitimate message.
	// If this fails, the test setup is broken and proves nothing.
	// ---------------------------------------------------------------
	foundLegit := false
	for _, msg := range histResp.Messages {
		if msg.Msg == "legitimate message from B" {
			foundLegit = true
			break
		}
	}
	require.True(t, foundLegit,
		"Floor check failed: user V cannot see agent B's legitimate "+
			"message in the B↔V DM — test infrastructure is broken, not the code under test")

	// ---------------------------------------------------------------
	// Security invariant: V must NOT see agent A's injected message.
	//
	// Agent A is in a different project (P1) and is not a participant in
	// the dm:agent:<B>:user:<V> conversation. Correct code must reject
	// the write at ingress. If V can read A's message, the authorization
	// check is missing.
	// ---------------------------------------------------------------
	for _, msg := range histResp.Messages {
		if msg.Msg == "INJECTED by attacker agent A" {
			t.Error("SECURITY DEFECT: agent A (project P1) injected a message " +
				"into the B↔V DM (project P2) and user V can read it. " +
				"The ingress handler validates DM key format but does not check " +
				"that the authenticated agent is a named participant in the key.")
		}
	}

	// ---------------------------------------------------------------
	// Step 4 (control): Agent A sends to a DIFFERENT thread (its own
	// legitimate DM key with the victim). Verify the control message
	// does NOT appear in B↔V's DM history — this pins the visibility
	// mechanism to the DM key.
	// ---------------------------------------------------------------
	controlDMKey := fmt.Sprintf("dm:agent:%s:user:%s", agentA.ID, victimUserID)
	controlRR := sendOutbound(agentA, p1.ID, "control message from A in own DM", "web", controlDMKey)
	if controlRR.Code == http.StatusOK {
		// Poll until the control message is visible in its own DM,
		// confirming delivery has settled before checking B↔V isolation.
		require.Eventually(t, func() bool {
			rec := doRequestAsUser(t, srv, victimUser, http.MethodGet,
				"/api/v1/chat/conversations/"+controlDMKey+"/messages", nil)
			if rec.Code != http.StatusOK {
				return false
			}
			var resp chatHistoryResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				return false
			}
			for _, m := range resp.Messages {
				if m.Msg == "control message from A in own DM" {
					return true
				}
			}
			return false
		}, 5*time.Second, 100*time.Millisecond,
			"control message should be visible in agent A's own DM")
	}

	// Re-fetch the B↔V conversation and verify control message is absent.
	histResp = fetchHistory()
	for _, msg := range histResp.Messages {
		assert.NotEqual(t, "control message from A in own DM", msg.Msg,
			"Control message from agent A's own DM leaked into agent B's DM with user V — "+
				"visibility is not properly scoped to the DM key")
	}
}
