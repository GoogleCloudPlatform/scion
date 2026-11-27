/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/sys/unix"
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

		// 2. Mtime-only binary metadata change forces re-probe.
		fakeBin := filepath.Join(t.TempDir(), "muse")
		if err := os.WriteFile(fakeBin, []byte("v1"), 0o755); err != nil {
			t.Fatal(err)
		}
		binInfo, err := os.Stat(fakeBin)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{
			CheckedVersion:   "1.4.25",
			Verified:         true,
			VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
			BinaryPath:       fakeBin,
			BinaryModTimeNS:  binInfo.ModTime().UnixNano() - 1, // mtime-only mismatch
			BinarySize:       binInfo.Size(),                   // exact size match
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
			t.Fatalf("probes after mtime-only binary change = %d, want 2", probes.Load())
		}

		// 3. Size-only binary metadata change forces re-probe.
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{
			CheckedVersion:   "1.4.25",
			Verified:         true,
			VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
			BinaryPath:       fakeBin,
			BinaryModTimeNS:  binInfo.ModTime().UnixNano(), // exact mtime match
			BinarySize:       binInfo.Size() + 1,           // size-only mismatch
		}); err != nil {
			t.Fatal(err)
		}
		h3 := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.4.25"
			},
		}
		h3.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 300))
		if probes.Load() != 3 {
			t.Fatalf("probes after size-only binary change = %d, want 3", probes.Load())
		}
	})

	t.Run("failed or unparseable probe writes negative marker with TTL and re-probes on expiry", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		now := time.Date(2026, 11, 27, 17, 0, 0, 0, time.UTC)
		prevNow := museVersionNow
		museVersionNow = func() time.Time { return now }
		defer func() { museVersionNow = prevNow }()

		statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")
		markerPath := filepath.Join(filepath.Dir(statePath), museVersionMarkerFileName)

		var probes atomic.Int32
		probeOutput := "muse dev" // unparseable
		probeFn := func(context.Context) string {
			probes.Add(1)
			return probeOutput
		}

		// 5 fresh handlers before TTL expiry: only 1 probe, 0 warnings.
		for i := 0; i < 5; i++ {
			h := &TelemetryHandler{
				MuseTurnUsageStatePath: statePath,
				museVersionProbe:       probeFn,
			}
			h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", int64((i+1)*100)))
		}
		if probes.Load() != 1 {
			t.Fatalf("probes before TTL expiry = %d, want 1", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 0 {
			t.Fatalf("warnings for failed probe = %+v, want 0", warns)
		}
		info, err := os.Stat(markerPath)
		if err != nil {
			t.Fatalf("stat negative marker: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("negative marker mode = %04o, want 0600", mode)
		}
		m, ok := readMuseVersionCheckMarker(markerPath)
		if !ok || !m.ProbeFailed || m.CheckedVersion != "" || m.CheckedAtUnixNS != now.UnixNano() {
			t.Fatalf("negative marker = (%+v, %v), want ProbeFailed=true CheckedVersion='' CheckedAtUnixNS=%d", m, ok, now.UnixNano())
		}

		// Just before TTL expiry: still memoized.
		now = now.Add(museVersionFailureMarkerTTL - time.Second)
		(&TelemetryHandler{MuseTurnUsageStatePath: statePath, museVersionProbe: probeFn}).normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 600))
		if probes.Load() != 1 {
			t.Fatalf("probes just before TTL expiry = %d, want 1", probes.Load())
		}

		// At TTL expiry: re-probes; if probe now succeeds with unverified version,
		// writes positive marker and warns once.
		now = now.Add(time.Second)
		probeOutput = "Muse Code 1.5.0"
		(&TelemetryHandler{MuseTurnUsageStatePath: statePath, museVersionProbe: probeFn}).normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 700))
		if probes.Load() != 2 {
			t.Fatalf("probes after TTL expiry = %d, want 2", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 || warns[0].attrs["installed_version"] != "1.5.0" {
			t.Fatalf("warnings after TTL expiry = %+v, want 1 warning for 1.5.0", warns)
		}
		// Positive marker now suppresses subsequent probes even after another TTL window.
		now = now.Add(2 * museVersionFailureMarkerTTL)
		(&TelemetryHandler{MuseTurnUsageStatePath: statePath, museVersionProbe: probeFn}).normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 800))
		if probes.Load() != 2 {
			t.Fatalf("probes after positive marker = %d, want 2", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 {
			t.Fatalf("warnings after positive marker = %d, want 1", len(warns))
		}
	})

	t.Run("0644 marker is replaced atomically with 0600 and stops repeat probes", func(t *testing.T) {
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
		if err := os.WriteFile(markerPath, []byte(`{"checked_version":"1.5.0","verified":false}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(markerPath, 0o644); err != nil {
			t.Fatal(err)
		}

		var probes atomic.Int32
		probeFn := func(context.Context) string {
			probes.Add(1)
			return "Muse Code 1.5.0"
		}
		for i := 0; i < 5; i++ {
			h := &TelemetryHandler{
				MuseTurnUsageStatePath: statePath,
				museVersionProbe:       probeFn,
			}
			h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", int64((i+1)*100)))
		}
		if probes.Load() != 1 {
			t.Fatalf("probes after 0644 marker = %d, want 1", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 {
			t.Fatalf("warnings after 0644 marker = %d, want 1", len(warns))
		}
		info, err := os.Stat(markerPath)
		if err != nil {
			t.Fatalf("stat healed marker: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("healed marker mode = %04o, want 0600", mode)
		}
	})

	t.Run("unwritable marker path memoizes per process and respects failure TTL", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 17, 0, 0, 0, time.UTC)
		prevNow := museVersionNow
		museVersionNow = func() time.Time { return now }
		defer func() { museVersionNow = prevNow }()

		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		unwritableMarker := filepath.Join(blocker, museVersionMarkerFileName)
		defer deleteMuseVersionProcessMemo(unwritableMarker)

		var probes atomic.Int32
		probeFn := func(context.Context) string {
			probes.Add(1)
			return ""
		}
		for i := 0; i < 5; i++ {
			h := &TelemetryHandler{
				MuseVersionCheckStatePath: unwritableMarker,
				museVersionProbe:          probeFn,
			}
			h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", int64((i+1)*100)))
		}
		if probes.Load() != 1 {
			t.Fatalf("probes on unwritable marker before TTL = %d, want 1", probes.Load())
		}
		now = now.Add(museVersionFailureMarkerTTL)
		(&TelemetryHandler{
			MuseVersionCheckStatePath: unwritableMarker,
			museVersionProbe:          probeFn,
		}).normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 600))
		if probes.Load() != 2 {
			t.Fatalf("probes on unwritable marker after TTL = %d, want 2", probes.Load())
		}
	})

	t.Run("symlinked marker is refused without touching target and memoized per process", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		base := t.TempDir()
		target := filepath.Join(base, "victim.txt")
		const sentinel = "sentinel-do-not-overwrite"
		if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(base, ".scion")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		markerPath := filepath.Join(dir, museVersionMarkerFileName)
		defer deleteMuseVersionProcessMemo(markerPath)
		if err := os.Symlink(target, markerPath); err != nil {
			t.Fatal(err)
		}

		// Direct write must refuse the symlink with ErrReparsePoint.
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{CheckedVersion: "1.5.0"}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("writeMuseVersionCheckMarker(symlink) err = %v, want ErrReparsePoint", err)
		}

		var probes atomic.Int32
		probeFn := func(context.Context) string {
			probes.Add(1)
			return "Muse Code 1.5.0"
		}
		for i := 0; i < 5; i++ {
			h := &TelemetryHandler{
				MuseTurnUsageStatePath: statePath,
				museVersionProbe:       probeFn,
			}
			h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", int64((i+1)*100)))
		}
		if probes.Load() != 1 {
			t.Fatalf("probes on symlinked marker = %d, want 1", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 {
			t.Fatalf("warnings on symlinked marker = %d, want 1", len(warns))
		}
		gotTarget, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if string(gotTarget) != sentinel {
			t.Fatalf("symlink target was overwritten: got %q, want %q", string(gotTarget), sentinel)
		}
		if fi, err := os.Lstat(markerPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("markerPath symlink was modified: fi=%v err=%v", fi, err)
		}
	})

	t.Run("directory and read-only dir markers cost at most one probe per process", func(t *testing.T) {
		t.Run("directory at marker path", func(t *testing.T) {
			cap := &museCaptureSlogHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(cap))
			defer slog.SetDefault(prev)

			dir := filepath.Join(t.TempDir(), ".scion")
			statePath := filepath.Join(dir, "muse-turn-usage.json")
			markerPath := filepath.Join(dir, museVersionMarkerFileName)
			if err := os.MkdirAll(markerPath, 0o700); err != nil {
				t.Fatal(err)
			}
			defer deleteMuseVersionProcessMemo(markerPath)

			var probes atomic.Int32
			probeFn := func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.5.0"
			}
			for i := 0; i < 5; i++ {
				h := &TelemetryHandler{
					MuseTurnUsageStatePath: statePath,
					museVersionProbe:       probeFn,
				}
				h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", int64((i+1)*100)))
			}
			if probes.Load() != 1 {
				t.Fatalf("probes on directory marker = %d, want 1", probes.Load())
			}
			if warns := cap.warnings(); len(warns) != 1 {
				t.Fatalf("warnings on directory marker = %d, want 1", len(warns))
			}
			if fi, err := os.Lstat(markerPath); err != nil || !fi.IsDir() {
				t.Fatalf("directory marker was altered: fi=%v err=%v", fi, err)
			}
		})

		t.Run("read-only scion dir with 0644 marker", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root bypasses 0500 directory permissions")
			}
			cap := &museCaptureSlogHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(cap))
			defer slog.SetDefault(prev)

			dir := filepath.Join(t.TempDir(), ".scion")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(dir, museVersionMarkerFileName)
			if err := os.WriteFile(markerPath, []byte(`{"checked_version":"1.5.0","verified":false}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(markerPath, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			defer deleteMuseVersionProcessMemo(markerPath)

			var probes atomic.Int32
			probeFn := func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.5.0"
			}
			for i := 0; i < 5; i++ {
				h := &TelemetryHandler{
					MuseVersionCheckStatePath: markerPath,
					museVersionProbe:          probeFn,
				}
				h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", int64((i+1)*100)))
			}
			if probes.Load() != 1 {
				t.Fatalf("probes on read-only dir with 0644 marker = %d, want 1", probes.Load())
			}
			if warns := cap.warnings(); len(warns) != 1 {
				t.Fatalf("warnings on read-only dir with 0644 marker = %d, want 1", len(warns))
			}
		})
	})

	t.Run("future CheckedAtUnixNS negative marker is treated as expired", func(t *testing.T) {
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
		prevNow := museVersionNow
		museVersionNow = func() time.Time { return now }
		defer func() { museVersionNow = prevNow }()

		dir := filepath.Join(t.TempDir(), ".scion")
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		markerPath := filepath.Join(dir, museVersionMarkerFileName)

		// Negative marker timestamped 1 hour in the future must not suppress probing forever.
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{
			ProbeFailed:      true,
			CheckedAtUnixNS:  now.Add(time.Hour).UnixNano(),
			VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
		}); err != nil {
			t.Fatal(err)
		}

		var probes atomic.Int32
		h := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.5.0"
			},
		}
		h.normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
		if probes.Load() != 1 {
			t.Fatalf("probes after future-timestamped negative marker = %d, want 1", probes.Load())
		}
		if warns := cap.warnings(); len(warns) != 1 || warns[0].attrs["installed_version"] != "1.5.0" {
			t.Fatalf("warnings after future-timestamped negative marker = %+v, want 1 warning for 1.5.0", warns)
		}
		m, ok := readMuseVersionCheckMarker(markerPath)
		if !ok || m.ProbeFailed || m.CheckedVersion != "1.5.0" {
			t.Fatalf("replaced marker = (%+v, %v), want positive marker for 1.5.0", m, ok)
		}
	})

	t.Run("negative marker invalidates before TTL when binary metadata changes", func(t *testing.T) {
		now := time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)
		prevNow := museVersionNow
		museVersionNow = func() time.Time { return now }
		defer func() { museVersionNow = prevNow }()

		fakeBin := filepath.Join(t.TempDir(), "muse")
		if err := os.WriteFile(fakeBin, []byte("v1"), 0o755); err != nil {
			t.Fatal(err)
		}
		binInfo, err := os.Stat(fakeBin)
		if err != nil {
			t.Fatal(err)
		}

		dir := filepath.Join(t.TempDir(), ".scion")
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		markerPath := filepath.Join(dir, museVersionMarkerFileName)
		if err := writeMuseVersionCheckMarker(markerPath, museVersionCheckMarker{
			ProbeFailed:      true,
			CheckedAtUnixNS:  now.UnixNano(),
			VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
			BinaryPath:       fakeBin,
			BinaryModTimeNS:  binInfo.ModTime().UnixNano(),
			BinarySize:       binInfo.Size(),
		}); err != nil {
			t.Fatal(err)
		}

		var probes atomic.Int32
		probeFn := func(context.Context) string {
			probes.Add(1)
			return "Muse Code 1.4.25"
		}

		// Within TTL with unchanged binary: negative marker still suppresses probing.
		now = now.Add(time.Minute)
		(&TelemetryHandler{MuseTurnUsageStatePath: statePath, museVersionProbe: probeFn}).normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 100))
		if probes.Load() != 0 {
			t.Fatalf("probes within TTL for unchanged binary = %d, want 0", probes.Load())
		}

		// Replace binary (different size) still within TTL: negative marker must invalidate immediately.
		if err := os.WriteFile(fakeBin, []byte("v2-replaced-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		(&TelemetryHandler{MuseTurnUsageStatePath: statePath, museVersionProbe: probeFn}).normalizeMuseTurnUsage(makeEvent("muse-code", "t1", 200))
		if probes.Load() != 1 {
			t.Fatalf("probes after binary replacement within negative-marker TTL = %d, want 1", probes.Load())
		}
	})

	t.Run("concurrent first writers never block each other and bound probes and warnings by racing count", func(t *testing.T) {
		// No cross-process or in-process lock is taken across the probe: a hook
		// must never wait on another hook (even if one probe stalls). On a
		// first-run race before the marker is written, the documented upper
		// bound on probes and WARNs is the number of racing processes/handlers
		// (workers), not strictly 1; atomic temp+rename guarantees a valid 0600
		// marker on disk and suppresses subsequent hooks.
		cap := &museCaptureSlogHandler{}
		prev := slog.Default()
		slog.SetDefault(slog.New(cap))
		defer slog.SetDefault(prev)

		statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")
		markerPath := filepath.Join(filepath.Dir(statePath), museVersionMarkerFileName)

		const workers = 16
		slowEntered := make(chan struct{})
		releaseSlow := make(chan struct{})
		var probes atomic.Int32
		var fastDoneCount atomic.Int32
		allFastDone := make(chan struct{})

		var wg sync.WaitGroup
		wg.Add(workers)

		// Start worker 0 first so it is guaranteed to be stalled inside its
		// probe before the other racing writers launch.
		go func() {
			defer wg.Done()
			h := &TelemetryHandler{
				MuseTurnUsageStatePath: statePath,
				museVersionProbe: func(context.Context) string {
					probes.Add(1)
					close(slowEntered)
					<-releaseSlow
					return "Muse Code 1.5.0"
				},
			}
			h.normalizeMuseTurnUsage(makeEvent("muse-code", "t0", 100))
		}()

		select {
		case <-slowEntered:
		case <-time.After(2 * time.Second):
			close(releaseSlow)
			t.Fatal("slow worker did not enter probe")
		}

		// Launch the remaining workers concurrently while worker 0 is stalled:
		// none may wait on worker 0.
		start := make(chan struct{})
		for i := 1; i < workers; i++ {
			go func(idx int) {
				defer wg.Done()
				<-start
				h := &TelemetryHandler{
					MuseTurnUsageStatePath: statePath,
					museVersionProbe: func(context.Context) string {
						probes.Add(1)
						return "Muse Code 1.5.0"
					},
				}
				h.normalizeMuseTurnUsage(makeEvent("muse-code", fmt.Sprintf("t%d", idx), 100))
				if fastDoneCount.Add(1) == workers-1 {
					close(allFastDone)
				}
			}(i)
		}
		close(start)

		select {
		case <-allFastDone:
		case <-time.After(2 * time.Second):
			close(releaseSlow)
			t.Fatal("fast hooks blocked waiting on stalled first writer")
		}
		close(releaseSlow)
		wg.Wait()

		gotProbes := int(probes.Load())
		if gotProbes < 1 || gotProbes > workers {
			t.Fatalf("concurrent probes = %d, want in [1, %d]", gotProbes, workers)
		}
		gotWarns := len(cap.warnings())
		if gotWarns < 1 || gotWarns > workers {
			t.Fatalf("concurrent warnings = %d, want in [1, %d]", gotWarns, workers)
		}
		info, err := os.Stat(markerPath)
		if err != nil {
			t.Fatalf("stat marker: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("marker mode = %04o, want 0600", mode)
		}
		m, ok := readMuseVersionCheckMarker(markerPath)
		if !ok || m.CheckedVersion != "1.5.0" || m.Verified {
			t.Errorf("marker after race = (%+v, %v), want CheckedVersion=1.5.0 Verified=false", m, ok)
		}
		if _, err := os.Lstat(markerPath + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected sibling lock file %s.lock: err=%v", markerPath, err)
		}

		// Subsequent fresh handler after the race sees the valid marker: 0 new probes/warnings.
		after := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe: func(context.Context) string {
				probes.Add(1)
				return "Muse Code 1.5.0"
			},
		}
		after.normalizeMuseTurnUsage(makeEvent("muse-code", "t-after", 100))
		if int(probes.Load()) != gotProbes {
			t.Fatalf("post-race handler probed: total probes = %d, want %d", probes.Load(), gotProbes)
		}
		if len(cap.warnings()) != gotWarns {
			t.Fatalf("post-race handler warned: total warnings = %d, want %d", len(cap.warnings()), gotWarns)
		}
	})
}

