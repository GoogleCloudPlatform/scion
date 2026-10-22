/*
Copyright 2026 The Scion Authors.
*/

package telemetry

// Tests for WP3 diagnostics and the telemetry.health event
// (telemetry/design/wp3-design.md). Expected counts are calculated by hand
// from the fixtures listed in each test, independently of the code.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const stubUsageScope = "stub.scope"

// stubLogRule recognizes three event names: usage.ok (one call, tokens),
// usage.empty (recognized, nothing to record) and usage.bad (one call, a
// malformed token field). Everything else is not a usage frame.
type stubLogRule struct{}

func (stubLogRule) Harness() string { return "stub" }

func (stubLogRule) MatchLog(_, eventName string, _ *logspb.LogRecord) (usageIncrement, bool, error) {
	switch eventName {
	case "usage.ok":
		return usageIncrement{Status: "success", Calls: 1, Tokens: map[string]int64{"input": 3}}, true, nil
	case "usage.empty":
		return usageIncrement{}, true, nil
	case "usage.bad":
		return usageIncrement{Status: "success", Calls: 1}, true, errors.New("malformed token field")
	}
	return usageIncrement{}, false, nil
}

// stubMetricRule derives a fixed set of increments per batch.
type stubMetricRule struct {
	increments []usageIncrement
	err        error
}

func (stubMetricRule) Harness() string { return "stub" }
func (stubMetricRule) MatchLog(string, string, *logspb.LogRecord) (usageIncrement, bool, error) {
	return usageIncrement{}, false, nil
}
func (r stubMetricRule) DeriveBatch(*metricpb.ResourceMetrics) ([]usageIncrement, map[string]bool, error) {
	return r.increments, map[string]bool{"stub.metric": true}, r.err
}

func namedRecord(name string, timeNanos uint64) *logspb.LogRecord {
	r := &logspb.LogRecord{TimeUnixNano: timeNanos}
	if name != "" {
		r.Attributes = []*commonpb.KeyValue{kvs("event.name", name)}
	}
	return r
}

// Counter populations (design section 1.1): non-usage frames are received
// but not recognized; recognized observations split exactly into ignored,
// duplicate and derived; malformed is a flag on recognized frames; log and
// metric units are kept apart.
func TestUsageDiagnosticsPopulations(t *testing.T) {
	d := bareUsageDeriver(stubLogRule{}, stubMetricRule{
		increments: []usageIncrement{
			{Status: "success", Calls: 1, dedupeKey: "a"}, // derived
			{dedupeKey: "zero"},                           // ignored: no call, no tokens
			{Status: "success", Calls: 1, dedupeKey: "a"}, // duplicate of the first
		},
		err: errors.New("one malformed point"),
	})
	logs := []*logspb.ResourceLogs{{
		Resource: nil,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: stubUsageScope}, LogRecords: []*logspb.LogRecord{
			namedRecord("tool_result", 1), // 1 non-usage
			namedRecord("user_prompt", 1), // 2 non-usage
			namedRecord("", 1),            // 3 no event name
			namedRecord("usage.ok", 10),   // 4 derived
			namedRecord("usage.ok", 10),   // 5 duplicate of 4
			namedRecord("usage.ok", 20),   // 6 derived
			namedRecord("usage.empty", 1), // 7 ignored
			namedRecord("usage.bad", 30),  // 8 malformed, derived
		}}},
	}}
	feed(d, logs)
	d.observeMetricBatch(context.Background(), d.rules[1].(metricBatchDeriver), &metricpb.ResourceMetrics{}, map[string]bool{})

	got := d.Diagnostics()
	want := UsageDiagnostics{
		Derived: 3 + 1, Duplicate: 1 + 1, Malformed: 1 + 1,
		LogRecordsReceived: 8, LogRecordsRecognized: 5, LogRecordsMalformed: 1,
		MetricBatchesReceived: 1, MetricBatchesMalformed: 1, MetricIncrements: 3,
		Ignored: 1 + 1,
	}
	if got != want {
		t.Fatalf("diagnostics\n got %+v\nwant %+v", got, want)
	}
	if obs := got.LogRecordsRecognized + got.MetricIncrements; obs != got.Ignored+got.Duplicate+got.Derived {
		t.Fatalf("observations %d != ignored+duplicate+derived %d", obs, got.Ignored+got.Duplicate+got.Derived)
	}

	setAnalyticsEnv(t, "claude")
	ev, err := buildHealthEvent(healthInputs{
		Now: time.Unix(1_761_091_200, 0), PipelineRunID: "run", Seq: 1, Trigger: telemetrycontract.HealthTriggerPeriodic,
		AgentID: "agent-1", Harness: "claude", DeliveryState: "running",
		Usage: enabledUsage(telemetrycontract.UsageSourceNative, "claude"), SourceOrigin: telemetrycontract.UsageSourceOriginOverlay,
		UsageDiag: &got, AnalyticsDisabledReason: "outbox_unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]int64{
		"log_records_malformed (Malformed - metric batches)": {*ev.UsageLogRecordsMalformed, 1},
		"metric_batches_malformed":                           {*ev.UsageMetricBatchesMalformed, 1},
		"observations_derived":                               {*ev.UsageObservationsDerived, 4},
		"log_records_received":                               {*ev.UsageLogRecordsReceived, 8},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d, want %d", name, pair[0], pair[1])
		}
	}
}

