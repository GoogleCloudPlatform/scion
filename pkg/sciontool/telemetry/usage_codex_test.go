/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

const codexUsageFixturePath = "testdata/usage/codex-0.158.0.pb.json"

// loadCodexUsageFixture loads a codex 0.158.0 payload. All four records are
// a scrubbed local capture: npm-installed @openai/codex@0.158.0, run as
// `codex exec` against a local mock Responses-API server (a tiny Python
// HTTP server streaming a fixed SSE sequence) and a local OTLP/HTTP+JSON
// log sink -- no network calls, no real API key, no Anthropic/OpenAI
// credentials. The fourth record (the failed-response event,
// see_event_completed_failed) came from a second run of the same mock,
// configured to end the SSE body without a response.completed frame, which
// drives the same failed-request code path as a real mid-stream
// disconnect; codex retries stream errors, so the fixture keeps only one
// of the resulting records.
//
// Scrubbed at the resource level: host.name is replaced with a placeholder
// ("scrubbed-host"); env and service.name are the capture's real values
// ("test" and "codex_exec", the `codex exec` subcommand's service name)
// and are not edited.
//
// Scrubbed or substituted per record: conversation.id is replaced with a
// placeholder; model/slug are replaced with a realistic value (the
// capture used a placeholder mock model name); originator is normalized
// from the capture's "codex_exec" to "codex_cli_rs" (interactive mode,
// what harnesses/codex's provision.py actually launches, per
// harnesses/authoring-guide.md's "always configure interactive/REPL mode"
// requirement) since the two subcommands' originator differs and
// interactive is what production runs.
//
// The token counts in the completion record are the mock server's
// configured usage block (1500/80/1200/0/20/1580 for
// input/output/cached/cache_write/reasoning/tool), not a real model's
// output, and are not edited. The fixture is also a *subset* of what the
// capture produced: a response.created frame and a response.output_item.done
// frame were emitted too but are omitted here, since the rule ignores
// every event.kind other than response.completed (the included
// output_text.delta frame is kept as a negative case).
//
// Every other field -- every attribute key, value type (stringValue vs
// intValue), the scope name, and every record's
// observedTimeUnixNano/event.timestamp -- is exactly what the capture
// produced. Verified by a script that matches each fixture record to its
// raw captured record (by observedTimeUnixNano) and diffs every resource
// attribute, scope name and record attribute: the only differences found
// are the scrubbing and substitutions listed above, nothing else.
//
// See codexUsageRule's doc comment for which emitter each record models
// and why. Every record's LogRecord.EventName is the literal
// tracing-appender callsite string for its emitting
// log_event!/log_and_trace_event! call ("event
// otel/src/events/session_telemetry.rs:<line>"), confirmed by the capture:
// without recognizing this shape, normalizedLogEventName treats it as
// conflicting with the record's real event.name attribute and rejects the
// whole batch (see the policy.go changes alongside this file).
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

// codexFixtureRecordByEventName returns the one fixture log record whose
// LogRecord.EventName equals name, failing the test unless exactly one
// matches. Use this only for an EventName unique to one record in the
// fixture; sse_event()'s two records (the delta frame and the per-frame
// response.completed marker) share one callsite EventName, so
// codexFixtureRecordByEventNameAndKind disambiguates those by event.kind
// instead.
func codexFixtureRecordByEventName(t *testing.T, name string) *logspb.LogRecord {
	t.Helper()
	var out *logspb.LogRecord
	count := 0
	for _, rl := range loadCodexUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if record.GetEventName() == name {
					out = record
					count++
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("fixture records with EventName %q = %d, want 1", name, count)
	}
	return out
}

// codexFixtureRecordByEventNameAndKind is codexFixtureRecordByEventName
// plus an event.kind match, for the two sse_event() records that share one
// callsite EventName (see that function's doc comment).
func codexFixtureRecordByEventNameAndKind(t *testing.T, name, kind string) *logspb.LogRecord {
	t.Helper()
	var out *logspb.LogRecord
	count := 0
	for _, rl := range loadCodexUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if record.GetEventName() == name && logAttrString(record.Attributes, "event.kind") == kind {
					out = record
					count++
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("fixture records with EventName %q kind %q = %d, want 1", name, kind, count)
	}
	return out
}

