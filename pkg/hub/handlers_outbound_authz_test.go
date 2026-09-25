//go:build !no_sqlite

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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Outbound agent-DM authorization gate tests (#1685)
//
// These are router-level tests that exercise the HTTP handler
// (handleAgentOutboundMessage) with authenticated agents, real store fixtures,
// and side-effect spies. They verify that mode and cross-project authorization
// gates are enforced on the outbound DM path before any side effects.
// ---------------------------------------------------------------------------

// authzDMSetup creates a server, project, two agents with configurable modes,
// a DM conversation between them, and a recording dispatcher. This mirrors
// def171Setup but allows configuring MessageMode on both agents.
func authzDMSetup(t *testing.T, senderMode, targetMode string) (
	srv *Server, s store.Store, project *store.Project,
	sender, target *store.Agent, convID string, dispatcher *recordingDispatcher,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   tid("authz-dm-project"),
		Name: "authz-dm-project",
		Slug: "authz-dm-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("authz-dm-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-dm-broker",
		Slug:   "authz-dm-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender = &store.Agent{
		ID:              tid("authz-dm-sender"),
		Name:            "sender-agent",
		Slug:            "sender-agent",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     senderMode,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	target = &store.Agent{
		ID:              tid("authz-dm-target"),
		Name:            "target-agent",
		Slug:            "target-agent",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     targetMode,
	}
	require.NoError(t, s.CreateAgent(ctx, target))

	// Create the DM conversation between the two agents.
	dmKey, err := messages.DMConversationKey("agent", sender.ID, "agent", target.ID)
	require.NoError(t, err)

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)
	convID = conv.ID

	dispatcher = &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	return srv, s, project, sender, target, convID, dispatcher
}

// sendAuthzDM sends an agent-to-agent DM through the real HTTP handler,
// using conversation_ref addressing. Ancestry from the agent record is
// included in the identity context for cross-project tests.
func sendAuthzDM(t *testing.T, srv *Server, sender *store.Agent, convID, projectID, msgText string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             msgText,
		Type:            "instruction",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: projectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// assertZeroSideEffects verifies that no message rows were created, no
// dispatch calls were made, and the HTTP response is non-2xx.
func assertZeroSideEffects(t *testing.T, s store.Store, senderID string, dispatcher *recordingDispatcher, rr *httptest.ResponseRecorder) {
	t.Helper()

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"denied request must return 403; body: %s", rr.Body.String())

	// Zero message rows.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: senderID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "denied request must produce zero message rows")

	// Zero dispatch calls.
	assert.Empty(t, dispatcher.getCalls(), "denied request must produce zero dispatch calls")
}

// assertDenialDetails checks that the error response contains the expected
// typed denial code and message_denied error code.
func assertDenialDetails(t *testing.T, rr *httptest.ResponseRecorder, expectedDenialCode string) {
	t.Helper()

	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp),
		"response body must be valid JSON: %s", rr.Body.String())
	assert.Equal(t, ErrCodeMessageDenied, resp.Error.Code,
		"error code must be message_denied")
	if expectedDenialCode != "" {
		assert.Equal(t, expectedDenialCode, resp.Error.Details["reason"],
			"denial reason must carry the typed code")
	}
}

// ---------------------------------------------------------------------------
// Test 1: Same-project — target mode revoked to none → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_TargetModeNone_Denied(t *testing.T) {
	srv, s, project, sender, target, convID, dispatcher := authzDMSetup(t,
		store.MessageModeProject, store.MessageModeProject)

	// Positive: DM succeeds before revocation.
	rr := sendAuthzDM(t, srv, sender, convID, project.ID, "before revocation")
	require.Equal(t, http.StatusOK, rr.Code,
		"DM must succeed before revocation; body: %s", rr.Body.String())

	// Revoke target mode to none.
	target.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(context.Background(), target))

	// Clear dispatch log for the side-effect assertion.
	dispatcher.mu.Lock()
	dispatcher.calls = nil
	dispatcher.mu.Unlock()

	// Now the DM must be denied.
	rr = sendAuthzDM(t, srv, sender, convID, project.ID, "after target revocation")
	assert.Equal(t, http.StatusForbidden, rr.Code,
		"DM must be denied after target mode revocation; body: %s", rr.Body.String())

	// Zero new message rows for the denied send.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	// Should have exactly 1 message (the pre-revocation send).
	found := 0
	for _, m := range msgs.Items {
		if m.Msg == "after target revocation" {
			found++
		}
	}
	assert.Equal(t, 0, found, "denied send must produce zero message rows")

	// Zero dispatch calls for the denied send.
	assert.Empty(t, dispatcher.getCalls(),
		"denied send must produce zero dispatch calls")

	// Typed denial details.
	assertDenialDetails(t, rr, "")
}

