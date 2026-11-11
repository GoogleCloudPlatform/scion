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
	"net/url"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Authority states of agents (ptone/scion#2143): computed at read from the
// recorded delegation chain, shown on the agent GET, on agent lists with
// include=authority, and in GET /api/v1/admin/orphaned-agents.

// authorityState computes agentID's state on f's AuthzService from a fresh
// read of the agent.
func authorityState(t *testing.T, f provenanceFixture, agentID string) api.AgentAuthority {
	t.Helper()
	ag, err := f.store.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	out, err := f.a.AgentAuthorityState(context.Background(), ag)
	require.NoError(t, err)
	return out
}

// revokeAgentEdges deactivates every active edge of agentID with cause and
// a new operation ID, as a forced user delete does for each agent it stops.
func revokeAgentEdges(t *testing.T, s store.Store, agentID string, cause store.EdgeDeactivationCause) string {
	t.Helper()
	opID := uuid.NewString()
	n, err := s.DeactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID,
		store.Deactivation{Cause: cause, OpID: opID})
	require.NoError(t, err)
	require.Positive(t, n, "the agent had an active edge")
	return opID
}

func TestAgentAuthorityState_Live(t *testing.T) {
	f := newProvenanceFixture(t, "auth-live", false)
	p := f.userChild(t, "auth-live-p", ceilPrincip)
	c := f.agentChild(t, "auth-live-c", p, boundedCeiling("agent.create"))
	for _, id := range []string{p.ID, c.ID} {
		assert.Equal(t, api.AgentAuthority{State: api.AgentAuthorityLive}, authorityState(t, f, id))
	}
}

// A child of a soft-deleted parent agent is orphaned with
// delegator_deleted, naming the parent and when it was deleted.
func TestAgentAuthorityState_ChildOfSoftDeletedParent(t *testing.T) {
	f := newProvenanceFixture(t, "auth-del-parent", false)
	p := f.userChild(t, "auth-del-parent-p", ceilPrincip)
	c := f.agentChild(t, "auth-del-parent-c", p, boundedCeiling("agent.create"))
	softDeleteStoredAgent(t, f.store, p.ID)

	got := authorityState(t, f, c.ID)
	assert.Equal(t, api.AgentAuthorityOrphaned, got.State)
	assert.Equal(t, api.AgentAuthorityCauseDelegatorDeleted, got.Cause)
	assert.Equal(t, &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalAgent, ID: p.ID, Deleted: true}, got.Principal)
	require.NotNil(t, got.Since, "the parent's deletion time")
	assert.True(t, got.Flagged())
}

// An agent whose root user is suspended is orphaned with
// delegator_inactive; the user is not deleted.
func TestAgentAuthorityState_RootUserSuspended(t *testing.T) {
	f := newProvenanceFixture(t, "auth-suspended", false)
	p := f.userChild(t, "auth-suspended-p", ceilPrincip)
	c := f.agentChild(t, "auth-suspended-c", p, boundedCeiling("agent.create"))
	setUserStatus(t, f.store, f.userID, "suspended")

	want := api.AgentAuthority{
		State:     api.AgentAuthorityOrphaned,
		Cause:     api.AgentAuthorityCauseDelegatorInactive,
		Principal: &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: f.userID},
	}
	assert.Equal(t, want, authorityState(t, f, p.ID))
	assert.Equal(t, want, authorityState(t, f, c.ID), "the walk reaches the same root user")
}

