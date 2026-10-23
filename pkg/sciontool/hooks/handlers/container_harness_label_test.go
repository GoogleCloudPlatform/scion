// Copyright 2026 The Scion Authors.

package handlers

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// containerHarnessCasesPath is the case table pkg/agent's
// TestStart_ContainerHarnessCases asserts Start against (ptone/scion#5946),
// read in place.
const containerHarnessCasesPath = "../../../agent/testdata/harness_env_cases.json"

// TestTelemetryHandler_ContainerHarnessCasesLabelHookPoints: for each
// SCION_HARNESS value Start emits in the shared case table (including
// "generic"), hook-sourced gen_ai.api.calls and scion.usage.tokens points
// carry that value, and only it, as their harness label.
func TestTelemetryHandler_ContainerHarnessCasesLabelHookPoints(t *testing.T) {
	data, err := os.ReadFile(containerHarnessCasesPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []struct {
			ContainerHarness string `json:"container_harness"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range file.Cases {
		harness := c.ContainerHarness
		if seen[harness] {
			continue
		}
		seen[harness] = true
		t.Run(harness, func(t *testing.T) {
			scrubTelemetryEnv(t)
			t.Setenv("SCION_USAGE_SOURCE", "hooks")
			t.Setenv("SCION_HARNESS", harness)

			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()
			h := NewTelemetryHandler(nil, nil, nil, mp)
			if err := h.Handle(&hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{Success: true, InputTokens: 5, OutputTokens: 2},
			}); err != nil {
				t.Fatal(err)
			}
			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatal(err)
			}
			checked := map[string]int{}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != telemetrycontract.MetricUsageTokens && m.Name != telemetrycontract.MetricAPICalls {
						continue
					}
					sum, ok := m.Data.(metricdata.Sum[int64])
					if !ok {
						t.Fatalf("%s: data %T", m.Name, m.Data)
					}
					for _, p := range sum.DataPoints {
						v, ok := p.Attributes.Value(attribute.Key(telemetrycontract.HarnessLabel))
						if !ok || v.AsString() != harness {
							t.Fatalf("%s point harness = %q (present %v), want %q", m.Name, v.AsString(), ok, harness)
						}
						checked[m.Name]++
					}
				}
			}
			if checked[telemetrycontract.MetricUsageTokens] == 0 || checked[telemetrycontract.MetricAPICalls] == 0 {
				t.Fatalf("points checked = %v, want both %s and %s", checked, telemetrycontract.MetricUsageTokens, telemetrycontract.MetricAPICalls)
			}
		})
	}
}
