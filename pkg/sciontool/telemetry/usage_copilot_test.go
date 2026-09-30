/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

// copilotUsageFixturePath is a real capture: GitHub Copilot CLI 1.0.89 (npm
// @github/copilot, unpinned per D11; platform binary sha256
// 4aeb1afad709a44b4e201e1b228c01418d01ab88b13aaad61ea8123a8beca017),
// captured 2026-09-30 through a local OTLP/HTTP protobuf sink, one
// non-interactive session (`copilot --allow-all-tools -s -p "..."`) that
// read a local file with the "view" tool and one "bash" tool call, producing
// 3 chat-operation model calls in one export batch. No scrubbing was
// needed or applied: the resource carries only service.name/service.version,
// and every point attribute is a gen_ai.*/github.copilot.* semantic-
// convention key -- no host name, session id, user, org or account
// identifier appears anywhere in a metrics export (unlike the trace export
// from the same session, which does carry a conversation id and is not
// captured here, since only metrics feed this deriver). Verified by grepping
// the raw capture and this fixture for anything id/session/user/org/account
// -shaped: no hits, both before and after.
//
// This is the authority for: OQ-1 (the calls histogram's observation count
// equals the number of chat spans: 3, confirmed against the same session's
// trace capture); model placement (gen_ai.request.model/gen_ai.response.model
// are on every point, never only on the resource); and the cache-inclusion
// finding (per-call trace evidence: input_tokens - cache_read.input_tokens -
// cache_write.input_tokens equals a small constant, 2, on every one of the 3
// calls -- the aggregated values below preserve that identity: 62803 -
// 55836 - 6961 = 6 = 2*3).
const copilotUsageFixturePath = "testdata/usage/copilot-1.0.89.pb.json"

// loadCopilotUsageFixture loads the real capture above as a single
// ResourceMetrics (the fixture has exactly one).
func loadCopilotUsageFixture(t *testing.T) *metricpb.ResourceMetrics {
	t.Helper()
	data, err := os.ReadFile(copilotUsageFixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var req colmetricpb.ExportMetricsServiceRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshaling fixture: %v", err)
	}
	if len(req.ResourceMetrics) != 1 {
		t.Fatalf("fixture has %d ResourceMetrics, want 1", len(req.ResourceMetrics))
	}
	return req.ResourceMetrics[0]
}

// newTestCopilotUsageRule builds a copilotUsageRule with a converter anchored
// at startedAt, so tests can control whether a stream's start_time counts as
// "at or after the deriver's own start" (trusted) or not (baselined),
// independent of wall-clock time. Most tests want everything trusted, hence
// newTrustingCopilotUsageRule (startedAt=0: every real timestamp is >= 0).
func newTestCopilotUsageRule(startedAt uint64, capacity int) *copilotUsageRule {
	return &copilotUsageRule{converter: newCumulativeToDeltaConverter(startedAt, capacity)}
}

func newTrustingCopilotUsageRule() *copilotUsageRule {
	return newTestCopilotUsageRule(0, copilotCumulativeStateCapacity)
}

// --- synthetic metric builders (edge cases the real capture cannot exercise
// on demand: absent siblings, negative clamping, stale/backwards values,
// restarts, eviction, and interleaved processes) ---

func copilotHistPoint(count uint64, startTime, timeUnix uint64, model string) *metricpb.HistogramDataPoint {
	return &metricpb.HistogramDataPoint{
		StartTimeUnixNano: startTime,
		TimeUnixNano:      timeUnix,
		Count:             count,
		BucketCounts:      []uint64{count},
		Attributes: []*commonpb.KeyValue{
			metricStringLabel("gen_ai.request.model", model),
			metricStringLabel("gen_ai.operation.name", "chat"),
		},
	}
}

func copilotCallsMetricProto(temporality metricpb.AggregationTemporality, points ...*metricpb.HistogramDataPoint) *metricpb.Metric {
	return &metricpb.Metric{
		Name: copilotCallsMetric,
		Unit: "{token}",
		Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
			AggregationTemporality: temporality,
			DataPoints:             points,
		}},
	}
}

func copilotCounterPoint(value int64, startTime, timeUnix uint64, model string) *metricpb.NumberDataPoint {
	return &metricpb.NumberDataPoint{
		StartTimeUnixNano: startTime,
		TimeUnixNano:      timeUnix,
		Value:             &metricpb.NumberDataPoint_AsInt{AsInt: value},
		Attributes: []*commonpb.KeyValue{
			metricStringLabel("gen_ai.request.model", model),
			metricStringLabel("gen_ai.operation.name", "chat"),
			metricStringLabel("gen_ai.token.modality", "unknown"),
		},
	}
}

func copilotCounterMetricProto(name string, temporality metricpb.AggregationTemporality, points ...*metricpb.NumberDataPoint) *metricpb.Metric {
	return &metricpb.Metric{
		Name: name,
		Unit: "{token}",
		Data: &metricpb.Metric_Sum{Sum: &metricpb.Sum{
			AggregationTemporality: temporality,
			IsMonotonic:            true,
			DataPoints:             points,
		}},
	}
}

func copilotResourceMetrics(resourceAttrs []*commonpb.KeyValue, metrics ...*metricpb.Metric) *metricpb.ResourceMetrics {
	return &metricpb.ResourceMetrics{
		Resource: &resourcepb.Resource{Attributes: resourceAttrs},
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "github.copilot"},
			Metrics: metrics,
		}},
	}
}

// tokensBatch builds one ResourceMetrics with input/cache_read/cache_write/
// output/reasoning counters (any subset -- pass 0 to omit a counter
// entirely, matching Copilot's real behavior of not exporting a counter
// until its first observation, per design §5's batch-correlation contract).
type tokenAmounts struct {
	input, output, cacheRead, cacheWrite, reasoning int64
	omitCacheWrite                                  bool
}

func copilotTokensResourceMetrics(temporality metricpb.AggregationTemporality, startTime, timeUnix uint64, model string, a tokenAmounts) *metricpb.ResourceMetrics {
	var metrics []*metricpb.Metric
	add := func(name string, v int64) {
		metrics = append(metrics, copilotCounterMetricProto(name, temporality, copilotCounterPoint(v, startTime, timeUnix, model)))
	}
	add("gen_ai.client.inference.usage.input_tokens", a.input)
	add("gen_ai.client.inference.usage.output_tokens", a.output)
	add("gen_ai.client.inference.usage.cache_read.input_tokens", a.cacheRead)
	if !a.omitCacheWrite {
		add("gen_ai.client.inference.usage.cache_write.input_tokens", a.cacheWrite)
	}
	if a.reasoning != 0 {
		add("gen_ai.client.inference.usage.reasoning.output_tokens", a.reasoning)
	}
	return copilotResourceMetrics(nil, metrics...)
}

// --- cumulativeToDeltaConverter unit tests ---

// convertForTest converts one data point in isolation, for tests that
// exercise the converter directly rather than through DeriveBatch. It wraps
// withLock plus convertLocked the same way DeriveBatch does, so production
// code keeps exactly one locking entry point (withLock).
func (c *cumulativeToDeltaConverter) convertForTest(streamKey string, startTimeUnixNano uint64, current int64, temporality metricpb.AggregationTemporality) int64 {
	var delta int64
	c.withLock(func() {
		delta = c.convertLocked(streamKey, startTimeUnixNano, current, temporality)
	})
	return delta
}

