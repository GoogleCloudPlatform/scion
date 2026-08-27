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

	// User V (the victim) is the dev user — doRequest authenticates as DevUserID.
	// The dev user is seeded automatically by testServer, so no CreateUser needed.
	victimUserID := DevUserID

	// Project P1 — the attacker's project.
	p1 := &store.Project{
		ID:         api.NewUUID(),
		Name:       "attacker-project",
		Slug:       "attacker-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, p1); err != nil {
		t.Fatalf("CreateProject P1: %v", err)
	}

	// Project P2 — the victim's project (agent B lives here).
	p2 := &store.Project{
		ID:         api.NewUUID(),
		Name:       "victim-project",
		Slug:       "victim-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, p2); err != nil {
		t.Fatalf("CreateProject P2: %v", err)
	}

	// Agent A — attacker, lives in P1.
	agentA := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "attacker-agent",
		Slug:       "attacker-agent",
		ProjectID:  p1.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agentA); err != nil {
		t.Fatalf("CreateAgent A: %v", err)
	}

	// Agent B — legitimate, lives in P2.
	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "legit-agent",
		Slug:       "legit-agent",
		ProjectID:  p2.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agentB); err != nil {
		t.Fatalf("CreateAgent B: %v", err)
	}

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
		if err != nil {
			t.Fatalf("marshal outbound request: %v", err)
		}
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
	// Step 1: Agent B sends a legitimate message into the B↔V DM.
	// This is the "floor" — if V cannot read this, the test is broken.
	// ---------------------------------------------------------------
	rr := sendOutbound(agentB, p2.ID, "legitimate message from B", "web", dmKey)
	if rr.Code != http.StatusOK {
		t.Fatalf("Floor message (agent B → V): expected 200, got %d: %s",
			rr.Code, rr.Body.String())
	}

	// Allow async broker delivery to persist the message.
	time.Sleep(300 * time.Millisecond)

	// ---------------------------------------------------------------
	// Step 2: Agent A (project P1) injects a message into the B↔V DM.
	// A is NOT a participant in this DM. On correct code the write must
	// be rejected; on the current defective code it succeeds.
	// ---------------------------------------------------------------
	rr = sendOutbound(agentA, p1.ID, "INJECTED by attacker agent A", "web", dmKey)

	// If the ingress correctly checks membership, we'd expect a 403 here.
	// On the defective code path, the write succeeds (200).
	// We proceed regardless and check the *effect* (whether V can read it).

	// Allow async broker delivery.
	time.Sleep(300 * time.Millisecond)

	// ---------------------------------------------------------------
	// Step 3: User V reads the B↔V conversation history.
	// doRequest authenticates as the dev user (DevUserID = victimUserID).
	// ---------------------------------------------------------------
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/"+dmKey+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ConversationHistory: expected 200, got %d: %s",
			rec.Code, rec.Body.String())
	}

	var histResp chatHistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&histResp); err != nil {
		t.Fatalf("decode conversation history: %v", err)
	}

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
	if !foundLegit {
		t.Fatal("Floor check failed: user V cannot see agent B's legitimate " +
			"message in the B↔V DM — test infrastructure is broken, not the code under test")
	}

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
}
