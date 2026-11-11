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
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	gcplog "cloud.google.com/go/logging"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

// CloudWriterName is the writer label of the direct Cloud Logging path on
// scion.logging.* metrics.
const CloudWriterName = "cloud"

// CloudFailureReason is the closed set of loss and failure reasons counted
// for writer=cloud on scion.logging.write.failures. It is separate from the
// generic asyncwrite.Result set; together the two sets give at most nine
// reason values on the one instrument.
type CloudFailureReason uint8

const (
	// CloudReasonError: the Cloud Logging client reported an error through
	// its OnError hook (a failed WriteLogEntries batch, an invalid entry or
	// an oversized entry). One count per reported error, not per record.
	CloudReasonError CloudFailureReason = iota
	// CloudReasonQueueFull: the client's buffer (BufferedByteLimit) was full
	// and it dropped the entry (gcplog.ErrOverflow). One count per entry.
	CloudReasonQueueFull
	// CloudReasonCircuitOpen: the circuit breaker was open or half-open and
	// the record was dropped from the Cloud path. One count per record.
	CloudReasonCircuitOpen
	// CloudReasonFlushError: a probe, periodic or shutdown flush failed.
	// One count per failed flush call.
	CloudReasonFlushError

	numCloudReasons
)

// String returns the bounded metric label for the reason.
func (r CloudFailureReason) String() string {
	switch r {
	case CloudReasonError:
		return "error"
	case CloudReasonQueueFull:
		return "queue_full"
	case CloudReasonCircuitOpen:
		return "circuit_open"
	case CloudReasonFlushError:
		return "flush_error"
	default:
		return "unknown"
	}
}

// CloudHealthReason is the closed set of cloud_logging health reasons.
type CloudHealthReason uint8

const (
	CloudHealthOK CloudHealthReason = iota
	CloudHealthCircuitOpen
	CloudHealthFlushFailures
	CloudHealthFailureRatio
)

// String returns a fixed, non-sensitive description.
func (r CloudHealthReason) String() string {
	switch r {
	case CloudHealthOK:
		return "healthy"
	case CloudHealthCircuitOpen:
		return "circuit open"
	case CloudHealthFlushFailures:
		return "consecutive flush failures"
	case CloudHealthFailureRatio:
		return "write failure ratio above 1%"
	default:
		return "unknown"
	}
}

// cloud_logging health thresholds. A single failed write is only counted;
// the check degrades on a sustained problem (ptone/scion#7361).
const (
	// cloudHealthWindow is the span the failure ratio and the per-reason
	// counts in the health message cover.
	cloudHealthWindow = asyncwrite.DefaultFailureWindow
	// cloudHealthSlotWidth is the window's granularity: the window covers
	// the current slot and the previous cloudHealthSlots-1, so its
	// effective span is between 4.5 and 5 minutes.
	cloudHealthSlotWidth = 30 * time.Second
	cloudHealthSlots     = int(cloudHealthWindow / cloudHealthSlotWidth)
	// cloudHealthMaxFailureBasisPoints is the failure ratio, in basis points
	// of the records handed to the client in the window, above which the
	// check degrades: 100 = 1%.
	cloudHealthMaxFailureBasisPoints = 100
	// cloudHealthMinWrites is the number of records handed to the client in
	// the window below which the ratio is not judged, so that a few failures
	// on a quiet hub cannot trip it. At 1% it takes at least 6 failures. A
	// total outage on a quiet hub still degrades through the circuit breaker.
	cloudHealthMinWrites = 500
	// cloudHealthMaxConsecutiveFlushFailures is the number of failed flushes
	// in a row, with no successful flush between them, that degrades the
	// check. It matches the circuit breaker's DefaultMaxFailures.
	cloudHealthMaxConsecutiveFlushFailures = DefaultMaxFailures
)

