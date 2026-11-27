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
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestListConversations_AgentIDFilter_SharedDirectAndGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	project := &store.Project{ID: api.NewUUID(), Name: "shared-conv-proj", Slug: "shared-conv-proj"}
	require.NoError(t, s.CreateProject(ctx, project))

	targetAgent := &store.Agent{ID: api.NewUUID(), Name: "target-agent", Slug: "target-agent", ProjectID: project.ID, Phase: "running"}
	otherAgent := &store.Agent{ID: api.NewUUID(), Name: "other-agent", Slug: "other-agent", ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	user := &store.User{ID: api.NewUUID(), Email: "viewer@example.com", DisplayName: "Viewer", Role: "member", Status: "active"}
	otherUser := &store.User{ID: api.NewUUID(), Email: "other@example.com", DisplayName: "Other", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))
	require.NoError(t, s.CreateUser(ctx, otherUser))
	grantUserProjectAccess(t, s, user.ID, project.ID)

	// 1. Shared DM between viewer and targetAgent (most recent).
	sharedDMKey, err := messages.DMConversationKey("user", user.ID, "agent", targetAgent.ID)
	require.NoError(t, err)
	sharedDM := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: sharedDMKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, sharedDM))
	addConvParticipant(t, s, sharedDM.ID, "user", user.ID)
	addConvParticipant(t, s, sharedDM.ID, "agent", targetAgent.ID)

	// 2. Shared group between viewer and targetAgent.
	sharedGroup := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &project.ID, Kind: "group", Surface: "native",
		DisplayName: "Shared Group", DriftState: "active",
		LastActivityAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute),
	}
	require.NoError(t, s.CreateConversation(ctx, sharedGroup))
	addConvParticipant(t, s, sharedGroup.ID, "user", user.ID)
	addConvParticipant(t, s, sharedGroup.ID, "agent", targetAgent.ID)

	// 3. Group where targetAgent previously participated but left (should be excluded).
	leftGroup := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &project.ID, Kind: "group", Surface: "native",
		DisplayName: "Left Group", DriftState: "active",
		LastActivityAt: now.Add(-2 * time.Minute), CreatedAt: now.Add(-2 * time.Minute),
	}
	require.NoError(t, s.CreateConversation(ctx, leftGroup))
	addConvParticipant(t, s, leftGroup.ID, "user", user.ID)
	addConvParticipant(t, s, leftGroup.ID, "agent", targetAgent.ID)
	require.NoError(t, s.RemoveParticipant(ctx, leftGroup.ID, "agent", targetAgent.ID))

	// 4. Caller-only DM with otherAgent (should be excluded).
	otherDMKey, err := messages.DMConversationKey("user", user.ID, "agent", otherAgent.ID)
	require.NoError(t, err)
	callerOnlyDM := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: otherDMKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, callerOnlyDM))
	addConvParticipant(t, s, callerOnlyDM.ID, "user", user.ID)
	addConvParticipant(t, s, callerOnlyDM.ID, "agent", otherAgent.ID)

	// 5. Caller-only group (should be excluded).
	callerOnlyGroup := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &project.ID, Kind: "group", Surface: "native",
		DisplayName: "Caller Only", DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, callerOnlyGroup))
	addConvParticipant(t, s, callerOnlyGroup.ID, "user", user.ID)

	// 6. Agent-only DM with otherUser (should be excluded).
	agentOnlyDMKey, err := messages.DMConversationKey("user", otherUser.ID, "agent", targetAgent.ID)
	require.NoError(t, err)
	agentOnlyDM := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: agentOnlyDMKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, agentOnlyDM))
	addConvParticipant(t, s, agentOnlyDM.ID, "user", otherUser.ID)
	addConvParticipant(t, s, agentOnlyDM.ID, "agent", targetAgent.ID)

	// 7. Agent-only group (should be excluded).
	agentOnlyGroup := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &project.ID, Kind: "group", Surface: "native",
		DisplayName: "Agent Only", DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, agentOnlyGroup))
	addConvParticipant(t, s, agentOnlyGroup.ID, "agent", targetAgent.ID)

	userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "web"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?agent_id="+targetAgent.ID, nil).WithContext(userCtx)
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, 2, result.Total)
	require.False(t, result.Truncated)
	require.Len(t, result.Conversations, 2)
	require.Equal(t, sharedDM.ID, result.Conversations[0].ID)
	require.NotNil(t, result.Conversations[0].DMPeer)
	require.Equal(t, targetAgent.ID, result.Conversations[0].DMPeer.ID)
	require.Equal(t, targetAgent.Name, result.Conversations[0].DMPeer.Name)
	require.Equal(t, sharedGroup.ID, result.Conversations[1].ID)
}

