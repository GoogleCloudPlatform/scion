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

package asyncwrite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// All synchronization in these tests is channel based: no sleeps and no
// wall-clock assertions. A test that would block forever fails via the
// package test timeout.

const (
	testBudget = 2 * time.Second
	testDrain  = 5 * time.Second
)

// fakeTimers hands every scheduled callback to the test instead of running
// it. Write-budget timers and drain-deadline timers are separated by their
// duration.
type fakeTimers struct {
	writes chan *fakeTimer
	drains chan *fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped atomic.Bool
	fired   atomic.Bool
}

func (t *fakeTimer) Stop() bool { return !t.stopped.Swap(true) && !t.fired.Load() }

// fire runs the callback on the calling goroutine, as the runtime would on
// a timer goroutine, even if Stop was already called (modelling a callback
// that had started before Stop).
func (t *fakeTimer) fire() {
	t.fired.Store(true)
	t.f()
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{writes: make(chan *fakeTimer, 64), drains: make(chan *fakeTimer, 8)}
}

func (ft *fakeTimers) afterFunc(d time.Duration, f func()) Timer {
	t := &fakeTimer{d: d, f: f}
	if d == testDrain {
		ft.drains <- t
	} else {
		ft.writes <- t
	}
	return t
}

// fakeClock is a settable clock.
type fakeClock struct{ n atomic.Int64 }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.set(t)
	return c
}
func (c *fakeClock) set(t time.Time) { c.n.Store(t.UnixNano()) }
func (c *fakeClock) now() time.Time  { return time.Unix(0, c.n.Load()) }

// gate is a controllable write function: each write announces itself on
// entered and waits for a release value (nil = success).
type gate struct {
	entered chan int
	release chan error
	mu      sync.Mutex
	got     []int
}

func newGate() *gate {
	return &gate{entered: make(chan int, 64), release: make(chan error)}
}

func (g *gate) write(v int) error {
	g.entered <- v
	err := <-g.release
	if err == nil {
		g.mu.Lock()
		g.got = append(g.got, v)
		g.mu.Unlock()
	}
	return err
}

func (g *gate) written() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.got...)
}

type recorderSpy struct {
	mu     sync.Mutex
	counts map[Result]int
	names  map[string]bool
}

func newRecorderSpy() *recorderSpy {
	return &recorderSpy{counts: map[Result]int{}, names: map[string]bool{}}
}

func (r *recorderSpy) Record(writer string, res Result) {
	r.mu.Lock()
	r.counts[res]++
	r.names[writer] = true
	r.mu.Unlock()
}

func (r *recorderSpy) get(res Result) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[res]
}

type harness struct {
	w      *Writer[int]
	g      *gate
	timers *fakeTimers
	clock  *fakeClock
	wrote  chan struct{} // afterWrite hook
}

func newHarness(t *testing.T, cfg Config, hooks testHooks) *harness {
	t.Helper()
	h := &harness{
		g:      newGate(),
		timers: newFakeTimers(),
		clock:  newFakeClock(time.Unix(1_700_000_000, 0)),
		wrote:  make(chan struct{}, 64),
	}
	if cfg.Name == "" {
		cfg.Name = "test"
	}
	cfg.WriteBudget = testBudget
	cfg.DrainTimeout = testDrain
	cfg.Now = h.clock.now
	cfg.AfterFunc = h.timers.afterFunc
	if hooks.afterWrite == nil {
		hooks.afterWrite = func() { h.wrote <- struct{}{} }
	}
	w, err := newWriter(cfg, h.g.write, hooks)
	if err != nil {
		t.Fatalf("newWriter: %v", err)
	}
	h.w = w
	return h
}

// finish releases everything and closes the writer cooperatively.
func (h *harness) closeWhenIdle(t *testing.T) {
	t.Helper()
	if err := h.w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-h.w.done
}

