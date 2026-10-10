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
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// countingHubInstanceStore is an in-memory store.HubInstanceStore that
// counts calls. missing makes the next Touch report found=false; failNext
// makes the next write fail.
type countingHubInstanceStore struct {
	mu       sync.Mutex
	upserts  int
	touches  int
	rows     map[string]store.HubInstance
	missing  bool
	failNext bool
}

func newCountingHubInstanceStore() *countingHubInstanceStore {
	return &countingHubInstanceStore{rows: map[string]store.HubInstance{}}
}

func (c *countingHubInstanceStore) UpsertHubInstance(_ context.Context, in store.HubInstance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upserts++
	if c.failNext {
		c.failNext = false
		return errors.New("write failed")
	}
	c.rows[in.ID] = in
	return nil
}

func (c *countingHubInstanceStore) TouchHubInstance(_ context.Context, id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touches++
	if c.failNext {
		c.failNext = false
		return false, errors.New("write failed")
	}
	if c.missing {
		c.missing = false
		delete(c.rows, id)
		return false, nil
	}
	_, ok := c.rows[id]
	return ok, nil
}

func (c *countingHubInstanceStore) ListHubInstances(context.Context, time.Duration) ([]store.HubInstance, time.Time, error) {
	return nil, time.Now(), nil
}

func (c *countingHubInstanceStore) counts() (upserts, touches int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.upserts, c.touches
}

// newTestHubInstanceRegistry returns a registry writer over st whose
// snapshot is *snap (so a test can change it between ticks).
func newTestHubInstanceRegistry(st store.HubInstanceStore, snap *hubInstanceSnapshot) *hubInstanceRegistry {
	return &hubInstanceRegistry{
		id:       "hub-test-1",
		store:    st,
		snapshot: func(context.Context) hubInstanceSnapshot { return *snap },
		interval: func() time.Duration { return time.Hour },
		log:      slog.New(slog.DiscardHandler),
	}
}

func quietSnapshot() *hubInstanceSnapshot {
	return &hubInstanceSnapshot{
		Label: "hub-a", Version: "v1", Status: HealthStatusHealthy,
		Checks: map[string]string{"database": "healthy"},
	}
}

// Over 20 quiet ticks (nothing material changes), the writer upserts once
// (the first tick) and touches 19 times.
func TestHubInstanceRegistry_TwentyQuietTicks(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		reg.tick(ctx)
	}
	upserts, touches := st.counts()
	assert.Equal(t, 1, upserts)
	assert.Equal(t, 19, touches)

	// The 21st tick is the next forced upsert (every 20th tick).
	reg.tick(ctx)
	upserts, touches = st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 19, touches)
}

func TestHubInstanceRegistry_MaterialChangeUpserts(t *testing.T) {
	st := newCountingHubInstanceStore()
	snap := quietSnapshot()
	reg := newTestHubInstanceRegistry(st, snap)
	ctx := context.Background()

	reg.tick(ctx) // first tick: upsert
	reg.tick(ctx) // quiet: touch

	snap.Checks = map[string]string{"database": "unhealthy"}
	snap.Status = HealthStatusUnhealthy
	reg.tick(ctx) // changed: upsert
	reg.tick(ctx) // quiet again: touch

	snap.Version = "v2"
	reg.tick(ctx) // changed: upsert

	upserts, touches := st.counts()
	assert.Equal(t, 3, upserts)
	assert.Equal(t, 2, touches)
	assert.Equal(t, "v2", st.rows["hub-test-1"].Version)
	assert.Equal(t, HealthStatusUnhealthy, st.rows["hub-test-1"].Status)
}

// A Touch that finds no row (pruned, or never written) upserts in the same
// tick.
func TestHubInstanceRegistry_TouchNotFoundUpserts(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx := context.Background()

	reg.tick(ctx)
	st.missing = true
	reg.tick(ctx)

	upserts, touches := st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 1, touches)
	assert.Contains(t, st.rows, "hub-test-1", "the row is written again")
}

