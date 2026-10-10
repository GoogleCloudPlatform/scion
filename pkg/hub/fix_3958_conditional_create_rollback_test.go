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
// finalizing (even with its lease expired), and a row already removed or
// soft-deleted is the delete's as well: the row, its edge and its quotas
// are then left to that delete. A failed delete, or a deleting row whose
// lease lapsed, does not hold it, and the create is rolled back. The
// scheduler event error is the one the failure gave before. The HTTP create
// answers 409 delete_in_progress with details.agentId when a delete holds
// the row, and the failure's own answer otherwise (ptone/scion#4061).

// rollbackClaimStore observes a create's rollback once its switch is armed.
// It runs onFinalize before every FinalizeAgentDeletion (the rollback's
// compensation is call 1, the fallback's conditional row deletes follow),
// returns finalizeErr from every FinalizeAgentDeletion when set, and counts
// store.DeleteAgent calls, inside a transaction or not. The agent ID is
// taken from the first row delete of either kind.
type rollbackClaimStore struct {
	store.Store
	fault         *storeFaultSwitch
	finalizeErr   error
	mu            sync.Mutex
	onFinalize    func(call int, agentID string)
	finalizeCalls int
	deleteCalls   int
	agentID       string
}

func (s *rollbackClaimStore) noteDelete(id string, finalize bool) (call int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentID == "" {
		s.agentID = id
	}
	if !finalize {
		s.deleteCalls++
		return 0
	}
	s.finalizeCalls++
	return s.finalizeCalls
}

func (s *rollbackClaimStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	if !s.fault.Active() {
		return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, hook)
	}
	call := s.noteDelete(id, true)
	s.mu.Lock()
	on := s.onFinalize
	s.mu.Unlock()
	if on != nil {
		on(call, id)
	}
	if s.finalizeErr != nil {
		return 0, s.finalizeErr
	}
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, hook)
}

func (s *rollbackClaimStore) DeleteAgent(ctx context.Context, id string) error {
	if s.fault.Active() {
		s.noteDelete(id, false)
	}
	return s.Store.DeleteAgent(ctx, id)
}

func (s *rollbackClaimStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&rollbackClaimTx{Store: tx, parent: s})
	})
}

// rollbackClaimTx counts a transaction's DeleteAgent calls on its parent.
type rollbackClaimTx struct {
	store.Store
	parent *rollbackClaimStore
}

func (tx *rollbackClaimTx) DeleteAgent(ctx context.Context, id string) error {
	if tx.parent.fault.Active() {
		tx.parent.noteDelete(id, false)
	}
	return tx.Store.DeleteAgent(ctx, id)
}

func (s *rollbackClaimStore) snapshot() (agentID string, finalizeCalls, deleteCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID, s.finalizeCalls, s.deleteCalls
}

// rollbackFaults are the store faults of one failed create: the run-intent
// write fails (runIntentErrStore), the compensation's audit insert fails so
// the fallback runs (createTxFaultStore), or every conditional row delete
// gives up (finalizeErr). onFinalize runs before each rollback row delete.
type rollbackFaults struct {
	runIntent    bool
	compensation bool
	finalizeErr  error
	onFinalize   func(t *testing.T, s store.Store, call int, id string)
}

// installRollbackStore installs the wrappers of faults on srv, innermost
// first: runIntentErrStore, createTxFaultStore, rollbackClaimStore. The
// fixtures that call it (setupCreateAgentServer, newSchedFire) write their
// setup through the raw store only and start nothing that reads srv.store,
// so no goroutine reads the field across the install. The switch is armed
// by the caller right before the create.
func installRollbackStore(t *testing.T, srv *Server, s store.Store, faults rollbackFaults) (*rollbackClaimStore, *storeFaultSwitch) {
	t.Helper()
	cs, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *rollbackClaimStore {
		if faults.runIntent {
			inner = runIntentErrStore{inner}
		}
		if faults.compensation {
			inner = &createTxFaultStore{Store: inner, auditErrFor: mutationTypeAgentCreateDispatchFailed}
		}
		return &rollbackClaimStore{Store: inner, fault: f, finalizeErr: faults.finalizeErr}
	})
	if faults.onFinalize != nil {
		cs.onFinalize = func(call int, id string) { faults.onFinalize(t, s, call, id) }
	}
	return cs, fault
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
	// removed: the delete already hard-deleted the row.
	removed bool
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
		{name: "hard deleted", apply: func(t *testing.T, s store.Store, id string) {
			require.NoError(t, s.DeleteAgent(context.Background(), id))
		}, held: true, removed: true},
		{name: "delete failed", apply: claim(store.DeletionStateFailed, time.Minute)},
		{name: "delete lease lapsed", apply: claim(store.DeletionStateDeleting, -time.Minute)},
	}
}

