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
	"fmt"
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

// ptone/scion#3746: the managed create's post-create write (the managed
// Runtime, the interaction ID, phase running) re-reads the row once on
// store.ErrVersionConflict. A row a delete won (deleteWonOnRead) goes to the
// ptone/scion#3557 rollback unchanged; a live row gets the create's fields
// re-applied and one more write, and the create answers 201. Any other first
// error, and any error of the retry, takes the rollback as before.

// managedConflictStore wraps the managed create's post-create write: the
// first UpdateAgent to phase running and the one write after it.
//
//   - beforeFirst runs just before the first running write, which then goes
//     to the real store: a hook that bumps state_version (a delete claim, a
//     status write) makes it a real ErrVersionConflict.
//   - firstErr, when set, is returned by the first running write instead.
//   - beforeReread runs once before the first GetAgent after the first
//     running write failed.
//   - beforeSecond runs just before the next UpdateAgent; secondErr, when
//     set, is returned by it instead.
//
// It counts every UpdateAgent call (updates) and the GetAgent calls made
// after the first running write failed and before the rollback's first
// FinalizeAgentDeletion (rereads).
type managedConflictStore struct {
	store.Store
	mu           sync.Mutex
	beforeFirst  func(agentID string)
	firstErr     error
	beforeReread func(agentID string)
	beforeSecond func(agentID string)
	secondErr    error

	updates     int
	rereads     int
	firstDone   bool
	firstFailed bool
	finalized   bool
}

func (s *managedConflictStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	s.mu.Lock()
	s.updates++
	first := !s.firstDone && a.Phase == string(state.PhaseRunning)
	second := s.firstDone && s.updates == 2
	var hook func(string)
	var injected error
	switch {
	case first:
		s.firstDone = true
		hook, injected = s.beforeFirst, s.firstErr
	case second:
		hook, injected = s.beforeSecond, s.secondErr
	}
	s.mu.Unlock()
	if hook != nil {
		hook(a.ID)
	}
	err := injected
	if err == nil {
		err = s.Store.UpdateAgent(ctx, a)
	}
	if first && err != nil {
		s.mu.Lock()
		s.firstFailed = true
		s.mu.Unlock()
	}
	return err
}

func (s *managedConflictStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	s.mu.Lock()
	var hook func(string)
	if s.firstFailed && !s.finalized {
		s.rereads++
		hook, s.beforeReread = s.beforeReread, nil
	}
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *managedConflictStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	s.mu.Lock()
	s.finalized = true
	s.mu.Unlock()
	return s.Store.FinalizeAgentDeletion(ctx, id, pred, mode, set, hook)
}

func (s *managedConflictStore) counts() (updates, rereads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates, s.rereads
}

// bumpPhase moves the stored row to phase and activity with a plain
// UpdateAgent on the unwrapped store (a concurrent status write, not
// counted by managedConflictStore): it bumps state_version.
func bumpPhase(t *testing.T, s store.Store, id, phase, activity string) {
	t.Helper()
	row, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	row.Phase = phase
	row.Activity = activity
	require.NoError(t, s.UpdateAgent(context.Background(), row))
}

// requireManagedCreated checks rec is a 201 and returns the stored row.
func requireManagedCreated(t *testing.T, rec *httptest.ResponseRecorder, s store.Store) *store.Agent {
	t.Helper()
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	row, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err, "the row is kept")
	return row
}

// assertManagedConflictRolledBack checks agentID's create was compensated at
// the managed_record stage with cause, and its quotas released.
func assertManagedConflictRolledBack(t *testing.T, s store.Store, project *store.Project, agentID, cause string) {
	t.Helper()
	sum := assertCompensated(t, s, agentID)
	assert.Equal(t, createStageManagedRecord, sum.Stage)
	assert.Contains(t, sum.Error, cause)
	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
		"the per-broker reservation is released")
}

