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
	"sort"
	"strconv"
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

// postOutbound sends one agent→human message as the given agent.
func postOutbound(t *testing.T, srv *Server, projectID, agentID, msg string) *httptest.ResponseRecorder {
	t.Helper()
	return postOutboundTyped(t, srv, projectID, agentID, msg, "")
}

// postOutboundTyped sends one agent→human message of a specific message type.
func postOutboundTyped(t *testing.T, srv *Server, projectID, agentID, msg, msgType string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:human@example.com",
		Msg:       msg,
		Type:      msgType,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// An agent stuck in a loop is cut off with an explicit, retryable 429 — the
// flood vector issue #1054 is actually about. The limit is per sender, so a
// second agent going about its business is untouched.
func TestOutboundMessage_RateLimitsFloodingAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "flood-project",
		Slug: "flood-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:          api.NewUUID(),
		Email:       "human@example.com",
		DisplayName: "Human",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	newAgent := func(name string) string {
		a := &store.Agent{
			ID:         api.NewUUID(),
			Name:       name,
			Slug:       name,
			ProjectID:  project.ID,
			Phase:      "running",
			Visibility: store.VisibilityPrivate,
		}
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent: %v", err)
		}
		return a.ID
	}
	flooder := newAgent("flooder")
	bystander := newAgent("bystander")

	// Production limits, test clock: the real 60/min ceiling without a real
	// minute of waiting.
	clock := newTestClock()
	srv.chatSendLimiter = newChatSendLimiterWithClock(clock.Now)

	for i := range chatSendAgentRatePerMinute {
		if rr := postOutbound(t, srv, project.ID, flooder, "spam"); rr.Code != http.StatusOK {
			t.Fatalf("send %d: expected 200, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}

	rr := postOutbound(t, srv, project.ID, flooder, "one too many")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("send %d: expected 429, got %d: %s",
			chatSendAgentRatePerMinute+1, rr.Code, rr.Body.String())
	}
	retryAfter := rr.Header().Get("Retry-After")
	if secs, err := strconv.Atoi(retryAfter); err != nil || secs < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds so the agent can back off", retryAfter)
	}
	if !strings.Contains(rr.Body.String(), ErrCodeRateLimited) {
		t.Errorf("expected a %q error code in the body, got %s", ErrCodeRateLimited, rr.Body.String())
	}
	// No current client reads Retry-After, so the delay in the message text is
	// what a sending agent actually sees. Assert it as well as the header.
	if want := "retry in " + retryAfter + "s"; !strings.Contains(rr.Body.String(), want) {
		t.Errorf("expected the body to carry the retry delay %q, got %s", want, rr.Body.String())
	}

	if rr := postOutbound(t, srv, project.ID, bystander, "unrelated report"); rr.Code != http.StatusOK {
		t.Errorf("a second agent must not be throttled by the flooder: got %d: %s", rr.Code, rr.Body.String())
	}

	// The refusal is transient: at 60/min a token accrues every second.
	clock.Advance(time.Second)
	if rr := postOutbound(t, srv, project.ID, flooder, "after backoff"); rr.Code != http.StatusOK {
		t.Errorf("expected the send to succeed after backing off, got %d: %s", rr.Code, rr.Body.String())
	}
}