func TestDetectLocalMuseVersionPreservesBinaryMetadataOnProbeFailure(t *testing.T) {
	resolved, err := rootexec.Resolve("false")
	if err != nil {
		t.Fatalf("rootexec.Resolve(\"false\"): %v", err)
	}
	wantInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("stat %s: %v", resolved, err)
	}

	prevBin := museProbeBinaryName
	museProbeBinaryName = "false"
	defer func() { museProbeBinaryName = prevBin }()

	ver, binPath, mtimeNS, size := detectLocalMuseVersion(context.Background())
	if ver != "" {
		t.Fatalf("ver = %q, want empty on non-zero exit", ver)
	}
	if binPath != resolved {
		t.Fatalf("binPath = %q, want %q (must be preserved on probe failure so negative markers invalidate on binary replacement)", binPath, resolved)
	}
	if mtimeNS != wantInfo.ModTime().UnixNano() || size != wantInfo.Size() {
		t.Fatalf("binary metadata = (mtime=%d, size=%d), want (%d, %d)", mtimeNS, size, wantInfo.ModTime().UnixNano(), wantInfo.Size())
	}
}

func TestMuseVersionProcessMemoIsBounded(t *testing.T) {
	museVersionMemoMu.Lock()
	saved := make(map[string]museVersionCheckMarker, len(museVersionMemo))
	for k, v := range museVersionMemo {
		saved[k] = v
	}
	clear(museVersionMemo)
	museVersionMemoMu.Unlock()
	t.Cleanup(func() {
		museVersionMemoMu.Lock()
		clear(museVersionMemo)
		for k, v := range saved {
			museVersionMemo[k] = v
		}
		museVersionMemoMu.Unlock()
	})

	for i := 0; i < museVersionProcessMemoMax+10; i++ {
		key := fmt.Sprintf("/tmp/muse-memo-cap-%d", i)
		storeMuseVersionProcessMemo(key, museVersionCheckMarker{
			CheckedVersion:   "1.5.0",
			VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
		})
		museVersionMemoMu.Lock()
		n := len(museVersionMemo)
		museVersionMemoMu.Unlock()
		if n > museVersionProcessMemoMax {
			t.Fatalf("after %d inserts, museVersionMemo len = %d, exceeds cap %d", i+1, n, museVersionProcessMemoMax)
		}
		if i == museVersionProcessMemoMax && n != 1 {
			t.Fatalf("at cap boundary (insert %d), museVersionMemo len = %d, want 1 after clear", i+1, n)
		}
	}
}