// ---------------------------------------------------------------------------
// Test 2: Same-project — sender mode revoked to none → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_SenderModeNone_Denied(t *testing.T) {
	srv, s, project, sender, _, convID, dispatcher := authzDMSetup(t,
		store.MessageModeProject, store.MessageModeProject)

	// Revoke sender mode to none.
	sender.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(context.Background(), sender))

	rr := sendAuthzDM(t, srv, sender, convID, project.ID, "sender mode none")
	assertZeroSideEffects(t, s, sender.ID, dispatcher, rr)
	assertDenialDetails(t, rr, "")
}

// ---------------------------------------------------------------------------
// Test 3: Same-project — lineage mode → agent-to-agent DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_LineageMode_Denied(t *testing.T) {
	srv, s, project, sender, target, convID, dispatcher := authzDMSetup(t,
		store.MessageModeLineage, store.MessageModeProject)
	_ = target // unused but kept for clarity

	rr := sendAuthzDM(t, srv, sender, convID, project.ID, "lineage mode denied")
	assertZeroSideEffects(t, s, sender.ID, dispatcher, rr)
}

// ---------------------------------------------------------------------------
// Test 4: Same-project — branch mode without parent/child → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_BranchModeNonParentChild_Denied(t *testing.T) {
	srv, s, project, sender, _, convID, dispatcher := authzDMSetup(t,
		store.MessageModeBranch, store.MessageModeBranch)

	// Neither agent is the other's parent → denied.
	rr := sendAuthzDM(t, srv, sender, convID, project.ID, "branch no relation")
	assertZeroSideEffects(t, s, sender.ID, dispatcher, rr)
}

// ---------------------------------------------------------------------------
// Test 5: Positive — same-project project mode → DM allowed
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_SameProjectProject_Allowed(t *testing.T) {
	srv, s, project, sender, target, convID, dispatcher := authzDMSetup(t,
		store.MessageModeProject, store.MessageModeProject)

	rr := sendAuthzDM(t, srv, sender, convID, project.ID, "project mode allowed")
	require.Equal(t, http.StatusOK, rr.Code,
		"same-project project-mode DM must succeed; body: %s", rr.Body.String())

	// Message persisted.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var found bool
	for _, m := range msgs.Items {
		if m.Msg == "project mode allowed" {
			found = true
			break
		}
	}
	assert.True(t, found, "allowed DM must be persisted")

	// Dispatch called.
	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call")
	assert.Equal(t, target.ID, calls[0].Agent.ID, "dispatch must target the correct agent")
}

