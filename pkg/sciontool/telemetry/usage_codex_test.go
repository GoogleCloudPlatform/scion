/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

const codexUsageFixturePath = "testdata/usage/codex-0.158.0.pb.json"

// loadCodexUsageFixture loads a codex 0.158.0 payload built from source,
// not a capture: no codex binary was available in this environment to run
// and capture from (design §5 "Fixture gate" prefers a capture; the task
// brief for this rule allows deriving the exact shape from the codex-rs
// source instead, citing the revision, when a capture isn't possible).
//
// The shape is pinned to codex-rs/otel/src/events/session_telemetry.rs,
// function sse_event_completed and sse_event/see_event_completed_failed, at
// tag rust-v0.158.0 (github.com/openai/codex, commit
// 54e1bd264b4122fe9471ee7d54c4d021a76bb8ff), which is also the exact
// version harnesses/codex/Dockerfile installs by default
// (CODEX_CLI_VERSION=latest resolves to @openai/codex@0.158.0 as of this
// writing). The instrumentation scope name ("") matches
// opentelemetry-appender-tracing's OpenTelemetryTracingBridge::new, which
// codex-rs/otel/src/provider.rs's logger_export_layer uses unmodified ("the
// default scope uses an empty scope name for the appender logger").
//
// One caveat this synthesis can't verify without a capture: whether
// %-formatted numeric fields (input_token_count, output_token_count,
// tool_token_count -- Display-wrapped in the Rust source) are exported as
// OTLP stringValue, versus the bare i64 fields (cached_token_count,
// cache_write_token_count, reasoning_token_count, ttft_ms) as intValue, as
// modeled here. logAttrInt accepts either encoding for every field
// regardless, so this doesn't affect correctness either way -- see
// TestCodexUsageRuleTokenFieldTypeTolerance.
func loadCodexUsageFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	data, err := os.ReadFile(codexUsageFixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var req collogspb.ExportLogsServiceRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshaling fixture: %v", err)
	}
	if len(req.ResourceLogs) == 0 {
		t.Fatal("fixture has no resource logs")
	}
	return req.ResourceLogs
}

// codexFixtureRecordsByKind returns every log record in the fixture whose
// event.kind attribute equals kind. Every record in the codex fixture
// shares event.name=codex.sse_event, so event.kind is the discriminator
// (unlike the Claude fixture, whose events carry distinct event.name
// values).
func codexFixtureRecordsByKind(t *testing.T, kind string) []*logspb.LogRecord {
	t.Helper()
	var out []*logspb.LogRecord
	for _, rl := range loadCodexUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if logAttrString(record.Attributes, "event.kind") == kind {
					out = append(out, record)
				}
			}
		}
	}
	return out
}

func TestCodexUsageRuleMatchesFixtureResponseCompleted(t *testing.T) {
	records := codexFixtureRecordsByKind(t, "response.completed")
	if len(records) != 2 {
		t.Fatalf("fixture response.completed records = %d, want 2 (one usable, one parse-failure)", len(records))
	}
	// The usable record carries token fields; the parse-failure record
	// carries error.message instead (see
	// TestCodexUsageRuleCountsCallWithNoUsableTokensOnParseFailure).
	var usable *logspb.LogRecord
	for _, record := range records {
		if logAttrString(record.Attributes, "input_token_count") != "" {
			usable = record
		}
	}
	if usable == nil {
		t.Fatal("fixture missing the usable response.completed record")
	}

	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, usable, codexUsageScope), usable)
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("response.completed did not match codexUsageRule")
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess || increment.Model != "gpt-5.1-codex" {
		t.Fatalf("increment = %+v", increment)
	}
	// input_token_count=15000, cached_token_count=12000: canonical input is
	// the difference (design §5: "input = input_token_count −
	// cached_token_count"), because unlike Claude, codex's input_token_count
	// includes cache hits.
	want := map[string]int64{
		telemetrycontract.TokenTypeInput:     3000,
		telemetrycontract.TokenTypeOutput:    842,
		telemetrycontract.TokenTypeCacheRead: 12000,
		telemetrycontract.TokenTypeReasoning: 512,
	}
	if len(increment.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", increment.Tokens, want)
	}
	for k, v := range want {
		if increment.Tokens[k] != v {
			t.Errorf("tokens[%q] = %d, want %d", k, increment.Tokens[k], v)
		}
	}
	// cache_write is not part of design §5's codex mapping (see the
	// codexUsageRule doc comment), even though the source event also
	// carries cache_write_token_count.
	if _, ok := increment.Tokens[telemetrycontract.TokenTypeCacheWrite]; ok {
		t.Error("cache_write must not appear: design §5's codex row has no mapping for it")
	}
}