// codexFixtureSseEventCallsite is the shared tracing-appender callsite
// EventName for sse_event()'s two records: the plain delta frame and the
// per-frame response.completed marker. Real codex would emit this
// identical EventName for both, since they come from the same source line;
// event.kind is what distinguishes them.
const codexFixtureSseEventCallsite = "event otel/src/events/session_telemetry.rs:1039"

const (
	codexFixtureCompletedEventName = "event otel/src/events/session_telemetry.rs:1103" // sse_event_completed(), the real usage event
	codexFixtureFailedEventName    = "event otel/src/events/session_telemetry.rs:1090" // see_event_completed_failed(), the failed-request event
)

func TestCodexUsageRuleMatchesFixtureResponseCompleted(t *testing.T) {
	record := codexFixtureRecordByEventName(t, codexFixtureCompletedEventName)

	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("sse_event_completed did not match codexUsageRule")
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess || increment.Model != "gpt-5.1-codex" {
		t.Fatalf("increment = %+v", increment)
	}
	// input_token_count=1500, cached_token_count=1200: canonical input is
	// the difference (design §5: "input = input_token_count −
	// cached_token_count"), because unlike Claude, codex's input_token_count
	// includes cache hits.
	want := map[string]int64{
		telemetrycontract.TokenTypeInput:     300,
		telemetrycontract.TokenTypeOutput:    80,
		telemetrycontract.TokenTypeCacheRead: 1200,
		telemetrycontract.TokenTypeReasoning: 20,
	}
	if len(increment.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", increment.Tokens, want)
	}
	for k, v := range want {
		if increment.Tokens[k] != v {
			t.Errorf("tokens[%q] = %d, want %d", k, increment.Tokens[k], v)
		}
	}
	// The 0.158.0 capture carries cache_write_token_count=0, and a zero
	// count is never emitted as a token point. The nonzero cache_write
	// mapping (ptone/scion#2245) is pinned by
	// TestCodexUsageRuleMapsCacheWriteFromFixture against the 0.160.0
	// capture.
	if !logAttrPresent(record.Attributes, "cache_write_token_count") {
		t.Fatal("fixture is expected to carry cache_write_token_count (0)")
	}
	if _, ok := increment.Tokens[telemetrycontract.TokenTypeCacheWrite]; ok {
		t.Error("a zero cache_write_token_count must not produce a cache_write token point")
	}
}

// TestCodexUsageRuleExcludesPerFrameMarker pins that the per-frame marker
// is excluded: sse_event() emits a record with the same event.name/
// event.kind as sse_event_completed for every SSE frame, including a
// plain "response.completed" frame with no usage attached yet. Matching
// it as a second, zero-token call would double-count one model response
// as two calls. The discriminator is duration_ms, which only the
// per-frame emitter ever sets.
func TestCodexUsageRuleExcludesPerFrameMarker(t *testing.T) {
	record := codexFixtureRecordByEventNameAndKind(t, codexFixtureSseEventCallsite, codexUsageEventKind)
	if _, matched, err := (codexUsageRule{}).MatchLog("", mustEventName(t, record, ""), record); matched || err != nil {
		t.Errorf("per-frame response.completed marker: matched=%v err=%v, want matched=false err=nil", matched, err)
	}
}

// TestCodexUsageRuleMapsFailedResponseToError pins that
// see_event_completed_failed reports a failed request (a transport or API
// error client.rs's map_api_error produced), mirroring the Claude rule's
// api_error arm -- Calls=1, Status=error, no tokens, and not malformed (a
// reported error is not a malformed event).
func TestCodexUsageRuleMapsFailedResponseToError(t *testing.T) {
	record := codexFixtureRecordByEventName(t, codexFixtureFailedEventName)
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("see_event_completed_failed did not match codexUsageRule")
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusError {
		t.Fatalf("increment = %+v, want Calls=1 Status=error", increment)
	}
	if len(increment.Tokens) != 0 {
		t.Fatalf("increment.Tokens = %+v, want none for a failed response", increment.Tokens)
	}
}

// TestCodexUsageRuleExcludesPerFrameFailureMarker pins the check ordering:
// sse_event_failed (the per-frame sibling of see_event_completed_failed)
// can also carry event.kind=response.completed and error.message, but it
// always carries duration_ms too. The duration_ms exclusion must run
// before the error.message check, so this record is excluded outright
// rather than counted as a failed call.
func TestCodexUsageRuleExcludesPerFrameFailureMarker(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "1200"}}},
		{Key: "error.message", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "idle timeout waiting for SSE"}}},
	}}
	if _, matched, err := (codexUsageRule{}).MatchLog("", mustEventName(t, record, ""), record); matched || err != nil {
		t.Errorf("per-frame failure marker (duration_ms + error.message): matched=%v err=%v, want matched=false err=nil", matched, err)
	}
}

