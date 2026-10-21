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

// Per-instance capacity, design P3.3-b §4.4 (ptone/scion#3279): a slot is
// not freed while a start owns it, a slot whose release was skipped under a
// claim is freed when the claim clears, and the backstop re-asserts write
// only when a release slipped through an unguarded path.

package hub

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// capacityCounters wires the capacity counters on srv and returns readers
// of the backstop count for a site and of the deregistered-scope count.
func capacityCounters(t *testing.T, srv *Server) (backstop func(site string) int64, deregistered func() int64) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelQuotaCapacityMetrics(mp)
	require.NoError(t, err)
	srv.SetQuotaCapacityMetrics(rec)
	collect := func(name, site string) int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != name {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					if site == "" {
						total += dp.Value
						continue
					}
					if v, ok := dp.Attributes.Value("site"); ok && v.AsString() == site {
						total += dp.Value
					}
				}
			}
		}
		return total
	}
	return func(site string) int64 { return collect("scion.hub.quota.backstop_reassert", site) },
		func() int64 { return collect("scion.hub.quota.deregistered_scope_released", "") }
}

// backstopCounter wires the capacity counters on srv and returns a reader of
// the backstop count for a site.
func backstopCounter(t *testing.T, srv *Server) func(site string) int64 {
	t.Helper()
	backstop, _ := capacityCounters(t, srv)
	return backstop
}

// capacityAudits returns the mutation audit records of mutationType on
// agentID.
func capacityAudits(t *testing.T, s store.Store, mutationType, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationType, TargetType: "agent", TargetID: agentID,
	})
	require.NoError(t, err)
	return recs
}

// brokerLimitDefID returns the max_agents_per_broker limit definition ID.
func brokerLimitDefID(t *testing.T, s store.Store) string {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	return def.ID
}

// seedSlot records agentID's reservation on brokerID and returns it.
func seedSlot(t *testing.T, s store.Store, brokerID, agentID string) *store.UsageReservation {
	t.Helper()
	return seedBrokerRow(t, s, brokerLimitDefID(t, s), brokerID, agentID, time.Time{})
}

// selfReport posts agent's own status report with phase through the real
// status endpoint, with an agent token, as sciontool does.
func selfReport(t *testing.T, srv *Server, agent *store.Agent, phase state.Phase) {
	t.Helper()
	rec := postAgentStatusAsAgent(t, srv, agent, `{"phase":"`+string(phase)+`"}`)
	require.Less(t, rec.Code, 300, rec.Body.String())
}

// An agent with no claim exits; the slot is free before any sweep runs.
// Driven through the real agent status endpoint, which reconciles before it
// persists the phase, so a release keyed on the stored row's countedness
// would wait for the sweep.
func TestObservedExit_NoClaim_ReleasesImmediately_SelfReport(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "oe-self")
	a := newQuotaTestAgent(t, s, broker, project, "oe-self", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)

	selfReport(t, srv, a, state.PhaseError)

	assert.Equal(t, string(state.PhaseError), getAgent(t, s, a.ID).Phase)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "released at once, no sweep")
}

// The same through the real broker heartbeat handler, which also reconciles
// before it writes the phase.
func TestObservedExit_NoClaim_ReleasesImmediately_Heartbeat(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("oe-hb", "running", "working")
	seedSlot(t, f.s, f.brokerID, a.ID)

	f.send(brokerHeartbeatRequest{
		Status: store.BrokerStatusOnline,
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "stopped", ContainerStatus: "Exited (0)", RuntimeTarget: "docker"},
		}}},
	})

	assert.Equal(t, string(state.PhaseStopped), getAgent(t, f.s, a.ID).Phase)
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "released at once, no sweep")
}

// exitHookDispatcher's stop runs onStop (under the caller's stop claim)
// before it returns.
type exitHookDispatcher struct {
	claimTestDispatcher
	onStop func(ctx context.Context, a *store.Agent)
}

func (d *exitHookDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	if d.onStop != nil {
		d.onStop(ctx, a)
	}
	return d.claimTestDispatcher.DispatchAgentStop(ctx, a)
}

