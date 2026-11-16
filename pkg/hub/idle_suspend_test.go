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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

func enableIdleSweepForTest(t *testing.T, srv *Server, after time.Duration) {
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
			enableIdleSweepForTest(t, srv, 15*time.Minute)

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
				assert.Equal(t, act, got.Activity, "activity %q must be retained across idle suspend", act)
				assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason, "activity %q must record suspend_reason=idle", act)
				assert.Nil(t, got.ActivitySince, "activity_since must clear when leaving running")
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
				enableIdleSweepForTest(t, srv, 15*time.Minute)

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
					assert.Zero(t, disp.stops.Load(), "candidate query must filter out recent live work")
					// Also invoke the worker directly to prove the authoritative worker guard logs and skips.
					srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *agent, reason: state.SuspendReasonIdle}})
					got, err = s.GetAgent(ctx, agent.ID)
					require.NoError(t, err)
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
		enableIdleSweepForTest(t, srv, 15*time.Minute)

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
		enableIdleSweepForTest(t, srv, 15*time.Minute)

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
		enableIdleSweepForTest(t, srv, 15*time.Minute)
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
		enableIdleSweepForTest(t, srv, 15*time.Minute)

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
			name: "heartbeat went stale (offline guard on fresh read)",
			mutate: func(t *testing.T, s store.Store, id string, now time.Time) {
				db := s.(*entadapter.CompositeStore).DB()
				_, err := db.ExecContext(context.Background(),
					"UPDATE agents SET last_seen = ? WHERE id = ?",
					now.Add(-(agentHeartbeatRecency + 15*time.Second)).UTC(), id)
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
			enableIdleSweepForTest(t, srv, 15*time.Minute)

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
	for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
		t.Run(string(reason), func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			disp := &runIntentDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			act := string(state.ActivityIdle)
			if reason == state.SuspendReasonStalled {
				act = string(state.ActivityStalled)
			}
			agent := newIdleCandidateBrokerAgent(t, s, "op-inflight-"+string(reason), act, &pastGrace, now.Add(-20*time.Second))

			end := srv.beginLifecycleOp(agent.ID)
			srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *agent, reason: reason}})

			assert.Zero(t, disp.stops.Load(), "in-flight lifecycle operation must block auto-suspend")
			got, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)

			end()
			srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *agent, reason: reason}})

			assert.Equal(t, int32(1), disp.stops.Load())
			got, err = s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(reason), got.SuspendReason)
		})
	}
}

