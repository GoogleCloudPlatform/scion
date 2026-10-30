/*
Copyright 2026 The Scion Authors.
*/

// Package ssemetrics provides the OpenTelemetry instruments for the hub's
// browser events streams (the /events Server-Sent Events endpoint) and for
// the events the hub publishes to them (ptone/scion#5228, phase 0 of
// ptone/scion#5219).
//
// The hub's events-stream registry feeds the stream and user gauges and the
// connect, close and write counters; both event publishers (in-memory and
// Postgres) feed MetricEventsPublished. The package mirrors the reapermetrics
// pattern: a Recorder interface backed by an OTel MeterProvider, or by a
// no-op provider when none is supplied. Every instrument is synchronous, so
// the hub dashboard test can replay them.
package ssemetrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const instrumentationName = "github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"

// Metric names.
const (
	// MetricStreams is the number of open events streams on this hub
	// instance, by AttrKind.
	MetricStreams = "scion.hub.sse.streams"
	// MetricUsers is the number of distinct users with at least one open
	// events stream on this hub instance.
	MetricUsers = "scion.hub.sse.users"
	// MetricConnects counts events streams opened, by AttrKind.
	MetricConnects = "scion.hub.sse.connects"
	// MetricCloses counts events streams closed, by AttrKind and AttrReason.
	MetricCloses = "scion.hub.sse.closes"
	// MetricStreamDuration is the lifetime of a closed events stream in
	// seconds, by AttrKind and AttrReason.
	MetricStreamDuration = "scion.hub.sse.stream_duration"
	// MetricEventsWritten counts update events written to events streams
	// (heartbeats and reconnect hints are not events), by AttrKind.
	MetricEventsWritten = "scion.hub.sse.events_written"
	// MetricBytesWritten counts every byte written to events streams,
	// heartbeats included, by AttrKind.
	MetricBytesWritten = "scion.hub.sse.bytes_written"
	// MetricCapReached counts streams closed because their user reached the
	// per-instance stream cap, by AttrKind. Phase 1 of ptone/scion#5219
	// records it; it stays zero until then.
	MetricCapReached = "scion.hub.sse.cap_reached"
	// MetricEventsPublished counts events handed to an event publisher, by
	// AttrSubjectClass and AttrTrigger.
	MetricEventsPublished = "scion.hub.events.published"
)

// Attribute keys.
const (
	// AttrKind is the stream kind: view, agent-feed, terminal-metadata or
	// other.
	AttrKind = "kind"
	// AttrReason is why a stream closed: client, max_age, superseded, cap,
	// publisher, shutdown or session_ended.
	AttrReason = "reason"
	// AttrSubjectClass is a published subject with its resource ids
	// removed, for example agent.status for agent.<id>.status.
	AttrSubjectClass = "subject_class"
	// AttrTrigger is what caused a publish: TriggerHeartbeat or
	// TriggerChange.
	AttrTrigger = "trigger"
)

// AttrTrigger values.
const (
	// TriggerHeartbeat marks an agent status event published for a
	// heartbeat (an agent heartbeat report with nothing else in it, or a
	// Runtime Broker heartbeat).
	TriggerHeartbeat = "heartbeat"
	// TriggerChange marks every other publish.
	TriggerChange = "change"
)

// durationBuckets are the MetricStreamDuration bucket boundaries in
// seconds, chosen around the stream max ages in use and planned (10 min,
// 15 min, about 1 h).
var durationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 7200}

// Recorder is the interface the hub uses to record events-stream metrics.
// All methods are safe to call concurrently and are cheap no-ops when
// metrics are disabled.
type Recorder interface {
	// RecordStreams sets the open-stream gauge for one kind.
	RecordStreams(ctx context.Context, kind string, n int64)
	// RecordUsers sets the distinct-user gauge.
	RecordUsers(ctx context.Context, n int64)
	// IncConnects counts one opened stream.
	IncConnects(ctx context.Context, kind string)
	// RecordClose counts one closed stream and records its lifetime.
	RecordClose(ctx context.Context, kind, reason string, lifetime time.Duration)
	// AddEventsWritten counts update events written to a stream.
	AddEventsWritten(ctx context.Context, kind string, n int64)
	// AddBytesWritten counts bytes written to a stream.
	AddBytesWritten(ctx context.Context, kind string, n int64)
	// IncCapReached counts one stream closed by the per-user cap.
	IncCapReached(ctx context.Context, kind string)
	// IncPublished counts one published event.
	IncPublished(ctx context.Context, subjectClass, trigger string)

	Enabled() bool
}

