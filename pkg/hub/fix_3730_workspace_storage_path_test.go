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
	"os"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3730: when the create uploads a hub-managed project workspace
// for a remote broker, the write that records the swap (Workspace cleared,
// WorkspaceStoragePath and WorkspaceStorageBucket set) must land on the row
// before anything is dispatched. A version conflict gets one re-read and one
// retry; a delete that won at the re-read, a failed retry or any other error
// rolls the create back at stage workspace_record (500 internal_error, or 409
// when a delete holds the row) with nothing dispatched and nothing published.

const workspaceRecordTestBucket = "ws-record-bucket"

var errWorkspaceRecordWrite = errors.New("workspace record write failed")

// workspaceRecordStore wraps the create's workspace record write: the first
// UpdateAgent whose AppliedConfig has a WorkspaceStoragePath, and the one
// write right after it when it failed (the retry).
//
//   - beforeFirst runs just before the first such write, which then goes to
//     the real store: a hook that bumps state_version (a delete claim, a
//     status write) makes it a real ErrVersionConflict. firstErr, when set,
//     is returned instead.
//   - rereadErr, when set, is returned by the first GetAgent after the
//     first record write failed (a failed re-read), instead of reading the
//     row.
//   - beforeSecond runs just before the retry; secondErr, when set, is
//     returned by it instead.
//
// It records the agent ID and counts every UpdateAgent from the first
// record write on (writes). Until fault is armed it delegates every call
// unchanged; installWorkspaceRecordStore installs it.
type workspaceRecordStore struct {
	store.Store
	fault        *storeFaultSwitch
	mu           sync.Mutex
	beforeFirst  func(agentID string)
	firstErr     error
	rereadErr    error
	beforeSecond func(agentID string)
	secondErr    error

	agentID     string
	writes      int
	firstDone   bool
	firstFailed bool
	retried     bool
}

func (s *workspaceRecordStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if !s.fault.Active() {
		return s.Store.UpdateAgent(ctx, a)
	}
	s.mu.Lock()
	first := !s.firstDone && a.AppliedConfig != nil && a.AppliedConfig.WorkspaceStoragePath != ""
	second := s.firstFailed && !s.retried
	if first || s.firstDone {
		s.writes++
	}
	var hook func(string)
	var injected error
	switch {
	case first:
		s.firstDone = true
		s.agentID = a.ID
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

func (s *workspaceRecordStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if !s.fault.Active() {
		return s.Store.GetAgent(ctx, id)
	}
	s.mu.Lock()
	var injected error
	if s.firstFailed {
		injected, s.rereadErr = s.rereadErr, nil
	}
	s.mu.Unlock()
	if injected != nil {
		return nil, injected
	}
	return s.Store.GetAgent(ctx, id)
}

// installWorkspaceRecordStore installs a workspaceRecordStore on srv through
// installStoreFault, right after the server is built. configure, when set,
// fills in the hooks and injected errors before the wrapper is installed.
// The wrapper delegates until the returned switch is armed; tests arm it
// just before the create under test.
func installWorkspaceRecordStore(t *testing.T, srv *Server, configure func(fs *workspaceRecordStore)) (*workspaceRecordStore, *storeFaultSwitch) {
	t.Helper()
	return installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) *workspaceRecordStore {
		fs := &workspaceRecordStore{Store: inner, fault: fault}
		if configure != nil {
			configure(fs)
		}
		return fs
	})
}

func (s *workspaceRecordStore) snapshot() (agentID string, writes int, retried bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID, s.writes, s.retried
}

// useHubWorkspaceUpload makes srv upload the hub-managed project workspace
// for a remote broker: GCS storage, a HOME for the project directory, and a
// stubbed GCS sync. It returns a counter of the uploads.
func useHubWorkspaceUpload(t *testing.T, srv *Server, project *store.Project) func() int {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv.SetStorage(newGCSContentMockStorage(workspaceRecordTestBucket))
	t.Cleanup(func() {
		if p, err := hubManagedProjectPath(project.Slug); err == nil {
			_ = os.RemoveAll(p)
		}
	})
	var mu sync.Mutex
	uploads := 0
	previous := syncToGCSForWorkspaceUpload
	syncToGCSForWorkspaceUpload = func(context.Context, string, string, string) error {
		mu.Lock()
		uploads++
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { syncToGCSForWorkspaceUpload = previous })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return uploads
	}
}

