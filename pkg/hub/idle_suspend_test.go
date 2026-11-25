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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
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
		srv, s, counter, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *countingIdleCandidatesStore {
			return &countingIdleCandidatesStore{Store: inner}
		})
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSweepForTest(t, srv, 15*time.Minute)
		setIdleSuspendExperiment(t, srv, false)

		now := time.Now().UTC()
		pastGrace := now.Add(-30 * time.Minute)
		agent := newIdleCandidateBrokerAgent(t, s, "exp-off", string(state.ActivityIdle), &pastGrace, now.Add(-20*time.Second))

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		assert.Zero(t, counter.listCalls.Load(), "ListIdleSuspendCandidates must not be queried when experiment is off")
		assert.Zero(t, disp.stops.Load())
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})

	t.Run("setting off does nothing even with experiment on", func(t *testing.T) {
		ctx := context.Background()
		srv, s, counter, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *countingIdleCandidatesStore {
			return &countingIdleCandidatesStore{Store: inner}
		})
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

		assert.Zero(t, counter.listCalls.Load(), "ListIdleSuspendCandidates must not be queried when auto_suspend_idle is off")
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

	t.Run("already user-suspended is an idempotent 200 OK no-op", func(t *testing.T) {
		ctx := context.Background()
		srv, s, _, target, disp := autoWakeFixture(t, state.SuspendReasonUser)
		target.Activity = string(state.ActivityCompleted)
		require.NoError(t, s.UpdateAgent(ctx, target))

		// Mark the broker offline to prove no runtime dispatch occurs.
		broker, err := s.GetRuntimeBroker(ctx, target.RuntimeBrokerID)
		require.NoError(t, err)
		broker.Status = store.BrokerStatusOffline
		require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

		u := wakeLifecycleUser(t, s, "conv-already-user", target.ProjectID, "agent.lifecycle", "agent.read")
		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.ActivityCompleted), got.Activity)
		assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("empty suspend_reason sets user when unheld and preserves empty when held", func(t *testing.T) {
		t.Run("unheld legacy row sets user", func(t *testing.T) {
			ctx := context.Background()
			srv, s, _, target, _ := autoWakeFixture(t, "")
			target.Activity = string(state.ActivityIdle)
			require.NoError(t, s.UpdateAgent(ctx, target))

			broker, err := s.GetRuntimeBroker(ctx, target.RuntimeBrokerID)
			require.NoError(t, err)
			broker.Status = store.BrokerStatusOffline
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			u := wakeLifecycleUser(t, s, "conv-empty-unheld", target.ProjectID, "agent.lifecycle", "agent.read")
			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(state.ActivityIdle), got.Activity)
			assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
		})

		t.Run("held row preserves empty suspend_reason", func(t *testing.T) {
			ctx := context.Background()
			srv, s, _, target, _ := autoWakeFixture(t, "")
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{{
				AgentID:           target.ID,
				ProjectID:         target.ProjectID,
				Cause:             store.AgentHoldCauseOwnerAccessEnded,
				RootPrincipalType: store.AgentHoldRootUser,
				RootPrincipalID:   "11111111-1111-1111-1111-111111111111",
				Trigger:           store.MembershipLossTriggerMemberRemove,
			}})
			require.NoError(t, err)

			u := wakeLifecycleUser(t, s, "conv-empty-held", target.ProjectID, "agent.lifecycle", "agent.read")
			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Empty(t, got.SuspendReason, "held agent must preserve empty suspend_reason")
		})
	})

	t.Run("held hub-suspended agent converts suspend_reason to user and does not auto-wake after hold lift", func(t *testing.T) {
		for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
			ctx := context.Background()
			srv, s, sender, target, disp := autoWakeFixture(t, reason)
			target.Activity = string(state.ActivityCompleted)
			require.NoError(t, s.UpdateAgent(ctx, target))
			_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{{
				AgentID:           target.ID,
				ProjectID:         target.ProjectID,
				Cause:             store.AgentHoldCauseOwnerAccessEnded,
				RootPrincipalType: store.AgentHoldRootUser,
				RootPrincipalID:   "11111111-1111-1111-1111-111111111111",
				Trigger:           store.MembershipLossTriggerMemberRemove,
			}})
			require.NoError(t, err)

			// Mark the broker offline to prove conversion performs no runtime dispatch.
			broker, err := s.GetRuntimeBroker(ctx, target.RuntimeBrokerID)
			require.NoError(t, err)
			broker.Status = store.BrokerStatusOffline
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			u := wakeLifecycleUser(t, s, "conv-held-"+string(reason), target.ProjectID, "agent.lifecycle", "agent.read")
			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(state.ActivityCompleted), got.Activity, "conversion must preserve retained activity")
			assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason, "held hub-suspended agent must convert suspend_reason to user")

			// Lift the hold and bring the broker back online: future DMs without --wake must not auto-wake.
			cleared, err := s.ClearAgentHolds(ctx, target.ID, store.ClearActor{
				Kind: store.ClearActorUser,
				ID:   "11111111-1111-1111-1111-111111111111",
			}, "lift hold")
			require.NoError(t, err)
			require.Equal(t, 1, cleared)
			broker.Status = store.BrokerStatusOnline
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			_, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, got, "should not auto-wake after hold lift"))
			require.NotNil(t, dmErr)
			assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
			assert.Empty(t, disp.getStartCalls())
		}
	})

	t.Run("experiment off still converts hub-suspended agent to user", func(t *testing.T) {
		for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
			ctx := context.Background()
			srv, s, _, target, _ := autoWakeFixture(t, reason)
			target.Activity = string(state.ActivityCompleted)
			require.NoError(t, s.UpdateAgent(ctx, target))
			setIdleSuspendExperiment(t, srv, false)

			broker, err := s.GetRuntimeBroker(ctx, target.RuntimeBrokerID)
			require.NoError(t, err)
			broker.Status = store.BrokerStatusOffline
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			u := wakeLifecycleUser(t, s, "conv-expoff-"+string(reason), target.ProjectID, "agent.lifecycle", "agent.read")
			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			got, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(state.ActivityCompleted), got.Activity)
			assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
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
			assert.Equal(t, string(state.PhaseSuspended), got.Phase, "once ClaimIdleSuspend commits and DispatchAgentStop succeeds, %s during stop must still record suspended(idle)", tc.name)
			assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "run_intent must never revert to running after a successful DispatchAgentStop")
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
			nd.idleSuspendGateFn = srv.idleSuspendDeliveryGateActive
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

