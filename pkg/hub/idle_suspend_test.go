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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableIdleSuspendForTest(t *testing.T, srv *Server, after time.Duration) {
	t.Helper()
	setIdleSuspendExperiment(t, srv, true)
	srv.mu.Lock()
	srv.config.AutoSuspendIdle = true
	srv.config.IdleSuspendAfter = after
	srv.mu.Unlock()
}

// newIdleCandidateBrokerAgent creates a running agent on an online broker with
// RunID, RunIntentRunning, activity, activitySince and lastSeen populated.
func newIdleCandidateBrokerAgent(
	t *testing.T,
	s store.Store,
	name string,
	activity string,
	activitySince *time.Time,
	lastSeen time.Time,
) *store.Agent {
	t.Helper()
	ctx := context.Background()
	_, _, agent := setupOnlineBrokerAgent(t, s, name)
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)

	db := s.(*entadapter.CompositeStore).DB()
	runID := "run-" + name
	var actSinceArg any
	if activitySince != nil {
		actSinceArg = activitySince.UTC()
	}
	_, err = db.ExecContext(ctx,
		"UPDATE agents SET run_id = ?, activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
		runID, activity, actSinceArg, lastSeen.UTC(), agent.ID,
	)
	require.NoError(t, err)

	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	return loaded
}

func TestIdleSuspend_SettingsDefaultAndClamp(t *testing.T) {
	def := DefaultServerConfig()
	assert.False(t, def.AutoSuspendIdle)
	assert.Equal(t, DefaultIdleSuspendAfter, def.IdleSuspendAfter)
	assert.Equal(t, 15*time.Minute, DefaultIdleSuspendAfter)
	assert.Equal(t, 5*time.Minute, MinIdleSuspendAfter)

	t.Run("New defaults zero, accepts 5m floor, and rejects below 5m to default", func(t *testing.T) {
		s, err := newTestStore(t, ":memory:")
		require.NoError(t, err)
		srvZero, err := New(ServerConfig{}, s)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srvZero.Shutdown(context.Background()) })
		enabled, after := srvZero.idleSuspendSettings()
		assert.False(t, enabled)
		assert.Equal(t, DefaultIdleSuspendAfter, after)

		s2, err := newTestStore(t, ":memory:")
		require.NoError(t, err)
		logs := captureSlog(t)
		srvLow, err := New(ServerConfig{AutoSuspendIdle: true, IdleSuspendAfter: 90 * time.Second}, s2)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srvLow.Shutdown(context.Background()) })
		enabled, after = srvLow.idleSuspendSettings()
		assert.True(t, enabled)
		assert.Equal(t, DefaultIdleSuspendAfter, after)
		assert.Contains(t, logs.String(), "idle_suspend_after below minimum 5m, using default")

		sMin, err := newTestStore(t, ":memory:")
		require.NoError(t, err)
		srvMin, err := New(ServerConfig{AutoSuspendIdle: true, IdleSuspendAfter: MinIdleSuspendAfter}, sMin)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srvMin.Shutdown(context.Background()) })
		_, after = srvMin.idleSuspendSettings()
		assert.Equal(t, MinIdleSuspendAfter, after)

		s3, err := newTestStore(t, ":memory:")
		require.NoError(t, err)
		srvValid, err := New(ServerConfig{AutoSuspendIdle: true, IdleSuspendAfter: 25 * time.Minute}, s3)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srvValid.Shutdown(context.Background()) })
		enabled, after = srvValid.idleSuspendSettings()
		assert.True(t, enabled)
		assert.Equal(t, 25*time.Minute, after)
	})

	t.Run("ApplySnapshot accepts >=5m and falls back to default below 5m", func(t *testing.T) {
		srv, _ := testServer(t)
		ApplySnapshot(srv, Layer1Snapshot{AutoSuspendIdle: true, IdleSuspendAfter: "5m"})
		enabled, after := srv.idleSuspendSettings()
		assert.True(t, enabled)
		assert.Equal(t, MinIdleSuspendAfter, after)

		ApplySnapshot(srv, Layer1Snapshot{AutoSuspendIdle: true, IdleSuspendAfter: "2m"})
		enabled, after = srv.idleSuspendSettings()
		assert.True(t, enabled)
		assert.Equal(t, DefaultIdleSuspendAfter, after)

		ApplySnapshot(srv, Layer1Snapshot{AutoSuspendIdle: false, IdleSuspendAfter: "0s"})
		enabled, after = srv.idleSuspendSettings()
		assert.False(t, enabled)
		assert.Equal(t, DefaultIdleSuspendAfter, after)
	})
}