// A stop-kind claim blocks every start, so an exit observed while one is
// held releases at once: the intent backstop stop and the compensating stop
// both hold one while their stop runs.
func TestObservedExit_UnderStopClaim_ReleasesWithoutSweep(t *testing.T) {
	for _, path := range []string{"intent-backstop", "compensating"} {
		t.Run(path, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			broker, project := newQuotaTestBrokerAndProject(t, s, "oe-stop-"+path)
			a := newQuotaTestAgent(t, s, broker, project, "oe-stop-"+path, state.PhaseRunning)
			seedSlot(t, s, broker.ID, a.ID)
			_, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
			require.NoError(t, err)

			var stopped atomic.Bool
			var claimKind store.StartClaimKind
			var freedUnderClaim bool
			d := &exitHookDispatcher{onStop: func(ctx context.Context, cur *store.Agent) {
				claimKind = getAgent(t, s, a.ID).StartClaimKind
				selfReport(t, srv, a, state.PhaseStopped)
				freedUnderClaim = !hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID)
				stopped.Store(true)
			}}
			srv.SetDispatcher(d)

			cur := getAgent(t, s, a.ID)
			if path == "intent-backstop" {
				srv.stopForStoppedIntent(ctx, *cur)
			} else {
				srv.compensatingStop(ctx, cur, store.StartClaim{At: time.Now().Add(-time.Hour)})
			}

			require.True(t, stopped.Load(), "the stop ran")
			assert.Equal(t, store.StartClaimStop, claimKind, "the stop ran under a stop-kind claim")
			assert.True(t, freedUnderClaim, "the observed exit released while the stop claim was held")
		})
	}
}

// Merge condition 4 (k8s-runtime-lead): an exit observed while a start
// claim is held frees nothing; once the claim is released by its holder's
// settle (finish), the slot frees through releaseSlotIfIdle, with no sweep.
func TestClaimHeld_ReleaseSkipped_ThenClaimCleared_SlotFrees(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "claim-held")
	a := newQuotaTestAgent(t, s, broker, project, "claim-held", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)

	run, err := srv.acquireStartClaim(ctx, a, store.StartClaimUser)
	require.NoError(t, err)
	selfReport(t, srv, a, state.PhaseError)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "skipped while the start claim is held")

	run.finish(startReleased)
	assert.Empty(t, getAgent(t, s, a.ID).StartClaimID)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "freed when the claim cleared")
}

// A running agent keeps its slot when its claim clears: the hook releases
// only an agent that holds no capacity.
func TestClaimCleared_RunningAgent_SlotKept(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "claim-run")
	a := newQuotaTestAgent(t, s, broker, project, "claim-run", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)

	run, err := srv.acquireStartClaim(ctx, a, store.StartClaimUser)
	require.NoError(t, err)
	run.finish(startReleased)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// A start that fails without acting releases its claim; a hold the dispatch
// did not create (the rollback keeps it) is freed by releaseSlotIfIdle once
// the agent is back at rest.
func TestStartFailure_ClaimCleared_SlotFrees(t *testing.T) {
	f, d, a := newClaimFixture(t) // a is stopped
	seedSlot(t, f.s, f.brokerID, a.ID)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		return errBrokerLacksEmptyPerAgent // a refusal confirmed not acted on
	}

	err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser})
	require.Error(t, err)

	got := getAgent(t, f.s, a.ID)
	assert.Empty(t, got.StartClaimID, "the claim is released")
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed when the claim cleared")
}

// syncExitCreateDispatcher answers a create synchronously; during the
// dispatch, the new container's own report of an exit (phase error) lands.
type syncExitCreateDispatcher struct {
	createAgentDispatcher
	t   *testing.T
	srv *Server
	// reservedDuring is whether the slot was still held right after the
	// exit report, inside the dispatch (the create claim is held).
	reservedDuring bool
}

func (d *syncExitCreateDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	cur := getAgent(d.t, d.srv.store, agent.ID)
	selfReport(d.t, d.srv, cur, state.PhaseError)
	d.reservedDuring = hasReservation(d.t, d.srv.store, store.LimitMaxAgentsPerBroker, agent.ID)
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
}

func (d *syncExitCreateDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if _, err := d.DispatchAgentCreate(ctx, agent); err != nil {
		return nil, err
	}
	return envReqsResult(d.envReqs), nil
}

