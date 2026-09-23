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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// This file covers Phase 3 (F2a project-based read authorization, design
// doc §3.2, §8 Phase 3, §9 AC-7/AC-8/AC-10): group conversation reads on the
// conversation API are authorized by project membership, not participant
// rows, with a strict cross-project denial for agents and an unchanged
// participant-based fallback for legacy projectless groups.

// grantUserProjectAccess grants a human user project-member access,
// mirroring grantAgentProjectAccess for the "user" principal kind.
func grantUserProjectAccess(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition not found")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// enableCrossProjectMessagingForTest turns the Hub cross-project messaging
// switch on, the same way TestCrossProjectConversationReadGate does.
func enableCrossProjectMessagingForTest(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	fakeStore := newFakeHubSettingStore()
	val, err := json.Marshal(map[string]interface{}{
		"cross_project_messaging_enabled": true,
	})
	require.NoError(t, err)
	_, err = fakeStore.UpsertHubSetting(ctx, "messaging", val, "test", 0, "test")
	require.NoError(t, err)
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

// seedGroupMessage creates one message in conv so the messages/message-id
// endpoints have something to return.
func seedGroupMessage(t *testing.T, s store.Store, projectID, agentID, convID string) *store.Message {
	t.Helper()
	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      projectID,
		AgentID:        agentID,
		Sender:         "agent:seed",
		SenderID:       agentID,
		Recipient:      "user:test@example.com",
		RecipientID:    api.NewUUID(),
		Msg:            "phase3 seed message",
		Type:           "instruction",
		ConversationID: convID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))
	return msg
}

// TestPhase3_GroupRead_SameProjectNonParticipant_Agent_Allowed is AC-7's
// agent half: an agent in the conversation's project, but not a
// participant, can GET the conversation, its messages, and a single
// message.
func TestPhase3_GroupRead_SameProjectNonParticipant_Agent_Allowed(t *testing.T) {
	srv, s := testServer(t)
	project, _, conv := setupConvTestData(t, s)

	nonParticipant := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "phase3-non-participant",
		Slug:       "phase3-non-participant",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), nonParticipant))
	grantAgentProjectAccess(t, s, nonParticipant.ID, project.ID)

	msg := seedGroupMessage(t, s, project.ID, nonParticipant.ID, conv.ID)
	ctxFn := func() context.Context {
		return agentContextWithScopes(nonParticipant.ID, project.ID, []AgentTokenScope{ScopeProjectRead})
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil).WithContext(ctxFn())
	getRR := httptest.NewRecorder()
	srv.handleGetConversation(getRR, getReq, conv.ID)
	require.Equal(t, http.StatusOK, getRR.Code, "GET conversation: body: %s", getRR.Body.String())

	msgsReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages", nil).WithContext(ctxFn())
	msgsRR := httptest.NewRecorder()
	srv.handleConvListMessages(msgsRR, msgsReq, conv.ID)
	require.Equal(t, http.StatusOK, msgsRR.Code, "GET messages: body: %s", msgsRR.Body.String())

	msgReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+msg.ID, nil).WithContext(ctxFn())
	msgRR := httptest.NewRecorder()
	srv.handleGetConversationMessage(msgRR, msgReq, conv.ID, msg.ID)
	require.Equal(t, http.StatusOK, msgRR.Code, "GET message: body: %s", msgRR.Body.String())
}