func TestIdleSuspend_RegisteredSingleton(t *testing.T) {
	srv, _ := testServer(t)
	srv.scheduler = NewScheduler(srv.store, logging.Subsystem("hub.scheduler"))
	srv.registerSchedulerHandlers()

	var idleHandler *RecurringHandler
	for i := range srv.scheduler.recurring {
		if srv.scheduler.recurring[i].Name == "agent-idle-suspend" {
			idleHandler = &srv.scheduler.recurring[i]
			break
		}
	}
	require.NotNil(t, idleHandler, "agent-idle-suspend must be registered in registerSchedulerHandlers")
	assert.Equal(t, 5, idleHandler.Interval)
	assert.True(t, idleHandler.Singleton, "agent-idle-suspend must be registered via RegisterRecurringSingleton")
}

func TestIdleSuspend_EveryActivityTable(t *testing.T) {
	var all []string
	for _, a := range state.Activities() {
		all = append(all, string(a))
	}
	all = append(all, "")

	eligible := map[string]bool{
		string(state.ActivityIdle):         true,
		string(state.ActivityCompleted):    true,
		string(state.ActivityHarnessError): true,
	}

	for _, act := range all {
		name := act
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			disp := &runIntentDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSuspendForTest(t, srv, 15*time.Minute)

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			freshSeen := now.Add(-30 * time.Second)
			agent := newIdleCandidateBrokerAgent(t, s, "act-"+name, act, &pastGrace, freshSeen)

			srv.agentIdleSuspendHandler()(ctx)
			srv.waitAutoSuspendIdle()

			got, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			if eligible[act] {
				assert.Equal(t, int32(1), disp.stops.Load(), "activity %q must be stopped", act)
				assert.Equal(t, string(state.PhaseSuspended), got.Phase, "activity %q must transition to suspended", act)
				assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason, "activity %q must record suspend_reason=idle", act)
			} else {
				assert.Zero(t, disp.stops.Load(), "activity %q must never be stopped by idle sweep", act)
				assert.Equal(t, string(state.PhaseRunning), got.Phase, "activity %q must remain running", act)
				assert.Empty(t, got.SuspendReason, "activity %q must not have suspend_reason", act)
			}
		})
	}
}

