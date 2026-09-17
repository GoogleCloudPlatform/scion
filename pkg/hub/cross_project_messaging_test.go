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

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers for cross-project messaging
// ---------------------------------------------------------------------------

// cpmSetup creates two projects (A and B) with agents, owners, and hub-level
// operational settings that enable cross-project messaging.
func cpmSetup(t *testing.T) (srv *Server, s store.Store, projectA, projectB string, ownerA, ownerB *store.User, agentA, agentB *store.Agent) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	// Create owners
	ownerA = &store.User{
		ID:          tid("cpm-owner-a"),
		Email:       "owner-a@test.com",
		DisplayName: "Owner A",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	ownerB = &store.User{
		ID:          tid("cpm-owner-b"),
		Email:       "owner-b@test.com",
		DisplayName: "Owner B",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerB))
	ensureHubMembership(ctx, s, ownerB.ID)

	// Create project A
	projectA = tid("cpm-project-a")
	pA := &store.Project{
		ID:        projectA,
		Name:      "project-a",
		Slug:      "project-a",
		OwnerID:   ownerA.ID,
		CreatedBy: ownerA.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, pA))
	srv.createProjectMembersGroup(ctx, pA)
	msgAuthzAddProjectMember(t, s, ownerA.ID, projectA, "project-a", store.GroupMemberRoleOwner)
	// Set inbound policy to "any" (CreateProject doesn't persist this field; default revision is 1)
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectA, store.CrossProjectInboundAny, 1)
	require.NoError(t, err, "failed to set project A inbound policy")

	// Create project B
	projectB = tid("cpm-project-b")
	pB := &store.Project{
		ID:        projectB,
		Name:      "project-b",
		Slug:      "project-b",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, pB))
	srv.createProjectMembersGroup(ctx, pB)
	msgAuthzAddProjectMember(t, s, ownerB.ID, projectB, "project-b", store.GroupMemberRoleOwner)
	// Set inbound policy to "any" (default revision is 1)
	_, err = s.UpdateProjectMessagingPolicy(ctx, projectB, store.CrossProjectInboundAny, 1)
	require.NoError(t, err, "failed to set project B inbound policy")

	// Create hub-mode agent in project A
	agentA = &store.Agent{
		ID:          tid("cpm-agent-a"),
		Name:        "agent-alpha",
		Slug:        "agent-alpha",
		ProjectID:   projectA,
		MessageMode: store.MessageModeHub,
		Ancestry:    []string{ownerA.ID},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	// Create project-mode agent in project B
	agentB = &store.Agent{
		ID:          tid("cpm-agent-b"),
		Name:        "agent-beta",
		Slug:        "agent-beta",
		ProjectID:   projectB,
		MessageMode: store.MessageModeProject,
		Ancestry:    []string{ownerB.ID},
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Enable cross-project messaging via OperationalSettings.
	enableCPM(t, srv, s)

	return srv, s, projectA, projectB, ownerA, ownerB, agentA, agentB
}

// enableCPM sets up OperationalSettings with cross_project_messaging_enabled=true.
func enableCPM(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := koanf.New(".")
	envK := koanf.New(".")
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	// Seed the messaging section with cross-project enabled.
	doc := []byte(`{"conversation_envelope_switch":true,"cross_project_messaging_enabled":true}`)
	rev, err := ops.Update(context.Background(), "messaging", doc, "test", 0, "managed")
	require.NoError(t, err, "failed to seed messaging opsettings")
	require.Greater(t, rev, int64(0), "expected positive revision")
	// Verify the value is readable.
	require.True(t, ops.CrossProjectMessagingEnabled(), "cross-project should be enabled after Update")
	srv.SetOperationalSettings(ops)
}

func cpmAgentIdentity(agentID, projectID string, ancestry []string) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Ancestry:  ancestry,
	}}
}

// ---------------------------------------------------------------------------
// D1: Target resolution API tests
// ---------------------------------------------------------------------------

