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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A failed create's rollback (cleanupFailedCreate) and the agent's
// service-account assignment. When a delete holds the row
// (ptone/scion#3958, ptone/scion#4061), the rollback leaves the agent's
// authority to that delete: its assignment and its delegation edge stay
// active and unchanged, no hook runs and nothing is audited. When the
// rollback removes the row, the compensation deactivates the edge and the
// assignment, runs the hard-delete hooks and writes its audit record, all
// in one transaction under one op ID; the fallback that removes the row
// after a failed compensation deactivates both too.

// rollbackHold is the rollback row delete a delete's state is applied
// before: the compensation (call 1), or the fallback's first row delete
// after a failed compensation (call 2).
type rollbackHold struct {
	name         string
	compensation bool
	call         int
}

func rollbackHolds() []rollbackHold {
	return []rollbackHold{
		{name: "compensation", call: 1},
		{name: "fallback", compensation: true, call: 2},
	}
}

// rollbackAssignmentFixture is a created agent with an active delegation
// edge and an active service-account assignment, on a server that records
// its hard-delete and soft-delete hook calls.
type rollbackAssignmentFixture struct {
	srv        *Server
	s          store.Store
	agent      *store.Agent
	edge       *store.DelegationEdge
	assignment *store.AgentServiceAccountAssignment
	hooks      *hookLog
}

func newRollbackAssignmentFixture(t *testing.T) *rollbackAssignmentFixture {
	t.Helper()
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	agent := &store.Agent{
		ID:              tid("sa-rollback"),
		Name:            "sa-rollback",
		Slug:            "sa-rollback",
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	seedAgentEdge(t, s, tid("delegator"), agent)
	// The edge as the store reads it back, to compare later reads with.
	edges := activeEdgesFor(t, s, agent.ID)
	require.Len(t, edges, 1)
	f := &rollbackAssignmentFixture{
		srv:        srv,
		s:          s,
		agent:      agent,
		edge:       edges[0],
		assignment: seedAssignment(t, s, agent, tid("delegator"), "sa-rollback-1"),
		hooks:      &hookLog{},
	}
	srv.RegisterHardDeleteHook("rollback-hard", recordingHook(f.hooks, "hard", func(tx store.Store, a *store.Agent) {
		rows, err := tx.GetActiveAgentServiceAccountAssignments(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Empty(t, rows, "the assignment is deactivated before the hooks, in the hooks' transaction")
	}, nil))
	srv.RegisterSoftDeleteHook("rollback-soft", recordingHook(f.hooks, "soft", nil, nil))
	return f
}

// rollback runs cleanupFailedCreate for the fixture's agent and returns
// the correlation ID and whether a delete won.
func (f *rollbackAssignmentFixture) rollback() (corrID string, deleteWon bool) {
	corrID = f.srv.cleanupFailedCreate(context.Background(), createRollback{
		Agent:           f.agent,
		RuntimeBrokerID: f.agent.RuntimeBrokerID,
		Stage:           createStageDispatch,
		Cause:           errors.New("dispatch failed"),
		DeleteWon:       &deleteWon,
	})
	return corrID, deleteWon
}

// assertAuthorityLeftToDelete checks the rollback left the agent's
// authority as it was: the same assignment and edge active, with no
// deactivation recorded on the assignment, no hook run and no
// compensation, soft-delete or hard-delete record.
func assertAuthorityLeftToDelete(t *testing.T, s store.Store, agentID string, assignment *store.AgentServiceAccountAssignment, edge *store.DelegationEdge) {
	t.Helper()
	rows := activeAssignments(t, s, agentID)
	require.Len(t, rows, 1, "the assignment is left to the delete")
	assert.Equal(t, assignment.ID, rows[0].ID)
	assert.Equal(t, assignment.ServiceAccountID, rows[0].ServiceAccountID)
	assert.Empty(t, rows[0].Deactivation.Cause, "no deactivation cause on the assignment")
	assert.Empty(t, rows[0].Deactivation.OpID, "no deactivation op on the assignment")

	edges := activeEdgesFor(t, s, agentID)
	require.Len(t, edges, 1, "the edge is left to the delete")
	assert.Equal(t, edge.ID, edges[0].ID)
	assert.Equal(t, edge.AuthorityProvenance, edges[0].AuthorityProvenance, "the edge's provenance is unchanged")
	assert.Equal(t, edge.EffectCeiling, edges[0].EffectCeiling, "the edge's ceiling is unchanged")

	assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation record")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentSoftDelete, agentID), "no soft-delete record")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentHardDelete, agentID), "no hard-delete record")
}

// compensationOpID returns the op ID of agentID's single compensation
// record.
func compensationOpID(t *testing.T, s store.Store, agentID string) string {
	t.Helper()
	recs := agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID)
	require.Len(t, recs, 1, "one compensation record")
	var sum compensationSummary
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &sum))
	require.NotEmpty(t, sum.OpID)
	return sum.OpID
}

