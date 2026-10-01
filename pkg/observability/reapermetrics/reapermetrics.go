/*
Copyright 2026 The Scion Authors.
*/

// Package reapermetrics provides Cloud Monitoring scaffolding for the
// async-create launch reaper (design §3.7).
//
// It defines OpenTelemetry metric instruments for the reaper's visibility
// requirement: a counter of ticks by outcome, a counter of per-row errors,
// and a gauge of how long the cluster has been disarmed. The package mirrors
// the dispatchmetrics pattern: a Recorder interface backed by an OTel
// MeterProvider (or no-op when none is supplied).
package reapermetrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const instrumentationName = "github.com/GoogleCloudPlatform/scion/pkg/observability/reapermetrics"

const (
	MetricLaunchReaperTicks       = "scion.launch_reaper.ticks"
	MetricLaunchReaperRowErrors   = "scion.launch_reaper.row_errors"
	MetricLaunchReaperDisarmedFor = "scion.launch_reaper.disarmed_for"
)

// Recorder is the interface callers use to record launch-reaper metrics. All
// methods are safe to call concurrently and are cheap no-ops when metrics
// are disabled.
type Recorder interface {
	// IncTicks records n completed reaper ticks, typically n=1 per call with
	// an "outcome" attribute (not_acquired | unavailable | completed | failed).
	IncTicks(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	// IncRowErrors records n per-row savepoint failures from a single tick
	// (design §3.7 step 6: a poison row does not fail the tick, but is
	// counted here).
	IncRowErrors(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	// RecordDisarmedFor records how long the cluster has been disarmed, in
	// seconds, as observed at the start of a completed tick (0 while armed).
	RecordDisarmedFor(ctx context.Context, seconds float64, attrs ...attribute.KeyValue)

	Enabled() bool
}

type recorder struct {
	enabled bool

	ticks       metric.Int64Counter
	rowErrors   metric.Int64Counter
	disarmedFor metric.Float64Gauge
}

var _ Recorder = (*recorder)(nil)

// New creates a Recorder backed by the supplied MeterProvider. If mp is nil,
// a no-op MeterProvider is used and every method becomes a cheap no-op.
func New(mp metric.MeterProvider) (Recorder, error) {
	enabled := mp != nil
	if mp == nil {
		mp = noop.NewMeterProvider()
	}

	meter := mp.Meter(instrumentationName)
	r := &recorder{enabled: enabled}
	var err error

	if r.ticks, err = meter.Int64Counter(
		MetricLaunchReaperTicks,
		metric.WithUnit("{tick}"),
		metric.WithDescription("Number of launch reaper ticks, by outcome"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricLaunchReaperTicks, err)
	}

	if r.rowErrors, err = meter.Int64Counter(
		MetricLaunchReaperRowErrors,
		metric.WithUnit("{row}"),
		metric.WithDescription("Number of per-row savepoint failures during a launch reaper tick"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricLaunchReaperRowErrors, err)
	}

	if r.disarmedFor, err = meter.Float64Gauge(
		MetricLaunchReaperDisarmedFor,
		metric.WithUnit("s"),
		metric.WithDescription("How long the launch reaper has been disarmed cluster-wide (0 while armed)"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricLaunchReaperDisarmedFor, err)
	}

	return r, nil
}

// NewDisabled returns a Recorder whose calls are all no-ops.
func NewDisabled() Recorder {
	r, _ := New(nil)
	return r
}

func (r *recorder) Enabled() bool { return r.enabled }

func (r *recorder) IncTicks(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.ticks.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncRowErrors(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.rowErrors.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) RecordDisarmedFor(ctx context.Context, seconds float64, attrs ...attribute.KeyValue) {
	r.disarmedFor.Record(ctx, seconds, metric.WithAttributes(attrs...))
}