// TestCodexUsageRuleCountsCallWithNoUsableTokensOnParseFailure pins a real
// codex-rs edge case found in source: see_event_completed_failed also emits
// event.kind="response.completed" (so this event name/kind pair isn't
// unique to a successful parse) but carries no token fields at all, only
// error.message. Absent fields are not malformed (design §3.3,
// logAttrInt's doc comment), so this still matches and still counts a call,
// with no tokens -- consistent with the rule as specified, though it means
// a response whose usage payload failed to parse is indistinguishable from
// a zero-token success. Flagged to the project for follow-up.
func TestCodexUsageRuleCountsCallWithNoUsableTokensOnParseFailure(t *testing.T) {
	records := codexFixtureRecordsByKind(t, "response.completed")
	var failure *logspb.LogRecord
	for _, record := range records {
		if logAttrString(record.Attributes, "error.message") != "" {
			failure = record
		}
	}
	if failure == nil {
		t.Fatal("fixture missing the parse-failure response.completed record")
	}
	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, failure, codexUsageScope), failure)
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("response.completed (parse failure) did not match codexUsageRule")
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess {
		t.Fatalf("increment = %+v, want Calls=1 Status=success", increment)
	}
	if len(increment.Tokens) != 0 {
		t.Fatalf("increment.Tokens = %+v, want none when every token field is absent", increment.Tokens)
	}
}

func TestCodexUsageRuleIgnoresUnrelatedEvents(t *testing.T) {
	rule := codexUsageRule{}
	records := codexFixtureRecordsByKind(t, "text_delta")
	if len(records) == 0 {
		t.Fatal("fixture missing text_delta records")
	}
	if _, matched, err := rule.MatchLog(codexUsageScope, mustEventName(t, records[0], codexUsageScope), records[0]); matched || err != nil {
		t.Errorf("event.kind=text_delta: matched=%v err=%v, want matched=false err=nil", matched, err)
	}

	// Wrong scope never matches, even for a real response.completed record.
	completed := codexFixtureRecordsByKind(t, "response.completed")[0]
	if _, matched, _ := rule.MatchLog("some.other.scope", mustEventName(t, completed, codexUsageScope), completed); matched {
		t.Error("codexUsageRule matched outside its native (empty) scope")
	}
}

func TestCodexUsageRuleMalformedTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "gpt-5.1-codex"}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "not-a-number"}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, record, codexUsageScope), record)
	if !matched {
		t.Fatal("expected the malformed response.completed to still match (so it counts as usage_malformed, not silently ignored)")
	}
	if err == nil {
		t.Fatal("expected a malformed-field error")
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess {
		t.Fatalf("increment = %+v, want Calls=1 Status=success even when malformed", increment)
	}
	if len(increment.Tokens) != 0 {
		t.Fatalf("increment.Tokens = %+v, want none when any token field is malformed", increment.Tokens)
	}
}

func TestCodexUsageRuleNegativeTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "output_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -1}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, record, codexUsageScope), record)
	if !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
	}
	if increment.Calls != 1 {
		t.Fatalf("increment.Calls = %d, want 1 even when malformed", increment.Calls)
	}
}