// After a failed write the next tick upserts, even when nothing changed.
func TestHubInstanceRegistry_FailedWriteUpsertsNext(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx := context.Background()

	reg.tick(ctx) // upsert
	st.failNext = true
	reg.tick(ctx) // touch fails
	reg.tick(ctx) // upsert (last write failed)
	reg.tick(ctx) // touch

	upserts, touches := st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 2, touches)
}

func TestJitteredHubInstanceInterval_WithinTenPercent(t *testing.T) {
	lo := hubInstanceTickInterval - hubInstanceTickInterval/10
	hi := hubInstanceTickInterval + hubInstanceTickInterval/10
	for i := 0; i < 1000; i++ {
		d := jitteredHubInstanceInterval()
		require.GreaterOrEqual(t, d, lo)
		require.LessOrEqual(t, d, hi)
	}
}

// tickCountingStore wraps a real store and counts the agent, project and
// runtime broker list calls that GetHealthInfo's stats make.
type tickCountingStore struct {
	store.Store
	mu      sync.Mutex
	agents  int
	project int
	brokers int
}

func (c *tickCountingStore) ListAgents(ctx context.Context, f store.AgentFilter, o store.ListOptions) (*store.ListResult[store.Agent], error) {
	c.mu.Lock()
	c.agents++
	c.mu.Unlock()
	return c.Store.ListAgents(ctx, f, o)
}

func (c *tickCountingStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	c.mu.Lock()
	c.project++
	c.mu.Unlock()
	return c.Store.ListProjects(ctx, f, o)
}

func (c *tickCountingStore) ListRuntimeBrokers(ctx context.Context, f store.RuntimeBrokerFilter, o store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	c.mu.Lock()
	c.brokers++
	c.mu.Unlock()
	return c.Store.ListRuntimeBrokers(ctx, f, o)
}

// The registry tick runs the health checks only: no ListAgents,
// ListProjects or runtime broker count. It writes this server's row with
// its instance ID, version and normalised checks.
func TestHubInstanceRegistry_TickRunsNoCountQueries(t *testing.T) {
	srv, s, counting, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *tickCountingStore {
		return &tickCountingStore{Store: inner}
	})
	ctx := context.Background()

	reg := srv.newHubInstanceRegistry()
	for i := 0; i < 3; i++ {
		reg.tick(ctx)
	}

	counting.mu.Lock()
	assert.Zero(t, counting.agents, "tick must not call ListAgents")
	assert.Zero(t, counting.project, "tick must not call ListProjects")
	assert.Zero(t, counting.brokers, "tick must not list runtime brokers")
	counting.mu.Unlock()

	rows, _, err := s.ListHubInstances(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, srv.InstanceID(), rows[0].ID)
	assert.NotEmpty(t, rows[0].Version)
	assert.Equal(t, "healthy", rows[0].Checks["database"])
	assert.Equal(t, deriveHealthStatus(srv.healthChecks(ctx)), rows[0].Status)
}

// The stored status comes from the raw checks; the stored checks are
// normalised to fixed values, and an existing long check name
// (workspace_storage_mount_verification, 36 characters) is kept.
func TestHubInstanceSnapshot_StatusFromRawChecksAndNormalisedChecks(t *testing.T) {
	raw := map[string]string{
		"database":                             "healthy",
		"workspace_storage":                    "healthy",
		"workspace_storage_mount_verification": "unavailable: could not compare filesystem device IDs",
		strings.Repeat("x", 65):                "unhealthy: detail",
	}
	snap := hubInstanceSnapshotFromChecks("hub-a", "v1", raw)
	// The 65-character key is dropped from the stored checks, but it is a
	// non-critical non-healthy check, so the status is degraded.
	assert.Equal(t, HealthStatusDegraded, snap.Status)
	assert.Equal(t, map[string]string{
		"database":                             "healthy",
		"workspace_storage":                    "healthy",
		"workspace_storage_mount_verification": "unavailable",
	}, snap.Checks)
}