func readGrandchildPID(t *testing.T, pidFile string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read grandchild pidfile %s: %v", pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("parse grandchild pid %q: %v", string(raw), err)
	}
	return pid
}

func waitForProcessDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConfigureMuseProbeCmdSetsHardenedEnvAndProcessGroup(t *testing.T) {
	t.Setenv("SCION_PROBE_TEST_LEAK", "must-not-inherit")
	cmd := exec.Command("/bin/true")
	configureMuseProbeCmd(cmd)

	wantEnv := rootexec.Env("MUSE_NO_AUTO_UPDATE=1")
	if !slices.Equal(cmd.Env, wantEnv) {
		t.Fatalf("cmd.Env = %v, want %v", cmd.Env, wantEnv)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "SCION_PROBE_TEST_LEAK=") {
			t.Fatalf("cmd.Env leaked caller env var: %q", kv)
		}
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("cmd.SysProcAttr = %+v, want Setpgid=true", cmd.SysProcAttr)
	}
	if cmd.WaitDelay != museVersionProbeWaitDelay {
		t.Fatalf("cmd.WaitDelay = %v, want %v", cmd.WaitDelay, museVersionProbeWaitDelay)
	}
	if cmd.Cancel == nil {
		t.Fatal("cmd.Cancel = nil, want process-group SIGKILL cancel func")
	}
	// Defensive guard: nil Process or non-positive Pid must be a no-op so
	// syscall.Kill(-pid, SIGKILL) can never signal the caller's own process
	// group (pid == 0) or an unrelated PID (pid < 0).
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cmd.Cancel() with nil Process = %v, want nil", err)
	}
	cmd.Process = &os.Process{Pid: 0}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cmd.Cancel() with zero Pid = %v, want nil", err)
	}
	cmd.Process = &os.Process{Pid: -999999}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("cmd.Cancel() with negative Pid = %v, want nil", err)
	}
}