// ---------------------------------------------------------------------------
// Test 6: Positive — same-project hub/project combination → DM allowed
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_SameProjectHubProject_Allowed(t *testing.T) {
	srv, _, project, sender, _, convID, _ := authzDMSetup(t,
		store.MessageModeHub, store.MessageModeProject)

	rr := sendAuthzDM(t, srv, sender, convID, project.ID, "hub to project allowed")
	require.Equal(t, http.StatusOK, rr.Code,
		"hub→project same-project DM must succeed; body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// Test 7: Cross-project — target mode revoked to none after setup → denied
// Mirrors test 1 (same-project) for the cross-project path.
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_CrossProject_TargetModeRevoked_Denied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerA := &store.User{
		ID:      tid("authz-cprev-owner-a"),
		Email:   "owner-cprev-a@authz.test",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	projectA := &store.Project{
		ID:   tid("authz-cprev-project-a"),
		Name: "cprev-project-a",
		Slug: "cprev-project-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:   tid("authz-cprev-project-b"),
		Name: "cprev-project-b",
		Slug: "cprev-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	brokerID := tid("authz-cprev-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-cprev-broker",
		Slug:   "authz-cprev-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-cprev-agent-a"),
		Name:            "cprev-agent-a",
		Slug:            "cprev-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-cprev-agent-b"),
		Name:            "cprev-agent-b",
		Slug:            "cprev-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableCPM(t, srv, s)

	// Positive: DM succeeds before revocation.
	rr := sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "before cross-project revocation")
	require.Equal(t, http.StatusOK, rr.Code,
		"cross-project DM must succeed before revocation; body: %s", rr.Body.String())

	// Revoke target mode to none.
	agentB.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(ctx, agentB))

	// Clear dispatch log.
	dispatcher.mu.Lock()
	dispatcher.calls = nil
	dispatcher.mu.Unlock()

	// Now the DM must be denied. Mode=none is checked before the cross-project
	// path, so the denial code is mode_none (from mapReasonToCode), not the
	// cross-project-specific code.
	rr = sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "after cross-project revocation")
	assert.Equal(t, http.StatusForbidden, rr.Code,
		"cross-project DM must be denied after target mode revocation; body: %s", rr.Body.String())
	assertDenialDetails(t, rr, "mode_none")

	// Zero new message rows for the denied send.
	msgs, listErr := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 100})
	require.NoError(t, listErr)
	for _, m := range msgs.Items {
		assert.NotEqual(t, "after cross-project revocation", m.Msg,
			"denied send must produce zero new message rows")
	}
	assert.Empty(t, dispatcher.getCalls(), "denied send must produce zero dispatch calls")
}

// ---------------------------------------------------------------------------
// Test 8: Cross-project — hub flag disabled → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_CrossProject_HubFlagDisabled_Denied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two projects.
	projectA := &store.Project{
		ID:   tid("authz-cp-project-a"),
		Name: "project-a",
		Slug: "project-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:                  tid("authz-cp-project-b"),
		Name:                "project-b",
		Slug:                "project-b",
		CrossProjectInbound: store.CrossProjectInboundAny,
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	ownerA := &store.User{
		ID:      tid("authz-cp-owner-a"),
		Email:   "owner-a@authz.test",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))

	brokerID := tid("authz-cp-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-cp-broker",
		Slug:   "authz-cp-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-cp-agent-a"),
		Name:            "cp-agent-a",
		Slug:            "cp-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-cp-agent-b"),
		Name:            "cp-agent-b",
		Slug:            "cp-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{ownerA.ID}, // same owner for simplicity
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Do NOT enable CPM — hub flag remains disabled.

	rr := sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "cross project disabled")
	assertZeroSideEffects(t, s, agentA.ID, dispatcher, rr)
	assertDenialDetails(t, rr, string(MessageDenialCrossProjectDisabled))
}

// ---------------------------------------------------------------------------
// Test 9: Cross-project — inbound policy none → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_CrossProject_InboundNone_Denied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerA := &store.User{
		ID:      tid("authz-inb-owner-a"),
		Email:   "owner-inb-a@authz.test",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))

	projectA := &store.Project{
		ID:   tid("authz-inb-project-a"),
		Name: "inb-project-a",
		Slug: "inb-project-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:                  tid("authz-inb-project-b"),
		Name:                "inb-project-b",
		Slug:                "inb-project-b",
		CrossProjectInbound: store.CrossProjectInboundNone,
	}
	require.NoError(t, s.CreateProject(ctx, projectB))

	brokerID := tid("authz-inb-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-inb-broker",
		Slug:   "authz-inb-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-inb-agent-a"),
		Name:            "inb-agent-a",
		Slug:            "inb-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-inb-agent-b"),
		Name:            "inb-agent-b",
		Slug:            "inb-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable CPM at hub level.
	enableCPM(t, srv, s)

	rr := sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "inbound none")
	assertZeroSideEffects(t, s, agentA.ID, dispatcher, rr)
	assertDenialDetails(t, rr, string(MessageDenialCrossProjectInboundNone))
}