func TestCodexUsageRuleIgnoresUnrelatedEvents(t *testing.T) {
	record := codexFixtureRecordByEventNameAndKind(t, codexFixtureSseEventCallsite, "response.output_text.delta")
	if _, matched, err := (codexUsageRule{}).MatchLog("", mustEventName(t, record, ""), record); matched || err != nil {
		t.Errorf("event.kind=response.output_text.delta: matched=%v err=%v, want matched=false err=nil", matched, err)
	}
}

// TestCodexUsageRuleScopeIsNotReliedOn pins that the rule does not gate on
// instrumentation scope (unlike Claude's, which does). An arbitrary,
// non-empty scope name must not stop a real match.
func TestCodexUsageRuleScopeIsNotReliedOn(t *testing.T) {
	record := codexFixtureRecordByEventName(t, codexFixtureCompletedEventName)
	if _, matched, err := (codexUsageRule{}).MatchLog("some.other.scope", mustEventName(t, record, "some.other.scope"), record); !matched || err != nil {
		t.Errorf("matched=%v err=%v under an arbitrary scope, want matched=true err=nil (scope is not relied on)", matched, err)
	}
}

func TestCodexUsageRuleMalformedTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "gpt-5.1-codex"}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "not-a-number"}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
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
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
	}
	if increment.Calls != 1 {
		t.Fatalf("increment.Calls = %d, want 1 even when malformed", increment.Calls)
	}
}

// TestCodexUsageRuleCachedExceedsInputIsMalformed pins the codex-specific
// malformed case: cached_token_count > input_token_count would make
// input = input_token_count − cached_token_count negative, which the
// design's ">=0" token invariant (§3.3) forbids.
func TestCodexUsageRuleCachedExceedsInputIsMalformed(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "10"}}},
		{Key: "cached_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 20}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil when cached exceeds input", matched, err)
	}
	if increment.Calls != 1 || len(increment.Tokens) != 0 {
		t.Fatalf("increment = %+v, want Calls=1 and no tokens", increment)
	}
}

// TestCodexUsageRuleTokenFieldTypeTolerance pins the same type tolerance as
// Claude's rule (design §3.3, logAttrInt): a string-encoded non-negative
// integer and an integral double must both be accepted.
func TestCodexUsageRuleTokenFieldTypeTolerance(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "50"}}},
		{Key: "cached_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 20}}},
		{Key: "output_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
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
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil for a native EventName field", matched, err)
	}
	if increment.Calls != 1 {
		t.Fatalf("increment = %+v, want Calls=1", increment)
	}
}

