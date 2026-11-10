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

// Design wave 3 phase D3b (ptone/scion#4669), store-free: the all-live-hubs
// check, the agent ID on the start URL and the lifecycle start body.
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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubInstanceListStore serves ListHubInstances from rows (or err), counts
// the reads, blocks each read until release is closed when set, and panics
// on the next read when panicNext is set.
type hubInstanceListStore struct {
	store.Store
	mu      sync.Mutex
	rows    []store.HubInstance
	now     time.Time
	err     error
	reads   atomic.Int32
	release chan struct{}
	// panicNext makes the next read panic (once).
	panicNext atomic.Bool
}

func (s *hubInstanceListStore) ListHubInstances(_ context.Context, _ time.Duration) ([]store.HubInstance, time.Time, error) {
	s.reads.Add(1)
	if s.panicNext.CompareAndSwap(true, false) {
		panic("registry read exploded")
	}
	s.mu.Lock()
	release := s.release
	s.mu.Unlock()
	if release != nil {
		<-release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.HubInstance(nil), s.rows...), s.now, s.err
}

func (s *hubInstanceListStore) set(rows []store.HubInstance, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows, s.err = rows, err
}

// hubRow is a registry row last written age before now, with features.
func hubRow(t *testing.T, label string, now time.Time, age time.Duration, stopped bool, features ...string) store.HubInstance {
	t.Helper()
	stats, err := json.Marshal(api.HubInstanceStats{Features: features})
	require.NoError(t, err)
	row := store.HubInstance{ID: "id-" + label, Label: label, Version: "v-" + label, LastSeen: now.Add(-age), Stats: stats}
	if stopped {
		at := now.Add(-age)
		row.StoppedAt = &at
	}
	return row
}

// countingHandler counts Warn records whose message contains substr.
type countingHandler struct {
	mu     sync.Mutex
	warns  []string
	infos  []string
	substr string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !strings.Contains(r.Message, h.substr) {
		return nil
	}
	if r.Level >= slog.LevelWarn {
		h.warns = append(h.warns, r.Message)
	} else {
		h.infos = append(h.infos, r.Message)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.warns), len(h.infos)
}

// TestAsyncStartGate_AllLiveHubsHave pins design D-T13: every live row with
// async_start_v1 → true; one live row without it → false, logged once
// while the answer stays the same; a stale or stopped row without it is
// ignored; a row whose stats cannot be read, a list error and an empty
// registry → false.
func TestAsyncStartGate_AllLiveHubsHave(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := api.HubInstanceFeatureAsyncStartV1
	cases := []struct {
		name string
		rows []store.HubInstance
		err  error
		want bool
		why  string
	}{
		{"all live have it", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "b", now, 10*time.Second, false, "other", f)}, nil, true, ""},
		{"one live lacks it", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "old", now, 2*time.Second, false)}, nil, false, "hub instance old (version v-old) lacks async_start_v1"},
		{"stale row ignored", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "gone", now, hubInstanceStaleAfter+time.Second, false)}, nil, true, ""},
		{"stopped row ignored", []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "down", now, time.Second, true)}, nil, true, ""},
		{"unreadable stats", []store.HubInstance{{ID: "x", Label: "bad", Version: "v", LastSeen: now, Stats: json.RawMessage(`{"features":`)}}, nil, false, "hub instance bad (version v) has unreadable stats"},
		{"list error", nil, errors.New("db down"), false, "hub instance registry read failed: db down"},
		{"no live row", []store.HubInstance{hubRow(t, "gone", now, hubInstanceStaleAfter+time.Second, false, f)}, nil, false, "no live hub instance in the registry yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &hubInstanceListStore{rows: c.rows, now: now, err: c.err}
			srv := &Server{store: st}
			got, why := srv.computeAsyncStartGate(context.Background())
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.why, why)
		})
	}

	// Logged once per change of the answer.
	h := &countingHandler{substr: "async start"}
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "old", now, time.Second, false)}}
	srv := &Server{store: st, agentLifecycleLog: slog.New(h)}
	srv.refreshAsyncStartGate(context.Background())
	srv.refreshAsyncStartGate(context.Background())
	warns, infos := h.counts()
	assert.Equal(t, 1, warns, "the same false answer is logged once")
	assert.Equal(t, 0, infos)
	st.set([]store.HubInstance{hubRow(t, "old", now, time.Second, false, f)}, nil)
	srv.refreshAsyncStartGate(context.Background())
	srv.refreshAsyncStartGate(context.Background())
	warns, infos = h.counts()
	assert.Equal(t, 1, warns)
	assert.Equal(t, 1, infos, "turning on is logged once")
}

