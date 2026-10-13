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

package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

// fakeClock is a settable clock for CloudWriteStats.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// countingSlogHandler counts every record that reaches the slog default.
type countingSlogHandler struct{ n *atomic.Int64 }

func (h countingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingSlogHandler) Handle(context.Context, slog.Record) error {
	h.n.Add(1)
	return nil
}
func (h countingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingSlogHandler) WithGroup(string) slog.Handler      { return h }

// captureSlogDefault installs a counting default handler for the test.
func captureSlogDefault(t *testing.T) *atomic.Int64 {
	t.Helper()
	prev := slog.Default()
	n := &atomic.Int64{}
	slog.SetDefault(slog.New(countingSlogHandler{n: n}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return n
}

func cloudCounts(s *CloudWriteStats) map[string]uint64 {
	out := map[string]uint64{}
	for r := CloudFailureReason(0); r < numCloudReasons; r++ {
		out[r.String()] = s.Failures(r)
	}
	return out
}

func TestCloudFailureReason_ClosedSet(t *testing.T) {
	want := []string{"error", "queue_full", "circuit_open", "flush_error"}
	if int(numCloudReasons) != len(want) {
		t.Fatalf("numCloudReasons = %d, want %d", numCloudReasons, len(want))
	}
	for i, w := range want {
		if got := CloudFailureReason(i).String(); got != w {
			t.Errorf("reason %d = %q, want %q", i, got, w)
		}
	}
	// The union with the async writer's failure reasons stays within ten
	// values on scion.logging.write.failures.
	union := map[string]bool{}
	for _, w := range want {
		union[w] = true
	}
	// The async writer's failure reasons, from the production mapping
	// WriteMetrics exports (asyncFailureReasons).
	for _, f := range asyncFailureReasons {
		union[f.result.String()] = true
	}
	if len(union) != 9 {
		t.Fatalf("reason union = %d values (%v), want 9", len(union), union)
	}
	s := newCloudWriteStats(nil)
	s.RecordFailure(numCloudReasons) // out of range: ignored
	for r, n := range cloudCounts(s) {
		if n != 0 {
			t.Fatalf("out-of-range reason counted as %s", r)
		}
	}
}

// P2-1 / R8 T1, T2, T3, T5: the named OnError hook counts first (error, or
// queue_full for a wrapped gcplog.ErrOverflow), then writes exactly
// "logging client: <err>\n" once per error to the injected writer, and
// sends nothing through slog (so nothing reaches stdout JSON, OTel or the
// Cloud handler). A nil error is a no-op.
func TestCloudClientOnError_CountsThenWritesStderrOnly(t *testing.T) {
	// T3: capture the slog default; as in production, slog.SetDefault also
	// routes the std log package into it.
	slogged := captureSlogDefault(t)
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	s := newCloudWriteStats(clk.Now)
	var w bytes.Buffer
	hook := cloudClientOnError(s, &w)

	// T1: one plain error.
	other := errors.New("rpc error: code = Unavailable desc = secret-ish text")
	hook(other)
	if got, want := w.String(), "logging client: "+other.Error()+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if got := cloudCounts(s); got["error"] != 1 || got["queue_full"] != 0 {
		t.Fatalf("counts after T1 = %v", got)
	}
	if !s.LastFailure().Equal(clk.Now()) {
		t.Fatalf("LastFailure = %v, want %v", s.LastFailure(), clk.Now())
	}

	// T2: a wrapped overflow is queue_full, same line format.
	w.Reset()
	wrapped := fmt.Errorf("bundler: %w", gcplog.ErrOverflow)
	hook(wrapped)
	if got, want := w.String(), "logging client: "+wrapped.Error()+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if got := cloudCounts(s); got["queue_full"] != 1 || got["error"] != 1 {
		t.Fatalf("counts after T2 = %v", got)
	}

	// Oversized entry is an error; one line per error.
	w.Reset()
	hook(gcplog.ErrOversizedEntry)
	hook(other)
	if got, want := w.String(), "logging client: "+gcplog.ErrOversizedEntry.Error()+"\nlogging client: "+other.Error()+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	// T5: nil is a no-op: no count, no write.
	w.Reset()
	before := cloudCounts(s)
	hook(nil)
	if w.Len() != 0 {
		t.Fatalf("nil error wrote %q", w.String())
	}
	after := cloudCounts(s)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("nil error changed %s: %d -> %d", k, v, after[k])
		}
	}
	if got := cloudCounts(s); got["error"] != 3 || got["queue_full"] != 1 || got["circuit_open"] != 0 || got["flush_error"] != 0 {
		t.Fatalf("final counts = %v", got)
	}

	// T3: nothing reached slog (or std log through it).
	if n := slogged.Load(); n != 0 {
		t.Fatalf("hook produced %d slog records", n)
	}

	// A nil writer only counts and does not panic.
	cloudClientOnError(s, nil)(other)
	if s.Failures(CloudReasonError) != 4 {
		t.Fatalf("error = %d after nil-writer call", s.Failures(CloudReasonError))
	}
}

