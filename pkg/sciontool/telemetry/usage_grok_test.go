/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"os"
)

const grokBuildUsageFixturePath = "testdata/usage/grok-build-1.0.92.pb.json"

// loadGrokBuildUsageFixture loads a real grok 1.0.92 (b06756fd6256) capture
// exporting native OTel over OTLP/gRPC to a local receiver using the exact env
// vars harnesses/grok-build/provision.py writes (GROK_EXTERNAL_OTEL=true,
// OTEL_EXPORTER_OTLP_PROTOCOL=grpc, OTEL_LOGS_EXPORTER=otlp,
// OTEL_METRICS_EXPORTER=otlp), pointed at a local mock chat_completions server
// with no real credentials.
//
// The fixture contains two ResourceLogs batches:
//   - Batch 0: a single agent turn that hit an internal HTTP 500 retry and then
//     executed two model requests (a tool call and a final response):
//     1) grok_code.api_request (stop_reason=toolcalls): input_tokens=800,
//     cache_read_tokens=200, cache_creation_tokens=0, output_tokens=120,
//     reasoning_tokens=40
//     2) grok_code.api_request (stop_reason=stop): input_tokens=1500,
//     cache_read_tokens=900, cache_creation_tokens=0, output_tokens=300,
//     reasoning_tokens=80
//     plus sibling user_prompt, turn_completed (outcome=completed), and
//     assistant_response records that must not be matched.
//   - Batch 1: a turn that failed with HTTP 400 Bad Request, emitting
//     grok_code.turn_completed (outcome=error, must not be matched) and
//     grok_code.api_error (error_category=invalid_request, model=grok-4.6).
//
// Scrubbed: service.instance.id, session.id, and prompt.id are replaced with
// deterministic placeholder UUIDs. All other attributes, scope ("ai.xai.grok_code"),
// eventName values, types, and timestamps are as captured.
func loadGrokBuildUsageFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	data, err := os.ReadFile(grokBuildUsageFixturePath)
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

func grokFixtureRecords(t *testing.T, eventName string) ([]*logspb.LogRecord, []string) {
	t.Helper()
	var records []*logspb.LogRecord
	var scopes []string
	for _, rl := range loadGrokBuildUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if record.GetEventName() == eventName {
					records = append(records, record)
					scopes = append(scopes, sl.GetScope().GetName())
				}
			}
		}
	}
	return records, scopes
}

func TestGrokBuildUsageRuleMatchesFixtureAPIRequests(t *testing.T) {
	records, scopes := grokFixtureRecords(t, grokBuildAPIRequestEvent)
	if len(records) != 2 {
		t.Fatalf("fixture %s records = %d, want 2", grokBuildAPIRequestEvent, len(records))
	}

	wantTokens := []map[string]int64{
		{
			// 800 inclusive input - 200 cache_read - 0 cache_creation = 600
			telemetrycontract.TokenTypeInput:     600,
			telemetrycontract.TokenTypeOutput:    120,
			telemetrycontract.TokenTypeCacheRead: 200,
			telemetrycontract.TokenTypeReasoning: 40,
		},
		{
			// 1500 inclusive input - 900 cache_read - 0 cache_creation = 600
			telemetrycontract.TokenTypeInput:     600,
			telemetrycontract.TokenTypeOutput:    300,
			telemetrycontract.TokenTypeCacheRead: 900,
			telemetrycontract.TokenTypeReasoning: 80,
		},
	}

	for i, record := range records {
		if scopes[i] != "ai.xai.grok_code" {
			t.Fatalf("record[%d] scope = %q, want ai.xai.grok_code", i, scopes[i])
		}
		inc, matched, err := grokBuildUsageRule{}.MatchLog(scopes[i], mustEventName(t, record, scopes[i]), record)
		if !matched || err != nil {
			t.Fatalf("record[%d]: matched=%v err=%v, want matched=true err=nil", i, matched, err)
		}
		if inc.Calls != 1 || inc.Status != telemetrycontract.StatusSuccess || inc.Model != "grok-4.6" {
			t.Fatalf("record[%d]: increment = %+v", i, inc)
		}
		assertTokensByType(t, inc.Tokens, wantTokens[i])
	}
}

