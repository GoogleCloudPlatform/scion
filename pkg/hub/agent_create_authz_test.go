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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---------------------------------------------------------------

// createFixtureAgent stores an agent in the fixture project with the given
// ancestry and applied role.
func createFixtureAgent(t *testing.T, f *bypassAgentsFixture, name string, ancestry []string, role AgentRole) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(name), Slug: tid(name), Name: name, ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: ancestry[0], OwnerID: ancestry[0], Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// createAsAgent posts a child-agent create in the fixture project with a
// token for agentID carrying the scopes of its stored role plus
// ScopeAgentCreate.
func createAsAgent(t *testing.T, f *bypassAgentsFixture, agentID string, req CreateAgentRequest) *httptest.ResponseRecorder {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	stored, err := f.store.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	role, _ := agentRoleAndScopes(stored)
	scopes := append([]AgentTokenScope{ScopeProjectRead, ScopeAgentCreate}, ScopesForRole(role)...)
	tok, err := svc.GenerateAgentToken(agentID, f.proj.ID, scopes, nil)
	require.NoError(t, err)
	return doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents", req, tok)
}

// grantFixtureRole binds userID to a seeded project role in the fixture
// project.
func grantFixtureRole(t *testing.T, f *bypassAgentsFixture, userID, role string) {
	t.Helper()
	grantProjectRole(t, f.store, userID, f.proj.ID, role)
}

// assertAgentCreateDenied asserts the neutral create 403 and whether it
// carries the delegation-ceiling detail.
func assertAgentCreateDenied(t *testing.T, rec *httptest.ResponseRecorder, ceiling bool) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	apiErr := decodeAPIError(t, rec)
	assert.Equal(t, ErrCodeForbidden, apiErr.Code)
	assert.Equal(t, agentCreateDenyMessage, apiErr.Message)
	if ceiling {
		assert.Equal(t, map[string]interface{}{"denied_by": "delegation_ceiling"}, apiErr.Details)
	} else {
		assert.Empty(t, apiErr.Details)
	}
}

// --- characterization ------------------------------------------------------