// An unrecognised message type is charged as ordinary agent traffic: the
// class must come from the closed enum, not from whatever the caller puts in
// the body, so an unfamiliar label cannot buy the cheaper mirror reservation.
// After the S3 validation audit (M2), outbound messages go through
// ValidateLegacyMessage which rejects unknown message types.
func TestOutboundMessage_UnknownTypeIsChargedAsAgentTraffic(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "type-project",
		Slug: "type-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:          api.NewUUID(),
		Email:       "human@example.com",
		DisplayName: "Human",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "mislabeller",
		Slug:       "mislabeller",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	clock := newTestClock()
	srv.chatSendLimiter = newChatSendLimiterWithClock(clock.Now)

	rr := postOutboundTyped(t, srv, project.ID, agent.ID, "mislabelled", "not-a-real-type")
	// After the S3 validation audit (M2), outbound messages go through
	// ValidateLegacyMessage which rejects unknown message types.
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected unknown message type to be rejected with 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// The automatic assistant-reply transcript mirror shares the agent's single
// aggregate allowance with the messages the agent writes itself, but it may
// only spend its own reservation of it: a chatty agent whose mirror is
// flooding can still deliver a completion report or a blocker escalation. Low
// value traffic must not starve high-value traffic.
//
// The mirror is driven well past the aggregate ceiling here, not merely up to
// its reservation — otherwise the test would pass even with no reservation at
// all and would prove nothing about starvation.
func TestOutboundMessage_TranscriptMirrorDoesNotStarveAgentMessages(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "mirror-project",
		Slug: "mirror-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:          api.NewUUID(),
		Email:       "human@example.com",
		DisplayName: "Human",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "chatty",
		Slug:       "chatty",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	clock := newTestClock()
	srv.chatSendLimiter = newChatSendLimiterWithClock(clock.Now)

	// Flood with hook-posted assistant replies, twice the agent's whole
	// aggregate allowance. Only the mirror's reservation may get through.
	accepted := 0
	for range 2 * chatSendAgentRatePerMinute {
		rr := postOutboundTyped(t, srv, project.ID, agent.ID, "mirrored transcript", messages.TypeAssistantReply)
		switch rr.Code {
		case http.StatusOK:
			accepted++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("mirror send: expected 200 or 429, got %d: %s", rr.Code, rr.Body.String())
		}
	}
	if accepted != chatSendAgentMirrorRatePerMinute {
		t.Fatalf("the flooding mirror got %d sends through, want exactly its reservation of %d",
			accepted, chatSendAgentMirrorRatePerMinute)
	}

	// The agent's own message to a human is unaffected.
	if rr := postOutbound(t, srv, project.ID, agent.ID, "task complete"); rr.Code != http.StatusOK {
		t.Fatalf("the agent's own message must not be starved by its transcript mirror: got %d: %s",
			rr.Code, rr.Body.String())
	}
}

// B5 SECURITY: A client sending a structured_message with a spoofed SenderID
// must not be able to create (or join) a DM conversation under the spoofed
// identity. The DM key IS the access control list; if an attacker can choose
// the sender ID in the key, they can read/write any user's DM.
//
// This test sends a message with SenderID set to a different user (the
// "victim") while the authenticated identity is the attacker. Without the
// fix, the dual-write path builds a DM key from the spoofed SenderID and
// creates a conversation that the victim would join on their next message.
// With the fix, the key is derived from the authenticated caller, so the
// conversation belongs to the attacker (correct, expected behaviour).
func TestAgentMessage_B5_SpoofedSenderDoesNotDeriveConversationKey(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "b5-security-project",
		Slug: "b5-security-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// The attacker: will authenticate as this user.
	attacker := &store.User{
		ID:          api.NewUUID(),
		Email:       "attacker@example.com",
		DisplayName: "Attacker",
	}
	if err := s.CreateUser(ctx, attacker); err != nil {
		t.Fatalf("CreateUser (attacker): %v", err)
	}

	// The victim: attacker will try to spoof this user's ID as SenderID.
	victim := &store.User{
		ID:          api.NewUUID(),
		Email:       "victim@example.com",
		DisplayName: "Victim",
	}
	if err := s.CreateUser(ctx, victim); err != nil {
		t.Fatalf("CreateUser (victim): %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "target-agent",
		Slug:       "target-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Build the request with spoofed sender: SenderID and Sender claim to be
	// the victim, but the authenticated identity is the attacker.
	spoofedMsg := &messages.StructuredMessage{
		Sender:    "user:" + victim.Email,
		SenderID:  victim.ID,
		Recipient: "agent:" + agent.Slug,
		Msg:       "spoofed message",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(MessageRequest{StructuredMessage: spoofedMsg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Authenticate as the attacker.
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(attacker.ID, attacker.Email, attacker.DisplayName, "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, req, agent.ID)

	// The handler may return 503 (no dispatcher) — that's fine, the
	// dual-write (conversation creation) happens before delivery.
	t.Logf("handler response: %d %s", rr.Code, rr.Body.String())

	// Build the expected keys.
	correctKey, err := messages.DMConversationKey("user", attacker.ID, "agent", agent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey (correct): %v", err)
	}
	spoofedKey, err := messages.DMConversationKey("user", victim.ID, "agent", agent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey (spoofed): %v", err)
	}
	t.Logf("correct key (attacker): %s", correctKey)
	t.Logf("spoofed key (victim):   %s", spoofedKey)

	// The conversation must be keyed to the attacker (the authenticated user),
	// NOT to the victim (the spoofed sender).
	correctConv, err := s.GetConversationByExternalRef(ctx, "native", correctKey)
	if err != nil {
		t.Fatalf("expected conversation with correct key (attacker) to exist, got: %v", err)
	}
	t.Logf("conversation created: id=%s external_ref=%s", correctConv.ID, correctConv.ExternalRef)

	// The spoofed key must NOT have produced a conversation.
	spoofedConv, spoofedErr := s.GetConversationByExternalRef(ctx, "native", spoofedKey)
	if spoofedErr == nil && spoofedConv != nil {
		t.Errorf("SECURITY VIOLATION: conversation created under spoofed victim key %s (conv_id=%s). "+
			"The DM key must be derived from the authenticated context, never the payload.",
			spoofedKey, spoofedConv.ID)
	}

	// Also verify the stored message uses the attacker's sender identity, not
	// the victim's. This ensures downstream consumers (broker, SSE) inherit
	// the authenticated identity.
	msgResult, err := s.ListMessages(ctx, store.MessageFilter{
		ConversationID: correctConv.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgResult.Items {
		if m.SenderID == victim.ID {
			t.Errorf("stored message %s has SenderID=%s (victim); should be %s (attacker)",
				m.ID, victim.ID, attacker.ID)
		}
		if strings.Contains(m.Sender, victim.Email) || strings.Contains(m.Sender, victim.DisplayName) {
			t.Errorf("stored message %s has Sender=%q containing victim identity; should use attacker",
				m.ID, m.Sender)
		}
	}
}

// B5/F1 SECURITY: handleProjectBroadcast is a sibling ingress that fans out
// to every running agent. Without the fix, a spoofed Sender/SenderID with
// Broadcasted=false creates a DM conversation per running agent under the
// victim's identity — wider blast radius than the handleAgentMessage path.
//
// This test verifies both halves of the fix:
//   - (a) unconditional auth-derivation of sender
//   - (b) Broadcasted forced to true server-side
func TestBroadcast_B5F1_SpoofedSenderDoesNotDeriveConversationKey(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "b5-f1-broadcast", Slug: "b5-f1-broadcast",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	attacker := &store.User{
		ID: api.NewUUID(), Email: "attacker@example.com",
		DisplayName: "Attacker", Role: store.UserRoleMember, Status: "active",
	}
	victim := &store.User{
		ID: api.NewUUID(), Email: "victim@example.com",
		DisplayName: "Victim", Role: store.UserRoleMember, Status: "active",
	}
	if err := s.CreateUser(ctx, attacker); err != nil {
		t.Fatalf("CreateUser attacker: %v", err)
	}
	if err := s.CreateUser(ctx, victim); err != nil {
		t.Fatalf("CreateUser victim: %v", err)
	}

	agent := &store.Agent{
		ID: api.NewUUID(), Name: "target-agent", Slug: "target-agent",
		ProjectID: project.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Set up broker infrastructure so the broadcast reaches deliverToAgent.
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	defer events.Close()
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectBroadcast(project.ID)
	proxy.subscribeAgent(project.ID, agent.Slug)

	// Attacker sends broadcast with spoofed sender (victim's identity)
	// and Broadcasted=false to try to reach the DM dual-write path.
	spoofed := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "user:" + victim.Email,
		SenderID:    victim.ID,
		Msg:         "broadcast as the victim",
		Broadcasted: false, // client attempts to disable broadcast flag
	}
	body, _ := json.Marshal(BroadcastMessageRequest{StructuredMessage: spoofed})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(attacker.ID, attacker.Email, attacker.DisplayName, "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	t.Logf("broadcast response: %d %s", rr.Code, rr.Body.String())

	// Give async bus fan-out time to land.
	spoofedKey, err := messages.DMConversationKey("user", victim.ID, "agent", agent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey spoofed: %v", err)
	}
	honestKey, err := messages.DMConversationKey("user", attacker.ID, "agent", agent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey honest: %v", err)
	}
	t.Logf("spoofed key (victim):  %s", spoofedKey)
	t.Logf("honest key (attacker): %s", honestKey)

	// Wait briefly for async processing.
	deadline := time.Now().Add(3 * time.Second)
	var spoofedFound, honestFound bool
	for time.Now().Before(deadline) {
		if !spoofedFound {
			if c, e := s.GetConversationByExternalRef(ctx, "native", spoofedKey); e == nil && c != nil {
				spoofedFound = true
				t.Logf("SPOOFED conversation found: id=%s ref=%s", c.ID, c.ExternalRef)
			}
		}
		if !honestFound {
			if c, e := s.GetConversationByExternalRef(ctx, "native", honestKey); e == nil && c != nil {
				honestFound = true
				t.Logf("honest conversation found: id=%s ref=%s", c.ID, c.ExternalRef)
			}
		}
		if spoofedFound || honestFound {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if spoofedFound {
		t.Errorf("SECURITY VIOLATION (B5/F1): broadcast ingress minted DM conversation "+
			"under spoofed victim key %s. handleProjectBroadcast must derive sender "+
			"from auth context and force Broadcasted=true.", spoofedKey)
	}

	// The broadcast path should NOT create DM conversations at all
	// (Broadcasted=true skips the dual-write). Neither key should exist.
	if honestFound {
		t.Errorf("broadcast created DM conversation under honest key %s — "+
			"Broadcasted must be forced true server-side to skip the DM dual-write", honestKey)
	}
}

// B5/F1b: The Broadcasted flag must be forced to true server-side on the
// broadcast path. Without this, a client setting Broadcasted=false walks the
// message through the DM dual-write in deliverToAgent, creating a DM
// conversation per running agent in the project.
func TestBroadcast_B5F1b_BroadcastedForcedTrueServerSide(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "b5-bcast-flag", Slug: "b5-bcast-flag",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	user := &store.User{
		ID: api.NewUUID(), Email: "user@example.com",
		DisplayName: "User", Role: store.UserRoleMember, Status: "active",
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create two agents so we can verify broadcast reaches them with
	// Broadcasted=true (which skips DM dual-write).
	agent1 := &store.Agent{
		ID: api.NewUUID(), Name: "a1", Slug: "a1",
		ProjectID: project.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	agent2 := &store.Agent{
		ID: api.NewUUID(), Name: "a2", Slug: "a2",
		ProjectID: project.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	for _, a := range []*store.Agent{agent1, agent2} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}

	// Set up broker.
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	defer events.Close()
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectBroadcast(project.ID)
	proxy.subscribeAgent(project.ID, agent1.Slug)
	proxy.subscribeAgent(project.ID, agent2.Slug)

	// Client explicitly sends Broadcasted=false.
	msg := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Msg:         "should be broadcast",
		Broadcasted: false,
	}
	body, _ := json.Marshal(BroadcastMessageRequest{StructuredMessage: msg})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	t.Logf("broadcast response: %d %s", rr.Code, rr.Body.String())

	// Wait for async processing.
	time.Sleep(2 * time.Second)

	// With Broadcasted forced true, NO DM conversations should be created.
	for _, a := range []*store.Agent{agent1, agent2} {
		key, err := messages.DMConversationKey("user", user.ID, "agent", a.ID)
		if err != nil {
			t.Fatalf("DMConversationKey for %s: %v", a.Slug, err)
		}
		conv, convErr := s.GetConversationByExternalRef(ctx, "native", key)
		if convErr == nil && conv != nil {
			t.Errorf("DM conversation created for agent %s (key=%s) — "+
				"Broadcasted flag was not forced to true server-side", a.Slug, key)
		}
	}
}

// B5/R1: An agent broadcasting via the broker must not receive its own
// broadcast. messagebroker.go fanOutToProject/fanOutGlobal must skip by
// SenderID (the canonical identity), not by the display-label Sender field
// (which is now in UUID form after the B5 auth-derivation override).
func TestBroadcast_R1_BroadcastingAgentDoesNotReceiveOwnMessage(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "r1-selfskip", Slug: "r1-selfskip",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	sender := &store.Agent{
		ID: api.NewUUID(), Name: "sender-agent", Slug: "sender-agent",
		ProjectID: project.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	peer := &store.Agent{
		ID: api.NewUUID(), Name: "peer-agent", Slug: "peer-agent",
		ProjectID: project.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	if err := s.CreateAgent(ctx, sender); err != nil {
		t.Fatalf("CreateAgent sender: %v", err)
	}
	if err := s.CreateAgent(ctx, peer); err != nil {
		t.Fatalf("CreateAgent peer: %v", err)
	}

	// Set up broker.
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	defer events.Close()
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectBroadcast(project.ID)
	proxy.subscribeAgent(project.ID, sender.Slug)
	proxy.subscribeAgent(project.ID, peer.Slug)

	// Agent broadcasts using its slug-form Sender (what agents actually send).
	body, _ := json.Marshal(BroadcastMessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "agent:" + sender.Slug,
			Msg:       "hello project",
		},
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		&agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: sender.ID},
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	t.Logf("broadcast response: %d %s", rr.Code, rr.Body.String())

	// Wait for async fan-out.
	countFor := func(agentID string) int {
		res, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentID}, store.ListOptions{})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		return len(res.Items)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countFor(peer.ID) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	peerN := countFor(peer.ID)
	selfN := countFor(sender.ID)
	t.Logf("delivered: peer=%d self=%d", peerN, selfN)

	// Positive control: peer must have received the broadcast.
	if peerN == 0 {
		t.Fatalf("PROBE INCONCLUSIVE: peer agent received nothing — fan-out never ran")
	}
	if selfN > 0 {
		t.Errorf("SELF-DELIVERY: broadcasting agent received its own broadcast (%d rows). "+
			"fanOutToProject must skip by SenderID, not by the Sender display label.", selfN)
	}
}

// B5/F1(a) — independent test for unconditional sender override.
// Pins sub-fix (a) independently of sub-fix (b) (Broadcasted=true).
// Uses broadcastDirect (no broker) to check stored message rows.
// Without sub-fix (a), the spoofed SenderID survives into the stored messages.
func TestBroadcast_B5F1a_SenderOverrideStoresAuthIdentity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "b5-f1a", Slug: "b5-f1a",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	attacker := &store.User{
		ID: api.NewUUID(), Email: "attacker@example.com",
		DisplayName: "Attacker", Role: store.UserRoleMember, Status: "active",
	}
	victim := &store.User{
		ID: api.NewUUID(), Email: "victim@example.com",
		DisplayName: "Victim", Role: store.UserRoleMember, Status: "active",
	}
	if err := s.CreateUser(ctx, attacker); err != nil {
		t.Fatalf("CreateUser attacker: %v", err)
	}
	if err := s.CreateUser(ctx, victim); err != nil {
		t.Fatalf("CreateUser victim: %v", err)
	}

	// Give the attacker minimum project membership so the ActionAttach authz
	// check added by #1347 passes and broadcastDirect actually runs.
	// Setting CreatedBy makes createProjectMembersGroupAndPolicy add the
	// attacker as the group owner, which grants sufficient project access.
	ensureHubMembership(ctx, s, attacker.ID)
	project.CreatedBy = attacker.ID
	if err := s.UpdateProject(ctx, project); err != nil {
		t.Fatalf("UpdateProject (set CreatedBy): %v", err)
	}
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	agent := &store.Agent{
		ID: api.NewUUID(), Name: "a1", Slug: "a1",
		ProjectID: project.ID, Phase: "running",
		Visibility: store.VisibilityPrivate,
		// No RuntimeBrokerID — uses broadcastDirect path.
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Set dispatcher so broadcastDirect doesn't 503.
	srv.SetDispatcher(noopDispatcher{})

	// Attacker sends broadcast with victim's SenderID.
	spoofed := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Sender:    "user:" + victim.Email,
		SenderID:  victim.ID,
		Msg:       "spoofed broadcast",
	}
	body, _ := json.Marshal(BroadcastMessageRequest{StructuredMessage: spoofed})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(attacker.ID, attacker.Email, attacker.DisplayName, "user", "web")))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	t.Logf("broadcast response: %d %s", rr.Code, rr.Body.String())

	// Check stored messages — SenderID must be the attacker (auth), not victim.
	res, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatalf("no messages stored for agent — broadcastDirect did not run")
	}
	for _, m := range res.Items {
		if m.SenderID == victim.ID {
			t.Errorf("stored broadcast message %s has SenderID=%s (victim); "+
				"must be %s (attacker). The sender override is not working.",
				m.ID, victim.ID, attacker.ID)
		}
		if m.SenderID != attacker.ID {
			t.Errorf("stored broadcast message %s has SenderID=%s; expected %s (attacker)",
				m.ID, m.SenderID, attacker.ID)
		}
		if strings.Contains(m.Sender, victim.Email) || strings.Contains(m.Sender, victim.DisplayName) {
			t.Errorf("stored broadcast message %s has Sender=%q containing victim identity",
				m.ID, m.Sender)
		}
	}
}

// B5/F1(c) — independent test for self-skip via authenticatedSender.
// Pins sub-fix (c) independently: a forged Sender must not change which
// agents are targeted. Uses broadcastDirect (no broker) to inspect
// which agents received messages.
//
// Scenario: sender-agent broadcasts with Sender forged to "agent:peer-agent".
// Without (c): the old slug comparison skips peer-agent and delivers to
// sender-agent — exactly backwards. With (c): auth identity skips
// sender-agent correctly.
func TestBroadcast_B5F1c_SelfSkipUsesAuthNotSender(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "b5-f1c", Slug: "b5-f1c",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	senderAgent := &store.Agent{
		ID: api.NewUUID(), Name: "sender-agent", Slug: "sender-agent",
		ProjectID: project.ID, Phase: "running",
		Visibility: store.VisibilityPrivate,
	}
	peerAgent := &store.Agent{
		ID: api.NewUUID(), Name: "peer-agent", Slug: "peer-agent",
		ProjectID: project.ID, Phase: "running",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, senderAgent); err != nil {
		t.Fatalf("CreateAgent sender: %v", err)
	}
	if err := s.CreateAgent(ctx, peerAgent); err != nil {
		t.Fatalf("CreateAgent peer: %v", err)
	}

	srv.SetDispatcher(noopDispatcher{})

	// Sender-agent broadcasts with Sender forged to look like peer-agent.
	forged := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Sender:    "agent:" + peerAgent.Slug, // forged!
		SenderID:  peerAgent.ID,              // forged!
		Msg:       "forged broadcast targeting",
	}
	body, _ := json.Marshal(BroadcastMessageRequest{StructuredMessage: forged})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/broadcast", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		&agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: senderAgent.ID},
			ProjectID: project.ID,
			Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
		}}))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)
	t.Logf("broadcast response: %d %s", rr.Code, rr.Body.String())

	// With correct self-skip: sender-agent is skipped (auth identity),
	// peer-agent receives the broadcast.
	// With forged self-skip: peer-agent is skipped (Sender match), sender
	// receives its own broadcast.
	peerMsgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: peerAgent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages peer: %v", err)
	}
	senderMsgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: senderAgent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages sender: %v", err)
	}

	t.Logf("peer received: %d, sender received: %d", len(peerMsgs.Items), len(senderMsgs.Items))

	if len(peerMsgs.Items) == 0 {
		t.Errorf("peer-agent received no messages — forged Sender caused it to be " +
			"skipped instead of the real sender. Self-skip must use auth identity.")
	}
	if len(senderMsgs.Items) > 0 {
		t.Errorf("sender-agent received its own broadcast (%d msgs) — "+
			"self-skip is using the forged Sender instead of auth identity",
			len(senderMsgs.Items))
	}
}

// B5/R2: fanOutGlobal self-skip must use SenderID, not Sender slug.
// fanOutGlobal is currently unreachable from the HTTP surface (PublishBroadcast
// only receives non-empty projectID), but fixing the class without testing
// every member leaves a latent bug that returns when a global broadcast
// endpoint is added.
//
// Direct sink test — no HTTP plumbing.
func TestBroker_R2_FanOutGlobalSelfSkipBySenderID(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectA := &store.Project{
		ID: api.NewUUID(), Name: "r2-pa", Slug: "r2-pa",
	}
	if err := s.CreateProject(ctx, projectA); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	sender := &store.Agent{
		ID: api.NewUUID(), Name: "global-sender", Slug: "global-sender",
		ProjectID: projectA.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	peer := &store.Agent{
		ID: api.NewUUID(), Name: "global-peer", Slug: "global-peer",
		ProjectID: projectA.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	if err := s.CreateAgent(ctx, sender); err != nil {
		t.Fatalf("CreateAgent sender: %v", err)
	}
	if err := s.CreateAgent(ctx, peer); err != nil {
		t.Fatalf("CreateAgent peer: %v", err)
	}

	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	defer events.Close()
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)

	// Subscribe agents so deliverToAgent can find them.
	proxy.subscribeAgent(projectA.ID, sender.Slug)
	proxy.subscribeAgent(projectA.ID, peer.Slug)

	// Call fanOutGlobal directly — SenderID is the sender agent's UUID,
	// Sender is the UUID form (as it would be after the B5 override).
	msg := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Type:        messages.TypeInstruction,
		Sender:      "agent:" + sender.ID, // UUID form, not slug
		SenderID:    sender.ID,
		Msg:         "global broadcast",
		Broadcasted: true,
	}
	proxy.fanOutGlobal(ctx, msg)

	// Check stored messages.
	countFor := func(agentID string) int {
		res, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentID}, store.ListOptions{})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		return len(res.Items)
	}

	peerN := countFor(peer.ID)
	selfN := countFor(sender.ID)
	t.Logf("fanOutGlobal delivered: peer=%d self=%d", peerN, selfN)

	if peerN == 0 {
		t.Fatalf("INCONCLUSIVE: peer received nothing — fanOutGlobal did not deliver")
	}
	if selfN > 0 {
		t.Errorf("SELF-DELIVERY in fanOutGlobal: sender received its own broadcast (%d rows). "+
			"fanOutGlobal must skip by SenderID, not by the Sender display label.", selfN)
	}
}