func brokerCreate(t *testing.T, srv *Server, projectID, name string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: name, ProjectID: projectID, Task: "do it",
	})
}

// assertWorkspaceRecorded checks the stored row carries the swap.
func assertWorkspaceRecorded(t *testing.T, row *store.Agent) {
	t.Helper()
	require.NotNil(t, row.AppliedConfig)
	assert.NotEmpty(t, row.AppliedConfig.WorkspaceStoragePath, "the storage path is stored")
	assert.Equal(t, workspaceRecordTestBucket, row.AppliedConfig.WorkspaceStorageBucket, "the bucket is stored")
	assert.Empty(t, row.AppliedConfig.Workspace, "the hub-local workspace is cleared")
}

// requireWorkspaceRecordFailed checks rec is the 500 of a create whose
// workspace record write failed, with no agent body.
func requireWorkspaceRecordFailed(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "agent", "the 500 carries no agent body")
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeInternalError, body.Error.Code)
	assert.Equal(t, "Failed to record workspace storage path", body.Error.Message)
}

// assertWorkspaceRecordRolledBack checks agentID's create was compensated at
// the workspace_record stage with cause, and its quotas released.
func assertWorkspaceRecordRolledBack(t *testing.T, s store.Store, project *store.Project, agentID, cause string) {
	t.Helper()
	sum := assertCompensated(t, s, agentID)
	assert.Equal(t, createStageWorkspaceRecord, sum.Stage)
	assert.Contains(t, sum.Error, cause)
	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
		"the per-broker reservation is released")
}

// The record write lands first time: one write, 201, the row and the
// dispatch both carry the storage path.
func TestWorkspaceRecord_FirstWriteLands(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	fs, fault := installWorkspaceRecordStore(t, srv, nil)
	uploads := useHubWorkspaceUpload(t, srv, project)
	fault.Arm()

	rec := brokerCreate(t, srv, project.ID, "ws-record-ok")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, 1, uploads(), "fixture check: the upload ran")

	agentID, _, retried := fs.snapshot()
	require.NotEmpty(t, agentID, "fixture check: the record write ran")
	assert.False(t, retried, "no retry")
	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	assertWorkspaceRecorded(t, row)
	require.NotNil(t, disp.capturedAgent, "dispatched")
	assert.NotEmpty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
}

// A non-conflict failure of the record write fails the create: 500, rolled
// back at workspace_record, no re-read retry, nothing dispatched, nothing
// published.
func TestWorkspaceRecord_WriteFails_RollsBack(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.firstErr = errWorkspaceRecordWrite
	})
	useHubWorkspaceUpload(t, srv, project)
	pub := recordCreatedEvents(t, srv)
	fault.Arm()

	rec := brokerCreate(t, srv, project.ID, "ws-record-fail")
	requireWorkspaceRecordFailed(t, rec)

	agentID, writes, _ := fs.snapshot()
	require.NotEmpty(t, agentID)
	assertWorkspaceRecordRolledBack(t, s, project, agentID, errWorkspaceRecordWrite.Error())
	assert.Equal(t, 1, writes, "no retry after a non-conflict error")
	assert.Nil(t, disp.capturedAgent, "nothing dispatched")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
}

