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
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// EnvHubUsageOTelCardinality controls the attribution label cardinality of
// Hub-re-emitted canonical usage counters (Tokenomics design §10 Q8).
// Supported values:
//   - "agent" (default): stamps scion_project_id, scion_agent_id, scion_agent_slug.
//   - "project": stamps scion_project_id only (project-level OTel aggregates;
//     per-agent views are served from Hub DB rollups).
const EnvHubUsageOTelCardinality = "SCION_HUB_USAGE_OTEL_CARDINALITY"

// MetricUsageMeterStale is the observable gauge reporting the count of active
// agent runs with stale Track 1 usage meters per project. Every Hub replica
// reports the same per-project value from the shared database, so dashboards
// must aggregate across service.instance.id using max (not sum).
const MetricUsageMeterStale = "scion.usage.meter_stale"

const (
	defaultMeterStaleCacheTTL     = 30 * time.Second
	defaultMeterStaleQueryTimeout = 5 * time.Second
	// defaultMeterStaleZeroHold keeps a recovered project's 0 observation in
	// the gauge snapshot for two full 60s export intervals so independent GCP
	// and OTLP periodic readers both observe the return to 0 before the
	// project is pruned.
	defaultMeterStaleZeroHold = 2 * time.Minute
)

// UsageOTelCardinality controls which attribution labels the Hub stamps on
// re-emitted gen_ai.api.calls and scion.usage.tokens counters.
type UsageOTelCardinality string

const (
	// UsageOTelCardinalityAgent stamps scion_project_id, scion_agent_id, and
	// scion_agent_slug from run_attribution (default; preserves full parity
	// with CloudMonitoringSource per-agent queries and UniqueAgents).
	UsageOTelCardinalityAgent UsageOTelCardinality = "agent"

	// UsageOTelCardinalityProject stamps scion_project_id only, reducing
	// active Cloud Monitoring / OTLP time-series cardinality on large hubs
	// while per-agent views remain available from Hub DB rollups.
	UsageOTelCardinalityProject UsageOTelCardinality = "project"
)

// ParseUsageOTelCardinality parses a cardinality mode string ("agent" or
// "project"). Empty input defaults to UsageOTelCardinalityAgent with ok=true.
// Unrecognized non-empty input returns UsageOTelCardinalityAgent with ok=false.
func ParseUsageOTelCardinality(raw string) (UsageOTelCardinality, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(UsageOTelCardinalityAgent):
		return UsageOTelCardinalityAgent, true
	case string(UsageOTelCardinalityProject):
		return UsageOTelCardinalityProject, true
	default:
		return UsageOTelCardinalityAgent, false
	}
}

// UsageOTelCardinalityFromEnv reads EnvHubUsageOTelCardinality, logging a
// warning and falling back to UsageOTelCardinalityAgent on unknown values.
func UsageOTelCardinalityFromEnv() UsageOTelCardinality {
	raw := os.Getenv(EnvHubUsageOTelCardinality)
	mode, ok := ParseUsageOTelCardinality(raw)
	if !ok {
		slog.Warn("hub usage OTel: unrecognized cardinality setting; falling back to default",
			"env", EnvHubUsageOTelCardinality, "value", raw, "default", mode)
	}
	return mode
}

// staleMeterCounter is the narrow store surface required to compute
// scion.usage.meter_stale{project}.
type staleMeterCounter interface {
	DBNow(ctx context.Context) (time.Time, error)
	CountStaleMetersByProject(ctx context.Context, staleBefore time.Time) (map[string]int64, error)
}

// UsageOTelOption configures NewUsageOTelMetrics.
type UsageOTelOption func(*usageOTelConfig)

type usageOTelConfig struct {
	cardinality UsageOTelCardinality
	hasSink     bool
	staleStore  staleMeterCounter
	enabledFn   func() bool
	cacheTTL    time.Duration
	zeroHold    time.Duration
	now         func() time.Time
}

// WithUsageOTelCardinality sets the attribution label cardinality mode.
func WithUsageOTelCardinality(c UsageOTelCardinality) UsageOTelOption {
	return func(cfg *usageOTelConfig) {
		if c == UsageOTelCardinalityAgent || c == UsageOTelCardinalityProject {
			cfg.cardinality = c
		}
	}
}