// containerShutdownReportDispatcher simulates sciontool init's graceful
// SIGTERM shutdown reports landing while DispatchAgentStop waits for the
// container to exit, and also supports subsequent wake starts and DM dispatches.
type containerShutdownReportDispatcher struct {
	wakeTrackingDispatcher
	postStopped bool
	store       store.Store
	t           *testing.T
}

func (d *containerShutdownReportDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	// 1. reportStoppingToHub (cmd/sciontool/commands/init.go:2017).
	require.NoError(d.t, d.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:              string(state.PhaseStopping),
		Message:            "Agent shutting down",
		ReportSource:       store.StatusReportSourceAgent,
		ReportRunID:        agent.RunID,
		ReportAgentToken:   true,
		GuardReincarnation: true,
	}))
	if d.postStopped {
		// 2. finalStatusUpdate(exitOutcome{signaled: true}) (init.go:2222).
		require.NoError(d.t, d.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase:              string(state.PhaseStopped),
			Message:            "Agent stopped",
			ExitReason:         string(state.ExitReasonSignaled),
			ReportSource:       store.StatusReportSourceAgent,
			ReportRunID:        agent.RunID,
			ReportAgentToken:   true,
			GuardReincarnation: true,
		}))
	}
	return nil
}

func TestIdleSuspend_ContainerStoppingAndStoppedReports(t *testing.T) {
	for _, postStopped := range []bool{false, true} {
		mode := "stopping_only"
		if postStopped {
			mode = "stopping_then_stopped"
		}
		for _, act := range []state.Activity{state.ActivityIdle, state.ActivityCompleted, state.ActivityHarnessError} {
			t.Run(mode+"/"+string(act), func(t *testing.T) {
				ctx := context.Background()
				srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
				disp := &containerShutdownReportDispatcher{
					postStopped: postStopped,
					store:       s,
					t:           t,
				}
				srv.SetDispatcher(disp)
				t.Cleanup(srv.waitAutoWakeIdle)
				enableIdleSweepForTest(t, srv, 15*time.Minute)
				awReadyOnStart(t, s, target.ID)

				_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
				require.NoError(t, err)
				require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
					Phase:    string(state.PhaseRunning),
					Activity: string(act),
				}))
				now := time.Now().UTC()
				db := s.(*entadapter.CompositeStore).DB()
				_, err = db.ExecContext(ctx,
					"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
					"run-shutdown-"+mode+"-"+string(act), now.Add(-20*time.Minute), now.Add(-15*time.Second), target.ID,
				)
				require.NoError(t, err)

				srv.agentIdleSuspendHandler()(ctx)
				srv.waitAutoSuspendIdle()

				suspended, err := s.GetAgent(ctx, target.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.PhaseSuspended), suspended.Phase)
				assert.Equal(t, string(act), suspended.Activity, "retained pre-suspend activity must survive %s shutdown reports", mode)
				assert.Equal(t, string(state.SuspendReasonIdle), suspended.SuspendReason)
				assert.Equal(t, store.RunIntentStopped, suspended.RunIntent)
				assert.False(t, suspended.HasActiveIdleSuspendClaim(time.Now()))

				// Later DM without --wake must auto-wake and deliver.
				res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, suspended, "wake after container shutdown report"))
				require.Nil(t, dmErr)
				require.Equal(t, AgentDMAccepted, res.Outcome)

				require.Len(t, disp.getStartCalls(), 1)
				assert.True(t, disp.getStartCalls()[0].Continue)
				msgs := disp.getMessageCalls()
				require.Len(t, msgs, 1)
				assert.Equal(t, "wake after container shutdown report", msgs[0].Message)

				woken, err := s.GetAgent(ctx, target.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.PhaseRunning), woken.Phase)
				assert.Empty(t, woken.SuspendReason)
				assert.Equal(t, store.RunIntentRunning, woken.RunIntent)
			})
		}
	}
}

func claimIdleForTest(t *testing.T, s store.Store, target *store.Agent, runID string) (*store.Agent, store.IdleSuspendCondition) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	idleSince := now.Add(-20 * time.Minute).Truncate(time.Microsecond)
	db := s.(*entadapter.CompositeStore).DB()
	_, err := db.ExecContext(ctx,
		"UPDATE agents SET phase = ?, activity = ?, run_id = ?, activity_since = ?, last_seen = ?, last_delivery_at = NULL, idle_suspend_claim_until = NULL WHERE id = ?",
		string(state.PhaseRunning), string(state.ActivityIdle), runID, idleSince, now.Add(-15*time.Second), target.ID,
	)
	require.NoError(t, err)
	fresh, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	return fresh, store.IdleSuspendCondition{
		RunID:            fresh.RunID,
		Activity:         fresh.Activity,
		ActivitySince:    *fresh.ActivitySince,
		IdleSuspendAfter: 15 * time.Minute,
		HeartbeatRecency: 2 * time.Minute,
		ClaimTTL:         30 * time.Second,
	}
}

func TestIdleSuspend_DelegatedSenderDMWithActiveClaim(t *testing.T) {
	t.Run("preflight claim populated by GetAgent rejects before persistence", func(t *testing.T) {
		ctx := context.Background()
		srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
		disp := &recordingDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		target, cond := claimIdleForTest(t, s, target, "run-delegated-claim")
		_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
		require.NoError(t, err)
		assert.True(t, func() bool {
			fresh, gerr := s.GetAgent(ctx, target.ID)
			require.NoError(t, gerr)
			return fresh.IdleSuspendClaimActive
		}(), "GetAgent must populate IdleSuspendClaimActive against the store clock")

		bodyBytes, err := json.Marshal(map[string]any{"message": "delegated dm during claim"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		delegated := &DelegatedAgentIdentity{
			agentID:           sender.ID,
			agentProjectID:    sender.ProjectID,
			authorizingUserID: DevUserID,
			grantID:           tid("delegated-claim-grant"),
		}
		req = req.WithContext(contextWithIdentity(req.Context(), delegated))
		rec := httptest.NewRecorder()
		srv.handleAgentMessage(rec, req, target.ID)

		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeAgentNotRunning)
		assert.Empty(t, disp.getCalls(), "delegatedSender DM must never dispatch into dying container while claim is active")

		msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items, "preflight refusal must not persist a message")
	})

	t.Run("claim acquired after preflight read rejects at dispatch gate and marks row failed", func(t *testing.T) {
		ctx := context.Background()
		srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
		disp := &recordingDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		target, cond := claimIdleForTest(t, s, target, "run-delegated-race-claim")
		installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *claimBeforeMarkStore {
			return &claimBeforeMarkStore{
				Store:       inner,
				targetID:    target.ID,
				claimBefore: func() { _, _, _, _ = s.ClaimIdleSuspend(ctx, target.ID, cond) },
			}
		})

		bodyBytes, err := json.Marshal(map[string]any{"message": "delegated dm racing claim"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		delegated := &DelegatedAgentIdentity{
			agentID:           sender.ID,
			agentProjectID:    sender.ProjectID,
			authorizingUserID: DevUserID,
			grantID:           tid("delegated-race-claim-grant"),
		}
		req = req.WithContext(contextWithIdentity(req.Context(), delegated))
		rec := httptest.NewRecorder()
		srv.handleAgentMessage(rec, req, target.ID)

		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeAgentNotRunning)
		assert.Empty(t, disp.getCalls(), "delegatedSender DM must never dispatch into dying container when claim wins before MarkAgentDelivery")

		msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, msgs.Items, 1)
		assert.Equal(t, store.MessageDispatchFailed, msgs.Items[0].DispatchState)
	})
}