// A delete claimed the row and gave up (failed, or its lease lapsed)
// before the record write: the write conflicts, the re-read finds a live
// row, the retry stores the swap, and the create answers 201 with the
// delete's fields kept. Covers the broker path, and the nil-dispatcher path
// where nothing writes the row after the record write.
func TestWorkspaceRecord_ConflictRetry_Answers201(t *testing.T) {
	cases := []struct {
		name  string
		state string
		lease time.Duration
	}{
		{"delete-failed", store.DeletionStateFailed, time.Minute},
		{"delete-lapsed", store.DeletionStateDeleting, -time.Minute},
	}
	for _, nilDispatcher := range []bool{false, true} {
		for i, tc := range cases {
			name := tc.name
			if nilDispatcher {
				name += "/nil-dispatcher"
			}
			t.Run(name, func(t *testing.T) {
				disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
				var srv *Server
				var s store.Store
				var project *store.Project
				if nilDispatcher {
					srv, s, project = setupCreateAgentServer(t, nil)
				} else {
					srv, s, project = setupCreateAgentServer(t, disp)
				}
				fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
					fs.beforeFirst = func(id string) {
						claimForTest(t, s, id, tc.state, tc.lease)
					}
				})
				useHubWorkspaceUpload(t, srv, project)
				fault.Arm()

				rec := brokerCreate(t, srv, project.ID, fmt.Sprintf("ws-record-retry-%d-%t", i, nilDispatcher))
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

				agentID, _, retried := fs.snapshot()
				require.NotEmpty(t, agentID)
				assert.True(t, retried, "the conflicting write was retried")
				row, err := s.GetAgent(context.Background(), agentID)
				require.NoError(t, err)
				assertWorkspaceRecorded(t, row)
				assert.Equal(t, tc.state, row.DeletionState, "the retry leaves the delete's fields alone")
				if !nilDispatcher {
					require.NotNil(t, disp.capturedAgent, "dispatched")
					assert.NotEmpty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath)
				}
			})
		}
	}
}

// A live delete claim holds the row at the re-read: the delete won. No
// retry writes the held row, the create answers 409 delete_in_progress with
// no agent body, the row is left to the delete, and nothing is dispatched
// or published.
func TestWorkspaceRecord_DeleteHoldsRow_Answers409(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.beforeFirst = func(id string) {
			claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
		}
	})
	useHubWorkspaceUpload(t, srv, project)
	pub := recordCreatedEvents(t, srv)
	fault.Arm()

	rec := brokerCreate(t, srv, project.ID, "ws-record-held")
	agentID, writes, retried := fs.snapshot()
	require.NotEmpty(t, agentID, "the claim ran: %d %s", rec.Code, rec.Body.String())
	requireDeletedDuringCreate(t, rec, agentID)

	assert.False(t, retried, "no retry")
	assert.Equal(t, 1, writes, "only the conflicting write")
	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err, "the delete's row is kept")
	assert.Equal(t, store.DeletionStateDeleting, row.DeletionState)
	if row.AppliedConfig != nil {
		assert.Empty(t, row.AppliedConfig.WorkspaceStoragePath, "no retry wrote the held row")
	}
	assert.Empty(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID), "no compensation was written")
	assert.Nil(t, disp.capturedAgent, "nothing dispatched")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
}

// The retry fails too (a second conflict, or another error): the create
// rolls back at workspace_record with the retry's error and answers 500.
func TestWorkspaceRecord_RetryFails_RollsBack(t *testing.T) {
	cases := []struct {
		name       string
		secondErr  error
		bumpSecond bool
		cause      string
	}{
		{name: "second conflict", bumpSecond: true, cause: store.ErrVersionConflict.Error()},
		{name: "other error", secondErr: errWorkspaceRecordWrite, cause: errWorkspaceRecordWrite.Error()},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
				fs.beforeFirst = func(id string) { bumpPhase(t, s, id, string(state.PhaseCreated), "") }
				fs.secondErr = tc.secondErr
				if tc.bumpSecond {
					fs.beforeSecond = func(id string) { bumpPhase(t, s, id, string(state.PhaseCreated), "") }
				}
			})
			useHubWorkspaceUpload(t, srv, project)
			pub := recordCreatedEvents(t, srv)
			fault.Arm()

			rec := brokerCreate(t, srv, project.ID, fmt.Sprintf("ws-record-retry-fail-%d", i))
			requireWorkspaceRecordFailed(t, rec)

			agentID, writes, retried := fs.snapshot()
			require.NotEmpty(t, agentID)
			assert.True(t, retried, "one retry")
			assert.Equal(t, 2, writes, "the conflicting write and one retry, no third")
			assertWorkspaceRecordRolledBack(t, s, project, agentID, tc.cause)
			assert.Nil(t, disp.capturedAgent, "nothing dispatched")
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
		})
	}
}

