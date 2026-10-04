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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---

// assertRestoreWroteNothing asserts a refused restore left the soft-deleted
// row, its operation ID and state_version, the edge's deactivation record
// and the audit log as they were.
func assertRestoreWroteNothing(t *testing.T, s store.Store, before *store.Agent, edge *store.DelegationEdge) {
	t.Helper()
	got := mustGetAgent(t, s, before.ID)
	assert.False(t, got.DeletedAt.IsZero(), "the row stays soft-deleted")
	assert.Equal(t, before.SoftDeleteOpID, got.SoftDeleteOpID, "the operation ID is kept")
	assert.Equal(t, before.StateVersion, got.StateVersion, "the row is not written")
	assert.Empty(t, activeEdgeIDs(t, s, before.ID), "no edge is reactivated")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentRestore, before.ID), "no restore audit")
	if edge != nil {
		edges, err := s.GetDeactivatedDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, before.ID,
			store.EdgeDeactivationAgentSoftDelete, before.SoftDeleteOpID)
		require.NoError(t, err)
		require.Len(t, edges, 1, "the deactivation record is intact")
		assert.Equal(t, edge.ID, edges[0].ID)
	}
}

// softDeletedWithEdge returns a soft-deleted agent whose single edge, from
// a delegator of delegatorType, was deactivated by the soft delete.
func softDeletedWithEdge(t *testing.T, srv *Server, s store.Store, name, delegatorType, delegatorID string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, name, state.PhaseStopped)
	var edge *store.DelegationEdge
	if delegatorType == store.DelegationPrincipalUser {
		edge = seedAgentEdge(t, s, delegatorID, agent)
	} else {
		edge = &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalAgent,
			DelegatorID:   delegatorID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    agent.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       agent.ProjectID,
			Role:          string(AgentRoleBaseline),
			Active:        true,
			AuthorityProvenance: store.AuthorityProvenance{
				ProvenanceVersion:    store.ProvenanceVersionV1,
				SourcePrincipalKind:  store.DelegationPrincipalAgent,
				SourcePrincipalID:    delegatorID,
				SourceCredentialKind: store.SourceCredentialAgent,
			},
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}
		require.NoError(t, s.CreateDelegationEdge(context.Background(), edge))
	}
	softDeleteForTest(t, srv, agent.ID)
	got := mustGetAgent(t, s, agent.ID)
	require.NotEmpty(t, got.SoftDeleteOpID)
	return got, edge
}

// lifecycleFaultStore injects store errors into the reads the restore's
// delegator check makes, including inside transactions.
type lifecycleFaultStore struct {
	store.Store
	failUser  bool
	failEdges bool
}

var errInjectedLookup = errors.New("injected lookup fault")

func (f *lifecycleFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&lifecycleFaultStore{Store: tx, failUser: f.failUser, failEdges: f.failEdges})
	})
}

func (f *lifecycleFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if f.failUser {
		return nil, errInjectedLookup
	}
	return f.Store.GetUser(ctx, id)
}

func (f *lifecycleFaultStore) GetDeactivatedDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, cause store.EdgeDeactivationCause, opID string) ([]*store.DelegationEdge, error) {
	if f.failEdges {
		return nil, errInjectedLookup
	}
	return f.Store.GetDeactivatedDelegationEdgesForDelegate(ctx, delegateType, delegateID, cause, opID)
}

// --- soft delete during the create dispatch ---

// softDeletingDispatcher soft-deletes the agent row, through the same
// lifecycle transaction the delete engine's finalize runs, while the create
// dispatch is in flight.
type softDeletingDispatcher struct {
	*createAgentDispatcher
	srv *Server
	err error
}

func (d *softDeletingDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	res, err := d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, agent)
	d.err = d.srv.store.WithTx(context.Background(), func(tx store.Store) error {
		row, err := tx.GetAgent(context.Background(), agent.ID)
		if err != nil {
			return err
		}
		row.Phase = string(state.PhaseStopped)
		row.DeletedAt = time.Now()
		if err := tx.UpdateAgent(context.Background(), row); err != nil {
			return err
		}
		return d.srv.softDeleteAgentTx(context.Background(), tx, row, AuditActor{})
	})
	return res, err
}