func TestCumulativeToDeltaConverterDeltaPassthrough(t *testing.T) {
	c := newCumulativeToDeltaConverter(1000, 10)
	// A DELTA point is returned unchanged, repeatedly, regardless of value
	// history: this is what keeps the converter's existence a no-op for any
	// non-cumulative caller.
	if got := c.convertForTest("k", 500, 42, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA); got != 42 {
		t.Fatalf("delta passthrough = %d, want 42", got)
	}
	if got := c.convertForTest("k", 500, 7, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA); got != 7 {
		t.Fatalf("delta passthrough = %d, want 7 (no state should have been stored)", got)
	}
	if len(c.state) != 0 {
		t.Fatalf("state = %+v, want empty: a delta point must never touch the cumulative state map", c.state)
	}
}

func TestCumulativeToDeltaConverterMultiExportSequenceNoDoubleCount(t *testing.T) {
	c := newCumulativeToDeltaConverter(0, 10)
	seq := []struct{ cumulative, wantDelta int64 }{
		{100, 100}, // first sighting, trusted (startedAt=0)
		{250, 150},
		{400, 150},
	}
	for i, step := range seq {
		got := c.convertForTest("stream", 1, step.cumulative, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE)
		if got != step.wantDelta {
			t.Fatalf("export %d: delta = %d, want %d", i, got, step.wantDelta)
		}
	}
}

// TestCumulativeToDeltaConverterStaleBackwardsIsNotAReset pins the design
// decision that a value going backwards for the SAME stream identity (same
// start_time) is stale -- an older export delivered after a newer one, on a
// retry or reordering -- not a reset. It emits 0, never a negative delta,
// and the remembered high-water mark is never regressed by the stale value,
// so a later, larger export still diffs correctly against it. Only a
// genuinely new start_time is ever treated as the stream starting over
// (TestCumulativeToDeltaConverterRestartBaseline and
// TestCumulativeToDeltaConverterEvictionThenReturnIsBaselined cover that
// case).
func TestCumulativeToDeltaConverterStaleBackwardsIsNotAReset(t *testing.T) {
	c := newCumulativeToDeltaConverter(0, 10)
	seq := []struct{ cumulative, wantDelta int64 }{
		{10, 10},
		{30, 20},
		{20, 0},  // stale: backwards, same stream identity.
		{40, 10}, // diffs against the high-water mark (30), not the stale 20.
	}
	for i, step := range seq {
		got := c.convertForTest("s", 1, step.cumulative, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE)
		if got != step.wantDelta {
			t.Fatalf("step %d (value %d): delta = %d, want %d", i, step.cumulative, got, step.wantDelta)
		}
	}
	if got := c.staleBackwards.Load(); got != 1 {
		t.Fatalf("staleBackwards = %d, want 1", got)
	}
}