// TestAsyncStartGate_CachedAndRefreshedInBackground pins the cache: the
// gate reads false until its first refresh, a start never waits for a
// refresh (the read here blocks until released), the answer is reused for
// asyncStartGateTTL and refreshed after it, and a list error turns it
// false.
func TestAsyncStartGate_CachedAndRefreshedInBackground(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	var clockMu sync.Mutex
	prev := asyncStartGateNow
	asyncStartGateNow = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	t.Cleanup(func() { asyncStartGateNow = prev })
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}, release: make(chan struct{})}
	srv := &Server{store: st}

	done := make(chan bool)
	go func() { done <- srv.allLiveHubsHaveAsyncStart() }()
	select {
	case got := <-done:
		assert.False(t, got, "false before the first refresh")
	case <-time.After(5 * time.Second):
		t.Fatal("a start waited for the registry read")
	}
	assert.False(t, srv.allLiveHubsHaveAsyncStart(), "still false while the first refresh runs")
	close(st.release)
	require.Eventually(t, srv.allLiveHubsHaveAsyncStart, 5*time.Second, 5*time.Millisecond, "true once the refresh read the registry")
	assert.Equal(t, int32(1), st.reads.Load(), "one refresh at a time")

	// Within the TTL: cached, no read.
	st.set(nil, errors.New("db down"))
	advance(asyncStartGateTTL - time.Second)
	assert.True(t, srv.allLiveHubsHaveAsyncStart())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(1), st.reads.Load(), "no read within the TTL")

	// After the TTL: the cached value is returned once more while a
	// refresh runs, then the list error turns it false.
	advance(2 * time.Second)
	assert.True(t, srv.allLiveHubsHaveAsyncStart(), "the cached value while the refresh runs")
	require.Eventually(t, func() bool { return !srv.allLiveHubsHaveAsyncStart() }, 5*time.Second, 5*time.Millisecond, "a list error turns it false")
	assert.Equal(t, int32(2), st.reads.Load())
}

// TestAsyncStartGate_ExpiredAnswerNotUsed pins that an answer
// older than asyncStartGateMaxAge is not used when nothing refreshed it
// (no refresh loop, as when refreshes stall). The start reads false while
// the refresh runs, and the refreshed answer is used once it lands.
func TestAsyncStartGate_ExpiredAnswerNotUsed(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	var clockMu sync.Mutex
	prev := asyncStartGateNow
	asyncStartGateNow = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	t.Cleanup(func() { asyncStartGateNow = prev })
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	assert.False(t, srv.allLiveHubsHaveAsyncStart())
	require.Eventually(t, srv.allLiveHubsHaveAsyncStart, 5*time.Second, 5*time.Millisecond)

	// No refresh past the bound, with the next read held.
	release := make(chan struct{})
	st.mu.Lock()
	st.release = release
	st.mu.Unlock()
	advance(asyncStartGateMaxAge + time.Second)
	assert.False(t, srv.allLiveHubsHaveAsyncStart(), "an expired true is not used")
	assert.False(t, srv.allLiveHubsHaveAsyncStart(), "still false while the refresh runs")
	close(release)
	require.Eventually(t, srv.allLiveHubsHaveAsyncStart, 5*time.Second, 5*time.Millisecond, "the refreshed answer is used")
	assert.Equal(t, int32(2), st.reads.Load())
}

