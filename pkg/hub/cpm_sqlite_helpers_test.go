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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

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
	srv.seedProjectCreatorMembership(ctx, pA)
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
	srv.seedProjectCreatorMembership(ctx, pB)
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

func (d *acceptanceDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *acceptanceDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *acceptanceDispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error {
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
func (d *acceptanceDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *acceptanceDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

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
