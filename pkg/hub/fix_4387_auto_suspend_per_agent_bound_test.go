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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setStalledDetectionTimeout sets stalledDetectionTimeout for one test.
func setStalledDetectionTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := stalledDetectionTimeout
	stalledDetectionTimeout = d
	t.Cleanup(func() { stalledDetectionTimeout = prev })
}

// setAutoSuspendStartWindow sets autoSuspendStartWindow for one test.
func setAutoSuspendStartWindow(t *testing.T, d time.Duration) {
	t.Helper()
	prev := autoSuspendStartWindow
	autoSuspendStartWindow = func() time.Duration { return d }
	t.Cleanup(func() { autoSuspendStartWindow = prev })
}

// makeStalled backdates agent's last activity, with a recent heartbeat, so
// the next stalled-detection tick marks it stalled.
func makeStalled(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	db := s.(*entadapter.CompositeStore).DB()
	_, err := db.ExecContext(context.Background(),
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		time.Now().Add(-time.Hour), time.Now().Add(-10*time.Second), agentID)
	require.NoError(t, err)
}

// markAgentStalled sets agent's activity to stalled, as the stalled-detection
// tick does, so the auto-suspend's re-read finds it still stalled.
func markAgentStalled(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	db := s.(*entadapter.CompositeStore).DB()
	_, err := db.ExecContext(context.Background(),
		"UPDATE agents SET activity = ? WHERE id = ?", string(state.ActivityStalled), agentID)
	require.NoError(t, err)
}

// ptone/scion#4387 item 2: the stalled-detection tick hands the agents to
// the auto-suspend worker and returns, and each agent's auto-suspend has
// its own bound, detached from the tick and from the agents before it. The
// first agent's sync-back download runs until its own bound cuts it, well
// past the tick's bound; the second agent in the batch is still suspended,
// with a sync-back bound of its own.
func TestAutoSuspend_SlowSyncBackDoesNotCutNextAgent(t *testing.T) {
	const (
		tickBound = 250 * time.Millisecond
		syncBound = time.Second
	)
	t.Setenv("HOME", t.TempDir())
	disp := &slowLaunchDispatcher{delay: 10 * time.Millisecond}
	srv, s, project := setupCreateAgentServer(t, disp) // hub-managed: no GitRemote.
	shortenSyncDispatchTimeout(t, syncBound)
	setStalledDetectionTimeout(t, tickBound)
	require.Equal(t, syncBound, stopSyncBackTimeout(), "fixture check: the sync-back bound follows syncDispatchTimeout")
	srv.config.AutoSuspendStalled = true

	srv.SetStorage(newContentMockStorage("test-bucket"))
	var (
		mu           sync.Mutex
		downloadErrs []error
		remaining    []time.Duration
	)
	srv.setHubWorkspaceDownloader(func(ctx context.Context, _, _, _ string) error {
		mu.Lock()
		first := len(downloadErrs) == 0
		if deadline, ok := ctx.Deadline(); ok {
			remaining = append(remaining, time.Until(deadline))
		} else {
			remaining = append(remaining, 0)
		}
		downloadErrs = append(downloadErrs, nil)
		i := len(downloadErrs) - 1
		mu.Unlock()
		var err error
		if first {
			// A download far larger than the bound: it runs until its
			// ctx is cut.
			err = errDownloadNeverCut
			if awaitCanceled(ctx) {
				err = ctx.Err()
			}
		}
		mu.Lock()
		downloadErrs[i] = err
		mu.Unlock()
		return err
	})

	a := createSiteAgent(t, s, project, "as-bound-a", state.PhaseRunning, store.RunIntentRunning)
	b := createSiteAgent(t, s, project, "as-bound-b", state.PhaseRunning, store.RunIntentRunning)
	makeStalled(t, s, a.ID)
	makeStalled(t, s, b.ID)
	broker := connectFakeBroker(t, srv, a.RuntimeBrokerID)
	uploaded := answerBrokerUploads(t, broker, 0, 2)

	start := time.Now()
	srv.agentStalledDetectionHandler()(context.Background())
	tickTook := time.Since(start)
	srv.waitAutoSuspendIdle()
	elapsed := time.Since(start)

	require.Len(t, uploaded, 2, "fixture check: both sync-backs were tunneled to the broker")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, downloadErrs, 2, "fixture check: both sync-backs downloaded")
	require.ErrorIs(t, downloadErrs[0], context.DeadlineExceeded, "the first download is cut at its own bound")
	assert.NoError(t, downloadErrs[1], "the second agent's sync-back is not cut")
	// Under one deadline shared by the batch, the second sync-back would
	// have had less than the tick's bound left, or nothing at all.
	assert.Greater(t, remaining[1], tickBound, "the second sync-back has a bound of its own")
	assert.Less(t, tickTook, syncBound, "the tick returns without waiting for the auto-suspends")
	assert.Greater(t, elapsed, tickBound, "fixture check: the batch outlasted the tick's bound")

	for _, id := range []string{a.ID, b.ID} {
		got := requireRunIntent(t, s, id, store.RunIntentStopped)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase, "agent %s is suspended", id)
	}
}