// R6-B1 (binding): a synchronous create whose container exits during the
// dispatch skips the observed release under the create claim and keeps the
// terminal phase; the slot is freed after the create's claim settles with a
// success outcome, with no sweep.
func TestSyncCreate_ExitObservedDuringDispatch_SlotFreed(t *testing.T) {
	disp := &syncExitCreateDispatcher{t: t}
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.srv = srv

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sync-exit", ProjectID: project.ID, Task: "do something",
	})
	require.Less(t, rec.Code, 300, rec.Body.String())
	require.NotNil(t, disp.capturedAgent, "the create reached the dispatch")
	id := disp.capturedAgent.ID

	assert.True(t, disp.reservedDuring, "the exit observed during the dispatch is skipped under the create claim")
	got := getAgent(t, s, id)
	assert.Equal(t, string(state.PhaseError), got.Phase, "the terminal phase is kept")
	assert.Empty(t, got.StartClaimID)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, id), "the slot is freed after the create returns")
}

// launchClaimedAgent creates a counted agent of f's broker with its slot,
// a create claim and an active launch linked to that claim, as an async
// create leaves it: phase provisioning, the only phases BeginLaunch accepts
// being created and provisioning. It returns the agent and the launch ID.
func launchClaimedAgent(t *testing.T, f *reconcileFixture, slug string) (*store.Agent, string) {
	t.Helper()
	ctx := context.Background()
	a := f.addAgent(slug, string(state.PhaseProvisioning), "")
	seedSlot(t, f.s, f.brokerID, a.ID)
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-x", store.StartClaimCreate, "", time.Minute)
	require.NoError(t, err)
	launchID, err := f.s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	got := getAgent(t, f.s, a.ID)
	require.Equal(t, launchID, got.StartClaimLaunchID, "the launch is linked to the create claim")
	return got, launchID
}

// Launch-end inventory L1-L11 (design §4.4): an exit observed during an
// async launch is skipped under the create claim; the launch-ending report
// then frees the slot through the launch-report hook, or inside its own
// write, and keeps it for a running agent.
func TestLaunchEnd_StoppedPhase_FailedReport_HookReleases(t *testing.T) {
	f := newReconcileFixture(t)
	a, launchID := launchClaimedAgent(t, f, "le-l3")
	selfReport(t, f.srv, a, state.PhaseStopped)
	require.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "skipped under the create claim")

	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), store.LaunchReportCodeStaleLaunch)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the launch end released the claim")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed right after the report")
}

func TestLaunchEnd_ErrorPhase_NonFailedReport_HookReleases(t *testing.T) {
	f := newReconcileFixture(t)
	a, launchID := launchClaimedAgent(t, f, "le-l4b")
	selfReport(t, f.srv, a, state.PhaseError)
	require.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "skipped under the create claim")

	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateProgress, Step: "cloning",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed right after the report")
}

func TestLaunchEnd_ErrorPhase_FailedReport_WriteReleases(t *testing.T) {
	f := newReconcileFixture(t)
	a, launchID := launchClaimedAgent(t, f, "le-l4a")
	selfReport(t, f.srv, a, state.PhaseError)
	require.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID))

	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "released inside the write")
}

func TestLaunchEnd_RunningPhase_SucceededReport_SlotKept(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "pre-running (L5)"
		if running {
			name = "running (L1, L11)"
		}
		t.Run(name, func(t *testing.T) {
			f := newReconcileFixture(t)
			a, launchID := launchClaimedAgent(t, f, "le-run-"+strings.Fields(name)[0])
			if running {
				selfReport(t, f.srv, a, state.PhaseRunning)
			}
			rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
				LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateSucceeded,
			})
			require.Less(t, rec.Code, 300, rec.Body.String())
			assert.Equal(t, string(state.PhaseRunning), getAgent(t, f.s, a.ID).Phase)
			assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "a running agent keeps its slot")
		})
	}
}