func TestIdleSuspend_NonAutoWakePathsWithActiveClaimAndPreClaimMark(t *testing.T) {
	paths := []struct {
		name string
		run  func(t *testing.T, srv *Server, s store.Store, u *store.User, sender, target *store.Agent)
	}{
		{
			name: "message_broker_proxy",
			run: func(t *testing.T, srv *Server, s store.Store, _ *store.User, sender, target *store.Agent) {
				bus := eventbus.NewInProcessEventBus(slog.Default())
				t.Cleanup(func() { _ = bus.Close() })
				proxy := NewMessageBrokerProxy(bus, s, srv.events, srv.GetDispatcher, slog.Default())
				proxy.SetIdleSuspendGate(srv.idleSuspendDeliveryGateActive)
				sm := &messages.StructuredMessage{
					Version:     messages.Version,
					Timestamp:   time.Now().UTC().Format(time.RFC3339),
					Sender:      "agent:" + sender.Slug,
					SenderID:    sender.ID,
					Recipient:   "agent:" + target.Slug,
					RecipientID: target.ID,
					Msg:         "broker proxy msg",
					Type:        messages.TypeInstruction,
				}
				proxy.deliverToAgent(context.Background(), target.ProjectID, target.Slug, sm)
			},
		},
		{
			name: "broadcast_direct",
			run: func(t *testing.T, srv *Server, _ store.Store, u *store.User, _ *store.Agent, target *store.Agent) {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+target.ProjectID+"/broadcast", nil)
				req = req.WithContext(contextWithIdentity(req.Context(), authUser(u)))
				rec := httptest.NewRecorder()
				sm := &messages.StructuredMessage{
					Version:   messages.Version,
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Sender:    "user:" + u.ID,
					SenderID:  u.ID,
					Recipient: "broadcast",
					Msg:       "broadcast msg",
					Type:      messages.TypeInstruction,
				}
				srv.broadcastDirect(rec, req, target.ProjectID, sm, false, []store.Agent{*target})
			},
		},
		{
			name: "broker_inbound",
			run: func(t *testing.T, srv *Server, _ store.Store, u *store.User, _ *store.Agent, target *store.Agent) {
				bodyBytes, err := json.Marshal(inboundMessageRequest{
					Topic: "scion.project." + target.ProjectID + ".agent." + target.Slug + ".messages",
					Message: &messages.StructuredMessage{
						Version:   messages.Version,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						Sender:    "user:" + u.Email,
						SenderID:  u.ID,
						Recipient: "agent:" + target.Slug,
						Msg:       "inbound msg",
						Type:      messages.TypeInstruction,
						Channel:   "slack",
					},
				})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(bodyBytes))
				req.Header.Set("Content-Type", "application/json")
				req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(target.RuntimeBrokerID)))
				rec := httptest.NewRecorder()
				srv.handleBrokerInbound(rec, req)
			},
		},
		{
			name: "routed_inbound",
			run: func(t *testing.T, srv *Server, _ store.Store, u *store.User, _ *store.Agent, target *store.Agent) {
				reqPayload := &routedInboundRequest{
					ProjectID: target.ProjectID,
					Message: &messages.StructuredMessage{
						Version:   messages.Version,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						Sender:    "user:" + u.ID,
						SenderID:  u.ID,
						Recipient: "agent:" + target.Slug,
						Msg:       "routed inbound msg",
						Type:      messages.TypeInstruction,
						Channel:   "slack",
					},
				}
				_ = srv.dispatchRoutedRecipient(context.Background(), dispatchRoutedParams{
					agent:            target,
					isPrimary:        true,
					sender:           authUser(u),
					req:              reqPayload,
					now:              time.Now().UTC(),
					primaryRecipient: "agent:" + target.Slug,
				})
			},
		},
		{
			name: "reconcile_deliver_message",
			run: func(t *testing.T, srv *Server, _ store.Store, _ *store.User, _ *store.Agent, target *store.Agent) {
				_ = srv.deliverMessage(context.Background(), &store.Message{
					ID:      tid("reconcile-msg-" + target.ID),
					AgentID: target.ID,
					Msg:     "reconcile msg",
				})
			},
		},
		{
			name: "artifact_review",
			run: func(t *testing.T, srv *Server, _ store.Store, _ *store.User, _ *store.Agent, target *store.Agent) {
				_ = srv.deliverArtifactReview(context.Background(), artifacts.ReviewNotice{
					ArtifactID:   tid("art-" + target.ID),
					Seq:          2,
					OwnerRef:     target.ID,
					ReviewerKind: artifacts.PrincipalKindUser,
				})
			},
		},
		{
			name: "decision_response",
			run: func(t *testing.T, srv *Server, s store.Store, u *store.User, _ *store.Agent, target *store.Agent) {
				convID := tid("decision-conv-" + target.ID)
				require.NoError(t, s.CreateConversation(context.Background(), &store.Conversation{
					ID:        convID,
					Kind:      "group",
					Surface:   "native",
					ProjectID: &target.ProjectID,
					CreatedAt: time.Now().UTC(),
				}))
				msgID := tid("decision-msg-" + target.ID)
				m := &store.Message{
					ID:             msgID,
					ProjectID:      target.ProjectID,
					Sender:         "user:" + u.ID,
					SenderID:       u.ID,
					Recipient:      "agent:" + target.Slug,
					RecipientID:    target.ID,
					AgentID:        target.ID,
					Msg:            "answer A",
					Type:           messages.TypeDecisionResponse,
					ConversationID: convID,
					DispatchState:  store.MessageDispatchDispatched,
					CreatedAt:      time.Now().UTC(),
				}
				require.NoError(t, s.CreateMessage(context.Background(), m))
				srv.deliverDecisionResponse(context.Background(), target, m, &store.DecisionAnswer{
					ChoiceID:   tid("choice-id"),
					Key:        "A",
					Version:    1,
					Provenance: "selected",
				}, nil)
			},
		},
		{
			name: "decision_set_response",
			run: func(t *testing.T, srv *Server, s store.Store, u *store.User, _ *store.Agent, target *store.Agent) {
				convID := tid("decisionset-conv-" + target.ID)
				require.NoError(t, s.CreateConversation(context.Background(), &store.Conversation{
					ID:        convID,
					Kind:      "group",
					Surface:   "native",
					ProjectID: &target.ProjectID,
					CreatedAt: time.Now().UTC(),
				}))
				msgID := tid("decisionset-msg-" + target.ID)
				m := &store.Message{
					ID:             msgID,
					ProjectID:      target.ProjectID,
					Sender:         "user:" + u.ID,
					SenderID:       u.ID,
					Recipient:      "agent:" + target.Slug,
					RecipientID:    target.ID,
					AgentID:        target.ID,
					Msg:            "batch answers",
					Type:           messages.TypeDecisionResponse,
					ConversationID: convID,
					DispatchState:  store.MessageDispatchDispatched,
					CreatedAt:      time.Now().UTC(),
				}
				require.NoError(t, s.CreateMessage(context.Background(), m))
				srv.deliverDecisionSetResponse(context.Background(), target, m, tid("prompt-id"), &store.AnswerSetResult{
					BatchID: tid("batch-id"),
				}, nil)
			},
		},
	}

	for _, tc := range paths {
		t.Run(tc.name+"/active_claim_blocks_dispatch", func(t *testing.T) {
			ctx := context.Background()
			srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
			disp := &recordingDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)
			u := wakeLifecycleUser(t, s, "nonwake-claim-"+tc.name, target.ProjectID, "agent.message", "agent.read")

			target, cond := claimIdleForTest(t, s, target, "run-nonwake-claim-"+tc.name)
			_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
			require.NoError(t, err)

			tc.run(t, srv, s, u, sender, target)
			var targetCalls int
			for _, c := range disp.getCalls() {
				if c.Agent != nil && c.Agent.ID == target.ID {
					targetCalls++
				}
			}
			assert.Zero(t, targetCalls, "%s must never dispatch into dying container while idle-suspend claim is active", tc.name)
		})

		t.Run(tc.name+"/pre_claim_delivery_stamps_last_delivery_at", func(t *testing.T) {
			ctx := context.Background()
			srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
			disp := &recordingDispatcher{}
			srv.SetDispatcher(disp)
			enableIdleSweepForTest(t, srv, 15*time.Minute)
			u := wakeLifecycleUser(t, s, "nonwake-mark-"+tc.name, target.ProjectID, "agent.message", "agent.read")

			target, cond := claimIdleForTest(t, s, target, "run-nonwake-mark-"+tc.name)
			tc.run(t, srv, s, u, sender, target)
			var targetCalls int
			for _, c := range disp.getCalls() {
				if c.Agent != nil && c.Agent.ID == target.ID {
					targetCalls++
				}
			}
			require.Equal(t, 1, targetCalls, "%s must dispatch when running with no claim", tc.name)

			after, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			require.NotNil(t, after.LastDeliveryAt, "%s must stamp last_delivery_at via dispatchMarkedAgentMessage", tc.name)

			_, _, _, err = s.ClaimIdleSuspend(ctx, target.ID, cond)
			assert.ErrorIs(t, err, store.ErrIdleSuspendPrecondition, "%s delivery mark must block racing ClaimIdleSuspend", tc.name)
		})
	}
}