// No new auto-suspend starts once the batch's start window has passed: the
// agents not reached are left running, with their intent untouched.
func TestAutoSuspend_StartWindowBoundsBatch(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "as-window")
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	markAgentStalled(t, s, agent.ID)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	setAutoSuspendStartWindow(t, 0)
	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	assert.Zero(t, disp.stops.Load(), "no auto-suspend starts after the window")
	got := requireRunIntent(t, s, agent.ID, store.RunIntentRunning)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
}

// A done ctx (server shutdown) starts no new auto-suspend.
func TestAutoSuspend_CancelledSchedulerStartsNone(t *testing.T) {
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, "as-cancel")
	markAgentStalled(t, s, agent.ID)
	loaded, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	assert.Zero(t, disp.stops.Load(), "no auto-suspend starts after shutdown")
}

// gatedStopDispatcher records each stop dispatch and holds it until it is
// released: one stop per send on release, or every stop once release is
// closed (or when its ctx ends). With failFirst the first stop fails once
// released; with panicFirst the first stop panics without waiting.
type gatedStopDispatcher struct {
	createAgentDispatcher
	mu         sync.Mutex
	stops      []string
	started    chan string
	release    chan struct{}
	failFirst  bool
	panicFirst bool
}

func newGatedStopDispatcher() *gatedStopDispatcher {
	return &gatedStopDispatcher{started: make(chan string, 16), release: make(chan struct{})}
}

// errGatedStopFailed is the first stop's error with failFirst.
var errGatedStopFailed = errors.New("gated stop failed")

func (d *gatedStopDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.mu.Lock()
	d.stops = append(d.stops, agent.ID)
	first := len(d.stops) == 1
	d.mu.Unlock()
	d.started <- agent.ID
	if first && d.panicFirst {
		panic("gated stop panicked")
	}
	select {
	case <-d.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if first && d.failFirst {
		return errGatedStopFailed
	}
	return nil
}

func (d *gatedStopDispatcher) stopped() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.stops...)
}

// awaitStopStarted waits for the next stop dispatch to begin.
func awaitStopStarted(t *testing.T, d *gatedStopDispatcher) string {
	t.Helper()
	select {
	case id := <-d.started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no stop dispatch started")
		return ""
	}
}

// releaseOneStop lets one held stop dispatch finish.
func releaseOneStop(t *testing.T, d *gatedStopDispatcher) {
	t.Helper()
	select {
	case d.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("no held stop dispatch to release")
	}
}

// newStalledBrokerAgent creates a running agent on an online broker with
// intent running, marked stalled, and returns it as loaded.
func newStalledBrokerAgent(t *testing.T, s store.Store, name string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	_, _, agent := setupOnlineBrokerAgent(t, s, name)
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	markAgentStalled(t, s, agent.ID)
	loaded, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	return loaded
}

