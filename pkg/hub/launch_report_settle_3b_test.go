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
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// WP1 slice 3b (ptone/scion#5927): a succeeded launch report answered
// "completed" settles the launch's run while the row still holds the run
// the launch dispatched (launch_run_id), under the atomic SettleLaunchedRun
// fence. Both launch-report settle paths are observed with
// adoption_source cas; the heartbeat adoption keeps heartbeat_adopt.
//
// These tests pin the run settlement and the adoption provenance. The echo
// side (effective_config for Rb, configured, lifecycle_stale_echo labels)
// is in lifecycle_echo_3b_test.go, on slice 3's echoReportFixture.

const (
	s3bRunPrev = "run-0"
	s3bRunM    = "run-1" // the run the hub minted and the launch dispatched
	s3bRunB    = "run-b" // the run the broker reports
)

// settle3bFixture is launchSettleFixture with a launch bound to s3bRunM
// and, when observed, the lifecycle observer installed on the raw store.
type settle3bFixture struct {
	*launchSettleFixture
	ls   lifecycleEventStore
	sink *telemetryemit.MemorySink
}

func newSettle3bFixture(t *testing.T, name string, observed bool) *settle3bFixture {
	t.Helper()
	srv, s := testServer(t)
	out := &settle3bFixture{}
	if observed {
		ls, ok := s.(lifecycleEventStore)
		require.True(t, ok, "the test store must support the lifecycle observer")
		srv.config.HubIDPinned = true
		out.ls, out.sink = ls, telemetryemit.NewMemorySink()
		srv.SetLifecycleEventsSink(out.sink, telemetryemit.NewMemoryCounters())
		st, err := srv.installLifecycleObserver()
		require.NoError(t, err)
		require.Equal(t, telemetryemit.ObserverStateEnabled, st)
	}
	hooks, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *launchSettleStore {
		return &launchSettleStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	brokerID := tid("broker-" + name)
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "broker-" + name, Slug: "broker-" + name,
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	project := &store.Project{ID: tid("project-" + name), Slug: name, Name: name, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("agent-" + name), Slug: name, Name: name, ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: brokerID, StateVersion: 1, Generation: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	cctx := lifecycleevents.WithCreateContext(ctx, lifecycleevents.CreateContext{CreatorType: "user", CreatorID: "u1"})
	require.NoError(t, s.WithTx(cctx, func(tx store.Store) error { return tx.CreateAgent(cctx, agent) }))
	for _, r := range []string{s3bRunPrev, s3bRunM} {
		_, err := s.SetAgentRunID(ctx, agent.ID, r, nil)
		require.NoError(t, err)
	}
	launchID, err := s.BeginLaunchWithOptions(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute, store.BeginLaunchOptions{RunID: s3bRunM})
	require.NoError(t, err)
	require.Equal(t, s3bRunM, mustGetAgent(t, s, agent.ID).LaunchRunID, "slice 3 binds the dispatched run")
	out.launchSettleFixture = &launchSettleFixture{srv: srv, store: s, agent: agent, brokerID: brokerID, launchID: launchID, hooks: hooks, fault: fault}
	return out
}

// observeRunning is a run-less running status write while the launch is
// active: the launch ends running_observed and the row stays on Rm.
func (f *settle3bFixture) observeRunning(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, store.LaunchStateEnded, got.LaunchState)
	require.Equal(t, store.LaunchEndReasonRunningObserved, got.LaunchEndReason)
	require.Equal(t, s3bRunM, got.RunID)
}

// commitReportWithoutSettle applies a succeeded report in the store only,
// as a hub that crashed after the report's commit and before the settle
// leaves it.
func (f *settle3bFixture) commitReportWithoutSettle(t *testing.T, runID string) {
	t.Helper()
	answer, _, err := f.store.ApplyLaunchReport(context.Background(), f.agent.ID, f.brokerID, store.LaunchReport{
		LaunchID: f.launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded, Phase: string(state.PhaseRunning),
		AgentConfig: brokerAgentConfigOf(&RemoteAgentInfo{ID: f.agent.Slug, Slug: f.agent.Slug, RunID: runID}),
	})
	require.NoError(t, err)
	require.Equal(t, 0, answer.HTTPStatus)
	require.Equal(t, s3bRunM, mustGetAgent(t, f.store, f.agent.ID).RunID, "nothing settled yet")
}

// adoptionEvent is the first delivered event of the agent that carries
// adoption provenance, after a later stop names the current run.
func (f *settle3bFixture) adoptionEvent(t *testing.T) *tc.LifecycleEvent {
	t.Helper()
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.NoError(t, f.store.UpdateAgentStatus(context.Background(), f.agent.ID,
		store.AgentStatusUpdate{Phase: string(state.PhaseStopped), IfRunID: got.RunID}))
	for _, e := range lcEvents(t, f.ls, f.sink, f.agent.ID) {
		if e.AdoptedFromLaunchID != "" {
			return e
		}
	}
	t.Fatalf("no event carries adoption provenance: %s", lcSeqFull(lcEvents(t, f.ls, f.sink, f.agent.ID)))
	return nil
}

func (f *settle3bFixture) assertSettledOnB(t *testing.T) {
	t.Helper()
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, s3bRunB, got.RunID, "the broker's run is adopted")
	assert.Empty(t, got.PreviousRunIDs, "the run settled")
}