func TestUsageDeriverObserveDedupesReplayedCodexRequest(t *testing.T) {
	d := bareUsageDeriver(codexUsageRule{})
	record := codexFixtureRecordByEventName(t, codexFixtureCompletedEventName)

	if !d.observe(context.Background(), "", record) {
		t.Fatal("first observation should record")
	}
	if d.observe(context.Background(), "", record) {
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

// TestPipelineDerivesCodexUsageThroughValidation is an end-to-end
// regression test: without the tracing-appender callsite-EventName
// exemption in normalizedLogEventName, validateLogs rejects this fixture's
// batch outright with "conflicting event name representations" before the
// deriver ever runs, since every record's LogRecord.EventName (the
// tracing-appender callsite default) conflicts with its event.name
// attribute. It posts a full, realistic response sequence (a delta frame,
// the per-frame response.completed marker, and the token-bearing
// completion) plus one failed response, and asserts the resulting
// canonical counters and point label keys end to end (handleLogs ->
// validateLogs -> the deriver -> the loopback metrics path -> metricStreams
// -> the GCP exporter), the same shape as TestPipelineDerivesClaudeUsageEndToEnd
// but without that test's golden-file and replay-dedup assertions, which
// are harness-agnostic and already covered there.
func TestPipelineDerivesCodexUsageThroughValidation(t *testing.T) {
	result := deriveUsageThroughPipeline(t, "codex", loadCodexUsageFixture(t))

	// The delta frame and the per-frame response.completed marker must not
	// add a second call for the one successful response -- Derived counts
	// one increment per matched record, so this is 2 (one success, one
	// error), not 3 or 4.
	if result.diag.Derived != 2 || result.diag.Malformed != 0 {
		t.Fatalf("diagnostics = %+v, want Derived=2 Malformed=0 (no per-frame double count)", result.diag)
	}
	// Exactly one success call and one error call, not two successes
	// (which is what a per-frame double count plus a mis-mapped error
	// status would produce together).
	if len(result.callsByStatus) != 2 || result.callsByStatus["success"] != 1 || result.callsByStatus["error"] != 1 {
		t.Fatalf("calls by status = %+v, want success=1 error=1", result.callsByStatus)
	}
	assertTokensByType(t, result.tokensByType, map[string]int64{
		telemetrycontract.TokenTypeInput:     300,
		telemetrycontract.TokenTypeOutput:    80,
		telemetrycontract.TokenTypeCacheRead: 1200,
		telemetrycontract.TokenTypeReasoning: 20,
	})
	for _, labels := range result.tokenLabels {
		if labels["harness"] != "codex" || labels["model"] != "gpt-5.1-codex" {
			t.Errorf("unexpected tokens series labels: %+v", labels)
		}
	}
}

// pipelineUsageResult is what deriveUsageThroughPipeline observed at the
// GCP exporter: calls summed per status label, tokens summed per
// token_type label, plus every scion.usage.tokens series' labels.
type pipelineUsageResult struct {
	diag          UsageDiagnostics
	callsByStatus map[string]int64
	tokensByType  map[string]int64
	tokenLabels   []map[string]string
}

// deriveUsageThroughPipeline is the end-to-end harness the per-harness
// pipeline tests share: it builds a real Pipeline with a native-source
// usage deriver for harness, feeds resourceLogs through handleLogs (so
// validateLogs and the pre-filter derivation both run), flushes the
// loopback metrics through metricStreams to a fake Cloud Monitoring
// server, and returns the canonical counters it captured. It also asserts
// the exact §3.2 point label keys on every gen_ai.api.calls and
// scion.usage.tokens series, and the canonical identity labels on the
// tokens series, so each caller only asserts its own values.
func deriveUsageThroughPipeline(t *testing.T, harness string, resourceLogs []*logspb.ResourceLogs) pipelineUsageResult {
	t.Helper()
	t.Setenv("SCION_AGENT_ID", "agent-"+harness+"-pipeline-1")
	t.Setenv("SCION_AGENT_SLUG", harness+"-agent-slug")
	t.Setenv("SCION_PROJECT_ID", "project-"+harness+"-pipeline-1")
	t.Setenv("SCION_HARNESS", harness)
	t.Setenv("SCION_MODEL", "")
	t.Setenv("SCION_BROKER_ID", "")
	t.Setenv("SCION_BROKER_NAME", "")
	t.Setenv("SCION_GCP_PROJECT_ID", "")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	capture := &monitoringCapture{}
	server := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	sdkExporter, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdkExporter.Shutdown(context.Background()) })

	cfg := &Config{
		Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t), HTTPPort: 0,
		// No real event name is included, so every raw native log record is
		// dropped by the filter and handleLogs returns before attempting a
		// (here unconfigured) raw-log export. The derived usage metrics,
		// which run before the filter (design §3.3, AC-1.4), must still
		// appear.
		Filter: FilterConfig{Include: []string{"nonexistent_event"}},
	}
	p := NewWithConfig(cfg)
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sdkExporter}}
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	p.metricNow = func() time.Time { return now }

	receiver := NewReceiver(cfg, nil, WithLogHandler(p.handleLogs), WithMetricHandler(p.handleMetrics))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Stop(context.Background()) })

	deriver, err := NewUsageDeriver(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(deriver.rules) == 0 {
		t.Fatalf("expected a usage rule to be active for harness %q", harness)
	}
	p.usageDeriver.Store(deriver)
	t.Cleanup(func() { _ = deriver.Shutdown(context.Background()) })

	// For codex, without the callsite-EventName exemption in
	// normalizedLogEventName this returns the InvalidArgument "conflicting
	// event name representations" policy-rejection error, and nothing
	// below ever runs.
	if err := p.handleLogs(context.Background(), resourceLogs); err != nil {
		t.Fatalf("handleLogs rejected a realistic %s fixture: %v", harness, err)
	}
	now = now.Add(10 * time.Millisecond)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("metric flush not confirmed")
	}

	result := pipelineUsageResult{
		diag:          p.UsageDiagnostics(),
		callsByStatus: map[string]int64{},
		tokensByType:  map[string]int64{},
	}
	for _, ts := range allCapturedSeries(capture) {
		switch ts.Metric.Type {
		case "workload.googleapis.com/gen_ai.api.calls":
			assertLabelKeys(t, ts.Metric.Labels, "agent_id", "project_id", "harness", "model", "status",
				"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
				"scion_agent_id", "scion_project_id", "scion_agent_slug",
				"service_name", "service_instance_id")
			result.callsByStatus[ts.Metric.Labels["status"]] += ts.Points[0].Value.GetInt64Value()
		case "workload.googleapis.com/scion.usage.tokens":
			// Exactly {harness, model, token_type} plus the exporter-stamped
			// canonical identity labels -- nothing else (design §3.2).
			assertLabelKeys(t, ts.Metric.Labels, "harness", "model", "token_type",
				"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
				"scion_agent_id", "scion_project_id", "scion_agent_slug",
				"service_name", "service_instance_id")
			if ts.Metric.Labels["scion_agent_id"] != "agent-"+harness+"-pipeline-1" ||
				ts.Metric.Labels["scion_project_id"] != "project-"+harness+"-pipeline-1" ||
				ts.Metric.Labels["scion_agent_slug"] != harness+"-agent-slug" {
				t.Errorf("unexpected canonical identity labels: %+v", ts.Metric.Labels)
			}
			result.tokensByType[ts.Metric.Labels["token_type"]] += ts.Points[0].Value.GetInt64Value()
			result.tokenLabels = append(result.tokenLabels, ts.Metric.Labels)
		}
	}
	return result
}

