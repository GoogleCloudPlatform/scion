/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const geminiCLIUsageFixturePath = "testdata/usage/gemini-cli-0.62.0.pb.json"

// loadGeminiCLIUsageFixture loads a gemini-cli 0.62.0 capture: the real,
// npm-installed @google/gemini-cli@0.62.0 run as `gemini -m gemini-2.5-pro
// -p ...` with GEMINI_API_KEY set to a fake value and
// GOOGLE_GEMINI_BASE_URL pointed at a local mock Gemini API server (a tiny
// Python HTTP server; no network calls, no real credentials), exporting
// native OTel over OTLP/gRPC with gzip to a local sink -- the same
// telemetry settings harnesses/gemini-cli's provision.py writes (target
// local, otlpProtocol grpc, logPrompts false).
//
// The mock fails the first streamGenerateContent call with HTTP 503;
// gemini-cli's retryWithBackoff retries it, and the mock answers the second
// attempt with a two-chunk SSE stream whose final chunk carries
// usageMetadata promptTokenCount=1500, cachedContentTokenCount=1200,
// candidatesTokenCount=80, thoughtsTokenCount=20,
// toolUsePromptTokenCount=10, totalTokenCount=1610 (the mock's configured
// values, not a real model's). So the capture holds two ResourceLogs (one
// per export batch): the first with an api_request, its
// gen_ai.client.inference.operation.details sibling, and the api_error for
// the 503; the second with an api_request, an operation.details sibling,
// the api_response, and its operation.details sibling (which carries
// gen_ai.usage.input_tokens/output_tokens and must not be counted).
//
// The capture is a subset: config, startup_stats, user_prompt,
// model_routing and plan.approval_mode_duration records are omitted, and
// the metric/trace exports are not part of this file. interactive=false
// is the real value for a `-p` run (production runs interactive mode; the
// usage events are the same LoggingContentGenerator code path either way).
// Scrubbed: resource host.name ("scrubbed-host"), process.pid,
// process.command and process.command_args (the capture's temp install path
// and prompt), and session.id/installation.id/prompt_id (placeholder
// UUIDs); gen_ai.system_instructions (gemini-cli's full system prompt) is
// replaced with "[scrubbed]". Every other key, value and value type, the
// scope name ("gemini-cli") and every timestamp is as captured.
func loadGeminiCLIUsageFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	data, err := os.ReadFile(geminiCLIUsageFixturePath)
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

// geminiFixtureRecords returns every fixture record with event.name
// eventName, with its scope name, in capture order.
func geminiFixtureRecords(t *testing.T, eventName string) ([]*logspb.LogRecord, []string) {
	t.Helper()
	var records []*logspb.LogRecord
	var scopes []string
	for _, rl := range loadGeminiCLIUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if logAttrString(record.Attributes, "event.name") == eventName {
					records = append(records, record)
					scopes = append(scopes, sl.GetScope().GetName())
				}
			}
		}
	}
	return records, scopes
}

func geminiFixtureRecord(t *testing.T, eventName string) (*logspb.LogRecord, string) {
	t.Helper()
	records, scopes := geminiFixtureRecords(t, eventName)
	if len(records) != 1 {
		t.Fatalf("fixture records with event.name %q = %d, want 1", eventName, len(records))
	}
	return records[0], scopes[0]
}