type failingMarkDeliveryStore struct {
	store.Store
	fault *storeFaultSwitch
	err   error
}

func (w *failingMarkDeliveryStore) MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error) {
	if w.fault.Active() {
		return nil, false, w.err
	}
	return w.Store.MarkAgentDelivery(ctx, agentID)
}

func TestIdleSuspend_MarkAgentDeliveryStoreErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("simulated MarkAgentDelivery store failure")
	srv, rawStore, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failingMarkDeliveryStore {
		return &failingMarkDeliveryStore{Store: inner, fault: f, err: boom}
	})
	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	now := time.Now().UTC()
	pastGrace := now.Add(-20 * time.Minute)
	target := newIdleCandidateBrokerAgent(t, rawStore, "fail-closed-target", string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
	sender := newIdleCandidateBrokerAgent(t, rawStore, "fail-closed-sender", string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
	sender.ProjectID = target.ProjectID
	_, err := rawStore.(*entadapter.CompositeStore).DB().ExecContext(ctx,
		"UPDATE agents SET project_id = ? WHERE id = ?", target.ProjectID, sender.ID)
	require.NoError(t, err)

	fault.Arm()

	t.Run("ExecuteAgentDM fails closed and marks message failed not ambiguous", func(t *testing.T) {
		res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, target, "dm when MarkAgentDelivery fails"))
		assert.Nil(t, res, "definite pre-dispatch mark failure must not return AgentDMAmbiguous")
		require.NotNil(t, dmErr)
		assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus)
		assert.Empty(t, disp.getCalls(), "DispatchAgentMessage must never run when MarkAgentDelivery errors")

		msgs, err := rawStore.ListMessages(ctx, store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, msgs.Items, 1)
		assert.Equal(t, store.MessageDispatchFailed, msgs.Items[0].DispatchState)
	})

	t.Run("handleAgentMessage fails closed with zero dispatches", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+target.ID+"/message", map[string]any{
			"message": "user dm when MarkAgentDelivery fails",
		})
		assert.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		assert.Empty(t, disp.getCalls(), "DispatchAgentMessage must never run when MarkAgentDelivery errors")
	})

	t.Run("non-DM helpers fail closed with zero dispatches", func(t *testing.T) {
		err := srv.deliverArtifactReview(ctx, artifacts.ReviewNotice{
			ArtifactID:   tid("fail-closed-art"),
			Seq:          1,
			OwnerRef:     target.ID,
			ReviewerKind: artifacts.PrincipalKindUser,
		})
		require.ErrorIs(t, err, boom)

		err = srv.deliverMessage(ctx, &store.Message{
			ID:      tid("fail-closed-reconcile"),
			AgentID: target.ID,
			Msg:     "reconcile fail closed",
		})
		require.ErrorIs(t, err, boom)
		assert.Empty(t, disp.getCalls())
	})
}

