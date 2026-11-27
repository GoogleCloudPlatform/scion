/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gopkg.in/yaml.v3"
)

// loadPiBridgePayloads reads
// pkg/sciontool/hooks/dialects/testdata/pi/bridge-payloads.jsonl: the exact
// stdin harnesses/pi's scion-bridge.js writes to `sciontool hook
// --dialect=pi` for a scripted session. harnesses/pi/scion-bridge.test.mjs
// asserts the bridge still produces this file byte for byte, so these are
// the bridge's real payload shapes (a tool_input object, a boolean success,
// an error string). The pi events fed to the bridge are synthetic, not a
// live pi capture.
func loadPiBridgePayloads(t *testing.T) []map[string]interface{} {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	f, err := os.Open(filepath.Join(root, "pkg", "sciontool", "hooks", "dialects", "testdata", "pi", "bridge-payloads.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var records []map[string]interface{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("fixture line %d: %v", len(records)+1, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return records
}

// TestPiDialect_BridgePayloads feeds the bridge's payloads through the real
// harnesses/pi/dialect.yaml and pins the normalized event, the fields the
// handlers read, readiness (and the in-place classification of a
// session-start) and the resulting status activity.
func TestPiDialect_BridgePayloads(t *testing.T) {
	specPath := testutil.HarnessDialectPath(t, "pi")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}
	if md.Name() != "pi" {
		t.Fatalf("dialect name = %q, want pi", md.Name())
	}

	type want struct {
		name      string
		toolName  string
		toolInput map[string]interface{} // nil: ToolInput must be empty
		success   bool
		// failed: EventData.Failed, set by the mapping from success
		// (false -> true); only record 5 reports a failed tool call.
		failed bool
		errMsg string
		source string
		// inPlace: a session-start that restarts the session inside the
		// running harness (hooks.SessionInPlace): not a readiness event.
		inPlace  bool
		activity state.Activity
	}
	wants := []want{
		{name: hooks.EventSessionStart, source: "startup", activity: state.ActivityWorking},
		{name: hooks.EventPromptSubmit, activity: state.ActivityThinking},
		{name: hooks.EventModelEnd, activity: state.ActivityWorking},
		{name: hooks.EventToolStart, toolName: "bash", toolInput: map[string]interface{}{"command": "ls -la", "timeout": float64(30)}, activity: state.ActivityExecuting},
		{name: hooks.EventToolEnd, toolName: "bash", success: true, activity: state.ActivityThinking},
		{name: hooks.EventToolStart, toolName: "read", toolInput: map[string]interface{}{"path": "missing.txt"}, activity: state.ActivityExecuting},
		{name: hooks.EventToolEnd, toolName: "read", success: false, failed: true, activity: state.ActivityThinking},
		{name: hooks.EventAgentEnd, activity: state.ActivityWorking},
		{name: hooks.EventPromptSubmit, activity: state.ActivityThinking},
		{name: hooks.EventModelEnd, errMsg: "429 rate limited: retry later", activity: state.ActivityWorking},
		{name: hooks.EventAgentEnd, errMsg: "429 rate limited: retry later", activity: state.ActivityWorking},
		{name: hooks.EventSessionStart, source: "resume", activity: state.ActivityWorking},
		// pi's /reload restarts the session in place: no readiness
		// effect, and the status handler releases only a
		// notification-set wait (an agent-set status is kept). The
		// activity is what eventToPhaseActivity maps a session-start to;
		// the handler routes an in-place start around it.
		{name: hooks.EventSessionStart, source: "reload", inPlace: true, activity: state.ActivityWorking},
	}

	records := loadPiBridgePayloads(t)
	if len(records) != len(wants) {
		t.Fatalf("fixture has %d records, want %d", len(records), len(wants))
	}
	for i, rec := range records {
		w := wants[i]
		ev, err := md.Parse(rec)
		if err != nil {
			t.Fatalf("record %d: Parse: %v", i, err)
		}
		if ev.Name != w.name {
			t.Errorf("record %d (%v): Name = %q, want %q", i, rec["hook_event_name"], ev.Name, w.name)
		}
		if ev.Data.ToolName != w.toolName {
			t.Errorf("record %d: ToolName = %q, want %q", i, ev.Data.ToolName, w.toolName)
		}
		if w.toolInput == nil {
			if ev.Data.ToolInput != "" {
				t.Errorf("record %d: ToolInput = %q, want empty", i, ev.Data.ToolInput)
			}
		} else {
			// The bridge sends tool_input as a JSON object; the dialect
			// must carry it through as JSON text, not drop it.
			var got map[string]interface{}
			if err := json.Unmarshal([]byte(ev.Data.ToolInput), &got); err != nil {
				t.Errorf("record %d: ToolInput %q is not JSON: %v", i, ev.Data.ToolInput, err)
			} else if !jsonEqual(got, w.toolInput) {
				t.Errorf("record %d: ToolInput = %v, want %v", i, got, w.toolInput)
			}
		}
		if ev.Data.Success != w.success {
			t.Errorf("record %d: Success = %v, want %v", i, ev.Data.Success, w.success)
		}
		if ev.Data.Failed != w.failed {
			t.Errorf("record %d: Failed = %v, want %v", i, ev.Data.Failed, w.failed)
		}
		if ev.Data.Source != w.source {
			t.Errorf("record %d: Source = %q, want %q", i, ev.Data.Source, w.source)
		}
		if ev.Data.Error != w.errMsg {
			t.Errorf("record %d: Error = %q, want %q", i, ev.Data.Error, w.errMsg)
		}
		if ev.Name == hooks.EventSessionStart {
			if got := hooks.ClassifySessionStart(ev.Data.Source) == hooks.SessionInPlace; got != w.inPlace {
				t.Errorf("record %d: source %q in place = %v, want %v", i, ev.Data.Source, got, w.inPlace)
			}
		}
		if got := hooks.IsReadinessEvent(ev); got != !w.inPlace {
			t.Errorf("record %d: %s readiness event = %v, want %v", i, ev.Name, got, !w.inPlace)
		}
		res := eventToPhaseActivity(ev)
		if w.activity == "" {
			if res != nil {
				t.Errorf("record %d: %s mapped to activity %q, want no status change", i, ev.Name, res.activity)
			}
		} else if res == nil {
			t.Errorf("record %d: %s maps to no status change", i, ev.Name)
		} else if res.activity != w.activity {
			t.Errorf("record %d: activity = %q, want %q", i, res.activity, w.activity)
		}
	}
}

// TestPiDialect_FixtureCoversEveryMapping fails when dialect.yaml gains a
// mapping the bridge fixture never exercises, or the fixture carries an
// event dialect.yaml does not map.
func TestPiDialect_FixtureCoversEveryMapping(t *testing.T) {
	raw, err := os.ReadFile(testutil.HarnessDialectPath(t, "pi"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Mappings map[string]interface{} `yaml:"mappings"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, rec := range loadPiBridgePayloads(t) {
		name, _ := rec["hook_event_name"].(string)
		if _, ok := spec.Mappings[name]; !ok {
			t.Errorf("record %d: event %q is not mapped in dialect.yaml", i, name)
		}
		seen[name] = true
	}
	for name := range spec.Mappings {
		if !seen[name] {
			t.Errorf("dialect.yaml maps %q but the bridge fixture never emits it", name)
		}
	}
}

func jsonEqual(a, b interface{}) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// TestPiDialect_ModelEndCanonicalUsageAndAnalytics drives pi-shaped
// bridge payloads (from bridge-payloads.jsonl) through harnesses/pi/dialect.yaml
// and TelemetryHandler, asserting canonical OTel counters (gen_ai.api.calls,
// scion.usage.tokens) and outbox agent.usage events (composite identity,
// exclusive input, output inclusive of reasoning, absent unknown fields,
// error outcome, and payload model precedence over SCION_MODEL).
func TestPiDialect_ModelEndCanonicalUsageAndAnalytics(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "pi")
	t.Setenv("SCION_MODEL", "configured-model-should-lose")

	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "pi"))
	if err != nil {
		t.Fatalf("LoadMappingDialect: %v", err)
	}
	records := loadPiBridgePayloads(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	analytics, outboxDir := hookAnalytics(t)
	h := NewTelemetryHandler(nil, nil, nil, mp)
	h.Analytics = analytics

	for i, raw := range records {
		ev, err := md.Parse(raw)
		if err != nil {
			t.Fatalf("record %d: Parse: %v", i, err)
		}
		if err := h.Handle(ev); err != nil {
			t.Fatalf("record %d: Handle: %v", i, err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if calls := apiCallTotal(rm); calls != 2 {
		t.Errorf("gen_ai.api.calls total = %d, want 2 (1 success + 1 error)", calls)
	}

	totals := usageTokenTotals(rm)
	if got := totals[telemetrycontract.TokenTypeInput]; got != 380 {
		t.Errorf("input total = %d, want 380", got)
	}
	if got := totals[telemetrycontract.TokenTypeOutput]; got != 40 {
		t.Errorf("output total = %d, want 40 (already inclusive of reasoning)", got)
	}
	if got := totals[telemetrycontract.TokenTypeCacheRead]; got != 120 {
		t.Errorf("cache_read total = %d, want 120", got)
	}
	if got := totals[telemetrycontract.TokenTypeCacheWrite]; got != 25 {
		t.Errorf("cache_write total = %d, want 25", got)
	}
	if got := totals[telemetrycontract.TokenTypeReasoning]; got != 15 {
		t.Errorf("reasoning total = %d, want 15", got)
	}

	for _, p := range int64CounterDataPoints(rm, telemetrycontract.MetricUsageTokens) {
		model, _ := p.Attributes.Value(attribute.Key(telemetrycontract.ModelLabel))
		if model.AsString() != "mockprov/mock-model-2026" {
			t.Errorf("usage token model label = %q, want mockprov/mock-model-2026", model.AsString())
		}
		harness, _ := p.Attributes.Value(attribute.Key(telemetrycontract.HarnessLabel))
		if harness.AsString() != "pi" {
			t.Errorf("usage token harness label = %q, want pi", harness.AsString())
		}
	}

	staged := hookStagedEvents(t, outboxDir)
	if len(staged) != 2 {
		t.Fatalf("staged agent.usage events = %d, want 2", len(staged))
	}
	okEv, errEv := staged[0], staged[1]
	if okEv.SourceKind != "hook.pi.model_end" || okEv.UsageSource != "hooks" || okEv.Harness != "pi" {
		t.Errorf("ok event source/harness = %q %q %q", okEv.SourceKind, okEv.UsageSource, okEv.Harness)
	}
	if okEv.Calls != 1 || okEv.Status != telemetrycontract.StatusSuccess || okEv.TokenStatus != telemetrycontract.TokenStatusReported || okEv.Model != "mockprov/mock-model-2026" || okEv.ModelSource != "event" {
		t.Errorf("ok event = %+v", okEv)
	}
	if okEv.Input == nil || *okEv.Input != 380 || okEv.Output == nil || *okEv.Output != 40 ||
		okEv.CacheRead == nil || *okEv.CacheRead != 120 || okEv.CacheWrite == nil || *okEv.CacheWrite != 25 ||
		okEv.Reasoning == nil || *okEv.Reasoning != 15 {
		t.Errorf("ok event tokens = %+v", okEv)
	}
	if okEv.ReasoningOverlapsOutput == nil || !*okEv.ReasoningOverlapsOutput || okEv.InputDerivation != telemetrycontract.InputDerivationReported {
		t.Errorf("ok event semantics: overlap=%v deriv=%q", okEv.ReasoningOverlapsOutput, okEv.InputDerivation)
	}
	if okEv.SourceIdentity != telemetrycontract.SourceIdentityComposite || okEv.IdentityAmbiguous || okEv.SourceEventID == "" {
		t.Errorf("ok event identity = %q ambiguous=%v id=%q", okEv.SourceIdentity, okEv.IdentityAmbiguous, okEv.SourceEventID)
	}

	if errEv.Calls != 1 || errEv.Status != telemetrycontract.StatusError || errEv.TokenStatus != telemetrycontract.TokenStatusNotReported {
		t.Errorf("error event = %+v", errEv)
	}
	if errEv.Input != nil || errEv.Output != nil || errEv.CacheRead != nil || errEv.CacheWrite != nil || errEv.Reasoning != nil {
		t.Errorf("error event with EMPTY_USAGE must leave all token fields nil, got %+v", errEv)
	}
	if errEv.SourceIdentity != telemetrycontract.SourceIdentityComposite || errEv.IdentityAmbiguous || errEv.SourceEventID == "" || errEv.SourceEventID == okEv.SourceEventID {
		t.Errorf("error event identity = %q ambiguous=%v id=%q (ok id=%q)", errEv.SourceIdentity, errEv.IdentityAmbiguous, errEv.SourceEventID, okEv.SourceEventID)
	}
}

// TestPiDialect_SuppressesUsageWhenSourceUnset pins D10: when SCION_USAGE_SOURCE
// is unset, pi model-end events publish zero usage counters and zero outbox
// events while tool metrics remain unaffected.
func TestPiDialect_SuppressesUsageWhenSourceUnset(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_HARNESS", "pi")
	t.Setenv("SCION_USAGE_SOURCE", "")

	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "pi"))
	if err != nil {
		t.Fatalf("LoadMappingDialect: %v", err)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	analytics, outboxDir := hookAnalytics(t)
	h := NewTelemetryHandler(nil, nil, nil, mp)
	h.Analytics = analytics

	for _, raw := range loadPiBridgePayloads(t) {
		ev, err := md.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Handle(ev); err != nil {
			t.Fatal(err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if calls := apiCallTotal(rm); calls != 0 {
		t.Errorf("gen_ai.api.calls = %d, want 0 when SCION_USAGE_SOURCE is unset", calls)
	}
	if totals := usageTokenTotals(rm); len(totals) != 0 {
		t.Errorf("usage tokens = %v, want empty when SCION_USAGE_SOURCE is unset", totals)
	}
	if staged := hookStagedEvents(t, outboxDir); len(staged) != 0 {
		t.Errorf("staged events = %d, want 0 when SCION_USAGE_SOURCE is unset", len(staged))
	}
}

// TestPiDialect_MessageEndMappingOmitsExtractTokensFields guards against the
// extractTokens override pitfall (dialects/common.go): input_tokens,
// output_tokens and cached_tokens are read from the top-level payload by
// extractTokens after `fields:`, so dialect.yaml must not duplicate them.
func TestPiDialect_MessageEndMappingOmitsExtractTokensFields(t *testing.T) {
	data, err := os.ReadFile(testutil.HarnessDialectPath(t, "pi"))
	if err != nil {
		t.Fatal(err)
	}
	var spec dialects.MappingDialectSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	entry, ok := spec.Mappings["message_end"]
	if !ok {
		t.Fatal(`dialect.yaml has no "message_end" mapping`)
	}
	for _, forbidden := range []string{"input_tokens", "output_tokens", "cached_tokens"} {
		if _, present := entry.Fields[forbidden]; present {
			t.Errorf("message_end mapping.fields contains %q; extractTokens reads this top-level key directly", forbidden)
		}
	}
}

func TestPiDialect_MeasuredZerosReported(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_USAGE_SOURCE", "hooks")
	t.Setenv("SCION_HARNESS", "pi")

	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "pi"))
	if err != nil {
		t.Fatalf("LoadMappingDialect: %v", err)
	}
	analytics, outboxDir := hookAnalytics(t)
	h := NewTelemetryHandler(nil, nil, nil)
	h.Analytics = analytics

	// All 5 fields present with measured zeros -> reported.
	ev1, err := md.Parse(map[string]interface{}{
		"hook_event_name":    "message_end",
		"response_id":        "resp-zero-1",
		"timestamp":          float64(1795719214000),
		"model":              "mockprov/mock-model",
		"input_tokens":       float64(200),
		"output_tokens":      float64(20),
		"cached_tokens":      float64(0),
		"cache_write_tokens": float64(0),
		"reasoning_tokens":   float64(0),
	})
	if err != nil {
		t.Fatalf("Parse ev1: %v", err)
	}
	if err := h.Handle(ev1); err != nil {
		t.Fatalf("Handle ev1: %v", err)
	}

	// Only 4 of 5 fields present (reasoning omitted) -> partial.
	ev2, err := md.Parse(map[string]interface{}{
		"hook_event_name":    "message_end",
		"response_id":        "resp-zero-2",
		"timestamp":          float64(1795719215000),
		"model":              "mockprov/mock-model",
		"input_tokens":       float64(200),
		"output_tokens":      float64(20),
		"cached_tokens":      float64(0),
		"cache_write_tokens": float64(0),
	})
	if err != nil {
		t.Fatalf("Parse ev2: %v", err)
	}
	if err := h.Handle(ev2); err != nil {
		t.Fatalf("Handle ev2: %v", err)
	}

	staged := hookStagedEvents(t, outboxDir)
	if len(staged) != 2 {
		t.Fatalf("staged events = %d, want 2", len(staged))
	}
	if staged[0].TokenStatus != telemetrycontract.TokenStatusReported {
		t.Errorf("staged[0].TokenStatus = %q, want reported", staged[0].TokenStatus)
	}
	if staged[0].CacheRead == nil || *staged[0].CacheRead != 0 ||
		staged[0].CacheWrite == nil || *staged[0].CacheWrite != 0 ||
		staged[0].Reasoning == nil || *staged[0].Reasoning != 0 {
		t.Errorf("staged[0] measured zeros = %+v", staged[0])
	}
	if staged[1].TokenStatus != telemetrycontract.TokenStatusPartial {
		t.Errorf("staged[1].TokenStatus = %q, want partial", staged[1].TokenStatus)
	}
	if staged[1].CacheRead == nil || *staged[1].CacheRead != 0 || staged[1].CacheWrite == nil || *staged[1].CacheWrite != 0 || staged[1].Reasoning != nil {
		t.Errorf("staged[1] tokens = %+v, want cr=0 cw=0 reas=nil", staged[1])
	}
}