// The harness version comes from the last recognized record that carried
// one; non-usage frames never set it.
func TestUsageDiagnosticsHarnessVersion(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	res := []*commonpb.KeyValue{kvs("service.version", "2.1.0")}
	feed(d, []*logspb.ResourceLogs{{
		Resource:  nil,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope}, LogRecords: []*logspb.LogRecord{namedRecord("tool_result", 1)}}},
	}})
	if v := d.Diagnostics().HarnessVersion; v != "" {
		t.Fatalf("version from a non-usage frame: %q", v)
	}
	d.observeRecord(context.Background(), claudeUsageScope, res, claudeRecord("req_011CUabcdefghijklmnopq", "1", 5, fullClaudeTokens))
	if v := d.Diagnostics().HarnessVersion; v != "2.1.0" {
		t.Fatalf("HarnessVersion = %q", v)
	}
}

// Export retries (design section 1.2): retries are calls after the first
// for the same batch, for logs (bounded retryExport) and metrics (flush
// slots of a pending batch).
func TestExportRetriesCounted(t *testing.T) {
	calls := 0
	logClient := &mockLogClient{exportFunc: func(context.Context, *collogspb.ExportLogsServiceRequest, ...grpc.CallOption) (*collogspb.ExportLogsServiceResponse, error) {
		calls++
		if calls < 3 {
			return nil, status.Error(codes.Unavailable, "try again")
		}
		return &collogspb.ExportLogsServiceResponse{}, nil
	}}
	metricCalls := 0
	metricClient := &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		metricCalls++
		if metricCalls == 1 {
			return nil, status.Error(codes.Unavailable, "try again")
		}
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}
	p := newTestPipelineWithExporter(nil, metricClient, logClient)
	p.retryConfig = fastRetryConfig()
	if err := p.handleLogs(context.Background(), []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}); err != nil {
		t.Fatal(err)
	}
	logsDiag := p.Diagnostics()["logs"]
	if logsDiag.Attempts != 3 || logsDiag.Retries != 2 || logsDiag.Failed != 0 || logsDiag.Delivered != 1 {
		t.Fatalf("logs diagnostics %+v, want attempts 3 retries 2 failed 0 delivered 1", logsDiag)
	}

	input := []*metricpb.ResourceMetrics{testMetricResource("native", "scope", "", "", testNumber("safe", metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, 1, 2, 1))}
	if err := p.handleMetrics(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first flush should fail")
	}
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("second flush should deliver")
	}
	m := p.Diagnostics()["metrics"]
	if m.Attempts != 2 || m.Failed != 1 || m.Retries != 1 || m.Delivered != 1 || m.Accepted != 1 {
		t.Fatalf("metric diagnostics %+v, want attempts 2 failed 1 retries 1 delivered 1", m)
	}
}

// A rejected metric request rejects all its points, and the reason is
// reported in the local snapshot (bounded), never in the health event.
func TestMetricRejectReasonsExposed(t *testing.T) {
	p := newTestPipelineWithExporter(nil, &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}, nil)
	p.ensureMetricStreamsLocked()
	p.metricStreams.rejected["b reason"] = 1
	p.metricStreams.rejected["a reason"] = 3
	p.metricStreams.rejected["c reason"] = 3
	if got := p.metricRejectReasons(2); got != `{"a reason":3,"c reason":3}` {
		t.Fatalf("reasons = %s", got)
	}
}

func TestCountingSendRetries(t *testing.T) {
	fail := true
	c := newCountingSend(func(context.Context, []byte) error {
		if fail {
			return errors.New("down")
		}
		return nil
	})
	ctx := context.Background()
	_ = c.Send(ctx, []byte("a")) // first attempt, fails
	_ = c.Send(ctx, []byte("a")) // retry 1, fails
	fail = false
	_ = c.Send(ctx, []byte("a")) // retry 2, succeeds
	_ = c.Send(ctx, []byte("b")) // new line
	_ = c.Send(ctx, []byte("b")) // a resend after success is not a retry
	if got := c.Retries(); got != 2 {
		t.Fatalf("retries = %d, want 2", got)
	}
	var nilSend *countingSend
	if nilSend.Retries() != 0 {
		t.Fatal("nil countingSend")
	}
}