// R3b/F4: When a broadcast has an agent-prefixed Sender but empty SenderID,
// the fan-out cannot self-skip and the sender silently receives its own
// broadcast. The R3b warning makes this visible. This test asserts the
// warning fires — without it, the safety net can silently disappear.
func TestBroker_R3b_WarnOnEmptySenderID(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: api.NewUUID(), Name: "r3b", Slug: "r3b",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID: api.NewUUID(), Name: "warn-agent", Slug: "warn-agent",
		ProjectID: project.ID, Phase: "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: "test-broker",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Capture log output via a slog handler backed by a bytes.Buffer.
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(handler)

	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	defer events.Close()
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, logger)
	proxy.Start()
	t.Cleanup(proxy.Stop)
	proxy.subscribeAgent(project.ID, agent.Slug)

	// Agent-prefixed Sender, but NO SenderID — the exact scenario R3b warns about.
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      messages.TypeInstruction,
		Sender:    "agent:warn-agent",
		SenderID:  "", // deliberately empty
		Msg:       "self-skip impossible",
	}

	// Test fanOutToProject warning.
	proxy.fanOutToProject(ctx, project.ID, msg)

	logged := logBuf.String()
	if !strings.Contains(logged, "self-skip not possible") {
		t.Errorf("fanOutToProject: expected R3b warning in log output, got:\n%s", logged)
	}

	// Reset and test fanOutGlobal warning.
	logBuf.Reset()
	proxy.fanOutGlobal(ctx, msg)

	logged = logBuf.String()
	if !strings.Contains(logged, "self-skip not possible") {
		t.Errorf("fanOutGlobal: expected R3b warning in log output, got:\n%s", logged)
	}
}