// TestAsyncStartAllowed_ExperimentFirst pins that the registry is not read
// while hub.async_agent_start resolves off. The test turns it off through
// the test registry's default (it is on by default since
// ptone/scion#7250); TestAsyncStartAllowed_OverrideOffRollback covers a
// real admin override.
func TestAsyncStartAllowed_ExperimentFirst(t *testing.T) {
	st := &hubInstanceListStore{now: time.Now()}
	srv := &Server{store: st}
	setAsyncStartExperiment(t, srv, false)
	assert.False(t, srv.asyncStartAllowed())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(0), st.reads.Load(), "no registry read while the experiment is off")
}

// setAsyncStartExperiment sets hub.async_agent_start's default on srv's
// registry.
func setAsyncStartExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.AsyncAgentStart {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.AsyncAgentStart))
}

// fakeAsyncStartGateClock replaces the gate's clock with one starting at
// now and returns the function that advances it.
func fakeAsyncStartGateClock(t *testing.T, now time.Time) func(time.Duration) {
	t.Helper()
	clock := now
	var mu sync.Mutex
	prev := asyncStartGateNow
	asyncStartGateNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	t.Cleanup(func() { asyncStartGateNow = prev })
	return func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
}

// TestAsyncStartGate_StartAfterPauseIsAsync pins ptone/scion#7250: with
// the experiment on and the refresh loop ticking, a start that arrives
// long after the previous one (90 s, then 10 minutes) answers async on its
// first read, without needing a refresh of its own.
func TestAsyncStartGate_StartAfterPauseIsAsync(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	advance := fakeAsyncStartGateClock(t, now)
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{
		hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1),
		hubRow(t, "b", now, 2*time.Second, false, api.HubInstanceFeatureAsyncStartV1),
	}}
	srv := &Server{store: st}
	setAsyncStartExperiment(t, srv, true)
	ctx := context.Background()

	srv.asyncStartGateRefreshTick(ctx) // the loop's first tick at start-up
	for _, pause := range []time.Duration{90 * time.Second, 10 * time.Minute} {
		for elapsed := time.Duration(0); elapsed < pause; elapsed += asyncStartGateRefreshInterval {
			advance(asyncStartGateRefreshInterval)
			srv.asyncStartGateRefreshTick(ctx)
		}
		advance(asyncStartGateRefreshInterval / 2) // the start lands between ticks
		reads := st.reads.Load()
		assert.True(t, srv.asyncStartAllowed(), "a start %v after the previous one answers async", pause)
		time.Sleep(20 * time.Millisecond)
		assert.Equal(t, reads, st.reads.Load(), "the start needed no refresh of its own")
	}
}

// TestAsyncStartGate_LoopFailClosed pins that the refresh loop keeps the
// fail-closed answers: after a passing tick, a tick that sees a live node
// without async_start_v1, a read error, unreadable stats or no live row
// makes the next start synchronous; so does a loop that stops ticking, once
// the answer is older than asyncStartGateMaxAge.
func TestAsyncStartGate_LoopFailClosed(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	f := api.HubInstanceFeatureAsyncStartV1
	good := func() []store.HubInstance { return []store.HubInstance{hubRow(t, "a", now, time.Second, false, f)} }
	cases := []struct {
		name  string
		rows  []store.HubInstance
		err   error
		stall bool
	}{
		{name: "a live node lacks the feature", rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, f), hubRow(t, "old", now, time.Second, false)}},
		{name: "read error", err: errors.New("db down")},
		{name: "unreadable stats", rows: []store.HubInstance{{ID: "x", Label: "bad", Version: "v", LastSeen: now, Stats: json.RawMessage(`{"features":`)}}},
		{name: "no live row", rows: []store.HubInstance{hubRow(t, "gone", now, hubInstanceStaleAfter+time.Second, false, f)}},
		{name: "loop stalled past the max age", rows: good(), stall: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			advance := fakeAsyncStartGateClock(t, now)
			st := &hubInstanceListStore{now: now, rows: good()}
			srv := &Server{store: st}
			setAsyncStartExperiment(t, srv, true)
			ctx := context.Background()
			srv.asyncStartGateRefreshTick(ctx)
			require.True(t, srv.asyncStartAllowed(), "passing before the fault")

			st.set(c.rows, c.err)
			if c.stall {
				advance(asyncStartGateMaxAge + time.Second)
			} else {
				advance(asyncStartGateRefreshInterval)
				srv.asyncStartGateRefreshTick(ctx)
				advance(time.Second)
			}
			assert.False(t, srv.asyncStartAllowed(), "the start runs synchronously")
			if c.stall {
				// Let the start's own background refresh land before the
				// clock is restored.
				require.Eventually(t, srv.asyncStartAllowed, 5*time.Second, time.Millisecond)
			}
		})
	}
}