func TestIdleSuspend_HarnessResumeSupport(t *testing.T) {
	t.Run("non-resumable custom harness config skipped with log", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

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
		enableIdleSweepForTest(t, srv, 15*time.Minute)

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
			enableIdleSweepForTest(t, srv, 15*time.Minute)

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
			require.Equal(t, string(state.ActivityIdle), suspended.Activity, "idle sweep retains pre-suspend activity")
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
			target.Activity = string(state.ActivityCompleted)
			require.NoError(t, s.UpdateAgent(ctx, target))

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
			assert.Equal(t, string(state.ActivityCompleted), got.Activity, "conversion to user suspend must preserve retained activity")
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

	t.Run("experiment off keeps 400 validation error on hub-suspended agent", func(t *testing.T) {
		for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
			ctx := context.Background()
			srv, s, _, target, _ := autoWakeFixture(t, reason)
			setIdleSuspendExperiment(t, srv, false)
			u := wakeLifecycleUser(t, s, "conv-expoff-"+string(reason), target.ProjectID, "agent.lifecycle", "agent.read")

			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Only running agents can be suspended.")

			got, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(reason), got.SuspendReason)
		}
	})

	t.Run("refuses conversion while start claim or lifecycle op is held", func(t *testing.T) {
		ctx := context.Background()
		srv, s, _, target, _ := autoWakeFixture(t, state.SuspendReasonIdle)
		u := wakeLifecycleUser(t, s, "conv-claimed", target.ProjectID, "agent.lifecycle", "agent.read")

		// 1. Held start claim (an auto-wake in progress before writing phase=starting).
		claim, err := s.ClaimAgentStart(ctx, target.ID, "hub-wake", store.StartClaimWake, "", 30*time.Second)
		require.NoError(t, err)

		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		got, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason, "conversion must not overwrite reason while start claim is held")

		_, err = s.ReleaseAgentStart(ctx, target.ID, claim.ID, "hub-wake")
		require.NoError(t, err)

		// 2. Held in-process lifecycle op.
		endOp := srv.beginLifecycleOp(target.ID)
		rec = requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		got, err = s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		endOp()

		// Once released, conversion succeeds.
		rec = requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err = s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
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

type afterWorkerReadStore struct {
	store.Store
	fault     *storeFaultSwitch
	targetID  string
	fired     atomic.Bool
	afterRead func()
}

func (w *afterWorkerReadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := w.Store.GetAgent(ctx, id)
	if err == nil && w.fault.Active() && id == w.targetID && w.fired.CompareAndSwap(false, true) {
		w.afterRead()
	}
	return a, err
}

type stopMutatingDispatcher struct {
	runIntentDispatcher
	onStop func(ctx context.Context, a *store.Agent)
}

func (d *stopMutatingDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	if d.onStop != nil {
		d.onStop(ctx, a)
	}
	return d.runIntentDispatcher.DispatchAgentStop(ctx, a)
}

type blockingStopWakeDispatcher struct {
	wakeTrackingDispatcher
	stopOnce    sync.Once
	stopEntered chan struct{}
	releaseStop chan struct{}
}

func (d *blockingStopWakeDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	d.stopOnce.Do(func() { close(d.stopEntered) })
	select {
	case <-d.releaseStop:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// TestIdleSuspend_PostReadTOCTOU verifies C1: state changes or deliveries that
// land AFTER autoSuspendAgent's initial GetAgent read (either before
// ClaimIdleSuspend or during DispatchAgentStop) never lose a message and never
// leave an active/blocked/waiting agent idle-suspended.
func TestIdleSuspend_PostReadTOCTOU(t *testing.T) {
	stateCases := []struct {
		name   string
		mutate func(t *testing.T, dbExec func(query string, args ...any))
	}{
		{
			name: "blocked",
			mutate: func(_ *testing.T, dbExec func(string, ...any)) {
				now := time.Now().UTC()
				dbExec("UPDATE agents SET activity = ?, activity_since = ? WHERE 1=1", string(state.ActivityBlocked), now)
			},
		},
		{
			name: "waiting_for_input",
			mutate: func(_ *testing.T, dbExec func(string, ...any)) {
				now := time.Now().UTC()
				dbExec("UPDATE agents SET activity = ?, activity_since = ? WHERE 1=1", string(state.ActivityWaitingForInput), now)
			},
		},
		{
			name: "live_work",
			mutate: func(_ *testing.T, dbExec func(string, ...any)) {
				now := time.Now().UTC()
				dbExec("UPDATE agents SET last_live_process_at = ? WHERE 1=1", now)
			},
		},
	}

	for _, tc := range stateCases {
		t.Run("before_claim/"+tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, rawStore, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *afterWorkerReadStore {
				return &afterWorkerReadStore{Store: inner, fault: f}
			})
			disp := &runIntentDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			snapshot := newIdleCandidateBrokerAgent(t, rawStore, "preclaim-"+tc.name, string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
			db := rawStore.(*entadapter.CompositeStore).DB()

			wrapped.targetID = snapshot.ID
			wrapped.afterRead = func() {
				tc.mutate(t, func(q string, args ...any) {
					_, err := db.ExecContext(ctx, q+" AND id = ?", append(args, snapshot.ID)...)
					require.NoError(t, err)
				})
			}
			fault.Arm()

			srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *snapshot, reason: state.SuspendReasonIdle}})

			assert.Zero(t, disp.stops.Load(), "ClaimIdleSuspend must abort before DispatchAgentStop when %s lands after GetAgent", tc.name)
			got, err := rawStore.GetAgent(ctx, snapshot.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
			assert.Empty(t, got.SuspendReason)
			assert.Equal(t, store.RunIntentRunning, got.RunIntent)
			assert.False(t, got.HasActiveIdleSuspendClaim(time.Now()))
		})

		t.Run("during_stop_dispatch/"+tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			db := s.(*entadapter.CompositeStore).DB()
			disp := &stopMutatingDispatcher{
				onStop: func(_ context.Context, a *store.Agent) {
					tc.mutate(t, func(q string, args ...any) {
						_, err := db.ExecContext(ctx, q+" AND id = ?", append(args, a.ID)...)
						require.NoError(t, err)
					})
				},
			}
			srv.SetDispatcher(disp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			snapshot := newIdleCandidateBrokerAgent(t, s, "midstop-"+tc.name, string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))

			srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *snapshot, reason: state.SuspendReasonIdle}})

			got, err := s.GetAgent(ctx, snapshot.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase, "recordStopStatus IfIdleSuspend must reject when %s lands during stop", tc.name)
			assert.Empty(t, got.SuspendReason)
			assert.Equal(t, store.RunIntentRunning, got.RunIntent)
			assert.False(t, got.HasActiveIdleSuspendClaim(time.Now()))
		})
	}

	for _, dmPath := range []string{"agent_dm", "user_dm"} {
		t.Run("message_delivered_before_claim/"+dmPath, func(t *testing.T) {
			ctx := context.Background()
			srv, rawStore, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
			wrapped, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *afterWorkerReadStore {
				return &afterWorkerReadStore{Store: inner, fault: f}
			})
			disp := &wakeTrackingDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)

			_, err := rawStore.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
			require.NoError(t, err)
			require.NoError(t, rawStore.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
				Phase:    string(state.PhaseRunning),
				Activity: string(state.ActivityIdle),
			}))
			now := time.Now().UTC()
			db := rawStore.(*entadapter.CompositeStore).DB()
			_, err = db.ExecContext(ctx,
				"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
				"run-dm-pre-"+dmPath, now.Add(-20*time.Minute), now.Add(-15*time.Second), target.ID,
			)
			require.NoError(t, err)
			snapshot, err := rawStore.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			u := wakeLifecycleUser(t, rawStore, "dm-pre-"+dmPath, target.ProjectID, "agent.message", "agent.read")

			wrapped.targetID = snapshot.ID
			wrapped.afterRead = func() {
				// Deliver a DM right after the worker read the candidate,
				// before ClaimIdleSuspend runs.
				if dmPath == "agent_dm" {
					res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, snapshot, "hello before claim"))
					require.Nil(t, dmErr)
					require.Equal(t, AgentDMAccepted, res.Outcome)
				} else {
					rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+snapshot.ID+"/message",
						map[string]any{"message": "hello before claim"})
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				}
			}
			fault.Arm()

			srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *snapshot, reason: state.SuspendReasonIdle}})

			got, err := rawStore.GetAgent(ctx, snapshot.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase, "message delivered before claim must prevent idle suspend")
			assert.Empty(t, got.SuspendReason)
			assert.Empty(t, disp.getStartCalls())
			msgs := disp.getMessageCalls()
			require.Len(t, msgs, 1)
			assert.Equal(t, "hello before claim", msgs[0].Message)
		})

		t.Run("message_during_claimed_stop_awaits_and_autowakes/"+dmPath, func(t *testing.T) {
			ctx := context.Background()
			srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
			disp := &blockingStopWakeDispatcher{
				stopEntered: make(chan struct{}),
				releaseStop: make(chan struct{}),
			}
			srv.SetDispatcher(disp)
			t.Cleanup(srv.waitAutoWakeIdle)
			enableIdleSweepForTest(t, srv, 15*time.Minute)

			_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
			require.NoError(t, err)
			require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
				Phase:    string(state.PhaseRunning),
				Activity: string(state.ActivityCompleted),
			}))
			now := time.Now().UTC()
			db := s.(*entadapter.CompositeStore).DB()
			_, err = db.ExecContext(ctx,
				"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
				"run-dm-mid-"+dmPath, now.Add(-20*time.Minute), now.Add(-15*time.Second), target.ID,
			)
			require.NoError(t, err)
			snapshot, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			u := wakeLifecycleUser(t, s, "dm-mid-"+dmPath, target.ProjectID, "agent.message", "agent.read")
			awReadyOnStart(t, s, target.ID)

			sweepDone := make(chan struct{})
			go func() {
				defer close(sweepDone)
				srv.autoSuspendAgents(ctx, []autoSuspendItem{{agent: *snapshot, reason: state.SuspendReasonIdle}})
			}()

			// Wait until ClaimIdleSuspend has committed and DispatchAgentStop is in flight.
			<-disp.stopEntered
			claimed, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			require.True(t, claimed.HasActiveIdleSuspendClaim(time.Now()), "idle-suspend claim must be held during DispatchAgentStop")

			// Start the DM while the container is being stopped.
			dmDone := make(chan error, 1)
			go func() {
				if dmPath == "agent_dm" {
					res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, snapshot, "hello during stop"))
					if dmErr != nil {
						dmDone <- fmt.Errorf("dmErr: %s: %s", dmErr.Code, dmErr.Message)
						return
					}
					if res.Outcome != AgentDMAccepted {
						dmDone <- fmt.Errorf("unexpected outcome: %v", res.Outcome)
						return
					}
					dmDone <- nil
				} else {
					rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+snapshot.ID+"/message",
						map[string]any{"message": "hello during stop"})
					if rec.Code != http.StatusOK {
						dmDone <- fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
						return
					}
					dmDone <- nil
				}
			}()

			// Give the DM goroutine time to enter MarkAgentDelivery + waitForIdleSuspendToSettle;
			// it must NOT deliver into the dying container while stop is in flight.
			time.Sleep(25 * time.Millisecond)
			assert.Empty(t, disp.getMessageCalls(), "DM must not dispatch into dying container while claim is held")

			// Let DispatchAgentStop finish -> agent suspends -> DM awaits, auto-wakes, and delivers.
			close(disp.releaseStop)
			<-sweepDone
			require.NoError(t, <-dmDone)

			require.Len(t, disp.getStartCalls(), 1, "agent must be auto-woken after idle suspend settles")
			assert.True(t, disp.getStartCalls()[0].Continue)
			msgs := disp.getMessageCalls()
			require.Len(t, msgs, 1)
			assert.Equal(t, "hello during stop", msgs[0].Message)

			after, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), after.Phase)
			assert.Equal(t, string(state.ActivityIdle), after.Activity, "retained completed activity must clear on resume and adopt woken activity")
			assert.Empty(t, after.SuspendReason)
			assert.False(t, after.HasActiveIdleSuspendClaim(time.Now()))
		})
	}
}