func TestGeminiCLIUsageRuleMatchesFixtureAPIResponse(t *testing.T) {
	record, scope := geminiFixtureRecord(t, geminiCLIAPIResponseEvent)
	if scope != "gemini-cli" {
		t.Fatalf("captured scope = %q, want gemini-cli", scope)
	}
	increment, matched, err := geminiCLIUsageRule{}.MatchLog(scope, mustEventName(t, record, scope), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess || increment.Model != "gemini-2.5-pro" {
		t.Fatalf("increment = %+v", increment)
	}
	// input = 1500 prompt - 1200 cached + 10 tool-use prompt; output = 80
	// candidates + 20 thoughts (contract output includes reasoning);
	// input + cache_read + output = 1610 = total_token_count.
	assertTokensByType(t, increment.Tokens, map[string]int64{
		telemetrycontract.TokenTypeInput:     310,
		telemetrycontract.TokenTypeOutput:    100,
		telemetrycontract.TokenTypeCacheRead: 1200,
		telemetrycontract.TokenTypeReasoning: 20,
	})
	total, err := logAttrInt(record.Attributes, "total_token_count")
	if err != nil {
		t.Fatal(err)
	}
	if sum := increment.Tokens[telemetrycontract.TokenTypeInput] + increment.Tokens[telemetrycontract.TokenTypeCacheRead] + increment.Tokens[telemetrycontract.TokenTypeOutput]; sum != total {
		t.Errorf("input+cache_read+output = %d, want total_token_count %d", sum, total)
	}
}

func TestGeminiCLIUsageRuleMapsFixtureAPIErrorToError(t *testing.T) {
	record, scope := geminiFixtureRecord(t, geminiCLIAPIErrorEvent)
	increment, matched, err := geminiCLIUsageRule{}.MatchLog(scope, mustEventName(t, record, scope), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusError || len(increment.Tokens) != 0 || increment.Model != "gemini-2.5-pro" {
		t.Fatalf("increment = %+v, want one tokenless error call", increment)
	}
}

// TestGeminiCLIUsageRuleIgnoresSiblingEvents pins that api_request and the
// gen_ai.client.inference.operation.details record gemini-cli emits next to
// every api_request/api_response/api_error never match: the latter carries
// gen_ai.usage.* token counts, so matching it would double-count calls and
// tokens.
func TestGeminiCLIUsageRuleIgnoresSiblingEvents(t *testing.T) {
	for _, eventName := range []string{"gemini_cli.api_request", "gen_ai.client.inference.operation.details"} {
		records, scopes := geminiFixtureRecords(t, eventName)
		if len(records) == 0 {
			t.Fatalf("fixture has no %s record", eventName)
		}
		for i, record := range records {
			if _, matched, err := (geminiCLIUsageRule{}).MatchLog(scopes[i], mustEventName(t, record, scopes[i]), record); matched || err != nil {
				t.Errorf("%s[%d]: matched=%v err=%v, want matched=false err=nil", eventName, i, matched, err)
			}
		}
	}
}

func geminiAPIResponseRecord(extra ...*commonpb.KeyValue) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: geminiCLIAPIResponseEvent}}},
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "gemini-2.5-flash"}}},
	}
	return &logspb.LogRecord{Attributes: append(attrs, extra...)}
}