// ---------------------------------------------------------------------------
// Test 10: Cross-project — members policy, origin not member → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_CrossProject_OriginNotMember_Denied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerA := &store.User{
		ID:      tid("authz-mem-owner-a"),
		Email:   "owner-mem-a@authz.test",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	projectA := &store.Project{
		ID:   tid("authz-mem-project-a"),
		Name: "mem-project-a",
		Slug: "mem-project-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:   tid("authz-mem-project-b"),
		Name: "mem-project-b",
		Slug: "mem-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	srv.createProjectMembersGroup(ctx, projectB)
	// Set inbound to "members".
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundMembers, 1)
	require.NoError(t, err)

	brokerID := tid("authz-mem-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-mem-broker",
		Slug:   "authz-mem-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-mem-agent-a"),
		Name:            "mem-agent-a",
		Slug:            "mem-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-mem-agent-b"),
		Name:            "mem-agent-b",
		Slug:            "mem-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable CPM at hub level.
	enableCPM(t, srv, s)

	// ownerA is NOT a member of projectB → should be denied.
	rr := sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "not a member")
	assertZeroSideEffects(t, s, agentA.ID, dispatcher, rr)
	assertDenialDetails(t, rr, string(MessageDenialCrossProjectNotMember))
}

// ---------------------------------------------------------------------------
// Test 11: Positive — cross-project allowed foreign DM succeeds
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_CrossProject_Allowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerA := &store.User{
		ID:      tid("authz-ok-owner-a"),
		Email:   "owner-ok-a@authz.test",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	projectA := &store.Project{
		ID:        tid("authz-ok-project-a"),
		Name:      "ok-project-a",
		Slug:      "ok-project-a",
		OwnerID:   ownerA.ID,
		CreatedBy: ownerA.ID,
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:   tid("authz-ok-project-b"),
		Name: "ok-project-b",
		Slug: "ok-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	// Set inbound to "any".
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	brokerID := tid("authz-ok-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-ok-broker",
		Slug:   "authz-ok-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-ok-agent-a"),
		Name:            "ok-agent-a",
		Slug:            "ok-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-ok-agent-b"),
		Name:            "ok-agent-b",
		Slug:            "ok-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable CPM at hub level.
	enableCPM(t, srv, s)

	rr := sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "cross project allowed")
	require.Equal(t, http.StatusOK, rr.Code,
		"cross-project DM with inbound=any must succeed; body: %s", rr.Body.String())

	// Verify message persisted.
	msgs, listErr := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, listErr)
	var found bool
	for _, m := range msgs.Items {
		if m.Msg == "cross project allowed" {
			found = true
			break
		}
	}
	assert.True(t, found, "allowed cross-project DM must be persisted")

	// Verify dispatch.
	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls))
	assert.Equal(t, agentB.ID, calls[0].Agent.ID)
}

