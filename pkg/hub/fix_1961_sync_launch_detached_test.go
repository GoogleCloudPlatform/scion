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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#1961: on the synchronous launch path (create, lifecycle start
// and restart, and a start/resume/restart through create), a client that
// gives up mid-dispatch (the CLI hub client times out at 30s) must not cancel
// the broker launch, and the hub must not roll the agent back as a failed
// create. Each dispatch is instead bounded by syncDispatchTimeout. These
// tests are not parallel: some shorten the package-level timeout.

// launchProbeMode selects how launchProbeDispatcher behaves in a dispatch.
type launchProbeMode int

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

func countCreateCompensations(t *testing.T, s store.Store) int {
	t.Helper()
	failed, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
	require.NoError(t, err)
	return len(failed)
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

func TestSyncLaunch_Create_ClientCancelDuringDispatch_AgentSurvives(t *testing.T) {
	for _, gather := range []bool{false, true} {
		name := "plain"
		if gather {
			name = "gather-env"
		}
		t.Run(name, func(t *testing.T) {
			disp := &launchProbeDispatcher{mode: probeCancelRequest}
			srv, s, project := setupCreateAgentServer(t, disp)
			setAgentQuotaLimits(t, s)

			cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "sync-cancel-" + name, ProjectID: project.ID, Task: "work", GatherEnv: gather,
			})
			disp.cancelRequest = cancel
			rec := serve()

			require.NotNil(t, disp.capturedAgent)
			requireAllLive(t, disp, 1)
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.False(t, disp.deleteCalled, "a client cancel must not delete the runtime")
			assert.Zero(t, countCreateCompensations(t, s), "a client cancel is not a failed create")

			got, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
			require.NoError(t, err, "the agent row must survive the client cancel")
			assert.Equal(t, string(state.PhaseRunning), got.Phase, "the post-dispatch write must land despite the cancel")
			assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, got.ID), "the broker reservation is kept")
		})
	}
}

func TestSyncLaunch_ProvisionOnly_ClientCancelDuringDispatch(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sync-cancel-provision", ProjectID: project.ID, ProvisionOnly: true,
	})
	disp.cancelRequest = cancel
	rec := serve()

	require.NotNil(t, disp.capturedAgent)
	requireAllLive(t, disp, 1)
	assert.Less(t, rec.Code, 300, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "context canceled", "no provision warning from the client cancel")
	got, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseCreated), got.Phase)
}

// A real dispatch failure still rolls the create back exactly as before.
func TestSyncLaunch_Create_RealDispatchFailure_StillCleansUp(t *testing.T) {
	disp := &failingCreateDispatcher{createErr: errors.New("simulated broker failure")}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sync-real-failure", ProjectID: project.ID, Task: "work",
	})
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	require.NotNil(t, disp.capturedAgent)
	assert.True(t, disp.deleteCalled, "the runtime is deleted")
	_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the agent row is deleted")
	assert.Equal(t, 1, countCreateCompensations(t, s), "the rolled-back create is recorded once")
	assertNoReservations(t, s, disp.capturedAgent.ID)
}

// The detached dispatch still honours its own deadline: the dispatch ctx is
// done once syncDispatchTimeout passes (on the control channel that sends
// the broker a cancel frame), and the create is rolled back as a failure.
func TestSyncLaunch_Create_DispatchHonoursOwnTimeout(t *testing.T) {
	shortenSyncDispatchTimeout(t, 50*time.Millisecond)
	disp := &launchProbeDispatcher{mode: probeBlock}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sync-timeout", ProjectID: project.ID, Task: "work",
	})
	require.NotNil(t, disp.capturedAgent)
	require.Len(t, disp.ctxErrs, 1)
	require.ErrorIs(t, disp.ctxErrs[0], context.DeadlineExceeded, "the dispatch ctx must be done at its deadline")
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.True(t, disp.deleteCalled, "the timed-out create deletes its runtime")
	_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 1, countCreateCompensations(t, s))
}

