/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const claudeUsageFixturePath = "testdata/usage/claude-2.1.280.pb.json"

// loadClaudeUsageFixture loads the captured, secret-scrubbed Claude Code
// 2.1.280 payload (api_request success, api_error failure, plus unrelated
// events) used to pin the Claude usage rule.
func loadClaudeUsageFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	data, err := os.ReadFile(claudeUsageFixturePath)
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

// fixtureRecordsByEvent returns every log record in the fixture whose
// event.name attribute equals name.
func fixtureRecordsByEvent(t *testing.T, name string) []*logspb.LogRecord {
	t.Helper()
	var out []*logspb.LogRecord
	for _, rl := range loadClaudeUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if logAttrString(record.Attributes, "event.name") == name {
					out = append(out, record)
				}
			}
		}
	}
	return out
}

func TestClaudeUsageRuleMatchesFixtureAPIRequest(t *testing.T) {
	records := fixtureRecordsByEvent(t, "api_request")
	if len(records) != 1 {
		t.Fatalf("fixture api_request records = %d, want 1", len(records))
	}
	increment, matched, err := claudeUsageRule{}.MatchLog(claudeUsageScope, records[0])
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("api_request did not match claudeUsageRule")
	}
	if increment.Calls != 1 || increment.Status != "success" || increment.Model != "claude-sonnet-5" {
		t.Fatalf("increment = %+v", increment)
	}
	want := map[string]int64{"input": 2, "output": 41, "cache_write": 26607}
	if len(increment.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", increment.Tokens, want)
	}
	for k, v := range want {
		if increment.Tokens[k] != v {
			t.Errorf("tokens[%q] = %d, want %d", k, increment.Tokens[k], v)
		}
	}
	// cache_read_tokens was 0 in the capture: a zero-valued type must not
	// appear (design §3.2 "Tokens map[string]int64 // token_type -> n (>=0)").
	if _, ok := increment.Tokens["cache_read"]; ok {
		t.Error("zero-valued cache_read token_type must be omitted")
	}
}

func TestClaudeUsageRuleMatchesFixtureAPIError(t *testing.T) {
	records := fixtureRecordsByEvent(t, "api_error")
	if len(records) != 1 {
		t.Fatalf("fixture api_error records = %d, want 1", len(records))
	}
	increment, matched, err := claudeUsageRule{}.MatchLog(claudeUsageScope, records[0])
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("api_error did not match claudeUsageRule")
	}
	if increment.Calls != 1 || increment.Status != "error" || len(increment.Tokens) != 0 {
		t.Fatalf("increment = %+v", increment)
	}
}

func TestClaudeUsageRuleIgnoresUnrelatedEvents(t *testing.T) {
	rule := claudeUsageRule{}
	for _, name := range []string{"hook_execution_start", "user_prompt", "assistant_response"} {
		records := fixtureRecordsByEvent(t, name)
		if len(records) == 0 {
			t.Fatalf("fixture missing %s records", name)
		}
		if _, matched, err := rule.MatchLog(claudeUsageScope, records[0]); matched || err != nil {
			t.Errorf("event %s: matched=%v err=%v, want matched=false err=nil", name, matched, err)
		}
	}
	// Wrong scope never matches, even for a real api_request record.
	apiRequest := fixtureRecordsByEvent(t, "api_request")[0]
	if _, matched, _ := rule.MatchLog("some.other.scope", apiRequest); matched {
		t.Error("claudeUsageRule matched outside its native scope")
	}
}

func TestClaudeUsageRuleMalformedTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-sonnet-5"}}},
		{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "not-a-number"}}},
	}}
	_, matched, err := claudeUsageRule{}.MatchLog(claudeUsageScope, record)
	if !matched {
		t.Fatal("expected the malformed api_request to still match (so it counts as usage_malformed, not silently ignored)")
	}
	if err == nil {
		t.Fatal("expected a malformed-field error")
	}
}

func TestClaudeUsageRuleNegativeTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -1}}},
	}}
	if _, matched, err := (claudeUsageRule{}).MatchLog(claudeUsageScope, record); !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
	}
}

func TestBoundedLRUDedupeCapacityAndTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newBoundedLRU(2, time.Minute)
	l.now = func() time.Time { return now }

	if l.SeenBefore("a") {
		t.Fatal("first sighting of a must not be a duplicate")
	}
	if !l.SeenBefore("a") {
		t.Fatal("second sighting of a within TTL must be a duplicate")
	}
	// Exceed capacity: "a" is evicted once "b" and "c" both arrive.
	l.SeenBefore("b")
	l.SeenBefore("c")
	if l.SeenBefore("a") {
		t.Fatal("a should have been evicted by capacity and count as new again")
	}
	// TTL expiry: advance past the window and confirm "b" is forgotten too.
	now = now.Add(2 * time.Minute)
	if l.SeenBefore("b") {
		t.Fatal("b should have expired via TTL")
	}
}