// The store writes of a forced user delete: a tombstone for the user and
// every edge of the user's agents deactivated with user_delete_cascade
// under the deletion's ID. Each such agent is revoked with that cause; a
// child the cascade did not stop is orphaned with delegator_revoked.
func TestAgentAuthorityState_ForceCascadeRevoked(t *testing.T) {
	f := newProvenanceFixture(t, "auth-cascade", false)
	ctx := context.Background()
	p := f.userChild(t, "auth-cascade-p", ceilPrincip)
	c := f.agentChild(t, "auth-cascade-c", p, boundedCeiling("agent.create"))

	requestedAt := time.Now().UTC().Truncate(time.Second)
	d := &store.UserDeletion{UserID: f.userID, RequestedByKind: store.DelegationPrincipalUser, RequestedByID: tid("auth-cascade-admin"), RequestedAt: requestedAt}
	require.NoError(t, f.store.CreateUserDeletion(ctx, d))
	n, err := f.store.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, p.ID,
		store.Deactivation{Cause: store.EdgeDeactivationUserDeleteCascade, OpID: d.ID})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got := authorityState(t, f, p.ID)
	assert.Equal(t, api.AgentAuthorityRevoked, got.State)
	assert.Equal(t, string(store.EdgeDeactivationUserDeleteCascade), got.Cause)
	assert.Equal(t, &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: f.userID, Deleted: true}, got.Principal,
		"the tombstone marks the user deleted while its row is present")
	require.NotNil(t, got.Since)

	got = authorityState(t, f, c.ID)
	assert.Equal(t, api.AgentAuthorityOrphaned, got.State)
	assert.Equal(t, api.AgentAuthorityCauseDelegatorRevoked, got.Cause)
	assert.Equal(t, &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalAgent, ID: p.ID}, got.Principal)
	require.NotNil(t, got.Since, "when the parent's delegation was deactivated")
}

// A plain user delete's delegator sweep (delegator_deleted) revokes too,
// with the cause recorded on the edge.
func TestAgentAuthorityState_DelegatorSweepRevoked(t *testing.T) {
	f := newProvenanceFixture(t, "auth-sweep", false)
	ag := f.userChild(t, "auth-sweep", ceilPrincip)
	revokeAgentEdges(t, f.store, ag.ID, store.EdgeDeactivationDelegatorDeleted)

	got := authorityState(t, f, ag.ID)
	assert.Equal(t, api.AgentAuthorityRevoked, got.State)
	assert.Equal(t, string(store.EdgeDeactivationDelegatorDeleted), got.Cause)
	assert.Equal(t, &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: f.userID}, got.Principal)
}

// Legacy agents are unrecorded, never orphaned: no edge at all, an edge
// written before provenance was recorded, the migration sentinel, and an
// agent with no active edge whose deactivation cause was not recorded.
func TestAgentAuthorityState_LegacyUnrecorded(t *testing.T) {
	f := newProvenanceFixture(t, "auth-legacy", false)
	unrecorded := api.AgentAuthority{State: api.AgentAuthorityUnrecorded}

	none := f.agent(t, "auth-legacy-none", AgentRoleFull)
	assert.Equal(t, unrecorded, authorityState(t, f, none.ID), "no edge")

	old := f.agent(t, "auth-legacy-old", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, old.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	assert.Equal(t, unrecorded, authorityState(t, f, old.ID), "unrecorded edge from an active user")

	sentinel := f.agent(t, "auth-legacy-sentinel", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, migrationDelegatorID, sentinel.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	assert.Equal(t, unrecorded, authorityState(t, f, sentinel.ID), "migration sentinel")

	child := f.agentChild(t, "auth-legacy-child", none, boundedCeiling("agent.create"))
	assert.Equal(t, unrecorded, authorityState(t, f, child.ID), "the parent never had an edge")
}

// An agent whose only delegation was deactivated without a recorded cause
// (an edge written inactive, as before causes were recorded) is unrecorded,
// not revoked. Control: the same agent shape with a recorded cause is
// revoked.
func TestAgentAuthorityState_CauselessDeactivationUnrecorded(t *testing.T) {
	f := newProvenanceFixture(t, "auth-causeless", false)
	ag := f.agent(t, "auth-causeless", AgentRoleFull)
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.userID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: ag.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, Role: string(AgentRoleFull), Active: false,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession),
		EffectCeiling:       ceilPrincip,
	}))
	assert.Equal(t, api.AgentAuthority{State: api.AgentAuthorityUnrecorded}, authorityState(t, f, ag.ID))

	control := f.userChild(t, "auth-causeless-control", ceilPrincip)
	revokeAgentEdges(t, f.store, control.ID, store.EdgeDeactivationDelegatorDeleted)
	assert.Equal(t, api.AgentAuthorityRevoked, authorityState(t, f, control.ID).State)
}