func TestMuseVersionProbeCancelKillsProcessGroup(t *testing.T) {
	// Hanging leader spawns a background grandchild; on timeout, cmd.Cancel
	// must SIGKILL the entire process group (not just cmd.Process.Kill()), so
	// the grandchild is dead as soon as OutputManaged returns.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", `sleep 6 & echo $! > "$1"; exec sleep 6`, "_", pidFile)
	configureMuseProbeCmd(cmd)

	start := time.Now()
	_, err := procreap.OutputManaged(cmd)
	elapsed := time.Since(start)

	pid := readGrandchildPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if err == nil {
		t.Fatalf("expected timeout error, got nil (elapsed=%v)", elapsed)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("hanging probe took %v, expected well under 2s", elapsed)
	}
	if !waitForProcessDead(pid, 300*time.Millisecond) {
		t.Fatalf("grandchild pid %d survived probe cancel (process-group SIGKILL failed)", pid)
	}
}

func TestMuseVersionProbeWaitDelayPreservesOutputAndKillsGrandchild(t *testing.T) {
	// Reproduce L2: direct child prints a version and exits normally while a
	// background grandchild keeps stdout open for 6s. runMuseProbeCmd must
	// return well before 6s (~WaitDelay), preserve the version already printed,
	// and kill the surviving grandchild in the probe's process group.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cctx, cancel := context.WithTimeout(context.Background(), museVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", `sleep 6 & echo $! > "$1"; echo 1.4.25`, "_", pidFile)
	configureMuseProbeCmd(cmd)

	start := time.Now()
	out, err := runMuseProbeCmd(cmd)
	elapsed := time.Since(start)

	pid := readGrandchildPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("probe took %v (err=%v), expected WaitDelay bound well under 2.5s", elapsed, err)
	}
	if err != nil {
		t.Fatalf("runMuseProbeCmd returned err=%v, want nil (WaitDelay after normal exit should preserve stdout)", err)
	}
	if got := normalizeMuseVersion(string(out)); got != "1.4.25" {
		t.Fatalf("normalizeMuseVersion(%q) = %q, want 1.4.25", string(out), got)
	}
	if !waitForProcessDead(pid, 300*time.Millisecond) {
		t.Fatalf("grandchild pid %d survived WaitDelay expiry (post-wait process-group SIGKILL failed)", pid)
	}
}