// assertTokensByType requires got to hold exactly the token types in want,
// with the same values.
func assertTokensByType(t *testing.T, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("token types = %+v, want %+v", got, want)
	}
	for tokenType, n := range want {
		if got[tokenType] != n {
			t.Errorf("tokens[%q] = %d, want %d", tokenType, got[tokenType], n)
		}
	}
}

const codex160UsageFixturePath = "testdata/usage/codex-0.160.0.pb.json"

// loadCodex160UsageFixture loads the codex 0.160.0 capture, made the same
// way as loadCodexUsageFixture's (npm-installed @openai/codex@0.160.0, run
// as `codex exec` against a local mock Responses-API server, no network
// calls and no real credentials), but exported over OTLP/gRPC to a local
// sink -- the protocol harnesses/codex's provision.py configures
// (exporter."otlp-grpc") -- rather than OTLP/HTTP. It pins the two
// behaviours added in 0.160.0's rule (ptone/scion#2245, #2246). It holds
// three ResourceLogs, one per run:
//
//  1. The mock returns HTTP 500 for the first /responses attempt, and codex
//     retries (request_max_retries) to a successful stream: an api_request
//     with http.response.status_code=500 and error.message="http 500", an
//     api_request with status 200, the per-frame response.completed marker,
//     and sse_event_completed with the mock's usage block
//     (input/cached/cache_write/output/reasoning/total =
//     1500/1000/300/80/20/1580; codex printed "tokens used 580").
//  2. The mock answers the first attempt with HTTP 200 and an SSE body cut
//     after response.created, and codex retries the stream
//     (stream_max_retries) to success: api_request status 200 (no
//     error.message), see_event_completed_failed ("stream closed before
//     response.completed"), then api_request 200, the per-frame marker and
//     sse_event_completed again. This is the no-double-count case: the
//     failed stream's api_request is a 2xx.
//  3. base_url points at a closed port: an api_request with no
//     http.response.status_code and error.message="error sending request".
//     codex keeps retrying ("Reconnecting... waiting for network"); the
//     fixture keeps only the first of those identical records.
//
// Records unrelated to usage (conversation_starts, startup_phase,
// user_prompt, turn_ttft, the non-completed SSE frames, and the
// codex_otel::metrics::client scope, which carried no records) are omitted.
// Scrubbed: resource host.name ("scrubbed-host") and every record's
// conversation.id ("scrubbed-conversation-id"). Everything else -- attribute
// keys, value types, model ("gpt-5.1-codex", set in the capture's
// config.toml), originator and service.name ("codex_exec", the `codex exec`
// subcommand's real values, not normalized to interactive mode), scope
// name, timestamps and LogRecord.EventName callsites -- is exactly what the
// capture produced.
func loadCodex160UsageFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	data, err := os.ReadFile(codex160UsageFixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var req collogspb.ExportLogsServiceRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshaling fixture: %v", err)
	}
	if len(req.ResourceLogs) != 3 {
		t.Fatalf("fixture resource logs = %d, want 3 (one per capture run)", len(req.ResourceLogs))
	}
	return req.ResourceLogs
}

