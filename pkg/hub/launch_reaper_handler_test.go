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

// This file covers the Hub-side wiring of the async-create launch reaper
// (design §3.7): the tick handler's metrics, event-publish and
// disarmed-warning behavior. The reaper's own arming,
// selection and per-row reap logic is covered by
// pkg/store/entadapter/launch_reaper_test.go; it is not re-verified here.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/reapermetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reaperResultStubStore wraps a real store.Store, overriding only
// RunLaunchReaperTick so a test can hand the Hub-side handler a canned
// result (e.g., a large DisarmedFor) without needing real wall-clock time or
// white-box access to pkg/store/entadapter's unexported test helpers.
type reaperResultStubStore struct {
	store.Store
	result store.ReaperTickResult
	err    error
}

func (s *reaperResultStubStore) RunLaunchReaperTick(_ context.Context, _ store.ReaperParams) (store.ReaperTickResult, error) {
	return s.result, s.err
}

// TestLaunchReaperTickHandler_ReapsDeadlineAndPublishes drives the handler
// against a real (sqlite) store with one agent whose launch deadline has
// already passed, and verifies it publishes AgentStatus for the reaped agent
// (design §3.7 step 5: "Publishes for reaped rows happen after commit").
func TestLaunchReaperTickHandler_ReapsDeadlineAndPublishes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("reaper-h-project"), Slug: "reaper-h-project", Name: "Reaper H Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("reaper-h-agent"), Slug: "reaper-h-agent", Name: "Reaper H Agent", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: "broker-1", StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// A 1ms budget plus a short sleep puts the deadline in the past without
	// needing white-box access to backdate launch_deadline directly.
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	events := NewChannelEventPublisher()
	defer events.Close()
	srv.events = events
	statusCh, unsub := events.Subscribe("agent." + agent.ID + ".status")
	defer unsub()

	srv.launchReaperTickHandler()(ctx)

	select {
	case evt := <-statusCh:
		var data AgentStatusEvent
		require.NoError(t, json.Unmarshal(evt.Data, &data))
		assert.Equal(t, "error", data.Phase)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for the reaper tick to publish a status event for the reaped agent")
	}

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, launchID, after.LaunchID)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
	assert.Equal(t, store.LaunchErrorLaunchTimeout, after.LaunchError)
}

// TestLaunchReaperTickHandler_NoOpWhenNothingDue verifies a tick with no
// active launches at all — the state when hub.asyncAgentLaunch is off, since
// then no agent ever has an active launch — completes without publishing
// anything and without error. See registerLaunchReaper's doc comment for the
// exact per-tick statement list and cost with the feature off.
func TestLaunchReaperTickHandler_NoOpWhenNothingDue(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("reaper-noop-project"), Slug: "reaper-noop-project", Name: "Reaper NoOp Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("reaper-noop-agent"), Slug: "reaper-noop-agent", Name: "Reaper NoOp Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	events := NewChannelEventPublisher()
	defer events.Close()
	srv.events = events

	// Subscribe to every agent subject ("agent.>", not one specific agent
	// ID): there is an agent (with no launch), so a wrongly-published event
	// for it would actually be caught here, unlike subscribing to a subject
	// no agent could ever use.
	allCh, unsub := events.Subscribe("agent.>")
	defer unsub()

	srv.launchReaperTickHandler()(ctx)

	select {
	case evt := <-allCh:
		t.Fatalf("expected no publish with nothing due, got %+v", evt)
	case <-time.After(50 * time.Millisecond):
		// Expected: no publish.
	}
}

// TestLaunchReaperTickHandler_RecordsMetrics verifies the tick-outcome
// counter and disarmed-time gauge are recorded through a real
// OTel provider when SetReaperMetrics has wired one.
func TestLaunchReaperTickHandler_RecordsMetrics(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	rec, err := reapermetrics.New(mp)
	require.NoError(t, err)
	srv.SetReaperMetrics(rec)

	srv.launchReaperTickHandler()(ctx)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	assert.True(t, names[reapermetrics.MetricLaunchReaperTicks], "expected the tick-outcome counter to be recorded")
	assert.True(t, names[reapermetrics.MetricLaunchReaperDisarmedFor], "expected the disarmed-time gauge to be recorded on a completed tick")
}

