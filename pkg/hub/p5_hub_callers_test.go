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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2550 P5: the remaining hub callers send the run
// they were dispatched for, and every hub delete caller honours the
// broker's run-mismatch refusal (ptone/scion#3080).

// flipDeleteRunMismatchPolicy makes refuseDeleteRunMismatch answer refuse
// for the test's duration. Tests calling it must not be parallel.
func flipDeleteRunMismatchPolicy(t *testing.T, refuse bool) {
	t.Helper()
	prev := deleteRunMismatchPolicy
	deleteRunMismatchPolicy = func(bool, bool) bool { return refuse }
	t.Cleanup(func() { deleteRunMismatchPolicy = prev })
}

// execDeleteFixture is an agent whose row records run-b, after run-p1 and
// run-p2, on a broker whose deletes are recorded.
func execDeleteFixture(t *testing.T, suffix string) (*Server, store.Store, *fenceRecordingClient, *store.Agent) {
	t.Helper()
	srv, s := testServer(t)
	client := &fenceRecordingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseRunning)
	ctx := context.Background()
	for _, run := range []string{"run-p1", "run-p2", "run-b"} {
		_, err := s.SetAgentRunID(ctx, agent.ID, run)
		require.NoError(t, err)
	}
	row := mustGetAgent(t, s, agent.ID)
	require.Equal(t, "run-b", row.RunID)
	require.Equal(t, []string{"run-p1", "run-p2"}, row.PreviousRunIDs)
	return srv, s, client, row
}

// A delete intent sends the runs captured when it was written, not the
// row's runs when it is executed: an intent written for run-a deletes
// run-a and the previous runs it lists (even none), and never run-b,
// which the row records now. An intent with no run ID (an older hub, or
// an agent with no run ID) sends the row's runs, as before.
func TestExecDispatchDelete_SendsIntentRuns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     DeleteDispatchArgs
		wantRuns []string
	}{
		{"intent run and previous runs", DeleteDispatchArgs{RunID: "run-a", PreviousRunIDs: []string{"run-old"}}, []string{"run-a", "run-old"}},
		{"intent run with no previous runs", DeleteDispatchArgs{RunID: "run-a"}, []string{"run-a"}},
		{"legacy intent: the row's runs", DeleteDispatchArgs{}, []string{"run-b", "run-p2", "run-p1"}},
		{"legacy intent listing previous runs", DeleteDispatchArgs{PreviousRunIDs: []string{"run-old"}}, []string{"run-b", "run-old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, client, agent := execDeleteFixture(t, "p5-exec-"+uuid.NewString()[:8])
			raw, err := MarshalDispatchArgs(&tc.args)
			require.NoError(t, err)
			_, err = srv.execDispatchDelete(context.Background(), store.BrokerDispatch{ID: "d1", AgentID: agent.ID, Op: brokerDispatchOpDelete, Args: raw})
			require.NoError(t, err)
			assert.Equal(t, tc.wantRuns, runsOf(client.sent()))
		})
	}
}

// An engine intent naming its run is fenced as before: every delete it
// sends (its run and each previous run it lists) carries the notAfter the
// executing node computes, and a stale intent sends none.
func TestExecDispatchDelete_IntentRunsFenced(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current claim", true: "stale claim"}[stale], func(t *testing.T) {
			fenceNow(t)
			srv, s, client, agent := execDeleteFixture(t, "p5-fence-"+uuid.NewString()[:8])
			seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
			var claimAdj int64
			if stale {
				seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
				claimAdj = -1
			}
			row := mustGetAgent(t, s, agent.ID)
			raw, err := MarshalDispatchArgs(&DeleteDispatchArgs{
				Claim: row.DeletionClaim + claimAdj, RunID: "run-a", PreviousRunIDs: []string{"run-old", "run-prev"},
			})
			require.NoError(t, err)
			_, execErr := srv.execDispatchDelete(context.Background(), store.BrokerDispatch{ID: "d1", AgentID: agent.ID, Op: brokerDispatchOpDelete, Args: raw})
			if stale {
				require.ErrorIs(t, execErr, errStaleDeleteDispatch)
				assert.Empty(t, client.sent(), "a stale intent reached the broker")
				return
			}
			require.NoError(t, execErr)
			sent := client.sent()
			require.Equal(t, []string{"run-a", "run-prev", "run-old"}, runsOf(sent))
			want := row.DeletionLeaseAt.Add(-deleteNotAfterMargin)
			for _, o := range sent {
				assert.True(t, o.NotAfter.Equal(want), "run %s: notAfter = %v, want %v", o.RunID, o.NotAfter, want)
			}
		})
	}
}

