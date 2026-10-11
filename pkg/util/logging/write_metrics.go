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
	"math"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

// writeMetricsScope is the OTel instrumentation scope of the logging write
// metrics (group SCION_METRICS_LOGGING, name pattern scion.logging.*).
const writeMetricsScope = "github.com/GoogleCloudPlatform/scion/pkg/util/logging"

// Metric names (design §3.5 / C1.3; P2B.2 adds the circuit_open gauge).
const (
	MetricWriteFailures     = "scion.logging.write.failures"
	MetricWriteRecords      = "scion.logging.write.records"
	MetricWriteLateReturns  = "scion.logging.write.late_returns"
	MetricQueueDepth        = "scion.logging.queue.depth"
	MetricWriterStalled     = "scion.logging.writer.stalled"
	MetricWriterCircuitOpen = "scion.logging.writer.circuit_open"
)

// WriterSource is what WriteMetrics observes at collection time. An
// *asyncwrite.Writer satisfies it.
type WriterSource interface {
	Name() string
	QueueDepth() int64
	Stalled() bool
	// Snapshot supplies the cumulative counters exported as observable
	// counters.
	Snapshot() asyncwrite.Snapshot
}

// WriteMetrics exports log writer outcomes and state over OTel. Every
// instrument is observable and read at collection time from the writers'
// in-process cumulative atomics (asyncwrite Snapshot for async writers,
// CloudWriteStats for writer=cloud). Nothing is recorded on the write path,
// counts recorded before WriteMetrics is attached are exported in full, and
// each reader sees the same cumulative totals. A stuck writer is visible even
// when no new records arrive and nobody polls /healthz.
//
// Exported series:
//
//   - scion.logging.write.failures{writer, reason} (cumulative counter).
//     For async writers (writer=audit) reason is one of error, timeout,
//     queue_full, oversize, unsupported, closed, shutdown. Rejections
//     (queue_full, oversize, unsupported, closed) were never admitted;
//     error, timeout and shutdown are terminal outcomes of admitted
//     records. reason=error includes a cooperative handler that returned
//     its budget context's deadline error before the budget timer claimed
//     the write; whichever claims first decides, never both.
//     For writer=cloud reason is one of error (a Cloud Logging client error
//     reported to OnError; one per error, not per record), queue_full (the
//     client buffer dropped an entry), circuit_open (a record dropped while
//     the circuit breaker was open or half-open) or flush_error (a failed
//     probe, periodic or shutdown flush; it can overlap with error for the
//     same incident). At most nine reason values in total.
//   - scion.logging.write.records{writer} (cumulative counter): records the
//     inner handler of an async writer accepted. That is local acceptance
//     (stdout write completed, or a cloud client buffered the entry), never
//     remote ingestion. Not reported for writer=cloud.
//   - scion.logging.write.late_returns{writer} (cumulative counter): writes
//     that returned after their timeout was already counted. Not a failure.
//   - scion.logging.queue.depth{writer}: queued records (gauge).
//   - scion.logging.writer.stalled{writer}: 1 while the in-flight write has
//     exceeded its budget, else 0 (gauge).
//   - scion.logging.writer.circuit_open{writer=cloud}: 1 while the Cloud
//     Logging circuit breaker is open or half-open, else 0 (gauge; present
//     only while a circuit source is registered).
//
// Counter series appear once their count is nonzero.
type WriteMetrics struct {
	failures    metric.Int64ObservableCounter
	records     metric.Int64ObservableCounter
	late        metric.Int64ObservableCounter
	depth       metric.Int64ObservableGauge
	stalled     metric.Int64ObservableGauge
	circuitOpen metric.Int64ObservableGauge

	mu      sync.Mutex
	sources []WriterSource
	cloud   []*CloudWriteStats
}