// codex160Records returns run's (0-based) log records whose event.name is
// eventName, in capture order.
func codex160Records(t *testing.T, run int, eventName string) []*logspb.LogRecord {
	t.Helper()
	var out []*logspb.LogRecord
	for _, sl := range loadCodex160UsageFixture(t)[run].ScopeLogs {
		for _, record := range sl.LogRecords {
			if logAttrString(record.Attributes, "event.name") == eventName {
				out = append(out, record)
			}
		}
	}
	return out
}

func TestCodexUsageRuleMapsCacheWriteFromFixture(t *testing.T) {
	var completion *logspb.LogRecord
	for _, record := range codex160Records(t, 0, codexUsageEventName) {
		if record.GetEventName() == codexFixtureCompletedEventName {
			completion = record
		}
	}
	if completion == nil {
		t.Fatal("run 1 has no sse_event_completed record")
	}
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, completion, ""), completion)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess || increment.Model != "gpt-5.1-codex" {
		t.Fatalf("increment = %+v", increment)
	}
	// input_token_count=1500 includes both cached_token_count=1000 and
	// cache_write_token_count=300 (both come from the Responses API's
	// usage.input_tokens_details), so canonical input is 1500-1000-300 and
	// input+cache_read+cache_write == input_token_count (design §3.2).
	assertTokensByType(t, increment.Tokens, map[string]int64{
		telemetrycontract.TokenTypeInput:      200,
		telemetrycontract.TokenTypeOutput:     80,
		telemetrycontract.TokenTypeCacheRead:  1000,
		telemetrycontract.TokenTypeCacheWrite: 300,
		telemetrycontract.TokenTypeReasoning:  20,
	})
}

// TestCodexUsageRuleCacheReadPlusWriteExceedsInputIsMalformed pins the
// extended bound: cached and cache_write are each within input, but their
// sum is not, which would make canonical input negative.
func TestCodexUsageRuleCacheReadPlusWriteExceedsInputIsMalformed(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "100"}}},
		{Key: "cached_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 60}}},
		{Key: "cache_write_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 50}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
	}
	if increment.Calls != 1 || len(increment.Tokens) != 0 {
		t.Fatalf("increment = %+v, want Calls=1 and no tokens", increment)
	}
}

func TestCodexUsageRuleMalformedCacheWriteField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventName}}},
		{Key: "event.kind", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexUsageEventKind}}},
		{Key: "input_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "100"}}},
		{Key: "cache_write_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "lots"}}},
	}}
	increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
	if !matched || err == nil || increment.Calls != 1 || len(increment.Tokens) != 0 {
		t.Fatalf("matched=%v err=%v increment=%+v, want a counted call with no tokens and a malformed error", matched, err, increment)
	}
}

// TestCodexUsageRuleAPIRequestFromFixture walks every captured
// codex.api_request record and pins which ones count: only run 1's HTTP 500
// attempt and run 3's transport error (no status code). Both 2xx attempts
// in run 2 -- including the one whose stream then failed and was counted by
// see_event_completed_failed -- and run 1's retried 2xx attempt must not
// match.
func TestCodexUsageRuleAPIRequestFromFixture(t *testing.T) {
	cases := []struct {
		run       int
		wantError []bool // per api_request record, in capture order
	}{
		{run: 0, wantError: []bool{true, false}},
		{run: 1, wantError: []bool{false, false}},
		{run: 2, wantError: []bool{true}},
	}
	for _, tc := range cases {
		records := codex160Records(t, tc.run, codexAPIRequestEventName)
		if len(records) != len(tc.wantError) {
			t.Fatalf("run %d api_request records = %d, want %d", tc.run+1, len(records), len(tc.wantError))
		}
		for i, record := range records {
			increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
			if err != nil {
				t.Fatalf("run %d api_request %d: MatchLog error: %v", tc.run+1, i, err)
			}
			if matched != tc.wantError[i] {
				t.Fatalf("run %d api_request %d: matched=%v, want %v", tc.run+1, i, matched, tc.wantError[i])
			}
			if matched && (increment.Calls != 1 || increment.Status != telemetrycontract.StatusError || len(increment.Tokens) != 0 || increment.Model != "gpt-5.1-codex") {
				t.Fatalf("run %d api_request %d: increment = %+v, want one tokenless error call", tc.run+1, i, increment)
			}
		}
	}
}