// A soft delete that finishes between the create dispatch and the
// post-dispatch write keeps the row soft-deleted with its operation ID, and
// a restore then reactivates the edge the soft delete deactivated.
func TestPostDispatchWriteKeepsSoftDeletedRow(t *testing.T) {
	disp := &softDeletingDispatcher{createAgentDispatcher: &createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.srv = srv
	srv.config.SoftDeleteRetention = time.Hour

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "soft-race", ProjectID: project.ID, Task: "t"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	require.NoError(t, disp.err, "the soft delete during dispatch")
	require.NotNil(t, disp.capturedAgent)
	id := disp.capturedAgent.ID

	got := mustGetAgent(t, s, id)
	assert.False(t, got.DeletedAt.IsZero(), "the row stays soft-deleted")
	require.NotEmpty(t, got.SoftDeleteOpID, "the operation ID is kept")
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	sum := auditSummary(t, s, mutationTypeAgentSoftDelete, id)
	assert.Equal(t, got.SoftDeleteOpID, sum["op_id"])
	edges, err := s.GetDeactivatedDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, id,
		store.EdgeDeactivationAgentSoftDelete, got.SoftDeleteOpID)
	require.NoError(t, err)
	require.Len(t, edges, 1, "the create's edge is deactivated under the operation ID")
	ensureActiveUser(t, s, edges[0].DelegatorID)

	rec = restoreForTest(t, srv, id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{edges[0].ID}, activeEdgeIDs(t, s, id), "the restore reactivates the edge")
	assert.Empty(t, mustGetAgent(t, s, id).SoftDeleteOpID)
}

// --- restore guards ---

// A restore of a row read before a concurrent write fails the
// state_version guard, runs no restore hook and writes nothing.
func TestRestoreStaleVersionWritesNothing(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent, edge := softDeletedWithEdge(t, srv, s, "restore-stale", store.DelegationPrincipalUser, tid("stale-delegator"))
	stale := *agent

	fresh := mustGetAgent(t, s, agent.ID)
	fresh.Message = "concurrent write"
	require.NoError(t, s.UpdateAgent(context.Background(), fresh))
	before := mustGetAgent(t, s, agent.ID)
	hookCalled := false
	srv.RegisterRestoreHook("record", func(context.Context, store.Store, *store.Agent, AuditActor) error {
		hookCalled = true
		return nil
	})

	err := srv.restoreAgentTx(context.Background(), &stale, AuditActor{})
	require.ErrorIs(t, err, store.ErrVersionConflict)
	assert.False(t, hookCalled, "the restore hook does not run")
	assertRestoreWroteNothing(t, s, before, edge)
}

// claimOnLoadStore claims a delete on the agent right after the restore
// handler loads it, so the claim lands between the load and the restore
// transaction.
type claimOnLoadStore struct {
	store.Store
	once  sync.Once
	claim func()
}

func (c *claimOnLoadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := c.Store.GetAgent(ctx, id)
	c.once.Do(c.claim)
	return a, err
}

// A restore whose row is claimed by a delete after the handler loaded it
// returns 409 and writes nothing.
func TestRestoreAfterDeleteClaimConflicts(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent, edge := softDeletedWithEdge(t, srv, s, "restore-claimed", store.DelegationPrincipalUser, tid("claimed-delegator"))
	srv.store = &claimOnLoadStore{Store: s, claim: func() { seedAgentDeletion(t, s, agent.ID, seedLiveDeleting) }}

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "delegated to this agent is not active", "the 409 comes from the delete claim")

	srv.store = s
	before := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, store.DeletionStateDeleting, before.DeletionState, "the delete claim stands")
	assert.Equal(t, agent.SoftDeleteOpID, before.SoftDeleteOpID)
	assertRestoreWroteNothing(t, s, before, edge)
}