func makeMuseModelEndEvent(sessionID, turnID string, input, output int64) *hooks.Event {
	return &hooks.Event{
		Name:    hooks.EventModelEnd,
		Dialect: "muse-code",
		Data: hooks.EventData{
			SessionID:    sessionID,
			InputTokens:  input,
			OutputTokens: output,
			TokenPresence: hooks.TokenPresence{
				Input:  true,
				Output: true,
			},
			Raw: map[string]interface{}{"turn_id": turnID},
		},
	}
}

// TestMuseTurnUsage_HeldLockReturnsWithinBound verifies that when another
// process/fd holds the sibling flock indefinitely, normalizeMuseTurnUsage
// returns within the bounded wait instead of hanging, leaves the state file
// untouched, fails closed on the skipped step's tokens, and recovers the
// skipped step's cumulative tokens on the next step once the lock is released.
func TestMuseTurnUsage_HeldLockReturnsWithinBound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "muse-turn-usage.json")
	lockPath := statePath + ".lock"

	// Step 0 succeeds and seeds the state file (high-water mark: in=600, out=100).
	h0 := &TelemetryHandler{
		MuseTurnUsageStatePath: statePath,
		museVersionProbe:       func(context.Context) string { return "1.4.25" },
	}
	ev0 := h0.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", 600, 100))
	if ev0.Data.InputTokens != 600 || ev0.Data.OutputTokens != 100 {
		t.Fatalf("step 0 delta = (%d, %d), want (600, 100)", ev0.Data.InputTokens, ev0.Data.OutputTokens)
	}
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read initial state: %v", err)
	}

	// Hold an exclusive flock on a separate fd and never release it during step 1.
	held, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	released := false
	releaseHeld := func() {
		if !released {
			released = true
			_ = unix.Flock(int(held.Fd()), unix.LOCK_UN)
			_ = held.Close()
		}
	}
	t.Cleanup(releaseHeld)

	// Step 1 arrives while the lock is held forever; must return within bound.
	done := make(chan *hooks.Event, 1)
	start := time.Now()
	go func() {
		h1 := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe:       func(context.Context) string { return "1.4.25" },
		}
		done <- h1.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", 1200, 250))
	}()

	var ev1 *hooks.Event
	select {
	case ev1 = <-done:
	case <-time.After(3 * time.Second):
		releaseHeld()
		<-done
		t.Fatal("normalizeMuseTurnUsage blocked past 3s with held flock")
	}
	elapsed := time.Since(start)
	if elapsed > 2500*time.Millisecond {
		t.Errorf("held-lock wait = %v, want <= 2.5s", elapsed)
	}
	if ev1.Data.HasInputTokens() || ev1.Data.HasOutputTokens() || ev1.Data.InputTokens != 0 || ev1.Data.OutputTokens != 0 {
		t.Errorf("skipped step must fail closed with cleared tokens, got in=%d (has=%v) out=%d (has=%v)",
			ev1.Data.InputTokens, ev1.Data.HasInputTokens(), ev1.Data.OutputTokens, ev1.Data.HasOutputTokens())
	}
	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after skip: %v", err)
	}
	if string(afterState) != string(beforeState) {
		t.Fatalf("state file modified while lock was held:\nbefore: %s\nafter:  %s", beforeState, afterState)
	}

	// Release lock: step 2 (cumulative 1900/400) must recover step 1 + step 2 delta (1300/300).
	releaseHeld()
	h2 := &TelemetryHandler{
		MuseTurnUsageStatePath: statePath,
		museVersionProbe:       func(context.Context) string { return "1.4.25" },
	}
	ev2 := h2.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", 1900, 400))
	if ev2.Data.InputTokens != 1300 || ev2.Data.OutputTokens != 300 {
		t.Fatalf("recovered step 2 delta = (%d, %d), want (1300, 300)", ev2.Data.InputTokens, ev2.Data.OutputTokens)
	}
}