// T1: a delete claimed the row and gave up (failed, or its lease lapsed)
// between the create's commit and its post-create write. The claim's
// state_version bump makes the write conflict; the re-read finds a live
// row, the retry lands, and the create answers 201 with the managed Runtime
// and the interaction ID stored, cancelling nothing.
func TestManagedCreateConflictRetry_DeleteGaveUp_Answers201(t *testing.T) {
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
			pub := recordCreatedEvents(t, srv)
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			fs := &managedConflictStore{Store: s, beforeFirst: func(id string) {
				claimForTest(t, s, id, tc.state, tc.lease)
			}}
			srv.store = fs

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-retry-%d", i))
			row := requireManagedCreated(t, rec, s)

			assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted: %q", row.Runtime)
			assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
			assert.NotEmpty(t, row.Annotations[annotationCloudProvider])
			assert.Equal(t, string(state.PhaseRunning), row.Phase)
			assert.Equal(t, "working", row.Activity)
			assert.Equal(t, tc.state, row.DeletionState, "the retry leaves the delete's fields alone")
			assert.Empty(t, backend.cancels(), "no CancelInteraction")
			assert.Equal(t, []string{"interaction-1"}, backend.inProgress())
			assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
			updates, rereads := fs.counts()
			assert.Equal(t, 2, updates, "the conflicting write and one retry")
			assert.GreaterOrEqual(t, rereads, 1, "the row was re-read")
		})
	}
}

// T2: a live delete claim holds the row at the re-read (claimed before the
// write, so the write conflicts; or claimed after a status write made it
// conflict, right before the re-read). The delete won: no retry, the
// rollback runs as before, and the create answers 409 delete_in_progress
// with no agent body, having stopped the interaction.
func TestManagedCreateConflictRetry_DeleteHoldsRow_Answers409(t *testing.T) {
	timings := []string{"claim before write", "claim before re-read"}
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
			fs := &managedConflictStore{Store: s}
			if i == 0 {
				fs.beforeFirst = claim
			} else {
				fs.beforeFirst = func(id string) { bumpPhase(t, s, id, string(state.PhaseCreated), "") }
				fs.beforeReread = claim
			}
			srv.store = fs

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-retry-held-%d", i))
			require.NotEmpty(t, agentID, "the claim ran: %d %s", rec.Code, rec.Body.String())
			assert.Equal(t, []string{managedCreateCompensatedWarning}, requireDeletedDuringCreate(t, rec, agentID))
			assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "the create stops the interaction, once")
			assert.Empty(t, backend.inProgress())
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())

			row, err := s.GetAgent(context.Background(), agentID)
			require.NoError(t, err, "the delete's row is kept")
			assert.Equal(t, store.DeletionStateDeleting, row.DeletionState)
			assert.Empty(t, row.Annotations[annotationInteractionID], "no retry wrote the held row")
			assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
			updates, _ := fs.counts()
			assert.Equal(t, 1, updates, "no retry")
		})
	}
}

// T3: the retry fails too (a second conflict, or another error). The
// create takes the ptone/scion#3557 rollback with the retry's error, as
// today: 500 with the stop's warning, rolled back, and no third write. When
// the rollback itself does not complete, the 500 carries the correlation ID.
func TestManagedCreateConflictRetry_RetryFails_RollsBack(t *testing.T) {
	cases := []struct {
		name       string
		secondErr  error
		bumpSecond bool
		cause      string
		compFails  bool
	}{
		{name: "second conflict", bumpSecond: true, cause: store.ErrVersionConflict.Error()},
		{name: "other error", secondErr: errManagedRecordWrite, cause: errManagedRecordWrite.Error()},
		{name: "second conflict, rollback incomplete", bumpSecond: true, cause: store.ErrVersionConflict.Error(), compFails: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			pub := recordCreatedEvents(t, srv)
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			var inner store.Store = s
			if tc.compFails {
				inner = &createTxFaultStore{Store: s, auditErrFor: mutationTypeAgentCreateDispatchFailed}
			}
			fs := &managedConflictStore{
				Store:       inner,
				beforeFirst: func(id string) { claimForTest(t, s, id, store.DeletionStateFailed, time.Minute) },
				secondErr:   tc.secondErr,
			}
			if tc.bumpSecond {
				fs.beforeSecond = func(id string) { bumpPhase(t, s, id, string(state.PhaseCreated), "") }
			}
			srv.store = fs

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-retry-fail-%d", i))
			var agentID string
			var warnings []string
			if tc.compFails {
				agentID, warnings = requireManagedCreateRollbackIncomplete(t, rec)
				assert.True(t, agentGone(t, s, agentID), "the fallback removed the row")
			} else {
				agentID, warnings = requireManagedCreateUnrecorded(t, rec)
				assertManagedConflictRolledBack(t, s, project, agentID, tc.cause)
			}
			assert.Equal(t, []string{managedCreateUnrecordedStoppedWarning + " (agent " + agentID + ", interaction interaction-1)"}, warnings)
			assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "exactly one cancel")
			assert.Empty(t, backend.inProgress())
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())

			updates, _ := fs.counts()
			assert.Equal(t, 2, updates, "the first write and one retry; no third write")
		})
	}
}