// ---------------------------------------------------------------------------
// DEF-11 / DEF-19 tests
// ---------------------------------------------------------------------------

// testDirectMessageExternalRef replicates the legacy (pre-DEF-8) external ref
// format: dm:<sortedID1>:<sortedID2>. The canonical format was changed to
// dm:<kind>:<uuid>:<kind>:<uuid> via DMConversationKey, but the divergence
// tests need the old shape for comparison.
func testDirectMessageExternalRef(idA, idB string) string {
	pair := []string{idA, idB}
	sort.Strings(pair)
	return fmt.Sprintf("dm:%s:%s", pair[0], pair[1])
}

// def11Setup creates a project, agent, and user for DEF-11 tests.
// The userID is DevUserID because the always-override pattern in handleAgentMessage
// replaces the sender identity with the authenticated user, and doRequest
// authenticates as the dev user.
func def11Setup(t *testing.T) (srv *Server, s store.Store, projectID, agentSlug, agentID, userID string) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	projectID = tid("def11-project")
	agentID = tid("def11-agent")
	userID = DevUserID // must match the always-override sender identity
	agentSlug = "def11-agent"

	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "def11-project",
		Slug: "def11-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	brokerID := tid("def11-broker")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "def11-broker",
		Slug:   "def11-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "def11-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}
	if err := s.CreateAgent(ctx, &store.Agent{
		ID:              agentID,
		Name:            "def11-agent",
		Slug:            agentSlug,
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// The dev user may already exist in the store (created by the auth
	// middleware's last-seen upsert). Ignore duplicate-key errors.
	_ = s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "dev@localhost",
		DisplayName: "Development User",
	})

	// Set a dispatcher so the handler doesn't fail with 503.
	srv.SetDispatcher(&recordingDispatcher{})

	return srv, s, projectID, agentSlug, agentID, userID
}