func geminiIntAttr(key string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func TestGeminiCLIUsageRuleMalformedCases(t *testing.T) {
	cases := []struct {
		name  string
		attrs []*commonpb.KeyValue
	}{
		{"cached exceeds prompt", []*commonpb.KeyValue{geminiIntAttr("input_token_count", 10), geminiIntAttr("cached_content_token_count", 11)}},
		{"negative output", []*commonpb.KeyValue{geminiIntAttr("output_token_count", -1)}},
		{"non-numeric thoughts", []*commonpb.KeyValue{{Key: "thoughts_token_count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "many"}}}}},
		{"output overflow", []*commonpb.KeyValue{geminiIntAttr("output_token_count", 1<<62), geminiIntAttr("thoughts_token_count", 1<<62)}},
		{"input overflow", []*commonpb.KeyValue{geminiIntAttr("input_token_count", 1<<62), geminiIntAttr("tool_token_count", 1<<62)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := geminiAPIResponseRecord(tc.attrs...)
			increment, matched, err := geminiCLIUsageRule{}.MatchLog("gemini-cli", mustEventName(t, record, "gemini-cli"), record)
			if !matched || err == nil {
				t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
			}
			if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess || len(increment.Tokens) != 0 {
				t.Fatalf("increment = %+v, want a counted success call with no tokens", increment)
			}
		})
	}
}

// TestGeminiCLIUsageRuleZeroUsage pins that an api_response with no
// usageMetadata (gemini-cli then reports every count as 0) is still one
// successful call, with no token points.
func TestGeminiCLIUsageRuleZeroUsage(t *testing.T) {
	record := geminiAPIResponseRecord(geminiIntAttr("input_token_count", 0), geminiIntAttr("output_token_count", 0))
	increment, matched, err := geminiCLIUsageRule{}.MatchLog("gemini-cli", mustEventName(t, record, "gemini-cli"), record)
	if !matched || err != nil || increment.Calls != 1 || len(increment.Tokens) != 0 {
		t.Fatalf("matched=%v err=%v increment=%+v, want one call and no tokens", matched, err, increment)
	}
}

func TestNewUsageDeriverBuildsGeminiCLIRule(t *testing.T) {
	for _, harness := range []string{"gemini-cli", "gemini"} {
		t.Run(harness, func(t *testing.T) {
			t.Setenv("SCION_HARNESS", harness)
			t.Setenv("SCION_USAGE_SOURCE", "native")
			d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
			if err != nil {
				t.Fatal(err)
			}
			if len(d.rules) != 1 {
				t.Fatalf("rules = %d, want 1", len(d.rules))
			}
			if _, ok := d.rules[0].(geminiCLIUsageRule); !ok {
				t.Fatalf("rule = %T, want geminiCLIUsageRule", d.rules[0])
			}
		})
	}
}

// TestNewUsageDeriverGeminiCLIStaysOffWithoutNativeSource pins the D10
// gate from the deriver side: with SCION_USAGE_SOURCE unset (what
// provision.py writes until it declares native), no rule is built.
func TestNewUsageDeriverGeminiCLIStaysOffWithoutNativeSource(t *testing.T) {
	t.Setenv("SCION_HARNESS", "gemini-cli")
	t.Setenv("SCION_USAGE_SOURCE", "")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("gemini-cli deriver must be a no-op unless SCION_USAGE_SOURCE=native")
	}
}

// TestPipelineDerivesGeminiCLIUsageEndToEnd runs the capture through the
// real pipeline: one error call (the 503), one success call with tokens,
// and nothing from api_request or the operation.details siblings.
func TestPipelineDerivesGeminiCLIUsageEndToEnd(t *testing.T) {
	result := deriveUsageThroughPipeline(t, "gemini-cli", loadGeminiCLIUsageFixture(t))
	if result.diag.Derived != 2 || result.diag.Malformed != 0 || result.diag.Duplicate != 0 {
		t.Fatalf("diagnostics = %+v, want Derived=2 Malformed=0 Duplicate=0", result.diag)
	}
	if len(result.callsByStatus) != 2 || result.callsByStatus["success"] != 1 || result.callsByStatus["error"] != 1 {
		t.Fatalf("calls by status = %+v, want success=1 error=1", result.callsByStatus)
	}
	assertTokensByType(t, result.tokensByType, map[string]int64{
		telemetrycontract.TokenTypeInput:     310,
		telemetrycontract.TokenTypeOutput:    100,
		telemetrycontract.TokenTypeCacheRead: 1200,
		telemetrycontract.TokenTypeReasoning: 20,
	})
	for _, labels := range result.tokenLabels {
		if labels["harness"] != "gemini-cli" || labels["model"] != "gemini-2.5-pro" {
			t.Errorf("unexpected tokens series labels: %+v", labels)
		}
	}
}

// TestReceiverGeminiCLIInstalledLogShapePrivacy pins that gemini-cli's
// prompt-bearing native log shape cannot carry its system prompt to log
// egress. gemini-cli emits gen_ai.system_instructions (its full system
// prompt) on every gen_ai.client.inference.operation.details record even
// with logPrompts=false, and today only the mandatory-redact entry for that
// key keeps it out of the exported logs -- so this asserts it with both the
// default and an empty redaction list, using the captured fixture's real
// record shape with a marker substituted for the (already scrubbed) value.
func TestReceiverGeminiCLIInstalledLogShapePrivacy(t *testing.T) {
	const marker = "PRIVATE_GEMINI_SYSTEM_INSTRUCTIONS"
	for _, tc := range []struct {
		name      string
		redaction RedactionConfig
	}{
		{name: "default", redaction: RedactionConfig{Redact: DefaultRedactFields, Hash: DefaultHashFields}},
		{name: "empty", redaction: RedactionConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resourceLogs := loadGeminiCLIUsageFixture(t)
			substituted := 0
			for _, rl := range resourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, record := range sl.LogRecords {
						for _, kv := range record.Attributes {
							if kv.Key == "gen_ai.system_instructions" {
								kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}}
								substituted++
							}
						}
					}
				}
			}
			if substituted == 0 {
				t.Fatal("fixture has no gen_ai.system_instructions attribute to substitute")
			}

			p := NewWithConfig(&Config{
				Enabled: true,
				Filter: FilterConfig{Include: []string{
					"gen_ai.client.inference.operation.details",
					geminiCLIAPIResponseEvent, geminiCLIAPIErrorEvent, "gemini_cli.api_request",
				}},
				Redaction: tc.redaction,
			})
			p.retryConfig = fastRetryConfig()
			var captured []*logspb.ResourceLogs
			p.exporter = &CloudExporter{logClient: &mockLogClient{exportFunc: func(_ context.Context, req *collogspb.ExportLogsServiceRequest, _ ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
				captured = append(captured, req.ResourceLogs...)
				return &collogspb.ExportLogsServiceResponse{}, nil
			}}}

			body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: resourceLogs})
			if err != nil {
				t.Fatal(err)
			}
			receiver := &Receiver{logHandler: p.handleLogs}
			response := httptest.NewRecorder()
			receiver.handleHTTPLogs(response, otlpHTTPRequest("/v1/logs", bytes.NewReader(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("receiver status %d: %s", response.Code, response.Body.String())
			}

			// The operation.details records must actually reach egress, so
			// the marker assertion below is not vacuous.
			exported := 0
			for _, rl := range captured {
				for _, sl := range rl.ScopeLogs {
					for _, record := range sl.LogRecords {
						if logAttrString(record.Attributes, "event.name") != "gen_ai.client.inference.operation.details" {
							continue
						}
						if logAttrPresent(record.Attributes, "gen_ai.system_instructions") {
							assertRedacted(t, record.Attributes, "gen_ai.system_instructions")
						}
						exported++
					}
				}
			}
			if exported != substituted {
				t.Fatalf("exported operation.details records = %d, want %d", exported, substituted)
			}
			egressBytes, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: captured})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(egressBytes, []byte(marker)) {
				t.Fatal("gemini-cli system instructions survived log egress")
			}
		})
	}
}
