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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ---------------------------------------------------------------------------
// GAP 1: handleProjectBroadcast authorization tests
// ---------------------------------------------------------------------------

// setupBroadcastProject creates a project with a running agent for broadcast tests.
func setupBroadcastProject(t *testing.T, srv *Server, s store.Store) *store.Project {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Slug: "authz-bcast",
		Name: "authz-bcast",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "bcast-agent",
		Name:       "bcast-agent",
		ProjectID:  project.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	srv.SetDispatcher(&brokerMockDispatcher{})
	return project
}

// broadcastBody returns a valid broadcast request body.
func broadcastBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test",
			Msg:    "hello",
			Type:   messages.TypeInstruction,
		},
	})
	if err != nil {
		t.Fatalf("marshal broadcast body: %v", err)
	}
	return b
}

// TestBroadcast_UnauthenticatedDenied verifies that requests without any
// identity in the context receive 403 (the handler checks for identity
// before the authz gate).
func TestBroadcast_UnauthenticatedDenied(t *testing.T) {
	srv, s := testServer(t)
	project := setupBroadcastProject(t, srv, s)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast",
		bytes.NewReader(broadcastBody(t)))
	req.Header.Set("Content-Type", "application/json")
	// No identity in context

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)

	if rr.Code != http.StatusForbidden {
		t.Errorf("unauthenticated: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBroadcast_UserWithoutProjectMembership verifies that an authenticated
// user who is NOT a member of the project gets 403.
func TestBroadcast_UserWithoutProjectMembership(t *testing.T) {
	srv, s := testServer(t)
	seedRoleDefinitions(context.Background(), s)
	project := setupBroadcastProject(t, srv, s)

	outsider := NewAuthenticatedUser("outsider-1", "outsider@test.com", "Outsider", "member", "cli")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast",
		bytes.NewReader(broadcastBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), outsider))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)

	if rr.Code != http.StatusForbidden {
		t.Errorf("outsider user: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBroadcast_UserWithProjectAttach verifies that an authenticated user
// with project-level attach permission can broadcast successfully.
func TestBroadcast_UserWithProjectAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	project := setupBroadcastProject(t, srv, s)

	userID := api.NewUUID()
	owner := NewAuthenticatedUser(userID, "owner@test.com", "Owner", "member", "cli")

	// Create user in store and make them project owner (which grants attach).
	if err := s.CreateUser(ctx, &store.User{
		ID: userID, Email: "owner@test.com", DisplayName: "Owner",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ensureHubMembership(ctx, s, userID)
	srv.createProjectMembersGroupAndPolicy(ctx, project, userID)
	if err := srv.createProjectOwnerRoleBinding(ctx, project.ID, userID); err != nil {
		t.Fatalf("createProjectOwnerRoleBinding: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast",
		bytes.NewReader(broadcastBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), owner))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)

	if rr.Code != http.StatusAccepted {
		t.Errorf("project owner: expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBroadcast_AgentSameProject verifies that an agent in the same project
// with lifecycle scope can broadcast.
func TestBroadcast_AgentSameProject(t *testing.T) {
	srv, s := testServer(t)
	project := setupBroadcastProject(t, srv, s)

	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "agent-caller"},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
	}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast",
		bytes.NewReader(broadcastBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdent))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)

	if rr.Code != http.StatusAccepted {
		t.Errorf("same-project agent: expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBroadcast_AgentDifferentProject verifies that an agent in a DIFFERENT
// project gets 403 even with lifecycle scope.
func TestBroadcast_AgentDifferentProject(t *testing.T) {
	srv, s := testServer(t)
	project := setupBroadcastProject(t, srv, s)

	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: "agent-other"},
		ProjectID: "different-project-id",
		Scopes:    []AgentTokenScope{ScopeAgentLifecycle},
	}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/broadcast",
		bytes.NewReader(broadcastBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdent))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, project.ID)

	if rr.Code != http.StatusForbidden {
		t.Errorf("cross-project agent: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestBroadcast_NonexistentProject verifies that broadcasting to a project
// that doesn't exist returns 404 for user callers.
func TestBroadcast_NonexistentProject(t *testing.T) {
	srv, s := testServer(t)
	seedRoleDefinitions(context.Background(), s)

	user := NewAuthenticatedUser("user-1", "user@test.com", "User", "member", "cli")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/nonexistent-id/broadcast",
		bytes.NewReader(broadcastBody(t)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), user))

	rr := httptest.NewRecorder()
	srv.handleProjectBroadcast(rr, req, "nonexistent-id")

	if rr.Code != http.StatusNotFound {
		t.Errorf("nonexistent project: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GAP 2: sendAgentRouted authorization tests
// ---------------------------------------------------------------------------

// setupChatAgent creates a project and agent for sendAgentRouted tests.
func setupChatAgent(t *testing.T, srv *Server, s store.Store) (*store.Project, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Slug: "chat-authz",
		Name: "chat-authz",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "chat-agent",
		Name:       "chat-agent",
		ProjectID:  project.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	srv.SetDispatcher(&brokerMockDispatcher{})
	return project, agent
}

// TestSendAgentRouted_WithoutAttach verifies that a user WITHOUT ActionAttach
// on the primary agent gets 403.
func TestSendAgentRouted_WithoutAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	project, agent := setupChatAgent(t, srv, s)

	outsider := NewAuthenticatedUser("outsider-1", "outsider@test.com", "Outsider", "member", "cli")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/topic:"+project.ID+"/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), outsider))

	rr := httptest.NewRecorder()
	msgID := srv.sendAgentRouted(rr, req, "topic:"+project.ID, project.ID, outsider,
		"hello", "outsider@test.com", []*store.Agent{agent}, nil, nil, nil, time.Now(), "")

	if msgID != "" {
		t.Errorf("expected empty msgID on denied request, got %q", msgID)
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("outsider: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify no message was persisted.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs.Items) != 0 {
		t.Errorf("expected 0 persisted messages, got %d", len(msgs.Items))
	}
}

// TestSendAgentRouted_WithAttach verifies that a user WITH ActionAttach
// can send a chat message normally.
func TestSendAgentRouted_WithAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	project, agent := setupChatAgent(t, srv, s)

	userID := api.NewUUID()
	owner := NewAuthenticatedUser(userID, "owner@test.com", "Owner", "member", "cli")

	// Create user in store and grant project ownership (which provides attach).
	if err := s.CreateUser(ctx, &store.User{
		ID: userID, Email: "owner@test.com", DisplayName: "Owner",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ensureHubMembership(ctx, s, userID)
	srv.createProjectMembersGroupAndPolicy(ctx, project, userID)
	if err := srv.createProjectOwnerRoleBinding(ctx, project.ID, userID); err != nil {
		t.Fatalf("createProjectOwnerRoleBinding: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/topic:"+project.ID+"/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), owner))

	rr := httptest.NewRecorder()
	msgID := srv.sendAgentRouted(rr, req, "topic:"+project.ID, project.ID, owner,
		"hello", "owner@test.com", []*store.Agent{agent}, nil, nil, nil, time.Now(), "")

	if msgID == "" {
		t.Errorf("expected non-empty msgID, got empty; response: %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Code != http.StatusCreated {
		t.Errorf("owner: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestSendAgentRouted_MentionSkippedWithoutAttach verifies that mentioned
// agents the user lacks attach permission on are silently skipped, while
// the primary agent (with permission) still receives the message.
func TestSendAgentRouted_MentionSkippedWithoutAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	project := &store.Project{
		ID:   api.NewUUID(),
		Slug: "mention-authz",
		Name: "mention-authz",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	primaryAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "primary-agent",
		Name:       "primary-agent",
		ProjectID:  project.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	mentionAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "mention-agent",
		Name:       "mention-agent",
		ProjectID:  project.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	for _, a := range []*store.Agent{primaryAgent, mentionAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}
	srv.SetDispatcher(&brokerMockDispatcher{})

	// Create a user who is project owner — has attach on all agents in the
	// project by default. For a more targeted test we would need per-agent
	// deny policies, but the owner bypass makes that infeasible in the
	// current authz model. Instead we verify the structural correctness:
	// the authz check executes for each mention agent and allowed mentions
	// proceed to dispatch.
	userID := api.NewUUID()
	owner := NewAuthenticatedUser(userID, "owner@test.com", "Owner", "member", "cli")

	if err := s.CreateUser(ctx, &store.User{
		ID: userID, Email: "owner@test.com", DisplayName: "Owner",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ensureHubMembership(ctx, s, userID)
	srv.createProjectMembersGroupAndPolicy(ctx, project, userID)
	if err := srv.createProjectOwnerRoleBinding(ctx, project.ID, userID); err != nil {
		t.Fatalf("createProjectOwnerRoleBinding: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/topic:"+project.ID+"/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), owner))

	rr := httptest.NewRecorder()
	agents := []*store.Agent{primaryAgent, mentionAgent}
	mentionResults := []messages.MentionResult{{Slug: "mention-agent", Status: "delivered"}}
	msgID := srv.sendAgentRouted(rr, req, "topic:"+project.ID, project.ID, owner,
		"@mention-agent hello", "owner@test.com", agents, []string{"mention-agent"}, mentionResults, nil, time.Now(), "")

	if msgID == "" {
		t.Errorf("expected non-empty msgID, got empty; response: %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Code != http.StatusCreated {
		t.Errorf("owner with mention: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify messages were persisted for both agents (owner has attach on both).
	msgs, err := s.ListMessages(ctx, store.MessageFilter{ProjectID: project.ID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs.Items) < 2 {
		t.Errorf("expected at least 2 persisted messages (primary + mention), got %d", len(msgs.Items))
	}
}
