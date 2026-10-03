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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---

// seedAgentEdge records an active project-scoped edge delegating to agent,
// with recorded session provenance, and returns it.
func seedAgentEdge(t *testing.T, s store.Store, delegatorID string, agent *store.Agent) *store.DelegationEdge {
	t.Helper()
	e := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       agent.ProjectID,
		Role:          string(AgentRoleBaseline),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	return e
}

// activeEdgeIDs returns the IDs of the agent's active delegation edges.
func activeEdgeIDs(t *testing.T, s store.Store, agentID string) []string {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, e.ID)
	}
	return ids
}

// auditSummary decodes the AfterSummary of the single record of
// mutationType for agentID.
func auditSummary(t *testing.T, s store.Store, mutationType, agentID string) map[string]any {
	t.Helper()
	recs := agentAudits(t, s, mutationType, agentID)
	require.Len(t, recs, 1, "one %s record", mutationType)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &m))
	return m
}

// hookLog records hook calls in order.
type hookLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *hookLog) add(name string) {
	l.mu.Lock()
	l.calls = append(l.calls, name)
	l.mu.Unlock()
}

func (l *hookLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// recordingHook logs name, runs check (when non-nil) against the
// transaction, and returns err.
func recordingHook(l *hookLog, name string, check func(tx store.Store, a *store.Agent), err error) AgentTxHook {
	return func(_ context.Context, tx store.Store, a *store.Agent, _ AuditActor) error {
		l.add(name)
		if check != nil {
			check(tx, a)
		}
		return err
	}
}

var errHookRefused = errors.New("hook refused")

// softDeleteForTest soft-deletes agent through the delete engine.
func softDeleteForTest(t *testing.T, srv *Server, agentID string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func restoreForTest(t *testing.T, srv *Server, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/restore", nil)
}

// --- hook order ---

// Soft delete: the row write (DeletedAt and a fresh SoftDeleteOpID) and the
// edge deactivation are visible to the hooks, the hooks run in registration
// order, and the audit record is written after them.
func TestSoftDeleteTxHookOrder(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "soft-order", state.PhaseStopped)
	seedAgentEdge(t, s, tid("delegator"), agent)

	var log hookLog
	check := func(tx store.Store, a *store.Agent) {
		row, err := tx.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.False(t, row.DeletedAt.IsZero(), "the row is soft-deleted before the hooks")
		assert.NotEmpty(t, row.SoftDeleteOpID, "the operation ID is stamped before the hooks")
		assert.Equal(t, row.SoftDeleteOpID, a.SoftDeleteOpID, "the hook sees the written row")
		assert.Empty(t, activeEdgeIDs(t, tx, a.ID), "edges are deactivated before the hooks")
		assert.Empty(t, agentAudits(t, tx, mutationTypeAgentSoftDelete, a.ID), "the audit record is written after the hooks")
	}
	srv.RegisterSoftDeleteHook("first", recordingHook(&log, "first", check, nil))
	srv.RegisterSoftDeleteHook("second", recordingHook(&log, "second", nil, nil))
	srv.RegisterHardDeleteHook("hard", recordingHook(&log, "hard", nil, nil))

	softDeleteForTest(t, srv, agent.ID)

	assert.Equal(t, []string{"first", "second"}, log.get(), "soft hooks in order; hard hooks do not run")
	got := mustGetAgent(t, s, agent.ID)
	require.NotEmpty(t, got.SoftDeleteOpID)
	sum := auditSummary(t, s, mutationTypeAgentSoftDelete, agent.ID)
	assert.Equal(t, got.SoftDeleteOpID, sum["op_id"])
	assert.EqualValues(t, 1, sum["edges_deactivated"])
	assert.Empty(t, activeEdgeIDs(t, s, agent.ID))
}

// Hard delete: the row is gone and the edges are deactivated when the hooks
// run, the hooks run in order, soft hooks do not run, and the audit record
// follows. The hard path writes no agent row (the hard delete would fail if
// it did).
func TestHardDeleteTxHookOrder(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "hard-order", state.PhaseStopped)
	seedAgentEdge(t, s, tid("delegator"), agent)

	var log hookLog
	check := func(tx store.Store, a *store.Agent) {
		assert.Equal(t, agent.ID, a.ID, "the hook gets the pre-delete row")
		_, err := tx.GetAgent(context.Background(), a.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "the row is removed before the hooks")
		assert.Empty(t, activeEdgeIDs(t, tx, a.ID), "edges are deactivated before the hooks")
		assert.Empty(t, agentAudits(t, tx, mutationTypeAgentHardDelete, a.ID), "the audit record is written after the hooks")
	}
	srv.RegisterHardDeleteHook("first", recordingHook(&log, "first", check, nil))
	srv.RegisterHardDeleteHook("second", recordingHook(&log, "second", nil, nil))
	srv.RegisterSoftDeleteHook("soft", recordingHook(&log, "soft", nil, nil))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, []string{"first", "second"}, log.get(), "hard hooks in order; soft hooks do not run")
	assert.True(t, agentGone(t, s, agent.ID))
	sum := auditSummary(t, s, mutationTypeAgentHardDelete, agent.ID)
	assert.NotEmpty(t, sum["op_id"])
	assert.EqualValues(t, 1, sum["edges_deactivated"])
	_, hasIncomplete := sum["incomplete_create"]
	assert.False(t, hasIncomplete, "a complete agent is not marked incomplete_create")
	assert.Empty(t, activeEdgeIDs(t, s, agent.ID))
}

