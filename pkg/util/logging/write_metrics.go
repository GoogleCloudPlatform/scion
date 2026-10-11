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
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
)

// writeMetricsScope is the OTel instrumentation scope of the logging write
// metrics (group SCION_METRICS_LOGGING, name pattern scion.logging.*).
const writeMetricsScope = "github.com/GoogleCloudPlatform/scion/pkg/util/logging"

// Metric names (design §3.5 / C1.3).
const (
	MetricWriteFailures    = "scion.logging.write.failures"
	MetricWriteRecords     = "scion.logging.write.records"
	MetricWriteLateReturns = "scion.logging.write.late_returns"
	MetricQueueDepth       = "scion.logging.queue.depth"
	MetricWriterStalled    = "scion.logging.writer.stalled"
)

// WriterSource is what WriteMetrics observes at collection time. An
// *asyncwrite.Writer satisfies it.
type WriterSource interface {
	Name() string
	QueueDepth() int64
	Stalled() bool
}

// WriteMetrics exports asyncwrite writer outcomes and state over OTel. It
// implements asyncwrite.Recorder (cheap, non-blocking, never panics) and
// registers observable gauges read at every collection, so a stuck writer is
// visible even when no new records arrive and nobody polls /healthz.
//
// Exported series per writer:
//
//   - scion.logging.write.failures{writer, reason}: reason is one of
//     error, timeout, queue_full, oversize, unsupported, closed, shutdown.
//     Rejections (queue_full, oversize, unsupported, closed) were never
//     admitted; error, timeout and shutdown are terminal outcomes of
//     admitted records. reason=error includes a cooperative handler that
//     returned its budget context's deadline error before the budget timer
//     claimed the write; whichever claims first decides, never both.
//   - scion.logging.write.records{writer}: records the inner handler
//     accepted. That is local acceptance (stdout write completed, or a cloud
//     client buffered the entry), never remote ingestion.
//   - scion.logging.write.late_returns{writer}: writes that returned after
//     their timeout was already counted. Not a failure.
//   - scion.logging.queue.depth{writer}: queued records (gauge).
//   - scion.logging.writer.stalled{writer}: 1 while the in-flight write has
//     exceeded its budget, else 0 (gauge).
type WriteMetrics struct {
	failures metric.Int64Counter
	records  metric.Int64Counter
	late     metric.Int64Counter
	depth    metric.Int64ObservableGauge
	stalled  metric.Int64ObservableGauge

	mu      sync.Mutex
	sources []WriterSource
}

// NewWriteMetrics creates the instruments on mp.
func NewWriteMetrics(mp metric.MeterProvider) (*WriteMetrics, error) {
	if mp == nil {
		return nil, errors.New("logging write metrics: nil MeterProvider")
	}
	m := mp.Meter(writeMetricsScope)
	wm := &WriteMetrics{}
	var err error
	if wm.failures, err = m.Int64Counter(MetricWriteFailures, metric.WithUnit("{record}"),
		metric.WithDescription("Log records lost or rejected by an async writer, by reason. "+
			"Rejections (queue_full, oversize, unsupported, closed) were never queued; error, timeout and "+
			"shutdown are terminal outcomes of queued records. error can include a cooperative handler "+
			"returning its write-budget deadline error. Losses are counted, never retried.")); err != nil {
		return nil, err
	}
	if wm.records, err = m.Int64Counter(MetricWriteRecords, metric.WithUnit("{record}"),
		metric.WithDescription("Log records accepted by the inner handler of an async writer "+
			"(local acceptance, not remote ingestion).")); err != nil {
		return nil, err
	}
	if wm.late, err = m.Int64Counter(MetricWriteLateReturns, metric.WithUnit("{record}"),
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
	if _, err = m.RegisterCallback(wm.observe, wm.depth, wm.stalled); err != nil {
		return nil, err
	}
	return wm, nil
}

// Observe adds a writer to the gauges read at collection time.
func (wm *WriteMetrics) Observe(src WriterSource) {
	if wm == nil || src == nil {
		return
	}
	wm.mu.Lock()
	wm.sources = append(wm.sources, src)
	wm.mu.Unlock()
}

func (wm *WriteMetrics) observe(_ context.Context, o metric.Observer) error {
	wm.mu.Lock()
	sources := append([]WriterSource(nil), wm.sources...)
	wm.mu.Unlock()
	for _, src := range sources {
		attrs := metric.WithAttributes(attribute.String("writer", src.Name()))
		o.ObserveInt64(wm.depth, src.QueueDepth(), attrs)
		var stalled int64
		if src.Stalled() {
			stalled = 1
		}
		o.ObserveInt64(wm.stalled, stalled, attrs)
	}
	return nil
}

// Record implements asyncwrite.Recorder.
func (wm *WriteMetrics) Record(writer string, result asyncwrite.Result) {
	if wm == nil {
		return
	}
	ctx := context.Background()
	switch {
	case result == asyncwrite.ResultWritten:
		wm.records.Add(ctx, 1, metric.WithAttributes(attribute.String("writer", writer)))
	case result == asyncwrite.ResultLateReturn:
		wm.late.Add(ctx, 1, metric.WithAttributes(attribute.String("writer", writer)))
	case result.IsFailure():
		wm.failures.Add(ctx, 1, metric.WithAttributes(
			attribute.String("writer", writer),
			attribute.String("reason", result.String()),
		))
	}
}

var _ asyncwrite.Recorder = (*WriteMetrics)(nil)