// TestCumulativeToDeltaConverterUnchangedValueEmitsZero: an export that
// repeats the same cumulative value (nothing happened in that interval) is
// not backwards and not stale, just a legitimate zero delta.
func TestCumulativeToDeltaConverterUnchangedValueEmitsZero(t *testing.T) {
	c := newCumulativeToDeltaConverter(0, 10)
	if got := c.convertForTest("s", 1, 30, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 30 {
		t.Fatalf("first delta = %d, want 30", got)
	}
	if got := c.convertForTest("s", 1, 30, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 0 {
		t.Fatalf("unchanged delta = %d, want 0", got)
	}
	if got := c.staleBackwards.Load(); got != 0 {
		t.Fatalf("staleBackwards = %d, want 0: an unchanged value is not backwards", got)
	}
}

func TestCumulativeToDeltaConverterRestartBaseline(t *testing.T) {
	// startedAt is in the future relative to the stream's start_time: this
	// simulates sciontool restarting mid-session, so the stream's history
	// may already have been counted by a prior process.
	c := newCumulativeToDeltaConverter(1_000_000, 10)
	if got := c.convertForTest("s", 500, 9000, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 0 {
		t.Fatalf("first sighting before deriver start = %d, want 0 (baselined, not re-emitted)", got)
	}
	// The next export for the same stream diffs against the baseline
	// normally.
	if got := c.convertForTest("s", 500, 9500, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 500 {
		t.Fatalf("post-baseline delta = %d, want 500", got)
	}
}

// TestCumulativeToDeltaConverterInterleavedProcesses is a converter-only
// unit test: it exercises convert() directly with two hand-built keys, to
// pin the converter's own key-independence property in isolation. The real
// key construction (copilotStreamKey) is exercised separately by
// TestCopilotStreamKeyDiffersByStartTime (a direct unit test) and by
// TestCopilotUsageRuleDeriveBatchInterleavedProcessesCumulative (an
// end-to-end DeriveBatch test using two real ResourceMetrics batches with
// different start_time, which is what actually proves two concurrent
// Copilot processes cannot cross-contaminate each other's state).
func TestCumulativeToDeltaConverterInterleavedProcesses(t *testing.T) {
	c := newCumulativeToDeltaConverter(0, 10)
	// Two concurrent processes sharing every attribute except start_time
	// (which is folded into the caller's stream key, not passed here
	// directly -- this test uses two distinct keys to model that).
	keyA := "resource\x00scope\x00metric\x00attrs\x00" + "\x00\x00\x00\x00\x00\x00\x00\x01"
	keyB := "resource\x00scope\x00metric\x00attrs\x00" + "\x00\x00\x00\x00\x00\x00\x00\x02"

	if got := c.convertForTest(keyA, 1, 100, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 100 {
		t.Fatalf("process A first delta = %d, want 100", got)
	}
	if got := c.convertForTest(keyB, 2, 50, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 50 {
		t.Fatalf("process B first delta = %d, want 50 (independent of A)", got)
	}
	if got := c.convertForTest(keyA, 1, 130, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 30 {
		t.Fatalf("process A second delta = %d, want 30 (unaffected by B)", got)
	}
	if got := c.convertForTest(keyB, 2, 80, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 30 {
		t.Fatalf("process B second delta = %d, want 30 (unaffected by A)", got)
	}
}

// TestCumulativeToDeltaConverterEvictionThenReturnIsBaselined: capacity 2,
// three distinct streams a/b/c (start_time 100/200/300
// respectively) fill and then overflow the map, evicting "a" (the least
// recently updated). "a" returning afterward must be baselined, never
// re-emitted in full -- but a genuinely NEW stream started after that
// eviction (start_time greater than anything ever evicted) must still be
// trusted: that is exactly what maxEvictedStart (as opposed to a permanent
// latch) buys over the old design.
func TestCumulativeToDeltaConverterEvictionThenReturnIsBaselined(t *testing.T) {
	c := newCumulativeToDeltaConverter(0, 2)
	if got := c.convertForTest("a", 100, 5, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 5 {
		t.Fatalf("a first delta = %d, want 5", got)
	}
	if got := c.convertForTest("b", 200, 1, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 1 {
		t.Fatalf("b first delta = %d, want 1", got)
	}
	// c is a third distinct stream: capacity 2 is now exceeded, evicting the
	// least-recently-updated entry, "a" (start_time 100).
	if got := c.convertForTest("c", 300, 1, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 1 {
		t.Fatalf("c first delta = %d, want 1", got)
	}
	if got := c.maxEvictedStart; got != 100 {
		t.Fatalf("maxEvictedStart = %d, want 100 (a's start_time)", got)
	}

	// a returns: its start_time (100) does not exceed maxEvictedStart (100),
	// so it is baselined, never re-emitted in full, even though it would
	// have been trusted under the plain startedAt check alone.
	if got := c.convertForTest("a", 100, 7, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 0 {
		t.Fatalf("returning evicted stream delta = %d, want 0 (baselined, never re-emitted)", got)
	}
	if got := c.baselinedAfterCap.Load(); got != 1 {
		t.Fatalf("baselinedAfterCap = %d, want 1", got)
	}
	if got := c.convertForTest("a", 100, 9, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 2 {
		t.Fatalf("delta after re-baseline = %d, want 2", got)
	}
	// Inserting "a" back evicted "b" (start_time 200, now the
	// least-recently-updated of {b, c, a}), raising maxEvictedStart to 200.

	// A genuinely new process, d, started after that eviction (start_time
	// 400 > maxEvictedStart 200) must still be trusted -- the whole point of
	// maxEvictedStart over a permanent latch: eviction must never cost every
	// future new stream its first interval, only a returning evicted one.
	if got := c.convertForTest("d", 400, 3, metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE); got != 3 {
		t.Fatalf("new process after eviction delta = %d, want 3 (trusted, not baselined)", got)
	}
}

// TestCopilotStreamKeyDiffersByStartTime is a direct unit test of
// copilotStreamKey, pinning the one property
// TestCumulativeToDeltaConverterInterleavedProcesses and the DeriveBatch-
// level interleaved-process test below both depend on: two otherwise-
// identical points with different start_time must produce different stream
// keys, since start_time is what lets the converter tell two concurrent
// Copilot processes apart.
func TestCopilotStreamKeyDiffersByStartTime(t *testing.T) {
	attrs := []*commonpb.KeyValue{metricStringLabel("gen_ai.request.model", "m")}
	keyA, err := copilotStreamKey(nil, "github.copilot", copilotCallsMetric, attrs, 100)
	if err != nil {
		t.Fatalf("copilotStreamKey: %v", err)
	}
	keyB, err := copilotStreamKey(nil, "github.copilot", copilotCallsMetric, attrs, 200)
	if err != nil {
		t.Fatalf("copilotStreamKey: %v", err)
	}
	if keyA == keyB {
		t.Fatalf("keys for start_time 100 and 200 must differ, both = %q", keyA)
	}
	// Same start_time, same everything else: same key (sanity check that the
	// key is otherwise deterministic, not merely always-unique).
	keyA2, err := copilotStreamKey(nil, "github.copilot", copilotCallsMetric, attrs, 100)
	if err != nil {
		t.Fatalf("copilotStreamKey: %v", err)
	}
	if keyA != keyA2 {
		t.Fatalf("keys for identical inputs must match: %q vs %q", keyA, keyA2)
	}
}

// --- copilotUsageRule.DeriveBatch tests ---

func TestCopilotUsageRuleDeriveBatchCallsFromHistogramCount(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := copilotResourceMetrics(nil,
		copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotHistPoint(3, 1, 2, "claude-sonnet-5")))
	increments, matched, err := r.DeriveBatch(rm)
	if err != nil {
		t.Fatalf("DeriveBatch error: %v", err)
	}
	if !matched[copilotCallsMetric] {
		t.Fatal("calls histogram not reported matched")
	}
	if len(increments) != 1 || increments[0].Calls != 3 || len(increments[0].Tokens) != 0 {
		t.Fatalf("increments = %+v, want a single Calls=3 increment", increments)
	}
	if increments[0].Model != "claude-sonnet-5" {
		t.Fatalf("model = %q", increments[0].Model)
	}
}

func TestCopilotUsageRuleDeriveBatchSubtractsCacheFromInput(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, "m",
		tokenAmounts{input: 100, output: 20, cacheRead: 30, cacheWrite: 10})
	increments, matched, err := r.DeriveBatch(rm)
	if err != nil {
		t.Fatalf("DeriveBatch error: %v", err)
	}
	for _, name := range []string{
		"gen_ai.client.inference.usage.input_tokens",
		"gen_ai.client.inference.usage.output_tokens",
		"gen_ai.client.inference.usage.cache_read.input_tokens",
		"gen_ai.client.inference.usage.cache_write.input_tokens",
	} {
		if !matched[name] {
			t.Fatalf("%s not reported matched", name)
		}
	}
	if len(increments) != 1 {
		t.Fatalf("increments = %+v, want exactly one correlated increment", increments)
	}
	tokens := increments[0].Tokens
	want := map[string]int64{
		telemetrycontract.TokenTypeInput:      60, // 100 - 30 - 10
		telemetrycontract.TokenTypeOutput:     20,
		telemetrycontract.TokenTypeCacheRead:  30,
		telemetrycontract.TokenTypeCacheWrite: 10,
	}
	for k, v := range want {
		if tokens[k] != v {
			t.Fatalf("tokens[%q] = %d, want %d (tokens=%+v)", k, tokens[k], v, tokens)
		}
	}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %+v, want exactly %+v", tokens, want)
	}
}

// TestCopilotUsageRuleDeriveBatchCacheWriteAbsentCountsAsZero pins design
// §5's batch-correlation contract: Copilot may not export a counter until
// its first observation, so a batch missing cache_write.input_tokens
// entirely must not error, panic, or skip the correlation -- the absent
// sibling simply contributes 0.
func TestCopilotUsageRuleDeriveBatchCacheWriteAbsentCountsAsZero(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, "m",
		tokenAmounts{input: 50, cacheRead: 20, omitCacheWrite: true})
	increments, matched, err := r.DeriveBatch(rm)
	if err != nil {
		t.Fatalf("DeriveBatch error: %v", err)
	}
	if matched["gen_ai.client.inference.usage.cache_write.input_tokens"] {
		t.Fatal("an absent metric must not be reported matched")
	}
	if len(increments) != 1 {
		t.Fatalf("increments = %+v, want exactly one", increments)
	}
	tokens := increments[0].Tokens
	if tokens[telemetrycontract.TokenTypeInput] != 30 { // 50 - 20 - 0
		t.Fatalf("input = %d, want 30", tokens[telemetrycontract.TokenTypeInput])
	}
	if _, ok := tokens[telemetrycontract.TokenTypeCacheWrite]; ok {
		t.Fatalf("tokens = %+v, must not contain cache_write when the metric never appeared", tokens)
	}
}

// TestCopilotUsageRuleDeriveBatchNegativeInputClampsToZero pins design §5's
// batch-correlation contract: a batch where the caches exceed input must
// clamp to 0, never go negative, and never panic.
func TestCopilotUsageRuleDeriveBatchNegativeInputClampsToZero(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, "m",
		tokenAmounts{input: 10, cacheRead: 8, cacheWrite: 5}) // 10-8-5 = -3
	increments, _, err := r.DeriveBatch(rm)
	if err != nil {
		t.Fatalf("DeriveBatch error: %v", err)
	}
	if len(increments) != 1 {
		t.Fatalf("increments = %+v, want exactly one", increments)
	}
	tokens := increments[0].Tokens
	if v, ok := tokens[telemetrycontract.TokenTypeInput]; ok && v != 0 {
		t.Fatalf("input = %d, want absent or 0, never negative", v)
	}
	if _, ok := tokens[telemetrycontract.TokenTypeInput]; ok {
		t.Fatal("a clamped-to-zero input should not be emitted as a zero-valued token point")
	}
	if got := r.clampedInputCount(); got != 1 {
		t.Fatalf("clampedInputCount = %d, want 1", got)
	}
}

// TestCopilotUsageRuleDeriveBatchCumulativeNoDoubleCountAcrossReexports
// simulates the real provisioning scenario (OTEL_METRIC_EXPORT_INTERVAL=
// 30000): the same long-running Copilot process re-exports its growing
// cumulative totals every export. Feeding two such batches through the same
// rule instance must yield only the second export's incremental usage, not
// its running total.
func TestCopilotUsageRuleDeriveBatchCumulativeNoDoubleCountAcrossReexports(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	first := copilotResourceMetrics(nil,
		copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotHistPoint(2, 1, 10, "m")))
	second := copilotResourceMetrics(nil,
		copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotHistPoint(5, 1, 40, "m")))

	inc1, _, err := r.DeriveBatch(first)
	if err != nil {
		t.Fatalf("first DeriveBatch error: %v", err)
	}
	if len(inc1) != 1 || inc1[0].Calls != 2 {
		t.Fatalf("first export increments = %+v, want Calls=2", inc1)
	}

	inc2, _, err := r.DeriveBatch(second)
	if err != nil {
		t.Fatalf("second DeriveBatch error: %v", err)
	}
	if len(inc2) != 1 || inc2[0].Calls != 3 {
		t.Fatalf("second export increments = %+v, want Calls=3 (5-2, not the raw running total 5)", inc2)
	}
}

// copilotCumulativeStep builds one export (one ResourceMetrics) for a
// single "process", carrying the calls histogram and the input/cache_read
// counters, all CUMULATIVE, all sharing startTime (one process's stream
// identity) and model. Raw values are the process's running totals as of
// this export, not deltas -- DeriveBatch's job is to recover the deltas.
func copilotCumulativeStep(startTime, timeUnix uint64, model string, calls uint64, input, cacheRead int64) *metricpb.ResourceMetrics {
	return copilotResourceMetrics(nil,
		copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotHistPoint(calls, startTime, timeUnix, model)),
		copilotCounterMetricProto("gen_ai.client.inference.usage.input_tokens", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotCounterPoint(input, startTime, timeUnix, model)),
		copilotCounterMetricProto("gen_ai.client.inference.usage.cache_read.input_tokens", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotCounterPoint(cacheRead, startTime, timeUnix, model)),
	)
}

// copilotStepIncrement sums one DeriveBatch call's increments down to the
// three numbers copilotCumulativeStep's caller cares about, so a zero-
// increment step (an empty slice) and an explicit-zero step compare equal.
func copilotStepIncrement(t *testing.T, r *copilotUsageRule, rm *metricpb.ResourceMetrics) (calls, input, cacheRead int64) {
	t.Helper()
	inc, _, err := r.DeriveBatch(rm)
	if err != nil {
		t.Fatalf("DeriveBatch error: %v", err)
	}
	for _, i := range inc {
		calls += i.Calls
		input += i.Tokens[telemetrycontract.TokenTypeInput]
		cacheRead += i.Tokens[telemetrycontract.TokenTypeCacheRead]
	}
	return calls, input, cacheRead
}

// TestCopilotUsageRuleDeriveBatchInterleavedProcessesCumulative: unlike
// TestCumulativeToDeltaConverterInterleavedProcesses (which feeds hand-built
// keys straight into the converter), this drives two concurrent processes
// through DeriveBatch itself with real ResourceMetrics batches -- the same
// resource, scope and point attributes, differing only in start_time --
// alternating requests, so it actually exercises copilotStreamKey's
// start_time folding rather than assuming it. A mutation that drops
// start_time from copilotStreamKey collapses A and B into one stream and
// fails this test.
func TestCopilotUsageRuleDeriveBatchInterleavedProcessesCumulative(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	const (
		startA = 100
		startB = 200
	)
	check := func(label string, rm *metricpb.ResourceMetrics, wantCalls, wantInput, wantCacheRead int64) {
		t.Helper()
		calls, input, cacheRead := copilotStepIncrement(t, r, rm)
		if calls != wantCalls || input != wantInput || cacheRead != wantCacheRead {
			t.Fatalf("%s: calls=%d input=%d cache_read=%d, want calls=%d input=%d cache_read=%d",
				label, calls, input, cacheRead, wantCalls, wantInput, wantCacheRead)
		}
	}

	check("A: calls 1, in 102, cr 100", copilotCumulativeStep(startA, 1, "m", 1, 102, 100), 1, 2, 100)
	check("B: calls 1, in 52, cr 50", copilotCumulativeStep(startB, 2, "m", 1, 52, 50), 1, 2, 50)
	check("A: calls 3, in 308, cr 300", copilotCumulativeStep(startA, 3, "m", 3, 308, 300), 2, 6, 200)
	check("B: calls 2, in 104, cr 100", copilotCumulativeStep(startB, 4, "m", 2, 104, 100), 1, 2, 50)
	check("A re-export: calls 3, in 308, cr 300 (unchanged)", copilotCumulativeStep(startA, 5, "m", 3, 308, 300), 0, 0, 0)
}

// TestCopilotUsageRuleDeriveBatchEvictionThenReturnBaselined is the
// DeriveBatch-level eviction-then-return test, with a small capacity
// injected directly (bypassing newCopilotUsageRule's real-time-anchored
// constructor, which would make a deterministic eviction sequence awkward
// to set up).
func TestCopilotUsageRuleDeriveBatchEvictionThenReturnBaselined(t *testing.T) {
	r := newTestCopilotUsageRule(0, 2) // capacity 2, startedAt 0 (everything trusted initially).
	check := func(label string, rm *metricpb.ResourceMetrics, wantCalls int64) {
		t.Helper()
		calls, _, _ := copilotStepIncrement(t, r, rm)
		if calls != wantCalls {
			t.Fatalf("%s: calls = %d, want %d", label, calls, wantCalls)
		}
	}
	callsOnly := func(startTime, timeUnix uint64, calls uint64) *metricpb.ResourceMetrics {
		return copilotResourceMetrics(nil, copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			copilotHistPoint(calls, startTime, timeUnix, "m")))
	}

	check("A(5)", callsOnly(100, 1, 5), 5)
	check("B(1)", callsOnly(200, 2, 1), 1)
	check("C(1) evicts A", callsOnly(300, 3, 1), 1)
	check("A(7) returns, baselined", callsOnly(100, 4, 7), 0)
	check("A(9) diffs against the baseline", callsOnly(100, 5, 9), 2)
}

// concurrentDeriveBatchTotal runs DeriveBatch on every element of rms
// concurrently (one goroutine each) against the same rule, and returns the
// sum of Calls/input/cache_read across every increment every goroutine
// produced. Used by the two regression tests below to assert an exact,
// order-independent total regardless of which goroutine's batch the
// converter happens to process first.
func concurrentDeriveBatchTotal(t *testing.T, r *copilotUsageRule, rms []*metricpb.ResourceMetrics) (calls, input, cacheRead int64) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	wg.Add(len(rms))
	for _, rm := range rms {
		go func(rm *metricpb.ResourceMetrics) {
			defer wg.Done()
			inc, _, err := r.DeriveBatch(rm)
			if err != nil {
				t.Errorf("DeriveBatch error: %v", err)
				return
			}
			var c, i, cr int64
			for _, x := range inc {
				c += x.Calls
				i += x.Tokens[telemetrycontract.TokenTypeInput]
				cr += x.Tokens[telemetrycontract.TokenTypeCacheRead]
			}
			mu.Lock()
			calls += c
			input += i
			cacheRead += cr
			mu.Unlock()
		}(rm)
	}
	wg.Wait()
	return calls, input, cacheRead
}

// TestCopilotUsageRuleDeriveBatchConcurrentOverlappingExportsNoOvercount:
// two goroutines submit two DIFFERENT cumulative exports for the SAME
// stream (same resource/scope/point attributes and start_time)
// concurrently. DeriveBatch must convert each whole batch atomically with
// respect to the other: regardless of which one the converter happens to
// process first, the total canonical input/cache_read across both must
// equal exactly what one request's worth of new data should be -- never
// more (mis-correlating one batch's input delta with the other batch's
// cache_read delta) and never less.
//
// E1 (raw input=102, cache_read=100) and E2 (raw input=204, cache_read=200):
// whichever is processed first gets the full delta (trusted first sighting:
// input=102, cache_read=100, canonical input=2); the other then diffs
// against that baseline (input=204-102=102, cache_read=200-100=100,
// canonical input=2) -- total input=4, total cache_read=200, in EITHER
// order. If the two batches interleave at the per-point level instead of
// converting atomically, one batch's input delta can pair with the other's
// cache_read delta and silently over-report canonical input (reproduced at
// over 100 instead of 4 in roughly a third of runs before this fix).
func TestCopilotUsageRuleDeriveBatchConcurrentOverlappingExportsNoOvercount(t *testing.T) {
	const iterations = 500
	for i := range iterations {
		r := newTrustingCopilotUsageRule()
		startTime := uint64(10_000 + i) // a fresh, iteration-local stream.
		e1 := copilotCumulativeStep(startTime, 1, "m", 0, 102, 100)
		e2 := copilotCumulativeStep(startTime, 2, "m", 0, 204, 200)

		_, input, cacheRead := concurrentDeriveBatchTotal(t, r, []*metricpb.ResourceMetrics{e1, e2})
		if input != 4 {
			t.Fatalf("iteration %d: total input = %d, want 4", i, input)
		}
		if cacheRead != 200 {
			t.Fatalf("iteration %d: total cache_read = %d, want 200", i, cacheRead)
		}
	}
}

// TestCopilotUsageRuleDeriveBatchConcurrentDuplicateRetryNoOvercount: a
// duplicate-retry variant -- the SAME export (byte-identical cumulative
// values) submitted twice concurrently, modeling an OTLP client retrying a
// request whose response it never saw (handleMetrics can block for up to
// usageDeriverFlushTimeout in ForceFlush). Whichever copy the converter
// processes first gets the real delta; the other sees an unchanged
// cumulative value and correctly contributes nothing. Total input/
// cache_read must equal exactly one copy's worth, never two.
func TestCopilotUsageRuleDeriveBatchConcurrentDuplicateRetryNoOvercount(t *testing.T) {
	const iterations = 500
	for i := range iterations {
		r := newTrustingCopilotUsageRule()
		startTime := uint64(20_000 + i)
		rm := copilotCumulativeStep(startTime, 1, "m", 0, 102, 100)
		rmRetry := copilotCumulativeStep(startTime, 1, "m", 0, 102, 100) // byte-identical, separate message.

		_, input, cacheRead := concurrentDeriveBatchTotal(t, r, []*metricpb.ResourceMetrics{rm, rmRetry})
		if input != 2 {
			t.Fatalf("iteration %d: total input = %d, want 2", i, input)
		}
		if cacheRead != 100 {
			t.Fatalf("iteration %d: total cache_read = %d, want 100", i, cacheRead)
		}
	}
}

// TestCopilotUsageRuleDeriveBatchMalformedPointDropsOnlyThatPoint: a negative
// or non-integer point is dropped and reported malformed, without
// discarding a well-formed sibling.
func TestCopilotUsageRuleDeriveBatchMalformedPointDropsOnlyThatPoint(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	bad := copilotCounterPoint(-1, 1, 2, "m")
	good := &metricpb.NumberDataPoint{
		StartTimeUnixNano: 1, TimeUnixNano: 3,
		Value:      &metricpb.NumberDataPoint_AsInt{AsInt: 9},
		Attributes: []*commonpb.KeyValue{metricStringLabel("gen_ai.request.model", "m2"), metricStringLabel("gen_ai.operation.name", "chat")},
	}
	m := copilotCounterMetricProto("gen_ai.client.inference.usage.output_tokens", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, bad, good)
	rm := copilotResourceMetrics(nil, m)
	increments, matched, err := r.DeriveBatch(rm)
	if err == nil {
		t.Fatal("want a malformed error for the negative point")
	}
	if !matched["gen_ai.client.inference.usage.output_tokens"] {
		t.Fatal("metric must still be reported matched")
	}
	if len(increments) != 1 || increments[0].Tokens[telemetrycontract.TokenTypeOutput] != 9 {
		t.Fatalf("increments = %+v, want exactly the good point's 9 output tokens", increments)
	}
}

func TestCopilotUsageRuleDeriveBatchUnmatchedMetricName(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := copilotResourceMetrics(nil, &metricpb.Metric{
		Name: "gen_ai.client.operation.duration",
		Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
			AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints:             []*metricpb.HistogramDataPoint{copilotHistPoint(1, 1, 2, "m")},
		}},
	})
	increments, matched, err := r.DeriveBatch(rm)
	if err != nil || len(matched) != 0 || len(increments) != 0 {
		t.Fatalf("increments=%+v matched=%v err=%v, want a fully unrecognized batch to match nothing", increments, matched, err)
	}
}

func TestCopilotUsageRuleDeriveBatchOperationHistogramsNeverProduceTokens(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := copilotResourceMetrics(nil, &metricpb.Metric{
		Name: "gen_ai.client.inference.operation.output_tokens",
		Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
			AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints:             []*metricpb.HistogramDataPoint{copilotHistPoint(3, 1, 2, "m")},
		}},
	})
	increments, matched, err := r.DeriveBatch(rm)
	if err != nil || len(matched) != 0 || len(increments) != 0 {
		t.Fatalf("increments=%+v matched=%v err=%v, want operation.output_tokens to never match (only operation.input_tokens is the calls source)", increments, matched, err)
	}
}

