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
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3958: every create rollback (cleanupFailedCreate) removes the
// failed create's row only if no delete holds it, on the HTTP create's
// non-managed failure paths and on the scheduler's dispatch_agent rollback
// too. A delete holds the row with a live deleting claim or while
// finalizing (even with its lease expired), and a row already removed is
// the delete's as well: the row, its edge and its quotas are then left to
// that delete. A failed delete, or a deleting row whose lease lapsed, does
// not hold it, and the create is rolled back. Either way the HTTP answer and
// the scheduler event error are the ones the failure gave before.

// rollbackClaimStore runs onFinalize before every FinalizeAgentDeletion
// (the rollback's compensation is call 1, the fallback's conditional row
// deletes follow) and counts store.DeleteAgent calls outside a
// transaction.
type rollbackClaimStore struct {
	store.Store
	mu            sync.Mutex
	onFinalize    func(call int, agentID string)
	finalizeCalls int
	deleteCalls   int
	agentID       string
}

func (s *rollbackClaimStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	s.mu.Lock()
	s.finalizeCalls++
	call := s.finalizeCalls
	if s.agentID == "" {
		s.agentID = id
	}
	on := s.onFinalize
	s.mu.Unlock()
	if on != nil {
		on(call, id)
	}
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, hook)
}

func (s *rollbackClaimStore) DeleteAgent(ctx context.Context, id string) error {
	s.mu.Lock()
	s.deleteCalls++
	s.mu.Unlock()
	return s.Store.DeleteAgent(ctx, id)
}

func (s *rollbackClaimStore) snapshot() (agentID string, finalizeCalls, deleteCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID, s.finalizeCalls, s.deleteCalls
}

// rollbackDelete is a delete's state applied to the failed create's row
// just before a rollback row delete.
type rollbackDelete struct {
	name  string
	apply func(t *testing.T, s store.Store, id string)
	// held: the delete owns the row (or removed it), so the rollback must
	// leave it alone.
	held bool
	// rowState is the deletion state the kept row must still carry.
	rowState string
	// softDeleted: the delete already soft-deleted the row.
	softDeleted bool
}

func rollbackDeletes() []rollbackDelete {
	claim := func(st string, lease time.Duration) func(*testing.T, store.Store, string) {
		return func(t *testing.T, s store.Store, id string) { claimForTest(t, s, id, st, lease) }
	}
	return []rollbackDelete{
		{name: "delete claimed", apply: claim(store.DeletionStateDeleting, time.Minute), held: true, rowState: store.DeletionStateDeleting},
		{name: "finalizing", apply: claim(store.DeletionStateFinalizing, time.Minute), held: true, rowState: store.DeletionStateFinalizing},
		{name: "finalizing lease expired", apply: claim(store.DeletionStateFinalizing, -time.Minute), held: true, rowState: store.DeletionStateFinalizing},
		{name: "soft deleted", apply: func(t *testing.T, s store.Store, id string) {
			a, err := s.GetAgent(context.Background(), id)
			require.NoError(t, err)
			a.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(context.Background(), a))
		}, held: true, softDeleted: true},
		{name: "delete failed", apply: claim(store.DeletionStateFailed, time.Minute)},
		{name: "delete lease lapsed", apply: claim(store.DeletionStateDeleting, -time.Minute)},
	}
}

// httpRollbackSites are the non-managed HTTP create failure sites covered
// here: one per answer family (502 runtime_error, the run-intent answer,
// the dispatch answer, 422 missing_env_vars).
func httpRollbackSites(t *testing.T) []createRollbackSite {
	t.Helper()
	want := map[string]bool{"storage": true, "run intent": true, "dispatch": true, "missing env": true}
	var out []createRollbackSite
	for _, site := range createRollbackSites() {
		if want[site.name] {
			out = append(out, site)
		}
	}
	require.Len(t, out, len(want), "every covered site is still in createRollbackSites")
	return out
}

// httpRollbackRun is the outcome of one failed HTTP create.
type httpRollbackRun struct {
	status        int
	body          string
	agentID       string
	finalizeCalls int
	deleteCalls   int
	s             store.Store
	project       *store.Project
}