func TestResolveUsageSelection(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		name    string
		env     map[string]string
		overlay map[string]string
		want    UsageSourceSelection
	}{
		{"runtime env wins even empty", map[string]string{"SCION_USAGE_SOURCE": ""}, map[string]string{"SCION_USAGE_SOURCE": "native"},
			UsageSourceSelection{Settled: true, Source: "", Origin: telemetrycontract.UsageSourceOriginRuntimeEnv, NativePolicy: "enabled"}},
		{"overlay", nil, map[string]string{"SCION_USAGE_SOURCE": "hooks"},
			UsageSourceSelection{Settled: true, Source: "hooks", Origin: telemetrycontract.UsageSourceOriginOverlay, NativePolicy: "enabled"}},
		{"unset", nil, nil,
			UsageSourceSelection{Settled: true, Origin: telemetrycontract.UsageSourceOriginUnset, NativePolicy: "enabled"}},
	}
	for _, c := range cases {
		if got := resolveUsageSelection(env(c.env), c.overlay, "enabled"); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

// Disabled collection: every reason, plus the enabled shapes.
func TestDeriveUsageCollection(t *testing.T) {
	native := func(origin, policy string) UsageSourceSelection {
		return UsageSourceSelection{Settled: true, Source: "native", Origin: origin, NativePolicy: policy}
	}
	cases := []struct {
		name           string
		sel            UsageSourceSelection
		harness        string
		active, hasRul bool
		state, reason  string
		caps           string
		capStatus      string
		granularity    string
		positiveOnly   bool
	}{
		{"undetermined", UsageSourceSelection{}, "claude", false, true, "undetermined", "", "", "", "", false},
		{"unset", UsageSourceSelection{Settled: true}, "claude", false, true, "disabled", "source_unset", "", "", "", false},
		{"unrecognized", UsageSourceSelection{Settled: true, Source: "otel"}, "claude", false, true, "disabled", "source_unrecognized", "", "", "", false},
		{"policy disabled", native("overlay", "disabled"), "claude", false, true, "disabled", "native_policy_disabled", "", "", "", false},
		{"no harness rule", native("overlay", "enabled"), "opencode", false, false, "disabled", "no_harness_rule", "", "", "", false},
		{"deriver failed", native("runtime_env", ""), "claude", false, true, "disabled", "deriver_failed", "", "", "", false},
		{"native claude", native("overlay", "enabled"), "claude", true, true, "enabled", "", "calls,input,output,cache_read,cache_write,reported_cost", "static_table", "request", false},
		{"native copilot", native("overlay", "enabled"), "copilot", true, true, "enabled", "", "calls,input,output,cache_read,cache_write", "static_table", "interval", false},
		{"runtime native before settle", UsageSourceSelection{}, "codex", true, true, "enabled", "", "calls,input,output,cache_read,cache_write,reasoning", "static_table", "request", false},
		{"hooks antigravity", UsageSourceSelection{Settled: true, Source: "hooks"}, "antigravity", false, false, "enabled", "", "calls", "static_table", "invocation", false},
		{"hooks other", UsageSourceSelection{Settled: true, Source: "hooks"}, "opencode", false, false, "enabled", "", "", "unknown", "invocation", true},
	}
	for _, c := range cases {
		got := deriveUsageCollection(c.sel, c.harness, c.active, c.hasRul)
		if got.State != c.state || got.Reason != c.reason || strings.Join(got.Capabilities, ",") != c.caps ||
			got.CapabilityStatus != c.capStatus || got.Granularity != c.granularity || got.PositiveOnly != c.positiveOnly {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
}

// fixedHealthInputs are the golden inputs; every value is chosen by hand.
func fixedHealthInputs(trigger string) healthInputs {
	return healthInputs{
		Now:           time.Date(2026, 10, 22, 6, 0, 0, 123456789, time.UTC),
		PipelineRunID: "0f9a3c1e-0000-4000-8000-000000000001",
		Seq:           1,
		Trigger:       trigger,
		AgentID:       "agent-1", ProjectID: "proj-1", BrokerID: "broker-1", LaunchID: "launch-1",
		Harness: "claude", HarnessVersion: "2.1.0",
		Usage:         enabledUsage(telemetrycontract.UsageSourceNative, "claude"),
		SourceOrigin:  telemetrycontract.UsageSourceOriginOverlay,
		DeliveryState: "running",
		UsageDiag: &UsageDiagnostics{Derived: 4, Duplicate: 1, Malformed: 1, LogRecordsReceived: 40, LogRecordsRecognized: 6,
			Ignored: 1},
		AnalyticsEnabled: true,
		Analytics: telemetrycontract.HealthAnalyticsCounters{
			Appended: 4, Finalized: 4, Delivered: 3, SendFailures: 2, SendRetries: 2, PendingBytes: 0,
			ProducerEpoch: "epoch-1", ProducerSeqLast: 4,
		},
		Metrics: DeliverySnapshot{Accepted: 12, Delivered: 12, Attempts: 3, Retries: 1, Failed: 1},
		Logs:    DeliverySnapshot{Accepted: 40, Filtered: 30, Delivered: 10, Attempts: 5},
	}
}

func goldenHealth(t *testing.T, name string, in healthInputs) []byte {
	t.Helper()
	ev, err := buildHealthEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	ev.HubID = "hub-1"
	line, err := telemetrycontract.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "health", name+".json")
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, line, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	if *updateUsageGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden (run with -update): %v", err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Fatalf("%s mismatch\n got %s\nwant %s", name, pretty.Bytes(), want)
	}
	var back telemetrycontract.HealthEvent
	if err := telemetrycontract.Unmarshal(line, &back); err != nil {
		t.Fatalf("golden does not round-trip: %v", err)
	}
	return line
}

func setTestVersion(t *testing.T) {
	t.Helper()
	old := version.Version
	version.Version = "v0.0.0-wp3test"
	t.Cleanup(func() { version.Version = old })
}

// Health golden JSON for each trigger and the main shapes.
func TestHealthEventGolden(t *testing.T) {
	setTestVersion(t)

	goldenHealth(t, "start-native-claude", fixedHealthInputs(telemetrycontract.HealthTriggerStart))

	periodic := fixedHealthInputs(telemetrycontract.HealthTriggerPeriodic)
	periodic.Seq = 2
	periodic.Harness, periodic.HarnessVersion = "antigravity", ""
	periodic.Usage = enabledUsage(telemetrycontract.UsageSourceHooks, "antigravity")
	periodic.UsageDiag = nil
	periodic.AnalyticsEnabled, periodic.AnalyticsDisabledReason = false, "outbox_unavailable"
	goldenHealth(t, "periodic-hooks-antigravity", periodic)

	disabled := fixedHealthInputs(telemetrycontract.HealthTriggerStart)
	disabled.Usage = deriveUsageCollection(UsageSourceSelection{Settled: true}, "claude", false, true)
	disabled.SourceOrigin = telemetrycontract.UsageSourceOriginUnset
	disabled.UsageDiag, disabled.HarnessVersion = nil, ""
	goldenHealth(t, "start-disabled-source-unset", disabled)

	final := fixedHealthInputs(telemetrycontract.HealthTriggerFinal)
	final.Seq = 7
	final.Analytics.PendingBytes, final.Analytics.OldestPendingFileAgeMs = 512, i64(61_000)
	final.Analytics.Overflow, final.Analytics.OverflowTotal = 1, 1
	final.FinalResidualRecords = 2
	goldenHealth(t, "final-not-drained", final)

	drained := fixedHealthInputs(telemetrycontract.HealthTriggerFinal)
	drained.Seq = 3
	goldenHealth(t, "final-drained", drained)
}

// Identity: the event ID is a pure function of namespace, run and seq.
func TestHealthEventIdentity(t *testing.T) {
	setTestVersion(t)
	a, _ := buildHealthEvent(fixedHealthInputs(telemetrycontract.HealthTriggerStart))
	b, _ := buildHealthEvent(fixedHealthInputs(telemetrycontract.HealthTriggerStart))
	if a.EventID != b.EventID || !strings.HasPrefix(a.EventID, "h1-") || len(a.EventID) != 35 {
		t.Fatalf("event IDs %q %q", a.EventID, b.EventID)
	}
	if a.ProducerNamespace != "sciontool/project/proj-1/agent/agent-1" {
		t.Fatalf("namespace %q differs from the usage namespace", a.ProducerNamespace)
	}
	in := fixedHealthInputs(telemetrycontract.HealthTriggerStart)
	in.Seq = 2
	c, _ := buildHealthEvent(in)
	in.Seq, in.PipelineRunID = 1, "another-run"
	d, _ := buildHealthEvent(in)
	if c.EventID == a.EventID || d.EventID == a.EventID {
		t.Fatal("seq and run must change the event ID")
	}
	in.AgentID = ""
	if _, err := buildHealthEvent(in); err == nil {
		t.Fatal("no agent identity must not build an event")
	}
}

// Final flush outcome is about drained queues, and loss is reported
// separately: overflow with nothing pending is drained but loss_indicated.
func TestFinalFlushOutcomeAndLoss(t *testing.T) {
	setTestVersion(t)
	in := fixedHealthInputs(telemetrycontract.HealthTriggerFinal)
	in.Analytics.OverflowTotal = 3
	ev, err := buildHealthEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FinalFlushOutcome != "drained" || !ev.LossIndicated || *ev.FinalResidualRecords != 0 {
		t.Fatalf("outcome %q loss %v residual %d", ev.FinalFlushOutcome, ev.LossIndicated, *ev.FinalResidualRecords)
	}
	for name, mutate := range map[string]func(*healthInputs){
		"dead letters total":  func(in *healthInputs) { in.Analytics.DeadLettersTotal = 1 },
		"quarantined":         func(in *healthInputs) { in.Analytics.Quarantined = 1 },
		"oversize total":      func(in *healthInputs) { in.Analytics.OversizeTotal = 1 },
		"append failed total": func(in *healthInputs) { in.Analytics.AppendFailedTotal = 1 },
	} {
		in := fixedHealthInputs(telemetrycontract.HealthTriggerPeriodic)
		mutate(&in)
		if ev, _ := buildHealthEvent(in); !ev.LossIndicated {
			t.Errorf("%s must indicate loss", name)
		}
	}
	conflict := fixedHealthInputs(telemetrycontract.HealthTriggerPeriodic)
	conflict.Analytics.Conflicts = 1
	if ev, _ := buildHealthEvent(conflict); ev.LossIndicated {
		t.Error("a request_id conflict is a quality signal, not loss")
	}
	for name, mutate := range map[string]func(*healthInputs){
		"metric residual":   func(in *healthInputs) { in.FinalResidualRecords = 1 },
		"queued records":    func(in *healthInputs) { in.Queue.Records = 1 },
		"analytics pending": func(in *healthInputs) { in.Analytics.PendingBytes = 10; in.Analytics.OldestPendingFileAgeMs = i64(5) },
		// A failed disk read is not an empty outbox.
		"analytics stats failed": func(in *healthInputs) { in.Analytics.StatsFailed = true },
	} {
		in := fixedHealthInputs(telemetrycontract.HealthTriggerFinal)
		mutate(&in)
		ev, err := buildHealthEvent(in)
		if err != nil {
			t.Fatal(err)
		}
		if ev.FinalFlushOutcome != "not_drained" {
			t.Errorf("%s: outcome %q", name, ev.FinalFlushOutcome)
		}
	}
	clean, _ := buildHealthEvent(fixedHealthInputs(telemetrycontract.HealthTriggerPeriodic))
	if clean.LossIndicated || clean.FinalFlushOutcome != "" || clean.FinalResidualRecords != nil {
		t.Fatalf("periodic clean event: %+v", clean)
	}
}

// healthTestPipeline is a pipeline with health enabled against fake send
// and hub ID functions, and a real outbox.
type healthHarness struct {
	p      *Pipeline
	outbox *AnalyticsOutbox
	mu     sync.Mutex
	sent   [][]byte
	fail   bool
	hub    string
}

// clearUsageSourceEnv removes an inherited SCION_USAGE_SOURCE (agent
// containers export it) for the test, restoring it afterwards.
func clearUsageSourceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_USAGE_SOURCE", "")
	if err := os.Unsetenv("SCION_USAGE_SOURCE"); err != nil {
		t.Fatal(err)
	}
}