// TestDEF11_PreResolvedConversation_PopulatesExternalRef verifies that when
// the CLI pre-resolves a ConversationID, the handler populates ExternalRef
// from the store — not leaving it as "".
func TestDEF11_PreResolvedConversation_PopulatesExternalRef(t *testing.T) {
	srv, s, projectID, agentSlug, agentID, userID := def11Setup(t)
	ctx := context.Background()

	// Create a conversation with a known ExternalRef.
	// DEF-49: use the kind-encoded key format so the authorization
	// check can parse it. The authenticated user is DevUserID.
	extRef, err := messages.DMConversationKey("user", userID, "agent", agentID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		t.Fatalf("UpsertConversationByExternalRef: %v", err)
	}

	// Snapshot divergence metrics; a successful DM match can only happen
	// when ExternalRef starts with "dm:", proving it was loaded from the store.
	beforeMatches := messaging.DivergenceMetrics.Matches()
	beforeMismatches := messaging.DivergenceMetrics.Mismatches()

	// Post a message with a pre-resolved ConversationID.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:def11",
				SenderID:       userID,
				Recipient:      "agent:" + agentSlug,
				Msg:            "DEF11 AC-1 test",
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The divergence check should record a dm-routing-agreement match,
	// which is only possible when ExternalRef is correctly populated from the store.
	afterMatches := messaging.DivergenceMetrics.Matches()
	afterMismatches := messaging.DivergenceMetrics.Mismatches()
	if afterMatches-beforeMatches < 1 {
		t.Errorf("expected at least 1 new match (dm-routing-agreement), got delta=%d", afterMatches-beforeMatches)
	}
	if afterMismatches-beforeMismatches != 0 {
		t.Errorf("expected 0 new mismatches, got delta=%d", afterMismatches-beforeMismatches)
	}

	// Verify the conversation in the store still has the correct ExternalRef.
	readBack, err := s.GetConversation(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if readBack.ExternalRef != extRef {
		t.Errorf("ExternalRef changed: got %q, want %q", readBack.ExternalRef, extRef)
	}
}

// TestDEF11_PreResolvedConversation_DivergenceMatch verifies that a
// pre-resolved send with a matching DM conversation produces a divergence
// match (not a mismatch).
func TestDEF11_PreResolvedConversation_DivergenceMatch(t *testing.T) {
	srv, s, projectID, agentSlug, agentID, userID := def11Setup(t)
	ctx := context.Background()

	// Create a conversation matching the sender/agent DM pair.
	// DEF-49: use the kind-encoded key format so the authorization
	// check can parse it. The authenticated user is DevUserID.
	extRef, err := messages.DMConversationKey("user", userID, "agent", agentID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: extRef,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		t.Fatalf("UpsertConversationByExternalRef: %v", err)
	}

	beforeMatches := messaging.DivergenceMetrics.Matches()
	beforeMismatches := messaging.DivergenceMetrics.Mismatches()

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:def11",
				SenderID:       userID,
				Recipient:      "agent:" + agentSlug,
				Msg:            "DEF11 AC-2 test",
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	afterMatches := messaging.DivergenceMetrics.Matches()
	afterMismatches := messaging.DivergenceMetrics.Mismatches()

	if afterMatches-beforeMatches < 1 {
		t.Errorf("expected Matches delta >= 1, got %d", afterMatches-beforeMatches)
	}
	if afterMismatches-beforeMismatches != 0 {
		t.Errorf("expected Mismatches delta == 0, got %d", afterMismatches-beforeMismatches)
	}
}

// TestDEF11_PreResolvedConversation_LookupFailure verifies that when a
// pre-resolved ConversationID does not exist in the store, the handler denies
// the request (DEF-49: fail closed on nonexistent conversation).
//
// Before DEF-49 this recorded a "conv-lookup-failed" divergence fallback and
// proceeded. After DEF-49, both lookup-failure cases deny, and the
// "conv-lookup-failed" divergence entry is dead code (removed per AC-D-7).
func TestDEF11_PreResolvedConversation_LookupFailure(t *testing.T) {
	srv, _, projectID, agentSlug, _, userID := def11Setup(t)

	// Use a non-existent conversation ID.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:def11",
				SenderID:       userID,
				Recipient:      "agent:" + agentSlug,
				Msg:            "DEF11 AC-3 test",
				Type:           messages.TypeInstruction,
				ConversationID: "nonexistent-conv-id",
			},
		})
	// DEF-49: nonexistent conversation_id is now denied with 400.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (DEF-49: nonexistent conversation denied), got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestDEF19_GroupRecipient_FullHandlerPath (AC-19-2) verifies that a group[]