// runHTTPRollbackSite runs one create that fails at site. faultCompensation
// makes the rollback's compensation transaction fail (its audit insert), so
// the fallback row delete runs. onFinalize runs before each rollback row
// delete.
func runHTTPRollbackSite(t *testing.T, site createRollbackSite, faultCompensation bool, onFinalize func(t *testing.T, s store.Store, call int, id string)) httpRollbackRun {
	t.Helper()
	// A fresh dispatcher per run: the site's dispatcher records state.
	for _, fresh := range createRollbackSites() {
		if fresh.name == site.name {
			site = fresh
		}
	}
	srv, s, project := setupCreateAgentServer(t, site.disp)
	if site.setup != nil {
		site.setup(t, srv)
	}
	if faultCompensation {
		srv.store = &createTxFaultStore{Store: srv.store, auditErrFor: mutationTypeAgentCreateDispatchFailed}
	}
	cs := &rollbackClaimStore{Store: srv.store}
	if onFinalize != nil {
		cs.onFinalize = func(call int, id string) { onFinalize(t, s, call, id) }
	}
	srv.store = cs

	req := site.req
	req.Name = "r3958-" + tidSlugSafe(site.name)
	req.ProjectID = project.ID
	req.Task = "do something"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
	agentID, finalizeCalls, deleteCalls := cs.snapshot()
	require.NotEmpty(t, agentID, "the rollback ran: %d %s", rec.Code, rec.Body.String())
	return httpRollbackRun{
		status:        rec.Code,
		body:          strings.ReplaceAll(rec.Body.String(), agentID, "<agent-id>"),
		agentID:       agentID,
		finalizeCalls: finalizeCalls,
		deleteCalls:   deleteCalls,
		s:             s,
		project:       project,
	}
}

// assertRowLeftToDelete checks a held row was not touched by the rollback:
// it keeps its deletion state, its phase is not marked failed, its edge
// stays active, no compensation was recorded, and its quotas are held.
func assertRowLeftToDelete(t *testing.T, s store.Store, brokerID, agentID string, del rollbackDelete) {
	t.Helper()
	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err, "the delete's row is kept")
	assert.Equal(t, del.rowState, row.DeletionState, "the delete's state is kept")
	assert.Equal(t, del.softDeleted, !row.DeletedAt.IsZero(), "the row's soft delete is the delete's own")
	assert.NotEqual(t, string(state.PhaseError), row.Phase, "no phase-error write")
	assert.NotEqual(t, createRowRemoveFailedMessage, row.Message)
	assert.Len(t, activeEdgesFor(t, s, agentID), 1, "the edge is left to the delete")
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
	if brokerID != "" {
		assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerID), "the quotas are left to the delete")
	}
}

// assertRolledBack checks the create was rolled back at stage.
func assertRolledBack(t *testing.T, s store.Store, brokerID, agentID, stage string) {
	t.Helper()
	assert.True(t, agentGone(t, s, agentID), "the agent row is rolled back")
	sum := assertCompensated(t, s, agentID)
	assert.Equal(t, stage, sum.Stage)
	if brokerID != "" {
		assert.EqualValues(t, 0, brokerReservationCount(t, s, brokerID), "the quotas are released")
	}
}

// (A) HTTP create, non-managed failure sites: a delete that holds the row
// when the rollback runs keeps it, and the answer is byte-identical to the
// same failure with no delete; a failed or lapsed delete does not hold it,
// and the create is rolled back with the same answer.
func TestFix3958_HTTPCreateRollback_DeferToHeldRow(t *testing.T) {
	for _, site := range httpRollbackSites(t) {
		t.Run(site.name, func(t *testing.T) {
			plain := runHTTPRollbackSite(t, site, false, nil)
			require.GreaterOrEqual(t, plain.status, 400, plain.body)
			require.NotContains(t, plain.body, "correlation_id", "the plain rollback completed")
			assertRolledBack(t, plain.s, plain.project.DefaultRuntimeBrokerID, plain.agentID, site.wantStage)
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")
			assert.Equal(t, 1, plain.finalizeCalls, "one conditional compensation")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runHTTPRollbackSite(t, site, false, func(t *testing.T, s store.Store, call int, id string) {
						if call == 1 {
							del.apply(t, s, id)
						}
					})
					assert.Equal(t, plain.status, run.status, "the answer's status is unchanged")
					assert.Equal(t, plain.body, run.body, "the answer's body is unchanged")
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						assertRowLeftToDelete(t, run.s, run.project.DefaultRuntimeBrokerID, run.agentID, del)
						assert.Equal(t, 1, run.finalizeCalls, "the refused compensation, and no fallback")
						return
					}
					assertRolledBack(t, run.s, run.project.DefaultRuntimeBrokerID, run.agentID, site.wantStage)
				})
			}
		})
	}
}

