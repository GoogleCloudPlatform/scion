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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3557: a managed (hub-direct) create whose post-create write
// (the managed Runtime and the interaction ID) fails no longer answers 201.
// It stops the interaction, rolls back the row, its edge and its quotas, and
// answers 500 with the stop's outcome in details.warnings. A delete that
// holds the row when the rollback runs keeps it, and the create answers 409
// as on the ptone/scion#3454 path.

// interactionLedgerBackend is a managed-agent backend that tracks each
// interaction's status: CreateInteraction starts one in progress, and a
// successful CancelInteraction moves it to cancelled.
type interactionLedgerBackend struct {
	failingManagedAgentBackend

	mu        sync.Mutex
	next      int
	status    map[string]managedagent.InteractionStatus
	cancelled []string
	cancelErr error
	getErr    error
}

func newInteractionLedgerBackend() *interactionLedgerBackend {
	return &interactionLedgerBackend{status: map[string]managedagent.InteractionStatus{}}
}

func (b *interactionLedgerBackend) CreateInteraction(context.Context, managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := fmt.Sprintf("interaction-%d", b.next)
	b.status[id] = managedagent.StatusInProgress
	return &managedagent.InteractionHandle{InteractionID: id}, nil
}

func (b *interactionLedgerBackend) GetInteraction(_ context.Context, id string) (*managedagent.InteractionState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.getErr != nil {
		return nil, b.getErr
	}
	st, ok := b.status[id]
	if !ok {
		return nil, fmt.Errorf("no interaction %s", id)
	}
	return &managedagent.InteractionState{InteractionID: id, Status: st}, nil
}

func (b *interactionLedgerBackend) CancelInteraction(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, id)
	if b.cancelErr != nil {
		return b.cancelErr
	}
	b.status[id] = managedagent.StatusCancelled
	return nil
}

func (b *interactionLedgerBackend) cancels() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cancelled...)
}

// inProgress returns the interactions still in progress.
func (b *interactionLedgerBackend) inProgress() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for id, st := range b.status {
		if st == managedagent.StatusInProgress {
			out = append(out, id)
		}
	}
	return out
}

var errManagedRecordWrite = errors.New("simulated store outage")

// managedRecordFaultStore fails the managed create's post-create write (the
// whole-row UpdateAgent to phase running) failures times. beforeFail, when
// set, runs just before the failure (a delete that claims the row first).
// beforeFinalize, when set, runs once before the first FinalizeAgentDeletion
// (a delete that claims the row after the write failed, right before the
// rollback's transaction).
type managedRecordFaultStore struct {
	store.Store
	mu             sync.Mutex
	failures       int
	beforeFail     func(agentID string)
	beforeFinalize func(agentID string)
	// finalizeErr, when set, is returned by FinalizeAgentDeletion from
	// call finalizeErrFrom on (every call when finalizeErrFrom is 0).
	finalizeErr     error
	finalizeErrFrom int
	// finalizeCalls counts FinalizeAgentDeletion calls.
	finalizeCalls int
	// onFinalizeCall, when set, runs before every FinalizeAgentDeletion
	// with its 1-based call index.
	onFinalizeCall func(call int, agentID string)
}

func (s *managedRecordFaultStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	s.mu.Lock()
	fail := a.Phase == string(state.PhaseRunning) && s.failures > 0
	if fail {
		s.failures--
	}
	before := s.beforeFail
	s.mu.Unlock()
	if fail {
		if before != nil {
			before(a.ID)
		}
		return errManagedRecordWrite
	}
	return s.Store.UpdateAgent(ctx, a)
}

func (s *managedRecordFaultStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	s.mu.Lock()
	before := s.beforeFinalize
	s.beforeFinalize = nil
	s.finalizeCalls++
	call := s.finalizeCalls
	onCall := s.onFinalizeCall
	failNow := s.finalizeErr != nil && call >= s.finalizeErrFrom
	s.mu.Unlock()
	if before != nil {
		before(id)
	}
	if onCall != nil {
		onCall(call, id)
	}
	if failNow {
		return 0, s.finalizeErr
	}
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, hook)
}

// managedCreate posts a managed create named name with a task.
func managedCreate(t *testing.T, srv *Server, projectID, name string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: name, ProjectID: projectID, Task: "do it", Profile: ManagedAgentsProfile,
	})
}