// Restore: the row is restored and exactly the edges are reactivated when the
// hooks run, the hooks run in order, and the audit record follows.
func TestRestoreTxHookOrder(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "restore-order", state.PhaseStopped)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	softDeleteForTest(t, srv, agent.ID)
	opID := mustGetAgent(t, s, agent.ID).SoftDeleteOpID
	require.NotEmpty(t, opID)

	var log hookLog
	check := func(tx store.Store, a *store.Agent) {
		row, err := tx.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.True(t, row.DeletedAt.IsZero(), "the row is restored before the hooks")
		assert.Empty(t, row.SoftDeleteOpID, "the operation ID is cleared before the hooks")
		assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, tx, a.ID), "edges are reactivated before the hooks")
		assert.Empty(t, agentAudits(t, tx, mutationTypeAgentRestore, a.ID), "the audit record is written after the hooks")
	}
	srv.RegisterRestoreHook("first", recordingHook(&log, "first", check, nil))
	srv.RegisterRestoreHook("second", recordingHook(&log, "second", nil, nil))

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, []string{"first", "second"}, log.get())
	sum := auditSummary(t, s, mutationTypeAgentRestore, agent.ID)
	assert.Equal(t, opID, sum["op_id"])
	assert.EqualValues(t, 1, sum["edges_reactivated"])
	assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, s, agent.ID))
}

// Reincarnation claim: the claim, the record and the re-recorded edge are
// visible to the hooks, the hooks run in order, and the audit record follows.
func TestReincarnateClaimTxHookOrder(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
	})

	var log hookLog
	check := func(tx store.Store, a *store.Agent) {
		row, err := tx.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ReincarnationStatePending, row.ReincarnationState, "the claim is written before the hooks")
		recs, err := tx.ListAgentReincarnations(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Len(t, recs, 1, "the record is created before the hooks")
		edges, err := tx.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, a.ID)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assert.Equal(t, coordinator.ID, edges[0].DelegatorID, "the edge is re-recorded before the hooks")
		assert.Empty(t, agentAudits(t, tx, mutationTypeAgentReincarnateClaim, a.ID), "the audit record is written after the hooks")
	}
	srv.RegisterReincarnateClaimHook("first", recordingHook(&log, "first", check, nil))
	srv.RegisterReincarnateClaimHook("second", recordingHook(&log, "second", nil, nil))

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, delegatingRequesterFor(coordinator.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	assert.Equal(t, []string{"first", "second"}, log.get())
	sum := auditSummary(t, s, mutationTypeAgentReincarnateClaim, agent.ID)
	assert.Equal(t, true, sum["re_recorded"])
	assert.EqualValues(t, 1, sum["edges_replaced"])
	assert.NotEmpty(t, sum["reincarnation_id"])
}

// --- hook errors roll back ---

