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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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