type countingIdleCandidatesStore struct {
	store.Store
	listCalls atomic.Int32
}

func (w *countingIdleCandidatesStore) ListIdleSuspendCandidates(ctx context.Context, cutoff, heartbeatRecency, liveWorkCutoff, backoffCutoff time.Time, after *store.IdleSuspendCursor, limit int, excludeNeverSuspend bool) ([]store.Agent, error) {
	w.listCalls.Add(1)
	return w.Store.ListIdleSuspendCandidates(ctx, cutoff, heartbeatRecency, liveWorkCutoff, backoffCutoff, after, limit, excludeNeverSuspend)
}

type claimBeforeMarkStore struct {
	store.Store
	targetID    string
	fired       atomic.Bool
	claimBefore func()
}

func (w *claimBeforeMarkStore) MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error) {
	if agentID == w.targetID && w.fired.CompareAndSwap(false, true) && w.claimBefore != nil {
		w.claimBefore()
	}
	return w.Store.MarkAgentDelivery(ctx, agentID)
}

type countingAndFailingMarkDeliveryStore struct {
	store.Store
	fault     *storeFaultSwitch
	markCalls atomic.Int32
	err       error
}

func (w *countingAndFailingMarkDeliveryStore) MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error) {
	w.markCalls.Add(1)
	if w.fault.Active() {
		return nil, false, w.err
	}
	return w.Store.MarkAgentDelivery(ctx, agentID)
}

type firstStopBlockingDispatcher struct {
	wakeTrackingDispatcher
	stopCalls        atomic.Int32
	postStopping     bool
	store            store.Store
	t                *testing.T
	firstStopEntered chan struct{}
	releaseFirstStop chan struct{}
}

func (d *firstStopBlockingDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	if d.stopCalls.Add(1) == 1 {
		if d.postStopping {
			require.NoError(d.t, d.store.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:              string(state.PhaseStopping),
				Message:            "Agent shutting down",
				ReportSource:       store.StatusReportSourceAgent,
				ReportRunID:        a.RunID,
				ReportAgentToken:   true,
				GuardReincarnation: true,
			}))
		}
		close(d.firstStopEntered)
		select {
		case <-d.releaseFirstStop:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// TestIdleSuspend_UserLifecycleDuringClaimWins verifies R1: when a user stop,
// stop-all, or user suspend lands while an idle-suspend claim is in flight
// (both while the row is still running and after the container posted
// stopping), the user action invalidates the idle claim, the idle worker's
// commit is rejected, and subsequent messages do NOT auto-wake the agent.
func TestIdleSuspend_UserLifecycleDuringClaimWins(t *testing.T) {
	actions := []struct {
		name          string
		runUserAction func(t *testing.T, srv *Server, u *store.User, target *store.Agent)
		wantPhase     string
		wantReason    string
	}{
		{
			name: "user_stop",
			runUserAction: func(t *testing.T, srv *Server, u *store.User, target *store.Agent) {
				rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/stop", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			},
			wantPhase:  string(state.PhaseStopped),
			wantReason: "",
		},
		{
			name: "user_stop_all",
			runUserAction: func(t *testing.T, srv *Server, _ *store.User, target *store.Agent) {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+target.ProjectID+"/agents/stop-all", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			},
			wantPhase:  string(state.PhaseStopped),
			wantReason: "",
		},
		{
			name: "user_suspend",
			runUserAction: func(t *testing.T, srv *Server, u *store.User, target *store.Agent) {
				rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/suspend", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			},
			wantPhase:  string(state.PhaseSuspended),
			wantReason: string(state.SuspendReasonUser),
		},
	}

	for _, postStopping := range []bool{false, true} {
		stateLabel := "while_running"
		if postStopping {
			stateLabel = "after_container_stopping"
		}
		for _, tc := range actions {
			t.Run(tc.name+"/"+stateLabel, func(t *testing.T) {
				ctx := context.Background()
				srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
				disp := &firstStopBlockingDispatcher{
					postStopping:     postStopping,
					store:            s,
					t:                t,
					firstStopEntered: make(chan struct{}),
					releaseFirstStop: make(chan struct{}),
				}
				srv.SetDispatcher(disp)
				t.Cleanup(srv.waitAutoWakeIdle)
				enableIdleSweepForTest(t, srv, 15*time.Minute)

				_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
				require.NoError(t, err)
				require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
					Phase:    string(state.PhaseRunning),
					Activity: string(state.ActivityIdle),
				}))
				now := time.Now().UTC()
				db := s.(*entadapter.CompositeStore).DB()
				_, err = db.ExecContext(ctx,
					"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
					"run-r1-"+tc.name+"-"+stateLabel, now.Add(-20*time.Minute), now.Add(-15*time.Second), target.ID,
				)
				require.NoError(t, err)
				u := wakeLifecycleUser(t, s, "r1-"+tc.name+"-"+stateLabel, target.ProjectID,
					"agent.lifecycle", "agent.message", "agent.read", "project.agent.stop_all")

				sweepDone := make(chan struct{})
				var releaseOnce sync.Once
				releaseFirstStop := func() {
					releaseOnce.Do(func() { close(disp.releaseFirstStop) })
				}
				t.Cleanup(func() {
					releaseFirstStop()
					<-sweepDone
				})
				go func() {
					defer close(sweepDone)
					srv.agentIdleSuspendHandler()(ctx)
					srv.waitAutoSuspendIdle()
				}()

				<-disp.firstStopEntered
				midClaim, err := s.GetAgent(ctx, target.ID)
				require.NoError(t, err)
				require.True(t, midClaim.IdleSuspendClaimActive, "idle claim must be active when user action arrives")

				// User lifecycle action executes while the idle worker's stop is in flight.
				tc.runUserAction(t, srv, u, target)

				afterUser, err := s.GetAgent(ctx, target.ID)
				require.NoError(t, err)
				assert.Equal(t, tc.wantPhase, afterUser.Phase)
				assert.Equal(t, tc.wantReason, afterUser.SuspendReason)
				assert.False(t, afterUser.IdleSuspendClaimActive, "user lifecycle action must clear idle-suspend claim")
				assert.Nil(t, afterUser.IdleSuspendClaimUntil)

				// Release the idle worker's DispatchAgentStop; its commit must NOT overwrite the user's state.
				releaseFirstStop()
				<-sweepDone

				final, err := s.GetAgent(ctx, target.ID)
				require.NoError(t, err)
				assert.Equal(t, tc.wantPhase, final.Phase, "idle worker commit must not overwrite %s", tc.name)
				assert.Equal(t, tc.wantReason, final.SuspendReason)
				assert.Nil(t, final.IdleSuspendClaimUntil)

				// Subsequent DM without --wake must be refused, NEVER auto-woken.
				res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, final, "must not auto-wake"))
				assert.Nil(t, res)
				require.NotNil(t, dmErr)
				assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
				assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
				assert.Empty(t, disp.getStartCalls(), "agent stopped/suspended by user must never auto-wake")
				assert.Empty(t, disp.getMessageCalls())
			})
		}
	}
}