// ---------------------------------------------------------------------------
// Test 12: Forged sender — agent uses a different agent's conversation
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_ForgedSender_CannotRedirect(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("authz-forge-project"),
		Name: "forge-project",
		Slug: "forge-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	brokerID := tid("authz-forge-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-forge-broker",
		Slug:   "authz-forge-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-forge-agent-a"),
		Name:            "forge-agent-a",
		Slug:            "forge-agent-a",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-forge-agent-b"),
		Name:            "forge-agent-b",
		Slug:            "forge-agent-b",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	agentC := &store.Agent{
		ID:              tid("authz-forge-agent-c"),
		Name:            "forge-agent-c",
		Slug:            "forge-agent-c",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agentC))

	// Create a DM between B and C.
	dmKey, err := messages.DMConversationKey("agent", agentB.ID, "agent", agentC.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Agent A tries to send to B↔C's conversation via conversation_ref.
	// S4 DM-key check should reject this (sender not named in the DM key).
	rr := sendAuthzDM(t, srv, agentA, conv.ID, project.ID, "forged sender")
	assert.NotEqual(t, http.StatusOK, rr.Code,
		"forged sender must not succeed; body: %s", rr.Body.String())

	// Zero side effects.
	msgs, listErr := s.ListMessages(ctx, store.MessageFilter{SenderID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, listErr)
	assert.Empty(t, msgs.Items, "forged sender must produce zero message rows")
	assert.Empty(t, dispatcher.getCalls(), "forged sender must produce zero dispatch calls")
}

// ---------------------------------------------------------------------------
// Test 13: Cross-project — sender mode not hub → DM denied
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_CrossProject_SenderModeNotHub_Denied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerA := &store.User{
		ID:      tid("authz-smode-owner-a"),
		Email:   "owner-smode-a@authz.test",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	projectA := &store.Project{
		ID:   tid("authz-smode-project-a"),
		Name: "smode-project-a",
		Slug: "smode-project-a",
	}
	require.NoError(t, s.CreateProject(ctx, projectA))

	projectB := &store.Project{
		ID:   tid("authz-smode-project-b"),
		Name: "smode-project-b",
		Slug: "smode-project-b",
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectB.ID, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	brokerID := tid("authz-smode-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "authz-smode-broker",
		Slug:   "authz-smode-broker",
		Status: store.BrokerStatusOnline,
	}))

	agentA := &store.Agent{
		ID:              tid("authz-smode-agent-a"),
		Name:            "smode-agent-a",
		Slug:            "smode-agent-a",
		ProjectID:       projectA.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject, // NOT hub
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("authz-smode-agent-b"),
		Name:            "smode-agent-b",
		Slug:            "smode-agent-b",
		ProjectID:       projectB.ID,
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{ownerA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create DM conversation.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	enableCPM(t, srv, s)

	rr := sendAuthzDM(t, srv, agentA, conv.ID, projectA.ID, "sender not hub mode")
	assertZeroSideEffects(t, s, agentA.ID, dispatcher, rr)
	assertDenialDetails(t, rr, string(MessageDenialCrossProjectSenderMode))
}

// ---------------------------------------------------------------------------
// conversation_id tests
//
// The conversation_id input (without conversation_ref) requires a recipient
// per the pre-S3 guard. With recipientID set, S5 derivation is skipped, so
// targetAgent stays nil and the request routes to the user delivery path —
// NOT deliveryAgentDM. The S7 gate fires only on deliveryAgentDM. These
// tests verify that conversation_id cannot produce a silently unguarded
// agent-to-agent message delivery.
// ---------------------------------------------------------------------------

// sendAuthzDMViaConversationID sends an agent-to-agent DM through the HTTP
// handler using conversation_id (not conversation_ref) alongside a recipient.
func sendAuthzDMViaConversationID(t *testing.T, srv *Server, sender, target *store.Agent, convID, projectID, msgText string) *httptest.ResponseRecorder {
	t.Helper()

	reqBody, err := json.Marshal(OutboundMessageRequest{
		ConversationID: convID,
		RecipientID:    target.ID,
		Recipient:      "agent:" + target.Slug,
		Msg:            msgText,
		Type:           "instruction",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+sender.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: projectID,
		Ancestry:  sender.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// ---------------------------------------------------------------------------
// Test 14: conversation_id — target mode none → denied
//
// With conversation_id + recipientID, the request bypasses S5 derivation
// (recipientID is already set), so targetAgent stays nil and the delivery
// path is user-direct rather than deliveryAgentDM. S7 case (b) detects
// that the DM conversation is between two agents and applies the
// authorization gate, preventing the message from being persisted.
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_ConversationID_TargetModeNone_Denied(t *testing.T) {
	srv, s, project, sender, target, convID, dispatcher := authzDMSetup(t,
		store.MessageModeProject, store.MessageModeNone)

	rr := sendAuthzDMViaConversationID(t, srv, sender, target, convID, project.ID, "conv_id mode none")
	assertZeroSideEffects(t, s, sender.ID, dispatcher, rr)
	assertDenialDetails(t, rr, "")
}

// ---------------------------------------------------------------------------
// Test 15: conversation_id — same-project project mode allowed → DM delivered
//
// When both agents are in project mode and conversation_id is used with a
// recipient, the request enters the user-direct delivery path (since
// targetAgent is nil in S5 when recipientID is set). This test verifies
// the conversation_id input reaches the handler and persists when authorized.
// ---------------------------------------------------------------------------

func TestOutboundDMAuthz_ConversationID_Allowed_Persisted(t *testing.T) {
	srv, s, project, sender, target, convID, _ := authzDMSetup(t,
		store.MessageModeProject, store.MessageModeProject)

	rr := sendAuthzDMViaConversationID(t, srv, sender, target, convID, project.ID, "conv_id allowed")

	// The request should succeed (user-direct path persists the message).
	require.Equal(t, http.StatusOK, rr.Code,
		"conversation_id with project-mode target must succeed; body: %s", rr.Body.String())

	// Verify the message was persisted.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	var found bool
	for _, m := range msgs.Items {
		if m.Msg == "conv_id allowed" {
			found = true
			break
		}
	}
	assert.True(t, found, "conversation_id allowed DM must be persisted")
}