// bareUsageDeriver builds a UsageDeriver with no MeterProvider, exercising
// observe()/dedupe/diagnostics without any network dependency. record()
// guards every use of d.calls/d.tokens against nil, so this is safe.
func bareUsageDeriver(rules ...usageRule) *UsageDeriver {
	return &UsageDeriver{
		rules:            rules,
		seen:             newBoundedLRU(usageDedupeCapacity, usageDedupeTTL),
		resourceIdentity: "test-resource",
	}
}

func TestUsageDeriverObserveDedupesReplayedRequest(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	record := fixtureRecordsByEvent(t, "api_request")[0]

	if !d.observe(context.Background(), claudeUsageScope, record) {
		t.Fatal("first observation should record")
	}
	if d.observe(context.Background(), claudeUsageScope, record) {
		t.Fatal("replayed observation should be deduped, not recorded again")
	}
	diag := d.Diagnostics()
	if diag.Derived != 1 || diag.Duplicate != 1 || diag.Malformed != 0 {
		t.Fatalf("diagnostics = %+v", diag)
	}
}

func TestUsageDeriverObserveCountsMalformed(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "bogus"}}},
	}}
	if d.observe(context.Background(), claudeUsageScope, record) {
		t.Fatal("malformed event must not be recorded")
	}
	diag := d.Diagnostics()
	if diag.Malformed != 1 || diag.Derived != 0 {
		t.Fatalf("diagnostics = %+v", diag)
	}
}

func TestUsageDeriverObserveIgnoresRecordWithNoEventName(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	if d.observe(context.Background(), claudeUsageScope, &logspb.LogRecord{}) {
		t.Fatal("a record with no event.name must never be recorded")
	}
}

func TestNewUsageDeriverIsNoOpWithoutNativeSource(t *testing.T) {
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "hooks") // not "native"
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("deriver must be a no-op unless SCION_USAGE_SOURCE=native")
	}
	// A no-op deriver must tolerate every call.
	d.ProcessResourceLogs(context.Background(), loadClaudeUsageFixture(t))
	if diag := d.Diagnostics(); diag != (UsageDiagnostics{}) {
		t.Fatalf("no-op deriver diagnostics = %+v", diag)
	}
}

func TestNewUsageDeriverIsNoOpForUnknownHarness(t *testing.T) {
	t.Setenv("SCION_HARNESS", "some-future-harness")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("a harness with no rule must yield a no-op deriver")
	}
}

// TestPipelineDerivesClaudeUsageEndToEnd is the §7.5 pipeline end-to-end
// test: it posts the captured Claude fixture over OTLP/HTTP through the real
// receiver, force-flushes, and asserts the exported GCP ResourceMetrics
// contain gen_ai.api.calls and scion.usage.tokens points with exactly the
// canonical label set (AC-1.1, AC-1.1b). The policy's Filter.Include is set
// to exclude every native Claude event, proving derivation runs before the
// filter (AC-1.4), and the fixture is replayed to prove a repeat doesn't
// double count (AC-1.4).
func TestPipelineDerivesClaudeUsageEndToEnd(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-usage-1")
	t.Setenv("SCION_AGENT_SLUG", "usage-agent-slug")
	t.Setenv("SCION_PROJECT_ID", "project-usage-1")
	t.Setenv("SCION_HARNESS", "claude")
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
		// No real event name is included, so every raw Claude log record
		// (including api_request/api_error) is dropped by the filter. The
		// derived usage metrics must still appear.
		Filter: FilterConfig{Include: []string{"nonexistent_event"}},
	}
	p := NewWithConfig(cfg)
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sdkExporter}}

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
		t.Fatal("expected the claude usage rule to be active")
	}
	p.usageDeriver = deriver
	defer func() { _ = deriver.Shutdown(context.Background()) }()

	postFixture := func() {
		t.Helper()
		body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: loadClaudeUsageFixture(t)})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		receiver.handleHTTPLogs(rec, otlpHTTPRequest("/v1/logs", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("post fixture status = %d: %s", rec.Code, rec.Body.String())
		}
	}

	postFixture()
	// snapshotGCP requires at least 2ms between a hook-style point's
	// collector epoch and its observed end, so the pinned Monitoring SDK
	// never has to rewrite a near-zero interval (metric_streams.go
	// snapshotGCP). The points were just admitted, so give it a moment.
	time.Sleep(10 * time.Millisecond)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first metric flush not confirmed")
	}

	diag := p.UsageDiagnostics()
	if diag.Derived != 2 || diag.Duplicate != 0 || diag.Malformed != 0 {
		t.Fatalf("diagnostics after first post = %+v", diag)
	}

	series := allCapturedSeries(capture)
	assertUsageSeries(t, series)

	// Replay: a retried request must not double count (AC-1.4).
	postFixture()
	time.Sleep(10 * time.Millisecond)
	p.flushMetricBuffer(context.Background(), true)
	diag = p.UsageDiagnostics()
	if diag.Derived != 2 || diag.Duplicate != 2 {
		t.Fatalf("diagnostics after replay = %+v, want Derived=2 Duplicate=2", diag)
	}
	assertUsageSeries(t, allCapturedSeries(capture))
}

