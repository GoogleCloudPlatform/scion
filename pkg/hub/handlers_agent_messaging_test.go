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
	"database/sql"
	"encoding/json"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// recordingBus is a fan-out spoke that keeps what the handler published, so a
// test can assert the route the handler chose without wiring persistence.
type recordingBus struct {
	mu   sync.Mutex
	sent []*messages.StructuredMessage
}

func (b *recordingBus) Publish(_ context.Context, _ string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, msg)
	return nil
}

func (b *recordingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (b *recordingBus) Close() error { return nil }

func (b *recordingBus) last() *messages.StructuredMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sent) == 0 {
		return nil
	}
	return b.sent[len(b.sent)-1]
}

// TestOutboundMessage_ReplyAffinityRestoresRoute exercises the affinity block
// end to end: with a webchat store and a registered channel present, an agent
// reply that names no route is put back where the user last spoke.
func TestOutboundMessage_ReplyAffinityRestoresRoute(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv.SetWebChatStore(wcs)

	// A fan-out bus with a "web" spoke, so the channel the affinity row names
	// passes the registered-channel check below it.
	rec := &recordingBus{}
	bus := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: "web", ChannelID: "web", Bus: rec, Observer: true},
	}, slog.Default())
	srv.SetMessageBrokerProxy(NewMessageBrokerProxy(bus, s, srv.events, func() AgentDispatcher { return nil }, slog.Default()))

	project := &store.Project{
		ID: api.NewUUID(), Name: "aff", Slug: "aff", Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	human := &store.User{ID: api.NewUUID(), Email: "human@example.com", DisplayName: "Human", Status: store.UserStatusActive}
	if err := s.CreateUser(ctx, human); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent := &store.Agent{
		ID: api.NewUUID(), Name: "a", Slug: "a", ProjectID: project.ID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// The user last spoke in a web thread.
	if err := wcs.RecordChannel(ctx, human.ID, project.ID, agent.ID, "web", "topic-42", time.Now()); err != nil {
		t.Fatalf("RecordChannel: %v", err)
	}

	send := func(t *testing.T, threadID string) *messages.StructuredMessage {
		t.Helper()
		body, _ := json.Marshal(OutboundMessageRequest{
			Recipient: "user:human@example.com",
			Msg:       "reply",
			ThreadID:  threadID,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims: jwt.Claims{Subject: agent.ID}, ProjectID: project.ID,
		}}))
		rr := httptest.NewRecorder()
		srv.handleAgentOutboundMessage(rr, req, agent.ID)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		m := rec.last()
		if m == nil {
			t.Fatal("nothing was published to the spoke")
		}
		return m
	}

	t.Run("an untagged reply is routed back to the thread", func(t *testing.T) {
		m := send(t, "")
		if m.Channel != "web" {
			t.Errorf("channel = %q, want web", m.Channel)
		}
		if m.ThreadID != "topic-42" {
			t.Errorf("thread = %q, want topic-42; an untagged reply lands beside the conversation without it", m.ThreadID)
		}
	})

	t.Run("a thread named by the caller is not overwritten", func(t *testing.T) {
		m := send(t, "topic-99")
		if m.ThreadID != "topic-99" {
			t.Errorf("thread = %q, want topic-99", m.ThreadID)
		}
	})
}