func TestSyncLaunch_LifecycleStart_ClientCancelDuringDispatch(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "sync-cancel-start", state.PhaseStopped, store.RunIntentStopped)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	disp.cancelRequest = cancel
	rec := serve()

	assert.Equal(t, 1, disp.startCalls)
	requireAllLive(t, disp, 1)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the final status write must land despite the cancel")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agent.ID), "the start keeps its reservation")
}

func TestSyncLaunch_LifecycleStart_DispatchHonoursOwnTimeout(t *testing.T) {
	shortenSyncDispatchTimeout(t, 50*time.Millisecond)
	disp := &launchProbeDispatcher{mode: probeBlock}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "sync-timeout-start", state.PhaseStopped, store.RunIntentStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Len(t, disp.ctxErrs, 1)
	require.ErrorIs(t, disp.ctxErrs[0], context.DeadlineExceeded, "the start dispatch ctx must be done at its deadline")
	assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agent.ID), "a timed-out start rolls back its reservation")
}

// The client gives up during the restart's stop leg: the start leg still
// runs, on a live ctx, and the agent ends up running.
func TestSyncLaunch_LifecycleRestart_ClientCancelDuringStopLeg(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "sync-cancel-restart", state.PhaseStopped, store.RunIntentStopped)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
	disp.cancelRequest = cancel
	rec := serve()

	assert.Equal(t, 1, disp.stopCalls)
	assert.Equal(t, 1, disp.startCalls)
	requireAllLive(t, disp, 2)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agent.ID))
}

// A create that starts, resumes or restarts an existing agent
// (handleExistingAgent) follows the same rule.
func TestSyncLaunch_CreateExistingAgent_ClientCancelDuringDispatch(t *testing.T) {
	cases := []struct {
		name  string
		phase state.Phase
		req   CreateAgentRequest
	}{
		{name: "suspended-resume", phase: state.PhaseSuspended},
		{name: "stopped-resume", phase: state.PhaseStopped, req: CreateAgentRequest{Resume: true}},
		{name: "provisioning-start", phase: state.PhaseProvisioning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &launchProbeDispatcher{mode: probeCancelRequest}
			srv, s, project := setupCreateAgentServer(t, disp)
			name := "sync-existing-" + tc.name
			agent := createSiteAgent(t, s, project, name, tc.phase, store.RunIntentStopped)

			req := tc.req
			req.Name, req.ProjectID, req.Task = name, project.ID, "work"
			cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
			disp.cancelRequest = cancel
			rec := serve()

			assert.Equal(t, 1, disp.startCalls)
			requireAllLive(t, disp, 1)
			assert.Less(t, rec.Code, 300, rec.Body.String())
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
		})
	}
}

func TestSyncDispatch_DetachedAndBounded(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "v"))
	detached := detachLaunchFromClient(parent)
	cancel()
	require.NoError(t, detached.Err(), "the detached ctx ignores the parent's cancel")
	assert.Equal(t, "v", detached.Value(key{}), "values are kept")

	shortenSyncDispatchTimeout(t, 20*time.Millisecond)
	var dctxErr error
	err := syncDispatch(detached, func(dctx context.Context) error {
		if !awaitCanceled(dctx) {
			return errProbeNeverDone
		}
		dctxErr = dctx.Err()
		return dctx.Err()
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, dctxErr, context.DeadlineExceeded)
}

// The gather-env 202 branch: the broker asks for env after the client gave
// up; the row stays provisioning for the env submit.
func TestSyncLaunch_Create_GatherEnv202_ClientCancelDuringDispatch(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	disp.envReqs = &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}}
	srv, s, project := setupCreateAgentServer(t, disp)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sync-cancel-gather-202", ProjectID: project.ID, Task: "work", GatherEnv: true,
	})
	disp.cancelRequest = cancel
	rec := serve()

	require.NotNil(t, disp.capturedAgent)
	requireAllLive(t, disp, 1)
	assert.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Zero(t, countCreateCompensations(t, s))
	got, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseProvisioning), got.Phase)
}