// A soft-delete hook error rolls back the finalize: the row is live and not
// soft-deleted, no operation ID, the edge is active, no audit, the delete
// fails with finalize_failed, and no deleted event is published. A retry
// without the failing hook completes.
func TestSoftDeleteHookErrorRollsBack(t *testing.T) {
	srv, s, pub, disp := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "soft-rollback", state.PhaseRunning)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	fail := true
	srv.RegisterSoftDeleteHook("refuse", func(context.Context, store.Store, *store.Agent, AuditActor) error {
		if fail {
			return errHookRefused
		}
		return nil
	})

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeFinalizeFailed, details["deletionCode"])
	assert.Equal(t, 0, pub.count("deleted"), "no deleted event")

	got := mustGetAgent(t, s, agent.ID)
	assert.True(t, got.DeletedAt.IsZero(), "not soft-deleted")
	assert.Empty(t, got.SoftDeleteOpID, "no operation ID")
	assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
	assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, s, agent.ID), "the edge is active")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentSoftDelete, agent.ID))

	fail = false
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 1, disp.callCount(), "the retry finalizes without re-dispatching")
	assert.False(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
	assert.Empty(t, activeEdgeIDs(t, s, agent.ID))
}

// A hard-delete hook error rolls back the finalize: the row is present, the
// edge is active, no audit, and the delete fails with finalize_failed.
func TestHardDeleteHookErrorRollsBack(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "hard-rollback", state.PhaseRunning)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	srv.RegisterHardDeleteHook("refuse", recordingHook(&hookLog{}, "refuse", nil, errHookRefused))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, store.DeletionCodeFinalizeFailed, details["deletionCode"])

	assert.False(t, agentGone(t, s, agent.ID), "the row is present")
	assert.Equal(t, store.DeletionStateFinalizing, mustGetAgent(t, s, agent.ID).DeletionState)
	assert.Equal(t, []string{edge.ID}, activeEdgeIDs(t, s, agent.ID), "the edge is active")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentHardDelete, agent.ID))
}

// A restore hook error rolls back the restore: the row stays soft-deleted
// with its operation ID, the edge stays inactive, and no audit is written.
func TestRestoreHookErrorRollsBack(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "restore-rollback", state.PhaseStopped)
	seedAgentEdge(t, s, tid("delegator"), agent)
	softDeleteForTest(t, srv, agent.ID)
	before := mustGetAgent(t, s, agent.ID)
	srv.RegisterRestoreHook("refuse", recordingHook(&hookLog{}, "refuse", nil, errHookRefused))

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	got := mustGetAgent(t, s, agent.ID)
	assert.False(t, got.DeletedAt.IsZero(), "the row stays soft-deleted")
	assert.Equal(t, before.SoftDeleteOpID, got.SoftDeleteOpID, "the operation ID is kept")
	assert.Equal(t, before.StateVersion, got.StateVersion)
	assert.Empty(t, activeEdgeIDs(t, s, agent.ID), "the edge stays inactive")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentRestore, agent.ID))
}

// A reincarnate-claim hook error rolls back the claim: nothing is claimed,
// no record, the original edge is active and unchanged, and no audit.
func TestReincarnateClaimHookErrorRollsBack(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
	})
	srv.RegisterReincarnateClaimHook("refuse", recordingHook(&hookLog{}, "refuse", nil, errHookRefused))

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, delegatingRequesterFor(coordinator.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	assertNothingClaimed(t, s, agent, edge)
}

// assertNothingClaimed asserts agent is unchanged by a refused reincarnation:
// same state_version, no claim, no record, the original edge active and
// delegated by the original delegator, and no claim audit.
func assertNothingClaimed(t *testing.T, s store.Store, agent *store.Agent, edge *store.DelegationEdge) {
	t.Helper()
	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, agent.StateVersion, got.StateVersion, "nothing is claimed")
	assert.Equal(t, store.ReincarnationStateNone, got.ReincarnationState)
	recs, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, recs, "no reincarnation record")
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, edge.ID, edges[0].ID)
	assert.Equal(t, edge.DelegatorID, edges[0].DelegatorID)
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentReincarnateClaim, agent.ID))
}

// --- restore reactivation ---