func TestGrokBuildUsageRuleMapsFixtureAPIErrorToError(t *testing.T) {
	records, scopes := grokFixtureRecords(t, grokBuildAPIErrorEvent)
	if len(records) != 1 {
		t.Fatalf("fixture %s records = %d, want 1", grokBuildAPIErrorEvent, len(records))
	}
	inc, matched, err := grokBuildUsageRule{}.MatchLog(scopes[0], mustEventName(t, records[0], scopes[0]), records[0])
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	if inc.Calls != 1 || inc.Status != telemetrycontract.StatusError || len(inc.Tokens) != 0 || inc.Model != "grok-4.6" {
		t.Fatalf("increment = %+v, want one tokenless error call", inc)
	}
}

func TestGrokBuildUsageRuleIgnoresSiblingAndSyntheticEvents(t *testing.T) {
	for _, eventName := range []string{"grok_code.user_prompt", "grok_code.turn_completed", "grok_code.assistant_response"} {
		records, scopes := grokFixtureRecords(t, eventName)
		if len(records) == 0 {
			t.Fatalf("fixture has no %s record", eventName)
		}
		for i, record := range records {
			if _, matched, err := (grokBuildUsageRule{}).MatchLog(scopes[i], mustEventName(t, record, scopes[i]), record); matched || err != nil {
				t.Errorf("%s[%d]: matched=%v err=%v, want matched=false err=nil", eventName, i, matched, err)
			}
		}
	}

	// Synthetic / internal CLI errors must never be counted as provider API errors.
	internalErr := &logspb.LogRecord{
		EventName: "grok_code.internal_error",
		Attributes: []*commonpb.KeyValue{
			{Key: "error_type", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "io_error"}}},
		},
	}
	if _, matched, err := (grokBuildUsageRule{}).MatchLog("ai.xai.grok_code", mustEventName(t, internalErr, "ai.xai.grok_code"), internalErr); matched || err != nil {
		t.Fatalf("grok_code.internal_error: matched=%v err=%v, want matched=false err=nil", matched, err)
	}
}

func grokAPIRequestRecord(extra ...*commonpb.KeyValue) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "grok-4.6"}}},
	}
	return &logspb.LogRecord{
		EventName:  grokBuildAPIRequestEvent,
		Attributes: append(attrs, extra...),
	}
}

func grokIntAttr(key string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func TestGrokBuildUsageRuleCacheCreationSubtraction(t *testing.T) {
	record := grokAPIRequestRecord(
		grokIntAttr("input_tokens", 1000),
		grokIntAttr("output_tokens", 200),
		grokIntAttr("cache_read_tokens", 300),
		grokIntAttr("cache_creation_tokens", 150),
		grokIntAttr("reasoning_tokens", 50),
	)
	inc, matched, err := grokBuildUsageRule{}.MatchLog("ai.xai.grok_code", mustEventName(t, record, "ai.xai.grok_code"), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	assertTokensByType(t, inc.Tokens, map[string]int64{
		telemetrycontract.TokenTypeInput:      550,
		telemetrycontract.TokenTypeOutput:     200,
		telemetrycontract.TokenTypeCacheRead:  300,
		telemetrycontract.TokenTypeCacheWrite: 150,
		telemetrycontract.TokenTypeReasoning:  50,
	})
}

func TestGrokBuildUsageRuleMalformedCases(t *testing.T) {
	cases := []struct {
		name  string
		attrs []*commonpb.KeyValue
	}{
		{"cached exceeds input", []*commonpb.KeyValue{grokIntAttr("input_tokens", 10), grokIntAttr("cache_read_tokens", 11)}},
		{"cached plus cache_creation exceeds input", []*commonpb.KeyValue{grokIntAttr("input_tokens", 10), grokIntAttr("cache_read_tokens", 6), grokIntAttr("cache_creation_tokens", 5)}},
		{"negative output", []*commonpb.KeyValue{grokIntAttr("output_tokens", -1)}},
		{"non-numeric reasoning", []*commonpb.KeyValue{{Key: "reasoning_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "lots"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := grokAPIRequestRecord(tc.attrs...)
			inc, matched, err := grokBuildUsageRule{}.MatchLog("ai.xai.grok_code", mustEventName(t, record, "ai.xai.grok_code"), record)
			if !matched || err == nil {
				t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
			}
			if inc.Calls != 1 || inc.Status != telemetrycontract.StatusSuccess || len(inc.Tokens) != 0 {
				t.Fatalf("increment = %+v, want a counted success call with no tokens", inc)
			}
		})
	}
}

func TestGrokBuildUsageRuleZeroUsage(t *testing.T) {
	record := grokAPIRequestRecord(grokIntAttr("input_tokens", 0), grokIntAttr("output_tokens", 0))
	inc, matched, err := grokBuildUsageRule{}.MatchLog("ai.xai.grok_code", mustEventName(t, record, "ai.xai.grok_code"), record)
	if !matched || err != nil || inc.Calls != 1 || len(inc.Tokens) != 0 {
		t.Fatalf("matched=%v err=%v increment=%+v, want one call and no tokens", matched, err, inc)
	}
}

func TestNewUsageDeriverBuildsGrokBuildRule(t *testing.T) {
	t.Setenv("SCION_HARNESS", "grok-build")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownTestDeriver(d) })
	if len(d.rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(d.rules))
	}
	if _, ok := d.rules[0].(grokBuildUsageRule); !ok {
		t.Fatalf("rule = %T, want grokBuildUsageRule", d.rules[0])
	}
}

