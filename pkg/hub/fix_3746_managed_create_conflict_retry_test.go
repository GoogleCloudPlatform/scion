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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
//   - rereadErr, when set, is returned by the first GetAgent after the first
//     running write failed (a failed re-read), instead of reading the row.
//   - beforeSecond runs just before the next UpdateAgent after the first
//     running write (the retry); secondErr, when set, is returned by it
//     instead.
//
// It counts every UpdateAgent call (updates), the GetAgent calls made after
// the first running write failed and before the rollback's first
// FinalizeAgentDeletion (rereads), and the GetAgent calls made after the
// first running write failed and before the retry write (rereadsBeforeRetry).
type managedConflictStore struct {
	store.Store
	mu           sync.Mutex
	beforeFirst  func(agentID string)
	firstErr     error
	beforeReread func(agentID string)
	rereadErr    error
	beforeSecond func(agentID string)
	secondErr    error

	updates            int
	rereads            int
	rereadsBeforeRetry int
	firstDone          bool
	firstFailed        bool
	retried            bool
	finalized          bool
}

func (s *managedConflictStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	s.mu.Lock()
	s.updates++
	first := !s.firstDone && a.Phase == string(state.PhaseRunning)
	second := s.firstDone && !s.retried
	var hook func(string)
	var injected error
	switch {
	case first:
		s.firstDone = true
		hook, injected = s.beforeFirst, s.firstErr
	case second:
		s.retried = true
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
	var injected error
	if s.firstFailed && !s.finalized {
		s.rereads++
		hook, s.beforeReread = s.beforeReread, nil
		injected, s.rereadErr = s.rereadErr, nil
	}
	if s.firstFailed && !s.retried {
		s.rereadsBeforeRetry++
	}
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if injected != nil {
		return nil, injected
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

func (s *managedConflictStore) rereadsBeforeRetryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rereadsBeforeRetry
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
			updates, _ := fs.counts()
			assert.Equal(t, 2, updates, "the conflicting write and one retry")
			// Exactly one read between the conflicting write and the retry:
			// the re-read. (Reads after the retry, such as the created
			// publish's own re-read, are not part of the retry.)
			assert.Equal(t, 1, fs.rereadsBeforeRetryCount(), "one re-read before the retry")
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
			inner := s
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

// The re-read after a conflict fails (not ErrNotFound): no retry. The
// create takes the ptone/scion#3557 rollback with the first write's
// conflict, as before: 500, rolled back, the interaction stopped once.
func TestManagedCreateConflictRetry_RereadFails_RollsBack(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedConflictStore{
		Store:       s,
		beforeFirst: func(id string) { claimForTest(t, s, id, store.DeletionStateFailed, time.Minute) },
		rereadErr:   errManagedRecordWrite,
	}
	srv.store = fs

	agentID, warnings := requireManagedCreateUnrecorded(t, managedCreate(t, srv, project.ID, "mgd-retry-rereadfail"))
	assert.Equal(t, []string{managedCreateUnrecordedStoppedWarning + " (agent " + agentID + ", interaction interaction-1)"}, warnings)
	assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "exactly one cancel")
	assertManagedConflictRolledBack(t, s, project, agentID, store.ErrVersionConflict.Error())
	updates, _ := fs.counts()
	assert.Equal(t, 1, updates, "no retry after a failed re-read")
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

const (
	foreignAnnotationKey = "test.scion.dev/concurrent-writer"
	concurrentMessage    = "set by a concurrent writer"
)

// T6 (phase): a concurrent status write moved the live row to error or
// stopped before the post-create write. The retry keeps that phase and
// activity (newer than the create's assumed running, as
// mergeDispatchedAgent does), but still records the managed Runtime and the
// interaction ID, and the create stops the interaction (that writer's stop
// could not find it). A non-terminal phase is moved on to running and the
// interaction is left running.
func TestManagedCreateConflictRetry_ConcurrentPhase(t *testing.T) {
	cases := []struct {
		phase, activity string
		wantPhase       string
		wantActivity    string
		wantCancel      bool
	}{
		{string(state.PhaseError), "", string(state.PhaseError), "", true},
		{string(state.PhaseStopped), "", string(state.PhaseStopped), "", true},
		{string(state.PhaseProvisioning), "", string(state.PhaseRunning), "working", false},
	}
	for i, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			fs := &managedConflictStore{Store: s, beforeFirst: func(id string) {
				// The concurrent writer also sets a Message and an
				// annotation key the create does not own: the retry keeps
				// both (it merges its annotation keys, it does not replace
				// the map).
				row, err := s.GetAgent(context.Background(), id)
				require.NoError(t, err)
				row.Phase = tc.phase
				row.Activity = tc.activity
				row.Message = concurrentMessage
				if row.Annotations == nil {
					row.Annotations = map[string]string{}
				}
				row.Annotations[foreignAnnotationKey] = "kept"
				require.NoError(t, s.UpdateAgent(context.Background(), row))
			}}
			srv.store = fs

			rec := managedCreate(t, srv, project.ID, fmt.Sprintf("mgd-retry-phase-%d", i))
			row := requireManagedCreated(t, rec, s)
			assert.Equal(t, tc.wantPhase, row.Phase)
			assert.Equal(t, tc.wantActivity, row.Activity)
			assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted")
			assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
			assert.Equal(t, "kept", row.Annotations[foreignAnnotationKey], "the concurrent writer's annotation is kept")
			assert.Equal(t, concurrentMessage, row.Message, "the concurrent writer's Message is kept")

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantPhase, resp.Agent.Phase, "the 201 answers the stored row")
			if tc.wantCancel {
				assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "a terminal phase stops the interaction, once")
				assert.Empty(t, backend.inProgress())
			} else {
				assert.Empty(t, backend.cancels(), "a live phase leaves the interaction running")
			}
			updates, _ := fs.counts()
			assert.Equal(t, 2, updates, "the conflicting write and one retry")
		})
	}
}