// TestPhase3_GroupRead_SameProjectNonParticipant_User_Allowed is AC-7's
// human half: a user with project access, but not a participant, can GET
// the conversation, its messages, and a single message.
func TestPhase3_GroupRead_SameProjectNonParticipant_User_Allowed(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)

	userID := api.NewUUID()
	user := &store.User{
		ID:          userID,
		Email:       "phase3-user@example.com",
		DisplayName: "Phase3 User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), user))
	grantUserProjectAccess(t, s, userID, project.ID)

	msg := seedGroupMessage(t, s, project.ID, agent.ID, conv.ID)
	ctxFn := func() context.Context {
		return contextWithIdentity(context.Background(),
			NewAuthenticatedUser(userID, user.Email, user.DisplayName, "member", "web"))
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil).WithContext(ctxFn())
	getRR := httptest.NewRecorder()
	srv.handleGetConversation(getRR, getReq, conv.ID)
	require.Equal(t, http.StatusOK, getRR.Code, "GET conversation: body: %s", getRR.Body.String())

	msgsReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages", nil).WithContext(ctxFn())
	msgsRR := httptest.NewRecorder()
	srv.handleConvListMessages(msgsRR, msgsReq, conv.ID)
	require.Equal(t, http.StatusOK, msgsRR.Code, "GET messages: body: %s", msgsRR.Body.String())

	msgReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+msg.ID, nil).WithContext(ctxFn())
	msgRR := httptest.NewRecorder()
	srv.handleGetConversationMessage(msgRR, msgReq, conv.ID, msg.ID)
	require.Equal(t, http.StatusOK, msgRR.Code, "GET message: body: %s", msgRR.Body.String())
}

// TestPhase3_GroupRead_OtherProjectAgent_Denied_EvenWithCrossProjectEnabled
// is AC-8's agent half and the ptone-decided strict rule (design §3.2): an
// agent from another project is denied, even when Hub cross-project
// messaging is enabled — group read authorization does not consult that
// switch at all.
func TestPhase3_GroupRead_OtherProjectAgent_Denied_EvenWithCrossProjectEnabled(t *testing.T) {
	srv, s := testServer(t)
	project, _, conv := setupConvTestData(t, s)
	enableCrossProjectMessagingForTest(t, srv)

	otherProject := &store.Project{ID: api.NewUUID(), Name: "phase3-other-project", Slug: "phase3-other-project"}
	require.NoError(t, s.CreateProject(context.Background(), otherProject))

	foreignAgent := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "phase3-foreign-agent",
		Slug:       "phase3-foreign-agent",
		ProjectID:  otherProject.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), foreignAgent))
	// Even with full project access granted on the CONVERSATION's project
	// (not just its own), the strict identity check in
	// authorizeGroupConversationAccess denies before authorize() ever runs,
	// because the agent's token project doesn't match conv's project.
	grantAgentProjectAccess(t, s, foreignAgent.ID, project.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContextWithScopes(foreignAgent.ID, otherProject.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"a foreign-project agent must be denied even with cross-project messaging enabled; body: %s", rr.Body.String())
}

// TestPhase3_GroupRead_OtherProjectUser_Denied is AC-8's human half: a user
// with no access to the conversation's project is denied.
func TestPhase3_GroupRead_OtherProjectUser_Denied(t *testing.T) {
	srv, s := testServer(t)
	_, _, conv := setupConvTestData(t, s)

	userID := api.NewUUID()
	user := &store.User{
		ID:          userID,
		Email:       "phase3-no-access@example.com",
		DisplayName: "Phase3 No Access",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), user))
	// No grantUserProjectAccess call: this user has zero role bindings.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(contextWithIdentity(context.Background(),
		NewAuthenticatedUser(userID, user.Email, user.DisplayName, "member", "web")))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"a user without project access must be denied; body: %s", rr.Body.String())
}