// The originating node writes the agent's run and previous runs into the
// delete intent, and the owning node, executing it after the row moved on
// to run-b, deletes run-a: the run captured at write time is the one
// dispatched.
func TestDeferredDelete_IntentCapturesRunAtWrite(t *testing.T) {
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	remoteBroker := uuid.NewString()
	events := NewChannelEventPublisher()
	defer events.Close()
	originator := NewHTTPAgentDispatcherWithClient(cs, &deferredDataOpTestClient{localBroker: "local-broker"}, false, slog.Default())
	originator.SetCrossNodeDeps(events, NoopCommandBus{})

	agent := seedAgentWithBrokerID(t, cs, remoteBroker)
	_, err := cs.SetAgentRunID(ctx, agent.ID, "run-p")
	require.NoError(t, err)
	_, err = cs.SetAgentRunID(ctx, agent.ID, "run-a")
	require.NoError(t, err)
	snapshot := mustGetAgent(t, cs, agent.ID)

	written := make(chan store.BrokerDispatch, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			pending, err := cs.ListPendingDispatch(ctx, remoteBroker)
			if err == nil && len(pending) > 0 {
				d := pending[0]
				written <- d
				_, _ = cs.ClaimBrokerDispatch(ctx, d.ID, "owner-hub")
				_ = cs.CompleteBrokerDispatch(ctx, d.ID, "")
				events.PublishDispatchDone(ctx, d.ID)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	require.NoError(t, originator.DispatchAgentDelete(ctx, snapshot, true, false, false, time.Time{}))
	var d store.BrokerDispatch
	select {
	case d = <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("no delete intent was written")
	}

	var args DeleteDispatchArgs
	require.NoError(t, json.Unmarshal([]byte(d.Args), &args))
	assert.Equal(t, "run-a", args.RunID, "the intent records the run it was written for")
	assert.Equal(t, []string{"run-p"}, args.PreviousRunIDs)

	// Run-b starts before the owning node executes the intent.
	_, err = cs.SetAgentRunID(ctx, agent.ID, "run-b")
	require.NoError(t, err)
	owner, _ := testServerWithStore(t, cs)
	client := &fenceRecordingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	owner.SetDispatcher(NewHTTPAgentDispatcherWithClient(cs, client, false, slog.Default()))
	d.BrokerID = remoteBroker
	_, err = owner.execDispatchDelete(ctx, d)
	require.NoError(t, err)
	assert.Equal(t, []string{"run-a", "run-p"}, runsOf(client.sent()), "run-b is never named")
}

// A failed create's cleanup deletes the run its row records when the
// cleanup runs (a cross-node create mints its run on the owning node, so
// the caller's copy can name another), with the row's previous runs. A
// row with no run deletes by name, as before; with no row left, the
// caller's copy is used.
func TestDispatchDeleteFailedCreate_SendsRowRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rowRuns  []string // SetAgentRunID calls, in order; none leaves the run ""
		gone     bool
		wantRuns []string
	}{
		{"row's run, not the caller's", []string{"run-owner"}, false, []string{"run-owner"}},
		{"row's previous runs too", []string{"run-prev", "run-owner"}, false, []string{"run-owner", "run-prev"}},
		{"row with no run: by name", nil, false, []string{""}},
		{"row gone: the caller's copy", []string{"run-owner"}, true, []string{"run-local"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := testServer(t)
			client := &fenceRecordingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
			d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
			agent := setupBrokerAgentInPhase(t, s, "p5-createfail-"+uuid.NewString()[:8], state.PhaseProvisioning)
			ctx := context.Background()
			for _, run := range tc.rowRuns {
				_, err := s.SetAgentRunID(ctx, agent.ID, run)
				require.NoError(t, err)
			}
			local := *mustGetAgent(t, s, agent.ID)
			local.RunID = "run-local"
			local.PreviousRunIDs = nil
			if tc.gone {
				require.NoError(t, s.DeleteAgent(ctx, agent.ID))
			}
			require.NoError(t, dispatchDeleteFailedCreate(s, d, &local)(ctx))
			assert.Equal(t, tc.wantRuns, runsOf(client.sent()))
			assert.Equal(t, "run-local", local.RunID, "the caller's copy is not modified")
		})
	}
}