// httpRollbackSite is a non-managed HTTP create failure site covered here.
type httpRollbackSite struct {
	name      string
	runIntent bool
	// own is the site's definition when it is not in createRollbackSites.
	own *createRollbackSite
	// deleteClaim: the failure is itself a delete's claim
	// (store.ErrDeleteInProgress), so the create answers the
	// delete_in_progress refusal whether or not the rollback finds the row
	// held.
	deleteClaim bool
	// wantStatus and wantCode are the failure's own answer, given when no
	// delete holds the row.
	wantStatus int
	wantCode   string
}

// httpRollbackSites are every non-managed HTTP create failure site that
// rolls back through failCreate (ptone/scion#4061), plus a dispatch whose
// failure is a delete's claim.
func httpRollbackSites() []httpRollbackSite {
	return []httpRollbackSite{
		{name: "storage", wantStatus: http.StatusBadGateway, wantCode: ErrCodeRuntimeError},
		{name: "upload URL", wantStatus: http.StatusBadGateway, wantCode: ErrCodeRuntimeError},
		{name: "workspace storage", wantStatus: http.StatusServiceUnavailable, wantCode: ErrCodeUnavailable},
		{name: "unsupported capability", wantStatus: http.StatusPreconditionFailed, wantCode: ErrCodeUnsupportedCapability, own: &createRollbackSite{
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				// A hub-managed project with no git remote on a remote
				// broker, on a hub whose storage is not GCS.
				t.Setenv("HOME", t.TempDir())
				srv.SetStorage(newContentMockStorage("local"))
			},
			wantStage: createStageWorkspaceStorage,
		}},
		{name: "workspace upload", wantStatus: http.StatusBadGateway, wantCode: ErrCodeRuntimeError, own: &createRollbackSite{
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				// The workspace upload runs past its own budget.
				t.Setenv("HOME", t.TempDir())
				prevBudget := hubWorkspaceUploadTimeout
				hubWorkspaceUploadTimeout = 50 * time.Millisecond
				prevSync := syncToGCSForWorkspaceUpload
				syncToGCSForWorkspaceUpload = func(uctx context.Context, _, _, _ string) error {
					select {
					case <-uctx.Done():
						return uctx.Err()
					case <-time.After(10 * time.Second):
						return errProbeNeverDone
					}
				}
				t.Cleanup(func() {
					hubWorkspaceUploadTimeout = prevBudget
					syncToGCSForWorkspaceUpload = prevSync
				})
				srv.SetStorage(newGCSContentMockStorage("test-bucket"))
			},
			wantStage: createStageWorkspaceUpload,
		}},
		{name: "provision-only run intent", runIntent: true, wantStatus: http.StatusInternalServerError, wantCode: ErrCodeInternalError, own: &createRollbackSite{
			disp:      &createAgentDispatcher{},
			req:       CreateAgentRequest{ProvisionOnly: true},
			wantStage: createStageRunIntent,
		}},
		{name: "run intent", runIntent: true, wantStatus: http.StatusInternalServerError, wantCode: ErrCodeInternalError},
		{name: "run intent with env gather", runIntent: true, wantStatus: http.StatusInternalServerError, wantCode: ErrCodeInternalError},
		{name: "dispatch with env gather", wantStatus: http.StatusBadGateway, wantCode: ErrCodeRuntimeError},
		{name: "dispatch", wantStatus: http.StatusBadGateway, wantCode: ErrCodeRuntimeError},
		{name: "missing env", wantStatus: http.StatusUnprocessableEntity, wantCode: ErrCodeMissingEnvVars},
		{name: "provision", wantStatus: http.StatusNotFound, wantCode: skillResolutionErrorCode},
		{name: "provision token", wantStatus: http.StatusInternalServerError, wantCode: ErrCodeInternalError, own: &createRollbackSite{
			disp:      &skillFailDispatcher{provisionErr: fmt.Errorf("provision: %w", errAgentTokenRecord)},
			req:       CreateAgentRequest{ProvisionOnly: true},
			wantStage: createStageProvision,
		}},
		{name: "dispatch delete claim", deleteClaim: true, wantStatus: http.StatusConflict, wantCode: ErrCodeDeleteInProgress, own: &createRollbackSite{
			disp:      &failingCreateDispatcher{createErr: fmt.Errorf("persist run id: %w", store.ErrDeleteInProgress)},
			wantStage: createStageDispatch,
		}},
	}
}