// R8 T4: errors counted by the hook export with exactly {writer, reason}
// and closed reason values; no error text leaks into attributes.
func TestCloudClientOnError_NoErrorTextInLabels(t *testing.T) {
	s := newCloudWriteStats(nil)
	hook := cloudClientOnError(s, io.Discard)
	const secret = "secret-ish text 12345"
	hook(errors.New(secret))
	hook(fmt.Errorf("%s: %w", secret, gcplog.ErrOverflow))
	reader := sdkmetric.NewManualReader()
	wm := newManualWriteMetrics(t, reader)
	wm.ObserveCloud(s)
	seen := map[string]int64{}
	for name, pts := range collectPoints(t, reader) {
		for _, p := range pts {
			if name != MetricWriteFailures {
				continue
			}
			keys := attrKeys(p)
			if len(keys) != 2 || keys[0] != "reason" || keys[1] != "writer" {
				t.Fatalf("attrs = %v", p.attrs)
			}
			for _, v := range p.attrs {
				if strings.Contains(v, "secret") {
					t.Fatalf("error text in labels: %v", p.attrs)
				}
			}
			if p.attrs["writer"] != CloudWriterName {
				t.Fatalf("writer = %q", p.attrs["writer"])
			}
			seen[p.attrs["reason"]] = p.value
		}
	}
	if len(seen) != 2 || seen["error"] != 1 || seen["queue_full"] != 1 {
		t.Fatalf("exported failures = %v", seen)
	}
}

func TestCloudWriteStats_LastFailureMonotonic(t *testing.T) {
	clk := &fakeClock{now: time.Unix(2_000, 0)}
	s := newCloudWriteStats(clk.Now)
	s.RecordFailure(CloudReasonError)
	clk.Set(time.Unix(1_000, 0)) // clock moves backwards
	s.RecordFailure(CloudReasonFlushError)
	if want := time.Unix(2_000, 0); !s.LastFailure().Equal(want) {
		t.Fatalf("LastFailure = %v, want %v", s.LastFailure(), want)
	}
	if s.Failures(CloudReasonError) != 1 || s.Failures(CloudReasonFlushError) != 1 {
		t.Fatalf("counts = %v", cloudCounts(s))
	}
}