func TestTargetResolve_ValidTarget(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/messaging/targets/resolve?project=project-b&agent=agent-beta", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))

	rr := httptest.NewRecorder()
	srv.handleMessagingTargetsResolve(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "expected 200 for valid target, got %d: %s", rr.Code, rr.Body.String())

	var resp targetResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.Equal(t, "agent-beta", resp.Agent.Slug)
	require.NotNil(t, resp.Messageability)
	require.True(t, resp.Messageability.CanMessage, "hub-mode A should be able to message project-mode B")
}

func TestTargetResolve_InvalidTarget_PrivacyPreserving(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/messaging/targets/resolve?project=nonexistent-project&agent=nonexistent", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))

	rr := httptest.NewRecorder()
	srv.handleMessagingTargetsResolve(rr, req)

	// Privacy-preserving: should return 404, indistinguishable from nonexistent
	require.Equal(t, http.StatusNotFound, rr.Code, "expected 404 for nonexistent target")
}

func TestTargetResolve_MissingParams(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/messaging/targets/resolve?project=project-b", nil) // no agent param
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))

	rr := httptest.NewRecorder()
	srv.handleMessagingTargetsResolve(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code, "expected 400 for missing params")
}

// ---------------------------------------------------------------------------
// D2: Capabilities API tests
// ---------------------------------------------------------------------------

func TestCapabilities_Authenticated(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messaging/capabilities", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))

	rr := httptest.NewRecorder()
	srv.handleMessagingCapabilities(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp messagingCapabilitiesResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.HubEnabled, "cross-project should be enabled")
	require.Contains(t, resp.CrossProjectConversationKinds, "direct")
	require.Len(t, resp.SupportedModes, 5)
}

func TestCapabilities_Unauthenticated(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messaging/capabilities", nil)
	// No identity in context

	rr := httptest.NewRecorder()
	srv.handleMessagingCapabilities(rr, req)

	require.Equal(t, http.StatusUnauthorized, rr.Code)
}

// ---------------------------------------------------------------------------
// D3: Conversation resolver tests
// ---------------------------------------------------------------------------

func TestConversationResolve_NoExistingConversation(t *testing.T) {
	srv, _, _, _, _, _, _, _ := cpmSetup(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/resolve?reference=@agent-beta&project_id="+tid("cpm-project-b"), nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))

	rr := httptest.NewRecorder()
	srv.handleConversationResolve(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp conversationResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.Exists, "should not exist without prior conversation")
	require.NotNil(t, resp.PeerAgent, "should return peer agent info")
	require.Equal(t, "agent-beta", resp.PeerAgent.Slug)
}

func TestConversationResolve_NoMintOnRead(t *testing.T) {
	srv, s, _, _, _, _, _, _ := cpmSetup(t)
	ctx := context.Background()

	// Count conversations before resolve
	convsBefore, err := s.GetConversationsForPrincipal(ctx, "agent", tid("cpm-agent-a"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/resolve?reference=@agent-beta&project_id="+tid("cpm-project-b"), nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		tid("cpm-agent-a"), tid("cpm-project-a"), []string{tid("cpm-owner-a")})))

	rr := httptest.NewRecorder()
	srv.handleConversationResolve(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify no rows were created
	convsAfter, err := s.GetConversationsForPrincipal(ctx, "agent", tid("cpm-agent-a"))
	require.NoError(t, err)
	require.Equal(t, len(convsBefore), len(convsAfter), "conversation resolve must not create rows")
}

func TestConversationResolve_ExistingConversation(t *testing.T) {
	srv, s, _, _, _, _, agentA, agentB := cpmSetup(t)
	ctx := context.Background()

	// Create a DM conversation manually
	key, kind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      agentA.ID,
		RecipientKind: "agent",
		RecipientID:   agentB.ID,
	})
	require.NoError(t, err)

	conv := &store.Conversation{
		Kind:        kind,
		Surface:     "native",
		ExternalRef: key,
		DisplayName: "DM",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)
	require.NotNil(t, created)

	// Now resolve — should find the existing conversation
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/resolve?reference=@agent-beta&project_id="+tid("cpm-project-b"), nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		agentA.ID, agentA.ProjectID, agentA.Ancestry)))

	rr := httptest.NewRecorder()
	srv.handleConversationResolve(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp conversationResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Exists, "should find existing conversation")
	require.NotNil(t, resp.Conversation, "should include conversation detail")
	require.Equal(t, created.ID, resp.Conversation.ID)
}

func TestConversationResolve_ByConvUUID(t *testing.T) {
	srv, s, _, _, _, _, agentA, agentB := cpmSetup(t)
	ctx := context.Background()

	// Create a DM conversation
	key, kind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      agentA.ID,
		RecipientKind: "agent",
		RecipientID:   agentB.ID,
	})
	require.NoError(t, err)

	conv := &store.Conversation{
		Kind:        kind,
		Surface:     "native",
		ExternalRef: key,
		DisplayName: "DM",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Resolve by conv:<uuid>
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/conversations/resolve?reference=conv:"+created.ID, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), cpmAgentIdentity(
		agentA.ID, agentA.ProjectID, agentA.Ancestry)))

	rr := httptest.NewRecorder()
	srv.handleConversationResolve(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp conversationResolveResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Exists)
}