// TestAsyncStartGate_NoRefreshWhileExperimentOff pins that the refresh
// loop does no registry work while hub.async_agent_start is off, and
// starts reading once it is on.
func TestAsyncStartGate_NoRefreshWhileExperimentOff(t *testing.T) {
	now := time.Now()
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	setAsyncStartExperiment(t, srv, false)

	for range 10 {
		srv.asyncStartGateRefreshTick(context.Background())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := srv.startAsyncStartGateRefresherEvery(ctx, func() time.Duration { return time.Millisecond })
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, int32(0), st.reads.Load(), "no registry read while the experiment is off")

	setAsyncStartExperiment(t, srv, true)
	srv.asyncStartGateRefreshTick(context.Background())
	assert.Equal(t, int32(1), st.reads.Load(), "a tick reads once the experiment is on")
	assert.True(t, srv.asyncStartAllowed())
}

// TestAsyncStartGate_RefresherStopsOnCancel pins that the refresh loop
// runs on its own and exits when its context is cancelled, with no read
// after it has exited.
func TestAsyncStartGate_RefresherStopsOnCancel(t *testing.T) {
	now := time.Now()
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	setAsyncStartExperiment(t, srv, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := srv.startAsyncStartGateRefresherEvery(ctx, func() time.Duration { return time.Millisecond })
	require.Eventually(t, func() bool { return st.reads.Load() >= 3 }, 5*time.Second, time.Millisecond, "the loop ticks on its own")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not exit after its context was cancelled")
	}
	reads := st.reads.Load()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, reads, st.reads.Load(), "no read after the loop exited")
}

// TestAsyncStartGate_RecoveryAfterLoggedFailureIsLogged pins that once a
// failed read has been logged, the return to the last logged answer is
// logged again (once), so the log never ends on "disabled" while the gate
// is enabled.
func TestAsyncStartGate_RecoveryAfterLoggedFailureIsLogged(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	advance := fakeAsyncStartGateClock(t, now)
	good := []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}
	h := &countingHandler{substr: "async start"}
	st := &hubInstanceListStore{now: now, rows: good}
	srv := &Server{store: st, agentLifecycleLog: slog.New(h)}
	setAsyncStartExperiment(t, srv, true)
	ctx := context.Background()

	srv.asyncStartGateRefreshTick(ctx)
	warns, infos := h.counts()
	require.Equal(t, 0, warns)
	require.Equal(t, 1, infos, "enabled is logged")

	st.set(nil, errors.New("db down"))
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	warns, infos = h.counts()
	require.Equal(t, 1, warns, "the failed read is logged")
	require.Equal(t, 1, infos)

	st.set(good, nil)
	for range 3 {
		advance(asyncStartGateRefreshInterval)
		srv.asyncStartGateRefreshTick(ctx)
	}
	warns, infos = h.counts()
	assert.Equal(t, 1, warns)
	assert.Equal(t, 2, infos, "the recovery is logged once, then nothing")
	assert.True(t, srv.asyncStartAllowed())
}

// TestAsyncStartGate_NoStartAfterStop pins the cleanup-before-startup
// race: once stopAsyncStartGateRefresher has run, a later
// startAsyncStartGateRefresher starts no loop (it would never be joined).
func TestAsyncStartGate_NoStartAfterStop(t *testing.T) {
	now := time.Now()
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	setAsyncStartExperiment(t, srv, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.stopAsyncStartGateRefresher()
	srv.startAsyncStartGateRefresher(ctx)
	srv.mu.RLock()
	h := srv.asyncStartGateRefresherStop
	srv.mu.RUnlock()
	assert.Nil(t, h, "no loop is started after the stop")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(0), st.reads.Load(), "no tick ran")
}