// TestCopilotUsageRuleDeriveBatchTwoExportRequestsNeverCorrelate proves the
// invariant copilotCorrelationKey's doc comment relies on: correlation must
// never reach across two separate export requests, even when they share
// every resource/scope/point attribute and differ only in start_time (the
// shape two concurrent Copilot processes would actually produce, since the
// capture shows the resource carries no per-process identifier). Each
// DeriveBatch call here models one received OTLP export request -- see the
// receiver/pipeline path cited in copilotCorrelationKey's comment for why
// that is exactly the granularity DeriveBatch is ever called at.
func TestCopilotUsageRuleDeriveBatchTwoExportRequestsNeverCorrelate(t *testing.T) {
	r := newTrustingCopilotUsageRule()

	// Request 1: process A. input=50, cache_read=10, cache_write=5 ->
	// canonical input 35.
	reqA := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 100, 101, "m",
		tokenAmounts{input: 50, cacheRead: 10, cacheWrite: 5})
	incA, _, err := r.DeriveBatch(reqA)
	if err != nil {
		t.Fatalf("request A: DeriveBatch error: %v", err)
	}
	if len(incA) != 1 || incA[0].Tokens[telemetrycontract.TokenTypeInput] != 35 {
		t.Fatalf("request A increments = %+v, want a single increment with input=35", incA)
	}

	// Request 2: process B, delivered back to back, same resource/scope/
	// point attributes as A (same model, same operation -- Copilot's
	// resource carries no per-process identifier) but a different
	// start_time, and deliberately NO input_tokens metric at all in this
	// request -- only cache_read. If correlation ever reached across
	// requests, B's cache_read would wrongly subtract from a remembered A
	// input and/or produce a phantom canonical-input increment. It must
	// not: with no input_tokens metric in this batch, there is nothing to
	// correlate, and cache_read passes through on its own.
	reqB := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 200, 201, "m",
		tokenAmounts{cacheRead: 7, omitCacheWrite: true})
	incB, _, err := r.DeriveBatch(reqB)
	if err != nil {
		t.Fatalf("request B: DeriveBatch error: %v", err)
	}
	if len(incB) != 1 {
		t.Fatalf("request B increments = %+v, want exactly one", incB)
	}
	if _, ok := incB[0].Tokens[telemetrycontract.TokenTypeInput]; ok {
		t.Fatalf("request B tokens = %+v, must not contain input: nothing in B correlates with A's remembered input", incB[0].Tokens)
	}
	if incB[0].Tokens[telemetrycontract.TokenTypeCacheRead] != 7 {
		t.Fatalf("request B cache_read = %d, want 7, unaffected by A", incB[0].Tokens[telemetrycontract.TokenTypeCacheRead])
	}

	// A third request on A's own stream (same start_time, new DELTA values --
	// DELTA passes through unconverted, so this isn't testing dedupe/diffing
	// at all) must produce exactly its own numbers, confirming B's
	// intervening request left no cross-request state on A's stream.
	reqA2 := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 100, 102, "m",
		tokenAmounts{input: 20, cacheRead: 3, cacheWrite: 1})
	incA2, _, err := r.DeriveBatch(reqA2)
	if err != nil {
		t.Fatalf("request A2: DeriveBatch error: %v", err)
	}
	if len(incA2) != 1 || incA2[0].Tokens[telemetrycontract.TokenTypeInput] != 16 { // 20-3-1
		t.Fatalf("request A2 increments = %+v, want input=16, unaffected by B", incA2)
	}
}