func newHealthHarness(t *testing.T) *healthHarness {
	t.Helper()
	setAnalyticsEnv(t, "claude")
	clearUsageSourceEnv(t)
	setTestVersion(t)
	h := &healthHarness{hub: "hub-1"}
	o, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "ob"))
	if err != nil {
		t.Fatal(err)
	}
	h.outbox = o
	p := NewWithConfig(&Config{Enabled: true})
	p.health.sendOverride = func(_ context.Context, line []byte) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail {
			return errors.New("logging down")
		}
		h.sent = append(h.sent, append([]byte(nil), line...))
		return nil
	}
	p.health.hubIDOverride = func(context.Context) string { h.mu.Lock(); defer h.mu.Unlock(); return h.hub }
	p.health.periodic, p.health.retry = time.Hour, time.Hour // driven by hand
	h.p = p
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { p.stopHealthLoop(); cancel() })
	handles := analyticsHandles{native: NewUsageAnalytics(o, telemetrycontract.UsageSourceNative), send: newCountingSend(nil)}
	p.startHealth(ctx, &GCPExporter{}, handles)
	// Drive flushes by hand: no background loop racing the assertions.
	p.stopHealthLoop()
	return h
}

func (h *healthHarness) sentEvents(t *testing.T) []telemetrycontract.HealthEvent {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []telemetrycontract.HealthEvent
	for _, l := range h.sent {
		var ev telemetrycontract.HealthEvent
		if err := telemetrycontract.Unmarshal(l, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// Start waits for the settled usage-source decision; events are held until
// the hub ID is pinned; a failed send is retried with identical bytes.
func TestHealthEmitterHoldRetryAndReplay(t *testing.T) {
	h := newHealthHarness(t)
	ctx := context.Background()

	h.p.flushHealth(ctx)
	if len(h.sent) != 0 {
		t.Fatal("no start event before the usage source settles")
	}
	h.mu.Lock()
	h.hub = ""
	h.fail = true
	h.mu.Unlock()
	h.p.SettleUsageSource(map[string]string{"SCION_USAGE_SOURCE": "native"}, "disabled")
	if got := h.p.health.queue[0].ev.UsageSource; got != "native" {
		t.Fatalf("the selected source must be kept when disabled: %q", got)
	}
	h.p.SettleUsageSource(nil, "") // a second settle never emits a second start
	h.p.flushHealth(ctx)
	if n := len(h.p.health.queue); n != 1 || h.p.health.queue[0].line != nil {
		t.Fatalf("start must be held unfrozen until the hub is pinned: queue=%d", n)
	}
	h.mu.Lock()
	h.hub = "hub-1"
	h.mu.Unlock()
	h.p.flushHealth(ctx) // freezes bytes, send fails
	frozen := append([]byte(nil), h.p.health.queue[0].line...)
	h.mu.Lock()
	h.fail = false
	h.mu.Unlock()
	h.p.flushHealth(ctx)
	if len(h.sent) != 1 || !bytes.Equal(h.sent[0], frozen) {
		t.Fatalf("retry must send the frozen bytes: sent=%d", len(h.sent))
	}
	ev := h.sentEvents(t)[0]
	if ev.Trigger != "start" || ev.HubID != "hub-1" || ev.UsageCollection != "disabled" || ev.UsageDisabledReason != "native_policy_disabled" ||
		ev.UsageSourceOrigin != "overlay" || ev.HealthSeq != 1 {
		t.Fatalf("start event %+v", ev)
	}
	if ev.AnalyticsState != "enabled" || ev.ProducerSeqLast == nil || *ev.ProducerSeqLast != 0 || ev.ProducerEpoch == "" {
		t.Fatalf("analytics fields: state %q seq %v epoch %q", ev.AnalyticsState, ev.ProducerSeqLast, ev.ProducerEpoch)
	}

	h.p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	h.p.flushHealth(ctx)
	evs := h.sentEvents(t)
	if len(evs) != 2 || evs[1].HealthSeq != 2 || evs[1].HealthSendFailures != 1 || evs[1].PipelineRunID != evs[0].PipelineRunID {
		t.Fatalf("periodic event: %+v", evs[len(evs)-1])
	}
}

// The queue is bounded: the oldest undelivered periodic snapshot is
// superseded, the start event is kept.
func TestHealthEmitterQueueCap(t *testing.T) {
	h := newHealthHarness(t)
	h.mu.Lock()
	h.fail = true
	h.mu.Unlock()
	h.p.SettleUsageSource(nil, "")
	for i := 0; i < healthQueueCap+2; i++ {
		h.p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	}
	q := h.p.health.queue
	if len(q) != healthQueueCap || q[0].ev.Trigger != "start" || h.p.health.superseded != 3 {
		t.Fatalf("queue %d first %q superseded %d", len(q), q[0].ev.Trigger, h.p.health.superseded)
	}
	if q[1].ev.HealthSeq != 5 {
		t.Fatalf("oldest periodic kept has seq %d, want 5 (seqs 2-4 superseded)", q[1].ev.HealthSeq)
	}
}

// Final flush with pending analytics items and an overflow: the final event
// is sent first, reports not_drained, pending bytes, the seq high-water
// mark (advanced by the refused event) and loss_indicated.
func TestHealthFinalWithPendingAndOverflow(t *testing.T) {
	h := newHealthHarness(t)
	h.p.SettleUsageSource(map[string]string{"SCION_USAGE_SOURCE": "hooks"}, "")
	h.mu.Lock()
	h.fail = true
	h.mu.Unlock()
	h.p.flushHealth(context.Background()) // start stays queued

	// Two usage events: the first fits, the second overflows.
	native := NewUsageAnalytics(h.outbox, telemetrycontract.UsageSourceNative)
	obs := claudeObservation("api_request", claudeRecord("req_011CUabcdefghijklmnopq", "1", 5, fullClaudeTokens).Attributes)
	obs.Identity, obs.SourceEventID, obs.SourceKind, obs.Status, obs.Calls = telemetrycontract.SourceIdentityComposite, "cmp1:aa", "claude.api_request", "success", 1
	if out, err := native.Record(obs); err != nil || out != AppendWritten {
		t.Fatalf("append: %v %v", out, err)
	}
	h.outbox.maxPending = 1 // everything further overflows
	if out, _ := h.p.health.analyticsHandles.native.Record(obs); out != AppendOverflow {
		t.Fatalf("second append outcome %v, want overflow", out)
	}
	h.mu.Lock()
	h.fail = false
	h.mu.Unlock()
	h.p.emitFinalHealth(context.Background())

	evs := h.sentEvents(t)
	if len(evs) != 2 || evs[0].Trigger != "final" || evs[1].Trigger != "start" {
		t.Fatalf("sent %d events, want final then start", len(evs))
	}
	final := evs[0]
	if final.FinalFlushOutcome != "not_drained" || *final.AnalyticsPendingBytes == 0 || final.AnalyticsOldestPendingFileAgeMs == nil {
		t.Fatalf("final pending: outcome %q pending %d", final.FinalFlushOutcome, *final.AnalyticsPendingBytes)
	}
	if *final.AnalyticsOverflow != 1 || *final.AnalyticsOverflowTotal != 1 || *final.ProducerSeqLast != 2 || *final.AnalyticsAppended != 1 || !final.LossIndicated {
		t.Fatalf("final overflow: overflow %d total %d seq %d appended %d loss %v",
			*final.AnalyticsOverflow, *final.AnalyticsOverflowTotal, *final.ProducerSeqLast, *final.AnalyticsAppended, final.LossIndicated)
	}
	if final.UsageSource != "hooks" || final.UsageCapabilityStatus != "unknown" || final.UsageLogRecordsReceived != nil {
		t.Fatalf("hooks final: source %q caps %q usage counters %v", final.UsageSource, final.UsageCapabilityStatus, final.UsageLogRecordsReceived)
	}
	if h.p.health.enabled || len(h.p.health.queue) != 0 {
		t.Fatal("emitter must be disabled and empty after the final flush")
	}
}

// Without a GCP exporter no health event is produced, and the local
// diagnostics report why analytics is off.
func TestHealthDisabledWithoutGCPExporter(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	clearUsageSourceEnv(t)
	p := NewWithConfig(&Config{Enabled: true})
	p.startHealth(context.Background(), nil, analyticsHandles{disabledReason: AnalyticsDisabledNoGCPExporter})
	p.SettleUsageSource(nil, "")
	p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
	if p.health.enabled || len(p.health.queue) != 0 {
		t.Fatal("health must stay disabled without the GCP exporter")
	}
	if _, ok, reason := p.AnalyticsDiagnostics(); ok || reason != AnalyticsDisabledNoGCPExporter {
		t.Fatalf("analytics diagnostics ok=%v reason=%q", ok, reason)
	}
	p.emitFinalHealth(context.Background()) // no-op, no panic
}

// With analytics enabled the native path offers one usage event per derived
// AND per duplicate observation (design section 1.1): offered = derived +
// duplicate, and every offered event ends in exactly one outbox outcome.
func TestOfferedEventsEqualDerivedPlusDuplicate(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), claudeUsageRule{})
	rid := "req_011CUabcdefghijklmnopq"
	feed(d, claudeLogs(
		claudeRecord(rid, "1", 10, fullClaudeTokens), // derived, written
		claudeRecord(rid, "1", 10, fullClaudeTokens), // counter duplicate, outbox certain duplicate
		claudeRecord("", "2", 20, fullClaudeTokens),  // derived, written (composite)
		claudeRecord("", "2", 20, fullClaudeTokens),  // counter duplicate, written as ambiguous occurrence 2
		namedRecord("tool_result", 30),               // not a usage frame: not offered
	))
	diag := d.Diagnostics()
	s := o.Stats()
	offered := s.Appended + s.CertainDuplicates + s.Overflow + s.AppendFailed + d.analytics.DroppedNoIdentity()
	if diag.Derived != 2 || diag.Duplicate != 2 || offered != diag.Derived+diag.Duplicate {
		t.Fatalf("derived %d duplicate %d offered %d (appended %d certain dup %d)", diag.Derived, diag.Duplicate, offered, s.Appended, s.CertainDuplicates)
	}
	if s.Appended != 3 || s.CertainDuplicates != 1 {
		t.Fatalf("appended %d certain duplicates %d, want 3 and 1", s.Appended, s.CertainDuplicates)
	}
}

// A failed on-disk stat of the outbox is reported, not read as empty.
func TestAnalyticsStatsFailureReported(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	dir := filepath.Join(t.TempDir(), "ob")
	o, err := OpenAnalyticsOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cursor"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := analyticsHandles{native: NewUsageAnalytics(o, telemetrycontract.UsageSourceNative)}.counters()
	if !c.StatsFailed || c.PendingBytes != 0 {
		t.Fatalf("stats failed %v pending %d", c.StatsFailed, c.PendingBytes)
	}
	in := fixedHealthInputs(telemetrycontract.HealthTriggerFinal)
	in.Analytics = c
	ev, err := buildHealthEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FinalFlushOutcome != "not_drained" || ev.AnalyticsStatsFailed == nil || !*ev.AnalyticsStatsFailed {
		t.Fatalf("outcome %q stats_failed %v", ev.FinalFlushOutcome, ev.AnalyticsStatsFailed)
	}
}

// With SCION_USAGE_SOURCE in the process environment the decision is
// settled at start: start is emitted without a settle call, with origin
// runtime_env, and a later settle (the overlay) emits no second start and
// cannot override the runtime value.
func TestHealthStartFromRuntimeEnv(t *testing.T) {
	setAnalyticsEnv(t, "antigravity")
	setTestVersion(t)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	o, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "ob"))
	if err != nil {
		t.Fatal(err)
	}
	var sent [][]byte
	p := NewWithConfig(&Config{Enabled: true})
	p.health.sendOverride = func(_ context.Context, line []byte) error { sent = append(sent, line); return nil }
	p.health.hubIDOverride = func(context.Context) string { return "hub-1" }
	p.health.periodic, p.health.retry = time.Hour, time.Hour
	p.startHealth(context.Background(), &GCPExporter{}, analyticsHandles{native: NewUsageAnalytics(o, telemetrycontract.UsageSourceNative)})
	p.stopHealthLoop()
	p.SettleUsageSource(map[string]string{"SCION_USAGE_SOURCE": "native"}, "enabled")
	p.flushHealth(context.Background())
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want exactly one start", len(sent))
	}
	var ev telemetrycontract.HealthEvent
	if err := telemetrycontract.Unmarshal(sent[0], &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Trigger != "start" || ev.UsageSourceOrigin != "runtime_env" || ev.UsageSource != "hooks" || ev.UsageCollection != "enabled" ||
		strings.Join(ev.UsageCapabilities, ",") != "calls" {
		t.Fatalf("start from runtime env: %+v", ev)
	}
}