// A restore whose row has its soft-delete operation ID changed after the
// handler loaded it uses the operation ID read inside the transaction:
// SetAgentSoftDeleteOpID does not bump state_version, so the restore
// proceeds, reactivates the edges deactivated under the new ID and records
// that ID in its audit.
func TestRestoreUsesOpIDReadInTx(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent, edge := softDeletedWithEdge(t, srv, s, "restore-opid-moved", store.DelegationPrincipalUser, tid("moved-delegator"))
	const movedOpID = "op-moved"
	srv.store = &claimOnLoadStore{Store: s, claim: func() {
		ctx := context.Background()
		n, err := s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID, store.EdgeDeactivationAgentSoftDelete, agent.SoftDeleteOpID)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		now := time.Now()
		n, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID, store.Deactivation{
			Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: movedOpID,
		})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.NoError(t, s.SetAgentSoftDeleteOpID(ctx, agent.ID, movedOpID))
	}}

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	srv.store = s
	got := mustGetAgent(t, s, agent.ID)
	assert.True(t, got.DeletedAt.IsZero(), "the row is restored")
	assert.Empty(t, got.SoftDeleteOpID)
	assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, s, agent.ID), "the edge deactivated under the new operation ID is reactivated")
	sum := auditSummary(t, s, mutationTypeAgentRestore, agent.ID)
	assert.Equal(t, movedOpID, sum["op_id"])
	assert.EqualValues(t, 1, sum["edges_reactivated"])
}

// A restore whose deactivated edges have a delegator that is not live
// returns 409 and writes nothing; the deactivation record stays intact, so
// the restore succeeds once the delegator is live again.
func TestRestoreRefusesNotLiveDelegator(t *testing.T) {
	cases := []struct {
		name          string
		delegatorType string
		// kill makes the delegator not live; revive makes it live again
		// (nil when it cannot be revived).
		kill   func(t *testing.T, s store.Store, id string)
		revive func(t *testing.T, s store.Store, id string)
	}{
		{
			name: "suspended user", delegatorType: store.DelegationPrincipalUser,
			kill:   func(t *testing.T, s store.Store, id string) { setUserStatus(t, s, id, store.UserStatusSuspended) },
			revive: func(t *testing.T, s store.Store, id string) { setUserStatus(t, s, id, store.UserStatusActive) },
		},
		{
			name: "missing user", delegatorType: store.DelegationPrincipalUser,
			kill: func(t *testing.T, s store.Store, id string) {
				require.NoError(t, s.DeleteUser(context.Background(), id))
			},
			revive: func(t *testing.T, s store.Store, id string) { ensureActiveUser(t, s, id) },
		},
		{
			name: "soft-deleted agent", delegatorType: store.DelegationPrincipalAgent,
			kill: func(t *testing.T, s store.Store, id string) {
				a := mustGetAgent(t, s, id)
				a.DeletedAt = time.Now()
				require.NoError(t, s.UpdateAgent(context.Background(), a))
			},
			revive: func(t *testing.T, s store.Store, id string) {
				a := mustGetAgent(t, s, id)
				a.DeletedAt = time.Time{}
				require.NoError(t, s.UpdateAgent(context.Background(), a))
			},
		},
		{
			name: "missing agent", delegatorType: store.DelegationPrincipalAgent,
			kill: func(t *testing.T, s store.Store, id string) {
				require.NoError(t, s.DeleteAgent(context.Background(), id))
			},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			suffix := string(rune('a' + i))
			delegatorID := tid("nl-delegator-" + suffix)
			if tc.delegatorType == store.DelegationPrincipalAgent {
				delegatorID = setupBrokerAgentInPhase(t, s, "nl-parent-"+suffix, state.PhaseRunning).ID
			}
			agent, edge := softDeletedWithEdge(t, srv, s, "nl-child-"+suffix, tc.delegatorType, delegatorID)
			tc.kill(t, s, delegatorID)

			rec := restoreForTest(t, srv, agent.ID)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "delegated to this agent is not active")
			assertRestoreWroteNothing(t, s, agent, edge)

			if tc.revive == nil {
				return
			}
			tc.revive(t, s, delegatorID)
			rec = restoreForTest(t, srv, agent.ID)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, s, agent.ID))
		})
	}
}

// A store error during the restore's delegator check returns 503 and writes
// nothing: a fault reading the deactivated edges, and one reading a
// delegator.
func TestRestoreDelegatorLookupFault503(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault lifecycleFaultStore
	}{
		{"edge read", lifecycleFaultStore{failEdges: true}},
		{"delegator read", lifecycleFaultStore{failUser: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			agent, edge := softDeletedWithEdge(t, srv, s, "lookup-fault-"+tidSlugSafe(tc.name), store.DelegationPrincipalUser, tid("fault-delegator-"+tc.name))
			fault := tc.fault
			fault.Store = s
			srv.store = &fault

			rec := restoreForTest(t, srv, agent.ID)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

			srv.store = s
			assertRestoreWroteNothing(t, s, agent, edge)
		})
	}
}

