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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// msFixture is the shared fixture of the membership standing tests
// (ptone/scion#3433): a project P owned by O, a member user U, U's agent A
// (edge U -> A) and A's child C (edge A -> C, owner A).
type msFixture struct {
	t         *testing.T
	srv       *Server
	s         store.Store
	projectID string
	ownerID   string
	userID    string
	agentA    *store.Agent
	childC    *store.Agent
}

func newMSFixture(t *testing.T, name string) *msFixture {
	t.Helper()
	srv, s := testServer(t)
	// Tests drive processing themselves.
	srv.membershipService.onMembershipLoss = nil
	f := &msFixture{
		t:         t,
		srv:       srv,
		s:         s,
		projectID: tid("ms-" + name + "-project"),
		ownerID:   tid("ms-" + name + "-owner"),
		userID:    tid("ms-" + name + "-user"),
	}
	createRS1Project(t, s, f.projectID, f.ownerID)
	f.addUser(f.userID)
	f.addMember(f.userID, store.ProjectRoleMember)
	f.agentA = f.userAgent("a-"+name, f.userID)
	f.childC = f.childAgent("c-"+name, f.agentA)
	return f
}

// addUser creates an active hub user.
func (f *msFixture) addUser(id string) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.s.GetUser(ctx, id); err == nil {
		return
	}
	require.NoError(f.t, f.s.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@test.com", DisplayName: "User", Role: "member", Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, f.s, id)
}

// addMember binds a user to the project with the named project role.
func (f *msFixture) addMember(userID, role string) *store.RoleBinding {
	f.t.Helper()
	ctx := context.Background()
	rd, err := f.s.GetRoleDefinitionByName(ctx, role, store.RoleScopeProject)
	require.NoError(f.t, err)
	rb, err := f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(f.t, err)
	return rb
}

// dropBindings deletes every project binding of the user directly (no
// membership loss check is written).
func (f *msFixture) dropBindings(userID string) {
	f.t.Helper()
	ctx := context.Background()
	rbs, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(f.t, err)
	for _, rb := range rbs {
		if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == f.projectID {
			require.NoError(f.t, f.s.DeleteRoleBinding(ctx, rb.ID))
		}
	}
}

// removeAndProcess drops the user's project bindings, enqueues a check and
// drains the outbox.
func (f *msFixture) removeAndProcess(userID string) {
	f.t.Helper()
	ctx := context.Background()
	f.dropBindings(userID)
	require.NoError(f.t, enqueueMembershipLossTx(ctx, f.s, userID, f.projectID, store.MembershipLossTriggerMemberRemove, AuditActor{}))
	f.srv.drainMembershipLossChecks(ctx)
}

func (f *msFixture) newAgentRow(name, ownerID, createdBy string, ancestry []string) *store.Agent {
	f.t.Helper()
	id := tid("ms-agent-" + name)
	if ownerID == "" && createdBy == "" && len(ancestry) == 0 {
		markRootlessTestAgent(id)
	}
	a := &store.Agent{
		ID:              id,
		Slug:            "ms-" + name,
		Name:            "ms-" + name,
		ProjectID:       f.projectID,
		OwnerID:         ownerID,
		CreatedBy:       createdBy,
		Ancestry:        ancestry,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: tid("ms-broker"),
		StateVersion:    1,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	require.NoError(f.t, f.s.CreateAgent(context.Background(), a))
	return a
}

// userAgent creates an agent rooted at userID by an edge, as a user create
// does.
func (f *msFixture) userAgent(name, userID string) *store.Agent {
	f.t.Helper()
	a := f.newAgentRow(name, userID, userID, []string{userID})
	f.edge(store.DelegationPrincipalUser, userID, a)
	return a
}

// childAgent creates an agent created by parent, with an edge from parent.
func (f *msFixture) childAgent(name string, parent *store.Agent) *store.Agent {
	f.t.Helper()
	ancestry := append(append([]string{}, parent.Ancestry...), parent.ID)
	c := f.newAgentRow(name, parent.ID, parent.ID, ancestry)
	f.edge(store.DelegationPrincipalAgent, parent.ID, c)
	return c
}

// edge records a delegation edge from (delegatorType, delegatorID) to a.
func (f *msFixture) edge(delegatorType, delegatorID string, a *store.Agent) {
	f.t.Helper()
	e := &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    a.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       a.ProjectID,
		Role:          string(AgentRoleBaseline),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(f.t, f.s.CreateDelegationEdge(context.Background(), e))
}

// hold places an active hold on the agent for root userID.
func (f *msFixture) hold(agentID, userID string) {
	f.t.Helper()
	_, err := f.s.CreateAgentHolds(context.Background(), []*store.AgentHold{{
		AgentID:           agentID,
		ProjectID:         f.projectID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   userID,
		Trigger:           store.MembershipLossTriggerMemberRemove,
		ActorKind:         "system",
		ActorID:           "hub",
		CorrelationID:     "test",
	}})
	require.NoError(f.t, err)
}

func (f *msFixture) held(agentID string) bool {
	f.t.Helper()
	held, err := f.s.HasActiveAgentHold(context.Background(), agentID)
	require.NoError(f.t, err)
	return held
}

// agentToken mints a token for the agent directly from the token service
// (no standing check), with the given scopes.
func (f *msFixture) agentToken(a *store.Agent, scopes ...AgentTokenScope) string {
	f.t.Helper()
	if len(scopes) == 0 {
		scopes = ScopesForRole(AgentRoleFull)
	}
	tok, err := f.srv.agentTokenService.GenerateAgentToken(a.ID, a.ProjectID, scopes, a.Ancestry)
	require.NoError(f.t, err)
	return tok
}

// agentIdentity returns an in-process identity for the agent.
func (f *msFixture) agentIdentity(a *store.Agent) AgentIdentity {
	return newFullAgentIdentity(a.ID, a.ProjectID, a.Ancestry, ScopesForRole(AgentRoleFull))
}

// ensureStandingRoot makes userID an active user with a project member
// binding in projectID (idempotent), so agents rooted at the user are in
// good standing (ptone/scion#3433). For shared fixtures whose agents name a
// user as owner, creator or ancestry root.
func ensureStandingRoot(t testing.TB, s store.Store, projectID, userID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, userID); err != nil {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: userID + "@test.example", DisplayName: "Fixture User",
			Role: store.UserRoleMember, Status: store.UserStatusActive,
		}))
	}
	rbs, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, rb := range rbs {
		if rb.ScopeType == store.RoleScopeProject && rb.ScopeID == projectID {
			return
		}
	}
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}
