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

	"go.opentelemetry.io/otel/metric"
)

// OTelGCPTokenMetrics implements GCPTokenMetricsRecorder using OTel
// instruments for Cloud Monitoring export. It embeds a GCPTokenMetrics for
// the /api/metrics JSON snapshot endpoint (dual-write).
type OTelGCPTokenMetrics struct {
	accessRequests   metric.Int64Counter
	accessSuccesses  metric.Int64Counter
	accessFailures   metric.Int64Counter
	idRequests       metric.Int64Counter
	idSuccesses      metric.Int64Counter
	idFailures       metric.Int64Counter
	rateLimitRejects metric.Int64Counter
	iamDuration      metric.Float64Histogram

	snap *GCPTokenMetrics
}

var _ GCPTokenMetricsRecorder = (*OTelGCPTokenMetrics)(nil)

// NewOTelGCPTokenMetrics creates an OTel-backed GCP token metrics recorder.
func NewOTelGCPTokenMetrics(mp metric.MeterProvider) (*OTelGCPTokenMetrics, error) {
	m := mp.Meter(instrumentationScope)
	r := &OTelGCPTokenMetrics{snap: NewGCPTokenMetrics()}

	var err error

	if r.accessRequests, err = m.Int64Counter("scion.hub.gcp.token.access.requests",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.access.requests counter: %w", err)
	}
	if r.accessSuccesses, err = m.Int64Counter("scion.hub.gcp.token.access.successes",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.access.successes counter: %w", err)
	}
	if r.accessFailures, err = m.Int64Counter("scion.hub.gcp.token.access.failures",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.access.failures counter: %w", err)
	}
	if r.idRequests, err = m.Int64Counter("scion.hub.gcp.token.identity.requests",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.identity.requests counter: %w", err)
	}
	if r.idSuccesses, err = m.Int64Counter("scion.hub.gcp.token.identity.successes",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.identity.successes counter: %w", err)
	}
	if r.idFailures, err = m.Int64Counter("scion.hub.gcp.token.identity.failures",
		metric.WithUnit("{request}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.identity.failures counter: %w", err)
	}
	if r.rateLimitRejects, err = m.Int64Counter("scion.hub.gcp.token.ratelimit.rejections",
		metric.WithUnit("{rejection}"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.token.ratelimit.rejections counter: %w", err)
	}
	if r.iamDuration, err = m.Float64Histogram("scion.hub.gcp.iam.duration",
		metric.WithUnit("ms"),
	); err != nil {
		return nil, fmt.Errorf("creating gcp.iam.duration histogram: %w", err)
	}

	return r, nil
}

func (r *OTelGCPTokenMetrics) RecordAccessTokenRequest(success bool, latency time.Duration) {
	ctx := context.Background()
	r.accessRequests.Add(ctx, 1)
	if success {
		r.accessSuccesses.Add(ctx, 1)
	} else {
		r.accessFailures.Add(ctx, 1)
	}
	r.iamDuration.Record(ctx, float64(latency.Milliseconds()))
	r.snap.RecordAccessTokenRequest(success, latency)
}

func (r *OTelGCPTokenMetrics) RecordIDTokenRequest(success bool, latency time.Duration) {
	ctx := context.Background()
	r.idRequests.Add(ctx, 1)
	if success {
		r.idSuccesses.Add(ctx, 1)
	} else {
		r.idFailures.Add(ctx, 1)
	}
	r.iamDuration.Record(ctx, float64(latency.Milliseconds()))
	r.snap.RecordIDTokenRequest(success, latency)
}

func (r *OTelGCPTokenMetrics) RecordRateLimitRejection() {
	r.rateLimitRejects.Add(context.Background(), 1)
	r.snap.RecordRateLimitRejection()
}

func (r *OTelGCPTokenMetrics) GetSnapshot() *GCPTokenMetricsSnapshot {
	return r.snap.GetSnapshot()
}
