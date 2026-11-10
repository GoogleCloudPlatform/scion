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
	"context"
	"errors"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gcplog "cloud.google.com/go/logging"
)

// healthBase is on a slot boundary (1.8e9 s is a multiple of 30 s), so
// window arithmetic in these tests is exact.
var healthBase = time.Unix(1_800_000_000, 0)

// recordN records n failures for reason r.
func recordN(s *CloudWriteStats, r CloudFailureReason, n int) {
	for i := 0; i < n; i++ {
		s.RecordFailure(r)
	}
}

// writeN records n writes.
func writeN(s *CloudWriteStats, n int) {
	for i := 0; i < n; i++ {
		s.RecordWrite()
	}
}

// The health decision, all at one instant: single failures stay healthy;
// the ratio judges error plus queue_full against the minimum sample; a run
// of failed flushes degrades; precedence is circuit, flushes, ratio.
func TestCloudWriteStats_HealthDecision(t *testing.T) {
	tests := []struct {
		name    string
		writes  int
		errs    int
		full    int
		dropped int    // circuit_open drops
		flushes string // in order: F failed flush, . successful flush
		circuit bool
		want    CloudHealthReason
	}{
		{name: "no activity", want: CloudHealthOK},
		{name: "single error among few writes", writes: 10, errs: 1, want: CloudHealthOK},
		{name: "single error with no writes", errs: 1, want: CloudHealthOK},
		{name: "one failure of each reason", writes: 100, errs: 1, full: 1, dropped: 1, flushes: "F", want: CloudHealthOK},
		{name: "ratio exactly at 1 percent", writes: 1000, errs: 5, full: 5, want: CloudHealthOK},
		{name: "ratio just above 1 percent", writes: 1000, errs: 6, full: 5, want: CloudHealthFailureRatio},
		{name: "queue_full alone counts", writes: 1000, full: 11, want: CloudHealthFailureRatio},
		{name: "just above 1 percent on a large window", writes: 10001, errs: 101, want: CloudHealthFailureRatio},
		{name: "ratio high but below minimum sample", writes: cloudHealthMinWrites - 1, errs: 100, want: CloudHealthOK},
		{name: "ratio above at minimum sample", writes: cloudHealthMinWrites, errs: 6, want: CloudHealthFailureRatio},
		{name: "flush_error is not in the ratio", writes: 1000, flushes: "FF", want: CloudHealthOK},
		{name: "circuit_open drops are not in the ratio", writes: 1000, dropped: 50, want: CloudHealthOK},
		{name: "two consecutive flush failures", flushes: "FF", want: CloudHealthOK},
		{name: "three consecutive flush failures", flushes: "FFF", want: CloudHealthFlushFailures},
		{name: "a successful flush breaks the run", flushes: "FF.FF", want: CloudHealthOK},
		{name: "run after a success", flushes: "F.FFF", want: CloudHealthFlushFailures},
		{name: "circuit open", circuit: true, want: CloudHealthCircuitOpen},
		{name: "circuit beats flushes and ratio", writes: 1000, errs: 50, flushes: "FFF", circuit: true, want: CloudHealthCircuitOpen},
		{name: "flushes beat ratio", writes: 1000, errs: 50, flushes: "FFF", want: CloudHealthFlushFailures},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: healthBase}
			s := newCloudWriteStats(clk.Now)
			var open atomic.Bool
			open.Store(tt.circuit)
			s.RegisterCircuitSource(open.Load)
			writeN(s, tt.writes)
			recordN(s, CloudReasonError, tt.errs)
			recordN(s, CloudReasonQueueFull, tt.full)
			recordN(s, CloudReasonCircuitOpen, tt.dropped)
			for _, f := range tt.flushes {
				if f == 'F' {
					s.RecordFailure(CloudReasonFlushError)
				} else {
					s.RecordFlushSuccess()
				}
			}
			if got := s.Health(clk.Now()); got != tt.want {
				t.Fatalf("Health = %v, want %v (status %q)", got, tt.want, s.HealthStatus(clk.Now()))
			}
		})
	}
}