// ---------------------------------------------------------------------------
// Cross-project authorization tests
// ---------------------------------------------------------------------------

func TestCrossProjectAuth_HubToProject_Allowed(t *testing.T) {
	srv, s, projectA, projectB, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	hubAgent := msgAuthzAgent(t, s, "cpm-hub-sender", projectA, store.MessageModeHub, []string{ownerA.ID})
	projectAgent := msgAuthzAgent(t, s, "cpm-project-receiver", projectB, store.MessageModeProject, []string{ownerB.ID})

	senderIdent := cpmAgentIdentity(hubAgent.ID, projectA, hubAgent.Ancestry)
	allowed, reason := srv.authorizeAgentMessage(ctx, senderIdent, projectAgent, false)
	require.True(t, allowed, "hub-mode sender to project-mode receiver with inbound=any should be allowed: %s", reason)
}

func TestCrossProjectAuth_ProjectToProject_Denied(t *testing.T) {
	srv, s, projectA, projectB, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	projectAgentA := msgAuthzAgent(t, s, "cpm-proj-sender", projectA, store.MessageModeProject, []string{ownerA.ID})
	projectAgentB := msgAuthzAgent(t, s, "cpm-proj-receiver", projectB, store.MessageModeProject, []string{ownerB.ID})

	senderIdent := cpmAgentIdentity(projectAgentA.ID, projectA, projectAgentA.Ancestry)
	allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, projectAgentB, false)
	require.False(t, allowed, "project-mode to project-mode cross-project should be denied (sender must be hub)")
}

func TestCrossProjectAuth_HubDisabled_Denied(t *testing.T) {
	srv, s, projectA, projectB, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	// Disable cross-project messaging
	disableCPM(t, srv)

	hubAgent := msgAuthzAgent(t, s, "cpm-hub-disabled", projectA, store.MessageModeHub, []string{ownerA.ID})
	projectAgent := msgAuthzAgent(t, s, "cpm-proj-disabled", projectB, store.MessageModeProject, []string{ownerB.ID})

	senderIdent := cpmAgentIdentity(hubAgent.ID, projectA, hubAgent.Ancestry)
	allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, projectAgent, false)
	require.False(t, allowed, "cross-project should be denied when hub switch is off")
}

func disableCPM(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := emptyKoanf()
	envK := emptyKoanf()
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	// cross-project defaults to false
	srv.SetOperationalSettings(ops)
}

func TestCrossProjectAuth_InboundNone_Denied(t *testing.T) {
	srv, s, _, _, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	// Create a project with inbound=none
	closedProjectID := tid("cpm-closed-project")
	closedProject := &store.Project{
		ID:                  closedProjectID,
		Name:                "closed-project",
		Slug:                "closed-project",
		OwnerID:             ownerB.ID,
		CreatedBy:           ownerB.ID,
		CrossProjectInbound: store.CrossProjectInboundNone,
		Created:             time.Now(),
		Updated:             time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, closedProject))

	closedAgent := msgAuthzAgent(t, s, "cpm-closed-agent", closedProjectID, store.MessageModeProject, []string{ownerB.ID})
	hubAgent := msgAuthzAgent(t, s, "cpm-hub-to-closed", tid("cpm-project-a"), store.MessageModeHub, []string{ownerA.ID})

	senderIdent := cpmAgentIdentity(hubAgent.ID, tid("cpm-project-a"), hubAgent.Ancestry)
	allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, closedAgent, false)
	require.False(t, allowed, "cross-project to inbound=none project should be denied")
}