// requireManagedCreateUnrecorded checks rec is the 500 of a rolled-back
// unrecorded managed create and returns the agent ID and the warnings.
func requireManagedCreateUnrecorded(t *testing.T, rec *httptest.ResponseRecorder) (string, []string) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "agent", "the 500 carries no agent body")
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeInternalError, body.Error.Code)
	assert.Equal(t, managedCreateUnrecordedMessage, body.Error.Message)
	assert.NotContains(t, body.Error.Details, "correlation_id", "the rollback completed")
	agentID, _ := body.Error.Details["agentId"].(string)
	require.NotEmpty(t, agentID)
	var warnings []string
	if ws, ok := body.Error.Details["warnings"].([]interface{}); ok {
		for _, w := range ws {
			warnings = append(warnings, w.(string))
		}
	}
	return agentID, warnings
}

// assertManagedCreateRolledBack checks agentID's create was compensated at
// the managed_record stage and its quota reservations released.
func assertManagedCreateRolledBack(t *testing.T, s store.Store, project *store.Project, agentID string) {
	t.Helper()
	sum := assertCompensated(t, s, agentID)
	assert.Equal(t, createStageManagedRecord, sum.Stage)
	assert.Contains(t, sum.Error, errManagedRecordWrite.Error())
	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
		"the per-broker reservation is released")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	n, err := s.CountActiveReservations(context.Background(), def.ID, DevUserID, store.QuotaScopeProject, project.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n, "the per-project reservation is released")
}

// The write fails: 500, the interaction is stopped exactly once and nothing
// is left in progress, and the row, edge and quotas are rolled back.
func TestManagedCreate_Unrecorded_Answers500AndRollsBack(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	pub := recordCreatedEvents(t, srv)
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	srv.store = &managedRecordFaultStore{Store: s, failures: 1}

	rec := managedCreate(t, srv, project.ID, "mgd-unrec")
	agentID, warnings := requireManagedCreateUnrecorded(t, rec)

	assert.Equal(t, []string{managedCreateUnrecordedStoppedWarning + " (agent " + agentID + ", interaction interaction-1)"}, warnings)
	assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "exactly one cancel")
	assert.Empty(t, backend.inProgress(), "no interaction is left in progress")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
	assertManagedCreateRolledBack(t, s, project, agentID)
}

// The stop fails (cancel error, or a failed read): the 500 still carries
// the failure, naming the agent and the interaction so an operator can stop
// it by hand, and the rollback still runs.
func TestManagedCreate_Unrecorded_StopFails_Warns(t *testing.T) {
	cases := []struct {
		name       string
		cancelErr  error
		getErr     error
		wantCancel bool
	}{
		{name: "cancel fails", cancelErr: errors.New("backend unavailable"), wantCancel: true},
		{name: "read fails", getErr: errors.New("backend unavailable")},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			backend := newInteractionLedgerBackend()
			backend.cancelErr = tc.cancelErr
			backend.getErr = tc.getErr
			useManagedBackend(t, backend)
			srv.store = &managedRecordFaultStore{Store: s, failures: 1}

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-unrec-stopfail-%d", i))
			agentID, warnings := requireManagedCreateUnrecorded(t, rec)

			require.Len(t, warnings, 1)
			assert.True(t, strings.HasPrefix(warnings[0], managedCreateUnrecordedStopFailedWarning), "got %q", warnings[0])
			assert.Contains(t, warnings[0], "backend unavailable")
			assert.Contains(t, warnings[0], agentID, "the warning names the agent")
			assert.Contains(t, warnings[0], "interaction-1", "the warning names the interaction")
			if tc.wantCancel {
				assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "the cancel was tried once")
			} else {
				assert.Empty(t, backend.cancels(), "no cancel without knowing the state")
			}
			assertManagedCreateRolledBack(t, s, project, agentID)
		})
	}
}

// After the failed create, nothing blocks the name: a delete of the failed
// agent finds nothing, a recreate with the same name answers 201, and a
// delete of the recreated agent stops its interaction.
func TestManagedCreate_Unrecorded_ThenDeleteAndRecreate(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	srv.store = &managedRecordFaultStore{Store: s, failures: 1}

	failedID, _ := requireManagedCreateUnrecorded(t, managedCreate(t, srv, project.ID, "mgd-again"))

	del := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+failedID, nil)
	assert.Equal(t, http.StatusNotFound, del.Code, "no orphan row to delete: %s", del.Body.String())

	rec := managedCreate(t, srv, project.ID, "mgd-again")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.NotEqual(t, failedID, resp.Agent.ID)
	assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "the recreate cancels nothing")

	del = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+resp.Agent.ID, nil)
	require.Contains(t, []int{http.StatusNoContent, http.StatusAccepted}, del.Code, del.Body.String())
	require.Eventually(t, func() bool { return agentGone(t, s, resp.Agent.ID) }, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"interaction-1", "interaction-2"}, backend.cancels(),
		"the delete stops the recreated agent's interaction")
	assert.Empty(t, backend.inProgress())
}