// TestIdleSuspend_GateOffSkipsMarkAgentDeliveryEntirely verifies R3 (AC 8):
// when either lifecycle.idle_suspend or server.hub.auto_suspend_idle is off,
// message delivery performs zero MarkAgentDelivery row-locked writes and cannot
// fail on an injected MarkAgentDelivery store error.
func TestIdleSuspend_GateOffSkipsMarkAgentDeliveryEntirely(t *testing.T) {
	modes := []struct {
		name      string
		expOn     bool
		settingOn bool
	}{
		{name: "experiment_off_setting_on", expOn: false, settingOn: true},
		{name: "both_off", expOn: false, settingOn: false},
	}

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			ctx := context.Background()
			boom := errors.New("MarkAgentDelivery must not be called when experiment is off")
			srv, rawStore, wrapper, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *countingAndFailingMarkDeliveryStore {
				return &countingAndFailingMarkDeliveryStore{Store: inner, fault: f, err: boom}
			})
			disp := &recordingDispatcher{}
			srv.SetDispatcher(disp)
			setIdleSuspendExperiment(t, srv, m.expOn)
			srv.mu.Lock()
			srv.config.AutoSuspendIdle = m.settingOn
			srv.config.IdleSuspendAfter = 15 * time.Minute
			srv.mu.Unlock()

			now := time.Now().UTC()
			pastGrace := now.Add(-20 * time.Minute)
			target := newIdleCandidateBrokerAgent(t, rawStore, "r3-target-"+m.name, string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
			sender := newIdleCandidateBrokerAgent(t, rawStore, "r3-sender-"+m.name, string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
			sender.ProjectID = target.ProjectID
			_, err := rawStore.(*entadapter.CompositeStore).DB().ExecContext(ctx,
				"UPDATE agents SET project_id = ? WHERE id = ?", target.ProjectID, sender.ID)
			require.NoError(t, err)

			fault.Arm()

			res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, target, "agent dm with gate off"))
			require.Nil(t, dmErr)
			require.NotNil(t, res)
			assert.Equal(t, AgentDMAccepted, res.Outcome)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+target.ID+"/message", map[string]any{
				"message": "user dm with gate off",
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			require.NoError(t, srv.deliverArtifactReview(ctx, artifacts.ReviewNotice{
				ArtifactID:   tid("r3-art-" + m.name),
				Seq:          1,
				OwnerRef:     target.ID,
				ReviewerKind: artifacts.PrincipalKindUser,
			}))
			require.NoError(t, srv.deliverMessage(ctx, &store.Message{
				ID:      tid("r3-rec-" + m.name),
				AgentID: target.ID,
				Msg:     "reconcile with gate off",
			}))

			assert.Zero(t, wrapper.markCalls.Load(), "MarkAgentDelivery must never be called when %s", m.name)
			assert.Len(t, disp.getCalls(), 4)
		})
	}

	t.Run("experiment_on_setting_off_marks_deliveries", func(t *testing.T) {
		ctx := context.Background()
		srv, rawStore, wrapper, _ := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *countingAndFailingMarkDeliveryStore {
			return &countingAndFailingMarkDeliveryStore{Store: inner, fault: f, err: errors.New("unused")}
		})
		disp := &recordingDispatcher{}
		srv.SetDispatcher(disp)
		setIdleSuspendExperiment(t, srv, true)
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = false
		srv.config.IdleSuspendAfter = 15 * time.Minute
		srv.mu.Unlock()

		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		target := newIdleCandidateBrokerAgent(t, rawStore, "r3-target-exp-on-set-off", string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
		sender := newIdleCandidateBrokerAgent(t, rawStore, "r3-sender-exp-on-set-off", string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
		sender.ProjectID = target.ProjectID
		_, err := rawStore.(*entadapter.CompositeStore).DB().ExecContext(ctx,
			"UPDATE agents SET project_id = ? WHERE id = ?", target.ProjectID, sender.ID)
		require.NoError(t, err)

		res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, target, "agent dm with experiment on setting off"))
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		assert.Equal(t, int32(1), wrapper.markCalls.Load(), "MarkAgentDelivery must stamp last_delivery_at whenever experiment is on")
		fresh, err := rawStore.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.NotNil(t, fresh.LastDeliveryAt)
	})
}