// cloudWindowSlot holds the counts of one cloudHealthSlotWidth span. tick
// is the span's time index (UnixNano / width); a slot whose tick is
// outside the window is stale and reset before reuse.
type cloudWindowSlot struct {
	tick     atomic.Int64
	writes   atomic.Uint64
	failures [numCloudReasons]atomic.Uint64
}

// cloudWindowCounts is a snapshot of the windowed counts.
type cloudWindowCounts struct {
	writes   uint64
	failures [numCloudReasons]uint64
}

// ratioFailures is the numerator of the failure ratio: the per-record and
// per-batch client failures. flush_error is judged by the consecutive-flush
// rule (a failed flush usually repeats errors already counted as error) and
// circuit_open by the circuit state.
func (c cloudWindowCounts) ratioFailures() uint64 {
	return c.failures[CloudReasonError] + c.failures[CloudReasonQueueFull]
}

// CloudWriteStats holds the in-process counters of the direct Cloud Logging
// path (writer=cloud). They are plain atomics, always present, whether or
// not a MeterProvider exists: logging is initialized before the hub's
// MeterProvider, so WriteMetrics reads these counters at every collection
// (ObserveCloud) and nothing recorded before it is attached is lost.
//
// Besides the cumulative counters it keeps a rolling window of writes and
// failures for Health. Counting never logs and never calls back into slog;
// it takes a mutex only to reset a stale window slot, at most once per
// slot width.
type CloudWriteStats struct {
	failures            [numCloudReasons]atomic.Uint64
	lastFailureUnixNano atomic.Int64
	circuit             atomic.Pointer[func() bool]

	// consecutiveFlushFailures counts flush_error since the last
	// RecordFlushSuccess.
	consecutiveFlushFailures atomic.Int64
	window                   [cloudHealthSlots]cloudWindowSlot
	windowMu                 sync.Mutex // serializes slot resets only

	// now is the clock (nil means time.Now). Set only before first use.
	now func() time.Time
}

var defaultCloudWriteStats = &CloudWriteStats{}

// CloudWriter returns the process-wide writer=cloud counters. Every Cloud
// Logging handler, its circuit breaker and the request and message loggers
// that share its client count here unless a test injects its own stats.
func CloudWriter() *CloudWriteStats { return defaultCloudWriteStats }

// newCloudWriteStats returns standalone counters with the given clock, for
// tests.
func newCloudWriteStats(now func() time.Time) *CloudWriteStats {
	return &CloudWriteStats{now: now}
}

// Name returns the writer label.
func (s *CloudWriteStats) Name() string { return CloudWriterName }

func (s *CloudWriteStats) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// RecordFailure counts one failure for reason. Order: timestamp sampled
// first, then the counter, then the LastFailure monotonic max-store (the
// same order as asyncwrite, design C3.2). The failure is also counted in the
// health window, and a flush_error extends the consecutive-flush-failure run.
// An out-of-range reason is ignored.
func (s *CloudWriteStats) RecordFailure(r CloudFailureReason) {
	if s == nil || r >= numCloudReasons {
		return
	}
	n := s.clock().UnixNano()
	s.failures[r].Add(1)
	if b := s.slotFor(n); b != nil {
		b.failures[r].Add(1)
	}
	if r == CloudReasonFlushError {
		s.consecutiveFlushFailures.Add(1)
	}
	for {
		old := s.lastFailureUnixNano.Load()
		if n <= old || s.lastFailureUnixNano.CompareAndSwap(old, n) {
			return
		}
	}
}

// RecordWrite counts one record handed to the Cloud Logging client in the
// health window: the denominator of the failure ratio. A client buffer
// accept is not ingestion, so there is no cumulative counter or metric.
func (s *CloudWriteStats) RecordWrite() {
	if s == nil {
		return
	}
	if b := s.slotFor(s.clock().UnixNano()); b != nil {
		b.writes.Add(1)
	}
}

