/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

const grokBuildUsageFixturePath = "testdata/usage/grok-build-1.0.92.pb.json"

// loadGrokBuildUsageFixture loads a real grok 1.0.92 (b06756fd6256) capture
// talking to real Vertex AI Model Garden (xai/grok-4.6, chat_completions
// backend) and exporting native OTel over OTLP/gRPC to a local receiver using
// the env vars harnesses/grok-build/provision.py writes
// (GROK_EXTERNAL_OTEL=true, OTEL_EXPORTER_OTLP_PROTOCOL=grpc,
// OTEL_LOGS_EXPORTER=otlp, OTEL_METRICS_EXPORTER=otlp).
//
// The fixture contains two ResourceLogs batches:
//   - Batch 0: a single agent turn executing a read_file tool call and then
//     answering a multiplication question (2 real xai/grok-4.6 requests):
//     1) grok_code.api_request (stop_reason=toolcalls): input_tokens=7378,
//     cache_read_tokens=0, cache_creation_tokens=0, output_tokens=29,
//     reasoning_tokens=30 (raw provider total_tokens=7437 = 7378+29+30)
//     2) grok_code.api_request (stop_reason=stop): input_tokens=7454,
//     cache_read_tokens=7296, cache_creation_tokens=0, output_tokens=6,
//     reasoning_tokens=26 (raw provider total_tokens=7486 = 7454+6+26)
//     plus sibling user_prompt, turn_completed (outcome=completed,
//     tool_call_count=1), and assistant_response records that must not match.
//   - Batch 1: a turn against an invalid Vertex model (HTTP 404 Not Found),
//     emitting grok_code.turn_completed (outcome=error, must not match) and
//     grok_code.api_error (error_category=invalid_request,
//     model=xai/grok-nonexistent-test).
//
// Scrubbed: service.instance.id, session.id, and prompt.id are replaced with
// deterministic placeholder UUIDs. All other attributes, scope
// ("ai.xai.grok_code"), eventName values, types, and timestamps are as
// captured.
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
			// input = 7378 - 0 - 0; output = 29 + 30 = 59 (canonical output
			// includes reasoning); input + cache_read + output = 7437 = total_tokens.
			telemetrycontract.TokenTypeInput:     7378,
			telemetrycontract.TokenTypeOutput:    59,
			telemetrycontract.TokenTypeReasoning: 30,
		},
		{
			// input = 7454 - 7296 - 0 = 158; output = 6 + 26 = 32;
			// input + cache_read + output = 158 + 7296 + 32 = 7486 = total_tokens.
			telemetrycontract.TokenTypeInput:     158,
			telemetrycontract.TokenTypeOutput:    32,
			telemetrycontract.TokenTypeCacheRead: 7296,
			telemetrycontract.TokenTypeReasoning: 26,
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
		if inc.Calls != 1 || inc.Status != telemetrycontract.StatusSuccess || inc.Model != "xai/grok-4.6" {
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
	if inc.Calls != 1 || inc.Status != telemetrycontract.StatusError || len(inc.Tokens) != 0 || inc.Model != "xai/grok-nonexistent-test" {
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
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "xai/grok-4.6"}}},
	}
	return &logspb.LogRecord{
		EventName:  grokBuildAPIRequestEvent,
		Attributes: append(attrs, extra...),
	}
}