func TestIdleSuspend_LiveWorkGuardTable(t *testing.T) {
	type liveCase struct {
		name       string
		age        *time.Duration
		expectSkip bool
	}
	recentAge := 10 * time.Second
	staleAge := store.LiveWorkFreshness + 30*time.Second

	cases := []liveCase{
		{name: "recent", age: &recentAge, expectSkip: true},
		{name: "stale", age: &staleAge, expectSkip: false},
		{name: "absent", age: nil, expectSkip: false},
	}

	for _, act := range []state.Activity{
		state.ActivityIdle,
		state.ActivityCompleted,
		state.ActivityHarnessError,
	} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/live_work=%s", act, tc.name), func(t *testing.T) {
				ctx := context.Background()
				srv, s := testServer(t)
				disp := &runIntentDispatcher{}
				srv.SetDispatcher(disp)
				enableIdleSuspendForTest(t, srv, 15*time.Minute)

				now := time.Now().UTC()
				pastGrace := now.Add(-20 * time.Minute)
				freshSeen := now.Add(-30 * time.Second)
				agent := newIdleCandidateBrokerAgent(t, s, fmt.Sprintf("lw-%s-%s", act, tc.name), string(act), &pastGrace, freshSeen)
				if tc.age != nil {
					at := now.Add(-*tc.age)
					db := s.(*entadapter.CompositeStore).DB()
					_, err := db.ExecContext(ctx, "UPDATE agents SET last_live_process_at = ? WHERE id = ?", at, agent.ID)
					require.NoError(t, err)
				}
				logs := captureSlog(t)

				srv.agentIdleSuspendHandler()(ctx)
				srv.waitAutoSuspendIdle()

				got, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				if tc.expectSkip {
					assert.Zero(t, disp.stops.Load(), "agent with recent live work must not be stopped")
					assert.Equal(t, string(state.PhaseRunning), got.Phase)
					assert.Empty(t, got.SuspendReason)
					assert.Contains(t, logs.String(), "agent has live work")
				} else {
					assert.Equal(t, int32(1), disp.stops.Load())
					assert.Equal(t, string(state.PhaseSuspended), got.Phase)
					assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
				}
			})
		}
	}
}

func TestIdleSuspend_GatesAndGrace(t *testing.T) {
	t.Run("before grace is untouched", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSuspendForTest(t, srv, 15*time.Minute)

		now := time.Now().UTC()
		withinGrace := now.Add(-14 * time.Minute)
		agent := newIdleCandidateBrokerAgent(t, s, "before-grace", string(state.ActivityIdle), &withinGrace, now.Add(-20*time.Second))

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Zero(t, disp.stops.Load())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})

	t.Run("null activity_since and offline last_seen are untouched", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSuspendForTest(t, srv, 15*time.Minute)

		now := time.Now().UTC()
		pastGrace := now.Add(-30 * time.Minute)
		nullSince := newIdleCandidateBrokerAgent(t, s, "null-since", string(state.ActivityIdle), nil, now.Add(-20*time.Second))
		staleSeen := newIdleCandidateBrokerAgent(t, s, "stale-seen", string(state.ActivityIdle), &pastGrace, now.Add(-5*time.Minute))

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Zero(t, disp.stops.Load())
		for _, id := range []string{nullSince.ID, staleSeen.ID} {
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
		}
	})

	t.Run("experiment off does nothing even with setting on", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSuspendForTest(t, srv, 15*time.Minute)
		setIdleSuspendExperiment(t, srv, false)

		now := time.Now().UTC()
		pastGrace := now.Add(-30 * time.Minute)
		agent := newIdleCandidateBrokerAgent(t, s, "exp-off", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Zero(t, disp.stops.Load())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})

	t.Run("setting off does nothing even with experiment on", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		setIdleSuspendExperiment(t, srv, true)
		srv.config.AutoSuspendIdle = false
		srv.config.IdleSuspendAfter = 15 * time.Minute

		now := time.Now().UTC()
		pastGrace := now.Add(-30 * time.Minute)
		agent := newIdleCandidateBrokerAgent(t, s, "setting-off", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Zero(t, disp.stops.Load())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})

	t.Run("setting turned off after candidate selection skips worker suspend", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSuspendForTest(t, srv, 15*time.Minute)

		now := time.Now().UTC()
		pastGrace := now.Add(-30 * time.Minute)
		snapshot := newIdleCandidateBrokerAgent(t, s, "toggled-off", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))

		// Turn setting off before the worker processes the queued item.
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = false
		srv.mu.Unlock()
		srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *snapshot, reason: state.SuspendReasonIdle}})

		assert.Zero(t, disp.stops.Load())
		got, err := s.GetAgent(ctx, snapshot.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})
}