// TestCopilotUsageRuleDeriveBatchRealFixture is the fixture-driven test the
// synthetic ones above are meant to be checked against: the real capture's
// numbers, run through DeriveBatch, must reproduce the same per-call
// arithmetic the trace capture independently proves (see the fixture doc
// comment).
func TestCopilotUsageRuleDeriveBatchRealFixture(t *testing.T) {
	r := newTrustingCopilotUsageRule()
	rm := loadCopilotUsageFixture(t)

	increments, matched, err := r.DeriveBatch(rm)
	if err != nil {
		t.Fatalf("DeriveBatch error: %v", err)
	}
	for _, name := range []string{
		copilotCallsMetric,
		"gen_ai.client.inference.usage.input_tokens",
		"gen_ai.client.inference.usage.output_tokens",
		"gen_ai.client.inference.usage.cache_read.input_tokens",
		"gen_ai.client.inference.usage.cache_write.input_tokens",
		"gen_ai.client.inference.usage.reasoning.output_tokens",
	} {
		if !matched[name] {
			t.Errorf("%s not reported matched", name)
		}
	}
	// The unmatched metrics real Copilot also emits in the same export
	// (duration histograms, tool-call counters, sandbox diagnostics) must
	// never be reported matched.
	for _, name := range []string{
		"gen_ai.client.operation.duration",
		"gen_ai.client.operation.time_to_first_chunk",
		"gen_ai.execute_tool.duration",
		"github.copilot.tool.call.count",
	} {
		if matched[name] {
			t.Errorf("%s must not be reported matched", name)
		}
	}

	var calls *usageIncrement
	var tokens *usageIncrement
	for i := range increments {
		switch {
		case increments[i].Calls != 0:
			calls = &increments[i]
		case len(increments[i].Tokens) != 0:
			tokens = &increments[i]
		}
	}
	if calls == nil {
		t.Fatal("no calls increment derived")
	}
	if calls.Calls != 3 {
		t.Fatalf("calls = %d, want 3 (matches the session's 3 chat spans and gen_ai.invoke_agent.inference_calls=3)", calls.Calls)
	}
	if calls.Model != "claude-sonnet-5" {
		t.Fatalf("calls model = %q, want claude-sonnet-5", calls.Model)
	}

	if tokens == nil {
		t.Fatal("no tokens increment derived")
	}
	want := map[string]int64{
		telemetrycontract.TokenTypeInput:      6, // 62803 - 55836 - 6961; matches 2 non-cached tokens/call * 3 calls
		telemetrycontract.TokenTypeOutput:     202,
		telemetrycontract.TokenTypeCacheRead:  55836,
		telemetrycontract.TokenTypeCacheWrite: 6961,
		telemetrycontract.TokenTypeReasoning:  9,
	}
	for k, v := range want {
		if tokens.Tokens[k] != v {
			t.Errorf("tokens[%q] = %d, want %d", k, tokens.Tokens[k], v)
		}
	}
	if len(tokens.Tokens) != len(want) {
		t.Errorf("tokens = %+v, want exactly %+v", tokens.Tokens, want)
	}
}