// The env submit (POST .../env) is where a CLI create (always GatherEnv)
// often launches: it follows the same rule.
func TestSyncLaunch_SubmitEnv_ClientCancelDuringDispatch(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)
	agent := createSiteAgent(t, s, project, "sync-cancel-env", state.PhaseProvisioning, store.RunIntentRunning)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/env", SubmitEnvRequest{
		Env: map[string]string{"SOME_REQUIRED_KEY": "v"},
	})
	disp.cancelRequest = cancel
	rec := serve()

	requireAllLive(t, disp, 1)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the post-dispatch write must land despite the cancel")
}

// The workspace-bootstrap finalize (POST .../workspace/sync-to/finalize on
// a provisioning agent) dispatches the create: same rule.
func TestSyncLaunch_WorkspaceBootstrapFinalize_ClientCancelDuringDispatch(t *testing.T) {
	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "sync-cancel-bootstrap", state.PhaseProvisioning, store.RunIntentRunning)

	cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize", SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0"},
	})
	disp.cancelRequest = cancel
	rec := serve()

	assert.Equal(t, 1, disp.createCalls)
	requireAllLive(t, disp, 1)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the post-dispatch write must land despite the cancel")
}

// requireFailedStillProvisioning checks a launch of an existing
// provisioning agent that ran past its own deadline: the request fails, and
// the row stays provisioning with no launch in flight, so it can be retried.
func requireFailedStillProvisioning(t *testing.T, s store.Store, disp *launchProbeDispatcher, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	require.NotNil(t, disp.capturedAgent)
	got, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseProvisioning), got.Phase)
	assert.False(t, got.IsInFlight(), "no launch is left in flight")
}