// TestIdleSuspend_SettleTimeoutErrorWording verifies O3: the default settle
// wait covers the full idle-suspend claim lease (bounded by the caller/wake
// budget), and an unsettled claim reports "is suspending" rather than "was
// already starting".
func TestIdleSuspend_SettleTimeoutErrorWording(t *testing.T) {
	assert.Equal(t, idleSuspendClaimLease(), idleSuspendSettleBudget())
	assert.GreaterOrEqual(t, idleSuspendSettleBudget(), stopWriteBudget()+10*time.Second)

	ctx := context.Background()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	target, cond := claimIdleForTest(t, s, target, "run-o3-settle")
	_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
	require.NoError(t, err)
	claimed, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.True(t, claimed.IdleSuspendClaimActive)

	// Even with the default 3m46s claim-lease settle budget, a tighter caller
	// request budget bounds waitForIdleSuspendToSettle cleanly.
	shortCtx, cancelShort := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancelShort()
	cp := *claimed
	err = srv.waitForIdleSuspendToSettle(shortCtx, &cp)
	require.ErrorIs(t, err, errIdleSuspendInProgressNotSettled)

	prevOverride := idleSuspendSettleTimeoutOverride
	idleSuspendSettleTimeoutOverride = 25 * time.Millisecond
	t.Cleanup(func() { idleSuspendSettleTimeoutOverride = prevOverride })

	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, claimed, "dm hitting settle timeout"))
	assert.Nil(t, res)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "is suspending and did not finish stopping in time")
	assert.NotContains(t, dmErr.Message, "was already starting")

	u := wakeLifecycleUser(t, s, "o3-user", target.ProjectID, "agent.message", "agent.read")
	rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+claimed.ID+"/message", map[string]any{
		"message": "user dm hitting settle timeout",
	})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "is suspending and did not finish stopping in time")
	assert.NotContains(t, rec.Body.String(), "was already starting")
	assert.Equal(t, "agent is suspending", srv.deferredReason(claimed))

	staleClaim := *claimed
	staleClaim.IdleSuspendClaimActive = false
	assert.Equal(t, "agent is already starting", srv.deferredReason(&staleClaim),
		"expired idle-suspend claim must not report 'agent is suspending'")

	srv.mu.Lock()
	srv.config.AutoSuspendIdle = false
	srv.mu.Unlock()
	assert.Equal(t, "agent is suspending", srv.deferredReason(claimed),
		"deferredReason must always honour an active IdleSuspendClaimActive regardless of auto_suspend_idle")
	assert.Empty(t, disp.getCalls())
}

type postStopHeartbeatDispatcher struct {
	runIntentDispatcher
	t      *testing.T
	store  store.Store
	onStop func(ctx context.Context, agent *store.Agent)
}

func (d *postStopHeartbeatDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.stops.Add(1)
	if d.onStop != nil {
		d.onStop(ctx, agent)
	}
	return nil
}

// TestIdleSuspend_CrossReplicaPostStopHeartbeatAndCrashReport reproduces pass-4
// R1: when a post-stop broker heartbeat on another replica (where in-process
// lifecycleOps is not held) promotes Exited (137) to phase=error, or a
// container/runtime shutdown report records non-zero stopped, IfIdleSuspendCommit
// still commits phase=suspended, suspend_reason=idle (with RetainedActivity)
// while the exact claim is held, whereas a concurrent user stop still clears
// the claim and wins.
func TestIdleSuspend_CrossReplicaPostStopHeartbeatAndCrashReport(t *testing.T) {
	t.Run("cross-replica Exited 137 broker heartbeat moving row to error commits suspended(idle)", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		var afterHeartbeat *store.Agent
		disp := &postStopHeartbeatDispatcher{
			t:     t,
			store: s,
			onStop: func(ctx context.Context, agent *store.Agent) {
				code := 137
				require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
					Phase:              string(state.PhaseError),
					ContainerStatus:    fmt.Sprintf("Exited (%d)", code),
					ExitCode:           &code,
					ExitReason:         string(state.ExitReasonCrashed),
					Heartbeat:          true,
					Message:            fmt.Sprintf("Agent crashed with exit code %d", code),
					ReportSource:       store.StatusReportSourceRuntime,
					ReportRunID:        agent.RunID,
					GuardReincarnation: true,
				}))
				var err error
				afterHeartbeat, err = s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
			},
		}
		srv.SetDispatcher(disp)

		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		target := newIdleCandidateBrokerAgent(t, s, "r1-other-replica-137", string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(ctx,
			"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			"run-rev4-137", pastGrace, now.Add(-15*time.Second), target.ID,
		)
		require.NoError(t, err)

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		require.NotNil(t, afterHeartbeat)
		assert.Equal(t, string(state.PhaseError), afterHeartbeat.Phase, "precondition: replica B heartbeat moved row to error")

		final, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), final.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
		assert.Equal(t, string(state.ActivityIdle), final.Activity)
		assert.True(t, srv.shouldAutoWake(final), "idle-suspended agent must remain auto-wakable after Exited 137 heartbeat")
		assert.Nil(t, final.IdleSuspendClaimUntil)
	})

	t.Run("container non-zero stopped report during shutdown commits suspended(idle)", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		disp := &postStopHeartbeatDispatcher{
			t:     t,
			store: s,
			onStop: func(ctx context.Context, agent *store.Agent) {
				code := 143
				require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
					Phase:              string(state.PhaseStopped),
					Activity:           string(state.ActivityCrashed),
					ContainerStatus:    "Exited (143)",
					ExitCode:           &code,
					ReportSource:       store.StatusReportSourceAgent,
					ReportRunID:        agent.RunID,
					ReportAgentToken:   true,
					GuardReincarnation: true,
				}))
			},
		}
		srv.SetDispatcher(disp)

		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		target := newIdleCandidateBrokerAgent(t, s, "r1-agent-143", string(state.ActivityCompleted), &pastGrace, now.Add(-15*time.Second))
		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(ctx,
			"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			"run-rev4-143", pastGrace, now.Add(-15*time.Second), target.ID,
		)
		require.NoError(t, err)

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		final, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), final.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), final.SuspendReason)
		assert.Equal(t, string(state.ActivityCompleted), final.Activity)
		assert.True(t, srv.shouldAutoWake(final))
	})

	t.Run("user stop landing alongside runtime heartbeat still clears claim and wins", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		disp := &postStopHeartbeatDispatcher{
			t:     t,
			store: s,
			onStop: func(ctx context.Context, agent *store.Agent) {
				code := 137
				require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
					Phase:              string(state.PhaseError),
					ContainerStatus:    "Exited (137)",
					ExitCode:           &code,
					ExitReason:         string(state.ExitReasonCrashed),
					Heartbeat:          true,
					ReportSource:       store.StatusReportSourceRuntime,
					ReportRunID:        agent.RunID,
					GuardReincarnation: true,
				}))
				// User stop arrives on another replica and records hub-authored stopped.
				_, _, err := s.SwapRunIntent(ctx, agent.ID, store.RunIntentStopped)
				require.NoError(t, err)
				require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
					Phase:           string(state.PhaseStopped),
					ContainerStatus: "stopped",
					IfRunID:         agent.RunID,
				}))
			},
		}
		srv.SetDispatcher(disp)

		now := time.Now().UTC()
		pastGrace := now.Add(-20 * time.Minute)
		target := newIdleCandidateBrokerAgent(t, s, "r1-user-stop-wins", string(state.ActivityIdle), &pastGrace, now.Add(-15*time.Second))
		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(ctx,
			"UPDATE agents SET run_id = ?, activity_since = ?, last_seen = ? WHERE id = ?",
			"run-rev4-user-stop", pastGrace, now.Add(-15*time.Second), target.ID,
		)
		require.NoError(t, err)

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		final, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), final.Phase)
		assert.Empty(t, final.SuspendReason)
		assert.False(t, srv.shouldAutoWake(final))
	})
}