// R1: a hub-ID lookup that never answers is bounded by the caller: the
// final send returns within its budget and stopping the loop does not wait
// for the lookup's own timeout.
func TestHealthHubLookupHonorsBudgets(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	clearUsageSourceEnv(t)
	setTestVersion(t)
	blocking := func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	lookup := cachedHubID(blocking)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if got := lookup(ctx); got != "" {
		t.Fatalf("lookup = %q", got)
	}
	cancel()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("lookup ignored the caller's deadline: %s", d)
	}

	p := NewWithConfig(&Config{Enabled: true})
	p.health.sendOverride = func(context.Context, []byte) error { return nil }
	p.health.hubIDOverride = lookup
	p.health.periodic, p.health.retry = time.Hour, 10*time.Millisecond
	p.startHealth(context.Background(), &GCPExporter{}, analyticsHandles{disabledReason: telemetrycontract.AnalyticsDisabledOutboxUnavailable})
	p.SettleUsageSource(nil, "") // start queued; the loop retries the lookup every 10 ms
	time.Sleep(30 * time.Millisecond)
	stopStart := time.Now()
	p.stopHealthLoop()
	if d := time.Since(stopStart); d > time.Second {
		t.Fatalf("stopHealthLoop waited for an in-flight lookup: %s", d)
	}
	// emitFinalHealth would stop the loop itself; restart state for it.
	p.health.mu.Lock()
	p.health.enabled = true
	p.health.mu.Unlock()
	finalStart := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	p.emitFinalHealth(stopCtx) // budget: a quarter of 1 s = 250 ms
	if d := time.Since(finalStart); d > 750*time.Millisecond {
		t.Fatalf("final send took %s, want about 250 ms", d)
	}
}