// An agent whose recorded user delegator row was deleted directly (as
// before deletes were refused) is orphaned with delegator_deleted.
func TestAgentAuthorityState_UserRowDeleted(t *testing.T) {
	f := newProvenanceFixture(t, "auth-user-gone", false)
	ctx := context.Background()
	gone := tid("auth-user-gone-delegator")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: gone, Email: "auth-user-gone@test.com", DisplayName: "gone", Role: store.UserRoleMember, Status: store.UserStatusActive}))
	ag := f.agent(t, "auth-user-gone", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, gone, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, gone, store.SourceCredentialSession))
	require.NoError(t, f.store.DeleteUser(ctx, gone))

	assert.Equal(t, api.AgentAuthority{
		State:     api.AgentAuthorityOrphaned,
		Cause:     api.AgentAuthorityCauseDelegatorDeleted,
		Principal: &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: gone, Deleted: true},
	}, authorityState(t, f, ag.ID))
}

func TestAgentAuthorityState_ChainInvalid(t *testing.T) {
	f := newProvenanceFixture(t, "auth-cycle", false)
	x := f.agent(t, "auth-cycle-x", AgentRoleFull)
	y := f.agent(t, "auth-cycle-y", AgentRoleFull)
	f.edge(t, store.DelegationPrincipalAgent, y.ID, x.ID, boundedCeiling("agent.create"), recordedProv(store.DelegationPrincipalAgent, y.ID, store.SourceCredentialAgent))
	f.edge(t, store.DelegationPrincipalAgent, x.ID, y.ID, boundedCeiling("agent.create"), recordedProv(store.DelegationPrincipalAgent, x.ID, store.SourceCredentialAgent))
	assert.Equal(t, api.AgentAuthority{State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseChainInvalid}, authorityState(t, f, x.ID))
}

// A soft-deleted agent reports the cause recorded when its own edges were
// deactivated.
func TestAgentAuthorityState_SoftDeletedAgent(t *testing.T) {
	f := newProvenanceFixture(t, "auth-self-del", false)
	ag := f.userChild(t, "auth-self-del", ceilPrincip)
	revokeAgentEdges(t, f.store, ag.ID, store.EdgeDeactivationAgentSoftDelete)
	softDeleteStoredAgent(t, f.store, ag.ID)

	got := authorityState(t, f, ag.ID)
	assert.Equal(t, api.AgentAuthorityRevoked, got.State)
	assert.Equal(t, string(store.EdgeDeactivationAgentSoftDelete), got.Cause)
}

// A lookup fault is an error, never a state.
func TestAgentAuthorityState_LookupFault(t *testing.T) {
	f := newProvenanceFixture(t, "auth-fault", false)
	ag := f.userChild(t, "auth-fault", ceilPrincip)
	a := f.authz(&faultStore{Store: f.store, edges: true}, false, false)
	_, err := a.AgentAuthorityState(context.Background(), ag)
	assert.ErrorIs(t, err, errProvenanceStoreFault)
}

// --- HTTP ---

// authorityHTTPFixture is a server with alice (owner of the project) and
// bob (a hub member outside it).
type authorityHTTPFixture struct {
	srv        *Server
	s          store.Store
	alice, bob *store.User
	project    *store.Project
}

func newAuthorityHTTPFixture(t *testing.T, srv *Server, s store.Store) *authorityHTTPFixture {
	t.Helper()
	alice, bob, project := setupDemoPolicyOn(t, srv, s)
	return &authorityHTTPFixture{srv: srv, s: s, alice: alice, bob: bob, project: project}
}