// The record write conflicts and the re-read fails: no retry, the create
// rolls back at workspace_record with the conflict as the cause and answers
// 500, with nothing dispatched or published.
func TestWorkspaceRecord_RereadFails_RollsBack(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.beforeFirst = func(id string) { bumpPhase(t, s, id, string(state.PhaseCreated), "") }
		fs.rereadErr = errors.New("re-read failed")
	})
	useHubWorkspaceUpload(t, srv, project)
	pub := recordCreatedEvents(t, srv)
	fault.Arm()

	rec := brokerCreate(t, srv, project.ID, "ws-record-reread-fail")
	requireWorkspaceRecordFailed(t, rec)

	agentID, writes, retried := fs.snapshot()
	require.NotEmpty(t, agentID)
	assert.False(t, retried, "no retry after a failed re-read")
	assert.Equal(t, 1, writes, "only the conflicting write")
	assertWorkspaceRecordRolledBack(t, s, project, agentID, store.ErrVersionConflict.Error())
	assert.Nil(t, disp.capturedAgent, "nothing dispatched")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
}

// A delete removed the row before the record write: the write fails with
// store.ErrNotFound, the rollback finds the delete won, and the create
// answers 409 delete_in_progress with nothing dispatched or published.
func TestWorkspaceRecord_DeleteRemovedRow_Answers409(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.beforeFirst = func(id string) {
			require.NoError(t, s.DeleteAgent(context.Background(), id))
		}
	})
	useHubWorkspaceUpload(t, srv, project)
	pub := recordCreatedEvents(t, srv)
	fault.Arm()

	rec := brokerCreate(t, srv, project.ID, "ws-record-removed")
	agentID, writes, retried := fs.snapshot()
	require.NotEmpty(t, agentID, "the delete ran: %d %s", rec.Code, rec.Body.String())
	requireDeletedDuringCreate(t, rec, agentID)

	assert.False(t, retried, "no retry after a non-conflict error")
	assert.Equal(t, 1, writes, "only the failed write")
	_, err := s.GetAgent(context.Background(), agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the row stays removed")
	assert.Nil(t, disp.capturedAgent, "nothing dispatched")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
}

// Broker create accepted for asynchronous launch: a conflicting record write
// is retried, the launch request carries the storage path, and the accepted
// launch's own write (persistAcceptedLaunch, whose retry merges with
// mergeDispatchedConfig) keeps the path on the row.
func TestWorkspaceRecord_AsyncAccepted_ConflictRetryKeepsStoragePath(t *testing.T) {
	srv, s, project, client := newAsyncCreateServer(t, true)
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.beforeFirst = func(id string) {
			claimForTest(t, s, id, store.DeletionStateFailed, time.Minute)
		}
	})
	useHubWorkspaceUpload(t, srv, project)
	fault.Arm()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "ws-record-async", "projectId": project.ID, "task": "do it", "acceptAsyncLaunch": true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agentID, _, retried := fs.snapshot()
	require.NotEmpty(t, agentID)
	assert.True(t, retried, "the conflicting record write was retried")
	require.Len(t, client.sends, 1, "one launch request")
	assert.True(t, client.sends[0].AsyncLaunch, "fixture check: an async launch")
	assert.NotEmpty(t, client.sends[0].WorkspaceStoragePath, "the launch request carries the storage path")
	assert.Equal(t, workspaceRecordTestBucket, client.sends[0].WorkspaceStorageBucket)
	row, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	assertWorkspaceRecorded(t, row)
	assert.Equal(t, store.DeletionStateFailed, row.DeletionState, "the delete's fields are kept")
}