// The window: counts age out on the injected clock at 5 minutes, slots
// spread across the window sum, a reused slot starts from zero, and a
// flush-failure run needs a failure inside the window.
func TestCloudWriteStats_HealthWindowExpiry(t *testing.T) {
	t.Run("ratio ages out", func(t *testing.T) {
		clk := &fakeClock{now: healthBase}
		s := newCloudWriteStats(clk.Now)
		writeN(s, 1000)
		recordN(s, CloudReasonError, 20)
		if got := s.Health(healthBase.Add(cloudHealthWindow - time.Second)); got != CloudHealthFailureRatio {
			t.Fatalf("inside window = %v", got)
		}
		if got := s.Health(healthBase.Add(cloudHealthWindow)); got != CloudHealthOK {
			t.Fatalf("after window = %v", got)
		}
	})

	t.Run("slots across the window sum", func(t *testing.T) {
		clk := &fakeClock{now: healthBase}
		s := newCloudWriteStats(clk.Now)
		writeN(s, 600)
		clk.Set(healthBase.Add(2 * time.Minute))
		writeN(s, 600)
		clk.Set(healthBase.Add(4 * time.Minute))
		recordN(s, CloudReasonError, 13) // 13 of 1200: 1.08%
		if c := s.windowCounts(clk.Now()); c.writes != 1200 || c.failures[CloudReasonError] != 13 {
			t.Fatalf("counts at 4m = %+v", c)
		}
		if got := s.Health(clk.Now()); got != CloudHealthFailureRatio {
			t.Fatalf("at 4m = %v", got)
		}
		// At 7m only the 4m slot is left: 13 errors, 0 writes, below the
		// minimum sample.
		if got := s.Health(healthBase.Add(7 * time.Minute)); got != CloudHealthOK {
			t.Fatalf("at 7m = %v", got)
		}
	})

	t.Run("reused slot starts from zero", func(t *testing.T) {
		clk := &fakeClock{now: healthBase}
		s := newCloudWriteStats(clk.Now)
		writeN(s, 1000)
		recordN(s, CloudReasonQueueFull, 50)
		clk.Set(healthBase.Add(cloudHealthWindow)) // same slot index
		writeN(s, 1000)
		c := s.windowCounts(clk.Now())
		if c.writes != 1000 || c.failures[CloudReasonQueueFull] != 0 {
			t.Fatalf("reused slot counts = %+v", c)
		}
		if got := s.Failures(CloudReasonQueueFull); got != 50 {
			t.Fatalf("cumulative queue_full = %d, want 50", got)
		}
		if got := s.Health(clk.Now()); got != CloudHealthOK {
			t.Fatalf("after reuse = %v", got)
		}
	})

	t.Run("flush failure run ages out", func(t *testing.T) {
		clk := &fakeClock{now: healthBase}
		s := newCloudWriteStats(clk.Now)
		recordN(s, CloudReasonFlushError, cloudHealthMaxConsecutiveFlushFailures)
		if got := s.Health(healthBase.Add(cloudHealthWindow - time.Second)); got != CloudHealthFlushFailures {
			t.Fatalf("inside window = %v", got)
		}
		if got := s.Health(healthBase.Add(cloudHealthWindow)); got != CloudHealthOK {
			t.Fatalf("after window = %v", got)
		}
	})

	t.Run("sample older than the slot is left out", func(t *testing.T) {
		clk := &fakeClock{now: healthBase.Add(cloudHealthWindow)}
		s := newCloudWriteStats(clk.Now)
		s.RecordWrite()
		clk.Set(healthBase) // a whole window behind, same slot index
		s.RecordWrite()
		if c := s.windowCounts(healthBase.Add(cloudHealthWindow)); c.writes != 1 {
			t.Fatalf("writes = %d, want 1", c.writes)
		}
	})
}

// The message: exactly "healthy" when healthy, even with counted failures;
// otherwise the "degraded: " prefix the hub requires, the reason, and only
// fixed labels with integers, so no error text or record content can reach
// it. Configured follows the circuit registration.
func TestCloudWriteStats_HealthStatusMessage(t *testing.T) {
	clk := &fakeClock{now: healthBase}
	s := newCloudWriteStats(clk.Now)
	if s.Configured() {
		t.Fatal("configured without a circuit source")
	}
	var open atomic.Bool
	unregister := s.RegisterCircuitSource(open.Load)
	if !s.Configured() {
		t.Fatal("not configured with a circuit source")
	}

	s.RecordFailure(CloudReasonError)
	if got := s.HealthStatus(clk.Now()); got != "healthy" {
		t.Fatalf("single failure = %q, want healthy", got)
	}

	shape := regexp.MustCompile(`^degraded: [a-z1% ]+ \(window=5m0s writes=[0-9]+ error=[0-9]+ queue_full=[0-9]+ flush_error=[0-9]+ circuit_open=[0-9]+ ratio=([0-9]+\.[0-9]{2}%|n/a)\)$`)
	check := func(got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("status = %q\nwant     %q", got, want)
		}
		if !strings.HasPrefix(got, "degraded: ") || !shape.MatchString(got) {
			t.Fatalf("status %q outside the fixed shape", got)
		}
	}

	// Ratio: 9 error + 6 queue_full of 1200 writes = 1.25%.
	writeN(s, 1200)
	recordN(s, CloudReasonError, 8)
	recordN(s, CloudReasonQueueFull, 6)
	check(s.HealthStatus(clk.Now()),
		"degraded: write failure ratio above 1% (window=5m0s writes=1200 error=9 queue_full=6 flush_error=0 circuit_open=0 ratio=1.25%)")

	// Circuit open, on a fresh window with no writes: ratio n/a.
	at := healthBase.Add(cloudHealthWindow)
	clk.Set(at)
	open.Store(true)
	recordN(s, CloudReasonCircuitOpen, 2)
	check(s.HealthStatus(at),
		"degraded: circuit open (window=5m0s writes=0 error=0 queue_full=0 flush_error=0 circuit_open=2 ratio=n/a)")

	open.Store(false)
	recordN(s, CloudReasonFlushError, 3)
	check(s.HealthStatus(at),
		"degraded: consecutive flush failures (window=5m0s writes=0 error=0 queue_full=0 flush_error=3 circuit_open=2 ratio=n/a)")

	unregister()
	if s.Configured() {
		t.Fatal("still configured after unregister")
	}
}