// agent creates an agent owned by alice in the project.
func (f *authorityHTTPFixture) agent(t *testing.T, name string) *store.Agent {
	t.Helper()
	id := tid("authz-http-" + name)
	createDCAgent(t, f.s, id, f.project.ID, f.alice.ID, AgentRoleFull)
	ag, err := f.s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return ag
}

// edge records an active project edge from (kind, id) to agentID.
func (f *authorityHTTPFixture) edge(t *testing.T, kind, id, agentID string) {
	t.Helper()
	cred := store.SourceCredentialSession
	ceiling := ceilPrincip
	if kind == store.DelegationPrincipalAgent {
		cred = store.SourceCredentialAgent
		ceiling = boundedCeiling("agent.create")
	}
	require.NoError(t, f.s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: kind, DelegatorID: id,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: agentID,
		ScopeType: store.RoleScopeProject, ScopeID: f.project.ID, Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: recordedProv(kind, id, cred),
		EffectCeiling:       ceiling,
	}))
}

// user creates an active user with no bindings.
func (f *authorityHTTPFixture) user(t *testing.T, name string) string {
	t.Helper()
	id := tid("authz-http-user-" + name)
	require.NoError(t, f.s.CreateUser(context.Background(), &store.User{ID: id, Email: name + "@authz.test", DisplayName: name, Role: store.UserRoleMember, Status: store.UserStatusActive}))
	return id
}