// RecordFlushSuccess ends a run of consecutive flush failures.
func (s *CloudWriteStats) RecordFlushSuccess() {
	if s == nil {
		return
	}
	s.consecutiveFlushFailures.Store(0)
}

// slotFor returns the window slot for the time n (UnixNano), resetting it
// first if it still holds an older tick. It returns nil for a sample older
// than the slot's current tick (a clock read that lost a race by a whole
// window), which is then left out of the window. A record racing a reset
// may be lost from, or misattributed within, the window; the cumulative
// counters are exact.
func (s *CloudWriteStats) slotFor(n int64) *cloudWindowSlot {
	tick := n / int64(cloudHealthSlotWidth)
	idx := tick % int64(cloudHealthSlots)
	if idx < 0 {
		idx += int64(cloudHealthSlots)
	}
	b := &s.window[idx]
	switch cur := b.tick.Load(); {
	case cur == tick:
		return b
	case cur > tick:
		return nil
	}
	s.windowMu.Lock()
	defer s.windowMu.Unlock()
	switch cur := b.tick.Load(); {
	case cur == tick:
		return b
	case cur > tick:
		return nil
	}
	b.writes.Store(0)
	for i := range b.failures {
		b.failures[i].Store(0)
	}
	b.tick.Store(tick)
	return b
}

// windowCounts sums the slots inside the health window that ends at the
// given time.
func (s *CloudWriteStats) windowCounts(at time.Time) cloudWindowCounts {
	var c cloudWindowCounts
	if s == nil {
		return c
	}
	cur := at.UnixNano() / int64(cloudHealthSlotWidth)
	for i := range s.window {
		b := &s.window[i]
		if tick := b.tick.Load(); tick > cur || tick <= cur-int64(cloudHealthSlots) {
			continue
		}
		c.writes += b.writes.Load()
		for r := range b.failures {
			c.failures[r] += b.failures[r].Load()
		}
	}
	return c
}

// Failures returns the cumulative count for reason.
func (s *CloudWriteStats) Failures(r CloudFailureReason) uint64 {
	if s == nil || r >= numCloudReasons {
		return 0
	}
	return s.failures[r].Load()
}