func TestIdleSuspend_FreshRereadSkipsChangedState(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(t *testing.T, s store.Store, id string, now time.Time)
		wantSuspend bool
	}{
		{
			name: "activity changed to working",
			mutate: func(t *testing.T, s store.Store, id string, now time.Time) {
				db := s.(*entadapter.CompositeStore).DB()
				_, err := db.ExecContext(context.Background(),
					"UPDATE agents SET activity = ?, activity_since = ? WHERE id = ?",
					string(state.ActivityWorking), now.UTC(), id)
				require.NoError(t, err)
			},
			wantSuspend: false,
		},
		{
			name: "activity changed to blocked",
			mutate: func(t *testing.T, s store.Store, id string, now time.Time) {
				db := s.(*entadapter.CompositeStore).DB()
				_, err := db.ExecContext(context.Background(),
					"UPDATE agents SET activity = ?, activity_since = ? WHERE id = ?",
					string(state.ActivityBlocked), now.UTC(), id)
				require.NoError(t, err)
			},
			wantSuspend: false,
		},
		{
			name: "activity changed to another eligible activity",
			mutate: func(t *testing.T, s store.Store, id string, _ time.Time) {
				db := s.(*entadapter.CompositeStore).DB()
				_, err := db.ExecContext(context.Background(),
					"UPDATE agents SET activity = ? WHERE id = ?",
					string(state.ActivityCompleted), id)
				require.NoError(t, err)
			},
			wantSuspend: false,
		},
		{
			name: "activity_since refreshed inside grace",
			mutate: func(t *testing.T, s store.Store, id string, now time.Time) {
				db := s.(*entadapter.CompositeStore).DB()
				_, err := db.ExecContext(context.Background(),
					"UPDATE agents SET activity_since = ? WHERE id = ?",
					now.Add(-2*time.Minute).UTC(), id)
				require.NoError(t, err)
			},
			wantSuspend: false,
		},
		{
			name: "run_id changed",
			mutate: func(t *testing.T, s store.Store, id string, _ time.Time) {
				db := s.(*entadapter.CompositeStore).DB()
				_, err := db.ExecContext(context.Background(),
					"UPDATE agents SET run_id = ? WHERE id = ?",
					"run-newer", id)
				require.NoError(t, err)
			},
			wantSuspend: false,
		},
		{
			name:        "unchanged candidate suspends",
			mutate:      func(_ *testing.T, _ store.Store, _ string, _ time.Time) {},
			wantSuspend: true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			disp := &runIntentDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSuspendForTest(t, srv, 15*time.Minute)

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			snapshot := newIdleCandidateBrokerAgent(t, s, fmt.Sprintf("race-%d", i), string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))

			tc.mutate(t, s, snapshot.ID, now)
			srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *snapshot, reason: state.SuspendReasonIdle}})

			got, err := s.GetAgent(ctx, snapshot.ID)
			require.NoError(t, err)
			if tc.wantSuspend {
				assert.Equal(t, int32(1), disp.stops.Load())
				assert.Equal(t, string(state.PhaseSuspended), got.Phase)
				assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
			} else {
				assert.Zero(t, disp.stops.Load())
				assert.Equal(t, string(state.PhaseRunning), got.Phase)
				assert.Empty(t, got.SuspendReason)
			}
		})
	}
}

func TestIdleSuspend_LifecycleOpInFlightSkipped(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	enableIdleSuspendForTest(t, srv, 15*time.Minute)

	now := time.Now().UTC()
	pastGrace := now.Add(-20 * time.Minute)
	agent := newIdleCandidateBrokerAgent(t, s, "op-inflight", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))

	end := srv.beginLifecycleOp(agent.ID)
	srv.agentIdleSuspendHandler()(ctx)
	srv.waitAutoSuspendIdle()

	assert.Zero(t, disp.stops.Load(), "in-flight lifecycle operation must block idle auto-suspend")
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)

	end()
	srv.agentIdleSuspendHandler()(ctx)
	srv.waitAutoSuspendIdle()

	assert.Equal(t, int32(1), disp.stops.Load())
	got, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
}