func decodeAgentAuthority(t *testing.T, body []byte) *api.AgentAuthority {
	t.Helper()
	var resp struct {
		Authority *api.AgentAuthority `json:"authority"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	return resp.Authority
}

// The agent GET always carries the state. principal.id is shown to a hub
// admin and hidden from a project member for a user principal; an agent
// principal in the project is shown to the member, who may read it.
func TestAgentAuthority_GetIncludesStateWithPrincipalVisibility(t *testing.T) {
	srv, s := testServer(t)
	f := newAuthorityHTTPFixture(t, srv, s)

	live := f.agent(t, "get-live")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, live.ID)
	rec := doRequestAsUser(t, srv, f.alice, http.MethodGet, "/api/v1/agents/"+live.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, &api.AgentAuthority{State: api.AgentAuthorityLive}, decodeAgentAuthority(t, rec.Body.Bytes()))

	carol := f.user(t, "carol")
	orphan := f.agent(t, "get-orphan")
	f.edge(t, store.DelegationPrincipalUser, carol, orphan.ID)
	setUserStatus(t, s, carol, "suspended")

	rec = doRequestAsUser(t, srv, f.alice, http.MethodGet, "/api/v1/agents/"+orphan.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, &api.AgentAuthority{
		State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorInactive,
		Principal: &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser},
	}, decodeAgentAuthority(t, rec.Body.Bytes()), "a project member does not see the user's ID")
	assert.NotContains(t, rec.Body.String(), carol)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+orphan.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, carol, decodeAgentAuthority(t, rec.Body.Bytes()).Principal.ID, "a hub admin sees the user's ID")

	// The project-scoped GET answers the same.
	rec = doRequestAsUser(t, srv, f.alice, http.MethodGet, "/api/v1/projects/"+f.project.ID+"/agents/"+orphan.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, api.AgentAuthorityOrphaned, decodeAgentAuthority(t, rec.Body.Bytes()).State)

	parent := f.agent(t, "get-parent")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, parent.ID)
	child := f.agent(t, "get-child")
	f.edge(t, store.DelegationPrincipalAgent, parent.ID, child.ID)
	softDeleteStoredAgent(t, s, parent.ID)
	rec = doRequestAsUser(t, srv, f.alice, http.MethodGet, "/api/v1/agents/"+child.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeAgentAuthority(t, rec.Body.Bytes())
	require.NotNil(t, got)
	assert.Equal(t, api.AgentAuthorityCauseDelegatorDeleted, got.Cause)
	assert.Equal(t, &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalAgent, ID: parent.ID, Deleted: true}, got.Principal,
		"the member may read the parent agent")

	// bob may not read the agent at all.
	rec = doRequestAsUser(t, srv, f.bob, http.MethodGet, "/api/v1/agents/"+orphan.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// An agent reads its own state with its agent token (the self-read on the
// agent GET). The token is minted while the root user is active; once the
// user is suspended the agent is orphaned, and the user's ID stays hidden
// from the agent, which may not read users.
func TestAgentAuthority_AgentReadsOwnState(t *testing.T) {
	srv, s := testServer(t)
	f := newAuthorityHTTPFixture(t, srv, s)
	ag := f.agent(t, "self-read")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, ag.ID)
	token, err := srv.issueAgentTokenForTest(context.Background(), ag)
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+ag.ID, nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, &api.AgentAuthority{State: api.AgentAuthorityLive}, decodeAgentAuthority(t, rec.Body.Bytes()))

	setUserStatus(t, s, f.alice.ID, "suspended")
	rec = doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+ag.ID, nil, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, &api.AgentAuthority{
		State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorInactive,
		Principal: &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser},
	}, decodeAgentAuthority(t, rec.Body.Bytes()), "the agent sees the cause but not the user's ID")
}

// listAuthorities decodes an agent list into agent ID → authority.
func listAuthorities(t *testing.T, body []byte) map[string]*api.AgentAuthority {
	t.Helper()
	var resp struct {
		Agents []struct {
			ID        string              `json:"id"`
			Authority *api.AgentAuthority `json:"authority"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	out := map[string]*api.AgentAuthority{}
	for _, a := range resp.Agents {
		out[a.ID] = a.Authority
	}
	return out
}

// Agent lists carry the state only with include=authority, on both
// endpoints, legacy and sorted; the compact view never does.
func TestAgentAuthority_ListOnlyWithInclude(t *testing.T) {
	srv, s := testServer(t)
	f := newAuthorityHTTPFixture(t, srv, s)
	live := f.agent(t, "list-live")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, live.ID)
	revoked := f.agent(t, "list-revoked")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, revoked.ID)
	revokeAgentEdges(t, s, revoked.ID, store.EdgeDeactivationUserDeleteCascade)

	for _, base := range []string{"/api/v1/agents", "/api/v1/projects/" + f.project.ID + "/agents"} {
		for _, mode := range []string{"", "sort=updated&fit=500&"} {
			rec := doRequest(t, srv, http.MethodGet, base+"?"+mode+"projectId="+f.project.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			for id, auth := range listAuthorities(t, rec.Body.Bytes()) {
				assert.Nil(t, auth, "%s %s: %s carries no authority without include", base, mode, id)
			}

			rec = doRequest(t, srv, http.MethodGet, base+"?"+mode+"include=authority&projectId="+f.project.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got := listAuthorities(t, rec.Body.Bytes())
			require.Contains(t, got, live.ID, "%s %s", base, mode)
			require.Contains(t, got, revoked.ID, "%s %s", base, mode)
			assert.Equal(t, &api.AgentAuthority{State: api.AgentAuthorityLive}, got[live.ID], "%s %s", base, mode)
			require.NotNil(t, got[revoked.ID], "%s %s", base, mode)
			assert.Equal(t, api.AgentAuthorityRevoked, got[revoked.ID].State, "%s %s", base, mode)
			assert.Equal(t, string(store.EdgeDeactivationUserDeleteCascade), got[revoked.ID].Cause, "%s %s", base, mode)

			rec = doRequest(t, srv, http.MethodGet, base+"?"+mode+"view=compact&include=authority&projectId="+f.project.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), `"authority"`, "%s %s: compact view", base, mode)
		}
	}
}

func TestAgentListIncludesAuthority(t *testing.T) {
	for raw, want := range map[string]bool{
		"":                              false,
		"include=authority":             true,
		"include=foo,authority":         true,
		"include=foo&include=authority": true,
		"include=authorityx":            false,
		"include=foo":                   false,
	} {
		q, err := url.ParseQuery(raw)
		require.NoError(t, err)
		assert.Equal(t, want, agentListIncludesAuthority(q), raw)
	}
}