func TestHubInstanceLabel(t *testing.T) {
	t.Run("pod name", func(t *testing.T) {
		t.Setenv("POD_NAME", "scion-hub-7d9f")
		t.Setenv("K_REVISION", "")
		assert.Equal(t, "scion-hub-7d9f", hubInstanceLabel("scion-hub-7d9f-1234"))
	})
	t.Run("cloud run revision", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("K_REVISION", "scion-hub-00042-abc")
		assert.Equal(t, "scion-hub-00042-abc/0123abcd", hubInstanceLabel("0123abcd-ef01-2345-6789-abcdef012345"))
	})
	t.Run("host name", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("K_REVISION", "")
		assert.NotEmpty(t, hubInstanceLabel("0123abcd"))
	})
	t.Run("bounded printable", func(t *testing.T) {
		t.Setenv("POD_NAME", "pod\x01\x7fname"+strings.Repeat("p", 100))
		got := hubInstanceLabel("x")
		assert.Len(t, got, hubInstanceMaxLabelBytes)
		assert.True(t, strings.HasPrefix(got, "podname"))
	})
}

// startHubInstanceRegistry returns after the first tick, so this replica's
// row exists before the listener serves; cancelling the context ends the
// loop and closes its done channel.
func TestStartHubInstanceRegistry_FirstTickBeforeReturn(t *testing.T) {
	srv, s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := srv.startHubInstanceRegistry(ctx)

	rows, _, err := s.ListHubInstances(context.Background(), time.Hour)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, srv.InstanceID(), rows[0].ID)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registry loop did not exit after its context was cancelled")
	}
}

// Cancelling the context makes the loop exit and close done.
func TestStartHubInstanceRegistryLoop_CancelClosesDone(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := startHubInstanceRegistryLoop(ctx, reg, time.Second)
	upserts, _ := st.counts()
	assert.Equal(t, 1, upserts, "the first tick ran before the start returned")
	select {
	case <-done:
		t.Fatal("done closed while the context is still live")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registry loop did not exit after its context was cancelled")
	}
}

// When the first tick hangs, the start returns at the wait bound and the
// tick keeps running in the background.
func TestStartHubInstanceRegistryLoop_StartReturnsAtBoundWhenFirstTickHangs(t *testing.T) {
	st := newCountingHubInstanceStore()
	release := make(chan struct{})
	entered := make(chan struct{})
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	reg.snapshot = func(context.Context) hubInstanceSnapshot {
		close(entered)
		<-release
		return *quietSnapshot()
	}
	ctx, cancel := context.WithCancel(context.Background())
	var done <-chan struct{}
	t.Cleanup(func() {
		cancel()
		close(release)
		if done != nil {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("registry loop did not exit after release and cancel")
			}
		}
	})

	const wait = 50 * time.Millisecond
	began := time.Now()
	done = startHubInstanceRegistryLoop(ctx, reg, wait)
	elapsed := time.Since(began)

	<-entered
	assert.GreaterOrEqual(t, elapsed, wait)
	assert.Less(t, elapsed, 2*time.Second, "start must return at the bound, not wait for the tick")
	upserts, touches := st.counts()
	assert.Zero(t, upserts+touches, "the first tick is still blocked")
	select {
	case <-done:
		t.Fatal("done closed while the first tick is still running")
	default:
	}
}

// A panicking tick is recovered: the goroutine keeps running, and the next
// tick upserts because the panicked tick counts as a failed write.
func TestHubInstanceRegistry_PanickingTickIsRecovered(t *testing.T) {
	st := newCountingHubInstanceStore()
	snap := quietSnapshot()
	reg := newTestHubInstanceRegistry(st, snap)
	panicNext := false
	reg.snapshot = func(context.Context) hubInstanceSnapshot {
		if panicNext {
			panicNext = false
			panic("health check fault")
		}
		return *snap
	}
	ctx := context.Background()

	reg.safeTick(ctx) // tick 0: upsert
	panicNext = true
	assert.NotPanics(t, func() { reg.safeTick(ctx) }) // tick 1: panics, recovered
	reg.safeTick(ctx)                                 // tick 2: upsert, not touch
	reg.safeTick(ctx)                                 // tick 3: touch

	upserts, touches := st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 1, touches)
}

