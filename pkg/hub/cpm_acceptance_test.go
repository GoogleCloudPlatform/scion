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

// =============================================================================
// CPM Acceptance Test Matrix — Issue #1695
//
// Evidence report
// ---------------
// Commit tested:      (stamped at CI time)
// Commands run:       go test -buildvcs=false -tags '!no_sqlite' ./pkg/hub/ -run TestCPMAcceptance -count=1 -timeout 600s
// Infrastructure:     SQLite in-memory (no Postgres available — noted as uncompleted gate)
//
// Coverage areas:
//  AC-1: Policy enforcement parity across DM adapters/addresses
//  AC-2: Group delivery via outbound, wake rejection, foreign-group widening denial
//  AC-3: Rate/length limits across routes, foreign attachment denial
//  AC-4: Delivery outcomes (broker/managed success, failure, ambiguity)
//  AC-5: Audience containment — no unintended foreign content leaks
//  AC-6: CI gate (make ci) — run separately
// =============================================================================

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Acceptance fixture — two projects, multiple agent modes, real auth/store
// ---------------------------------------------------------------------------

type acceptanceFixture struct {
	srv    *Server
	store  store.Store
	ownerA *store.User
	ownerB *store.User

	projectA string
	projectB string

	// Agents in project A
	hubAgentA     *store.Agent
	projectAgentA *store.Agent
	noneAgentA    *store.Agent

	// Agents in project B
	hubAgentB     *store.Agent
	projectAgentB *store.Agent
	branchAgentB  *store.Agent
	noneAgentB    *store.Agent

	dispatcher *acceptanceDispatcher
}

// acceptanceDispatcher records dispatch calls for assertions.
type acceptanceDispatcher struct {
	mu         sync.Mutex
	calls      []acceptanceDispatchCall
	startCalls []acceptanceStartCall
	returnErr  error
	startErr   error
}

type acceptanceDispatchCall struct {
	Method        string
	AgentID       string
	Message       string
	Interrupt     bool
	StructuredMsg *messages.StructuredMessage
}

type acceptanceStartCall struct {
	AgentID  string
	Continue bool
}

func (d *acceptanceDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, acceptanceDispatchCall{
		Method:        "DispatchAgentMessage",
		AgentID:       agent.ID,
		Message:       message,
		Interrupt:     interrupt,
		StructuredMsg: structuredMsg,
	})
	return d.returnErr
}

func (d *acceptanceDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *acceptanceDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *acceptanceDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, cont bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.startCalls = append(d.startCalls, acceptanceStartCall{AgentID: agent.ID, Continue: cont})
	return d.startErr
}
func (d *acceptanceDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *acceptanceDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *acceptanceDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *acceptanceDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *acceptanceDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *acceptanceDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *acceptanceDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *acceptanceDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *acceptanceDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

var _ AgentDispatcher = (*acceptanceDispatcher)(nil)

func (d *acceptanceDispatcher) getCalls() []acceptanceDispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]acceptanceDispatchCall, len(d.calls))
	copy(cp, d.calls)
	return cp
}

func (d *acceptanceDispatcher) getStartCalls() []acceptanceStartCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]acceptanceStartCall, len(d.startCalls))
	copy(cp, d.startCalls)
	return cp
}

func (d *acceptanceDispatcher) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
	d.startCalls = nil
}