// Managed create: a conflicting record write is retried, which leaves the
// create's copy at an older state_version, so the managed post-create write
// conflicts too and goes through mergeManagedCreate. The stored row keeps
// the storage path and the managed fields, and the create answers 201.
func TestWorkspaceRecord_Managed_ConflictRetryKeepsStoragePath(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.beforeFirst = func(id string) {
			claimForTest(t, s, id, store.DeletionStateFailed, time.Minute)
		}
	})
	useHubWorkspaceUpload(t, srv, project)
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fault.Arm()

	rec := managedCreate(t, srv, project.ID, "ws-record-managed")
	row := requireManagedCreated(t, rec, s)

	_, _, retried := fs.snapshot()
	assert.True(t, retried, "the conflicting record write was retried")
	assertWorkspaceRecorded(t, row)
	assert.True(t, isManagedAgentRuntime(row.Runtime), "the managed Runtime is persisted: %q", row.Runtime)
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
	assert.Equal(t, string(state.PhaseRunning), row.Phase)
}

// Managed create: a concurrent stop lands before the record write, so the
// write conflicts and the retry lands on the live, stopped row. The
// create's copy keeps its older state_version, so its post-create write
// conflicts too, and mergeManagedCreate keeps the stopped phase: the create
// answers 201 from the stored row, the storage path is kept, and the
// create stops its interaction once.
func TestWorkspaceRecord_Managed_ConcurrentStopBeforeRecord_KeepsStopped(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createRaceDispatcher{})
	fs, fault := installWorkspaceRecordStore(t, srv, func(fs *workspaceRecordStore) {
		fs.beforeFirst = func(id string) {
			bumpPhase(t, s, id, string(state.PhaseStopped), "")
		}
	})
	useHubWorkspaceUpload(t, srv, project)
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	fault.Arm()

	rec := managedCreate(t, srv, project.ID, "ws-record-managed-stopped")
	row := requireManagedCreated(t, rec, s)

	_, _, retried := fs.snapshot()
	assert.True(t, retried, "the conflicting record write was retried")
	assert.Equal(t, string(state.PhaseStopped), row.Phase, "the concurrent stop is kept")
	assertWorkspaceRecorded(t, row)
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, string(state.PhaseStopped), resp.Agent.Phase, "the 201 answers the stored row")
	assert.Equal(t, []string{"interaction-1"}, backend.cancels(), "the create stops the interaction, once")
	assert.Empty(t, backend.inProgress())
}

// mergeManagedCreate carries the hub-managed workspace swap onto the
// re-read row, including when a concurrent writer's terminal phase is kept,
// and leaves the row's workspace alone when the create made no swap.
func TestMergeManagedCreate_CopiesWorkspaceStorage(t *testing.T) {
	swapped := func() *store.Agent {
		return &store.Agent{
			Phase: string(state.PhaseRunning),
			AppliedConfig: &store.AgentAppliedConfig{
				WorkspaceStoragePath:   "hubs/h/projects/p/workspace",
				WorkspaceStorageBucket: workspaceRecordTestBucket,
			},
		}
	}
	for _, tc := range []struct{ phase, wantPhase string }{
		{string(state.PhaseCreated), string(state.PhaseRunning)},
		{string(state.PhaseStopped), string(state.PhaseStopped)},
	} {
		t.Run("swap/"+tc.phase, func(t *testing.T) {
			dst := &store.Agent{Phase: tc.phase, AppliedConfig: &store.AgentAppliedConfig{Workspace: "/hub/local", Task: "kept"}}
			mergeManagedCreate(dst, swapped())
			assertWorkspaceRecorded(t, dst)
			assert.Equal(t, tc.wantPhase, dst.Phase, "a terminal phase is kept; a live one takes the create's running")
			assert.Equal(t, "kept", dst.AppliedConfig.Task, "other applied config is kept")
		})
	}
	t.Run("nil applied config", func(t *testing.T) {
		dst := &store.Agent{Phase: string(state.PhaseCreated)}
		mergeManagedCreate(dst, swapped())
		assertWorkspaceRecorded(t, dst)
	})
	t.Run("no swap", func(t *testing.T) {
		dst := &store.Agent{Phase: string(state.PhaseCreated), AppliedConfig: &store.AgentAppliedConfig{Workspace: "/hub/local"}}
		mergeManagedCreate(dst, &store.Agent{Phase: string(state.PhaseRunning), AppliedConfig: &store.AgentAppliedConfig{}})
		assert.Equal(t, "/hub/local", dst.AppliedConfig.Workspace)
		assert.Empty(t, dst.AppliedConfig.WorkspaceStoragePath)
	})
}
