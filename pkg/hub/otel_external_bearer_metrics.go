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
// OTel instruments for Cloud Monitoring / Prometheus export, following the
// same registration convention as OTelMetricsRecorder and
// OTelGCPTokenMetrics: one Int64Counter per metric, registered once under
// the shared instrumentationScope, with the label set passed as attributes
// on each Add call rather than as separate counters. One recorder backs all
// three counters because they are wired together (server.go) and retired
// together (design §5's "Later" section, once ge_exchange's soak completes).
//
// Instrument names use dot/underscore segments so that OTel's Prometheus
// bridge (which lower-cases and joins on "_", then appends "_total" to a
// monotonic sum) produces exactly the names design §4.7 specifies:
// scion_hub_external_bearer_total, scion_hub_google_validator_cache_total,
// and scion_hub_ge_exchange_requests_total.
type OTelExternalBearerMetrics struct {
	externalBearerTotal metric.Int64Counter
	validatorCacheTotal metric.Int64Counter
	geExchangeTotal     metric.Int64Counter
}

var (
	_ ExternalBearerMetricsRecorder       = (*OTelExternalBearerMetrics)(nil)
	_ GoogleValidatorCacheMetricsRecorder = (*OTelExternalBearerMetrics)(nil)
	_ GEExchangeMetricsRecorder           = (*OTelExternalBearerMetrics)(nil)
)

// NewOTelExternalBearerMetrics creates an OTel-backed recorder for all three
// design §4.7 counters.
func NewOTelExternalBearerMetrics(mp metric.MeterProvider) (*OTelExternalBearerMetrics, error) {
	m := mp.Meter(instrumentationScope)
	r := &OTelExternalBearerMetrics{}

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
}

// RecordGoogleValidatorCache implements GoogleValidatorCacheMetricsRecorder.
func (r *OTelExternalBearerMetrics) RecordGoogleValidatorCache(result GoogleValidatorCacheResult) {
	r.validatorCacheTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("result", string(result)),
	))
}

// RecordGEExchangeRequest implements GEExchangeMetricsRecorder.
func (r *OTelExternalBearerMetrics) RecordGEExchangeRequest(outcome GEExchangeOutcome) {
	r.geExchangeTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", string(outcome)),
	))
}