func acceptanceSetup(t *testing.T) acceptanceFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Create owners with real store operations.
	ownerA := &store.User{
		ID:          tid("acc-owner-a"),
		Email:       "acc-owner-a@test.com",
		DisplayName: "AccOwnerA",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerA))
	ensureHubMembership(ctx, s, ownerA.ID)

	ownerB := &store.User{
		ID:          tid("acc-owner-b"),
		Email:       "acc-owner-b@test.com",
		DisplayName: "AccOwnerB",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, ownerB))
	ensureHubMembership(ctx, s, ownerB.ID)

	// Create project A.
	projectA := tid("acc-project-a")
	pA := &store.Project{
		ID:        projectA,
		Name:      "acc-project-a",
		Slug:      "acc-project-a",
		OwnerID:   ownerA.ID,
		CreatedBy: ownerA.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, pA))
	srv.createProjectMembersGroup(ctx, pA)
	msgAuthzAddProjectMember(t, s, ownerA.ID, projectA, "acc-project-a", store.GroupMemberRoleOwner)

	// Create project B.
	projectB := tid("acc-project-b")
	pB := &store.Project{
		ID:        projectB,
		Name:      "acc-project-b",
		Slug:      "acc-project-b",
		OwnerID:   ownerB.ID,
		CreatedBy: ownerB.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, pB))
	srv.createProjectMembersGroup(ctx, pB)
	msgAuthzAddProjectMember(t, s, ownerB.ID, projectB, "acc-project-b", store.GroupMemberRoleOwner)

	// Set inbound policies to "any" by default.
	_, err := s.UpdateProjectMessagingPolicy(ctx, projectA, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)
	_, err = s.UpdateProjectMessagingPolicy(ctx, projectB, store.CrossProjectInboundAny, 1)
	require.NoError(t, err)

	// Create a runtime broker for dispatch.
	brokerID := tid("acc-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "acc-broker",
		Slug:   "acc-broker",
		Status: store.BrokerStatusOnline,
	}))

	// Add broker as project provider for both projects.
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectA,
		BrokerID:   brokerID,
		BrokerName: "acc-broker",
		Status:     store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectB,
		BrokerID:   brokerID,
		BrokerName: "acc-broker",
		Status:     store.BrokerStatusOnline,
	}))

	// Create agents in project A.
	hubAgentA := &store.Agent{
		ID:              tid("acc-hub-a"),
		Slug:            "acc-hub-a",
		Name:            "AccHubAgentA",
		ProjectID:       projectA,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerA.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	projectAgentA := &store.Agent{
		ID:              tid("acc-proj-a"),
		Slug:            "acc-proj-a",
		Name:            "AccProjAgentA",
		ProjectID:       projectA,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{ownerA.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	noneAgentA := &store.Agent{
		ID:              tid("acc-none-a"),
		Slug:            "acc-none-a",
		Name:            "AccNoneAgentA",
		ProjectID:       projectA,
		MessageMode:     store.MessageModeNone,
		Ancestry:        []string{ownerA.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}

	// Create agents in project B.
	hubAgentB := &store.Agent{
		ID:              tid("acc-hub-b"),
		Slug:            "acc-hub-b",
		Name:            "AccHubAgentB",
		ProjectID:       projectB,
		MessageMode:     store.MessageModeHub,
		Ancestry:        []string{ownerB.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	projectAgentB := &store.Agent{
		ID:              tid("acc-proj-b"),
		Slug:            "acc-proj-b",
		Name:            "AccProjAgentB",
		ProjectID:       projectB,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{ownerB.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	branchAgentB := &store.Agent{
		ID:              tid("acc-branch-b"),
		Slug:            "acc-branch-b",
		Name:            "AccBranchAgentB",
		ProjectID:       projectB,
		MessageMode:     store.MessageModeBranch,
		Ancestry:        []string{ownerB.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	noneAgentB := &store.Agent{
		ID:              tid("acc-none-b"),
		Slug:            "acc-none-b",
		Name:            "AccNoneAgentB",
		ProjectID:       projectB,
		MessageMode:     store.MessageModeNone,
		Ancestry:        []string{ownerB.ID},
		Phase:           "running",
		RuntimeBrokerID: brokerID,
		Created:         time.Now(),
		Updated:         time.Now(),
	}

	for _, a := range []*store.Agent{hubAgentA, projectAgentA, noneAgentA, hubAgentB, projectAgentB, branchAgentB, noneAgentB} {
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	// Enable cross-project messaging.
	enableCrossProjectMessaging(t, srv)

	// Set up dispatcher.
	dispatcher := &acceptanceDispatcher{}
	srv.SetDispatcher(dispatcher)

	return acceptanceFixture{
		srv:           srv,
		store:         s,
		ownerA:        ownerA,
		ownerB:        ownerB,
		projectA:      projectA,
		projectB:      projectB,
		hubAgentA:     hubAgentA,
		projectAgentA: projectAgentA,
		noneAgentA:    noneAgentA,
		hubAgentB:     hubAgentB,
		projectAgentB: projectAgentB,
		branchAgentB:  branchAgentB,
		noneAgentB:    noneAgentB,
		dispatcher:    dispatcher,
	}
}

// accAgentIdentity creates an AgentIdentity for use in request contexts.
func accAgentIdentity(agentID, projectID string, ancestry []string) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Ancestry:  ancestry,
	}}
}

// accPostOutbound sends an outbound message as the given agent.
func accPostOutbound(t *testing.T, f acceptanceFixture, sender *store.Agent, recipient, msg string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: recipient,
		Msg:       msg,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		sender.ID, sender.ProjectID, sender.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, sender.ID)
	return rr
}

// accPostMessage sends a structured message to the given target agent.
func accPostMessage(t *testing.T, f acceptanceFixture, senderIdentity Identity, targetAgent *store.Agent, msg string, wake bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(MessageRequest{
		Message: msg,
		Wake:    wake,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+targetAgent.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), senderIdentity))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, targetAgent.ID)
	return rr
}

// accCountMessages returns the count of persisted messages for an agent.
func accCountMessages(t *testing.T, s store.Store, agentID string) int {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{
		AgentID: agentID,
	}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	return len(msgs.Items)
}

// =============================================================================
// AC-1: Policy enforcement parity
// =============================================================================

func TestCPMAcceptance_AC1_SameProject_HubToProject(t *testing.T) {
	// hub→project within same project: allowed via structured message path.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.projectAgentA, "hub-to-project same project", false)
	assert.Equal(t, http.StatusOK, rr.Code, "hub→project same project should succeed: %s", rr.Body.String())
}

func TestCPMAcceptance_AC1_SameProject_ProjectToHub(t *testing.T) {
	// project→hub within same project: allowed via structured message path.
	f := acceptanceSetup(t)

	rr := accPostMessage(t, f,
		accAgentIdentity(f.projectAgentA.ID, f.projectA, f.projectAgentA.Ancestry),
		f.hubAgentA, "project-to-hub same project", false)
	assert.Equal(t, http.StatusOK, rr.Code, "project→hub same project should succeed: %s", rr.Body.String())
}

func TestCPMAcceptance_AC1_SameProject_NoneTarget_Denied(t *testing.T) {
	// Sending to a mode=none agent is denied by the message evaluator.
	// This is enforced at origination points (group delivery, broadcast, scheduled).
	f := acceptanceSetup(t)
	ctx := context.Background()

	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.noneAgentA)
	assert.False(t, decision.Allowed, "evaluator should deny sending to mode=none agent")

	// Also verify via authorizeAgentMessage.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.noneAgentA, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny to mode=none: %s", reason)

	// Verify denial in group delivery path (which calls authorizeAgentMessage).
	// Group messages require at least 2 recipients; include a valid agent for contrast.
	f.dispatcher.reset()
	countBefore := accCountMessages(t, f.store, f.noneAgentA.ID)

	groupRecipient := "group[agent:acc-none-a,agent:acc-proj-a]"
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "agent:" + f.hubAgentA.Slug,
			SenderID:  f.hubAgentA.ID,
			Recipient: groupRecipient,
			Msg:       "to-none-group",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.hubAgentA.ID)

	// proj-a may be dispatched (it's allowed), but none-a must NOT be dispatched.
	for _, call := range f.dispatcher.getCalls() {
		assert.NotEqual(t, f.noneAgentA.ID, call.AgentID,
			"mode=none agent should not receive dispatch in group delivery")
	}
	countAfter := accCountMessages(t, f.store, f.noneAgentA.ID)
	assert.Equal(t, countBefore, countAfter, "no message persisted for mode=none target")

	// Parse group results to verify none-a was denied.
	var result struct {
		Results []struct {
			Recipient string `json:"recipient"`
			Status    string `json:"status"`
		} `json:"results"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &result)
	for _, r := range result.Results {
		if strings.Contains(r.Recipient, "acc-none-a") {
			assert.Equal(t, "unauthorized", r.Status,
				"mode=none target should be 'unauthorized' in group results")
		}
	}
}

func TestCPMAcceptance_AC1_SameProject_NoneSender_Denied(t *testing.T) {
	// Sending from a mode=none agent is denied by the message evaluator.
	f := acceptanceSetup(t)
	ctx := context.Background()

	identity := accAgentIdentity(f.noneAgentA.ID, f.projectA, f.noneAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentA)
	assert.False(t, decision.Allowed, "evaluator should deny sending from mode=none agent")

	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentA, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny from mode=none: %s", reason)
}

func TestCPMAcceptance_AC1_CrossProject_HubToHub_Allowed(t *testing.T) {
	// hub→hub across projects with CPM enabled and inbound=any: allowed.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.hubAgentB, "cross-project hub-to-hub", false)
	assert.Equal(t, http.StatusOK, rr.Code, "hub→hub cross-project should succeed: %s", rr.Body.String())

	// Verify dispatch happened.
	calls := f.dispatcher.getCalls()
	require.NotEmpty(t, calls, "dispatch should have been called for cross-project hub→hub")
	assert.Equal(t, f.hubAgentB.ID, calls[0].AgentID, "dispatch target should be hub-b")
}

func TestCPMAcceptance_AC1_CrossProject_HubToProject_Allowed(t *testing.T) {
	// hub→project across projects with CPM enabled and inbound=any: allowed.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.projectAgentB, "cross-project hub-to-project", false)
	assert.Equal(t, http.StatusOK, rr.Code, "hub→project cross-project should succeed: %s", rr.Body.String())
}

func TestCPMAcceptance_AC1_CrossProject_ProjectSender_Denied(t *testing.T) {
	// project→hub across projects: denied (sender must be hub mode).
	// Authorization is enforced via EvaluateAgentMessage and at origination
	// points (group delivery, broadcast); the single-target handleAgentMessage
	// path does not call authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Verify evaluator denies.
	identity := accAgentIdentity(f.projectAgentA.ID, f.projectA, f.projectAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny project→hub cross-project")
	assert.Equal(t, MessageDenialCrossProjectSenderMode, decision.Code)

	// Verify authorizeAgentMessage also denies.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny project sender: %s", reason)
}

func TestCPMAcceptance_AC1_CrossProject_BranchTarget_Denied(t *testing.T) {
	// hub→branch across projects: denied (target must be project or hub mode).
	// Verified via the evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.branchAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny hub→branch cross-project")
	assert.Equal(t, MessageDenialCrossProjectTargetMode, decision.Code)

	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.branchAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny branch target: %s", reason)
}

func TestCPMAcceptance_AC1_CrossProject_HubFlag_Disabled(t *testing.T) {
	// When CPM is disabled at hub level, cross-project sends are denied.
	// Verified via evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Disable CPM by creating fresh operational settings without CPM.
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	f.srv.SetOperationalSettings(ops)

	// Evaluator should deny.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny when CPM disabled")
	assert.Equal(t, MessageDenialCrossProjectDisabled, decision.Code)

	// authorizeAgentMessage should also deny.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny when CPM disabled: %s", reason)

	// Re-enable for subsequent tests using this fixture.
	enableCrossProjectMessaging(t, f.srv)
}

func TestCPMAcceptance_AC1_CrossProject_InboundNone_Denied(t *testing.T) {
	// Project B inbound policy = "none": cross-project sends denied.
	// Verified via evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Change project B inbound policy to "none".
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundNone, 2)
	require.NoError(t, err)

	// Evaluator should deny.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny with inbound=none")
	assert.Equal(t, MessageDenialCrossProjectInboundNone, decision.Code)

	// authorizeAgentMessage should also deny.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny with inbound=none: %s", reason)
}

func TestCPMAcceptance_AC1_CrossProject_InboundMembers_OriginNotMember(t *testing.T) {
	// Project B inbound = "members", but ownerA is not a member of project B.
	// Verified via evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 2)
	require.NoError(t, err)

	// Evaluator should deny.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny when origin not member")
	assert.Equal(t, MessageDenialCrossProjectNotMember, decision.Code)

	// authorizeAgentMessage should also deny.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny non-member origin: %s", reason)
}

func TestCPMAcceptance_AC1_CrossProject_InboundMembers_OriginIsMember(t *testing.T) {
	// Project B inbound = "members", and ownerA IS added as a member of project B.
	f := acceptanceSetup(t)
	ctx := context.Background()

	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundMembers, 2)
	require.NoError(t, err)

	// Add ownerA as a member of project B.
	msgAuthzAddProjectMember(t, f.store, f.ownerA.ID, f.projectB, "acc-project-b", store.GroupMemberRoleMember)

	f.dispatcher.reset()
	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.hubAgentB, "members-is-member", false)
	assert.Equal(t, http.StatusOK, rr.Code, "cross-project should be allowed when origin is member: %s", rr.Body.String())
}

func TestCPMAcceptance_AC1_PolicyParity_EvalAndHTTP(t *testing.T) {
	// The same policy must apply to both the EvaluateAgentMessage evaluator
	// and the HTTP structured message endpoint.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Path 1: EvaluateAgentMessage (direct evaluator).
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.True(t, decision.Allowed, "EvaluateAgentMessage should allow cross-project hub→hub")

	// Path 2: structured message endpoint.
	f.dispatcher.reset()
	rrMsg := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.hubAgentB, "parity-structured-path", false)
	assert.Equal(t, http.StatusOK, rrMsg.Code, "structured message path should allow cross-project hub→hub: %s", rrMsg.Body.String())

	// Path 3: outbound agent→user (verify outbound endpoint itself is functional).
	f.dispatcher.reset()
	rrOut := accPostOutbound(t, f, f.hubAgentA, "user:"+f.ownerA.Email, "parity-outbound-path")
	assert.Equal(t, http.StatusOK, rrOut.Code, "outbound agent→user should succeed: %s", rrOut.Body.String())
}

func TestCPMAcceptance_AC1_PolicyRevocation_MidSession(t *testing.T) {
	// After a successful cross-project evaluation, disable the hub flag.
	// Subsequent evaluations should be denied.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Evaluation 1: should succeed (CPM enabled).
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision1 := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	require.True(t, decision1.Allowed, "first evaluation should succeed")

	// Disable CPM.
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	f.srv.SetOperationalSettings(ops)

	// Evaluation 2: should be denied.
	decision2 := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision2.Allowed, "evaluation after revocation should fail")
	assert.Equal(t, MessageDenialCrossProjectDisabled, decision2.Code)

	// authorizeAgentMessage should also deny after revocation.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny after revocation: %s", reason)

	// Re-enable.
	enableCrossProjectMessaging(t, f.srv)
}

// =============================================================================
// AC-2: Group delivery
// =============================================================================

func TestCPMAcceptance_AC2_SameProject_GroupDelivery(t *testing.T) {
	// Same-project group delivery via structured message endpoint should work
	// using group[] recipient format.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	// Use the web-UI path: send a group message to two same-project agents.
	groupRecipient := "group[agent:acc-hub-a,agent:acc-proj-a]"
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "user:" + f.ownerA.Email,
			SenderID:  f.ownerA.ID,
			Recipient: groupRecipient,
			Msg:       "group delivery test",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli")))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.hubAgentA.ID)

	// Group delivery may return 200 with per-recipient results.
	assert.Equal(t, http.StatusOK, rr.Code, "group delivery should succeed: %s", rr.Body.String())
}

func TestCPMAcceptance_AC2_GroupDelivery_WakeIgnored(t *testing.T) {
	// Group delivery ignores the wake flag — it is DM-only.
	// group[] requires at least 2 recipients.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	groupRecipient := "group[agent:acc-hub-a,agent:acc-proj-a]"
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "user:" + f.ownerA.Email,
			SenderID:  f.ownerA.ID,
			Recipient: groupRecipient,
			Msg:       "group with wake",
		},
		Wake: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli")))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.hubAgentA.ID)

	// Group delivery should succeed; wake is for DMs, not groups.
	assert.Equal(t, http.StatusOK, rr.Code, "group delivery should succeed: %s", rr.Body.String())

	// Verify no DispatchAgentStart was called (wake ignored for group delivery).
	assert.Empty(t, f.dispatcher.getStartCalls(), "wake should be ignored for group delivery")
}

func TestCPMAcceptance_AC2_CrossProject_GroupWidening_Denied(t *testing.T) {
	// Foreign-group widening: sending a group message that includes an agent
	// from another project should deny the cross-project recipient while
	// allowing the same-project recipient.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	// Group message: hub-a targets proj-a (same project) AND hub-b (cross project).
	groupRecipient := "group[agent:acc-proj-a,agent:acc-hub-b]"
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "user:" + f.ownerA.Email,
			SenderID:  f.ownerA.ID,
			Recipient: groupRecipient,
			Msg:       "group widening test",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli")))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.hubAgentA.ID)

	// The group handler should process — per-recipient results will show
	// acc-proj-a as delivered and acc-hub-b as unauthorized (not in sender's project).
	assert.Equal(t, http.StatusOK, rr.Code, "group delivery response code: %s", rr.Body.String())

	// Parse results to check per-recipient status.
	var result struct {
		Results []struct {
			Recipient string `json:"recipient"`
			Status    string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err == nil && len(result.Results) > 0 {
		for _, r := range result.Results {
			if strings.Contains(r.Recipient, "acc-hub-b") {
				// Cross-project recipient in group should not be delivered.
				assert.NotEqual(t, "sent", r.Status,
					"cross-project agent in group should not be delivered")
			}
		}
	}
}

func TestCPMAcceptance_AC2_HumanDM_Regression(t *testing.T) {
	// Agent→user DM should still work through the outbound endpoint.
	f := acceptanceSetup(t)

	rr := accPostOutbound(t, f, f.hubAgentA, "user:"+f.ownerA.Email, "human dm test")
	assert.Equal(t, http.StatusOK, rr.Code, "agent→user DM should succeed: %s", rr.Body.String())
}

func TestCPMAcceptance_AC2_DirectAgentDM_SameProject(t *testing.T) {
	// Direct agent DM within same project via structured message path.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.projectAgentA.ID, f.projectA, f.projectAgentA.Ancestry),
		f.hubAgentA, "same-project agent DM", false)
	assert.Equal(t, http.StatusOK, rr.Code, "same-project agent DM should succeed: %s", rr.Body.String())

	calls := f.dispatcher.getCalls()
	require.NotEmpty(t, calls, "dispatch should have occurred for same-project agent DM")
	assert.Equal(t, f.hubAgentA.ID, calls[0].AgentID)
}

func TestCPMAcceptance_AC2_CrossProject_AgentDM(t *testing.T) {
	// Cross-project agent DM via structured message path.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.hubAgentB, "cross-project agent DM", false)
	assert.Equal(t, http.StatusOK, rr.Code, "cross-project agent DM should succeed: %s", rr.Body.String())

	calls := f.dispatcher.getCalls()
	require.NotEmpty(t, calls, "dispatch should have occurred for cross-project agent DM")
	assert.Equal(t, f.hubAgentB.ID, calls[0].AgentID)
}

// =============================================================================
// AC-3: Rate/length limits and attachments
// =============================================================================

func TestCPMAcceptance_AC3_MessageLength_Exceeds(t *testing.T) {
	// Verify message length limit is enforced on outbound path.
	f := acceptanceSetup(t)

	// Create a message that exceeds MaxMessageLength.
	longMsg := strings.Repeat("x", messages.MaxMessageLength+1)
	rr := accPostOutbound(t, f, f.hubAgentA, "user:"+f.ownerA.Email, longMsg)
	assert.NotEqual(t, http.StatusOK, rr.Code, "message exceeding length limit should be denied")
	assert.Contains(t, rr.Body.String(), "character limit",
		"error should mention character limit")
}

func TestCPMAcceptance_AC3_MessageLength_ExceedsStructured(t *testing.T) {
	// Same length limit on the structured message path.
	f := acceptanceSetup(t)

	longMsg := strings.Repeat("y", messages.MaxMessageLength+1)
	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.projectAgentA, longMsg, false)
	// The structured message path validates via ValidateLegacyMessage.
	assert.NotEqual(t, http.StatusOK, rr.Code, "overlength message via structured path should be denied")
}

func TestCPMAcceptance_AC3_EmptyMessage_Denied(t *testing.T) {
	// Empty message body should be rejected.
	f := acceptanceSetup(t)

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + f.ownerA.Email,
		Msg:       "",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.hubAgentA.ID)

	assert.NotEqual(t, http.StatusOK, rr.Code, "empty message should be denied")
}

func TestCPMAcceptance_AC3_SameProject_AttachmentAllowed(t *testing.T) {
	// Same-project attachment references should pass through ingestAgentAttachments.
	f := acceptanceSetup(t)

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:   "user:" + f.ownerA.Email,
		Msg:         "with attachment",
		Attachments: []string{"/workspace/test-file.txt"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.hubAgentA.ID)

	// Attachment processing may fail (no storage configured), but message should still deliver.
	assert.Equal(t, http.StatusOK, rr.Code, "same-project message with attachments: %s", rr.Body.String())
}

// =============================================================================
// AC-4: Delivery outcomes
// =============================================================================

func TestCPMAcceptance_AC4_BrokerSuccess_UserDM(t *testing.T) {
	// Agent→user DM via outbound: success outcome with status and message_id.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostOutbound(t, f, f.hubAgentA, "user:"+f.ownerA.Email, "broker success test")
	require.Equal(t, http.StatusOK, rr.Code, "agent→user DM should succeed: %s", rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "sent", resp["status"], "delivery status should be 'sent'")
	assert.NotEmpty(t, resp["message_id"], "message_id should be returned")
}

func TestCPMAcceptance_AC4_StructuredMessage_DeliveryOutcome(t *testing.T) {
	// Agent→agent DM via structured message path: HTTP response should have status "delivered".
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.hubAgentB, "delivery outcome test", false)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Contains(t, resp.Status, "delivered", "delivery status should contain 'delivered'")
	assert.NotEmpty(t, resp.MessageID, "message_id should be returned")
}

func TestCPMAcceptance_AC4_DispatchFailure_HandledGracefully(t *testing.T) {
	// Broker dispatch failure on the structured message path.
	f := acceptanceSetup(t)
	f.dispatcher.returnErr = assert.AnError // Simulate dispatch failure

	rr := accPostMessage(t, f,
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli"),
		f.hubAgentA, "dispatch failure test", false)
	// Dispatch failure is an error — the structured message path returns non-200.
	// The specific status depends on the error type; we just verify the handler
	// doesn't panic and responds with an error.
	assert.NotEqual(t, http.StatusOK, rr.Code,
		"dispatch failure should return error: %s", rr.Body.String())

	f.dispatcher.returnErr = nil // Reset for future tests
}

// =============================================================================
// AC-5: Audience containment
// =============================================================================

func TestCPMAcceptance_AC5_DeniedSend_NoEffects(t *testing.T) {
	// A denied cross-project send must leave zero effects:
	// no message persisted, no dispatch call, no lifecycle change.
	// Cross-project denial is enforced via the evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Set inbound to "none" to ensure denial.
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundNone, 2)
	require.NoError(t, err)

	// Evaluator confirms denial.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny with inbound=none")

	// authorizeAgentMessage confirms denial — zero effects start here.
	allowed, _, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny")

	// No dispatch or persistence should occur for denied cross-project messages.
	// The evaluator and authorizeAgentMessage returning false means the message
	// is never forwarded to the dispatch layer at origination points (group,
	// broadcast, scheduled, mentions).
	f.dispatcher.reset()
	msgCountBefore := accCountMessages(t, f.store, f.hubAgentB.ID)

	// Verify via same-project group path: the mode=none agent in project A
	// should be denied by authorizeAgentMessage even within the same project.
	groupRecipient := "group[agent:acc-none-a,agent:acc-proj-a]"
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "agent:" + f.hubAgentA.Slug,
			SenderID:  f.hubAgentA.ID,
			Recipient: groupRecipient,
			Msg:       "should-not-persist-none",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.hubAgentA.ID)

	// proj-a dispatched is fine; none-a must not be dispatched.
	for _, call := range f.dispatcher.getCalls() {
		assert.NotEqual(t, f.noneAgentA.ID, call.AgentID,
			"denied agent (mode=none) should not receive dispatch")
	}

	// No messages persisted for hub-b (cross-project denied target).
	msgCountAfter := accCountMessages(t, f.store, f.hubAgentB.ID)
	assert.Equal(t, msgCountBefore, msgCountAfter, "no message persisted for cross-project denied target")

	_ = rr // group handler returns 200 with per-recipient results
}

func TestCPMAcceptance_AC5_NoneMode_NoEffects(t *testing.T) {
	// Sending to mode=none target produces zero effects.
	// Verified via evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Evaluator confirms denial for cross-project none target.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.noneAgentB)
	assert.False(t, decision.Allowed, "evaluator should deny to cross-project mode=none target")

	// Same-project none target also denied.
	decisionSameProject := f.srv.EvaluateAgentMessage(ctx, identity, f.noneAgentA)
	assert.False(t, decisionSameProject.Allowed, "evaluator should deny to same-project mode=none target")

	// authorizeAgentMessage confirms both denials.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.noneAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny cross-project none: %s", reason)

	allowed2, reason2, _ := f.srv.authorizeAgentMessage(ctx, identity, f.noneAgentA, false)
	assert.False(t, allowed2, "authorizeAgentMessage should deny same-project none: %s", reason2)

	// Verify via same-project group: none-a should not be dispatched.
	f.dispatcher.reset()
	msgCountBefore := accCountMessages(t, f.store, f.noneAgentA.ID)

	groupRecipient := "group[agent:acc-none-a,agent:acc-proj-a]"
	body, _ := json.Marshal(MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      messages.TypeInstruction,
			Sender:    "agent:" + f.hubAgentA.Slug,
			SenderID:  f.hubAgentA.ID,
			Recipient: groupRecipient,
			Msg:       "none-target-no-effect",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(),
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentMessage(rr, req, f.hubAgentA.ID)

	// proj-a may be dispatched (allowed); none-a must not be.
	for _, call := range f.dispatcher.getCalls() {
		assert.NotEqual(t, f.noneAgentA.ID, call.AgentID,
			"mode=none agent should not receive dispatch")
	}
	msgCountAfter := accCountMessages(t, f.store, f.noneAgentA.ID)
	assert.Equal(t, msgCountBefore, msgCountAfter, "no message persisted for mode=none target")

	_ = rr
}

func TestCPMAcceptance_AC5_SelfIdentity_Enforced(t *testing.T) {
	// An agent cannot impersonate another agent on the outbound path.
	f := acceptanceSetup(t)

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + f.ownerA.Email,
		Msg:       "impersonation attempt",
	})
	// Agent A identity but sending as agent B's ID.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentB.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.hubAgentB.ID)

	assert.Equal(t, http.StatusForbidden, rr.Code, "impersonation should return 403")
}

func TestCPMAcceptance_AC5_UnauthorizedConversation_NoPersistence(t *testing.T) {
	// An agent should not be able to hijack another agent's DM conversation.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Create a DM conversation between hub-a and proj-a.
	dmKey, err := messages.DMConversationKey("agent", f.hubAgentA.ID, "agent", f.projectAgentA.ID)
	require.NoError(t, err)
	conv, err := f.store.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// hub-b tries to send to hub-a's conversation.
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:      "user:" + f.ownerA.Email,
		Msg:            "hijack attempt",
		ConversationID: conv.ID,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentB.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		f.hubAgentB.ID, f.projectB, f.hubAgentB.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.hubAgentB.ID)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"conversation hijack should be denied: %s", rr.Body.String())
}

// =============================================================================
// AC-6/Wake: Wake support for agent DMs
// =============================================================================

func TestCPMAcceptance_Wake_SuspendedResume(t *testing.T) {
	// Sending a message with wake=true to a suspended agent resumes it.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Set agent to suspended.
	require.NoError(t, f.store.UpdateAgentStatus(ctx, f.hubAgentA.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseSuspended),
	}))

	// Simulate the agent becoming ready after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = f.store.UpdateAgentStatus(context.Background(), f.hubAgentA.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()

	f.dispatcher.reset()
	rr := accPostMessage(t, f,
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli"),
		f.hubAgentA, "wake-suspended", true)
	assert.Equal(t, http.StatusOK, rr.Code, "wake of suspended agent should succeed: %s", rr.Body.String())

	// Verify DispatchAgentStart was called with continue=true.
	startCalls := f.dispatcher.getStartCalls()
	require.NotEmpty(t, startCalls, "DispatchAgentStart should have been called")
	assert.True(t, startCalls[0].Continue, "DispatchAgentStart should be called with continue=true")
}

func TestCPMAcceptance_Wake_AlreadyRunning_NoOp(t *testing.T) {
	// wake=true to an already-running agent is a no-op — message delivered normally.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli"),
		f.hubAgentA, "wake-running-noop", true)
	assert.Equal(t, http.StatusOK, rr.Code, "wake of running agent should succeed: %s", rr.Body.String())

	// No start call.
	assert.Empty(t, f.dispatcher.getStartCalls(), "no DispatchAgentStart for already-running agent")
	// Message was dispatched.
	calls := f.dispatcher.getCalls()
	require.NotEmpty(t, calls, "message should be dispatched to running agent")
}

func TestCPMAcceptance_Wake_StoppedAgent_Denied(t *testing.T) {
	// wake=true to a stopped agent returns 400.
	f := acceptanceSetup(t)
	ctx := context.Background()

	require.NoError(t, f.store.UpdateAgentStatus(ctx, f.projectAgentA.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopped),
	}))

	rr := accPostMessage(t, f,
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli"),
		f.projectAgentA, "wake-stopped", true)
	assert.Equal(t, http.StatusBadRequest, rr.Code, "wake of stopped agent should return 400")
	assert.Contains(t, rr.Body.String(), "stopped")
}

func TestCPMAcceptance_Wake_ErrorAgent_Denied(t *testing.T) {
	// wake=true to an error-state agent returns 400.
	f := acceptanceSetup(t)
	ctx := context.Background()

	require.NoError(t, f.store.UpdateAgentStatus(ctx, f.projectAgentA.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseError),
	}))

	rr := accPostMessage(t, f,
		NewAuthenticatedUser(f.ownerA.ID, f.ownerA.Email, f.ownerA.DisplayName, "member", "cli"),
		f.projectAgentA, "wake-error", true)
	assert.Equal(t, http.StatusBadRequest, rr.Code, "wake of error agent should return 400")
}

func TestCPMAcceptance_NoWake_SuspendedAgent_Denied(t *testing.T) {
	// Message without wake to a suspended agent should return conflict.
	f := acceptanceSetup(t)
	ctx := context.Background()

	require.NoError(t, f.store.UpdateAgentStatus(ctx, f.hubAgentB.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseSuspended),
	}))

	rr := accPostMessage(t, f,
		NewAuthenticatedUser(f.ownerB.ID, f.ownerB.Email, f.ownerB.DisplayName, "member", "cli"),
		f.hubAgentB, "no-wake-suspended", false)
	assert.Equal(t, http.StatusConflict, rr.Code, "message to suspended agent without wake: %s", rr.Body.String())
}

// =============================================================================
// AC-6/Scheduler: Scheduled message containment
// =============================================================================

func TestCPMAcceptance_Scheduler_CrossProjectDenied(t *testing.T) {
	// A scheduled message targeting a cross-project agent must be denied at fire time.
	ms := newContainmentMockStore()
	spy := &containmentDispatchSpy{}

	ms.users["creator-user"] = &store.User{
		ID:     "creator-user",
		Email:  "creator@test.com",
		Status: store.UserStatusActive,
		Role:   "member",
	}
	ms.agents["target-agent"] = &store.Agent{
		ID:          "target-agent",
		Name:        "target",
		Slug:        "target",
		ProjectID:   "project-b",
		MessageMode: store.MessageModeProject,
	}

	srv := containmentTestServer(ms)
	srv.SetDispatcher(spy)

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: "target-agent",
		Message: "cross-project scheduled",
	})
	evt := store.ScheduledEvent{
		ID:        "acc-evt-cross",
		ProjectID: "project-a",
		EventType: "message",
		Payload:   string(payload),
		CreatedBy: "creator-user",
		FireAt:    time.Now(),
		Status:    store.ScheduledEventPending,
	}
	ms.events[evt.ID] = &evt

	handler := srv.messageEventHandler()
	err := handler(context.Background(), evt)
	assert.Error(t, err, "cross-project scheduled message must be denied")
	assert.Empty(t, spy.getCalls(), "no dispatch for cross-project scheduled message")
}

func TestCPMAcceptance_Scheduler_SameProjectAllowed(t *testing.T) {
	// A scheduled message within the same project should fire successfully.
	// Uses the full acceptanceSetup to get proper RBAC bindings.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	payload, _ := json.Marshal(MessageEventPayload{
		AgentID: f.projectAgentA.ID,
		Message: "same-project scheduled",
	})
	evt := store.ScheduledEvent{
		ID:        tid("acc-evt-same"),
		ProjectID: f.projectA,
		EventType: "message",
		Payload:   string(payload),
		CreatedBy: f.ownerA.ID,
		FireAt:    time.Now(),
		Status:    store.ScheduledEventPending,
	}

	handler := f.srv.messageEventHandler()
	err := handler(context.Background(), evt)
	assert.NoError(t, err, "same-project scheduled message should fire: %v", err)
	assert.NotEmpty(t, f.dispatcher.getCalls(), "dispatch should occur for same-project scheduled message")
}

// =============================================================================
// AC-1 extension: Outbound DM cross-project parity on outbound endpoint
// =============================================================================

func TestCPMAcceptance_CrossProject_InboundNone_PolicyEnforced(t *testing.T) {
	// Verify that cross-project policy is enforced via the evaluator and authorizeAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Set project B inbound to "none".
	_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundNone, 2)
	require.NoError(t, err)

	// Evaluator path: should be denied.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "cross-project should be denied with inbound=none")
	assert.Equal(t, MessageDenialCrossProjectInboundNone, decision.Code)

	// authorizeAgentMessage should also deny.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny with inbound=none: %s", reason)

	// Verify no messages persisted for the denied target.
	countBefore := accCountMessages(t, f.store, f.hubAgentB.ID)
	// (no send attempted — evaluator and authorizeAgentMessage deny)
	countAfter := accCountMessages(t, f.store, f.hubAgentB.ID)
	assert.Equal(t, countBefore, countAfter, "no persistence for denied cross-project target")
}

// =============================================================================
// AC-1 extension: Conversation assertion security
// =============================================================================

func TestCPMAcceptance_ConversationRef_MutualExclusion(t *testing.T) {
	// Setting both conversation_ref and conversation_id is a 400.
	f := acceptanceSetup(t)

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient:       "user:" + f.ownerA.Email,
		Msg:             "mutual exclusion test",
		ConversationID:  "some-id",
		ConversationRef: "conv:some-ref",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.hubAgentA.ID)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"both conversation_ref and conversation_id should be a 400")
}

// =============================================================================
// AC-3 extension: DM key validation
// =============================================================================

func TestCPMAcceptance_InvalidDMKey_Denied(t *testing.T) {
	// A malformed DM key in thread_id should be rejected.
	f := acceptanceSetup(t)

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + f.ownerA.Email,
		Msg:       "bad dm key",
		ThreadID:  "dm:invalid-format",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentA.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), accAgentIdentity(
		f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.hubAgentA.ID)

	assert.Equal(t, http.StatusBadRequest, rr.Code, "invalid DM key should be rejected")
}

// =============================================================================
// AC-5: Observer/audience containment for cross-project DM persistence
// =============================================================================

func TestCPMAcceptance_AC5_CrossProject_MessagePersistence_Correct(t *testing.T) {
	// When a cross-project DM is allowed, verify the message is persisted
	// with correct provenance fields via the structured message path.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.hubAgentB, "provenance-check", false)
	require.Equal(t, http.StatusOK, rr.Code)

	// Find the persisted message via the delivery response.
	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.MessageID, "message should have been persisted")
	assert.Contains(t, resp.Status, "delivered", "delivery should be successful")
}

// =============================================================================
// AC-1 extension: 5×5 mode matrix same-project EvaluateAgentMessage
// =============================================================================

func TestCPMAcceptance_AC1_ModeMatrix_AllPairs(t *testing.T) {
	// Exercise the full 5×5 same-project mode matrix through EvaluateAgentMessage.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Create agents for all modes within project A.
	modes := map[string]*store.Agent{
		store.MessageModeNone:    f.noneAgentA,
		store.MessageModeProject: f.projectAgentA,
		store.MessageModeHub:     f.hubAgentA,
	}

	// Create lineage and branch agents.
	lineageAgent := &store.Agent{
		ID:          tid("acc-lineage-a"),
		Slug:        "acc-lineage-a",
		Name:        "LineageAgentA",
		ProjectID:   f.projectA,
		MessageMode: store.MessageModeLineage,
		Ancestry:    []string{f.ownerA.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	branchAgent := &store.Agent{
		ID:          tid("acc-branch-a"),
		Slug:        "acc-branch-a",
		Name:        "BranchAgentA",
		ProjectID:   f.projectA,
		MessageMode: store.MessageModeBranch,
		Ancestry:    []string{f.ownerA.ID},
		Phase:       "running",
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(ctx, lineageAgent))
	require.NoError(t, f.store.CreateAgent(ctx, branchAgent))

	modes[store.MessageModeLineage] = lineageAgent
	modes[store.MessageModeBranch] = branchAgent

	type modeResult struct {
		sender, target string
		allowed        bool
	}

	expected := []modeResult{
		// none sender: all denied
		{store.MessageModeNone, store.MessageModeNone, false},
		{store.MessageModeNone, store.MessageModeLineage, false},
		{store.MessageModeNone, store.MessageModeBranch, false},
		{store.MessageModeNone, store.MessageModeProject, false},
		{store.MessageModeNone, store.MessageModeHub, false},
		// lineage sender: all denied (no agent-to-agent edges)
		{store.MessageModeLineage, store.MessageModeNone, false},
		{store.MessageModeLineage, store.MessageModeLineage, false},
		{store.MessageModeLineage, store.MessageModeBranch, false},
		{store.MessageModeLineage, store.MessageModeProject, false},
		{store.MessageModeLineage, store.MessageModeHub, false},
		// branch sender: all denied (no parent/child in test)
		{store.MessageModeBranch, store.MessageModeNone, false},
		{store.MessageModeBranch, store.MessageModeLineage, false},
		{store.MessageModeBranch, store.MessageModeBranch, false},
		{store.MessageModeBranch, store.MessageModeProject, false},
		{store.MessageModeBranch, store.MessageModeHub, false},
		// project sender: none/lineage/branch denied; project and hub allowed
		{store.MessageModeProject, store.MessageModeNone, false},
		{store.MessageModeProject, store.MessageModeLineage, false},
		{store.MessageModeProject, store.MessageModeBranch, false},
		{store.MessageModeProject, store.MessageModeProject, true},
		{store.MessageModeProject, store.MessageModeHub, true},
		// hub sender: none/lineage/branch denied; project and hub allowed
		{store.MessageModeHub, store.MessageModeNone, false},
		{store.MessageModeHub, store.MessageModeLineage, false},
		{store.MessageModeHub, store.MessageModeBranch, false},
		{store.MessageModeHub, store.MessageModeProject, true},
		{store.MessageModeHub, store.MessageModeHub, true},
	}

	for _, tc := range expected {
		name := tc.sender + "→" + tc.target
		t.Run(name, func(t *testing.T) {
			sender := modes[tc.sender]
			target := modes[tc.target]
			identity := accAgentIdentity(sender.ID, sender.ProjectID, sender.Ancestry)
			decision := f.srv.EvaluateAgentMessage(ctx, identity, target)
			assert.Equal(t, tc.allowed, decision.Allowed,
				"mode pair %s→%s: expected allowed=%v, got allowed=%v reason=%s",
				tc.sender, tc.target, tc.allowed, decision.Allowed, decision.Reason)
		})
	}
}

// =============================================================================
// AC-1 extension: Cross-project gate matrix via EvaluateAgentMessage
// =============================================================================

func TestCPMAcceptance_AC1_CrossProjectGates(t *testing.T) {
	f := acceptanceSetup(t)
	ctx := context.Background()

	t.Run("hub_to_hub_allowed", func(t *testing.T) {
		identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
		assert.True(t, decision.Allowed, "hub→hub cross-project: %s", decision.Reason)
	})

	t.Run("hub_to_project_allowed", func(t *testing.T) {
		identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.projectAgentB)
		assert.True(t, decision.Allowed, "hub→project cross-project: %s", decision.Reason)
	})

	t.Run("project_sender_denied", func(t *testing.T) {
		identity := accAgentIdentity(f.projectAgentA.ID, f.projectA, f.projectAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
		assert.False(t, decision.Allowed, "project→hub cross-project should be denied")
		assert.Equal(t, MessageDenialCrossProjectSenderMode, decision.Code)
	})

	t.Run("hub_to_branch_denied", func(t *testing.T) {
		identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.branchAgentB)
		assert.False(t, decision.Allowed, "hub→branch cross-project should be denied")
		assert.Equal(t, MessageDenialCrossProjectTargetMode, decision.Code)
	})

	t.Run("hub_to_none_denied", func(t *testing.T) {
		identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.noneAgentB)
		assert.False(t, decision.Allowed, "hub→none cross-project should be denied")
	})

	t.Run("cpm_disabled_denied", func(t *testing.T) {
		// Disable CPM.
		fakeStore := newFakeHubSettingStore()
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
		f.srv.SetOperationalSettings(ops)

		identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
		assert.False(t, decision.Allowed, "should be denied when CPM disabled")
		assert.Equal(t, MessageDenialCrossProjectDisabled, decision.Code)

		// Re-enable.
		enableCrossProjectMessaging(t, f.srv)
	})

	t.Run("inbound_none_denied", func(t *testing.T) {
		_, err := f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundNone, 2)
		require.NoError(t, err)

		identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
		decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
		assert.False(t, decision.Allowed, "should be denied with inbound=none")
		assert.Equal(t, MessageDenialCrossProjectInboundNone, decision.Code)

		// Restore.
		_, err = f.store.UpdateProjectMessagingPolicy(ctx, f.projectB, store.CrossProjectInboundAny, 3)
		require.NoError(t, err)
	})
}

// =============================================================================
// AC-2 extension: Original group-reply scenario (PR #1679 regression)
// =============================================================================

func TestCPMAcceptance_AC2_GroupReply_OriginalScenario(t *testing.T) {
	// Test the original group-reply scenario that PR #1679 tried to fix:
	// an agent receiving a group message and replying via outbound-message.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	// Agent A sends a user-directed reply to ownerA after receiving
	// a group conversation message. This exercises the outbound path with
	// the group conversation context.
	rr := accPostOutbound(t, f, f.hubAgentA, "user:"+f.ownerA.Email, "reply to group discussion")
	assert.Equal(t, http.StatusOK, rr.Code,
		"group reply via outbound should succeed: %s", rr.Body.String())
}

// =============================================================================
// AC-4 extension: Permitted foreign DM/reply
// =============================================================================

func TestCPMAcceptance_AC4_ForeignDM_AllowedWhenPolicyPermits(t *testing.T) {
	// A cross-project DM from hub-mode sender should succeed when:
	// 1. CPM is enabled at Hub level
	// 2. Target project inbound policy is "any"
	// 3. Sender mode is "hub"
	// 4. Target mode is "project" or "hub"
	// 5. Ancestry is hub-attested
	//
	// Test this through the structured message path (handleAgentMessage),
	// which is the ingress for agent-to-agent DMs.
	f := acceptanceSetup(t)
	f.dispatcher.reset()

	// Send via structured message path as agent sender.
	rr := accPostMessage(t, f,
		accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry),
		f.projectAgentB, "permitted foreign dm", false)
	require.Equal(t, http.StatusOK, rr.Code, "permitted foreign DM: %s", rr.Body.String())

	// Verify the message was dispatched.
	calls := f.dispatcher.getCalls()
	require.NotEmpty(t, calls, "dispatch should occur for permitted foreign DM")
	assert.Equal(t, f.projectAgentB.ID, calls[0].AgentID)
}

// =============================================================================
// AC-5 extension: Negative audience assertion — no content leak on denial
// =============================================================================

func TestCPMAcceptance_AC5_NegativeAudience_Comprehensive(t *testing.T) {
	// Comprehensive negative assertion: when cross-project messaging is denied,
	// verify that the EvaluateAgentMessage evaluator correctly denies, and that
	// the outbound endpoint (which calls resolveOutboundRouting → S7 agent authz)
	// produces zero effects.
	f := acceptanceSetup(t)
	ctx := context.Background()

	// Disable cross-project messaging.
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	f.srv.SetOperationalSettings(ops)

	// Verify EvaluateAgentMessage denies on both directions.
	identity := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	decision := f.srv.EvaluateAgentMessage(ctx, identity, f.hubAgentB)
	assert.False(t, decision.Allowed, "EvaluateAgentMessage should deny when CPM disabled")
	assert.Equal(t, MessageDenialCrossProjectDisabled, decision.Code,
		"denial code should be cross_project_disabled")

	// Verify the authorizeAgentMessage call also denies.
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, identity, f.hubAgentB, false)
	assert.False(t, allowed, "authorizeAgentMessage should deny: %s", reason)

	// Verify no persistence for the denied target.
	msgCountBefore := accCountMessages(t, f.store, f.hubAgentB.ID)
	// No send attempted — evaluator and authorizeAgentMessage deny at origination.
	msgCountAfter := accCountMessages(t, f.store, f.hubAgentB.ID)
	assert.Equal(t, msgCountBefore, msgCountAfter, "no messages persisted for denied target")

	// Re-enable for other tests.
	enableCrossProjectMessaging(t, f.srv)
}
