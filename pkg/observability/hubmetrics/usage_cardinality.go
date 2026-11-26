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

package hubmetrics

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// EnvHubUsageOTelCardinalityLimit configures the per-instrument OTel SDK
// cardinality limit (AggregationLimit) for Hub-re-emitted canonical usage
// counters (gen_ai.api.calls and scion.usage.tokens). Cumulative counters keep
// every observed attribute set in memory until process restart; the OTel SDK
// default of 2000 overflows at ~400 (agent, model) pairs and drops attribution
// labels into otel.metric.overflow=true.
//
// Default: 20000 attribute sets per instrument (~4,000 (agent, model) pairs ×
// 5 token_types per replica lifetime, ~8-16 MiB bounded RAM across both
// counters). Large multi-tenant fleets should set
// SCION_HUB_USAGE_OTEL_CARDINALITY=project.
const (
	EnvHubUsageOTelCardinalityLimit  = "SCION_HUB_USAGE_OTEL_CARDINALITY_LIMIT"
	DefaultUsageOTelCardinalityLimit = 20000
	MaxUsageOTelCardinalityLimit     = 250000

	hubUsageInstrumentationScope = "github.com/GoogleCloudPlatform/scion/pkg/hub"
	otelOverflowAttrKey          = attribute.Key("otel.metric.overflow")
	overflowWarnInterval         = 5 * time.Minute
)

// ParseUsageOTelCardinalityLimit parses a bounded positive integer cardinality
// limit in [1, MaxUsageOTelCardinalityLimit]. Empty input returns
// DefaultUsageOTelCardinalityLimit with ok=true. Invalid or out-of-range values
// return DefaultUsageOTelCardinalityLimit with ok=false.
func ParseUsageOTelCardinalityLimit(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultUsageOTelCardinalityLimit, true
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 1 || n > MaxUsageOTelCardinalityLimit {
		return DefaultUsageOTelCardinalityLimit, false
	}
	return n, true
}

// UsageOTelCardinalityLimitFromEnv reads EnvHubUsageOTelCardinalityLimit,
// logging a warning and falling back to DefaultUsageOTelCardinalityLimit on
// invalid or out-of-range values.
func UsageOTelCardinalityLimitFromEnv() int {
	raw := os.Getenv(EnvHubUsageOTelCardinalityLimit)
	limit, ok := ParseUsageOTelCardinalityLimit(raw)
	if !ok {
		slog.Warn("hub metrics: invalid usage OTel cardinality limit; falling back to default",
			"env", EnvHubUsageOTelCardinalityLimit,
			"value", raw,
			"default", limit,
			"max", MaxUsageOTelCardinalityLimit,
		)
	}
	return limit
}

// WithUsageCardinalityLimit overrides the per-instrument AggregationLimit for
// Hub-re-emitted gen_ai.api.calls and scion.usage.tokens counters. Values <= 0
// fall back to UsageOTelCardinalityLimitFromEnv().
func WithUsageCardinalityLimit(limit int) Option {
	return func(o *options) {
		if limit > 0 {
			if limit > MaxUsageOTelCardinalityLimit {
				limit = MaxUsageOTelCardinalityLimit
			}
			o.usageCardinalityLimit = limit
		}
	}
}

// UsageMetricViews returns the OTel SDK MeterProvider options that configure
// the per-instrument cardinality limit (metric.WithCardinalityLimit) for
// Hub-re-emitted gen_ai.api.calls and scion.usage.tokens counters. Production
// NewMeterProvider attaches these options automatically; tests using
// ManualReader can pass UsageMetricViews(...) to build a provider identically
// to production.
func UsageMetricViews(limit int) []metric.Option {
	if limit <= 0 {
		limit = DefaultUsageOTelCardinalityLimit
	}
	if limit > MaxUsageOTelCardinalityLimit {
		limit = MaxUsageOTelCardinalityLimit
	}
	return []metric.Option{
		metric.WithCardinalityLimit(limit),
	}
}

type usageOverflowTracker struct {
	mu       sync.Mutex
	lastWarn time.Time
	count    atomic.Int64
	now      func() time.Time
}

func (t *usageOverflowTracker) observe(rm *metricdata.ResourceMetrics) {
	if rm == nil {
		return
	}
	metricName, overflowVal := detectUsageOverflow(rm)
	if metricName == "" {
		return
	}
	total := t.count.Add(1)
	nowFn := t.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	t.mu.Lock()
	shouldWarn := t.lastWarn.IsZero() || now.Sub(t.lastWarn) >= overflowWarnInterval
	if shouldWarn {
		t.lastWarn = now
	}
	t.mu.Unlock()
	if shouldWarn {
		slog.Warn("hub metrics: usage OTel counter hit SDK cardinality limit (otel.metric.overflow=true); raise SCION_HUB_USAGE_OTEL_CARDINALITY_LIMIT or switch SCION_HUB_USAGE_OTEL_CARDINALITY=project",
			"metric", metricName,
			"overflow_value", overflowVal,
			"overflow_exports", total,
		)
	}
}

// detectUsageOverflow inspects an exported ResourceMetrics batch for
// gen_ai.api.calls or scion.usage.tokens points carrying
// otel.metric.overflow=true.
func detectUsageOverflow(rm *metricdata.ResourceMetrics) (string, int64) {
	if rm == nil {
		return "", 0
	}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != hubUsageInstrumentationScope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != telemetrycontract.MetricAPICalls && m.Name != telemetrycontract.MetricUsageTokens {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if val, exists := dp.Attributes.Value(otelOverflowAttrKey); exists && val.AsBool() {
					return m.Name, dp.Value
				}
			}
		}
	}
	return "", 0
}
