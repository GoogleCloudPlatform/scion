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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
		ID:         api.NewUUID(),
		Name:       "flood-project",
		Slug:       "flood-project",
		Visibility: store.VisibilityPrivate,
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
// The send itself is still accepted, as it is today — tightening the type
// contract on the wire is a separate compatibility change (#1054).
func TestOutboundMessage_UnknownTypeIsChargedAsAgentTraffic(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "type-project",
		Slug:       "type-project",
		Visibility: store.VisibilityPrivate,
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
	if rr.Code != http.StatusOK {
		t.Fatalf("expected an unknown message type to be accepted as before, got %d: %s", rr.Code, rr.Body.String())
	}

	// It cost a token from the agent's aggregate allowance, not from the
	// cheaper mirror reservation.
	if got := srv.chatSendLimiter.buckets["agent:"+agent.ID].tokens; got != chatSendAgentRatePerMinute-1 {
		t.Errorf("agent bucket = %v tokens, want %v: the send must be charged to the agent aggregate",
			got, chatSendAgentRatePerMinute-1)
	}
	if _, ok := srv.chatSendLimiter.buckets["agent-mirror:"+agent.ID]; ok {
		t.Error("an unknown type must not be classified as transcript-mirror traffic")
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
		ID:         api.NewUUID(),
		Name:       "mirror-project",
		Slug:       "mirror-project",
		Visibility: store.VisibilityPrivate,
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
		ID:         api.NewUUID(),
		Name:       "b5-security-project",
		Slug:       "b5-security-project",
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
		Visibility: store.VisibilityPrivate,
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