// L9: the launch reaper ends a wind-down launch (phase stopped) as lost and
// demotes the create claim, so the slot is still held right after its
// write; it is freed at the start-claim reaper's release, once a fresh
// complete inventory shows nothing running.
func TestLaunchEnd_WindDownLost_ReleasedAtReaperRelease(t *testing.T) {
	f := newReconcileFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	a, _ := launchClaimedAgent(t, f, "le-l9")
	selfReport(t, f.srv, a, state.PhaseStopped)

	params := store.ReaperParams{KeepaliveInterval: time.Millisecond, ReaperInterval: time.Minute}
	_, err := f.s.RunLaunchReaperTick(ctx, params) // arms the reaper
	require.NoError(t, err)
	time.Sleep(30 * time.Millisecond)
	_, err = f.s.RunLaunchReaperTick(ctx, params)
	require.NoError(t, err)

	got := getAgent(t, f.s, a.ID)
	require.Equal(t, store.LaunchStateEnded, got.LaunchState, "the reaper ended the wind-down launch")
	require.Equal(t, store.LaunchEndReasonLost, got.LaunchEndReason)
	require.NotEmpty(t, got.StartClaimID, "the claim is demoted, not released")
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "held right after the launch reaper's write")

	f.heartbeat(completeInventory()) // the container is absent
	f.srv.reapStartClaims(ctx)

	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the start-claim reaper released the claim")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed at the reaper's release")
}

// A restart whose holder died: its claim is demoted when the lease expires
// and released once a fresh inventory shows the outcome; the slot of the
// stopped agent then frees through releaseSlotIfIdle.
func TestAbandonedRestartClaim_ReaperReleases_ThenSlotFrees(t *testing.T) {
	f, _, a := newClaimFixture(t) // a is stopped
	noObservationLag(t)
	ctx := context.Background()
	seedSlot(t, f.s, f.brokerID, a.ID)
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "dead-hub", store.StartClaimRestart, "", time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	f.srv.reapStartClaims(ctx)
	require.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState)
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "held while the claim is held")

	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(ctx)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed once the claim was released")
}

// A restart whose holder died after a counted phase was written: the
// reaper releases the claim once a fresh inventory shows nothing running,
// but the agent still reads a counted phase, so releaseSlotIfIdle keeps the
// slot. The missing-container reconcile then moves the phase to error, and
// the observed-phase release frees the slot, with no sweep.
func TestAbandonedRestartClaim_CountedPhase_SlotFreesWhenMissing(t *testing.T) {
	f := newReconcileFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	a := f.addAgent("abandon-running", "running", "working")
	seedSlot(t, f.s, f.brokerID, a.ID)
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "dead-hub", store.StartClaimRestart, "", time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	f.srv.reapStartClaims(ctx) // demoted
	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(ctx) // released
	require.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim")
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "a counted phase keeps the slot")

	f.heartbeat(completeInventory())
	f.expireClock(a.ID)
	f.heartbeat(completeInventory())

	assert.Equal(t, string(state.PhaseError), getAgent(t, f.s, a.ID).Phase, "marked missing")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed by the observed-phase release")
}

// A delete wins over a held start claim: the delete finalize releases the
// agent's quotas (the delete-won path is agent-delete-lead's; this pins the
// end state only).
func TestDeleteWin_ClaimSuperseded_QuotaReleased(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	broker, project := newQuotaTestBrokerAndProject(t, s, "del-win")
	a := newQuotaTestAgent(t, s, broker, project, "del-win", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)
	_, err := s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// A restart at a full instance keeps its own slot across both legs and is
// never left stopped; the dying container's report does not free it, and a
// new start racing the restart is refused at the cap.
func TestRestartAtCap_KeepsSlot_NeverStopped(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	backstops := backstopCounter(t, srv)
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-cap")
	a := newQuotaTestAgent(t, s, broker, project, "rs-cap", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)
	filler := newQuotaTestAgent(t, s, broker, project, "rs-cap-filler", state.PhaseRunning)
	seedSlot(t, s, broker.ID, filler.ID)
	newcomer := newQuotaTestAgent(t, s, broker, project, "rs-cap-new", state.PhaseStopped)

	var counts []int64
	var racingCode int
	disp.onStop = func(*store.Agent) {
		postDyingStoppedReport(t, srv, a)
		counts = append(counts, brokerReservationCount(t, s, broker.ID))
	}
	disp.onStart = func(*store.Agent) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+newcomer.ID+"/start", nil)
		racingCode = rec.Code
		counts = append(counts, brokerReservationCount(t, s, broker.ID))
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []int64{2, 2}, counts, "the count is unchanged throughout")
	assert.Equal(t, http.StatusTooManyRequests, racingCode, "a new start at the cap is refused")
	assert.Equal(t, string(state.PhaseRunning), getAgent(t, s, a.ID).Phase)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, newcomer.ID))
	assert.EqualValues(t, 2, brokerReservationCount(t, s, broker.ID))
	assert.Zero(t, backstops(backstopSiteRestart))
}