// newHandOffAgent creates a running agent on an online broker with intent
// running that the next stalled-detection tick marks stalled.
func newHandOffAgent(t *testing.T, s store.Store, name string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	_, _, agent := setupOnlineBrokerAgent(t, s, name)
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentRunning)
	require.NoError(t, err)
	db := s.(*entadapter.CompositeStore).DB()
	_, err = db.ExecContext(ctx, "UPDATE agents SET activity = '' WHERE id = ?", agent.ID)
	require.NoError(t, err)
	makeStalled(t, s, agent.ID)
	return agent
}

// ptone/scion#4387 (R1, O-1): the auto-suspends run on the server-owned
// worker, on the server-lifetime ctx, not on the scheduler's handler ctx.
// The tick hands four agents off and returns; its handler ctx then passes
// its deadline and its parent is cancelled. The worker goes on: the second
// and third agents' stops start while the server is up. The server then
// shuts down while the third is in flight, and the fourth never starts.
// The third completes under its own bound, and shutdown waits for it.
func TestAutoSuspend_WorkerStopsOnShutdownAfterHandlerCtxEnds(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	srv.SetDispatcher(disp)
	srv.config.AutoSuspendStalled = true
	all := map[string]bool{}
	for _, name := range []string{"as-shutdown-1", "as-shutdown-2", "as-shutdown-3", "as-shutdown-4"} {
		all[newHandOffAgent(t, s, name).ID] = true
	}

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	handlerCtx, cancelHandler := context.WithTimeout(parent, time.Second)
	defer cancelHandler()
	srv.agentStalledDetectionHandler()(handlerCtx)

	first := awaitStopStarted(t, disp)
	<-handlerCtx.Done()
	require.ErrorIs(t, handlerCtx.Err(), context.DeadlineExceeded, "fixture check: the handler ctx passed its deadline")
	cancelParent()

	releaseOneStop(t, disp)
	second := awaitStopStarted(t, disp) // the server is up: the worker goes on.
	releaseOneStop(t, disp)
	third := awaitStopStarted(t, disp)

	cleaned := make(chan struct{})
	go func() {
		_ = srv.CleanupBackgroundResources(context.Background())
		close(cleaned)
	}()
	// Shutdown has begun while the third is in flight.
	select {
	case <-srv.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the server ctx")
	}
	releaseOneStop(t, disp)
	select {
	case <-cleaned:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not return")
	}

	assert.Equal(t, []string{first, second, third}, disp.stopped(), "no auto-suspend starts after shutdown")
	for _, id := range []string{first, second, third} {
		delete(all, id)
		got := requireRunIntent(t, s, id, store.RunIntentStopped)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase, "agent %s is suspended", id)
	}
	require.Len(t, all, 1)
	for fourth := range all {
		got := requireRunIntent(t, s, fourth, store.RunIntentRunning)
		assert.Equal(t, string(state.PhaseRunning), got.Phase, "the agent not reached is left running")
		// A hand-off after shutdown is dropped.
		srv.enqueueAutoSuspend([]store.Agent{*got})
	}
	srv.waitAutoSuspendIdle()
	assert.Len(t, disp.stopped(), 3)
}