func TestNewUsageDeriverIncludesCopilotRuleForCopilotHarness(t *testing.T) {
	t.Setenv("SCION_HARNESS", "copilot")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 1 {
		t.Fatalf("rules = %d, want exactly the copilot rule", len(d.rules))
	}
	if d.rules[0].Harness() != "copilot" {
		t.Fatalf("rule harness = %q, want copilot", d.rules[0].Harness())
	}
	if _, ok := d.rules[0].(metricBatchDeriver); !ok {
		t.Fatal("the copilot rule must implement metricBatchDeriver")
	}
}

// TestUsageDeriverObserveMetricBatchDedupesReplayedBatch pins the metric-rule
// dedupe (design §3.3's "interval-fingerprint idea") through the new batch
// path: a replayed export carrying byte-identical points must not double
// count, while still being reported matched for GCP consume purposes.
func TestUsageDeriverObserveMetricBatchDedupesReplayedBatch(t *testing.T) {
	d := bareUsageDeriver(newTrustingCopilotUsageRule())
	rm := copilotResourceMetrics(nil,
		copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			copilotHistPoint(2, 1, 2, "m")))

	batchRule := d.rules[0].(metricBatchDeriver)
	matched := map[string]bool{}
	if !d.observeMetricBatch(context.Background(), batchRule, rm, matched) {
		t.Fatal("first observation should record")
	}
	matched2 := map[string]bool{}
	if d.observeMetricBatch(context.Background(), batchRule, rm, matched2) {
		t.Fatal("replayed observation should be deduped, not recorded again")
	}
	if !matched2[copilotCallsMetric] {
		t.Fatal("a deduped point must still be reported matched, for GCP consume purposes")
	}
	diag := d.Diagnostics()
	if diag.Derived != 1 || diag.Duplicate != 1 {
		t.Fatalf("diagnostics = %+v, want Derived=1 Duplicate=1", diag)
	}
}