// TestAsyncStartGate_StopCancelsLoopOwnContext pins that the refresh loop
// runs on its own child context: stopAsyncStartGateRefresher ends it while
// the server context is still live (CleanupBackgroundResources stops it
// before the registry row is marked stopped), takes the handle, and is a
// no-op the second time.
func TestAsyncStartGate_StopCancelsLoopOwnContext(t *testing.T) {
	now := time.Now()
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	setAsyncStartExperiment(t, srv, true)

	serverCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startAsyncStartGateRefresher(serverCtx)
	require.Eventually(t, func() bool { return st.reads.Load() >= 1 }, 5*time.Second, time.Millisecond, "the first tick ran")
	srv.mu.RLock()
	h := srv.asyncStartGateRefresherStop
	srv.mu.RUnlock()
	require.NotNil(t, h)

	stopped := make(chan struct{})
	go func() { srv.stopAsyncStartGateRefresher(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(asyncStartGateStopWait - time.Second):
		t.Fatal("stopAsyncStartGateRefresher waited: the loop did not stop on its own context")
	}
	select {
	case <-h.done:
	default:
		t.Fatal("the loop had not exited when stopAsyncStartGateRefresher returned")
	}
	require.NoError(t, serverCtx.Err(), "the server context was never cancelled")
	srv.mu.RLock()
	assert.Nil(t, srv.asyncStartGateRefresherStop)
	srv.mu.RUnlock()
	srv.stopAsyncStartGateRefresher() // a second call is a no-op
}

// TestAsyncStartGate_ReadErrorWarnRateLimited pins that a persistent read
// failure, whose error text differs on every read, warns once per
// hubInstanceWarnEvery rather than on every refresh, that recovery is
// logged once, and that a registry flapping between failed and good reads
// logs at most one recovery per emitted warning: a suppressed failed read
// does not make the next good read log again.
func TestAsyncStartGate_ReadErrorWarnRateLimited(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	advance := fakeAsyncStartGateClock(t, now)
	h := &countingHandler{substr: "async start"}
	st := &hubInstanceListStore{now: now}
	srv := &Server{store: st, agentLifecycleLog: slog.New(h)}
	setAsyncStartExperiment(t, srv, true)
	ctx := context.Background()

	for i := range 10 {
		st.set(nil, fmt.Errorf("db down (attempt %d)", i))
		srv.asyncStartGateRefreshTick(ctx)
		assert.False(t, srv.asyncStartAllowed())
		advance(asyncStartGateRefreshInterval)
	}
	g := &srv.asyncStartGate
	g.mu.Lock()
	assert.Equal(t, asyncStartGateReadFailed, g.reason, "a read error stores the reason class, not the error text")
	g.mu.Unlock()
	warns, infos := h.counts()
	assert.Equal(t, 1, warns, "10 failing refreshes warn once")
	assert.Equal(t, 0, infos)

	advance(hubInstanceWarnEvery)
	st.set(nil, errors.New("db still down"))
	srv.asyncStartGateRefreshTick(ctx)
	warns, _ = h.counts()
	assert.Equal(t, 2, warns, "warned again after hubInstanceWarnEvery")

	st.set([]store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}, nil)
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	srv.asyncStartGateRefreshTick(ctx)
	warns, infos = h.counts()
	assert.Equal(t, 2, warns)
	assert.Equal(t, 1, infos, "recovery is logged once")
	assert.True(t, srv.asyncStartAllowed())

	// Flapping: failed and good reads alternate, inside one warn window.
	good := []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}
	for i := range 10 {
		advance(asyncStartGateRefreshInterval)
		st.set(nil, fmt.Errorf("db blip %d", i))
		srv.asyncStartGateRefreshTick(ctx)
		advance(asyncStartGateRefreshInterval)
		st.set(good, nil)
		srv.asyncStartGateRefreshTick(ctx)
		assert.True(t, srv.asyncStartAllowed())
	}
	warns, infos = h.counts()
	assert.Equal(t, 3, warns, "the flapping reads warn once more, when the 5-minute window reopens (the 10th failed read)")
	assert.Equal(t, 2, infos, "one recovery is logged, after the one failed read that warned; none after the suppressed ones")
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Equal(t, "", g.reason)
}

