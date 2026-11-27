/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
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
	if ev.CacheWrite == nil || *ev.CacheWrite != 0 {
		t.Errorf("staged CacheWrite = %v, want 0 (measured zero preserved)", ev.CacheWrite)
	}
	if ev.TokenStatus != telemetrycontract.TokenStatusReported {
		t.Errorf("TokenStatus = %q, want reported", ev.TokenStatus)
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

	// Step 1 of turn 2 must carry only the delta (600/150/600/0/30), not cumulative (1200/270/800/0/70).
	step1 := staged[1]
	if step1.Input == nil || *step1.Input != 600 || step1.Output == nil || *step1.Output != 150 ||
		step1.CacheRead == nil || *step1.CacheRead != 600 || step1.CacheWrite == nil || *step1.CacheWrite != 0 ||
		step1.Reasoning == nil || *step1.Reasoning != 30 {
		t.Errorf("turn 2 step 1 staged = %+v, want delta 600/150/600/0/30", step1)
	}
	if step1.TokenStatus != telemetrycontract.TokenStatusReported {
		t.Errorf("turn 2 step 1 TokenStatus = %q, want reported", step1.TokenStatus)
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

	// Record 5 (subagent turn 4, step 0) has measured zeros for cache_write and reasoning -> reported.
	subagentStep := staged[4]
	if subagentStep.TokenStatus != telemetrycontract.TokenStatusReported {
		t.Errorf("subagent step TokenStatus = %q, want reported", subagentStep.TokenStatus)
	}
	if subagentStep.CacheWrite == nil || *subagentStep.CacheWrite != 0 || subagentStep.Reasoning == nil || *subagentStep.Reasoning != 0 {
		t.Errorf("subagent step measured zeros = cw:%v reas:%v, want 0/0", subagentStep.CacheWrite, subagentStep.Reasoning)
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

func TestMuseCodeVerifiedVersionsMatchesFixture(t *testing.T) {
	if len(MuseCodeVerifiedVersions) == 0 {
		t.Fatal("MuseCodeVerifiedVersions must not be empty")
	}
	if !slices.Contains(MuseCodeVerifiedVersions, "1.4.25") {
		t.Fatalf("MuseCodeVerifiedVersions %v does not include fixture version 1.4.25", MuseCodeVerifiedVersions)
	}
}

type museCapturedSlogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type museCaptureSlogHandler struct {
	mu      sync.Mutex
	records []museCapturedSlogRecord
}

func (h *museCaptureSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *museCaptureSlogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, museCapturedSlogRecord{level: r.Level, msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}
func (h *museCaptureSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *museCaptureSlogHandler) WithGroup(string) slog.Handler      { return h }

func (h *museCaptureSlogHandler) warnings() []museCapturedSlogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []museCapturedSlogRecord
	for _, r := range h.records {
		if r.level == slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

func TestNormalizeMuseVersion(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"Muse Code 1.4.25 (1.4.25-R9361.1)\n", "1.4.25"},
		{"1.4.25-R9361.1", "1.4.25"},
		{"v1.4.25", "1.4.25"},
		{"  v1.4.25  \t\n", "1.4.25"},
		{"Muse Code v1.5.0", "1.5.0"},
		{"Muse Code 1.5.0 (1.5.0-R9999.1)", "1.5.0"},
		{"1.4.25-rc", "1.4.25-rc"},
		{"1.4.25-rc.1", "1.4.25-rc.1"},
		{"1.4.25.1", "1.4.25.1"},
		{"1.4.25+build.1", "1.4.25+build.1"},
		{"", ""},
		{"   ", ""},
		{"muse dev", ""},
		{"unknown", ""},
	}
	for _, tc := range cases {
		if got := normalizeMuseVersion(tc.raw); got != tc.want {
			t.Errorf("normalizeMuseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestMuseCodeUnverifiedVersionWarning(t *testing.T) {
	makeEvent := func(dialect, turnID string, inputTokens int64) *hooks.Event {
		return &hooks.Event{
			Name:    hooks.EventModelEnd,
			Dialect: dialect,
			Data: hooks.EventData{
				SessionID:   "sess-1",
				InputTokens: inputTokens,
				Raw:         map[string]interface{}{"turn_id": turnID},
			},
		}
	}

	t.Run("matching version emits no warning and probes once", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		var probes atomic.Int32
		h := &TelemetryHandler{
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.4.25 (1.4.25-R9361.1)\n"
			},
		}
		ev1 := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
		ev2 := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 250))
		if ev1.Data.InputTokens != 100 || ev2.Data.InputTokens != 150 {
			t.Fatalf("deltas = (%d, %d), want (100, 150)", ev1.Data.InputTokens, ev2.Data.InputTokens)
		}
		if probes.Load() != 1 {
			t.Fatalf("probe calls = %d, want 1", probes.Load())
		}
		if got := cap.warnings(); len(got) != 0 {
			t.Fatalf("warnings = %+v, want 0", got)
		}
	})

	t.Run("differing version emits exactly one warning across multiple events", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		var probes atomic.Int32
		h := &TelemetryHandler{
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.5.0 (1.5.0-R9999.1)"
			},
		}
		ev1 := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
		ev2 := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 300))
		if ev1.Data.InputTokens != 100 || ev2.Data.InputTokens != 200 {
			t.Fatalf("deltas = (%d, %d), want (100, 200)", ev1.Data.InputTokens, ev2.Data.InputTokens)
		}
		if probes.Load() != 1 {
			t.Fatalf("probe calls = %d, want 1", probes.Load())
		}
		warns := cap.warnings()
		if len(warns) != 1 {
			t.Fatalf("warnings count = %d, want 1 (%+v)", len(warns), warns)
		}
		w := warns[0]
		if w.attrs["harness"] != "muse-code" || w.attrs["installed_version"] != "1.5.0" || w.attrs["verified_versions"] != strings.Join(MuseCodeVerifiedVersions, ",") {
			t.Errorf("warning attrs = %+v, want harness=muse-code installed_version=1.5.0 verified_versions=%s", w.attrs, strings.Join(MuseCodeVerifiedVersions, ","))
		}
		if !strings.Contains(w.msg, "usage reporting") || !strings.Contains(w.msg, "harness-cli-version-bump.md") {
			t.Errorf("warning msg = %q, want usage reporting degradation and bump doc reference", w.msg)
		}
	})

	t.Run("missing or unparseable version emits no warning and does not panic", func(t *testing.T) {
		for _, raw := range []string{"", "muse dev", "error: not found"} {
			cap := &museCaptureSlogHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(cap))
			func() {
				defer slog.SetDefault(prev)
				var probes atomic.Int32
				h := &TelemetryHandler{
					museVersionProbe: func(context.Context) string {
						probes.Add(1)
						return raw
					},
				}
				ev := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
				if ev.Data.InputTokens != 100 {
					t.Fatalf("raw=%q: InputTokens = %d, want 100", raw, ev.Data.InputTokens)
				}
				if probes.Load() != 1 {
					t.Fatalf("raw=%q: probe calls = %d, want 1", raw, probes.Load())
				}
				if got := cap.warnings(); len(got) != 0 {
					t.Fatalf("raw=%q: warnings = %+v, want 0", raw, got)
				}
			}()
		}
	})

	t.Run("non-muse dialect does not invoke probe", func(t *testing.T) {
		var probes atomic.Int32
		h := &TelemetryHandler{
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 9.9.9"
			},
		}
		h.normalizeMuseTurnUsage(makeEvent("claude", "t1", 100))
		if probes.Load() != 0 {
			t.Fatalf("probe calls for non-muse dialect = %d, want 0", probes.Load())
		}
	})

	t.Run("prerelease and extra segment versions emit warning", func(t *testing.T) {
		for _, raw := range []string{"1.4.25-rc.1", "1.4.25.1", "1.4.25+build.1"} {
			cap := &museCaptureSlogHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(cap))
			func() {
				defer slog.SetDefault(prev)
				h := &TelemetryHandler{
					museVersionProbe: func(context.Context) string { return raw },
				}
				h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
				warns := cap.warnings()
				if len(warns) != 1 || warns[0].attrs["installed_version"] != raw {
					t.Errorf("raw=%q: warnings = %+v, want 1 warning for %q", raw, warns, raw)
				}
			}()
		}
	})

	t.Run("cross-process marker suppresses repeat probes and warnings across separate handlers", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")
		var probes atomic.Int32
		probeFn := func(context.Context) string {
			probes.Add(1)
			return "Muse Code 1.5.0 (1.5.0-R9999.1)"
		}

		// Simulate 3 separate `sciontool hook` processes (fresh TelemetryHandler each).
		for i, tokens := range []int64{100, 250, 400} {
			h := &TelemetryHandler{
				MuseTurnUsageStatePath: statePath,
				museVersionProbe:       probeFn,
			}
			ev := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", tokens))
			if ev == nil {
				t.Fatalf("handler %d returned nil event", i)
			}
		}

		if probes.Load() != 1 {
			t.Fatalf("probe calls across 3 separate handlers = %d, want 1", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 {
			t.Fatalf("warnings across 3 separate handlers = %d, want 1 (%+v)", len(warns), warns)
		}

		// Verify marker file exists alongside muse-turn-usage.json with 0600 mode and expected payload.
		markerPath := filepath.Join(filepath.Dir(statePath), museVersionMarkerFileName)
		info, err := os.Stat(markerPath)
		if err != nil {
			t.Fatalf("stat marker %s: %v", markerPath, err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("marker mode = %04o, want 0600", mode)
		}
		m, ok := readMuseVersionCheckMarker(markerPath)
		if !ok || m.CheckedVersion != "1.5.0" || m.Verified {
			t.Errorf("marker = (%+v, %v), want CheckedVersion=1.5.0 Verified=false", m, ok)
		}
	})

	t.Run("corrupt marker re-probes once, heals marker, and does not spam", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		dir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		markerPath := filepath.Join(dir, museVersionMarkerFileName)
		if err := os.WriteFile(markerPath, []byte("{corrupt json"), 0o600); err != nil {
			t.Fatal(err)
		}

		var probes atomic.Int32
		probeFn := func(context.Context) string {
			probes.Add(1)
			return "Muse Code 1.5.0"
		}

		for _, tokens := range []int64{100, 200} {
			h := &TelemetryHandler{
				MuseTurnUsageStatePath: statePath,
				museVersionProbe:       probeFn,
			}
			h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", tokens))
		}
		if probes.Load() != 1 {
			t.Fatalf("probes after corrupt marker = %d, want 1", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 {
			t.Fatalf("warnings after corrupt marker = %d, want 1 (%+v)", len(warns), warns)
		}
	})

	t.Run("unwritable marker path does not fail hook or emit marker error spam", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		unwritableMarker := filepath.Join(blocker, museVersionMarkerFileName)

		h := &TelemetryHandler{
			MuseVersionCheckStatePath: unwritableMarker,
			museVersionProbe: func(context.Context) string {
				return "Muse Code 1.4.25"
			},
		}
		ev := h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
		if ev.Data.InputTokens != 100 {
			t.Fatalf("InputTokens = %d, want 100", ev.Data.InputTokens)
		}
		if warns := cap.warnings(); len(warns) != 0 {
			t.Fatalf("warnings on unwritable marker with verified version = %+v, want 0", warns)
		}
	})

	t.Run("marker invalidates when verified_versions list or binary metadata changes", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".scion")
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		markerPath := filepath.Join(dir, museVersionMarkerFileName)

		// 1. Stale verified_versions list forces re-probe.
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{
			CheckedVersion:   "1.4.25",
			Verified:         true,
			VerifiedVersions: []string{"1.4.20"},
		}); err != nil {
			t.Fatal(err)
		}
		var probes atomic.Int32
		h1 := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.4.25"
			},
		}
		h1.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
		if probes.Load() != 1 {
			t.Fatalf("probes after verified_versions change = %d, want 1", probes.Load())
		}

		// 2. Changed binary metadata forces re-probe.
		fakeBin := filepath.Join(t.TempDir(), "muse")
		if err := os.WriteFile(fakeBin, []byte("v1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{
			CheckedVersion:   "1.4.25",
			Verified:         true,
			VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
			BinaryPath:       fakeBin,
			BinaryModTimeNS:  1, // mismatched mtime
			BinarySize:       999,
		}); err != nil {
			t.Fatal(err)
		}
		h2 := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.4.25"
			},
		}
		h2.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 200))
		if probes.Load() != 2 {
			t.Fatalf("probes after binary metadata change = %d, want 2", probes.Load())
		}
	})
}

func TestMuseVersionProbeWaitDelayBoundsGrandchildHoldingStdout(t *testing.T) {
	// Reproduce M2: direct child prints a version and exits while a background
	// grandchild keeps stdout open for 6s. With configureMuseProbeCmd (WaitDelay
	// + process group), OutputManaged must return well before 6s.
	cctx, cancel := context.WithTimeout(context.Background(), museVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", "sleep 6 & echo 1.4.25")
	configureMuseProbeCmd(cmd)

	start := time.Now()
	_, err := procreap.OutputManaged(cmd)
	elapsed := time.Since(start)

	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("probe took %v (err=%v), expected WaitDelay bound well under 2.5s (grandchild held stdout open)", elapsed, err)
	}
	if err == nil {
		t.Fatalf("expected WaitDelay/timeout error when grandchild holds stdout open, got nil (elapsed=%v)", elapsed)
	}
}