// Restore reactivates only the edges deactivated under the operation ID
// stored on the row: an edge deactivated by an earlier soft delete, a create
// compensation, or a delegator delete stays inactive.
func TestRestoreReactivatesOnlyItsOwnEdges(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "restore-exact", state.PhaseStopped)
	ctx := context.Background()
	now := time.Now()

	// One active edge per scope at a time: deactivate each before the next.
	seedAgentEdge(t, s, tid("d-old-soft"), agent)
	_, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID,
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: "earlier-soft-delete"})
	require.NoError(t, err)
	seedAgentEdge(t, s, tid("d-compensation"), agent)
	_, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID,
		store.Deactivation{Cause: store.EdgeDeactivationCreateCompensation, At: &now, OpID: "compensation"})
	require.NoError(t, err)
	seedAgentEdge(t, s, tid("d-deleted"), agent)
	_, err = s.DeactivateDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, tid("d-deleted"),
		store.Deactivation{Cause: store.EdgeDeactivationDelegatorDeleted, At: &now, OpID: "delegator-delete"})
	require.NoError(t, err)
	current := seedAgentEdge(t, s, tid("d-current"), agent)

	softDeleteForTest(t, srv, agent.ID)
	require.Empty(t, activeEdgeIDs(t, s, agent.ID))

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{current.ID}, activeEdgeIDs(t, s, agent.ID), "only this soft delete's edge is reactivated")
	assert.EqualValues(t, 1, auditSummary(t, s, mutationTypeAgentRestore, agent.ID)["edges_reactivated"])
}

// An agent with no stored operation ID (soft-deleted without one) restores
// with nothing reactivated, even when an edge carries the soft-delete cause.
func TestRestoreNullOpIDReactivatesNothing(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "restore-null-op", state.PhaseStopped)
	ctx := context.Background()
	now := time.Now()
	seedAgentEdge(t, s, tid("delegator"), agent)
	_, err := s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID,
		store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, At: &now, OpID: "unrecorded"})
	require.NoError(t, err)
	row := mustGetAgent(t, s, agent.ID)
	row.DeletedAt = now
	require.NoError(t, s.UpdateAgent(ctx, row))
	require.Empty(t, mustGetAgent(t, s, agent.ID).SoftDeleteOpID)

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
	assert.Empty(t, activeEdgeIDs(t, s, agent.ID), "nothing is reactivated")
	sum := auditSummary(t, s, mutationTypeAgentRestore, agent.ID)
	assert.EqualValues(t, 0, sum["edges_reactivated"])
	_, hasOp := sum["op_id"]
	assert.False(t, hasOp)
}

// A conflicting active edge makes the restore roll back with 409: the row
// stays soft-deleted with its operation ID, and only the conflicting edge
// is active.
func TestRestoreConflictingEdgeRollsBack409(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "restore-conflict", state.PhaseStopped)
	seedAgentEdge(t, s, tid("delegator"), agent)
	softDeleteForTest(t, srv, agent.ID)
	before := mustGetAgent(t, s, agent.ID)
	conflicting := seedAgentEdge(t, s, tid("other-delegator"), agent)

	rec := restoreForTest(t, srv, agent.ID)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	got := mustGetAgent(t, s, agent.ID)
	assert.False(t, got.DeletedAt.IsZero(), "the row stays soft-deleted")
	assert.Equal(t, before.SoftDeleteOpID, got.SoftDeleteOpID, "the operation ID is kept")
	assert.Equal(t, []string{conflicting.ID}, activeEdgeIDs(t, s, agent.ID))
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentRestore, agent.ID))
}

// --- reincarnation authority ---

// Self-reincarnation keeps the existing edge unchanged.
func TestSelfReincarnateKeepsExistingEdge(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, agentIdentityFor(agent.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, edge.ID, edges[0].ID)
	assert.Equal(t, edge.DelegatorID, edges[0].DelegatorID)
	assert.Equal(t, edge.AuthorityProvenance, edges[0].AuthorityProvenance)
	sum := auditSummary(t, s, mutationTypeAgentReincarnateClaim, agent.ID)
	assert.Equal(t, false, sum["re_recorded"])
	assert.EqualValues(t, 0, sum["edges_replaced"])
}

// A requester who passes the lifecycle check but fails CanDelegate for the
// agent's role gets 403 with nothing claimed, on a dry run and a real run.
func TestReincarnateRequiresCanDelegate(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
	})
	lifecycleOnly := agentIdentityFor(coordinator.ID, project.ID, ScopeAgentLifecycle)

	for _, dryRun := range []bool{true, false} {
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, lifecycleOnly, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}), agent.ID)
		require.Equal(t, http.StatusForbidden, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "Cannot delegate agent authority")
	}
	assertNothingClaimed(t, s, agent, edge)
}