// Health precedence and the 5-minute recent-failure window, on a fake clock.
func TestCloudWriteStats_Health(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	s := newCloudWriteStats(clk.Now)
	if s.Configured() {
		t.Fatal("configured without a circuit source")
	}
	var open atomic.Bool
	unregister := s.RegisterCircuitSource(open.Load)
	if !s.Configured() {
		t.Fatal("not configured with a circuit source")
	}
	now := clk.Now()
	if got := s.HealthStatus(now); got != "healthy" {
		t.Fatalf("initial = %q", got)
	}
	s.RecordFailure(CloudReasonQueueFull)
	if got := s.HealthStatus(now); got != "degraded: recent write failures" {
		t.Fatalf("after failure = %q", got)
	}
	open.Store(true)
	if got := s.HealthStatus(now); got != "degraded: circuit open" {
		t.Fatalf("circuit open = %q", got)
	}
	open.Store(false)
	if got := s.HealthStatus(now.Add(asyncwrite.DefaultFailureWindow - time.Second)); got != "degraded: recent write failures" {
		t.Fatalf("inside window = %q", got)
	}
	if got := s.HealthStatus(now.Add(asyncwrite.DefaultFailureWindow)); got != "healthy" {
		t.Fatalf("after window = %q", got)
	}
	unregister()
	if s.Configured() {
		t.Fatal("still configured after unregister")
	}
}

// manualTimers is an asyncwrite.AfterFunc that records each callback for
// the test to fire instead of scheduling it.
type manualTimers struct {
	mu  sync.Mutex
	fns []func()
}

func (m *manualTimers) after(_ time.Duration, f func()) asyncwrite.Timer {
	m.mu.Lock()
	m.fns = append(m.fns, f)
	m.mu.Unlock()
	return neverTimer{}
}

// fireLast runs the most recently registered callback: while a write is
// inside the handler, that is its write-budget timer.
func (m *manualTimers) fireLast() {
	m.mu.Lock()
	f := m.fns[len(m.fns)-1]
	m.mu.Unlock()
	f()
}

// A handler's cleanup withdraws only its own circuit registration.
func TestCloudWriteStats_RegisterCircuitSourceUnregistersOnlyItself(t *testing.T) {
	s := newCloudWriteStats(nil)
	unA := s.RegisterCircuitSource(func() bool { return true })
	if open, ok := s.CircuitOpen(); !ok || !open {
		t.Fatalf("A: open=%v ok=%v", open, ok)
	}
	unB := s.RegisterCircuitSource(func() bool { return false })
	unA() // stale: B stays registered
	if open, ok := s.CircuitOpen(); !ok || open {
		t.Fatalf("after unregistering A: open=%v ok=%v, want B (false, true)", open, ok)
	}
	unB()
	if s.Configured() {
		t.Fatal("still configured after unregistering B")
	}
	unB() // idempotent
	s.RegisterCircuitSource(nil)()
	var nilStats *CloudWriteStats
	nilStats.RegisterCircuitSource(func() bool { return true })()
}

// newObservedResilient builds a ResilientCloudHandler whose inner
// CloudHandler hands entries to a counter instead of the API, with its own
// writer=cloud stats registered as the circuit source. The health loop is
// not started; tests drive runHealthCheck.
func newObservedResilient(t *testing.T, cfg ResilientCloudHandlerConfig) (*ResilientCloudHandler, *CloudWriteStats, *atomic.Int64) {
	t.Helper()
	sent := &atomic.Int64{}
	inner := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
		hostname:  "test-host",
		logHook:   func(gcplog.Entry) { sent.Add(1) },
	}
	cfg.applyDefaults()
	cb := &circuitBreaker{}
	cb.state.Store(int32(circuitClosed))
	stats := newCloudWriteStats(nil)
	h := &ResilientCloudHandler{
		inner:  inner,
		config: cfg,
		cb:     cb,
		done:   make(chan struct{}),
		stats:  stats,
	}
	stats.RegisterCircuitSource(h.CircuitOpen)
	return h, stats, sent
}

// cloudPoints collects the writer=cloud series from reader.
func cloudPoints(t *testing.T, reader *sdkmetric.ManualReader) (failures map[string]int64, circuit []int64) {
	t.Helper()
	failures = map[string]int64{}
	for name, pts := range collectPoints(t, reader) {
		for _, p := range pts {
			if p.attrs["writer"] != CloudWriterName {
				continue
			}
			switch name {
			case MetricWriteFailures:
				failures[p.attrs["reason"]] = p.value
			case MetricWriterCircuitOpen:
				circuit = append(circuit, p.value)
			}
		}
	}
	return failures, circuit
}

