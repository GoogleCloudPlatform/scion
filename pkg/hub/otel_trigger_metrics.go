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

package hub

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OpenTelemetry metric instrument names for durable queues, trigger ingress,
// trigger fires, and the firing-row reaper (Stage 1 §6.5). Exported via
// pkg/observability/hubmetrics when GCP export is enabled, and dual-written
// to TriggerSnapshotMetrics for GET /metrics.
const (
	MetricDurableQueueItems     = "scion.hub.durablequeue.items"
	MetricDurableQueuePending   = "scion.hub.durablequeue.pending"
	MetricDurableQueueLeased    = "scion.hub.durablequeue.leased"
	MetricDurableQueueOldestAge = "scion.hub.durablequeue.oldest_age"

	MetricTriggerEvents            = "scion.hub.trigger.events"
	MetricTriggerEventPayloadBytes = "scion.hub.trigger.event.payload_bytes"
	MetricTriggerFires             = "scion.hub.trigger.fires"
	MetricTriggerReaperReaped      = "scion.hub.trigger.reaper.reaped"
	MetricTriggerReaperRuns        = "scion.hub.trigger.reaper.runs"
	MetricTriggerReaperLastRun     = "scion.hub.trigger.reaper.last_run"
)

// OTelTriggerMetrics implements TriggerMetricsRecorder using OpenTelemetry
// instruments registered under instrumentationScope, dual-writing every
// observation to an optional TriggerSnapshotMetrics.
type OTelTriggerMetrics struct {
	queueItems     metric.Int64Counter
	queuePending   metric.Int64Gauge
	queueLeased    metric.Int64Gauge
	queueOldestAge metric.Float64Gauge

	ingressEvents       metric.Int64Counter
	ingressPayloadBytes metric.Int64Histogram
	triggerFires        metric.Int64Counter

	reaperReaped  metric.Int64Counter
	reaperRuns    metric.Int64Counter
	reaperLastRun metric.Float64Gauge

	snap *TriggerSnapshotMetrics
}

var _ TriggerMetricsRecorder = (*OTelTriggerMetrics)(nil)

// NewOTelTriggerMetrics registers the durable queue, trigger ingress, trigger
// fire, and firing-row reaper OTel instruments on mp. Pass
// srv.TriggerSnapshotMetrics() as snap so /metrics keeps counting across the
// switch from the default in-process recorder to OTel.
func NewOTelTriggerMetrics(mp metric.MeterProvider, snap *TriggerSnapshotMetrics) (*OTelTriggerMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("otel trigger metrics: nil MeterProvider")
	}
	m := mp.Meter(instrumentationScope)
	r := &OTelTriggerMetrics{snap: snap}

	var err error
	if r.queueItems, err = m.Int64Counter(MetricDurableQueueItems,
		metric.WithUnit("{item}"),
		metric.WithDescription("Durable queue items by kind and outcome"),
	); err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", MetricDurableQueueItems, err)
	}
	if r.queuePending, err = m.Int64Gauge(MetricDurableQueuePending,
		metric.WithUnit("{item}"),
		metric.WithDescription("Current pending item count per durable queue kind"),
	); err != nil {
		return nil, fmt.Errorf("creating %s gauge: %w", MetricDurableQueuePending, err)
	}
	if r.queueLeased, err = m.Int64Gauge(MetricDurableQueueLeased,
		metric.WithUnit("{item}"),
		metric.WithDescription("Current leased item count per durable queue kind"),
	); err != nil {
		return nil, fmt.Errorf("creating %s gauge: %w", MetricDurableQueueLeased, err)
	}
	if r.queueOldestAge, err = m.Float64Gauge(MetricDurableQueueOldestAge,
		metric.WithUnit("s"),
		metric.WithDescription("Age in seconds of the oldest pending item per durable queue kind"),
	); err != nil {
		return nil, fmt.Errorf("creating %s gauge: %w", MetricDurableQueueOldestAge, err)
	}
	if r.ingressEvents, err = m.Int64Counter(MetricTriggerEvents,
		metric.WithUnit("{event}"),
		metric.WithDescription("Trigger ingress events by source_kind and result"),
	); err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", MetricTriggerEvents, err)
	}
	if r.ingressPayloadBytes, err = m.Int64Histogram(MetricTriggerEventPayloadBytes,
		metric.WithUnit("By"),
		metric.WithDescription("Accepted trigger ingress payload size in bytes"),
	); err != nil {
		return nil, fmt.Errorf("creating %s histogram: %w", MetricTriggerEventPayloadBytes, err)
	}
	if r.triggerFires, err = m.Int64Counter(MetricTriggerFires,
		metric.WithUnit("{fire}"),
		metric.WithDescription("Trigger fires by action, status, and skip reason"),
	); err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", MetricTriggerFires, err)
	}
	if r.reaperReaped, err = m.Int64Counter(MetricTriggerReaperReaped,
		metric.WithUnit("{fire}"),
		metric.WithDescription("Abandoned firing rows reaped by trigger maintenance"),
	); err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", MetricTriggerReaperReaped, err)
	}
	if r.reaperRuns, err = m.Int64Counter(MetricTriggerReaperRuns,
		metric.WithUnit("{run}"),
		metric.WithDescription("Trigger firing-row reaper runs by outcome"),
	); err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", MetricTriggerReaperRuns, err)
	}
	if r.reaperLastRun, err = m.Float64Gauge(MetricTriggerReaperLastRun,
		metric.WithUnit("s"),
		metric.WithDescription("Unix timestamp in seconds of the last trigger firing-row reaper run"),
	); err != nil {
		return nil, fmt.Errorf("creating %s gauge: %w", MetricTriggerReaperLastRun, err)
	}
	return r, nil
}