// rollbackSite returns a fresh site definition for site. The store fault
// of a run-intent site is installed by installRollbackStore instead of its
// setup; any other site's setup must not touch srv.store.
func (site httpRollbackSite) rollbackSite(t *testing.T) createRollbackSite {
	t.Helper()
	if site.own != nil {
		own := *site.own
		own.name = site.name
		return own
	}
	for _, s := range createRollbackSites() {
		if s.name == site.name {
			if site.runIntent {
				s.setup = nil
			}
			return s
		}
	}
	t.Fatalf("site %q is not in createRollbackSites", site.name)
	return createRollbackSite{}
}

// httpRollbackRun is the outcome of one failed HTTP create.
type httpRollbackRun struct {
	status        int
	body          string
	agentID       string
	finalizeCalls int
	deleteCalls   int
	s             store.Store
}

// runHTTPRollbackSite runs one create that fails at site, with faults.
func runHTTPRollbackSite(t *testing.T, site httpRollbackSite, faults rollbackFaults) httpRollbackRun {
	t.Helper()
	rs := site.rollbackSite(t)
	srv, s, project := setupCreateAgentServer(t, rs.disp)
	if rs.setup != nil {
		rs.setup(t, srv)
	}
	setAgentQuotaLimits(t, s)
	faults.runIntent = site.runIntent
	cs, fault := installRollbackStore(t, srv, s, faults)
	fault.Arm()

	req := rs.req
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
	}
}

// assertRowLeftToDelete checks a held row was not touched by the rollback:
// it keeps its deletion state (or stays removed), its phase is not marked
// failed, its edge stays active, no compensation was recorded, and, when
// quotas is set, both quota reservations are still held.
func assertRowLeftToDelete(t *testing.T, s store.Store, agentID string, del rollbackDelete, quotas bool) {
	t.Helper()
	if del.removed {
		assert.True(t, agentGone(t, s, agentID), "the delete removed the row")
	} else {
		row, err := s.GetAgent(context.Background(), agentID)
		require.NoError(t, err, "the delete's row is kept")
		assert.Equal(t, del.rowState, row.DeletionState, "the delete's state is kept")
		assert.Equal(t, del.softDeleted, !row.DeletedAt.IsZero(), "the row's soft delete is the delete's own")
		assert.NotEqual(t, string(state.PhaseError), row.Phase, "no phase-error write")
		assert.NotEqual(t, createRowRemoveFailedMessage, row.Message)
		assert.Len(t, activeEdgesFor(t, s, agentID), 1, "the edge is left to the delete")
	}
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
	if quotas {
		held := observeReservations(t, s, agentID)
		assert.True(t, held.broker, "the per-broker reservation is left to the delete")
		assert.True(t, held.project, "the per-project reservation is left to the delete")
	}
}

// assertQuotasReleased checks both quota reservations of agentID are gone.
func assertQuotasReleased(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	held := observeReservations(t, s, agentID)
	assert.False(t, held.broker, "the per-broker reservation is released")
	assert.False(t, held.project, "the per-project reservation is released")
}

// assertRolledBack checks the create was rolled back at stage and, when
// quotas is set, its quota reservations released.
func assertRolledBack(t *testing.T, s store.Store, agentID, stage string, quotas bool) {
	t.Helper()
	assert.True(t, agentGone(t, s, agentID), "the agent row is rolled back")
	sum := assertCompensated(t, s, agentID)
	assert.Equal(t, stage, sum.Stage)
	if quotas {
		assertQuotasReleased(t, s, agentID)
	}
}