func newManualWriteMetrics(t *testing.T, readers ...*sdkmetric.ManualReader) *WriteMetrics {
	t.Helper()
	opts := make([]sdkmetric.Option, 0, len(readers))
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	wm, err := NewWriteMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	return wm
}

// P2-2 (ResilientCloudHandler) and P2-3: closed -> open -> half-open ->
// closed. Each record dropped while open or half-open counts circuit_open;
// the gauge follows the state; each failed flush counts flush_error once.
// Synchronization-based: an injected flush func and a back-dated state
// change instead of waiting for OpenDuration.
func TestResilientCloudHandler_CountsCircuitDropsAndFlushErrors(t *testing.T) {
	h, stats, sent := newObservedResilient(t, ResilientCloudHandlerConfig{
		MaxFailures:  2,
		OpenDuration: time.Minute,
		ProbeTimeout: time.Hour, // never the deciding path here
	})
	reader := sdkmetric.NewManualReader()
	wm := newManualWriteMetrics(t, reader)
	wm.ObserveCloud(stats)
	ctx := context.Background()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "entry", 0)

	// Closed: forwarded, nothing counted, gauge 0.
	if err := h.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 1 {
		t.Fatalf("sent = %d", sent.Load())
	}
	if f, c := cloudPoints(t, reader); len(f) != 0 || len(c) != 1 || c[0] != 0 {
		t.Fatalf("closed: failures %v circuit %v", f, c)
	}

	// waitFlushIdle waits until the previous flush goroutine has cleared
	// flushInFlight. flushOnce clears it in a deferred store that runs after
	// the result is sent, so receiving the result does not order it; without
	// this wait the next flush could be rejected as "still in progress"
	// without calling the test's flushFn. The loop always ends (the store is
	// unconditional); the deadline only turns a bug into a failure, it is
	// not a timing assertion.
	waitFlushIdle := func() {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for h.flushInFlight.Load() {
			if time.Now().After(deadline) {
				t.Fatal("previous flush never cleared flushInFlight")
			}
			runtime.Gosched()
		}
	}

	// Two failed periodic flushes open the circuit: flush_error 2.
	h.flushFn = func() error { return errors.New("flush failed") }
	h.runHealthCheck()
	if stats.Failures(CloudReasonFlushError) != 1 || h.CircuitOpen() {
		t.Fatalf("after 1 failure: flush_error %d open %v", stats.Failures(CloudReasonFlushError), h.CircuitOpen())
	}
	h.runHealthCheck()
	if !h.CircuitOpen() {
		t.Fatal("circuit not open after MaxFailures")
	}
	// Open: three drops, none forwarded.
	for i := 0; i < 3; i++ {
		if err := h.Handle(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if sent.Load() != 1 {
		t.Fatalf("forwarded while open: sent = %d", sent.Load())
	}
	f, c := cloudPoints(t, reader)
	if f["circuit_open"] != 3 || f["flush_error"] != 2 || len(c) != 1 || c[0] != 1 {
		t.Fatalf("open: failures %v circuit %v", f, c)
	}
	// Derived handlers share the circuit and the stats.
	if err := h.WithAttrs([]slog.Attr{slog.String("k", "v")}).Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := h.WithGroup("g").Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	if stats.Failures(CloudReasonCircuitOpen) != 5 {
		t.Fatalf("derived drops: circuit_open = %d", stats.Failures(CloudReasonCircuitOpen))
	}

	// Open, OpenDuration not elapsed: no probe, no new flush_error.
	h.runHealthCheck()
	if stats.Failures(CloudReasonFlushError) != 2 {
		t.Fatalf("flush without probe: flush_error = %d", stats.Failures(CloudReasonFlushError))
	}
	backdate := func() {
		h.cb.mu.Lock()
		h.cb.lastStateChange = time.Now().Add(-2 * h.config.OpenDuration)
		h.cb.mu.Unlock()
	}
	// Failed probe: flush_error +1, still open.
	waitFlushIdle()
	backdate()
	h.runHealthCheck()
	if stats.Failures(CloudReasonFlushError) != 3 || !h.CircuitOpen() {
		t.Fatalf("failed probe: flush_error %d open %v", stats.Failures(CloudReasonFlushError), h.CircuitOpen())
	}

	// Half-open: the probe's flush blocks until the test releases it.
	entered := make(chan struct{})
	release := make(chan error)
	waitFlushIdle()
	h.flushFn = func() error {
		close(entered)
		return <-release
	}
	backdate()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.runHealthCheck()
	}()
	<-entered
	if st := circuitState(h.cb.state.Load()); st != circuitHalfOpen {
		t.Fatalf("state during probe = %d, want half-open", st)
	}
	if err := h.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	f, c = cloudPoints(t, reader)
	if f["circuit_open"] != 6 || len(c) != 1 || c[0] != 1 {
		t.Fatalf("half-open: failures %v circuit %v", f, c)
	}
	release <- nil // probe succeeds
	<-done

	// Closed again: drops stop, gauge 0, counts unchanged, forwarding resumes.
	if h.CircuitOpen() {
		t.Fatal("circuit still open after a successful probe")
	}
	if err := h.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 2 {
		t.Fatalf("sent after close = %d", sent.Load())
	}
	f, c = cloudPoints(t, reader)
	if f["circuit_open"] != 6 || f["flush_error"] != 3 || len(c) != 1 || c[0] != 0 {
		t.Fatalf("closed again: failures %v circuit %v", f, c)
	}

	// A successful periodic flush adds nothing.
	waitFlushIdle()
	h.flushFn = func() error { return nil }
	h.runHealthCheck()
	if stats.Failures(CloudReasonFlushError) != 3 {
		t.Fatalf("successful flush counted: %d", stats.Failures(CloudReasonFlushError))
	}
}