// Own-deadline cases for the remaining sites: each dispatch ctx is done at
// syncDispatchTimeout.
func TestSyncLaunch_OwnDeadline_RemainingSites(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the request to send.
		setup   func(t *testing.T, s store.Store, project *store.Project) (method, path string, body any)
		storage bool // configure workspace storage
		// blockStartOnly uses probeBlockStart instead of probeBlock.
		blockStartOnly bool
		wantCalls      int
		// check runs extra assertions on the outcome.
		check func(t *testing.T, s store.Store, disp *launchProbeDispatcher, rec *httptest.ResponseRecorder)
	}{
		{
			// A stop leg that runs past its own deadline aborts the restart
			// before the start leg (GoogleCloudPlatform/scion#2486).
			name: "restart-stop-leg-timeout-aborts",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				a := createSiteAgent(t, s, project, "own-deadline-restart-stop", state.PhaseRunning, store.RunIntentRunning)
				return http.MethodPost, "/api/v1/agents/" + a.ID + "/restart", nil
			},
			wantCalls: 1,
			check: func(t *testing.T, _ store.Store, disp *launchProbeDispatcher, rec *httptest.ResponseRecorder) {
				assert.Equal(t, 1, disp.stopCalls)
				assert.Equal(t, 0, disp.startCalls, "a stop that timed out must not be followed by a start")
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				assert.Equal(t, restartStopFailedRetryAfter, rec.Header().Get("Retry-After"))
				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, ErrCodeUnavailable, body.Error.Code)
				assert.Empty(t, body.Error.Details, "a ctx error carries no broker code")
			},
		},
		{
			// After a clean stop, the start leg gets its own deadline.
			name: "restart-start-leg-own-deadline",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				a := createSiteAgent(t, s, project, "own-deadline-restart-start", state.PhaseRunning, store.RunIntentRunning)
				return http.MethodPost, "/api/v1/agents/" + a.ID + "/restart", nil
			},
			blockStartOnly: true,
			wantCalls:      1, // the stop leg is not probed in probeBlockStart
			check: func(t *testing.T, _ store.Store, disp *launchProbeDispatcher, rec *httptest.ResponseRecorder) {
				assert.Equal(t, 1, disp.stopCalls)
				assert.Equal(t, 1, disp.startCalls)
				assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
			},
		},
		{
			name: "existing-suspended-resume",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				createSiteAgent(t, s, project, "own-deadline-suspended", state.PhaseSuspended, store.RunIntentStopped)
				return http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "own-deadline-suspended", ProjectID: project.ID, Task: "work"}
			},
			wantCalls: 1,
		},
		{
			name: "existing-stopped-resume",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				createSiteAgent(t, s, project, "own-deadline-stopped", state.PhaseStopped, store.RunIntentStopped)
				return http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "own-deadline-stopped", ProjectID: project.ID, Task: "work", Resume: true}
			},
			wantCalls: 1,
		},
		{
			name: "existing-provisioning-start",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				createSiteAgent(t, s, project, "own-deadline-provisioning", state.PhaseProvisioning, store.RunIntentStopped)
				return http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "own-deadline-provisioning", ProjectID: project.ID, Task: "work"}
			},
			wantCalls: 1,
		},
		{
			name: "provision-only-keeps-row",
			setup: func(t *testing.T, _ store.Store, project *store.Project) (string, string, any) {
				return http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "own-deadline-provision", ProjectID: project.ID, ProvisionOnly: true}
			},
			wantCalls: 1,
			check: func(t *testing.T, s store.Store, disp *launchProbeDispatcher, rec *httptest.ResponseRecorder) {
				assert.Less(t, rec.Code, 300, rec.Body.String())
				require.NotNil(t, disp.capturedAgent)
				_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
				assert.NoError(t, err, "a provision that timed out keeps the row (a warning, as before)")
			},
		},
		{
			name: "submit-env",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				a := createSiteAgent(t, s, project, "own-deadline-env", state.PhaseProvisioning, store.RunIntentRunning)
				return http.MethodPost, "/api/v1/agents/" + a.ID + "/env", SubmitEnvRequest{Env: map[string]string{"K": "v"}}
			},
			wantCalls: 1,
			check:     requireFailedStillProvisioning,
		},
		{
			name: "workspace-bootstrap-finalize",
			setup: func(t *testing.T, s store.Store, project *store.Project) (string, string, any) {
				a := createSiteAgent(t, s, project, "own-deadline-bootstrap", state.PhaseProvisioning, store.RunIntentRunning)
				return http.MethodPost, "/api/v1/agents/" + a.ID + "/workspace/sync-to/finalize", SyncToFinalizeRequest{Manifest: &transfer.Manifest{Version: "1.0"}}
			},
			storage:   true,
			wantCalls: 1,
			check:     requireFailedStillProvisioning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortenSyncDispatchTimeout(t, 50*time.Millisecond)
			disp := &launchProbeDispatcher{mode: probeBlock}
			if tc.blockStartOnly {
				disp.mode = probeBlockStart
			}
			srv, s, project := setupCreateAgentServer(t, disp)
			if tc.storage {
				srv.SetStorage(newContentMockStorage("test-bucket"))
			}
			method, path, body := tc.setup(t, s, project)
			rec := doRequest(t, srv, method, path, body)

			require.Len(t, disp.ctxErrs, tc.wantCalls, rec.Body.String())
			for i, err := range disp.ctxErrs {
				assert.ErrorIs(t, err, context.DeadlineExceeded, "dispatch %d ctx must be done at its deadline", i)
			}
			if tc.check != nil {
				tc.check(t, s, disp, rec)
			}
		})
	}
}