// TestLaunchReportSettle3b_RecoversAfterStoreError: a settle that fails
// with a store error leaves the row on Rm; the broker's resend (completed)
// settles it (cause 3).
func TestLaunchReportSettle3b_RecoversAfterStoreError(t *testing.T) {
	f := newSettle3bFixture(t, "s3b-err", false)
	f.observeRunning(t)
	f.hooks.settleErr = errors.New("injected settle failure")
	f.fault.Arm()
	assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
	assert.Equal(t, s3bRunM, mustGetAgent(t, f.store, f.agent.ID).RunID, "the failed settle wrote nothing")

	f.hooks.settleErr = nil // the switch stays armed; with no hook set, the call delegates
	assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
	f.assertSettledOnB(t)
}

// TestLaunchReportSettle3b_RecoversAfterCrash: the hub committed the
// succeeded report and stopped before the settle; the resend is answered
// completed and settles (cause 3). The resend after that changes nothing.
func TestLaunchReportSettle3b_RecoversAfterCrash(t *testing.T) {
	f := newSettle3bFixture(t, "s3b-crash", true)
	f.commitReportWithoutSettle(t, s3bRunB)
	assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
	f.assertSettledOnB(t)
	settledAt := *mustGetAgent(t, f.store, f.agent.ID).RunIDUpdatedAt

	// A second resend: the row is on Rb, not launch_run_id; no settle.
	assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
	got := mustGetAgent(t, f.store, f.agent.ID)
	assert.Equal(t, s3bRunB, got.RunID)
	assert.True(t, got.RunIDUpdatedAt.Equal(settledAt), "the resend wrote nothing")

	e := f.adoptionEvent(t)
	assert.Equal(t, s3bRunM, e.AdoptedFromLaunchID)
	assert.Equal(t, store.AdoptionSourceCAS, e.AdoptionSource)
}