// TestLaunchReaperTickHandler_RowErrorsRecordedWhenNonZero verifies
// IncRowErrors is called when the tick reports row errors, via a stub store
// returning a canned result — a poison row is a store-level concern
// (pkg/store/entadapter's own tests cover producing one for real); this only
// checks the Hub-side metrics wiring reacts to the field.
func TestLaunchReaperTickHandler_RowErrorsRecordedWhenNonZero(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.store = &reaperResultStubStore{
		Store: s,
		result: store.ReaperTickResult{
			Outcome:   store.ReaperTickCompleted,
			RowErrors: 3,
		},
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := reapermetrics.New(mp)
	require.NoError(t, err)
	srv.SetReaperMetrics(rec)

	srv.launchReaperTickHandler()(ctx)

	dps := rowErrorsDataPoints(t, ctx, reader)
	require.Len(t, dps, 1)
	assert.Equal(t, int64(3), dps[0])
}

// TestLaunchReaperTickHandler_RowErrorsMetricAbsentWhenZero is the zero-case
// companion to TestLaunchReaperTickHandler_RowErrorsRecordedWhenNonZero: a
// completed tick with RowErrors == 0 must not record a data point on the
// row-errors counter at all (the handler only calls IncRowErrors when
// RowErrors > 0). Note this is not "the sum is 0": an actual
// IncRowErrors(ctx, 0) call would still export a Sum data point with value
// 0, so the assertion below checks for the data point's absence, not its
// value.
func TestLaunchReaperTickHandler_RowErrorsMetricAbsentWhenZero(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.store = &reaperResultStubStore{
		Store: s,
		result: store.ReaperTickResult{
			Outcome:   store.ReaperTickCompleted,
			RowErrors: 0,
		},
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := reapermetrics.New(mp)
	require.NoError(t, err)
	srv.SetReaperMetrics(rec)

	srv.launchReaperTickHandler()(ctx)

	assert.Empty(t, rowErrorsDataPoints(t, ctx, reader), "expected no scion.launch_reaper.row_errors data point when RowErrors == 0")
}

// rowErrorsDataPoints collects every data point currently exported on the
// scion.launch_reaper.row_errors counter. An empty slice means the metric
// was never recorded at all (IncRowErrors was never called); this is
// distinct from a recorded value of 0, which would appear as a single data
// point with Value 0.
func rowErrorsDataPoints(t *testing.T, ctx context.Context, reader *sdkmetric.ManualReader) []int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	var values []int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != reapermetrics.MetricLaunchReaperRowErrors {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					values = append(values, dp.Value)
				}
			}
		}
	}
	return values
}

// TestLaunchReaperTickHandler_WarnsWhenDisarmedOverTenMinutes verifies the
// throttled "disarmed for more than 10 minutes" warning (design §3.7) fires
// on a completed tick reporting DisarmedFor > 10m, and does not fire again
// on an immediately following tick (throttling), using a stub store so the
// test does not need to wait 10 real minutes.
func TestLaunchReaperTickHandler_WarnsWhenDisarmedOverTenMinutes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srv.store = &reaperResultStubStore{
		Store: s,
		result: store.ReaperTickResult{
			Outcome:     store.ReaperTickCompleted,
			DisarmedFor: 11 * time.Minute,
		},
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	handler := srv.launchReaperTickHandler()

	handler(ctx)
	first := buf.String()
	assert.Contains(t, first, "disarmed for more than 10 minutes")

	buf.Reset()
	handler(ctx)
	second := buf.String()
	assert.Empty(t, second, "expected the warning to be throttled on an immediately-following tick")
}

// TestLaunchReaperTickHandler_DoesNotWarnWhenArmed verifies a completed tick
// with DisarmedFor at or under launchReaperDisarmedWarnAfter (the 10-minute
// threshold) never logs the disarmed warning: the check is a strict `>`, so
// exactly at the threshold must not warn either.
func TestLaunchReaperTickHandler_DoesNotWarnWhenArmed(t *testing.T) {
	for _, disarmedFor := range []time.Duration{0, launchReaperDisarmedWarnAfter} {
		t.Run(disarmedFor.String(), func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			srv.store = &reaperResultStubStore{
				Store: s,
				result: store.ReaperTickResult{
					Outcome:     store.ReaperTickCompleted,
					DisarmedFor: disarmedFor,
				},
			}

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			srv.launchReaperTickHandler()(ctx)

			assert.Empty(t, buf.String())
		})
	}
}