func TestFinalHealthBudget(t *testing.T) {
	now := time.Now()
	at := func(left time.Duration) context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(left))
		t.Cleanup(cancel)
		return ctx
	}
	for name, c := range map[string]struct {
		ctx  context.Context
		want time.Duration
	}{
		"no deadline":       {context.Background(), healthFinalBudget},
		"ample (20 s)":      {at(20 * time.Second), healthFinalBudget},
		"quarter (4 s)":     {at(4 * time.Second), time.Second},
		"nearly spent":      {at(100 * time.Millisecond), healthFinalMinBudget},
		"expired (overrun)": {at(-time.Second), healthFinalMinBudget},
	} {
		if got := finalHealthBudget(c.ctx, now); got != c.want {
			t.Errorf("%s: budget %s, want %s", name, got, c.want)
		}
	}
}

// R2: concurrent malformed metric batches and log records never produce a
// negative or torn per-unit counter in a snapshot, so a health event built
// meanwhile always validates.
func TestUsageMalformedCountersRaceFree(t *testing.T) {
	setTestVersion(t)
	d := bareUsageDeriver(stubLogRule{}, stubMetricRule{err: errors.New("bad point")})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			d.observeMetricBatch(context.Background(), d.rules[1].(metricBatchDeriver), &metricpb.ResourceMetrics{}, map[string]bool{})
			d.observe(context.Background(), stubUsageScope, namedRecord("usage.bad", uint64(i+1)))
		}
	}()
	go func() {
		defer wg.Done()
		<-time.After(100 * time.Millisecond)
		close(stop)
	}()
	for i := 0; i < 2000; i++ {
		diag := d.Diagnostics()
		in := fixedHealthInputs(telemetrycontract.HealthTriggerPeriodic)
		in.UsageDiag = &diag
		ev, err := buildHealthEvent(in)
		if err != nil {
			t.Fatal(err)
		}
		ev.HubID = "hub-1"
		if _, err := telemetrycontract.Marshal(ev); err != nil {
			t.Fatalf("snapshot %d invalid: %v", i, err)
		}
	}
	wg.Wait()
	diag := d.Diagnostics()
	if diag.LogRecordsMalformed+diag.MetricBatchesMalformed != diag.Malformed {
		t.Fatalf("quiescent: log %d + metric %d != malformed %d", diag.LogRecordsMalformed, diag.MetricBatchesMalformed, diag.Malformed)
	}
}