// TestMuseTurnUsage_ConcurrentUpdatesBothLand verifies that concurrent hook
// handlers updating distinct turns when the lock is free both persist their
// high-water marks without lost updates or state corruption.
func TestMuseTurnUsage_ConcurrentUpdatesBothLand(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), ".scion", "muse-turn-usage.json")
	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			key := fmt.Sprintf("sess:turn-%d", idx)
			d0, err := applyMuseTurnUsageFile(statePath, key, museTurnUsageTotals{Input: 100, Output: 20})
			if err != nil {
				errs <- fmt.Errorf("worker %d step 0: %w", idx, err)
				return
			}
			if d0.Input != 100 || d0.Output != 20 {
				errs <- fmt.Errorf("worker %d step 0 delta = %+v, want (100,20)", idx, d0)
				return
			}
			d1, err := applyMuseTurnUsageFile(statePath, key, museTurnUsageTotals{Input: 250, Output: 55})
			if err != nil {
				errs <- fmt.Errorf("worker %d step 1: %w", idx, err)
				return
			}
			if d1.Input != 150 || d1.Output != 35 {
				errs <- fmt.Errorf("worker %d step 1 delta = %+v, want (150,35)", idx, d1)
				return
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read final state: %v", err)
	}
	var st museTurnUsageState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal final state: %v", err)
	}
	for i := 0; i < workers; i++ {
		key := fmt.Sprintf("sess:turn-%d", i)
		got, ok := st.Turns[key]
		if !ok || got.Input != 250 || got.Output != 55 {
			t.Errorf("state[%q] = (%+v, %v), want (250, 55, true)", key, got, ok)
		}
	}
}