func TestCrossProjectAuth_InboundMembers_OriginNotMember(t *testing.T) {
	srv, s, _, _, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	// Create a project with inbound=members
	membersProjectID := tid("cpm-members-project")
	membersProject := &store.Project{
		ID:        membersProjectID,
		Name:      "members-project",
		Slug:      "members-project",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, membersProject))
	srv.createProjectMembersGroup(ctx, membersProject)
	msgAuthzAddProjectMember(t, s, ownerB.ID, membersProjectID, "members-project", store.GroupMemberRoleOwner)
	_, err := s.UpdateProjectMessagingPolicy(ctx, membersProjectID, store.CrossProjectInboundMembers, 1)
	require.NoError(t, err, "failed to set inbound=members")

	membersAgent := msgAuthzAgent(t, s, "cpm-members-agent", membersProjectID, store.MessageModeProject, []string{ownerB.ID})
	hubAgent := msgAuthzAgent(t, s, "cpm-hub-to-members", tid("cpm-project-a"), store.MessageModeHub, []string{ownerA.ID})

	senderIdent := cpmAgentIdentity(hubAgent.ID, tid("cpm-project-a"), hubAgent.Ancestry)
	allowed, _ := srv.authorizeAgentMessage(ctx, senderIdent, membersAgent, false)
	require.False(t, allowed, "cross-project to inbound=members should deny when origin user is not a member")
}

func TestCrossProjectAuth_InboundMembers_OriginIsMember(t *testing.T) {
	srv, s, _, _, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	// Create a project with inbound=members
	membersProjectID := tid("cpm-members2-project")
	membersProject := &store.Project{
		ID:        membersProjectID,
		Name:      "members2-project",
		Slug:      "members2-project",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, membersProject))
	srv.createProjectMembersGroup(ctx, membersProject)
	msgAuthzAddProjectMember(t, s, ownerB.ID, membersProjectID, "members2-project", store.GroupMemberRoleOwner)
	_, err := s.UpdateProjectMessagingPolicy(ctx, membersProjectID, store.CrossProjectInboundMembers, 1)
	require.NoError(t, err, "failed to set inbound=members")

	// Add ownerA as a member of the members project
	msgAuthzAddProjectMember(t, s, ownerA.ID, membersProjectID, "members2-project", store.GroupMemberRoleMember)

	membersAgent := msgAuthzAgent(t, s, "cpm-members2-agent", membersProjectID, store.MessageModeProject, []string{ownerB.ID})
	hubAgent := msgAuthzAgent(t, s, "cpm-hub-to-members2", tid("cpm-project-a"), store.MessageModeHub, []string{ownerA.ID})

	senderIdent := cpmAgentIdentity(hubAgent.ID, tid("cpm-project-a"), hubAgent.Ancestry)
	allowed, reason := srv.authorizeAgentMessage(ctx, senderIdent, membersAgent, false)
	require.True(t, allowed, "cross-project to inbound=members should allow when origin user is a member: %s", reason)
}

// ---------------------------------------------------------------------------
// Cross-project read access tests
// ---------------------------------------------------------------------------

func TestCrossProjectReadAccess_BothHubMode(t *testing.T) {
	srv, s, projectA, projectB, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	hubAgentA := msgAuthzAgent(t, s, "cpm-read-hub-a", projectA, store.MessageModeHub, []string{ownerA.ID})
	hubAgentB := msgAuthzAgent(t, s, "cpm-read-hub-b", projectB, store.MessageModeHub, []string{ownerB.ID})

	allowed, reason := srv.EvaluateCrossProjectReadAccess(ctx, hubAgentA, hubAgentB)
	require.True(t, allowed, "hub-mode pair should have read access: %s", reason)
}

