/*
Copyright 2025 The Scion Authors.
*/

// Package dbmetrics provides Cloud Monitoring scaffolding for the Postgres
// LISTEN/NOTIFY observability requirement.
//
// It defines the OpenTelemetry metric instruments used to observe the
// notification pipeline (publish-to-deliver latency, notification counts,
// subscriber lag, listener reconnects, payload sizes) and the database
// connection pools (active/idle/max gauges and a cumulative wait counter).
//
// The package is intentionally lightweight: it registers instruments against an
// OpenTelemetry MeterProvider and exposes a small Recorder interface so callers
// just invoke Record/Observe/Inc methods without touching the OTel SDK. When no
// MeterProvider is supplied (the safe default, e.g. when no GCP project or
// exporter is configured), a no-op MeterProvider is used so every call becomes a
// cheap no-op and nothing is exported. A Cloud Monitoring exporter can be wired
// into the MeterProvider later without any change to callers.
package dbmetrics

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// instrumentationName is the OTel instrumentation scope for this package.
const instrumentationName = "github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"

// Metric names. Kept as constants so dashboards, alerts, and tests can reference
// the canonical strings.
const (
	MetricPublishToDeliverLatency = "scion.db.notify.publish_to_deliver.duration"
	MetricNotificationsPublished  = "scion.db.notify.published"
	MetricNotificationsDelivered  = "scion.db.notify.delivered"
	MetricNotificationsDropped    = "scion.db.notify.dropped"
	MetricSubscriberLag           = "scion.db.notify.subscriber.lag"
	MetricListenerReconnects      = "scion.db.notify.listener.reconnects"
	MetricPayloadSize             = "scion.db.notify.payload.size"
	MetricPoolConnectionsActive   = "scion.db.pool.connections.active"
	MetricPoolConnectionsIdle     = "scion.db.pool.connections.idle"
	MetricPoolConnectionsWaits    = "scion.db.pool.connections.wait_count"
	MetricPoolConnectionsMax      = "scion.db.pool.connections.max"
)

// AttrDropReason is the attribute key callers set on MetricNotificationsDropped
// to say why a notification was dropped (for example "decode", "refetch" or
// "full_buffer"). Dashboards group drops by it.
const AttrDropReason = "reason"

// AttrPool is the attribute key that names the connection pool a
// scion.db.pool.* point describes. The hub runs two pools, so every pool
// point carries it (ObservePoolStats sets it) or the two pools would write
// the same series.
const AttrPool = "pool"

// Values of AttrPool.
const (
	// PoolStore is the database/sql pool behind the hub store.
	PoolStore = "store"
	// PoolEvents is the pgx pool behind the Postgres event publisher.
	PoolEvents = "events"
)