// assertCompensationDeactivatedAuthority checks the compensation
// deactivated the agent's assignment and edge with cause
// create_compensation under the op ID its record names: reactivating
// exactly that (cause, op ID) finds one of each.
func assertCompensationDeactivatedAuthority(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.Empty(t, activeAssignments(t, s, agentID), "no active assignment")
	assert.Empty(t, activeEdgesFor(t, s, agentID), "no active edge")
	opID := compensationOpID(t, s, agentID)
	n, err := s.ReactivateAgentServiceAccountAssignments(context.Background(), agentID, store.EdgeDeactivationCreateCompensation, opID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the assignment was deactivated by the compensation that wrote the record")
	n, err = s.ReactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID, store.EdgeDeactivationCreateCompensation, opID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the edge was deactivated by the same compensation")
}

// TestSAParentCeiling_CreateRollbackHeldByDeleteKeepsAssignment: a
// rollback that finds the row held by a delete, at the compensation or at
// the fallback's row delete, leaves the assignment, the edge and the row to
// that delete.
func TestSAParentCeiling_CreateRollbackHeldByDeleteKeepsAssignment(t *testing.T) {
	for _, hold := range rollbackHolds() {
		for _, del := range rollbackDeletes() {
			if !del.held {
				continue
			}
			t.Run(hold.name+"/"+del.name, func(t *testing.T) {
				f := newRollbackAssignmentFixture(t)
				// A failed compensation ran the hooks in its rolled-back
				// transaction; none runs from the held row delete on.
				hooksAtHold := -1
				_, fault := installRollbackStore(t, f.srv, f.s, rollbackFaults{
					compensation: hold.compensation,
					onFinalize: func(t *testing.T, s store.Store, call int, id string) {
						if call == hold.call {
							hooksAtHold = len(f.hooks.get())
							del.apply(t, s, id)
						}
					},
				})
				fault.Arm()

				_, deleteWon := f.rollback()
				require.True(t, deleteWon, "the delete owns the row")
				assertAuthorityLeftToDelete(t, f.s, f.agent.ID, f.assignment, f.edge)
				assertRowLeftToDelete(t, f.s, f.agent.ID, del, false)
				require.GreaterOrEqual(t, hooksAtHold, 0, "the held row delete ran")
				assert.Len(t, f.hooks.get(), hooksAtHold, "no hook runs from the held row delete on")
				assert.NotContains(t, f.hooks.get(), "soft", "no soft-delete hook runs")
			})
		}
	}
}

// TestSAParentCeiling_CreateRollbackDeactivatesAssignment: a rollback that
// removes the row (no delete, or one that does not hold it) deactivates
// the assignment and the edge in the compensation's transaction, runs the
// hard-delete hooks (and no soft-delete hook) and writes the compensation
// record. When the compensation fails, the fallback that removes the row
// deactivates both.
func TestSAParentCeiling_CreateRollbackDeactivatesAssignment(t *testing.T) {
	dels := []rollbackDelete{{name: "no delete"}}
	for _, del := range rollbackDeletes() {
		if !del.held {
			dels = append(dels, del)
		}
	}
	for _, del := range dels {
		t.Run(del.name, func(t *testing.T) {
			f := newRollbackAssignmentFixture(t)
			_, fault := installRollbackStore(t, f.srv, f.s, rollbackFaults{
				onFinalize: func(t *testing.T, s store.Store, call int, id string) {
					if call == 1 && del.apply != nil {
						del.apply(t, s, id)
					}
				},
			})
			fault.Arm()

			corrID, deleteWon := f.rollback()
			assert.Empty(t, corrID, "the compensation committed")
			assert.False(t, deleteWon)
			assert.True(t, agentGone(t, f.s, f.agent.ID), "the row is removed")
			assert.Equal(t, []string{"hard"}, f.hooks.get(), "the hard-delete hooks run; no soft-delete hook")
			assertCompensationDeactivatedAuthority(t, f.s, f.agent.ID)
			assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentSoftDelete, f.agent.ID), "no soft-delete record")
		})
	}

	t.Run("fallback removes the row", func(t *testing.T) {
		f := newRollbackAssignmentFixture(t)
		_, fault := installRollbackStore(t, f.srv, f.s, rollbackFaults{compensation: true})
		fault.Arm()

		corrID, deleteWon := f.rollback()
		assert.NotEmpty(t, corrID, "the failed compensation is reported")
		assert.False(t, deleteWon)
		assert.True(t, agentGone(t, f.s, f.agent.ID), "the fallback removes the row")
		assert.Empty(t, activeAssignments(t, f.s, f.agent.ID), "the fallback deactivates the assignment")
		assert.Empty(t, activeEdgesFor(t, f.s, f.agent.ID), "the fallback deactivates the edge")
		assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentCreateDispatchFailed, f.agent.ID), "the failed compensation wrote nothing")
	})
}