// The loop survives a panicking first tick and closes done on cancel.
func TestStartHubInstanceRegistryLoop_SurvivesPanic(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	var mu sync.Mutex
	calls := 0
	reg.snapshot = func(context.Context) hubInstanceSnapshot {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			panic("health check fault")
		}
		return *quietSnapshot()
	}
	reg.interval = func() time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := startHubInstanceRegistryLoop(ctx, reg, time.Second)
	require.Eventually(t, func() bool {
		upserts, _ := st.counts()
		return upserts >= 1
	}, 2*time.Second, time.Millisecond, "the loop keeps ticking after a panic")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registry loop did not exit after its context was cancelled")
	}
}

// recordingHandler is a slog.Handler that keeps every record.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// panicLines returns the attributes of every recorded panic log line.
func (h *recordingHandler) panicLines() []map[string]slog.Value {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]slog.Value
	for _, r := range h.records {
		if !strings.Contains(r.Message, "tick panicked") {
			continue
		}
		attrs := map[string]slog.Value{}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value
			return true
		})
		out = append(out, attrs)
	}
	return out
}

// newPanickingRegistry returns a registry whose every tick panics, logging
// to h, with a fake rate-limit clock advanced by the caller.
func newPanickingRegistry(h *recordingHandler, now *time.Time) *hubInstanceRegistry {
	reg := newTestHubInstanceRegistry(newCountingHubInstanceStore(), quietSnapshot())
	reg.snapshot = func(context.Context) hubInstanceSnapshot { panic("health check fault") }
	reg.log = slog.New(h)
	reg.now = func() time.Time { return *now }
	return reg
}

// A tick that panics on every run logs a bounded number of lines: the
// first panic with its stack, then at most one line per
// hubInstanceWarnEvery, each carrying the count of panics not logged.
func TestHubInstanceRegistry_PersistentPanicLogIsRateLimited(t *testing.T) {
	h := &recordingHandler{}
	now := hubInstanceT0
	reg := newPanickingRegistry(h, &now)
	ctx := context.Background()

	// 1000 ticks at the nominal 15 s interval: 250 minutes.
	const ticks = 1000
	for i := 0; i < ticks; i++ {
		reg.safeTick(ctx)
		now = now.Add(hubInstanceTickInterval)
	}

	lines := h.panicLines()
	// One line per 5-minute window: 250 min / 5 min = 50 lines.
	assert.Len(t, lines, 50)
	require.NotEmpty(t, lines)

	first := lines[0]
	assert.Equal(t, int64(0), first["suppressed_since_last_log"].Int64())
	assert.Equal(t, "health check fault", first["panic"].String())
	stack := first["stack"].String()
	assert.Contains(t, stack, "safeTick", "the first line carries the stack trace")
	assert.LessOrEqual(t, len(stack), hubInstancePanicStackBytes)

	// 20 ticks per 5-minute window: one logged, 19 suppressed.
	assert.Equal(t, int64(19), lines[1]["suppressed_since_last_log"].Int64())
}

// Many panicking ticks inside one window log a single line.
func TestHubInstanceRegistry_RapidPanicsLogOnce(t *testing.T) {
	h := &recordingHandler{}
	now := hubInstanceT0
	reg := newPanickingRegistry(h, &now)
	ctx := context.Background()

	for i := 0; i < 500; i++ {
		reg.safeTick(ctx)
	}
	assert.Len(t, h.panicLines(), 1)
}

// Once the context is done (shutdown), a recovered panic is not logged.
func TestHubInstanceRegistry_PanicAfterCancelIsNotLogged(t *testing.T) {
	h := &recordingHandler{}
	now := hubInstanceT0
	reg := newPanickingRegistry(h, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.NotPanics(t, func() { reg.safeTick(ctx) })
	assert.Empty(t, h.panicLines())
}