// The hub-managed workspace upload runs detached from the client under its
// own budget: a client that gives up mid-upload does not leave the launch
// to go ahead with the unswapped hub-local workspace. A failing upload is
// still only logged, as before, and the create goes ahead.
func TestSyncLaunch_HubManagedWorkspaceUpload_ClientCancel(t *testing.T) {
	for _, uploadFails := range []bool{false, true} {
		name := "upload-succeeds"
		if uploadFails {
			name = "upload-fails"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			disp := &launchProbeDispatcher{mode: probeCancelRequest}
			srv, s, project := setupCreateAgentServer(t, disp) // hub-managed: no GitRemote.
			disp.s = s
			srv.SetStorage(newContentMockStorage("test-bucket"))
			t.Cleanup(func() {
				if p, err := hubManagedProjectPath(project.Slug); err == nil {
					_ = os.RemoveAll(p)
				}
			})

			cancel, serve := newCancelableRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "sync-cancel-upload-" + name, ProjectID: project.ID, Task: "work",
			})
			var upload ctxObservation
			prev := syncToGCSForWorkspaceUpload
			syncToGCSForWorkspaceUpload = func(uctx context.Context, _, _, _ string) error {
				cancel()
				upload = observeCtx(uctx)
				if uploadFails {
					return errors.New("simulated upload failure")
				}
				return nil
			}
			t.Cleanup(func() { syncToGCSForWorkspaceUpload = prev })
			// The probe dispatcher must not cancel again; the upload did.
			disp.cancelRequest = nil
			rec := serve()

			require.True(t, upload.called, "fixture check: the upload branch must be reached")
			assert.NoError(t, upload.err, "the upload must not follow the canceled request")
			assert.True(t, upload.hasDeadline, "the upload runs under its own budget")
			assert.Greater(t, upload.budget, time.Minute, "the upload budget is generous")

			require.NotNil(t, disp.capturedAgent, "the launch goes ahead")
			requireAllLive(t, disp, 1)
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			got, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
			require.NoError(t, err)
			require.NotNil(t, got.AppliedConfig)
			if uploadFails {
				assert.Empty(t, got.AppliedConfig.WorkspaceStoragePath, "a failed upload leaves the workspace unswapped, as before")
			} else {
				// Read at dispatch time: the post-dispatch write rewrites
				// AppliedConfig, so the row after the create cannot show
				// whether the swap write itself landed.
				assert.NotEmpty(t, disp.rowStoragePath, "the swap write must land despite the cancel")
				assert.NotEmpty(t, disp.capturedAgent.AppliedConfig.WorkspaceStoragePath, "the launch uses the uploaded workspace")
			}
		})
	}
}

// An upload that runs past our own budget fails the create through the
// cleanup: nothing is launched with the hub-local workspace.
func TestSyncLaunch_HubManagedWorkspaceUpload_OwnBudgetExpired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	prevBudget := hubWorkspaceUploadTimeout
	hubWorkspaceUploadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hubWorkspaceUploadTimeout = prevBudget })

	disp := &launchProbeDispatcher{mode: probeCancelRequest}
	srv, s, project := setupCreateAgentServer(t, disp) // hub-managed: no GitRemote.
	srv.SetStorage(newContentMockStorage("test-bucket"))
	setAgentQuotaLimits(t, s)
	t.Cleanup(func() {
		if p, err := hubManagedProjectPath(project.Slug); err == nil {
			_ = os.RemoveAll(p)
		}
	})

	var uploadCalled bool
	prev := syncToGCSForWorkspaceUpload
	syncToGCSForWorkspaceUpload = func(uctx context.Context, _, _, _ string) error {
		uploadCalled = true
		if !awaitCanceled(uctx) {
			return errProbeNeverDone
		}
		return uctx.Err()
	}
	t.Cleanup(func() { syncToGCSForWorkspaceUpload = prev })

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "upload-budget-expired", ProjectID: project.ID, Task: "work",
	})
	require.True(t, uploadCalled, "fixture check: the upload branch must be reached")
	assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.Zero(t, disp.createCalls, "nothing is launched after the upload budget expired")

	result, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "the create is rolled back")
	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID), "the broker reservation is released")

	failed, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
	require.NoError(t, err)
	require.Len(t, failed, 1)
	var sum compensationSummary
	require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
	assert.Equal(t, createStageWorkspaceUpload, sum.Stage)
}