// message survives the FULL HTTP handler path — going through
// POST /api/v1/projects/{pid}/agents/{slug}/message with a group[] recipient.
// It must reach handleGroupMessage and fan out, returning 200 (not 400).
func TestDEF19_GroupRecipient_FullHandlerPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("def19-project")
	agentSlugA := "agent-a"
	agentIDA := tid("def19-agent-a")
	agentSlugB := "agent-b"
	agentIDB := tid("def19-agent-b")
	userID := tid("def19-user")
	// Use agentSlugA as the anchor agent in the URL path.
	anchorSlug := agentSlugA

	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "def19-project",
		Slug: "def19-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	brokerID := tid("def19-broker")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "def19-broker",
		Slug:   "def19-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "def19-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}
	if err := s.CreateAgent(ctx, &store.Agent{
		ID:              agentIDA,
		Name:            "agent-a",
		Slug:            agentSlugA,
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}); err != nil {
		t.Fatalf("CreateAgent A: %v", err)
	}
	if err := s.CreateAgent(ctx, &store.Agent{
		ID:              agentIDB,
		Name:            "agent-b",
		Slug:            agentSlugB,
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}); err != nil {
		t.Fatalf("CreateAgent B: %v", err)
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "def19@example.com",
		DisplayName: "DEF19 User",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Suppress the unused-var warning for agentIDB.
	_ = agentIDB

	// Set a dispatcher so the handler doesn't fail with 503.
	srv.SetDispatcher(&recordingDispatcher{})

	// Send a group[] message through the full HTTP path.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+anchorSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:def19",
				SenderID:  userID,
				Recipient: "group[agent:" + agentSlugA + ",agent:" + agentSlugB + "]",
				Msg:       "DEF-19 group message test",
				Type:      messages.TypeInstruction,
			},
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for group[] message, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the response is a GroupMessageResponse.
	var resp GroupMessageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal GroupMessageResponse: %v", err)
	}
	if resp.GroupID == "" {
		t.Error("expected non-empty group_id in response")
	}
	if resp.Delivered != 2 {
		t.Errorf("expected 2 delivered, got %d", resp.Delivered)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	for i, r := range resp.Results {
		if r.Status != "delivered" {
			t.Errorf("result[%d]: expected status=delivered, got %q (error: %s)", i, r.Status, r.Error)
		}
	}
}