// Merge condition 4: while a restart proceeds under its claim at a full
// instance, a new start is refused with 429.
func TestNewStartAtCapRefused_WhileRestartUnderClaimProceeds(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-claim-new")
	a := newQuotaTestAgent(t, s, broker, project, "rs-claim-new", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)
	newcomer := newQuotaTestAgent(t, s, broker, project, "rs-claim-new-b", state.PhaseStopped)

	var claimKind store.StartClaimKind
	var racing int
	disp.onStop = func(*store.Agent) {
		claimKind = getAgent(t, s, a.ID).StartClaimKind
		racing = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+newcomer.ID+"/start", nil).Code
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.StartClaimRestart, claimKind, "the restart ran under its claim")
	assert.Equal(t, http.StatusTooManyRequests, racing)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
}

// A stopped agent's restart at a full instance is a checked admission: 429
// before any stop leg.
func TestRestartOfStoppedAgent_IsCheckedAdmission(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-stopped")
	held := newQuotaTestAgent(t, s, broker, project, "rs-stopped-held", state.PhaseRunning)
	seedSlot(t, s, broker.ID, held.ID)
	cand := newQuotaTestAgent(t, s, broker, project, "rs-stopped-cand", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/restart", nil)
	assertBrokerQuotaExceeded(t, rec)
	assert.Zero(t, disp.stopCount.Load(), "no stop leg")
	assert.Zero(t, disp.startCount.Load())
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, cand.ID))
}

// A stopped self-report handled by another replica during a restart's stop
// leg does not free the slot: the restart's claim is in the store.
func TestStatusReportDuringRestart_DoesNotRelease(t *testing.T) {
	srv, s := testServer(t)
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	backstops := backstopCounter(t, srv)
	other := newTestServerFromStore(t, s, nil)
	other.instanceID = "replica-b"
	other.startClaimsOn = true
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-xrep")
	a := newQuotaTestAgent(t, s, broker, project, "rs-xrep", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)

	var heldDuringStop bool
	disp.onStop = func(*store.Agent) {
		selfReport(t, other, a, state.PhaseStopped)
		heldDuringStop = hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID)
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, heldDuringStop, "the other replica's report did not free the slot")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.Zero(t, backstops(backstopSiteRestart))
}

// claimHookStore runs onClaim once, right before the first start claim it
// is asked for while armed.
type claimHookStore struct {
	store.Store
	fault   *storeFaultSwitch
	once    sync.Once
	onClaim func(agentID string, kind store.StartClaimKind)
}

func (c *claimHookStore) ClaimAgentStart(ctx context.Context, agentID, owner string, kind store.StartClaimKind, target string, ttl time.Duration) (store.StartClaim, error) {
	if c.fault.Active() && c.onClaim != nil {
		c.once.Do(func() { c.onClaim(agentID, kind) })
	}
	return c.Store.ClaimAgentStart(ctx, agentID, owner, kind, target, ttl)
}