// WithUsageOTelActiveSink marks whether the underlying MeterProvider has at
// least one constructed and active export sink (Cloud Monitoring and/or OTLP).
// Capability probes only advertise suppress_direct_otel=true when this is true.
func WithUsageOTelActiveSink(active bool) UsageOTelOption {
	return func(cfg *usageOTelConfig) {
		cfg.hasSink = active
	}
}

// WithUsageOTelStaleStore wires the store used to compute the
// scion.usage.meter_stale{project} observable gauge.
func WithUsageOTelStaleStore(st staleMeterCounter) UsageOTelOption {
	return func(cfg *usageOTelConfig) {
		cfg.staleStore = st
	}
}

// WithUsageOTelEnabled gates the scion.usage.meter_stale observable gauge
// callback on the tokenomics.usage_ledger experiment (D2). When fn returns
// false, the callback skips DB queries and records no observations.
func WithUsageOTelEnabled(fn func() bool) UsageOTelOption {
	return func(cfg *usageOTelConfig) {
		cfg.enabledFn = fn
	}
}

// UsageOTelMetrics re-emits canonical Track 1 usage counters
// (gen_ai.api.calls, scion.usage.tokens) from committed usage_events rows and
// exposes the scion.usage.meter_stale{project} observable gauge on the Hub's
// MeterProvider (Tokenomics design §4.1 item 2).
type UsageOTelMetrics struct {
	apiCalls    metric.Int64Counter
	usageTokens metric.Int64Counter
	meterStale  metric.Int64ObservableGauge
	reg         metric.Registration

	cardinality UsageOTelCardinality
	hasSink     bool
	enabledFn   func() bool

	staleStore staleMeterCounter
	cacheTTL   time.Duration
	zeroHold   time.Duration
	now        func() time.Time

	staleMu        sync.Mutex
	staleFetchedAt time.Time
	staleCache     map[string]int64
	staleProjects  map[string]struct{}
	recoveredUntil map[string]time.Time
}