func TestCrossProjectReadAccess_Disabled(t *testing.T) {
	srv, s, projectA, projectB, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	disableCPM(t, srv)

	hubAgentA := msgAuthzAgent(t, s, "cpm-read-dis-a", projectA, store.MessageModeHub, []string{ownerA.ID})
	hubAgentB := msgAuthzAgent(t, s, "cpm-read-dis-b", projectB, store.MessageModeHub, []string{ownerB.ID})

	allowed, _ := srv.EvaluateCrossProjectReadAccess(ctx, hubAgentA, hubAgentB)
	require.False(t, allowed, "read access should be denied when hub switch is off")
}

// ---------------------------------------------------------------------------
// One-way policy: send succeeds, reply denied
// ---------------------------------------------------------------------------

func TestCrossProject_OneWayPolicy(t *testing.T) {
	srv, s, projectA, _, ownerA, ownerB, _, _ := cpmSetup(t)
	ctx := context.Background()

	// Agent A is hub mode in project A (inbound=any)
	senderAgent := msgAuthzAgent(t, s, "cpm-oneway-sender", projectA, store.MessageModeHub, []string{ownerA.ID})

	// Agent B is project-mode in a project that allows any inbound
	receiverProjectID := tid("cpm-oneway-project")
	receiverProject := &store.Project{
		ID:        receiverProjectID,
		Name:      "oneway-project",
		Slug:      "oneway-project",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, receiverProject))
	_, err := s.UpdateProjectMessagingPolicy(ctx, receiverProjectID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err, "failed to set inbound=any for oneway-project")

	receiverAgent := msgAuthzAgent(t, s, "cpm-oneway-receiver", receiverProjectID, store.MessageModeProject, []string{ownerB.ID})

	// Forward: sender → receiver should be allowed
	senderIdent := cpmAgentIdentity(senderAgent.ID, projectA, senderAgent.Ancestry)
	allowed, reason := srv.authorizeAgentMessage(ctx, senderIdent, receiverAgent, false)
	require.True(t, allowed, "forward send should be allowed: %s", reason)

	// Reverse: receiver → sender should be denied (receiver is project-mode, not hub)
	receiverIdent := cpmAgentIdentity(receiverAgent.ID, receiverProjectID, receiverAgent.Ancestry)
	allowed, _ = srv.authorizeAgentMessage(ctx, receiverIdent, senderAgent, false)
	require.False(t, allowed, "reply should be denied because receiver is project-mode, can't send cross-project")
}

// ---------------------------------------------------------------------------
// derivePeerFromExternalRef tests
// ---------------------------------------------------------------------------

func TestDerivePeerFromExternalRef(t *testing.T) {
	t.Run("agent-agent DM", func(t *testing.T) {
		ref := "dm:agent:aaa-bbb-ccc-ddd-eee-fff-111-222:agent:xxx-yyy-zzz-www-111-222-333-444"
		kind, id := derivePeerFromExternalRef(ref, "agent", "aaa-bbb-ccc-ddd-eee-fff-111-222")
		require.Equal(t, "agent", kind)
		require.Equal(t, "xxx-yyy-zzz-www-111-222-333-444", id)
	})

	t.Run("reverse direction", func(t *testing.T) {
		ref := "dm:agent:aaa-bbb-ccc-ddd-eee-fff-111-222:agent:xxx-yyy-zzz-www-111-222-333-444"
		kind, id := derivePeerFromExternalRef(ref, "agent", "xxx-yyy-zzz-www-111-222-333-444")
		require.Equal(t, "agent", kind)
		require.Equal(t, "aaa-bbb-ccc-ddd-eee-fff-111-222", id)
	})

	t.Run("not a participant", func(t *testing.T) {
		ref := "dm:agent:aaa:agent:bbb"
		kind, id := derivePeerFromExternalRef(ref, "agent", "ccc")
		require.Equal(t, "", kind)
		require.Equal(t, "", id)
	})

	t.Run("not a DM ref", func(t *testing.T) {
		ref := "thread:some-thread"
		kind, id := derivePeerFromExternalRef(ref, "agent", "aaa")
		require.Equal(t, "", kind)
		require.Equal(t, "", id)
	})
}