// The one remaining window (design §4.4 Bound; k8s caveat a): replica A's
// restart reserves before it claims, and a stopped report handled by
// replica B in between frees the slot, which a racing new start on B takes.
// The restart's metered backstop then re-asserts: the instance is at most
// one over its cap, the backstop fires exactly once, and its audit event
// names the agent and the Runtime Broker. Once the restart's claim clears
// the slot is held because the agent runs.
func TestRestartWindow_TwoReplicas_AtMostCapPlusOne(t *testing.T) {
	srv, s, hook, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *claimHookStore {
		return &claimHookStore{Store: inner, fault: f}
	})
	disp := &hookedStartDispatcher{}
	srv.SetDispatcher(disp)
	backstops := backstopCounter(t, srv)
	other := newTestServerFromStore(t, s, nil)
	other.instanceID = "replica-b"
	other.startClaimsOn = true
	other.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "rs-window")
	a := newQuotaTestAgent(t, s, broker, project, "rs-window", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)
	filler := newQuotaTestAgent(t, s, broker, project, "rs-window-filler", state.PhaseRunning)
	seedSlot(t, s, broker.ID, filler.ID)
	newcomer := newQuotaTestAgent(t, s, broker, project, "rs-window-new", state.PhaseStopped)

	var maxCount int64
	observe := func() {
		if n := brokerReservationCount(t, s, broker.ID); n > maxCount {
			maxCount = n
		}
	}
	var racing int
	hook.onClaim = func(agentID string, kind store.StartClaimKind) {
		require.Equal(t, a.ID, agentID)
		require.Equal(t, store.StartClaimRestart, kind)
		selfReport(t, other, a, state.PhaseStopped)
		observe()
		racing = doRequest(t, other, http.MethodPost, "/api/v1/agents/"+newcomer.ID+"/start", nil).Code
		observe()
	}
	disp.onStop = func(*store.Agent) { observe() }
	disp.onStart = func(*store.Agent) { observe() }
	fault.Arm()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	observe()

	require.Equal(t, http.StatusOK, racing, "the racing start took the slot the report freed")
	assert.LessOrEqual(t, maxCount, int64(3), "never more than cap+1")
	assert.EqualValues(t, 3, brokerReservationCount(t, s, broker.ID), "one over, per in-flight restart")
	assert.EqualValues(t, 1, backstops(backstopSiteRestart), "the backstop fired exactly once")
	audits := capacityAudits(t, s, auditQuotaBackstopReassert, a.ID)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].AfterSummary, broker.ID, "the audit event names the Runtime Broker")
	assert.Equal(t, a.ID, audits[0].TargetID, "and the agent")
	got := getAgent(t, s, a.ID)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Empty(t, got.StartClaimID, "the restart's claim cleared")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "held because the agent runs")
}

// The backstop writes only when a release slipped through an unguarded
// path, and each write is counted and audited.
func TestBackstop_WritesOnlyWhenUnguardedReleaseSlipped(t *testing.T) {
	for _, slip := range []bool{false, true} {
		name := "no-slip"
		if slip {
			name = "slip"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &hookedStartDispatcher{}
			srv.SetDispatcher(disp)
			backstops := backstopCounter(t, srv)
			broker, project := newQuotaTestBrokerAndProject(t, s, "bs-"+name)
			a := newQuotaTestAgent(t, s, broker, project, "bs-"+name, state.PhaseRunning)
			seedSlot(t, s, broker.ID, a.ID)
			disp.onStop = func(*store.Agent) {
				postDyingStoppedReport(t, srv, a) // guarded: frees nothing
				if slip {
					// An unguarded path: an unconditional release.
					srv.quotaService.Release(context.Background(), store.LimitMaxAgentsPerBroker, a.ID)
				}
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
			audits := capacityAudits(t, s, auditQuotaBackstopReassert, a.ID)
			if !slip {
				assert.Zero(t, backstops(backstopSiteRestart))
				assert.Empty(t, audits)
				return
			}
			assert.EqualValues(t, 1, backstops(backstopSiteRestart))
			require.Len(t, audits, 1)
			assert.True(t, strings.Contains(audits[0].AfterSummary, broker.ID))
		})
	}
}

// The conditional release on a missing agent row returns ErrNotFound and
// releases nothing; the sweep still releases a missing agent's row by its
// reservation ID.
func TestReleaseIfAgent_MissingRow_ReturnsNotFound_SweepStillReleases(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, _ := newQuotaTestBrokerAndProject(t, s, "missing-row")
	ghost := tid("agent-missing-row")
	seedSlot(t, s, broker.ID, ghost)

	released, err := s.ReleaseReservationIfAgent(ctx, brokerLimitDefID(t, s), ghost, store.AgentReleaseCond{})
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.False(t, released)
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, ghost))

	srv.ReconcileStaleBrokerQuotaReservations(ctx)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, ghost))
}

