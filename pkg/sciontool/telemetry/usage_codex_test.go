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
// Scrubbed: conversation.id and host.name are replaced with placeholders;
// model/slug are replaced with a realistic value (the capture used a
// placeholder mock model name); originator is normalized from the
// capture's "codex_exec" (the `codex exec` subcommand) to "codex_cli_rs"
// (interactive mode, what harnesses/codex's provision.py actually
// launches, per harnesses/authoring-guide.md's "always configure
// interactive/REPL mode" requirement) since the two subcommands'
// originator differs and interactive is what production runs. The token
// counts are the mock server's configured usage block, not a real model's
// output. The fixture is also a *subset* of what the capture produced: a
// response.created frame and a response.output_item.done frame were
// emitted too but are omitted here, since the rule ignores every
// event.kind other than the two included ones. Every other attribute key,
// value type (stringValue vs intValue), the scope name, and (for the
// failure record) the error.message text are exactly what the capture
// produced.
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

// TestCodexUsageRuleExcludesPerFrameMarker pins: sse_event() emits a
// record with the same event.name/event.kind as sse_event_completed for
// every SSE frame, including a plain "response.completed" frame with no
// usage attached yet. Matching it as a second, zero-token call would
// double-count one model response as two calls. The discriminator is
// duration_ms, which only the per-frame emitter ever sets.
func TestCodexUsageRuleExcludesPerFrameMarker(t *testing.T) {
	record := codexFixtureRecordByEventNameAndKind(t, codexFixtureSseEventCallsite, codexUsageEventKind)
	if _, matched, err := (codexUsageRule{}).MatchLog("", mustEventName(t, record, ""), record); matched || err != nil {
		t.Errorf("per-frame response.completed marker: matched=%v err=%v, want matched=false err=nil", matched, err)
	}
}

// TestCodexUsageRuleMapsFailedResponseToError pins:
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

// TestCodexUsageRuleScopeIsNotReliedOn pins: the rule does not gate on
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
	t.Setenv("SCION_AGENT_ID", "agent-codex-pipeline-1")
	t.Setenv("SCION_AGENT_SLUG", "codex-agent-slug")
	t.Setenv("SCION_PROJECT_ID", "project-codex-pipeline-1")
	t.Setenv("SCION_HARNESS", "codex")
	t.Setenv("SCION_MODEL", "")
	t.Setenv("SCION_BROKER_ID", "")
	t.Setenv("SCION_BROKER_NAME", "")
	t.Setenv("SCION_GCP_PROJECT_ID", "")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	capture := &monitoringCapture{}
	server := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sdkExporter, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sdkExporter.Shutdown(context.Background()) }()

	cfg := &Config{
		Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t), HTTPPort: 0,
		// No real event name is included, so every raw codex log record is
		// dropped by the filter and handleLogs returns before attempting a
		// (here unconfigured) raw-log export. The derived usage metrics,
		// which run before the filter (design §3.3, AC-1.4), must still
		// appear -- this is also the pre-filter-derivation guarantee the
		// callsite-EventName exemption must not break.
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
	defer func() { _ = receiver.Stop(context.Background()) }()

	deriver, err := NewUsageDeriver(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(deriver.rules) == 0 {
		t.Fatal("expected the codex usage rule to be active")
	}
	p.usageDeriver.Store(deriver)
	defer func() { _ = deriver.Shutdown(context.Background()) }()

	// Without the callsite-EventName exemption, this returns the
	// InvalidArgument "conflicting event name representations"
	// policy-rejection error, and nothing below ever runs.
	if err := p.handleLogs(context.Background(), loadCodexUsageFixture(t)); err != nil {
		t.Fatalf("handleLogs rejected a realistic codex fixture: %v", err)
	}
	now = now.Add(10 * time.Millisecond)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("metric flush not confirmed")
	}

	diag := p.UsageDiagnostics()
	// The delta frame and the per-frame response.completed marker must not
	// add a second call for the one successful response -- Derived counts
	// one increment per matched record, so this is 2 (one success, one
	// error), not 3 or 4.
	if diag.Derived != 2 || diag.Malformed != 0 {
		t.Fatalf("diagnostics = %+v, want Derived=2 Malformed=0 (no per-frame double count)", diag)
	}

	series := allCapturedSeries(capture)
	var calls, tokens []*monitoringpb.TimeSeries
	for _, ts := range series {
		switch ts.Metric.Type {
		case "workload.googleapis.com/gen_ai.api.calls":
			calls = append(calls, ts)
		case "workload.googleapis.com/scion.usage.tokens":
			tokens = append(tokens, ts)
		}
	}

	// Exactly one success call and one error call, not two successes
	// (which is what a per-frame double count plus a mis-mapped error
	// status would produce together).
	if len(calls) != 2 {
		t.Fatalf("gen_ai.api.calls series = %d, want 2 (success, error)", len(calls))
	}
	byStatus := map[string]int64{}
	for _, ts := range calls {
		assertLabelKeys(t, ts.Metric.Labels, "agent_id", "project_id", "harness", "model", "status",
			"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
			"scion_agent_id", "scion_project_id", "scion_agent_slug",
			"service_name", "service_instance_id")
		byStatus[ts.Metric.Labels["status"]] = ts.Points[0].Value.GetInt64Value()
	}
	if byStatus["success"] != 1 || byStatus["error"] != 1 {
		t.Fatalf("calls by status = %+v, want success=1 error=1", byStatus)
	}

	// Assert the exported scion.usage.tokens point label keys are exactly
	// {harness, model, token_type} plus the exporter-stamped canonical
	// identity labels -- nothing else (design §3.2).
	wantTokens := map[string]int64{
		telemetrycontract.TokenTypeInput:     3000,
		telemetrycontract.TokenTypeOutput:    842,
		telemetrycontract.TokenTypeCacheRead: 12000,
		telemetrycontract.TokenTypeReasoning: 512,
	}
	if len(tokens) != len(wantTokens) {
		t.Fatalf("scion.usage.tokens series = %d, want %d", len(tokens), len(wantTokens))
	}
	byType := map[string]int64{}
	for _, ts := range tokens {
		assertLabelKeys(t, ts.Metric.Labels, "harness", "model", "token_type",
			"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
			"scion_agent_id", "scion_project_id", "scion_agent_slug",
			"service_name", "service_instance_id")
		if ts.Metric.Labels["harness"] != "codex" || ts.Metric.Labels["model"] != "gpt-5.1-codex" {
			t.Errorf("unexpected tokens series labels: %+v", ts.Metric.Labels)
		}
		if ts.Metric.Labels["scion_agent_id"] != "agent-codex-pipeline-1" || ts.Metric.Labels["scion_project_id"] != "project-codex-pipeline-1" || ts.Metric.Labels["scion_agent_slug"] != "codex-agent-slug" {
			t.Errorf("unexpected canonical identity labels: %+v", ts.Metric.Labels)
		}
		byType[ts.Metric.Labels["token_type"]] = ts.Points[0].Value.GetInt64Value()
	}
	for tokenType, want := range wantTokens {
		if byType[tokenType] != want {
			t.Errorf("tokens[%q] = %d, want %d", tokenType, byType[tokenType], want)
		}
	}
}