// A reincarnation by another agent writes a new edge with the requester's
// provenance and ceiling and the agent's role; the replaced edge is
// deactivated under the claim's operation ID with cause
// reincarnate_replaced.
func TestReincarnateReRecordsProvenance(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	old := seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
	})

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, delegatingRequesterFor(coordinator.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	e := edges[0]
	assert.NotEqual(t, old.ID, e.ID)
	assert.Equal(t, store.DelegationPrincipalAgent, e.DelegatorType)
	assert.Equal(t, coordinator.ID, e.DelegatorID)
	assert.Equal(t, store.RoleScopeProject, e.ScopeType)
	assert.Equal(t, project.ID, e.ScopeID)
	assert.Equal(t, string(AgentRoleBaseline), e.Role)
	assert.Equal(t, store.ProvenanceVersionV1, e.ProvenanceVersion)
	assert.Equal(t, store.DelegationPrincipalAgent, e.SourcePrincipalKind)
	assert.Equal(t, coordinator.ID, e.SourcePrincipalID)
	assert.Equal(t, store.SourceCredentialAgent, e.SourceCredentialKind)
	assert.NotEqual(t, store.EffectCeilingUnrecorded, e.EffectCeiling.Kind, "the requester's ceiling is recorded")

	sum := auditSummary(t, s, mutationTypeAgentReincarnateClaim, agent.ID)
	assert.Equal(t, true, sum["re_recorded"])
	assert.EqualValues(t, 1, sum["edges_replaced"])
	opID, _ := sum["op_id"].(string)
	require.NotEmpty(t, opID)
	// Oracle for the inactive edge: reactivating it under the claim's
	// operation ID and cause collides with the new active edge.
	_, err = s.ReactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID, store.EdgeDeactivationReincarnateReplaced, opID)
	assert.ErrorIs(t, err, store.ErrAlreadyExists, "the replaced edge is inactive under the claim's operation ID")
}

// A requester whose effect ceiling does not cover the agent's role gets 403
// (delegation_ceiling) with nothing claimed, even when its token scopes pass
// CanDelegate.
func TestReincarnateOverCeilingForbidden(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	edge := seedAgentEdge(t, s, tid("delegator"), agent)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
		a.AppliedConfig.AgentRole = string(AgentRoleNone)
	})
	// Without the dev-auth mint override the requester's coverage is its
	// stored role, none, which does not cover baseline's project:read.
	srv.authzService.mintDevAuthOverride = false

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, delegatingRequesterFor(coordinator.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, details := errorBody(t, rec)
	assert.Equal(t, string(DeniedByDelegationCeiling), details["denied_by"])

	assertNothingClaimed(t, s, agent, edge)
}

// --- incomplete create and the env-gather recreate ---

// The hard delete of an incomplete create is an agent_hard_delete with
// incomplete_create=true, and its edges are deactivated with the hard-delete
// cause.
func TestIncompleteCreateHardDeleteAudit(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent, launchID := launchingAgent(t, s, "incomplete", state.PhaseProvisioning, 5*time.Minute)
	launchReport(t, s, agent, launchID, store.LaunchReportStateFailed, 1)
	agent = mustGetAgent(t, s, agent.ID)
	require.True(t, agent.IsIncompleteCreate(), "precondition: a failed create launch, phase %s", agent.Phase)
	seedAgentEdge(t, s, tid("delegator"), agent)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	sum := auditSummary(t, s, mutationTypeAgentHardDelete, agent.ID)
	assert.Equal(t, true, sum["incomplete_create"])
	assert.EqualValues(t, 1, sum["edges_deactivated"])
	opID, _ := sum["op_id"].(string)
	require.NotEmpty(t, opID)
	n, err := s.ReactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agent.ID, store.EdgeDeactivationAgentHardDelete, opID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the edge was deactivated with the hard-delete cause under the audit's operation ID")
}

// The env-gather recreate hard-deletes the existing provisioning agent as a
// hard-delete lifecycle transaction: its edges are deactivated and an
// agent_hard_delete record is written.
func TestEnvGatherRecreateDeactivatesEdges(t *testing.T) {
	disp := &createAgentDispatcher{
		envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}},
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	body := CreateAgentRequest{Name: "env-gather-edges", ProjectID: project.ID, Task: "t", GatherEnv: true}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.NotNil(t, disp.capturedAgent)
	oldID := disp.capturedAgent.ID
	require.NotEmpty(t, activeEdgeIDs(t, s, oldID), "precondition: the create recorded an edge")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.Less(t, rec.Code, 300, rec.Body.String())

	assert.True(t, agentGone(t, s, oldID))
	assert.Empty(t, activeEdgeIDs(t, s, oldID), "no active edge outlives the deleted agent")
	assert.Len(t, agentAudits(t, s, mutationTypeAgentHardDelete, oldID), 1)
}