// TestSAParentCeiling_ScheduledCreateRollbackKeepsAssignmentForDelete: a
// scheduled create that records a project-default assignment and then
// fails to dispatch. When a delete holds the row at the rollback, the fire
// fails with errScheduledChildDeletedDuringCreate and the assignment and
// edge the create recorded stay active; otherwise the compensation
// deactivates both.
func TestSAParentCeiling_ScheduledCreateRollbackKeepsAssignmentForDelete(t *testing.T) {
	type outcome struct {
		f             *bypassAgentsFixture
		saID          string
		err           error
		agentID       string
		atHold        []store.AgentServiceAccountAssignment
		edgesAtHold   []*store.DelegationEdge
		finalizeCalls int
	}
	run := func(t *testing.T, hold *rollbackHold) outcome {
		t.Helper()
		f := bypassAgentsSetup(t)
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
		setProjectDefaultSAAnnotations(t, f, sa.ID)
		f.srv.SetDispatcher(&schedFailingDispatcher{failingCreateDispatcher{createErr: errors.New("broker unavailable")}})
		out := outcome{f: f, saID: sa.ID}
		faults := rollbackFaults{}
		if hold != nil {
			faults.compensation = hold.compensation
			faults.onFinalize = func(t *testing.T, s store.Store, call int, id string) {
				if call == hold.call {
					out.atHold = activeAssignments(t, s, id)
					out.edgesAtHold = activeEdgesFor(t, s, id)
					claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
				}
			}
		}
		cs, fault := installRollbackStore(t, f.srv, f.store, faults)
		fault.Arm()
		out.err = fireScheduledDispatchAsOwner(t, f, "sa-sched-rollback")
		require.Error(t, out.err, "the fire fails")
		out.agentID, out.finalizeCalls, _ = cs.snapshot()
		require.NotEmpty(t, out.agentID, "the rollback ran: %v", out.err)
		return out
	}

	for _, hold := range rollbackHolds() {
		t.Run("held at "+hold.name, func(t *testing.T) {
			out := run(t, &hold)
			require.ErrorIs(t, out.err, errScheduledChildDeletedDuringCreate)
			assert.Equal(t, hold.call, out.finalizeCalls, "the refused row delete is the last")
			require.Len(t, out.atHold, 1, "the create recorded the assignment")
			assert.Equal(t, out.saID, out.atHold[0].ServiceAccountID)
			require.Len(t, out.edgesAtHold, 1, "the create recorded the edge")
			assertAuthorityLeftToDelete(t, out.f.store, out.agentID, &out.atHold[0], out.edgesAtHold[0])
			row := mustGetAgent(t, out.f.store, out.agentID)
			assert.Equal(t, store.DeletionStateDeleting, row.DeletionState, "the delete's claim is kept")
			assert.True(t, row.DeletedAt.IsZero(), "the row is not soft-deleted")
		})
	}

	t.Run("no delete", func(t *testing.T) {
		out := run(t, nil)
		assert.NotErrorIs(t, out.err, errScheduledChildDeletedDuringCreate)
		assert.True(t, agentGone(t, out.f.store, out.agentID), "the row is removed")
		assertCompensationDeactivatedAuthority(t, out.f.store, out.agentID)
	})
}