func TestNewUsageDeriverGrokBuildStaysOffWithoutNativeSource(t *testing.T) {
	t.Setenv("SCION_HARNESS", "grok-build")
	t.Setenv("SCION_USAGE_SOURCE", "")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("grok-build deriver must be a no-op unless SCION_USAGE_SOURCE=native")
	}
}

func TestPipelineDerivesGrokBuildUsageEndToEnd(t *testing.T) {
	result := deriveUsageThroughPipeline(t, "grok-build", loadGrokBuildUsageFixture(t))
	if result.diag.Derived != 3 || result.diag.Malformed != 0 || result.diag.Duplicate != 0 {
		t.Fatalf("diagnostics = %+v, want Derived=3 Malformed=0 Duplicate=0", result.diag)
	}
	if len(result.callsByStatus) != 2 || result.callsByStatus["success"] != 2 || result.callsByStatus["error"] != 1 {
		t.Fatalf("calls by status = %+v, want success=2 error=1", result.callsByStatus)
	}
	assertTokensByType(t, result.tokensByType, map[string]int64{
		telemetrycontract.TokenTypeInput:     1200,
		telemetrycontract.TokenTypeOutput:    420,
		telemetrycontract.TokenTypeCacheRead: 1100,
		telemetrycontract.TokenTypeReasoning: 120,
	})
	for _, labels := range result.tokenLabels {
		if labels["harness"] != "grok-build" || labels["model"] != "grok-4.6" {
			t.Errorf("unexpected tokens series labels: %+v", labels)
		}
	}
}

func TestUsageAnalyticsGrokBuildFixture(t *testing.T) {
	setAnalyticsEnv(t, "grok-build")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), grokBuildUsageRule{})
	fixture := loadGrokBuildUsageFixture(t)
	feed(d, fixture)

	evs := stagedEvents(t, o)
	if len(evs) != 3 {
		t.Fatalf("staged events = %d, want 3 (2 api_request + 1 api_error)", len(evs))
	}

	// Call 1 (toolcalls): input=800-200-0=600, output=120, cache_read=200, cache_write=0, reasoning=40.
	if evs[0].UsageSource != telemetrycontract.UsageSourceNative ||
		evs[0].SourceKind != "grok-build.api_request" ||
		evs[0].Harness != "grok-build" ||
		evs[0].HarnessVersion != "1.0.92 (b06756fd6256)" ||
		evs[0].Model != "grok-4.6" ||
		evs[0].SessionID != "00000002-0000-4000-8000-000000000002" ||
		evs[0].Status != telemetrycontract.StatusSuccess ||
		evs[0].Calls != 1 {
		t.Fatalf("evs[0] metadata = %+v", evs[0])
	}
	assertTokens(t, "grok call 1", evs[0], tokenView{
		Input: i64(600), Output: i64(120), CacheRead: i64(200), CacheWrite: i64(0), Reasoning: i64(40),
		Overlaps: true, Derivation: telemetrycontract.InputDerivationDerived, Status: telemetrycontract.TokenStatusReported,
	})

	// Call 2 (stop): input=1500-900-0=600, output=300, cache_read=900, cache_write=0, reasoning=80.
	if evs[1].SourceKind != "grok-build.api_request" ||
		evs[1].SessionID != "00000002-0000-4000-8000-000000000002" ||
		evs[1].Calls != 1 {
		t.Fatalf("evs[1] metadata = %+v", evs[1])
	}
	assertTokens(t, "grok call 2", evs[1], tokenView{
		Input: i64(600), Output: i64(300), CacheRead: i64(900), CacheWrite: i64(0), Reasoning: i64(80),
		Overlaps: true, Derivation: telemetrycontract.InputDerivationDerived, Status: telemetrycontract.TokenStatusReported,
	})

	// Call 3 (api_error): tokenless error call with batch 1's session.id.
	if evs[2].SourceKind != "grok-build.api_error" ||
		evs[2].SessionID != "00000005-0000-4000-8000-000000000005" ||
		evs[2].Status != telemetrycontract.StatusError ||
		evs[2].Calls != 1 ||
		evs[2].TokenStatus != telemetrycontract.TokenStatusNotReported {
		t.Fatalf("evs[2] = %+v", evs[2])
	}

	// Replaying the exact same fixture dedupes at the counter layer.
	feed(d, fixture)
	if diag := d.Diagnostics(); diag.Derived != 3 || diag.Duplicate != 3 {
		t.Fatalf("after replay diagnostics = %+v, want Derived=3 Duplicate=3", diag)
	}
}