// An ErrAlreadyExists from a restore write other than the edge reactivation
// is not reported as a delegation conflict.
func TestRestoreOnlyEdgeConflictIsDelegationConflict(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent, edge := softDeletedWithEdge(t, srv, s, "restore-other-exists", store.DelegationPrincipalUser, tid("exists-delegator"))
	srv.RegisterRestoreHook("exists", func(context.Context, store.Store, *store.Agent, AuditActor) error {
		return store.ErrAlreadyExists
	})

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeConflict, code, "the generic already-exists error")
	assert.Contains(t, rec.Body.String(), "Resource already exists")
	assert.NotContains(t, rec.Body.String(), "active delegation that conflicts")
	assertRestoreWroteNothing(t, s, agent, edge)
}

// --- reincarnation authority ordering ---

// The authority check runs right after the start gate: a requester that
// fails CanDelegate gets 403, not the 409 of a pending reincarnation or the
// 412 of a broker without reprovision, with nothing claimed, on a dry run
// and a real run.
func TestReincarnateAuthorityCheckedBeforeStateAndBroker(t *testing.T) {
	t.Run("pending reincarnation", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		agent.ReincarnationState = store.ReincarnationStatePending
		require.NoError(t, s.UpdateAgent(context.Background(), agent))
		agent = mustGetAgent(t, s, agent.ID)
		require.Equal(t, store.ReincarnationStatePending, agent.ReincarnationState)
		edge := seedAgentEdge(t, s, tid("delegator"), agent)
		requester := reincarnateRequesterWithoutDelegation(t, s, project, broker)

		for _, dryRun := range []bool{true, false} {
			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}), agent.ID)
			require.Equal(t, http.StatusForbidden, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		}
		got := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, agent.StateVersion, got.StateVersion, "nothing is claimed")
		assert.Equal(t, store.ReincarnationStatePending, got.ReincarnationState)
		recs, err := s.ListAgentReincarnations(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Empty(t, recs)
		assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, s, agent.ID))
		assert.Empty(t, agentAudits(t, s, mutationTypeAgentReincarnateClaim, agent.ID))
	})

	t.Run("broker without reprovision", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		broker.Capabilities = &store.BrokerCapabilities{Reprovision: false}
		require.NoError(t, s.UpdateRuntimeBroker(context.Background(), broker))
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		agent = mustGetAgent(t, s, agent.ID)
		edge := seedAgentEdge(t, s, tid("delegator"), agent)
		requester := reincarnateRequesterWithoutDelegation(t, s, project, broker)

		for _, dryRun := range []bool{true, false} {
			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}), agent.ID)
			require.Equal(t, http.StatusForbidden, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		}
		assertNothingClaimed(t, s, agent, edge)
	})
}

// reincarnateRequesterWithoutDelegation returns an agent requester in
// project that holds the lifecycle scope but not the scopes CanDelegate
// requires for a baseline agent.
func reincarnateRequesterWithoutDelegation(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker) AgentIdentity {
	t.Helper()
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
		a.ReincarnationState = store.ReincarnationStateNone
	})
	return agentIdentityFor(coordinator.ID, project.ID, ScopeAgentLifecycle)
}

// --- reincarnation ceiling refusals ---

// uatLookupErrStore fails every user access token read.
type uatLookupErrStore struct {
	store.Store
}

func (s *uatLookupErrStore) GetUserAccessToken(context.Context, string) (*store.UserAccessToken, error) {
	return nil, errInjectedLookup
}

// uatRequester returns a V1 user access token identity for user in
// projectID covering agent creation and lifecycle, with credential ID
// credID.
func uatRequester(t *testing.T, user *store.User, projectID, credID string) Identity {
	t.Helper()
	selectors := append(minimalSelectors(t), "agent:lifecycle")
	c := uatCeilingFromSelectors(t, selectors...)
	return NewScopedUserIdentityWithCeiling(authUser(user), projectID, selectors, credID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: c.PermissionIDs})
}

