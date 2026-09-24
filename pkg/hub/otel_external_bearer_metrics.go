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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OTelExternalBearerMetrics implements ExternalBearerMetricsRecorder,
// GoogleValidatorCacheMetricsRecorder and GEExchangeMetricsRecorder using
// OTel instruments exported to Cloud Monitoring, following the same
// registration convention as OTelMetricsRecorder and OTelGCPTokenMetrics:
// one Int64Counter per metric, registered once under the shared
// instrumentationScope, with the label set passed as attributes on each Add
// call rather than as separate counters. One recorder backs all three
// counters because they are wired together (server.go) and retired together,
// once ge_exchange's soak completes.
//
// Export only happens through pkg/observability/hubmetrics (mexporter) to
// GCP Cloud Monitoring, and only when cfg.Hub.GCPProjectID is set
// (cmd/server_foreground.go); there is no Prometheus exporter anywhere in
// this binary. The instrument names below (scion.hub.external_bearer,
// scion.hub.google_validator_cache, scion.hub.ge_exchange.requests) become,
// in Cloud Monitoring, the metric types workload.googleapis.com/scion.hub.
// external_bearer, …/scion.hub.google_validator_cache and
// …/scion.hub.ge_exchange.requests. Every Hub also serves these three
// counters, aggregated across labels, in the in-process /metrics JSON
// (embedded snap; see ExternalBearerSnapshotMetrics), which exists
// regardless of GCP export.
type OTelExternalBearerMetrics struct {
	externalBearerTotal metric.Int64Counter
	validatorCacheTotal metric.Int64Counter
	geExchangeTotal     metric.Int64Counter

	// snap dual-writes every Record* call into the same in-process snapshot
	// Server.New wires as the default recorder, so /metrics keeps counting
	// across the switch from the default recorder to this OTel-backed one
	// (see server.go's Set*Metrics methods).
	snap *ExternalBearerSnapshotMetrics
}

var (
	_ ExternalBearerMetricsRecorder       = (*OTelExternalBearerMetrics)(nil)
	_ GoogleValidatorCacheMetricsRecorder = (*OTelExternalBearerMetrics)(nil)
	_ GEExchangeMetricsRecorder           = (*OTelExternalBearerMetrics)(nil)
)

// NewOTelExternalBearerMetrics creates an OTel-backed recorder for all three
// counters. snap receives a dual-write of every Record* call;
// callers should pass the same instance already wired as Server's default
// recorder (Server.ExternalBearerSnapshotMetrics), so /metrics keeps
// counting the same totals before and after this recorder is wired in.
func NewOTelExternalBearerMetrics(mp metric.MeterProvider, snap *ExternalBearerSnapshotMetrics) (*OTelExternalBearerMetrics, error) {
	m := mp.Meter(instrumentationScope)
	r := &OTelExternalBearerMetrics{snap: snap}

	var err error
	if r.externalBearerTotal, err = m.Int64Counter("scion.hub.external_bearer",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating external_bearer counter: %w", err)
	}
	if r.validatorCacheTotal, err = m.Int64Counter("scion.hub.google_validator_cache",
		metric.WithUnit("{lookup}"),
	); err != nil {
		return nil, fmt.Errorf("creating google_validator_cache counter: %w", err)
	}
	if r.geExchangeTotal, err = m.Int64Counter("scion.hub.ge_exchange.requests",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating ge_exchange.requests counter: %w", err)
	}

	return r, nil
}

// RecordExternalBearer implements ExternalBearerMetricsRecorder.
func (r *OTelExternalBearerMetrics) RecordExternalBearer(kind ExternalBearerKind, principal ExternalBearerPrincipal, outcome ExternalBearerOutcome) {
	r.externalBearerTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("kind", string(kind)),
		attribute.String("principal", string(principal)),
		attribute.String("outcome", string(outcome)),
	))
	if r.snap != nil {
		r.snap.RecordExternalBearer(kind, principal, outcome)
	}
}

// RecordGoogleValidatorCache implements GoogleValidatorCacheMetricsRecorder.
func (r *OTelExternalBearerMetrics) RecordGoogleValidatorCache(result GoogleValidatorCacheResult) {
	r.validatorCacheTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("result", string(result)),
	))
	if r.snap != nil {
		r.snap.RecordGoogleValidatorCache(result)
	}
}

// RecordGEExchangeRequest implements GEExchangeMetricsRecorder.
func (r *OTelExternalBearerMetrics) RecordGEExchangeRequest(outcome GEExchangeOutcome) {
	r.geExchangeTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", string(outcome)),
	))
	if r.snap != nil {
		r.snap.RecordGEExchangeRequest(outcome)
	}
}