func TestUsageAnalyticsGrokBuildOptionalCostAndPartialMalformed(t *testing.T) {
	t.Run("cost_usd_micros forwarded", func(t *testing.T) {
		rec := grokAPIRequestRecord(
			grokIntAttr("input_tokens", 100),
			grokIntAttr("output_tokens", 20),
			grokIntAttr("cache_read_tokens", 40),
			grokIntAttr("cache_creation_tokens", 10),
			grokIntAttr("reasoning_tokens", 5),
			grokIntAttr("cost_usd_micros", 1234),
		)
		obs := grokBuildObservation(grokBuildAPIRequestEvent, rec.Attributes)
		if obs.ReportedCostMicros == nil || *obs.ReportedCostMicros != 1234 {
			t.Fatalf("ReportedCostMicros = %v, want 1234", obs.ReportedCostMicros)
		}
		if obs.Input == nil || *obs.Input != 50 || obs.InputDerivation != telemetrycontract.InputDerivationDerived {
			t.Fatalf("Input = %v deriv=%q, want 50 derived", obs.Input, obs.InputDerivation)
		}
	})

	t.Run("omitted cache_creation leaves input unknown and status partial", func(t *testing.T) {
		rec := grokAPIRequestRecord(
			grokIntAttr("input_tokens", 100),
			grokIntAttr("output_tokens", 20),
			grokIntAttr("cache_read_tokens", 40),
			grokIntAttr("reasoning_tokens", 5),
		)
		obs := grokBuildObservation(grokBuildAPIRequestEvent, rec.Attributes)
		if obs.Input != nil || obs.InputDerivation != "" || obs.TokenStatus != telemetrycontract.TokenStatusPartial {
			t.Fatalf("obs = %+v, want nil Input and partial status", obs)
		}
	})

	t.Run("cache sum exceeding input marks all three malformed", func(t *testing.T) {
		rec := grokAPIRequestRecord(
			grokIntAttr("input_tokens", 100),
			grokIntAttr("output_tokens", 20),
			grokIntAttr("cache_read_tokens", 80),
			grokIntAttr("cache_creation_tokens", 30),
			grokIntAttr("reasoning_tokens", 5),
		)
		obs := grokBuildObservation(grokBuildAPIRequestEvent, rec.Attributes)
		if obs.Input != nil || obs.TokenStatus != telemetrycontract.TokenStatusMalformed {
			t.Fatalf("obs = %+v, want nil Input and malformed status", obs)
		}
		if len(obs.MalformedFields) != 3 {
			t.Fatalf("MalformedFields = %v, want 3 fields", obs.MalformedFields)
		}
	})
}

func TestAPIOutcomeGrokBuildAPIError(t *testing.T) {
	records, _ := grokFixtureRecords(t, grokBuildAPIErrorEvent)
	out := apiOutcomeFromRecord(records[0], usageIncrement{Model: "grok-4.6", Status: telemetrycontract.StatusError, Calls: 1}, time.Now())
	if out.OK || out.Model != "grok-4.6" || out.Message != "invalid_request" {
		t.Fatalf("outcome = %+v, want OK=false Model=grok-4.6 Message=invalid_request", out)
	}
}