// A ceiling lookup fault is a 503 with nothing claimed, on a dry run and a
// real run.
func TestReincarnateCeilingLookupFault503(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	agent = mustGetAgent(t, s, agent.ID)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	user := newReincarnateAuthzUser(t, s, "ceiling-fault")
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	grantAgentDelegationAtProject(t, s, user.ID, project.ID)
	requester := uatRequester(t, user, project.ID, "uat-"+tid("ceiling-fault"))
	srv.authzService.store = &uatLookupErrStore{Store: s}

	for _, dryRun := range []bool{true, false} {
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}), agent.ID)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
	}
	assertNothingClaimed(t, s, agent, edge)
}

// --- reincarnation by a user ---

// reincarnateByUser runs a real reincarnation of a baseline agent by a
// member user holding agent.lifecycle and agent.create in the project, as
// identity(user), and returns the re-recorded edge.
func reincarnateByUser(t *testing.T, identity func(user *store.User, projectID string) Identity) (*Server, *store.User, *store.DelegationEdge, Identity) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	old := seedAgentEdge(t, s, tid("delegator"), agent)
	user := newReincarnateAuthzUser(t, s, tidSlugSafe(t.Name()))
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	grantAgentDelegationAtProject(t, s, user.ID, project.ID)
	id := identity(user, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, id, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	require.NotEqual(t, old.ID, edges[0].ID, "the edge is re-recorded")
	return srv, user, edges[0], id
}

// A reincarnation by a session user re-records the edge with the user as
// delegator, session provenance and a principal ceiling.
func TestReincarnateBySessionUserReRecordsUserEdge(t *testing.T) {
	srv, user, e, id := reincarnateByUser(t, func(user *store.User, _ string) Identity {
		return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	})
	assert.Equal(t, store.DelegationPrincipalUser, e.DelegatorType)
	assert.Equal(t, user.ID, e.DelegatorID)
	assert.Equal(t, store.DelegationPrincipalUser, e.SourcePrincipalKind)
	assert.Equal(t, user.ID, e.SourcePrincipalID)
	assert.Equal(t, store.SourceCredentialSession, e.SourceCredentialKind)
	assert.Equal(t, store.EffectCeilingPrincipal, e.Kind)
	assertCeilingFromSource(t, srv, id, e.EffectCeiling)
}

// A reincarnation by a user access token re-records the edge with the user
// as delegator, UAT provenance and the token's bounded V1 ceiling.
func TestReincarnateByUATReRecordsBoundedEdge(t *testing.T) {
	srv, user, e, id := reincarnateByUser(t, func(user *store.User, projectID string) Identity {
		return uatRequester(t, user, projectID, "")
	})
	assert.Equal(t, store.DelegationPrincipalUser, e.DelegatorType)
	assert.Equal(t, user.ID, e.DelegatorID)
	assert.Equal(t, store.DelegationPrincipalUser, e.SourcePrincipalKind)
	assert.Equal(t, user.ID, e.SourcePrincipalID)
	assert.Equal(t, store.SourceCredentialUAT, e.SourceCredentialKind)
	assert.Equal(t, store.EffectCeilingBounded, e.Kind)
	assert.Equal(t, permissions.CeilingVersionV1, e.Version)
	assertCeilingFromSource(t, srv, id, e.EffectCeiling)
}

// assertCeilingFromSource asserts got is the ceiling sourceEffectCeiling
// derives for the requester: kind, version and permission IDs.
func assertCeilingFromSource(t *testing.T, srv *Server, requester Identity, got store.EffectCeiling) {
	t.Helper()
	want, _, err := srv.authzService.sourceEffectCeiling(context.Background(), requester)
	require.NoError(t, err)
	assert.Equal(t, want.Kind, got.Kind, "ceiling kind")
	assert.Equal(t, want.Version, got.Version, "ceiling version")
	assert.Equal(t, want.PermissionIDs, got.PermissionIDs, "ceiling permission IDs")
}

// --- project delete ---