// P2-3: a flush rejected because another is still in flight is one failure.
func TestResilientCloudHandler_ConcurrentFlushCountsOnce(t *testing.T) {
	h, stats, _ := newObservedResilient(t, ResilientCloudHandlerConfig{ProbeTimeout: time.Hour})
	entered := make(chan struct{})
	release := make(chan struct{})
	h.flushFn = func() error {
		close(entered)
		<-release
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- h.flushWithTimeout() }()
	<-entered
	if err := h.flushWithTimeout(); err == nil {
		t.Fatal("concurrent flush succeeded")
	}
	if n := stats.Failures(CloudReasonFlushError); n != 1 {
		t.Fatalf("flush_error = %d, want 1", n)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if n := stats.Failures(CloudReasonFlushError); n != 1 {
		t.Fatalf("flush_error after success = %d, want 1", n)
	}
}

// P2-2 (circuitGatedHandler, the request and message loggers): each
// skipped record counts circuit_open; forwarding records count nothing.
func TestCircuitGatedHandler_CountsDrops(t *testing.T) {
	stats := newCloudWriteStats(nil)
	var open atomic.Bool
	forwarded := &atomic.Int64{}
	inner := &CloudHandler{level: slog.LevelInfo, logHook: func(gcplog.Entry) { forwarded.Add(1) }}
	g := &circuitGatedHandler{inner: inner, circuitOpen: open.Load, stats: stats}
	ctx := context.Background()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "req", 0)

	if err := g.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	open.Store(true)
	for _, h := range []slog.Handler{g, g.WithAttrs([]slog.Attr{slog.String("a", "b")}), g.WithGroup("grp")} {
		if err := h.Handle(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	open.Store(false)
	if err := g.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	if forwarded.Load() != 2 {
		t.Fatalf("forwarded = %d, want 2", forwarded.Load())
	}
	if got := cloudCounts(stats); got["circuit_open"] != 3 || got["error"]+got["queue_full"]+got["flush_error"] != 0 {
		t.Fatalf("counts = %v", got)
	}
}

// metricKinds returns name -> data kind for every exported metric.
func metricKinds(t *testing.T, reader *sdkmetric.ManualReader) map[string]string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				if d.IsMonotonic && d.Temporality == metricdata.CumulativeTemporality {
					out[m.Name] = "cumulative_monotonic_sum_int64"
				} else {
					out[m.Name] = fmt.Sprintf("sum_int64 monotonic=%v temporality=%v", d.IsMonotonic, d.Temporality)
				}
			case metricdata.Gauge[int64]:
				out[m.Name] = "gauge_int64"
			default:
				out[m.Name] = fmt.Sprintf("%T", m.Data)
			}
		}
	}
	return out
}