// Regression: the write succeeds, so the create answers 201 and cancels
// nothing.
func TestManagedCreate_Recorded_Answers201WithoutCancel(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	srv.store = &managedRecordFaultStore{Store: s}

	rec := managedCreate(t, srv, project.ID, "mgd-ok")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Empty(t, backend.cancels())
	assert.Equal(t, []string{"interaction-1"}, backend.inProgress())

	row, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted")
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
}

// A delete that holds the row when the rollback runs wins: the rollback
// leaves the row, its edge and its quotas to the delete, and the create
// answers 409 as on the ptone/scion#3454 path, having stopped the
// interaction no row names. Two timings: the claim lands before the write
// (so the write fails, as a claim's state_version bump makes it), or after
// the write failed and right before the rollback's transaction.
func TestManagedCreate_Unrecorded_DeleteHoldsRow_Answers409(t *testing.T) {
	timings := []string{"claim before write", "claim before rollback tx"}
	for i, timing := range timings {
		t.Run(timing, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			pub := recordCreatedEvents(t, srv)
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			var agentID string
			claim := func(id string) {
				agentID = id
				claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
			}
			fs := &managedRecordFaultStore{Store: s, failures: 1}
			if i == 0 {
				fs.beforeFail = claim
			} else {
				fs.beforeFinalize = claim
			}
			srv.store = fs

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-held-%d", i))
			require.NotEmpty(t, agentID, "the claim ran: %d %s", rec.Code, rec.Body.String())
			warnings := requireDeletedDuringCreate(t, rec, agentID)
			assert.Equal(t, []string{managedCreateCompensatedWarning}, warnings)
			assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "the create stops the interaction, once")
			assert.Empty(t, backend.inProgress())
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())

			row, err := s.GetAgent(context.Background(), agentID)
			require.NoError(t, err, "the delete's row is kept")
			assert.Equal(t, store.DeletionStateDeleting, row.DeletionState)
			assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
			assert.EqualValues(t, 1, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
				"the quotas are left to the delete")
		})
	}
}

// A delete that holds the row (claimed, finalizing) or removed it (hard or
// soft delete) wins: 409, and nothing of the delete's outcome is undone.
func TestManagedCreate_Unrecorded_DeleteHoldsOrRemovedRow_Answers409(t *testing.T) {
	for i, del := range landingDeletes {
		if !del.compensate {
			continue
		}
		t.Run(del.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			var agentID string
			srv.store = &managedRecordFaultStore{Store: s, failures: 1, beforeFail: func(id string) {
				agentID = id
				del.apply(t, s, id)
			}}

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-gone-%d", i))
			require.NotEmpty(t, agentID, "the delete ran: %d %s", rec.Code, rec.Body.String())
			assert.Equal(t, []string{managedCreateCompensatedWarning}, requireDeletedDuringCreate(t, rec, agentID))
			assert.Equal(t, []string{"interaction-1"}, backend.cancels())
			assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
		})
	}
}

// A delete that gave up does not hold the row (design ptone/scion#2483
// §2.1): a failed delete, or a deleting row whose lease lapsed (no engine
// owns it), leaves the agent live. The unrecorded create is rolled back and
// answers 500, rather than leaving a live row whose interaction it stopped.
func TestManagedCreate_Unrecorded_DeleteGaveUp_RollsBack(t *testing.T) {
	cases := []struct {
		name  string
		state string
		lease time.Duration
	}{
		{"delete-failed", store.DeletionStateFailed, time.Minute},
		{"delete-lapsed", store.DeletionStateDeleting, -time.Minute},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			srv.store = &managedRecordFaultStore{Store: s, failures: 1, beforeFail: func(id string) {
				claimForTest(t, s, id, tc.state, tc.lease)
			}}

			agentID, warnings := requireManagedCreateUnrecorded(t, managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-gaveup-%d", i)))
			assert.Len(t, warnings, 1)
			assert.Equal(t, []string{"interaction-1"}, backend.cancels())
			assertManagedCreateRolledBack(t, s, project, agentID)
		})
	}
}