// hideAfterReadStore drops agentID from the sweep's batch read of the agents
// holding rows, as a soft-deleted agent would look, and runs afterRead right
// after that read, once, while armed.
type hideAfterReadStore struct {
	store.Store
	fault     *storeFaultSwitch
	agentID   string
	afterRead func()
	done      bool
}

func (h *hideAfterReadStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	out, err := h.Store.GetAgentsByIDs(ctx, ids)
	if err != nil || !h.fault.Active() || h.done {
		return out, err
	}
	for _, id := range ids {
		if id == h.agentID {
			h.done = true
			delete(out, id)
			h.afterRead()
			break
		}
	}
	return out, err
}

// The release of a missing or soft-deleted agent's row is conditional on
// the row too: a restore and a start that re-reserved after the read keep
// their slot.
func TestSweep_StaleDeletedObservationDoesNotReleaseAReReservedSlot(t *testing.T) {
	srv, s, hook, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *hideAfterReadStore {
		return &hideAfterReadStore{Store: inner, fault: f}
	})
	broker, project := newQuotaTestBrokerAndProject(t, s, "rerez-del")
	a := newQuotaTestAgent(t, s, broker, project, "rerez-del", state.PhaseRunning)
	seedSlot(t, s, broker.ID, a.ID)

	var fresh *store.UsageReservation
	hook.agentID = a.ID
	hook.afterRead = func() {
		require.NoError(t, s.ReleaseReservation(context.Background(), brokerLimitDefID(t, s), a.ID))
		fresh = seedSlot(t, s, broker.ID, a.ID)
	}
	fault.Arm()

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	require.NotNil(t, fresh)
	held := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, held, "the re-reserved slot is not freed by the stale observation")
	assert.Equal(t, fresh.ID, held.ID)
}

// The idle release frees only the slot the claim's start held: a
// reservation on the broker that start targeted, which is also the agent
// row's broker (k8s merge condition 2, R6-B1). A start refused because the
// agent's reservation is on another broker releases its claim, and the
// reservation on the other broker is kept; the sweep decides it. The same
// scenario as TestReserve_FailedRetryDoesNotReleaseExisting, driven once,
// that also pins the claim release and that nothing is dispatched.
func TestStartRefusedScopeConflict_StoppedAgent_KeepsOtherBrokerRow(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 5)
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "keep-a")
	brokerB, projectB := newQuotaTestBrokerAndProject(t, s, "keep-b")
	a := newQuotaTestAgent(t, s, brokerB, projectB, "keep-other", state.PhaseStopped)
	held := seedSlot(t, s, brokerA.ID, a.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeBrokerReservationScopeConflict)
	assert.Empty(t, getAgent(t, s, a.ID).StartClaimID, "the refused start released its claim")
	after := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, after, "the reservation on the other broker is kept")
	assert.Equal(t, held.ID, after.ID)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerA.ID))
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID))
	assert.Zero(t, disp.startCount.Load(), "nothing was dispatched")
}

// An empty broker skips the idle release: there is no start target to scope
// it to. The same idle agent's slot is freed on its own broker (control).
// The empty case documents the guard; the reservation's scope check would
// also skip it. TestReleaseSlotIfIdle_ClaimBrokerMustBeTheRowBroker pins the
// row-broker clause.
func TestReleaseSlotIfIdle_EmptyBrokerSkips(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "idle-empty")
	a := newQuotaTestAgent(t, s, broker, project, "idle-empty", state.PhaseStopped)
	seedSlot(t, s, broker.ID, a.ID)

	srv.releaseSlotIfIdleOn(ctx, a.ID, "")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "an empty broker skips the release")
	srv.releaseSlotIfIdleOn(ctx, a.ID, broker.ID)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the idle agent's slot on its broker is freed")
}

// The launch-report hook (a caller with no claim run) scopes the idle
// release to the agent row's broker: an async create's launch ends while
// the agent's reservation is on another broker, and that reservation is
// kept.
func TestLaunchEnd_HookKeepsOtherBrokerRow(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	a := f.addAgent("le-other", string(state.PhaseProvisioning), "")
	other := seedSlot(t, f.s, tid("le-other-broker"), a.ID)
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-x", store.StartClaimCreate, "", time.Minute)
	require.NoError(t, err)
	launchID, err := f.s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	a = getAgent(t, f.s, a.ID)
	selfReport(t, f.srv, a, state.PhaseStopped)

	rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
		LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the launch end released the claim")
	after := activeReservation(t, f.s, brokerLimitDefID(t, f.s), a.ID)
	require.NotNil(t, after, "the reservation on the other broker is kept")
	assert.Equal(t, other.ID, after.ID)
}