func TestListConversations_AgentIDFilter_CallerReadScopeEnforced(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	allowedProj := &store.Project{ID: api.NewUUID(), Name: "allowed-proj", Slug: "allowed-proj"}
	deniedProj := &store.Project{ID: api.NewUUID(), Name: "denied-proj", Slug: "denied-proj"}
	require.NoError(t, s.CreateProject(ctx, allowedProj))
	require.NoError(t, s.CreateProject(ctx, deniedProj))

	allowedAgent := &store.Agent{ID: api.NewUUID(), Name: "allowed-agent", Slug: "allowed-agent", ProjectID: allowedProj.ID, Phase: "running"}
	deniedAgent := &store.Agent{ID: api.NewUUID(), Name: "denied-agent", Slug: "denied-agent", ProjectID: deniedProj.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, allowedAgent))
	require.NoError(t, s.CreateAgent(ctx, deniedAgent))

	user := &store.User{ID: api.NewUUID(), Email: "scoped@example.com", DisplayName: "Scoped", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))
	grantUserProjectAccess(t, s, user.ID, allowedProj.ID)
	// Intentionally do NOT grant user access to deniedProj.

	// Group in deniedProj where BOTH user and deniedAgent have participant rows.
	// groupReads.canRead must still filter it out.
	deniedGroup := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &deniedProj.ID, Kind: "group", Surface: "native",
		DisplayName: "Revoked Project Group", DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, deniedGroup))
	addConvParticipant(t, s, deniedGroup.ID, "user", user.ID)
	addConvParticipant(t, s, deniedGroup.ID, "agent", deniedAgent.ID)

	userIdent := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "web")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?agent_id="+deniedAgent.ID, nil).
		WithContext(contextWithIdentity(ctx, userIdent))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Empty(t, result.Conversations)
	require.Equal(t, 0, result.Total)

	// Shared group + DM in allowedProj, accessed via a UAT token scoped to deniedProj
	// (wrong project boundary) — tokenMayUseConversation must filter both out.
	allowedGroup := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &allowedProj.ID, Kind: "group", Surface: "native",
		DisplayName: "Allowed Group", DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, allowedGroup))
	addConvParticipant(t, s, allowedGroup.ID, "user", user.ID)
	addConvParticipant(t, s, allowedGroup.ID, "agent", allowedAgent.ID)

	allowedDMKey, err := messages.DMConversationKey("user", user.ID, "agent", allowedAgent.ID)
	require.NoError(t, err)
	allowedDM := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: allowedDMKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, allowedDM))
	addConvParticipant(t, s, allowedDM.ID, "user", user.ID)
	addConvParticipant(t, s, allowedDM.ID, "agent", allowedAgent.ID)

	// Scoped UAT pinned to a different project the user CAN access (otherAllowedProj)
	// with inbox:read authorizes the list call (200 OK), while tokenMayUseConversation
	// filters out allowedProj's group and DM because they fall outside the token boundary.
	otherAllowedProj := &store.Project{ID: api.NewUUID(), Name: "other-allowed-proj", Slug: "other-allowed-proj"}
	require.NoError(t, s.CreateProject(ctx, otherAllowedProj))
	grantUserProjectAccess(t, s, user.ID, otherAllowedProj.ID)

	tok := mintHubConfigToken(t, srv, user.ID, projectBoundary(otherAllowedProj.ID), "inbox:read")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/conversations?agent_id="+allowedAgent.ID, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Empty(t, result.Conversations)

	// Agent caller with cross-project messaging disabled cannot see a cross-project DM
	// with deniedAgent even when both have participant rows.
	crossDMKey, err := messages.DMConversationKey("agent", allowedAgent.ID, "agent", deniedAgent.ID)
	require.NoError(t, err)
	crossDM := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: crossDMKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, crossDM))
	addConvParticipant(t, s, crossDM.ID, "agent", allowedAgent.ID)
	addConvParticipant(t, s, crossDM.ID, "agent", deniedAgent.ID)

	agentCtx := projectReaderAgentContext(t, s, allowedAgent.ID, allowedProj.ID)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/conversations?agent_id="+deniedAgent.ID, nil).
		WithContext(agentCtx)
	rr = httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Empty(t, result.Conversations)
}