// NewUsageOTelMetrics registers the canonical usage counters and optional
// scion.usage.meter_stale observable gauge on mp.
func NewUsageOTelMetrics(mp metric.MeterProvider, opts ...UsageOTelOption) (*UsageOTelMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("meter provider is required")
	}

	cfg := usageOTelConfig{
		cardinality: UsageOTelCardinalityAgent,
		cacheTTL:    defaultMeterStaleCacheTTL,
		zeroHold:    defaultMeterStaleZeroHold,
		now:         time.Now,
	}
	for _, fn := range opts {
		if fn != nil {
			fn(&cfg)
		}
	}

	m := mp.Meter(instrumentationScope)

	apiCalls, err := m.Int64Counter(telemetrycontract.MetricAPICalls,
		metric.WithUnit("{call}"),
		metric.WithDescription("Number of LLM API calls re-emitted by the Hub"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", telemetrycontract.MetricAPICalls, err)
	}

	usageTokens, err := m.Int64Counter(telemetrycontract.MetricUsageTokens,
		metric.WithUnit("{token}"),
		metric.WithDescription("Tokens attributed to model requests re-emitted by the Hub, by token_type"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", telemetrycontract.MetricUsageTokens, err)
	}

	u := &UsageOTelMetrics{
		apiCalls:       apiCalls,
		usageTokens:    usageTokens,
		cardinality:    cfg.cardinality,
		hasSink:        cfg.hasSink,
		enabledFn:      cfg.enabledFn,
		staleStore:     cfg.staleStore,
		cacheTTL:       cfg.cacheTTL,
		zeroHold:       cfg.zeroHold,
		now:            cfg.now,
		staleProjects:  make(map[string]struct{}),
		recoveredUntil: make(map[string]time.Time),
	}

	if cfg.staleStore != nil {
		gauge, err := m.Int64ObservableGauge(MetricUsageMeterStale,
			metric.WithUnit("{agent}"),
			metric.WithDescription("Number of active agent runs with stale Track 1 usage meters per project (take max across replicas)"),
		)
		if err != nil {
			return nil, fmt.Errorf("creating %s gauge: %w", MetricUsageMeterStale, err)
		}
		reg, err := m.RegisterCallback(u.observeStaleMeters, gauge)
		if err != nil {
			return nil, fmt.Errorf("registering %s callback: %w", MetricUsageMeterStale, err)
		}
		u.meterStale = gauge
		u.reg = reg
	}

	return u, nil
}

// HasConfiguredSink reports whether this re-emitter is backed by at least one
// constructed and active OTel metrics export sink (Cloud Monitoring or OTLP).
func (u *UsageOTelMetrics) HasConfiguredSink() bool {
	return u != nil && u.hasSink
}

// Cardinality returns the active attribution label cardinality mode.
func (u *UsageOTelMetrics) Cardinality() UsageOTelCardinality {
	if u == nil {
		return UsageOTelCardinalityAgent
	}
	return u.cardinality
}

// Close unregisters any observable gauge callbacks. Safe on nil receiver.
func (u *UsageOTelMetrics) Close() error {
	if u == nil || u.reg == nil {
		return nil
	}
	return u.reg.Unregister()
}

// RecordCommittedEvents increments gen_ai.api.calls and scion.usage.tokens for
// each newly inserted usage_events row after the ingest transaction commits.
//
// Invariants enforced:
//   - Attribution labels (scion_project_id, scion_agent_id, scion_agent_slug,
//     harness) are stamped strictly from run_attribution (attrByRun), never
//     from the ingest payload.
//   - Unknown (nil) token fields are never emitted as 0.
//   - Only the closed telemetrycontract.TokenTypes allowlist is emitted
//     (input, output, cache_read, cache_write, reasoning); cache_write_1h is
//     never emitted.
//   - Never returns an error or blocks the ingest response.
func (u *UsageOTelMetrics) RecordCommittedEvents(
	ctx context.Context,
	events []*store.UsageEvent,
	attrByRun map[string]*store.RunAttribution,
) {
	if u == nil || len(events) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for _, ev := range events {
		if ev == nil {
			continue
		}
		attr := attrByRun[ev.RunID]
		if attr == nil {
			continue
		}

		var identityAttrs [3]attribute.KeyValue
		nIdent := 0
		if attr.ProjectID != "" {
			identityAttrs[nIdent] = attribute.String(telemetrycontract.ProjectLabel, attr.ProjectID)
			nIdent++
		}
		if u.cardinality == UsageOTelCardinalityAgent {
			if attr.AgentID != "" {
				identityAttrs[nIdent] = attribute.String(telemetrycontract.AgentLabel, attr.AgentID)
				nIdent++
			}
			if attr.AgentSlug != "" {
				identityAttrs[nIdent] = attribute.String(telemetrycontract.AgentSlugLabel, attr.AgentSlug)
				nIdent++
			}
		}

		harness := attr.Harness
		model := telemetrycontract.ResolveModelLabel(ev.Model, "")

		if ev.Calls > 0 {
			callKVs := telemetrycontract.APICallsPointAttrs(harness, model, ev.Status)
			callAttrs := make([]attribute.KeyValue, 0, len(callKVs)+nIdent)
			for _, kv := range callKVs {
				callAttrs = append(callAttrs, attribute.String(kv.Key, kv.Value))
			}
			callAttrs = append(callAttrs, identityAttrs[:nIdent]...)
			u.apiCalls.Add(ctx, ev.Calls, metric.WithAttributes(callAttrs...))
		}

		// Closed token_type allowlist (§3.1, §4.1 item 2): cache_write_1h is
		// intentionally excluded.
		buckets := [5]struct {
			tokenType string
			val       *int64
		}{
			{telemetrycontract.TokenTypeInput, ev.Input},
			{telemetrycontract.TokenTypeOutput, ev.Output},
			{telemetrycontract.TokenTypeCacheRead, ev.CacheRead},
			{telemetrycontract.TokenTypeCacheWrite, ev.CacheWrite},
			{telemetrycontract.TokenTypeReasoning, ev.Reasoning},
		}

		baseTokenKVs := telemetrycontract.UsageTokenPointAttrs(harness, model)
		for _, b := range buckets {
			if b.val == nil || *b.val <= 0 {
				continue
			}
			if !telemetrycontract.ValidTokenType(b.tokenType) {
				continue
			}
			tokAttrs := make([]attribute.KeyValue, 0, len(baseTokenKVs)+1+nIdent)
			for _, kv := range baseTokenKVs {
				tokAttrs = append(tokAttrs, attribute.String(kv.Key, kv.Value))
			}
			tokAttrs = append(tokAttrs, attribute.String(telemetrycontract.TokenTypeLabel, b.tokenType))
			tokAttrs = append(tokAttrs, identityAttrs[:nIdent]...)
			u.usageTokens.Add(ctx, *b.val, metric.WithAttributes(tokAttrs...))
		}
	}
}

func (u *UsageOTelMetrics) observeStaleMeters(ctx context.Context, obs metric.Observer) error {
	if u == nil || u.staleStore == nil || u.meterStale == nil {
		return nil
	}
	if u.enabledFn != nil && !u.enabledFn() {
		return nil
	}

	counts := u.cachedStaleCounts(ctx)
	for projectID, count := range counts {
		obs.ObserveInt64(u.meterStale, count, metric.WithAttributes(
			attribute.String("project", projectID),
		))
	}
	return nil
}

func (u *UsageOTelMetrics) cachedStaleCounts(ctx context.Context) map[string]int64 {
	u.staleMu.Lock()
	defer u.staleMu.Unlock()

	now := u.now()
	if u.staleCache != nil && (u.cacheTTL <= 0 || now.Sub(u.staleFetchedAt) < u.cacheTTL) {
		return copyInt64Map(u.staleCache)
	}

	queryCtx, cancel := context.WithTimeout(ctx, defaultMeterStaleQueryTimeout)
	defer cancel()

	dbNow, err := u.staleStore.DBNow(queryCtx)
	if err != nil {
		slog.Warn("hub usage OTel: failed to read DB clock for meter_stale", "error", err)
		return copyInt64Map(u.staleCache)
	}
	staleBefore := dbNow.Add(-staleHeartbeatMultiplier * defaultHeartbeatInterval)
	fresh, err := u.staleStore.CountStaleMetersByProject(queryCtx, staleBefore)
	if err != nil {
		slog.Warn("hub usage OTel: failed to query stale meters", "error", err)
		return copyInt64Map(u.staleCache)
	}

	// Keep recovered projects at 0 for u.zeroHold (default 2m = 2× export
	// interval) so independent GCP and OTLP periodic readers both observe the
	// return to 0 before the project is pruned.
	for proj := range u.staleProjects {
		if count := fresh[proj]; count <= 0 {
			delete(u.staleProjects, proj)
			u.recoveredUntil[proj] = now.Add(u.zeroHold)
		}
	}

	snapshot := make(map[string]int64, len(fresh)+len(u.recoveredUntil))
	for proj, count := range fresh {
		if count > 0 {
			snapshot[proj] = count
			u.staleProjects[proj] = struct{}{}
			delete(u.recoveredUntil, proj)
		}
	}
	for proj, expiry := range u.recoveredUntil {
		if now.Before(expiry) {
			snapshot[proj] = 0
		} else {
			delete(u.recoveredUntil, proj)
		}
	}

	u.staleCache = snapshot
	u.staleFetchedAt = now
	return copyInt64Map(snapshot)
}

func copyInt64Map(src map[string]int64) map[string]int64 {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]int64, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// RegisterUsageOTelMetrics constructs and wires UsageOTelMetrics on mp using
// s.store for scion.usage.meter_stale computation, gated by
// tokenomics.usage_ledger (D2).
func (s *Server) RegisterUsageOTelMetrics(mp metric.MeterProvider, opts ...UsageOTelOption) error {
	allOpts := make([]UsageOTelOption, 0, len(opts)+2)
	if s.store != nil {
		allOpts = append(allOpts, WithUsageOTelStaleStore(s.store))
	}
	allOpts = append(allOpts, WithUsageOTelEnabled(func() bool {
		return s.experimentEnabled(experiments.TokenomicsUsageLedger)
	}))
	allOpts = append(allOpts, opts...)
	m, err := NewUsageOTelMetrics(mp, allOpts...)
	if err != nil {
		return err
	}
	s.SetUsageOTelMetrics(m)
	return nil
}
