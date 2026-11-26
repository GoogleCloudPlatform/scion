/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gopkg.in/yaml.v3"
)

func loadMuseUsagePayloads(t *testing.T) []map[string]interface{} {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	f, err := os.Open(filepath.Join(root, "pkg", "sciontool", "hooks", "dialects", "testdata", "muse-code", "session-usage-1.4.25.jsonl"))
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

// TestMuseCodeHookTokensReachUsageMetric verifies that a real Muse Code 1.4.25
// PostLLMCall payload parsed through harnesses/muse-code/dialect.yaml converts
// inclusive input_tokens to canonical exclusive input, preserves output
// (inclusive of reasoning), cache_read and reasoning, forwards model and
// session_id, and records gen_ai.api.calls, scion.usage.tokens and agent.usage.
func TestMuseCodeHookTokensReachUsageMetric(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_HARNESS", "muse-code")
	t.Setenv("SCION_USAGE_SOURCE", "hooks")

	specPath := testutil.HarnessDialectPath(t, "muse-code")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}
	if md.Name() != "muse-code" {
		t.Fatalf("dialect name = %q, want muse-code", md.Name())
	}

	records := loadMuseUsagePayloads(t)
	event, err := md.Parse(records[0])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event.Name != "model-end" {
		t.Fatalf("event.Name = %q, want model-end", event.Name)
	}
	if event.Data.Model != "muse-spark" {
		t.Errorf("Model = %q, want muse-spark", event.Data.Model)
	}
	if event.Data.SessionID != "00000001-0000-4000-8000-000000000001" {
		t.Errorf("SessionID = %q, want 00000001-0000-4000-8000-000000000001", event.Data.SessionID)
	}
	// 1000 inclusive input - 250 cached = 750 exclusive input.
	if event.Data.InputTokens != 750 || !event.Data.InputTokensDerived {
		t.Errorf("InputTokens=%d derived=%v, want 750/true", event.Data.InputTokens, event.Data.InputTokensDerived)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	analytics, outboxDir := hookAnalytics(t)

	h := NewTelemetryHandler(nil, nil, nil, mp)
	h.Analytics = analytics
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	totals := usageTokenTotals(rm)
	if totals[telemetrycontract.TokenTypeInput] != 750 {
		t.Errorf("token_type=input = %d, want 750", totals[telemetrycontract.TokenTypeInput])
	}
	if totals[telemetrycontract.TokenTypeOutput] != 300 {
		t.Errorf("token_type=output = %d, want 300", totals[telemetrycontract.TokenTypeOutput])
	}
	if totals[telemetrycontract.TokenTypeCacheRead] != 250 {
		t.Errorf("token_type=cache_read = %d, want 250", totals[telemetrycontract.TokenTypeCacheRead])
	}
	if totals[telemetrycontract.TokenTypeReasoning] != 80 {
		t.Errorf("token_type=reasoning = %d, want 80", totals[telemetrycontract.TokenTypeReasoning])
	}
	if calls := apiCallTotal(rm); calls != 1 {
		t.Errorf("gen_ai.api.calls = %d, want 1", calls)
	}

	staged := hookStagedEvents(t, outboxDir)
	if len(staged) != 1 {
		t.Fatalf("staged agent.usage events = %d, want 1", len(staged))
	}
	ev := staged[0]
	if ev.Model != "muse-spark" || ev.Status != telemetrycontract.StatusSuccess || ev.Calls != 1 {
		t.Errorf("staged event header = %+v", ev)
	}
	if ev.Input == nil || *ev.Input != 750 || ev.InputDerivation != telemetrycontract.InputDerivationDerived {
		t.Errorf("staged Input=%v derivation=%q, want 750/derived", ev.Input, ev.InputDerivation)
	}
	if ev.Output == nil || *ev.Output != 300 || ev.CacheRead == nil || *ev.CacheRead != 250 || ev.Reasoning == nil || *ev.Reasoning != 80 {
		t.Errorf("staged tokens = %+v", ev)
	}
	if ev.CacheWrite != nil {
		t.Errorf("staged CacheWrite = %v, want nil (measured 0 dropped by hook path)", *ev.CacheWrite)
	}
	if ev.TokenStatus != telemetrycontract.TokenStatusPartial {
		t.Errorf("TokenStatus = %q, want partial", ev.TokenStatus)
	}
	if ev.SourceIdentity != telemetrycontract.SourceIdentityComposite || ev.IdentityAmbiguous || ev.SourceEventID == "" {
		t.Errorf("identity = %q ambiguous=%v id=%q", ev.SourceIdentity, ev.IdentityAmbiguous, ev.SourceEventID)
	}
	if ev.SessionID != "00000001-0000-4000-8000-000000000001" {
		t.Errorf("staged SessionID = %q, want 00000001-0000-4000-8000-000000000001", ev.SessionID)
	}
}

// TestMuseDialect_CumulativeTurnDeltaAndFailedStep verifies Muse Code 1.4.25's
// turn-cumulative PostLLMCall.usage across:
//   - a 2-step tool loop (step 0 + step 1 cumulative -> exact per-step deltas),
//   - a 2-step turn where step 1 fails with HTTP 400 and repeats step 0's
//     cumulative usage (step 1 produces 1 error call with nil tokens),
//   - a separate subagent turn (independent watermark),
//   - cross-process persistence via MuseTurnUsageStatePath (new TelemetryHandler
//     per event, mirroring separate `sciontool hook` subprocesses).
func TestMuseDialect_CumulativeTurnDeltaAndFailedStep(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_HARNESS", "muse-code")
	t.Setenv("SCION_USAGE_SOURCE", "hooks")

	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "muse-code"))
	if err != nil {
		t.Fatalf("LoadMappingDialect: %v", err)
	}
	records := loadMuseUsagePayloads(t)
	if len(records) != 6 {
		t.Fatalf("fixture has %d records, want 6", len(records))
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	analytics, outboxDir := hookAnalytics(t)
	statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")

	// Replay records 1..5, creating a fresh TelemetryHandler per event to
	// exercise cross-process TurnUsageStatePath persistence.
	for _, raw := range records[1:] {
		ev, err := md.Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		h := NewTelemetryHandler(nil, nil, nil, mp)
		h.Analytics = analytics
		h.MuseTurnUsageStatePath = statePath
		if err := h.Handle(ev); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Expected per-step canonical totals across records[1..5]:
	//   rec 1 (turn 2, step 0): in=600, out=120, cr=200, reas=40  (ok)
	//   rec 2 (turn 2, step 1): in=600, out=150, cr=600, reas=30  (ok, delta of 1200/270/800/70)
	//   rec 3 (turn 3, step 0): in=600, out=120, cr=200, reas=40  (ok)
	//   rec 4 (turn 3, step 1): in=0,   out=0,   cr=0,   reas=0   (error, repeated cumulative)
	//   rec 5 (turn 4, step 0): in=450, out=60,  cr=50,  reas=0   (ok, subagent)
	// Totals: calls=5, in=2250, out=450, cr=1050, reas=110.
	if calls := apiCallTotal(rm); calls != 5 {
		t.Errorf("gen_ai.api.calls = %d, want 5", calls)
	}
	totals := usageTokenTotals(rm)
	if totals[telemetrycontract.TokenTypeInput] != 2250 {
		t.Errorf("input = %d, want 2250", totals[telemetrycontract.TokenTypeInput])
	}
	if totals[telemetrycontract.TokenTypeOutput] != 450 {
		t.Errorf("output = %d, want 450", totals[telemetrycontract.TokenTypeOutput])
	}
	if totals[telemetrycontract.TokenTypeCacheRead] != 1050 {
		t.Errorf("cache_read = %d, want 1050", totals[telemetrycontract.TokenTypeCacheRead])
	}
	if totals[telemetrycontract.TokenTypeReasoning] != 110 {
		t.Errorf("reasoning = %d, want 110", totals[telemetrycontract.TokenTypeReasoning])
	}

	staged := hookStagedEvents(t, outboxDir)
	if len(staged) != 5 {
		t.Fatalf("staged events = %d, want 5", len(staged))
	}

	// Step 1 of turn 2 must carry only the delta (600/150/600/30), not cumulative (1200/270/800/70).
	step1 := staged[1]
	if step1.Input == nil || *step1.Input != 600 || step1.Output == nil || *step1.Output != 150 || step1.CacheRead == nil || *step1.CacheRead != 600 || step1.Reasoning == nil || *step1.Reasoning != 30 {
		t.Errorf("turn 2 step 1 staged = %+v, want delta 600/150/600/30", step1)
	}
	if step1.SourceEventID == staged[0].SourceEventID {
		t.Errorf("turn 2 step 0 and step 1 shared SourceEventID %q", step1.SourceEventID)
	}

	// Failed step 1 of turn 3 must be status=error with all token fields nil.
	failedStep := staged[3]
	if failedStep.Status != telemetrycontract.StatusError || failedStep.TokenStatus != telemetrycontract.TokenStatusNotReported {
		t.Errorf("failed step status=%q token_status=%q, want error/not_reported", failedStep.Status, failedStep.TokenStatus)
	}
	if failedStep.Input != nil || failedStep.Output != nil || failedStep.CacheRead != nil || failedStep.CacheWrite != nil || failedStep.Reasoning != nil {
		t.Errorf("failed step must have nil tokens, got %+v", failedStep)
	}
	if failedStep.SourceIdentity != telemetrycontract.SourceIdentityComposite || failedStep.SourceEventID == "" || failedStep.SourceEventID == staged[2].SourceEventID {
		t.Errorf("failed step identity=%q id=%q (step0 id=%q)", failedStep.SourceIdentity, failedStep.SourceEventID, staged[2].SourceEventID)
	}
}

// TestMuseDialect_SuppressesUsageWhenSourceUnset pins D10: when
// SCION_USAGE_SOURCE is unset, muse-code model-end events publish zero usage
// counters and zero outbox events.
func TestMuseDialect_SuppressesUsageWhenSourceUnset(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_HARNESS", "muse-code")
	t.Setenv("SCION_USAGE_SOURCE", "")

	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "muse-code"))
	if err != nil {
		t.Fatalf("LoadMappingDialect: %v", err)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	analytics, outboxDir := hookAnalytics(t)
	h := NewTelemetryHandler(nil, nil, nil, mp)
	h.Analytics = analytics

	for _, raw := range loadMuseUsagePayloads(t) {
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

// TestMuseDialect_PostLLMCallMappingConfig pins the PostLLMCall mapping:
// input_tokens_inclusive must be true, and raw token keys must not be
// duplicated in fields: (extractTokens reads nested usage.* directly).
func TestMuseDialect_PostLLMCallMappingConfig(t *testing.T) {
	data, err := os.ReadFile(testutil.HarnessDialectPath(t, "muse-code"))
	if err != nil {
		t.Fatal(err)
	}
	var spec dialects.MappingDialectSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	entry, ok := spec.Mappings["PostLLMCall"]
	if !ok {
		t.Fatal(`dialect.yaml has no "PostLLMCall" mapping`)
	}
	if !entry.InputTokensInclusive {
		t.Error("PostLLMCall mapping must set input_tokens_inclusive: true")
	}
	for _, forbidden := range []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens"} {
		if _, present := entry.Fields[forbidden]; present {
			t.Errorf("PostLLMCall mapping.fields contains %q; extractTokens reads nested usage.* directly", forbidden)
		}
	}
}

// TestMuseTurnUsage_LRUEvictionInterleaved verifies R1 and the widened 1024-slot
// LRU bound:
//   - when more than maxMuseTrackedTurns (1024) subagent turns interleave with
//     an active main turn, LRU eviction refreshes the main turn on every step so
//     its high-water mark is never dropped and summed deltas equal the true
//     cumulative;
//   - when 100 distinct other turns run strictly between two consecutive steps
//     of one turn (with no touch to that turn in between), the second step's
//     delta still equals the exact per-step increment through both apply() and
//     the file path.
func TestMuseTurnUsage_LRUEvictionInterleaved(t *testing.T) {
	const (
		mainKey    = "sess-main:turn-main"
		stepInc    = int64(1000)
		iterations = maxMuseTrackedTurns + 6 // triggers eviction cycles
		wantTotal  = stepInc * int64(1+iterations)
	)

	t.Run("in-memory apply across eviction", func(t *testing.T) {
		var st museTurnUsageState
		cum := stepInc
		sum := st.apply(mainKey, museTurnUsageTotals{Input: cum}).Input
		if sum != stepInc {
			t.Fatalf("step 0 delta = %d, want %d", sum, stepInc)
		}
		for i := 1; i <= iterations; i++ {
			subKey := fmt.Sprintf("sess-sub-%d:turn-sub-%d", i, i)
			st.apply(subKey, museTurnUsageTotals{Input: 500})
			cum += stepInc
			d := st.apply(mainKey, museTurnUsageTotals{Input: cum})
			if d.Input != stepInc {
				t.Fatalf("iter %d: main delta = %d, want %d (LRU must retain active main turn)", i, d.Input, stepInc)
			}
			sum += d.Input
		}
		if sum != wantTotal {
			t.Errorf("summed main deltas = %d, want %d", sum, wantTotal)
		}
		if len(st.Turns) != maxMuseTrackedTurns || len(st.Order) != maxMuseTrackedTurns {
			t.Errorf("state size turns=%d order=%d, want %d", len(st.Turns), len(st.Order), maxMuseTrackedTurns)
		}
	})

	t.Run("100 turns strictly between two consecutive steps", func(t *testing.T) {
		const between = 100

		// Direct apply() check.
		var st museTurnUsageState
		if d0 := st.apply(mainKey, museTurnUsageTotals{Input: stepInc}); d0.Input != stepInc {
			t.Fatalf("apply step 0 = %d, want %d", d0.Input, stepInc)
		}
		for i := 1; i <= between; i++ {
			st.apply(fmt.Sprintf("sess-sub-%d:turn-sub-%d", i, i), museTurnUsageTotals{Input: 500})
		}
		if d1 := st.apply(mainKey, museTurnUsageTotals{Input: 2 * stepInc}); d1.Input != stepInc {
			t.Fatalf("apply step 1 after %d turns = %d, want %d", between, d1.Input, stepInc)
		}

		// File-path check across separate TelemetryHandlers.
		scrubTelemetryEnv(t)
		t.Setenv("SCION_HARNESS", "muse-code")
		t.Setenv("SCION_USAGE_SOURCE", "hooks")

		md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "muse-code"))
		if err != nil {
			t.Fatalf("LoadMappingDialect: %v", err)
		}
		statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")

		runStep := func(sessionID, turnID string, step int, rawInput int64) int64 {
			ev, err := md.Parse(map[string]interface{}{
				"hook_event_name": "PostLLMCall",
				"session_id":      sessionID,
				"turn_id":         turnID,
				"step":            float64(step),
				"request_id":      fmt.Sprintf("%s:%d:1", turnID, step),
				"response_id":     fmt.Sprintf("resp_%s_%d", turnID, step),
				"model":           "muse-spark",
				"status":          "success",
				"usage": map[string]interface{}{
					"input_tokens":  float64(rawInput),
					"output_tokens": float64(10),
					"cached_tokens": float64(0),
				},
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()
			h := NewTelemetryHandler(nil, nil, nil, mp)
			h.MuseTurnUsageStatePath = statePath
			if err := h.Handle(ev); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			return usageTokenTotals(rm)[telemetrycontract.TokenTypeInput]
		}

		if d0 := runStep("sess-main", "turn-main", 0, stepInc); d0 != stepInc {
			t.Fatalf("file-path step 0 = %d, want %d", d0, stepInc)
		}
		for i := 1; i <= between; i++ {
			runStep(fmt.Sprintf("sess-sub-%d", i), fmt.Sprintf("turn-sub-%d", i), 0, 500)
		}
		if d1 := runStep("sess-main", "turn-main", 1, 2*stepInc); d1 != stepInc {
			t.Fatalf("file-path step 1 after %d turns = %d, want %d", between, d1, stepInc)
		}
	})
}

// TestMuseTurnUsage_UnwritableStateFailsClosed verifies O1: when the state file
// cannot be used (unwritable path or required-but-empty path), turn-cumulative
// totals are never published as a step delta; the call is still counted with
// token_status=not_reported and all token fields nil.
func TestMuseTurnUsage_UnwritableStateFailsClosed(t *testing.T) {
	scrubTelemetryEnv(t)
	t.Setenv("SCION_HARNESS", "muse-code")
	t.Setenv("SCION_USAGE_SOURCE", "hooks")

	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "muse-code"))
	if err != nil {
		t.Fatalf("LoadMappingDialect: %v", err)
	}
	records := loadMuseUsagePayloads(t)

	// Blocking file at parent dir makes MkdirAll/OpenFile fail regardless of uid.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unwritablePath := filepath.Join(blocker, "muse-turn-usage.json")

	cases := []struct {
		name     string
		path     string
		required bool
	}{
		{name: "unwritable state path", path: unwritablePath, required: true},
		{name: "required but empty state path", path: "", required: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()
			analytics, outboxDir := hookAnalytics(t)

			// Replay 2-step turn (records[1] step 0, records[2] step 1 cumulative).
			for _, raw := range records[1:3] {
				ev, err := md.Parse(raw)
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				h := NewTelemetryHandler(nil, nil, nil, mp)
				h.Analytics = analytics
				h.MuseTurnUsageStatePath = tc.path
				h.MuseTurnUsageStateRequired = tc.required
				if err := h.Handle(ev); err != nil {
					t.Fatalf("Handle: %v", err)
				}
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if calls := apiCallTotal(rm); calls != 2 {
				t.Errorf("gen_ai.api.calls = %d, want 2", calls)
			}
			if totals := usageTokenTotals(rm); len(totals) != 0 {
				t.Errorf("usage tokens = %v, want empty (must not publish untracked cumulative totals)", totals)
			}
			staged := hookStagedEvents(t, outboxDir)
			if len(staged) != 2 {
				t.Fatalf("staged events = %d, want 2", len(staged))
			}
			for i, ev := range staged {
				if ev.TokenStatus != telemetrycontract.TokenStatusNotReported {
					t.Errorf("staged[%d].TokenStatus = %q, want not_reported", i, ev.TokenStatus)
				}
				if ev.Input != nil || ev.Output != nil || ev.CacheRead != nil || ev.CacheWrite != nil || ev.Reasoning != nil {
					t.Errorf("staged[%d] must have nil tokens, got %+v", i, ev)
				}
			}
		})
	}
}

// TestMuseTurnUsage_CorruptStateFileRecovers verifies O2: a corrupt state file
// resets cleanly and is replaced atomically via temp file + rename.
func TestMuseTurnUsage_CorruptStateFileRecovers(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{corrupt json"), 0o600); err != nil {
		t.Fatal(err)
	}
	delta, err := applyMuseTurnUsageFile(statePath, "s:t", museTurnUsageTotals{Input: 400, Output: 50})
	if err != nil {
		t.Fatalf("applyMuseTurnUsageFile on corrupt file: %v", err)
	}
	if delta.Input != 400 || delta.Output != 50 {
		t.Errorf("delta = %+v, want input=400 output=50", delta)
	}
	// Verify state file is now valid JSON and persists for step 1.
	delta2, err := applyMuseTurnUsageFile(statePath, "s:t", museTurnUsageTotals{Input: 900, Output: 120})
	if err != nil {
		t.Fatalf("step 1 applyMuseTurnUsageFile: %v", err)
	}
	if delta2.Input != 500 || delta2.Output != 70 {
		t.Errorf("step 1 delta = %+v, want input=500 output=70", delta2)
	}
}