// TestDEF49_DivergenceMismatch_AuthorizedButWrongAgent verifies that the
// divergence comparison detects a real routing mismatch on the pre-resolved
// path, even when DEF-49 authorization passes.
//
// The authenticated user (DevUserID) is named in the conversation's DM key
// (one of the two slots), so authorization succeeds. But the conversation
// names a *different* agent than the one the message is addressed to, so
// OldRoutingFromMessage (which uses the addressed agent's ID) disagrees with
// the conversation's ExternalRef — producing a "dm-routing-mismatch".
//
// This is the coverage successor for the original
// TestDEF11_PreResolvedConversation_GenuineDisagreement, which was deleted
// because DEF-49 turned it into a duplicate of the non-membership negative
// test (AC-D-7).
func TestDEF49_DivergenceMismatch_AuthorizedButWrongAgent(t *testing.T) {
	srv, s, projectID, agentSlug, _, userID := def11Setup(t)
	ctx := context.Background()

	// Create a conversation keyed to (DevUserID, otherAgentID).
	// DevUserID occupies one slot, so DEF-49 direct authorization passes.
	// But the message is addressed to agentSlug (agentID), not otherAgentID,
	// so the divergence comparison will disagree.
	otherAgentID := tid("def49-divergence-other-agent")
	divergentRef, err := messages.DMConversationKey("user", userID, "agent", otherAgentID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: divergentRef,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		t.Fatalf("UpsertConversationByExternalRef: %v", err)
	}

	beforeMismatches := messaging.DivergenceMetrics.Mismatches()

	// Post a message to agentSlug with the divergent conversation.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:def49-divergence",
				SenderID:       userID,
				Recipient:      "agent:" + agentSlug,
				Msg:            "DEF49 divergence mismatch test",
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (authorized but divergent), got %d: %s",
			rec.Code, rec.Body.String())
	}

	afterMismatches := messaging.DivergenceMetrics.Mismatches()
	if afterMismatches-beforeMismatches < 1 {
		t.Errorf("expected Mismatches delta >= 1 (dm-routing-mismatch), got %d",
			afterMismatches-beforeMismatches)
	}
}

// ---------------------------------------------------------------------------
// DEF-49 — caller-supplied conversation_id authorization
// ---------------------------------------------------------------------------
//
// AC-D-8: Three negative tests, one per facet, each reachable on main today
// (expected RED on upstream/main, GREEN after the fix).
//
// AC-D-9: One positive test proving the legitimate scion message @agent path
// still works (expected GREEN on both main and after the fix).

// def49Setup creates a project, two agents (attacker and target), a user
// (the legitimate sender, DevUserID), and a dispatcher so the handler
// doesn't fail with 503. It returns everything needed to exercise the
// caller-supplied conversation_id authorization path.
func def49Setup(t *testing.T) (srv *Server, s store.Store, projectID string, targetAgent *store.Agent, userID string) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	projectID = tid("def49-project")
	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "def49-project",
		Slug: "def49-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	brokerID := tid("def49-broker")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "def49-broker",
		Slug:   "def49-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "def49-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}

	targetAgent = &store.Agent{
		ID:              tid("def49-target-agent"),
		Name:            "def49-target-agent",
		Slug:            "def49-target-agent",
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, targetAgent); err != nil {
		t.Fatalf("CreateAgent (target): %v", err)
	}

	userID = DevUserID
	_ = s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "dev@localhost",
		DisplayName: "Development User",
	})

	srv.SetDispatcher(&recordingDispatcher{})
	return srv, s, projectID, targetAgent, userID
}