// TestUsageDeriverDiagnosticsAggregatesRuleCounters pins
// UsageDeriver.Diagnostics's optional usageRuleDiagnostics aggregation: a
// rule's clamp and stale-backwards counters must surface through
// Diagnostics(), not just be readable off the rule directly.
func TestUsageDeriverDiagnosticsAggregatesRuleCounters(t *testing.T) {
	rule := newTrustingCopilotUsageRule()
	d := bareUsageDeriver(rule)

	// One clamp: caches exceed input.
	clampBatch := copilotTokensResourceMetrics(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, "m",
		tokenAmounts{input: 5, cacheRead: 10})
	if _, _, err := rule.DeriveBatch(clampBatch); err != nil {
		t.Fatalf("DeriveBatch: %v", err)
	}

	// One stale-backwards: same cumulative stream, value goes down.
	if _, _, err := rule.DeriveBatch(copilotCumulativeStep(50, 1, "m", 0, 100, 0)); err != nil {
		t.Fatalf("DeriveBatch: %v", err)
	}
	if _, _, err := rule.DeriveBatch(copilotCumulativeStep(50, 2, "m", 0, 60, 0)); err != nil {
		t.Fatalf("DeriveBatch: %v", err)
	}

	diag := d.Diagnostics()
	if diag.ClampedInput != 1 {
		t.Fatalf("ClampedInput = %d, want 1", diag.ClampedInput)
	}
	if diag.StaleBackwards != 1 {
		t.Fatalf("StaleBackwards = %d, want 1", diag.StaleBackwards)
	}
	if diag.BaselinedAfterCap != 0 {
		t.Fatalf("BaselinedAfterCap = %d, want 0 (never triggered in this test)", diag.BaselinedAfterCap)
	}
}

func TestStripMatchedUsageMetricsRemovesOnlyMatchedNames(t *testing.T) {
	keep := &metricpb.Metric{Name: "gen_ai.client.operation.duration", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{}}}
	drop := copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA)
	rm := &metricpb.ResourceMetrics{
		Resource:     &resourcepb.Resource{},
		ScopeMetrics: []*metricpb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "github.copilot"}, Metrics: []*metricpb.Metric{keep, drop}}},
	}
	matched := map[string]bool{copilotCallsMetric: true}
	out := stripMatchedUsageMetrics([]*metricpb.ResourceMetrics{rm}, matched)
	if len(out) != 1 || len(out[0].ScopeMetrics) != 1 || len(out[0].ScopeMetrics[0].Metrics) != 1 {
		t.Fatalf("stripped result = %+v", out)
	}
	if out[0].ScopeMetrics[0].Metrics[0].Name != "gen_ai.client.operation.duration" {
		t.Fatalf("kept metric = %q, want the unmatched one", out[0].ScopeMetrics[0].Metrics[0].Name)
	}

	// A ResourceMetrics left with nothing else is dropped entirely.
	onlyMatched := &metricpb.ResourceMetrics{
		Resource:     &resourcepb.Resource{},
		ScopeMetrics: []*metricpb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "github.copilot"}, Metrics: []*metricpb.Metric{drop}}},
	}
	out2 := stripMatchedUsageMetrics([]*metricpb.ResourceMetrics{onlyMatched}, matched)
	if len(out2) != 0 {
		t.Fatalf("stripped result = %+v, want an empty slice once every metric is matched", out2)
	}
}

func TestStripMatchedUsageMetricsNoopWhenNothingMatched(t *testing.T) {
	rms := []*metricpb.ResourceMetrics{{Resource: &resourcepb.Resource{}}}
	if got := stripMatchedUsageMetrics(rms, nil); len(got) != 1 {
		t.Fatalf("stripMatchedUsageMetrics with no matches must return the input unchanged, got %+v", got)
	}
}

// copilotUsageBatch builds one ResourceMetrics carrying the calls histogram
// and the token counters (including both cache counters, so the pipeline
// tests below exercise the real canonical-input subtraction, not just a
// passthrough), each with a vendor attribute (gen_ai.request.model) that is
// not in any GCP point-label allowlist for an ordinary native metric -- so
// on GCP, admission would reject them unless the deriver consumes them
// first.
func copilotUsageBatch() []*metricpb.ResourceMetrics {
	return []*metricpb.ResourceMetrics{copilotTokensResourceMetrics(
		metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, 1, 2, "gpt-5-copilot",
		tokenAmounts{input: 50, output: 12, cacheRead: 10, cacheWrite: 5},
	)}
}

// copilotUsageBatchWithCalls adds the calls histogram to copilotUsageBatch's
// token counters, since the pipeline tests below need both in the same
// export.
func copilotUsageBatchWithCalls() []*metricpb.ResourceMetrics {
	rms := copilotUsageBatch()
	rms[0].ScopeMetrics[0].Metrics = append(rms[0].ScopeMetrics[0].Metrics,
		copilotCallsMetricProto(metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			copilotHistPoint(2, 1, 2, "gpt-5-copilot")))
	return rms
}

// newCopilotDeriverPipeline builds a Pipeline with a real receiver listening
// (so the deriver's own loopback export has somewhere to land) and an active
// copilot UsageDeriver stored on it.
func newCopilotDeriverPipeline(t *testing.T, cloudProvider string) *Pipeline {
	t.Helper()
	t.Setenv("SCION_HARNESS", "copilot")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	cfg := &Config{Enabled: true, CloudProvider: cloudProvider, GRPCPort: availableTCPPort(t)}
	p := NewWithConfig(cfg)
	receiver := NewReceiver(cfg, nil, WithMetricHandler(p.handleMetrics))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Stop(context.Background()) })
	deriver, err := NewUsageDeriver(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(deriver.rules) == 0 {
		t.Fatal("expected the copilot usage rule to be active")
	}
	p.usageDeriver.Store(deriver)
	t.Cleanup(func() { _ = deriver.Shutdown(context.Background()) })
	return p
}