// NewWriteMetrics creates the instruments on mp.
func NewWriteMetrics(mp metric.MeterProvider) (*WriteMetrics, error) {
	if mp == nil {
		return nil, errors.New("logging write metrics: nil MeterProvider")
	}
	m := mp.Meter(writeMetricsScope)
	wm := &WriteMetrics{}
	var err error
	if wm.failures, err = m.Int64ObservableCounter(MetricWriteFailures, metric.WithUnit("{record}"),
		metric.WithDescription("Log records lost or rejected, or log write failures, by writer and reason. "+
			"Async writers (audit): rejections (queue_full, oversize, unsupported, closed) were never queued; error, timeout and "+
			"shutdown are terminal outcomes of queued records. error can include a cooperative handler "+
			"returning its write-budget deadline error. "+
			"Cloud Logging (cloud): error (client-reported error, one per error), queue_full (client buffer drop), "+
			"circuit_open (record dropped while the circuit breaker is open), flush_error (failed flush). "+
			"Losses are counted, never retried.")); err != nil {
		return nil, err
	}
	if wm.records, err = m.Int64ObservableCounter(MetricWriteRecords, metric.WithUnit("{record}"),
		metric.WithDescription("Log records accepted by the inner handler of an async writer "+
			"(local acceptance, not remote ingestion).")); err != nil {
		return nil, err
	}
	if wm.late, err = m.Int64ObservableCounter(MetricWriteLateReturns, metric.WithUnit("{record}"),
		metric.WithDescription("Async writes that returned after their timeout was already counted.")); err != nil {
		return nil, err
	}
	if wm.depth, err = m.Int64ObservableGauge(MetricQueueDepth, metric.WithUnit("{record}"),
		metric.WithDescription("Records queued in an async writer.")); err != nil {
		return nil, err
	}
	if wm.stalled, err = m.Int64ObservableGauge(MetricWriterStalled,
		metric.WithDescription("1 while an async writer's in-flight write has exceeded its budget, else 0.")); err != nil {
		return nil, err
	}
	if wm.circuitOpen, err = m.Int64ObservableGauge(MetricWriterCircuitOpen,
		metric.WithDescription("1 while the Cloud Logging circuit breaker is open or half-open "+
			"(records are dropped from the Cloud path), else 0.")); err != nil {
		return nil, err
	}
	if _, err = m.RegisterCallback(wm.observe,
		wm.failures, wm.records, wm.late, wm.depth, wm.stalled, wm.circuitOpen); err != nil {
		return nil, err
	}
	return wm, nil
}

// Observe adds an async writer to the instruments read at collection time.
func (wm *WriteMetrics) Observe(src WriterSource) {
	if wm == nil || src == nil {
		return
	}
	wm.mu.Lock()
	wm.sources = append(wm.sources, src)
	wm.mu.Unlock()
}

// ObserveCloud adds the writer=cloud counters (see CloudWriter) to the
// instruments read at collection time. Its failure series appear once a
// count is nonzero; the circuit_open gauge appears while a circuit source is
// registered.
func (wm *WriteMetrics) ObserveCloud(cs *CloudWriteStats) {
	if wm == nil || cs == nil {
		return
	}
	wm.mu.Lock()
	wm.cloud = append(wm.cloud, cs)
	wm.mu.Unlock()
}

func (wm *WriteMetrics) observe(_ context.Context, o metric.Observer) error {
	wm.mu.Lock()
	sources := append([]WriterSource(nil), wm.sources...)
	cloud := append([]*CloudWriteStats(nil), wm.cloud...)
	wm.mu.Unlock()
	observeCount := func(inst metric.Int64Observable, n uint64, kv ...attribute.KeyValue) {
		if n == 0 {
			return
		}
		o.ObserveInt64(inst, clampInt64(n), metric.WithAttributes(kv...))
	}
	for _, src := range sources {
		name := src.Name()
		writer := attribute.String("writer", name)
		snap := src.Snapshot()
		observeCount(wm.records, snap.Written, writer)
		observeCount(wm.late, snap.LateReturns, writer)
		for _, f := range asyncFailureReasons {
			observeCount(wm.failures, f.count(snap), writer, attribute.String("reason", f.result.String()))
		}
		attrs := metric.WithAttributes(writer)
		o.ObserveInt64(wm.depth, src.QueueDepth(), attrs)
		var stalled int64
		if src.Stalled() {
			stalled = 1
		}
		o.ObserveInt64(wm.stalled, stalled, attrs)
	}
	for _, cs := range cloud {
		writer := attribute.String("writer", cs.Name())
		for r := CloudFailureReason(0); r < numCloudReasons; r++ {
			observeCount(wm.failures, cs.Failures(r), writer, attribute.String("reason", r.String()))
		}
		if open, ok := cs.CircuitOpen(); ok {
			var v int64
			if open {
				v = 1
			}
			o.ObserveInt64(wm.circuitOpen, v, metric.WithAttributes(writer))
		}
	}
	return nil
}

// asyncFailureReasons is the single mapping from an async writer's
// Snapshot counters to scion.logging.write.failures{reason}. Every
// asyncwrite.Result except written and late_return is a failure; those two
// are exported as write.records and write.late_returns instead.
var asyncFailureReasons = []struct {
	result asyncwrite.Result
	count  func(asyncwrite.Snapshot) uint64
}{
	{asyncwrite.ResultError, func(s asyncwrite.Snapshot) uint64 { return s.WriteErrors }},
	{asyncwrite.ResultTimeout, func(s asyncwrite.Snapshot) uint64 { return s.WriteTimeouts }},
	{asyncwrite.ResultShutdown, func(s asyncwrite.Snapshot) uint64 { return s.DroppedShutdown }},
	{asyncwrite.ResultQueueFull, func(s asyncwrite.Snapshot) uint64 { return s.DroppedFull }},
	{asyncwrite.ResultOversize, func(s asyncwrite.Snapshot) uint64 { return s.DroppedOversize }},
	{asyncwrite.ResultUnsupported, func(s asyncwrite.Snapshot) uint64 { return s.DroppedUnsupported }},
	{asyncwrite.ResultClosed, func(s asyncwrite.Snapshot) uint64 { return s.DroppedClosed }},
}

func clampInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