// TestDEF49_NonMembership_DirectConversation verifies that an authenticated
// user cannot attribute a message to a direct conversation whose DM key
// does not name them (AC-D-8 facet a, AC-INGRESS-1 violation).
//
// RED on upstream/main: the if-branch accepts any caller-supplied
// conversation_id without checking the authenticated sender.
// GREEN after DEF-49: denied with 403.
func TestDEF49_NonMembership_DirectConversation(t *testing.T) {
	srv, s, _, targetAgent, _ := def49Setup(t)
	ctx := context.Background()

	// Create two uninvolved users whose DM conversation the attacker
	// (dev user) should NOT be able to write into.
	otherUserA := &store.User{
		ID:          tid("def49-other-user-a"),
		Email:       "other-a@example.com",
		DisplayName: "Other A",
	}
	if err := s.CreateUser(ctx, otherUserA); err != nil {
		t.Fatalf("CreateUser (otherA): %v", err)
	}

	// Build a DM key between otherUserA and the target agent.
	// The dev user (DevUserID) is NOT a participant.
	dmKey, err := messages.DMConversationKey("user", otherUserA.ID, "agent", targetAgent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		t.Fatalf("UpsertConversationByExternalRef: %v", err)
	}

	// Send a message as the dev user (DevUserID), claiming to belong to
	// that conversation. The dev user is NOT named in the DM key.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:dev",
				SenderID:       DevUserID,
				Recipient:      "agent:" + targetAgent.Slug,
				Msg:            "DEF49 non-membership test",
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
		})

	if rec.Code != http.StatusForbidden {
		t.Errorf("DEF-49 facet (a): expected 403 Forbidden for non-member "+
			"direct conversation attribution, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestDEF49_NonExistent_ConversationID verifies that an authenticated user
// cannot attribute a message to a conversation ID that does not exist in the
// store (AC-D-8 facet b).
//
// RED on upstream/main: the if-branch sets lookupFailed=true and proceeds.
// GREEN after DEF-49: denied with 400.
func TestDEF49_NonExistent_ConversationID(t *testing.T) {
	srv, _, _, targetAgent, _ := def49Setup(t)

	// Use a valid-looking UUID that does not correspond to any conversation.
	fakeConvID := tid("def49-nonexistent-conv")

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:dev",
				SenderID:       DevUserID,
				Recipient:      "agent:" + targetAgent.Slug,
				Msg:            "DEF49 nonexistent conversation test",
				Type:           messages.TypeInstruction,
				ConversationID: fakeConvID,
			},
		})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("DEF-49 facet (b): expected 400 Bad Request for nonexistent "+
			"conversation_id, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestDEF49_CrossProject_GroupConversation verifies that an authenticated
// user cannot attribute a message to a group conversation that belongs to a
// different project than the target agent (AC-D-8 facet c).
//
// RED on upstream/main: the if-branch accepts any conversation_id regardless
// of project. GREEN after DEF-49: denied with 403.
func TestDEF49_CrossProject_GroupConversation(t *testing.T) {
	srv, s, _, targetAgent, _ := def49Setup(t)
	ctx := context.Background()

	// Create a second project that the target agent does NOT belong to.
	otherProjectID := tid("def49-other-project")
	if err := s.CreateProject(ctx, &store.Project{
		ID:   otherProjectID,
		Name: "def49-other-project",
		Slug: "def49-other-project",
	}); err != nil {
		t.Fatalf("CreateProject (other): %v", err)
	}

	// Create a group conversation in the OTHER project.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "group:" + otherProjectID + ":general",
		ProjectID:   &otherProjectID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		t.Fatalf("UpsertConversationByExternalRef: %v", err)
	}

	// Send a message to the target agent (in the first project), claiming
	// the conversation belongs to the other project.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:dev",
				SenderID:       DevUserID,
				Recipient:      "agent:" + targetAgent.Slug,
				Msg:            "DEF49 cross-project test",
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
		})

	if rec.Code != http.StatusForbidden {
		t.Errorf("DEF-49 facet (c): expected 403 Forbidden for cross-project "+
			"group conversation attribution, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestDEF49_LegitimatePreResolved_DirectConversation verifies that the
// legitimate scion message @agent path — where the CLI resolves the
// conversation and supplies the id — is still accepted after the DEF-49
// authorization check (AC-D-9).
//
// The authenticated user (DevUserID) sends a message with a pre-resolved
// conversation_id for a direct conversation whose DM key DOES name them.
// Expected: 200 OK (or 503 if dispatcher is missing, but we set one).
func TestDEF49_LegitimatePreResolved_DirectConversation(t *testing.T) {
	srv, s, _, targetAgent, userID := def49Setup(t)
	ctx := context.Background()

	// Create a direct conversation between the authenticated user (DevUserID)
	// and the target agent — exactly what `scion message @agent` would produce.
	dmKey, err := messages.DMConversationKey("user", userID, "agent", targetAgent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		t.Fatalf("UpsertConversationByExternalRef: %v", err)
	}

	// Send a message with the pre-resolved ConversationID — the legitimate path.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.Slug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:        messages.Version,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
				Sender:         "user:dev",
				SenderID:       userID,
				Recipient:      "agent:" + targetAgent.Slug,
				Msg:            "DEF49 legitimate pre-resolved test",
				Type:           messages.TypeInstruction,
				ConversationID: created.ID,
			},
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("DEF-49 AC-D-9: expected 200 OK for legitimate pre-resolved "+
			"conversation_id (the scion message @agent path), got %d: %s",
			rec.Code, rec.Body.String())
	}

	// Verify the message was persisted in the correct conversation.
	msgResult, err := s.ListMessages(ctx, store.MessageFilter{
		ConversationID: created.ID,
	}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgResult.Items) == 0 {
		t.Error("expected at least one message persisted in the conversation")
	}
}

// TestDEF49_GroupConversation_UnsetProjectID verifies that a group
// conversation with an unset project ID (nil or the zero UUID) is
// denied even when the agent has a real project ID. This prevents the
// "two unset IDs compare equal" class of bug (see isUnsetProjectID in
// validate.go:136).
func TestDEF49_GroupConversation_UnsetProjectID(t *testing.T) {
	srv, s, _, targetAgent, _ := def49Setup(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		projectID *string // nil or a zero-value UUID pointer
	}{
		{"nil project ID", nil},
		{"zero UUID", strPtr("00000000-0000-0000-0000-000000000000")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conv := &store.Conversation{
				Kind:        "group",
				Surface:     "native",
				ExternalRef: "group:unset-" + tc.name + ":general",
				ProjectID:   tc.projectID,
				DriftState:  "active",
			}
			created, err := s.UpsertConversationByExternalRef(ctx, conv)
			if err != nil {
				t.Fatalf("UpsertConversationByExternalRef: %v", err)
			}

			rec := doRequest(t, srv, http.MethodPost,
				"/api/v1/projects/"+targetAgent.ProjectID+"/agents/"+targetAgent.Slug+"/message",
				MessageRequest{
					StructuredMessage: &messages.StructuredMessage{
						Version:        messages.Version,
						Timestamp:      time.Now().UTC().Format(time.RFC3339),
						Sender:         "user:dev",
						SenderID:       DevUserID,
						Recipient:      "agent:" + targetAgent.Slug,
						Msg:            "DEF49 unset project test",
						Type:           messages.TypeInstruction,
						ConversationID: created.ID,
					},
				})

			if rec.Code != http.StatusForbidden {
				t.Errorf("expected 403 for conversation with unset project ID (%s), got %d: %s",
					tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func strPtr(s string) *string { return &s }
