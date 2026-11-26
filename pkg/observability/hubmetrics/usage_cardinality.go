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

// EnvHubOTelCardinalityLimit configures the OTel SDK per-instrument cardinality
// limit applied across all Hub metric instruments on the Hub MeterProvider
// (metric.WithCardinalityLimit).
//
// Cumulative counters retain every observed attribute set in memory until Hub
// process restart; the OTel Go SDK default of 2000 overflows agent-level usage
// counters (gen_ai.api.calls, scion.usage.tokens) at ~400 (agent, model) pairs
// and folds excess series into otel.metric.overflow=true.
//
// Values below MinHubOTelCardinalityLimit (2000, the OTel SDK default) are
// rejected so configuring this knob can never lower other Hub instruments below
// the SDK default. Default: 20000 (max 250000). Because the limit applies to
// every instrument on the MeterProvider, worst-case memory scales with both the
// usage counters and any other high-cardinality Hub instruments; large
// multi-tenant fleets should set SCION_HUB_USAGE_OTEL_CARDINALITY=project.
const (
	EnvHubOTelCardinalityLimit     = "SCION_HUB_OTEL_CARDINALITY_LIMIT"
	MinHubOTelCardinalityLimit     = 2000
	DefaultHubOTelCardinalityLimit = 20000
	MaxHubOTelCardinalityLimit     = 250000

	hubUsageInstrumentationScope = "github.com/GoogleCloudPlatform/scion/pkg/hub"
	otelOverflowAttrKey          = attribute.Key("otel.metric.overflow")
	overflowWarnInterval         = 5 * time.Minute
)

// ParseHubOTelCardinalityLimit parses a bounded integer cardinality limit in
// [MinHubOTelCardinalityLimit, MaxHubOTelCardinalityLimit]. Empty input returns
// DefaultHubOTelCardinalityLimit with ok=true. Invalid, non-numeric, or
// out-of-range values (including values below the OTel SDK default of 2000)
// return DefaultHubOTelCardinalityLimit with ok=false.
func ParseHubOTelCardinalityLimit(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultHubOTelCardinalityLimit, true
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < MinHubOTelCardinalityLimit || n > MaxHubOTelCardinalityLimit {
		return DefaultHubOTelCardinalityLimit, false
	}
	return n, true
}

// HubOTelCardinalityLimitFromEnv reads EnvHubOTelCardinalityLimit, logging a
// warning and falling back to DefaultHubOTelCardinalityLimit on invalid or
// out-of-range values (including values below MinHubOTelCardinalityLimit).
func HubOTelCardinalityLimitFromEnv() int {
	raw := os.Getenv(EnvHubOTelCardinalityLimit)
	limit, ok := ParseHubOTelCardinalityLimit(raw)
	if !ok {
		slog.Warn("hub metrics: invalid OTel cardinality limit; falling back to default",
			"env", EnvHubOTelCardinalityLimit,
			"value", raw,
			"min", MinHubOTelCardinalityLimit,
			"default", limit,
			"max", MaxHubOTelCardinalityLimit,
		)
	}
	return limit
}

// WithCardinalityLimit overrides the MeterProvider-wide OTel SDK cardinality
// limit applied to all Hub metric instruments. Values <= 0 fall back to
// HubOTelCardinalityLimitFromEnv(). Values in (0, MinHubOTelCardinalityLimit)
// are clamped up to MinHubOTelCardinalityLimit so other Hub instruments can
// never be lowered below the SDK default; values above
// MaxHubOTelCardinalityLimit are clamped to MaxHubOTelCardinalityLimit.
func WithCardinalityLimit(limit int) Option {
	return func(o *options) {
		if limit <= 0 {
			return
		}
		if limit < MinHubOTelCardinalityLimit {
			limit = MinHubOTelCardinalityLimit
		}
		if limit > MaxHubOTelCardinalityLimit {
			limit = MaxHubOTelCardinalityLimit
		}
		o.cardinalityLimit = limit
	}
}

// HubCardinalityLimitOptions returns the OTel SDK MeterProvider options that
// configure the provider-wide per-instrument cardinality limit
// (metric.WithCardinalityLimit) for all Hub metric instruments. Production
// NewMeterProvider attaches these options automatically.
func HubCardinalityLimitOptions(limit int) []metric.Option {
	if limit < MinHubOTelCardinalityLimit || limit > MaxHubOTelCardinalityLimit {
		limit = DefaultHubOTelCardinalityLimit
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
		slog.Warn("hub metrics: usage OTel counter hit SDK cardinality limit (otel.metric.overflow=true); raise SCION_HUB_OTEL_CARDINALITY_LIMIT or switch SCION_HUB_USAGE_OTEL_CARDINALITY=project",
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