// TestPipelineConsumesCopilotUsageMetricsOnGCP is the §7.5 pipeline
// end-to-end variant for copilot, GCP mode: the raw histogram and counters
// are consumed (removed before metricStreams.add), so the request succeeds
// instead of being rejected for their unrecognized vendor attributes, and
// the canonical gen_ai.api.calls/scion.usage.tokens increments land in
// metricStreams with exactly the §3.2 label set and the cache-subtracted
// canonical input value.
func TestPipelineConsumesCopilotUsageMetricsOnGCP(t *testing.T) {
	p := newCopilotDeriverPipeline(t, "gcp")
	p.exporter = &CloudExporter{} // non-nil marker only: this test never flushes to Cloud.

	if err := p.handleMetrics(context.Background(), copilotUsageBatchWithCalls()); err != nil {
		t.Fatalf("handleMetrics returned an error, want the raw copilot metrics consumed rather than rejected: %v", err)
	}
	if got := p.metricDiagnostics.rejected.Load(); got != 0 {
		t.Fatalf("rejected data points = %d, want 0", got)
	}

	p.metricStateMu.Lock()
	snapshot := p.metricStreams.snapshot()
	p.metricStateMu.Unlock()

	// scion.usage.tokens can legitimately appear as more than one *metricpb.Metric
	// occurrence across the snapshot (metricStreams groups by resource/scope,
	// and separate Add calls for different token_type attribute sets are not
	// guaranteed to land in a single proto message) -- gather every data
	// point across every occurrence rather than keeping only the last-seen
	// Metric object, which would silently drop the others.
	var calls *metricpb.Metric
	var tokenPoints []*metricpb.NumberDataPoint
	for _, rm := range snapshot {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch m.Name {
				case telemetrycontract.MetricAPICalls:
					calls = m
				case telemetrycontract.MetricUsageTokens:
					tokenPoints = append(tokenPoints, m.GetSum().GetDataPoints()...)
				}
				if m.Name == copilotCallsMetric || m.Name == "gen_ai.client.inference.usage.input_tokens" {
					t.Fatalf("raw copilot metric %q reached metricStreams; it must be consumed on GCP, not admitted", m.Name)
				}
			}
		}
	}
	if calls == nil {
		t.Fatal("no gen_ai.api.calls stream derived")
	}
	if len(calls.GetSum().DataPoints) != 1 || calls.GetSum().DataPoints[0].GetAsInt() != 2 {
		t.Fatalf("gen_ai.api.calls = %+v, want a single point with value 2", calls.GetSum().DataPoints)
	}
	if len(tokenPoints) == 0 {
		t.Fatal("no scion.usage.tokens points derived")
	}
	if got := findTokenPoint(t, tokenPoints, telemetrycontract.TokenTypeInput); got != 35 { // 50 - 10 - 5
		t.Fatalf("input tokens = %d, want 35 (canonical, cache-subtracted)", got)
	}
	if got := findTokenPoint(t, tokenPoints, telemetrycontract.TokenTypeOutput); got != 12 {
		t.Fatalf("output tokens = %d, want 12", got)
	}
	if got := findTokenPoint(t, tokenPoints, telemetrycontract.TokenTypeCacheRead); got != 10 {
		t.Fatalf("cache_read tokens = %d, want 10", got)
	}
	if got := findTokenPoint(t, tokenPoints, telemetrycontract.TokenTypeCacheWrite); got != 5 {
		t.Fatalf("cache_write tokens = %d, want 5", got)
	}
}

func findTokenPoint(t *testing.T, points []*metricpb.NumberDataPoint, tokenType string) int64 {
	t.Helper()
	for _, pt := range points {
		if attrString(pt.Attributes, telemetrycontract.TokenTypeLabel) == tokenType {
			return pt.GetAsInt()
		}
	}
	t.Fatalf("no %s point among %+v", tokenType, points)
	return 0
}

func attrString(attrs []*commonpb.KeyValue, key string) string { return logAttrString(attrs, key) }

// TestPipelineUnmatchedNativeMetricStillRejectedOnGCPWithUsageDeriverActive
// is the control for the test above (AC "unmatched native metric: behaviour
// unchanged"): an unrelated native metric with the same kind of unrecognized
// vendor attribute must still be rejected on GCP, proving the deriver only
// consumes the metrics it actually recognizes.
func TestPipelineUnmatchedNativeMetricStillRejectedOnGCPWithUsageDeriverActive(t *testing.T) {
	p := newCopilotDeriverPipeline(t, "gcp")
	p.exporter = &CloudExporter{}

	unmatched := []*metricpb.ResourceMetrics{{
		Resource: &resourcepb.Resource{},
		ScopeMetrics: []*metricpb.ScopeMetrics{{
			Scope: &commonpb.InstrumentationScope{Name: "github.copilot"},
			Metrics: []*metricpb.Metric{{
				Name: "gen_ai.client.operation.duration",
				Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
					AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
					DataPoints:             []*metricpb.HistogramDataPoint{copilotHistPoint(1, 1, 2, "gpt-5-copilot")},
				}},
			}},
		}},
	}}
	if err := p.handleMetrics(context.Background(), unmatched); err == nil {
		t.Fatal("an unmatched native metric with a disallowed attribute must still be rejected on GCP")
	}
	if got := p.metricDiagnostics.rejected.Load(); got == 0 {
		t.Fatal("expected the rejection to be counted")
	}
}

// TestPipelineForwardsCopilotUsageMetricsOnGenericOTLP is the §7.5
// end-to-end variant for generic OTLP: the raw copilot metrics are
// forwarded unchanged, and the canonical counters are added alongside them
// (design §3.3).
func TestPipelineForwardsCopilotUsageMetricsOnGenericOTLP(t *testing.T) {
	p := newCopilotDeriverPipeline(t, "") // not gcp

	var captured *colmetricpb.ExportMetricsServiceRequest
	p.exporter = &CloudExporter{metricClient: &mockMetricClient{exportFunc: func(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest, _ ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		captured = req
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}}

	if err := p.handleMetrics(context.Background(), copilotUsageBatchWithCalls()); err != nil {
		t.Fatalf("handleMetrics: %v", err)
	}
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("metric flush not confirmed")
	}
	if captured == nil {
		t.Fatal("no export captured")
	}

	var sawRawCalls, sawRawInput, sawCanonicalCalls, sawCanonicalTokens bool
	for _, rm := range captured.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch m.Name {
				case copilotCallsMetric:
					sawRawCalls = true
				case "gen_ai.client.inference.usage.input_tokens":
					sawRawInput = true
				case telemetrycontract.MetricAPICalls:
					sawCanonicalCalls = true
				case telemetrycontract.MetricUsageTokens:
					sawCanonicalTokens = true
				}
			}
		}
	}
	if !sawRawCalls || !sawRawInput {
		t.Fatal("generic OTLP must forward the raw copilot metrics unchanged")
	}
	if !sawCanonicalCalls || !sawCanonicalTokens {
		t.Fatal("generic OTLP must also carry the derived canonical counters alongside the raw metrics")
	}
}