func assertConservation(t *testing.T, s Snapshot) {
	t.Helper()
	rhs := s.Written + s.WriteErrors + s.WriteTimeouts + s.DroppedShutdown +
		uint64(s.Queued) + uint64(s.InflightUnresolved)
	if s.Enqueued != rhs {
		t.Fatalf("conservation violated: enqueued=%d != written=%d + error=%d + timeout=%d + shutdown=%d + queued=%d + inflight=%d",
			s.Enqueued, s.Written, s.WriteErrors, s.WriteTimeouts, s.DroppedShutdown, s.Queued, s.InflightUnresolved)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	ok := func(int) error { return nil }
	if _, err := New(Config{Name: "x"}, (func(int) error)(nil)); err == nil {
		t.Fatal("nil write accepted")
	}
	if _, err := New[int](Config{}, ok); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := New[int](Config{Name: "x", Capacity: -1}, ok); err == nil {
		t.Fatal("negative capacity accepted")
	}
	w, err := New[int](Config{Name: "x"}, ok)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if w.cfg.Capacity != DefaultCapacity || w.cfg.MaxQueuedBytes != DefaultMaxQueuedBytes ||
		w.cfg.WriteBudget != DefaultWriteBudget || w.cfg.DrainTimeout != DefaultDrainTimeout {
		t.Fatalf("unexpected defaults: %+v", w.cfg)
	}
	if DefaultCapacity != 2048 || DefaultMaxQueuedBytes != 2*1024*1024 {
		t.Fatal("design parameters changed")
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// P1-1: enqueue never blocks while the worker is stuck; drop counts exact.
func TestTryEnqueueNeverBlocksWhenFull(t *testing.T) {
	const capacity = 4
	h := newHarness(t, Config{Capacity: capacity}, testHooks{})
	if err := h.w.TryEnqueue(0, 1); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	<-h.g.entered // item 0 is in flight; the worker is blocked
	<-h.timers.writes

	// Capacity+1 attempts from the test goroutine. If any blocked, the test
	// would hang and hit the test timeout.
	var errs []error
	for i := 1; i <= capacity+1; i++ {
		errs = append(errs, h.w.TryEnqueue(i, 1))
	}
	for i := 0; i < capacity; i++ {
		if errs[i] != nil {
			t.Fatalf("enqueue %d: %v", i+1, errs[i])
		}
	}
	if !errors.Is(errs[capacity], ErrFull) {
		t.Fatalf("overflow enqueue = %v, want ErrFull", errs[capacity])
	}
	s := h.w.Snapshot()
	if s.Enqueued != capacity+1 || s.DroppedFull != 1 || s.Queued != capacity || s.InflightUnresolved != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
	assertConservation(t, s)

	// Drain in order.
	h.g.release <- nil
	<-h.wrote
	for i := 1; i <= capacity; i++ {
		if v := <-h.g.entered; v != i {
			t.Fatalf("order: got %d want %d", v, i)
		}
		<-h.timers.writes
		h.g.release <- nil
		<-h.wrote
	}
	if got := h.g.written(); len(got) != capacity+1 {
		t.Fatalf("written = %v", got)
	}
	h.closeWhenIdle(t)
	s = h.w.Snapshot()
	if s.Written != capacity+1 || s.QueuedBytes != 0 {
		t.Fatalf("final snapshot = %+v", s)
	}
	assertConservation(t, s)
}

func TestByteBoundRejectsAndReturnsToZero(t *testing.T) {
	h := newHarness(t, Config{Capacity: 16, MaxQueuedBytes: 100}, testHooks{})
	if err := h.w.TryEnqueue(0, 10); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	<-h.timers.writes
	if s := h.w.Snapshot(); s.QueuedBytes != 0 {
		t.Fatalf("in-flight bytes still charged: %d", s.QueuedBytes)
	}
	if err := h.w.TryEnqueue(1, 60); err != nil {
		t.Fatal(err)
	}
	if err := h.w.TryEnqueue(2, 40); err != nil {
		t.Fatal(err)
	}
	if err := h.w.TryEnqueue(3, 1); !errors.Is(err, ErrFull) {
		t.Fatalf("byte overflow = %v, want ErrFull", err)
	}
	if err := h.w.TryEnqueue(4, 101); !errors.Is(err, ErrFull) {
		t.Fatalf("single oversized = %v, want ErrFull", err)
	}
	if s := h.w.Snapshot(); s.QueuedBytes != 100 || s.DroppedFull != 2 {
		t.Fatalf("snapshot = %+v", s)
	}
	for i := 0; i < 3; i++ {
		if i > 0 {
			<-h.g.entered
			<-h.timers.writes
		}
		h.g.release <- nil
		<-h.wrote
	}
	h.closeWhenIdle(t)
	s := h.w.Snapshot()
	if s.QueuedBytes != 0 || s.Written != 3 {
		t.Fatalf("snapshot = %+v", s)
	}
	assertConservation(t, s)
}

func TestWriteErrorAndPanicAreCountedAndWorkerContinues(t *testing.T) {
	timers := newFakeTimers()
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	wrote := make(chan struct{}, 8)
	var calls atomic.Int32
	w, err := newWriter(Config{Name: "p", WriteBudget: testBudget, DrainTimeout: testDrain,
		Now: clock.now, AfterFunc: timers.afterFunc},
		func(v int) error {
			calls.Add(1)
			switch v {
			case 0:
				return errors.New("boom")
			case 1:
				panic("kaboom")
			}
			return nil
		}, testHooks{afterWrite: func() { wrote <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	spy := newRecorderSpy()
	w.SetRecorder(spy)
	for i := 0; i < 3; i++ {
		if err := w.TryEnqueue(i, 1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		<-wrote
	}
	s := w.Snapshot()
	if s.WriteErrors != 2 || s.Written != 1 || calls.Load() != 3 {
		t.Fatalf("snapshot = %+v calls=%d", s, calls.Load())
	}
	if !s.LastFailure.Equal(clock.now()) {
		t.Fatalf("LastFailure = %v, want %v", s.LastFailure, clock.now())
	}
	if spy.get(ResultError) != 2 || spy.get(ResultWritten) != 1 || !spy.names["p"] {
		t.Fatalf("recorder = %+v", spy.counts)
	}
	if hl := w.Health(clock.now()); hl.Healthy || hl.Reason != HealthRecentFailures {
		t.Fatalf("health = %+v", hl)
	}
	if hl := w.Health(clock.now().Add(DefaultFailureWindow)); !hl.Healthy {
		t.Fatalf("health after window = %+v", hl)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-w.done
	assertConservation(t, w.Snapshot())
}

// P1-1 timeout + stalled: an injected expiry claims the write; the late
// return records no second outcome.
func TestTimeoutClaimsWriteAndLateReturnIsNotDoubleCounted(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	spy := newRecorderSpy()
	h.w.SetRecorder(spy)
	if err := h.w.TryEnqueue(7, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	tm := <-h.timers.writes
	if tm.d != testBudget {
		t.Fatalf("timer duration = %v", tm.d)
	}
	if h.w.Stalled() {
		t.Fatal("stalled before expiry")
	}
	if hl := h.w.Health(h.clock.now()); !hl.Healthy {
		t.Fatalf("health = %+v", hl)
	}
	tm.fire()
	s := h.w.Snapshot()
	if s.WriteTimeouts != 1 || !s.Stalled || !h.w.Stalled() || s.InflightUnresolved != 0 {
		t.Fatalf("after expiry: %+v", s)
	}
	if hl := h.w.Health(h.clock.now()); hl.Reason != HealthStalled {
		t.Fatalf("health = %+v", hl)
	}
	assertConservation(t, s)

	h.g.release <- nil
	<-h.wrote
	s = h.w.Snapshot()
	if s.WriteTimeouts != 1 || s.Written != 0 || s.LateReturns != 1 || s.Stalled {
		t.Fatalf("after late return: %+v", s)
	}
	if spy.get(ResultLateReturn) != 1 || spy.get(ResultTimeout) != 1 || spy.get(ResultWritten) != 0 {
		t.Fatalf("recorder = %+v", spy.counts)
	}
	if !tm.stopped.Load() {
		t.Fatal("worker did not Stop the write timer")
	}
	if hl := h.w.Health(h.clock.now()); hl.Reason != HealthRecentFailures {
		t.Fatalf("health = %+v", hl)
	}
	h.closeWhenIdle(t)
	assertConservation(t, h.w.Snapshot())
}

// C2.2 required deterministic test: a stale callback cannot touch a later
// ticket.
func TestStaleTimerCallbackCannotAffectLaterWrite(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	if err := h.w.TryEnqueue(1, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	cb1 := <-h.timers.writes // held, not invoked
	h.g.release <- nil       // write 1 completes
	<-h.wrote

	if err := h.w.TryEnqueue(2, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered // write 2 blocks in the handler
	cb2 := <-h.timers.writes
	tk2 := h.w.current.Load()
	if tk2 == nil || tk2.gen != 2 {
		t.Fatalf("current ticket = %+v", tk2)
	}

	cb1.fire()
	s := h.w.Snapshot()
	if st := tk2.state.Load(); st != stateInflight {
		t.Fatalf("ticket 2 state = %d after stale callback", st)
	}
	if s.WriteTimeouts != 0 || s.Stalled || s.Written != 1 {
		t.Fatalf("after stale callback: %+v", s)
	}
	assertConservation(t, s)

	cb2.fire()
	s = h.w.Snapshot()
	if s.WriteTimeouts != 1 || !s.Stalled {
		t.Fatalf("after callback 2: %+v", s)
	}
	assertConservation(t, s)

	h.g.release <- nil
	<-h.wrote
	s = h.w.Snapshot()
	if s.Written != 1 || s.WriteTimeouts != 1 || s.LateReturns != 1 || s.Stalled {
		t.Fatalf("after release: %+v", s)
	}
	h.closeWhenIdle(t)
	assertConservation(t, h.w.Snapshot())
}

// C2.2 companion: the callback races the worker's completion; exactly one
// terminal outcome is recorded whichever wins. Meaningful under -race.
func TestTimerCallbackRacingCompletionYieldsOneOutcome(t *testing.T) {
	for i := 0; i < 50; i++ {
		h := newHarness(t, Config{}, testHooks{})
		if err := h.w.TryEnqueue(1, 1); err != nil {
			t.Fatal(err)
		}
		<-h.g.entered
		cb := <-h.timers.writes
		start := make(chan struct{})
		fired := make(chan struct{})
		go func() {
			<-start
			cb.fire()
			close(fired)
		}()
		close(start)
		h.g.release <- nil
		<-fired
		<-h.wrote
		s := h.w.Snapshot()
		if s.Written+s.WriteTimeouts != 1 {
			t.Fatalf("iteration %d: terminal outcomes = %+v", i, s)
		}
		if s.WriteTimeouts == 1 && s.LateReturns != 1 {
			t.Fatalf("iteration %d: timeout without late return: %+v", i, s)
		}
		if s.Written == 1 && s.LateReturns != 0 {
			t.Fatalf("iteration %d: written with late return: %+v", i, s)
		}
		h.closeWhenIdle(t)
		assertConservation(t, h.w.Snapshot())
	}
}

// C3.3 deterministic test: a claimed-but-unfinished expiry for write 1 never
// marks write 2 stalled; LastFailure is the sampled time and monotonic.
func TestDelayedClaimDoesNotStallLaterWriteAndLastFailureIsMonotonic(t *testing.T) {
	claimEntered := make(chan struct{}, 4)
	claimRelease := make(chan struct{}, 4)
	var hookOn atomic.Bool
	hooks := testHooks{afterClaim: func() {
		if hookOn.Load() {
			claimEntered <- struct{}{}
			<-claimRelease
		}
	}}
	h := newHarness(t, Config{}, hooks)
	t0 := time.Unix(1_800_000_000, 0)
	h.clock.set(t0)

	// 2. write 1 starts and blocks; callback 1 claims, then parks.
	if err := h.w.TryEnqueue(1, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	cb1 := <-h.timers.writes
	hookOn.Store(true)
	cbDone := make(chan struct{})
	go func() { cb1.fire(); close(cbDone) }()
	<-claimEntered
	hookOn.Store(false)

	// 3. write 1's handler returns: the worker's CAS loses.
	h.g.release <- nil
	<-h.wrote
	s := h.w.Snapshot()
	if s.LateReturns != 1 || h.w.current.Load() != nil || s.Written != 0 {
		t.Fatalf("after write 1 return: %+v current=%v", s, h.w.current.Load())
	}

	// 4. write 2 starts and blocks.
	if err := h.w.TryEnqueue(2, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	<-h.timers.writes
	if h.w.Stalled() {
		t.Fatal("stalled while current is write 2 (inflight)")
	}

	// 5. release the parked claim; the clock moved meanwhile.
	tClaim := t0.Add(time.Second)
	h.clock.set(tClaim)
	claimRelease <- struct{}{}
	<-cbDone
	s = h.w.Snapshot()
	if s.WriteTimeouts != 1 {
		t.Fatalf("timeouts = %d", s.WriteTimeouts)
	}
	if !s.LastFailure.Equal(tClaim) {
		t.Fatalf("LastFailure = %v, want sampled %v", s.LastFailure, tClaim)
	}
	if h.w.Stalled() || s.Stalled {
		t.Fatal("stale claim made the writer stalled")
	}
	tk2 := h.w.current.Load()
	if tk2 == nil || tk2.state.Load() != stateInflight || s.InflightUnresolved != 1 {
		t.Fatalf("write 2 ticket not inflight: %+v", tk2)
	}

	// 6. a later claim with the clock moved backwards does not decrease
	// LastFailure.
	later := tClaim.Add(time.Hour)
	h.w.noteFailure(later)
	h.clock.set(tClaim.Add(-time.Hour))
	h.w.noteFailure(h.clock.now())
	if got := h.w.Snapshot().LastFailure; !got.Equal(later) {
		t.Fatalf("LastFailure decreased: %v, want %v", got, later)
	}
	// Same through a real claim: a backwards-clock timeout on a fresh ticket.
	tkX := &ticket{gen: 99}
	h.w.expire(tkX)
	if got := h.w.Snapshot().LastFailure; !got.Equal(later) {
		t.Fatalf("LastFailure decreased via claim: %v, want %v", got, later)
	}
	// Undo the synthetic ticket's counter for the conservation check below.
	h.w.stats.WriteTimeouts.Add(^uint64(0))

	// 7. complete write 2; conservation 2 = 1 written + 1 timeout.
	h.g.release <- nil
	<-h.wrote
	s = h.w.Snapshot()
	if s.Enqueued != 2 || s.Written != 1 || s.WriteTimeouts != 1 || s.Queued != 0 || s.InflightUnresolved != 0 {
		t.Fatalf("final: %+v", s)
	}
	assertConservation(t, s)
	h.closeWhenIdle(t)
}

// P1-1 Close (cooperative): drains, done closes, all items accounted.
func TestCloseCooperativeDrains(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	for i := 0; i < 3; i++ {
		if err := h.w.TryEnqueue(i, 5); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan error, 1)
	<-h.g.entered
	<-h.timers.writes
	go func() { closed <- h.w.Close(context.Background()) }()
	// Close sets closed before waiting; observe it, then later enqueues fail.
	<-h.timers.drains
	if err := h.w.TryEnqueue(9, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("enqueue after close = %v", err)
	}
	h.g.release <- nil
	for i := 1; i < 3; i++ {
		<-h.g.entered
		<-h.timers.writes
		h.g.release <- nil
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}
	<-h.w.done
	s := h.w.Snapshot()
	if s.Written != 3 || s.DroppedClosed != 1 || s.Enqueued != 3 || s.QueuedBytes != 0 || !s.Closed {
		t.Fatalf("snapshot = %+v", s)
	}
	assertConservation(t, s)
	if hl := h.w.Health(h.clock.now()); hl.Reason != HealthClosed {
		t.Fatalf("health = %+v", hl)
	}
	// Idempotent.
	if err := h.w.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// P1-1 Close (noncooperative): the drain deadline fires while the worker is
// blocked; queued items are shutdown; the in-flight item gets no outcome
// from Close and is not double-counted later.
func TestCloseNoncooperativeReturnsWorkerStuck(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	spy := newRecorderSpy()
	h.w.SetRecorder(spy)
	for i := 0; i < 4; i++ {
		if err := h.w.TryEnqueue(i, 3); err != nil {
			t.Fatal(err)
		}
	}
	<-h.g.entered
	wt := <-h.timers.writes
	closed := make(chan error, 1)
	go func() { closed <- h.w.Close(context.Background()) }()
	dl := <-h.timers.drains
	if dl.d != testDrain {
		t.Fatalf("drain timer = %v", dl.d)
	}
	dl.fire()
	if err := <-closed; !errors.Is(err, ErrWorkerStuck) {
		t.Fatalf("Close = %v, want ErrWorkerStuck", err)
	}
	if wt.stopped.Load() {
		t.Fatal("Close stopped the in-flight write timer")
	}
	s := h.w.Snapshot()
	if s.DroppedShutdown != 3 || s.Queued != 0 || s.QueuedBytes != 0 || s.InflightUnresolved != 1 || s.WriteTimeouts != 0 {
		t.Fatalf("after Close: %+v", s)
	}
	if spy.get(ResultShutdown) != 3 {
		t.Fatalf("recorder = %+v", spy.counts)
	}
	assertConservation(t, s)

	// Release the handler: the worker records exactly one outcome and exits.
	h.g.release <- nil
	<-h.w.done
	s = h.w.Snapshot()
	if s.Written != 1 || s.DroppedShutdown != 3 || s.InflightUnresolved != 0 {
		t.Fatalf("after release: %+v", s)
	}
	assertConservation(t, s)
}

// Close (noncooperative) where the budget expires before the drain deadline:
// the timer's claim stands and the late return is not counted again.
func TestCloseAfterTimeoutClaimConservation(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	if err := h.w.TryEnqueue(0, 1); err != nil {
		t.Fatal(err)
	}
	if err := h.w.TryEnqueue(1, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	(<-h.timers.writes).fire()
	closed := make(chan error, 1)
	go func() { closed <- h.w.Close(context.Background()) }()
	(<-h.timers.drains).fire()
	if err := <-closed; !errors.Is(err, ErrWorkerStuck) {
		t.Fatalf("Close = %v", err)
	}
	s := h.w.Snapshot()
	if s.WriteTimeouts != 1 || s.DroppedShutdown != 1 || s.InflightUnresolved != 0 || !s.Stalled {
		t.Fatalf("after Close: %+v", s)
	}
	assertConservation(t, s)
	h.g.release <- nil
	<-h.w.done
	s = h.w.Snapshot()
	if s.Written != 0 || s.LateReturns != 1 || s.Stalled {
		t.Fatalf("after release: %+v", s)
	}
	assertConservation(t, s)
}

// Close bounded by a caller context earlier than DrainTimeout.
func TestCloseHonorsContext(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	if err := h.w.TryEnqueue(0, 1); err != nil {
		t.Fatal(err)
	}
	<-h.g.entered
	<-h.timers.writes
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.w.Close(ctx); !errors.Is(err, ErrWorkerStuck) {
		t.Fatalf("Close = %v", err)
	}
	<-h.timers.drains
	h.g.release <- nil
	<-h.w.done
	assertConservation(t, h.w.Snapshot())
}

func TestEnqueueAfterCloseCountsClosed(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	h.closeWhenIdle(t)
	for i := 0; i < 3; i++ {
		if err := h.w.TryEnqueue(i, 1); !errors.Is(err, ErrClosed) {
			t.Fatalf("enqueue = %v", err)
		}
	}
	s := h.w.Snapshot()
	if s.DroppedClosed != 3 || s.Enqueued != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	assertConservation(t, s)
}

func TestRejectCountsProducerRejectionsOnly(t *testing.T) {
	h := newHarness(t, Config{}, testHooks{})
	spy := newRecorderSpy()
	h.w.SetRecorder(spy)
	h.w.Reject(ResultOversize)
	h.w.Reject(ResultUnsupported)
	h.w.Reject(ResultWritten) // ignored
	h.w.Reject(ResultShutdown)
	s := h.w.Snapshot()
	if s.DroppedOversize != 1 || s.DroppedUnsupported != 1 || s.Written != 0 || s.DroppedShutdown != 0 || s.Enqueued != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if spy.get(ResultOversize) != 1 || spy.get(ResultUnsupported) != 1 {
		t.Fatalf("recorder = %+v", spy.counts)
	}
	h.closeWhenIdle(t)
}

func TestResultLabelsAreClosedSet(t *testing.T) {
	want := map[Result]string{
		ResultWritten: "written", ResultError: "error", ResultTimeout: "timeout",
		ResultShutdown: "shutdown", ResultQueueFull: "queue_full", ResultOversize: "oversize",
		ResultUnsupported: "unsupported", ResultClosed: "closed", ResultLateReturn: "late_return",
	}
	failures := 0
	for r, s := range want {
		if r.String() != s {
			t.Errorf("%d.String() = %q, want %q", r, r.String(), s)
		}
		if r.IsFailure() {
			failures++
		}
	}
	if failures != 7 {
		t.Fatalf("failure reasons = %d, want 7", failures)
	}
	if ResultWritten.IsFailure() || ResultLateReturn.IsFailure() {
		t.Fatal("non-failures marked as failures")
	}
	for _, r := range []Result{ResultError, ResultTimeout, ResultShutdown} {
		if r.IsRejection() {
			t.Fatalf("%v is terminal, not a rejection", r)
		}
	}
}

// Real timers, cooperative writer: the worker goroutine exits on Close.
func TestRealTimersCooperativeClose(t *testing.T) {
	var n atomic.Int32
	w, err := New(Config{Name: "real"}, func(int) error { n.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := w.TryEnqueue(i, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-w.done
	s := w.Snapshot()
	if s.Written != 10 || n.Load() != 10 {
		t.Fatalf("snapshot = %+v", s)
	}
	assertConservation(t, s)
}