// reservationSwapStore, once armed, answers the first GetActiveReservation
// of agentID with the row it read, then releases that row and records a new
// one on swapTo, as a concurrent move between the idle release's read and
// its write would.
type reservationSwapStore struct {
	store.Store
	fault   *storeFaultSwitch
	t       *testing.T
	agentID string
	swapTo  string
	swapped bool
}

func (w *reservationSwapStore) GetActiveReservation(ctx context.Context, limitDefID, resourceID string) (*store.UsageReservation, error) {
	got, err := w.Store.GetActiveReservation(ctx, limitDefID, resourceID)
	if err != nil || w.swapped || !w.fault.Active() || resourceID != w.agentID {
		return got, err
	}
	w.swapped = true
	require.NoError(w.t, w.ReleaseReservationByID(ctx, got.ID))
	seedBrokerRow(w.t, w.Store, limitDefID, w.swapTo, resourceID, time.Time{})
	return got, nil
}

// The idle release's write names the reservation it read
// (AgentReleaseCond.ReservationID): a row that replaced it in between, here
// on another broker, is not released.
func TestReleaseSlotIfIdle_WriteNamesTheReservationRead(t *testing.T) {
	srv, s, swap, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *reservationSwapStore {
		return &reservationSwapStore{Store: inner, fault: fault, t: t}
	})
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "idle-swap")
	a := newQuotaTestAgent(t, s, broker, project, "idle-swap", state.PhaseStopped)
	seedSlot(t, s, broker.ID, a.ID)
	otherBroker := tid("idle-swap-other")
	swap.agentID, swap.swapTo = a.ID, otherBroker
	fault.Arm()

	srv.releaseSlotIfIdleOn(ctx, a.ID, broker.ID)
	after := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, after, "the row that replaced the one read is kept")
	assert.Equal(t, otherBroker, after.ScopeID)
}

// The claim's broker must also be the agent row's broker: a reservation on
// the claim's broker X is kept while the agent row is on broker Y, even
// though the reservation's scope matches X (design r7 §4.4, R6-B1, owner
// rule: reservation scope == claim broker == row broker).
func TestReleaseSlotIfIdle_ClaimBrokerMustBeTheRowBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	brokerX, _ := newQuotaTestBrokerAndProject(t, s, "rowclaim-x")
	brokerY, projectY := newQuotaTestBrokerAndProject(t, s, "rowclaim-y")
	a := newQuotaTestAgent(t, s, brokerY, projectY, "rowclaim", state.PhaseStopped)
	held := seedSlot(t, s, brokerX.ID, a.ID)

	srv.releaseSlotIfIdleOn(ctx, a.ID, brokerX.ID)
	after := activeReservation(t, s, brokerLimitDefID(t, s), a.ID)
	require.NotNil(t, after, "the row is on another broker than the claim's: kept")
	assert.Equal(t, held.ID, after.ID)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerX.ID))
}

// A halted agent (a run the platform ended, GoogleCloudPlatform/scion#3643)
// holds no broker capacity: it is not counted, and the idle release frees
// its slot on its own broker as for stopped (store.BrokerQuotaCountedPhase).
func TestHalted_NotCounted_IdleReleaseFreesSlot(t *testing.T) {
	assert.False(t, store.AgentHoldsBrokerCapacity(&store.Agent{Phase: string(state.PhaseHalted)}), "a halted agent holds no capacity")
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "halted-idle")
	a := newQuotaTestAgent(t, s, broker, project, "halted-idle", state.PhaseHalted)
	seedSlot(t, s, broker.ID, a.ID)

	srv.releaseSlotIfIdleOn(ctx, a.ID, broker.ID)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the halted agent's slot is freed")
	assert.Zero(t, brokerReservationCount(t, s, broker.ID))
}
