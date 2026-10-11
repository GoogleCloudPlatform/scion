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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// probeCancelRequest cancels the client's request inside the dispatch,
	// records the dispatch ctx's state, and fails with ctx.Err() if the
	// dispatch ctx followed the request (as a broker abort would).
	probeCancelRequest launchProbeMode = iota
	// probeBlock blocks until the dispatch ctx is done (bounded by
	// requestCancelWait) and records how it ended.
	probeBlock
	// probeBlockStart lets a stop dispatch succeed without probing it and
	// behaves as probeBlock for every other dispatch: a restart's stop leg
	// succeeds and its start leg blocks.
	probeBlockStart
)

// errProbeNeverDone is returned by a probeBlock dispatch whose ctx was not
// done within requestCancelWait: the dispatch had no deadline.
var errProbeNeverDone = errors.New("dispatch ctx never done")

// launchProbeDispatcher records the ctx each launch dispatch ran on.
type launchProbeDispatcher struct {
	createAgentDispatcher
	mode          launchProbeMode
	cancelRequest context.CancelFunc
	// s, when set, is read at create-dispatch time to record what the row
	// held when the launch began.
	s store.Store
	// rowStoragePath is the row's WorkspaceStoragePath when the create
	// dispatch began.
	rowStoragePath string

	createCalls int
	startCalls  int
	stopCalls   int
	// ctxErrs holds, per dispatch in call order, the dispatch ctx's error
	// once the probe acted (nil: live).
	ctxErrs []error
	// hadDeadline records, per dispatch, whether its ctx carried a deadline.
	hadDeadline []bool
	// deadlineIn records, per dispatch, how far away its deadline was when
	// the dispatch began (0 without one).
	deadlineIn []time.Duration
}

// newCancelableRequest builds a request whose context the returned cancel
// func cancels; serve runs it through the handler and returns the recorder.
func newCancelableRequest(t *testing.T, srv *Server, method, path string, body any) (cancel context.CancelFunc, serve func() *httptest.ResponseRecorder) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	reqCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(method, path, reader).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	return cancel, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
}

// shortenSyncDispatchTimeout sets syncDispatchTimeout to d for the test.
func shortenSyncDispatchTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := syncDispatchTimeout
	syncDispatchTimeout = d
	t.Cleanup(func() { syncDispatchTimeout = prev })
}

func requireAllLive(t *testing.T, disp *launchProbeDispatcher, wantCalls int) {
	t.Helper()
	require.Len(t, disp.ctxErrs, wantCalls, "dispatch count")
	for i, err := range disp.ctxErrs {
		assert.NoError(t, err, "dispatch %d must not follow the canceled request", i)
		assertSyncDispatchDeadline(t, disp, i)
	}
}

// assertSyncDispatchDeadline asserts dispatch i ran under a deadline no
// later than syncDispatchTimeout from its start: a site that passes the
// launch ctx instead of the syncDispatch ctx has no deadline, or a later one.
func assertSyncDispatchDeadline(t *testing.T, disp *launchProbeDispatcher, i int) {
	t.Helper()
	require.Greater(t, len(disp.hadDeadline), i)
	assert.True(t, disp.hadDeadline[i], "dispatch %d must run under syncDispatchTimeout", i)
	assert.LessOrEqual(t, disp.deadlineIn[i], syncDispatchTimeout, "dispatch %d deadline is later than syncDispatchTimeout", i)
	assert.Greater(t, disp.deadlineIn[i], syncDispatchTimeout-5*time.Second, "dispatch %d deadline is much earlier than syncDispatchTimeout", i)
}

// failingManagedAgentBackend is a managedagent.ManagedAgentBackend whose
// CreateInteraction always fails, simulating a cloud-provider error during
// managed-agent creation (handlers_managed_agents.go's managedAgentCreate).
// Swapping the package-level managedBackendInst under managedBackendMu is an
// existing test seam (see stubManagedAgentBackend in
// handlers_agent_messaging_test.go).
type failingManagedAgentBackend struct{}

// requestCancelWait bounds how long a mock waits to observe the request ctx
// being canceled after it fires cancelRequest. If a refactor stops passing
// the request ctx through, the test fails on ctxNotCanceled instead of
// hanging until the package timeout.
const requestCancelWait = 5 * time.Second

// awaitCanceled waits up to requestCancelWait for ctx to be canceled and
// reports whether it was.
func awaitCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(requestCancelWait):
		return false
	}
}

// setAgentQuotaLimits gives both create-time limits a positive value so
// that Reserve actually records a reservation for each: both are seeded
// unlimited (0), and QuotaService.Reserve skips an unlimited limit entirely,
// which would make a "no reservation left" assertion vacuous.
func setAgentQuotaLimits(t *testing.T, s store.Store) {
	t.Helper()
	setBrokerAgentCeiling(t, s, 5)
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	def.DefaultValue = 5
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

func observeReservations(t *testing.T, s store.Store, agentID string) reservationsHeld {
	return reservationsHeld{
		broker:  hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID),
		project: hasReservation(t, s, store.LimitMaxAgentsPerProject, agentID),
	}
}