// sumByAttrs flattens one reader's points to "metric|writer|reason" -> value.
func sumByAttrs(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for name, pts := range collectPoints(t, reader) {
		for _, p := range pts {
			out[name+"|"+p.attrs["writer"]+"|"+p.attrs["reason"]] = p.value
		}
	}
	return out
}

// R6-Q3 evidence: counts recorded before WriteMetrics is attached and
// counts recorded after it are exported as the exact cumulative atomic
// totals, for both writers (audit and cloud), identically to two readers on
// one provider, with no double count on repeated collection. Names,
// attribute sets and kinds are pinned as well.
func TestWriteMetrics_CumulativeConservationAcrossAttachAndTwoReaders(t *testing.T) {
	ctx := context.Background()
	// Audit writer with test-fired write-budget timers and a gated inner
	// handler, so the test decides when a write times out and when it
	// returns (a late return), without sleeps.
	timers := &manualTimers{}
	w, err := NewAsyncWriter(asyncwrite.Config{Name: "audit", AfterFunc: timers.after})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	c := newCaptureInner(gate)
	h := NewAsyncHandler(c.handler(), w)
	handle := func() {
		t.Helper()
		if err := h.Handle(ctx, rec("scion.audit")); err != nil {
			t.Fatal(err)
		}
	}
	// lateReturn makes the write now inside the handler time out, then
	// return: one timeout plus one late return.
	lateReturn := func() {
		t.Helper()
		timers.fireLast()
		gate <- struct{}{}
		<-c.out
	}

	// Before any MeterProvider exists: record 1 written, record 2 timed out
	// and returned late. Record 3 entering the handler proves the single
	// worker finished accounting record 2.
	handle()
	<-c.entered
	gate <- struct{}{}
	<-c.out
	handle()
	<-c.entered
	lateReturn()
	handle()
	<-c.entered
	if s := w.Snapshot(); s.Written != 1 || s.WriteTimeouts != 1 || s.LateReturns != 1 {
		t.Fatalf("pre-attach audit snapshot = %+v", s)
	}

	cloud := newCloudWriteStats(nil)
	cloud.RecordFailure(CloudReasonError)
	cloud.RecordFailure(CloudReasonCircuitOpen)
	cloud.RecordFailure(CloudReasonCircuitOpen)
	var open atomic.Bool
	open.Store(true)
	cloud.RegisterCircuitSource(open.Load)

	// Attach with two readers on one provider.
	r1, r2 := sdkmetric.NewManualReader(), sdkmetric.NewManualReader()
	wm := newManualWriteMetrics(t, r1, r2)
	wm.Observe(w)
	wm.ObserveCloud(cloud)

	// First collection on both readers: every pre-attach count is exported.
	for i, reader := range []*sdkmetric.ManualReader{r1, r2} {
		got := sumByAttrs(t, reader)
		for k, v := range map[string]int64{
			MetricWriteRecords + "|audit|":              1,
			MetricWriteFailures + "|audit|timeout":      1,
			MetricWriteLateReturns + "|audit|":          1,
			MetricWriteFailures + "|cloud|error":        1,
			MetricWriteFailures + "|cloud|circuit_open": 2,
		} {
			if got[k] != v {
				t.Fatalf("pre-attach export, reader %d: %s = %d, want %d (all %v)", i+1, k, got[k], v, got)
			}
		}
	}

	// More counts after attach: record 3 also times out and returns late;
	// Close drains the (now empty) queue cooperatively; three closed
	// rejections follow. Close's drain timer is captured, never fired.
	lateReturn()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = h.Handle(ctx, rec("scion.audit"))
	}
	cloud.RecordFailure(CloudReasonQueueFull)
	cloud.RecordFailure(CloudReasonFlushError)
	cloud.RecordFailure(CloudReasonCircuitOpen)

	snap := w.Snapshot()
	if snap.Written != 1 || snap.WriteTimeouts != 2 || snap.LateReturns != 2 || snap.DroppedClosed != 3 {
		t.Fatalf("audit snapshot = %+v", snap)
	}
	want := map[string]int64{
		MetricWriteRecords + "|audit|":              int64(snap.Written),
		MetricWriteLateReturns + "|audit|":          int64(snap.LateReturns),
		MetricWriteFailures + "|audit|timeout":      int64(snap.WriteTimeouts),
		MetricWriteFailures + "|audit|closed":       int64(snap.DroppedClosed),
		MetricQueueDepth + "|audit|":                0,
		MetricWriterStalled + "|audit|":             0,
		MetricWriteFailures + "|cloud|error":        int64(cloud.Failures(CloudReasonError)),
		MetricWriteFailures + "|cloud|queue_full":   int64(cloud.Failures(CloudReasonQueueFull)),
		MetricWriteFailures + "|cloud|circuit_open": int64(cloud.Failures(CloudReasonCircuitOpen)),
		MetricWriteFailures + "|cloud|flush_error":  int64(cloud.Failures(CloudReasonFlushError)),
		MetricWriterCircuitOpen + "|cloud|":         1,
	}
	if want[MetricWriteFailures+"|cloud|circuit_open"] != 3 || want[MetricWriteLateReturns+"|audit|"] != 2 {
		t.Fatalf("atomics: cloud circuit_open %d, audit late returns %d",
			cloud.Failures(CloudReasonCircuitOpen), snap.LateReturns)
	}
	for round := 1; round <= 2; round++ {
		for i, reader := range []*sdkmetric.ManualReader{r1, r2} {
			got := sumByAttrs(t, reader)
			if len(got) != len(want) {
				t.Fatalf("round %d reader %d: series %v, want %v", round, i+1, got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("round %d reader %d: %s = %d, want %d (all %v)", round, i+1, k, got[k], v, got)
				}
			}
		}
	}

	// Attribute sets: failures {reason, writer}; everything else {writer}.
	for name, pts := range collectPoints(t, r1) {
		for _, p := range pts {
			keys := attrKeys(p)
			if name == MetricWriteFailures {
				if len(keys) != 2 || keys[0] != "reason" || keys[1] != "writer" {
					t.Fatalf("%s attrs = %v", name, keys)
				}
			} else if len(keys) != 1 || keys[0] != "writer" {
				t.Fatalf("%s attrs = %v", name, keys)
			}
		}
	}

	// Kinds: counters export as cumulative monotonic int64 sums (Cloud
	// Monitoring CUMULATIVE INT64, as before); gauges as int64 gauges.
	open.Store(false)
	kinds := metricKinds(t, r2)
	for name, want := range map[string]string{
		MetricWriteFailures:     "cumulative_monotonic_sum_int64",
		MetricWriteRecords:      "cumulative_monotonic_sum_int64",
		MetricWriteLateReturns:  "cumulative_monotonic_sum_int64",
		MetricQueueDepth:        "gauge_int64",
		MetricWriterStalled:     "gauge_int64",
		MetricWriterCircuitOpen: "gauge_int64",
	} {
		if kinds[name] != want {
			t.Errorf("%s kind = %q, want %q", name, kinds[name], want)
		}
	}
	if f, c := cloudPoints(t, r1); len(c) != 1 || c[0] != 0 || f["circuit_open"] != 3 {
		t.Fatalf("after circuit closed: failures %v circuit %v", f, c)
	}
}