// A create with no task started no interaction: the unrecorded create is
// still rolled back, with nothing to stop and no warning.
func TestManagedCreate_Unrecorded_NoTask_RollsBackWithoutWarning(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	srv.store = &managedRecordFaultStore{Store: s, failures: 1}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "mgd-unrec-notask", ProjectID: project.ID, Profile: ManagedAgentsProfile,
	})
	agentID, warnings := requireManagedCreateUnrecorded(t, rec)
	assert.Empty(t, warnings)
	assert.Empty(t, backend.cancels())
	assertManagedCreateRolledBack(t, s, project, agentID)
}

// hookFailingManagedBackend fails CreateInteraction after running hook with
// the create's row ID: a delete that lands while the interaction is being
// created, before managedAgentCreate's failure rolls the create back.
type hookFailingManagedBackend struct {
	failingManagedAgentBackend
	s         store.Store
	projectID string
	hook      func(agentID string)
}

func (b *hookFailingManagedBackend) CreateInteraction(ctx context.Context, req managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	result, err := b.s.ListAgents(context.Background(), store.AgentFilter{ProjectID: b.projectID}, store.ListOptions{})
	if err == nil && len(result.Items) == 1 {
		b.hook(result.Items[0].ID)
	}
	return b.failingManagedAgentBackend.CreateInteraction(ctx, req)
}

// The managedAgentCreate-failure rollback opts into the same conditional
// row removal: a delete that holds the row wins (409, row and quotas left
// to it); a delete that gave up does not, and the create is rolled back
// with its usual 502.
func TestManagedCreate_CreateFails_DeleteHoldsRow(t *testing.T) {
	cases := []struct {
		name     string
		state    string
		lease    time.Duration
		deleteOK bool
	}{
		{"delete-claimed", store.DeletionStateDeleting, time.Minute, true},
		{"delete-failed", store.DeletionStateFailed, time.Minute, false},
		{"delete-lapsed", store.DeletionStateDeleting, -time.Minute, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			var agentID string
			useManagedBackend(t, &hookFailingManagedBackend{s: s, projectID: project.ID, hook: func(id string) {
				agentID = id
				claimForTest(t, s, id, tc.state, tc.lease)
			}})

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-createfail-%d", i))
			require.NotEmpty(t, agentID, "the claim ran: %d %s", rec.Code, rec.Body.String())
			if tc.deleteOK {
				assert.Empty(t, requireDeletedDuringCreate(t, rec, agentID))
				row, err := s.GetAgent(context.Background(), agentID)
				require.NoError(t, err, "the delete's row is kept")
				assert.Equal(t, tc.state, row.DeletionState)
				assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
				assert.EqualValues(t, 1, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID), "the quotas are left to the delete")
				return
			}
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			sum := assertCompensated(t, s, agentID)
			assert.Equal(t, createStageManaged, sum.Stage)
			assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID))
		})
	}
}

// requireManagedCreateRollbackIncomplete checks rec is the 500 of an
// unrecorded managed create whose rollback did not complete, and returns
// the agent ID and the warnings.
func requireManagedCreateRollbackIncomplete(t *testing.T, rec *httptest.ResponseRecorder) (string, []string) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeInternalError, body.Error.Code)
	assert.Contains(t, body.Error.Message, "did not complete")
	assert.NotEmpty(t, body.Error.Details["correlation_id"], "the correlation ID is reported")
	agentID, _ := body.Error.Details["agentId"].(string)
	require.NotEmpty(t, agentID)
	var warnings []string
	if ws, ok := body.Error.Details["warnings"].([]interface{}); ok {
		for _, w := range ws {
			warnings = append(warnings, w.(string))
		}
	}
	return agentID, warnings
}

// The rollback's compensation transaction fails (its audit insert): the
// fallback still removes the row, and the 500 says the rollback did not
// complete, with the correlation ID and the stop's warning.
func TestManagedCreate_Unrecorded_CompensationFails_ReportsCorrelationID(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	srv.store = &managedRecordFaultStore{
		Store:    &createTxFaultStore{Store: s, auditErrFor: mutationTypeAgentCreateDispatchFailed},
		failures: 1,
	}

	agentID, warnings := requireManagedCreateRollbackIncomplete(t, managedCreate(t, srv, project.ID, "mgd-unrec-compfail"))
	assert.Equal(t, []string{managedCreateUnrecordedStoppedWarning + " (agent " + agentID + ", interaction interaction-1)"}, warnings)
	assert.Equal(t, []string{"interaction-1"}, backend.cancels())
	assert.True(t, agentGone(t, s, agentID), "the fallback removed the row")
}