// (A) HTTP create, non-managed failure sites: a delete that holds the row
// when the rollback runs keeps it, and the create answers 409
// delete_in_progress with details.agentId (ptone/scion#4061); a failure that
// is itself a delete's claim keeps its own delete_in_progress answer. A
// failed or lapsed delete does not hold the row: the create is rolled back
// and answers as the same failure with no delete does.
func TestFix3958_HTTPCreateRollback_DeferToHeldRow(t *testing.T) {
	for _, site := range httpRollbackSites() {
		t.Run(site.name, func(t *testing.T) {
			stage := site.rollbackSite(t).wantStage
			plain := runHTTPRollbackSite(t, site, rollbackFaults{})
			require.GreaterOrEqual(t, plain.status, 400, plain.body)
			require.NotContains(t, plain.body, "correlation_id", "the plain rollback completed")
			assertRolledBack(t, plain.s, plain.agentID, stage, true)
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")
			assert.Equal(t, 1, plain.finalizeCalls, "one conditional compensation")
			requireSiteAnswer(t, site, plain)

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runHTTPRollbackSite(t, site, rollbackFaults{onFinalize: func(t *testing.T, s store.Store, call int, id string) {
						if call == 1 {
							del.apply(t, s, id)
						}
					}})
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						requireHeldRowAnswer(t, site, run)
						assertRowLeftToDelete(t, run.s, run.agentID, del, true)
						assert.Equal(t, 1, run.finalizeCalls, "the refused compensation, and no fallback")
						return
					}
					assert.Equal(t, plain.status, run.status, "the answer's status is unchanged")
					assert.Equal(t, plain.body, run.body, "the answer's body is unchanged")
					assertRolledBack(t, run.s, run.agentID, stage, true)
				})
			}
		})
	}
}

// (A) The fallback row delete, after a failed compensation, is conditional
// too: a delete that holds the row before it keeps the row, and the create
// answers 409 delete_in_progress with details.agentId and no correlation ID
// (ptone/scion#4061). When the fallback removes the row, the answer is the
// 500 with a correlation ID.
func TestFix3958_HTTPCreateRollback_FallbackDefersToHeldRow(t *testing.T) {
	for _, site := range httpRollbackSites() {
		t.Run(site.name, func(t *testing.T) {
			plain := runHTTPRollbackSite(t, site, rollbackFaults{compensation: true})
			requireRollbackIncomplete500(t, plain)
			assert.True(t, agentGone(t, plain.s, plain.agentID), "the fallback removes the row")
			assertQuotasReleased(t, plain.s, plain.agentID)
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")
			assert.Equal(t, 2, plain.finalizeCalls, "the failed compensation, then one fallback delete")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runHTTPRollbackSite(t, site, rollbackFaults{compensation: true, onFinalize: func(t *testing.T, s store.Store, call int, id string) {
						if call == 2 {
							del.apply(t, s, id)
						}
					}})
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						requireHeldRowAnswer(t, site, run)
						assertRowLeftToDelete(t, run.s, run.agentID, del, true)
						assert.Equal(t, 2, run.finalizeCalls, "the failed compensation, then one refused fallback delete")
						return
					}
					requireRollbackIncomplete500(t, run)
					assert.Equal(t, normalizeCorrelationID(plain.body), normalizeCorrelationID(run.body), "the answer is unchanged")
					assert.True(t, agentGone(t, run.s, run.agentID), "the fallback removes the row")
					assertQuotasReleased(t, run.s, run.agentID)
				})
			}
		})
	}
}

// requireSiteAnswer checks run answered the site's own failure:
// site.wantStatus and site.wantCode, and the delete_in_progress refusal's
// body when the failure was the delete's claim.
func requireSiteAnswer(t *testing.T, site httpRollbackSite, run httpRollbackRun) {
	t.Helper()
	require.Equal(t, site.wantStatus, run.status, run.body)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(run.body), &body))
	assert.Equal(t, site.wantCode, body.Error.Code)
	if site.deleteClaim {
		requireDeleteInProgressRefusal(t, run)
	}
}

// requireHeldRowAnswer checks the answer of a create whose rollback left
// the row to a delete: 409 delete_in_progress with details.agentId and no
// correlation ID; the deleted-during-create message, or the
// delete_in_progress refusal's when the failure was the delete's claim.
func requireHeldRowAnswer(t *testing.T, site httpRollbackSite, run httpRollbackRun) {
	t.Helper()
	if site.deleteClaim {
		requireDeleteInProgressRefusal(t, run)
		return
	}
	body := requireDeleteInProgress409(t, run)
	assert.Equal(t, deletedDuringCreateMessage, body.Error.Message)
}

// requireDeleteInProgressRefusal checks run answered the
// delete_in_progress refusal (deleteInProgressRefusal).
func requireDeleteInProgressRefusal(t *testing.T, run httpRollbackRun) {
	t.Helper()
	body := requireDeleteInProgress409(t, run)
	assert.Equal(t, deleteInProgressRefusal(run.agentID).Message, body.Error.Message)
}