func assertReservationsHeldBeforeCleanup(t *testing.T, held reservationsHeld) {
	t.Helper()
	require.True(t, held.broker, "precondition: the per-broker reservation must be held before cleanup")
	require.True(t, held.project, "precondition: the per-project reservation must be held before cleanup")
}

func assertNoReservations(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID),
		"a create canceled before its failure cleanup must release the per-broker reservation")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, agentID),
		"a create canceled before its failure cleanup must release the per-project reservation")
}

// ctxObservation records how a cleanup step's ctx looked when it ran.
type ctxObservation struct {
	called      bool
	err         error
	hasDeadline bool
	budget      time.Duration
}

func observeCtx(ctx context.Context) ctxObservation {
	o := ctxObservation{called: true, err: ctx.Err()}
	var deadline time.Time
	deadline, o.hasDeadline = ctx.Deadline()
	if o.hasDeadline {
		o.budget = time.Until(deadline)
	}
	return o
}

func newInteractionLedgerBackend() *interactionLedgerBackend {
	return &interactionLedgerBackend{status: map[string]managedagent.InteractionStatus{}}
}

var errManagedRecordWrite = errors.New("simulated store outage")

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

const (
	slowPathWriteTimeout = 200 * time.Millisecond
	slowPathDelay        = 400 * time.Millisecond
)

// slowLaunchDispatcher succeeds each launch-path broker call after delay.
type slowLaunchDispatcher struct {
	createAgentDispatcher
	delay time.Duration
}

// serveThroughSlowListener serves srv through a real http.Server whose
// WriteTimeout is slowPathWriteTimeout (the hub's configured one too), sends
// the request, and returns the status and body. It fails the test if the
// response is dropped.
func serveThroughSlowListener(t *testing.T, srv *Server, method, path string, body any, minElapsed time.Duration) (int, []byte) {
	t.Helper()
	srv.config.WriteTimeout = slowPathWriteTimeout
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Config.WriteTimeout = slowPathWriteTimeout
	hs.Start()
	t.Cleanup(hs.Close)

	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, hs.URL+path, reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	start := time.Now()
	resp, err := hs.Client().Do(req)
	require.NoError(t, err, "the response must not be dropped at the listener's WriteTimeout")
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err, "the response body must arrive in full")
	require.GreaterOrEqual(t, time.Since(start), minElapsed, "fixture check: the wait must outlast the WriteTimeout")
	return resp.StatusCode, buf.Bytes()
}

func setupSlowLaunchServer(t *testing.T) (*Server, store.Store, *store.Project) {
	t.Helper()
	return setupSlowLaunchServerWithDelay(t, slowPathDelay)
}

// setSyncDispatchWriteSlack sets syncDispatchWriteSlack for one test. The
// production slack (30s) dwarfs the scaled waits, so a test that must tell
// the per-path budgets apart shrinks it.
func setSyncDispatchWriteSlack(t *testing.T, d time.Duration) {
	t.Helper()
	prev := syncDispatchWriteSlack
	syncDispatchWriteSlack = d
	t.Cleanup(func() { syncDispatchWriteSlack = prev })
}

// setWorkspaceCheckTimeout sets workspaceCheckTimeout for one test.
func setWorkspaceCheckTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := workspaceCheckTimeout
	workspaceCheckTimeout = d
	t.Cleanup(func() { workspaceCheckTimeout = prev })
}

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

// answerBrokerUploads answers the next n workspace upload requests tunneled
// to broker, each after delay, with an empty manifest, and sends each
// request's path on the returned channel.
func answerBrokerUploads(t *testing.T, broker *fakeBroker, delay time.Duration, n int) <-chan string {
	t.Helper()
	paths := make(chan string, n)
	go func() {
		for answered := 0; answered < n; {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			paths <- env.Path
			time.Sleep(delay)
			body, _ := json.Marshal(RuntimeBrokerWorkspaceUploadResponse{Manifest: &transfer.Manifest{Version: "1.0"}})
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, body))
			answered++
		}
	}()
	return paths
}

// errDownloadNeverCut is returned by a blocking download whose ctx was not
// done within requestCancelWait: the download had no bound.
var errDownloadNeverCut = errors.New("download ctx never done")

// setStopAllAgentOpTimeout sets stopAllAgentOpTimeout for one test.
func setStopAllAgentOpTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := stopAllAgentOpTimeout
	stopAllAgentOpTimeout = d
	t.Cleanup(func() { stopAllAgentOpTimeout = prev })
}