// The production mapping from each asyncwrite.Result to its exported
// series: written -> write.records, late_return -> write.late_returns,
// every other result -> write.failures{reason=<result>}. Each Snapshot
// counter is set alone and the exported series is read back, so the test
// checks asyncFailureReasons and the callback, not a copy of them.
func TestWriteMetrics_AsyncResultMapping(t *testing.T) {
	set := map[asyncwrite.Result]func(*asyncwrite.Snapshot){
		asyncwrite.ResultWritten:     func(s *asyncwrite.Snapshot) { s.Written = 1 },
		asyncwrite.ResultLateReturn:  func(s *asyncwrite.Snapshot) { s.LateReturns = 1 },
		asyncwrite.ResultError:       func(s *asyncwrite.Snapshot) { s.WriteErrors = 1 },
		asyncwrite.ResultTimeout:     func(s *asyncwrite.Snapshot) { s.WriteTimeouts = 1 },
		asyncwrite.ResultShutdown:    func(s *asyncwrite.Snapshot) { s.DroppedShutdown = 1 },
		asyncwrite.ResultQueueFull:   func(s *asyncwrite.Snapshot) { s.DroppedFull = 1 },
		asyncwrite.ResultOversize:    func(s *asyncwrite.Snapshot) { s.DroppedOversize = 1 },
		asyncwrite.ResultUnsupported: func(s *asyncwrite.Snapshot) { s.DroppedUnsupported = 1 },
		asyncwrite.ResultClosed:      func(s *asyncwrite.Snapshot) { s.DroppedClosed = 1 },
	}
	if len(set) != int(asyncwrite.ResultLateReturn) {
		t.Fatalf("mapping test covers %d results, want all %d", len(set), asyncwrite.ResultLateReturn)
	}
	failures := 0
	for r, apply := range set {
		var snap asyncwrite.Snapshot
		apply(&snap)
		reader := sdkmetric.NewManualReader()
		wm := newManualWriteMetrics(t, reader)
		wm.Observe(&fakeWriterSource{name: "audit", snap: snap})
		got := sumByAttrs(t, reader)
		delete(got, MetricQueueDepth+"|audit|")
		delete(got, MetricWriterStalled+"|audit|")
		var want string
		switch r {
		case asyncwrite.ResultWritten:
			want = MetricWriteRecords + "|audit|"
		case asyncwrite.ResultLateReturn:
			want = MetricWriteLateReturns + "|audit|"
		default:
			want = MetricWriteFailures + "|audit|" + r.String()
			failures++
		}
		if len(got) != 1 || got[want] != 1 {
			t.Errorf("%s: exported %v, want only %s = 1", r, got, want)
		}
	}
	if failures != len(asyncFailureReasons) || failures != 7 {
		t.Fatalf("failure results = %d, production mapping has %d, want 7", failures, len(asyncFailureReasons))
	}
}

// Late returns keep their cumulative-counter kind too.
func TestWriteMetrics_LateReturnsKind(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	wm := newManualWriteMetrics(t, reader)
	wm.Observe(&fakeWriterSource{name: "audit", snap: asyncwrite.Snapshot{LateReturns: 2}})
	if k := metricKinds(t, reader)[MetricWriteLateReturns]; k != "cumulative_monotonic_sum_int64" {
		t.Fatalf("late returns kind = %q", k)
	}
	if got := sumByAttrs(t, reader)[MetricWriteLateReturns+"|audit|"]; got != 2 {
		t.Fatalf("late returns = %d", got)
	}
}

// Without a circuit source and with zero counts, writer=cloud exports
// nothing (no Cloud handler configured).
func TestWriteMetrics_UnconfiguredCloudExportsNothing(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	wm := newManualWriteMetrics(t, reader)
	wm.ObserveCloud(newCloudWriteStats(nil))
	if f, c := cloudPoints(t, reader); len(f) != 0 || len(c) != 0 {
		t.Fatalf("unconfigured cloud exported failures %v circuit %v", f, c)
	}
}
