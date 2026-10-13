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
	CloudHealthRecentFailures
)

// String returns a fixed, non-sensitive description.
func (r CloudHealthReason) String() string {
	switch r {
	case CloudHealthOK:
		return "healthy"
	case CloudHealthCircuitOpen:
		return "circuit open"
	case CloudHealthRecentFailures:
		return "recent write failures"
	default:
		return "unknown"
	}
}

// CloudWriteStats holds the in-process counters of the direct Cloud Logging
// path (writer=cloud). They are plain atomics, always present, whether or
// not a MeterProvider exists: logging is initialized before the hub's
// MeterProvider, so WriteMetrics reads these counters at every collection
// (ObserveCloud) and nothing recorded before it is attached is lost.
//
// Counting never logs, never blocks and never calls back into slog.
type CloudWriteStats struct {
	failures            [numCloudReasons]atomic.Uint64
	lastFailureUnixNano atomic.Int64
	circuit             atomic.Pointer[func() bool]

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
// same order as asyncwrite, design C3.2). An out-of-range reason is ignored.
func (s *CloudWriteStats) RecordFailure(r CloudFailureReason) {
	if s == nil || r >= numCloudReasons {
		return
	}
	n := s.clock().UnixNano()
	s.failures[r].Add(1)
	for {
		old := s.lastFailureUnixNano.Load()
		if n <= old || s.lastFailureUnixNano.CompareAndSwap(old, n) {
			return
		}
	}
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
// (including half-open), recent failures within
// asyncwrite.DefaultFailureWindow (5 min), healthy.
func (s *CloudWriteStats) Health(now time.Time) CloudHealthReason {
	if open, _ := s.CircuitOpen(); open {
		return CloudHealthCircuitOpen
	}
	if last := s.LastFailure(); !last.IsZero() && now.Sub(last) < asyncwrite.DefaultFailureWindow {
		return CloudHealthRecentFailures
	}
	return CloudHealthOK
}

// HealthStatus returns the /healthz value for the cloud_logging key:
// "healthy", "degraded: circuit open" or "degraded: recent write failures".
func (s *CloudWriteStats) HealthStatus(now time.Time) string {
	r := s.Health(now)
	if r == CloudHealthOK {
		return r.String()
	}
	return "degraded: " + r.String()
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