// TestCodexUsageRuleCachedExceedsInputIsMalformed pins the codex-specific
// malformed case the brief calls out: cached_token_count > input_token_count
// would make input = input_token_count − cached_token_count negative, which
// the design's ">=0" token invariant (§3.3) forbids.
func TestCodexUsageRuleCachedExceedsInputIsMalformed(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "10"}}},
		{Key: "cached_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 20}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, record, codexUsageScope), record)
	if !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil when cached exceeds input", matched, err)
	}
	if increment.Calls != 1 || len(increment.Tokens) != 0 {
		t.Fatalf("increment = %+v, want Calls=1 and no tokens", increment)
	}
}

// TestCodexUsageRuleTokenFieldTypeTolerance pins the same type tolerance as
// Claude's rule (design §3.3, logAttrInt): a string-encoded non-negative
// integer and an integral double must both be accepted. This also covers
// this fixture's central uncertainty (see loadCodexUsageFixture's doc
// comment): whether a given field arrives as stringValue or intValue
// doesn't change the derived increment.
func TestCodexUsageRuleTokenFieldTypeTolerance(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "50"}}},
		{Key: "cached_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 20}}},
		{Key: "output_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, record, codexUsageScope), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	if increment.Tokens[telemetrycontract.TokenTypeInput] != 30 || increment.Tokens[telemetrycontract.TokenTypeOutput] != 7 || increment.Tokens[telemetrycontract.TokenTypeCacheRead] != 20 {
		t.Fatalf("increment.Tokens = %+v, want input=30 output=7 cache_read=20", increment.Tokens)
	}
}

// TestCodexUsageRuleMatchesEventNameField pins that MatchLog trusts its
// eventName argument (already resolved by normalizedLogEventName, which
// recognizes either LogRecord.EventName or an event.name-family attribute),
// the same interface contract Claude's rule relies on.
func TestCodexUsageRuleMatchesEventNameField(t *testing.T) {
	record := &logspb.LogRecord{
		EventName: codexUsageEventName,
		Attributes: []*commonpb.KeyValue{
			{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		},
	}
	increment, matched, err := codexUsageRule{}.MatchLog(codexUsageScope, mustEventName(t, record, codexUsageScope), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil for a native EventName field", matched, err)
	}
	if increment.Calls != 1 {
		t.Fatalf("increment = %+v, want Calls=1", increment)
	}
}

func TestUsageDeriverObserveDedupesReplayedCodexRequest(t *testing.T) {
	d := bareUsageDeriver(codexUsageRule{})
	records := codexFixtureRecordsByKind(t, "response.completed")
	var usable *logspb.LogRecord
	for _, record := range records {
		if logAttrString(record.Attributes, "input_token_count") != "" {
			usable = record
		}
	}
	if usable == nil {
		t.Fatal("fixture missing the usable response.completed record")
	}

	if !d.observe(context.Background(), codexUsageScope, usable) {
		t.Fatal("first observation should record")
	}
	if d.observe(context.Background(), codexUsageScope, usable) {
		t.Fatal("replayed observation should be deduped, not recorded again")
	}
	diag := d.Diagnostics()
	if diag.Derived != 1 || diag.Duplicate != 1 || diag.Malformed != 0 {
		t.Fatalf("diagnostics = %+v", diag)
	}
}

// TestNewUsageDeriverBuildsCodexRuleForCodexHarness pins that the codex
// harness gets exactly the codex rule (not Claude's), and that it remains a
// no-op for any other configuration -- complementing
// TestNewUsageDeriverIsNoOpForUnknownHarness and
// TestNewUsageDeriverIsNoOpWithoutNativeSource in usage_test.go, which
// already cover the harness-agnostic mechanism.
func TestNewUsageDeriverBuildsCodexRuleForCodexHarness(t *testing.T) {
	t.Setenv("SCION_HARNESS", "codex")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 1 {
		t.Fatalf("codex deriver rules = %d, want 1", len(d.rules))
	}
	if _, ok := d.rules[0].(codexUsageRule); !ok {
		t.Fatalf("codex deriver rule = %T, want codexUsageRule", d.rules[0])
	}
}