// Overlapping hand-offs (ticks) do not process an agent twice: an agent
// already waiting, or in the batch being processed, is not added again. A
// batch handed off while the worker is busy is processed after it. The
// first agent's stop fails, so it stays running and stalled: a duplicate
// entry would dispatch it a second time.
func TestAutoSuspend_WorkerDeduplicatesHandOffs(t *testing.T) {
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	disp.failFirst = true
	srv.SetDispatcher(disp)
	a := newStalledBrokerAgent(t, s, "as-dedup-a")
	b := newStalledBrokerAgent(t, s, "as-dedup-b")

	srv.enqueueAutoSuspend([]store.Agent{*a})
	require.Equal(t, a.ID, awaitStopStarted(t, disp))
	// While a is in flight: a again (in progress), b twice (waiting).
	srv.enqueueAutoSuspend([]store.Agent{*a, *b})
	srv.enqueueAutoSuspend([]store.Agent{*b})
	pending, queued := autoSuspendQueueState(srv)
	assert.Equal(t, []string{b.ID}, pending, "only b waits, once")
	assert.Equal(t, 2, queued, "a (in progress) and b (waiting)")
	close(disp.release)
	srv.waitAutoSuspendIdle()

	assert.Equal(t, []string{a.ID, b.ID}, disp.stopped(), "each agent is dispatched once, in hand-off order")
	gotA := requireRunIntent(t, s, a.ID, store.RunIntentRunning)
	assert.Equal(t, string(state.PhaseRunning), gotA.Phase, "fixture check: a's failed stop left it running")
	assert.Equal(t, string(state.ActivityStalled), gotA.Activity, "fixture check: and stalled")
	gotB := requireRunIntent(t, s, b.ID, store.RunIntentStopped)
	assert.Equal(t, string(state.PhaseSuspended), gotB.Phase)
}

// The hand-off is bounded: agents beyond autoSuspendQueueLimit are dropped
// and stay stalled.
func TestAutoSuspend_WorkerQueueBounded(t *testing.T) {
	prev := autoSuspendQueueLimit
	autoSuspendQueueLimit = 1
	t.Cleanup(func() { autoSuspendQueueLimit = prev })
	srv, s := testServer(t)
	disp := newGatedStopDispatcher()
	close(disp.release)
	srv.SetDispatcher(disp)
	a := newStalledBrokerAgent(t, s, "as-bound-q-a")
	b := newStalledBrokerAgent(t, s, "as-bound-q-b")

	srv.enqueueAutoSuspend([]store.Agent{*a, *b})
	srv.waitAutoSuspendIdle()

	assert.Equal(t, []string{a.ID}, disp.stopped(), "only the agents within the limit are processed")
	got := requireRunIntent(t, s, b.ID, store.RunIntentRunning)
	assert.Equal(t, string(state.ActivityStalled), got.Activity, "a dropped agent stays stalled")
}

// ptone/scion#4387 round 1 (R2): the auto-suspend reads the agent again
// and skips it unless it is still running and stalled in the same run, so
// an agent that recovered after it was marked stalled is not suspended.
func TestAutoSuspend_SkipsAgentThatRecovered(t *testing.T) {
	for name, change := range map[string]string{
		"activity recovered": "UPDATE agents SET activity = 'working' WHERE id = ?",
		"phase changed":      "UPDATE agents SET phase = 'stopped' WHERE id = ?",
		"new run":            "UPDATE agents SET run_id = 'run-newer' WHERE id = ?",
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &runIntentDispatcher{}
			srv.SetDispatcher(disp)
			snapshot := newStalledBrokerAgent(t, s, "as-recovered")
			db := s.(*entadapter.CompositeStore).DB()
			_, err := db.ExecContext(context.Background(), change, snapshot.ID)
			require.NoError(t, err)

			srv.autoSuspendStalledAgents(context.Background(), []store.Agent{*snapshot})

			assert.Zero(t, disp.stops.Load(), "no stop is dispatched")
			got := requireRunIntent(t, s, snapshot.ID, store.RunIntentRunning)
			assert.NotEqual(t, string(state.PhaseSuspended), got.Phase)
		})
	}

	// Control: unchanged, the same agent is suspended.
	srv, s := testServer(t)
	disp := &runIntentDispatcher{}
	srv.SetDispatcher(disp)
	snapshot := newStalledBrokerAgent(t, s, "as-recovered-control")
	srv.autoSuspendStalledAgents(context.Background(), []store.Agent{*snapshot})
	assert.Equal(t, int32(1), disp.stops.Load())
}
