/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestMuseCodeHookTokensReachUsageMetric is the unit-level check called for
// by design §5 ("muse-code | hook tokens land in scion.usage.tokens via
// §3.5") and §9 Phase 2's AC ("muse-code hook tokens, if present, reach the
// dashboard"). It loads the real harnesses/muse-code/dialect.yaml -- the
// "muse dialect fixture" -- and proves that a PostLLMCall payload, parsed
// through that mapping, ends up on scion.usage.tokens{token_type} the same
// way any other hook-sourced harness's tokens would.
//
// This pins the mechanism, not production behavior: muse-code's
// provision.py does not set SCION_USAGE_SOURCE, so today it still publishes
// nothing (design D10's vetting gate) until its own fixture-backed PR
// declares "hooks" -- muse-code runtime verification is explicitly out of
// scope for this project (design §2, §5 "runtime verification is
// follow-up"). The test sets the env var itself to exercise the opted-in
// state.
func TestMuseCodeHookTokensReachUsageMetric(t *testing.T) {
	t.Setenv("SCION_USAGE_SOURCE", "hooks")

	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	specPath := filepath.Join(root, "harnesses", "muse-code", "dialect.yaml")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}
	if md.Name() != "muse-code" {
		t.Fatalf("dialect name = %q, want muse-code", md.Name())
	}

	// A synthetic PostLLMCall payload shaped like the fields
	// harnesses/muse-code/dialect.yaml declares for that hook.
	payload := map[string]interface{}{
		"hook_event_name": "PostLLMCall",
		"input_tokens":    float64(1200),
		"output_tokens":   float64(400),
		"cached_tokens":   float64(150),
	}
	event, err := md.Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event.Name != "model-end" {
		t.Fatalf("event.Name = %q, want model-end", event.Name)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()

	h := NewTelemetryHandler(nil, nil, nil, mp)
	// PostLLMCall arrives as its own `sciontool hook` invocation with no
	// matching model-start in this process: each harness event invokes a
	// separate sciontool process, so a start and its matching end rarely
	// land in the same one -- this is the normal hook-per-process case.
	if err := h.Handle(event); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	totals := usageTokenTotals(rm)
	if totals[telemetrycontract.TokenTypeInput] != 1200 {
		t.Errorf("token_type=input = %d, want 1200", totals[telemetrycontract.TokenTypeInput])
	}
	if totals[telemetrycontract.TokenTypeOutput] != 400 {
		t.Errorf("token_type=output = %d, want 400", totals[telemetrycontract.TokenTypeOutput])
	}
	// muse-code's cached_tokens maps to the canonical cache_read (design §3.5:
	// "cached maps to cache_read").
	if totals[telemetrycontract.TokenTypeCacheRead] != 150 {
		t.Errorf("token_type=cache_read = %d, want 150", totals[telemetrycontract.TokenTypeCacheRead])
	}

	foundAPICalls := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == telemetrycontract.MetricAPICalls {
				foundAPICalls = true
			}
		}
	}
	if !foundAPICalls {
		t.Error("expected gen_ai.api.calls to be recorded alongside the tokens")
	}
}