func TestIdleSuspend_HarnessResumeSupport(t *testing.T) {
	t.Run("non-resumable custom harness config skipped with log", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSuspendForTest(t, srv, 15*time.Minute)

		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID: tid("hc-idle-generic"), Name: "team-shell", Slug: "team-shell", Harness: "generic",
			Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{Harness: "generic"},
		}))
		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		agent := newIdleCandidateBrokerAgent(t, s, "idle-generic", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))
		agent.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "team-shell"}
		require.NoError(t, s.UpdateAgent(ctx, agent))

		logs := captureSlog(t)
		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Zero(t, disp.stops.Load())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Contains(t, logs.String(), "skipping auto-suspend; harness does not support resume")
	})

	t.Run("resumable custom harness config suspends and exposes suspendReason=idle", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSuspendForTest(t, srv, 15*time.Minute)

		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID: tid("hc-idle-claude"), Name: "team-claude", Slug: "team-claude", Harness: "claude",
			Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{Harness: "claude"},
		}))
		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		agent := newIdleCandidateBrokerAgent(t, s, "idle-claude", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))
		agent.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "team-claude"}
		require.NoError(t, s.UpdateAgent(ctx, agent))

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Equal(t, int32(1), disp.stops.Load())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "idle", body["suspendReason"])
	})
}

func TestIdleSuspend_EndToEnd_SweepSuspendsAndDMWakesWithoutFlag(t *testing.T) {
	for _, path := range []string{"agent DM", "user DM"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
			disp := &wakeTrackingDispatcher{}
			srv.SetDispatcher(disp)
			t.Cleanup(srv.waitAutoWakeIdle)
			enableIdleSuspendForTest(t, srv, 15*time.Minute)

			_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
			require.NoError(t, err)

			// Transition running agent to idle via UpdateAgentStatus, then age
			// activity_since past the grace while keeping heartbeat fresh.
			require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
				Phase:    string(state.PhaseRunning),
				Activity: string(state.ActivityIdle),
			}))
			now := time.Now().UTC()
			db := s.(*entadapter.CompositeStore).DB()
			_, err = db.ExecContext(ctx,
				"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
				"run-e2e-"+path, now.Add(-20*time.Minute), now.Add(-15*time.Second), target.ID,
			)
			require.NoError(t, err)

			// Sweep + worker suspends the idle agent with suspend_reason=idle.
			srv.agentIdleSuspendHandler()(ctx)
			srv.waitAutoSuspendIdle()

			suspended, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
			require.Equal(t, string(state.SuspendReasonIdle), suspended.SuspendReason)

			// Sending a DM without --wake auto-resumes the agent and delivers.
			awReadyOnStart(t, s, target.ID)
			if path == "agent DM" {
				res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, suspended, "wake up on new work"))
				require.Nil(t, dmErr)
				assert.Equal(t, AgentDMAccepted, res.Outcome)
			} else {
				u := wakeLifecycleUser(t, s, "idle-e2e-user", target.ProjectID, "agent.message", "agent.read")
				rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
					map[string]any{"message": "wake up on new work"})
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}

			require.Len(t, disp.getStartCalls(), 1, "one resume dispatched")
			assert.True(t, disp.getStartCalls()[0].Continue, "resumed with Continue=true")
			msgs := disp.getMessageCalls()
			require.Len(t, msgs, 1)
			assert.Equal(t, "wake up on new work", msgs[0].Message)

			resumed, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), resumed.Phase)
			assert.Empty(t, resumed.SuspendReason, "resume clears suspend_reason")
		})
	}
}