// requireDeleteInProgress409 checks run answered 409 delete_in_progress
// with details.agentId naming the created agent and no correlation ID.
func requireDeleteInProgress409(t *testing.T, run httpRollbackRun) ErrorResponse {
	t.Helper()
	require.Equal(t, http.StatusConflict, run.status, run.body)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(run.body, "<agent-id>", run.agentID)), &body))
	assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
	assert.Equal(t, run.agentID, body.Error.Details["agentId"], "details.agentId")
	assert.NotContains(t, body.Error.Details, "correlation_id", "no correlation ID")
	assert.Len(t, body.Error.Details, 1, "details carry only agentId")
	return body
}

// (A) At a failCreate site (dispatch), when every
// conditional row delete gives up because the row keeps changing
// (store.ErrVersionConflict), the row is left to whatever is writing it:
// it is kept, its phase is not marked failed, no compensation is recorded
// and its quotas stay held. The answer is the 500 with a correlation ID, as
// for any incomplete rollback.
func TestFix3958_HTTPCreateRollback_RowContendedLeavesRow(t *testing.T) {
	site := httpRollbackSite{name: "dispatch"}
	run := runHTTPRollbackSite(t, site, rollbackFaults{finalizeErr: fmt.Errorf("finalize agent deletion: %w", store.ErrVersionConflict)})
	requireRollbackIncomplete500(t, run)

	row, err := run.s.GetAgent(context.Background(), run.agentID)
	require.NoError(t, err, "the row is kept")
	assert.NotEqual(t, string(state.PhaseError), row.Phase, "no phase-error write")
	assert.NotEqual(t, createRowRemoveFailedMessage, row.Message)
	assert.Empty(t, agentAudits(t, run.s, mutationTypeAgentCreateDispatchFailed, run.agentID), "no compensation was written")
	held := observeReservations(t, run.s, run.agentID)
	assert.True(t, held.broker, "the per-broker reservation stays held")
	assert.True(t, held.project, "the per-project reservation stays held")
	assert.Zero(t, run.deleteCalls, "no unconditional row delete")
	assert.Equal(t, 1+createCleanupDeleteAttempts, run.finalizeCalls,
		"the compensation, then the fallback's conditional deletes")
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
	name      string
	stage     string
	runIntent bool
	// createErr is the dispatch's error; "broker unavailable" when nil.
	createErr error
	// deleteClaim: the failure is itself a delete's claim
	// (store.ErrDeleteInProgress), so the fire keeps its own error whether
	// or not the rollback finds the row held.
	deleteClaim bool
}

func schedRollbackStages() []schedRollbackStage {
	return []schedRollbackStage{
		{name: "run intent", stage: createStageRunIntent, runIntent: true},
		{name: "dispatch", stage: createStageDispatch},
		{name: "dispatch delete claim", stage: createStageDispatch, deleteClaim: true,
			createErr: fmt.Errorf("persist run id: %w", store.ErrDeleteInProgress)},
	}
}

// schedDeletedDuringCreateErr is the error text of a fire whose rollback
// left the row to a delete (errScheduledChildDeletedDuringCreate).
const schedDeletedDuringCreateErr = `scheduled dispatch of agent "s3958-child": agent was deleted while it was being created`

// assertSchedHeldRowErr checks the error of a fire whose rollback left the
// row to a delete: errScheduledChildDeletedDuringCreate, or the plain error
// when the failure was the delete's claim.
func assertSchedHeldRowErr(t *testing.T, st schedRollbackStage, plain, run schedRollbackRun) {
	t.Helper()
	if st.deleteClaim {
		assert.Equal(t, plain.errText, run.errText, "the event error is unchanged")
		assert.NotErrorIs(t, run.err, errScheduledChildDeletedDuringCreate)
		return
	}
	assert.ErrorIs(t, run.err, errScheduledChildDeletedDuringCreate)
	assert.Equal(t, schedDeletedDuringCreateErr, run.errText, "no correlation ID in the event error")
}

// schedRollbackRun is the outcome of one failed scheduled fire.
type schedRollbackRun struct {
	err           error
	errText       string
	agentID       string
	finalizeCalls int
	deleteCalls   int
	s             store.Store
}