// TestMuseTurnUsage_SymlinkedStateAndLockRefused verifies that symlinked state
// files (live or dangling) and symlinked lock files are refused without
// touching the symlink target.
func TestMuseTurnUsage_SymlinkedStateAndLockRefused(t *testing.T) {
	t.Run("live symlink at state path", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "victim.txt")
		const sentinel = "sentinel-state-target"
		if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(base, ".scion")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		if err := os.Symlink(target, statePath); err != nil {
			t.Fatal(err)
		}

		if _, err := applyMuseTurnUsageFile(statePath, "s:t", museTurnUsageTotals{Input: 100}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("applyMuseTurnUsageFile(live symlink) err = %v, want ErrReparsePoint", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != sentinel {
			t.Fatalf("victim file altered: got=%q err=%v", string(got), err)
		}
	})

	t.Run("dangling symlink at state path", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "nonexistent-victim.txt")
		dir := filepath.Join(base, ".scion")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		if err := os.Symlink(target, statePath); err != nil {
			t.Fatal(err)
		}

		if _, err := applyMuseTurnUsageFile(statePath, "s:t", museTurnUsageTotals{Input: 100}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("applyMuseTurnUsageFile(dangling symlink) err = %v, want ErrReparsePoint", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dangling symlink target was created: err=%v", err)
		}
		if fi, err := os.Lstat(statePath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("dangling symlink was replaced: fi=%v err=%v", fi, err)
		}
	})

	t.Run("symlink at lock path", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "victim-lock.txt")
		const sentinel = "sentinel-lock-target"
		if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(base, ".scion")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "muse-turn-usage.json")
		if err := os.Symlink(target, statePath+".lock"); err != nil {
			t.Fatal(err)
		}

		if _, err := applyMuseTurnUsageFile(statePath, "s:t", museTurnUsageTotals{Input: 100}); !errors.Is(err, privatefs.ErrReparsePoint) {
			t.Fatalf("applyMuseTurnUsageFile(symlink lock) err = %v, want ErrReparsePoint", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != sentinel {
			t.Fatalf("lock symlink target altered: got=%q err=%v", string(got), err)
		}
	})
}