// authorityCountingStore counts, once armed, the reads the authority state
// makes.
type authorityCountingStore struct {
	store.Store
	fault *storeFaultSwitch

	mu             sync.Mutex
	edgeReads      int
	historyReads   int
	tombstoneReads int
	userReads      map[string]int
}

func (c *authorityCountingStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edgeReads, c.historyReads, c.tombstoneReads = 0, 0, 0
	c.userReads = map[string]int{}
}

func (c *authorityCountingStore) count(f func()) {
	if c.fault.Active() {
		c.mu.Lock()
		f()
		c.mu.Unlock()
	}
}

func (c *authorityCountingStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	c.count(func() { c.edgeReads++ })
	return c.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

func (c *authorityCountingStore) ListAllDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	c.count(func() { c.historyReads++ })
	return c.Store.ListAllDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

func (c *authorityCountingStore) GetUserDeletionByUser(ctx context.Context, userID string) (*store.UserDeletion, error) {
	c.count(func() { c.tombstoneReads++ })
	return c.Store.GetUserDeletionByUser(ctx, userID)
}

func (c *authorityCountingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.count(func() {
		if c.userReads == nil {
			c.userReads = map[string]int{}
		}
		c.userReads[id]++
	})
	return c.Store.GetUser(ctx, id)
}

// A list without include=authority makes none of the authority reads.
// With it, each agent's edges are read once however many children share
// it, the user at the root once, and the history and tombstone only for
// the agent with no active delegation.
func TestAgentAuthority_ListQueryCount(t *testing.T) {
	srv, s, counting, fault := testServerWithStoreFault(t, func(inner store.Store, fs *storeFaultSwitch) *authorityCountingStore {
		return &authorityCountingStore{Store: inner, fault: fs}
	})
	// The authority state reads through the authorization service's store.
	srv.authzService.store = counting
	f := newAuthorityHTTPFixture(t, srv, s)

	parent := f.agent(t, "qc-parent")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, parent.ID)
	for _, name := range []string{"qc-c1", "qc-c2", "qc-c3"} {
		c := f.agent(t, name)
		f.edge(t, store.DelegationPrincipalAgent, parent.ID, c.ID)
	}
	revoked := f.agent(t, "qc-revoked")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, revoked.ID)
	revokeAgentEdges(t, s, revoked.ID, store.EdgeDeactivationUserDeleteCascade)

	path := "/api/v1/projects/" + f.project.ID + "/agents"
	fault.Arm()

	counting.reset()
	rec := doRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, listAuthorities(t, rec.Body.Bytes()), 5)
	counting.mu.Lock()
	assert.Zero(t, counting.edgeReads, "edge reads without include")
	assert.Zero(t, counting.historyReads, "edge history reads without include")
	assert.Zero(t, counting.tombstoneReads, "tombstone reads without include")
	baseAlice := counting.userReads[f.alice.ID]
	counting.mu.Unlock()

	counting.reset()
	rec = doRequest(t, srv, http.MethodGet, path+"?include=authority", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := listAuthorities(t, rec.Body.Bytes())
	require.Len(t, got, 5)
	var states []string
	for _, a := range got {
		require.NotNil(t, a)
		states = append(states, a.State)
	}
	sort.Strings(states)
	assert.Equal(t, []string{"live", "live", "live", "live", "revoked"}, states)
	counting.mu.Lock()
	defer counting.mu.Unlock()
	assert.Equal(t, 5, counting.edgeReads, "one edge read per agent; the parent's is shared by its children")
	assert.Equal(t, 1, counting.historyReads, "history only for the revoked agent")
	assert.Equal(t, 1, counting.tombstoneReads, "the revoked agent's delegator")
	assert.LessOrEqual(t, counting.userReads[f.alice.ID]-baseAlice, 1, "the root user is read once")
}