func TestIdleSuspend_RetainedActivityDisplayAndSuspendedOnlyNotification(t *testing.T) {
	cases := []struct {
		act       state.Activity
		wantLabel string
	}{
		{act: state.ActivityIdle, wantLabel: "suspended · idle (idle)"},
		{act: state.ActivityCompleted, wantLabel: "suspended · completed (idle)"},
		{act: state.ActivityHarnessError, wantLabel: "suspended · harness error (idle)"},
	}

	for _, tc := range cases {
		t.Run(string(tc.act), func(t *testing.T) {
			ctx := context.Background()
			srv, s, _, _ := engineTestServer(t)
			stopDisp := &runIntentDispatcher{}
			srv.SetDispatcher(stopDisp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)

			statusCh, unsub := srv.events.Subscribe("project.>.agent.status")
			defer unsub()

			notifDisp := &recordingDispatcher{}
			nd := NewNotificationDispatcher(s, srv.events, func() AgentDispatcher { return notifDisp }, slog.Default())
			nd.Start()
			defer nd.Stop()

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			freshSeen := now.Add(-20 * time.Second)
			agent := newIdleCandidateBrokerAgent(t, s, "ret-notif-"+string(tc.act), string(tc.act), &pastGrace, freshSeen)

			subscriber := &store.Agent{
				ID:              tid("idle-notif-sub-" + string(tc.act)),
				Name:            "idle-notif-sub-" + string(tc.act),
				Slug:            "idle-notif-sub-" + string(tc.act),
				ProjectID:       agent.ProjectID,
				RuntimeBrokerID: agent.RuntimeBrokerID,
				Phase:           string(state.PhaseRunning),
			}
			require.NoError(t, s.CreateAgent(ctx, subscriber))

			sub := &store.NotificationSubscription{
				ID:                tid("idle-notif-subrec-" + string(tc.act)),
				Scope:             store.SubscriptionScopeAgent,
				AgentID:           agent.ID,
				ProjectID:         agent.ProjectID,
				SubscriberType:    store.SubscriberTypeAgent,
				SubscriberID:      subscriber.Slug,
				TriggerActivities: []string{"COMPLETED", "IDLE", "HARNESS_ERROR", "STALLED", "SUSPENDED"},
				CreatedAt:         now,
				CreatedBy:         "test",
			}
			require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

			srv.agentIdleSuspendHandler()(ctx)
			srv.waitAutoSuspendIdle()

			persisted, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), persisted.Phase)
			assert.Equal(t, string(tc.act), persisted.Activity)
			assert.Equal(t, string(state.SuspendReasonIdle), persisted.SuspendReason)
			assert.Nil(t, persisted.ActivitySince)

			// Verify API response fields used to render "suspended · <activity> (idle)".
			rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var apiAgent store.Agent
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiAgent))
			assert.Equal(t, string(state.PhaseSuspended), apiAgent.Phase)
			assert.Equal(t, string(tc.act), apiAgent.Activity)
			assert.Equal(t, string(state.SuspendReasonIdle), apiAgent.SuspendReason)
			renderedPhase := apiAgent.Phase + " · " + state.Activity(apiAgent.Activity).DisplayLabel() + " (" + apiAgent.SuspendReason + ")"
			assert.Equal(t, tc.wantLabel, renderedPhase)

			// Verify SSE status event carries both suspended phase and retained activity.
			evt, raw := waitAgentStatusEvent(t, statusCh, agent.ID)
			assert.Equal(t, string(state.PhaseSuspended), evt.Phase)
			assert.Equal(t, string(tc.act), evt.Activity)
			assert.Equal(t, string(tc.act), raw["activity"])

			// Verify NotificationDispatcher fires SUSPENDED only (never the retained activity).
			require.Eventually(t, func() bool {
				return len(notifDisp.getCalls()) >= 1
			}, 2*time.Second, 20*time.Millisecond)
			time.Sleep(100 * time.Millisecond)

			calls := notifDisp.getCalls()
			require.Len(t, calls, 1, "idle sweep suspend of %s must fire SUSPENDED only", tc.act)
			assert.Contains(t, calls[0].Message, "SUSPENDED")
			assert.NotContains(t, calls[0].Message, strings.ToUpper(string(tc.act)))

			notifs, err := s.GetNotifications(ctx, store.SubscriberTypeAgent, subscriber.Slug, false)
			require.NoError(t, err)
			require.Len(t, notifs, 1)
			assert.Equal(t, "SUSPENDED", notifs[0].Status)
		})
	}
}