func allCapturedSeries(capture *monitoringCapture) []*monitoringpb.TimeSeries {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	var out []*monitoringpb.TimeSeries
	for _, req := range capture.series {
		out = append(out, req.TimeSeries...)
	}
	return out
}

// assertUsageSeries pins AC-1.1, AC-1.1b and AC-1.2: exactly the expected
// points, with exactly the canonical label set and nothing else (no leaked
// Claude attribute such as request_id, prompt.id or event.timestamp).
func assertUsageSeries(t *testing.T, series []*monitoringpb.TimeSeries) {
	t.Helper()
	var calls, tokens []*monitoringpb.TimeSeries
	for _, ts := range series {
		switch ts.Metric.Type {
		case "workload.googleapis.com/gen_ai.api.calls":
			calls = append(calls, ts)
		case "workload.googleapis.com/scion.usage.tokens":
			tokens = append(tokens, ts)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("gen_ai.api.calls series = %d, want 2 (success, error): %+v", len(calls), describeSeries(calls))
	}
	byStatus := map[string]int64{}
	for _, ts := range calls {
		assertLabelKeys(t, ts.Metric.Labels, "agent_id", "project_id", "harness", "model", "status",
			"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
			"scion_agent_id", "scion_project_id", "scion_agent_slug",
			"service_name", "service_instance_id")
		if ts.Metric.Labels["harness"] != "claude" || ts.Metric.Labels["model"] != "claude-sonnet-5" {
			t.Errorf("unexpected calls series labels: %+v", ts.Metric.Labels)
		}
		if ts.Metric.Labels["scion_agent_id"] != "agent-usage-1" || ts.Metric.Labels["scion_project_id"] != "project-usage-1" || ts.Metric.Labels["scion_agent_slug"] != "usage-agent-slug" {
			t.Errorf("unexpected canonical identity labels: %+v", ts.Metric.Labels)
		}
		byStatus[ts.Metric.Labels["status"]] = ts.Points[0].Value.GetInt64Value()
	}
	if byStatus["success"] != 1 || byStatus["error"] != 1 {
		t.Fatalf("calls by status = %+v, want success=1 error=1", byStatus)
	}

	wantTokens := map[string]int64{"input": 2, "output": 41, "cache_write": 26607}
	if len(tokens) != len(wantTokens) {
		t.Fatalf("scion.usage.tokens series = %d, want %d: %+v", len(tokens), len(wantTokens), describeSeries(tokens))
	}
	byType := map[string]int64{}
	for _, ts := range tokens {
		assertLabelKeys(t, ts.Metric.Labels, "harness", "model", "token_type",
			"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
			"scion_agent_id", "scion_project_id", "scion_agent_slug",
			"service_name", "service_instance_id")
		byType[ts.Metric.Labels["token_type"]] = ts.Points[0].Value.GetInt64Value()
	}
	for tokenType, want := range wantTokens {
		if byType[tokenType] != want {
			t.Errorf("tokens[%q] = %d, want %d", tokenType, byType[tokenType], want)
		}
	}
}

func assertLabelKeys(t *testing.T, labels map[string]string, want ...string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for k := range labels {
		if !wantSet[k] {
			t.Errorf("unexpected label leaked into export: %q (value %q)", k, labels[k])
		}
	}
	for _, k := range want {
		if _, ok := labels[k]; !ok {
			t.Errorf("missing expected label %q in %+v", k, labels)
		}
	}
}

func describeSeries(series []*monitoringpb.TimeSeries) []string {
	var out []string
	for _, ts := range series {
		out = append(out, ts.Metric.Type)
	}
	sort.Strings(out)
	return out
}