type recorder struct {
	enabled bool

	streams       metric.Int64Gauge
	users         metric.Int64Gauge
	connects      metric.Int64Counter
	closes        metric.Int64Counter
	duration      metric.Float64Histogram
	eventsWritten metric.Int64Counter
	bytesWritten  metric.Int64Counter
	capReached    metric.Int64Counter
	published     metric.Int64Counter
}

var _ Recorder = (*recorder)(nil)

// New creates a Recorder backed by the supplied MeterProvider. If mp is nil,
// a no-op MeterProvider is used and every method becomes a cheap no-op.
func New(mp metric.MeterProvider) (Recorder, error) {
	enabled := mp != nil
	if mp == nil {
		mp = noop.NewMeterProvider()
	}

	m := mp.Meter(instrumentationName)
	r := &recorder{enabled: enabled}
	var err error

	if r.streams, err = m.Int64Gauge(MetricStreams,
		metric.WithUnit("{stream}"),
		metric.WithDescription("Open events streams on this hub instance, by kind"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricStreams, err)
	}
	if r.users, err = m.Int64Gauge(MetricUsers,
		metric.WithUnit("{user}"),
		metric.WithDescription("Distinct users with an open events stream on this hub instance"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricUsers, err)
	}
	if r.connects, err = m.Int64Counter(MetricConnects,
		metric.WithUnit("{stream}"),
		metric.WithDescription("Events streams opened, by kind"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricConnects, err)
	}
	if r.closes, err = m.Int64Counter(MetricCloses,
		metric.WithUnit("{stream}"),
		metric.WithDescription("Events streams closed, by kind and reason"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricCloses, err)
	}
	if r.duration, err = m.Float64Histogram(MetricStreamDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Lifetime of closed events streams, by kind and reason"),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricStreamDuration, err)
	}
	if r.eventsWritten, err = m.Int64Counter(MetricEventsWritten,
		metric.WithUnit("{event}"),
		metric.WithDescription("Update events written to events streams, by kind"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricEventsWritten, err)
	}
	if r.bytesWritten, err = m.Int64Counter(MetricBytesWritten,
		metric.WithUnit("By"),
		metric.WithDescription("Bytes written to events streams, heartbeats included, by kind"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricBytesWritten, err)
	}
	if r.capReached, err = m.Int64Counter(MetricCapReached,
		metric.WithUnit("{stream}"),
		metric.WithDescription("Events streams closed because the user reached the per-instance stream cap"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricCapReached, err)
	}
	if r.published, err = m.Int64Counter(MetricEventsPublished,
		metric.WithUnit("{event}"),
		metric.WithDescription("Events handed to the event publisher, by subject class and trigger"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricEventsPublished, err)
	}
	return r, nil
}

// NewDisabled returns a Recorder whose calls are all no-ops.
func NewDisabled() Recorder {
	r, _ := New(nil)
	return r
}

func (r *recorder) Enabled() bool { return r.enabled }

func kindAttr(kind string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(AttrKind, kind))
}

func (r *recorder) RecordStreams(ctx context.Context, kind string, n int64) {
	r.streams.Record(ctx, n, kindAttr(kind))
}

func (r *recorder) RecordUsers(ctx context.Context, n int64) {
	r.users.Record(ctx, n)
}

func (r *recorder) IncConnects(ctx context.Context, kind string) {
	r.connects.Add(ctx, 1, kindAttr(kind))
}

func (r *recorder) RecordClose(ctx context.Context, kind, reason string, lifetime time.Duration) {
	attrs := metric.WithAttributes(attribute.String(AttrKind, kind), attribute.String(AttrReason, reason))
	r.closes.Add(ctx, 1, attrs)
	r.duration.Record(ctx, lifetime.Seconds(), attrs)
}

func (r *recorder) AddEventsWritten(ctx context.Context, kind string, n int64) {
	r.eventsWritten.Add(ctx, n, kindAttr(kind))
}

func (r *recorder) AddBytesWritten(ctx context.Context, kind string, n int64) {
	r.bytesWritten.Add(ctx, n, kindAttr(kind))
}

func (r *recorder) IncCapReached(ctx context.Context, kind string) {
	r.capReached.Add(ctx, 1, kindAttr(kind))
}

func (r *recorder) IncPublished(ctx context.Context, subjectClass, trigger string) {
	r.published.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrSubjectClass, subjectClass),
		attribute.String(AttrTrigger, trigger),
	))
}