// launchProbeMode selects how launchProbeDispatcher behaves in a dispatch.
type launchProbeMode int

func (d *launchProbeDispatcher) probe(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	d.hadDeadline = append(d.hadDeadline, ok)
	var in time.Duration
	if ok {
		in = time.Until(deadline)
	}
	d.deadlineIn = append(d.deadlineIn, in)
	switch d.mode {
	case probeBlock, probeBlockStart:
		if !awaitCanceled(ctx) {
			d.ctxErrs = append(d.ctxErrs, nil)
			return errProbeNeverDone
		}
		d.ctxErrs = append(d.ctxErrs, ctx.Err())
		return ctx.Err()
	default:
		if d.cancelRequest != nil {
			d.cancelRequest()
		}
		err := ctx.Err()
		d.ctxErrs = append(d.ctxErrs, err)
		return err
	}
}

func (d *launchProbeDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.createCalls++
	d.capturedAgent = agent
	if d.s != nil {
		if row, err := d.s.GetAgent(context.Background(), agent.ID); err == nil && row.AppliedConfig != nil {
			d.rowStoragePath = row.AppliedConfig.WorkspaceStoragePath
		}
	}
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	if d.envReqs != nil {
		// The broker asks for env (the gather-env 202 branch).
		return envReqsResult(d.envReqs), nil
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return envReqsResult(nil), nil
}

func (d *launchProbeDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.createCalls++
	d.capturedAgent = agent
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil, nil
}

func (d *launchProbeDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	d.capturedAgent = agent
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	agent.ContainerStatus = "running"
	return nil, nil
}

func (d *launchProbeDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	d.capturedAgent = agent
	if err := d.probe(ctx); err != nil {
		return err
	}
	agent.Phase = string(state.PhaseCreated)
	return nil
}

func (d *launchProbeDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCalls++
	if err := d.probe(ctx); err != nil {
		return err
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil
}

func (d *launchProbeDispatcher) DispatchAgentStop(ctx context.Context, _ *store.Agent) error {
	d.stopCalls++
	if d.mode == probeBlockStart {
		return nil
	}
	return d.probe(ctx)
}

func (failingManagedAgentBackend) Name() string { return "failing-managed" }

func (failingManagedAgentBackend) CreateAgent(_ context.Context, _ managedagent.CreateAgentConfig) (string, error) {
	return "", fmt.Errorf("simulated managed agent create failure")
}

func (failingManagedAgentBackend) DeleteAgent(_ context.Context, _ string) error { return nil }

func (failingManagedAgentBackend) CreateInteraction(_ context.Context, _ managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	return nil, fmt.Errorf("simulated managed agent create failure")
}

func (failingManagedAgentBackend) GetInteraction(_ context.Context, _ string) (*managedagent.InteractionState, error) {
	return nil, fmt.Errorf("not implemented")
}

func (failingManagedAgentBackend) CancelInteraction(_ context.Context, _ string) error { return nil }

func (failingManagedAgentBackend) StreamInteraction(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

func (d *slowLaunchDispatcher) wait(ctx context.Context) error {
	select {
	case <-time.After(d.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *slowLaunchDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if err := d.wait(ctx); err != nil {
		return nil, err
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil, nil
}

func (d *slowLaunchDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	if err := d.wait(ctx); err != nil {
		return nil, err
	}
	agent.ContainerStatus = "running"
	return nil, nil
}

func (d *slowLaunchDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, _ string, _ bool) error {
	if err := d.wait(ctx); err != nil {
		return err
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil
}

func (d *slowLaunchDispatcher) DispatchAgentStop(ctx context.Context, _ *store.Agent) error {
	return d.wait(ctx)
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

func (s *rollbackClaimStore) snapshot() (agentID string, finalizeCalls, deleteCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID, s.finalizeCalls, s.deleteCalls
}

// reservationsHeld records whether each create-time reservation is held for
// an agent ID at some point, so a test can prove the reservations existed
// before asserting the cleanup released them.
type reservationsHeld struct {
	broker, project bool
}

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

func setupSlowLaunchServerWithDelay(t *testing.T, delay time.Duration) (*Server, store.Store, *store.Project) {
	t.Helper()
	shortenSyncDispatchTimeout(t, 3*time.Second)
	require.Greater(t, delay, slowPathWriteTimeout)
	return setupCreateAgentServer(t, &slowLaunchDispatcher{delay: delay})
}

// rollbackClaimTx counts a transaction's DeleteAgent calls on its parent.
type rollbackClaimTx struct {
	store.Store
	parent *rollbackClaimStore
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

func (tx *rollbackClaimTx) DeleteAgent(ctx context.Context, id string) error {
	if tx.parent.fault.Active() {
		tx.parent.noteDelete(id, false)
	}
	return tx.Store.DeleteAgent(ctx, id)
}