// (A) The fallback row delete, after a failed compensation, is conditional
// too: a delete that claims the row before it keeps the row, and the answer
// is the same 500 with a correlation ID as when the fallback removes the
// row.
func TestFix3958_HTTPCreateRollback_FallbackDefersToHeldRow(t *testing.T) {
	for _, site := range httpRollbackSites(t) {
		t.Run(site.name, func(t *testing.T) {
			plain := runHTTPRollbackSite(t, site, true, nil)
			requireRollbackIncomplete500(t, plain)
			assert.True(t, agentGone(t, plain.s, plain.agentID), "the fallback removes the row")
			assert.EqualValues(t, 0, brokerReservationCount(t, plain.s, plain.project.DefaultRuntimeBrokerID), "the quotas are released")
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runHTTPRollbackSite(t, site, true, func(t *testing.T, s store.Store, call int, id string) {
						if call == 2 {
							del.apply(t, s, id)
						}
					})
					requireRollbackIncomplete500(t, run)
					assert.Equal(t, normalizeCorrelationID(plain.body), normalizeCorrelationID(run.body), "the answer is unchanged")
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						assertRowLeftToDelete(t, run.s, run.project.DefaultRuntimeBrokerID, run.agentID, del)
						assert.Equal(t, 2, run.finalizeCalls, "the failed compensation, then one refused fallback delete")
						return
					}
					assert.True(t, agentGone(t, run.s, run.agentID), "the fallback removes the row")
					assert.EqualValues(t, 0, brokerReservationCount(t, run.s, run.project.DefaultRuntimeBrokerID), "the quotas are released")
				})
			}
		})
	}
}

// requireRollbackIncomplete500 checks run answered the 500 that carries a
// compensation correlation ID.
func requireRollbackIncomplete500(t *testing.T, run httpRollbackRun) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, run.status, run.body)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(run.body), &body))
	assert.Equal(t, ErrCodeInternalError, body.Error.Code)
	assert.NotEmpty(t, body.Error.Details["correlation_id"])
}

var correlationIDPattern = regexp.MustCompile(`("correlation_id":"|correlation ID )[^")]+`)

// normalizeCorrelationID replaces a generated correlation ID.
func normalizeCorrelationID(s string) string {
	return correlationIDPattern.ReplaceAllString(s, "${1}<correlation-id>")
}

// schedFailingDispatcher fails the scheduler's DispatchAgentCreate.
type schedFailingDispatcher struct {
	failingCreateDispatcher
}

func (d *schedFailingDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.capturedAgent = agent
	return nil, d.createErr
}

// schedRollbackStage is one scheduler rollback site.
type schedRollbackStage struct {
	name  string
	stage string
	setup func(srv *Server)
}

func schedRollbackStages() []schedRollbackStage {
	return []schedRollbackStage{
		{name: "run intent", stage: createStageRunIntent, setup: func(srv *Server) {
			srv.store = runIntentErrStore{srv.store}
		}},
		{name: "dispatch", stage: createStageDispatch},
	}
}

// schedRollbackRun is the outcome of one failed scheduled fire.
type schedRollbackRun struct {
	errText       string
	agentID       string
	finalizeCalls int
	deleteCalls   int
	s             store.Store
}

// runSchedRollback fires one dispatch_agent event that fails at st.
func runSchedRollback(t *testing.T, st schedRollbackStage, faultCompensation bool, onFinalize func(t *testing.T, s store.Store, call int, id string)) schedRollbackRun {
	t.Helper()
	f := newSchedFire(t, "s3958-"+tidSlugSafe(st.name))
	f.srv.SetDispatcher(&schedFailingDispatcher{failingCreateDispatcher{createErr: errors.New("broker unavailable")}})
	if st.setup != nil {
		st.setup(f.srv)
	}
	if faultCompensation {
		f.srv.store = &createTxFaultStore{Store: f.srv.store, auditErrFor: mutationTypeAgentCreateDispatchFailed}
	}
	cs := &rollbackClaimStore{Store: f.srv.store}
	if onFinalize != nil {
		cs.onFinalize = func(call int, id string) { onFinalize(t, f.store, call, id) }
	}
	f.srv.store = cs

	slug := "s3958-child"
	err := f.fire(t, withSessionRevision(f.event(slug), f.creator.ID))
	require.Error(t, err, "the fire fails")
	agentID, finalizeCalls, deleteCalls := cs.snapshot()
	require.NotEmpty(t, agentID, "the rollback ran: %v", err)
	return schedRollbackRun{
		errText:       normalizeCorrelationID(strings.ReplaceAll(err.Error(), agentID, "<agent-id>")),
		agentID:       agentID,
		finalizeCalls: finalizeCalls,
		deleteCalls:   deleteCalls,
		s:             f.store,
	}
}