// gateLogRecorder keeps the level, message and attributes of every record.
type gateLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *gateLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *gateLogRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *gateLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *gateLogRecorder) WithGroup(string) slog.Handler      { return h }

// at returns the attributes (plus "msg") of every record at exactly level.
func (h *gateLogRecorder) at(level slog.Level) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, r := range h.records {
		if r.Level != level {
			continue
		}
		m := map[string]string{"msg": r.Message}
		r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.String(); return true })
		out = append(out, m)
	}
	return out
}

// TestAsyncStartGate_RefreshPanicFailsClosed pins that a panic in a
// registry read, on the loop or on a start's background refresh, does not
// stop the process, turns the answer false and clears inFlight, so the
// next refresh runs. The panic is logged at error with a stack cut to
// hubInstancePanicStackBytes, once per hubInstanceWarnEvery, and a read
// error warned just before does not suppress it (separate rate limits).
func TestAsyncStartGate_RefreshPanicFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	advance := fakeAsyncStartGateClock(t, now)
	good := []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}
	st := &hubInstanceListStore{now: now, rows: good}
	rec := &gateLogRecorder{}
	srv := &Server{store: st, agentLifecycleLog: slog.New(rec)}
	setAsyncStartExperiment(t, srv, true)
	ctx := context.Background()
	srv.asyncStartGateRefreshTick(ctx)
	require.True(t, srv.asyncStartAllowed())

	// A read error warns first.
	st.set(nil, errors.New("db down"))
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	require.Len(t, rec.at(slog.LevelWarn), 1)
	st.set(good, nil)
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	require.True(t, srv.asyncStartAllowed())
	require.Len(t, rec.at(slog.LevelInfo), 2, "enabled, then the recovery after the logged read error")

	// A panic on the loop.
	st.panicNext.Store(true)
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	assert.False(t, srv.asyncStartAllowed(), "a panicked refresh fails closed")
	errs := rec.at(slog.LevelError)
	require.Len(t, errs, 1, "the panic is logged at error, not suppressed by the read-error warning")
	assert.Contains(t, errs[0]["msg"], "panicked")
	assert.Equal(t, "registry read exploded", errs[0]["panic"])
	assert.NotEmpty(t, errs[0]["stack"])
	assert.LessOrEqual(t, len(errs[0]["stack"]), hubInstancePanicStackBytes)
	assert.Len(t, rec.at(slog.LevelWarn), 1, "the panic is not logged as a read error")
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	assert.True(t, srv.asyncStartAllowed(), "the next tick refreshed: inFlight was cleared")
	assert.Len(t, rec.at(slog.LevelInfo), 3, "the recovery after the logged panic is logged")

	// A panic on a start's background refresh (no loop tick), within
	// hubInstanceWarnEvery of the first: not logged again.
	st.panicNext.Store(true)
	advance(asyncStartGateTTL)
	assert.True(t, srv.asyncStartAllowed(), "the cached answer while the background refresh runs")
	require.Eventually(t, func() bool { return !srv.asyncStartAllowed() }, 5*time.Second, time.Millisecond, "the panicked background refresh fails closed")
	assert.Len(t, rec.at(slog.LevelError), 1, "a second panic within the window is suppressed")
	advance(asyncStartGateRefreshInterval)
	srv.asyncStartGateRefreshTick(ctx)
	assert.True(t, srv.asyncStartAllowed(), "inFlight was cleared after the background panic")
	assert.Len(t, rec.at(slog.LevelInfo), 3, "no recovery line after a suppressed panic")

	// After the window, a panic is logged again with the suppressed count.
	advance(hubInstanceWarnEvery)
	st.panicNext.Store(true)
	srv.asyncStartGateRefreshTick(ctx)
	errs = rec.at(slog.LevelError)
	require.Len(t, errs, 2)
	assert.Equal(t, "1", errs[1]["suppressed_since_last_log"])
}