// Nil receivers are no-ops for the new recorders.
func TestCloudWriteStats_NilRecorders(t *testing.T) {
	var s *CloudWriteStats
	s.RecordWrite()
	s.RecordFlushSuccess()
	if got := s.HealthStatus(healthBase); got != "healthy" {
		t.Fatalf("nil stats status = %q", got)
	}
}

// The resilient handler ends a flush-failure run on a successful flush, so
// failures with successes between them never degrade the check.
func TestResilientCloudHandler_FlushSuccessEndsFailureRun(t *testing.T) {
	h, stats, _ := newObservedResilient(t, ResilientCloudHandlerConfig{
		MaxFailures:  100, // keep the circuit closed: this test is about the run
		ProbeTimeout: time.Hour,
	})
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
	flush := func(err error) {
		t.Helper()
		h.flushFn = func() error { return err }
		h.runHealthCheck()
		waitFlushIdle()
	}

	failed := errors.New("flush failed")
	flush(failed)
	flush(failed)
	flush(nil)
	flush(failed)
	flush(failed)
	if got := stats.consecutiveFlushFailures.Load(); got != 2 {
		t.Fatalf("run = %d, want 2", got)
	}
	if got := stats.Health(time.Now()); got != CloudHealthOK {
		t.Fatalf("health = %v, want healthy", got)
	}
	flush(failed)
	if got := stats.Health(time.Now()); got != CloudHealthFlushFailures {
		t.Fatalf("health after 3 in a row = %v", got)
	}
}

// The ratio in the message is rounded up to whole basis points, so a
// degraded window never prints 1.00%.
func TestCloudWindowCounts_SummaryRatio(t *testing.T) {
	tests := []struct {
		writes, errs, full uint64
		want               string
	}{
		{writes: 0, errs: 3, want: "ratio=n/a"},
		{writes: 1000, errs: 10, want: "ratio=1.00%"},
		{writes: 1200, errs: 9, full: 6, want: "ratio=1.25%"},
		{writes: 10001, errs: 101, want: "ratio=1.01%"},
		{writes: 3, errs: 1, want: "ratio=33.34%"},
		{writes: 7, errs: 7, want: "ratio=100.00%"},
	}
	for _, tt := range tests {
		var c cloudWindowCounts
		c.writes = tt.writes
		c.failures[CloudReasonError] = tt.errs
		c.failures[CloudReasonQueueFull] = tt.full
		if got := c.summary(); !strings.HasSuffix(got, " "+tt.want) {
			t.Errorf("writes=%d errs=%d full=%d: summary %q, want suffix %q", tt.writes, tt.errs, tt.full, got, tt.want)
		}
	}

	// The decision and the display agree on a large window just above 1%.
	clk := &fakeClock{now: healthBase}
	s := newCloudWriteStats(clk.Now)
	writeN(s, 10001)
	recordN(s, CloudReasonError, 101)
	want := "degraded: write failure ratio above 1% (window=5m0s writes=10001 error=101 queue_full=0 flush_error=0 circuit_open=0 ratio=1.01%)"
	if got := s.HealthStatus(clk.Now()); got != want {
		t.Fatalf("status = %q\nwant     %q", got, want)
	}
}