// (B) The scheduler's dispatch_agent rollback: a delete that holds the row
// keeps it, and the fire fails with the same error text as the same failure
// with no delete; a failed or lapsed delete does not hold it, and the
// create is rolled back. The scheduled create takes no quota reservation,
// so there is none to hold or release.
func TestFix3958_SchedDispatchRollback_DeferToHeldRow(t *testing.T) {
	for _, st := range schedRollbackStages() {
		t.Run(st.name, func(t *testing.T) {
			plain := runSchedRollback(t, st, false, nil)
			assert.True(t, strings.HasPrefix(plain.errText, `failed to dispatch agent "s3958-child": `), plain.errText)
			assert.NotContains(t, plain.errText, "rollback incomplete")
			assertRolledBack(t, plain.s, "", plain.agentID, st.stage)
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runSchedRollback(t, st, false, func(t *testing.T, s store.Store, call int, id string) {
						if call == 1 {
							del.apply(t, s, id)
						}
					})
					assert.Equal(t, plain.errText, run.errText, "the event error is unchanged")
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						assertRowLeftToDelete(t, run.s, "", run.agentID, del)
						assert.Equal(t, 1, run.finalizeCalls, "the refused compensation, and no fallback")
						return
					}
					assertRolledBack(t, run.s, "", run.agentID, st.stage)
				})
			}
		})
	}
}

// (B) The scheduler rollback's fallback row delete is conditional too.
func TestFix3958_SchedDispatchRollback_FallbackDefersToHeldRow(t *testing.T) {
	for _, st := range schedRollbackStages() {
		t.Run(st.name, func(t *testing.T) {
			plain := runSchedRollback(t, st, true, nil)
			assert.Contains(t, plain.errText, "(rollback incomplete, correlation ID <correlation-id>)")
			assert.True(t, agentGone(t, plain.s, plain.agentID), "the fallback removes the row")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runSchedRollback(t, st, true, func(t *testing.T, s store.Store, call int, id string) {
						if call == 2 {
							del.apply(t, s, id)
						}
					})
					assert.Equal(t, plain.errText, run.errText, "the event error is unchanged")
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						assertRowLeftToDelete(t, run.s, "", run.agentID, del)
						assert.Equal(t, 2, run.finalizeCalls, "the failed compensation, then one refused fallback delete")
						return
					}
					assert.True(t, agentGone(t, run.s, run.agentID), "the fallback removes the row")
				})
			}
		})
	}
}

// cleanupFailedCreate reports the outcome through DeleteWon when the caller
// asks for it, and writes it only once the rollback is done.
func TestFix3958_CleanupFailedCreate_ReportsDeleteWon(t *testing.T) {
	for _, del := range rollbackDeletes() {
		t.Run(del.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
			ctx := context.Background()
			agent := &store.Agent{
				ID:              tid("r3958-direct"),
				Name:            "r3958-direct",
				Slug:            "r3958-direct",
				ProjectID:       project.ID,
				RuntimeBrokerID: project.DefaultRuntimeBrokerID,
			}
			require.NoError(t, s.CreateAgent(ctx, agent))
			del.apply(t, s, agent.ID)

			deleteWon := !del.held
			corrID := srv.cleanupFailedCreate(ctx, createRollback{
				Agent:           agent,
				RuntimeBrokerID: agent.RuntimeBrokerID,
				Stage:           createStageDispatch,
				Cause:           fmt.Errorf("dispatch failed"),
				DeleteWon:       &deleteWon,
			})
			assert.Empty(t, corrID)
			assert.Equal(t, del.held, deleteWon, "DeleteWon reports whether a delete owns the row")
			assert.Equal(t, !del.held, agentGone(t, s, agent.ID), "only a row no delete holds is removed")
		})
	}
}
