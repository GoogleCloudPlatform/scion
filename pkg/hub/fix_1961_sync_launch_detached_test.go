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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
)

// errProbeNeverDone is returned by a probeBlock dispatch whose ctx was not
// done within requestCancelWait: the dispatch had no deadline.
var errProbeNeverDone = errors.New("dispatch ctx never done")

// launchProbeDispatcher records the ctx each launch dispatch ran on.
type launchProbeDispatcher struct {
	createAgentDispatcher
	mode          launchProbeMode
	cancelRequest context.CancelFunc

	createCalls int
	startCalls  int
	stopCalls   int
	// ctxErrs holds, per dispatch in call order, the dispatch ctx's error
	// once the probe acted (nil: live).
	ctxErrs []error
	// hadDeadline records, per dispatch, whether its ctx carried a deadline.
	hadDeadline []bool
}

func (d *launchProbeDispatcher) probe(ctx context.Context) error {
	_, ok := ctx.Deadline()
	d.hadDeadline = append(d.hadDeadline, ok)
	switch d.mode {
	case probeBlock:
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
	if err := d.probe(ctx); err != nil {
		return nil, err
	}
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return envReqsResult(nil), nil
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
		assert.True(t, disp.hadDeadline[i], "dispatch %d must run under syncDispatchTimeout", i)
	}
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