func TestListConversations_AgentIDFilter_CanonicalDMCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	project := &store.Project{ID: api.NewUUID(), Name: "dm-canon-proj", Slug: "dm-canon-proj"}
	require.NoError(t, s.CreateProject(ctx, project))

	targetAgent := &store.Agent{ID: api.NewUUID(), Name: "target-agent", Slug: "target-agent", ProjectID: project.ID, Phase: "running"}
	otherAgent := &store.Agent{ID: api.NewUUID(), Name: "other-agent", Slug: "other-agent", ProjectID: project.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	user := &store.User{ID: api.NewUUID(), Email: "dmcheck@example.com", DisplayName: "DM Check", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))
	grantUserProjectAccess(t, s, user.ID, project.ID)

	// Canonical DM is between user and otherAgent, NOT targetAgent.
	// Insert a stale/forged participant row for targetAgent.
	otherDMKey, err := messages.DMConversationKey("user", user.ID, "agent", otherAgent.ID)
	require.NoError(t, err)
	forgedDM := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native", ExternalRef: otherDMKey,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, forgedDM))
	addConvParticipant(t, s, forgedDM.ID, "user", user.ID)
	addConvParticipant(t, s, forgedDM.ID, "agent", otherAgent.ID)
	// Store.AddParticipant rejects non-canonical DM participants on write;
	// insert directly via SQL to model a stale or forged participant row.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB() for this test")
	_, err = dbProvider.DB().ExecContext(
		ctx,
		`INSERT INTO conversation_participants (id, conversation_id, principal_kind, principal_id, role, joined_at) VALUES (?, ?, 'agent', ?, 'member', ?)`,
		api.NewUUID(), forgedDM.ID, targetAgent.ID, now,
	)
	require.NoError(t, err)

	userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "web"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?agent_id="+targetAgent.ID, nil).WithContext(userCtx)
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Empty(t, result.Conversations)
	require.Equal(t, 0, result.Total)
}

func TestListConversations_AgentIDFilter_UnknownOrCrossProjectReturnsEmpty200(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	projA := &store.Project{ID: api.NewUUID(), Name: "proj-a", Slug: "proj-a"}
	projB := &store.Project{ID: api.NewUUID(), Name: "proj-b", Slug: "proj-b"}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))

	foreignAgent := &store.Agent{ID: api.NewUUID(), Name: "foreign-agent", Slug: "foreign-agent", ProjectID: projB.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, foreignAgent))

	user := &store.User{ID: api.NewUUID(), Email: "oracle@example.com", DisplayName: "Oracle", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))
	grantUserProjectAccess(t, s, user.ID, projA.ID)

	// Give the caller a conversation in projA so the unfiltered list is non-empty.
	conv := &store.Conversation{
		ID: api.NewUUID(), ProjectID: &projA.ID, Kind: "group", Surface: "native",
		DisplayName: "Proj A Group", DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))
	addConvParticipant(t, s, conv.ID, "user", user.ID)

	userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "web"))

	for _, tc := range []struct {
		name    string
		agentID string
	}{
		{name: "unknown agent UUID", agentID: api.NewUUID()},
		{name: "cross-project unreadable agent", agentID: foreignAgent.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?agent_id="+tc.agentID, nil).WithContext(userCtx)
			rr := httptest.NewRecorder()
			srv.handleListConversations(rr, req)

			require.Equal(t, http.StatusOK, rr.Code, "must return 200 empty rather than 403/404")
			var result conversationListResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
			require.Empty(t, result.Conversations)
			require.Equal(t, 0, result.Total)
			require.False(t, result.Truncated)
		})
	}
}