func grokIntAttr(key string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func TestGrokBuildUsageRuleOutputIncludesReasoningAndCacheCreationSubtraction(t *testing.T) {
	// Pins both output = output_tokens + reasoning_tokens (e.g. 9 + 110 = 119,
	// matching real xAI/Vertex exclusive completion_tokens) and cache_creation
	// subtraction from inclusive input_tokens.
	record := grokAPIRequestRecord(
		grokIntAttr("input_tokens", 1000),
		grokIntAttr("output_tokens", 9),
		grokIntAttr("cache_read_tokens", 300),
		grokIntAttr("cache_creation_tokens", 150),
		grokIntAttr("reasoning_tokens", 110),
	)
	inc, matched, err := grokBuildUsageRule{}.MatchLog("ai.xai.grok_code", mustEventName(t, record, "ai.xai.grok_code"), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	assertTokensByType(t, inc.Tokens, map[string]int64{
		telemetrycontract.TokenTypeInput:      550,
		telemetrycontract.TokenTypeOutput:     119,
		telemetrycontract.TokenTypeCacheRead:  300,
		telemetrycontract.TokenTypeCacheWrite: 150,
		telemetrycontract.TokenTypeReasoning:  110,
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
		{"output plus reasoning overflow", []*commonpb.KeyValue{grokIntAttr("output_tokens", 1<<62), grokIntAttr("reasoning_tokens", 1<<62)}},
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
		telemetrycontract.TokenTypeInput:     7536,
		telemetrycontract.TokenTypeOutput:    91,
		telemetrycontract.TokenTypeCacheRead: 7296,
		telemetrycontract.TokenTypeReasoning: 56,
	})
	for _, labels := range result.tokenLabels {
		if labels["harness"] != "grok-build" || labels["model"] != "xai/grok-4.6" {
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

	// Call 1 (toolcalls): input=7378-0-0=7378, output=29+30=59, cache_read=0, cache_write=0, reasoning=30.
	if evs[0].UsageSource != telemetrycontract.UsageSourceNative ||
		evs[0].SourceKind != "grok-build.api_request" ||
		evs[0].Harness != "grok-build" ||
		evs[0].HarnessVersion != "1.0.92 (b06756fd6256)" ||
		evs[0].Model != "xai/grok-4.6" ||
		evs[0].SessionID != "00000002-0000-4000-8000-000000000002" ||
		evs[0].Status != telemetrycontract.StatusSuccess ||
		evs[0].Calls != 1 {
		t.Fatalf("evs[0] metadata = %+v", evs[0])
	}
	assertTokens(t, "grok call 1", evs[0], tokenView{
		Input: i64(7378), Output: i64(59), CacheRead: i64(0), CacheWrite: i64(0), Reasoning: i64(30),
		Overlaps: true, Derivation: telemetrycontract.InputDerivationDerived, Status: telemetrycontract.TokenStatusReported,
	})

	// Call 2 (stop): input=7454-7296-0=158, output=6+26=32, cache_read=7296, cache_write=0, reasoning=26.
	if evs[1].SourceKind != "grok-build.api_request" ||
		evs[1].SessionID != "00000002-0000-4000-8000-000000000002" ||
		evs[1].Calls != 1 {
		t.Fatalf("evs[1] metadata = %+v", evs[1])
	}
	assertTokens(t, "grok call 2", evs[1], tokenView{
		Input: i64(158), Output: i64(32), CacheRead: i64(7296), CacheWrite: i64(0), Reasoning: i64(26),
		Overlaps: true, Derivation: telemetrycontract.InputDerivationDerived, Status: telemetrycontract.TokenStatusReported,
	})

	// Call 3 (api_error): tokenless error call with batch 1's session.id.
	if evs[2].SourceKind != "grok-build.api_error" ||
		evs[2].SessionID != "00000005-0000-4000-8000-000000000005" ||
		evs[2].Model != "xai/grok-nonexistent-test" ||
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

func TestUsageAnalyticsGrokBuildPartialAndMalformed(t *testing.T) {
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
		if obs.Output == nil || *obs.Output != 25 {
			t.Fatalf("Output = %v, want 25 (20+5)", obs.Output)
		}
	})

	t.Run("omitted reasoning leaves output unknown and status partial", func(t *testing.T) {
		rec := grokAPIRequestRecord(
			grokIntAttr("input_tokens", 100),
			grokIntAttr("output_tokens", 20),
			grokIntAttr("cache_read_tokens", 40),
			grokIntAttr("cache_creation_tokens", 0),
		)
		obs := grokBuildObservation(grokBuildAPIRequestEvent, rec.Attributes)
		if obs.Output != nil || obs.TokenStatus != telemetrycontract.TokenStatusPartial {
			t.Fatalf("obs = %+v, want nil Output and partial status", obs)
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

	t.Run("output plus reasoning overflow marks both malformed", func(t *testing.T) {
		rec := grokAPIRequestRecord(
			grokIntAttr("input_tokens", 100),
			grokIntAttr("output_tokens", 1<<62),
			grokIntAttr("cache_read_tokens", 0),
			grokIntAttr("cache_creation_tokens", 0),
			grokIntAttr("reasoning_tokens", 1<<62),
		)
		obs := grokBuildObservation(grokBuildAPIRequestEvent, rec.Attributes)
		if obs.Output != nil || obs.TokenStatus != telemetrycontract.TokenStatusMalformed {
			t.Fatalf("obs = %+v, want nil Output and malformed status", obs)
		}
		if len(obs.MalformedFields) != 2 {
			t.Fatalf("MalformedFields = %v, want 2 fields", obs.MalformedFields)
		}
	})
}

func TestAPIOutcomeGrokBuildAPIError(t *testing.T) {
	records, _ := grokFixtureRecords(t, grokBuildAPIErrorEvent)
	out := apiOutcomeFromRecord(records[0], usageIncrement{Model: "xai/grok-nonexistent-test", Status: telemetrycontract.StatusError, Calls: 1}, time.Now())
	if out.OK || out.Model != "xai/grok-nonexistent-test" || out.Message != "invalid_request" {
		t.Fatalf("outcome = %+v, want OK=false Model=xai/grok-nonexistent-test Message=invalid_request", out)
	}
}

func TestGrokBuildVerifiedVersionsMatchesFixture(t *testing.T) {
	if len(GrokBuildVerifiedVersions) == 0 {
		t.Fatal("GrokBuildVerifiedVersions must not be empty")
	}
	matched := false
	for _, v := range GrokBuildVerifiedVersions {
		if strings.Contains(grokBuildUsageFixturePath, "grok-build-"+v+".pb.json") {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("GrokBuildVerifiedVersions %v does not include fixture %q", GrokBuildVerifiedVersions, grokBuildUsageFixturePath)
	}
}

type capturedSlogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type captureSlogHandler struct {
	mu      sync.Mutex
	records []capturedSlogRecord
}

func (h *captureSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureSlogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedSlogRecord{level: r.Level, msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}
func (h *captureSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureSlogHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureSlogHandler) warnings() []capturedSlogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedSlogRecord
	for _, r := range h.records {
		if r.level == slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

func TestMatchGrokBuildVerifiedVersion(t *testing.T) {
	cases := []struct {
		raw     string
		wantVer string
		wantOK  bool
	}{
		{"1.0.92", "1.0.92", true},
		{"1.0.92 (b06756fd6256)", "1.0.92", true},
		{"v1.0.92\n", "1.0.92", true},
		{"1.1.0 (abc1234)", "1.1.0", false},
		{"", "", false},
		{"dev", "", false},
		{"unknown", "", false},
	}
	for _, tc := range cases {
		ver, ok := matchGrokBuildVerifiedVersion(tc.raw)
		if ver != tc.wantVer || ok != tc.wantOK {
			t.Errorf("matchGrokBuildVerifiedVersion(%q) = (%q, %v), want (%q, %v)", tc.raw, ver, ok, tc.wantVer, tc.wantOK)
		}
	}
}

func TestGrokBuildUnverifiedVersionWarning(t *testing.T) {
	makeRecord := func(promptID string) *logspb.LogRecord {
		return grokAPIRequestRecord(
			&commonpb.KeyValue{Key: "prompt.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: promptID}}},
			grokIntAttr("input_tokens", 100),
			grokIntAttr("output_tokens", 20),
			grokIntAttr("cache_read_tokens", 0),
			grokIntAttr("cache_creation_tokens", 0),
			grokIntAttr("reasoning_tokens", 0),
		)
	}
	resAttrs := func(ver string) []*commonpb.KeyValue {
		if ver == "" {
			return nil
		}
		return []*commonpb.KeyValue{
			{Key: "service.version", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: ver}}},
		}
	}

	t.Run("matching version emits no warning", func(t *testing.T) {
		cap := &captureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		d := &UsageDeriver{
			rules: []usageRule{grokBuildUsageRule{}},
			seen:  newBoundedLRU(usageDedupeCapacity, usageDedupeTTL),
		}
		if !d.observeRecord(context.Background(), "ai.xai.grok_code", resAttrs("1.0.92 (b06756fd6256)"), makeRecord("p1")) {
			t.Fatal("observeRecord returned false, want true")
		}
		if got := cap.warnings(); len(got) != 0 {
			t.Fatalf("warnings = %+v, want 0", got)
		}
	})

	t.Run("differing version emits exactly one warning across multiple events", func(t *testing.T) {
		cap := &captureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		d := &UsageDeriver{
			rules: []usageRule{grokBuildUsageRule{}},
			seen:  newBoundedLRU(usageDedupeCapacity, usageDedupeTTL),
		}
		if !d.observeRecord(context.Background(), "ai.xai.grok_code", resAttrs("1.1.0 (abc1234)"), makeRecord("p1")) {
			t.Fatal("first observeRecord returned false, want true")
		}
		if !d.observeRecord(context.Background(), "ai.xai.grok_code", resAttrs("1.1.0 (abc1234)"), makeRecord("p2")) {
			t.Fatal("second observeRecord returned false, want true")
		}
		warns := cap.warnings()
		if len(warns) != 1 {
			t.Fatalf("warnings count = %d, want 1 (%+v)", len(warns), warns)
		}
		w := warns[0]
		if w.attrs["harness"] != "grok-build" || w.attrs["installed_version"] != "1.1.0" || w.attrs["verified_versions"] != strings.Join(GrokBuildVerifiedVersions, ",") {
			t.Errorf("warning attrs = %+v, want harness=grok-build installed_version=1.1.0 verified_versions=%s", w.attrs, strings.Join(GrokBuildVerifiedVersions, ","))
		}
		if !strings.Contains(w.msg, "usage reporting") || !strings.Contains(w.msg, "harness-cli-version-bump.md") {
			t.Errorf("warning msg = %q, want usage reporting degradation and bump doc reference", w.msg)
		}
	})

	t.Run("missing or unparseable version emits no warning and does not suppress later mismatch", func(t *testing.T) {
		cap := &captureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		d := &UsageDeriver{
			rules: []usageRule{grokBuildUsageRule{}},
			seen:  newBoundedLRU(usageDedupeCapacity, usageDedupeTTL),
		}
		if !d.observeRecord(context.Background(), "ai.xai.grok_code", nil, makeRecord("p1")) {
			t.Fatal("missing-version observeRecord returned false, want true")
		}
		if !d.observeRecord(context.Background(), "ai.xai.grok_code", resAttrs("dev"), makeRecord("p2")) {
			t.Fatal("unparseable-version observeRecord returned false, want true")
		}
		if got := cap.warnings(); len(got) != 0 {
			t.Fatalf("warnings for missing/unparseable = %+v, want 0", got)
		}
		// A later event carrying a real unverified version still warns once.
		if !d.observeRecord(context.Background(), "ai.xai.grok_code", resAttrs("1.2.0"), makeRecord("p3")) {
			t.Fatal("mismatch observeRecord returned false, want true")
		}
		if got := cap.warnings(); len(got) != 1 || got[0].attrs["installed_version"] != "1.2.0" {
			t.Fatalf("warnings after later mismatch = %+v, want 1 warning for 1.2.0", got)
		}
	})
}
