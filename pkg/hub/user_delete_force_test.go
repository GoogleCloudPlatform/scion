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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the forced user delete (ptone/scion#2143):
// DELETE /api/v1/users/{id}?force=true, the deletion resource, the stop
// work and the completion.

// enableUserDeleteForce turns hub.user_delete_force_cascade on for srv.
func enableUserDeleteForce(t *testing.T, srv *Server) {
	t.Helper()
	setUserDeleteForce(t, srv, true)
}

// setUserDeleteForce turns hub.user_delete_force_cascade on or off for srv.
func setUserDeleteForce(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == experiments.UserDeleteForceCascade {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.UserDeleteForceCascade))
}

// forceDelFixture is a hub with a broker-backed project, the force
// experiment on and a stub dispatcher, and a target user, dave.
type forceDelFixture struct {
	srv     *Server
	s       store.Store
	disp    *engineStubDispatcher
	project *store.Project
	broker  *store.RuntimeBroker
	dave    *store.User
}

func newForceDelFixture(t *testing.T, retention time.Duration) *forceDelFixture {
	t.Helper()
	srv, s, _, disp := engineTestServer(t)
	srv.config.SoftDeleteRetention = retention
	enableUserDeleteForce(t, srv)
	ctx := context.Background()
	project := &store.Project{ID: tid("proj-force-del"), Name: "Force Delete", Slug: "force-delete"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{ID: tid("broker-force-del"), Name: "Force Delete Broker", Slug: "force-delete-broker",
		Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	return &forceDelFixture{srv: srv, s: s, disp: disp, project: project, broker: broker,
		dave: newActiveMember(t, s, "user-dave", "dave@test.com")}
}

// agent creates a running agent of dave's on the fixture broker. owner is
// dave for a top-level agent or the parent agent for a child, whose
// ancestry is [dave, parent].
func (f *forceDelFixture) agent(t *testing.T, slug string, parent *store.Agent) *store.Agent {
	t.Helper()
	a := &store.Agent{ID: tid("agent-" + slug), Slug: slug, Name: slug, ProjectID: f.project.ID,
		RuntimeBrokerID: f.broker.ID, Phase: string(state.PhaseRunning),
		OwnerID: f.dave.ID, CreatedBy: f.dave.ID, Ancestry: []string{f.dave.ID}}
	if parent != nil {
		a.OwnerID, a.CreatedBy, a.Ancestry = parent.ID, parent.ID, []string{f.dave.ID, parent.ID}
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	return a
}

// agentEdge records an active project edge from a delegator to agent a.
func (f *forceDelFixture) agentEdge(t *testing.T, delegatorType, delegatorID string, a *store.Agent) *store.DelegationEdge {
	t.Helper()
	e := &store.DelegationEdge{DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: a.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.project.ID, Role: "member", Active: true}
	require.NoError(t, f.s.CreateDelegationEdge(context.Background(), e))
	return e
}

// uat gives dave an unrevoked hub-boundary access token.
func (f *forceDelFixture) uat(t *testing.T, name string) string {
	t.Helper()
	id := tid("uat-" + name)
	require.NoError(t, f.s.CreateUserAccessToken(context.Background(), &store.UserAccessToken{
		ID: id, UserID: f.dave.ID, Name: name, Prefix: "pfx-" + name, KeyHash: "hash-" + id,
		BoundaryKind: "hub", Scopes: []string{"agent:read"},
	}))
	return id
}

func (f *forceDelFixture) forceDelete(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, f.srv, http.MethodDelete, "/api/v1/users/"+f.dave.ID+"?force=true", nil)
}

// decodeDeletion decodes a deletion resource.
func decodeDeletion(t *testing.T, rec *httptest.ResponseRecorder) userDeletionView {
	t.Helper()
	var v userDeletionView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

// getDeletion reads GET /api/v1/users/{id}/deletion as the dev admin.
func (f *forceDelFixture) getDeletion(t *testing.T) userDeletionView {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/users/"+f.dave.ID+"/deletion", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decodeDeletion(t, rec)
}

// waitCompleted waits until dave's deletion is completed and returns it.
func (f *forceDelFixture) waitCompleted(t *testing.T) userDeletionView {
	t.Helper()
	require.Eventually(t, func() bool {
		d, err := f.s.GetUserDeletionByUser(context.Background(), f.dave.ID)
		return err == nil && d.State == store.UserDeletionStateCompleted
	}, 15*time.Second, 20*time.Millisecond, "the deletion completes")
	return f.getDeletion(t)
}

// auditsOf returns the mutation audit records of type for target.
func auditsOf(t *testing.T, s store.Store, mutationType, targetID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType, TargetID: targetID})
	require.NoError(t, err)
	return recs
}

// requireEdgeCause asserts edge id is inactive with cause and opID.
func requireEdgeCause(t *testing.T, s store.Store, id string, cause store.EdgeDeactivationCause, opID string) {
	t.Helper()
	e, err := s.GetDelegationEdge(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, e.Active, "edge %s is inactive", id)
	assert.Equal(t, cause, e.Deactivation.Cause, "edge %s cause", id)
	assert.Equal(t, opID, e.Deactivation.OpID, "edge %s operation", id)
}

// runForceCascade drives the whole force delete of dave with three agents
// (a, its child b, and c), edges, an assignment and access tokens, and
// checks the revoke, the tombstone, the stop work and the completion.
// It returns the completed resource and the agents.
func runForceCascade(t *testing.T, f *forceDelFixture) (userDeletionView, []*store.Agent) {
	t.Helper()
	ctx := context.Background()
	b0 := useLocalSecretBackend(t, f.srv, f.s)
	seedUserScopedData(t, f.s, b0, f.dave.ID)
	a := f.agent(t, "fd-a", nil)
	b := f.agent(t, "fd-b", a)
	c := f.agent(t, "fd-c", nil)
	edgeA := f.agentEdge(t, store.DelegationPrincipalUser, f.dave.ID, a)
	edgeB := f.agentEdge(t, store.DelegationPrincipalAgent, a.ID, b)
	edgeC := f.agentEdge(t, store.DelegationPrincipalUser, f.dave.ID, c)
	sa := wiringSA(t, f.s, store.ScopeProject, f.project.ID, "fd-a-sa@fd.iam.gserviceaccount.com")
	require.NoError(t, f.s.ReplaceAgentServiceAccountAssignment(ctx, &store.AgentServiceAccountAssignment{
		AgentID: a.ID, ProjectID: f.project.ID, ServiceAccountID: sa.ID,
		Origin:              store.SAAssignmentOriginCreateExplicit,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.dave.ID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
	f.uat(t, "one")
	f.uat(t, "two")
	// A grant dave issued to another user's agent, which the cascade does
	// not stop: the completion revokes every grant dave issued.
	other := createOwnedAgent(t, f.s, "fd-other", f.project.ID, DevUserID)
	grant := seedLifecycleGrant(t, f.s, other, f.dave.ID)

	// Hold the teardown so the request's own writes can be checked
	// before the engines run.
	entered := make(chan struct{})
	release := make(chan struct{})
	f.disp.setFn(blockingDelete(entered, release, nil))

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	v := decodeDeletion(t, rec)
	require.NotEmpty(t, v.DeletionID)
	assert.Equal(t, f.dave.ID, v.UserID)
	assert.Equal(t, store.UserDeletionStateStopping, v.State)
	assert.Equal(t, userDeletionPrincipal{Kind: "user", ID: DevUserID}, v.RequestedBy)
	assert.Len(t, v.Agents, 3)

	// Phase A: one transaction revoked the user's authority.
	u, err := f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusDeleting, u.Status)
	n, err := f.s.CountUserAccessTokens(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Zero(t, n, "the user's access tokens are revoked")
	for _, e := range []*store.DelegationEdge{edgeA, edgeB, edgeC} {
		requireEdgeCause(t, f.s, e.ID, store.EdgeDeactivationUserDeleteCascade, v.DeletionID)
	}
	active, err := f.s.GetActiveAgentServiceAccountAssignments(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, active, "the agent's assignment is deactivated")
	assert.Len(t, auditsOf(t, f.s, "user_delete_requested", f.dave.ID), 1)
	_, err = f.s.GetUserDeletionByUser(ctx, f.dave.ID)
	require.NoError(t, err, "the tombstone is recorded")

	<-entered
	close(release)
	done := f.waitCompleted(t)

	_, err = f.s.GetUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the user row is gone")
	assert.Equal(t, userDeletionCounts{Stopped: 3}, done.Counts)
	require.NotNil(t, done.CompletedAt)
	completion := auditsOf(t, f.s, "user_delete", f.dave.ID)
	require.Len(t, completion, 1)
	var after struct {
		Stopped int `json:"stopped"`
		Removed int `json:"removed"`
	}
	require.NoError(t, json.Unmarshal([]byte(completion[0].AfterSummary), &after))
	assert.Equal(t, 3, after.Stopped)
	assert.Equal(t, 0, after.Removed)
	sec, ev := userScopedCounts(t, f.s, f.dave.ID)
	assert.Zero(t, sec, "afterUserDeleted removed the user-scope secrets")
	assert.Zero(t, ev, "afterUserDeleted removed the user-scope env vars")
	// afterUserDeleted runs after the completion commits.
	require.Eventually(t, func() bool {
		g, err := f.s.GetAgentDelegationGrant(ctx, grant.grant.ID)
		return err == nil && g.RevokedAt != nil
	}, 15*time.Second, 20*time.Millisecond, "the grant dave issued is revoked")
	assertGrantRevoked(t, f.s, grant, revokeReasonIssuerDeleted)
	assert.True(t, mustGetAgent(t, f.s, other.ID).DeletedAt.IsZero(), "the other user's agent is not stopped")
	return done, []*store.Agent{a, b, c}
}

// With soft-delete retention, the agents are soft-deleted (teardown
// confirmed): stopped.
func TestForceDeleteUser_RevokesStopsAndCompletes_SoftDelete(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	_, agents := runForceCascade(t, f)
	for _, a := range agents {
		got := mustGetAgent(t, f.s, a.ID)
		assert.False(t, got.DeletedAt.IsZero(), "agent %s is soft-deleted", a.Slug)
	}
}

// With no retention, the engine removes each row after confirmed
// teardown: reported stopped, not removed.
func TestForceDeleteUser_RevokesStopsAndCompletes_NoRetention(t *testing.T) {
	f := newForceDelFixture(t, 0)
	_, agents := runForceCascade(t, f)
	for _, a := range agents {
		assert.True(t, agentGone(t, f.s, a.ID), "agent %s row is removed", a.Slug)
	}
}

// Force with nothing blocking is a plain delete: 204 and no tombstone.
func TestForceDeleteUser_NoBlockersIsPlainDelete(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	rec := f.forceDelete(t)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := f.s.GetUser(context.Background(), f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.GetUserDeletionByUser(context.Background(), f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Empty(t, auditsOf(t, f.s, "user_delete_requested", f.dave.ID))
}

// startHeldDeletion force-deletes dave with one agent whose teardown is
// held, and returns the deletion and the release channel.
func startHeldDeletion(t *testing.T, f *forceDelFixture) (userDeletionView, *store.Agent, chan struct{}) {
	t.Helper()
	a := f.agent(t, "held", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.disp.setFn(blockingDelete(entered, release, nil))
	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-entered
	return decodeDeletion(t, rec), a, release
}

// A repeated force request while the deletion is in progress joins it.
func TestForceDeleteUser_RepeatJoins(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	v, _, release := startHeldDeletion(t, f)
	defer close(release)

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	again := decodeDeletion(t, rec)
	assert.Equal(t, v.DeletionID, again.DeletionID)
	assert.Len(t, auditsOf(t, f.s, "user_delete_requested", f.dave.ID), 1, "a join writes nothing")
}

// While the deletion is in progress the user and its tree are refused.
func TestForceDeleteUser_StoppingRefusals(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	gone := softDeletedAgent(t, f.s, "dave-old", f.project.ID, f.dave.ID, []string{f.dave.ID})
	v, held, release := startHeldDeletion(t, f)
	defer close(release)

	// The user's credentials fail the account check.
	_, err := checkUserAccount(ctx, f.s, PrincipalContext{Kind: PrincipalKindUser, ID: f.dave.ID})
	require.Error(t, err)
	u, _ := f.s.GetUser(ctx, f.dave.ID)
	assert.Equal(t, accountNotActive, classifyUserAccount(u, err))

	// The agent holds no active edge: only the depth-0 read allowance
	// remains for it.
	edges, err := f.s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, held.ID)
	require.NoError(t, err)
	assert.Empty(t, edges)
	// A non-read decision for the agent as the principal is denied.
	stored := mustGetAgent(t, f.s, held.ID)
	decision := f.srv.agentCreateDecision(ctx, &storedAgentIdentity{agent: stored}, f.project.ID)
	assert.False(t, decision.Allowed, "an agent of a deleting user may not create agents")

	// Restore in the user's tree.
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+gone.ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "the user it belongs to is being deleted")
	assert.False(t, mustGetAgent(t, f.s, gone.ID).DeletedAt.IsZero())

	// Create of a child of an agent in the tree.
	child := &store.Agent{ID: tid("agent-held-child"), Slug: "held-child", Name: "held-child",
		ProjectID: f.project.ID, Phase: "created", OwnerID: held.ID, CreatedBy: held.ID,
		Ancestry: []string{f.dave.ID, held.ID}}
	err = f.srv.commitAgentCreate(ctx, agentCreateWrite{
		Provenance: store.AuthorityProvenance{ProvenanceVersion: 1},
		Agent:      child,
		Slug:       child.Slug,
		Edge: &store.DelegationEdge{DelegatorType: store.DelegationPrincipalAgent, DelegatorID: held.ID,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
			ScopeID: f.project.ID, Role: string(AgentRoleNone), Active: true},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
	})
	require.ErrorIs(t, err, errAgentOwnerUserDeleting)
	_, err = f.s.GetAgent(ctx, child.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	// A plain delete and a PATCH answer 409 user_deletion_in_progress.
	for _, tc := range []struct {
		method string
		body   interface{}
	}{{http.MethodDelete, nil}, {http.MethodPatch, map[string]interface{}{"status": "active"}}, {http.MethodPatch, map[string]interface{}{"displayName": "D"}}} {
		rec := doRequest(t, f.srv, tc.method, "/api/v1/users/"+f.dave.ID, tc.body)
		require.Equal(t, http.StatusConflict, rec.Code, "%s: %s", tc.method, rec.Body.String())
		code, details := errorBody(t, rec)
		assert.Equal(t, ErrCodeUserDeletionInProgress, code)
		assert.Equal(t, v.DeletionID, details["deletionId"])
	}
	u, err = f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusDeleting, u.Status, "nothing moves the status out of deleting")

	// The blocker listing names the deletion.
	rec = doRequest(t, f.srv, http.MethodGet, "/api/v1/users/"+f.dave.ID+"/delete-blockers", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing userDeleteBlockersResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	require.NotNil(t, listing.InProgress)
	assert.Equal(t, v.DeletionID, listing.InProgress.DeletionID)
}

// The caller lacks agent.delete on the user's agents: 403 and nothing is
// written.
func TestForceDeleteUser_ForbiddenWritesNothing(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	enableUserDeleteForce(t, srv)
	ctx := context.Background()
	target := newActiveMember(t, s, "user-target", "target@test.com")
	projectID := pvSeedProject(t, s, "force-forbidden-project")
	a := createOwnedAgent(t, s, "target-agent", projectID, target.ID)
	edge := createUserEdge(t, s, target.ID, store.DelegationPrincipalAgent, a.ID, projectID)
	actor := bulkInviteOnlyCaller(t, s, "deleter", "user.delete")

	rec := doRequestAsUser(t, srv, actor, http.MethodDelete, "/api/v1/users/"+target.ID+"?force=true", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := errorBody(t, rec)
	assert.Equal(t, ErrCodeUserDeleteForceForbidden, code)
	agents, _ := details["agents"].([]interface{})
	require.Len(t, agents, 1)
	assert.Equal(t, a.ID, agents[0].(map[string]interface{})["id"])

	u, err := s.GetUser(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	_, err = s.GetUserDeletionByUser(ctx, target.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	e, err := s.GetDelegationEdge(ctx, edge.ID)
	require.NoError(t, err)
	assert.True(t, e.Active)
	assert.Empty(t, auditsOf(t, s, "user_delete_requested", target.ID))
}

// While the experiment is off, force is ignored: the plain refusal.
func TestForceDeleteUser_ExperimentOffIgnoresForce(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	dave := newActiveMember(t, s, "user-dave", "dave@test.com")
	a := createOwnedAgent(t, s, "dave-agent", project.ID, dave.ID)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID+"?force=true", nil)
	requireOwnsAgentsDenial(t, rec, a)
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/users/"+dave.ID+"/deletion", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// The blockers changed between the authorization and the transaction:
// errUserDeleteBlockersChanged, and the transaction writes nothing.
func TestForceDeleteUser_BlockersChangedWritesNothing(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	a := f.agent(t, "bc-a", nil)
	late := f.agent(t, "bc-late", nil)
	edge := f.agentEdge(t, store.DelegationPrincipalUser, f.dave.ID, a)
	f.uat(t, "bc")
	actor := NewAuthenticatedUser(DevUserID, "dev@test.com", "Dev", "admin", string(ClientTypeAPI))

	err := f.s.WithTx(ctx, func(tx store.Store) error {
		_, _, err := f.srv.forceDeleteUserTx(ctx, tx, actor, f.dave, map[string]bool{a.ID: true}, AuditActor{})
		return err
	})
	require.ErrorIs(t, err, errUserDeleteBlockersChanged, "agent %s blocks but was not authorized", late.Slug)

	u, err := f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	n, err := f.s.CountUserAccessTokens(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	e, err := f.s.GetDelegationEdge(ctx, edge.ID)
	require.NoError(t, err)
	assert.True(t, e.Active)
	_, err = f.s.GetUserDeletionByUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// Through the route: an agent created after the decisions makes the
// request 409 user_delete_blockers_changed, and nothing is written.
func TestForceDeleteUser_BlockersChangedRoute409(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	f.agent(t, "bcr-a", nil)
	forceDeleteUserBeforeTx = func() { f.agent(t, "bcr-late", nil) }
	t.Cleanup(func() { forceDeleteUserBeforeTx = nil })

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeUserDeleteBlockersChanged, code)
	u, err := f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	_, err = f.s.GetUserDeletionByUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// The last-owner guard refuses under force too, before any write.
func TestForceDeleteUser_LastOwnerStillRefused(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	enableUserDeleteForce(t, srv)
	ctx := context.Background()
	daveID := tid("user-lastowner-dave")
	projectID := tid("proj-lastowner")
	createRS1Project(t, s, projectID, daveID)
	a := createOwnedAgent(t, s, "lastowner-agent", projectID, daveID)
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID: tid("uat-lastowner"), UserID: daveID, Name: "lo", Prefix: "pfx-lo", KeyHash: "hash-lastowner",
		BoundaryKind: "hub", Scopes: []string{"agent:read"},
	}))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+daveID+"?force=true", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeLastOwner, code)
	u, err := s.GetUser(ctx, daveID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	n, err := s.CountUserAccessTokens(ctx, daveID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the transaction rolled back: tokens kept")
	_, err = s.GetUserDeletionByUser(ctx, daveID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.True(t, mustGetAgent(t, s, a.ID).DeletedAt.IsZero())
}

// A token credential is refused with force, as without it.
func TestForceDeleteUser_TokenCredentialRefused(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	f.agent(t, "tok-a", nil)
	actor := NewAuthenticatedUser(DevUserID, "dev@test.com", "Dev", "admin", string(ClientTypeAPI))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+f.dave.ID+"?force=true", nil)
	ctx := contextWithCredentialContext(contextWithIdentity(req.Context(), actor), CredentialContext{Kind: CredentialKindUAT})
	rec := httptest.NewRecorder()
	f.srv.deleteUser(rec, req.WithContext(ctx), f.dave.ID)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "force delete with a token")
	_, err := f.s.GetUserDeletionByUser(context.Background(), f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// An agent whose teardown fails stays failed and keeps the deletion
// stopping, with the error visible; once an operator force-deletes it, it
// is reported removed (teardown not confirmed) and the deletion completes.
func TestForceDeleteUser_FailedAgentUntilOperatorRemoves(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ok := f.agent(t, "fa-ok", nil)
	stuck := f.agent(t, "fa-stuck", nil)
	f.disp.setFn(func(_ context.Context, a *store.Agent) error {
		if a.ID == stuck.ID {
			return errors.New("broker unreachable")
		}
		return nil
	})

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var v userDeletionView
	require.Eventually(t, func() bool {
		v = f.getDeletion(t)
		return v.Counts.Stopped == 1 && v.Counts.Failed == 1
	}, 15*time.Second, 20*time.Millisecond, "one agent stopped, one failed")
	assert.Equal(t, store.UserDeletionStateStopping, v.State)
	for _, a := range v.Agents {
		if a.ID == stuck.ID {
			assert.Equal(t, store.UserDeletionOutcomeFailed, a.Outcome)
			assert.Contains(t, a.LastError, "broker unreachable")
		} else {
			assert.Equal(t, ok.ID, a.ID)
			assert.Equal(t, store.UserDeletionOutcomeStopped, a.Outcome)
		}
	}
	// Reconciling again does not complete it, and the deletion names the
	// failed agent and its error.
	f.srv.reconcileUserDeletion(context.Background(), v.DeletionID, false)
	v = f.getDeletion(t)
	assert.Equal(t, store.UserDeletionStateStopping, v.State)
	assert.Contains(t, v.LastError, stuck.ID)
	assert.Contains(t, v.LastError, "broker unreachable")

	// The operator removes the stuck agent with force.
	rec = doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+stuck.ID+"?force=true", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	done := f.waitCompleted(t)
	assert.Equal(t, userDeletionCounts{Stopped: 1, Removed: 1}, done.Counts)
	completion := auditsOf(t, f.s, "user_delete", f.dave.ID)
	require.Len(t, completion, 1)
	var after struct {
		Stopped       int      `json:"stopped"`
		Removed       int      `json:"removed"`
		RemovedAgents []string `json:"removedAgents"`
	}
	require.NoError(t, json.Unmarshal([]byte(completion[0].AfterSummary), &after))
	assert.Equal(t, 1, after.Stopped)
	assert.Equal(t, 1, after.Removed)
	assert.Equal(t, []string{stuck.ID}, after.RemovedAgents)
}

// A deletion left stopping by one hub process is completed by a fresh one
// over the same database.
func TestForceDeleteUser_ResumesAfterRestart(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	f.agent(t, "rs-a", nil)
	f.disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker restarting") })
	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Eventually(t, func() bool { return f.getDeletion(t).Counts.Failed == 1 }, 15*time.Second, 20*time.Millisecond)

	old := userDeletionRetryBase
	userDeletionRetryBase = 0
	t.Cleanup(func() { userDeletionRetryBase = old })
	srv2, _ := testServerOnMigratedStore(t, f.s)
	srv2.SetDispatcher(&engineStubDispatcher{})
	srv2.config.SoftDeleteRetention = time.Hour
	enableUserDeleteForce(t, srv2)
	srv2.userDeletionReconcileHandler()(context.Background())
	f.srv = srv2
	done := f.waitCompleted(t)
	assert.Equal(t, userDeletionCounts{Stopped: 1}, done.Counts)
}

// An agent that blocks again at completion (written without the user
// lock) is recorded and stopped before the deletion completes, and an
// edge from the user left active is deactivated by the completion.
func TestForceDeleteUser_CompletionRechecksBlockers(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	v, _, release := startHeldDeletion(t, f)

	// Written behind the guard's back, as a path that does not take the
	// user lock would.
	late := f.agent(t, "late", nil)
	edge := f.agentEdge(t, store.DelegationPrincipalUser, f.dave.ID, late)
	close(release)

	done := f.waitCompleted(t)
	assert.Equal(t, v.DeletionID, done.DeletionID)
	assert.Equal(t, userDeletionCounts{Stopped: 2}, done.Counts, "the late agent was listed and stopped")
	assert.False(t, mustGetAgent(t, f.s, late.ID).DeletedAt.IsZero())
	e, err := f.s.GetDelegationEdge(ctx, edge.ID)
	require.NoError(t, err)
	assert.False(t, e.Active)
	left, err := f.s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, f.dave.ID)
	require.NoError(t, err)
	assert.Empty(t, left, "no active edge has the deleted user as the delegator")
}

// stoppedDeletionReadyToComplete records, without the request or the
// engine, a stopping deletion of dave whose one agent is soft-deleted and
// recorded stopped, so only the completion is left.
func stoppedDeletionReadyToComplete(t *testing.T, f *forceDelFixture) *store.UserDeletion {
	t.Helper()
	ctx := context.Background()
	a := softDeletedAgent(t, f.s, "ready-"+f.dave.ID[:8], f.project.ID, f.dave.ID, []string{f.dave.ID})
	require.NoError(t, f.s.MarkUserDeleting(ctx, f.dave.ID))
	d := &store.UserDeletion{UserID: f.dave.ID, RequestedByKind: "user", RequestedByID: DevUserID,
		CredentialKind: "session", RequestedAt: time.Now()}
	require.NoError(t, f.s.CreateUserDeletion(ctx, d))
	require.NoError(t, f.s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
		DeletionID: d.ID, AgentID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID,
		Outcome: store.UserDeletionOutcomeStopped, Attempts: 1,
	}))
	return d
}

// Two completions of one deletion: exactly one takes effect, with one
// audit record.
func TestForceDeleteUser_SingleCompletion(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.srv.completeUserDeletion(ctx, d)
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.True(t, results[0] != results[1], "exactly one completion takes effect: %v", results)
	assert.Len(t, auditsOf(t, f.s, "user_delete", f.dave.ID), 1, "one completion audit")
	_, err := f.s.GetUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// GET /api/v1/users/{id}/deletion needs user.delete; 404 without a
// deletion; it answers after the user row is gone.
func TestUserDeletion_RequiresUserDelete(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	enableUserDeleteForce(t, srv)
	target := newActiveMember(t, s, "user-target", "target@test.com")
	reader := bulkInviteOnlyCaller(t, s, "reader", "user.read")
	deleter := bulkInviteOnlyCaller(t, s, "deleter", "user.delete")
	path := "/api/v1/users/" + target.ID + "/deletion"

	rec := doRequestAsUser(t, srv, deleter, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	d := &store.UserDeletion{UserID: target.ID, RequestedByKind: "user", RequestedByID: deleter.ID,
		CredentialKind: "session", RequestedAt: time.Now()}
	require.NoError(t, s.CreateUserDeletion(context.Background(), d))
	rec = doRequestAsUser(t, srv, reader, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	require.NoError(t, s.DeleteUser(context.Background(), target.ID))
	rec = doRequestAsUser(t, srv, deleter, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	v := decodeDeletion(t, rec)
	assert.Equal(t, d.ID, v.DeletionID)
	assert.Equal(t, []userDeletionAgentView{}, v.Agents)

	rec = doRequestAsUser(t, srv, deleter, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	rec = doRequestAsUser(t, srv, deleter, http.MethodGet, path+"/extra", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// userDeletionRetryDue doubles the wait per attempt, up to the cap.
func TestUserDeletionRetryDue(t *testing.T) {
	ts := time.Now()
	rec := func(attempts int, ago time.Duration) *store.UserDeletionAgent {
		return &store.UserDeletionAgent{Attempts: attempts, UpdatedAt: ts.Add(-ago)}
	}
	assert.True(t, userDeletionRetryDue(rec(0, 0), ts))
	assert.False(t, userDeletionRetryDue(rec(1, userDeletionRetryBase-time.Second), ts))
	assert.True(t, userDeletionRetryDue(rec(1, userDeletionRetryBase), ts))
	assert.False(t, userDeletionRetryDue(rec(2, userDeletionRetryBase), ts))
	assert.True(t, userDeletionRetryDue(rec(2, 2*userDeletionRetryBase), ts))
	assert.True(t, userDeletionRetryDue(rec(40, userDeletionRetryMax), ts))
	assert.False(t, userDeletionRetryDue(rec(40, userDeletionRetryMax-time.Second), ts))
}

// One blocking agent the caller may not delete, among several it may:
// 403 user_delete_force_forbidden naming only that agent, and nothing is
// written.
func TestForceDeleteUser_OneForbiddenAgentRefusesAll(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	enableUserDeleteForce(t, srv)
	ctx := context.Background()
	target := newActiveMember(t, s, "user-target", "target@test.com")
	allowedProject := pvSeedProject(t, s, "force-allowed-project")
	deniedProject := pvSeedProject(t, s, "force-denied-project")
	createOwnedAgent(t, s, "target-ok-1", allowedProject, target.ID)
	okAgent := createOwnedAgent(t, s, "target-ok-2", allowedProject, target.ID)
	denied := createOwnedAgent(t, s, "target-denied", deniedProject, target.ID)
	edge := createUserEdge(t, s, target.ID, store.DelegationPrincipalAgent, okAgent.ID, allowedProject)
	tokenID := tid("uat-one-forbidden")
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID: tokenID, UserID: target.ID, Name: "of", Prefix: "pfx-of", KeyHash: "hash-" + tokenID,
		BoundaryKind: "hub", Scopes: []string{"agent:read"},
	}))
	actor := bulkInviteOnlyCaller(t, s, "deleter", "user.delete")
	rd := createTestRoleDefinition(t, s, "force-agent-deleter", store.RoleScopeProject,
		[]string{"project.read", "agent.read", "agent.list", "agent.delete"})
	pvSeedRoleBinding(t, s, rd.ID, "user", actor.ID, store.RoleScopeProject, allowedProject)

	rec := doRequestAsUser(t, srv, actor, http.MethodDelete, "/api/v1/users/"+target.ID+"?force=true", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := errorBody(t, rec)
	assert.Equal(t, ErrCodeUserDeleteForceForbidden, code)
	agents, _ := details["agents"].([]interface{})
	require.Len(t, agents, 1, "only the agent the caller may not delete is named: %v", agents)
	assert.Equal(t, denied.ID, agents[0].(map[string]interface{})["id"])

	u, err := s.GetUser(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	_, err = s.GetUserDeletionByUser(ctx, target.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	e, err := s.GetDelegationEdge(ctx, edge.ID)
	require.NoError(t, err)
	assert.True(t, e.Active)
	n, err := s.CountUserAccessTokens(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the access token is not revoked")
	assert.Empty(t, auditsOf(t, s, "user_delete_requested", target.ID))
}

// A finalize that fails and rolls back (a hook of the finalize
// transaction refuses) leaves the agent failed with the engine's error;
// it is not claimed again before its backoff, and the user stays
// deleting.
func TestForceDeleteUser_FinalizeFailureIsFailedAndBacksOff(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	a := f.agent(t, "ff-a", nil)
	var finalizes atomic.Int32
	var refuse atomic.Bool
	refuse.Store(true)
	old := agentDeletionFinalizeSeam
	t.Cleanup(func() { agentDeletionFinalizeSeam = old })
	agentDeletionFinalizeSeam = func(_ context.Context, _ store.Store, got *store.Agent, _ store.DeletionFinalizeMode) error {
		if got.ID == a.ID && refuse.Load() {
			finalizes.Add(1)
			return errors.New("audit write refused")
		}
		return nil
	}

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var v userDeletionView
	require.Eventually(t, func() bool {
		v = f.getDeletion(t)
		return v.Counts.Failed == 1
	}, 15*time.Second, 20*time.Millisecond, "the agent reads failed")
	require.Len(t, v.Agents, 1)
	assert.Equal(t, store.UserDeletionOutcomeFailed, v.Agents[0].Outcome)
	assert.Contains(t, v.Agents[0].LastError, "audit write refused")
	assert.Equal(t, 1, v.Agents[0].Attempts)
	assert.Equal(t, store.DeletionStateFinalizing, mustGetAgent(t, f.s, a.ID).DeletionState)

	// Before the backoff passes, reconciles claim nothing: the finalize
	// does not run again.
	seen := finalizes.Load()
	require.Positive(t, seen)
	for i := 0; i < 3; i++ {
		f.srv.reconcileUserDeletion(ctx, v.DeletionID, false)
	}
	assert.Equal(t, seen, finalizes.Load(), "no claim before the backoff")
	v = f.getDeletion(t)
	assert.Equal(t, 1, v.Agents[0].Attempts)
	assert.Equal(t, store.UserDeletionStateStopping, v.State)
	assert.Contains(t, v.LastError, "audit write refused")
	u, err := f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusDeleting, u.Status)

	// Once the hook accepts, a retry finalizes the agent and the deletion
	// completes with it stopped.
	refuse.Store(false)
	f.srv.reconcileUserDeletion(ctx, v.DeletionID, true)
	done := f.waitCompleted(t)
	assert.Equal(t, userDeletionCounts{Stopped: 1}, done.Counts)
}

// An agent whose Runtime Broker is not connected is failed with no delete
// attempted, and the deletion names it in its last error.
func TestForceDeleteUser_BrokerUnavailableIsFailed(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	offline := &store.RuntimeBroker{ID: tid("broker-force-del-offline"), Name: "Offline Broker", Slug: "force-del-offline",
		Status: store.BrokerStatusOffline, Endpoint: "http://localhost:9801"}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, offline))
	a := &store.Agent{ID: tid("agent-bu-a"), Slug: "bu-a", Name: "bu-a", ProjectID: f.project.ID,
		RuntimeBrokerID: offline.ID, Phase: string(state.PhaseRunning),
		OwnerID: f.dave.ID, CreatedBy: f.dave.ID, Ancestry: []string{f.dave.ID}}
	require.NoError(t, f.s.CreateAgent(ctx, a))

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var v userDeletionView
	require.Eventually(t, func() bool {
		v = f.getDeletion(t)
		return v.Counts.Failed == 1 && v.LastError != ""
	}, 15*time.Second, 20*time.Millisecond, "the agent reads failed and the deletion names it")
	assert.Equal(t, userDeletionBrokerUnavailable, v.Agents[0].LastError)
	assert.Contains(t, v.LastError, a.ID)
	assert.Contains(t, v.LastError, userDeletionBrokerUnavailable)
	assert.Equal(t, store.DeletionStateNone, mustGetAgent(t, f.s, a.ID).DeletionState, "no delete was claimed")
}

// A reconcile pass holding an older copy of an agent's record cannot
// replace the stopped outcome a finalize recorded since it read it: the
// agent is reported stopped, not removed.
func TestForceDeleteUser_StaleWriteKeepsStopped(t *testing.T) {
	f := newForceDelFixture(t, 0)
	ctx := context.Background()
	a := f.agent(t, "stale-a", nil)
	require.NoError(t, f.s.MarkUserDeleting(ctx, f.dave.ID))
	d := &store.UserDeletion{UserID: f.dave.ID, RequestedByKind: "user", RequestedByID: DevUserID,
		CredentialKind: "session", RequestedAt: time.Now()}
	require.NoError(t, f.s.CreateUserDeletion(ctx, d))
	stale := store.UserDeletionAgent{DeletionID: d.ID, AgentID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID,
		Outcome: store.UserDeletionOutcomePending}
	require.NoError(t, f.s.UpsertUserDeletionAgent(ctx, &stale))

	// Another delete holds the agent, and its finalize records the agent
	// stopped after the pass read the record.
	deleting := store.DeletionStateDeleting
	lease := time.Now().Add(time.Hour)
	n, err := f.s.UpdateAgentDeletion(ctx, a.ID,
		store.DeletionPredicate{States: []string{store.DeletionStateNone}, DeletedAtNull: true},
		store.DeletionFields{State: &deleting, LeaseAt: &lease})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	confirmed := stale
	confirmed.Outcome = store.UserDeletionOutcomeStopped
	require.NoError(t, f.s.UpsertUserDeletionAgent(ctx, &confirmed))

	// The pass refreshes from its older copy: it sees the delete in flight
	// and writes stopping, which leaves the stopped record as it is.
	row, err := f.srv.refreshUserDeletionAgent(ctx, d, &stale, time.Now())
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, store.UserDeletionOutcomeStopping, stale.Outcome, "the pass decided from its older copy")
	recs, err := f.s.ListUserDeletionAgents(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, store.UserDeletionOutcomeStopped, recs[0].Outcome)

	// The finalize removed the row (no retention): the deletion completes
	// with the agent stopped.
	require.NoError(t, f.s.DeleteAgent(ctx, a.ID))
	f.srv.reconcileUserDeletion(ctx, d.ID, false)
	done := f.waitCompleted(t)
	assert.Equal(t, userDeletionCounts{Stopped: 1}, done.Counts)
}

// An agent the scheduler started for one of the user's schedules (no
// owner or ancestry; the user only as created_by) blocks the user, and a
// force request stops it.
func TestForceDeleteUser_ScheduleStartedAgentIsStopped(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	a := &store.Agent{ID: tid("agent-sched"), Slug: "sched", Name: "sched", ProjectID: f.project.ID,
		RuntimeBrokerID: f.broker.ID, Phase: string(state.PhaseRunning), CreatedBy: f.dave.ID}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	v := decodeDeletion(t, rec)
	require.Len(t, v.Agents, 1)
	assert.Equal(t, a.ID, v.Agents[0].ID)
	done := f.waitCompleted(t)
	assert.Equal(t, userDeletionCounts{Stopped: 1}, done.Counts)
	assert.False(t, mustGetAgent(t, f.s, a.ID).DeletedAt.IsZero())
}

// The last super-admin is refused under force too, before any write.
func TestForceDeleteUser_LastSuperAdminRefused(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	a := f.agent(t, "lsa-a", nil)
	f.srv.ensureSuperAdminBinding(ctx, f.dave.ID)
	// The requester keeps the permissions a forced delete needs through a
	// role other than super-admin, so the last super-admin guard decides.
	deleter := createTestRoleDefinition(t, f.s, "lsa-deleter", store.RoleScopeSystem,
		[]string{"user.read", "user.delete", "project.read", "agent.read", "agent.list", "agent.delete"})
	pvSeedRoleBinding(t, f.s, deleter.ID, "user", DevUserID, store.RoleScopeSystem, "")
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := f.s.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
	require.NoError(t, err)
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID && b.PrincipalID != f.dave.ID {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	rec := f.forceDelete(t)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "last super-admin")
	u, err := f.s.GetUser(ctx, f.dave.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, u.Status)
	_, err = f.s.GetUserDeletionByUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.True(t, mustGetAgent(t, f.s, a.ID).DeletedAt.IsZero())
}

// With the experiment off, a deletion accepted while it was on runs to
// completion on the recurring reconcile.
func TestForceDeleteUser_InFlightCompletesWithExperimentOff(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)
	setUserDeleteForce(t, f.srv, false)

	f.srv.userDeletionReconcileHandler()(ctx)
	got, err := f.s.GetUserDeletion(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserDeletionStateCompleted, got.State)
	_, err = f.s.GetUser(ctx, f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// The completion reopens the record of an agent recorded stopped that
// blocks the user again; the deletion stays stopping until it is stopped.
func TestForceDeleteUser_CompletionReopensStoppedRecord(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)
	// A live agent of dave's whose record reads stopped (written behind
	// the guard's back).
	live := f.agent(t, "reopen-live", nil)
	require.NoError(t, f.s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
		DeletionID: d.ID, AgentID: live.ID, Slug: live.Slug, ProjectID: live.ProjectID,
		Outcome: store.UserDeletionOutcomeStopped, Attempts: 1,
	}))

	completed, err := f.srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	assert.False(t, completed)
	recs, err := f.s.ListUserDeletionAgents(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	for _, r := range recs {
		if r.AgentID == live.ID {
			assert.Equal(t, store.UserDeletionOutcomePending, r.Outcome, "the record is reopened")
		} else {
			assert.Equal(t, store.UserDeletionOutcomeStopped, r.Outcome)
		}
	}
	got, err := f.s.GetUserDeletion(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserDeletionStateStopping, got.State)
}

// The admin_emails reconcile neither promotes nor binds a user being
// deleted, even when it is listed.
func TestReconcileSuperAdminBindings_SkipsDeletingUser(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()
	id := tid("reconcile-deleting")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: id, Email: "reconcile-deleting@test.com", DisplayName: "Deleting", Role: "member", Status: "active",
	}))
	require.NoError(t, s.MarkUserDeleting(ctx, id))

	_, err := ReconcileSuperAdminBindings(ctx, s, []string{"reconcile-deleting@test.com"}, "")
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
	require.NoError(t, err)
	assert.Empty(t, bindings, "no super-admin binding for a user being deleted")
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusDeleting, u.Status)
	assert.Equal(t, store.UserRoleMember, u.Role)
}

// An active delegation from the user to an agent the cascade does not stop
// (another user's agent), left active at completion, ends with the user:
// the completion deactivates it with cause delegator_deleted.
func TestForceDeleteUser_CompletionSweepsDelegatorEdges(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)
	other := createOwnedAgent(t, f.s, "sweep-other", f.project.ID, DevUserID)
	edge := f.agentEdge(t, store.DelegationPrincipalUser, f.dave.ID, other)

	completed, err := f.srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	require.True(t, completed)
	requireEdgeCause(t, f.s, edge.ID, store.EdgeDeactivationDelegatorDeleted, d.ID)
	assert.True(t, mustGetAgent(t, f.s, other.ID).DeletedAt.IsZero(), "the other user's agent is not stopped")
}

// A finalize that commits between the completion's re-listing of the
// blockers and its lock on their agent rows wins: the completion re-reads
// the agent under the lock, finds it gone, and keeps the stopped record
// instead of reopening it. The next completion counts it stopped.
func TestForceDeleteUser_CompletionKeepsStoppedOfFinalizeCommittedFirst(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)
	live := f.agent(t, "finalized-first", nil)
	require.NoError(t, f.s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
		DeletionID: d.ID, AgentID: live.ID, Slug: live.Slug, ProjectID: live.ProjectID,
		Outcome: store.UserDeletionOutcomeStopped, Attempts: 1,
	}))
	var seen []string
	completeUserDeletionBeforeAgentLock = func(ctx context.Context, tx store.Store, agents []store.Agent) {
		for _, a := range agents {
			seen = append(seen, a.ID)
		}
		// The finalize of a non-force delete with no retention: the row
		// is removed; its record already reads stopped.
		require.NoError(t, tx.DeleteAgent(ctx, live.ID))
	}
	t.Cleanup(func() { completeUserDeletionBeforeAgentLock = nil })

	completed, err := f.srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	assert.False(t, completed, "the pass that saw a blocker does not complete")
	assert.Equal(t, []string{live.ID}, seen, "the agent was listed as a blocker before the lock")
	recs, err := f.s.ListUserDeletionAgents(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	for _, r := range recs {
		assert.Equal(t, store.UserDeletionOutcomeStopped, r.Outcome, "agent %s keeps stopped", r.AgentID)
	}

	completeUserDeletionBeforeAgentLock = nil
	completed, err = f.srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	require.True(t, completed)
	assert.Equal(t, userDeletionCounts{Stopped: 2}, f.getDeletion(t).Counts)
}

// A finalize with retention soft-deletes the agent. One that commits
// between the completion's re-listing and its lock is seen only by the
// re-read under the lock, which skips the soft-deleted row: its stopped
// record is not reopened.
func TestForceDeleteUser_CompletionSkipsSoftDeletedBlocker(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	d := stoppedDeletionReadyToComplete(t, f)
	live := f.agent(t, "soft-deleted-first", nil)
	require.NoError(t, f.s.UpsertUserDeletionAgent(ctx, &store.UserDeletionAgent{
		DeletionID: d.ID, AgentID: live.ID, Slug: live.Slug, ProjectID: live.ProjectID,
		Outcome: store.UserDeletionOutcomeStopped, Attempts: 1,
	}))
	completeUserDeletionBeforeAgentLock = func(ctx context.Context, tx store.Store, agents []store.Agent) {
		require.Len(t, agents, 1)
		// The row as a soft-deleting finalize leaves it: present, with
		// DeletedAt set. The store has no write of DeletedAt outside the
		// finalize, so the row is replaced by its soft-deleted copy.
		gone := agents[0]
		gone.DeletedAt = time.Now()
		require.NoError(t, tx.DeleteAgent(ctx, gone.ID))
		require.NoError(t, tx.CreateAgent(ctx, &gone))
	}
	t.Cleanup(func() { completeUserDeletionBeforeAgentLock = nil })

	completed, err := f.srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	assert.False(t, completed)
	require.False(t, mustGetAgent(t, f.s, live.ID).DeletedAt.IsZero(), "the row is soft-deleted")
	recs, err := f.s.ListUserDeletionAgents(ctx, d.ID)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	for _, r := range recs {
		assert.Equal(t, store.UserDeletionOutcomeStopped, r.Outcome, "agent %s keeps stopped", r.AgentID)
	}

	completeUserDeletionBeforeAgentLock = nil
	completed, err = f.srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	require.True(t, completed)
	assert.Equal(t, userDeletionCounts{Stopped: 2}, f.getDeletion(t).Counts)
}