// TestAgentCreate_Characterization pins who may create an agent in a
// project: a super-admin session, a user holding agent.create, a UAT
// carrying agent:create for that user, an agent whose live user delegator
// holds agent.create, an agent at the end of a live agent chain, and an agent
// delegated by a super-admin. A user without agent.create and a UAT without
// agent:create are denied.
func TestAgentCreate_Characterization(t *testing.T) {
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	grantFixtureRole(t, f, f.owner.ID, store.ProjectRoleMember)
	path := "/api/v1/projects/" + f.proj.ID + "/agents"

	creator := hubMemberUser(t, f.store, "create-member")
	grantFixtureRole(t, f, creator.ID, store.ProjectRoleMember)
	outsider := hubMemberUser(t, f.store, "create-outsider")

	// Agent chain: owner -> caller -> chained. The caller's stored role
	// carries project:agent:create.
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
	chained := createFixtureAgent(t, f, "create-chained", []string{f.owner.ID, f.caller.ID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalAgent, f.caller.ID, chained.ID, f.proj.ID)
	// Agent chain through an intermediate agent whose stored role lacks
	// project:agent:create: owner -> limited -> underLimited.
	limited := createFixtureAgent(t, f, "create-limited", []string{f.owner.ID}, AgentRoleReadOnly)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, limited.ID, f.proj.ID)
	underLimited := createFixtureAgent(t, f, "create-under-limited", []string{f.owner.ID, limited.ID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalAgent, limited.ID, underLimited.ID, f.proj.ID)

	// Agent delegated by a super-admin with no project role.
	adminID := tid("create-super-admin")
	createTestUserWithRole(t, f.store, adminID, "create-super-admin@create.test", "admin", store.SystemRoleSuperAdmin)
	adminAgent := createFixtureAgent(t, f, "create-admin-agent", []string{adminID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, adminID, adminAgent.ID, f.proj.ID)

	t.Run("super-admin session", func(t *testing.T) {
		rec := doRequest(t, f.srv, http.MethodPost, path, CreateAgentRequest{Name: "char-dev"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("user with agent.create", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, authUser(creator), http.MethodPost, path, CreateAgentRequest{Name: "char-user"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("UAT with agent:create", func(t *testing.T) {
		uat := NewScopedUserIdentity(authUser(creator), f.proj.ID, []string{"agent:create"})
		rec := requestAsIdentity(t, f.srv, uat, http.MethodPost, path, CreateAgentRequest{Name: "char-uat"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("agent with live user delegator", func(t *testing.T) {
		rec := createAsAgent(t, f, f.caller.ID, CreateAgentRequest{Name: "char-agent"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("agent with live agent chain", func(t *testing.T) {
		caller, err := f.store.GetAgent(context.Background(), f.caller.ID)
		require.NoError(t, err)
		caller.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
		require.NoError(t, f.store.UpdateAgent(context.Background(), caller))
		rec := createAsAgent(t, f, chained.ID, CreateAgentRequest{Name: "char-chained"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("agent chain through an agent without the create scope", func(t *testing.T) {
		rec := createAsAgent(t, f, underLimited.ID, CreateAgentRequest{Name: "char-under-limited"})
		assertAgentCreateDenied(t, rec, true)
	})
	t.Run("agent delegated by super-admin", func(t *testing.T) {
		rec := createAsAgent(t, f, adminAgent.ID, CreateAgentRequest{Name: "char-admin-agent"})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("user without agent.create", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, authUser(outsider), http.MethodPost, path, CreateAgentRequest{Name: "char-outsider"})
		assertAgentCreateDenied(t, rec, false)
	})
	t.Run("UAT without agent:create", func(t *testing.T) {
		uat := NewScopedUserIdentity(authUser(creator), f.proj.ID, []string{"agent:read"})
		rec := requestAsIdentity(t, f.srv, uat, http.MethodPost, path, CreateAgentRequest{Name: "char-uat-read"})
		assertAgentCreateDenied(t, rec, false)
	})
}

// --- live delegator ---------------------------------------------------------

// TestAgentCreate_RequiresLiveDelegator pins that an agent creates a child
// only while every delegator in its chain is live: a soft-deleted, missing
// or suspended delegator user, or a soft-deleted intermediate agent, denies
// the create with the delegation-ceiling detail and stores no child.
func TestAgentCreate_RequiresLiveDelegator(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *bypassAgentsFixture) string // returns the creator agent ID
	}{
		{"soft-deleted delegator user", func(t *testing.T, f *bypassAgentsFixture) string {
			u := hubMemberUser(t, f.store, "live-deleted")
			grantFixtureRole(t, f, u.ID, store.ProjectRoleMember)
			a := createFixtureAgent(t, f, "live-deleted-agent", []string{u.ID}, AgentRoleFull)
			addProjectEdge(t, f.store, store.DelegationPrincipalUser, u.ID, a.ID, f.proj.ID)
			require.NoError(t, f.store.DeleteUser(context.Background(), u.ID))
			return a.ID
		}},
		{"missing delegator user", func(t *testing.T, f *bypassAgentsFixture) string {
			missing := tid("live-missing")
			a := createFixtureAgent(t, f, "live-missing-agent", []string{missing}, AgentRoleFull)
			addProjectEdge(t, f.store, store.DelegationPrincipalUser, missing, a.ID, f.proj.ID)
			return a.ID
		}},
		{"suspended delegator user", func(t *testing.T, f *bypassAgentsFixture) string {
			u := hubMemberUser(t, f.store, "live-suspended")
			grantFixtureRole(t, f, u.ID, store.ProjectRoleMember)
			a := createFixtureAgent(t, f, "live-suspended-agent", []string{u.ID}, AgentRoleFull)
			addProjectEdge(t, f.store, store.DelegationPrincipalUser, u.ID, a.ID, f.proj.ID)
			setUserStatus(t, f.store, u.ID, store.UserStatusSuspended)
			return a.ID
		}},
		{"soft-deleted intermediate agent", func(t *testing.T, f *bypassAgentsFixture) string {
			grantFixtureRole(t, f, f.owner.ID, store.ProjectRoleMember)
			addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
			a := createFixtureAgent(t, f, "live-deep-agent", []string{f.owner.ID, f.caller.ID}, AgentRoleFull)
			addProjectEdge(t, f.store, store.DelegationPrincipalAgent, f.caller.ID, a.ID, f.proj.ID)
			softDeleteStoredAgent(t, f.store, f.caller.ID)
			return a.ID
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := bypassAgentsSetup(t)
			markEdgeBackfillComplete(t, f.store)
			creatorID := tc.setup(t, f)
			rec := createAsAgent(t, f, creatorID, CreateAgentRequest{Name: "live-child"})
			assertAgentCreateDenied(t, rec, true)
			_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "live-child")
			assert.ErrorIs(t, err, store.ErrNotFound, "no child agent is stored")
		})
	}
}

// TestAgentCreate_DelegatorNeedsAgentCreate pins that an agent's live
// delegator must itself hold agent.create on the target project.
func TestAgentCreate_DelegatorNeedsAgentCreate(t *testing.T) {
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	member := hubMemberUser(t, f.store, "delegator-no-create")
	a := createFixtureAgent(t, f, "delegator-no-create-agent", []string{member.ID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, member.ID, a.ID, f.proj.ID)

	rec := createAsAgent(t, f, a.ID, CreateAgentRequest{Name: "no-create-child"})
	assertAgentCreateDenied(t, rec, true)

	grantFixtureRole(t, f, member.ID, store.ProjectRoleMember)
	rec = createAsAgent(t, f, a.ID, CreateAgentRequest{Name: "no-create-child"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestAgentCreate_AgentScopeAndProjectChecks pins the agent-caller checks
// that precede the decision: the create scope and the caller's own project.
func TestAgentCreate_AgentScopeAndProjectChecks(t *testing.T) {
	f := bypassAgentsSetup(t)

	rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "scope-less"}, ScopeAgentStatusUpdate)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "Missing required scope: "+string(ScopeAgentCreate), decodeAPIError(t, rec).Message)

	rec = f.asAgent(t, http.MethodPost, "/api/v1/agents",
		CreateAgentRequest{Name: "cross-project", ProjectID: f.other.ID}, ScopeAgentCreate)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "Agents can only create sub-agents within their own project", decodeAPIError(t, rec).Message)
}

// TestAgentCreate_ExplicitRoleAboveParentDenied pins that an agent caller
// requesting a child role above its own stored role is refused, while the
// defaulted role follows the parent.
func TestAgentCreate_ExplicitRoleAboveParentDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	markEdgeBackfillComplete(t, f.store)
	grantFixtureRole(t, f, f.owner.ID, store.ProjectRoleMember)
	parent := createFixtureAgent(t, f, "role-parent", []string{f.owner.ID}, AgentRoleBaseline)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, parent.ID, f.proj.ID)

	rec := createAsAgent(t, f, parent.ID, CreateAgentRequest{Name: "role-full", AgentRole: string(AgentRoleFull)})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, decodeAPIError(t, rec).Message, "Cannot grant sub-agent role")
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "role-full")
	assert.ErrorIs(t, err, store.ErrNotFound)

	rec = createAsAgent(t, f, parent.ID, CreateAgentRequest{Name: "role-default"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	child, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "role-default")
	require.NoError(t, err)
	require.NotNil(t, child.AppliedConfig)
	assert.Equal(t, string(AgentRoleBaseline), child.AppliedConfig.AgentRole)
}

// --- scheduled dispatch ----------------------------------------------------

// TestAgentCreate_ScheduledCreatorRequiresLiveDelegator pins the fire-time
// check for a scheduled dispatch created by an agent: the creator agent must
// be stored and not deleted, and its live delegation chain must hold
// agent.create on the project.
func TestAgentCreate_ScheduledCreatorRequiresLiveDelegator(t *testing.T) {
	setup := func(t *testing.T) (*bypassAgentsFixture, *store.Agent, *store.User) {
		f := bypassAgentsSetup(t)
		markEdgeBackfillComplete(t, f.store)
		u := hubMemberUser(t, f.store, "sched-delegator")
		grantFixtureRole(t, f, u.ID, store.ProjectRoleMember)
		a := createFixtureAgent(t, f, "sched-creator", []string{u.ID}, AgentRoleFull)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, u.ID, a.ID, f.proj.ID)
		return f, a, u
	}
	evt := func(f *bypassAgentsFixture, creatorID string) store.ScheduledEvent {
		return store.ScheduledEvent{ID: "sched-dispatch", ProjectID: f.proj.ID, EventType: "dispatch_agent", CreatedBy: creatorID}
	}

	t.Run("live delegator", func(t *testing.T) {
		f, a, _ := setup(t)
		ok, err := f.srv.authorizeScheduledAgentCreate(context.Background(), evt(f, a.ID))
		require.NoError(t, err)
		assert.True(t, ok)
	})
	t.Run("deleted delegator", func(t *testing.T) {
		f, a, u := setup(t)
		require.NoError(t, f.store.DeleteUser(context.Background(), u.ID))
		ok, err := f.srv.authorizeScheduledAgentCreate(context.Background(), evt(f, a.ID))
		require.Error(t, err)
		assert.False(t, ok)
		assert.Contains(t, err.Error(), "is not authorized to create agents")
	})
	t.Run("delegator without agent.create", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		markEdgeBackfillComplete(t, f.store)
		u := hubMemberUser(t, f.store, "sched-no-create")
		a := createFixtureAgent(t, f, "sched-no-create-agent", []string{u.ID}, AgentRoleFull)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, u.ID, a.ID, f.proj.ID)
		ok, err := f.srv.authorizeScheduledAgentCreate(context.Background(), evt(f, a.ID))
		require.Error(t, err)
		assert.False(t, ok)
	})
	t.Run("soft-deleted creator agent", func(t *testing.T) {
		f, a, _ := setup(t)
		softDeleteStoredAgent(t, f.store, a.ID)
		ok, err := f.srv.authorizeScheduledAgentCreate(context.Background(), evt(f, a.ID))
		require.Error(t, err)
		assert.False(t, ok)
	})
}

// --- default service account parent authority ------------------------------

// TestAgentCreate_ServiceAccountParentAuthority pins how the parent authority
// for assigning a service account to an agent's child is evaluated on the
// actual account:
//   - a project-scoped account in the agent's project is assignable while the
//     agent's live delegator holds assign on that account (here as its
//     registrar); an account the delegator holds nothing on, or a deleted
//     delegator, denies at the delegation ceiling;
//   - for a hub-scoped account, a user delegator's authority includes the
//     hub-member assign relationship, while the agent itself holds no assign
//     authority on a hub-scoped account, so the decision denies before the
//     ceiling.
func TestAgentCreate_ServiceAccountParentAuthority(t *testing.T) {
	t.Run("project-scoped account", func(t *testing.T) {
		f := newParentCeilingFixture(t, "projsa")
		parent := tid("pc-projsa-parent")
		f.chain(t, parent)
		// The delegator registered sa and holds nothing on other.
		sa := gcpServiceAccountResource(&store.GCPServiceAccount{
			ID: tid("pc-projsa-sa"), CreatedBy: f.userID,
			Scope: store.ScopeProject, ScopeID: f.projectID, CreatedAt: time.Now(),
		})
		other := gcpServiceAccountResource(&store.GCPServiceAccount{
			ID: tid("pc-projsa-other"), CreatedBy: tid("pc-projsa-registrar"),
			Scope: store.ScopeProject, ScopeID: f.projectID, CreatedAt: time.Now(),
		})

		d := decidePerm(f.authz, f.agent(parent), sa, ActionAssign, "gcp_service_account.assign", false)
		assert.True(t, d.Allowed, "live delegator owns the account: reason %q", d.Reason)
		d = decidePerm(f.authz, f.agent(parent), other, ActionAssign, "gcp_service_account.assign", false)
		assertCeilingDeny(t, d, "delegator holds no assign authority on the account")

		require.NoError(t, f.store.DeleteUser(context.Background(), f.userID))
		d = decidePerm(f.authz, f.agent(parent), sa, ActionAssign, "gcp_service_account.assign", false)
		assertCeilingDeny(t, d, "deleted delegator")
	})

	t.Run("hub-scoped account", func(t *testing.T) {
		f := newParentCeilingFixture(t, "hubsa")
		ensureHubMembership(context.Background(), f.store, f.userID)
		parent := tid("pc-hubsa-parent")
		f.chain(t, parent)
		sa := gcpServiceAccountResource(&store.GCPServiceAccount{
			ID: tid("pc-hubsa-sa"), CreatedBy: tid("pc-hubsa-registrar"),
			Scope: store.ScopeHub, ScopeID: "hub", CreatedAt: time.Now(),
		})

		ok, reason, err := f.authz.evaluateUserDelegatorAuthority(context.Background(), f.userID,
			sa, ActionAssign, "gcp_service_account.assign", store.RoleScopeProject, f.projectID)
		require.NoError(t, err)
		assert.True(t, ok, "hub-member delegator: reason %q", reason)

		nonMember := tid("pc-hubsa-nonmember")
		createDCUser(t, f.store, nonMember, "hubsa-nonmember@pc.test", f.projectID, store.ProjectRoleMember)
		ok, reason, err = f.authz.evaluateUserDelegatorAuthority(context.Background(), nonMember,
			sa, ActionAssign, "gcp_service_account.assign", store.RoleScopeProject, f.projectID)
		require.NoError(t, err)
		assert.False(t, ok, "delegator outside the hub: reason %q", reason)

		d := decidePerm(f.authz, f.agent(parent), sa, ActionAssign, "gcp_service_account.assign", false)
		assert.False(t, d.Allowed, "agent on a hub-scoped account: reason %q", d.Reason)
		assert.Empty(t, d.DeniedBy, "the agent's own authority denies: reason %q", d.Reason)
	})
}