func codexAPIRequestRecord(extra ...*commonpb.KeyValue) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: codexAPIRequestEventName}}},
		{Key: "duration_ms", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "12"}}},
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "gpt-5.1-codex"}}},
	}
	return &logspb.LogRecord{Attributes: append(attrs, extra...)}
}

func TestCodexUsageRuleAPIRequestCases(t *testing.T) {
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	num := func(k string, v int64) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
	}
	cases := []struct {
		name      string
		attrs     []*commonpb.KeyValue
		wantError bool
	}{
		{"2xx success is the sse completion's call, not counted here", []*commonpb.KeyValue{num("http.response.status_code", 200), str("endpoint", "/responses")}, false},
		{"204 is still success", []*commonpb.KeyValue{num("http.response.status_code", 204)}, false},
		{"429 without error.message", []*commonpb.KeyValue{num("http.response.status_code", 429), str("endpoint", "/responses")}, true},
		{"string-encoded 503", []*commonpb.KeyValue{str("http.response.status_code", "503")}, true},
		{"3xx is not 2xx", []*commonpb.KeyValue{num("http.response.status_code", 302)}, true},
		{"error.message with no status (transport error)", []*commonpb.KeyValue{str("error.message", "error sending request")}, true},
		{"error.message with a 2xx status", []*commonpb.KeyValue{num("http.response.status_code", 200), str("error.message", "boom")}, true},
		{"no endpoint attribute is treated as /responses", []*commonpb.KeyValue{num("http.response.status_code", 500)}, true},
		{"other endpoint failure is not counted", []*commonpb.KeyValue{num("http.response.status_code", 500), str("endpoint", "/memories/trace_summarize")}, false},
		{"malformed status alone is not a failure", []*commonpb.KeyValue{str("http.response.status_code", "teapot")}, false},
		{"no status and no error is not a failure", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := codexAPIRequestRecord(tc.attrs...)
			increment, matched, err := codexUsageRule{}.MatchLog("", mustEventName(t, record, ""), record)
			if err != nil {
				t.Fatalf("MatchLog error: %v", err)
			}
			if matched != tc.wantError {
				t.Fatalf("matched=%v, want %v", matched, tc.wantError)
			}
			if matched && (increment.Calls != 1 || increment.Status != telemetrycontract.StatusError || len(increment.Tokens) != 0) {
				t.Fatalf("increment = %+v, want one tokenless error call", increment)
			}
		})
	}
}

// TestPipelineDerivesCodex160UsageEndToEnd runs the whole 0.160.0 capture
// through the pipeline: two successful responses (with cache_write) and
// three failed attempts -- an HTTP 500 api_request, a stream cut after a
// 2xx (counted once, by see_event_completed_failed, not again by its 2xx
// api_request), and a transport-error api_request.
func TestPipelineDerivesCodex160UsageEndToEnd(t *testing.T) {
	result := deriveUsageThroughPipeline(t, "codex", loadCodex160UsageFixture(t))
	if result.diag.Derived != 5 || result.diag.Malformed != 0 || result.diag.Duplicate != 0 {
		t.Fatalf("diagnostics = %+v, want Derived=5 Malformed=0 Duplicate=0", result.diag)
	}
	if len(result.callsByStatus) != 2 || result.callsByStatus["success"] != 2 || result.callsByStatus["error"] != 3 {
		t.Fatalf("calls by status = %+v, want success=2 error=3", result.callsByStatus)
	}
	assertTokensByType(t, result.tokensByType, map[string]int64{
		telemetrycontract.TokenTypeInput:      400,
		telemetrycontract.TokenTypeOutput:     160,
		telemetrycontract.TokenTypeCacheRead:  2000,
		telemetrycontract.TokenTypeCacheWrite: 600,
		telemetrycontract.TokenTypeReasoning:  40,
	})
}