// envGatherRefusingClient refuses every delete with the broker's
// run-mismatch answer once refuse is set (ptone/scion#3080).
type envGatherRefusingClient struct {
	*envGatherMockBrokerClient
	refuse bool
}

func (c *envGatherRefusingClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	if !c.refuse {
		return nil
	}
	return &DeleteRunMismatchError{RequestedRunID: opts.RunID, CurrentRunID: "run-other", Err: ErrDeleteRunMismatch}
}

// Env-gather recreate honours the broker's refusal through
// refuseDeleteRunMismatch: with the policy refusing (today's), the
// provisioning row is kept and the request answers 409 conflict, in strict
// and force cleanup modes (strict answered 502 before P5). With the policy
// flipped, force removes the row and recreates, and strict keeps today's
// cleanup-failure answer.
func TestEnvGatherRecreate_RunMismatchRefusal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cleanupMode string
		refuse      bool
		wantCode    int
		wantKept    bool
	}{
		{"strict, policy refuses", "", true, http.StatusConflict, true},
		{"force, policy refuses", "force", true, http.StatusConflict, true},
		{"strict, policy flipped", "", false, http.StatusBadGateway, true},
		{"force, policy flipped", "force", false, http.StatusAccepted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flipDeleteRunMismatchPolicy(t, tc.refuse)
			srv, st := testServer(t)
			ctx := context.Background()
			suffix := uuid.NewString()[:8]
			project := &store.Project{ID: tid("p5-eg-project-" + suffix), Name: "p5-eg-" + suffix, Slug: "p5-eg-" + suffix}
			require.NoError(t, st.CreateProject(ctx, project))
			broker := &store.RuntimeBroker{ID: tid("p5-eg-broker-" + suffix), Name: "p5-eg-broker-" + suffix, Slug: "p5-eg-broker-" + suffix,
				Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
			require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
			require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: broker.ID, BrokerName: "test-broker", LocalPath: "/tmp/test-project",
			}))
			client := &envGatherRefusingClient{envGatherMockBrokerClient: &envGatherMockBrokerClient{
				gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{AgentID: "x", Required: []string{"GEMINI_API_KEY"}, Needs: []string{"GEMINI_API_KEY"}},
			}}
			d := NewHTTPAgentDispatcherWithClient(st, client, true, slog.Default())
			d.SetTokenGenerator(&fakeMintingTokenGenerator{store: st})
			srv.SetDispatcher(d)

			body := map[string]interface{}{"name": "p5-eg-agent", "projectId": project.ID, "template": "claude", "gatherEnv": true}
			rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
			require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
			var resp1 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
			oldID := resp1.Agent.ID

			client.refuse = true
			if tc.cleanupMode != "" {
				body["cleanupMode"] = tc.cleanupMode
			}
			rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
			require.Equal(t, tc.wantCode, rec2.Code, rec2.Body.String())
			if tc.wantCode == http.StatusConflict {
				code, _ := errorBody(t, rec2)
				assert.Equal(t, ErrCodeConflict, code)
				assert.Contains(t, rec2.Body.String(), "holds run run-other of this agent")
			}
			_, err := st.GetAgent(ctx, oldID)
			if tc.wantKept {
				require.NoError(t, err, "the provisioning row was removed after the refusal")
			} else {
				require.ErrorIs(t, err, store.ErrNotFound)
			}
		})
	}
}

// The delete engine follows the flipped policy too: with force, a refused
// delete then finalizes, as for any other dispatch error under force.
func TestAgentDelete_RunMismatchPolicyFlipped_ForceFinalizes(t *testing.T) {
	flipDeleteRunMismatchPolicy(t, false)
	f := newRunMismatchFixture(t, "p5-rm-flip", state.PhaseRunning)
	f.client.answer = func(runID string) error { return runMismatchEnvelope(t, runID, "run-b", true) }
	r := f.del(t, "?force=true")
	require.Less(t, r.rec.Code, 300, r.rec.Body.String())
	got, err := f.store.GetAgent(context.Background(), f.agent.ID)
	if err == nil {
		assert.False(t, got.DeletedAt.IsZero(), "the row was finalized")
	}
}