// --- admin listing ---

func TestAdminOrphanedAgents_Access(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	f := newAuthorityHTTPFixture(t, srv, s)
	path := "/api/v1/admin/orphaned-agents"

	rec := doRequestAsUser(t, srv, f.bob, http.MethodGet, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "without hub.health.read: %s", rec.Body.String())

	admin := newHubAdminRoleUser(t, s, "authz-orphans-hub-admin")
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"agents":[],"scanned":0}`, rec.Body.String())

	rec = doRequestAsUser(t, srv, admin, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, path+"?state=live", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodGet, path+"?limit=0", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// A user access token is refused even with hub.health.read on the hub
// boundary: the listing is session only.
func TestAdminOrphanedAgents_BearerRefused(t *testing.T) {
	srv, s := testServer(t)
	super := hubConfigTokenUser(t, s, "authz-orphans-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, super, hubBoundary(), "hub_health:read")
	rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/admin/orphaned-agents", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "GOV_PENDING")

	// The same token reads the health summary, so the refusal is the
	// session-only rule, not the permission.
	rec = doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/admin/health/summary", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

type orphanedAgentsPage struct {
	Agents []struct {
		ID        string             `json:"id"`
		ProjectID string             `json:"projectId"`
		Authority api.AgentAuthority `json:"authority"`
	} `json:"agents"`
	Scanned    int    `json:"scanned"`
	NextCursor string `json:"nextCursor"`
}

// The listing pages over the hub's agents and finds an agent orphaned by a
// user row deleted directly in the store, and a revoked agent; it skips
// live and unrecorded agents. state narrows it.
func TestAdminOrphanedAgents_FindsOrphansAcrossPages(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	f := newAuthorityHTTPFixture(t, srv, s)
	ctx := context.Background()
	admin := newHubAdminRoleUser(t, s, "authz-orphans-pager")

	live := f.agent(t, "page-live")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, live.ID)
	f.agent(t, "page-unrecorded")
	gone := f.user(t, "page-gone")
	orphan := f.agent(t, "page-orphan")
	f.edge(t, store.DelegationPrincipalUser, gone, orphan.ID)
	require.NoError(t, s.DeleteUser(ctx, gone))
	revoked := f.agent(t, "page-revoked")
	f.edge(t, store.DelegationPrincipalUser, f.alice.ID, revoked.ID)
	revokeAgentEdges(t, s, revoked.ID, store.EdgeDeactivationUserDeleteCascade)

	collect := func(query string) (map[string]api.AgentAuthority, int) {
		t.Helper()
		found := map[string]api.AgentAuthority{}
		scanned, cursor := 0, ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 10, "paging ends")
			path := "/api/v1/admin/orphaned-agents?limit=1" + query
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			rec := doRequestAsUser(t, srv, admin, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var page orphanedAgentsPage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
			assert.LessOrEqual(t, len(page.Agents), 1)
			scanned += page.Scanned
			for _, a := range page.Agents {
				found[a.ID] = a.Authority
			}
			if page.NextCursor == "" {
				return found, scanned
			}
			cursor = page.NextCursor
		}
	}

	found, scanned := collect("")
	assert.Equal(t, 4, scanned)
	require.Len(t, found, 2)
	assert.Equal(t, api.AgentAuthority{
		State: api.AgentAuthorityOrphaned, Cause: api.AgentAuthorityCauseDelegatorDeleted,
		Principal: &api.AgentAuthorityPrincipal{Kind: store.DelegationPrincipalUser, ID: gone, Deleted: true},
	}, found[orphan.ID], "the hub admin sees the user's ID")
	assert.Equal(t, api.AgentAuthorityRevoked, found[revoked.ID].State)
	assert.Equal(t, string(store.EdgeDeactivationUserDeleteCascade), found[revoked.ID].Cause)

	found, _ = collect("&state=revoked")
	require.Len(t, found, 1)
	assert.Contains(t, found, revoked.ID)
}