// runSchedRollback fires one dispatch_agent event that fails at st.
func runSchedRollback(t *testing.T, st schedRollbackStage, faults rollbackFaults) schedRollbackRun {
	t.Helper()
	f := newSchedFire(t, "s3958-"+tidSlugSafe(st.name))
	createErr := st.createErr
	if createErr == nil {
		createErr = errors.New("broker unavailable")
	}
	f.srv.SetDispatcher(&schedFailingDispatcher{failingCreateDispatcher{createErr: createErr}})
	faults.runIntent = st.runIntent
	cs, fault := installRollbackStore(t, f.srv, f.store, faults)
	fault.Arm()

	slug := "s3958-child"
	err := f.fire(t, withSessionRevision(f.event(slug), f.creator.ID))
	require.Error(t, err, "the fire fails")
	agentID, finalizeCalls, deleteCalls := cs.snapshot()
	require.NotEmpty(t, agentID, "the rollback ran: %v", err)
	return schedRollbackRun{
		err:           err,
		errText:       normalizeCorrelationID(strings.ReplaceAll(err.Error(), agentID, "<agent-id>")),
		agentID:       agentID,
		finalizeCalls: finalizeCalls,
		deleteCalls:   deleteCalls,
		s:             f.store,
	}
}

// (B) The scheduler's dispatch_agent rollback: a delete that holds the row
// keeps it, and the fire fails with errScheduledChildDeletedDuringCreate
// (ptone/scion#4061), unless the failure was the delete's claim; a failed
// or lapsed delete does not hold it, the create is rolled back and the
// fire fails with the same error text as the same failure with no delete.
// The scheduled create takes no quota reservation, so there is none to hold
// or release.
func TestFix3958_SchedDispatchRollback_DeferToHeldRow(t *testing.T) {
	for _, st := range schedRollbackStages() {
		t.Run(st.name, func(t *testing.T) {
			plain := runSchedRollback(t, st, rollbackFaults{})
			assert.True(t, strings.HasPrefix(plain.errText, `failed to dispatch agent "s3958-child": `), plain.errText)
			assert.NotContains(t, plain.errText, "rollback incomplete")
			assertRolledBack(t, plain.s, plain.agentID, st.stage, false)
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")
			assert.Equal(t, 1, plain.finalizeCalls, "one conditional compensation")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runSchedRollback(t, st, rollbackFaults{onFinalize: func(t *testing.T, s store.Store, call int, id string) {
						if call == 1 {
							del.apply(t, s, id)
						}
					}})
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						assertSchedHeldRowErr(t, st, plain, run)
						assertRowLeftToDelete(t, run.s, run.agentID, del, false)
						assert.Equal(t, 1, run.finalizeCalls, "the refused compensation, and no fallback")
						return
					}
					assert.Equal(t, plain.errText, run.errText, "the event error is unchanged")
					assertRolledBack(t, run.s, run.agentID, st.stage, false)
				})
			}
		})
	}
}

// (B) The scheduler rollback's fallback row delete is conditional too: a
// delete that holds the row before it keeps the row, and the fire fails
// with errScheduledChildDeletedDuringCreate and no correlation ID, unless
// the failure was the delete's claim (ptone/scion#4061).
func TestFix3958_SchedDispatchRollback_FallbackDefersToHeldRow(t *testing.T) {
	for _, st := range schedRollbackStages() {
		t.Run(st.name, func(t *testing.T) {
			plain := runSchedRollback(t, st, rollbackFaults{compensation: true})
			assert.Contains(t, plain.errText, "(rollback incomplete, correlation ID <correlation-id>)")
			assert.True(t, agentGone(t, plain.s, plain.agentID), "the fallback removes the row")
			assert.Zero(t, plain.deleteCalls, "no unconditional row delete")
			assert.Equal(t, 2, plain.finalizeCalls, "the failed compensation, then one fallback delete")

			for _, del := range rollbackDeletes() {
				t.Run(del.name, func(t *testing.T) {
					run := runSchedRollback(t, st, rollbackFaults{compensation: true, onFinalize: func(t *testing.T, s store.Store, call int, id string) {
						if call == 2 {
							del.apply(t, s, id)
						}
					}})
					assert.Zero(t, run.deleteCalls, "no unconditional row delete")
					if del.held {
						assertSchedHeldRowErr(t, st, plain, run)
						assertRowLeftToDelete(t, run.s, run.agentID, del, false)
						assert.Equal(t, 2, run.finalizeCalls, "the failed compensation, then one refused fallback delete")
						return
					}
					assert.Equal(t, plain.errText, run.errText, "the event error is unchanged")
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
				Cause:           errors.New("dispatch failed"),
				DeleteWon:       &deleteWon,
			})
			assert.Empty(t, corrID)
			assert.Equal(t, del.held, deleteWon, "DeleteWon reports whether a delete owns the row")
			assert.Equal(t, !del.held || del.removed, agentGone(t, s, agent.ID), "only a row no delete holds is removed")
		})
	}
}