// T4: a first error that is not a conflict is not retried: no re-read, no
// second write, and the ptone/scion#3557 500 and rollback as today.
func TestManagedCreateConflictRetry_NonConflict_NoReread(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedConflictStore{Store: s, firstErr: errManagedRecordWrite}
	srv.store = fs

	agentID, warnings := requireManagedCreateUnrecorded(t, managedCreate(t, srv, project.ID, "mgd-retry-nonconflict"))
	assert.Equal(t, []string{managedCreateUnrecordedStoppedWarning + " (agent " + agentID + ", interaction interaction-1)"}, warnings)
	assert.Equal(t, []string{"interaction-1"}, backend.cancels())
	assertManagedConflictRolledBack(t, s, project, agentID, errManagedRecordWrite.Error())
	updates, rereads := fs.counts()
	assert.Equal(t, 1, updates, "no retry")
	assert.Zero(t, rereads, "no re-read")
}

// T5 (regression): the first write lands: 201 with one write.
func TestManagedCreateConflictRetry_FirstWriteLands_OneWrite(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedConflictStore{Store: s}
	srv.store = fs

	rec := managedCreate(t, srv, project.ID, "mgd-retry-ok")
	row := requireManagedCreated(t, rec, s)
	assert.True(t, isManagedAgentRuntime(row.Runtime))
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
	assert.Equal(t, string(state.PhaseRunning), row.Phase)
	assert.Empty(t, backend.cancels())
	updates, _ := fs.counts()
	assert.Equal(t, 1, updates, "one write")
}

// T6 (phase): a concurrent status write moved the live row to error or
// stopped before the post-create write. The retry keeps that phase and
// activity (newer than the create's assumed running, as
// mergeDispatchedAgent does), but still records the managed Runtime and the
// interaction ID, so a later stop or delete can find the interaction. A
// non-terminal phase is moved on to running.
func TestManagedCreateConflictRetry_ConcurrentPhase(t *testing.T) {
	cases := []struct {
		phase, activity string
		wantPhase       string
		wantActivity    string
	}{
		{string(state.PhaseError), "", string(state.PhaseError), ""},
		{string(state.PhaseStopped), "", string(state.PhaseStopped), ""},
		{string(state.PhaseProvisioning), "", string(state.PhaseRunning), "working"},
	}
	for i, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			fs := &managedConflictStore{Store: s, beforeFirst: func(id string) {
				bumpPhase(t, s, id, tc.phase, tc.activity)
			}}
			srv.store = fs

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-retry-phase-%d", i))
			row := requireManagedCreated(t, rec, s)
			assert.Equal(t, tc.wantPhase, row.Phase)
			assert.Equal(t, tc.wantActivity, row.Activity)
			assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted")
			assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantPhase, resp.Agent.Phase, "the 201 answers the stored row")
			assert.Empty(t, backend.cancels())
			updates, _ := fs.counts()
			assert.Equal(t, 2, updates, "the conflicting write and one retry")
		})
	}
}