// TestPhase3_GroupRead_LegacyProjectlessGroup_ParticipantCheck is the
// design's explicitly reversible fallback: a group conversation with no
// project (pre-#1846) keeps today's participant-based gate — a participant
// is allowed, a non-participant is denied — rather than being widened or
// fail-closed.
func TestPhase3_GroupRead_LegacyProjectlessGroup_ParticipantCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	now := time.Now().UTC()
	legacyConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Legacy Projectless Group",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, legacyConv))

	project := &store.Project{ID: api.NewUUID(), Name: "phase3-legacy-project", Slug: "phase3-legacy-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	participant := &store.Agent{
		ID: api.NewUUID(), Name: "phase3-legacy-participant", Slug: "phase3-legacy-participant",
		ProjectID: project.ID, Phase: "running", Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, participant))
	addConvParticipant(t, s, legacyConv.ID, "agent", participant.ID)

	nonParticipant := &store.Agent{
		ID: api.NewUUID(), Name: "phase3-legacy-nonparticipant", Slug: "phase3-legacy-nonparticipant",
		ProjectID: project.ID, Phase: "running", Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, nonParticipant))
	// Grant nonParticipant full project access — it must still be denied,
	// because a projectless conversation is never gated by project
	// membership, only by the participant table.
	grantAgentProjectAccess(t, s, nonParticipant.ID, project.ID)

	participantReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+legacyConv.ID, nil)
	participantReq = participantReq.WithContext(agentContext(participant.ID, project.ID))
	participantRR := httptest.NewRecorder()
	srv.handleGetConversation(participantRR, participantReq, legacyConv.ID)
	require.Equal(t, http.StatusOK, participantRR.Code,
		"a participant of a legacy projectless group must still be allowed; body: %s", participantRR.Body.String())

	nonParticipantReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+legacyConv.ID, nil)
	nonParticipantReq = nonParticipantReq.WithContext(agentContextWithScopes(nonParticipant.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	nonParticipantRR := httptest.NewRecorder()
	srv.handleGetConversation(nonParticipantRR, nonParticipantReq, legacyConv.ID)
	require.Equal(t, http.StatusForbidden, nonParticipantRR.Code,
		"a non-participant of a legacy projectless group must be denied even with full project access; body: %s",
		nonParticipantRR.Body.String())
}

// TestPhase3_ListConversations_ProjectUnion_IncludesNonParticipatedGroup is
// AC-10: GET /conversations?project_id=P includes every group in P for an
// authorized caller, not just the ones the caller participates in.
func TestPhase3_ListConversations_ProjectUnion_IncludesNonParticipatedGroup(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, agent, participatedConv := setupConvTestData(t, s)
	addConvParticipant(t, s, participatedConv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	now := time.Now().UTC()
	otherConv := &store.Conversation{
		ID:             api.NewUUID(),
		ProjectID:      &project.ID,
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Not Participated",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, otherConv))
	// Deliberately no participant row for `agent` on otherConv.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?project_id="+project.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))

	var sawParticipated, sawOther bool
	for _, c := range result.Conversations {
		if c.ID == participatedConv.ID {
			sawParticipated = true
		}
		if c.ID == otherConv.ID {
			sawOther = true
		}
	}
	require.True(t, sawParticipated, "the participated group must still appear")
	require.True(t, sawOther, "AC-10: a non-participated group in the same project must appear when project_id is supplied")
}

// TestPhase3_ListConversations_ProjectUnion_UnauthorizedProjectIDNoUnion
// covers the additive-only degrade path: an unauthorized project_id must
// not error or leak other projects' groups — it just gets no union, same
// as passing an unknown project_id.
func TestPhase3_ListConversations_ProjectUnion_UnauthorizedProjectIDNoUnion(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	otherProject := &store.Project{ID: api.NewUUID(), Name: "phase3-union-other", Slug: "phase3-union-other"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	otherConv := &store.Conversation{
		ID:             api.NewUUID(),
		ProjectID:      &otherProject.ID,
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Other Project Group",
		DriftState:     "active",
		LastActivityAt: time.Now().UTC(),
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateConversation(ctx, otherConv))

	// The caller has no access to otherProject at all.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?project_id="+otherProject.ID, nil)
	req = req.WithContext(agentContext(agent.ID, otherProject.ID))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "an unauthorized project_id must degrade quietly, not error; body: %s", rr.Body.String())
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	for _, c := range result.Conversations {
		require.NotEqual(t, otherConv.ID, c.ID, "must not leak a group from a project the caller cannot read")
	}
}