func (r *OTelTriggerMetrics) addQueueItems(kind, outcome string, n int64) {
	r.queueItems.Add(context.Background(), n, metric.WithAttributes(
		attribute.String("kind", normalizeQueueKindLabel(kind)),
		attribute.String("outcome", outcome),
	))
}

// Enqueued implements durablequeue.QueueMetrics.
func (r *OTelTriggerMetrics) Enqueued(kind string) {
	r.addQueueItems(kind, QueueOutcomeEnqueued, 1)
	if r.snap != nil {
		r.snap.Enqueued(kind)
	}
}

// Claimed implements durablequeue.Metrics.
func (r *OTelTriggerMetrics) Claimed(kind string, n int) {
	if n <= 0 {
		return
	}
	r.addQueueItems(kind, QueueOutcomeClaimed, int64(n))
	if r.snap != nil {
		r.snap.Claimed(kind, n)
	}
}

// Done implements durablequeue.Metrics.
func (r *OTelTriggerMetrics) Done(kind string) {
	r.addQueueItems(kind, QueueOutcomeCompleted, 1)
	if r.snap != nil {
		r.snap.Done(kind)
	}
}

// Retried implements durablequeue.Metrics.
func (r *OTelTriggerMetrics) Retried(kind string) {
	r.addQueueItems(kind, QueueOutcomeRetried, 1)
	if r.snap != nil {
		r.snap.Retried(kind)
	}
}

// Permanent implements durablequeue.Metrics.
func (r *OTelTriggerMetrics) Permanent(kind string) {
	r.addQueueItems(kind, QueueOutcomePermanent, 1)
	if r.snap != nil {
		r.snap.Permanent(kind)
	}
}

// Dropped implements durablequeue.Metrics.
func (r *OTelTriggerMetrics) Dropped(kind, reason string, n int) {
	if n <= 0 {
		return
	}
	r.addQueueItems(kind, normalizeQueueDropOutcome(reason), int64(n))
	if r.snap != nil {
		r.snap.Dropped(kind, reason, n)
	}
}

// ObserveQueue implements durablequeue.Metrics.
func (r *OTelTriggerMetrics) ObserveQueue(kind string, pending, leased int64, oldestAge time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("kind", normalizeQueueKindLabel(kind)))
	r.queuePending.Record(ctx, pending, attrs)
	r.queueLeased.Record(ctx, leased, attrs)
	ageSec := 0.0
	if oldestAge > 0 {
		ageSec = oldestAge.Seconds()
	}
	r.queueOldestAge.Record(ctx, ageSec, attrs)
	if r.snap != nil {
		r.snap.ObserveQueue(kind, pending, leased, oldestAge)
	}
}

// RecordIngressEvent implements trigger.IngressMetrics.
func (r *OTelTriggerMetrics) RecordIngressEvent(sourceKind, result string) {
	r.ingressEvents.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("source_kind", normalizeTriggerSourceKindLabel(sourceKind)),
		attribute.String("result", normalizeTriggerIngressResultLabel(result)),
	))
	if r.snap != nil {
		r.snap.RecordIngressEvent(sourceKind, result)
	}
}

// ObserveIngressPayloadBytes implements trigger.IngressPayloadMetrics.
func (r *OTelTriggerMetrics) ObserveIngressPayloadBytes(sourceKind string, bytes int) {
	if bytes < 0 {
		bytes = 0
	}
	r.ingressPayloadBytes.Record(context.Background(), int64(bytes), metric.WithAttributes(
		attribute.String("source_kind", normalizeTriggerSourceKindLabel(sourceKind)),
	))
	if r.snap != nil {
		r.snap.ObserveIngressPayloadBytes(sourceKind, bytes)
	}
}

// RecordTriggerFire implements trigger.DispatcherMetrics.
func (r *OTelTriggerMetrics) RecordTriggerFire(actionType, status, skipReason string) {
	st := normalizeTriggerFireStatusLabel(status)
	r.triggerFires.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("action", normalizeTriggerActionLabel(actionType)),
		attribute.String("status", st),
		attribute.String("reason", normalizeTriggerFireSkipReasonLabel(st, skipReason)),
	))
	if r.snap != nil {
		r.snap.RecordTriggerFire(actionType, status, skipReason)
	}
}

// RecordReaperRun implements TriggerMetricsRecorder.
func (r *OTelTriggerMetrics) RecordReaperRun(reaped int, at time.Time, err error) {
	ctx := context.Background()
	if reaped > 0 {
		r.reaperReaped.Add(ctx, int64(reaped))
	}
	outcome := TriggerReaperOutcomeOK
	if err != nil {
		outcome = TriggerReaperOutcomeError
	}
	r.reaperRuns.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
	))
	if !at.IsZero() {
		r.reaperLastRun.Record(ctx, float64(at.UTC().Unix()))
	}
	if r.snap != nil {
		r.snap.RecordReaperRun(reaped, at, err)
	}
}
