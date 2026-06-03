/*
Copyright 2026 The Scion Authors.
*/

// Package dispatchmetrics provides Cloud Monitoring scaffolding for the
// multi-node broker-dispatch observability requirement (B5-2).
//
// It defines OpenTelemetry metric instruments for the dispatch pipeline:
// published/claimed/done/failed counters, intent-to-done latency histogram,
// message dispatched/stuck counters, command-bus reconnects, and reconcile
// drain duration. The package mirrors the dbmetrics pattern: a Recorder
// interface backed by an OTel MeterProvider (or no-op when none is supplied).
package dispatchmetrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const instrumentationName = "github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"

const (
	MetricDispatchPublished  = "scion.dispatch.published"
	MetricDispatchClaimed    = "scion.dispatch.claimed"
	MetricDispatchDone       = "scion.dispatch.done"
	MetricDispatchFailed     = "scion.dispatch.failed"
	MetricDispatchLatency    = "scion.dispatch.intent_to_done.duration"
	MetricMessageDispatched  = "scion.dispatch.message.dispatched"
	MetricMessageStuck       = "scion.dispatch.message.stuck"
	MetricCmdBusReconnects   = "scion.dispatch.cmdbus.reconnects"
	MetricReconcileDrainDur  = "scion.dispatch.reconcile.drain.duration"
)

// Recorder is the interface callers use to record broker-dispatch metrics.
// All methods are safe to call concurrently and are cheap no-ops when metrics
// are disabled.
type Recorder interface {
	IncPublished(ctx context.Context, n int64, attrs ...attribute.KeyValue)
	IncClaimed(ctx context.Context, n int64, attrs ...attribute.KeyValue)
	IncDone(ctx context.Context, n int64, attrs ...attribute.KeyValue)
	IncFailed(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	RecordDispatchLatency(ctx context.Context, ms float64, attrs ...attribute.KeyValue)

	IncMessageDispatched(ctx context.Context, n int64, attrs ...attribute.KeyValue)
	ObserveMessageStuck(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	IncCmdBusReconnects(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	RecordReconcileDrainDuration(ctx context.Context, ms float64, attrs ...attribute.KeyValue)

	Enabled() bool
}

type recorder struct {
	enabled bool

	published  metric.Int64Counter
	claimed    metric.Int64Counter
	done       metric.Int64Counter
	failed     metric.Int64Counter
	latency    metric.Float64Histogram

	msgDispatched metric.Int64Counter
	msgStuck      metric.Int64Gauge

	cmdBusReconn metric.Int64Counter
	drainDur     metric.Float64Histogram
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

	if r.published, err = meter.Int64Counter(
		MetricDispatchPublished,
		metric.WithUnit("{dispatch}"),
		metric.WithDescription("Number of broker dispatch intents published"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricDispatchPublished, err)
	}

	if r.claimed, err = meter.Int64Counter(
		MetricDispatchClaimed,
		metric.WithUnit("{dispatch}"),
		metric.WithDescription("Number of broker dispatch intents claimed"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricDispatchClaimed, err)
	}

	if r.done, err = meter.Int64Counter(
		MetricDispatchDone,
		metric.WithUnit("{dispatch}"),
		metric.WithDescription("Number of broker dispatch intents completed"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricDispatchDone, err)
	}

	if r.failed, err = meter.Int64Counter(
		MetricDispatchFailed,
		metric.WithUnit("{dispatch}"),
		metric.WithDescription("Number of broker dispatch intents failed"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricDispatchFailed, err)
	}

	if r.latency, err = meter.Float64Histogram(
		MetricDispatchLatency,
		metric.WithUnit("ms"),
		metric.WithDescription("Latency from dispatch intent creation to completion"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricDispatchLatency, err)
	}

	if r.msgDispatched, err = meter.Int64Counter(
		MetricMessageDispatched,
		metric.WithUnit("{message}"),
		metric.WithDescription("Number of messages dispatched to remote broker"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricMessageDispatched, err)
	}

	if r.msgStuck, err = meter.Int64Gauge(
		MetricMessageStuck,
		metric.WithUnit("{message}"),
		metric.WithDescription("Number of messages stuck in pending state beyond threshold"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricMessageStuck, err)
	}

	if r.cmdBusReconn, err = meter.Int64Counter(
		MetricCmdBusReconnects,
		metric.WithUnit("{reconnect}"),
		metric.WithDescription("Number of command bus listener reconnects"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricCmdBusReconnects, err)
	}

	if r.drainDur, err = meter.Float64Histogram(
		MetricReconcileDrainDur,
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of a reconcile broker drain cycle"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricReconcileDrainDur, err)
	}

	return r, nil
}

// NewDisabled returns a Recorder whose calls are all no-ops.
func NewDisabled() Recorder {
	r, _ := New(nil)
	return r
}

func (r *recorder) Enabled() bool { return r.enabled }

func (r *recorder) IncPublished(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.published.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncClaimed(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.claimed.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncDone(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.done.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncFailed(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.failed.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) RecordDispatchLatency(ctx context.Context, ms float64, attrs ...attribute.KeyValue) {
	r.latency.Record(ctx, ms, metric.WithAttributes(attrs...))
}

func (r *recorder) IncMessageDispatched(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.msgDispatched.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) ObserveMessageStuck(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.msgStuck.Record(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncCmdBusReconnects(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.cmdBusReconn.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) RecordReconcileDrainDuration(ctx context.Context, ms float64, attrs ...attribute.KeyValue) {
	r.drainDur.Record(ctx, ms, metric.WithAttributes(attrs...))
}