// N7: Stop end to end. With native usage active and a metric residual at
// stop, the final event (sent after the analytics drain, before the
// exporter shuts down) says enabled, carries the captured usage counters
// and final_residual_records > 0.
func TestPipelineStopEmitsFinalHealth(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	clearUsageSourceEnv(t)
	setTestVersion(t)
	o, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "ob"))
	if err != nil {
		t.Fatal(err)
	}
	client := &mockMetricClient{exportFunc: func(context.Context, *colmetricpb.ExportMetricsServiceRequest, ...grpc.CallOption) (*colmetricpb.ExportMetricsServiceResponse, error) {
		return &colmetricpb.ExportMetricsServiceResponse{}, nil
	}}
	p := &Pipeline{running: true, config: &Config{Enabled: true}, exporter: &CloudExporter{metricClient: client}, metricStreams: newMetricStreams(), metricLastFlush: time.Now()}
	var sent [][]byte
	p.health.sendOverride = func(_ context.Context, line []byte) error { sent = append(sent, line); return nil }
	p.health.hubIDOverride = func(context.Context) string { return "hub-1" }
	p.health.periodic, p.health.retry = time.Hour, time.Hour
	d := bareUsageDeriver(claudeUsageRule{})
	feed(d, claudeLogs(claudeRecord("req_011CUabcdefghijklmnopq", "1", 5, fullClaudeTokens)))
	p.usageDeriver.Store(d)
	p.startHealth(context.Background(), &GCPExporter{}, analyticsHandles{native: NewUsageAnalytics(o, telemetrycontract.UsageSourceNative)})
	p.SettleUsageSource(map[string]string{"SCION_USAGE_SOURCE": "native"}, "enabled")
	// A dirty stream that the tight stop deadline cannot export.
	if err := p.metricStreams.add([]*metricpb.ResourceMetrics{stopTestMetric(5, 2)}); err != nil {
		t.Fatal(err)
	}
	p.metricDirtyRecords, p.metricDirtyEntries = 1, 1
	p.budget.records, p.budget.entries = 1, 1
	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = p.Stop(stopCtx) // reports the residual as an error

	var final *telemetrycontract.HealthEvent
	for _, l := range sent {
		var ev telemetrycontract.HealthEvent
		if err := telemetrycontract.Unmarshal(l, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Trigger == "final" {
			final = &ev
		}
	}
	if final == nil {
		t.Fatalf("no final event among %d sent", len(sent))
	}
	if final.UsageCollection != "enabled" || final.UsageObservationsDerived == nil || *final.UsageObservationsDerived != 1 ||
		final.FinalResidualRecords == nil || *final.FinalResidualRecords < 1 || final.FinalFlushOutcome != "not_drained" {
		t.Fatalf("final: collection %q derived %v residual %v outcome %q", final.UsageCollection, final.UsageObservationsDerived, final.FinalResidualRecords, final.FinalFlushOutcome)
	}
}