// TestAsyncStartGate_CancelledRefreshClearsInFlight pins the shutdown
// path: a refresh whose context is cancelled stores and logs nothing, but
// clears inFlight, so the gate is never left wedged.
func TestAsyncStartGate_CancelledRefreshClearsInFlight(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fakeAsyncStartGateClock(t, now)
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	rec := &gateLogRecorder{}
	srv := &Server{store: st, agentLifecycleLog: slog.New(rec)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g := &srv.asyncStartGate
	g.mu.Lock()
	g.inFlight = true
	g.mu.Unlock()
	srv.refreshAsyncStartGate(ctx)

	g.mu.Lock()
	defer g.mu.Unlock()
	assert.False(t, g.inFlight, "inFlight is cleared on a cancelled refresh")
	assert.True(t, g.refreshedAt.IsZero(), "nothing is stored")
	assert.False(t, g.value)
	assert.Empty(t, rec.records, "nothing is logged")
}

// TestAsyncStartAllowed_DefaultOn pins the default-on of
// hub.async_agent_start (ptone/scion#7250): with no override (the registry
// default), both experiment checks see it on. A start consults the registry
// and answers async once the refresh lands, and a refresh-loop tick reads
// the registry.
func TestAsyncStartAllowed_DefaultOn(t *testing.T) {
	now := time.Now()
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	require.True(t, srv.experimentEnabled(experiments.AsyncAgentStart), "on by default")

	assert.False(t, srv.asyncStartAllowed(), "false until the first refresh lands")
	require.Eventually(t, srv.asyncStartAllowed, 5*time.Second, 5*time.Millisecond, "the default-on start check reads the registry and answers async")
	reads := st.reads.Load()
	require.GreaterOrEqual(t, reads, int32(1))

	srv.asyncStartGateRefreshTick(context.Background())
	assert.Equal(t, reads+1, st.reads.Load(), "a refresh-loop tick reads the registry under the default")
}

// TestAsyncStartAllowed_OverrideOffRollback pins the rollback path of the
// default-on (ptone/scion#7250), following hub.async_agent_launch's toggle
// test: on with no stored override, with the gate refreshed to a fresh true;
// an admin override to false then makes starts synchronous at once (the
// cached true is not used) and stops the loop's registry reads, without a
// restart; an override back to true turns them on again.
func TestAsyncStartAllowed_OverrideOffRollback(t *testing.T) {
	now := time.Now()
	st := &hubInstanceListStore{now: now, rows: []store.HubInstance{hubRow(t, "a", now, time.Second, false, api.HubInstanceFeatureAsyncStartV1)}}
	srv := &Server{store: st}
	settings := newFakeHubSettingStore()
	ops := NewOperationalSettings(settings, emptyKoanf(), emptyKoanf())
	seed := func(doc string) {
		t.Helper()
		settings.seed("experiments", json.RawMessage(doc))
		_, err := ops.Refresh(context.Background())
		require.NoError(t, err)
	}
	seed(`{"overrides":{}}`)
	srv.SetOperationalSettings(ops)
	require.True(t, srv.experimentEnabled(experiments.AsyncAgentStart), "on with no stored override (the default)")
	srv.asyncStartGateRefreshTick(context.Background())
	require.Equal(t, int32(1), st.reads.Load())
	require.True(t, srv.asyncStartAllowed(), "a fresh true while on")

	seed(fmt.Sprintf(`{"overrides":{%q:false}}`, experiments.AsyncAgentStart))
	require.False(t, srv.experimentEnabled(experiments.AsyncAgentStart), "off once an admin turns it off")
	assert.False(t, srv.asyncStartAllowed(), "starts are synchronous at once: the cached true is not used while off")
	srv.asyncStartGateRefreshTick(context.Background())
	assert.Equal(t, int32(1), st.reads.Load(), "no registry read while an admin has it off")

	// Same Server, no restart: the next read sees the change.
	seed(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.AsyncAgentStart))
	require.True(t, srv.experimentEnabled(experiments.AsyncAgentStart), "on again once the admin turns it back on")
	srv.asyncStartGateRefreshTick(context.Background())
	assert.Equal(t, int32(2), st.reads.Load(), "the loop reads the registry again")
	assert.True(t, srv.asyncStartAllowed(), "and starts answer async")
}