// Each Cloud handler counts one window write per entry it hands on (the
// failure ratio's denominator), derived handlers count into the same stats,
// and the count happens on the test hook path too.
func TestCloudHandlers_RecordWritePerEntry(t *testing.T) {
	newBase := func(s *CloudWriteStats, sent *atomic.Int64) CloudHandler {
		return CloudHandler{
			level:     slog.LevelInfo,
			component: "test",
			logHook:   func(gcplog.Entry) { sent.Add(1) },
			stats:     s,
		}
	}
	handlers := map[string]func(CloudHandler) slog.Handler{
		"cloud":    func(b CloudHandler) slog.Handler { return &b },
		"message":  func(b CloudHandler) slog.Handler { return &messageCloudHandler{CloudHandler: b} },
		"artifact": func(b CloudHandler) slog.Handler { return &artifactCloudHandler{CloudHandler: b} },
	}
	for name, build := range handlers {
		t.Run(name, func(t *testing.T) {
			clk := &fakeClock{now: healthBase}
			s := newCloudWriteStats(clk.Now)
			sent := &atomic.Int64{}
			h := build(newBase(s, sent))
			ctx := context.Background()
			r := slog.NewRecord(clk.Now(), slog.LevelInfo, "entry", 0)
			for i := 1; i <= 3; i++ {
				if err := h.Handle(ctx, r); err != nil {
					t.Fatal(err)
				}
				if got := s.windowCounts(clk.Now()).writes; got != uint64(i) {
					t.Fatalf("after %d entries: window writes = %d", i, got)
				}
			}
			if err := h.WithAttrs([]slog.Attr{slog.String("k", "v")}).Handle(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err := h.WithGroup("g").Handle(ctx, r); err != nil {
				t.Fatal(err)
			}
			if got := s.windowCounts(clk.Now()).writes; got != 5 {
				t.Fatalf("with derived handlers: window writes = %d, want 5", got)
			}
			if sent.Load() != 5 {
				t.Fatalf("entries handed to the hook = %d, want 5", sent.Load())
			}
		})
	}
}

// Concurrent writers, failures and health reads across a slot rollover, for
// the race detector. Every worker writes its first half in one slot, then
// blocks mid-run while the clock moves to the next slot, then writes its
// second half there, so all workers race the reset of the new slot. A
// one-slot advance never reuses a slot that holds a tick inside the window,
// so each slot's counts end exact. No sleeps: channels and wait groups only.
func TestCloudWriteStats_ConcurrentSlotRollover(t *testing.T) {
	const (
		workers   = 8
		perWorker = 1000
		errEvery  = 100
	)
	before := healthBase.Add(cloudHealthSlotWidth - time.Millisecond)
	after := healthBase.Add(cloudHealthSlotWidth) // the next slot
	clk := &fakeClock{now: before}
	s := newCloudWriteStats(clk.Now)
	var open atomic.Bool
	s.RegisterCircuitSource(open.Load)

	record := func(i int) {
		s.RecordWrite()
		if i%errEvery == 0 {
			s.RecordFailure(CloudReasonError)
		}
	}
	var halfDone, workersDone sync.WaitGroup
	start := make(chan struct{})
	advanced := make(chan struct{})
	for w := 0; w < workers; w++ {
		halfDone.Add(1)
		workersDone.Add(1)
		go func() {
			defer workersDone.Done()
			<-start
			for i := 0; i < perWorker/2; i++ {
				record(i)
			}
			halfDone.Done()
			<-advanced // blocked mid-run while the clock moves
			for i := perWorker / 2; i < perWorker; i++ {
				record(i)
			}
		}()
	}
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := s.HealthStatus(clk.Now())
			if got != "healthy" && !strings.HasPrefix(got, "degraded: ") {
				t.Errorf("status %q outside the contract", got)
				return
			}
		}
	}()

	close(start)
	halfDone.Wait()
	clk.Set(after)
	close(advanced)
	workersDone.Wait()
	close(stop)
	<-readerDone

	half := uint64(workers * perWorker / 2)
	halfErrs := uint64(workers * perWorker / 2 / errEvery)
	if got := s.Failures(CloudReasonError); got != 2*halfErrs {
		t.Fatalf("cumulative error = %d, want %d", got, 2*halfErrs)
	}
	// At the first tick the window holds only the first halves.
	if c := s.windowCounts(before); c.writes != half || c.failures[CloudReasonError] != halfErrs {
		t.Fatalf("first slot window = %+v, want writes %d error %d", c, half, halfErrs)
	}
	// At the new tick it holds both halves.
	if c := s.windowCounts(after); c.writes != 2*half || c.failures[CloudReasonError] != 2*halfErrs {
		t.Fatalf("window = %+v, want writes %d error %d", c, 2*half, 2*halfErrs)
	}
}