// A project delete deactivates every active edge where an agent of the
// project, soft-deleted or not, is the delegate or the delegator, inside
// the delete transaction, with the hard-delete cause.
func TestProjectDeleteDeactivatesAgentEdges(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("pd-edges")
	ownerID := tid("pd-edges-owner")
	createRS3Project(t, s, projectID, ownerID)

	other := &store.Project{ID: tid("pd-edges-other"), Name: "Other", Slug: "pd-edges-other"}
	require.NoError(t, s.CreateProject(ctx, other))

	mk := func(projID, name string) *store.Agent {
		a := &store.Agent{ID: tid(name), Slug: name, Name: name, ProjectID: projID}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	live := mk(projectID, "pd-live")
	soft := mk(projectID, "pd-soft")
	outside := mk(other.ID, "pd-outside")
	bystander := mk(other.ID, "pd-bystander")

	edge := func(delegatorType, delegatorID string, delegate *store.Agent) *store.DelegationEdge {
		e := &store.DelegationEdge{
			DelegatorType: delegatorType, DelegatorID: delegatorID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: delegate.ID,
			ScopeType: store.RoleScopeProject, ScopeID: delegate.ProjectID,
			Role: string(AgentRoleBaseline), Active: true,
			AuthorityProvenance: store.AuthorityProvenance{
				ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: delegatorType,
				SourcePrincipalID: delegatorID, SourceCredentialKind: store.SourceCredentialSession,
			},
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		}
		require.NoError(t, s.CreateDelegationEdge(ctx, e))
		return e
	}
	edge(store.DelegationPrincipalUser, ownerID, live)              // project agent as delegate
	edge(store.DelegationPrincipalUser, ownerID, soft)              // soft-deleted project agent as delegate
	edge(store.DelegationPrincipalAgent, live.ID, outside)          // project agent as delegator
	kept := edge(store.DelegationPrincipalUser, ownerID, bystander) // unrelated
	soft.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, soft))

	req := ProjectDeleteRequest{
		ProjectID: projectID,
		Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "web"),
	}
	result, decision := srv.deletionService.Delete(setTestIdentity(ctx, req.Actor), req)
	require.Nil(t, decision)
	require.NotNil(t, result)
	assert.Equal(t, 3, result.CascadeSummary.DelegationEdges)

	for _, a := range []*store.Agent{live, soft} {
		assert.Empty(t, activeEdgeIDs(t, s, a.ID), "no active edge delegates to project agent %s", a.Slug)
		delegated, err := s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalAgent, a.ID)
		require.NoError(t, err)
		for _, e := range delegated {
			assert.False(t, e.Active, "no active edge is delegated by project agent %s", a.Slug)
		}
	}
	assert.Empty(t, activeEdgeIDs(t, s, outside.ID), "the edge delegated by the project agent is deactivated")
	assert.Equal(t, []string{kept.ID}, activeEdgeIDs(t, s, bystander.ID), "an unrelated edge stays active")
}

// --- soft-delete operation ID carry-through ---

// reloadGuardedColumns carries the stored soft-delete operation ID into a
// stale in-memory copy.
func TestReloadGuardedColumnsCarriesSoftDeleteOpID(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "reload-opid", state.PhaseStopped)
	stale := mustGetAgent(t, s, agent.ID)
	require.NoError(t, s.SetAgentSoftDeleteOpID(context.Background(), agent.ID, "op-reload"))

	require.NoError(t, srv.reloadGuardedColumns(context.Background(), stale))
	assert.Equal(t, "op-reload", stale.SoftDeleteOpID)
}

// The hard-delete audit of a soft-deleted agent records the soft delete's
// operation ID; a never soft-deleted agent's record omits the key.
func TestHardDeleteAuditRecordsSoftDeleteOpID(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	hardDelete := func(id string) map[string]any {
		row := mustGetAgent(t, s, id)
		require.NoError(t, s.WithTx(context.Background(), func(tx store.Store) error {
			return srv.hardDeleteAgentTx(context.Background(), tx, row, AuditActor{})
		}))
		return auditSummary(t, s, mutationTypeAgentHardDelete, id)
	}

	soft, _ := softDeletedWithEdge(t, srv, s, "hard-after-soft", store.DelegationPrincipalUser, tid("hard-delegator"))
	assert.Equal(t, soft.SoftDeleteOpID, hardDelete(soft.ID)["soft_delete_op_id"])

	live := setupBrokerAgentInPhase(t, s, "hard-live", state.PhaseStopped)
	assert.NotContains(t, hardDelete(live.ID), "soft_delete_op_id")
}
