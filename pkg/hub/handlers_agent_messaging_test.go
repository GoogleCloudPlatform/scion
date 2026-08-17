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

	if rr := postOutbound(t, srv, project.ID, bystander, "unrelated report"); rr.Code != http.StatusOK {
		t.Errorf("a second agent must not be throttled by the flooder: got %d: %s", rr.Code, rr.Body.String())
	}

	// The refusal is transient: at 60/min a token accrues every second.
	clock.Advance(time.Second)
	if rr := postOutbound(t, srv, project.ID, flooder, "after backoff"); rr.Code != http.StatusOK {
		t.Errorf("expected the send to succeed after backing off, got %d: %s", rr.Code, rr.Body.String())
	}
}

// The automatic assistant-reply transcript mirror shares an agent with the
// messages the agent writes itself, but not a bucket: a chatty agent whose
// mirror is over its limit can still deliver a completion report or a blocker
// escalation. Low-value traffic must not starve high-value traffic.
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

	// Exhaust the mirror's allowance with hook-posted assistant replies.
	for i := range chatSendAgentMirrorRatePerMinute {
		rr := postOutboundTyped(t, srv, project.ID, agent.ID, "mirrored transcript", messages.TypeAssistantReply)
		if rr.Code != http.StatusOK {
			t.Fatalf("mirror send %d: expected 200, got %d: %s", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := postOutboundTyped(t, srv, project.ID, agent.ID, "mirrored transcript", messages.TypeAssistantReply)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("the mirror is over its limit and should be refused, got %d: %s", rr.Code, rr.Body.String())
	}

	// The agent's own message to a human is unaffected.
	if rr := postOutbound(t, srv, project.ID, agent.ID, "task complete"); rr.Code != http.StatusOK {
		t.Fatalf("the agent's own message must not be starved by its transcript mirror: got %d: %s",
			rr.Code, rr.Body.String())
	}
}