func TestSuspendConversion_HubSuspendedToUser(t *testing.T) {
	for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
		t.Run("converts "+string(reason)+" to user without broker action and stops auto-wake", func(t *testing.T) {
			ctx := context.Background()
			srv, s, sender, target, disp := autoWakeFixture(t, reason)

			// Mark the broker offline to prove conversion performs no runtime dispatch.
			broker, err := s.GetRuntimeBroker(ctx, target.RuntimeBrokerID)
			require.NoError(t, err)
			broker.Status = store.BrokerStatusOffline
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			u := wakeLifecycleUser(t, s, "conv-ok-"+string(reason), target.ProjectID, "agent.lifecycle", "agent.read")
			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)

			// Bring broker back online and verify future DMs without --wake no longer auto-wake.
			broker.Status = store.BrokerStatusOnline
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			_, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, got, "should not auto-wake"))
			require.NotNil(t, dmErr)
			assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
			assert.Empty(t, disp.getStartCalls())
		})
	}

	t.Run("requires lifecycle permission", func(t *testing.T) {
		ctx := context.Background()
		srv, s, _, target, _ := autoWakeFixture(t, state.SuspendReasonIdle)
		msgOnly := wakeLifecycleUser(t, s, "conv-denied", target.ProjectID, "agent.message", "agent.read")

		rec := requestAsIdentity(t, srv, authUser(msgOnly), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		got, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
	})

	t.Run("already user-suspended keeps existing validation error", func(t *testing.T) {
		for _, reason := range []state.SuspendReason{state.SuspendReasonUser, ""} {
			srv, s, _, target, _ := autoWakeFixture(t, reason)
			u := wakeLifecycleUser(t, s, "conv-already-"+string(reason), target.ProjectID, "agent.lifecycle", "agent.read")

			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Only running agents can be suspended.")
		}
	})
}

func TestGroupMessage_AutoWakeHintForHubSuspendedMember(t *testing.T) {
	cases := []struct {
		name         string
		experimentOn bool
		reason       state.SuspendReason
		wantAutoHint bool
	}{
		{name: "idle with experiment on", experimentOn: true, reason: state.SuspendReasonIdle, wantAutoHint: true},
		{name: "stalled with experiment on", experimentOn: true, reason: state.SuspendReasonStalled, wantAutoHint: true},
		{name: "user with experiment on", experimentOn: true, reason: state.SuspendReasonUser, wantAutoHint: false},
		{name: "empty reason with experiment on", experimentOn: true, reason: "", wantAutoHint: false},
		{name: "idle with experiment off", experimentOn: false, reason: state.SuspendReasonIdle, wantAutoHint: false},
		{name: "stalled with experiment off", experimentOn: false, reason: state.SuspendReasonStalled, wantAutoHint: false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			setIdleSuspendExperiment(t, srv, tc.experimentOn)
			prefix := fmt.Sprintf("p3-grp-%d", i)
			projectID, agents := setupGroupTest(t, s, prefix, map[string]string{
				"grp-run":  string(state.PhaseRunning),
				"grp-susp": string(state.PhaseSuspended),
			})
			if tc.reason != "" {
				awSuspendFor(t, s, agents["grp-susp"], tc.reason)
			}
			srv.SetDispatcher(&brokerMockDispatcher{})

			rec := doRequest(t, srv, http.MethodPost,
				"/api/v1/projects/"+projectID+"/agents/grp-run/message",
				MessageRequest{StructuredMessage: &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Sender:    "user:p3",
					SenderID:  DevUserID,
					Recipient: "group[agent:grp-run,agent:grp-susp]",
					Msg:       "group hello",
					Type:      messages.TypeInstruction,
				}})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp GroupMessageResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, 1, resp.Delivered)

			var suspRes GroupMessageRecipientResult
			for _, r := range resp.Results {
				if r.Recipient == "agent:grp-susp" {
					suspRes = r
					break
				}
			}
			assert.Equal(t, "failed", suspRes.Status)
			assert.Contains(t, suspRes.Error, "group messages do not wake agents")
			if tc.wantAutoHint {
				assert.Contains(t, suspRes.Error, "message it directly to wake and deliver")
				assert.NotContains(t, suspRes.Error, "--wake")
			} else {
				assert.Contains(t, suspRes.Error, "message it directly with --wake")
			}
		})
	}
}