// TestLaunchReportSettle3b_Interleavings: a write that lands between the
// handler's check and the swap refuses the settle atomically; the row's
// run is unchanged and Rb is never adopted.
func TestLaunchReportSettle3b_Interleavings(t *testing.T) {
	for _, tt := range []struct {
		name    string
		write   func(t *testing.T, f *settle3bFixture, ctx context.Context, agentID string)
		wantRun string
	}{
		{"a newer run is minted", func(t *testing.T, f *settle3bFixture, ctx context.Context, agentID string) {
			_, err := f.store.SetAgentRunID(ctx, agentID, "run-3", nil)
			require.NoError(t, err)
		}, "run-3"},
		{"a new launch begins on the same run", func(t *testing.T, f *settle3bFixture, ctx context.Context, agentID string) {
			_, err := f.store.BeginStartLaunch(ctx, agentID, store.LaunchKindRestart, 5*time.Minute, store.BeginLaunchOptions{RunID: s3bRunM})
			require.NoError(t, err)
			require.Equal(t, s3bRunM, mustGetAgent(t, f.store, agentID).LaunchRunID, "only the launch differs")
		}, s3bRunM},
		{"the agent is reassigned to another broker", func(t *testing.T, f *settle3bFixture, ctx context.Context, agentID string) {
			other := tid("broker-s3b-other")
			require.NoError(t, f.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: other, Name: "broker-s3b-other", Slug: "broker-s3b-other",
				Endpoint: "http://localhost:9801", Status: store.BrokerStatusOnline,
			}))
			n, err := f.store.ReassignAgentsToBroker(ctx, []*store.Agent{mustGetAgent(t, f.store, agentID)}, other)
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}, s3bRunM},
		{"the generation is bumped", func(t *testing.T, f *settle3bFixture, ctx context.Context, agentID string) {
			a := mustGetAgent(t, f.store, agentID)
			a.Generation++
			require.NoError(t, f.store.UpdateAgent(ctx, a))
			require.Equal(t, s3bRunM, mustGetAgent(t, f.store, agentID).RunID)
		}, s3bRunM},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newSettle3bFixture(t, "s3b-race", false)
			f.observeRunning(t)
			f.hooks.beforeSettle = func(ctx context.Context, agentID string) { tt.write(t, f, ctx, agentID) }
			f.fault.Arm()
			assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
			got := mustGetAgent(t, f.store, f.agent.ID)
			assert.Equal(t, tt.wantRun, got.RunID, "the settle is refused")
			assert.NotEmpty(t, got.PreviousRunIDs, "nothing settled")
		})
	}
}

// TestLaunchReportSettle3b_Refused: a completed report that must not
// settle leaves the row as it was.
func TestLaunchReportSettle3b_Refused(t *testing.T) {
	t.Run("late report after a sync restart minted R3", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-late", false)
		f.observeRunning(t)
		_, err := f.store.SetAgentRunID(context.Background(), f.agent.ID, "run-3", nil)
		require.NoError(t, err)
		assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
		assert.Equal(t, "run-3", mustGetAgent(t, f.store, f.agent.ID).RunID)
	})
	t.Run("delete holds the row", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-held", false)
		f.observeRunning(t)
		claimForTest(t, f.store, f.agent.ID, store.DeletionStateDeleting, time.Minute)
		f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB)
		got := mustGetAgent(t, f.store, f.agent.ID)
		assert.Equal(t, s3bRunM, got.RunID)
		assert.Equal(t, []string{s3bRunPrev}, got.PreviousRunIDs)
	})
	t.Run("unbound launch (older binary)", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-unbound", false)
		// An older replica's BeginLaunch binds nothing.
		_, err := f.store.BeginLaunch(context.Background(), f.agent.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)
		f.launchID = mustGetAgent(t, f.store, f.agent.ID).LaunchID
		require.Empty(t, mustGetAgent(t, f.store, f.agent.ID).LaunchRunID)
		f.observeRunning(t)
		assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
		got := mustGetAgent(t, f.store, f.agent.ID)
		assert.Equal(t, s3bRunM, got.RunID, "an unknown binding is never settled on completed")
		assert.Equal(t, []string{s3bRunPrev}, got.PreviousRunIDs)
	})
}

// TestLaunchReportSettle3b_AppliedUnchanged: the applied path keeps
// today's AdoptAgentRunID(row run) swap (design v9 section 3, review r1
// R1).
func TestLaunchReportSettle3b_AppliedUnchanged(t *testing.T) {
	t.Run("a stale launch_run_id from an earlier launch: adopted", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-stale", false)
		ctx := context.Background()
		// A newer dispatch mints Rm2; an older replica begins L2 without
		// touching launch_run_id, which keeps Rm.
		_, err := f.store.SetAgentRunID(ctx, f.agent.ID, "run-m2", nil)
		require.NoError(t, err)
		_, err = f.store.BeginLaunch(ctx, f.agent.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)
		// BeginLaunch without a RunID writes ""; restore the stale value an
		// old binary would have left.
		f.setLaunchRunIDForTest(t, s3bRunM)
		f.launchID = mustGetAgent(t, f.store, f.agent.ID).LaunchID
		assert.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
		f.assertSettledOnB(t)
	})
}