// TestMuseTurnUsage_SkipPathLogsOncePerProcess verifies that repeated lock
// timeouts in the same process leave the state file unchanged, never panic,
// fail closed on tokens, and log a warning only once per process (re-arming
// after a successful update).
func TestMuseTurnUsage_SkipPathLogsOncePerProcess(t *testing.T) {
	cap := &museCaptureSlogHandler{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prevLogger)

	prevWait := museTurnUsageLockWait
	museTurnUsageLockWait = 40 * time.Millisecond
	defer func() { museTurnUsageLockWait = prevWait }()

	dir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "muse-turn-usage.json")
	defer resetMuseTurnUsageWarned(statePath)

	// Seed initial state (step 0 = 500).
	h0 := &TelemetryHandler{
		MuseTurnUsageStatePath: statePath,
		museVersionProbe:       func(context.Context) string { return "1.4.25" },
	}
	ev0 := h0.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", 500, 50))
	if ev0.Data.InputTokens != 500 {
		t.Fatalf("step 0 input = %d, want 500", ev0.Data.InputTokens)
	}
	initialBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// Hold lock across 5 consecutive hook invocations.
	held, err := os.OpenFile(statePath+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = unix.Flock(int(held.Fd()), unix.LOCK_UN)
			_ = held.Close()
		}
	}
	t.Cleanup(release)

	for i := 1; i <= 5; i++ {
		h := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe:       func(context.Context) string { return "1.4.25" },
		}
		ev := h.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", int64(500+i*100), int64(50+i*10)))
		if ev.Data.HasInputTokens() || ev.Data.HasOutputTokens() || ev.Data.InputTokens != 0 || ev.Data.OutputTokens != 0 {
			t.Fatalf("call %d with held lock did not fail closed: in=%d out=%d", i, ev.Data.InputTokens, ev.Data.OutputTokens)
		}
	}

	if warns := cap.warnings(); len(warns) != 1 {
		t.Fatalf("warnings across 5 skipped calls = %d, want 1 (%+v)", len(warns), warns)
	}
	afterBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(initialBytes) {
		t.Fatalf("state file changed during skipped calls:\nbefore: %s\nafter:  %s", initialBytes, afterBytes)
	}

	// Release lock: next step recovers cumulative delta and re-arms warning.
	release()
	hOk := &TelemetryHandler{
		MuseTurnUsageStatePath: statePath,
		museVersionProbe:       func(context.Context) string { return "1.4.25" },
	}
	evOk := hOk.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", 1200, 120))
	if evOk.Data.InputTokens != 700 || evOk.Data.OutputTokens != 70 {
		t.Fatalf("post-recovery delta = (%d, %d), want (700, 70)", evOk.Data.InputTokens, evOk.Data.OutputTokens)
	}

	// Re-acquire lock: warning must be re-armed so a subsequent failure burst
	// logs exactly one new warning (total 2), still deduped within the burst.
	held2, err := os.OpenFile(statePath+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(held2.Fd()), unix.LOCK_EX); err != nil {
		_ = held2.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Flock(int(held2.Fd()), unix.LOCK_UN)
		_ = held2.Close()
	})

	for i := 1; i <= 2; i++ {
		h := &TelemetryHandler{
			MuseTurnUsageStatePath: statePath,
			museVersionProbe:       func(context.Context) string { return "1.4.25" },
		}
		ev := h.normalizeMuseTurnUsage(makeMuseModelEndEvent("s1", "t1", int64(1200+i*100), int64(120+i*10)))
		if ev.Data.HasInputTokens() || ev.Data.HasOutputTokens() || ev.Data.InputTokens != 0 || ev.Data.OutputTokens != 0 {
			t.Fatalf("post-recovery held-lock call %d did not fail closed: in=%d out=%d", i, ev.Data.InputTokens, ev.Data.OutputTokens)
		}
	}
	if warns := cap.warnings(); len(warns) != 2 {
		t.Fatalf("warnings after re-arm and second held-lock burst = %d, want 2 (%+v)", len(warns), warns)
	}
}

// TestMuseTurnUsage_RetryCapStopsBeforeTimeout verifies that the retry cap
// bounds attempts even when the time budget is large.
func TestMuseTurnUsage_RetryCapStopsBeforeTimeout(t *testing.T) {
	prevWait := museTurnUsageLockWait
	prevCap := museTurnUsageLockRetryCap
	museTurnUsageLockWait = 10 * time.Second
	museTurnUsageLockRetryCap = 3
	defer func() {
		museTurnUsageLockWait = prevWait
		museTurnUsageLockRetryCap = prevCap
	}()

	dir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "muse-turn-usage.json")
	held, err := os.OpenFile(statePath+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Flock(int(held.Fd()), unix.LOCK_UN)
		_ = held.Close()
	}()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = applyMuseTurnUsageFile(statePath, "s:t", museTurnUsageTotals{Input: 100})
	elapsed := time.Since(start)
	if !errors.Is(err, errMuseTurnUsageLocked) {
		t.Fatalf("err = %v, want errMuseTurnUsageLocked", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("retry cap=3 took %v (with 10s wait), expected < 500ms", elapsed)
	}
}