// TestStartAgentURL_AgentID pins design R1: both transports send the hub's
// agent ID (api.AgentIDQueryParam) on the start URL when the dispatcher
// attached one, next to projectId and runId, and nothing otherwise.
func TestStartAgentURL_AgentID(t *testing.T) {
	ctx := withDispatchAgentID(context.Background(), "agent a&b")

	var mu sync.Mutex
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"created":false}`))
	}))
	defer server.Close()
	client := NewHTTPRuntimeBrokerClient()
	_, err := client.StartAgent(ctx, tid("broker-1"), server.URL, "a1", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{RunID: "run-1"})
	require.NoError(t, err)
	_, err = client.StartAgent(context.Background(), tid("broker-1"), server.URL, "a1", "", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
	require.NoError(t, err)
	mu.Lock()
	require.Len(t, queries, 2)
	assert.Equal(t, "agent a&b", queries[0].Get(api.AgentIDQueryParam), "HTTP: agentId sent")
	assert.Equal(t, "p1", queries[0].Get("projectId"))
	assert.Equal(t, "run-1", queries[0].Get("runId"))
	assert.False(t, queries[1].Has(api.AgentIDQueryParam), "HTTP: no agentId without an agent ID")
	mu.Unlock()

	runOverHubTunnels(t, func(t *testing.T, tr hubTunnelTransport) {
		tunnel := &mockControlChannelTunnel{connected: true}
		cc := &ControlChannelBrokerClient{manager: tr.wrap(t, tunnel), signer: &mockBrokerSigner{}}
		_, err := cc.StartAgent(ctx, "broker-1", "unused", "a1", "", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
		require.NoError(t, err)
		require.NotNil(t, tunnel.lastRequest)
		assert.Equal(t, "/api/v1/agents/a1/start?"+api.AgentIDQueryParam+"=agent+a%26b", tunnel.lastRequest.Path, "control channel: agentId sent")

		_, err = cc.StartAgent(context.Background(), "broker-1", "unused", "a1", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
		require.NoError(t, err)
		assert.Equal(t, "/api/v1/agents/a1/start?projectId=p1", tunnel.lastRequest.Path, "control channel: no agentId without an agent ID")
	})
}

// TestLifecycleStartHasInputs_IgnoresAcceptAsync pins that the asynchronous
// opt-in alone is not an input a launching agent ignores (no warning),
// while forceResume still is.
func TestLifecycleStartHasInputs_IgnoresAcceptAsync(t *testing.T) {
	req := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/start", bytes.NewBufferString(body))
	}
	assert.False(t, lifecycleStartHasInputs(req(`{"acceptAsyncLaunch":true}`)), "the opt-in alone is not an ignored input")
	assert.True(t, lifecycleStartHasInputs(req(`{"forceResume":true}`)))
	assert.True(t, lifecycleStartHasInputs(req(`{"forceResume":true,"acceptAsyncLaunch":true}`)))
	assert.False(t, lifecycleStartHasInputs(req(`{}`)))
}

// TestStartDispatchOptions_LaunchKind pins the kind default.
func TestStartDispatchOptions_LaunchKind(t *testing.T) {
	assert.Equal(t, store.LaunchKindStart, StartDispatchOptions{}.launchKind())
	assert.Equal(t, store.LaunchKindStart, StartDispatchOptions{Kind: store.LaunchKindStart}.launchKind())
	assert.Equal(t, store.LaunchKindRestart, StartDispatchOptions{Kind: store.LaunchKindRestart}.launchKind())
}

// TestStartDidNotHappen_DeleteWrapped pins that an error that wraps
// store.ErrDeleteInProgress is never a start that did not happen, whatever
// else it wraps; the same refusal without the delete sentinel still is.
func TestStartDidNotHappen_DeleteWrapped(t *testing.T) {
	assert.False(t, startDidNotHappen(fmt.Errorf("%w: %w: %w", ErrLaunchInvalidPhase, store.ErrInvalidPhase, store.ErrDeleteInProgress)),
		"a delete-held launch refusal is the delete engine's")
	assert.True(t, startDidNotHappen(fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, store.ErrInvalidPhase)),
		"a plain launch refusal did not happen, as on main")
}