// Recorder is the interface callers use to record Postgres LISTEN/NOTIFY and
// connection-pool metrics. All methods are safe to call concurrently and are
// cheap no-ops when metrics are disabled.
//
// The LISTEN/NOTIFY event-agent (P3-8) is the primary intended caller.
type Recorder interface {
	// RecordPublishToDeliverLatency records the end-to-end latency, in
	// milliseconds, between a notification being published to Postgres and it
	// being delivered to a subscriber.
	RecordPublishToDeliverLatency(ctx context.Context, ms float64, attrs ...attribute.KeyValue)

	// IncPublished increments the count of notifications published to Postgres.
	IncPublished(ctx context.Context, n int64, attrs ...attribute.KeyValue)
	// IncDelivered increments the count of notifications delivered to subscribers.
	IncDelivered(ctx context.Context, n int64, attrs ...attribute.KeyValue)
	// IncDropped increments the count of notifications dropped (e.g. full buffer,
	// decode failure, no subscriber).
	IncDropped(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	// ObserveSubscriberLag records the current subscriber lag (number of
	// notifications a subscriber is behind, or another caller-defined lag unit).
	ObserveSubscriberLag(ctx context.Context, lag int64, attrs ...attribute.KeyValue)

	// IncListenerReconnects increments the count of LISTEN connection reconnects.
	IncListenerReconnects(ctx context.Context, n int64, attrs ...attribute.KeyValue)

	// RecordPayloadSize records the size, in bytes, of a notification payload.
	RecordPayloadSize(ctx context.Context, bytes int64, attrs ...attribute.KeyValue)

	// ObservePoolStats records a snapshot of the DB connection pool named
	// pool (PoolStore or PoolEvents). The name is exported as the AttrPool
	// attribute so each pool writes its own series.
	ObservePoolStats(ctx context.Context, pool string, stats PoolStats, attrs ...attribute.KeyValue)

	// Enabled reports whether metrics are backed by a real (non-no-op)
	// MeterProvider. Callers may use this to skip building attribute sets when
	// nothing will be recorded.
	Enabled() bool
}

// PoolStats is a snapshot of database connection pool values.
type PoolStats struct {
	Active int64 // connections currently in use
	Idle   int64 // connections open but unused
	// WaitCount is the cumulative number of times a caller had to wait for a
	// connection since the pool opened. Neither database/sql nor pgxpool
	// exposes how many callers are waiting right now. The recorder exports
	// the increase since the previous snapshot of the same pool as a counter.
	WaitCount int64
	Max       int64 // configured maximum pool size
}

// recorder is the OpenTelemetry-backed implementation of Recorder.
type recorder struct {
	enabled bool

	publishToDeliver metric.Float64Histogram
	published        metric.Int64Counter
	delivered        metric.Int64Counter
	dropped          metric.Int64Counter
	subscriberLag    metric.Int64Gauge
	listenerReconn   metric.Int64Counter
	payloadSize      metric.Int64Histogram

	poolActive metric.Int64Gauge
	poolIdle   metric.Int64Gauge
	poolWaits  metric.Int64Counter
	poolMax    metric.Int64Gauge

	// lastWaitCount holds the previous PoolStats.WaitCount per attribute set,
	// so ObservePoolStats can add only the increase to poolWaits.
	waitMu        sync.Mutex
	lastWaitCount map[attribute.Distinct]int64
}

// compile-time check that recorder satisfies Recorder.
var _ Recorder = (*recorder)(nil)

// New creates a Recorder backed by the supplied MeterProvider.
//
// If mp is nil, a no-op MeterProvider is used: instruments still register
// successfully and every Record/Observe/Inc call becomes a cheap no-op. This is
// the safe default when no exporter (e.g. Cloud Monitoring) is configured, such
// as when no GCP project is set.
//
// New returns an error only if instrument registration fails, which should not
// happen with a well-behaved MeterProvider.
func New(mp metric.MeterProvider) (Recorder, error) {
	enabled := mp != nil
	if mp == nil {
		mp = noop.NewMeterProvider()
	}

	meter := mp.Meter(instrumentationName)
	r := &recorder{enabled: enabled, lastWaitCount: map[attribute.Distinct]int64{}}

	var err error

	if r.publishToDeliver, err = meter.Float64Histogram(
		MetricPublishToDeliverLatency,
		metric.WithUnit("ms"),
		metric.WithDescription("Latency from publishing a notification to delivering it to a subscriber"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricPublishToDeliverLatency, err)
	}

	if r.published, err = meter.Int64Counter(
		MetricNotificationsPublished,
		metric.WithUnit("{notification}"),
		metric.WithDescription("Number of notifications published to Postgres"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricNotificationsPublished, err)
	}

	if r.delivered, err = meter.Int64Counter(
		MetricNotificationsDelivered,
		metric.WithUnit("{notification}"),
		metric.WithDescription("Number of notifications delivered to subscribers"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricNotificationsDelivered, err)
	}

	if r.dropped, err = meter.Int64Counter(
		MetricNotificationsDropped,
		metric.WithUnit("{notification}"),
		metric.WithDescription("Number of notifications dropped before delivery"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricNotificationsDropped, err)
	}

	if r.subscriberLag, err = meter.Int64Gauge(
		MetricSubscriberLag,
		metric.WithUnit("{notification}"),
		metric.WithDescription("Current subscriber lag (notifications behind)"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricSubscriberLag, err)
	}

	if r.listenerReconn, err = meter.Int64Counter(
		MetricListenerReconnects,
		metric.WithUnit("{reconnect}"),
		metric.WithDescription("Number of LISTEN connection reconnects"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricListenerReconnects, err)
	}

	if r.payloadSize, err = meter.Int64Histogram(
		MetricPayloadSize,
		metric.WithUnit("By"),
		metric.WithDescription("Size of notification payloads in bytes"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricPayloadSize, err)
	}

	if r.poolActive, err = meter.Int64Gauge(
		MetricPoolConnectionsActive,
		metric.WithUnit("{connection}"),
		metric.WithDescription("Database connections currently in use"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricPoolConnectionsActive, err)
	}

	if r.poolIdle, err = meter.Int64Gauge(
		MetricPoolConnectionsIdle,
		metric.WithUnit("{connection}"),
		metric.WithDescription("Database connections open but idle"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricPoolConnectionsIdle, err)
	}

	if r.poolWaits, err = meter.Int64Counter(
		MetricPoolConnectionsWaits,
		metric.WithUnit("{wait}"),
		metric.WithDescription("Times a caller had to wait for a database connection"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricPoolConnectionsWaits, err)
	}

	if r.poolMax, err = meter.Int64Gauge(
		MetricPoolConnectionsMax,
		metric.WithUnit("{connection}"),
		metric.WithDescription("Configured maximum database pool size"),
	); err != nil {
		return nil, fmt.Errorf("registering %s: %w", MetricPoolConnectionsMax, err)
	}

	return r, nil
}

// NewDisabled returns a Recorder whose calls are all no-ops. It is equivalent to
// New(nil) but never returns an error, which is convenient for tests and for
// call sites that want an explicit disabled recorder.
func NewDisabled() Recorder {
	r, _ := New(nil)
	return r
}

func (r *recorder) Enabled() bool { return r.enabled }

func (r *recorder) RecordPublishToDeliverLatency(ctx context.Context, ms float64, attrs ...attribute.KeyValue) {
	r.publishToDeliver.Record(ctx, ms, metric.WithAttributes(attrs...))
}

func (r *recorder) IncPublished(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.published.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncDelivered(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.delivered.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) IncDropped(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.dropped.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) ObserveSubscriberLag(ctx context.Context, lag int64, attrs ...attribute.KeyValue) {
	r.subscriberLag.Record(ctx, lag, metric.WithAttributes(attrs...))
}

func (r *recorder) IncListenerReconnects(ctx context.Context, n int64, attrs ...attribute.KeyValue) {
	r.listenerReconn.Add(ctx, n, metric.WithAttributes(attrs...))
}

func (r *recorder) RecordPayloadSize(ctx context.Context, bytes int64, attrs ...attribute.KeyValue) {
	r.payloadSize.Record(ctx, bytes, metric.WithAttributes(attrs...))
}

func (r *recorder) ObservePoolStats(ctx context.Context, pool string, stats PoolStats, attrs ...attribute.KeyValue) {
	set := attribute.NewSet(append([]attribute.KeyValue{attribute.String(AttrPool, pool)}, attrs...)...)
	opt := metric.WithAttributeSet(set)
	r.poolActive.Record(ctx, stats.Active, opt)
	r.poolIdle.Record(ctx, stats.Idle, opt)
	r.poolMax.Record(ctx, stats.Max, opt)
	if delta := r.waitCountDelta(set.Equivalent(), stats.WaitCount); delta > 0 {
		r.poolWaits.Add(ctx, delta, opt)
	}
}

// waitCountDelta returns how much the cumulative wait count of the pool
// identified by key grew since its previous snapshot, and remembers cur. The
// first snapshot of a pool counts in full: the pool and this process start
// together. A count that went down means the pool was replaced, so cur is
// all new.
func (r *recorder) waitCountDelta(key attribute.Distinct, cur int64) int64 {
	r.waitMu.Lock()
	defer r.waitMu.Unlock()
	prev, seen := r.lastWaitCount[key]
	r.lastWaitCount[key] = cur
	if !seen || cur < prev {
		return cur
	}
	return cur - prev
}