// A stop lands before the post-create write records the interaction
// (ptone/scion#3746, review NB3): the stop's own managedAgentStop finds no
// interaction ID, so the create stops it after the retried write. 201 with
// the stopped row, which names the interaction; exactly one cancel; no
// warning in the response.
func TestManagedCreateConflictRetry_StopBeforeRecord_CancelsOnce(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedConflictStore{Store: s, beforeFirst: func(id string) {
		bumpPhase(t, s, id, string(state.PhaseStopped), "")
	}}
	srv.store = fs

	rec := managedCreate(t, srv, project.ID, "mgd-retry-stop-race")
	row := requireManagedCreated(t, rec, s)
	assert.Equal(t, string(state.PhaseStopped), row.Phase)
	assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted")
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
	assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "exactly one CancelInteraction")
	assert.Empty(t, backend.inProgress(), "nothing is left running")
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Warnings, "no warning in the response")
	updates, _ := fs.counts()
	assert.Equal(t, 2, updates, "the conflicting write and one retry")
}

// The stop of that interaction fails: it is logged (Warn, naming the agent
// and the interaction) and the create still answers 201 with no warning in
// the response. The row names the interaction, so a later stop or delete
// can retry the cancel.
func TestManagedCreateConflictRetry_StopBeforeRecord_CancelFails_Logs(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	backend := newInteractionLedgerBackend()
	backend.cancelErr = errors.New("backend unavailable")
	useManagedBackend(t, backend)
	srv.store = &managedConflictStore{Store: s, beforeFirst: func(id string) {
		bumpPhase(t, s, id, string(state.PhaseStopped), "")
	}}

	rec := managedCreate(t, srv, project.ID, "mgd-retry-stop-cancelfail")
	row := requireManagedCreated(t, rec, s)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "warnings", "a failed cancel adds no warning to the 201")
	assert.Equal(t, string(state.PhaseStopped), row.Phase)
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID], "the row names the interaction for a later stop")
	assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "the cancel was tried once")
	assert.Equal(t, []string{"interaction-1"}, backend.inProgress(), "the failed cancel left it running")

	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "interaction_id=interaction-1") && strings.Contains(l, "backend unavailable") {
			line = l
			break
		}
	}
	require.NotEmpty(t, line, "the failed cancel is logged: %s", logs.String())
	assert.Contains(t, line, "level=WARN")
	assert.Contains(t, line, "agent_id="+row.ID)
}

// A terminal phase with no interaction (a create with no task started
// none): nothing to stop, no cancel, and the create still answers 201.
func TestManagedCreateConflictRetry_StopBeforeRecord_NoInteraction(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fs := &managedConflictStore{Store: s, beforeFirst: func(id string) {
		bumpPhase(t, s, id, string(state.PhaseStopped), "")
	}}
	srv.store = fs

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "mgd-retry-stop-notask", ProjectID: project.ID, Profile: ManagedAgentsProfile,
	})
	row := requireManagedCreated(t, rec, s)
	assert.Equal(t, string(state.PhaseStopped), row.Phase)
	assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted")
	assert.Empty(t, row.Annotations[annotationInteractionID], "no interaction was started")
	assert.Empty(t, backend.cancels(), "no cancel")
	updates, _ := fs.counts()
	assert.Equal(t, 2, updates, "the conflicting write and one retry")
}