// setLaunchRunIDForTest writes launch_run_id directly in SQL (white-box
// setup of a mixed-version row: no store method writes it outside
// BeginLaunch).
func (f *settle3bFixture) setLaunchRunIDForTest(t *testing.T, run string) {
	t.Helper()
	dber, ok := f.store.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store exposes its database")
	res, err := dber.DB().ExecContext(context.Background(), "UPDATE agents SET launch_run_id = ? WHERE id = ?", run, f.agent.ID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.Equal(t, run, mustGetAgent(t, f.store, f.agent.ID).LaunchRunID)
}

// recordBrokerRunCredential issues a token for s3bRunB (as the dispatch
// that started it did) and puts the row back on Rm, so the heartbeat
// adoption's credential rule (e) accepts s3bRunB. The launch stays active
// and bound to Rm. The row goes back to Rm by a revert, not by minting Rm
// again: SetAgentRunID takes a newly generated run only (its store
// contract), and the lifecycle run attribution holds a re-mint of an older
// run as unproven (WP1 attribution design v2.8, rule 2F).
func (f *settle3bFixture) recordBrokerRunCredential(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, cred, err := f.srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID: f.agent.ID, ProjectID: f.agent.ProjectID, Scopes: ScopesForRole(AgentRoleFull),
	}, s3bRunB)
	require.NoError(t, err)
	_, err = f.store.SetAgentRunID(ctx, f.agent.ID, s3bRunB, cred)
	require.NoError(t, err)
	reverted, err := f.store.RevertAgentRunID(ctx, f.agent.ID, s3bRunB, s3bRunM)
	require.NoError(t, err)
	require.True(t, reverted)
	got := mustGetAgent(t, f.store, f.agent.ID)
	require.Equal(t, s3bRunM, got.LaunchRunID)
	require.Equal(t, store.LaunchStateActive, got.LaunchState)
}

// heartbeatAdopt runs the heartbeat stranded-run adoption for an entry
// labelled s3bRunB, past the quiet period.
func (f *settle3bFixture) heartbeatAdopt(t *testing.T) {
	t.Helper()
	quiet := runAdoptionQuietPeriod()
	f.srv.classifyReportedRuns(context.Background(), f.brokerID,
		&ownRunEntries{agent: mustGetAgent(t, f.store, f.agent.ID), labels: []string{s3bRunB}}, true,
		newHeartbeatReport(), time.Now().Add(quiet+30*time.Second), quiet)
	require.Equal(t, s3bRunB, mustGetAgent(t, f.store, f.agent.ID).RunID, "the heartbeat adopted the broker's run")
}

// TestLaunchReportSettle3b_HeartbeatThenApplied: a heartbeat adopted Rb
// while the launch was active; the applied report then makes a same-value
// swap Rb -> Rb that clears the previous runs, and does not relabel the
// heartbeat adoption (source heartbeat_adopt, origin Rm, adopted_at from
// the heartbeat) (design v9 sections 6, 7; review r1 R1(a), r2 R2).
func TestLaunchReportSettle3b_HeartbeatThenApplied(t *testing.T) {
	f := newSettle3bFixture(t, "s3b-hb-applied", true)
	f.recordBrokerRunCredential(t)
	f.heartbeatAdopt(t)
	beforeApplied := time.Now()
	assert.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
	f.assertSettledOnB(t)
	f.assertSameValueAppliedSettle(t)

	e := f.adoptionEvent(t)
	assert.Equal(t, s3bRunB, e.LaunchID)
	assert.Equal(t, s3bRunM, e.AdoptedFromLaunchID)
	assert.Equal(t, store.AdoptionSourceHeartbeat, e.AdoptionSource, "the same-value settle does not relabel")
	at, err := tc.ParseTS(e.AdoptedAt)
	require.NoError(t, err)
	assert.True(t, at.Before(beforeApplied), "adopted_at is the heartbeat adoption's, not the settle's: %s", e.AdoptedAt)
}