// LastFailure returns the time of the most recent failure (zero if none).
func (s *CloudWriteStats) LastFailure() time.Time {
	if s == nil {
		return time.Time{}
	}
	if n := s.lastFailureUnixNano.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// RegisterCircuitSource registers the circuit-breaker state read by the
// scion.logging.writer.circuit_open gauge and by Health; a Cloud handler
// counts as configured while a source is registered. NewResilientCloudHandler
// registers itself and calls the returned unregister from its cleanup.
// unregister removes the source only if it is still the registered one, so
// a stopped handler never leaves its state behind and never removes a later
// registration. A nil open, or a nil receiver, registers nothing.
func (s *CloudWriteStats) RegisterCircuitSource(open func() bool) (unregister func()) {
	if s == nil || open == nil {
		return func() {}
	}
	p := &open
	s.circuit.Store(p)
	return func() { s.circuit.CompareAndSwap(p, nil) }
}

// CircuitOpen reports the registered circuit state; ok is false when no
// source is registered.
func (s *CloudWriteStats) CircuitOpen() (open, ok bool) {
	if s == nil {
		return false, false
	}
	p := s.circuit.Load()
	if p == nil {
		return false, false
	}
	return (*p)(), true
}

// Configured reports whether a Cloud Logging circuit source is registered.
func (s *CloudWriteStats) Configured() bool {
	_, ok := s.CircuitOpen()
	return ok
}

// Health evaluates cloud_logging health at now. Precedence: circuit open
// (including half-open); cloudHealthMaxConsecutiveFlushFailures flushes in a
// row failed, the latest inside the window; error plus queue_full above
// cloudHealthMaxFailureBasisPoints of at least cloudHealthMinWrites records
// in the window; healthy. Isolated failures are only counted.
func (s *CloudWriteStats) Health(now time.Time) CloudHealthReason {
	r, _ := s.evaluate(now)
	return r
}

func (s *CloudWriteStats) evaluate(at time.Time) (CloudHealthReason, cloudWindowCounts) {
	c := s.windowCounts(at)
	if open, _ := s.CircuitOpen(); open {
		return CloudHealthCircuitOpen, c
	}
	if s != nil && s.consecutiveFlushFailures.Load() >= cloudHealthMaxConsecutiveFlushFailures &&
		c.failures[CloudReasonFlushError] > 0 {
		return CloudHealthFlushFailures, c
	}
	if c.writes >= cloudHealthMinWrites &&
		c.ratioFailures()*10000 > c.writes*cloudHealthMaxFailureBasisPoints {
		return CloudHealthFailureRatio, c
	}
	return CloudHealthOK, c
}

// HealthStatus returns the /healthz value for the cloud_logging key:
// "healthy", or "degraded: <reason> (<counts>)" where reason is a
// CloudHealthReason string and counts are the window's records handed to the
// client, failures by reason and the failure ratio, for example
// "degraded: write failure ratio above 1% (window=5m0s writes=1200 error=9
// queue_full=6 flush_error=0 circuit_open=0 ratio=1.25%)". The value holds
// only fixed labels and integers, never error text or record content.
func (s *CloudWriteStats) HealthStatus(now time.Time) string {
	r, c := s.evaluate(now)
	if r == CloudHealthOK {
		return r.String()
	}
	return "degraded: " + r.String() + " (" + c.summary() + ")"
}

// summary formats the window counts with fixed labels. The ratio is shown in
// whole basis points rounded up, so a ratio above the threshold never prints
// as 1.00%.
func (c cloudWindowCounts) summary() string {
	ratio := "n/a"
	if c.writes > 0 {
		bp := (c.ratioFailures()*10000 + c.writes - 1) / c.writes
		ratio = fmt.Sprintf("%d.%02d%%", bp/100, bp%100)
	}
	return fmt.Sprintf("window=%s writes=%d %s=%d %s=%d %s=%d %s=%d ratio=%s",
		cloudHealthWindow, c.writes,
		CloudReasonError, c.failures[CloudReasonError],
		CloudReasonQueueFull, c.failures[CloudReasonQueueFull],
		CloudReasonFlushError, c.failures[CloudReasonFlushError],
		CloudReasonCircuitOpen, c.failures[CloudReasonCircuitOpen],
		ratio)
}

// classifyCloudError maps a Cloud Logging client error to its reason. Only
// the error identity is inspected; its text never reaches a label.
func classifyCloudError(err error) CloudFailureReason {
	if errors.Is(err, gcplog.ErrOverflow) {
		return CloudReasonQueueFull
	}
	return CloudReasonError
}

// cloudClientOnError returns the Cloud Logging client's OnError hook. It
// counts the error under writer=cloud (queue_full for gcplog.ErrOverflow,
// error otherwise), then writes "logging client: <err>" plus a newline to w,
// the same text as gcplog's default hook. Production passes os.Stderr.
//
// The line deliberately bypasses slog, the std log package, OTel and the
// Cloud handler, so a failing Cloud Logging pipeline is never fed its own
// errors (owner-approved, ptone 17dd4ceb; architect ruling R8). Error text
// goes only to w, never into metric labels. A write error from w is ignored.
// A nil error is a no-op. The hook uses only atomics and one Fprintf, so it
// is safe for concurrent calls. gcplog never calls OnError concurrently and
// feeds it from a small buffered channel, so under an error storm some
// errors skip the hook and the count is low.
func cloudClientOnError(stats *CloudWriteStats, w io.Writer) func(error) {
	return func(err error) {
		if err == nil {
			return
		}
		stats.RecordFailure(classifyCloudError(err))
		if w != nil {
			_, _ = fmt.Fprintf(w, "logging client: %v\n", err)
		}
	}
}