// Every conditional row delete gives up because the row kept changing
// (ErrVersionConflict): the compensation fails, the fallback's conditional
// deletes (createCleanupDeleteAttempts attempts) fail the same way, and the rollback then leaves the
// row to whatever is writing it. The row is kept, its phase is not marked
// error, its quotas are held, and the 500 reports the correlation ID.
func TestManagedCreate_Unrecorded_RowContended_LeavesRow(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedRecordFaultStore{Store: s, failures: 1,
		finalizeErr: fmt.Errorf("finalize agent deletion: %w", store.ErrVersionConflict)}
	srv.store = fs

	agentID, warnings := requireManagedCreateRollbackIncomplete(t, managedCreate(t, srv, project.ID, "mgd-unrec-contended"))
	require.Len(t, warnings, 1)
	assert.Equal(t, managedCreateUnrecordedStoppedWarning+" (agent "+agentID+", interaction interaction-1)", warnings[0])
	assert.Equal(t, []string{"interaction-1"}, backend.cancels())

	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err, "the row is kept")
	assert.NotEqual(t, string(state.PhaseError), row.Phase, "no phase-error write")
	assert.NotEqual(t, createRowRemoveFailedMessage, row.Message)
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID), "no quota release")
	fs.mu.Lock()
	defer fs.mu.Unlock()
	assert.Equal(t, 1+createCleanupDeleteAttempts, fs.finalizeCalls,
		"the compensation, then the fallback's conditional deletes")
}

// The compensation fails for another reason (its audit insert), and the
// fallback's conditional row deletes then give up with ErrVersionConflict:
// the same guard leaves the row to whatever is writing it. 500 with the
// correlation ID, row kept, phase not error, quotas held.
func TestManagedCreate_Unrecorded_CompensationFails_ThenContended_LeavesRow(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedRecordFaultStore{
		Store:           &createTxFaultStore{Store: s, auditErrFor: mutationTypeAgentCreateDispatchFailed},
		failures:        1,
		finalizeErr:     fmt.Errorf("finalize agent deletion: %w", store.ErrVersionConflict),
		finalizeErrFrom: 2,
	}
	srv.store = fs

	agentID, warnings := requireManagedCreateRollbackIncomplete(t, managedCreate(t, srv, project.ID, "mgd-unrec-compfail-contended"))
	require.Len(t, warnings, 1)
	assert.Equal(t, managedCreateUnrecordedStoppedWarning+" (agent "+agentID+", interaction interaction-1)", warnings[0])

	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err, "the row is kept")
	assert.NotEqual(t, string(state.PhaseError), row.Phase, "no phase-error write")
	assert.NotEqual(t, createRowRemoveFailedMessage, row.Message)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID), "no quota release")
	fs.mu.Lock()
	defer fs.mu.Unlock()
	assert.Equal(t, 1+createCleanupDeleteAttempts, fs.finalizeCalls,
		"the failed compensation, then the fallback's conditional deletes")
}

// The compensation fails (its audit insert), and a live delete claims the
// row before the fallback's first conditional row delete: that delete
// refuses the held row (createRowHeldCheck), so the create answers 409
// and leaves the row and its quotas to the delete.
func TestManagedCreate_Unrecorded_CompensationFails_ThenHeld_Answers409(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	var agentID string
	fs := &managedRecordFaultStore{
		Store:    &createTxFaultStore{Store: s, auditErrFor: mutationTypeAgentCreateDispatchFailed},
		failures: 1,
		onFinalizeCall: func(call int, id string) {
			if call == 2 {
				agentID = id
				claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
			}
		},
	}
	srv.store = fs

	rec := managedCreate(t, srv, project.ID, "mgd-unrec-compfail-held")
	require.NotEmpty(t, agentID, "the claim ran: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, []string{managedCreateCompensatedWarning}, requireDeletedDuringCreate(t, rec, agentID))
	assert.Equal(t, []string{"interaction-1"}, backend.cancels())

	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err, "the delete's row is kept")
	assert.Equal(t, store.DeletionStateDeleting, row.DeletionState)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID), "the quotas are left to the delete")
	fs.mu.Lock()
	defer fs.mu.Unlock()
	assert.Equal(t, 2, fs.finalizeCalls, "the failed compensation, then one refused fallback delete")
}