// TestLifecycleEvents_AdoptionSourcePerCaller is the per-caller producer
// fixture (WP4 r7; design v9 section 7): one adoption per caller, each
// with the adoption_source carried by the next event.
func TestLifecycleEvents_AdoptionSourcePerCaller(t *testing.T) {
	t.Run("launch-report settle, applied: cas", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-pc-applied", true)
		assert.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
		f.assertSettledOnB(t)
		e := f.adoptionEvent(t)
		assert.Equal(t, s3bRunM, e.AdoptedFromLaunchID)
		assert.Equal(t, store.AdoptionSourceCAS, e.AdoptionSource)
	})
	t.Run("launch-report settle, completed (running observed): cas", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-pc-completed", true)
		f.observeRunning(t)
		assert.Equal(t, store.LaunchReportResultCompleted, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
		f.assertSettledOnB(t)
		e := f.adoptionEvent(t)
		assert.Equal(t, s3bRunM, e.AdoptedFromLaunchID)
		assert.Equal(t, store.AdoptionSourceCAS, e.AdoptionSource)
	})
	t.Run("heartbeat stranded-run adoption: heartbeat_adopt", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-pc-heartbeat", true)
		f.recordBrokerRunCredential(t)
		// The agent runs (the launch ends running_observed, a segment opens
		// on Rm); the heartbeat then adopts the run its entry holds.
		f.observeRunning(t)
		f.heartbeatAdopt(t)
		e := f.adoptionEvent(t)
		assert.Equal(t, s3bRunM, e.AdoptedFromLaunchID)
		assert.Equal(t, store.AdoptionSourceHeartbeat, e.AdoptionSource)
	})
	t.Run("sync compare-and-swap: cas", func(t *testing.T) {
		srv, ls, projectID, sink, client := lcDispatchServer(t, "running", false)
		client.runID = "run-kept"
		id := lifecycleCreateAgent(t, srv, projectID, "s3b-pc-sync")
		row := mustGetAgent(t, srv.store, id)
		require.Equal(t, "run-kept", row.RunID, "the sync dispatch adopted the broker's run")
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+id, map[string]interface{}{"labels": map[string]string{"k": "v"}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var adopted *tc.LifecycleEvent
		for _, e := range lcEvents(t, ls, sink, id) {
			if e.AdoptedFromLaunchID != "" {
				adopted = e
				break
			}
		}
		require.NotNil(t, adopted, "an event carries the adoption")
		assert.Equal(t, "run-kept", adopted.LaunchID)
		assert.Equal(t, store.AdoptionSourceCAS, adopted.AdoptionSource)
	})
	t.Run("heartbeat during an active launch, then the applied same-value settle: heartbeat_adopt", func(t *testing.T) {
		f := newSettle3bFixture(t, "s3b-pc-hb-applied", true)
		f.recordBrokerRunCredential(t)
		f.heartbeatAdopt(t)
		assert.Equal(t, store.LaunchReportResultApplied, f.reportResult(t, store.LaunchReportStateSucceeded, s3bRunB))
		f.assertSameValueAppliedSettle(t)
		e := f.adoptionEvent(t)
		assert.Equal(t, s3bRunM, e.AdoptedFromLaunchID)
		assert.Equal(t, store.AdoptionSourceHeartbeat, e.AdoptionSource)
	})
}

// assertSameValueAppliedSettle checks that the applied report's settle
// ran as a same-value swap: the heartbeat adopted Rm -> Rb, then the
// applied settle swapped Rb -> Rb (review r1 O1: the previous runs were
// already cleared by the heartbeat, so the row alone cannot show it).
func (f *settle3bFixture) assertSameValueAppliedSettle(t *testing.T) {
	t.Helper()
	assert.Equal(t, []adoptCall{
		{from: s3bRunM, to: s3bRunB, swapped: true}, // the heartbeat adoption
		{from: s3bRunB, to: s3bRunB, swapped: true}, // the applied same-value settle
	}, f.hooks.adoptCalls())
}