// mintAfterEmptySwapStore is the dispatcher's store: when the failed start
// leg's compare-and-swap moves the row from the minted run to "" (the broker
// reported no single current run), another caller mints run-other right
// after, before the restart records anything.
type mintAfterEmptySwapStore struct {
	store.Store
}

func (s mintAfterEmptySwapStore) CompareAndSwapAgentRunID(ctx context.Context, agentID, from, to string) (bool, error) {
	swapped, err := s.Store.CompareAndSwapAgentRunID(ctx, agentID, from, to)
	if err == nil && swapped && to == "" {
		if _, err := s.SetAgentRunID(ctx, agentID, "run-other"); err != nil {
			return swapped, err
		}
	}
	return swapped, err
}

// A restart whose start leg fails with the broker reporting an empty
// current run (review N-a of GoogleCloudPlatform/scion#2506) leaves the run
// "" on the restart's copy. It records nothing then: no stopped status, no
// quota release, so a run another caller minted meanwhile is not recorded
// stopped and keeps its reservation. The heartbeat settles the agent.
func TestRestartStartLegFailureWithEmptyCurrentRunRecordsNothing(t *testing.T) {
	srv, s, agent := restartQuotaAgent(t, "p5-restart-empty-run")
	mockClient := &mockRuntimeBrokerClient{}
	startErr := brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttemptedAt("", ""))
	d := NewHTTPAgentDispatcherWithClient(mintAfterEmptySwapStore{s}, startErrClient{mockClient, startErr}, false, slog.Default())
	d.SetTokenGenerator(staticTokenGenerator{token: "test-token"})
	srv.SetDispatcher(d)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	require.True(t, mockClient.stopCalled, "the stop leg was not dispatched")
	got := mustGetAgent(t, s, agent.ID)
	require.Equal(t, "run-other", got.RunID)
	assert.NotEqual(t, string(state.PhaseStopped), got.Phase, "the other caller's run was recorded stopped")
	assert.NotEqual(t, "stopped", got.ContainerStatus, "the other caller's run was recorded stopped")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, agent.RuntimeBrokerID), "the reservation is held for the other run")
}

func TestRestartStoppedRecordable(t *testing.T) {
	assert.False(t, restartStoppedRecordable(""))
	assert.True(t, restartStoppedRecordable("run-a"))
}

// A failed create whose cleanup the broker refuses because it holds a
// different run (ptone/scion#3080) keeps its row (ptone's ruling, P5 Q1):
// no compensation, the row in phase error naming both runs, its quotas
// held, and the refusal logged with both runs. A cleanup failing any other
// way still removes the row and releases the quotas, as before.
func TestCleanupFailedCreate_RunMismatchKeepsRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantKept bool
	}{
		{"refused", &DeleteRunMismatchError{RequestedRunID: "run-mine", CurrentRunID: "run-other", Err: ErrDeleteRunMismatch}, true},
		{"other failure", errors.New("broker unreachable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			var logs bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logs, nil))
			setBrokerAgentCeiling(t, s, 5)
			agent := setupBrokerAgentInPhase(t, s, "p5-cleanup-"+uuid.NewString()[:8], state.PhaseProvisioning)
			ctx := context.Background()
			broker, err := s.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
			require.NoError(t, err)
			reserveBrokerSlot(t, s, broker, agent.ID)

			corrID := srv.cleanupFailedCreate(ctx, createRollback{
				Agent:           agent,
				RuntimeBrokerID: agent.RuntimeBrokerID,
				Stage:           createStageDispatch,
				Cause:           errors.New("dispatch failed"),
				DeleteRuntime:   func(context.Context) error { return tc.err },
			})
			assert.Empty(t, corrID)

			got, err := s.GetAgent(ctx, agent.ID)
			if !tc.wantKept {
				require.ErrorIs(t, err, store.ErrNotFound, "the row is removed")
				assert.EqualValues(t, 0, brokerReservationCount(t, s, agent.RuntimeBrokerID), "the reservation is released")
				return
			}
			require.NoError(t, err, "the row was removed after the refusal")
			assert.Equal(t, string(state.PhaseError), got.Phase)
			assert.Contains(t, got.Message, "holds run run-other of this agent, not run run-mine")
			assert.EqualValues(t, 1, brokerReservationCount(t, s, agent.RuntimeBrokerID), "the reservation is held")
			assert.Contains(t, logs.String(), "hub_run_id=run-mine")
			assert.Contains(t, logs.String(), "broker_run_id=run-other")
		})
	}
}