// TestIdleSuspend_QueueReserveAcceptsStalledHandoffAndAdvancesPastNonResumable
// covers pass-4 O2:
//  1. The idle sweep caps its queue usage at half of autoSuspendQueueLimit so a
//     stalled hand-off arriving while an idle batch is queued/running is still
//     accepted and suspended.
//  2. Non-resumable agents sitting at the head of ORDER BY activity_since ASC
//     are skipped via keyset pagination rather than starving resumable agents
//     behind them.
func TestIdleSuspend_QueueReserveAcceptsStalledHandoffAndAdvancesPastNonResumable(t *testing.T) {
	t.Run("stalled hand-off accepted while idle batch fills its queue budget", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		prevLimit := autoSuspendQueueLimit
		autoSuspendQueueLimit = 4
		t.Cleanup(func() { autoSuspendQueueLimit = prevLimit })

		firstStopEntered := make(chan struct{})
		releaseFirstStop := make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirstStop) }) })

		disp := &postStopHeartbeatDispatcher{
			t:     t,
			store: s,
			onStop: func(_ context.Context, _ *store.Agent) {
				entered := false
				enteredOnce.Do(func() {
					entered = true
					close(firstStopEntered)
				})
				if entered {
					<-releaseFirstStop
				}
			},
		}
		srv.SetDispatcher(disp)

		now := time.Now().UTC()
		var idleIDs []string
		for i := 0; i < 4; i++ {
			since := now.Add(-time.Duration(30-i) * time.Minute)
			a := newIdleCandidateBrokerAgent(t, s, fmt.Sprintf("o2-idle-%d", i), string(state.ActivityIdle), &since, now.Add(-15*time.Second))
			idleIDs = append(idleIDs, a.ID)
		}

		stalledSince := now.Add(-25 * time.Minute)
		stalledAgent := newIdleCandidateBrokerAgent(t, s, "o2-stalled", string(state.ActivityStalled), &stalledSince, now.Add(-15*time.Second))

		// Run the idle sweep: with autoSuspendQueueLimit=4, at most 2 idle agents
		// may enter the queue, leaving 2 reserved slots for stalled hand-offs.
		srv.agentIdleSuspendHandler()(ctx)
		<-firstStopEntered

		srv.autoSuspend.mu.Lock()
		queuedIdle := len(srv.autoSuspend.queued)
		srv.autoSuspend.mu.Unlock()
		assert.Equal(t, 2, queuedIdle, "idle sweep must leave half the queue reserved for stalled hand-offs")

		// Stalled hand-off arrives while the idle batch is still running.
		freshStalled, err := s.GetAgent(ctx, stalledAgent.ID)
		require.NoError(t, err)
		srv.enqueueAutoSuspend([]store.Agent{*freshStalled})

		srv.autoSuspend.mu.Lock()
		stalledQueued := srv.autoSuspend.queued[stalledAgent.ID]
		srv.autoSuspend.mu.Unlock()
		assert.True(t, stalledQueued, "stalled hand-off must be accepted while idle batch holds its half of the queue")

		releaseOnce.Do(func() { close(releaseFirstStop) })
		srv.waitAutoSuspendIdle()

		gotStalled, err := s.GetAgent(ctx, stalledAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), gotStalled.Phase)
		assert.Equal(t, string(state.SuspendReasonStalled), gotStalled.SuspendReason)

		for _, id := range idleIDs[:2] {
			gotIdle, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), gotIdle.Phase)
			assert.Equal(t, string(state.SuspendReasonIdle), gotIdle.SuspendReason)
		}
	})

	t.Run("non-resumable harnesses at head of ORDER BY do not starve resumable agents", func(t *testing.T) {
		ctx := context.Background()
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)
		enableIdleSweepForTest(t, srv, 15*time.Minute)

		prevLimit := autoSuspendQueueLimit
		autoSuspendQueueLimit = 4 // idle budget = 2
		t.Cleanup(func() { autoSuspendQueueLimit = prevLimit })

		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID: tid("hc-o2-generic"), Name: "o2-shell", Slug: "o2-shell", Harness: "generic",
			Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{Harness: "generic"},
		}))
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID: tid("hc-o2-claude"), Name: "o2-claude", Slug: "o2-claude", Harness: "claude",
			Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{Harness: "claude"},
		}))

		now := time.Now().UTC()
		var nonResumableIDs []string
		for i := 0; i < 5; i++ {
			since := now.Add(-time.Duration(60-i) * time.Minute)
			a := newIdleCandidateBrokerAgent(t, s, fmt.Sprintf("o2-generic-%d", i), string(state.ActivityIdle), &since, now.Add(-15*time.Second))
			a.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "o2-shell"}
			require.NoError(t, s.UpdateAgent(ctx, a))
			// Restore activity_since after UpdateAgent.
			_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx, "UPDATE agents SET activity_since = ? WHERE id = ?", since, a.ID)
			require.NoError(t, err)
			nonResumableIDs = append(nonResumableIDs, a.ID)
		}

		var resumableIDs []string
		for i := 0; i < 2; i++ {
			since := now.Add(-time.Duration(25-i) * time.Minute)
			a := newIdleCandidateBrokerAgent(t, s, fmt.Sprintf("o2-claude-%d", i), string(state.ActivityIdle), &since, now.Add(-15*time.Second))
			a.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "o2-claude"}
			require.NoError(t, s.UpdateAgent(ctx, a))
			_, err := s.(*entadapter.CompositeStore).DB().ExecContext(ctx, "UPDATE agents SET activity_since = ? WHERE id = ?", since, a.ID)
			require.NoError(t, err)
			resumableIDs = append(resumableIDs, a.ID)
		}

		srv.agentIdleSuspendHandler()(ctx)
		srv.waitAutoSuspendIdle()

		for _, id := range nonResumableIDs {
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
		}
		for _, id := range resumableIDs {
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase, "resumable agent behind non-resumable head must be suspended")
			assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		}
	})
}