// TestSAParentCeiling_HTTPCreateRollbackKeepsAssignmentForDelete: an HTTP
// create with an explicit service account whose dispatch fails. When a
// delete holds the row at the rollback, the create answers 409
// delete_in_progress and the assignment and edge it recorded stay active;
// otherwise the compensation deactivates both.
func TestSAParentCeiling_HTTPCreateRollbackKeepsAssignmentForDelete(t *testing.T) {
	type outcome struct {
		f           *bypassAgentsFixture
		saID        string
		status      int
		body        string
		agentID     string
		atHold      []store.AgentServiceAccountAssignment
		edgesAtHold []*store.DelegationEdge
	}
	run := func(t *testing.T, hold *rollbackHold) outcome {
		t.Helper()
		f := bypassAgentsSetup(t)
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
		f.srv.SetDispatcher(&failingCreateDispatcher{createErr: errors.New("broker unavailable")})
		out := outcome{f: f, saID: sa.ID}
		faults := rollbackFaults{}
		if hold != nil {
			faults.compensation = hold.compensation
			faults.onFinalize = func(t *testing.T, s store.Store, call int, id string) {
				if call == hold.call {
					out.atHold = activeAssignments(t, s, id)
					out.edgesAtHold = activeEdgesFor(t, s, id)
					claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
				}
			}
		}
		cs, fault := installRollbackStore(t, f.srv, f.store, faults)
		fault.Arm()
		rec := createAgentAsOwner(t, f, CreateAgentRequest{
			Name: "sa-http-rollback",
			Task: "do something",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID},
		})
		out.status, out.body = rec.Code, rec.Body.String()
		out.agentID, _, _ = cs.snapshot()
		require.NotEmpty(t, out.agentID, "the rollback ran: %d %s", out.status, out.body)
		return out
	}

	for _, hold := range rollbackHolds() {
		t.Run("held at "+hold.name, func(t *testing.T) {
			out := run(t, &hold)
			require.Equal(t, http.StatusConflict, out.status, out.body)
			var body ErrorResponse
			require.NoError(t, json.Unmarshal([]byte(out.body), &body))
			assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
			require.Len(t, out.atHold, 1, "the create recorded the assignment")
			assert.Equal(t, out.saID, out.atHold[0].ServiceAccountID)
			require.Len(t, out.edgesAtHold, 1, "the create recorded the edge")
			assertAuthorityLeftToDelete(t, out.f.store, out.agentID, &out.atHold[0], out.edgesAtHold[0])
			row := mustGetAgent(t, out.f.store, out.agentID)
			assert.Equal(t, store.DeletionStateDeleting, row.DeletionState, "the delete's claim is kept")
			assert.True(t, row.DeletedAt.IsZero(), "the row is not soft-deleted")
		})
	}

	t.Run("no delete", func(t *testing.T) {
		out := run(t, nil)
		require.Equal(t, http.StatusBadGateway, out.status, out.body)
		assert.True(t, agentGone(t, out.f.store, out.agentID), "the row is removed")
		assertCompensationDeactivatedAuthority(t, out.f.store, out.agentID)
	})
}
